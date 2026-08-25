package update

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// windowCase realizes one row of the four-clause truth table. Each clause is
// driven by an input the other three do not share, so the sixteen rows are
// genuinely independent rather than four assertions dressed up as sixteen.
func windowInputFor(generation, schema, dwell, evidence bool) WindowInput {
	in := WindowInput{
		Version:           "1.0.0",
		SchemaMin:         1,
		SchemaMax:         2,
		SuccessorStableAt: at(0),
		MinimumDwell:      time.Hour,
	}
	if generation {
		in.PreviousStable = "1.0.0"
	} else {
		in.PreviousStable = "2.0.0"
	}
	if schema {
		in.CanonicalSchema = 1
	} else {
		in.CanonicalSchema = 9
	}
	if dwell {
		in.Now = at(30)
	} else {
		in.Now = at(120)
	}
	in.SuccessorExerciseComplete = !evidence
	return in
}

// TestRollbackWindowIsTheConjunctionOfFourClauses enumerates the whole truth
// table: sixteen rows, one per assignment of the four "still holds" clauses.
// The window is open in fifteen of them and closes in exactly the one where
// every clause has ceased, which is what "closure is the logical conjunction
// of all four having ceased" means operationally.
func TestRollbackWindowIsTheConjunctionOfFourClauses(t *testing.T) {
	rows := 0
	closable := 0
	for _, generation := range []bool{true, false} {
		for _, schema := range []bool{true, false} {
			for _, dwell := range []bool{true, false} {
				for _, evidence := range []bool{true, false} {
					rows++
					name := fmt.Sprintf("generation=%t/schema=%t/time=%t/evidence=%t", generation, schema, dwell, evidence)
					t.Run(name, func(t *testing.T) {
						in := windowInputFor(generation, schema, dwell, evidence)
						open, holding := WindowOpen(in)
						want := map[WindowClause]bool{
							ClauseGeneration:  generation,
							ClauseSchemaStage: schema,
							ClauseTime:        dwell,
							ClauseEvidence:    evidence,
						}
						got := map[WindowClause]bool{}
						for _, clause := range holding {
							got[clause] = true
						}
						for _, clause := range WindowClauses {
							if got[clause] != want[clause] {
								t.Fatalf("clause %s holding = %t, want %t", clause, got[clause], want[clause])
							}
						}
						wantOpen := generation || schema || dwell || evidence
						if open != wantOpen {
							t.Fatalf("open = %t, want %t", open, wantOpen)
						}
						state := NewState(1)
						state.Versions = []InstalledVersion{{Version: "1.0.0", SchemaMin: 1, SchemaMax: 2}}
						_, err := CloseWindow(state, "1.0.0", in)
						if wantOpen {
							if !errors.Is(err, ErrWindowStillOpen) {
								t.Fatalf("closure err = %v, want ErrWindowStillOpen", err)
							}
							if _, _, closed := WindowClosure(state, "1.0.0"); closed {
								t.Fatal("an open window recorded a closure")
							}
							return
						}
						closable++
						if err != nil {
							t.Fatal(err)
						}
						recordedAt, criterion, closed := WindowClosure(state, "1.0.0")
						if !closed || !recordedAt.Equal(in.Now) || criterion == "" {
							t.Fatalf("closure = %v %q %t", recordedAt, criterion, closed)
						}
					})
				}
			}
		}
	}
	if rows != 16 || closable != 1 {
		t.Fatalf("enumerated %d rows with %d closable; want 16 and 1", rows, closable)
	}
	t.Logf("verdict: 16 of 16 clause assignments enumerated, exactly 1 closes the window")
}

// TestWindowClosureIsRecordedAndNeverRecomputed pins section 9.2: the
// closure and the criterion that caused it are written once, and a later read
// never re-derives them -- not from the wall clock, and not from inputs that
// have since changed.
func TestWindowClosureIsRecordedAndNeverRecomputed(t *testing.T) {
	state := NewState(1)
	state.Versions = []InstalledVersion{{Version: "1.0.0", SchemaMin: 1, SchemaMax: 2}}

	closed := windowInputFor(false, false, false, false)
	closed.SchemaMovedByContract = true
	recorded, err := CloseWindow(state, "1.0.0", closed)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Criterion != CriterionSchemaContract {
		t.Fatalf("criterion = %q, want %q", recorded.Criterion, CriterionSchemaContract)
	}

	// Every clause holds again and the clock has moved backwards; the
	// recorded closure does not move and the window does not reopen.
	reopening := windowInputFor(true, true, true, true)
	reopening.Now = at(-600)
	again, err := CloseWindow(state, "1.0.0", reopening)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ClosedAt.Equal(recorded.ClosedAt) || again.Criterion != recorded.Criterion {
		t.Fatalf("recorded closure changed: %+v then %+v", recorded, again)
	}
	at2, criterion, ok := WindowClosure(state, "1.0.0")
	if !ok || !at2.Equal(closed.Now) || criterion != CriterionSchemaContract {
		t.Fatalf("read back %v %q %t", at2, criterion, ok)
	}
	t.Logf("verdict: closure recorded at %s with criterion %s and unchanged by a later evaluation", at2.Format(time.RFC3339), criterion)

	// A closure caused by a bare schema advance is recorded distinctly,
	// because only the contract one is one-way.
	advanced := NewState(1)
	advanced.Versions = state.Versions
	bare := windowInputFor(false, false, false, false)
	if _, err := CloseWindow(advanced, "1.0.0", bare); err != nil {
		t.Fatal(err)
	}
	if _, criterion, _ := WindowClosure(advanced, "1.0.0"); criterion != CriterionSchemaAdvance {
		t.Fatalf("criterion = %q, want %q", criterion, CriterionSchemaAdvance)
	}

	// The window predicate's whole input set is the machine-local record, a
	// contract-declared dwell and an injected clock: WindowInputFor reads
	// nothing else and refuses a version the record does not name.
	if _, err := advanced.WindowInputFor("9.9.9", WindowPolicy{}, at(0)); err == nil {
		t.Fatal("a window input was assembled for an unknown version")
	}
}
