package domain

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

type ControlMode string

const (
	ControlAllow         ControlMode = "allow"
	ControlPauseIntake   ControlMode = "pause-intake"
	ControlPauseClaim    ControlMode = "pause-claim"
	ControlGracefulStop  ControlMode = "graceful-stop"
	ControlImmediateStop ControlMode = "immediate-stop"
	ControlEmergencyStop ControlMode = "emergency-stop"
	ControlCancel        ControlMode = "cancel"
)

type ControlScopeKind string

const (
	ScopeInstallation ControlScopeKind = "installation"
	ScopeRepository   ControlScopeKind = "repository"
	ScopeRequirement  ControlScopeKind = "requirement"
	ScopeIncrement    ControlScopeKind = "increment"
	ScopeRunner       ControlScopeKind = "runner"
	ScopeChannel      ControlScopeKind = "channel"
)

type ControlScope struct {
	Kind  ControlScopeKind `json:"kind"`
	Value string           `json:"value"`
}
type ControlIntent struct {
	Scope    ControlScope
	Mode     ControlMode
	Revision Revision
	Actor    ActorID
	At       time.Time
	Reason   string
}

// ControlProgress is durable observation, not an inference from an intent.
// A newly-created intent is requested only; later states require an explicit
// observation/evidence transition.
type ControlProgress struct {
	Revision       Revision
	State          ControlState
	RequestedAt    time.Time
	AcknowledgedAt time.Time
	EffectiveAt    time.Time
	VerifiedAt     time.Time
	EvidenceRef    string
}

type ControlTarget struct {
	InstallationID string
	RepositoryID   string
	RequirementID  RequirementID
	IncrementID    IncrementID
	RunnerID       RunnerID
	Channel        string
}

type EffectiveControlResult struct {
	Mode     ControlMode
	Revision Revision
	Scope    ControlScope
	Found    bool
}

func controlSeverity(mode ControlMode) int {
	switch mode {
	case ControlEmergencyStop:
		return 7
	case ControlImmediateStop, ControlCancel:
		return 6
	case ControlGracefulStop:
		return 5
	case ControlPauseClaim:
		return 3
	case ControlPauseIntake:
		return 2
	case ControlAllow:
		return 0
	}
	return -1
}
func validControlMode(mode ControlMode) bool { return controlSeverity(mode) >= 0 }
func matches(scope ControlScope, target ControlTarget) bool {
	if scope.Value == "" {
		return false
	}
	switch scope.Kind {
	case ScopeInstallation:
		return target.InstallationID == scope.Value
	case ScopeRepository:
		return target.RepositoryID == scope.Value
	case ScopeRequirement:
		return target.RequirementID.String() == scope.Value
	case ScopeIncrement:
		return target.IncrementID.String() == scope.Value
	case ScopeRunner:
		return target.RunnerID.String() == scope.Value
	case ScopeChannel:
		return target.Channel == scope.Value
	default:
		return false
	}
}

// EffectiveControl combines only intents which cover target. An intent is
// replaced by a newer intent for the same scope; overlapping scopes then use
// the most restrictive surviving policy.
func EffectiveControl(intents []ControlIntent, target ControlTarget) EffectiveControlResult {
	best := EffectiveControlResult{Mode: ControlAllow}
	for index, intent := range intents {
		if !validControlMode(intent.Mode) || !matches(intent.Scope, target) {
			continue
		}
		// A newer intent for the same scope is an explicit replacement. This is
		// what permits a higher-revision resume while preventing an old allow
		// from shadowing a newer stop.
		superseded := false
		for otherIndex, other := range intents {
			if otherIndex == index || other.Scope != intent.Scope || other.Revision <= intent.Revision {
				continue
			}
			superseded = true
			break
		}
		if superseded {
			continue
		}
		candidate := EffectiveControlResult{Mode: intent.Mode, Revision: intent.Revision, Scope: intent.Scope, Found: true}
		if !best.Found || controlSeverity(candidate.Mode) > controlSeverity(best.Mode) ||
			(controlSeverity(candidate.Mode) == controlSeverity(best.Mode) && candidate.Revision > best.Revision) {
			best = candidate
		}
	}
	return best
}

type PermitKind string

const (
	PermitClaim          PermitKind = "claim"
	PermitIntake         PermitKind = "intake"
	PermitCredential     PermitKind = "credential"
	PermitProcess        PermitKind = "process"
	PermitExternalEffect PermitKind = "external-effect"
	PermitIntegration    PermitKind = "integration"
	PermitPreviewDeploy  PermitKind = "preview-deploy"
	PermitPromotion      PermitKind = "promotion"
	PermitCheckpoint     PermitKind = "checkpoint"
)

func (k PermitKind) SideEffect() bool {
	return k != PermitClaim && k != PermitCredential && k != PermitProcess
}

type PermitRequest struct {
	Kind                 PermitKind
	Target               ControlTarget
	ControlRevision      Revision
	FencingToken         FencingToken
	ExpectedFencingToken FencingToken
	Resource             string
}
type PermitDecision struct {
	allowed      bool
	revision     Revision
	fencingToken FencingToken
	reason       string
	kind         PermitKind
	target       string
	scope        ControlScope
	nonce        *permitNonce
}

type permitNonce struct{}

func (p PermitDecision) Allowed() bool              { return p.allowed && p.nonce != nil }
func (p PermitDecision) Revision() Revision         { return p.revision }
func (p PermitDecision) FencingToken() FencingToken { return p.fencingToken }
func (p PermitDecision) Kind() PermitKind           { return p.kind }
func (p PermitDecision) Target() string             { return p.target }
func (p PermitDecision) Scope() ControlScope        { return p.scope }
func (p PermitDecision) Reason() string             { return p.reason }

// Permit is the last pure gate before a durable operation intent is emitted.
// It never emits credentials or performs I/O.
func Permit(control EffectiveControlResult, request PermitRequest) (PermitDecision, error) {
	if !validPermitKind(request.Kind) {
		return PermitDecision{}, fmt.Errorf("unknown permit kind %q", request.Kind)
	}
	if control.Mode == "" {
		control.Mode = ControlAllow
	}
	if control.Found && request.ControlRevision != control.Revision {
		return permitDenied(control.Revision, "control revision is not exact", request, control), ErrControlDenied
	}
	if !control.Found && request.ControlRevision != 0 {
		return permitDenied(0, "no control revision is authoritative", request, control), ErrControlDenied
	}
	if !permitAllowed(control.Mode, request.Kind) {
		return permitDenied(control.Revision, "control mode denies this operation", request, control), ErrControlDenied
	}
	if request.ExpectedFencingToken != 0 && request.FencingToken != request.ExpectedFencingToken {
		return permitDenied(request.ControlRevision, "stale fencing token", request, control), ErrStaleFence
	}
	return PermitDecision{allowed: true, revision: request.ControlRevision, fencingToken: request.FencingToken, kind: request.Kind, target: request.Resource, scope: control.Scope, nonce: &permitNonce{}}, nil
}
func permitDenied(revision Revision, reason string, request PermitRequest, control EffectiveControlResult) PermitDecision {
	return PermitDecision{revision: revision, reason: reason, kind: request.Kind, target: request.Resource, scope: control.Scope}
}
func validPermitKind(k PermitKind) bool {
	switch k {
	case PermitIntake, PermitClaim, PermitCredential, PermitProcess, PermitExternalEffect, PermitIntegration, PermitPreviewDeploy, PermitPromotion, PermitCheckpoint:
		return true
	}
	return false
}

func permitAllowed(mode ControlMode, kind PermitKind) bool {
	switch mode {
	case ControlAllow:
		return true
	case ControlPauseIntake:
		return kind != PermitIntake
	case ControlPauseClaim:
		return kind != PermitClaim
	case ControlGracefulStop:
		return kind == PermitCheckpoint
	case ControlImmediateStop, ControlEmergencyStop, ControlCancel:
		return false
	default:
		return false
	}
}

type Effect struct {
	OperationID     OperationID
	RequestID       RequestID
	Kind            PermitKind
	Target          string
	ExpectedVersion Version
	FencingToken    FencingToken
	ControlRevision Revision
	Payload         []byte
}

// EffectFromPermit is the only effect constructor. An adapter must first
// obtain a valid, scope-bound permit; stale or hand-written decisions cannot
// manufacture durable side effects.
func EffectFromPermit(decision PermitDecision, current EffectiveControlResult, currentFencingToken FencingToken, operationID OperationID, requestID RequestID, kind PermitKind, target string, expected Version, fence FencingToken, revision Revision, payload []byte) (Effect, error) {
	if current.Mode == "" {
		current.Mode = ControlAllow
	}
	if !decision.Allowed() || current.Mode != ControlAllow || decision.kind != kind || decision.target != target || decision.fencingToken != fence || decision.revision != revision || current.Revision != decision.revision || current.Scope != decision.scope || currentFencingToken != decision.fencingToken || (!current.Found && revision != 0) {
		return Effect{}, ErrControlDenied
	}
	if _, err := NewOperationID(operationID.String()); err != nil {
		return Effect{}, err
	}
	if _, err := NewRequestID(requestID.String()); err != nil {
		return Effect{}, err
	}
	if !validPermitKind(kind) || target == "" {
		return Effect{}, errors.New("effect intent requires kind and target")
	}
	return Effect{OperationID: operationID, RequestID: requestID, Kind: kind, Target: target, ExpectedVersion: expected, FencingToken: fence, ControlRevision: revision, Payload: append([]byte(nil), payload...)}, nil
}

// Keep imports and policy sorting deterministic for callers building indexes.
func SortIntents(intents []ControlIntent) []ControlIntent {
	out := append([]ControlIntent(nil), intents...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Revision != out[j].Revision {
			return out[i].Revision < out[j].Revision
		}
		return out[i].Scope.Value < out[j].Scope.Value
	})
	return out
}

type ControlState string

const (
	ControlRequested    ControlState = "requested"
	ControlAcknowledged ControlState = "acknowledged"
	ControlEffective    ControlState = "effective"
	ControlVerified     ControlState = "verified"
)

func AdvanceControlState(current ControlState, next ControlState) error {
	if current == "" && next == ControlRequested {
		return nil
	}
	valid := map[ControlState]ControlState{ControlRequested: ControlAcknowledged, ControlAcknowledged: ControlEffective, ControlEffective: ControlVerified}
	if valid[current] != next {
		return ErrInvalidTransition
	}
	return nil
}
