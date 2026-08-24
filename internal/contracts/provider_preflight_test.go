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

// TestProviderPreflightLedgerPassesWithNoRecords enforces the dp-v2-047 d9
// approval-before-side-effect invariant against this repository's real
// state. At the end of V2-047, .agents/v2/provider-preflight/ holds no
// files and the evidence index holds no provider-live- component entry, so
// this must pass trivially: an absent or empty directory is a pass, not an
// error.
func TestProviderPreflightLedgerPassesWithNoRecords(t *testing.T) {
	root := filepath.Join("..", "..")
	if entries, err := os.ReadDir(filepath.Join(root, ".agents", "v2", "provider-preflight")); err == nil && len(entries) > 0 {
		t.Fatalf("V2-047 end state must leave .agents/v2/provider-preflight/ empty, found %d entries", len(entries))
	}
	index := readJSON(t, filepath.Join(root, ".agents", "v2", "evidence", "index.json"))
	for _, raw := range index["entries"].([]any) {
		entry := raw.(map[string]any)
		if strings.HasPrefix(stringValue(entry["component"]), "provider-live-") {
			t.Fatalf("V2-047 end state must hold no provider-live- evidence component, found %v", entry["id"])
		}
	}
	if err := CheckProviderPreflightLedger(root); err != nil {
		t.Fatalf("ledger check must pass with no preflight records and no provider-live evidence: %v", err)
	}
}

// providerLedgerFixture builds a temporary root with a minimal
// .agents/v2/provider-preflight/, .agents/v2/packets/ (for approval
// subject_path resolution) and .agents/v2/evidence/index.json + evidence
// file, so the ledger's failure modes can be demonstrated without touching
// the real repository tree. t.TempDir() auto-removes everything when the
// subtest ends.
type providerLedgerFixture struct {
	root string
}

func newProviderLedgerFixture(t *testing.T) *providerLedgerFixture {
	t.Helper()
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "contracts", "schemas"))
	mustMkdirAll(t, filepath.Join(root, ".agents", "v2", "provider-preflight"))
	mustMkdirAll(t, filepath.Join(root, ".agents", "v2", "evidence"))
	mustMkdirAll(t, filepath.Join(root, ".agents", "v2", "packets"))
	schema := mustRead(t, filepath.Join("..", "..", "contracts", "schemas", "provider-preflight.json"))
	mustWrite(t, filepath.Join(root, "contracts", "schemas", "provider-preflight.json"), schema)
	return &providerLedgerFixture{root: root}
}

// writeSubject writes the approval subject file the record will reference
// and returns its repository-relative path (as used in approval.subject_path)
// and the sha256 digest of its bytes (as used in approval.subject_digest).
func (f *providerLedgerFixture) writeSubject(t *testing.T, name string, content []byte) (relPath, digest string) {
	t.Helper()
	relPath = ".agents/v2/packets/" + name
	mustWrite(t, filepath.Join(f.root, filepath.FromSlash(relPath)), content)
	digest = fmt.Sprintf("%x", sha256.Sum256(content))
	return relPath, digest
}

func (f *providerLedgerFixture) writePreflight(t *testing.T, name string, record map[string]any) string {
	t.Helper()
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, ".agents", "v2", "provider-preflight", name)
	mustWrite(t, path, b)
	return path
}

func (f *providerLedgerFixture) writeEvidence(t *testing.T, evidenceRelPath string, evidence map[string]any, indexEntry map[string]any) {
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

func baseProviderPreflightRecord(taskID, createdAt, approvedAt, subjectPath, subjectDigest string) map[string]any {
	return map[string]any{
		"schema_version": "v1",
		"id":             "pp-" + strings.ToLower(taskID) + "-demo",
		"kind":           "provider-preflight",
		"created_at":     createdAt,
		"correlation_id": "demo",
		"task_id":        taskID,
		"provider": map[string]any{
			"name":            "claude",
			"version":         "2.1.241",
			"executable_path": "/usr/local/bin/claude",
		},
		"limits": map[string]any{
			"max_invocations":            16,
			"max_total_cost_usd":         10.0,
			"worst_case_reservation_usd": 1.0,
			"currency":                   "USD",
			"window":                     "total",
			"ledger_path":                "/var/lib/agentic-loop/provider-ledger.json",
			"enforced_by":                "demo",
			"fail_closed":                true,
		},
		"environment": map[string]any{
			"base_names":    []any{"HOME", "PATH"},
			"granted_names": []any{},
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
			"subject_path":       subjectPath,
			"subject_digest":     subjectDigest,
		},
	}
}

// TestProviderPreflightLedgerRejectsMissingRecord demonstrates failure mode
// 1/3: a provider-live- evidence entry with no matching preflight record at
// all must fail the ledger check.
func TestProviderPreflightLedgerRejectsMissingRecord(t *testing.T) {
	f := newProviderLedgerFixture(t)
	// No preflight record is written at all for V2-999.
	f.writeEvidence(t, ".agents/v2/evidence/V2-999-provider-live.json", map[string]any{
		"schema_version": "v1", "id": "ev-v2-999-provider-live", "kind": "evidence", "created_at": "2026-08-24T10:00:00Z",
		"correlation_id": "demo", "task_id": "V2-999", "component": "provider-live-invoke", "evidence_key": strings.Repeat("c", 16), "result": "passed",
		"checks":      []any{map[string]any{"name": "demo", "status": "passed", "argv": []any{"true"}, "working_directory": ".", "timeout_seconds": 1}},
		"observed_at": "2026-08-24T11:00:00Z",
	}, map[string]any{"id": "ev-v2-999-provider-live", "task_id": "V2-999", "component": "provider-live-invoke", "result": "passed", "release_eligible": false, "evidence_hash": strings.Repeat("d", 64)})

	err := CheckProviderPreflightLedger(f.root)
	if err == nil || !strings.Contains(err.Error(), "expected exactly one preflight record") {
		t.Fatalf("expected a missing-preflight-record failure, got %v", err)
	}
	t.Logf("before=no preflight record, after=%v", err)
}

// TestProviderPreflightLedgerRejectsDigestMismatch demonstrates failure mode
// 2/3: a preflight record exists for the task_id, but the evidence's
// artifact_refs never names it by digest.
func TestProviderPreflightLedgerRejectsDigestMismatch(t *testing.T) {
	f := newProviderLedgerFixture(t)
	subjectPath, subjectDigest := f.writeSubject(t, "V2-998-work-order.json", []byte(`{"id":"wo-v2-998"}`))
	f.writePreflight(t, "V2-998-invoke.json", baseProviderPreflightRecord("V2-998", "2026-08-24T10:00:00Z", "2026-08-24T09:00:00Z", subjectPath, subjectDigest))
	f.writeEvidence(t, ".agents/v2/evidence/V2-998-provider-live.json", map[string]any{
		"schema_version": "v1", "id": "ev-v2-998-provider-live", "kind": "evidence", "created_at": "2026-08-24T10:00:00Z",
		"correlation_id": "demo", "task_id": "V2-998", "component": "provider-live-invoke", "evidence_key": strings.Repeat("c", 16), "result": "passed",
		"checks":        []any{map[string]any{"name": "demo", "status": "passed", "argv": []any{"true"}, "working_directory": ".", "timeout_seconds": 1}},
		"artifact_refs": []any{map[string]any{"artifact_id": "something-else", "media_type": "application/json", "sha256": strings.Repeat("0", 64), "size_bytes": 1}},
		"observed_at":   "2026-08-24T11:00:00Z",
	}, map[string]any{"id": "ev-v2-998-provider-live", "task_id": "V2-998", "component": "provider-live-invoke", "result": "passed", "release_eligible": false, "evidence_hash": strings.Repeat("d", 64)})

	err := CheckProviderPreflightLedger(f.root)
	if err == nil || !strings.Contains(err.Error(), "no artifact_refs entry names provider-preflight") {
		t.Fatalf("expected an artifact_refs mismatch failure, got %v", err)
	}
	t.Logf("before=artifact_refs points elsewhere, after=%v", err)
}

// TestProviderPreflightLedgerRejectsApprovalAfterObservation demonstrates
// failure mode 3/3: approval.approved_at is not strictly before the
// evidence's observed_at (approved after the fact).
func TestProviderPreflightLedgerRejectsApprovalAfterObservation(t *testing.T) {
	f := newProviderLedgerFixture(t)
	subjectPath, subjectDigest := f.writeSubject(t, "V2-997-work-order.json", []byte(`{"id":"wo-v2-997"}`))
	path := f.writePreflight(t, "V2-997-invoke.json", baseProviderPreflightRecord("V2-997", "2026-08-24T13:00:00Z", "2026-08-24T12:00:00Z", subjectPath, subjectDigest))
	digest := sha256HexFile(t, path)
	f.writeEvidence(t, ".agents/v2/evidence/V2-997-provider-live.json", map[string]any{
		"schema_version": "v1", "id": "ev-v2-997-provider-live", "kind": "evidence", "created_at": "2026-08-24T10:00:00Z",
		"correlation_id": "demo", "task_id": "V2-997", "component": "provider-live-invoke", "evidence_key": strings.Repeat("c", 16), "result": "passed",
		"checks":        []any{map[string]any{"name": "demo", "status": "passed", "argv": []any{"true"}, "working_directory": ".", "timeout_seconds": 1}},
		"artifact_refs": []any{map[string]any{"artifact_id": "provider-preflight", "media_type": "application/json", "sha256": digest, "size_bytes": 1}},
		// observed_at is *before* approval.approved_at (12:00): approval
		// came after the effect was already observed.
		"observed_at": "2026-08-24T11:00:00Z",
	}, map[string]any{"id": "ev-v2-997-provider-live", "task_id": "V2-997", "component": "provider-live-invoke", "result": "passed", "release_eligible": false, "evidence_hash": strings.Repeat("d", 64)})

	err := CheckProviderPreflightLedger(f.root)
	if err == nil || !strings.Contains(err.Error(), "is not strictly before observed_at") {
		t.Fatalf("expected an approval-after-observation failure, got %v", err)
	}
	t.Logf("before=approval after observation, after=%v", err)
}

// TestProviderPreflightLedgerPassesWithAWellFormedRecord is the positive
// counterpart of the three failures above: a well-formed preflight record
// and a matching, correctly-ordered provider-live- evidence entry must
// pass.
func TestProviderPreflightLedgerPassesWithAWellFormedRecord(t *testing.T) {
	f := newProviderLedgerFixture(t)
	subjectPath, subjectDigest := f.writeSubject(t, "V2-996-work-order.json", []byte(`{"id":"wo-v2-996"}`))
	path := f.writePreflight(t, "V2-996-invoke.json", baseProviderPreflightRecord("V2-996", "2026-08-24T10:00:00Z", "2026-08-24T09:00:00Z", subjectPath, subjectDigest))
	digest := sha256HexFile(t, path)
	f.writeEvidence(t, ".agents/v2/evidence/V2-996-provider-live.json", map[string]any{
		"schema_version": "v1", "id": "ev-v2-996-provider-live", "kind": "evidence", "created_at": "2026-08-24T10:00:00Z",
		"correlation_id": "demo", "task_id": "V2-996", "component": "provider-live-invoke", "evidence_key": strings.Repeat("c", 16), "result": "passed",
		"checks":        []any{map[string]any{"name": "demo", "status": "passed", "argv": []any{"true"}, "working_directory": ".", "timeout_seconds": 1}},
		"artifact_refs": []any{map[string]any{"artifact_id": "provider-preflight", "media_type": "application/json", "sha256": digest, "size_bytes": 1}},
		"observed_at":   "2026-08-24T11:00:00Z",
	}, map[string]any{"id": "ev-v2-996-provider-live", "task_id": "V2-996", "component": "provider-live-invoke", "result": "passed", "release_eligible": false, "evidence_hash": strings.Repeat("d", 64)})

	if err := CheckProviderPreflightLedger(f.root); err != nil {
		t.Fatalf("expected a well-formed record to pass, got %v", err)
	}
}

// The four tests below give validateProviderPreflight's dp-v2-047 d8 rules
// 1-4 their own explicit negative assertion each, as A8 requires: a single
// test covering all four is not acceptable, because a single test that
// fails for any of four reasons cannot tell a reader which rule regressed.
// Each mutates one field of the committed valid provider-preflight fixture
// in memory (never writing the mutation back to disk) and calls
// validateProviderPreflight directly.

func loadValidProviderPreflightFixture(t *testing.T) map[string]any {
	t.Helper()
	root := filepath.Join("..", "..")
	return readJSON(t, filepath.Join(root, "contracts", "fixtures", "valid", "provider-preflight.json"))
}

// Rule 1: worst_case_reservation_usd must be > 0 and <= max_total_cost_usd.
func TestValidateProviderPreflightRejectsReservationAboveCeiling(t *testing.T) {
	record := loadValidProviderPreflightFixture(t)
	limits := record["limits"].(map[string]any)
	limits["worst_case_reservation_usd"] = limits["max_total_cost_usd"].(float64) + 1
	if err := validateProviderPreflight(record); err == nil || !strings.Contains(err.Error(), "worst_case_reservation_usd") {
		t.Fatalf("expected a reservation-above-ceiling rejection, got %v", err)
	}
}

// Rule 2: ledger_path must be absolute and outside .agents/.
func TestValidateProviderPreflightRejectsInternalLedgerPath(t *testing.T) {
	record := loadValidProviderPreflightFixture(t)
	limits := record["limits"].(map[string]any)
	limits["ledger_path"] = "/repo/.agents/v2/provider-ledger.json"
	if err := validateProviderPreflight(record); err == nil || !strings.Contains(err.Error(), "ledger_path") {
		t.Fatalf("expected a ledger_path rejection, got %v", err)
	}
}

// Rule 3: approved_at must not be later than created_at.
func TestValidateProviderPreflightRejectsApprovalAfterCreation(t *testing.T) {
	record := loadValidProviderPreflightFixture(t)
	approval := record["approval"].(map[string]any)
	approval["approved_at"] = "2099-01-01T00:00:00Z"
	if err := validateProviderPreflight(record); err == nil || !strings.Contains(err.Error(), "approved_at") {
		t.Fatalf("expected an approval-after-creation rejection, got %v", err)
	}
}

// Rule 4: if provider.name is claude, environment.granted_names must be
// empty.
func TestValidateProviderPreflightRejectsGrantedNamesForClaude(t *testing.T) {
	record := loadValidProviderPreflightFixture(t)
	environment := record["environment"].(map[string]any)
	environment["granted_names"] = []any{"SOME_NAME"}
	if err := validateProviderPreflight(record); err == nil || !strings.Contains(err.Error(), "granted_names") {
		t.Fatalf("expected a granted_names rejection for claude, got %v", err)
	}
}
