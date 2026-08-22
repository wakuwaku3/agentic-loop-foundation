package domain

import (
	"errors"
	"time"
)

type LeaseRequest struct {
	ID                   LeaseID
	ExecutionID          ExecutionID
	IncrementID          IncrementID
	RunnerID             RunnerID
	PreviousFencingToken FencingToken
	ControlRevision      Revision
	IssuedAt             time.Time
	ExpiresAt            time.Time
}

// IssueLease is pure: the caller supplies the previously authoritative fence
// and explicit timestamps. A lease always owns exactly one execution/runner.
func IssueLease(request LeaseRequest) (Lease, error) {
	if _, err := NewLeaseID(request.ID.String()); err != nil {
		return Lease{}, err
	}
	if _, err := NewExecutionID(request.ExecutionID.String()); err != nil {
		return Lease{}, err
	}
	if _, err := NewIncrementID(request.IncrementID.String()); err != nil {
		return Lease{}, err
	}
	if _, err := NewRunnerID(request.RunnerID.String()); err != nil {
		return Lease{}, err
	}
	if request.IssuedAt.IsZero() || request.ExpiresAt.IsZero() || !request.ExpiresAt.After(request.IssuedAt) {
		return Lease{}, errors.New("lease requires explicit positive lifetime")
	}
	next, err := request.PreviousFencingToken.Next()
	if err != nil {
		return Lease{}, err
	}
	return Lease{ID: request.ID, ExecutionID: request.ExecutionID, IncrementID: request.IncrementID, RunnerID: request.RunnerID, FencingToken: next, ControlRevision: request.ControlRevision, IssuedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt, Status: LeaseActive, Version: 1}, nil
}

// RotateLease closes the previous owner and issues the sole next owner. It is
// the in-memory equivalent of the transaction boundary used by a store.
func RotateLease(previous Lease, request LeaseRequest) (Lease, Lease, error) {
	if previous.Status == LeaseActive {
		previous.Status = LeaseRevoked
		previous.Version++
	}
	request.PreviousFencingToken = previous.FencingToken
	next, err := IssueLease(request)
	return previous, next, err
}

func (l Lease) ActiveAt(at time.Time) bool {
	return l.Status == LeaseActive && !at.IsZero() && at.Before(l.ExpiresAt) && !at.Before(l.IssuedAt)
}

func RenewLease(current Lease, executionID ExecutionID, runnerID RunnerID, token FencingToken, now, expiresAt time.Time, control EffectiveControlResult) (Lease, error) {
	if current.ExecutionID != executionID || current.RunnerID != runnerID {
		return current, ErrLeaseNotOwned
	}
	if token != current.FencingToken {
		return current, ErrStaleFence
	}
	if !current.ActiveAt(now) {
		return current, ErrLeaseExpired
	}
	if control.Mode == "" {
		control.Mode = ControlAllow
	}
	if control.Mode != ControlAllow || (control.Found && control.Revision != current.ControlRevision) || (!control.Found && current.ControlRevision != 0) {
		return current, ErrControlDenied
	}
	if !expiresAt.After(now) {
		return current, errors.New("renewal expiry must be after now")
	}
	next := current
	next.ExpiresAt = expiresAt
	next.Version++
	return next, nil
}

func ExpireLease(current Lease, at time.Time) (Lease, error) {
	if current.Status != LeaseActive {
		return current, errors.New("lease is not active")
	}
	if at.IsZero() || at.Before(current.ExpiresAt) {
		return current, errors.New("lease has not expired")
	}
	next := current
	next.Status = LeaseExpired
	next.Version++
	return next, nil
}

type ExecutionResult struct {
	ExecutionID     ExecutionID
	LeaseID         LeaseID
	FencingToken    FencingToken
	ControlRevision Revision
	At              time.Time
	Succeeded       bool
}

func ValidateExecutionResult(execution Execution, lease Lease, result ExecutionResult, control EffectiveControlResult) error {
	if result.ExecutionID != execution.ID || result.LeaseID != execution.LeaseID || result.LeaseID != lease.ID || execution.IncrementID != lease.IncrementID || execution.RunnerID != lease.RunnerID {
		return ErrLeaseNotOwned
	}
	if result.FencingToken != execution.FencingToken || result.FencingToken != lease.FencingToken {
		return ErrStaleFence
	}
	if result.ControlRevision != execution.ControlRevision || result.ControlRevision != lease.ControlRevision || (control.Found && result.ControlRevision != control.Revision) || (!control.Found && result.ControlRevision != 0) {
		return ErrControlDenied
	}
	if control.Mode == ControlImmediateStop || control.Mode == ControlEmergencyStop || control.Mode == ControlCancel || control.Mode == ControlGracefulStop {
		return ErrControlDenied
	}
	if !lease.ActiveAt(result.At) {
		return ErrLeaseExpired
	}
	if execution.Status != ExecutionRunning && execution.Status != ExecutionCheckpointing {
		return ErrInvalidTransition
	}
	if lease.Status != LeaseActive {
		return ErrLeaseExpired
	}
	if result.ControlRevision > execution.ControlRevision {
		return ErrControlDenied
	}
	if execution.Status == ExecutionLost || execution.Status == ExecutionTerminated || execution.Status == ExecutionFailed || execution.Status == ExecutionSucceeded {
		return errors.New("execution cannot accept another result")
	}
	return nil
}

func AcceptExecutionResult(execution Execution, lease Lease, result ExecutionResult, control EffectiveControlResult) (Execution, error) {
	if err := ValidateExecutionResult(execution, lease, result, control); err != nil {
		return execution, err
	}
	next := execution
	next.Status = ExecutionSucceeded
	if !result.Succeeded {
		next.Status = ExecutionFailed
	}
	next.Version++
	return next, nil
}

func StartExecution(execution Execution, lease Lease, at time.Time, control EffectiveControlResult) (Execution, error) {
	if execution.LeaseID != lease.ID || execution.FencingToken != lease.FencingToken || execution.IncrementID != lease.IncrementID || execution.RunnerID != lease.RunnerID || execution.ControlRevision != lease.ControlRevision || !lease.ActiveAt(at) || lease.Status != LeaseActive {
		return execution, ErrStaleFence
	}
	if control.Mode == "" {
		control.Mode = ControlAllow
	}
	if control.Mode != ControlAllow || (control.Found && control.Revision != execution.ControlRevision) || (!control.Found && execution.ControlRevision != 0) {
		return execution, ErrControlDenied
	}
	if execution.Status != ExecutionOffered && execution.Status != ExecutionLeased && execution.Status != ExecutionStarting {
		return execution, ErrInvalidTransition
	}
	next := execution
	next.Status = ExecutionRunning
	next.Version++
	return next, nil
}
