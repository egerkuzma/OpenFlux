package main

import "testing"

func TestDocURLListParsing(t *testing.T) {
	cases := []struct {
		name  string
		input []string // successive --url values
		want  []string
	}{
		{"one", []string{"https://a"}, []string{"https://a"}},
		{"repeated flag", []string{"https://a", "https://b"}, []string{"https://a", "https://b"}},
		{"comma separated", []string{"https://a,https://b,https://c"}, []string{"https://a", "https://b", "https://c"}},
		{"whitespace and commas", []string{" https://a , https://b "}, []string{"https://a", "https://b"}},
		{"newlines, as pasted from a list", []string{"https://a\nhttps://b"}, []string{"https://a", "https://b"}},
		{"empty pieces ignored", []string{"https://a,,  ,https://b"}, []string{"https://a", "https://b"}},
		{"nothing at all", []string{""}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got docURLList
			for _, in := range c.input {
				if err := got.Set(in); err != nil {
					t.Fatalf("Set(%q): %v", in, err)
				}
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("got %q, want %q", got, c.want)
				}
			}
		})
	}
}

func TestEncryptionContext(t *testing.T) {
	// A single document must derive exactly what it derived before bonding
	// existed, or every current deployment stops decrypting.
	if got := encryptionContext("mailru", []string{"https://a"}); got != "https://a" {
		t.Errorf("single document: got %q, want the URL itself", got)
	}

	// Both peers must land on the same context even if the documents were
	// listed in a different order.
	a := encryptionContext("mailru", []string{"https://c", "https://a", "https://b"})
	b := encryptionContext("mailru", []string{"https://a", "https://b", "https://c"})
	if a != b {
		t.Errorf("order changed the context: %q vs %q", a, b)
	}

	// A different set of documents must derive a different context.
	if c := encryptionContext("mailru", []string{"https://a", "https://b"}); c == a {
		t.Error("a different set of documents must not share a context")
	}

	// No document at all: fall back to the transport name.
	if got := encryptionContext("oneme", nil); got != "oneme" {
		t.Errorf("no document: got %q, want the transport name", got)
	}
	if got := encryptionContext("oneme", []string{""}); got != "oneme" {
		t.Errorf("empty document: got %q, want the transport name", got)
	}
}

func TestParseDocumentList(t *testing.T) {
	const file = `
# документы для узла, по одному в строке
https://cloud.mail.ru/public/AAA/1
https://cloud.mail.ru/public/BBB/2   # запасной

# пустая строка выше игнорируется
https://cloud.mail.ru/public/CCC/3,https://cloud.mail.ru/public/DDD/4
   https://cloud.mail.ru/public/EEE/5
#https://cloud.mail.ru/public/OFF/6
`
	got := parseDocumentList(file)
	want := []string{
		"https://cloud.mail.ru/public/AAA/1",
		"https://cloud.mail.ru/public/BBB/2",
		"https://cloud.mail.ru/public/CCC/3",
		"https://cloud.mail.ru/public/DDD/4",
		"https://cloud.mail.ru/public/EEE/5",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d documents %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("document %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseDocumentListEdgeCases(t *testing.T) {
	cases := map[string]int{
		"":                     0,
		"\n\n   \n":            0,
		"# только комментарий": 0,
		"https://a":            1,
		"https://a https://b":  2,
	}
	for in, want := range cases {
		if got := len(parseDocumentList(in)); got != want {
			t.Errorf("parseDocumentList(%q) gave %d documents, want %d", in, got, want)
		}
	}
}
