package transport

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"openflux/utils"
)

// Defaults for the coalescing layer. Tunable at runtime via env vars so the
// batch size can be matched to the channel's per-message limits without a
// rebuild (OPENFLUX_BATCH_BYTES / OPENFLUX_BATCH_COUNT / OPENFLUX_BATCH_LINGER_MS).
const (
	defaultMaxBatchBytes = 8192
	defaultMaxBatchCount = 64
	defaultLingerMs      = 5
	batchQueueDepth      = 4096
)

// BatchedTransport replaces the old per-packet CompressedTransport. It queues
// outgoing tunnel packets, coalesces bursts into a single framed+zstd batch per
// inner transport message, and splits batches back into packets on receive.
//
// This is the symmetric layer: client and exit node must both use it (they do,
// because main.go wraps both the same way).
type BatchedTransport struct {
	Transport

	queue         chan []byte
	lingerMs      int
	maxBatchBytes int
	maxBatchCount int

	running atomic.Bool

	// Two things that used to happen in silence: a batch the channel below
	// refused, and a batch that arrived but could not be read.
	dropTally   utils.Tally
	decodeTally utils.Tally

	mu     sync.RWMutex
	userCb func([]byte)
}

// sendBatch hands a batch down and says so when it does not get there.
//
// The error was discarded at every call site. The transport below is the bond,
// which fails only when every link refused at once — the one event worth
// knowing about — so a whole batch, up to maxBatchCount packets, went missing
// with no log, no counter and nothing returned to any caller. A search for
// where frames were being lost concluded that nothing in our code dropped
// anything, having read a log that could not have said otherwise.
func (b *BatchedTransport) sendBatch(batch [][]byte) {
	err := b.Transport.Send(encodeBatch(batch))
	if err == nil {
		return
	}
	if n := b.dropTally.Note(); n > 0 {
		utils.Infof("[BATCH] dropped %d batch(es), nothing below would take them: %v", n, err)
	}
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func NewBatchedTransport(inner Transport) *BatchedTransport {
	return &BatchedTransport{
		Transport:     inner,
		queue:         make(chan []byte, batchQueueDepth),
		lingerMs:      envInt("OPENFLUX_BATCH_LINGER_MS", defaultLingerMs),
		maxBatchBytes: envInt("OPENFLUX_BATCH_BYTES", defaultMaxBatchBytes),
		maxBatchCount: envInt("OPENFLUX_BATCH_COUNT", defaultMaxBatchCount),
	}
}

func (b *BatchedTransport) Start() error {
	if err := b.Transport.Start(); err != nil {
		return err
	}
	b.running.Store(true)
	go b.flushLoop()
	return nil
}

func (b *BatchedTransport) Stop() error {
	b.running.Store(false)
	return b.Transport.Stop()
}

// Send copies the packet (the caller's buffer is reused by gVisor) and enqueues
// it for batching. A full queue drops the packet; the tunnel's TCP will
// retransmit, same as the old "write queue full" behavior.
func (b *BatchedTransport) Send(data []byte) error {
	p := make([]byte, len(data))
	copy(p, data)
	select {
	case b.queue <- p:
		return nil
	default:
		return fmt.Errorf("batch queue full")
	}
}

func (b *BatchedTransport) Receive(callback func([]byte)) {
	b.mu.Lock()
	b.userCb = callback
	b.mu.Unlock()

	b.Transport.Receive(func(data []byte) {
		pkts, err := decodeBatch(data)
		if err != nil {
			// Silent until now, and the one failure that looks exactly like
			// the channel going quiet: the frame arrived, was unreadable, and
			// was dropped without a word.
			if n := b.decodeTally.Note(); n > 0 {
				utils.Infof("[BATCH] %d batch(es) arrived unreadable (%d bytes): %v", n, len(data), err)
			}
			return
		}
		b.mu.RLock()
		cb := b.userCb
		b.mu.RUnlock()
		if cb == nil {
			return
		}
		for _, p := range pkts {
			cb(p)
		}
	})
}

func (b *BatchedTransport) flushLoop() {
	for b.running.Load() {
		first, ok := <-b.queue
		if !ok {
			return
		}
		batch := [][]byte{first}
		size := 2 + len(first)

		// Phase 1: absorb everything already queued (burst coalescing). This
		// alone collapses a window's worth of segments into one message.
	drainNow:
		for size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
			select {
			case p, ok := <-b.queue:
				if !ok {
					b.sendBatch(batch)
					return
				}
				batch = append(batch, p)
				size += 2 + len(p)
			default:
				break drainNow
			}
		}

		// Phase 2: brief linger to catch stragglers arriving just after the
		// burst. Negligible next to the channel RTT, but it fills batches
		// during steady bulk transfer.
		if b.lingerMs > 0 && size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
			timer := time.NewTimer(time.Duration(b.lingerMs) * time.Millisecond)
		linger:
			for size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
				select {
				case p, ok := <-b.queue:
					if !ok {
						timer.Stop()
						b.sendBatch(batch)
						return
					}
					batch = append(batch, p)
					size += 2 + len(p)
				case <-timer.C:
					break linger
				}
			}
			timer.Stop()
		}

		b.sendBatch(batch)
	}
}
