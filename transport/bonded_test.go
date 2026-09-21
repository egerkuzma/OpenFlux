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
	since     time.Time
}

func (f *fakeLink) ConnectedSince() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since
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

func (f *fakeLink) SetStartStagger(d time.Duration) {
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

// Traffic stays on one link. Spreading it was measured to cost five to seven
// times the throughput, because a frame queued on a link whose session just
// died waits a second while the others keep delivering, and the TCP streams
// inside the tunnel read that reordering as loss.
func TestBondedKeepsTrafficOnOneLink(t *testing.T) {
	b, links := bondOf(&fakeLink{connected: true}, &fakeLink{connected: true}, &fakeLink{connected: true})

	for i := 0; i < 30; i++ {
		if err := b.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	carrying := 0
	for i, l := range links {
		if l.count() > 0 {
			carrying++
			if l.count() != 30 {
				t.Errorf("link %d carried %d of 30 — traffic must not be split", i+1, l.count())
			}
		}
	}
	if carrying != 1 {
		t.Errorf("%d links carried traffic, want exactly one", carrying)
	}
}

// The reason the bond exists: a link dying must not interrupt anything, so
// traffic moves to one that is up.
func TestBondedFailsOverToALiveLink(t *testing.T) {
	b, links := bondOf(&fakeLink{connected: true}, &fakeLink{connected: true})

	for i := 0; i < 5; i++ {
		b.Send([]byte{byte(i)})
	}
	if links[0].count() != 5 {
		t.Fatalf("the first link carried %d of the first 5", links[0].count())
	}

	links[0].setConnected(false)

	for i := 0; i < 10; i++ {
		if err := b.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("send %d failed while a link was still up: %v", i, err)
		}
	}
	if links[0].count() != 5 {
		t.Errorf("the dead link took %d more frames", links[0].count()-5)
	}
	if links[1].count() != 10 {
		t.Errorf("the live link carried %d, want all 10", links[1].count())
	}
	if !b.IsConnected() {
		t.Error("the bond must report itself connected while any link is up")
	}
}

// Having failed over, traffic must stay put rather than drifting back and
// forth, which would reintroduce the reordering this design removes.
func TestBondedStaysOnTheNewLinkAfterFailover(t *testing.T) {
	b, links := bondOf(&fakeLink{connected: true}, &fakeLink{connected: true})

	b.Send([]byte("a"))
	links[0].setConnected(false)
	b.Send([]byte("b")) // fails over to link 2
	links[0].setConnected(true)

	for i := 0; i < 10; i++ {
		b.Send([]byte{byte(i)})
	}
	if links[0].count() != 1 {
		t.Errorf("traffic drifted back to the first link (%d frames)", links[0].count())
	}
	if links[1].count() != 11 {
		t.Errorf("the active link carried %d, want 11", links[1].count())
	}
}

// Nothing connected is a reconnect in progress, not a reason to throw traffic
// away or to scatter it: the link's queue survives the reconnect, and frames
// held together stay in order.
func TestBondedBuffersWhenNothingIsConnected(t *testing.T) {
	b, links := bondOf(&fakeLink{}, &fakeLink{})

	for i := 0; i < 5; i++ {
		if err := b.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("send must still be accepted while links reconnect: %v", err)
		}
	}
	if links[0].count() != 5 {
		t.Errorf("frames were scattered: link 1 holds %d, link 2 holds %d", links[0].count(), links[1].count())
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
	if b.ActiveLink() != 2 {
		t.Errorf("active link reported as %d, want 2", b.ActiveLink())
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

// Links opened in the same second have their sessions ended by the provider in
// the same second, so without an offset they also renew together, and one
// failed renewal takes several of them down at once. The offset is applied to
// the first connection, not to a reconnect: a bond expiring all at once is
// exactly when it must not be held back.
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
			t.Error("one link has to come up immediately, or the tunnel waits for nothing")
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

// Sessions are closed a fixed time after they open, so the link that
// reconnected most recently has the longest left. Switching to it instead of
// to the next one in rotation roughly halves how often traffic has to move —
// and every move reorders whatever was still queued on the link being left.
func TestBondedSwitchesToTheFreshestLink(t *testing.T) {
	now := time.Now()
	old := &fakeLink{connected: true, since: now.Add(-50 * time.Second)}
	fresh := &fakeLink{connected: true, since: now.Add(-2 * time.Second)}
	middle := &fakeLink{connected: true, since: now.Add(-30 * time.Second)}
	active := &fakeLink{connected: true, since: now.Add(-60 * time.Second)}

	// Order puts the oldest links first in rotation, so round-robin alone
	// would pick the wrong one.
	b, links := bondOf(active, old, middle, fresh)

	b.Send([]byte("first")) // settles on the active link
	links[0].setConnected(false)
	b.Send([]byte("after the active link died"))

	if links[3].count() != 1 {
		t.Errorf("the freshest link carried %d frames, want 1", links[3].count())
	}
	if links[1].count() != 0 {
		t.Errorf("traffic went to the oldest link instead (%d frames)", links[1].count())
	}
	if b.ActiveLink() != 4 {
		t.Errorf("active link is %d, want 4 (the freshest)", b.ActiveLink())
	}
}

func TestBondedFallsBackToRotationWithoutAges(t *testing.T) {
	// A link that cannot report its age must not be excluded, or a transport
	// without the optional interface would never be chosen.
	b, links := bondOf(&fakeLink{connected: true}, &fakeLink{connected: true})
	b.Send([]byte("a"))
	links[0].setConnected(false)
	if err := b.Send([]byte("b")); err != nil {
		t.Fatalf("switch failed: %v", err)
	}
	if links[1].count() != 1 {
		t.Errorf("the remaining link carried %d, want 1", links[1].count())
	}
}
