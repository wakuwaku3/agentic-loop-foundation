package application

import (
	"context"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ===========================================================================
// V2-082: the one edge that leaves the captured status.
// ===========================================================================
//
// Measured on the parent tree, with a fresh grep rather than from any earlier
// record: domain.RequirementStartFraming occurred in exactly two non-test
// lines, both inside internal/domain/model.go -- its declaration and its
// switch case -- and in zero non-test lines anywhere else. Nothing outside a
// test ever constructed a domain.RequirementCommand with that Kind. The
// consequence was not that one edge was missing: the set of Requirement
// statuses reachable through every application command was the single element
// {captured}, because Capture constructs the aggregate directly at captured
// and never calls domain.DecideRequirement, Plan bumps the version without
// touching the status, and both needs-input commands require a source status
// -- framing, active, evaluating -- that no application command could
// produce. So the question surface V2-065 shipped was not merely unexercised;
// its precondition was unreachable.
//
// StartFraming below issues that one transition and nothing else. After it the
// reachable set is exactly {captured, framing, needs-input, ready}. The six
// other domain Requirement commands (start, wait, recover, evaluate, pause,
// cancel) stay unissued by design, as does domain.CompleteRequirementFromRelease:
// the outcome this file serves is "a Requirement can reach the state that asks
// a person a question", not a Requirement lifecycle driver.
//
// internal/domain is not edited. The transition, its guard and its target
// status all already exist -- RequirementStartFraming is admitted from
// RequirementCaptured only and sets RequirementFraming -- so the whole repair
// is that this layer finally calls it. A repair that needs no domain change is
// a repair that cannot have widened the state machine.
//
// Why this is a separate caller command rather than an advance inside Capture.
// A Capture caller asked to record a Requirement, not to start shaping it.
// Advancing inside Capture would move a Requirement out of captured because of
// a request that did not ask for it, would make `captured` a status no
// observer can ever see, would leave the domain's captured->framing edge
// permanently dead, and would change CaptureResponse.Version from 1 to 2 --
// moving every capture request fingerprint and the openapi response contract.
// A flag on CaptureRequest is no better: it fuses two transitions into one
// idempotency key, so a partial retry cannot be reasoned about. A reconciler
// sweep is worse still: nobody asks at all.

// StartFramingRequest names the Requirement and the version the caller
// believes it is at, and carries nothing else.
//
// It deliberately has NO control_revision field, NO repository_id field and NO
// timestamp field. The revision comes from domain.EffectiveControl inside the
// transaction exactly as Capture derives it -- a caller-supplied revision
// would be a trap, because domain.Permit refuses unless the supplied value is
// EXACTLY the authoritative one, so a caller that had not re-read controls
// would be denied for a reason unrelated to the owner's intent. The repository
// comes from the Requirement's own link, as Plan resolves it. The instant
// comes from the transaction authority accessor Service.record also uses, so
// it is byte-identical to the recorded event's At and does not move if the
// transaction is retried.
type StartFramingRequest struct {
	RequestID       string
	RequirementID   string
	ExpectedVersion domain.Version
}

type StartFramingResponse struct {
	RequirementID string                   `json:"requirement_id"`
	Status        domain.RequirementStatus `json:"status"`
	Version       domain.Version           `json:"version"`
}

// StartFraming moves a captured Requirement into framing, which is the status
// from which the needs-input question can be asked.
//
// Roles: owner and scheduler -- exactly the pair Capture accepts, and exactly
// the pair Plan, Prepare, Control and RegisterRepository accept. A runner is
// refused: a Runner executes an Increment it was handed and does not decide
// that a Requirement should start being shaped, and letting it would make the
// recorded actor unusable as attribution. The command is NOT scheduler-only,
// and the reason is a measurement rather than a preference: no non-test code
// path in this repository constructs a Caller with RoleScheduler. Every
// RoleScheduler occurrence outside tests is a role ACCEPTED by a command,
// never a role PRODUCED by a transport, so a scheduler-only command would be
// unreachable over both the production authenticator and the preview-local
// one -- this file's own defect reproduced one level up.
//
// The gate is domain.Permit with Kind domain.PermitClaim, evaluated with
// domain.Permit alone. Nothing is passed to domain.EffectFromPermit, because
// no durable effect is staged, and that omission is load-bearing rather than
// incidental: EffectFromPermit requires the current effective mode to be
// exactly ControlAllow, so routing this command through it would deny framing
// under pause-intake, which is wrong.
//
// The seven-mode behaviour that follows from PermitClaim is the design, and it
// is asserted in framing_test.go rather than described here: allow ALLOWS,
// pause-intake ALLOWS, pause-claim DENIES, graceful-stop DENIES,
// immediate-stop DENIES, emergency-stop DENIES, cancel DENIES.
//
//   - pause-intake ALLOWS framing an already-captured Requirement, while the
//     same control revision still DENIES a fresh Capture. "Take no new work
//     in, finish what you already have" is exactly that asymmetry, and
//     framing something already captured is finishing what you have. Copying
//     Capture's PermitIntake here would have turned pause-intake into a full
//     stop by accident.
//   - pause-claim DENIES, because starting to shape a Requirement is the first
//     commitment of Loop capacity to it, which is what pause-claim exists to
//     withhold.
//   - every stop mode DENIES. That is the property a permit-free command would
//     not have. Plan and Prepare evaluate no Permit at all and therefore move
//     canonical Requirement and Increment state under emergency-stop; that is
//     a measured defect of theirs, recorded for the tech lead and deliberately
//     NOT propagated here. A precedent is not an argument when the precedent
//     is the thing being escalated.
//
// It stages no outbox item: nothing outside the control plane is asked to do
// anything by a Requirement beginning to be shaped. domain.PermitClaim's own
// SideEffect() is false for the same reason.
func (s *Service) StartFraming(ctx context.Context, req StartFramingRequest) (out StartFramingResponse, err error) {
	_, actor, err := callerActor(ctx, RoleOwner, RoleScheduler)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("start-framing", req)
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
	err = s.mutate(ctx, "start-framing:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "start-framing"); e != nil {
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
		// The ControlTarget's RepositoryID comes from the captured
		// Requirement's own link, read inside this same transaction, exactly
		// as Plan resolves it. A Requirement with no link yields an empty
		// RepositoryID, which is not an error: the association is optional,
		// and ServiceConfig deliberately holds no repository id, because an
		// Installation registers many Repositories and a per-process id would
		// make a repository-scoped Control Intent match the wrong one.
		link, hasLink, e := u.RequirementRepositoryLink(ctx, req.RequirementID)
		if e != nil {
			return e
		}
		linkedRepository := ""
		if hasLink {
			linkedRepository = link.RepositoryID.String()
		}
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		target := domain.ControlTarget{InstallationID: s.config.InstallationID, RepositoryID: linkedRepository, RequirementID: r.ID}
		effective := domain.EffectiveControl(controls, target)
		revision := domain.Revision(0)
		if effective.Found {
			revision = effective.Revision
		}
		if _, e = domain.Permit(effective, domain.PermitRequest{Kind: domain.PermitClaim, Target: target, ControlRevision: revision, Resource: req.RequirementID}); e != nil {
			return e
		}
		at, e := transactionAuthorityTime(ctx, u)
		if e != nil {
			return e
		}
		next, e := domain.DecideRequirement(r, domain.RequirementCommand{
			Kind:            domain.RequirementStartFraming,
			Actor:           actor,
			At:              at,
			ExpectedVersion: req.ExpectedVersion,
		})
		if e != nil {
			return e
		}
		if e = u.SaveRequirement(ctx, next, r.Version); e != nil {
			return e
		}
		out = StartFramingResponse{RequirementID: req.RequirementID, Status: next.Status, Version: next.Version}
		// The nil outbox argument is the assertion in code that this command
		// stages no durable effect; framing_test.go asserts it from the store.
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "start-framing", "requirement", req.RequirementID, next.Version, "requirement.framing-started", actor.String(), nil, out)
	})
	return out, err
}
