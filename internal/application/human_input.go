package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ===========================================================================
// V2-065: the needs-input question surface.
// ===========================================================================
//
// The domain already models the needs-input status and both of its edges
// (domain.RequirementNeedInput is allowed from framing, active and
// evaluating; domain.RequirementReadyCommand is allowed from framing, waiting
// and needs-input). What was missing was every way to observe the question:
// no application command asked one, no route carried one and no read model
// reported one. Measured before any of this was written: nothing in
// internal/application called domain.DecideRequirement at all, so
// RequestHumanInput and AnswerHumanInput below are the first two commands in
// this layer that move a Requirement through its own transition table.
//
// internal/domain is not edited. The question is an application-owned record
// in its own keyed side table, exactly as ControlRequestedByRepository is,
// because domain.Requirement carries M1's closure proof and a free-text
// question with a bounded option list is not part of it.

// Bounds. Every free-text field is length-bounded and every list is
// count-bounded, following MaxPageSize's constant idiom in readmodels.go
// rather than inlining magic numbers at the check site. The values are
// deliberately small: a question the owner has to read is not a document, and
// a set of options the owner has to choose between is not a catalogue.
const (
	// MaxHumanInputQuestionLength bounds "what has to be decided".
	MaxHumanInputQuestionLength = 500
	// MaxHumanInputReasonLength bounds the prose that accompanies the closed
	// reason class.
	MaxHumanInputReasonLength = 1000
	// MaxHumanInputOptions bounds the option list. An ask with no option at
	// all is refused: an option-free question is not answerable.
	MaxHumanInputOptions = 8
	// MaxHumanInputOptionIDLength bounds one option's identifier.
	MaxHumanInputOptionIDLength = 64
	// MaxHumanInputOptionTextLength bounds one option's summary and its
	// impact independently.
	MaxHumanInputOptionTextLength = 300
	// MaxHumanInputScopeEntries bounds each scope list. The scope vocabulary
	// is closed, so this is a bound on repetition rather than on content.
	MaxHumanInputScopeEntries = 8
)

// HumanInputReasonClass is closed to exactly three values, derived
// mechanically from cap-human-input-request's declared user_action:
// 破壊的・不可逆な判断 -> destructive-irreversible, 費用・権限上限の変更 ->
// limit-change, 要求の本質的な曖昧さ -> ambiguous-requirement. A fourth class
// is rejected rather than stored, so "why the Loop could not decide" stays a
// value the owner console can group by instead of free text.
type HumanInputReasonClass string

const (
	ReasonDestructiveIrreversible HumanInputReasonClass = "destructive-irreversible"
	ReasonLimitChange             HumanInputReasonClass = "limit-change"
	ReasonAmbiguousRequirement    HumanInputReasonClass = "ambiguous-requirement"
)

// HumanInputReasonClasses is the closed set, in declaration order.
func HumanInputReasonClasses() []HumanInputReasonClass {
	return []HumanInputReasonClass{ReasonDestructiveIrreversible, ReasonLimitChange, ReasonAmbiguousRequirement}
}

// Valid reports whether v is one of the three declared classes.
func (v HumanInputReasonClass) Valid() bool {
	for _, c := range HumanInputReasonClasses() {
		if v == c {
			return true
		}
	}
	return false
}

// HumanInputScope is the closed vocabulary both scope lists draw from. It is
// a vocabulary rather than free text for one reason: the stopped scope is
// checked against what the system actually enforces, and a sentence cannot be
// checked. ScopeNewClaimsForThisRequirement and
// ScopeLeaseRenewalForThisRequirement are the two entries this task backs
// with enforcement -- Service.Claim and Service.Renew both refuse while the
// parent Requirement is in needs-input -- and RequestHumanInput refuses an
// ask that does not report them as stopped, so the displayed scope and the
// enforcement cannot drift apart.
type HumanInputScope string

const (
	// ScopeNewClaimsForThisRequirement: no new claim is issued for any
	// Increment of this Requirement. Enforced by Service.Claim.
	ScopeNewClaimsForThisRequirement HumanInputScope = "new-claims-for-this-requirement"
	// ScopeLeaseRenewalForThisRequirement: a lease held for an Increment of
	// this Requirement is not extended. Enforced by Service.Renew. A lease
	// that was already held when the question was asked lapses at its
	// existing ExpiresAt; nothing here revokes it early.
	ScopeLeaseRenewalForThisRequirement HumanInputScope = "lease-renewal-for-this-requirement"
	// ScopeOtherRequirements: work on other Requirements.
	ScopeOtherRequirements HumanInputScope = "other-requirements"
	// ScopeIntakeOfNewRequirements: capturing new Requirements.
	ScopeIntakeOfNewRequirements HumanInputScope = "intake-of-new-requirements"
	// ScopeOwnerReads: the owner's read surfaces.
	ScopeOwnerReads HumanInputScope = "owner-reads"
)

// HumanInputScopes is the closed vocabulary, in declaration order.
func HumanInputScopes() []HumanInputScope {
	return []HumanInputScope{
		ScopeNewClaimsForThisRequirement,
		ScopeLeaseRenewalForThisRequirement,
		ScopeOtherRequirements,
		ScopeIntakeOfNewRequirements,
		ScopeOwnerReads,
	}
}

// Valid reports whether v is in the closed vocabulary.
func (v HumanInputScope) Valid() bool {
	for _, s := range HumanInputScopes() {
		if v == s {
			return true
		}
	}
	return false
}

// EnforcedStoppedHumanInputScopes are the vocabulary entries this task backs
// with a refusal in the issuing path. An ask that does not report all of them
// as stopped is refused, which is what makes the displayed stop scope and the
// measured refusal agree by construction rather than by prose.
func EnforcedStoppedHumanInputScopes() []HumanInputScope {
	return []HumanInputScope{ScopeNewClaimsForThisRequirement, ScopeLeaseRenewalForThisRequirement}
}

// HumanInputOption is one answerable option. Impact is mandatory: the
// declaration's 選択肢と影響 is one field pair, so an option whose impact is
// empty is refused rather than stored as a choice with no stated consequence.
type HumanInputOption struct {
	OptionID string `json:"option_id"`
	Summary  string `json:"summary"`
	Impact   string `json:"impact"`
}

// HumanInputRequest is the application-owned record of one question and, once
// the owner has answered it, of that answer. It is not a domain aggregate: it
// has no status and no Version, and no domain rule consults it. The answer
// fields are the single documented difference from
// ControlRequestedByRepository's write-once-per-key behaviour: they are
// written by a second, later transaction on the same row, and an
// implementation must never let that second write erase the recorded
// question.
type HumanInputRequest struct {
	RequirementID   string                `json:"requirement_id"`
	Question        string                `json:"question"`
	ReasonClass     HumanInputReasonClass `json:"reason_class"`
	Reason          string                `json:"reason"`
	Options         []HumanInputOption    `json:"options"`
	StoppedScope    []HumanInputScope     `json:"stopped_scope"`
	ContinuingScope []HumanInputScope     `json:"continuing_scope"`
	AskedAt         time.Time             `json:"asked_at"`
	AskedBy         domain.RequestedBy    `json:"asked_by"`

	// The answer half. All three are pointers or omitempty strings and are
	// absent until the owner answers. They are pointers for the reason
	// V2-052 and V2-073 both found the hard way: encoding/json's omitempty
	// does NOT elide a zero struct or a zero time.Time, so a value-typed
	// answered_at would marshal as "0001-01-01T00:00:00Z" and a value-typed
	// answered_by as {} on every unanswered question -- a real-looking
	// instant and a real-looking attribution for an answer nobody gave.
	AnsweredAt       *time.Time          `json:"answered_at,omitempty"`
	AnsweredOptionID string              `json:"answered_option_id,omitempty"`
	AnsweredBy       *domain.RequestedBy `json:"answered_by,omitempty"`
}

// Answered reports whether the owner has answered this question.
func (v HumanInputRequest) Answered() bool {
	return v.AnsweredAt != nil && !v.AnsweredAt.IsZero() && v.AnsweredOptionID != ""
}

// SameAnswer reports whether other carries the identical answer half.
func (v HumanInputRequest) SameAnswer(other HumanInputRequest) bool {
	if v.AnsweredOptionID != other.AnsweredOptionID {
		return false
	}
	if (v.AnsweredAt == nil) != (other.AnsweredAt == nil) {
		return false
	}
	if v.AnsweredAt != nil && !v.AnsweredAt.Equal(*other.AnsweredAt) {
		return false
	}
	if (v.AnsweredBy == nil) != (other.AnsweredBy == nil) {
		return false
	}
	if v.AnsweredBy != nil && *v.AnsweredBy != *other.AnsweredBy {
		return false
	}
	return true
}

// Clone returns a deep copy. Every slice is copied, so a caller handed this
// record -- through the read model's pointer, for instance -- cannot reach
// the stored value through it.
func (v HumanInputRequest) Clone() HumanInputRequest {
	out := v
	out.Options = append([]HumanInputOption(nil), v.Options...)
	out.StoppedScope = append([]HumanInputScope(nil), v.StoppedScope...)
	out.ContinuingScope = append([]HumanInputScope(nil), v.ContinuingScope...)
	if v.AnsweredAt != nil {
		at := *v.AnsweredAt
		out.AnsweredAt = &at
	}
	if v.AnsweredBy != nil {
		by := *v.AnsweredBy
		out.AnsweredBy = &by
	}
	return out
}

// HasOption reports whether id is one of the recorded options.
func (v HumanInputRequest) HasOption(id string) bool {
	for _, o := range v.Options {
		if o.OptionID == id {
			return true
		}
	}
	return false
}

// StopsScope reports whether the recorded stopped scope names s.
func (v HumanInputRequest) StopsScope(s HumanInputScope) bool {
	for _, x := range v.StoppedScope {
		if x == s {
			return true
		}
	}
	return false
}

// SameQuestion reports whether other carries the identical question half of
// the record. The answer fields are excluded on purpose: they are what the
// second transaction is allowed to add.
func (v HumanInputRequest) SameQuestion(other HumanInputRequest) bool {
	if v.RequirementID != other.RequirementID || v.Question != other.Question ||
		v.ReasonClass != other.ReasonClass || v.Reason != other.Reason ||
		!v.AskedAt.Equal(other.AskedAt) || v.AskedBy != other.AskedBy {
		return false
	}
	if len(v.Options) != len(other.Options) || len(v.StoppedScope) != len(other.StoppedScope) || len(v.ContinuingScope) != len(other.ContinuingScope) {
		return false
	}
	for i := range v.Options {
		if v.Options[i] != other.Options[i] {
			return false
		}
	}
	for i := range v.StoppedScope {
		if v.StoppedScope[i] != other.StoppedScope[i] {
			return false
		}
	}
	for i := range v.ContinuingScope {
		if v.ContinuingScope[i] != other.ContinuingScope[i] {
			return false
		}
	}
	return true
}

// ErrInvalidHumanInputRequest is the malformed-question condition. It is
// returned before anything is staged, so a refused ask records nothing.
var ErrInvalidHumanInputRequest = errors.New("invalid human input request")

// ErrUnknownHumanInputOption is returned when an answer names an option the
// recorded question does not carry. There is no default option, so an unknown
// option is a refusal and never a fallback.
var ErrUnknownHumanInputOption = errors.New("option_id is not one of the recorded options")

// ErrAwaitingHumanInput is returned by Service.Claim and Service.Renew while
// the Increment's parent Requirement is in needs-input. It is the enforcement
// behind ScopeNewClaimsForThisRequirement and
// ScopeLeaseRenewalForThisRequirement.
var ErrAwaitingHumanInput = errors.New("the parent Requirement is waiting for human input")

func boundedText(field, value string, max int, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%w: %s is required", ErrInvalidHumanInputRequest, field)
		}
		return nil
	}
	if len(value) > max {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidHumanInputRequest, field, max)
	}
	return nil
}

func validateScopeList(field string, values []HumanInputScope) error {
	if len(values) == 0 {
		return fmt.Errorf("%w: %s must name at least one scope", ErrInvalidHumanInputRequest, field)
	}
	if len(values) > MaxHumanInputScopeEntries {
		return fmt.Errorf("%w: %s exceeds %d entries", ErrInvalidHumanInputRequest, field, MaxHumanInputScopeEntries)
	}
	seen := map[HumanInputScope]bool{}
	for _, v := range values {
		if !v.Valid() {
			return fmt.Errorf("%w: %s carries the unknown scope %q", ErrInvalidHumanInputRequest, field, string(v))
		}
		if seen[v] {
			return fmt.Errorf("%w: %s repeats the scope %q", ErrInvalidHumanInputRequest, field, string(v))
		}
		seen[v] = true
	}
	return nil
}

// ValidateHumanInputRequest is the single rule both store adapters and the
// service call, so a record cannot reach storage through one path with checks
// the other path would refuse.
func ValidateHumanInputRequest(v HumanInputRequest) error {
	if v.RequirementID == "" {
		return fmt.Errorf("%w: requirement_id is required", ErrInvalidHumanInputRequest)
	}
	if err := boundedText("question", v.Question, MaxHumanInputQuestionLength, true); err != nil {
		return err
	}
	if !v.ReasonClass.Valid() {
		return fmt.Errorf("%w: reason_class %q is outside the closed set", ErrInvalidHumanInputRequest, string(v.ReasonClass))
	}
	if err := boundedText("reason", v.Reason, MaxHumanInputReasonLength, true); err != nil {
		return err
	}
	if len(v.Options) == 0 {
		return fmt.Errorf("%w: at least one option is required", ErrInvalidHumanInputRequest)
	}
	if len(v.Options) > MaxHumanInputOptions {
		return fmt.Errorf("%w: options exceed %d entries", ErrInvalidHumanInputRequest, MaxHumanInputOptions)
	}
	seen := map[string]bool{}
	for _, o := range v.Options {
		if err := boundedText("option_id", o.OptionID, MaxHumanInputOptionIDLength, true); err != nil {
			return err
		}
		if seen[o.OptionID] {
			return fmt.Errorf("%w: option_id %q is repeated", ErrInvalidHumanInputRequest, o.OptionID)
		}
		seen[o.OptionID] = true
		if err := boundedText("summary", o.Summary, MaxHumanInputOptionTextLength, true); err != nil {
			return err
		}
		// The impact is mandatory: an option with no stated consequence is
		// exactly the shape that satisfies a schema while telling the owner
		// nothing.
		if err := boundedText("impact", o.Impact, MaxHumanInputOptionTextLength, true); err != nil {
			return err
		}
	}
	if err := validateScopeList("stopped_scope", v.StoppedScope); err != nil {
		return err
	}
	if err := validateScopeList("continuing_scope", v.ContinuingScope); err != nil {
		return err
	}
	for _, s := range v.ContinuingScope {
		if v.StopsScope(s) {
			return fmt.Errorf("%w: scope %q is reported as both stopped and continuing", ErrInvalidHumanInputRequest, string(s))
		}
	}
	for _, s := range EnforcedStoppedHumanInputScopes() {
		if !v.StopsScope(s) {
			return fmt.Errorf("%w: stopped_scope must report %q, which this Requirement's needs-input status actually enforces", ErrInvalidHumanInputRequest, string(s))
		}
	}
	if v.AskedAt.IsZero() {
		return fmt.Errorf("%w: asked_at is required", ErrInvalidHumanInputRequest)
	}
	if !v.AskedBy.Recorded() {
		return fmt.Errorf("%w: asked_by is required", ErrInvalidHumanInputRequest)
	}
	if err := boundedText("answered_option_id", v.AnsweredOptionID, MaxHumanInputOptionIDLength, false); err != nil {
		return err
	}
	if v.AnsweredOptionID != "" && !v.HasOption(v.AnsweredOptionID) {
		return fmt.Errorf("%w: answered_option_id %q is not one of the recorded options", ErrInvalidHumanInputRequest, v.AnsweredOptionID)
	}
	hasAt := v.AnsweredAt != nil && !v.AnsweredAt.IsZero()
	if (v.AnsweredOptionID == "") != !hasAt {
		return fmt.Errorf("%w: an answer must carry both answered_at and answered_option_id", ErrInvalidHumanInputRequest)
	}
	if v.AnsweredOptionID != "" && (v.AnsweredBy == nil || !v.AnsweredBy.Recorded()) {
		return fmt.Errorf("%w: answered_by is required for an answer", ErrInvalidHumanInputRequest)
	}
	if v.AnsweredOptionID == "" && v.AnsweredBy != nil {
		return fmt.Errorf("%w: answered_by is recorded for a question with no answer", ErrInvalidHumanInputRequest)
	}
	return nil
}

// askedBy maps an already-authenticated asking Caller to the domain's
// RequestedBy value. Both roles allowed to ask -- the Runner reporting what
// it could not decide and the scheduler deciding to stop -- are the Loop
// itself, so both map to domain.ActorTypeLoop with the caller's own component
// subject. It deliberately does not reuse requestedBy: that function's
// contract is owner-or-scheduler and widening it would change the attribution
// of every other command that calls it.
func askedBy(c Caller) (domain.RequestedBy, error) {
	switch c.Role {
	case RoleRunner, RoleScheduler:
		return domain.RequestedBy{ActorType: domain.ActorTypeLoop, Subject: c.Subject}, nil
	default:
		return domain.RequestedBy{}, ErrForbidden
	}
}

// RequestHumanInputRequest is the ask. The caller supplies the question, its
// closed reason class, the options with their impacts and both scope lists,
// plus the Requirement's expected version. It carries no timestamp field: the
// asked_at instant is the transaction's authority time, exactly as Capture's
// capture time is, so neither a Runner clock nor a caller can supply it.
type RequestHumanInputRequest struct {
	RequestID                  string
	RequirementID              string
	ExpectedRequirementVersion domain.Version
	Question                   string
	ReasonClass                HumanInputReasonClass
	Reason                     string
	Options                    []HumanInputOption
	StoppedScope               []HumanInputScope
	ContinuingScope            []HumanInputScope
}

type RequestHumanInputResponse struct {
	RequirementID string                   `json:"requirement_id"`
	Status        domain.RequirementStatus `json:"status"`
	Version       domain.Version           `json:"version"`
	AskedAt       time.Time                `json:"asked_at"`
	AskedBy       domain.RequestedBy       `json:"asked_by"`
}

// RequestHumanInput records the question and moves the Requirement into
// needs-input in the same transaction.
//
// Roles: runner and scheduler only. The owner is refused -- the owner answers
// questions, and letting the owner ask one on the Loop's behalf would make
// the recorded asker unusable as attribution.
//
// It evaluates no domain.Permit and stages no outbox item. Precedent for a
// permit-free command that changes canonical state: Plan and Prepare both do.
// The reason it must be permit-free is specific rather than convenient:
// asking a question only removes capability -- it stops work and stages no
// external effect -- so gating it behind a side-effect permit would leave the
// system unable to record why it stopped at exactly the moment a stop is in
// force.
func (s *Service) RequestHumanInput(ctx context.Context, req RequestHumanInputRequest) (out RequestHumanInputResponse, err error) {
	caller, actor, err := callerActor(ctx, RoleRunner, RoleScheduler)
	if err != nil {
		return out, err
	}
	asker, err := askedBy(caller)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("request-human-input", req)
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
	err = s.mutate(ctx, "request-human-input:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "request-human-input"); e != nil {
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
		askedAt, e := transactionAuthorityTime(ctx, u)
		if e != nil {
			return e
		}
		record := HumanInputRequest{
			RequirementID:   req.RequirementID,
			Question:        req.Question,
			ReasonClass:     req.ReasonClass,
			Reason:          req.Reason,
			Options:         append([]HumanInputOption(nil), req.Options...),
			StoppedScope:    append([]HumanInputScope(nil), req.StoppedScope...),
			ContinuingScope: append([]HumanInputScope(nil), req.ContinuingScope...),
			AskedAt:         askedAt,
			AskedBy:         asker,
		}
		if e = ValidateHumanInputRequest(record); e != nil {
			return e
		}
		next, e := domain.DecideRequirement(r, domain.RequirementCommand{
			Kind:            domain.RequirementNeedInput,
			Actor:           actor,
			At:              askedAt,
			ExpectedVersion: req.ExpectedRequirementVersion,
		})
		if e != nil {
			return e
		}
		if e = u.SaveRequirement(ctx, next, r.Version); e != nil {
			return e
		}
		if e = u.SaveHumanInputRequest(ctx, record); e != nil {
			return e
		}
		out = RequestHumanInputResponse{RequirementID: req.RequirementID, Status: next.Status, Version: next.Version, AskedAt: askedAt, AskedBy: asker}
		// No outbox item: nothing outside the control plane is asked to do
		// anything by a question.
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "request-human-input", "requirement", req.RequirementID, next.Version, "requirement.human-input-requested", actor.String(), nil, out)
	})
	return out, err
}

// AnswerHumanInputRequest is the answer. It carries the chosen option and the
// Requirement's expected version, and nothing else: there is no default
// option field, no expiry and no timeout, so no shape of this request can
// answer on the owner's behalf.
type AnswerHumanInputRequest struct {
	RequestID                  string
	RequirementID              string
	ExpectedRequirementVersion domain.Version
	OptionID                   string
}

type AnswerHumanInputResponse struct {
	RequirementID    string                   `json:"requirement_id"`
	Status           domain.RequirementStatus `json:"status"`
	Version          domain.Version           `json:"version"`
	AnsweredOptionID string                   `json:"answered_option_id"`
	AnsweredAt       time.Time                `json:"answered_at"`
	AnsweredBy       domain.RequestedBy       `json:"answered_by"`
}

// AnswerHumanInput resumes the same Requirement. Owner role only.
//
// It creates no Requirement: the answer is domain.RequirementReadyCommand
// applied to the same id, so the total number of Requirements cannot change.
// A second answer to an already-answered question is refused by that same
// transition table -- ready is not an allowed source for the ready command --
// and not by any flag this layer maintains.
//
// It evaluates domain.Permit with domain.PermitIntake against the
// installation target, exactly as Capture does, so a pause-intake or any stop
// mode denies resuming work for the same reason it denies taking new work in.
func (s *Service) AnswerHumanInput(ctx context.Context, req AnswerHumanInputRequest) (out AnswerHumanInputResponse, err error) {
	caller, actor, err := callerActor(ctx, RoleOwner)
	if err != nil {
		return out, err
	}
	answerer, err := requestedBy(caller)
	if err != nil {
		return out, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return out, err
	}
	fingerprint, err := requestFingerprint("answer-human-input", req)
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
	err = s.mutate(ctx, "answer-human-input:"+req.RequestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, req.RequestID, "answer-human-input"); e != nil {
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
		record, ok, e := u.HumanInputRequest(ctx, req.RequirementID)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("%w: no question was recorded for requirement %q", ErrNotFound, req.RequirementID)
		}
		if !record.HasOption(req.OptionID) {
			return fmt.Errorf("%w: %q", ErrUnknownHumanInputOption, req.OptionID)
		}
		answeredAt, e := transactionAuthorityTime(ctx, u)
		if e != nil {
			return e
		}
		link, hasLink, e := u.RequirementRepositoryLink(ctx, req.RequirementID)
		if e != nil {
			return e
		}
		repositoryID := ""
		if hasLink {
			repositoryID = link.RepositoryID.String()
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
		if _, e = domain.Permit(effective, domain.PermitRequest{Kind: domain.PermitIntake, Target: target, ControlRevision: revision, Resource: req.RequirementID}); e != nil {
			return e
		}
		next, e := domain.DecideRequirement(r, domain.RequirementCommand{
			Kind:            domain.RequirementReadyCommand,
			Actor:           actor,
			At:              answeredAt,
			ExpectedVersion: req.ExpectedRequirementVersion,
		})
		if e != nil {
			return e
		}
		if e = u.SaveRequirement(ctx, next, r.Version); e != nil {
			return e
		}
		// The answer is added to the recorded row. The question half is
		// carried through unchanged, and the store refuses any save that
		// would alter it.
		answered := record.Clone()
		at := answeredAt
		by := answerer
		answered.AnsweredAt = &at
		answered.AnsweredOptionID = req.OptionID
		answered.AnsweredBy = &by
		if e = ValidateHumanInputRequest(answered); e != nil {
			return e
		}
		if e = u.SaveHumanInputRequest(ctx, answered); e != nil {
			return e
		}
		out = AnswerHumanInputResponse{RequirementID: req.RequirementID, Status: next.Status, Version: next.Version, AnsweredOptionID: req.OptionID, AnsweredAt: answeredAt, AnsweredBy: answerer}
		return s.record(ctx, u, eventID, operationID, fingerprint, req.RequestID, "answer-human-input", "requirement", req.RequirementID, next.Version, "requirement.human-input-answered", actor.String(), nil, out)
	})
	return out, err
}

// requirementAwaitsHumanInput reports whether the Increment's parent
// Requirement is in needs-input. It is the one read both guards share, so
// Claim and Renew cannot disagree about what "waiting" means.
func requirementAwaitsHumanInput(ctx context.Context, u UnitOfWork, inc domain.Increment) (bool, error) {
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
	return r.Status == domain.RequirementNeedsInput, nil
}
