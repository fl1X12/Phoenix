// Command server runs The Other Side game server.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fl1X12/phoenix/internal/game"
	"github.com/fl1X12/phoenix/internal/voice"
	"github.com/fl1X12/phoenix/internal/world"
	"github.com/fl1X12/phoenix/internal/ws"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true }, // Unity clients send no meaningful Origin
}

func main() {
	addr := flag.String("addr", "", "listen address (default $PORT or :8080)")
	worldDir := flag.String("world", "worlds", "a world directory (holds world.json), or a directory of world directories; each new room picks one at random")
	voiceLoop := flag.Bool("voice-loop", false, "echo a player's voice frames back to them while their partner has no voice connection (solo testing)")
	flag.Parse()
	if *addr == "" {
		if p := os.Getenv("PORT"); p != "" { // Render, Railway, Fly all set PORT
			*addr = ":" + p
		} else {
			*addr = ":8080"
		}
	}

	var current atomic.Pointer[[]*world.World]
	worlds, err := world.LoadAll(*worldDir)
	if err != nil {
		log.Fatalf("load worlds: %v", err)
	}
	current.Store(&worlds)
	for _, w := range worlds {
		log.Printf("world %q loaded: %d sides, %d rules, %d keys", w.Name, len(w.Sides), len(w.Rules), len(w.Visibility))
	}

	pick := func() *world.World {
		ws := *current.Load()
		return ws[rand.IntN(len(ws))]
	}
	lobby := game.NewLobby(pick)
	hub := voice.New(*voiceLoop)
	lobby.OnRoomClosed = hub.CloseRoom

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) { rw.Write([]byte("ok")) })
	// Keep-alive target for an external cron (Render free tier sleeps after 15 min idle).
	started := time.Now()
	mux.HandleFunc("GET /ping", func(rw http.ResponseWriter, req *http.Request) {
		ip := req.Header.Get("X-Forwarded-For") // Render's proxy sets this; RemoteAddr is the proxy
		if ip == "" {
			ip = req.RemoteAddr
		}
		log.Printf("ping from %s (%s), uptime %s, rooms %d", ip, req.UserAgent(), time.Since(started).Round(time.Second), len(lobby.Codes()))
		writeJSON(rw, map[string]any{
			"ok":     true,
			"uptime": time.Since(started).Round(time.Second).String(),
			"rooms":  len(lobby.Codes()),
			"voice":  hub.Stats(),
			"time":   time.Now().UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /ws", func(rw http.ResponseWriter, req *http.Request) { serveWS(lobby, rw, req) })
	mux.HandleFunc("GET /voice", func(rw http.ResponseWriter, req *http.Request) { serveVoice(lobby, hub, rw, req) })

	// Debug endpoints.
	mux.HandleFunc("GET /debug/rooms", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, lobby.Codes())
	})
	mux.HandleFunc("GET /debug/rooms/{code}", func(rw http.ResponseWriter, req *http.Request) {
		r := lobby.Get(req.PathValue("code"), false)
		if r == nil {
			http.Error(rw, "no such room", 404)
			return
		}
		writeJSON(rw, map[string]any{"room": r.Snapshot(), "voice": hub.Sides(r.Code)})
	})
	mux.HandleFunc("POST /debug/rooms/{code}/set", func(rw http.ResponseWriter, req *http.Request) {
		r := lobby.Get(req.PathValue("code"), false)
		if r == nil {
			http.Error(rw, "no such room", 404)
			return
		}
		var body map[string]any // {key: value, ...}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		for k, v := range body {
			r.SetKey(k, v)
		}
		writeJSON(rw, r.Snapshot().State)
	})
	mux.HandleFunc("POST /debug/rooms/{code}/item", func(rw http.ResponseWriter, req *http.Request) {
		r := lobby.Get(req.PathValue("code"), false)
		if r == nil {
			http.Error(rw, "no such room", 404)
			return
		}
		var body struct {
			Side string `json:"side"`
			Item string `json:"item"`
			Give bool   `json:"give"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		if err := r.GiveItem(body.Side, body.Item, body.Give); err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		writeJSON(rw, r.Snapshot().State)
	})
	mux.HandleFunc("GET /debug/worlds", func(rw http.ResponseWriter, _ *http.Request) {
		var names []string
		for _, w := range *current.Load() {
			names = append(names, w.Name)
		}
		writeJSON(rw, names)
	})
	mux.HandleFunc("POST /debug/reload", func(rw http.ResponseWriter, _ *http.Request) {
		nw, err := world.LoadAll(*worldDir)
		if err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		current.Store(&nw)
		fmt.Fprintf(rw, "reloaded %d world(s); applies to new rooms\n", len(nw))
	})

	log.Printf("listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// serveWS upgrades the connection, waits for the first "join", then hands the socket to the room.
func serveWS(lobby *game.Lobby, rw http.ResponseWriter, req *http.Request) {
	sock, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	c := ws.New(sock)

	var room *game.Room
	for {
		select {
		case env := <-c.Inbound:
			if room == nil {
				if env.Type != "join" {
					c.Send("error", map[string]string{"reason": "send join first"})
					continue
				}
				var d struct {
					Code  string `json:"code"`
					Token string `json:"token"`
				}
				if err := json.Unmarshal(env.Data, &d); err != nil || !game.ValidCode(d.Code) {
					c.Send("error", map[string]string{"reason": "bad room code"})
					continue
				}
				d.Code = strings.ToUpper(d.Code)
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
}

// serveVoice authenticates ?code=&token= against the lobby, upgrades, and hands the socket to the voice hub.
// Auth happens before the upgrade so a bad request gets a plain HTTP status.
func serveVoice(lobby *game.Lobby, hub *voice.Hub, rw http.ResponseWriter, req *http.Request) {
	code := strings.ToUpper(req.URL.Query().Get("code"))
	token := req.URL.Query().Get("token")
	if !game.ValidCode(code) || token == "" {
		http.Error(rw, "code and token required", http.StatusBadRequest)
		return
	}
	side, ok := lobby.SideForToken(code, token)
	if !ok {
		http.Error(rw, "unknown room or token", http.StatusUnauthorized)
		return
	}
	sock, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
		log.Printf("voice upgrade: %v", err)
		return
	}
	hub.Serve(sock, code, side)
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(rw)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
