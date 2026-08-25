// The rollback window (docs/operations/self-update.md section 9).
//
// A version stays inside its window while ANY of four clauses still holds,
// and the window closes only when EVERY one of them has ceased -- the
// logical conjunction of the four negations. A purely time-based window
// would contradict docs/architecture/validation.md section 8; a purely
// generation-based one would close at the instant a successor is promoted,
// which is precisely when rollback is most likely to be needed.
//
// Closure is recorded, never recomputed: nothing in this file reads the wall
// clock, and WindowClosure reads back only what was written.
package update

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// WindowClause names one of the four "still holds" clauses.
type WindowClause string

const (
	// ClauseGeneration: the version is still the previous stable route
	// target, or nothing newer has ever been Stable.
	ClauseGeneration WindowClause = "generation"
	// ClauseSchemaStage: no contract-stage step has moved the canonical
	// schema outside this version's [schema_min, schema_max].
	ClauseSchemaStage WindowClause = "schema-stage"
	// ClauseTime: the contract-declared minimum dwell has not elapsed on
	// the injected clock since the successor became Stable.
	ClauseTime WindowClause = "time"
	// ClauseEvidence: the successor does not yet hold a passed Preview
	// capability exercise for the whole Release Contract.
	ClauseEvidence WindowClause = "evidence"
)

// WindowClauses is the four clauses in section 9.1 order. A test that
// enumerates the window's whole truth table iterates this.
var WindowClauses = []WindowClause{ClauseGeneration, ClauseSchemaStage, ClauseTime, ClauseEvidence}

// The two recorded closure criteria. Closure always requires the schema
// clause to have ceased, so the criterion always names a movement of the
// canonical schema out of this version's interval; the generation, dwell and
// evidence clauses can only delay a closure, never cause one. The
// distinction between the two recorded criteria matters because only the
// contract one is one-way: after a contract step the store no longer serves
// a shape this version can read, so even a rebuilt binary cannot be routed,
// whereas after a bare advance the shape is still there and the refusal is
// the declared interval.
const (
	CriterionSchemaContract = "schema-contract"
	CriterionSchemaAdvance  = "schema-advance"
)

// WindowInput is the whole input set of the window predicate: the
// machine-local record plus an injected clock and a contract-declared
// dwell. Nothing else, and no wall clock.
type WindowInput struct {
	Version         string
	PreviousStable  string
	CanonicalSchema int
	SchemaMin       int
	SchemaMax       int

	// SuccessorStableAt is zero while nothing newer has been Stable.
	SuccessorStableAt time.Time
	MinimumDwell      time.Duration
	Now               time.Time

	// SuccessorExerciseComplete is true only when the successor holds a
	// passed Preview capability exercise for the whole Release Contract.
	SuccessorExerciseComplete bool

	// SchemaMovedByContract records whether the canonical schema left this
	// version's interval because of a contract-stage step rather than an
	// expand-stage advance. It changes only the recorded criterion, never
	// whether the window is open.
	SchemaMovedByContract bool
}

// WindowOpen reports whether the window is open and which clauses still
// hold. The window is open while the disjunction of the four is true.
func WindowOpen(in WindowInput) (bool, []WindowClause) {
	var holding []WindowClause
	if in.SuccessorStableAt.IsZero() || in.Version == in.PreviousStable {
		holding = append(holding, ClauseGeneration)
	}
	if in.CanonicalSchema >= in.SchemaMin && in.CanonicalSchema <= in.SchemaMax {
		holding = append(holding, ClauseSchemaStage)
	}
	if in.SuccessorStableAt.IsZero() || in.Now.Before(in.SuccessorStableAt.Add(in.MinimumDwell)) {
		holding = append(holding, ClauseTime)
	}
	if !in.SuccessorExerciseComplete {
		holding = append(holding, ClauseEvidence)
	}
	return len(holding) > 0, holding
}

// ClosureCriterion names why a closed window closed. A closure caused by a
// contract step is the one irreversible transition in M8, so it is recorded
// distinctly from a closure caused by a bare schema advance.
func ClosureCriterion(in WindowInput) string {
	if in.SchemaMovedByContract {
		return CriterionSchemaContract
	}
	return CriterionSchemaAdvance
}

// WindowPolicy carries the contract-declared inputs the machine-local
// record does not hold: the minimum dwell, and which successor versions
// hold a complete Preview capability exercise.
type WindowPolicy struct {
	MinimumDwell     time.Duration
	ExerciseComplete map[string]bool
	// ContractApplied records that the canonical schema's current value was
	// reached through a contract-stage step.
	ContractApplied bool
}

// WindowInputFor assembles the predicate's input for one version out of the
// machine-local record, the contract policy and the injected clock.
func (s *State) WindowInputFor(version string, policy WindowPolicy, now time.Time) (WindowInput, error) {
	installed, ok := s.Find(version)
	if !ok {
		return WindowInput{}, fmt.Errorf("version %s is not recorded in the machine state", version)
	}
	w := s.window(version)
	return WindowInput{
		Version:                   version,
		PreviousStable:            s.PreviousStable,
		CanonicalSchema:           s.CanonicalSchema,
		SchemaMin:                 installed.SchemaMin,
		SchemaMax:                 installed.SchemaMax,
		SuccessorStableAt:         w.SuccessorStableAt,
		MinimumDwell:              policy.MinimumDwell,
		Now:                       now,
		SuccessorExerciseComplete: policy.ExerciseComplete[w.SuccessorVersion],
		SchemaMovedByContract:     policy.ContractApplied,
	}, nil
}

// ErrWindowStillOpen refuses a closure that at least one clause still
// supports.
var ErrWindowStillOpen = errors.New("update: the rollback window is still open")

// CloseWindow records a closure. It refuses while any clause still holds,
// and it is idempotent: a window that already carries a recorded closure
// keeps the recorded time and criterion, because a recomputed closure is
// exactly what section 9.2 forbids. The caller persists the state.
func CloseWindow(state *State, version string, in WindowInput) (WindowState, error) {
	if state == nil {
		return WindowState{}, errors.New("invalid installed state")
	}
	current := state.window(version)
	if !current.ClosedAt.IsZero() {
		return current, nil
	}
	if open, holding := WindowOpen(in); open {
		return current, fmt.Errorf("%w for %s: %s", ErrWindowStillOpen, version, formatClauses(holding))
	}
	current.ClosedAt = in.Now
	current.Criterion = ClosureCriterion(in)
	state.setWindow(version, current)
	return current, nil
}

// WindowClosure reads back a recorded closure. It performs no evaluation
// whatsoever, so the answer to "why was this deletable?" does not depend on
// when the question is asked.
func WindowClosure(state *State, version string) (time.Time, string, bool) {
	if state == nil {
		return time.Time{}, "", false
	}
	w := state.window(version)
	if w.ClosedAt.IsZero() {
		return time.Time{}, "", false
	}
	return w.ClosedAt, w.Criterion, true
}

func formatClauses(clauses []WindowClause) string {
	names := make([]string, 0, len(clauses))
	for _, c := range clauses {
		names = append(names, string(c))
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
