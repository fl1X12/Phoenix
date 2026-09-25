package game

import (
	"crypto/rand"
	"regexp"
	"sync"

	"github.com/fl1X12/phoenix/internal/world"
)

var codeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)

// Generated codes avoid look-alikes (0/O, 1/I) so they survive being read out loud.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
const codeLen = 4

// Lobby maps room codes to running rooms. Create makes a room under a fresh code; Get looks one up
// (or, for tests and debug tooling, creates it under a chosen code).
type Lobby struct {
	mu     sync.Mutex
	rooms  map[string]*Room
	tokens map[string]map[string]string // code -> token -> side; mirrors each room's players
	world  func() *world.World

	// OnRoomClosed, if set, runs after a room is removed. Used to tear down voice peers.
	OnRoomClosed func(code string)
	// VoiceURL, if set, is handed to every room and sent to clients in "assigned"
	// (the public wss URL of a separate voice relay, e.g. wss://voice.example.com/voice).
	VoiceURL string
}

func NewLobby(w func() *world.World) *Lobby {
	return &Lobby{rooms: map[string]*Room{}, tokens: map[string]map[string]string{}, world: w}
}

func ValidCode(code string) bool { return codeRe.MatchString(code) }

// Get returns the room for code, creating and starting it if needed.
func (l *Lobby) Get(code string, create bool) *Room {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.rooms[code]; ok {
		return r
	}
	if !create {
		return nil
	}
	r := NewRoom(code, l.world(), l.remove)
	r.onToken = l.addToken
	r.VoiceURL = l.VoiceURL
	l.rooms[code] = r
	go r.Run()
	return r
}

// Create starts a room under a new random code that no live room is using.
func (l *Lobby) Create() *Room {
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		code := newRoomCode()
		if _, taken := l.rooms[code]; taken {
			continue
		}
		r := NewRoom(code, l.world(), l.remove)
		r.onToken = l.addToken
		r.VoiceURL = l.VoiceURL
		l.rooms[code] = r
		go r.Run()
		return r
	}
}

func newRoomCode() string {
	b := make([]byte, codeLen)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

func (l *Lobby) remove(code string) {
	l.mu.Lock()
	delete(l.rooms, code)
	delete(l.tokens, code)
	cb := l.OnRoomClosed
	l.mu.Unlock()
	if cb != nil {
		cb(code)
	}
}

func (l *Lobby) addToken(code, token, side string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tokens[code] == nil {
		l.tokens[code] = map[string]string{}
	}
	l.tokens[code][token] = side
}

// SideForToken resolves a player token to its side without touching the room goroutine,
// so it is safe to call from any HTTP handler even while the room is shutting down.
func (l *Lobby) SideForToken(code, token string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	side, ok := l.tokens[code][token]
	return side, ok
}

func (l *Lobby) Codes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.rooms))
	for c := range l.rooms {
		out = append(out, c)
	}
	return out
}
