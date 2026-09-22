package network

import (
	"strings"
	"testing"
)

// A checksum is right when the receiver's sum over header-plus-checksum comes
// to zero. That is the property every stack actually applies, and checking it
// rather than a magic number means the test does not have to be rewritten when
// the sample packet changes.
func verifies(b []byte) bool { return IPChecksum(b) == 0 }

func ipHeader(src, dst [4]byte, proto byte, total int) []byte {
	h := make([]byte, 20)
	h[0] = 0x45
	h[2] = byte(total >> 8)
	h[3] = byte(total)
	h[8] = 64
	h[9] = proto
	copy(h[12:16], src[:])
	copy(h[16:20], dst[:])
	return h
}

func TestAnIPHeaderVerifiesOnceItsChecksumIsFilledIn(t *testing.T) {
	h := ipHeader([4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 6, 40)

	ck := IPChecksum(h)
	h[10], h[11] = byte(ck>>8), byte(ck)

	if !verifies(h) {
		t.Errorf("заголовок с проставленной суммой не сходится: %#04x", IPChecksum(h))
	}
}

// One flipped bit has to break it, or the checksum is not doing its job.
func TestACorruptedHeaderStopsVerifying(t *testing.T) {
	h := ipHeader([4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 6, 40)
	ck := IPChecksum(h)
	h[10], h[11] = byte(ck>>8), byte(ck)

	h[15] ^= 0x01 // сосед по сети стал другим адресом
	if verifies(h) {
		t.Error("испорченный заголовок прошёл проверку")
	}
}

// An odd number of bytes is the case the folding loop gets wrong if the last
// byte is dropped instead of being taken as the high half of a word.
func TestOddLengthsCountTheLastByte(t *testing.T) {
	even := IPChecksum([]byte{0x01, 0x02, 0x03, 0x04})
	odd := IPChecksum([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
	if even == odd {
		t.Error("лишний байт не повлиял на сумму — он потерян")
	}
}

func TestTCPChecksumCoversThePseudoHeader(t *testing.T) {
	seg := []byte{
		0x9c, 0x40, 0x01, 0xbb, // порты
		0, 0, 0x03, 0xe8, 0, 0, 0, 0,
		0x50, 0x02, 0xff, 0xff, 0, 0, 0, 0,
	}
	src, dst := [4]byte{10, 0, 0, 1}, [4]byte{93, 184, 216, 34}

	base := TCPChecksum(seg, src, dst)

	// The addresses are not in the segment, so a checksum that ignored the
	// pseudo-header would not notice them changing — and the packet would be
	// accepted by a host it was never addressed to.
	if other := TCPChecksum(seg, src, [4]byte{93, 184, 216, 35}); other == base {
		t.Error("смена адреса назначения не изменила сумму — псевдозаголовок не учтён")
	}
	if other := TCPChecksum(seg, [4]byte{10, 0, 0, 9}, dst); other == base {
		t.Error("смена адреса источника не изменила сумму")
	}
	// Length is in the pseudo-header too.
	if other := TCPChecksum(append(seg, 0x00, 0x01), src, dst); other == base {
		t.Error("смена длины не изменила сумму")
	}
}

// ParsePacketInfo is what every debug line in the tunnel is built from. It runs
// on packets that arrive from outside, so a short or malformed one must produce
// a string, not a panic: the alternative is a crash on the first stray frame.
func TestPacketInfoDescribesATCPSegment(t *testing.T) {
	p := make([]byte, 40)
	copy(p, ipHeader([4]byte{10, 0, 0, 2}, [4]byte{93, 184, 216, 34}, 6, 40))
	p[20], p[21] = 0x9c, 0x40 // порт источника 40000
	p[22], p[23] = 0x01, 0xbb // порт назначения 443
	p[33] = 0x02              // SYN
	p[34], p[35] = 0xff, 0xff

	got := ParsePacketInfo(p)
	for _, want := range []string{"TCP", "10.0.0.2", "93.184.216.34", "443", "SYN"} {
		if !strings.Contains(got, want) {
			t.Errorf("в описании %q нет %q", got, want)
		}
	}
}

func TestPacketInfoSurvivesRubbish(t *testing.T) {
	for name, pkt := range map[string][]byte{
		"пусто":            {},
		"обрывок":          {0x45, 0x00, 0x00},
		"ровно по границе": make([]byte, 20),
		"TCP без сегмента": func() []byte { h := make([]byte, 21); h[0] = 0x45; h[9] = 6; return h }(),
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: разбор упал: %v", name, r)
				}
			}()
			if got := ParsePacketInfo(pkt); got == "" {
				t.Errorf("%s: описание пустое", name)
			}
		}()
	}
}
