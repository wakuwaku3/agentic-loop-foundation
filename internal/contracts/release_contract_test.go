package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestFoundationReleaseContractBaseline proves that the Foundation Repository's
// Release Contract baseline (contracts/release-contract/foundation.json) and
// its sibling capability declaration set
// (contracts/release-contract/foundation-capabilities.json) are schema valid,
// bijective on capability id, internally consistent, anti-fabrication safe,
// and resolvable against the documents and manifests they reference.
func TestFoundationReleaseContractBaseline(t *testing.T) {
	root := filepath.Join("..", "..")
	contractPath := filepath.Join(root, "contracts", "release-contract", "foundation.json")
	declPath := filepath.Join(root, "contracts", "release-contract", "foundation-capabilities.json")
	contractSchemaPath := filepath.Join(root, "contracts", "schemas", "release-contract.json")
	declSchemaPath := filepath.Join(root, "contracts", "schemas", "capability-declaration-set.json")

	// (a) both instances validate against their schemas.
	if err := ValidateFile(contractSchemaPath, mustRead(t, contractPath)); err != nil {
		t.Fatalf("foundation.json does not validate: %v", err)
	}
	if err := ValidateFile(declSchemaPath, mustRead(t, declPath)); err != nil {
		t.Fatalf("foundation-capabilities.json does not validate: %v", err)
	}

	contract := readJSON(t, contractPath)
	decl := readJSON(t, declPath)

	contractCaps, ok := contract["capabilities"].([]any)
	if !ok || len(contractCaps) == 0 {
		t.Fatal("foundation.json: capabilities missing or empty")
	}
	declCaps, ok := decl["capabilities"].([]any)
	if !ok || len(declCaps) == 0 {
		t.Fatal("foundation-capabilities.json: capabilities missing or empty")
	}

	// (b) capability ids are unique, non-empty and identical between the two
	// files in the same order.
	contractIDs := make([]string, 0, len(contractCaps))
	seenContractID := map[string]bool{}
	for _, raw := range contractCaps {
		capability := raw.(map[string]any)
		id := stringValue(capability["id"])
		if id == "" || seenContractID[id] {
			t.Fatalf("foundation.json: empty or duplicate capability id %q", id)
		}
		seenContractID[id] = true
		contractIDs = append(contractIDs, id)
	}
	declByID := map[string]map[string]any{}
	declIDs := make([]string, 0, len(declCaps))
	seenDeclID := map[string]bool{}
	for _, raw := range declCaps {
		capability := raw.(map[string]any)
		id := stringValue(capability["id"])
		if id == "" || seenDeclID[id] {
			t.Fatalf("foundation-capabilities.json: empty or duplicate capability id %q", id)
		}
		seenDeclID[id] = true
		declIDs = append(declIDs, id)
		declByID[id] = capability
	}
	if len(contractIDs) != len(declIDs) {
		t.Fatalf("capability id count mismatch: foundation.json has %d, foundation-capabilities.json has %d", len(contractIDs), len(declIDs))
	}
	for i := range contractIDs {
		if contractIDs[i] != declIDs[i] {
			t.Fatalf("capability id order mismatch at index %d: %q vs %q", i, contractIDs[i], declIDs[i])
		}
	}

	// (c) contract_ref equals the contract id and both release strings are equal.
	if got, want := stringValue(decl["contract_ref"]), stringValue(contract["id"]); got != want {
		t.Fatalf("contract_ref = %q, want %q", got, want)
	}
	if got, want := stringValue(decl["release"]), stringValue(contract["release"]); got != want {
		t.Fatalf("release mismatch: foundation-capabilities.json has %q, foundation.json has %q", got, want)
	}

	// (k1) unchanged or Provider-neutral behavior needs one declared Provider;
	// a direct Provider change needs every Provider it affects. The contract
	// must not name a permanently subscribed representative Provider.
	policy, present := decl["provider_verification"].(map[string]any)
	if !present {
		t.Fatal("k1: foundation-capabilities.json missing provider_verification")
	}
	if got := stringValue(policy["unchanged_or_provider_neutral"]); got != "any-one-declared" {
		t.Fatalf("k1: unchanged_or_provider_neutral = %q, want any-one-declared", got)
	}
	if got := stringValue(policy["directly_affected"]); got != "every-affected" {
		t.Fatalf("k1: directly_affected = %q, want every-affected", got)
	}
	if _, present := decl["representative_provider"]; present {
		t.Fatal("k1: a fixed representative_provider would require a permanent subscription")
	}

	// (k3) the three Provider-dependent capabilities continue to support all
	// three Providers. This declares availability, not a requirement to run all
	// three for every release.
	wantProviders := []string{"codex", "claude", "opencode"}
	for _, id := range []string{"cap-autonomous-resolution", "cap-shared-resource-allocation", "cap-provider-operation"} {
		capability := declByID[id]
		if capability == nil {
			t.Fatalf("k3: %s: not found in foundation-capabilities.json", id)
		}
		deps, _ := capability["external_dependencies"].(map[string]any)
		providers, _ := deps["providers"].([]any)
		got := make([]string, len(providers))
		for i, p := range providers {
			got[i] = stringValue(p)
		}
		if !reflect.DeepEqual(got, wantProviders) {
			t.Errorf("k3: %s: external_dependencies.providers = %v, want %v", id, got, wantProviders)
		}
	}

	release := stringValue(contract["release"])
	isBaseline := false
	if idx := strings.Index(release, "-"); idx >= 0 {
		isBaseline = release[idx+1:] == "baseline"
	}

	for _, raw := range contractCaps {
		capability := raw.(map[string]any)
		id := stringValue(capability["id"])
		status := stringValue(capability["status"])
		// (d) baseline invariant.
		if isBaseline {
			if status != "preview" {
				t.Errorf("%s: baseline release requires status preview, got %q", id, status)
			}
		}

		// (e) Stable claims must publish Stable documentation. Runtime
		// verification results are carried by the candidate/session, not by
		// identifiers in this tracked declaration.
		if status == "stable" {
			declared := declByID[id]
			if declared == nil || nestedString(declared, "documentation", "stable") == "" {
				t.Errorf("%s: status stable requires documentation.stable", id)
			}
		}
	}

	openAPIPaths := openAPIDeclaredPaths(t, root)
	makeTargets := makefileTargets(t, root)

	for _, raw := range declCaps {
		capability := raw.(map[string]any)
		id := stringValue(capability["id"])

		// (g) anchor and path resolution.
		documentation, _ := capability["documentation"].(map[string]any)
		if documentation == nil {
			t.Fatalf("%s: documentation missing", id)
		}
		checkDocumentationAnchor(t, root, id, "preview", stringValue(documentation["preview"]))
		if stableRef, exists := documentation["stable"]; exists {
			checkDocumentationAnchor(t, root, id, "stable", stringValue(stableRef))
		}

		// (i) owner_surfaces resolution.
		surfaces, _ := capability["owner_surfaces"].([]any)
		if len(surfaces) == 0 {
			t.Errorf("%s: owner_surfaces missing or empty", id)
		}
		for _, rawSurface := range surfaces {
			surface := stringValue(rawSurface)
			if surface != "/owner/" && !openAPIPaths[surface] {
				t.Errorf("%s: owner_surfaces entry %q is neither /owner/ nor a declared OpenAPI path", id, surface)
			}
		}
	}

	// (j) every verification entry whose first token is make names a target
	// present in the Makefile.
	verification, _ := contract["verification"].([]any)
	if len(verification) == 0 {
		t.Fatal("foundation.json: verification missing or empty")
	}
	for _, raw := range verification {
		entry := stringValue(raw)
		fields := strings.Fields(entry)
		if len(fields) < 2 || fields[0] != "make" {
			continue
		}
		if !makeTargets[fields[1]] {
			t.Errorf("verification entry %q names an unknown make target %q", entry, fields[1])
		}
	}
}

// TestCapabilityDeclarationSetProviderVerificationPolicyIsClosed proves the
// promotion policy cannot be weakened to an unrecognized mode.
func TestCapabilityDeclarationSetProviderVerificationPolicyIsClosed(t *testing.T) {
	root := filepath.Join("..", "..")
	schemaPath := filepath.Join(root, "contracts", "schemas", "capability-declaration-set.json")
	validPath := filepath.Join(root, "contracts", "fixtures", "valid", "capability-declaration-set.json")

	schema := mustRead(t, schemaPath)
	decoded := readJSON(t, validPath)
	decoded["provider_verification"] = map[string]any{
		"unchanged_or_provider_neutral": "none-required",
		"directly_affected":             "every-affected",
	}
	mutated, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(schema, mutated, ResolveSchemaRef(filepath.Dir(schemaPath))); err == nil {
		t.Fatal("provider_verification accepted the weakening none-required")
	}
}

// nestedString reads a dotted-path string field out of nested map[string]any
// values, returning "" if any segment is absent or not a string.
func nestedString(value map[string]any, keys ...string) string {
	var current any = value
	for _, key := range keys {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = obj[key]
	}
	return stringValue(current)
}

// checkDocumentationAnchor asserts that ref splits into an existing file path
// and an anchor that occurs exactly once in that file as <a id="anchor"></a>.
func checkDocumentationAnchor(t *testing.T, root, capabilityID, field, ref string) {
	t.Helper()
	parts := strings.SplitN(ref, "#", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("%s: documentation.%s %q must be path#anchor", capabilityID, field, ref)
	}
	docPath := filepath.Join(root, parts[0])
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("%s: documentation.%s %q: %v", capabilityID, field, ref, err)
	}
	anchor := `<a id="` + parts[1] + `"></a>`
	if count := strings.Count(string(data), anchor); count != 1 {
		t.Fatalf("%s: documentation.%s %q: anchor occurs %d times in %s, want 1", capabilityID, field, ref, count, docPath)
	}
}

// openAPIDeclaredPaths extracts the literal top-level path keys declared
// under the OpenAPI document's paths section.
func openAPIDeclaredPaths(t *testing.T, root string) map[string]bool {
	t.Helper()
	data := mustRead(t, filepath.Join(root, "contracts", "openapi", "openapi-v1.yaml"))
	paths := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  (/\S+):$`).FindAllStringSubmatch(string(data), -1) {
		paths[m[1]] = true
	}
	return paths
}

// makefileTargets extracts the target names declared in the repository Makefile.
func makefileTargets(t *testing.T, root string) map[string]bool {
	t.Helper()
	data := mustRead(t, filepath.Join(root, "Makefile"))
	targets := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^([A-Za-z0-9_.-]+):`).FindAllStringSubmatch(string(data), -1) {
		targets[m[1]] = true
	}
	return targets
}
