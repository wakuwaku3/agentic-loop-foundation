package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// ===========================================================================
// V2-065: the needs-input question surface.
// ===========================================================================
//
// Every instant in this file is a literal or comes from the injected clock in
// service_test.go. There is no sleep, no timer and no goroutine.
//
// One measurement shapes every fixture below, and it is asserted rather than
// assumed in TestRequirementTransitionsInTheApplicationLayerAreExactlyTheTwoNewCommands:
// before this task, NOTHING in internal/application called
// domain.DecideRequirement, so no application command could move a
// Requirement out of "captured" at all. The fixtures therefore seed the
// starting status through the store -- the same thing
// TestLegacyRequirementWithoutRequestedByStillReads does for a legacy record
// -- because there is no command to do it with, and that absence is the
// finding rather than a shortcut.

// schedulerCaller returns a scheduler-role caller context. It is not named
// scheduler because V2-068 imports the internal/scheduler package into this
// test package, and a top-level identifier of that name would shadow it. loop() in
// requested_by_test.go already builds one; this name says which of the two
// asking roles is being exercised.
func schedulerCaller(ctx context.Context, subject string) context.Context {
	return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleScheduler, Subject: subject})
}

// seedRequirementStatus moves a Requirement to status directly through the
// store, bumping its version exactly as a transition would. It exists because
// no application command can reach framing, active or evaluating (see the
// package comment above), and it is deliberately the only place in this file
// that writes a Requirement outside the Service.
func seedRequirementStatus(t *testing.T, st *memory.Store, id string, status domain.RequirementStatus) domain.Version {
	t.Helper()
	ctx := context.Background()
	var version domain.Version
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		r, ok, err := u.Requirement(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatalf("seed: requirement %q does not exist", id)
		}
		next := r
		next.Status = status
		next.Version++
		if err = domain.Validate(next); err != nil {
			return err
		}
		version = next.Version
		return u.SaveRequirement(ctx, next, r.Version)
	}); err != nil {
		t.Fatalf("seed requirement %q to %q: %v", id, status, err)
	}
	return version
}

// askableQuestion is one well-formed ask. Every test that needs a valid
// question starts from this and changes exactly the field under test.
func askableQuestion() application.RequestHumanInputRequest {
	return application.RequestHumanInputRequest{
		Question:    "Delete the abandoned branch, or keep it and stop touching it?",
		ReasonClass: application.ReasonDestructiveIrreversible,
		Reason:      "Both branches carry unmerged work and neither is reachable from a release, so either choice loses something the Loop is not authorised to lose.",
		Options: []application.HumanInputOption{
			{OptionID: "delete", Summary: "Delete the branch", Impact: "The unmerged commits stop being reachable; recovery needs the reflog on one machine."},
			{OptionID: "keep", Summary: "Keep the branch untouched", Impact: "The Increment stays blocked and the branch keeps accruing drift."},
		},
		StoppedScope:    []application.HumanInputScope{application.ScopeNewClaimsForThisRequirement, application.ScopeLeaseRenewalForThisRequirement},
		ContinuingScope: []application.HumanInputScope{application.ScopeOtherRequirements, application.ScopeIntakeOfNewRequirements, application.ScopeOwnerReads},
	}
}

// humanInputFixture is one Requirement with one prepared Increment, seeded to
// a status the needs-input transition is legal from.
type humanInputFixture struct {
	service       *application.Service
	store         *memory.Store
	requirementID string
	incrementID   string
	// requirementVersion is the version to declare on the next Requirement
	// command; incrementVersion the same for the Increment.
	requirementVersion domain.Version
	incrementVersion   domain.Version
}

func buildHumanInputFixture(t *testing.T, tag string, from domain.RequirementStatus) *humanInputFixture {
	t.Helper()
	svc, st := service()
	ownerCtx := owner(context.Background())
	cap, err := svc.Capture(ownerCtx, application.CaptureRequest{RequestID: tag + ":capture", Text: "a requirement that will need a decision"})
	if err != nil {
		t.Fatalf("fixture %s: capture: %v", tag, err)
	}
	plan, err := svc.Plan(ownerCtx, application.PlanRequest{RequestID: tag + ":plan", RequirementID: cap.RequirementID, ExpectedRequirementVersion: cap.Version})
	if err != nil {
		t.Fatalf("fixture %s: plan: %v", tag, err)
	}
	prepared, err := svc.Prepare(ownerCtx, application.PrepareRequest{RequestID: tag + ":prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatalf("fixture %s: prepare: %v", tag, err)
	}
	f := &humanInputFixture{service: svc, store: st, requirementID: cap.RequirementID, incrementID: plan.IncrementID, incrementVersion: prepared.Version}
	f.requirementVersion = seedRequirementStatus(t, st, cap.RequirementID, from)
	return f
}

// ask records a question through the real command.
func (f *humanInputFixture) ask(t *testing.T, ctx context.Context, requestID string, mutate func(*application.RequestHumanInputRequest)) (application.RequestHumanInputResponse, error) {
	t.Helper()
	req := askableQuestion()
	req.RequestID = requestID
	req.RequirementID = f.requirementID
	req.ExpectedRequirementVersion = f.requirementVersion
	if mutate != nil {
		mutate(&req)
	}
	out, err := f.service.RequestHumanInput(ctx, req)
	if err == nil {
		f.requirementVersion = out.Version
	}
	return out, err
}

func readQuestion(t *testing.T, st *memory.Store, id string) (application.HumanInputRequest, bool) {
	t.Helper()
	ctx := context.Background()
	var out application.HumanInputRequest
	var found bool
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		v, ok, e := u.HumanInputRequest(ctx, id)
		out, found = v, ok
		return e
	}); err != nil {
		t.Fatalf("read question row for %q: %v", id, err)
	}
	return out, found
}

func countRequirements(t *testing.T, st *memory.Store) int {
	t.Helper()
	ctx := context.Background()
	n := 0
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		rows, e := u.Requirements(ctx)
		n = len(rows)
		return e
	}); err != nil {
		t.Fatalf("count requirements: %v", err)
	}
	return n
}

// TestRequirementTransitionsInTheApplicationLayerAreExactlyTheTwoNewCommands
// is A1(i) pinned as an assertion rather than left as a grep in prose: before
// this task the set was EMPTY, and after it, it is exactly the two commands
// this task adds. A third caller has to come with its own justification.
func TestRequirementTransitionsInTheApplicationLayerAreExactlyTheTwoNewCommands(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("the scan found no files; the working directory is not internal/application")
	}
	sort.Strings(matches)
	callers := map[string]bool{}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		_, file := parseApplicationFile(t, name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "DecideRequirement" {
					callers[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	// V2-082 is the third caller this guard's own doc comment asked to arrive
	// with its own justification, and here it is. Service.StartFraming issues
	// domain.RequirementStartFraming, the one transition that leaves the
	// captured status. Before it, the set of Requirement statuses reachable
	// through every application command was the single element {captured}, so
	// the two commands above -- both of which require a source status of
	// framing, active, evaluating, waiting or needs-input -- could never be
	// reached at all through the real surface. Widening the set by exactly one
	// entry is what makes that reachable; the set stays CLOSED, and a fourth
	// caller still fails here. internal/domain was not edited: the transition,
	// its guard and its target status already existed.
	// V2-084 widens the closed set from three to five, and the widening is the
	// MEASUREMENT rather than a weakening. Two entries are added, each with its
	// own reason:
	//   - CompleteFraming issues domain.RequirementReadyCommand from framing.
	//     Before it, RequirementReadyCommand's only issuer was AnswerHumanInput,
	//     so `ready` -- the only status the scheduler calls schedulable -- was
	//     reachable ONLY by asking a Requirement a question and answering it.
	//   - Claim issues domain.RequirementStart when the claimed Increment's
	//     parent Requirement is in ready, because
	//     docs/architecture/domain-model.md:266 DEFINES active as "1つ以上の
	//     Incrementが進行中", so the parent is active precisely because a
	//     subordinate Increment is in progress and the issuer must be the
	//     transaction that creates the progress rather than a caller command.
	// Nothing else in this guard changes, and the set stays CLOSED: a sixth
	// caller still fails here. internal/domain was not edited -- model.go:480
	// -484 and :485-489 already admit both transitions.
	// V2-090 widens the closed set from five to EIGHT, and the widening is again
	// the MEASUREMENT rather than a weakening. Three entries are added, and they
	// are one decision rather than three: docs/product/user-facing-spec.md:201
	// names "Requirementのpause、resume、cancel" as a single owner-issued triple
	// under section 4.8, whose subject sentence at :194 is 利用者.
	//   - PauseRequirement issues domain.RequirementPause, whose two non-test
	//     occurrences before this task were both inside internal/domain/model.go
	//     -- the declaration and the switch case -- so the transition had no
	//     issuer at all.
	//   - CancelRequirement issues domain.RequirementCancel, which had the same
	//     two-occurrence shape and the same absent issuer.
	//   - ResumeRequirement issues domain.RequirementResume, which did not exist
	//     before this task in ANY file, test files included. It is in the same
	//     task as the pause because `paused` was a SOURCE status in exactly ONE
	//     of DecideRequirement's ten branches -- cancel -- so a pause shipped
	//     without an exit would have been a button whose only sequel is
	//     destroying the Requirement.
	// Nothing else in this guard changes, and the set stays CLOSED: a ninth
	// caller still fails here.
	// V2-091 widens the closed set from eight to TEN, under v2-task-dag.md 12.12,
	// and the widening is again the MEASUREMENT rather than a weakening. Two
	// entries are added, each with its own reason, and both live in the ONE new
	// file internal/application/loop.go:
	//   - loopRequirementTransition issues domain.RequirementWait and
	//     domain.RequirementRecover. Measured at 848d899, both command kinds had
	//     ZERO non-test occurrences outside internal/domain/model.go: the domain
	//     declared them, admitted them from three and two source statuses
	//     respectively (model.go:580-583 and :590-591), and nothing in any
	//     running process could issue either, so `waiting` and `recovering` --
	//     two of the eleven declared Requirement statuses -- were unreachable.
	//   - loopCompleteRequirement issues domain.RequirementEvaluate, whose
	//     non-test occurrence count outside model.go was also ZERO, in the SAME
	//     transaction as domain.CompleteRequirementFromRelease, so a failure
	//     between them leaves the Requirement in neither evaluating nor
	//     completed.
	// It is ONE decision rather than two: all three commands are issued by the
	// same single bounded pass, under the same single scheduler identity, and
	// each carries the observation that justified it. internal/domain was not
	// edited -- every transition, guard and target status already existed.
	// Nothing else in this guard changes, and the set stays CLOSED: an ELEVENTH
	// caller still fails here.
	want := map[string]bool{"RequestHumanInput": true, "AnswerHumanInput": true, "StartFraming": true, "CompleteFraming": true, "Claim": true, "PauseRequirement": true, "ResumeRequirement": true, "CancelRequirement": true, "loopRequirementTransition": true, "loopCompleteRequirement": true}
	if !reflect.DeepEqual(callers, want) {
		t.Fatalf("domain.DecideRequirement is called from %v, want exactly %v", keysSorted(callers), keysSorted(want))
	}
}

// TestRequestHumanInputRecordsTheQuestionAndStagesNoOutbox is A3.
func TestRequestHumanInputRecordsTheQuestionAndStagesNoOutbox(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		from domain.RequirementStatus
	}{
		{"a Runner asks from active", runner(context.Background(), "runner-1"), domain.RequirementActive},
		{"the scheduler asks from framing", schedulerCaller(context.Background(), "scheduler.self"), domain.RequirementFraming},
		{"the scheduler asks from evaluating", schedulerCaller(context.Background(), "scheduler.self"), domain.RequirementEvaluating},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildHumanInputFixture(t, "ask-"+string(tc.from), tc.from)
			before := len(f.store.Events())
			out, err := f.ask(t, tc.ctx, "ask-1", nil)
			if err != nil {
				t.Fatalf("ask: %v", err)
			}
			if out.Status != domain.RequirementNeedsInput {
				t.Fatalf("status after the ask = %q, want needs-input", out.Status)
			}
			if out.AskedBy.ActorType != domain.ActorTypeLoop || out.AskedBy.Subject == "" {
				t.Fatalf("asked_by = %+v, want the Loop with a subject", out.AskedBy)
			}
			if !out.AskedAt.Equal(clock{}.Now()) {
				t.Fatalf("asked_at = %v, want the injected transaction authority time %v", out.AskedAt, clock{}.Now())
			}
			stored, ok := f.store.Requirement(f.requirementID)
			if !ok || stored.Status != domain.RequirementNeedsInput || stored.Version != out.Version {
				t.Fatalf("stored requirement = %+v ok=%v", stored, ok)
			}
			row, ok := readQuestion(t, f.store, f.requirementID)
			if !ok {
				t.Fatal("no question row was written")
			}
			if row.Question == "" || row.ReasonClass != application.ReasonDestructiveIrreversible || len(row.Options) != 2 {
				t.Fatalf("stored question = %+v", row)
			}
			if row.Answered() {
				t.Fatal("a fresh question reports itself as answered")
			}
			// A3's last clause: no outbox item at all.
			if n := len(f.store.Outbox()); n != 0 {
				t.Fatalf("the ask staged %d outbox item(s); asking stages no external effect", n)
			}
			if n := len(f.store.Events()) - before; n != 1 {
				t.Fatalf("the ask recorded %d event(s), want exactly 1", n)
			}
			// Idempotent by request_id, through the same path Capture uses.
			replay, err := f.service.RequestHumanInput(tc.ctx, application.RequestHumanInputRequest{
				RequestID: "ask-1", RequirementID: f.requirementID, ExpectedRequirementVersion: out.Version - 1,
				Question: askableQuestion().Question, ReasonClass: askableQuestion().ReasonClass, Reason: askableQuestion().Reason,
				Options: askableQuestion().Options, StoppedScope: askableQuestion().StoppedScope, ContinuingScope: askableQuestion().ContinuingScope,
			})
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if replay != out {
				t.Fatalf("replay changed the response: %#v vs %#v", replay, out)
			}
			if n := len(f.store.Events()) - before; n != 1 {
				t.Fatalf("the replay recorded another event: %d", n)
			}
		})
	}
}

// TestRequestHumanInputRefusesTheOwnerAndTheUnauthenticated is A3's role gate.
// The owner may not ask: the owner answers, and letting the owner ask on the
// Loop's behalf would make the recorded asker unusable as attribution.
func TestRequestHumanInputRefusesTheOwnerAndTheUnauthenticated(t *testing.T) {
	f := buildHumanInputFixture(t, "ask-roles", domain.RequirementActive)
	if _, err := f.ask(t, context.Background(), "ask-unauth", nil); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("unauthenticated ask = %v, want ErrUnauthenticated", err)
	}
	if _, err := f.ask(t, owner(context.Background()), "ask-owner", nil); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("owner ask = %v, want ErrForbidden", err)
	}
	if _, ok := readQuestion(t, f.store, f.requirementID); ok {
		t.Fatal("a refused ask wrote a question row")
	}
	stored, _ := f.store.Requirement(f.requirementID)
	if stored.Status != domain.RequirementActive {
		t.Fatalf("a refused ask moved the Requirement to %q", stored.Status)
	}
}

// TestAnswerHumanInputResumesTheSameRequirementAndCreatesNothing is A4, A5
// and d10: the id is identical, the count is unchanged, the status is ready,
// and the recorded question is still there with the answer added to it.
func TestAnswerHumanInputResumesTheSameRequirementAndCreatesNothing(t *testing.T) {
	f := buildHumanInputFixture(t, "answer", domain.RequirementActive)
	runnerCtx := runner(context.Background(), "runner-1")
	asked, err := f.ask(t, runnerCtx, "answer:ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	question, _ := readQuestion(t, f.store, f.requirementID)
	countBefore := countRequirements(t, f.store)

	out, err := f.service.AnswerHumanInput(owner(context.Background()), application.AnswerHumanInputRequest{
		RequestID: "answer:answer", RequirementID: f.requirementID, ExpectedRequirementVersion: asked.Version, OptionID: "keep",
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if out.RequirementID != f.requirementID {
		t.Fatalf("the answer reported requirement %q, want the same %q", out.RequirementID, f.requirementID)
	}
	if out.Status != domain.RequirementReady {
		t.Fatalf("status after the answer = %q, want ready", out.Status)
	}
	if got := countRequirements(t, f.store); got != countBefore {
		t.Fatalf("the answer changed the Requirement count from %d to %d; an answer creates no Requirement", countBefore, got)
	}
	if out.AnsweredBy.ActorType != domain.ActorTypeOwner || out.AnsweredOptionID != "keep" {
		t.Fatalf("answer attribution = %+v option=%q", out.AnsweredBy, out.AnsweredOptionID)
	}
	answered, ok := readQuestion(t, f.store, f.requirementID)
	if !ok {
		t.Fatal("the answer erased the question row")
	}
	if !answered.SameQuestion(question) {
		t.Fatalf("the answer changed the recorded question:\n before %+v\n after  %+v", question, answered)
	}
	if !answered.Answered() || answered.AnsweredOptionID != "keep" || !answered.AnsweredAt.Equal(clock{}.Now()) {
		t.Fatalf("stored answer = %+v", answered)
	}
	// A5: the SECOND answer is refused by the domain transition table -- ready
	// is not an allowed source for the ready command -- and not by any flag
	// this layer keeps.
	_, err = f.service.AnswerHumanInput(owner(context.Background()), application.AnswerHumanInputRequest{
		RequestID: "answer:again", RequirementID: f.requirementID, ExpectedRequirementVersion: out.Version, OptionID: "delete",
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a second answer = %v, want domain.ErrInvalidTransition from the transition table itself", err)
	}
	again, _ := readQuestion(t, f.store, f.requirementID)
	if again.AnsweredOptionID != "keep" {
		t.Fatalf("the refused second answer changed the recorded answer to %q", again.AnsweredOptionID)
	}
}

// TestEveryHumanInputRejectionPathLeavesEverythingUnchanged is A5 and A8's
// negative half: four rejection paths, each asserted to leave the Requirement
// status, the Requirement version and the stored row untouched.
func TestEveryHumanInputRejectionPathLeavesEverythingUnchanged(t *testing.T) {
	f := buildHumanInputFixture(t, "reject", domain.RequirementActive)
	asked, err := f.ask(t, runner(context.Background(), "runner-1"), "reject:ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	rowBefore, _ := readQuestion(t, f.store, f.requirementID)
	statusBefore, _ := f.store.Requirement(f.requirementID)

	assertUnchanged := func(t *testing.T, label string) {
		t.Helper()
		stored, ok := f.store.Requirement(f.requirementID)
		if !ok || stored.Status != statusBefore.Status || stored.Version != statusBefore.Version {
			t.Fatalf("%s: requirement = %+v, want status %q version %d unchanged", label, stored, statusBefore.Status, statusBefore.Version)
		}
		row, ok := readQuestion(t, f.store, f.requirementID)
		if !ok || !reflect.DeepEqual(row, rowBefore) {
			t.Fatalf("%s: stored question changed:\n before %+v\n after  %+v", label, rowBefore, row)
		}
	}

	t.Run("a non-owner caller", func(t *testing.T) {
		for _, ctx := range []context.Context{
			context.Background(),
			runner(context.Background(), "runner-1"),
			schedulerCaller(context.Background(), "scheduler.self"),
		} {
			_, err := f.service.AnswerHumanInput(ctx, application.AnswerHumanInputRequest{RequestID: "reject:role", RequirementID: f.requirementID, ExpectedRequirementVersion: asked.Version, OptionID: "keep"})
			if !errors.Is(err, application.ErrForbidden) && !errors.Is(err, application.ErrUnauthenticated) {
				t.Fatalf("non-owner answer = %v, want a refusal", err)
			}
		}
		assertUnchanged(t, "non-owner caller")
	})
	t.Run("an unknown option_id", func(t *testing.T) {
		_, err := f.service.AnswerHumanInput(owner(context.Background()), application.AnswerHumanInputRequest{RequestID: "reject:option", RequirementID: f.requirementID, ExpectedRequirementVersion: asked.Version, OptionID: "whatever-the-loop-prefers"})
		if !errors.Is(err, application.ErrUnknownHumanInputOption) {
			t.Fatalf("unknown option = %v, want ErrUnknownHumanInputOption", err)
		}
		assertUnchanged(t, "unknown option_id")
	})
	t.Run("a stale expected version", func(t *testing.T) {
		_, err := f.service.AnswerHumanInput(owner(context.Background()), application.AnswerHumanInputRequest{RequestID: "reject:stale", RequirementID: f.requirementID, ExpectedRequirementVersion: asked.Version + 7, OptionID: "keep"})
		if !errors.Is(err, domain.ErrStaleVersion) {
			t.Fatalf("stale version = %v, want domain.ErrStaleVersion", err)
		}
		assertUnchanged(t, "stale expected version")
	})
	t.Run("an already-answered question", func(t *testing.T) {
		accepted, err := f.service.AnswerHumanInput(owner(context.Background()), application.AnswerHumanInputRequest{RequestID: "reject:first", RequirementID: f.requirementID, ExpectedRequirementVersion: asked.Version, OptionID: "keep"})
		if err != nil {
			t.Fatalf("the first answer must be accepted: %v", err)
		}
		rowBefore, _ = readQuestion(t, f.store, f.requirementID)
		statusBefore, _ = f.store.Requirement(f.requirementID)
		_, err = f.service.AnswerHumanInput(owner(context.Background()), application.AnswerHumanInputRequest{RequestID: "reject:second", RequirementID: f.requirementID, ExpectedRequirementVersion: accepted.Version, OptionID: "delete"})
		if !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("already answered = %v, want domain.ErrInvalidTransition", err)
		}
		assertUnchanged(t, "already-answered question")
	})
}

// TestNothingCanAnswerOnTheOwnersBehalf is A4's last sentence and d9: the
// absence of a default option is a design property, so it is asserted on the
// request types themselves rather than only documented.
func TestNothingCanAnswerOnTheOwnersBehalf(t *testing.T) {
	fields := func(v any) []string {
		typ := reflect.TypeOf(v)
		out := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			out = append(out, typ.Field(i).Name)
		}
		sort.Strings(out)
		return out
	}
	gotAnswer := fields(application.AnswerHumanInputRequest{})
	wantAnswer := []string{"ExpectedRequirementVersion", "OptionID", "RequestID", "RequirementID"}
	if !reflect.DeepEqual(gotAnswer, wantAnswer) {
		t.Fatalf("AnswerHumanInputRequest fields = %v, want exactly %v", gotAnswer, wantAnswer)
	}
	gotAsk := fields(application.RequestHumanInputRequest{})
	wantAsk := []string{"ContinuingScope", "ExpectedRequirementVersion", "Options", "Question", "Reason", "ReasonClass", "RequestID", "RequirementID", "StoppedScope"}
	if !reflect.DeepEqual(gotAsk, wantAsk) {
		t.Fatalf("RequestHumanInputRequest fields = %v, want exactly %v", gotAsk, wantAsk)
	}
	// No field name can carry a default, an expiry or a timeout, and the
	// record type itself carries none either.
	forbidden := []string{"default", "expire", "expiry", "timeout", "deadline", "auto"}
	for _, names := range [][]string{gotAsk, gotAnswer, fields(application.HumanInputRequest{}), fields(application.HumanInputOption{})} {
		for _, name := range names {
			lower := strings.ToLower(name)
			for _, bad := range forbidden {
				if strings.Contains(lower, bad) {
					t.Fatalf("field %q could answer on the owner's behalf", name)
				}
			}
		}
	}
}

// TestHumanInputVocabulariesAreClosed is A7's closure half, in the shape of
// TestRequestedByActorTypeIsAClosedEnum: the proof is a rejection, not a
// comment.
func TestHumanInputVocabulariesAreClosed(t *testing.T) {
	if got := len(application.HumanInputReasonClasses()); got != 3 {
		t.Fatalf("reason_class has %d values, want exactly the 3 derived from the declaration's user_action", got)
	}
	for _, c := range application.HumanInputReasonClasses() {
		f := buildHumanInputFixture(t, "class-"+string(c), domain.RequirementActive)
		if _, err := f.ask(t, runner(context.Background(), "runner-1"), "class:"+string(c), func(r *application.RequestHumanInputRequest) { r.ReasonClass = c }); err != nil {
			t.Fatalf("reason_class %q should be accepted: %v", c, err)
		}
	}
	f := buildHumanInputFixture(t, "class-bad", domain.RequirementActive)
	if _, err := f.ask(t, runner(context.Background(), "runner-1"), "class:bad", func(r *application.RequestHumanInputRequest) {
		r.ReasonClass = application.HumanInputReasonClass("the-loop-felt-unsure")
	}); !errors.Is(err, application.ErrInvalidHumanInputRequest) {
		t.Fatalf("a reason_class outside the closed set = %v, want a refusal", err)
	}
	if _, ok := readQuestion(t, f.store, f.requirementID); ok {
		t.Fatal("a refused ask wrote a row")
	}

	if _, err := f.ask(t, runner(context.Background(), "runner-1"), "scope:bad", func(r *application.RequestHumanInputRequest) {
		r.StoppedScope = append(r.StoppedScope, application.HumanInputScope("everything-probably"))
	}); !errors.Is(err, application.ErrInvalidHumanInputRequest) {
		t.Fatalf("an out-of-vocabulary scope = %v, want a refusal", err)
	}
	if _, err := f.ask(t, runner(context.Background(), "runner-1"), "scope:empty", func(r *application.RequestHumanInputRequest) {
		r.ContinuingScope = nil
	}); !errors.Is(err, application.ErrInvalidHumanInputRequest) {
		t.Fatalf("an empty continuing_scope = %v, want a refusal", err)
	}
	// A7: an option with an empty impact is rejected. This is the shape that
	// satisfies a schema while telling the owner nothing.
	if _, err := f.ask(t, runner(context.Background(), "runner-1"), "impact:empty", func(r *application.RequestHumanInputRequest) {
		r.Options = []application.HumanInputOption{{OptionID: "just-do-it", Summary: "Proceed", Impact: ""}}
	}); !errors.Is(err, application.ErrInvalidHumanInputRequest) {
		t.Fatalf("an option with no stated impact = %v, want a refusal", err)
	}
	// And the enforced stopped-scope entries cannot be omitted from an ask.
	for _, s := range application.EnforcedStoppedHumanInputScopes() {
		kept := []application.HumanInputScope{}
		for _, v := range askableQuestion().StoppedScope {
			if v != s {
				kept = append(kept, v)
			}
		}
		if _, err := f.ask(t, runner(context.Background(), "runner-1"), "omit:"+string(s), func(r *application.RequestHumanInputRequest) { r.StoppedScope = kept }); !errors.Is(err, application.ErrInvalidHumanInputRequest) {
			t.Fatalf("an ask that omits the enforced stopped scope %q = %v, want a refusal", s, err)
		}
	}
	if _, ok := readQuestion(t, f.store, f.requirementID); ok {
		t.Fatal("one of the refused asks wrote a row")
	}
}

// TestTheDisplayedStoppedScopeAndTheClaimRefusalAgree is A7's agreement half
// and d5. It is a biconditional over both directions in ONE test run: the
// vocabulary entry that says new claims stop is reported for a Requirement if
// and only if Service.Claim actually refuses for one of its Increments.
func TestTheDisplayedStoppedScopeAndTheClaimRefusalAgree(t *testing.T) {
	claimRefused := func(f *humanInputFixture, tag string) bool {
		_, err := f.service.Claim(runner(context.Background(), "runner-1"), application.ClaimRequest{
			RequestID:                tag,
			IncrementID:              f.incrementID,
			ExpectedIncrementVersion: f.incrementVersion,
			Target:                   domain.ControlTarget{InstallationID: "install", RequirementID: domain.RequirementID(f.requirementID), IncrementID: domain.IncrementID(f.incrementID), RunnerID: "runner-1"},
		})
		if err == nil {
			return false
		}
		if !errors.Is(err, application.ErrAwaitingHumanInput) {
			t.Fatalf("%s: Claim failed for an unrelated reason: %v", tag, err)
		}
		return true
	}
	reportsTheStop := func(f *humanInputFixture) bool {
		detail, ok, err := f.service.GetRequirementDetail(owner(context.Background()), f.requirementID)
		if err != nil || !ok {
			t.Fatalf("detail read: ok=%v err=%v", ok, err)
		}
		if detail.NeedsInput == nil {
			return false
		}
		return detail.NeedsInput.StopsScope(application.ScopeNewClaimsForThisRequirement)
	}

	waiting := buildHumanInputFixture(t, "iff-waiting", domain.RequirementActive)
	if _, err := waiting.ask(t, runner(context.Background(), "runner-1"), "iff:ask", nil); err != nil {
		t.Fatal(err)
	}
	notWaiting := buildHumanInputFixture(t, "iff-not-waiting", domain.RequirementActive)

	for _, tc := range []struct {
		name string
		f    *humanInputFixture
	}{
		{"a Requirement waiting for human input", waiting},
		{"a Requirement that was never asked", notWaiting},
	} {
		reported := reportsTheStop(tc.f)
		refused := claimRefused(tc.f, "iff-claim:"+tc.name)
		if reported != refused {
			t.Fatalf("%s: the detail reports %q as stopped = %v, but Claim refusing = %v; the displayed scope and the enforcement must agree", tc.name, application.ScopeNewClaimsForThisRequirement, reported, refused)
		}
		t.Logf("%s: reported=%v refused=%v", tc.name, reported, refused)
	}
	// The test would pass vacuously if neither case ever reported or refused,
	// so both truth values must actually occur.
	if !reportsTheStop(waiting) || reportsTheStop(notWaiting) {
		t.Fatal("the two cases did not produce both truth values; the biconditional would be vacuous")
	}
}

// TestClaimAndRenewRefuseWhileTheRequirementWaitsForHumanInput is A6. It also
// records, by assertion, exactly what is NOT guaranteed: a lease held when the
// question was asked is not extended, and it is not revoked either -- it keeps
// its existing ExpiresAt.
func TestClaimAndRenewRefuseWhileTheRequirementWaitsForHumanInput(t *testing.T) {
	f := buildHumanInputFixture(t, "lease", domain.RequirementActive)
	runnerCtx := runner(context.Background(), "runner-1")

	// A lease is held BEFORE the question is asked.
	claimed, err := f.service.Claim(runnerCtx, application.ClaimRequest{
		RequestID:                "lease:claim",
		IncrementID:              f.incrementID,
		ExpectedIncrementVersion: f.incrementVersion,
		Target:                   domain.ControlTarget{InstallationID: "install", RequirementID: domain.RequirementID(f.requirementID), IncrementID: domain.IncrementID(f.incrementID), RunnerID: "runner-1"},
	})
	if err != nil {
		t.Fatalf("the claim before the ask must succeed: %v", err)
	}
	held, ok := f.store.Lease(claimed.LeaseID)
	if !ok {
		t.Fatal("the lease was not stored")
	}
	expiresAtBefore := held.ExpiresAt

	if _, err = f.ask(t, runnerCtx, "lease:ask", nil); err != nil {
		t.Fatalf("ask: %v", err)
	}

	// Renew refuses: a held claim is not extended.
	if _, err = f.service.Renew(runnerCtx, application.RenewRequest{RequestID: "lease:renew", LeaseID: claimed.LeaseID, ExpectedLeaseVersion: held.Version, FencingToken: claimed.FencingToken}); !errors.Is(err, application.ErrAwaitingHumanInput) {
		t.Fatalf("Renew while waiting = %v, want ErrAwaitingHumanInput", err)
	}
	after, _ := f.store.Lease(claimed.LeaseID)
	if !after.ExpiresAt.Equal(expiresAtBefore) {
		t.Fatalf("the refused Renew moved ExpiresAt from %v to %v", expiresAtBefore, after.ExpiresAt)
	}
	// Stated honestly and asserted: the lease is NOT revoked by the ask. It is
	// still active and lapses at its own ExpiresAt, because no domain
	// transition can revoke an active lease early (domain.ExpireLease refuses
	// while at is before ExpiresAt) and internal/domain is not edited here.
	if after.Status != domain.LeaseActive {
		t.Fatalf("lease status after the ask = %q; the ask does not revoke a held lease, so it must still be active", after.Status)
	}
	if _, err = domain.ExpireLease(after, expiresAtBefore.Add(-time.Second)); err == nil {
		t.Fatal("domain.ExpireLease accepted an instant before ExpiresAt; the measurement d6 rests on has changed and must be escalated")
	}

	// A new claim is refused too, for the same Increment and for a second one.
	if _, err = f.service.Claim(runnerCtx, application.ClaimRequest{
		RequestID:                "lease:claim-again",
		IncrementID:              f.incrementID,
		ExpectedIncrementVersion: claimed.Version,
		Target:                   domain.ControlTarget{InstallationID: "install", RequirementID: domain.RequirementID(f.requirementID), IncrementID: domain.IncrementID(f.incrementID), RunnerID: "runner-1"},
	}); !errors.Is(err, application.ErrAwaitingHumanInput) {
		t.Fatalf("Claim while waiting = %v, want ErrAwaitingHumanInput", err)
	}
}

// TestLegacyRequirementWithoutNeedsInputRowStillReads is A8, deliberately
// named after TestLegacyRequirementWithoutRequestedByStillReads so the
// precedent it follows is visible from the name alone.
func TestLegacyRequirementWithoutNeedsInputRowStillReads(t *testing.T) {
	s, st := service()
	ctx := context.Background()
	legacy := domain.Requirement{ID: domain.RequirementID("legacy-needs-input"), Status: domain.RequirementCaptured, Version: 1}
	if err := domain.Validate(legacy); err != nil {
		t.Fatalf("legacy record (no question row) should validate: %v", err)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveRequirement(ctx, legacy, 0); e != nil {
			return e
		}
		return u.SaveRequirementText(ctx, "legacy-needs-input", "pre-existing")
	}); err != nil {
		t.Fatal(err)
	}
	detail, ok, err := s.GetRequirementDetail(owner(ctx), "legacy-needs-input")
	if err != nil || !ok {
		t.Fatalf("legacy requirement should still be readable: ok=%v err=%v", ok, err)
	}
	if detail.NeedsInput != nil {
		t.Fatalf("a Requirement with no row should have no needs_input, got %+v", detail.NeedsInput)
	}
	body, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "needs_input") {
		t.Fatalf("the marshalled detail carries the needs_input key for a Requirement with no row: %s", body)
	}

	// The stronger case: the status IS needs-input and there is still no row.
	// The field stays absent and no question is synthesised from the status.
	inconsistent := domain.Requirement{ID: domain.RequirementID("needs-input-without-a-row"), Status: domain.RequirementNeedsInput, Version: 1}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirement(ctx, inconsistent, 0)
	}); err != nil {
		t.Fatal(err)
	}
	detail, ok, err = s.GetRequirementDetail(owner(ctx), "needs-input-without-a-row")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if detail.Status != domain.RequirementNeedsInput {
		t.Fatalf("status = %q, want needs-input so the case is not vacuous", detail.Status)
	}
	if detail.NeedsInput != nil {
		t.Fatalf("a needs-input Requirement with no row synthesised a question: %+v", detail.NeedsInput)
	}
	body, err = json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "needs_input") {
		t.Fatalf("the marshalled detail synthesised needs_input from the status: %s", body)
	}

	// A recorded question does appear, so the absence above is a statement
	// about the value and not about the marshaller.
	f := buildHumanInputFixture(t, "legacy-present", domain.RequirementActive)
	if _, err = f.ask(t, runner(ctx, "runner-1"), "legacy:ask", nil); err != nil {
		t.Fatal(err)
	}
	present, ok, err := f.service.GetRequirementDetail(owner(ctx), f.requirementID)
	if err != nil || !ok || present.NeedsInput == nil {
		t.Fatalf("a recorded question must be reported: ok=%v err=%v view=%+v", ok, err, present.NeedsInput)
	}
	body, err = json.Marshal(present)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"needs_input", "reason_class", "stopped_scope", "continuing_scope", "impact"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("the marshalled detail is missing %q: %s", want, body)
		}
	}
	// While unanswered, the answer keys are omitted entirely rather than
	// reported as empty or as the zero instant.
	for _, absent := range []string{"answered_at", "answered_option_id", "0001-01-01"} {
		if strings.Contains(string(body), absent) {
			t.Fatalf("an unanswered question reported %q: %s", absent, body)
		}
	}
	// The pointer hands back a COPY: writing through it changes neither the
	// view on a re-read nor the stored record.
	present.NeedsInput.Question = "rewritten through the pointer"
	present.NeedsInput.Options[0].Impact = "rewritten through the pointer"
	present.NeedsInput.StoppedScope[0] = application.HumanInputScope("rewritten")
	again, _, err := f.service.GetRequirementDetail(owner(ctx), f.requirementID)
	if err != nil {
		t.Fatal(err)
	}
	if again.NeedsInput.Question == "rewritten through the pointer" || again.NeedsInput.Options[0].Impact == "rewritten through the pointer" || again.NeedsInput.StoppedScope[0] == "rewritten" {
		t.Fatalf("the read model handed out a pointer into stored state: %+v", again.NeedsInput)
	}
	row, _ := readQuestion(t, f.store, f.requirementID)
	if row.Question == "rewritten through the pointer" {
		t.Fatal("the stored record was mutated through the read model's pointer")
	}
}

// TestHumanInputListViewAndExportAreUnchanged is d11: the question appears on
// the detail only.
func TestHumanInputListViewAndExportAreUnchanged(t *testing.T) {
	f := buildHumanInputFixture(t, "list-view", domain.RequirementActive)
	if _, err := f.ask(t, runner(context.Background(), "runner-1"), "list:ask", nil); err != nil {
		t.Fatal(err)
	}
	page, err := f.service.ListRequirementsPage(owner(context.Background()), "", 25)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"needs_input", "reason_class", "stopped_scope"} {
		if strings.Contains(string(body), absent) {
			t.Fatalf("the list page carries %q: %s", absent, body)
		}
	}
	for _, name := range []string{"NeedsInput", "Question", "ReasonClass"} {
		if _, ok := reflect.TypeOf(application.RequirementView{}).FieldByName(name); ok {
			t.Fatalf("RequirementView gained the field %q", name)
		}
	}
}

// TestHumanInputFieldsAreBounded is A10.
func TestHumanInputFieldsAreBounded(t *testing.T) {
	// The bounds are named constants, and these are their values.
	if application.MaxHumanInputQuestionLength != 500 || application.MaxHumanInputReasonLength != 1000 ||
		application.MaxHumanInputOptions != 8 || application.MaxHumanInputOptionIDLength != 64 ||
		application.MaxHumanInputOptionTextLength != 300 || application.MaxHumanInputScopeEntries != 8 {
		t.Fatal("the declared bounds moved; the evidence records these values")
	}
	f := buildHumanInputFixture(t, "bounds", domain.RequirementActive)
	long := strings.Repeat("x", 2000)
	cases := []struct {
		name   string
		mutate func(*application.RequestHumanInputRequest)
	}{
		{"an over-long question", func(r *application.RequestHumanInputRequest) { r.Question = long }},
		{"an over-long reason", func(r *application.RequestHumanInputRequest) { r.Reason = long }},
		{"an over-long option summary", func(r *application.RequestHumanInputRequest) { r.Options[0].Summary = long }},
		{"an over-long option impact", func(r *application.RequestHumanInputRequest) { r.Options[0].Impact = long }},
		{"an over-long option id", func(r *application.RequestHumanInputRequest) { r.Options[0].OptionID = long }},
		{"too many options", func(r *application.RequestHumanInputRequest) {
			opts := make([]application.HumanInputOption, 0, application.MaxHumanInputOptions+1)
			for i := 0; i <= application.MaxHumanInputOptions; i++ {
				opts = append(opts, application.HumanInputOption{OptionID: strings.Repeat("o", i+1), Summary: "s", Impact: "i"})
			}
			r.Options = opts
		}},
		{"no option at all", func(r *application.RequestHumanInputRequest) { r.Options = nil }},
		{"a repeated scope entry", func(r *application.RequestHumanInputRequest) {
			r.ContinuingScope = []application.HumanInputScope{application.ScopeOwnerReads, application.ScopeOwnerReads}
		}},
		{"a scope reported as both stopped and continuing", func(r *application.RequestHumanInputRequest) {
			r.ContinuingScope = append(r.ContinuingScope, application.ScopeNewClaimsForThisRequirement)
		}},
	}
	for _, tc := range cases {
		mutate := tc.mutate
		t.Run(tc.name, func(t *testing.T) {
			// Each case builds its own copy of the options slice so a mutation
			// in one case cannot leak into the next.
			if _, err := f.ask(t, runner(context.Background(), "runner-1"), "bounds:"+tc.name, func(r *application.RequestHumanInputRequest) {
				r.Options = append([]application.HumanInputOption(nil), r.Options...)
				mutate(r)
			}); !errors.Is(err, application.ErrInvalidHumanInputRequest) {
				t.Fatalf("%s = %v, want ErrInvalidHumanInputRequest", tc.name, err)
			}
		})
	}
	if _, ok := readQuestion(t, f.store, f.requirementID); ok {
		t.Fatal("a refused, over-long ask wrote a row")
	}
	// An option list exactly at the bound is accepted, so the bound is a
	// bound and not an off-by-one refusal.
	opts := make([]application.HumanInputOption, 0, application.MaxHumanInputOptions)
	for i := 0; i < application.MaxHumanInputOptions; i++ {
		opts = append(opts, application.HumanInputOption{OptionID: strings.Repeat("o", i+1), Summary: "s", Impact: "the impact of this option"})
	}
	if _, err := f.ask(t, runner(context.Background(), "runner-1"), "bounds:at-the-bound", func(r *application.RequestHumanInputRequest) { r.Options = opts }); err != nil {
		t.Fatalf("an ask exactly at the option bound = %v, want acceptance", err)
	}
}

// TestTheQuestionRowIsWriteOnceAndTheAnswerNeverErasesIt is A2's store rule,
// driven against the memory adapter through the port. The Firestore adapter
// carries a table with the same meaning in internal/store/firestore.
func TestTheQuestionRowIsWriteOnceAndTheAnswerNeverErasesIt(t *testing.T) {
	st := memory.New()
	ctx := context.Background()
	base := application.HumanInputRequest{
		RequirementID:   "req-1",
		Question:        "which one?",
		ReasonClass:     application.ReasonLimitChange,
		Reason:          "the ceiling would have to move",
		Options:         []application.HumanInputOption{{OptionID: "a", Summary: "raise it", Impact: "cost rises"}, {OptionID: "b", Summary: "stop", Impact: "work stops"}},
		StoppedScope:    []application.HumanInputScope{application.ScopeNewClaimsForThisRequirement, application.ScopeLeaseRenewalForThisRequirement},
		ContinuingScope: []application.HumanInputScope{application.ScopeOwnerReads},
		AskedAt:         time.Unix(1700000000, 0).UTC(),
		AskedBy:         domain.RequestedBy{ActorType: domain.ActorTypeLoop, Subject: "runner-1"},
	}
	save := func(v application.HumanInputRequest) error {
		return st.Transact(ctx, func(u application.UnitOfWork) error { return u.SaveHumanInputRequest(ctx, v) })
	}
	if err := save(base); err != nil {
		t.Fatal(err)
	}
	if err := save(base); err != nil {
		t.Fatalf("an identical re-write must be an idempotent replay: %v", err)
	}
	changed := base.Clone()
	changed.Question = "a different question"
	if err := save(changed); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("rewriting the question = %v, want domain.ErrStaleVersion", err)
	}
	answered := base.Clone()
	answeredAt := time.Unix(1700000060, 0).UTC()
	answeredBy := domain.RequestedBy{ActorType: domain.ActorTypeOwner, Subject: "owner-1"}
	answered.AnsweredAt = &answeredAt
	answered.AnsweredOptionID = "a"
	answered.AnsweredBy = &answeredBy
	if err := save(answered); err != nil {
		t.Fatalf("the answer must be writable by a second transaction: %v", err)
	}
	stored, ok, err := func() (application.HumanInputRequest, bool, error) {
		var v application.HumanInputRequest
		var found bool
		e := st.Transact(ctx, func(u application.UnitOfWork) error {
			x, o, err := u.HumanInputRequest(ctx, "req-1")
			v, found = x, o
			return err
		})
		return v, found, e
	}()
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if !stored.SameQuestion(base) {
		t.Fatalf("the answer erased part of the question: %+v", stored)
	}
	if !stored.Answered() {
		t.Fatal("the answer was not stored")
	}
	if err = save(base); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("clearing a recorded answer = %v, want domain.ErrStaleVersion", err)
	}
	// A rolled-back transaction leaks nothing, including through the record's
	// slices, which the adapter deep-copies for exactly this reason.
	sentinel := errors.New("rollback")
	rolled := base.Clone()
	rolled.Question = "rolled back"
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		_ = u.SaveHumanInputRequest(ctx, rolled)
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("rollback = %v", err)
	}
	after, _, _ := func() (application.HumanInputRequest, bool, error) {
		var v application.HumanInputRequest
		var found bool
		e := st.Transact(ctx, func(u application.UnitOfWork) error {
			x, o, err := u.HumanInputRequest(ctx, "req-1")
			v, found = x, o
			return err
		})
		return v, found, e
	}()
	if after.Question != base.Question {
		t.Fatalf("a rolled-back transaction changed the committed question to %q", after.Question)
	}
}

// ===========================================================================
// A7 / d8: the seven-mode matrix, measured rather than assumed.
// ===========================================================================

// humanInputModeOutcome is one measured cell.
type humanInputModeOutcome struct {
	mode      domain.ControlMode
	askOK     bool
	answerOK  bool
	captureOK bool
	askErr    string
	notes     string
}

// TestHumanInputSevenModeMatrix measures both commands under all seven
// control modes through the real Service. The ask evaluates no Permit, so it
// is expected to be allowed under every mode; the answer evaluates
// PermitIntake, so it is expected to behave exactly as Capture's intake does.
// Both expectations are asserted against the MEASURED intake behaviour of
// Capture in the same run rather than against a copied table.
func TestHumanInputSevenModeMatrix(t *testing.T) {
	modes := []domain.ControlMode{
		domain.ControlAllow,
		domain.ControlPauseIntake,
		domain.ControlPauseClaim,
		domain.ControlGracefulStop,
		domain.ControlImmediateStop,
		domain.ControlEmergencyStop,
		domain.ControlCancel,
	}
	outcomes := make([]humanInputModeOutcome, 0, len(modes))
	askAllowed, answerAllowed, captureAllowed := 0, 0, 0
	for _, mode := range modes {
		tag := "matrix-" + string(mode)
		// The fixture is built BEFORE any Control Intent exists, exactly as
		// stop_matrix_test.go's fixture is, because intake itself is denied
		// under most of these modes.
		f := buildHumanInputFixture(t, tag, domain.RequirementActive)
		ownerCtx := owner(context.Background())
		if mode != domain.ControlAllow {
			if _, err := f.service.Control(ownerCtx, application.ControlRequest{RequestID: tag + ":control", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: mode, At: clock{}.Now()}); err != nil {
				t.Fatalf("%s: control: %v", mode, err)
			}
		}
		cell := humanInputModeOutcome{mode: mode}
		// The intake reference: does Capture work under this mode right now?
		if _, err := f.service.Capture(ownerCtx, application.CaptureRequest{RequestID: tag + ":intake-reference", Text: "reference intake"}); err == nil {
			captureAllowed++
			cell.captureOK = true
			cell.notes = "capture-allowed"
		} else {
			cell.notes = "capture-denied: " + err.Error()
		}
		asked, err := f.ask(t, runner(context.Background(), "runner-1"), tag+":ask", nil)
		if err == nil {
			cell.askOK = true
			askAllowed++
		} else {
			cell.askErr = err.Error()
		}
		if cell.askOK {
			if _, err = f.service.AnswerHumanInput(ownerCtx, application.AnswerHumanInputRequest{RequestID: tag + ":answer", RequirementID: f.requirementID, ExpectedRequirementVersion: asked.Version, OptionID: "keep"}); err == nil {
				cell.answerOK = true
				answerAllowed++
			} else {
				cell.notes += " | answer-denied: " + err.Error()
			}
		}
		outcomes = append(outcomes, cell)
		t.Logf("mode=%-15s ask=%v answer=%v capture=%v %s", mode, cell.askOK, cell.answerOK, cell.captureOK, cell.notes)
	}
	if len(outcomes) != 7 {
		t.Fatalf("measured %d modes, want 7", len(outcomes))
	}
	// The ask is permit-free, so no mode may deny it: a stop must never take
	// away the system's ability to record why it stopped.
	if askAllowed != 7 {
		for _, c := range outcomes {
			if !c.askOK {
				t.Errorf("mode %q denied the ask: %s", c.mode, c.askErr)
			}
		}
		t.Fatalf("the ask was allowed under %d of 7 modes, want 7", askAllowed)
	}
	// The answer must agree with the MEASURED intake behaviour, mode for mode,
	// because it evaluates the same PermitIntake against the same target. The
	// reference is Capture's own outcome in this same run rather than a table
	// copied from any prior evidence: measured here, pause-claim allows intake
	// (it stops claims, not intake), so the answer is allowed under allow AND
	// under pause-claim, and denied under the other five. Resuming a
	// Requirement to ready issues no claim, so that is the correct behaviour
	// rather than a leak.
	for _, c := range outcomes {
		if c.answerOK != c.captureOK {
			t.Fatalf("mode %q: answer allowed = %v but capture/intake allowed = %v; the answer evaluates PermitIntake exactly as capture does, so the two must agree", c.mode, c.answerOK, c.captureOK)
		}
	}
	if answerAllowed != captureAllowed {
		t.Fatalf("answers allowed = %d, reference intakes allowed = %d", answerAllowed, captureAllowed)
	}
	// Non-vacuity: both truth values must occur, or the agreement above would
	// hold trivially.
	if answerAllowed == 0 || answerAllowed == len(modes) {
		t.Fatalf("the answer was allowed under %d of %d modes; the agreement assertion would be vacuous", answerAllowed, len(modes))
	}
	t.Logf("measured 7-mode matrix: ask allowed %d/7, answer allowed %d/7, reference intake allowed %d/7", askAllowed, answerAllowed, captureAllowed)
}

// ===========================================================================
// A15: the quota reservation.
// ===========================================================================

// countingUnit counts the reads and writes one command actually performs.
type countingUnit struct {
	application.UnitOfWork
	authority context.Context
	reads     *int
	writes    *int
}

func (u countingUnit) AuthorityContext() context.Context { return u.authority }

func (u countingUnit) Requirement(ctx context.Context, id string) (domain.Requirement, bool, error) {
	*u.reads++
	return u.UnitOfWork.Requirement(ctx, id)
}
func (u countingUnit) HumanInputRequest(ctx context.Context, id string) (application.HumanInputRequest, bool, error) {
	*u.reads++
	return u.UnitOfWork.HumanInputRequest(ctx, id)
}
func (u countingUnit) Idempotency(ctx context.Context, requestID, operation string) (application.IdempotentResponse, bool, error) {
	*u.reads++
	return u.UnitOfWork.Idempotency(ctx, requestID, operation)
}
func (u countingUnit) Controls(ctx context.Context) ([]domain.ControlIntent, error) {
	*u.reads++
	return u.UnitOfWork.Controls(ctx)
}
func (u countingUnit) RequirementRepositoryLink(ctx context.Context, id string) (domain.RequirementRepositoryLink, bool, error) {
	*u.reads++
	return u.UnitOfWork.RequirementRepositoryLink(ctx, id)
}
func (u countingUnit) SaveRequirement(ctx context.Context, value domain.Requirement, expected domain.Version) error {
	*u.writes++
	return u.UnitOfWork.SaveRequirement(ctx, value, expected)
}
func (u countingUnit) SaveHumanInputRequest(ctx context.Context, value application.HumanInputRequest) error {
	*u.writes++
	return u.UnitOfWork.SaveHumanInputRequest(ctx, value)
}
func (u countingUnit) Record(event application.Event, outbox *application.OutboxItem) error {
	// One event document, plus the idempotency record and the quota document
	// the adapter writes for every mutation. Counted as three writes so the
	// comparison against the reservation is conservative in the same
	// direction the reservation itself is.
	*u.writes += 3
	return u.UnitOfWork.Record(event, outbox)
}

type countingTransactor struct {
	inner  application.Transactor
	reads  int
	writes int
}

func (c *countingTransactor) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	return c.inner.Transact(ctx, func(u application.UnitOfWork) error {
		return fn(countingUnit{UnitOfWork: u, authority: ctx, reads: &c.reads, writes: &c.writes})
	})
}

// TestBothHumanInputCommandsStayInsideTheMutationReservation is A15.
func TestBothHumanInputCommandsStayInsideTheMutationReservation(t *testing.T) {
	st := memory.New()
	counter := &countingTransactor{inner: st}
	svc, err := application.NewServiceWithConfig(counter, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := owner(context.Background())
	cap, err := svc.Capture(ownerCtx, application.CaptureRequest{RequestID: "quota:capture", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	version := seedRequirementStatus(t, st, cap.RequirementID, domain.RequirementActive)

	measure := func(name string, run func() error) {
		counter.reads, counter.writes = 0, 0
		if err := run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%s: reads=%d writes=%d; reservation reads=%d writes=%d", name, counter.reads, counter.writes, quota.MutationUsage.Reads, quota.MutationUsage.Writes)
		if int64(counter.reads) > quota.MutationUsage.Reads || int64(counter.writes) > quota.MutationUsage.Writes {
			t.Fatalf("%s performed reads=%d writes=%d, outside the conservative mutation reservation %+v", name, counter.reads, counter.writes, quota.MutationUsage)
		}
	}
	ask := askableQuestion()
	ask.RequestID = "quota:ask"
	ask.RequirementID = cap.RequirementID
	ask.ExpectedRequirementVersion = version
	var asked application.RequestHumanInputResponse
	measure("the ask", func() error {
		var e error
		asked, e = svc.RequestHumanInput(runner(context.Background(), "runner-1"), ask)
		return e
	})
	measure("the answer", func() error {
		_, e := svc.AnswerHumanInput(ownerCtx, application.AnswerHumanInputRequest{RequestID: "quota:answer", RequirementID: cap.RequirementID, ExpectedRequirementVersion: asked.Version, OptionID: "keep"})
		return e
	})
}
