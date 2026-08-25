package provider_test

// Provider-neutral handoff conversion (V2-027 A14).

import (
	"errors"
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
