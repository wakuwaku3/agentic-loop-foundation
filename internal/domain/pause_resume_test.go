package domain

// V2-090 A8: the exhaustive proof that a paused Requirement has an exit, and
// that the exit restores the status it actually came from.
//
// The measurement this file exists for, taken before a byte of model.go was
// edited: `paused` was a SOURCE status in exactly ONE of DecideRequirement's
// ten branches -- cancel -- and in no other. Nine branches excluded it. So a
// pause shipped without an exit would have handed the owner a button whose
// only sequel is destroying the Requirement, and domain.Requirement carried no
// field that could hold a previous status.
//
// docs/architecture/domain-model.md:269 defines both exits in one row:
//
//	| `paused` | 人間が処理を停止 | 直前の安全な非終端状態、`cancelled` |
//
// 直前の安全な非終端状態 -- "the immediately preceding safe non-terminal
// state" -- names the status the Requirement was ACTUALLY IN. That is why the
// exit is a MEMORY (PausedFrom) rather than a fixed status, and why resume's
// legal TARGETS are derived from pause's legal SOURCES instead of being
// declared a second time. The four pause entrances are cited one per edge at
// domain-model.md:265 (ready), :266 (active), :267 (waiting) and :270
// (recovering).
//
// Only imports already in source_guard_test.go's testImportAllowlist are used
// -- errors, go/ast, go/parser, go/token, testing, time -- so this file adds no
// import edge to internal/domain and source_guard_test.go, which is prohibited
// to this task, is not opened. In particular `reflect` is unavailable, so every
// byte-unchanged assertion goes through invariant_model_test.go's
// requirementsEqual, which this task extends to compare PausedFrom.

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// pauseResumeActor and pauseResumeAt are literals. source_guard_test.go
// forbids the time.Now selector outright in this package, so nothing here
// reads a clock.
const pauseResumeActor = ActorID("actor-pause-resume")

func pauseResumeAt() time.Time { return time.Unix(1_750_000_000, 0).UTC() }

// wantPausableStatusCount is the size of the pausable set, written out once so
// a fifth entry appearing in model.go fails a test instead of silently widening
// resume's target set. It is four because the pause branch admits exactly
// ready, active, waiting and recovering, and because exactly four cells of
// docs/architecture/domain-model.md's Requirement lifecycle table name `paused`
// as a 主な遷移先.
const wantPausableStatusCount = 4

// pauseResumeRequirement builds a Requirement in the given status that Validate
// accepts. It mirrors capture_time_test.go's requirementWithStatus: `completed`
// is the one status Validate demands a StableSnapshot for, and `paused` is the
// one status this task gives a memory to.
func pauseResumeRequirement(status RequirementStatus, pausedFrom RequirementStatus, version Version) Requirement {
	r := Requirement{ID: RequirementID("requirement-pause-resume"), Status: status, Version: version, PausedFrom: pausedFrom}
	if status == RequirementCompleted {
		r.StableSnapshot = StableReleaseSnapshot{ReleaseID: "release-1", ReleaseVersion: 1, BundleDigest: "bundle", EvidenceDigest: "evidence"}
	}
	return r
}

func pauseResumeCommand(kind RequirementCommandKind, version Version) RequirementCommand {
	return RequirementCommand{Kind: kind, Actor: pauseResumeActor, At: pauseResumeAt(), ExpectedVersion: version}
}

// TestResumeSucceedsExactlyWhenThePausedStatusCarriesALegalMemory is A8(a): the
// full status-by-memory table, and A8(e) inside the same run.
//
// The axes are DERIVED rather than re-declared, for the reason
// capture_time_test.go:34-40 already gives: allRequirementStatuses() and
// pausableRequirementStatuses() own the two lists, so a status or a pausable
// entry added without revisiting this file fails the count guards below in the
// same edit. Eleven statuses crossed with the four pausable memories plus the
// EMPTY memory gives 55 cells.
func TestResumeSucceedsExactlyWhenThePausedStatusCarriesALegalMemory(t *testing.T) {
	statuses := allRequirementStatuses()
	if len(statuses) != wantRequirementStatusCount {
		t.Fatalf("status axis drifted: %d statuses, want %d", len(statuses), wantRequirementStatusCount)
	}
	pausable := pausableRequirementStatuses()
	if len(pausable) != wantPausableStatusCount {
		t.Fatalf("pausable axis drifted: %d statuses, want %d", len(pausable), wantPausableStatusCount)
	}
	// The memory axis is the four pausable statuses plus the empty value. The
	// empty value is a real, reachable record -- A7 keeps it VALID on purpose
	// -- and it is the one memory resume must refuse.
	memories := append(append([]RequirementStatus(nil), pausable...), RequirementStatus(""))
	if len(memories) != wantPausableStatusCount+1 {
		t.Fatalf("memory axis has %d entries, want %d", len(memories), wantPausableStatusCount+1)
	}

	cells, succeeded, refused := 0, 0, 0
	for _, status := range statuses {
		for _, memory := range memories {
			cells++
			current := pauseResumeRequirement(status, memory, 7)
			// Every cell in this table is a record Validate accepts, so a
			// refusal below is always the resume guard's refusal and never
			// Validate's. That is asserted here rather than assumed: a cell
			// Validate rejected would make DecideRequirement return before
			// reaching the switch at all and the table would prove nothing.
			if err := Validate(current); err != nil {
				t.Fatalf("status %q with memory %q: Validate = %v, want nil; this cell measures nothing", status, memory, err)
			}
			next, err := DecideRequirement(current, pauseResumeCommand(RequirementResume, current.Version))
			wantSuccess := status == RequirementPaused && memory != ""
			if wantSuccess {
				succeeded++
				if err != nil {
					t.Fatalf("resume from %q with memory %q: err = %v, want a legal transition", status, memory, err)
				}
				// The restored status is the memory EXACTLY, not merely a
				// legal status: an implementation that resumed into `ready`
				// from every memory would pass a "is the result legal" check
				// and fail this one.
				if next.Status != memory {
					t.Fatalf("resume from %q with memory %q restored %q, want exactly %q", status, memory, next.Status, memory)
				}
				if next.PausedFrom != "" {
					t.Fatalf("resume from %q with memory %q left PausedFrom = %q, want it cleared", status, memory, next.PausedFrom)
				}
				if next.Version != current.Version+1 {
					t.Fatalf("resume from %q with memory %q: version = %d, want %d", status, memory, next.Version, current.Version+1)
				}
				continue
			}
			refused++
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("resume from %q with memory %q: err = %v, want ErrInvalidTransition", status, memory, err)
			}
			if !requirementsEqual(next, current) {
				t.Fatalf("a refused resume from %q with memory %q mutated the record: before=%+v after=%+v", status, memory, current, next)
			}
		}
	}
	if cells != wantRequirementStatusCount*(wantPausableStatusCount+1) {
		t.Fatalf("the table measured %d cells, want %d", cells, wantRequirementStatusCount*(wantPausableStatusCount+1))
	}
	// Exactly the four (paused, pausable-memory) cells succeed; the other 51
	// are refusals. A8(e)'s "resume is refused from all TEN non-paused
	// statuses" is the 50 of those 51 whose status is not paused, plus the one
	// paused cell whose memory is empty.
	if succeeded != wantPausableStatusCount {
		t.Fatalf("%d cells succeeded, want exactly %d", succeeded, wantPausableStatusCount)
	}
	if refused != cells-wantPausableStatusCount {
		t.Fatalf("%d cells were refused, want %d", refused, cells-wantPausableStatusCount)
	}
	nonPausedRefusals := 0
	for _, status := range statuses {
		if status == RequirementPaused {
			continue
		}
		for _, memory := range memories {
			current := pauseResumeRequirement(status, memory, 3)
			next, err := DecideRequirement(current, pauseResumeCommand(RequirementResume, current.Version))
			if !errors.Is(err, ErrInvalidTransition) || !requirementsEqual(next, current) {
				t.Fatalf("resume from the non-paused status %q with memory %q: err=%v unchanged=%v", status, memory, err, requirementsEqual(next, current))
			}
			nonPausedRefusals++
		}
	}
	if nonPausedRefusals != (wantRequirementStatusCount-1)*(wantPausableStatusCount+1) {
		t.Fatalf("A8(e) measured %d non-paused cells, want %d", nonPausedRefusals, (wantRequirementStatusCount-1)*(wantPausableStatusCount+1))
	}
	t.Logf("A8(a)+(e): %d cells measured, %d succeeded, %d refused; %d of the refusals are the ten non-paused statuses", cells, succeeded, refused, nonPausedRefusals)
}

// TestPauseThenResumeRoundTripsEveryPausableStatus is A8(b).
//
// The version assertion is +2 and NOT equality: pause increments once and
// resume increments once, so a round trip that came back at the original
// version would mean one of the two transitions did not happen.
func TestPauseThenResumeRoundTripsEveryPausableStatus(t *testing.T) {
	pausable := pausableRequirementStatuses()
	if len(pausable) != wantPausableStatusCount {
		t.Fatalf("pausable axis drifted: %d statuses, want %d", len(pausable), wantPausableStatusCount)
	}
	for _, source := range pausable {
		before := pauseResumeRequirement(source, "", 11)
		before.Increments = []IncrementID{"increment-1", "increment-2"}
		before.RequestedBy = RequestedBy{ActorType: ActorTypeOwner, Subject: "owner-1"}
		before.CapturedAt = time.Unix(1_700_000_000, 123_456_789).UTC()

		paused, err := DecideRequirement(before, pauseResumeCommand(RequirementPause, before.Version))
		if err != nil {
			t.Fatalf("pause from %q: %v", source, err)
		}
		if paused.Status != RequirementPaused {
			t.Fatalf("pause from %q produced status %q, want paused", source, paused.Status)
		}
		if paused.PausedFrom != source {
			t.Fatalf("pause from %q remembered %q, want %q", source, paused.PausedFrom, source)
		}
		if paused.Version != before.Version+1 {
			t.Fatalf("pause from %q: version = %d, want %d", source, paused.Version, before.Version+1)
		}

		resumed, err := DecideRequirement(paused, pauseResumeCommand(RequirementResume, paused.Version))
		if err != nil {
			t.Fatalf("resume back to %q: %v", source, err)
		}
		if resumed.Status != source {
			t.Fatalf("the round trip from %q came back as %q; the memory was ignored", source, resumed.Status)
		}
		if resumed.PausedFrom != "" {
			t.Fatalf("the round trip from %q left PausedFrom = %q, want it cleared", source, resumed.PausedFrom)
		}
		if resumed.Version != before.Version+2 {
			t.Fatalf("the round trip from %q ended at version %d, want %d (pause and resume each increment once)", source, resumed.Version, before.Version+2)
		}
		// Every other field is byte-unchanged. requirementsEqual compares ID,
		// Status, Version, StableSnapshot, PausedFrom and the Increments
		// slice, so the comparison is made against a copy of `before` carrying
		// only the version the round trip legitimately moved.
		want := before
		want.Version = before.Version + 2
		if !requirementsEqual(resumed, want) {
			t.Fatalf("the round trip from %q changed something other than the version: got=%+v want=%+v", source, resumed, want)
		}
		if resumed.RequestedBy != before.RequestedBy {
			t.Fatalf("the round trip from %q rewrote RequestedBy: %+v -> %+v", source, before.RequestedBy, resumed.RequestedBy)
		}
		if !resumed.CapturedAt.Equal(before.CapturedAt) {
			t.Fatalf("the round trip from %q rewrote CapturedAt: %v -> %v", source, before.CapturedAt, resumed.CapturedAt)
		}
	}
	t.Logf("A8(b): all %d pausable statuses round-tripped, each returning to the SAME status two versions above where it started", len(pausable))
}

// TestASecondPauseIsRefusedAndLeavesTheMemoryByteUnchanged is A8(c), and it
// guards the most destructive bug this design admits: a second pause that
// SUCCEEDED would overwrite PausedFrom with `paused`, which is not a member of
// the pausable set, so the Requirement would become unresumable while the
// request that broke it returned success.
func TestASecondPauseIsRefusedAndLeavesTheMemoryByteUnchanged(t *testing.T) {
	for _, source := range pausableRequirementStatuses() {
		paused := pauseResumeRequirement(RequirementPaused, source, 5)
		next, err := DecideRequirement(paused, pauseResumeCommand(RequirementPause, paused.Version))
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("a second pause of a Requirement paused from %q: err = %v, want ErrInvalidTransition", source, err)
		}
		if next.PausedFrom != source {
			t.Fatalf("a refused second pause overwrote the memory %q with %q", source, next.PausedFrom)
		}
		if !requirementsEqual(next, paused) {
			t.Fatalf("a refused second pause mutated the record: before=%+v after=%+v", paused, next)
		}
		// And the memory still works: the record the refused pause left behind
		// is still resumable into the status it actually came from.
		resumed, err := DecideRequirement(next, pauseResumeCommand(RequirementResume, next.Version))
		if err != nil || resumed.Status != source {
			t.Fatalf("after a refused second pause the Requirement no longer resumes into %q: status=%q err=%v", source, resumed.Status, err)
		}
	}
}

// TestEveryPausedRequirementHasBothExits is A8(d): from each of the four
// pausable sources, BOTH exits docs/architecture/domain-model.md:269 names are
// available on the same paused record -- 直前の安全な非終端状態 and `cancelled`.
func TestEveryPausedRequirementHasBothExits(t *testing.T) {
	for _, source := range pausableRequirementStatuses() {
		paused := pauseResumeRequirement(RequirementPaused, source, 9)

		resumed, err := DecideRequirement(paused, pauseResumeCommand(RequirementResume, paused.Version))
		if err != nil || resumed.Status != source {
			t.Fatalf("exit one (resume) from a Requirement paused from %q: status=%q err=%v", source, resumed.Status, err)
		}
		cancelled, err := DecideRequirement(paused, pauseResumeCommand(RequirementCancel, paused.Version))
		if err != nil || cancelled.Status != RequirementCancelled {
			t.Fatalf("exit two (cancel) from a Requirement paused from %q: status=%q err=%v", source, cancelled.Status, err)
		}
		// Cancel clears the memory: a cancelled Requirement is terminal
		// (domain-model.md:273 "終端。再開は新しい明示Intentでのみ可能"), so a
		// remembered origin on it would be a resumption offer the lifecycle
		// does not honour.
		if cancelled.PausedFrom != "" {
			t.Fatalf("cancel from a Requirement paused from %q left PausedFrom = %q, want it cleared", source, cancelled.PausedFrom)
		}
		// A paused record with NO memory keeps exit two, which is the whole
		// reason A7 refuses to make such a record invalid.
		forgotten := pauseResumeRequirement(RequirementPaused, "", 9)
		cancelledForgotten, err := DecideRequirement(forgotten, pauseResumeCommand(RequirementCancel, forgotten.Version))
		if err != nil || cancelledForgotten.Status != RequirementCancelled {
			t.Fatalf("a paused Requirement with no memory lost its cancel exit: status=%q err=%v", cancelledForgotten.Status, err)
		}
	}
}

// TestValidateKeepsAPausedRequirementWithNoMemoryValidAndCancellable is A7's
// three halves, asserted together because the argument is a triple and any one
// of them alone would permit the trap.
//
// DecideRequirement calls Validate on its FIRST line (measured at
// model.go:456-458), so a record Validate rejected would be a record NO command
// could be issued against -- INCLUDING cancel. A paused record with no
// remembered origin is precisely the record that most needs an exit, so
// Validate accepts it, resume refuses it, and cancel still succeeds on it.
//
// The asymmetry with the completed clause is terminality: a completed
// Requirement with no StableSnapshot IS invalid, and that is right, because an
// invalid terminal record needs no exit. `paused` is the one non-terminal
// status whose entire design problem is having one.
func TestValidateKeepsAPausedRequirementWithNoMemoryValidAndCancellable(t *testing.T) {
	forgotten := pauseResumeRequirement(RequirementPaused, "", 4)
	if err := Validate(forgotten); err != nil {
		t.Fatalf("Validate rejected a paused Requirement with no memory: %v; such a record could not be cancelled either", err)
	}
	next, err := DecideRequirement(forgotten, pauseResumeCommand(RequirementResume, forgotten.Version))
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resume of a paused Requirement with no memory: err = %v, want ErrInvalidTransition", err)
	}
	if !requirementsEqual(next, forgotten) {
		t.Fatalf("a refused resume mutated the record: before=%+v after=%+v", forgotten, next)
	}
	cancelled, err := DecideRequirement(forgotten, pauseResumeCommand(RequirementCancel, forgotten.Version))
	if err != nil {
		t.Fatalf("cancel of a paused Requirement with no memory: %v", err)
	}
	if cancelled.Status != RequirementCancelled {
		t.Fatalf("cancel produced status %q, want cancelled", cancelled.Status)
	}

	// The one clause A7 DOES add: a non-empty memory outside the pausable set
	// is invalid. `paused` itself is the value a buggy second pause would have
	// written, so it is the value asserted here.
	for _, illegal := range []RequirementStatus{RequirementPaused, RequirementCaptured, RequirementCompleted, RequirementCancelled, RequirementStatus("not-a-status")} {
		bad := pauseResumeRequirement(RequirementPaused, illegal, 4)
		if err := Validate(bad); err == nil {
			t.Fatalf("Validate accepted a PausedFrom of %q, which is not a member of the pausable set", illegal)
		}
	}
	// And the clause is about PausedFrom's VALUE, not about the record's
	// status: a non-paused Requirement carrying a legal memory stays valid,
	// because that is exactly the shape a refused resume leaves behind and a
	// cancel is about to clear.
	for _, source := range pausableRequirementStatuses() {
		carried := pauseResumeRequirement(RequirementActive, source, 4)
		if err := Validate(carried); err != nil {
			t.Fatalf("Validate rejected an active Requirement carrying the legal memory %q: %v", source, err)
		}
	}
}

// TestThePausableSetIsDeclaredOnceAndReadTwice makes C7 a RED GUARD instead of
// a convention.
//
// "The pausable set is declared once and read twice" is an acceptance property:
// resume's legal TARGETS must be DERIVED from pause's legal SOURCES, because two
// hand-written lists can drift and a drifted resume lands a Requirement in a
// status it was never in. A comment cannot enforce that. This scan can: it
// parses model.go and asserts that the whole file contains exactly ONE
// []RequirementStatus composite literal, that it holds exactly the four pausable
// statuses, that it lives inside the one function pausableRequirementStatuses,
// and that that function is CALLED at least twice elsewhere in the file. Writing
// the four statuses out a second time -- in the resume guard, in the Validate
// clause, anywhere -- fails this test.
//
// It is verified against a synthetic known-positive first, so a mis-written
// visitor cannot make it pass vacuously, and it fails outright on a parse error.
func TestThePausableSetIsDeclaredOnceAndReadTwice(t *testing.T) {
	const helperName = "pausableRequirementStatuses"

	// Positive control: the visitor must see a []RequirementStatus literal
	// where one exists.
	positive := "package domain\n\nfunc x() []RequirementStatus { return []RequirementStatus{RequirementReady} }\n"
	synthetic, err := parser.ParseFile(token.NewFileSet(), "positive.go", positive, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := statusSliceLiterals(synthetic); n != 1 {
		t.Fatalf("positive control: the visitor counted %d []RequirementStatus literals, want 1", n)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "model.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse model.go: %v", err)
	}
	count, elements := statusSliceLiterals(file)
	if count != 1 {
		t.Fatalf("model.go contains %d []RequirementStatus composite literals, want exactly 1; the pausable set is declared more than once", count)
	}
	want := pausableRequirementStatuses()
	if len(elements) != len(want) {
		t.Fatalf("the one []RequirementStatus literal in model.go names %v, want the %d pausable statuses", elements, len(want))
	}
	wantIdents := []string{"RequirementReady", "RequirementActive", "RequirementWaiting", "RequirementRecovering"}
	for i := range wantIdents {
		if elements[i] != wantIdents[i] {
			t.Fatalf("the pausable literal names %v, want %v in that order", elements, wantIdents)
		}
	}

	// The literal must live inside the helper, and the helper must have at
	// least two readers in the same file.
	enclosing := ""
	readers := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if n, _ := statusSliceLiterals(fn.Body); n > 0 {
			if fn.Name != nil {
				enclosing = fn.Name.Name
			}
		}
		if fn.Name != nil && fn.Name.Name == helperName {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			if ident, isIdent := call.Fun.(*ast.Ident); isIdent && ident.Name == helperName {
				readers++
			}
			// A call passed through a spread -- allowed(pausableRequirement
			// Statuses()...) -- is a CallExpr argument of another CallExpr, and
			// ast.Inspect walks into it, so it is counted by the branch above.
			return true
		})
	}
	if enclosing != helperName {
		t.Fatalf("the pausable literal lives in %q, want %q", enclosing, helperName)
	}
	if readers < 2 {
		t.Fatalf("%s has %d readers outside itself in model.go, want at least 2 (the pause branch's allowed(...) call and the resume guard)", helperName, readers)
	}
	t.Logf("C7: the pausable set is declared once (in %s) and read %d times elsewhere in model.go", helperName, readers)
}

// statusSliceLiterals counts []RequirementStatus composite literals under the
// given node and returns the identifier names of the last one's elements.
func statusSliceLiterals(node ast.Node) (int, []string) {
	count := 0
	var elements []string
	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		arr, ok := lit.Type.(*ast.ArrayType)
		if !ok || arr.Len != nil {
			return true
		}
		ident, ok := arr.Elt.(*ast.Ident)
		if !ok || ident.Name != "RequirementStatus" {
			return true
		}
		count++
		elements = nil
		for _, e := range lit.Elts {
			if id, isIdent := e.(*ast.Ident); isIdent {
				elements = append(elements, id.Name)
				continue
			}
			elements = append(elements, "<not-an-identifier>")
		}
		return true
	})
	return count, elements
}
