package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
)

type CodexAdapter struct{}
type ClaudeAdapter struct{}
type OpenCodeAdapter struct{}

func (CodexAdapter) Name() string    { return "codex" }
func (ClaudeAdapter) Name() string   { return "claude" }
func (OpenCodeAdapter) Name() string { return "opencode" }

func build(executable string, flags []string, includeWorkspaceArg bool, req Request) (Invocation, error) {
	if err := req.Validate(); err != nil {
		return Invocation{}, err
	}
	b, err := json.Marshal(req.Packet)
	if err != nil || len(b) > MaxPacketBytes {
		return Invocation{}, ErrInvalidRequest
	}
	// A4: Build refuses a workspace it could use to ask to leave the
	// workspace, before any argv exists to carry it.
	if hasTraversalSegment(req.Workspace) {
		return Invocation{}, ErrInvalidRequest
	}
	argv := append([]string{executable}, flags...)
	if includeWorkspaceArg {
		argv = append(argv, req.Workspace)
	}
	for _, v := range argv {
		if strings.ContainsAny(v, "\x00\r\n") || secret.MatchString(v) {
			return Invocation{}, ErrInvalidRequest
		}
		if hasTraversalSegment(v) {
			return Invocation{}, ErrInvalidRequest
		}
	}
	return Invocation{Argv: argv, Stdin: b, WorkingDirectory: req.Workspace}, nil
}

// The three argv surfaces below are measured, not assumed. Every flag was read
// out of the CLI's own help output (V2-027 A3, help only: no subcommand of any
// Provider CLI was executed, which consumes no Provider usage and needs no
// authentication). The measurement is quoted verbatim in
// docs/operations/provider-adapters.md. No flag is present that help does not
// declare, and no flag help declares for a property the adapter needs was
// dropped.
//
// The workspace argument is kept as an argv element for codex and opencode,
// but the reason has changed and the old reason is kept here rather than
// deleted, because the conclusion was reached from it.
//
// HISTORICAL MEASUREMENT (2026-08-24, V2-027, true of the tree as it then
// stood): the runner's ProcessSupervisor.Run took only a context and an argv
// and never assigned a child working directory, and SupervisedInvocationRunner
// did not read Invocation.WorkingDirectory. For codex and opencode the flag
// was therefore the ONLY representation of the workspace that reached the
// child process at all, and deleting it would have left the child running in
// whatever directory the runner happened to be in.
//
// CURRENT MEASUREMENT (2026-08-25, V2-077): that premise no longer holds.
// ProcessSupervisor carries an additive Dir field and assigns it to the child,
// and SupervisedInvocationRunner reads Invocation.WorkingDirectory, validates
// it fail-closed on five properties before it loads a preflight record or
// debits a ledger reservation, and hands it to the supervisor. The declared
// working directory now reaches the child on its own, so the flag is no longer
// the only representation of the workspace.
//
// The flags still stay, for two reasons that do not depend on the old
// premise. First, each is declared by that CLI's own help output, and removing
// a flag a CLI declares is out of scope. Second, the double expression is
// harmless for the values that actually occur, and that is asserted rather
// than assumed: the single build call below produces the argv element and
// WorkingDirectory from the same req.Workspace, so they are the same string by
// construction (asserted for all three adapters by
// TestDirectoryFlagArgumentAndWorkingDirectoryAreTheSameString), and for an
// absolute path equal to the working directory a directory flag is idempotent.
// SupervisedInvocationRunner refuses fail-closed if the two ever disagree.
// What remains unmeasured is whether some future CLI version refuses when both
// are supplied; measuring that needs the run subcommand, which V2-028 owns.
//
// The kernel still holds the write boundary independently: NamespaceConfinement
// pins the writable mount at the workspace, and under confinement the chdir is
// expressed inside the namespace after that mount exists rather than through
// the exec's own directory. The adapter's job is only to be unable to ask to
// leave the workspace, which is what the argv guards below enforce.
//
// claude needs no such flag: it takes the Work Packet on stdin, and its four
// arguments are the one argv in this file that is live-proven wire-compatible
// against a real CLI, so not one of them is changed here.
func (CodexAdapter) Build(req Request) (Invocation, error) {
	return build("codex", []string{"exec", "--json", "--ephemeral", "-C"}, true, req)
}
func (ClaudeAdapter) Build(req Request) (Invocation, error) {
	return build("claude", []string{"--print", "--output-format", "json", "--no-session-persistence"}, false, req)
}
func (OpenCodeAdapter) Build(req Request) (Invocation, error) {
	return build("opencode", []string{"run", "--pure", "--format", "json", "--dir"}, true, req)
}

func (CodexAdapter) Parse(b []byte) (Result, error)      { return parseFixture("codex", b) }
func (ClaudeAdapter) Parse(b []byte) (Result, error)     { return parseFixture("claude", b) }
func (OpenCodeAdapter) Parse(b []byte) (Result, error)   { return parseFixture("opencode", b) }
func (CodexAdapter) NormalizeError(err error) Failure    { return normalize("codex", err) }
func (ClaudeAdapter) NormalizeError(err error) Failure   { return normalize("claude", err) }
func (OpenCodeAdapter) NormalizeError(err error) Failure { return normalize("opencode", err) }

// Run deliberately does not start a process in the core package. The local
// runner's process supervisor consumes Invocation and returns fixture bytes.
func (a CodexAdapter) Run(ctx context.Context, req Request) (Result, error)  { return NoExec(ctx, req) }
func (a ClaudeAdapter) Run(ctx context.Context, req Request) (Result, error) { return NoExec(ctx, req) }
func (a OpenCodeAdapter) Run(ctx context.Context, req Request) (Result, error) {
	return NoExec(ctx, req)
}

// hasTraversalSegment reports whether value contains a ".." path segment,
// under either separator, including as the whole value. It is a segment test
// and not a substring test on purpose: a directory legitimately named
// "..config" is not a traversal, and refusing it would be a false positive
// that pushes callers toward disabling the check.
func hasTraversalSegment(value string) bool {
	normalized := strings.ReplaceAll(value, "\\", "/")
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

type fixture struct {
	Status     string `json:"status"`
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	Success    *bool  `json:"success"`
	Succeeded  *bool  `json:"succeeded"`
	Checkpoint string `json:"checkpoint"`
	Output     string `json:"output"`
	Result     string `json:"result"`
	Summary    string `json:"summary"`
	Error      string `json:"error"`
	Code       string `json:"code"`
	ExitCode   *int   `json:"exit_code"`
	// Usage is a pointer so that an absent usage object is a different fact
	// from a usage object whose counts are all zero (V2-027 A6).
	Usage *Usage `json:"usage"`
}

func parseFixture(name string, b []byte) (Result, error) {
	if len(b) == 0 || len(b) > MaxFixtureBytes {
		return Result{}, ErrInvalidFixture
	}
	if secret.Match(b) {
		return Result{}, ErrInvalidFixture
	}
	var f fixture
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&f) != nil {
		return Result{}, ErrInvalidFixture
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Result{}, ErrInvalidFixture
	}
	r := Result{Provider: name, Checkpoint: f.Checkpoint}
	if f.Usage != nil {
		r.Usage = *f.Usage
		r.UsageReported = true
	}
	success := f.Success
	if success == nil {
		success = f.Succeeded
	}
	if success == nil {
		switch strings.ToLower(f.Status) {
		case "success", "succeeded", "completed", "ok":
			v := true
			success = &v
		case "error", "failed", "failure":
			v := false
			success = &v
		}
	}
	if success == nil {
		if f.Type == "result" && f.Subtype == "success" {
			v := true
			success = &v
		} else {
			return Result{}, ErrInvalidFixture
		}
	}
	r.Succeeded = *success
	out := f.Output
	if out == "" {
		out = f.Result
	}
	if out == "" {
		out = f.Summary
	}
	if out != "" {
		r.OutputDigest = digest(out)
	}
	if !r.Succeeded {
		fai := Failure{Class: FailureModel, Retryable: false, Code: f.Code, Message: safeMessage(f.Error)}
		if f.ExitCode != nil && *f.ExitCode != 0 {
			fai = Failure{Class: FailureTransport, Retryable: true, Code: strconv.Itoa(*f.ExitCode), Message: "provider exited unsuccessfully"}
		}
		if strings.Contains(strings.ToLower(f.Error), "quota") || strings.Contains(strings.ToLower(f.Error), "rate limit") {
			fai = Failure{Class: FailureQuota, Retryable: true, Code: f.Code, Message: "provider quota exhausted"}
		}
		r.Failure = &fai
	}
	if err := r.Validate(); err != nil {
		return Result{}, err
	}
	return r, nil
}
func normalize(_ string, err error) Failure {
	if err == nil {
		return Failure{}
	}
	// These two come first and return early. An output the adapter cannot
	// parse is a contract disagreement, not a transport fault: reporting it as
	// transport would send a reader looking for a network problem and would
	// make it retryable, when a retry cannot change either side's output
	// shape. An invalid request is about our own request and is not an
	// observation about the Provider at all.
	if errors.Is(err, ErrInvalidFixture) {
		return Failure{Class: FailureContract, Retryable: false, Message: "provider output is not a shape this adapter can parse"}
	}
	if errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrInvalidPacket) {
		return Failure{Class: FailureInvalidInput, Retryable: false, Message: "provider request is not valid for this adapter"}
	}
	f := classify(err)
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "quota") || strings.Contains(s, "rate limit") || strings.Contains(s, "429") {
		f.Class = FailureQuota
		f.Retryable = true
		f.Message = "provider quota exhausted"
	}
	if strings.Contains(s, "model") || strings.Contains(s, "overloaded") {
		f.Class = FailureModel
		f.Retryable = false
		f.Message = "provider model failure"
	}
	// The unauthenticated test comes last and therefore wins over the two
	// above: a CLI that has no session on this machine commonly answers with
	// an authentication phrase that also carries a transport-shaped or
	// model-shaped word, and reporting that as transport or model is exactly
	// the misdirection FailureUnauthenticated exists to remove (dp-v2-067
	// d9). The message is a fixed literal: no provider text is ever copied
	// into the Failure, so the existing safeMessage redaction path has
	// nothing new to redact here.
	if unauthenticated.MatchString(s) {
		f.Class = FailureUnauthenticated
		f.Retryable = false
		f.Ambiguous = false
		f.Message = "provider cli has no authenticated session on this machine"
	}
	return f
}

// unauthenticated matches the phrases a local AI CLI uses when it is
// installed and reachable but has no logged-in session. It is matched against
// the lowercased error text only; nothing it matches is ever copied into the
// Failure.
var unauthenticated = regexp.MustCompile(`(not (logged in|authenticated|signed in)|unauthenticated|unauthori[sz]ed|authentication (required|failed)|(please |must )?(log ?in|sign ?in) (required|first|to continue)|no (active )?(session|credentials) found|\bhttp 401\b|\b401\b)`)

// ParseOrClassify is the single entry point the nine-case contract-fixture
// table uses for all three adapters. It runs Adapter.Parse and, when the bytes
// are not the projected shape, returns the Failure the same adapter's own
// NormalizeError produces for that error, so a cell whose bytes cannot parse
// still has a FailureClass, a Retryable and an Ambiguous value to assert.
// Routing every cell through one function is what makes a change to build,
// parseFixture or normalize visibly reach all three adapters.
func ParseOrClassify(a Adapter, raw []byte) (Result, Failure) {
	r, err := a.Parse(raw)
	if err != nil {
		return Result{Provider: a.Name()}, a.NormalizeError(err)
	}
	if r.Failure != nil {
		return r, *r.Failure
	}
	return r, Failure{}
}

// Run is intentionally unavailable in this package: the local process
// supervisor owns execution. Adapters only build argv and parse fixtures.
func NoExec(context.Context, Request) (Result, error) {
	return Result{}, errors.New("provider CLI execution is outside the adapter contract")
}
