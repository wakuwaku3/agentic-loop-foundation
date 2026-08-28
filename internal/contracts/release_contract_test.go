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

	// (k1) the declaration set names a representative Provider and it is
	// "claude" (V2-047: Sol's re-plan of M3 around the representative
	// Provider role rather than a hardcoded CLI).
	repProviderRaw, present := decl["representative_provider"]
	if !present {
		t.Fatal("k1: foundation-capabilities.json missing representative_provider")
	}
	representativeProvider := stringValue(repProviderRaw)
	if representativeProvider != "claude" {
		t.Fatalf("k1: representative_provider = %q, want %q", representativeProvider, "claude")
	}

	// (k2) the declared representative Provider must actually be a
	// dependency of every capability that declares a non-empty providers
	// array: naming a representative Provider the contract does not depend
	// on anywhere would be an unenforceable claim.
	for _, raw := range declCaps {
		capability := raw.(map[string]any)
		id := stringValue(capability["id"])
		deps, _ := capability["external_dependencies"].(map[string]any)
		providers, _ := deps["providers"].([]any)
		if len(providers) == 0 {
			continue
		}
		found := false
		for _, p := range providers {
			if stringValue(p) == representativeProvider {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("k2: %s: external_dependencies.providers %v does not include representative_provider %q", id, providers, representativeProvider)
		}
	}

	// (k3) anti-weakening pin: the three Provider-dependent capabilities
	// must each still declare all three Providers, in the same order.
	// Promoting a capability by deleting codex or opencode from its
	// providers array would be a substantive contract weakening that Sol
	// explicitly prohibited; three-Provider live remains V2-028's M6 work.
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

	// (k4) forward compatibility: if foundation.json ever gains its own
	// representative_provider field (a later, sequenced migration once
	// internal/release/release.go's DisallowUnknownFields decoder gains the
	// field), it must not disagree with the declaration set's value.
	if raw, present := contract["representative_provider"]; present {
		if got := stringValue(raw); got != representativeProvider {
			t.Errorf("k4: foundation.json representative_provider = %q, want %q (must match declaration set)", got, representativeProvider)
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

// TestCapabilityDeclarationSetRepresentativeProviderEnumIsEnforced proves
// dp-v2-047 d3(ii): representative_provider's enum keyword does the
// rejecting, not some other field of an already-broken fixture. It mutates
// a decoded copy of the VALID capability-declaration-set fixture (the only
// way to isolate the enum's effect) to an out-of-enum value and requires
// Validate to reject it.
func TestCapabilityDeclarationSetRepresentativeProviderEnumIsEnforced(t *testing.T) {
	root := filepath.Join("..", "..")
	schemaPath := filepath.Join(root, "contracts", "schemas", "capability-declaration-set.json")
	validPath := filepath.Join(root, "contracts", "fixtures", "valid", "capability-declaration-set.json")

	schema := mustRead(t, schemaPath)
	decoded := readJSON(t, validPath)
	decoded["representative_provider"] = "gemini"
	mutated, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(schema, mutated, ResolveSchemaRef(filepath.Dir(schemaPath))); err == nil {
		t.Fatal("representative_provider enum accepted the out-of-enum value \"gemini\"")
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
