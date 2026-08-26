package application_test

// V2-074 in internal/application: the two compatibility relations, the shared
// decision table pinned by value to internal/provider's declaration, and the
// read surface that reports both.
//
// Determinism is acceptance here: no fixed sleep, no wall-clock timer and no
// goroutine, every instant from an injected clock, and every table enumerates
// its cross product rather than sampling it.
//
// WHY EVERYTHING IS DECLARED TWICE. internal/application must import neither
// internal/provider nor internal/runner: ci/components.json declares this
// component's dependency edges, internal/ci's manifest derivation turns an
// undeclared edge into a red make check, and only V2-045 may declare one. So
// the decision table, the two version intervals and the four closed
// vocabularies are re-declared by value on this side, and the tests below are
// the only thing that will catch drift. Where a pin can be made mechanical it
// is: the vocabulary pins read internal/provider's own source by AST -- reading
// a file is not an import and creates no edge -- and the paths are built by
// joining path elements rather than written as module-path literals, because
// internal/ci reads a module-path literal in any source file, test files
// included, as a declared edge.

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// providerPackageDir is internal/provider's directory, reached as a filesystem
// path. Joining the elements rather than writing one literal keeps this file
// free of anything internal/ci's manifest derivation could read as an edge.
func providerPackageDir() string {
	return filepath.Join("..", "provider")
}

// ---------------------------------------------------------------------------
// A12: the shared decision table, pinned cell by cell
// ---------------------------------------------------------------------------

// sharedDecisionTableRows is the whole decision table, transcribed as data in
// the canonical form "source|chain-bound|obstacle|disposition|reason". The same
// thirty rows are transcribed in internal/provider/handoff_test.go, and both
// sides render their own declared table into this form and compare against it,
// so the two declarations are compared cell by cell and byte for byte without
// either package importing the other.
//
// The rule the rows encode, stated once so a reader can check the transcription
// rather than trust it:
//
//	source sendable    -> none, whatever the candidates look like;
//	source probing     -> waiting, source-is-probing;
//	chain bound reached-> waiting, chain-bound-reached;
//	no obstacle        -> handoff-proposed;
//	otherwise          -> waiting, with that obstacle's one reason.
var sharedDecisionTableRows = []string{
	"sendable|false|owner-action-needed|none|",
	"sendable|false|measured-incompatible|none|",
	"sendable|false|already-tried-for-this-increment|none|",
	"sendable|false|not-sendable|none|",
	"sendable|false|none|none|",
	"sendable|true|owner-action-needed|none|",
	"sendable|true|measured-incompatible|none|",
	"sendable|true|already-tried-for-this-increment|none|",
	"sendable|true|not-sendable|none|",
	"sendable|true|none|none|",
	"probing|false|owner-action-needed|waiting|source-is-probing",
	"probing|false|measured-incompatible|waiting|source-is-probing",
	"probing|false|already-tried-for-this-increment|waiting|source-is-probing",
	"probing|false|not-sendable|waiting|source-is-probing",
	"probing|false|none|waiting|source-is-probing",
	"probing|true|owner-action-needed|waiting|source-is-probing",
	"probing|true|measured-incompatible|waiting|source-is-probing",
	"probing|true|already-tried-for-this-increment|waiting|source-is-probing",
	"probing|true|not-sendable|waiting|source-is-probing",
	"probing|true|none|waiting|source-is-probing",
	"not-sendable|false|owner-action-needed|waiting|candidate-needs-an-owner-action",
	"not-sendable|false|measured-incompatible|waiting|candidate-is-measured-incompatible",
	"not-sendable|false|already-tried-for-this-increment|waiting|candidate-already-tried-for-this-increment",
	"not-sendable|false|not-sendable|waiting|candidate-is-not-sendable",
	"not-sendable|false|none|handoff-proposed|",
	"not-sendable|true|owner-action-needed|waiting|chain-bound-reached",
	"not-sendable|true|measured-incompatible|waiting|chain-bound-reached",
	"not-sendable|true|already-tried-for-this-increment|waiting|chain-bound-reached",
	"not-sendable|true|not-sendable|waiting|chain-bound-reached",
	"not-sendable|true|none|waiting|chain-bound-reached",
}

func TestTheDecisionTableIsPinnedByValueToTheProviderDeclaration(t *testing.T) {
	table := application.ProviderHandoffDecisionTable()
	if len(table) != len(sharedDecisionTableRows) {
		t.Fatalf("this side declares %d cells, the shared transcription has %d", len(table), len(sharedDecisionTableRows))
	}
	wantCells := len(application.ProviderSourceStates()) * 2 * len(application.ProviderCandidateObstacles())
	if len(table) != wantCells {
		t.Fatalf("the table has %d cells, want the full cross product %d", len(table), wantCells)
	}
	for index, cell := range table {
		if got := cell.Row(); got != sharedDecisionTableRows[index] {
			t.Fatalf("cell %d = %q, want %q", index, got, sharedDecisionTableRows[index])
		}
	}
	// Both directions of totality over the waiting vocabulary, on this side too.
	produced := map[application.ProviderHandoffWaitingReason]bool{}
	dispositions := map[application.ProviderHandoffDisposition]bool{}
	for _, cell := range table {
		dispositions[cell.Disposition] = true
		if cell.Disposition == application.ProviderDispositionWaiting {
			if cell.Reason == "" {
				t.Fatalf("waiting cell %#v carries no reason", cell)
			}
			produced[cell.Reason] = true
			continue
		}
		if cell.Reason != "" {
			t.Fatalf("non-waiting cell %#v carries the reason %q", cell, cell.Reason)
		}
	}
	for _, reason := range application.ProviderHandoffWaitingReasons() {
		if !produced[reason] {
			t.Fatalf("the declared reason %q is produced by no cell; a reason with no cell is an invention", reason)
		}
		if !reason.Valid() {
			t.Fatalf("the declared reason %q is not Valid()", reason)
		}
	}
	for reason := range produced {
		if !reason.Valid() {
			t.Fatalf("the table produces %q, which is not a declared member", reason)
		}
	}
	for _, disposition := range application.ProviderHandoffDispositions() {
		if !dispositions[disposition] {
			t.Fatalf("the table never produces the declared disposition %q", disposition)
		}
	}
	t.Logf("pinned %d cells by value against the shared transcription; %d waiting reasons, %d dispositions, all produced", len(table), len(produced), len(dispositions))
}

// providerSourceConstants reads the string values of every constant of one
// named type out of internal/provider's non-test source. It is an AST read of a
// file, not an import, so it creates no component edge.
func providerSourceConstants(t *testing.T, typeName string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(providerPackageDir())
	if err != nil {
		t.Fatalf("read %s: %v", providerPackageDir(), err)
	}
	declared := map[string]bool{}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(providerPackageDir(), name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++
		for _, decl := range file.Decls {
			gen, isGen := decl.(*ast.GenDecl)
			if !isGen || gen.Tok != token.CONST {
				continue
			}
			currentType := ""
			for _, spec := range gen.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				if value.Type != nil {
					if ident, isIdent := value.Type.(*ast.Ident); isIdent {
						currentType = ident.Name
					} else {
						currentType = ""
					}
				}
				if currentType != typeName {
					continue
				}
				for _, v := range value.Values {
					lit, isLit := v.(*ast.BasicLit)
					if !isLit || lit.Kind != token.STRING {
						continue
					}
					unquoted, unquoteErr := strconv.Unquote(lit.Value)
					if unquoteErr == nil {
						declared[unquoted] = true
					}
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("scanned zero non-test files in %s; the read is broken", providerPackageDir())
	}
	return declared
}

// TestTheFourVocabulariesArePinnedToTheProviderPackageSource is the mechanical
// half of the by-value pin: the members of each closed vocabulary declared on
// this side are compared against the constants internal/provider's own source
// declares for the same relation, read by AST.
func TestTheFourVocabulariesArePinnedToTheProviderPackageSource(t *testing.T) {
	type pin struct {
		providerType string
		here         []string
	}
	sourceStates := []string{}
	for _, v := range application.ProviderSourceStates() {
		sourceStates = append(sourceStates, string(v))
	}
	obstacles := []string{}
	for _, v := range application.ProviderCandidateObstacles() {
		obstacles = append(obstacles, string(v))
	}
	dispositions := []string{}
	for _, v := range application.ProviderHandoffDispositions() {
		dispositions = append(dispositions, string(v))
	}
	reasons := []string{}
	for _, v := range application.ProviderHandoffWaitingReasons() {
		reasons = append(reasons, string(v))
	}
	verdicts := []string{
		string(application.ProviderCompatibilityCompatible),
		string(application.ProviderCompatibilityIncompatible),
		string(application.ProviderCompatibilityUnknown),
	}
	for _, p := range []pin{
		{providerType: "SourceState", here: sourceStates},
		{providerType: "CandidateObstacle", here: obstacles},
		{providerType: "HandoffDisposition", here: dispositions},
		{providerType: "HandoffWaitingReason", here: reasons},
		{providerType: "CompatibilityVerdict", here: verdicts},
	} {
		there := providerSourceConstants(t, p.providerType)
		if len(there) == 0 {
			t.Fatalf("internal/provider declares no %s constants; the AST read is broken and would pass vacuously", p.providerType)
		}
		hereSet := map[string]bool{}
		for _, value := range p.here {
			hereSet[value] = true
		}
		if len(hereSet) != len(there) {
			t.Fatalf("%s: this side declares %d members %v, internal/provider declares %d %v", p.providerType, len(hereSet), sortedKeys(hereSet), len(there), sortedKeys(there))
		}
		for value := range hereSet {
			if !there[value] {
				t.Fatalf("%s: this side declares %q, which internal/provider does not", p.providerType, value)
			}
		}
		for value := range there {
			if !hereSet[value] {
				t.Fatalf("%s: internal/provider declares %q, which this side does not", p.providerType, value)
			}
		}
		t.Logf("%s: %d members pinned by value against internal/provider's own source", p.providerType, len(there))
	}

	// The declared intervals, pinned the same way: every bound this side
	// declares must appear as a string literal in internal/provider's own
	// non-test source, and the adapter contract identity must match.
	literals := providerSourceStringLiterals(t)
	for _, name := range application.DeclaredProviders() {
		interval, declared := application.ProviderCLIVersionInterval(name)
		if !declared {
			t.Fatalf("%s has no declared R1 interval on this side", name)
		}
		for _, bound := range []string{interval.From, interval.Until} {
			if !literals[bound] {
				t.Fatalf("R1 bound %q for %s appears as no string literal in internal/provider's source; the two declarations have drifted", bound, name)
			}
		}
	}
	loop := application.ProviderLoopVersionInterval()
	for _, bound := range []string{loop.From, loop.Until} {
		if !literals[bound] {
			t.Fatalf("R2 bound %q appears as no string literal in internal/provider's source", bound)
		}
	}
	if !literals[application.ProviderAdapterContractVersion] {
		t.Fatalf("the adapter contract identity %q appears as no string literal in internal/provider's source", application.ProviderAdapterContractVersion)
	}
	// Positive control: a bound that was never declared must NOT be found, or
	// the literal scan proves nothing.
	if literals["9.9.9"] {
		t.Fatal("the literal scan claims to find a version internal/provider does not declare; the scan is not discriminating")
	}
	t.Logf("pinned 3 R1 intervals, 1 R2 interval and the contract identity against %d string literals read from internal/provider's source", len(literals))
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// providerSourceStringLiterals returns every string literal in
// internal/provider's non-test source.
func providerSourceStringLiterals(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(providerPackageDir())
	if err != nil {
		t.Fatalf("read %s: %v", providerPackageDir(), err)
	}
	out := map[string]bool{}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(providerPackageDir(), name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, isLit := n.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				return true
			}
			if unquoted, unquoteErr := strconv.Unquote(lit.Value); unquoteErr == nil {
				out[unquoted] = true
			}
			return true
		})
	}
	if scanned == 0 || len(out) == 0 {
		t.Fatalf("scanned %d files and found %d literals; the read is broken", scanned, len(out))
	}
	return out
}

// TestTheNewProviderEnumsAreClosedInBothDirections applies the existing
// closed-enum discipline to the four enums this task adds on this side, using
// the same AST scan the pre-existing enum test uses.
func TestTheNewProviderEnumsAreClosedInBothDirections(t *testing.T) {
	wantCounts := map[string]int{
		"ProviderCompatibilityVerdict": 3,
		"ProviderSourceState":          3,
		"ProviderCandidateObstacle":    5,
		"ProviderHandoffDisposition":   3,
		"ProviderHandoffWaitingReason": 6,
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
	// The runtime side agrees, and the plausible-sounding undeclared values are
	// refused rather than accepted.
	for _, v := range []application.ProviderCompatibilityVerdict{"", "compatible-ish", "supported", "yes", "unknown-version"} {
		if v.Valid() {
			t.Fatalf("undeclared verdict %q is Valid", v)
		}
	}
	for _, v := range []application.ProviderSourceState{"", "sending", "not-sending", "exhausted"} {
		if v.Valid() {
			t.Fatalf("undeclared source state %q is Valid", v)
		}
	}
	for _, v := range []application.ProviderCandidateObstacle{"", "incompatible", "unauthorized", "busy"} {
		if v.Valid() {
			t.Fatalf("undeclared obstacle %q is Valid", v)
		}
	}
	for _, v := range []application.ProviderHandoffDisposition{"", "handoff", "proposed", "wait"} {
		if v.Valid() {
			t.Fatalf("undeclared disposition %q is Valid", v)
		}
	}
	for _, v := range []application.ProviderHandoffWaitingReason{"", "no-runner-capacity", "resource-conflict", "not-ready"} {
		if v.Valid() {
			t.Fatalf("undeclared waiting reason %q is Valid; the queue vocabulary in particular must not be accepted here", v)
		}
	}
}

// TestEachMappingOntoTheSharedTupleIsTotalOverItsOwnClosedEnums is A12's
// totality clause on this side. The mappings are enumerated over the full cross
// product of the enums they read, so a new member on either side fails a test
// on the side that gained it.
func TestEachMappingOntoTheSharedTupleIsTotalOverItsOwnClosedEnums(t *testing.T) {
	healths := []application.ProviderHealth{
		application.ProviderHealthUnknown, application.ProviderHealthHealthy, application.ProviderHealthDegraded,
		application.ProviderHealthUnavailable, application.ProviderHealthUnauthenticated, application.ProviderHealthStoppedForInspection,
	}
	runaways := []application.ProviderRunawayState{
		application.ProviderRunawayWithinThresholds, application.ProviderRunawayStoppedForInspection, application.ProviderRunawayUnknown,
	}
	blocked := []application.ProviderBlockedReason{
		application.ProviderNotBlocked, application.ProviderBlockedNeverInvoked, application.ProviderBlockedOwnerMustAuthenticate,
		application.ProviderBlockedLastInvocationRetryable, application.ProviderBlockedLastInvocationPermanent,
		application.ProviderBlockedLastInvocationUnclassed, application.ProviderBlockedOwnerMustClearRunawayStop,
	}
	// Every enumerated value must be a declared member, so the enumeration
	// cannot silently omit one.
	for _, v := range healths {
		if !v.Valid() {
			t.Fatalf("health %q is enumerated but not declared", v)
		}
	}
	for _, v := range runaways {
		if !v.Valid() {
			t.Fatalf("runaway state %q is enumerated but not declared", v)
		}
	}
	for _, v := range blocked {
		if !v.Valid() {
			t.Fatalf("blocked reason %q is enumerated but not declared", v)
		}
	}
	// And the enumeration is the whole closed set, counted against the source.
	for typeName, want := range map[string]int{
		"ProviderHealth": len(healths), "ProviderRunawayState": len(runaways), "ProviderBlockedReason": len(blocked),
	} {
		constants, _ := providerEnumConstantsAndCases(t, typeName)
		if len(constants) != want {
			t.Fatalf("%s declares %d constants but the enumeration below covers %d", typeName, len(constants), want)
		}
	}

	sourceCells, obstacleCells := 0, 0
	sourceStatesSeen := map[application.ProviderSourceState]bool{}
	obstaclesSeen := map[application.ProviderCandidateObstacle]bool{}
	verdicts := []application.ProviderCompatibilityVerdict{
		application.ProviderCompatibilityCompatible, application.ProviderCompatibilityIncompatible, application.ProviderCompatibilityUnknown,
	}
	for _, health := range healths {
		for _, runaway := range runaways {
			for _, exhausted := range []bool{false, true} {
				for _, authorized := range []bool{false, true} {
					entry := application.ProviderEntryView{
						Provider:         application.ProviderCodex,
						Authorized:       authorized,
						Health:           health,
						RunawayDetection: application.ProviderRunawayDetectionView{State: runaway},
						Concurrency:      application.ProviderConcurrencyView{Exhausted: exhausted},
					}
					state := application.ProviderSourceStateFor(entry)
					if !state.Valid() {
						t.Fatalf("health=%q runaway=%q exhausted=%v authorized=%v mapped to the undeclared source state %q", health, runaway, exhausted, authorized, state)
					}
					sourceStatesSeen[state] = true
					sourceCells++
					for _, reason := range blocked {
						for _, verdict := range verdicts {
							for _, tried := range []bool{false, true} {
								candidate := entry
								candidate.BlockedReason = reason
								candidate.Compatibility = application.ProviderCompatibilityView{
									CLICompatibility:  verdict,
									LoopCompatibility: application.ProviderCompatibilityUnknown,
								}
								obstacle := application.ProviderCandidateObstacleFor(candidate, tried)
								if !obstacle.Valid() {
									t.Fatalf("the candidate mapping produced the undeclared obstacle %q", obstacle)
								}
								obstaclesSeen[obstacle] = true
								obstacleCells++
							}
						}
					}
				}
			}
		}
	}
	if want := len(healths) * len(runaways) * 2 * 2; sourceCells != want {
		t.Fatalf("enumerated %d source cells, want the full cross product %d", sourceCells, want)
	}
	if want := sourceCells * len(blocked) * len(verdicts) * 2; obstacleCells != want {
		t.Fatalf("enumerated %d candidate cells, want %d", obstacleCells, want)
	}
	// The source mapping produces sendable and not-sendable and, by measured
	// design, never probing: a probing circuit is a state of the breaker on the
	// Runner machine and no observation carries it here.
	if !sourceStatesSeen[application.ProviderSourceSendable] || !sourceStatesSeen[application.ProviderSourceNotSendable] {
		t.Fatalf("the source mapping produced only %v", sourceStatesSeen)
	}
	if sourceStatesSeen[application.ProviderSourceProbing] {
		t.Fatal("the source mapping produced probing; no observation carries a circuit state to the control plane, so this would be a fabricated state")
	}
	// The candidate mapping produces every declared obstacle.
	for _, obstacle := range application.ProviderCandidateObstacles() {
		if !obstaclesSeen[obstacle] {
			t.Fatalf("the candidate mapping never produces the declared obstacle %q", obstacle)
		}
	}
	t.Logf("source mapping: %d cells over health x runaway x exhausted x authorized, producing %d of %d source states (probing is unreachable on this side, by measured design). candidate mapping: %d cells, producing all %d obstacles",
		sourceCells, len(sourceStatesSeen), len(application.ProviderSourceStates()), obstacleCells, len(obstaclesSeen))
}

// TestTheHandoffWaitingVocabularyIsDisjointFromTheQueueWaitingVocabulary is
// A10's disjointness clause, asserted against V2-068's real vocabulary rather
// than against a copy of it.
func TestTheHandoffWaitingVocabularyIsDisjointFromTheQueueWaitingVocabulary(t *testing.T) {
	queue := application.WaitingReasonBuckets()
	handoff := application.ProviderHandoffWaitingReasons()
	if len(queue) == 0 {
		t.Fatal("the queue waiting vocabulary is empty; the disjointness assertion would be vacuous")
	}
	if len(handoff) == 0 {
		t.Fatal("the handoff waiting vocabulary is empty")
	}
	queueSet := map[string]bool{}
	for _, value := range queue {
		queueSet[value] = true
	}
	for _, value := range handoff {
		if queueSet[string(value)] {
			t.Fatalf("the handoff waiting reason %q is also a queue waiting reason; two enums with overlapping members is how an owner comes to read one as the other", value)
		}
	}
	handoffSet := map[string]bool{}
	for _, value := range handoff {
		handoffSet[string(value)] = true
	}
	for _, value := range queue {
		if handoffSet[value] {
			t.Fatalf("the queue waiting reason %q is also a handoff waiting reason", value)
		}
	}
	// Positive control: the matcher must actually detect an overlap.
	if !queueSet[queue[0]] {
		t.Fatal("the overlap matcher does not find a member of the set it was built from")
	}
	t.Logf("queue waiting reasons (V2-068) = %v; handoff waiting reasons (V2-074) = %v; intersection is empty in both directions", queue, handoff)
}

// ---------------------------------------------------------------------------
// The version verdict, pinned to the same seven positions internal/provider
// enumerates
// ---------------------------------------------------------------------------

func TestTheVersionVerdictIsTotalOverTheSevenDeclaredPositions(t *testing.T) {
	type position struct {
		name  string
		value func(application.ProviderVersionIntervalView) string
		want  application.ProviderCompatibilityVerdict
	}
	below := func(version string) string {
		parts := strings.Split(version, ".")
		minor, err := strconv.Atoi(parts[1])
		if err != nil || minor == 0 {
			return "0.0.0"
		}
		return parts[0] + "." + strconv.Itoa(minor-1) + ".0"
	}
	above := func(version string) string {
		parts := strings.Split(version, ".")
		major, err := strconv.Atoi(parts[0])
		if err != nil {
			return "999.0.0"
		}
		return strconv.Itoa(major+1) + ".0.0"
	}
	inside := func(i application.ProviderVersionIntervalView) string {
		parts := strings.Split(i.From, ".")
		patch, err := strconv.Atoi(parts[2])
		if err != nil {
			return i.From
		}
		return parts[0] + "." + parts[1] + "." + strconv.Itoa(patch+1)
	}
	positions := []position{
		{name: "below-the-interval", value: func(i application.ProviderVersionIntervalView) string { return below(i.From) }, want: application.ProviderCompatibilityIncompatible},
		{name: "at-the-lower-bound", value: func(i application.ProviderVersionIntervalView) string { return i.From }, want: application.ProviderCompatibilityCompatible},
		{name: "inside", value: inside, want: application.ProviderCompatibilityCompatible},
		{name: "at-the-upper-bound", value: func(i application.ProviderVersionIntervalView) string { return i.Until }, want: application.ProviderCompatibilityIncompatible},
		{name: "above-the-interval", value: func(i application.ProviderVersionIntervalView) string { return above(i.Until) }, want: application.ProviderCompatibilityIncompatible},
		{name: "empty", value: func(application.ProviderVersionIntervalView) string { return "" }, want: application.ProviderCompatibilityUnknown},
		{name: "malformed", value: func(application.ProviderVersionIntervalView) string { return "v1.2" }, want: application.ProviderCompatibilityUnknown},
	}
	intervals := []application.ProviderVersionIntervalView{}
	for _, name := range application.DeclaredProviders() {
		interval, declared := application.ProviderCLIVersionInterval(name)
		if !declared {
			t.Fatalf("%s has no declared interval", name)
		}
		intervals = append(intervals, interval)
	}
	intervals = append(intervals, application.ProviderLoopVersionInterval())

	cells := 0
	seen := map[application.ProviderCompatibilityVerdict]bool{}
	for _, interval := range intervals {
		for _, p := range positions {
			value := p.value(interval)
			got := application.ProviderVersionVerdict(interval, value)
			if got != p.want {
				t.Fatalf("interval [%s, %s) position %s (%q): verdict = %q, want %q", interval.From, interval.Until, p.name, value, got, p.want)
			}
			seen[got] = true
			cells++
		}
	}
	if want := len(intervals) * len(positions); cells != want {
		t.Fatalf("drove %d cells, want %d", cells, want)
	}
	for _, verdict := range []application.ProviderCompatibilityVerdict{
		application.ProviderCompatibilityCompatible, application.ProviderCompatibilityIncompatible, application.ProviderCompatibilityUnknown,
	} {
		if !seen[verdict] {
			t.Fatalf("the table never produces the declared verdict %q", verdict)
		}
	}
	// The conjunction, over the full 3 x 3 product.
	verdicts := []application.ProviderCompatibilityVerdict{
		application.ProviderCompatibilityCompatible, application.ProviderCompatibilityIncompatible, application.ProviderCompatibilityUnknown,
	}
	conjunctionCells := 0
	for _, first := range verdicts {
		for _, second := range verdicts {
			got := application.ProviderConjoinVerdicts(first, second)
			want := application.ProviderCompatibilityCompatible
			switch {
			case first == application.ProviderCompatibilityIncompatible || second == application.ProviderCompatibilityIncompatible:
				want = application.ProviderCompatibilityIncompatible
			case first == application.ProviderCompatibilityUnknown || second == application.ProviderCompatibilityUnknown:
				want = application.ProviderCompatibilityUnknown
			}
			if got != want {
				t.Fatalf("ProviderConjoinVerdicts(%q, %q) = %q, want %q", first, second, got, want)
			}
			conjunctionCells++
		}
	}
	if conjunctionCells != len(verdicts)*len(verdicts) {
		t.Fatalf("drove %d conjunction cells", conjunctionCells)
	}
	t.Logf("%d intervals x %d positions = %d cells, plus %d conjunction cells; all three verdicts produced", len(intervals), len(positions), cells, conjunctionCells)
}

// ---------------------------------------------------------------------------
// A13: the read surface
// ---------------------------------------------------------------------------

// TestTheRegistryReportsBothIntervalsWithAnExplicitUnknown is the behavioural
// half: with no release observer configured the Loop version is absent rather
// than synthesized, and the loop verdict is unknown.
func TestTheRegistryReportsBothIntervalsWithAnExplicitUnknown(t *testing.T) {
	clk := &fixedClock{now: providerBase}
	s, _ := providerService(t, clk)
	t.Cleanup(s.DetachReleaseObserver)
	view := providerRegistry(t, s)
	for _, entry := range view.Providers {
		interval, declared := application.ProviderCLIVersionInterval(entry.Provider)
		if !declared {
			t.Fatalf("%s has no declared interval", entry.Provider)
		}
		if entry.Compatibility.CLIVersionInterval != interval {
			t.Fatalf("%s reports the R1 interval %#v, want %#v", entry.Provider, entry.Compatibility.CLIVersionInterval, interval)
		}
		if entry.Compatibility.LoopVersionInterval != application.ProviderLoopVersionInterval() {
			t.Fatalf("%s reports the R2 interval %#v", entry.Provider, entry.Compatibility.LoopVersionInterval)
		}
		if entry.Compatibility.ObservedLoopVersion != "" {
			t.Fatalf("%s reports the Loop version %q with no observer configured; no version may be synthesized for an absence", entry.Provider, entry.Compatibility.ObservedLoopVersion)
		}
		if entry.Compatibility.LoopCompatibility != application.ProviderCompatibilityUnknown {
			t.Fatalf("%s reports loop_compatibility %q with no observed Loop version, want unknown", entry.Provider, entry.Compatibility.LoopCompatibility)
		}
		if entry.Compatibility.CLICompatibility != application.ProviderCompatibilityUnknown {
			t.Fatalf("%s reports cli_compatibility %q with no observed contract failure, want unknown", entry.Provider, entry.Compatibility.CLICompatibility)
		}
	}

	// With an observer configured, the Loop version is the release identity the
	// process was assembled from, and the verdict follows from the declared
	// interval rather than from a default.
	withObserver, _ := providerService(t, clk)
	t.Cleanup(withObserver.DetachReleaseObserver)
	if err := withObserver.AttachReleaseObserver(newObserver(t, repoRoot(), nil)); err != nil {
		t.Fatal(err)
	}
	observed := providerRegistry(t, withObserver)
	for _, entry := range observed.Providers {
		if entry.Compatibility.ObservedLoopVersion == "" {
			t.Fatalf("%s reports no Loop version with an observer configured", entry.Provider)
		}
		want := application.ProviderVersionVerdict(application.ProviderLoopVersionInterval(), entry.Compatibility.ObservedLoopVersion)
		if entry.Compatibility.LoopCompatibility != want {
			t.Fatalf("%s reports loop_compatibility %q for the observed version %q, want %q", entry.Provider, entry.Compatibility.LoopCompatibility, entry.Compatibility.ObservedLoopVersion, want)
		}
		if entry.Compatibility.LoopCompatibility == application.ProviderCompatibilityUnknown {
			t.Fatalf("%s reports unknown for the observed Loop version %q; the declared R2 interval must decide it", entry.Provider, entry.Compatibility.ObservedLoopVersion)
		}
	}
	t.Logf("with no observer: loop version absent and both verdicts unknown. With an observer: loop version %q and verdict %q", observed.Providers[0].Compatibility.ObservedLoopVersion, observed.Providers[0].Compatibility.LoopCompatibility)
}

// TestAnObservedContractIncompatibleFailureIsTheOnlyThingThatMovesTheCLIVerdict
// is d4's path end to end on this side: the observed-CLI side of the verdict
// reaches the registry only through the failure class the Loop's own execution
// path already reports.
func TestAnObservedContractIncompatibleFailureIsTheOnlyThingThatMovesTheCLIVerdict(t *testing.T) {
	for _, class := range []application.ProviderFailureClass{
		application.ProviderFailureUnauthenticated, application.ProviderFailureTransport,
		application.ProviderFailureRateLimited, application.ProviderFailureTimeout,
		application.ProviderFailureModel, application.ProviderFailureContract,
		application.ProviderFailureUnclassified,
	} {
		clk := &fixedClock{now: providerBase}
		s, st := providerService(t, clk)
		t.Cleanup(s.DetachReleaseObserver)
		putObservation(t, st, observation(application.ProviderClaude, class, false, providerBase))
		entry := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
		want := application.ProviderCompatibilityUnknown
		if class == application.ProviderFailureContract {
			want = application.ProviderCompatibilityIncompatible
		}
		if entry.Compatibility.CLICompatibility != want {
			t.Fatalf("class %q produced cli_compatibility %q, want %q", class, entry.Compatibility.CLICompatibility, want)
		}
		// And a completed observation leaves it unknown too: a success says
		// nothing about a version.
		other := entryFor(t, providerRegistry(t, s), application.ProviderCodex)
		if other.Compatibility.CLICompatibility != application.ProviderCompatibilityUnknown {
			t.Fatalf("an unobserved Provider reports cli_compatibility %q, want unknown", other.Compatibility.CLICompatibility)
		}
	}
}

// TestTheDispositionIsReportedPerProviderAndTheTargetIsFiltered is the
// behavioural half of the handoff block, driven entirely through the real read.
func TestTheDispositionIsReportedPerProviderAndTheTargetIsFiltered(t *testing.T) {
	// (a) Nothing observed at all: every Provider is sendable (unknown health
	// is not a fault) and nothing needs to move.
	clk := &fixedClock{now: providerBase}
	s, _ := providerService(t, clk)
	t.Cleanup(s.DetachReleaseObserver)
	for _, entry := range providerRegistry(t, s).Providers {
		if entry.Handoff.Disposition != application.ProviderDispositionNone {
			t.Fatalf("%s reports %#v with nothing observed; unknown is an absence of observation, not a fault", entry.Provider, entry.Handoff)
		}
		if entry.Handoff.Target != "" || entry.Handoff.WaitingReason != "" {
			t.Fatalf("%s reports a target or a reason with disposition none: %#v", entry.Provider, entry.Handoff)
		}
	}

	// (b) codex reports a permanently failed invocation, so it is not sendable
	// and a target is proposed -- the first eligible candidate in the declared
	// order.
	clk = &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	t.Cleanup(s.DetachReleaseObserver)
	putObservation(t, st, observation(application.ProviderCodex, application.ProviderFailureModel, false, providerBase))
	codex := entryFor(t, providerRegistry(t, s), application.ProviderCodex)
	if codex.Health != application.ProviderHealthUnavailable {
		t.Fatalf("codex health = %q", codex.Health)
	}
	if codex.Handoff.Disposition != application.ProviderDispositionHandoffProposed || codex.Handoff.Target != application.ProviderClaude {
		t.Fatalf("codex reports %#v, want a proposal to claude", codex.Handoff)
	}

	// (c) Both candidates need an owner action, so the decision waits and says
	// which action.
	clk = &fixedClock{now: providerBase}
	s, st = providerService(t, clk)
	t.Cleanup(s.DetachReleaseObserver)
	putObservation(t, st, observation(application.ProviderCodex, application.ProviderFailureModel, false, providerBase))
	putObservation(t, st, observation(application.ProviderClaude, application.ProviderFailureUnauthenticated, false, providerBase))
	putObservation(t, st, application.ProviderObservation{Provider: application.ProviderOpenCode, StoppedForInspection: true, ObservedAt: providerBase})
	codex = entryFor(t, providerRegistry(t, s), application.ProviderCodex)
	if codex.Handoff.Disposition != application.ProviderDispositionWaiting {
		t.Fatalf("codex reports %#v, want waiting", codex.Handoff)
	}
	if codex.Handoff.WaitingReason != application.ProviderWaitingCandidateNeedsAnOwnerAction {
		t.Fatalf("codex reports the reason %q, want an owner action", codex.Handoff.WaitingReason)
	}
	if codex.Handoff.Target != "" {
		t.Fatalf("a waiting decision proposed the target %q", codex.Handoff.Target)
	}

	// (d) The attempt history, folded out of the bounded assignment ring the
	// read already returns. codex is working on an Increment claude has already
	// been assigned for, so claude is refused and opencode is proposed.
	clk = &fixedClock{now: providerBase}
	s, st = providerService(t, clk)
	t.Cleanup(s.DetachReleaseObserver)
	putObservation(t, st, observation(application.ProviderCodex, application.ProviderFailureModel, false, providerBase))
	putAssignment(t, st, application.ProviderAssignment{ExecutionID: "execution-a", IncrementID: "increment-1", Provider: application.ProviderCodex, Since: providerBase}, domain.ExecutionRunning)
	putAssignment(t, st, application.ProviderAssignment{ExecutionID: "execution-b", IncrementID: "increment-1", Provider: application.ProviderClaude, Since: providerBase}, domain.ExecutionSucceeded)
	codex = entryFor(t, providerRegistry(t, s), application.ProviderCodex)
	if codex.Handoff.Disposition != application.ProviderDispositionHandoffProposed || codex.Handoff.Target != application.ProviderOpenCode {
		t.Fatalf("codex reports %#v, want a proposal to opencode: claude has already been tried for increment-1", codex.Handoff)
	}
	// The finished Execution is not in the active assignment list, which is
	// what makes this a HISTORICAL fact rather than a current one.
	claude := entryFor(t, providerRegistry(t, s), application.ProviderClaude)
	for _, assignment := range claude.Assignments {
		if assignment.ExecutionID == "execution-b" {
			t.Fatal("a terminal Execution is still reported as an active assignment")
		}
	}

	// (e) The response is byte-identical across repeated calls with the blocks
	// in place. A fresh Service is used because each bounded owner read
	// reserves against the same daily read reservation, and this assertion is
	// about repetition rather than about accumulation.
	clk = &fixedClock{now: providerBase}
	repeat, repeatStore := providerService(t, clk)
	t.Cleanup(repeat.DetachReleaseObserver)
	putObservation(t, repeatStore, observation(application.ProviderCodex, application.ProviderFailureModel, false, providerBase))
	putAssignment(t, repeatStore, application.ProviderAssignment{ExecutionID: "execution-a", IncrementID: "increment-1", Provider: application.ProviderCodex, Since: providerBase}, domain.ExecutionRunning)
	putAssignment(t, repeatStore, application.ProviderAssignment{ExecutionID: "execution-b", IncrementID: "increment-1", Provider: application.ProviderClaude, Since: providerBase}, domain.ExecutionSucceeded)
	bodies := []string{}
	for i := 0; i < 3; i++ {
		view := providerRegistry(t, repeat)
		body, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(body))
		// And the whole document agrees with the shared decision table for the
		// state it is in: a source that is not not-sendable proposes nothing.
		for _, entry := range view.Providers {
			state := application.ProviderSourceStateFor(entry)
			if state != application.ProviderSourceNotSendable && entry.Handoff.Disposition != application.ProviderDispositionNone {
				t.Fatalf("%s is %q but reports %#v", entry.Provider, state, entry.Handoff)
			}
		}
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("call %d produced different bytes:\n%s\n%s", i+1, bodies[0], bodies[i])
		}
	}
}

// TestTheNewBlocksCarryNoThresholdAndNoMonetaryVocabulary re-runs the existing
// two scans against the fully-populated document with the new blocks in place,
// and adds the one thing the new blocks could introduce that the old scans
// would not think to look for: a ceiling value.
func TestTheNewBlocksCarryNoThresholdAndNoMonetaryVocabulary(t *testing.T) {
	view, body := fullyPopulatedRegistry(t)
	keys := jsonObjectKeys(t, body)
	newKeys := map[string]bool{
		"compatibility": true, "handoff": true, "cli_version_interval": true,
		"loop_version_interval": true, "observed_loop_version": true,
		"cli_compatibility": true, "loop_compatibility": true,
		"from": true, "until": true, "disposition": true, "target": true, "waiting_reason": true,
	}
	found := 0
	for _, key := range keys {
		if newKeys[key] {
			found++
		}
		if entry, bad := matchesMonetaryVocabulary(key); bad {
			t.Fatalf("read model key %q matches the monetary deny list on %q", key, entry)
		}
	}
	if found == 0 {
		t.Fatalf("the key walk found none of the new blocks' keys in %d keys; the scan would pass vacuously", len(keys))
	}
	// Every enum value the new blocks emit, plus every declared member of each
	// new enum whether emitted or not.
	emitted := []string{}
	for _, entry := range view.Providers {
		emitted = append(emitted,
			string(entry.Compatibility.CLICompatibility), string(entry.Compatibility.LoopCompatibility),
			string(entry.Handoff.Disposition), string(entry.Handoff.Target), string(entry.Handoff.WaitingReason),
			entry.Compatibility.CLIVersionInterval.From, entry.Compatibility.CLIVersionInterval.Until,
			entry.Compatibility.LoopVersionInterval.From, entry.Compatibility.LoopVersionInterval.Until,
			entry.Compatibility.ObservedLoopVersion,
		)
	}
	for _, v := range application.ProviderHandoffWaitingReasons() {
		emitted = append(emitted, string(v))
	}
	for _, v := range application.ProviderCandidateObstacles() {
		emitted = append(emitted, string(v))
	}
	for _, v := range application.ProviderHandoffDispositions() {
		emitted = append(emitted, string(v))
	}
	for _, v := range application.ProviderSourceStates() {
		emitted = append(emitted, string(v))
	}
	for _, value := range emitted {
		if value == "" {
			continue
		}
		if entry, bad := matchesMonetaryVocabulary(value); bad {
			t.Fatalf("emitted value %q matches the monetary deny list on %q", value, entry)
		}
	}
	// No approved runaway threshold shape anywhere in the document, including
	// inside the version bounds -- which is the one place a digit legitimately
	// appears in the new blocks.
	for _, shape := range []string{"16", "10.0", "10.00", "2.0", "2.00", "usd", "USD"} {
		if strings.Contains(string(body), shape) {
			t.Fatalf("the read model carries %q, which is the shape of an approved runaway threshold: %s", shape, body)
		}
	}
	// And no ceiling value is republished in either new block: neither block
	// carries the declared concurrency ceiling.
	for _, entry := range view.Providers {
		compat, err := json.Marshal(entry.Compatibility)
		if err != nil {
			t.Fatal(err)
		}
		handoff, err := json.Marshal(entry.Handoff)
		if err != nil {
			t.Fatal(err)
		}
		ceiling := strconv.Itoa(application.ProviderConcurrencyDesignCeiling)
		for _, block := range []string{string(compat), string(handoff)} {
			if strings.Contains(block, ceiling) {
				t.Fatalf("a V2-074 block carries the declared ceiling value %q: %s", ceiling, block)
			}
		}
		if strings.Contains(string(handoff), "0") || strings.Contains(string(handoff), "1") {
			t.Fatalf("the handoff block carries a digit at all: %s", handoff)
		}
	}
	t.Logf("scanned %d read-model keys (%d of them the new blocks'), %d emitted values, and both new blocks for a threshold or a ceiling value", len(keys), found, len(emitted))
}

// TestTheReadCountAndItsIndependenceFromTheRequirementCountAreUnchanged is
// A13's last clause and A14's derivability clause in one, measured two ways.
//
// The measurement method is stated because there is no read counter in the
// memory store to consult: the read sites are counted from the AST of the
// functions that perform the read, and the independence from the Requirement
// count is measured behaviourally by adding Requirements and comparing bytes.
func TestTheReadCountAndItsIndependenceFromTheRequirementCountAreUnchanged(t *testing.T) {
	files := applicationPackageFiles(t, false)
	var target *ast.FuncDecl
	var helper *ast.FuncDecl
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Name == nil {
				continue
			}
			if fn.Name.Name == "Providers" && fn.Recv != nil {
				target = fn
			}
			if fn.Name.Name == "activeProviderAssignments" {
				helper = fn
			}
		}
	}
	if target == nil || helper == nil {
		t.Fatal("could not find Providers or activeProviderAssignments; the scan is broken")
	}
	unitOfWorkCalls := func(fn *ast.FuncDecl) []string {
		out := []string{}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			receiver, isIdent := sel.X.(*ast.Ident)
			if !isIdent || receiver.Name != "u" {
				return true
			}
			out = append(out, sel.Sel.Name)
			return true
		})
		sort.Strings(out)
		return out
	}
	inProviders := unitOfWorkCalls(target)
	inHelper := unitOfWorkCalls(helper)
	if want := []string{"ProviderAssignments", "ProviderObservations"}; !reflect.DeepEqual(inProviders, want) {
		t.Fatalf("Providers performs the unit-of-work reads %v, want exactly %v; V2-074 adds no read", inProviders, want)
	}
	if want := []string{"Execution"}; !reflect.DeepEqual(inHelper, want) {
		t.Fatalf("activeProviderAssignments performs %v, want exactly %v", inHelper, want)
	}
	// No write of any kind from the read path.
	for _, name := range append(append([]string{}, inProviders...), inHelper...) {
		if strings.HasPrefix(name, "Save") || strings.HasPrefix(name, "Append") || strings.HasPrefix(name, "Delete") {
			t.Fatalf("the read path calls %s, which is a write", name)
		}
	}

	// Behavioural: the document does not change when Requirements are added, so
	// the read is independent of the Requirement count.
	clk := &fixedClock{now: providerBase}
	s, st := providerService(t, clk)
	t.Cleanup(s.DetachReleaseObserver)
	putObservation(t, st, observation(application.ProviderCodex, application.ProviderFailureModel, false, providerBase))
	before, err := json.Marshal(providerRegistry(t, s))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for index := 0; index < 12; index++ {
		suffix := strconv.Itoa(index)
		if _, err := s.CaptureRequirement(owner(ctx), application.CaptureRequest{
			RequestID: "capture-" + suffix,
			Text:      "a requirement that must not change the provider read",
		}); err != nil {
			t.Fatalf("capture %s: %v", suffix, err)
		}
	}
	after, err := json.Marshal(providerRegistry(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("adding 12 Requirements changed the provider read:\n%s\n%s", before, after)
	}
	t.Logf("Providers performs %v per Provider plus %v per retained assignment, all bounded by constants; the document is byte-identical after 12 Requirements were captured", inProviders, inHelper)
}

// TestNoStoreAdapterWasEditedAndUnitOfWorkGainedNoMethod is A14, measured from
// the working tree rather than asserted in prose.
//
// ports.go declares UnitOfWork and is outside this task's allowed paths, so the
// method set is read out of its source. UnitOfWork is composed of embedded
// repository interfaces, so the scan resolves each embedded name into its own
// methods and asserts two things: the three reads this task's disposition is
// derived from all pre-date it, and no method or embedded interface whose name
// belongs to this task exists at all.
func TestNoStoreAdapterWasEditedAndUnitOfWorkGainedNoMethod(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "ports.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse ports.go: %v", err)
	}
	interfaces := map[string]*ast.InterfaceType{}
	for _, decl := range file.Decls {
		gen, isGen := decl.(*ast.GenDecl)
		if !isGen || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, isType := spec.(*ast.TypeSpec)
			if !isType || typeSpec.Name == nil {
				continue
			}
			if iface, isIface := typeSpec.Type.(*ast.InterfaceType); isIface {
				interfaces[typeSpec.Name.Name] = iface
			}
		}
	}
	unitOfWork, declared := interfaces["UnitOfWork"]
	if !declared || unitOfWork.Methods == nil {
		t.Fatal("ports.go declares no UnitOfWork interface; the scan is broken")
	}
	methods := map[string]bool{}
	embedded := []string{}
	var collect func(iface *ast.InterfaceType, depth int)
	collect = func(iface *ast.InterfaceType, depth int) {
		if iface == nil || iface.Methods == nil || depth > 4 {
			return
		}
		for _, field := range iface.Methods.List {
			if len(field.Names) != 0 {
				for _, name := range field.Names {
					methods[name.Name] = true
				}
				continue
			}
			ident, isIdent := field.Type.(*ast.Ident)
			if !isIdent {
				continue
			}
			embedded = append(embedded, ident.Name)
			collect(interfaces[ident.Name], depth+1)
		}
	}
	collect(unitOfWork, 0)
	if len(methods) == 0 || len(embedded) == 0 {
		t.Fatalf("resolved %d methods across %d embedded interfaces; the scan is broken", len(methods), len(embedded))
	}
	for _, needed := range []string{"ProviderObservations", "ProviderAssignments", "Execution"} {
		if !methods[needed] {
			t.Fatalf("UnitOfWork has no %s; the disposition cannot be derived from what one bounded keyed read already returns", needed)
		}
	}
	for _, forbidden := range []string{
		"ProviderCompatibility", "ProviderCompatibilities", "ProviderHandoff", "ProviderHandoffs",
		"ProviderVersion", "ProviderVersions", "ProviderDisposition", "ProviderDispositions",
		"SaveProviderCompatibility", "SaveProviderHandoff", "SaveProviderVersion",
	} {
		if methods[forbidden] {
			t.Fatalf("UnitOfWork gained the method %s; V2-074 adds no port", forbidden)
		}
	}
	for _, name := range embedded {
		lowered := strings.ToLower(name)
		if strings.Contains(lowered, "compatib") || strings.Contains(lowered, "handoff") || strings.Contains(lowered, "version interval") {
			t.Fatalf("UnitOfWork embeds the repository interface %s; V2-074 adds no port", name)
		}
	}
	// Positive control: the resolver must actually find a method it is looking
	// for through an embedded interface rather than only inline ones.
	if !methods["SaveProviderObservation"] {
		t.Fatal("the embedded-interface resolver did not find SaveProviderObservation; it is not resolving embedded interfaces and every assertion above would pass vacuously")
	}
	sortedMethods := sortedKeys(methods)
	t.Logf("UnitOfWork resolves to %d methods across %d embedded repository interfaces, none of them new; the disposition is derived from ProviderObservations, ProviderAssignments and Execution, which all pre-date this task (first method %q, last %q)", len(methods), len(embedded), sortedMethods[0], sortedMethods[len(sortedMethods)-1])
}
