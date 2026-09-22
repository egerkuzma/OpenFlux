package cupsonline

import (
	"testing"
)

// testChannels builds n rooms that accept sends without a network, and returns
// the transport plus each room's queue.
func testChannels(n int, connected ...bool) (*CupsonlineTransport, []chan []byte) {
	t := &CupsonlineTransport{config: DefaultCupsonlineConfig()}
	queues := make([]chan []byte, n)
	for i := 0; i < n; i++ {
		q := make(chan []byte, 64)
		queues[i] = q
		ws := &cupsWS{
			auth:      &cupsAuth{roomUUID: "room"},
			config:    t.config,
			ctx:       make(chan struct{}),
			sendQueue: q,
			stats:     &channelStats{idx: i},
		}
		up := true
		if i < len(connected) {
			up = connected[i]
		}
		ws.connected.Store(up)
		t.wss = append(t.wss, ws)
	}
	return t, queues
}

// Every room must carry traffic. Until this was fixed the transport hashed a
// flow key out of what it thought was an IPv4 packet, but a codec sits above it
// and hands down a batch, so the key was always zero: all four rooms' worth of
// traffic went down one of them and three quarters of the channel sat idle.
func TestSendUsesEveryRoom(t *testing.T) {
	old := cupsSpread
	cupsSpread = true
	defer func() { cupsSpread = old }()

	tr, queues := testChannels(4)

	for i := 0; i < 40; i++ {
		if err := tr.Send([]byte{0x02, byte(i)}); err != nil {
			t.Fatalf("отправка %d не прошла: %v", i, err)
		}
	}

	for i, q := range queues {
		if len(q) == 0 {
			t.Errorf("комната %d не получила ни одного батча — раскладка снова схлопнулась", i)
		}
	}

	total := 0
	for _, q := range queues {
		total += len(q)
	}
	if total != 40 {
		t.Errorf("доставлено %d батчей из 40", total)
	}
}

// A room that is down must be stepped over, not queued into, or a reconnect
// would swallow traffic that three healthy rooms could have carried.
func TestSendSkipsRoomsThatAreDown(t *testing.T) {
	old := cupsSpread
	cupsSpread = true
	defer func() { cupsSpread = old }()

	tr, queues := testChannels(4, true, false, false, true)

	for i := 0; i < 20; i++ {
		if err := tr.Send([]byte{0x02, byte(i)}); err != nil {
			t.Fatalf("отправка %d не прошла: %v", i, err)
		}
	}

	if len(queues[1]) != 0 || len(queues[2]) != 0 {
		t.Errorf("в отключённые комнаты попало %d и %d батчей", len(queues[1]), len(queues[2]))
	}
	if got := len(queues[0]) + len(queues[3]); got != 20 {
		t.Errorf("живые комнаты получили %d батчей из 20", got)
	}
}

// With nothing up the batch is still queued rather than dropped: a reconnect
// should cost latency, not data.
func TestSendKeepsDataWhenEveryRoomIsDown(t *testing.T) {
	tr, queues := testChannels(3, false, false, false)

	if err := tr.Send([]byte{0x02, 0x01}); err != nil {
		t.Fatalf("батч отвергнут, хотя очередь свободна: %v", err)
	}

	total := 0
	for _, q := range queues {
		total += len(q)
	}
	if total != 1 {
		t.Errorf("поставлено в очередь %d батчей, ожидался 1", total)
	}
}

// With spreading off every batch must go down one room, in order. TCP reads
// the reordering that four rooms of differing latency produce as packet loss,
// and a flow that is losing packets is capped by loss, not by capacity — which
// is why the choice has to be measurable rather than assumed.
func TestSpreadingOffPinsEverythingToOneRoom(t *testing.T) {
	tr, queues := testChannels(4)
	for i := 0; i < 20; i++ {
		if err := tr.Send([]byte{0x02, byte(i)}); err != nil {
			t.Fatalf("отправка %d не прошла: %v", i, err)
		}
	}

	used, carried := 0, 0
	for _, q := range queues {
		if len(q) > 0 {
			used++
			carried = len(q)
		}
	}
	if used != 1 {
		t.Errorf("задействовано комнат: %d, ожидалась 1", used)
	}
	if carried != 20 {
		t.Errorf("закреплённая комната несёт %d батчей из 20", carried)
	}
}

// Starting twice used to append a second set of channels and launch a second
// stats loop over counters sized for the first, which panicked with an index
// out of range as soon as it ticked. A probe found it; in production a wrapper
// that starts what it wraps would have.
func TestStartingTwiceDoesNotDoubleTheChannels(t *testing.T) {
	tr, _ := testChannels(4)
	tr.started.Store(true) // как будто уже поднят

	if err := tr.Start(); err != nil {
		t.Fatalf("повторный запуск вернул ошибку: %v", err)
	}
	if got := len(tr.wss); got != 4 {
		t.Errorf("каналов стало %d вместо 4 — комплект задвоился", got)
	}
}
