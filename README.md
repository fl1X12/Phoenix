# Phoenix — The Other Side game server

Go WebSocket server for *The Other Side*, a two-player co-op puzzle game. The server owns the building's state;
each Unity client owns its own player. See `The Other Side — technical design.md` for the full design.

## Run

```sh
go run ./cmd/server -addr :8080 -world worlds/default
```

- `GET /healthz`
- `GET /ws` — game socket
- `GET /debug/rooms`, `GET /debug/rooms/{code}` — list rooms, dump a room's state
- `POST /debug/rooms/{code}/set` body `{"door_B1": true}` — set any keys
- `POST /debug/rooms/{code}/item` body `{"side":"A","item":"key","give":true}` — give or take an item
- `POST /debug/reload` — reload the world files for new rooms

Test: `go test -race ./...`

## Protocol

Plain WebSockets, JSON, one envelope for every message: `{ "type": "...", "data": { ... } }`.

| Direction | Type | Data | When |
| --- | --- | --- | --- |
| C→S | `join` | `{ code, token? }` | First message on the socket. `token` when reconnecting |
| C→S | `interact` | `{ id, action, value? }` | Using an object; `value` carries a keypad code |
| C→S | `enter` | `{ portalId }` | Walking through the exit door |
| C→S | `room` | `{ id }` | Crossing a room boundary |
| S→C | `assigned` | `{ side, token }` | Side assigned; keep the token for reconnects |
| S→C | `waiting` | `{}` | Partner not yet connected |
| S→C | `world` | `{ side, tileSize, camera, tiles, spawn, rooms, objects, colors, state }` | Game start or reconnect; this side only |
| S→C | `patch` | `{ key: value, … }` | Changed keys this player can see. Always sent **before** any `fx` from the same event |
| S→C | `fx` | `{ effect, at? }` | One-off cue; `at` is the source object id |
| S→C | `partner` | `{ connected }` | Partner dropped (slot held 2 min) or came back |
| S→C | `error` | `{ reason }` | Rejected message. Socket closes after `room full` / `unknown token` |
| S→C | `game_complete` | `{}` | A player crossed the open exit door |

Room codes are 1–16 chars of `[A-Za-z0-9_-]`, upper-cased by the server. First join creates the room.

### Object types and actions

| Type | Accepts | Key value | Notes |
| --- | --- | --- | --- |
| `button` | `press` | bool | Fires rules |
| `latch_button` | `press` | bool | Final button; rule sets its key true. Exactly one per side |
| `light_switch`, `laser_switch` | `press` | bool | Fires rules; key is the room/laser key it controls (for glow color) |
| `button_door`, `code_door`, `laser`, `fire` | — | bool | Drawn from key |
| `key_door` | `use_key` | bool | Rule with `requires.holding: key`, `consume: key` |
| `bombable_wall` | `use_bomb` | bool | Rule with `requires.holding: bomb`, `consume: bomb` |
| `keypad` | `submit` | bool | Key = the door it opens. Rule needs `requires.code`. Wrong code → `fx buzz` to actor |
| `code_panel` | — | string | 4-digit code generated per room, visible only to its side |
| `bomb`, `key` | `pickup` | `home`/`held`/`used` | Built in; updates `inv_<side>` |
| `exit_door` | — (use `enter`) | bool | Key is `exit_open`. Closed → `fx locked` |
| `boss` | — | — | Client-simulated; extra props pass through untouched |

Built-in fx sent to the actor on failure: `buzz` (wrong code), `missing_key` / `missing_bomb`, `locked`.

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

## Layout

```
cmd/server/        main, HTTP mux, /ws upgrade, debug endpoints
internal/world/    types, loader (json + ASCII maps), validation, visibility/colors
internal/game/     lobby, room goroutine, rules, inventory, codes, diff/patches
internal/ws/       socket wrapper: write queue, ping/pong, read deadline
worlds/default/    sample world
```

One goroutine per room owns all state; connections push events through a channel, so there are no locks.
