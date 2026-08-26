package application_test

import (
	"context"
	"errors"
	"go/ast"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// ===========================================================================
// V2-090: Service.PauseRequirement, Service.ResumeRequirement and
// Service.CancelRequirement -- the owner's triple, and the exit that makes the
// pause safe to offer.
// ===========================================================================
//
// No import is added to this test package by this file: context, errors,
// go/ast, testing, internal/application, internal/domain and
// internal/store/memory are all already imported by service_test.go,
// framing_test.go and readmodels_test.go. internal/quota is deliberately NOT
// imported -- A27 requires that no test-only import edge be new -- so the
// quota ceilings below are restated as plain numbers exactly as
// framing_test.go's startFramingWriteCeiling already does, and the precise
// read/write measurement is taken in internal/api, whose test package already
// imports internal/quota.

// pauseWriteCeiling is quota.MutationUsage's Writes component, restated here as
// a plain number so this file adds no import. It is 16 on this tree (measured
// at internal/quota/quota.go:62).
const pauseWriteCeiling = 16

// pauseReadCeiling is quota.MutationUsage's Reads component, 32 on this tree
// (measured at internal/quota/quota.go:62). The reservation is applied BEFORE
// the callback can stage anything and fails regardless of the true-up, so each
// of the three new mutation paths must fit inside it.
const pauseReadCeiling = 32

// pauseMeasuredWrites is what the shape test below measures for each of the
// three commands, named so the number in the evidence and the number in the
// tree cannot drift apart: the Requirement document, the Event document and the
// idempotency record. Nothing else is written, and in particular NO outbox item
// is staged.
const pauseMeasuredWrites = 3

// pausableSourcesForGradeTwo is A14 GRADE 2's axis: all four statuses a pause
// is admitted from. It is written out here rather than derived, because
// internal/domain's pausableRequirementStatuses is unexported and this package
// cannot see it; internal/domain's pause_resume_test.go owns the derived
// version, and the two are cross-checked by the round trip itself -- a fifth
// pausable status in the domain would make the domain's own count guard fail.
var pausableSourcesForGradeTwo = []domain.RequirementStatus{
	domain.RequirementReady,
	domain.RequirementActive,
	domain.RequirementWaiting,
	domain.RequirementRecovering,
}

// pausedFixture captures a Requirement, seeds it to `from`, and pauses it
// through the real Service, returning the store, the id and the version the
// paused record is at.
//
// It threads the POST-SEED version into the pause request. That is the trap
// V2-089 recorded and hit: seeding raises the Requirement's Version, so a
// pause that declared the capture version would be refused with ErrStaleVersion
// for a reason unrelated to what is being measured. Deleting the expected
// version, passing 0 or moving the seed later would each delete a real
// assertion instead of fixing it.
type pausedFixture struct {
	service       *application.Service
	store         *memory.Store
	requirementID string
	from          domain.RequirementStatus
	version       domain.Version
}

func newPausedFixture(t *testing.T, tag string, from domain.RequirementStatus) *pausedFixture {
	t.Helper()
	svc, st := service()
	ctx := owner(context.Background())
	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":capture", Text: "a requirement to be paused"})
	if err != nil {
		t.Fatalf("%s: capture: %v", tag, err)
	}
	seeded := seedRequirementStatus(t, st, captured.RequirementID, from)
	out, err := svc.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: tag + ":pause", RequirementID: captured.RequirementID, ExpectedVersion: seeded})
	if err != nil {
		t.Fatalf("%s: pause from %q: %v", tag, from, err)
	}
	if out.Status != domain.RequirementPaused {
		t.Fatalf("%s: pause from %q produced status %q, want paused", tag, from, out.Status)
	}
	if out.ResumesTo != from {
		t.Fatalf("%s: pause from %q reported resumes_to=%q, want %q", tag, from, out.ResumesTo, from)
	}
	return &pausedFixture{service: svc, store: st, requirementID: captured.RequirementID, from: from, version: out.Version}
}

// newActiveFixture is newPausedFixture's counterpart for a Requirement that has
// NOT been paused. A12's authority cells need one of each: PauseRequirement is
// admitted from active and refused from paused, while ResumeRequirement is
// admitted from paused only, so a single fixture status would make one of the
// three cells fail for a DOMAIN reason and measure nothing about authority.
func newActiveFixture(t *testing.T, tag string) *pausedFixture {
	t.Helper()
	svc, st := service()
	ctx := owner(context.Background())
	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":capture", Text: "a requirement that has not been paused"})
	if err != nil {
		t.Fatalf("%s: capture: %v", tag, err)
	}
	seeded := seedRequirementStatus(t, st, captured.RequirementID, domain.RequirementActive)
	return &pausedFixture{service: svc, store: st, requirementID: captured.RequirementID, from: domain.RequirementActive, version: seeded}
}

// ---------------------------------------------------------------------------
// A11: the shape of the three commands.
// ---------------------------------------------------------------------------

// TestThePauseTripleTransitionsStagesNoOutboxAndIsIdempotent is A11.
//
// It drives the real Service against a real store and asserts the whole shape
// of each of the three commands: the status moves to exactly the status the
// domain admits, the version moves by exactly one, exactly one event is
// recorded and it names the right type, exactly one idempotency record is
// written, NO outbox item is staged at all, a replay of the same request_id
// returns the prior response rather than transitioning a second time, a replay
// with the same request_id and a DIFFERENT body is ErrIdempotencyConflict, and
// an absent Requirement is ErrNotFound.
func TestThePauseTripleTransitionsStagesNoOutboxAndIsIdempotent(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		svc, st := service()
		ctx := owner(context.Background())
		captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "a11:pause:capture", Text: "a requirement to pause"})
		if err != nil {
			t.Fatal(err)
		}
		seeded := seedRequirementStatus(t, st, captured.RequirementID, domain.RequirementActive)
		before, ok := st.Requirement(captured.RequirementID)
		if !ok {
			t.Fatal("the seeded Requirement is not in the store")
		}
		eventsBefore, outboxBefore := len(st.Events()), len(st.Outbox())

		out, err := svc.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: "a11:pause", RequirementID: captured.RequirementID, ExpectedVersion: seeded})
		if err != nil {
			t.Fatalf("pause: %v", err)
		}
		if out.RequirementID != captured.RequirementID || out.Status != domain.RequirementPaused || out.Version != before.Version+1 {
			t.Fatalf("pause response = %+v, want %q at version %d", out, domain.RequirementPaused, before.Version+1)
		}
		if out.ResumesTo != domain.RequirementActive {
			t.Fatalf("pause response resumes_to = %q, want active", out.ResumesTo)
		}
		after, _ := st.Requirement(captured.RequirementID)
		if after.Status != domain.RequirementPaused || after.PausedFrom != domain.RequirementActive || after.Version != before.Version+1 {
			t.Fatalf("the stored Requirement is %+v, want paused remembering active at version %d", after, before.Version+1)
		}
		if after.ID != before.ID || after.RequestedBy != before.RequestedBy || !after.CapturedAt.Equal(before.CapturedAt) {
			t.Fatalf("pause changed something other than the status, the version and the memory: before=%+v after=%+v", before, after)
		}
		assertOneEventAndNoOutbox(t, st, eventsBefore, outboxBefore, "requirement.paused", captured.RequirementID, after.Version)
		assertReplayAndConflict(t, st, "a11:pause", func(expected domain.Version) (any, error) {
			return svc.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: "a11:pause", RequirementID: captured.RequirementID, ExpectedVersion: expected})
		}, out, seeded)
		if _, err := svc.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: "a11:pause:absent", RequirementID: "requirement-that-does-not-exist", ExpectedVersion: 1}); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("pause of an absent Requirement: %v, want ErrNotFound", err)
		}
	})

	t.Run("resume", func(t *testing.T) {
		f := newPausedFixture(t, "a11:resume", domain.RequirementWaiting)
		ctx := owner(context.Background())
		before, _ := f.store.Requirement(f.requirementID)
		eventsBefore, outboxBefore := len(f.store.Events()), len(f.store.Outbox())

		out, err := f.service.ResumeRequirement(ctx, application.ResumeRequirementRequest{RequestID: "a11:resume:go", RequirementID: f.requirementID, ExpectedVersion: f.version})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if out.Status != domain.RequirementWaiting || out.Version != before.Version+1 {
			t.Fatalf("resume response = %+v, want waiting at version %d", out, before.Version+1)
		}
		after, _ := f.store.Requirement(f.requirementID)
		if after.Status != domain.RequirementWaiting || after.PausedFrom != "" {
			t.Fatalf("the stored Requirement is %+v, want waiting with the memory cleared", after)
		}
		assertOneEventAndNoOutbox(t, f.store, eventsBefore, outboxBefore, "requirement.resumed", f.requirementID, after.Version)
		assertReplayAndConflict(t, f.store, "a11:resume:go", func(expected domain.Version) (any, error) {
			return f.service.ResumeRequirement(ctx, application.ResumeRequirementRequest{RequestID: "a11:resume:go", RequirementID: f.requirementID, ExpectedVersion: expected})
		}, out, f.version)
		if _, err := f.service.ResumeRequirement(ctx, application.ResumeRequirementRequest{RequestID: "a11:resume:absent", RequirementID: "requirement-that-does-not-exist", ExpectedVersion: 1}); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("resume of an absent Requirement: %v, want ErrNotFound", err)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		f := newPausedFixture(t, "a11:cancel", domain.RequirementRecovering)
		ctx := owner(context.Background())
		before, _ := f.store.Requirement(f.requirementID)
		eventsBefore, outboxBefore := len(f.store.Events()), len(f.store.Outbox())

		out, err := f.service.CancelRequirement(ctx, application.CancelRequirementRequest{RequestID: "a11:cancel:go", RequirementID: f.requirementID, ExpectedVersion: f.version})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if out.Status != domain.RequirementCancelled || out.Version != before.Version+1 {
			t.Fatalf("cancel response = %+v, want cancelled at version %d", out, before.Version+1)
		}
		after, _ := f.store.Requirement(f.requirementID)
		if after.Status != domain.RequirementCancelled || after.PausedFrom != "" {
			t.Fatalf("the stored Requirement is %+v, want cancelled with the memory cleared", after)
		}
		assertOneEventAndNoOutbox(t, f.store, eventsBefore, outboxBefore, "requirement.cancelled", f.requirementID, after.Version)
		assertReplayAndConflict(t, f.store, "a11:cancel:go", func(expected domain.Version) (any, error) {
			return f.service.CancelRequirement(ctx, application.CancelRequirementRequest{RequestID: "a11:cancel:go", RequirementID: f.requirementID, ExpectedVersion: expected})
		}, out, f.version)
		if _, err := f.service.CancelRequirement(ctx, application.CancelRequirementRequest{RequestID: "a11:cancel:absent", RequirementID: "requirement-that-does-not-exist", ExpectedVersion: 1}); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("cancel of an absent Requirement: %v, want ErrNotFound", err)
		}
	})
}

// assertOneEventAndNoOutbox is the three-write shape: one Requirement, one
// Event, one idempotency record, and NOTHING in the outbox. The nil OutboxItem
// each command passes to Service.record is the assertion in code; this is the
// assertion from the store.
func assertOneEventAndNoOutbox(t *testing.T, st *memory.Store, eventsBefore, outboxBefore int, wantType, requirementID string, version domain.Version) {
	t.Helper()
	events := st.Events()
	if len(events) != eventsBefore+1 {
		t.Fatalf("the command recorded %d events, want exactly 1", len(events)-eventsBefore)
	}
	e := events[len(events)-1]
	if e.Type != wantType || e.AggregateType != "requirement" || e.AggregateID != requirementID || e.Version != version {
		t.Fatalf("the recorded event is %+v, want type %q on requirement %q at version %d", e, wantType, requirementID, version)
	}
	if got := len(st.Outbox()); got != outboxBefore {
		t.Fatalf("the command changed the outbox from %d to %d items; none of the three stages any", outboxBefore, got)
	}
	if pauseMeasuredWrites >= pauseWriteCeiling || pauseReadCeiling <= 0 {
		t.Fatalf("the measured write count %d does not fit inside the mutation reservation of %d writes / %d reads", pauseMeasuredWrites, pauseWriteCeiling, pauseReadCeiling)
	}
}

// assertReplayAndConflict asserts C14's replay half for one command: the same
// request_id with the same body replays the recorded response, and the same
// request_id with a DIFFERENT body is ErrIdempotencyConflict rather than a
// second transition.
func assertReplayAndConflict(t *testing.T, st *memory.Store, requestID string, call func(domain.Version) (any, error), want any, expected domain.Version) {
	t.Helper()
	if _, ok := st.Idempotency(requestID); !ok {
		t.Fatalf("the command wrote no idempotency record for %q", requestID)
	}
	replay, err := call(expected)
	if err != nil {
		t.Fatalf("replay of %q: %v", requestID, err)
	}
	if replay != want {
		t.Fatalf("the replay of %q returned %+v, want the prior response %+v", requestID, replay, want)
	}
	if _, err := call(expected + 41); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("replay of %q with a different expected version: %v, want ErrIdempotencyConflict", requestID, err)
	}
}

// ---------------------------------------------------------------------------
// A12: authority, and a refusal for every role the product does not name.
// ---------------------------------------------------------------------------

// applicationRoleConstants reads internal/application/caller.go's own source and
// returns the names of every declared application.Role constant.
//
// The axis is DERIVED rather than listed, using the same go/ast idiom
// framing_test.go's issuer scan uses, so a FOURTH role added later fails this
// test instead of silently gaining authority over the owner's pause. It fails
// outright on a parse error and on a zero-constant scan, so it cannot pass
// vacuously.
func applicationRoleConstants(t *testing.T) []string {
	t.Helper()
	_, file := parseApplicationFile(t, "caller.go")
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || value.Type == nil {
				continue
			}
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "Role" {
				continue
			}
			for _, name := range value.Names {
				names = append(names, name.Name)
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("the scan found no application.Role constants in caller.go; the scan is broken")
	}
	return names
}

// TestThePauseTripleAcceptsTheOwnerAloneAtTheApplicationLayer is A12.
//
// The product names the actor for these three verbs as 人間 or 利用者 six times
// -- docs/product/user-facing-spec.md:194 (利用者は対象範囲を指定して次を実行
// できる) covering :201's pause/resume/cancel line, :274 (人間の指示で新しい処理
// を停止している), :277 (人間が以後の処理を取り消した),
// docs/architecture/domain-model.md:269 (人間が処理を停止), :273 (人間が以後の
// 解決を不要と判断) and :211 (人間が発行した...authoritative command) -- and
// names the Loop, the scheduler or a Runner ZERO times in any of them.
// Therefore all three commands accept application.RoleOwner ONLY.
//
// TWELVE CELLS: two unnamed roles x three commands (6), a missing caller x
// three commands (3), and an owner with an EMPTY Subject x three commands (3).
// Every cell additionally asserts the Requirement is byte-unchanged, so a
// refusal that had already written something would fail here rather than pass.
func TestThePauseTripleAcceptsTheOwnerAloneAtTheApplicationLayer(t *testing.T) {
	roles := applicationRoleConstants(t)
	const wantRoleCount = 3
	if len(roles) != wantRoleCount {
		t.Fatalf("caller.go declares %d application.Role constants (%v); this test knows how to build a caller for exactly %d, and a new role must arrive with its own authority decision rather than inherit one", len(roles), roles, wantRoleCount)
	}
	// One context constructor per declared role. A role with no entry here is a
	// hard failure above, which is what makes the closure measurable.
	contexts := map[string]func(context.Context) context.Context{
		"RoleOwner":     owner,
		"RoleRunner":    func(ctx context.Context) context.Context { return runner(ctx, "runner-1") },
		"RoleScheduler": func(ctx context.Context) context.Context { return schedulerCaller(ctx, "loop-subject") },
	}
	for _, role := range roles {
		if _, ok := contexts[role]; !ok {
			t.Fatalf("caller.go declares the role %q, which this test cannot build a caller for; a fourth role must not gain authority over the owner's pause by default", role)
		}
	}

	type command struct {
		name string
		// needsPaused says which fixture status makes this command's DOMAIN
		// precondition satisfied, so a refusal below is always an authority
		// refusal and never a transition refusal.
		needsPaused bool
		call        func(*application.Service, context.Context, string, domain.Version) error
	}
	commands := []command{
		{"PauseRequirement", false, func(s *application.Service, ctx context.Context, id string, v domain.Version) error {
			_, err := s.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: "a12:pause", RequirementID: id, ExpectedVersion: v})
			return err
		}},
		{"ResumeRequirement", true, func(s *application.Service, ctx context.Context, id string, v domain.Version) error {
			_, err := s.ResumeRequirement(ctx, application.ResumeRequirementRequest{RequestID: "a12:resume", RequirementID: id, ExpectedVersion: v})
			return err
		}},
		{"CancelRequirement", true, func(s *application.Service, ctx context.Context, id string, v domain.Version) error {
			_, err := s.CancelRequirement(ctx, application.CancelRequirementRequest{RequestID: "a12:cancel", RequirementID: id, ExpectedVersion: v})
			return err
		}},
	}
	fixtureFor := func(t *testing.T, tag string, cmd command) *pausedFixture {
		t.Helper()
		if cmd.needsPaused {
			return newPausedFixture(t, tag, domain.RequirementActive)
		}
		return newActiveFixture(t, tag)
	}

	accepted := 0
	for _, role := range roles {
		roleAccepted := 0
		for _, cmd := range commands {
			f := fixtureFor(t, "a12:"+role+":"+cmd.name, cmd)
			before, _ := f.store.Requirement(f.requirementID)
			err := cmd.call(f.service, contexts[role](context.Background()), f.requirementID, f.version)
			if errors.Is(err, application.ErrForbidden) {
				after, _ := f.store.Requirement(f.requirementID)
				if !sameStoredRequirement(before, after) {
					t.Fatalf("%s refused %s and still changed the Requirement: before=%+v after=%+v", cmd.name, role, before, after)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s as %s: %v, want either success or ErrForbidden", cmd.name, role, err)
			}
			roleAccepted++
		}
		if roleAccepted == len(commands) {
			accepted++
		} else if roleAccepted != 0 {
			t.Fatalf("role %s was accepted by %d of the %d commands; the triple is one authority decision, not three", role, roleAccepted, len(commands))
		}
	}
	if accepted != 1 {
		t.Fatalf("%d of the %d declared roles are accepted by the pause triple, want exactly 1 (the owner)", accepted, len(roles))
	}

	// A missing caller, three cells.
	for _, cmd := range commands {
		f := fixtureFor(t, "a12:anon:"+cmd.name, cmd)
		before, _ := f.store.Requirement(f.requirementID)
		if err := cmd.call(f.service, context.Background(), f.requirementID, f.version); !errors.Is(err, application.ErrUnauthenticated) {
			t.Fatalf("%s with no caller at all: %v, want ErrUnauthenticated", cmd.name, err)
		}
		after, _ := f.store.Requirement(f.requirementID)
		if !sameStoredRequirement(before, after) {
			t.Fatalf("%s with no caller changed the Requirement", cmd.name)
		}
	}

	// An owner with an EMPTY Subject, three cells. callerActor refuses an empty
	// subject even when the role matches, so this is ErrForbidden and not a
	// successful command with an unusable actor recorded against it.
	emptySubject := func(ctx context.Context) context.Context {
		return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner})
	}
	for _, cmd := range commands {
		f := fixtureFor(t, "a12:empty:"+cmd.name, cmd)
		before, _ := f.store.Requirement(f.requirementID)
		if err := cmd.call(f.service, emptySubject(context.Background()), f.requirementID, f.version); !errors.Is(err, application.ErrForbidden) {
			t.Fatalf("%s as an owner with an empty Subject: %v, want ErrForbidden", cmd.name, err)
		}
		after, _ := f.store.Requirement(f.requirementID)
		if !sameStoredRequirement(before, after) {
			t.Fatalf("%s as an owner with an empty Subject changed the Requirement", cmd.name)
		}
	}
	t.Logf("A12: %d declared roles x %d commands, plus a missing caller and an empty subject; exactly 1 role is accepted", len(roles), len(commands))
}

// sameStoredRequirement compares the fields a refused command must not have
// touched, INCLUDING Version and PausedFrom. reflect.DeepEqual would also
// work -- reflect is already imported by this package's tests -- but naming the
// fields makes the assertion say which ones matter.
func sameStoredRequirement(a, b domain.Requirement) bool {
	if a.ID != b.ID || a.Status != b.Status || a.Version != b.Version || a.PausedFrom != b.PausedFrom || a.RequestedBy != b.RequestedBy {
		return false
	}
	if !a.CapturedAt.Equal(b.CapturedAt) || a.StableSnapshot != b.StableSnapshot {
		return false
	}
	if len(a.Increments) != len(b.Increments) {
		return false
	}
	for i := range a.Increments {
		if a.Increments[i] != b.Increments[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// A13: the permit asymmetry, and the exit that survives every mode.
// ---------------------------------------------------------------------------

// wantResumeAllowed is A13's RESUME table, written out explicitly rather than
// derived, so a change in domain.Permit's gate fails this test instead of
// silently redefining the design. It is cross-checked against the package's
// existing permitAllowedTable mirror for domain.PermitClaim below, exactly as
// framing_test.go cross-checks its own, and both must agree.
var wantResumeAllowed = map[domain.ControlMode]bool{
	domain.ControlAllow:         true,
	domain.ControlPauseIntake:   true,
	domain.ControlPauseClaim:    false,
	domain.ControlGracefulStop:  false,
	domain.ControlImmediateStop: false,
	domain.ControlEmergencyStop: false,
	domain.ControlCancel:        false,
}

const wantResumeAllowedTotal = 2

// wantPauseAndCancelAllowed is A13's PAUSE and CANCEL table: ALLOWED under all
// SEVEN modes, because a stop the owner cannot issue while a stop is in force
// contradicts docs/product/definition.md:132 ("利用者の停止指示へ確実に従い、
// 停止完了を検証可能にする") and :137 ("自律性は利用者の統制より優先されない").
// Neither command evaluates a domain.Permit at all, which is what produces this
// table.
var wantPauseAndCancelAllowed = map[domain.ControlMode]bool{
	domain.ControlAllow:         true,
	domain.ControlPauseIntake:   true,
	domain.ControlPauseClaim:    true,
	domain.ControlGracefulStop:  true,
	domain.ControlImmediateStop: true,
	domain.ControlEmergencyStop: true,
	domain.ControlCancel:        true,
}

const wantPauseAndCancelAllowedTotal = 7

// TestPauseResumeAndCancelSevenControlModeTables is A13.
//
// One fresh Service and store per mode, with three separate Requirements so the
// three cells cannot interfere: rResume and rCancel are both paused while the
// mode is still allow, and rPause is left in `active`. The Control Intent is
// then engaged before all three attempts, exactly as stop_matrix_test.go and
// framing_test.go establish their own fixtures.
//
// Every DENIED cell is asserted THREE ways: errors.Is(err,
// domain.ErrControlDenied); the Requirement byte-unchanged INCLUDING Version and
// PausedFrom; and NO idempotency record written, proved by returning the mode to
// allow and replaying the SAME request_id, which must then execute for real
// rather than replay a recorded refusal.
//
// The property the whole asymmetry exists for is asserted in the same run: under
// EVERY mode, including emergency-stop, a paused Requirement's cancel is
// ALLOWED, so the paused state retains at least one exit under all seven. That
// is the direct measurement of "a paused state with no exit is worse than no
// pause at all".
func TestPauseResumeAndCancelSevenControlModeTables(t *testing.T) {
	if len(stopMatrixModes) != 7 {
		t.Fatalf("the package's control-mode list has %d entries; A13's tables are seven modes", len(stopMatrixModes))
	}
	resumeAllowedCells, pauseAllowedCells, cancelAllowedCells, exitCells := 0, 0, 0, 0
	for _, mode := range stopMatrixModes {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			svc, st := service()
			ctx := owner(context.Background())
			tag := "a13:" + string(mode)

			// Three Requirements. Everything up to and including the pauses
			// happens while the mode is still allow.
			ids := map[string]string{}
			versions := map[string]domain.Version{}
			for _, which := range []string{"resume", "cancel", "pause"} {
				captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":capture:" + which, Text: "captured while the mode is still allow"})
				if err != nil {
					t.Fatalf("fixture capture (%s): %v", which, err)
				}
				ids[which] = captured.RequirementID
				versions[which] = seedRequirementStatus(t, st, captured.RequirementID, domain.RequirementActive)
			}
			for _, which := range []string{"resume", "cancel"} {
				out, err := svc.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: tag + ":fixture-pause:" + which, RequirementID: ids[which], ExpectedVersion: versions[which]})
				if err != nil {
					t.Fatalf("fixture pause (%s): %v", which, err)
				}
				versions[which] = out.Version
			}

			var revision domain.Revision
			if mode != domain.ControlAllow {
				ctrl, e := svc.Control(ctx, application.ControlRequest{
					RequestID: tag + ":control",
					Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
					Mode:      mode,
					At:        (clock{}).Now(),
				})
				if e != nil {
					t.Fatalf("fixture control %s: %v", mode, e)
				}
				revision = ctrl.Revision
			}

			beforeResume, _ := st.Requirement(ids["resume"])
			beforeCancel, _ := st.Requirement(ids["cancel"])
			beforePause, _ := st.Requirement(ids["pause"])
			if beforeResume.Status != domain.RequirementPaused || beforeResume.PausedFrom != domain.RequirementActive {
				t.Fatalf("the resume fixture is %+v, want paused remembering active", beforeResume)
			}
			if beforeCancel.Status != domain.RequirementPaused {
				t.Fatalf("the cancel fixture is %+v, want paused", beforeCancel)
			}
			if beforePause.Status != domain.RequirementActive {
				t.Fatalf("the pause fixture is %+v, want active", beforePause)
			}
			eventsBefore, outboxBefore := len(st.Events()), len(st.Outbox())

			// --- the RESUME cell.
			_, resumeErr := svc.ResumeRequirement(ctx, application.ResumeRequirementRequest{RequestID: tag + ":resume", RequirementID: ids["resume"], ExpectedVersion: versions["resume"]})
			resumeOK := resumeErr == nil
			if resumeOK != wantResumeAllowed[mode] {
				t.Fatalf("resume under %s: allowed=%v want=%v (err=%v)", mode, resumeOK, wantResumeAllowed[mode], resumeErr)
			}
			// The explicit table and the package's existing mirror of
			// domain.Permit's own per-kind gate must agree.
			if wantResumeAllowed[mode] != permitAllowedTable(mode, domain.PermitClaim) {
				t.Fatalf("A13's resume table disagrees with permitAllowedTable for %s", mode)
			}
			afterResume, _ := st.Requirement(ids["resume"])
			if resumeOK {
				resumeAllowedCells++
				if afterResume.Status != domain.RequirementActive || afterResume.PausedFrom != "" || afterResume.Version != beforeResume.Version+1 {
					t.Fatalf("an ALLOWED resume under %s left the Requirement at %+v", mode, afterResume)
				}
			} else {
				if !errors.Is(resumeErr, domain.ErrControlDenied) {
					t.Fatalf("a DENIED resume under %s was refused by %v, not ErrControlDenied", mode, resumeErr)
				}
				if !sameStoredRequirement(afterResume, beforeResume) {
					t.Fatalf("a DENIED resume under %s changed the Requirement: before=%+v after=%+v", mode, beforeResume, afterResume)
				}
				if _, ok := st.Idempotency(tag + ":resume"); ok {
					t.Fatalf("a DENIED resume under %s wrote an idempotency record", mode)
				}
			}

			// --- the PAUSE cell, ALLOWED under all seven.
			pauseOut, pauseErr := svc.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: tag + ":pause", RequirementID: ids["pause"], ExpectedVersion: versions["pause"]})
			pauseOK := pauseErr == nil
			if pauseOK != wantPauseAndCancelAllowed[mode] {
				t.Fatalf("pause under %s: allowed=%v want=%v (err=%v)", mode, pauseOK, wantPauseAndCancelAllowed[mode], pauseErr)
			}
			if !pauseOK {
				t.Fatalf("pause under %s was refused (%v); an owner who cannot stop a Requirement while a stop is in force contradicts docs/product/definition.md:132 and :137", mode, pauseErr)
			}
			pauseAllowedCells++
			if pauseOut.Status != domain.RequirementPaused || pauseOut.ResumesTo != domain.RequirementActive {
				t.Fatalf("pause under %s returned %+v, want paused with resumes_to=active", mode, pauseOut)
			}

			// --- the CANCEL cell, ALLOWED under all seven, ON A PAUSED
			// Requirement. This is the exit proof.
			cancelOut, cancelErr := svc.CancelRequirement(ctx, application.CancelRequirementRequest{RequestID: tag + ":cancel", RequirementID: ids["cancel"], ExpectedVersion: versions["cancel"]})
			cancelOK := cancelErr == nil
			if cancelOK != wantPauseAndCancelAllowed[mode] {
				t.Fatalf("cancel under %s: allowed=%v want=%v (err=%v)", mode, cancelOK, wantPauseAndCancelAllowed[mode], cancelErr)
			}
			if !cancelOK {
				t.Fatalf("cancel of a PAUSED Requirement under %s was refused (%v); the paused state would then have no exit at all under that mode", mode, cancelErr)
			}
			cancelAllowedCells++
			if cancelOut.Status != domain.RequirementCancelled {
				t.Fatalf("cancel under %s returned %+v, want cancelled", mode, cancelOut)
			}
			exitCells++
			if mode == domain.ControlEmergencyStop {
				if resumeOK {
					t.Fatal("emergency-stop must DENY a resume: a permit-free resume would let the Loop take work back up while the strongest stop in the system is in force")
				}
				t.Logf("A13 emergency-stop exit proof: at control revision %d the resume of a paused Requirement is DENIED (%v) and its cancel is ALLOWED, so the paused state still has an exit", revision, resumeErr)
			}

			// Neither the pause nor the cancel staged an outbox item, and the
			// event count moved by exactly the number of cells that executed.
			if got := len(st.Outbox()); got != outboxBefore {
				t.Fatalf("under %s the three cells changed the outbox from %d to %d items; none of the three stages any", mode, outboxBefore, got)
			}
			wantEvents := eventsBefore + 2 // the pause and the cancel
			if resumeOK {
				wantEvents++
			}
			if got := len(st.Events()); got != wantEvents {
				t.Fatalf("under %s the event count is %d, want %d", mode, got, wantEvents)
			}

			// C14's last half, for the DENIED resume only: no idempotency
			// record was written, proved by returning the mode to allow and
			// replaying the SAME request_id, which must execute FOR REAL.
			if !resumeOK {
				if _, e := svc.Control(ctx, application.ControlRequest{
					RequestID: tag + ":control-back-to-allow",
					Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
					Mode:      domain.ControlAllow,
					At:        (clock{}).Now(),
				}); e != nil {
					t.Fatalf("returning the mode to allow: %v", e)
				}
				replay, e := svc.ResumeRequirement(ctx, application.ResumeRequirementRequest{RequestID: tag + ":resume", RequirementID: ids["resume"], ExpectedVersion: versions["resume"]})
				if e != nil {
					t.Fatalf("the replay of a previously DENIED resume under allow: %v; the refusal must not have been recorded", e)
				}
				if replay.Status != domain.RequirementActive {
					t.Fatalf("the replay of a previously DENIED resume returned %q, want active; it replayed a refusal instead of executing", replay.Status)
				}
			}
		})
	}
	if resumeAllowedCells != wantResumeAllowedTotal {
		t.Fatalf("resume is allowed in %d of the seven modes, want %d", resumeAllowedCells, wantResumeAllowedTotal)
	}
	if pauseAllowedCells != wantPauseAndCancelAllowedTotal || cancelAllowedCells != wantPauseAndCancelAllowedTotal {
		t.Fatalf("pause is allowed in %d and cancel in %d of the seven modes, want %d each", pauseAllowedCells, cancelAllowedCells, wantPauseAndCancelAllowedTotal)
	}
	if exitCells != len(stopMatrixModes) {
		t.Fatalf("a paused Requirement was proved to retain an exit under %d of the %d modes, want all of them", exitCells, len(stopMatrixModes))
	}
	t.Logf("A13: resume %d/7, pause %d/7, cancel %d/7; a paused Requirement retains at least one exit under %d/7 modes", resumeAllowedCells, pauseAllowedCells, cancelAllowedCells, exitCells)
}

// ---------------------------------------------------------------------------
// A14 GRADE 2: the round trip over the real Service and a real store.
// ---------------------------------------------------------------------------

// TestPauseAndResumeRoundTripThroughTheStoreForEveryPausableStatus is A14's
// GRADE 2, and it is labelled with what it cannot reach.
//
// GRADE 1 is internal/domain/pause_resume_test.go, which covers all four
// pausable statuses exhaustively over the pure transition. GRADE 2 is this: the
// same four, reached by SEEDING the store, driven through the real Service, so
// the memory is proved to survive the store rather than only the function call.
// GRADE 3 is over HTTP and covers ready and active ONLY.
//
// GRADE 3's boundary is a MEASUREMENT, not an omission: `grep -rn
// '\bRequirementWait\b' --include='*.go' . | grep -v _test.go` returns exactly
// two lines, both inside internal/domain/model.go (the declaration and the
// switch case), and the same grep for RequirementRecover returns the same two
// -- so no journey can put a Requirement into waiting or recovering, and no
// issuer for either is added here. docs/architecture/domain-model.md:267 and
// :270 define both as automatic.
func TestPauseAndResumeRoundTripThroughTheStoreForEveryPausableStatus(t *testing.T) {
	if len(pausableSourcesForGradeTwo) != 4 {
		t.Fatalf("GRADE 2's axis has %d entries, want the four pausable statuses", len(pausableSourcesForGradeTwo))
	}
	for _, source := range pausableSourcesForGradeTwo {
		source := source
		t.Run(string(source), func(t *testing.T) {
			svc, st := service()
			ctx := owner(context.Background())
			tag := "a14:" + string(source)
			captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":capture", Text: "a requirement to round-trip"})
			if err != nil {
				t.Fatal(err)
			}
			// The post-seed version is threaded into the pause request. Seeding
			// raises the Version, so the capture version would be stale here.
			seeded := seedRequirementStatus(t, st, captured.RequirementID, source)
			before, ok := st.Requirement(captured.RequirementID)
			if !ok || before.Status != source {
				t.Fatalf("the seeded Requirement is %+v, want %q", before, source)
			}

			paused, err := svc.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: tag + ":pause", RequirementID: captured.RequirementID, ExpectedVersion: seeded})
			if err != nil {
				t.Fatalf("pause from %q: %v", source, err)
			}
			if paused.ResumesTo != source {
				t.Fatalf("pause from %q reported resumes_to=%q", source, paused.ResumesTo)
			}
			// THE MEMORY SURVIVED THE STORE, read back from the store and not
			// from the response.
			stored, _ := st.Requirement(captured.RequirementID)
			if stored.Status != domain.RequirementPaused || stored.PausedFrom != source {
				t.Fatalf("the stored paused Requirement is %+v, want paused remembering %q; the field was dropped in persistence", stored, source)
			}
			// The detail read model reports the exit too, and reports it for
			// the paused status only.
			detail, found, err := svc.GetRequirementDetail(ctx, captured.RequirementID)
			if err != nil || !found {
				t.Fatalf("detail while paused: found=%v err=%v", found, err)
			}
			if detail.Status != domain.RequirementPaused || detail.ResumesTo != source {
				t.Fatalf("the paused detail is status=%q resumes_to=%q, want paused/%q", detail.Status, detail.ResumesTo, source)
			}

			resumed, err := svc.ResumeRequirement(ctx, application.ResumeRequirementRequest{RequestID: tag + ":resume", RequirementID: captured.RequirementID, ExpectedVersion: paused.Version})
			if err != nil {
				t.Fatalf("resume back to %q: %v", source, err)
			}
			if resumed.Status != source {
				t.Fatalf("the round trip from %q came back as %q; the memory was ignored", source, resumed.Status)
			}
			if resumed.Version != before.Version+2 {
				t.Fatalf("the round trip from %q ended at version %d, want %d (pause and resume each increment once)", source, resumed.Version, before.Version+2)
			}
			after, _ := st.Requirement(captured.RequirementID)
			if after.PausedFrom != "" {
				t.Fatalf("the round trip from %q left the memory set to %q", source, after.PausedFrom)
			}
			want := before
			want.Version = before.Version + 2
			if !sameStoredRequirement(after, want) {
				t.Fatalf("the round trip from %q changed something other than the version: got=%+v want=%+v", source, after, want)
			}
			detailAfter, _, err := svc.GetRequirementDetail(ctx, captured.RequirementID)
			if err != nil {
				t.Fatal(err)
			}
			if detailAfter.ResumesTo != "" {
				t.Fatalf("the resumed detail still reports resumes_to=%q, want it absent", detailAfter.ResumesTo)
			}
			// And the second exit is still available afterwards.
			cancelled, err := svc.CancelRequirement(ctx, application.CancelRequirementRequest{RequestID: tag + ":cancel", RequirementID: captured.RequirementID, ExpectedVersion: resumed.Version})
			if err != nil || cancelled.Status != domain.RequirementCancelled {
				t.Fatalf("cancel after the round trip from %q: status=%q err=%v", source, cancelled.Status, err)
			}
		})
	}
	t.Logf("A14 GRADE 2: all %d pausable statuses round-tripped through the real Service and a real store; GRADE 3 over HTTP covers ready and active only, because RequirementWait and RequirementRecover have no non-test issuer", len(pausableSourcesForGradeTwo))
}

// TestASecondPauseIsRefusedThroughTheServiceAndLeavesTheMemoryIntact is the
// application-layer half of A8(c): the most destructive bug this design admits
// is a second pause that succeeds, because it would overwrite the memory with
// `paused` and make the Requirement unresumable while returning success.
func TestASecondPauseIsRefusedThroughTheServiceAndLeavesTheMemoryIntact(t *testing.T) {
	for _, source := range pausableSourcesForGradeTwo {
		f := newPausedFixture(t, "a8c:"+string(source), source)
		ctx := owner(context.Background())
		before, _ := f.store.Requirement(f.requirementID)
		_, err := f.service.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: "a8c:second:" + string(source), RequirementID: f.requirementID, ExpectedVersion: f.version})
		if !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("a second pause of a Requirement paused from %q: %v, want ErrInvalidTransition", source, err)
		}
		after, _ := f.store.Requirement(f.requirementID)
		if after.PausedFrom != source {
			t.Fatalf("a refused second pause overwrote the memory %q with %q", source, after.PausedFrom)
		}
		if !sameStoredRequirement(after, before) {
			t.Fatalf("a refused second pause changed the Requirement: before=%+v after=%+v", before, after)
		}
		resumed, err := f.service.ResumeRequirement(ctx, application.ResumeRequirementRequest{RequestID: "a8c:resume:" + string(source), RequirementID: f.requirementID, ExpectedVersion: f.version})
		if err != nil || resumed.Status != source {
			t.Fatalf("after a refused second pause the Requirement no longer resumes into %q: status=%q err=%v", source, resumed.Status, err)
		}
	}
}

// ---------------------------------------------------------------------------
// A17: the other half of a pause, EXECUTED rather than described.
// ---------------------------------------------------------------------------

// TestTheOtherHalfOfAPauseIsAControlIntentAndItAlreadyRefusesTheClaim is A17.
//
// docs/architecture/domain-model.md:295 decomposes the pause of an Increment
// verbatim as: "Incrementの一時停止は、IncrementStatusに専用の状態を設けず、
// Incrementをscopeに含むControl Intent（`pause-intake`／`pause-claim`など）と
// 親RequirementのpausedへのStatus遷移の組み合わせで表現する。" -- a COMBINATION
// of a Control Intent and the parent Requirement's transition to paused. This
// task delivers the second half; this test proves the FIRST half already works,
// by execution rather than by reading.
//
// The mechanism measured, so the assertion below is not a coincidence:
// Service.Plan writes the canonical ControlTarget with InstallationID,
// RepositoryID, IncrementID AND RequirementID (measured at
// internal/application/service.go:962-964); Service.Claim reads it back through
// u.CanonicalTarget and evaluates domain.Permit against it (measured at
// service.go:1289-1311); domain.matches resolves a requirement-scoped intent
// against target.RequirementID (measured at internal/domain/control.go:139-140);
// and permitAllowed(ControlCancel, PermitClaim) is false (measured at
// control.go:276-277).
//
// THE TWO HALVES ARE NOT FUSED, and that is asserted here too: after the
// Control Intent is set, the Requirement's own status is UNCHANGED, because
// Service.Control transitions no Requirement; and PauseRequirement writes no
// control record, because it never touches the control collection.
func TestTheOtherHalfOfAPauseIsAControlIntentAndItAlreadyRefusesTheClaim(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())
	runnerCtx := runner(context.Background(), "runner-1")

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "a17:capture", Text: "a requirement whose increment will be stopped"})
	if err != nil {
		t.Fatal(err)
	}
	// Plan and Prepare happen while the mode is still allow: V2-085 gave both
	// a claim Permit, so a Control Intent set first would deny the fixture
	// rather than the claim, and the test would measure the wrong refusal.
	planned, err := svc.Plan(ctx, application.PlanRequest{RequestID: "a17:plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	prepared, err := svc.Prepare(ctx, application.PrepareRequest{RequestID: "a17:prepare", IncrementID: planned.IncrementID, ExpectedVersion: planned.Version})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// The parent must be in a status that admits work, or V2-089's guard --
	// not the control plane -- would be the thing refusing the claim, and this
	// test would prove nothing about the Control Intent.
	readyVersion := seedRequirementStatus(t, st, captured.RequirementID, domain.RequirementReady)
	beforeIntent, _ := st.Requirement(captured.RequirementID)

	// A control-plane sanity claim under allow would consume the Increment, so
	// the refusal is measured directly instead: set a REQUIREMENT-scoped cancel
	// Intent and attempt the claim.
	if _, err = svc.Control(ctx, application.ControlRequest{
		RequestID: "a17:control",
		Scope:     domain.ControlScope{Kind: domain.ScopeRequirement, Value: captured.RequirementID},
		Mode:      domain.ControlCancel,
		At:        (clock{}).Now(),
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	// HALF ONE IS NOT HALF TWO: the Intent moved no Requirement status.
	afterIntent, _ := st.Requirement(captured.RequirementID)
	if afterIntent.Status != beforeIntent.Status || afterIntent.PausedFrom != beforeIntent.PausedFrom || afterIntent.Version != beforeIntent.Version {
		t.Fatalf("Service.Control changed the Requirement from %+v to %+v; the two halves of a pause must stay separate", beforeIntent, afterIntent)
	}

	_, claimErr := svc.Claim(runnerCtx, application.ClaimRequest{
		RequestID:                "a17:claim",
		IncrementID:              planned.IncrementID,
		ExpectedIncrementVersion: prepared.Version,
		ControlRevision:          0,
		Target:                   domain.ControlTarget{InstallationID: "install", IncrementID: domain.IncrementID(planned.IncrementID), RequirementID: domain.RequirementID(captured.RequirementID), RunnerID: "runner-1"},
	})
	if !errors.Is(claimErr, domain.ErrControlDenied) {
		t.Fatalf("claiming a planned and prepared Increment under a requirement-scoped cancel Intent: %v, want domain.ErrControlDenied", claimErr)
	}

	// HALF TWO IS NOT HALF ONE: pausing writes no control record. The control
	// list is read before and after and must be identical.
	controlsBefore, err := svc.ListControls(ctx, application.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: "a17:pause", RequirementID: captured.RequirementID, ExpectedVersion: readyVersion}); err != nil {
		t.Fatalf("pause under a cancel Intent: %v; pause evaluates no Permit and must be allowed under every mode", err)
	}
	controlsAfter, err := svc.ListControls(ctx, application.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(controlsAfter) != len(controlsBefore) {
		t.Fatalf("Service.PauseRequirement changed the control list from %d to %d entries; it must write no Control Intent", len(controlsBefore), len(controlsAfter))
	}
	paused, _ := st.Requirement(captured.RequirementID)
	if paused.Status != domain.RequirementPaused || paused.PausedFrom != domain.RequirementReady {
		t.Fatalf("the paused Requirement is %+v, want paused remembering ready", paused)
	}
	t.Logf("A17: the propagation half (a requirement-scoped Control Intent) refuses the claim with %v while the status half is independent; RECORDED AND NOT ASSERTED: Service.Plan and Service.Prepare still write the parent without consulting its status, so an Increment can still be planned and prepared under a paused or cancelled Requirement (V2-084's F1, half-closed by V2-089 on the Claim side only)", claimErr)
}

// ---------------------------------------------------------------------------
// C4: exactly one issuer for each of the three domain commands.
// ---------------------------------------------------------------------------

// TestEachRequirementStopCommandHasExactlyOneIssuerInThisPackage is C4 asserted
// rather than promised, using the same go/ast idiom framing_test.go's issuer
// scan uses: exactly ONE non-test line in this package names each of
// domain.RequirementPause, domain.RequirementResume and
// domain.RequirementCancel, and all three live in pause.go. So there is exactly
// one way to reach paused, one way to leave it by resuming, and one way to
// cancel.
func TestEachRequirementStopCommandHasExactlyOneIssuerInThisPackage(t *testing.T) {
	for _, symbol := range []string{"RequirementPause", "RequirementResume", "RequirementCancel"} {
		symbol := symbol
		t.Run(symbol, func(t *testing.T) {
			countIn := func(node ast.Node) int {
				n := 0
				ast.Inspect(node, func(x ast.Node) bool {
					sel, isSel := x.(*ast.SelectorExpr)
					if !isSel {
						return true
					}
					if pkg, isIdent := sel.X.(*ast.Ident); isIdent && pkg.Name == "domain" && sel.Sel.Name == symbol {
						n++
					}
					return true
				})
				return n
			}
			paths := applicationNonTestFiles(t)
			if len(paths) == 0 {
				t.Fatal("scanned zero non-test files; the scan is broken")
			}
			perFile := map[string]int{}
			total := 0
			for _, path := range paths {
				_, file := parseApplicationFile(t, path)
				if n := countIn(file); n != 0 {
					perFile[path] = n
					total += n
				}
			}
			if total != 1 {
				t.Fatalf("this package names domain.%s %d times in non-test source, want exactly 1: %v", symbol, total, perFile)
			}
			if perFile["pause.go"] != 1 {
				t.Fatalf("the single issuer of domain.%s is not pause.go: %v", symbol, perFile)
			}
			t.Logf("C4: %d non-test files scanned; domain.%s is issued from exactly one of them", len(paths), symbol)
		})
	}
}

// applicationNonTestFiles lists this package's non-test Go files. It fails
// outright on an empty result so a mis-written glob cannot make a scan pass
// vacuously.
func applicationNonTestFiles(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("the glob matched no files; the working directory is not internal/application")
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	return out
}
