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
//	reverse  every check in a V2-022 evidence record whose name equals one of
//	         the contract's capability ids and whose status is passed must be
//	         referenced back by that capability's evidence_ids.
//
// The reverse direction is the more valuable half: it makes it impossible to
// exercise a capability successfully and then under-report it, which is the
// failure mode that would let a later release claim less than it had proved
// while still passing every existing check.
//
// This is a new file. No existing test file is edited, and in particular
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
func capabilityEvidenceReverse(claims capabilityClaims, records map[string]capabilityEvidenceRecord, onlyTaskID string) []string {
	var violations []string
	paths := make([]string, 0, len(records))
	for path := range records {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		record := records[path]
		if onlyTaskID != "" && record.TaskID != onlyTaskID {
			continue
		}
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

	// Every V2-022 record in the index, plus every record any capability
	// cites, is read once. Nothing else is read: this test judges the claims
	// this repository actually makes, not every record ever issued.
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
	for evidenceID, path := range index {
		_ = evidenceID
		record := capabilityEvidenceReadRecord(t, root, path)
		if record.TaskID == "V2-022" {
			records[path] = record
		}
	}

	if violations := capabilityEvidenceForward(claims, index, records); len(violations) != 0 {
		for _, v := range violations {
			t.Errorf("forward: %s", v)
		}
	}
	if violations := capabilityEvidenceReverse(claims, records, "V2-022"); len(violations) != 0 {
		for _, v := range violations {
			t.Errorf("reverse: %s", v)
		}
	}
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

	violations := capabilityEvidenceReverse(claims, records, "V2-022")
	if len(violations) != 1 {
		t.Fatalf("the reverse rule reported %d violations, want exactly 1: %v", len(violations), violations)
	}

	// A check recorded failed is not a claim, so it must not be demanded back.
	failed := capabilityEvidenceRecord{ID: "ev-synthetic-failed", TaskID: "V2-022"}
	failed.Checks = append(failed.Checks, struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{Name: "cap-beta", Status: "failed"})
	if violations := capabilityEvidenceReverse(claims, map[string]capabilityEvidenceRecord{"synthetic/failed.json": failed}, "V2-022"); len(violations) != 0 {
		t.Fatalf("the reverse rule demanded an evidence id for a check recorded failed: %v", violations)
	}

	// And once the contract cites the record, the same inputs pass.
	claims.cites["cap-beta"] = []string{"ev-synthetic"}
	if violations := capabilityEvidenceReverse(claims, records, "V2-022"); len(violations) != 0 {
		t.Fatalf("the reverse rule refused a correctly cited record: %v", violations)
	}
}
