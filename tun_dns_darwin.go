//go:build darwin

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"time"

	"openflux/network"
	"openflux/utils"
)

// DNS interception for the macOS utun client.
//
// The tunnel carries TCP only: a raw UDP datagram sent into it never comes
// back. That breaks name resolution whenever the resolver lives behind the
// exit node, because macOS (like every normal client) queries DNS over UDP.
//
// So we intercept IPv4/UDP packets with destination port 53 before they are
// forwarded, re-issue the very same DNS message to the very same resolver as
// DNS-over-TCP (RFC 7766, 2-byte length prefix) — that TCP flow does traverse
// the tunnel — and inject the answer back into utun as a normal UDP reply.
// The querying application sees a plain UDP DNS exchange and never knows.
//
// This mirrors tunnel/packettunnel.go's handleUDP, which does the same for the
// gVisor packet-tunnel path, rather than the iOS/Android clients, which resolve
// outside the tunnel and therefore cannot see a private zone behind the exit.

// dnsQueryTimeout bounds one upstream DNS-over-TCP exchange.
const dnsQueryTimeout = 8 * time.Second

// dnsSem caps concurrent upstream DNS resolutions so a burst of queries cannot
// spawn an unbounded number of tunnelled TCP connections.
var dnsSem = make(chan struct{}, 32)

// isDNSQuery reports whether pkt is an IPv4 UDP datagram addressed to port 53,
// and returns the IP header length for the caller.
func isDNSQuery(pkt []byte) (ihl int, ok bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 || pkt[9] != 17 { // IPv4 + UDP
		return 0, false
	}
	ihl = int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+8 {
		return 0, false
	}
	dstPort := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
	return ihl, dstPort == 53
}

// handleDNSQuery resolves one intercepted UDP DNS query over TCP through the
// tunnel and queues the UDP answer back to the device. Runs in its own
// goroutine; never blocks the tun read loop.
func (c *TUNClient) handleDNSQuery(pkt []byte, ihl int) {
	defer func() {
		if r := recover(); r != nil {
			utils.Debugf("[DNS] recovered: %v", r)
		}
	}()

	srcIP := append([]byte(nil), pkt[12:16]...)
	dstIP := append([]byte(nil), pkt[16:20]...)
	srcPort := append([]byte(nil), pkt[ihl:ihl+2]...)
	dstPort := append([]byte(nil), pkt[ihl+2:ihl+4]...)

	// The UDP length field bounds the payload; trust it over the slice length.
	udpLen := int(binary.BigEndian.Uint16(pkt[ihl+4 : ihl+6]))
	if udpLen < 8 || ihl+udpLen > len(pkt) {
		return
	}
	query := append([]byte(nil), pkt[ihl+8:ihl+udpLen]...)
	if len(query) == 0 {
		return
	}

	resolver := net.IP(dstIP).String()
	dest := net.JoinHostPort(resolver, "53")

	// Keep the resolver off the bypass list: it must stay reachable through
	// the tunnel, not via the physical gateway.
	c.protectIP(resolver)

	utils.Debugf("[DNS] intercepted UDP query for %s (%d bytes), re-issuing over TCP", dest, len(query))
	answer, err := dnsOverTCP(dest, query)
	if err != nil {
		utils.Debugf("[DNS] %s over TCP FAILED: %v", dest, err)
		return
	}
	utils.Debugf("[DNS] %s answered %d bytes over TCP", dest, len(answer))

	resp := buildDNSResponse(pkt[:ihl], srcIP, dstIP, srcPort, dstPort, answer)
	if resp == nil {
		return
	}
	select {
	case c.inbound <- resp:
	default:
		utils.Debugf("[DNS] inbound queue full, dropping answer")
	}
}

// dnsOverTCP performs one RFC 7766 length-prefixed DNS exchange with dest.
// The connection is an ordinary socket: the tunnel's default routes carry it
// to the exit node, which is exactly how it reaches a resolver behind it.
func dnsOverTCP(dest string, query []byte) ([]byte, error) {
	select {
	case dnsSem <- struct{}{}:
		defer func() { <-dnsSem }()
	default:
		return nil, fmt.Errorf("too many DNS queries in flight")
	}

	conn, err := net.DialTimeout("tcp", dest, dnsQueryTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(dnsQueryTimeout))

	msg := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(msg[:2], uint16(len(query)))
	copy(msg[2:], query)
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}

	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	answer := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(conn, answer); err != nil {
		return nil, err
	}
	return answer, nil
}

// buildDNSResponse wraps a DNS answer in a UDP/IPv4 packet addressed back to
// the original querier, mirroring the request's addresses and ports.
func buildDNSResponse(ipHdr, srcIP, dstIP, srcPort, dstPort, answer []byte) []byte {
	ihl := len(ipHdr)
	udpLen := 8 + len(answer)
	total := ihl + udpLen
	if total > 65535 {
		return nil
	}

	resp := make([]byte, total)
	copy(resp[:ihl], ipHdr)
	binary.BigEndian.PutUint16(resp[2:4], uint16(total))
	binary.BigEndian.PutUint16(resp[4:6], 0) // fresh ID, no fragmentation
	binary.BigEndian.PutUint16(resp[6:8], 0)
	resp[8] = 64 // TTL
	resp[9] = 17 // UDP
	copy(resp[12:16], dstIP)  // src = the resolver
	copy(resp[16:20], srcIP)  // dst = the querying host
	resp[10], resp[11] = 0, 0 // checksum recomputed below
	ck := network.IPChecksum(resp[:ihl])
	resp[10] = byte(ck >> 8)
	resp[11] = byte(ck & 0xFF)

	copy(resp[ihl:ihl+2], dstPort)   // src port = 53
	copy(resp[ihl+2:ihl+4], srcPort) // dst port = the querier's
	binary.BigEndian.PutUint16(resp[ihl+4:ihl+6], uint16(udpLen))
	resp[ihl+6], resp[ihl+7] = 0, 0 // zero UDP checksum is legal on IPv4
	copy(resp[ihl+8:], answer)
	return resp
}

// ---- bypass protection -------------------------------------------------

// protectIP marks an address as "must stay inside the tunnel", so the socket
// watcher never installs a /32 route for it via the physical gateway.
func (c *TUNClient) protectIP(ip string) {
	c.protectedMu.Lock()
	if c.protected == nil {
		c.protected = make(map[string]bool)
	}
	c.protected[ip] = true
	c.protectedMu.Unlock()
}

// IsProtected reports whether ip must not be bypassed around the tunnel.
func (c *TUNClient) IsProtected(ip string) bool {
	c.protectedMu.Lock()
	defer c.protectedMu.Unlock()
	return c.protected[ip]
}

// ---- system resolver ---------------------------------------------------

// SetSystemDNS points the primary network service at addr for the lifetime of
// the tunnel, remembering the previous setting for RestoreSystemDNS.
//
// Without this the OS keeps querying the local network's resolver over an
// on-link route, which never enters the tunnel and so can never be intercepted.
func (c *TUNClient) SetSystemDNS(addr string) error {
	// Protect it up front: even if we fail to switch the system resolver,
	// the address must not be routed around the tunnel.
	c.protectIP(addr)

	// Pin our own lookups to the resolvers in use right now, reached outside
	// the tunnel, before we hand the system a resolver that lives inside it.
	c.isolateProcessResolver(currentResolvers())

	service, err := primaryNetworkService(c.savedIface)
	if err != nil {
		return err
	}
	c.dnsService = service

	out, err := exec.Command("networksetup", "-getdnsservers", service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("getdnsservers %s: %w (%s)", service, err, strings.TrimSpace(string(out)))
	}
	prev := strings.Fields(strings.TrimSpace(string(out)))
	// "There aren't any DNS Servers set on <service>." means DHCP-provided.
	if len(prev) > 0 && net.ParseIP(prev[0]) != nil {
		c.savedDNS = prev
	} else {
		c.savedDNS = nil
	}

	args := append([]string{"-setdnsservers", service}, addr)
	if out, err := exec.Command("sudo", append([]string{"networksetup"}, args...)...).CombinedOutput(); err != nil {
		return fmt.Errorf("setdnsservers %s: %w (%s)", service, err, strings.TrimSpace(string(out)))
	}
	c.dnsSet = true
	c.protectIP(addr)
	utils.Debugf("[DNS] system resolver on %q set to %s (was %v)", service, addr, c.savedDNS)
	return nil
}

// RestoreSystemDNS puts the previous resolver configuration back. Best effort.
func (c *TUNClient) RestoreSystemDNS() {
	if !c.dnsSet || c.dnsService == "" {
		return
	}
	args := []string{"networksetup", "-setdnsservers", c.dnsService}
	if len(c.savedDNS) > 0 {
		args = append(args, c.savedDNS...)
	} else {
		args = append(args, "Empty") // back to DHCP-provided resolvers
	}
	if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
		utils.Debugf("[DNS] restore failed: %v (%s)", err, strings.TrimSpace(string(out)))
	} else {
		utils.Debugf("[DNS] system resolver on %q restored to %v", c.dnsService, c.savedDNS)
	}
	c.dnsSet = false
}

// primaryNetworkService maps a BSD device (en0) to the network service name
// ("Wi-Fi") that networksetup expects. Falls back to the first service that
// has an IPv4 address when the device cannot be matched.
func primaryNetworkService(device string) (string, error) {
	out, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return "", fmt.Errorf("listnetworkserviceorder: %w", err)
	}
	var current string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// "(1) Wi-Fi"
		if strings.HasPrefix(line, "(") && !strings.HasPrefix(line, "(Hardware Port:") {
			if idx := strings.Index(line, ") "); idx > 0 {
				current = strings.TrimSpace(line[idx+2:])
			}
			continue
		}
		// "(Hardware Port: Wi-Fi, Device: en0)"
		if strings.HasPrefix(line, "(Hardware Port:") && device != "" {
			if strings.HasSuffix(strings.TrimSuffix(line, ")"), "Device: "+device) && current != "" {
				return current, nil
			}
		}
	}
	return "", fmt.Errorf("no network service found for device %q", device)
}

// ---- keeping our own lookups off the tunnel ----------------------------

// currentResolvers reports the resolvers the system is using right now.
// Unlike `networksetup -getdnsservers` this also sees DHCP-provided servers,
// which is the usual case on Wi-Fi.
func currentResolvers() []string {
	out, err := exec.Command("scutil", "--dns").Output()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var servers []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "nameserver[") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		ip := strings.TrimSpace(line[idx+1:])
		if net.ParseIP(ip) == nil || seen[ip] {
			continue
		}
		seen[ip] = true
		servers = append(servers, ip)
	}
	return servers
}

// addBypassRoute pins one address to the physical gateway, so traffic to it
// leaves the machine directly instead of entering the tunnel. Recorded like
// every other bypass route, so it is cleaned up on exit.
func (c *TUNClient) addBypassRoute(ip string) {
	if c.gateway == "" {
		return
	}
	out, err := exec.Command("sudo", "route", "add", "-host", ip, "-gateway", c.gateway).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "File exists") {
		utils.Debugf("[DNS] bypass route %s failed: %v (%s)", ip, err, strings.TrimSpace(string(out)))
		return
	}
	recordBypassRoute(ip)
	utils.Debugf("[DNS] our own lookups bypass the tunnel via %s -> %s", ip, c.gateway)
}

// isolateProcessResolver makes THIS process resolve names through the given
// servers, reached outside the tunnel.
//
// Without it we deadlock: pointing the system at a resolver that only answers
// through the tunnel means the transport cannot resolve its own backend
// hostname when it (re)connects, so the tunnel never comes up, so the resolver
// stays unreachable. Our lookups must not depend on the thing they bring up.
func (c *TUNClient) isolateProcessResolver(servers []string) {
	if len(servers) == 0 {
		return
	}
	for _, s := range servers {
		c.addBypassRoute(s)
	}
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var lastErr error
			d := net.Dialer{Timeout: 5 * time.Second}
			for _, s := range servers {
				conn, err := d.DialContext(ctx, network, net.JoinHostPort(s, "53"))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	utils.Debugf("[DNS] process resolver pinned to %v (outside the tunnel)", servers)
}
