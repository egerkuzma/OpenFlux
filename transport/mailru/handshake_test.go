package mailru

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// scriptedConn plays a fixed sequence of frames and then fails, which is what
// a real socket does when the provider closes it. Writes are kept so a test
// can see what the transport said.
type scriptedConn struct {
	mu       sync.Mutex
	incoming [][]byte
	written  [][]byte
	readErr  error
	deadline time.Time
	closed   bool
	blockFor time.Duration // held before the frames start arriving
}

func (c *scriptedConn) ReadMessage() (int, []byte, error) {
	if c.blockFor > 0 {
		time.Sleep(c.blockFor)
		c.blockFor = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, nil, errors.New("use of closed connection")
	}
	if len(c.incoming) == 0 {
		if c.readErr != nil {
			return 0, nil, c.readErr
		}
		return 0, nil, io.EOF
	}
	msg := c.incoming[0]
	c.incoming = c.incoming[1:]
	return websocket.TextMessage, msg, nil
}

func (c *scriptedConn) WriteMessage(_ int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("use of closed connection")
	}
	c.written = append(c.written, append([]byte(nil), data...))
	return nil
}

func (c *scriptedConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

func (c *scriptedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *scriptedConn) wrote() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.written...)
}

// Joining the namespace before the server has opened the session is what got
// sessions closed: nine of fourteen closures had the open packet as the last
// thing that ever arrived. So the wait has to actually wait for it, and has to
// keep handling ordinary traffic while it does.
func TestTheOpenPacketIsWaitedFor(t *testing.T) {
	tr := newTestTransport()
	got := make(chan []byte, 4)
	tr.Receive(func(b []byte) { got <- append([]byte(nil), b...) })

	conn := &scriptedConn{incoming: [][]byte{
		[]byte(`42["message",{"cursor":"u;0JDQkdCS"}]`), // обычный кадр до открытия
		[]byte(`0{"sid":"abc","upgrades":[],"pingInterval":25000}`),
	}}
	session := &DocSession{UserID: "u", Conn: conn}

	if !tr.awaitOpenPacket(session, conn) {
		t.Fatal("открывающий пакет не распознан")
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Error("кадр, пришедший до открытия, потерян вместо доставки")
	}
}

// A socket that dies while we wait must be reported, not waited on forever:
// the caller writes anyway and lets the read loop handle the failure through
// the one path that knows how.
func TestASilentSocketEndsTheOpenWait(t *testing.T) {
	tr := newTestTransport()
	conn := &scriptedConn{readErr: errors.New("i/o timeout")}

	done := make(chan bool, 1)
	go func() { done <- tr.awaitOpenPacket(&DocSession{Conn: conn}, conn) }()
	select {
	case ok := <-done:
		if ok {
			t.Error("мёртвый сокет выдан за открытый")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ожидание открывающего пакета не кончилось")
	}
}

// The deadline is the whole reason the wait terminates. Without it a silent
// provider holds the renewal past the moment the session gets cut.
func TestTheOpenWaitArmsItsDeadline(t *testing.T) {
	tr := newTestTransport()
	conn := &scriptedConn{incoming: [][]byte{[]byte(`0{"sid":"abc"}`)}}

	before := time.Now()
	tr.awaitOpenPacket(&DocSession{Conn: conn}, conn)

	conn.mu.Lock()
	d := conn.deadline
	conn.mu.Unlock()
	if d.IsZero() {
		t.Fatal("дедлайн чтения не выставлен — ожидание может не кончиться никогда")
	}
	if got := d.Sub(before); got < openPacketTimeout/2 || got > openPacketTimeout*2 {
		t.Errorf("дедлайн через %v, ожидалось около %v", got.Round(time.Millisecond), openPacketTimeout)
	}
}

func TestTheJoinAcknowledgementIsRecognised(t *testing.T) {
	tr := newTestTransport()
	conn := &scriptedConn{incoming: [][]byte{
		[]byte("2"), // ping по пути
		[]byte("40{\"sid\":\"xyz\"}"),
	}}
	session := &DocSession{UserID: "u", Conn: conn}

	if !tr.awaitJoinAck(session, conn) {
		t.Fatal("подтверждение входа в пространство имён не распознано")
	}
	// The ping on the way through must have been answered, or the provider
	// closes the session for going quiet.
	var pong bool
	for _, w := range conn.wrote() {
		if string(w) == "3" {
			pong = true
		}
	}
	if !pong {
		t.Error("на ping по пути не ответили pong")
	}
}

// An unanswered join is the failure the renewal has to notice: half of every
// renewal attempt used to end here, and each one costs five seconds against
// eleven seconds of margin.
func TestAnUnansweredJoinIsReported(t *testing.T) {
	tr := newTestTransport()
	conn := &scriptedConn{readErr: errors.New("i/o timeout")}

	if tr.awaitJoinAck(&DocSession{Conn: conn}, conn) {
		t.Error("молчание выдано за подтверждение входа")
	}
}

func TestTheJoinWaitArmsItsDeadline(t *testing.T) {
	tr := newTestTransport()
	conn := &scriptedConn{incoming: [][]byte{[]byte("40")}}

	before := time.Now()
	tr.awaitJoinAck(&DocSession{Conn: conn}, conn)

	conn.mu.Lock()
	d := conn.deadline
	conn.mu.Unlock()
	if got := d.Sub(before); got < joinAckTimeout/2 || got > joinAckTimeout*3 {
		t.Errorf("дедлайн через %v, ожидалось около %v", got.Round(time.Millisecond), joinAckTimeout)
	}
}

// Writes go through one mutex because two goroutines write to a session: the
// writer loop and whatever answers a ping. Interleaved frames on a websocket
// are not recoverable.
func TestWritesToASessionAreSerialised(t *testing.T) {
	conn := &scriptedConn{}
	s := &DocSession{Conn: conn}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.safeWrite(websocket.TextMessage, []byte("кадр"))
		}()
	}
	wg.Wait()

	if got := len(conn.wrote()); got != 50 {
		t.Errorf("записано %d кадров из 50", got)
	}
}

// A closed session reports the failure rather than pretending the frame went.
func TestWritingToAClosedSessionFails(t *testing.T) {
	conn := &scriptedConn{}
	s := &DocSession{Conn: conn}
	conn.Close()

	if err := s.safeWrite(websocket.TextMessage, []byte("кадр")); err == nil {
		t.Error("запись в закрытую сессию сошла за успешную")
	}
}
