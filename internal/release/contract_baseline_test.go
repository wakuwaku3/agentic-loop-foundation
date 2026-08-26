package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCompileFoundationBaselineContract proves that CompileContract can parse
// the Foundation Repository's real Release Contract baseline end to end, and
// that it still rejects unknown top-level fields, unknown capability fields,
// and a capability that claims stable status without evidence.
func TestCompileFoundationBaselineContract(t *testing.T) {
	root := filepath.Join("..", "..")
	contractPath := filepath.Join(root, "contracts", "release-contract", "foundation.json")
	docsPath := filepath.Join(root, "docs", "preview", "capabilities.md")

	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatal(err)
	}

	compiled, err := CompileContract(data, docs)
	if err != nil {
		t.Fatalf("compile foundation baseline contract: %v", err)
	}
	if compiled.Version != "0.1.0-preview.1" {
		t.Fatalf("Version = %q, want 0.1.0-preview.1", compiled.Version)
	}
	wantIDs := []string{
		"cap-repository-registration",
		"cap-requirement-intake",
		"cap-backlog-visibility",
		"cap-autonomous-resolution",
		"cap-human-input-request",
		"cap-preview-operation",
		"cap-stable-promotion",
		"cap-loop-control",
		"cap-loop-self-update",
		"cap-shared-resource-allocation",
		"cap-provider-operation",
		"cap-user-documentation",
	}
	if len(compiled.Capabilities) != len(wantIDs) {
		t.Fatalf("Capabilities count = %d, want %d", len(compiled.Capabilities), len(wantIDs))
	}
	for i, id := range wantIDs {
		if compiled.Capabilities[i] != id {
			t.Fatalf("Capabilities[%d] = %q, want %q", i, compiled.Capabilities[i], id)
		}
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	unknownTop := decodeCopy(t, decoded)
	unknownTop["unexpected_field"] = "unexpected"
	if _, err := CompileContract(encodeCopy(t, unknownTop), docs); err == nil {
		t.Fatal("(a) contract with an unknown top-level field was accepted")
	}

	unknownCapability := decodeCopy(t, decoded)
	capabilities := unknownCapability["capabilities"].([]any)
	firstUnknown := capabilities[0].(map[string]any)
	firstUnknown["unexpected"] = "unexpected"
	if _, err := CompileContract(encodeCopy(t, unknownCapability), docs); err == nil {
		t.Fatal("(b) contract with an unknown capability field was accepted")
	}

	stableWithoutEvidence := decodeCopy(t, decoded)
	capabilities = stableWithoutEvidence["capabilities"].([]any)
	firstStable := capabilities[0].(map[string]any)
	firstStable["status"] = "stable"
	firstStable["evidence_ids"] = []any{}
	if _, err := CompileContract(encodeCopy(t, stableWithoutEvidence), docs); err == nil {
		t.Fatal("(c) capability with status stable and empty evidence_ids was accepted")
	}
}

func decodeCopy(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var copied map[string]any
	if err := json.Unmarshal(b, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

func encodeCopy(t *testing.T, value map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
