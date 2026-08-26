package release

import (
	"strings"
	"testing"
)

// TestRealTreeFoundationCandidateIsNotPromotable is the A15 real-tree
// conformance test. It assembles the Foundation candidate from the
// repository root and asserts the honest negative result: assembly
// succeeds (all seven roles resolve, the documentation role resolves to
// exactly the real doc set, the contract compiles with the twelve baseline
// ids), but the assembled candidate is NOT promotable, because every
// capability has status preview with empty evidence_ids in the real
// contract. No capability evidence is fabricated for the real tree.
func TestRealTreeFoundationCandidateIsNotPromotable(t *testing.T) {
	root := realRoot()
	assembled, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatalf("assemble the real Foundation tree: %v", err)
	}

	roleCounts := map[Role]int{}
	for _, m := range assembled.Members {
		roleCounts[m.Role]++
	}
	for _, role := range []Role{
		RoleContract, RoleSchema, RoleAPIContract, RoleImplementationManifest,
		RoleMigration, RoleConfiguration, RoleDocumentation,
	} {
		if roleCounts[role] == 0 {
			t.Fatalf("role %s resolved to zero members in the real tree", role)
		}
	}
	t.Logf("real-tree bundle member counts by role: %#v (total %d)", roleCounts, len(assembled.Members))

	wantDocs := map[string]bool{
		"docs/preview/README.md":       true,
		"docs/preview/capabilities.md": true,
		"docs/preview/index.md":        true,
		"docs/preview/stable-diff.md":  true,
		"docs/stable/index.md":         true,
	}
	gotDocs := map[string]bool{}
	for _, m := range assembled.Members {
		if m.Role == RoleDocumentation {
			gotDocs[m.Path] = true
		}
	}
	if len(gotDocs) != len(wantDocs) {
		t.Fatalf("documentation role resolved to %d members %v, want exactly %v", len(gotDocs), gotDocs, wantDocs)
	}
	for path := range wantDocs {
		if !gotDocs[path] {
			t.Fatalf("documentation role is missing expected member %s", path)
		}
	}

	if assembled.Contract.Version != "0.1.0-preview.1" {
		t.Fatalf("Contract.Version = %q, want 0.1.0-preview.1", assembled.Contract.Version)
	}
	wantIDs := []string{
		"cap-repository-registration", "cap-requirement-intake", "cap-backlog-visibility",
		"cap-autonomous-resolution", "cap-human-input-request", "cap-preview-operation",
		"cap-stable-promotion", "cap-loop-control", "cap-loop-self-update",
		"cap-shared-resource-allocation", "cap-provider-operation", "cap-user-documentation",
	}
	if len(assembled.Contract.Capabilities) != len(wantIDs) {
		t.Fatalf("Contract.Capabilities count = %d, want %d", len(assembled.Contract.Capabilities), len(wantIDs))
	}
	for i, id := range wantIDs {
		if assembled.Contract.Capabilities[i] != id {
			t.Fatalf("Contract.Capabilities[%d] = %q, want %q", i, assembled.Contract.Capabilities[i], id)
		}
	}

	// The honest negative result: derive the candidate's capability set
	// from the compiled contract (dp-v2-021 d6) with NO evidence recorded
	// (the real contract's baseline capabilities all carry empty
	// evidence_ids), and assert it is refused rather than fabricating
	// capability evidence for the real tree to make it pass.
	input := buildFullCandidateInput(assembled, "candidate-foundation-real-tree")
	input.Evidence = nil // no fabricated evidence for the real tree
	_, candidate, err := AssembleCandidate(root, input)
	if err != nil {
		t.Fatalf("AssembleCandidate on the real tree: %v", err)
	}
	if len(candidate.Capabilities) != 12 {
		t.Fatalf("candidate declares %d capabilities, want 12", len(candidate.Capabilities))
	}

	err = candidate.CanPromote()
	if err == nil {
		t.Fatal("the real Foundation candidate must NOT be promotable: all twelve capabilities have empty evidence_ids")
	}
	if !strings.Contains(err.Error(), "capability") {
		t.Fatalf("refusal does not name a missing capability: %v", err)
	}
	t.Logf("real-tree candidate refusal (expected): %v", err)
}
