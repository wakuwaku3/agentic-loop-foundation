package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// The Secret Broker is the runner's two-channel credential boundary
// (dp-v2-016 d7). The guarded base environment (GuardEnvironment) is what the
// bounded log and the journal may observe. The granted channel below is
// produced only by Lease, for one Scope and one Invocation, and is merged
// into the child environment by the caller (ProviderClient.Run merges it onto
// the Invocation immediately before the InvocationRunner seam); it is never
// returned to any code path that writes the journal, the Work Packet, the
// canonical store or the bounded log.
var (
	ErrSecretExpired          = errors.New("secret broker: scope has already expired")
	ErrSecretDenied           = errors.New("secret broker: credential permit denied")
	ErrSecretNotAllowed       = errors.New("secret broker: credential name is not allowlisted for this provider")
	ErrSecretFencingRequired  = errors.New("secret broker: a non-zero fencing token is required")
	ErrSecretFencingMismatch  = errors.New("secret broker: fencing token does not match the expected fencing token")
	ErrSecretGrantConsumed    = errors.New("secret broker: grant has already been consumed")
	ErrSecretGrantRevoked     = errors.New("secret broker: grant has been revoked")
	ErrSecretScopeIncomplete  = errors.New("secret broker: scope is incomplete")
	ErrSecretSourceUnavailble = errors.New("secret broker: credential source produced no value")
)

// CredentialSource resolves a credential name to its current value. Test
// callers must implement it with a value derived from crypto/rand inside the
// test, never a real credential.
type CredentialSource interface {
	Value(name string) (string, error)
}

// MapCredentialSource is a CredentialSource backed by an in-memory map. It
// exists so tests can inject a generated fixture without touching disk or the
// environment.
type MapCredentialSource map[string]string

func (m MapCredentialSource) Value(name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", ErrSecretSourceUnavailble
	}
	return v, nil
}

// Scope identifies exactly which execution, repository, provider and fencing
// generation a Lease grant is bound to, and when it expires.
type Scope struct {
	ExecutionID string
	Repository  string
	Provider    string
	Target      domain.ControlTarget
	// ControlRevision must equal the control revision the caller's own Claim
	// or Permit already observed; a stale value is denied the same way a
	// stale value denies any other Permit.
	ControlRevision domain.Revision
	// FencingToken is the fencing token the caller currently believes is
	// active for ExecutionID. ExpectedFencingToken is the fencing token the
	// caller independently obtained (for example from Claim). A zero
	// FencingToken, or a FencingToken that does not equal a non-zero
	// ExpectedFencingToken, is refused before any Permit call is made.
	FencingToken         domain.FencingToken
	ExpectedFencingToken domain.FencingToken
	ExpiresAt            time.Time
}

// Grant is a single-use, revocable credential handed out by Lease for one
// Scope. Only Environment() ever exposes the credential value, and only to
// the code path that is about to merge it into a child process environment.
type Grant struct {
	id  string
	env []string
}

// Environment returns the KEY=VALUE environment entries this grant carries.
// The returned slice is a defensive copy.
func (g *Grant) Environment() []string { return append([]string(nil), g.env...) }

// Apply merges this grant's environment onto inv.Environment and returns the
// updated Invocation. It never mutates any other field.
func (g *Grant) Apply(inv provider.Invocation) provider.Invocation {
	if g == nil {
		return inv
	}
	inv.Environment = append(append([]string(nil), inv.Environment...), g.env...)
	return inv
}

// SecretBroker is the local, in-process Secret Broker. Lease fails closed
// on: an expiry at or before now; a domain.PermitCredential denial under an
// effective stop (including a stale/mismatched control revision); a
// credential name outside the per-provider allowlist; a zero or mismatched
// fencing token; and a second use of an already-consumed or revoked grant.
type SecretBroker struct {
	Service   *application.Service
	Source    CredentialSource
	Allowlist map[string]map[string]bool
	Now       func() time.Time

	mu       sync.Mutex
	consumed map[string]bool
	revoked  map[string]bool
	seq      uint64
}

// NewSecretBroker builds an allowlist map from a provider->names shape that
// is easier for callers (and tests) to write out literally.
func NewSecretBroker(service *application.Service, source CredentialSource, allowlist map[string][]string, now func() time.Time) *SecretBroker {
	al := make(map[string]map[string]bool, len(allowlist))
	for providerName, names := range allowlist {
		set := make(map[string]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		al[providerName] = set
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &SecretBroker{Service: service, Source: source, Allowlist: al, Now: now, consumed: map[string]bool{}, revoked: map[string]bool{}}
}

func (b *SecretBroker) allowed(providerName, name string) bool {
	set, ok := b.Allowlist[providerName]
	return ok && set[name]
}

// Revoke marks every future Lease for executionID as denied. It is
// idempotent and safe to call more than once.
func (b *SecretBroker) Revoke(executionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked[executionID] = true
}

func (b *SecretBroker) nextRequestID(scope Scope) string {
	b.mu.Lock()
	b.seq++
	seq := b.seq
	b.mu.Unlock()
	return fmt.Sprintf("secret-broker:%s:%d", scope.ExecutionID, seq)
}

// Lease grants one Invocation's worth of credential environment for scope.
// It is single-use: a second call for the same ExecutionID fails closed with
// ErrSecretGrantConsumed even if the first call succeeded.
func (b *SecretBroker) Lease(ctx context.Context, scope Scope, names []string) (*Grant, error) {
	if scope.ExecutionID == "" || scope.Provider == "" {
		return nil, ErrSecretScopeIncomplete
	}
	if scope.ExpiresAt.IsZero() || !scope.ExpiresAt.After(b.Now()) {
		return nil, ErrSecretExpired
	}
	if scope.FencingToken == 0 {
		return nil, ErrSecretFencingRequired
	}
	if scope.ExpectedFencingToken != 0 && scope.FencingToken != scope.ExpectedFencingToken {
		return nil, ErrSecretFencingMismatch
	}
	for _, name := range names {
		if !b.allowed(scope.Provider, name) {
			return nil, fmt.Errorf("%w: %s/%s", ErrSecretNotAllowed, scope.Provider, name)
		}
	}
	b.mu.Lock()
	if b.revoked[scope.ExecutionID] {
		b.mu.Unlock()
		return nil, ErrSecretGrantRevoked
	}
	if b.consumed[scope.ExecutionID] {
		b.mu.Unlock()
		return nil, ErrSecretGrantConsumed
	}
	b.mu.Unlock()

	if b.Service != nil {
		if _, err := b.Service.Permit(ctx, application.PermitRequest{
			RequestID:            b.nextRequestID(scope),
			Kind:                 domain.PermitCredential,
			Target:               scope.Target,
			ControlRevision:      scope.ControlRevision,
			FencingToken:         scope.FencingToken,
			ExpectedFencingToken: scope.FencingToken,
			Resource:             scope.ExecutionID,
		}); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSecretDenied, err)
		}
	}
	if b.Source == nil {
		return nil, errors.New("secret broker: no credential source configured")
	}
	env := make([]string, 0, len(names))
	for _, name := range names {
		v, err := b.Source.Value(name)
		if err != nil {
			return nil, err
		}
		env = append(env, name+"="+v)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.revoked[scope.ExecutionID] {
		return nil, ErrSecretGrantRevoked
	}
	if b.consumed[scope.ExecutionID] {
		return nil, ErrSecretGrantConsumed
	}
	b.consumed[scope.ExecutionID] = true
	id := randomGrantID()
	return &Grant{id: id, env: env}, nil
}

func randomGrantID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
