package transport

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeLink stands in for one document's channel.
type fakeLink struct {
	mu        sync.Mutex
	connected bool
	sent      [][]byte
	failSend  bool
	startErr  error
	started   bool
	stopped   bool
	cb        func([]byte)
	stagger   time.Duration
}

func (f *fakeLink) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	return nil
}

func (f *fakeLink) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

func (f *fakeLink) Send(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSend {
		return fmt.Errorf("link refused")
	}
	f.sent = append(f.sent, append([]byte(nil), data...))
	return nil
}

func (f *fakeLink) Receive(cb func([]byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cb = cb
}

func (f *fakeLink) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeLink) Stats() TransportStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return TransportStats{
		BytesSent:   uint64(len(f.sent)),
		PacketsSent: uint64(len(f.sent)),
		Connected:   f.connected,
	}
}

func (f *fakeLink) SetReconnectStagger(d time.Duration) {
	f.mu.Lock()
	f.stagger = d
	f.mu.Unlock()
}

func (f *fakeLink) staggerOf() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stagger
}

func (f *fakeLink) setConnected(v bool) {
	f.mu.Lock()
	f.connected = v
	f.mu.Unlock()
}

func (f *fakeLink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeLink) deliver(data []byte) {
	f.mu.Lock()
	cb := f.cb
	f.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

func bondOf(links ...*fakeLink) (*BondedTransport, []*fakeLink) {
	ts := make([]Transport, len(links))
	for i, l := range links {
		ts[i] = l
	}
	b, err := NewBondedTransport(ts)
	if err != nil {
		panic(err)
	}
	return b, links
}

func TestBondedRequiresALink(t *testing.T) {
	if _, err := NewBondedTransport(nil); err == nil {
		t.Error("a bond with no links must be rejected")
	}
}

func TestBondedSpreadsAcrossLinks(t *testing.T) {
	b, links := bondOf(&fakeLink{connected: true}, &fakeLink{connected: true}, &fakeLink{connected: true})

	for i := 0; i < 30; i++ {
		if err := b.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	for i, l := range links {
		if l.count() == 0 {
			t.Errorf("link %d carried nothing; traffic must be spread", i+1)
		}
	}
	total := 0
	for _, l := range links {
		total += l.count()
	}
	if total != 30 {
		t.Errorf("links carried %d frames in total, want 30", total)
	}
}

// The reason the bond exists: one document being closed must not interrupt
// anything. Traffic has to keep flowing over the links that remain.
func TestBondedSurvivesALinkGoingDown(t *testing.T) {
	b, links := bondOf(&fakeLink{connected: true}, &fakeLink{connected: true})

	links[0].setConnected(false)

	for i := 0; i < 10; i++ {
		if err := b.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("send %d failed while a link was still up: %v", i, err)
		}
	}
	if links[0].count() != 0 {
		t.Errorf("the down link carried %d frames, want none", links[0].count())
	}
	if links[1].count() != 10 {
		t.Errorf("the live link carried %d frames, want all 10", links[1].count())
	}
	if !b.IsConnected() {
		t.Error("the bond must report itself connected while any link is up")
	}
}

// Nothing connected is a reconnect in progress, not a reason to throw traffic
// away: the links buffer, and dropping here is what turns a brief blip into a
// stall while TCP waits out a retransmission timeout.
func TestBondedBuffersWhenNothingIsConnected(t *testing.T) {
	b, links := bondOf(&fakeLink{}, &fakeLink{})

	if err := b.Send([]byte("held")); err != nil {
		t.Fatalf("send must still be accepted while links reconnect: %v", err)
	}
	if links[0].count()+links[1].count() != 1 {
		t.Error("the frame must have been handed to a link, not dropped")
	}
	if b.IsConnected() {
		t.Error("with every link down the bond must report itself down")
	}
}

func TestBondedFailsOnlyWhenEveryLinkRefuses(t *testing.T) {
	b, _ := bondOf(&fakeLink{connected: true, failSend: true}, &fakeLink{connected: true, failSend: true})

	if err := b.Send([]byte("x")); err == nil {
		t.Error("an error is expected once no link will take the frame")
	}
}

func TestBondedSkipsARefusingLink(t *testing.T) {
	b, links := bondOf(&fakeLink{connected: true, failSend: true}, &fakeLink{connected: true})

	for i := 0; i < 5; i++ {
		if err := b.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if links[1].count() != 5 {
		t.Errorf("the working link carried %d, want all 5", links[1].count())
	}
}

func TestBondedFunnelsReceives(t *testing.T) {
	b, links := bondOf(&fakeLink{connected: true}, &fakeLink{connected: true})

	var mu sync.Mutex
	var got [][]byte
	b.Receive(func(p []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), p...))
		mu.Unlock()
	})

	links[0].deliver([]byte("from one"))
	links[1].deliver([]byte("from two"))

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("received %d frames, want 2 — every link must feed the callback", len(got))
	}
}

func TestBondedStartAndStop(t *testing.T) {
	t.Run("one working link is enough to start", func(t *testing.T) {
		b, links := bondOf(&fakeLink{startErr: fmt.Errorf("nope")}, &fakeLink{})
		if err := b.Start(); err != nil {
			t.Fatalf("the bond must start when any link does: %v", err)
		}
		if !links[1].started {
			t.Error("the working link was not started")
		}
	})

	t.Run("no working link is a failure", func(t *testing.T) {
		b, _ := bondOf(&fakeLink{startErr: fmt.Errorf("nope")}, &fakeLink{startErr: fmt.Errorf("nope")})
		if err := b.Start(); err == nil {
			t.Error("the bond must fail when no link starts")
		}
	})

	t.Run("stop reaches every link", func(t *testing.T) {
		b, links := bondOf(&fakeLink{}, &fakeLink{})
		b.Stop()
		for i, l := range links {
			if !l.stopped {
				t.Errorf("link %d was not stopped", i+1)
			}
		}
	})
}

func TestBondedStatsAreSummed(t *testing.T) {
	b, links := bondOf(&fakeLink{connected: true}, &fakeLink{})
	links[0].Send([]byte("a"))
	links[0].Send([]byte("b"))
	links[1].Send([]byte("c"))

	s := b.Stats()
	if s.PacketsSent != 3 {
		t.Errorf("PacketsSent = %d, want 3", s.PacketsSent)
	}
	if !s.Connected {
		t.Error("Connected must reflect that one link is up")
	}
	if b.ConnectedLinks() != 1 {
		t.Errorf("ConnectedLinks = %d, want 1", b.ConnectedLinks())
	}
}

func TestBondedWaitForAnyLink(t *testing.T) {
	b, links := bondOf(&fakeLink{}, &fakeLink{})

	if b.WaitForAnyLink(200 * time.Millisecond) {
		t.Error("nothing is connected, so the wait must time out")
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		links[1].setConnected(true)
	}()
	if !b.WaitForAnyLink(3 * time.Second) {
		t.Error("the wait must return once a link comes up")
	}
}

func TestBondedSingleLinkIsPassThrough(t *testing.T) {
	b, links := bondOf(&fakeLink{connected: true})
	for i := 0; i < 4; i++ {
		b.Send([]byte{byte(i)})
	}
	if links[0].count() != 4 {
		t.Errorf("the only link carried %d, want 4", links[0].count())
	}
}

// Links opened in the same second have their sessions closed by the provider in
// the same second, so without an offset the bond keeps collapsing to a couple
// of live links at once instead of losing them one at a time.
func TestBondedStaggersLinks(t *testing.T) {
	b, links := bondOf(&fakeLink{}, &fakeLink{}, &fakeLink{}, &fakeLink{}, &fakeLink{})

	b.StaggerLinks(5 * time.Second)

	for i, l := range links {
		want := time.Duration(i) * 5 * time.Second
		if got := l.staggerOf(); got != want {
			t.Errorf("link %d got %v, want %v", i+1, got, want)
		}
	}

	t.Run("the first link is not delayed", func(t *testing.T) {
		if links[0].staggerOf() != 0 {
			t.Error("one link has to come back immediately, or the bond is needlessly down")
		}
	})

	t.Run("offsets are distinct", func(t *testing.T) {
		seen := map[time.Duration]bool{}
		for _, l := range links {
			d := l.staggerOf()
			if seen[d] {
				t.Fatalf("two links share the offset %v and will still expire together", d)
			}
			seen[d] = true
		}
	})
}

func TestBondedStaggerIgnoresPointlessCases(t *testing.T) {
	t.Run("a single link has nothing to spread apart", func(t *testing.T) {
		b, links := bondOf(&fakeLink{})
		b.StaggerLinks(5 * time.Second)
		if links[0].staggerOf() != 0 {
			t.Error("the only link must not be delayed")
		}
	})

	t.Run("zero spacing does nothing", func(t *testing.T) {
		b, links := bondOf(&fakeLink{}, &fakeLink{})
		b.StaggerLinks(0)
		for i, l := range links {
			if l.staggerOf() != 0 {
				t.Errorf("link %d was delayed despite zero spacing", i+1)
			}
		}
	})
}
