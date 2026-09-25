package world

import (
	"fmt"
	"strings"
)

// Validate returns every problem it finds; an empty slice means the world is loadable.
func (w *World) Validate() []string {
	var errs []string
	e := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if len(w.Sides) != 2 {
		e("world must have exactly 2 sides, has %d", len(w.Sides))
	}

	readers := map[string]bool{} // keys read by some object, room, or derived expression
	objByID := map[string]*Object{}
	objSide := map[string]string{}
	for k := range w.Derived {
		readers[k] = true
	}

	for side, s := range w.Sides {
		h := len(s.Tiles)
		wdt := 0
		if h > 0 {
			wdt = len(s.Tiles[0])
		}
		inside := func(x, y int) bool { return x >= 0 && y >= 0 && x < wdt && y < h }
		if !inside(s.Spawn[0], s.Spawn[1]) {
			e("side %s: spawn %v outside map", side, s.Spawn)
		}
		for _, r := range s.Rooms {
			if len(r.Rects) == 0 {
				e("side %s: room %q has no rectangles", side, r.ID)
			}
			for _, rc := range r.Rects {
				x, y, rw, rh := rc[0], rc[1], rc[2], rc[3]
				if rw <= 0 || rh <= 0 || !inside(x, y) || !inside(x+rw-1, y+rh-1) {
					e("side %s: room %q rectangle %v outside map (%dx%d)", side, r.ID, rc, wdt, h)
				}
			}
			readers[r.Lights] = true
			readers[r.Flooded] = true
		}
		counts := map[string]int{}
		for i := range s.Objects {
			o := &s.Objects[i]
			if o.ID == "" {
				e("side %s: object #%d has no id", side, i)
				continue
			}
			if _, dup := objByID[o.ID]; dup {
				e("duplicate object id %q", o.ID)
			}
			objByID[o.ID] = o
			objSide[o.ID] = side
			if _, ok := ObjectActions[o.Type]; !ok {
				e("object %q: unknown type %q", o.ID, o.Type)
			}
			if !inside(o.X, o.Y) || !inside(o.X+o.W-1, o.Y+o.H-1) {
				e("object %q at (%d,%d) size %dx%d outside map", o.ID, o.X, o.Y, o.W, o.H)
			}
			if o.Key == "" && o.Type != "boss" {
				e("object %q (%s) needs a key", o.ID, o.Type)
			}
			readers[o.Key] = true
			if IsFinalButton(o.Type) {
				counts["final"]++
			} else {
				counts[o.Type]++
			}
		}
		for _, it := range ItemTypes {
			if counts[it] > 1 {
				e("side %s has %d %s objects, max 1", side, counts[it], it)
			}
		}
		if counts["bomb"] != counts["bombable_wall"] {
			e("side %s: %d bomb(s) but %d bombable wall(s)", side, counts["bomb"], counts["bombable_wall"])
		}
		if counts["key"] != counts["key_door"] {
			e("side %s: %d key(s) but %d key door(s)", side, counts["key"], counts["key_door"])
		}
		if counts["final"] != 1 {
			e("side %s: needs exactly 1 final_button, has %d", side, counts["final"])
		}
		if counts["exit_door"] != 1 {
			e("side %s: needs exactly 1 exit_door, has %d", side, counts["exit_door"])
		}
	}
	delete(readers, "")

	// Derived expressions reference known keys.
	for k, expr := range w.Derived {
		for _, tok := range exprTokens(expr) {
			if !readers[tok] && !w.hasInitial(tok) && !w.isWritten(tok) {
				e("derived %q references unknown key %q", k, tok)
			}
		}
	}

	// Rules.
	cleared := map[string]bool{}
	for i, r := range w.Rules {
		id, action, ok := strings.Cut(r.On, ":")
		if !ok {
			e("rule #%d: on=%q must be <objectId>:<action>", i, r.On)
			continue
		}
		o := objByID[id]
		if o == nil {
			e("rule #%d: unknown object %q", i, id)
			continue
		}
		if !contains(ObjectActions[o.Type], action) {
			e("rule #%d: object %q (%s) does not accept action %q", i, id, o.Type, action)
		}
		if r.Requires != nil {
			if r.Requires.Holding != "" && !IsItemType(r.Requires.Holding) {
				e("rule #%d: requires.holding %q is not an item type", i, r.Requires.Holding)
			}
			if r.Requires.Code != "" {
				panel := w.objectByKey(r.Requires.Code, "code_panel")
				if panel == nil {
					e("rule #%d: requires.code %q is not the key of any code_panel", i, r.Requires.Code)
				} else if objSide[panel.ID] == objSide[id] {
					e("rule #%d: keypad %q and code panel %q are on the same side", i, id, panel.ID)
				}
			}
		}
		for _, a := range r.Do {
			set := 0
			if a.Toggle != "" {
				set++
			}
			if len(a.Set) > 0 {
				set++
			}
			if a.Fx != "" {
				set++
			}
			if a.Consume != "" {
				set++
			}
			if a.Call != "" {
				set++
			}
			if set != 1 {
				e("rule #%d (%s): each action must set exactly one of toggle/set/fx/consume/call", i, r.On)
			}
			if a.Fx != "" && a.To != "A" && a.To != "B" && a.To != "all" && a.To != "actor" && a.To != "" {
				e("rule #%d (%s): fx.to must be A, B, all or actor", i, r.On)
			}
			if a.Consume != "" && !IsItemType(a.Consume) {
				e("rule #%d (%s): consume %q is not an item type", i, r.On, a.Consume)
			}
		}
		for _, k := range r.writes() {
			if !readers[k] {
				e("rule #%d (%s) writes key %q that nothing reads (typo?)", i, r.On, k)
			}
			for _, a := range r.Do {
				if a.Toggle == k {
					cleared[k] = true
				}
				if v, ok := a.Set[k]; ok && v == false {
					cleared[k] = true
				}
			}
		}
	}

	// Fire keys and flooded rooms must be clearable.
	for side, s := range w.Sides {
		for _, o := range s.Objects {
			if MustBeClearable[o.Type] && !cleared[o.Key] {
				e("side %s: %s %q key %q is never cleared by any rule", side, o.Type, o.ID, o.Key)
			}
		}
		for _, r := range s.Rooms {
			if r.Flooded != "" && !cleared[r.Flooded] {
				e("side %s: room %q flooded key %q is never drained by any rule", side, r.ID, r.Flooded)
			}
		}
	}
	// Every keypad must have a rule with requires.code.
	for _, o := range objByID {
		if o.Type == "keypad" {
			found := false
			for _, r := range w.Rules {
				if r.On == o.ID+":submit" && r.Requires != nil && r.Requires.Code != "" {
					found = true
				}
			}
			if !found {
				e("keypad %q has no submit rule with requires.code", o.ID)
			}
		}
	}
	return errs
}

func (w *World) hasInitial(k string) bool { _, ok := w.Initial[k]; return ok }

func (w *World) isWritten(k string) bool {
	for _, r := range w.Rules {
		for _, wk := range r.writes() {
			if wk == k {
				return true
			}
		}
	}
	return false
}

func (w *World) objectByKey(key, typ string) *Object {
	for _, s := range w.Sides {
		for i := range s.Objects {
			if s.Objects[i].Key == key && (typ == "" || s.Objects[i].Type == typ) {
				return &s.Objects[i]
			}
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
