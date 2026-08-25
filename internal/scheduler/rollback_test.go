package scheduler

// V2-030 / A6: strengthens the cross-repository rollback assertion from
// all-or-nothing (already covered by
// TestDecideCrossRepositoryUsesOneRunnerAndApplyIsAtomic's partial-plan
// case) to convergence. The M7 condition is that rollback CONVERGES, not
// merely that it rolls back: a rollback that always rolls back again is a
// livelock the all-or-nothing assertion alone would call a pass.
//
// Scenario: a genuine Plan for one cross-repository Requirement is first
// corrupted by appending a conflicting foreign Claim on the second
// Repository (representing, e.g., a stale plan being applied against
// reality), so Apply must reject it and roll back to the exact pre-Apply
// Snapshot. The immediately following cycle re-Decides from that
// (unmodified) Snapshot and applies the genuine Plan, which now succeeds
// completely. A third cycle, from the now-Assigned Snapshot, must be a
// no-op -- proving convergence rather than oscillation.

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func crossRepoRollbackSnapshot() Snapshot {
	s := testSnapshot()
	s.Requirements = []Requirement{{
		ID:            "req-1",
		RepositoryIDs: []string{"repo-a", "repo-b"},
		Status:        StatusReady,
		CreatedAt:     s.Now.Add(-time.Hour),
		Resources:     []ResourceRequest{{Name: "workspace", Mode: Write}},
	}}
	return s
}

// claimMultiset builds a comparable representation of a Claim slice as a
// count per distinct Claim value, so two claim sets can be compared as
// multisets (order-independent, duplicate-sensitive) rather than merely by
// length.
func claimMultiset(claims []Claim) map[Claim]int {
	m := map[Claim]int{}
	for _, c := range claims {
		m[c]++
	}
	return m
}

func TestCrossRepositoryRollbackConverges(t *testing.T) {
	s0 := crossRepoRollbackSnapshot()

	genuinePlan, err := Decide(s0)
	if err != nil {
		t.Fatal(err)
	}
	if len(genuinePlan.Assignments) != 2 || len(genuinePlan.Claims) != 2 {
		t.Fatalf("fixture assumption broken: expected 2 assignments/claims (one per repository), got %#v", genuinePlan)
	}
	if genuinePlan.Assignments[0].RunnerID != genuinePlan.Assignments[1].RunnerID {
		t.Fatalf("fixture assumption broken: cross-repository requirement must use one runner: %#v", genuinePlan.Assignments)
	}

	// (a) Corrupt the genuine plan: append a foreign, conflicting Write
	// claim for the same resource on repo-b (as if another actor's claim
	// landed there between Decide and Apply). Apply must reject the whole
	// plan and leave the pre-Apply Snapshot untouched.
	corrupt := Plan{
		Assignments: append([]Assignment(nil), genuinePlan.Assignments...),
		Claims:      append(append([]Claim(nil), genuinePlan.Claims...), Claim{Resource: "workspace", Owner: "intruder", RepositoryID: "repo-b", Mode: Write}),
	}
	rolledBack, err := Apply(s0, corrupt)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict from the corrupted plan, got %v", err)
	}
	if !reflect.DeepEqual(rolledBack, s0) {
		t.Fatalf("rollback did not return the exact pre-Apply snapshot:\n got  %#v\n want %#v", rolledBack, s0)
	}
	if len(rolledBack.Claims) != 0 {
		t.Fatalf("rollback left new claims behind: %#v", rolledBack.Claims)
	}
	if rolledBack.Requirements[0].Status != StatusReady {
		t.Fatalf("rollback changed requirement status: %#v", rolledBack.Requirements[0])
	}

	// (b) The immediately following Decide/Apply cycle, over that same
	// (unmodified) snapshot, must reach a complete assignment with no
	// duplicated claim and no duplicated assignment.
	plan2, err := Decide(rolledBack)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan2.Assignments, genuinePlan.Assignments) {
		t.Fatalf("second cycle's Decide did not reproduce the genuine plan deterministically:\n got  %#v\n want %#v", plan2.Assignments, genuinePlan.Assignments)
	}
	complete, err := Apply(rolledBack, plan2)
	if err != nil {
		t.Fatalf("second cycle's Apply failed: %v", err)
	}
	if complete.Requirements[0].Status != StatusAssigned {
		t.Fatalf("second cycle did not reach a complete assignment: %#v", complete.Requirements[0])
	}
	if len(complete.Claims) != 2 {
		t.Fatalf("second cycle produced %d claims, want exactly 2 (one per repository, no duplication): %#v", len(complete.Claims), complete.Claims)
	}
	wantClaims := claimMultiset([]Claim{
		{Resource: "workspace", Owner: "req-1", RepositoryID: "repo-a", Mode: Write},
		{Resource: "workspace", Owner: "req-1", RepositoryID: "repo-b", Mode: Write},
	})
	if got := claimMultiset(complete.Claims); !reflect.DeepEqual(got, wantClaims) {
		t.Fatalf("claim multiset mismatch after convergence: got %#v want %#v", got, wantClaims)
	}
	assignedRepos := map[string]int{}
	for _, a := range plan2.Assignments {
		assignedRepos[a.RepositoryID]++
	}
	if assignedRepos["repo-a"] != 1 || assignedRepos["repo-b"] != 1 || len(assignedRepos) != 2 {
		t.Fatalf("assignment set is not exactly one per repository with no duplication: %#v", assignedRepos)
	}

	// (c) A third cycle, from the now-Assigned snapshot, must be a no-op:
	// req-1 is no longer StatusReady, so Decide produces an empty plan, and
	// Apply of that empty plan changes nothing.
	plan3, err := Decide(complete)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan3.Assignments) != 0 || len(plan3.Claims) != 0 {
		t.Fatalf("third cycle's Decide was not empty: %#v", plan3)
	}
	final, err := Apply(complete, plan3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final, complete) {
		t.Fatalf("third cycle was not a no-op:\n got  %#v\n want %#v", final, complete)
	}
}
