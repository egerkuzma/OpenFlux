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

type health struct {
	StagedSize string
	StagedTime string
	StagedSum  string
	BinarySum  string
	SameBuild  bool
	Rooms      string
	Active     string
	Since      string
	Links      string
	Joins      string
	Closures   string
	Watchdog   string
	Batches    string
	BinarySize string
	BinaryTime string
}

var (
	reLinks    = regexp.MustCompile(`Bonded links: (\d+ of \d+ up)`)
	reAnswered = regexp.MustCompile(`join answered in`)
	reUnanswer = regexp.MustCompile(`join unanswered`)
)

// readHealth turns the last twenty minutes of journal into the handful of
// numbers every investigation so far has ended up grepping for.
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

	out, _ := run("journalctl", "-u", unitName, "--since", "20 min ago", "--no-pager")
	var answered, unanswered, closures, watchdog, batches int
	links := "—"
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
			links = m[1]
		}
	}
	h.Links = links
	if total := answered + unanswered; total > 0 {
		h.Joins = fmt.Sprintf("%d из %d без ответа (%d%%)", unanswered, total, unanswered*100/total)
	} else {
		h.Joins = "нет данных"
	}
	h.Closures = strconv.Itoa(closures)
	h.Watchdog = strconv.Itoa(watchdog)
	h.Batches = strconv.Itoa(batches)
	if fi, err := os.Stat(stagedPath); err == nil {
		h.StagedSize = fmt.Sprintf("%.1f МБ", float64(fi.Size())/1e6)
		h.StagedTime = fi.ModTime().Format("2006-01-02 15:04")
		h.StagedSum = sumOf(stagedPath)
	}
	h.BinarySum = sumOf(binaryPath)
	h.SameBuild = h.StagedSum != "" && h.StagedSum == h.BinarySum
	h.Rooms = clientRooms()
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
func sumOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
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

	tpl := template.Must(template.New("page").Parse(pageHTML))

	authed := func(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.URL.Query().Get("t")
			if got == "" {
				got = r.Header.Get("X-Token")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "нет доступа", http.StatusForbidden)
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
		reply(w, r, token, "параметры записаны, перезапускаю — страница вернётся через полминуты", "")
	}))

	http.HandleFunc("/unit", authed(func(w http.ResponseWriter, r *http.Request) {
		switch r.FormValue("action") {
		case "restart":
			afterReply(func() { unitAction("restart") })
			reply(w, r, token, "перезапускаю — страница вернётся через полминуты", "")
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
			reply(w, r, token, "сборка установлена, перезапускаю — страница вернётся через полминуты", "")
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

func redirect(w http.ResponseWriter, r *http.Request, token, ok, problem string) {
	v := url.Values{"t": {token}}
	if ok != "" {
		v.Set("ok", ok)
	}
	if problem != "" {
		v.Set("err", problem)
	}
	http.Redirect(w, r, "/?"+v.Encode(), http.StatusSeeOther)
}
