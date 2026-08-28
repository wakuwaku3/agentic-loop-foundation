package runner

// V2-091 A14. Every assertion here is driven through a STUB http.RoundTripper.
//
// WHY NOT httptest.NewServer. Measured at 848d899, `grep -rn
// 'httptest.NewServer' --include='*_test.go' .` returned ZERO hits across this
// whole repository. httptest.NewServer starts a goroutine, and determinism is
// acceptance for this task, not preference: no fixed sleep, no wall-clock timer,
// no goroutine and no randomness in any new or modified test. A RoundTripper
// stub answers in the calling goroutine, so every assertion below is
// synchronous and the whole file is reproducible byte for byte.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixture token is COMPUTED, not written down. Splitting a token into a
// prefix and a suffix is not enough on its own: `gitleaks git` flagged the
// suffix half of the earlier form, because a 16-hex quoted value sitting on
// a line whose identifier contains "Token" is exactly what generic-api-key
// looks for. The half was still secret-shaped and the name supplied the
// trigger keyword. So the high-entropy part is now built by repeating a
// short group, and no quoted value in this file has the shape of a
// credential. See docs/operations/v2-task-dag.md section 9.7.
const controlPlaneFixtureLabel = "runner-session-"
const controlPlaneFixtureGroup = "01ab"

func controlPlaneTestToken() string {
	return controlPlaneFixtureLabel + strings.Repeat(controlPlaneFixtureGroup, 4)
}

// stubRoundTrip records every request it saw and answers from a table. It starts
// no goroutine and reads no clock.
type stubRoundTrip struct {
	seen     []*http.Request
	headers  []http.Header
	bodies   []string
	status   map[string]int
	response map[string]string
	failWith error
	// claimFromBody makes the stub answer a claim with the increment the
	// request BODY named, so a test can assert the driver claimed exactly what
	// it was OFFERED rather than something the table happened to hold.
	claimFromBody bool
}

func (s *stubRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	s.seen = append(s.seen, r)
	s.headers = append(s.headers, r.Header.Clone())
	body := ""
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
	}
	s.bodies = append(s.bodies, body)
	if s.failWith != nil {
		return nil, s.failWith
	}
	key := r.Method + " " + r.URL.Path
	status, ok := s.status[key]
	if !ok {
		status = http.StatusOK
	}
	payload, ok := s.response[key]
	if !ok {
		payload = "{}"
	}
	if s.claimFromBody && key == "POST "+controlPlaneClaimPath {
		payload = stubClaimAnswer(body)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d", status),
		Body:       io.NopCloser(strings.NewReader(payload)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    r,
	}, nil
}

type stubClock struct{ now time.Time }

func (c stubClock) Now() time.Time { return c.now }

// controlPlaneTestTokenFile writes the token to a 0600 file inside t.TempDir.
func controlPlaneTestTokenFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session")
	if err := os.WriteFile(path, []byte(controlPlaneTestToken()+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func controlPlaneTestClient(t *testing.T, stub *stubRoundTrip) *ControlPlaneClient {
	t.Helper()
	client, err := NewControlPlaneClient(ControlPlaneConfig{
		BaseURL:          "http://127.0.0.1:65535",
		SessionTokenPath: controlPlaneTestTokenFile(t, 0o600),
		HTTPClient:       &http.Client{Transport: stub, Timeout: time.Second},
		Clock:            stubClock{now: time.Unix(1700000000, 0).UTC()},
	})
	if err != nil {
		t.Fatalf("NewControlPlaneClient: %v", err)
	}
	return client
}

// TestTheSessionTokenTravelsOnlyInTheHeaderTheServerReads is the central
// security assertion of the client: the token appears in the one header
// internal/api's authenticators read, and in NO url, NO body and NO error.
func TestTheSessionTokenTravelsOnlyInTheHeaderTheServerReads(t *testing.T) {
	stub := &stubRoundTrip{
		response: map[string]string{
			"GET /v1/runner/work":            `{"schema_version":"v1","cap":16,"increments":[{"requirement_id":"r1","increment_id":"i1","expected_increment_version":2}]}`,
			"POST /v1/runner/claims:acquire": `{"increment_id":"i1","execution_id":"e1","lease_id":"l1","runner_id":"runner-1","version":3,"fencing_token":7}`,
			"POST /v1/executions/e1:start":   `{"execution_id":"e1","status":"running","version":2}`,
			"POST /v1/runner/heartbeat":      `{"accepted":true}`,
		},
	}
	client := controlPlaneTestClient(t, stub)
	ctx := context.Background()
	offer, err := client.OfferedWork(ctx)
	if err != nil {
		t.Fatalf("OfferedWork: %v", err)
	}
	if len(offer.Increments) != 1 || offer.Increments[0].IncrementID != "i1" || offer.Increments[0].ExpectedIncrementVersion != 2 {
		t.Fatalf("offer = %+v", offer)
	}
	claimed, err := client.Claim(ctx, "req-1", offer.Increments[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed.ExecutionID != "e1" || claimed.FencingToken != 7 {
		t.Fatalf("claimed = %+v", claimed)
	}
	if _, err = client.StartExecution(ctx, "req-2", claimed.ExecutionID, 1); err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	if _, err = client.Heartbeat(ctx, "req-3"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if len(stub.seen) != 4 {
		t.Fatalf("the client made %d requests, want exactly 4 (the four calls it exposes)", len(stub.seen))
	}
	token := controlPlaneTestToken()
	for i, req := range stub.seen {
		if got := stub.headers[i].Get(ControlPlaneSessionHeader); got != token {
			t.Fatalf("request %d did not carry the session token in %s", i, ControlPlaneSessionHeader)
		}
		if strings.Contains(req.URL.String(), token) {
			t.Fatalf("request %d carries the session token in its URL: %s", i, req.URL.String())
		}
		if strings.Contains(stub.bodies[i], token) {
			t.Fatalf("request %d carries the session token in its body: %s", i, stub.bodies[i])
		}
		// The token travels in exactly ONE header, never a second one.
		carrying := 0
		for name, values := range stub.headers[i] {
			for _, value := range values {
				if strings.Contains(value, token) {
					carrying++
					if name != ControlPlaneSessionHeader {
						t.Fatalf("request %d carries the session token in header %q as well", i, name)
					}
				}
			}
		}
		if carrying != 1 {
			t.Fatalf("request %d carries the session token in %d header values, want exactly 1", i, carrying)
		}
	}
	// The claim body names the increment and the version the OFFER gave, and
	// nothing the client invented.
	if !strings.Contains(stub.bodies[1], `"increment_id":"i1"`) || !strings.Contains(stub.bodies[1], `"expected_increment_version":2`) {
		t.Fatalf("the claim body did not carry the offered identifiers verbatim: %s", stub.bodies[1])
	}
}

// TestEveryClientErrorIsTokenFreeAndNothingIsRetried drives a failure on EVERY
// call and asserts the token appears in none of the returned errors, and that a
// non-2xx produced exactly one request rather than a retry loop.
func TestEveryClientErrorIsTokenFreeAndNothingIsRetried(t *testing.T) {
	token := controlPlaneTestToken()
	offered := OfferedIncrement{RequirementID: "r1", IncrementID: "i1", ExpectedIncrementVersion: 2}
	calls := map[string]func(*ControlPlaneClient) error{
		"OfferedWork": func(c *ControlPlaneClient) error { _, err := c.OfferedWork(context.Background()); return err },
		"Claim":       func(c *ControlPlaneClient) error { _, err := c.Claim(context.Background(), "r", offered); return err },
		"Start": func(c *ControlPlaneClient) error {
			_, err := c.StartExecution(context.Background(), "r", "e1", 1)
			return err
		},
		"Heartbeat": func(c *ControlPlaneClient) error { _, err := c.Heartbeat(context.Background(), "r"); return err },
	}
	for name, call := range calls {
		// (a) a transport failure.
		transportStub := &stubRoundTrip{failWith: errors.New("dial refused")}
		if err := call(controlPlaneTestClient(t, transportStub)); err == nil {
			t.Fatalf("%s: a transport failure returned no error", name)
		} else if strings.Contains(err.Error(), token) {
			t.Fatalf("%s: the transport error carries the session token: %v", name, err)
		}
		if len(transportStub.seen) != 1 {
			t.Fatalf("%s: a transport failure produced %d requests, want exactly 1: nothing may be retried in a loop", name, len(transportStub.seen))
		}
		// (b) a non-2xx refusal.
		refuseStub := &stubRoundTrip{
			status: map[string]int{
				"GET /v1/runner/work": 403, "POST /v1/runner/claims:acquire": 409,
				"POST /v1/executions/e1:start": 400, "POST /v1/runner/heartbeat": 503,
			},
			response: map[string]string{
				"GET /v1/runner/work": `{"error":"forbidden","message":"` + token + `"}`,
			},
		}
		err := call(controlPlaneTestClient(t, refuseStub))
		if err == nil {
			t.Fatalf("%s: a non-2xx response returned no error", name)
		}
		if !errors.Is(err, ErrControlPlaneRefused) {
			t.Fatalf("%s: a non-2xx response = %v, want ErrControlPlaneRefused", name, err)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("%s: the refusal error carries the session token (it echoed the response body): %v", name, err)
		}
		if len(refuseStub.seen) != 1 {
			t.Fatalf("%s: a non-2xx produced %d requests, want exactly 1", name, len(refuseStub.seen))
		}
		// (c) a malformed body is an ERROR, never a zero value.
		malformedStub := &stubRoundTrip{response: map[string]string{
			"GET /v1/runner/work": `{"schema_version":`, "POST /v1/runner/claims:acquire": `not json`,
			"POST /v1/executions/e1:start": `[]`, "POST /v1/runner/heartbeat": `{} {}`,
		}}
		if err = call(controlPlaneTestClient(t, malformedStub)); err == nil {
			t.Fatalf("%s: a malformed body returned no error; a Runner that read an unparseable offer as an empty one would silently do nothing and report success", name)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("%s: the decode error carries the session token: %v", name, err)
		}
	}
}

// TestTheSessionTokenFileIsRefusedWhenItIsNotSafeToRead asserts the file
// discipline: absolute path, no wider than 0600, present, non-empty -- and that
// no refusal message names the token.
func TestTheSessionTokenFileIsRefusedWhenItIsNotSafeToRead(t *testing.T) {
	token := controlPlaneTestToken()
	dir := t.TempDir()
	wide := filepath.Join(dir, "wide")
	if err := os.WriteFile(wide, []byte(token), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
	}{
		{"empty path", ""},
		{"relative path", "session"},
		{"absent file", filepath.Join(dir, "missing")},
		{"a directory", dir},
		{"permissions wider than 0600", wide},
		{"empty contents", empty},
	} {
		got, err := ReadSessionTokenFile(tc.path)
		if err == nil {
			t.Fatalf("%s: ReadSessionTokenFile returned no error (value length %d)", tc.name, len(got))
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("%s: the refusal names the token: %v", tc.name, err)
		}
	}
	good, err := ReadSessionTokenFile(controlPlaneTestTokenFile(t, 0o600))
	if err != nil {
		t.Fatalf("a 0600 file with a token: %v", err)
	}
	if good != token {
		t.Fatal("the token read back does not match the token written")
	}
}

// TestTheClientRefusesToBeConstructedWithoutAnInjectedClientOrClock asserts the
// determinism contract as a construction-time refusal rather than a convention:
// a nil *http.Client would inherit http.DefaultClient's zero Timeout, which is
// an unbounded request a Runner cannot recover from.
func TestTheClientRefusesToBeConstructedWithoutAnInjectedClientOrClock(t *testing.T) {
	path := controlPlaneTestTokenFile(t, 0o600)
	base := "http://127.0.0.1:65535"
	for _, tc := range []struct {
		name string
		cfg  ControlPlaneConfig
	}{
		{"no base url", ControlPlaneConfig{SessionTokenPath: path, HTTPClient: &http.Client{}, Clock: stubClock{}}},
		{"a base url that is not an origin", ControlPlaneConfig{BaseURL: "127.0.0.1:8080", SessionTokenPath: path, HTTPClient: &http.Client{}, Clock: stubClock{}}},
		{"no http client", ControlPlaneConfig{BaseURL: base, SessionTokenPath: path, Clock: stubClock{}}},
		{"no clock", ControlPlaneConfig{BaseURL: base, SessionTokenPath: path, HTTPClient: &http.Client{}}},
		{"no token file", ControlPlaneConfig{BaseURL: base, HTTPClient: &http.Client{}, Clock: stubClock{}}},
	} {
		if _, err := NewControlPlaneClient(tc.cfg); err == nil {
			t.Fatalf("%s: NewControlPlaneClient returned no error", tc.name)
		}
	}
}

// TestTheClientExposesOnlyTheBoundedRunnerCalls pins the client surface,
// including the result call needed after a real Provider execution.
func TestTheClientExposesOnlyTheBoundedRunnerCalls(t *testing.T) {
	body, err := os.ReadFile("controlplane.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "/v1/executions/result") {
		t.Fatal("controlplane.go does not name the result route")
	}
	// The four call names, and no fifth, on *ControlPlaneClient.
	wanted := []string{"func (c *ControlPlaneClient) OfferedWork(", "func (c *ControlPlaneClient) Claim(", "func (c *ControlPlaneClient) StartExecution(", "func (c *ControlPlaneClient) CompleteExecution(", "func (c *ControlPlaneClient) Heartbeat("}
	for _, want := range wanted {
		if !strings.Contains(text, want) {
			t.Fatalf("controlplane.go does not declare %q", want)
		}
	}
	methods := strings.Count(text, "func (c *ControlPlaneClient) ")
	if methods != len(wanted)+1 {
		t.Fatalf("*ControlPlaneClient declares %d methods, want exactly %d (the five calls plus the single private request path)", methods, len(wanted)+1)
	}
	// A RESPONSE body is never echoed into an error. `raw` is the response
	// bytes' only name in this file, so a conversion of it to a string is the
	// shape this assertion catches; the token file's own bytes are named
	// `contents` for exactly that reason.
	if strings.Contains(text, "string(raw)") {
		t.Fatal("controlplane.go converts the raw RESPONSE body to a string; a body echoed into an error or a log is how a value ends up somewhere it was never meant to be")
	}
	// Determinism, structurally: no goroutine, no timer, no sleep, no wall clock.
	for _, forbidden := range []string{"go func", "time.After", "time.Sleep", "time.NewTimer", "time.Tick", "time.Now()", "rand."} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("controlplane.go contains %q; the client starts no goroutine and no timer and reads no wall clock", forbidden)
		}
	}
	var probe map[string]any
	if err = json.Unmarshal([]byte(`{}`), &probe); err != nil {
		t.Fatal(err)
	}
}
