// OpenFluxMenu — menu-bar controller for the OpenFlux macOS tun client.
// Compiled with swiftc (no Xcode) and packaged into OpenFlux.app by build.sh.
// It launches /usr/local/bin/openflux under passwordless sudo, shows status and
// the external IP, streams the log, and exposes settings (transport / URL / key).
// Route cleanup on stop and on crash is done by openflux / the kernel.

import AppKit
import Foundation

// MARK: - Configuration (persisted in UserDefaults)

struct Config {
    var binary    = "/usr/local/bin/openflux"
    // No defaults for the document URL or the key: they identify a specific
    // tunnel and belong to the operator, not to the source tree. Both are set
    // in Settings and persist in UserDefaults.
    var url       = ""
    var transport = "mailru"
    var keyFile   = ""
    var dns       = ""
    var maxToken  = ""
    var maxUid    = ""
    var debug     = false

    static let transports = ["mailru", "vyandex", "yandex", "cupsonline", "oneme"]

    static func load() -> Config {
        var c = Config()
        let d = UserDefaults.standard
        if let v = d.string(forKey: "binary")    { c.binary = v }
        if let v = d.string(forKey: "url")        { c.url = v }
        if let v = d.string(forKey: "transport")  { c.transport = v }
        if let v = d.string(forKey: "keyFile")    { c.keyFile = v }
        if let v = d.string(forKey: "dns")         { c.dns = v }
        if let v = d.string(forKey: "maxToken")   { c.maxToken = v }
        if let v = d.string(forKey: "maxUid")     { c.maxUid = v }
        if d.object(forKey: "debug") != nil       { c.debug = d.bool(forKey: "debug") }
        return c
    }

    func save() {
        let d = UserDefaults.standard
        d.set(binary, forKey: "binary")
        d.set(url, forKey: "url")
        d.set(transport, forKey: "transport")
        d.set(keyFile, forKey: "keyFile")
        d.set(dns, forKey: "dns")
        d.set(maxToken, forKey: "maxToken")
        d.set(maxUid, forKey: "maxUid")
        d.set(debug, forKey: "debug")
    }
}

// MARK: - State

enum TunnelState: String {
    case disconnected  = "Отключено"
    case connecting    = "Подключаюсь…"
    case connected     = "Подключено"
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
    private(set) var state: TunnelState = .disconnected
    private var proc: Process?
    private var requestedStop = false
    let log = LogStore()
    var onState: ((TunnelState) -> Void)?

    private func setState(_ s: TunnelState) {
        state = s
        DispatchQueue.main.async { self.onState?(s) }
    }

    func connect(_ cfg: Config) {
        guard state == .disconnected else { return }
        requestedStop = false
        setState(.connecting)
        log.startSession(header: "connect \(cfg.transport) \(Date())")

        var args = ["-n", cfg.binary,
                    "--role=client", "--inbound=tun",
                    "--transport=\(cfg.transport)",
                    "--url=\(cfg.url)"]
        // Encryption is optional: an empty key file means the flag is omitted,
        // so the transport runs unencrypted. The node must match (also keyless).
        let key = cfg.keyFile.trimmingCharacters(in: .whitespaces)
        if !key.isEmpty          { args.append("--encryption-key-file=\(key)") }
        // A resolver behind the exit node: the client turns the OS's UDP
        // queries into DNS-over-TCP through the tunnel and restores the
        // previous system resolver when it stops.
        let dns = cfg.dns.trimmingCharacters(in: .whitespaces)
        if !dns.isEmpty          { args.append("--dns=\(dns)") }
        if !cfg.maxToken.isEmpty { args.append("--maxToken=\(cfg.maxToken)") }
        if !cfg.maxUid.isEmpty   { args.append("--maxUid=\(cfg.maxUid)") }
        if cfg.debug             { args.append("--debug") }

        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sudo")
        p.arguments = args
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        pipe.fileHandleForReading.readabilityHandler = { [weak self] h in
            let d = h.availableData
            guard let self = self, !d.isEmpty else { return }
            self.log.append(d)
            if let s = String(data: d, encoding: .utf8), s.contains("Tunnel active") {
                self.setState(.connected)
            }
        }
        p.terminationHandler = { [weak self] proc in
            guard let self = self else { return }
            pipe.fileHandleForReading.readabilityHandler = nil
            self.log.append("\n==== openflux exited (code \(proc.terminationStatus)) ====\n".data(using: .utf8)!)
            self.log.closeSession()
            let wasRequested = self.requestedStop
            self.proc = nil
            self.setState(.disconnected)
            if !wasRequested {
                DispatchQueue.main.async { self.notifyUnexpected(code: proc.terminationStatus) }
            }
        }
        do { try p.run(); proc = p }
        catch {
            log.append("failed to launch: \(error)\n".data(using: .utf8)!)
            setState(.disconnected)
        }
    }

    func disconnect() {
        guard state == .connected || state == .connecting else { return }
        requestedStop = true
        setState(.disconnecting)
        DispatchQueue.global().async {
            self.runSudo(["pkill", "-TERM", "-f", "/usr/local/bin/openflux"])
            for _ in 0..<12 {
                if self.proc == nil { return }
                usleep(500_000)
            }
            self.runSudo(["pkill", "-KILL", "-f", "/usr/local/bin/openflux"])
        }
    }

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
    var cfg = Config.load()

    let statusLine = NSMenuItem(title: "Отключено", action: nil, keyEquivalent: "")
    let ipLine     = NSMenuItem(title: "IP: —", action: nil, keyEquivalent: "")
    let toggleItem = NSMenuItem(title: "Подключить", action: #selector(toggle), keyEquivalent: "c")
    let loginItem  = NSMenuItem(title: "Запуск при входе", action: #selector(toggleLogin), keyEquivalent: "")

    var logWindow: NSWindow?
    var logTextView: NSTextView?
    var logTimer: Timer?

    // Settings controls
    var settingsWindow: NSWindow?
    var fTransport: NSPopUpButton!
    var fURL: NSTextField!
    var fKey: NSTextField!
    var fDNS: NSTextField!
    var fToken: NSTextField!
    var fUid: NSTextField!
    var fDebug: NSButton!

    func applicationDidFinishLaunching(_ n: Notification) {
        NSApp.setActivationPolicy(.accessory)
        ctrl.onState = { [weak self] s in self?.render(s) }

        let menu = NSMenu()
        statusLine.isEnabled = false
        ipLine.isEnabled = false
        menu.addItem(statusLine)
        menu.addItem(ipLine)
        menu.addItem(.separator())
        toggleItem.target = self
        menu.addItem(toggleItem)
        addItem(menu, "Обновить IP", #selector(refreshIP), "r")
        addItem(menu, "Показать лог", #selector(showLog), "l")
        addItem(menu, "Настройки…", #selector(showSettings), ",")
        menu.addItem(.separator())
        loginItem.target = self
        loginItem.state = LoginItem.isEnabled() ? .on : .off
        menu.addItem(loginItem)
        menu.addItem(.separator())
        addItem(menu, "Выход", #selector(quit), "q")
        statusItem.menu = menu

        render(.disconnected)
        refreshIP()
    }

    private func addItem(_ menu: NSMenu, _ title: String, _ sel: Selector, _ key: String) {
        let mi = NSMenuItem(title: title, action: sel, keyEquivalent: key)
        mi.target = self
        menu.addItem(mi)
    }

    @objc func toggle() {
        switch ctrl.state {
        case .disconnected: ctrl.connect(cfg)
        case .connected, .connecting: ctrl.disconnect()
        default: break
        }
    }

    func statusImage(_ s: TunnelState) -> NSImage? {
        let name: String, color: NSColor
        switch s {
        case .connected:                  name = "circle.fill"; color = .systemGreen
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
            let w = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 720, height: 420),
                             styleMask: [.titled, .closable, .resizable],
                             backing: .buffered, defer: false)
            w.title = "OpenFlux — лог"
            w.isReleasedWhenClosed = false
            w.delegate = self
            w.center()
            let scroll = NSScrollView(frame: w.contentView!.bounds)
            scroll.autoresizingMask = [.width, .height]
            scroll.hasVerticalScroller = true
            let tv = NSTextView(frame: scroll.bounds)
            tv.isEditable = false
            tv.font = NSFont.monospacedSystemFont(ofSize: 11, weight: .regular)
            tv.autoresizingMask = [.width]
            scroll.documentView = tv
            w.contentView?.addSubview(scroll)
            logWindow = w
            logTextView = tv
        }
        logTextView?.string = ctrl.log.text()
        logTextView?.scrollToEndOfDocument(nil)
        logTimer?.invalidate()
        logTimer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
            guard let self = self, let w = self.logWindow, w.isVisible else { return }
            self.logTextView?.string = self.ctrl.log.text()
            self.logTextView?.scrollToEndOfDocument(nil)
        }
        NSApp.activate(ignoringOtherApps: true)
        logWindow?.makeKeyAndOrderFront(nil)
    }

    // Stop the log refresh timer when its window closes, so no callback fires
    // against a hidden window. Windows keep isReleasedWhenClosed = false, so the
    // references stay valid and reopening is safe.
    func windowWillClose(_ notification: Notification) {
        if let w = notification.object as? NSWindow, w == logWindow {
            logTimer?.invalidate()
            logTimer = nil
        }
    }

    // MARK: settings window

    @objc func showSettings() {
        if settingsWindow == nil { buildSettingsWindow() }
        loadSettingsIntoFields()
        NSApp.activate(ignoringOtherApps: true)
        settingsWindow?.center()
        settingsWindow?.makeKeyAndOrderFront(nil)
    }

    private func buildSettingsWindow() {
        let w = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 480, height: 380),
                         styleMask: [.titled, .closable],
                         backing: .buffered, defer: false)
        w.title = "OpenFlux — настройки"
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
            let f = NSTextField(frame: NSRect(x: 134, y: y - 2, width: 330, height: 24))
            v.addSubview(f); return f
        }

        label("Транспорт:", 340)
        fTransport = NSPopUpButton(frame: NSRect(x: 132, y: 336, width: 200, height: 26))
        fTransport.addItems(withTitles: Config.transports)
        v.addSubview(fTransport)

        label("Ссылка (URL):", 302); fURL = field(302)
        label("Файл ключа:", 264);   fKey = field(264)
        label("DNS-сервер:", 226);   fDNS = field(226)
        label("maxToken:", 188);     fToken = field(188)
        label("maxUid:", 150);       fUid = field(150)

        fDebug = NSButton(checkboxWithTitle: "Debug-логи (подробный лог)", target: nil, action: nil)
        fDebug.frame = NSRect(x: 134, y: 112, width: 320, height: 20)
        v.addSubview(fDebug)

        let note = NSTextField(labelWithString: "DNS: пусто = системный. Задан — станет системным на время работы.")
        note.frame = NSRect(x: 16, y: 68, width: 448, height: 18)
        note.textColor = .secondaryLabelColor
        note.font = NSFont.systemFont(ofSize: 11)
        v.addSubview(note)

        let save = NSButton(title: "Сохранить", target: self, action: #selector(saveSettings))
        save.frame = NSRect(x: 366, y: 16, width: 100, height: 32)
        save.keyEquivalent = "\r"
        v.addSubview(save)
        let cancel = NSButton(title: "Отмена", target: self, action: #selector(closeSettings))
        cancel.frame = NSRect(x: 272, y: 16, width: 88, height: 32)
        v.addSubview(cancel)

        settingsWindow = w
    }

    private func loadSettingsIntoFields() {
        fTransport.selectItem(withTitle: cfg.transport)
        if fTransport.selectedItem == nil { fTransport.selectItem(at: 0) }
        fURL.stringValue = cfg.url
        fKey.stringValue = cfg.keyFile
        fDNS.stringValue = cfg.dns
        fToken.stringValue = cfg.maxToken
        fUid.stringValue = cfg.maxUid
        fDebug.state = cfg.debug ? .on : .off
    }

    @objc private func saveSettings() {
        cfg.transport = fTransport.titleOfSelectedItem ?? cfg.transport
        cfg.url = fURL.stringValue.trimmingCharacters(in: .whitespaces)
        cfg.keyFile = fKey.stringValue.trimmingCharacters(in: .whitespaces)
        cfg.dns = fDNS.stringValue.trimmingCharacters(in: .whitespaces)
        cfg.maxToken = fToken.stringValue.trimmingCharacters(in: .whitespaces)
        cfg.maxUid = fUid.stringValue.trimmingCharacters(in: .whitespaces)
        cfg.debug = (fDebug.state == .on)
        cfg.save()
        settingsWindow?.close()
        if ctrl.state == .connected || ctrl.state == .connecting {
            let a = NSAlert()
            a.messageText = "Настройки сохранены"
            a.informativeText = "Туннель сейчас активен. Новые настройки применятся после переподключения."
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
        if ctrl.state == .connected || ctrl.state == .connecting {
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
