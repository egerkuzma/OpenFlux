package transport

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
)

// A frame that arrives twice must be delivered once.
//
// This is not a spare property. The mailru transport writes every frame to
// both the old and the new session for two seconds after it hands a session
// over, because the server does not always carry traffic on the replacement
// immediately — and the only thing that stops the far side seeing each of
// those frames twice is the replay window in the encryption layer. Duplicate
// segments into a TCP stream are not merely wasteful: three duplicate
// acknowledgements trigger a fast retransmit and halve the congestion window.
//
// The test runs the real stack rather than the window alone, because the
// property has to survive batching: a repeat is one batch containing many
// packets, and each packet inside it has to be recognised.
func TestARepeatedFrameIsDeliveredOnce(t *testing.T) {
	const secret = "a sufficiently long shared secret"

	wire := &testTransport{}
	sender, err := NewEncryptedTransport(NewBatchedTransport(wire), secret, "doc", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sender.Stop() })

	recvWire := &testTransport{}
	receiver, err := NewEncryptedTransport(NewBatchedTransport(recvWire), secret, "doc", true)
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

	want := make([][]byte, 12)
	for i := range want {
		want[i] = []byte(fmt.Sprintf("payload number %d", i))
		if err := sender.Send(want[i]); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// Wait for the batching layer to flush rather than guessing at a sleep.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(wire.sent) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)
	frame := append([]byte(nil), wire.sent...)
	if len(frame) == 0 {
		t.Fatal("ничего не дошло до провода за две секунды")
	}

	// Exactly what a session handover produces: the same frame on two
	// connections, arriving one after the other.
	recvWire.deliver(frame)
	recvWire.deliver(frame)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("доставлено %d кадров, ожидалось %d — повтор просочился наверх", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("кадр %d: %q, ожидался %q", i, got[i], want[i])
		}
	}
}
