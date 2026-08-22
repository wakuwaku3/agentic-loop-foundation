package api

import (
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
)

// BearerAuthenticator is intentionally explicit and small for local/dev
// composition. Production deployments must provide an authenticator backed by
// their identity provider; an absent authenticator fails closed in Handler.
type BearerAuthenticator map[string]application.Caller

func (a BearerAuthenticator) Authenticate(r *http.Request) (application.Caller, error) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, "Bearer ") {
		return application.Caller{}, errors.New("bearer token required")
	}
	c, ok := a[strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))]
	if !ok {
		return application.Caller{}, errors.New("unknown bearer token")
	}
	return c, nil
}

const RunnerSessionHeader = "X-Agentic-Runner-Session"

// CombinedAuthenticator is the production transport boundary. Cloud Run
// direct IAP authenticates the HTTP request; runner sessions use a separate
// header because IAP consumes Authorization. Never treat an arbitrary IAP
// assertion as a runner identity.
type CombinedAuthenticator struct {
	Runner      *runner.Service
	OwnerEmails map[string]struct{}
}

func (a CombinedAuthenticator) Authenticate(r *http.Request) (application.Caller, error) {
	if value := strings.TrimSpace(r.Header.Get(RunnerSessionHeader)); value != "" {
		if a.Runner == nil {
			return application.Caller{}, errors.New("runner session verifier is not configured")
		}
		runnerID, err := a.Runner.VerifySession(r.Context(), value)
		if err != nil || runnerID == "" {
			return application.Caller{}, errors.New("invalid runner session")
		}
		return application.Caller{Role: application.RoleRunner, Subject: runnerID, RunnerID: runnerID}, nil
	}
	email, err := parseIAPEmail(r.Header.Get("X-Goog-Authenticated-User-Email"))
	if err != nil {
		return application.Caller{}, err
	}
	if _, ok := a.OwnerEmails[email]; !ok {
		return application.Caller{}, errors.New("IAP identity is not an owner")
	}
	return application.Caller{Role: application.RoleOwner, Subject: email}, nil
}

func parseIAPEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	const prefix = "accounts.google.com:"
	if !strings.HasPrefix(value, prefix) {
		return "", errors.New("invalid IAP identity assertion")
	}
	email := strings.ToLower(strings.TrimPrefix(value, prefix))
	if email == "" || strings.IndexByte(email, '@') <= 0 || strings.Count(email, "@") != 1 || strings.ContainsAny(email, " \t\r\n") {
		return "", errors.New("invalid IAP email assertion")
	}
	for _, r := range email {
		if unicode.IsControl(r) {
			return "", errors.New("invalid IAP email assertion")
		}
	}
	return email, nil
}
