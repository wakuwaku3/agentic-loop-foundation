package contracts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCapabilityProviderVerificationPolicy prevents the release declaration
// from restoring a permanently selected Provider or weakening direct-change
// verification to fewer than every affected Provider.
func TestCapabilityProviderVerificationPolicy(t *testing.T) {
	path := filepath.Join("..", "..", "contracts", "release-contract", "foundation-capabilities.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var declaration struct {
		RepresentativeProvider any `json:"representative_provider"`
		ProviderVerification   struct {
			UnchangedOrProviderNeutral string `json:"unchanged_or_provider_neutral"`
			DirectlyAffected           string `json:"directly_affected"`
		} `json:"provider_verification"`
	}
	if err := json.Unmarshal(raw, &declaration); err != nil {
		t.Fatal(err)
	}
	if declaration.RepresentativeProvider != nil {
		t.Fatal("fixed representative_provider requires a permanent Provider subscription")
	}
	if declaration.ProviderVerification.UnchangedOrProviderNeutral != "any-one-declared" {
		t.Fatalf("unchanged/provider-neutral policy = %q, want any-one-declared", declaration.ProviderVerification.UnchangedOrProviderNeutral)
	}
	if declaration.ProviderVerification.DirectlyAffected != "every-affected" {
		t.Fatalf("direct-change policy = %q, want every-affected", declaration.ProviderVerification.DirectlyAffected)
	}
}
