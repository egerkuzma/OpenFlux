package transport

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"openflux/utils"
)

// BondedTransport spreads traffic over several independent channels — in
// practice several cloud documents — and presents them as one.
//
// The point is continuity, and only continuity. A single document's WebSocket
// is closed by the provider now and then (a plain close, no error), and until
// it is back the tunnel carries nothing. With a bond, traffic moves to another
// link and the interruption never reaches the traffic above.
//
// Traffic is deliberately NOT spread across the links. Spreading was tried and
// measured, and it cost five to seven times the throughput: 7.5 Mbit/s on one
// document against 1.0-1.5 on five, reproduced by switching back and forth.
// The reason is that a frame handed to a link whose session has just died
// waits in that link's queue for about a second while the other links keep
// delivering, so the TCP streams inside the tunnel see reordering measured in
// seconds. TCP reads that as loss and keeps its window shut. Aggregate
// throughput across parallel streams fell just as far, so this is not merely a
// single-flow artefact.
//
// So one link carries everything until it fails, and the rest are standby.
// Reordering is then confined to the moment of a switch instead of being
// continuous.
//
// Both peers must be given the same set of documents. They do not have to
// agree on the order: a frame written to a document is read from that same
// document by whoever else is attached to it, so the links pair up by identity
// rather than by index.
type BondedTransport struct {
	links []Transport

	// current is the link carrying traffic; the others stand by.
	current atomic.Int64

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

// Send gives the frame to the active link, switching only when that link can
// no longer take it.
//
// Sticking to one link is the whole point: see the type comment for what
// spreading cost when it was measured. A switch reorders whatever was still
// queued on the old link, but that happens once per failure rather than once
// per frame.
func (b *BondedTransport) Send(data []byte) error {
	n := len(b.links)
	if n == 1 {
		return b.links[0].Send(data)
	}

	cur := int(b.current.Load())
	if cur < 0 || cur >= n {
		cur = 0
	}

	// The common path: the active link is up and takes the frame.
	if b.links[cur].IsConnected() {
		if err := b.links[cur].Send(data); err == nil {
			return nil
		}
	}

	// It is down or refused, so move to another link — preferring the one that
	// reconnected most recently. Sessions here are closed a fixed time after
	// they open, so the youngest has the longest left, and picking it roughly
	// halves how often traffic has to move. That matters because every move
	// reorders whatever was still queued on the old link, and a long-lived
	// connection through the tunnel feels each one.
	var lastErr error
	for _, idx := range b.switchOrder(cur) {
		if !b.links[idx].IsConnected() {
			continue
		}
		if err := b.links[idx].Send(data); err == nil {
			// Worth saying out loud: a switch reorders whatever was still
			// queued on the old link, and a long-lived connection through the
			// tunnel feels every one of them. How often this happens is the
			// number to watch when something upstream keeps dropping.
			if b.current.Swap(int64(idx)) != int64(idx) {
				utils.Infof("[bond] traffic moved to link %d of %d", idx+1, n)
			}
			return nil
		} else {
			lastErr = err
		}
	}

	// Nothing is up. Keep the frame on the active link rather than scattering
	// it: its queue survives the reconnect, and frames held together stay in
	// order, which is exactly what spreading them would destroy.
	if err := b.links[cur].Send(data); err == nil {
		return nil
	} else if lastErr == nil {
		lastErr = err
	}
	return fmt.Errorf("all %d links failed: %w", n, lastErr)
}

// switchOrder lists the candidate links, freshest first, falling back to
// round-robin order for links that cannot report their age.
func (b *BondedTransport) switchOrder(cur int) []int {
	n := len(b.links)
	order := make([]int, 0, n)
	for i := 1; i <= n; i++ {
		order = append(order, (cur+i)%n)
	}
	ages := make(map[int]time.Time, n)
	known := 0
	for _, idx := range order {
		if f, ok := b.links[idx].(Freshness); ok {
			if t := f.ConnectedSince(); !t.IsZero() {
				ages[idx] = t
				known++
			}
		}
	}
	if known == 0 {
		return order
	}
	sort.SliceStable(order, func(i, j int) bool {
		ti, oki := ages[order[i]]
		tj, okj := ages[order[j]]
		if oki != okj {
			return oki // links of known age come first
		}
		if !oki {
			return false
		}
		return ti.After(tj) // most recently connected first
	})
	return order
}

// ActiveLink reports which link currently carries traffic, for diagnostics.
func (b *BondedTransport) ActiveLink() int { return int(b.current.Load()) + 1 }

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
