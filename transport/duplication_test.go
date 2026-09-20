package transport

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
)

// These tests run the real stack the tunnel runs — Encrypted over Batched over
// Bonded — because duplication only works if every layer plays its part: the
// bond writes the copy, batching carries it unchanged, and the encryption
// layer's replay window is what throws it away. Testing the bond alone would
// prove nothing about whether anything above it sees the frame twice.

func (f *fakeLink) frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.sent))
	copy(out, f.sent)
	return out
}

// duplexPair builds a sender over two links and a matching receiver, returning
// the two links, a function to push a wire frame into the receiver, and the
// payloads the receiver has accepted so far.
type duplexPair struct {
	sender   *EncryptedTransport
	linkA    *fakeLink
	linkB    *fakeLink
	deliver  func([]byte)
	received func() [][]byte
}

func newDuplexPair(t *testing.T, mirror bool) *duplexPair {
	t.Helper()
	const secret = "a sufficiently long shared secret"

	a := &fakeLink{connected: true, since: time.Now().Add(-60 * time.Second)}
	b := &fakeLink{connected: true, since: time.Now().Add(-30 * time.Second)}
	bond, err := NewBondedTransport([]Transport{a, b})
	if err != nil {
		t.Fatal(err)
	}
	bond.MirrorFrames(mirror)

	sender, err := NewEncryptedTransport(NewBatchedTransport(bond), secret, "doc", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sender.Stop() })

	wire := &testTransport{}
	receiver, err := NewEncryptedTransport(NewBatchedTransport(wire), secret, "doc", true)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got [][]byte
	receiver.Receive(func(p []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), p...))
		mu.Unlock()
	})

	return &duplexPair{
		sender:  sender,
		linkA:   a,
		linkB:   b,
		deliver: wire.deliver,
		received: func() [][]byte {
			mu.Lock()
			defer mu.Unlock()
			out := make([][]byte, len(got))
			copy(out, got)
			return out
		},
	}
}

// sendAndSettle pushes the payloads and waits for batching to flush them,
// rather than guessing at a sleep long enough to be slow and short enough to
// be flaky.
func (d *duplexPair) sendAndSettle(t *testing.T, payloads [][]byte) {
	t.Helper()
	for _, p := range payloads {
		if err := d.sender.Send(p); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.linkA.count() > 0 || d.linkB.count() > 0 {
			// Give the linger window a moment to close on the last batch.
			time.Sleep(50 * time.Millisecond)
			if d.linkA.count() > 0 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nothing reached the links within two seconds")
}

func payloads(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf("payload number %d", i))
	}
	return out
}

func assertExactly(t *testing.T, got [][]byte, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("received %d payloads, want %d — a duplicate leaked through or a frame was lost", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("payload %d is %q, want %q", i, got[i], want[i])
		}
	}
}

// The core promise: both documents carry the frame, the far side sees it once.
func TestDuplicatedFrameIsDeliveredExactlyOnce(t *testing.T) {
	d := newDuplexPair(t, true)
	want := payloads(20)
	d.sendAndSettle(t, want)

	a, b := d.linkA.frames(), d.linkB.frames()
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("duplication did not happen: link A carried %d frames, link B %d", len(a), len(b))
	}
	if len(a) != len(b) {
		t.Fatalf("the copy is not whole: link A carried %d frames, link B %d", len(a), len(b))
	}

	// Both documents deliver, interleaved, as two live channels would.
	for i := range a {
		d.deliver(a[i])
		d.deliver(b[i])
	}
	assertExactly(t, d.received(), want)
}

// The case duplication exists for: one document dies and everything written to
// it is gone. Nothing below reports this — the write succeeded — so the only
// evidence is that the payload still arrives via the other document.
func TestDuplicationSurvivesOneDocumentSwallowingEverything(t *testing.T) {
	d := newDuplexPair(t, true)
	want := payloads(20)
	d.sendAndSettle(t, want)

	// Link A's frames are dropped on the floor, exactly as a closed document
	// drops them.
	for _, f := range d.linkB.frames() {
		d.deliver(f)
	}
	assertExactly(t, d.received(), want)
}

// Without duplication that same dead document costs the payloads outright.
// This is the measurement the feature is answering, kept as a test so the
// difference stays visible.
func TestWithoutDuplicationADeadDocumentLosesTheFrames(t *testing.T) {
	d := newDuplexPair(t, false)
	d.sendAndSettle(t, payloads(20))

	if d.linkB.count() != 0 {
		t.Fatalf("link B carried %d frames although duplication is off", d.linkB.count())
	}
	// Nothing was written to link B, so a peer attached only to that document
	// receives nothing at all.
	if got := d.received(); len(got) != 0 {
		t.Fatalf("received %d payloads from a document nothing was written to", len(got))
	}
}

// The copy may lag: its document can be slower or its queue backed up. The
// replay window has to be deep enough that a late duplicate is still
// recognised as one, or it would be delivered a second time.
func TestLateDuplicateIsStillDiscarded(t *testing.T) {
	d := newDuplexPair(t, true)
	want := payloads(20)
	d.sendAndSettle(t, want)

	a, b := d.linkA.frames(), d.linkB.frames()
	for _, f := range a {
		d.deliver(f)
	}
	// Only now does the second document catch up, every copy at once.
	for _, f := range b {
		d.deliver(f)
	}
	assertExactly(t, d.received(), want)
}
