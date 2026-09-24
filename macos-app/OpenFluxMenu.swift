// OpenFluxMenu — menu-bar controller for the OpenFlux macOS tun client.
// Compiled with swiftc (no Xcode) and packaged into OpenFlux.app by build.sh.
// It launches /usr/local/bin/openflux under passwordless sudo, shows status and
// the external IP, streams the log, and keeps a set of connection profiles.
// Route cleanup on stop and on crash is done by openflux / the kernel.

import AppKit
import Foundation

// MARK: - Profile

/// One saved connection: a transport plus everything that transport needs.
/// Several may exist side by side (say a Mail.ru one and a Yandex one); the
/// menu picks which is used.
struct Profile: Codable {
    var id = UUID()
    var name = "Новый профиль"
    // No defaults for the document URL or the key: they identify a specific
    // tunnel and belong to the operator, not to the source tree.
    var transport = "mailru"
    var url = ""
    var keyFile = ""
    /// Only read, never written: the resolver moved to the store. Kept so an
    /// older saved profile can still be decoded and its value lifted out.
    var dns = ""
    var maxToken = ""
    var maxUid = ""
    var debug = false

    static let transports = ["mailru", "vyandex", "yandex", "cupsonline", "jitsi", "oneme"]

    /// The documents this profile bonds, one per line in the editor. Several
    /// of them are what keeps the tunnel up when a provider closes one.
    ///
    /// The separator test has to be `isNewline`, not a comparison against
    /// "\n". Swift counts a CR-LF pair as one Character, and that Character
    /// equals neither "\n" nor ","; text arriving with Windows line endings
    /// therefore did not split at all, and five documents were counted as one.
    /// The tunnel itself splits the list it is handed, so the bond was built
    /// correctly and nothing looked wrong — except the count in the menu, and
    /// every decision made from it.
    var documents: [String] {
        url.split(whereSeparator: { $0.isNewline || $0 == "," })
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
    }

    /// How to describe what this profile carries. cups.online is not counted
    /// in documents: its single entry is a packed list of rooms the exit node
    /// made, so "1 док." would be both wrong and confusing.
    var carriesDescription: String {
        switch transport {
        case "cupsonline": return "комнаты ноды"
        case "oneme":      return "по учётным данным"
        case "jitsi":      return "\(documents.count) комн."
        default:           return "\(documents.count) док."
        }
    }

    /// A profile is usable once its transport has what it needs: oneme is
    /// driven by credentials, everything else by at least one document.
    var isComplete: Bool {
        transport == "oneme"
            ? !maxToken.trimmingCharacters(in: .whitespaces).isEmpty
            : !documents.isEmpty
    }
}

/// The whole persisted state: the profiles and which one is selected.
struct Store: Codable {
    var profiles: [Profile] = []
    var selected: UUID?

    /// The resolver to use while the tunnel is up.
    ///
    /// It lives here rather than in a profile because it describes where the
    /// exit node is, not how the tunnel reaches it: switching from documents
    /// to rooms changes the carrier, not which resolver sits behind the far
    /// end. Kept per profile, every new profile started with an empty field
    /// and the same address had to be typed again.
    var dns = ""

    /// Older stores kept the resolver on each profile. Take the first one that
    /// was filled in, so an upgrade keeps working without being re-entered.
    mutating func liftDNSFromProfiles() {
        guard dns.isEmpty else { return }
        if let found = profiles.first(where: { !$0.dns.trimmingCharacters(in: .whitespaces).isEmpty }) {
            dns = found.dns.trimmingCharacters(in: .whitespaces)
        }
    }

    static let key = "store"
    private static let binary = "/usr/local/bin/openflux"

    var active: Profile? {
        if let id = selected, let p = profiles.first(where: { $0.id == id }) { return p }
        return profiles.first
    }

    static func load() -> Store {
        let d = UserDefaults.standard
        if let data = d.data(forKey: key),
           var s = try? JSONDecoder().decode(Store.self, from: data), !s.profiles.isEmpty {
            s.liftDNSFromProfiles()
            return s
        }
        return migrateLegacy()
    }

    func save() {
        if let data = try? JSONEncoder().encode(self) {
            UserDefaults.standard.set(data, forKey: Store.key)
        }
    }

    /// Earlier builds kept a single flat configuration. Fold it into one
    /// profile so an upgrade never loses a working setup.
    private static func migrateLegacy() -> Store {
        let d = UserDefaults.standard
        var p = Profile()
        p.name = "Основной"
        if let v = d.string(forKey: "transport"), !v.isEmpty { p.transport = v }
        if let v = d.string(forKey: "url") { p.url = v }
        if let v = d.string(forKey: "keyFile") { p.keyFile = v }
        if let v = d.string(forKey: "dns") { p.dns = v }
        if let v = d.string(forKey: "maxToken") { p.maxToken = v }
        if let v = d.string(forKey: "maxUid") { p.maxUid = v }
        p.debug = d.bool(forKey: "debug")

        var s = Store(profiles: [p], selected: p.id)
        s.liftDNSFromProfiles()
        s.save()
        return s
    }

    /// Path of the tunnel binary. Kept out of the profile: it is a property of
    /// the installation, not of a connection.
    static var binaryPath: String {
        UserDefaults.standard.string(forKey: "binary") ?? binary
    }
}

// MARK: - State

enum TunnelState: String {
    case disconnected  = "Отключено"
    case connecting    = "Подключаюсь…"
    case connected     = "Подключено"
    // The tunnel is up but its channel is down: routes and utun stay in place
    // while the transport reconnects, and saying "connected" here would be a
    // lie the user can see through — traffic has stopped.
    case reconnecting  = "Связь потеряна, восстанавливаю…"
    case disconnecting = "Отключаюсь…"
}

// MARK: - Log store (ring buffer + file)

final class LogStore {
    let fileURL: URL
    private var handle: FileHandle?
    private var lines: [String] = []
    private let maxLines = 800
    private let q = DispatchQueue(label: "openflux.log")
    var onAppend: (() -> Void)?

    init() {
        let dir = (NSHomeDirectory() as NSString).appendingPathComponent("Library/Logs/OpenFlux")
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        fileURL = URL(fileURLWithPath: dir).appendingPathComponent("openflux.log")
    }

    func startSession(header: String) {
        q.sync {
            FileManager.default.createFile(atPath: fileURL.path, contents: nil)
            handle = try? FileHandle(forWritingTo: fileURL)
            lines.removeAll()
            let line = "==== \(header) ====\n"
            handle?.write(line.data(using: .utf8)!)
            lines.append(line)
        }
        onAppend?()
    }

    func append(_ data: Data) {
        guard !data.isEmpty else { return }
        q.async {
            self.handle?.write(data)
            if let s = String(data: data, encoding: .utf8) {
                for part in s.split(separator: "\n", omittingEmptySubsequences: false) {
                    self.lines.append(String(part))
                }
                if self.lines.count > self.maxLines {
                    self.lines.removeFirst(self.lines.count - self.maxLines)
                }
            }
            DispatchQueue.main.async { self.onAppend?() }
        }
    }

    func text() -> String { q.sync { lines.joined(separator: "\n") } }
    func closeSession() { q.sync { try? handle?.close(); handle = nil } }
}

// MARK: - Tunnel controller (process lifecycle)

final class TunnelController {
    let log = LogStore()
    var onState: ((TunnelState) -> Void)?

    // The child process, the stop flag and the state are touched from the
    // launching thread, the pipe's reader thread, the termination handler and
    // the stop worker. One lock covers all three rather than leaving them to
    // race.
    private let lock = NSLock()
    private var _state: TunnelState = .disconnected
    private var _proc: Process?
    private var _requestedStop = false

    var state: TunnelState {
        lock.lock(); defer { lock.unlock() }
        return _state
    }

    /// The tunnel exists — utun is up and the routes are installed — whether
    /// or not its channel is carrying traffic at this instant. Anything that
    /// must not leave a running process behind has to test this, not just
    /// `.connected`.
    var isActive: Bool {
        switch state {
        case .connected, .connecting, .reconnecting, .disconnecting: return true
        case .disconnected: return false
        }
    }
    private var proc: Process? {
        get { lock.lock(); defer { lock.unlock() }; return _proc }
        set { lock.lock(); _proc = newValue; lock.unlock() }
    }
    private var requestedStop: Bool {
        get { lock.lock(); defer { lock.unlock() }; return _requestedStop }
        set { lock.lock(); _requestedStop = newValue; lock.unlock() }
    }

    private func setState(_ s: TunnelState) {
        lock.lock(); _state = s; lock.unlock()
        DispatchQueue.main.async { self.onState?(s) }
    }

    func connect(_ p: Profile, dns resolver: String) {
        guard state == .disconnected else { return }
        requestedStop = false
        setState(.connecting)
        log.startSession(header: "connect \(p.name) [\(p.transport), \(p.carriesDescription)] \(Date())")

        var args = ["-n", Store.binaryPath,
                    "--role=client", "--inbound=tun",
                    "--transport=\(p.transport)"]
        // One --url per document. Bonding them means a document being closed
        // by the provider costs one link, not the tunnel.
        for doc in p.documents { args.append("--url=\(doc)") }
        // Encryption is optional: an empty key file means the flag is omitted,
        // so the transport runs unencrypted. The node must match (also keyless).
        let key = p.keyFile.trimmingCharacters(in: .whitespaces)
        if !key.isEmpty { args.append("--encryption-key-file=\(key)") }
        // A resolver behind the exit node: the client turns the OS's UDP
        // queries into DNS-over-TCP through the tunnel and restores the
        // previous system resolver when it stops.
        let dns = resolver.trimmingCharacters(in: .whitespaces)
        if !dns.isEmpty { args.append("--dns=\(dns)") }
        if !p.maxToken.isEmpty { args.append("--maxToken=\(p.maxToken)") }
        if !p.maxUid.isEmpty { args.append("--maxUid=\(p.maxUid)") }
        if p.debug { args.append("--debug") }

        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/bin/sudo")
        proc.arguments = args
        let pipe = Pipe()
        proc.standardOutput = pipe
        proc.standardError = pipe
        pipe.fileHandleForReading.readabilityHandler = { [weak self] h in
            let d = h.availableData
            guard let self = self, !d.isEmpty else { return }
            self.log.append(d)
            guard let s = String(data: d, encoding: .utf8) else { return }
            if s.contains("Tunnel active") || s.contains("Transport reconnected") {
                self.setState(.connected)
            } else if s.contains("Transport disconnected") {
                self.setState(.reconnecting)
            }
        }
        proc.terminationHandler = { [weak self] pr in
            guard let self = self else { return }
            pipe.fileHandleForReading.readabilityHandler = nil
            self.log.append("\n==== openflux exited (code \(pr.terminationStatus)) ====\n".data(using: .utf8)!)
            self.log.closeSession()
            let wasRequested = self.requestedStop
            self.proc = nil
            self.setState(.disconnected)
            if !wasRequested {
                DispatchQueue.main.async { self.notifyUnexpected(code: pr.terminationStatus) }
            }
        }
        do { try proc.run(); self.proc = proc }
        catch {
            log.append("failed to launch: \(error)\n".data(using: .utf8)!)
            setState(.disconnected)
        }
    }

    func disconnect() {
        guard state == .connected || state == .connecting || state == .reconnecting else { return }
        requestedStop = true
        setState(.disconnecting)
        DispatchQueue.global().async {
            self.runSudo(["pkill", "-TERM", "-f", Store.binaryPath])
            for _ in 0..<12 {
                if self.proc == nil { return }
                usleep(500_000)
            }
            self.runSudo(["pkill", "-KILL", "-f", Store.binaryPath])
        }
    }

    /// Runs one of the two commands the sudoers rule allows, as root.
    ///
    /// The stop uses `pkill -f <binary path>`, which also matches the `sudo`
    /// wrapper and would match a second tunnel started outside this app. Both
    /// are acceptable here — the wrapper is the process we want gone anyway,
    /// and the rule in /etc/sudoers.d/openflux pins these exact arguments, so
    /// narrowing the match would mean widening the sudo grant.
    @discardableResult
    private func runSudo(_ argv: [String]) -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sudo")
        p.arguments = ["-n", "/usr/bin/" + argv[0]] + Array(argv.dropFirst())
        do { try p.run(); p.waitUntilExit(); return p.terminationStatus }
        catch { return -1 }
    }

    private func notifyUnexpected(code: Int32) {
        let a = NSAlert()
        a.messageText = "OpenFlux: туннель остановился"
        a.informativeText = "Процесс openflux завершился (код \(code)). Маршруты восстановлены системой. Откройте лог для подробностей."
        a.alertStyle = .warning
        a.addButton(withTitle: "OK")
        a.runModal()
    }
}

// MARK: - App delegate

final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate {
    let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let ctrl = TunnelController()
    var store = Store.load()

    let statusLine   = NSMenuItem(title: "Отключено", action: nil, keyEquivalent: "")
    let ipLine       = NSMenuItem(title: "IP: —", action: nil, keyEquivalent: "")
    let profilesItem = NSMenuItem(title: "Профиль", action: nil, keyEquivalent: "")
    let toggleItem   = NSMenuItem(title: "Подключить", action: #selector(toggle), keyEquivalent: "c")
    let loginItem    = NSMenuItem(title: "Запуск при входе", action: #selector(toggleLogin), keyEquivalent: "")

    var logWindow: NSWindow?
    var logTextView: NSTextView?
    var logScroll: NSScrollView?
    var logTimer: Timer?
    /// Last text pushed into the log view, so an unchanged log is not
    /// re-rendered (re-rendering throws away the user's selection).
    var lastLogText = ""

    // Settings controls
    var settingsWindow: NSWindow?
    var fProfiles: NSPopUpButton!
    var fName: NSTextField!
    var fTransport: NSPopUpButton!
    var fURLLabel: NSTextField!
    var fURLHint: NSTextField!
    var fURL: NSTextView!
    var fKey: NSTextField!
    var fDNS: NSTextField!
    var draftDNS = ""
    var fToken: NSTextField!
    var fUid: NSTextField!
    var fDebug: NSButton!
    /// Profiles being edited; committed to `store` only when Save is pressed.
    var draft: [Profile] = []
    var draftIndex = 0

    func applicationDidFinishLaunching(_ n: Notification) {
        NSApp.setActivationPolicy(.accessory)
        buildMainMenu()
        ctrl.onState = { [weak self] s in self?.render(s) }

        let menu = NSMenu()
        statusLine.isEnabled = false
        ipLine.isEnabled = false
        menu.addItem(statusLine)
        menu.addItem(ipLine)
        menu.addItem(.separator())
        menu.addItem(profilesItem)
        menu.addItem(.separator())
        toggleItem.target = self
        menu.addItem(toggleItem)
        addItem(menu, "Обновить IP", #selector(refreshIP), "r")
        addItem(menu, "Показать лог", #selector(showLog), "l")
        addItem(menu, "Профили…", #selector(showSettings), ",")
        menu.addItem(.separator())
        loginItem.target = self
        loginItem.state = LoginItem.isEnabled() ? .on : .off
        menu.addItem(loginItem)
        menu.addItem(.separator())
        addItem(menu, "Выход", #selector(quit), "q")
        statusItem.menu = menu

        rebuildProfilesMenu()
        render(.disconnected)
        refreshIP()
    }

    /// A menu-bar-only app has no menu bar of its own, and macOS routes the
    /// standard editing shortcuts through menu items. Without an Edit menu
    /// Cmd+C, Cmd+A and Cmd+V do nothing in our windows — nothing claims them.
    /// The menu is never displayed; it exists so those key equivalents resolve.
    private func buildMainMenu() {
        let main = NSMenu()

        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        let hide = NSMenuItem(title: "Скрыть", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        hide.target = NSApp
        appMenu.addItem(hide)
        appMenu.addItem(.separator())
        let quitMI = NSMenuItem(title: "Выход", action: #selector(quit), keyEquivalent: "q")
        quitMI.target = self
        appMenu.addItem(quitMI)
        appItem.submenu = appMenu
        main.addItem(appItem)

        let editItem = NSMenuItem()
        let edit = NSMenu(title: "Правка")
        // String selectors: these travel the responder chain to whatever text
        // control is focused, and avoid Swift's ambiguity around copy(_:).
        edit.addItem(withTitle: "Отменить", action: Selector(("undo:")), keyEquivalent: "z")
        edit.addItem(withTitle: "Повторить", action: Selector(("redo:")), keyEquivalent: "Z")
        edit.addItem(.separator())
        edit.addItem(withTitle: "Вырезать", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Копировать", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Вставить", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(.separator())
        edit.addItem(withTitle: "Выделить всё", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit
        main.addItem(editItem)

        NSApp.mainMenu = main
    }

    private func addItem(_ menu: NSMenu, _ title: String, _ sel: Selector, _ key: String) {
        let mi = NSMenuItem(title: title, action: sel, keyEquivalent: key)
        mi.target = self
        menu.addItem(mi)
    }

    // MARK: profiles menu

    /// Rebuilds the submenu listing every profile, a check mark on the active
    /// one. Selecting a different profile while connected does not tear the
    /// tunnel down; it takes effect on the next connect.
    func rebuildProfilesMenu() {
        let sub = NSMenu()
        let activeID = store.active?.id
        for p in store.profiles {
            let mi = NSMenuItem(title: p.name.isEmpty ? "(без имени)" : p.name,
                                action: #selector(selectProfile(_:)), keyEquivalent: "")
            mi.target = self
            mi.representedObject = p.id.uuidString
            mi.state = (p.id == activeID) ? .on : .off
            if !p.isComplete {
                mi.title += " — не настроен"
            } else if p.documents.count > 1 {
                mi.title += "  (\(p.carriesDescription))"
            }
            sub.addItem(mi)
        }
        if store.profiles.isEmpty {
            let mi = NSMenuItem(title: "(нет профилей)", action: nil, keyEquivalent: "")
            mi.isEnabled = false
            sub.addItem(mi)
        }
        sub.addItem(.separator())
        let manage = NSMenuItem(title: "Управление профилями…", action: #selector(showSettings), keyEquivalent: "")
        manage.target = self
        sub.addItem(manage)

        profilesItem.submenu = sub
        profilesItem.title = "Профиль: " + (store.active?.name ?? "—")
    }

    @objc func selectProfile(_ sender: NSMenuItem) {
        guard let raw = sender.representedObject as? String, let id = UUID(uuidString: raw) else { return }
        store.selected = id
        store.save()
        rebuildProfilesMenu()
        if ctrl.isActive {
            let a = NSAlert()
            a.messageText = "Профиль переключён"
            a.informativeText = "Туннель сейчас активен. Новый профиль будет использован после переподключения."
            a.addButton(withTitle: "OK")
            a.runModal()
        }
    }

    @objc func toggle() {
        switch ctrl.state {
        case .disconnected:
            guard let p = store.active else {
                warn("Нет профиля", "Создайте профиль в «Профили…» и укажите транспорт и ссылку.")
                return
            }
            guard p.isComplete else {
                warn("Профиль не настроен",
                     p.transport == "oneme"
                        ? "Для транспорта oneme нужен maxToken."
                        : "Укажите хотя бы одну ссылку на документ для профиля «\(p.name)».")
                return
            }
            ctrl.connect(p, dns: store.dns)
        case .connected, .connecting:
            ctrl.disconnect()
        default: break
        }
    }

    private func warn(_ title: String, _ text: String) {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        a.alertStyle = .warning
        a.addButton(withTitle: "OK")
        NSApp.activate(ignoringOtherApps: true)
        a.runModal()
    }

    func statusImage(_ s: TunnelState) -> NSImage? {
        let name: String, color: NSColor
        switch s {
        case .connected:                  name = "circle.fill"; color = .systemGreen
        case .reconnecting:               name = "circle.fill"; color = .systemOrange
        case .connecting, .disconnecting: name = "circle.fill"; color = .systemYellow
        case .disconnected:               name = "circle";      color = .secondaryLabelColor
        }
        let symCfg = NSImage.SymbolConfiguration(pointSize: 13, weight: .regular)
            .applying(NSImage.SymbolConfiguration(paletteColors: [color]))
        let img = NSImage(systemSymbolName: name, accessibilityDescription: "OpenFlux")?
            .withSymbolConfiguration(symCfg)
        img?.isTemplate = false
        return img
    }

    func render(_ s: TunnelState) {
        statusLine.title = "Статус: \(s.rawValue)"
        if let b = statusItem.button {
            b.image = statusImage(s)
            b.title = ""
        }
        switch s {
        case .disconnected:  toggleItem.title = "Подключить"
        case .connected:     toggleItem.title = "Отключить"; refreshIP()
        case .reconnecting:  toggleItem.title = "Отключить"
        case .connecting:    toggleItem.title = "Отключить (идёт подключение)"
        case .disconnecting: toggleItem.title = "Отключаюсь…"
        }
    }

    @objc func refreshIP() {
        ipLine.title = "IP: проверяю…"
        var req = URLRequest(url: URL(string: "https://api.ipify.org")!)
        req.timeoutInterval = 8
        URLSession.shared.dataTask(with: req) { [weak self] data, _, _ in
            let ip = data.flatMap { String(data: $0, encoding: .utf8) } ?? "—"
            DispatchQueue.main.async { self?.ipLine.title = "IP: \(ip)" }
        }.resume()
    }

    // MARK: log window

    @objc func showLog() {
        if logWindow == nil {
            let w = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 760, height: 470),
                             styleMask: [.titled, .closable, .resizable],
                             backing: .buffered, defer: false)
            w.title = "OpenFlux — лог"
            w.isReleasedWhenClosed = false
            w.delegate = self
            w.center()
            let content = w.contentView!

            let bar: CGFloat = 44
            let scroll = NSScrollView(frame: NSRect(x: 0, y: bar,
                                                   width: content.bounds.width,
                                                   height: content.bounds.height - bar))
            scroll.autoresizingMask = [.width, .height]
            scroll.hasVerticalScroller = true
            let tv = NSTextView(frame: scroll.bounds)
            tv.isEditable = false
            tv.isSelectable = true            // selection is what Cmd+C copies
            tv.font = NSFont.monospacedSystemFont(ofSize: 11, weight: .regular)
            tv.autoresizingMask = [.width]
            scroll.documentView = tv
            content.addSubview(scroll)

            let copyAll = NSButton(title: "Копировать всё", target: self, action: #selector(copyWholeLog))
            copyAll.frame = NSRect(x: 12, y: 8, width: 140, height: 28)
            copyAll.autoresizingMask = [.maxXMargin]
            content.addSubview(copyAll)

            let copySel = NSButton(title: "Копировать выделенное", target: self, action: #selector(copySelectedLog))
            copySel.frame = NSRect(x: 160, y: 8, width: 190, height: 28)
            copySel.autoresizingMask = [.maxXMargin]
            content.addSubview(copySel)

            let reveal = NSButton(title: "Показать файл", target: self, action: #selector(revealLogFile))
            reveal.frame = NSRect(x: 358, y: 8, width: 130, height: 28)
            reveal.autoresizingMask = [.maxXMargin]
            content.addSubview(reveal)

            logWindow = w
            logTextView = tv
            logScroll = scroll
        }

        lastLogText = ctrl.log.text()
        logTextView?.string = lastLogText
        logTextView?.scrollToEndOfDocument(nil)

        logTimer?.invalidate()
        logTimer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
            self?.refreshLogView()
        }
        NSApp.activate(ignoringOtherApps: true)
        logWindow?.makeKeyAndOrderFront(nil)
    }

    /// Redraws the log without destroying what the user is doing: an unchanged
    /// log is left alone, a live selection is never clobbered, and the view
    /// only jumps to the end if it was already there.
    private func refreshLogView() {
        guard let w = logWindow, w.isVisible,
              let tv = logTextView, let scroll = logScroll else { return }
        if tv.selectedRange().length > 0 { return }

        let text = ctrl.log.text()
        if text == lastLogText { return }
        lastLogText = text

        let wasAtBottom: Bool = {
            guard let doc = scroll.documentView else { return true }
            return scroll.contentView.bounds.maxY >= doc.bounds.height - 4
        }()
        tv.string = text
        if wasAtBottom { tv.scrollToEndOfDocument(nil) }
    }

    @objc private func copyWholeLog() {
        let pb = NSPasteboard.general
        pb.clearContents()
        pb.setString(ctrl.log.text(), forType: .string)
    }

    @objc private func copySelectedLog() {
        guard let tv = logTextView else { return }
        let range = tv.selectedRange()
        let text = range.length > 0
            ? (tv.string as NSString).substring(with: range)
            : ctrl.log.text()
        let pb = NSPasteboard.general
        pb.clearContents()
        pb.setString(text, forType: .string)
    }

    @objc private func revealLogFile() {
        NSWorkspace.shared.activateFileViewerSelecting([ctrl.log.fileURL])
    }

    // Stop the log refresh timer when its window closes. Windows keep
    // isReleasedWhenClosed = false, so reopening is safe.
    func windowWillClose(_ notification: Notification) {
        if let w = notification.object as? NSWindow, w == logWindow {
            logTimer?.invalidate()
            logTimer = nil
        }
    }

    // MARK: profiles window

    @objc func showSettings() {
        if settingsWindow == nil { buildSettingsWindow() }
        draft = store.profiles
        if draft.isEmpty { draft = [Profile()] }
        draftDNS = store.dns
        draftIndex = draft.firstIndex(where: { $0.id == store.active?.id }) ?? 0
        reloadProfilePopup()
        loadFields()
        NSApp.activate(ignoringOtherApps: true)
        settingsWindow?.center()
        settingsWindow?.makeKeyAndOrderFront(nil)
    }

    private func buildSettingsWindow() {
        let w = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 520, height: 600),
                         styleMask: [.titled, .closable],
                         backing: .buffered, defer: false)
        w.title = "OpenFlux — профили"
        w.isReleasedWhenClosed = false
        w.delegate = self
        let v = w.contentView!

        func label(_ text: String, _ y: CGFloat) {
            let l = NSTextField(labelWithString: text)
            l.frame = NSRect(x: 16, y: y, width: 110, height: 20)
            l.alignment = .right
            v.addSubview(l)
        }
        func field(_ y: CGFloat) -> NSTextField {
            let f = NSTextField(frame: NSRect(x: 132, y: y - 2, width: 364, height: 24))
            v.addSubview(f); return f
        }

        label("Профиль:", 556)
        fProfiles = NSPopUpButton(frame: NSRect(x: 132, y: 552, width: 280, height: 26))
        fProfiles.target = self
        fProfiles.action = #selector(switchDraftProfile)
        v.addSubview(fProfiles)
        let add = NSButton(title: "+", target: self, action: #selector(addProfile))
        add.frame = NSRect(x: 420, y: 552, width: 36, height: 26)
        v.addSubview(add)
        let del = NSButton(title: "−", target: self, action: #selector(deleteProfile))
        del.frame = NSRect(x: 460, y: 552, width: 36, height: 26)
        v.addSubview(del)

        label("Название:", 516);  fName = field(516)
        label("Транспорт:", 476)
        fTransport = NSPopUpButton(frame: NSRect(x: 132, y: 472, width: 220, height: 26))
        fTransport.addItems(withTitles: Profile.transports)
        fTransport.target = self
        fTransport.action = #selector(transportChanged)
        v.addSubview(fTransport)

        // Several documents, one per line: the field has to be multi-line,
        // because ten of them on a single line is unreadable and unfixable.
        // The label and the hint below it are set from the transport: for
        // cups.online this field is not a list of documents at all.
        fURLLabel = NSTextField(labelWithString: "Ссылки:")
        fURLLabel.frame = NSRect(x: 16, y: 416, width: 110, height: 20)
        fURLLabel.alignment = .right
        v.addSubview(fURLLabel)
        let urlScroll = NSScrollView(frame: NSRect(x: 132, y: 336, width: 364, height: 100))
        urlScroll.hasVerticalScroller = true
        urlScroll.borderType = .bezelBorder
        let tv = NSTextView(frame: urlScroll.bounds)
        tv.isEditable = true
        tv.isRichText = false
        tv.font = NSFont.monospacedSystemFont(ofSize: 11, weight: .regular)
        tv.isAutomaticQuoteSubstitutionEnabled = false
        tv.autoresizingMask = [.width]
        urlScroll.documentView = tv
        v.addSubview(urlScroll)
        fURL = tv

        fURLHint = NSTextField(labelWithString: "")
        fURLHint.frame = NSRect(x: 132, y: 310, width: 364, height: 28)
        fURLHint.textColor = .secondaryLabelColor
        fURLHint.font = NSFont.systemFont(ofSize: 10)
        fURLHint.maximumNumberOfLines = 2
        fURLHint.lineBreakMode = .byWordWrapping
        v.addSubview(fURLHint)

        label("Файл ключа:", 280);  fKey = field(280)
        label("maxToken:", 240);    fToken = field(240)
        label("maxUid:", 200);      fUid = field(200)

        // Below the profile fields: this one belongs to the machine, not to
        // any profile, and the separator says so.
        let sep = NSBox(frame: NSRect(x: 16, y: 160, width: 480, height: 1))
        sep.boxType = .separator
        v.addSubview(sep)
        label("DNS-сервер:", 128);  fDNS = field(128)
        let dnsHint = NSTextField(labelWithString:
            "общий для всех профилей. Пусто — системный резолвер не трогаем, всё как обычно")
        dnsHint.frame = NSRect(x: 132, y: 108, width: 364, height: 16)
        dnsHint.textColor = .secondaryLabelColor
        dnsHint.font = NSFont.systemFont(ofSize: 10)
        v.addSubview(dnsHint)

        fDebug = NSButton(checkboxWithTitle: "Debug-логи (подробный лог)", target: nil, action: nil)
        fDebug.frame = NSRect(x: 132, y: 50, width: 320, height: 20)
        v.addSubview(fDebug)

        let note = NSTextField(labelWithString: "Ключ и DNS необязательны. Оба поля можно оставить пустыми.")
        note.toolTip = "Поле DNS подменяет системный резолвер, пока туннель поднят, и возвращает прежний при выходе. "
            + "Само по себе разрешение имён через туннель работает и без него: туннель несёт только TCP, "
            + "поэтому запросы к UDP:53, попадающие в него, переспрашиваются по TCP в любом случае."
        note.frame = NSRect(x: 16, y: 28, width: 488, height: 18)
        note.textColor = .secondaryLabelColor
        note.font = NSFont.systemFont(ofSize: 11)
        v.addSubview(note)

        let save = NSButton(title: "Сохранить", target: self, action: #selector(saveSettings))
        save.frame = NSRect(x: 396, y: -2, width: 100, height: 32)
        save.keyEquivalent = "\r"
        v.addSubview(save)
        let cancel = NSButton(title: "Отмена", target: self, action: #selector(closeSettings))
        cancel.frame = NSRect(x: 300, y: -2, width: 88, height: 32)
        v.addSubview(cancel)

        settingsWindow = w
    }

    /// The URL field means a different thing for different transports:
    /// cups.online takes one packed string of rooms the node printed at
    /// startup; jitsi takes host/room links to Meet servers, one per line;
    /// everything else takes one document link per line. The field's label
    /// and its hint have to reflect that, or half the settings say the wrong
    /// thing.
    @objc private func transportChanged() {
        let t = fTransport.titleOfSelectedItem
        switch t {
        case "cupsonline":
            fURLLabel.stringValue = "Комнаты:"
            fURLHint.stringValue = "строку комнат печатает нода при запуске — скопируйте её оттуда целиком"
        case "jitsi":
            fURLLabel.stringValue = "Комнаты:"
            fURLHint.stringValue = "по одной комнате в строке в виде host/имя-комнаты; тот же набор должен стоять на ноде"
        default:
            fURLLabel.stringValue = "Ссылки:"
            fURLHint.stringValue = "по одной ссылке в строке; тот же набор должен стоять на ноде"
        }
    }

    private func reloadProfilePopup() {
        fProfiles.removeAllItems()
        for p in draft { fProfiles.addItem(withTitle: p.name.isEmpty ? "(без имени)" : p.name) }
        if draftIndex < fProfiles.numberOfItems { fProfiles.selectItem(at: draftIndex) }
    }

    /// Copies the form into the profile being edited, so switching profiles or
    /// saving never silently drops what was typed.
    private func commitFields() {
        guard draft.indices.contains(draftIndex) else { return }
        draft[draftIndex].name = fName.stringValue.trimmingCharacters(in: .whitespaces)
        draft[draftIndex].transport = fTransport.titleOfSelectedItem ?? draft[draftIndex].transport
        draft[draftIndex].url = fURL.string.trimmingCharacters(in: .whitespacesAndNewlines)
        draft[draftIndex].keyFile = fKey.stringValue.trimmingCharacters(in: .whitespaces)
        draft[draftIndex].maxToken = fToken.stringValue.trimmingCharacters(in: .whitespaces)
        draft[draftIndex].maxUid = fUid.stringValue.trimmingCharacters(in: .whitespaces)
        draft[draftIndex].debug = (fDebug.state == .on)
        // Not part of the profile being edited: one value for all of them.
        draftDNS = fDNS.stringValue.trimmingCharacters(in: .whitespaces)
    }

    private func loadFields() {
        guard draft.indices.contains(draftIndex) else { return }
        let p = draft[draftIndex]
        fName.stringValue = p.name
        fTransport.selectItem(withTitle: p.transport)
        if fTransport.selectedItem == nil { fTransport.selectItem(at: 0) }
        transportChanged()
        fURL.string = p.url
        fKey.stringValue = p.keyFile
        fToken.stringValue = p.maxToken
        fUid.stringValue = p.maxUid
        fDNS.stringValue = draftDNS
        fDebug.state = p.debug ? .on : .off
    }

    @objc private func switchDraftProfile() {
        commitFields()
        draftIndex = fProfiles.indexOfSelectedItem
        loadFields()
        reloadProfilePopup()
    }

    @objc private func addProfile() {
        commitFields()
        var p = Profile()
        p.name = "Профиль \(draft.count + 1)"
        draft.append(p)
        draftIndex = draft.count - 1
        reloadProfilePopup()
        loadFields()
    }

    @objc private func deleteProfile() {
        guard draft.count > 1 else {
            warn("Нельзя удалить", "Должен остаться хотя бы один профиль.")
            return
        }
        draft.remove(at: draftIndex)
        draftIndex = max(0, draftIndex - 1)
        reloadProfilePopup()
        loadFields()
    }

    @objc private func saveSettings() {
        commitFields()
        for i in draft.indices where draft[i].name.isEmpty {
            draft[i].name = "Профиль \(i + 1)"
        }
        let previous = store.selected
        store.profiles = draft
        store.dns = draftDNS
        // Keep the selection if it still exists, otherwise fall back.
        store.selected = draft.contains(where: { $0.id == previous }) ? previous : draft.first?.id
        store.save()
        rebuildProfilesMenu()
        settingsWindow?.close()
        if ctrl.isActive {
            let a = NSAlert()
            a.messageText = "Профили сохранены"
            a.informativeText = "Туннель сейчас активен. Изменения применятся после переподключения."
            a.addButton(withTitle: "OK")
            a.runModal()
        }
    }

    @objc private func closeSettings() { settingsWindow?.close() }

    // MARK: login item / quit

    @objc func toggleLogin() {
        let now = LoginItem.isEnabled()
        LoginItem.setEnabled(!now, execPath: LoginItem.selfPath())
        loginItem.state = LoginItem.isEnabled() ? .on : .off
    }

    @objc func quit() {
        // Quitting while the channel is down must still tear the tunnel down:
        // otherwise openflux outlives the app with the routes still installed.
        if ctrl.isActive {
            ctrl.disconnect()
            usleep(1_500_000)
        }
        NSApp.terminate(nil)
    }
}

// MARK: - Login item (LaunchAgent)

enum LoginItem {
    static let label = "com.openflux.menubar"
    static var plistPath: String {
        (NSHomeDirectory() as NSString).appendingPathComponent("Library/LaunchAgents/\(label).plist")
    }
    static func selfPath() -> String {
        let arg0 = CommandLine.arguments[0]
        if arg0.hasPrefix("/") { return arg0 }
        let cwd = FileManager.default.currentDirectoryPath
        return URL(fileURLWithPath: cwd).appendingPathComponent(arg0).standardizedFileURL.path
    }
    static func isEnabled() -> Bool { FileManager.default.fileExists(atPath: plistPath) }
    static func setEnabled(_ on: Bool, execPath: String) {
        let dir = (NSHomeDirectory() as NSString).appendingPathComponent("Library/LaunchAgents")
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        if on {
            let plist = """
            <?xml version="1.0" encoding="UTF-8"?>
            <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
            <plist version="1.0">
            <dict>
              <key>Label</key><string>\(label)</string>
              <key>ProgramArguments</key>
              <array><string>\(execPath)</string></array>
              <key>RunAtLoad</key><true/>
              <key>ProcessType</key><string>Interactive</string>
            </dict>
            </plist>
            """
            try? plist.write(toFile: plistPath, atomically: true, encoding: .utf8)
        } else {
            try? FileManager.default.removeItem(atPath: plistPath)
        }
    }
}

// MARK: - main

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.run()
