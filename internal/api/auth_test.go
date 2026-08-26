package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// ===========================================================================
// V2-086: CombinedAuthenticator is the transport that can assert the Loop's own
// identity. The three properties below are the ones the design calls
// load-bearing, and each is asserted rather than read off the source.
// ===========================================================================

// authSchedulerEmail and authOwnerEmail are the two IAP identities these tests
// configure. Neither is a credential: an IAP composition's only secret is the
// upstream assertion, which httptest supplies as a header value.
const (
	authOwnerEmail     = "owner@example.com"
	authSchedulerEmail = "reconciler@example.iam.gserviceaccount.com"
	authIAPHeader      = "X-Goog-Authenticated-User-Email"
	authIAPPrefix      = "accounts.google.com:"
)

func authIAPRequest(email string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(authIAPHeader, authIAPPrefix+email)
	return r
}

// TestCombinedAuthenticatorResolvesTheSchedulerBeforeTheOwnerMap is V2-086 A5.
//
// The scheduler branch sits after the runner-session branch and after
// parseIAPEmail and BEFORE the OwnerEmails lookup. The ORDER is asserted, not
// assumed: the same authenticator resolves the scheduler identity to
// RoleScheduler and an allowlisted owner to RoleOwner in the same run, a
// stranger is still refused, and the resolved scheduler Subject is the
// lowercased IAP email rather than the raw assertion -- which is what makes it
// usable as a domain ActorID. The upper-case assertion case also shows the
// comparison is case-insensitive on both sides.
func TestCombinedAuthenticatorResolvesTheSchedulerBeforeTheOwnerMap(t *testing.T) {
	auth := CombinedAuthenticator{
		OwnerEmails:       map[string]struct{}{authOwnerEmail: {}},
		SchedulerIdentity: authSchedulerEmail,
	}
	caller, err := auth.Authenticate(authIAPRequest(authSchedulerEmail))
	if err != nil {
		t.Fatalf("the scheduler identity was refused: %v", err)
	}
	if caller.Role != application.RoleScheduler || caller.Subject != authSchedulerEmail || caller.RunnerID != "" {
		t.Fatalf("scheduler identity resolved to %#v, want the scheduler role with the asserted email as subject", caller)
	}
	// Case folding on both sides, and the subject is normalised.
	caller, err = auth.Authenticate(authIAPRequest("RECONCILER@Example.IAM.gserviceaccount.COM"))
	if err != nil || caller.Role != application.RoleScheduler || caller.Subject != authSchedulerEmail {
		t.Fatalf("an upper-case assertion of the scheduler identity resolved to %#v (err=%v)", caller, err)
	}
	// The owner branch is untouched by the addition.
	caller, err = auth.Authenticate(authIAPRequest(authOwnerEmail))
	if err != nil || caller.Role != application.RoleOwner || caller.Subject != authOwnerEmail {
		t.Fatalf("the owner identity resolved to %#v (err=%v), want the owner role", caller, err)
	}
	// And an identity that is neither is still refused, so the new branch
	// widened nothing.
	if _, err := auth.Authenticate(authIAPRequest("stranger@example.com")); err == nil {
		t.Fatal("an identity that is neither owner nor scheduler was accepted")
	}
	// With the field left empty the authenticator behaves exactly as it did
	// before V2-086: the scheduler's own address is simply not an owner.
	unconfigured := CombinedAuthenticator{OwnerEmails: map[string]struct{}{authOwnerEmail: {}}}
	if _, err := unconfigured.Authenticate(authIAPRequest(authSchedulerEmail)); err == nil {
		t.Fatal("an unconfigured scheduler identity was accepted; the field must be opt-in")
	}
	if caller, err := unconfigured.Authenticate(authIAPRequest(authOwnerEmail)); err != nil || caller.Role != application.RoleOwner {
		t.Fatalf("the unconfigured authenticator resolved the owner to %#v (err=%v)", caller, err)
	}
}

// TestCombinedAuthenticatorRefusesAnIdentityConfiguredAsBothSchedulerAndOwner
// is V2-086 A5's and A6's refusal proof.
//
// An identity present in both OwnerEmails and SchedulerIdentity is a
// misconfiguration, and it is refused OUTRIGHT rather than resolved by
// precedence: a single asserted email must produce exactly one role, or an
// error. The refusal covers every IAP caller of that composition, not only the
// overlapping address, because a deployment in that state has no defensible
// answer for any of them -- and because cmd/control-plane has no test package,
// this is where A6's "must not resolve one of the two silently" is measured.
// The error message names the misconfiguration so an operator can act on it.
func TestCombinedAuthenticatorRefusesAnIdentityConfiguredAsBothSchedulerAndOwner(t *testing.T) {
	auth := CombinedAuthenticator{
		OwnerEmails:       map[string]struct{}{authOwnerEmail: {}, authSchedulerEmail: {}},
		SchedulerIdentity: authSchedulerEmail,
	}
	for _, asserted := range []string{authSchedulerEmail, authOwnerEmail, "stranger@example.com"} {
		caller, err := auth.Authenticate(authIAPRequest(asserted))
		if err == nil {
			t.Fatalf("an identity configured as both scheduler and owner resolved %q to %#v; it must be refused", asserted, caller)
		}
		if caller != (application.Caller{}) {
			t.Fatalf("the refusal for %q returned %#v alongside its error", asserted, caller)
		}
		if !strings.Contains(err.Error(), "scheduler identity is also an owner email") {
			t.Fatalf("the refusal for %q does not name the misconfiguration: %v", asserted, err)
		}
	}
	// Case folding does not let the overlap slip through.
	folded := CombinedAuthenticator{
		OwnerEmails:       map[string]struct{}{authSchedulerEmail: {}},
		SchedulerIdentity: "  RECONCILER@Example.IAM.gserviceaccount.COM  ",
	}
	if _, err := folded.Authenticate(authIAPRequest(authSchedulerEmail)); err == nil || !strings.Contains(err.Error(), "scheduler identity is also an owner email") {
		t.Fatalf("an overlap that differs only in case and padding was not refused: %v", err)
	}
}

// TestARunnerSessionStillResolvesAsTheRunnerWithASchedulerIdentityConfigured is
// the third half of A5: the runner-session branch is byte-unchanged and still
// runs FIRST, so configuring a scheduler identity cannot demote or promote a
// runner. The session is a real one, produced by the enrollment protocol rather
// than forged, and the request also carries the scheduler's IAP assertion, so
// the only thing that decides the answer is the branch order.
func TestARunnerSessionStillResolvesAsTheRunnerWithASchedulerIdentityConfigured(t *testing.T) {
	enrollment, err := runner.NewService(runner.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	token, err := enrollment.IssueEnrollment(ctx, "runner-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A deterministic key from an all-zero seed, not a generated one, so no
	// assertion here depends on randomness the test introduced.
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	challenge, err := enrollment.Begin(ctx, token, private.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(private, runner.ChallengeMessage(challenge))
	session, err := enrollment.Complete(ctx, challenge.ID, base64.RawURLEncoding.EncodeToString(signature))
	if err != nil {
		t.Fatal(err)
	}
	auth := CombinedAuthenticator{
		Runner:            enrollment,
		OwnerEmails:       map[string]struct{}{authOwnerEmail: {}},
		SchedulerIdentity: authSchedulerEmail,
	}
	r := authIAPRequest(authSchedulerEmail)
	r.Header.Set(RunnerSessionHeader, session.Token)
	caller, err := auth.Authenticate(r)
	if err != nil {
		t.Fatalf("a valid runner session with a scheduler assertion was refused: %v", err)
	}
	if caller.Role != application.RoleRunner || caller.Subject != "runner-1" || caller.RunnerID != "runner-1" {
		t.Fatalf("a valid runner session resolved to %#v, want the runner identity", caller)
	}
}

// TestNewCombinedAuthenticatorRefusesAnIdentityConfiguredAsBothSchedulerAndOwner
// is V2-086 A6's START-TIME refusal, asserted here because this is where the
// predicate lives and where a test package exists. cmd/control-plane's run()
// calls this constructor and returns its error, so the refusal proven here is
// the one that stops the process from starting; the untested part is one call
// site rather than the logic.
//
// The returned value on refusal is the ZERO CombinedAuthenticator, which
// matters: a caller that ignored the error must not end up holding a working
// authenticator. That is asserted, not assumed.
func TestNewCombinedAuthenticatorRefusesAnIdentityConfiguredAsBothSchedulerAndOwner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		owners    map[string]struct{}
		scheduler string
	}{
		{
			name:      "the scheduler identity is the only owner",
			owners:    map[string]struct{}{authSchedulerEmail: {}},
			scheduler: authSchedulerEmail,
		},
		{
			name:      "the scheduler identity is one of several owners",
			owners:    map[string]struct{}{authOwnerEmail: {}, authSchedulerEmail: {}},
			scheduler: authSchedulerEmail,
		},
		{
			name:      "the overlap differs only in case and padding",
			owners:    map[string]struct{}{authOwnerEmail: {}, authSchedulerEmail: {}},
			scheduler: "  RECONCILER@Example.IAM.gserviceaccount.COM  ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := NewCombinedAuthenticator(nil, tc.owners, tc.scheduler)
			if err == nil {
				t.Fatal("NewCombinedAuthenticator accepted an identity configured as both scheduler and owner; the process must fail to start")
			}
			if !strings.Contains(err.Error(), "scheduler identity is also an owner email") {
				t.Fatalf("the refusal does not name the conflict: %v", err)
			}
			if auth.OwnerEmails != nil || auth.SchedulerIdentity != "" || auth.Runner != nil {
				t.Fatalf("a refused configuration returned a usable authenticator: %#v", auth)
			}
			// The start-time refusal and the request-time refusal are the same
			// rule and the same message, because both call one predicate.
			literal := CombinedAuthenticator{OwnerEmails: tc.owners, SchedulerIdentity: tc.scheduler}
			_, requestErr := literal.Authenticate(authIAPRequest(authSchedulerEmail))
			if requestErr == nil || requestErr.Error() != err.Error() {
				t.Fatalf("the request-time refusal (%v) and the start-time refusal (%v) disagree; they must be one predicate", requestErr, err)
			}
			if literal.IdentityConfigurationError() == nil {
				t.Fatal("IdentityConfigurationError reported no problem for a configuration both other paths refuse")
			}
		})
	}
}

// TestNewCombinedAuthenticatorAcceptsADistinctSchedulerIdentity is the positive
// half: a non-conflicting configuration constructs, and the authenticator it
// returns is the working one -- so the start-time guard refuses exactly the
// misconfiguration and nothing else. An absent scheduler identity is also
// accepted, because the field is opt-in and that is the shape every deployment
// had before V2-086.
func TestNewCombinedAuthenticatorAcceptsADistinctSchedulerIdentity(t *testing.T) {
	auth, err := NewCombinedAuthenticator(nil, map[string]struct{}{authOwnerEmail: {}}, authSchedulerEmail)
	if err != nil {
		t.Fatalf("a distinct scheduler identity was refused: %v", err)
	}
	if auth.SchedulerIdentity != authSchedulerEmail {
		t.Fatalf("the constructed authenticator carries SchedulerIdentity %q", auth.SchedulerIdentity)
	}
	if auth.IdentityConfigurationError() != nil {
		t.Fatalf("a valid configuration reports an error: %v", auth.IdentityConfigurationError())
	}
	caller, err := auth.Authenticate(authIAPRequest(authSchedulerEmail))
	if err != nil || caller.Role != application.RoleScheduler || caller.Subject != authSchedulerEmail {
		t.Fatalf("the constructed authenticator resolved the scheduler to %#v (err=%v)", caller, err)
	}
	caller, err = auth.Authenticate(authIAPRequest(authOwnerEmail))
	if err != nil || caller.Role != application.RoleOwner {
		t.Fatalf("the constructed authenticator resolved the owner to %#v (err=%v)", caller, err)
	}

	// No scheduler identity at all: accepted, and it asserts owner and runner
	// only.
	bare, err := NewCombinedAuthenticator(nil, map[string]struct{}{authOwnerEmail: {}}, "")
	if err != nil {
		t.Fatalf("an absent scheduler identity was refused; the field is opt-in: %v", err)
	}
	if bare.IdentityConfigurationError() != nil {
		t.Fatalf("an absent scheduler identity reports an error: %v", bare.IdentityConfigurationError())
	}
	if _, err := bare.Authenticate(authIAPRequest(authSchedulerEmail)); err == nil {
		t.Fatal("an unconfigured scheduler address authenticated")
	}
}
