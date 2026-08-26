package application

import (
	"context"
	"errors"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// V2-089: claiming asks whether the Increment's parent Requirement is in a
// state that admits work.
//
// It already asked, for exactly one status: V2-065's guard in Service.Claim
// refuses when the parent is in needs-input, through
// requirementAwaitsHumanInput. This file widens that one question to the
// complement of the set that admits work, at the same line, in the same
// transaction, reusing the same parent read -- no new port, no new
// transaction, no second place where the parent is consulted.
//
// ErrRequirementNotClaimable is a STATE CONFLICT and not a control denial.
// internal/api classifies it on the same 409-conflict branch that already
// carries ErrAwaitingHumanInput, for the reason that branch's comment already
// gives: nothing the caller can rewrite makes the claim issuable while the
// parent is in that state. It is deliberately NOT ErrAwaitingHumanInput --
// a paused or cancelled Requirement is not waiting for a human answer, and
// saying so would be a lie the owner console would repeat -- and deliberately
// NOT domain.ErrControlDenied, which would put a state fact into the control
// plane's vocabulary and make it indistinguishable from a stop.
var ErrRequirementNotClaimable = errors.New("the parent Requirement is not in a state that admits work")

// requirementStatusAdmitsClaim reports whether a Requirement in this status may
// have one of its Increments claimed.
//
// THE SET IS DERIVED, NOT CHOSEN, and this switch is the ONLY place the four
// statuses are written out. "May be claimed" means "may legally be active once
// the claim lands", because docs/architecture/domain-model.md:266 defines
// `active` as "1つ以上のIncrementが進行中" -- one or more Increments in
// progress -- which is exactly what a claim produces, and
// docs/product/definition.md:111 requires a worker to claim an 実行可能
// Requirement. The domain already answers that question in exactly one place:
// internal/domain/model.go:485-489 admits domain.RequirementStart from
// RequirementReady, RequirementRecovering and RequirementWaiting. Add
// RequirementActive, which is already there and needs no transition, and the
// set is closed at four of eleven. So this switch is
//
//	{s : domain.DecideRequirement admits domain.RequirementStart from s} ∪ {active}
//
// and claimable_test.go asserts exactly that equivalence over the complete
// RequirementStatus axis derived from internal/domain/model.go by go/ast.
// internal/domain is not edited by this task, so widening this switch -- to
// `captured`, say, which would make every migrated fixture green with no
// fixture edits at all -- makes that assertion FAIL rather than making a
// decision.
//
// The product's own transition table agrees cell by cell for these four:
// domain-model.md:265 lists `active` among ready's 主な遷移先, :266 is
// `active` itself, :267 lists it for waiting and :270 for recovering. The
// table is NOT the authority, and it is two cells WIDER than the domain --
// :268 lists `active` for needs-input and :271 for evaluating. needs-input is
// the product's own counterexample to its own table: definition.md:143 defines
// pause claim as "新しい仕事のclaimを止める" and V2-065 shipped exactly that
// refusal for needs-input, so the product demonstrably does not read "the
// table lists active as a target" as "a claim is admitted". That two-cell gap
// is recorded for the tech_lead and asserted nowhere.
func requirementStatusAdmitsClaim(s domain.RequirementStatus) bool {
	switch s {
	case domain.RequirementReady, domain.RequirementActive, domain.RequirementWaiting, domain.RequirementRecovering:
		return true
	default:
		return false
	}
}

// requirementAdmitsClaim reads the Increment's parent Requirement inside the
// caller's transaction and reports whether that Requirement's status admits
// work.
//
// It REFUSES -- returns false -- for an Increment whose RequirementID is empty
// and for a parent that is not in the store. That is the opposite of what
// requirementAwaitsHumanInput answers for the same two cases, and it is why
// this is a SECOND helper rather than a widening of that one: "is the parent in
// needs-input" is correctly false for a record that cannot be read, while "is
// the parent in a state that admits work" is correctly false too --
// docs/product/definition.md:111's 実行可能 cannot be established for a record
// that does not exist. The two questions have opposite polarity on the unknown
// case, so one helper would have to get one of them wrong. Measured at V2-089:
// no fixture in the repository reaches either case, so both are asserted
// explicitly in claimable_test.go.
func requirementAdmitsClaim(ctx context.Context, u UnitOfWork, inc domain.Increment) (bool, error) {
	id := inc.RequirementID.String()
	if id == "" {
		return false, nil
	}
	r, ok, err := u.Requirement(ctx, id)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if !requirementStatusAdmitsClaim(r.Status) {
		tmpProbeParentStatus = string(r.Status)
	}
	return requirementStatusAdmitsClaim(r.Status), nil
}

// TEMPORARY V2-089 reproduction probe. Removed before the fixture migration.
var tmpProbeParentStatus string
