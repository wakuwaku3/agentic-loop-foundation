package application_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

type ids struct {
	mu sync.Mutex
	n  int
}

func (i *ids) Next(kind string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.n++
	return kind + "-" + string(rune('a'+i.n)), nil
}

type clock struct{ now time.Time }

func (c clock) Now() time.Time {
	if c.now.IsZero() {
		return time.Unix(1700000000, 0).UTC()
	}
	return c.now
}

func service() (*application.Service, *memory.Store) {
	st := memory.New()
	s, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		panic(err)
	}
	return s, st
}
func owner(ctx context.Context) context.Context {
	return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "actor-1"})
}
func runner(ctx context.Context, id string) context.Context {
	return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: "runner-subject", RunnerID: id})
}

func TestCallerAuthenticationAndFailClosedAuthority(t *testing.T) {
	st := memory.New()
	if _, err := application.NewService(st, clock{}, &ids{}); err == nil {
		t.Fatal("empty installation authority accepted")
	}
	s, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Capture(context.Background(), application.CaptureRequest{RequestID: "unauth"}); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("got %v", err)
	}
	if _, err = s.Capture(application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner", RunnerID: "r1"}), application.CaptureRequest{RequestID: "runner-capture"}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("runner role mismatch: %v", err)
	}
}
func TestCaptureIsIdempotent(t *testing.T) {
	s, st := service()
	ctx := owner(context.Background())
	a, err := s.Capture(ctx, application.CaptureRequest{RequestID: "request-1", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Capture(ctx, application.CaptureRequest{RequestID: "request-1", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("retry changed response: %#v %#v", a, b)
	}
	if got := len(st.Events()); got != 1 {
		t.Fatalf("events=%d", got)
	}
	if got := len(st.Outbox()); got != 0 {
		t.Fatalf("outbox=%d", got)
	}
}

func TestTransactionRollsBackAllRecords(t *testing.T) {
	_, st := service()
	ctx := owner(context.Background())
	before := len(st.Events())
	err := st.Transact(ctx, func(u application.UnitOfWork) error {
		id, _ := domain.NewRequirementID("r")
		if e := u.SaveRequirement(ctx, domain.Requirement{ID: id, Status: domain.RequirementCaptured, Version: 1}, 0); e != nil {
			return e
		}
		_ = u.Record(application.Event{ID: "event", AggregateID: "r"}, &application.OutboxItem{ID: "outbox"})
		return errors.New("abort")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	if _, ok := st.Requirement("r"); ok {
		t.Fatal("current state leaked")
	}
	if len(st.Events()) != before || len(st.Outbox()) != 0 {
		t.Fatal("event/outbox leaked")
	}
}

func TestOptimisticConflict(t *testing.T) {
	st := memory.New()
	ctx := context.Background()
	id, _ := domain.NewRequirementID("r")
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirement(ctx, domain.Requirement{ID: id, Status: domain.RequirementCaptured, Version: 1}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirement(ctx, domain.Requirement{ID: id, Status: domain.RequirementFraming, Version: 2}, 0)
	})
	if !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrentClaimOnlyOneSucceeds(t *testing.T) {
	s, st := service()
	ctx := owner(context.Background())
	p, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, p.RequirementID, domain.RequirementReady)
	plan, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: p.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(ctx, plan.IncrementID)
		if e != nil {
			return e
		}
		aid, _ := domain.NewActorID("a")
		at := clock{}.Now()
		next, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: aid, At: at, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, next, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, e := s.Claim(runner(ctx, "runner-"+string(rune('0'+n))), application.ClaimRequest{RequestID: "claim-" + string(rune('0'+n)), IncrementID: plan.IncrementID, ExpectedIncrementVersion: 2})
			results <- e
		}(n)
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful claims=%d", success)
	}
}

func TestStopDeniesPermit(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	if _, err := s.Control(ctx, application.ControlRequest{RequestID: "stop", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlImmediateStop, At: clock{}.Now()}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Permit(ctx, application.PermitRequest{RequestID: "permit-1", Kind: domain.PermitClaim, Target: domain.ControlTarget{InstallationID: "install"}, ControlRevision: 1})
	if !errors.Is(err, domain.ErrControlDenied) {
		t.Fatalf("expected control denial, got %v", err)
	}
	if result.Allowed {
		t.Fatal("stop allowed claim")
	}
}

func TestRequestFingerprintAndRawText(t *testing.T) {
	s, st := service()
	ctx := context.Background()
	if _, err := s.Capture(owner(ctx), application.CaptureRequest{}); err == nil {
		t.Fatal("empty request id accepted")
	}
	got, err := s.Capture(owner(ctx), application.CaptureRequest{RequestID: "same", Text: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.RequirementText(got.RequirementID); !ok {
		t.Fatal("raw text was not persisted")
	}
	if _, err = s.Capture(owner(ctx), application.CaptureRequest{RequestID: "same", Text: "changed"}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("got %v", err)
	}
	if len(st.Events()) != 1 {
		t.Fatalf("events=%d", len(st.Events()))
	}
	record, ok := st.Idempotency("same")
	if !ok || record.Fingerprint == "" || len(record.ResponseJSON) == 0 {
		t.Fatal("canonical idempotency record missing")
	}
}

func TestCaptureHonorsCanonicalInstallationIntakeControl(t *testing.T) {
	st := memory.New()
	s, _ := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	ctx := context.Background()
	now := clock{}.Now()
	if _, err := s.Control(owner(ctx), application.ControlRequest{RequestID: "pause", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlPauseIntake, At: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(owner(ctx), application.CaptureRequest{RequestID: "capture-paused", Text: "blocked"}); !errors.Is(err, domain.ErrControlDenied) {
		t.Fatalf("capture bypassed pause-intake: %v", err)
	}
}

func TestControlAndResultWriteOutbox(t *testing.T) {
	s, st := service()
	ctx := context.Background()
	now := clock{}.Now()
	control, err := s.Control(owner(ctx), application.ControlRequest{RequestID: "ctl", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "i"}, Mode: domain.ControlPauseClaim, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Outbox()) != 1 || st.Outbox()[0].ControlRevision != control.Revision {
		t.Fatal("control outbox metadata missing")
	}
	if _, err := s.Control(owner(ctx), application.ControlRequest{RequestID: "bad", Scope: domain.ControlScope{Kind: domain.ControlScopeKind("unknown"), Value: "i"}, Mode: domain.ControlAllow, At: now}); err == nil {
		t.Fatal("unknown scope accepted")
	}
}

func TestExpiredLeaseCanBeReclaimed(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, _ := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	ctx := context.Background()
	ctx = owner(ctx)
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, c.RequirementID, domain.RequirementReady)
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "p", RequirementID: c.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(ctx, p.IncrementID)
		if e != nil {
			return e
		}
		aid, _ := domain.NewActorID("a")
		ready, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: aid, At: t0, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, ready, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	first, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "cl1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	cclock.now = t0.Add(2 * time.Minute)
	second, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "cl2", IncrementID: p.IncrementID, ExpectedIncrementVersion: first.Version})
	if err != nil {
		t.Fatal(err)
	}
	if second.FencingToken <= first.FencingToken {
		t.Fatal("fencing token did not advance")
	}
}

func TestClaimCannotBypassCanonicalStop(t *testing.T) {
	st := memory.New()
	s, _ := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	ctx := context.Background()
	now := clock{}.Now()
	ctx = owner(ctx)
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, c.RequirementID, domain.RequirementReady)
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(ctx, p.IncrementID)
		if e != nil {
			return e
		}
		aid, _ := domain.NewActorID("a")
		next, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: aid, At: now, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, next, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Control(owner(ctx), application.ControlRequest{RequestID: "stop", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlImmediateStop, At: now}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Claim(runner(ctx, "r"), application.ClaimRequest{RequestID: "claim", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if !errors.Is(err, domain.ErrControlDenied) {
		t.Fatalf("claim bypassed stop: %v", err)
	}
}

func TestAcceptResultWritesEffectOutbox(t *testing.T) {
	s, st := service()
	ctx := context.Background()
	now := clock{}.Now()
	ctx = owner(ctx)
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-r"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, c.RequirementID, domain.RequirementReady)
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan-r", RequirementID: c.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(ctx, p.IncrementID)
		if e != nil {
			return e
		}
		aid, _ := domain.NewActorID("a")
		next, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: aid, At: now, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, next, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(runner(ctx, "runner-r"), application.ClaimRequest{RequestID: "claim-r", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		exec, _, e := u.Execution(ctx, claim.ExecutionID)
		if e != nil {
			return e
		}
		exec.Status = domain.ExecutionRunning
		return u.SaveExecution(ctx, exec, exec.Version)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptResult(runner(ctx, "cross-runner"), application.AcceptResultRequest{RequestID: "cross-result", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: 1, FencingToken: claim.FencingToken, Succeeded: true}); !errors.Is(err, domain.ErrLeaseNotOwned) {
		t.Fatalf("cross-runner result accepted: %v", err)
	}
	if _, err = s.AcceptResult(runner(ctx, "runner-r"), application.AcceptResultRequest{RequestID: "result-r", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: 1, FencingToken: claim.FencingToken, Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	outbox := st.Outbox()
	if len(outbox) < 2 {
		t.Fatalf("outbox=%d", len(outbox))
	}
	last := outbox[len(outbox)-1]
	if last.OperationID == "" || last.ExpectedVersion == 0 || last.FencingToken == 0 {
		t.Fatalf("incomplete result effect: %#v", last)
	}
}

// =============================================================================
// V2-058: Service.Claim's reclaim branch terminates the superseded Execution
// in the same transaction that reclaims its expired Lease (dp-v2-058 d1),
// closing the source of the orphan that previously only OrphanSweep could
// eventually converge. Acceptance A1's reproduction against the unmodified
// source (a real `go test` run showing the superseded Execution left at
// status=running, version=2 after reclaim) was captured before this fix
// landed and is recorded in .agents/v2/evidence/V2-058-application.json; it
// is not kept as a permanent test here because, once fixed, the same drive
// sequence no longer reproduces the bug -- that outcome is what
// TestClaimReclaimTerminatesSupersededExecutionAtSource below asserts
// instead (A5's positive invariant).
// =============================================================================

// isExecutionTerminalForTest mirrors domain.MarkExecutionLost's own notion
// of "already terminal" (internal/reconciler's executionIsTerminal uses the
// identical set) purely for test assertions; it duplicates no production
// logic and calls no unexported symbol.
func isExecutionTerminalForTest(status domain.ExecutionStatus) bool {
	switch status {
	case domain.ExecutionSucceeded, domain.ExecutionFailed, domain.ExecutionTerminated, domain.ExecutionLost:
		return true
	}
	return false
}

// TestClaimReclaimTerminatesSupersededExecutionAtSource is V2-058's core
// positive test (A2, A4's version/fencing assertions, A5). It drives a
// claim, start, clock-advance-past-TTL, reclaim sequence and asserts the
// superseded Execution is terminal (ExecutionLost) at the source of the
// reclaim itself -- not eventually, via OrphanSweep -- with its Version
// advanced by exactly one and its FencingToken retained, and that no
// non-terminal Execution is left behind an inactive Lease for the
// Increment.
func TestClaimReclaimTerminatesSupersededExecutionAtSource(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, c.RequirementID, domain.RequirementReady)
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	attempt1, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "claim-1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(runner(ctx, "r1"), application.StartRequest{RequestID: "start-1", ExecutionID: attempt1.ExecutionID, ExpectedExecutionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	before, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("attempt 1 execution missing before reclaim")
	}

	cclock.now = t0.Add(2 * time.Minute) // > the 1-minute LeaseTTL
	attempt2, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "claim-2", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version})
	if err != nil {
		t.Fatal(err)
	}
	if attempt2.ExecutionID == attempt1.ExecutionID {
		t.Fatal("reclaim reused the superseded execution id")
	}
	if attempt2.LeaseID == attempt1.LeaseID {
		t.Fatal("reclaim reused the superseded lease id")
	}
	if attempt2.FencingToken <= attempt1.FencingToken {
		t.Fatal("fencing token did not strictly increase on reclaim")
	}

	// A2/A5: the superseded Execution is terminal at the source, not
	// eventually via OrphanSweep.
	after, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("superseded execution disappeared")
	}
	if after.Status != domain.ExecutionLost {
		t.Fatalf("superseded execution status = %s, want %s", after.Status, domain.ExecutionLost)
	}
	if after.Version != before.Version+1 {
		t.Fatalf("superseded execution version = %d, want exactly one more than %d", after.Version, before.Version)
	}
	if after.FencingToken != before.FencingToken {
		t.Fatalf("superseded execution fencing token changed: before=%d after=%d", before.FencingToken, after.FencingToken)
	}

	// A5: the positive invariant, checked directly against the store (never
	// by invoking OrphanSweep) -- no non-terminal Execution exists behind
	// an inactive Lease for this Increment.
	supersededLease, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("superseded lease missing")
	}
	if supersededLease.ActiveAt(cclock.now) {
		t.Fatal("superseded lease should be inactive after expiry")
	}
	if !isExecutionTerminalForTest(after.Status) {
		t.Fatalf("no non-terminal execution may exist behind an inactive lease for this increment; got status %s", after.Status)
	}
}

// TestClaimReclaimAtomicWithSupersededTermination is A4: the superseded
// Execution's termination and the rest of the reclaim are one unit. It
// forces Claim's reclaim transaction to fail AFTER the termination work has
// already been staged inside the same closure (by requesting a Target whose
// RequirementID cannot match the canonical target that Plan already saved,
// which Claim only checks later in the same mutate closure via sameTarget)
// and asserts that neither the new Lease nor the superseded Execution's
// terminal status is visible; a subsequent successful reclaim then makes
// both visible together.
func TestClaimReclaimAtomicWithSupersededTermination(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, c.RequirementID, domain.RequirementReady)
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	attempt1, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "claim-1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(runner(ctx, "r1"), application.StartRequest{RequestID: "start-1", ExecutionID: attempt1.ExecutionID, ExpectedExecutionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	before, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("attempt 1 execution missing before reclaim")
	}
	leaseBefore, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("attempt 1 lease missing before reclaim")
	}

	cclock.now = t0.Add(2 * time.Minute)

	bogusReqID, err := domain.NewRequirementID("does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	_, failErr := s.Claim(runner(ctx, "r2"), application.ClaimRequest{
		RequestID: "claim-2-fails", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version,
		Target: domain.ControlTarget{RequirementID: bogusReqID},
	})
	if !errors.Is(failErr, domain.ErrControlDenied) {
		t.Fatalf("expected the forced failure to be ErrControlDenied, got %v", failErr)
	}

	// Neither the new Lease nor the superseded Execution's terminal status
	// is visible after the failed transaction.
	afterFailedExec, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("superseded execution missing after failed reclaim")
	}
	if afterFailedExec != before {
		t.Fatalf("superseded execution changed despite the reclaim transaction failing: before=%#v after=%#v", before, afterFailedExec)
	}
	afterFailedLease, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("superseded lease missing after failed reclaim")
	}
	if afterFailedLease != leaseBefore {
		t.Fatal("superseded lease changed despite the reclaim transaction failing")
	}

	// A successful reclaim then makes both visible together.
	attempt2, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "claim-2-succeeds", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version})
	if err != nil {
		t.Fatal(err)
	}
	if attempt2.ExecutionID == "" || attempt2.LeaseID == "" {
		t.Fatal("expected a usable new lease/execution")
	}
	afterExec, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("superseded execution missing after successful reclaim")
	}
	if afterExec.Status != domain.ExecutionLost {
		t.Fatalf("superseded execution status = %s, want %s", afterExec.Status, domain.ExecutionLost)
	}
	afterLease, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("superseded lease missing after successful reclaim")
	}
	if afterLease.ActiveAt(cclock.now) {
		t.Fatal("superseded lease should be inactive after a successful reclaim")
	}
}

// TestClaimReclaimSkipsWhenSupersededExecutionIsAbsent is A3's first skip
// branch: the expired Lease's ExecutionID has no corresponding Execution
// record at all (manufactured directly against the store, since
// Service.Claim itself never leaves a Lease pointing at nothing). Claim
// still succeeds and returns a usable new Lease/Execution.
func TestClaimReclaimSkipsWhenSupersededExecutionIsAbsent(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, c.RequirementID, domain.RequirementReady)
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	iid, err := domain.NewIncrementID(p.IncrementID)
	if err != nil {
		t.Fatal(err)
	}
	ghostLeaseID, err := domain.NewLeaseID("ghost-lease")
	if err != nil {
		t.Fatal(err)
	}
	ghostExecID, err := domain.NewExecutionID("ghost-execution-never-saved")
	if err != nil {
		t.Fatal(err)
	}
	ghostRunnerID, err := domain.NewRunnerID("r1")
	if err != nil {
		t.Fatal(err)
	}

	var leasedVersion domain.Version
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, ok, e := u.Increment(ctx, p.IncrementID)
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("increment not found")
		}
		aid, _ := domain.NewActorID("a")
		leased, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementLease, Actor: aid, At: t0, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		if e := u.SaveIncrement(ctx, leased, inc.Version); e != nil {
			return e
		}
		leasedVersion = leased.Version
		ghostLease := domain.Lease{ID: ghostLeaseID, IncrementID: iid, ExecutionID: ghostExecID, RunnerID: ghostRunnerID, FencingToken: 1, IssuedAt: t0, ExpiresAt: t0.Add(30 * time.Second), Status: domain.LeaseActive, Version: 1}
		return u.SaveLease(ctx, ghostLease, 0)
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok := st.Execution(ghostExecID.String()); ok {
		t.Fatal("test setup bug: the ghost execution unexpectedly exists")
	}

	cclock.now = t0.Add(2 * time.Minute) // past both the ghost lease's own expiry and LeaseTTL
	reclaim, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "reclaim", IncrementID: p.IncrementID, ExpectedIncrementVersion: leasedVersion})
	if err != nil {
		t.Fatalf("claim should succeed when the superseded execution is absent (skip branch), got %v", err)
	}
	if reclaim.ExecutionID == "" || reclaim.LeaseID == "" {
		t.Fatal("expected a usable new lease/execution")
	}
}

// TestClaimReclaimSkipsWhenSupersededExecutionAlreadyTerminal is A3's
// second skip branch: the superseded Execution already reached a terminal
// outcome (ExecutionSucceeded here, via the ordinary happy path) before its
// Lease's TTL elapsed. Claim still succeeds and leaves the terminal
// Execution untouched. It also proves the negative direction A3 requires:
// an unconditional domain.MarkExecutionLost call on this exact
// already-terminal Execution/Lease pair -- what Claim would do without the
// guard -- fails with domain.ErrInvalidTransition.
func TestClaimReclaimSkipsWhenSupersededExecutionAlreadyTerminal(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, c.RequirementID, domain.RequirementReady)
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	attempt1, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "claim-1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	start1, err := s.Start(runner(ctx, "r1"), application.StartRequest{RequestID: "start-1", ExecutionID: attempt1.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Checkpoint(runner(ctx, "r1"), application.CheckpointRequest{RequestID: "checkpoint-1", ExecutionID: attempt1.ExecutionID, LeaseID: attempt1.LeaseID, FencingToken: attempt1.FencingToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Permit(runner(ctx, "r1"), application.PermitRequest{RequestID: "permit-1", Kind: domain.PermitExternalEffect, FencingToken: attempt1.FencingToken, ExpectedFencingToken: attempt1.FencingToken, Resource: attempt1.ExecutionID}); err != nil {
		t.Fatal(err)
	}
	accepted, err := s.AcceptResult(runner(ctx, "r1"), application.AcceptResultRequest{RequestID: "accept-1", ExecutionID: attempt1.ExecutionID, LeaseID: attempt1.LeaseID, ExpectedExecutionVersion: start1.Version, FencingToken: attempt1.FencingToken, Succeeded: true})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != domain.ExecutionSucceeded {
		t.Fatalf("expected terminal success, got %s", accepted.Status)
	}

	terminalBefore, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("terminal execution missing")
	}
	leaseAfterAccept, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("lease missing")
	}

	// Negative direction (dp-v2-058 d5, A3): without the guard, Claim's
	// unconditional MarkExecutionLost call on this already-terminal
	// Execution would fail with exactly this error.
	if _, mlErr := domain.MarkExecutionLost(terminalBefore, leaseAfterAccept); !errors.Is(mlErr, domain.ErrInvalidTransition) {
		t.Fatalf("expected an unconditional MarkExecutionLost on an already-terminal execution to fail with ErrInvalidTransition, got %v", mlErr)
	}

	// With the guard restored (the actual production code), Claim still
	// succeeds and the terminal Execution is left untouched.
	cclock.now = t0.Add(2 * time.Minute)
	reclaim, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "claim-2", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version})
	if err != nil {
		t.Fatalf("claim should succeed when the superseded execution is already terminal (skip branch), got %v", err)
	}
	if reclaim.ExecutionID == "" || reclaim.LeaseID == "" {
		t.Fatal("expected a usable new lease/execution")
	}

	terminalAfter, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("terminal execution disappeared")
	}
	if terminalAfter != terminalBefore {
		t.Fatalf("already-terminal execution changed during reclaim: before=%#v after=%#v", terminalBefore, terminalAfter)
	}
}

// TestClaimReclaimSkipsWhenSupersededExecutionLinkageMismatches is A3's
// third skip branch: the superseded Execution's linkage to its own Lease is
// corrupted (its FencingToken no longer matches, manufactured directly
// against the store -- Service.Claim itself never produces this). Claim
// still succeeds and leaves the mismatched Execution untouched. It also
// proves the negative direction: an unconditional domain.MarkExecutionLost
// call on this exact mismatched pair fails with domain.ErrStaleFence.
func TestClaimReclaimSkipsWhenSupersededExecutionLinkageMismatches(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, c.RequirementID, domain.RequirementReady)
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	attempt1, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "claim-1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}

	var mismatched domain.Execution
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		exec, ok, e := u.Execution(ctx, attempt1.ExecutionID)
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("execution not found")
		}
		exec.FencingToken = exec.FencingToken + 100
		if e := u.SaveExecution(ctx, exec, exec.Version); e != nil {
			return e
		}
		mismatched = exec
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	leaseBefore, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("lease missing")
	}

	// Negative direction: an unconditional MarkExecutionLost call on this
	// mismatched pair fails.
	if _, mlErr := domain.MarkExecutionLost(mismatched, leaseBefore); !errors.Is(mlErr, domain.ErrStaleFence) {
		t.Fatalf("expected an unconditional MarkExecutionLost with mismatched linkage to fail with ErrStaleFence, got %v", mlErr)
	}

	cclock.now = t0.Add(2 * time.Minute)
	reclaim, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "claim-2", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version})
	if err != nil {
		t.Fatalf("claim should succeed when the superseded execution's linkage mismatches (skip branch), got %v", err)
	}
	if reclaim.ExecutionID == "" || reclaim.LeaseID == "" {
		t.Fatal("expected a usable new lease/execution")
	}

	after, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("mismatched execution disappeared")
	}
	if after != mismatched {
		t.Fatalf("mismatched execution changed during reclaim: before=%#v after=%#v", mismatched, after)
	}
}

// ===========================================================================
// V2-073: the Requirement's capture time comes from the transaction authority
// time, exactly once, and from nowhere else.
// ===========================================================================
//
// A4/A5. Every assertion below is about the SOURCE of the value, not about
// its formatting: the value must equal the At of the requirement.captured
// event recorded by the same operation, must not move when the transaction
// callback is retried, must not be suppliable by a caller, and must not be
// derivable from a read time or from the event log. Every instant comes from
// an injected clock; there is no sleep, no timer and no goroutine.

// errCaptureRetryAttempt aborts one emulated transaction attempt.
var errCaptureRetryAttempt = errors.New("emulated transaction retry")

// captureSpyUnit records the capture time of every Requirement staged through
// it, and forwards the transaction-scoped authority context the way both real
// adapters do.
type captureSpyUnit struct {
	application.UnitOfWork
	authority context.Context
	seen      *[]time.Time
}

func (u captureSpyUnit) AuthorityContext() context.Context { return u.authority }

func (u captureSpyUnit) SaveRequirement(ctx context.Context, value domain.Requirement, expected domain.Version) error {
	*u.seen = append(*u.seen, value.CapturedAt)
	return u.UnitOfWork.SaveRequirement(ctx, value, expected)
}

// captureRetryTransactor emulates a Firestore transaction retry
// deterministically: every attempt but the last runs the caller's callback in
// full and is then aborted, so its writes roll back, and the callback is
// invoked again with the SAME context -- which is what makes the authority
// time survive a retry. No goroutine and no contention is involved, so the
// number of attempts is fixed rather than raced for.
type captureRetryTransactor struct {
	inner    application.Transactor
	attempts int
	seen     []time.Time
}

func (r *captureRetryTransactor) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	for attempt := 1; attempt < r.attempts; attempt++ {
		err := r.inner.Transact(ctx, func(u application.UnitOfWork) error {
			if e := fn(captureSpyUnit{UnitOfWork: u, authority: ctx, seen: &r.seen}); e != nil {
				return e
			}
			return errCaptureRetryAttempt
		})
		if !errors.Is(err, errCaptureRetryAttempt) {
			return err
		}
	}
	return r.inner.Transact(ctx, func(u application.UnitOfWork) error {
		return fn(captureSpyUnit{UnitOfWork: u, authority: ctx, seen: &r.seen})
	})
}

// TestCaptureTimeIsTheTransactionAuthorityTime is A4's first assertion: the
// stored value equals the requirement.captured event's At, compared value to
// value rather than by format.
func TestCaptureTimeIsTheTransactionAuthorityTime(t *testing.T) {
	s, st := service()
	ctx := owner(context.Background())
	out, err := s.Capture(ctx, application.CaptureRequest{RequestID: "capture-authority", Text: "a requirement"})
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := st.Requirement(out.RequirementID)
	if !ok {
		t.Fatalf("requirement %q was not stored", out.RequirementID)
	}
	if !stored.CaptureRecorded() {
		t.Fatal("a freshly captured Requirement reports no capture time")
	}
	var captured []application.Event
	for _, e := range st.Events() {
		if e.Type == "requirement.captured" && e.AggregateID == out.RequirementID {
			captured = append(captured, e)
		}
	}
	if len(captured) != 1 {
		t.Fatalf("requirement.captured events = %d, want exactly 1", len(captured))
	}
	if !stored.CapturedAt.Equal(captured[0].At) {
		t.Fatalf("capture time %v != the requirement.captured event's At %v", stored.CapturedAt, captured[0].At)
	}
	// Byte-identity, not merely the same instant: the persisted form is the
	// representation, so the seconds, the nanoseconds and the location must
	// all agree.
	if stored.CapturedAt.Unix() != captured[0].At.Unix() || stored.CapturedAt.Nanosecond() != captured[0].At.Nanosecond() {
		t.Fatalf("capture time representation %d.%09d != event At %d.%09d", stored.CapturedAt.Unix(), stored.CapturedAt.Nanosecond(), captured[0].At.Unix(), captured[0].At.Nanosecond())
	}
	if stored.CapturedAt.Location().String() != captured[0].At.Location().String() {
		t.Fatalf("capture time location %q != event At location %q", stored.CapturedAt.Location().String(), captured[0].At.Location().String())
	}
	// The injected clock is the only source: the value is exactly what the
	// clock returns, so no second read and no adjustment happened.
	if !stored.CapturedAt.Equal(clock{}.Now()) {
		t.Fatalf("capture time %v is not the injected clock's instant %v", stored.CapturedAt, clock{}.Now())
	}
}

// TestCaptureTimeIsStableAcrossATransactionRetry is A4's retry assertion,
// driven through a transactor that re-invokes the callback the way Firestore
// does. The Firestore adapter's own contention retry cannot be driven without
// concurrency, which the determinism rule forbids, so the retry is emulated at
// the same seam the adapter retries at: the callback, with the same context.
func TestCaptureTimeIsStableAcrossATransactionRetry(t *testing.T) {
	const attempts = 4
	st := memory.New()
	tx := &captureRetryTransactor{inner: st, attempts: attempts}
	s, err := application.NewServiceWithConfig(tx, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	out, err := s.Capture(ctx, application.CaptureRequest{RequestID: "capture-retry", Text: "retried"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.seen) != attempts {
		t.Fatalf("the callback staged a Requirement %d times, want %d; the retry was not actually driven", len(tx.seen), attempts)
	}
	for i, at := range tx.seen {
		if !at.Equal(tx.seen[0]) {
			t.Fatalf("attempt %d staged capture time %v, attempt 1 staged %v; the value moved across a retry", i+1, at, tx.seen[0])
		}
		if at.Unix() != tx.seen[0].Unix() || at.Nanosecond() != tx.seen[0].Nanosecond() {
			t.Fatalf("attempt %d staged representation %d.%09d, attempt 1 staged %d.%09d", i+1, at.Unix(), at.Nanosecond(), tx.seen[0].Unix(), tx.seen[0].Nanosecond())
		}
	}
	stored, ok := st.Requirement(out.RequirementID)
	if !ok {
		t.Fatalf("requirement %q was not committed", out.RequirementID)
	}
	if !stored.CapturedAt.Equal(tx.seen[0]) {
		t.Fatalf("committed capture time %v != the value every attempt staged %v", stored.CapturedAt, tx.seen[0])
	}
	// The aborted attempts leaked nothing: exactly one Requirement and one
	// event are committed.
	if got := len(st.Events()); got != 1 {
		t.Fatalf("events = %d, want 1; an aborted attempt leaked", got)
	}
	for _, e := range st.Events() {
		if !stored.CapturedAt.Equal(e.At) {
			t.Fatalf("committed capture time %v != the committed event's At %v", stored.CapturedAt, e.At)
		}
	}
}

// TestCaptureRequestCarriesNoCaptureTimeField is A4's "a caller cannot supply
// the value" assertion at the application boundary. The transport-level
// refusal of a captured_at body field is asserted in internal/api.
func TestCaptureRequestCarriesNoCaptureTimeField(t *testing.T) {
	typ := reflect.TypeOf(application.CaptureRequest{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"RequestID", "RequirementID", "Text", "RepositoryID"}
	if len(got) != len(want) {
		t.Fatalf("CaptureRequest fields = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CaptureRequest fields = %v, want exactly %v", got, want)
		}
	}
}

// TestCaptureRefusesAZeroClockRatherThanRecordingAZeroCaptureTime is A4's
// last assertion: a clock that returns the zero instant still produces the
// pre-existing error, and no Requirement (and therefore no zero capture time)
// is written.
func TestCaptureRefusesAZeroClockRatherThanRecordingAZeroCaptureTime(t *testing.T) {
	st := memory.New()
	s, err := application.NewServiceWithConfig(st, zeroClock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Capture(owner(context.Background()), application.CaptureRequest{RequestID: "capture-zero-clock", Text: "x"})
	if err == nil {
		t.Fatal("a zero clock was accepted")
	}
	if err.Error() != "clock returned zero time" {
		t.Fatalf("error = %q, want the pre-existing \"clock returned zero time\"", err.Error())
	}
	if got := len(st.Events()); got != 0 {
		t.Fatalf("events = %d, want 0", got)
	}
}

// zeroClock returns the zero instant. clock{} above deliberately substitutes a
// fixed instant for its own zero value, so a separate type is needed to reach
// the refusal path.
type zeroClock struct{}

func (zeroClock) Now() time.Time { return time.Time{} }
