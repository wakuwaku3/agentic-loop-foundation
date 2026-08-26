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

// ===========================================================================
// The Sendable predicate, the handoff trigger and the single-producer decision
// (V2-074 A7-A10, dp-v2-074 d5-d8, d12)
// ===========================================================================
//
// WHAT THE TRIGGER IS, AND WHY IT IS NOT ANY OF THE THREE OBVIOUS ALTERNATIVES.
// The trigger is the negation of a declared Sendable predicate over three
// inputs -- the breaker's circuit state, the usage window's state and the pool
// slot's state -- evaluated for the SOURCE Provider. Each rejected alternative
// fails on a specific row of a table already in this package:
//
//   - A FailureClass alone contradicts three rows of the d12 opening table.
//     cancelled and invalid-input are declared there as not observations about
//     the Provider and must never move work. transport, timeout and unknown
//     are count-toward-windowed-threshold, so handing off on a first
//     occurrence would defeat the very threshold that exists to avoid
//     reacting to one blip. model opens only the narrower
//     Provider-and-model circuit, whose prescribed action is to evaluate other
//     model candidates in the SAME pool -- impossible if the whole Provider
//     moved.
//   - The circuit alone is necessary but not sufficient. Exhausting our own
//     attempt ceiling is not a Provider fault and produces no FailureClass, so
//     the window's exhausted state would reach nothing.
//   - The window alone is insufficient and dangerous, because WindowUnknown
//     means an invocation reported no usage at all, and rounding unknown to
//     exhausted would move work on an absence of information.
//
// WHY THE WINDOW IS NOT ROUTED THROUGH THE BREAKER (d6). Opening a circuit
// requires a FailureClass: open() records obs.Failure.Class in the circuit's
// because set, and every report carries those classes as its stated reason. So
// routing an own-ceiling exhaustion through the breaker would require
// inventing a class for it, and every candidate class is a lie -- quota would
// claim the Provider signalled exhaustion when it did not, unknown would claim
// we could not classify something we classified exactly, and a new class would
// need a row in the opening table and would then appear in Because as though
// the Provider had done something. Sendable therefore reads the window
// directly, alongside the circuit. No FailureClass is synthesized anywhere
// below, and ApplyObservation is not touched.
//
// WHY THE SLOT IS AN INPUT (d7). A measured defect: NewBreaker creates every
// circuit closed regardless of the slot, Report maps closed to
// CircuitSending, and ApplyObservation's ActionMoveSlotWithoutOpening branch
// touches only the pool. So after an observed provider-unauthenticated failure
// the slot is SlotUnauthenticated while the circuit still reports sending, and
// RequestFromHandoff -- which consults only the breaker -- accepts it. That is
// how the existing code path can hand work to a Provider with no authenticated
// session. Sendable reads the slot, and DecideHandoff is the only producer of
// a handoff target, which closes it at the decision. RequestFromHandoff keeps
// its signature and every one of its verdicts: widening its parameters would
// stop the existing call sites in handoff_test.go compiling, so those tests
// could not keep passing under their own names with their own assertions
// unchanged. The residual -- a caller who hand-picks a target and calls
// RequestFromHandoff directly -- is recorded rather than hidden, and an AST
// single-caller guard is what makes it unreachable in this tree. A guard is
// weaker than a signature, and that is stated rather than glossed.
//
// PROBING IS A WAIT, NOT A HANDOFF. A probing circuit is this Loop spending
// exactly one invocation to find out whether to resume sending. It is not
// sending, so Sendable is false -- but moving work while the answer is being
// bought would throw away the invocation that was spent to get it. The
// disposition for a probing source is therefore waiting, with its own reason.

// CircuitStates is the closed set of circuit states, in declaration order. It
// is declared here rather than in breaker.go because breaker.go is read-only
// for V2-074; a source guard compares this list against the CircuitState
// constants read from this package's own AST, so a member added without being
// listed fails a test rather than silently falling outside the Sendable table.
func CircuitStates() []CircuitState {
	return []CircuitState{CircuitSending, CircuitNotSending, CircuitProbing}
}

// WindowStates is the closed set of usage-window states, same discipline.
func WindowStates() []WindowState {
	return []WindowState{WindowWithin, WindowExhausted, WindowUnknown}
}

// SlotStates is the closed set of pool slot states, same discipline.
func SlotStates() []SlotState {
	return []SlotState{
		SlotAvailable, SlotInUse, SlotCoolingDown,
		SlotUnauthenticated, SlotQuarantined, SlotStoppedForInspection,
	}
}

// ErrUndeclaredState is returned when one of the three inputs is not a member
// of its own closed set. It is a refusal rather than a default branch: a state
// this package does not declare has no row, and inventing one would make the
// table total by guessing.
var ErrUndeclaredState = errors.New("state is not a declared member of its closed set")

// Sendable is the declared predicate over the three inputs. It is the
// conjunction of three per-axis predicates, each an exhaustive switch over its
// own closed set with no default branch: an undeclared member is refused, not
// defaulted. TestSendableIsTotalOverTheFullCrossProduct enumerates the full
// 3 x 3 x 6 product rather than sampling it.
func Sendable(circuit CircuitState, window WindowState, slot SlotState) (bool, error) {
	circuitSends, err := circuitIsSending(circuit)
	if err != nil {
		return false, err
	}
	windowAllows, err := windowIsNotExhausted(window)
	if err != nil {
		return false, err
	}
	slotAllows, err := slotNeedsNoOwnerActionAndIsReachable(slot)
	if err != nil {
		return false, err
	}
	return circuitSends && windowAllows && slotAllows, nil
}

func circuitIsSending(circuit CircuitState) (bool, error) {
	switch circuit {
	case CircuitSending:
		return true, nil
	case CircuitNotSending:
		return false, nil
	case CircuitProbing:
		// Probing is not sending. The disposition layer turns this into a wait
		// rather than a handoff; see the file comment.
		return false, nil
	}
	return false, ErrUndeclaredState
}

func windowIsNotExhausted(window WindowState) (bool, error) {
	switch window {
	case WindowWithin:
		return true, nil
	case WindowUnknown:
		// An invocation reported no usage at all. That is an absence of
		// information, not an exhaustion, and rounding it to exhausted would
		// move work on nothing.
		return true, nil
	case WindowExhausted:
		return false, nil
	}
	return false, ErrUndeclaredState
}

func slotNeedsNoOwnerActionAndIsReachable(slot SlotState) (bool, error) {
	switch slot {
	case SlotAvailable:
		return true, nil
	case SlotInUse:
		// A lease is outstanding. The Loop is already using this Provider, so
		// the source holds rather than hands off.
		return true, nil
	case SlotCoolingDown:
		return false, nil
	case SlotUnauthenticated, SlotQuarantined, SlotStoppedForInspection:
		// Each of these needs an owner's action. Nothing the Loop can do moves
		// them, and this is the state the measured defect left reachable.
		return false, nil
	}
	return false, ErrUndeclaredState
}

// SourceState is the closed set of source-side outcomes the decision table is
// declared over. It is derived from the three inputs and is not a fourth input.
type SourceState string

const (
	// SourceSendable means Sendable holds for the source; nothing moves.
	SourceSendable SourceState = "sendable"
	// SourceProbing means Sendable does not hold and the reason is that this
	// Loop is currently spending one invocation to find out whether to resume.
	SourceProbing SourceState = "probing"
	// SourceNotSendable means Sendable does not hold for any other reason.
	SourceNotSendable SourceState = "not-sendable"
)

// SourceStates is the closed set in declaration order.
func SourceStates() []SourceState {
	return []SourceState{SourceSendable, SourceProbing, SourceNotSendable}
}

// SourceStateOf is the declared derivation. The order is fixed: Sendable
// first, so a sendable circuit is never reported as probing, and probing next,
// so a wait is never reported as a handoff trigger.
func SourceStateOf(circuit CircuitState, window WindowState, slot SlotState) (SourceState, error) {
	sendable, err := Sendable(circuit, window, slot)
	if err != nil {
		return "", err
	}
	if sendable {
		return SourceSendable, nil
	}
	if circuit == CircuitProbing {
		return SourceProbing, nil
	}
	return SourceNotSendable, nil
}

// CandidateObstacle is the closed set of reasons one candidate Provider is not
// a permissible handoff target. ObstacleNone means it is one.
type CandidateObstacle string

const (
	// ObstacleNone means the candidate passed every filter.
	ObstacleNone CandidateObstacle = "none"
	// ObstacleOwnerAction means the owner's standing authorization does not
	// cover the candidate, or its slot needs an owner's action. Both refusals
	// are acceptance, not preference (wo-v2-074 non_goal 2).
	ObstacleOwnerAction CandidateObstacle = "owner-action-needed"
	// ObstacleMeasuredIncompatible means the candidate's declared version was
	// measured outside its adapter's declared interval. A candidate whose
	// compatibility is UNKNOWN is deliberately not refused: unknown is not
	// rounded to incompatible anywhere in this task.
	ObstacleMeasuredIncompatible CandidateObstacle = "measured-incompatible"
	// ObstacleAlreadyTried means this Increment has already been handed to the
	// candidate. A chain with no memory is exactly how a Loop ping-pongs A to
	// B to A while both retry budgets read as untouched.
	ObstacleAlreadyTried CandidateObstacle = "already-tried-for-this-increment"
	// ObstacleNotSendable means Sendable does not hold for the candidate
	// either. The same predicate decides the source and the target, so one
	// handoff never consults two authorities.
	ObstacleNotSendable CandidateObstacle = "not-sendable"
)

// CandidateObstacles is the closed set in the declared PRECEDENCE order, most
// actionable first. The order is what makes the reported waiting reason
// deterministic when several candidates are refused for different reasons: the
// obstacle reported is the highest-precedence one actually observed, and an
// owner action is first because it is the only one a person can clear.
func CandidateObstacles() []CandidateObstacle {
	return []CandidateObstacle{
		ObstacleOwnerAction,
		ObstacleMeasuredIncompatible,
		ObstacleAlreadyTried,
		ObstacleNotSendable,
		ObstacleNone,
	}
}

// HandoffDisposition is the closed set of dispositions DecideHandoff returns.
type HandoffDisposition string

const (
	// DispositionNone means nothing needs to move.
	DispositionNone HandoffDisposition = "none"
	// DispositionHandoffProposed means a target was selected. It is a
	// PROPOSAL: nothing in this repository executes it, because no selection
	// loop exists yet on the production path. The evidence says so plainly.
	DispositionHandoffProposed HandoffDisposition = "handoff-proposed"
	// DispositionWaiting means work must move but nothing may receive it, and
	// exactly one closed waiting reason says why.
	DispositionWaiting HandoffDisposition = "waiting"
)

// HandoffDispositions is the closed set in declaration order.
func HandoffDispositions() []HandoffDisposition {
	return []HandoffDisposition{DispositionNone, DispositionHandoffProposed, DispositionWaiting}
}

// HandoffWaitingReason is the closed vocabulary of waiting reasons. Every
// member is derived mechanically as exactly one reason per cell of
// HandoffDecisionTable whose disposition is waiting: a waiting cell with no
// reason and a declared reason no cell can produce are both test failures.
//
// This vocabulary shares no member with the queue waiting reason V2-068
// introduced for shared resource allocation, and a test asserts that both ways
// round. Two enums with overlapping members is how an owner comes to read one
// as the other, and a single merged enum would have to answer questions from
// both domains.
type HandoffWaitingReason string

const (
	// WaitingSourceIsProbing is the probing wait: the answer is being bought
	// with exactly one invocation, and moving work would discard it.
	WaitingSourceIsProbing HandoffWaitingReason = "source-is-probing"
	// WaitingChainBoundReached is the stated bound on the attempt history.
	WaitingChainBoundReached HandoffWaitingReason = "chain-bound-reached"
	// WaitingCandidateNeedsAnOwnerAction is the refusal of an unauthorized
	// candidate and of one whose slot needs an owner's action.
	WaitingCandidateNeedsAnOwnerAction HandoffWaitingReason = "candidate-needs-an-owner-action"
	// WaitingCandidateIsMeasuredIncompatible is the declared-version refusal.
	WaitingCandidateIsMeasuredIncompatible HandoffWaitingReason = "candidate-is-measured-incompatible"
	// WaitingCandidateAlreadyTried is the attempt-history refusal.
	WaitingCandidateAlreadyTried HandoffWaitingReason = "candidate-already-tried-for-this-increment"
	// WaitingCandidateIsNotSendable is the Sendable refusal applied to the
	// candidate.
	WaitingCandidateIsNotSendable HandoffWaitingReason = "candidate-is-not-sendable"
)

// HandoffWaitingReasons is the closed set in declaration order.
func HandoffWaitingReasons() []HandoffWaitingReason {
	return []HandoffWaitingReason{
		WaitingSourceIsProbing,
		WaitingChainBoundReached,
		WaitingCandidateNeedsAnOwnerAction,
		WaitingCandidateIsMeasuredIncompatible,
		WaitingCandidateAlreadyTried,
		WaitingCandidateIsNotSendable,
	}
}

// waitingReasonForObstacle is the one-to-one mapping from a candidate obstacle
// to its waiting reason. ObstacleNone has no reason, because a cell whose
// candidate passed every filter is not a waiting cell.
func waitingReasonForObstacle(obstacle CandidateObstacle) (HandoffWaitingReason, bool) {
	switch obstacle {
	case ObstacleOwnerAction:
		return WaitingCandidateNeedsAnOwnerAction, true
	case ObstacleMeasuredIncompatible:
		return WaitingCandidateIsMeasuredIncompatible, true
	case ObstacleAlreadyTried:
		return WaitingCandidateAlreadyTried, true
	case ObstacleNotSendable:
		return WaitingCandidateIsNotSendable, true
	case ObstacleNone:
		return "", false
	}
	return "", false
}

// SelectionCell is one cell of the shared decision table: the normalised input
// tuple and the disposition and waiting reason it maps to. The tuple is
// deliberately normalised rather than raw, because the two packages that
// declare this table necessarily start from different observations --
// internal/provider sees a circuit, a window and a slot; internal/application
// sees health, blocked_reason, a runaway state, a concurrency exhaustion, a
// standing authorization and an assignment ring -- so only a normalised tuple
// can be compared by value.
type SelectionCell struct {
	Source            SourceState          `json:"source"`
	ChainBoundReached bool                 `json:"chain_bound_reached"`
	Obstacle          CandidateObstacle    `json:"obstacle"`
	Disposition       HandoffDisposition   `json:"disposition"`
	Reason            HandoffWaitingReason `json:"reason"`
}

// Row renders one cell in the canonical, comparable form
// "source|chain-bound|obstacle|disposition|reason". It exists so a second,
// independent declaration of this table in another package can be compared
// against this one cell by cell and byte for byte, without either package
// importing the other -- internal/application must not import
// internal/provider, so a shared type is not available.
func (c SelectionCell) Row() string {
	bound := "false"
	if c.ChainBoundReached {
		bound = "true"
	}
	return string(c.Source) + "|" + bound + "|" + string(c.Obstacle) + "|" + string(c.Disposition) + "|" + string(c.Reason)
}

// HandoffDecisionTable is the whole decision table, enumerated over the full
// cross product of the normalised tuple in a fixed order, so two independent
// declarations of it can be compared cell by cell and byte for byte.
func HandoffDecisionTable() []SelectionCell {
	sources := SourceStates()
	obstacles := CandidateObstacles()
	out := make([]SelectionCell, 0, len(sources)*2*len(obstacles))
	for _, source := range sources {
		for _, bound := range []bool{false, true} {
			for _, obstacle := range obstacles {
				disposition, reason := decideCell(source, bound, obstacle)
				out = append(out, SelectionCell{
					Source:            source,
					ChainBoundReached: bound,
					Obstacle:          obstacle,
					Disposition:       disposition,
					Reason:            reason,
				})
			}
		}
	}
	return out
}

// decideCell is the rule the table is generated from, stated once. The order of
// the branches is the whole of the judgement:
//
//	sendable            -> nothing moves, whatever the candidates look like;
//	probing             -> wait, because the answer is already being bought;
//	chain bound reached -> wait, because the stated bound is reached;
//	no obstacle         -> propose the candidate;
//	otherwise           -> wait, with that obstacle's one reason.
func decideCell(source SourceState, chainBoundReached bool, obstacle CandidateObstacle) (HandoffDisposition, HandoffWaitingReason) {
	switch source {
	case SourceSendable:
		return DispositionNone, ""
	case SourceProbing:
		return DispositionWaiting, WaitingSourceIsProbing
	case SourceNotSendable:
		if chainBoundReached {
			return DispositionWaiting, WaitingChainBoundReached
		}
		if obstacle == ObstacleNone {
			return DispositionHandoffProposed, ""
		}
		reason, waiting := waitingReasonForObstacle(obstacle)
		if !waiting {
			return DispositionWaiting, WaitingCandidateIsNotSendable
		}
		return DispositionWaiting, reason
	}
	// An undeclared SourceState never reaches here: SourceStateOf produces
	// only declared members and the closed-set test asserts the list is
	// exactly what the AST declares. Waiting is the fail-closed answer.
	return DispositionWaiting, WaitingCandidateIsNotSendable
}

// ProviderSelectionState is one Provider's declared state as the decision
// reads it. Every field is a closed-enum value, a boolean, or a version
// string; there is no field able to hold a prompt, a response, a session
// identifier or a credential value.
type ProviderSelectionState struct {
	// Name is one of the three adapter names.
	Name string
	// Authorized is whether the owner's standing authorization covers this
	// Provider. It is a fact about a record, never about a session.
	Authorized bool
	// Circuit, Window and Slot are the three Sendable inputs.
	Circuit CircuitState
	Window  WindowState
	Slot    SlotState
	// CLIVersionDeclared is the version an approved, digest-bound preflight
	// record measured for this machine's CLI, or empty for unknown.
	CLIVersionDeclared string
	// AlreadyTriedForIncrement is whether the Increment being decided about
	// has already been handed to this Provider. It is a fold over the bounded
	// attempt history the caller already holds; nothing here stores it.
	AlreadyTriedForIncrement bool
}

// HandoffDecision is the whole input to DecideHandoff.
type HandoffDecision struct {
	// Source is the Provider the work is on now.
	Source string
	// IncrementID is the Increment being decided about. It is carried so a
	// decision can be recorded against it; the revisit rule is expressed by
	// each candidate's AlreadyTriedForIncrement.
	IncrementID string
	// ObservedLoopVersion is the Loop's own release identity, or empty when
	// this process was given no explicit release source root and can report
	// none.
	ObservedLoopVersion string
	// ChainLength is how many handoffs this Increment has already taken.
	ChainLength int
	// States must cover exactly the three declared Provider names.
	States []ProviderSelectionState
}

// HandoffDecisionResult is what DecideHandoff returns. Target is non-empty
// exactly when Disposition is handoff-proposed, and Reason is non-empty
// exactly when it is waiting.
type HandoffDecisionResult struct {
	Disposition HandoffDisposition   `json:"disposition"`
	Target      string               `json:"target,omitempty"`
	Reason      HandoffWaitingReason `json:"reason,omitempty"`
	// Source is echoed so a recorded result names what it decided about.
	Source string `json:"source"`
}

// ErrIncompleteSelectionState is returned when the decision was not given
// exactly one state per declared Provider name. It fails closed: no target is
// produced and no disposition is guessed.
var ErrIncompleteSelectionState = errors.New("handoff decision was not given exactly one state per declared provider name")

// DecideHandoff is the ONLY function in this package that may produce a
// handoff target. That is the whole of the wiring: RequestFromDisposition is
// the only non-test caller of RequestFromHandoff, and it refuses anything
// whose target this function did not produce, so no handoff Request can be
// constructed without passing through here.
//
// It is deterministic. The candidate order is PoolNames -- the documented order
// the standing authorization record's enum uses -- and driving the same state
// twice produces byte-identical results.
func DecideHandoff(in HandoffDecision) (HandoffDecisionResult, error) {
	if !IsProviderName(in.Source) {
		return HandoffDecisionResult{}, ErrUnknownProvider
	}
	if in.ChainLength < 0 {
		return HandoffDecisionResult{}, ErrInvalidHandoff
	}
	byName := map[string]ProviderSelectionState{}
	for _, state := range in.States {
		if !IsProviderName(state.Name) {
			return HandoffDecisionResult{}, ErrUnknownProvider
		}
		if _, duplicate := byName[state.Name]; duplicate {
			return HandoffDecisionResult{}, ErrIncompleteSelectionState
		}
		byName[state.Name] = state
	}
	if len(byName) != len(PoolNames()) {
		return HandoffDecisionResult{}, ErrIncompleteSelectionState
	}

	source := byName[in.Source]
	sourceState, err := SourceStateOf(source.Circuit, source.Window, source.Slot)
	if err != nil {
		return HandoffDecisionResult{}, err
	}
	result := HandoffDecisionResult{Source: in.Source}
	if sourceState != SourceNotSendable {
		disposition, reason := decideCell(sourceState, false, ObstacleNone)
		result.Disposition = disposition
		result.Reason = reason
		return result, nil
	}

	chainBoundReached := in.ChainLength >= MaxHandoffHistory
	if chainBoundReached {
		disposition, reason := decideCell(sourceState, true, ObstacleNone)
		result.Disposition = disposition
		result.Reason = reason
		return result, nil
	}

	// The candidate scan, in the declared order, with the obstacle of each
	// candidate recorded so the reported reason is the highest-precedence one
	// actually observed rather than whichever candidate came last.
	observed := map[CandidateObstacle]bool{}
	for _, name := range PoolNames() {
		if name == in.Source {
			continue
		}
		candidate, present := byName[name]
		if !present {
			return HandoffDecisionResult{}, ErrIncompleteSelectionState
		}
		obstacle, err := CandidateObstacleFor(candidate, in.ObservedLoopVersion)
		if err != nil {
			return HandoffDecisionResult{}, err
		}
		if obstacle == ObstacleNone {
			disposition, reason := decideCell(sourceState, false, ObstacleNone)
			result.Disposition = disposition
			result.Reason = reason
			result.Target = name
			return result, nil
		}
		observed[obstacle] = true
	}
	for _, obstacle := range CandidateObstacles() {
		if obstacle == ObstacleNone || !observed[obstacle] {
			continue
		}
		disposition, reason := decideCell(sourceState, false, obstacle)
		result.Disposition = disposition
		result.Reason = reason
		return result, nil
	}
	// Unreachable while there are three names and the source is one of them:
	// two candidates always remain, and each yields an obstacle. Waiting on the
	// candidate axis is the fail-closed answer rather than a silent none.
	result.Disposition = DispositionWaiting
	result.Reason = WaitingCandidateIsNotSendable
	return result, nil
}

// CandidateObstacleFor is the declared per-candidate filter, evaluated in the
// declared precedence order. It is exported so the obstacle a candidate
// presents is readable on its own, and so the precedence is asserted rather
// than inferred.
//
// A candidate whose compatibility is UNKNOWN is not refused. Rounding unknown
// to incompatible would take a Provider out of service on an absence of
// information, and the observed-CLI side of the verdict is permanently unknown
// in this repository until a Runner client exists.
func CandidateObstacleFor(candidate ProviderSelectionState, observedLoopVersion string) (CandidateObstacle, error) {
	if !IsProviderName(candidate.Name) {
		return "", ErrUnknownProvider
	}
	sendable, err := Sendable(candidate.Circuit, candidate.Window, candidate.Slot)
	if err != nil {
		return "", err
	}
	if !candidate.Authorized || slotNeedsAnOwnerAction(candidate.Slot) {
		return ObstacleOwnerAction, nil
	}
	verdict, err := Compatibility(candidate.Name, ContractVersion, candidate.CLIVersionDeclared, observedLoopVersion)
	if err != nil {
		return "", err
	}
	if verdict == VerdictIncompatible {
		return ObstacleMeasuredIncompatible, nil
	}
	if candidate.AlreadyTriedForIncrement {
		return ObstacleAlreadyTried, nil
	}
	if !sendable {
		return ObstacleNotSendable, nil
	}
	return ObstacleNone, nil
}

// slotNeedsAnOwnerAction names the three slot states nothing the Loop can do
// moves. It is the same set the pool refuses an automatic clearance for.
func slotNeedsAnOwnerAction(slot SlotState) bool {
	return slot == SlotUnauthenticated || slot == SlotQuarantined || slot == SlotStoppedForInspection
}

// RequestFromDisposition is the ONLY non-test caller of RequestFromHandoff in
// this module, and the only path from a decision to a Request. It refuses --
// it never repairs -- when the disposition is not handoff-proposed, when the
// decision's target is not the Handoff's target, or when the source disagrees.
//
// This is the structural half of the wiring. RequestFromHandoff's own
// signature and verdicts are unchanged, so a caller who hand-picks a target
// could still reach it; an AST guard asserting exactly one non-test caller is
// what makes that unreachable in this tree, and a guard is weaker than a
// signature. Widening the signature was rejected for one measurable reason:
// it would stop the existing tests compiling under their own names.
func RequestFromDisposition(decision HandoffDecisionResult, h Handoff, operationID, workspace string, breaker *Breaker) (Request, error) {
	if decision.Disposition != DispositionHandoffProposed {
		return Request{}, ErrHandoffTargetNotSending
	}
	if decision.Target == "" || decision.Target != h.ToProvider {
		return Request{}, ErrInvalidHandoff
	}
	if decision.Source != "" && decision.Source != h.FromProvider {
		return Request{}, ErrInvalidHandoff
	}
	return RequestFromHandoff(h, operationID, workspace, breaker)
}
