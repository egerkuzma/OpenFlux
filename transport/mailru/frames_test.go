package mailru

import (
	"strings"
	"testing"
)

// The tail exists to answer one question — what did the server say before it
// closed — so the things that would crowd out the answer must not enter it.
func TestRememberFrameSkipsHeartbeats(t *testing.T) {
	var tail []recentFrame
	for _, msg := range []string{"2", "3", `{"x":"---KA---"}`} {
		tail = rememberFrame(tail, []byte(msg))
	}
	if len(tail) != 0 {
		t.Fatalf("tail holds %d heartbeat frames, want none", len(tail))
	}

	tail = rememberFrame(tail, []byte(`41{"reason":"kicked"}`))
	if len(tail) != 1 {
		t.Fatalf("tail holds %d frames, want the one that matters", len(tail))
	}
	if !strings.Contains(tail[0].text, "kicked") {
		t.Errorf("frame recorded as %q, want the server's own words", tail[0].text)
	}
}

// The payload is the user's traffic and this log gets pasted into chats, so a
// data frame may be counted but never quoted.
func TestRememberFrameNeverRecordsThePayload(t *testing.T) {
	secret := "c2VjcmV0IHR1bm5lbCBwYXlsb2Fk"
	msg := `42["change",{"cursor":"` + secret + `"}]`

	tail := rememberFrame(nil, []byte(msg))
	if len(tail) != 1 {
		t.Fatalf("tail holds %d frames, want 1", len(tail))
	}
	if strings.Contains(tail[0].text, secret) {
		t.Fatal("the payload was written to the log")
	}
	if !strings.Contains(tail[0].text, "байт") {
		t.Errorf("data frame recorded as %q, want its size", tail[0].text)
	}
}

func TestRememberFrameKeepsOnlyTheLastFew(t *testing.T) {
	var tail []recentFrame
	for i := 0; i < recentFrames*3; i++ {
		tail = rememberFrame(tail, []byte(string(rune('a'+i))+"-frame"))
	}
	if len(tail) != recentFrames {
		t.Fatalf("tail holds %d frames, want %d", len(tail), recentFrames)
	}
	// The newest must survive: it is the one closest to the close.
	last := string(rune('a'+recentFrames*3-1)) + "-frame"
	if tail[len(tail)-1].text != last {
		t.Errorf("newest frame is %q, want %q", tail[len(tail)-1].text, last)
	}
}

// The frames are JSON with Russian text in them. A byte-wise cut would produce
// mojibake in the one line someone reads to find out what happened.
func TestTruncateCutsOnCharacters(t *testing.T) {
	s := strings.Repeat("я", 10)
	got := truncate(s, 4)
	if got != "яяяя…" {
		t.Errorf("truncate gave %q, want %q", got, "яяяя…")
	}
	if short := truncate("коротко", 300); short != "коротко" {
		t.Errorf("a short string was changed: %q", short)
	}
}

// The open packet is what the client must wait for before joining the
// namespace. Recognising it wrongly would either hang every connect or bring
// back the premature join this was added to stop.
func TestIsOpenPacket(t *testing.T) {
	open := []byte(`0{"sid":"uNVrP6N_P0QNIqOeHTLT","upgrades":[],"pingInterval":25000}`)
	if !isOpenPacket(open) {
		t.Error("the server's open packet was not recognised")
	}
	for _, other := range []string{
		"2",                        // ping
		"3",                        // pong
		"40",                       // our own namespace join
		"41",                       // the server's goodbye
		`42["change",{"a":1}]`,     // an ordinary event
		"0",                        // a bare 0 is not an open packet
		`{"sid":"no type prefix"}`, // JSON without the packet type
	} {
		if isOpenPacket([]byte(other)) {
			t.Errorf("%q was taken for the open packet", other)
		}
	}
}
