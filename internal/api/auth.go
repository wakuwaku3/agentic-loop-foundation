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
	// SchedulerIdentity is the single IAP-asserted email of the Loop's own
	// machine identity -- in a real deployment the Cloud Scheduler OIDC
	// service account cmd/control-plane already requires as
	// RECONCILE_IDENTITY. It is optional: an empty value means this
	// composition can assert an owner and a runner and nothing else, which is
	// exactly the shape every deployment had before V2-086.
	SchedulerIdentity string
}

// NewCombinedAuthenticator validates an identity configuration and returns the
// production transport boundary, refusing a scheduler identity that is also an
// owner email. It exists so the misconfiguration is caught at START-UP and not
// only at the request boundary: cmd/control-plane's run() calls it and returns
// its error the way it returns every other constructor's, exactly the shape
// NewJSONOperatorRecorder established, so a control-plane whose
// RECONCILE_IDENTITY is also in OWNER_EMAILS fails to start and names the
// conflict. A process that started and then rejected every IAP request would
// present to an operator as an authentication outage; the same fault caught at
// start-up presents as what it is, at the only moment anyone is positioned to
// fix it.
//
// The predicate lives here rather than in cmd/control-plane because this
// package has a test package and that one does not: the logic is asserted by
// unit test and the untested part is one call site. Authenticate keeps its own
// refusal too -- defence in depth against a CombinedAuthenticator built as a
// composite literal, which every test in this repository still does.
func NewCombinedAuthenticator(runnerService *runner.Service, ownerEmails map[string]struct{}, schedulerIdentity string) (CombinedAuthenticator, error) {
	a := CombinedAuthenticator{Runner: runnerService, OwnerEmails: ownerEmails, SchedulerIdentity: schedulerIdentity}
	if err := a.IdentityConfigurationError(); err != nil {
		return CombinedAuthenticator{}, err
	}
	return a, nil
}

// IdentityConfigurationError reports the one way an identity configuration can
// be self-contradictory: a single asserted email that would have to be both the
// owner and the Loop. It is the SINGLE source of truth for that rule -- both
// NewCombinedAuthenticator and Authenticate call it, so the start-time refusal
// and the request-time refusal cannot drift apart or disagree on the message.
// An empty SchedulerIdentity is not a misconfiguration: the field is opt-in and
// a composition without it asserts an owner and a runner and nothing else.
func (a CombinedAuthenticator) IdentityConfigurationError() error {
	scheduler := strings.ToLower(strings.TrimSpace(a.SchedulerIdentity))
	if scheduler == "" {
		return nil
	}
	if _, ok := a.OwnerEmails[scheduler]; ok {
		return errors.New("misconfigured identity: the scheduler identity is also an owner email; an asserted identity must have exactly one role")
	}
	return nil
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
	// The scheduler identity is checked BEFORE the owner map, and that order is
	// load-bearing rather than stylistic: an identity that resolved as owner
	// first would silently acquire every owner-only route, which is the same
	// self-naming defect one level up. The runner-session branch above still
	// runs first and is unchanged, so a verified runner session is a runner
	// even when a scheduler identity is configured.
	//
	// An identity configured as BOTH scheduler and owner is refused outright,
	// for both of them and for every other IAP caller, instead of being
	// resolved by precedence. A single asserted email must produce exactly one
	// role, or an error; silently picking one would hand whichever role lost
	// the tie to an operator who believes they configured the other.
	if err := a.IdentityConfigurationError(); err != nil {
		return application.Caller{}, err
	}
	if scheduler := strings.ToLower(strings.TrimSpace(a.SchedulerIdentity)); scheduler != "" && scheduler == email {
		return application.LoopCaller(email)
	}
	if _, ok := a.OwnerEmails[email]; !ok {
		return application.Caller{}, errors.New("IAP identity is not an owner")
	}
	return application.Caller{Role: application.RoleOwner, Subject: email}, nil
}

// LocalOwnerBearerAuthenticator is the preview-local owner authentication
// boundary (roadmap M2: "owner認証はローカルではsession/token境界。IAP境界は
// D1"). On the owner's machine, without a Cloud Run/IAP layer in front, owner
// identity is established by a bearer token instead of an upstream identity
// assertion header. Runner identity still goes through the existing runner
// session header/verifier, unchanged from CombinedAuthenticator. This type
// must never be selected for a real Cloud Run deployment; IAP remains the
// only owner boundary there. cmd/control-plane only selects it behind an
// explicit local-only opt-in environment variable.
type LocalOwnerBearerAuthenticator struct {
	Runner      *runner.Service
	OwnerTokens map[string]string // bearer token -> owner email
}

func (a LocalOwnerBearerAuthenticator) Authenticate(r *http.Request) (application.Caller, error) {
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
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, "Bearer ") {
		return application.Caller{}, errors.New("bearer token required")
	}
	token := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	if token == "" {
		return application.Caller{}, errors.New("bearer token required")
	}
	email, ok := a.OwnerTokens[token]
	if !ok || email == "" {
		return application.Caller{}, errors.New("unknown owner bearer token")
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
