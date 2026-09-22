package mailru

import (
	"strings"
	"testing"
	"time"

	"openflux/transport"
)

// newTestTransport builds the real object: BaseTransport is embedded by
// pointer, so a bare struct literal has no mutex and no stats and panics the
// moment anything touches them.
func newTestTransport() *MailruDocsTransport {
	return NewMailruDocsTransport("abc/def", transport.TransportConfig{})
}

// The stagger exists to keep links from opening — and therefore expiring —
// in the same second, and it is paid once, on the first connection, where no
// traffic is flowing yet. Handing it out twice would delay every reconnect for
// the life of the link, which is the opposite of what it is for.
func TestStaggerIsPaidOnceAndOnlyOnce(t *testing.T) {
	tr := newTestTransport()
	tr.SetStartStagger(7 * time.Second)

	if got := tr.takeStagger(); got != 7*time.Second {
		t.Errorf("первый запуск подождал %v, ожидалось 7s", got)
	}
	if got := tr.takeStagger(); got != 0 {
		t.Errorf("переподключение снова ждёт %v — задержка стала постоянной", got)
	}
}

func TestNoStaggerMeansNoWait(t *testing.T) {
	tr := newTestTransport()
	if got := tr.takeStagger(); got != 0 {
		t.Errorf("без настройки ждём %v", got)
	}
}

// A replaced session keeps being read for a short while after the handover, so
// frames already in flight are not lost. Reading it past that window would
// mean taking traffic from a connection the provider is about to cut.
func TestARetiredSessionIsOfferedOnlyInsideItsWindow(t *testing.T) {
	tr := newTestTransport()
	old := &DocSession{UserID: "старая"}

	tr.Mu.Lock()
	tr.retiring, tr.retiringUntil = old, time.Now().Add(time.Second)
	tr.Mu.Unlock()
	if got := tr.retiringSession(); got != old {
		t.Error("внутри окна старая сессия не отдаётся — кадры в полёте потеряются")
	}

	tr.Mu.Lock()
	tr.retiringUntil = time.Now().Add(-time.Millisecond)
	tr.Mu.Unlock()
	if got := tr.retiringSession(); got != nil {
		t.Error("окно истекло, а сессия всё ещё отдаётся")
	}
}

func TestNoRetiredSessionWhenThereWasNoHandover(t *testing.T) {
	tr := newTestTransport()
	if got := tr.retiringSession(); got != nil {
		t.Errorf("без передачи эстафеты вернулась сессия %v", got)
	}
}

// Backoff has to grow, and it has to stop growing. An unbounded one turns a
// provider hiccup into a link that never comes back; a flat one hammers a
// service that is already refusing.
func TestReconnectBackoffGrowsThenLevelsOff(t *testing.T) {
	const ceiling = 15 * time.Second

	first := reconnectBackoff(1)
	if first < 500*time.Millisecond || first > 750*time.Millisecond {
		t.Errorf("первая пауза %v, ожидалось 500–750мс", first)
	}

	var prev time.Duration
	for n := 1; n <= 10; n++ {
		d := reconnectBackoff(n)
		if d <= 0 {
			t.Fatalf("попытка %d дала паузу %v", n, d)
		}
		if d > ceiling+ceiling/2 {
			t.Errorf("попытка %d ждёт %v — потолок пробит", n, d)
		}
		if n <= 6 && n > 1 && d < prev/2 {
			t.Errorf("попытка %d ждёт %v, меньше половины от предыдущей %v", n, d, prev)
		}
		prev = d
	}

	// A nonsensical attempt number must not produce a negative wait.
	if d := reconnectBackoff(0); d <= 0 {
		t.Errorf("нулевая попытка дала паузу %v", d)
	}
	if d := reconnectBackoff(-5); d <= 0 {
		t.Errorf("отрицательная попытка дала паузу %v", d)
	}
}

// The link is given as a URL by a person and used as a bare identifier. Every
// shape mail.ru hands out has to reduce to the same thing, or two peers given
// the same document in different forms would not meet in it.
func TestEveryShapeOfLinkReducesToTheSameDocument(t *testing.T) {
	const want = "abc123/def456"
	for _, in := range []string{
		"https://cloud.mail.ru/public/abc123/def456",
		"http://cloud.mail.ru/public/abc123/def456",
		"https://cloud.mail.ru/public/abc123/def456/",
		"  https://cloud.mail.ru/public/abc123/def456  ",
		"abc123/def456",
	} {
		if got := normalizeWeblink(in); got != want {
			t.Errorf("%q превратилось в %q, ожидалось %q", in, got, want)
		}
	}
}

func TestAnUnknownLinkIsLeftAlone(t *testing.T) {
	const in = "https://example.invalid/whatever"
	if got := normalizeWeblink(in); got != in {
		t.Errorf("чужая ссылка изменена: %q", got)
	}
}

// The payload rides in a cursor field after a semicolon. Reading it wrongly
// loses traffic silently, which is indistinguishable from the peer never
// having sent it.
func TestThePayloadIsReadOutOfTheCursorField(t *testing.T) {
	tr := newTestTransport()

	got := tr.extractBase64String(`{"cursor":"user-42;SGVsbG8=","x":1}`)
	if got != "SGVsbG8=" {
		t.Errorf("прочитано %q, ожидалось SGVsbG8=", got)
	}

	for _, in := range []string{
		`{"cursor":"нет разделителя"}`,
		`{"nothing":"here"}`,
		``,
	} {
		if got := tr.extractBase64String(in); got != "" {
			t.Errorf("из %q извлечено %q, ожидалась пустота", in, got)
		}
	}
}

// A heartbeat is not traffic: counting it as a frame would make an idle
// channel look busy, and the frame log exists to say what the last real thing
// was before a session died.
func TestHeartbeatsAndPongsAreNotTraffic(t *testing.T) {
	tr := newTestTransport()
	before := tr.Stats().BytesReceived

	for _, text := range []string{"3", `{"x":"---KA---"}`} {
		tr.handleMessage(nil, []byte(text))
	}

	if after := tr.Stats().BytesReceived; after != before {
		t.Errorf("служебные кадры засчитаны как трафик: %d -> %d", before, after)
	}
}

// A cursor frame is the payload path: it must arrive at the callback decoded,
// because everything above this layer is built on that.
func TestACursorFrameArrivesDecoded(t *testing.T) {
	tr := newTestTransport()
	got := make(chan []byte, 1)
	tr.Receive(func(b []byte) { got <- append([]byte(nil), b...) })

	tr.handleMessage(nil, []byte(`42["message",{"cursor":"user-42;0JDQkdCS"}]`))

	select {
	case b := <-got:
		if string(b) != "АБВ" {
			t.Errorf("доставлено %q, ожидалось АБВ", string(b))
		}
	case <-time.After(time.Second):
		t.Fatal("кадр с полезной нагрузкой не дошёл до получателя")
	}
}

// Undecodable base64 must be dropped, not delivered: handing rubbish upwards
// would fail decryption and look like a key mismatch.
func TestAMangledPayloadIsDroppedNotDelivered(t *testing.T) {
	tr := newTestTransport()
	got := make(chan []byte, 1)
	tr.Receive(func(b []byte) { got <- b })

	tr.handleMessage(nil, []byte(`42["message",{"cursor":"user-42;не-base64!!"}]`))

	select {
	case b := <-got:
		t.Errorf("испорченный кадр доставлен наверх: %q", string(b))
	case <-time.After(200 * time.Millisecond):
	}
}

func TestLabelIsWhatTheLinkIsCalledInLogs(t *testing.T) {
	tr := newTestTransport()
	tr.SetLabel("документ 2 из 5")
	if got := tr.name(); !strings.Contains(got, "документ 2 из 5") {
		t.Errorf("имя в логах %q не содержит метку", got)
	}
}
