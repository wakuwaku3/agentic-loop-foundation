package application

import (
	"context"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ===========================================================================
// V2-090: the owner's pause, its exit, and its cancel.
// ===========================================================================
//
// Measured on the parent tree with a fresh grep rather than from any earlier
// record: `grep -rn '\bRequirementPause\b' --include='*.go' . | grep -v
// _test.go` returned exactly TWO lines, both inside internal/domain/model.go --
// the declaration at :444 and the switch case at :510 -- and the same grep for
// RequirementCancel returned exactly two, also both inside model.go. So neither
// command had an issuer anywhere outside the domain, and `grep -rn
// '\bRequirementResume\b' --include='*.go' .` returned ZERO lines, including in
// test files: there was no resume at all.
//
// THE MEASUREMENT THAT SHAPES THIS FILE. `paused` was a SOURCE status in
// exactly ONE of DecideRequirement's ten branches -- cancel -- derived by
// reading all ten rather than by grepping. So a pause shipped without an exit
// would have handed the owner a button whose only sequel is destroying the
// Requirement. That is why the exit is here and not in a follow-up, and why
// domain.Requirement gained PausedFrom: docs/architecture/domain-model.md:269
// defines the exit from paused as "直前の安全な非終端状態、`cancelled`", and
// 直前の安全な非終端状態 names the status the Requirement was ACTUALLY IN.
//
// Every edge these three commands make reachable is already described by the
// product, one citation per edge: ready->paused at
// docs/architecture/domain-model.md:265, active->paused at :266, waiting->paused
// at :267, recovering->paused at :270, and BOTH exits at :269. The owner-issued
// triple is docs/product/user-facing-spec.md:201, "- Requirementのpause、
// resume、cancel", under section 4.8 whose subject sentence at :194 is
// "利用者は対象範囲を指定して次を実行できる。". resume is a product verb at
// docs/product/definition.md:146 and cancel at :147. docs/** is prohibited to
// this task precisely so a missing citation could not be manufactured.
//
// WHY THE STATUS TRANSITION IS NOT FUSED WITH A CONTROL INTENT.
// docs/architecture/domain-model.md:295 decomposes an Increment's pause as the
// COMBINATION of "Incrementをscopeに含むControl Intent（`pause-intake`／
// `pause-claim`など）と親RequirementのpausedへのStatus遷移" -- two halves, not
// one. Service.PauseRequirement below writes NO Control Intent, and Service.
// Control transitions NO Requirement (measured: internal/application/service.go
// :1457's Control body contains no SaveRequirement and no DecideRequirement
// call). Fusing them would put two lifecycles behind one request_id, which
// docs/product/user-facing-spec.md:288-296 forbids -- "各ライフサイクルは関連
// 付けるが、同じ状態として同期しない" -- and would let one idempotency key own
// two stories. It would also claim a stop completion nobody observed: docs/
// product/definition.md:164 requires worker ack, process exit and lease release
// or expiry before a stop may be called complete, and a Requirement status
// transition has none of that machinery. So these commands terminate no
// Execution, expire no Lease, propagate nothing to a Runner and verify no stop
// completion. The propagation half already exists and is POST /v1/controls.
//
// THE PERMIT ASYMMETRY, which is the reason a paused Requirement always has an
// exit. A permit withholds the Loop's capacity, so it must gate an operation
// that can INCREASE what the Loop may do and must not gate one that can only
// REDUCE it.
//
//   - PauseRequirement and CancelRequirement evaluate NO domain.Permit at all
//     and are therefore ALLOWED under all seven control modes, 7 of 7 each. An
//     owner who cannot stop a Requirement while a stop is in force contradicts
//     docs/product/definition.md:132 ("利用者の停止指示へ確実に従い、停止完了を
//     検証可能にする") and :137 ("自律性は利用者の統制より優先されない").
//   - ResumeRequirement evaluates ONE domain.Permit of kind domain.PermitClaim,
//     exactly as Service.StartFraming and Service.CompleteFraming do, and is
//     therefore ALLOWED under allow and pause-intake and DENIED under
//     pause-claim, graceful-stop, immediate-stop, emergency-stop and cancel --
//     2 of 7. Resuming is the moment a Requirement becomes claimable again,
//     which is precisely what pause-claim exists to withhold.
//
// So under emergency-stop a paused Requirement's resume is DENIED and its
// cancel is ALLOWED: the paused state retains at least one exit under every one
// of the seven modes. pause_test.go asserts that as seven cells rather than
// describing it here.
//
// NOTHING is passed to domain.EffectFromPermit by any of the three. That
// omission is load-bearing rather than incidental: measured at
// internal/domain/control.go:301, EffectFromPermit requires the CURRENT
// effective mode to be exactly domain.ControlAllow, so routing a PAUSE through
// it would deny the pause under all six non-allow modes -- the exact inversion
// of what a stop control is for.
//
// Every s.record call below passes a NIL OutboxItem: nothing outside the
// control plane is asked to do anything by a Requirement's own status moving,
// and domain.PermitClaim's SideEffect() is false for the same reason. The event
// types requirement.paused, requirement.resumed and requirement.cancelled need
// no schema change: measured, contracts/schemas/domain-event.json constrains
// event_type by the pattern ^[a-z][a-z0-9.-]{1,63}$ and by NO enum.
//
// internal/application/service.go and internal/application/framing.go are both
// PROHIBITED to this task and both byte-unchanged. Every helper these three
// commands use -- callerActor, requireRequest, requestFingerprint,
// restoreResponse, s.mutate, transactionAuthorityTime, s.record -- is
// package-private and callable from a sibling file, so no edit to either was
// needed.

// PauseRequirementRequest names the Requirement and the version the caller
// believes it is at, and carries nothing else.
//
// Like StartFramingRequest and CompleteFramingRequest it deliberately has NO
// control_revision field, NO repository_id field and NO timestamp field. A
// caller-supplied revision would be a trap, because domain.Permit refuses
// unless the supplied value is EXACTLY the authoritative one; the repository
// comes from the Requirement's own link; and the instant comes from the
// transaction authority accessor Service.record also uses, so it is
// byte-identical to the recorded event's At and does not move on a retry.
type PauseRequirementRequest struct {
	RequestID       string
	RequirementID   string
	ExpectedVersion domain.Version
}

// PauseRequirementResponse carries the status a resume would restore, in
// addition to the three fields its siblings carry.
//
// ResumesTo exists because a pause whose exit is invisible is still a trap. It
// is the status the Requirement was in immediately before the pause -- the
// 直前の安全な非終端状態 of docs/architecture/domain-model.md:269 -- read back
// from the domain's own PausedFrom rather than from the request, so it cannot
// disagree with what a later resume will actually do.
type PauseRequirementResponse struct {
	RequirementID string                   `json:"requirement_id"`
	Status        domain.RequirementStatus `json:"status"`
	Version       domain.Version           `json:"version"`
	ResumesTo     domain.RequirementStatus `json:"resumes_to"`
}

type ResumeRequirementRequest struct {
	RequestID       string
	RequirementID   string
	ExpectedVersion domain.Version
}

type ResumeRequirementResponse struct {
	RequirementID string                   `json:"requirement_id"`
	Status        domain.RequirementStatus `json:"status"`
	Version       domain.Version           `json:"version"`
}

type CancelRequirementRequest struct {
	RequestID       string
	RequirementID   string
	ExpectedVersion domain.Version
}

type CancelRequirementResponse struct {
	RequirementID string                   `json:"requirement_id"`
	Status        domain.RequirementStatus `json:"status"`
	Version       domain.Version           `json:"version"`
}

// PauseRequirement stops further processing of a Requirement at the owner's
// request, moving it to paused and recording which of ready, active, waiting or
// recovering it came from.
//
// Roles: RoleOwner ONLY. That is not a preference: docs/product/user-facing-spec
// .md:201 puts the pause/resume/cancel triple under section 4.8, whose subject
// sentence at :194 is 利用者; docs/architecture/domain-model.md:269 says
// 人間が処理を停止, :273 says 人間が以後の解決を不要と判断 and :211 calls a
// Control Intent 人間が発行した...command; and docs/product/user-facing-spec.md
// :274 says 人間の指示で新しい処理を停止している and :277 人間が以後の処理を
// 取り消した. The product names the actor as 人間 or 利用者 six times across
// those passages and names the Loop, the scheduler or a Runner ZERO times.
// application.Role is a closed three-value enum, so "every role the product does
// not name" is a finite set of exactly two, and pause_test.go asserts an explicit
// refusal for each of them on each of the three commands, deriving the role axis
// from caller.go's source so a fourth role added later fails the test instead of
// silently gaining authority.
//
// It evaluates NO domain.Permit. See the file comment above: a stop the owner
// cannot issue while a stop is in force contradicts docs/product/definition.md
// :132 and :137.
func (s *Service) PauseRequirement(ctx context.Context, req PauseRequirementRequest) (out PauseRequirementResponse, err error) {
	_, actor, err := callerActor(ctx, RoleOwner)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("pause-requirement", req)
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
	err = s.mutate(ctx, "pause-requirement:"+req.RequestID, func(u UnitOfWork) error {
		// The idempotency replay stays FIRST: a request that already executed
		// replays its recorded response rather than being re-judged.
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "pause-requirement"); e != nil {
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
		at, e := transactionAuthorityTime(ctx, u)
		if e != nil {
			return e
		}
		next, e := domain.DecideRequirement(r, domain.RequirementCommand{
			Kind:            domain.RequirementPause,
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
		// ResumesTo is read from the domain's own memory, never from r.Status
		// directly, so the response and a later resume cannot disagree.
		out = PauseRequirementResponse{RequirementID: req.RequirementID, Status: next.Status, Version: next.Version, ResumesTo: next.PausedFrom}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "pause-requirement", "requirement", req.RequirementID, next.Version, "requirement.paused", actor.String(), nil, out)
	})
	return out, err
}

// ResumeRequirement lifts a pause, restoring the EXACT status the Requirement
// was in when it was paused and clearing the memory.
//
// Roles: RoleOwner ONLY, for the reason PauseRequirement gives -- and with one
// extra force behind it here. A pause the Loop could lift on its own is not a
// pause: docs/product/definition.md:137 says 自律性は利用者の統制より優先されない,
// so the actor that may undo an owner's stop is the owner.
//
// This is the ONE of the three that evaluates a domain.Permit, of kind
// domain.PermitClaim, resolved against the Requirement's own ControlTarget with
// the revision read from domain.EffectiveControl INSIDE this transaction --
// exactly the shape Service.StartFraming and Service.CompleteFraming use.
// Resuming is the moment the Requirement becomes claimable again, which is what
// pause-claim exists to withhold, so allow and pause-intake ALLOW it and the
// five remaining modes DENY it: 2 of 7. Nothing is passed to
// domain.EffectFromPermit.
//
// The asymmetry with pause and cancel is the whole design: a resume can only
// INCREASE what the Loop may do, so it is gated; a pause and a cancel can only
// REDUCE it, so they are not. That is what leaves a paused Requirement an exit
// under emergency-stop.
func (s *Service) ResumeRequirement(ctx context.Context, req ResumeRequirementRequest) (out ResumeRequirementResponse, err error) {
	_, actor, err := callerActor(ctx, RoleOwner)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("resume-requirement", req)
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
	err = s.mutate(ctx, "resume-requirement:"+req.RequestID, func(u UnitOfWork) error {
		// The idempotency replay stays FIRST, ahead of the Permit: a request
		// that already executed must replay its recorded response rather than
		// be re-judged against a control revision that may have moved since.
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "resume-requirement"); e != nil {
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
		// The ControlTarget's RepositoryID comes from the Requirement's own
		// link, read inside this same transaction, exactly as StartFraming,
		// CompleteFraming and Plan resolve it. A Requirement with no link
		// yields an empty RepositoryID, which is not an error: the association
		// is optional, and ServiceConfig deliberately holds no repository id.
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
			Kind:            domain.RequirementResume,
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
		out = ResumeRequirementResponse{RequirementID: req.RequirementID, Status: next.Status, Version: next.Version}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "resume-requirement", "requirement", req.RequirementID, next.Version, "requirement.resumed", actor.String(), nil, out)
	})
	return out, err
}

// CancelRequirement records the owner's decision that this Requirement will not
// be solved, moving it to the terminal cancelled status and clearing any pause
// memory.
//
// Roles: RoleOwner ONLY, for the reason PauseRequirement gives.
//
// It evaluates NO domain.Permit, so it is ALLOWED under all seven control
// modes, and that is the property that makes the pause safe to ship: under
// emergency-stop a paused Requirement's resume is denied and its cancel is
// allowed, so the paused state is never a dead end.
//
// The source set is the domain's, NOT narrowed here: internal/domain/model.go's
// cancel branch admits every status except completed and cancelled. Two cells
// of docs/architecture/domain-model.md's table are narrower than that -- :270
// omits `cancelled` from recovering's 主な遷移先 and :271 omits it from
// evaluating's -- and the gap is RECORDED for the tech lead, not repaired.
// docs/product/definition.md:147 describes cancel unconditionally, with no
// source-status qualifier at all, and a cancel refused from `recovering` would
// be a stop the owner cannot issue in the state where a stop is most likely
// wanted. Asserting the gap here would pin it in place and turn its repair into
// a failure in a file the repairing task may not own, which is the reason
// internal/application/framing_test.go:552-561 gives for the identical choice.
func (s *Service) CancelRequirement(ctx context.Context, req CancelRequirementRequest) (out CancelRequirementResponse, err error) {
	_, actor, err := callerActor(ctx, RoleOwner)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("cancel-requirement", req)
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
	err = s.mutate(ctx, "cancel-requirement:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "cancel-requirement"); e != nil {
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
		at, e := transactionAuthorityTime(ctx, u)
		if e != nil {
			return e
		}
		next, e := domain.DecideRequirement(r, domain.RequirementCommand{
			Kind:            domain.RequirementCancel,
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
		out = CancelRequirementResponse{RequirementID: req.RequirementID, Status: next.Status, Version: next.Version}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "cancel-requirement", "requirement", req.RequirementID, next.Version, "requirement.cancelled", actor.String(), nil, out)
	})
	return out, err
}
