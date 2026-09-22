package tunnel

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// fakeDialer answers DialTCP from a function, so a flow can be driven without
// a network and the address asked for can be checked.
type fakeDialer struct {
	mu     sync.Mutex
	asked  []string
	answer func(string) (net.Conn, error)
}

func (d *fakeDialer) DialTCP(address string) (net.Conn, error) {
	d.mu.Lock()
	d.asked = append(d.asked, address)
	d.mu.Unlock()
	if d.answer == nil {
		return nil, io.EOF
	}
	return d.answer(address)
}

func (d *fakeDialer) askedFor() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

// resolverPipe is a fake resolver on the far end of the dialer: it reads one
// length-prefixed query and writes back the canned answer, which is the framing
// of RFC 7766.
func resolverPipe(t *testing.T, answer []byte, mangle func(hdr []byte)) func(string) (net.Conn, error) {
	t.Helper()
	return func(string) (net.Conn, error) {
		ours, theirs := net.Pipe()
		go func() {
			defer theirs.Close()
			var lp [2]byte
			if _, err := io.ReadFull(theirs, lp[:]); err != nil {
				return
			}
			q := make([]byte, binary.BigEndian.Uint16(lp[:]))
			if _, err := io.ReadFull(theirs, q); err != nil {
				return
			}
			var out [2]byte
			binary.BigEndian.PutUint16(out[:], uint16(len(answer)))
			if mangle != nil {
				mangle(out[:])
			}
			theirs.Write(append(out[:], answer...))
		}()
		return ours, nil
	}
}

func TestDNSOverTCPFramesTheQueryAndUnwrapsTheAnswer(t *testing.T) {
	answer := []byte{0xAB, 0xCD, 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0, 7, 7, 7}
	d := &fakeDialer{answer: resolverPipe(t, answer, nil)}
	pt := NewPacketTunnel(d, 1500)
	defer pt.Close()

	got, err := pt.dnsOverTCP("10.0.0.53:53", []byte{0xAB, 0xCD, 0x01, 0x00})
	if err != nil {
		t.Fatalf("запрос не прошёл: %v", err)
	}
	if string(got) != string(answer) {
		t.Errorf("ответ %x, ожидался %x", got, answer)
	}
	if asked := d.askedFor(); len(asked) != 1 || asked[0] != "10.0.0.53:53" {
		t.Errorf("дозвон шёл к %v, ожидался 10.0.0.53:53", asked)
	}
}

// A resolver that announces more than it sends must be an error, not a hang:
// the tunnel is TCP-only and a stuck DNS read holds the flow open.
func TestDNSOverTCPRejectsATruncatedAnswer(t *testing.T) {
	d := &fakeDialer{answer: resolverPipe(t, []byte{0xAB, 0xCD}, func(hdr []byte) {
		binary.BigEndian.PutUint16(hdr, 100) // обещает 100 байт, пришлёт 2
	})}
	pt := NewPacketTunnel(d, 1500)
	defer pt.Close()

	done := make(chan error, 1)
	go func() {
		_, err := pt.dnsOverTCP("10.0.0.53:53", []byte{0xAB, 0xCD, 0x01, 0x00})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("обрезанный ответ принят за хороший")
		}
	case <-time.After(3 * time.Second):
		t.Error("dnsOverTCP завис вместо ошибки")
	}
}

func TestDNSOverTCPReportsADialFailure(t *testing.T) {
	d := &fakeDialer{} // answer == nil -> дозвон всегда падает
	pt := NewPacketTunnel(d, 1500)
	defer pt.Close()

	if _, err := pt.dnsOverTCP("10.0.0.53:53", []byte{1, 2, 3, 4}); err == nil {
		t.Error("несостоявшийся дозвон выдан за успех")
	}
}

// synTo builds a bare TCP SYN, the packet a device sends to open a connection.
func synTo(src, dst [4]byte, srcPort, dstPort uint16) []byte {
	p := make([]byte, 20+20)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	p[8] = 64
	p[9] = 6 // TCP
	copy(p[12:16], src[:])
	copy(p[16:20], dst[:])
	ip := header.IPv4(p)
	ip.SetChecksum(0)
	ip.SetChecksum(^ip.CalculateChecksum())

	tcpHdr := header.TCP(p[20:])
	tcpHdr.Encode(&header.TCPFields{
		SrcPort:    srcPort,
		DstPort:    dstPort,
		SeqNum:     1000,
		DataOffset: 20,
		Flags:      header.TCPFlagSyn,
		WindowSize: 65535,
	})
	tcpHdr.SetChecksum(^tcpHdr.CalculateChecksum(header.PseudoHeaderChecksum(
		6, tcpip.AddrFrom4(src), tcpip.AddrFrom4(dst), uint16(len(tcpHdr)))))
	return p
}

// The stack must answer a connection attempt from the device. This is the whole
// premise of the exit: a SYN goes in, a SYN-ACK comes back, and only then does
// anything get dialled. When a handshake fails here it looks exactly like a
// destination refusing the connection, which is how a day was once spent
// blaming a node's own routing.
func TestASynFromTheDeviceIsAnswered(t *testing.T) {
	d := &fakeDialer{answer: func(string) (net.Conn, error) {
		ours, theirs := net.Pipe()
		go func() { io.Copy(io.Discard, theirs); theirs.Close() }()
		return ours, nil
	}}
	pt := NewPacketTunnel(d, 1500)
	defer pt.Close()

	pt.WriteInbound(synTo([4]byte{10, 0, 0, 2}, [4]byte{93, 184, 216, 34}, 40000, 443))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := pt.ReadOutbound(ctx)
	if out == nil {
		t.Fatal("стек не ответил на SYN — рукопожатие не начинается")
	}

	ip := header.IPv4(out)
	if ip.Protocol() != 6 {
		t.Fatalf("в ответе протокол %d, ожидался TCP", ip.Protocol())
	}
	th := header.TCP(out[ip.HeaderLength():])
	if th.Flags()&header.TCPFlagSyn == 0 || th.Flags()&header.TCPFlagAck == 0 {
		t.Errorf("ответ с флагами %v, ожидался SYN+ACK", th.Flags())
	}
	if th.SourcePort() != 443 {
		t.Errorf("ответ с порта %d, ожидался 443", th.SourcePort())
	}
}

// UDP that is not DNS has nowhere to go: the transport carries TCP only, and
// pretending otherwise would open flows that silently never deliver.
func TestNonDNSUDPIsNotDialled(t *testing.T) {
	d := &fakeDialer{answer: func(string) (net.Conn, error) {
		t.Error("для не-DNS UDP был вызван дозвон")
		return nil, io.EOF
	}}
	pt := NewPacketTunnel(d, 1500)
	defer pt.Close()

	p := make([]byte, 20+8+4)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	p[8] = 64
	p[9] = 17 // UDP
	copy(p[12:16], []byte{10, 0, 0, 2})
	copy(p[16:20], []byte{8, 8, 8, 8})
	binary.BigEndian.PutUint16(p[20:22], 40000)
	binary.BigEndian.PutUint16(p[22:24], 443) // не 53
	binary.BigEndian.PutUint16(p[24:26], 12)
	pt.WriteInbound(p)

	time.Sleep(200 * time.Millisecond)
	if asked := d.askedFor(); len(asked) != 0 {
		t.Errorf("дозвон состоялся к %v", asked)
	}
}
