package provider

// Per-Provider usage window (V2-027 A10, dp-v2-027 d9).
//
// This window is a third, distinct object, and confusing it with either of
// the other two would be a category error:
//
//   - internal/quota is the Firestore daily free-tier reservation. Its unit
//     is Firestore reads, writes and deletes, its owner is the datastore, and
//     its failure meaning is "this Loop would exceed the free tier today".
//     This task does not edit it, and this window does not reuse it.
//   - the runner's cost ledger is a runner-local, owner-approved, fail-closed
//     runaway detector that decides, before exec, whether a process may start
//     at all. That authority is the ledger's alone. This window never decides
//     whether a process may start.
//   - this window counts what the adapter layer can see: invocations
//     attempted through the pool, and the token counts a Provider reported.
//     It is an input to breaker and pool selection and nothing else.
//
// The window never claims to know a subscription's remaining allowance. Its
// population is only what this Loop invoked, which is a strict subset of what
// the subscription was actually used for, so an "allowance remaining" claim
// built on it would be false in the only case that matters.
//
// A reported usage of zero and an unreported usage are different facts and
// are stored as different facts: an invocation that reported no usage object
// makes the token side of the window unknown, never zero consumption.
//
// No identifier and no emitted JSON key in this package contains usd, cost,
// price, billing, spend, budget or credit. A mechanical substring scan over
// the package's declared identifiers and over the marshalled keys of every
// type it emits asserts that. The window is not a monetary object, and the
// vocabulary ban is the only enforceable form of that.

import (
	"errors"
	"time"
)

// WindowState is the closed set of states a usage window may report.
type WindowState string

const (
	// WindowWithin means the window's counts are under the caller's ceiling.
	WindowWithin WindowState = "within-window"
	// WindowExhausted means a count has passed the caller's ceiling.
	WindowExhausted WindowState = "exhausted"
	// WindowUnknown means at least one invocation in this window reported no
	// usage at all, so the token side of the window cannot be established.
	WindowUnknown WindowState = "unknown"
)

var (
	// ErrInvalidWindow is returned when a window cannot be constructed.
	ErrInvalidWindow = errors.New("invalid provider usage window")
	// ErrWindowProviderMismatch is returned when a window is handed a
	// Result belonging to a different Provider.
	ErrWindowProviderMismatch = errors.New("usage window belongs to a different provider")
)

// WindowCeiling is supplied by the caller, not read from any approved record
// and not copied out of one. Both values must be at least one, so a window
// can never be constructed with an implicit "no ceiling" that would make
// exhaustion unreachable.
type WindowCeiling struct {
	Invocations int64
	TotalTokens int64
}

// UsageWindow counts, per Provider and per named window, the invocations the
// Loop attempted through the pool and the token counts the Provider
// reported.
type UsageWindow struct {
	provider      string
	name          string
	length        time.Duration
	start         time.Time
	generation    int64
	attempted     int64
	withUsage     int64
	withoutUsage  int64
	reportedIn    int64
	reportedOut   int64
	reportedTotal int64
	ceiling       WindowCeiling
}

// WindowReport is the emitted read model. It carries no ceiling value and no
// other threshold number: a reader is told which side of the line the window
// is on, never where the line is.
type WindowReport struct {
	Provider                        string      `json:"provider"`
	Window                          string      `json:"window"`
	WindowStart                     time.Time   `json:"window_start"`
	Generation                      int64       `json:"generation"`
	AttemptedInvocations            int64       `json:"attempted_invocations"`
	InvocationsWithUsageReported    int64       `json:"invocations_with_usage_reported"`
	InvocationsWithUsageNotReported int64       `json:"invocations_with_usage_not_reported"`
	ReportedInputTokens             int64       `json:"reported_input_tokens"`
	ReportedOutputTokens            int64       `json:"reported_output_tokens"`
	ReportedTotalTokens             int64       `json:"reported_total_tokens"`
	State                           WindowState `json:"state"`
	ObservationScope                string      `json:"observation_scope"`
}

// NewUsageWindow builds a window for one Provider. start is the caller's
// time; the window reads no clock.
func NewUsageWindow(providerName, windowName string, start time.Time, length time.Duration, ceiling WindowCeiling) (*UsageWindow, error) {
	if !IsProviderName(providerName) {
		return nil, ErrUnknownProvider
	}
	if windowName == "" || length <= 0 || start.IsZero() {
		return nil, ErrInvalidWindow
	}
	if ceiling.Invocations < 1 || ceiling.TotalTokens < 1 {
		return nil, ErrInvalidWindow
	}
	return &UsageWindow{provider: providerName, name: windowName, length: length, start: start, ceiling: ceiling}, nil
}

// Provider is the Provider this window belongs to.
func (w *UsageWindow) Provider() string { return w.provider }

// WindowName is the window's name. It is deliberately not called Name: a
// pre-existing assertion pins the set of types in this package that declare a
// Name method to exactly the three adapters, so that the closed Provider
// identity cannot gain a fourth member unnoticed. Adding a Name method here
// would have broken that pin for no gain.
func (w *UsageWindow) WindowName() string { return w.name }

// roll advances the window to the period containing now and resets the
// counts once per elapsed period. It is driven entirely by the caller's time
// argument; there is no stored clock and no timer.
func (w *UsageWindow) roll(now time.Time) {
	for !now.Before(w.start.Add(w.length)) {
		w.start = w.start.Add(w.length)
		w.generation++
		w.attempted = 0
		w.withUsage = 0
		w.withoutUsage = 0
		w.reportedIn = 0
		w.reportedOut = 0
		w.reportedTotal = 0
	}
}

// Generation is the number of rollovers that have happened by now. A
// breaker opened by an exhaustion class compares this value against the
// value it recorded when it opened, so elapsed time alone can never make an
// exhausted window read as fresh.
func (w *UsageWindow) Generation(now time.Time) int64 {
	w.roll(now)
	return w.generation
}

// RecordAttempt counts one invocation the Loop attempted through the pool.
func (w *UsageWindow) RecordAttempt(now time.Time) {
	w.roll(now)
	w.attempted++
}

// RecordResult folds one parsed Result into the window. A Result whose
// UsageReported is false increments the not-reported count and contributes
// no token counts at all, which is what keeps an unreported invocation from
// reading as zero consumption.
func (w *UsageWindow) RecordResult(r Result, now time.Time) error {
	if r.Provider != w.provider {
		return ErrWindowProviderMismatch
	}
	w.roll(now)
	if !r.UsageReported {
		w.withoutUsage++
		return nil
	}
	w.withUsage++
	w.reportedIn += r.Usage.InputTokens
	w.reportedOut += r.Usage.OutputTokens
	total := r.Usage.TotalTokens
	if total == 0 {
		total = r.Usage.InputTokens + r.Usage.OutputTokens
	}
	w.reportedTotal += total
	return nil
}

// Report is the window's read model at now.
func (w *UsageWindow) Report(now time.Time) WindowReport {
	w.roll(now)
	return WindowReport{
		Provider:                        w.provider,
		Window:                          w.name,
		WindowStart:                     w.start,
		Generation:                      w.generation,
		AttemptedInvocations:            w.attempted,
		InvocationsWithUsageReported:    w.withUsage,
		InvocationsWithUsageNotReported: w.withoutUsage,
		ReportedInputTokens:             w.reportedIn,
		ReportedOutputTokens:            w.reportedOut,
		ReportedTotalTokens:             w.reportedTotal,
		State:                           w.state(),
		ObservationScope:                ObservationScope,
	}
}

// State is the window's state at now.
func (w *UsageWindow) State(now time.Time) WindowState {
	w.roll(now)
	return w.state()
}

// state is the closed-set decision. Exhaustion is reached exactly one unit
// over the ceiling and never before. An attempted-invocation exhaustion is
// decided first, because it is established regardless of whether any
// Provider reported usage; only after that does an unreported usage make the
// token side unknown.
func (w *UsageWindow) state() WindowState {
	if w.attempted > w.ceiling.Invocations {
		return WindowExhausted
	}
	if w.withoutUsage == 0 && w.reportedTotal > w.ceiling.TotalTokens {
		return WindowExhausted
	}
	if w.withoutUsage > 0 {
		return WindowUnknown
	}
	return WindowWithin
}
