package contracts_test

// The deferral partition, derived from the declaration file by a test rather
// than asserted in prose (wo-v2-095 A4, dp-v2-095 d4).
//
// Section 8.3 says a deferral is "not a judgement but a measurable fact
// derivable from the capability's declaration". If that is true then it
// belongs in a test, and if it is in a test then no gate ever has to take a
// live record's word for it (G6a: the classification is re-executed at every
// judging commit).
//
// Two grades of deferral exist and both are read out of
// external_dependencies: a capability declaring "Google Cloud Run" among its
// systems belongs to the initial deploy gate D1, and a capability declaring
// BOTH "codex" and "opencode" among its providers belongs to M6, because only
// claude is authenticated on the machine any preview-local observation runs
// on. Everything else is M5-owned and may never be deferred.
//
// THE REVERSE CONSTRAINT IS IN THE SAME TABLE ON PURPOSE. Asserting only the
// three memberships would leave the set growable: a later task could move a
// product gap onto the deferred side and the assertion about the first two
// sets would still hold. Asserting that no member of the M5-owned five
// declares Cloud Run, codex or opencode makes an eighth deferral require
// either an edit to contracts/release-contract/foundation-capabilities.json
// -- prohibited in the task that would want it -- or a red test.
//
// WHAT THIS TEST DELIBERATELY DOES NOT READ: the machine's authentication
// state. Whether codex and opencode are logged in here is not deterministic
// and is not a property of the declaration; it is re-measured at observation
// time and recorded in the live record. Reading it here would make `make
// check` depend on a credential store.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// capabilityDeclarationFile is the decoded shape this partition needs.
type capabilityDeclarationFile struct {
	Capabilities []capabilityDeclaration `json:"capabilities"`
}

type capabilityDeclaration struct {
	ID                   string `json:"id"`
	ExternalDependencies struct {
		Systems   []string `json:"systems"`
		Providers []string `json:"providers"`
	} `json:"external_dependencies"`
}

// The two literals the deferral grades are derived from. They are declared
// once and read from one place, so a typo cannot make a predicate vacuously
// true in one direction and false in the other.
const (
	deferralSystemCloudRun = "Google Cloud Run"
	deferralProviderCodex  = "codex"
	deferralProviderOpen   = "opencode"
)

func capabilityDeclarationRead(t *testing.T, root string) capabilityDeclarationFile {
	t.Helper()
	path := filepath.Join(root, "contracts", "release-contract", "foundation-capabilities.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded capabilityDeclarationFile
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Capabilities) == 0 {
		t.Fatalf("%s declares no capabilities; the partition below would pass vacuously", path)
	}
	return decoded
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// capabilityDeferralPartition is the whole classification as a pure function
// over decoded input, so the negative control can drive it with a synthetic
// declaration set and no tree at all.
//
// It returns three sorted id sets: the D1 grade (declares Cloud Run), the M6
// grade (declares both codex and opencode), and the M5-owned remainder.
func capabilityDeferralPartition(decoded capabilityDeclarationFile) (cloudRun, providers, owned []string) {
	for _, c := range decoded.Capabilities {
		switch {
		case containsString(c.ExternalDependencies.Systems, deferralSystemCloudRun):
			cloudRun = append(cloudRun, c.ID)
		case containsString(c.ExternalDependencies.Providers, deferralProviderCodex) &&
			containsString(c.ExternalDependencies.Providers, deferralProviderOpen):
			providers = append(providers, c.ID)
		default:
			owned = append(owned, c.ID)
		}
	}
	sort.Strings(cloudRun)
	sort.Strings(providers)
	sort.Strings(owned)
	return cloudRun, providers, owned
}

// capabilityDeferralReverseViolations is the REVERSE direction, computed
// independently of the partition above rather than derived from it: for every
// id in owned, it re-reads that capability's declaration and complains if it
// declares any of the three deferral-bearing literals. Deriving it from the
// partition would make it a tautology.
func capabilityDeferralReverseViolations(decoded capabilityDeclarationFile, owned []string) []string {
	inOwned := map[string]bool{}
	for _, id := range owned {
		inOwned[id] = true
	}
	var violations []string
	for _, c := range decoded.Capabilities {
		if !inOwned[c.ID] {
			continue
		}
		if containsString(c.ExternalDependencies.Systems, deferralSystemCloudRun) {
			violations = append(violations, c.ID+" is in the M5-owned set but declares "+deferralSystemCloudRun)
		}
		if containsString(c.ExternalDependencies.Providers, deferralProviderCodex) {
			violations = append(violations, c.ID+" is in the M5-owned set but declares provider "+deferralProviderCodex)
		}
		if containsString(c.ExternalDependencies.Providers, deferralProviderOpen) {
			violations = append(violations, c.ID+" is in the M5-owned set but declares provider "+deferralProviderOpen)
		}
	}
	sort.Strings(violations)
	return violations
}

func equalStringSets(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	a := append([]string(nil), got...)
	b := append([]string(nil), want...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCapabilityDeclarationPartition is the real-tree assertion: the three
// sets BY NAME, their disjointness, the reverse constraint, and the sum
// against the declared capability count.
func TestCapabilityDeclarationPartition(t *testing.T) {
	root := filepath.Join("..", "..")
	decoded := capabilityDeclarationRead(t, root)
	cloudRun, providers, owned := capabilityDeferralPartition(decoded)

	wantCloudRun := []string{"cap-loop-control", "cap-loop-self-update", "cap-preview-operation", "cap-stable-promotion"}
	wantProviders := []string{"cap-autonomous-resolution", "cap-provider-operation", "cap-shared-resource-allocation"}
	wantOwned := []string{"cap-backlog-visibility", "cap-human-input-request", "cap-repository-registration", "cap-requirement-intake", "cap-user-documentation"}

	if !equalStringSets(cloudRun, wantCloudRun) {
		t.Errorf("the set declaring %q is %v, want exactly %v", deferralSystemCloudRun, cloudRun, wantCloudRun)
	}
	if !equalStringSets(providers, wantProviders) {
		t.Errorf("the set declaring both %q and %q is %v, want exactly %v", deferralProviderCodex, deferralProviderOpen, providers, wantProviders)
	}
	if !equalStringSets(owned, wantOwned) {
		t.Errorf("the remaining (M5-owned) set is %v, want exactly %v", owned, wantOwned)
	}

	// Disjointness, asserted rather than inferred from the switch: a
	// capability declaring Cloud Run AND both providers would be classified
	// into the first arm silently, so the overlap is measured directly.
	both := 0
	for _, c := range decoded.Capabilities {
		if containsString(c.ExternalDependencies.Systems, deferralSystemCloudRun) &&
			containsString(c.ExternalDependencies.Providers, deferralProviderCodex) &&
			containsString(c.ExternalDependencies.Providers, deferralProviderOpen) {
			both++
			t.Errorf("%s declares both %q and the provider pair; the two deferral grades are not disjoint", c.ID, deferralSystemCloudRun)
		}
	}
	if both != 0 {
		t.Errorf("the two deferral grades overlap on %d capabilities", both)
	}

	if violations := capabilityDeferralReverseViolations(decoded, owned); len(violations) != 0 {
		for _, v := range violations {
			t.Errorf("reverse constraint: %s", v)
		}
	}

	if got := len(cloudRun) + len(providers) + len(owned); got != len(decoded.Capabilities) {
		t.Fatalf("the three sets sum to %d but %d capabilities are declared", got, len(decoded.Capabilities))
	}
	t.Logf("partition measured: %d declare %q (D1), %d declare both %q and %q (M6), %d declare neither (M5-owned); %d+%d+%d=%d",
		len(cloudRun), deferralSystemCloudRun, len(providers), deferralProviderCodex, deferralProviderOpen,
		len(owned), len(cloudRun), len(providers), len(owned), len(decoded.Capabilities))
}

// TestCapabilityDeclarationPartitionNegativeControl proves each assertion of
// the partition can fail, over synthetic decoded input and no tree.
func TestCapabilityDeclarationPartitionNegativeControl(t *testing.T) {
	decl := func(id string, systems, providers []string) capabilityDeclaration {
		var c capabilityDeclaration
		c.ID = id
		c.ExternalDependencies.Systems = systems
		c.ExternalDependencies.Providers = providers
		return c
	}

	// (a) a capability moved from the M5-owned set onto the Cloud Run side is
	// detected by the membership assertion.
	moved := capabilityDeclarationFile{Capabilities: []capabilityDeclaration{
		decl("cap-alpha", []string{"owner UI"}, nil),
		decl("cap-beta", []string{"owner UI", deferralSystemCloudRun}, nil),
	}}
	cloudRun, providers, owned := capabilityDeferralPartition(moved)
	if !equalStringSets(cloudRun, []string{"cap-beta"}) || !equalStringSets(owned, []string{"cap-alpha"}) || len(providers) != 0 {
		t.Fatalf("the partition did not classify the synthetic move: cloudRun=%v providers=%v owned=%v", cloudRun, providers, owned)
	}

	// (b) declaring only ONE of the two providers is NOT the M6 grade. This
	// is what stops a capability declaring claude alone from being deferred.
	oneProvider := capabilityDeclarationFile{Capabilities: []capabilityDeclaration{
		decl("cap-alpha", []string{"owner UI"}, []string{deferralProviderCodex}),
	}}
	_, providers, owned = capabilityDeferralPartition(oneProvider)
	if len(providers) != 0 || !equalStringSets(owned, []string{"cap-alpha"}) {
		t.Fatalf("declaring only %q was treated as the provider deferral grade: providers=%v owned=%v", deferralProviderCodex, providers, owned)
	}

	// (c) the REVERSE constraint fires when an id is claimed as M5-owned
	// while its declaration carries a deferral-bearing literal. The owned set
	// is supplied by the caller here precisely so the reverse rule is driven
	// independently of the partition that would never have produced it.
	claimed := capabilityDeclarationFile{Capabilities: []capabilityDeclaration{
		decl("cap-alpha", []string{"owner UI", deferralSystemCloudRun}, []string{deferralProviderOpen}),
	}}
	violations := capabilityDeferralReverseViolations(claimed, []string{"cap-alpha"})
	if len(violations) != 2 {
		t.Fatalf("the reverse constraint reported %d violations, want exactly 2 (one system, one provider): %v", len(violations), violations)
	}

	// (d) positive half: a genuinely M5-owned declaration yields no violation,
	// so the reverse rule is not simply always failing.
	clean := capabilityDeclarationFile{Capabilities: []capabilityDeclaration{
		decl("cap-alpha", []string{"owner UI", "Firestore"}, []string{"claude"}),
	}}
	if violations := capabilityDeferralReverseViolations(clean, []string{"cap-alpha"}); len(violations) != 0 {
		t.Fatalf("the reverse constraint refused a declaration that names neither deferral grade: %v", violations)
	}

	// (e) the sum assertion can fail: a declaration with an empty id still
	// counts, so a partition that silently dropped one would be caught.
	dropped := capabilityDeclarationFile{Capabilities: []capabilityDeclaration{
		decl("cap-alpha", []string{"owner UI"}, nil),
		decl("cap-beta", []string{deferralSystemCloudRun}, nil),
		decl("cap-gamma", nil, []string{deferralProviderCodex, deferralProviderOpen}),
	}}
	c2, p2, o2 := capabilityDeferralPartition(dropped)
	if len(c2)+len(p2)+len(o2) != len(dropped.Capabilities) {
		t.Fatalf("the partition dropped a synthetic declaration: %d+%d+%d != %d", len(c2), len(p2), len(o2), len(dropped.Capabilities))
	}
}
