package contracts_test

// A claimed capability evidence id and a real passed exercise, linked in both
// directions and machine-checked (wo-v2-022 A15, dp-v2-022 d8).
//
// Clause (f) of TestFoundationReleaseContractBaseline already proves that an
// evidence id written into contracts/release-contract/foundation.json resolves
// to some entry in .agents/v2/evidence/index.json. It does not prove that the
// record that entry names says anything at all about the capability that cites
// it, and it does not prove the converse: that a capability actually observed
// succeeding was not quietly left out of the contract.
//
// Both directions are enforced here:
//
//	forward  every capability with a non-empty evidence_ids must have, in every
//	         record it references, a checks entry whose name is exactly that
//	         capability id and whose status is passed.
//	reverse  every check in ANY evidence record the index names whose name
//	         equals one of the contract's capability ids and whose status is
//	         passed must be referenced back by that capability's evidence_ids.
//
// The reverse direction is the more valuable half: it makes it impossible to
// exercise a capability successfully and then under-report it, which is the
// failure mode that would let a later release claim less than it had proved
// while still passing every existing check.
//
// V2-095 WIDENED THE REVERSE RULE'S REACH, AND MEASURED FIRST THAT DOING SO IS
// NON-BREAKING (wo-v2-095 A5, dp-v2-095 d6). Until then capabilityEvidenceReverse
// took an onlyTaskID parameter and every caller passed the literal "V2-022",
// and the record-collection loop below only loaded records whose task_id was
// V2-022. So the half the header calls "the more valuable" one did not reach
// any record issued after V2-022 at all: a later task could exercise a
// capability, report it passed, omit it from the contract, and `make check`
// would stay green.
//
// The measurement that made the widening safe, re-run in V2-095's worktree with
//
//	python3 - <<'EOF'
//	scan every entry of .agents/v2/evidence/index.json, read the record it
//	names, and count the checks whose name equals one of the twelve declared
//	capability ids
//	EOF
//
// over all 185 pre-existing index entries: EXACTLY ONE record carries any such
// check -- ev-v2-022-release-live-dogfood, with 12 of them, 1 passed and 11
// failed. Every other record, including all five provider-live ones, carries
// zero. So removing the scope adds coverage and zero new violations, which the
// real-tree test below then re-confirms by execution.
//
// The scope was REMOVED rather than widened to a two-element set on purpose: a
// guard that must be edited to keep working is a guard that will stop working,
// and the next live record would have needed a third edit.
//
// NO ASSERTION WAS DROPPED. The two negative controls that existed before are
// kept verbatim except for the removed parameter, and a third control
// (TestCapabilityEvidenceReverseReachesRecordsBeyondV2022) drives the widened
// rule with a synthetic record whose task id is NOT V2-022 and asserts exactly
// one violation, plus the positive half where citing it yields zero. A
// generalisation without that control is vacuous.
//
// internal/contracts/release_contract_test.go and
// internal/contracts/contracts_test.go are untouched.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// capabilityEvidenceIndex is the subset of the evidence ledger these two
// directions need: which record file each evidence id names.
type capabilityEvidenceIndex map[string]string // evidence id -> repository-relative record path

// capabilityEvidenceRecord is the subset of one evidence record they need.
type capabilityEvidenceRecord struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Checks []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"checks"`
}

func capabilityEvidenceReadIndex(t *testing.T, root string) capabilityEvidenceIndex {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".agents", "v2", "evidence", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Entries []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	out := capabilityEvidenceIndex{}
	for _, e := range index.Entries {
		out[e.ID] = e.Path
	}
	return out
}

func capabilityEvidenceReadRecord(t *testing.T, root, rel string) capabilityEvidenceRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	var record capabilityEvidenceRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// capabilityClaims is the contract side: capability id -> the evidence ids it
// cites, in declaration order.
type capabilityClaims struct {
	order  []string
	cites  map[string][]string
	byID   map[string]bool
	source string
}

func capabilityEvidenceReadContract(t *testing.T, root string) capabilityClaims {
	t.Helper()
	path := filepath.Join(root, "contracts", "release-contract", "foundation.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Capabilities []struct {
			ID          string   `json:"id"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	claims := capabilityClaims{cites: map[string][]string{}, byID: map[string]bool{}, source: path}
	for _, c := range contract.Capabilities {
		if c.ID == "" {
			t.Fatalf("%s: a capability has no id", path)
		}
		claims.order = append(claims.order, c.ID)
		claims.byID[c.ID] = true
		claims.cites[c.ID] = append([]string(nil), c.EvidenceIDs...)
	}
	if len(claims.order) == 0 {
		t.Fatalf("%s declares no capabilities", path)
	}
	return claims
}

// capabilityEvidenceForward is the forward rule as a pure function over the
// decoded inputs, so a negative control can drive it without a real tree.
// It returns the violations it found, newest complaint last.
func capabilityEvidenceForward(claims capabilityClaims, index capabilityEvidenceIndex, records map[string]capabilityEvidenceRecord) []string {
	var violations []string
	for _, capability := range claims.order {
		for _, evidenceID := range claims.cites[capability] {
			path, resolved := index[evidenceID]
			if !resolved {
				violations = append(violations, capability+": evidence id "+evidenceID+" does not resolve in the evidence index")
				continue
			}
			record, present := records[path]
			if !present {
				violations = append(violations, capability+": evidence record "+path+" could not be read")
				continue
			}
			passed := false
			for _, check := range record.Checks {
				if check.Name == capability && check.Status == "passed" {
					passed = true
				}
			}
			if !passed {
				violations = append(violations, capability+": record "+evidenceID+" carries no check named exactly "+capability+" with status passed")
			}
		}
	}
	return violations
}

// capabilityEvidenceReverse is the reverse rule as a pure function: a passed
// check whose name is a capability id must be cited back by that capability.
//
// It judges EVERY record it is handed. There is no task-id scope: V2-095
// removed the onlyTaskID parameter after measuring that exactly one existing
// record carries a capability-named check at all, so the removal adds coverage
// and no new violation. Which records are handed in is the caller's decision,
// and the real-tree test hands in every record the evidence index names.
func capabilityEvidenceReverse(claims capabilityClaims, records map[string]capabilityEvidenceRecord) []string {
	var violations []string
	paths := make([]string, 0, len(records))
	for path := range records {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		record := records[path]
		for _, check := range record.Checks {
			if check.Status != "passed" || !claims.byID[check.Name] {
				continue
			}
			cited := false
			for _, evidenceID := range claims.cites[check.Name] {
				if evidenceID == record.ID {
					cited = true
				}
			}
			if !cited {
				violations = append(violations, check.Name+": record "+record.ID+" reports it passed, but the capability's evidence_ids does not reference that record")
			}
		}
	}
	return violations
}

// TestCapabilityEvidenceLinkedBothWays is the real-tree conformance test.
func TestCapabilityEvidenceLinkedBothWays(t *testing.T) {
	root := filepath.Join("..", "..")
	claims := capabilityEvidenceReadContract(t, root)
	index := capabilityEvidenceReadIndex(t, root)

	// EVERY record the index names is read once, plus every record any
	// capability cites. The task-id filter that used to stand here -- keep the
	// record only if its TaskID equals "V2-022" -- was removed by V2-095 for
	// the reason the header records: it made the reverse rule blind to every
	// record issued after V2-022.
	records := map[string]capabilityEvidenceRecord{}
	read := func(path string) {
		if _, already := records[path]; already {
			return
		}
		records[path] = capabilityEvidenceReadRecord(t, root, path)
	}
	for _, capability := range claims.order {
		for _, evidenceID := range claims.cites[capability] {
			if path, ok := index[evidenceID]; ok {
				read(path)
			}
		}
	}
	for _, path := range index {
		read(path)
	}
	if len(records) < len(index) {
		t.Fatalf("the reverse rule was handed %d records for %d index entries; it must judge every record the index names", len(records), len(index))
	}

	if violations := capabilityEvidenceForward(claims, index, records); len(violations) != 0 {
		for _, v := range violations {
			t.Errorf("forward: %s", v)
		}
	}
	if violations := capabilityEvidenceReverse(claims, records); len(violations) != 0 {
		for _, v := range violations {
			t.Errorf("reverse: %s", v)
		}
	}
	t.Logf("the reverse rule judged %d records, every one the evidence index names, with zero violations", len(records))
}

// capabilityEvidenceSyntheticClaims is a two-capability contract stand-in used
// by both negative controls.
func capabilityEvidenceSyntheticClaims(cites map[string][]string) capabilityClaims {
	claims := capabilityClaims{
		order:  []string{"cap-alpha", "cap-beta"},
		cites:  map[string][]string{"cap-alpha": nil, "cap-beta": nil},
		byID:   map[string]bool{"cap-alpha": true, "cap-beta": true},
		source: "synthetic",
	}
	for id, list := range cites {
		claims.cites[id] = list
	}
	return claims
}

// TestCapabilityEvidenceForwardNegativeControl proves the forward direction
// can fail: a capability cites a record whose checks say nothing about it.
func TestCapabilityEvidenceForwardNegativeControl(t *testing.T) {
	claims := capabilityEvidenceSyntheticClaims(map[string][]string{"cap-alpha": {"ev-synthetic"}})
	index := capabilityEvidenceIndex{"ev-synthetic": "synthetic/record.json"}
	record := capabilityEvidenceRecord{ID: "ev-synthetic", TaskID: "V2-022"}
	record.Checks = append(record.Checks, struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{Name: "something-else", Status: "passed"})
	records := map[string]capabilityEvidenceRecord{"synthetic/record.json": record}

	violations := capabilityEvidenceForward(claims, index, records)
	if len(violations) != 1 {
		t.Fatalf("the forward rule reported %d violations, want exactly 1: %v", len(violations), violations)
	}

	// Positive half of the same control: add the missing check and the same
	// inputs pass, so the rule is not simply always failing.
	record.Checks = append(record.Checks, struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{Name: "cap-alpha", Status: "passed"})
	records["synthetic/record.json"] = record
	if violations := capabilityEvidenceForward(claims, index, records); len(violations) != 0 {
		t.Fatalf("the forward rule refused a record that does carry the capability's passed check: %v", violations)
	}
}

// TestCapabilityEvidenceReverseNegativeControl proves the reverse direction
// can fail: a record reports a capability passed and the contract does not
// cite that record.
func TestCapabilityEvidenceReverseNegativeControl(t *testing.T) {
	claims := capabilityEvidenceSyntheticClaims(nil)
	record := capabilityEvidenceRecord{ID: "ev-synthetic", TaskID: "V2-022"}
	record.Checks = append(record.Checks, struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{Name: "cap-beta", Status: "passed"})
	records := map[string]capabilityEvidenceRecord{"synthetic/record.json": record}

	violations := capabilityEvidenceReverse(claims, records)
	if len(violations) != 1 {
		t.Fatalf("the reverse rule reported %d violations, want exactly 1: %v", len(violations), violations)
	}

	// A check recorded failed is not a claim, so it must not be demanded back.
	failed := capabilityEvidenceRecord{ID: "ev-synthetic-failed", TaskID: "V2-022"}
	failed.Checks = append(failed.Checks, struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{Name: "cap-beta", Status: "failed"})
	if violations := capabilityEvidenceReverse(claims, map[string]capabilityEvidenceRecord{"synthetic/failed.json": failed}); len(violations) != 0 {
		t.Fatalf("the reverse rule demanded an evidence id for a check recorded failed: %v", violations)
	}

	// And once the contract cites the record, the same inputs pass.
	claims.cites["cap-beta"] = []string{"ev-synthetic"}
	if violations := capabilityEvidenceReverse(claims, records); len(violations) != 0 {
		t.Fatalf("the reverse rule refused a correctly cited record: %v", violations)
	}
}

// TestCapabilityEvidenceReverseReachesRecordsBeyondV2022 is the control that
// makes V2-095's removal of the task-id scope non-vacuous. It drives the
// reverse rule with a synthetic record whose task id is NOT V2-022, reporting a
// capability passed that the synthetic contract does not cite, and asserts
// EXACTLY ONE violation. Under the old scoped rule this record was skipped
// entirely and the same inputs produced zero violations, so this test would
// have failed before the widening -- which is what "non-vacuous" means here.
func TestCapabilityEvidenceReverseReachesRecordsBeyondV2022(t *testing.T) {
	claims := capabilityEvidenceSyntheticClaims(nil)
	record := capabilityEvidenceRecord{ID: "ev-synthetic-beyond", TaskID: "V2-095"}
	record.Checks = append(record.Checks, struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{Name: "cap-alpha", Status: "passed"})
	records := map[string]capabilityEvidenceRecord{"synthetic/beyond.json": record}

	violations := capabilityEvidenceReverse(claims, records)
	if len(violations) != 1 {
		t.Fatalf("the widened reverse rule reported %d violations for a non-V2-022 record reporting an uncited passed capability, want exactly 1: %v", len(violations), violations)
	}

	// A record with a task id from neither task is judged too: the rule has no
	// allowlist of task ids at all, which is the property the removal created.
	other := capabilityEvidenceRecord{ID: "ev-synthetic-other", TaskID: "V2-123"}
	other.Checks = append(other.Checks, struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{Name: "cap-beta", Status: "passed"})
	if got := capabilityEvidenceReverse(claims, map[string]capabilityEvidenceRecord{"synthetic/other.json": other}); len(got) != 1 {
		t.Fatalf("the widened reverse rule reported %d violations for a third task id, want exactly 1: %v", len(got), got)
	}

	// Positive half: once the contract cites the non-V2-022 record, the same
	// inputs pass. Without this the control could be satisfied by a rule that
	// always complains.
	claims.cites["cap-alpha"] = []string{"ev-synthetic-beyond"}
	if got := capabilityEvidenceReverse(claims, records); len(got) != 0 {
		t.Fatalf("the widened reverse rule refused a correctly cited non-V2-022 record: %v", got)
	}
}

// TestCapabilityEvidenceReverseSkippedIsNeverAPass is G7 at the record layer,
// asserted rather than assumed: a check recorded `skipped` -- which is how
// V2-095 represents ineligible-by-declaration, because
// contracts/schemas/evidence.json's status enum has no "ineligible" member --
// must NOT be demanded back as a claim, and must never be readable as a pass.
// This is the property that lets a deferred capability keep an empty
// evidence_ids array while the record still names it.
func TestCapabilityEvidenceReverseSkippedIsNeverAPass(t *testing.T) {
	claims := capabilityEvidenceSyntheticClaims(nil)
	skipped := capabilityEvidenceRecord{ID: "ev-synthetic-skipped", TaskID: "V2-095"}
	skipped.Checks = append(skipped.Checks, struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{Name: "cap-beta", Status: "skipped"})
	if got := capabilityEvidenceReverse(claims, map[string]capabilityEvidenceRecord{"synthetic/skipped.json": skipped}); len(got) != 0 {
		t.Fatalf("the reverse rule demanded an evidence id for a check recorded skipped: %v", got)
	}

	// And the forward rule refuses to accept a skipped check as the passed
	// exercise a citation claims, so a capability cannot be given an evidence
	// id on the strength of a skip either.
	cited := capabilityEvidenceSyntheticClaims(map[string][]string{"cap-beta": {"ev-synthetic-skipped"}})
	index := capabilityEvidenceIndex{"ev-synthetic-skipped": "synthetic/skipped.json"}
	got := capabilityEvidenceForward(cited, index, map[string]capabilityEvidenceRecord{"synthetic/skipped.json": skipped})
	if len(got) != 1 {
		t.Fatalf("the forward rule accepted a skipped check as the passed exercise a citation claims: %v", got)
	}
}
