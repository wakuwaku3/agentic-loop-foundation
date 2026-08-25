package scheduler

// This file connects internal/scheduler's scalar priority score to
// domain.PriorityAssessment's multi-factor evaluation (V2-030 / A3).
// docs/architecture/domain-model.md's Priority Assessment section is explicit
// that "a single fixed score is not the source of truth" -- Decide is meant
// to record the *comparison result* as a Decision (see decision.go), not to
// canonicalize one number. This file only supplies the score that comparison
// is based on.

import (
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ageSeconds is the elapsed time between a candidate's CreatedAt and the
// snapshot's Now, clamped to zero so a clock skew that makes a requirement
// look "created in the future" never produces a negative age bonus. This is
// unchanged from the scheduler's pre-V2-030 behavior.
func ageSeconds(createdAt, now time.Time) int64 {
	age := int64(now.Sub(createdAt).Seconds())
	if age < 0 {
		return 0
	}
	return age
}

// clampFactor bounds an arbitrary caller-supplied integer factor to [0,100]
// before it is weighted, exactly as legacyPriority already did for the
// scalar Priority field, so one caller's out-of-range input cannot silently
// dominate every other candidate's score.
func clampFactor(v int) int64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return int64(v)
}

// legacyPriority reproduces the pre-V2-030 scheduler's clamp verbatim. It
// must never change: A3 requires the no-assessment ordering to be identical
// to what the scalar-only scheduler always produced.
func legacyPriority(p int64) int64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// legacyScore reproduces the pre-V2-030 scheduler's score() body verbatim:
// priority dominates in 300-point steps, and age (in whole seconds) breaks
// ties among same-priority candidates as they wait. It is preserved
// unchanged as the branch computeScore takes when a Requirement carries no
// domain.PriorityAssessment, which is what lets all five pre-existing
// scheduler tests keep passing under their own names with unchanged
// assertions (A2, A3).
func legacyScore(priority int64, age int64) int64 {
	return legacyPriority(priority)*300 + age
}

// Multi-factor weights for domain.PriorityAssessment. Each is a deliberate,
// documented product judgement, chosen once before starvation_test.go's
// StarvationBoundTicks was picked (per the Work Order's constraint to
// justify the starvation bound before tuning the aging/starvation weight,
// not after):
//
//   - weightValue (400) is the single largest ordinary weight. ValueScore
//     already folds together "利用者価値と影響範囲" (user value and blast
//     radius) per domain-model.md's Priority Assessment section, so it is
//     the strongest ordinary signal that a Requirement matters at all.
//   - weightUrgency (350) sits just under value: a hard deadline should
//     almost be able to outrank raw value on its own, but a Requirement that
//     is both high-value and urgent must still outrank one that is merely
//     urgent, so urgency stays second.
//   - weightRisk (250) is POSITIVE, not negative. RiskScore models exposure
//     if the Requirement is left undone -- security/availability/data/cost
//     risk per domain-model.md -- so a higher score means "resolve this
//     sooner," the same direction as value and urgency, just weighted lower
//     because risk alone, with no value or urgency behind it, should not
//     dominate the queue.
//   - weightDependency (200) is positive and moderate: DependencyScore is
//     the unblocking effect on other, separate work. That is real but
//     indirect, so it sits below the three factors that describe the
//     Requirement's own importance.
//   - weightLearning (100) is the smallest positive ordinary weight:
//     learning value and uncertainty reduction are real but soft and
//     exploratory, so this factor should only ever break near-ties, never
//     override a concrete value/urgency/risk case.
//   - weightResourceCost (150) is SUBTRACTED. ResourceCost estimates what
//     the Requirement will consume, so an expensive candidate is
//     deprioritized relative to an equally-valuable cheap one (a
//     cost-of-delay-style bias toward quick wins) -- but only modestly: 150
//     is below every "this requirement matters" weight, so cost alone can
//     never veto an otherwise clearly-important Requirement.
//   - weightStarvationRisk (500) is the LARGEST weight of all. This is the
//     factor the M7 starvation-prevention condition (A5) depends on: it must
//     be able to close a real deficit against weightValue+weightUrgency
//     within a small, bounded number of re-assessments (ticks). 500 was
//     chosen because it is strictly larger than every other single weight,
//     so a sequence of a handful of StarvationRisk increments is guaranteed
//     to eventually dominate any one fixed competing factor, which is
//     exactly what starvation_test.go measures and pins as
//     StarvationBoundTicks.
//   - age contributes 1 point per elapsed second, unchanged from
//     legacyScore, so a Requirement that has an assessment does not stop
//     aging the way the scalar path always aged. Because age is computed
//     from the same `now` for every candidate in one Decide call, it cancels
//     out of any single comparison between two candidates created at the
//     same time; it only matters across candidates with different CreatedAt.
const (
	weightValue          int64 = 400
	weightUrgency        int64 = 350
	weightRisk           int64 = 250
	weightDependency     int64 = 200
	weightLearning       int64 = 100
	weightResourceCost   int64 = 150
	weightStarvationRisk int64 = 500
)

// multiFactorScore computes the ranking score for a Requirement that carries
// a domain.PriorityAssessment, per the weight table above.
func multiFactorScore(a domain.PriorityAssessment, age int64) int64 {
	return clampFactor(a.ValueScore)*weightValue +
		clampFactor(a.UrgencyScore)*weightUrgency +
		clampFactor(a.RiskScore)*weightRisk +
		clampFactor(a.DependencyScore)*weightDependency +
		clampFactor(a.LearningScore)*weightLearning -
		clampFactor(a.ResourceCost)*weightResourceCost +
		clampFactor(a.StarvationRisk)*weightStarvationRisk +
		age
}

// computeScore is the single scoring entry point Decide uses (through the
// scoreFn indirection below). When q carries no domain.PriorityAssessment it
// takes the legacy scalar branch unchanged (A3's identical-order guarantee
// with the pre-V2-030 scheduler); when q does carry one, it takes the
// multi-factor branch.
func computeScore(q Requirement, now time.Time) int64 {
	age := ageSeconds(q.CreatedAt, now)
	if q.Assessment == nil {
		return legacyScore(q.Priority, age)
	}
	return multiFactorScore(*q.Assessment, age)
}

// scoreFn is a package-level variable, not a plain function call, solely so
// that starvation_test.go's negative control can substitute a variant with
// the StarvationRisk term neutralised and prove the same flood scenario
// fails to converge inside the same tick bound without it (A5). Decide
// always calls through scoreFn; nothing outside a test may reassign it, and
// every test that reassigns it must restore the original via defer.
var scoreFn = computeScore
