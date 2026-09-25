package world

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultWorldLoads(t *testing.T) {
	w, err := Load("../../worlds/default")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Visibility["door_B1"]; len(got) != 1 || got[0] != "B" {
		t.Fatalf("door_B1 visibility %v", got)
	}
	if got := w.Visibility["exit_open"]; len(got) != 2 {
		t.Fatalf("exit_open visibility %v", got)
	}
	if got := w.Visibility["code_A1"]; len(got) != 1 || got[0] != "A" {
		t.Fatalf("code_A1 visibility %v", got)
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
	if _, o := w.SideOf("panel_A1"); o == nil || o.Key != "code_A1" {
		t.Fatalf("code panel key not normalised: %+v", o)
	}
}

// mutate loads the default world.json, applies fn, writes it to a temp dir with the maps, and loads it.
func mutate(t *testing.T, fn func(m map[string]any)) error {
	t.Helper()
	b, _ := os.ReadFile("../../worlds/default/world.json")
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	fn(m)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "maps"), 0o755)
	for _, f := range []string{"side_a.txt", "side_b.txt"} {
		src, _ := os.ReadFile(filepath.Join("../../worlds/default/maps", f))
		_ = os.WriteFile(filepath.Join(dir, "maps", f), src, 0o644)
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
		"keypad on same side as panel": {func(m map[string]any) {
			rule(m, "keypad_B2:submit")["requires"] = map[string]any{"code": "code_B1"}
		}, "same side"},
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
