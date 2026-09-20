package transport

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// BondedTransport spreads traffic over several independent channels — in
// practice several cloud documents — and presents them as one.
//
// The point is continuity. A single document's WebSocket is closed by the
// provider now and then (a plain close, no error), and until it is back the
// tunnel carries nothing. With a bond, that link simply stops being chosen and
// the rest keep going; the interruption never reaches the traffic above.
// Throughput may add up too, but that depends on where the bottleneck is and
// is not the reason this exists.
//
// This works only because every layer above is per-message: the batching codec
// frames each message independently and the encryption uses a random nonce per
// packet rather than a sequence counter. Frames may therefore arrive in any
// order, which is exactly what several links with different latencies produce.
// TCP inside the tunnel copes with the reordering, as it does on any multipath
// network.
//
// Both peers must be given the same set of documents. They do not have to
// agree on the order: a frame written to a document is read from that same
// document by whoever else is attached to it, so the links pair up by identity
// rather than by index.
type BondedTransport struct {
	links []Transport

	next atomic.Uint64

	mu      sync.RWMutex
	started bool
}

// NewBondedTransport bonds the given links. Passing a single link is allowed
// and behaves as that link alone.
func NewBondedTransport(links []Transport) (*BondedTransport, error) {
	if len(links) == 0 {
		return nil, fmt.Errorf("a bonded transport needs at least one link")
	}
	return &BondedTransport{links: links}, nil
}

// StaggerLinks spreads the links' reconnects, delaying link i by i*spacing the
// first time it comes back.
//
// Links opened together stay in lockstep: the provider closes each session
// after a fixed lifetime, so sessions started in the same second also expire in
// the same second, and the bond repeatedly collapses to a couple of live links
// instead of losing one at a time. Offsetting each link once shifts its phase
// permanently, because every later cycle inherits the offset.
//
// The cost is paid once: on that first cycle the last link stays down for
// len(links)*spacing before returning. Spacing only has to exceed how long a
// reconnect takes — about a second — for the reconnects to stop overlapping,
// so it is kept small rather than spread across the whole lifetime.
func (b *BondedTransport) StaggerLinks(spacing time.Duration) {
	if spacing <= 0 || len(b.links) < 2 {
		return
	}
	for i, l := range b.links {
		if s, ok := l.(Staggerer); ok {
			s.SetReconnectStagger(time.Duration(i) * spacing)
		}
	}
}

// Len reports how many links the bond holds.
func (b *BondedTransport) Len() int { return len(b.links) }

// Start brings up every link. Links connect in the background, so this
// succeeding only means they were started; it fails when none could be.
func (b *BondedTransport) Start() error {
	var firstErr error
	started := 0
	for i, l := range b.links {
		if err := l.Start(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("link %d: %w", i+1, err)
			}
			continue
		}
		started++
	}
	if started == 0 {
		return fmt.Errorf("no link could be started: %w", firstErr)
	}
	b.mu.Lock()
	b.started = true
	b.mu.Unlock()
	return nil
}

func (b *BondedTransport) Stop() error {
	var firstErr error
	for _, l := range b.links {
		if err := l.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	b.mu.Lock()
	b.started = false
	b.mu.Unlock()
	return firstErr
}

// Send hands the frame to one link, preferring those currently connected and
// rotating between them so no single document carries everything.
//
// When nothing is connected the frame still goes to a link rather than being
// dropped: each link buffers into a queue that survives its own reconnect, and
// discarding traffic during a reconnect is what makes a sub-second outage look
// like a multi-second stall to the TCP streams above.
func (b *BondedTransport) Send(data []byte) error {
	if len(b.links) == 1 {
		return b.links[0].Send(data)
	}

	start := b.next.Add(1)
	n := uint64(len(b.links))

	// First pass: connected links only.
	var lastErr error
	for i := uint64(0); i < n; i++ {
		l := b.links[(start+i)%n]
		if !l.IsConnected() {
			continue
		}
		if err := l.Send(data); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}

	// Second pass: nothing is connected, or every connected link refused
	// (a full queue). Let some link buffer it.
	for i := uint64(0); i < n; i++ {
		l := b.links[(start+i)%n]
		if err := l.Send(data); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable link")
	}
	return fmt.Errorf("all %d links failed: %w", len(b.links), lastErr)
}

// Receive funnels every link into one callback. Frames are self-describing, so
// the consumer neither knows nor cares which document carried each one.
func (b *BondedTransport) Receive(callback func([]byte)) {
	for _, l := range b.links {
		l.Receive(callback)
	}
}

// IsConnected reports whether the bond can carry traffic, which it can as long
// as one link is up. This is the property that makes a single document's
// closure invisible.
func (b *BondedTransport) IsConnected() bool {
	for _, l := range b.links {
		if l.IsConnected() {
			return true
		}
	}
	return false
}

// ConnectedLinks counts the links currently up, for logging and diagnostics.
func (b *BondedTransport) ConnectedLinks() int {
	n := 0
	for _, l := range b.links {
		if l.IsConnected() {
			n++
		}
	}
	return n
}

// Stats sums the links. Uptime is the longest of them, since that is how long
// the bond as a whole has been usable.
func (b *BondedTransport) Stats() TransportStats {
	var out TransportStats
	for _, l := range b.links {
		s := l.Stats()
		out.BytesSent += s.BytesSent
		out.BytesReceived += s.BytesReceived
		out.PacketsSent += s.PacketsSent
		out.PacketsRecv += s.PacketsRecv
		out.Reconnects += s.Reconnects
		if s.Uptime > out.Uptime {
			out.Uptime = s.Uptime
		}
	}
	out.Connected = b.IsConnected()
	return out
}

// WaitForAnyLink blocks until at least one link is connected or the timeout
// expires, reporting whether one came up. Starting the tunnel before any
// document has attached would send the first frames into nothing.
func (b *BondedTransport) WaitForAnyLink(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b.IsConnected() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return b.IsConnected()
}
