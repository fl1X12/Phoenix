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
	if w.Colors["button_A1"] != "B" || w.Colors["lever_B1"] != "both" || w.Colors["key_A"] != "A" {
		t.Fatalf("colors %v", w.Colors)
	}
	if _, ok := w.Colors["door_A1"]; ok {
		t.Fatal("door should have no color")
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
	rules := func(m map[string]any) []any { return m["rules"].([]any) }
	cases := map[string]struct {
		fn   func(m map[string]any)
		want string
	}{
		"typo key": {func(m map[string]any) {
			rules(m)[0].(map[string]any)["do"] = []any{map[string]any{"toggle": "door_b1"}}
		}, "nothing reads"},
		"fire never cleared": {func(m map[string]any) {
			m["rules"] = append(rules(m)[:7:7], rules(m)[8:]...) // drop ext_B1 rule
		}, "never cleared"},
		"keypad on same side as panel": {func(m map[string]any) {
			rules(m)[4].(map[string]any)["requires"] = map[string]any{"code": "code_B1"}
		}, "same side"},
		"unknown action": {func(m map[string]any) {
			rules(m)[0].(map[string]any)["on"] = "door_A1:press"
		}, "does not accept"},
		"ragged map": {func(m map[string]any) {
			m["sides"].(map[string]any)["A"].(map[string]any)["map"] = "maps/side_a.txt"
			// write a ragged map instead
		}, ""},
	}
	for name, tc := range cases {
		if tc.want == "" {
			continue
		}
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
