package ci

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestCandidateComponentsDoesNotRequireUnchangedEvidence(t *testing.T) {
	root := "."
	m := fixture()
	key, err := EvidenceKeyWithManifest(root, m, m.Components[0])
	if err != nil {
		t.Fatal(err)
	}
	evidence := t.TempDir()
	if err := os.MkdirAll(evidence, 0755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"result":"passed","evidence_key":"` + key + `"}`)
	if err := os.WriteFile(filepath.Join(evidence, "a-"+key+".json"), body, 0644); err != nil {
		t.Fatal(err)
	}
	if err := CandidateComponents(m, root, evidence, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := CandidateComponents(m, root, evidence, []string{"b"}); err == nil {
		t.Fatal("missing affected evidence accepted")
	}
}
