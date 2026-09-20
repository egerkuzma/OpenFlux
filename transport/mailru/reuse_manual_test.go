package mailru

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
)

// Every reconnect currently fetches the document's session data over HTTP
// before opening the WebSocket, and a link reconnects about once a minute
// because the provider closes each session after roughly that long. Across two
// peers and five documents that is some six hundred API calls an hour, all of
// which would be avoidable if the data from the previous fetch could simply be
// reused.
//
// Whether it can is not visible in the code: the token looks like a JWT with a
// lifetime of its own, and the document key comes back different on every
// fetch, which is exactly what a single-use, session-scoped value would do. So
// this measures it instead of guessing — fetch once, then open two connections
// in a row with that same data and see whether the second one is accepted.
//
// It talks to the live service, so it only runs when a document is supplied:
//
//	OPENFLUX_TEST_DOC='https://cloud.mail.ru/public/XXXX/YYYY' \
//	    go test ./transport/mailru/ -run TestDocInfoReuse -v
//
// Note that it briefly opens extra sessions on that document. If the document
// is carrying a live tunnel, expect a short blip on that one link.
func TestDocInfoReuse(t *testing.T) {
	weblink := os.Getenv("OPENFLUX_TEST_DOC")
	if weblink == "" {
		t.Skip("set OPENFLUX_TEST_DOC to a cloud.mail.ru document to run this")
	}

	tr := NewMailruDocsTransport(weblink, transport.DefaultConfig())

	info, err := tr.fetchDocInfo(tr.weblink)
	if err != nil {
		t.Fatalf("первый запрос данных документа не прошёл: %v", err)
	}
	t.Logf("получены данные документа: ключ %s…, токен %d символов",
		safePrefix(info.DocKey), len(info.Token))

	if ok, why := tryConnect(t, tr, info, "первое соединение"); !ok {
		t.Fatalf("даже первое соединение по свежим данным не поднялось: %s", why)
	}

	// The question: does the same session data open a second connection, or is
	// it consumed by the first?
	ok, why := tryConnect(t, tr, info, "второе соединение по тем же данным")
	if ok {
		t.Log("ВЫВОД: данные переиспользуются — запрос к API при переподключении можно пропускать")
	} else {
		t.Logf("ВЫВОД: данные одноразовые (%s) — запрос к API при каждом переподключении обязателен", why)
	}

	// Whether the key changes between fetches tells us the same thing from the
	// other side, and costs one more request.
	second, err := tr.fetchDocInfo(tr.weblink)
	if err != nil {
		t.Logf("повторный запрос не прошёл: %v", err)
		return
	}
	if second.DocKey == info.DocKey {
		t.Log("ключ документа не меняется между запросами — он относится к документу, не к сессии")
	} else {
		t.Log("ключ документа меняется при каждом запросе — он выдаётся на сессию")
	}
}

// tryConnect opens one WebSocket with the given session data and reports
// whether the server accepted the authentication.
func tryConnect(t *testing.T, tr *MailruDocsTransport, info MailruDocsInfo, what string) (bool, string) {
	t.Helper()

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		NetDialContext:   (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}
	headers := http.Header{}
	headers.Set("User-Agent", mailruUserAgent)
	headers.Set("Origin", "https://docs.datacloudmail.ru")

	conn, resp, err := dialer.Dial(info.WsURL, headers)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return false, fmt.Sprintf("%s: рукопожатие не прошло (http %d): %v", what, status, err)
	}
	defer conn.Close()

	session := &DocSession{Info: info, Conn: conn, UserID: randUserID()}
	session.safeWrite(websocket.TextMessage, []byte(fmt.Sprintf(`40{"token":"%s"}`, info.Token)))
	session.safeWrite(websocket.TextMessage, []byte(authMessage(info, session.UserID)))

	// Accept either an explicit auth confirmation or simply the absence of a
	// rejection within a few seconds: the server is chatty, and a refusal
	// arrives as a close or an error payload.
	conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return false, fmt.Sprintf("%s: соединение закрыто до подтверждения: %v", what, err)
		}
		text := string(msg)
		if strings.Contains(text, `"type":"auth"`) && strings.Contains(text, `"result":1`) {
			t.Logf("%s: авторизация принята", what)
			return true, ""
		}
		if strings.Contains(text, "access deny") || strings.Contains(text, `"error"`) {
			return false, fmt.Sprintf("%s: сервер отказал: %s", what, trim(text, 120))
		}
	}
}

func safePrefix(s string) string {
	if len(s) > 6 {
		return s[:6]
	}
	return s
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
