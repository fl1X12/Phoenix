package game

import (
	"regexp"
	"sync"

	"github.com/fl1X12/phoenix/internal/world"
)

var codeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)

// Lobby maps room codes to running rooms. A join on an unknown code creates the room.
type Lobby struct {
	mu    sync.Mutex
	rooms map[string]*Room
	world func() *world.World
}

func NewLobby(w func() *world.World) *Lobby {
	return &Lobby{rooms: map[string]*Room{}, world: w}
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
	l.rooms[code] = r
	go r.Run()
	return r
}

func (l *Lobby) remove(code string) {
	l.mu.Lock()
	delete(l.rooms, code)
	l.mu.Unlock()
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
