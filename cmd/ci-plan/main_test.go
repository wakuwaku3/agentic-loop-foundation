package main

import (
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/ci"
)

func TestResolveIdentityDefaultsWhenEmptyOrWhitespace(t *testing.T) {
	cases := []string{"", " ", "\t", "\n  \t"}
	for _, in := range cases {
		got := resolveIdentity(in, defaultTaskID)
		if got != defaultTaskID {
			t.Errorf("resolveIdentity(%q, %q) = %q, want default %q", in, defaultTaskID, got, defaultTaskID)
		}
	}
}

func TestResolveIdentityUsesInjectedValue(t *testing.T) {
	got := resolveIdentity("V2-008", defaultTaskID)
	if got != "V2-008" {
		t.Errorf("resolveIdentity injected value = %q, want %q", got, "V2-008")
	}
	got = resolveIdentity("v2-008-candidate-gate", defaultCorrelationID)
	if got != "v2-008-candidate-gate" {
		t.Errorf("resolveIdentity injected value = %q, want %q", got, "v2-008-candidate-gate")
	}
}

func TestTaskIDPatternRejectsInvalidValues(t *testing.T) {
	invalid := []string{"V2-5", "x", "v2-005", "V2-0055", "V2-05a", "V2005"}
	for _, id := range invalid {
		if taskIDPattern.MatchString(id) {
			t.Errorf("taskIDPattern unexpectedly matched invalid id %q", id)
		}
	}
}

func TestTaskIDPatternAcceptsValidValues(t *testing.T) {
	valid := []string{"V2-000", "V2-008", "V2-999"}
	for _, id := range valid {
		if !taskIDPattern.MatchString(id) {
			t.Errorf("taskIDPattern rejected valid id %q", id)
		}
	}
}

func TestEvidenceRecordIDShape(t *testing.T) {
	c := ci.Component{
		ID: "ci",
		Check: ci.Check{
			Runner: "make",
			Target: "component-ci",
		},
	}
	// Built at run time so that a 64-hex literal never appears in the source
	// tree, where a secret scanner would read it as a generic API key.
	evidenceKey := strings.Repeat("abcdef0123456789", 4)
	if len(evidenceKey) != 64 {
		t.Fatalf("evidence key fixture length = %d, want 64", len(evidenceKey))
	}
	rec := evidenceRecord("ci", evidenceKey, c, "V2-008", "v2-008-candidate-gate", time.Now().UTC())

	wantID := "component-ci-" + evidenceKey[:16]
	gotID, _ := rec["id"].(string)
	if gotID != wantID {
		t.Errorf("record id = %q, want %q", gotID, wantID)
	}
	if got, _ := rec["task_id"].(string); got != "V2-008" {
		t.Errorf("record task_id = %q, want V2-008", got)
	}
	if got, _ := rec["correlation_id"].(string); got != "v2-008-candidate-gate" {
		t.Errorf("record correlation_id = %q, want v2-008-candidate-gate", got)
	}
	if got, _ := rec["result"].(string); got != "passed" {
		t.Errorf("record result = %q, want passed", got)
	}
	if got, _ := rec["evidence_key"].(string); got != evidenceKey {
		t.Errorf("record evidence_key = %q, want %q", got, evidenceKey)
	}
	if got, _ := rec["component"].(string); got != "ci" {
		t.Errorf("record component = %q, want ci", got)
	}
	if !strings.HasPrefix(gotID, "component-ci-") {
		t.Errorf("record id %q does not have expected prefix", gotID)
	}
}
