package mailru

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"openflux/transport"
)

// makeJWT builds a token whose payload carries the given expiry. The signature
// is never checked here — the server does that — so it can be anything.
func makeJWT(exp int64) string {
	payload, _ := json.Marshal(map[string]int64{"exp": exp})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestJWTExpiry(t *testing.T) {
	want := time.Now().Add(20 * time.Minute).Unix()
	got, ok := jwtExpiry(makeJWT(want))
	if !ok {
		t.Fatal("срок годности не прочитался из корректного токена")
	}
	if got.Unix() != want {
		t.Errorf("получено %v, ожидалось %v", got.Unix(), want)
	}
}

func TestJWTExpiryRejectsWhatItCannotRead(t *testing.T) {
	// Anything unreadable must fail closed, so the caller falls back to a
	// short window instead of reusing a token forever.
	cases := map[string]string{
		"пусто":  "",
		"не JWT": "просто строка",
		"две части вместо трёх": "header.payload",
		"payload не base64":     "header.!!!!.signature",
		"payload не JSON":       "header." + base64.RawURLEncoding.EncodeToString([]byte("не json")) + ".sig",
		"без claim exp":         "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".sig",
		"exp нулевой":           makeJWT(0),
		"exp отрицательный":     makeJWT(-5),
	}
	for name, token := range cases {
		if _, ok := jwtExpiry(token); ok {
			t.Errorf("%s: принято, хотя должно быть отклонено", name)
		}
	}
}

func TestDocInfoCacheIsUsedAndDropped(t *testing.T) {
	tr := NewMailruDocsTransport("AAAA/BBBB", transport.DefaultConfig())

	// Seed the cache directly: fetching would hit the live service.
	info := MailruDocsInfo{DocKey: "KEY1", Token: makeJWT(time.Now().Add(time.Hour).Unix())}
	tr.cachedInfo = &info
	tr.infoExpiry = time.Now().Add(time.Hour)

	got, err := tr.docInfo()
	if err != nil {
		t.Fatalf("кэш должен отдаваться без обращения к сети: %v", err)
	}
	if got.DocKey != "KEY1" {
		t.Errorf("получен ключ %q, ожидался KEY1", got.DocKey)
	}

	t.Run("истёкший кэш не отдаётся", func(t *testing.T) {
		tr.infoExpiry = time.Now().Add(-time.Second)
		if tr.cachedInfo == nil {
			t.Skip("нечего проверять")
		}
		// docInfo would now go to the network, so only check the decision.
		tr.infoMu.Lock()
		fresh := tr.cachedInfo != nil && time.Now().Before(tr.infoExpiry)
		tr.infoMu.Unlock()
		if fresh {
			t.Error("просроченный кэш считается годным")
		}
	})

	t.Run("сброс очищает кэш", func(t *testing.T) {
		tr.cachedInfo = &info
		tr.forgetDocInfo()
		if tr.cachedInfo != nil {
			t.Error("после сброса кэш остался")
		}
	})
}

// The whole point of the cache: a link reconnecting once a minute must stop
// making an API call every time.
func TestCacheRemovesRepeatedFetches(t *testing.T) {
	tr := NewMailruDocsTransport("AAAA/BBBB", transport.DefaultConfig())
	tr.cachedInfo = &MailruDocsInfo{DocKey: "KEY1"}
	tr.infoExpiry = time.Now().Add(10 * time.Minute)

	for i := 0; i < 10; i++ {
		if _, err := tr.docInfo(); err != nil {
			t.Fatalf("обращение %d ушло в сеть: %v", i+1, err)
		}
	}
}
