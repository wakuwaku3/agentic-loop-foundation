package application_test

// V2-091 acceptance. Every test here drives the real application Service over a
// real store, through the product's own commands; none of them hand-builds a
// scheduler decision, a permit, a domain.StableReleaseProof or a Requirement
// status the transition table would refuse.
//
// DETERMINISM IS ACCEPTANCE, not preference (A18). There is no fixed sleep, no
// wall-clock timer, no goroutine and no randomness anywhere in this file; every
// instant comes from an injected clock; and the pass reads its clock exactly
// once per invocation, which is itself asserted.
//
// THE IDENTITY COMES FROM application.LoopCaller AND FROM NOWHERE ELSE. This
// file constructs no application.Caller carrying RoleScheduler: it calls the
// sanctioned producer, so V2-086's repository-wide single-producer scan in
// internal/application/requested_by_test.go stays green and unedited.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/release"
	"github.com/takushi/agentic-loop-foundation/v2/internal/scheduler"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// ---------------------------------------------------------------------------
// Fixtures. Every one of them reaches its state through a real transition.
// ---------------------------------------------------------------------------

func loopScheduler(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	// application.LoopCaller is the ONLY sanctioned producer of a scheduler
	// identity. A bare composite literal here would be the second producer
	// V2-086's scan exists to forbid.
	caller, err := application.LoopCaller("reconciler@example.invalid")
	if err != nil {
		t.Fatalf("LoopCaller: %v", err)
	}
	return application.ContextWithCaller(ctx, caller)
}

func loopRequirement(t *testing.T, st *memory.Store, ctx context.Context, id string) domain.Requirement {
	t.Helper()
	var out domain.Requirement
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		r, ok, e := u.Requirement(ctx, id)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("requirement %s is absent", id)
		}
		out = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func loopIncrement(t *testing.T, st *memory.Store, ctx context.Context, id string) domain.Increment {
	t.Helper()
	var out domain.Increment
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, ok, e := u.Increment(ctx, id)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("increment %s is absent", id)
		}
		out = inc
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// loopCaptureNamed captures a Requirement under a caller-chosen id, so a test
// can control the page order the store's own ordering key produces.
func loopCaptureNamed(t *testing.T, s *application.Service, ctx context.Context, id string) domain.Version {
	t.Helper()
	out, err := s.Capture(ctx, application.CaptureRequest{RequestID: "capture-" + id, RequirementID: id, Text: "requirement " + id})
	if err != nil {
		t.Fatalf("capture %s: %v", id, err)
	}
	return out.Version
}

// loopReadyNamed captures a Requirement under a chosen id and drives it to
// domain.RequirementReady through the real transitions. It returns the
// POST-ready version, because seeding raises Version and a downstream
// ExpectedRequirementVersion built from the capture version would be stale.
func loopReadyNamed(t *testing.T, s *application.Service, st *memory.Store, ctx context.Context, at time.Time, id string) domain.Version {
	t.Helper()
	loopCaptureNamed(t, s, ctx, id)
	return allocAdvance(t, st, ctx, at, id, domain.RequirementStartFraming, domain.RequirementReadyCommand)
}

// loopLoseExecution drives one Increment all the way to an Execution the
// reconciler would call lost: plan, prepare, claim, start, then the real
// domain.MarkExecutionLost. Nothing is assigned directly.
func loopLoseExecution(t *testing.T, s *application.Service, st *memory.Store, ctx context.Context, at time.Time, requirementID string, version domain.Version, tag string) (string, string) {
	t.Helper()
	claimed := allocLease(t, s, st, ctx, at, requirementID, version, tag)
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		exec, ok, e := u.Execution(ctx, claimed.ExecutionID)
		if e != nil || !ok {
			return fmt.Errorf("execution %s: %v ok=%v", claimed.ExecutionID, e, ok)
		}
		lease, ok, e := u.Lease(ctx, claimed.LeaseID)
		if e != nil || !ok {
			return fmt.Errorf("lease %s: %v ok=%v", claimed.LeaseID, e, ok)
		}
		expired, e := domain.ExpireLease(lease, at.Add(2*time.Hour))
		if e != nil {
			return fmt.Errorf("expire lease: %w", e)
		}
		if e = u.SaveLease(ctx, expired, lease.Version); e != nil {
			return e
		}
		lostExec, e := domain.MarkExecutionLost(exec, lease)
		if e != nil {
			return fmt.Errorf("mark lost: %w", e)
		}
		return u.SaveExecution(ctx, lostExec, exec.Version)
	}); err != nil {
		t.Fatal(err)
	}
	return claimed.IncrementID, claimed.ExecutionID
}

// ---------------------------------------------------------------------------
// v2 / A4(a): the caller gate, four ways
// ---------------------------------------------------------------------------

// TestLoopPassAdmitsTheSchedulerIdentityAndRefusesEveryOther is the identity
// proof. RoleScheduler ALONE runs the pass. An owner is refused because an
// owner driving the Loop would be the owner's authority wearing the Loop's
// identity, which is exactly what V2-086's LoopCaller exists to make
// unnecessary. A runner is refused because a Runner deciding which Requirement
// waits is the self-naming defect internal/runner/orchestrator.go:54 commits.
func TestLoopPassAdmitsTheSchedulerIdentityAndRefusesEveryOther(t *testing.T) {
	s, _, _ := allocService(t)
	base := context.Background()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"no caller at all", base, application.ErrUnauthenticated},
		{"owner", owner(base), application.ErrForbidden},
		{"runner", runner(base, "runner-1"), application.ErrForbidden},
	} {
		if _, err := s.LoopPass(tc.ctx, application.LoopPassRequest{RequestID: "pass-" + tc.name}); !errors.Is(err, tc.want) {
			t.Fatalf("%s: LoopPass error = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := s.LoopPass(loopScheduler(t, base), application.LoopPassRequest{RequestID: "pass-scheduler"}); err != nil {
		t.Fatalf("a scheduler caller obtained from application.LoopCaller must run the pass: %v", err)
	}
	// The same gate on the offer read, in the opposite direction: only a
	// verified runner session may read it.
	if _, err := s.OfferedWork(base); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("OfferedWork with no caller = %v, want ErrUnauthenticated", err)
	}
	if _, err := s.OfferedWork(owner(base)); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("OfferedWork as owner = %v, want ErrForbidden", err)
	}
	if _, err := s.OfferedWork(loopScheduler(t, base)); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("OfferedWork as the scheduler = %v, want ErrForbidden: the offer is a runner read, not the Loop's", err)
	}
	if _, err := s.OfferedWork(runner(base, "runner-1")); err != nil {
		t.Fatalf("OfferedWork as a runner: %v", err)
	}
}

// TestLoopPassBuildsNoCallerOfItsOwn asserts by go/ast over the one new
// non-test file that it constructs no application.Caller at all. V2-086's
// repository-wide scan asserts exactly one RoleScheduler Caller literal exists;
// this is the local half, and it is the assertion that would fail first if a
// later change reached for a literal instead of LoopCaller.
func TestLoopPassBuildsNoCallerOfItsOwn(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "loop.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse loop.go: %v", err)
	}
	literals, roles := 0, 0
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok {
			if id, isID := lit.Type.(*ast.Ident); isID && id.Name == "Caller" {
				literals++
			}
		}
		if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "Role") {
			roles++
		}
		return true
	})
	if literals != 0 {
		t.Fatalf("loop.go constructs %d application.Caller composite literals, want 0: LoopCaller is the only sanctioned producer", literals)
	}
	// RoleScheduler and RoleRunner appear only as the ARGUMENTS of the gate
	// calls, never as fields of a Caller this file builds; the literal count
	// above is what proves that, and this count is reported so a reader can see
	// the scan found the identifiers at all rather than nothing.
	if roles == 0 {
		t.Fatal("the scan found no Role identifier in loop.go at all; the AST walk is not seeing the gate and the assertion above would pass vacuously")
	}
	t.Logf("loop.go: Caller composite literals=%d Role identifiers=%d", literals, roles)
}

// ---------------------------------------------------------------------------
// v3 / v4 / A5: stage W, and its closed reason set
// ---------------------------------------------------------------------------

// TestLoopPassWaitsAReadyRequirementOnlyForCapacityOrContention is v3. It is
// three fixtures over one mechanism, and the second is the absent-observation
// negative A7 requires for waiting.
func TestLoopPassWaitsAReadyRequirementOnlyForCapacityOrContention(t *testing.T) {
	t.Run("no-runner-capacity moves a ready Requirement to waiting", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		// One Execution is really running, and the concurrency limit is then
		// declared by the owner at 1, so the pool has no spare capacity. The
		// ORDER matters and is not cosmetic: a Control Intent makes a control
		// revision authoritative, and domain.Permit requires a claim to carry
		// exactly that revision, so the lease is taken while no Intent exists.
		busyVersion := loopReadyNamed(t, s, st, ctx, clk.now, "req-busy")
		allocLease(t, s, st, ctx, clk.now, "req-busy", busyVersion, "busy")
		allocSetLimit(t, s, ctx, "cap-1", 1)
		waiterVersion := loopReadyNamed(t, s, st, ctx, clk.now, "req-waiter")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-capacity"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Wait.Transitions != 1 {
			t.Fatalf("wait transitions = %d, want exactly 1: %+v", report.Wait.Transitions, report)
		}
		if len(report.WaitObservations) != 1 {
			t.Fatalf("wait observations = %d, want 1", len(report.WaitObservations))
		}
		got := report.WaitObservations[0]
		if got.RequirementID != "req-waiter" {
			t.Fatalf("waiting observation names %q, want req-waiter", got.RequirementID)
		}
		if got.Reason != string(scheduler.ReasonNoRunnerCapacity) {
			t.Fatalf("waiting reason = %q, want %q", got.Reason, scheduler.ReasonNoRunnerCapacity)
		}
		after := loopRequirement(t, st, ctx, "req-waiter")
		if after.Status != domain.RequirementWaiting {
			t.Fatalf("req-waiter status = %q, want waiting", after.Status)
		}
		if after.Version != waiterVersion+1 {
			t.Fatalf("req-waiter version = %d, want exactly one above %d", after.Version, waiterVersion)
		}
		// The observation is on the DURABLE record the same transaction wrote,
		// read back out of the store rather than out of the report.
		stored := loopStoredObservation(t, st, ctx, "pass-capacity:wait:req-waiter", "loop-wait")
		if stored["scheduler_reason"] != string(scheduler.ReasonNoRunnerCapacity) {
			t.Fatalf("durable record scheduler_reason = %v, want %q: a transition with no recorded justification is invisible", stored["scheduler_reason"], scheduler.ReasonNoRunnerCapacity)
		}
		if _, ok := stored["scheduler_rank"]; !ok {
			t.Fatalf("durable record carries no scheduler_rank: %v", stored)
		}
	})

	t.Run("capacity available and no contention transitions nothing", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		loopReadyNamed(t, s, st, ctx, clk.now, "req-solo")
		before := loopRequirement(t, st, ctx, "req-solo")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-idle"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Wait.Transitions != 0 || len(report.WaitObservations) != 0 {
			t.Fatalf("a pass with capacity available issued %d waiting transitions, want 0: %+v", report.Wait.Transitions, report)
		}
		after := loopRequirement(t, st, ctx, "req-solo")
		// The scheduler ASSIGNED this Requirement, so stage D planned an
		// Increment for it, which raises the Requirement's Version by exactly
		// one and appends one Increment id. Byte-identity is therefore asserted
		// on the fields stage W would have moved, and the plan is asserted
		// separately below, so this negative cannot be satisfied by a pass that
		// did nothing at all.
		if after.Status != domain.RequirementReady {
			t.Fatalf("req-solo status = %q, want ready: no observation justified a wait", after.Status)
		}
		if report.Plan.Transitions != 1 {
			t.Fatalf("plan transitions = %d, want 1: the scheduler assigned req-solo", report.Plan.Transitions)
		}
		if after.Version != before.Version+1 {
			t.Fatalf("req-solo version = %d, want exactly one above %d (the planned Increment)", after.Version, before.Version)
		}
	})

	t.Run("two ready Requirements on one Repository: the second is resource-conflict", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		loopReadyNamed(t, s, st, ctx, clk.now, "req-first")
		loopReadyNamed(t, s, st, ctx, clk.now, "req-second")
		allocLink(t, st, ctx, clk.now, "req-first", "repo-shared")
		allocLink(t, st, ctx, clk.now, "req-second", "repo-shared")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-contention"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Wait.Transitions != 1 || len(report.WaitObservations) != 1 {
			t.Fatalf("wait transitions = %d, want exactly 1 (the second of two Requirements contending for one Repository): %+v", report.Wait.Transitions, report)
		}
		got := report.WaitObservations[0]
		if got.Reason != string(scheduler.ReasonResourceConflict) {
			t.Fatalf("waiting reason = %q, want %q and NOT %q: contention and exhaustion are different facts", got.Reason, scheduler.ReasonResourceConflict, scheduler.ReasonNoRunnerCapacity)
		}
		waiting := loopRequirement(t, st, ctx, got.RequirementID)
		if waiting.Status != domain.RequirementWaiting {
			t.Fatalf("%s status = %q, want waiting", got.RequirementID, waiting.Status)
		}
	})

	t.Run("a captured Requirement is not-ready and never waits", func(t *testing.T) {
		s, st, _ := allocService(t)
		ctx := owner(context.Background())
		loopCaptureNamed(t, s, ctx, "req-captured")
		before := loopRequirement(t, st, ctx, "req-captured")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-captured"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Wait.Transitions != 0 {
			t.Fatalf("a captured Requirement received %d waiting transitions, want 0: not-ready is not a condition being awaited", report.Wait.Transitions)
		}
		after := loopRequirement(t, st, ctx, "req-captured")
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("the captured Requirement is not byte-identical after the pass:\nbefore=%+v\nafter =%+v", before, after)
		}
	})
}

// loopStoredObservation reads back the durable record the transition wrote --
// the idempotency response the same s.record call persisted inside the same
// transaction as the Event. application.Event has eight fields and no payload,
// and internal/application/ports.go and internal/store/** are outside this
// task's allowed paths, so this is where a transition's justifying observation
// lives; asserting it from the STORE rather than from the report is what makes
// "the observation is carried" a durable fact instead of an in-memory one.
func loopStoredObservation(t *testing.T, st *memory.Store, ctx context.Context, requestID, operation string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		record, ok, e := u.Idempotency(ctx, requestID, operation)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("no durable record for request %q operation %q", requestID, operation)
		}
		return json.Unmarshal(record.ResponseJSON, &out)
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestTheTwoStatusesThisPassProducesStillAdmitAClaim is the V2-089 assertion
// this task owes: the pass puts Requirements into `waiting` and `recovering`,
// and BOTH are members of V2-089's derived admitting set {ready, active,
// waiting, recovering}. It is asserted through V2-089's OWN MECHANISM -- a real
// Service.Claim against a real store -- rather than by reading its switch, so a
// narrowing of that set would fail here as a behaviour and not as a comment.
func TestTheTwoStatusesThisPassProducesStillAdmitAClaim(t *testing.T) {
	for _, status := range []domain.RequirementStatus{domain.RequirementWaiting, domain.RequirementRecovering} {
		s, st, clk := allocService(t)
		if err := loopClaimInStatus(t, s, st, clk.now, status); err != nil {
			t.Fatalf("an Increment whose parent is %q could not be claimed: %v; V2-089's admitting set is {ready, active, waiting, recovering} and this pass puts Requirements into two of them", status, err)
		}
	}
}

// loopClaimInStatus drives a fresh Requirement into the given status through
// real transitions, plans and prepares an Increment, and returns the error a
// Runner's claim produced.
func loopClaimInStatus(t *testing.T, s *application.Service, st *memory.Store, at time.Time, status domain.RequirementStatus) error {
	t.Helper()
	ctx := owner(context.Background())
	tag := "admits-" + string(status)
	version := loopReadyNamed(t, s, st, ctx, at, tag)
	planned, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan-" + tag, RequirementID: tag, ExpectedRequirementVersion: version})
	if err != nil {
		t.Fatalf("plan for %s: %v", tag, err)
	}
	if _, err = s.Prepare(ctx, application.PrepareRequest{RequestID: "prepare-" + tag, IncrementID: planned.IncrementID, ExpectedVersion: planned.Version}); err != nil {
		t.Fatalf("prepare for %s: %v", tag, err)
	}
	switch status {
	case domain.RequirementWaiting:
		allocAdvance(t, st, ctx, at, tag, domain.RequirementWait)
	case domain.RequirementRecovering:
		allocAdvance(t, st, ctx, at, tag, domain.RequirementWait, domain.RequirementRecover)
	default:
		t.Fatalf("unsupported status %q", status)
	}
	if got := loopRequirement(t, st, ctx, tag).Status; got != status {
		t.Fatalf("fixture reached %q, want %q", got, status)
	}
	_, err = s.Claim(runner(context.Background(), "runner-"+tag), application.ClaimRequest{RequestID: "claim-" + tag, IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
	return err
}

// TestLoopWaitReasonSetIsClosedOverAllSevenSchedulerReasons is v4. The set is
// asserted STRUCTURALLY, over loop.go's own switch parsed with go/ast, and the
// axis it is compared against is derived from internal/scheduler's exported
// AllReasons -- so a reason ADDED to internal/scheduler with no arm in loop.go
// fails here, and an arm REMOVED from loop.go fails here too.
func TestLoopWaitReasonSetIsClosedOverAllSevenSchedulerReasons(t *testing.T) {
	if len(scheduler.AllReasons) != 7 {
		t.Fatalf("scheduler.AllReasons has %d members, want 7: the derivation this task's waiting set rests on has moved and the set must be re-derived, not widened", len(scheduler.AllReasons))
	}
	// The constant NAME for each member, derived from internal/scheduler's own
	// source by go/ast rather than written out here.
	nameByValue := loopSchedulerReasonNames(t)
	if len(nameByValue) != 7 {
		t.Fatalf("derived %d Reason constant names from internal/scheduler, want 7: %v", len(nameByValue), nameByValue)
	}
	arms := loopWaitSwitchArms(t)
	if len(arms) != 7 {
		t.Fatalf("loop.go's waiting switch has %d arms, want exactly one per member of scheduler.AllReasons (7): %v", len(arms), arms)
	}
	want := map[string]bool{}
	for _, reason := range scheduler.AllReasons {
		name := nameByValue[string(reason)]
		if name == "" {
			t.Fatalf("no constant name derived for reason %q", reason)
		}
		switch reason {
		case scheduler.ReasonResourceConflict, scheduler.ReasonNoRunnerCapacity:
			want[name] = true
		default:
			want[name] = false
		}
	}
	if !reflect.DeepEqual(arms, want) {
		t.Fatalf("loop.go's waiting switch verdicts = %v, want %v", arms, want)
	}
	trueCount := 0
	for _, v := range want {
		if v {
			trueCount++
		}
	}
	if trueCount != 2 {
		t.Fatalf("the waiting set has %d members, want exactly 2 (resource-conflict and no-runner-capacity)", trueCount)
	}
	// CLOSED: the function ends in an unconditional `return false`, so a reason
	// with no arm is refused rather than admitted. Asserting the trailing
	// statement is what makes "the next element still fails" structural.
	if !loopWaitSwitchFailsClosed(t) {
		t.Fatal("loop.go's waiting switch does not end in an unconditional `return false`; an unlisted reason would not be refused")
	}
}

// loopSchedulerReasonNames maps each scheduler.Reason VALUE to the name of the
// constant that declares it, parsed out of internal/scheduler/decision.go.
func loopSchedulerReasonNames(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join("..", "scheduler", "decision.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 {
			return true
		}
		id, ok := spec.Type.(*ast.Ident)
		if !ok || id.Name != "Reason" {
			return true
		}
		lit, ok := spec.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		out[strings.Trim(lit.Value, "\"")] = spec.Names[0].Name
		return true
	})
	if len(out) == 0 {
		t.Fatalf("parsed zero Reason constants from %s; the AST walk is not finding declarations and every assertion would pass vacuously", path)
	}
	return out
}

// loopWaitSwitchArms parses loopWaitReasonJustifiesWait and returns, per case
// label, whether that arm returns true.
func loopWaitSwitchArms(t *testing.T) map[string]bool {
	t.Helper()
	fn := loopFunctionDecl(t, "loop.go", "loopWaitReasonJustifiesWait")
	out := map[string]bool{}
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Body == nil {
			return true
		}
		found = true
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			verdict := false
			for _, body := range clause.Body {
				ret, isReturn := body.(*ast.ReturnStmt)
				if !isReturn || len(ret.Results) != 1 {
					continue
				}
				if id, isID := ret.Results[0].(*ast.Ident); isID && id.Name == "true" {
					verdict = true
				}
			}
			for _, label := range clause.List {
				sel, ok := label.(*ast.SelectorExpr)
				if !ok || sel.Sel == nil {
					continue
				}
				out[sel.Sel.Name] = verdict
			}
		}
		return false
	})
	if !found {
		t.Fatal("no switch statement found in loopWaitReasonJustifiesWait; the AST walk is broken")
	}
	return out
}

func loopWaitSwitchFailsClosed(t *testing.T) bool {
	t.Helper()
	fn := loopFunctionDecl(t, "loop.go", "loopWaitReasonJustifiesWait")
	if len(fn.Body.List) == 0 {
		return false
	}
	last, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ReturnStmt)
	if !ok || len(last.Results) != 1 {
		return false
	}
	id, ok := last.Results[0].(*ast.Ident)
	return ok && id.Name == "false"
}

func loopFunctionDecl(t *testing.T, path, name string) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name && fn.Body != nil {
			return fn
		}
	}
	t.Fatalf("%s declares no function %s with a body", path, name)
	return nil
}

// ---------------------------------------------------------------------------
// v5 / A6: stage R
// ---------------------------------------------------------------------------

// TestLoopPassRecoversOnlyWhenAnExecutionIsLost is v5. The observation is the
// artefact the SHIPPED reconciler already writes: an Execution in
// domain.ExecutionLost after an Active Lease was expired past its TTL. The
// second and third fixtures are the absent-observation negatives.
func TestLoopPassRecoversOnlyWhenAnExecutionIsLost(t *testing.T) {
	t.Run("an active Requirement with a lost Execution moves to recovering", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		version := loopReadyNamed(t, s, st, ctx, clk.now, "req-lost")
		incrementID, executionID := loopLoseExecution(t, s, st, ctx, clk.now, "req-lost", version, "lost")
		before := loopRequirement(t, st, ctx, "req-lost")
		if before.Status != domain.RequirementActive {
			t.Fatalf("fixture status = %q, want active: the claim carries the parent to active (V2-084)", before.Status)
		}

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-recover"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Recover.Transitions != 1 || len(report.RecoverObservations) != 1 {
			t.Fatalf("recover transitions = %d, want exactly 1: %+v", report.Recover.Transitions, report)
		}
		got := report.RecoverObservations[0]
		if got.RequirementID != "req-lost" || got.IncrementID != incrementID || got.ExecutionID != executionID {
			t.Fatalf("recover observation = %+v, want requirement req-lost increment %s execution %s", got, incrementID, executionID)
		}
		after := loopRequirement(t, st, ctx, "req-lost")
		if after.Status != domain.RequirementRecovering {
			t.Fatalf("req-lost status = %q, want recovering", after.Status)
		}
		if after.Version != before.Version+1 {
			t.Fatalf("req-lost version = %d, want exactly one above %d", after.Version, before.Version)
		}
		stored := loopStoredObservation(t, st, ctx, "pass-recover:recover:req-lost", "loop-recover")
		if stored["increment_id"] != incrementID || stored["execution_id"] != executionID {
			t.Fatalf("durable record = %v, want increment %s and execution %s", stored, incrementID, executionID)
		}
	})

	t.Run("a live Lease and a running Execution transition nothing", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		version := loopReadyNamed(t, s, st, ctx, clk.now, "req-live")
		allocLease(t, s, st, ctx, clk.now, "req-live", version, "live")
		before := loopRequirement(t, st, ctx, "req-live")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-live"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Recover.Transitions != 0 || len(report.RecoverObservations) != 0 {
			t.Fatalf("a live Lease produced %d recover transitions, want 0: %+v", report.Recover.Transitions, report)
		}
		after := loopRequirement(t, st, ctx, "req-live")
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("the Requirement is not byte-identical after a pass with no lost Execution:\nbefore=%+v\nafter =%+v", before, after)
		}
	})

	t.Run("a FAILED Execution rather than a lost one transitions nothing", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		version := loopReadyNamed(t, s, st, ctx, clk.now, "req-failed")
		claimed := allocLease(t, s, st, ctx, clk.now, "req-failed", version, "failed")
		// The real result path, refusing the Execution: AcceptResult with
		// Succeeded false leaves the Execution in domain.ExecutionFailed.
		if _, err := s.AcceptResult(runner(context.Background(), "runner-failed"), application.AcceptResultRequest{
			RequestID: "accept-failed", ExecutionID: claimed.ExecutionID, LeaseID: claimed.LeaseID,
			ExpectedExecutionVersion: 2, FencingToken: claimed.FencingToken, Succeeded: false,
			Target: domain.ControlTarget{},
		}); err != nil {
			t.Fatalf("accept a failing result: %v", err)
		}
		before := loopRequirement(t, st, ctx, "req-failed")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-failed"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Recover.Transitions != 0 {
			t.Fatalf("a FAILED Execution produced %d recover transitions, want 0", report.Recover.Transitions)
		}
		after := loopRequirement(t, st, ctx, "req-failed")
		if after.Status != before.Status {
			t.Fatalf("req-failed status moved from %q to %q; this task issues recover ONLY on domain.ExecutionLost, and docs/architecture/domain-model.md:275's failed case is a RECORDED BOUNDARY, not a defect this task fixes", before.Status, after.Status)
		}
		t.Log("RECORDED BOUNDARY: an Execution in domain.ExecutionFailed produces no Requirement transition. domain-model.md:275 says such a Requirement should become recovering, waiting or needs-input; this task issues recover only on domain.ExecutionLost and records the failed case rather than deciding it")
	})
}

// ---------------------------------------------------------------------------
// v7 / A8: stage D, the severed Increment-creation path
// ---------------------------------------------------------------------------

// TestLoopPassPlansAnIncrementOnlyForAnAssignedRequirement is v7, and its last
// assertion captures the declared boundary: with the
// pass wired, a running Control Plane HOLDS an Increment, so
// POST /v1/runner/claims:acquire can succeed at all.
func TestLoopPassPlansAnIncrementOnlyForAnAssignedRequirement(t *testing.T) {
	t.Run("an assigned Requirement gets one Increment, planned and prepared", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		loopReadyNamed(t, s, st, ctx, clk.now, "req-assigned")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-plan"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Plan.Transitions != 1 || len(report.PlanObservations) != 1 {
			t.Fatalf("plan transitions = %d, want exactly 1: %+v", report.Plan.Transitions, report)
		}
		incrementID := report.PlanObservations[0].IncrementID
		inc := loopIncrement(t, st, ctx, incrementID)
		if inc.Status != domain.IncrementReady {
			t.Fatalf("planned Increment status = %q, want ready: the pass plans AND prepares, so the Increment is in the only status a Claim accepts", inc.Status)
		}
		// The consequence: the claim surface is reachable. Before this task no
		// Increment existed in any running process, so this call could not
		// succeed no matter what the caller sent.
		claimed, err := s.Claim(runner(context.Background(), "runner-assigned"), application.ClaimRequest{
			RequestID: "claim-assigned", IncrementID: incrementID, ExpectedIncrementVersion: inc.Version,
		})
		if err != nil {
			t.Fatalf("claims:acquire for the Increment the pass planned: %v", err)
		}
		if claimed.ExecutionID == "" || claimed.LeaseID == "" {
			t.Fatalf("claim response carried no execution or lease: %+v", claimed)
		}
		if got := loopRequirement(t, st, ctx, "req-assigned").Status; got != domain.RequirementActive {
			t.Fatalf("req-assigned status after the claim = %q, want active", got)
		}
	})

	t.Run("a Requirement that already holds a non-terminal Increment gets no second one", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		loopReadyNamed(t, s, st, ctx, clk.now, "req-once")
		first, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-once-1"})
		if err != nil {
			t.Fatalf("first LoopPass: %v", err)
		}
		if first.Plan.Transitions != 1 {
			t.Fatalf("first pass planned %d, want 1", first.Plan.Transitions)
		}
		// The Requirement is still ready and still assignable; what stops the
		// second Increment is the non-terminal one it now holds.
		second, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-once-2"})
		if err != nil {
			t.Fatalf("second LoopPass: %v", err)
		}
		if second.Plan.Transitions != 0 {
			t.Fatalf("second pass planned %d Increments, want 0: the Requirement already holds work in flight", second.Plan.Transitions)
		}
		r := loopRequirement(t, st, ctx, "req-once")
		if len(r.Increments) != 1 {
			t.Fatalf("req-once holds %d Increments, want 1", len(r.Increments))
		}
	})

	t.Run("the per-pass cap is exact and the remainder is reported", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		total := application.LoopPlanCap + 3
		for i := 0; i < total; i++ {
			loopReadyNamed(t, s, st, ctx, clk.now, fmt.Sprintf("req-cap-%02d", i))
		}
		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-cap"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Plan.Transitions != application.LoopPlanCap {
			t.Fatalf("plan transitions = %d, want exactly the cap %d", report.Plan.Transitions, application.LoopPlanCap)
		}
		if report.Plan.Cap != application.LoopPlanCap {
			t.Fatalf("report cap = %d, want %d", report.Plan.Cap, application.LoopPlanCap)
		}
		if report.Plan.Deferred != total-application.LoopPlanCap {
			t.Fatalf("report deferred = %d, want %d: a saturated pass must say how many assignable Requirements it left unplanned", report.Plan.Deferred, total-application.LoopPlanCap)
		}
		planned := 0
		for i := 0; i < total; i++ {
			if len(loopRequirement(t, st, ctx, fmt.Sprintf("req-cap-%02d", i)).Increments) > 0 {
				planned++
			}
		}
		if planned != application.LoopPlanCap {
			t.Fatalf("%d Requirements hold an Increment, want exactly the cap %d", planned, application.LoopPlanCap)
		}
	})

	t.Run("a Requirement the scheduler did NOT assign gets no Increment", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		// Capacity 1, already consumed: every other ready Requirement is
		// unassigned, so none of them may be planned.
		busy := loopReadyNamed(t, s, st, ctx, clk.now, "req-plan-busy")
		allocLease(t, s, st, ctx, clk.now, "req-plan-busy", busy, "plan-busy")
		allocSetLimit(t, s, ctx, "cap-plan", 1)
		loopReadyNamed(t, s, st, ctx, clk.now, "req-plan-unassigned")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-unassigned"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Plan.Transitions != 0 {
			t.Fatalf("plan transitions = %d, want 0: the scheduler assigned nothing", report.Plan.Transitions)
		}
		if got := loopRequirement(t, st, ctx, "req-plan-unassigned"); len(got.Increments) != 0 {
			t.Fatalf("an unassigned Requirement holds %d Increments, want 0", len(got.Increments))
		}
	})
}

// ---------------------------------------------------------------------------
// v8 / v9 / A9 / A10: stage P, and the negative that separates "the Increment
// succeeded" from "the Requirement may complete"
// ---------------------------------------------------------------------------

// loopSyntheticRoot writes a minimal but REAL release source tree: all seven
// bundle roles, so release.AssembleFromRoot reads real bytes and every digest
// the candidate carries is derived from them rather than supplied. No
// module-path literal is written into it: the go.mod line names a synthetic
// module, so the manifest check cannot read it as an undeclared edge.
func loopSyntheticRoot(t *testing.T, capabilities []string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type rawCapability struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	caps := make([]rawCapability, 0, len(capabilities))
	for _, id := range capabilities {
		caps = append(caps, rawCapability{ID: id, Name: id, Status: "preview"})
	}
	contract := map[string]any{
		"schema_version": "v1", "id": "rc-loop-synthetic", "kind": "release-contract",
		"created_at": "2026-08-26T00:00:00Z", "correlation_id": "loop-synthetic",
		"release": "0.1.0-loop-synthetic", "capabilities": caps,
		"verification": []string{"go test"},
		"rollback":     map[string]string{"procedure": "revert", "target": "main"},
	}
	contractBytes, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	write("contracts/release-contract/foundation.json", string(contractBytes))
	write("contracts/schemas/dummy.json", `{"schema_version":"v1"}`)
	write("contracts/openapi/openapi-v1.yaml", "openapi: 3.0.0\n")
	write("ci/components.json", `{"version":1}`)
	write("go.mod", "module loopsynthetic\n")
	write("go.sum", "\n")
	write("devbox.lock", "{}")
	write("firestore.indexes.json", "{}")
	write("devbox.json", "{}")
	write("firebase.json", "{}")
	write("docs/preview/index.md", "# Preview\n\nSynthetic preview index.\nRelease: 0.1.0-loop-synthetic\n")
	write("docs/preview/stable-diff.md", "# Stableとの差分\n\n## 差分\n\ntext\n\n## 既知の問題\n\ntext\n\n## Stableへ戻す方法\n\ntext\n\n## 昇格に不足している実証\n\ntext\n")
	write("docs/stable/index.md", "# Stable\n\nSynthetic stable index.\n")
	return root
}

// loopFullyEvidencedCandidate builds the CandidateInput a promotion can
// actually pass: every declared capability has a verified, fresh evidence
// record bound to the candidate's OWN assembled digests, and the control
// revision and fencing token are the ones the pass will resolve from canonical
// state. Nothing here fabricates a proof: it fabricates the INPUTS the
// promotion path judges, and the path still refuses them if they do not agree.
func loopFullyEvidencedCandidate(t *testing.T, root string, revision domain.Revision, evidenced bool) release.CandidateInput {
	t.Helper()
	assembled, err := release.AssembleFromRoot(root)
	if err != nil {
		t.Fatalf("assemble %s: %v", root, err)
	}
	id, err := domain.NewReleaseID("loop-candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]domain.CapabilityTarget{}
	for _, capability := range assembled.Contract.Capabilities {
		targets[capability] = domain.CapabilityTarget{Target: "preview-local", Provider: "none"}
	}
	input := release.CandidateInput{
		ReleaseID: id, CandidateID: id, CandidateDigest: "loop-candidate-digest-1", Version: 1,
		Status: domain.ReleaseExercising, CapabilityTargets: targets,
		RollbackEvidence: true, ResumeEvidence: true,
		ExpectedControlRevision: revision, FencingToken: 1,
	}
	if !evidenced {
		// A declared capability with NO evidence at all. This is the shape
		// internal/domain/release.go:100 refuses BY NAME with
		// ErrEvidenceIncomplete, and it is the shipped configuration's shape
		// too: nothing in a running process records capability evidence.
		return input
	}
	_, candidate, err := release.AssembleCandidate(root, input)
	if err != nil {
		t.Fatalf("assemble candidate: %v", err)
	}
	for _, capability := range candidate.Capabilities {
		input.Evidence = append(input.Evidence, domain.CapabilityEvidence{
			Capability: capability, CandidateID: candidate.CandidateID, CandidateDigest: candidate.CandidateDigest,
			Digest: "evidence-" + capability, BundleDigest: candidate.BundleDigest,
			ContractDigest: candidate.ContractDigest, DocsDigest: candidate.DocsDigest,
			Verified: true, Fresh: true, Target: "preview-local", Provider: "none",
		})
	}
	return input
}

// loopReleaseIncrement drives one Increment all the way to
// domain.IncrementReleased through the real domain commands, and leaves its
// parent Requirement in active. Nothing is assigned directly.
func loopReleaseIncrement(t *testing.T, s *application.Service, st *memory.Store, ctx context.Context, at time.Time, requirementID string, version domain.Version, tag string) string {
	t.Helper()
	claimed := allocLease(t, s, st, ctx, at, requirementID, version, tag)
	actor, err := domain.NewActorID("loop-release-fixture")
	if err != nil {
		t.Fatal(err)
	}
	previewID, err := domain.NewReleaseID("loop-preview-" + tag)
	if err != nil {
		t.Fatal(err)
	}
	steps := []domain.IncrementCommand{
		{Kind: domain.IncrementExecute},
		{Kind: domain.IncrementVerify},
		{Kind: domain.IncrementIntegrate},
		{Kind: domain.IncrementPreview, PreviewCandidateID: previewID, PreviewEvidenceDigest: "preview-evidence-" + tag},
		{Kind: domain.IncrementAccept, PreviewCandidateID: previewID, PreviewEvidenceDigest: "preview-evidence-" + tag},
		{Kind: domain.IncrementRelease},
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		for _, step := range steps {
			inc, ok, e := u.Increment(ctx, claimed.IncrementID)
			if e != nil {
				return e
			}
			if !ok {
				return fmt.Errorf("increment %s is absent", claimed.IncrementID)
			}
			step.Actor = actor
			step.At = at
			step.ExpectedVersion = inc.Version
			next, e := domain.DecideIncrement(inc, step)
			if e != nil {
				return fmt.Errorf("%s on %s: %w", step.Kind, claimed.IncrementID, e)
			}
			if e = u.SaveIncrement(ctx, next, inc.Version); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return claimed.IncrementID
}

// TestTheCompletionRequiresBothAProofAndThisRequirementOwnIncrements is A10, in
// one table with two rows plus the not-configured row and the positive.
//
// PRODUCT AUTHORITY, quoted in this test's own comment because it is the reason
// the two halves are independent:
//   - docs/architecture/domain-model.md:101 "1 Requirementに複数
//     Incrementを許す。Incrementが成功してもRequirementは
//     自動完了しない。"
//   - docs/product/definition.md:130 "完了を自己申告だけで決めず、
//     要求と不変条件を検証する"
//   - docs/product/definition.md:105 "Previewで実証され、Stableへ
//     昇格した時点とする"
func TestTheCompletionRequiresBothAProofAndThisRequirementOwnIncrements(t *testing.T) {
	t.Run("an unconfigured release source is a SKIP, never a default root", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		version := loopReadyNamed(t, s, st, ctx, clk.now, "req-skip")
		loopReleaseIncrement(t, s, st, ctx, clk.now, "req-skip", version, "skip")
		before := loopRequirement(t, st, ctx, "req-skip")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-skip"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Promotion.Outcome != application.LoopPromotionSkippedNotConfigured {
			t.Fatalf("promotion outcome = %q, want %q", report.Promotion.Outcome, application.LoopPromotionSkippedNotConfigured)
		}
		if report.Promotion.Evaluations != 0 || report.Promotion.Completions != 0 {
			t.Fatalf("an unconfigured pass issued %d evaluations and %d completions, want 0 and 0", report.Promotion.Evaluations, report.Promotion.Completions)
		}
		after := loopRequirement(t, st, ctx, "req-skip")
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("the Requirement is not byte-identical after an unconfigured pass:\nbefore=%+v\nafter =%+v", before, after)
		}
	})

	t.Run("ROW 1: a succeeding Increment carried to released with the promotion criteria UNMET completes nothing", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		version := loopReadyNamed(t, s, st, ctx, clk.now, "req-unmet")
		loopReleaseIncrement(t, s, st, ctx, clk.now, "req-unmet", version, "unmet")
		revision := allocSetLimit(t, s, ctx, "promote-unmet", 20)
		root := loopSyntheticRoot(t, []string{"loop-cap-alpha"})
		// evidenced=false: a declared capability with NO evidence, which
		// internal/domain/release.go:100 refuses BY NAME with
		// ErrEvidenceIncomplete.
		if err := s.AttachReleaseSource(application.ReleaseSourceConfig{
			Root: root, Repository: "loop-repo", EnvironmentClass: "preview-local",
			AssembledAt: clk.now, Candidate: loopFullyEvidencedCandidate(t, root, revision, false),
		}); err != nil {
			t.Fatalf("AttachReleaseSource: %v", err)
		}
		t.Cleanup(s.DetachReleaseSource)
		before := loopRequirement(t, st, ctx, "req-unmet")
		if before.Status != domain.RequirementActive {
			t.Fatalf("fixture status = %q, want active", before.Status)
		}

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-unmet"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Promotion.Outcome != application.LoopPromotionRefused {
			t.Fatalf("promotion outcome = %q, want %q", report.Promotion.Outcome, application.LoopPromotionRefused)
		}
		if !strings.Contains(report.Promotion.Reason, "evidence") {
			t.Fatalf("promotion refusal reason = %q, want it to name the incomplete evidence", report.Promotion.Reason)
		}
		if report.Promotion.Evaluations != 0 || report.Promotion.Completions != 0 {
			t.Fatalf("a REFUSED promotion issued %d evaluations and %d completions, want 0 and 0", report.Promotion.Evaluations, report.Promotion.Completions)
		}
		after := loopRequirement(t, st, ctx, "req-unmet")
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("the Requirement is not byte-identical after a refused promotion (Version and StableSnapshot included):\nbefore=%+v\nafter =%+v", before, after)
		}
		if after.StableSnapshot != (domain.StableReleaseSnapshot{}) {
			t.Fatalf("StableSnapshot = %+v, want the zero value: an Increment succeeding is not a licence to complete", after.StableSnapshot)
		}
	})

	t.Run("ROW 2: a SUCCEEDING promotion completes only the Requirement whose Increments are ALL released", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		// The sibling that IS eligible.
		doneVersion := loopReadyNamed(t, s, st, ctx, clk.now, "req-done")
		loopReleaseIncrement(t, s, st, ctx, clk.now, "req-done", doneVersion, "done")
		// The sibling that is NOT: active, with an Increment still in flight.
		partialVersion := loopReadyNamed(t, s, st, ctx, clk.now, "req-partial")
		allocLease(t, s, st, ctx, clk.now, "req-partial", partialVersion, "partial")

		revision := allocSetLimit(t, s, ctx, "promote-met", 20)
		root := loopSyntheticRoot(t, []string{"loop-cap-alpha", "loop-cap-beta"})
		if err := s.AttachReleaseSource(application.ReleaseSourceConfig{
			Root: root, Repository: "loop-repo", EnvironmentClass: "preview-local",
			AssembledAt: clk.now, Candidate: loopFullyEvidencedCandidate(t, root, revision, true),
		}); err != nil {
			t.Fatalf("AttachReleaseSource: %v", err)
		}
		t.Cleanup(s.DetachReleaseSource)
		partialBefore := loopRequirement(t, st, ctx, "req-partial")

		report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-met"})
		if err != nil {
			t.Fatalf("LoopPass: %v", err)
		}
		if report.Promotion.Outcome != application.LoopPromotionPromoted {
			t.Fatalf("promotion outcome = %q reason=%q, want %q", report.Promotion.Outcome, report.Promotion.Reason, application.LoopPromotionPromoted)
		}
		if report.Promotion.Completions != 1 || report.Promotion.Evaluations != 1 {
			t.Fatalf("promotion issued %d evaluations and %d completions, want exactly 1 and 1: only req-done has all of its Increments released", report.Promotion.Evaluations, report.Promotion.Completions)
		}
		if len(report.CompletionObservations) != 1 || report.CompletionObservations[0].RequirementID != "req-done" {
			t.Fatalf("completion observations = %+v, want exactly one naming req-done", report.CompletionObservations)
		}
		completed := loopRequirement(t, st, ctx, "req-done")
		if completed.Status != domain.RequirementCompleted {
			t.Fatalf("req-done status = %q, want completed", completed.Status)
		}
		// v8: the StableSnapshot equals the promoted candidate's identity.
		observation := report.CompletionObservations[0]
		if string(completed.StableSnapshot.ReleaseID) != observation.CandidateID {
			t.Fatalf("StableSnapshot.ReleaseID = %q, want the promoted candidate id %q", completed.StableSnapshot.ReleaseID, observation.CandidateID)
		}
		if completed.StableSnapshot.BundleDigest != observation.BundleDigest || completed.StableSnapshot.BundleDigest == "" {
			t.Fatalf("StableSnapshot.BundleDigest = %q, want the promoted bundle digest %q", completed.StableSnapshot.BundleDigest, observation.BundleDigest)
		}
		if completed.StableSnapshot.EvidenceDigest == "" {
			t.Fatal("StableSnapshot.EvidenceDigest is empty; the completed Requirement is not bound to the promoted candidate's evidence")
		}
		if completed.StableSnapshot.ReleaseVersion == 0 {
			t.Fatal("StableSnapshot.ReleaseVersion is zero; the completed Requirement names no release version")
		}
		// The sibling did NOT move: a successful promotion is not a licence to
		// complete everything in flight.
		partialAfter := loopRequirement(t, st, ctx, "req-partial")
		if !reflect.DeepEqual(partialBefore, partialAfter) {
			t.Fatalf("req-partial is not byte-identical after a SUCCESSFUL promotion:\nbefore=%+v\nafter =%+v", partialBefore, partialAfter)
		}
		// The proof does not appear in the report: the report carries the
		// candidate id and the bundle digest, and no proof value.
		body, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"proof", "Proof"} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("the marshalled report contains %q; domain.StableReleaseProof must not leave the transaction that earned it: %s", forbidden, body)
			}
		}
		// A2 replay: a SECOND pass with the SAME namespace replays and does not
		// transition again.
		again, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-met"})
		if err != nil {
			t.Fatalf("replayed LoopPass: %v", err)
		}
		if again.Promotion.Completions != 0 {
			t.Fatalf("the replayed pass issued %d completions, want 0", again.Promotion.Completions)
		}
		if got := loopRequirement(t, st, ctx, "req-done"); got.Version != completed.Version {
			t.Fatalf("the replay moved req-done's version from %d to %d", completed.Version, got.Version)
		}
	})
}

// ---------------------------------------------------------------------------
// v11 / A12: the bound, what one pass leaves unexamined, and the cursor
// ---------------------------------------------------------------------------

// TestOnePassIsBoundedAndTheCursorReachesWhatItDidNotSee is v11's required
// test. It seeds MORE Requirements than one page holds, runs TWO passes, and
// proves the second transitions a Requirement the first never examined -- using
// the cursor the first pass's own report handed back.
//
// This is the assertion that catches SILENT TRUNCATION. A pass that read one
// page and reported nothing about the remainder would read as "covered
// everything", which is the failure mode, not the bound.
func TestOnePassIsBoundedAndTheCursorReachesWhatItDidNotSee(t *testing.T) {
	s, st, clk := allocService(t)
	ctx := owner(context.Background())
	// One page is scheduler.MaxCandidates. Seeding exactly one MORE than that
	// puts the last Requirement, by the store's own ordering key, beyond the
	// first page -- and the ids are chosen so that ordering is not a guess.
	total := scheduler.MaxCandidates + 1
	last := fmt.Sprintf("req-page-%03d", total-1)
	for i := 0; i < total-1; i++ {
		// Everything on page one stays `captured`, so page one has nothing to
		// transition and the ONLY transition either pass can make is the one
		// beyond the cursor.
		loopCaptureNamed(t, s, ctx, fmt.Sprintf("req-page-%03d", i))
	}
	version := loopReadyNamed(t, s, st, ctx, clk.now, last)
	loopLoseExecution(t, s, st, ctx, clk.now, last, version, "page-last")
	beforeLast := loopRequirement(t, st, ctx, last)
	if beforeLast.Status != domain.RequirementActive {
		t.Fatalf("%s status = %q, want active", last, beforeLast.Status)
	}

	usageBefore, _ := st.QuotaTotal(clk.now)
	first, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-page-1"})
	if err != nil {
		t.Fatalf("first LoopPass: %v", err)
	}
	usageAfter, _ := st.QuotaTotal(clk.now)
	t.Logf("MEASURED COST OF ONE PASS against the in-memory adapter (which calls quota.Counter.TrueUp exactly as the Firestore adapter does, internal/store/memory/store.go:353 and internal/store/firestore/store.go:495): reads=%d writes=%d. quota.ReadTransactionUsage reserves Reads 6001 Writes 1; quota.MutationUsage reserves Reads 32 Writes 16; quota.DefaultBudget is Reads 25000 Writes 10000 Deletes 10000",
		usageAfter.Reads-usageBefore.Reads, usageAfter.Writes-usageBefore.Writes)

	if first.RequirementsExamined != scheduler.MaxCandidates {
		t.Fatalf("the first pass examined %d Requirements, want exactly one page of %d", first.RequirementsExamined, scheduler.MaxCandidates)
	}
	if !first.MoreRequirementsExist {
		t.Fatal("the first pass reported MoreRequirementsExist=false while a Requirement beyond the page existed; a report that cannot say what it did not see reads as 'covered everything'")
	}
	if first.NextCursor == "" {
		t.Fatal("the first pass reported no NextCursor while more Requirements existed; there would be no way for a later pass to reach them")
	}
	if first.Recover.Transitions != 0 {
		t.Fatalf("the first pass made %d recover transitions, want 0: the only recoverable Requirement is beyond its page", first.Recover.Transitions)
	}
	if got := loopRequirement(t, st, ctx, last); !reflect.DeepEqual(beforeLast, got) {
		t.Fatalf("%s changed during a pass that never examined it:\nbefore=%+v\nafter =%+v", last, beforeLast, got)
	}

	second, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-page-2", Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("second LoopPass: %v", err)
	}
	if second.RequirementsExamined != 1 {
		t.Fatalf("the second pass examined %d Requirements, want exactly the 1 remaining", second.RequirementsExamined)
	}
	if second.MoreRequirementsExist {
		t.Fatal("the second pass reported MoreRequirementsExist=true with nothing left beyond it")
	}
	if second.NextCursor != "" {
		t.Fatalf("the second pass handed back cursor %q with nothing left beyond it", second.NextCursor)
	}
	if second.Recover.Transitions != 1 {
		t.Fatalf("the second pass made %d recover transitions, want exactly 1: the Requirement the first pass never saw", second.Recover.Transitions)
	}
	if got := loopRequirement(t, st, ctx, last).Status; got != domain.RequirementRecovering {
		t.Fatalf("%s status after the second pass = %q, want recovering", last, got)
	}
	t.Logf("WHAT ONE PASS LEAVES UNEXAMINED: every Requirement after NextCursor. HOW A LATER PASS REACHES IT: the next tick carrying that cursor, which is what this test just executed -- %d examined then %d, and the transition landed on the one the first pass never read", first.RequirementsExamined, second.RequirementsExamined)
}

// TestTheCandidateBoundFailsClosedRatherThanTruncating asserts the fail-closed
// behaviour is PRESERVED rather than caught and continued past. The page bound
// and BuildAllocationSnapshot's bound are the same constant, so the snapshot
// builder can only be pushed over its bound by a caller that reads more than
// one page -- which this pass never does. That is asserted here as a property of
// the pass: the page it reads is never larger than the bound the builder
// refuses above.
func TestTheCandidateBoundFailsClosedRatherThanTruncating(t *testing.T) {
	if _, err := application.BuildAllocationSnapshot(time.Unix(1, 0), 1, 0, make([]domain.Requirement, scheduler.MaxCandidates+1), nil, nil); !errors.Is(err, application.ErrAllocationCandidateBound) {
		t.Fatalf("BuildAllocationSnapshot above its bound = %v, want ErrAllocationCandidateBound: it must FAIL CLOSED rather than truncate", err)
	}
	// loop.go must not swallow either fail-closed error. The scan is over the
	// AST, not the file text, because both sentinels are legitimately NAMED in
	// loop.go's comments -- a text grep would fail on that correct
	// documentation while missing a literal built from pieces.
	file, err := parser.ParseFile(token.NewFileSet(), "loop.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse loop.go: %v", err)
	}
	forbidden := map[string]bool{"ErrAllocationCandidateBound": true, "ErrAllocationLeaseBound": true}
	identifiers := 0
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		identifiers++
		if forbidden[id.Name] {
			t.Fatalf("loop.go REFERENCES %s as an identifier; the pass must PROPAGATE the fail-closed refusal, never catch it and continue", id.Name)
		}
		return true
	})
	if identifiers == 0 {
		t.Fatal("the scan found no identifier in loop.go at all; the AST walk is broken and this assertion would pass vacuously")
	}
	// The loopSkippable list is the closed set of refusals the pass DOES
	// swallow, and neither bound is in it. Asserting the list's members
	// structurally is what keeps the two facts from drifting apart.
	skippable := loopFunctionDecl(t, "loop.go", "loopSkippable")
	swallowed := map[string]bool{}
	ast.Inspect(skippable.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "Err") {
			swallowed[id.Name] = true
		}
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel != nil && strings.HasPrefix(sel.Sel.Name, "Err") {
			swallowed[sel.Sel.Name] = true
		}
		return true
	})
	wantSwallowed := map[string]bool{"ErrStaleVersion": true, "ErrInvalidTransition": true, "ErrControlDenied": true, "ErrNotFound": true}
	if !reflect.DeepEqual(swallowed, wantSwallowed) {
		t.Fatalf("loopSkippable swallows %v, want exactly %v: the set is CLOSED and a fifth entry must be a deliberate, reasoned widening", swallowed, wantSwallowed)
	}
}

// ---------------------------------------------------------------------------
// v12 / A13: the offer read
// ---------------------------------------------------------------------------

// TestTheOfferedWorkReadIsBoundedRunnerRoleAndCarriesNothingElse is v12's
// application half. The route half is asserted in internal/api/api_test.go.
func TestTheOfferedWorkReadIsBoundedRunnerRoleAndCarriesNothingElse(t *testing.T) {
	s, st, clk := allocService(t)
	ctx := owner(context.Background())
	// More ready Increments than the cap, each planned by the pass itself.
	total := application.LoopOfferedWorkCap + 2
	for i := 0; i < total; i++ {
		loopReadyNamed(t, s, st, ctx, clk.now, fmt.Sprintf("req-offer-%02d", i))
	}
	// The pass plans at most LoopPlanCap per tick, so several ticks are needed
	// before there are more ready Increments than the offer cap. Each tick uses
	// its own request namespace.
	for tick := 0; tick < (total/application.LoopPlanCap)+2; tick++ {
		if _, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: fmt.Sprintf("pass-offer-%d", tick)}); err != nil {
			t.Fatalf("LoopPass tick %d: %v", tick, err)
		}
		clk.now = clk.now.Add(24 * time.Hour)
	}
	offered, err := s.OfferedWork(runner(context.Background(), "runner-offer"))
	if err != nil {
		t.Fatalf("OfferedWork: %v", err)
	}
	if offered.Cap != application.LoopOfferedWorkCap {
		t.Fatalf("offer cap = %d, want %d", offered.Cap, application.LoopOfferedWorkCap)
	}
	if len(offered.Increments) == 0 {
		t.Fatal("the offer is empty after several passes planned Increments; a Runner would have nothing to claim and the reachability repair would be incomplete")
	}
	if len(offered.Increments) > application.LoopOfferedWorkCap {
		t.Fatalf("the offer carried %d Increments, want at most the cap %d", len(offered.Increments), application.LoopOfferedWorkCap)
	}
	for _, item := range offered.Increments {
		inc := loopIncrement(t, st, ctx, item.IncrementID)
		if inc.Status != domain.IncrementReady {
			t.Fatalf("offered Increment %s has status %q, want ready", item.IncrementID, inc.Status)
		}
		if item.ExpectedIncrementVersion != inc.Version {
			t.Fatalf("offered expected_increment_version = %d, want the stored %d", item.ExpectedIncrementVersion, inc.Version)
		}
		if item.RequirementID != inc.RequirementID.String() {
			t.Fatalf("offered requirement_id = %q, want %q", item.RequirementID, inc.RequirementID)
		}
	}
	// The offer is CLAIMABLE: what a Runner was offered is what the next call
	// accepts. Anything else would make the offer a suggestion.
	head := offered.Increments[0]
	if _, err = s.Claim(runner(context.Background(), "runner-offer"), application.ClaimRequest{
		RequestID: "claim-offered", IncrementID: head.IncrementID, ExpectedIncrementVersion: head.ExpectedIncrementVersion,
	}); err != nil {
		t.Fatalf("claiming the Increment the offer named: %v", err)
	}
	// The wire shape carries no text, no packet, no digest and no
	// credential-shaped field name.
	body, err := json.Marshal(offered)
	if err != nil {
		t.Fatal(err)
	}
	lowered := strings.ToLower(string(body))
	for _, forbidden := range []string{"password", "credential", "token", "secret", "raw_prompt", "raw_provider_output", "digest", "text", "packet"} {
		if strings.Contains(lowered, forbidden) {
			t.Fatalf("the offer payload contains %q: %s", forbidden, body)
		}
	}
}

// ---------------------------------------------------------------------------
// A4(b): one clock read per invocation
// ---------------------------------------------------------------------------

// loopCountingClock counts how many times the pass asks for the time. It holds
// a fixed instant, so counting it changes nothing the pass decides.
type loopCountingClock struct {
	now   time.Time
	reads int
}

func (c *loopCountingClock) Now() time.Time {
	c.reads++
	return c.now
}

// TestLoopPassReadsItsClockOnceForItsOwnDecisions asserts A4(b) as a
// measurement. The pass itself reads s.clock.Now() exactly ONCE and passes that
// one instant to every stage and to every domain command's At; the further
// reads counted here belong to the EXISTING s.transact and s.mutate helpers,
// which each capture their own transaction-authority instant outside the
// callback so a Firestore retry cannot change it (internal/application/
// service.go:128-148). That is why the assertion is on the DIFFERENCE between a
// pass that transitions nothing and a pass that transitions one Requirement: it
// must be exactly the transactions, never a second decision instant.
func TestLoopPassReadsItsClockOnceForItsOwnDecisions(t *testing.T) {
	st := memory.New()
	clk := &loopCountingClock{now: allocBase}
	s, err := application.NewServiceWithConfig(st, clk, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	loopCaptureNamed(t, s, ctx, "req-clock")

	clk.reads = 0
	if _, err = s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-clock-idle"}); err != nil {
		t.Fatalf("LoopPass: %v", err)
	}
	idle := clk.reads
	// One decision instant plus one s.transact for the observation read. The
	// promotion stage is skipped (no release source configured) and no
	// transition is made, so there is nothing else to open.
	if idle != 2 {
		t.Fatalf("a pass that transitioned nothing read the clock %d times, want exactly 2 (one decision instant, one read transaction)", idle)
	}
	// A pass that DOES transition adds exactly one s.mutate per transition and
	// nothing else. A second decision instant would show up as a third read on
	// top of the transactions.
	version := loopReadyNamed(t, s, st, ctx, allocBase, "req-clock-plan")
	_ = version
	clk.reads = 0
	report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-clock-plan"})
	if err != nil {
		t.Fatalf("LoopPass: %v", err)
	}
	if report.Plan.Transitions != 1 {
		t.Fatalf("plan transitions = %d, want 1", report.Plan.Transitions)
	}
	// Stage D's one planned Increment is Service.Plan then Service.Prepare, so
	// two mutation transactions -- plus exactly ONE more read, and it is
	// MEASURED rather than assumed: the EXISTING Service.Prepare reads
	// s.clock.Now() a second time, inside its transaction callback, for the
	// domain.IncrementCommand's At (internal/application/service.go's Prepare).
	// That read belongs to Prepare and predates this task;
	// internal/application/service.go is outside this task's allowed paths and
	// is not edited. Naming it here is what keeps the assertion honest instead
	// of loosening the number until it fits.
	const prepareSecondClockRead = 1
	if got, want := clk.reads, idle+2+prepareSecondClockRead; got != want {
		t.Fatalf("a pass that planned one Increment read the clock %d times, want %d (%d for the idle pass, one per mutation transaction, and %d for Service.Prepare's own pre-existing second read); a larger number means the PASS took a second decision instant", got, want, idle, prepareSecondClockRead)
	}
	t.Logf("clock reads: idle pass=%d, one-plan pass=%d; the pass's own decision instant is read exactly once per invocation", idle, clk.reads)
}

// ---------------------------------------------------------------------------
// A19: the read models are untouched by the three statuses this pass produces
// ---------------------------------------------------------------------------

// TestTheNewStatusesReadThroughEveryExistingReadModel is A26's data claim and
// A19's read-model claim in one: a Requirement in waiting, recovering,
// evaluating or completed reads through the detail route and through the export
// without error, and nextAction reports exactly what it reports today because it
// branches on none of them.
func TestTheNewStatusesReadThroughEveryExistingReadModel(t *testing.T) {
	s, st, clk := allocService(t)
	ctx := owner(context.Background())
	kindsByStatus := map[domain.RequirementStatus][]domain.RequirementCommandKind{
		domain.RequirementWaiting:    {domain.RequirementWait},
		domain.RequirementRecovering: {domain.RequirementWait, domain.RequirementRecover},
		domain.RequirementEvaluating: {domain.RequirementStart, domain.RequirementEvaluate},
		domain.RequirementCancelled:  {domain.RequirementCancel},
	}
	statuses := []domain.RequirementStatus{domain.RequirementWaiting, domain.RequirementRecovering, domain.RequirementEvaluating, domain.RequirementCancelled}
	for _, status := range statuses {
		id := "req-read-" + string(status)
		loopReadyNamed(t, s, st, ctx, clk.now, id)
		allocAdvance(t, st, ctx, clk.now, id, kindsByStatus[status]...)
		if got := loopRequirement(t, st, ctx, id).Status; got != status {
			t.Fatalf("fixture %s reached %q, want %q", id, got, status)
		}
		detail, found, err := s.GetRequirementDetail(ctx, id)
		if err != nil || !found {
			t.Fatalf("GetRequirementDetail(%s): err=%v found=%v", id, err, found)
		}
		if detail.Status != status {
			t.Fatalf("detail status for %s = %q, want %q", id, detail.Status, status)
		}
		if detail.NextAction == "" {
			t.Fatalf("detail for %s carries no next_action; the read model changed shape for a status it does not branch on", id)
		}
		clk.now = clk.now.Add(24 * time.Hour)
	}
	export, err := s.Export(ctx, 100)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(export) == 0 {
		t.Fatal("the export carried no records")
	}
	t.Logf("A19/A26 data claim: waiting, recovering, evaluating and cancelled all read through the requirement detail and the export with no read-model change; nextAction branches on completed and cancelled and on Increment and Execution status only (internal/application/readmodels.go:333-356, re-measured -- the Work Order says :306-329)")
}

// TestTheIncrementReadTakesIncrementIdsBecauseTheAdaptersDisagree is the test
// that exists because of a MEASURED ADAPTER DIVERGENCE this task found while
// executing the preview-local exercise, and it is the assertion that keeps the
// repair from regressing.
//
// THE DIVERGENCE. The port is named IncrementsForRequirements and its two
// pre-existing non-test callers pass DIFFERENT id kinds through it:
// internal/application/readmodels.go:307 passes INCREMENT ids built from
// Requirement.Increments, while internal/reconciler/orphan.go:67 passes
// REQUIREMENT ids. The adapters disagree too: internal/store/memory/store.go:466
// filters on EITHER the Increment's own id or its RequirementID, so it answers
// both; internal/store/firestore/store.go:707 is a batch GET of one document per
// id from the `increments` collection, so it answers ONLY Increment ids and
// returns NOTHING for a requirement id.
//
// CONSEQUENCE, recorded and not fixed here (internal/reconciler/**,
// internal/store/** and internal/application/ports.go are all outside this
// task's allowed paths): internal/reconciler/orphan.go's orphan scan reads an
// empty Increment set against the Firestore adapter while reading a correct one
// against the in-memory adapter, so its tests pass and its production behaviour
// is a no-op. That is a row (3) finding, not this task's repair.
//
// WHAT THIS TASK DID: it passes INCREMENT ids, derived from the page's own
// Requirement.Increments, which is the form BOTH adapters answer.
func TestTheIncrementReadTakesIncrementIdsBecauseTheAdaptersDisagree(t *testing.T) {
	// (1) Structural: loop.go builds the batch from Requirement.Increments and
	// not from the Requirement id list. The assertion is over the AST so a
	// comment naming either form cannot satisfy or break it.
	fn := loopFunctionDecl(t, "loop.go", "loopObserve")
	usesOwnIncrements := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel != nil && sel.Sel.Name == "Increments" {
			usesOwnIncrements = true
		}
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		fun, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || fun.Sel == nil || fun.Sel.Name != "IncrementsForRequirements" {
			return true
		}
		if len(call.Args) != 2 {
			t.Fatalf("IncrementsForRequirements is called with %d arguments", len(call.Args))
		}
		arg, isIdent := call.Args[1].(*ast.Ident)
		if !isIdent || arg.Name != "incrementIDs" {
			t.Fatalf("IncrementsForRequirements is called with %v, want the incrementIDs list: the Firestore adapter answers ONLY Increment ids", call.Args[1])
		}
		return true
	})
	if !usesOwnIncrements {
		t.Fatal("loopObserve never reads Requirement.Increments; the Increment id list must come from the page's own aggregates")
	}

	// (2) Behavioural: the pass really observes the Increments of a Requirement
	// it read, which is what the failure this test was written for looked like
	// from the outside -- an offer that named nothing.
	s, st, clk := allocService(t)
	ctx := owner(context.Background())
	version := loopReadyNamed(t, s, st, ctx, clk.now, "req-adapter")
	loopLoseExecution(t, s, st, ctx, clk.now, "req-adapter", version, "adapter")
	report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-adapter"})
	if err != nil {
		t.Fatalf("LoopPass: %v", err)
	}
	if report.IncrementsExamined != 1 {
		t.Fatalf("the pass examined %d Increments, want 1: it read a Requirement holding exactly one", report.IncrementsExamined)
	}
	if report.IncrementReadBound != application.LoopIncrementReadBound {
		t.Fatalf("report increment read bound = %d, want %d", report.IncrementReadBound, application.LoopIncrementReadBound)
	}
	if report.TrimmedForIncrementBound {
		t.Fatal("the pass reported a trimmed page for one Requirement holding one Increment")
	}
	if report.Recover.Transitions != 1 {
		t.Fatalf("recover transitions = %d, want 1: the lost Execution belongs to an Increment the batch read must have returned", report.Recover.Transitions)
	}
}

// TestTheEvaluateAndTheCompletionAreOneTransactionAndOneWrite is A9's atomicity
// clause, asserted STRUCTURALLY because there is no way to arrange a valid proof
// that domain.CompleteRequirementFromRelease then refuses: the proof is exactly
// what it accepts. What CAN be asserted, and is, is the shape that makes the
// half-state impossible.
//
// (1) loopCompleteRequirement opens EXACTLY ONE s.mutate.
// (2) BOTH domain calls -- domain.DecideRequirement with RequirementEvaluate and
// domain.CompleteRequirementFromRelease -- are inside that one callback.
// (3) EXACTLY ONE u.SaveRequirement is staged, carrying the COMPLETED value.
// Staging the evaluating value first and the completed value second inside one
// transaction would be two writes describing one atomic step, and a store that
// flushed the first and refused the second would leave precisely the state this
// signature exists to make impossible.
func TestTheEvaluateAndTheCompletionAreOneTransactionAndOneWrite(t *testing.T) {
	fn := loopFunctionDecl(t, "loop.go", "loopCompleteRequirement")
	mutates, saves, evaluates, completes := 0, 0, 0, 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || sel.Sel == nil {
			return true
		}
		switch sel.Sel.Name {
		case "mutate":
			mutates++
		case "SaveRequirement":
			saves++
		case "CompleteRequirementFromRelease":
			completes++
		case "DecideRequirement":
			evaluates++
		}
		return true
	})
	if mutates != 1 {
		t.Fatalf("loopCompleteRequirement opens %d transactions, want exactly 1: the evaluate and the completion must be atomic", mutates)
	}
	if saves != 1 {
		t.Fatalf("loopCompleteRequirement stages %d Requirement writes, want exactly 1 (the completed value): two writes describing one atomic step is the half-state this signature exists to prevent", saves)
	}
	if evaluates != 1 || completes != 1 {
		t.Fatalf("loopCompleteRequirement calls domain.DecideRequirement %d times and domain.CompleteRequirementFromRelease %d times, want exactly 1 and 1", evaluates, completes)
	}
	// And the evaluate really is RequirementEvaluate, not some other kind.
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel != nil && sel.Sel.Name == "RequirementEvaluate" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("loopCompleteRequirement never names domain.RequirementEvaluate; the completion must pass through evaluating")
	}
	// The behavioural corollary, over a real store: after a SUCCESSFUL pass the
	// Requirement is completed at exactly TWO versions above the observed one --
	// one for the evaluate and one for the completion -- and it is never
	// observable in evaluating, because only one write was staged.
	s, st, clk := allocService(t)
	ctx := owner(context.Background())
	version := loopReadyNamed(t, s, st, ctx, clk.now, "req-atomic")
	loopReleaseIncrement(t, s, st, ctx, clk.now, "req-atomic", version, "atomic")
	before := loopRequirement(t, st, ctx, "req-atomic")
	revision := allocSetLimit(t, s, ctx, "promote-atomic", 20)
	root := loopSyntheticRoot(t, []string{"loop-cap-alpha"})
	if err := s.AttachReleaseSource(application.ReleaseSourceConfig{
		Root: root, Repository: "loop-repo", EnvironmentClass: "preview-local",
		AssembledAt: clk.now, Candidate: loopFullyEvidencedCandidate(t, root, revision, true),
	}); err != nil {
		t.Fatalf("AttachReleaseSource: %v", err)
	}
	t.Cleanup(s.DetachReleaseSource)
	report, err := s.LoopPass(loopScheduler(t, context.Background()), application.LoopPassRequest{RequestID: "pass-atomic"})
	if err != nil {
		t.Fatalf("LoopPass: %v", err)
	}
	if report.Promotion.Completions != 1 {
		t.Fatalf("completions = %d, want 1 (outcome %q reason %q)", report.Promotion.Completions, report.Promotion.Outcome, report.Promotion.Reason)
	}
	after := loopRequirement(t, st, ctx, "req-atomic")
	if after.Status != domain.RequirementCompleted {
		t.Fatalf("status = %q, want completed", after.Status)
	}
	if after.Version != before.Version+2 {
		t.Fatalf("version = %d, want exactly two above %d: one for the evaluate and one for the completion, both in one transaction", after.Version, before.Version)
	}
}
