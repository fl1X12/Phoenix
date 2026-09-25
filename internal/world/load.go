package world

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Load reads world.json from dir, loads each side's ASCII map, validates, and derives visibility and colors.
func Load(dir string) (*World, error) {
	b, err := os.ReadFile(filepath.Join(dir, "world.json"))
	if err != nil {
		return nil, err
	}
	var w World
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("world.json: %w", err)
	}
	if w.Initial == nil {
		w.Initial = map[string]any{}
	}
	if w.Camera.Radius == 0 {
		w.Camera.Radius = 8
	}
	if w.Camera.DarkRadius == 0 {
		w.Camera.DarkRadius = 2
	}
	for name, s := range w.Sides {
		tiles, err := loadMap(filepath.Join(dir, s.Map))
		if err != nil {
			return nil, fmt.Errorf("side %s: %w", name, err)
		}
		s.Tiles = tiles
		if err := s.normaliseBoss(name); err != nil {
			return nil, fmt.Errorf("side %s: boss: %w", name, err)
		}
		for i := range s.Objects {
			o := &s.Objects[i]
			// A code panel may name its key as "code" instead of "key".
			if o.Type == "code_panel" && o.Key == "" {
				if c, ok := o.Props["code"].(string); ok {
					o.Key = c
				}
			}
		}
	}
	w.panels = map[string]string{}
	for side, s := range w.Sides {
		for _, o := range s.Objects {
			if o.Type == "code_panel" && o.PanelColor() != "" {
				w.panels[side+"/"+o.PanelColor()] = o.Key
			}
		}
	}
	if errs := w.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("world validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	w.derive()
	// Keypads learn their colour sequence from the code their submit rule requires,
	// so the client shows the order without it being authored twice.
	for _, s := range w.Sides {
		for i := range s.Objects {
			o := &s.Objects[i]
			if o.Type != "keypad" {
				continue
			}
			for _, r := range w.Rules {
				if r.On == o.ID+":submit" && r.Requires != nil {
					if spec, ok := w.Codes[r.Requires.Code]; ok {
						if o.Props == nil {
							o.Props = map[string]any{}
						}
						o.Props["order"] = spec.Order
					}
				}
			}
		}
	}
	w.Name = filepath.Base(filepath.Clean(dir))
	return &w, nil
}

// LoadAll loads every world under dir. If dir itself holds a world.json it is the only world;
// otherwise each subdirectory with a world.json is loaded. Every world must validate.
func LoadAll(dir string) ([]*World, error) {
	if _, err := os.Stat(filepath.Join(dir, "world.json")); err == nil {
		w, err := Load(dir)
		if err != nil {
			return nil, err
		}
		return []*World{w}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*World
	var errs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(sub, "world.json")); err != nil {
			continue
		}
		w, err := Load(sub)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		out = append(out, w)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%d world(s) failed to load:\n%s", len(errs), strings.Join(errs, "\n"))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no worlds found under %s", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func loadMap(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%s: empty map", path)
	}
	width := len(lines[0])
	for i, l := range lines {
		if len(l) != width {
			return nil, fmt.Errorf("%s: row %d has length %d, expected %d (ragged grid)", path, i+1, len(l), width)
		}
		for j, c := range l {
			if c != '#' && c != '.' {
				return nil, fmt.Errorf("%s: row %d col %d: unknown tile %q", path, i+1, j+1, c)
			}
		}
	}
	return lines, nil
}

// derive fills Visibility, Colors and DerivedOrd. Called after validation.
func (w *World) derive() {
	vis := map[string]map[string]bool{}
	add := func(key, side string) {
		if key == "" {
			return
		}
		if vis[key] == nil {
			vis[key] = map[string]bool{}
		}
		vis[key][side] = true
	}
	for side, s := range w.Sides {
		for _, o := range s.Objects {
			add(o.Key, side)
		}
		for _, r := range s.Rooms {
			add(r.Lights, side)
			add(r.Flooded, side)
		}
		add("inv_"+side, side)
	}
	// Derived keys are global: both sides see them.
	for k := range w.Derived {
		for side := range w.Sides {
			add(k, side)
		}
	}
	w.Visibility = map[string][]string{}
	for k, sides := range vis {
		var list []string
		for s := range sides {
			list = append(list, s)
		}
		sort.Strings(list)
		w.Visibility[k] = list
	}

	// Object colors: which side(s) the keys written by this object's rules are visible to.
	w.Colors = map[string]string{}
	for side, s := range w.Sides {
		for _, o := range s.Objects {
			affected := map[string]bool{}
			for _, r := range w.Rules {
				if !strings.HasPrefix(r.On, o.ID+":") {
					continue
				}
				for _, k := range r.writes() {
					for _, vs := range w.Visibility[k] {
						affected[vs] = true
					}
				}
			}
			// Items and item-consuming objects affect their own side.
			if IsItemType(o.Type) || o.Type == "key_door" || o.Type == "bombable_wall" {
				affected[side] = true
			}
			switch {
			case len(affected) == 0:
				// not interactable, or affects nothing: no color
			case len(affected) > 1:
				w.Colors[o.ID] = "both"
			default:
				for k := range affected {
					w.Colors[o.ID] = k
				}
			}
		}
	}

	w.DerivedOrd = make([]string, 0, len(w.Derived))
	for k := range w.Derived {
		w.DerivedOrd = append(w.DerivedOrd, k)
	}
	sort.Strings(w.DerivedOrd)
}

// writes lists the state keys a rule may change.
func (r Rule) writes() []string {
	var out []string
	for _, a := range r.Do {
		if a.Toggle != "" {
			out = append(out, a.Toggle)
		}
		for k := range a.Set {
			out = append(out, k)
		}
	}
	return out
}

// SideOf returns the side that owns object id, and the object.
func (w *World) SideOf(id string) (string, *Object) {
	for side, s := range w.Sides {
		for i := range s.Objects {
			if s.Objects[i].ID == id {
				return side, &s.Objects[i]
			}
		}
	}
	return "", nil
}

// FindByType returns the first object of the given type on a side.
func (w *World) FindByType(side, typ string) *Object {
	s := w.Sides[side]
	if s == nil {
		return nil
	}
	for i := range s.Objects {
		if s.Objects[i].Type == typ {
			return &s.Objects[i]
		}
	}
	return nil
}

// OtherSide returns the side that is not the given one (two-side worlds).
func (w *World) OtherSide(side string) string {
	for s := range w.Sides {
		if s != side {
			return s
		}
	}
	return ""
}

// normaliseBoss turns a side-level "boss" block into a boss object, which is the form the
// client spawns from. The block's first path point is the spawn tile unless x/y are given.
func (s *Side) normaliseBoss(side string) error {
	if len(s.Boss) == 0 {
		return nil
	}
	for _, o := range s.Objects {
		if o.Type == "boss" {
			return nil // already authored as an object
		}
	}
	var block map[string]any
	if err := json.Unmarshal(s.Boss, &block); err != nil {
		return err
	}
	o := Object{ID: "boss_" + side, Type: "boss", W: 1, H: 1, Props: map[string]any{}}
	if id, ok := block["id"].(string); ok && id != "" {
		o.ID = id
	}
	x, hasX := block["x"].(float64)
	y, hasY := block["y"].(float64)
	if hasX && hasY {
		o.X, o.Y = int(x), int(y)
	} else {
		path, _ := block["path"].([]any)
		if len(path) == 0 {
			path, _ = block["patrol"].([]any)
		}
		if len(path) == 0 {
			return fmt.Errorf("needs x/y or a non-empty path")
		}
		pt, _ := path[0].([]any)
		if len(pt) < 2 {
			return fmt.Errorf("path points must be [x, y]")
		}
		px, _ := pt[0].(float64)
		py, _ := pt[1].(float64)
		o.X, o.Y = int(px), int(py)
	}
	for k, v := range block {
		if k != "id" && k != "x" && k != "y" {
			o.Props[k] = v
		}
	}
	if _, ok := o.Props["patrol"]; !ok {
		if p, ok := block["path"]; ok {
			o.Props["patrol"] = p
		}
	}
	s.Objects = append(s.Objects, o)
	return nil
}
