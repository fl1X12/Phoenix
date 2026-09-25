// Package world holds the data model for a level: tiles, rooms, objects and rules.
package world

import "encoding/json"

// Camera radii, in tiles.
type Camera struct {
	Radius     int `json:"radius"`
	DarkRadius int `json:"darkRadius"`
}

// Room is a rectangle in tile coordinates. An L-shaped room is two entries with the same ID.
type Room struct {
	ID      string `json:"id"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	Lights  string `json:"lights,omitempty"`  // state key; bool
	Flooded string `json:"flooded,omitempty"` // state key; bool
}

// Object is anything on the floor that can change or be used. It reads exactly one state key.
type Object struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	Key  string `json:"key,omitempty"`
	// Props carries type-specific extras (e.g. patrol path for the boss). Passed to the client untouched.
	Props map[string]any `json:"-"`
}

func (o *Object) UnmarshalJSON(b []byte) error {
	type plain Object
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var all map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, k := range []string{"id", "type", "x", "y", "key"} {
		delete(all, k)
	}
	*o = Object(p)
	if len(all) > 0 {
		o.Props = all
	}
	return nil
}

func (o Object) MarshalJSON() ([]byte, error) {
	m := map[string]any{"id": o.ID, "type": o.Type, "x": o.X, "y": o.Y}
	if o.Key != "" {
		m["key"] = o.Key
	}
	for k, v := range o.Props {
		m[k] = v
	}
	return json.Marshal(m)
}

// Side is one player's half of the floor.
type Side struct {
	Map     string   `json:"map"`
	Spawn   [2]int   `json:"spawn"`
	Rooms   []Room   `json:"rooms"`
	Objects []Object `json:"objects"`
	Tiles   []string `json:"tiles,omitempty"` // filled from Map at load
}

// Requires gates a rule on the acting player's situation.
type Requires struct {
	Holding string `json:"holding,omitempty"` // item type the player must hold
	Code    string `json:"code,omitempty"`    // state key of a code panel; submitted value must match
}

// Action is one step of a rule. Exactly one field is set.
type Action struct {
	Toggle  string         `json:"toggle,omitempty"`
	Set     map[string]any `json:"set,omitempty"`
	Fx      string         `json:"fx,omitempty"`
	To      string         `json:"to,omitempty"` // for fx: "A", "B", "all", or "actor"
	Consume string         `json:"consume,omitempty"`
	Call    string         `json:"call,omitempty"` // escape hatch: named Go function
}

// Rule fires on "<objectId>:<action>".
type Rule struct {
	On       string    `json:"on"`
	Requires *Requires `json:"requires,omitempty"`
	Do       []Action  `json:"do"`
}

// World is the whole level file.
type World struct {
	TileSize int               `json:"tileSize"`
	Camera   Camera            `json:"camera"`
	Sides    map[string]*Side  `json:"sides"`
	Initial  map[string]any    `json:"initial"`
	Derived  map[string]string `json:"derived"` // key -> boolean expression over keys
	Rules    []Rule            `json:"rules"`

	// Derived at load.
	Visibility map[string][]string `json:"-"` // state key -> sides that receive it
	Colors     map[string]string   `json:"-"` // object id -> "A", "B" or "both"
	DerivedOrd []string            `json:"-"` // fixed evaluation order for Derived
}

// Object types and the interact actions each accepts.
// Actions not listed here are ignored by the server.
var ObjectActions = map[string][]string{
	"button":        {"press"},
	"latch_button":  {"press"},
	"light_switch":  {"press"},
	"laser_switch":  {"press"},
	"button_door":   {},
	"code_door":     {},
	"key_door":      {"use_key"},
	"exit_door":     {},
	"laser":         {},
	"fire":          {},
	"bombable_wall": {"use_bomb"},
	"code_panel":    {},
	"keypad":        {"submit"},
	"bomb":          {"pickup"},
	"key":           {"pickup"},
	"boss":          {},
}

// Item types a player can carry. Each side has at most one object of each.
var ItemTypes = []string{"bomb", "key"}

// Types whose key must be cleared by some rule (never revert on their own).
var MustBeClearable = map[string]bool{"fire": true}

func IsItemType(t string) bool {
	for _, it := range ItemTypes {
		if it == t {
			return true
		}
	}
	return false
}
