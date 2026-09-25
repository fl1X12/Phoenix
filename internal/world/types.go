// Package world holds the data model for a level: tiles, rooms, objects and rules.
package world

import "encoding/json"

// Camera radii, in tiles.
type Camera struct {
	Radius     float64 `json:"radius"`
	DarkRadius float64 `json:"darkRadius"`
}

// Rect is x, y, w, h in tile coordinates.
type Rect [4]int

// Room is one or more rectangles sharing an id (an L-shape is two rects).
// Authored either as {x,y,w,h} or {rects:[[x,y,w,h],...]}. Extra fields (name, theme) pass through to the client.
type Room struct {
	ID      string `json:"id"`
	Rects   []Rect `json:"rects"`
	Lights  string `json:"lights,omitempty"`  // state key; bool
	Flooded string `json:"flooded,omitempty"` // state key; bool
	Props   map[string]any
}

func (r *Room) UnmarshalJSON(b []byte) error {
	var all map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	var p struct {
		ID      string `json:"id"`
		Rects   []Rect `json:"rects"`
		X, Y    int
		W, H    int
		Lights  string `json:"lights"`
		Flooded string `json:"flooded"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	r.ID, r.Rects, r.Lights, r.Flooded = p.ID, p.Rects, p.Lights, p.Flooded
	if len(r.Rects) == 0 && (p.W > 0 || p.H > 0) {
		r.Rects = []Rect{{p.X, p.Y, p.W, p.H}}
	}
	for _, k := range []string{"id", "rects", "x", "y", "w", "h", "lights", "flooded"} {
		delete(all, k)
	}
	if len(all) > 0 {
		r.Props = all
	}
	return nil
}

func (r Room) MarshalJSON() ([]byte, error) {
	m := map[string]any{"id": r.ID, "rects": r.Rects}
	if r.Lights != "" {
		m["lights"] = r.Lights
	}
	if r.Flooded != "" {
		m["flooded"] = r.Flooded
	}
	for k, v := range r.Props {
		m[k] = v
	}
	return json.Marshal(m)
}

// Object is anything on the floor that can change or be used. It reads exactly one state key.
// W and H default to 1; lasers and fire may span several tiles.
type Object struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w,omitempty"`
	H    int    `json:"h,omitempty"`
	Key  string `json:"key,omitempty"`
	// Props carries type-specific extras (label, latch, code, patrol path). Passed to the client untouched.
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
	for _, k := range []string{"id", "type", "x", "y", "w", "h", "key"} {
		delete(all, k)
	}
	*o = Object(p)
	if o.W == 0 {
		o.W = 1
	}
	if o.H == 0 {
		o.H = 1
	}
	if len(all) > 0 {
		o.Props = all
	}
	return nil
}

func (o Object) MarshalJSON() ([]byte, error) {
	m := map[string]any{"id": o.ID, "type": o.Type, "x": o.X, "y": o.Y, "w": o.W, "h": o.H}
	if o.Key != "" {
		m["key"] = o.Key
	}
	for k, v := range o.Props {
		m[k] = v
	}
	return json.Marshal(m)
}

// Latch reports whether the object is one-way: once its key is true, further interactions are ignored.
func (o *Object) Latch() bool {
	v, _ := o.Props["latch"].(bool)
	return v || o.Type == "final_button" || o.Type == "latch_button"
}

// Side is one player's half of the floor.
type Side struct {
	Name    string          `json:"name,omitempty"`
	Map     string          `json:"map"`
	Spawn   [2]int          `json:"spawn"`
	Rooms   []Room          `json:"rooms"`
	Objects []Object        `json:"objects"`
	Boss    json.RawMessage `json:"boss,omitempty"`  // client-simulated; passed through
	Tiles   []string        `json:"tiles,omitempty"` // filled from Map at load
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

// CodeSpec composes a keypad answer from one side's colour panels read in Order.
type CodeSpec struct {
	Side  string   `json:"side"`  // wing holding the panels
	Order []string `json:"order"` // colour sequence the keypad displays
}

// Panel colours; each side has either none or exactly one code_panel per colour.
var PanelColors = []string{"red", "green", "blue", "yellow"}

// World is the whole level file.
type World struct {
	Name      string              `json:"-"` // directory name, set at load
	TileSize  int                 `json:"tileSize"`
	Camera    Camera              `json:"camera"`
	Debuff    json.RawMessage     `json:"debuff,omitempty"` // boss debuff tuning; passed through
	Sides     map[string]*Side    `json:"sides"`
	Initial   map[string]any      `json:"initial"`
	Derived   map[string]string   `json:"derived"`         // key -> boolean expression over keys
	DerivedFx map[string]string   `json:"derivedFx"`       // derived key -> fx effect sent to all when it changes
	Codes     map[string]CodeSpec `json:"codes,omitempty"` // composed keypad answers
	Rules     []Rule              `json:"rules"`

	// Derived at load.
	Visibility map[string][]string `json:"-"` // state key -> sides that receive it
	Colors     map[string]string   `json:"-"` // object id -> "A", "B" or "both"
	DerivedOrd []string            `json:"-"` // fixed evaluation order for Derived
	panels     map[string]string   // "side/colour" -> panel state key
}

// PanelKey returns the state key of the code_panel with that colour on that side, or "".
func (w *World) PanelKey(side, colour string) string { return w.panels[side+"/"+colour] }

// PanelColor returns a code_panel's colour prop, or "".
func (o *Object) PanelColor() string {
	c, _ := o.Props["color"].(string)
	return c
}

// Object types and the interact actions each accepts.
// Actions not listed here are ignored by the server.
var ObjectActions = map[string][]string{
	"button":        {"press"},
	"switch":        {"toggle", "press"},
	"valve":         {"toggle", "press"},
	"light_switch":  {"toggle", "press"},
	"laser_switch":  {"toggle", "press"},
	"final_button":  {"press"},
	"latch_button":  {"press"}, // alias of final_button
	"button_door":   {},
	"code_door":     {},
	"key_door":      {"use_key"},
	"exit_door":     {},
	"lasers":        {},
	"laser":         {}, // alias of lasers
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

func IsFinalButton(t string) bool { return t == "final_button" || t == "latch_button" }
