package release

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

func buildControlAndPermit(t *testing.T, revision domain.Revision, fence domain.FencingToken) (domain.EffectiveControlResult, domain.PermitDecision) {
	t.Helper()
	control := domain.EffectiveControlResult{Found: true, Mode: domain.ControlAllow, Revision: revision}
	permit, err := domain.Permit(control, domain.PermitRequest{Kind: domain.PermitPromotion, ControlRevision: revision, FencingToken: fence, ExpectedFencingToken: fence, Resource: "release"})
	if err != nil {
		t.Fatal(err)
	}
	return control, permit
}

func buildFullCandidateInput(assembled AssembledBundle, candidateDigest string) CandidateInput {
	// ReleaseID and CandidateID are both derived from candidateDigest so
	// that distinct candidates (e.g. journey_test.go's v1/v2) occupy
	// distinct Store keys; MemoryStore.Put keys on Candidate.ID.
	id, _ := domain.NewReleaseID(candidateDigest)
	cid, _ := domain.NewReleaseID(candidateDigest)
	targets := map[string]domain.CapabilityTarget{}
	for _, cap := range assembled.Contract.Capabilities {
		targets[cap] = domain.CapabilityTarget{Target: "preview", Provider: "fake"}
	}
	return CandidateInput{
		ReleaseID: id, CandidateID: cid, CandidateDigest: candidateDigest, Version: 1,
		Status: domain.ReleaseExercising, CapabilityTargets: targets,
		RollbackEvidence: true, ResumeEvidence: true, ExpectedControlRevision: 1, FencingToken: 1,
	}
}

// fullyEvidencedBundle assembles a candidate from root with full capability
// evidence bound to its own assembled digests, ready to pass PromotionGate.
func fullyEvidencedBundle(t *testing.T, root, repository, candidateDigest string) (Bundle, AssembledBundle, CandidateInput) {
	t.Helper()
	assembled0, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	input := buildFullCandidateInput(assembled0, candidateDigest)
	bundle, assembled, err := AssembleBundle(root, repository, input, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	bundle.Candidate.Evidence = evidenceFor(bundle.Candidate, assembled.BundleDigest)
	bundle.Candidate.EvidenceDigest = computeEvidenceDigest(bundle.Candidate.Evidence)
	return bundle, assembled, input
}

// --- A2: the measured defect (fabricated matching DocsDigest promotes) is
// pinned by a regression test showing the before and the after.

func TestPipelinePromoteRefusesFabricatedMatchingDocsDigest(t *testing.T) {
	root := newSyntheticTree(t, defaultSyntheticOptions())
	bundle, _, _ := fullyEvidencedBundle(t, root, "repo", "candidate-1")
	control, permit := buildControlAndPermit(t, 1, 1)

	fabricated := bundle.Snapshot()
	fabricated.Candidate.DocsDigest = "drifted-docs"
	for i := range fabricated.Candidate.Evidence {
		fabricated.Candidate.Evidence[i].DocsDigest = "drifted-docs"
	}

	// Measured pre-change behaviour: PromotionGate alone (unchanged; this is
	// exactly what release_test.go's TestContractAndPromotionGateRejectDriftAndStaleEvidence
	// exercises) only compares the candidate against its own evidence, so a
	// candidate and evidence that agree with each other on a fabricated
	// value promote with a nil error.
	if _, err := PromotionGate(fabricated, control, permit); err != nil {
		t.Fatalf("measured pre-change behaviour: PromotionGate alone must still accept an internally-consistent fabricated digest (got error %v); the defect is real", err)
	}

	// After the fix: Pipeline.Promote re-verifies the candidate's digests
	// against the source tree and refuses, because "drifted-docs" does not
	// equal the doc set's real digest even though candidate and evidence
	// agree with each other.
	store := NewMemoryStore()
	router := NewRouter()
	pipeline := NewPipeline(store, router, root)
	if err := router.SetPreview("repo", "candidate-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Promote(fabricated, control, permit); err == nil {
		t.Fatal("after the fix: Pipeline.Promote must refuse a candidate whose DocsDigest was fabricated to match only its own evidence")
	}
	if route, _ := router.Get("repo"); route.StableDigest != "" {
		t.Fatalf("route must not advance on a refused promotion, got %#v", route)
	}
}

// --- A5 / d4 L1: the docs drift gate is proven hermetically, including the
// mandatory negative control (e).

func TestPipelinePromoteDocsDriftHermeticWithNegativeControl(t *testing.T) {
	opts := defaultSyntheticOptions()
	opts.ExtraFiles = map[string]string{"NOTES.txt": "unrelated note, matches no role glob\n"}
	root := newSyntheticTree(t, opts)
	bundle, assembled, input := fullyEvidencedBundle(t, root, "repo", "candidate-1")
	control, permit := buildControlAndPermit(t, 1, 1)
	store := NewMemoryStore()
	router := NewRouter()
	pipeline := NewPipeline(store, router, root)

	if err := router.SetPreview("repo", "candidate-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Promote(bundle, control, permit); err != nil {
		t.Fatalf("baseline candidate must promote to stable: %v", err)
	}
	route, _ := router.Get("repo")
	if route.StableDigest != "candidate-1" {
		t.Fatalf("route did not advance to stable: %#v", route)
	}
	generationAfterPromote := route.Generation

	// Flip one byte in one documentation member.
	flipByte(t, filepath.Join(root, "docs", "preview", "index.md"))

	afterFlip, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	// (a) DocsDigest changed.
	if afterFlip.DocsDigest == assembled.DocsDigest {
		t.Fatal("(a) DocsDigest did not change after a one-byte documentation edit")
	}

	// (b) VerifySource refuses and names the drifted member path.
	err = VerifySource(bundle.Members, root)
	if err == nil {
		t.Fatal("(b) VerifySource did not refuse after documentation drift")
	}
	var drift *DriftError
	if !errors.As(err, &drift) {
		t.Fatalf("(b) VerifySource error is not a *DriftError: %v", err)
	}
	if drift.Path != "docs/preview/index.md" {
		t.Fatalf("(b) VerifySource error names %q, want docs/preview/index.md", drift.Path)
	}

	// (c) the candidate assembled before the edit no longer passes
	// CanPromote once its evidence is checked against the post-edit digest:
	// re-deriving the candidate from the now-drifted tree and reusing the
	// pre-edit evidence shows the evidence is stale.
	_, freshCandidate, err := AssembleCandidate(root, input)
	if err != nil {
		t.Fatal(err)
	}
	freshCandidate.Evidence = bundle.Candidate.Evidence
	if err := freshCandidate.CanPromote(); err == nil {
		t.Fatal("(c) evidence bound to the pre-edit bundle digest still promoted after documentation drift")
	}

	// (d) Pipeline.Promote returns an error and Router generation is
	// unchanged.
	if _, err := pipeline.Promote(bundle, control, permit); err == nil {
		t.Fatal("(d) Pipeline.Promote succeeded after documentation drift")
	}
	routeAfterDrift, _ := router.Get("repo")
	if routeAfterDrift.Generation != generationAfterPromote {
		t.Fatalf("(d) Router generation changed after a refused promotion: before=%d after=%d", generationAfterPromote, routeAfterDrift.Generation)
	}

	// (e) mandatory negative control: flipping a byte in a file matching no
	// role glob leaves BundleDigest and DocsDigest unchanged. Without this,
	// a whole-tree hash would satisfy (a)-(d) too, and every unrelated
	// commit would register as a docs drift.
	beforeNegative, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	flipByte(t, filepath.Join(root, "NOTES.txt"))
	afterNegative, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if afterNegative.BundleDigest != beforeNegative.BundleDigest {
		t.Fatal("(e) BundleDigest changed after mutating a file matching no role glob")
	}
	if afterNegative.DocsDigest != beforeNegative.DocsDigest {
		t.Fatal("(e) DocsDigest changed after mutating a file matching no role glob")
	}
}

// --- A7 (third case): a hand-built candidate declaring one capability,
// bound to the real contract and docs digests, is refused.

func TestPipelinePromoteRefusesCapabilitySubsetAgreeingDigests(t *testing.T) {
	root := realRoot()
	assembled, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(assembled.Contract.Capabilities) != 12 {
		t.Fatalf("expected the real Foundation contract to declare 12 capabilities, got %d", len(assembled.Contract.Capabilities))
	}

	id, _ := domain.NewReleaseID("release-1")
	cid, _ := domain.NewReleaseID("candidate-healthz")
	evidence := []domain.CapabilityEvidence{{
		Capability: "healthz", CandidateID: cid, CandidateDigest: "candidate-healthz",
		BundleDigest: assembled.BundleDigest, ContractDigest: assembled.ContractDigest, DocsDigest: assembled.DocsDigest,
		Digest: "ev-healthz", Verified: true, Fresh: true, Target: "preview", Provider: "fake",
	}}
	handBuilt := domain.ReleaseCandidate{
		ID: id, CandidateID: cid, CandidateDigest: "candidate-healthz", Version: 1, Status: domain.ReleaseExercising,
		Capabilities:            []string{"healthz"},
		CapabilityTargets:       map[string]domain.CapabilityTarget{"healthz": {Target: "preview", Provider: "fake"}},
		Evidence:                evidence,
		RollbackEvidence:        true,
		ResumeEvidence:          true,
		BundleDigest:            assembled.BundleDigest,
		ContractDigest:          assembled.ContractDigest,
		DocsDigest:              assembled.DocsDigest,
		EvidenceDigest:          computeEvidenceDigest(evidence),
		ExpectedControlRevision: 1,
		FencingToken:            1,
	}
	control, permit := buildControlAndPermit(t, 1, 1)
	handBuiltBundle, err := NewBundle("repo", handBuilt, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}

	// Measured pre-change behaviour: PromotionGate alone still promotes
	// this candidate to stable with a nil error, exactly as measured by
	// dp-v2-021 d6, because it never compares the candidate's declared
	// capability set against the compiled contract.
	if _, err := PromotionGate(handBuiltBundle, control, permit); err != nil {
		t.Fatalf("measured pre-change behaviour: PromotionGate alone must still promote a single-capability candidate whose digests agree with the real tree (got error %v)", err)
	}

	// After the fix: VerifyCandidateAgainstContract, and therefore
	// Pipeline.Promote, refuses it.
	if err := VerifyCandidateAgainstContract(handBuilt, assembled.Contract); err == nil {
		t.Fatal("expected a candidate declaring one capability to be refused against a twelve-capability contract")
	}

	handBuiltBundle.Members = assembled.Members
	store := NewMemoryStore()
	router := NewRouter()
	pipeline := NewPipeline(store, router, root)
	if err := router.SetPreview("repo", "candidate-healthz"); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Promote(handBuiltBundle, control, permit); err == nil {
		t.Fatal("Pipeline.Promote must refuse a candidate whose capability set diverges from the contract")
	}
}

// --- A8: rollback is monotonic.

func TestRouterRollbackIsMonotonic(t *testing.T) {
	router := NewRouter()
	if err := router.SetPreview("repo", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := router.Promote("repo", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := router.SetPreview("repo", "v2"); err != nil {
		t.Fatal(err)
	}
	if err := router.Promote("repo", "v2"); err != nil {
		t.Fatal(err)
	}

	record, err := router.RollbackWithReason("repo", "regression", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if record.From != "v2" || record.To != "v1" || record.Reason != "regression" {
		t.Fatalf("unexpected rollback record: %#v", record)
	}
	route, _ := router.Get("repo")
	if route.StableDigest != "v1" || route.PreviousStableDigest != "" {
		t.Fatalf("route after rollback = %#v, want StableDigest=v1 and a cleared forward pointer", route)
	}

	// Second consecutive rollback is refused: no gate-free re-promotion of a
	// withdrawn digest.
	if _, err := router.RollbackWithReason("repo", "again", time.Unix(200, 0)); err == nil {
		t.Fatal("expected a second consecutive rollback to be refused")
	}
	route, _ = router.Get("repo")
	if route.StableDigest != "v1" {
		t.Fatalf("stable digest must remain v1 after a refused second rollback, got %#v", route)
	}

	// Moving forward again requires SetPreview plus a full Promote, not
	// another rollback.
	if err := router.SetPreview("repo", "v2"); err != nil {
		t.Fatal(err)
	}
	if err := router.Promote("repo", "v2"); err != nil {
		t.Fatal(err)
	}
	route, _ = router.Get("repo")
	if route.StableDigest != "v2" {
		t.Fatalf("re-promotion after rollback failed: %#v", route)
	}

	if history := router.RollbackHistory("repo"); len(history) != 1 {
		t.Fatalf("expected exactly one recorded rollback, got %d", len(history))
	}
}

// --- A10: retention eligibility has three independent refusals and one
// positive case, over an injected clock and a rollback window.

func TestRetentionEligibleThreeRefusalsAndPositiveCase(t *testing.T) {
	now := time.Unix(1000, 0)
	window := 24 * time.Hour

	if eligible, reason := RetentionEligible(RetentionInput{Digest: "v1", CurrentStable: "v1", Now: now, RollbackWindow: window}); eligible || reason == "" {
		t.Fatalf("current stable target must be refused, got eligible=%v reason=%q", eligible, reason)
	}
	if eligible, reason := RetentionEligible(RetentionInput{Digest: "v1", CurrentStable: "v2", PreviousStable: "v1", Now: now, RollbackWindow: window}); eligible || reason == "" {
		t.Fatalf("previous stable target must be refused, got eligible=%v reason=%q", eligible, reason)
	}
	if eligible, reason := RetentionEligible(RetentionInput{Digest: "v1", CurrentStable: "v3", PreviousStable: "v2", RolledBackAt: now, RollbackWindow: window, Now: now.Add(time.Hour)}); eligible || reason == "" {
		t.Fatalf("open rollback window must be refused, got eligible=%v reason=%q", eligible, reason)
	}
	if eligible, reason := RetentionEligible(RetentionInput{Digest: "v1", CurrentStable: "v3", PreviousStable: "v2", ReferencedByRequirement: true, Now: now, RollbackWindow: window}); eligible || reason == "" {
		t.Fatalf("Requirement reference must be refused, got eligible=%v reason=%q", eligible, reason)
	}
	if eligible, reason := RetentionEligible(RetentionInput{
		Digest: "v1", CurrentStable: "v3", PreviousStable: "v2",
		RolledBackAt: now, RollbackWindow: window, Now: now.Add(48 * time.Hour),
	}); !eligible || reason != "" {
		t.Fatalf("expected the positive case to be eligible with no reason, got eligible=%v reason=%q", eligible, reason)
	}
}

// --- A11: Requirement completion is chained to the gate.

func TestRequirementCompletionChainedToGate(t *testing.T) {
	root := newSyntheticTree(t, defaultSyntheticOptions())
	bundle, _, _ := fullyEvidencedBundle(t, root, "repo", "candidate-1")
	control, permit := buildControlAndPermit(t, 1, 1)
	store := NewMemoryStore()
	router := NewRouter()
	pipeline := NewPipeline(store, router, root)
	if err := router.SetPreview("repo", "candidate-1"); err != nil {
		t.Fatal(err)
	}

	actor, _ := domain.NewActorID("tester")
	reqID, _ := domain.NewRequirementID("req-1")
	requirement := domain.Requirement{ID: reqID, Status: domain.RequirementEvaluating, Version: 1}

	// A refused gate returns the zero proof; CompleteRequirementFromRelease
	// refuses that zero proof with ErrEvidenceIncomplete (V2-010 already
	// proves this transition is reachable only with a valid proof; this
	// test only chains a refused gate's output to that refusal).
	if _, err := domain.CompleteRequirementFromRelease(requirement, domain.StableReleaseProof{}, actor, time.Unix(1, 0)); !errors.Is(err, domain.ErrEvidenceIncomplete) {
		t.Fatalf("zero proof must refuse completion with ErrEvidenceIncomplete, got %v", err)
	}

	result, err := pipeline.Promote(bundle, control, permit)
	if err != nil {
		t.Fatalf("baseline candidate must promote: %v", err)
	}

	completed, err := domain.CompleteRequirementFromRelease(requirement, result.Proof, actor, time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}

	stored, ok, err := store.Get("repo", bundle.Candidate.ID.String())
	if err != nil || !ok {
		t.Fatalf("bundle must be stored under the candidate id: ok=%v err=%v", ok, err)
	}
	if completed.StableSnapshot.BundleDigest != stored.Candidate.BundleDigest {
		t.Fatalf("StableSnapshot.BundleDigest = %q, want the stored bundle's %q", completed.StableSnapshot.BundleDigest, stored.Candidate.BundleDigest)
	}
	if completed.StableSnapshot.EvidenceDigest != stored.Candidate.EvidenceDigest {
		t.Fatalf("StableSnapshot.EvidenceDigest = %q, want the stored bundle's %q", completed.StableSnapshot.EvidenceDigest, stored.Candidate.EvidenceDigest)
	}

	route, _ := router.Get("repo")
	if route.StableDigest != stored.Candidate.CandidateDigest {
		t.Fatalf("router stable digest = %q, want the completed candidate digest %q", route.StableDigest, stored.Candidate.CandidateDigest)
	}
	if got := ResolveChannel(route.StableDigest != ""); got != "stable" {
		t.Fatalf("default documentation channel = %q, want stable once a stable release exists", got)
	}

	// If the completion step itself fails (stale version), the router
	// generation must be unchanged: completion has no router side effect at
	// all, so there is no half-promoted state to construct.
	generationBefore := route.Generation
	staleRequirement := completed // already completed; RequirementEvaluate-only transition refuses this
	if _, err := domain.CompleteRequirementFromRelease(staleRequirement, result.Proof, actor, time.Unix(3, 0)); err == nil {
		t.Fatal("expected completing an already-completed Requirement to be refused")
	}
	routeAfter, _ := router.Get("repo")
	if routeAfter.Generation != generationBefore {
		t.Fatalf("router generation changed after a failed completion step: before=%d after=%d", generationBefore, routeAfter.Generation)
	}
}
