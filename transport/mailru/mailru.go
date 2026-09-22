// Package mailru implements a transport that tunnels packets through
// Mail.ru's cloud document editor (docs.datacloudmail.ru), the same
// coauthoring backend family as Yandex.Docs. Two peers open the same
// public document and smuggle packets through the "cursor" field of the
// collaborative editing protocol.
package mailru

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
	"openflux/utils"
)

// wsReadTimeout bounds how long we wait for anything at all from the server.
//
// A dead TCP connection is often silent rather than reset — after a NAT
// timeout, a sleep, or a network hiccup there is no FIN and no RST — and a
// read without a deadline then blocks forever. No error means no reconnect,
// so the transport sits there looking connected while carrying nothing.
//
// Both peers send a keep-alive every KeepAliveInterval (10s) and each receives
// the other's, so a live channel is never quiet for long; a minute of silence
// means the connection is gone.
const wsReadTimeout = 60 * time.Second

const mailruUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"

var cursorPayloadRe = regexp.MustCompile(`"cursor":"[^;]+;([^"]+)"`)

type MailruDocsInfo struct {
	Token        string
	DocKey       string
	WsURL        string
	FileType     string
	DocURL       string
	DocTitle     string
	Permissions  map[string]interface{}
	CallbackURL  string
	EditorUserID string
}

// wsConn is the part of a websocket connection this transport actually uses.
// Naming it is what lets reconnect, handover and the watchdog be exercised
// without a provider on the other end — everything that used to be reachable
// only by running against the live service and reading the journal afterwards.
type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadDeadline(t time.Time) error
	Close() error
}

// dialWS opens one. Both places that connect — the first attempt and the
// renewal that replaces a session before the provider cuts it — went through
// the same dialler with the same timeouts and headers, so there is one of it.
type dialWS func(wsURL string) (wsConn, *http.Response, error)

func realDialWS(wsURL string) (wsConn, *http.Response, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		NetDialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	headers := http.Header{}
	headers.Set("User-Agent", mailruUserAgent)
	headers.Set("Origin", "https://docs.datacloudmail.ru")

	c, resp, err := dialer.Dial(wsURL, headers)
	if err != nil {
		// A typed nil in an interface is not nil, and every caller checks.
		return nil, resp, err
	}
	return c, resp, nil
}

type DocSession struct {
	Info       MailruDocsInfo
	Conn       wsConn
	WriteQueue chan []byte
	UserID     string
	writeMu    sync.Mutex

	// superseded marks a session that was replaced on purpose, so its read
	// loop can tell a planned handover from a failure and stay quiet.
	superseded atomic.Bool
}

func (s *DocSession) safeWrite(messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.Conn.WriteMessage(messageType, data)
}

type MailruDocsTransport struct {
	*transport.BaseTransport

	weblink string
	session *DocSession

	// dial is realDialWS in production; tests replace it.
	dial dialWS

	userCounter atomic.Int32
	baseUserID  string

	// label names this link inside a bond ("документ 2 из 5"), so a failure
	// says which document dropped. Empty when there is only one.
	labelMu sync.Mutex
	label   string

	// Session data is reusable: a second WebSocket authenticates with the same
	// token and document key, and the key belongs to the document rather than
	// the session (both verified against the live service). Caching it removes
	// an HTTP round trip from every reconnect — and with the provider closing
	// each session about once a minute, that was some six hundred API calls an
	// hour across two peers.
	infoMu     sync.Mutex
	cachedInfo *MailruDocsInfo
	infoExpiry time.Time

	// connectedAt is when the current session was established. A bond uses it
	// to pick the freshest link when it has to switch: sessions here live a
	// fixed time from connection, so the youngest one has the longest left.
	connectedAtMu sync.Mutex
	connectedAt   time.Time

	// stagger holds this link's first connection back once, shifting its phase
	// away from its siblings in a bond. Consumed on use: every later cycle
	// inherits the offset, so it never needs applying twice.
	staggerMu sync.Mutex
	stagger   time.Duration

	// retiring is the session just replaced by a handover, and retiringUntil
	// is how long frames are still copied to it. See writeOverlap.
	retiring      *DocSession
	retiringUntil time.Time

	// reconnecting admits one pending attempt at a time. Several paths can
	// notice a dead connection at once (the read loop, the keep-alive), and
	// without this they would each open a session to the same document.
	reconnecting atomic.Bool
}

// NewMailruDocsTransport accepts either a bare weblink ("AbCdEfGh1/IjKlMnOp2")
// or a full public URL ("https://cloud.mail.ru/public/AbCdEfGh1/IjKlMnOp2"),
// normalizing the latter to the former.
func NewMailruDocsTransport(weblink string, config transport.TransportConfig) *MailruDocsTransport {
	t := &MailruDocsTransport{
		BaseTransport: transport.NewBaseTransport(config),
		weblink:       normalizeWeblink(weblink),
		dial:          realDialWS,
	}
	t.baseUserID = randUserID()
	return t
}

// SetLabel implements transport.Labeler.
func (t *MailruDocsTransport) SetLabel(label string) {
	t.labelMu.Lock()
	t.label = label
	t.labelMu.Unlock()
}

// docInfo returns session data for the document, reusing the previous fetch
// while its token is still valid.
func (t *MailruDocsTransport) docInfo() (MailruDocsInfo, error) {
	t.infoMu.Lock()
	if t.cachedInfo != nil && time.Now().Before(t.infoExpiry) {
		info := *t.cachedInfo
		t.infoMu.Unlock()
		return info, nil
	}
	t.infoMu.Unlock()

	info, err := t.fetchDocInfo(t.weblink)
	if err != nil {
		return info, err
	}

	// Cache until shortly before the token itself expires. Falling back to a
	// short window when the expiry cannot be read keeps a malformed or changed
	// token from being reused indefinitely.
	expiry := time.Now().Add(5 * time.Minute)
	if exp, ok := jwtExpiry(info.Token); ok {
		if safe := exp.Add(-30 * time.Second); safe.After(time.Now()) {
			expiry = safe
		} else {
			expiry = time.Now()
		}
	}
	t.infoMu.Lock()
	t.cachedInfo = &info
	t.infoExpiry = expiry
	t.infoMu.Unlock()
	return info, nil
}

// forgetDocInfo drops the cache, so the next attempt fetches afresh. Called
// whenever a connection made with cached data fails early, which is what a
// no-longer-accepted token looks like from here.
func (t *MailruDocsTransport) forgetDocInfo() {
	t.infoMu.Lock()
	t.cachedInfo = nil
	t.infoMu.Unlock()
}

// jwtExpiry reads the exp claim out of a JWT without verifying the signature:
// we only need to know when to stop reusing the token, not whether to trust
// it — the server decides that.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// ConnectedSince implements transport.Freshness.
func (t *MailruDocsTransport) ConnectedSince() time.Time {
	t.connectedAtMu.Lock()
	defer t.connectedAtMu.Unlock()
	return t.connectedAt
}

func (t *MailruDocsTransport) markConnected() {
	t.connectedAtMu.Lock()
	t.connectedAt = time.Now()
	t.connectedAtMu.Unlock()
}

// SetStartStagger implements transport.Staggerer.
func (t *MailruDocsTransport) SetStartStagger(d time.Duration) {
	t.staggerMu.Lock()
	t.stagger = d
	t.staggerMu.Unlock()
}

// takeStagger returns the pending offset and clears it.
func (t *MailruDocsTransport) takeStagger() time.Duration {
	t.staggerMu.Lock()
	defer t.staggerMu.Unlock()
	d := t.stagger
	t.stagger = 0
	return d
}

// name returns the label for log lines, falling back to the transport name.
func (t *MailruDocsTransport) name() string {
	t.labelMu.Lock()
	defer t.labelMu.Unlock()
	if t.label == "" {
		return "mailru"
	}
	return t.label
}

func normalizeWeblink(weblink string) string {
	weblink = strings.TrimSpace(weblink)
	for _, prefix := range []string{
		"https://cloud.mail.ru/public/",
		"http://cloud.mail.ru/public/",
		"https://cloud.mail.ru/",
		"http://cloud.mail.ru/",
	} {
		if strings.HasPrefix(weblink, prefix) {
			return strings.Trim(strings.TrimPrefix(weblink, prefix), "/")
		}
	}
	return weblink
}

func (t *MailruDocsTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	t.baseUserID = randUserID()
	utils.SafeGo("mailru.keepAlive", t.keepAliveLoop)

	// A bond hands each link a different offset so their sessions do not all
	// start — and so do not all renew — in the same second. Paid here, before
	// anything is flowing, rather than on a reconnect.
	if d := t.takeStagger(); d > 0 {
		utils.Infof("[%s] starting in %v to spread the links apart", t.name(), d)
		utils.SafeGo("mailru.delayedStart", func() {
			time.Sleep(d)
			if t.IsRunning() {
				t.connectToDoc(0)
			}
		})
	} else {
		t.connectToDoc(0)
	}

	return nil
}

func (t *MailruDocsTransport) Send(data []byte) error {
	t.Mu.RLock()
	session := t.session
	t.Mu.RUnlock()

	if session == nil {
		return fmt.Errorf("no active session")
	}

	// Deliberately not gated on IsConnected: a reconnect takes well under a
	// second, the write queue survives it, and writerLoop holds each packet
	// until a new session exists. Refusing here instead threw those packets
	// away, and the TCP streams inside the tunnel then waited out a
	// retransmission timeout — turning a sub-second blip into a stall of
	// several seconds. The queue is bounded, so a genuinely long outage still
	// applies backpressure rather than buffering without limit.

	select {
	case session.WriteQueue <- data:
		t.RecordSend(len(data))
		return nil
	default:
		return fmt.Errorf("write queue full")
	}
}

func (t *MailruDocsTransport) connectToDoc(attempt int) {
	if !t.IsRunning() {
		return
	}

	utils.Debugf("[M-DOCS] connectToDoc attempt %d", attempt)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Debugf("[PANIC] recovered in mailru.connect: %v", r)
			}
		}()
		t.Mu.Lock()
		existingSession := t.session
		t.Mu.Unlock()

		var userID string
		if existingSession != nil {
			userID = existingSession.UserID
		} else {
			suffix := fmt.Sprintf("%03d", t.userCounter.Add(1)%1000)
			userID = t.baseUserID + suffix
		}

		info, err := t.fetchDocInfo(t.weblink)
		if err != nil {
			utils.Infof("[%s] cannot open the document: %v", t.name(), err)
			t.scheduleReconnect(attempt)
			return
		}

		utils.Debugf("[M-DOCS] WebSocket dial %s", info.WsURL)
		conn, resp, err := t.dial(info.WsURL)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			utils.Infof("[%s] connect failed (http %d): %v", t.name(), status, err)
			t.scheduleReconnect(attempt)
			return
		}
		t.markConnected()
		utils.Infof("[%s] connected", t.name())

		writeQueue := make(chan []byte, t.GetConfig().MaxQueueSize)
		if existingSession != nil {
			writeQueue = existingSession.WriteQueue
		}

		session := &DocSession{
			Info:       info,
			Conn:       conn,
			WriteQueue: writeQueue,
			UserID:     userID,
		}

		t.Mu.Lock()
		t.session = session
		t.SetConnected(true)
		t.Mu.Unlock()

		if existingSession == nil {
			utils.SafeGo("mailru.writer", t.writerLoop)
		}

		// Join the namespace only once the server has opened the session.
		//
		// Engine.IO has the client wait for the server's "0{sid...}" before
		// sending "40". We used to send it the moment the socket was up, and
		// most of the time the server tolerated it — but not always. Reading
		// back what arrived before each closure showed nine of fourteen
		// sessions where that open packet was the last thing that ever came:
		// no namespace acknowledgement, no auth reply, nothing, and then a
		// polite close. A join the server never saw is the obvious way to end
		// up in that state.
		//
		// This waits for one round trip, not for an auth confirmation —
		// Mail.ru can delay that by half a minute while it reconciles with
		// the other participant, which is why the code never waited for it
		// and still does not.
		if !t.awaitOpenPacket(session, conn) {
			// Carrying on is better than dropping the link: this is exactly
			// what the code did before, and it usually works.
			utils.Debugf("[M-DOCS] [%s] no open packet in %v, joining anyway", t.name(), openPacketTimeout)
		}

		t.authenticate(session, info, userID)
		t.scheduleRenewal(session)
		t.readLoop(session, attempt)
	}()
}

// authenticate joins the namespace and identifies us on it.
func (t *MailruDocsTransport) authenticate(session *DocSession, info MailruDocsInfo, userID string) {
	session.safeWrite(websocket.TextMessage, []byte(fmt.Sprintf(`40{"token":"%s"}`, info.Token)))
	session.safeWrite(websocket.TextMessage, []byte(authMessage(info, userID)))
}

// readLoop carries one session until its connection ends, and then arranges
// for a replacement — unless a replacement is already carrying the traffic.
func (t *MailruDocsTransport) readLoop(session *DocSession, attempt int) {
	conn := session.Conn
	connectedAt := time.Now()
	// What arrived just before a close is the only clue to why the provider
	// closed the session. Everything that is not a ping, an auth reply or a
	// cursor update is discarded without a word, and the close itself carries
	// an empty status — gorilla reports that as 1005, "no status received",
	// which means the server shut the session down deliberately and politely
	// rather than the connection breaking. If it says anything first, it says
	// it in a frame we were throwing away.
	var tail []recentFrame
	for t.IsRunning() {
		// Reset before every read: the deadline is absolute, not a per-call
		// idle timeout.
		conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		_, message, err := conn.ReadMessage()
		if err != nil {
			// A session we replaced on purpose is not a failure and must not
			// start a reconnect: its successor is already carrying traffic,
			// and reconnecting here would open a third session to the same
			// document.
			if session.superseded.Load() {
				utils.Debugf("[M-DOCS] [%s] old session closed after handover: %v", t.name(), err)
				conn.Close()
				return
			}
			// The reason a link dies is one line per event and the first thing
			// anyone asks, so it belongs in the ordinary log rather than
			// behind --debug.
			utils.Infof("[%s] connection lost: %v", t.name(), err)
			reportLastFrames(t.name(), tail)
			t.SetConnected(false)
			conn.Close()

			// A session that barely lasted suggests the data we opened it with
			// is no longer good; a long-lived one that ends is just the
			// provider recycling it, and the cache is still fine.
			next := attempt
			if time.Since(connectedAt) > 15*time.Second {
				next = -1
			} else {
				t.forgetDocInfo()
			}
			t.scheduleReconnect(next)
			return
		}
		tail = rememberFrame(tail, message)
		t.handleMessage(session, message)
	}
}

// sessionLifetime is how long Mail.ru lets a coauthoring session live before
// it sends a socket.io "41" and closes. Measured on both peers at once on
// 2026-09-20: 63 of 64 sessions ended between 60 and 68 seconds, median 61.
//
// renewAfter is when we replace the session ourselves, early enough that the
// margin covers the spread in that measurement.
const (
	sessionLifetime = 61 * time.Second
	renewAfter      = sessionLifetime - 11*time.Second

	// joinAckTimeout bounds the wait for the server to answer the namespace
	// join. It is not a wait for the auth result, which Mail.ru can delay by
	// half a minute; it is the acknowledgement that the join was seen at all.
	//
	// The server's answer is binary: it arrives at once or it never arrives.
	// Measured over fifteen minutes across both peers — 171 answers, every one
	// of them between 10 and 30 milliseconds, not a single one slower than half
	// a second, while roughly a third of joins went unanswered entirely.
	// Waiting three seconds bought nothing and cost everything it waited: a
	// renewal starts eleven seconds before the provider's cut, so three seconds
	// of dead time plus the five before a retry left room for exactly one more
	// attempt. At 300ms — ten times the slowest answer ever seen — two fit,
	// which takes the chance of missing the cut from about nine percent to
	// three.
	joinAckTimeout = 300 * time.Millisecond

	// renewRetry is how soon a renewal that could not be completed tries
	// again. Short enough that several attempts still fit before the
	// provider's cut, long enough not to hammer a service that just refused.
	renewRetry = 5 * time.Second

	// retireLinger is how long a replaced session keeps reading before its
	// connection is closed.
	//
	// Make-before-break protected what we send. It did nothing for what we
	// receive: closing the old connection the moment the swap happened threw
	// away whatever the server had already put into it and we had not read
	// yet, which at twenty megabits a second is a great many frames. Measured
	// before this existed — a transfer ran clean for six minutes, then stalled
	// twelve times in a row at ten seconds apiece, starting twenty seconds
	// before the nearest closure and right after two links renewed in the same
	// second.
	retireLinger = 3 * time.Second

	// writeOverlap is how long frames are written to the replaced session as
	// well as to its replacement.
	//
	// The handover swaps once the server acknowledges the namespace join. It
	// has not yet accepted our identity on the document by then — that
	// confirmation can take half a minute, far more than the eleven seconds
	// the old session has left — and frames written before it lands appear to
	// go nowhere. Measured: a twenty-minute transfer ran eleven minutes clean
	// and then stalled for forty-five seconds without a single byte arriving,
	// beginning in the same second the sending peer renewed two of its links.
	// The two short stalls earlier in the same run each began two seconds
	// after a renewal on the receiving peer.
	//
	// Waiting for the confirmation is not available, so both sessions are
	// written to for a moment instead and whichever the server honours
	// delivers. The copy costs nothing because the encryption layer's replay
	// window discards a frame whose nonce it has already seen — without that
	// window this overlap would deliver every frame twice, so it must not be
	// used on an unencrypted channel. It is the
	// mirror of retireLinger — that one keeps reading the old session, this
	// one keeps writing to it — and is kept shorter so it never writes to a
	// connection that has already been closed.
	writeOverlap = 2 * time.Second
)

// scheduleRenewal arranges to replace this session before the provider kills
// it.
//
// The churn is a schedule, not a fault: every session is cut at about 61
// seconds. Reacting to the cut means the traffic in flight is lost and the
// streams inside the tunnel wait out a retransmission timer measured in
// seconds. Nothing can talk the provider out of the schedule — but a session
// that is replaced before it is cut never loses anything, because the old one
// carries traffic until the new one is ready. Break-before-make becomes
// make-before-break.
func (t *MailruDocsTransport) scheduleRenewal(session *DocSession) {
	t.scheduleRenewalIn(session, renewAfter)
}

// scheduleRenewalIn is scheduleRenewal with an explicit delay, used to try
// again soon after a renewal could not be completed.
//
// Rescheduling is not optional. The timer is one-shot, so a renewal that gives
// up without leaving a successor behind ends renewal for that link for good:
// it then survives only until the provider's next cut and falls back to
// reconnecting. That is exactly what happened — links dropped out of the cycle
// one at a time, each after a single transient failure, and the bond carried
// on with the ones still renewing while the rest quietly stopped.
func (t *MailruDocsTransport) scheduleRenewalIn(session *DocSession, delay time.Duration) {
	utils.SafeGo("mailru.renew", func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
		if !t.IsRunning() || session.superseded.Load() {
			return
		}
		// Only the session still carrying traffic may renew itself. One that
		// was already replaced, or died and was reconnected, has a successor
		// whose own timer is running.
		t.Mu.RLock()
		current := t.session
		t.Mu.RUnlock()
		if current != session {
			return
		}
		t.renewSession(session)
	})
}

// Renewal failures are reported in the ordinary log rather than behind --debug.
//
// They are rare — the whole point is that renewal normally succeeds — and they
// are the only warning that a link is drifting towards being aged out. Hidden
// behind a flag, the first evidence anyone gets is the watchdog line, by which
// time the session is eighty seconds old, the link has been useless for most of
// that, and the reason is gone. Measured: three watchdog firings in ten minutes
// with no way to tell what the renewals had been failing on.

// renewSession opens a replacement alongside the live one and swaps them over.
//
// The old session keeps carrying traffic throughout: it is only retired once
// the replacement has joined the namespace and been identified. If anything
// goes wrong the old session is left exactly as it was, which is no worse than
// not trying — the provider will cut it at its appointed second and the
// ordinary reconnect path takes over.
func (t *MailruDocsTransport) renewSession(old *DocSession) {
	info, err := t.docInfo()
	if err != nil {
		utils.Infof("[%s] renewal deferred, document info: %v", t.name(), err)
		t.scheduleRenewalIn(old, renewRetry)
		return
	}

	conn, _, err := t.dial(info.WsURL)
	if err != nil {
		utils.Infof("[%s] renewal deferred, dial: %v", t.name(), err)
		t.scheduleRenewalIn(old, renewRetry)
		return
	}

	// The replacement shares the write queue, so whatever is waiting in it
	// crosses the handover untouched, and keeps the same identity, so the
	// provider sees the same participant reconnecting rather than a second one
	// joining.
	fresh := &DocSession{
		Info:       info,
		Conn:       conn,
		WriteQueue: old.WriteQueue,
		UserID:     old.UserID,
	}

	if !t.awaitOpenPacket(fresh, conn) {
		utils.Infof("[%s] renewal deferred, the server sent no handshake", t.name())
		conn.Close()
		t.scheduleRenewalIn(old, renewRetry)
		return
	}
	t.authenticate(fresh, info, fresh.UserID)
	if !t.awaitJoinAck(fresh, conn) {
		// Swapping onto a connection the server has not acknowledged would
		// hand traffic to a session that may never carry it. The old one is
		// still good for another ten seconds.
		utils.Infof("[%s] renewal deferred, the join went unacknowledged", t.name())
		conn.Close()
		t.scheduleRenewalIn(old, renewRetry)
		return
	}

	// The swap, and the one place this can race: while the replacement was
	// being prepared the old session may have been cut anyway, and its read
	// loop may already have started a reconnect. Installing the replacement on
	// top of that would leave two live sessions on one document. So the swap
	// only happens if the old session is still the current one and no
	// reconnect is in flight; otherwise the replacement is thrown away, which
	// costs nothing — the other path is bringing up a session of its own.
	t.Mu.Lock()
	if t.session != old || t.reconnecting.Load() {
		t.Mu.Unlock()
		// No retry here, and only here: the link belongs to another session
		// now, and that one brought its own timer with it.
		utils.Infof("[%s] renewal dropped, a reconnect got there first", t.name())
		conn.Close()
		return
	}
	t.session = fresh
	t.retiring = old
	t.retiringUntil = time.Now().Add(writeOverlap)
	// Stated rather than assumed: the flag is already true, but the keep-alive
	// can turn it false if its write lands on the connection we are about to
	// retire, and nothing on this path would ever turn it back on.
	t.SetConnected(true)
	t.Mu.Unlock()
	// Read the age before the mark is reset, or the line reports zero.
	age := time.Since(t.ConnectedSince())
	t.markConnected()
	old.superseded.Store(true)
	// Retire the old connection, but not this instant. Its read loop is still
	// the one delivering whatever the server has already sent into it, and
	// closing it here threw all of that away — the half of the handover that
	// make-before-break did not cover. Nothing writes to it any more, so
	// letting it read for a few more seconds costs nothing; frames that also
	// arrive on the new session are dropped as duplicates by the replay window
	// above.
	utils.SafeGo("mailru.retire", func() {
		time.Sleep(retireLinger)
		old.Conn.Close()
	})

	// Said out loud, for the same reason a closure is: these two lines are how
	// anyone tells a link that is being recycled cleanly from one that keeps
	// dying. Without it the only evidence a handover happened is the absence
	// of a closure, which is not evidence anyone can act on.
	utils.Infof("[%s] session renewed after %.0fs, before the provider could cut it",
		t.name(), age.Seconds())
	t.scheduleRenewal(fresh)
	utils.SafeGo("mailru.read", func() { t.readLoop(fresh, 0) })
}

// awaitJoinAck reads until the server answers the namespace join, reporting
// whether it did. Frames arriving meanwhile are handled normally: this
// connection is not carrying our traffic yet, but the server may already be
// sending the other participant's.
func (t *MailruDocsTransport) awaitJoinAck(session *DocSession, conn wsConn) bool {
	// How long the server takes to answer is reported because the answer
	// decides whether the timeout is the binding constraint. Measured with no
	// timing at all: about half of every renewal attempt gave up here, on both
	// peers, which is what drives sessions into the watchdog — each failure
	// costs five seconds before the retry, against eleven seconds of margin
	// between the renewal and the provider's cut. Whether the fix is a longer
	// wait or an earlier start depends on where the successful answers land,
	// and nothing so far has recorded that.
	started := time.Now()
	conn.SetReadDeadline(time.Now().Add(joinAckTimeout))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			utils.Infof("[%s] join unanswered after %.1fs", t.name(), time.Since(started).Seconds())
			return false
		}
		if isJoinAck(msg) {
			utils.Infof("[%s] join answered in %.2fs", t.name(), time.Since(started).Seconds())
			return true
		}
		t.handleMessage(session, msg)
	}
}

// isJoinAck reports whether the frame is the server accepting the namespace
// join: socket.io packet "40", optionally carrying the session id. The
// goodbye, "41", must not be mistaken for it.
func isJoinAck(msg []byte) bool {
	s := string(msg)
	return s == "40" || strings.HasPrefix(s, "40{")
}

// openPacketTimeout bounds the wait for the server's Engine.IO open packet.
// It is there so a silent server costs a reconnect a few seconds instead of
// holding it forever; the packet itself arrives within a round trip.
const openPacketTimeout = 5 * time.Second

// isOpenPacket reports whether the frame is Engine.IO's "open": packet type 0
// followed by the session's JSON.
func isOpenPacket(msg []byte) bool {
	return strings.HasPrefix(string(msg), "0{")
}

// awaitOpenPacket reads until the server opens the session, reporting whether
// it did. Frames arriving before it are handled normally rather than dropped —
// there should be none, and swallowing one would be the same mistake that hid
// the closures in the first place.
func (t *MailruDocsTransport) awaitOpenPacket(session *DocSession, conn wsConn) bool {
	conn.SetReadDeadline(time.Now().Add(openPacketTimeout))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// A dead or silent socket. The caller writes anyway and the read
			// loop reports the failure through the one path that handles it.
			return false
		}
		if isOpenPacket(msg) {
			return true
		}
		t.handleMessage(session, msg)
	}
}

// recentFrames is how many of the last frames are kept for the post-mortem:
// enough to show a goodbye packet and the couple that preceded it.
const recentFrames = 5

type recentFrame struct {
	at   time.Time
	text string
}

// rememberFrame appends to a bounded tail of what the server sent.
//
// Heartbeats are skipped: they arrive constantly and would push everything
// worth seeing out of the tail. A frame carrying tunnel data is reduced to its
// size — the payload is the user's traffic, and this log gets pasted into
// chats and issues, so it must never contain it.
func rememberFrame(tail []recentFrame, msg []byte) []recentFrame {
	text := string(msg)
	switch {
	case text == "2" || text == "3" || strings.Contains(text, "---KA---"):
		return tail
	case strings.Contains(text, "cursor"):
		text = fmt.Sprintf("<кадр с данными, %d байт>", len(msg))
	default:
		text = truncate(text, 300)
	}
	tail = append(tail, recentFrame{at: time.Now(), text: text})
	if len(tail) > recentFrames {
		tail = tail[len(tail)-recentFrames:]
	}
	return tail
}

// truncate cuts to at most n runes, never mid-character: the frames are JSON
// with Russian text in them, and a byte-wise cut would produce mojibake in the
// one line someone reads to find out what happened.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// reportLastFrames says what the server sent before it closed. One line in the
// ordinary log, because at six closures a minute more than that is noise; the
// whole tail behind --debug for when the one line is not enough.
func reportLastFrames(name string, tail []recentFrame) {
	if len(tail) == 0 {
		utils.Infof("[%s] before the close: nothing arrived at all", name)
		return
	}
	now := time.Now()
	last := tail[len(tail)-1]
	utils.Infof("[%s] before the close, %.1fs earlier: %s", name, now.Sub(last.at).Seconds(), last.text)
	for _, f := range tail[:len(tail)-1] {
		utils.Debugf("[M-DOCS] [%s] %.1fs before the close: %s", name, now.Sub(f.at).Seconds(), f.text)
	}
}

// authMessage builds the socket.io frame that joins the collaborative editing
// session. Extracted so the reuse experiment can send exactly what the
// transport sends, rather than an approximation of it.
func authMessage(info MailruDocsInfo, userID string) string {
	authMsg := map[string]interface{}{
		"type":                "auth",
		"docid":               info.DocKey,
		"documentCallbackUrl": info.CallbackURL,
		"token":               "fghhfgsjdgfjs",
		"user": map[string]interface{}{
			"id":        info.EditorUserID,
			"username":  userID,
			"indexUser": -1,
		},
		"editorType":         0,
		"lastOtherSaveTime":  -1,
		"block":              []interface{}{},
		"documentFormatSave": 65,
		"view":               false,
		"isCloseCoAuthoring": false,
		"openCmd": map[string]interface{}{
			"c":               "open",
			"id":              info.DocKey,
			"userid":          info.EditorUserID,
			"format":          info.FileType,
			"url":             info.DocURL,
			"title":           info.DocTitle,
			"lcid":            25,
			"nobase64":        true,
			"convertToOrigin": ".pdf.xps.oxps.djvu",
		},
		"lang":                  "ru",
		"mode":                  "edit",
		"permissions":           info.Permissions,
		"IsAnonymousUser":       false,
		"timezoneOffset":        -180,
		"coEditingMode":         "fast",
		"jwtOpen":               info.Token,
		"time":                  1000,
		"supportAuthChangesAck": true,
	}
	messagePart, _ := json.Marshal([]interface{}{"message", authMsg})
	return fmt.Sprintf("42%s", string(messagePart))
}

func (t *MailruDocsTransport) writerLoop() {
	// The write queue is created once and preserved across reconnects, so we
	// capture it and block on it instead of polling with a sleep.
	var queue chan []byte
	for t.IsRunning() && queue == nil {
		t.Mu.Lock()
		if t.session != nil {
			queue = t.session.WriteQueue
		}
		t.Mu.Unlock()
		if queue == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if queue == nil {
		return
	}

	var pending []byte
	for t.IsRunning() {
		if pending == nil {
			packet, ok := <-queue
			if !ok {
				return
			}
			pending = packet
		}

		t.Mu.RLock()
		session := t.session
		t.Mu.RUnlock()
		if session == nil || session.Conn == nil {
			// Mid-reconnect: hold the packet and retry rather than drop it.
			time.Sleep(15 * time.Millisecond)
			continue
		}

		payload := base64.StdEncoding.EncodeToString(pending)
		msg := fmt.Sprintf(`42["message",{"type":"cursor","cursor":"18;%s"}]`, payload)
		if err := session.safeWrite(websocket.TextMessage, []byte(msg)); err != nil {
			utils.Debugf("[M-DOCS] Write error: %v", err)
			time.Sleep(15 * time.Millisecond)
			continue // keep pending; the reconnect will bring up a new conn
		}
		// Just after a handover, send the same frame down the session being
		// retired as well: the replacement may not be carrying traffic yet.
		// Errors are ignored — the frame has already been written to the
		// session that is supposed to carry it.
		if shadow := t.retiringSession(); shadow != nil && shadow != session {
			_ = shadow.safeWrite(websocket.TextMessage, []byte(msg))
		}
		pending = nil
	}
}

// retiringSession returns the session replaced by the most recent handover
// while frames are still worth copying to it, and nil once that moment has
// passed.
func (t *MailruDocsTransport) retiringSession() *DocSession {
	t.Mu.RLock()
	s, until := t.retiring, t.retiringUntil
	t.Mu.RUnlock()
	if s == nil || time.Now().After(until) {
		return nil
	}
	return s
}

func (t *MailruDocsTransport) keepAliveLoop() {
	ticker := time.NewTicker(t.GetConfig().KeepAliveInterval)
	defer ticker.Stop()
	keepAliveMsg := `42["message",{"type":"cursor","cursor":"18;---KA---"}]`

	for t.IsRunning() {
		<-ticker.C
		t.Mu.Lock()
		session := t.session
		t.Mu.Unlock()

		// A session older than the provider allows is a contradiction: either
		// it was renewed and this age is stale, or nobody is watching it any
		// more. The second is the dangerous one — the link goes on counting as
		// live while no read loop is left to notice the next cut, so the bond
		// keeps it in reserve and would hand it traffic. Dropping the
		// connection puts it back on the one path that knows how to recover.
		//
		// This is a net, not a mechanism: renewal at fifty seconds is what
		// should keep sessions young. It is here because the first version of
		// renewal could stop silently, and a link that stops renewing must not
		// also stop being noticed.
		if session != nil && session.Conn != nil {
			if age := time.Since(t.ConnectedSince()); age > sessionLifetime+20*time.Second {
				utils.Infof("[%s] session is %.0fs old and was never renewed, dropping it", t.name(), age.Seconds())
				t.SetConnected(false)
				session.Conn.Close()
				continue
			}
		}

		if session != nil && session.Conn != nil {
			if err := session.safeWrite(websocket.TextMessage, []byte(keepAliveMsg)); err != nil {
				// A write that failed on a session we retired ourselves says
				// nothing about the link: its replacement is already carrying
				// traffic. Treating it as a failure would mark a working link
				// dead, and nothing would mark it live again.
				if session.superseded.Load() {
					utils.Debugf("[M-DOCS] [%s] keep-alive hit the retired session, ignoring", t.name())
					continue
				}
				// Marking it disconnected is not enough: nothing acts on that
				// flag. Close the connection instead, which makes the read
				// loop return and take its usual reconnect path — one place
				// owns reconnection rather than two racing each other.
				utils.Infof("[%s] keep-alive failed, dropping the connection: %v", t.name(), err)
				t.SetConnected(false)
				session.Conn.Close()
			}
		}
	}
}

func (t *MailruDocsTransport) handleMessage(session *DocSession, data []byte) {
	text := string(data)

	if strings.Contains(text, "---KA---") {
		return
	}

	// Socket.IO ping - respond with pong
	if text == "2" {
		if session != nil && session.Conn != nil {
			session.safeWrite(websocket.TextMessage, []byte("3"))
		}
		return
	}
	if text == "3" {
		return
	}

	if strings.Contains(text, `"type":"auth"`) && strings.Contains(text, `"result":1`) {
		utils.Debugf("[M-DOCS] Auth OK for user %s", session.UserID)
		return
	}

	if strings.Contains(text, "cursor") {
		base64Str := t.extractBase64String(text)
		if base64Str == "" {
			return
		}

		decoded, err := base64.StdEncoding.DecodeString(base64Str)
		if err != nil {
			utils.Debugf("[M-DOCS] Base64 decode error: %v", err)
			return
		}

		t.RecordReceive(len(decoded))
		t.CallReceive(decoded)
		return
	}

	// Anything else used to fall off the end of this function without a word.
	// Socket.IO has packets the server says goodbye with — "1" for a transport
	// disconnect, "41" for leaving the namespace — and an error event is an
	// ordinary message too. Discarding them silently is why every closure has
	// looked unexplained.
	utils.Debugf("[M-DOCS] [%s] unhandled frame: %s", t.name(), truncate(text, 300))
}

func (t *MailruDocsTransport) extractBase64String(response string) string {
	matches := cursorPayloadRe.FindStringSubmatch(response)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func (t *MailruDocsTransport) scheduleReconnect(attempt int) {
	next := attempt + 1
	if !t.IsRunning() || next >= t.GetConfig().MaxReconnectAttempts {
		return
	}
	if !t.reconnecting.CompareAndSwap(false, true) {
		utils.Debugf("[M-DOCS] reconnect already pending, skipping")
		return
	}
	// Held only across the backoff: connectToDoc hands off to a goroutine, and
	// a failure there must be free to schedule the next attempt.
	defer t.reconnecting.Store(false)

	// No stagger here. Phase is set by holding the first connection back, at
	// startup; adding it to a reconnect would hold a link down at the one
	// moment the bond most needs it back.
	d := reconnectBackoff(next)
	utils.Debugf("[M-DOCS] reconnecting in %v (attempt %d)", d, next)
	time.Sleep(d)
	if !t.IsRunning() {
		return
	}

	t.RecordReconnect()
	t.connectToDoc(next)
}

// reconnectBackoff returns an exponential backoff with jitter, capped at 15s.
func reconnectBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 5 {
		shift = 5
	}
	d := 500 * time.Millisecond * time.Duration(1<<uint(shift))
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	// add up to +50% jitter
	d += time.Duration(rand.Int63n(int64(d/2) + 1))
	return d
}

// fetchDocInfo POSTs to Mail.ru's public-document editor API and parses the
// response into the fields needed to open the collaborative WebSocket.
func (t *MailruDocsTransport) fetchDocInfo(weblink string) (MailruDocsInfo, error) {
	client := &http.Client{Timeout: 15 * time.Second}

	reqBody := map[string]string{
		"x-email":  "anonym",
		"public":   "/" + weblink,
		"platform": "desktop_web",
	}
	jsonData, _ := json.Marshal(reqBody)

	apiURL := "https://cloud.mail.ru/api/v4/r7/edit"
	utils.Debugf("[M-DOCS] fetchDocInfo POST %s", apiURL)

	req, _ := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", mailruUserAgent)
	req.Header.Set("X-Api-Version", "4")
	req.Header.Set("Referer", fmt.Sprintf("https://cloud.mail.ru/public/%s?weblink=%s", weblink, weblink))

	resp, err := client.Do(req)
	if err != nil {
		return MailruDocsInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return MailruDocsInfo{}, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	bodyBytes, _ := io.ReadAll(resp.Body)

	var res map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return MailruDocsInfo{}, fmt.Errorf("failed to parse JSON: %w", err)
	}

	apiBase, _ := res["api"].(string)
	token, _ := res["token"].(string)

	document, ok := res["document"].(map[string]interface{})
	if !ok || document == nil {
		return MailruDocsInfo{}, fmt.Errorf("document object missing")
	}

	docKey, _ := document["key"].(string)
	fileType, _ := document["fileType"].(string)
	docURL, _ := document["url"].(string)
	docTitle, _ := document["title"].(string)
	// document.permissions is an object of booleans (comment/edit/download/…),
	// not a number - sending it as anything else makes the editor server
	// reject the auth message with "access deny".
	permissions, _ := document["permissions"].(map[string]interface{})
	if permissions == nil {
		permissions = make(map[string]interface{})
	}

	editorConfig, ok := res["editorConfig"].(map[string]interface{})
	if !ok || editorConfig == nil {
		return MailruDocsInfo{}, fmt.Errorf("editorConfig object missing")
	}
	callbackURL, _ := editorConfig["callbackUrl"].(string)

	userObj, _ := editorConfig["user"].(map[string]interface{})
	var editorUserID string
	if userObj != nil {
		editorUserID, _ = userObj["id"].(string)
	}

	wsBase := strings.Replace(apiBase, "https://", "wss://", 1)
	wsURL := fmt.Sprintf("%s/doc/%s/c/?EIO=4&transport=websocket", wsBase, docKey)

	return MailruDocsInfo{
		Token:        token,
		DocKey:       docKey,
		WsURL:        wsURL,
		FileType:     fileType,
		DocURL:       docURL,
		DocTitle:     docTitle,
		Permissions:  permissions,
		CallbackURL:  callbackURL,
		EditorUserID: editorUserID,
	}, nil
}

func randUserID() string {
	return fmt.Sprintf("%010d", rand.New(rand.NewSource(time.Now().UnixNano())).Intn(1000000000))
}
