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

type DocSession struct {
	Info       MailruDocsInfo
	Conn       *websocket.Conn
	WriteQueue chan []byte
	UserID     string
	writeMu    sync.Mutex
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

	// stagger delays this link's next reconnect once, shifting its phase away
	// from its siblings in a bond. Consumed on first use: every later cycle
	// inherits the offset, so it never needs applying twice.
	staggerMu sync.Mutex
	stagger   time.Duration

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

// SetReconnectStagger implements transport.Staggerer.
func (t *MailruDocsTransport) SetReconnectStagger(d time.Duration) {
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
	t.connectToDoc(0)

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

		utils.Debugf("[M-DOCS] WebSocket dial %s", info.WsURL)
		conn, resp, err := dialer.Dial(info.WsURL, headers)
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

		// Auth - fired immediately, same as the Yandex.Docs transport. No
		// need to wait for the server's own "0{"/"40" handshake frames
		// first: Mail.ru's coauthoring server buffers and processes these
		// once its own session state catches up, and waiting for explicit
		// acks here only stretches the outage window on every reconnect
		// (Mail.ru can delay a fresh joiner's auth confirmation by up to
		// ~30s while it reconciles with the other participant).
		auth1 := fmt.Sprintf(`40{"token":"%s"}`, info.Token)
		session.safeWrite(websocket.TextMessage, []byte(auth1))

		session.safeWrite(websocket.TextMessage, []byte(authMessage(info, userID)))

		connectedAt := time.Now()
		for t.IsRunning() {
			// Reset before every read: the deadline is absolute, not a
			// per-call idle timeout.
			conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
			_, message, err := conn.ReadMessage()
			if err != nil {
				// The reason a link dies is one line per event and the first
				// thing anyone asks, so it belongs in the ordinary log rather
				// than behind --debug.
				utils.Infof("[%s] connection lost: %v", t.name(), err)
				t.SetConnected(false)
				conn.Close()

				// A session that barely lasted suggests the data we opened it
				// with is no longer good; a long-lived one that ends is just
				// the provider recycling it, and the cache is still fine.
				next := attempt
				if time.Since(connectedAt) > 15*time.Second {
					next = -1
				} else {
					t.forgetDocInfo()
				}
				t.scheduleReconnect(next)
				return
			}
			t.handleMessage(session, message)
		}
	}()
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
		pending = nil
	}
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

		if session != nil && session.Conn != nil {
			if err := session.safeWrite(websocket.TextMessage, []byte(keepAliveMsg)); err != nil {
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
	}
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

	d := reconnectBackoff(next)
	if extra := t.takeStagger(); extra > 0 {
		// Once, on the way back from the first closure: hold off so this link
		// stops expiring in the same second as its siblings.
		utils.Infof("[%s] holding off %v to spread the links apart", t.name(), extra)
		d += extra
	}
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
