//go:build darwin

package main

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"openflux/network"
)

// udpPacket builds an IPv4/UDP datagram for the tests: 20-byte header, the
// given ports and payload.
func udpPacket(srcIP, dstIP string, srcPort, dstPort uint16, payload []byte) []byte {
	total := 20 + 8 + len(payload)
	p := make([]byte, total)
	p[0] = 0x45 // IPv4, IHL 5
	binary.BigEndian.PutUint16(p[2:4], uint16(total))
	p[8] = 64
	p[9] = 17 // UDP
	copy(p[12:16], net.ParseIP(srcIP).To4())
	copy(p[16:20], net.ParseIP(dstIP).To4())
	binary.BigEndian.PutUint16(p[20:22], srcPort)
	binary.BigEndian.PutUint16(p[22:24], dstPort)
	binary.BigEndian.PutUint16(p[24:26], uint16(8+len(payload)))
	copy(p[28:], payload)
	return p
}

func TestIsDNSQuery(t *testing.T) {
	query := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}

	t.Run("udp port 53 is a query", func(t *testing.T) {
		ihl, ok := isDNSQuery(udpPacket("10.10.10.2", "192.168.1.35", 5300, 53, query))
		if !ok {
			t.Fatal("expected a DNS query")
		}
		if ihl != 20 {
			t.Fatalf("ihl = %d, want 20", ihl)
		}
	})

	t.Run("other udp ports are not", func(t *testing.T) {
		if _, ok := isDNSQuery(udpPacket("10.10.10.2", "1.1.1.1", 5300, 443, query)); ok {
			t.Fatal("port 443 must not be treated as DNS")
		}
	})

	t.Run("tcp is not", func(t *testing.T) {
		p := udpPacket("10.10.10.2", "192.168.1.35", 5300, 53, query)
		p[9] = 6 // TCP
		if _, ok := isDNSQuery(p); ok {
			t.Fatal("TCP must be forwarded, not intercepted")
		}
	})

	t.Run("short and malformed packets are rejected", func(t *testing.T) {
		cases := map[string][]byte{
			"empty":        {},
			"stub":         {0x45, 0, 0, 20},
			"ipv6":         append([]byte{0x60}, make([]byte, 40)...),
			"truncated":    udpPacket("10.10.10.2", "192.168.1.35", 5300, 53, query)[:22],
			"bogus header": {0x40, 0, 0, 40, 0, 0, 0, 0, 64, 17, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 0, 53, 0, 53, 0, 8, 0, 0},
		}
		for name, pkt := range cases {
			if _, ok := isDNSQuery(pkt); ok {
				t.Errorf("%s: must not be accepted as a DNS query", name)
			}
		}
	})
}

func TestBuildDNSResponse(t *testing.T) {
	const (
		client   = "10.10.10.2"
		resolver = "192.168.1.35"
		srcPort  = uint16(51234)
	)
	answer := []byte{0xAB, 0xCD, 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0, 0xde, 0xad}
	req := udpPacket(client, resolver, srcPort, 53, []byte{0xAB, 0xCD})

	resp := buildDNSResponse(req[:20], req[12:16], req[16:20], req[20:22], req[22:24], answer)
	if resp == nil {
		t.Fatal("no response built")
	}

	// Addresses and ports must be mirrored: the reply comes back from the
	// resolver to the querier, otherwise the OS drops it.
	if got := net.IP(resp[12:16]).String(); got != resolver {
		t.Errorf("source = %s, want the resolver %s", got, resolver)
	}
	if got := net.IP(resp[16:20]).String(); got != client {
		t.Errorf("destination = %s, want the querier %s", got, client)
	}
	if got := binary.BigEndian.Uint16(resp[20:22]); got != 53 {
		t.Errorf("source port = %d, want 53", got)
	}
	if got := binary.BigEndian.Uint16(resp[22:24]); got != srcPort {
		t.Errorf("destination port = %d, want %d", got, srcPort)
	}

	if got, want := binary.BigEndian.Uint16(resp[2:4]), uint16(len(resp)); got != want {
		t.Errorf("IP total length = %d, want %d", got, want)
	}
	if got, want := binary.BigEndian.Uint16(resp[24:26]), uint16(8+len(answer)); got != want {
		t.Errorf("UDP length = %d, want %d", got, want)
	}
	if resp[9] != 17 {
		t.Errorf("protocol = %d, want 17 (UDP)", resp[9])
	}
	// A header whose checksum does not verify to zero is dropped by the stack.
	if ck := network.IPChecksum(resp[:20]); ck != 0 {
		t.Errorf("IP checksum does not verify: %#04x", ck)
	}
	if got := resp[28:]; string(got) != string(answer) {
		t.Errorf("payload = %x, want %x", got, answer)
	}
}

func TestTruncateDNS(t *testing.T) {
	t.Run("small answers pass through untouched", func(t *testing.T) {
		in := make([]byte, 64)
		in[2] = 0x81
		out := truncateDNS(in)
		if len(out) != len(in) {
			t.Fatalf("length = %d, want %d", len(out), len(in))
		}
		if out[2]&0x02 != 0 {
			t.Error("TC must not be set on an answer that fits")
		}
	})

	t.Run("oversized answers are cut and flagged", func(t *testing.T) {
		// A DNS-over-TCP answer has no 512-byte limit, so this is ordinary.
		in := make([]byte, maxDNSPayload+500)
		in[2] = 0x81
		out := truncateDNS(in)
		if len(out) != maxDNSPayload {
			t.Fatalf("length = %d, want %d", len(out), maxDNSPayload)
		}
		if out[2]&0x02 == 0 {
			t.Error("TC must be set so the client retries over TCP")
		}
		// The whole point: what we inject must fit the interface MTU.
		if 20+8+len(out) > tunMTU {
			t.Errorf("datagram of %d bytes still exceeds MTU %d", 20+8+len(out), tunMTU)
		}
	})
}

// fakeDNSServer speaks the length-prefixed framing of RFC 7766 and answers
// every query with the canned reply.
func fakeDNSServer(t *testing.T, reply []byte) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var hdr [2]byte
				if _, err := io.ReadFull(c, hdr[:]); err != nil {
					return
				}
				body := make([]byte, binary.BigEndian.Uint16(hdr[:]))
				if _, err := io.ReadFull(c, body); err != nil {
					return
				}
				var out [2]byte
				binary.BigEndian.PutUint16(out[:], uint16(len(reply)))
				c.Write(append(out[:], reply...))
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestDNSOverTCP(t *testing.T) {
	t.Run("returns the answer the resolver sent", func(t *testing.T) {
		reply := []byte{0xAB, 0xCD, 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0, 1, 2, 3}
		addr, stop := fakeDNSServer(t, reply)
		defer stop()

		got, err := dnsOverTCP(addr, []byte{0xAB, 0xCD, 0x01, 0x00})
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if string(got) != string(reply) {
			t.Errorf("answer = %x, want %x", got, reply)
		}
	})

	t.Run("a refused connection is an error, not a hang", func(t *testing.T) {
		// Port 1 on loopback refuses immediately.
		done := make(chan error, 1)
		go func() {
			_, err := dnsOverTCP("127.0.0.1:1", []byte{0, 0})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Error("expected an error from a refused connection")
			}
		case <-time.After(dnsQueryTimeout + 2*time.Second):
			t.Error("dnsOverTCP hung instead of failing")
		}
	})

	t.Run("a server that hangs up mid-answer is an error", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Announce 100 bytes, send none, close.
			c.Write([]byte{0x00, 0x64})
			c.Close()
		}()
		if _, err := dnsOverTCP(ln.Addr().String(), []byte{0, 0}); err == nil {
			t.Error("expected an error on a truncated answer")
		}
	})
}

func TestParseResolvers(t *testing.T) {
	const out = `
DNS configuration

resolver #1
  search domain[0] : lan
  nameserver[0] : 192.168.1.1
  nameserver[1] : 8.8.8.8
  if_index : 14 (en0)

resolver #2
  nameserver[0] : 192.168.1.1
  nameserver[1] : not-an-ip
`
	got := parseResolvers(out)
	want := []string{"192.168.1.1", "8.8.8.8"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resolver %d = %s, want %s", i, got[i], want[i])
		}
	}
	if len(parseResolvers("")) != 0 {
		t.Error("empty input must yield no resolvers")
	}
}

func TestParseNetworkService(t *testing.T) {
	const listing = `An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) USB 10/100/1000 LAN
(Hardware Port: USB 10/100/1000 LAN, Device: en5)

(3) Thunderbolt Bridge
(Hardware Port: Thunderbolt Bridge, Device: bridge0)
`
	cases := map[string]string{
		"en0":     "Wi-Fi",
		"en5":     "USB 10/100/1000 LAN",
		"bridge0": "Thunderbolt Bridge",
		"utun4":   "", // another VPN's interface is not a service
		"":        "",
	}
	for device, want := range cases {
		if got := parseNetworkService(listing, device); got != want {
			t.Errorf("device %q -> %q, want %q", device, got, want)
		}
	}
}

func TestTrimmedNonEmpty(t *testing.T) {
	got := trimmedNonEmpty([]string{" 1.1.1.1 ", "", "   ", "8.8.8.8"})
	if len(got) != 2 || got[0] != "1.1.1.1" || got[1] != "8.8.8.8" {
		t.Errorf("got %q, want [1.1.1.1 8.8.8.8]", got)
	}
}

// useTempDNSState points the resolver state file at a throwaway path.
func useTempDNSState(t *testing.T) {
	t.Helper()
	original := dnsStatePath
	dnsStatePath = filepath.Join(t.TempDir(), "dns-state")
	t.Cleanup(func() { dnsStatePath = original })
}

func TestDNSStateRoundTrip(t *testing.T) {
	useTempDNSState(t)

	if _, ok := readDNSState(); ok {
		t.Fatal("nothing should be on record yet")
	}

	recordDNSState("Wi-Fi", []string{"192.168.1.1", "8.8.8.8"})
	st, ok := readDNSState()
	if !ok {
		t.Fatal("the record was not read back")
	}
	if st.service != "Wi-Fi" {
		t.Errorf("service = %q, want Wi-Fi", st.service)
	}
	if len(st.previous) != 2 || st.previous[0] != "192.168.1.1" || st.previous[1] != "8.8.8.8" {
		t.Errorf("previous = %q, want the two original servers", st.previous)
	}

	clearDNSState()
	if _, ok := readDNSState(); ok {
		t.Error("the record must be gone after clearing")
	}
}

// A resolver change outlives SIGKILL, unlike routes, which the kernel reclaims
// with the interface. Recording it is what lets the next start put DNS back
// instead of leaving the machine pointed at a resolver behind a dead tunnel.
func TestDNSStateWithNoPreviousServers(t *testing.T) {
	useTempDNSState(t)

	// Nothing was configured before: the service used DHCP-provided servers.
	recordDNSState("Wi-Fi", nil)
	st, ok := readDNSState()
	if !ok {
		t.Fatal("the record was not read back")
	}
	if len(st.previous) != 0 {
		t.Errorf("previous = %q, want none", st.previous)
	}

	args := restoreDNSArgs(st)
	want := []string{"networksetup", "-setdnsservers", "Wi-Fi", "Empty"}
	if len(args) != len(want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %q, want %q", args, want)
		}
	}
}

func TestRestoreDNSArgsWithServers(t *testing.T) {
	args := restoreDNSArgs(dnsState{service: "Ethernet", previous: []string{"1.1.1.1", "9.9.9.9"}})
	want := []string{"networksetup", "-setdnsservers", "Ethernet", "1.1.1.1", "9.9.9.9"}
	if len(args) != len(want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %q, want %q", args, want)
		}
	}
}

func TestReadDNSStateRejectsGarbage(t *testing.T) {
	useTempDNSState(t)

	// A truncated or empty file must not produce a command with a blank
	// service name, which networksetup would apply to the wrong thing.
	for _, body := range []string{"", "\n", "   \n\n"} {
		if err := os.WriteFile(dnsStatePath, []byte(body), 0600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if st, ok := readDNSState(); ok {
			t.Errorf("body %q accepted as %+v, want rejected", body, st)
		}
	}
}
