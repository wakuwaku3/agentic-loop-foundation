package contracts

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// requiredPreflightResources is the minimum resource coverage dp-v2-012 d5
// requires every preflight record's limits array to name.
var requiredPreflightResources = []string{
	"firestore-reads-per-day",
	"firestore-writes-per-day",
	"firestore-deletes-per-day",
	"firestore-stored-bytes",
	"cloud-run-instance-seconds-per-month",
	"cloud-run-requests-per-month",
	"cloud-scheduler-jobs",
}

// validateDeploymentPreflight enforces the parts of dp-v2-012 d5 that are not
// expressible in the schema's restricted keyword subset: the live-project
// naming rule and the minimum resource coverage of limits. It is invoked by
// Validate for every value whose kind is "deployment-preflight", so it also
// runs for the fixtures in TestSchemasAndFixtures.
func validateDeploymentPreflight(record map[string]any) error {
	target, _ := record["target"].(map[string]any)
	if target != nil && stringValue(target["environment"]) == "live" {
		projectRef := stringValue(target["project_ref"])
		installationID := stringValue(target["installation_id"])
		if !strings.HasPrefix(projectRef, "al-") {
			return fmt.Errorf("/target/project_ref: must start with al- for a live environment")
		}
		if !hasHyphenSegment(projectRef, installationID) {
			return fmt.Errorf("/target/project_ref: must contain installation_id %q as a hyphen-delimited segment", installationID)
		}
	}
	limits, _ := record["limits"].([]any)
	seen := map[string]bool{}
	for _, raw := range limits {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		seen[stringValue(entry["resource"])] = true
	}
	var missing []string
	for _, want := range requiredPreflightResources {
		if !seen[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("/limits: missing required resource coverage: %s", strings.Join(missing, ", "))
	}
	return nil
}

// hasHyphenSegment reports whether needle appears as a contiguous run of
// hyphen-delimited segments inside haystack, e.g. installation_id
// "acme-corp" inside project_ref "al-acme-corp-live".
func hasHyphenSegment(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	hSegs := strings.Split(haystack, "-")
	nSegs := strings.Split(needle, "-")
	if len(nSegs) == 0 || len(nSegs) > len(hSegs) {
		return false
	}
	for start := 0; start+len(nSegs) <= len(hSegs); start++ {
		match := true
		for i, seg := range nSegs {
			if hSegs[start+i] != seg {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// CheckDeploymentPreflightLedger enforces the dp-v2-012 d5 invariant that
// human approval precedes any side effect a "gcp-live-" evidence component
// reports. root must contain .agents/v2/preflight/ (which may be absent or
// empty: that is a pass) and .agents/v2/evidence/index.json.
//
// For every file under <root>/.agents/v2/preflight/*.json: it must validate
// against contracts/schemas/deployment-preflight.json, and its filename must
// begin with its own task_id.
//
// For every entry of the evidence index whose component begins with
// "gcp-live-": exactly one preflight record must share its task_id; the
// evidence record itself (read from entry's path) must carry an
// artifact_refs entry whose artifact_id is "deployment-preflight" and whose
// sha256 equals the sha256 of that preflight record file; and the record's
// approval.approved_at must be strictly earlier than the evidence's
// observed_at.
func CheckDeploymentPreflightLedger(root string) error {
	schemaPath := filepath.Join(root, "contracts", "schemas", "deployment-preflight.json")
	schema, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("reading deployment-preflight schema: %w", err)
	}
	preflightDir := filepath.Join(root, ".agents", "v2", "preflight")
	dirEntries, err := os.ReadDir(preflightDir)
	if err != nil {
		if os.IsNotExist(err) {
			dirEntries = nil
		} else {
			return fmt.Errorf("reading %s: %w", preflightDir, err)
		}
	}

	byTaskID := map[string][]string{} // task_id -> preflight file paths
	byPath := map[string]map[string]any{}
	for _, e := range dirEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(preflightDir, e.Name())
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
		if !strings.HasPrefix(component, "gcp-live-") {
			continue
		}
		taskID := stringValue(entry["task_id"])
		matches := byTaskID[taskID]
		if len(matches) != 1 {
			return fmt.Errorf("gcp-live evidence %s (task_id %s): expected exactly one preflight record, found %d", stringValue(entry["id"]), taskID, len(matches))
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
			if stringValue(ref["artifact_id"]) == "deployment-preflight" && stringValue(ref["sha256"]) == recordDigest {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("gcp-live evidence %s: no artifact_refs entry names deployment-preflight with sha256 %s", stringValue(entry["id"]), recordDigest)
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
			return fmt.Errorf("gcp-live evidence %s: approval.approved_at %s is not strictly before observed_at %s", stringValue(entry["id"]), approvedAt, observedAt)
		}
	}
	return nil
}
