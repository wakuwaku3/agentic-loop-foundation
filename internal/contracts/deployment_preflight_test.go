package contracts

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeploymentPreflightLedgerPassesWithNoRecords enforces the dp-v2-012 d5
// approval-before-side-effect invariant against this repository's real
// state. At the end of V2-013, .agents/v2/preflight/ holds no files and the
// evidence index holds no gcp-live- component entry, so this must pass
// trivially: an absent or empty preflight directory is a pass, not an error.
func TestDeploymentPreflightLedgerPassesWithNoRecords(t *testing.T) {
	root := filepath.Join("..", "..")
	if entries, err := os.ReadDir(filepath.Join(root, ".agents", "v2", "preflight")); err == nil && len(entries) > 0 {
		t.Fatalf("V2-013 end state must leave .agents/v2/preflight/ empty, found %d entries", len(entries))
	}
	index := readJSON(t, filepath.Join(root, ".agents", "v2", "evidence", "index.json"))
	for _, raw := range index["entries"].([]any) {
		entry := raw.(map[string]any)
		if strings.HasPrefix(stringValue(entry["component"]), "gcp-live-") {
			t.Fatalf("V2-013 end state must hold no gcp-live- evidence component, found %v", entry["id"])
		}
	}
	if err := CheckDeploymentPreflightLedger(root); err != nil {
		t.Fatalf("ledger check must pass with no preflight records and no gcp-live evidence: %v", err)
	}
}

// ledgerFixture builds a temporary root with a minimal .agents/v2/preflight/
// and .agents/v2/evidence/index.json + evidence file, so the three failure
// modes below can be demonstrated without touching the real repository tree.
// t.TempDir() auto-removes everything when the subtest ends, satisfying "use
// and then remove a temporary fixture" without any manual cleanup step.
type ledgerFixture struct {
	root string
}

func newLedgerFixture(t *testing.T) *ledgerFixture {
	t.Helper()
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "contracts", "schemas"))
	mustMkdirAll(t, filepath.Join(root, ".agents", "v2", "preflight"))
	mustMkdirAll(t, filepath.Join(root, ".agents", "v2", "evidence"))
	schema := mustRead(t, filepath.Join("..", "..", "contracts", "schemas", "deployment-preflight.json"))
	mustWrite(t, filepath.Join(root, "contracts", "schemas", "deployment-preflight.json"), schema)
	return &ledgerFixture{root: root}
}

func (f *ledgerFixture) writePreflight(t *testing.T, name string, record map[string]any) string {
	t.Helper()
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, ".agents", "v2", "preflight", name)
	mustWrite(t, path, b)
	return path
}

func (f *ledgerFixture) writeEvidence(t *testing.T, evidenceRelPath string, evidence map[string]any, indexEntry map[string]any) {
	t.Helper()
	b, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.root, evidenceRelPath), b)
	indexEntry["path"] = evidenceRelPath
	index := map[string]any{
		"schema_version":   "v1",
		"kind":             "evidence-index",
		"release_eligible": false,
		"entries":          []any{indexEntry},
	}
	ib, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.root, ".agents", "v2", "evidence", "index.json"), ib)
}

func basePreflightRecord(taskID string, approvedAt string) map[string]any {
	return map[string]any{
		"schema_version": "v1",
		"id":             "pf-" + strings.ToLower(taskID) + "-demo",
		"kind":           "deployment-preflight",
		"created_at":     "2026-08-24T10:00:00Z",
		"correlation_id": "demo",
		"task_id":        taskID,
		"target": map[string]any{
			"environment":     "live",
			"project_ref":     "al-demo-install-live",
			"installation_id": "demo-install",
			"region":          "us-central1",
			"service_name":    "control-plane",
			"image_digest":    strings.Repeat("a", 64),
			"plan_digest":     strings.Repeat("b", 64),
		},
		"limits": []any{
			limit("firestore-reads-per-day"), limit("firestore-writes-per-day"), limit("firestore-deletes-per-day"),
			limit("firestore-stored-bytes"), limit("cloud-run-instance-seconds-per-month"), limit("cloud-run-requests-per-month"),
			limit("cloud-scheduler-jobs"),
		},
		"rollback": map[string]any{
			"trigger":               "demo",
			"argv":                  []any{"true"},
			"completion_conditions": []any{"demo condition"},
		},
		"verification": []any{
			map[string]any{"name": "demo", "argv": []any{"true"}, "expected": "0", "read_only": true},
		},
		"approval": map[string]any{
			"approver":           "owner@example.com",
			"approved_at":        approvedAt,
			"approval_reference": "demo-ref",
			"scope":              "demo scope",
		},
	}
}
func limit(resource string) map[string]any {
	return map[string]any{"resource": resource, "ceiling": 1, "unit": "operations", "window": "day", "enforced_by": "demo", "fail_closed": true}
}

// A5 failure demonstration 1/3: a gcp-live- evidence entry with no matching
// preflight record at all must fail the ledger check.
func TestDeploymentPreflightLedgerFailsWithNoMatchingRecord(t *testing.T) {
	f := newLedgerFixture(t)
	// No preflight record is written at all for V2-999.
	f.writeEvidence(t, ".agents/v2/evidence/V2-999-gcp-live.json", map[string]any{
		"schema_version": "v1", "id": "ev-v2-999-gcp-live", "kind": "evidence", "created_at": "2026-08-24T10:00:00Z",
		"correlation_id": "demo", "task_id": "V2-999", "component": "gcp-live-apply", "evidence_key": strings.Repeat("c", 16), "result": "passed",
		"checks":      []any{map[string]any{"name": "demo", "status": "passed", "argv": []any{"true"}, "working_directory": ".", "timeout_seconds": 1}},
		"observed_at": "2026-08-24T11:00:00Z",
	}, map[string]any{"id": "ev-v2-999-gcp-live", "task_id": "V2-999", "component": "gcp-live-apply", "result": "passed", "release_eligible": false, "evidence_hash": strings.Repeat("d", 64)})

	err := CheckDeploymentPreflightLedger(f.root)
	if err == nil || !strings.Contains(err.Error(), "expected exactly one preflight record") {
		t.Fatalf("expected a missing-preflight-record failure, got %v", err)
	}
	t.Logf("before=no preflight record, after=%v", err)
}

// A5 failure demonstration 2/3: a preflight record exists for the task_id,
// but the evidence's artifact_refs never names it by digest.
func TestDeploymentPreflightLedgerFailsWithNoArtifactRefMatch(t *testing.T) {
	f := newLedgerFixture(t)
	f.writePreflight(t, "V2-998-apply.json", basePreflightRecord("V2-998", "2026-08-24T10:00:00Z"))
	f.writeEvidence(t, ".agents/v2/evidence/V2-998-gcp-live.json", map[string]any{
		"schema_version": "v1", "id": "ev-v2-998-gcp-live", "kind": "evidence", "created_at": "2026-08-24T10:00:00Z",
		"correlation_id": "demo", "task_id": "V2-998", "component": "gcp-live-apply", "evidence_key": strings.Repeat("c", 16), "result": "passed",
		"checks":        []any{map[string]any{"name": "demo", "status": "passed", "argv": []any{"true"}, "working_directory": ".", "timeout_seconds": 1}},
		"artifact_refs": []any{map[string]any{"artifact_id": "something-else", "media_type": "application/json", "sha256": strings.Repeat("0", 64), "size_bytes": 1}},
		"observed_at":   "2026-08-24T11:00:00Z",
	}, map[string]any{"id": "ev-v2-998-gcp-live", "task_id": "V2-998", "component": "gcp-live-apply", "result": "passed", "release_eligible": false, "evidence_hash": strings.Repeat("d", 64)})

	err := CheckDeploymentPreflightLedger(f.root)
	if err == nil || !strings.Contains(err.Error(), "no artifact_refs entry names deployment-preflight") {
		t.Fatalf("expected an artifact_refs mismatch failure, got %v", err)
	}
	t.Logf("before=artifact_refs points elsewhere, after=%v", err)
}

// A5 failure demonstration 3/3: approval.approved_at is not strictly before
// the evidence's observed_at (approved after the fact).
func TestDeploymentPreflightLedgerFailsWithApprovalAfterObservation(t *testing.T) {
	f := newLedgerFixture(t)
	path := f.writePreflight(t, "V2-997-apply.json", basePreflightRecord("V2-997", "2026-08-24T12:00:00Z"))
	digest := sha256HexFile(t, path)
	f.writeEvidence(t, ".agents/v2/evidence/V2-997-gcp-live.json", map[string]any{
		"schema_version": "v1", "id": "ev-v2-997-gcp-live", "kind": "evidence", "created_at": "2026-08-24T10:00:00Z",
		"correlation_id": "demo", "task_id": "V2-997", "component": "gcp-live-apply", "evidence_key": strings.Repeat("c", 16), "result": "passed",
		"checks":        []any{map[string]any{"name": "demo", "status": "passed", "argv": []any{"true"}, "working_directory": ".", "timeout_seconds": 1}},
		"artifact_refs": []any{map[string]any{"artifact_id": "deployment-preflight", "media_type": "application/json", "sha256": digest, "size_bytes": 1}},
		// observed_at is *before* approval.approved_at (12:00): approval
		// came after the effect was already observed.
		"observed_at": "2026-08-24T11:00:00Z",
	}, map[string]any{"id": "ev-v2-997-gcp-live", "task_id": "V2-997", "component": "gcp-live-apply", "result": "passed", "release_eligible": false, "evidence_hash": strings.Repeat("d", 64)})

	err := CheckDeploymentPreflightLedger(f.root)
	if err == nil || !strings.Contains(err.Error(), "is not strictly before observed_at") {
		t.Fatalf("expected an approval-after-observation failure, got %v", err)
	}
	t.Logf("before=approval after observation, after=%v", err)
}

// The positive counterpart of the three failures above: a well-formed
// preflight record and a matching, correctly-ordered gcp-live- evidence
// entry must pass.
func TestDeploymentPreflightLedgerPassesWithAWellFormedRecord(t *testing.T) {
	f := newLedgerFixture(t)
	path := f.writePreflight(t, "V2-996-apply.json", basePreflightRecord("V2-996", "2026-08-24T10:00:00Z"))
	digest := sha256HexFile(t, path)
	f.writeEvidence(t, ".agents/v2/evidence/V2-996-gcp-live.json", map[string]any{
		"schema_version": "v1", "id": "ev-v2-996-gcp-live", "kind": "evidence", "created_at": "2026-08-24T10:00:00Z",
		"correlation_id": "demo", "task_id": "V2-996", "component": "gcp-live-apply", "evidence_key": strings.Repeat("c", 16), "result": "passed",
		"checks":        []any{map[string]any{"name": "demo", "status": "passed", "argv": []any{"true"}, "working_directory": ".", "timeout_seconds": 1}},
		"artifact_refs": []any{map[string]any{"artifact_id": "deployment-preflight", "media_type": "application/json", "sha256": digest, "size_bytes": 1}},
		"observed_at":   "2026-08-24T11:00:00Z",
	}, map[string]any{"id": "ev-v2-996-gcp-live", "task_id": "V2-996", "component": "gcp-live-apply", "result": "passed", "release_eligible": false, "evidence_hash": strings.Repeat("d", 64)})

	if err := CheckDeploymentPreflightLedger(f.root); err != nil {
		t.Fatalf("expected a well-formed record to pass, got %v", err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
func sha256HexFile(t *testing.T, path string) string {
	t.Helper()
	b := mustRead(t, path)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
