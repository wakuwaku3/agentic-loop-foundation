package application_test

// Acceptance tests for V2-067, the Provider registry.
//
// Determinism is acceptance here: there is no fixed sleep, no wall-clock timer
// and no goroutine anywhere in this file. Every instant comes from an injected
// clock, and every "later" is the test moving that clock forward.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// ---------------------------------------------------------------------------
// Fixtures and helpers
// ---------------------------------------------------------------------------

// providerBase is the frozen instant every case starts from.
var providerBase = time.Unix(1700000000, 0).UTC()

func providerService(t *testing.T, clk application.Clock) (*application.Service, *memory.Store) {
	t.Helper()
	st := memory.New()
	s, err := application.NewServiceWithConfig(st, clk, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func putObservation(t *testing.T, st *memory.Store, value application.ProviderObservation) {
	t.Helper()
	ctx := context.Background()
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveProviderObservation(ctx, value)
	}); err != nil {
		t.Fatalf("save observation %#v: %v", value, err)
	}
}

func putAssignment(t *testing.T, st *memory.Store, value application.ProviderAssignment, status domain.ExecutionStatus) {
	t.Helper()
	ctx := context.Background()
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveProviderAssignment(ctx, value); err != nil {
			return err
		}
		if status == "" {
			return nil
		}
		eid, err := domain.NewExecutionID(value.ExecutionID)
		if err != nil {
			return err
		}
		iid, err := domain.NewIncrementID(value.IncrementID)
		if err != nil {
			return err
		}
		rid, err := domain.NewRunnerID("runner-1")
		if err != nil {
			return err
		}
		existing, found, err := u.Execution(ctx, value.ExecutionID)
		if err != nil {
			return err
		}
		expected := domain.Version(0)
		if found {
			expected = existing.Version
		}
		return u.SaveExecution(ctx, domain.Execution{ID: eid, IncrementID: iid, RunnerID: rid, Status: status, Version: expected + 1}, expected)
	}); err != nil {
		t.Fatalf("save assignment %#v: %v", value, err)
	}
}

func providerRegistry(t *testing.T, s *application.Service) application.ProviderRegistryView {
	t.Helper()
	view, err := s.Providers(owner(context.Background()))
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if len(view.Providers) != 3 {
		t.Fatalf("registry reported %d providers, want exactly 3", len(view.Providers))
	}
	return view
}

func entryFor(t *testing.T, view application.ProviderRegistryView, name application.ProviderName) application.ProviderEntryView {
	t.Helper()
	for _, e := range view.Providers {
		if e.Provider == name {
			return e
		}
	}
	t.Fatalf("no entry for provider %q", name)
	return application.ProviderEntryView{}
}

func observation(name application.ProviderName, class application.ProviderFailureClass, stopped bool, at time.Time) application.ProviderObservation {
	return application.ProviderObservation{Provider: name, FailureClass: class, StoppedForInspection: stopped, ObservedAt: at}
}

// ---------------------------------------------------------------------------
// A3: the second, independent pin of the closed identity
// ---------------------------------------------------------------------------

// TestProviderRegistryNameTableIsExactlyThreeNames is the application half of
// A3. The provider half is TestProviderIdentityIsExactlyThreeAdapterNames in
// internal/provider. The two are deliberately not one test sharing an import:
// internal/application must not import internal/provider (asserted by
// source_guard_test.go), so the set is declared twice and these two tests are
// what force the two declarations to change together.
func TestProviderRegistryNameTableIsExactlyThreeNames(t *testing.T) {
	want := []application.ProviderName{"codex", "claude", "opencode"}
	if got := application.DeclaredProviders(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DeclaredProviders() = %v, want exactly %v in that documented order", got, want)
	}
	for _, name := range want {
		if !name.Valid() {
			t.Fatalf("declared provider %q is not Valid()", name)
		}
	}
	for _, name := range []application.ProviderName{"", "codex ", "Codex", "gemini", "cursor", "claude-code", "opencode2"} {
		if name.Valid() {
			t.Fatalf("undeclared provider %q is Valid()", name)
		}
	}
	// The table is a copy: mutating the returned slice cannot widen the set.
	mutated := application.DeclaredProviders()
	mutated[0] = "gemini"
	if application.DeclaredProviders()[0] != "codex" {
		t.Fatal("DeclaredProviders() returns the backing array; a caller can rewrite the closed set")
	}

	// And the same three names, in the same order, are what the openapi
	// document publishes as the provider enum.
	enums := openapiProviderEnums(t)
	if len(enums) == 0 {
		t.Fatal("found no provider enum in the openapi document; the extraction is broken and would pass vacuously")
	}
	for where, values := range enums {
		if !reflect.DeepEqual(values, []string{"codex", "claude", "opencode"}) {
			t.Fatalf("openapi %s declares the provider enum %v, want exactly [codex claude opencode]", where, values)
		}
	}
	t.Logf("pinned the three-name identity in internal/application and in %d openapi enum sites", len(enums))
}

// openapiPath is the contract document this task edits.
func openapiPath() string {
	return filepath.Join(repoRoot(), "contracts", "openapi", "openapi-v1.yaml")
}

var providerEnumLine = regexp.MustCompile(`(?m)^\s*(provider|name):\s*\{type: string, enum: \[([^\]]*)\]\}`)

// openapiProviderEnums extracts every provider-name enum declared in the
// openapi document, keyed by the property name plus its line number.
func openapiProviderEnums(t *testing.T) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(openapiPath())
	if err != nil {
		t.Fatalf("read %s: %v", openapiPath(), err)
	}
	out := map[string][]string{}
	for _, m := range providerEnumLine.FindAllStringSubmatch(string(data), -1) {
		values := []string{}
		for _, raw := range strings.Split(m[2], ",") {
			values = append(values, strings.TrimSpace(raw))
		}
		if len(values) != 3 || values[0] != "codex" {
			// Not a provider-name enum (the failure-class enum also lives on a
			// property called failure_class, which this pattern does not match).
			continue
		}
		out[fmt.Sprintf("%s (%d values)", m[1], len(values))] = values
	}
	return out
}

// ---------------------------------------------------------------------------
// A4: every enum is closed in both directions
// ---------------------------------------------------------------------------

// providerEnumConstantsAndCases parses provider_registry.go and returns, for
// the named type, the constant names declared with that type and the identifier
// names appearing as case values in that type's Valid() switch.
func providerEnumConstantsAndCases(t *testing.T, typeName string) (constants, cases []string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "provider_registry.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse provider_registry.go: %v", err)
	}
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || value.Type == nil {
					continue
				}
				ident, ok := value.Type.(*ast.Ident)
				if !ok || ident.Name != typeName {
					continue
				}
				for _, name := range value.Names {
					constants = append(constants, name.Name)
				}
			}
			continue
		}
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Valid" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		recv, ok := fn.Recv.List[0].Type.(*ast.Ident)
		if !ok || recv.Name != typeName {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if ident, ok := expr.(*ast.Ident); ok {
					cases = append(cases, ident.Name)
				}
			}
			return true
		})
	}
	sort.Strings(constants)
	sort.Strings(cases)
	return constants, cases
}

// TestProviderRegistryEnumsAreClosed is A4's closed-enum table. It fails if a
// constant is added without a case, and if a case is added without a constant,
// for every one of the six enums this task declares. The scan is verified
// against a synthetic type whose switch is deliberately missing one case.
func TestProviderRegistryEnumsAreClosed(t *testing.T) {
	const synthetic = `package application
type Synthetic string
const (
	SyntheticA Synthetic = "a"
	SyntheticB Synthetic = "b"
)
func (s Synthetic) Valid() bool {
	switch s {
	case SyntheticA:
		return true
	}
	return false
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", synthetic, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	sConst, sCase := 0, 0
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			for _, spec := range gen.Specs {
				if v, ok := spec.(*ast.ValueSpec); ok {
					if id, ok := v.Type.(*ast.Ident); ok && id.Name == "Synthetic" {
						sConst += len(v.Names)
					}
				}
			}
		}
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Valid" {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if clause, ok := n.(*ast.CaseClause); ok {
					sCase += len(clause.List)
				}
				return true
			})
		}
	}
	if sConst != 2 || sCase != 1 {
		t.Fatalf("positive control failed to parse: constants=%d cases=%d", sConst, sCase)
	}

	wantCounts := map[string]int{
		"ProviderName":          3,
		"ProviderHealth":        6,
		"ProviderBlockedReason": 7,
		"ProviderFailureClass":  7,
		"ProviderRunawayState":  3,
		"ProviderCeilingSource": 2,
	}
	for typeName, want := range wantCounts {
		constants, cases := providerEnumConstantsAndCases(t, typeName)
		if len(constants) == 0 || len(cases) == 0 {
			t.Fatalf("%s: constants=%v cases=%v; the scan found nothing and would pass vacuously", typeName, constants, cases)
		}
		if !reflect.DeepEqual(constants, cases) {
			t.Fatalf("%s: constants %v and Valid() cases %v are not the same set", typeName, constants, cases)
		}
		if len(constants) != want {
			t.Fatalf("%s declares %d constants (%v), want %d", typeName, len(constants), constants, want)
		}
	}

	// The runtime side agrees: the six declared health values are Valid and
	// nothing else is, including the plausible-sounding ones.
	for _, v := range []application.ProviderHealth{
		application.ProviderHealthUnknown, application.ProviderHealthHealthy, application.ProviderHealthDegraded,
		application.ProviderHealthUnavailable, application.ProviderHealthUnauthenticated, application.ProviderHealthStoppedForInspection,
	} {
		if !v.Valid() {
			t.Fatalf("health %q is declared but not Valid", v)
		}
	}
	for _, v := range []application.ProviderHealth{"", "ok", "up", "down", "connected", "disconnected", "authenticated", "unavailable-for-inspection"} {
		if v.Valid() {
			t.Fatalf("undeclared health %q is Valid", v)
		}
	}
	for _, v := range []application.ProviderRunawayState{"", "ok", "halted", "over-budget", "exhausted"} {
		if v.Valid() {
			t.Fatalf("undeclared runaway state %q is Valid", v)
		}
	}
	for _, v := range []application.ProviderFailureClass{"", "provider-quota", "invalid-input", "cancelled", "policy-denied"} {
		if v.Valid() {
			t.Fatalf("undeclared failure class %q is Valid; provider-quota in particular is deliberately re-spelled provider-rate-limited", v)
		}
	}
	for _, v := range []application.ProviderCeilingSource{"", "installation-declared"} {
		if v.Valid() {
			t.Fatalf("undeclared ceiling source %q is Valid; V2-068 introduces the settable limit and its own member", v)
		}
	}
}

// TestProviderRegistryFailureClassMapIsTotal is the other half of A4's closed
// enums: every declared failure class maps to a health value and a blocked
// reason, and every mapping is one of the declared values. A class with no case
// would fall through to the fail-closed default, which the table below refuses
// to accept as a mapping for a declared class.
func TestProviderRegistryFailureClassMapIsTotal(t *testing.T) {
	cases := []struct {
		class   application.ProviderFailureClass
		health  application.ProviderHealth
		blocked application.ProviderBlockedReason
	}{
		{application.ProviderFailureUnauthenticated, application.ProviderHealthUnauthenticated, application.ProviderBlockedOwnerMustAuthenticate},
		{application.ProviderFailureTransport, application.ProviderHealthDegraded, application.ProviderBlockedLastInvocationRetryable},
		{application.ProviderFailureRateLimited, application.ProviderHealthDegraded, application.ProviderBlockedLastInvocationRetryable},
		{application.ProviderFailureTimeout, application.ProviderHealthDegraded, application.ProviderBlockedLastInvocationRetryable},
		{application.ProviderFailureModel, application.ProviderHealthUnavailable, application.ProviderBlockedLastInvocationPermanent},
		{application.ProviderFailureContract, application.ProviderHealthUnavailable, application.ProviderBlockedLastInvocationPermanent},
		{application.ProviderFailureUnclassified, application.ProviderHealthUnknown, application.ProviderBlockedLastInvocationUnclassed},
	}
	declared, _ := providerEnumConstantsAndCases(t, "ProviderFailureClass")
	if len(cases) != len(declared) {
		t.Fatalf("the mapping table has %d rows for %d declared classes (%v); a declared class with no row would fall through to the fail-closed default", len(cases), len(declared), declared)
	}
	clk := &fixedClock{now: providerBase}
	for _, c := range cases {
		s, st := providerService(t, clk)
		putObservation(t, st, observation(application.ProviderClaude, c.class, false, providerBase))
		entry := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
		if entry.Health != c.health || entry.BlockedReason != c.blocked {
			t.Fatalf("class %q mapped to health=%q blocked=%q, want health=%q blocked=%q", c.class, entry.Health, entry.BlockedReason, c.health, c.blocked)
		}
		if !entry.Health.Valid() || !entry.BlockedReason.Valid() {
			t.Fatalf("class %q produced an undeclared health or blocked reason: %#v", c.class, entry)
		}
	}
}

// TestProviderRegistryViewIsDeterministic is A4's determinism clause: the same
// stored state produces byte-identical JSON across repeated calls, with entries
// in a fixed order.
func TestProviderRegistryViewIsDeterministic(t *testing.T) {
	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	putObservation(t, st, observation(application.ProviderClaude, "", false, providerBase))
	putObservation(t, st, observation(application.ProviderCodex, application.ProviderFailureUnauthenticated, false, providerBase))
	// Assignments are inserted out of order on purpose: the read must sort.
	putAssignment(t, st, application.ProviderAssignment{ExecutionID: "execution-c", IncrementID: "increment-1", Provider: application.ProviderClaude, Since: providerBase}, domain.ExecutionRunning)
	putAssignment(t, st, application.ProviderAssignment{ExecutionID: "execution-a", IncrementID: "increment-2", Provider: application.ProviderClaude, Since: providerBase}, domain.ExecutionRunning)

	// Three reads is the most this store's daily read reservation admits
	// (quota.ReadTransactionUsage), and three is enough: byte equality across
	// repeated calls is the property, not the call count.
	views := make([]application.ProviderRegistryView, 0, 3)
	bodies := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		v := providerRegistry(t, s)
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, v)
		bodies = append(bodies, string(b))
	}
	first := []byte(bodies[0])
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("call %d produced different bytes:\n%s\n%s", i+1, bodies[0], bodies[i])
		}
	}
	view := views[len(views)-1]
	order := []application.ProviderName{}
	for _, e := range view.Providers {
		order = append(order, e.Provider)
	}
	if !reflect.DeepEqual(order, []application.ProviderName{"codex", "claude", "opencode"}) {
		t.Fatalf("entry order = %v, want the fixed declared order", order)
	}
	claude := entryFor(t, view, application.ProviderClaude)
	if len(claude.Assignments) != 2 || claude.Assignments[0].ExecutionID != "execution-a" || claude.Assignments[1].ExecutionID != "execution-c" {
		t.Fatalf("assignments are not in execution-id order: %#v", claude.Assignments)
	}
	// An empty assignment list marshals as [] rather than null, so a reader
	// never has to tell the two apart.
	if !strings.Contains(string(first), `"assignments":[]`) {
		t.Fatalf("a provider with no assignment did not marshal an empty array: %s", first)
	}
}

// ---------------------------------------------------------------------------
// A5: health is derived, never assumed, and silence is not health
// ---------------------------------------------------------------------------

func TestProviderHealthIsDerivedAndSilenceIsNotHealth(t *testing.T) {
	// (a) zero observations.
	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	for _, name := range application.DeclaredProviders() {
		entry := entryFor(t, providerRegistry(t, s), name)
		if entry.Health != application.ProviderHealthUnknown {
			t.Fatalf("(a) %s with zero observations reports health %q, want unknown", name, entry.Health)
		}
		if entry.Health == application.ProviderHealthHealthy {
			t.Fatalf("(a) %s reports healthy with no observation at all", name)
		}
		if !entry.Stale {
			t.Fatalf("(a) %s with zero observations reports stale=false", name)
		}
		if entry.ObservationCount != 0 {
			t.Fatalf("(a) %s observation_count=%d, want 0", name, entry.ObservationCount)
		}
		if entry.BlockedReason != application.ProviderBlockedNeverInvoked {
			t.Fatalf("(a) %s blocked_reason=%q, want never-invoked-by-loop", name, entry.BlockedReason)
		}
		if entry.LastObservedAt != "" {
			t.Fatalf("(a) %s synthesized a last_observed_at %q for an absence", name, entry.LastObservedAt)
		}
		if entry.VerifiedByLoopInvocation {
			t.Fatalf("(a) %s reports verified with no observation", name)
		}
		if entry.RunawayDetection.State != application.ProviderRunawayUnknown {
			t.Fatalf("(a) %s runaway state=%q, want unknown", name, entry.RunawayDetection.State)
		}
	}

	// (b) one successful observation in the window.
	putObservation(t, st, observation(application.ProviderClaude, "", false, providerBase))
	entry := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	if entry.Health != application.ProviderHealthHealthy || entry.BlockedReason != application.ProviderNotBlocked {
		t.Fatalf("(b) health=%q blocked=%q, want healthy and empty", entry.Health, entry.BlockedReason)
	}
	if !entry.VerifiedByLoopInvocation {
		t.Fatalf("(b) verified_by_loop_invocation=false after a completed invocation")
	}
	if entry.Stale || entry.ObservationCount != 1 || entry.LastObservedAt == "" {
		t.Fatalf("(b) entry=%#v", entry)
	}
	if entry.RunawayDetection.State != application.ProviderRunawayWithinThresholds {
		t.Fatalf("(b) runaway state=%q, want within-thresholds", entry.RunawayDetection.State)
	}

	// (c) a retryable failure class.
	s, st = providerService(t, clk)
	putObservation(t, st, observation(application.ProviderClaude, application.ProviderFailureTransport, false, providerBase))
	entry = entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	if entry.Health != application.ProviderHealthDegraded {
		t.Fatalf("(c) retryable failure reports health %q, want degraded", entry.Health)
	}

	// (d) a non-retryable failure class.
	s, st = providerService(t, clk)
	putObservation(t, st, observation(application.ProviderClaude, application.ProviderFailureModel, false, providerBase))
	entry = entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	if entry.Health != application.ProviderHealthUnavailable {
		t.Fatalf("(d) non-retryable failure reports health %q, want unavailable", entry.Health)
	}

	// (e) provider-unauthenticated.
	s, st = providerService(t, clk)
	putObservation(t, st, observation(application.ProviderCodex, application.ProviderFailureUnauthenticated, false, providerBase))
	entry = entryFor(t, providerRegistry(t, s), application.ProviderCodex)
	if entry.Health != application.ProviderHealthUnauthenticated {
		t.Fatalf("(e) health=%q, want unauthenticated", entry.Health)
	}
	if entry.BlockedReason != "owner-must-authenticate-cli-on-runner-machine" {
		t.Fatalf("(e) blocked_reason=%q, want owner-must-authenticate-cli-on-runner-machine", entry.BlockedReason)
	}
	if entry.VerifiedByLoopInvocation {
		t.Fatal("(e) an unauthenticated failure verified the provider")
	}

	// (f) an observation older than the declared window. The previous health
	// value is preserved and reported as stale rather than silently refreshed
	// -- and rather than silently downgraded, because "healthy yesterday, not
	// exercised since" and "never exercised" are different facts.
	s, st = providerService(t, clk)
	putObservation(t, st, observation(application.ProviderClaude, "", false, providerBase))
	fresh := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	clk.Set(providerBase.Add(application.ProviderObservationWindow + time.Second))
	stale := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	if !stale.Stale {
		t.Fatal("(f) an observation older than the window is not reported stale")
	}
	if stale.Health != fresh.Health {
		t.Fatalf("(f) health changed from %q to %q as the observation aged; a stale value must be preserved and reported as stale", fresh.Health, stale.Health)
	}
	if stale.LastObservedAt != fresh.LastObservedAt {
		t.Fatalf("(f) last_observed_at was refreshed from %q to %q", fresh.LastObservedAt, stale.LastObservedAt)
	}
	if stale.ObservationCount != 0 {
		t.Fatalf("(f) observation_count=%d, want 0: no retained observation falls inside the window", stale.ObservationCount)
	}
	// The one value age does change, in the direction that hides nothing: a
	// stale "no stop reported" is not a statement about now.
	if stale.RunawayDetection.State != application.ProviderRunawayUnknown {
		t.Fatalf("(f) runaway state=%q, want unknown once the observation is stale", stale.RunawayDetection.State)
	}
	// Exactly at the window boundary the entry is still fresh.
	clk.Set(providerBase.Add(application.ProviderObservationWindow))
	if entryFor(t, providerRegistry(t, s), application.ProviderClaude).Stale {
		t.Fatal("(f) an observation exactly at the window edge is reported stale")
	}
	clk.Set(providerBase)
}

// TestProviderObservationRetentionIsBounded is A5(g): the window and the ring
// bound are named constants, and the retained count is bounded by the ring.
func TestProviderObservationRetentionIsBounded(t *testing.T) {
	if application.ProviderObservationWindow <= 0 {
		t.Fatal("ProviderObservationWindow is not a positive declared constant")
	}
	if application.MaxProviderObservations <= 0 {
		t.Fatal("MaxProviderObservations is not a positive declared constant")
	}
	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	const extra = 5
	total := application.MaxProviderObservations + extra
	for i := 0; i < total; i++ {
		putObservation(t, st, observation(application.ProviderClaude, application.ProviderFailureTransport, false, providerBase.Add(time.Duration(i)*time.Minute)))
	}
	clk.Set(providerBase.Add(time.Duration(total) * time.Minute))

	// The store retained exactly the bound, newest first.
	ctx := context.Background()
	var log application.ProviderObservationLog
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		var e error
		log, e = u.ProviderObservations(ctx, application.ProviderClaude)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(log.Observations) != application.MaxProviderObservations {
		t.Fatalf("stored %d observations after driving %d, want the declared bound %d", len(log.Observations), total, application.MaxProviderObservations)
	}
	newest := providerBase.Add(time.Duration(total-1) * time.Minute)
	if !log.Observations[0].ObservedAt.Equal(newest) {
		t.Fatalf("newest retained observation is %s, want %s", log.Observations[0].ObservedAt, newest)
	}
	oldestKept := providerBase.Add(time.Duration(extra) * time.Minute)
	if !log.Observations[len(log.Observations)-1].ObservedAt.Equal(oldestKept) {
		t.Fatalf("oldest retained observation is %s, want %s; the ring dropped the wrong end", log.Observations[len(log.Observations)-1].ObservedAt, oldestKept)
	}
	// And the view reports no more than the bound.
	entry := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	if entry.ObservationCount != application.MaxProviderObservations {
		t.Fatalf("observation_count=%d, want %d", entry.ObservationCount, application.MaxProviderObservations)
	}
}

// ---------------------------------------------------------------------------
// A6: a stop for inspection is neither success nor failure
// ---------------------------------------------------------------------------

func TestProviderStopForInspectionIsNeitherSuccessNorFailure(t *testing.T) {
	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	putAssignment(t, st, application.ProviderAssignment{ExecutionID: "execution-a", IncrementID: "increment-1", Provider: application.ProviderClaude, Since: providerBase}, domain.ExecutionRunning)
	putObservation(t, st, application.ProviderObservation{Provider: application.ProviderClaude, StoppedForInspection: true, ObservedAt: providerBase})

	view := providerRegistry(t, s)
	entry := entryFor(t, view, application.ProviderClaude)
	if entry.Health != application.ProviderHealthStoppedForInspection {
		t.Fatalf("health=%q, want stopped-for-inspection", entry.Health)
	}
	if entry.Health == application.ProviderHealthUnavailable {
		t.Fatal("a stop for inspection was reported as unavailable")
	}
	if entry.RunawayDetection.State != application.ProviderRunawayStoppedForInspection {
		t.Fatalf("runaway state=%q, want stopped-for-inspection", entry.RunawayDetection.State)
	}
	if entry.BlockedReason != application.ProviderBlockedOwnerMustClearRunawayStop {
		t.Fatalf("blocked_reason=%q", entry.BlockedReason)
	}
	// It is verified by no invocation: a stop is not a completion.
	if entry.VerifiedByLoopInvocation {
		t.Fatal("a stop for inspection verified the provider")
	}
	// The entry still reports its assignments and its concurrency block.
	if len(entry.Assignments) != 1 || entry.Assignments[0].ExecutionID != "execution-a" {
		t.Fatalf("a stopped provider stopped reporting its assignments: %#v", entry.Assignments)
	}
	if entry.Concurrency.ActiveAssignments != 1 || entry.Concurrency.DeclaredCeiling != application.ProviderConcurrencyDesignCeiling {
		t.Fatalf("a stopped provider stopped reporting concurrency: %#v", entry.Concurrency)
	}

	// It is counted in no failure or degraded tally anywhere in the view --
	// there is no such tally at all, asserted mechanically over every JSON key.
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	allowedCountKeys := map[string]bool{"observation_count": true, "active_assignments": true}
	tallyShaped := []string{"count", "total", "failure", "failures", "failed", "degraded", "tally", "num"}
	for _, key := range jsonObjectKeys(t, body) {
		if allowedCountKeys[key] {
			continue
		}
		lower := strings.ToLower(key)
		for _, shape := range tallyShaped {
			if strings.Contains(lower, shape) {
				t.Fatalf("the view reports a tally-shaped key %q; a stop for inspection would eventually be miscounted in it", key)
			}
		}
	}

	// A stop does not make a later completion impossible to record.
	clk.Set(providerBase.Add(time.Minute))
	putObservation(t, st, observation(application.ProviderClaude, "", false, providerBase.Add(time.Minute)))
	after := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	if after.Health != application.ProviderHealthHealthy || !after.VerifiedByLoopInvocation {
		t.Fatalf("a completion after a stop was not recorded: %#v", after)
	}
	if after.RunawayDetection.State != application.ProviderRunawayWithinThresholds {
		t.Fatalf("runaway state after a fresh completion = %q", after.RunawayDetection.State)
	}

	// And the view has no mutation path for clearing a stop: the read model
	// declares no method that could, and the Service exposes none either.
	if names := methodsMatching(t, "provider_registry.go", []string{"Clear", "Reset", "Resume", "Acknowledge", "Ack", "Override", "SetRunaway", "SetHealth", "SetThreshold"}); len(names) != 0 {
		t.Fatalf("provider_registry.go declares %v; clearing a stop requires the owner to issue a new approved record and cannot be done from this surface", names)
	}

	// A stale stop is still reported as a stop: a stop that aged out into
	// silence would be a stop nobody was told about.
	s, st = providerService(t, clk)
	putObservation(t, st, application.ProviderObservation{Provider: application.ProviderCodex, StoppedForInspection: true, ObservedAt: providerBase})
	clk.Set(providerBase.Add(application.ProviderObservationWindow + time.Hour))
	staleStop := entryFor(t, providerRegistry(t, s), application.ProviderCodex)
	if staleStop.Health != application.ProviderHealthStoppedForInspection || staleStop.RunawayDetection.State != application.ProviderRunawayStoppedForInspection {
		t.Fatalf("a stale stop was erased by age: %#v", staleStop)
	}
	if !staleStop.Stale {
		t.Fatal("a stale stop did not report its age")
	}
}

// methodsMatching returns the declared function and method names in the named
// file of this package that contain any of the given substrings.
func methodsMatching(t *testing.T, path string, substrings []string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := []string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		for _, sub := range substrings {
			if strings.Contains(fn.Name.Name, sub) {
				out = append(out, fn.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// jsonObjectKeys returns every object key appearing anywhere in a JSON
// document, at any depth.
func jsonObjectKeys(t *testing.T, body []byte) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch value := v.(type) {
		case map[string]any:
			for k, inner := range value {
				seen[k] = true
				walk(inner)
			}
		case []any:
			for _, inner := range value {
				walk(inner)
			}
		}
	}
	walk(doc)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// A7: the naming requirement is enforced mechanically
// ---------------------------------------------------------------------------

// monetaryVocabulary is the closed deny list of dp-v2-067 d6. Reaching a
// runaway threshold is a stop for inspection, never a billing event, and a
// field named for money would reintroduce exactly the reading the standing
// authorization denies.
var monetaryVocabulary = []string{"budget", "quota", "billing", "spend", "cost", "credit"}

func matchesMonetaryVocabulary(name string) (string, bool) {
	lower := strings.ToLower(strings.ReplaceAll(name, "_", ""))
	for _, entry := range monetaryVocabulary {
		if strings.Contains(lower, entry) {
			return entry, true
		}
	}
	return "", false
}

// TestMonetaryVocabularyMatcherIsVerified checks the matcher against a
// known-positive and a known-negative before the scan below trusts it.
func TestMonetaryVocabularyMatcherIsVerified(t *testing.T) {
	positives := []string{"remaining_budget_usd", "budget", "provider-quota", "quota_exhausted", "billing_account", "total_spend", "max_total_cost_usd", "remaining_credit", "worst_case_reservation_usd_cost"}
	for _, name := range positives {
		if _, ok := matchesMonetaryVocabulary(name); !ok {
			t.Fatalf("known-positive %q was not matched by the monetary matcher", name)
		}
	}
	negatives := []string{"runaway_detection", "within-thresholds", "stopped-for-inspection", "active_assignments", "declared_ceiling", "ceiling_source", "remaining", "exhausted", "observation_count", "blocked_reason", "verified_by_loop_invocation", "provider-rate-limited", "thresholds_declared_in", "last_observed_at", "stale"}
	for _, name := range negatives {
		if entry, ok := matchesMonetaryVocabulary(name); ok {
			t.Fatalf("known-negative %q was matched on %q", name, entry)
		}
	}
}

// fullyPopulatedRegistry drives every field of the read model to a non-zero
// value so the scans below have something to walk.
func fullyPopulatedRegistry(t *testing.T) (application.ProviderRegistryView, []byte) {
	t.Helper()
	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	putObservation(t, st, observation(application.ProviderClaude, "", false, providerBase))
	putObservation(t, st, observation(application.ProviderCodex, application.ProviderFailureUnauthenticated, false, providerBase))
	putObservation(t, st, application.ProviderObservation{Provider: application.ProviderOpenCode, StoppedForInspection: true, ObservedAt: providerBase})
	putAssignment(t, st, application.ProviderAssignment{ExecutionID: "execution-a", IncrementID: "increment-1", Provider: application.ProviderClaude, Since: providerBase}, domain.ExecutionRunning)
	putAssignment(t, st, application.ProviderAssignment{ExecutionID: "execution-b", IncrementID: "increment-2", Provider: application.ProviderCodex, Since: providerBase}, domain.ExecutionRunning)
	putAssignment(t, st, application.ProviderAssignment{ExecutionID: "execution-c", IncrementID: "increment-3", Provider: application.ProviderOpenCode, Since: providerBase}, domain.ExecutionRunning)
	view := providerRegistry(t, s)
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	return view, body
}

func TestProviderRegistryNamesNoMonetaryVocabularyAndNoThreshold(t *testing.T) {
	view, body := fullyPopulatedRegistry(t)

	// Half one: every object key of the marshalled read model.
	keys := jsonObjectKeys(t, body)
	if len(keys) < 20 {
		t.Fatalf("the key walk found only %d keys; it is not seeing the whole document", len(keys))
	}
	for _, key := range keys {
		if entry, bad := matchesMonetaryVocabulary(key); bad {
			t.Fatalf("read model key %q matches the monetary deny list on %q", key, entry)
		}
	}

	// And every enum value the read model actually emits.
	emitted := []string{}
	for _, e := range view.Providers {
		emitted = append(emitted, string(e.Provider), string(e.Health), string(e.BlockedReason), string(e.RunawayDetection.State), string(e.Concurrency.CeilingSource))
	}
	// Plus every declared value of every enum, emitted or not.
	for _, v := range []string{
		"unknown", "healthy", "degraded", "unavailable", "unauthenticated", "stopped-for-inspection",
		"never-invoked-by-loop", "owner-must-authenticate-cli-on-runner-machine",
		"last-invocation-failed-retryably", "last-invocation-failed-non-retryably",
		"last-invocation-failed-without-a-classified-reason",
		"owner-must-clear-the-runaway-stop-with-a-new-approved-record",
		"within-thresholds", "architecture-design-ceiling",
		"provider-unauthenticated", "provider-transport", "provider-rate-limited", "timeout", "provider-model", "contract-incompatible",
	} {
		emitted = append(emitted, v)
	}
	for _, value := range emitted {
		if value == "" {
			continue
		}
		if entry, bad := matchesMonetaryVocabulary(value); bad {
			t.Fatalf("enum value %q matches the monetary deny list on %q", value, entry)
		}
	}

	// Half two: the openapi schema block this task added.
	block := openapiProviderSchemaBlock(t)
	blockKeys, blockEnums := yamlishKeysAndEnums(block)
	if len(blockKeys) < 20 || len(blockEnums) < 15 {
		t.Fatalf("the openapi block extraction found %d keys and %d enum values; it is not seeing the block", len(blockKeys), len(blockEnums))
	}
	for _, key := range blockKeys {
		if entry, bad := matchesMonetaryVocabulary(key); bad {
			t.Fatalf("openapi key %q matches the monetary deny list on %q", key, entry)
		}
	}
	for _, value := range blockEnums {
		if entry, bad := matchesMonetaryVocabulary(value); bad {
			t.Fatalf("openapi enum value %q matches the monetary deny list on %q", value, entry)
		}
	}

	// No numeric runaway threshold value appears in the read model, in the port
	// types, in the stored record or in the schema block. The three approved
	// thresholds are 16 invocations, 10.00 USD total and 2.00 USD per
	// invocation; the shapes below are how any of them would be spelled.
	thresholdShapes := []string{"16", "10.0", "10.00", "2.0", "2.00", "usd", "USD"}
	for _, shape := range thresholdShapes {
		if strings.Contains(string(body), shape) {
			t.Fatalf("the read model carries %q, which is the shape of an approved runaway threshold: %s", shape, body)
		}
		if strings.Contains(block, shape) {
			t.Fatalf("the openapi provider schema block carries %q", shape)
		}
	}
	// The one place a number may appear is the concurrency block, whose values
	// are control-plane quantities and not thresholds.
	if entryFor(t, view, application.ProviderClaude).Concurrency.DeclaredCeiling != application.ProviderConcurrencyDesignCeiling {
		t.Fatal("the declared ceiling is not the architecture design ceiling")
	}
	// And the runaway block carries no number at all.
	for _, e := range view.Providers {
		rd, err := json.Marshal(e.RunawayDetection)
		if err != nil {
			t.Fatal(err)
		}
		if e.RunawayDetection.ThresholdsDeclaredIn != application.ProviderRunawayThresholdsDeclaredIn {
			t.Fatalf("thresholds_declared_in = %q, want the declared constant", e.RunawayDetection.ThresholdsDeclaredIn)
		}
		if !strings.Contains(e.RunawayDetection.ThresholdsDeclaredIn, application.ProviderStandingAuthorizationRef) {
			t.Fatalf("thresholds_declared_in does not name an approved record: %q", e.RunawayDetection.ThresholdsDeclaredIn)
		}
		// Outside the record reference it names, the block carries no digit at
		// all: no invocation count, no monetary figure, no window.
		scrubbed := strings.ReplaceAll(string(rd), e.RunawayDetection.ThresholdsDeclaredIn, "")
		if regexp.MustCompile(`[0-9]`).MatchString(scrubbed) {
			t.Fatalf("the runaway detection block carries a digit outside the record reference it names: %s", rd)
		}
	}
}

// openapiProviderSchemaBlock returns the text of the schema block this task
// added to the openapi document, and fails if it cannot be located.
func openapiProviderSchemaBlock(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(openapiPath())
	if err != nil {
		t.Fatalf("read %s: %v", openapiPath(), err)
	}
	text := string(data)
	start := strings.Index(text, "    ProviderObservation:\n")
	if start < 0 {
		t.Fatal("could not find the ProviderObservation schema; the block extraction is broken")
	}
	rest := text[start:]
	end := strings.Index(rest, "    ProcessObservation:\n")
	if end < 0 {
		t.Fatal("could not find the end of the provider schema block")
	}
	return rest[:end]
}

var yamlKeyLine = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*):`)
var yamlInlineKey = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*):\s*\{`)
var yamlEnumList = regexp.MustCompile(`enum:\s*\[([^\]]*)\]`)

// yamlishKeysAndEnums extracts declared keys and enum values from a block of
// this document's YAML without a YAML parser. It is deliberately textual: the
// point is to catch a forbidden spelling wherever it appears, and a parser
// would need the whole document to be loadable first.
func yamlishKeysAndEnums(block string) (keys, enums []string) {
	seenKey := map[string]bool{}
	add := func(k string) {
		if !seenKey[k] {
			seenKey[k] = true
			keys = append(keys, k)
		}
	}
	for _, m := range yamlKeyLine.FindAllStringSubmatch(block, -1) {
		add(m[1])
	}
	for _, m := range yamlInlineKey.FindAllStringSubmatch(block, -1) {
		add(m[1])
	}
	for _, m := range yamlEnumList.FindAllStringSubmatch(block, -1) {
		for _, raw := range strings.Split(m[1], ",") {
			value := strings.Trim(strings.TrimSpace(raw), "'\"")
			if value != "" {
				enums = append(enums, value)
			}
		}
	}
	sort.Strings(keys)
	sort.Strings(enums)
	return keys, enums
}

// ---------------------------------------------------------------------------
// A8: the authentication wait is observable, and the approver is not
// ---------------------------------------------------------------------------

// standingAuthorizationRecord is the in-repository record this task reads by
// id only. The test reads the file to pin the constant against reality; the
// production code never does.
func standingAuthorizationRecord(t *testing.T) map[string]any {
	t.Helper()
	path := filepath.Join(repoRoot(), ".agents", "v2", "packets", "provider-standing-authorization.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return doc
}

func TestUnauthenticatedProvidersAreObservableAsTheAuthenticationWait(t *testing.T) {
	// The measured present state, as a fixture: the standing authorization
	// covers all three, and only claude has ever completed an invocation.
	record := standingAuthorizationRecord(t)
	if record["id"] != application.ProviderStandingAuthorizationRef {
		t.Fatalf("the standing authorization record id is %v but the constant is %q", record["id"], application.ProviderStandingAuthorizationRef)
	}
	covered, ok := record["providers"].([]any)
	if !ok || len(covered) != 3 {
		t.Fatalf("the standing authorization covers %v, want three providers", record["providers"])
	}
	for i, name := range []string{"codex", "claude", "opencode"} {
		if covered[i] != name {
			t.Fatalf("the record covers %v at index %d, want %q", covered[i], i, name)
		}
	}
	approver, _ := record["approver"].(string)
	if approver == "" || !strings.Contains(approver, "@") {
		t.Fatalf("the record's approver %q is not the email shape this test uses as its positive control", approver)
	}

	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	// Only claude has completed an invocation. codex reported that its CLI has
	// no session; opencode has never been invoked at all.
	putObservation(t, st, observation(application.ProviderClaude, "", false, providerBase))
	putObservation(t, st, observation(application.ProviderCodex, application.ProviderFailureUnauthenticated, false, providerBase))

	view := providerRegistry(t, s)
	for _, name := range application.DeclaredProviders() {
		entry := entryFor(t, view, name)
		if !entry.Authorized {
			t.Fatalf("%s reports authorized=false, but %s covers it", name, application.ProviderStandingAuthorizationRef)
		}
		if entry.AuthorizationRef != application.ProviderStandingAuthorizationRef {
			t.Fatalf("%s authorization_ref=%q", name, entry.AuthorizationRef)
		}
	}
	if !entryFor(t, view, application.ProviderClaude).VerifiedByLoopInvocation {
		t.Fatal("claude is not reported as verified by a Loop invocation")
	}
	for _, name := range []application.ProviderName{application.ProviderCodex, application.ProviderOpenCode} {
		entry := entryFor(t, view, name)
		if entry.VerifiedByLoopInvocation {
			t.Fatalf("%s is reported as verified by a Loop invocation", name)
		}
		switch entry.Health {
		case application.ProviderHealthUnauthenticated, application.ProviderHealthUnknown:
		default:
			t.Fatalf("%s health=%q, want unauthenticated or unknown", name, entry.Health)
		}
		if entry.BlockedReason == application.ProviderNotBlocked {
			t.Fatalf("%s carries no blocked_reason naming the human action", name)
		}
		if !strings.Contains(string(entry.BlockedReason), "owner-must") && entry.BlockedReason != application.ProviderBlockedNeverInvoked {
			t.Fatalf("%s blocked_reason=%q does not name a human action", name, entry.BlockedReason)
		}
	}
	// codex's reason names the action; opencode's says no invocation was made.
	if entryFor(t, view, application.ProviderCodex).BlockedReason != application.ProviderBlockedOwnerMustAuthenticate {
		t.Fatalf("codex blocked_reason=%q", entryFor(t, view, application.ProviderCodex).BlockedReason)
	}
	if entryFor(t, view, application.ProviderOpenCode).BlockedReason != application.ProviderBlockedNeverInvoked {
		t.Fatalf("opencode blocked_reason=%q", entryFor(t, view, application.ProviderOpenCode).BlockedReason)
	}

	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}

	// The two blocked entries are excluded from every assignable or eligible
	// count the view reports -- and the view reports none at all, which is
	// asserted rather than assumed.
	for _, key := range jsonObjectKeys(t, body) {
		lower := strings.ToLower(key)
		for _, shape := range []string{"assignable", "eligible", "available_provider", "usable"} {
			if strings.Contains(lower, shape) {
				t.Fatalf("the view reports %q; a blocked entry could be counted in it", key)
			}
		}
	}

	// The approver never appears. The positive control comes first: a synthetic
	// entry carrying the record's own approver value must be caught by the
	// same scan, so a passing negative below is not a broken matcher.
	control := view
	control.Providers = append([]application.ProviderEntryView(nil), view.Providers...)
	control.Providers[0].AuthorizationRef = approver
	controlBody, err := json.Marshal(control)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(controlBody), "@") {
		t.Fatal("positive control: an approver-shaped authorization_ref did not produce an '@' in the body; the scan is broken")
	}
	if !strings.Contains(string(controlBody), approver) {
		t.Fatal("positive control: the approver value is not detectable in the body")
	}
	if strings.Contains(string(body), "@") {
		t.Fatalf("the response body contains an '@': %s", body)
	}
	if strings.Contains(string(body), approver) {
		t.Fatalf("the response body carries the approver value: %s", body)
	}
	// And the approver is nowhere in the production source of this package
	// either, so it cannot reach a log line.
	for _, path := range mustGlob(t, "*.go") {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), approver) {
			t.Fatalf("%s carries the approver value", path)
		}
		if strings.Contains(string(data), "@") && !strings.HasPrefix(path, "outbox") {
			// Not a hard failure by itself, but the registry file must be clean.
			if path == "provider_registry.go" {
				t.Fatalf("provider_registry.go carries an '@'")
			}
		}
	}
}

func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("glob %q matched nothing", pattern)
	}
	sort.Strings(matches)
	return matches
}

// ---------------------------------------------------------------------------
// A9: assignment
// ---------------------------------------------------------------------------

// startedExecution drives the real lifecycle to a started Execution, naming
// provider on start when one is given, and returns the ids and the version.
func startedExecution(t *testing.T, s *application.Service, st *memory.Store, suffix string, provider application.ProviderName) (executionID, leaseID, incrementID string, version domain.Version, fencing domain.FencingToken, startErr error) {
	t.Helper()
	ctx := owner(context.Background())
	captured, err := s.Capture(ctx, application.CaptureRequest{RequestID: "capture-" + suffix, Text: "provider registry " + suffix})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan-" + suffix, RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatal(err)
	}
	bare := context.Background()
	if err = st.Transact(bare, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(bare, planned.IncrementID)
		if e != nil {
			return e
		}
		aid, _ := domain.NewActorID("actor-1")
		next, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: aid, At: providerBase, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(bare, next, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(runner(ctx, "runner-1"), application.ClaimRequest{RequestID: "claim-" + suffix, IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.Start(runner(ctx, "runner-1"), application.StartRequest{RequestID: "start-" + suffix, ExecutionID: claimed.ExecutionID, ExpectedExecutionVersion: 1, Provider: provider})
	if err != nil {
		return claimed.ExecutionID, claimed.LeaseID, planned.IncrementID, 0, claimed.FencingToken, err
	}
	return claimed.ExecutionID, claimed.LeaseID, planned.IncrementID, started.Version, claimed.FencingToken, nil
}

func TestProviderAssignmentIsASideTableKeyedByExecution(t *testing.T) {
	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)

	// (a) starting with provider=claude puts the Execution in claude's
	// assignments and in no other Provider's.
	execID, leaseID, incID, version, fencing, err := startedExecution(t, s, st, "a", application.ProviderClaude)
	if err != nil {
		t.Fatalf("start with provider=claude: %v", err)
	}
	view := providerRegistry(t, s)
	claude := entryFor(t, view, application.ProviderClaude)
	if len(claude.Assignments) != 1 || claude.Assignments[0].ExecutionID != execID {
		t.Fatalf("claude assignments = %#v, want exactly the started execution %s", claude.Assignments, execID)
	}
	if claude.Assignments[0].IncrementID != incID {
		t.Fatalf("assignment increment_id=%q, want %q", claude.Assignments[0].IncrementID, incID)
	}
	if claude.Assignments[0].Since == "" {
		t.Fatal("the assignment carries no since instant")
	}
	if claude.Concurrency.ActiveAssignments != 1 || claude.Concurrency.Remaining != application.ProviderConcurrencyDesignCeiling-1 || claude.Concurrency.Exhausted {
		t.Fatalf("claude concurrency = %#v", claude.Concurrency)
	}
	for _, other := range []application.ProviderName{application.ProviderCodex, application.ProviderOpenCode} {
		if got := entryFor(t, view, other); len(got.Assignments) != 0 || got.Concurrency.ActiveAssignments != 0 {
			t.Fatalf("%s reports the assignment too: %#v", other, got)
		}
	}
	// The side table really is keyed by Execution id.
	bare := context.Background()
	var stored application.ProviderAssignment
	var found bool
	if err := st.Transact(bare, func(u application.UnitOfWork) error {
		var e error
		stored, found, e = u.ProviderAssignment(bare, execID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if !found || stored.Provider != application.ProviderClaude || stored.ExecutionID != execID {
		t.Fatalf("keyed read of the side table = %#v found=%v", stored, found)
	}
	// internal/domain is untouched: the Execution aggregate has no Provider
	// field to carry this at all.
	if fields := domainExecutionFieldNames(t); containsAny(fields, []string{"Provider", "ProviderName", "Adapter"}) {
		t.Fatalf("domain.Execution declares a Provider field: %v", fields)
	}

	// (b) a terminal Execution is no longer an active assignment.
	if _, err := s.AcceptResult(runner(owner(bare), "runner-1"), application.AcceptResultRequest{
		RequestID: "result-a", ExecutionID: execID, LeaseID: leaseID,
		ExpectedExecutionVersion: version, FencingToken: fencing, Succeeded: true,
	}); err != nil {
		t.Fatalf("accept result: %v", err)
	}
	after := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	if len(after.Assignments) != 0 || after.Concurrency.ActiveAssignments != 0 {
		t.Fatalf("a terminal execution is still reported as an active assignment: %#v", after)
	}

	// (c) starting without the field records no assignment.
	s2, st2 := providerService(t, clk)
	execID2, _, _, _, _, err := startedExecution(t, s2, st2, "c", "")
	if err != nil {
		t.Fatalf("start without provider: %v", err)
	}
	view2 := providerRegistry(t, s2)
	for _, name := range application.DeclaredProviders() {
		if got := entryFor(t, view2, name); len(got.Assignments) != 0 || got.Concurrency.ActiveAssignments != 0 {
			t.Fatalf("%s gained an assignment from a start that named no provider: %#v", name, got)
		}
	}
	if err := st2.Transact(bare, func(u application.UnitOfWork) error {
		_, ok, e := u.ProviderAssignment(bare, execID2)
		if ok {
			t.Fatalf("a start naming no provider wrote a side-table record for %s", execID2)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}

	// (d) an unknown provider value is rejected and records nothing.
	s3, st3 := providerService(t, clk)
	execID3, _, _, _, _, startErr := startedExecution(t, s3, st3, "d", application.ProviderName("gemini"))
	if startErr == nil {
		t.Fatal("start accepted an undeclared provider name")
	}
	if !errors.Is(startErr, application.ErrProviderUnknown) {
		t.Fatalf("start error = %v, want ErrProviderUnknown", startErr)
	}
	view3 := providerRegistry(t, s3)
	for _, name := range application.DeclaredProviders() {
		if got := entryFor(t, view3, name); len(got.Assignments) != 0 {
			t.Fatalf("%s gained an assignment from a refused start: %#v", name, got)
		}
	}
	// capture, plan and claim each recorded one event; the refused start
	// recorded none.
	if len(st3.Events()) != 3 {
		t.Fatalf("a refused start recorded %d events; only capture, plan and claim should have", len(st3.Events()))
	}
	if err := st3.Transact(bare, func(u application.UnitOfWork) error {
		exec, ok, e := u.Execution(bare, execID3)
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("the claimed execution disappeared")
		}
		if exec.Status == domain.ExecutionRunning || exec.Status == domain.ExecutionStarting {
			t.Fatalf("a refused start still transitioned the execution to %q", exec.Status)
		}
		_, assigned, e := u.ProviderAssignment(bare, execID3)
		if assigned {
			t.Fatal("a refused start wrote a side-table record")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}

	// (e) the assignment list is bounded, asserted by exceeding it.
	s4, st4 := providerService(t, clk)
	const extra = 3
	total := application.MaxProviderAssignments + extra
	for i := 0; i < total; i++ {
		putAssignment(t, st4, application.ProviderAssignment{
			ExecutionID: fmt.Sprintf("execution-%03d", i), IncrementID: "increment-1",
			Provider: application.ProviderCodex, Since: providerBase,
		}, domain.ExecutionRunning)
	}
	bounded := entryFor(t, providerRegistry(t, s4), application.ProviderCodex)
	if len(bounded.Assignments) != application.MaxProviderAssignments {
		t.Fatalf("assignments=%d after driving %d, want the declared bound %d", len(bounded.Assignments), total, application.MaxProviderAssignments)
	}
	if bounded.Assignments[0].ExecutionID != fmt.Sprintf("execution-%03d", extra) {
		t.Fatalf("the bound dropped the wrong end: first retained is %q", bounded.Assignments[0].ExecutionID)
	}
	// The bound is larger than the ceiling on purpose, so exceeding the ceiling
	// is reportable before the bound truncates.
	if bounded.Concurrency.ActiveAssignments <= bounded.Concurrency.DeclaredCeiling {
		t.Fatalf("active=%d ceiling=%d; the retention bound must exceed the ceiling", bounded.Concurrency.ActiveAssignments, bounded.Concurrency.DeclaredCeiling)
	}
	if !bounded.Concurrency.Exhausted || bounded.Concurrency.Remaining != 0 {
		t.Fatalf("concurrency = %#v, want exhausted with remaining 0", bounded.Concurrency)
	}
}

func containsAny(haystack, needles []string) bool {
	for _, h := range haystack {
		for _, n := range needles {
			if h == n {
				return true
			}
		}
	}
	return false
}

// domainExecutionFieldNames reads the Execution struct out of
// internal/domain/model.go. It is a read, never an edit: internal/domain is not
// touched by this task, and this is how that is asserted from inside a test.
func domainExecutionFieldNames(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(repoRoot(), "internal", "domain", "model.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := []string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name == nil || spec.Name.Name != "Execution" {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				out = append(out, name.Name)
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("found no fields on domain.Execution; the scan is broken")
	}
	return out
}

// ---------------------------------------------------------------------------
// A10: no prompt, no response, no credential, structurally
// ---------------------------------------------------------------------------

// forbiddenProviderFieldTokens is the deny list of V2-067 A10: a field of any
// of these shapes would be somewhere a prompt, a raw provider response or a
// session identifier could be put.
var forbiddenProviderFieldTokens = []string{"message", "detail", "output", "result", "session", "text"}

// providerCredentialDenyList is internal/domain's credential deny list with one
// entry removed, and the removal is deliberate and narrow.
//
// "authorization" is dropped because A4 mandates the field authorization_ref,
// whose value is the id of an in-repository approval record (psa-foundation-001)
// and never a credential or a person. Every other entry is kept, and
// TestProviderFieldMatchersAreVerified proves the remaining list still bites.
// The authorization_ref value itself is separately asserted to match a
// record-id shape and to contain no '@'.
var providerCredentialDenyList = []string{
	"password", "passwd", "credential", "credentials", "secret", "apikey",
	"privatekey", "accesstoken", "refreshtoken", "bearer", "sessioncookie",
	"rawprompt", "rawproviderout",
}

func TestProviderFieldMatchersAreVerified(t *testing.T) {
	for _, positive := range []string{"Message", "ErrorDetail", "RawOutput", "ResultText", "SessionID", "text"} {
		if _, ok := matchesAny(positive, forbiddenProviderFieldTokens); !ok {
			t.Fatalf("known-positive %q was not matched by the forbidden provider field list", positive)
		}
	}
	for _, negative := range []string{"Provider", "Health", "BlockedReason", "ObservedAt", "StoppedForInspection", "FailureClass", "ExecutionID", "IncrementID", "Since", "Stale", "ObservationCount"} {
		if entry, ok := matchesAny(negative, forbiddenProviderFieldTokens); ok {
			t.Fatalf("known-negative %q matched the forbidden provider field list on %q", negative, entry)
		}
	}
	for _, positive := range []string{"Password", "APIKey", "ClientSecret", "RawPrompt", "raw_provider_output", "SessionCookie"} {
		if _, ok := matchesAny(positive, providerCredentialDenyList); !ok {
			t.Fatalf("known-positive %q was not matched by the credential deny list", positive)
		}
	}
	for _, negative := range []string{"AuthorizationRef", "Authorized", "FencingToken", "VerifiedAt", "ThresholdsDeclaredIn"} {
		if entry, ok := matchesAny(negative, providerCredentialDenyList); ok {
			t.Fatalf("known-negative %q matched the credential deny list on %q", negative, entry)
		}
	}
	// And the one dropped entry really is the only difference: "authorization"
	// would have matched AuthorizationRef, which is why it is dropped and why
	// the value is asserted separately.
	if _, ok := matchesAny("AuthorizationRef", []string{"authorization"}); !ok {
		t.Fatal("the dropped entry no longer matches AuthorizationRef; re-check whether it can be restored")
	}
}

func TestProviderObservationShapeIsClosedAndCarriesNoText(t *testing.T) {
	files := applicationPackageFiles(t, false)
	targets := []string{
		"ProviderObservationInput", "ProviderObservation", "ProviderObservationLog", "ProviderAssignment",
		"ProviderAssignmentView", "ProviderRunawayDetectionView", "ProviderConcurrencyView",
		"ProviderEntryView", "ProviderRegistryView",
	}
	found := structFieldNames(t, files, targets)
	for _, target := range targets {
		names, ok := found[target]
		if !ok {
			t.Fatalf("the AST scan did not find type %s; the scan is broken or the type was renamed", target)
		}
		if len(names) == 0 {
			t.Fatalf("%s has no fields; the scan is broken", target)
		}
		for _, name := range names {
			if entry, bad := matchesAny(name, forbiddenProviderFieldTokens); bad {
				t.Fatalf("%s.%s matches the forbidden provider field list on %q", target, name, entry)
			}
			if entry, bad := matchesAny(name, providerCredentialDenyList); bad {
				t.Fatalf("%s.%s matches the credential deny list on %q", target, name, entry)
			}
			if entry, bad := matchesMonetaryVocabulary(name); bad {
				t.Fatalf("%s.%s matches the monetary deny list on %q", target, name, entry)
			}
		}
	}
	// The request DTO carries exactly three fields: a name, a closed failure
	// class and a boolean. No timestamp field of any spelling, so a Runner
	// clock cannot reach the record.
	if got := found["ProviderObservationInput"]; !reflect.DeepEqual(got, []string{"Name", "FailureClass", "StoppedForInspection"}) {
		t.Fatalf("ProviderObservationInput fields = %v, want exactly [Name FailureClass StoppedForInspection]", got)
	}
	// The stored record adds only the server-authoritative instant.
	if got := found["ProviderObservation"]; !reflect.DeepEqual(got, []string{"Provider", "FailureClass", "StoppedForInspection", "ObservedAt"}) {
		t.Fatalf("ProviderObservation fields = %v", got)
	}
	// The read model entry carries exactly the named fields and no others.
	wantEntry := []string{
		"Provider", "provider", "Authorized", "authorized", "AuthorizationRef", "authorization_ref",
		"VerifiedByLoopInvocation", "verified_by_loop_invocation", "Health", "health",
		"BlockedReason", "blocked_reason", "LastObservedAt", "last_observed_at",
		"ObservationCount", "observation_count", "Stale", "stale",
		"RunawayDetection", "runaway_detection", "Concurrency", "concurrency",
		"Assignments", "assignments",
	}
	if got := found["ProviderEntryView"]; !reflect.DeepEqual(got, wantEntry) {
		t.Fatalf("ProviderEntryView fields = %v, want %v", got, wantEntry)
	}

	// A failure class outside the closed enum is refused, and the refusal is a
	// shape error rather than a policy decision.
	if err := (application.ProviderObservationInput{Name: application.ProviderClaude, FailureClass: "provider-quota"}).Validate(); !errors.Is(err, application.ErrProviderObservationInvalid) {
		t.Fatalf("an undeclared failure class was accepted: %v", err)
	}
	if err := (application.ProviderObservationInput{Name: "gemini"}).Validate(); !errors.Is(err, application.ErrProviderUnknown) {
		t.Fatalf("an undeclared provider name was accepted: %v", err)
	}
	if err := (application.ProviderObservationInput{Name: application.ProviderClaude}).Validate(); err != nil {
		t.Fatalf("a bare completed observation was refused: %v", err)
	}

	// And an undeclared class records nothing at all through the real path.
	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	execID, leaseID, _, version, fencing, err := startedExecution(t, s, st, "obs", application.ProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	_ = execID
	bare := context.Background()
	_, err = s.AcceptResult(runner(owner(bare), "runner-1"), application.AcceptResultRequest{
		RequestID: "result-bad", ExecutionID: execID, LeaseID: leaseID,
		ExpectedExecutionVersion: version, FencingToken: fencing, Succeeded: true,
		ProviderObservation: &application.ProviderObservationInput{Name: application.ProviderClaude, FailureClass: "provider-quota"},
	})
	if !errors.Is(err, application.ErrProviderObservationInvalid) {
		t.Fatalf("accept-result with an undeclared failure class returned %v", err)
	}
	entry := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	if entry.ObservationCount != 0 || entry.Health != application.ProviderHealthUnknown {
		t.Fatalf("a refused observation was recorded: %#v", entry)
	}

	// The good path records exactly one observation.
	if _, err = s.AcceptResult(runner(owner(bare), "runner-1"), application.AcceptResultRequest{
		RequestID: "result-good", ExecutionID: execID, LeaseID: leaseID,
		ExpectedExecutionVersion: version, FencingToken: fencing, Succeeded: true,
		ProviderObservation: &application.ProviderObservationInput{Name: application.ProviderClaude},
	}); err != nil {
		t.Fatalf("accept-result with a valid observation: %v", err)
	}
	entry = entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	if entry.ObservationCount != 1 || entry.Health != application.ProviderHealthHealthy || !entry.VerifiedByLoopInvocation {
		t.Fatalf("the observation on the result was not recorded: %#v", entry)
	}
	if entry.LastObservedAt == "" {
		t.Fatal("observed_at was not set from the transaction authority time")
	}
}

// ---------------------------------------------------------------------------
// The owner read: role, and no write
// ---------------------------------------------------------------------------

func TestProvidersRequiresTheOwnerRole(t *testing.T) {
	s, _ := providerService(t, &fixedClock{now: providerBase})
	ctx := context.Background()
	if _, err := s.Providers(ctx); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("unauthenticated caller got %v", err)
	}
	if _, err := s.Providers(runner(ctx, "runner-1")); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("runner role got %v", err)
	}
	if _, err := s.Providers(owner(ctx)); err != nil {
		t.Fatalf("owner got %v", err)
	}
}

func TestProvidersPerformsNoWriteAndNoOutbox(t *testing.T) {
	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	putObservation(t, st, observation(application.ProviderClaude, "", false, providerBase))
	putAssignment(t, st, application.ProviderAssignment{ExecutionID: "execution-a", IncrementID: "increment-1", Provider: application.ProviderClaude, Since: providerBase}, domain.ExecutionRunning)

	beforeEvents := len(st.Events())
	beforeOutbox := len(st.Outbox())
	for i := 0; i < 4; i++ {
		providerRegistry(t, s)
	}
	if got := len(st.Events()); got != beforeEvents {
		t.Fatalf("the read recorded %d events", got-beforeEvents)
	}
	if got := len(st.Outbox()); got != beforeOutbox {
		t.Fatalf("the read enqueued %d outbox items", got-beforeOutbox)
	}
	// The stored state is byte-identical after the reads.
	ctx := context.Background()
	var log application.ProviderObservationLog
	var assignments []application.ProviderAssignment
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		var e error
		if log, e = u.ProviderObservations(ctx, application.ProviderClaude); e != nil {
			return e
		}
		assignments, e = u.ProviderAssignments(ctx, application.ProviderClaude)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(log.Observations) != 1 || len(assignments) != 1 {
		t.Fatalf("the read changed the stored state: observations=%d assignments=%d", len(log.Observations), len(assignments))
	}
}

// ---------------------------------------------------------------------------
// A12: the shared behavioural table, memory adapter
// ---------------------------------------------------------------------------

// TestProviderRegistryCasesAreARealTable guards the shared table against
// becoming vacuous.
func TestProviderRegistryCasesAreARealTable(t *testing.T) {
	cases := application.ProviderRegistryCases()
	if len(cases) < 8 {
		t.Fatalf("the shared behavioural table has %d cases, want at least 8", len(cases))
	}
	names := map[string]bool{}
	observationCases, assignmentCases := 0, 0
	for _, c := range cases {
		if c.Name == "" {
			t.Fatal("a case has no name")
		}
		if names[c.Name] {
			t.Fatalf("duplicate case name %q", c.Name)
		}
		names[c.Name] = true
		if !c.Query.Valid() {
			t.Fatalf("case %q queries the undeclared provider %q", c.Name, c.Query)
		}
		if len(c.Observations) > 0 {
			observationCases++
		}
		if len(c.Assignments) > 0 {
			assignmentCases++
		}
	}
	if observationCases == 0 || assignmentCases == 0 {
		t.Fatalf("the table covers %d observation cases and %d assignment cases; both must be exercised", observationCases, assignmentCases)
	}
}

// TestProviderRegistryBehaviouralTableOnMemory runs the shared table against
// internal/store/memory. internal/store/firestore runs the same table by value
// in its own test, so the memory store cannot pass a behaviour the Firestore
// store does not implement.
func TestProviderRegistryBehaviouralTableOnMemory(t *testing.T) {
	cases := application.ProviderRegistryCases()
	if len(cases) == 0 {
		t.Fatal("the shared behavioural table is empty")
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			st := memory.New()
			ctx := context.Background()
			for _, o := range c.Observations {
				value := o
				if err := st.Transact(ctx, func(u application.UnitOfWork) error {
					return u.SaveProviderObservation(ctx, value)
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, a := range c.Assignments {
				value := a
				if err := st.Transact(ctx, func(u application.UnitOfWork) error {
					return u.SaveProviderAssignment(ctx, value)
				}); err != nil {
					t.Fatal(err)
				}
			}
			var log application.ProviderObservationLog
			var assignments []application.ProviderAssignment
			if err := st.Transact(ctx, func(u application.UnitOfWork) error {
				var e error
				if log, e = u.ProviderObservations(ctx, c.Query); e != nil {
					return e
				}
				assignments, e = u.ProviderAssignments(ctx, c.Query)
				return e
			}); err != nil {
				t.Fatal(err)
			}
			if len(log.Observations) != len(c.WantObservations) {
				t.Fatalf("observations=%d want %d", len(log.Observations), len(c.WantObservations))
			}
			for i := range c.WantObservations {
				if !reflect.DeepEqual(log.Observations[i], c.WantObservations[i]) {
					t.Fatalf("observation %d = %#v, want %#v", i, log.Observations[i], c.WantObservations[i])
				}
			}
			if got := !log.VerifiedAt.IsZero(); got != c.WantVerified {
				t.Fatalf("verified=%v want %v (VerifiedAt=%s)", got, c.WantVerified, log.VerifiedAt)
			}
			if len(assignments) != len(c.WantAssignments) {
				t.Fatalf("assignments=%d want %d: %#v", len(assignments), len(c.WantAssignments), assignments)
			}
			for i := range c.WantAssignments {
				if !reflect.DeepEqual(assignments[i], c.WantAssignments[i]) {
					t.Fatalf("assignment %d = %#v, want %#v", i, assignments[i], c.WantAssignments[i])
				}
			}
		})
	}
}

// TestProviderRegistryRollbackLeaksNothing asserts the memory adapter copies
// the ring and the index on write, so a rolled-back transaction cannot leak an
// observation or an assignment into committed state.
func TestProviderRegistryRollbackLeaksNothing(t *testing.T) {
	st := memory.New()
	ctx := context.Background()
	abort := errors.New("abort")
	err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveProviderObservation(ctx, observation(application.ProviderClaude, "", false, providerBase)); e != nil {
			return e
		}
		if e := u.SaveProviderAssignment(ctx, application.ProviderAssignment{ExecutionID: "execution-a", IncrementID: "increment-1", Provider: application.ProviderClaude, Since: providerBase}); e != nil {
			return e
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("expected the rollback error, got %v", err)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		log, e := u.ProviderObservations(ctx, application.ProviderClaude)
		if e != nil {
			return e
		}
		if len(log.Observations) != 0 || !log.VerifiedAt.IsZero() {
			t.Fatalf("a rolled-back observation leaked: %#v", log)
		}
		assignments, e := u.ProviderAssignments(ctx, application.ProviderClaude)
		if e != nil {
			return e
		}
		if len(assignments) != 0 {
			t.Fatalf("a rolled-back assignment leaked: %#v", assignments)
		}
		_, ok, e := u.ProviderAssignment(ctx, "execution-a")
		if ok {
			t.Fatal("a rolled-back side-table record leaked")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}
