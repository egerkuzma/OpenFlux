package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// reply has two modes, and the one-line check that they both terminate is
// worth having: a careless rename once made it call itself instead of
// redirect, and every action on the panel then killed the process with a stack
// overflow. Nothing on the page worked, and the reason was three frames deep in
// a crash dump.
func TestReplyAnswersBrowsersWithARedirect(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/unit", nil)

	reply(w, r, "тк", "сделано", "")

	if w.Code != 303 {
		t.Errorf("код %d, ожидался 303", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "ok=") {
		t.Errorf("перенаправление без результата: %q", loc)
	}
}

func TestReplyAnswersScriptsWithOneLine(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/upload?fmt=text", nil)

	reply(w, r, "тк", "принято", "")

	if w.Code != 200 {
		t.Errorf("код %d, ожидался 200", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "принято" {
		t.Errorf("тело %q, ожидалось %q", body, "принято")
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("тип %q, ожидался текст", ct)
	}
}

// A failure has to come back as a failure, or a deploy script would treat a
// refused build as a successful one.
func TestReplyMarksFailuresForScripts(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/upload?fmt=text", nil)

	reply(w, r, "тк", "", "файл не исполняемый для Linux")

	if w.Code != 400 {
		t.Errorf("код %d, ожидался 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "не исполняемый") {
		t.Errorf("причина потерялась: %q", w.Body.String())
	}
}

// Settings become a command line, and that line is the whole security boundary
// of the panel: only what the struct describes may reach the node.
func TestFlagLineCarriesOnlyWhatWasAsked(t *testing.T) {
	s := settings{Transport: "mailru", Mode: "l4", Codec: "batched", Encrypt: true}
	line := s.flagLine()
	for _, want := range []string{"--role=exit", "--mode=l4", "--transport=mailru", "--url-file="} {
		if !strings.Contains(line, want) {
			t.Errorf("в строке запуска нет %q: %s", want, line)
		}
	}
	for _, unwanted := range []string{"--debug"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("в строке запуска появилось незапрошенное %q: %s", unwanted, line)
		}
	}

	// cupsonline makes its own channels, so a document list would be a file it
	// never reads.
	c := settings{Transport: "cupsonline", Mode: "l4", Codec: "batched"}
	if strings.Contains(c.flagLine(), "--url-file") {
		t.Errorf("cupsonline получил список документов: %s", c.flagLine())
	}
}

// The panel must refuse what the node would refuse, and for the same reasons,
// rather than writing a configuration that cannot start.
func TestValidateRefusesWhatWouldNotStart(t *testing.T) {
	ok := settings{Transport: "mailru", Mode: "l4", Codec: "batched", Encrypt: true,
		URLs: "https://cloud.mail.ru/public/aaaa/bbbb\nhttps://cloud.mail.ru/public/cccc/dddd"}
	if err := ok.validate(); err != nil {
		t.Fatalf("исправные настройки отвергнуты: %v", err)
	}

	bad := ok
	bad.URLs = "не ссылка вовсе"
	if err := bad.validate(); err == nil {
		t.Error("принята строка, которая не является ссылкой")
	}

	empty := settings{Transport: "mailru", Mode: "l4", Codec: "batched"}
	if err := empty.validate(); err == nil {
		t.Error("mailru принят без единой ссылки")
	}
}

// The room list is the one thing the panel hands back rather than sets, and
// without it a cups.online client has nothing to connect to. A wrong reading
// shows nothing at all, which looks exactly like the node never printing it —
// so the reading is checked against the banner the node actually emits.
func TestExtractRoomsFindsTheList(t *testing.T) {
	journal := strings.Join([]string{
		"2026/09/22 02:10:01 Role: exit",
		"2026/09/22 02:10:02 Transport: cupsonline",
		"",
		"=== COPY THIS TO CLIENT ===",
		"WyJhYmMiLCJkZWYiXQ",
		"===========================",
		"",
		"2026/09/22 02:10:05 Tunnel active",
	}, "\n")

	if got := extractRooms(journal); got != "WyJhYmMiLCJkZWYiXQ" {
		t.Errorf("прочитано %q, ожидалось %q", got, "WyJhYmMiLCJkZWYiXQ")
	}
}

// Every restart makes new rooms, and an old list points at rooms nobody is in.
func TestExtractRoomsPrefersTheNewest(t *testing.T) {
	journal := strings.Join([]string{
		"=== COPY THIS TO CLIENT ===", "СТАРАЯ", "===========================",
		"2026/09/22 02:20:00 перезапуск",
		"=== COPY THIS TO CLIENT ===", "НОВАЯ", "===========================",
	}, "\n")

	if got := extractRooms(journal); got != "НОВАЯ" {
		t.Errorf("прочитано %q — взята не последняя строка", got)
	}
}

func TestExtractRoomsSaysNothingWhenThereIsNothing(t *testing.T) {
	if got := extractRooms("2026/09/22 02:10:01 Transport: mailru\nBonded links: 5 of 5 up"); got != "" {
		t.Errorf("из журнала без комнат прочитано %q", got)
	}
}
