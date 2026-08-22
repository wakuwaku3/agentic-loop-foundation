package ci

import "testing"

func fixture() Manifest {
	return Manifest{Version: 1, Components: []Component{{ID: "a", Roots: []string{"a/**"}, PublicContracts: []string{"a/api.json"}, Check: Check{Runner: "make", Target: "a"}}, {ID: "b", Roots: []string{"b/**"}, ContractDependencies: []string{"a/api.json"}, Dependencies: []string{"a"}, Check: Check{Runner: "make", Target: "b"}}}}
}
func TestAffectedReverseClosure(t *testing.T) {
	p, e := Affected(fixture(), []string{"a/api.json"})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Selected) != 2 || p.Selected[0] != "a" || p.Selected[1] != "b" {
		t.Fatalf("selected=%v", p.Selected)
	}
}
func TestManifestRejectsCycle(t *testing.T) {
	m := fixture()
	m.Components[0].Dependencies = []string{"b"}
	if err := Validate(m); err == nil {
		t.Fatal("expected cycle")
	}
}
func TestTrackedOwnership(t *testing.T) {
	m := fixture()
	if err := ValidateTracked(m, []string{"a/file"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTracked(m, []string{"other"}); err == nil {
		t.Fatal("expected unowned")
	}
}
