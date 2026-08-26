package application

// Publishing one verified local commit as one reviewable branch (V2-072).
//
// This file holds three things and deliberately nothing else: the command
// that creates the Operation Intent and the Outbox Item in ONE domain
// transaction, the target read the runner-side adapter performs immediately
// before its first external write, and the write-once Observation write.
//
// It creates no Pull Request, no read model, no view and no HTTP surface, and
// it never writes a Requirement or an Increment: a published branch is not a
// finished Requirement (non_goal 2). internal/application/readmodels.go and
// internal/application/repository.go are untouched by this task, and an AST
// control in internal/runner asserts mechanically that no call in this file
// saves a Requirement or an Increment.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

var (
	// ErrRequirementHasNoRepository refuses a publication for a Requirement
	// that names no Repository. It is the refusal that makes non_goal 1
	// unrepresentable rather than merely forbidden: registration is the only
	// way a SourceLocator enters the store, and the link is the only way a
	// Requirement reaches one.
	ErrRequirementHasNoRepository = errors.New("the requirement names no registered repository, so there is no target to publish to")
	// ErrPublicationTargetDisagrees refuses a delivery whose payload does not
	// agree with the target read back from the store. It exists for
	// docs/architecture/domain-model.md section 10: the next Runner does not
	// trust the Work Packet and re-observes external state.
	ErrPublicationTargetDisagrees = errors.New("the publication payload disagrees with the stored publication target")
)

// PublicationOutboxKind is the Outbox Item kind of a publication. It is a new
// kind rather than a reuse of "result-accepted", so an operator reading the
// outbox can tell a publication from a result acceptance without decoding a
// payload.
const PublicationOutboxKind = "publication-requested"

// PublishChangeRequest asks for one verified local commit to be published as
// one reviewable branch.
//
// It carries NO forge coordinate, and that absence is the whole point of
// A17: there is no field here, and no environment variable in production,
// through which a caller could name an owner, a name, a host or a locator.
// The coordinate is resolved inside the command's own transaction from the
// registered Repository the Requirement is linked to, and from nowhere else.
//
// The Git facts below are the measurement the Runner already performed with
// GitSourceControl.VerifyIntegrity and Commit. They describe the committed
// tree, not the caller's intended ChangeSet: publishing the intent rather
// than the verified commit would mean the reviewable artefact was never the
// verified one (dp-v2-072 d4).
type PublishChangeRequest struct {
	RequestID                string
	ExecutionID              string
	LeaseID                  string
	ExpectedExecutionVersion domain.Version
	FencingToken             domain.FencingToken
	ControlRevision          domain.Revision
	Target                   domain.ControlTarget
	BaseBranch               string
	BaseCommit               string
	HeadCommit               string
	HeadTree                 string
	ChangedPaths             int
}

// PublishChangeResponse names what was recorded, so a caller can find the
// Outbox Item and the eventual Observation. It carries no URL: the
// human-readable link is derived from the locator, the ref and the commit at
// the moment it is displayed.
type PublishChangeResponse struct {
	OperationID  string `json:"operation_id"`
	OutboxID     string `json:"outbox_id"`
	RepositoryID string `json:"repository_id"`
	Ref          string `json:"ref"`
}

// PublicationTarget is what the runner-side adapter re-reads immediately
// before its first external write. Owner and Name come from the registered
// Repository as it stands NOW; Ref and BaseCommit come from the durable
// Outbox Item's own payload. Comparing the two against the payload the
// adapter was handed is what turns "the Work Packet is not trusted" into a
// refusal rather than a convention.
type PublicationTarget struct {
	RepositoryID string `json:"repository_id"`
	Owner        string `json:"owner"`
	Name         string `json:"name"`
	Ref          string `json:"ref"`
	BaseCommit   string `json:"base_commit"`
}

// resolvePublicationTarget walks Execution -> Increment -> Requirement ->
// RequirementRepositoryLink -> Repository inside the caller's transaction and
// returns the registered Repository. It is the ONLY way a coordinate enters a
// publication, which is why it is a private helper with exactly two callers
// in this file rather than an exported service method.
func resolvePublicationTarget(ctx context.Context, u UnitOfWork, execution domain.Execution) (domain.Repository, error) {
	increment, ok, err := u.Increment(ctx, execution.IncrementID.String())
	if err != nil {
		return domain.Repository{}, err
	}
	if !ok {
		return domain.Repository{}, fmt.Errorf("%w: increment %q", ErrNotFound, execution.IncrementID)
	}
	requirement, ok, err := u.Requirement(ctx, increment.RequirementID.String())
	if err != nil {
		return domain.Repository{}, err
	}
	if !ok {
		return domain.Repository{}, fmt.Errorf("%w: requirement %q", ErrNotFound, increment.RequirementID)
	}
	link, ok, err := u.RequirementRepositoryLink(ctx, requirement.ID.String())
	if err != nil {
		return domain.Repository{}, err
	}
	if !ok || !link.Recorded() {
		return domain.Repository{}, fmt.Errorf("%w: requirement %q", ErrRequirementHasNoRepository, requirement.ID)
	}
	repository, ok, err := u.Repository(ctx, link.RepositoryID.String())
	if err != nil {
		return domain.Repository{}, err
	}
	if !ok {
		return domain.Repository{}, fmt.Errorf("%w: repository %q is not registered", ErrNotFound, link.RepositoryID)
	}
	if repository.Status != domain.RepositoryRegistered {
		return domain.Repository{}, fmt.Errorf("%w: repository %q", ErrRepositoryNotAvailable, repository.ID)
	}
	return repository, nil
}

// PublishChange records the Operation Intent and the Outbox Item in one
// domain transaction. It follows AcceptResult's ceremony exactly -- the
// caller role check, requireRequest, requestFingerprint, s.mutate keyed by
// the operation and the request id, the idempotency read that replays a
// matching fingerprint and refuses a differing one, then the transaction --
// and it adds the A17 target resolution before the existing effectOutbox
// path.
//
// It records the publication as an intent only. Nothing here reaches the
// forge: delivery is the OutboxDispatcher's job and happens outside every
// transaction, which is the boundary that stops a transaction retry from
// repeating an external effect.
func (s *Service) PublishChange(ctx context.Context, req PublishChangeRequest) (out PublishChangeResponse, err error) {
	_, actor, runner, err := runnerCaller(ctx)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("publish-change", req)
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
	err = s.mutate(ctx, "publish-change:"+operationID+":"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "publish-change"); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			return restoreResponse(prior, &out)
		}
		execution, ok, e := u.Execution(ctx, req.ExecutionID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if execution.RunnerID != runner {
			return domain.ErrLeaseNotOwned
		}
		if execution.Version != req.ExpectedExecutionVersion {
			return domain.ErrStaleVersion
		}
		canonical, found, e := u.CanonicalTarget(ctx, execution.IncrementID.String(), execution.RunnerID.String())
		if e != nil {
			return e
		}
		if !found {
			canonical = domain.ControlTarget{IncrementID: execution.IncrementID, RunnerID: execution.RunnerID}
		}
		if !sameTarget(req.Target, canonical) {
			return domain.ErrControlDenied
		}
		canonical = canonicalizeTarget(req.Target, canonical)
		// A17: the coordinate can only come from a registered Repository
		// reached through the Requirement's link. Every refusal below happens
		// before effectOutbox is reached, so a refused publication creates no
		// Outbox Item at all.
		repository, e := resolvePublicationTarget(ctx, u, execution)
		if e != nil {
			return e
		}
		ref, e := domain.PublicationRefName(execution.IncrementID, execution.ID)
		if e != nil {
			return e
		}
		intent := domain.PublicationIntent{
			RepositoryID: repository.ID,
			Locator:      repository.Locator,
			Ref:          ref,
			BaseBranch:   req.BaseBranch,
			BaseCommit:   req.BaseCommit,
			HeadCommit:   req.HeadCommit,
			HeadTree:     req.HeadTree,
			ChangedPaths: req.ChangedPaths,
		}
		if e = domain.ValidatePublicationIntent(intent); e != nil {
			return e
		}
		payload, e := json.Marshal(intent)
		if e != nil {
			return e
		}
		out = PublishChangeResponse{OperationID: operationID, OutboxID: outboxID, RepositoryID: repository.ID.String(), Ref: ref}
		item, e := s.effectOutbox(ctx, u, outboxID, operationID, req.RequestID, domain.PermitExternalEffect, canonical, execution.Version, req.FencingToken, req.ControlRevision, PublicationOutboxKind, req.ExecutionID, payload)
		if e != nil {
			return e
		}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "publish-change", "execution", req.ExecutionID, execution.Version, "execution.publication-requested", actor.String(), item, out)
	})
	return out, err
}

// PublicationTargetForOutbox is the pre-write re-read of A18. It is called by
// the runner-side adapter immediately before its first forge call, and it
// deliberately re-walks the aggregate graph rather than trusting the payload:
// docs/architecture/domain-model.md section 10 requires that the next Runner
// not trust the Work Packet and re-observe external state, and the
// publication payload is the one packet that would otherwise be trusted
// blindly.
//
// It is a read. It stages nothing and reserves the read-transaction budget
// like every other bounded read in this package.
func (s *Service) PublicationTargetForOutbox(ctx context.Context, outboxID string) (out PublicationTarget, found bool, err error) {
	if _, _, _, err = runnerCaller(ctx); err != nil {
		return out, false, err
	}
	if outboxID == "" {
		return out, false, errors.New("outbox id is required")
	}
	err = s.transact(ctx, func(u UnitOfWork) error {
		item, ok, e := u.Outbox(ctx, outboxID)
		if e != nil {
			return e
		}
		if !ok {
			return nil
		}
		if item.Kind != PublicationOutboxKind {
			return fmt.Errorf("%w: outbox item %q is not a publication", ErrInvalidOutbox, outboxID)
		}
		var intent domain.PublicationIntent
		if e = json.Unmarshal(item.Payload, &intent); e != nil {
			return fmt.Errorf("%w: the publication payload could not be read", ErrInvalidOutbox)
		}
		if e = domain.ValidatePublicationIntent(intent); e != nil {
			return e
		}
		execution, exists, e := u.Execution(ctx, item.Target)
		if e != nil {
			return e
		}
		if !exists {
			return fmt.Errorf("%w: execution %q", ErrNotFound, item.Target)
		}
		repository, e := resolvePublicationTarget(ctx, u, execution)
		if e != nil {
			return e
		}
		out = PublicationTarget{
			RepositoryID: repository.ID.String(),
			Owner:        repository.Locator.Owner,
			Name:         repository.Locator.Name,
			Ref:          intent.Ref,
			BaseCommit:   intent.BaseCommit,
		}
		found = true
		return nil
	})
	if err != nil {
		return PublicationTarget{}, false, err
	}
	return out, found, nil
}

// Agrees reports whether a target read back from the store agrees with the
// intent an adapter was handed, on exactly the four fields A18 names. It is a
// pure comparison so the refusal is testable without a store.
func (t PublicationTarget) Agrees(intent domain.PublicationIntent) bool {
	return t.RepositoryID == intent.RepositoryID.String() &&
		t.Owner == intent.Locator.Owner &&
		t.Name == intent.Locator.Name &&
		t.Ref == intent.Ref &&
		t.BaseCommit == intent.BaseCommit
}

// RecordPublication writes the publication Observation, write-once per
// operation identifier. The store enforces at-most-once; this command
// enforces that the value is a valid Observation before it can reach the
// store, so a record that measured nothing must say so with the unobserved
// state rather than with empty success fields.
//
// It writes NO Requirement and NO Increment. That is asserted structurally by
// the AST control in internal/runner and behaviourally by the test that reads
// the Requirement's status and version back unchanged after a confirmed
// publication.
func (s *Service) RecordPublication(ctx context.Context, value domain.PublicationObservation) error {
	if _, _, _, err := runnerCaller(ctx); err != nil {
		return err
	}
	if err := domain.ValidatePublicationObservation(value); err != nil {
		return err
	}
	return s.mutate(ctx, "publication-observation:"+value.OperationID.String(), func(u UnitOfWork) error {
		return u.SavePublicationObservation(ctx, value)
	})
}

// Publication reads one Observation by operation identifier. It returns false
// for an operation with no row and never synthesizes one.
func (s *Service) Publication(ctx context.Context, operationID string) (out domain.PublicationObservation, found bool, err error) {
	if _, _, _, err = runnerCaller(ctx); err != nil {
		return out, false, err
	}
	if operationID == "" {
		return out, false, errors.New("operation id is required")
	}
	err = s.transact(ctx, func(u UnitOfWork) error {
		var e error
		out, found, e = u.PublicationObservation(ctx, operationID)
		return e
	})
	if err != nil {
		return domain.PublicationObservation{}, false, err
	}
	return out, found, nil
}
