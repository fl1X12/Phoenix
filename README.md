# Phoenix — The Other Side game server

Go WebSocket server for *The Other Side*, a two-player co-op puzzle game. The server owns the building's state;
each Unity client owns its own player. See `The Other Side — technical design.md` for the full design.

## Run

```sh
go run ./cmd/server -addr :8080 -world worlds
```

`-world` takes either one world directory (holding `world.json`) or a directory of them. Every world must
pass validation or the server refuses to start. A world's id is its directory name; `world.json` may add
`title`, `description` and `hidden` (loaded but left out of `/worlds` and random picks, still creatable by id).

- `GET /healthz`
- `GET /worlds` — `{ worlds: [{ id, title, description? }] }` for the create-room picker. Client shows a
  picker (plus a "Random" choice) only when there are two or more; with one it sends `create {}`
- `GET /ws` — game socket
- `GET /voice?code=&token=` — voice relay socket, see [Voice](#voice)
- `GET /ping` — keep-alive target for an external cron; logs the hit
- `GET /debug/worlds` — loaded world names
- `GET /debug/rooms`, `GET /debug/rooms/{code}` — list rooms, dump a room's state (includes which world it picked)
- `POST /debug/rooms/{code}/set` body `{"door_B1": true}` — set any keys
- `POST /debug/rooms/{code}/item` body `{"side":"A","item":"key","give":true}` — give or take an item
- `POST /debug/reload` — reload the world files for new rooms
- `-voice-loop` flag — echo a player's voice frames back to them while alone in the room (solo testing)

### Voice on its own machine

One binary, three modes (`-mode` flag or `MODE` env):

| Mode | Serves | Needs |
| --- | --- | --- |
| `all` (default) | game + voice relay in one process | nothing |
| `game` | game only; sends clients the voice URL in `assigned` | `VOICE_URL=wss://<voice-host>/voice`, `INTERNAL_SECRET=<random>` |
| `voice` | `/voice` relay only; verifies tokens against the game box | `GAME_URL=https://<game-host>`, same `INTERNAL_SECRET` |

On Render: two Web Services from the same repo, same build command, start commands `./server -mode game`
and `./server -mode voice`, env vars as above. Each needs its own keep-alive ping.

Test: `go test -race ./...`

## Protocol

Plain WebSockets, JSON, one envelope for every message: `{ "type": "...", "data": { ... } }`.

| Direction | Type | Data | When |
| --- | --- | --- | --- |
| C→S | `create` | `{ world? }` | First message on the socket: open a new room on world id `world` (omitted or `"random"` = random). The code comes back in `assigned`; an unknown id gets `error unknown world` and the socket stays open |
| C→S | `join` | `{ code, token? }` | First message on the socket: enter an existing room. `token` when reconnecting |
| C→S | `interact` | `{ id, action, value? }` | Using an object; `value` carries a keypad code |
| C→S | `enter` | `{ portalId }` | Walking through the exit door |
| C→S | `room` | `{ id }` | Crossing a room boundary |
| C→S | `log` | `{ msg }` | Client diagnostics; server prints `client CODE/SIDE: msg` (voice counters, mic info) |
| C→S | `leave` | `{}` | Leave for good: ends the game for both players and frees the room (also valid while waiting) |
| S→C | `assigned` | `{ code, side, token, world: { id, title }, voice? }` | Side dealt at random; `world` is what the creator picked, so the joiner can show it; show the code to your partner, keep the token for reconnects. `voice` = URL of a separate voice relay when one runs, else use `/voice` on the game host |
| S→C | `waiting` | `{}` | Partner not yet connected |
| S→C | `world` | `{ side, tileSize, camera, tiles, spawn, rooms, objects, colors, state }` | Game start or reconnect; this side only |
| S→C | `patch` | `{ key: value, … }` | Changed keys this player can see. Always sent **before** any `fx` from the same event |
| S→C | `fx` | `{ effect, at? }` | One-off cue; `at` is the source object id |
| S→C | `partner` | `{ connected }` | Partner dropped (slot held 2 min) or came back |
| S→C | `error` | `{ reason }` | Rejected message. Socket closes after `room full` / `unknown token` |
| S→C | `game_complete` | `{}` | A player crossed the open exit door |
| S→C | `game_over` | `{ reason }` | Game ended early: `partner_left` (they pressed leave) or `partner_timeout` (gone past the 2 min grace). Socket closes after |

Generated codes are 4 chars from `A-Z2-9` minus `I`/`O`. `join` accepts 1–16 chars of `[A-Za-z0-9_-]` (upper-cased) but only for a live room; `no such room` otherwise. Socket closes after `room full` / `unknown token`.

### Object types and actions

| Type | Accepts | Key value | Notes |
| --- | --- | --- | --- |
| `button` | `press` | bool | Fires rules |
| `switch`, `valve`, `light_switch`, `laser_switch` | `toggle` (or `press`) | bool | Fires rules. `"latch": true` = one-way: ignored once key is true |
| `final_button` | `press` | bool | Latched. Exactly one per side |
| `button_door`, `code_door`, `lasers`, `fire` | — | bool | Drawn from key. `w`/`h` span tiles (default 1x1) |
| `key_door` | `use_key` | bool | Rule with `requires.holding: key`, `consume: key` |
| `bombable_wall` | `use_bomb` | bool | Rule with `requires.holding: bomb`, `consume: bomb` |
| `keypad` | `submit` | bool | Key = the door it opens. Rule needs `requires.code`. Wrong code → `fx buzz` to actor |
| `code_panel` | — | string | With `"color"`: one random digit per room, four panels per side (red/green/blue/yellow). Legacy form without colour: `"code": "code_A1"` names the key and holds a whole 4-digit code |
| `bomb`, `key` | `pickup` | `home`/`held`/`used` | Built in; updates `inv_<side>` |
| `exit_door` | — (use `enter`) | bool | Key is `exit_open`. Closed → `fx locked` |

Unknown fields on objects, rooms (`name`, `theme`), plus side `name`/`boss` and top-level `debuff`, pass through
to the client in `world` untouched. Rooms use `"rects": [[x,y,w,h], ...]`; several rects make an L-shape.

**Colour codes** (see `docs/code-panels.md`): a `codes` block composes each keypad answer from the partner
side's panel digits read in a colour order. The keypad object is sent with that `order` so the client can show
the swatches; the composed answer key is never sent to anyone.

```json
"codes": { "code_A1": { "side": "A", "order": ["red", "green", "blue", "yellow"] } }
```

Built-in fx sent to the actor on failure: `buzz` (wrong code), `missing_key` / `missing_bomb`, `locked`.
`derivedFx: { "exit_open": "exit_open" }` sends an fx to both players whenever that derived key changes.

### State keys

| Kind | Example | Sent to |
| --- | --- | --- |
| Object | `door_B1` | Side whose objects read it |
| Room | `B.server.lights`, `A.archive.flooded` | Side the room is on |
| Inventory | `inv_B` = `["bomb"]` | That player only |
| Derived / global | `exit_open` | Both |

Visibility and glow colors are derived at load from which objects read which keys. Rules write keys;
a player only ever sees effects on their own side.

### Rule vocabulary

```json
{ "on": "keypad_B1:submit", "requires": { "code": "code_A1" },
  "do": [ { "set": { "door_B2": true } }, { "fx": "unlock", "to": "B" } ] }
```

`do` steps: `toggle`, `set`, `fx` (+ `to`: `A`, `B`, `all`, `actor`), `consume`, `call` (named Go function in `game.Calls`).
`requires`: `holding` (item type), `code` (code panel key).

## Voice

Players talk over a second socket, `GET /voice?code=ROOM&token=TOKEN`, using the token from `assigned`.
The server is a relay: every binary frame from one side is forwarded, unchanged, to the other side of the
same room. Nothing is decoded or stored. Auth is checked before the upgrade (400 bad request, 401 unknown
room/token). Client-side details: [docs/voice-client.md](docs/voice-client.md).

Frame (binary message): `[0]` version `1`, `[1..2]` uint16 big-endian sequence, `[3..]` Opus payload 1–480 bytes.

Rules: text frames close with 1003; bad version 4001; bad size 4002; over 100 frames/s 4003; a second
connection for the same side replaces the first (old one gets 4004); room end closes with 1001. A slow
receiver gets frames dropped, never queued beyond ~160 ms. Ping every 20 s, 30 s pong timeout.

## Layout

```
cmd/server/        main, HTTP mux, /ws upgrade, debug endpoints
internal/world/    types, loader (json + ASCII maps), validation, visibility/colors
internal/game/     lobby, room goroutine, rules, inventory, codes, diff/patches
internal/voice/    voice relay hub: per-room peers, forward-or-drop, rate limit
internal/ws/       socket wrapper: write queue, ping/pong, read deadline
worlds/<name>/     one world per directory; `default` is the live level
worlds/archive/    retired levels, not loaded (e.g. `-world worlds/archive/classic` to play one)
```

One goroutine per room owns all state; connections push events through a channel, so there are no locks.
