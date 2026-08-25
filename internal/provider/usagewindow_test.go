package provider_test

// Per-Provider usage window (V2-027 A10).

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

func newWindow(t *testing.T, name string, invocations, tokens int64) *provider.UsageWindow {
	t.Helper()
	window, err := provider.NewUsageWindow(name, "rolling-hour", base(), time.Hour, provider.WindowCeiling{Invocations: invocations, TotalTokens: tokens})
	if err != nil {
		t.Fatalf("NewUsageWindow: %v", err)
	}
	return window
}

// TestWindowIsNotTheFirestoreReservationAndNotTheRunnersLedger states the
// three-way distinction as an executable fact rather than only as prose. The
// window is a different type, in a different package, with a different unit,
// and it has no authority to start or refuse a process.
func TestWindowIsNotTheFirestoreReservationAndNotTheRunnersLedger(t *testing.T) {
	window := newWindow(t, "claude", 4, 1000)
	report := window.Report(base())
	if report.Provider != "claude" || report.Window != "rolling-hour" {
		t.Fatalf("report = %#v", report)
	}
	// It counts invocations and reported tokens. It has no method that
	// decides whether anything may start: that authority is the runner's
	// cost ledger alone, before exec.
	windowType := reflect.TypeOf(window)
	for i := 0; i < windowType.NumMethod(); i++ {
		name := windowType.Method(i).Name
		for _, forbidden := range []string{"Reserve", "Allow", "Permit", "Start", "TrueUp", "Halt"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("UsageWindow.%s reads as an authority to start or settle an invocation; deciding whether a process may start belongs to the runner's cost ledger alone, before exec, and settling belongs to it as well", name)
			}
		}
	}
	// It does not claim to know a subscription's remaining allowance: there
	// is no such field and no such method.
	reportType := reflect.TypeOf(report)
	for i := 0; i < reportType.NumField(); i++ {
		name := strings.ToLower(reportType.Field(i).Name)
		for _, forbidden := range []string{"remaining", "allowance", "balance", "quota"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("WindowReport.%s claims knowledge of the subscription itself; the counted population is only what this Loop invoked, which is a strict subset", reportType.Field(i).Name)
			}
		}
	}
	t.Log("three distinct objects: internal/quota is the Firestore daily free-tier reservation in reads/writes/deletes; internal/runner.CostLedger is the runner-local, owner-approved, fail-closed runaway detector that alone decides before exec whether a process may start; this window counts invocations attempted through the pool and the token counts a Provider reported, and is an input to breaker and pool selection only")
}

func TestNoMonetaryVocabularyInAnyEmittedJSONKey(t *testing.T) {
	window := newWindow(t, "codex", 2, 10)
	pool := newPool(t)
	slot, err := pool.Slot("codex")
	if err != nil {
		t.Fatal(err)
	}
	breaker := newBreaker(t)
	circuit, err := breaker.Report("codex")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Acquire("codex", base())
	if err != nil {
		t.Fatal(err)
	}
	probe, err := pool.IssueProbe("claude")
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := provider.PrepareHandoff("codex", "claude", packet(), provider.Result{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := provider.ParseOrClassify(provider.CodexAdapter{}, readFixture(t, "codex", "success"))
	emitted := []any{
		window.Report(base()), slot, circuit, lease, probe, handoff, result,
		provider.Observation{}, provider.Usage{}, provider.Failure{}, provider.HandoffAttempt{},
		provider.Request{}, provider.WorkPacket{}, provider.Artifact{}, provider.Verification{}, provider.Decision{},
	}
	keys := 0
	for _, value := range emitted {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatalf("marshal %T: %v", value, marshalErr)
		}
		for _, key := range jsonKeysOf(t, raw) {
			keys++
			lowered := strings.ToLower(strings.ReplaceAll(key, "_", ""))
			for _, word := range monetaryWords() {
				if strings.Contains(lowered, word) {
					t.Fatalf("%T emits the JSON key %q, which contains the monetary word %q", value, key, word)
				}
			}
		}
	}
	if keys == 0 {
		t.Fatal("scanned zero emitted JSON keys")
	}
	t.Logf("scanned %d emitted JSON keys across %d types against %v", keys, len(emitted), monetaryWords())
}

func jsonKeysOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var keys []string
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				keys = append(keys, key)
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(decoded)
	return keys
}

func TestExhaustionIsReachedExactlyOneUnitOverTheCeilingAndNotBefore(t *testing.T) {
	// Invocation ceiling of three: three attempts are within, the fourth is
	// exhausted.
	window := newWindow(t, "claude", 3, 1_000_000)
	for i := 1; i <= 3; i++ {
		window.RecordAttempt(base())
		if state := window.State(base()); state != provider.WindowWithin {
			t.Fatalf("attempt %d of 3: state = %q, want %q", i, state, provider.WindowWithin)
		}
	}
	window.RecordAttempt(base())
	if state := window.State(base()); state != provider.WindowExhausted {
		t.Fatalf("attempt 4 of 3: state = %q, want %q", state, provider.WindowExhausted)
	}

	// Token ceiling of one hundred: exactly one hundred reported is within,
	// one hundred and one is exhausted.
	tokens := newWindow(t, "codex", 1_000_000, 100)
	reported := func(total int64) provider.Result {
		return provider.Result{Provider: "codex", Succeeded: true, UsageReported: true, Usage: provider.Usage{TotalTokens: total}}
	}
	if err := tokens.RecordResult(reported(100), base()); err != nil {
		t.Fatal(err)
	}
	if state := tokens.State(base()); state != provider.WindowWithin {
		t.Fatalf("exactly at the token ceiling: state = %q, want %q", state, provider.WindowWithin)
	}
	if err := tokens.RecordResult(reported(1), base()); err != nil {
		t.Fatal(err)
	}
	if state := tokens.State(base()); state != provider.WindowExhausted {
		t.Fatalf("one token over the ceiling: state = %q, want %q", state, provider.WindowExhausted)
	}
}

func TestAnUnreportedUsageIsUnknownAndNeverZeroConsumption(t *testing.T) {
	window := newWindow(t, "opencode", 100, 100)
	absent, _ := provider.ParseOrClassify(provider.OpenCodeAdapter{}, readFixture(t, "opencode", "usage-not-reported"))
	zeroes, _ := provider.ParseOrClassify(provider.OpenCodeAdapter{}, readFixture(t, "opencode", "usage-reported"))
	if absent.UsageReported || !zeroes.UsageReported {
		t.Fatalf("the two fixtures are not the two cases: absent=%v reported=%v", absent.UsageReported, zeroes.UsageReported)
	}

	// The reported-zero case leaves the window within: consumption is known
	// and is zero.
	known := newWindow(t, "opencode", 100, 100)
	known.RecordAttempt(base())
	if err := known.RecordResult(zeroes, base()); err != nil {
		t.Fatal(err)
	}
	if state := known.State(base()); state != provider.WindowWithin {
		t.Fatalf("reported zero: state = %q, want %q", state, provider.WindowWithin)
	}

	// The absent case makes the window unknown, not within and not zero.
	window.RecordAttempt(base())
	if err := window.RecordResult(absent, base()); err != nil {
		t.Fatal(err)
	}
	if state := window.State(base()); state != provider.WindowUnknown {
		t.Fatalf("usage absent: state = %q, want %q -- an unreported usage is not zero consumption", state, provider.WindowUnknown)
	}
	report := window.Report(base())
	if report.InvocationsWithUsageNotReported != 1 || report.InvocationsWithUsageReported != 0 {
		t.Fatalf("report = %#v", report)
	}
	if report.ReportedTotalTokens != 0 {
		t.Fatalf("an unreported usage contributed %d tokens; it must contribute none", report.ReportedTotalTokens)
	}
	if report.ObservationScope == "" || !strings.Contains(report.ObservationScope, "this Loop's own execution path") {
		t.Fatalf("observation scope = %q", report.ObservationScope)
	}

	// An explicit invocation-count exhaustion still reads as exhausted even
	// when the token side is unknown: an established exhaustion is not
	// downgraded by an unrelated unknown.
	precedence := newWindow(t, "opencode", 1, 100)
	precedence.RecordAttempt(base())
	precedence.RecordAttempt(base())
	if err := precedence.RecordResult(absent, base()); err != nil {
		t.Fatal(err)
	}
	if state := precedence.State(base()); state != provider.WindowExhausted {
		t.Fatalf("an established invocation exhaustion with an unknown token side = %q, want %q", state, provider.WindowExhausted)
	}
}

func TestWindowRolloverIsDrivenByTheCallersTimeAndNeverByAStoredClock(t *testing.T) {
	window := newWindow(t, "claude", 1, 1_000_000)
	window.RecordAttempt(base())
	window.RecordAttempt(base())
	if state := window.State(base()); state != provider.WindowExhausted {
		t.Fatalf("state = %q, want exhausted before the roll", state)
	}
	if generation := window.Generation(base()); generation != 0 {
		t.Fatalf("generation = %d, want 0 before any roll", generation)
	}
	// One second before the window length: still the same generation, still
	// exhausted. Nothing has elapsed in the window's own terms.
	almost := base().Add(time.Hour - time.Second)
	if window.Generation(almost) != 0 || window.State(almost) != provider.WindowExhausted {
		t.Fatalf("one second early: generation=%d state=%q", window.Generation(almost), window.State(almost))
	}
	rolled := base().Add(time.Hour)
	if generation := window.Generation(rolled); generation != 1 {
		t.Fatalf("generation at the roll = %d, want 1", generation)
	}
	if state := window.State(rolled); state != provider.WindowWithin {
		t.Fatalf("state after the roll = %q, want within", state)
	}
	// Three periods at once roll three times: the caller's time drives it,
	// not a tick anyone has to deliver.
	far := base().Add(4 * time.Hour)
	if generation := window.Generation(far); generation != 4 {
		t.Fatalf("generation four periods later = %d, want 4", generation)
	}
	report := window.Report(far)
	if !report.WindowStart.Equal(base().Add(4 * time.Hour)) {
		t.Fatalf("window start = %s", report.WindowStart)
	}
}

func TestWindowRefusesConstructionAndForeignResults(t *testing.T) {
	if _, err := provider.NewUsageWindow("gemini", "w", base(), time.Hour, provider.WindowCeiling{Invocations: 1, TotalTokens: 1}); !errors.Is(err, provider.ErrUnknownProvider) {
		t.Fatalf("a fourth provider got a window: %v", err)
	}
	for _, ceiling := range []provider.WindowCeiling{{Invocations: 0, TotalTokens: 1}, {Invocations: 1, TotalTokens: 0}, {Invocations: -1, TotalTokens: 1}} {
		if _, err := provider.NewUsageWindow("claude", "w", base(), time.Hour, ceiling); !errors.Is(err, provider.ErrInvalidWindow) {
			t.Fatalf("ceiling %#v was accepted; an implicit no-ceiling would make exhaustion unreachable: %v", ceiling, err)
		}
	}
	if _, err := provider.NewUsageWindow("claude", "", base(), time.Hour, provider.WindowCeiling{Invocations: 1, TotalTokens: 1}); !errors.Is(err, provider.ErrInvalidWindow) {
		t.Fatalf("an unnamed window was accepted: %v", err)
	}
	if _, err := provider.NewUsageWindow("claude", "w", base(), 0, provider.WindowCeiling{Invocations: 1, TotalTokens: 1}); !errors.Is(err, provider.ErrInvalidWindow) {
		t.Fatalf("a zero-length window was accepted: %v", err)
	}
	window := newWindow(t, "claude", 5, 5)
	if err := window.RecordResult(provider.Result{Provider: "codex", UsageReported: true}, base()); !errors.Is(err, provider.ErrWindowProviderMismatch) {
		t.Fatalf("a claude window accepted a codex Result: %v", err)
	}
}
