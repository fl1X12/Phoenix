// Command server runs The Other Side game server.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
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
	mode := flag.String("mode", envOr("MODE", "all"), "all: game + voice in one process; game: game only, clients are sent VOICE_URL for voice; voice: voice relay only, tokens checked against GAME_URL")
	flag.Parse()
	if *addr == "" {
		if p := os.Getenv("PORT"); p != "" { // Render, Railway, Fly all set PORT
			*addr = ":" + p
		} else {
			*addr = ":8080"
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) { rw.Write([]byte("ok")) })
	hub := voice.New(*voiceLoop)
	started := time.Now()

	switch *mode {
	case "all", "game":
		runGame(mux, hub, *mode == "all", *worldDir, started)
	case "voice":
		runVoice(mux, hub, started)
	default:
		log.Fatalf("unknown -mode %q", *mode)
	}

	log.Printf("mode %s, listening on %s", *mode, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runGame mounts the game server. withVoice also mounts the in-process voice relay; otherwise
// VOICE_URL is sent to clients and INTERNAL_SECRET guards the token check the voice box calls.
func runGame(mux *http.ServeMux, hub *voice.Hub, withVoice bool, worldDir string, started time.Time) {
	var current atomic.Pointer[[]*world.World]
	worlds, err := world.LoadAll(worldDir)
	if err != nil {
		log.Fatalf("load worlds: %v", err)
	}
	current.Store(&worlds)
	for _, w := range worlds {
		log.Printf("world %q loaded: %d sides, %d rules, %d keys", w.Name, len(w.Sides), len(w.Rules), len(w.Visibility))
	}

	// pick returns the world with the given id, or for "" / "random" a random non-hidden one.
	pick := func(id string) *world.World {
		all := *current.Load()
		if id == "" || id == "random" {
			pool := visibleWorlds(all)
			if len(pool) == 0 {
				pool = all
			}
			return pool[rand.IntN(len(pool))]
		}
		for _, w := range all {
			if w.Name == id {
				return w
			}
		}
		return nil
	}
	lobby := game.NewLobby(pick)
	if withVoice {
		lobby.OnRoomClosed = hub.CloseRoom
		mux.HandleFunc("GET /voice", func(rw http.ResponseWriter, req *http.Request) { serveVoice(lobby.SideForToken, hub, rw, req) })
	} else {
		lobby.VoiceURL = os.Getenv("VOICE_URL")
		if lobby.VoiceURL == "" {
			log.Printf("warning: -mode game without VOICE_URL; clients will have no voice")
		}
		// The voice box asks here whether a token belongs to a room.
		secret := os.Getenv("INTERNAL_SECRET")
		mux.HandleFunc("GET /internal/voice-auth", func(rw http.ResponseWriter, req *http.Request) {
			if secret != "" && req.Header.Get("X-Internal-Secret") != secret {
				http.Error(rw, "forbidden", http.StatusForbidden)
				return
			}
			code := strings.ToUpper(req.URL.Query().Get("code"))
			side, ok := lobby.SideForToken(code, req.URL.Query().Get("token"))
			if !ok {
				http.Error(rw, "unknown room or token", http.StatusUnauthorized)
				return
			}
			writeJSON(rw, map[string]string{"side": side})
		})
	}

	// Keep-alive target for an external cron (Render free tier sleeps after 15 min idle).
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
	// Public world list for the create-room picker. The client shows a picker only when there are two or more.
	mux.HandleFunc("GET /worlds", func(rw http.ResponseWriter, _ *http.Request) {
		type entry struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description,omitempty"`
		}
		out := []entry{}
		for _, w := range visibleWorlds(*current.Load()) {
			out = append(out, entry{w.Name, w.Title, w.Description})
		}
		writeJSON(rw, map[string]any{"worlds": out})
	})
	mux.HandleFunc("GET /debug/worlds", func(rw http.ResponseWriter, _ *http.Request) {
		var names []string
		for _, w := range *current.Load() {
			names = append(names, w.Name)
		}
		writeJSON(rw, names)
	})
	mux.HandleFunc("POST /debug/reload", func(rw http.ResponseWriter, _ *http.Request) {
		nw, err := world.LoadAll(worldDir)
		if err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		current.Store(&nw)
		fmt.Fprintf(rw, "reloaded %d world(s); applies to new rooms\n", len(nw))
	})
}

func visibleWorlds(all []*world.World) []*world.World {
	var out []*world.World
	for _, w := range all {
		if !w.Hidden {
			out = append(out, w)
		}
	}
	return out
}

// runVoice mounts only the voice relay. Tokens are verified against the game server at GAME_URL.
func runVoice(mux *http.ServeMux, hub *voice.Hub, started time.Time) {
	gameURL := strings.TrimRight(os.Getenv("GAME_URL"), "/")
	if gameURL == "" {
		log.Fatal("-mode voice needs GAME_URL (e.g. https://phoenix.onrender.com)")
	}
	secret := os.Getenv("INTERNAL_SECRET")
	client := &http.Client{Timeout: 5 * time.Second}
	auth := func(code, token string) (string, bool) {
		req, _ := http.NewRequest("GET", gameURL+"/internal/voice-auth?code="+url.QueryEscape(code)+"&token="+url.QueryEscape(token), nil)
		if secret != "" {
			req.Header.Set("X-Internal-Secret", secret)
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("voice-auth: %v", err)
			return "", false
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", false
		}
		var d struct {
			Side string `json:"side"`
		}
		if json.NewDecoder(resp.Body).Decode(&d) != nil || d.Side == "" {
			return "", false
		}
		return d.Side, true
	}
	mux.HandleFunc("GET /voice", func(rw http.ResponseWriter, req *http.Request) { serveVoice(auth, hub, rw, req) })
	mux.HandleFunc("GET /ping", func(rw http.ResponseWriter, req *http.Request) {
		log.Printf("ping (%s), uptime %s, voice %v", req.UserAgent(), time.Since(started).Round(time.Second), hub.Stats())
		writeJSON(rw, map[string]any{
			"ok":     true,
			"uptime": time.Since(started).Round(time.Second).String(),
			"voice":  hub.Stats(),
			"time":   time.Now().UTC().Format(time.RFC3339),
		})
	})
}

// serveWS upgrades the connection, waits for the first "create" or "join", then hands the socket to the room.
// "create" makes a room under a server-generated code (returned in "assigned"); "join" needs a live code.
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
				switch env.Type {
				case "create":
					var d struct {
						World string `json:"world"`
					}
					_ = json.Unmarshal(env.Data, &d) // empty or missing data means a random world
					if room = lobby.Create(d.World); room == nil {
						c.Send("error", map[string]string{"reason": "unknown world"})
						continue
					}
					room.Join(c, "")
				case "join":
					var d struct {
						Code  string `json:"code"`
						Token string `json:"token"`
					}
					if err := json.Unmarshal(env.Data, &d); err != nil || !game.ValidCode(d.Code) {
						c.Send("error", map[string]string{"reason": "bad room code"})
						continue
					}
					d.Code = strings.ToUpper(d.Code)
					if room = lobby.Get(d.Code, false); room == nil {
						c.Send("error", map[string]string{"reason": "no such room"})
						continue
					}
					room.Join(c, d.Token)
				default:
					c.Send("error", map[string]string{"reason": "send create or join first"})
				}
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

// serveVoice authenticates ?code=&token= with auth, upgrades, and hands the socket to the voice hub.
// Auth happens before the upgrade so a bad request gets a plain HTTP status.
func serveVoice(auth func(code, token string) (string, bool), hub *voice.Hub, rw http.ResponseWriter, req *http.Request) {
	code := strings.ToUpper(req.URL.Query().Get("code"))
	token := req.URL.Query().Get("token")
	if !game.ValidCode(code) || token == "" {
		http.Error(rw, "code and token required", http.StatusBadRequest)
		return
	}
	side, ok := auth(code, token)
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
