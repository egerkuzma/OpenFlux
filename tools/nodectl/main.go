// Command nodectl is a small control panel for the OpenFlux exit node.
//
// It exists because every experiment so far has cost the same four steps by
// hand: build on the laptop, copy the binary over, install it as root, restart
// the unit — and then grep the journal to find out what happened. This serves
// one page that does all of it, from a machine that can reach the node.
//
// It runs processes on request, so three things are deliberate:
//
//   - Every request carries a token. Without it nothing is served, including
//     the page itself.
//   - It listens on the node's own address only. That address is private, so
//     the panel is reachable from the local network and through the tunnel,
//     and from nowhere else.
//   - Flags are never taken as free text. They are composed here from a fixed
//     set of fields, because the node's flags can name files — an unchecked
//     --encryption-key-file would hand out anything on the machine.
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	unitName   = "openflux"
	flagsFile  = "/etc/openflux/flags"
	urlFile    = "/etc/openflux/url.txt"
	keyFile    = "/etc/openflux/secret.txt"
	binaryPath = "/usr/local/bin/openflux"
	stagedPath = "/var/lib/nodectl/openflux.new"

	maxUpload = 64 << 20 // a Go binary for this project is about 19 MB
)

// settings is everything the panel may change. Anything not here cannot be
// passed to the node, which is the point: the set is the allowlist.
type settings struct {
	Transport string // mailru | yandex | vyandex | cupsonline
	Mode      string // l3 | l4
	Codec     string // batched | legacy
	Debug     bool
	Encrypt   bool
	URLs      string // one per line, written to urlFile
}

// needsDocuments says whether a transport is driven by a list of documents the
// operator supplies, or brings its own channels.
//
// cupsonline is the second kind: on the exit side it creates its own rooms and
// prints the list for the client to use. Asking for documents there would be
// asking for something that does not exist yet — the node is what produces it.
var needsDocuments = map[string]bool{
	"mailru": true, "yandex": true, "vyandex": true, "cupsonline": false,
}

var (
	knownTransports = map[string]bool{"mailru": true, "yandex": true, "vyandex": true, "cupsonline": true}
	knownModes      = map[string]bool{"l3": true, "l4": true}
	knownCodecs     = map[string]bool{"batched": true, "legacy": true}

	// A document URL as the node accepts it. Anything else is refused rather
	// than written to a file the node will read.
	urlOK = regexp.MustCompile(`^https://[A-Za-z0-9./_?=&%:-]+$`)
)

// flagLine renders the settings as the node's command line. It is the only
// place that builds it, and it can only emit flags from the struct above.
func (s settings) flagLine() string {
	f := []string{"--role=exit"}
	f = append(f, "--mode="+s.Mode)
	f = append(f, "--transport="+s.Transport)
	f = append(f, "--codec="+s.Codec)
	if needsDocuments[s.Transport] {
		f = append(f, "--url-file="+urlFile)
	}
	if s.Encrypt {
		f = append(f, "--encryption-key-file="+keyFile)
	}
	if s.Debug {
		f = append(f, "--debug")
	}
	return strings.Join(f, " ")
}

func (s settings) validate() error {
	if !knownTransports[s.Transport] {
		return fmt.Errorf("неизвестный транспорт %q", s.Transport)
	}
	if !knownModes[s.Mode] {
		return fmt.Errorf("неизвестный режим %q", s.Mode)
	}
	if !knownCodecs[s.Codec] {
		return fmt.Errorf("неизвестный кодек %q", s.Codec)
	}
	urls := s.urlList()
	if needsDocuments[s.Transport] && len(urls) == 0 {
		return fmt.Errorf("транспорту %s нужна хотя бы одна ссылка на документ", s.Transport)
	}
	for _, u := range urls {
		if !urlOK.MatchString(u) {
			return fmt.Errorf("ссылка не похожа на адрес документа: %q", u)
		}
		if _, err := url.Parse(u); err != nil {
			return fmt.Errorf("ссылку не разобрать: %q", u)
		}
	}
	return nil
}

func (s settings) urlList() []string {
	var out []string
	for _, l := range strings.Split(s.URLs, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// ---- reading the node's current state ----

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func currentSettings() settings {
	s := settings{Transport: "mailru", Mode: "l4", Codec: "batched", Encrypt: true}
	if b, err := os.ReadFile(flagsFile); err == nil {
		line := string(b)
		if i := strings.Index(line, "="); i >= 0 {
			line = line[i+1:]
		}
		line = strings.Trim(strings.TrimSpace(line), `"`)
		for _, f := range strings.Fields(line) {
			switch {
			case strings.HasPrefix(f, "--transport="):
				s.Transport = strings.TrimPrefix(f, "--transport=")
			case strings.HasPrefix(f, "--mode="):
				s.Mode = strings.TrimPrefix(f, "--mode=")
			case strings.HasPrefix(f, "--codec="):
				s.Codec = strings.TrimPrefix(f, "--codec=")
			case f == "--debug":
				s.Debug = true
			case strings.HasPrefix(f, "--encryption-key-file="):
				s.Encrypt = true
			}
		}
	}
	if b, err := os.ReadFile(urlFile); err == nil {
		s.URLs = strings.TrimSpace(string(b))
	}
	return s
}

// The field names double as the keys the page reads, so a state update is one
// JSON object and no template.
type health struct {
	Log        string `json:"log"`
	StagedSize string `json:"stagedSize"`
	StagedTime string `json:"stagedTime"`
	StagedSum  string `json:"stagedSum"`
	BinarySum  string `json:"binarySum"`
	SameBuild  bool   `json:"sameBuild"`
	Rooms      string `json:"rooms"`
	Active     string `json:"active"`
	Since      string `json:"since"`
	Links      string `json:"links"`
	Joins      string `json:"joins"`
	Closures   string `json:"closures"`
	Watchdog   string `json:"watchdog"`
	Batches    string `json:"batches"`
	BinarySize string `json:"binarySize"`
	BinaryTime string `json:"binaryTime"`
	Scanned    string `json:"scanned"`
}

var (
	reLinks    = regexp.MustCompile(`Bonded links: (\d+ of \d+ up)`)
	reAnswered = regexp.MustCompile(`join answered in`)
	reUnanswer = regexp.MustCompile(`join unanswered`)
)

// readHealth turns the last twenty minutes of journal into the handful of
// numbers every investigation so far has ended up grepping for.
// scanInterval is how often the journal is read. Reading it is what made this
// panel slow: twenty minutes of a node running with --debug is forty thousand
// lines, journalctl needs four and a half seconds to hand them over, and the
// old code paid that on every request — for six counters that only move when
// something goes wrong. A page that takes six seconds to open is one nobody
// opens while the thing they are watching is still happening.
const scanInterval = 20 * time.Second

// journalScan is what the last read of the journal found. Twenty-second-old
// numbers are fine for a health page. What must be exact — whether the unit is
// up, which build is installed — is still read on every request, because those
// are what a person changes and then immediately looks at.
type journalScan struct {
	links, joins, closures, watchdog, batches, rooms string
	taken                                            time.Time
}

var lastScan struct {
	sync.Mutex
	v journalScan
}

// scanJournal does the expensive reading, away from any request.
func scanJournal() journalScan {
	sc := journalScan{taken: time.Now()}
	out, _ := run("journalctl", "-u", unitName, "--since", "20 min ago", "--no-pager")
	var answered, unanswered, closures, watchdog, batches int
	sc.links = "—"
	for _, l := range strings.Split(out, "\n") {
		switch {
		case reAnswered.MatchString(l):
			answered++
		case reUnanswer.MatchString(l):
			unanswered++
		case strings.Contains(l, "connection lost"):
			closures++
		case strings.Contains(l, "never renewed"):
			watchdog++
		case strings.Contains(l, "[BATCH]"):
			batches++
		}
		if m := reLinks.FindStringSubmatch(l); m != nil {
			sc.links = m[1]
		}
	}
	if total := answered + unanswered; total > 0 {
		sc.joins = fmt.Sprintf("%d из %d без ответа (%d%%)", unanswered, total, unanswered*100/total)
	} else {
		sc.joins = "нет данных"
	}
	sc.closures = strconv.Itoa(closures)
	sc.watchdog = strconv.Itoa(watchdog)
	sc.batches = strconv.Itoa(batches)
	sc.rooms = clientRooms()
	return sc
}

// keepScanning refreshes those numbers for as long as the panel runs.
func keepScanning() {
	for {
		sc := scanJournal()
		lastScan.Lock()
		lastScan.v = sc
		lastScan.Unlock()
		time.Sleep(scanInterval)
	}
}

func readHealth() health {
	h := health{Active: "неизвестно"}
	if out, err := run("systemctl", "is-active", unitName); err == nil || out != "" {
		h.Active = out
	}
	if out, err := run("systemctl", "show", unitName, "-p", "ActiveEnterTimestamp", "--value"); err == nil {
		h.Since = out
	}
	if fi, err := os.Stat(binaryPath); err == nil {
		h.BinarySize = fmt.Sprintf("%.1f МБ", float64(fi.Size())/1e6)
		h.BinaryTime = fi.ModTime().Format("2006-01-02 15:04")
	}

	lastScan.Lock()
	sc := lastScan.v
	lastScan.Unlock()
	h.Links, h.Joins = sc.links, sc.joins
	h.Closures, h.Watchdog, h.Batches = sc.closures, sc.watchdog, sc.batches
	h.Rooms = sc.rooms
	if sc.taken.IsZero() {
		h.Scanned = "журнал ещё не прочитан"
	} else {
		h.Scanned = fmt.Sprintf("%d с назад", int(time.Since(sc.taken).Seconds()))
	}
	if fi, err := os.Stat(stagedPath); err == nil {
		h.StagedSize = fmt.Sprintf("%.1f МБ", float64(fi.Size())/1e6)
		h.StagedTime = fi.ModTime().Format("2006-01-02 15:04")
		h.StagedSum = sumOf(stagedPath)
	}
	h.BinarySum = sumOf(binaryPath)
	h.SameBuild = h.StagedSum != "" && h.StagedSum == h.BinarySum
	return h
}

// clientRooms digs out the room list cupsonline prints when it starts.
//
// The exit node creates the rooms itself and writes them to its own output as a
// single packed string; without it the client has nothing to connect to. It is
// the one piece of state this panel exists to hand back rather than set.
func clientRooms() string {
	out, _ := run("journalctl", "-u", unitName, "-n", "4000", "--no-pager", "-o", "cat")
	return extractRooms(out)
}

// extractRooms picks the packed room list out of the node's own output.
//
// Kept apart from the journal call so it can be tested: the format is the exit
// node's banner, three lines with the list in the middle, and if the reading of
// it is wrong the panel simply shows nothing — a silence indistinguishable
// from the node never having printed it.
//
// The newest one wins. Every restart makes new rooms, and an old list points at
// rooms nobody is in.
func extractRooms(journal string) string {
	lines := strings.Split(journal, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.Contains(lines[i], "COPY THIS TO CLIENT") {
			continue
		}
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			v := strings.TrimSpace(lines[j])
			if v == "" || strings.HasPrefix(v, "=") {
				continue
			}
			return v
		}
	}
	return ""
}

// sumOf identifies a build by what is in it.
//
// Dates cannot do this job: installing copies the file and stamps it with the
// current time, so whatever is running always looks newer than whatever was
// sent, even when they are the same build — and after an install the panel
// went on offering a build that was already in place. A hash says plainly
// whether the two are the same thing.
var sums struct {
	sync.Mutex
	m map[string]string
}

func sumOf(path string) string {
	// Hashing two 19 MB builds on every request buys nothing: a file that has
	// not changed cannot have a different hash, and size plus modification
	// time is enough to say so. Installing rewrites the file, which moves both.
	fi, statErr := os.Stat(path)
	var key string
	if statErr == nil {
		key = fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
		sums.Lock()
		cached, ok := sums.m[key]
		sums.Unlock()
		if ok {
			return cached
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	sum := hex.EncodeToString(h.Sum(nil))[:12]
	if key != "" {
		sums.Lock()
		if sums.m == nil {
			sums.m = map[string]string{}
		}
		sums.m[key] = sum
		sums.Unlock()
	}
	return sum
}

func tailLog(n int) string {
	out, _ := run("journalctl", "-u", unitName, "-n", strconv.Itoa(n), "--no-pager", "-o", "short-iso")
	return out
}

// ---- actions ----

func apply(s settings) error {
	if err := s.validate(); err != nil {
		return err
	}
	if err := os.WriteFile(urlFile, []byte(strings.Join(s.urlList(), "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("записать список документов: %w", err)
	}
	line := fmt.Sprintf("OPENFLUX_FLAGS=%s\n", s.flagLine())
	if err := os.WriteFile(flagsFile, []byte(line), 0o644); err != nil {
		return fmt.Errorf("записать параметры: %w", err)
	}
	return nil
}

func unitAction(action string) (string, error) {
	switch action {
	case "start", "stop", "restart":
	default:
		return "", fmt.Errorf("неизвестное действие %q", action)
	}
	return run("sudo", "-n", "/usr/bin/systemctl", action, unitName)
}

// afterReply runs something that will cut the caller off, once the reply has
// had time to reach them.
//
// This panel is only reachable through the tunnel the node provides: the node
// is on a network the laptop cannot otherwise see, and ssh to it goes the same
// way. So restarting the node closes the connection carrying the answer, and
// the browser shows a failure for an action that in fact succeeded. Replying
// first and acting a moment later costs nothing and tells the truth.
func afterReply(do func()) {
	go func() {
		time.Sleep(time.Second)
		do()
	}()
}

// checkStaged says whether what was uploaded can be run by this machine.
//
// The panel takes a file over the network, and the one check worth having is
// that it is a Linux executable rather than whatever else got sent.
func checkStaged() error {
	fi, err := os.Stat(stagedPath)
	if err != nil {
		return fmt.Errorf("залитой сборки нет: %w", err)
	}
	if fi.Size() < 1<<20 {
		return fmt.Errorf("файл слишком мал (%d байт) — вряд ли это бинарник", fi.Size())
	}
	f, err := os.Open(stagedPath)
	if err != nil {
		return err
	}
	var magic [4]byte
	_, _ = io.ReadFull(f, magic[:])
	f.Close()
	if string(magic[:]) != "\x7fELF" {
		return fmt.Errorf("файл не исполняемый для Linux")
	}
	return nil
}

// installStaged puts the uploaded build where the unit will run it. Only the
// panel calls this, and only when someone presses the button.
func installStaged() (string, error) {
	if err := checkStaged(); err != nil {
		return "", err
	}
	return run("sudo", "-n", "/usr/bin/install", "-m755", "-o", "root", "-g", "root", stagedPath, binaryPath)
}

// ---- http ----

type page struct {
	NeedsDocs bool
	Settings  settings
	Health    health
	Log       string
	Notice    string
	Problem   string
	Token     string
}

func main() {
	addr := os.Getenv("NODECTL_ADDR")
	if addr == "" {
		addr = "192.168.1.35:8787"
	}
	token := os.Getenv("NODECTL_TOKEN")
	if len(token) < 16 {
		log.Fatal("NODECTL_TOKEN должен быть задан и содержать хотя бы 16 символов")
	}
	if err := os.MkdirAll("/var/lib/nodectl", 0o755); err != nil {
		log.Fatalf("каталог для заливки: %v", err)
	}

	// The first scan runs before the first page is served, so the panel never
	// opens with empty counters and no explanation for them.
	lastScan.v = scanJournal()
	go keepScanning()

	tpl := template.Must(template.New("page").Parse(pageHTML))

	authed := func(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.URL.Query().Get("t")
			if got == "" {
				got = r.Header.Get("X-Token")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				// Say which of the two it is. Bare "нет доступа" sent someone
				// hunting for a broken panel when the address had simply lost
				// its token — and it used to hide itself, because every action
				// redirected and put the token back in the bar. Nothing
				// navigates now, so an address without it stays that way.
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				if got == "" {
					io.WriteString(w, "В адресе нет токена.\n\n"+
						"Панель открывается так:  http://"+r.Host+"/?t=<токен>\n"+
						"Токен лежит в /etc/systemd/system/nodectl.service на ноде.\n")
					return
				}
				io.WriteString(w, "Токен не подходит.\n")
				return
			}
			h(w, r)
		}
	}

	show := func(w http.ResponseWriter, r *http.Request, notice, problem string) {
		cur := currentSettings()
		p := page{Settings: cur, NeedsDocs: needsDocuments[cur.Transport],
			Health: readHealth(), Log: tailLog(120),
			Notice: notice, Problem: problem, Token: token}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tpl.Execute(w, p); err != nil {
			log.Printf("шаблон: %v", err)
		}
	}

	http.HandleFunc("/", authed(func(w http.ResponseWriter, r *http.Request) {
		show(w, r, r.URL.Query().Get("ok"), r.URL.Query().Get("err"))
	}))

	// The page asks for this every few seconds and redraws itself from it.
	// Nothing here navigates: an action that restarts the node would otherwise
	// send the browser to a page it cannot reach, because when the tunnel is up
	// the route to this panel runs through the node being restarted.
	http.HandleFunc("/state", authed(func(w http.ResponseWriter, r *http.Request) {
		h := readHealth()
		h.Log = tailLog(120)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(h)
	}))

	http.HandleFunc("/apply", authed(func(w http.ResponseWriter, r *http.Request) {
		s := settings{
			Transport: r.FormValue("transport"),
			Mode:      r.FormValue("mode"),
			Codec:     r.FormValue("codec"),
			Debug:     r.FormValue("debug") == "on",
			Encrypt:   r.FormValue("encrypt") == "on",
			URLs:      strings.ReplaceAll(r.FormValue("urls"), "\r\n", "\n"),
		}
		if err := apply(s); err != nil {
			reply(w, r, token, "", err.Error())
			return
		}
		afterReply(func() { unitAction("restart") })
		reply(w, r, token, "параметры записаны, нода перезапускается", "")
	}))

	http.HandleFunc("/unit", authed(func(w http.ResponseWriter, r *http.Request) {
		switch r.FormValue("action") {
		case "restart":
			afterReply(func() { unitAction("restart") })
			reply(w, r, token, "нода перезапускается", "")
		case "start":
			// Starting cannot cut us off, so there is no reason to defer it.
			if out, err := unitAction("start"); err != nil {
				reply(w, r, token, "", "не вышло: "+out)
				return
			}
			reply(w, r, token, "нода запущена", "")
		case "stop":
			// Deferred for the same reason a restart is: the reply travels
			// through the tunnel this is about to take down.
			afterReply(func() { unitAction("stop") })
			reply(w, r, token, "останавливаю — туннеля и этой страницы не будет, пока нода не запущена снова", "")
		case "install":
			out, err := installStaged()
			if err != nil {
				reply(w, r, token, "", err.Error()+" "+out)
				return
			}
			afterReply(func() { unitAction("restart") })
			reply(w, r, token, "сборка установлена, нода перезапускается", "")
		default:
			reply(w, r, token, "", "неизвестное действие")
		}
	}))

	// No form on the page points here. Deploying is a scripted step — build,
	// send, done — and putting a file picker in the panel would only invite a
	// human back into a loop that no longer needs one.
	http.HandleFunc("/upload", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			reply(w, r, token, "", "не принял файл: "+err.Error())
			return
		}
		f, _, err := r.FormFile("binary")
		if err != nil {
			reply(w, r, token, "", "файл не приложен")
			return
		}
		defer f.Close()
		dst, err := os.CreateTemp("/var/lib/nodectl", "upload-*")
		if err != nil {
			reply(w, r, token, "", err.Error())
			return
		}
		n, err := io.Copy(dst, io.LimitReader(f, maxUpload))
		dst.Close()
		if err != nil {
			os.Remove(dst.Name())
			reply(w, r, token, "", err.Error())
			return
		}
		if err := os.Rename(dst.Name(), stagedPath); err != nil {
			reply(w, r, token, "", err.Error())
			return
		}
		// Принято и проверено — и на этом всё. Устанавливать и перезапускать
		// отсюда нельзя: это уронит туннель, а решать когда — тому, кто у
		// панели, а не тому, кто прислал сборку.
		if err := checkStaged(); err != nil {
			reply(w, r, token, "", err.Error())
			return
		}
		reply(w, r, token, fmt.Sprintf("принято %.1f МБ, лежит и ждёт установки в панели", float64(n)/1e6), "")
	}))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("слушать %s: %v", addr, err)
	}
	log.Printf("панель на http://%s/?t=<токен>", addr)
	srv := &http.Server{Handler: http.DefaultServeMux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}

// reply answers a browser with a page and a script with a line of text.
//
// The point of the panel is to take the human out of the deploy loop, and a
// 303 to an HTML page is unusable from a script: curl either follows it into
// markup or reports the redirect as the result. With fmt=text the outcome
// comes back as one line and a status code, which is all a deploy script
// needs to decide whether it worked.
func reply(w http.ResponseWriter, r *http.Request, token, ok, problem string) {
	if r.FormValue("fmt") != "text" {
		redirect(w, r, token, ok, problem)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if problem != "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "не вышло:", problem)
		return
	}
	fmt.Fprintln(w, ok)
}

// redirect sends the browser back to the panel.
//
// Only a problem travels in the address: the page itself shows what happened —
// which build is running, whether the unit is up, what the journal says — so a
// banner repeating "done" adds a line to read and nothing to learn. A failure
// is the opposite: without it the page looks exactly as it did before, and the
// reason would be gone.
func redirect(w http.ResponseWriter, r *http.Request, token, ok, problem string) {
	v := url.Values{"t": {token}}
	if problem != "" {
		v.Set("err", problem)
	}
	http.Redirect(w, r, "/?"+v.Encode(), http.StatusSeeOther)
}
