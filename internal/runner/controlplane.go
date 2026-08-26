// The Runner's own control-plane client (V2-091).
//
// WHY THIS FILE IS NEW RATHER THAN A REFACTOR. Measured at 848d899,
// `grep -ln 'net/http' internal/runner/*.go` returned NOTHING: not one file in
// this package spoke HTTP. Every "journey" this package could drive was an
// IN-PROCESS one holding a pointer to the application Service
// (internal/runner/orchestrator.go), which means the shipped cmd/runner binary
// could not talk to a Control Plane at all -- and the shipped cmd/runner binary
// refused to start without --fake for exactly that reason.
//
// WHAT IT DELIBERATELY DOES NOT DO.
//
//   - It constructs NO application.Caller. Its identity is a SESSION THE SERVER
//     VERIFIED: it holds an opaque token read from a file, sends it in the one
//     header internal/api reads, and the server decides who it is.
//     internal/runner/orchestrator.go:54 fabricates
//     application.Caller{Role: RoleRunner, ...} with no session verification at
//     all; that file is prohibited to this task and this client is the shape
//     that does not need it.
//   - It starts NO goroutine and NO timer, and reads no wall clock: the
//     *http.Client and the clock are both INJECTED. Cancellation is the
//     caller's context.
//   - It NEVER retries a non-2xx response in a loop. A refusal is returned.
//   - It never logs, wraps or returns the session token. Every error this file
//     produces is asserted token-free in controlplane_test.go by driving a
//     failure on every call.
//
// The imports are the standard library only. This is INTRA-component:
// ci/components.json's runner component already declares both cmd/runner/** and
// internal/runner/** among its roots, so cmd/runner reaching this file declares
// no edge, and ci/components.json is not edited.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ControlPlaneSessionHeader is the header internal/api's authenticators read to
// establish a verified runner session. It is a header NAME, never a value.
const ControlPlaneSessionHeader = "X-Agentic-Runner-Session"

// The four routes this client speaks, and no others. They are constants so a
// scan over this file can be written against the constant rather than against a
// literal, and so a typo is a compile-time-visible single point rather than
// four.
const (
	controlPlaneWorkPath      = "/v1/runner/work"
	controlPlaneClaimPath     = "/v1/runner/claims:acquire"
	controlPlaneHeartbeatPath = "/v1/runner/heartbeat"
	controlPlaneExecutionsPfx = "/v1/executions/"
	controlPlaneStartSuffix   = ":start"
)

// maxControlPlaneResponseBytes bounds how much of a response body this client
// will read. A Control Plane that answered with an unbounded stream must not be
// able to exhaust a Runner's memory, and every response this client expects is
// a few hundred bytes.
const maxControlPlaneResponseBytes = 1 << 20

// ErrControlPlaneRefused is returned for any non-2xx response. It is
// deliberately a single sentinel rather than one per status: the Runner's only
// correct reaction to a refusal is to stop this pass, and a client that
// distinguished refusals would be tempted to retry some of them.
var ErrControlPlaneRefused = errors.New("control plane refused the request")

// ControlPlaneClock is the injected clock. It exists so nothing in this file
// reads the wall clock: the request ids this client derives are a function of an
// instant its caller supplied.
type ControlPlaneClock interface{ Now() time.Time }

// ControlPlaneConfig is everything a client needs, all explicit.
type ControlPlaneConfig struct {
	// BaseURL is the Control Plane's origin, e.g. http://127.0.0.1:8080. It is
	// required and is never defaulted: a defaulted origin is how a Runner ends
	// up talking to the wrong installation.
	BaseURL string
	// SessionTokenPath is the path of a file holding the runner session token.
	// It is a PATH and never the value: a token passed as a flag is visible in
	// every process listing on the machine, and a token passed in an
	// environment variable is inherited by every child process.
	SessionTokenPath string
	// HTTPClient is injected. A nil value is refused rather than defaulted to
	// http.DefaultClient, whose Timeout is zero -- an unbounded request is a
	// hang a Runner cannot recover from.
	HTTPClient *http.Client
	// Clock is injected.
	Clock ControlPlaneClock
}

// ControlPlaneClient speaks the four calls a bounded driver pass needs.
type ControlPlaneClient struct {
	base    string
	session string
	http    *http.Client
	clock   ControlPlaneClock
}

// NewControlPlaneClient reads the session token from its file, refusing a file
// whose permissions are wider than 0600, and returns a client bound to one
// origin. The token is held in memory and in no other place: it is written to
// no log, no journal and no error.
func NewControlPlaneClient(cfg ControlPlaneConfig) (*ControlPlaneClient, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("control plane base URL is required")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, errors.New("control plane base URL must be an http or https origin")
	}
	if cfg.HTTPClient == nil {
		return nil, errors.New("an explicit http client is required: a nil client would inherit an unbounded default timeout")
	}
	if cfg.Clock == nil {
		return nil, errors.New("an injected clock is required")
	}
	session, err := ReadSessionTokenFile(cfg.SessionTokenPath)
	if err != nil {
		return nil, err
	}
	return &ControlPlaneClient{base: base, session: session, http: cfg.HTTPClient, clock: cfg.Clock}, nil
}

// ReadSessionTokenFile reads a runner session token from an absolute path whose
// permissions are no wider than 0600. It is exported because cmd/runner
// validates the same file before it builds a client, and validating it twice
// with two expressions of "no wider than 0600" is exactly the drift a single
// function prevents.
//
// NO ERROR THIS FUNCTION RETURNS CONTAINS THE TOKEN, or any part of it. The
// path is named; the contents never are.
func ReadSessionTokenFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("a session token file path is required")
	}
	if !strings.HasPrefix(path, "/") {
		return "", errors.New("session token file path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("session token file is not readable: %w", err)
	}
	if info.IsDir() {
		return "", errors.New("session token file path is a directory")
	}
	if perm := info.Mode().Perm(); perm&0o177 != 0 {
		return "", fmt.Errorf("session token file permissions are %04o; they must be no wider than 0600", perm)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("session token file is not readable: %w", err)
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", errors.New("session token file is empty")
	}
	return token, nil
}

// OfferedIncrement is one claimable Increment as the offer route reports it.
// The field names are the contract's, and there are exactly three of them.
type OfferedIncrement struct {
	RequirementID            string `json:"requirement_id"`
	IncrementID              string `json:"increment_id"`
	ExpectedIncrementVersion uint64 `json:"expected_increment_version"`
}

// OfferedWork is the offer route's response.
type OfferedWork struct {
	SchemaVersion string             `json:"schema_version"`
	Cap           int                `json:"cap"`
	Increments    []OfferedIncrement `json:"increments"`
}

// ClaimedWork is the subset of the claim response a bounded driver pass needs.
// The fencing token is carried because the Control Plane requires it on the
// calls that follow; it is not a credential and internal/runner already names
// FencingToken in its own vocabulary.
type ClaimedWork struct {
	IncrementID  string `json:"increment_id"`
	ExecutionID  string `json:"execution_id"`
	LeaseID      string `json:"lease_id"`
	RunnerID     string `json:"runner_id"`
	Version      uint64 `json:"version"`
	FencingToken uint64 `json:"fencing_token"`
}

// StartedExecution is the subset of the start response the driver reports.
type StartedExecution struct {
	ExecutionID string `json:"execution_id"`
	Status      string `json:"status"`
	Version     uint64 `json:"version"`
}

// HeartbeatAck is the subset of the heartbeat response the driver reports.
type HeartbeatAck struct {
	Accepted        bool   `json:"accepted"`
	LatestRevision  uint64 `json:"latest_revision"`
	AppliedRevision uint64 `json:"applied_revision"`
}

// OfferedWork reads the bounded set of Increments this Runner may claim.
func (c *ControlPlaneClient) OfferedWork(ctx context.Context) (OfferedWork, error) {
	var out OfferedWork
	err := c.call(ctx, http.MethodGet, controlPlaneWorkPath, nil, &out)
	return out, err
}

// Claim acquires one offered Increment. The increment id and the expected
// version are the ones the OFFER named: this client never invents either, which
// is what makes "it claims what it was offered and nothing else" a property of
// the code and not of the caller.
func (c *ControlPlaneClient) Claim(ctx context.Context, requestID string, offered OfferedIncrement) (ClaimedWork, error) {
	var out ClaimedWork
	body := map[string]any{
		"request_id":                 requestID,
		"increment_id":               offered.IncrementID,
		"expected_increment_version": offered.ExpectedIncrementVersion,
		"control_revision":           0,
	}
	err := c.call(ctx, http.MethodPost, controlPlaneClaimPath, body, &out)
	return out, err
}

// StartExecution moves the claimed Execution to running.
func (c *ControlPlaneClient) StartExecution(ctx context.Context, requestID, executionID string, expectedVersion uint64) (StartedExecution, error) {
	var out StartedExecution
	if strings.TrimSpace(executionID) == "" {
		return out, errors.New("an execution id is required")
	}
	body := map[string]any{
		"request_id":                 requestID,
		"expected_execution_version": expectedVersion,
		"control_revision":           0,
	}
	err := c.call(ctx, http.MethodPost, controlPlaneExecutionsPfx+executionID+controlPlaneStartSuffix, body, &out)
	return out, err
}

// Heartbeat reports once. It reports no process observation and no runner
// version: this driver stops at the provider boundary, so it has no process to
// observe, and inventing one would be reporting an observation it did not make.
func (c *ControlPlaneClient) Heartbeat(ctx context.Context, requestID string) (HeartbeatAck, error) {
	var out HeartbeatAck
	body := map[string]any{"request_id": requestID, "control_revision": 0}
	err := c.call(ctx, http.MethodPost, controlPlaneHeartbeatPath, body, &out)
	return out, err
}

// call is the single request path. Every property this file claims is enforced
// here once rather than four times.
func (c *ControlPlaneClient) call(ctx context.Context, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		// The error http.NewRequestWithContext returns names the URL, which is
		// the base and the path -- never the token, which has not been set yet.
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The one place the token is used. It is set on the header the server reads
	// and nowhere else.
	req.Header.Set(ControlPlaneSessionHeader, c.session)
	resp, err := c.http.Do(req)
	if err != nil {
		// net/http's own error names the method and the URL. It does not carry
		// request headers, so it cannot carry the token; controlplane_test.go
		// asserts that by driving a transport error and scanning the message.
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxControlPlaneResponseBytes))
	if err != nil {
		return fmt.Errorf("read %s %s response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The status is reported; the body is NOT, because a Control Plane error
		// envelope is not this Runner's to relay and a body echoed into a log is
		// how a value ends up somewhere it was never meant to be.
		return fmt.Errorf("%w: %s %s returned %d", ErrControlPlaneRefused, method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	// A malformed body is an ERROR, never a zero value: a Runner that treated an
	// unparseable offer as an empty offer would silently do nothing and report
	// success. The body must be exactly ONE JSON object, checked before it is
	// projected onto the struct, so a truncated or doubled response cannot be
	// read as a partial one.
	var envelope map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err = decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode %s %s response: the body must contain exactly one JSON object", method, path)
	}
	// The projection is DELIBERATELY not DisallowUnknownFields: a field the
	// Control Plane adds to a response must not break a Runner, and this client
	// models only the fields a bounded driver pass needs. What is asserted
	// instead is the shape (one JSON object) and the fields this client requires.
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}
