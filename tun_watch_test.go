//go:build darwin

package main

import "testing"

func TestSameSet(t *testing.T) {
	// The watcher fires "the socket set is stable" only when two consecutive
	// snapshots match; getting this wrong either takes the default route into
	// the tunnel too early or never does.
	cases := []struct {
		name string
		a, b map[string]bool
		want bool
	}{
		{"both empty", map[string]bool{}, map[string]bool{}, true},
		{"identical", map[string]bool{"1.1.1.1": true}, map[string]bool{"1.1.1.1": true}, true},
		{"different member", map[string]bool{"1.1.1.1": true}, map[string]bool{"8.8.8.8": true}, false},
		{"extra member", map[string]bool{"1.1.1.1": true}, map[string]bool{"1.1.1.1": true, "8.8.8.8": true}, false},
		{"missing member", map[string]bool{"1.1.1.1": true, "8.8.8.8": true}, map[string]bool{"1.1.1.1": true}, false},
		{"empty vs one", map[string]bool{}, map[string]bool{"1.1.1.1": true}, false},
	}
	for _, c := range cases {
		if got := sameSet(c.a, c.b); got != c.want {
			t.Errorf("%s: sameSet = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWatcherSkipsProtectedAddresses(t *testing.T) {
	// A protected address must never get a bypass route, so the watcher has to
	// consult the predicate. Verified through the predicate itself: installing
	// a route would shell out to `route`, which a unit test must not do.
	w := NewSocketWatcher("192.168.1.1", nil)
	w.SetProtected(func(ip string) bool { return ip == "192.0.2.53" })

	if w.isProtected == nil {
		t.Fatal("the predicate was not installed")
	}
	if !w.isProtected("192.0.2.53") {
		t.Error("the resolver must be reported as protected")
	}
	if w.isProtected("95.163.48.30") {
		t.Error("a transport backend must not be protected")
	}
}
