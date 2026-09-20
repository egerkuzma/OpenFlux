package mailru

import (
	"strings"
	"testing"
	"time"
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

// The join acknowledgement decides when a replacement session may take over
// the traffic. Mistaking the goodbye "41" for it would hand traffic to a
// connection the server is closing — the exact failure renewal exists to
// prevent.
func TestIsJoinAck(t *testing.T) {
	for _, ok := range []string{"40", `40{"sid":"abc"}`} {
		if !isJoinAck([]byte(ok)) {
			t.Errorf("%q was not recognised as the join acknowledgement", ok)
		}
	}
	for _, bad := range []string{
		"41",                    // the goodbye
		`41{"reason":"bye"}`,    // the goodbye with a body
		"4",                     // a bare message packet
		"2",                     // ping
		`0{"sid":"abc"}`,        // the Engine.IO open packet
		`42["message",{"a":1}]`, // an ordinary event
	} {
		if isJoinAck([]byte(bad)) {
			t.Errorf("%q was taken for the join acknowledgement", bad)
		}
	}
}

// Renewal has to happen with enough margin that it beats the provider's cut,
// which was measured at 60-68 seconds with a median of 61.
func TestRenewalLeavesMarginBeforeTheCut(t *testing.T) {
	if renewAfter >= sessionLifetime {
		t.Fatalf("renewal at %v is not before the cut at %v", renewAfter, sessionLifetime)
	}
	if margin := sessionLifetime - renewAfter; margin < 10*time.Second {
		t.Errorf("margin is %v; the measured sessions varied by eight seconds around the median", margin)
	}
}
