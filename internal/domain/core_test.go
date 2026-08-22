package domain

import (
	"errors"
	"testing"
	"time"
)

func reqID(t *testing.T, value string) RequirementID {
	t.Helper()
	v, err := NewRequirementID(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func incID(t *testing.T, value string) IncrementID {
	t.Helper()
	v, err := NewIncrementID(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func execID(t *testing.T, value string) ExecutionID {
	t.Helper()
	v, err := NewExecutionID(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func runID(t *testing.T, value string) RunnerID {
	t.Helper()
	v, err := NewRunnerID(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func leaseID(t *testing.T, value string) LeaseID {
	t.Helper()
	v, err := NewLeaseID(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func actorID(t *testing.T, value string) ActorID {
	t.Helper()
	v, err := NewActorID(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func opID(t *testing.T, value string) OperationID {
	t.Helper()
	v, err := NewOperationID(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func requestID(t *testing.T, value string) RequestID {
	t.Helper()
	v, err := NewRequestID(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func releaseID(t *testing.T, value string) ReleaseID {
	t.Helper()
	v, err := NewReleaseID(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpaqueIDsAreDistinct(t *testing.T) {
	r := reqID(t, "r-1")
	i := incID(t, "i-1")
	if r.String() != "r-1" || i.String() != "i-1" {
		t.Fatal("opaque ID formatting changed")
	}
}

func TestRequirementAndIncrementLifecycleAreIndependent(t *testing.T) {
	r := Requirement{ID: reqID(t, "r"), Status: RequirementCaptured}
	a := time.Unix(10, 0).UTC()
	actor := actorID(t, "actor")
	var err error
	r, err = DecideRequirement(r, RequirementCommand{Kind: RequirementStartFraming, Actor: actor, At: a})
	if err != nil {
		t.Fatal(err)
	}
	r, err = DecideRequirement(r, RequirementCommand{Kind: RequirementReadyCommand, Actor: actor, At: a, ExpectedVersion: r.Version})
	if err != nil {
		t.Fatal(err)
	}
	i := Increment{ID: incID(t, "i"), RequirementID: r.ID, Status: IncrementProposed}
	i, err = DecideIncrement(i, IncrementCommand{Kind: IncrementPrepare, Actor: actor, At: a})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != RequirementReady || i.Status != IncrementReady {
		t.Fatal("lifecycles unexpectedly coupled")
	}
}

func TestControlStopDeniesAllSideEffectPermits(t *testing.T) {
	target := ControlTarget{InstallationID: "inst", IncrementID: incID(t, "inc")}
	intents := []ControlIntent{{Scope: ControlScope{Kind: ScopeInstallation, Value: "inst"}, Mode: ControlAllow, Revision: 1}, {Scope: ControlScope{Kind: ScopeIncrement, Value: "inc"}, Mode: ControlImmediateStop, Revision: 2}}
	effective := EffectiveControl(intents, target)
	for _, kind := range []PermitKind{PermitClaim, PermitCredential, PermitProcess, PermitExternalEffect, PermitIntegration, PermitPreviewDeploy, PermitPromotion} {
		if _, err := Permit(effective, PermitRequest{Kind: kind, ControlRevision: 2}); !errors.Is(err, ErrControlDenied) {
			t.Fatalf("%s permit = %v", kind, err)
		}
	}
}

func TestControlNewerSameScopeIntentReplacesOlderStop(t *testing.T) {
	target := ControlTarget{InstallationID: "inst"}
	control := EffectiveControl([]ControlIntent{
		{Scope: ControlScope{Kind: ScopeInstallation, Value: "inst"}, Mode: ControlImmediateStop, Revision: 2},
		{Scope: ControlScope{Kind: ScopeInstallation, Value: "inst"}, Mode: ControlAllow, Revision: 3},
	}, target)
	if control.Mode != ControlAllow || control.Revision != 3 {
		t.Fatalf("got %+v", control)
	}
}

func TestControlPermitModeMatrix(t *testing.T) {
	kinds := []PermitKind{PermitIntake, PermitClaim, PermitExternalEffect, PermitPromotion}
	want := map[ControlMode]map[PermitKind]bool{
		ControlAllow: {}, ControlPauseIntake: {PermitIntake: false}, ControlPauseClaim: {PermitClaim: false},
		ControlGracefulStop: {}, ControlImmediateStop: {}, ControlEmergencyStop: {}, ControlCancel: {},
	}
	for mode := range want {
		for _, kind := range kinds {
			control := EffectiveControlResult{Mode: mode, Revision: 7, Found: true}
			_, err := Permit(control, PermitRequest{Kind: kind, ControlRevision: 7})
			allowed := err == nil
			if expected, exists := want[mode][kind]; exists {
				allowed = expected
			}
			if mode == ControlAllow {
				allowed = true
			}
			if mode == ControlPauseIntake && kind != PermitIntake {
				allowed = true
			}
			if mode == ControlPauseClaim && kind != PermitClaim {
				allowed = true
			}
			if mode == ControlGracefulStop || mode == ControlImmediateStop || mode == ControlEmergencyStop || mode == ControlCancel {
				allowed = false
			}
			if allowed != (err == nil) {
				t.Fatalf("mode=%s kind=%s err=%v", mode, kind, err)
			}
		}
	}
}

func TestLeaseFenceAndExpiryRejectOldResults(t *testing.T) {
	base := time.Unix(100, 0).UTC()
	exp := base.Add(time.Minute)
	l, err := IssueLease(LeaseRequest{ID: leaseID(t, "l1"), ExecutionID: execID(t, "e1"), IncrementID: incID(t, "i1"), RunnerID: runID(t, "runner"), IssuedAt: base, ExpiresAt: exp})
	if err != nil {
		t.Fatal(err)
	}
	e := Execution{ID: l.ExecutionID, IncrementID: l.IncrementID, RunnerID: l.RunnerID, LeaseID: l.ID, FencingToken: l.FencingToken, Status: ExecutionLeased}
	if _, err := AcceptExecutionResult(e, l, ExecutionResult{ExecutionID: e.ID, LeaseID: e.LeaseID, FencingToken: l.FencingToken - 1, At: base.Add(time.Second), Succeeded: true}, EffectiveControlResult{}); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old fence accepted: %v", err)
	}
	if _, err := AcceptExecutionResult(e, l, ExecutionResult{ExecutionID: e.ID, LeaseID: e.LeaseID, FencingToken: l.FencingToken, At: exp, Succeeded: true}, EffectiveControlResult{}); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired result accepted: %v", err)
	}
	l2, err := IssueLease(LeaseRequest{ID: leaseID(t, "l2"), ExecutionID: execID(t, "e2"), IncrementID: l.IncrementID, RunnerID: l.RunnerID, PreviousFencingToken: l.FencingToken, IssuedAt: exp.Add(time.Second), ExpiresAt: exp.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if l2.FencingToken <= l.FencingToken {
		t.Fatal("fence did not increase")
	}
}

func TestReleaseGateRequiresEveryFreshCapabilityAndRecoveryEvidence(t *testing.T) {
	r := ReleaseCandidate{ID: releaseID(t, "rel"), CandidateID: releaseID(t, "candidate"), CandidateDigest: "candidate-digest", Version: 1, Status: ReleaseExercising, BundleDigest: "bundle", ContractDigest: "contract", DocsDigest: "docs", EvidenceDigest: "evidence", Capabilities: []string{"a", "b"}, CapabilityTargets: map[string]CapabilityTarget{"a": {Target: "t", Provider: "p"}, "b": {Target: "t", Provider: "p"}}, Evidence: []CapabilityEvidence{{Capability: "a", CandidateID: releaseID(t, "candidate"), CandidateDigest: "candidate-digest", BundleDigest: "bundle", ContractDigest: "contract", DocsDigest: "docs", Digest: "d", Provider: "p", Target: "t", Verified: true, Fresh: true}}, RollbackEvidence: true, ResumeEvidence: true}
	if err := r.CanPromote(); !errors.Is(err, ErrEvidenceIncomplete) {
		t.Fatalf("incomplete release accepted: %v", err)
	}
	r.Evidence = append(r.Evidence, CapabilityEvidence{Capability: "b", CandidateID: releaseID(t, "candidate"), CandidateDigest: "candidate-digest", BundleDigest: "bundle", ContractDigest: "contract", DocsDigest: "docs", Digest: "d", Provider: "p", Target: "t", Verified: true, Fresh: true})
	if err := r.CanPromote(); err != nil {
		t.Fatal(err)
	}
	if _, err := PromoteRelease(r, EffectiveControlResult{Mode: ControlImmediateStop, Found: true, Revision: 3}); !errors.Is(err, ErrControlDenied) {
		t.Fatalf("stopped release promoted: %v", err)
	}
}

func TestReleaseCloneDoesNotAliasCollections(t *testing.T) {
	r := ReleaseCandidate{Capabilities: []string{"a"}, Evidence: []CapabilityEvidence{{Capability: "a"}}, CapabilityTargets: map[string]CapabilityTarget{"a": {Target: "t", Provider: "p"}}}
	c := r.Clone()
	c.Capabilities[0] = "changed"
	c.Evidence[0].Capability = "changed"
	c.CapabilityTargets["a"] = CapabilityTarget{Target: "changed", Provider: "changed"}
	if r.Capabilities[0] != "a" || r.Evidence[0].Capability != "a" || r.CapabilityTargets["a"].Target != "t" {
		t.Fatal("release clone aliases mutable collections")
	}
}

func TestRetryBudgetStopsRepeatedFailures(t *testing.T) {
	b := RetryBudget{MaxAttempts: 4, MaxSameFingerprint: 2, MaxSameApproach: 3}
	for n := 0; n < 2; n++ {
		var err error
		b, err = b.Consume("same", "same")
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Consume("same", "same"); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("repeated failure accepted: %v", err)
	}
	if b.Allow("new-fingerprint", "new-approach") == false {
		t.Fatal("changed approach should be allowed")
	}
}

func TestDeterministicControlSequenceNeverAllowsAfterStop(t *testing.T) {
	target := ControlTarget{InstallationID: "x", IncrementID: incID(t, "i")}
	for n := Revision(1); n < 100; n++ {
		control := EffectiveControl([]ControlIntent{{Scope: ControlScope{Kind: ScopeInstallation, Value: "x"}, Mode: ControlImmediateStop, Revision: n}}, target)
		if _, err := Permit(control, PermitRequest{Kind: PermitPromotion, ControlRevision: n}); err == nil {
			t.Fatalf("sequence %d allowed promotion after stop", n)
		}
	}
}

func TestDeterministicRandomSequencePreservesPermitRevisionInvariant(t *testing.T) {
	// Small deterministic LCG: no wall clock, random source, or I/O enters the
	// domain test while still exploring varied mode/revision sequences.
	seed := uint32(17)
	modes := []ControlMode{ControlAllow, ControlPauseIntake, ControlPauseClaim, ControlImmediateStop, ControlEmergencyStop}
	for n := 0; n < 500; n++ {
		seed = seed*1664525 + 1013904223
		mode := modes[seed%uint32(len(modes))]
		revision := Revision(seed % 9)
		control := EffectiveControlResult{Mode: mode, Revision: revision, Found: true}
		_, err := Permit(control, PermitRequest{Kind: PermitPromotion, ControlRevision: revision})
		if (mode == ControlImmediateStop || mode == ControlEmergencyStop) && err == nil {
			t.Fatalf("sequence %d allowed stopped promotion", n)
		}
		if _, err := Permit(control, PermitRequest{Kind: PermitPromotion, ControlRevision: revision + 1}); err == nil {
			t.Fatalf("sequence %d allowed mismatched revision", n)
		}
	}
}

func TestStatefulDeterministicModelSequence(t *testing.T) {
	// The seed is deliberately deterministic and reported on every failure so
	// a failing command sequence can be replayed without external state.
	for seed := uint32(1); seed <= 64; seed++ {
		next := seed
		nextRand := func() uint32 { next = next*1664525 + 1013904223; return next }
		target := ControlTarget{InstallationID: "installation", IncrementID: incID(t, "increment")}
		intents := []ControlIntent{{Scope: ControlScope{Kind: ScopeInstallation, Value: "installation"}, Mode: ControlAllow, Revision: 1}}
		control := EffectiveControl(intents, target)
		if !control.Found || control.Mode != ControlAllow {
			t.Fatalf("seed=%d initial control=%+v", seed, control)
		}
		oldEffectPermit, err := Permit(control, PermitRequest{Kind: PermitExternalEffect, ControlRevision: 1, FencingToken: 1, ExpectedFencingToken: 1, Resource: "resource"})
		if err != nil {
			t.Fatalf("seed=%d initial effect permit: %v", seed, err)
		}
		kinds := []PermitKind{PermitIntake, PermitClaim, PermitCredential, PermitProcess, PermitExternalEffect, PermitIntegration, PermitPreviewDeploy, PermitPromotion}
		for _, kind := range kinds {
			decision, err := Permit(control, PermitRequest{Kind: kind, ControlRevision: 1, FencingToken: 1, ExpectedFencingToken: 1, Resource: "resource"})
			if err != nil || !decision.Allowed() {
				t.Fatalf("seed=%d kind=%s initial permit err=%v", seed, kind, err)
			}
			if _, err := EffectFromPermit(decision, control, 1, opID(t, "op"), requestID(t, "request"), kind, "resource", 1, 1, 1, nil); err != nil {
				t.Fatalf("seed=%d kind=%s effect err=%v", seed, kind, err)
			}
		}
		// requested -> effective stop: every permit and every old effect intent
		// must be rejected after the current control is re-read.
		intents = append(intents, ControlIntent{Scope: ControlScope{Kind: ScopeInstallation, Value: "installation"}, Mode: ControlImmediateStop, Revision: 2})
		control = EffectiveControl(intents, target)
		if control.Mode != ControlImmediateStop || control.Revision != 2 {
			t.Fatalf("seed=%d stop control=%+v", seed, control)
		}
		if _, err := EffectFromPermit(oldEffectPermit, control, 1, opID(t, "stale-op"), requestID(t, "stale-request"), PermitExternalEffect, "resource", 1, 1, 1, nil); err == nil {
			t.Fatalf("seed=%d stale permit crossed revision stop", seed)
		}
		for _, kind := range kinds {
			decision, err := Permit(control, PermitRequest{Kind: kind, ControlRevision: 2, FencingToken: 1, Resource: "resource"})
			if err == nil || decision.Allowed() {
				t.Fatalf("seed=%d kind=%s permit escaped stop", seed, kind)
			}
		}
		// Explicit higher-revision resume for the same scope restores permits.
		intents = append(intents, ControlIntent{Scope: ControlScope{Kind: ScopeInstallation, Value: "installation"}, Mode: ControlAllow, Revision: 3})
		control = EffectiveControl(intents, target)
		if control.Mode != ControlAllow || control.Revision != 3 {
			t.Fatalf("seed=%d resume control=%+v", seed, control)
		}

		base := time.Unix(int64(nextRand()%1000+100), 0).UTC()
		lease, err := IssueLease(LeaseRequest{ID: leaseID(t, "lease"), ExecutionID: execID(t, "execution"), IncrementID: target.IncrementID, RunnerID: runID(t, "runner"), ControlRevision: 3, IssuedAt: base, ExpiresAt: base.Add(time.Minute)})
		if err != nil {
			t.Fatalf("seed=%d issue lease: %v", seed, err)
		}
		if _, err := RenewLease(lease, lease.ExecutionID, lease.RunnerID, lease.FencingToken, base.Add(time.Second), base.Add(2*time.Minute), control); err != nil {
			t.Fatalf("seed=%d renew: %v", seed, err)
		}
		old, err := ExpireLease(lease, lease.ExpiresAt)
		if err != nil {
			t.Fatalf("seed=%d expire: %v", seed, err)
		}
		rotated, newer, err := RotateLease(old, LeaseRequest{ID: leaseID(t, "lease-new"), ExecutionID: execID(t, "execution-new"), IncrementID: target.IncrementID, RunnerID: runID(t, "runner-new"), ControlRevision: 3, IssuedAt: lease.ExpiresAt.Add(time.Second), ExpiresAt: lease.ExpiresAt.Add(2 * time.Minute)})
		if err != nil || rotated.Status != LeaseExpired || newer.FencingToken <= lease.FencingToken {
			t.Fatalf("seed=%d rotate old=%+v new=%+v err=%v", seed, rotated, newer, err)
		}
		if err := Validate(newer); err != nil {
			t.Fatalf("seed=%d validate lease: %v", seed, err)
		}
		if err := ValidateExecutionResult(Execution{ID: lease.ExecutionID, IncrementID: lease.IncrementID, RunnerID: lease.RunnerID, LeaseID: lease.ID, FencingToken: lease.FencingToken, ControlRevision: 3, Status: ExecutionRunning}, old, ExecutionResult{ExecutionID: lease.ExecutionID, LeaseID: lease.ID, FencingToken: lease.FencingToken, ControlRevision: 3, At: lease.ExpiresAt}, control); err == nil {
			t.Fatalf("seed=%d expired old result accepted", seed)
		}

		candidate := ReleaseCandidate{ID: releaseID(t, "release"), CandidateID: releaseID(t, "candidate"), CandidateDigest: "candidate-digest", Version: 1, Status: ReleaseExercising, BundleDigest: "bundle", ContractDigest: "contract", DocsDigest: "docs", EvidenceDigest: "evidence", Capabilities: []string{"cap"}, CapabilityTargets: map[string]CapabilityTarget{"cap": {Target: "preview", Provider: "provider"}}, Evidence: []CapabilityEvidence{{Capability: "cap", CandidateID: releaseID(t, "candidate"), CandidateDigest: "candidate-digest", BundleDigest: "bundle", ContractDigest: "contract", DocsDigest: "docs", Digest: "cap-evidence", Provider: "provider", Target: "preview", Verified: true, Fresh: true}}, RollbackEvidence: true, ResumeEvidence: true, ExpectedControlRevision: 3, FencingToken: newer.FencingToken}
		promotable, err := PromoteRelease(candidate, control)
		if err != nil || promotable.Status != ReleasePromoting {
			t.Fatalf("seed=%d promote=%+v err=%v", seed, promotable, err)
		}
		promotionPermit, err := Permit(control, PermitRequest{Kind: PermitPromotion, ControlRevision: 3, FencingToken: newer.FencingToken, ExpectedFencingToken: newer.FencingToken, Resource: "release"})
		if err != nil {
			t.Fatalf("seed=%d promotion permit: %v", seed, err)
		}
		stable, proof, err := CompletePromotionWithProof(promotable, control, promotionPermit)
		if err != nil || stable.Status != ReleaseStable || !proof.valid() {
			t.Fatalf("seed=%d stable=%+v proof=%v err=%v", seed, stable, proof.valid(), err)
		}
		req := Requirement{ID: reqID(t, "requirement"), Status: RequirementEvaluating}
		completed, err := CompleteRequirementFromRelease(req, proof, actorID(t, "actor"), base)
		if err != nil || completed.Status != RequirementCompleted || Validate(completed) != nil {
			t.Fatalf("seed=%d complete=%+v err=%v", seed, completed, err)
		}
	}
}
