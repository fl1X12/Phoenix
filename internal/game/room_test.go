package game_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fl1X12/phoenix/internal/game"
	"github.com/fl1X12/phoenix/internal/world"
	"github.com/fl1X12/phoenix/internal/ws"
)

type client struct {
	t    *testing.T
	c    *websocket.Conn
	in   chan ws.Envelope
	side string
	tok  string
	// last world snapshot
	state map[string]any
}

func newClient(t *testing.T, c *websocket.Conn) *client {
	cl := &client{t: t, c: c, in: make(chan ws.Envelope, 64)}
	go func() {
		defer close(cl.in)
		for {
			var env ws.Envelope
			if err := c.ReadJSON(&env); err != nil {
				return
			}
			cl.in <- env
		}
	}()
	return cl
}

func (c *client) send(typ string, data any) {
	raw, _ := json.Marshal(data)
	if err := c.c.WriteJSON(ws.Envelope{Type: typ, Data: raw}); err != nil {
		c.t.Fatal(err)
	}
}

// expect reads messages until one of the given type arrives (or fails after 2s).
func (c *client) expect(typ string) map[string]any {
	c.t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		var env ws.Envelope
		var ok bool
		select {
		case env, ok = <-c.in:
			if !ok {
				c.t.Fatalf("[%s] closed while waiting for %s", c.side, typ)
			}
		case <-timeout:
			c.t.Fatalf("[%s] timeout waiting for %s", c.side, typ)
		}
		var d map[string]any
		_ = json.Unmarshal(env.Data, &d)
		c.t.Logf("[%s] <- %s %v", c.side, env.Type, d)
		if env.Type == typ {
			return d
		}
		if env.Type == "error" {
			c.t.Fatalf("[%s] got error while waiting for %s: %v", c.side, typ, d)
		}
	}
}

// expectFx reads until an fx with the given effect arrives.
func (c *client) expectFx(effect string) {
	c.t.Helper()
	for i := 0; i < 10; i++ {
		if fx := c.expect("fx"); fx["effect"] == effect {
			return
		}
	}
	c.t.Fatalf("[%s] never got fx %s", c.side, effect)
}

// expectNone asserts no message of type typ arrives within 200ms.
func (c *client) expectNone(typ string) {
	c.t.Helper()
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case env, ok := <-c.in:
			if !ok {
				return
			}
			if env.Type == typ {
				c.t.Fatalf("[%s] unexpected %s: %s", c.side, typ, env.Data)
			}
		case <-timeout:
			return
		}
	}
}

func setup(t *testing.T) (*client, *client, *game.Lobby) {
	t.Helper()
	w, err := world.Load("../../worlds/default")
	if err != nil {
		t.Fatal(err)
	}
	lobby := game.NewLobby(func() *world.World { return w })
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		sock, _ := up.Upgrade(rw, req, nil)
		c := ws.New(sock)
		var room *game.Room
		for {
			select {
			case env := <-c.Inbound:
				if room == nil {
					var d struct{ Code, Token string }
					_ = json.Unmarshal(env.Data, &d)
					room = lobby.Get(d.Code, true)
					room.Join(c, d.Token)
					continue
				}
				room.Message(c, env)
			case <-c.Closed:
				if room != nil {
					room.Leave(c)
				}
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	dial := func() *client {
		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return newClient(t, c)
	}
	a, b := dial(), dial()
	a.send("join", map[string]string{"code": "TEST"})
	as := a.expect("assigned")
	a.side, a.tok = as["side"].(string), as["token"].(string)
	a.expect("waiting")
	b.send("join", map[string]string{"code": "TEST"})
	bs := b.expect("assigned")
	b.side, b.tok = bs["side"].(string), bs["token"].(string)
	if a.side != "A" || b.side != "B" {
		t.Fatalf("sides: %s %s", a.side, b.side)
	}
	a.state = a.expect("world")["state"].(map[string]any)
	b.state = b.expect("world")["state"].(map[string]any)
	return a, b, lobby
}

func TestFullPlaythrough(t *testing.T) {
	a, b, _ := setup(t)

	// Visibility: A must not see B's keys, and neither sees the other's code.
	if _, ok := a.state["door_B1"]; ok {
		t.Fatal("A can see door_B1")
	}
	if _, ok := b.state["code_A1"]; ok {
		t.Fatal("B can see code_A1")
	}
	if _, ok := a.state["exit_open"]; !ok {
		t.Fatal("A cannot see global exit_open")
	}
	if a.state["A.lobby.lights"] != true || b.state["A.lobby.lights"] != true {
		t.Fatal("lights should default on")
	}

	// Button door: A presses, B gets patch + fx, A gets nothing.
	a.send("interact", map[string]string{"id": "button_A1", "action": "press"})
	if p := b.expect("patch"); p["door_B1"] != true {
		t.Fatalf("door_B1 patch: %v", p)
	}
	b.expect("fx")
	a.expectNone("patch")

	// Interacting with an object on the other side is rejected.
	a.send("interact", map[string]string{"id": "button_B1", "action": "press"})
	a.expect("error")

	// Code door: wrong code buzzes actor only; right code opens.
	codeA := a.state["code_A1"].(string)
	b.send("interact", map[string]string{"id": "keypad_B1", "action": "submit", "value": "0000"})
	b.expectFx("buzz")
	b.expectNone("patch")
	b.send("interact", map[string]string{"id": "keypad_B1", "action": "submit", "value": codeA})
	if p := b.expect("patch"); p["door_B2"] != true {
		t.Fatalf("door_B2: %v", p)
	}

	// Key door needs the key.
	a.send("interact", map[string]string{"id": "door_A3", "action": "use_key"})
	a.expectFx("missing_key")
	a.send("interact", map[string]string{"id": "key_A", "action": "pickup"})
	p := a.expect("patch")
	if p["key_A"] != "held" || len(p["inv_A"].([]any)) != 1 {
		t.Fatalf("pickup patch: %v", p)
	}
	a.send("interact", map[string]string{"id": "door_A3", "action": "use_key"})
	p = a.expect("patch")
	if p["door_A3"] != true || p["key_A"] != "used" || len(p["inv_A"].([]any)) != 0 {
		t.Fatalf("use_key patch: %v", p)
	}

	// Bomb wall on B.
	b.send("interact", map[string]string{"id": "bomb_B", "action": "pickup"})
	b.expect("patch")
	b.send("interact", map[string]string{"id": "wall_B1", "action": "use_bomb"})
	if p := b.expect("patch"); p["wall_B1"] != true || p["bomb_B"] != "used" {
		t.Fatalf("bomb patch: %v", p)
	}
	a.expectFx("explosion")

	// Exit is locked until both finals.
	a.send("enter", map[string]string{"portalId": "exit_A"})
	a.expectFx("locked")
	a.send("interact", map[string]string{"id": "final_A", "action": "press"})
	a.expect("patch")
	b.send("interact", map[string]string{"id": "final_B", "action": "press"})
	if p := a.expect("patch"); p["exit_open"] != true {
		t.Fatalf("A exit_open: %v", p)
	}
	if p := b.expect("patch"); p["exit_open"] != true || p["final_B"] != true {
		t.Fatalf("B exit_open: %v", p)
	}

	b.send("enter", map[string]string{"portalId": "exit_B"})
	a.expect("game_complete")
	b.expect("game_complete")
}

func TestReconnect(t *testing.T) {
	a, b, lobby := setup(t)
	a.send("interact", map[string]string{"id": "button_A1", "action": "press"})
	b.expect("patch")

	// B drops; A is told; B rejoins with token and gets a fresh world with the opened door.
	b.c.Close()
	if p := a.expect("partner"); p["connected"] != false {
		t.Fatalf("partner: %v", p)
	}
	nb := newClient(t, redial(t, a))
	nb.send("join", map[string]string{"code": "TEST", "token": b.tok})
	as := nb.expect("assigned")
	if as["side"] != "B" {
		t.Fatalf("reconnect side: %v", as)
	}
	w := nb.expect("world")
	if w["state"].(map[string]any)["door_B1"] != true {
		t.Fatal("reconnect world lost door_B1 state")
	}
	if p := a.expect("partner"); p["connected"] != true {
		t.Fatalf("partner back: %v", p)
	}
	// Third join on a full room is rejected.
	third := newClient(t, redial(t, a))
	third.send("join", map[string]string{"code": "TEST"})
	third.expect("error")
	if len(lobby.Codes()) != 1 {
		t.Fatalf("rooms: %v", lobby.Codes())
	}
}

// redial opens a new socket to the same test server as an existing client.
func redial(t *testing.T, ref *client) *websocket.Conn {
	t.Helper()
	url := "ws://" + ref.c.RemoteAddr().String() + "/ws"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
