package scheduler

import (
	"errors"
	"testing"
	"time"
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
