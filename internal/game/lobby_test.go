package game_test

import "testing"

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
