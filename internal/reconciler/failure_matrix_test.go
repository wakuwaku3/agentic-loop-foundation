package reconciler

import (
	"reflect"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// wantDocumentedFailureClassCount is acceptance A13 Table 1's named
// constant: the exact number of rows in docs/architecture/failure-model.md
// section 2's failure taxonomy, measured by counting its table rows.
const wantDocumentedFailureClassCount = 17

// wantInjectionScenarioCount is acceptance A13 Table 2's named constant: the
// exact number of bullet items in docs/architecture/failure-model.md
// section 10's failure-injection list.
const wantInjectionScenarioCount = 12

// failureClassRow is one row of Table 1. Convergence must be linked to
// exactly one Go test function that observes it, in exactly one of this
// task's own tests (convergesHere) or an existing test from prior work
// (convergedElsewhere) -- never both, and a deferred row must instead name
// the owning task and milestone verbatim.
type failureClassRow struct {
	class      string
	bucket     string // "convergesHere", "convergedElsewhere", or "deferred"
	observable string // one sentence: what observably converges, or empty for deferred
	test       string // the Go test function name that carries the claim, or empty for deferred
	owner      string // "task Mxx" for deferred rows, empty otherwise
}

// documentedFailureClasses is docs/architecture/failure-model.md section 2,
// row for row, partitioned exactly as dp-v2-019 d8 specifies.
var documentedFailureClasses = []failureClassRow{
	{class: "invalid-input", bucket: "convergedElsewhere", test: "TestForbiddenIncrementTransitionsClosure (internal/domain)"},
	{class: "policy-denied", bucket: "convergesHere", observable: "A denied Permit produces no side effect: no Requirement/Lease/Execution mutation and no delivered outbox effect for the denied cell.", test: "TestStopModeByKindMatrix"},
	{class: "capacity-unavailable", bucket: "convergedElsewhere", test: "TestReserveRefusesOverBudgetWorstCaseBeforeStaging (internal/quota)"},
	{class: "provider-transport", bucket: "convergedElsewhere", test: "TestSyntheticAndFakeProvider (internal/runner)"},
	{class: "provider-model", bucket: "deferred", owner: "V2-027"},
	{class: "provider-quota", bucket: "deferred", owner: "V2-027 (live probe V2-028)"},
	{class: "execution-lost", bucket: "convergesHere", observable: "A Lease past its TTL is expired and its Execution reaches ExecutionLost with its Increment returned to ready, whether the Lease was still active-but-expired or already moved past active.", test: "TestTickExpiresLeaseAndFencesLostExecution / TestOrphanSweepRecoversExecutionBehindAlreadyExpiredLease"},
	{class: "progress-stalled", bucket: "convergesHere", observable: "A poison Lease whose Execution already reached a terminal state before the TTL elapsed is closed (not re-processed forever), and the next candidate still converges in the same pass.", test: "TestTickClosesTerminalExecutionLeaseAndRecoversNextCandidate"},
	{class: "verification-failed", bucket: "deferred", owner: "V2-030"},
	{class: "external-ambiguous", bucket: "convergesHere", observable: "An ambiguous outbox item consults the local record before any external read, and each of Confirmed/NotObserved/unknown resolves to exactly one of Confirmed/retried-under-the-same-key/needs-input.", test: "TestOutboxAmbiguousRequiresObservationBeforeAnyRedelivery / TestOutboxAmbiguousConsultsLocalRecordBeforeReobserving"},
	{class: "integration-conflict", bucket: "deferred", owner: "V2-022"},
	{class: "preview-regression", bucket: "deferred", owner: "V2-022"},
	{class: "promotion-partial", bucket: "deferred", owner: "V2-022"},
	{class: "secret-suspected", bucket: "deferred", owner: "V2-017"},
	{class: "budget-exceeded", bucket: "convergedElsewhere", test: "TestDailyBudgetFailsClosedExactlyOneOverTheLine (internal/quota)"},
	{class: "contract-incompatible", bucket: "deferred", owner: "V2-034"},
	{class: "unknown", bucket: "convergesHere", observable: "A mixed reachable/unreachable target set, or a target missing past its deadline, never verifies: VerifyControl fails closed to Pending or BlockedUnreachable rather than guessing.", test: "TestVerifyControlFailsClosedForMixedReachabilityAndAmbiguity"},
}

// injectionScenarioRow is one row of Table 2.
type injectionScenarioRow struct {
	scenario string
	bucket   string // "injectedHere", "injectedElsewhere", or "deferred"
	test     string
	owner    string
}

// injectionScenarios is docs/architecture/failure-model.md section 10, bullet
// for bullet, partitioned exactly as dp-v2-019 d8 specifies.
var injectionScenarios = []injectionScenarioRow{
	{scenario: "Runner stop immediately before/after a Control Plane request", bucket: "injectedHere", test: "TestControlAgentClosesVerificationLoopPositiveAndNegativeControl"},
	{scenario: "Lease renewal stopped and an old Result submitted", bucket: "injectedHere", test: "TestLeaseKeeperStopsRenewingOnceLeaseIsExpiredWithoutResurrectingIt (internal/runner)"},
	{scenario: "Provider CLI non-zero exit, zero-exit error envelope, malformed JSON, empty result", bucket: "injectedElsewhere", test: "TestSyntheticAndFakeProvider (internal/runner)"},
	{scenario: "Provider quota exhaustion and recovery probe", bucket: "deferred", owner: "V2-028"},
	{scenario: "Source Control write timeout resolving to success, to not-executed, and to indistinguishable", bucket: "injectedHere", test: "TestOutboxAmbiguousRequiresObservationBeforeAnyRedelivery / TestOutboxAmbiguousConsultsLocalRecordBeforeReobserving"},
	{scenario: "validation process hang", bucket: "injectedHere", test: "TestVerifyControlFailsClosedForMixedReachabilityAndAmbiguity"},
	{scenario: "Preview deploy partial failure", bucket: "deferred", owner: "V2-022"},
	{scenario: "docs-routing-only failure", bucket: "deferred", owner: "V2-022"},
	{scenario: "schema migration with an old Stable restart", bucket: "deferred", owner: "V2-034"},
	{scenario: "new claim and promotion attempted during emergency stop", bucket: "injectedHere", test: "TestStopModeByKindMatrix"},
	{scenario: "secret-like fixture in commit, log, Provider outbound", bucket: "injectedElsewhere", test: "TestSecretBrokerCredentialNeverLeaksExceptIntoOneInvocationEnvironment (internal/runner)"},
	{scenario: "two-Repository capacity starvation", bucket: "deferred", owner: "V2-030"},
}

// TestFailureTaxonomyIsFinitelyEnumeratedAndPartitioned is acceptance A13
// Table 1 and acceptance A14's drift record.
func TestFailureTaxonomyIsFinitelyEnumeratedAndPartitioned(t *testing.T) {
	if len(documentedFailureClasses) != wantDocumentedFailureClassCount {
		t.Fatalf("documentedFailureClasses has %d rows, want %d", len(documentedFailureClasses), wantDocumentedFailureClassCount)
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, row := range documentedFailureClasses {
		if seen[row.class] {
			t.Fatalf("class %q listed twice", row.class)
		}
		seen[row.class] = true
		switch row.bucket {
		case "convergesHere":
			if row.observable == "" || row.test == "" {
				t.Fatalf("class %q is convergesHere but missing an observable sentence or a linked test", row.class)
			}
		case "convergedElsewhere":
			if row.test == "" {
				t.Fatalf("class %q is convergedElsewhere but names no existing test", row.class)
			}
		case "deferred":
			if row.owner == "" {
				t.Fatalf("class %q is deferred but names no owning task", row.class)
			}
		default:
			t.Fatalf("class %q has unknown bucket %q", row.class, row.bucket)
		}
		counts[row.bucket]++
	}
	if counts["convergesHere"] != 5 {
		t.Fatalf("convergesHere=%d, want 5", counts["convergesHere"])
	}
	if counts["convergedElsewhere"] != 4 {
		t.Fatalf("convergedElsewhere=%d, want 4", counts["convergedElsewhere"])
	}
	if counts["deferred"] != 8 {
		t.Fatalf("deferred=%d, want 8", counts["deferred"])
	}
	if sum := counts["convergesHere"] + counts["convergedElsewhere"] + counts["deferred"]; sum != wantDocumentedFailureClassCount {
		t.Fatalf("bucket sum=%d, want %d (buckets must be disjoint and exhaustive)", sum, wantDocumentedFailureClassCount)
	}

	// V2-021 is complete and must never be named as a deferred owner (A13).
	for _, row := range documentedFailureClasses {
		if row.bucket == "deferred" && (row.owner == "V2-021" || row.owner == "task V2-021") {
			t.Fatalf("class %q defers to V2-021, which is complete: a deferral to nobody", row.class)
		}
	}

	// Acceptance A13's fixed owner table, verbatim.
	wantOwners := map[string]string{
		"provider-model":        "V2-027",
		"provider-quota":        "V2-027 (live probe V2-028)",
		"verification-failed":   "V2-030",
		"integration-conflict":  "V2-022",
		"preview-regression":    "V2-022",
		"promotion-partial":     "V2-022",
		"secret-suspected":      "V2-017",
		"contract-incompatible": "V2-034",
	}
	for _, row := range documentedFailureClasses {
		if row.bucket != "deferred" {
			continue
		}
		want, ok := wantOwners[row.class]
		if !ok {
			t.Fatalf("class %q is deferred but has no fixed-owner entry in this test", row.class)
		}
		if row.owner != want {
			t.Fatalf("class %q owner=%q, want %q", row.class, row.owner, want)
		}
	}

	// Acceptance A3: domain.FailureClass declares exactly 17 constants, one
	// for every documented class in Table 1, so this test now witnesses
	// agreement between the failure model document and internal/domain
	// instead of the drift that V2-019 recorded (14 constants, 3 missing).
	domainDeclared := map[domain.FailureClass]bool{
		domain.FailureInvalidInput: true, domain.FailurePolicyDenied: true, domain.FailureCapacity: true,
		domain.FailureProviderTransport: true, domain.FailureProviderModel: true, domain.FailureProviderQuota: true,
		domain.FailureExecutionLost: true, domain.FailureProgressStalled: true, domain.FailureVerification: true,
		domain.FailureExternalAmbiguous: true, domain.FailureIntegration: true, domain.FailurePreviewRegression: true,
		domain.FailurePromotionPartial: true, domain.FailureSecretSuspected: true,
		domain.FailureBudgetExceeded: true, domain.FailureContractIncompat: true, domain.FailureUnknown: true,
	}
	if len(domainDeclared) != wantDocumentedFailureClassCount {
		t.Fatalf("domain.FailureClass constants referenced here=%d, want %d", len(domainDeclared), wantDocumentedFailureClassCount)
	}
	var missingFromDomain []string
	for _, row := range documentedFailureClasses {
		if !domainDeclared[domain.FailureClass(row.class)] {
			missingFromDomain = append(missingFromDomain, row.class)
		}
	}
	if len(missingFromDomain) != 0 {
		t.Fatalf("classes missing a domain.FailureClass constant=%v, want none: every documented class must have an owning domain constant", missingFromDomain)
	}

	// Acceptance A4: internal/provider declares its own, separate
	// FailureClass type (values including "contract-incompatible" and
	// "timeout"). Assert it stays a distinct Go type from
	// domain.FailureClass -- a different taxonomy serving the provider
	// boundary, not a duplicate merged into the domain one -- even though
	// it shares the "contract-incompatible" string value with domain.
	if string(provider.FailureContract) != "contract-incompatible" {
		t.Fatalf("provider.FailureContract=%q, want %q", provider.FailureContract, "contract-incompatible")
	}
	if string(provider.FailureTimeout) != "timeout" {
		t.Fatalf("provider.FailureTimeout=%q, want %q", provider.FailureTimeout, "timeout")
	}
	domainType := reflect.TypeOf(domain.FailureContractIncompat)
	providerType := reflect.TypeOf(provider.FailureContract)
	if domainType == providerType {
		t.Fatalf("domain.FailureClass and provider.FailureClass must remain distinct types (got the same reflect.Type %v), not unified into one taxonomy", domainType)
	}
}

func indexOfClass(t *testing.T, class string) int {
	t.Helper()
	for i, row := range documentedFailureClasses {
		if row.class == class {
			return i
		}
	}
	t.Fatalf("class %q not found", class)
	return -1
}

// TestInjectionScenariosAreFinitelyEnumeratedAndPartitioned is acceptance
// A13 Table 2.
func TestInjectionScenariosAreFinitelyEnumeratedAndPartitioned(t *testing.T) {
	if len(injectionScenarios) != wantInjectionScenarioCount {
		t.Fatalf("injectionScenarios has %d rows, want %d", len(injectionScenarios), wantInjectionScenarioCount)
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, row := range injectionScenarios {
		if seen[row.scenario] {
			t.Fatalf("scenario %q listed twice", row.scenario)
		}
		seen[row.scenario] = true
		switch row.bucket {
		case "injectedHere", "injectedElsewhere":
			if row.test == "" {
				t.Fatalf("scenario %q is %s but names no test", row.scenario, row.bucket)
			}
		case "deferred":
			if row.owner == "" {
				t.Fatalf("scenario %q is deferred but names no owning task", row.scenario)
			}
		default:
			t.Fatalf("scenario %q has unknown bucket %q", row.scenario, row.bucket)
		}
		counts[row.bucket]++
	}
	if counts["injectedHere"] != 5 {
		t.Fatalf("injectedHere=%d, want 5", counts["injectedHere"])
	}
	if counts["injectedElsewhere"] != 2 {
		t.Fatalf("injectedElsewhere=%d, want 2", counts["injectedElsewhere"])
	}
	if counts["deferred"] != 5 {
		t.Fatalf("deferred=%d, want 5", counts["deferred"])
	}
	if sum := counts["injectedHere"] + counts["injectedElsewhere"] + counts["deferred"]; sum != wantInjectionScenarioCount {
		t.Fatalf("bucket sum=%d, want %d", sum, wantInjectionScenarioCount)
	}

	wantOwners := map[string]string{
		"Provider quota exhaustion and recovery probe": "V2-028",
		"Preview deploy partial failure":               "V2-022",
		"docs-routing-only failure":                    "V2-022",
		"schema migration with an old Stable restart":  "V2-034",
		"two-Repository capacity starvation":           "V2-030",
	}
	for _, row := range injectionScenarios {
		if row.bucket != "deferred" {
			continue
		}
		want, ok := wantOwners[row.scenario]
		if !ok {
			t.Fatalf("scenario %q is deferred but has no fixed-owner entry in this test", row.scenario)
		}
		if row.owner != want {
			t.Fatalf("scenario %q owner=%q, want %q", row.scenario, row.owner, want)
		}
	}
}
