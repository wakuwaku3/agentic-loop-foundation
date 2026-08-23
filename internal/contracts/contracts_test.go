package contracts

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestSchemasAndFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "contracts")
	schemaDir := filepath.Join(root, "schemas")
	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		schemaPath := filepath.Join(schemaDir, entry.Name())
		schema, err := os.ReadFile(schemaPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed any
		if err := json.Unmarshal(schema, &parsed); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		valid := filepath.Join(root, "fixtures", "valid", entry.Name())
		invalid := filepath.Join(root, "fixtures", "invalid", entry.Name())
		for _, tc := range []struct {
			path string
			want bool
		}{{valid, true}, {invalid, false}} {
			value, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("%s: %v", tc.path, err)
			}
			err = Validate(schema, value, ResolveSchemaRef(schemaDir))
			if (err == nil) != tc.want {
				t.Errorf("%s fixture %s: want valid=%v, got %v", name, tc.path, tc.want, err)
			}
		}
	}
}

func TestCanonicalTaskStateMigration(t *testing.T) {
	root := filepath.Join("..", "..")
	stateDir := filepath.Join(root, ".agents", "v2", "task-state")
	paths, err := filepath.Glob(filepath.Join(stateDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	expectedIDs := []string{"V2-000", "V2-001", "V2-002", "V2-003", "V2-004", "V2-005", "V2-006", "V2-007", "V2-008", "V2-009", "V2-023", "V2-024", "V2-025"}
	if got := taskIDsFromPaths(paths); !reflect.DeepEqual(got, expectedIDs) {
		t.Fatalf("canonical task-state files = %v, want %v", got, expectedIDs)
	}
	states := map[string]map[string]any{}
	for _, path := range paths {
		state := readJSON(t, path)
		if err := ValidateFile(filepath.Join(root, "contracts", "schemas", "task-state.json"), mustRead(t, path)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if state["task_id"] != id || state["id"] != "ts-"+strings.ToLower(id) {
			t.Errorf("%s: filename/id/task_id mismatch", path)
		}
		states[id] = state
	}
	assertDependencyDAG(t, states)
	assertStringSlice(t, states["V2-006"]["dependencies"], []string{"V2-025", "V2-008", "V2-009"})
	if states["V2-006"]["next_owner"] != "sol" {
		t.Error("V2-006 must be blocked for sol")
	}
	for _, id := range []string{"V2-008", "V2-009"} {
		if states[id]["status"] != "blocked" || states[id]["next_owner"] != "terra" {
			t.Errorf("%s must be blocked for terra", id)
		}
		assertStringSlice(t, states[id]["dependencies"], []string{"V2-025"})
	}
	for _, id := range []string{"V2-007", "V2-023"} {
		state := states[id]
		if state["status"] != "failed" || state["next_owner"] != nil || len(state["transitions"].([]any)) != 1 {
			t.Errorf("%s must be an exhausted terminal failure", id)
		}
	}
	assertBudget(t, states["V2-007"], 2, 2, 0)
	assertBudget(t, states["V2-023"], 1, 1, 0)
	v24 := states["V2-024"]
	assertStringSlice(t, v24["dependencies"], []string{})
	assertStringSlice(t, v24["repair_of"], []string{"V2-007", "V2-023"})
	if v24["status"] != "failed" || v24["actor"] != "terra" || v24["next_owner"] != nil {
		t.Error("V2-024 must be a terminal terra failure")
	}
	assertBudget(t, v24, 1, 1, 0)
	v25 := states["V2-025"]
	assertStringSlice(t, v25["dependencies"], []string{})
	assertStringSlice(t, v25["repair_of"], []string{"V2-024"})
	assertBudget(t, v25, 1, 1, 0)
	transitions := v25["transitions"].([]any)
	if got := []string{transitions[0].(map[string]any)["to_status"].(string), transitions[1].(map[string]any)["to_status"].(string), transitions[2].(map[string]any)["to_status"].(string)}; !reflect.DeepEqual(got, []string{"queued", "running", "complete"}) {
		t.Errorf("V2-025 transitions = %v", got)
	}
	assertStringSlice(t, v25["output_refs"], []string{"V2-025", "ev-v2-025-contracts"})
	if _, err := os.Stat(filepath.Join(root, ".agents", "v2", "records")); !os.IsNotExist(err) {
		Fatalf(t, "legacy records path remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "v2", "task-ledger.yaml")); !os.IsNotExist(err) {
		Fatalf(t, "legacy task ledger remains: %v", err)
	}
}

func TestHistoricalBootstrapRecordsAndIndexes(t *testing.T) {
	root := filepath.Join("..", "..")
	historicalDir := filepath.Join(root, ".agents", "v2", "historical", "bootstrap-records")
	paths, err := filepath.Glob(filepath.Join(historicalDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	expectedDigests := map[string]string{
		"V2-000-evidence.json": "8dc92ee4a194bb390d698a97706e09d2e441697faebca6421dc57c94157d442e", "V2-000-task-record.json": "e530ca98b6a52e5b3afa378740817e73736a6f09a5948633b9b8da6db48e7e8a", "V2-000-work-packet.json": "f0d8edd0bfc2da746593dab03afee7920008a701a2a869596b32d6c1338bf42a",
		"V2-001-evidence.json": "f27082d06128c4d07bc7c9e5cc127a8c1c5ca9f74fb52f574d4ba9fdc5b99678", "V2-001-task-record.json": "9e55394217a60acbf5cc38f0ad45ca50e76f471dc321ac80e8eebfdc9c67f566", "V2-001-work-packet.json": "994395bb42830c1c3cc3aa3be183c11d9487f5dc23ea541ded0bce266717f9b9",
		"V2-002-evidence.json": "679c8aff199af16bec43b542c83ec4a47e11f920231c78d5125829f00d7a5076", "V2-002-task-record.json": "82c0e3f982691530a552a7c937bde94fc0ee65d83d3dce2b9491dc1688e067bb", "V2-002-work-packet.json": "7f05a88f3a4f8d95d43daac0345fcd83becfb9255612fea051cb8961642fb50b",
		"V2-003-evidence.json": "4a049cef0bcef7d26b1fa4aa3e47b253296c86260379b50a7aa2a18b22da40d2", "V2-003-task-record.json": "2615554ebb830a476afb3e3590a79c2a97b15b3cd1d96832892a923d61bb6d30", "V2-003-work-packet.json": "7b1f6f42a2edc27b6555ccc050a601ba22a33dac697142f6d542ec4d29cb8f56",
	}
	if len(paths) != len(expectedDigests) {
		t.Fatalf("historical records = %d, want %d", len(paths), len(expectedDigests))
	}
	for _, path := range paths {
		name := filepath.Base(path)
		bytes := mustRead(t, path)
		if got := fmt.Sprintf("%x", sha256.Sum256(bytes)); got != expectedDigests[name] {
			t.Errorf("%s digest = %s", name, got)
		}
		state := readJSON(t, path)
		taskID := strings.Split(name, "-")[0] + "-" + strings.Split(name, "-")[1]
		if state["task_id"] != taskID {
			t.Errorf("%s task_id = %v", name, state["task_id"])
		}
		var schema string
		switch {
		case strings.HasSuffix(name, "-evidence.json"):
			schema = "evidence.json"
		case strings.HasSuffix(name, "-task-record.json"):
			schema = "task-record.json"
		case strings.HasSuffix(name, "-work-packet.json"):
			schema = "work-packet.json"
		default:
			t.Fatalf("unexpected historical record %s", name)
		}
		if err := ValidateFile(filepath.Join(root, "contracts", "schemas", schema), bytes); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	historicalIndexPath := filepath.Join(root, ".agents", "v2", "evidence", "historical", "index.json")
	indexBytes := mustRead(t, historicalIndexPath)
	if err := ValidateFile(filepath.Join(root, "contracts", "schemas", "historical-evidence-index.json"), indexBytes); err != nil {
		t.Fatal(err)
	}
	index := readJSON(t, historicalIndexPath)
	entries := index["entries"].([]any)
	if len(entries) != 4 {
		t.Fatalf("historical evidence entries = %d, want 4", len(entries))
	}
	seen := map[string]bool{}
	for _, raw := range entries {
		entry := raw.(map[string]any)
		path := filepath.Join(root, entry["source_path"].(string))
		bytes := mustRead(t, path)
		artifact := readJSON(t, path)
		if entry["id"] != artifact["id"] || entry["task_id"] != artifact["task_id"] || entry["sha256"] != fmt.Sprintf("%x", sha256.Sum256(bytes)) || seen[path] {
			t.Errorf("historical index entry does not bijectively identify %s", path)
		}
		seen[path] = true
	}
	if len(seen) != 4 {
		t.Error("historical evidence index is not a four-artifact bijection")
	}
}

func TestActiveEvidenceIndex(t *testing.T) {
	root := filepath.Join("..", "..")
	indexPath := filepath.Join(root, ".agents", "v2", "evidence", "index.json")
	indexBytes := mustRead(t, indexPath)
	if err := ValidateFile(filepath.Join(root, "contracts", "schemas", "evidence-index.json"), indexBytes); err != nil {
		t.Fatal(err)
	}
	index := readJSON(t, indexPath)
	entries := index["entries"].([]any)
	paths, err := filepath.Glob(filepath.Join(root, ".agents", "v2", "evidence", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]bool{}
	for _, path := range paths {
		if filepath.Base(path) != "index.json" {
			actual[filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))] = true
		}
	}
	indexed := map[string]bool{}
	for _, raw := range entries {
		entry := raw.(map[string]any)
		path := entry["path"].(string)
		artifactPath := filepath.Join(root, path)
		artifact := mustRead(t, artifactPath)
		if err := ValidateFile(filepath.Join(root, "contracts", "schemas", "evidence.json"), artifact); err != nil {
			t.Errorf("%s: %v", path, err)
		}
		parsed := readJSON(t, artifactPath)
		if entry["id"] != parsed["id"] || entry["task_id"] != parsed["task_id"] || entry["evidence_hash"] != fmt.Sprintf("%x", sha256.Sum256(artifact)) {
			t.Errorf("active evidence index mismatch for %s", path)
		}
		indexed[path] = true
	}
	if !reflect.DeepEqual(actual, indexed) {
		t.Errorf("active evidence paths = %v, indexed = %v", actual, indexed)
	}
}

func TestCanonicalTaskStateNegativeMatrix(t *testing.T) {
	root := filepath.Join("..", "..")
	schema := mustRead(t, filepath.Join(root, "contracts", "schemas", "task-state.json"))
	valid := readJSON(t, filepath.Join(root, "contracts", "fixtures", "valid", "task-state.json"))
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"top-level projection", func(s map[string]any) { s["status"] = "blocked" }},
		{"reference digest", func(s map[string]any) {
			s["transitions"].([]any)[2].(map[string]any)["input_hash"] = strings.Repeat("0", 64)
		}},
		{"empty reference", func(s map[string]any) { s["transitions"].([]any)[1].(map[string]any)["input_refs"] = []any{""} }},
		{"duplicate reference", func(s map[string]any) {
			s["transitions"].([]any)[1].(map[string]any)["input_refs"] = []any{"V2-024", "V2-024"}
		}},
		{"non-contiguous sequence", func(s map[string]any) { s["transitions"].([]any)[1].(map[string]any)["sequence"] = float64(3) }},
		{"disconnected transition", func(s map[string]any) { s["transitions"].([]any)[1].(map[string]any)["from_status"] = "blocked" }},
		{"illegal transition", func(s map[string]any) {
			s["transitions"].([]any)[1].(map[string]any)["to_status"] = "complete"
			s["transitions"].([]any)[2].(map[string]any)["from_status"] = "complete"
		}},
		{"complete terminal owner", func(s map[string]any) {
			s["next_owner"] = "terra"
			s["transitions"].([]any)[2].(map[string]any)["next_owner"] = "terra"
		}},
		{"failed terminal transition", addFailedExit},
		{"retry budget", func(s map[string]any) { s["retry_budget"].(map[string]any)["remaining"] = float64(1) }},
		{"self dependency", func(s map[string]any) { s["dependencies"] = []any{"V2-024"} }},
		{"duplicate repair reference", func(s map[string]any) { s["repair_of"] = []any{"V2-007", "V2-007"} }},
	}
	if len(cases) != 12 {
		t.Fatal("negative matrix must retain twelve categories")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := cloneJSON(t, valid)
			tc.mutate(state)
			bytes, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(schema, bytes, ResolveSchemaRef(filepath.Join(root, "contracts", "schemas"))); err == nil {
				t.Fatal("invalid state accepted")
			}
		})
	}
}

func addFailedExit(state map[string]any) {
	transitions := state["transitions"].([]any)
	last := transitions[2].(map[string]any)
	last["to_status"] = "failed"
	last["next_owner"] = nil
	state["status"] = "queued"
	state["next_owner"] = "terra"
	queued := cloneMap(last)
	queued["sequence"] = float64(4)
	queued["from_status"] = "failed"
	queued["to_status"] = "queued"
	queued["next_owner"] = "terra"
	queued["reason"] = "invalid retry"
	transitions = append(transitions, queued)
	state["transitions"] = transitions
}

func assertDependencyDAG(t *testing.T, states map[string]map[string]any) {
	t.Helper()
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if visiting[id] {
			t.Fatalf("dependency cycle at %s", id)
		}
		if done[id] {
			return
		}
		visiting[id] = true
		for _, raw := range states[id]["dependencies"].([]any) {
			dependency := raw.(string)
			if states[dependency] == nil {
				t.Errorf("%s depends on absent %s", id, dependency)
				continue
			}
			visit(dependency)
		}
		delete(visiting, id)
		done[id] = true
	}
	for id := range states {
		visit(id)
	}
}

func assertStringSlice(t *testing.T, raw any, want []string) {
	t.Helper()
	values := raw.([]any)
	got := make([]string, len(values))
	for i, value := range values {
		got[i] = value.(string)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertBudget(t *testing.T, state map[string]any, max, used, remaining float64) {
	t.Helper()
	budget := state["retry_budget"].(map[string]any)
	if budget["max_attempts"] != max || budget["attempts_used"] != used || budget["remaining"] != remaining {
		t.Errorf("retry budget = %v", budget)
	}
}

func taskIDsFromPaths(paths []string) []string {
	ids := make([]string, 0, len(paths))
	for _, path := range paths {
		ids = append(ids, strings.TrimSuffix(filepath.Base(path), ".json"))
	}
	sort.Strings(ids)
	return ids
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(mustRead(t, path), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func cloneJSON(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	bytes, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var copied map[string]any
	if err := json.Unmarshal(bytes, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

func cloneMap(value map[string]any) map[string]any {
	copy := make(map[string]any, len(value))
	for key, item := range value {
		copy[key] = item
	}
	return copy
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes
}

func ValidateFile(schemaPath string, value []byte) error {
	schema, err := os.ReadFile(schemaPath)
	if err != nil {
		return err
	}
	return Validate(schema, value, ResolveSchemaRef(filepath.Dir(schemaPath)))
}

func Fatalf(t *testing.T, format string, args ...any) { t.Helper(); t.Fatalf(format, args...) }

func TestSchemasRejectSensitiveFieldNames(t *testing.T) {
	root := filepath.Join("..", "..", "contracts", "schemas")
	var paths []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && strings.HasSuffix(path, ".json") {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	for _, path := range paths {
		b, _ := os.ReadFile(path)
		text := strings.ToLower(string(b))
		for _, forbidden := range []string{"password", "credential", "raw_prompt", "raw_provider_output"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains forbidden field name %q", path, forbidden)
			}
		}
	}
}
