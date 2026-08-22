package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
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
