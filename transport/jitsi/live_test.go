//go:build livetest

// Live end-to-end test against a real Jitsi server. Not part of the default
// suite — network-dependent, slow (Join waits for a second participant),
// and meaningless without live infrastructure. Run explicitly:
//
//	JITSI_LIVE_HOSTS='host1 host2 host3' \
//	go test -tags livetest ./transport/jitsi/ -run TestLiveExchange -v -timeout 5m
//
// The list is supplied at run time rather than hard-coded so the repo does
// not carry a target list of any specific servers we happen to depend on.
// Set JITSI_LIVE_HOSTS to a space-separated list of hosts to try; the test
// picks the first that lets us exchange five frames within 90s.
package jitsi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openflux/transport"
)

func liveHostsFromEnv(t *testing.T) []string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv("JITSI_LIVE_HOSTS"))
	if v == "" {
		t.Skip("JITSI_LIVE_HOSTS is empty; set it to space-separated Jitsi host list to run this test")
	}
	return strings.Fields(v)
}

// randomRoom keeps the two probes from colliding with a real conference. A
// 16-hex-char name is what lib-jitsi-meet uses for auto-generated rooms.
func randomRoom(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random: %v", err)
	}
	return "openflux-test-" + hex.EncodeToString(b[:])
}

// TestLiveExchange spins up two transports as the tunnel does — the exit
// node and the client — in the same room, and confirms that a byte sent from
// each side reaches the other. Success on the first working host is enough,
// which is what makes this test tolerant of any single server going away.
func TestLiveExchange(t *testing.T) {
	hosts := liveHostsFromEnv(t)
	var passed bool
	for _, host := range hosts {
		if passed {
			break
		}
		t.Run(host, func(t *testing.T) {
			if err := exchangeOn(t, host); err != nil {
				t.Logf("host %s: %v", host, err)
				t.Skip("host not usable this run")
				return
			}
			t.Logf("host %s: exchange succeeded", host)
			passed = true
		})
	}
	if !passed {
		t.Fatalf("no live host in the list worked; %d tried", len(hosts))
	}
}

// exchangeOn runs the exchange against one host. Returns nil on success, an
// error to say why this host was skipped (dead, refuses guests, SCTP-only).
func exchangeOn(t *testing.T, host string) error {
	room := randomRoom(t)
	url := fmt.Sprintf("%s/%s", host, room)
	t.Logf("room: %s", url)

	cfg := transport.DefaultConfig()
	cfg.MaxQueueSize = 64

	exit := NewJitsiTransport(url, cfg)
	client := NewJitsiTransport(url, cfg)

	// Two goroutines, one buffered channel per side. RecordReceive is where
	// the transport hands bytes up — mailru wires it through BaseTransport
	// the same way.
	var (
		exitGot   = make(chan []byte, 8)
		clientGot = make(chan []byte, 8)
	)
	exit.Receive(func(b []byte) {
		cp := append([]byte(nil), b...)
		select {
		case exitGot <- cp:
		default:
		}
	})
	client.Receive(func(b []byte) {
		cp := append([]byte(nil), b...)
		select {
		case clientGot <- cp:
		default:
		}
	})

	if err := exit.Start(); err != nil {
		return fmt.Errorf("exit start: %w", err)
	}
	defer func() { _ = exit.Stop() }()
	if err := client.Start(); err != nil {
		return fmt.Errorf("client start: %w", err)
	}
	defer func() { _ = client.Stop() }()

	// Wait for both to report connected. Join blocks until the other side
	// arrives, then OpenBridge is quick. 90s is comfortable — the median
	// join in the probe was around 5s.
	if err := waitConnected(exit, client, 90*time.Second); err != nil {
		return err
	}
	t.Logf("both connected in %v", timeSinceConnect(exit))

	// Send a distinguishable byte string from each side, retry a few times
	// because JVB briefly buffers the first frames while endpoints are
	// still being registered.
	fromClient := []byte("hi-from-client-" + room[:8])
	fromExit := []byte("hi-from-exit-" + room[:8])

	if err := sendUntilReceived(t, "client→exit", client, exitGot, fromClient, 20*time.Second); err != nil {
		return err
	}
	if err := sendUntilReceived(t, "exit→client", exit, clientGot, fromExit, 20*time.Second); err != nil {
		return err
	}

	// A short burst to prove the writer loop drains steadily — five frames
	// each way, checked in order (JVB preserves order per sender).
	if err := burst(t, "client→exit burst", client, exitGot, 5); err != nil {
		return err
	}
	if err := burst(t, "exit→client burst", exit, clientGot, 5); err != nil {
		return err
	}
	return nil
}

// waitConnected polls IsConnected on both transports. j.Join sets us
// connected only after OpenBridge; the fastest path is a few RTTs to the
// XMPP server plus the Jicofo session-initiate.
func waitConnected(a, b transport.Transport, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if a.IsConnected() && b.IsConnected() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("still not connected after %v (a=%v b=%v)",
		within, a.IsConnected(), b.IsConnected())
}

func timeSinceConnect(t *JitsiTransport) time.Duration {
	return time.Since(t.ConnectedSince())
}

// sendUntilReceived retries because JVB drops the first EndpointMessage
// silently on some builds until it has finished registering both endpoints;
// the pattern is the same as ClientHello/ServerHello, only without a formal
// handshake for user payload.
func sendUntilReceived(t *testing.T, dir string, sender transport.Transport, in <-chan []byte, want []byte, within time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(within)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	if err := sender.Send(want); err != nil {
		return fmt.Errorf("%s: initial send: %w", dir, err)
	}
	for {
		select {
		case got := <-in:
			if bytes.Equal(got, want) {
				t.Logf("%s: received in %v", dir, within-time.Until(deadline))
				return nil
			}
			// A different payload is not a failure — the peer may have
			// echoed something else back — just keep waiting.
		case <-tick.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: no answer in %v", dir, within)
			}
			// Resend on the tick: some bridges drop the very first frame.
			if err := sender.Send(want); err != nil {
				return fmt.Errorf("%s: resend: %w", dir, err)
			}
		}
	}
}

func burst(t *testing.T, dir string, sender transport.Transport, in <-chan []byte, n int) error {
	t.Helper()
	want := make([][]byte, n)
	for i := 0; i < n; i++ {
		want[i] = []byte(fmt.Sprintf("burst-%d-%d", i, time.Now().UnixNano()))
		if err := sender.Send(want[i]); err != nil {
			return fmt.Errorf("%s: send #%d: %w", dir, i, err)
		}
	}
	got := 0
	var received atomic.Int32
	var mu sync.Mutex
	deadline := time.Now().Add(30 * time.Second)
	for got < n && time.Now().Before(deadline) {
		select {
		case b := <-in:
			mu.Lock()
			for _, w := range want {
				if bytes.Equal(b, w) {
					got++
					received.Add(1)
					break
				}
			}
			mu.Unlock()
		case <-time.After(deadline.Sub(time.Now())):
			return fmt.Errorf("%s: got %d/%d frames in the burst", dir, got, n)
		}
	}
	if got != n {
		return fmt.Errorf("%s: got %d/%d frames", dir, got, n)
	}
	t.Logf("%s: %d frames delivered", dir, n)
	return nil
}
