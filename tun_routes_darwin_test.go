//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// useTempBypassState points the state file at a throwaway path for one test.
func useTempBypassState(t *testing.T) {
	t.Helper()
	original := bypassStatePath
	bypassStatePath = filepath.Join(t.TempDir(), "bypass-routes")
	t.Cleanup(func() { bypassStatePath = original })
}

func TestBypassStateRecordAndForget(t *testing.T) {
	useTempBypassState(t)

	if got := recordedBypassRoutes(); len(got) != 0 {
		t.Fatalf("a fresh state must be empty, got %v", got)
	}

	recordBypassRoute("95.163.48.30")
	recordBypassRoute("95.163.59.187")
	recordBypassRoute("95.163.48.30") // duplicate

	got := recordedBypassRoutes()
	if len(got) != 2 {
		t.Fatalf("got %v, want two distinct addresses", got)
	}

	forgetBypassRoute("95.163.48.30")
	got = recordedBypassRoutes()
	if len(got) != 1 || got[0] != "95.163.59.187" {
		t.Fatalf("got %v, want only 95.163.59.187 left", got)
	}

	forgetBypassRoute("95.163.59.187")
	if got := recordedBypassRoutes(); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
	if _, err := os.Stat(bypassStatePath); !os.IsNotExist(err) {
		t.Error("the file must be removed once the last entry is gone")
	}
}

// Regression: the socket watcher used to clear the whole state file when it
// removed its own routes. Other owners — notably the pinned resolvers — record
// here too, and wiping the file stranded their routes: nothing deleted them,
// and they outlived the process pointing at a gateway that would soon be gone.
func TestForgettingOneRouteKeepsTheOthers(t *testing.T) {
	useTempBypassState(t)

	watcherRoutes := []string{"95.163.48.30", "95.163.59.187"}
	resolverRoute := "192.168.1.1" // recorded by the resolver pinning

	for _, ip := range watcherRoutes {
		recordBypassRoute(ip)
	}
	recordBypassRoute(resolverRoute)

	// The watcher cleans up only what it installed.
	for _, ip := range watcherRoutes {
		forgetBypassRoute(ip)
	}

	got := recordedBypassRoutes()
	if len(got) != 1 || got[0] != resolverRoute {
		t.Fatalf("got %v, want the resolver route %s still on record", got, resolverRoute)
	}
}

func TestClearRecordedBypassRoutes(t *testing.T) {
	useTempBypassState(t)

	recordBypassRoute("1.2.3.4")
	recordBypassRoute("5.6.7.8")
	clearRecordedBypassRoutes()

	if got := recordedBypassRoutes(); len(got) != 0 {
		t.Fatalf("got %v, want everything cleared", got)
	}
}

func TestBypassStateIgnoresBlankLines(t *testing.T) {
	useTempBypassState(t)

	if err := os.WriteFile(bypassStatePath, []byte("1.2.3.4\n\n  \n5.6.7.8\n1.2.3.4\n"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := recordedBypassRoutes()
	if len(got) != 2 || got[0] != "1.2.3.4" || got[1] != "5.6.7.8" {
		t.Fatalf("got %v, want the two distinct addresses in order", got)
	}
}

func TestBypassStateSurvivesMissingFile(t *testing.T) {
	useTempBypassState(t)

	// Nothing recorded yet: these must be no-ops rather than failures, because
	// they run on every start and every shutdown.
	forgetBypassRoute("1.2.3.4")
	clearRecordedBypassRoutes()
	if got := recordedBypassRoutes(); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}
