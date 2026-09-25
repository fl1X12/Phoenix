// Package game runs one goroutine per room that owns all of that room's state.
package game

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	mrand "math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/fl1X12/phoenix/internal/world"
	"github.com/fl1X12/phoenix/internal/ws"
)

const GracePeriod = 2 * time.Minute

// Player is one side's slot. conn is nil while the player is disconnected.
type Player struct {
	Side  string
	Token string
	Room  string // last reported room id
	conn  *ws.Conn
	gone  *time.Timer
}

type eventKind int

const (
	evJoin eventKind = iota
	evMsg
	evLeave
	evGraceExpired
	evDebug
)

type event struct {
	kind  eventKind
	conn  *ws.Conn
	token string
	env   ws.Envelope
	side  string
	fn    func(*Room) // evDebug: runs on the room goroutine
}

// Room is one game session. All fields are owned by Run's goroutine.
type Room struct {
	Code    string
	world   *world.World
	state   map[string]any
	players map[string]*Player // side -> player
	inv     map[string]map[string]bool
	events  chan event
	started bool
	done    bool
	onClose func(code string)
	onToken func(code, token, side string) // called when a fresh player gets a token; may be nil
	// VoiceURL, when set, is sent in "assigned" so the client dials a separate voice relay.
	VoiceURL string
	pending  []pendingFx // cues queued during handle, sent after the patch
}

type pendingFx struct {
	to    string
	actor *Player
	msg   map[string]any
}

func NewRoom(code string, w *world.World, onClose func(string)) *Room {
	r := &Room{
		Code:    code,
		world:   w,
		state:   map[string]any{},
		players: map[string]*Player{},
		inv:     map[string]map[string]bool{},
		events:  make(chan event, 64),
		onClose: onClose,
	}
	r.reset()
	return r
}

// reset builds the initial state: initial keys, item homes, random codes, derived keys.
func (r *Room) reset() {
	r.state = map[string]any{}
	maps.Copy(r.state, r.world.Initial)
	// Rooms first so room defaults (lights on, not flooded) win over switches on the other side that read the same key.
	for _, s := range r.world.Sides {
		for _, rm := range s.Rooms {
			if rm.Lights != "" {
				if _, ok := r.state[rm.Lights]; !ok {
					r.state[rm.Lights] = true
				}
			}
			if rm.Flooded != "" {
				if _, ok := r.state[rm.Flooded]; !ok {
					r.state[rm.Flooded] = false
				}
			}
		}
	}
	for side, s := range r.world.Sides {
		r.inv[side] = map[string]bool{}
		for _, o := range s.Objects {
			switch o.Type {
			case "code_panel":
				if o.PanelColor() != "" {
					r.state[o.Key] = randomDigit()
				} else {
					r.state[o.Key] = randomCode() // legacy: one panel holds the whole code
				}
			case "bomb", "key":
				r.state[o.Key] = "home"
			case "boss":
			default:
				if _, ok := r.state[o.Key]; !ok && o.Key != "" {
					r.state[o.Key] = false
				}
			}
		}
		r.state["inv_"+side] = []string{}
	}
	// Composed codes: the partner's panel digits read in the keypad's colour order.
	// No object reads these keys, so they are never sent to anyone.
	for id, spec := range r.world.Codes {
		var sb strings.Builder
		for _, colour := range spec.Order {
			d, _ := r.state[r.world.PanelKey(spec.Side, colour)].(string)
			sb.WriteString(d)
		}
		r.state[id] = sb.String()
	}
	r.recomputeDerived()
}

// --- inbound API (any goroutine) ---

func (r *Room) Join(c *ws.Conn, token string) { r.events <- event{kind: evJoin, conn: c, token: token} }
func (r *Room) Leave(c *ws.Conn)              { r.events <- event{kind: evLeave, conn: c} }
func (r *Room) Message(c *ws.Conn, env ws.Envelope) {
	r.events <- event{kind: evMsg, conn: c, env: env}
}

// Do runs fn on the room goroutine and waits for it. Used by debug endpoints.
func (r *Room) Do(fn func(*Room)) {
	done := make(chan struct{})
	r.events <- event{kind: evDebug, fn: func(rm *Room) { fn(rm); close(done) }}
	<-done
}

// --- goroutine ---

func (r *Room) Run() {
	log.Printf("room %s: started with world %q", r.Code, r.world.Name)
	for ev := range r.events {
		before := maps.Clone(r.state)
		r.handle(ev)
		r.recomputeDerived()
		for k, effect := range r.world.DerivedFx {
			if !reflect.DeepEqual(before[k], r.state[k]) {
				r.fx("all", nil, effect, k)
			}
		}
		r.sendChanges(before)
		r.flushFx()
		if r.done {
			break
		}
	}
	for _, p := range r.players {
		if p.conn != nil {
			p.conn.CloseAfterSend()
		}
	}
	log.Printf("room %s: closed", r.Code)
	if r.onClose != nil {
		r.onClose(r.Code)
	}
}

func (r *Room) handle(ev event) {
	switch ev.kind {
	case evJoin:
		r.join(ev.conn, ev.token)
	case evLeave:
		r.leave(ev.conn)
	case evGraceExpired:
		if p := r.players[ev.side]; p != nil && p.conn == nil {
			log.Printf("room %s: side %s grace expired", r.Code, ev.side)
			r.done = true
		}
	case evDebug:
		ev.fn(r)
	case evMsg:
		p := r.playerOf(ev.conn)
		if p == nil {
			ev.conn.Send("error", errMsg("not joined"))
			return
		}
		r.message(p, ev.env)
	}
}

func (r *Room) playerOf(c *ws.Conn) *Player {
	for _, p := range r.players {
		if p.conn == c {
			return p
		}
	}
	return nil
}

func (r *Room) join(c *ws.Conn, token string) {
	// Reconnect by token.
	if token != "" {
		for _, p := range r.players {
			if p.Token == token {
				if p.conn != nil {
					c.Send("error", errMsg("slot already connected"))
					c.CloseAfterSend()
					return
				}
				if p.gone != nil {
					p.gone.Stop()
					p.gone = nil
				}
				p.conn = c
				c.Send("assigned", r.assigned(p))
				if r.started {
					r.sendWorld(p)
					r.tellOther(p, "partner", map[string]bool{"connected": true})
				} else {
					c.Send("waiting", struct{}{})
				}
				log.Printf("room %s: side %s reconnected", r.Code, p.Side)
				return
			}
		}
		c.Send("error", errMsg("unknown token"))
		c.CloseAfterSend()
		return
	}
	// Fresh join: a random free side, so who joined first says nothing about which side you get.
	var free []string
	for s := range r.world.Sides {
		if r.players[s] == nil {
			free = append(free, s)
		}
	}
	sort.Strings(free) // map order is random too, but shuffle from a stable base
	mrand.Shuffle(len(free), func(i, j int) { free[i], free[j] = free[j], free[i] })
	for _, s := range free {
		p := &Player{Side: s, Token: newToken(), conn: c}
		r.players[s] = p
		if r.onToken != nil {
			r.onToken(r.Code, p.Token, s)
		}
		c.Send("assigned", r.assigned(p))
		log.Printf("room %s: side %s joined", r.Code, s)
		if len(r.players) == len(r.world.Sides) {
			r.started = true
			for _, pl := range r.players {
				r.sendWorld(pl)
			}
			log.Printf("room %s: game started", r.Code)
		} else {
			c.Send("waiting", struct{}{})
		}
		return
	}
	c.Send("error", errMsg("room full"))
	c.CloseAfterSend()
}

func (r *Room) leave(c *ws.Conn) {
	p := r.playerOf(c)
	if p == nil {
		return
	}
	p.conn = nil
	log.Printf("room %s: side %s disconnected, holding slot %s", r.Code, p.Side, GracePeriod)
	r.tellOther(p, "partner", map[string]bool{"connected": false})
	side := p.Side
	p.gone = time.AfterFunc(GracePeriod, func() {
		r.events <- event{kind: evGraceExpired, side: side}
	})
}

func (r *Room) tellOther(p *Player, typ string, data any) {
	for _, o := range r.players {
		if o != p && o.conn != nil {
			o.conn.Send(typ, data)
		}
	}
}

func (r *Room) sendWorld(p *Player) {
	s := r.world.Sides[p.Side]
	colors := map[string]string{}
	for _, o := range s.Objects {
		if c, ok := r.world.Colors[o.ID]; ok {
			colors[o.ID] = c
		}
	}
	msg := map[string]any{
		"side":     p.Side,
		"name":     s.Name,
		"tileSize": r.world.TileSize,
		"camera":   r.world.Camera,
		"tiles":    s.Tiles,
		"spawn":    s.Spawn,
		"rooms":    s.Rooms,
		"objects":  s.Objects,
		"colors":   colors,
		"state":    r.visibleState(p.Side),
	}
	if len(s.Boss) > 0 {
		msg["boss"] = s.Boss
	}
	if len(r.world.Debuff) > 0 {
		msg["debuff"] = r.world.Debuff
	}
	p.conn.Send("world", msg)
}

func (r *Room) visibleState(side string) map[string]any {
	out := map[string]any{}
	for k, v := range r.state {
		if r.canSee(side, k) {
			out[k] = v
		}
	}
	return out
}

func (r *Room) canSee(side, key string) bool {
	for _, s := range r.world.Visibility[key] {
		if s == side {
			return true
		}
	}
	return false
}

// sendChanges diffs state against before and sends each player only the changed keys it can see.
func (r *Room) sendChanges(before map[string]any) {
	changed := map[string]any{}
	for k, v := range r.state {
		if old, ok := before[k]; !ok || !reflect.DeepEqual(old, v) {
			changed[k] = v
		}
	}
	for k := range before {
		if _, ok := r.state[k]; !ok {
			changed[k] = nil
		}
	}
	if len(changed) == 0 {
		return
	}
	for _, p := range r.players {
		if p.conn == nil {
			continue
		}
		patch := map[string]any{}
		for k, v := range changed {
			if r.canSee(p.Side, k) {
				patch[k] = v
			}
		}
		if len(patch) > 0 {
			p.conn.Send("patch", patch)
		}
	}
}

func (r *Room) recomputeDerived() {
	for _, k := range r.world.DerivedOrd {
		r.state[k] = world.Eval(r.world.Derived[k], r.state)
	}
}

// fx queues a cue; cues go out after the patch so clients apply state before playing the sound.
func (r *Room) fx(to string, actor *Player, effect string, at any) {
	msg := map[string]any{"effect": effect}
	if at != nil {
		msg["at"] = at
	}
	r.pending = append(r.pending, pendingFx{to: to, actor: actor, msg: msg})
}

func (r *Room) flushFx() {
	for _, f := range r.pending {
		for _, p := range r.players {
			if p.conn == nil {
				continue
			}
			if f.to == "all" || f.to == p.Side || ((f.to == "actor" || f.to == "") && p == f.actor) {
				p.conn.Send("fx", f.msg)
			}
		}
	}
	r.pending = r.pending[:0]
}

// --- state snapshot for debug ---

type Snapshot struct {
	Code    string            `json:"code"`
	World   string            `json:"world"`
	Started bool              `json:"started"`
	Players map[string]string `json:"players"` // side -> "connected" | "disconnected"
	State   map[string]any    `json:"state"`
}

func (r *Room) Snapshot() Snapshot {
	var s Snapshot
	r.Do(func(rm *Room) {
		s = Snapshot{Code: rm.Code, World: rm.world.Name, Started: rm.started, Players: map[string]string{}, State: maps.Clone(rm.state)}
		for side, p := range rm.players {
			if p.conn != nil {
				s.Players[side] = "connected"
			} else {
				s.Players[side] = "disconnected"
			}
		}
	})
	return s
}

// SetKey sets one state key from the debug endpoint.
func (r *Room) SetKey(key string, value any) {
	r.Do(func(rm *Room) { rm.state[key] = value })
}

// GiveItem puts an item into a player's inventory (or takes it away).
func (r *Room) GiveItem(side, item string, give bool) error {
	var err error
	r.Do(func(rm *Room) {
		if _, ok := rm.world.Sides[side]; !ok {
			err = fmt.Errorf("unknown side %q", side)
			return
		}
		if !world.IsItemType(item) {
			err = fmt.Errorf("unknown item %q", item)
			return
		}
		rm.inv[side][item] = give
		if !give {
			delete(rm.inv[side], item)
		}
		rm.syncInv(side)
		if o := rm.world.FindByType(side, item); o != nil {
			if give {
				rm.state[o.Key] = "held"
			} else {
				rm.state[o.Key] = "home"
			}
		}
	})
	return err
}

func (r *Room) assigned(p *Player) map[string]string {
	m := map[string]string{"code": r.Code, "side": p.Side, "token": p.Token}
	if r.VoiceURL != "" {
		m["voice"] = r.VoiceURL
	}
	return m
}

func errMsg(reason string) map[string]string { return map[string]string{"reason": reason} }

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomDigit() string {
	b := make([]byte, 1)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d", b[0]%10)
}

func randomCode() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d%d%d%d", b[0]%10, b[1]%10, b[2]%10, b[3]%10)
}

func decode(raw json.RawMessage, v any) bool {
	return json.Unmarshal(raw, v) == nil
}
