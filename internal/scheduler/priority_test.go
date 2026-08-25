package scheduler

// V2-030 / A3: connects internal/scheduler's scalar Priority to
// domain.PriorityAssessment's multi-factor evaluation. This file proves:
//   - with no assessment, the ordering is byte-identical to the pre-V2-030
//     scalar-only scheduler, on a fixture of 8+ candidates with mixed
//     Priority and CreatedAt;
//   - with an assessment, the multi-factor branch actually drives ranking
//     (a candidate with a low legacy Priority but a strong assessment
//     outranks one with a high legacy Priority and no assessment at all);
//   - Executable == false excludes a candidate from assignment regardless of
//     score;
//   - Assessment.Version, Reason and ReevaluateWhen are carried through into
//     the candidate's Decision record.

import (
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// legacyOrderedIDs is an independent oracle: it reimplements the pre-V2-030
// score() formula directly (not by calling any production code) and sorts
// with the same criteria Decide's sort.SliceStable uses (score desc, ID
// tie-break asc), then returns the ordered requirement IDs. It exists solely
// so TestNoAssessmentOrderingIsIdenticalToLegacyScalarPath has a reference
// that does not share implementation with computeScore/legacyScore, and so a
// bug introduced into both at once would not go unnoticed.
func legacyOrderedIDs(reqs []Requirement, now time.Time) []string {
	type scored struct {
		id    string
		score int64
	}
	items := make([]scored, len(reqs))
	for i, q := range reqs {
		age := int64(now.Sub(q.CreatedAt).Seconds())
		if age < 0 {
			age = 0
		}
		p := q.Priority
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		items[i] = scored{id: q.ID, score: p*300 + age}
	}
	// insertion sort with the same tie-break Decide's sort.SliceStable uses,
	// to avoid depending on Go's sort package matching Decide's usage.
	for i := 1; i < len(items); i++ {
		j := i
		for j > 0 {
			a, b := items[j-1], items[j]
			less := a.score > b.score || (a.score == b.score && a.id < b.id)
			if less {
				break
			}
			items[j-1], items[j] = items[j], items[j-1]
			j--
		}
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.id
	}
	return ids
}

func decisionOrderedIDs(t *testing.T, decisions []Decision) []string {
	t.Helper()
	byRank := append([]Decision(nil), decisions...)
	for i := 1; i < len(byRank); i++ {
		j := i
		for j > 0 && byRank[j-1].Rank > byRank[j].Rank {
			byRank[j-1], byRank[j] = byRank[j], byRank[j-1]
			j--
		}
	}
	ids := make([]string, len(byRank))
	for i, d := range byRank {
		if d.Rank != i {
			t.Fatalf("decision ranks are not a dense 0..n-1 permutation: rank %d landed at position %d (%#v)", d.Rank, i, byRank)
		}
		ids[i] = d.RequirementID
	}
	return ids
}

func TestNoAssessmentOrderingIsIdenticalToLegacyScalarPath(t *testing.T) {
	s := testSnapshot()
	s.Requirements = []Requirement{
		{ID: "r1", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 80, CreatedAt: s.Now.Add(-30 * time.Minute)},
		{ID: "r2", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 80, CreatedAt: s.Now.Add(-90 * time.Minute)},
		{ID: "r3", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 10, CreatedAt: s.Now.Add(-3 * time.Hour)},
		{ID: "r4", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 100, CreatedAt: s.Now},
		{ID: "r5", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 0, CreatedAt: s.Now.Add(-10 * time.Hour)},
		{ID: "r6", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 45, CreatedAt: s.Now.Add(-45 * time.Minute)},
		{ID: "r7", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 45, CreatedAt: s.Now.Add(-45 * time.Minute)},
		{ID: "r8", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 200, CreatedAt: s.Now.Add(-1 * time.Minute)}, // out-of-range, exercises the clamp
	}
	// None of these carry an Assessment: computeScore must take the fallback
	// branch for every one of them.
	for _, q := range s.Requirements {
		if q.Assessment != nil {
			t.Fatalf("fixture requirement %s unexpectedly carries an Assessment", q.ID)
		}
	}

	plan, err := Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Decisions) != len(s.Requirements) {
		t.Fatalf("expected one Decision per candidate, got %d for %d requirements", len(plan.Decisions), len(s.Requirements))
	}
	for _, d := range plan.Decisions {
		if d.Inputs.UsedAssessment {
			t.Fatalf("requirement %s recorded UsedAssessment=true with no Assessment set", d.RequirementID)
		}
	}

	got := decisionOrderedIDs(t, plan.Decisions)
	want := legacyOrderedIDs(s.Requirements, s.Now)
	if len(got) != len(want) {
		t.Fatalf("order length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fallback ordering diverged from the legacy scalar oracle at position %d: got %v want %v", i, got, want)
		}
	}
}

func TestAssessmentPresentDrivesMultiFactorRankingOverLegacyPriority(t *testing.T) {
	s := testSnapshot()
	// A single Runner forces contention: only one of the two candidates
	// below can be assigned in this tick, so which one wins the slot proves
	// which ranked first.
	s.Runners = []Runner{{ID: "r1", Provider: "fake", Capacity: 1}}
	s.Requirements = []Requirement{
		// legacy-only: high scalar Priority, no Assessment.
		{ID: "legacy-high", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 90, CreatedAt: s.Now},
		// multi-factor: low scalar Priority (irrelevant once Assessment is
		// set) but a strong assessment that must still outrank legacy-high.
		{
			ID: "assessed-strong", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 1, CreatedAt: s.Now,
			Assessment: &domain.PriorityAssessment{
				Version: 3, ValueScore: 90, UrgencyScore: 90, RiskScore: 50, DependencyScore: 20, LearningScore: 10, ResourceCost: 5, StarvationRisk: 0,
				Executable: true, Reason: "top-priority security fix", ReevaluateWhen: "on next incident report",
			},
		},
	}
	plan, err := Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	ids := decisionOrderedIDs(t, plan.Decisions)
	if ids[0] != "assessed-strong" {
		t.Fatalf("multi-factor assessment did not drive ranking above legacy priority: order=%v", ids)
	}

	// The one Runner must have gone to the higher-ranked candidate.
	if len(plan.Assignments) != 1 || plan.Assignments[0].RequirementID != "assessed-strong" {
		t.Fatalf("expected assessed-strong to win the single runner slot: %#v", plan.Assignments)
	}
}

func TestAssessmentExecutableFalseExcludesFromAssignment(t *testing.T) {
	s := testSnapshot()
	s.Requirements = []Requirement{
		{
			ID: "blocked", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, CreatedAt: s.Now,
			Assessment: &domain.PriorityAssessment{Version: 1, ValueScore: 100, UrgencyScore: 100, Executable: false, Reason: "waiting on legal review"},
		},
	}
	plan, err := Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assignments) != 0 {
		t.Fatalf("Executable=false candidate was assigned: %#v", plan.Assignments)
	}
	if len(plan.Decisions) != 1 {
		t.Fatalf("expected exactly one Decision, got %d", len(plan.Decisions))
	}
	d := plan.Decisions[0]
	if d.Assigned {
		t.Fatalf("Decision.Assigned should be false for a non-executable candidate: %#v", d)
	}
	if d.Reason != ReasonNotExecutable {
		t.Fatalf("Decision.Reason = %q, want %q", d.Reason, ReasonNotExecutable)
	}
}

func TestAssessmentVersionReasonAndReevaluateWhenCarryThroughToDecision(t *testing.T) {
	s := testSnapshot()
	s.Requirements = []Requirement{
		{
			ID: "carried", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, CreatedAt: s.Now,
			Assessment: &domain.PriorityAssessment{
				Version: 7, ValueScore: 40, UrgencyScore: 40, Executable: true,
				Reason: "customer-reported outage", ReevaluateWhen: "after the next status page update",
			},
		},
	}
	plan, err := Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Decisions) != 1 {
		t.Fatalf("expected exactly one Decision, got %d", len(plan.Decisions))
	}
	d := plan.Decisions[0]
	if d.AssessmentVersion != 7 {
		t.Fatalf("AssessmentVersion = %d, want 7", d.AssessmentVersion)
	}
	if d.AssessmentReason != "customer-reported outage" {
		t.Fatalf("AssessmentReason = %q, want %q", d.AssessmentReason, "customer-reported outage")
	}
	if d.AssessmentReevaluateWhen != "after the next status page update" {
		t.Fatalf("AssessmentReevaluateWhen = %q, want %q", d.AssessmentReevaluateWhen, "after the next status page update")
	}
}
