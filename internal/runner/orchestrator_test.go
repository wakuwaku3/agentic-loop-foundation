package runner

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// runnerSeedRequirementStatus is internal/runner's ONE seeding helper, added by
// V2-089 because this package had none. It moves a Requirement to status
// directly through the store, bumping its Version exactly as a transition
// would, and validates before saving so a fixture cannot reach a record the
// domain rejects. It exists because V2-089 refuses a claim whose parent
// Requirement is not in one of the four statuses that admit work -- ready,
// active, waiting, recovering -- and this package's fixtures capture, plan,
// prepare and claim without framing.
//
// Every call site passes the status as a domain constant literal, never a
// variable and never a string, so the state a fixture establishes is readable
// at the fixture. The returned Version is the POST-seed version and the Plan
// that follows must carry it as its ExpectedRequirementVersion: dropping it,
// passing zero, or seeding after the Plan would each delete a real assertion.
//
// A store write rather than Service.StartFraming plus Service.CompleteFraming
// is deliberate and is recorded: each of these fixtures is a UNIT fixture for
// something else -- a lease keeper, a secret broker, a control agent, a crash
// resume, a publication dispatcher -- and threading two owner commands and two
// Control permits through each would put a Requirement lifecycle inside a test
// whose subject is not the Requirement lifecycle. The ONE journey in this
// package that must prove the admitting state is REACHABLE through the
// product's own commands is RunFakeJourney, and it does: see
// TestOrchestratorFakeJourney above and orchestrator.go's StartFraming and
// CompleteFraming calls, which use no store write at all.
func runnerSeedRequirementStatus(t *testing.T, st *memory.Store, ctx context.Context, id string, status domain.RequirementStatus) domain.Version {
	t.Helper()
	var version domain.Version
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		r, ok, e := u.Requirement(ctx, id)
		if e != nil {
			return e
		}
		if !ok {
			t.Fatalf("seed: requirement %q does not exist", id)
		}
		next := r
		next.Status = status
		next.Version++
		if e = domain.Validate(next); e != nil {
			return e
		}
		version = next.Version
		return u.SaveRequirement(ctx, next, r.Version)
	}); err != nil {
		t.Fatalf("seed requirement %q to %q: %v", id, status, err)
	}
	return version
}

type journeyClock struct{}

func (journeyClock) Now() time.Time { return time.Unix(1700000000, 0).UTC() }

type journeyIDs struct {
	mu sync.Mutex
	n  int
}

func (g *journeyIDs) Next(kind string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("%s-%d", kind, g.n), nil
}

func TestOrchestratorFakeJourney(t *testing.T) {
	st := memory.New()
	service, err := application.NewServiceWithConfig(st, journeyClock{}, &journeyIDs{}, application.ServiceConfig{
		InstallationID: "install",
		LeaseTTL:       time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &FakeProvider{Result: ProviderResult{Succeeded: true, Checkpoint: "checkpoint-1"}}
	workspaceRoot := t.TempDir()
	if err := os.Chmod(workspaceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	// V2-086: the identity is supplied by the constructing site rather than
	// fabricated inside RunFakeJourney. It is the same owner identity the
	// literal at orchestrator.go used to invent, so every assertion below is
	// unchanged -- what changed is who names it.
	o := &Orchestrator{Service: service, Provider: provider, Workspace: workspace, Journal: journal, RunnerID: "runner-1",
		Caller: application.Caller{Role: application.RoleOwner, Subject: "runner-local-owner"}}

	result, err := o.RunFakeJourney(context.Background(), JourneyRequest{RequestID: "journey-1", Text: "build the fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.ExecutionSucceeded || result.Checkpoint != "checkpoint-1" {
		t.Fatalf("unexpected journey result: %#v", result)
	}
	if _, err := o.RunFakeJourney(context.Background(), JourneyRequest{RequestID: "journey-1", Text: "build the fixture"}); err != nil {
		t.Fatalf("retry journey: %v", err)
	}
	if len(provider.Calls) != 1 {
		t.Fatalf("provider boundary called unexpectedly: %#v", provider.Calls)
	}
	// ProviderRequest no longer has a Prompt field at all (a raw prompt is
	// structurally unrepresentable); it carries a validated Work Packet
	// instead. This replaces the old Calls[0].Prompt == "" assertion.
	if err := provider.Calls[0].Packet.Validate(); err != nil {
		t.Fatalf("provider boundary carried an invalid work packet instead of a raw prompt: %v", err)
	}
	events, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Kind != "assignment" || events[1].Kind != "result_pending" || events[2].Kind != "result_accepted" {
		t.Fatalf("unexpected journal: %#v", events)
	}
	// V2-089 A17: this journey is the ONE place in the tree where the status
	// that admits a claim is established by the product's own commands rather
	// than by a fixture's store write, so the reachability is ASSERTED here
	// rather than described. RunFakeJourney issues Service.StartFraming and
	// V2-084's Service.CompleteFraming between the Capture and the Plan, so
	// the Requirement is `ready` BEFORE the claim -- observed through the
	// recorded requirement.ready event, which CompleteFraming writes and which
	// must precede the claim's own increment.claimed event -- and `active`
	// AFTER it, through V2-084's ready-to-active edge inside Claim.
	//
	// Neither half is a store read of a state a fixture assigned: the whole
	// path is Capture -> StartFraming -> CompleteFraming -> Plan -> Prepare ->
	// Claim, every step a real command through the real Service.
	readyAt, claimedAt := -1, -1
	for i, e := range st.Events() {
		switch {
		case e.Type == "requirement.ready" && e.AggregateID == result.RequirementID:
			if readyAt < 0 {
				readyAt = i
			}
		case e.Type == "increment.claimed" && e.AggregateID == result.IncrementID:
			if claimedAt < 0 {
				claimedAt = i
			}
		}
	}
	if readyAt < 0 {
		t.Fatalf("the journey recorded no requirement.ready event for %q, so the Requirement never reached `ready` through the product's own commands: %#v", result.RequirementID, st.Events())
	}
	if claimedAt < 0 {
		t.Fatalf("the journey recorded no increment.claimed event for %q: %#v", result.IncrementID, st.Events())
	}
	if readyAt >= claimedAt {
		t.Fatalf("requirement.ready was recorded at index %d and increment.claimed at %d: the Requirement was not `ready` before the claim", readyAt, claimedAt)
	}
	parent, ok := st.Requirement(result.RequirementID)
	if !ok {
		t.Fatalf("the journey's Requirement %q is absent", result.RequirementID)
	}
	if parent.Status != domain.RequirementActive {
		t.Fatalf("after the claim the journey's Requirement is %q, want %q", parent.Status, domain.RequirementActive)
	}
	t.Logf("V2-089 A17 reachability: requirement.ready at event index %d, increment.claimed at %d, and the parent Requirement %q is %q after the claim -- established by Capture, StartFraming, CompleteFraming, Plan, Prepare and Claim with no store write.",
		readyAt, claimedAt, result.RequirementID, parent.Status)
}

// TestOrchestratorRefusesAnIncompleteInjectedCaller is V2-086 A8's refusal
// proof. A required injected identity that silently defaults is the fabrication
// again, so the ABSENCE case is asserted rather than described: the zero value,
// a Role with no Subject and a Subject with no Role are each refused before any
// command runs, and the refusal is in the same style as the existing
// "orchestrator dependencies are incomplete" guard.
//
// The proof that the refusal happens before any effect is that the Provider
// boundary is never reached and the Journal stays empty, which is checked for
// every case rather than only for the zero value.
func TestOrchestratorRefusesAnIncompleteInjectedCaller(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller application.Caller
	}{
		{name: "the zero value: no role and no subject", caller: application.Caller{}},
		{name: "a role with no subject", caller: application.Caller{Role: application.RoleOwner}},
		{name: "a subject with no role", caller: application.Caller{Subject: "runner-local-owner"}},
		{name: "a scheduler role with no subject", caller: application.Caller{Role: application.RoleScheduler}},
		{name: "a runner id but still no role", caller: application.Caller{RunnerID: "runner-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := memory.New()
			service, err := application.NewServiceWithConfig(st, journeyClock{}, &journeyIDs{}, application.ServiceConfig{
				InstallationID: "install",
				LeaseTTL:       time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			journal, err := OpenJournal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			provider := &FakeProvider{Result: ProviderResult{Succeeded: true, Checkpoint: "checkpoint-1"}}
			workspaceRoot := t.TempDir()
			if err := os.Chmod(workspaceRoot, 0700); err != nil {
				t.Fatal(err)
			}
			workspace, err := NewWorkspace(workspaceRoot)
			if err != nil {
				t.Fatal(err)
			}
			o := &Orchestrator{Service: service, Provider: provider, Workspace: workspace, Journal: journal, RunnerID: "runner-1", Caller: tc.caller}
			_, err = o.RunFakeJourney(context.Background(), JourneyRequest{RequestID: "journey-refused", Text: "must not run"})
			if err == nil {
				t.Fatal("RunFakeJourney ran with an incomplete injected caller; a required identity that defaults is the fabrication again")
			}
			if got := err.Error(); got != "orchestrator caller identity is incomplete" {
				t.Fatalf("refusal message = %q, want the fail-closed guard's own message", got)
			}
			if len(provider.Calls) != 0 {
				t.Fatalf("the Provider boundary was reached before the identity was checked: %#v", provider.Calls)
			}
			events, err := journal.Replay()
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Fatalf("the journal recorded %d events before the identity was checked: %#v", len(events), events)
			}
		})
	}
}

// TestOrchestratorNamesNoIdentityOfItsOwn is the other half of A8: the
// component must not be able to reacquire the fabrication by reintroducing a
// literal. It parses internal/runner's own non-test source and asserts that no
// application.Caller composite literal in this package names a Role at all
// except the runner identity built from o.RunnerID -- so neither RoleOwner nor
// RoleScheduler can be minted here again.
func TestOrchestratorNamesNoIdentityOfItsOwn(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("the scan found zero *.go files; the working directory is not internal/runner")
	}
	sort.Strings(matches)
	fset := token.NewFileSet()
	scanned, found := 0, []string{}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "Caller" {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Role" {
					continue
				}
				value, ok := kv.Value.(*ast.SelectorExpr)
				if !ok || value.Sel == nil {
					continue
				}
				if value.Sel.Name == "RoleRunner" {
					// The one identity this component may assert is its own,
					// and its subject is o.RunnerID rather than a string.
					continue
				}
				found = append(found, path+":"+strconv.Itoa(fset.Position(lit.Pos()).Line)+" Role: "+value.Sel.Name)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("the scan parsed zero non-test files")
	}
	if len(found) != 0 {
		t.Fatalf("internal/runner names an identity of its own at %v; the owner-side caller is injected, never fabricated", found)
	}
	t.Logf("identity scan parsed %d non-test files of internal/runner and found no fabricated Caller role", scanned)
}
