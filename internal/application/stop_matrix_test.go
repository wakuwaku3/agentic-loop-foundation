package application_test

import (
	"context"
	"errors"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// wantStopMatrixCells is acceptance A12's named-constant total: 7 control
// modes (allow, pause-intake, pause-claim, graceful-stop, immediate-stop,
// emergency-stop, cancel) x 8 kinds (Capture/intake, Claim, Checkpoint,
// AcceptResult, and OutboxDispatcher items whose PermitKind is integration,
// promotion, preview-deploy and external-effect), driven through the real
// application Service and the real OutboxDispatcher over a store, never
// through a hand-built domain.PermitDecision (dp-v2-019 d10).
const wantStopMatrixCells = 56

var stopMatrixModes = []domain.ControlMode{
	domain.ControlAllow,
	domain.ControlPauseIntake,
	domain.ControlPauseClaim,
	domain.ControlGracefulStop,
	domain.ControlImmediateStop,
	domain.ControlEmergencyStop,
	domain.ControlCancel,
}

const (
	kindCapture      = "capture-intake"
	kindClaim        = "claim"
	kindCheckpoint   = "checkpoint"
	kindAcceptResult = "accept-result"
	kindIntegration  = "outbox-integration"
	kindPromotion    = "outbox-promotion"
	kindPreview      = "outbox-preview-deploy"
	kindExternal     = "outbox-external-effect"
)

var stopMatrixKinds = []string{kindCapture, kindClaim, kindCheckpoint, kindAcceptResult, kindIntegration, kindPromotion, kindPreview, kindExternal}

// wantStopModeAllowedCounts is acceptance A2's named table: the number of
// stopMatrixKinds cells this test measures as allowed under each control
// mode, and their total. These are not copied from any prior evidence
// prose (V2-019's a12 summary records pause-intake=2/8 and a total of
// 13/56, which is wrong -- see dp-v2-059 d3): they are the values
// TestStopModeByKindMatrix itself computes and asserts below, and
// docs/product/definition.md section 7 cites this test by name for
// exactly these counts.
var wantStopModeAllowedCounts = map[domain.ControlMode]int{
	domain.ControlAllow:         8,
	domain.ControlPauseIntake:   1,
	domain.ControlPauseClaim:    2,
	domain.ControlGracefulStop:  1,
	domain.ControlImmediateStop: 0,
	domain.ControlEmergencyStop: 0,
	domain.ControlCancel:        0,
}

const wantStopModeAllowedTotal = 12

// stopMatrixFixture builds one fresh Service+store per mode under test.
// incA is always claimed and started under ControlAllow, before any Control
// Intent is ever set: Claim and AcceptResult both internally construct a
// durable outbox effect via domain.EffectFromPermit, which (measured, see
// TestStopModeByKindMatrix's doc comment) requires the *current effective
// mode* to be exactly ControlAllow, full stop -- not merely
// permitAllowed(mode, kind) for the operation's own PermitKind. Because
// Renew (the only way to refresh a Lease's stored ControlRevision) is
// denied under every non-Allow mode exactly like Claim is, incA's Lease can
// never be refreshed to the stop mode's own revision once a stop mode is
// engaged, for any mode. So incA is always established once, under Allow,
// and every cell attempt after that runs against whatever mode is now
// engaged.
type stopMatrixFixture struct {
	service   *application.Service
	store     *memory.Store
	ownerCtx  context.Context
	runnerCtx context.Context
	mode      domain.ControlMode
	revision  domain.Revision // 0 when mode is ControlAllow (Control is never called)

	incA        string
	incB        string
	incBVersion domain.Version

	execA, leaseA string
	fenceA        domain.FencingToken
	execAVersion  domain.Version

	tag string
}

func buildStopMatrixFixture(t *testing.T, mode domain.ControlMode) *stopMatrixFixture {
	t.Helper()
	svc, st := service()
	ownerCtx := owner(context.Background())
	runnerCtx := runner(context.Background(), "runner-1")
	tag := string(mode)

	prepareIncrement := func(name string) (incrementID string, version domain.Version) {
		cap, err := svc.Capture(ownerCtx, application.CaptureRequest{RequestID: tag + ":cap-" + name})
		if err != nil {
			t.Fatalf("fixture setup: capture %s: %v", name, err)
		}
		// V2-089: a claim is refused unless the parent Requirement is in a
		// status that admits work. This fixture claims, so the parent is moved
		// to domain.RequirementReady -- '優先順位評価済みで実行可能',
		// docs/architecture/domain-model.md:265 -- and the Plan below carries
		// the POST-seed version, because the seed bumps the Requirement's
		// Version and a dropped or zeroed ExpectedRequirementVersion would
		// delete a real assertion.
		readyVersion := seedRequirementStatus(t, st, cap.RequirementID, domain.RequirementReady)
		plan, err := svc.Plan(ownerCtx, application.PlanRequest{RequestID: tag + ":plan-" + name, RequirementID: cap.RequirementID, ExpectedRequirementVersion: readyVersion})
		if err != nil {
			t.Fatalf("fixture setup: plan %s: %v", name, err)
		}
		prepared, err := svc.Prepare(ownerCtx, application.PrepareRequest{RequestID: tag + ":prep-" + name, IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
		if err != nil {
			t.Fatalf("fixture setup: prepare %s: %v", name, err)
		}
		return plan.IncrementID, prepared.Version
	}

	incAID, incAVersion := prepareIncrement("a")
	incBID, incBVersion := prepareIncrement("b")

	claim, err := svc.Claim(runnerCtx, application.ClaimRequest{RequestID: tag + ":claimA", IncrementID: incAID, ExpectedIncrementVersion: incAVersion, ControlRevision: 0})
	if err != nil {
		t.Fatalf("fixture setup: claim A: %v", err)
	}
	start, err := svc.Start(runnerCtx, application.StartRequest{RequestID: tag + ":startA", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1, ControlRevision: 0})
	if err != nil {
		t.Fatalf("fixture setup: start A: %v", err)
	}

	var revision domain.Revision
	if mode != domain.ControlAllow {
		ctrl, err := svc.Control(ownerCtx, application.ControlRequest{RequestID: tag + ":control", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: mode, At: (clock{}).Now()})
		if err != nil {
			t.Fatalf("fixture setup: control %s: %v", mode, err)
		}
		revision = ctrl.Revision
	}

	return &stopMatrixFixture{
		service: svc, store: st, ownerCtx: ownerCtx, runnerCtx: runnerCtx, mode: mode, revision: revision,
		incA: incAID, incB: incBID, incBVersion: incBVersion,
		execA: claim.ExecutionID, leaseA: claim.LeaseID, fenceA: claim.FencingToken, execAVersion: start.Version,
		tag: tag,
	}
}

func (fx *stopMatrixFixture) attemptCapture() error {
	_, err := fx.service.Capture(fx.ownerCtx, application.CaptureRequest{RequestID: fx.tag + ":cell-capture"})
	return err
}

// attemptClaim always targets incB with ControlRevision set to the mode's
// own current revision (the CALLER-supplied field the operation's own
// domain.Permit check compares against the authoritative revision -- unlike
// a pre-existing Lease's stored field, this one is never stale). This
// isolates the Claim cell to exactly domain.EffectFromPermit's
// Allow-only gate.
func (fx *stopMatrixFixture) attemptClaim() error {
	_, err := fx.service.Claim(fx.runnerCtx, application.ClaimRequest{RequestID: fx.tag + ":cell-claim", IncrementID: fx.incB, ExpectedIncrementVersion: fx.incBVersion, ControlRevision: fx.revision})
	return err
}

func (fx *stopMatrixFixture) attemptCheckpoint() error {
	_, err := fx.service.Checkpoint(fx.runnerCtx, application.CheckpointRequest{RequestID: fx.tag + ":cell-checkpoint", ExecutionID: fx.execA, LeaseID: fx.leaseA, FencingToken: fx.fenceA, ControlRevision: fx.revision})
	return err
}

func (fx *stopMatrixFixture) attemptAcceptResult() error {
	_, err := fx.service.AcceptResult(fx.runnerCtx, application.AcceptResultRequest{RequestID: fx.tag + ":cell-accept", ExecutionID: fx.execA, LeaseID: fx.leaseA, ExpectedExecutionVersion: fx.execAVersion, FencingToken: fx.fenceA, ControlRevision: fx.revision, Succeeded: true})
	return err
}

// attemptOutboxKind seeds one OutboxItem bound to incA's Lease (which
// always carries ControlRevision 0, from before any mode engaged) with the
// given PermitKind, and runs a real OutboxDispatcher.Dispatch pass with a
// counting fake sink that only counts deliveries of this specific id (the
// store also carries earlier cells' own outbox items, notably Claim's own
// "claim-issued" effect, which a real Dispatch pass also revisits in the
// same batch).
func (fx *stopMatrixFixture) attemptOutboxKind(t *testing.T, kind domain.PermitKind, id string) (sinkCalls int, delivered bool) {
	t.Helper()
	ctx := context.Background()
	err := fx.store.Transact(ctx, func(u application.UnitOfWork) error {
		target, found, e := u.CanonicalTarget(ctx, fx.incA, "runner-1")
		if e != nil {
			return e
		}
		if !found {
			return errors.New("canonical target for incA not found")
		}
		return u.Record(application.Event{ID: fx.tag + ":outbox-event-" + id}, &application.OutboxItem{
			ID: id, OperationID: "operation-" + id, Kind: "fake-effect", Target: "target-" + id,
			IncrementID: target.IncrementID.String(), LeaseID: fx.leaseA, RunnerID: "runner-1",
			FencingToken: fx.fenceA, ControlRevision: fx.revision, ControlTarget: target, PermitKind: kind,
		})
	})
	if err != nil {
		t.Fatalf("seed outbox %s: %v", kind, err)
	}
	sink := &stopMatrixSink{expectID: id}
	dispatcher, err := application.NewOutboxDispatcher(fx.store, clock{}, sink, application.DispatcherConfig{Owner: fx.tag + ":dispatcher-" + id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.Dispatch(ctx); err != nil {
		t.Fatalf("dispatch %s: %v", kind, err)
	}
	if err := fx.store.Transact(ctx, func(u application.UnitOfWork) error {
		item, found, e := u.Outbox(ctx, id)
		if e != nil {
			return e
		}
		delivered = found && item.Status == application.OutboxDelivered
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return sink.calls, delivered
}

type stopMatrixSink struct {
	expectID string
	calls    int
}

func (s *stopMatrixSink) Deliver(_ context.Context, delivery application.EffectDelivery) error {
	if delivery.OutboxID == s.expectID {
		s.calls++
	}
	return nil
}

// permitAllowedTable mirrors domain.Permit's own per-mode/per-kind gate
// (control.go's unexported permitAllowed), stated here only to compute this
// test's own expected outcome for Capture and Checkpoint, the two
// operations that never construct a durable outbox effect and so are
// gated by nothing else. This is not a re-derivation of the V2-010
// domain-closure proof, which already exhaustively proved
// EffectFromPermit's stop behaviour at the domain layer.
func permitAllowedTable(mode domain.ControlMode, kind domain.PermitKind) bool {
	switch mode {
	case domain.ControlAllow:
		return true
	case domain.ControlPauseIntake:
		return kind != domain.PermitIntake
	case domain.ControlPauseClaim:
		return kind != domain.PermitClaim
	case domain.ControlGracefulStop:
		return kind == domain.PermitCheckpoint
	case domain.ControlImmediateStop, domain.ControlEmergencyStop, domain.ControlCancel:
		return false
	}
	return false
}

// TestStopModeByKindMatrix is acceptance A12. It asserts the expected
// outcome of every (mode, kind) cell against wantStopMatrixCells, asserts
// that Checkpoint is the only kind graceful-stop still allows, and asserts
// that a denied cell produces no sink invocation and no delivered outbox
// record. pause-claim was previously covered by no test at all.
//
// Measured finding, load-bearing for this table: Capture and Checkpoint
// never construct a durable outbox effect, so they are gated purely by
// domain.Permit's per-kind table (permitAllowedTable below) and behave
// exactly as their mode names suggest -- pause-intake denies only intake,
// pause-claim denies only claim. Claim and AcceptResult, and every
// OutboxDispatcher effect kind (integration/promotion/preview-deploy/
// external-effect), all pass through domain.EffectFromPermit, which
// requires the *current effective mode* to be exactly ControlAllow to
// construct any effect at all -- a strictly stronger, mode-blind gate. So
// under every one of the six non-allow modes, including pause-intake and
// pause-claim, Claim/AcceptResult/every outbox kind are denied, even though
// permitAllowedTable would say pause-intake and pause-claim allow most of
// them in isolation. This is a real, measured fact about the deployed
// system's actual blast radius for pause-intake/pause-claim, not a
// rounding of the table to make the test pass; internal/domain is closed to
// this task, so it is reported here rather than changed.
func TestStopModeByKindMatrix(t *testing.T) {
	total := 0
	sumAllowed := 0
	gracefulStopAllowedKinds := map[string]bool{}
	for _, mode := range stopMatrixModes {
		fx := buildStopMatrixFixture(t, mode)
		effectEmittingAllowed := mode == domain.ControlAllow
		modeAllowed := 0

		for _, kind := range stopMatrixKinds {
			total++
			var allowed bool
			var err error
			var sinkCalls int
			var delivered bool
			switch kind {
			case kindCapture:
				err = fx.attemptCapture()
				allowed = permitAllowedTable(mode, domain.PermitIntake)
			case kindClaim:
				err = fx.attemptClaim()
				allowed = effectEmittingAllowed
			case kindCheckpoint:
				err = fx.attemptCheckpoint()
				allowed = permitAllowedTable(mode, domain.PermitCheckpoint)
			case kindAcceptResult:
				err = fx.attemptAcceptResult()
				allowed = effectEmittingAllowed
			case kindIntegration:
				sinkCalls, delivered = fx.attemptOutboxKind(t, domain.PermitIntegration, "int-"+string(mode))
				allowed = effectEmittingAllowed
			case kindPromotion:
				sinkCalls, delivered = fx.attemptOutboxKind(t, domain.PermitPromotion, "promo-"+string(mode))
				allowed = effectEmittingAllowed
			case kindPreview:
				sinkCalls, delivered = fx.attemptOutboxKind(t, domain.PermitPreviewDeploy, "prev-"+string(mode))
				allowed = effectEmittingAllowed
			case kindExternal:
				sinkCalls, delivered = fx.attemptOutboxKind(t, domain.PermitExternalEffect, "ext-"+string(mode))
				allowed = effectEmittingAllowed
			}

			switch kind {
			case kindCapture, kindClaim, kindCheckpoint, kindAcceptResult:
				if allowed && err != nil {
					t.Fatalf("mode=%s kind=%s: expected allowed, got error %v", mode, kind, err)
				}
				if !allowed {
					if err == nil {
						t.Fatalf("mode=%s kind=%s: expected denied, got no error", mode, kind)
					}
					if !errors.Is(err, domain.ErrControlDenied) {
						t.Fatalf("mode=%s kind=%s: expected domain.ErrControlDenied, got %v", mode, kind, err)
					}
				}
			default:
				if allowed {
					if sinkCalls != 1 || !delivered {
						t.Fatalf("mode=%s kind=%s: expected exactly one delivered effect, sinkCalls=%d delivered=%v", mode, kind, sinkCalls, delivered)
					}
				} else {
					if sinkCalls != 0 {
						t.Fatalf("mode=%s kind=%s: denied cell reached the sink %d times", mode, kind, sinkCalls)
					}
					if delivered {
						t.Fatalf("mode=%s kind=%s: denied cell was nonetheless delivered", mode, kind)
					}
				}
			}

			if mode == domain.ControlGracefulStop && allowed {
				gracefulStopAllowedKinds[kind] = true
			}

			if allowed {
				modeAllowed++
			}
		}

		if want := wantStopModeAllowedCounts[mode]; modeAllowed != want {
			t.Fatalf("mode=%s allowed cells=%d, want %d", mode, modeAllowed, want)
		}
		sumAllowed += modeAllowed
	}
	if total != wantStopMatrixCells {
		t.Fatalf("total cells=%d, want %d", total, wantStopMatrixCells)
	}
	if len(gracefulStopAllowedKinds) != 1 || !gracefulStopAllowedKinds[kindCheckpoint] {
		t.Fatalf("graceful-stop allowed kinds=%#v, want exactly {checkpoint}", gracefulStopAllowedKinds)
	}
	if sumAllowed != wantStopModeAllowedTotal {
		t.Fatalf("total allowed cells=%d, want %d", sumAllowed, wantStopModeAllowedTotal)
	}
}
