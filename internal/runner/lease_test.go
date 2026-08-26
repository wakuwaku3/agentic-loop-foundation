package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// mutableClock is an injected clock a test can advance explicitly. It is
// shared by lease_test.go, journey_test.go and crash_test.go: every instance
// is created fresh inside a test function, so it is not package-level
// mutable state.
type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMutableClock(start time.Time) *mutableClock { return &mutableClock{now: start} }
func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *mutableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// leaseTestFixture builds a claimed lease directly against a fresh in-memory
// application.Service, so lease_test.go does not need the full journey.
type leaseTestFixture struct {
	service      *application.Service
	clock        *mutableClock
	leaseID      string
	fencingToken domain.FencingToken
	leaseVersion domain.Version
}

func newLeaseTestFixture(t *testing.T) *leaseTestFixture {
	t.Helper()
	clock := newMutableClock(time.Unix(1_700_000_000, 0).UTC())
	st := memory.New()
	service, err := application.NewServiceWithConfig(st, clock, &journeyIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	owner := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
	runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

	cap, err := service.Capture(owner, application.CaptureRequest{RequestID: "lk:capture", Text: "lease keeper fixture"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: this fixture claims, so its parent Requirement is moved to
	// domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- before the Plan, and the Plan
	// carries the POST-seed version.
	readyVersion := runnerSeedRequirementStatus(t, st, owner, cap.RequirementID, domain.RequirementReady)
	plan, err := service.Plan(owner, application.PlanRequest{RequestID: "lk:plan", RequirementID: cap.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := service.Prepare(owner, application.PrepareRequest{RequestID: "lk:prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatal(err)
	}
	target := domain.ControlTarget{RequirementID: mustRequirement(cap.RequirementID), IncrementID: mustIncrement(plan.IncrementID)}
	claim, err := service.Claim(runnerCtx, application.ClaimRequest{RequestID: "lk:claim", IncrementID: plan.IncrementID, ExpectedIncrementVersion: prepared.Version, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	return &leaseTestFixture{service: service, clock: clock, leaseID: claim.LeaseID, fencingToken: claim.FencingToken, leaseVersion: 1}
}

func TestLeaseKeeperRenewsAndHeartbeatsAsDistinctOperations(t *testing.T) {
	fx := newLeaseTestFixture(t)
	keeper := &LeaseKeeper{Service: fx.service, LeaseID: fx.leaseID, RequestBase: "journey-1:attempt-1"}
	runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

	firstExpiry := time.Time{}
	for i := 0; i < 3; i++ {
		fx.clock.Advance(20 * time.Second) // < LeaseTTL (1m) each tick, so the lease never lapses here.
		out, err := keeper.Tick(runnerCtx, fx.leaseVersion, fx.fencingToken)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if !out.Renewed {
			t.Fatalf("tick %d: expected a renewal", i)
		}
		if out.RenewResponse.ExpiresAt.Equal(firstExpiry) {
			t.Fatalf("tick %d: renew did not actually extend the lease (idempotent replay?)", i)
		}
		firstExpiry = out.RenewResponse.ExpiresAt
		fx.leaseVersion = out.RenewResponse.Version
		if !out.HeartbeatResult.Accepted {
			t.Fatalf("tick %d: heartbeat was not accepted", i)
		}
	}
	// A provider run spanning more than one LeaseTTL (60s here, across three
	// 20s ticks = 60s of elapsed clock time) kept the lease alive: the lease
	// never lapsed and every renew was a genuine extension, not an idempotent
	// replay of a prior response.
}

func TestLeaseKeeperRefusesRenewUnderStaleFencingToken(t *testing.T) {
	fx := newLeaseTestFixture(t)
	keeper := &LeaseKeeper{Service: fx.service, LeaseID: fx.leaseID, RequestBase: "journey-1:attempt-1"}
	runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

	stale := fx.fencingToken + 1
	if _, err := keeper.Tick(runnerCtx, fx.leaseVersion, stale); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("expected ErrStaleFence, got %v", err)
	}
}

func TestLeaseKeeperStopsRenewingOnceLeaseIsExpiredWithoutResurrectingIt(t *testing.T) {
	fx := newLeaseTestFixture(t)
	keeper := &LeaseKeeper{Service: fx.service, LeaseID: fx.leaseID, RequestBase: "journey-1:attempt-1"}
	runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

	// Advance well past the 1-minute LeaseTTL without ever renewing.
	fx.clock.Advance(5 * time.Minute)
	out, err := keeper.Tick(runnerCtx, fx.leaseVersion, fx.fencingToken)
	if !errors.Is(err, domain.ErrLeaseExpired) {
		t.Fatalf("expected ErrLeaseExpired on first observed expiry, got %v (out=%#v)", err, out)
	}
	if !out.Expired {
		t.Fatal("expected the keeper to report the lease as expired")
	}

	// A second tick must not attempt to resurrect the expired lease: it must
	// not call Renew again (which the fixture's fixed LeaseTTL would refuse
	// identically, but the keeper must not even try), while still reporting
	// a heartbeat.
	out2, err := keeper.Tick(runnerCtx, fx.leaseVersion, fx.fencingToken)
	if err != nil {
		t.Fatalf("second tick after expiry: %v", err)
	}
	if out2.Renewed {
		t.Fatal("keeper resurrected an expired lease by renewing it again")
	}
	if !out2.HeartbeatResult.Accepted {
		t.Fatal("expired-lease tick should still heartbeat")
	}
}

// TestReconnectRefusesNewClaimUntilPriorExecutionIsReportedThenAllows is
// acceptance A11: after a restart, replaying the journal must surface an
// unreconciled prior Execution, and while one is present the Runner must
// refuse to obtain a new claim with an explicit error rather than a silent
// skip; once it has been reported, a new claim is allowed again. Both
// directions are asserted against the same on-disk Journal, replayed twice
// (as a fresh OpenJournal would after a real restart), never against an
// in-memory flag.
func TestReconnectRefusesNewClaimUntilPriorExecutionIsReportedThenAllows(t *testing.T) {
	dir := t.TempDir()
	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}

	// No prior Execution at all yet: the gate must allow a claim.
	if err := RefuseClaimIfUnreconciled(journal); err != nil {
		t.Fatalf("fresh journal refused a claim: %v", err)
	}

	// A provider result is journaled pending canonical acceptance for a
	// prior attempt, exactly as a crashed/partitioned Runner's last attempt
	// would leave it (dp-v2-016 d5's own JournalProviderPending).
	if err := JournalProviderPending(journal, PendingProviderRecord{IncrementID: "increment-x", ExecutionID: "execution-x", WorkPacketDigest: "digest-x", Succeeded: true, Checkpoint: "cp-x"}); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: reopen the journal fresh from disk rather than
	// reusing the in-memory handle, so this is genuinely a replay-surfaced
	// fact, not an in-process flag.
	reopened, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = RefuseClaimIfUnreconciled(reopened)
	if !errors.Is(err, ErrUnreconciledExecution) {
		t.Fatalf("err=%v, want ErrUnreconciledExecution while execution-x is unreconciled", err)
	}
	if !strings.Contains(err.Error(), "execution-x") {
		t.Fatalf("err=%v, want it to name the dangling execution id", err)
	}

	// The prior Execution's result is now canonically accepted.
	if err := JournalResultAccepted(reopened, "execution-x", domain.ExecutionSucceeded); err != nil {
		t.Fatal(err)
	}

	// Restart again: replaying the journal must now show nothing dangling.
	reopenedAgain, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := RefuseClaimIfUnreconciled(reopenedAgain); err != nil {
		t.Fatalf("err=%v, want nil once execution-x has been reported", err)
	}
}
