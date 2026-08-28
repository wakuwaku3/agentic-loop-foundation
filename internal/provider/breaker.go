package provider

// Circuit breaker (V2-027 A11-A13, dp-v2-027 d11-d14).
//
// The breaker switches on this package's own FailureClass -- the class the
// adapter actually produced -- and imports internal/domain not at all, so the
// provider component stays a leaf and the decision stays where the
// observation is. The mapping from the Loop-level taxonomy to the behaviour
// below is a declared table in breaker_test.go and in
// docs/operations/provider-adapters.md, never an import.
//
// Two things about this breaker are deliberate and are the whole point.
//
// First, the exported state names this Loop's own action -- sending,
// not-sending, probing -- and never the Provider's condition. The counted
// population is only invocations that passed through this Loop's own
// execution path, which is a strict subset of reality: a person running the
// same CLI by hand from a shell is invisible here. A field named "healthy"
// would therefore be false in the one case that matters, a Provider a person
// is using successfully while this Loop's own last attempts failed. Every
// not-sending and probing report carries the classes that produced it, the
// window the count was taken over, and an observation scope that says so in
// words.
//
// Second, closing is never a function of elapsed time alone. A time-only
// close is a retry loop with extra steps, and against a Provider whose usage
// window has not rolled over, a timed probe is a guaranteed second
// exhaustion that spends a real invocation to learn nothing.

import (
	"errors"
	"sort"
	"time"
)

// ObservationScope states, in words, which population the breaker and the
// usage window counted. It is attached to every report that says the Loop is
// not sending, so a reader cannot take the state as a claim about the
// Provider itself.
const ObservationScope = "only invocations that passed through this Loop's own execution path were counted; an invocation a person or another process started directly against the same CLI is not in this population and cannot be seen here"

// CircuitState is the closed set of states the breaker exports. Each names
// what this Loop is doing, not what the Provider is.
type CircuitState string

const (
	// CircuitSending means this Loop will send work to the Provider.
	CircuitSending CircuitState = "sending"
	// CircuitNotSending means this Loop has decided not to send work.
	CircuitNotSending CircuitState = "not-sending"
	// CircuitProbing means this Loop is spending exactly one invocation to
	// find out whether to resume sending.
	CircuitProbing CircuitState = "probing"
)

// BreakerAction is the closed set of behaviours the d12 table assigns.
type BreakerAction string

const (
	// ActionOpenImmediately opens the Provider circuit on first occurrence.
	ActionOpenImmediately BreakerAction = "open-immediately"
	// ActionCountTowardWindowedThreshold counts toward a windowed count and
	// opens only once that count is reached.
	ActionCountTowardWindowedThreshold BreakerAction = "count-toward-windowed-threshold"
	// ActionOpenModelCircuitOnly opens the narrower Provider-and-model
	// circuit and leaves the Provider circuit closed, because the prescribed
	// action for this class is to evaluate other model candidates in the same
	// pool, which is impossible if the whole Provider is open.
	ActionOpenModelCircuitOnly BreakerAction = "open-provider-and-model-circuit-only"
	// ActionMoveSlotWithoutOpening moves the pool slot to a state only an
	// explicit external clearance leaves, and does not open the circuit.
	ActionMoveSlotWithoutOpening BreakerAction = "move-pool-slot-without-opening"
	// ActionNeitherCountsNorOpens does nothing to the circuit.
	ActionNeitherCountsNorOpens BreakerAction = "neither-counts-nor-opens"
)

var (
	// ErrInvalidBreakerPolicy is returned for a policy that cannot express
	// the d12 table.
	ErrInvalidBreakerPolicy = errors.New("invalid circuit breaker policy")
	// ErrUnmappedFailureClass is returned for a FailureClass with no row in
	// the opening table. It exists so that adding a class without adding a
	// row is a loud refusal rather than a silent default.
	ErrUnmappedFailureClass = errors.New("failure class has no row in the circuit breaker opening table")
	// ErrCircuitNotOpen is returned when a probe is attempted against a
	// circuit that is not open.
	ErrCircuitNotOpen = errors.New("circuit is not open")
	// ErrProbeNotEligible is returned for an open no probe can ever leave.
	ErrProbeNotEligible = errors.New("this open is not probe-eligible")
	// ErrWindowNotRolledOver is returned when an exhaustion-caused open has
	// waited out its cooldown but the usage window has not rolled over.
	// Elapsed time alone does not make an exhausted window fresh.
	ErrWindowNotRolledOver = errors.New("usage window has not rolled over since the circuit opened")
	// ErrIncompleteDependencies is returned when a pool, breaker or window
	// the call needs was not supplied. It fails closed: nothing is recorded
	// and no state moves.
	ErrIncompleteDependencies = errors.New("circuit breaker dependencies are incomplete")
)

// openingTable is the d12 mapping over this package's own class set. It is
// the single source of the behaviour, and a test asserts that its key set is
// exactly the set of FailureClass constants the package declares, measured
// from the AST rather than from a second copy of the list.
var openingTable = map[FailureClass]BreakerAction{
	// An explicit exhaustion signal needs no rate: it already says the next
	// attempt will fail.
	FailureQuota: ActionOpenImmediately,
	// An output shape the adapter cannot parse will not parse on a retry.
	FailureContract: ActionOpenImmediately,
	// The ordinary Provider-side fault, after the bounded retry.
	FailureTransport: ActionCountTowardWindowedThreshold,
	// A timeout is not a failure, it is an unknown result, so it counts but
	// marks any resulting open ambiguous.
	FailureTimeout: ActionCountTowardWindowedThreshold,
	// Unknown counts at a strictly higher count than transport and never on
	// a first occurrence.
	FailureUnknown: ActionCountTowardWindowedThreshold,
	// Evaluate other model candidates in the same pool: the narrower circuit.
	FailureModel: ActionOpenModelCircuitOnly,
	// Retrying cannot produce a session; the required action is the owner's.
	FailureUnauthenticated: ActionMoveSlotWithoutOpening,
	// We stopped it. It is not an observation about the Provider.
	FailureCancelled: ActionNeitherCountsNorOpens,
	// It is about our request, not about the Provider.
	FailureInvalidInput: ActionNeitherCountsNorOpens,
}

// ActionForFailureClass returns the d12 row for one class, and refuses a
// class with no row rather than defaulting.
func ActionForFailureClass(class FailureClass) (BreakerAction, error) {
	action, present := openingTable[class]
	if !present {
		return "", ErrUnmappedFailureClass
	}
	return action, nil
}

// OpeningTableClasses returns the classes the opening table covers, sorted,
// so a test can compare it against the classes the package declares.
func OpeningTableClasses() []FailureClass {
	classes := make([]FailureClass, 0, len(openingTable))
	for class := range openingTable {
		classes = append(classes, class)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	return classes
}

// BreakerPolicy is supplied by the caller. No value here is copied out of an
// runtime invocation policy, and no value here is emitted in any
// report.
type BreakerPolicy struct {
	// TransportCount is the windowed count at which a transport-class or
	// timeout-class run of failures opens the circuit.
	TransportCount int
	// UnknownCount is the windowed count at which unknown-class failures
	// open the circuit. It must be strictly greater than TransportCount,
	// which also makes it at least two, so unknown never opens on a first
	// occurrence.
	UnknownCount int
	// Cooldown is the first cooldown length.
	Cooldown time.Duration
	// CooldownMultiplier multiplies the cooldown after a failed probe.
	CooldownMultiplier int
	// CooldownCeiling is the declared ceiling the multiplied cooldown never
	// passes.
	CooldownCeiling time.Duration
}

func (p BreakerPolicy) validate() error {
	if p.TransportCount < 1 || p.UnknownCount <= p.TransportCount {
		return ErrInvalidBreakerPolicy
	}
	if p.Cooldown <= 0 || p.CooldownCeiling < p.Cooldown || p.CooldownMultiplier < 2 {
		return ErrInvalidBreakerPolicy
	}
	return nil
}

type circuitPhase string

const (
	phaseClosed   circuitPhase = "closed"
	phaseOpen     circuitPhase = "open"
	phaseHalfOpen circuitPhase = "half-open"
)

type circuit struct {
	phase            circuitPhase
	because          map[FailureClass]bool
	window           string
	transportCount   int
	unknownCount     int
	cooldown         time.Duration
	deadline         time.Time
	ambiguous        bool
	openedByQuota    bool
	openedByContract bool
	windowGeneration int64
	cliVersionAtOpen string
	probe            Probe
	probeHeld        bool
}

// Breaker holds one circuit per Provider name plus the narrower
// Provider-and-model circuits.
type Breaker struct {
	policy   BreakerPolicy
	circuits map[string]*circuit
	models   map[string]map[string]bool
}

// NewBreaker builds a breaker with one circuit per Provider name.
func NewBreaker(policy BreakerPolicy) (*Breaker, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	breaker := &Breaker{policy: policy, circuits: map[string]*circuit{}, models: map[string]map[string]bool{}}
	for _, name := range PoolNames() {
		breaker.circuits[name] = &circuit{phase: phaseClosed, because: map[FailureClass]bool{}, cooldown: policy.Cooldown}
		breaker.models[name] = map[string]bool{}
	}
	return breaker, nil
}

// CircuitReport is the exported read model. For a sending circuit the
// evidence fields are empty, because there is nothing this Loop declined to
// do; for anything else they are always populated.
type CircuitReport struct {
	Provider         string         `json:"provider"`
	State            CircuitState   `json:"state"`
	Because          []FailureClass `json:"because,omitempty"`
	Window           string         `json:"window,omitempty"`
	ObservationScope string         `json:"observation_scope,omitempty"`
	Ambiguous        bool           `json:"ambiguous,omitempty"`
}

// Report is the circuit's state for one Provider.
func (b *Breaker) Report(providerName string) (CircuitReport, error) {
	c, present := b.circuits[providerName]
	if !present {
		return CircuitReport{}, ErrUnknownProvider
	}
	report := CircuitReport{Provider: providerName}
	switch c.phase {
	case phaseClosed:
		report.State = CircuitSending
		return report, nil
	case phaseHalfOpen:
		report.State = CircuitProbing
	default:
		report.State = CircuitNotSending
	}
	report.Because = sortedClasses(c.because)
	report.Window = c.window
	report.ObservationScope = ObservationScope
	report.Ambiguous = c.ambiguous
	return report, nil
}

// ModelState reports the narrower Provider-and-model circuit.
func (b *Breaker) ModelState(providerName, model string) (CircuitState, error) {
	models, present := b.models[providerName]
	if !present {
		return "", ErrUnknownProvider
	}
	if models[model] {
		return CircuitNotSending, nil
	}
	return CircuitSending, nil
}

func sortedClasses(set map[FailureClass]bool) []FailureClass {
	classes := make([]FailureClass, 0, len(set))
	for class := range set {
		classes = append(classes, class)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	if len(classes) == 0 {
		return nil
	}
	return classes
}

// Observed is one reported invocation outcome, as the adapter layer saw it.
type Observed struct {
	// Provider is one of the three adapter names.
	Provider string
	// Model, when set, is the model the invocation asked for. It selects the
	// narrower circuit for a model-class failure.
	Model string
	// Window names the window the counts are taken over. It is recorded on
	// the report so a reader knows what population produced the decision.
	Window string
	// DeclaredCLIVersion is the CLI version the invocation declared. It is
	// recorded when a contract-incompatible open happens, because an
	// explicitly observed change of that value is the only thing that closes
	// such an open.
	DeclaredCLIVersion string
	// Failure is the adapter's own classification.
	Failure Failure
}

// Observation is what one reported failure did, for the caller and for a
// record. It carries no count and no threshold.
type Observation struct {
	Provider  string        `json:"provider"`
	Class     FailureClass  `json:"class"`
	Action    BreakerAction `json:"action"`
	State     CircuitState  `json:"state"`
	SlotState SlotState     `json:"slot_state"`
}

// ApplyObservation routes one reported failure to the breaker and to the pool
// according to the d12 table. It is the single entry point, so no caller can
// apply half of a row.
//
// window is read, never written: an exhaustion-caused open records the
// window's current generation so that the probe in Probe can require a real
// rollover rather than a mere passage of time.
func ApplyObservation(pool *Pool, breaker *Breaker, window *UsageWindow, obs Observed, now time.Time) (Observation, error) {
	if pool == nil || breaker == nil || window == nil {
		return Observation{}, ErrIncompleteDependencies
	}
	c, present := breaker.circuits[obs.Provider]
	if !present {
		return Observation{}, ErrUnknownProvider
	}
	if window.Provider() != obs.Provider {
		return Observation{}, ErrWindowProviderMismatch
	}
	action, err := ActionForFailureClass(obs.Failure.Class)
	if err != nil {
		return Observation{}, err
	}
	if err := pool.MarkFailure(obs.Provider, obs.Failure.Class); err != nil {
		return Observation{}, err
	}
	if obs.Window != "" {
		c.window = obs.Window
	}

	switch action {
	case ActionOpenImmediately:
		if obs.Failure.Class == FailureQuota {
			c.openedByQuota = true
		}
		if obs.Failure.Class == FailureContract {
			c.openedByContract = true
			c.cliVersionAtOpen = obs.DeclaredCLIVersion
		}
		breaker.open(pool, c, obs, window, now)
	case ActionCountTowardWindowedThreshold:
		if obs.Failure.Class == FailureUnknown {
			c.unknownCount++
		} else {
			c.transportCount++
		}
		if obs.Failure.Ambiguous || obs.Failure.Class == FailureTimeout {
			c.ambiguous = true
		}
		if c.transportCount >= breaker.policy.TransportCount || c.unknownCount >= breaker.policy.UnknownCount {
			breaker.open(pool, c, obs, window, now)
		}
	case ActionOpenModelCircuitOnly:
		breaker.models[obs.Provider][obs.Model] = true
	case ActionMoveSlotWithoutOpening:
		if err := pool.MarkUnauthenticated(obs.Provider); err != nil {
			return Observation{}, err
		}
	case ActionNeitherCountsNorOpens:
		// Deliberately nothing. Neither a cancellation we performed nor an
		// invalid request of ours is an observation about the Provider.
	}

	report, err := breaker.Report(obs.Provider)
	if err != nil {
		return Observation{}, err
	}
	slot, err := pool.Slot(obs.Provider)
	if err != nil {
		return Observation{}, err
	}
	return Observation{Provider: obs.Provider, Class: obs.Failure.Class, Action: action, State: report.State, SlotState: slot.State}, nil
}

// open puts the circuit into the open phase and moves the pool slot to
// cooling-down. It never overwrites a slot that needs an explicit external
// clearance: that refusal comes from the pool.
func (b *Breaker) open(pool *Pool, c *circuit, obs Observed, window *UsageWindow, now time.Time) {
	if c.phase == phaseClosed {
		c.cooldown = b.policy.Cooldown
	}
	c.phase = phaseOpen
	c.because[obs.Failure.Class] = true
	if obs.Failure.Ambiguous {
		c.ambiguous = true
	}
	c.windowGeneration = window.Generation(now)
	c.deadline = now.Add(c.cooldown)
	_ = pool.StartCooldown(obs.Provider, c.deadline, now)
}

// ApplySuccess reports a successful invocation observation. It is the only
// thing that closes a half-open circuit: issuing a probe does not.
func ApplySuccess(pool *Pool, breaker *Breaker, providerName string, now time.Time) error {
	if pool == nil || breaker == nil {
		return ErrIncompleteDependencies
	}
	c, present := breaker.circuits[providerName]
	if !present {
		return ErrUnknownProvider
	}
	if c.probeHeld {
		if err := pool.ReturnProbe(c.probe); err != nil {
			return err
		}
		c.probeHeld = false
		c.probe = Probe{}
	}
	c.phase = phaseClosed
	c.because = map[FailureClass]bool{}
	c.transportCount = 0
	c.unknownCount = 0
	c.ambiguous = false
	c.openedByQuota = false
	c.openedByContract = false
	c.cliVersionAtOpen = ""
	c.cooldown = breaker.policy.Cooldown
	c.deadline = time.Time{}
	return pool.MarkSuccess(providerName)
}

// Probe attempts the open-to-half-open transition. All of the following must
// hold, and each is driven by the caller's time argument:
//
//   - the circuit is open;
//   - the open is probe-eligible at all (a contract-incompatible open never
//     is: nothing about waiting changes either side's declared version);
//   - the cooldown deadline has passed under now;
//   - when the open was caused by an exhaustion class, the usage window has
//     also rolled over since the open;
//   - the pool can issue the single outstanding probe.
func (b *Breaker) Probe(pool *Pool, window *UsageWindow, providerName string, now time.Time) (Probe, error) {
	if pool == nil || window == nil {
		return Probe{}, ErrIncompleteDependencies
	}
	c, present := b.circuits[providerName]
	if !present {
		return Probe{}, ErrUnknownProvider
	}
	if window.Provider() != providerName {
		return Probe{}, ErrWindowProviderMismatch
	}
	if c.phase != phaseOpen {
		return Probe{}, ErrCircuitNotOpen
	}
	if c.openedByContract {
		return Probe{}, ErrProbeNotEligible
	}
	if now.Before(c.deadline) {
		return Probe{}, ErrCooldownNotElapsed
	}
	if c.openedByQuota && window.Generation(now) <= c.windowGeneration {
		return Probe{}, ErrWindowNotRolledOver
	}
	// The slot must be reachable before the probe is issued, so a refusal
	// never leaves an outstanding probe nobody can return. A quarantined or
	// stopped-for-inspection slot is refused here as well as by the pool.
	slot, err := pool.Slot(providerName)
	if err != nil {
		return Probe{}, err
	}
	if slot.State != SlotCoolingDown && slot.State != SlotAvailable {
		return Probe{}, ErrSlotNeedsExternalClearance
	}
	probe, err := pool.IssueProbe(providerName)
	if err != nil {
		return Probe{}, err
	}
	if slot.State == SlotCoolingDown {
		if err := pool.EndCooldown(providerName, now); err != nil {
			_ = pool.ReturnProbe(probe)
			return Probe{}, err
		}
	}
	c.probe = probe
	c.probeHeld = true
	c.phase = phaseHalfOpen
	return probe, nil
}

// ObserveProbeFailure records that the single probe failed. The cooldown is
// multiplied up to the declared ceiling and the counters are deliberately
// not reset: a failed probe is evidence the run of failures continues.
func (b *Breaker) ObserveProbeFailure(pool *Pool, providerName string, probe Probe, f Failure, now time.Time) error {
	if pool == nil {
		return ErrIncompleteDependencies
	}
	c, present := b.circuits[providerName]
	if !present {
		return ErrUnknownProvider
	}
	if !c.probeHeld || probe != c.probe {
		return ErrProbeMismatch
	}
	if err := pool.ReturnProbe(probe); err != nil {
		return err
	}
	c.probeHeld = false
	c.probe = Probe{}
	c.phase = phaseOpen
	if f.Class != "" {
		c.because[f.Class] = true
	}
	if f.Ambiguous {
		c.ambiguous = true
	}
	next := c.cooldown * time.Duration(b.policy.CooldownMultiplier)
	if next > b.policy.CooldownCeiling {
		next = b.policy.CooldownCeiling
	}
	c.cooldown = next
	c.deadline = now.Add(c.cooldown)
	if err := pool.MarkFailure(providerName, f.Class); err != nil {
		return err
	}
	return pool.StartCooldown(providerName, c.deadline, now)
}

// Cooldown reports the current cooldown length for one Provider. It exists so
// a test can assert the multiplication and the ceiling; it is not part of any
// emitted read model.
func (b *Breaker) Cooldown(providerName string) (time.Duration, error) {
	c, present := b.circuits[providerName]
	if !present {
		return 0, ErrUnknownProvider
	}
	return c.cooldown, nil
}

// ObserveDeclaredCLIVersion is the only thing that closes a
// contract-incompatible open. It closes the circuit when, and only when, the
// declared version differs from the one recorded when the circuit opened.
func (b *Breaker) ObserveDeclaredCLIVersion(pool *Pool, providerName, version string) (bool, error) {
	if pool == nil {
		return false, ErrIncompleteDependencies
	}
	c, present := b.circuits[providerName]
	if !present {
		return false, ErrUnknownProvider
	}
	if !c.openedByContract || c.phase == phaseClosed {
		return false, ErrCircuitNotOpen
	}
	if version == "" || version == c.cliVersionAtOpen {
		return false, nil
	}
	c.phase = phaseClosed
	c.because = map[FailureClass]bool{}
	c.openedByContract = false
	c.openedByQuota = false
	c.cliVersionAtOpen = ""
	c.transportCount = 0
	c.unknownCount = 0
	c.ambiguous = false
	c.cooldown = b.policy.Cooldown
	c.deadline = time.Time{}
	// An explicitly observed version change is an external clearance, so the
	// slot is cleared rather than waiting out a deadline that means nothing
	// here. A slot that is not merely cooling down is left alone: only its
	// own clearance may move it.
	slot, err := pool.Slot(providerName)
	if err != nil {
		return false, err
	}
	if slot.State == SlotCoolingDown {
		if err := pool.Clear(providerName); err != nil {
			return false, err
		}
	}
	return true, nil
}
