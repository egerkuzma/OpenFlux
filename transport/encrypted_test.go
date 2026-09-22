package transport

import (
	"bytes"
	"sync"
	"testing"
)

// testTransport is written by whatever goroutine the layer above happens to
// flush on — BatchedTransport has one of its own — and read by the test. The
// mutex is what makes that safe; without it the race detector fails the whole
// package, which is how CI found this.
type testTransport struct {
	mu       sync.Mutex
	receiver func([]byte)
	sent     []byte
}

func (t *testTransport) Start() error          { return nil }
func (t *testTransport) Stop() error           { return nil }
func (t *testTransport) IsConnected() bool     { return true }
func (t *testTransport) Stats() TransportStats { return TransportStats{} }
func (t *testTransport) Receive(callback func([]byte)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.receiver = callback
}
func (t *testTransport) Send(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = append([]byte(nil), data...)
	return nil
}

// lastSent returns a copy, so the caller cannot read the slice while the next
// send replaces it.
func (t *testTransport) lastSent() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.sent...)
}
func (t *testTransport) deliver(data []byte) {
	t.mu.Lock()
	r := t.receiver
	t.mu.Unlock()
	if r != nil {
		r(data)
	}
}

func TestEncryptedTransportRoundTrip(t *testing.T) {
	clientWire := &testTransport{}
	exitWire := &testTransport{}
	client, err := NewEncryptedTransport(clientWire, "a sufficiently long shared secret", "document", false)
	if err != nil {
		t.Fatal(err)
	}
	exitNode, err := NewEncryptedTransport(exitWire, "a sufficiently long shared secret", "document", true)
	if err != nil {
		t.Fatal(err)
	}

	want := []byte("private IPv4 packet")
	var got []byte
	exitNode.Receive(func(data []byte) { got = append([]byte(nil), data...) })
	if err := client.Send(want); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(clientWire.sent, want) {
		t.Fatal("ciphertext contains plaintext")
	}
	exitWire.deliver(clientWire.sent)
	if !bytes.Equal(got, want) {
		t.Fatalf("received %q, want %q", got, want)
	}

	reply := []byte("private response")
	got = nil
	client.Receive(func(data []byte) { got = append([]byte(nil), data...) })
	if err := exitNode.Send(reply); err != nil {
		t.Fatal(err)
	}
	clientWire.deliver(exitWire.sent)
	if !bytes.Equal(got, reply) {
		t.Fatalf("received %q, want %q", got, reply)
	}
}

func TestEncryptedTransportRejectsWrongKeyTamperingAndReplay(t *testing.T) {
	wire := &testTransport{}
	client, err := NewEncryptedTransport(wire, "first sufficiently long secret", "document", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Send([]byte("packet")); err != nil {
		t.Fatal(err)
	}

	wrongWire := &testTransport{}
	wrongExit, err := NewEncryptedTransport(wrongWire, "other sufficiently long secret", "document", true)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	wrongExit.Receive(func([]byte) { called++ })
	wrongWire.deliver(wire.sent)
	if called != 0 {
		t.Fatal("wrong key was accepted")
	}

	rightWire := &testTransport{}
	rightExit, err := NewEncryptedTransport(rightWire, "first sufficiently long secret", "document", true)
	if err != nil {
		t.Fatal(err)
	}
	rightExit.Receive(func([]byte) { called++ })
	tampered := append([]byte(nil), wire.sent...)
	tampered[len(tampered)-1] ^= 1
	rightWire.deliver(tampered)
	if called != 0 {
		t.Fatal("tampered packet was accepted")
	}
	rightWire.deliver(wire.sent)
	rightWire.deliver(wire.sent)
	if called != 1 {
		t.Fatalf("replayed packet delivered %d times, want 1", called)
	}
}

func TestEncryptedTransportRequiresStrongSecret(t *testing.T) {
	if _, err := NewEncryptedTransport(&testTransport{}, "too short", "document", false); err == nil {
		t.Fatal("short secret was accepted")
	}
}
