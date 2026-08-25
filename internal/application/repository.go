package application

// Repository registration, retire and forge Observation (V2-064).
//
// Every mutating operation here mirrors Service.Capture step for step
// (service.go L581-651): callerActor role check, requestedBy for
// attribution, requireRequest on RequestID, requestFingerprint for
// idempotency, s.ids.Next for the identifier, s.mutate keyed by operation
// and request id, an Idempotency read that replays a matching fingerprint
// and refuses a differing one, a domain.Permit gate on domain.PermitIntake,
// domain validation through domain.ValidateRepository, and one s.record
// event. Nothing in this file constructs an external process, opens a
// socket, or holds a forge credential: the Control Plane never reaches the
// forge, and reachability arrives only as a Runner-submitted Observation
// (dp-v2-064 d6).
//
// The two read operations (ListRepositories, GetRepository) deliberately do
// NOT carry the mutation ceremony: a read has no request body, so it has no
// request_id to be idempotent on, and running s.mutate for a GET would
// reserve the Installation's bounded Firestore mutation budget and write an
// idempotency record for an operation that changes nothing. They follow the
// house read pattern instead (callerActor plus s.transact), exactly as
// Service.GetRequirementDetail and Service.QueueSummary do.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ErrRepositoryAlreadyRegistered is the duplicate-locator refusal: the
// normalised (forge, owner, name) triple is unique per Installation among
// registered Repositories. It is a conflict, not a caller mistake, so the
// transport maps it to 409.
var ErrRepositoryAlreadyRegistered = errors.New("a repository with this source locator is already registered")

// RepositoryStaleAfter bounds how long a forge Observation is treated as
// current when answering "can the loop run". It is a constant rather than a
// wall-clock timer: staleness is always evaluated against the injected
// clock's instant (dp-v2-064 d15, acceptance A21).
const RepositoryStaleAfter = domain.DefaultObservationStaleAfter

type RegisterRepositoryRequest struct {
	RequestID string
	// RepositoryID is optional and follows CaptureRequest.RequirementID: a
	// caller may supply an identifier for a deterministic retry, otherwise
	// the Loop issues an opaque one.
	RepositoryID string
	// SourceURL is the raw locator as written by the owner. It is parsed and
	// normalised immediately and is never stored: the stored coordinate is
	// the normalised SourceLocator, and the identity is the opaque
	// repository id (dp-v2-064 d2).
	SourceURL     string
	DefaultBranch string
}

type RepositoryResponse struct {
	RepositoryID string                  `json:"repository_id"`
	Locator      domain.SourceLocator    `json:"locator"`
	Status       domain.RepositoryStatus `json:"status"`
	Version      domain.Version          `json:"version"`
	RequestedBy  domain.RequestedBy      `json:"requested_by"`
}

type RetireRepositoryRequest struct {
	RequestID       string
	RepositoryID    string
	ExpectedVersion domain.Version
}

// ObserveRepositoryRequest is the bounded forge Observation a Runner submits.
// It carries only parsed fields: no raw process output, no response body, no
// credential, and no runner identity beyond the authenticated caller's own
// session, which is never persisted on the Observation.
type ObserveRepositoryRequest struct {
	RequestID      string
	RepositoryID   string
	Reachable      bool
	DefaultBranch  string
	CanPush        bool
	ForgeNodeID    string
	AdapterVersion string
	Reason         string
}

type ObserveRepositoryResponse struct {
	RepositoryID  string                         `json:"repository_id"`
	Accepted      bool                           `json:"accepted"`
	ObservedAt    time.Time                      `json:"observed_at"`
	Executability domain.RepositoryExecutability `json:"executability"`
}

// repositoryTarget is the Control target of a Repository operation: the
// Installation plus the Repository's own id, so a domain.ScopeRepository
// Control Intent naming this repository actually matches it.
//
// Measured debt, recorded and deliberately NOT repaired here (A6, d14):
// ServiceConfig.RepositoryID (service.go L135) is consumed at L434, L734 and
// L1088 to build ControlTarget but is never set by
// cmd/control-plane/main.go L172, so ControlTarget.RepositoryID is the empty
// string on every existing production path and a ScopeRepository Control
// Intent can never match there (domain/control.go L138 compares
// target.RepositoryID against the scope value). This function is the first
// call site in the codebase that supplies a real repository id, because
// registration is what makes one exist. Repairing the other three call sites
// belongs with the Increment-to-Repository link that gives them a repository
// to name, which is the successor stage-2 task's content.
func (s *Service) repositoryTarget(repositoryID string) domain.ControlTarget {
	return domain.ControlTarget{InstallationID: s.config.InstallationID, RepositoryID: repositoryID}
}

// repositoryPermit runs the Control gate for one repository-scoped operation
// and returns the effective revision the caller must record.
func repositoryPermit(ctx context.Context, u UnitOfWork, target domain.ControlTarget, kind domain.PermitKind, resource string) error {
	controls, err := u.Controls(ctx)
	if err != nil {
		return err
	}
	effective := domain.EffectiveControl(controls, target)
	revision := domain.Revision(0)
	if effective.Found {
		revision = effective.Revision
	}
	_, err = domain.Permit(effective, domain.PermitRequest{Kind: kind, Target: target, ControlRevision: revision, Resource: resource})
	return err
}

func (s *Service) RegisterRepository(ctx context.Context, req RegisterRepositoryRequest) (out RepositoryResponse, err error) {
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
	locator, err := domain.ParseSourceLocator(req.SourceURL)
	if err != nil {
		return out, err
	}
	locator.DefaultBranch = req.DefaultBranch
	locator, err = domain.NormalizeSourceLocator(locator)
	if err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("register-repository", req)
	if err != nil {
		return out, err
	}
	id := req.RepositoryID
	if id == "" {
		if id, err = s.ids.Next("repository"); err != nil {
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
	now := s.clock.Now()
	if now.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	err = s.mutate(ctx, "register-repository:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "register-repository"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		rid, e := domain.NewRepositoryID(id)
		if e != nil {
			return e
		}
		if e = repositoryPermit(ctx, u, s.repositoryTarget(id), domain.PermitIntake, id); e != nil {
			return e
		}
		existing, e := u.Repositories(ctx)
		if e != nil {
			return e
		}
		for _, candidate := range existing {
			if candidate.Status == domain.RepositoryRegistered && candidate.Locator.Key() == locator.Key() {
				return fmt.Errorf("%w: %s", ErrRepositoryAlreadyRegistered, locator.Key())
			}
			if candidate.ID == rid {
				return fmt.Errorf("%w: repository id is already in use", ErrRepositoryAlreadyRegistered)
			}
		}
		repository, e := domain.DecideRepository(
			domain.Repository{ID: rid, Locator: locator, RequestedBy: reqBy},
			domain.RepositoryCommand{Kind: domain.RepositoryRegister, Actor: actor, At: now, ExpectedVersion: 0},
		)
		if e != nil {
			return e
		}
		if e = domain.ValidateRepository(repository); e != nil {
			return e
		}
		if e = u.SaveRepository(ctx, repository, 0); e != nil {
			return e
		}
		out = RepositoryResponse{RepositoryID: id, Locator: repository.Locator, Status: repository.Status, Version: repository.Version, RequestedBy: reqBy}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "register-repository", "repository", id, repository.Version, "repository.registered", actor.String(), nil, out)
	})
	return out, err
}

func (s *Service) RetireRepository(ctx context.Context, req RetireRepositoryRequest) (out RepositoryResponse, err error) {
	caller, actor, err := callerActor(ctx, RoleOwner, RoleScheduler)
	if err != nil {
		return out, err
	}
	if _, err = requestedBy(caller); err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("retire-repository", req)
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
	now := s.clock.Now()
	if now.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	err = s.mutate(ctx, "retire-repository:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "retire-repository"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		current, ok, e := u.Repository(ctx, req.RepositoryID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if e = repositoryPermit(ctx, u, s.repositoryTarget(req.RepositoryID), domain.PermitIntake, req.RepositoryID); e != nil {
			return e
		}
		expected := req.ExpectedVersion
		if expected == 0 {
			expected = current.Version
		}
		next, e := domain.DecideRepository(current, domain.RepositoryCommand{Kind: domain.RepositoryRetire, Actor: actor, At: now, ExpectedVersion: expected})
		if e != nil {
			return e
		}
		if e = domain.ValidateRepository(next); e != nil {
			return e
		}
		if e = u.SaveRepository(ctx, next, current.Version); e != nil {
			return e
		}
		out = RepositoryResponse{RepositoryID: next.ID.String(), Locator: next.Locator, Status: next.Status, Version: next.Version, RequestedBy: next.RequestedBy}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "retire-repository", "repository", next.ID.String(), next.Version, "repository.retired", actor.String(), nil, out)
	})
	return out, err
}

func (s *Service) ObserveRepository(ctx context.Context, req ObserveRepositoryRequest) (out ObserveRepositoryResponse, err error) {
	_, actor, _, err := runnerCaller(ctx)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("observe-repository", req)
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
	now := s.clock.Now()
	if now.IsZero() {
		return out, errors.New("clock returned zero time")
	}
	err = s.mutate(ctx, "observe-repository:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "observe-repository"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		repository, ok, e := u.Repository(ctx, req.RepositoryID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if e = domain.ValidateRepository(repository); e != nil {
			return e
		}
		target := s.repositoryTarget(req.RepositoryID)
		if e = repositoryPermit(ctx, u, target, domain.PermitIntake, req.RepositoryID); e != nil {
			return e
		}
		observation := domain.RepositoryObservation{
			RepositoryID:   repository.ID,
			Locator:        repository.Locator,
			Reachable:      req.Reachable,
			DefaultBranch:  req.DefaultBranch,
			CanPush:        req.CanPush,
			ForgeNodeID:    req.ForgeNodeID,
			AdapterVersion: req.AdapterVersion,
			Reason:         req.Reason,
			ObservedAt:     now,
		}
		if e = u.SaveRepositoryObservation(ctx, observation); e != nil {
			return e
		}
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		effective := domain.EffectiveControl(controls, target)
		out = ObserveRepositoryResponse{
			RepositoryID:  repository.ID.String(),
			Accepted:      true,
			ObservedAt:    now,
			Executability: domain.RepositoryExecutabilityFrom(repository, effective, observation, true, now, RepositoryStaleAfter),
		}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "observe-repository", "repository", repository.ID.String(), repository.Version, "repository.observed", actor.String(), nil, out)
	})
	return out, err
}

// RepositoryView is one row of the owner list: identity, coordinate, status
// and the measured (or explicitly unmeasured) answer to whether the loop can
// run against it.
type RepositoryView struct {
	RepositoryID  string                         `json:"repository_id"`
	Locator       domain.SourceLocator           `json:"locator"`
	Status        domain.RepositoryStatus        `json:"status"`
	Version       domain.Version                 `json:"version"`
	RequestedBy   domain.RequestedBy             `json:"requested_by"`
	Executability domain.RepositoryExecutability `json:"executability"`
}

type RepositoryListResponse struct {
	Repositories []RepositoryView `json:"repositories"`
}

// ObservedState is how this service reports a declared observable for which
// no data source exists at this commit. State is machine-readable and Reason
// names the specific missing seam. It never carries a plausible-looking
// value: an absent measurement is reported as absent (acceptance A10).
type ObservedState struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

const (
	// ObservedMeasured marks a field whose value in this response was
	// actually read from a store in this request.
	ObservedMeasured = "measured"
	// ObservedUnobserved marks a field whose data source exists in the
	// design but has never been populated for this Repository.
	ObservedUnobserved = "unobserved"
	// ObservedNotImplemented marks a field whose data source does not exist
	// in the code at this commit.
	ObservedNotImplemented = "not-implemented"
)

// RepositoryBacklogView answers "Requirement Backlogの状態". Since V2-071 the
// repository-scoped answer is measured: RequirementCount is the number of
// Requirements actually linked to this Repository, read in this request
// through the write-once Requirement-to-Repository link, and State is
// ObservedMeasured to say so.
//
// Truncated reports that the bounded read hit its limit, so RequirementCount
// is "at least this many" rather than an exact total. A bounded count that
// silently presented itself as exact would be the plausible-looking
// unmeasured value this view exists to avoid.
//
// InstallationScope is unchanged and keeps its name: it is the
// Installation-wide queue summary, not a repository-scoped figure, and a
// reader must still be able to tell the two apart.
type RepositoryBacklogView struct {
	State             string        `json:"state"`
	Reason            string        `json:"reason"`
	RequirementCount  int           `json:"requirement_count"`
	Truncated         bool          `json:"truncated,omitempty"`
	InstallationScope *QueueSummary `json:"installation_scope,omitempty"`
}

// RepositoryDetailView renders all six observables cap-repository-registration
// declares. Each one is either measured in this request or carries an
// explicit machine-readable unobserved/not-implemented state with its reason.
type RepositoryDetailView struct {
	// 1. 識別情報 -- measured.
	RepositoryID string                  `json:"repository_id"`
	Locator      domain.SourceLocator    `json:"locator"`
	Status       domain.RepositoryStatus `json:"status"`
	Version      domain.Version          `json:"version"`
	RequestedBy  domain.RequestedBy      `json:"requested_by"`
	// 2. ApplicationのPreview/Stable状態.
	ApplicationRelease ObservedState `json:"application_release"`
	// 3. 適用中の大原則とRepository固有ルール.
	Policy ObservedState `json:"policy"`
	// 4. Requirement Backlogの状態.
	RequirementBacklog RepositoryBacklogView `json:"requirement_backlog"`
	// 5. 利用可能なRunnerとAI資源.
	RunnersAndAIResources ObservedState `json:"runners_and_ai_resources"`
	// 6. ループが実行可能かとその理由 -- measured from the forge Observation,
	// or explicitly unobserved when none has been submitted.
	Executability domain.RepositoryExecutability `json:"executability"`
	// Observation is the bounded forge Observation this answer was derived
	// from, or nil when none exists.
	Observation *domain.RepositoryObservation `json:"observation,omitempty"`
	// EffectiveControl is the Control policy actually in force for this
	// Repository, a measured input of Executability.
	EffectiveControlMode     domain.ControlMode `json:"effective_control_mode"`
	EffectiveControlRevision domain.Revision    `json:"effective_control_revision"`
}

// backlogReason states what was measured and how it was bounded, so the
// reason is a description of the measurement rather than a restatement of the
// number. A truncated count says so in words as well as in the flag.
func backlogReason(count int, truncated bool) string {
	if truncated {
		return fmt.Sprintf("at least %d Requirements are linked to this Repository through the write-once Requirement-to-Repository association; the read was bounded at %d rows in the storage query, so this is a lower bound and not an exact total. installation_scope below remains the Installation-wide count, not a repository-scoped figure", count, MaxPageSize)
	}
	return fmt.Sprintf("%d Requirements are linked to this Repository through the write-once Requirement-to-Repository association, read in this request and bounded at %d rows in the storage query. installation_scope below remains the Installation-wide count, not a repository-scoped figure", count, MaxPageSize)
}

// ListRepositories returns every registered Repository. Retired Repositories
// are excluded: retire is the rollback of a registration, so a retired
// Repository must not appear as one the Installation operates on.
//
// The list is not filtered by caller: a Repository belongs to the
// Installation, so any authenticated owner caller sees all of them (A7).
func (s *Service) ListRepositories(ctx context.Context) (out RepositoryListResponse, err error) {
	if _, _, err = callerActor(ctx, RoleOwner, RoleScheduler); err != nil {
		return out, err
	}
	out.Repositories = []RepositoryView{}
	err = s.transact(ctx, func(u UnitOfWork) error {
		rows, e := u.Repositories(ctx)
		if e != nil {
			return e
		}
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		now := s.clock.Now()
		views := make([]RepositoryView, 0, len(rows))
		for _, repository := range rows {
			if repository.Status != domain.RepositoryRegistered {
				continue
			}
			observation, found, e := u.RepositoryObservation(ctx, repository.ID.String())
			if e != nil {
				return e
			}
			effective := domain.EffectiveControl(controls, s.repositoryTarget(repository.ID.String()))
			views = append(views, RepositoryView{
				RepositoryID:  repository.ID.String(),
				Locator:       repository.Locator,
				Status:        repository.Status,
				Version:       repository.Version,
				RequestedBy:   repository.RequestedBy,
				Executability: domain.RepositoryExecutabilityFrom(repository, effective, observation, found, now, RepositoryStaleAfter),
			})
		}
		sort.Slice(views, func(i, j int) bool { return views[i].RepositoryID < views[j].RepositoryID })
		out.Repositories = views
		return nil
	})
	return out, err
}

// GetRepository renders the six declared observables for one Repository. A
// retired Repository is still readable: the rollback must be observable.
func (s *Service) GetRepository(ctx context.Context, repositoryID string) (out RepositoryDetailView, found bool, err error) {
	if _, _, err = callerActor(ctx, RoleOwner, RoleScheduler); err != nil {
		return out, false, err
	}
	if repositoryID == "" {
		return out, false, errors.New("repository_id is required")
	}
	err = s.transact(ctx, func(u UnitOfWork) error {
		repository, ok, e := u.Repository(ctx, repositoryID)
		if e != nil {
			return e
		}
		found = ok
		if !ok {
			return nil
		}
		observation, hasObservation, e := u.RepositoryObservation(ctx, repositoryID)
		if e != nil {
			return e
		}
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		summary, e := u.QueueSummary(ctx)
		if e != nil {
			return e
		}
		// The repository-scoped backlog is read through the write-once
		// Requirement-to-Repository link, bounded by MaxPageSize in the
		// storage query. The bound is reported rather than hidden.
		backlogIDs, backlogTruncated, e := u.RequirementIDsForRepository(ctx, repositoryID, MaxPageSize)
		if e != nil {
			return e
		}
		now := s.clock.Now()
		effective := domain.EffectiveControl(controls, s.repositoryTarget(repositoryID))
		mode := effective.Mode
		if mode == "" {
			mode = domain.ControlAllow
		}
		out = RepositoryDetailView{
			RepositoryID: repository.ID.String(),
			Locator:      repository.Locator,
			Status:       repository.Status,
			Version:      repository.Version,
			RequestedBy:  repository.RequestedBy,
			ApplicationRelease: ObservedState{
				State:  ObservedNotImplemented,
				Reason: "no Application Release is bound to a Repository at this commit: the Preview and Stable release records exist only per Requirement (domain.StableReleaseSnapshot) and per release candidate, and no port associates either with a repository_id",
			},
			Policy: ObservedState{
				State:  ObservedNotImplemented,
				Reason: "no policy aggregate exists at this commit: neither the 大原則 set nor a Repository Contract / repository-specific rule record is persisted or exposed through any application port",
			},
			RequirementBacklog: RepositoryBacklogView{
				State:             ObservedMeasured,
				Reason:            backlogReason(len(backlogIDs), backlogTruncated),
				RequirementCount:  len(backlogIDs),
				Truncated:         backlogTruncated,
				InstallationScope: &summary,
			},
			RunnersAndAIResources: ObservedState{
				State:  ObservedNotImplemented,
				Reason: "no Runner registry and no Provider Account aggregate exist at this commit: RunnerObservationRepository is keyed by a single runner id with no enumeration port, and provider accounts and their AI resource limits are held only on the Runner machine",
			},
			Executability:            domain.RepositoryExecutabilityFrom(repository, effective, observation, hasObservation, now, RepositoryStaleAfter),
			EffectiveControlMode:     mode,
			EffectiveControlRevision: effective.Revision,
		}
		if hasObservation {
			copied := observation
			out.Observation = &copied
		}
		return nil
	})
	return out, found, err
}
