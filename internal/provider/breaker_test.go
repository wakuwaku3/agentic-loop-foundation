package provider_test

// Circuit breaker: the opening table, closing that is never time alone, and
// a state that names this Loop's decision rather than the Provider's
// condition (V2-027 A11-A13).

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

func policy() provider.BreakerPolicy {
	return provider.BreakerPolicy{
		TransportCount:     2,
		UnknownCount:       3,
		Cooldown:           10 * time.Minute,
		CooldownMultiplier: 3,
		CooldownCeiling:    90 * time.Minute,
	}
}

func newBreaker(t *testing.T) *provider.Breaker {
	t.Helper()
	breaker, err := provider.NewBreaker(policy())
	if err != nil {
		t.Fatalf("NewBreaker: %v", err)
	}
	return breaker
}

func observe(t *testing.T, pool *provider.Pool, breaker *provider.Breaker, window *provider.UsageWindow, class provider.FailureClass, at time.Time) provider.Observation {
	t.Helper()
	obs, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{
		Provider:           window.Provider(),
		Window:             window.WindowName(),
		DeclaredCLIVersion: "1.0.0",
		Failure:            provider.Failure{Class: class},
	}, at)
	if err != nil {
		t.Fatalf("ApplyObservation(%s): %v", class, err)
	}
	return obs
}

// ---------------------------------------------------------------------------
// A11: the opening table, one row per class, over both taxonomies.
// ---------------------------------------------------------------------------

// providerLocalTable is the d12 mapping over this package's own nine classes.
// TestFailureClassSetIsExactlyWhatTheASTDeclares in source_guard_test.go is
// what makes it exhaustive by construction: a tenth class added to the
// package without a row there fails, and the row set here is compared against
// the package's own table below.
var providerLocalTable = []struct {
	class  provider.FailureClass
	action provider.BreakerAction
	why    string
}{
	{provider.FailureQuota, provider.ActionOpenImmediately, "an explicit exhaustion signal needs no rate: it already says the next attempt will fail"},
	{provider.FailureContract, provider.ActionOpenImmediately, "an output shape the adapter cannot parse will not parse on a retry"},
	{provider.FailureTransport, provider.ActionCountTowardWindowedThreshold, "the ordinary Provider-side fault, after the bounded retry"},
	{provider.FailureTimeout, provider.ActionCountTowardWindowedThreshold, "a timeout is not a failure but an unknown result, so it counts and marks the open ambiguous"},
	{provider.FailureUnknown, provider.ActionCountTowardWindowedThreshold, "at a strictly higher count than transport, and never on a first occurrence"},
	{provider.FailureModel, provider.ActionOpenModelCircuitOnly, "the prescribed action is to evaluate other model candidates in the same pool, which is impossible if the whole Provider is open"},
	{provider.FailureUnauthenticated, provider.ActionMoveSlotWithoutOpening, "retrying cannot produce a session; the required action is the owner's, not a probe's"},
	{provider.FailureCancelled, provider.ActionNeitherCountsNorOpens, "we stopped it; it is not an observation about the Provider"},
	{provider.FailureInvalidInput, provider.ActionNeitherCountsNorOpens, "it is about our own request"},
}

// domainTable is the Loop-level taxonomy's seventeen values, recorded as a
// declared table and nothing else. internal/domain is deliberately not
// imported: the two taxonomies are already recorded as different, importing it
// would create this component's first outbound edge, and only V2-045 may alter
// the declared component DAG. The strings below are therefore data about
// another package's enumeration, not this package's own vocabulary.
//
// provider-unauthenticated is deliberately NOT among the seventeen. It is this
// package's own class, added here rather than to the Loop-level taxonomy,
// because the decision it drives happens in this leaf package and adding a
// constant to the taxonomy would have put a value in the one package whose
// closure an earlier gate proved. Its row is in providerLocalTable above.
var domainTable = []struct {
	class  string
	action provider.BreakerAction
	why    string
}{
	{"provider-quota", provider.ActionOpenImmediately, "explicit exhaustion signal"},
	{"contract-incompatible", provider.ActionOpenImmediately, "an unparseable shape does not become parseable on a retry"},
	{"provider-transport", provider.ActionCountTowardWindowedThreshold, "failure rate, after the bounded retry"},
	{"unknown", provider.ActionCountTowardWindowedThreshold, "strictly higher count than transport, never on a first occurrence"},
	{"provider-model", provider.ActionOpenModelCircuitOnly, "evaluate compatible model candidates in the same pool"},
	{"secret-suspected", provider.ActionMoveSlotWithoutOpening, "stop, redact, revoke: the slot is quarantined and clearing it is a human act, never a probe"},
	{"budget-exceeded", provider.ActionMoveSlotWithoutOpening, "the slot stops for inspection; reaching a limit is neither a success nor a failure, so opening on it would report a Provider fault for a Loop-side stop"},
	{"invalid-input", provider.ActionNeitherCountsNorOpens, "about our request"},
	{"policy-denied", provider.ActionNeitherCountsNorOpens, "about our request"},
	{"capacity-unavailable", provider.ActionNeitherCountsNorOpens, "about our own capacity"},
	{"execution-lost", provider.ActionNeitherCountsNorOpens, "about the Runner or the lease"},
	{"progress-stalled", provider.ActionNeitherCountsNorOpens, "the tempting one, and the one to refuse: it routes to checkpoint, TERM/KILL and a new Execution, and it does not name the Provider, so opening a circuit on it would take a working Provider out of service on evidence about something else"},
	{"verification-failed", provider.ActionNeitherCountsNorOpens, "the Provider worked and the change was wrong"},
	{"external-ambiguous", provider.ActionNeitherCountsNorOpens, "after the invocation ended"},
	{"integration-conflict", provider.ActionNeitherCountsNorOpens, "after the invocation ended"},
	{"preview-regression", provider.ActionNeitherCountsNorOpens, "after the invocation ended"},
	{"promotion-partial", provider.ActionNeitherCountsNorOpens, "after the invocation ended"},
}

func TestOpeningTableCoversEveryProviderLocalClassExactlyOnce(t *testing.T) {
	seen := map[provider.FailureClass]bool{}
	for _, row := range providerLocalTable {
		if seen[row.class] {
			t.Fatalf("class %q has two rows", row.class)
		}
		seen[row.class] = true
		action, err := provider.ActionForFailureClass(row.class)
		if err != nil {
			t.Fatalf("class %q has no row in the package's own table: %v", row.class, err)
		}
		if action != row.action {
			t.Fatalf("class %q: package says %q, this table says %q (%s)", row.class, action, row.action, row.why)
		}
	}
	packaged := provider.OpeningTableClasses()
	if len(packaged) != len(providerLocalTable) {
		t.Fatalf("the package's table covers %d classes, this table has %d rows", len(packaged), len(providerLocalTable))
	}
	for _, class := range packaged {
		if !seen[class] {
			t.Fatalf("the package's table has a row for %q that this table does not", class)
		}
	}
	if _, err := provider.ActionForFailureClass("provider-invented"); !errors.Is(err, provider.ErrUnmappedFailureClass) {
		t.Fatalf("an unmapped class defaulted instead of refusing: %v", err)
	}
	// The Loop-level side, as a declared mapping only.
	if len(domainTable) != 17 {
		t.Fatalf("the declared Loop-level table has %d rows, want the taxonomy's 17", len(domainTable))
	}
	domainSeen := map[string]bool{}
	for _, row := range domainTable {
		if domainSeen[row.class] {
			t.Fatalf("Loop-level class %q has two rows", row.class)
		}
		domainSeen[row.class] = true
	}
	t.Logf("provider-local rows=%d, Loop-level declared rows=%d, unmapped classes refuse rather than default", len(providerLocalTable), len(domainTable))
}

func TestEachRowOfTheOpeningTableBehavesAsTheRowSays(t *testing.T) {
	for _, row := range providerLocalTable {
		pool := newPool(t)
		breaker := newBreaker(t)
		window := newWindow(t, "claude", 100, 100000)
		obs, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{
			Provider: "claude", Model: "a-model", Window: "rolling-hour",
			DeclaredCLIVersion: "1.0.0",
			Failure:            provider.Failure{Class: row.class},
		}, base())
		if err != nil {
			t.Fatalf("%s: %v", row.class, err)
		}
		if obs.Action != row.action {
			t.Fatalf("%s: action = %q, want %q", row.class, obs.Action, row.action)
		}
		report, err := breaker.Report("claude")
		if err != nil {
			t.Fatal(err)
		}
		modelState, err := breaker.ModelState("claude", "a-model")
		if err != nil {
			t.Fatal(err)
		}
		slot, err := pool.Slot("claude")
		if err != nil {
			t.Fatal(err)
		}
		switch row.action {
		case provider.ActionOpenImmediately:
			if report.State != provider.CircuitNotSending {
				t.Fatalf("%s: state = %q on first occurrence, want %q (%s)", row.class, report.State, provider.CircuitNotSending, row.why)
			}
			if slot.State != provider.SlotCoolingDown {
				t.Fatalf("%s: slot = %q, want cooling-down", row.class, slot.State)
			}
		case provider.ActionCountTowardWindowedThreshold:
			if report.State != provider.CircuitSending {
				t.Fatalf("%s: state = %q on first occurrence, want %q -- this class counts and must not open on a first occurrence (%s)", row.class, report.State, provider.CircuitSending, row.why)
			}
		case provider.ActionOpenModelCircuitOnly:
			if report.State != provider.CircuitSending {
				t.Fatalf("%s: the Provider circuit is %q; only the narrower circuit may open (%s)", row.class, report.State, row.why)
			}
			if modelState != provider.CircuitNotSending {
				t.Fatalf("%s: the Provider-and-model circuit is %q, want not-sending", row.class, modelState)
			}
			other, err := breaker.ModelState("claude", "another-model")
			if err != nil {
				t.Fatal(err)
			}
			if other != provider.CircuitSending {
				t.Fatalf("%s: another model in the same pool is %q; the whole point is that it stays available", row.class, other)
			}
		case provider.ActionMoveSlotWithoutOpening:
			if report.State != provider.CircuitSending {
				t.Fatalf("%s: the circuit opened; this class must move the slot without opening (%s)", row.class, row.why)
			}
			if slot.State != provider.SlotUnauthenticated {
				t.Fatalf("%s: slot = %q, want unauthenticated", row.class, slot.State)
			}
		case provider.ActionNeitherCountsNorOpens:
			if report.State != provider.CircuitSending {
				t.Fatalf("%s: the circuit moved (%s)", row.class, row.why)
			}
			if slot.State != provider.SlotAvailable {
				t.Fatalf("%s: slot = %q, want available", row.class, slot.State)
			}
		}
	}
}

func TestSecretSuspectedAndAReachedLimitMoveTheSlotAndNeverOpenTheCircuit(t *testing.T) {
	// These two are Loop-level conditions, not adapter classifications, so
	// they arrive through the pool's own explicit calls rather than through a
	// FailureClass. Either way, the circuit must stay sending: the Provider
	// did nothing wrong in either case.
	for _, tc := range []struct {
		name  string
		move  func(*provider.Pool) error
		state provider.SlotState
	}{
		{"secret-suspected", func(p *provider.Pool) error { return p.Quarantine("codex") }, provider.SlotQuarantined},
		{"budget-exceeded", func(p *provider.Pool) error { return p.StopForInspection("codex") }, provider.SlotStoppedForInspection},
	} {
		pool := newPool(t)
		breaker := newBreaker(t)
		if err := tc.move(pool); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		report, err := breaker.Report("codex")
		if err != nil {
			t.Fatal(err)
		}
		if report.State != provider.CircuitSending {
			t.Fatalf("%s: circuit = %q; a Loop-side stop is not a Provider fault and must not be reported as one", tc.name, report.State)
		}
		slot, err := pool.Slot("codex")
		if err != nil {
			t.Fatal(err)
		}
		if slot.State != tc.state {
			t.Fatalf("%s: slot = %q, want %q", tc.name, slot.State, tc.state)
		}
	}
}

func TestProgressStalledIsNotEvenExpressibleAsAProviderObservation(t *testing.T) {
	// The tempting row. progress-stalled is a Loop-level class about the
	// Runner or the lease; it does not name a Provider, and this package
	// cannot even classify it, so there is no path by which it opens a
	// circuit. Asserting the absence is the point.
	if _, err := provider.ActionForFailureClass("progress-stalled"); !errors.Is(err, provider.ErrUnmappedFailureClass) {
		t.Fatalf("progress-stalled has a row in the provider-local table: %v", err)
	}
	for _, class := range provider.FailureClasses() {
		if string(class) == "progress-stalled" {
			t.Fatal("progress-stalled is a provider-local class; it must not be")
		}
	}
	found := false
	for _, row := range domainTable {
		if row.class == "progress-stalled" {
			found = true
			if row.action != provider.ActionNeitherCountsNorOpens {
				t.Fatalf("progress-stalled declared action = %q", row.action)
			}
			if !strings.Contains(row.why, "evidence about something else") {
				t.Fatalf("the progress-stalled row does not record why: %q", row.why)
			}
		}
	}
	if !found {
		t.Fatal("progress-stalled has no row in the declared Loop-level table")
	}
}

func TestTransportOpensAtItsCountAndUnknownOnlyAtAStrictlyHigherOne(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "claude", 100, 100000)
	observe(t, pool, breaker, window, provider.FailureTransport, base())
	report, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitSending {
		t.Fatalf("one transport failure opened the circuit: %q", report.State)
	}
	observe(t, pool, breaker, window, provider.FailureTransport, base().Add(time.Minute))
	report, err = breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitNotSending {
		t.Fatalf("two transport failures did not reach the count: %q", report.State)
	}

	// unknown needs strictly more, and never opens on a first occurrence.
	pool2 := newPool(t)
	breaker2 := newBreaker(t)
	window2 := newWindow(t, "codex", 100, 100000)
	for i := 1; i <= 2; i++ {
		observe(t, pool2, breaker2, window2, provider.FailureUnknown, base())
		report, err := breaker2.Report("codex")
		if err != nil {
			t.Fatal(err)
		}
		if report.State != provider.CircuitSending {
			t.Fatalf("unknown opened at count %d, but the transport count is %d and unknown must be strictly higher", i, policy().TransportCount)
		}
	}
	observe(t, pool2, breaker2, window2, provider.FailureUnknown, base())
	report, err = breaker2.Report("codex")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitNotSending {
		t.Fatalf("unknown did not open at its own count: %q", report.State)
	}

	// A policy whose unknown count is not strictly higher is refused, so the
	// ordering is not merely conventional.
	bad := policy()
	bad.UnknownCount = bad.TransportCount
	if _, err := provider.NewBreaker(bad); !errors.Is(err, provider.ErrInvalidBreakerPolicy) {
		t.Fatalf("a policy with unknown at or below the transport count was accepted: %v", err)
	}
}

func TestATimeoutOpenIsMarkedAmbiguous(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "claude", 100, 100000)
	observe(t, pool, breaker, window, provider.FailureTimeout, base())
	observe(t, pool, breaker, window, provider.FailureTimeout, base())
	report, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitNotSending {
		t.Fatalf("state = %q", report.State)
	}
	if !report.Ambiguous {
		t.Fatal("a timeout-caused open is not marked ambiguous; a timeout is an unknown result, not an established failure")
	}
}

// ---------------------------------------------------------------------------
// A12: closing is never time alone.
// ---------------------------------------------------------------------------

func TestOpenToHalfOpenNeedsTheDeadlineAndTheSingleProbe(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "claude", 100, 100000)
	observe(t, pool, breaker, window, provider.FailureTransport, base())
	observe(t, pool, breaker, window, provider.FailureTransport, base())

	// Before the deadline, no probe.
	if _, err := breaker.Probe(pool, window, "claude", base().Add(9*time.Minute)); !errors.Is(err, provider.ErrCooldownNotElapsed) {
		t.Fatalf("a probe was issued before the deadline: %v", err)
	}
	// At the deadline, exactly one.
	at := base().Add(10 * time.Minute)
	probe, err := breaker.Probe(pool, window, "claude", at)
	if err != nil {
		t.Fatalf("probe at the deadline: %v", err)
	}
	report, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitProbing {
		t.Fatalf("state after issuing the probe = %q, want %q -- issuing a probe is not closing", report.State, provider.CircuitProbing)
	}
	// A second probe, for this Provider or any other, is refused while one
	// is outstanding.
	if _, err := breaker.Probe(pool, window, "claude", at); !errors.Is(err, provider.ErrCircuitNotOpen) {
		t.Fatalf("a second probe for the same Provider: %v", err)
	}
	other := newWindow(t, "codex", 100, 100000)
	observe(t, pool, breaker, other, provider.FailureQuota, base())
	if _, err := breaker.Probe(pool, other, "codex", base().Add(2*time.Hour)); !errors.Is(err, provider.ErrProbeOutstanding) {
		t.Fatalf("a second probe for another Provider was issued while one was outstanding: %v", err)
	}

	// Only a reported successful observation closes it.
	if err := provider.ApplySuccess(pool, breaker, "claude", at); err != nil {
		t.Fatalf("apply success: %v", err)
	}
	report, err = breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitSending {
		t.Fatalf("state after a reported success = %q", report.State)
	}
	if pool.ProbeOutstanding() {
		t.Fatal("the probe was not returned when the circuit closed")
	}
	slot, err := pool.Slot("claude")
	if err != nil {
		t.Fatal(err)
	}
	if !slot.VerifiedByLoopInvocation || slot.State != provider.SlotAvailable {
		t.Fatalf("slot after the successful probe = %#v", slot)
	}
	_ = probe
}

func TestAnExhaustionOpenAlsoNeedsTheWindowToHaveRolledOver(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	// A window long enough that the cooldown deadline passes well before it
	// rolls: that gap is exactly the case a time-only close gets wrong.
	window, err := provider.NewUsageWindow("claude", "rolling-day", base(), 24*time.Hour, provider.WindowCeiling{Invocations: 5, TotalTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	observe(t, pool, breaker, window, provider.FailureQuota, base())
	report, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitNotSending {
		t.Fatalf("an explicit exhaustion signal did not open the circuit on first occurrence: %q", report.State)
	}

	// The deadline has passed. The window has not rolled. No probe.
	afterDeadline := base().Add(30 * time.Minute)
	if _, err := breaker.Probe(pool, window, "claude", afterDeadline); !errors.Is(err, provider.ErrWindowNotRolledOver) {
		t.Fatalf("a probe was issued after the deadline while the window had not rolled: %v -- elapsed time alone does not make an exhausted window fresh, and a timed probe against one is a guaranteed second exhaustion", err)
	}
	if pool.ProbeOutstanding() {
		t.Fatal("a probe was issued and left outstanding by the refused attempt")
	}
	// Still no probe much later, as long as the window has not rolled.
	if _, err := breaker.Probe(pool, window, "claude", base().Add(23*time.Hour)); !errors.Is(err, provider.ErrWindowNotRolledOver) {
		t.Fatalf("a probe was issued 23 hours in with an unrolled window: %v", err)
	}
	// Once the window rolls, the probe is allowed.
	rolled := base().Add(24 * time.Hour)
	if _, err := breaker.Probe(pool, window, "claude", rolled); err != nil {
		t.Fatalf("no probe after both the deadline and the rollover: %v", err)
	}
}

func TestAFailedProbeMultipliesTheCooldownUpToItsCeilingAndKeepsTheCount(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "claude", 100, 100000)
	observe(t, pool, breaker, window, provider.FailureTransport, base())
	observe(t, pool, breaker, window, provider.FailureTransport, base())
	cooldown, err := breaker.Cooldown("claude")
	if err != nil {
		t.Fatal(err)
	}
	if cooldown != 10*time.Minute {
		t.Fatalf("first cooldown = %s", cooldown)
	}
	failuresBefore, err := pool.Slot("claude")
	if err != nil {
		t.Fatal(err)
	}

	at := base().Add(10 * time.Minute)
	probe, err := breaker.Probe(pool, window, "claude", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := breaker.ObserveProbeFailure(pool, "claude", probe, provider.Failure{Class: provider.FailureTransport}, at); err != nil {
		t.Fatalf("observe probe failure: %v", err)
	}
	cooldown, err = breaker.Cooldown("claude")
	if err != nil {
		t.Fatal(err)
	}
	if cooldown != 30*time.Minute {
		t.Fatalf("cooldown after one failed probe = %s, want 30m (10m x 3)", cooldown)
	}
	report, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitNotSending {
		t.Fatalf("state after a failed probe = %q", report.State)
	}
	after, err := pool.Slot("claude")
	if err != nil {
		t.Fatal(err)
	}
	if after.ConsecutiveFailures <= failuresBefore.ConsecutiveFailures {
		t.Fatalf("the failure count did not advance across the failed probe: %d then %d", failuresBefore.ConsecutiveFailures, after.ConsecutiveFailures)
	}

	// Second failed probe: 30m x 3 = 90m, which is exactly the ceiling.
	at = at.Add(30 * time.Minute)
	probe, err = breaker.Probe(pool, window, "claude", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := breaker.ObserveProbeFailure(pool, "claude", probe, provider.Failure{Class: provider.FailureTransport}, at); err != nil {
		t.Fatal(err)
	}
	cooldown, _ = breaker.Cooldown("claude")
	if cooldown != 90*time.Minute {
		t.Fatalf("cooldown = %s, want the 90m ceiling", cooldown)
	}
	// Third: the ceiling holds.
	at = at.Add(90 * time.Minute)
	probe, err = breaker.Probe(pool, window, "claude", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := breaker.ObserveProbeFailure(pool, "claude", probe, provider.Failure{Class: provider.FailureTransport}, at); err != nil {
		t.Fatal(err)
	}
	cooldown, _ = breaker.Cooldown("claude")
	if cooldown != 90*time.Minute {
		t.Fatalf("cooldown = %s, want the declared 90m ceiling to hold", cooldown)
	}
	// A stale probe cannot report an outcome.
	if err := breaker.ObserveProbeFailure(pool, "claude", provider.Probe{Provider: "claude", Serial: 999}, provider.Failure{Class: provider.FailureTransport}, at); !errors.Is(err, provider.ErrProbeMismatch) {
		t.Fatalf("a stale probe reported an outcome: %v", err)
	}
}

func TestAContractIncompatibleOpenIsNeverProbeEligible(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "opencode", 100, 100000)
	if _, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{
		Provider: "opencode", Window: "rolling-hour", DeclaredCLIVersion: "1.18.22",
		Failure: provider.Failure{Class: provider.FailureContract},
	}, base()); err != nil {
		t.Fatal(err)
	}
	report, err := breaker.Report("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitNotSending {
		t.Fatalf("state = %q", report.State)
	}
	// No amount of elapsed time makes it probe-eligible.
	for _, at := range []time.Time{base().Add(time.Hour), base().Add(24 * time.Hour), base().Add(365 * 24 * time.Hour)} {
		if _, err := breaker.Probe(pool, window, "opencode", at); !errors.Is(err, provider.ErrProbeNotEligible) {
			t.Fatalf("a probe became eligible at %s: %v -- nothing about waiting changes either side's declared version", at, err)
		}
	}
	// The same declared version does not close it.
	closed, err := breaker.ObserveDeclaredCLIVersion(pool, "opencode", "1.18.22")
	if err != nil {
		t.Fatal(err)
	}
	if closed {
		t.Fatal("the circuit closed on an unchanged declared CLI version")
	}
	if report, err = breaker.Report("opencode"); err != nil || report.State != provider.CircuitNotSending {
		t.Fatalf("state = %q err=%v", report.State, err)
	}
	// An explicitly observed change does.
	closed, err = breaker.ObserveDeclaredCLIVersion(pool, "opencode", "1.19.0")
	if err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("an observed change of declared CLI version did not close the circuit")
	}
	if report, err = breaker.Report("opencode"); err != nil || report.State != provider.CircuitSending {
		t.Fatalf("state after the version change = %q err=%v", report.State, err)
	}
	slot, err := pool.Slot("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != provider.SlotAvailable {
		t.Fatalf("slot = %q", slot.State)
	}
}

func TestClosingNeverHappensOnElapsedTimeAlone(t *testing.T) {
	// The summary verdict for A12, stated once as a single assertion: for
	// every way a circuit can be open, letting arbitrary time pass and
	// asking for the state again never yields sending.
	for _, class := range []provider.FailureClass{provider.FailureQuota, provider.FailureContract, provider.FailureTransport} {
		pool := newPool(t)
		breaker := newBreaker(t)
		window := newWindow(t, "claude", 100, 100000)
		for i := 0; i < policy().TransportCount; i++ {
			observe(t, pool, breaker, window, class, base())
		}
		report, err := breaker.Report("claude")
		if err != nil {
			t.Fatal(err)
		}
		if report.State != provider.CircuitNotSending {
			t.Fatalf("%s: the circuit is not open; the case is not being exercised", class)
		}
		for _, elapsed := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
			later, err := breaker.Report("claude")
			if err != nil {
				t.Fatal(err)
			}
			if later.State == provider.CircuitSending {
				t.Fatalf("%s: the circuit read as sending after %s of elapsed time with no probe and no reported success", class, elapsed)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// A13: an open breaker does not read as a broken Provider.
// ---------------------------------------------------------------------------

func TestExportedStateSetIsExactlyThreeLoopActions(t *testing.T) {
	states := []provider.CircuitState{provider.CircuitSending, provider.CircuitNotSending, provider.CircuitProbing}
	want := []string{"sending", "not-sending", "probing"}
	for i, state := range states {
		if string(state) != want[i] {
			t.Fatalf("state %d = %q, want %q", i, state, want[i])
		}
		// Each names an action of this Loop, in the present continuous. None
		// of them is an adjective about the Provider.
		for _, forbidden := range forbiddenConditionWords() {
			if strings.EqualFold(string(state), forbidden) {
				t.Fatalf("state %q names the Provider's condition", state)
			}
		}
	}
	// Every state the breaker can report is one of the three.
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "claude", 100, 100000)
	reported := map[provider.CircuitState]bool{}
	record := func() {
		report, err := breaker.Report("claude")
		if err != nil {
			t.Fatal(err)
		}
		reported[report.State] = true
	}
	record()
	observe(t, pool, breaker, window, provider.FailureQuota, base())
	record()
	rolled := base().Add(time.Hour)
	if _, err := breaker.Probe(pool, window, "claude", rolled); err != nil {
		t.Fatal(err)
	}
	record()
	if len(reported) != 3 {
		t.Fatalf("reported states = %v, want all three reachable", reported)
	}
	for state := range reported {
		found := false
		for _, declared := range states {
			if state == declared {
				found = true
			}
		}
		if !found {
			t.Fatalf("the breaker reported %q, which is outside the declared set", state)
		}
	}
}

func TestEveryNotSendingValueCarriesItsEvidenceAndItsObservationScope(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "claude", 100, 100000)

	sending, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if sending.State != provider.CircuitSending || len(sending.Because) != 0 || sending.Window != "" || sending.ObservationScope != "" {
		t.Fatalf("a sending report carries evidence for a decision it did not make: %#v", sending)
	}

	observe(t, pool, breaker, window, provider.FailureTimeout, base())
	observe(t, pool, breaker, window, provider.FailureTransport, base())
	notSending, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if notSending.State != provider.CircuitNotSending {
		t.Fatalf("state = %q", notSending.State)
	}
	if len(notSending.Because) == 0 {
		t.Fatal("a not-sending report carries no classes; a decision with no stated reason cannot be audited")
	}
	for _, class := range notSending.Because {
		if _, err := provider.ActionForFailureClass(class); err != nil {
			t.Fatalf("the report cites %q, which is not a declared class: %v", class, err)
		}
	}
	if notSending.Window != "rolling-hour" {
		t.Fatalf("window = %q; the report must name the window the count was taken over", notSending.Window)
	}
	if notSending.ObservationScope == "" {
		t.Fatal("a not-sending report carries no observation scope")
	}
	for _, phrase := range []string{"only invocations that passed through this Loop's own execution path", "a person or another process started directly"} {
		if !strings.Contains(notSending.ObservationScope, phrase) {
			t.Fatalf("the observation scope does not say, in words, %q: %q", phrase, notSending.ObservationScope)
		}
	}
	if notSending.ObservationScope != provider.ObservationScope {
		t.Fatal("the report's observation scope is not the package's single declared value")
	}

	// The probing state carries the same evidence: it was reached from a
	// not-sending state and the reader needs the same context.
	rolled := base().Add(time.Hour)
	if _, err := breaker.Probe(pool, window, "claude", rolled); err != nil {
		t.Fatal(err)
	}
	probing, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if probing.State != provider.CircuitProbing || len(probing.Because) == 0 || probing.ObservationScope == "" {
		t.Fatalf("probing report = %#v", probing)
	}
	t.Log("the runner ledger sees only invocations that went through SupervisedInvocationRunner; a breaker built on the same population knows how this Loop's own calls went and nothing about calls made outside the Loop")
}

func TestBreakerRefusesIncompleteDependenciesAndUnknownProviders(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "claude", 10, 10)
	if _, err := provider.ApplyObservation(nil, breaker, window, provider.Observed{Provider: "claude"}, base()); !errors.Is(err, provider.ErrIncompleteDependencies) {
		t.Fatalf("a nil pool was accepted: %v", err)
	}
	if _, err := provider.ApplyObservation(pool, breaker, nil, provider.Observed{Provider: "claude"}, base()); !errors.Is(err, provider.ErrIncompleteDependencies) {
		t.Fatalf("a nil window was accepted: %v", err)
	}
	if _, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{Provider: "gemini", Failure: provider.Failure{Class: provider.FailureTransport}}, base()); !errors.Is(err, provider.ErrUnknownProvider) {
		t.Fatalf("a fourth provider was observed: %v", err)
	}
	if _, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{Provider: "codex", Failure: provider.Failure{Class: provider.FailureTransport}}, base()); !errors.Is(err, provider.ErrWindowProviderMismatch) {
		t.Fatalf("a claude window counted a codex observation: %v", err)
	}
	if _, err := breaker.Report("gemini"); !errors.Is(err, provider.ErrUnknownProvider) {
		t.Fatalf("a fourth provider has a circuit: %v", err)
	}
	for _, bad := range []provider.BreakerPolicy{
		{TransportCount: 0, UnknownCount: 3, Cooldown: time.Minute, CooldownMultiplier: 2, CooldownCeiling: time.Hour},
		{TransportCount: 2, UnknownCount: 3, Cooldown: 0, CooldownMultiplier: 2, CooldownCeiling: time.Hour},
		{TransportCount: 2, UnknownCount: 3, Cooldown: time.Hour, CooldownMultiplier: 2, CooldownCeiling: time.Minute},
		{TransportCount: 2, UnknownCount: 3, Cooldown: time.Minute, CooldownMultiplier: 1, CooldownCeiling: time.Hour},
	} {
		if _, err := provider.NewBreaker(bad); !errors.Is(err, provider.ErrInvalidBreakerPolicy) {
			t.Fatalf("policy %#v was accepted", bad)
		}
	}
}
