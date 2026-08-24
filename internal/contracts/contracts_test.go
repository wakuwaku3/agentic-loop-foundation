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

// TestCanonicalTaskStateMigration pins the historical fact of the v1 -> v2
// canonical task-state migration: it does not know or care how far the DAG
// has grown since. It asserts a *floor* of IDs (the migration set plus the
// V2-007/V2-023/V2-024/V2-025 repair chain) is still present, pins the three
// exhausted terminal failures byte-for-byte via sha256 digest, and pins the
// repair chain that resolved them. Ongoing-work assertions (V2-006/008/009
// status, owner, dependencies) do not belong here: they change as the DAG
// progresses and are covered instead by TestCanonicalTaskStateInvariants.
func TestCanonicalTaskStateMigration(t *testing.T) {
	root := filepath.Join("..", "..")
	stateDir := filepath.Join(root, ".agents", "v2", "task-state")
	paths, err := filepath.Glob(filepath.Join(stateDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, id := range taskIDsFromPaths(paths) {
		present[id] = true
	}
	floorIDs := []string{"V2-000", "V2-001", "V2-002", "V2-003", "V2-004", "V2-005", "V2-006", "V2-007", "V2-008", "V2-009", "V2-023", "V2-024", "V2-025"}
	for _, id := range floorIDs {
		if !present[id] {
			t.Errorf("canonical task-state floor is missing %s", id)
		}
	}

	readState := func(id string) map[string]any {
		return readJSON(t, filepath.Join(stateDir, id+".json"))
	}

	// Terminal failed tasks are historical fact once retries are exhausted:
	// pin the whole file byte-for-byte so no field can silently drift.
	terminalDigests := map[string]string{
		"V2-007": "b7995e365c28e8dd0f414cda417d5f73af235d5784674af3ecce2f1ea69a5981",
		"V2-023": "abb272d1df88e8f91f6873627ebfa990492c62f0dd2fc5dd9e13189e120dc305",
		"V2-024": "e98dc5dea32671e0205868bbf3603588b537e703710eada5e863685609a7d902",
	}
	for id, want := range terminalDigests {
		bytes := mustRead(t, filepath.Join(stateDir, id+".json"))
		if got := fmt.Sprintf("%x", sha256.Sum256(bytes)); got != want {
			t.Errorf("%s digest = %s, want %s (terminal failed files must not be edited)", id, got, want)
		}
	}

	v24 := readState("V2-024")
	assertStringSlice(t, v24["repair_of"], []string{"V2-007", "V2-023"})

	v25 := readState("V2-025")
	assertStringSlice(t, v25["repair_of"], []string{"V2-024"})
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

// TestCanonicalTaskStateInvariants checks every file under
// .agents/v2/task-state/ against structural invariants that must hold no
// matter how far the DAG has grown. Unlike TestCanonicalTaskStateMigration
// (a fixed historical floor), this test scales with the DAG: adding a new
// task-state file that respects the invariants requires no test change.
func TestCanonicalTaskStateInvariants(t *testing.T) {
	root := filepath.Join("..", "..")
	stateDir := filepath.Join(root, ".agents", "v2", "task-state")
	schemaPath := filepath.Join(root, "contracts", "schemas", "task-state.json")
	paths, err := filepath.Glob(filepath.Join(stateDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no task-state files found")
	}

	states := map[string]map[string]any{}
	for _, path := range paths {
		bytes := mustRead(t, path)
		// 1. schema-valid.
		if err := ValidateFile(schemaPath, bytes); err != nil {
			t.Errorf("%s: schema invalid: %v", path, err)
		}
		state := readJSON(t, path)
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		// 2. filename == task_id, id == "ts-" + lowercase(task_id).
		if state["task_id"] != id {
			t.Errorf("%s: task_id = %v, want %s", path, state["task_id"], id)
		}
		if want := "ts-" + strings.ToLower(id); state["id"] != want {
			t.Errorf("%s: id = %v, want %s", path, state["id"], want)
		}
		states[id] = state
	}

	// 3. dependencies and repair_of reference existing files, no
	// self-reference, no cycle across either edge type.
	assertDependencyDAG(t, states)

	for id, state := range states {
		assertTransitionsConsistent(t, id, state)        // 4.
		assertRetryBudgetInvariant(t, id, state)         // 5.
		assertTerminalNextOwnerNil(t, id, state)         // 6.
		assertBlockReasonInvariant(t, id, state, states) // 7.
		assertReleaseNotEligible(t, id, state)           // 9.
	}

	assertSupersededDependencies(t, states)    // 8.
	assertNoCompleteDependsOnFailed(t, states) // 10.
}

var transitionProjectionFields = []string{"actor", "owner", "next_owner", "retry_budget", "input_refs", "input_hash", "output_refs", "output_hash"}

// assertTransitionsConsistent checks (4): sequence is 1..n, from_status
// chains onto the previous to_status (first is null), and the top-level
// projection matches the final transition.
func assertTransitionsConsistent(t *testing.T, id string, state map[string]any) {
	t.Helper()
	transitions := state["transitions"].([]any)
	var prevTo any
	for i, raw := range transitions {
		tr := raw.(map[string]any)
		if seq, ok := tr["sequence"].(float64); !ok || int(seq) != i+1 {
			t.Errorf("%s: transition %d sequence = %v, want %d", id, i, tr["sequence"], i+1)
		}
		if i == 0 {
			if tr["from_status"] != nil {
				t.Errorf("%s: transition 0 from_status = %v, want nil", id, tr["from_status"])
			}
		} else if !reflect.DeepEqual(tr["from_status"], prevTo) {
			t.Errorf("%s: transition %d from_status = %v, want %v", id, i, tr["from_status"], prevTo)
		}
		prevTo = tr["to_status"]
	}
	last := transitions[len(transitions)-1].(map[string]any)
	if !reflect.DeepEqual(state["status"], last["to_status"]) {
		t.Errorf("%s: status = %v, want last transition to_status %v", id, state["status"], last["to_status"])
	}
	for _, field := range transitionProjectionFields {
		if !reflect.DeepEqual(state[field], last[field]) {
			t.Errorf("%s: %s = %v, want last transition value %v", id, field, state[field], last[field])
		}
	}
}

// assertRetryBudgetInvariant checks (5).
func assertRetryBudgetInvariant(t *testing.T, id string, state map[string]any) {
	t.Helper()
	budget := state["retry_budget"].(map[string]any)
	max := budget["max_attempts"].(float64)
	used := budget["attempts_used"].(float64)
	remaining := budget["remaining"].(float64)
	if remaining != max-used {
		t.Errorf("%s: retry_budget.remaining = %v, want %v (max-used)", id, remaining, max-used)
	}
	if used < 0 || used > max {
		t.Errorf("%s: retry_budget.attempts_used = %v out of [0,%v]", id, used, max)
	}
	if state["status"] == "failed" && remaining != 0 {
		t.Errorf("%s: failed task must have retry_budget.remaining == 0, got %v", id, remaining)
	}
}

// assertTerminalNextOwnerNil checks (6).
func assertTerminalNextOwnerNil(t *testing.T, id string, state map[string]any) {
	t.Helper()
	status := state["status"]
	if (status == "complete" || status == "failed") && state["next_owner"] != nil {
		t.Errorf("%s: status %v must have next_owner == null, got %v", id, status, state["next_owner"])
	}
}

// assertReleaseNotEligible checks (9).
func assertReleaseNotEligible(t *testing.T, id string, state map[string]any) {
	t.Helper()
	if state["release_eligible"] != false {
		t.Errorf("%s: release_eligible = %v, want false", id, state["release_eligible"])
	}
}

// assertBlockReasonInvariant checks (7): a blocked task must carry a
// block_reason. An "external-unavailable:" reason requires next_owner ==
// "sol" (a human is needed, so only Sol may hold it). Otherwise block_reason
// is a comma-separated task_id list: every referenced file must exist and at
// least one must not be complete (otherwise the block should have cleared).
func assertBlockReasonInvariant(t *testing.T, id string, state map[string]any, states map[string]map[string]any) {
	t.Helper()
	if state["status"] != "blocked" {
		return
	}
	reason, _ := state["block_reason"].(string)
	if reason == "" {
		t.Errorf("%s: status blocked requires a non-empty block_reason", id)
		return
	}
	if strings.HasPrefix(reason, "external-unavailable:") {
		if state["next_owner"] != "sol" {
			t.Errorf("%s: external-unavailable block_reason requires next_owner == sol, got %v", id, state["next_owner"])
		}
		return
	}
	anyIncomplete := false
	for _, ref := range strings.Split(reason, ",") {
		ref = strings.TrimSpace(ref)
		referenced, ok := states[ref]
		if !ok {
			t.Errorf("%s: block_reason references absent task %s", id, ref)
			continue
		}
		if referenced["status"] != "complete" {
			anyIncomplete = true
		}
	}
	if !anyIncomplete {
		t.Errorf("%s: block_reason %q has no incomplete referenced task; block should have cleared", id, reason)
	}
}

// supersededTaskIDs returns the set of task_ids referenced from either the
// repair_of or input_refs of any complete task: the definition of
// "superseded" used by (8) and the DAG doc.
func supersededTaskIDs(states map[string]map[string]any) map[string]bool {
	superseded := map[string]bool{}
	for _, state := range states {
		if state["status"] != "complete" {
			continue
		}
		for _, field := range []string{"repair_of", "input_refs"} {
			raw, ok := state[field]
			if !ok {
				continue
			}
			for _, v := range raw.([]any) {
				superseded[v.(string)] = true
			}
		}
	}
	return superseded
}

// assertSupersededDependencies checks (8): a queued or running task may not
// depend on a failed task unless that failed task is superseded.
func assertSupersededDependencies(t *testing.T, states map[string]map[string]any) {
	t.Helper()
	superseded := supersededTaskIDs(states)
	for id, state := range states {
		if state["status"] != "queued" && state["status"] != "running" {
			continue
		}
		deps, ok := state["dependencies"].([]any)
		if !ok {
			continue
		}
		for _, raw := range deps {
			dep := raw.(string)
			depState, ok := states[dep]
			if !ok || depState["status"] != "failed" {
				continue
			}
			if !superseded[dep] {
				t.Errorf("%s: depends on failed %s which is not superseded by any complete task", id, dep)
			}
		}
	}
}

// assertNoCompleteDependsOnFailed checks (10).
func assertNoCompleteDependsOnFailed(t *testing.T, states map[string]map[string]any) {
	t.Helper()
	for id, state := range states {
		if state["status"] != "complete" {
			continue
		}
		deps, ok := state["dependencies"].([]any)
		if !ok {
			continue
		}
		for _, raw := range deps {
			dep := raw.(string)
			if depState, ok := states[dep]; ok && depState["status"] == "failed" {
				t.Errorf("%s: complete task depends on failed %s", id, dep)
			}
		}
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
		// 11. every index entry's task_id must point to a real task-state file.
		taskID, _ := entry["task_id"].(string)
		taskStatePath := filepath.Join(root, ".agents", "v2", "task-state", taskID+".json")
		if _, err := os.Stat(taskStatePath); err != nil {
			t.Errorf("active evidence entry %s references task_id %q with no task-state file: %v", path, taskID, err)
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

// assertDependencyDAG walks both the "dependencies" and "repair_of" edges of
// every task-state: every referenced task_id must exist, no task may
// reference itself, and the combined graph must be acyclic.
func assertDependencyDAG(t *testing.T, states map[string]map[string]any) {
	t.Helper()
	edgesOf := func(state map[string]any) []string {
		var out []string
		for _, field := range []string{"dependencies", "repair_of"} {
			raw, ok := state[field]
			if !ok {
				continue
			}
			for _, v := range raw.([]any) {
				out = append(out, v.(string))
			}
		}
		return out
	}
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
		for _, ref := range edgesOf(states[id]) {
			if ref == id {
				t.Errorf("%s: self-references %s", id, ref)
				continue
			}
			if states[ref] == nil {
				t.Errorf("%s depends on absent %s", id, ref)
				continue
			}
			visit(ref)
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
