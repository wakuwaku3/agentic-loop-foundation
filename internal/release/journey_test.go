package release

import (
	"context"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// TestJourneyReleaseSegment is Journey 1's release segment: assemble from a
// synthetic tree, set the preview route, record capability evidence for
// every declared capability, pass the gate, promote the route, complete the
// Requirement from the proof, and assert the stable documentation channel
// is the default. Its own t.TempDir(), its own context deadline, no
// package-level mutable state, no fixed sleep, no timer, no goroutine, no
// dependence on another test's order.
func TestJourneyReleaseSegment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	root := newSyntheticTree(t, defaultSyntheticOptions())
	bundle, _, _ := fullyEvidencedBundle(t, root, "repo", "candidate-1")
	control, permit := buildControlAndPermit(t, 1, 1)
	store := NewMemoryStore()
	router := NewRouter()
	pipeline := NewPipeline(store, router, root)

	if err := ctx.Err(); err != nil {
		t.Fatal("journey deadline expired before starting")
	}
	if err := router.SetPreview("repo", "candidate-1"); err != nil {
		t.Fatal(err)
	}

	result, err := pipeline.Promote(bundle, control, permit)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal("journey exceeded its deadline before completing the Requirement")
	}

	actor, _ := domain.NewActorID("tester")
	reqID, _ := domain.NewRequirementID("req-journey-1")
	requirement := domain.Requirement{ID: reqID, Status: domain.RequirementEvaluating, Version: 1}
	completed, err := domain.CompleteRequirementFromRelease(requirement, result.Proof, actor, time.Unix(2, 0))
	if err != nil {
		t.Fatalf("CompleteRequirementFromRelease: %v", err)
	}
	if completed.Status != domain.RequirementCompleted {
		t.Fatalf("Requirement status = %s, want completed", completed.Status)
	}

	route, ok := router.Get("repo")
	if !ok || route.StableDigest != "candidate-1" {
		t.Fatalf("route after promotion = %#v (ok=%v), want stable=candidate-1", route, ok)
	}
	if got := ResolveChannel(route.StableDigest != ""); got != "stable" {
		t.Fatalf("default documentation channel = %q, want stable", got)
	}
}

// TestJourneyLocalSegment is Journey 7's local segment: a Preview
// regression withdraws the candidate, rollback restores the previous
// stable route and its doc set, and a Requirement is resumed against the
// restored stable snapshot. Journey 7's live half — intentionally breaking
// a deployed Preview and resuming against a real previous Stable — is
// V2-022 and is untouched here.
func TestJourneyLocalSegment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}

	root := newSyntheticTree(t, defaultSyntheticOptions())
	control, permit := buildControlAndPermit(t, 1, 1)
	store := NewMemoryStore()
	router := NewRouter()
	pipeline := NewPipeline(store, router, root)

	bundleV1, _, _ := fullyEvidencedBundle(t, root, "repo", "candidate-v1")
	if err := router.SetPreview("repo", "candidate-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Promote(bundleV1, control, permit); err != nil {
		t.Fatalf("promote v1: %v", err)
	}

	bundleV2, _, _ := fullyEvidencedBundle(t, root, "repo", "candidate-v2")
	if err := router.SetPreview("repo", "candidate-v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Promote(bundleV2, control, permit); err != nil {
		t.Fatalf("promote v2: %v", err)
	}
	route, _ := router.Get("repo")
	if route.StableDigest != "candidate-v2" {
		t.Fatalf("expected v2 to be stable before the regression, got %#v", route)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal("journey exceeded its deadline before the rollback")
	}

	// A Preview regression is discovered against v2 (locally: this is a
	// withdrawal of v2 as stable, not a real deployed Preview break).
	record, err := router.RollbackWithReason("repo", "preview-regression", time.Unix(500, 0))
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if record.From != "candidate-v2" || record.To != "candidate-v1" {
		t.Fatalf("unexpected rollback record: %#v", record)
	}
	route, _ = router.Get("repo")
	if route.StableDigest != "candidate-v1" {
		t.Fatalf("expected route to restore v1 after rollback, got %#v", route)
	}
	if route.PreviousStableDigest != "" {
		t.Fatalf("rollback must clear the forward pointer, got %#v", route)
	}

	// The restored stable's own bundle is still resolvable in the Store and
	// its documentation channel is the default.
	restored, ok, err := store.Get("repo", bundleV1.Candidate.ID.String())
	if err != nil || !ok {
		t.Fatalf("restored stable bundle must still be resolvable: ok=%v err=%v", ok, err)
	}
	if restored.Candidate.CandidateDigest != "candidate-v1" {
		t.Fatalf("restored bundle candidate digest = %q, want candidate-v1", restored.Candidate.CandidateDigest)
	}
	if got := ResolveChannel(route.StableDigest != ""); got != "stable" {
		t.Fatalf("default documentation channel after rollback = %q, want stable", got)
	}

	// A Requirement is resumed against the restored stable snapshot: the
	// gate is a pure function of the stored bundle, so re-running it over
	// the already-stable restored bundle reproduces the same proof a resume
	// would use, without a second route transition.
	proof, err := PromotionGate(restored, control, permit)
	if err != nil {
		t.Fatalf("re-deriving the restored stable's proof: %v", err)
	}
	actor, _ := domain.NewActorID("tester")
	reqID, _ := domain.NewRequirementID("req-journey-7")
	requirement := domain.Requirement{ID: reqID, Status: domain.RequirementEvaluating, Version: 1}
	resumed, err := domain.CompleteRequirementFromRelease(requirement, proof.Proof, actor, time.Unix(600, 0))
	if err != nil {
		t.Fatalf("resume Requirement against restored stable: %v", err)
	}
	if resumed.StableSnapshot.BundleDigest != restored.Candidate.BundleDigest {
		t.Fatalf("resumed StableSnapshot.BundleDigest = %q, want restored bundle digest %q", resumed.StableSnapshot.BundleDigest, restored.Candidate.BundleDigest)
	}

	// A second consecutive rollback is refused: the withdrawn v2 cannot
	// come back without a fresh SetPreview and a full gate pass.
	if _, err := router.RollbackWithReason("repo", "again", time.Unix(700, 0)); err == nil {
		t.Fatal("expected a second consecutive rollback to be refused")
	}
}
