package scheduler

// V2-073 A7/A8/A9: the interval of capture-time spread inside which V2-030's
// starvation bound is a true statement, derived from the declared numbers and
// then measured at both endpoints.
//
// Why this file exists. starvation_test.go proves the M7 starvation bound with
// every candidate sharing one CreatedAt, and says so three times in its own
// comments: "every one of these Requirements shares the same CreatedAt, the
// age term is identical across the whole scenario and cancels out of every
// comparison", and the negative control's closing note that "the deficit
// (6500) can only be closed by age, which is a no-op here". The cancellation
// is what the proof rests on. V2-073 gives a Requirement a real recorded
// capture time, so in production the cancellation stops holding, and the
// bound becomes a conditional statement. This file states the condition.
//
// THE DERIVATION, from the declared numbers only.
//
// Let D be the number of seconds by which the flood candidates' capture time
// PRECEDES the waiter's, so age_flood == age_waiter + D at every tick. From
// starvation_test.go's declared scenario and priority.go's declared weights:
//
//	flood score  = legacyPriority(100)*300 + age_flood
//	             = 30000 + age_waiter + D
//	waiter score = ValueScore(50)*weightValue(400)
//	             + UrgencyScore(10)*weightUrgency(350)
//	             + StarvationRisk*weightStarvationRisk(500) + age_waiter
//	             = 23500 + 2500*(tick-1) + age_waiter
//
// because StarvationRisk is raised by +5 on each tick the waiter is not
// assigned, and 5*500 == 2500 per step, with tick 1 using StarvationRisk == 0.
//
// scheduler.go's comparator (the sort.SliceStable less function in Decide) is
// "a > b || (a == b && items[i].ID < items[j].ID)". Every flood id begins
// "flood-" and the waiter's id is "important"; "flood-" sorts before
// "important", so on a tie the flood wins the only Runner. The waiter must
// therefore be STRICTLY greater:
//
//	23500 + 2500*(tick-1) + age_waiter > 30000 + age_waiter + D
//	                    2500*(tick-1) > 6500 + D
//
// With StarvationBoundTicks == 5 (scheduler.go), convergence inside the bound
// needs tick <= 5, i.e. 2500*4 = 10000 > 6500 + D, i.e. D < 3500, i.e.
//
//	D <= captureSpreadCeilingSeconds == 3499.
//
// The floor comes from the negative control, not from the bound. With the
// StarvationRisk term neutralised the waiter scores a flat 23500 + age_waiter,
// so it stays behind the flood only while 23500 <= 30000 + D, i.e.
// D >= -6500. At D == -6501 the negative control CONVERGES, and the proof
// stops attributing convergence to the StarvationRisk term at all:
//
//	D >= captureSpreadFloorSeconds == -6500.
//
// So V2-030's two starvation tests are true statements exactly for
// D in [-6500, +3499] seconds, and outside that interval one of them is
// false. Separately, the "one-tick margin" scheduler.go's StarvationBoundTicks
// comment records needs convergence by tick 4, i.e. 2500*3 = 7500 > 6500 + D,
// i.e. D < 1000, i.e. D <= captureSpreadMarginCeilingSeconds == 999. From
// 1000 to 3499 convergence lands on the bound itself with no margin at all.
//
// A9 / ESCALATION, RECORDED AND NOT FIXED. Nothing in production bounds D.
// A Requirement's capture time is whatever instant its intake transaction
// happened at, so a flood whose candidates were captured more than 3499
// seconds (58 minutes 19 seconds) before the waiter exceeds the declared
// bound in production, and this file's D == 3500 case measures exactly that.
// Every available remedy -- raising the StarvationRisk step, normalizing or
// capping the age term, raising StarvationBoundTicks -- is a change to a
// scheduler decision rule, which non_goal 3 of V2-073 forbids. The remedy is
// therefore NOT applied here: the tech_lead owns the follow-up decision about
// what production should guarantee about capture-time spread, and V2-068
// (which owns the Snapshot builder) is where a chosen guarantee would be
// enforced. Nothing below is weakened, relaxed or reworded to make the limit
// disappear; the limit is the finding.
//
// WHAT THIS FILE DOES NOT DO. It does not edit starvation_test.go, reuse-copy
// or modify starvationScenario, touch priority.go, decision.go or
// scheduler.go, change any weight or clamp, or change StarvationBoundTicks.
// It reuses starvation_test.go's runStarvationTicks driver and floodID
// unchanged, so the only difference between the scenario below and V2-030's is
// the capture times.
//
// DETERMINISM. No sleep, no wall-clock timer, no goroutine: every tick is a
// loop iteration over an explicitly advanced Snapshot.Now, and every capture
// time is derived from that same injected instant.

import (
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// The interval, declared as constants with the derivation above. These are
// arithmetic consequences of numbers declared elsewhere (priority.go's
// weights, starvation_test.go's scenario, scheduler.go's
// StarvationBoundTicks); they are not tuned, and
// TestCaptureSpreadIntervalIsDerivedFromTheDeclaredNumbers below recomputes
// each one from those numbers so a weight change cannot leave them stale.
const (
	// captureSpreadFloorSeconds is the most negative D (flood captured AFTER
	// the waiter) for which the negative control still fails to converge.
	captureSpreadFloorSeconds = -6500
	// captureSpreadCeilingSeconds is the largest D (flood captured BEFORE the
	// waiter) for which the waiter is still assigned within
	// StarvationBoundTicks.
	captureSpreadCeilingSeconds = 3499
	// captureSpreadMarginCeilingSeconds is the largest D for which the
	// one-tick margin scheduler.go's StarvationBoundTicks comment records
	// actually exists.
	captureSpreadMarginCeilingSeconds = 999
	// captureSpreadWaiterDeficit is the waiter's fixed score deficit against
	// the flood at StarvationRisk == 0: 30000 - 23500.
	captureSpreadWaiterDeficit = 6500
	// captureSpreadRiskStepScore is one StarvationRisk step's contribution:
	// starvation_test.go raises StarvationRisk by 5 per tick and
	// weightStarvationRisk is 500.
	captureSpreadRiskStep      = 5
	captureSpreadRiskStepScore = captureSpreadRiskStep * weightStarvationRisk
	// captureSpreadBaseAgeSeconds is how far before the scenario's start
	// instant the waiter was captured. It exists only so that BOTH candidates
	// have a strictly positive age at every tick for every D in the table
	// below: ageSeconds clamps a negative age to zero, and a clamped age
	// would silently replace the arithmetic above with a different one. It
	// must exceed the largest absolute D tested.
	captureSpreadBaseAgeSeconds = 100_000
)

// captureSpreadScenario is starvation_test.go's scenario with exactly one
// difference: the flood's capture time precedes the waiter's by
// spreadSeconds. starvationScenario itself is deliberately not called and
// not modified -- it pins V2-030's byte-identical shared-CreatedAt ordering,
// which this file must not disturb.
func captureSpreadScenario(now time.Time, spreadSeconds int64) Snapshot {
	waiterCapturedAt := now.Add(-time.Duration(captureSpreadBaseAgeSeconds) * time.Second)
	floodCapturedAt := waiterCapturedAt.Add(-time.Duration(spreadSeconds) * time.Second)
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
			CreatedAt:     floodCapturedAt,
		})
	}
	reqs = append(reqs, Requirement{
		ID:            "important",
		RepositoryIDs: []string{"repo-important"},
		Status:        StatusReady,
		CreatedAt:     waiterCapturedAt,
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

// captureSpreadStart is the scenario's start instant, the same literal
// starvation_test.go uses. It is an injected value, never a clock read.
func captureSpreadStart() time.Time { return time.Unix(3_000_000, 0) }

// firstTickSatisfying returns the smallest tick k >= 1 with
// 2500*(k-1) > 6500 + d, or 0 if no k up to limit satisfies it. This is the
// derivation restated as code so the expected ticks in the table below are
// computed from the declared numbers rather than typed in by hand.
func firstTickSatisfying(d int64, limit int) int {
	for k := 1; k <= limit; k++ {
		if int64(k-1)*captureSpreadRiskStepScore > captureSpreadWaiterDeficit+d {
			return k
		}
	}
	return 0
}

// TestCaptureSpreadIntervalIsDerivedFromTheDeclaredNumbers recomputes the
// three declared constants from priority.go's weights and scheduler.go's
// bound, so a weight change that invalidates the interval fails here rather
// than leaving a stale comment behind.
func TestCaptureSpreadIntervalIsDerivedFromTheDeclaredNumbers(t *testing.T) {
	// The deficit is the flood's fixed score minus the waiter's, both from
	// the declared weights.
	flood := legacyPriority(100) * 300
	waiter := int64(50)*weightValue + int64(10)*weightUrgency
	if flood-waiter != captureSpreadWaiterDeficit {
		t.Fatalf("deficit = %d, declared %d; a weight changed and the interval is stale", flood-waiter, captureSpreadWaiterDeficit)
	}
	if captureSpreadRiskStepScore != 2500 {
		t.Fatalf("one StarvationRisk step = %d, want 2500", int64(captureSpreadRiskStepScore))
	}
	// Ceiling: the largest D still converging by StarvationBoundTicks.
	ceiling := int64(StarvationBoundTicks-1)*captureSpreadRiskStepScore - captureSpreadWaiterDeficit - 1
	if ceiling != captureSpreadCeilingSeconds {
		t.Fatalf("derived ceiling = %d, declared %d", ceiling, int64(captureSpreadCeilingSeconds))
	}
	// Margin ceiling: the largest D still converging one tick early.
	margin := int64(StarvationBoundTicks-2)*captureSpreadRiskStepScore - captureSpreadWaiterDeficit - 1
	if margin != captureSpreadMarginCeilingSeconds {
		t.Fatalf("derived margin ceiling = %d, declared %d", margin, int64(captureSpreadMarginCeilingSeconds))
	}
	// Floor: the waiter's neutralised score is 23500 + age; it stays behind
	// the flood while 23500 <= 30000 + D.
	floor := -(flood - waiter)
	if floor != captureSpreadFloorSeconds {
		t.Fatalf("derived floor = %d, declared %d", floor, int64(captureSpreadFloorSeconds))
	}
	if captureSpreadBaseAgeSeconds <= captureSpreadCeilingSeconds || captureSpreadBaseAgeSeconds <= -(captureSpreadFloorSeconds-1) {
		t.Fatalf("base age %d does not exceed the largest tested spread; ageSeconds would clamp and the arithmetic would silently change", int64(captureSpreadBaseAgeSeconds))
	}
	if StarvationBoundTicks != 5 {
		t.Fatalf("StarvationBoundTicks = %d; this file's derivation is written for 5 and must be re-derived", StarvationBoundTicks)
	}
}

// TestCaptureSpreadConvergesInsideTheDerivedInterval is A8(a) and A8(d): the
// bound holds, and the tick it converges on, for every D across the interval
// including both of its own ends. The expected tick is computed by
// firstTickSatisfying from the declared numbers, so this is a measurement
// against the derivation rather than against a hand-written number.
func TestCaptureSpreadConvergesInsideTheDerivedInterval(t *testing.T) {
	for _, d := range []int64{captureSpreadFloorSeconds, -1, 0, captureSpreadMarginCeilingSeconds, 1000, captureSpreadCeilingSeconds} {
		want := firstTickSatisfying(d, StarvationBoundTicks)
		if want == 0 {
			t.Fatalf("D=%d: the derivation says this D does not converge inside the bound, so it does not belong in the in-interval table", d)
		}
		s := captureSpreadScenario(captureSpreadStart(), d)
		got := runStarvationTicks(t, s, StarvationBoundTicks, time.Second, captureSpreadRiskStep)
		if got == 0 {
			t.Fatalf("D=%d: 'important' was never assigned within the %d-tick bound; the derivation predicted tick %d", d, StarvationBoundTicks, want)
		}
		if got != want {
			t.Fatalf("D=%d: converged on tick %d, derivation predicted tick %d", d, got, want)
		}
		margin := StarvationBoundTicks - got
		// A8(d): the one-tick margin exists only below 1000 seconds. This is
		// a measured statement, asserted in both directions.
		if d <= captureSpreadMarginCeilingSeconds && margin < 1 {
			t.Fatalf("D=%d: margin = %d, want at least 1 for a D at or below %d", d, margin, int64(captureSpreadMarginCeilingSeconds))
		}
		if d > captureSpreadMarginCeilingSeconds && margin != 0 {
			t.Fatalf("D=%d: margin = %d, want exactly 0 for a D above %d", d, margin, int64(captureSpreadMarginCeilingSeconds))
		}
		t.Logf("D=%+6d seconds: converged on tick %d of %d, margin %d tick(s)", d, got, StarvationBoundTicks, margin)
	}
}

// TestCaptureSpreadAboveTheCeilingExceedsTheDeclaredBound is A8(b), run as a
// positive control: at one second past the ceiling the waiter is NOT assigned
// inside StarvationBoundTicks, and the tick it would have converged on is
// measured rather than asserted from the comment. This is the finding A9
// escalates; it is not a defect in this test.
func TestCaptureSpreadAboveTheCeilingExceedsTheDeclaredBound(t *testing.T) {
	const d = int64(captureSpreadCeilingSeconds) + 1 // 3500
	if got := firstTickSatisfying(d, StarvationBoundTicks); got != 0 {
		t.Fatalf("D=%d: the derivation says it converges on tick %d inside the bound; the ceiling is wrong", d, got)
	}
	inside := runStarvationTicks(t, captureSpreadScenario(captureSpreadStart(), d), StarvationBoundTicks, time.Second, captureSpreadRiskStep)
	if inside != 0 {
		t.Fatalf("D=%d: 'important' was assigned on tick %d, inside the %d-tick bound; the measured ceiling is higher than %d and the declared interval is wrong", d, inside, StarvationBoundTicks, int64(captureSpreadCeilingSeconds))
	}
	// A fresh scenario, because runStarvationTicks raises StarvationRisk
	// through the shared *domain.PriorityAssessment pointer.
	const extended = StarvationBoundTicks * 4
	beyond := runStarvationTicks(t, captureSpreadScenario(captureSpreadStart(), d), extended, time.Second, captureSpreadRiskStep)
	want := firstTickSatisfying(d, extended)
	if beyond == 0 {
		t.Fatalf("D=%d: never converged even in %d ticks; the derivation predicted tick %d", d, extended, want)
	}
	if beyond != want {
		t.Fatalf("D=%d: converged on tick %d beyond the bound, derivation predicted tick %d", d, beyond, want)
	}
	t.Logf("D=%+6d seconds: NOT assigned within the %d-tick bound; converges on tick %d, which is %d tick(s) past the declared bound", d, StarvationBoundTicks, beyond, beyond-StarvationBoundTicks)
}

// neutralisedScoreFn is the StarvationRisk-free scorer, term for term the
// same substitution starvation_test.go's negative control makes. It is a
// function value here only so both negative-control cases below can install
// it and restore the original via defer.
func neutralisedScoreFn(q Requirement, now time.Time) int64 {
	age := ageSeconds(q.CreatedAt, now)
	if q.Assessment == nil {
		return legacyScore(q.Priority, age)
	}
	a := *q.Assessment
	return clampFactor(a.ValueScore)*weightValue +
		clampFactor(a.UrgencyScore)*weightUrgency +
		clampFactor(a.RiskScore)*weightRisk +
		clampFactor(a.DependencyScore)*weightDependency +
		clampFactor(a.LearningScore)*weightLearning -
		clampFactor(a.ResourceCost)*weightResourceCost +
		age
}

// TestCaptureSpreadBelowTheFloorBreaksTheNegativeControl is A8(c), run as a
// positive control. Below the floor the negative control -- the same scenario
// with the StarvationRisk term neutralised -- converges on its own, so
// V2-030's proof can no longer attribute convergence to that term. The floor
// itself is asserted too, so the endpoint is measured and not assumed.
func TestCaptureSpreadBelowTheFloorBreaksTheNegativeControl(t *testing.T) {
	original := scoreFn
	scoreFn = neutralisedScoreFn
	defer func() { scoreFn = original }()

	// At the floor the negative control still does not converge: the waiter's
	// neutralised score exactly equals the flood's, and the comparator gives
	// a tie to the lexically smaller id, which is always a flood id.
	atFloor := runStarvationTicks(t, captureSpreadScenario(captureSpreadStart(), captureSpreadFloorSeconds), StarvationBoundTicks*4, time.Second, captureSpreadRiskStep)
	if atFloor != 0 {
		t.Fatalf("D=%d: the negative control converged on tick %d at the declared floor; the floor is wrong", int64(captureSpreadFloorSeconds), atFloor)
	}

	// One second below the floor it converges immediately, with the
	// StarvationRisk term still neutralised.
	const below = int64(captureSpreadFloorSeconds) - 1 // -6501
	got := runStarvationTicks(t, captureSpreadScenario(captureSpreadStart(), below), StarvationBoundTicks, time.Second, captureSpreadRiskStep)
	if got == 0 {
		t.Fatalf("D=%d: the negative control did not converge below the declared floor; the floor is wrong", below)
	}
	t.Logf("D=%+6d seconds: negative control (StarvationRisk neutralised) converged on tick %d -- V2-030's proof can no longer attribute convergence to the StarvationRisk term", below, got)
	t.Logf("D=%+6d seconds: negative control still did not converge in %d ticks (the declared floor)", int64(captureSpreadFloorSeconds), StarvationBoundTicks*4)

	if scoreFn == nil {
		t.Fatal("scoreFn was cleared")
	}
}

// TestCaptureSpreadScenarioMatchesV2030WhenTheSpreadIsZero pins that this
// file's scenario builder is V2-030's scenario plus the spread and nothing
// else: at D == 0 both candidates share one capture time exactly as
// starvationScenario's do, and the convergence tick is the one V2-030
// measured.
func TestCaptureSpreadScenarioMatchesV2030WhenTheSpreadIsZero(t *testing.T) {
	s := captureSpreadScenario(captureSpreadStart(), 0)
	if len(s.Requirements) != starvationFloodSize+1 {
		t.Fatalf("candidate count = %d, want %d", len(s.Requirements), starvationFloodSize+1)
	}
	var waiter, flood time.Time
	for _, q := range s.Requirements {
		if q.ID == "important" {
			waiter = q.CreatedAt
		} else {
			flood = q.CreatedAt
		}
	}
	if !waiter.Equal(flood) {
		t.Fatalf("at D=0 the capture times differ: waiter=%v flood=%v", waiter, flood)
	}
	baseline := runStarvationTicks(t, starvationScenario(captureSpreadStart()), StarvationBoundTicks, time.Second, captureSpreadRiskStep)
	measured := runStarvationTicks(t, captureSpreadScenario(captureSpreadStart(), 0), StarvationBoundTicks, time.Second, captureSpreadRiskStep)
	if baseline == 0 || measured == 0 {
		t.Fatalf("V2-030 scenario converged on tick %d and this file's D=0 scenario on tick %d; both must converge", baseline, measured)
	}
	if baseline != measured {
		t.Fatalf("V2-030's shared-CreatedAt scenario converges on tick %d but this file's D=0 scenario on tick %d; the builder is not V2-030's scenario plus a spread", baseline, measured)
	}
	t.Logf("V2-030 scenario and this file's D=0 scenario both converge on tick %d", measured)
}

// ===========================================================================
// A7: the mapping rule for a Requirement with no recorded capture time.
// ===========================================================================

// TestMissingCaptureTimeMapsToAgeZeroAndNotToTheZeroInstant is A7. It proves
// the two halves of the rule V2-073 declares and V2-068 applies:
//
//   - A candidate presented at age zero (CreatedAt == the snapshot's Now, the
//     rule) is ordered BELOW an otherwise identical candidate that carries a
//     real, earlier capture time. Missing therefore means least privileged.
//   - A candidate presented with the ZERO INSTANT outranks every other
//     candidate regardless of priority. ageSeconds clamps only the negative
//     side, and now.Sub(time.Time{}) does not even reach year 1 in whole
//     seconds: time.Duration is an int64 of nanoseconds, so the subtraction
//     SATURATES at the maximum Duration, which is 9223372036 seconds (about
//     292 years). MEASURED, and recorded here because dp-v2-073 d6 predicted
//     "roughly two thousand years expressed in seconds": the magnitude is
//     Duration saturation, not the true year-1 offset. The consequence d6
//     drew is unchanged and is what this test asserts -- 9223372036 dwarfs
//     legacyScore's priority*300 term (at most 30000), so the zero instant
//     converts a missing value into absolute precedence. That case is run
//     explicitly as a positive control, with the computed scores recorded, so
//     the evidence shows WHY the rule is age zero rather than the zero
//     instant.
//
// THE RULE, in one sentence: a Requirement with no recorded capture time is
// ordered as if captured at the snapshot's Now, never as an unbounded age.
func TestMissingCaptureTimeMapsToAgeZeroAndNotToTheZeroInstant(t *testing.T) {
	now := captureSpreadStart()
	// "aaa-missing" sorts before "zzz-recorded", so if the two scored equal
	// the comparator would rank the missing one first. Ranking the recorded
	// one first is therefore a statement about the score, not about the ids.
	missing := Requirement{ID: "aaa-missing", RepositoryIDs: []string{"repo"}, Status: StatusReady, Priority: 50, CreatedAt: now}
	recorded := Requirement{ID: "zzz-recorded", RepositoryIDs: []string{"repo"}, Status: StatusReady, Priority: 50, CreatedAt: now.Add(-time.Hour)}
	s := Snapshot{
		Now:          now,
		Repositories: []Repository{{ID: "repo"}},
		Runners:      []Runner{{ID: "only-runner", Provider: "fake", Capacity: 1}},
		Requirements: []Requirement{missing, recorded},
	}
	plan, err := Decide(s)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	byID := map[string]Decision{}
	for _, d := range plan.Decisions {
		byID[d.RequirementID] = d
	}
	if byID["zzz-recorded"].Rank >= byID["aaa-missing"].Rank {
		t.Fatalf("a candidate mapped to age zero outranked one with a real earlier capture time: recorded rank=%d score=%d, missing rank=%d score=%d",
			byID["zzz-recorded"].Rank, byID["zzz-recorded"].Score, byID["aaa-missing"].Rank, byID["aaa-missing"].Score)
	}
	if byID["aaa-missing"].Inputs.Age != 0 {
		t.Fatalf("the age-zero mapping produced age %d, want 0", byID["aaa-missing"].Inputs.Age)
	}
	if byID["zzz-recorded"].Inputs.Age != 3600 {
		t.Fatalf("the recorded candidate's age = %d, want 3600", byID["zzz-recorded"].Inputs.Age)
	}
	t.Logf("age-zero mapping: recorded rank=%d score=%d age=%d; missing-mapped-to-now rank=%d score=%d age=%d",
		byID["zzz-recorded"].Rank, byID["zzz-recorded"].Score, byID["zzz-recorded"].Inputs.Age,
		byID["aaa-missing"].Rank, byID["aaa-missing"].Score, byID["aaa-missing"].Inputs.Age)

	// Positive control: the same candidate presented with the ZERO INSTANT
	// instead. It carries the LOWEST priority in the snapshot and still
	// outranks everything, which is the whole reason the rule is age zero.
	zeroInstant := Requirement{ID: "zzz-zero-instant", RepositoryIDs: []string{"repo"}, Status: StatusReady, Priority: 0, CreatedAt: time.Time{}}
	top := Requirement{ID: "aaa-top-priority", RepositoryIDs: []string{"repo"}, Status: StatusReady, Priority: 100, CreatedAt: now.Add(-time.Hour)}
	control := Snapshot{
		Now:          now,
		Repositories: []Repository{{ID: "repo"}},
		Runners:      []Runner{{ID: "only-runner", Provider: "fake", Capacity: 1}},
		Requirements: []Requirement{top, zeroInstant},
	}
	plan, err = Decide(control)
	if err != nil {
		t.Fatalf("Decide (zero-instant control): %v", err)
	}
	byID = map[string]Decision{}
	for _, d := range plan.Decisions {
		byID[d.RequirementID] = d
	}
	if byID["zzz-zero-instant"].Rank != 0 {
		t.Fatalf("positive control failed: the zero instant did NOT outrank the highest-priority candidate (rank=%d score=%d vs rank=%d score=%d); the reason the mapping rule exists is unproven",
			byID["zzz-zero-instant"].Rank, byID["zzz-zero-instant"].Score, byID["aaa-top-priority"].Rank, byID["aaa-top-priority"].Score)
	}
	if byID["zzz-zero-instant"].Score <= byID["aaa-top-priority"].Score {
		t.Fatalf("positive control failed: zero-instant score %d did not exceed the top-priority score %d", byID["zzz-zero-instant"].Score, byID["aaa-top-priority"].Score)
	}
	t.Logf("zero-instant positive control: priority-0 candidate with the zero instant scored %d (age %d seconds) and took rank 0, beating a priority-100 candidate scoring %d (age %d seconds). This is why a missing capture time is mapped to the snapshot's Now and never to the zero instant.",
		byID["zzz-zero-instant"].Score, byID["zzz-zero-instant"].Inputs.Age,
		byID["aaa-top-priority"].Score, byID["aaa-top-priority"].Inputs.Age)
}
