// Package ws wraps a gorilla websocket with a write queue and JSON envelopes.
package ws

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 30 * time.Second
	pingPeriod = 20 * time.Second
	maxMsgSize = 64 * 1024
)

// Envelope is the single message shape in both directions.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Conn is one client socket. Send is safe from any goroutine; Inbound is read by the owner.
type Conn struct {
	ws      *websocket.Conn
	send    chan []byte
	Inbound chan Envelope
	Closed  chan struct{}
	once    bool
}

func New(c *websocket.Conn) *Conn {
	conn := &Conn{
		ws:      c,
		send:    make(chan []byte, 64),
		Inbound: make(chan Envelope, 32),
		Closed:  make(chan struct{}),
	}
	go conn.readLoop()
	go conn.writeLoop()
	return conn
}

// Send marshals data into an envelope and queues it. Drops the message if the queue is full.
func (c *Conn) Send(typ string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		log.Printf("ws: marshal %s: %v", typ, err)
		return
	}
	b, _ := json.Marshal(Envelope{Type: typ, Data: raw})
	select {
	case c.send <- b:
	default:
		log.Printf("ws: send queue full, dropping %s", typ)
	}
}

func (c *Conn) Close() {
	_ = c.ws.Close()
}

// CloseAfterSend closes the socket once everything already queued has been written.
func (c *Conn) CloseAfterSend() {
	select {
	case c.send <- nil:
	default:
		_ = c.ws.Close()
	}
}

func (c *Conn) readLoop() {
	defer func() {
		close(c.Closed)
		_ = c.ws.Close()
	}()
	c.ws.SetReadLimit(maxMsgSize)
	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		_, msg, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
		var env Envelope
		if err := json.Unmarshal(msg, &env); err != nil || env.Type == "" {
			c.Send("error", map[string]string{"reason": "bad envelope"})
			continue
		}
		select {
		case c.Inbound <- env:
		case <-c.Closed:
			return
		}
	}
}

func (c *Conn) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case b := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if b == nil {
				_ = c.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				_ = c.ws.Close()
				return
			}
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.Closed:
			return
		}
	}
}
