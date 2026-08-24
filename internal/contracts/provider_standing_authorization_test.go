package contracts

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// loadValidProviderStandingAuthorizationFixture returns the committed valid
// fixture for mutation-based negative tests below. Each test mutates its own
// in-memory copy and never writes back to disk.
func loadValidProviderStandingAuthorizationFixture(t *testing.T) map[string]any {
	t.Helper()
	root := filepath.Join("..", "..")
	return readJSON(t, filepath.Join(root, "contracts", "fixtures", "valid", "provider-standing-authorization.json"))
}

// TestProviderStandingAuthorizationRejectsApprovalAfterCreation gives the one
// rule validateProviderStandingAuthorization enforces its own explicit
// negative assertion: approved_at must not be later than created_at.
func TestProviderStandingAuthorizationRejectsApprovalAfterCreation(t *testing.T) {
	record := loadValidProviderStandingAuthorizationFixture(t)
	record["approved_at"] = "2099-01-01T00:00:00Z"
	if err := validateProviderStandingAuthorization(record); err == nil || !strings.Contains(err.Error(), "approved_at") {
		t.Fatalf("expected an approval-after-creation rejection, got %v", err)
	}
}

// TestProviderStandingAuthorizationSchemaRejectsUnknownProvider demonstrates
// the schema-only half of the dual-layer invalid fixture: an enum violation
// in providers is caught by the schema's enum keyword, not by
// validateProviderStandingAuthorization (which never inspects providers).
func TestProviderStandingAuthorizationSchemaRejectsUnknownProvider(t *testing.T) {
	root := filepath.Join("..", "..", "contracts")
	schema := mustRead(t, filepath.Join(root, "schemas", "provider-standing-authorization.json"))
	record := loadValidProviderStandingAuthorizationFixture(t)
	record["providers"] = []any{"gemini"}
	value, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(schema, value, ResolveSchemaRef(filepath.Join(root, "schemas"))); err == nil {
		t.Fatal("expected an unknown-provider rejection from the schema enum keyword")
	}
	// validateProviderStandingAuthorization on its own must not object: the
	// created_at/approved_at ordering is still fine, proving the rejection
	// above came from the schema layer, not this function.
	if err := validateProviderStandingAuthorization(record); err != nil {
		t.Fatalf("validateProviderStandingAuthorization must not reject providers content, got %v", err)
	}
}

// TestProviderStandingAuthorizationSchemaRejectsEmptyProviders pins that
// providers must be non-empty via the schema's minItems keyword.
func TestProviderStandingAuthorizationSchemaRejectsEmptyProviders(t *testing.T) {
	root := filepath.Join("..", "..", "contracts")
	schema := mustRead(t, filepath.Join(root, "schemas", "provider-standing-authorization.json"))
	record := loadValidProviderStandingAuthorizationFixture(t)
	record["providers"] = []any{}
	value, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(schema, value, ResolveSchemaRef(filepath.Join(root, "schemas"))); err == nil {
		t.Fatal("expected an empty-providers rejection from the schema minItems keyword")
	}
}

// TestProviderStandingAuthorizationSchemaRejectsMalformedApprover pins that
// approver must be email-shaped via the schema's pattern keyword.
func TestProviderStandingAuthorizationSchemaRejectsMalformedApprover(t *testing.T) {
	root := filepath.Join("..", "..", "contracts")
	schema := mustRead(t, filepath.Join(root, "schemas", "provider-standing-authorization.json"))
	record := loadValidProviderStandingAuthorizationFixture(t)
	record["approver"] = "not-an-email"
	value, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(schema, value, ResolveSchemaRef(filepath.Join(root, "schemas"))); err == nil {
		t.Fatal("expected a malformed-approver rejection from the schema pattern keyword")
	}
}

// TestProviderStandingAuthorizationSchemaRejectsUnknownKind pins that kind
// must be the const "provider-standing-authorization".
func TestProviderStandingAuthorizationSchemaRejectsUnknownKind(t *testing.T) {
	root := filepath.Join("..", "..", "contracts")
	schema := mustRead(t, filepath.Join(root, "schemas", "provider-standing-authorization.json"))
	record := loadValidProviderStandingAuthorizationFixture(t)
	record["kind"] = "something-else"
	value, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(schema, value, ResolveSchemaRef(filepath.Join(root, "schemas"))); err == nil {
		t.Fatal("expected an unknown-kind rejection from the schema const keyword")
	}
}
