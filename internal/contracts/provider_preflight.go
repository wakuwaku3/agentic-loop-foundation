package contracts

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// validateProviderPreflight enforces dp-v2-047 d8 rules 1-4, the parts of a
// provider preflight record that are not expressible in the restricted
// schema keyword subset (internal/contracts/validator.go implements only
// $ref, const, enum, type, required, properties, additionalProperties:false,
// minItems, items, minLength, maxLength, pattern, format, minimum, maximum,
// none of which can cross-check two sibling fields or a field against the
// enclosing record). It is invoked by Validate for every value whose kind is
// "provider-preflight", so it also runs for the fixtures in
// TestSchemasAndFixtures.
//
//  1. limits.worst_case_reservation_usd must be greater than zero and no
//     greater than limits.max_total_cost_usd: a worst case that exceeds the
//     ceiling it is meant to sit under is not a ceiling.
//  2. limits.ledger_path must be an absolute path and must not sit inside
//     this repository's .agents/ tree, so the cost ledger cannot be
//     committed or mistaken for a workspace artifact.
//  3. approval.approved_at must not be later than created_at: approval
//     cannot postdate the record that describes what was approved.
//  4. if provider.name is "claude", environment.granted_names must be
//     empty, because the claude CLI reads its own on-disk credential store
//     and the Secret Broker has nothing to hand it.
func validateProviderPreflight(record map[string]any) error {
	limits, _ := record["limits"].(map[string]any)
	worstCase, maxTotal := number(limits["worst_case_reservation_usd"]), number(limits["max_total_cost_usd"])
	if worstCase <= 0 || worstCase > maxTotal {
		return fmt.Errorf("/limits/worst_case_reservation_usd: must be greater than zero and no greater than max_total_cost_usd")
	}

	ledgerPath := stringValue(limits["ledger_path"])
	if !strings.HasPrefix(ledgerPath, "/") {
		return fmt.Errorf("/limits/ledger_path: must be an absolute path")
	}
	if strings.Contains(ledgerPath, ".agents/") {
		return fmt.Errorf("/limits/ledger_path: must not sit inside the repository's .agents/ tree")
	}

	createdAt, err := time.Parse(time.RFC3339, stringValue(record["created_at"]))
	if err != nil {
		return fmt.Errorf("/created_at: %w", err)
	}
	approval, _ := record["approval"].(map[string]any)
	approvedAt, err := time.Parse(time.RFC3339, stringValue(approval["approved_at"]))
	if err != nil {
		return fmt.Errorf("/approval/approved_at: %w", err)
	}
	if approvedAt.After(createdAt) {
		return fmt.Errorf("/approval/approved_at: must not be later than created_at")
	}

	provider, _ := record["provider"].(map[string]any)
	if stringValue(provider["name"]) == "claude" {
		environment, _ := record["environment"].(map[string]any)
		granted, _ := environment["granted_names"].([]any)
		if len(granted) != 0 {
			return fmt.Errorf("/environment/granted_names: must be empty when provider.name is claude")
		}
	}
	return nil
}

// CheckProviderPreflightLedger enforces the dp-v2-047 d9 invariant that
// human approval precedes any billed Provider side effect a "provider-live-"
// evidence component reports. root must contain
// .agents/v2/provider-preflight/ (which may be absent or empty: that is a
// pass) and .agents/v2/evidence/index.json.
//
// For every file under <root>/.agents/v2/provider-preflight/*.json: it must
// validate against contracts/schemas/provider-preflight.json, its filename
// must begin with its own task_id, and sha256 of the file named by
// approval.subject_path must equal approval.subject_digest (the record
// cannot claim an approval that was granted for different text).
//
// For every entry of the evidence index whose component begins with
// "provider-live-": exactly one preflight record must share its task_id;
// the evidence record itself (read from the entry's path) must carry an
// artifact_refs entry whose artifact_id is "provider-preflight" and whose
// sha256 equals the sha256 of that preflight record file; and the record's
// approval.approved_at must be strictly earlier than the evidence's
// observed_at.
func CheckProviderPreflightLedger(root string) error {
	schemaPath := filepath.Join(root, "contracts", "schemas", "provider-preflight.json")
	schema, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("reading provider-preflight schema: %w", err)
	}
	recordDir := filepath.Join(root, ".agents", "v2", "provider-preflight")
	dirEntries, err := os.ReadDir(recordDir)
	if err != nil {
		if os.IsNotExist(err) {
			dirEntries = nil
		} else {
			return fmt.Errorf("reading %s: %w", recordDir, err)
		}
	}

	byTaskID := map[string][]string{} // task_id -> preflight file paths
	byPath := map[string]map[string]any{}
	for _, e := range dirEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(recordDir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if err := Validate(schema, raw, ResolveSchemaRef(filepath.Join(root, "contracts", "schemas"))); err != nil {
			return fmt.Errorf("%s: schema invalid: %w", path, err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		taskID := stringValue(parsed["task_id"])
		if !strings.HasPrefix(e.Name(), taskID) {
			return fmt.Errorf("%s: filename does not begin with task_id %q", path, taskID)
		}

		approval, _ := parsed["approval"].(map[string]any)
		subjectPath := filepath.Join(root, stringValue(approval["subject_path"]))
		subjectBytes, err := os.ReadFile(subjectPath)
		if err != nil {
			return fmt.Errorf("%s: reading approval.subject_path %s: %w", path, subjectPath, err)
		}
		subjectDigest := fmt.Sprintf("%x", sha256.Sum256(subjectBytes))
		if subjectDigest != stringValue(approval["subject_digest"]) {
			return fmt.Errorf("%s: approval.subject_digest does not match sha256 of %s", path, subjectPath)
		}

		byTaskID[taskID] = append(byTaskID[taskID], path)
		byPath[path] = parsed
	}

	indexPath := filepath.Join(root, ".agents", "v2", "evidence", "index.json")
	indexRaw, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", indexPath, err)
	}
	var index struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(indexRaw, &index); err != nil {
		return fmt.Errorf("%s: %w", indexPath, err)
	}

	for _, entry := range index.Entries {
		component := stringValue(entry["component"])
		if !strings.HasPrefix(component, "provider-live-") {
			continue
		}
		taskID := stringValue(entry["task_id"])
		matches := byTaskID[taskID]
		if len(matches) != 1 {
			return fmt.Errorf("provider-live evidence %s (task_id %s): expected exactly one preflight record, found %d", stringValue(entry["id"]), taskID, len(matches))
		}
		recordPath := matches[0]
		record := byPath[recordPath]
		recordBytes, err := os.ReadFile(recordPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", recordPath, err)
		}
		recordDigest := fmt.Sprintf("%x", sha256.Sum256(recordBytes))

		evidencePath := filepath.Join(root, stringValue(entry["path"]))
		evidenceRaw, err := os.ReadFile(evidencePath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", evidencePath, err)
		}
		var evidence map[string]any
		if err := json.Unmarshal(evidenceRaw, &evidence); err != nil {
			return fmt.Errorf("%s: %w", evidencePath, err)
		}

		refs, _ := evidence["artifact_refs"].([]any)
		found := false
		for _, raw := range refs {
			ref, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if stringValue(ref["artifact_id"]) == "provider-preflight" && stringValue(ref["sha256"]) == recordDigest {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("provider-live evidence %s: no artifact_refs entry names provider-preflight with sha256 %s", stringValue(entry["id"]), recordDigest)
		}

		approval, _ := record["approval"].(map[string]any)
		approvedAt, err := time.Parse(time.RFC3339, stringValue(approval["approved_at"]))
		if err != nil {
			return fmt.Errorf("%s: approval.approved_at: %w", recordPath, err)
		}
		observedAt, err := time.Parse(time.RFC3339, stringValue(evidence["observed_at"]))
		if err != nil {
			return fmt.Errorf("%s: observed_at: %w", evidencePath, err)
		}
		if !approvedAt.Before(observedAt) {
			return fmt.Errorf("provider-live evidence %s: approval.approved_at %s is not strictly before observed_at %s", stringValue(entry["id"]), approvedAt, observedAt)
		}
	}
	return nil
}
