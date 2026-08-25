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
	argv := append([]string{executable}, flags...)
	if includeWorkspaceArg {
		argv = append(argv, req.Workspace)
	}
	for _, v := range argv {
		if strings.ContainsAny(v, "\x00\r\n") || secret.MatchString(v) {
			return Invocation{}, ErrInvalidRequest
		}
	}
	return Invocation{Argv: argv, Stdin: b, WorkingDirectory: req.Workspace}, nil
}
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
	Usage      Usage  `json:"usage"`
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
	r := Result{Provider: name, Checkpoint: f.Checkpoint, Usage: f.Usage}
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

// Run is intentionally unavailable in this package: the local process
// supervisor owns execution. Adapters only build argv and parse fixtures.
func NoExec(context.Context, Request) (Result, error) {
	return Result{}, errors.New("provider CLI execution is outside the adapter contract")
}
