package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Opaque identifiers deliberately have distinct types.  The domain never
// parses their values; formatting and allocation belong to an adapter.
type RequirementID string
type IncrementID string
type ExecutionID string
type RunnerID string
type LeaseID string
type OperationID string
type RequestID string
type ReleaseID string
type ActorID string

func opaqueID[T ~string](value T) (T, error) {
	if strings.TrimSpace(string(value)) == "" {
		return "", ErrEmptyID
	}
	return value, nil
}

func NewRequirementID(v string) (RequirementID, error) { return opaqueID(RequirementID(v)) }
func NewIncrementID(v string) (IncrementID, error)     { return opaqueID(IncrementID(v)) }
func NewExecutionID(v string) (ExecutionID, error)     { return opaqueID(ExecutionID(v)) }
func NewRunnerID(v string) (RunnerID, error)           { return opaqueID(RunnerID(v)) }
func NewLeaseID(v string) (LeaseID, error)             { return opaqueID(LeaseID(v)) }
func NewOperationID(v string) (OperationID, error)     { return opaqueID(OperationID(v)) }
func NewRequestID(v string) (RequestID, error)         { return opaqueID(RequestID(v)) }
func NewReleaseID(v string) (ReleaseID, error)         { return opaqueID(ReleaseID(v)) }
func NewActorID(v string) (ActorID, error)             { return opaqueID(ActorID(v)) }

func (v RequirementID) String() string { return string(v) }
func (v IncrementID) String() string   { return string(v) }
func (v ExecutionID) String() string   { return string(v) }
func (v RunnerID) String() string      { return string(v) }
func (v LeaseID) String() string       { return string(v) }
func (v OperationID) String() string   { return string(v) }
func (v RequestID) String() string     { return string(v) }
func (v ReleaseID) String() string     { return string(v) }
func (v ActorID) String() string       { return string(v) }

type Revision uint64
type Version uint64

func (r Revision) Next() (Revision, error) {
	if r == ^Revision(0) {
		return 0, errors.New("revision overflow")
	}
	return r + 1, nil
}
func (v Version) Next() (Version, error) {
	if v == ^Version(0) {
		return 0, errors.New("version overflow")
	}
	return v + 1, nil
}

type PriorityAssessment struct {
	Version         Version
	ValueScore      int
	UrgencyScore    int
	RiskScore       int
	DependencyScore int
	LearningScore   int
	ResourceCost    int
	StarvationRisk  int
	Executable      bool
	Reason          string
	ReevaluateWhen  string
}

var (
	ErrInvalidTransition  = errors.New("invalid domain state transition")
	ErrStaleVersion       = errors.New("stale domain version")
	ErrLeaseNotOwned      = errors.New("lease is not owned by execution")
	ErrLeaseExpired       = errors.New("lease is expired")
	ErrStaleFence         = errors.New("stale fencing token")
	ErrControlDenied      = errors.New("control policy denies operation")
	ErrEvidenceIncomplete = errors.New("release evidence is incomplete")
	ErrBudgetExhausted    = errors.New("retry budget exhausted")
)

type RequirementStatus string

const (
	RequirementCaptured   RequirementStatus = "captured"
	RequirementFraming    RequirementStatus = "framing"
	RequirementReady      RequirementStatus = "ready"
	RequirementActive     RequirementStatus = "active"
	RequirementWaiting    RequirementStatus = "waiting"
	RequirementNeedsInput RequirementStatus = "needs-input"
	RequirementPaused     RequirementStatus = "paused"
	RequirementRecovering RequirementStatus = "recovering"
	RequirementEvaluating RequirementStatus = "evaluating"
	RequirementCompleted  RequirementStatus = "completed"
	RequirementCancelled  RequirementStatus = "cancelled"
)

// Verbose aliases make the public enum names easy to discover while keeping
// the serialized values stable.
const (
	RequirementStatusCaptured   = RequirementCaptured
	RequirementStatusFraming    = RequirementFraming
	RequirementStatusReady      = RequirementReady
	RequirementStatusActive     = RequirementActive
	RequirementStatusWaiting    = RequirementWaiting
	RequirementStatusNeedsInput = RequirementNeedsInput
	RequirementStatusPaused     = RequirementPaused
	RequirementStatusRecovering = RequirementRecovering
	RequirementStatusEvaluating = RequirementEvaluating
	RequirementStatusCompleted  = RequirementCompleted
	RequirementStatusCancelled  = RequirementCancelled
)

type IncrementStatus string

const (
	IncrementProposed          IncrementStatus = "proposed"
	IncrementReady             IncrementStatus = "ready"
	IncrementLeased            IncrementStatus = "leased"
	IncrementExecuting         IncrementStatus = "executing"
	IncrementVerifying         IncrementStatus = "verifying"
	IncrementIntegrated        IncrementStatus = "integrated"
	IncrementPreviewValidating IncrementStatus = "preview-validating"
	IncrementFailed            IncrementStatus = "failed"
	IncrementAccepted          IncrementStatus = "accepted"
	IncrementReleased          IncrementStatus = "released"
	IncrementAbandoned         IncrementStatus = "abandoned"
	IncrementCancelled         IncrementStatus = "cancelled"
)

const (
	IncrementStatusProposed          = IncrementProposed
	IncrementStatusReady             = IncrementReady
	IncrementStatusLeased            = IncrementLeased
	IncrementStatusExecuting         = IncrementExecuting
	IncrementStatusVerifying         = IncrementVerifying
	IncrementStatusIntegrated        = IncrementIntegrated
	IncrementStatusPreviewValidating = IncrementPreviewValidating
	IncrementStatusFailed            = IncrementFailed
	IncrementStatusAccepted          = IncrementAccepted
	IncrementStatusReleased          = IncrementReleased
	IncrementStatusAbandoned         = IncrementAbandoned
	IncrementStatusCancelled         = IncrementCancelled
)

type ExecutionStatus string

const (
	ExecutionOffered       ExecutionStatus = "offered"
	ExecutionLeased        ExecutionStatus = "leased"
	ExecutionStarting      ExecutionStatus = "starting"
	ExecutionRunning       ExecutionStatus = "running"
	ExecutionCheckpointing ExecutionStatus = "checkpointing"
	ExecutionSucceeded     ExecutionStatus = "succeeded"
	ExecutionFailed        ExecutionStatus = "failed"
	ExecutionTerminated    ExecutionStatus = "terminated"
	ExecutionLost          ExecutionStatus = "lost"
)

type LeaseStatus string

const (
	LeaseActive   LeaseStatus = "active"
	LeaseExpired  LeaseStatus = "expired"
	LeaseRevoked  LeaseStatus = "revoked"
	LeaseReleased LeaseStatus = "released"
)

type Requirement struct {
	ID             RequirementID
	Status         RequirementStatus
	Version        Version
	Increments     []IncrementID
	StableSnapshot StableReleaseSnapshot
	// RequestedBy records who caused this Requirement to be captured: an
	// owner acting through an authenticated session, or the Loop acting on
	// its own. It is a value addition only; every RequirementCommand still
	// carries state forward with `next := current`, so no transition
	// function inspects or rewrites it. A record captured before this field
	// existed simply carries the zero value (ActorType == ""), which
	// Validate treats as legitimate and unknown rather than invalid: past
	// records are never retrofitted.
	RequestedBy RequestedBy
}

// ActorType distinguishes an owner-originated request from a decision the
// Loop made on its own. It carries no permission semantics: it does not
// grant or restrict what an actor may do, only records which of the two
// originated a given Requirement intake or Control Intent.
type ActorType string

const (
	ActorTypeOwner ActorType = "owner"
	ActorTypeLoop  ActorType = "loop"
)

func validActorType(t ActorType) bool {
	switch t {
	case ActorTypeOwner, ActorTypeLoop:
		return true
	}
	return false
}

// RequestedBy is an identity reference, never a credential: Subject is an
// opaque label (an owner session subject, an IAP subject, or a Loop
// component identifier) carried for attribution only. It is never
// interpreted, authenticated, or authorized by the domain package.
type RequestedBy struct {
	ActorType ActorType `json:"actor_type,omitempty"`
	Subject   string    `json:"subject,omitempty"`
}

// Recorded reports whether this value was actually populated at capture
// time, distinguishing it from a legacy record that predates this field.
func (r RequestedBy) Recorded() bool { return r.ActorType != "" || r.Subject != "" }

type StableReleaseSnapshot struct {
	ReleaseID      ReleaseID
	ReleaseVersion Version
	BundleDigest   string
	EvidenceDigest string
}

type Increment struct {
	ID                    IncrementID
	RequirementID         RequirementID
	Status                IncrementStatus
	Version               Version
	Retry                 RetryBudget
	PreviewCandidateID    ReleaseID
	PreviewEvidenceDigest string
}

type Execution struct {
	ID              ExecutionID
	IncrementID     IncrementID
	RunnerID        RunnerID
	LeaseID         LeaseID
	FencingToken    FencingToken
	ControlRevision Revision
	Status          ExecutionStatus
	Version         Version
}

type Lease struct {
	ID              LeaseID
	IncrementID     IncrementID
	ExecutionID     ExecutionID
	RunnerID        RunnerID
	FencingToken    FencingToken
	ControlRevision Revision
	IssuedAt        time.Time
	ExpiresAt       time.Time
	Status          LeaseStatus
	Version         Version
}

func Validate(v any) error {
	switch value := v.(type) {
	case *Requirement:
		if value == nil {
			return errors.New("nil requirement")
		}
		return Validate(*value)
	case *Increment:
		if value == nil {
			return errors.New("nil increment")
		}
		return Validate(*value)
	case *Execution:
		if value == nil {
			return errors.New("nil execution")
		}
		return Validate(*value)
	case *Lease:
		if value == nil {
			return errors.New("nil lease")
		}
		return Validate(*value)
	case Requirement:
		if _, err := NewRequirementID(value.ID.String()); err != nil {
			return err
		}
		if !validRequirementStatus(value.Status) {
			return fmt.Errorf("unknown requirement status %q", value.Status)
		}
		if value.Status == RequirementCompleted {
			if value.StableSnapshot.ReleaseID == "" || value.StableSnapshot.BundleDigest == "" || value.StableSnapshot.EvidenceDigest == "" {
				return ErrEvidenceIncomplete
			}
		}
		// A zero RequestedBy (ActorType == "") is a legacy record that
		// predates this field and remains valid; any non-empty ActorType
		// must be one of the closed enum values.
		if value.RequestedBy.ActorType != "" && !validActorType(value.RequestedBy.ActorType) {
			return fmt.Errorf("unknown requested_by actor_type %q", value.RequestedBy.ActorType)
		}
	case Increment:
		if _, err := NewIncrementID(value.ID.String()); err != nil {
			return err
		}
		if _, err := NewRequirementID(value.RequirementID.String()); err != nil {
			return err
		}
		if !validIncrementStatus(value.Status) {
			return fmt.Errorf("unknown increment status %q", value.Status)
		}
		if value.Status == IncrementAccepted && (value.PreviewCandidateID == "" || value.PreviewEvidenceDigest == "") {
			return ErrEvidenceIncomplete
		}
	case Execution:
		if _, err := NewExecutionID(value.ID.String()); err != nil {
			return err
		}
		if _, err := NewIncrementID(value.IncrementID.String()); err != nil {
			return err
		}
		if _, err := NewRunnerID(value.RunnerID.String()); err != nil {
			return err
		}
		if value.Status == ExecutionLeased || value.Status == ExecutionRunning {
			if _, err := NewLeaseID(value.LeaseID.String()); err != nil {
				return err
			}
		}
	case Lease:
		if _, err := NewLeaseID(value.ID.String()); err != nil {
			return err
		}
		if _, err := NewExecutionID(value.ExecutionID.String()); err != nil {
			return err
		}
		if _, err := NewIncrementID(value.IncrementID.String()); err != nil {
			return err
		}
		if _, err := NewRunnerID(value.RunnerID.String()); err != nil {
			return err
		}
		if !value.IssuedAt.IsZero() && !value.ExpiresAt.IsZero() && !value.ExpiresAt.After(value.IssuedAt) {
			return errors.New("lease expiry must be after issue time")
		}
		if !validLeaseStatus(value.Status) {
			return fmt.Errorf("unknown lease status %q", value.Status)
		}
	default:
		return fmt.Errorf("unsupported domain value %T", v)
	}
	return nil
}

// Decide is the generic pure entry point used by adapters and model tests.
// Concrete helpers below retain static types for normal callers.
func Decide[T any](state T, command any) (T, error) {
	var zero T
	switch current := any(state).(type) {
	case Requirement:
		cmd, ok := command.(RequirementCommand)
		if !ok {
			return zero, fmt.Errorf("requirement expects RequirementCommand")
		}
		next, err := DecideRequirement(current, cmd)
		return any(next).(T), err
	case Increment:
		cmd, ok := command.(IncrementCommand)
		if !ok {
			return zero, fmt.Errorf("increment expects IncrementCommand")
		}
		next, err := DecideIncrement(current, cmd)
		return any(next).(T), err
	default:
		return zero, fmt.Errorf("unsupported state %T", state)
	}
}

func validRequirementStatus(s RequirementStatus) bool {
	switch s {
	case RequirementCaptured, RequirementFraming, RequirementReady, RequirementActive, RequirementWaiting, RequirementNeedsInput, RequirementPaused, RequirementRecovering, RequirementEvaluating, RequirementCompleted, RequirementCancelled:
		return true
	}
	return false
}
func validIncrementStatus(s IncrementStatus) bool {
	switch s {
	case IncrementProposed, IncrementReady, IncrementLeased, IncrementExecuting, IncrementVerifying, IncrementIntegrated, IncrementPreviewValidating, IncrementFailed, IncrementAccepted, IncrementReleased, IncrementAbandoned, IncrementCancelled:
		return true
	}
	return false
}
func validLeaseStatus(s LeaseStatus) bool {
	switch s {
	case LeaseActive, LeaseExpired, LeaseRevoked, LeaseReleased:
		return true
	}
	return false
}

type RequirementCommandKind string

const (
	RequirementStartFraming RequirementCommandKind = "start-framing"
	RequirementReadyCommand RequirementCommandKind = "ready"
	RequirementStart        RequirementCommandKind = "start"
	RequirementWait         RequirementCommandKind = "wait"
	RequirementNeedInput    RequirementCommandKind = "needs-input"
	RequirementRecover      RequirementCommandKind = "recover"
	RequirementEvaluate     RequirementCommandKind = "evaluate"
	RequirementPause        RequirementCommandKind = "pause"
	RequirementCancel       RequirementCommandKind = "cancel"
)

type RequirementCommand struct {
	Kind            RequirementCommandKind
	Actor           ActorID
	At              time.Time
	ExpectedVersion Version
}

func DecideRequirement(current Requirement, command RequirementCommand) (Requirement, error) {
	if err := Validate(current); err != nil {
		return current, err
	}
	if command.Actor == "" || command.At.IsZero() {
		return current, errors.New("actor and explicit timestamp are required")
	}
	if current.Version != command.ExpectedVersion {
		return current, ErrStaleVersion
	}
	next := current
	allowed := func(statuses ...RequirementStatus) bool {
		for _, s := range statuses {
			if current.Status == s {
				return true
			}
		}
		return false
	}
	switch command.Kind {
	case RequirementStartFraming:
		if !allowed(RequirementCaptured) {
			return current, ErrInvalidTransition
		}
		next.Status = RequirementFraming
	case RequirementReadyCommand:
		if !allowed(RequirementFraming, RequirementWaiting, RequirementNeedsInput) {
			return current, ErrInvalidTransition
		}
		next.Status = RequirementReady
	case RequirementStart:
		if !allowed(RequirementReady, RequirementRecovering, RequirementWaiting) {
			return current, ErrInvalidTransition
		}
		next.Status = RequirementActive
	case RequirementWait:
		if !allowed(RequirementReady, RequirementActive, RequirementRecovering) {
			return current, ErrInvalidTransition
		}
		next.Status = RequirementWaiting
	case RequirementNeedInput:
		if !allowed(RequirementFraming, RequirementActive, RequirementEvaluating) {
			return current, ErrInvalidTransition
		}
		next.Status = RequirementNeedsInput
	case RequirementRecover:
		if !allowed(RequirementActive, RequirementWaiting) {
			return current, ErrInvalidTransition
		}
		next.Status = RequirementRecovering
	case RequirementEvaluate:
		if !allowed(RequirementActive) {
			return current, ErrInvalidTransition
		}
		next.Status = RequirementEvaluating
	case RequirementPause:
		if !allowed(RequirementReady, RequirementActive, RequirementWaiting, RequirementRecovering) {
			return current, ErrInvalidTransition
		}
		next.Status = RequirementPaused
	case RequirementCancel:
		if allowed(RequirementCompleted, RequirementCancelled) {
			return current, ErrInvalidTransition
		}
		next.Status = RequirementCancelled
	default:
		return current, fmt.Errorf("unknown requirement command %q", command.Kind)
	}
	next.Version++
	return next, nil
}

// CompleteRequirementFromRelease binds completion to the immutable stable
// release snapshot rather than to a caller-supplied boolean claim.
func CompleteRequirementFromRelease(current Requirement, proof StableReleaseProof, actor ActorID, at time.Time) (Requirement, error) {
	if current.Status != RequirementEvaluating {
		return current, ErrInvalidTransition
	}
	if !proof.valid() {
		return current, ErrEvidenceIncomplete
	}
	if actor == "" || at.IsZero() {
		return current, errors.New("actor and explicit timestamp are required")
	}
	next := current
	next.Status = RequirementCompleted
	next.Version++
	next.StableSnapshot = StableReleaseSnapshot{ReleaseID: proof.data.candidateID, ReleaseVersion: proof.data.version, BundleDigest: proof.data.bundleDigest, EvidenceDigest: proof.data.evidenceDigest}
	return next, nil
}

type IncrementCommandKind string

const (
	IncrementPrepare   IncrementCommandKind = "ready"
	IncrementLease     IncrementCommandKind = "lease"
	IncrementExecute   IncrementCommandKind = "execute"
	IncrementVerify    IncrementCommandKind = "verify"
	IncrementIntegrate IncrementCommandKind = "integrate"
	IncrementPreview   IncrementCommandKind = "preview-validate"
	IncrementFail      IncrementCommandKind = "fail"
	IncrementAccept    IncrementCommandKind = "accept"
	IncrementRelease   IncrementCommandKind = "release"
	IncrementAbandon   IncrementCommandKind = "abandon"
	IncrementCancel    IncrementCommandKind = "cancel"
	IncrementRecover   IncrementCommandKind = "recover"
)

type IncrementCommand struct {
	Kind                  IncrementCommandKind
	Actor                 ActorID
	At                    time.Time
	ExpectedVersion       Version
	PreviewCandidateID    ReleaseID
	PreviewEvidenceDigest string
}

func DecideIncrement(current Increment, command IncrementCommand) (Increment, error) {
	if err := Validate(current); err != nil {
		return current, err
	}
	if command.Actor == "" || command.At.IsZero() {
		return current, errors.New("actor and explicit timestamp are required")
	}
	if current.Version != command.ExpectedVersion {
		return current, ErrStaleVersion
	}
	next := current
	ok := func(s ...IncrementStatus) bool {
		for _, x := range s {
			if current.Status == x {
				return true
			}
		}
		return false
	}
	switch command.Kind {
	case IncrementPrepare:
		if !ok(IncrementProposed) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementReady
	case IncrementLease:
		if !ok(IncrementReady) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementLeased
	case IncrementExecute:
		if !ok(IncrementLeased, IncrementPreviewValidating) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementExecuting
	case IncrementVerify:
		if !ok(IncrementExecuting) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementVerifying
	case IncrementIntegrate:
		if !ok(IncrementVerifying) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementIntegrated
	case IncrementPreview:
		if !ok(IncrementIntegrated) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementPreviewValidating
		if command.PreviewCandidateID == "" || command.PreviewEvidenceDigest == "" {
			return current, ErrEvidenceIncomplete
		}
		next.PreviewCandidateID = command.PreviewCandidateID
		next.PreviewEvidenceDigest = command.PreviewEvidenceDigest
	case IncrementFail:
		if !ok(IncrementExecuting, IncrementVerifying, IncrementPreviewValidating) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementFailed
	case IncrementAccept:
		if !ok(IncrementPreviewValidating) {
			return current, ErrInvalidTransition
		}
		if command.PreviewCandidateID == "" || command.PreviewEvidenceDigest == "" || command.PreviewCandidateID != current.PreviewCandidateID || command.PreviewEvidenceDigest != current.PreviewEvidenceDigest {
			return current, ErrEvidenceIncomplete
		}
		next.Status = IncrementAccepted
	case IncrementRelease:
		if !ok(IncrementAccepted) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementReleased
	case IncrementAbandon:
		if ok(IncrementReleased, IncrementAbandoned, IncrementCancelled) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementAbandoned
	case IncrementCancel:
		if ok(IncrementReleased, IncrementAbandoned, IncrementCancelled) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementCancelled
	case IncrementRecover:
		if !ok(IncrementLeased, IncrementExecuting, IncrementFailed) {
			return current, ErrInvalidTransition
		}
		next.Status = IncrementReady
	default:
		return current, fmt.Errorf("unknown increment command %q", command.Kind)
	}
	next.Version++
	return next, nil
}

// Ensure deterministic handling of evidence/capability collections.
func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
