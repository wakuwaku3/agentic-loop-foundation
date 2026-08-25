package scheduler

// V2-030 / A5: starvation prevention is a different property from failure-
// storm isolation. TestDecidePreventsDoubleOwnerAndIsolatesFailureStorm (in
// scheduler_test.go) proves that an isolated, unhealthy Repository is
// excluded and a healthy one stays schedulable -- that is isolation. This
// file proves the M7 condition that is genuinely still open: one healthy
// Repository's mass demand (a flood of same-priority requirements and
// retries) must not keep occupying all runner capacity forever, starving an
// important Requirement waiting on a second, equally healthy Repository.
//
// The worked numbers, so the bound is derived rather than tuned to fit:
//
//   - The flood: 20 Requirements in repo-flood, Priority=100 (clamped),
//     CreatedAt fixed at the scenario's start. Since every candidate in one
//     Decide call is scored against the same s.Now, and every one of these
//     Requirements shares the same CreatedAt, the age term is identical
//     across the whole scenario and cancels out of every comparison; the
//     flood's score is therefore pinned at legacyScore(100, age) =
//     100*300 + age = 30000 + age for as long as the scenario runs.
//   - The waiter: one Requirement in repo-important with a
//     domain.PriorityAssessment carrying ValueScore=50, UrgencyScore=10, and
//     everything else zero. Its static score (StarvationRisk=0) is
//     50*400 + 10*350 = 23500 -- a real, fixed deficit of 6500 against the
//     flood's 30000, plus the identical age term (also cancels: same
//     CreatedAt).
//   - StarvationRisk is raised by +5 on every tick the waiter is not
//     assigned, representing a re-assessment triggered by the wait itself.
//     At weightStarvationRisk=500 (priority.go), each +5 step adds 2500 to
//     the waiter's score: tick 1 uses StarvationRisk=0 (23500, loses), tick 2
//     uses 5 (26000, loses), tick 3 uses 10 (28500, loses), tick 4 uses 15
//     (31000, beats the flood's 30000 and wins the only Runner).
//   - StarvationBoundTicks = 5 (declared in scheduler.go) therefore gives a
//     one-tick margin over the tick on which convergence is actually
//     measured below.
//
// Only one Runner (capacity 1) exists, so at most one candidate can be
// assigned per tick -- capacity is provably too small to serve the flood and
// the waiter in the same tick. There is no sleep, timer, or goroutine: each
// tick is a plain loop iteration with an explicitly advanced Snapshot.Now
// (an injected clock), and every flood item the tick did NOT pick is left
// untouched while the one it DID pick is reset back to StatusReady before
// the next tick, simulating an immediate, indefinite retry.

import (
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

const starvationFloodSize = 20

func starvationScenario(now time.Time) Snapshot {
	s := Snapshot{
		Now:          now,
		Repositories: []Repository{{ID: "repo-flood"}, {ID: "repo-important"}},
		Runners:      []Runner{{ID: "only-runner", Provider: "fake", Capacity: 1}},
	}
	reqs := make([]Requirement, 0, starvationFloodSize+1)
	for i := 0; i < starvationFloodSize; i++ {
		reqs = append(reqs, Requirement{
			ID:            floodID(i),
			RepositoryIDs: []string{"repo-flood"},
			Status:        StatusReady,
			Priority:      100,
			CreatedAt:     now,
		})
	}
	reqs = append(reqs, Requirement{
		ID:            "important",
		RepositoryIDs: []string{"repo-important"},
		Status:        StatusReady,
		CreatedAt:     now,
		Assessment: &domain.PriorityAssessment{
			Version:        1,
			ValueScore:     50,
			UrgencyScore:   10,
			StarvationRisk: 0,
			Executable:     true,
			Reason:         "cross-team high-value ask waiting behind a same-repository flood",
			ReevaluateWhen: "next reconcile tick",
		},
	})
	s.Requirements = reqs
	return s
}

func floodID(i int) string {
	// Fixed width so lexical and numeric ordering agree; irrelevant to the
	// property being proven (any deterministic tie-break is fine), but
	// keeps failure output readable.
	const digits = "0123456789"
	tens, ones := i/10, i%10
	return "flood-" + string(digits[tens]) + string(digits[ones])
}

// runStarvationTicks drives Decide/Apply for up to maxTicks ticks over an
// injected clock (s.Now advances by one tickStep per tick; no sleep, no
// timer, no goroutine). On every tick that "important" is not assigned, its
// StarvationRisk assessment is raised by starvationRiskStep to model a
// re-assessment triggered by the wait, and any flood item assigned that tick
// is reset to StatusReady before the next tick, modeling an immediate,
// indefinite retry. It returns the tick number (1-indexed) on which
// "important" was first assigned, or 0 if it was never assigned within
// maxTicks.
func runStarvationTicks(t *testing.T, s Snapshot, maxTicks int, tickStep time.Duration, starvationRiskStep int) int {
	t.Helper()
	for tick := 1; tick <= maxTicks; tick++ {
		s.Now = s.Now.Add(tickStep)
		plan, err := Decide(s)
		if err != nil {
			t.Fatalf("tick %d: Decide: %v", tick, err)
		}
		next, err := Apply(s, plan)
		if err != nil {
			t.Fatalf("tick %d: Apply: %v", tick, err)
		}
		s = next
		for _, a := range plan.Assignments {
			if a.RequirementID == "important" {
				return tick
			}
		}
		// Reset whichever flood item(s) got assigned back to Ready, and
		// raise "important"'s StarvationRisk for the next re-assessment.
		for i := range s.Requirements {
			q := &s.Requirements[i]
			if q.ID != "important" && q.Status == StatusAssigned {
				q.Status = StatusReady
			}
			if q.ID == "important" && q.Assessment != nil {
				q.Assessment.StarvationRisk += starvationRiskStep
			}
		}
	}
	return 0
}

func TestStarvationBoundAssignsWaitingHighValueRequirementWithinBoundTicks(t *testing.T) {
	s := starvationScenario(time.Unix(3_000_000, 0))
	assignedOnTick := runStarvationTicks(t, s, StarvationBoundTicks, time.Second, 5)
	if assignedOnTick == 0 {
		t.Fatalf("waiting requirement 'important' was never assigned within the %d-tick starvation bound", StarvationBoundTicks)
	}
	t.Logf("'important' was assigned on tick %d (bound=%d)", assignedOnTick, StarvationBoundTicks)
}

// TestStarvationNegativeControlFailsToConvergeWithoutStarvationRiskTerm
// neutralises weightStarvationRisk (by swapping scoreFn to a variant that
// omits it) and re-runs the IDENTICAL scenario, including the identical
// per-tick StarvationRisk increments, to prove the positive result above is
// actually caused by that term and not by some other effect (e.g. aging,
// which is a no-op here because every candidate shares one CreatedAt).
func TestStarvationNegativeControlFailsToConvergeWithoutStarvationRiskTerm(t *testing.T) {
	original := scoreFn
	scoreFn = func(q Requirement, now time.Time) int64 {
		age := ageSeconds(q.CreatedAt, now)
		if q.Assessment == nil {
			return legacyScore(q.Priority, age)
		}
		a := *q.Assessment
		// Every term except StarvationRisk, unchanged from multiFactorScore.
		return clampFactor(a.ValueScore)*weightValue +
			clampFactor(a.UrgencyScore)*weightUrgency +
			clampFactor(a.RiskScore)*weightRisk +
			clampFactor(a.DependencyScore)*weightDependency +
			clampFactor(a.LearningScore)*weightLearning -
			clampFactor(a.ResourceCost)*weightResourceCost +
			age
	}
	defer func() { scoreFn = original }()

	s := starvationScenario(time.Unix(3_000_000, 0))
	assignedOnTick := runStarvationTicks(t, s, StarvationBoundTicks, time.Second, 5)
	if assignedOnTick != 0 {
		t.Fatalf("negative control converged on tick %d even with the StarvationRisk term neutralised; the positive result is not actually caused by that term", assignedOnTick)
	}

	// Extend well past the bound to show this is genuine non-convergence,
	// not merely a slower convergence that the chosen bound happens to miss:
	// the deficit (6500) can only be closed by age, which is a no-op here
	// (every candidate shares CreatedAt == the scenario's start), so no
	// number of ticks converges.
	stillNotAssigned := runStarvationTicks(t, s, StarvationBoundTicks*20, time.Second, 5)
	if stillNotAssigned != 0 {
		t.Fatalf("negative control eventually converged on tick %d after extending well past the bound; expected permanent non-convergence", stillNotAssigned)
	}
}
