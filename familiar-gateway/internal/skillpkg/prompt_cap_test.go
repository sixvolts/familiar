package skillpkg

import "testing"

// The cap keeps built-ins: a user with 20+ chat skills sorting before
// "research" pushed the built-in out of their chat prompt.
func TestCapPromptSkills_KeepsBuiltins(t *testing.T) {
	var pkgs []*Package
	for _, n := range []string{"a1", "a2", "a3", "a4", "research", "z1"} {
		o := "authored"
		if n == "research" {
			o = "builtin"
		}
		pkgs = append(pkgs, &Package{Name: n, Origin: o})
	}
	kept, dropped := CapPromptSkills(pkgs, 3)
	if dropped != 3 || len(kept) != 3 {
		t.Fatalf("kept %d dropped %d", len(kept), dropped)
	}
	names := kept[0].Name + "," + kept[1].Name + "," + kept[2].Name
	if names != "a1,a2,research" {
		t.Errorf("kept %s, want the built-in and the first two others, in order", names)
	}
	if k, d := CapPromptSkills(pkgs, 10); d != 0 || len(k) != 6 {
		t.Error("under the cap nothing is dropped")
	}
}
