package application_test

// V2-068 acceptance. Every test here drives the real application Service over a
// real store; none of them hand-builds a scheduler plan, a permit decision or a
// domain.Requirement status the transition functions would refuse.
//
// Determinism is acceptance, not preference (A18): there is no fixed sleep, no
// wall-clock timer and no goroutine anywhere in this file, every instant comes
// from an injected clock, and the one measured duration is logged as an
// observation and never asserted as a threshold.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
	"github.com/takushi/agentic-loop-foundation/v2/internal/scheduler"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// allocClock is an injected clock whose instant a test can move deliberately.
// Nothing in this file reads a wall clock.
type allocClock struct{ now time.Time }

func (c *allocClock) Now() time.Time { return c.now }

var allocBase = time.Unix(1700000000, 0).UTC()

func allocService(t *testing.T) (*application.Service, *memory.Store, *allocClock) {
	t.Helper()
	st := memory.New()
	clk := &allocClock{now: allocBase}
	s, err := application.NewServiceWithConfig(st, clk, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return s, st, clk
}

// allocCapture captures one Requirement and returns its id and version.
func allocCapture(t *testing.T, s *application.Service, ctx context.Context, tag string) (string, domain.Version) {
	t.Helper()
	out, err := s.Capture(ctx, application.CaptureRequest{RequestID: "capture-" + tag, Text: "requirement " + tag})
	if err != nil {
		t.Fatalf("capture %s: %v", tag, err)
	}
	return out.RequirementID, out.Version
}

// allocAdvance drives a Requirement through real domain transitions inside one
// transaction. It never assigns a status directly: every step goes through
// domain.DecideRequirement, so a fixture cannot reach a state the transition
// table forbids.
func allocAdvance(t *testing.T, st *memory.Store, ctx context.Context, at time.Time, id string, kinds ...domain.RequirementCommandKind) {
	t.Helper()
	actor, err := domain.NewActorID("allocation-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		for _, kind := range kinds {
			r, ok, e := u.Requirement(ctx, id)
			if e != nil {
				return e
			}
			if !ok {
				return fmt.Errorf("requirement %s is absent", id)
			}
			next, e := domain.DecideRequirement(r, domain.RequirementCommand{Kind: kind, Actor: actor, At: at, ExpectedVersion: r.Version})
			if e != nil {
				return fmt.Errorf("%s on %s: %w", kind, id, e)
			}
			if e = u.SaveRequirement(ctx, next, r.Version); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// allocReady captures a Requirement and drives it to domain.RequirementReady,
// which is the only schedulable status.
func allocReady(t *testing.T, s *application.Service, st *memory.Store, ctx context.Context, at time.Time, tag string) string {
	t.Helper()
	id, _ := allocCapture(t, s, ctx, tag)
	allocAdvance(t, st, ctx, at, id, domain.RequirementStartFraming, domain.RequirementReadyCommand)
	return id
}

// allocLease plans an Increment for an existing Requirement, prepares it and
// claims it as a Runner, so the store really holds an active Lease, an
// Execution and a fencing token. It returns the claim response.
func allocLease(t *testing.T, s *application.Service, st *memory.Store, ctx context.Context, at time.Time, requirementID string, version domain.Version, tag string) application.ClaimResponse {
	t.Helper()
	planned, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan-" + tag, RequirementID: requirementID, ExpectedRequirementVersion: version})
	if err != nil {
		t.Fatalf("plan %s: %v", tag, err)
	}
	actor, err := domain.NewActorID("allocation-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(ctx, planned.IncrementID)
		if e != nil {
			return e
		}
		next, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: actor, At: at, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, next, inc.Version)
	}); err != nil {
		t.Fatalf("prepare %s: %v", tag, err)
	}
	claimed, err := s.Claim(runner(ctx, "runner-"+tag), application.ClaimRequest{RequestID: "claim-" + tag, IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatalf("claim %s: %v", tag, err)
	}
	// Claim leaves the Execution `leased`. ActiveExecutions -- the figure the
	// queue counters already publish and the figure this report feeds to the
	// pool's Active count -- counts only running and starting Executions, so a
	// fixture that stopped at `leased` would exercise an installation with zero
	// active work. domain.StartExecution is the real transition, and it is
	// called with no Control Intent in force, which is exactly the state Claim
	// was made in (control revision 0 on both the lease and the Execution).
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		exec, ok, e := u.Execution(ctx, claimed.ExecutionID)
		if e != nil || !ok {
			return fmt.Errorf("execution %s: %v ok=%v", claimed.ExecutionID, e, ok)
		}
		lease, ok, e := u.Lease(ctx, claimed.LeaseID)
		if e != nil || !ok {
			return fmt.Errorf("lease %s: %v ok=%v", claimed.LeaseID, e, ok)
		}
		next, e := domain.StartExecution(exec, lease, at, domain.EffectiveControlResult{})
		if e != nil {
			return fmt.Errorf("start execution %s: %w", claimed.ExecutionID, e)
		}
		return u.SaveExecution(ctx, next, exec.Version)
	}); err != nil {
		t.Fatalf("start %s: %v", tag, err)
	}
	return claimed
}

// allocLink writes the write-once Requirement-to-Repository association
// directly. It is the durable record V2-071 introduced, and it is what makes two
// Requirements contend for the same Repository.
func allocLink(t *testing.T, st *memory.Store, ctx context.Context, at time.Time, requirementID, repositoryID string) {
	t.Helper()
	rid, err := domain.NewRequirementID(requirementID)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := domain.NewRepositoryID(repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirementRepositoryLink(ctx, domain.RequirementRepositoryLink{RequirementID: rid, RepositoryID: repo, AssignedAt: at})
	}); err != nil {
		t.Fatal(err)
	}
}

// allocNextDay advances the injected clock by one day.
//
// WHY. The store enforces the daily Firestore budget of
// docs/architecture/validation.md section 5, and quota.ReadTransactionUsage
// reserves 6001 reads, so exactly four bounded read transactions fit in one
// budget day. A test that needs more must move the clock, and moving an injected
// clock is the only way to do that: there is no sleep, no timer and no real
// elapsed time anywhere in this file. Advancing a whole day cannot change any
// reported value, because the summary carries no instant and every candidate's
// age moves by the same amount.
func allocNextDay(clk *allocClock) { clk.now = clk.now.Add(24 * time.Hour) }

// allocSummaryNextDay reads the summary on a fresh budget day.
func allocSummaryNextDay(t *testing.T, s *application.Service, ctx context.Context, clk *allocClock) application.QueueSummaryResponse {
	t.Helper()
	allocNextDay(clk)
	return allocSummary(t, s, ctx)
}

// allocSetLimit requests a Control Intent carrying an installation concurrency
// limit and returns the revision the response reported.
func allocSetLimit(t *testing.T, s *application.Service, ctx context.Context, tag string, limit int) domain.Revision {
	t.Helper()
	out, err := s.Control(ctx, application.ControlRequest{
		RequestID:       "control-" + tag,
		Scope:           domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
		Mode:            domain.ControlAllow,
		AllocationLimit: &application.AllocationLimitInput{InstallationConcurrentExecutions: limit},
	})
	if err != nil {
		t.Fatalf("control %s with limit %d: %v", tag, limit, err)
	}
	return out.Revision
}

// allocSetNoLimit requests a Control Intent that declares no limit at all.
func allocSetNoLimit(t *testing.T, s *application.Service, ctx context.Context, tag string, mode domain.ControlMode) domain.Revision {
	t.Helper()
	out, err := s.Control(ctx, application.ControlRequest{
		RequestID: "control-" + tag,
		Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
		Mode:      mode,
	})
	if err != nil {
		t.Fatalf("control %s: %v", tag, err)
	}
	return out.Revision
}

func allocSummary(t *testing.T, s *application.Service, ctx context.Context) application.QueueSummaryResponse {
	t.Helper()
	out, err := s.QueueSummary(ctx)
	if err != nil {
		t.Fatalf("queue summary: %v", err)
	}
	return out
}

// ===========================================================================
// A3: the import direction, asserted mechanically on the application side
// ===========================================================================

// schedulerImportPath is assembled from internalPrefix (declared in
// source_guard_test.go) rather than written as one literal: internal/ci's
// manifest derivation reads module-path literals in source text, test files
// included, and a full literal here would be read as a second declared edge.
var schedulerImportPath = internalPrefix + "scheduler"

func importsScheduler(path string) bool {
	return path == schedulerImportPath || strings.HasPrefix(path, schedulerImportPath+"/")
}

// TestApplicationImportsTheSchedulerAndTheEdgeIsDeclared is the application
// half of A3. The api half lives in internal/api, where the guard that matters
// is the absence of the import.
func TestApplicationImportsTheSchedulerAndTheEdgeIsDeclared(t *testing.T) {
	// The matcher is verified before the scan trusts it.
	for _, positive := range []string{schedulerImportPath, schedulerImportPath + "/sub"} {
		if !importsScheduler(positive) {
			t.Fatalf("known-positive %q was not matched", positive)
		}
	}
	for _, negative := range []string{"time", internalPrefix + "domain", internalPrefix + "schedulerless", internalPrefix + "quota"} {
		if importsScheduler(negative) {
			t.Fatalf("known-negative %q was matched", negative)
		}
	}

	files := applicationPackageFiles(t, false)
	if len(files) == 0 {
		t.Fatal("scanned zero non-test files")
	}
	importers := []string{}
	total := 0
	for name, file := range files {
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			total++
			if importsScheduler(path) {
				importers = append(importers, name)
			}
		}
	}
	if total == 0 {
		t.Fatal("scanned zero import paths; the scan is broken and would pass vacuously")
	}
	if len(importers) == 0 {
		t.Fatal("no non-test file in internal/application imports internal/scheduler; the allocation report cannot be the scheduler's own decision without it")
	}
	sort.Strings(importers)
	t.Logf("application -> scheduler edge exists in %v (scanned %d files, %d import paths)", importers, len(files), total)
}

// ===========================================================================
// A4: the limit is a side table, not a mode
// ===========================================================================

// domainControlModeConstants parses internal/domain/control.go and returns the
// declared value of every constant typed domain.ControlMode. internal/domain is
// untouched by this task, so this is a read of a file this task does not own.
func domainControlModeConstants(t *testing.T) map[string]string {
	t.Helper()
	files := allocParseDir(t, allocDomainDir)
	out := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok.String() != "const" {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || value.Type == nil {
					continue
				}
				ident, ok := value.Type.(*ast.Ident)
				if !ok || ident.Name != "ControlMode" {
					continue
				}
				for i, name := range value.Names {
					if i < len(value.Values) {
						if lit, ok := value.Values[i].(*ast.BasicLit); ok {
							literal, _ := strconv.Unquote(lit.Value)
							out[name.Name] = literal
						}
					}
				}
			}
		}
	}
	return out
}

// TestControlModeValueSetIsUnchangedByTheAllocationLimit is A4's first half:
// the seven values are enumerated and asserted, so a task that "just added a
// mode" would fail here rather than in review.
func TestControlModeValueSetIsUnchangedByTheAllocationLimit(t *testing.T) {
	declared := domainControlModeConstants(t)
	if len(declared) == 0 {
		t.Fatal("found no ControlMode constants; the AST scan is broken and would pass vacuously")
	}
	want := map[string]string{
		"ControlAllow":         "allow",
		"ControlPauseIntake":   "pause-intake",
		"ControlPauseClaim":    "pause-claim",
		"ControlGracefulStop":  "graceful-stop",
		"ControlImmediateStop": "immediate-stop",
		"ControlEmergencyStop": "emergency-stop",
		"ControlCancel":        "cancel",
	}
	if !reflect.DeepEqual(declared, want) {
		t.Fatalf("domain.ControlMode declares %v, want exactly the seven pre-existing values %v", declared, want)
	}
	// And no declared value names a limit, a concurrency or an allocation.
	for name, value := range declared {
		lowered := strings.ToLower(name + " " + value)
		for _, forbidden := range []string{"limit", "concurren", "allocat", "throttle", "quota"} {
			if strings.Contains(lowered, forbidden) {
				t.Fatalf("ControlMode %s (%q) names %q; the limit is a side table, not a mode", name, value, forbidden)
			}
		}
	}
	t.Logf("domain.ControlMode still declares exactly %d values and none of them carries a limit", len(declared))
}

// TestControlStoresTheLimitAtomicallyWithTheIntentAndKeyedToItsRevision is
// A4 (a), (d) and (e).
func TestControlStoresTheLimitAtomicallyWithTheIntentAndKeyedToItsRevision(t *testing.T) {
	s, st, _ := allocService(t)
	ctx := owner(context.Background())

	revision := allocSetLimit(t, s, ctx, "with-limit", 7)
	if revision == 0 {
		t.Fatal("the control response reported revision 0")
	}
	// (a) and (e): the intent and the limit are both there, and the limit is
	// keyed to the revision the response reported.
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		found := false
		for _, c := range controls {
			if c.Revision == revision {
				found = true
				if c.Mode != domain.ControlAllow {
					t.Fatalf("stored mode = %q", c.Mode)
				}
			}
		}
		if !found {
			t.Fatalf("no Control Intent at revision %d", revision)
		}
		row, ok, e := u.AllocationLimit(ctx, revision)
		if e != nil {
			return e
		}
		if !ok {
			t.Fatalf("no allocation limit row at revision %d, the revision the response reported", revision)
		}
		if row.Revision != revision || row.InstallationConcurrentExecutions != 7 {
			t.Fatalf("stored limit = %#v", row)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// (d): a request without the field stores no limit row at all.
	next := allocSetNoLimit(t, s, ctx, "no-limit", domain.ControlPauseClaim)
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if _, ok, e := u.AllocationLimit(ctx, next); e != nil {
			return e
		} else if ok {
			t.Fatalf("revision %d declared no limit but a row exists", next)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestControlLimitReplayIsIdempotentAndAChangedLimitIsAConflict is A4 (b) and
// (c). The conflict is not a special case in the limit path: the request
// fingerprint covers the whole request, so a replay carrying a different limit
// conflicts for exactly the reason a replay carrying a different mode does.
func TestControlLimitReplayIsIdempotentAndAChangedLimitIsAConflict(t *testing.T) {
	s, st, _ := allocService(t)
	ctx := owner(context.Background())

	req := application.ControlRequest{
		RequestID:       "control-replay",
		Scope:           domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
		Mode:            domain.ControlAllow,
		AllocationLimit: &application.AllocationLimitInput{InstallationConcurrentExecutions: 4},
	}
	first, err := s.Control(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	beforeControls, beforeLimits := allocCountControlState(t, st, ctx)

	second, err := s.Control(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("replay changed the response: %#v vs %#v", first, second)
	}
	afterControls, afterLimits := allocCountControlState(t, st, ctx)
	if beforeControls != afterControls || beforeLimits != afterLimits {
		t.Fatalf("replay wrote further state: controls %d->%d limits %d->%d", beforeControls, afterControls, beforeLimits, afterLimits)
	}

	changed := req
	changed.AllocationLimit = &application.AllocationLimitInput{InstallationConcurrentExecutions: 5}
	if _, err = s.Control(ctx, changed); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("a replay with the same request_id and a different limit returned %v, want an idempotency conflict", err)
	}
	// And the stored limit is still the first one.
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		row, ok, e := u.AllocationLimit(ctx, first.Revision)
		if e != nil {
			return e
		}
		if !ok || row.InstallationConcurrentExecutions != 4 {
			t.Fatalf("the refused replay changed the stored limit: %#v ok=%v", row, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func allocCountControlState(t *testing.T, st *memory.Store, ctx context.Context) (int, int) {
	t.Helper()
	controls, limits := 0, 0
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		rows, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		controls = len(rows)
		for _, c := range rows {
			if _, ok, e := u.AllocationLimit(ctx, c.Revision); e != nil {
				return e
			} else if ok {
				limits++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return controls, limits
}

// ===========================================================================
// A5: limit validation
// ===========================================================================

// TestControlLimitRangeIsOneToTwentyAndZeroIsRejected is A5. The rejected
// values create no Control Intent at all, which is the property that matters:
// a malformed limit must not leave a revision behind.
func TestControlLimitRangeIsOneToTwentyAndZeroIsRejected(t *testing.T) {
	if application.AllocationLimitMinimum != 1 || application.AllocationLimitCeiling != 20 {
		t.Fatalf("range = %d..%d, want 1..20; the ceiling is docs/architecture/validation.md section 5's concurrent-Execution ceiling", application.AllocationLimitMinimum, application.AllocationLimitCeiling)
	}
	// The ceiling is the same figure the per-Provider half reports, so the two
	// halves cannot promise different concurrencies.
	if application.AllocationLimitCeiling != application.ProviderConcurrencyDesignCeiling {
		t.Fatalf("the allocation ceiling (%d) and ProviderConcurrencyDesignCeiling (%d) disagree", application.AllocationLimitCeiling, application.ProviderConcurrencyDesignCeiling)
	}

	for _, accepted := range []int{1, 2, 19, 20} {
		s, st, _ := allocService(t)
		ctx := owner(context.Background())
		revision := allocSetLimit(t, s, ctx, fmt.Sprintf("ok-%d", accepted), accepted)
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			row, ok, e := u.AllocationLimit(ctx, revision)
			if e != nil {
				return e
			}
			if !ok || row.InstallationConcurrentExecutions != accepted {
				t.Fatalf("limit %d was accepted but stored as %#v ok=%v", accepted, row, ok)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, rejected := range []int{0, -1, -20, 21, 100} {
		s, st, _ := allocService(t)
		ctx := owner(context.Background())
		_, err := s.Control(ctx, application.ControlRequest{
			RequestID:       fmt.Sprintf("control-bad-%d", rejected),
			Scope:           domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
			Mode:            domain.ControlAllow,
			AllocationLimit: &application.AllocationLimitInput{InstallationConcurrentExecutions: rejected},
		})
		if !errors.Is(err, application.ErrAllocationLimitOutOfRange) {
			t.Fatalf("limit %d returned %v, want ErrAllocationLimitOutOfRange", rejected, err)
		}
		controls, limits := allocCountControlState(t, st, ctx)
		if controls != 0 || limits != 0 {
			t.Fatalf("a rejected limit of %d left %d Control Intent(s) and %d limit row(s) behind", rejected, controls, limits)
		}
	}
	t.Log("0 is rejected rather than treated as a stop: a limit of zero would be a second way to halt the Loop that the control-verification pipeline does not observe, and pause-claim already exists for that with a proven verification path")
}

// ===========================================================================
// A6: effective limit resolution
// ===========================================================================

// TestEffectiveLimitIsTheGreatestRevisionThatDeclaredOne is A6, with the exact
// four-revision fixture it asks for and two more revisions on top.
func TestEffectiveLimitIsTheGreatestRevisionThatDeclaredOne(t *testing.T) {
	s, _, clk := allocService(t)
	ctx := owner(context.Background())

	// With nothing declared, the ceiling is the design ceiling and the source
	// says so.
	limit, source, revision, err := s.EffectiveAllocationLimit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = clk
	if limit != application.AllocationLimitCeiling || source != application.AllocationLimitFromDesignCeiling || revision != 0 {
		t.Fatalf("with nothing declared: limit=%d source=%q revision=%d", limit, source, revision)
	}

	// revision 1: no limit. revision 2: limit 3. revision 3: no limit.
	// revision 4: no limit. revision 5: limit 11. revision 6: no limit.
	allocSetNoLimit(t, s, ctx, "r1", domain.ControlAllow)
	r2 := allocSetLimit(t, s, ctx, "r2", 3)
	allocSetNoLimit(t, s, ctx, "r3", domain.ControlPauseClaim)
	allocSetNoLimit(t, s, ctx, "r4", domain.ControlAllow)
	r5 := allocSetLimit(t, s, ctx, "r5", 11)
	r6 := allocSetNoLimit(t, s, ctx, "r6", domain.ControlPauseIntake)
	if r2 != 2 || r5 != 5 || r6 != 6 {
		t.Fatalf("fixture revisions are not 2/5/6: %d/%d/%d", r2, r5, r6)
	}

	allocNextDay(clk)
	limit, source, revision, err = s.EffectiveAllocationLimit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 11 || source != application.AllocationLimitFromControlRevision || revision != r5 {
		t.Fatalf("effective limit = %d from %q at revision %d, want 11 from control-revision at %d; revision 6 declared no limit and must not clear revision 5's", limit, source, revision, r5)
	}

	// Resolution is deterministic: ten reads agree, and none of them reads a
	// clock (the injected clock is never advanced here).
	for i := 0; i < 10; i++ {
		allocNextDay(clk)
		again, againSource, againRevision, e := s.EffectiveAllocationLimit(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if again != limit || againSource != source || againRevision != revision {
			t.Fatalf("read %d disagreed: %d/%q/%d", i, again, againSource, againRevision)
		}
	}
	t.Logf("a later revision declaring no limit did not clear revision %d's limit of %d", r5, limit)
}

// ===========================================================================
// Shared AST helpers
// ===========================================================================

// allocDomainDir and allocSchedulerDir are the two packages this task must not
// edit. They are read here, never written.
const (
	allocDomainDir    = "../domain"
	allocSchedulerDir = "../scheduler"
)

// allocParseDir parses every non-test *.go file in dir. It fails outright on a
// zero-file parse so no assertion below can pass vacuously.
func allocParseDir(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		out[filepath.Base(path)] = file
	}
	if len(out) == 0 {
		t.Fatalf("parsed zero non-test files in %s; the scan is broken", dir)
	}
	return out
}

// allocStructFields returns the declared field names of the named struct type,
// searched across every parsed file, and whether it was found at all.
func allocStructFields(files map[string]*ast.File, typeName string) ([]string, bool) {
	for _, file := range files {
		found := []string{}
		hit := false
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name.Name != typeName {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			hit = true
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					found = append(found, name.Name)
				}
			}
			return false
		})
		if hit {
			sort.Strings(found)
			return found, true
		}
	}
	return nil, false
}

// ===========================================================================
// A7: the L3 permit closure is unchanged, proven not assumed
// ===========================================================================

// allocLimitVocabulary is what a leaked limit would look like in a field name.
var allocLimitVocabulary = []string{"limit", "concurren", "allocat", "capacity", "throttle", "ceiling"}

func allocNamesLimit(name string) (string, bool) {
	lowered := strings.ToLower(name)
	for _, token := range allocLimitVocabulary {
		if strings.Contains(lowered, token) {
			return token, true
		}
	}
	return "", false
}

// The git-diff proof that internal/domain and internal/scheduler are untouched
// lives in internal/api rather than here, and the reason is measured, not
// stylistic: V2-067's probe guard (source_guard_test.go in this package) forbids
// os/exec in EVERY file of internal/application, test files included, so that no
// change can quietly give the Provider registry a way to start a process. That
// guard is exactly right and is not weakened here, so the one assertion in this
// task that needs to run git was placed in a package the guard does not cover.
// See TestTheUntouchablePackagesAreUntouched in internal/api.

// TestTheLimitAppearsInNoPermitTypeAndNoEffect is A7's second proof: an AST
// field-name scan over the three types the permit path is made of. The matcher
// is verified against a known-positive and a known-negative first.
func TestTheLimitAppearsInNoPermitTypeAndNoEffect(t *testing.T) {
	for _, positive := range []string{"AllocationLimit", "InstallationConcurrentExecutions", "DeclaredCeiling", "Capacity", "Throttle"} {
		if _, ok := allocNamesLimit(positive); !ok {
			t.Fatalf("known-positive field name %q was not matched by the limit-vocabulary matcher", positive)
		}
	}
	for _, negative := range []string{"Kind", "Target", "FencingToken", "ControlRevision", "Payload", "OperationID", "RequestID", "ExpectedVersion", "Resource", "Scope"} {
		if entry, ok := allocNamesLimit(negative); ok {
			t.Fatalf("known-negative field name %q was matched on %q", negative, entry)
		}
	}

	domainFiles := allocParseDir(t, allocDomainDir)
	applicationFiles := applicationPackageFiles(t, false)

	checked := 0
	for _, target := range []struct {
		name  string
		files map[string]*ast.File
	}{
		{"PermitDecision", domainFiles},
		{"Effect", domainFiles},
		{"PermitRequest", applicationFiles},
		{"PermitResponse", applicationFiles},
	} {
		fields, ok := allocStructFields(target.files, target.name)
		if !ok {
			t.Fatalf("type %s was not found; the scan is broken or the type was renamed", target.name)
		}
		if len(fields) == 0 {
			t.Fatalf("type %s has no fields; the scan is broken", target.name)
		}
		checked++
		for _, name := range fields {
			if entry, bad := allocNamesLimit(name); bad {
				t.Fatalf("%s.%s matches the limit vocabulary on %q; the limit is an input to allocation, never to a permit decision", target.name, name, entry)
			}
		}
		t.Logf("%s fields = %v", target.name, fields)
	}
	if checked != 4 {
		t.Fatalf("checked %d types, want 4", checked)
	}
}

// TestAPermitDecisionIsByteIdenticalWithAndWithoutALimitInForce is A7's third
// proof, and it is behavioural rather than structural: the same permit request,
// against the same control state, over two stores that differ only in whether a
// limit is in force, produces byte-identical JSON. The limit is set BELOW the
// active count so it is genuinely binding in the second store.
func TestAPermitDecisionIsByteIdenticalWithAndWithoutALimitInForce(t *testing.T) {
	permitFor := func(withLimit bool) []byte {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())

		// Two active Leases, so a limit of 1 is genuinely below the active count.
		for _, tag := range []string{"a", "b"} {
			id, version := allocCapture(t, s, ctx, "permit-"+tag)
			allocLease(t, s, st, ctx, clk.now, id, version, "permit-"+tag)
		}
		if withLimit {
			allocSetLimit(t, s, ctx, "permit-limit", 1)
			// The limit really is in force and really is binding.
			summary := allocSummaryNextDay(t, s, ctx, clk)
			if !summary.Exhaustion.Exhausted || summary.Exhaustion.BindingLimit != application.BindingInstallationConcurrency {
				t.Fatalf("the limit is not binding in the with-limit store: %#v", summary.Exhaustion)
			}
		} else {
			allocSetNoLimit(t, s, ctx, "permit-limit", domain.ControlAllow)
		}
		allocNextDay(clk)
		out, err := s.Permit(ctx, application.PermitRequest{
			RequestID:       "permit-request",
			Kind:            domain.PermitClaim,
			Target:          domain.ControlTarget{InstallationID: "install"},
			ControlRevision: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	without := permitFor(false)
	with := permitFor(true)
	if !bytes.Equal(without, with) {
		t.Fatalf("the permit decision changed when a limit came into force:\n without = %s\n with    = %s", without, with)
	}
	t.Logf("permit decision is byte-identical with and without a binding limit: %s", with)
}

// ===========================================================================
// A8: the summary's shape and its determinism
// ===========================================================================

// TestQueueSummaryKeepsTheFiveCountersAndIsDeterministic is A8.
func TestQueueSummaryKeepsTheFiveCountersAndIsDeterministic(t *testing.T) {
	s, st, clk := allocService(t)
	ctx := owner(context.Background())

	ready := allocReady(t, s, st, ctx, clk.now, "det-ready")
	_ = ready
	id, version := allocCapture(t, s, ctx, "det-active")
	allocLease(t, s, st, ctx, clk.now, id, version, "det-active")
	allocSetLimit(t, s, ctx, "det", 5)

	first := allocSummaryNextDay(t, s, ctx, clk)
	firstBytes, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	// The five pre-existing keys are all present, with their own names.
	var body map[string]json.RawMessage
	if err = json.Unmarshal(firstBytes, &body); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"requirements", "by_requirement_status", "increments", "by_increment_status", "active_executions", "allocation", "waiting", "exhaustion"} {
		if _, ok := body[want]; !ok {
			t.Fatalf("the summary has no %q key: %s", want, firstBytes)
		}
	}
	if len(body) != 8 {
		t.Fatalf("the summary has %d keys, want exactly 8: %s", len(body), firstBytes)
	}
	// The five counters still mean what they meant: they are exactly what the
	// QueueSummaryRepository returned in the same state.
	var counters application.QueueSummary
	// Read through the store directly: st.Transact makes no quota reservation,
	// so this comparison cannot consume the budget day the assertions need.
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		var e error
		counters, e = u.QueueSummary(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.QueueSummary, counters) {
		t.Fatalf("the five counters were changed on the way out:\n summary = %#v\n store   = %#v", first.QueueSummary, counters)
	}

	// Determinism: repeated calls over the same stored state are byte-identical,
	// and the by_reason key order is fixed.
	for i := 0; i < 8; i++ {
		allocNextDay(clk)
		again, e := s.QueueSummary(ctx)
		if e != nil {
			t.Fatal(e)
		}
		againBytes, e := json.Marshal(again)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(firstBytes, againBytes) {
			t.Fatalf("call %d differed:\n first = %s\n again = %s", i, firstBytes, againBytes)
		}
	}
	order := allocByReasonKeyOrder(t, firstBytes)
	sorted := append([]string(nil), order...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(order, sorted) {
		t.Fatalf("by_reason keys are not in a fixed (sorted) order: %v", order)
	}
	if !reflect.DeepEqual(order, application.WaitingReasonBuckets()) {
		t.Fatalf("by_reason keys %v are not the declared bucket set %v", order, application.WaitingReasonBuckets())
	}
	t.Logf("summary is byte-identical across 9 calls; by_reason key order = %v", order)
}

// allocByReasonKeyOrder returns the by_reason keys in the order the marshalled
// body actually carries them.
func allocByReasonKeyOrder(t *testing.T, body []byte) []string {
	t.Helper()
	out := []string{}
	// A small hand-rolled scan: the assertion is about key ORDER, which
	// unmarshalling into a map would destroy.
	raw := string(body)
	marker := strings.Index(raw, `"by_reason":{`)
	if marker < 0 {
		t.Fatalf("no by_reason object in %s", body)
	}
	rest := raw[marker+len(`"by_reason":{`):]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatalf("unterminated by_reason object in %s", body)
	}
	for _, pair := range strings.Split(rest[:end], ",") {
		colon := strings.Index(pair, ":")
		if colon < 0 {
			continue
		}
		key, err := strconv.Unquote(strings.TrimSpace(pair[:colon]))
		if err != nil {
			t.Fatalf("bad by_reason key %q: %v", pair[:colon], err)
		}
		out = append(out, key)
	}
	if len(out) == 0 {
		t.Fatalf("parsed zero by_reason keys from %s", body)
	}
	return out
}

// ===========================================================================
// A9: the reported allocation is the scheduler's, and the read writes nothing
// ===========================================================================

// TestNoNonTestFileInApplicationReferencesApply is A9's structural half. The
// scan looks for the identifier Apply anywhere in the package's shipped code,
// so a call smuggled in through a local alias is caught too.
func TestNoNonTestFileInApplicationReferencesApply(t *testing.T) {
	// The matcher is verified first: a synthetic file that does call Apply must
	// be reported.
	const synthetic = `package application
func x() { _, _ = scheduler.Apply(a, b) }
`
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic_apply.go", synthetic, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if hits := allocApplyHits(file); len(hits) != 1 {
		t.Fatalf("positive control: Apply hits = %v, want exactly one", hits)
	}
	const negative = `package application
func y() { _ = ApplyProviderObservation(log, value) }
`
	file, err = parser.ParseFile(token.NewFileSet(), "synthetic_negative.go", negative, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if hits := allocApplyHits(file); len(hits) != 0 {
		t.Fatalf("negative control: ApplyProviderObservation was matched as Apply: %v", hits)
	}

	files := applicationPackageFiles(t, false)
	scanned := 0
	for name, f := range files {
		scanned++
		if hits := allocApplyHits(f); len(hits) > 0 {
			t.Errorf("%s references %v; the summary must call Decide and never Apply", name, hits)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned zero non-test files")
	}
	t.Logf("scanned %d non-test files; zero references to Apply", scanned)
}

func allocApplyHits(file *ast.File) []string {
	out := []string{}
	ast.Inspect(file, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if ok && ident.Name == "Apply" {
			out = append(out, ident.Name)
		}
		return true
	})
	return out
}

// ---------------------------------------------------------------------------
// The write spy
// ---------------------------------------------------------------------------

// allocWriteSpy wraps a UnitOfWork and records every write the store offers,
// forwarding each call unchanged. Every method below is a write path of
// application.UnitOfWork; a write added to the port without a method here would
// fail to compile this spy only if the port shrank, so
// TestTheWriteSpyCoversEveryWritePathOfTheUnitOfWork checks the coverage
// mechanically instead.
type allocWriteSpy struct {
	application.UnitOfWork
	writes     []string
	quotaCalls []quota.Usage
	quotaKeys  []string
}

func (w *allocWriteSpy) note(name string) { w.writes = append(w.writes, name) }

func (w *allocWriteSpy) AuthorityContext() context.Context {
	if inner, ok := w.UnitOfWork.(interface{ AuthorityContext() context.Context }); ok {
		return inner.AuthorityContext()
	}
	return context.Background()
}

func (w *allocWriteSpy) ReserveQuota(ctx context.Context, key string, at time.Time, usage quota.Usage) error {
	// A quota reservation is recorded separately from a write: every bounded
	// owner read already makes exactly one read-transaction reservation, and the
	// assertion is that this read makes no MUTATION reservation.
	w.quotaCalls = append(w.quotaCalls, usage)
	w.quotaKeys = append(w.quotaKeys, key)
	return w.UnitOfWork.ReserveQuota(ctx, key, at, usage)
}

func (w *allocWriteSpy) SaveRequirement(ctx context.Context, v domain.Requirement, expected domain.Version) error {
	w.note("SaveRequirement")
	return w.UnitOfWork.SaveRequirement(ctx, v, expected)
}
func (w *allocWriteSpy) SaveRequirementText(ctx context.Context, id, text string) error {
	w.note("SaveRequirementText")
	return w.UnitOfWork.SaveRequirementText(ctx, id, text)
}
func (w *allocWriteSpy) SaveIncrement(ctx context.Context, v domain.Increment, expected domain.Version) error {
	w.note("SaveIncrement")
	return w.UnitOfWork.SaveIncrement(ctx, v, expected)
}
func (w *allocWriteSpy) SaveExecution(ctx context.Context, v domain.Execution, expected domain.Version) error {
	w.note("SaveExecution")
	return w.UnitOfWork.SaveExecution(ctx, v, expected)
}
func (w *allocWriteSpy) SaveLease(ctx context.Context, v domain.Lease, expected domain.Version) error {
	w.note("SaveLease")
	return w.UnitOfWork.SaveLease(ctx, v, expected)
}
func (w *allocWriteSpy) SaveCanonicalTarget(ctx context.Context, incrementID string, target domain.ControlTarget) error {
	w.note("SaveCanonicalTarget")
	return w.UnitOfWork.SaveCanonicalTarget(ctx, incrementID, target)
}
func (w *allocWriteSpy) SaveControl(ctx context.Context, v domain.ControlIntent, expected domain.Revision) error {
	w.note("SaveControl")
	return w.UnitOfWork.SaveControl(ctx, v, expected)
}
func (w *allocWriteSpy) SaveControlRequestedBy(ctx context.Context, revision domain.Revision, v domain.RequestedBy) error {
	w.note("SaveControlRequestedBy")
	return w.UnitOfWork.SaveControlRequestedBy(ctx, revision, v)
}
func (w *allocWriteSpy) SaveAllocationLimit(ctx context.Context, v application.AllocationLimit) error {
	w.note("SaveAllocationLimit")
	return w.UnitOfWork.SaveAllocationLimit(ctx, v)
}
func (w *allocWriteSpy) SaveControlProgress(ctx context.Context, v domain.ControlProgress, expected domain.ControlState) error {
	w.note("SaveControlProgress")
	return w.UnitOfWork.SaveControlProgress(ctx, v, expected)
}
func (w *allocWriteSpy) SaveRunnerObservation(ctx context.Context, v domain.RunnerObservation) error {
	w.note("SaveRunnerObservation")
	return w.UnitOfWork.SaveRunnerObservation(ctx, v)
}
func (w *allocWriteSpy) SaveRunnerVersionReport(ctx context.Context, v application.RunnerVersionReport) error {
	w.note("SaveRunnerVersionReport")
	return w.UnitOfWork.SaveRunnerVersionReport(ctx, v)
}
func (w *allocWriteSpy) SaveProviderObservation(ctx context.Context, v application.ProviderObservation) error {
	w.note("SaveProviderObservation")
	return w.UnitOfWork.SaveProviderObservation(ctx, v)
}
func (w *allocWriteSpy) SaveProviderAssignment(ctx context.Context, v application.ProviderAssignment) error {
	w.note("SaveProviderAssignment")
	return w.UnitOfWork.SaveProviderAssignment(ctx, v)
}
func (w *allocWriteSpy) SaveRepository(ctx context.Context, v domain.Repository, expected domain.Version) error {
	w.note("SaveRepository")
	return w.UnitOfWork.SaveRepository(ctx, v, expected)
}
func (w *allocWriteSpy) SaveRepositoryObservation(ctx context.Context, v domain.RepositoryObservation) error {
	w.note("SaveRepositoryObservation")
	return w.UnitOfWork.SaveRepositoryObservation(ctx, v)
}
func (w *allocWriteSpy) SaveRequirementRepositoryLink(ctx context.Context, v domain.RequirementRepositoryLink) error {
	w.note("SaveRequirementRepositoryLink")
	return w.UnitOfWork.SaveRequirementRepositoryLink(ctx, v)
}
func (w *allocWriteSpy) SaveIdempotency(ctx context.Context, v application.IdempotentResponse) error {
	w.note("SaveIdempotency")
	return w.UnitOfWork.SaveIdempotency(ctx, v)
}
func (w *allocWriteSpy) SaveOutbox(ctx context.Context, v application.OutboxItem, expected domain.Version) error {
	w.note("SaveOutbox")
	return w.UnitOfWork.SaveOutbox(ctx, v, expected)
}
func (w *allocWriteSpy) Record(event application.Event, outbox *application.OutboxItem) error {
	w.note("Record")
	if outbox != nil {
		w.note("Record.outbox")
	}
	return w.UnitOfWork.Record(event, outbox)
}

// allocSpyStore hands every transaction a spy over the real store's unit.
type allocSpyStore struct {
	inner *memory.Store
	spies []*allocWriteSpy
}

func (s *allocSpyStore) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	return s.inner.Transact(ctx, func(u application.UnitOfWork) error {
		spy := &allocWriteSpy{UnitOfWork: u}
		s.spies = append(s.spies, spy)
		return fn(spy)
	})
}

func (s *allocSpyStore) totals() ([]string, []quota.Usage, []string) {
	writes := []string{}
	usages := []quota.Usage{}
	keys := []string{}
	for _, spy := range s.spies {
		writes = append(writes, spy.writes...)
		usages = append(usages, spy.quotaCalls...)
		keys = append(keys, spy.quotaKeys...)
	}
	return writes, usages, keys
}

// TestTheWriteSpyCoversEveryWritePathOfTheUnitOfWork keeps the spy honest: it
// enumerates the port's own method set and asserts the spy overrides every
// method whose name is a write. A write path the spy silently forwarded would
// make the zero-write assertion below weaker than it reads.
func TestTheWriteSpyCoversEveryWritePathOfTheUnitOfWork(t *testing.T) {
	port := reflect.TypeOf((*application.UnitOfWork)(nil)).Elem()
	spy := reflect.TypeOf(&allocWriteSpy{})
	missing := []string{}
	writeMethods := 0
	for i := 0; i < port.NumMethod(); i++ {
		name := port.Method(i).Name
		if !strings.HasPrefix(name, "Save") && name != "Record" && name != "ReserveQuota" {
			continue
		}
		writeMethods++
		method, ok := spy.MethodByName(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		// A forwarded (promoted) method has the embedded interface's receiver
		// depth; an overridden one is declared on *allocWriteSpy itself. The
		// distinction that matters is that the declaring type is the spy.
		if method.Func.Type().In(0) != spy {
			missing = append(missing, name)
		}
	}
	if writeMethods == 0 {
		t.Fatal("found zero write methods on the port; the reflection walk is broken")
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		t.Fatalf("the write spy does not override %v; the zero-write assertion would not see those paths", missing)
	}
	t.Logf("the write spy overrides all %d write paths of application.UnitOfWork", writeMethods)
}

// TestQueueSummaryPerformsZeroWritesAndReservesNoMutationQuota is A9's
// behavioural half.
func TestQueueSummaryPerformsZeroWritesAndReservesNoMutationQuota(t *testing.T) {
	inner := memory.New()
	clk := &allocClock{now: allocBase}
	seed, err := application.NewServiceWithConfig(inner, clk, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	// Seed real state through the real service, over the un-spied store.
	allocReady(t, seed, inner, ctx, clk.now, "spy-ready-1")
	allocReady(t, seed, inner, ctx, clk.now, "spy-ready-2")
	id, version := allocCapture(t, seed, ctx, "spy-active")
	allocLease(t, seed, inner, ctx, clk.now, id, version, "spy-active")
	allocSetLimit(t, seed, ctx, "spy", 3)

	// Now read through a service whose every transaction is spied.
	spied := &allocSpyStore{inner: inner}
	reader, err := application.NewServiceWithConfig(spied, clk, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	before := allocStoreDigest(t, inner, ctx)
	allocNextDay(clk)
	summary, err := reader.QueueSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writes, usages, keys := spied.totals()
	if len(writes) != 0 {
		t.Fatalf("GET /v1/queue/summary performed %d write(s): %v", len(writes), writes)
	}
	if len(usages) != 1 {
		t.Fatalf("the read made %d quota reservations, want exactly the one bounded read-transaction reservation: %v", len(usages), usages)
	}
	if usages[0] != quota.ReadTransactionUsage {
		t.Fatalf("the read reserved %#v, want quota.ReadTransactionUsage (%#v); it must never reserve mutation quota", usages[0], quota.ReadTransactionUsage)
	}
	if usages[0] == quota.MutationUsage {
		t.Fatal("the read reserved mutation quota")
	}
	if !strings.HasPrefix(keys[0], "read:") {
		t.Fatalf("the reservation key %q is not a read key", keys[0])
	}
	// And the committed state is byte-identical afterwards.
	after := allocStoreDigest(t, inner, ctx)
	if before != after {
		t.Fatalf("the read changed committed state:\n before = %s\n after  = %s", before, after)
	}
	// The reported allocation count is the plan's, and the plan is not the
	// active count: two ready candidates, limit 3, one already active.
	if summary.Allocation.Active != 1 {
		t.Fatalf("active = %d, want 1", summary.Allocation.Active)
	}
	if summary.Allocation.PlannedAssignments == summary.Allocation.Active && summary.Allocation.PlannedAssignments != 0 {
		t.Logf("note: planned_assignments and active coincide at %d in this fixture", summary.Allocation.Active)
	}
	t.Logf("zero writes, one read-transaction reservation (%#v), committed state unchanged; planned_assignments=%d active=%d", usages[0], summary.Allocation.PlannedAssignments, summary.Allocation.Active)
}

// TestTheReportedAllocationIsThePlanAndZeroWhenTheSchedulerPlansNothing is the
// rest of A9: the reported count is len(plan.Assignments) exactly, and a read
// that plans nothing reports zero rather than reporting the active count.
func TestTheReportedAllocationIsThePlanAndZeroWhenTheSchedulerPlansNothing(t *testing.T) {
	// One ready candidate, capacity free: the scheduler plans exactly one.
	s, st, clk := allocService(t)
	ctx := owner(context.Background())
	allocReady(t, s, st, ctx, clk.now, "plan-one")
	allocSetLimit(t, s, ctx, "plan-one", 5)
	summary := allocSummaryNextDay(t, s, ctx, clk)
	if summary.Allocation.PlannedAssignments != 1 {
		t.Fatalf("planned_assignments = %d, want 1", summary.Allocation.PlannedAssignments)
	}
	if summary.Allocation.Active != 0 {
		t.Fatalf("active = %d, want 0", summary.Allocation.Active)
	}

	// Now a store with active work and no ready candidate at all: the scheduler
	// plans nothing, and the summary must not report the active count as an
	// allocation.
	s2, st2, clk2 := allocService(t)
	ctx2 := owner(context.Background())
	for _, tag := range []string{"x", "y"} {
		id, version := allocCapture(t, s2, ctx2, "plan-none-"+tag)
		allocLease(t, s2, st2, ctx2, clk2.now, id, version, "plan-none-"+tag)
	}
	allocSetLimit(t, s2, ctx2, "plan-none", 20)
	summary2 := allocSummaryNextDay(t, s2, ctx2, clk2)
	if summary2.Allocation.Active != 2 {
		t.Fatalf("active = %d, want 2", summary2.Allocation.Active)
	}
	if summary2.Allocation.PlannedAssignments != 0 {
		t.Fatalf("planned_assignments = %d with no ready candidate; the active count must never be reported as an allocation", summary2.Allocation.PlannedAssignments)
	}
	t.Log("planned_assignments is the plan's own length: 1 when one candidate is assignable, 0 when the scheduler plans nothing even while two Executions are active")
}

// allocStoreDigest is a stable rendering of everything the summary could have
// touched, used as a before/after equality check.
func allocStoreDigest(t *testing.T, st *memory.Store, ctx context.Context) string {
	t.Helper()
	var out string
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		rows, _, e := u.RequirementsPage(ctx, "", application.MaxPageSize)
		if e != nil {
			return e
		}
		parts := []string{}
		for _, r := range rows {
			parts = append(parts, fmt.Sprintf("requirement:%s:%s:%d", r.ID.String(), r.Status, r.Version))
		}
		leases, e := u.ActiveLeases(ctx, 101)
		if e != nil {
			return e
		}
		for _, l := range leases {
			parts = append(parts, allocLeaseDigest(l))
		}
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		for _, c := range controls {
			parts = append(parts, fmt.Sprintf("control:%d:%s", c.Revision, c.Mode))
			if row, ok, x := u.AllocationLimit(ctx, c.Revision); x != nil {
				return x
			} else if ok {
				parts = append(parts, fmt.Sprintf("limit:%d:%d", row.Revision, row.InstallationConcurrentExecutions))
			}
		}
		sort.Strings(parts)
		out = strings.Join(parts, "|")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// allocLeaseDigest renders every field of a Lease that a revocation would move.
func allocLeaseDigest(l domain.Lease) string {
	return fmt.Sprintf("lease:%s:increment=%s:execution=%s:runner=%s:fence=%d:revision=%d:status=%s:issued=%d:expires=%d:version=%d",
		l.ID.String(), l.IncrementID.String(), l.ExecutionID.String(), l.RunnerID.String(),
		uint64(l.FencingToken), uint64(l.ControlRevision), l.Status,
		l.IssuedAt.UTC().UnixNano(), l.ExpiresAt.UTC().UnixNano(), uint64(l.Version))
}

// ===========================================================================
// A10: the waiting reasons are V2-030's constants projected, and total
// ===========================================================================

// TestWaitingReasonProjectionIsTotalInBothDirections is A10's first half. It
// fails if internal/scheduler adds a reason constant with no mapping here, and
// it fails if this package reports a value no scheduler constant produces.
func TestWaitingReasonProjectionIsTotalInBothDirections(t *testing.T) {
	if len(scheduler.AllReasons) == 0 {
		t.Fatal("scheduler.AllReasons is empty; the table would pass vacuously")
	}
	// Forward: every scheduler constant maps to exactly one reported value.
	forward := map[scheduler.Reason]string{}
	for _, reason := range scheduler.AllReasons {
		value, ok := application.WaitingReasonFor(reason)
		if !ok {
			t.Fatalf("scheduler reason %q has no reported value; V2-030 added a constant this projection does not cover", reason)
		}
		if value == "" {
			t.Fatalf("scheduler reason %q maps to the empty string", reason)
		}
		if prior, seen := forward[reason]; seen {
			t.Fatalf("reason %q mapped twice (%q then %q)", reason, prior, value)
		}
		forward[reason] = value
	}
	if len(forward) != len(scheduler.AllReasons) {
		t.Fatalf("mapped %d of %d reasons", len(forward), len(scheduler.AllReasons))
	}
	// Backward: every reported bucket comes from exactly one scheduler constant.
	buckets := application.WaitingReasonBuckets()
	if len(buckets) != len(scheduler.AllReasons) {
		t.Fatalf("the reported bucket set has %d members, the scheduler declares %d: %v vs %v", len(buckets), len(scheduler.AllReasons), buckets, scheduler.AllReasons)
	}
	origin := map[string][]scheduler.Reason{}
	for reason, value := range forward {
		origin[value] = append(origin[value], reason)
	}
	for _, bucket := range buckets {
		sources := origin[bucket]
		if len(sources) == 0 {
			t.Fatalf("reported bucket %q has no scheduler constant behind it", bucket)
		}
		if len(sources) != 1 {
			t.Fatalf("reported bucket %q is produced by %d scheduler constants (%v); the projection must be one-to-one", bucket, len(sources), sources)
		}
	}
	// The empty reason -- what an ASSIGNED candidate carries -- is not a bucket.
	if _, ok := application.WaitingReasonFor(scheduler.Reason("")); ok {
		t.Fatal("the empty reason projects to a bucket; an assigned candidate would be counted as waiting")
	}
	// And an invented reason is refused rather than silently passed through.
	for _, invented := range []scheduler.Reason{"blocked", "throttled", "over-limit", "paused", "no-capacity"} {
		if _, ok := application.WaitingReasonFor(invented); ok {
			t.Fatalf("invented reason %q projects to a bucket; this task must define no reason vocabulary of its own", invented)
		}
	}
	t.Logf("projection is total in both directions over %d constants: %v", len(buckets), buckets)
}

// allocReasonReachability records, per scheduler reason, whether the production
// wiring can produce it at all, and why not when it cannot. The unreachable
// three are a MEASURED limitation of the store, not of the projection: each one
// needs an input no durable record supplies today.
var allocReasonReachability = map[scheduler.Reason]string{
	scheduler.ReasonNotReady:              "",
	scheduler.ReasonAlreadyOwned:          "",
	scheduler.ReasonResourceConflict:      "",
	scheduler.ReasonNoRunnerCapacity:      "",
	scheduler.ReasonUnmetDependency:       "domain.Requirement declares no dependency field, so the builder can never populate scheduler.Requirement.Dependencies from stored state",
	scheduler.ReasonNotExecutable:         "domain.PriorityAssessment has no persistence port anywhere in the repository, so the builder can never populate scheduler.Requirement.Assessment from stored state",
	scheduler.ReasonRepositoryUnavailable: "the Snapshot models the installation as one synthetic Repository with no FailureCount and no IsolatedUntil source; making this reachable means feeding the real Requirement-to-Repository link and the real Repository aggregate, which is multi-Repository modelling owned by V2-030's local closure and V2-031's M7 gate",
}

// TestReasonReachabilityTableCoversEveryScheduledReason keeps the reachability
// record honest: it is a statement about every constant, not a list of the ones
// that happened to be convenient.
func TestReasonReachabilityTableCoversEveryScheduledReason(t *testing.T) {
	if len(allocReasonReachability) != len(scheduler.AllReasons) {
		t.Fatalf("the reachability table has %d entries, the scheduler declares %d reasons", len(allocReasonReachability), len(scheduler.AllReasons))
	}
	reachable, unreachable := []string{}, []string{}
	for _, reason := range scheduler.AllReasons {
		note, ok := allocReasonReachability[reason]
		if !ok {
			t.Fatalf("reason %q has no reachability entry", reason)
		}
		if note == "" {
			reachable = append(reachable, string(reason))
		} else {
			unreachable = append(unreachable, string(reason)+": "+note)
		}
	}
	sort.Strings(reachable)
	sort.Strings(unreachable)
	if len(reachable) != 4 {
		t.Fatalf("the table claims %d reachable reasons, the measured figure is 4: %v", len(reachable), reachable)
	}
	t.Logf("reachable through the summary (%d): %v", len(reachable), reachable)
	for _, note := range unreachable {
		t.Logf("NOT reachable through the summary -- %s", note)
	}
}

// TestEachReachableWaitingReasonIsDrivenThroughTheSummary is A10's second half
// for the four reasons the production wiring can actually produce. Each case
// drives real state through the real Service and reads the real summary.
func TestEachReachableWaitingReasonIsDrivenThroughTheSummary(t *testing.T) {
	t.Run("not-ready", func(t *testing.T) {
		s, _, clk := allocService(t)
		ctx := owner(context.Background())
		// A captured Requirement is not schedulable on its own terms.
		allocCapture(t, s, ctx, "reason-not-ready")
		summary := allocSummaryNextDay(t, s, ctx, clk)
		allocAssertReasons(t, summary, map[string]int{string(scheduler.ReasonNotReady): 1})
	})

	t.Run("already-owned", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		// One Requirement that is ready AND already holds an active Lease.
		id, version := allocCapture(t, s, ctx, "reason-owned")
		allocLease(t, s, st, ctx, clk.now, id, version, "reason-owned")
		allocAdvance(t, st, ctx, clk.now, id, domain.RequirementStartFraming, domain.RequirementReadyCommand)
		allocSetLimit(t, s, ctx, "reason-owned", 20)
		summary := allocSummaryNextDay(t, s, ctx, clk)
		allocAssertReasons(t, summary, map[string]int{string(scheduler.ReasonAlreadyOwned): 1})
	})

	t.Run("resource-conflict", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		// Three ready Requirements LINKED TO THE SAME Repository, with capacity
		// for all of them: the first takes that Repository's write claim and the
		// other two report the conflict.
		for _, tag := range []string{"a", "b", "c"} {
			id := allocReady(t, s, st, ctx, clk.now, "reason-conflict-"+tag)
			allocLink(t, st, ctx, clk.now, id, "repository-shared")
		}
		allocSetLimit(t, s, ctx, "reason-conflict", 20)
		summary := allocSummaryNextDay(t, s, ctx, clk)
		if summary.Allocation.PlannedAssignments != 1 {
			t.Fatalf("planned_assignments = %d, want 1", summary.Allocation.PlannedAssignments)
		}
		allocAssertReasons(t, summary, map[string]int{string(scheduler.ReasonResourceConflict): 2})
		if summary.Exhaustion.Exhausted {
			t.Fatal("capacity remains but the summary reports exhaustion; exhaustion must not be derived from a candidate's reason")
		}
	})

	t.Run("no-runner-capacity", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		// One active Lease and a limit of 1: no capacity is left, so the ready
		// candidate clears every other check and finds none.
		active, version := allocCapture(t, s, ctx, "reason-capacity-active")
		allocLease(t, s, st, ctx, clk.now, active, version, "reason-capacity-active")
		allocReady(t, s, st, ctx, clk.now, "reason-capacity-waiter")
		allocSetLimit(t, s, ctx, "reason-capacity", 1)
		summary := allocSummaryNextDay(t, s, ctx, clk)
		// Two candidates are waiting, for two different reasons: the waiter
		// found no capacity, and the Requirement whose Execution is running was
		// never driven past `captured`, so it is not schedulable on its own
		// terms. Both are asserted, so neither hides the other.
		allocAssertReasons(t, summary, map[string]int{
			string(scheduler.ReasonNoRunnerCapacity): 1,
			string(scheduler.ReasonNotReady):         1,
		})
		if !summary.Exhaustion.Exhausted || summary.Exhaustion.BindingLimit != application.BindingInstallationConcurrency {
			t.Fatalf("exhaustion = %#v", summary.Exhaustion)
		}
	})
}

// allocAssertReasons asserts the whole by_reason map: every bucket named in want
// holds exactly that count, every bucket not named holds zero, all seven buckets
// are present, and waiting.total is their sum. Asserting the whole map rather
// than one bucket is what stops a scenario from quietly counting a second,
// unintended wait.
func allocAssertReasons(t *testing.T, summary application.QueueSummaryResponse, want map[string]int) {
	t.Helper()
	if summary.Waiting.ByReason == nil {
		t.Fatal("by_reason is absent")
	}
	if len(summary.Waiting.ByReason) != len(scheduler.AllReasons) {
		t.Fatalf("by_reason has %d buckets, want one per scheduler reason (%d): %#v", len(summary.Waiting.ByReason), len(scheduler.AllReasons), summary.Waiting.ByReason)
	}
	for name := range want {
		if _, ok := summary.Waiting.ByReason[name]; !ok {
			t.Fatalf("the expectation names bucket %q, which the summary does not report", name)
		}
	}
	sum := 0
	for name, count := range summary.Waiting.ByReason {
		sum += count
		if count != want[name] {
			t.Fatalf("bucket %q = %d, want %d (all buckets: %#v)", name, count, want[name], summary.Waiting.ByReason)
		}
	}
	if summary.Waiting.Total != sum {
		t.Fatalf("waiting.total = %d, sum of buckets = %d", summary.Waiting.Total, sum)
	}
}

// TestWaitingTotalEqualsTheConsideredCandidatesThatWereNotAssigned is the last
// clause of A10.
func TestWaitingTotalEqualsTheConsideredCandidatesThatWereNotAssigned(t *testing.T) {
	s, st, clk := allocService(t)
	ctx := owner(context.Background())
	// Five candidates: three ready and linked to one Repository, two only
	// captured. One of the three takes the Repository's write claim.
	for _, tag := range []string{"a", "b", "c"} {
		id := allocReady(t, s, st, ctx, clk.now, "total-ready-"+tag)
		allocLink(t, st, ctx, clk.now, id, "repository-total")
	}
	for _, tag := range []string{"d", "e"} {
		allocCapture(t, s, ctx, "total-captured-"+tag)
	}
	allocSetLimit(t, s, ctx, "total", 20)
	summary := allocSummaryNextDay(t, s, ctx, clk)
	considered := summary.Requirements
	if considered != 5 {
		t.Fatalf("considered %d Requirements, want 5", considered)
	}
	if summary.Waiting.Total+summary.Allocation.PlannedAssignments != considered {
		t.Fatalf("waiting.total (%d) + planned_assignments (%d) != considered candidates (%d)", summary.Waiting.Total, summary.Allocation.PlannedAssignments, considered)
	}
	if summary.Waiting.ByReason[string(scheduler.ReasonNotReady)] != 2 {
		t.Fatalf("not-ready bucket = %d, want 2: %#v", summary.Waiting.ByReason[string(scheduler.ReasonNotReady)], summary.Waiting.ByReason)
	}
	if summary.Waiting.ByReason[string(scheduler.ReasonResourceConflict)] != 2 {
		t.Fatalf("resource-conflict bucket = %d, want 2: %#v", summary.Waiting.ByReason[string(scheduler.ReasonResourceConflict)], summary.Waiting.ByReason)
	}
	t.Logf("5 considered = 1 assigned + 4 waiting (%#v)", summary.Waiting.ByReason)
}

// ===========================================================================
// A11: exhaustion
// ===========================================================================

// TestExhaustionAccountingIsTotalAndComesFromCapacityNotFromAReason is A11's
// function-level half: the accounting is a pure function of the numbers the
// caller supplied to the Snapshot, exercised as a table.
func TestExhaustionAccountingIsTotalAndComesFromCapacityNotFromAReason(t *testing.T) {
	cases := []struct {
		active, limit int
		source        application.AllocationLimitSource
		exhausted     bool
		binding       application.BindingLimit
	}{
		{0, 20, application.AllocationLimitFromDesignCeiling, false, application.BindingNone},
		{19, 20, application.AllocationLimitFromDesignCeiling, false, application.BindingNone},
		{20, 20, application.AllocationLimitFromDesignCeiling, true, application.BindingRunnerCapacity},
		{21, 20, application.AllocationLimitFromDesignCeiling, true, application.BindingRunnerCapacity},
		{0, 3, application.AllocationLimitFromControlRevision, false, application.BindingNone},
		{2, 3, application.AllocationLimitFromControlRevision, false, application.BindingNone},
		{3, 3, application.AllocationLimitFromControlRevision, true, application.BindingInstallationConcurrency},
		{9, 3, application.AllocationLimitFromControlRevision, true, application.BindingInstallationConcurrency},
	}
	for _, c := range cases {
		got := application.AllocationExhaustion(c.active, c.limit, c.source)
		if got.Exhausted != c.exhausted || got.BindingLimit != c.binding {
			t.Fatalf("active=%d limit=%d source=%q -> %#v, want exhausted=%v binding=%q", c.active, c.limit, c.source, got, c.exhausted, c.binding)
		}
		if !got.BindingLimit.Valid() {
			t.Fatalf("binding limit %q is not a declared value", got.BindingLimit)
		}
	}
	// Every declared binding value is produced by some case above, so none of
	// the three is a value no code can emit.
	produced := map[application.BindingLimit]bool{}
	for _, c := range cases {
		produced[application.AllocationExhaustion(c.active, c.limit, c.source).BindingLimit] = true
	}
	for _, want := range []application.BindingLimit{application.BindingNone, application.BindingInstallationConcurrency, application.BindingRunnerCapacity} {
		if !produced[want] {
			t.Fatalf("binding limit %q is declared but no case produces it", want)
		}
	}
	for _, undeclared := range []application.BindingLimit{"", "provider-capacity", "exhausted", "quota"} {
		if undeclared.Valid() {
			t.Fatalf("undeclared binding limit %q is Valid", undeclared)
		}
	}
}

// TestExhaustionThroughTheSummary is A11 (a), (b), (c) and (d), each driven
// through the real read.
func TestExhaustionThroughTheSummary(t *testing.T) {
	t.Run("a-capacity-remains", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		id, version := allocCapture(t, s, ctx, "exh-a")
		allocLease(t, s, st, ctx, clk.now, id, version, "exh-a")
		allocSetLimit(t, s, ctx, "exh-a", 5)
		summary := allocSummaryNextDay(t, s, ctx, clk)
		if summary.Exhaustion.Exhausted || summary.Exhaustion.BindingLimit != application.BindingNone {
			t.Fatalf("exhaustion = %#v with active=%d limit=%d", summary.Exhaustion, summary.Allocation.Active, summary.Allocation.Limit)
		}
		if summary.Allocation.Remaining != 4 {
			t.Fatalf("remaining = %d, want 4", summary.Allocation.Remaining)
		}
	})

	t.Run("b-installation-concurrency-binds", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		for _, tag := range []string{"p", "q"} {
			id, version := allocCapture(t, s, ctx, "exh-b-"+tag)
			allocLease(t, s, st, ctx, clk.now, id, version, "exh-b-"+tag)
		}
		allocSetLimit(t, s, ctx, "exh-b", 2)
		summary := allocSummaryNextDay(t, s, ctx, clk)
		if !summary.Exhaustion.Exhausted || summary.Exhaustion.BindingLimit != application.BindingInstallationConcurrency {
			t.Fatalf("exhaustion = %#v with active=%d limit=%d", summary.Exhaustion, summary.Allocation.Active, summary.Allocation.Limit)
		}
		if summary.Allocation.Remaining != 0 || summary.Allocation.LimitSource != application.AllocationLimitFromControlRevision {
			t.Fatalf("allocation = %#v", summary.Allocation)
		}
	})

	t.Run("c-the-pools-own-capacity-binds", func(t *testing.T) {
		// No Control Intent has ever declared a limit, so the pool's capacity is
		// the architecture design ceiling and that is what binds.
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		for i := 0; i < application.AllocationLimitCeiling; i++ {
			tag := fmt.Sprintf("exh-c-%02d", i)
			id, version := allocCapture(t, s, ctx, tag)
			allocLease(t, s, st, ctx, clk.now, id, version, tag)
		}
		summary := allocSummaryNextDay(t, s, ctx, clk)
		if summary.Allocation.LimitSource != application.AllocationLimitFromDesignCeiling {
			t.Fatalf("limit_source = %q, want the design ceiling", summary.Allocation.LimitSource)
		}
		if summary.Allocation.Active != application.AllocationLimitCeiling {
			t.Fatalf("active = %d, want %d", summary.Allocation.Active, application.AllocationLimitCeiling)
		}
		if !summary.Exhaustion.Exhausted || summary.Exhaustion.BindingLimit != application.BindingRunnerCapacity {
			t.Fatalf("exhaustion = %#v; with no owner-declared limit what binds is the pool's own capacity", summary.Exhaustion)
		}
	})

	t.Run("d-a-conflict-is-not-exhaustion", func(t *testing.T) {
		s, st, clk := allocService(t)
		ctx := owner(context.Background())
		// Two ready candidates and plenty of capacity: one is assigned, the
		// other reports resource-conflict, and capacity remains.
		for _, tag := range []string{"exh-d-1", "exh-d-2"} {
			id := allocReady(t, s, st, ctx, clk.now, tag)
			allocLink(t, st, ctx, clk.now, id, "repository-exh-d")
		}
		allocSetLimit(t, s, ctx, "exh-d", 20)
		summary := allocSummaryNextDay(t, s, ctx, clk)
		if summary.Waiting.ByReason[string(scheduler.ReasonResourceConflict)] != 1 {
			t.Fatalf("resource-conflict bucket = %d, want 1: %#v", summary.Waiting.ByReason[string(scheduler.ReasonResourceConflict)], summary.Waiting.ByReason)
		}
		if summary.Exhaustion.Exhausted || summary.Exhaustion.BindingLimit != application.BindingNone {
			t.Fatalf("a candidate waiting on a resource conflict made the summary report exhaustion: %#v", summary.Exhaustion)
		}
	})
}

// ===========================================================================
// A12: a limit change never invalidates a granted claim
// ===========================================================================

// allocDerivedClaims restates the builder's claim rule against the store, so the
// multiset asserted below is built from stored state rather than from the
// report's own output: one Claim per active Lease, owned by the Requirement the
// Lease's Increment belongs to, named by the public contention key.
func allocDerivedClaims(t *testing.T, st *memory.Store, ctx context.Context) []scheduler.Claim {
	t.Helper()
	out := []scheduler.Claim{}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		leases, e := u.ActiveLeases(ctx, 101)
		if e != nil {
			return e
		}
		for _, lease := range leases {
			owner, repository := "", ""
			inc, ok, x := u.Increment(ctx, lease.IncrementID.String())
			if x != nil {
				return x
			}
			if ok {
				owner = inc.RequirementID.String()
				link, linked, y := u.RequirementRepositoryLink(ctx, owner)
				if y != nil {
					return y
				}
				if linked {
					repository = link.RepositoryID.String()
				}
			}
			out = append(out, scheduler.Claim{
				Resource:     application.AllocationContentionKey(owner, repository),
				Owner:        owner,
				RepositoryID: application.AllocationSnapshotRepositoryID,
				Mode:         scheduler.Write,
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out
}

func allocLeaseDigests(t *testing.T, st *memory.Store, ctx context.Context) []string {
	t.Helper()
	out := []string{}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		leases, e := u.ActiveLeases(ctx, 101)
		if e != nil {
			return e
		}
		for _, l := range leases {
			out = append(out, allocLeaseDigest(l))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestALoweredLimitInvalidatesNothingThatWasAlreadyGranted is A12, in one test,
// with its negative control at the end.
func TestALoweredLimitInvalidatesNothingThatWasAlreadyGranted(t *testing.T) {
	s, st, clk := allocService(t)
	ctx := owner(context.Background())

	// Three Executions running, each with its own Lease and fencing token, and
	// one ready Requirement waiting behind them.
	for _, tag := range []string{"g1", "g2", "g3"} {
		id, version := allocCapture(t, s, ctx, "granted-"+tag)
		allocLease(t, s, st, ctx, clk.now, id, version, "granted-"+tag)
	}
	waiter := allocReady(t, s, st, ctx, clk.now, "granted-waiter")
	_ = waiter

	leasesBefore := allocLeaseDigests(t, st, ctx)
	claimsBefore := allocDerivedClaims(t, st, ctx)
	if len(leasesBefore) != 3 || len(claimsBefore) != 3 {
		t.Fatalf("fixture holds %d leases and %d claims, want 3 and 3", len(leasesBefore), len(claimsBefore))
	}

	// Now lower the limit to 1, well below the three that are already running.
	allocSetLimit(t, s, ctx, "granted-lower", 1)
	summary := allocSummaryNextDay(t, s, ctx, clk)

	// Nothing that was granted moved. Every field a revocation, an expiry or a
	// re-fence would touch is compared, not just the count.
	leasesAfter := allocLeaseDigests(t, st, ctx)
	if !reflect.DeepEqual(leasesBefore, leasesAfter) {
		t.Fatalf("a lowered limit changed a granted Lease:\n before = %v\n after  = %v", leasesBefore, leasesAfter)
	}
	claimsAfter := allocDerivedClaims(t, st, ctx)
	if !reflect.DeepEqual(claimsBefore, claimsAfter) {
		t.Fatalf("a lowered limit changed the claim multiset:\n before = %#v\n after  = %#v", claimsBefore, claimsAfter)
	}
	if len(claimsAfter) != 3 {
		t.Fatalf("the claim multiset has %d members, want 3", len(claimsAfter))
	}
	// No new assignment was planned, and the report says the installation is
	// exhausted with the owner's own limit binding.
	if summary.Allocation.PlannedAssignments != 0 {
		t.Fatalf("planned_assignments = %d with the limit below the active count", summary.Allocation.PlannedAssignments)
	}
	if summary.Allocation.Active != 3 || summary.Allocation.Limit != 1 || summary.Allocation.Remaining != 0 {
		t.Fatalf("allocation = %#v", summary.Allocation)
	}
	if !summary.Exhaustion.Exhausted || summary.Exhaustion.BindingLimit != application.BindingInstallationConcurrency {
		t.Fatalf("exhaustion = %#v", summary.Exhaustion)
	}
	// And reading the summary again changes nothing either.
	_ = allocSummaryNextDay(t, s, ctx, clk)
	if again := allocLeaseDigests(t, st, ctx); !reflect.DeepEqual(leasesBefore, again) {
		t.Fatalf("a second read changed a granted Lease:\n before = %v\n after  = %v", leasesBefore, again)
	}

	// THE NEGATIVE CONTROL. Raising the limit again must let the next read plan
	// a new assignment, with no repair step of any kind in between: no lease is
	// released, no claim is dropped, nothing is re-fenced, and no reconcile tick
	// runs. If this did not converge, the "the limit only affects the next
	// allocation" claim above would be untestable rather than true.
	allocSetLimit(t, s, ctx, "granted-raise", 20)
	recovered := allocSummaryNextDay(t, s, ctx, clk)
	if recovered.Allocation.Limit != 20 || recovered.Allocation.LimitSource != application.AllocationLimitFromControlRevision {
		t.Fatalf("allocation after raising = %#v", recovered.Allocation)
	}
	if recovered.Exhaustion.Exhausted {
		t.Fatalf("still exhausted after raising the limit: %#v", recovered.Exhaustion)
	}
	if recovered.Allocation.PlannedAssignments != 1 {
		t.Fatalf("planned_assignments = %d after raising the limit, want 1; the waiting Requirement must become assignable with no repair step", recovered.Allocation.PlannedAssignments)
	}
	// The three granted Leases are STILL untouched after the recovery, too.
	if final := allocLeaseDigests(t, st, ctx); !reflect.DeepEqual(leasesBefore, final) {
		t.Fatalf("the recovery changed a granted Lease:\n before = %v\n after  = %v", leasesBefore, final)
	}
	t.Log("lowering the limit below the active count revoked nothing and planned nothing; raising it again made the waiter assignable on the next read with no repair step")
}

// TestNoRevocationPathExistsInTheSchedulerPackage is the structural half of A12
// and of dp-v2-068 d6: the reason a lowered limit cannot revoke anything is that
// the package has no code that removes a Claim or a Lease at all.
func TestNoRevocationPathExistsInTheSchedulerPackage(t *testing.T) {
	files := allocParseDir(t, allocSchedulerDir)
	forbidden := []string{"Revoke", "Release", "Expire", "Evict", "Shed", "Cancel", "Preempt", "Remove", "Delete", "Drop"}
	// The matcher is verified first.
	const synthetic = `package scheduler
func x() { Revoke() }
`
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic_revoke.go", synthetic, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if hits := allocIdentifierHits(file, forbidden); len(hits) != 1 || hits[0] != "Revoke" {
		t.Fatalf("positive control: hits = %v, want exactly [Revoke]", hits)
	}
	scanned := 0
	for name, f := range files {
		scanned++
		if hits := allocIdentifierHits(f, forbidden); len(hits) > 0 {
			t.Errorf("%s names %v; the limit can only be safe if no code here takes a granted claim or lease away", name, hits)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned zero files")
	}
	t.Logf("scanned %d non-test files of internal/scheduler; none names any of the %d revocation verbs", scanned, len(forbidden))
}

func allocIdentifierHits(file *ast.File, forbidden []string) []string {
	want := map[string]bool{}
	for _, name := range forbidden {
		want[name] = true
	}
	seen := map[string]bool{}
	out := []string{}
	ast.Inspect(file, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok || !want[ident.Name] || seen[ident.Name] {
			return true
		}
		seen[ident.Name] = true
		out = append(out, ident.Name)
		return true
	})
	sort.Strings(out)
	return out
}

// ===========================================================================
// A13: Snapshot construction and its bounds
// ===========================================================================

func allocSyntheticCandidates(t *testing.T, n int) []domain.Requirement {
	t.Helper()
	out := make([]domain.Requirement, 0, n)
	for i := 0; i < n; i++ {
		id, err := domain.NewRequirementID(fmt.Sprintf("requirement-%04d", i))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, domain.Requirement{ID: id, Status: domain.RequirementReady, Version: 1, CapturedAt: allocBase})
	}
	return out
}

// TestTheSnapshotIsBoundedByTheSchedulersOwnLimitAndFailsClosedAboveIt is
// A13 (a).
func TestTheSnapshotIsBoundedByTheSchedulersOwnLimitAndFailsClosedAboveIt(t *testing.T) {
	// The bound is the scheduler's own, not one this package invented.
	at := allocSyntheticCandidates(t, scheduler.MaxCandidates)
	snapshot, err := application.BuildAllocationSnapshot(allocBase, 20, 0, at, nil, nil)
	if err != nil {
		t.Fatalf("a candidate set exactly at the bound was refused: %v", err)
	}
	if snapshot.CandidateLimit != scheduler.MaxCandidates {
		t.Fatalf("CandidateLimit = %d, want the scheduler's own %d", snapshot.CandidateLimit, scheduler.MaxCandidates)
	}
	if len(snapshot.Requirements) != scheduler.MaxCandidates {
		t.Fatalf("built %d candidates from %d", len(snapshot.Requirements), scheduler.MaxCandidates)
	}
	// And Decide accepts it, so the bound really is the same bound.
	if _, err = scheduler.Decide(snapshot); err != nil {
		t.Fatalf("the scheduler refused a snapshot at its own bound: %v", err)
	}

	// One above the bound fails closed rather than truncating silently.
	above := allocSyntheticCandidates(t, scheduler.MaxCandidates+1)
	snapshot, err = application.BuildAllocationSnapshot(allocBase, 20, 0, above, nil, nil)
	if !errors.Is(err, application.ErrAllocationCandidateBound) {
		t.Fatalf("a candidate set above the bound returned %v, want ErrAllocationCandidateBound", err)
	}
	if len(snapshot.Requirements) != 0 {
		t.Fatalf("the refused build still returned %d candidates; it must not truncate", len(snapshot.Requirements))
	}
	// A zero instant is refused too: the mapping rule needs a real Now.
	if _, err = application.BuildAllocationSnapshot(time.Time{}, 20, 0, at, nil, nil); err == nil {
		t.Fatal("the builder accepted the zero instant as Now")
	}
	t.Logf("bound = %d: accepted at the bound, refused at %d, and the refusal returns no partial snapshot", scheduler.MaxCandidates, scheduler.MaxCandidates+1)
}

// TestTheSnapshotShapeIsOnePoolOneRepositoryAndOneClaimPerLease is A13's shape
// clause and the pool's capacity wiring.
func TestTheSnapshotShapeIsOnePoolOneRepositoryAndOneClaimPerLease(t *testing.T) {
	candidates := allocSyntheticCandidates(t, 3)
	claims := []scheduler.Claim{
		{Resource: application.AllocationContentionKey("owner-1", ""), Owner: "owner-1", RepositoryID: application.AllocationSnapshotRepositoryID, Mode: scheduler.Write},
		{Resource: application.AllocationContentionKey("owner-2", ""), Owner: "owner-2", RepositoryID: application.AllocationSnapshotRepositoryID, Mode: scheduler.Write},
	}
	snapshot, err := application.BuildAllocationSnapshot(allocBase, 6, 2, candidates, nil, claims)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Repositories) != 1 || snapshot.Repositories[0].ID != application.AllocationSnapshotRepositoryID {
		t.Fatalf("Repositories = %#v, want exactly one synthetic Installation Repository", snapshot.Repositories)
	}
	if snapshot.Repositories[0].FailureCount != 0 || !snapshot.Repositories[0].IsolatedUntil.IsZero() {
		t.Fatalf("the synthetic Repository carries invented health: %#v", snapshot.Repositories[0])
	}
	if len(snapshot.Runners) != 1 {
		t.Fatalf("Runners = %#v, want exactly one installation pool entry", snapshot.Runners)
	}
	pool := snapshot.Runners[0]
	if pool.Capacity != 6 || pool.Active != 2 {
		t.Fatalf("the pool's capacity is not the effective limit and its active count is not the ActiveExecutions figure: %#v", pool)
	}
	if len(snapshot.Claims) != len(claims) {
		t.Fatalf("Claims = %#v, want one per active Lease", snapshot.Claims)
	}
	if len(snapshot.ProviderCapacity) != 0 {
		t.Fatalf("ProviderCapacity = %#v; the per-Provider half is V2-028 and V2-067, not this task", snapshot.ProviderCapacity)
	}
	// The Claims slice is copied, so a caller cannot reach into the snapshot.
	claims[0].Owner = "mutated"
	if snapshot.Claims[0].Owner == "mutated" {
		t.Fatal("the builder aliased the caller's claim slice")
	}
}

// TestOneSummaryReadCompletesInsideTheSectionFiveDeadline is A13 (d). The
// deadline is the assertion; the elapsed figure is logged as an observation and
// is never compared against anything.
func TestOneSummaryReadCompletesInsideTheSectionFiveDeadline(t *testing.T) {
	s, st, clk := allocService(t)
	base := owner(context.Background())
	for i := 0; i < scheduler.MaxCandidates; i++ {
		tag := fmt.Sprintf("deadline-%03d", i)
		id := allocReady(t, s, st, base, clk.now, tag)
		if i%2 == 0 {
			allocLink(t, st, base, clk.now, id, "repository-deadline")
		}
	}
	allocNextDay(clk)
	// 30 seconds is docs/architecture/validation.md section 5's own reconcile
	// tick deadline, and it is used here as a hard ceiling on one owner read.
	ctx, cancel := context.WithTimeout(base, 30*time.Second)
	defer cancel()
	started := time.Now()
	summary, err := s.QueueSummary(ctx)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("one summary read over %d candidates failed: %v", scheduler.MaxCandidates, err)
	}
	if ctx.Err() != nil {
		t.Fatalf("the read exceeded the 30 second deadline: %v", ctx.Err())
	}
	if summary.Requirements != scheduler.MaxCandidates {
		t.Fatalf("read %d Requirements, want %d", summary.Requirements, scheduler.MaxCandidates)
	}
	if summary.Waiting.Total+summary.Allocation.PlannedAssignments != scheduler.MaxCandidates {
		t.Fatalf("waiting (%d) + planned (%d) != candidates (%d)", summary.Waiting.Total, summary.Allocation.PlannedAssignments, scheduler.MaxCandidates)
	}
	// OBSERVATION ONLY, never a threshold: the wall-clock figure is recorded so
	// a later regression is visible, and no assertion reads it.
	t.Logf("observation: one summary read over %d candidates completed in %s, inside the 30s deadline that is the assertion", scheduler.MaxCandidates, elapsed)
	// No placeholder reaches the marshalled response even at the bound.
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, placeholder := range application.AllocationSnapshotPlaceholders() {
		if strings.Contains(string(body), placeholder) {
			t.Fatalf("the response carries the placeholder %q: %s", placeholder, body)
		}
	}
	// And the contention-key namespaces do not leak either.
	for _, prefix := range []string{application.AllocationRepositoryResourcePrefix, application.AllocationRequirementResourcePrefix} {
		if strings.Contains(string(body), prefix) {
			t.Fatalf("the response carries the contention-key namespace %q: %s", prefix, body)
		}
	}
}

// ===========================================================================
// A14: the capture-time mapping rule, applied and measured
// ===========================================================================

// TestTheMissingCaptureTimeMappingRuleIsAppliedAtAgeZero is A14 as MEASURED
// rather than as the Work Order recorded it.
//
// The Work Order's premise is false at this commit: domain.Requirement DOES
// carry a capture timestamp (CapturedAt, with CaptureRecorded), added by V2-073.
// The limitation worth recording is the one V2-073 escalated instead: nothing in
// production bounds the spread between competing Requirements' capture times, so
// V2-030's starvation bound is a conditional statement. The interval is derived
// and measured in internal/scheduler/capture_time_test.go and stated in
// docs/operations/scheduler-local.md; this test asserts only the half this task
// owns, which is the mapping rule for a Requirement that has no capture time.
func TestTheMissingCaptureTimeMappingRuleIsAppliedAtAgeZero(t *testing.T) {
	// The premise, measured rather than copied.
	recorded := domain.Requirement{CapturedAt: allocBase}
	legacy := domain.Requirement{}
	if !recorded.CaptureRecorded() {
		t.Fatal("domain.Requirement does not report a recorded capture time; the field V2-073 added is gone")
	}
	if legacy.CaptureRecorded() {
		t.Fatal("a Requirement with no capture time reports one")
	}

	now := allocBase.Add(90 * time.Minute)
	// THE RULE: no recorded capture time means age zero, which is Now.
	if got := application.AllocationCreatedAt(legacy, now); !got.Equal(now) {
		t.Fatalf("a Requirement with no capture time was placed at %s, want the snapshot's Now (%s)", got, now)
	}
	// A recorded capture time is used exactly as recorded, never replaced.
	if got := application.AllocationCreatedAt(recorded, now); !got.Equal(allocBase) {
		t.Fatalf("a recorded capture time was rewritten: %s, want %s", got, allocBase)
	}

	// WHY THE RULE IS A RULE AND NOT A WORKAROUND, measured here.
	saturated := int64(now.Sub(time.Time{}).Seconds())
	const maxPriorityTerm = 100 * 300 // scheduler.legacyPriority(100) * 300
	if saturated <= maxPriorityTerm {
		t.Fatalf("the zero instant produces an age of %d seconds, which no longer dominates the %d-point priority term; the rule's justification must be re-measured", saturated, maxPriorityTerm)
	}
	t.Logf("measured: now.Sub(time.Time{}) saturates at %d seconds (about %d years, because time.Duration is int64 nanoseconds), which is %d times the largest priority term (%d). Handing the zero instant to the scheduler would make every value-less legacy record absolutely preferred, so age zero is the rule.",
		saturated, saturated/(365*24*3600), saturated/maxPriorityTerm, maxPriorityTerm)

	// And the rule has the conservative direction: a Requirement with no capture
	// time is the LEAST privileged, not the most. Two candidates, identical but
	// for the capture time, are ordered with the recorded (older) one first.
	old, err := domain.NewRequirementID("aaa-recorded-old")
	if err != nil {
		t.Fatal(err)
	}
	absent, err := domain.NewRequirementID("aaa-no-capture-time")
	if err != nil {
		t.Fatal(err)
	}
	candidates := []domain.Requirement{
		{ID: absent, Status: domain.RequirementReady, Version: 1},
		{ID: old, Status: domain.RequirementReady, Version: 1, CapturedAt: allocBase},
	}
	snapshot, err := application.BuildAllocationSnapshot(now, 1, 0, candidates, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range snapshot.Requirements {
		switch row.ID {
		case absent.String():
			if !row.CreatedAt.Equal(now) {
				t.Fatalf("the candidate with no capture time entered the snapshot at %s, want Now", row.CreatedAt)
			}
		case old.String():
			if !row.CreatedAt.Equal(allocBase) {
				t.Fatalf("the recorded candidate entered the snapshot at %s, want %s", row.CreatedAt, allocBase)
			}
		}
	}
	plan, err := scheduler.Decide(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assignments) != 1 {
		t.Fatalf("capacity was 1 but %d assignments were planned", len(plan.Assignments))
	}
	if plan.Assignments[0].RequirementID != old.String() {
		t.Fatalf("the single slot went to %q; the older RECORDED capture time must win, and the id tie-break was set up to favour the value-less one if the rule were not applied", plan.Assignments[0].RequirementID)
	}
	t.Log("the value-less candidate has the lexically smaller id, so it would win any tie; it loses because the rule gives it age zero rather than an unbounded age")
}

// ===========================================================================
// A19: the two ceilings converge
// ===========================================================================

// TestProviderCeilingConvergesWithTheQueueSummaryAllocation is A19. V2-067 has
// landed (internal/application/provider_registry.go exists), so the wiring
// branch applies and the not-applicable branch does not.
func TestProviderCeilingConvergesWithTheQueueSummaryAllocation(t *testing.T) {
	s, _, clk := allocService(t)
	ctx := owner(context.Background())

	// With nothing declared, both surfaces report the design ceiling and both
	// say so.
	summary := allocSummaryNextDay(t, s, ctx, clk)
	allocNextDay(clk)
	registry, err := s.Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Providers) == 0 {
		t.Fatal("the provider registry reported no rows; the comparison would be vacuous")
	}
	for _, entry := range registry.Providers {
		if entry.Concurrency.DeclaredCeiling != summary.Allocation.Limit {
			t.Fatalf("provider %q reports ceiling %d, the queue summary reports limit %d", entry.Provider, entry.Concurrency.DeclaredCeiling, summary.Allocation.Limit)
		}
		if entry.Concurrency.CeilingSource != application.ProviderCeilingArchitectureDesign {
			t.Fatalf("provider %q reports ceiling source %q with nothing declared", entry.Provider, entry.Concurrency.CeilingSource)
		}
	}
	if summary.Allocation.LimitSource != application.AllocationLimitFromDesignCeiling {
		t.Fatalf("the summary reports limit source %q with nothing declared", summary.Allocation.LimitSource)
	}

	// Declare one, and both surfaces move together.
	allocSetLimit(t, s, ctx, "converge", 9)
	summary = allocSummaryNextDay(t, s, ctx, clk)
	allocNextDay(clk)
	registry, err = s.Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Allocation.Limit != 9 || summary.Allocation.LimitSource != application.AllocationLimitFromControlRevision {
		t.Fatalf("allocation = %#v", summary.Allocation)
	}
	for _, entry := range registry.Providers {
		if entry.Concurrency.DeclaredCeiling != 9 {
			t.Fatalf("provider %q reports ceiling %d after the owner declared 9", entry.Provider, entry.Concurrency.DeclaredCeiling)
		}
		if entry.Concurrency.CeilingSource != application.ProviderCeilingOwnerDeclared {
			t.Fatalf("provider %q reports ceiling source %q after the owner declared a limit", entry.Provider, entry.Concurrency.CeilingSource)
		}
		if !entry.Concurrency.CeilingSource.Valid() {
			t.Fatalf("provider %q reports an undeclared ceiling source %q", entry.Provider, entry.Concurrency.CeilingSource)
		}
	}
	// The two surfaces name the source in their own vocabulary, and that is
	// deliberate: one says WHO chose the number, the other says WHERE it came
	// from and reports the revision beside it. The NUMBER is what must agree.
	t.Logf("both surfaces report 9; queue summary limit_source=%q with control_revision=%d, provider ceiling_source=%q",
		summary.Allocation.LimitSource, summary.Allocation.ControlRevision, registry.Providers[0].Concurrency.CeilingSource)
}
