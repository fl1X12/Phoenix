package world

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped level must load; content assertions are on the testdata fixtures.
func TestDefaultWorldLoads(t *testing.T) {
	w, err := Load("../../worlds/default")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Visibility["exit_open"]; len(got) != 2 {
		t.Fatalf("exit_open visibility %v", got)
	}
	for id, c := range w.Colors {
		if c != "A" && c != "B" && c != "both" {
			t.Fatalf("object %s has colour %q", id, c)
		}
	}
}

func TestFixtureWorld(t *testing.T) {
	w, err := Load("testdata/colour")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Visibility["door_B1"]; len(got) != 1 || got[0] != "B" {
		t.Fatalf("door_B1 visibility %v", got)
	}
	if got := w.Visibility["panel_A_red"]; len(got) != 1 || got[0] != "A" {
		t.Fatalf("panel_A_red visibility %v", got)
	}
	if _, ok := w.Visibility["code_A1"]; ok {
		t.Fatal("composed code_A1 must not be visible")
	}
	// button_A1 sets its own key (A) and door_B1 (B) -> both.
	if w.Colors["button_A1"] != "both" || w.Colors["light_B_server"] != "B" || w.Colors["key_B"] != "B" {
		t.Fatalf("colors %v", w.Colors)
	}
	if _, ok := w.Colors["door_A1"]; ok {
		t.Fatal("door should have no color")
	}
	if _, o := w.SideOf("lasers_A1"); o == nil || o.H != 7 {
		t.Fatalf("lasers_A1 should be 1x7: %+v", o)
	}
}

// mutate loads the default world.json, applies fn, writes it to a temp dir with the maps, and loads it.
func mutate(t *testing.T, fn func(m map[string]any)) error {
	t.Helper()
	return mutateDir(t, "testdata/colour", fn)
}

func mutateDir(t *testing.T, src string, fn func(m map[string]any)) error {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(src, "world.json"))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	fn(m)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "maps"), 0o755)
	for _, f := range []string{"side_a.txt", "side_b.txt"} {
		mb, _ := os.ReadFile(filepath.Join(src, "maps", f))
		_ = os.WriteFile(filepath.Join(dir, "maps", f), mb, 0o644)
	}
	out, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(dir, "world.json"), out, 0o644)
	_, err := Load(dir)
	return err
}

func TestValidationCatches(t *testing.T) {
	rule := func(m map[string]any, on string) map[string]any {
		for _, r := range m["rules"].([]any) {
			if rm := r.(map[string]any); rm["on"] == on {
				return rm
			}
		}
		t.Fatalf("no rule %s", on)
		return nil
	}
	dropRule := func(m map[string]any, on string) {
		var keep []any
		for _, r := range m["rules"].([]any) {
			if r.(map[string]any)["on"] != on {
				keep = append(keep, r)
			}
		}
		m["rules"] = keep
	}
	cases := map[string]struct {
		fn   func(m map[string]any)
		want string
	}{
		"typo key": {func(m map[string]any) {
			rule(m, "button_A1:press")["do"] = []any{map[string]any{"toggle": "door_b1"}}
		}, "nothing reads"},
		"fire never cleared": {func(m map[string]any) {
			dropRule(m, "sprinkler_A:toggle")
		}, "never cleared"},
		"flood never drained": {func(m map[string]any) {
			dropRule(m, "valve_B:toggle")
		}, "never drained"},
		"keypad reads own side": {func(m map[string]any) {
			rule(m, "keypad_B2:submit")["requires"] = map[string]any{"code": "code_B1"}
		}, "own side"},
		"unknown action": {func(m map[string]any) {
			rule(m, "button_A1:press")["on"] = "door_A1:press"
		}, "does not accept"},
		"room outside map": {func(m map[string]any) {
			rooms := m["sides"].(map[string]any)["A"].(map[string]any)["rooms"].([]any)
			rooms[0].(map[string]any)["rects"] = []any{[]any{30.0, 1.0, 20.0, 5.0}}
		}, "outside map"},
	}
	for name, tc := range cases {
		err := mutate(t, tc.fn)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestRaggedMapRejected(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "m.txt"), []byte("####\n#..#\n###\n"), 0o644)
	if _, err := loadMap(filepath.Join(dir, "m.txt")); err == nil || !strings.Contains(err.Error(), "ragged") {
		t.Fatalf("want ragged error, got %v", err)
	}
}

func TestEval(t *testing.T) {
	st := map[string]any{"a": true, "b": false}
	for expr, want := range map[string]bool{
		"a && b": false, "a || b": true, "!b": true, "a && !b": true, "b || b && a": false, "missing": false,
	} {
		if got := Eval(expr, st); got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
}

func TestColourPanels(t *testing.T) {
	w, err := Load("testdata/colour")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.PanelKey("A", "blue"); got != "panel_A_blue" {
		t.Fatalf("PanelKey = %q", got)
	}
	_, kp := w.SideOf("keypad_B2")
	order, _ := kp.Props["order"].([]string)
	if len(order) != 4 || order[0] != "red" || order[3] != "yellow" {
		t.Fatalf("keypad_B2 order = %v", kp.Props["order"])
	}
	// Composed code keys are visible to nobody; panel digits only to their own side.
	if _, ok := w.Visibility["code_A1"]; ok {
		t.Fatal("code_A1 must not be visible")
	}
	if got := w.Visibility["panel_A_red"]; len(got) != 1 || got[0] != "A" {
		t.Fatalf("panel_A_red visibility %v", got)
	}

	objs := func(m map[string]any, side string) []any {
		return m["sides"].(map[string]any)[side].(map[string]any)["objects"].([]any)
	}
	cases := map[string]struct {
		fn   func(m map[string]any)
		want string
	}{
		"missing colour": {func(m map[string]any) {
			var keep []any
			for _, o := range objs(m, "A") {
				if o.(map[string]any)["id"] != "panel_A_blue" {
					keep = append(keep, o)
				}
			}
			m["sides"].(map[string]any)["A"].(map[string]any)["objects"] = keep
		}, "exactly one blue"},
		"own side": {func(m map[string]any) {
			m["codes"].(map[string]any)["code_A1"].(map[string]any)["side"] = "B"
		}, "own side"},
		"unknown colour in order": {func(m map[string]any) {
			m["codes"].(map[string]any)["code_A1"].(map[string]any)["order"] = []any{"red", "purple"}
		}, "no purple code_panel"},
		"empty order": {func(m map[string]any) {
			m["codes"].(map[string]any)["code_A1"].(map[string]any)["order"] = []any{}
		}, "order is empty"},
	}
	for name, tc := range cases {
		err := mutateDir(t, "testdata/colour", tc.fn)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}

// The one-panel-per-code form still loads and validates.
func TestLegacyCodePanels(t *testing.T) {
	w, err := Load("testdata/legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Visibility["code_A1"]; len(got) != 1 || got[0] != "A" {
		t.Fatalf("legacy code_A1 visibility %v", got)
	}
	_, kp := w.SideOf("keypad_B2")
	if _, ok := kp.Props["order"]; ok {
		t.Fatal("legacy keypad should have no order")
	}
	if _, o := w.SideOf("panel_A1"); o == nil || o.Key != "code_A1" {
		t.Fatalf("code panel key not normalised: %+v", o)
	}
	err = mutateDir(t, "testdata/legacy", func(m map[string]any) {
		for _, r := range m["rules"].([]any) {
			if rm := r.(map[string]any); rm["on"] == "keypad_B2:submit" {
				rm["requires"] = map[string]any{"code": "code_B1"}
			}
		}
	})
	if err == nil || !strings.Contains(err.Error(), "same side") {
		t.Fatalf("want same-side error, got %v", err)
	}
}

// A side-level boss block becomes a boss object so the client spawns it.
func TestBossBlockBecomesObject(t *testing.T) {
	for _, dir := range []string{"testdata/colour", "../../worlds/default"} {
		w, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for side, s := range w.Sides {
			if len(s.Boss) == 0 {
				continue
			}
			var boss *Object
			for i := range s.Objects {
				if s.Objects[i].Type == "boss" {
					boss = &s.Objects[i]
				}
			}
			if boss == nil {
				t.Fatalf("%s side %s: boss block but no boss object", dir, side)
			}
			found++
			if _, ok := boss.Props["patrol"]; !ok {
				t.Fatalf("%s side %s: boss object has no patrol: %+v", dir, side, boss.Props)
			}
			if boss.X == 0 && boss.Y == 0 {
				t.Fatalf("%s side %s: boss has no position", dir, side)
			}
			if _, ok := w.Colors[boss.ID]; ok {
				t.Fatalf("%s: boss should have no glow colour", dir)
			}
		}
		if found == 0 {
			t.Fatalf("%s: no boss block found", dir)
		}
	}
}
