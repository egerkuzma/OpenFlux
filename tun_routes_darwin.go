//go:build darwin

package main

import (
	"os"
	"os/exec"
	"strings"
	"sync"

	"openflux/utils"
)

// Bookkeeping for the /32 "bypass" routes that keep the transport's own
// sockets outside the tunnel.
//
// These routes are the one piece of state the kernel will not clean up for us.
// The default overrides (0.0.0.0/1, 128.0.0.0/1) are bound to the utun
// interface, so they disappear the moment the process dies and the interface
// goes with it. A bypass route instead points at the physical gateway and
// survives both a crash and a normal exit. Left behind, it silently blackholes
// the transport's backend as soon as the machine joins a different network,
// where that gateway address no longer routes anywhere.
//
// So every route we install is also written to a small state file, letting the
// next run delete whatever a crashed run left behind.

const bypassStateFile = "/var/run/openflux-bypass-routes"

var bypassStateMu sync.Mutex

// recordBypassRoute appends ip to the on-disk list of installed bypass routes.
func recordBypassRoute(ip string) {
	bypassStateMu.Lock()
	defer bypassStateMu.Unlock()

	f, err := os.OpenFile(bypassStateFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		utils.Debugf("[ROUTE] cannot record bypass %s: %v", ip, err)
		return
	}
	defer f.Close()
	f.WriteString(ip + "\n")
}

// deleteBypassRoute removes one /32 route. Best effort: the route may already
// be gone, which is not an error worth surfacing.
func deleteBypassRoute(ip string) {
	if out, err := exec.Command("sudo", "route", "delete", "-host", ip).CombinedOutput(); err != nil {
		utils.Debugf("[ROUTE] delete %s: %v (%s)", ip, err, strings.TrimSpace(string(out)))
	}
}

// clearRecordedBypassRoutes forgets the on-disk list once its routes are gone.
func clearRecordedBypassRoutes() {
	bypassStateMu.Lock()
	defer bypassStateMu.Unlock()
	os.Remove(bypassStateFile)
}

// purgeRecordedBypassRoutes deletes every route a previous run recorded and
// then drops the list. Called at startup so a crashed run cannot leave the
// machine with routes pointing at a gateway that no longer exists.
func purgeRecordedBypassRoutes() {
	bypassStateMu.Lock()
	data, err := os.ReadFile(bypassStateFile)
	bypassStateMu.Unlock()
	if err != nil {
		return // no leftovers
	}

	seen := make(map[string]bool)
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		ip := strings.TrimSpace(line)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		deleteBypassRoute(ip)
		n++
	}
	clearRecordedBypassRoutes()
	if n > 0 {
		utils.Debugf("[ROUTE] purged %d stale bypass route(s) from a previous run", n)
	}
}

// purgeStaleHostRoutes removes bypass routes left over by an earlier run.
func (c *TUNClient) purgeStaleHostRoutes() { purgeRecordedBypassRoutes() }
