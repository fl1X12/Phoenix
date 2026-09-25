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
	return setupWorld(t, "../../worlds/default")
}

func setupWorld(t *testing.T, dir string) (*client, *client, *game.Lobby) {
	t.Helper()
	w, err := world.Load(dir)
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
	if _, ok := b.state["panel_A_red"]; ok {
		t.Fatal("B can see panel_A_red")
	}
	if _, ok := a.state["exit_open"]; !ok {
		t.Fatal("A cannot see global exit_open")
	}
	// initial overrides the room default (lights on).
	if b.state["B.server.lights"] != false {
		t.Fatalf("B.server.lights should start off, got %v", b.state["B.server.lights"])
	}

	// Button door: A presses, B gets patch, A only sees its own button key.
	a.send("interact", map[string]string{"id": "button_A1", "action": "press"})
	if p := b.expect("patch"); p["door_B1"] != true {
		t.Fatalf("door_B1 patch: %v", p)
	}
	if p := a.expect("patch"); p["button_A1.on"] != true || p["door_B1"] != nil {
		t.Fatalf("A patch: %v", p)
	}

	// Interacting with an object on the other side is rejected.
	a.send("interact", map[string]string{"id": "button_B1", "action": "press"})
	a.expect("error")

	// Light switch toggles the other side's room key.
	b.send("interact", map[string]string{"id": "light_B_server", "action": "toggle"})
	if p := b.expect("patch"); p["B.server.lights"] != true {
		t.Fatalf("lights patch: %v", p)
	}

	// Code door: wrong code buzzes actor only; right code (A's panels in keypad_B2's order) opens.
	codeA := a.state["panel_A_red"].(string) + a.state["panel_A_green"].(string) +
		a.state["panel_A_blue"].(string) + a.state["panel_A_yellow"].(string)
	wrongA := "0000"
	if codeA == wrongA {
		wrongA = "1111"
	}
	b.send("interact", map[string]string{"id": "keypad_B2", "action": "submit", "value": wrongA})
	b.expectFx("buzz")
	b.expectNone("patch")
	b.send("interact", map[string]string{"id": "keypad_B2", "action": "submit", "value": codeA})
	if p := b.expect("patch"); p["door_B2"] != true {
		t.Fatalf("door_B2: %v", p)
	}

	// Key door needs the key (B side).
	b.send("interact", map[string]string{"id": "keydoor_B", "action": "use_key"})
	b.expectFx("missing_key")
	b.send("interact", map[string]string{"id": "key_B", "action": "pickup"})
	p := b.expect("patch")
	if p["key_B"] != "held" || len(p["inv_B"].([]any)) != 1 {
		t.Fatalf("pickup patch: %v", p)
	}
	b.send("interact", map[string]string{"id": "keydoor_B", "action": "use_key"})
	p = b.expect("patch")
	if p["keydoor_B.open"] != true || p["key_B"] != "used" || len(p["inv_B"].([]any)) != 0 {
		t.Fatalf("use_key patch: %v", p)
	}

	// Bomb wall on A; B hears the explosion.
	a.send("interact", map[string]string{"id": "bomb_A", "action": "pickup"})
	a.expect("patch")
	a.send("interact", map[string]string{"id": "wall_A1", "action": "use_bomb"})
	if p := a.expect("patch"); p["wall_A1.broken"] != true || p["bomb_A"] != "used" {
		t.Fatalf("bomb patch: %v", p)
	}
	b.expectFx("explosion")

	// Latched switch: second toggle is ignored.
	a.send("interact", map[string]string{"id": "sprinkler_A", "action": "toggle"})
	if p := a.expect("patch"); p["sprinkler_A.on"] != true {
		t.Fatalf("sprinkler: %v", p)
	}
	if p := b.expect("patch"); p["fire_B1"] != false {
		t.Fatalf("fire: %v", p)
	}
	a.send("interact", map[string]string{"id": "sprinkler_A", "action": "toggle"})
	a.expectNone("patch")

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
	// derivedFx: both hear the exit open, after the patch.
	a.expectFx("exit_open")
	b.expectFx("exit_open")

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

func TestColourCodes(t *testing.T) {
	a, b, _ := setup(t)

	// A sees four single digits, nobody sees a composed code.
	for _, c := range []string{"red", "green", "blue", "yellow"} {
		d, _ := a.state["panel_A_"+c].(string)
		if len(d) != 1 || d[0] < '0' || d[0] > '9' {
			t.Fatalf("panel_A_%s = %q", c, d)
		}
		if _, ok := b.state["panel_A_"+c]; ok {
			t.Fatalf("B can see panel_A_%s", c)
		}
	}
	for _, k := range []string{"code_A1", "code_A2", "code_B1"} {
		if _, ok := a.state[k]; ok {
			t.Fatalf("A can see %s", k)
		}
		if _, ok := b.state[k]; ok {
			t.Fatalf("B can see %s", k)
		}
	}

	// keypad_B2 requires code_A1 = A's panels in red, green, blue, yellow.
	answer := a.state["panel_A_red"].(string) + a.state["panel_A_green"].(string) +
		a.state["panel_A_blue"].(string) + a.state["panel_A_yellow"].(string)
	wrong := answer[1:] + answer[:1]
	if wrong == answer {
		wrong = "0000"
		if answer == wrong {
			wrong = "1111"
		}
	}
	b.send("interact", map[string]string{"id": "keypad_B2", "action": "submit", "value": wrong})
	b.expectFx("buzz")
	b.send("interact", map[string]string{"id": "keypad_B2", "action": "submit", "value": answer})
	if p := b.expect("patch"); p["door_B2"] != true {
		t.Fatalf("door_B2: %v", p)
	}

	// keypad_B5 reads the same panels in a different order (yellow, blue, green, red).
	answer2 := a.state["panel_A_yellow"].(string) + a.state["panel_A_blue"].(string) +
		a.state["panel_A_green"].(string) + a.state["panel_A_red"].(string)
	b.send("interact", map[string]string{"id": "keypad_B5", "action": "submit", "value": answer2})
	if p := b.expect("patch"); p["door_B5"] != true {
		t.Fatalf("door_B5: %v", p)
	}
}
