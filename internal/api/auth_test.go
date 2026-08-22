package api

import (
	"net/http/httptest"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
)

func TestCombinedAuthenticatorStrictIAPAllowlist(t *testing.T) {
	auth := CombinedAuthenticator{OwnerEmails: map[string]struct{}{"owner@example.com": {}}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:OWNER@example.com")
	caller, err := auth.Authenticate(r)
	if err != nil || caller.Role != application.RoleOwner || caller.Subject != "owner@example.com" {
		t.Fatalf("owner authentication: %#v %v", caller, err)
	}
	r.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:attacker@example.com")
	if _, err := auth.Authenticate(r); err == nil {
		t.Fatal("unallowlisted IAP identity impersonated owner")
	}
	r.Header.Set("X-Goog-Authenticated-User-Email", "owner@example.com")
	if _, err := auth.Authenticate(r); err == nil {
		t.Fatal("non-IAP header accepted")
	}
}

func TestCombinedAuthenticatorDoesNotFallbackFromRunnerHeader(t *testing.T) {
	s, err := runner.NewService(runner.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	auth := CombinedAuthenticator{Runner: s, OwnerEmails: map[string]struct{}{"owner@example.com": {}}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(RunnerSessionHeader, "forged")
	r.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:owner@example.com")
	if _, err := auth.Authenticate(r); err == nil {
		t.Fatal("invalid runner session fell back to owner IAP identity")
	}
}
