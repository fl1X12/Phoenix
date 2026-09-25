// Package voice relays Opus frames between the two players of a room.
//
// The server never decodes audio. Each player opens a second websocket on
// /voice; every binary message it sends is forwarded, untouched, to the other
// side of the same room. Frames are dropped rather than queued when the
// receiver is slow, because late audio is worse than missing audio.
//
// Frame layout (binary websocket message):
//
//	byte 0     version, must be 1
//	byte 1-2   sequence number, uint16 big-endian, wraps; set by the sender
//	byte 3..   Opus payload, 1..480 bytes
package voice

import (
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	Version      = 1
	HeaderSize   = 3
	MaxPayload   = 480
	MaxFrame     = HeaderSize + MaxPayload
	queueLen     = 8 // ~160 ms of 20 ms frames
	writeWait    = 10 * time.Second
	pongWait     = 30 * time.Second
	pingPeriod   = 20 * time.Second
	rateLimit    = 100 // frames per second, sustained
	rateBurst    = 20
	closeGrace   = time.Second
	maxReadLimit = MaxFrame + 32
)

// Close codes sent to the client.
const (
	CloseBadVersion = 4001
	CloseBadFrame   = 4002
	CloseRateLimit  = 4003
	CloseReplaced   = 4004
)

// Hub owns every voice peer, grouped by room code.
type Hub struct {
	mu    sync.Mutex
	rooms map[string]map[string]*peer // code -> side -> peer
	loop  bool
}

type peer struct {
	code, side string
	ws         *websocket.Conn
	send       chan []byte
	closeCode  int
	closeOnce  sync.Once
	closed     chan struct{}

	// stats, read by the owner goroutine only
	in, out, dropped int
}

// New returns a hub. loop=true echoes a player's frames back to them when the
// partner is absent, so one client can test capture and playback alone.
func New(loop bool) *Hub {
	return &Hub{rooms: map[string]map[string]*peer{}, loop: loop}
}

// Serve registers the connection as side's voice peer for code, forwards its
// frames until it disconnects, then unregisters it. It blocks for the life of
// the socket. A second Serve for the same code+side replaces the first.
func (h *Hub) Serve(ws *websocket.Conn, code, side string) {
	p := &peer{code: code, side: side, ws: ws, send: make(chan []byte, queueLen), closed: make(chan struct{})}
	h.add(p)
	started := time.Now()
	log.Printf("voice %s/%s: connected", code, side)

	go p.writeLoop()
	h.readLoop(p)

	h.remove(p)
	p.close(websocket.CloseNormalClosure)
	log.Printf("voice %s/%s: gone after %s, in %d, out %d, dropped %d", code, side,
		time.Since(started).Round(time.Second), p.in, p.out, p.dropped)
}

// CloseRoom disconnects every voice peer of a room. Called when the game room ends.
func (h *Hub) CloseRoom(code string) {
	h.mu.Lock()
	peers := h.rooms[code]
	delete(h.rooms, code)
	h.mu.Unlock()
	for _, p := range peers {
		p.close(websocket.CloseGoingAway)
	}
}

// Stats returns the number of connected voice peers per room.
func (h *Hub) Stats() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int, len(h.rooms))
	for code, peers := range h.rooms {
		out[code] = len(peers)
	}
	return out
}

// Sides lists the sides with a live voice peer in a room.
func (h *Hub) Sides(code string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for side := range h.rooms[code] {
		out = append(out, side)
	}
	return out
}

func (h *Hub) add(p *peer) {
	h.mu.Lock()
	peers := h.rooms[p.code]
	if peers == nil {
		peers = map[string]*peer{}
		h.rooms[p.code] = peers
	}
	old := peers[p.side]
	peers[p.side] = p
	h.mu.Unlock()
	if old != nil {
		old.close(CloseReplaced)
	}
}

func (h *Hub) remove(p *peer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if peers := h.rooms[p.code]; peers != nil && peers[p.side] == p {
		delete(peers, p.side)
		if len(peers) == 0 {
			delete(h.rooms, p.code)
		}
	}
}

// target returns who should receive p's frames: the other side, or p itself in loop mode when alone.
func (h *Hub) target(p *peer) *peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	for side, o := range h.rooms[p.code] {
		if side != p.side {
			return o
		}
	}
	if h.loop {
		return p
	}
	return nil
}

func (h *Hub) readLoop(p *peer) {
	p.ws.SetReadLimit(maxReadLimit)
	_ = p.ws.SetReadDeadline(time.Now().Add(pongWait))
	p.ws.SetPongHandler(func(string) error { return p.ws.SetReadDeadline(time.Now().Add(pongWait)) })

	tokens := float64(rateBurst)
	last := time.Now()
	for {
		typ, msg, err := p.ws.ReadMessage()
		if err != nil {
			return
		}
		_ = p.ws.SetReadDeadline(time.Now().Add(pongWait))
		if typ != websocket.BinaryMessage {
			p.close(websocket.CloseUnsupportedData)
			return
		}
		if len(msg) <= HeaderSize || len(msg) > MaxFrame {
			p.close(CloseBadFrame)
			return
		}
		if msg[0] != Version {
			p.close(CloseBadVersion)
			return
		}

		now := time.Now()
		tokens += now.Sub(last).Seconds() * rateLimit
		if tokens > rateBurst {
			tokens = rateBurst
		}
		last = now
		if tokens < 1 {
			p.close(CloseRateLimit)
			return
		}
		tokens--

		p.in++
		t := h.target(p)
		if t == nil {
			p.dropped++
			continue
		}
		select {
		case t.send <- msg:
			p.out++
		default:
			p.dropped++
		}
	}
}

func (p *peer) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case b := <-p.send:
			_ = p.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := p.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
				p.close(websocket.CloseAbnormalClosure)
				return
			}
		case <-ticker.C:
			_ = p.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := p.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				p.close(websocket.CloseAbnormalClosure)
				return
			}
		case <-p.closed:
			_ = p.ws.SetWriteDeadline(time.Now().Add(closeGrace))
			_ = p.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(p.closeCode, ""))
			_ = p.ws.Close()
			return
		}
	}
}

// close asks the write loop to send a close frame with code and shut the socket. Idempotent.
func (p *peer) close(code int) {
	p.closeOnce.Do(func() {
		p.closeCode = code
		close(p.closed)
	})
}
