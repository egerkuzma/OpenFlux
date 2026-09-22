//go:build darwin

package main

import (
	"net"
	"testing"
)

func TestParseMask(t *testing.T) {
	cases := []struct {
		in   string
		want string // dotted form of the resulting mask, "" when nil
	}{
		{"0xffffff00", "255.255.255.0"},
		{"0XFFFF0000", "255.255.0.0"},
		{"0xfffffffe", "255.255.255.254"},
		{"255.255.255.0", "255.255.255.0"},
		{"garbage", ""},
		{"0xzz", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := parseMask(c.in)
		if c.want == "" {
			if got != nil {
				t.Errorf("parseMask(%q) = %v, want nil", c.in, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("parseMask(%q) = nil, want %s", c.in, c.want)
			continue
		}
		if net.IP(got).String() != c.want {
			t.Errorf("parseMask(%q) = %v, want %s", c.in, net.IP(got), c.want)
		}
	}
}

func TestFirstUsableHost(t *testing.T) {
	cases := []struct {
		ip, mask, want string
	}{
		// The gateway is derived as network+1, which is where home routers sit.
		{"192.168.1.37", "255.255.255.0", "192.168.1.1"},
		{"10.4.5.6", "255.0.0.0", "10.0.0.1"},
		{"172.20.10.3", "255.255.255.240", "172.20.10.1"},
		// A /31 or /32 has no usable host, so there is no gateway to guess.
		{"10.0.0.1", "255.255.255.254", ""},
		{"10.0.0.1", "255.255.255.255", ""},
	}
	for _, c := range cases {
		got := firstUsableHost(net.ParseIP(c.ip), net.IPMask(net.ParseIP(c.mask).To4()))
		if got != c.want {
			t.Errorf("firstUsableHost(%s, %s) = %q, want %q", c.ip, c.mask, got, c.want)
		}
	}
}

func TestParseIfconfigIPv4(t *testing.T) {
	const out = `en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	options=400<CHANNEL_IO>
	ether ac:de:48:00:11:22
	inet6 fe80::1cbd:1ff:fe00:1122%en0 prefixlen 64 secured scopeid 0xe
	inet 192.168.1.37 netmask 0xffffff00 broadcast 192.168.1.255
	nd6 options=201<PERFORMNUD,DAD>
	media: autoselect
	status: active
`
	ip, mask := parseIfconfigIPv4(out)
	if ip == nil || ip.String() != "192.168.1.37" {
		t.Fatalf("ip = %v, want 192.168.1.37", ip)
	}
	if mask == nil || net.IP(mask).String() != "255.255.255.0" {
		t.Fatalf("mask = %v, want 255.255.255.0", mask)
	}

	t.Run("an interface with no IPv4 yields nothing", func(t *testing.T) {
		const down = `en3: flags=8963<UP,BROADCAST,SMART,RUNNING> mtu 1500
	ether ac:de:48:00:33:44
	inet6 fe80::1%en3 prefixlen 64 scopeid 0x5
	status: inactive
`
		if ip, mask := parseIfconfigIPv4(down); ip != nil || mask != nil {
			t.Errorf("got %v/%v, want nil/nil", ip, mask)
		}
	})
}

func TestProtectedAddresses(t *testing.T) {
	c := &TUNClient{}

	if c.IsProtected("192.0.2.53") {
		t.Error("nothing is protected before anything is registered")
	}

	// A protected address must keep going through the tunnel: the socket
	// watcher consults this before installing a bypass route, and pinning a
	// resolver that lives behind the exit node to the physical gateway would
	// make it unreachable.
	c.protectIP("192.0.2.53")
	if !c.IsProtected("192.0.2.53") {
		t.Error("the registered address must be protected")
	}
	if c.IsProtected("8.8.8.8") {
		t.Error("an unrelated address must not be protected")
	}
}
