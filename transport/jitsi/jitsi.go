// Package jitsi implements a transport that tunnels packets through a Jitsi
// Meet conference. Two peers join the same MUC room, wait for the Videobridge
// to open its data channel (colibri-ws), and exchange base64-encoded bytes
// inside EndpointMessage frames — the same shape the browser uses for chat
// and hand-raising, only the payload is opaque.
//
// The XMPP, Jingle and colibri plumbing is delegated to
// github.com/zarazaex69/j. The traps that library papers over are worth
// keeping in mind, because breaking any of them puts the transport back into
// the "sessions close silently" state that took a session or two to unpick:
//
//   - j.Join, not j.JoinMUC. Join waits for Jicofo's session-initiate, which
//     Jicofo only sends after a second participant is in the room. JoinMUC
//     returns before that and Session.ColibriWS is empty.
//   - The wire frame must be a struct, not a map. Some JVB builds drop
//     msgPayload silently when it precedes "to" in the JSON — Go sorts map
//     keys, so a map produces exactly the wrong order.
//   - BridgeSendRaw puts the base64 payload at the top level as "raw"; some
//     bridges accept it, some do not. Sending msgPayload.raw is the shape
//     lib-jitsi-meet uses and every bridge accepts.
//   - The 16 KiB message ceiling is a JVB configuration default; going over
//     is silently dropped by the bridge.
package jitsi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	j "openflux/transport/jitsi/upstream"

	"openflux/transport"
	"openflux/utils"
)

// bridgeMaxMessageSize is the largest EndpointMessage the bridge accepts.
// JVB's default is 16 KiB and the payload is base64-encoded, so the raw
// bytes ceiling is 3/4 of that with a margin for the JSON envelope.
const (
	bridgeMaxMessageSize = 16 << 10
	envelopeOverhead     = 256
	maxPayloadBytes      = (bridgeMaxMessageSize-envelopeOverhead)*3/4 - 1
)

const (
	// joinTimeout bounds how long we wait for Jicofo's session-initiate,
	// which only arrives once a second participant is in the room. On an
	// idle conference this waits close to the ceiling.
	joinTimeout = 120 * time.Second
	// bridgeOpenTimeout is the colibri-ws dial itself, once we have a URL.
	bridgeOpenTimeout = 15 * time.Second
	// reconnectBase is doubled on each attempt up to reconnectMax.
	reconnectBase = 2 * time.Second
	reconnectMax  = 30 * time.Second
)

// endpointMessage is the wire frame — a struct, deliberately, because the
// field order matters. See the package doc for why.
type endpointMessage struct {
	ColibriClass string     `json:"colibriClass"`
	To           string     `json:"to"`
	MsgPayload   rawPayload `json:"msgPayload"`
}

type rawPayload struct {
	Raw string `json:"raw"`
}

// JitsiTransport carries traffic through one Jitsi room.
type JitsiTransport struct {
	*transport.BaseTransport

	host string
	room string
	nick string

	// session is the current j.Session; nil when disconnected. Reads under
	// Mu.RLock, writes under Mu.Lock.
	session *j.Session

	// writeQueue survives reconnects: a packet enqueued during a hiccup is
	// delivered by the next bridge, so a sub-second reconnect does not force
	// TCP inside the tunnel to retransmit.
	writeQueue chan []byte

	// labelMu guards label. Set once from outside, read every log line.
	labelMu sync.Mutex
	label   string

	// stagger holds the first-connection delay for bonded links; consumed
	// once and cleared.
	staggerMu sync.Mutex
	stagger   time.Duration

	// connectedAt is when the current room+bridge came up. A bond reads it
	// through Freshness to prefer the youngest link.
	connectedAtMu sync.Mutex
	connectedAt   time.Time

	// reconnecting admits one pending attempt at a time.
	reconnecting atomic.Bool

	// stop cancels the supervisor when Stop is called.
	stopCh chan struct{}
	stopMu sync.Mutex
}

// NewJitsiTransport accepts host+room in one string. Formats:
//
//	jitsi://meet.example.org/roomname
//	https://meet.example.org/roomname
//	meet.example.org/roomname
//
// Both peers must be given the same URL. The nickname is randomised: it is
// only ever seen by the bridge (nobody actually enters this conference), and
// its only job is to distinguish our own frames from the peer's on receive.
func NewJitsiTransport(url string, config transport.TransportConfig) *JitsiTransport {
	host, room := parseJitsiURL(url)
	t := &JitsiTransport{
		BaseTransport: transport.NewBaseTransport(config),
		host:          host,
		room:          room,
		nick:          randNick(),
		writeQueue:    make(chan []byte, config.MaxQueueSize),
	}
	return t
}

// parseJitsiURL is deliberately permissive: an operator pasting a Jitsi link
// out of a browser gets it right, and a bare host/room also works.
func parseJitsiURL(url string) (host, room string) {
	s := strings.TrimSpace(url)
	for _, prefix := range []string{"jitsi://", "https://", "http://"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	s = strings.TrimSuffix(s, "/")
	// The path portion of a Jitsi meet URL sometimes carries a #config bag
	// after the room name. Drop it.
	if i := strings.Index(s, "#"); i >= 0 {
		s = s[:i]
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// nickCounter is bumped once per randNick, so successive calls always differ
// even inside one nanosecond — which is the case whenever the platform's
// clock resolution is coarser than one call to fmt.Sprintf.
var nickCounter atomic.Uint32

func randNick() string {
	// Eight hex chars is what lib-jitsi-meet's default nick looks like. The
	// nick is only ever seen by the bridge, so uniqueness — not
	// unpredictability — is what matters here.
	n := uint64(time.Now().UnixNano())
	c := uint64(nickCounter.Add(1))
	x := n ^ (c * 0x9E3779B97F4A7C15)
	return fmt.Sprintf("of%08x", uint32(x)^uint32(x>>32))
}

// SetLabel implements transport.Labeler.
func (t *JitsiTransport) SetLabel(label string) {
	t.labelMu.Lock()
	t.label = label
	t.labelMu.Unlock()
}

// SetStartStagger implements transport.Staggerer.
func (t *JitsiTransport) SetStartStagger(d time.Duration) {
	t.staggerMu.Lock()
	t.stagger = d
	t.staggerMu.Unlock()
}

func (t *JitsiTransport) takeStagger() time.Duration {
	t.staggerMu.Lock()
	defer t.staggerMu.Unlock()
	d := t.stagger
	t.stagger = 0
	return d
}

// ConnectedSince implements transport.Freshness.
func (t *JitsiTransport) ConnectedSince() time.Time {
	t.connectedAtMu.Lock()
	defer t.connectedAtMu.Unlock()
	return t.connectedAt
}

func (t *JitsiTransport) markConnected() {
	t.connectedAtMu.Lock()
	t.connectedAt = time.Now()
	t.connectedAtMu.Unlock()
}

func (t *JitsiTransport) name() string {
	t.labelMu.Lock()
	defer t.labelMu.Unlock()
	if t.label == "" {
		return "jitsi"
	}
	return t.label
}

// Start begins connecting in the background. Join blocks until a second
// participant arrives, so a link that comes up alone waits — sometimes for
// minutes. That is expected: a bonded set of rooms is not stalled by any one
// room being idle.
func (t *JitsiTransport) Start() error {
	if t.host == "" || t.room == "" {
		return fmt.Errorf("jitsi: missing host or room in URL")
	}
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}
	t.stopMu.Lock()
	t.stopCh = make(chan struct{})
	t.stopMu.Unlock()

	if d := t.takeStagger(); d > 0 {
		utils.Infof("[%s] starting in %v to spread the links apart", t.name(), d)
		utils.SafeGo("jitsi.delayedStart", func() {
			select {
			case <-time.After(d):
			case <-t.stopSignal():
				return
			}
			if t.IsRunning() {
				t.superviseLoop()
			}
		})
	} else {
		utils.SafeGo("jitsi.supervise", t.superviseLoop)
	}
	return nil
}

func (t *JitsiTransport) stopSignal() <-chan struct{} {
	t.stopMu.Lock()
	ch := t.stopCh
	t.stopMu.Unlock()
	return ch
}

// Stop tears the transport down. Closing the current session flushes j's
// LeaveMUC handshake, which is what stops Jicofo and JVB from waiting minutes
// for us to time out (see the notes in j.Session.Close).
func (t *JitsiTransport) Stop() error {
	_ = t.BaseTransport.Stop()

	t.stopMu.Lock()
	if t.stopCh != nil {
		select {
		case <-t.stopCh:
		default:
			close(t.stopCh)
		}
	}
	t.stopMu.Unlock()

	t.Mu.Lock()
	sess := t.session
	t.session = nil
	t.Mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
	return nil
}

// Send is refuse-early: a queue full means the tunnel cannot carry more, and
// dropping here so TCP inside the tunnel notices and slows down is better
// than growing an unbounded buffer.
func (t *JitsiTransport) Send(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxPayloadBytes {
		// The bridge silently drops oversize messages, so refusing here is
		// the only way the caller learns.
		return fmt.Errorf("jitsi: payload %d bytes exceeds bridge limit %d", len(data), maxPayloadBytes)
	}
	select {
	case t.writeQueue <- data:
		t.RecordSend(len(data))
		return nil
	default:
		return fmt.Errorf("write queue full")
	}
}

// superviseLoop is the outer connect/reconnect loop.
func (t *JitsiTransport) superviseLoop() {
	attempt := 0
	for t.IsRunning() {
		if err := t.runOnce(); err != nil {
			utils.Infof("[%s] session ended: %v", t.name(), err)
		}
		if !t.IsRunning() {
			return
		}
		wait := reconnectBase << attempt
		if wait > reconnectMax {
			wait = reconnectMax
		}
		attempt++
		select {
		case <-time.After(wait):
		case <-t.stopSignal():
			return
		}
	}
}

// runOnce holds one session from join through the first fatal error.
func (t *JitsiTransport) runOnce() error {
	joinCtx, joinCancel := context.WithTimeout(context.Background(), joinTimeout)
	defer joinCancel()

	utils.Infof("[%s] joining %s/%s (waits for a second participant)", t.name(), t.host, t.room)
	sess, err := j.Join(joinCtx, j.Config{
		Host:  t.host,
		Room:  t.room,
		Nick:  t.nick,
		Debug: false,
	})
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	defer func() { _ = sess.Close() }()

	if sess.ColibriWS == "" {
		// SCTP-only bridge: needs a full pion PeerConnection to reach, which
		// is out of scope for the thin variant. Drop and let the supervisor
		// wait before retrying — the bridge assignment is unlikely to change
		// on its own.
		return fmt.Errorf("bridge offers SCTP only; this server is unusable without a PeerConnection")
	}

	brCtx, brCancel := context.WithTimeout(context.Background(), bridgeOpenTimeout)
	err = sess.OpenBridge(brCtx)
	brCancel()
	if err != nil {
		return fmt.Errorf("open bridge: %w", err)
	}

	t.Mu.Lock()
	t.session = sess
	t.SetConnected(true)
	t.Mu.Unlock()
	t.markConnected()
	utils.Infof("[%s] bridge open: %s", t.name(), sess.ColibriWS)

	// Both loops share this cancellation. Whichever returns first cancels
	// the other, so a dead read does not leave the writer looping against a
	// bridge that has already closed.
	loopCtx, loopCancel := context.WithCancel(context.Background())
	defer loopCancel()

	done := make(chan error, 2)
	go func() { done <- t.readerLoop(loopCtx, sess) }()
	go func() { done <- t.writerLoop(loopCtx, sess) }()

	var firstErr error
	select {
	case firstErr = <-done:
	case <-t.stopSignal():
		firstErr = fmt.Errorf("stopped")
	}
	loopCancel()
	// Drain the second goroutine.
	<-done

	t.Mu.Lock()
	if t.session == sess {
		t.session = nil
		t.SetConnected(false)
	}
	t.Mu.Unlock()
	return firstErr
}

// readerLoop delivers every EndpointMessage whose payload we planted, and
// drops the rest. Our own frames are echoed back by the bridge — filtering by
// From is the standard defence.
func (t *JitsiTransport) readerLoop(ctx context.Context, sess *j.Session) error {
	msgs := sess.BridgeMessages()
	if msgs == nil {
		return fmt.Errorf("bridge messages channel unavailable")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-msgs:
			if !ok {
				return fmt.Errorf("bridge closed")
			}
			payload := extractPayload(m)
			if payload == nil {
				continue
			}
			if fromResource(m.From) == t.nick {
				// Our own echo; ignore.
				continue
			}
			t.RecordReceive(len(payload))
			t.CallReceive(payload)
		}
	}
}

// writerLoop drains the write queue into the bridge. TrySendJSON is
// non-blocking: if j's own queue is full, we drop here rather than stalling
// every other packet. The queue survives a reconnect.
func (t *JitsiTransport) writerLoop(ctx context.Context, sess *j.Session) error {
	br := sess.Bridge()
	if br == nil {
		return fmt.Errorf("bridge not open")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case data := <-t.writeQueue:
			frame := endpointMessage{
				ColibriClass: "EndpointMessage",
				To:           "", // broadcast
				MsgPayload:   rawPayload{Raw: base64.StdEncoding.EncodeToString(data)},
			}
			if err := br.TrySendJSON(frame); err != nil {
				// A full bridge queue is a loud signal in a log; a closed
				// bridge is fatal for this session and drives the reconnect.
				utils.Debugf("[%s] bridge send: %v", t.name(), err)
				if strings.Contains(err.Error(), "closed") {
					return err
				}
			}
		}
	}
}

// extractPayload matches the payloadOf in the probe: primary path
// msgPayload.raw, fallback to top-level raw. Any bridge build's output
// reaches the caller.
func extractPayload(m j.BridgeMessage) []byte {
	if m.Class != "EndpointMessage" {
		return nil
	}
	enc := ""
	if p, ok := m.Fields["msgPayload"].(map[string]any); ok {
		enc, _ = p["raw"].(string)
	}
	if enc == "" {
		enc, _ = m.Fields["raw"].(string)
	}
	if enc == "" {
		return nil
	}
	out, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil
	}
	return out
}

// fromResource returns the resource part of a JID (the piece after '/'). JVB
// tags EndpointMessage.from with the sender's MUC-JID; the resource is the
// nickname we joined with, which is what our filter compares against.
func fromResource(from string) string {
	if i := strings.LastIndex(from, "/"); i >= 0 {
		return from[i+1:]
	}
	return from
}

// jsonForTest is a small helper the tests use to inspect the wire frame we
// build without depending on Go's map key ordering.
func jsonForTest(data []byte) ([]byte, error) {
	frame := endpointMessage{
		ColibriClass: "EndpointMessage",
		To:           "",
		MsgPayload:   rawPayload{Raw: base64.StdEncoding.EncodeToString(data)},
	}
	return json.Marshal(frame)
}
