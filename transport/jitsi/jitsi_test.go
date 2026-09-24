package jitsi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	j "openflux/transport/jitsi/upstream"

	"openflux/transport"
)

// TestURLParseAcceptsEveryShapeAnOperatorMightPaste — we accept a bare
// host+room, a jitsi:// URL, a https:// URL from a browser, and forgive a
// trailing slash or a #config fragment. Missing pieces come back as empty
// strings so Start can refuse cleanly.
func TestURLParseAcceptsEveryShapeAnOperatorMightPaste(t *testing.T) {
	cases := []struct {
		in         string
		host, room string
	}{
		{"meet.example.org/room", "meet.example.org", "room"},
		{"jitsi://meet.example.org/room", "meet.example.org", "room"},
		{"https://meet.example.org/room", "meet.example.org", "room"},
		{"https://meet.example.org/room/", "meet.example.org", "room"},
		{"https://meet.example.org/room#config.disableThirdPartyRequests=true", "meet.example.org", "room"},
		{"meet.example.org", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		host, room := parseJitsiURL(c.in)
		if host != c.host || room != c.room {
			t.Errorf("parseJitsiURL(%q) = (%q, %q); want (%q, %q)", c.in, host, room, c.host, c.room)
		}
	}
}

// TestSendRefusesWhenTheQueueIsFull — the transport must not buffer without
// bound. A queue full is what forces TCP inside the tunnel to notice and
// throttle back; the old behaviour of blocking here turned a short outage
// into a much longer stall of every stream.
func TestSendRefusesWhenTheQueueIsFull(t *testing.T) {
	cfg := transport.DefaultConfig()
	cfg.MaxQueueSize = 4
	tr := NewJitsiTransport("meet.example.org/room", cfg)

	// The writer is not running (Start is not called), so nothing drains.
	// Fill the queue exactly, then expect the next Send to be refused.
	for i := 0; i < cfg.MaxQueueSize; i++ {
		if err := tr.Send([]byte("x")); err != nil {
			t.Fatalf("Send #%d unexpectedly refused: %v", i, err)
		}
	}
	if err := tr.Send([]byte("x")); err == nil {
		t.Fatal("Send did not refuse on a full queue")
	} else if !strings.Contains(err.Error(), "queue full") {
		t.Fatalf("Send refused with the wrong error: %v", err)
	}
}

// TestSendRefusesOversizePayload — the bridge silently drops anything above
// 16 KiB, and the caller only hears about it if we refuse here.
func TestSendRefusesOversizePayload(t *testing.T) {
	cfg := transport.DefaultConfig()
	tr := NewJitsiTransport("meet.example.org/room", cfg)

	if err := tr.Send(make([]byte, maxPayloadBytes)); err != nil {
		t.Fatalf("Send of the largest legal payload was refused: %v", err)
	}
	if err := tr.Send(make([]byte, maxPayloadBytes+1)); err == nil {
		t.Fatal("Send did not refuse a payload past the bridge limit")
	}
}

// TestStartRefusesAnEmptyURL — Start must not silently launch a supervisor
// against nothing. This is what parseJitsiURL's empty return promises.
func TestStartRefusesAnEmptyURL(t *testing.T) {
	tr := NewJitsiTransport("", transport.DefaultConfig())
	if err := tr.Start(); err == nil {
		t.Fatal("Start accepted an empty URL")
	}
}

// TestExtractPayloadReadsThePrimaryPathAndTheFallback — different JVB builds
// put the base64 payload in different places; both must reach the caller, or
// half of Jitsi's servers become unusable for us.
func TestExtractPayloadReadsThePrimaryPathAndTheFallback(t *testing.T) {
	want := []byte("hello")
	enc := base64.StdEncoding.EncodeToString(want)

	primary := j.BridgeMessage{
		Class: "EndpointMessage",
		Fields: map[string]any{
			"colibriClass": "EndpointMessage",
			"msgPayload":   map[string]any{"raw": enc},
		},
	}
	if got := extractPayload(primary); !bytes.Equal(got, want) {
		t.Errorf("primary path: got %q want %q", got, want)
	}

	fallback := j.BridgeMessage{
		Class: "EndpointMessage",
		Fields: map[string]any{
			"colibriClass": "EndpointMessage",
			"raw":          enc,
		},
	}
	if got := extractPayload(fallback); !bytes.Equal(got, want) {
		t.Errorf("fallback path: got %q want %q", got, want)
	}
}

// TestExtractPayloadIgnoresEveryOtherFrame — every non-EndpointMessage frame
// (LastNEndpointsChanged, EndpointConnectivityStatus, …) must return nil, so
// the reader loop drops them silently rather than forwarding garbage into
// the codec.
func TestExtractPayloadIgnoresEveryOtherFrame(t *testing.T) {
	for _, class := range []string{"LastNEndpointsChanged", "EndpointConnectivityStatus", "DominantSpeakerEndpointChangeEvent", ""} {
		m := j.BridgeMessage{Class: class, Fields: map[string]any{"colibriClass": class}}
		if got := extractPayload(m); got != nil {
			t.Errorf("class %q leaked %d bytes into the reader", class, len(got))
		}
	}
}

// TestExtractPayloadRejectsMalformedBase64 — a corrupt frame must not crash
// or return partial bytes; it must be dropped like any other unusable frame.
func TestExtractPayloadRejectsMalformedBase64(t *testing.T) {
	m := j.BridgeMessage{
		Class: "EndpointMessage",
		Fields: map[string]any{
			"colibriClass": "EndpointMessage",
			"msgPayload":   map[string]any{"raw": "not-base64!!!"},
		},
	}
	if got := extractPayload(m); got != nil {
		t.Errorf("malformed base64 returned %d bytes; want nil", len(got))
	}
}

// TestFromResourceExtractsTheNickFromAMUCJID — JVB tags every frame with the
// sender's full JID (room@conference.host/nickname). Our echo filter reads
// only the nickname, so this is what the filter compares against.
func TestFromResourceExtractsTheNickFromAMUCJID(t *testing.T) {
	cases := map[string]string{
		"room@conference.meet.example.org/of1a2b3c4d": "of1a2b3c4d",
		"":                             "",
		"noresource":                   "noresource",
		"a/b/c":                        "c",
	}
	for in, want := range cases {
		if got := fromResource(in); got != want {
			t.Errorf("fromResource(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestEndpointMessageJSONOrder — JVB builds using Jackson can lose
// msgPayload when it precedes "to" in the JSON body. A struct fixes the
// order at compile time; this test locks it in so a well-meaning refactor
// to a map (which reorders alphabetically) is caught before it ships.
func TestEndpointMessageJSONOrder(t *testing.T) {
	raw, err := jsonForTest([]byte("payload"))
	if err != nil {
		t.Fatalf("jsonForTest: %v", err)
	}
	// colibriClass, then to, then msgPayload — in that order.
	iClass := bytes.Index(raw, []byte(`"colibriClass"`))
	iTo := bytes.Index(raw, []byte(`"to"`))
	iPayload := bytes.Index(raw, []byte(`"msgPayload"`))
	if iClass < 0 || iTo < 0 || iPayload < 0 {
		t.Fatalf("missing fields in %s", raw)
	}
	if !(iClass < iTo && iTo < iPayload) {
		t.Errorf("wrong field order in wire frame: %s", raw)
	}

	// And the payload round-trips through base64.
	var back endpointMessage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := base64.StdEncoding.DecodeString(back.MsgPayload.Raw)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("payload round-trip: got %q want %q", got, "payload")
	}
}

// TestNickIsUnique — two links built in the same nanosecond must not share
// a nickname, because a shared nickname means the echo filter drops the
// peer's traffic thinking it is our own.
func TestNickIsUnique(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		n := randNick()
		if seen[n] {
			t.Fatalf("nick %q appeared twice in a tight loop", n)
		}
		seen[n] = true
	}
}

// TestSetLabelReplacesTheNameInLogs — a bond gives each link a label so a
// failure says which document dropped; without a label the transport falls
// back to its own kind.
func TestSetLabelReplacesTheNameInLogs(t *testing.T) {
	tr := NewJitsiTransport("meet.example.org/room", transport.DefaultConfig())
	if got := tr.name(); got != "jitsi" {
		t.Errorf("default name = %q; want jitsi", got)
	}
	tr.SetLabel("jitsi 1 of 3")
	if got := tr.name(); got != "jitsi 1 of 3" {
		t.Errorf("labelled name = %q; want jitsi 1 of 3", got)
	}
}

// TestStopWithoutStartIsSafe — Stop must not panic when the transport never
// started, because a bond may Stop several links after one of them fails to
// build.
func TestStopWithoutStartIsSafe(t *testing.T) {
	tr := NewJitsiTransport("meet.example.org/room", transport.DefaultConfig())
	if err := tr.Stop(); err != nil {
		t.Errorf("Stop before Start returned %v", err)
	}
}
