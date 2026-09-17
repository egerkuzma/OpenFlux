//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"openflux/utils"
)

// SocketWatcher watches the process's own outbound TCP connections and
// installs a /32 bypass route (via the physical gateway) for every distinct
// remote IPv4 it sees. This keeps the transport's sockets off the tunnel
// while everything else goes through utun.
type SocketWatcher struct {
	pid     int
	gateway string

	stop    chan struct{}
	stopped sync.WaitGroup

	mu       sync.Mutex
	known    map[string]bool
	lastSet  map[string]bool
	stableAt time.Time
	onStable func()
	fired    bool

	// isProtected reports addresses that must keep going through the tunnel
	// (e.g. a DNS resolver behind the exit node) and so must never get a
	// /32 route via the physical gateway.
	isProtected func(string) bool
}

// SetProtected installs the predicate guarding addresses from being bypassed.
func (w *SocketWatcher) SetProtected(fn func(string) bool) {
	w.mu.Lock()
	w.isProtected = fn
	w.mu.Unlock()
}

func NewSocketWatcher(gateway string, onStable func()) *SocketWatcher {
	return &SocketWatcher{
		pid:      os.Getpid(),
		gateway:  gateway,
		stop:     make(chan struct{}),
		known:    make(map[string]bool),
		lastSet:  make(map[string]bool),
		onStable: onStable,
	}
}

func (w *SocketWatcher) Start(interval time.Duration) {
	w.stopped.Add(1)
	go func() {
		defer w.stopped.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		w.snapshot()
		for {
			select {
			case <-w.stop:
				return
			case <-tick.C:
				w.snapshot()
			}
		}
	}()
}

func (w *SocketWatcher) Stop() {
	select {
	case <-w.stop:
		return
	default:
	}
	close(w.stop)
	w.stopped.Wait()
	w.removeRoutes()
}

// removeRoutes deletes every /32 bypass route this watcher installed.
//
// These are NOT cleaned up by the kernel: unlike the default overrides, which
// are bound to utun and vanish with it, a bypass route points at the physical
// gateway and outlives the process. Leaving them behind blackholes the
// transport's own backend as soon as the machine moves to another network,
// where that gateway no longer exists.
func (w *SocketWatcher) removeRoutes() {
	w.mu.Lock()
	ips := make([]string, 0, len(w.known))
	for ip := range w.known {
		ips = append(ips, ip)
	}
	w.known = make(map[string]bool)
	w.mu.Unlock()

	for _, ip := range ips {
		deleteBypassRoute(ip)
	}
	clearRecordedBypassRoutes()
	if len(ips) > 0 {
		utils.Debugf("[WATCH] removed %d bypass route(s)", len(ips))
	}
}

func (w *SocketWatcher) snapshot() {
	out, err := exec.Command("lsof", "-nP", "-i", "-a", "-p",
		fmt.Sprintf("%d", w.pid)).Output()
	if err != nil {
		return
	}

	current := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		idx := strings.Index(line, "->")
		if idx < 0 {
			continue
		}
		rest := line[idx+2:]
		if sp := strings.IndexAny(rest, " \t"); sp > 0 {
			rest = rest[:sp]
		}
		colon := strings.LastIndex(rest, ":")
		if colon < 0 {
			continue
		}
		host := rest[:colon]
		if !strings.Contains(host, ".") || strings.HasPrefix(host, "127.") {
			continue
		}
		current[host] = true
	}

	w.mu.Lock()
	for ip := range current {
		if w.known[ip] {
			continue
		}
		// Never bypass an address that has to stay inside the tunnel.
		if w.isProtected != nil && w.isProtected(ip) {
			utils.Debugf("[WATCH] %s is protected, keeping it in the tunnel", ip)
			continue
		}
		if err := w.addRoute(ip); err != nil {
			utils.Debugf("[WATCH] route %s failed: %v", ip, err)
			continue
		}
		w.known[ip] = true
		utils.Debugf("[WATCH] bypass route %s via %s", ip, w.gateway)
	}

	if sameSet(current, w.lastSet) {
		if w.stableAt.IsZero() {
			w.stableAt = time.Now()
		} else if time.Since(w.stableAt) > 3*time.Second && !w.fired && w.onStable != nil {
			w.fired = true
			w.mu.Unlock()
			w.onStable()
			return
		}
	} else {
		w.stableAt = time.Time{}
	}
	w.lastSet = current
	w.mu.Unlock()
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func (w *SocketWatcher) addRoute(ip string) error {
	out, err := exec.Command("sudo", "route", "add", "-host", ip,
		"-gateway", w.gateway).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "File exists") {
			// Ours from an earlier pass: still record it so it gets removed.
			recordBypassRoute(ip)
			return nil
		}
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	recordBypassRoute(ip)
	return nil
}
