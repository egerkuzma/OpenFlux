package cupsonline

import (
	"crypto/rand"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"openflux/transport"
)

// TestRoomCapacity answers one question the architecture turns on: is the
// ~3.2 Mbit/s a room saturates at a limit of that room, or of the service
// behind all of them?
//
// It matters because four rooms are created and only one is used. Spreading a
// flow across them costs 6.6× in throughput — packets of one connection arrive
// out of order and TCP reads that as loss — so the only way to use the other
// three is to pin each flow to a room, and that needs the room chosen above
// the codec, where flows are still visible. That is a sizeable rebuild, and
// worth doing only if rooms add up.
//
// Both peers run here, in one process, with no tunnel and no codec between
// them: what this measures is the channel itself.
//
//	CUPS_PROBE=1 go test ./transport/cupsonline/ -run RoomCapacity -v -timeout 10m
func TestRoomCapacity(t *testing.T) {
	if os.Getenv("CUPS_PROBE") == "" {
		t.Skip("создаёт настоящие комнаты на сервисе; включается CUPS_PROBE=1")
	}

	for _, rooms := range []int{1, 4} {
		got := measureRooms(t, rooms, 20*time.Second)
		t.Logf("комнат %d: %.0f КБ/с (%.2f Мбит/с)", rooms, got/1024, got*8/1e6)
	}
}

// measureRooms brings up both ends over n rooms and returns bytes per second
// actually delivered.
func measureRooms(t *testing.T, rooms int, dur time.Duration) float64 {
	t.Helper()

	// Раскладку включаем принудительно: в бою она выключена, потому что ломает
	// порядок и TCP считает это потерей. Здесь TCP внутри нет — мы считаем
	// сырые байты, — поэтому переупорядочивание безвредно, а без раскладки
	// обе стороны писали бы в одну комнату и замер сравнивал бы её с собой.
	oldSpread := cupsSpread
	cupsSpread = true
	defer func() { cupsSpread = oldSpread }()

	cfg := transport.TransportConfig{}
	exit := NewCupsonlineTransport("", cfg, false)
	exit.config.NumRooms = rooms
	if err := exit.Start(); err != nil {
		t.Fatalf("%d комнат: нода не поднялась: %v", rooms, err)
	}
	defer exit.Stop()
	if len(exit.auths) != rooms {
		t.Fatalf("создано %d комнат из %d — сервис отказал, замер недействителен",
			len(exit.auths), rooms)
	}

	client := NewCupsonlineTransport(packRooms(exit.auths), cfg, true)
	client.config.NumRooms = rooms
	var received atomic.Uint64
	client.Receive(func(b []byte) { received.Add(uint64(len(b))) })
	if err := client.Start(); err != nil {
		t.Fatalf("%d комнат: клиент не поднялся: %v", rooms, err)
	}
	defer client.Stop()

	time.Sleep(3 * time.Second) // дать websocket'ам встать

	// 8 КБ — столько несёт одна пачка кодека в бою.
	// Случайные байты, а не узор. Первая версия набивала пакет как byte(i), и
	// zstd в кодеке ужимал его почти в ничто: замер показал 130 Мбит/с через
	// канал, который тянет восемь. Боевой трафик — шифртекст, он не сжимается.
	payload := make([]byte, 8192)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("случайные данные: %v", err)
	}

	started := time.Now()
	deadline := started.Add(dur)
	for time.Now().Before(deadline) {
		if err := exit.Send(payload); err != nil {
			t.Logf("%d комнат: отправка отказала: %v", rooms, err)
			break
		}
	}
	// Дать долететь тому, что уже в полёте.
	time.Sleep(5 * time.Second)

	return float64(received.Load()) / time.Since(started).Seconds()
}

// TestLayerCost finds where the throughput goes. The channel itself carries
// 10.5 Mbit/s through one room, measured; the tunnel built on it delivers 3.2.
// That threefold loss is ours, somewhere between an IP packet and a room, and
// until now no layer had ever been measured on its own.
//
// Each step adds exactly one layer to the same probe, so the difference between
// two lines is the cost of what was added.
//
//	CUPS_PROBE=1 go test ./transport/cupsonline/ -run LayerCost -v -timeout 15m
func TestLayerCost(t *testing.T) {
	if os.Getenv("CUPS_PROBE") == "" {
		t.Skip("создаёт нагрузку на сервис; включается CUPS_PROBE=1")
	}
	packed := os.Getenv("CUPS_ROOMS")
	if packed == "" {
		t.Skip("нужен список комнат: CUPS_ROOMS=<упакованный список>")
	}

	for _, step := range []struct {
		name string
		wrap func(transport.Transport, bool) (transport.Transport, error)
	}{
		{"голый cups", nil},
		{"+ кодек (batched)", func(in transport.Transport, _ bool) (transport.Transport, error) {
			return transport.NewBatchedTransport(in), nil
		}},
		{"+ кодек + шифрование", func(in transport.Transport, exit bool) (transport.Transport, error) {
			return transport.NewEncryptedTransport(transport.NewBatchedTransport(in),
				"проба-ключа-для-замера", "cupsonline", exit)
		}},
	} {
		got, refused := measureLayer(t, packed, step.wrap, 20*time.Second)
		line := fmt.Sprintf("%-24s %7.0f КБ/с  (%.2f Мбит/с)", step.name, got/1024, got*8/1e6)
		if refused > 0 {
			line += fmt.Sprintf("   обратное давление: %d", refused)
		}
		t.Log(line)
	}
}

// measureLayer поднимает пару заново под каждый слой: Start не идемпотентен, и
// обёртка над уже поднятым транспортом поднимает его вторично, удваивая
// каналы. Комнаты при этом берутся готовые — создание их сервис ограничивает.
func measureLayer(t *testing.T, packed string,
	wrap func(transport.Transport, bool) (transport.Transport, error), dur time.Duration) (float64, int) {
	t.Helper()

	cfg := transport.TransportConfig{}
	senderCups := NewCupsonlineTransport(packed, cfg, true)
	receiverCups := NewCupsonlineTransport(packed, cfg, true)

	var sender, receiver transport.Transport = senderCups, receiverCups
	if wrap != nil {
		var err error
		if sender, err = wrap(senderCups, true); err != nil {
			t.Fatalf("обёртка отправителя: %v", err)
		}
		if receiver, err = wrap(receiverCups, false); err != nil {
			t.Fatalf("обёртка получателя: %v", err)
		}
	}
	if err := sender.Start(); err != nil {
		t.Fatalf("отправитель не поднялся: %v", err)
	}
	defer sender.Stop()
	if err := receiver.Start(); err != nil {
		t.Fatalf("получатель не поднялся: %v", err)
	}
	defer receiver.Stop()

	var received atomic.Uint64
	receiver.Receive(func(b []byte) { received.Add(uint64(len(b))) })

	time.Sleep(3 * time.Second)

	// Случайные байты, а не узор. Первая версия набивала пакет как byte(i), и
	// zstd в кодеке ужимал его почти в ничто: замер показал 130 Мбит/с через
	// канал, который тянет восемь. Боевой трафик — шифртекст, он не сжимается.
	payload := make([]byte, 8192)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("случайные данные: %v", err)
	}

	// «batch queue full» — не отказ, а обратное давление: слой сообщает, что
	// быстрее не переваривает. Притормаживаем, иначе замерим скорость записи в
	// очередь, а не скорость доставки.
	var refused int
	started := time.Now()
	for deadline := started.Add(dur); time.Now().Before(deadline); {
		if err := sender.Send(payload); err != nil {
			refused++
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(5 * time.Second)
	return float64(received.Load()) / time.Since(started).Seconds(), refused
}
