package tunnel

import (
	"sync"
	"testing"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// recordingDispatcher stands in for the network stack and keeps what it was
// handed.
type recordingDispatcher struct {
	mu   sync.Mutex
	seen [][]byte
}

func (d *recordingDispatcher) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, pkt.ToView().ToSlice())
}

func (d *recordingDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {
}

func (d *recordingDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// ipv4Packet builds a minimal well-formed IPv4 datagram so the tests exercise
// the same parsing the endpoint does on every packet.
func ipv4Packet(proto byte, payload []byte) []byte {
	p := make([]byte, 20+len(payload))
	p[0] = 0x45
	p[2] = byte((20 + len(payload)) >> 8)
	p[3] = byte(20 + len(payload))
	p[8] = 64
	p[9] = proto
	copy(p[12:16], []byte{10, 0, 0, 1})
	copy(p[16:20], []byte{10, 0, 0, 2})
	copy(p[20:], payload)
	return p
}

func TestInjectInboundReachesTheStack(t *testing.T) {
	e := NewTunnelLinkEndpoint()
	d := &recordingDispatcher{}
	e.Attach(d)
	if !e.IsAttached() {
		t.Fatal("endpoint не считает себя присоединённым")
	}

	pkt := ipv4Packet(6, []byte{1, 2, 3, 4})
	e.InjectInbound(pkt)

	if got := d.count(); got != 1 {
		t.Fatalf("стек получил %d пакетов, ожидался 1", got)
	}
	if got := string(d.seen[0]); got != string(pkt) {
		t.Errorf("пакет пришёл изменённым: %x вместо %x", d.seen[0], pkt)
	}
	if got := e.packetIn.Load(); got != 1 {
		t.Errorf("счётчик входящих %d, ожидался 1", got)
	}
}

// The endpoint hands the caller its own copy: the buffer a transport gives us
// is reused for the next frame, and the stack keeps packets past the call.
func TestInjectInboundDoesNotKeepTheCallersBuffer(t *testing.T) {
	e := NewTunnelLinkEndpoint()
	d := &recordingDispatcher{}
	e.Attach(d)

	pkt := ipv4Packet(6, []byte{9, 9, 9, 9})
	e.InjectInbound(pkt)
	for i := range pkt { // the caller reuses its buffer straight away
		pkt[i] = 0
	}

	if d.seen[0][0] == 0 {
		t.Error("стек получил ссылку на буфер вызывающего, а не копию")
	}
}

// A packet arriving before the stack has attached must not take the process
// down with it. The endpoint is created first and attached second, so the gap
// is real, and a tunnel that panics on a stray packet loses every other flow
// it was carrying.
func TestInjectInboundBeforeAttachIsSurvivable(t *testing.T) {
	e := NewTunnelLinkEndpoint()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("пакет до присоединения уронил процесс: %v", r)
		}
	}()
	e.InjectInbound(ipv4Packet(6, []byte{1, 2}))
}

func TestWritePacketsHandsEveryPacketOut(t *testing.T) {
	e := NewTunnelLinkEndpoint()
	var got [][]byte
	e.onOutgoingPacket = func(b []byte) { got = append(got, b) }

	var list stack.PacketBufferList
	for _, payload := range [][]byte{{1}, {2}, {3}} {
		list.PushBack(stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: bufferOf(ipv4Packet(6, payload)),
		}))
	}
	defer list.DecRef()

	n, err := e.WritePackets(list)
	if err != nil {
		t.Fatalf("запись вернула ошибку: %v", err)
	}
	if n != 3 {
		t.Errorf("записано %d пакетов, ожидалось 3", n)
	}
	if len(got) != 3 {
		t.Errorf("наружу ушло %d пакетов, ожидалось 3", len(got))
	}
	if e.packetOut.Load() != 3 {
		t.Errorf("счётчик исходящих %d, ожидался 3", e.packetOut.Load())
	}
}

// Without a consumer the packets are counted and dropped, not panicked over:
// the callback is installed after the endpoint is built.
func TestWritePacketsWithoutAConsumerIsSurvivable(t *testing.T) {
	e := NewTunnelLinkEndpoint()
	var list stack.PacketBufferList
	list.PushBack(stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: bufferOf(ipv4Packet(6, []byte{1})),
	}))
	defer list.DecRef()

	if _, err := e.WritePackets(list); err != nil {
		t.Fatalf("запись без получателя вернула ошибку: %v", err)
	}
	if e.packetOut.Load() != 1 {
		t.Errorf("пакет не посчитан: %d", e.packetOut.Load())
	}
}

func bufferOf(b []byte) buffer.Buffer { return buffer.MakeWithData(b) }
