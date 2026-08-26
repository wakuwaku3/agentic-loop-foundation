package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// reconcilerSeedRequirementStatus is internal/reconciler's ONE seeding helper,
// added by V2-089 because this package had none. It moves a Requirement to
// status directly through the store, bumping its Version exactly as a
// transition would, and it validates before saving so a fixture cannot reach a
// record the domain rejects. It exists because V2-089 refuses a claim whose
// parent Requirement is not in one of the four statuses that admit work --
// ready, active, waiting, recovering -- and this package's journeys capture,
// plan, prepare and claim without framing.
//
// The status is passed by every call site as a domain constant literal, never a
// variable and never a string, so the state a fixture establishes is readable
// at the fixture. The returned Version is the POST-seed version and the Plan
// that follows must carry it: dropping the ExpectedRequirementVersion, passing
// zero, or seeding after the Plan would each delete a real assertion.
//
// A store write rather than the product's own path is deliberate here and is
// recorded: these journeys assert reconciliation over fencing and stop, and
// threading Service.StartFraming plus Service.CompleteFraming through them
// would add two owner commands and two Control permits to a fixture whose
// subject is neither. internal/runner/orchestrator.go carries this task's
// reachability proof through the product's own commands instead.
func reconcilerSeedRequirementStatus(t *testing.T, st *memory.Store, ctx context.Context, id string, status domain.RequirementStatus) domain.Version {
	t.Helper()
	var version domain.Version
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		r, ok, e := u.Requirement(ctx, id)
		if e != nil {
			return e
		}
		if !ok {
			t.Fatalf("seed: requirement %q does not exist", id)
		}
		next := r
		next.Status = status
		next.Version++
		if e = domain.Validate(next); e != nil {
			return e
		}
		version = next.Version
		return u.SaveRequirement(ctx, next, r.Version)
	}); err != nil {
		t.Fatalf("seed requirement %q to %q: %v", id, status, err)
	}
	return version
}

// fencingJourneyClock is a small injected clock private to this test
// function's fixture, advanced explicitly between phases. It is not
// package-level mutable state: a fresh instance is created inside the test
// function below.
type fencingJourneyClock struct{ now time.Time }

func (c *fencingJourneyClock) Now() time.Time { return c.now }

// fencingJourneyIDs is a small deterministic id generator private to this
// test function.
type fencingJourneyIDs struct{ n int }

func (g *fencingJourneyIDs) Next(kind string) (string, error) {
	g.n++
	return kind + "-" + string(rune('a'+g.n)), nil
}

// TestJourneyFourPartitionRunnerStaleSubmissionsAreRejected is Journey 4's
// partition segment (acceptance A7). It carries the partition-path claim:
// a Runner that stops renewing (network partition, not a crash) is fenced
// by the reconciler, a different Runner claims with a strictly greater
// fencing token, and the partitioned Runner's later Renew/Checkpoint/
// AcceptResult -- submitted under its old lease id, version, control
// revision and fencing token -- are each rejected with a specific domain
// error, leaving the new claim's canonical Execution state byte-identical
// before and after every rejected call.
//
// This is distinct from V2-016's TestJourneyFourLocalCrashResumeAcrossExecutions
// (internal/runner/crash_test.go, not modified or duplicated here): that
// test carries the crash-resume claim -- a Runner that crashes and never
// gets to submit anything again, resumed by a second Claim, with the
// crashed Execution's late AcceptResult rejected. This test carries the
// partition claim -- a Runner that is still alive and does try to keep
// submitting under its old view of the world (Renew, Checkpoint, and
// AcceptResult, not just AcceptResult), and it asserts the specific error
// each of those three calls gets, plus that the new claim's state is
// provably untouched by every one of them.
func TestJourneyFourPartitionRunnerStaleSubmissionsAreRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	clock := &fencingJourneyClock{now: time.Unix(1_700_200_000, 0).UTC()}
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock, &fencingJourneyIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "owner"})
	runner1Ctx := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})
	runner2Ctx := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: "runner-2", RunnerID: "runner-2"})

	cap, err := svc.Capture(ownerCtx, application.CaptureRequest{RequestID: "fj:capture"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: this journey claims, so its parent Requirement is moved to
	// domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- before the Plan, and the Plan
	// carries the POST-seed version.
	readyVersion := reconcilerSeedRequirementStatus(t, st, ownerCtx, cap.RequirementID, domain.RequirementReady)
	plan, err := svc.Plan(ownerCtx, application.PlanRequest{RequestID: "fj:plan", RequirementID: cap.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := svc.Prepare(ownerCtx, application.PrepareRequest{RequestID: "fj:prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatal(err)
	}

	// Runner 1 claims and holds an active Lease and a running Execution.
	claim1, err := svc.Claim(runner1Ctx, application.ClaimRequest{RequestID: "fj:claim1", IncrementID: plan.IncrementID, ExpectedIncrementVersion: prepared.Version})
	if err != nil {
		t.Fatal(err)
	}
	start1, err := svc.Start(runner1Ctx, application.StartRequest{RequestID: "fj:start1", ExecutionID: claim1.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatal(err)
	}

	// Network partition: the clock advances past the Lease TTL without a
	// renewal ever reaching the Control Plane.
	clock.now = clock.now.Add(2 * time.Minute)

	rec := &Reconciler{Tx: st, Clock: clock}
	report, _, err := rec.Tick(ctx, "")
	if err != nil {
		t.Fatalf("reconciler tick: %v", err)
	}
	if report.Recovered != 1 {
		t.Fatalf("report=%#v, want Recovered:1", report)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		lease, _, e := u.Lease(ctx, claim1.LeaseID)
		if e != nil {
			return e
		}
		if lease.Status != domain.LeaseExpired {
			t.Fatalf("lease1=%#v, want LeaseExpired", lease)
		}
		if lease.FencingToken != claim1.FencingToken {
			t.Fatalf("lease1 fencing token changed by fencing itself: got %d want %d", lease.FencingToken, claim1.FencingToken)
		}
		exec, _, e := u.Execution(ctx, claim1.ExecutionID)
		if e != nil {
			return e
		}
		if exec.Status != domain.ExecutionLost {
			t.Fatalf("exec1=%#v, want ExecutionLost", exec)
		}
		inc, _, e := u.Increment(ctx, plan.IncrementID)
		if e != nil {
			return e
		}
		if inc.Status != domain.IncrementReady {
			t.Fatalf("increment=%#v, want IncrementReady", inc)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A different Runner claims the now-ready Increment and receives a
	// strictly greater fencing token.
	incAfterFence, _, err := (func() (domain.Increment, bool, error) {
		var inc domain.Increment
		var ok bool
		err := st.Transact(ctx, func(u application.UnitOfWork) error {
			var e error
			inc, ok, e = u.Increment(ctx, plan.IncrementID)
			return e
		})
		return inc, ok, err
	})()
	if err != nil {
		t.Fatal(err)
	}
	claim2, err := svc.Claim(runner2Ctx, application.ClaimRequest{RequestID: "fj:claim2", IncrementID: plan.IncrementID, ExpectedIncrementVersion: incAfterFence.Version})
	if err != nil {
		t.Fatal(err)
	}
	if !(claim2.FencingToken > claim1.FencingToken) {
		t.Fatalf("fencing token did not strictly increase: claim1=%d claim2=%d", claim1.FencingToken, claim2.FencingToken)
	}
	if claim2.ExecutionID == claim1.ExecutionID {
		t.Fatal("resumed claim reused the partitioned Runner's Execution id")
	}
	if _, err := svc.Start(runner2Ctx, application.StartRequest{RequestID: "fj:start2", ExecutionID: claim2.ExecutionID, ExpectedExecutionVersion: 1}); err != nil {
		t.Fatal(err)
	}

	snapshotExec2 := func() domain.Execution {
		var exec domain.Execution
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			var e error
			exec, _, e = u.Execution(ctx, claim2.ExecutionID)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return exec
	}

	// The partitioned Runner 1, still alive, keeps submitting under its old
	// view of the world: the pre-fencing lease id, lease version, fencing
	// token and (implicitly, via omission) control revision. Each call must
	// be rejected with a specific domain error, and none of them may touch
	// claim2's canonical Execution state.
	before := snapshotExec2()
	_, err = svc.Renew(runner1Ctx, application.RenewRequest{RequestID: "fj:stale-renew", LeaseID: claim1.LeaseID, ExpectedLeaseVersion: 1, FencingToken: claim1.FencingToken})
	if !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("stale Renew: err=%v, want domain.ErrStaleVersion", err)
	}
	after := snapshotExec2()
	if before != after {
		t.Fatalf("stale Renew mutated claim2's Execution: before=%#v after=%#v", before, after)
	}

	before = snapshotExec2()
	_, err = svc.Checkpoint(runner1Ctx, application.CheckpointRequest{RequestID: "fj:stale-checkpoint", ExecutionID: claim1.ExecutionID, LeaseID: claim1.LeaseID, FencingToken: claim1.FencingToken})
	if !errors.Is(err, domain.ErrLeaseNotOwned) {
		t.Fatalf("stale Checkpoint: err=%v, want domain.ErrLeaseNotOwned", err)
	}
	after = snapshotExec2()
	if before != after {
		t.Fatalf("stale Checkpoint mutated claim2's Execution: before=%#v after=%#v", before, after)
	}

	before = snapshotExec2()
	_, err = svc.AcceptResult(runner1Ctx, application.AcceptResultRequest{RequestID: "fj:stale-accept", ExecutionID: claim1.ExecutionID, LeaseID: claim1.LeaseID, ExpectedExecutionVersion: start1.Version, FencingToken: claim1.FencingToken, Succeeded: true})
	if !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("stale AcceptResult: err=%v, want domain.ErrStaleVersion", err)
	}
	after = snapshotExec2()
	if before != after {
		t.Fatalf("stale AcceptResult mutated claim2's Execution: before=%#v after=%#v", before, after)
	}
}
