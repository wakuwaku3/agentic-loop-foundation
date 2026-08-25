package provider_test

// Provider account pool (V2-027 A9).

import (
	"errors"
	"go/ast"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// base is the single injected time value every test in this package builds
// its deadlines from. Nothing here reads a clock.
func base() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) }

func seeds() map[string]provider.SlotSeed {
	return map[string]provider.SlotSeed{
		"codex":    {Authorized: true, PreflightRecordID: "V2-075-provider-live-claude-rebind"},
		"claude":   {Authorized: true, PreflightRecordID: "V2-075-provider-live-claude-rebind"},
		"opencode": {Authorized: true, PreflightRecordID: "V2-075-provider-live-claude-rebind"},
	}
}

func newPool(t *testing.T) *provider.Pool {
	t.Helper()
	pool, err := provider.NewPool(seeds())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool
}

func TestPoolHasExactlyThreeSlotsAndNoFourthIsConstructible(t *testing.T) {
	pool := newPool(t)
	if want := []string{"codex", "claude", "opencode"}; !reflect.DeepEqual(pool.Names(), want) {
		t.Fatalf("pool names = %v, want %v", pool.Names(), want)
	}
	for _, name := range pool.Names() {
		slot, err := pool.Slot(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if slot.Name != name || slot.State != provider.SlotAvailable {
			t.Fatalf("%s: slot = %#v", name, slot)
		}
	}
	if _, err := pool.Slot("gemini"); !errors.Is(err, provider.ErrUnknownProvider) {
		t.Fatalf("a fourth name resolved to a slot: %v", err)
	}

	// A fourth seed, a missing seed and a renamed seed are all refused, so
	// there is no construction path to a fourth slot.
	fourth := seeds()
	fourth["gemini"] = provider.SlotSeed{Authorized: true}
	if _, err := provider.NewPool(fourth); !errors.Is(err, provider.ErrPoolSeedIncomplete) {
		t.Fatalf("a four-seed pool was constructed: %v", err)
	}
	two := seeds()
	delete(two, "opencode")
	if _, err := provider.NewPool(two); !errors.Is(err, provider.ErrPoolSeedIncomplete) {
		t.Fatalf("a two-seed pool was constructed: %v", err)
	}
	renamed := seeds()
	delete(renamed, "opencode")
	renamed["open-code"] = provider.SlotSeed{Authorized: true}
	if _, err := provider.NewPool(renamed); !errors.Is(err, provider.ErrPoolSeedIncomplete) {
		t.Fatalf("a pool with a misspelled name was constructed: %v", err)
	}
}

// TestSlotFieldSetIsExactlyTheEightDeclaredFields is the structural half of
// A9: a slot holds only what dp-v2-027 d8 lists. A ninth field -- a token, a
// session identifier, an owner identity, an executable path, a threshold
// number -- fails here rather than being caught by a name pattern that a
// creative spelling could slip past.
func TestSlotFieldSetIsExactlyTheEightDeclaredFields(t *testing.T) {
	want := []string{
		"Authorized",
		"ConsecutiveFailures",
		"CooldownDeadline",
		"LastFailureClass",
		"Name",
		"PreflightRecordID",
		"State",
		"VerifiedByLoopInvocation",
	}
	slotType := reflect.TypeOf(provider.Slot{})
	got := make([]string, 0, slotType.NumField())
	for i := 0; i < slotType.NumField(); i++ {
		got = append(got, slotType.Field(i).Name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Slot fields = %v, want exactly %v", got, want)
	}
	// The preflight record is referenced by id only. Its type is a string
	// name, and no numeric field exists on the slot other than the failure
	// counter, so no limit value out of that record can be copied here.
	numeric := 0
	for i := 0; i < slotType.NumField(); i++ {
		field := slotType.Field(i)
		switch field.Type.Kind() {
		case reflect.Int, reflect.Int64, reflect.Float64, reflect.Uint64:
			numeric++
			if field.Name != "ConsecutiveFailures" {
				t.Fatalf("Slot.%s is a number; the only number a slot may hold is its own consecutive-failure count, never a threshold", field.Name)
			}
		}
	}
	if numeric != 1 {
		t.Fatalf("Slot holds %d numeric fields, want exactly 1", numeric)
	}
	slot, err := newPool(t).Slot("claude")
	if err != nil {
		t.Fatal(err)
	}
	if slot.PreflightRecordID != "V2-075-provider-live-claude-rebind" {
		t.Fatalf("preflight record id = %q", slot.PreflightRecordID)
	}
	t.Logf("Slot fields = %v; the preflight record is named by id and nothing is copied out of it", got)
}

// TestNoCodePathReturnsTwoLiveLeasesForOneName is the by-construction half of
// the per-Provider concurrency ceiling. Acquire is asserted, from the AST, to
// be the only function in the package that returns a Lease.
func TestNoCodePathReturnsTwoLiveLeasesForOneName(t *testing.T) {
	returning := map[string]bool{}
	for _, f := range parseProviderPackage(t) {
		if f.isTest {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Type.Results == nil {
				continue
			}
			for _, result := range fn.Type.Results.List {
				ident, isIdent := result.Type.(*ast.Ident)
				if isIdent && ident.Name == "Lease" {
					returning[fn.Name.Name] = true
				}
			}
		}
	}
	if !reflect.DeepEqual(returning, map[string]bool{"Acquire": true}) {
		t.Fatalf("functions returning a Lease = %v, want exactly {Acquire}; a second one would be a second way to hand out a live handle", returning)
	}

	pool := newPool(t)
	lease, err := pool.Acquire("claude", base())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	slot, err := pool.Slot("claude")
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != provider.SlotInUse {
		t.Fatalf("slot state after acquire = %q", slot.State)
	}
	// A second acquire is refused, not queued.
	if _, err := pool.Acquire("claude", base().Add(time.Second)); !errors.Is(err, provider.ErrSlotUnavailable) {
		t.Fatalf("second acquire = %v, want a refusal: two concurrent invocations on one subscription share one usage window, so a second lease would be claiming an isolation that does not exist", err)
	}
	// Another Provider is unaffected: the ceiling is per Provider.
	if _, err := pool.Acquire("codex", base()); err != nil {
		t.Fatalf("acquiring a different provider: %v", err)
	}
	// Release of a slot nobody holds is refused.
	if err := pool.Release(provider.Lease{Provider: "opencode", Serial: 1, AcquiredAt: base()}, base()); !errors.Is(err, provider.ErrSlotNotHeld) {
		t.Fatalf("release of an unheld slot = %v", err)
	}
	// A stale lease cannot release a slot.
	if err := pool.Release(provider.Lease{Provider: "claude", Serial: 99, AcquiredAt: base()}, base()); !errors.Is(err, provider.ErrLeaseStale) {
		t.Fatalf("release under a stale lease = %v", err)
	}
	if err := pool.Release(lease, base().Add(time.Minute)); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := pool.Acquire("claude", base().Add(2*time.Minute)); err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if _, err := pool.Acquire("gemini", base()); !errors.Is(err, provider.ErrUnknownProvider) {
		t.Fatalf("acquiring a fourth name = %v", err)
	}
}

func TestAcquireAndReleaseTakeTheCallersTimeAndThePackageReadsNoClock(t *testing.T) {
	// The signature half. The determinism scan in source_guard_test.go is
	// the other half: no non-test file may name time.Now at all.
	acquire, present := reflect.TypeOf(&provider.Pool{}).MethodByName("Acquire")
	if !present {
		t.Fatal("Pool has no Acquire method")
	}
	if acquire.Type.NumIn() != 3 || acquire.Type.In(2) != reflect.TypeOf(time.Time{}) {
		t.Fatalf("Acquire signature = %v, want an explicit time argument", acquire.Type)
	}
	release, present := reflect.TypeOf(&provider.Pool{}).MethodByName("Release")
	if !present {
		t.Fatal("Pool has no Release method")
	}
	if release.Type.NumIn() != 3 || release.Type.In(2) != reflect.TypeOf(time.Time{}) {
		t.Fatalf("Release signature = %v, want an explicit time argument", release.Type)
	}

	// A released slot whose cooldown has not passed under the caller's time
	// goes back to cooling-down, not to available.
	pool := newPool(t)
	deadline := base().Add(10 * time.Minute)
	if err := pool.StartCooldown("codex", deadline, base()); err != nil {
		t.Fatalf("start cooldown: %v", err)
	}
	if err := pool.EndCooldown("codex", base().Add(time.Minute)); !errors.Is(err, provider.ErrCooldownNotElapsed) {
		t.Fatalf("cooldown ended early: %v", err)
	}
	if err := pool.EndCooldown("codex", deadline); err != nil {
		t.Fatalf("cooldown did not end at the deadline: %v", err)
	}
	slot, err := pool.Slot("codex")
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != provider.SlotAvailable {
		t.Fatalf("slot state after the deadline = %q", slot.State)
	}
}

func TestQuarantinedAndStoppedForInspectionSlotsNeedAnExplicitClearance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		move  func(*provider.Pool) error
		state provider.SlotState
		why   string
	}{
		{
			name:  "quarantined",
			move:  func(p *provider.Pool) error { return p.Quarantine("codex") },
			state: provider.SlotQuarantined,
			why:   "the prescribed actions are stop, redact and revoke, all of which are human acts",
		},
		{
			name:  "stopped-for-inspection",
			move:  func(p *provider.Pool) error { return p.StopForInspection("codex") },
			state: provider.SlotStoppedForInspection,
			why:   "reaching a runaway-detector limit is a stop for inspection, never a success and never a failure",
		},
		{
			name:  "unauthenticated",
			move:  func(p *provider.Pool) error { return p.MarkUnauthenticated("codex") },
			state: provider.SlotUnauthenticated,
			why:   "retrying cannot produce a session, because authenticating a CLI uses the owner's own identity",
		},
	} {
		pool := newPool(t)
		if err := tc.move(pool); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		slot, err := pool.Slot("codex")
		if err != nil {
			t.Fatal(err)
		}
		if slot.State != tc.state {
			t.Fatalf("%s: slot state = %q, want %q", tc.name, slot.State, tc.state)
		}
		if _, err := pool.Acquire("codex", base()); !errors.Is(err, provider.ErrSlotUnavailable) {
			t.Fatalf("%s: slot was acquirable: %v", tc.name, err)
		}
		// No deadline moves it.
		if err := pool.StartCooldown("codex", base().Add(time.Hour), base()); !errors.Is(err, provider.ErrSlotNeedsExternalClearance) {
			t.Fatalf("%s: a cooldown was allowed to overwrite the state (%s): %v", tc.name, tc.why, err)
		}
		if err := pool.EndCooldown("codex", base().Add(1000*time.Hour)); !errors.Is(err, provider.ErrSlotNeedsExternalClearance) {
			t.Fatalf("%s: elapsed time moved the slot (%s): %v", tc.name, tc.why, err)
		}
		// No probe moves it either.
		if _, err := pool.IssueProbe("codex"); !errors.Is(err, provider.ErrSlotNeedsExternalClearance) {
			t.Fatalf("%s: a probe was issued (%s): %v", tc.name, tc.why, err)
		}
		// Only the explicit clearance does.
		if err := pool.Clear("codex"); err != nil {
			t.Fatalf("%s: clear: %v", tc.name, err)
		}
		if _, err := pool.Acquire("codex", base()); err != nil {
			t.Fatalf("%s: slot not acquirable after clearance: %v", tc.name, err)
		}
	}
}

func TestAnUnauthorizedProviderRestsUnauthenticatedAndCannotBeCleared(t *testing.T) {
	unauthorized := seeds()
	unauthorized["opencode"] = provider.SlotSeed{Authorized: false, PreflightRecordID: "none"}
	pool, err := provider.NewPool(unauthorized)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	slot, err := pool.Slot("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != provider.SlotUnauthenticated || slot.Authorized {
		t.Fatalf("slot = %#v", slot)
	}
	if _, err := pool.Acquire("opencode", base()); !errors.Is(err, provider.ErrSlotUnavailable) {
		t.Fatalf("an unauthorized provider was acquirable: %v", err)
	}
	if err := pool.Clear("opencode"); !errors.Is(err, provider.ErrSlotNeedsExternalClearance) {
		t.Fatalf("a Provider the standing authorization does not cover was cleared by the Loop: %v", err)
	}
}

func TestExactlyOneProbeIsOutstandingAtATime(t *testing.T) {
	pool := newPool(t)
	first, err := pool.IssueProbe("claude")
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if !pool.ProbeOutstanding() {
		t.Fatal("no probe is outstanding after issuing one")
	}
	if _, err := pool.IssueProbe("claude"); !errors.Is(err, provider.ErrProbeOutstanding) {
		t.Fatalf("a second probe for the same Provider was issued: %v", err)
	}
	if _, err := pool.IssueProbe("codex"); !errors.Is(err, provider.ErrProbeOutstanding) {
		t.Fatalf("a second probe for another Provider was issued: %v", err)
	}
	if err := pool.ReturnProbe(provider.Probe{Provider: "claude", Serial: 99}); !errors.Is(err, provider.ErrProbeMismatch) {
		t.Fatalf("a stale probe was returned: %v", err)
	}
	if err := pool.ReturnProbe(first); err != nil {
		t.Fatalf("return probe: %v", err)
	}
	if pool.ProbeOutstanding() {
		t.Fatal("a probe is still outstanding after returning it")
	}
	second, err := pool.IssueProbe("codex")
	if err != nil {
		t.Fatalf("probe after return: %v", err)
	}
	if second == first {
		t.Fatal("the second probe is indistinguishable from the first")
	}
}

func TestMarkSuccessIsTheOnlyThingThatVerifiesASlot(t *testing.T) {
	pool := newPool(t)
	slot, err := pool.Slot("claude")
	if err != nil {
		t.Fatal(err)
	}
	if slot.VerifiedByLoopInvocation {
		t.Fatal("a slot is verified before the Loop has ever completed an invocation through it")
	}
	if err := pool.MarkFailure("claude", provider.FailureTransport); err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkFailure("claude", provider.FailureTransport); err != nil {
		t.Fatal(err)
	}
	slot, err = pool.Slot("claude")
	if err != nil {
		t.Fatal(err)
	}
	if slot.ConsecutiveFailures != 2 || slot.LastFailureClass != provider.FailureTransport {
		t.Fatalf("slot after two failures = %#v", slot)
	}
	if slot.VerifiedByLoopInvocation {
		t.Fatal("failures verified the slot")
	}
	if err := pool.MarkSuccess("claude"); err != nil {
		t.Fatal(err)
	}
	slot, err = pool.Slot("claude")
	if err != nil {
		t.Fatal(err)
	}
	if !slot.VerifiedByLoopInvocation || slot.ConsecutiveFailures != 0 || slot.LastFailureClass != "" {
		t.Fatalf("slot after success = %#v", slot)
	}
}
