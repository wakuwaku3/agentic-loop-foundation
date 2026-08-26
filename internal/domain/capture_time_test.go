package domain

// V2-073 A3: the closure argument for domain.Requirement.CapturedAt, proven
// mechanically rather than by review.
//
// Three properties have to hold for a value addition to this struct, and all
// three are checked here rather than asserted in prose:
//
//   - Validate accepts both a recorded and an absent capture time for every
//     valid RequirementStatus, so a record written before the field existed
//     stays valid (a legacy record) instead of becoming unreadable.
//   - Every RequirementCommandKind through DecideRequirement carries the
//     value forward byte-identically, checked by a table over EVERY command
//     kind rather than over a sample, so a future branch that starts
//     rewriting the field fails this test instead of passing unnoticed.
//   - CompleteRequirementFromRelease does the same.
//
// A fourth check reads this package's own source and pins the field list of
// Requirement plus the absence of a json tag on CapturedAt, so the field
// cannot be renamed, tagged or joined by a silent sibling without a
// deliberate edit here.
//
// Only the imports already in internal/domain/source_guard_test.go's test
// allowlist are used: no import is added to the package by this file.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// The status and command axes are NOT re-declared here. invariant_model_test.go
// already owns allRequirementStatuses() and allRequirementCommandKinds()
// together with wantRequirementStatusCount and wantRequirementCommandCount,
// and re-declaring them would let this file's table silently drift away from
// the axis the invariant tests explore. Reusing them means a status or a
// command kind added without revisiting this file fails the count guards
// below in the same edit.

// captureTimeFixture is an explicit instant. Nothing in this package may read
// a real clock (source_guard_test.go forbids the time.Now selector outright),
// so every timestamp here is a literal.
func captureTimeFixture() time.Time { return time.Unix(1_700_000_000, 123_456_789).UTC() }

func requirementWithStatus(t *testing.T, status RequirementStatus, capturedAt time.Time) Requirement {
	t.Helper()
	id, err := NewRequirementID("requirement-capture-time")
	if err != nil {
		t.Fatal(err)
	}
	r := Requirement{ID: id, Status: status, Version: 1, CapturedAt: capturedAt}
	if status == RequirementCompleted {
		// The only status Validate demands more of; the demand is about the
		// stable release snapshot and has nothing to do with the capture time.
		r.StableSnapshot = StableReleaseSnapshot{ReleaseID: "release-1", ReleaseVersion: 1, BundleDigest: "bundle", EvidenceDigest: "evidence"}
	}
	if status == RequirementPaused {
		// V2-090: a paused Requirement carries the status it was paused from,
		// exactly as a completed one carries its stable release snapshot above.
		// This is MANDATORY rather than cosmetic: RequirementResume joins the
		// command axis below and is admitted from paused only, so without a
		// memory here TestCapturedAtSurvivesEveryRequirementCommandKind would
		// fail at its "want a legal transition" assertion. Making resume
		// tolerate an empty memory instead would be the defect this task exists
		// to prevent -- a resume that lands a Requirement in a status it was
		// never in.
		r.PausedFrom = RequirementReady
	}
	return r
}

// TestValidateAcceptsBothARecordedAndAnAbsentCaptureTime is the legacy-record
// clause of A3 and A6: a zero CapturedAt is valid for every status, so no
// Requirement written before this field existed becomes invalid.
func TestValidateAcceptsBothARecordedAndAnAbsentCaptureTime(t *testing.T) {
	statuses := allRequirementStatuses()
	if len(statuses) != wantRequirementStatusCount {
		t.Fatalf("status axis drifted: %d statuses, want %d", len(statuses), wantRequirementStatusCount)
	}
	for _, status := range statuses {
		if !validRequirementStatus(status) {
			t.Fatalf("allRequirementStatuses() names %q, which validRequirementStatus rejects; the axis is stale", status)
		}
		recorded := requirementWithStatus(t, status, captureTimeFixture())
		if err := Validate(recorded); err != nil {
			t.Fatalf("status %q with a recorded capture time: Validate = %v, want nil", status, err)
		}
		if !recorded.CaptureRecorded() {
			t.Fatalf("status %q: CaptureRecorded() = false for a non-zero CapturedAt", status)
		}
		legacy := requirementWithStatus(t, status, time.Time{})
		if err := Validate(legacy); err != nil {
			t.Fatalf("status %q with no capture time (legacy record): Validate = %v, want nil", status, err)
		}
		if legacy.CaptureRecorded() {
			t.Fatalf("status %q: CaptureRecorded() = true for a zero CapturedAt", status)
		}
	}
}

// startStatusFor returns a status from which command is a legal transition,
// so the table below exercises the SUCCESS path of every kind and not just
// its refusal.
func startStatusFor(kind RequirementCommandKind) RequirementStatus {
	switch kind {
	case RequirementStartFraming:
		return RequirementCaptured
	case RequirementReadyCommand:
		return RequirementFraming
	case RequirementStart:
		return RequirementReady
	case RequirementWait:
		return RequirementReady
	case RequirementNeedInput:
		return RequirementFraming
	case RequirementRecover:
		return RequirementActive
	case RequirementEvaluate:
		return RequirementActive
	case RequirementPause:
		return RequirementReady
	case RequirementCancel:
		return RequirementActive
	case RequirementResume:
		// The only status a resume is admitted from. requirementWithStatus
		// gives a paused Requirement its memory, so this transition is legal
		// and lands back on RequirementReady.
		return RequirementPaused
	}
	return ""
}

// TestCapturedAtSurvivesEveryRequirementCommandKind is A3's table over every
// command kind. Byte-identity is asserted with time.Time.Equal AND with the
// monotonic-free triple (Unix seconds, nanoseconds, location name), because
// Equal alone would accept a value that was replaced by a different
// representation of the same instant, and the persisted form is the
// representation.
func TestCapturedAtSurvivesEveryRequirementCommandKind(t *testing.T) {
	at := captureTimeFixture()
	commandAt := time.Unix(1_800_000_000, 0).UTC()
	kinds := allRequirementCommandKinds()
	if len(kinds) != wantRequirementCommandCount {
		t.Fatalf("command axis drifted: %d kinds, want %d", len(kinds), wantRequirementCommandCount)
	}
	for _, kind := range kinds {
		start := startStatusFor(kind)
		if start == "" {
			t.Fatalf("kind %q has no declared start status; the table is incomplete", kind)
		}
		current := requirementWithStatus(t, start, at)
		next, err := DecideRequirement(current, RequirementCommand{Kind: kind, Actor: "actor-1", At: commandAt, ExpectedVersion: current.Version})
		if err != nil {
			t.Fatalf("kind %q from %q: DecideRequirement = %v, want a legal transition", kind, start, err)
		}
		if next.Status == current.Status {
			t.Fatalf("kind %q from %q did not change status; the start status is wrong and the table proves nothing", kind, start)
		}
		if next.Version != current.Version+1 {
			t.Fatalf("kind %q: version = %d, want %d", kind, next.Version, current.Version+1)
		}
		assertSameInstant(t, string(kind), current.CapturedAt, next.CapturedAt)
		if !next.CaptureRecorded() {
			t.Fatalf("kind %q: CaptureRecorded() = false after a transition", kind)
		}
		// The command's own At is a different instant from the capture time,
		// so a branch that overwrote CapturedAt with command.At would be
		// caught rather than pass by coincidence.
		if next.CapturedAt.Equal(commandAt) {
			t.Fatalf("kind %q: CapturedAt was overwritten with the command timestamp", kind)
		}
		// A legacy record stays legacy: no transition invents a capture time.
		legacy := requirementWithStatus(t, start, time.Time{})
		nextLegacy, err := DecideRequirement(legacy, RequirementCommand{Kind: kind, Actor: "actor-1", At: commandAt, ExpectedVersion: legacy.Version})
		if err != nil {
			t.Fatalf("kind %q from %q on a legacy record: DecideRequirement = %v", kind, start, err)
		}
		if !nextLegacy.CapturedAt.IsZero() || nextLegacy.CaptureRecorded() {
			t.Fatalf("kind %q invented a capture time for a legacy record: %v", kind, nextLegacy.CapturedAt)
		}
	}
	// An unknown kind is still refused, so the table above is a statement
	// about the closed switch and not about a permissive default.
	current := requirementWithStatus(t, RequirementCaptured, at)
	if _, err := DecideRequirement(current, RequirementCommand{Kind: RequirementCommandKind("not-a-kind"), Actor: "a", At: commandAt, ExpectedVersion: 1}); err == nil {
		t.Fatal("an unknown requirement command kind was accepted")
	}
}

// TestCapturedAtSurvivesCompleteRequirementFromRelease is the second half of
// A3's transition table: completion is not a RequirementCommandKind, so it
// needs its own assertion.
func TestCapturedAtSurvivesCompleteRequirementFromRelease(t *testing.T) {
	at := captureTimeFixture()
	current := requirementWithStatus(t, RequirementEvaluating, at)
	proof := validStableReleaseProof(t)
	next, err := CompleteRequirementFromRelease(current, proof, "actor-1", time.Unix(1_900_000_000, 0).UTC())
	if err != nil {
		t.Fatalf("CompleteRequirementFromRelease: %v", err)
	}
	if next.Status != RequirementCompleted {
		t.Fatalf("status = %q, want completed", next.Status)
	}
	assertSameInstant(t, "CompleteRequirementFromRelease", current.CapturedAt, next.CapturedAt)

	legacy := requirementWithStatus(t, RequirementEvaluating, time.Time{})
	nextLegacy, err := CompleteRequirementFromRelease(legacy, proof, "actor-1", time.Unix(1_900_000_000, 0).UTC())
	if err != nil {
		t.Fatalf("CompleteRequirementFromRelease on a legacy record: %v", err)
	}
	if !nextLegacy.CapturedAt.IsZero() {
		t.Fatalf("completion invented a capture time for a legacy record: %v", nextLegacy.CapturedAt)
	}
}

// validStableReleaseProof produces a real StableReleaseProof through the only
// path that can mint one, reusing invariant_model_test.go's fully satisfied
// candidate rather than constructing a proof value directly (the proof's data
// is unexported precisely so it cannot be forged).
func validStableReleaseProof(t *testing.T) StableReleaseProof {
	t.Helper()
	candidate := fullySatisfiedReleaseCandidate(t)
	control := EffectiveControlResult{Found: true, Mode: ControlAllow, Revision: candidate.ExpectedControlRevision}
	promoting, err := PromoteRelease(candidate, control)
	if err != nil {
		t.Fatalf("PromoteRelease: %v", err)
	}
	permit, err := Permit(control, PermitRequest{Kind: PermitPromotion, ControlRevision: candidate.ExpectedControlRevision, FencingToken: candidate.FencingToken, ExpectedFencingToken: candidate.FencingToken, Resource: "release"})
	if err != nil {
		t.Fatalf("Permit(promotion): %v", err)
	}
	_, proof, err := CompletePromotionWithProof(promoting, control, permit)
	if err != nil || !proof.valid() {
		t.Fatalf("CompletePromotionWithProof: valid=%v err=%v", proof.valid(), err)
	}
	return proof
}

// assertSameInstant compares two capture times as the persisted
// representation, not merely as instants.
func assertSameInstant(t *testing.T, label string, want, got time.Time) {
	t.Helper()
	if !want.Equal(got) {
		t.Fatalf("%s: capture time changed: %v -> %v", label, want, got)
	}
	if want.Unix() != got.Unix() || want.Nanosecond() != got.Nanosecond() {
		t.Fatalf("%s: capture time representation changed: %d.%09d -> %d.%09d", label, want.Unix(), want.Nanosecond(), got.Unix(), got.Nanosecond())
	}
	if want.Location().String() != got.Location().String() {
		t.Fatalf("%s: capture time location changed: %q -> %q", label, want.Location().String(), got.Location().String())
	}
}

// TestRequirementFieldListAndCaptureTagArePinned reads this package's own
// source and pins two facts A3 depends on: Requirement's field list is
// exactly the seven declared names, and CapturedAt carries no struct tag at
// all. Both are read from the AST rather than from a text scan, because a
// text scan cannot tell a struct field from a selector that reads it.
func TestRequirementFieldListAndCaptureTagArePinned(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "model.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse model.go: %v", err)
	}
	want := []string{"ID", "Status", "Version", "Increments", "StableSnapshot", "RequestedBy", "CapturedAt", "PausedFrom"}
	var got []string
	tagged := []string{}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name == nil || spec.Name.Name != "Requirement" {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		found = true
		for _, f := range st.Fields.List {
			for _, name := range f.Names {
				got = append(got, name.Name)
				if f.Tag != nil {
					tagged = append(tagged, name.Name)
				}
			}
		}
		return false
	})
	if !found {
		t.Fatal("the Requirement type declaration was not found in model.go; this scan proves nothing")
	}
	if len(got) != len(want) {
		t.Fatalf("Requirement fields = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Requirement fields = %v, want exactly %v", got, want)
		}
	}
	if len(tagged) != 0 {
		t.Fatalf("Requirement carries struct tags on %v; no field on this struct may carry one, so the persisted document stays consistent with itself", tagged)
	}
}
