package provider_test

// Provider-neutral handoff conversion (V2-027 A14).

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// richPacket carries every field the content digest covers, so a test that
// drops one has something to drop.
func richPacket() provider.WorkPacket {
	return provider.WorkPacket{
		Version:            provider.ContractVersion,
		RequirementID:      "req-9",
		RequirementSummary: "carry the increment across a provider change",
		Repository:         "agentic-loop-foundation",
		IncrementID:        "inc-9",
		Constraints:        []string{"bounded", "no fixed sleep"},
		Decisions: []provider.Decision{
			{Kind: "scope", Detail: "fixture layer only"},
			{Kind: "ordering", Detail: "after V2-067"},
		},
		Artifacts: []provider.Artifact{
			{Name: "internal/provider/pool.go", Digest: "sha256:" + "a1" + "00000000000000000000000000000000000000000000000000000000000000"[:62]},
			{Name: "internal/provider/breaker.go", Digest: "sha256:" + "b2" + "00000000000000000000000000000000000000000000000000000000000000"[:62]},
		},
		Verification: []provider.Verification{
			{Name: "go test ./internal/provider", Status: "passed"},
			{Name: "make check", Status: "passed", EvidenceDigest: "sha256:" + "c3" + "00000000000000000000000000000000000000000000000000000000000000"[:62]},
		},
		Unresolved: []string{"three real CLIs are still unobserved"},
	}
}

func orderedPairs() [][2]string {
	var pairs [][2]string
	for _, from := range provider.PoolNames() {
		for _, to := range provider.PoolNames() {
			if from != to {
				pairs = append(pairs, [2]string{from, to})
			}
		}
	}
	return pairs
}

func TestAllSixOrderedPairsPreserveThePacketFieldByField(t *testing.T) {
	pairs := orderedPairs()
	if len(pairs) != 6 {
		t.Fatalf("ordered pairs = %d, want 6", len(pairs))
	}
	breaker := newBreaker(t)
	for _, pair := range pairs {
		from, to := pair[0], pair[1]
		source := richPacket()
		handoff, err := provider.PrepareHandoff(from, to, source, provider.Result{
			Provider:     from,
			Failure:      &provider.Failure{Class: provider.FailureQuota},
			Usage:        provider.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7},
			OutputDigest: provider.DigestOutput("the source provider's own output"),
		})
		if err != nil {
			t.Fatalf("%s->%s: PrepareHandoff: %v", from, to, err)
		}
		if err := handoff.Validate(); err != nil {
			t.Fatalf("%s->%s: Validate: %v", from, to, err)
		}
		request, err := provider.RequestFromHandoff(handoff, "op-"+from+"-"+to, "/workspace", breaker)
		if err != nil {
			t.Fatalf("%s->%s: RequestFromHandoff: %v", from, to, err)
		}
		got := request.Packet
		if !reflect.DeepEqual(got, source) {
			t.Fatalf("%s->%s: packet changed across the conversion:\n got %#v\nwant %#v", from, to, got, source)
		}
		// Field by field, and by name, so a field added to WorkPacket later
		// is not silently unchecked.
		packetType := reflect.TypeOf(source)
		for i := 0; i < packetType.NumField(); i++ {
			name := packetType.Field(i).Name
			sourceValue := reflect.ValueOf(source).Field(i).Interface()
			gotValue := reflect.ValueOf(got).Field(i).Interface()
			if !reflect.DeepEqual(sourceValue, gotValue) {
				t.Fatalf("%s->%s: WorkPacket.%s = %#v, want %#v", from, to, name, gotValue, sourceValue)
			}
		}
		if len(got.Artifacts) != len(source.Artifacts) {
			t.Fatalf("%s->%s: %d artifacts survived of %d", from, to, len(got.Artifacts), len(source.Artifacts))
		}
		for i := range source.Artifacts {
			if got.Artifacts[i].Name != source.Artifacts[i].Name || got.Artifacts[i].Digest != source.Artifacts[i].Digest {
				t.Fatalf("%s->%s: artifact %d = %#v, want %#v", from, to, i, got.Artifacts[i], source.Artifacts[i])
			}
		}
		for i := range source.Verification {
			if got.Verification[i] != source.Verification[i] {
				t.Fatalf("%s->%s: verification %d = %#v, want %#v", from, to, i, got.Verification[i], source.Verification[i])
			}
		}
		// The target's own Result carries the target's own name and its own
		// digest, not the ones the handoff carried.
		var targetAdapter provider.Adapter
		for _, a := range allAdapters() {
			if a.Name() == to {
				targetAdapter = a
			}
		}
		if targetAdapter == nil {
			t.Fatalf("no adapter named %q", to)
		}
		result, failure := provider.ParseOrClassify(targetAdapter, readFixture(t, to, "success"))
		if failure.Class != "" {
			t.Fatalf("%s->%s: the target's own success fixture failed: %q", from, to, failure.Class)
		}
		if result.Provider != to {
			t.Fatalf("%s->%s: the target's Result names %q", from, to, result.Provider)
		}
		if result.OutputDigest == handoff.OutputDigest {
			t.Fatalf("%s->%s: the target's Result carries the digest the handoff carried", from, to)
		}
		if result.OutputDigest == "" {
			t.Fatalf("%s->%s: the target's Result has no output digest of its own", from, to)
		}
	}
	t.Logf("all %d ordered pairs of %v converted and preserved the packet field by field", len(pairs), provider.PoolNames())
}

// TestMutatingAnyCarriedFactIsRefusedNotSilentlyAccepted is the fail-closed
// half. Before the content digest existed, a handoff that dropped an Artifact
// produced a perfectly valid Handoff.
func TestMutatingAnyCarriedFactIsRefusedNotSilentlyAccepted(t *testing.T) {
	breaker := newBreaker(t)
	mutations := []struct {
		field  string
		mutate func(*provider.Handoff)
	}{
		{"IncrementID", func(h *provider.Handoff) { h.Packet.IncrementID = "inc-other" }},
		{"Constraints", func(h *provider.Handoff) { h.Packet.Constraints = h.Packet.Constraints[:1] }},
		{"Constraints (reordered)", func(h *provider.Handoff) {
			h.Packet.Constraints = []string{h.Packet.Constraints[1], h.Packet.Constraints[0]}
		}},
		{"Decisions", func(h *provider.Handoff) { h.Packet.Decisions = h.Packet.Decisions[:1] }},
		{"Decisions (detail)", func(h *provider.Handoff) { h.Packet.Decisions[0].Detail = "something else" }},
		{"Artifacts (dropped)", func(h *provider.Handoff) { h.Packet.Artifacts = h.Packet.Artifacts[:1] }},
		{"Artifacts (name)", func(h *provider.Handoff) { h.Packet.Artifacts[0].Name = "internal/provider/other.go" }},
		{"Artifacts (digest)", func(h *provider.Handoff) {
			h.Packet.Artifacts[0].Digest = provider.DigestOutput("a different artifact")
		}},
		{"Verification (dropped)", func(h *provider.Handoff) { h.Packet.Verification = h.Packet.Verification[:1] }},
		{"Verification (status)", func(h *provider.Handoff) { h.Packet.Verification[0].Status = "failed" }},
		{"Verification (evidence digest)", func(h *provider.Handoff) {
			h.Packet.Verification[1].EvidenceDigest = provider.DigestOutput("other evidence")
		}},
		{"Unresolved", func(h *provider.Handoff) { h.Packet.Unresolved = nil }},
	}
	for _, mutation := range mutations {
		handoff, err := provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		mutation.mutate(&handoff)
		if err := handoff.Validate(); !errors.Is(err, provider.ErrHandoffContentChanged) {
			t.Fatalf("mutating %s produced Validate() = %v, want a refusal", mutation.field, err)
		}
		if _, err := provider.RequestFromHandoff(handoff, "op-1", "/workspace", breaker); !errors.Is(err, provider.ErrHandoffContentChanged) {
			t.Fatalf("mutating %s produced a Request anyway: %v", mutation.field, err)
		}
	}
	// A blanked digest is refused too, so the check cannot be skipped by
	// removing the value it checks against.
	handoff, err := provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	handoff.ContentDigest = ""
	if err := handoff.Validate(); !errors.Is(err, provider.ErrHandoffContentChanged) {
		t.Fatalf("an empty content digest was accepted: %v", err)
	}
	// The digest is over the carried facts only: changing something the
	// handoff does not promise to preserve does not fabricate a mismatch.
	handoff, err = provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if provider.HandoffContentDigest(handoff.Packet) != handoff.ContentDigest {
		t.Fatal("the digest does not recompute over its own packet")
	}
	summaryChanged := richPacket()
	summaryChanged.RequirementSummary = "a differently worded summary of the same increment"
	if provider.HandoffContentDigest(summaryChanged) != handoff.ContentDigest {
		t.Fatal("the content digest covers the requirement summary; it is declared to cover the increment, constraints, decisions, artifacts, verification and unresolved list only")
	}
}

func TestRequestFromHandoffRefusesAnUnknownTargetAndANotSendingTarget(t *testing.T) {
	breaker := newBreaker(t)
	handoff, err := provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	// A target outside the three names is refused at both ends.
	if _, err := provider.PrepareHandoff("codex", "gemini", richPacket(), provider.Result{Provider: "codex"}); !errors.Is(err, provider.ErrUnknownProvider) {
		t.Fatalf("PrepareHandoff accepted a fourth name: %v", err)
	}
	outside := handoff
	outside.ToProvider = "gemini"
	if _, err := provider.RequestFromHandoff(outside, "op-1", "/workspace", breaker); !errors.Is(err, provider.ErrUnknownProvider) {
		t.Fatalf("RequestFromHandoff accepted a fourth name: %v", err)
	}
	if _, err := provider.RequestFromHandoff(handoff, "op-1", "/workspace", nil); !errors.Is(err, provider.ErrIncompleteDependencies) {
		t.Fatalf("a nil breaker was accepted: %v", err)
	}

	// The target's circuit decides. While the Loop is not sending to claude,
	// no handoff to claude converts.
	pool := newPool(t)
	window := newWindow(t, "claude", 100, 100000)
	observe(t, pool, breaker, window, provider.FailureQuota, base())
	if _, err := provider.RequestFromHandoff(handoff, "op-1", "/workspace", breaker); !errors.Is(err, provider.ErrHandoffTargetNotSending) {
		t.Fatalf("a handoff converted to a target this Loop is not sending to: %v", err)
	}
	// A probing target is not a sending target either: one invocation is
	// already committed to finding out, and a handoff is not that invocation.
	rolled := base().Add(time.Hour)
	if _, err := breaker.Probe(pool, window, "claude", rolled); err != nil {
		t.Fatalf("probe: %v", err)
	}
	report, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if report.State != provider.CircuitProbing {
		t.Fatalf("state = %q, want probing", report.State)
	}
	if _, err := provider.RequestFromHandoff(handoff, "op-1", "/workspace", breaker); !errors.Is(err, provider.ErrHandoffTargetNotSending) {
		t.Fatalf("a handoff converted to a probing target: %v", err)
	}
	// Once the target is sending again, the same handoff converts, so the
	// refusal above was about the circuit and not about the handoff.
	if err := provider.ApplySuccess(pool, breaker, "claude", rolled); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RequestFromHandoff(handoff, "op-1", "/workspace", breaker); err != nil {
		t.Fatalf("the handoff did not convert once the target was sending again: %v", err)
	}
}

func TestAChainPreservesTheFirstFailureClassAndRefusesAPingPong(t *testing.T) {
	breaker := newBreaker(t)
	first, err := provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{
		Provider: "codex",
		Failure:  &provider.Failure{Class: provider.FailureQuota, Retryable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.History) != 1 || first.History[0].Class != provider.FailureQuota {
		t.Fatalf("first history = %#v", first.History)
	}
	second, err := provider.ChainHandoff(first, "opencode", richPacket(), provider.Result{
		Provider: "claude",
		Failure:  &provider.Failure{Class: provider.FailureModel},
	})
	if err != nil {
		t.Fatalf("ChainHandoff: %v", err)
	}
	if len(second.History) != 2 {
		t.Fatalf("chained history = %#v", second.History)
	}
	if second.History[0].Class != provider.FailureQuota {
		t.Fatalf("the first failure class did not survive the chain: %#v", second.History)
	}
	if second.History[0].From != "codex" || second.History[0].To != "claude" {
		t.Fatalf("the first attempt was rewritten: %#v", second.History[0])
	}
	if second.History[1] != (provider.HandoffAttempt{From: "claude", To: "opencode", Class: provider.FailureModel}) {
		t.Fatalf("second attempt = %#v", second.History[1])
	}
	if err := second.Validate(); err != nil {
		t.Fatalf("chained handoff does not validate: %v", err)
	}
	if _, err := provider.RequestFromHandoff(second, "op-2", "/workspace", breaker); err != nil {
		t.Fatalf("the chained handoff did not convert: %v", err)
	}

	// Handing the same Increment back to a Provider already tried for it is
	// refused, in both directions.
	if _, err := provider.ChainHandoff(first, "codex", richPacket(), provider.Result{Provider: "claude"}); !errors.Is(err, provider.ErrHandoffRevisit) {
		t.Fatalf("handing back to the origin was accepted: %v", err)
	}
	if _, err := provider.ChainHandoff(second, "codex", richPacket(), provider.Result{Provider: "opencode"}); !errors.Is(err, provider.ErrHandoffRevisit) {
		t.Fatalf("a three-hop ping-pong was accepted: %v", err)
	}
	if _, err := provider.ChainHandoff(second, "claude", richPacket(), provider.Result{Provider: "opencode"}); !errors.Is(err, provider.ErrHandoffRevisit) {
		t.Fatalf("a return to the middle provider was accepted: %v", err)
	}

	// A different Increment is a different question: the history restarts
	// rather than carrying an unrelated Provider's turn.
	other := richPacket()
	other.IncrementID = "inc-10"
	restarted, err := provider.ChainHandoff(first, "codex", other, provider.Result{Provider: "claude"})
	if err != nil {
		t.Fatalf("a different Increment was refused: %v", err)
	}
	if len(restarted.History) != 1 || restarted.History[0].From != "claude" || restarted.History[0].To != "codex" {
		t.Fatalf("restarted history = %#v", restarted.History)
	}

	// The bound is stated and enforced.
	if provider.MaxHandoffHistory != 2 {
		t.Fatalf("MaxHandoffHistory = %d; the stated bound is 2", provider.MaxHandoffHistory)
	}
	overLong := second
	overLong.History = append(append([]provider.HandoffAttempt(nil), second.History...), provider.HandoffAttempt{From: "opencode", To: "codex"})
	if err := overLong.Validate(); !errors.Is(err, provider.ErrInvalidHandoff) {
		t.Fatalf("a history past the stated bound was accepted: %v", err)
	}
}

// TestHandoffAndHistoryHaveNowhereToPutAPromptAResponseOrASession is the
// structural half of A14. Every string-typed field of both types is
// enumerated and allow-listed, so a field able to hold free text cannot be
// added without failing here.
func TestHandoffAndHistoryHaveNowhereToPutAPromptAResponseOrASession(t *testing.T) {
	allowedHandoffStrings := map[string]string{
		"Version":       "the contract version, a constant",
		"FromProvider":  "one of the three Provider names",
		"ToProvider":    "one of the three Provider names",
		"OutputDigest":  "a sha256 digest of the source Provider's output, never the output",
		"ContentDigest": "a sha256 digest of the carried facts",
	}
	handoffType := reflect.TypeOf(provider.Handoff{})
	seen := map[string]bool{}
	for i := 0; i < handoffType.NumField(); i++ {
		field := handoffType.Field(i)
		if matchesProviderCredentialDenyList(field.Name) {
			t.Fatalf("Handoff.%s matches the credential deny list", field.Name)
		}
		for _, forbidden := range []string{"prompt", "response", "conversation", "transcript", "session", "text", "body", "message"} {
			if strings.Contains(strings.ToLower(field.Name), forbidden) {
				t.Fatalf("Handoff.%s could hold a %s", field.Name, forbidden)
			}
		}
		if field.Type.Kind() != reflect.String {
			continue
		}
		reason, allowed := allowedHandoffStrings[field.Name]
		if !allowed {
			t.Fatalf("Handoff.%s is a bare string with no declared purpose; a field able to hold free text is how a prompt or a response gets carried", field.Name)
		}
		seen[field.Name] = true
		_ = reason
	}
	if len(seen) != len(allowedHandoffStrings) {
		t.Fatalf("string-typed Handoff fields = %v, want exactly %v", seen, allowedHandoffStrings)
	}

	attemptType := reflect.TypeOf(provider.HandoffAttempt{})
	if attemptType.NumField() != 3 {
		t.Fatalf("HandoffAttempt has %d fields; the history is (from, to, class) and nothing else", attemptType.NumField())
	}
	for i := 0; i < attemptType.NumField(); i++ {
		field := attemptType.Field(i)
		switch field.Name {
		case "From", "To":
			if field.Type.Kind() != reflect.String {
				t.Fatalf("HandoffAttempt.%s is not a name", field.Name)
			}
		case "Class":
			if field.Type != reflect.TypeOf(provider.FailureClass("")) {
				t.Fatalf("HandoffAttempt.Class is %s, want the closed FailureClass enum", field.Type)
			}
		default:
			t.Fatalf("HandoffAttempt.%s is not one of (From, To, Class)", field.Name)
		}
	}
	// The closed-enum claim, enforced: a history entry naming a class the
	// package does not declare is refused.
	handoff, err := provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	handoff.History[0].Class = "free text that is not a class"
	if err := handoff.Validate(); !errors.Is(err, provider.ErrUnmappedFailureClass) {
		t.Fatalf("a history entry with an undeclared class was accepted: %v", err)
	}
	// A name outside the closed set is refused in the history too.
	handoff, err = provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	handoff.History[0].From = "gemini"
	if err := handoff.Validate(); !errors.Is(err, provider.ErrInvalidHandoff) && !errors.Is(err, provider.ErrUnknownProvider) {
		t.Fatalf("a history entry naming a fourth Provider was accepted: %v", err)
	}
}

// MEASURED EXCEPTION, recorded rather than papered over. Handoff.Failure is a
// Failure, whose Message field is a string, so the closed-enum claim above is
// about the fields Handoff itself declares -- and Failure.Message is reached
// only through that one named field. Message is not free text in practice: it
// is produced by safeMessage, which caps it at 256 bytes and replaces it
// outright when it matches the package's secret pattern, and every failure
// this task's own code produces sets it to a fixed literal. The assertion
// below states that boundary as a fact rather than leaving it implied.
func TestTheOneNestedStringFieldIsBoundedAndRedacted(t *testing.T) {
	long := strings.Repeat("x", 4096)
	f := (provider.CodexAdapter{}).NormalizeError(errors.New(long))
	if len(f.Message) > 256 {
		t.Fatalf("failure message is %d bytes; safeMessage caps it at 256", len(f.Message))
	}
	secretShaped := (provider.CodexAdapter{}).NormalizeError(errors.New("Bearer abcdefghijklmnopqrstuvwxyz012345"))
	if strings.Contains(secretShaped.Message, "abcdefghijklmnop") {
		t.Fatalf("a secret-shaped error text reached the failure message: %q", secretShaped.Message)
	}
	// And a handoff built from such a failure carries only the redacted form.
	handoff, err := provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex", Failure: &secretShaped})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(handoff.Failure.Message, "abcdefghijklmnop") {
		t.Fatalf("the handoff carries an unredacted message: %q", handoff.Failure.Message)
	}
}

// ===========================================================================
// V2-074 A7-A11: the Sendable predicate, the single-producer decision, the
// measured defect, the waiting-reason vocabulary and fixture-level
// preservation.
//
// Nothing below is appended to or edited inside any existing test case above:
// every assertion here is its own named test, and RequestFromHandoff's
// signature, its verdicts and its existing cases are untouched.
//
// SCOPE. Every property below is fixture-level and in-process. No Provider CLI
// was started, no invocation of any kind was made, and nothing here establishes
// that a real exhaustion or a real contract break produces the class the table
// assumes, nor that a real handoff preserves anything through a real journal,
// lease or Evidence record. V2-028 owns all of that.
// ===========================================================================

// TestTheThreeClosedStateSetsAreExactlyWhatTheASTDeclares is what makes the
// Sendable table's totality mean something. The three lists this task declares
// -- CircuitStates, WindowStates, SlotStates -- are compared against the
// constants of each type read out of this package's own AST, so a member added
// to breaker.go, usagewindow.go or pool.go without being listed fails here
// instead of silently falling outside the table.
func TestTheThreeClosedStateSetsAreExactlyWhatTheASTDeclares(t *testing.T) {
	type pinned struct {
		typeName string
		listed   map[string]bool
	}
	circuit := map[string]bool{}
	for _, v := range provider.CircuitStates() {
		circuit[string(v)] = true
	}
	window := map[string]bool{}
	for _, v := range provider.WindowStates() {
		window[string(v)] = true
	}
	slot := map[string]bool{}
	for _, v := range provider.SlotStates() {
		slot[string(v)] = true
	}
	source := map[string]bool{}
	for _, v := range provider.SourceStates() {
		source[string(v)] = true
	}
	obstacle := map[string]bool{}
	for _, v := range provider.CandidateObstacles() {
		obstacle[string(v)] = true
	}
	disposition := map[string]bool{}
	for _, v := range provider.HandoffDispositions() {
		disposition[string(v)] = true
	}
	reason := map[string]bool{}
	for _, v := range provider.HandoffWaitingReasons() {
		reason[string(v)] = true
	}
	for _, p := range []pinned{
		{typeName: "CircuitState", listed: circuit},
		{typeName: "WindowState", listed: window},
		{typeName: "SlotState", listed: slot},
		{typeName: "SourceState", listed: source},
		{typeName: "CandidateObstacle", listed: obstacle},
		{typeName: "HandoffDisposition", listed: disposition},
		{typeName: "HandoffWaitingReason", listed: reason},
	} {
		declared := constantsOfDeclaredType(t, p.typeName)
		if len(declared) == 0 {
			t.Fatalf("the AST scan found zero %s constants", p.typeName)
		}
		for value := range declared {
			if !p.listed[value] {
				t.Fatalf("%s %q is declared in the package but is not in this task's declared list; the decision table would not cover it", p.typeName, value)
			}
		}
		for value := range p.listed {
			if !declared[value] {
				t.Fatalf("this task's list names the %s %q, which the package does not declare as a constant", p.typeName, value)
			}
		}
		t.Logf("%s: %d constants, all listed", p.typeName, len(declared))
	}
	if len(provider.CircuitStates()) != 3 || len(provider.WindowStates()) != 3 || len(provider.SlotStates()) != 6 {
		t.Fatalf("the Sendable cross product is %d x %d x %d; A7 names 3 x 3 x 6", len(provider.CircuitStates()), len(provider.WindowStates()), len(provider.SlotStates()))
	}
}

// TestSendableIsTotalOverTheFullCrossProduct is A7's first clause. The full
// 3 x 3 x 6 product is enumerated, never sampled, and every cell is compared
// against a per-axis expectation declared independently in this test.
func TestSendableIsTotalOverTheFullCrossProduct(t *testing.T) {
	wantCircuit := map[provider.CircuitState]bool{
		provider.CircuitSending:    true,
		provider.CircuitNotSending: false,
		provider.CircuitProbing:    false,
	}
	wantWindow := map[provider.WindowState]bool{
		provider.WindowWithin:    true,
		provider.WindowUnknown:   true,
		provider.WindowExhausted: false,
	}
	wantSlot := map[provider.SlotState]bool{
		provider.SlotAvailable:            true,
		provider.SlotInUse:                true,
		provider.SlotCoolingDown:          false,
		provider.SlotUnauthenticated:      false,
		provider.SlotQuarantined:          false,
		provider.SlotStoppedForInspection: false,
	}
	if len(wantCircuit) != len(provider.CircuitStates()) || len(wantWindow) != len(provider.WindowStates()) || len(wantSlot) != len(provider.SlotStates()) {
		t.Fatalf("the expectation is not declared for every member: circuit=%d/%d window=%d/%d slot=%d/%d",
			len(wantCircuit), len(provider.CircuitStates()), len(wantWindow), len(provider.WindowStates()), len(wantSlot), len(provider.SlotStates()))
	}

	cells, sendableCells := 0, 0
	sourceStatesSeen := map[provider.SourceState]bool{}
	for _, circuit := range provider.CircuitStates() {
		for _, window := range provider.WindowStates() {
			for _, slot := range provider.SlotStates() {
				got, err := provider.Sendable(circuit, window, slot)
				if err != nil {
					t.Fatalf("Sendable(%q, %q, %q): %v", circuit, window, slot, err)
				}
				want := wantCircuit[circuit] && wantWindow[window] && wantSlot[slot]
				if got != want {
					t.Fatalf("Sendable(%q, %q, %q) = %v, want %v", circuit, window, slot, got, want)
				}
				if got {
					sendableCells++
				}
				state, err := provider.SourceStateOf(circuit, window, slot)
				if err != nil {
					t.Fatalf("SourceStateOf(%q, %q, %q): %v", circuit, window, slot, err)
				}
				sourceStatesSeen[state] = true
				// Every cell is logged, so the evidence record can carry the
				// full cross product as a transcript of measured behaviour
				// rather than as a table re-derived by hand.
				t.Logf("CELL circuit=%s window=%s slot=%s sendable=%v source=%s", circuit, window, slot, got, state)
				switch {
				case got && state != provider.SourceSendable:
					t.Fatalf("SourceStateOf(%q, %q, %q) = %q while Sendable is true", circuit, window, slot, state)
				case !got && circuit == provider.CircuitProbing && state != provider.SourceProbing:
					t.Fatalf("SourceStateOf(%q, %q, %q) = %q; a probing circuit that is not sendable is a wait", circuit, window, slot, state)
				case !got && circuit != provider.CircuitProbing && state != provider.SourceNotSendable:
					t.Fatalf("SourceStateOf(%q, %q, %q) = %q, want not-sendable", circuit, window, slot, state)
				}
				cells++
			}
		}
	}
	if want := len(provider.CircuitStates()) * len(provider.WindowStates()) * len(provider.SlotStates()); cells != want {
		t.Fatalf("enumerated %d cells, want the full cross product %d", cells, want)
	}
	if sendableCells == 0 || sendableCells == cells {
		t.Fatalf("%d of %d cells are sendable; a table that is all one value would satisfy every assertion vacuously", sendableCells, cells)
	}
	for _, state := range provider.SourceStates() {
		if !sourceStatesSeen[state] {
			t.Fatalf("the cross product never produces the declared source state %q", state)
		}
	}
	// An undeclared member of any axis is refused, not defaulted.
	for _, undeclared := range []struct {
		circuit provider.CircuitState
		window  provider.WindowState
		slot    provider.SlotState
	}{
		{circuit: "healthy-ish", window: provider.WindowWithin, slot: provider.SlotAvailable},
		{circuit: provider.CircuitSending, window: "almost-gone", slot: provider.SlotAvailable},
		{circuit: provider.CircuitSending, window: provider.WindowWithin, slot: "borrowed"},
	} {
		if _, err := provider.Sendable(undeclared.circuit, undeclared.window, undeclared.slot); !errors.Is(err, provider.ErrUndeclaredState) {
			t.Fatalf("Sendable(%q, %q, %q) err = %v, want ErrUndeclaredState", undeclared.circuit, undeclared.window, undeclared.slot, err)
		}
	}
	t.Logf("enumerated the full %d-cell Sendable cross product; %d cells sendable, %d not", cells, sendableCells, cells-sendableCells)
}

// TestProbingHoldsRatherThanHandsOff is A7's own named case.
func TestProbingHoldsRatherThanHandsOff(t *testing.T) {
	decision, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		if s.Name == "codex" {
			s.Circuit = provider.CircuitProbing
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != provider.DispositionWaiting || decision.Reason != provider.WaitingSourceIsProbing {
		t.Fatalf("a probing source produced %#v; probing is a wait, because the answer is already being bought with exactly one invocation", decision)
	}
	if decision.Target != "" {
		t.Fatalf("a probing source produced the target %q", decision.Target)
	}
}

// TestWindowUnknownDoesNotTriggerAndWindowExhaustedDoes is A7's two window
// cases, each named.
func TestWindowUnknownDoesNotTriggerAndWindowExhaustedDoes(t *testing.T) {
	unknown, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		if s.Name == "codex" {
			s.Window = provider.WindowUnknown
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Disposition != provider.DispositionNone {
		t.Fatalf("an unknown window triggered %#v; an invocation that reported no usage at all is an absence of information, not an exhaustion", unknown)
	}
	exhausted, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		if s.Name == "codex" {
			s.Window = provider.WindowExhausted
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.Disposition != provider.DispositionHandoffProposed || exhausted.Target != "claude" {
		t.Fatalf("an exhausted window produced %#v, want a handoff proposal to the first eligible candidate in the declared order", exhausted)
	}
}

// TestSlotInUseHoldsAndTheThreeOwnerActionSlotsMakeTheSourceNotSendable is
// A7's four slot cases.
func TestSlotInUseHoldsAndTheThreeOwnerActionSlotsMakeTheSourceNotSendable(t *testing.T) {
	inUse, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		if s.Name == "codex" {
			s.Slot = provider.SlotInUse
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if inUse.Disposition != provider.DispositionNone {
		t.Fatalf("an in-use slot triggered %#v; a lease is outstanding, so the source holds", inUse)
	}
	for _, slot := range []provider.SlotState{provider.SlotUnauthenticated, provider.SlotQuarantined, provider.SlotStoppedForInspection} {
		sendable, err := provider.Sendable(provider.CircuitSending, provider.WindowWithin, slot)
		if err != nil {
			t.Fatal(err)
		}
		if sendable {
			t.Fatalf("slot %q is sendable; each of these needs an owner's action and nothing the Loop can do moves it", slot)
		}
		state := slot
		decision, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
			if s.Name == "codex" {
				s.Slot = state
			}
		}))
		if err != nil {
			t.Fatal(err)
		}
		if decision.Disposition != provider.DispositionHandoffProposed {
			t.Fatalf("slot %q on the source produced %#v, want a handoff proposal", slot, decision)
		}
	}
}

// TestCancelledAndInvalidInputObservationsMoveNothing is A7's last named case,
// driven through ApplyObservation so the assertion is about the real path
// rather than about a hand-built state.
func TestCancelledAndInvalidInputObservationsMoveNothing(t *testing.T) {
	for _, class := range []provider.FailureClass{provider.FailureCancelled, provider.FailureInvalidInput} {
		action, err := provider.ActionForFailureClass(class)
		if err != nil || action != provider.ActionNeitherCountsNorOpens {
			t.Fatalf("%q: opening table action = %q err=%v", class, action, err)
		}
		pool := newPool(t)
		breaker := newBreaker(t)
		window := newWindow(t, "codex", 5, 100)
		observation, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{
			Provider: "codex",
			Window:   "rolling-hour",
			Failure:  provider.Failure{Class: class},
		}, base())
		if err != nil {
			t.Fatalf("%q: ApplyObservation: %v", class, err)
		}
		if observation.State != provider.CircuitSending {
			t.Fatalf("%q: circuit moved to %q", class, observation.State)
		}
		slot, err := pool.Slot("codex")
		if err != nil {
			t.Fatal(err)
		}
		windowState := window.State(base())
		decision, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
			if s.Name == "codex" {
				s.Circuit = observation.State
				s.Slot = slot.State
				s.Window = windowState
			}
		}))
		if err != nil {
			t.Fatal(err)
		}
		if decision.Disposition != provider.DispositionNone || decision.Target != "" {
			t.Fatalf("%q: an observation that is not about the Provider moved work: %#v", class, decision)
		}
	}
}

// TestNoFailureClassIsSynthesizedForAWindowExhaustion is A7's AST half. The
// files this task may edit contain no assignment of a FailureClass derived
// from a window state, and ApplyObservation's own behaviour is unchanged for
// every input it accepted before.
func TestNoFailureClassIsSynthesizedForAWindowExhaustion(t *testing.T) {
	windowIdentifiers := map[string]bool{
		"WindowState": true, "WindowExhausted": true, "WindowWithin": true,
		"WindowUnknown": true, "WindowStates": true, "WindowReport": true,
	}
	failureIdentifiers := map[string]bool{"FailureClass": true}
	for _, class := range provider.FailureClasses() {
		_ = class
	}
	scanned, functions := 0, 0
	for _, f := range nonTestGuardFiles(t) {
		scanned++
		for _, decl := range f.file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc {
				continue
			}
			functions++
			namesWindow, namesFailureClass := false, false
			ast.Inspect(fn, func(n ast.Node) bool {
				ident, isIdent := n.(*ast.Ident)
				if !isIdent {
					return true
				}
				if windowIdentifiers[ident.Name] {
					namesWindow = true
				}
				if failureIdentifiers[ident.Name] {
					namesFailureClass = true
				}
				return true
			})
			if namesWindow && namesFailureClass {
				t.Fatalf("%s: %s names both a window state and FailureClass; exhausting our own attempt ceiling is not a Provider fault and must not be recorded as one", f.path, fn.Name.Name)
			}
		}
	}
	if scanned == 0 || functions == 0 {
		t.Fatalf("scanned files=%d functions=%d; the walk is broken", scanned, functions)
	}
	// Positive control: the scan must catch the shape it exists for.
	synthetic := parseSyntheticProviderFile(t, "package provider\n\nfunc bad(w WindowState) FailureClass {\n\tif w == WindowExhausted {\n\t\treturn FailureQuota\n\t}\n\treturn \"\"\n}\n")
	caught := false
	for _, decl := range synthetic.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc {
			continue
		}
		namesWindow, namesFailureClass := false, false
		ast.Inspect(fn, func(n ast.Node) bool {
			ident, isIdent := n.(*ast.Ident)
			if !isIdent {
				return true
			}
			if windowIdentifiers[ident.Name] {
				namesWindow = true
			}
			if failureIdentifiers[ident.Name] {
				namesFailureClass = true
			}
			return true
		})
		if namesWindow && namesFailureClass {
			caught = true
		}
	}
	if !caught {
		t.Fatal("positive control: a synthetic function deriving a FailureClass from a window state was not flagged")
	}

	// And ApplyObservation is byte-identical in behaviour for every class the
	// opening table declares, driven through the real path.
	for _, class := range provider.FailureClasses() {
		action, err := provider.ActionForFailureClass(class)
		if err != nil {
			t.Fatalf("%q has no row in the opening table", class)
		}
		pool := newPool(t)
		breaker := newBreaker(t)
		window := newWindow(t, "codex", 5, 100)
		observation, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{
			Provider: "codex", Model: "m", Window: "rolling-hour",
			Failure: provider.Failure{Class: class},
		}, base())
		if err != nil {
			t.Fatalf("%q: ApplyObservation: %v", class, err)
		}
		if observation.Action != action {
			t.Fatalf("%q: ApplyObservation returned action %q, want the opening table's %q", class, observation.Action, action)
		}
		// The window is read and never written by the observation: its state
		// is unchanged by applying any class.
		if state := window.State(base()); state != provider.WindowWithin {
			t.Fatalf("%q: the usage window moved to %q from applying an observation", class, state)
		}
	}
	t.Logf("scanned non-test files=%d functions=%d; no function derives a FailureClass from a window state, and ApplyObservation's action matches the opening table for all %d declared classes", scanned, functions, len(provider.FailureClasses()))
}

// ---------------------------------------------------------------------------
// A8: the disposition function and its target filters
// ---------------------------------------------------------------------------

// sendableState is one Provider's state with everything permissive: authorized,
// sending, within-window, available slot, an in-interval declared version and
// no attempt history. Every case below starts from three of these and changes
// exactly what it is about.
func sendableState(t *testing.T, name string) provider.ProviderSelectionState {
	t.Helper()
	interval, err := provider.SupportedCLIVersions(name)
	if err != nil {
		t.Fatal(err)
	}
	return provider.ProviderSelectionState{
		Name:               name,
		Authorized:         true,
		Circuit:            provider.CircuitSending,
		Window:             provider.WindowWithin,
		Slot:               provider.SlotAvailable,
		CLIVersionDeclared: interval.From,
	}
}

// decisionWith builds a decision input over all three declared names, applying
// mutate to each state so a case changes exactly one thing.
func decisionWith(t *testing.T, source string, mutate func(*provider.ProviderSelectionState)) provider.HandoffDecision {
	t.Helper()
	loop, err := provider.SupportedLoopVersions(provider.ContractVersion)
	if err != nil {
		t.Fatal(err)
	}
	states := make([]provider.ProviderSelectionState, 0, len(provider.PoolNames()))
	for _, name := range provider.PoolNames() {
		state := sendableState(t, name)
		if mutate != nil {
			mutate(&state)
		}
		states = append(states, state)
	}
	return provider.HandoffDecision{
		Source:              source,
		IncrementID:         "inc-9",
		ObservedLoopVersion: loop.From,
		States:              states,
	}
}

// notSendableSource makes the source not sendable in the least interesting way
// available: a not-sending circuit.
func notSendableSource(source string) func(*provider.ProviderSelectionState) {
	return func(s *provider.ProviderSelectionState) {
		if s.Name == source {
			s.Circuit = provider.CircuitNotSending
		}
	}
}

// TestTheCandidateOrderIsTheDeclaredPoolNamesOrderAndIsDeterministic is A8's
// first clause.
func TestTheCandidateOrderIsTheDeclaredPoolNamesOrderAndIsDeterministic(t *testing.T) {
	// codex is the source, so the first eligible candidate must be claude:
	// PoolNames is codex, claude, opencode.
	first, err := provider.DecideHandoff(decisionWith(t, "codex", notSendableSource("codex")))
	if err != nil {
		t.Fatal(err)
	}
	if first.Disposition != provider.DispositionHandoffProposed || first.Target != "claude" {
		t.Fatalf("decision = %#v, want a proposal to claude, the first candidate in the declared PoolNames order", first)
	}
	// Driving the same state twice produces byte-identical results.
	second, err := provider.DecideHandoff(decisionWith(t, "codex", notSendableSource("codex")))
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("two runs over the same state produced %s and %s", firstBytes, secondBytes)
	}
	// And when the first candidate in the order is refused, the second is
	// chosen -- not some other order.
	skipped, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		switch s.Name {
		case "codex":
			s.Circuit = provider.CircuitNotSending
		case "claude":
			s.Authorized = false
			s.Slot = provider.SlotUnauthenticated
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Disposition != provider.DispositionHandoffProposed || skipped.Target != "opencode" {
		t.Fatalf("decision = %#v, want a proposal to opencode once claude is refused", skipped)
	}
	// Reversing which candidate is refused reverses the target, so the order is
	// a scan and not a hardcoded name.
	other, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		switch s.Name {
		case "codex":
			s.Circuit = provider.CircuitNotSending
		case "opencode":
			s.Authorized = false
			s.Slot = provider.SlotUnauthenticated
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if other.Target != "claude" {
		t.Fatalf("decision = %#v, want claude", other)
	}
	t.Logf("candidate order = %v; the same state twice produced byte-identical results", provider.PoolNames())
}

// TestBothOwnerActionRefusalShapesAreEnforced is A8's non_goal 2 clause,
// asserted for both shapes: a candidate the standing authorization does not
// cover, and a candidate whose slot needs an owner's action.
func TestBothOwnerActionRefusalShapesAreEnforced(t *testing.T) {
	// Shape one: unauthorized, with an otherwise perfectly available slot.
	unauthorized, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		switch s.Name {
		case "codex":
			s.Circuit = provider.CircuitNotSending
		case "claude", "opencode":
			s.Authorized = false
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if unauthorized.Disposition != provider.DispositionWaiting || unauthorized.Reason != provider.WaitingCandidateNeedsAnOwnerAction {
		t.Fatalf("decision = %#v; a Provider the owner has not authorized may never receive work", unauthorized)
	}
	if unauthorized.Target != "" {
		t.Fatalf("an unauthorized candidate was proposed as the target %q", unauthorized.Target)
	}
	// Shape two: authorized, but the slot needs an owner's action. Each of the
	// three such slot states, separately.
	for _, slot := range []provider.SlotState{provider.SlotUnauthenticated, provider.SlotQuarantined, provider.SlotStoppedForInspection} {
		state := slot
		decision, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
			switch s.Name {
			case "codex":
				s.Circuit = provider.CircuitNotSending
			case "claude", "opencode":
				s.Slot = state
			}
		}))
		if err != nil {
			t.Fatal(err)
		}
		if decision.Disposition != provider.DispositionWaiting || decision.Reason != provider.WaitingCandidateNeedsAnOwnerAction {
			t.Fatalf("slot %q: decision = %#v; a slot that needs an owner's action may never receive work", slot, decision)
		}
		if decision.Target != "" {
			t.Fatalf("slot %q: a candidate needing an owner action was proposed as the target %q", slot, decision.Target)
		}
	}
}

// TestAMeasuredIncompatibleCandidateIsRefusedAndAnUnknownOneIsNot is A8's
// compatibility clause, with BOTH cases run.
func TestAMeasuredIncompatibleCandidateIsRefusedAndAnUnknownOneIsNot(t *testing.T) {
	// Measured incompatible: refused.
	incompatible, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		switch s.Name {
		case "codex":
			s.Circuit = provider.CircuitNotSending
		case "claude", "opencode":
			interval, err := provider.SupportedCLIVersions(s.Name)
			if err != nil {
				t.Fatal(err)
			}
			s.CLIVersionDeclared = interval.Until
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if incompatible.Disposition != provider.DispositionWaiting || incompatible.Reason != provider.WaitingCandidateIsMeasuredIncompatible {
		t.Fatalf("decision = %#v, want waiting on a measured incompatibility", incompatible)
	}
	// Unknown: NOT refused. Both shapes of unknown -- an absent declared CLI
	// version and an absent Loop version -- are run.
	absentCLI, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		if s.Name == "codex" {
			s.Circuit = provider.CircuitNotSending
		}
		s.CLIVersionDeclared = ""
	}))
	if err != nil {
		t.Fatal(err)
	}
	if absentCLI.Disposition != provider.DispositionHandoffProposed || absentCLI.Target != "claude" {
		t.Fatalf("decision = %#v; an absent declared version is unknown and unknown is never rounded to incompatible", absentCLI)
	}
	absentLoop := decisionWith(t, "codex", notSendableSource("codex"))
	absentLoop.ObservedLoopVersion = ""
	withoutLoop, err := provider.DecideHandoff(absentLoop)
	if err != nil {
		t.Fatal(err)
	}
	if withoutLoop.Disposition != provider.DispositionHandoffProposed || withoutLoop.Target != "claude" {
		t.Fatalf("decision = %#v; an absent Loop version is unknown and must not refuse a candidate", withoutLoop)
	}
	// And an incompatible LOOP version does refuse every candidate, because
	// the verdict is the conjunction of the two relations.
	loopOutside := decisionWith(t, "codex", notSendableSource("codex"))
	loopInterval, err := provider.SupportedLoopVersions(provider.ContractVersion)
	if err != nil {
		t.Fatal(err)
	}
	loopOutside.ObservedLoopVersion = loopInterval.Until
	outside, err := provider.DecideHandoff(loopOutside)
	if err != nil {
		t.Fatal(err)
	}
	if outside.Disposition != provider.DispositionWaiting || outside.Reason != provider.WaitingCandidateIsMeasuredIncompatible {
		t.Fatalf("decision = %#v, want waiting: a Loop version outside R2 makes every candidate's conjunction incompatible", outside)
	}
}

// TestACandidateAlreadyTriedForThisIncrementIsRefusedAndTheChainBoundHolds is
// A8's last two clauses.
func TestACandidateAlreadyTriedForThisIncrementIsRefusedAndTheChainBoundHolds(t *testing.T) {
	tried, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		switch s.Name {
		case "codex":
			s.Circuit = provider.CircuitNotSending
		case "claude", "opencode":
			s.AlreadyTriedForIncrement = true
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if tried.Disposition != provider.DispositionWaiting || tried.Reason != provider.WaitingCandidateAlreadyTried {
		t.Fatalf("decision = %#v; a chain with no memory is how a Loop ping-pongs A to B to A", tried)
	}
	// Only one already tried: the other is still proposed.
	partly, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		switch s.Name {
		case "codex":
			s.Circuit = provider.CircuitNotSending
		case "claude":
			s.AlreadyTriedForIncrement = true
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if partly.Target != "opencode" {
		t.Fatalf("decision = %#v, want a proposal to opencode", partly)
	}

	// The chain bound: at the bound, waiting, whatever the candidates look
	// like; one below it, a proposal.
	atBound := decisionWith(t, "codex", notSendableSource("codex"))
	atBound.ChainLength = provider.MaxHandoffHistory
	bounded, err := provider.DecideHandoff(atBound)
	if err != nil {
		t.Fatal(err)
	}
	if bounded.Disposition != provider.DispositionWaiting || bounded.Reason != provider.WaitingChainBoundReached {
		t.Fatalf("decision = %#v, want waiting on the stated chain bound", bounded)
	}
	if bounded.Target != "" {
		t.Fatalf("a decision at the chain bound produced the target %q", bounded.Target)
	}
	below := decisionWith(t, "codex", notSendableSource("codex"))
	below.ChainLength = provider.MaxHandoffHistory - 1
	unbounded, err := provider.DecideHandoff(below)
	if err != nil {
		t.Fatal(err)
	}
	if unbounded.Disposition != provider.DispositionHandoffProposed {
		t.Fatalf("decision = %#v one below the chain bound, want a proposal", unbounded)
	}
	// A malformed input fails closed: no target and no guessed disposition.
	incomplete := decisionWith(t, "codex", nil)
	incomplete.States = incomplete.States[:2]
	if _, err := provider.DecideHandoff(incomplete); !errors.Is(err, provider.ErrIncompleteSelectionState) {
		t.Fatalf("an incomplete state set was accepted: %v", err)
	}
	if _, err := provider.DecideHandoff(decisionWith(t, "gemini", nil)); !errors.Is(err, provider.ErrUnknownProvider) {
		t.Fatalf("a fourth name was accepted as the source: %v", err)
	}
}

// TestDecideHandoffIsTheOnlyProducerOfAHandoffTarget is A8's AST clause, and
// the structural half of the wiring. Two facts:
//
//   - RequestFromHandoff is referenced from exactly ONE non-test location in
//     the whole module, which is RequestFromDisposition;
//   - RequestFromDisposition refuses any decision whose disposition is not
//     handoff-proposed and any target the decision did not produce, so
//     DecideHandoff is the only producer of the target it receives.
//
// The scanner is verified against a known-positive and a known-negative first.
func TestDecideHandoffIsTheOnlyProducerOfAHandoffTarget(t *testing.T) {
	const target = "RequestFromHandoff"
	// The scanner: count references to target in non-test Go files, excluding
	// the declaration itself.
	count := func(dir string) (int, map[string]int, error) {
		perFile := map[string]int{}
		total := 0
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
			if parseErr != nil {
				return parseErr
			}
			for _, decl := range file.Decls {
				fn, isFunc := decl.(*ast.FuncDecl)
				if isFunc && fn.Name != nil && fn.Name.Name == target {
					// The declaration itself is not a reference.
					continue
				}
				// Only identifiers are counted. A qualified reference such
				// as provider.RequestFromHandoff carries the name as the
				// selector's own *ast.Ident, so counting identifiers alone
				// covers both the qualified and the unqualified form exactly
				// once each -- which the control below proves.
				ast.Inspect(decl, func(n ast.Node) bool {
					ident, isIdent := n.(*ast.Ident)
					if isIdent && ident.Name == target {
						perFile[path]++
						total++
					}
					return true
				})
			}
			return nil
		})
		return total, perFile, err
	}

	// Known-positive and known-negative, in a temporary directory, so the
	// scanner cannot pass because it counts nothing.
	control := t.TempDir()
	positive := filepath.Join(control, "positive.go")
	if err := os.WriteFile(positive, []byte("package p\n\nfunc caller() { _, _ = provider."+target+"(a, b, c, d) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	negative := filepath.Join(control, "negative_test.go")
	if err := os.WriteFile(negative, []byte("package p\n\nfunc TestX() { _, _ = provider."+target+"(a, b, c, d) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	controlTotal, controlPerFile, err := count(control)
	if err != nil {
		t.Fatal(err)
	}
	if controlTotal != 1 || controlPerFile[positive] != 1 || controlPerFile[negative] != 0 {
		t.Fatalf("scanner control failed: total=%d perFile=%v; a non-test reference must be counted and a _test.go reference must not", controlTotal, controlPerFile)
	}

	// The real scan, over the whole module.
	total, perFile, err := count(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("%s is referenced from %d non-test locations in the module (%v), want exactly 1; a second execution path must not appear unnoticed", target, total, perFile)
	}
	only := ""
	for path := range perFile {
		only = path
	}
	if !strings.HasSuffix(filepath.ToSlash(only), "internal/provider/handoff.go") {
		t.Fatalf("the single non-test reference is in %s, not in the file that declares the single-producer path", only)
	}

	// And the consumer refuses everything DecideHandoff did not produce.
	handoff, err := provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	breaker := newBreaker(t)
	for _, bad := range []provider.HandoffDecisionResult{
		{Disposition: provider.DispositionNone, Source: "codex"},
		{Disposition: provider.DispositionWaiting, Reason: provider.WaitingChainBoundReached, Source: "codex"},
		{Disposition: provider.DispositionHandoffProposed, Target: "", Source: "codex"},
		{Disposition: provider.DispositionHandoffProposed, Target: "opencode", Source: "codex"},
		{Disposition: provider.DispositionHandoffProposed, Target: "claude", Source: "opencode"},
	} {
		if _, err := provider.RequestFromDisposition(bad, handoff, "op-1", "/workspace", breaker); err == nil {
			t.Fatalf("RequestFromDisposition accepted a decision it must refuse: %#v", bad)
		}
	}
	accepted, err := provider.RequestFromDisposition(provider.HandoffDecisionResult{
		Disposition: provider.DispositionHandoffProposed, Target: "claude", Source: "codex",
	}, handoff, "op-1", "/workspace", breaker)
	if err != nil {
		t.Fatalf("RequestFromDisposition refused a real proposal: %v", err)
	}
	if accepted.Packet.IncrementID != richPacket().IncrementID {
		t.Fatalf("the accepted Request carries Increment %q", accepted.Packet.IncrementID)
	}
	t.Logf("%s has exactly %d non-test reference in the module, in %s. A guard is weaker than a signature: widening RequestFromHandoff's parameters was rejected only because it would stop the existing tests compiling under their own names", target, total, filepath.ToSlash(only))
}

// ---------------------------------------------------------------------------
// A9: the measured defect, its positive control, and what remains
// ---------------------------------------------------------------------------

// TestPositiveControlAnUnauthenticatedSlotStillReportsASendingCircuit is A9's
// positive control, kept as a test that documents WHY the decision consults
// the slot. It asserts the defect, not its absence: NewBreaker creates every
// circuit closed regardless of the slot, Report maps closed to sending, and
// ActionMoveSlotWithoutOpening touches only the pool -- so after an observed
// provider-unauthenticated failure the slot is unauthenticated while the
// circuit still reports sending, and RequestFromHandoff, which consults only
// the breaker, accepts it.
//
// This behaviour is UNCHANGED by V2-074. RequestFromHandoff keeps its
// signature and every verdict; what changes is that a target may now only be
// produced by DecideHandoff, which refuses such a Provider.
func TestPositiveControlAnUnauthenticatedSlotStillReportsASendingCircuit(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "claude", 5, 100)

	// Before: every circuit is created closed regardless of the slot.
	initial, err := breaker.Report("claude")
	if err != nil {
		t.Fatal(err)
	}
	if initial.State != provider.CircuitSending {
		t.Fatalf("a freshly constructed circuit reports %q", initial.State)
	}

	observation, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{
		Provider: "claude", Window: "rolling-hour",
		Failure: provider.Failure{Class: provider.FailureUnauthenticated},
	}, base())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Action != provider.ActionMoveSlotWithoutOpening {
		t.Fatalf("the opening table's action for provider-unauthenticated is %q", observation.Action)
	}
	if observation.SlotState != provider.SlotUnauthenticated {
		t.Fatalf("the slot moved to %q, want unauthenticated", observation.SlotState)
	}
	// THE DEFECT, asserted: the circuit still reports sending.
	if observation.State != provider.CircuitSending {
		t.Fatalf("the circuit reports %q; if this ever becomes not-sending the defect this test documents has been closed elsewhere and this test must be revisited rather than deleted", observation.State)
	}

	// And RequestFromHandoff -- which consults only the breaker -- accepts it.
	handoff, err := provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.RequestFromHandoff(handoff, "op-1", "/workspace", breaker)
	if err != nil {
		t.Fatalf("the positive control did not reproduce: RequestFromHandoff refused an unauthenticated-slot target with %v", err)
	}
	if request.Packet.IncrementID == "" {
		t.Fatal("the positive control produced an empty Request")
	}
	t.Log("POSITIVE CONTROL REPRODUCED: after an observed provider-unauthenticated failure the pool slot is unauthenticated while the circuit still reports sending, and RequestFromHandoff accepts that Provider as a handoff target. This is the measured defect dp-v2-074 d7 names, and it is why the decision consults the slot")
}

// TestDecideHandoffRefusesAProviderWhoseSlotIsUnauthenticated is A9's closing
// half, driven through the same real path: the slot state comes from
// ApplyObservation, not from a hand-built value.
func TestDecideHandoffRefusesAProviderWhoseSlotIsUnauthenticated(t *testing.T) {
	pool := newPool(t)
	breaker := newBreaker(t)
	window := newWindow(t, "claude", 5, 100)
	observation, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{
		Provider: "claude", Window: "rolling-hour",
		Failure: provider.Failure{Class: provider.FailureUnauthenticated},
	}, base())
	if err != nil {
		t.Fatal(err)
	}
	claudeSlot, err := pool.Slot("claude")
	if err != nil {
		t.Fatal(err)
	}
	if claudeSlot.State != provider.SlotUnauthenticated || observation.State != provider.CircuitSending {
		t.Fatalf("the real path did not produce the state under test: slot=%q circuit=%q", claudeSlot.State, observation.State)
	}
	decision, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		switch s.Name {
		case "codex":
			s.Circuit = provider.CircuitNotSending
		case "claude":
			// Exactly what the real path produced: sending circuit,
			// unauthenticated slot.
			s.Circuit = observation.State
			s.Slot = claudeSlot.State
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Target == "claude" {
		t.Fatal("DecideHandoff proposed a Provider whose slot is unauthenticated")
	}
	if decision.Target != "opencode" {
		t.Fatalf("decision = %#v, want the remaining eligible candidate", decision)
	}
	// With BOTH candidates in that state, the decision waits and says why.
	blocked, err := provider.DecideHandoff(decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		switch s.Name {
		case "codex":
			s.Circuit = provider.CircuitNotSending
		case "claude", "opencode":
			s.Circuit = observation.State
			s.Slot = claudeSlot.State
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Disposition != provider.DispositionWaiting || blocked.Reason != provider.WaitingCandidateNeedsAnOwnerAction {
		t.Fatalf("decision = %#v, want waiting on an owner action", blocked)
	}
	if blocked.Target != "" {
		t.Fatalf("a waiting decision produced the target %q", blocked.Target)
	}
}

// ---------------------------------------------------------------------------
// A10: the waiting reason vocabulary, derived not invented
// ---------------------------------------------------------------------------

// TestTheWaitingReasonVocabularyIsExactlyWhatTheTableProduces is A10's
// two-directional totality. A waiting cell with no reason fails, and a
// declared reason no cell can produce fails.
func TestTheWaitingReasonVocabularyIsExactlyWhatTheTableProduces(t *testing.T) {
	table := provider.HandoffDecisionTable()
	if len(table) == 0 {
		t.Fatal("the decision table is empty")
	}
	wantCells := len(provider.SourceStates()) * 2 * len(provider.CandidateObstacles())
	if len(table) != wantCells {
		t.Fatalf("the table has %d cells, want the full cross product %d", len(table), wantCells)
	}
	produced := map[provider.HandoffWaitingReason]bool{}
	dispositions := map[provider.HandoffDisposition]bool{}
	for _, cell := range table {
		dispositions[cell.Disposition] = true
		switch cell.Disposition {
		case provider.DispositionWaiting:
			if cell.Reason == "" {
				t.Fatalf("waiting cell %#v carries no reason", cell)
			}
			produced[cell.Reason] = true
		default:
			if cell.Reason != "" {
				t.Fatalf("non-waiting cell %#v carries the reason %q", cell, cell.Reason)
			}
		}
	}
	declared := map[provider.HandoffWaitingReason]bool{}
	for _, reason := range provider.HandoffWaitingReasons() {
		declared[reason] = true
	}
	for reason := range produced {
		if !declared[reason] {
			t.Fatalf("the table produces the reason %q, which is not a declared member", reason)
		}
	}
	for reason := range declared {
		if !produced[reason] {
			t.Fatalf("the declared reason %q is produced by no cell of the table; a reason with no cell is an invention", reason)
		}
	}
	for _, disposition := range provider.HandoffDispositions() {
		if !dispositions[disposition] {
			t.Fatalf("the table never produces the declared disposition %q", disposition)
		}
	}
	if len(declared) == 0 {
		t.Fatal("the waiting reason vocabulary is empty")
	}

	// And DecideHandoff agrees with the table for every cell it can be driven
	// into, so the table is the behaviour rather than a second description of
	// it.
	driven := 0
	for _, cell := range table {
		decision, drivable := driveCell(t, cell)
		if !drivable {
			continue
		}
		if decision.Disposition != cell.Disposition || decision.Reason != cell.Reason {
			t.Fatalf("cell %#v: DecideHandoff produced disposition %q reason %q", cell, decision.Disposition, decision.Reason)
		}
		driven++
	}
	if driven == 0 {
		t.Fatal("no table cell was driven through DecideHandoff; the agreement check is vacuous")
	}
	t.Logf("decision table = %d cells, %d driven through DecideHandoff; waiting reasons declared=%d produced=%d", len(table), driven, len(declared), len(produced))
}

// driveCell builds a real decision input for one table cell and returns
// whether the cell is reachable through the real function. Every cell whose
// source is sendable or probing ignores the candidate axis, so those cells are
// driven once each rather than once per obstacle.
func driveCell(t *testing.T, cell provider.SelectionCell) (provider.HandoffDecisionResult, bool) {
	t.Helper()
	in := decisionWith(t, "codex", func(s *provider.ProviderSelectionState) {
		if s.Name == "codex" {
			switch cell.Source {
			case provider.SourceSendable:
			case provider.SourceProbing:
				s.Circuit = provider.CircuitProbing
			case provider.SourceNotSendable:
				s.Circuit = provider.CircuitNotSending
			}
			return
		}
		if cell.Source != provider.SourceNotSendable {
			return
		}
		switch cell.Obstacle {
		case provider.ObstacleNone:
		case provider.ObstacleOwnerAction:
			s.Slot = provider.SlotUnauthenticated
		case provider.ObstacleMeasuredIncompatible:
			interval, err := provider.SupportedCLIVersions(s.Name)
			if err != nil {
				t.Fatal(err)
			}
			s.CLIVersionDeclared = interval.Until
		case provider.ObstacleAlreadyTried:
			s.AlreadyTriedForIncrement = true
		case provider.ObstacleNotSendable:
			s.Circuit = provider.CircuitNotSending
		}
	})
	if cell.ChainBoundReached {
		in.ChainLength = provider.MaxHandoffHistory
	}
	decision, err := provider.DecideHandoff(in)
	if err != nil {
		t.Fatalf("cell %#v: DecideHandoff: %v", cell, err)
	}
	return decision, true
}

// ---------------------------------------------------------------------------
// A11: the handoff loses nothing, at the strength a fixture can prove
// ---------------------------------------------------------------------------

// TestADispositionFollowedByTheExistingConversionLosesNothing is A11, over all
// six ordered pairs, computed by the EXISTING HandoffContentDigest and not by a
// second copy of it.
//
// This is fixture-level preservation only. V2-027 already established the same
// property for the conversion itself and attributed preservation through a real
// journal, lease, Increment and Evidence record to V2-028; what this test adds
// is that the decision in FRONT of the conversion loses nothing either.
func TestADispositionFollowedByTheExistingConversionLosesNothing(t *testing.T) {
	pairs := orderedPairs()
	if len(pairs) != 6 {
		t.Fatalf("ordered pairs = %d, want 6", len(pairs))
	}
	breaker := newBreaker(t)
	digests := map[string]string{}
	for _, pair := range pairs {
		from, to := pair[0], pair[1]
		source := richPacket()
		handoff, err := provider.PrepareHandoff(from, to, source, provider.Result{
			Provider: from,
			Failure:  &provider.Failure{Class: provider.FailureQuota},
		})
		if err != nil {
			t.Fatalf("%s->%s: PrepareHandoff: %v", from, to, err)
		}
		// The decision, driven so that the proposal names exactly this target.
		decision, err := provider.DecideHandoff(decisionWith(t, from, func(s *provider.ProviderSelectionState) {
			switch s.Name {
			case from:
				s.Circuit = provider.CircuitNotSending
			default:
				if s.Name != to {
					s.Slot = provider.SlotUnauthenticated
				}
			}
		}))
		if err != nil {
			t.Fatalf("%s->%s: DecideHandoff: %v", from, to, err)
		}
		if decision.Disposition != provider.DispositionHandoffProposed || decision.Target != to {
			t.Fatalf("%s->%s: decision = %#v", from, to, decision)
		}
		request, err := provider.RequestFromDisposition(decision, handoff, "op-"+from+"-"+to, "/workspace", breaker)
		if err != nil {
			t.Fatalf("%s->%s: RequestFromDisposition: %v", from, to, err)
		}
		// The digest is computed by the existing function, applied to the
		// source packet and to the packet that came out the other side.
		want := provider.HandoffContentDigest(source)
		got := provider.HandoffContentDigest(request.Packet)
		if got != want {
			t.Fatalf("%s->%s: content digest %q != %q", from, to, got, want)
		}
		if handoff.ContentDigest != want {
			t.Fatalf("%s->%s: the Handoff carries the digest %q, want %q", from, to, handoff.ContentDigest, want)
		}
		digests[from+"->"+to] = got
	}
	if len(digests) != 6 {
		t.Fatalf("recorded %d digests, want 6", len(digests))
	}
	// All six pairs carry the same packet, so all six digests agree -- which is
	// what makes a single mutation below visible in all of them.
	first := digests["codex->claude"]
	for pair, value := range digests {
		if value != first {
			t.Fatalf("%s carries the digest %q while codex->claude carries %q", pair, value, first)
		}
	}

	// Dropping or mutating any one of the six carried facts produces a REFUSAL
	// rather than a repair, through the decision path.
	mutations := []struct {
		field  string
		mutate func(*provider.WorkPacket)
	}{
		{field: "IncrementID", mutate: func(p *provider.WorkPacket) { p.IncrementID = "inc-other" }},
		{field: "Constraints", mutate: func(p *provider.WorkPacket) { p.Constraints = nil }},
		{field: "Decisions", mutate: func(p *provider.WorkPacket) { p.Decisions = p.Decisions[:1] }},
		{field: "Artifacts", mutate: func(p *provider.WorkPacket) { p.Artifacts = p.Artifacts[:1] }},
		{field: "Verification", mutate: func(p *provider.WorkPacket) { p.Verification = nil }},
		{field: "Unresolved", mutate: func(p *provider.WorkPacket) { p.Unresolved = nil }},
	}
	decision := provider.HandoffDecisionResult{Disposition: provider.DispositionHandoffProposed, Target: "claude", Source: "codex"}
	for _, m := range mutations {
		handoff, err := provider.PrepareHandoff("codex", "claude", richPacket(), provider.Result{Provider: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		m.mutate(&handoff.Packet)
		if _, err := provider.RequestFromDisposition(decision, handoff, "op-1", "/workspace", breaker); !errors.Is(err, provider.ErrHandoffContentChanged) {
			t.Fatalf("losing %s produced %v, want a refusal", m.field, err)
		}
	}
	t.Logf("six ordered pairs, one shared content digest %s, and %d single-field losses each producing a refusal. This is FIXTURE-LEVEL preservation only: preservation through a real journal, lease, Increment and Evidence record is V2-028's", first, len(mutations))
}

// sharedDecisionTableRows is the whole decision table, transcribed as data in
// the canonical form "source|chain-bound|obstacle|disposition|reason". The same
// thirty rows are transcribed in internal/application's own test, and each side
// renders its own declared table into this form and compares against it, so the
// two independent declarations are compared cell by cell and byte for byte
// without either package importing the other -- internal/application must
// import neither internal/provider nor internal/runner, so a shared type is not
// available and a shared literal is the strongest form left.
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

// TestTheDecisionTableIsPinnedByValueToTheApplicationDeclaration is this side
// of A12's pinning test.
func TestTheDecisionTableIsPinnedByValueToTheApplicationDeclaration(t *testing.T) {
	table := provider.HandoffDecisionTable()
	if len(table) != len(sharedDecisionTableRows) {
		t.Fatalf("this side declares %d cells, the shared transcription has %d", len(table), len(sharedDecisionTableRows))
	}
	for index, cell := range table {
		if got := cell.Row(); got != sharedDecisionTableRows[index] {
			t.Fatalf("cell %d = %q, want %q", index, got, sharedDecisionTableRows[index])
		}
		t.Logf("CELL %s", cell.Row())
	}
	// The rendering is discriminating: a mutated cell does not still match.
	mutated := table[0]
	mutated.Disposition = provider.DispositionWaiting
	if mutated.Row() == sharedDecisionTableRows[0] {
		t.Fatal("the canonical rendering does not distinguish a changed disposition; the pin proves nothing")
	}
	t.Logf("pinned %d cells by value against the shared transcription internal/application also declares", len(table))
}
