package scheduler

// V2-030 / A8: validation.md section 5 fixes one reconcile tick at 100 items
// within 30 seconds. MaxCandidates already bounds the count to 100, but
// nothing measured the wall-clock budget until now. The deadline is the
// assertion; the measured duration is logged as an observation only, never
// asserted as a threshold (Work Order constraint: no timing number may
// become a flaky assertion). No goroutine is used to enforce the context
// deadline: Decide/Apply are synchronous, so the deadline is checked after
// the synchronous call returns, which is enough to prove the real elapsed
// wall-clock time did not exceed it.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// budgetSnapshot builds n candidates spread over at least two Repositories
// and at least two Runner records, as A8 requires.
func budgetSnapshot(n int) Snapshot {
	now := time.Unix(4_000_000, 0)
	s := Snapshot{
		Now: now,
		Repositories: []Repository{
			{ID: "repo-a"}, {ID: "repo-b"}, {ID: "repo-c"}, {ID: "repo-d"},
		},
		Runners: []Runner{
			{ID: "r1", Provider: "fake", Capacity: 25},
			{ID: "r2", Provider: "fake", Capacity: 25},
			{ID: "r3", Provider: "fake", Capacity: 25},
			{ID: "r4", Provider: "fake", Capacity: 25},
		},
	}
	repos := []string{"repo-a", "repo-b", "repo-c", "repo-d"}
	reqs := make([]Requirement, 0, n)
	for i := 0; i < n; i++ {
		reqs = append(reqs, Requirement{
			ID:            budgetID(i),
			RepositoryIDs: []string{repos[i%len(repos)]},
			Status:        StatusReady,
			Priority:      int64(i % 100),
			CreatedAt:     now.Add(-time.Duration(i) * time.Second),
		})
	}
	s.Requirements = reqs
	return s
}

func budgetID(i int) string {
	digits := "0123456789"
	out := []byte{'c', 'a', 'n', 'd', '-'}
	if i >= 100 {
		out = append(out, digits[(i/100)%10])
	}
	out = append(out, digits[(i/10)%10], digits[i%10])
	return string(out)
}

// decideApply runs one full Decide+Apply cycle and returns the wall-clock
// duration it took, as a real measurement (not a fixed sleep).
func decideApplyTimed(t *testing.T, s Snapshot) (time.Duration, Plan) {
	t.Helper()
	start := time.Now()
	plan, err := Decide(s)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := Apply(s, plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return time.Since(start), plan
}

func TestReconcileTickCompletesWithin30SecondBudgetAt100Candidates(t *testing.T) {
	s := budgetSnapshot(100)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	elapsed, plan := decideApplyTimed(t, s)

	if err := ctx.Err(); err != nil {
		t.Fatalf("the 30 second reconcile budget was exceeded: %v (measured %s)", err, elapsed)
	}
	if len(plan.Decisions) != 100 {
		t.Fatalf("expected 100 Decisions for 100 candidates, got %d", len(plan.Decisions))
	}
	t.Logf("observation: 100 candidates, Decide+Apply took %s (budget 30s, not asserted as a threshold)", elapsed)
}

func TestReconcileTickDurationObservationsAt10And50Candidates(t *testing.T) {
	for _, n := range []int{10, 50, 100} {
		n := n
		t.Run(budgetID(n), func(t *testing.T) {
			s := budgetSnapshot(n)
			elapsed, plan := decideApplyTimed(t, s)
			if len(plan.Decisions) != n {
				t.Fatalf("expected %d Decisions, got %d", n, len(plan.Decisions))
			}
			// Observation only; deliberately not compared against any
			// threshold, per the Work Order's determinism constraint.
			t.Logf("observation: %d candidates, Decide+Apply took %s", n, elapsed)
		})
	}
}

func TestReconcileTickFailsClosedAboveCandidateLimit(t *testing.T) {
	t.Run("101 candidates with the default MaxCandidates=100 limit", func(t *testing.T) {
		s := budgetSnapshot(101)
		if _, err := Decide(s); !errors.Is(err, ErrCandidateLimit) {
			t.Fatalf("Decide error = %v, want ErrCandidateLimit", err)
		}
	})
	t.Run("CandidateLimit+1 with an explicit CandidateLimit=100", func(t *testing.T) {
		s := budgetSnapshot(101)
		s.CandidateLimit = 100
		if _, err := Decide(s); !errors.Is(err, ErrCandidateLimit) {
			t.Fatalf("Decide error = %v, want ErrCandidateLimit", err)
		}
	})
}
