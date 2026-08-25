package scheduler

import (
	"errors"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

func testSnapshot() Snapshot {
	now := time.Unix(2_000_000, 0)
	return Snapshot{
		Now:          now,
		Repositories: []Repository{{ID: "repo-a"}, {ID: "repo-b"}},
		Runners:      []Runner{{ID: "runner-a", Provider: "codex", Capacity: 1}, {ID: "runner-b", Provider: "fake", Capacity: 1}},
	}
}

func TestDecideCrossRepositoryUsesOneRunnerAndApplyIsAtomic(t *testing.T) {
	s := testSnapshot()
	s.Requirements = []Requirement{{ID: "req-1", RepositoryIDs: []string{"repo-a", "repo-b"}, Status: StatusReady, CreatedAt: s.Now.Add(-time.Hour), Resources: []ResourceRequest{{Name: "workspace", Mode: Write}}}, {ID: "req-2", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, CreatedAt: s.Now.Add(-time.Hour)}}
	plan, err := Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assignments) != 3 || plan.Assignments[0].RunnerID != plan.Assignments[1].RunnerID || plan.Assignments[2].RunnerID == plan.Assignments[0].RunnerID {
		t.Fatalf("expected one runner across repos: %#v", plan.Assignments)
	}
	next, err := Apply(s, plan)
	if err != nil {
		t.Fatal(err)
	}
	if next.Requirements[0].Status != StatusAssigned || next.Requirements[1].Status != StatusAssigned || len(next.Claims) != 2 {
		t.Fatalf("unexpected applied snapshot: %#v", next)
	}

	partial := Plan{Assignments: plan.Assignments[:1]}
	rolled, err := Apply(s, partial)
	if !errors.Is(err, ErrAtomicPlan) {
		t.Fatalf("partial plan error = %v", err)
	}
	if len(rolled.Claims) != 0 || rolled.Requirements[0].Status != StatusReady {
		t.Fatalf("partial plan changed state: %#v", rolled)
	}
}

func TestDecidePreventsDoubleOwnerAndIsolatesFailureStorm(t *testing.T) {
	s := testSnapshot()
	s.Claims = []Claim{{Resource: "slot", Owner: "req-1", RepositoryID: "repo-a", Mode: Write}}
	s.Requirements = []Requirement{{ID: "req-1", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, CreatedAt: s.Now, Resources: []ResourceRequest{{Name: "slot", Mode: Write}}}}
	plan, err := Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assignments) != 0 {
		t.Fatalf("double owner was scheduled: %#v", plan)
	}

	s.Claims = nil
	s.Repositories[1].FailureCount = FailureStormThreshold
	s.Requirements = []Requirement{{ID: "cross", RepositoryIDs: []string{"repo-a", "repo-b"}, Status: StatusReady, CreatedAt: s.Now}}
	plan, err = Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assignments) != 0 {
		t.Fatalf("storm-isolated repo was scheduled: %#v", plan)
	}
	s.Requirements[0].RepositoryIDs = []string{"repo-a"}
	plan, err = Decide(s)
	if err != nil || len(plan.Assignments) != 1 {
		t.Fatalf("healthy repo should remain schedulable: plan=%#v err=%v", plan, err)
	}
}

func TestDecideReadSharingWriteExclusionAndProviderCapacity(t *testing.T) {
	s := testSnapshot()
	s.Runners = []Runner{{ID: "r1", Provider: "fake", Capacity: 1}, {ID: "r2", Provider: "fake", Capacity: 1}}
	s.ProviderCapacity = map[string]int{"fake": 1}
	s.Claims = []Claim{{Resource: "cache", RepositoryID: "repo-a", Mode: Read}}
	s.Requirements = []Requirement{
		{ID: "read", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, CreatedAt: s.Now, Resources: []ResourceRequest{{Name: "cache", Mode: Read}}},
		{ID: "write", RepositoryIDs: []string{"repo-b"}, Status: StatusReady, CreatedAt: s.Now, Resources: []ResourceRequest{{Name: "cache", Mode: Write}}},
	}
	plan, err := Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assignments) != 1 || plan.Assignments[0].RequirementID != "read" {
		t.Fatalf("read sharing/provider capacity failed: %#v", plan)
	}

	s.Claims = []Claim{{Resource: "cache", RepositoryID: "repo-a", Mode: Write}}
	s.Requirements = []Requirement{{ID: "read", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, CreatedAt: s.Now, Resources: []ResourceRequest{{Name: "cache", Mode: Read}}}}
	plan, err = Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assignments) != 0 {
		t.Fatalf("write claim did not exclude read: %#v", plan)
	}
}

func TestDecideDependencyDAGAndAging(t *testing.T) {
	s := testSnapshot()
	s.Runners = []Runner{{ID: "r", Provider: "fake", Capacity: 1}}
	s.Requirements = []Requirement{
		{ID: "dependent", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, CreatedAt: s.Now.Add(-time.Hour), Dependencies: []string{"base"}},
		{ID: "base", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, CreatedAt: s.Now.Add(-2 * time.Hour)},
	}
	plan, err := Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assignments) != 1 || plan.Assignments[0].RequirementID != "base" {
		t.Fatalf("dependency ordering failed: %#v", plan)
	}

	s.Requirements = []Requirement{{ID: "a", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Dependencies: []string{"b"}}, {ID: "b", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Dependencies: []string{"a"}}}
	if _, err := Decide(s); !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestDecideBoundsCandidateInput(t *testing.T) {
	s := testSnapshot()
	s.CandidateLimit = 1
	s.Requirements = []Requirement{{ID: "one"}, {ID: "two"}}
	if _, err := Decide(s); !errors.Is(err, ErrCandidateLimit) {
		t.Fatalf("limit error = %v", err)
	}
}

// TestDecideRecordsExactlyOneReasonPerRejectedCandidateAndReasonSetIsExhaustive
// is V2-030 / A4's table test. It builds one Snapshot containing exactly one
// scenario per member of the closed Reason set plus one assignable
// candidate, then asserts: (a) every considered candidate has a Decision
// record, (b) every unassigned candidate has exactly one non-empty Reason,
// (c) the assigned candidate has none, and (d) the set of Reasons actually
// observed is exactly AllReasons -- so this test fails if a Reason constant
// is ever added without a corresponding scenario here, or if a scenario's
// expected reason is not a member of AllReasons.
func TestDecideRecordsExactlyOneReasonPerRejectedCandidateAndReasonSetIsExhaustive(t *testing.T) {
	s := testSnapshot()
	s.Runners = []Runner{{ID: "only-runner", Provider: "fake", Capacity: 1}}
	s.Repositories = []Repository{
		{ID: "repo-a"},
		{ID: "repo-storm", FailureCount: FailureStormThreshold},
	}
	s.Claims = []Claim{
		{Resource: "whatever", Owner: "already-owned", RepositoryID: "repo-a", Mode: Read},
		{Resource: "res-conflict", Owner: "someone-else", RepositoryID: "repo-a", Mode: Write},
	}
	s.Requirements = []Requirement{
		// Assigned: highest score, wins the only runner. Reason == "".
		{ID: "assigned-ok", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 100, CreatedAt: s.Now, Resources: []ResourceRequest{{Name: "res-ok", Mode: Write}}},
		// ReasonNoRunnerCapacity: otherwise fully eligible, but the single
		// runner is already taken by assigned-ok (which sorts first).
		{ID: "no-capacity", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 90, CreatedAt: s.Now},
		// ReasonNotReady: wrong status.
		{ID: "not-ready", RepositoryIDs: []string{"repo-a"}, Status: StatusAssigned, Priority: 10, CreatedAt: s.Now},
		// ReasonUnmetDependency: dependency never completes.
		{ID: "unmet-dep", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 10, CreatedAt: s.Now, Dependencies: []string{"missing-dep"}},
		// ReasonNotExecutable: assessment explicitly says not executable.
		{ID: "not-executable", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 10, CreatedAt: s.Now, Assessment: &domain.PriorityAssessment{Executable: false, Reason: "blocked on legal review"}},
		// ReasonRepositoryUnavailable: bound to a storm-isolated repository.
		{ID: "repo-unavailable", RepositoryIDs: []string{"repo-storm"}, Status: StatusReady, Priority: 10, CreatedAt: s.Now},
		// ReasonAlreadyOwned: an existing claim's Owner matches this ID.
		{ID: "already-owned", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 10, CreatedAt: s.Now},
		// ReasonResourceConflict: an existing Write claim on the same
		// resource and repository, owned by someone else.
		{ID: "resource-conflict", RepositoryIDs: []string{"repo-a"}, Status: StatusReady, Priority: 10, CreatedAt: s.Now, Resources: []ResourceRequest{{Name: "res-conflict", Mode: Write}}},
	}

	want := map[string]Reason{
		"assigned-ok":       "",
		"no-capacity":       ReasonNoRunnerCapacity,
		"not-ready":         ReasonNotReady,
		"unmet-dep":         ReasonUnmetDependency,
		"not-executable":    ReasonNotExecutable,
		"repo-unavailable":  ReasonRepositoryUnavailable,
		"already-owned":     ReasonAlreadyOwned,
		"resource-conflict": ReasonResourceConflict,
	}

	plan, err := Decide(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Decisions) != len(s.Requirements) {
		t.Fatalf("expected one Decision per considered candidate: got %d for %d requirements", len(plan.Decisions), len(s.Requirements))
	}

	byID := map[string]Decision{}
	for _, d := range plan.Decisions {
		byID[d.RequirementID] = d
	}
	observedReasons := map[Reason]bool{}
	for id, wantReason := range want {
		d, ok := byID[id]
		if !ok {
			t.Fatalf("no Decision recorded for candidate %q", id)
		}
		if wantReason == "" {
			if !d.Assigned {
				t.Fatalf("%s: expected Assigned=true, got Decision=%#v", id, d)
			}
			if d.Reason != "" {
				t.Fatalf("%s: expected empty Reason for an assigned candidate, got %q", id, d.Reason)
			}
			continue
		}
		if d.Assigned {
			t.Fatalf("%s: expected Assigned=false, got Decision=%#v", id, d)
		}
		if d.Reason != wantReason {
			t.Fatalf("%s: Reason = %q, want %q", id, d.Reason, wantReason)
		}
		if d.Reason == "" {
			t.Fatalf("%s: unassigned candidate has an empty Reason", id)
		}
		observedReasons[d.Reason] = true
	}

	allReasonsSet := map[Reason]bool{}
	for _, r := range AllReasons {
		allReasonsSet[r] = true
	}
	for r := range observedReasons {
		if !allReasonsSet[r] {
			t.Fatalf("scenario produced Reason %q, which is not a member of AllReasons", r)
		}
	}
	for _, r := range AllReasons {
		if !observedReasons[r] {
			t.Fatalf("AllReasons constant %q has no covering scenario in this table test", r)
		}
	}
	if len(observedReasons) != len(AllReasons) {
		t.Fatalf("observed %d distinct reasons, want exactly %d (AllReasons): observed=%v", len(observedReasons), len(AllReasons), observedReasons)
	}
}

// TestDecideGlobalResourceScopeExcludesAcrossRepositoriesButNotScoped is
// V2-030 / A7: ResourceRequest.Global had no test at all before this. It
// asserts three directions and the repository id actually recorded on each
// planned Claim (empty for global, the repository id otherwise).
func TestDecideGlobalResourceScopeExcludesAcrossRepositoriesButNotScoped(t *testing.T) {
	t.Run("a global write claim excludes a same-named global request from a different repository", func(t *testing.T) {
		s := testSnapshot()
		s.Claims = []Claim{{Resource: "license", Owner: "holder", RepositoryID: "", Mode: Write}}
		s.Requirements = []Requirement{{ID: "req", RepositoryIDs: []string{"repo-b"}, Status: StatusReady, CreatedAt: s.Now, Resources: []ResourceRequest{{Name: "license", Mode: Write, Global: true}}}}
		plan, err := Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Assignments) != 0 {
			t.Fatalf("global write claim did not exclude a cross-repository global request: %#v", plan)
		}
	})

	t.Run("a global read shares with a global read", func(t *testing.T) {
		s := testSnapshot()
		s.Claims = []Claim{{Resource: "catalog", Owner: "holder", RepositoryID: "", Mode: Read}}
		s.Requirements = []Requirement{{ID: "req", RepositoryIDs: []string{"repo-b"}, Status: StatusReady, CreatedAt: s.Now, Resources: []ResourceRequest{{Name: "catalog", Mode: Read, Global: true}}}}
		plan, err := Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Assignments) != 1 {
			t.Fatalf("global read did not share with an existing global read: %#v", plan)
		}
		if len(plan.Claims) != 1 || plan.Claims[0].RepositoryID != "" {
			t.Fatalf("global claim did not record an empty RepositoryID: %#v", plan.Claims)
		}
	})

	t.Run("a repository-scoped claim does not exclude a request in another repository", func(t *testing.T) {
		s := testSnapshot()
		s.Claims = []Claim{{Resource: "workspace", Owner: "holder", RepositoryID: "repo-a", Mode: Write}}
		s.Requirements = []Requirement{{ID: "req", RepositoryIDs: []string{"repo-b"}, Status: StatusReady, CreatedAt: s.Now, Resources: []ResourceRequest{{Name: "workspace", Mode: Write}}}}
		plan, err := Decide(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Assignments) != 1 {
			t.Fatalf("repository-scoped claim wrongly excluded a request in a different repository: %#v", plan)
		}
		if len(plan.Claims) != 1 || plan.Claims[0].RepositoryID != "repo-b" {
			t.Fatalf("repository-scoped claim did not record its own repository id: %#v", plan.Claims)
		}
	})
}
