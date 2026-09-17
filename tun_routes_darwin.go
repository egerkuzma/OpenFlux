//go:build darwin

package main

import (
	"os"
	"os/exec"
	"strings"
	"sync"

	"openflux/utils"
)

// Bookkeeping for the /32 "bypass" routes that keep traffic which must not
// enter the tunnel — the transport's own sockets, and the resolvers this
// process uses — pinned to the physical gateway.
//
// These routes are the one piece of state the kernel will not clean up for us.
// The default overrides (0.0.0.0/1, 128.0.0.0/1) are bound to the utun
// interface, so they disappear the moment the process dies and the interface
// goes with it. A bypass route instead points at the physical gateway and
// survives both a crash and a normal exit. Left behind, it silently blackholes
// whatever it pinned as soon as the machine joins a different network, where
// that gateway address no longer routes anywhere.
//
// So every route we install is also written to a state file, letting the next
// run delete whatever a crashed run left behind. Entries are removed one at a
// time as their routes go away: several owners write here (the socket watcher
// and the resolver pinning), and clearing the file wholesale would strand the
// routes belonging to the others.

// bypassStatePath is a variable rather than a constant so tests can point it
// at a temporary file.
var bypassStatePath = "/var/run/openflux-bypass-routes"

var bypassStateMu sync.Mutex

// readBypassStateLocked returns the recorded addresses. Caller holds the mutex.
func readBypassStateLocked() []string {
	data, err := os.ReadFile(bypassStatePath)
	if err != nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		ip := strings.TrimSpace(line)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	return out
}

// writeBypassStateLocked replaces the file with ips, removing it when empty.
// Caller holds the mutex.
func writeBypassStateLocked(ips []string) {
	if len(ips) == 0 {
		os.Remove(bypassStatePath)
		return
	}
	if err := os.WriteFile(bypassStatePath, []byte(strings.Join(ips, "\n")+"\n"), 0600); err != nil {
		utils.Debugf("[ROUTE] cannot write %s: %v", bypassStatePath, err)
	}
}

// recordBypassRoute remembers that we installed a bypass route for ip.
func recordBypassRoute(ip string) {
	bypassStateMu.Lock()
	defer bypassStateMu.Unlock()
	ips := readBypassStateLocked()
	for _, have := range ips {
		if have == ip {
			return
		}
	}
	writeBypassStateLocked(append(ips, ip))
}

// forgetBypassRoute drops one address from the record, leaving the rest — the
// routes other owners installed must survive our cleanup.
func forgetBypassRoute(ip string) {
	bypassStateMu.Lock()
	defer bypassStateMu.Unlock()
	ips := readBypassStateLocked()
	out := ips[:0]
	for _, have := range ips {
		if have != ip {
			out = append(out, have)
		}
	}
	writeBypassStateLocked(out)
}

// recordedBypassRoutes reports everything currently on record.
func recordedBypassRoutes() []string {
	bypassStateMu.Lock()
	defer bypassStateMu.Unlock()
	return readBypassStateLocked()
}

// deleteBypassRoute removes one /32 route. Best effort: the route may already
// be gone, which is not an error worth surfacing.
func deleteBypassRoute(ip string) {
	if out, err := exec.Command("sudo", "route", "delete", "-host", ip).CombinedOutput(); err != nil {
		utils.Debugf("[ROUTE] delete %s: %v (%s)", ip, err, strings.TrimSpace(string(out)))
	}
}

// clearRecordedBypassRoutes forgets the whole list.
func clearRecordedBypassRoutes() {
	bypassStateMu.Lock()
	defer bypassStateMu.Unlock()
	os.Remove(bypassStatePath)
}

// purgeRecordedBypassRoutes deletes every recorded route and drops the list.
// Called at startup so a crashed run cannot leave the machine with routes
// pointing at a gateway that no longer exists, and again at shutdown as a
// safety net for anything still on record.
func purgeRecordedBypassRoutes() {
	ips := recordedBypassRoutes()
	for _, ip := range ips {
		deleteBypassRoute(ip)
	}
	clearRecordedBypassRoutes()
	if len(ips) > 0 {
		utils.Debugf("[ROUTE] purged %d recorded bypass route(s)", len(ips))
	}
}

// purgeStaleHostRoutes removes bypass routes left over by an earlier run.
func (c *TUNClient) purgeStaleHostRoutes() { purgeRecordedBypassRoutes() }
