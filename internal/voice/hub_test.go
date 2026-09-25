package voice

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// server exposes /voice?code=&side= straight into the hub; auth is the caller's job in production.
func server(t *testing.T, h *Hub) (dial func(code, side string) *websocket.Conn) {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		sock, err := up.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		h.Serve(sock, req.URL.Query().Get("code"), req.URL.Query().Get("side"))
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	return func(code, side string) *websocket.Conn {
		c, _, err := websocket.DefaultDialer.Dial(url+"?code="+code+"&side="+side, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
}

func frame(seq uint16, payload string) []byte {
	return append([]byte{Version, byte(seq >> 8), byte(seq)}, payload...)
}

func send(t *testing.T, c *websocket.Conn, b []byte) {
	t.Helper()
	if err := c.WriteMessage(websocket.BinaryMessage, b); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, b, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.BinaryMessage {
		t.Fatalf("got message type %d", typ)
	}
	return b
}

func recvNone(t *testing.T, c *websocket.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, b, err := c.ReadMessage(); err == nil {
		t.Fatalf("unexpected frame %v", b)
	}
}

func expectClose(t *testing.T, c *websocket.Conn, code int) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := c.ReadMessage()
	ce, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("want close %d, got %v", code, err)
	}
	if ce.Code != code {
		t.Fatalf("want close %d, got %d", code, ce.Code)
	}
}

// waitPeers blocks until the room has n registered peers, so a test does not send before the hub sees both sockets.
func waitPeers(t *testing.T, h *Hub, code string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.Stats()[code] == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("room %s never reached %d peers (have %d)", code, n, h.Stats()[code])
}

func TestForwardToOtherSide(t *testing.T) {
	h := New(false)
	dial := server(t, h)
	a, b := dial("R", "A"), dial("R", "B")
	waitPeers(t, h, "R", 2)

	send(t, a, frame(7, "hello"))
	if got := recv(t, b); string(got) != string(frame(7, "hello")) {
		t.Fatalf("B got %v", got)
	}
	send(t, b, frame(1, "back"))
	if got := recv(t, a); string(got[3:]) != "back" {
		t.Fatalf("A got %v", got)
	}
	recvNone(t, a) // A must not hear itself
}

func TestAloneDropsSilently(t *testing.T) {
	h := New(false)
	dial := server(t, h)
	a := dial("R", "A")
	waitPeers(t, h, "R", 1)
	for i := 0; i < 5; i++ {
		send(t, a, frame(uint16(i), "x"))
	}
	recvNone(t, a)
	if h.Stats()["R"] != 1 {
		t.Fatal("A should still be connected")
	}
}

func TestLoopbackWhenAlone(t *testing.T) {
	h := New(true)
	dial := server(t, h)
	a := dial("R", "A")
	waitPeers(t, h, "R", 1)
	send(t, a, frame(3, "me"))
	if got := recv(t, a); string(got[3:]) != "me" {
		t.Fatalf("A got %v", got)
	}

	// Partner arrives: loopback stops, frames go to partner.
	b := dial("R", "B")
	waitPeers(t, h, "R", 2)
	send(t, a, frame(4, "you"))
	if got := recv(t, b); string(got[3:]) != "you" {
		t.Fatalf("B got %v", got)
	}
	recvNone(t, a)
}

func TestBadVersion(t *testing.T) {
	h := New(false)
	dial := server(t, h)
	a := dial("R", "A")
	send(t, a, []byte{9, 0, 0, 1, 2, 3})
	expectClose(t, a, CloseBadVersion)
}

func TestBadFrameSize(t *testing.T) {
	h := New(false)
	dial := server(t, h)
	a := dial("R", "A")
	send(t, a, frame(0, "")) // header only, no payload
	expectClose(t, a, CloseBadFrame)

	b := dial("R", "B")
	send(t, b, frame(0, strings.Repeat("x", MaxPayload+1)))
	expectClose(t, b, CloseBadFrame)
}

func TestTextFrameRejected(t *testing.T) {
	h := New(false)
	dial := server(t, h)
	a := dial("R", "A")
	if err := a.WriteMessage(websocket.TextMessage, []byte(`{"type":"nope"}`)); err != nil {
		t.Fatal(err)
	}
	expectClose(t, a, websocket.CloseUnsupportedData)
}

func TestReconnectReplacesPeer(t *testing.T) {
	h := New(false)
	dial := server(t, h)
	a1 := dial("R", "A")
	waitPeers(t, h, "R", 1)
	a2 := dial("R", "A")
	expectClose(t, a1, CloseReplaced)
	waitPeers(t, h, "R", 1)

	b := dial("R", "B")
	waitPeers(t, h, "R", 2)
	send(t, b, frame(1, "hi"))
	if got := recv(t, a2); string(got[3:]) != "hi" {
		t.Fatalf("A2 got %v", got)
	}
}

func TestCloseRoom(t *testing.T) {
	h := New(false)
	dial := server(t, h)
	a, b := dial("R", "A"), dial("R", "B")
	waitPeers(t, h, "R", 2)
	h.CloseRoom("R")
	expectClose(t, a, websocket.CloseGoingAway)
	expectClose(t, b, websocket.CloseGoingAway)
	if n := h.Stats()["R"]; n != 0 {
		t.Fatalf("room still has %d peers", n)
	}
}

func TestRateLimit(t *testing.T) {
	h := New(false)
	dial := server(t, h)
	a := dial("R", "A")
	for i := 0; i < rateBurst*3; i++ {
		if err := a.WriteMessage(websocket.BinaryMessage, frame(uint16(i), "x")); err != nil {
			break // server may have closed already
		}
	}
	expectClose(t, a, CloseRateLimit)
}

func TestSlowReceiverDropsNotBlocks(t *testing.T) {
	h := New(false)
	dial := server(t, h)
	a, b := dial("R", "A"), dial("R", "B")
	waitPeers(t, h, "R", 2)
	// b never reads. Send well past the queue length at the allowed rate; a must not stall.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queueLen*4; i++ {
			send(t, a, frame(uint16(i), "x"))
			time.Sleep(12 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sender blocked on slow receiver")
	}
	_ = b
}
