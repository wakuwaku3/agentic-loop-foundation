package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
)

var (
	ErrNotFound         = errors.New("application record not found")
	ErrDuplicateRequest = errors.New("request id has already been used for another operation")
	// ErrRepositoryNotAvailable refuses an operation that names a Repository
	// that exists but is no longer one the Installation operates on (its
	// registration was rolled back by a retire). It is a conflict with
	// existing state rather than a malformed request.
	ErrRepositoryNotAvailable = errors.New("repository is registered no longer and cannot take new work")
)

func requireRequest(requestID string) error {
	if requestID == "" {
		return errors.New("request_id is required")
	}
	return nil
}

func sameTarget(requested, canonical domain.ControlTarget) bool {
	if requested.InstallationID != "" && requested.InstallationID != canonical.InstallationID {
		return false
	}
	if requested.RepositoryID != "" && requested.RepositoryID != canonical.RepositoryID {
		return false
	}
	if requested.RequirementID != "" && requested.RequirementID != canonical.RequirementID {
		return false
	}
	if requested.IncrementID != "" && requested.IncrementID != canonical.IncrementID {
		return false
	}
	if requested.RunnerID != "" && requested.RunnerID != canonical.RunnerID {
		return false
	}
	if requested.Channel != "" && requested.Channel != canonical.Channel {
		return false
	}
	return true
}

// executionAlreadyTerminal reports whether an Execution has already reached
// an outcome that domain.MarkExecutionLost refuses to move away from
// (ErrInvalidTransition). Used by Claim's reclaim branch (dp-v2-058 d5) to
// skip terminating a superseded Execution that is already terminal, so an
// unconditional MarkExecutionLost call can never turn a reclaim into a
// failed Claim. This mirrors internal/reconciler's own definition of the
// same set (executionIsTerminal in reconciler.go), duplicated here rather
// than imported because internal/reconciler already imports
// internal/application and a reverse import would cycle.
func executionAlreadyTerminal(status domain.ExecutionStatus) bool {
	switch status {
	case domain.ExecutionSucceeded, domain.ExecutionFailed, domain.ExecutionTerminated, domain.ExecutionLost:
		return true
	}
	return false
}

func canonicalizeTarget(requested, canonical domain.ControlTarget) domain.ControlTarget {
	if canonical.InstallationID == "" {
		canonical.InstallationID = requested.InstallationID
	}
	if canonical.RepositoryID == "" {
		canonical.RepositoryID = requested.RepositoryID
	}
	if canonical.RequirementID == "" {
		canonical.RequirementID = requested.RequirementID
	}
	if canonical.IncrementID == "" {
		canonical.IncrementID = requested.IncrementID
	}
	if canonical.RunnerID == "" {
		canonical.RunnerID = requested.RunnerID
	}
	if canonical.Channel == "" {
		canonical.Channel = requested.Channel
	}
	return canonical
}

func validControlMode(mode domain.ControlMode) bool {
	switch mode {
	case domain.ControlAllow, domain.ControlPauseIntake, domain.ControlPauseClaim, domain.ControlGracefulStop, domain.ControlImmediateStop, domain.ControlEmergencyStop, domain.ControlCancel:
		return true
	}
	return false
}
func validControlScope(scope domain.ControlScope) bool {
	if scope.Value == "" {
		return false
	}
	switch scope.Kind {
	case domain.ScopeInstallation, domain.ScopeRepository, domain.ScopeRequirement, domain.ScopeIncrement, domain.ScopeRunner, domain.ScopeChannel:
		return true
	}
	return false
}

type Service struct {
	tx     Transactor
	clock  Clock
	ids    IDGenerator
	config ServiceConfig
}

type authorityTimeKey struct{}

func withAuthorityTime(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, authorityTimeKey{}, at)
}

func (s *Service) transact(ctx context.Context, fn func(UnitOfWork) error) error {
	// Capture once outside the transaction callback. Firestore may retry the
	// callback; authority time must not change between attempts.
	at := s.clock.Now()
	if at.IsZero() {
		return errors.New("clock returned zero time")
	}
	return s.tx.Transact(withAuthorityTime(ctx, at), func(u UnitOfWork) error {
		if err := u.ReserveQuota(ctx, fmt.Sprintf("read:%s:%d", s.config.InstallationID, at.UnixNano()), at, quota.ReadTransactionUsage); err != nil {
			return err
		}
		return fn(u)
	})
}

// mutate reserves the conservative mutation-boundary maximum before the
// callback can stage any aggregate, event, or outbox change. Firestore retries
// the callback atomically, so an over-budget request cannot leave a partial
// mutation.
func (s *Service) mutate(ctx context.Context, key string, fn func(UnitOfWork) error) error {
	at := s.clock.Now()
	if at.IsZero() {
		return errors.New("clock returned zero time")
	}
	return s.tx.Transact(withAuthorityTime(ctx, at), func(u UnitOfWork) error {
		if err := u.ReserveQuota(ctx, "mutation:"+s.config.InstallationID+":"+key, at, quota.MutationUsage); err != nil {
			return err
		}
		return fn(u)
	})
}

// ServiceConfig carries process-level authority only. It deliberately holds
// no repository id (V2-071 A6, dp-v2-071 d14): an Installation registers many
// Repositories, so a single per-process repository id would make a
// domain.ScopeRepository Control Intent match the wrong Repository instead of
// none. Every ControlTarget's RepositoryID is resolved from the aggregate
// graph inside the transaction that builds it -- from the canonical target's
// stored value, or from the Requirement-to-Repository link -- and never from
// configuration, an environment variable or a flag.
type ServiceConfig struct {
	InstallationID string
	LeaseTTL       time.Duration
}

func NewService(tx Transactor, clock Clock, ids IDGenerator) (*Service, error) {
	return NewServiceWithConfig(tx, clock, ids, ServiceConfig{LeaseTTL: time.Minute})
}
func NewServiceWithConfig(tx Transactor, clock Clock, ids IDGenerator, config ServiceConfig) (*Service, error) {
	if config.InstallationID == "" {
		return nil, errors.New("installation authority is required")
	}
	if config.LeaseTTL <= 0 {
		config.LeaseTTL = time.Minute
	}
	return &Service{tx: tx, clock: clock, ids: ids, config: config}, nil
}

// CaptureRequest gains the optional RepositoryID (V2-071 A4). When it is
// supplied, Capture writes the write-once Requirement-to-Repository link in
// its own transaction and gates the intake on a ControlTarget that names that
// Repository; when it is empty, Capture behaves exactly as before and writes
// no link.
type CaptureRequest struct {
	RequestID, RequirementID, Text string
	RepositoryID                   string
}
type CaptureResponse struct {
	RequirementID string             `json:"requirement_id"`
	Version       domain.Version     `json:"version"`
	RequestedBy   domain.RequestedBy `json:"requested_by"`
	// RepositoryID is present only when this Capture actually wrote a link.
	// An unlinked Requirement omits the field entirely rather than reporting
	// an empty string that could be read as "no repository was measured".
	RepositoryID string `json:"repository_id,omitempty"`
}
type LifecycleResponse struct {
	Accepted            bool                        `json:"accepted"`
	AppliedRevision     domain.Revision             `json:"applied_revision,omitempty"`
	LatestRevision      domain.Revision             `json:"latest_revision,omitempty"`
	LatestEffectiveAt   time.Time                   `json:"latest_effective_at,omitempty"`
	ProcessObservations []domain.ProcessObservation `json:"process_observations,omitempty"`
	// LatestMode is additive (dp-v2-019 d4, acceptance A10): the effective
	// domain.ControlMode as of LatestRevision, letting a runner-side
	// ControlAgent choose checkpoint (graceful stop) over terminate
	// (immediate/emergency stop/cancel) instead of guessing. It is carried
	// only on the response of heartbeat and of checkpoint.
	LatestMode domain.ControlMode `json:"latest_mode,omitempty"`
}
type RenewRequest struct {
	RequestID, LeaseID   string
	ExpectedLeaseVersion domain.Version
	FencingToken         domain.FencingToken
	ControlRevision      domain.Revision
}
type RenewResponse struct {
	LeaseID   string         `json:"lease_id"`
	ExpiresAt time.Time      `json:"expires_at"`
	Version   domain.Version `json:"version"`
}

func (s *Service) Renew(ctx context.Context, req RenewRequest) (out RenewResponse, err error) {
	_, actor, runner, e := runnerCaller(ctx)
	if e != nil {
		return out, e
	}
	if e = requireRequest(req.RequestID); e != nil {
		return out, e
	}
	now := s.clock.Now()
	if now.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	fp, e := requestFingerprint("renew", req)
	if e != nil {
		return out, e
	}
	eid, e := s.ids.Next("event")
	if e != nil {
		return out, e
	}
	oid, e := s.ids.Next("operation")
	if e != nil {
		return out, e
	}
	xid, e := s.ids.Next("outbox")
	if e != nil {
		return out, e
	}
	err = s.mutate(ctx, "renew:"+req.RequestID, func(u UnitOfWork) error {
		if p, ok, x := u.Idempotency(ctx, req.RequestID, "renew"); x != nil {
			return x
		} else if ok {
			if p.Fingerprint != fp {
				return ErrIdempotencyConflict
			}
			return restoreResponse(p, &out)
		}
		lease, ok, x := u.Lease(ctx, req.LeaseID)
		if x != nil {
			return x
		}
		if !ok {
			return ErrNotFound
		}
		if lease.Version != req.ExpectedLeaseVersion || lease.RunnerID != runner {
			return domain.ErrStaleVersion
		}
		target, found, x := u.CanonicalTarget(ctx, lease.IncrementID.String(), runner.String())
		if x != nil {
			return x
		}
		if !found {
			target = domain.ControlTarget{IncrementID: lease.IncrementID, RunnerID: runner}
		}
		controls, x := u.Controls(ctx)
		if x != nil {
			return x
		}
		next, x := domain.RenewLease(lease, lease.ExecutionID, runner, req.FencingToken, now, now.Add(s.config.LeaseTTL), domain.EffectiveControl(controls, target))
		if x != nil {
			return x
		}
		if x = u.SaveLease(ctx, next, lease.Version); x != nil {
			return x
		}
		out = RenewResponse{LeaseID: req.LeaseID, ExpiresAt: next.ExpiresAt, Version: next.Version}
		effect, x := s.effectOutbox(ctx, u, xid, oid, req.RequestID, domain.PermitProcess, target, next.Version, next.FencingToken, req.ControlRevision, "lease-renewed", req.LeaseID)
		if x != nil {
			return x
		}
		return s.record(ctx, u, eid, oid, fp, req.RequestID, "renew", "lease", req.LeaseID, next.Version, "lease.renewed", actor.String(), effect, out)
	})
	return out, err
}

type StartRequest struct {
	RequestID, ExecutionID   string
	ExpectedExecutionVersion domain.Version
	ControlRevision          domain.Revision
}
type StartResponse struct {
	ExecutionID string                 `json:"execution_id"`
	Status      domain.ExecutionStatus `json:"status"`
	Version     domain.Version         `json:"version"`
}

func (s *Service) Start(ctx context.Context, req StartRequest) (out StartResponse, err error) {
	_, actor, runner, e := runnerCaller(ctx)
	if e != nil {
		return out, e
	}
	if e = requireRequest(req.RequestID); e != nil {
		return out, e
	}
	now := s.clock.Now()
	if now.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	fp, e := requestFingerprint("start", req)
	if e != nil {
		return out, e
	}
	eid, e := s.ids.Next("event")
	if e != nil {
		return out, e
	}
	oid, e := s.ids.Next("operation")
	if e != nil {
		return out, e
	}
	err = s.mutate(ctx, "start:"+req.RequestID, func(u UnitOfWork) error {
		if p, ok, x := u.Idempotency(ctx, req.RequestID, "start"); x != nil {
			return x
		} else if ok {
			if p.Fingerprint != fp {
				return ErrIdempotencyConflict
			}
			return restoreResponse(p, &out)
		}
		exec, ok, x := u.Execution(ctx, req.ExecutionID)
		if x != nil {
			return x
		}
		if !ok {
			return ErrNotFound
		}
		if exec.Version != req.ExpectedExecutionVersion || exec.RunnerID != runner {
			return domain.ErrStaleVersion
		}
		lease, ok, x := u.Lease(ctx, exec.LeaseID.String())
		if x != nil {
			return x
		}
		if !ok {
			return ErrNotFound
		}
		if exec.LeaseID != lease.ID || lease.ExecutionID != exec.ID || lease.RunnerID != runner {
			return domain.ErrLeaseNotOwned
		}
		if exec.ControlRevision != req.ControlRevision {
			return domain.ErrControlDenied
		}
		target, found, x := u.CanonicalTarget(ctx, exec.IncrementID.String(), runner.String())
		if x != nil {
			return x
		}
		if !found {
			target = domain.ControlTarget{IncrementID: exec.IncrementID, RunnerID: runner}
		}
		controls, x := u.Controls(ctx)
		if x != nil {
			return x
		}
		effective := domain.EffectiveControl(controls, target)
		if _, x = domain.Permit(effective, domain.PermitRequest{Kind: domain.PermitProcess, Target: target, ControlRevision: req.ControlRevision, FencingToken: exec.FencingToken, ExpectedFencingToken: lease.FencingToken, Resource: req.ExecutionID}); x != nil {
			return x
		}
		next, x := domain.StartExecution(exec, lease, now, effective)
		if x != nil {
			return x
		}
		if x = u.SaveExecution(ctx, next, exec.Version); x != nil {
			return x
		}
		out = StartResponse{ExecutionID: req.ExecutionID, Status: next.Status, Version: next.Version}
		return s.record(ctx, u, eid, oid, fp, req.RequestID, "start", "execution", req.ExecutionID, next.Version, "execution.started", actor.String(), nil, out)
	})
	return out, err
}

type HeartbeatRequest struct {
	RequestID       string
	ControlRevision domain.Revision
	Processes       []domain.ProcessObservation
}
type CheckpointRequest struct {
	RequestID, ExecutionID, LeaseID string
	FencingToken                    domain.FencingToken
	ControlRevision                 domain.Revision
}

func (s *Service) Heartbeat(ctx context.Context, req HeartbeatRequest) (out LifecycleResponse, err error) {
	_, actor, runner, e := runnerCaller(ctx)
	if e != nil {
		return out, e
	}
	if e = requireRequest(req.RequestID); e != nil {
		return out, e
	}
	authorityAt := s.clock.Now()
	if authorityAt.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	fp, e := requestFingerprint("heartbeat", req)
	if e != nil {
		return out, e
	}
	eid, e := s.ids.Next("event")
	if e != nil {
		return out, e
	}
	oid, e := s.ids.Next("operation")
	if e != nil {
		return out, e
	}
	err = s.mutate(ctx, "heartbeat:"+req.RequestID, func(u UnitOfWork) error {
		if p, ok, x := u.Idempotency(ctx, req.RequestID, "heartbeat"); x != nil {
			return x
		} else if ok {
			if p.Fingerprint != fp {
				return ErrIdempotencyConflict
			}
			return restoreResponse(p, &out)
		}
		if len(req.Processes) > 32 {
			return errors.New("at most 32 process observations are allowed")
		}
		for i := range req.Processes {
			if req.Processes[i].ProcessID == "" || len(req.Processes[i].ProcessID) > 128 {
				return errors.New("process observation id is invalid")
			}
			switch req.Processes[i].State {
			case "running", "checkpointed", "terminated", "unknown":
			default:
				return errors.New("process observation state is invalid")
			}
			// Runner clocks and caller-provided timestamps are not authoritative.
			req.Processes[i].At = authorityAt
		}
		if req.ControlRevision != 0 {
			{
				progress, exists, e := u.ControlProgress(ctx, req.ControlRevision)
				if e != nil {
					return e
				}
				if exists && progress.State == domain.ControlRequested {
					if e = domain.AdvanceControlState(progress.State, domain.ControlAcknowledged); e != nil {
						return e
					}
					progress.State = domain.ControlAcknowledged
					progress.AcknowledgedAt = authorityAt
					if e = u.SaveControlProgress(ctx, progress, domain.ControlRequested); e != nil {
						return e
					}
				}
			}
		}
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		// A Heartbeat's target deliberately carries no RepositoryID.
		// HeartbeatRequest names a Runner and nothing else -- no Increment
		// and no Lease -- and a Runner is not repository-bound: one Runner
		// serves every Repository of the Installation. Attaching a
		// repository here would make a repository-scoped stop apply to
		// unrelated work, so the runner-scoped observation stays
		// runner-scoped and TestHeartbeatTargetCarriesNoRepository asserts
		// that a domain.ScopeRepository intent does not match it.
		target := domain.ControlTarget{InstallationID: s.config.InstallationID, RunnerID: runner}
		effective := domain.EffectiveControl(controls, target)
		latest := effective.Revision
		var deadline time.Time
		for _, control := range controls {
			if control.Revision == effective.Revision && control.EffectiveAt.After(deadline) {
				deadline = control.EffectiveAt
			}
		}
		observation := domain.RunnerObservation{RunnerID: runner, Target: target, AppliedRevision: req.ControlRevision, LatestRevision: latest, LatestEffectiveAt: deadline, Reachable: true, Processes: append([]domain.ProcessObservation(nil), req.Processes...), ObservedAt: authorityAt}
		if e = u.SaveRunnerObservation(ctx, observation); e != nil {
			return e
		}
		out = LifecycleResponse{Accepted: true, AppliedRevision: req.ControlRevision, LatestRevision: latest, LatestEffectiveAt: deadline, ProcessObservations: observation.Processes, LatestMode: effective.Mode}
		return s.record(ctx, u, eid, oid, fp, req.RequestID, "heartbeat", "runner", runner.String(), 1, "runner.heartbeat", actor.String(), nil, out)
	})
	return out, err
}
func (s *Service) Checkpoint(ctx context.Context, req CheckpointRequest) (out LifecycleResponse, err error) {
	_, actor, runner, e := runnerCaller(ctx)
	if e != nil {
		return out, e
	}
	if e = requireRequest(req.RequestID); e != nil {
		return out, e
	}
	now := s.clock.Now()
	if now.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	fp, e := requestFingerprint("checkpoint", req)
	if e != nil {
		return out, e
	}
	eid, e := s.ids.Next("event")
	if e != nil {
		return out, e
	}
	oid, e := s.ids.Next("operation")
	if e != nil {
		return out, e
	}
	err = s.mutate(ctx, "checkpoint:"+req.RequestID, func(u UnitOfWork) error {
		if p, ok, x := u.Idempotency(ctx, req.RequestID, "checkpoint"); x != nil {
			return x
		} else if ok {
			if p.Fingerprint != fp {
				return ErrIdempotencyConflict
			}
			return restoreResponse(p, &out)
		}
		exec, ok, x := u.Execution(ctx, req.ExecutionID)
		if x != nil {
			return x
		}
		if !ok {
			return ErrNotFound
		}
		lease, ok, x := u.Lease(ctx, req.LeaseID)
		if x != nil {
			return x
		}
		if !ok {
			return ErrNotFound
		}
		if exec.LeaseID != lease.ID || lease.ExecutionID != exec.ID || exec.RunnerID != runner || lease.RunnerID != runner || !lease.ActiveAt(now) {
			return domain.ErrLeaseNotOwned
		}
		if req.FencingToken != lease.FencingToken {
			return domain.ErrStaleFence
		}
		target, found, x := u.CanonicalTarget(ctx, exec.IncrementID.String(), runner.String())
		if x != nil {
			return x
		}
		if !found {
			target = domain.ControlTarget{IncrementID: exec.IncrementID, RunnerID: runner}
		}
		controls, x := u.Controls(ctx)
		if x != nil {
			return x
		}
		effective := domain.EffectiveControl(controls, target)
		if _, x = domain.Permit(effective, domain.PermitRequest{Kind: domain.PermitCheckpoint, Target: target, ControlRevision: req.ControlRevision, FencingToken: req.FencingToken, ExpectedFencingToken: lease.FencingToken, Resource: req.ExecutionID}); x != nil {
			return x
		}
		out = LifecycleResponse{Accepted: true, LatestMode: effective.Mode}
		return s.record(ctx, u, eid, oid, fp, req.RequestID, "checkpoint", "execution", req.ExecutionID, exec.Version, "execution.checkpointed", actor.String(), nil, out)
	})
	return out, err
}

func (s *Service) ListRequirements(ctx context.Context) ([]RequirementView, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return nil, err
	}
	var out []RequirementView
	err := s.transact(ctx, func(u UnitOfWork) error {
		rows, err := u.Requirements(ctx)
		if err != nil {
			return err
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID.String() < rows[j].ID.String() })
		pageIDs := make([]string, len(rows))
		for i := range rows {
			pageIDs[i] = rows[i].ID.String()
		}
		links, err := u.RequirementRepositoryLinks(ctx, pageIDs)
		if err != nil {
			return err
		}
		for _, r := range rows {
			text, _, e := u.RequirementText(ctx, r.ID.String())
			if e != nil {
				return e
			}
			ids := make([]string, len(r.Increments))
			for i, id := range r.Increments {
				ids[i] = id.String()
			}
			out = append(out, RequirementView{RequirementID: r.ID.String(), Status: r.Status, Version: r.Version, IncrementIDs: ids, Text: text, RepositoryID: links[r.ID.String()].RepositoryID.String()})
		}
		return nil
	})
	return out, err
}
func (s *Service) GetRequirement(ctx context.Context, id string) (RequirementView, bool, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return RequirementView{}, false, err
	}
	var out RequirementView
	var found bool
	err := s.transact(ctx, func(u UnitOfWork) error {
		r, ok, e := u.Requirement(ctx, id)
		if e != nil {
			return e
		}
		found = ok
		if !ok {
			return nil
		}
		text, _, e := u.RequirementText(ctx, id)
		if e != nil {
			return e
		}
		ids := make([]string, len(r.Increments))
		for i, x := range r.Increments {
			ids[i] = x.String()
		}
		link, hasLink, e := u.RequirementRepositoryLink(ctx, id)
		if e != nil {
			return e
		}
		out = RequirementView{RequirementID: r.ID.String(), Status: r.Status, Version: r.Version, IncrementIDs: ids, Text: text}
		if hasLink {
			out.RepositoryID = link.RepositoryID.String()
		}
		return nil
	})
	return out, found, err
}

func (s *Service) Capture(ctx context.Context, req CaptureRequest) (out CaptureResponse, err error) {
	caller, actor, err := callerActor(ctx, RoleOwner, RoleScheduler)
	if err != nil {
		return out, err
	}
	reqBy, err := requestedBy(caller)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("capture", req)
	if err != nil {
		return out, err
	}
	id := req.RequirementID
	if id == "" {
		id, err = s.ids.Next("requirement")
		if err != nil {
			return out, err
		}
	}
	eventID, err := s.ids.Next("event")
	if err != nil {
		return out, err
	}
	operationID, err := s.ids.Next("operation")
	if err != nil {
		return out, err
	}
	// The link's assignment instant comes from the injected clock, read once
	// before the transaction, exactly as RegisterRepository does. No wall
	// clock is read inside the callback, which Firestore may retry.
	assignedAt := s.clock.Now()
	if assignedAt.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	err = s.mutate(ctx, "capture:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "capture"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		rid, e := domain.NewRequirementID(id)
		if e != nil {
			return e
		}
		// A4: a Capture may name the Repository the Requirement belongs to.
		// The Repository must already exist and must still be registered:
		// a retired Repository is a rolled-back registration, so associating
		// new intake with it would be an association with something the
		// Installation no longer operates on. Both refusals happen before
		// any aggregate is staged, so a refused Capture creates nothing.
		repositoryID := ""
		if req.RepositoryID != "" {
			repository, ok, x := u.Repository(ctx, req.RepositoryID)
			if x != nil {
				return x
			}
			if !ok {
				return fmt.Errorf("%w: repository %q is not registered", ErrNotFound, req.RepositoryID)
			}
			if repository.Status != domain.RepositoryRegistered {
				return fmt.Errorf("%w: repository %q", ErrRepositoryNotAvailable, req.RepositoryID)
			}
			repositoryID = repository.ID.String()
		}
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		target := domain.ControlTarget{InstallationID: s.config.InstallationID, RepositoryID: repositoryID}
		effective := domain.EffectiveControl(controls, target)
		revision := domain.Revision(0)
		if effective.Found {
			revision = effective.Revision
		}
		if _, e = domain.Permit(effective, domain.PermitRequest{Kind: domain.PermitIntake, Target: target, ControlRevision: revision, Resource: id}); e != nil {
			return e
		}
		r := domain.Requirement{ID: rid, Status: domain.RequirementCaptured, Version: 1, RequestedBy: reqBy}
		if e = domain.Validate(r); e != nil {
			return e
		}
		if e = u.SaveRequirement(ctx, r, 0); e != nil {
			return e
		}
		if e = u.SaveRequirementText(ctx, id, req.Text); e != nil {
			return e
		}
		if repositoryID != "" {
			link := domain.RequirementRepositoryLink{RequirementID: rid, RepositoryID: domain.RepositoryID(repositoryID), AssignedAt: assignedAt, RequestedBy: reqBy}
			if e = domain.ValidateRequirementRepositoryLink(link); e != nil {
				return e
			}
			if e = u.SaveRequirementRepositoryLink(ctx, link); e != nil {
				return e
			}
		}
		out = CaptureResponse{RequirementID: id, Version: r.Version, RequestedBy: reqBy, RepositoryID: repositoryID}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "capture", "requirement", id, r.Version, "requirement.captured", actor.String(), nil, out)
	})
	return out, err
}

// Verbose aliases are kept at the application boundary so transport adapters
// can use names that read naturally without introducing another service.
func (s *Service) CaptureRequirement(ctx context.Context, req CaptureRequest) (CaptureResponse, error) {
	return s.Capture(ctx, req)
}

type PlanRequest struct {
	RequestID, RequirementID, IncrementID string
	ExpectedRequirementVersion            domain.Version
}
type PlanResponse struct {
	RequirementID string         `json:"requirement_id"`
	IncrementID   string         `json:"increment_id"`
	Version       domain.Version `json:"version"`
}

func (s *Service) Plan(ctx context.Context, req PlanRequest) (out PlanResponse, err error) {
	_, actor, err := callerActor(ctx, RoleOwner, RoleScheduler)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("plan", req)
	if err != nil {
		return out, err
	}
	id := req.IncrementID
	if id == "" {
		id, err = s.ids.Next("increment")
		if err != nil {
			return out, err
		}
	}
	eventID, err := s.ids.Next("event")
	if err != nil {
		return out, err
	}
	operationID, err := s.ids.Next("operation")
	if err != nil {
		return out, err
	}
	err = s.mutate(ctx, "plan:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "plan"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		r, ok, e := u.Requirement(ctx, req.RequirementID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if req.ExpectedRequirementVersion != r.Version {
			return domain.ErrStaleVersion
		}
		iid, e := domain.NewIncrementID(id)
		if e != nil {
			return e
		}
		rid, e := domain.NewRequirementID(req.RequirementID)
		if e != nil {
			return e
		}
		next := r
		next.Increments = append(append([]domain.IncrementID(nil), r.Increments...), iid)
		next.Version++
		if e = u.SaveRequirement(ctx, next, r.Version); e != nil {
			return e
		}
		inc := domain.Increment{ID: iid, RequirementID: rid, Status: domain.IncrementProposed, Version: 1}
		if e = u.SaveIncrement(ctx, inc, 0); e != nil {
			return e
		}
		// A7: the canonical target's RepositoryID comes from the captured
		// Requirement's own link, read inside this same transaction. A
		// Requirement with no link yields an empty RepositoryID, which is
		// not an error: the association is optional.
		link, hasLink, e := u.RequirementRepositoryLink(ctx, req.RequirementID)
		if e != nil {
			return e
		}
		linkedRepository := ""
		if hasLink {
			linkedRepository = link.RepositoryID.String()
		}
		target := domain.ControlTarget{InstallationID: s.config.InstallationID, RepositoryID: linkedRepository}
		target.IncrementID = iid
		target.RequirementID = rid
		if e = u.SaveCanonicalTarget(ctx, id, target); e != nil {
			return e
		}
		out = PlanResponse{RequirementID: req.RequirementID, IncrementID: id, Version: inc.Version}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "plan", "increment", id, inc.Version, "increment.proposed", actor.String(), nil, out)
	})
	return out, err
}
func (s *Service) PlanIncrement(ctx context.Context, req PlanRequest) (PlanResponse, error) {
	return s.Plan(ctx, req)
}

// Prepare moves a planned increment from proposed to ready. Keeping this
// transition at the application boundary lets runners claim only increments
// that passed the explicit planning/validation gate.
type PrepareRequest struct {
	RequestID, IncrementID string
	ExpectedVersion        domain.Version
}

type PrepareResponse struct {
	IncrementID string         `json:"increment_id"`
	Version     domain.Version `json:"version"`
}

func (s *Service) Prepare(ctx context.Context, req PrepareRequest) (out PrepareResponse, err error) {
	_, actor, err := callerActor(ctx, RoleOwner, RoleScheduler)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("prepare", req)
	if err != nil {
		return out, err
	}
	eventID, err := s.ids.Next("event")
	if err != nil {
		return out, err
	}
	operationID, err := s.ids.Next("operation")
	if err != nil {
		return out, err
	}
	err = s.mutate(ctx, "prepare:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "prepare"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		inc, ok, e := u.Increment(ctx, req.IncrementID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if inc.Version != req.ExpectedVersion {
			return domain.ErrStaleVersion
		}
		next, e := domain.DecideIncrement(inc, domain.IncrementCommand{
			Kind:            domain.IncrementPrepare,
			Actor:           actor,
			At:              s.clock.Now(),
			ExpectedVersion: req.ExpectedVersion,
		})
		if e != nil {
			return e
		}
		if e = u.SaveIncrement(ctx, next, inc.Version); e != nil {
			return e
		}
		out = PrepareResponse{IncrementID: req.IncrementID, Version: next.Version}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "prepare", "increment", req.IncrementID, next.Version, "increment.ready", actor.String(), nil, out)
	})
	return out, err
}

func (s *Service) PrepareIncrement(ctx context.Context, req PrepareRequest) (PrepareResponse, error) {
	return s.Prepare(ctx, req)
}

type ClaimRequest struct {
	RequestID, IncrementID   string
	ExpectedIncrementVersion domain.Version
	ControlRevision          domain.Revision
	Target                   domain.ControlTarget
}
type ClaimResponse struct {
	IncrementID  string              `json:"increment_id"`
	ExecutionID  string              `json:"execution_id"`
	LeaseID      string              `json:"lease_id"`
	RunnerID     string              `json:"runner_id"`
	Version      domain.Version      `json:"version"`
	FencingToken domain.FencingToken `json:"fencing_token"`
}

func (s *Service) Claim(ctx context.Context, req ClaimRequest) (out ClaimResponse, err error) {
	_, actor, runner, err := runnerCaller(ctx)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("claim", req)
	if err != nil {
		return out, err
	}
	eventID, err := s.ids.Next("event")
	if err != nil {
		return out, err
	}
	operationID, err := s.ids.Next("operation")
	if err != nil {
		return out, err
	}
	outboxID, err := s.ids.Next("outbox")
	if err != nil {
		return out, err
	}
	executionID, err := s.ids.Next("execution")
	if err != nil {
		return out, err
	}
	leaseID, err := s.ids.Next("lease")
	if err != nil {
		return out, err
	}
	issuedAt := s.clock.Now()
	if issuedAt.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	expiresAt := issuedAt.Add(s.config.LeaseTTL)
	err = s.mutate(ctx, "claim:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "claim"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		active, e := u.ActiveLeases(ctx, 100)
		if e != nil {
			return e
		}
		if len(active) >= 100 {
			return errors.New("active lease safety limit reached")
		}
		inc, ok, e := u.Increment(ctx, req.IncrementID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if req.ExpectedIncrementVersion != inc.Version {
			return domain.ErrStaleVersion
		}
		if lease, exists, e := u.ActiveLeaseForIncrementAt(ctx, req.IncrementID, issuedAt); e != nil {
			return e
		} else if exists && lease.Status == domain.LeaseActive {
			return fmt.Errorf("increment already claimed")
		}
		claimBase := inc
		var expiredLease domain.Lease
		if inc.Status == domain.IncrementLeased {
			var found bool
			expiredLease, found, e = u.LatestLeaseForIncrement(ctx, req.IncrementID)
			if !found || expiredLease.Status != domain.LeaseActive || expiredLease.ActiveAt(issuedAt) {
				return domain.ErrInvalidTransition
			}
			expiredLease, e = domain.ExpireLease(expiredLease, issuedAt)
			if e != nil {
				return e
			}
			claimBase.Status = domain.IncrementReady
			claimBase.Version++

			// V2-058 (dp-v2-058 d1): terminate the superseded Execution in
			// the very same transaction that reclaims its expired Lease, so
			// a reclaim never manufactures a fresh orphan for OrphanSweep
			// to find later. It is read and written inside this closure
			// only -- no new UnitOfWork port and no store change.
			//
			// It is guarded and must never make the reclaim itself fail
			// (dp-v2-058 d5): domain.MarkExecutionLost returns
			// ErrInvalidTransition for an already-terminal Execution and
			// ErrStaleFence for a LeaseID/ExecutionID/FencingToken linkage
			// mismatch, and an unconditional call would propagate exactly
			// those errors out of Claim, turning the recovery path itself
			// into a new way for a crashed Runner to become unrecoverable
			// -- strictly worse than the orphan OrphanSweep already
			// converges. So the termination runs only when the superseded
			// Execution is found, is not already terminal (using the same
			// terminal set domain.MarkExecutionLost refuses to move away
			// from), and its linkage matches the Lease being expired; any
			// other case is left untouched, Claim proceeds unchanged, and
			// OrphanSweep remains the fallback for it.
			if superseded, found, e2 := u.Execution(ctx, expiredLease.ExecutionID.String()); e2 != nil {
				return e2
			} else if found && !executionAlreadyTerminal(superseded.Status) &&
				superseded.LeaseID == expiredLease.ID &&
				superseded.FencingToken == expiredLease.FencingToken &&
				superseded.ID == expiredLease.ExecutionID {
				lost, e2 := domain.MarkExecutionLost(superseded, expiredLease)
				if e2 != nil {
					return e2
				}
				if e2 = u.SaveExecution(ctx, lost, superseded.Version); e2 != nil {
					return e2
				}
			}
		}
		iid, e := domain.NewIncrementID(req.IncrementID)
		if e != nil {
			return e
		}
		eid, e := domain.NewExecutionID(executionID)
		if e != nil {
			return e
		}
		lid, e := domain.NewLeaseID(leaseID)
		if e != nil {
			return e
		}
		canonical, found, e := u.CanonicalTarget(ctx, req.IncrementID, runner.String())
		if e != nil {
			return e
		}
		if !found {
			canonical = domain.ControlTarget{IncrementID: iid, RunnerID: runner}
		}
		if canonical.RunnerID == "" {
			canonical.RunnerID = runner
		}
		if canonical.IncrementID == "" {
			canonical.IncrementID = iid
		}
		if !sameTarget(req.Target, canonical) {
			return domain.ErrControlDenied
		}
		canonical = canonicalizeTarget(req.Target, canonical)
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		effective := domain.EffectiveControl(controls, canonical)
		permit, e := domain.Permit(effective, domain.PermitRequest{Kind: domain.PermitClaim, Target: canonical, ControlRevision: req.ControlRevision, Resource: req.IncrementID})
		if e != nil {
			return e
		}
		_ = permit
		fence, e := u.MaxFencingToken(ctx, req.IncrementID)
		if e != nil {
			return e
		}
		lease, e := domain.IssueLease(domain.LeaseRequest{ID: lid, ExecutionID: eid, IncrementID: iid, RunnerID: runner, PreviousFencingToken: fence, ControlRevision: req.ControlRevision, IssuedAt: issuedAt, ExpiresAt: expiresAt})
		if e != nil {
			return e
		}
		cmd := domain.IncrementCommand{Kind: domain.IncrementLease, Actor: actor, At: issuedAt, ExpectedVersion: claimBase.Version}
		next, e := domain.DecideIncrement(claimBase, cmd)
		if e != nil {
			return e
		}
		exec := domain.Execution{ID: eid, IncrementID: iid, RunnerID: runner, LeaseID: lid, FencingToken: lease.FencingToken, ControlRevision: req.ControlRevision, Status: domain.ExecutionLeased, Version: 1}
		if e = u.SaveIncrement(ctx, next, inc.Version); e != nil {
			return e
		}
		if expiredLease.ID != "" {
			if e = u.SaveLease(ctx, expiredLease, expiredLease.Version-1); e != nil {
				return e
			}
		}
		if e = u.SaveLease(ctx, lease, 0); e != nil {
			return e
		}
		if e = u.SaveExecution(ctx, exec, 0); e != nil {
			return e
		}
		out = ClaimResponse{IncrementID: req.IncrementID, ExecutionID: executionID, LeaseID: leaseID, RunnerID: runner.String(), Version: next.Version, FencingToken: lease.FencingToken}
		effectOutbox, e := s.effectOutbox(ctx, u, outboxID, operationID, req.RequestID, domain.PermitClaim, canonical, next.Version, lease.FencingToken, req.ControlRevision, "claim-issued", runner.String())
		if e != nil {
			return e
		}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "claim", "increment", req.IncrementID, next.Version, "increment.claimed", actor.String(), effectOutbox, out)
	})
	return out, err
}
func (s *Service) ClaimIncrement(ctx context.Context, req ClaimRequest) (ClaimResponse, error) {
	return s.Claim(ctx, req)
}

type ControlRequest struct {
	RequestID string
	Scope     domain.ControlScope
	Mode      domain.ControlMode
	Reason    string
	At        time.Time
}
type ControlResponse struct {
	Revision    domain.Revision     `json:"revision"`
	Mode        domain.ControlMode  `json:"mode"`
	State       domain.ControlState `json:"state"`
	RequestedBy domain.RequestedBy  `json:"requested_by"`
}

func (s *Service) Control(ctx context.Context, req ControlRequest) (out ControlResponse, err error) {
	caller, actor, err := callerActor(ctx, RoleOwner, RoleScheduler)
	if err != nil {
		return out, err
	}
	reqBy, err := requestedBy(caller)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	if !validControlScope(req.Scope) || !validControlMode(req.Mode) {
		return out, errors.New("invalid control mode or scope")
	}
	if req.At.IsZero() {
		req.At = s.clock.Now()
	}
	fingerprint, err := requestFingerprint("control", req)
	if err != nil {
		return out, err
	}
	eventID, err := s.ids.Next("event")
	if err != nil {
		return out, err
	}
	operationID, err := s.ids.Next("operation")
	if err != nil {
		return out, err
	}
	outboxID, err := s.ids.Next("outbox")
	if err != nil {
		return out, err
	}
	err = s.mutate(ctx, "control:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "control"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		revision, e := u.ControlRevision(ctx)
		if e != nil {
			return e
		}
		revision++
		at := req.At
		if at.IsZero() {
			return errors.New("control requires timestamp")
		}
		intent := domain.ControlIntent{Scope: req.Scope, Mode: req.Mode, Revision: revision, Actor: actor, At: at, EffectiveAt: at, Reason: req.Reason}
		if e = u.SaveControl(ctx, intent, revision-1); e != nil {
			return e
		}
		if e = u.SaveControlRequestedBy(ctx, revision, reqBy); e != nil {
			return e
		}
		leases, e := u.ActiveLeases(ctx, 101)
		if e != nil {
			return e
		}
		if len(leases) > 100 {
			return errors.New("active lease safety limit exceeded")
		}
		snapshots := make([]domain.ControlTargetSnapshot, 0, len(leases))
		for _, lease := range leases {
			target, found, x := u.CanonicalTarget(ctx, lease.IncrementID.String(), lease.RunnerID.String())
			if x != nil {
				return x
			}
			if !found {
				// A8: with no stored canonical target the repository is
				// resolved from the aggregate graph the lease already
				// names -- lease -> Increment -> Requirement -> link -- so
				// the snapshot still carries a real RepositoryID and a
				// repository-scoped Control Intent still matches it. A
				// missing Increment or a Requirement with no link yields an
				// empty RepositoryID, which is the honest answer rather than
				// a guess.
				fallbackRepository := ""
				increment, hasIncrement, x := u.Increment(ctx, lease.IncrementID.String())
				if x != nil {
					return x
				}
				if hasIncrement {
					link, hasLink, x := u.RequirementRepositoryLink(ctx, increment.RequirementID.String())
					if x != nil {
						return x
					}
					if hasLink {
						fallbackRepository = link.RepositoryID.String()
					}
				}
				target = domain.ControlTarget{InstallationID: s.config.InstallationID, RepositoryID: fallbackRepository, IncrementID: lease.IncrementID, RunnerID: lease.RunnerID}
			} else if target.RunnerID == "" {
				// CanonicalTarget is saved once at Plan time, before any
				// Runner has claimed the Increment, so it never durably
				// carries a RunnerID (SaveCanonicalTarget takes no runner
				// parameter). Without this, a ControlTargetSnapshot built
				// from a found-but-runner-less canonical target would carry
				// an empty Target.RunnerID, and the verification
				// reconciler's per-target RunnerObservation lookup
				// (keyed by Target.RunnerID) would then never match any
				// real Runner's observation. This mirrors the same
				// fill-only-when-empty merge canonicalizeTarget already
				// does for Claim's request-vs-canonical target.
				target.RunnerID = lease.RunnerID
			}
			if domain.ControlApplies(req.Scope, target) {
				snapshots = append(snapshots, domain.ControlTargetSnapshot{Target: target, LeaseID: lease.ID, ExecutionID: lease.ExecutionID, FencingToken: lease.FencingToken})
			}
		}
		if e = u.SaveControlProgress(ctx, domain.ControlProgress{Revision: revision, State: domain.ControlRequested, RequestedAt: at, EffectiveAt: at, Verification: domain.VerificationPending, Targets: snapshots}, ""); e != nil {
			return e
		}
		out = ControlResponse{Revision: revision, Mode: req.Mode, State: domain.ControlRequested, RequestedBy: reqBy}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "control", "control", req.Scope.Value, domain.Version(revision), "control.changed", actor.String(), &OutboxItem{ID: outboxID, Kind: "control-changed", Target: req.Scope.Value, ControlScope: req.Scope, ExpectedVersion: domain.Version(revision), ControlRevision: revision}, out)
	})
	return out, err
}
func (s *Service) SetControl(ctx context.Context, req ControlRequest) (ControlResponse, error) {
	return s.Control(ctx, req)
}

type PermitRequest struct {
	RequestID                          string
	Kind                               domain.PermitKind
	Target                             domain.ControlTarget
	ControlRevision                    domain.Revision
	FencingToken, ExpectedFencingToken domain.FencingToken
	Resource                           string
}
type PermitResponse struct {
	Allowed  bool            `json:"allowed"`
	Revision domain.Revision `json:"revision"`
	Reason   string          `json:"reason"`
}

func (s *Service) Permit(ctx context.Context, req PermitRequest) (out PermitResponse, err error) {
	if _, _, err = callerActor(ctx, RoleOwner, RoleRunner, RoleScheduler); err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	err = s.transact(ctx, func(u UnitOfWork) error {
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		d, e := domain.Permit(domain.EffectiveControl(controls, req.Target), domain.PermitRequest{Kind: req.Kind, Target: req.Target, ControlRevision: req.ControlRevision, FencingToken: req.FencingToken, ExpectedFencingToken: req.ExpectedFencingToken, Resource: req.Resource})
		out = PermitResponse{Allowed: d.Allowed(), Revision: d.Revision(), Reason: d.Reason()}
		return e
	})
	return out, err
}
func (s *Service) EvaluatePermit(ctx context.Context, req PermitRequest) (PermitResponse, error) {
	return s.Permit(ctx, req)
}

type AcceptResultRequest struct {
	RequestID, ExecutionID, LeaseID string
	ExpectedExecutionVersion        domain.Version
	FencingToken                    domain.FencingToken
	ControlRevision                 domain.Revision
	Succeeded                       bool
	Target                          domain.ControlTarget
}
type AcceptResultResponse struct {
	ExecutionID string                 `json:"execution_id"`
	Status      domain.ExecutionStatus `json:"status"`
	Version     domain.Version         `json:"version"`
}

func (s *Service) AcceptResult(ctx context.Context, req AcceptResultRequest) (out AcceptResultResponse, err error) {
	_, actor, runner, err := runnerCaller(ctx)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	now := s.clock.Now()
	if now.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	fingerprint, err := requestFingerprint("accept-result", req)
	if err != nil {
		return out, err
	}
	eventID, err := s.ids.Next("event")
	if err != nil {
		return out, err
	}
	operationID, err := s.ids.Next("operation")
	if err != nil {
		return out, err
	}
	outboxID, err := s.ids.Next("outbox")
	if err != nil {
		return out, err
	}
	err = s.mutate(ctx, "accept-result:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "accept-result"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		exec, ok, e := u.Execution(ctx, req.ExecutionID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if exec.RunnerID != runner {
			return domain.ErrLeaseNotOwned
		}
		if exec.Version != req.ExpectedExecutionVersion {
			return domain.ErrStaleVersion
		}
		lease, ok, e := u.Lease(ctx, req.LeaseID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if lease.RunnerID != runner {
			return domain.ErrLeaseNotOwned
		}
		canonical, found, e := u.CanonicalTarget(ctx, exec.IncrementID.String(), exec.RunnerID.String())
		if e != nil {
			return e
		}
		if !found {
			canonical = domain.ControlTarget{IncrementID: exec.IncrementID, RunnerID: exec.RunnerID}
		}
		if !sameTarget(req.Target, canonical) {
			return domain.ErrControlDenied
		}
		canonical = canonicalizeTarget(req.Target, canonical)
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		next, e := domain.AcceptExecutionResult(exec, lease, domain.ExecutionResult{ExecutionID: exec.ID, LeaseID: lease.ID, FencingToken: req.FencingToken, ControlRevision: req.ControlRevision, At: now, Succeeded: req.Succeeded}, domain.EffectiveControl(controls, canonical))
		if e != nil {
			return e
		}
		if e = u.SaveExecution(ctx, next, exec.Version); e != nil {
			return e
		}
		out = AcceptResultResponse{ExecutionID: req.ExecutionID, Status: next.Status, Version: next.Version}
		effectOutbox, e := s.effectOutbox(ctx, u, outboxID, operationID, req.RequestID, domain.PermitExternalEffect, canonical, next.Version, req.FencingToken, req.ControlRevision, "result-accepted", req.ExecutionID)
		if e != nil {
			return e
		}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "accept-result", "execution", req.ExecutionID, next.Version, "execution.result-accepted", actor.String(), effectOutbox, out)
	})
	return out, err
}
func (s *Service) AcceptExecutionResult(ctx context.Context, req AcceptResultRequest) (AcceptResultResponse, error) {
	return s.AcceptResult(ctx, req)
}

// effectOutbox is the sole construction path for external-effect outbox
// records. It re-reads policy and fence immediately before durable intent is
// recorded, then obtains the opaque domain Effect permit.
func (s *Service) effectOutbox(ctx context.Context, u UnitOfWork, outboxID, operationID, requestID string, kind domain.PermitKind, target domain.ControlTarget, expected domain.Version, fence domain.FencingToken, revision domain.Revision, outboxKind, resource string) (*OutboxItem, error) {
	var latest domain.Lease
	if target.IncrementID != "" {
		var found bool
		var err error
		latest, found, err = u.LatestLeaseForIncrement(ctx, target.IncrementID.String())
		if err != nil {
			return nil, err
		}
		if !found || latest.FencingToken != fence {
			return nil, domain.ErrStaleFence
		}
	}
	controls, err := u.Controls(ctx)
	if err != nil {
		return nil, err
	}
	current := domain.EffectiveControl(controls, target)
	authoritativeRevision := domain.Revision(0)
	if current.Found {
		authoritativeRevision = current.Revision
	}
	if authoritativeRevision != revision {
		return nil, domain.ErrControlDenied
	}
	operation, err := domain.NewOperationID(operationID)
	if err != nil {
		return nil, err
	}
	request, err := domain.NewRequestID(requestID)
	if err != nil {
		return nil, err
	}
	permit, err := domain.Permit(current, domain.PermitRequest{Kind: kind, Target: target, ControlRevision: revision, FencingToken: fence, ExpectedFencingToken: fence, Resource: resource})
	if err != nil {
		return nil, err
	}
	effect, err := domain.EffectFromPermit(permit, current, fence, operation, request, kind, resource, expected, fence, revision, nil)
	if err != nil {
		return nil, err
	}
	// The OutboxDispatcher later re-validates this item by re-fetching
	// CanonicalTarget fresh from the durable store and comparing it against
	// the item's own stored ControlTarget (internal/application/outbox.go
	// beforeEffect). CanonicalTarget is saved once at Plan time and never
	// carries a RunnerID (SaveCanonicalTarget takes no runner parameter),
	// but target here has already been canonicalized with the calling
	// Runner's id filled in (Claim/Renew/AcceptResult all do this for their
	// own Permit evaluation above, which does need the RunnerID to match a
	// ScopeRunner control). Persisting that ephemeral, runner-filled target
	// as ControlTarget would make it permanently mismatch the durable
	// canonical target beforeEffect re-fetches, so every such effect would
	// fail ErrOutboxNotReady and go Dead on its very first delivery
	// attempt. Persist the same durable value beforeEffect will compare
	// against instead.
	persistedTarget := target
	if target.IncrementID != "" {
		if canonical, found, cerr := u.CanonicalTarget(ctx, target.IncrementID.String(), target.RunnerID.String()); cerr != nil {
			return nil, cerr
		} else if found {
			persistedTarget = canonical
		}
	}
	return &OutboxItem{ID: outboxID, OperationID: effect.OperationID.String(), Kind: outboxKind, Target: effect.Target, ExpectedVersion: effect.ExpectedVersion, FencingToken: effect.FencingToken, ControlRevision: effect.ControlRevision, IncrementID: target.IncrementID.String(), LeaseID: latest.ID.String(), RunnerID: target.RunnerID.String(), ControlTarget: persistedTarget, PermitKind: kind}, nil
}

func (s *Service) record(ctx context.Context, u UnitOfWork, eventID, operationID, fingerprint, requestID, operation, aggregateType, aggregateID string, version domain.Version, typ, actor string, outbox *OutboxItem, value any) error {
	if authority, ok := u.(interface{ AuthorityContext() context.Context }); ok {
		ctx = authority.AuthorityContext()
	}
	err := error(nil)
	at, ok := ctx.Value(authorityTimeKey{}).(time.Time)
	if !ok || at.IsZero() {
		return errors.New("transaction authority time is required")
	}
	if at.IsZero() {
		return errors.New("clock returned zero time")
	}
	if outbox != nil {
		outbox.RequestID = requestID
		outbox.OperationID = operationID
		outbox.CreatedAt = at
	}
	if err = u.Record(Event{ID: eventID, RequestID: requestID, AggregateType: aggregateType, AggregateID: aggregateID, Type: typ, ActorID: actor, Version: version, At: at}, outbox); err != nil {
		return err
	}
	response, err := responseJSON(value)
	if err != nil {
		return err
	}
	return u.SaveIdempotency(ctx, IdempotentResponse{RequestID: requestID, Operation: operation, Fingerprint: fingerprint, ResponseJSON: response, Value: value})
}
