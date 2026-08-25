package provider

// Provider-neutral handoff (V2-027 A14, dp-v2-027 d15).
//
// Before this task nothing converted a Handoff back into anything, so the
// type recorded an intention no code consumed and no test could catch losing.
// A handoff that silently dropped an Artifact produced a perfectly valid
// Handoff. The content digest is what turns Increment, Artifact and
// Verification preservation from a property nobody checks into a fail-closed
// one: after the change, a dropped Artifact produces a refusal.
//
// The attempt history exists because a handoff chain with no memory is
// exactly how a Loop ping-pongs A to B to A while both retry budgets read as
// untouched. It is closed-enum only -- three Provider names and one failure
// class per entry -- and bounded, and it carries no free text, no prompt, no
// response and no session identifier, because there is no field able to hold
// one.
//
// Everything here is a pure function over values already in the packet, so
// the package gains no import and stays a leaf.

import (
	"encoding/json"
	"errors"
)

// MaxHandoffHistory is the stated bound on the attempt history.
//
// The bound is two, and it is not arbitrary. With three Provider names and
// the rule that an Increment is never handed back to a Provider already tried
// for it, a chain can contain at most two handoffs: the first has two
// participants, the second adds the third, and a third handoff would have to
// return to a participant. The explicit length check below is therefore
// redundant with the revisit rule, and is kept so that the bound is enforced
// by a stated number as well as by an argument.
const MaxHandoffHistory = 2

var (
	// ErrHandoffContentChanged is returned when the carried facts do not
	// survive the conversion: the recomputed content digest disagrees with
	// the one the Handoff carries. It is a refusal, never a repair.
	ErrHandoffContentChanged = errors.New("handoff carried content did not survive the conversion")
	// ErrHandoffTargetNotSending is returned when the target Provider's
	// circuit is anything other than sending.
	ErrHandoffTargetNotSending = errors.New("handoff target is not a provider this loop is sending to")
	// ErrHandoffRevisit is returned when a chain would hand an Increment back
	// to a Provider already tried for it.
	ErrHandoffRevisit = errors.New("handoff would return an increment to a provider already tried for it")
	// ErrInvalidHandoff is returned for a structurally invalid Handoff.
	ErrInvalidHandoff = errors.New("invalid provider handoff")
)

// HandoffAttempt is one entry of the bounded attempt history. Every field is
// a closed-enum value: two Provider names from the closed set of three, and
// one FailureClass from the package's own declared taxonomy.
type HandoffAttempt struct {
	From  string       `json:"from"`
	To    string       `json:"to"`
	Class FailureClass `json:"class"`
}

// Handoff is the provider-neutral handoff. Its only string-typed fields are
// the contract version, the two Provider names and the two digests; a source
// guard asserts that, so a prompt, a response or free text has nowhere to go.
type Handoff struct {
	Version       string           `json:"version"`
	FromProvider  string           `json:"from_provider"`
	ToProvider    string           `json:"to_provider"`
	Packet        WorkPacket       `json:"packet"`
	Failure       Failure          `json:"failure,omitempty"`
	Usage         Usage            `json:"usage,omitempty"`
	OutputDigest  string           `json:"output_digest,omitempty"`
	ContentDigest string           `json:"content_digest"`
	History       []HandoffAttempt `json:"history,omitempty"`
}

// carriedFacts is the exact set of packet fields a handoff must preserve:
// the Increment it belongs to, the constraints and decisions that bound it,
// the Artifacts it produced, the Verification entries that judged them, and
// what is still unresolved. Marshalling it in this fixed field order over
// slices (never maps) makes the digest deterministic.
type carriedFacts struct {
	IncrementID  string         `json:"increment_id"`
	Constraints  []string       `json:"constraints"`
	Decisions    []Decision     `json:"decisions"`
	Artifacts    []Artifact     `json:"artifacts"`
	Verification []Verification `json:"verification"`
	Unresolved   []string       `json:"unresolved"`
}

// HandoffContentDigest is the canonical digest over the facts a handoff
// carries. Mutating any one of them changes it, which is what makes the loss
// of an Artifact or a Verification entry a refusal rather than a silence.
func HandoffContentDigest(packet WorkPacket) string {
	body, err := json.Marshal(carriedFacts{
		IncrementID:  packet.IncrementID,
		Constraints:  packet.Constraints,
		Decisions:    packet.Decisions,
		Artifacts:    packet.Artifacts,
		Verification: packet.Verification,
		Unresolved:   packet.Unresolved,
	})
	if err != nil {
		return ""
	}
	return digest(string(body))
}

// PrepareHandoff builds the first handoff of a chain. Its existing meaning is
// unchanged: the same arguments produce the same Handoff, with the same
// Packet, Usage, OutputDigest and Failure. It now additionally records the
// content digest and opens the attempt history.
func PrepareHandoff(from, to string, packet WorkPacket, result Result) (Handoff, error) {
	return prepareHandoff(from, to, packet, result)
}

func prepareHandoff(from, to string, packet WorkPacket, result Result) (Handoff, error) {
	if from == "" || to == "" || from == to {
		return Handoff{}, ErrInvalidRequest
	}
	if !IsProviderName(from) || !IsProviderName(to) {
		return Handoff{}, ErrUnknownProvider
	}
	if err := packet.Validate(); err != nil {
		return Handoff{}, err
	}
	if result.Provider != "" && result.Provider != from {
		return Handoff{}, errors.New("handoff provider mismatch")
	}
	handoff := Handoff{
		Version:       ContractVersion,
		FromProvider:  from,
		ToProvider:    to,
		Packet:        packet,
		Usage:         result.Usage,
		OutputDigest:  result.OutputDigest,
		ContentDigest: HandoffContentDigest(packet),
	}
	attempt := HandoffAttempt{From: from, To: to}
	if result.Failure != nil {
		handoff.Failure = *result.Failure
		attempt.Class = result.Failure.Class
	}
	handoff.History = []HandoffAttempt{attempt}
	return handoff, nil
}

// ChainHandoff builds the second handoff of a chain from the first. It
// preserves the earlier attempts, including the first failure class, and
// refuses to hand the same Increment back to a Provider already tried for it.
func ChainHandoff(previous Handoff, to string, packet WorkPacket, result Result) (Handoff, error) {
	if err := previous.Validate(); err != nil {
		return Handoff{}, err
	}
	handoff, err := prepareHandoff(previous.ToProvider, to, packet, result)
	if err != nil {
		return Handoff{}, err
	}
	if packet.IncrementID != previous.Packet.IncrementID {
		// A different Increment is a different question, so the history
		// starts again rather than carrying an unrelated Provider's turn.
		return handoff, nil
	}
	for _, attempt := range previous.History {
		if attempt.From == to || attempt.To == to {
			return Handoff{}, ErrHandoffRevisit
		}
	}
	history := make([]HandoffAttempt, 0, len(previous.History)+1)
	history = append(history, previous.History...)
	history = append(history, handoff.History[0])
	if len(history) > MaxHandoffHistory {
		return Handoff{}, ErrHandoffRevisit
	}
	handoff.History = history
	return handoff, nil
}

// Validate checks the Handoff structurally and fails closed on any loss of
// the facts it carries.
func (h Handoff) Validate() error {
	if h.Version != ContractVersion {
		return ErrInvalidHandoff
	}
	if !IsProviderName(h.FromProvider) || !IsProviderName(h.ToProvider) || h.FromProvider == h.ToProvider {
		return ErrUnknownProvider
	}
	if err := h.Packet.Validate(); err != nil {
		return err
	}
	if h.ContentDigest == "" || h.ContentDigest != HandoffContentDigest(h.Packet) {
		return ErrHandoffContentChanged
	}
	if len(h.History) == 0 || len(h.History) > MaxHandoffHistory {
		return ErrInvalidHandoff
	}
	last := h.History[len(h.History)-1]
	if last.From != h.FromProvider || last.To != h.ToProvider {
		return ErrInvalidHandoff
	}
	for index, attempt := range h.History {
		if !IsProviderName(attempt.From) || !IsProviderName(attempt.To) || attempt.From == attempt.To {
			return ErrUnknownProvider
		}
		if attempt.Class != "" {
			if _, err := ActionForFailureClass(attempt.Class); err != nil {
				return err
			}
		}
		if index < len(h.History)-1 && (attempt.From == h.ToProvider || attempt.To == h.ToProvider) {
			return ErrHandoffRevisit
		}
	}
	return nil
}

// RequestFromHandoff converts a Handoff back into a Request for the target
// adapter. It refuses -- it never repairs -- when the target is not one of
// the three names, when the recomputed content digest disagrees with the one
// the Handoff carries, when the target is a Provider already tried for this
// Increment, or when this Loop is not sending to the target.
//
// The Request it returns carries the packet and nothing else about the
// previous attempt: the target's own Result will carry the target's own
// Provider name and its own output digest, never the ones the Handoff
// carried.
func RequestFromHandoff(h Handoff, operationID, workspace string, breaker *Breaker) (Request, error) {
	if breaker == nil {
		return Request{}, ErrIncompleteDependencies
	}
	if err := h.Validate(); err != nil {
		return Request{}, err
	}
	report, err := breaker.Report(h.ToProvider)
	if err != nil {
		return Request{}, err
	}
	if report.State != CircuitSending {
		return Request{}, ErrHandoffTargetNotSending
	}
	request := Request{OperationID: operationID, Workspace: workspace, Packet: h.Packet}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}
