package domain

// This file proves, by exhaustive enumeration rather than by sampling, four
// of the five pure-domain Safety Invariants named in
// docs/architecture/validation.md section 2:
//
//   - Invariant 1: stale or expired fencing does not advance state.
//   - Invariant 2: no side-effect permit survives an effective stop.
//   - Invariant 3: a completed Requirement references a Stable Release.
//   - Forbidden transitions: DecideIncrement accepts exactly the declared
//     edge set and nothing else.
//   - Invariant 4: a candidate with missing capability evidence is not
//     promotable.
//
// (Invariant 5, no credential field in the domain schema, is proven in
// source_guard_test.go by an AST scan, not here.)
//
// Every enumeration below asserts two things: a per-case outcome, and the
// total number of cases actually enumerated against a named constant. The
// second assertion is what stops a later refactor from silently narrowing
// the covered space while the test keeps reporting PASS: if a future change
// removes a status, a mode, or a permit kind from the grid, the case-count
// assertion fails even though every remaining per-case assertion still
// passes.
//
// No test in this file uses math/rand, crypto/rand, or time.Now/Since/Until.
// Every varied value is produced by exhaustive nested enumeration over a
// closed, explicit set, or by an explicit table. This is enforced
// mechanically for the whole package in source_guard_test.go.

import (
	"errors"
	"testing"
	"time"
)

// ===========================================================================
// Invariant 1: stale or expired fencing does not advance state.
// ===========================================================================
//
// AcceptExecutionResult and StartExecution are exercised over the full cross
// product of:
//
//   - fencing-token offset:  stale, exact, unissued-future           (3)
//   - result/start instant:  before expiry, at expiry, after expiry  (3)
//   - LeaseStatus:           all four values                         (4)
//   - ExecutionStatus:       all nine values                         (9)
//   - ControlMode:           all seven values                        (7)
//   - ownership mismatch:    none, lease id, increment id, runner id (4)
//
// giving 3*3*4*9*7*4 = 9072 grid cells per function. Each function has its
// own legitimate success shape (AcceptExecutionResult succeeds only from
// ExecutionRunning/ExecutionCheckpointing; StartExecution succeeds only from
// ExecutionOffered/ExecutionLeased/ExecutionStarting), and both succeed only
// under an exact token, an active lease, an instant strictly before expiry,
// no ownership mismatch, and a ControlMode that does not deny the
// operation. Measured directly against the real functions (not asserted by
// construction), the surviving success set for AcceptExecutionResult is
// wider than "allow mode alone": ValidateExecutionResult only denies
// ControlGracefulStop, ControlImmediateStop, ControlEmergencyStop and
// ControlCancel, so ControlPauseIntake and ControlPauseClaim also permit
// accepting an already-running/checkpointing execution's result, because
// those two modes pause new intake/claim, not in-flight executions. That
// gives AcceptExecutionResult 2 statuses * 3 permissive modes = 6 success
// cells, and StartExecution 3 statuses * 1 permissive mode (Allow is the
// only mode StartExecution accepts) = 3 success cells. Every one of the
// remaining 9066 (Accept) / 9069 (Start) cells is asserted to return a
// non-nil error and an unmodified Execution, including its Version field.

type fenceOffset int

const (
	fenceOffsetStale fenceOffset = iota
	fenceOffsetExact
	fenceOffsetFuture
	fenceOffsetCount
)

type resultInstant int

const (
	instantBeforeExpiry resultInstant = iota
	instantAtExpiry
	instantAfterExpiry
	instantCount
)

type ownershipMismatch int

const (
	mismatchNone ownershipMismatch = iota
	mismatchLeaseID
	mismatchIncrementID
	mismatchRunnerID
	mismatchCount
)

func allLeaseStatuses() []LeaseStatus {
	return []LeaseStatus{LeaseActive, LeaseExpired, LeaseRevoked, LeaseReleased}
}
func allExecutionStatuses() []ExecutionStatus {
	return []ExecutionStatus{
		ExecutionOffered, ExecutionLeased, ExecutionStarting, ExecutionRunning,
		ExecutionCheckpointing, ExecutionSucceeded, ExecutionFailed,
		ExecutionTerminated, ExecutionLost,
	}
}
func allControlModes() []ControlMode {
	return []ControlMode{
		ControlAllow, ControlPauseIntake, ControlPauseClaim, ControlGracefulStop,
		ControlImmediateStop, ControlEmergencyStop, ControlCancel,
	}
}

const (
	wantLeaseStatusCount     = 4
	wantExecutionStatusCount = 9
	wantControlModeCount     = 7
)

func TestInvariant1FencingAndExpiryClosure(t *testing.T) {
	leaseStatuses := allLeaseStatuses()
	execStatuses := allExecutionStatuses()
	modes := allControlModes()
	if len(leaseStatuses) != wantLeaseStatusCount || len(execStatuses) != wantExecutionStatusCount || len(modes) != wantControlModeCount {
		t.Fatalf("axis size drifted: lease=%d exec=%d mode=%d", len(leaseStatuses), len(execStatuses), len(modes))
	}

	const wantGridSize = int(fenceOffsetCount) * int(instantCount) * wantLeaseStatusCount * wantExecutionStatusCount * wantControlModeCount * int(mismatchCount)
	const wantGridSizeLiteral = 9072
	if wantGridSize != wantGridSizeLiteral {
		t.Fatalf("grid size constant drifted: %d", wantGridSize)
	}

	const (
		wantAcceptSuccessCells = 6
		wantStartSuccessCells  = 3
	)

	issuedAt := time.Unix(2_000_000, 0).UTC()
	expiresAt := issuedAt.Add(60 * time.Second)
	const authoritativeToken FencingToken = 10
	const authoritativeRevision Revision = 3

	instantAt := func(i resultInstant) time.Time {
		switch i {
		case instantBeforeExpiry:
			return issuedAt.Add(30 * time.Second)
		case instantAtExpiry:
			return expiresAt
		default:
			return issuedAt.Add(90 * time.Second)
		}
	}
	tokenFor := func(f fenceOffset) FencingToken {
		switch f {
		case fenceOffsetStale:
			return authoritativeToken - 1
		case fenceOffsetExact:
			return authoritativeToken
		default:
			return authoritativeToken + 1
		}
	}

	total := 0
	acceptSuccess, acceptRejectChecked := 0, 0
	startSuccess, startRejectChecked := 0, 0

	for fo := fenceOffset(0); fo < fenceOffsetCount; fo++ {
		for ii := resultInstant(0); ii < instantCount; ii++ {
			at := instantAt(ii)
			for _, ls := range leaseStatuses {
				for _, es := range execStatuses {
					for _, mode := range modes {
						for mm := ownershipMismatch(0); mm < mismatchCount; mm++ {
							total++

							baseLease := Lease{
								ID: "lease-1", ExecutionID: "exec-1", IncrementID: "inc-1", RunnerID: "runner-1",
								FencingToken: authoritativeToken, ControlRevision: authoritativeRevision,
								IssuedAt: issuedAt, ExpiresAt: expiresAt, Status: ls, Version: 7,
							}
							control := EffectiveControlResult{Found: true, Mode: mode, Revision: authoritativeRevision}

							// --- AcceptExecutionResult ---
							acceptExec := Execution{
								ID: "exec-1", IncrementID: "inc-1", RunnerID: "runner-1", LeaseID: "lease-1",
								FencingToken: authoritativeToken, ControlRevision: authoritativeRevision, Status: es, Version: 9,
							}
							applyMismatch(&acceptExec, mm)
							result := ExecutionResult{
								ExecutionID: acceptExec.ID, LeaseID: acceptExec.LeaseID,
								FencingToken: tokenFor(fo), ControlRevision: authoritativeRevision, At: at, Succeeded: true,
							}
							nextExec, err := AcceptExecutionResult(acceptExec, baseLease, result, control)
							isAcceptSuccessShape := fo == fenceOffsetExact && ii == instantBeforeExpiry && ls == LeaseActive &&
								(es == ExecutionRunning || es == ExecutionCheckpointing) && mm == mismatchNone &&
								(mode == ControlAllow || mode == ControlPauseIntake || mode == ControlPauseClaim)
							if isAcceptSuccessShape {
								acceptSuccess++
								if err != nil {
									t.Fatalf("AcceptExecutionResult unexpectedly denied at success shape fo=%v ii=%v ls=%v es=%v mode=%v: %v", fo, ii, ls, es, mode, err)
								}
							} else {
								acceptRejectChecked++
								if err == nil {
									t.Fatalf("AcceptExecutionResult accepted outside its closed success shape: fo=%v ii=%v ls=%v es=%v mode=%v mm=%v", fo, ii, ls, es, mode, mm)
								}
								if nextExec != acceptExec {
									t.Fatalf("AcceptExecutionResult mutated a rejected execution: got %+v want %+v", nextExec, acceptExec)
								}
							}

							// --- StartExecution ---
							startExec := Execution{
								ID: "exec-1", IncrementID: "inc-1", RunnerID: "runner-1", LeaseID: "lease-1",
								FencingToken: tokenFor(fo), ControlRevision: authoritativeRevision, Status: es, Version: 9,
							}
							applyMismatch(&startExec, mm)
							nextStart, err2 := StartExecution(startExec, baseLease, at, control)
							isStartSuccessShape := fo == fenceOffsetExact && ii == instantBeforeExpiry && ls == LeaseActive &&
								(es == ExecutionOffered || es == ExecutionLeased || es == ExecutionStarting) && mm == mismatchNone &&
								mode == ControlAllow
							if isStartSuccessShape {
								startSuccess++
								if err2 != nil {
									t.Fatalf("StartExecution unexpectedly denied at success shape fo=%v ii=%v ls=%v es=%v mode=%v: %v", fo, ii, ls, es, mode, err2)
								}
							} else {
								startRejectChecked++
								if err2 == nil {
									t.Fatalf("StartExecution accepted outside its closed success shape: fo=%v ii=%v ls=%v es=%v mode=%v mm=%v", fo, ii, ls, es, mode, mm)
								}
								if nextStart != startExec {
									t.Fatalf("StartExecution mutated a rejected execution: got %+v want %+v", nextStart, startExec)
								}
							}
						}
					}
				}
			}
		}
	}

	if total != wantGridSizeLiteral {
		t.Fatalf("enumerated %d grid cells, want %d", total, wantGridSizeLiteral)
	}
	if acceptSuccess != wantAcceptSuccessCells {
		t.Fatalf("AcceptExecutionResult success cells = %d, want %d", acceptSuccess, wantAcceptSuccessCells)
	}
	if acceptRejectChecked != wantGridSizeLiteral-wantAcceptSuccessCells {
		t.Fatalf("AcceptExecutionResult rejection cells checked = %d, want %d", acceptRejectChecked, wantGridSizeLiteral-wantAcceptSuccessCells)
	}
	if startSuccess != wantStartSuccessCells {
		t.Fatalf("StartExecution success cells = %d, want %d", startSuccess, wantStartSuccessCells)
	}
	if startRejectChecked != wantGridSizeLiteral-wantStartSuccessCells {
		t.Fatalf("StartExecution rejection cells checked = %d, want %d", startRejectChecked, wantGridSizeLiteral-wantStartSuccessCells)
	}
}

func applyMismatch(e *Execution, mm ownershipMismatch) {
	switch mm {
	case mismatchLeaseID:
		e.LeaseID = "wrong-lease"
	case mismatchIncrementID:
		e.IncrementID = "wrong-increment"
	case mismatchRunnerID:
		e.RunnerID = "wrong-runner"
	}
}

// ===========================================================================
// Invariant 2: no side-effect permit survives an effective stop.
// ===========================================================================
//
// Closure argument (required by the Work Order to be stated here):
//
// Permit's allow/deny branch reads only (control.Mode, control.Found,
// control.Revision, control.Scope) and (request.Kind, request.ControlRevision,
// request.FencingToken, request.ExpectedFencingToken, request.Resource).
// Scope and Resource are carried into the returned decision but never
// participate in an allow/deny branch. The function is therefore total over
// ControlMode x PermitKind x revision-relation x fence-relation: every input
// to Permit falls into exactly one of those cells, so enumerating the cross
// product decides Permit for every possible call, not for a sample (layer
// L1). EffectiveControl is a pure fold that keeps at most one surviving
// intent per scope (a newer intent for the same scope always replaces an
// older one); no other state is threaded across a sequence. Exhaustive
// breadth-first search over every intent sequence of length 1 to 4 drawn
// from a 21-symbol alphabet (3 scopes x 7 modes) with strictly increasing
// revisions therefore covers every stop/resume/overlap ordering the fold can
// produce, for sequences of any length: a length-5 sequence's prefix
// behaviour is already covered by some length-4 (or shorter) prefix in this
// search, because EffectiveControl only ever looks at the surviving intent
// per scope, never at how many intents preceded it (layer L2). Finally,
// EffectFromPermit is the only Effect constructor in the package, and it
// re-reads the current EffectiveControlResult and requires current.Mode ==
// ControlAllow before it will construct anything, independent of whether the
// PermitDecision it is given claims to be Allowed(). Counting constructed
// effects at every L2 prefix whose effective mode is a stop mode -
// including permits captured at an earlier, non-stop prefix and replayed
// after the stop takes effect - therefore closes the claim at the one place
// a side effect can become durable (layer L3).
//
// One measured, load-bearing exception exists at the Permit layer: under
// ControlGracefulStop, PermitCheckpoint is deliberately still allowed
// (permitAllowed's ControlGracefulStop case returns kind == PermitCheckpoint),
// which is exactly what the pre-existing, unmodified TestCheckpointPermitStopMatrix
// in core_test.go asserts. PermitCheckpoint.SideEffect() is true, so a naive
// "every side-effect kind is denied under every stop mode" claim is false at
// Permit()'s output. This is why the invariant is proven at L3 rather than at
// L1/L2 alone: EffectFromPermit refuses to materialize even an Allowed()
// Checkpoint decision unless the current control mode is exactly
// ControlAllow, so the Checkpoint exception is authorization-only and never
// reaches a durable Effect while any stop mode - including graceful-stop -
// is in effect. L1 and L2 below assert the real, measured shape of that one
// exception explicitly instead of silently asserting a stronger claim than
// the code makes; L3 asserts the unconditional zero the invariant actually
// requires.

func allPermitKindsCanonical() []PermitKind {
	return []PermitKind{
		PermitClaim, PermitIntake, PermitCredential, PermitProcess, PermitExternalEffect,
		PermitIntegration, PermitPreviewDeploy, PermitPromotion, PermitCheckpoint,
	}
}

const wantPermitKindCount = 9

const bogusPermitKind PermitKind = "bogus-permit-kind"
const bogusControlMode ControlMode = "bogus-control-mode"

func TestInvariant2Layer1PermitDecisionClosure(t *testing.T) {
	canonicalKinds := allPermitKindsCanonical()
	if len(canonicalKinds) != wantPermitKindCount {
		t.Fatalf("PermitKind axis drifted: %d", len(canonicalKinds))
	}
	modes := append(append([]ControlMode{}, allControlModes()...), "", bogusControlMode)
	kinds := append(append([]PermitKind{}, canonicalKinds...), bogusPermitKind)
	const wantModeAxis = wantControlModeCount + 2 // +empty, +unknown
	const wantKindAxis = wantPermitKindCount + 1  // +unknown
	if len(modes) != wantModeAxis || len(kinds) != wantKindAxis {
		t.Fatalf("L1 axis drifted: modes=%d kinds=%d", len(modes), len(kinds))
	}

	type revRelation int
	const (
		revExact revRelation = iota
		revLower
		revHigher
		revRelationCount
	)
	type fenceRelation int
	const (
		fenceZero fenceRelation = iota
		fenceMatching
		fenceMismatching
		fenceRelationCount
	)

	const wantL1Total = wantModeAxis * wantKindAxis * int(revRelationCount) * int(fenceRelationCount)
	const wantL1TotalLiteral = 810
	if wantL1Total != wantL1TotalLiteral {
		t.Fatalf("L1 total constant drifted: %d", wantL1Total)
	}

	stopModes := map[ControlMode]bool{
		ControlGracefulStop: true, ControlImmediateStop: true, ControlEmergencyStop: true, ControlCancel: true,
	}

	const authoritativeRevision Revision = 5

	total := 0
	consistent := 0
	stopSideEffectChecked := 0
	stopSideEffectAllowed := 0
	stopSideEffectDenied := 0

	for _, mode := range modes {
		for _, kind := range kinds {
			for rr := revRelation(0); rr < revRelationCount; rr++ {
				for fr := fenceRelation(0); fr < fenceRelationCount; fr++ {
					total++
					control := EffectiveControlResult{Found: true, Mode: mode, Revision: authoritativeRevision}
					var reqRevision Revision
					switch rr {
					case revExact:
						reqRevision = authoritativeRevision
					case revLower:
						reqRevision = authoritativeRevision - 1
					default:
						reqRevision = authoritativeRevision + 1
					}
					var expected, actual FencingToken
					switch fr {
					case fenceZero:
						expected, actual = 0, 42
					case fenceMatching:
						expected, actual = 42, 42
					default:
						expected, actual = 42, 99
					}
					req := PermitRequest{Kind: kind, ControlRevision: reqRevision, FencingToken: actual, ExpectedFencingToken: expected, Resource: "resource"}
					decision, err := Permit(control, req)

					// Base totality/consistency property, true for every one of
					// the 810 cells: Permit never returns a nil error with
					// Allowed()==false, nor a non-nil error with Allowed()==true.
					if (err == nil) != decision.Allowed() {
						t.Fatalf("Permit inconsistent mode=%q kind=%q rev=%v fence=%v: err=%v allowed=%v", mode, kind, rr, fr, err, decision.Allowed())
					}
					consistent++

					if stopModes[mode] && kind.SideEffect() {
						stopSideEffectChecked++
						isCheckpointExceptionShape := mode == ControlGracefulStop && kind == PermitCheckpoint && rr == revExact && fr != fenceMismatching
						if decision.Allowed() {
							stopSideEffectAllowed++
							if !isCheckpointExceptionShape {
								t.Fatalf("side-effect permit unexpectedly allowed under stop mode=%q kind=%q rev=%v fence=%v", mode, kind, rr, fr)
							}
						} else {
							stopSideEffectDenied++
							if isCheckpointExceptionShape {
								t.Fatalf("checkpoint exception unexpectedly denied mode=%q kind=%q rev=%v fence=%v: %v", mode, kind, rr, fr, err)
							}
							if !errors.Is(err, ErrControlDenied) && !errors.Is(err, ErrStaleFence) && kind != bogusPermitKind {
								t.Fatalf("side-effect denial under stop mode used an unexpected error: %v", err)
							}
						}
					}
				}
			}
		}
	}

	const (
		wantStopSideEffectChecked = 252
		wantStopSideEffectAllowed = 2
		wantStopSideEffectDenied  = 250
	)
	if total != wantL1TotalLiteral {
		t.Fatalf("L1 enumerated %d cases, want %d", total, wantL1TotalLiteral)
	}
	if consistent != wantL1TotalLiteral {
		t.Fatalf("L1 consistency held for %d/%d cases", consistent, wantL1TotalLiteral)
	}
	if stopSideEffectChecked != wantStopSideEffectChecked {
		t.Fatalf("L1 stop x side-effect-kind cells = %d, want %d", stopSideEffectChecked, wantStopSideEffectChecked)
	}
	if stopSideEffectAllowed != wantStopSideEffectAllowed {
		t.Fatalf("L1 stop x side-effect-kind allowed = %d, want %d (the graceful-stop/checkpoint exception)", stopSideEffectAllowed, wantStopSideEffectAllowed)
	}
	if stopSideEffectDenied != wantStopSideEffectDenied {
		t.Fatalf("L1 stop x side-effect-kind denied = %d, want %d", stopSideEffectDenied, wantStopSideEffectDenied)
	}
}

// intentSymbol is one letter of the 21-symbol alphabet used by layers L2/L3:
// one of 3 scopes crossed with one of 7 modes.
type intentSymbol struct {
	scope ControlScope
	mode  ControlMode
}

func l2Alphabet() []intentSymbol {
	scopes := []ControlScope{
		{Kind: ScopeInstallation, Value: "installation"},
		{Kind: ScopeIncrement, Value: "increment"},
		{Kind: ScopeRunner, Value: "runner"},
	}
	modes := allControlModes()
	alphabet := make([]intentSymbol, 0, len(scopes)*len(modes))
	for _, sc := range scopes {
		for _, m := range modes {
			alphabet = append(alphabet, intentSymbol{scope: sc, mode: m})
		}
	}
	return alphabet
}

const wantL2AlphabetSize = 21
const l2MaxSequenceLength = 4

// TestInvariant2Layer2And3EffectiveControlAndEffectClosure implements layers
// L2 and L3 of the invariant 2 closure argument documented above. It walks,
// by exhaustive breadth-first search, every ControlIntent sequence of length
// 1 to l2MaxSequenceLength over the 21-symbol alphabet, with strictly
// increasing revisions (revision == position in the sequence, 1-indexed,
// which is sufficient to realize every strictly increasing assignment
// EffectiveControl can distinguish). At every prefix it recomputes
// EffectiveControl (L2) and, whenever the effective mode is a stop mode,
// attempts to construct an Effect for all nine canonical PermitKind values,
// both from a freshly requested permit and by replaying every permit that
// was captured Allowed() at any earlier prefix in the same sequence (L3).
func TestInvariant2Layer2And3EffectiveControlAndEffectClosure(t *testing.T) {
	alphabet := l2Alphabet()
	if len(alphabet) != wantL2AlphabetSize {
		t.Fatalf("L2 alphabet size drifted: %d", len(alphabet))
	}
	kinds := allPermitKindsCanonical()
	if len(kinds) != wantPermitKindCount {
		t.Fatalf("PermitKind axis drifted: %d", len(kinds))
	}
	target := ControlTarget{InstallationID: "installation", IncrementID: "increment", RunnerID: "runner"}
	stopModes := map[ControlMode]bool{
		ControlGracefulStop: true, ControlImmediateStop: true, ControlEmergencyStop: true, ControlCancel: true,
	}

	type capturedPermit struct {
		decision PermitDecision
		kind     PermitKind
	}

	var (
		sequenceCount         int
		prefixCount           int
		stopPrefixCount       int
		sideEffectCheckCount  int
		exceptionGrantedCount int
		deniedSideEffectCount int
		directEffectChecks    int
		directEffectSucceeded int
		replayEffectChecks    int
		replayEffectSucceeded int
	)

	var walk func(seq []intentSymbol)
	walk = func(seq []intentSymbol) {
		sequenceCount++
		intents := make([]ControlIntent, len(seq))
		for i, s := range seq {
			intents[i] = ControlIntent{Scope: s.scope, Mode: s.mode, Revision: Revision(i + 1)}
		}
		var captured []capturedPermit
		for prefixLen := 1; prefixLen <= len(seq); prefixLen++ {
			prefixCount++
			control := EffectiveControl(intents[:prefixLen], target)
			isStop := stopModes[control.Mode]
			if isStop {
				stopPrefixCount++
				// Replay every permit captured Allowed() at an earlier prefix in
				// this same sequence against the current, now-stopped control.
				for _, c := range captured {
					replayEffectChecks++
					if _, err := EffectFromPermit(c.decision, control, 0, "op", "req", c.kind, "res", 1, 0, c.decision.Revision(), nil); err == nil {
						replayEffectSucceeded++
						t.Fatalf("replayed permit constructed an effect after stop: mode=%q kind=%q", control.Mode, c.kind)
					}
				}
			}
			for _, k := range kinds {
				decision, err := Permit(control, PermitRequest{Kind: k, ControlRevision: control.Revision, Resource: "res"})
				if err == nil && decision.Allowed() {
					captured = append(captured, capturedPermit{decision: decision, kind: k})
				}
				if isStop {
					directEffectChecks++
					if _, effErr := EffectFromPermit(decision, control, 0, "op", "req", k, "res", 1, 0, control.Revision, nil); effErr == nil {
						directEffectSucceeded++
						t.Fatalf("EffectFromPermit constructed an effect while effective mode is a stop mode: mode=%q kind=%q", control.Mode, k)
					}
					if k.SideEffect() {
						sideEffectCheckCount++
						isCheckpointException := control.Mode == ControlGracefulStop && k == PermitCheckpoint
						if err == nil && decision.Allowed() {
							exceptionGrantedCount++
							if !isCheckpointException {
								t.Fatalf("unexpected side-effect permit grant under stop mode=%q kind=%q", control.Mode, k)
							}
						} else {
							deniedSideEffectCount++
						}
					}
				}
			}
		}
		if len(seq) < l2MaxSequenceLength {
			for _, s := range alphabet {
				walk(append(append([]intentSymbol{}, seq...), s))
			}
		}
	}
	for _, s := range alphabet {
		walk([]intentSymbol{s})
	}

	const (
		wantSequenceCount         = 204204 // 21 + 21^2 + 21^3 + 21^4
		wantPrefixCount           = 806610
		wantStopPrefixCount       = 598296
		wantSideEffectCheckCount  = 3589776
		wantExceptionGrantedCount = 110622
		wantDeniedSideEffectCount = 3479154
		wantDirectEffectChecks    = 5384664
		wantReplayEffectChecks    = 2399724
	)
	if sequenceCount != wantSequenceCount {
		t.Fatalf("L2 sequence count = %d, want %d", sequenceCount, wantSequenceCount)
	}
	if prefixCount != wantPrefixCount {
		t.Fatalf("L2 prefix count = %d, want %d", prefixCount, wantPrefixCount)
	}
	if stopPrefixCount != wantStopPrefixCount {
		t.Fatalf("L2 stop-mode prefix count = %d, want %d", stopPrefixCount, wantStopPrefixCount)
	}
	if sideEffectCheckCount != wantSideEffectCheckCount {
		t.Fatalf("L2 stop x side-effect-kind checks = %d, want %d", sideEffectCheckCount, wantSideEffectCheckCount)
	}
	if exceptionGrantedCount != wantExceptionGrantedCount {
		t.Fatalf("L2 checkpoint-exception grants = %d, want %d", exceptionGrantedCount, wantExceptionGrantedCount)
	}
	if deniedSideEffectCount != wantDeniedSideEffectCount {
		t.Fatalf("L2 denied side-effect checks = %d, want %d", deniedSideEffectCount, wantDeniedSideEffectCount)
	}
	if directEffectChecks != wantDirectEffectChecks {
		t.Fatalf("L3 direct effect checks = %d, want %d", directEffectChecks, wantDirectEffectChecks)
	}
	if directEffectSucceeded != 0 {
		t.Fatalf("L3 direct effect successes at stop = %d, want 0", directEffectSucceeded)
	}
	if replayEffectChecks != wantReplayEffectChecks {
		t.Fatalf("L3 replay effect checks = %d, want %d", replayEffectChecks, wantReplayEffectChecks)
	}
	if replayEffectSucceeded != 0 {
		t.Fatalf("L3 replay effect successes at stop = %d, want 0", replayEffectSucceeded)
	}
	// L3's closing assertion: across every stop-mode prefix reachable within
	// this closure, direct and replayed effect construction is unconditionally
	// zero. This is the property invariant 2 actually names.
	if directEffectSucceeded+replayEffectSucceeded != 0 {
		t.Fatalf("L3 total effect successes at stop = %d, want 0", directEffectSucceeded+replayEffectSucceeded)
	}
}

// ===========================================================================
// Invariant 3: a completed Requirement references a Stable Release.
// ===========================================================================
//
// The reachability property (RequirementCompleted is never a DecideRequirement
// output) is decidable by breadth-first search to fixpoint over a finite
// labelled transition system, so exploring it exhaustively is a proof, not a
// sample: once every reachable status has had every command kind applied
// with no new status discovered, no additional exploration can change the
// answer.

func allRequirementStatuses() []RequirementStatus {
	return []RequirementStatus{
		RequirementCaptured, RequirementFraming, RequirementReady, RequirementActive,
		RequirementWaiting, RequirementNeedsInput, RequirementPaused, RequirementRecovering,
		RequirementEvaluating, RequirementCompleted, RequirementCancelled,
	}
}
func allRequirementCommandKinds() []RequirementCommandKind {
	return []RequirementCommandKind{
		RequirementStartFraming, RequirementReadyCommand, RequirementStart, RequirementWait,
		RequirementNeedInput, RequirementRecover, RequirementEvaluate, RequirementPause, RequirementCancel,
		RequirementResume,
	}
}

const (
	wantRequirementStatusCount  = 11
	wantRequirementCommandCount = 10
)

// requirementsEqual compares Requirement values field by field. Requirement
// contains an Increments []IncrementID slice, which makes the struct
// non-comparable with ==; every construction in this file leaves Increments
// nil on both sides, so a length-and-elementwise comparison is sufficient
// and avoids adding a reflect import to the test-file allowlist.
//
// V2-090 added PausedFrom to the comparison, and it is a MANDATORY addition
// rather than tidiness: PausedFrom is the only field on Requirement that a
// transition function writes, so every "mutated a rejected requirement"
// assertion in this package -- and in pause_resume_test.go, where the whole
// 55-cell table rests on it -- would have gone blind to the one field this
// change writes. internal/domain would have kept passing while measuring LESS
// than before.
func requirementsEqual(a, b Requirement) bool {
	if a.ID != b.ID || a.Status != b.Status || a.Version != b.Version || a.StableSnapshot != b.StableSnapshot || a.PausedFrom != b.PausedFrom {
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

func TestInvariant3RequirementCompletionClosure(t *testing.T) {
	statuses := allRequirementStatuses()
	commands := allRequirementCommandKinds()
	if len(statuses) != wantRequirementStatusCount || len(commands) != wantRequirementCommandCount {
		t.Fatalf("Requirement axis drifted: statuses=%d commands=%d", len(statuses), len(commands))
	}

	actor := ActorID("actor")
	at := time.Unix(3_000_000, 0).UTC()

	// Breadth-first search to fixpoint, starting from RequirementCaptured
	// (the only status a fresh Requirement can have).
	visited := map[RequirementStatus]bool{RequirementCaptured: true}
	queue := []RequirementStatus{RequirementCaptured}
	edgeCount := 0
	reachedCompleted := false
	for len(queue) > 0 {
		from := queue[0]
		queue = queue[1:]
		for _, kind := range commands {
			cur := Requirement{ID: "requirement", Status: from, Version: 5}
			cmd := RequirementCommand{Kind: kind, Actor: actor, At: at, ExpectedVersion: 5}
			next, err := DecideRequirement(cur, cmd)
			if err != nil {
				continue
			}
			edgeCount++
			if next.Status == RequirementCompleted {
				reachedCompleted = true
			}
			if next.StableSnapshot != (StableReleaseSnapshot{}) {
				t.Fatalf("DecideRequirement produced a non-zero StableSnapshot from %q via %q", from, kind)
			}
			if !visited[next.Status] {
				visited[next.Status] = true
				queue = append(queue, next.Status)
			}
		}
	}

	const (
		wantReachedStatusCount = 10 // all eleven statuses except RequirementCompleted
		wantEdgeCount          = 29
	)
	if reachedCompleted {
		t.Fatal("(a) RequirementCompleted is reachable through DecideRequirement")
	}
	if visited[RequirementCompleted] {
		t.Fatal("RequirementCompleted marked visited despite being unreachable")
	}
	if len(visited) != wantReachedStatusCount {
		t.Fatalf("reached status set size = %d, want %d (visited=%v)", len(visited), wantReachedStatusCount, visited)
	}
	if edgeCount != wantEdgeCount {
		t.Fatalf("DecideRequirement accepted edge count = %d, want %d", edgeCount, wantEdgeCount)
	}

	// (c) CompleteRequirementFromRelease returns ErrInvalidTransition from
	// every status other than evaluating, regardless of proof quality.
	nonEvaluating := 0
	for _, s := range statuses {
		if s == RequirementEvaluating {
			continue
		}
		nonEvaluating++
		cur := Requirement{ID: "requirement", Status: s, Version: 1}
		next, err := CompleteRequirementFromRelease(cur, StableReleaseProof{}, actor, at)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("CompleteRequirementFromRelease from %q: got err=%v, want ErrInvalidTransition", s, err)
		}
		if !requirementsEqual(next, cur) {
			t.Fatalf("CompleteRequirementFromRelease mutated a rejected requirement from %q", s)
		}
	}
	if nonEvaluating != wantRequirementStatusCount-1 {
		t.Fatalf("non-evaluating status count = %d, want %d", nonEvaluating, wantRequirementStatusCount-1)
	}

	// (d) From evaluating, a zero-value proof is rejected as incomplete evidence.
	evaluating := Requirement{ID: "requirement", Status: RequirementEvaluating, Version: 1}
	if _, err := CompleteRequirementFromRelease(evaluating, StableReleaseProof{}, actor, at); !errors.Is(err, ErrEvidenceIncomplete) {
		t.Fatalf("CompleteRequirementFromRelease with zero proof: got err=%v, want ErrEvidenceIncomplete", err)
	}

	// (e) With a proof produced by CompletePromotionWithProof, completion
	// succeeds and the StableSnapshot matches the promoted candidate exactly.
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
	stable, proof, err := CompletePromotionWithProof(promoting, control, permit)
	if err != nil || !proof.valid() {
		t.Fatalf("CompletePromotionWithProof: stable=%+v proof.valid=%v err=%v", stable, proof.valid(), err)
	}
	completed, err := CompleteRequirementFromRelease(evaluating, proof, actor, at)
	if err != nil {
		t.Fatalf("CompleteRequirementFromRelease with valid proof: %v", err)
	}
	if completed.Status != RequirementCompleted {
		t.Fatalf("completed.Status = %q, want completed", completed.Status)
	}
	snap := completed.StableSnapshot
	if snap.ReleaseID == "" || snap.ReleaseVersion == 0 || snap.BundleDigest == "" || snap.EvidenceDigest == "" {
		t.Fatalf("StableSnapshot has a zero field: %+v", snap)
	}
	if string(snap.ReleaseID) != string(stable.CandidateID) || snap.ReleaseVersion != stable.Version ||
		snap.BundleDigest != stable.BundleDigest || snap.EvidenceDigest != stable.EvidenceDigest {
		t.Fatalf("StableSnapshot %+v does not match promoted candidate %+v", snap, stable)
	}

	// Stale-version guard, shared assertion for both DecideRequirement and
	// DecideIncrement (see TestForbiddenIncrementTransitionsClosure for the
	// increment half).
	stale := Requirement{ID: "requirement", Status: RequirementReady, Version: 5}
	next, err := DecideRequirement(stale, RequirementCommand{Kind: RequirementStart, Actor: actor, At: at, ExpectedVersion: 4})
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("DecideRequirement with stale ExpectedVersion: got err=%v, want ErrStaleVersion", err)
	}
	if !requirementsEqual(next, stale) {
		t.Fatal("DecideRequirement mutated state on a stale version command")
	}
}

// fullySatisfiedReleaseCandidate builds the one ReleaseCandidate shape that
// TestInvariant4ReleaseCapabilityEvidenceClosure proves is unique: every
// declared capability has present, verified, fresh, digest-matching
// evidence, and both RollbackEvidence and ResumeEvidence are present.
func fullySatisfiedReleaseCandidate(t *testing.T) ReleaseCandidate {
	t.Helper()
	caps := []string{"cap-a", "cap-b", "cap-c"}
	r := ReleaseCandidate{
		ID: "release", Version: 1, Status: ReleaseExercising, Capabilities: append([]string(nil), caps...),
		CandidateID: "candidate", CandidateDigest: "candidate-digest", BundleDigest: "bundle",
		ContractDigest: "contract", DocsDigest: "docs", EvidenceDigest: "evidence",
		RollbackEvidence: true, ResumeEvidence: true, ExpectedControlRevision: 3, FencingToken: 44,
		CapabilityTargets: map[string]CapabilityTarget{},
	}
	for _, cap := range caps {
		r.CapabilityTargets[cap] = CapabilityTarget{Target: "target-" + cap, Provider: "provider-" + cap}
		r.Evidence = append(r.Evidence, CapabilityEvidence{
			Capability: cap, CandidateID: r.CandidateID, CandidateDigest: r.CandidateDigest,
			BundleDigest: r.BundleDigest, ContractDigest: r.ContractDigest, DocsDigest: r.DocsDigest,
			Digest: "evidence-digest-" + cap, Provider: "provider-" + cap, Target: "target-" + cap,
			Verified: true, Fresh: true,
		})
	}
	if err := r.CanPromote(); err != nil {
		t.Fatalf("fixture candidate is not fully satisfied: %v", err)
	}
	return r
}

// ===========================================================================
// Forbidden transitions (part of the "same closure" required by A5).
// ===========================================================================

func allIncrementStatuses() []IncrementStatus {
	return []IncrementStatus{
		IncrementProposed, IncrementReady, IncrementLeased, IncrementExecuting, IncrementVerifying,
		IncrementIntegrated, IncrementPreviewValidating, IncrementFailed, IncrementAccepted,
		IncrementReleased, IncrementAbandoned, IncrementCancelled,
	}
}
func allIncrementCommandKinds() []IncrementCommandKind {
	return []IncrementCommandKind{
		IncrementPrepare, IncrementLease, IncrementExecute, IncrementVerify, IncrementIntegrate,
		IncrementPreview, IncrementFail, IncrementAccept, IncrementRelease, IncrementAbandon,
		IncrementCancel, IncrementRecover,
	}
}

const (
	wantIncrementStatusCount  = 12
	wantIncrementCommandCount = 12
)

type incrementEdge struct {
	from IncrementStatus
	kind IncrementCommandKind
	to   IncrementStatus
}

// expectedIncrementEdges is transcribed from the lifecycle table in
// docs/architecture/domain-model.md section 5, matched against the twelve
// IncrementCommandKind values DecideIncrement actually implements. That
// section's table and this declared edge table now agree exactly: it lists
// no "ready --revise--> proposed" loop (IncrementCommandKind has no command
// that ever sets the next status to IncrementProposed; Proposed is only ever
// the initial status of a freshly created Increment) and no transition into
// a paused status (IncrementStatus has no Paused member, unlike
// RequirementStatus, which does define RequirementPaused and does implement
// it). Docs section 5 states that pausing work on an Increment is expressed
// by a Control Intent scoped to the Increment together with the parent
// Requirement's paused state, and that re-proposal is expressed by creating
// a new Increment rather than returning an existing one to Proposed.
var expectedIncrementEdges = []incrementEdge{
	{IncrementProposed, IncrementPrepare, IncrementReady},
	{IncrementReady, IncrementLease, IncrementLeased},
	{IncrementLeased, IncrementExecute, IncrementExecuting},
	{IncrementLeased, IncrementRecover, IncrementReady}, // diagram: "lost -> ready"
	{IncrementExecuting, IncrementVerify, IncrementVerifying},
	{IncrementExecuting, IncrementFail, IncrementFailed},
	{IncrementExecuting, IncrementRecover, IncrementReady},
	{IncrementVerifying, IncrementIntegrate, IncrementIntegrated},
	{IncrementVerifying, IncrementFail, IncrementFailed},
	{IncrementIntegrated, IncrementPreview, IncrementPreviewValidating},
	{IncrementPreviewValidating, IncrementAccept, IncrementAccepted},
	{IncrementPreviewValidating, IncrementExecute, IncrementExecuting}, // diagram: "revise" back into execution
	{IncrementPreviewValidating, IncrementFail, IncrementFailed},
	{IncrementAccepted, IncrementRelease, IncrementReleased},
	{IncrementFailed, IncrementRecover, IncrementReady},
	// abandon/cancel are accepted from every non-terminal status; released,
	// abandoned and cancelled are the terminal statuses (docs: "非成功終端" /
	// "終端"), and abandon/cancel are correctly denied once already terminal.
	{IncrementProposed, IncrementAbandon, IncrementAbandoned},
	{IncrementReady, IncrementAbandon, IncrementAbandoned},
	{IncrementLeased, IncrementAbandon, IncrementAbandoned},
	{IncrementExecuting, IncrementAbandon, IncrementAbandoned},
	{IncrementVerifying, IncrementAbandon, IncrementAbandoned},
	{IncrementIntegrated, IncrementAbandon, IncrementAbandoned},
	{IncrementPreviewValidating, IncrementAbandon, IncrementAbandoned},
	{IncrementFailed, IncrementAbandon, IncrementAbandoned},
	{IncrementAccepted, IncrementAbandon, IncrementAbandoned},
	{IncrementProposed, IncrementCancel, IncrementCancelled},
	{IncrementReady, IncrementCancel, IncrementCancelled},
	{IncrementLeased, IncrementCancel, IncrementCancelled},
	{IncrementExecuting, IncrementCancel, IncrementCancelled},
	{IncrementVerifying, IncrementCancel, IncrementCancelled},
	{IncrementIntegrated, IncrementCancel, IncrementCancelled},
	{IncrementPreviewValidating, IncrementCancel, IncrementCancelled},
	{IncrementFailed, IncrementCancel, IncrementCancelled},
	{IncrementAccepted, IncrementCancel, IncrementCancelled},
}

func TestForbiddenIncrementTransitionsClosure(t *testing.T) {
	statuses := allIncrementStatuses()
	commands := allIncrementCommandKinds()
	if len(statuses) != wantIncrementStatusCount || len(commands) != wantIncrementCommandCount {
		t.Fatalf("Increment axis drifted: statuses=%d commands=%d", len(statuses), len(commands))
	}

	actor := ActorID("actor")
	at := time.Unix(4_000_000, 0).UTC()

	// Breadth-first search to fixpoint, starting from IncrementProposed.
	visited := map[IncrementStatus]bool{IncrementProposed: true}
	queue := []IncrementStatus{IncrementProposed}
	seenEdges := map[incrementEdge]bool{}
	for len(queue) > 0 {
		from := queue[0]
		queue = queue[1:]
		for _, kind := range commands {
			cur := Increment{ID: "increment", RequirementID: "requirement", Status: from, Version: 5, PreviewCandidateID: "candidate", PreviewEvidenceDigest: "digest"}
			cmd := IncrementCommand{Kind: kind, Actor: actor, At: at, ExpectedVersion: 5, PreviewCandidateID: "candidate", PreviewEvidenceDigest: "digest"}
			next, err := DecideIncrement(cur, cmd)
			if err != nil {
				continue
			}
			seenEdges[incrementEdge{from, kind, next.Status}] = true
			if !visited[next.Status] {
				visited[next.Status] = true
				queue = append(queue, next.Status)
			}
		}
	}

	const wantReachedStatusCount = 12 // every IncrementStatus is reachable
	if len(visited) != wantReachedStatusCount {
		t.Fatalf("reached increment status set size = %d, want %d (visited=%v)", len(visited), wantReachedStatusCount, visited)
	}

	const wantEdgeCount = 33
	if len(seenEdges) != wantEdgeCount {
		t.Fatalf("DecideIncrement accepted edge count = %d, want %d", len(seenEdges), wantEdgeCount)
	}
	if len(expectedIncrementEdges) != wantEdgeCount {
		t.Fatalf("expectedIncrementEdges has %d entries, want %d", len(expectedIncrementEdges), wantEdgeCount)
	}
	expected := map[incrementEdge]bool{}
	for _, e := range expectedIncrementEdges {
		if expected[e] {
			t.Fatalf("duplicate entry in expectedIncrementEdges: %+v", e)
		}
		expected[e] = true
	}
	for e := range seenEdges {
		if !expected[e] {
			t.Fatalf("DecideIncrement accepts an edge not in the declared transition table (a newly permitted forbidden edge): %+v", e)
		}
	}
	for e := range expected {
		if !seenEdges[e] {
			t.Fatalf("declared transition table names an edge DecideIncrement does not accept: %+v", e)
		}
	}

	// Stale-version guard for DecideIncrement, matching Invariant 3's
	// assertion for DecideRequirement above.
	stale := Increment{ID: "increment", RequirementID: "requirement", Status: IncrementReady, Version: 5}
	next, err := DecideIncrement(stale, IncrementCommand{Kind: IncrementLease, Actor: actor, At: at, ExpectedVersion: 4})
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("DecideIncrement with stale ExpectedVersion: got err=%v, want ErrStaleVersion", err)
	}
	if next != stale {
		t.Fatal("DecideIncrement mutated state on a stale version command")
	}
}

// ===========================================================================
// Invariant 4: a candidate with missing capability evidence is not
// promotable.
// ===========================================================================

type capabilityCase struct {
	present, verified, fresh, digestAgree bool
}

func allCapabilityCases() []capabilityCase {
	var cases []capabilityCase
	for _, p := range []bool{false, true} {
		for _, v := range []bool{false, true} {
			for _, f := range []bool{false, true} {
				for _, d := range []bool{false, true} {
					cases = append(cases, capabilityCase{p, v, f, d})
				}
			}
		}
	}
	return cases
}

const wantCapabilityCaseCount = 16 // 2^4: present x Verified x Fresh x digest agreement

func buildReleaseCandidateFor(cases [3]capabilityCase, rollback, resume bool) ReleaseCandidate {
	caps := []string{"cap-a", "cap-b", "cap-c"}
	r := ReleaseCandidate{
		ID: "release", Version: 1, Status: ReleaseExercising, Capabilities: append([]string(nil), caps...),
		CandidateID: "candidate", CandidateDigest: "candidate-digest", BundleDigest: "bundle",
		ContractDigest: "contract", DocsDigest: "docs", EvidenceDigest: "evidence",
		RollbackEvidence: rollback, ResumeEvidence: resume, ExpectedControlRevision: 3, FencingToken: 44,
		CapabilityTargets: map[string]CapabilityTarget{},
	}
	for i, cap := range caps {
		r.CapabilityTargets[cap] = CapabilityTarget{Target: "target-" + cap, Provider: "provider-" + cap}
		c := cases[i]
		if !c.present {
			continue
		}
		ev := CapabilityEvidence{
			Capability: cap, Digest: "evidence-digest-" + cap, Provider: "provider-" + cap, Target: "target-" + cap,
			Verified: c.verified, Fresh: c.fresh, CandidateID: r.CandidateID,
			BundleDigest: r.BundleDigest, ContractDigest: r.ContractDigest, DocsDigest: r.DocsDigest,
		}
		if c.digestAgree {
			ev.CandidateDigest = r.CandidateDigest
		} else {
			ev.CandidateDigest = "wrong-candidate-digest"
		}
		r.Evidence = append(r.Evidence, ev)
	}
	return r
}

func TestInvariant4ReleaseCapabilityEvidenceClosure(t *testing.T) {
	cases := allCapabilityCases()
	if len(cases) != wantCapabilityCaseCount {
		t.Fatalf("capability case axis drifted: %d", len(cases))
	}

	const wantGridSize = wantCapabilityCaseCount * wantCapabilityCaseCount * wantCapabilityCaseCount * 2 * 2
	const wantGridSizeLiteral = 16384
	if wantGridSize != wantGridSizeLiteral {
		t.Fatalf("capability grid size constant drifted: %d", wantGridSize)
	}

	total := 0
	successCount := 0
	var fullySatisfied ReleaseCandidate
	for _, c0 := range cases {
		for _, c1 := range cases {
			for _, c2 := range cases {
				for _, rollback := range []bool{false, true} {
					for _, resume := range []bool{false, true} {
						total++
						candidate := buildReleaseCandidateFor([3]capabilityCase{c0, c1, c2}, rollback, resume)
						err := candidate.CanPromote()
						isFullySatisfied := c0 == (capabilityCase{true, true, true, true}) &&
							c1 == (capabilityCase{true, true, true, true}) &&
							c2 == (capabilityCase{true, true, true, true}) && rollback && resume
						if isFullySatisfied {
							successCount++
							if err != nil {
								t.Fatalf("CanPromote denied the fully-satisfied candidate: %v", err)
							}
							fullySatisfied = candidate
						} else if !errors.Is(err, ErrEvidenceIncomplete) {
							t.Fatalf("CanPromote on an incomplete candidate: got err=%v, want ErrEvidenceIncomplete (case %v/%v/%v rollback=%v resume=%v)", c0, c1, c2, rollback, resume, err)
						}
					}
				}
			}
		}
	}
	if total != wantGridSizeLiteral {
		t.Fatalf("enumerated %d capability grid cells, want %d", total, wantGridSizeLiteral)
	}
	if successCount != 1 {
		t.Fatalf("CanPromote succeeded for %d grid cells, want exactly 1", successCount)
	}

	// Cross the fully-satisfied candidate with all seven ControlMode values:
	// PromoteRelease succeeds only under ControlAllow, and is denied with
	// ErrControlDenied under every stop mode (and every pause mode, which
	// PromoteRelease also treats as non-allow).
	modes := allControlModes()
	if len(modes) != wantControlModeCount {
		t.Fatalf("ControlMode axis drifted: %d", len(modes))
	}
	allowSuccesses := 0
	deniedUnderNonAllow := 0
	for _, mode := range modes {
		control := EffectiveControlResult{Found: true, Mode: mode, Revision: fullySatisfied.ExpectedControlRevision}
		next, err := PromoteRelease(fullySatisfied, control)
		if mode == ControlAllow {
			allowSuccesses++
			if err != nil || next.Status != ReleasePromoting {
				t.Fatalf("PromoteRelease under Allow: next=%+v err=%v", next, err)
			}
			continue
		}
		deniedUnderNonAllow++
		if !errors.Is(err, ErrControlDenied) {
			t.Fatalf("PromoteRelease under %q: got err=%v, want ErrControlDenied", mode, err)
		}
		if next.Status != fullySatisfied.Status {
			t.Fatalf("PromoteRelease under %q mutated status to %q", mode, next.Status)
		}
		// A candidate that never advanced past its pre-promotion status can
		// never yield a valid StableReleaseProof: CompletePromotionWithProof
		// requires current.Status == ReleasePromoting.
		if _, proof, completeErr := CompletePromotionWithProof(next, control, PermitDecision{}); completeErr == nil || proof.valid() {
			t.Fatalf("CompletePromotionWithProof yielded a valid proof after a denied promotion under %q", mode)
		}
	}
	if allowSuccesses != 1 {
		t.Fatalf("PromoteRelease succeeded for %d modes, want exactly 1 (Allow)", allowSuccesses)
	}
	if deniedUnderNonAllow != wantControlModeCount-1 {
		t.Fatalf("PromoteRelease denied for %d non-allow modes, want %d", deniedUnderNonAllow, wantControlModeCount-1)
	}
}
