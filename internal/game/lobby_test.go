package game_test

import (
	"regexp"
	"testing"

	"github.com/fl1X12/phoenix/internal/game"
	"github.com/fl1X12/phoenix/internal/world"
)

func TestSideForToken(t *testing.T) {
	a, b, lobby := setup(t)
	if side, ok := lobby.SideForToken("TEST", a.tok); !ok || side != "A" {
		t.Fatalf("A token -> %q %v", side, ok)
	}
	if side, ok := lobby.SideForToken("TEST", b.tok); !ok || side != "B" {
		t.Fatalf("B token -> %q %v", side, ok)
	}
	if _, ok := lobby.SideForToken("TEST", "nope"); ok {
		t.Fatal("bogus token resolved")
	}
	if _, ok := lobby.SideForToken("OTHER", a.tok); ok {
		t.Fatal("token resolved for wrong room")
	}
}

func TestCreateCodes(t *testing.T) {
	w, err := world.Load(fixture)
	if err != nil {
		t.Fatal(err)
	}
	lobby := game.NewLobby(func(string) *world.World { return w })
	shape := regexp.MustCompile(`^[A-HJ-NP-Z2-9]{4}$`)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		r := lobby.Create("")
		if !shape.MatchString(r.Code) {
			t.Fatalf("code %q has the wrong shape", r.Code)
		}
		if seen[r.Code] {
			t.Fatalf("code %q issued twice", r.Code)
		}
		seen[r.Code] = true
		if lobby.Get(r.Code, false) != r {
			t.Fatalf("code %q not findable", r.Code)
		}
	}
	if lobby.Get("ZZZZ", false) != nil {
		t.Fatal("unknown code resolved")
	}
}

func TestCreateUnknownWorld(t *testing.T) {
	w, err := world.Load(fixture)
	if err != nil {
		t.Fatal(err)
	}
	lobby := game.NewLobby(func(id string) *world.World {
		if id == "" || id == w.Name {
			return w
		}
		return nil
	})
	if r := lobby.Create("nope"); r != nil {
		t.Fatalf("unknown world made room %q", r.Code)
	}
	if r := lobby.Create(w.Name); r == nil {
		t.Fatal("known world made no room")
	}
	if len(lobby.Codes()) != 1 {
		t.Fatalf("want 1 room, got %v", lobby.Codes())
	}
}
