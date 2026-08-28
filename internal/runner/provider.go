package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// ProviderRequest is the runner's provider-neutral invocation boundary. It
// carries a provider.WorkPacket -- facts and evidence only -- and never a raw
// prompt: no Prompt field exists on this type, so a raw prompt is structurally
// unrepresentable rather than merely asserted absent.
type ProviderRequest struct {
	OperationID string
	Packet      provider.WorkPacket
	Workspace   string
}
type ProviderResult struct {
	Output     string
	Succeeded  bool
	Checkpoint string
}
type Provider interface {
	Run(context.Context, ProviderRequest) (ProviderResult, error)
}

// CodexSyntheticAdapter models the provider boundary without starting Codex
// or reading credentials. Production wiring must replace it explicitly.
type CodexSyntheticAdapter struct{}

func (CodexSyntheticAdapter) Run(ctx context.Context, req ProviderRequest) (ProviderResult, error) {
	select {
	case <-ctx.Done():
		return ProviderResult{}, ctx.Err()
	default:
	}
	if req.OperationID == "" || req.Workspace == "" {
		return ProviderResult{}, errors.New("provider request is incomplete")
	}
	return ProviderResult{Output: fmt.Sprintf("synthetic codex completed %s", req.OperationID), Succeeded: true, Checkpoint: "synthetic:" + req.OperationID}, nil
}

type FakeProvider struct {
	mu     sync.Mutex
	Calls  []ProviderRequest
	Result ProviderResult
	Err    error
}

func (f *FakeProvider) Run(ctx context.Context, req ProviderRequest) (ProviderResult, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, req)
	result, err := f.Result, f.Err
	f.mu.Unlock()
	if err != nil {
		return ProviderResult{}, err
	}
	select {
	case <-ctx.Done():
		return ProviderResult{}, ctx.Err()
	default:
		return result, nil
	}
}

// InvocationRunner is the seam between a built provider.Invocation and the
// bytes an adapter can parse. It never itself decides success/failure; it
// only produces (or fails to produce) fixture bytes for Adapter.Parse.
type InvocationRunner interface {
	Run(ctx context.Context, inv provider.Invocation) ([]byte, error)
}

// FakeInvocationRunner never starts a process. It records every Invocation it
// is given and returns a fixed fixture.
//
// HISTORICAL MEASUREMENT, 2026-08-25 (V2-077): this doc said the recorded
// Invocation included "its Environment, so a Secret Broker grant merged onto
// the Invocation is observable by a test as the positive control for A11".
// That was true of the fake and false of every real child: this fake was the
// ONLY reader of provider.Invocation's Environment field in the tree.
//
// CURRENT MEASUREMENT, 2026-08-26 (V2-078): the field is gone, so what this
// fake records is exactly {Argv, Stdin, WorkingDirectory}. A11's positive
// control now reads grant.Environment() directly, which is the same value one
// hop earlier and is where the credential actually lives.
type FakeInvocationRunner struct {
	mu      sync.Mutex
	Calls   []provider.Invocation
	Fixture []byte
	Err     error
}

func (f *FakeInvocationRunner) Run(_ context.Context, inv provider.Invocation) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, inv)
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Fixture, nil
}

// CallCount reports how many times Run has been invoked so far.
func (f *FakeInvocationRunner) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// ErrSupervisedInvocationRunnerIncomplete is returned by Run (and starts no
// process) whenever any required dependency is missing. There is no
// exported constructor or field combination that lets a caller reach the
// exec step without a CostLedger and a runtime invocation policy
// (dp-v2-017 B4): every field below is required, and Run checks all of them
// before doing anything else.
var ErrSupervisedInvocationRunnerIncomplete = errors.New("supervised invocation runner dependencies are incomplete")

// ErrInvocationWorkingDirectoryUnusable is returned by
// SupervisedInvocationRunner.Run -- with no process started, no
// invocation policy accepted and no ledger reservation debited -- when
// the working directory the Invocation declares cannot be used as-is
// (V2-077).
//
// There is deliberately no fallback: an unusable value is never repaired and
// never replaced by the Runner's own current directory. Substituting the
// Runner's directory is precisely the defect V2-077 removes, and making it
// the documented fallback would make the correct and the broken case
// indistinguishable from outside.
//
// The refusal is fail-closed on five properties of the value itself, each
// wrapped with its own distinct reason: it must be non-empty, absolute,
// already canonical (filepath.Clean leaves it unchanged, which rejects a
// ".." segment, a trailing separator and a doubled separator alike), an
// existing directory, and not a symlink. When the supervisor is confined it
// must additionally be the confinement workspace itself or a path beneath
// it, because with Confine set the kernel makes exactly one path read-write
// and a cwd outside it would put the child where its own workspace is
// sealed. Finally, when argv itself carries a Provider CLI's own directory
// flag, the element that flag carries must be identical to the declared
// working directory: the two are the same string by construction today (one
// call to provider's shared build helper produces both from the same
// request workspace), so a disagreement means something rebuilt one of them
// and is refused rather than silently preferred one way or the other.
var ErrInvocationWorkingDirectoryUnusable = errors.New("supervised invocation runner: invocation working directory is unusable")

// ErrInvocationEnvironmentGrantUndeliverable is returned by
// SupervisedInvocationRunner.Run -- with no process started and no ledger
// reservation debited -- when the runtime invocation policy declares
// a non-empty environment.granted_names (V2-078, dp-v2-078 d3).
//
// The runner builds the child's environment solely from the record's
// environment.base_names and hands it to a supervisor that REPLACES the
// parent environment, so there is no channel through which a granted name
// could reach the child. Before V2-078 a declaration of this shape was
// silently dropped: a field on provider.Invocation looked like the channel,
// nothing ever wrote it and nothing read it. Deleting that field without
// adding this refusal would have converted a silently-dropped grant into a
// silently-ignored declaration, which is the same defect one level up --
// environment.granted_names is schema-required, semantically enforced (a
// non-empty value is forbidden for provider.name claude) and bound by the
// approval digest, so it carries real enforcement that must be honoured
// rather than deleted.
//
// The refusal is therefore fail-closed with no fallback and no repair: the
// declaration is never partially delivered, never rewritten and never
// ignored. Its position is load-bearing rather than tidy -- after
// LoadPreflightRecord, because it needs the record, and STRICTLY before
// Ledger.Reserve, because a reservation debited before a refusal stays
// charged at worst case forever (dp-v2-017 d9). When a delivery path is
// built, in the shape dp-v2-078 d7 names, whoever builds it deletes this
// refusal in the same change.
var ErrInvocationEnvironmentGrantUndeliverable = errors.New("supervised invocation runner: the runtime invocation policy declares environment names the runner has no channel to deliver; refusing rather than running the child with a silently narrower environment")

// validateInvocationWorkingDirectory implements the five fail-closed
// properties named on ErrInvocationWorkingDirectoryUnusable, plus the
// confinement containment clause. It touches nothing and starts nothing: it
// only reads directory metadata, so it is safe to run before a
// runtime invocation policy is accepted and before a ledger reservation is
// debited.
func validateInvocationWorkingDirectory(dir string, confine *NamespaceConfinement) error {
	if dir == "" {
		return fmt.Errorf("%w: the invocation declares no working directory, and substituting the runner's own current directory is forbidden", ErrInvocationWorkingDirectoryUnusable)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: %s is not an absolute path", ErrInvocationWorkingDirectoryUnusable, dir)
	}
	if filepath.Clean(dir) != dir {
		return fmt.Errorf("%w: %s is not canonical (a traversal segment, a trailing separator or a doubled separator)", ErrInvocationWorkingDirectoryUnusable, dir)
	}
	lstat, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w: %s cannot be inspected (it does not exist, or its parent is unreadable): %v", ErrInvocationWorkingDirectoryUnusable, dir, err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink, so a chdir into it would put the child somewhere no argv guard inspected", ErrInvocationWorkingDirectoryUnusable, dir)
	}
	if !lstat.IsDir() {
		return fmt.Errorf("%w: %s exists but is not a directory", ErrInvocationWorkingDirectoryUnusable, dir)
	}
	if confine != nil && !pathWithinWorkspace(confine.Workspace, dir) {
		return fmt.Errorf("%w: %s is neither the confinement workspace nor a path beneath it, and the confinement is never relaxed to make a directory take effect", ErrInvocationWorkingDirectoryUnusable, dir)
	}
	return nil
}

// providerDirectoryFlags are the directory flags the three Provider CLIs'
// own help output declares and the adapters therefore emit: codex's -C and
// opencode's --dir. claude declares none and carries no path in argv at all.
// They are not removed, renamed or reordered here; this list exists only so
// the Runner can prove that the flag's argument and the declared working
// directory are the same string rather than assume it.
var providerDirectoryFlags = []string{"-C", "--dir"}

// directoryFlagArgument returns the first directory flag present in argv and
// the element immediately following it. found is false when argv carries no
// directory flag at all (which is the case for claude).
func directoryFlagArgument(argv []string) (flag string, value string, found bool) {
	for i, element := range argv {
		for _, candidate := range providerDirectoryFlags {
			if element != candidate {
				continue
			}
			if i+1 < len(argv) {
				return element, argv[i+1], true
			}
			return element, "", true
		}
	}
	return "", "", false
}

// SupervisedInvocationRunner is the seam a real Provider CLI fills: it runs
// a real provider.Invocation through ProcessSupervisor (the package's only
// other reference to that type, dp-v2-017 B4/TestProcessSupervisorReferenced
// ExactlyOnceInProviderExecutionPath) after first debiting a CostLedger
// reservation against the active runtime invocation policy
// (dp-v2-017 d1). The order inside Run is strictly: (1) refuse if any
// dependency is nil/empty, and refuse if the Invocation's declared working
// directory is unusable (V2-077, see ErrInvocationWorkingDirectoryUnusable);
// (2) LoadPreflightRecord (schema + repository-wide
// validation, freshly read from disk); (3) Ledger.Reserve, which persists a
// reservation to disk before anything may execute; (4) resolve argv[0] to
// the approved record's absolute executable_path and exec through
// ProcessSupervisor with the invocation's Stdin wired to the child's stdin,
// its stdout captured in memory and its validated WorkingDirectory set as
// the supervisor's Dir (V2-077), so the child actually runs in the
// Increment's workspace rather than wherever the Runner was started; (5) project the real CLI's stdout, in
// memory, into the minimal fixture shape provider.ClaudeAdapter.Parse
// accepts (dp-v2-017 d5) -- the raw stdout bytes are never written to any
// file and are dropped as soon as the projection is built; (6)
// Ledger.TrueUp from the parsed total_cost_usd and usage. If the process is
// cancelled or killed before it produces output, TrueUp is skipped entirely
// so the reservation stays charged at worst case forever (dp-v2-017 d9's
// risk note) and no result is ever produced to journal.
type SupervisedInvocationRunner struct {
	Supervisor ProcessSupervisor
	Log        *BoundedLog
	Ledger     *CostLedger
	// Policy is supplied directly by the active Runner session. No tracked
	// handoff, preflight or evidence file participates in invocation.
	Policy InvocationPolicy
	// Purpose names, for the ledger and for evidence, which named
	// invocation (e.g. "V2-017-I1-happy-journey") this Run call is.
	Purpose string
	// Now, if set, replaces time.Now().UTC() everywhere this type needs a
	// timestamp (tests only; production leaves it nil).
	Now func() time.Time
	// ExtraEnv is additive, "NAME=value" environment entries merged onto
	// the base_names-derived environment for this one Run call only
	// (dp-v2-017 B16/I7: inducing a deterministic transport failure by
	// pointing the CLI at an unreachable base URL needs one diagnostic
	// override that is not part of the approved record's base_names). It
	// is nil for every other invocation in this task; when nil, the
	// environment the child receives stays exactly set-equal to
	// environment.base_names (dp-v2-017 d8(c)). Re-measured 2026-08-26
	// (V2-078): the claim used to be phrased about
	// "Invocation.Environment", a field that no longer exists; the property
	// it names is unchanged, because the environment was always built here
	// and never read off the Invocation.
	ExtraEnv []string
}

func (r SupervisedInvocationRunner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// buildEnvironmentFromBaseNames returns "NAME=value" pairs for exactly
// baseNames, sourced from this process's own environment. It fails closed
// (returns an error, builds nothing) if any declared base name is not
// actually set, so the built environment can never silently be a subset of
// what the approved record declares (dp-v2-017 d8(c): the environment handed
// to the child must be set-equal to environment.base_names; re-measured
// 2026-08-26 for V2-078, which deleted the provider.Invocation field that
// clause used to be phrased in terms of without changing the property).
func buildEnvironmentFromBaseNames(baseNames []string) ([]string, error) {
	env := make([]string, 0, len(baseNames))
	for _, name := range baseNames {
		v, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("required base environment variable %s is not set in this process", name)
		}
		env = append(env, name+"="+v)
	}
	return env, nil
}

func allowlistFromNames(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// realCLIOutcome is the subset of the real claude CLI's reported JSON that
// SupervisedInvocationRunner projects and settles (dp-v2-017 B7/B12/d5). It
// is never itself handed to provider.Adapter.Parse or persisted anywhere:
// only the projection produced from it (whose "output" field is a sha256
// digest, never response text) may reach the journal, the bounded log, the
// ledger's SessionID field, or evidence.
type realCLIOutcome struct {
	Classification     string
	SessionID          string
	TotalCostUSD       float64
	InputCount         int64
	OutputCount        int64
	CacheReadCount     int64
	CacheCreationCount int64
	DurationAPIMS      int64
	DurationMS         int64
	NumTurns           int
}

// projectRealCLIResult turns raw real-CLI stdout bytes into the minimal
// fixture shape provider.ClaudeAdapter.Parse accepts (dp-v2-017 d5). raw is
// read only here, in memory, and never persisted; the returned []byte is the
// only representation of the response that may reach Adapter.Parse, the
// journal or evidence, and it carries a sha256 digest of the response text
// rather than the text itself.
func projectRealCLIResult(providerName string, raw []byte, runErr error) ([]byte, realCLIOutcome, error) {
	body := raw
	if len(body) > provider.MaxFixtureBytes {
		body = body[:provider.MaxFixtureBytes]
	}
	if providerName == "codex" || providerName == "opencode" {
		return projectJSONLCLIResult(providerName, body, runErr)
	}
	var real struct {
		Type          string  `json:"type"`
		Subtype       string  `json:"subtype"`
		IsError       bool    `json:"is_error"`
		DurationMS    int64   `json:"duration_ms"`
		DurationAPIMS int64   `json:"duration_api_ms"`
		NumTurns      int     `json:"num_turns"`
		Result        string  `json:"result"`
		SessionID     string  `json:"session_id"`
		TotalCostUSD  float64 `json:"total_cost_usd"`
		Usage         struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	parseErr := json.Unmarshal(body, &real)
	outcome := realCLIOutcome{
		SessionID:          real.SessionID,
		TotalCostUSD:       real.TotalCostUSD,
		InputCount:         real.Usage.InputTokens,
		OutputCount:        real.Usage.OutputTokens,
		CacheReadCount:     real.Usage.CacheReadInputTokens,
		CacheCreationCount: real.Usage.CacheCreationInputTokens,
		DurationAPIMS:      real.DurationAPIMS,
		DurationMS:         real.DurationMS,
		NumTurns:           real.NumTurns,
	}
	if parseErr == nil && real.Type == "result" && real.Subtype == "success" && !real.IsError {
		outcome.Classification = "success"
		fixture := map[string]any{
			"type":       "result",
			"subtype":    "success",
			"checkpoint": "claude:" + real.SessionID,
			"output":     provider.DigestOutput(real.Result),
			"usage": map[string]any{
				"input_tokens":  real.Usage.InputTokens,
				"output_tokens": real.Usage.OutputTokens,
				"total_tokens":  real.Usage.InputTokens + real.Usage.OutputTokens,
			},
		}
		b, mErr := json.Marshal(fixture)
		return b, outcome, mErr
	}
	outcome.Classification = "error"
	exitCode := 1
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	code := real.Subtype
	if code == "" {
		code = "transport"
	}
	fixture := map[string]any{
		"type":      "result",
		"subtype":   "error",
		"status":    "error",
		"code":      code,
		"error":     "provider run did not succeed (redacted)",
		"exit_code": exitCode,
	}
	b, mErr := json.Marshal(fixture)
	return b, outcome, mErr
}

// projectJSONLCLIResult consumes the documented JSONL event streams emitted by
// `codex exec --json` and `opencode run --format json`. It retains only the
// provider-issued session identifier, aggregate usage/cost and a digest of the
// final assistant text. Raw events and response text never leave this function.
func projectJSONLCLIResult(providerName string, raw []byte, runErr error) ([]byte, realCLIOutcome, error) {
	var outcome realCLIOutcome
	var finalText string
	decoder := json.NewDecoder(bytes.NewReader(raw))
	seen := false
	for {
		var event struct {
			Type      string `json:"type"`
			ThreadID  string `json:"thread_id"`
			SessionID string `json:"sessionID"`
			Usage     struct {
				InputTokens       int64 `json:"input_tokens"`
				CachedInputTokens int64 `json:"cached_input_tokens"`
				OutputTokens      int64 `json:"output_tokens"`
			} `json:"usage"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Part struct {
				Type    string  `json:"type"`
				Text    string  `json:"text"`
				Cost    float64 `json:"cost"`
				Session string  `json:"sessionID"`
				Tokens  struct {
					Input     int64 `json:"input"`
					Output    int64 `json:"output"`
					Reasoning int64 `json:"reasoning"`
					Cache     struct {
						Read  int64 `json:"read"`
						Write int64 `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
			} `json:"part"`
		}
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, outcome, err
		}
		seen = true
		if event.ThreadID != "" {
			outcome.SessionID = event.ThreadID
		}
		if event.SessionID != "" {
			outcome.SessionID = event.SessionID
		}
		if event.Part.Session != "" {
			outcome.SessionID = event.Part.Session
		}
		if event.Item.Type == "agent_message" && event.Item.Text != "" {
			finalText = event.Item.Text
		}
		if event.Part.Type == "text" && event.Part.Text != "" {
			finalText += event.Part.Text
		}
		if event.Usage.InputTokens != 0 || event.Usage.OutputTokens != 0 || event.Usage.CachedInputTokens != 0 {
			outcome.InputCount = event.Usage.InputTokens
			outcome.OutputCount = event.Usage.OutputTokens
			outcome.CacheReadCount = event.Usage.CachedInputTokens
		}
		if event.Part.Tokens.Input != 0 || event.Part.Tokens.Output != 0 || event.Part.Tokens.Reasoning != 0 || event.Part.Tokens.Cache.Read != 0 || event.Part.Tokens.Cache.Write != 0 {
			outcome.InputCount += event.Part.Tokens.Input
			outcome.OutputCount += event.Part.Tokens.Output + event.Part.Tokens.Reasoning
			outcome.CacheReadCount += event.Part.Tokens.Cache.Read
			outcome.CacheCreationCount += event.Part.Tokens.Cache.Write
		}
		outcome.TotalCostUSD += event.Part.Cost
	}
	if !seen || finalText == "" || runErr != nil {
		outcome.Classification = "error"
		exitCode := 1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		fixture, err := json.Marshal(map[string]any{"status": "error", "code": "transport", "error": "provider run did not succeed (redacted)", "exit_code": exitCode})
		return fixture, outcome, err
	}
	outcome.Classification = "success"
	fixture, err := json.Marshal(map[string]any{
		"status": "success", "checkpoint": providerName + ":" + outcome.SessionID,
		"output": provider.DigestOutput(finalText),
		"usage":  map[string]any{"input_tokens": outcome.InputCount, "output_tokens": outcome.OutputCount, "total_tokens": outcome.InputCount + outcome.OutputCount},
	})
	return fixture, outcome, err
}

// wasSignaled reports whether err wraps an *exec.ExitError whose process
// was terminated by a signal (e.g. a real external SIGKILL to the process
// group, as I5 sends directly) rather than exiting on its own, even
// unsuccessfully (I7's transport failure exits normally with a non-zero
// code, which wasSignaled correctly reports as false).
func wasSignaled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// Run implements InvocationRunner for a real, supervised claude CLI
// process. See the type doc comment for the exact step order.
func (r SupervisedInvocationRunner) Run(ctx context.Context, inv provider.Invocation) ([]byte, error) {
	if r.Ledger == nil || r.Log == nil || r.Policy.ProviderName == "" || r.Purpose == "" {
		return nil, ErrSupervisedInvocationRunnerIncomplete
	}
	if len(inv.Argv) == 0 || inv.Argv[0] == "" {
		return nil, errors.New("supervised invocation runner: invocation argv is required")
	}
	// V2-077: the declared working directory is validated here, inside step
	// (1), and therefore strictly before LoadPreflightRecord and before
	// Ledger.Reserve. The position is load-bearing rather than tidy: a
	// reservation debited before a refusal stays charged at worst case
	// forever (dp-v2-017 d9), so a refusal placed after Reserve would spend
	// a real reservation to report a caller's own malformed request.
	if err := validateInvocationWorkingDirectory(inv.WorkingDirectory, r.Supervisor.Confine); err != nil {
		return nil, err
	}
	if flag, value, ok := directoryFlagArgument(inv.Argv); ok && value != inv.WorkingDirectory {
		return nil, fmt.Errorf("%w: argv carries the directory flag %s with %q while the invocation declares %q; the two are one string by construction, so a disagreement is refused rather than resolved", ErrInvocationWorkingDirectoryUnusable, flag, value, inv.WorkingDirectory)
	}

	record := r.Policy
	// V2-078: a declared grant the runner cannot deliver stops the
	// invocation here, after the record is loaded (it is the record that
	// declares it) and strictly before Ledger.Reserve (a reservation
	// debited before a refusal stays charged at worst case forever,
	// dp-v2-017 d9). Nothing is repaired and nothing is dropped.
	if len(record.EnvironmentGranted) != 0 {
		return nil, fmt.Errorf("%w: the record declares %d granted name(s)", ErrInvocationEnvironmentGrantUndeliverable, len(record.EnvironmentGranted))
	}

	adapterArgv0 := inv.Argv[0]
	seq, err := r.Ledger.Reserve(record, adapterArgv0, r.Purpose, r.now())
	if err != nil {
		return nil, err
	}

	env, err := buildEnvironmentFromBaseNames(record.EnvironmentBaseNames)
	if err != nil {
		return nil, fmt.Errorf("supervised invocation runner: %w", err)
	}
	allowlistNames := append([]string(nil), record.EnvironmentBaseNames...)
	if len(r.ExtraEnv) > 0 {
		env = append(append([]string(nil), env...), r.ExtraEnv...)
		for _, kv := range r.ExtraEnv {
			if name, _, ok := strings.Cut(kv, "="); ok {
				allowlistNames = append(allowlistNames, name)
			}
		}
	}
	if err := GuardEnvironment(env, allowlistFromNames(allowlistNames)); err != nil {
		return nil, fmt.Errorf("supervised invocation runner: %w", err)
	}

	resolvedArgv := append([]string(nil), inv.Argv...)
	resolvedArgv[0] = record.ExecutablePath
	if err := GuardCommand(resolvedArgv, env); err != nil {
		return nil, fmt.Errorf("supervised invocation runner: %w", err)
	}

	var stdout bytes.Buffer
	sup := r.Supervisor
	sup.Stdin = inv.Stdin
	sup.Stdout = &stdout
	sup.Env = env
	// V2-077: the validated directory is handed to the one type that can
	// physically assign it. Assignment and policy are split on purpose:
	// nothing else in this package sets a child directory, and nothing in
	// ProcessSupervisor knows what a provider.Invocation is.
	sup.Dir = inv.WorkingDirectory

	_ = r.Log.Write(fmt.Sprintf("invocation purpose=%s seq=%d argv0_basename=%s starting", r.Purpose, seq, filepath.Base(resolvedArgv[0])))
	runErr := sup.Run(ctx, resolvedArgv)
	raw := stdout.Bytes()

	if runErr != nil && (errors.Is(runErr, context.Canceled) || wasSignaled(runErr)) {
		// I5/I8 (dp-v2-017 B15/B17): killed (a real external SIGKILL to the
		// process group, I5) or cancelled through ctx (I8) before
		// completion. Either way the true cost is unknowable from outside,
		// so TrueUp is deliberately never called: the reservation stays
		// charged at worst case forever, and no result is ever produced
		// for a caller to journal.
		_ = r.Log.Write(RedactLog(fmt.Sprintf("invocation purpose=%s seq=%d killed/cancelled before completion", r.Purpose, seq)))
		return nil, fmt.Errorf("supervised invocation runner: %w", runErr)
	}

	projected, outcome, projErr := projectRealCLIResult(record.ProviderName, raw, runErr)
	settleErr := r.Ledger.TrueUp(seq, Settlement{
		ActualUSD:          outcome.TotalCostUSD,
		SessionID:          outcome.SessionID,
		InputCount:         outcome.InputCount,
		OutputCount:        outcome.OutputCount,
		CacheReadCount:     outcome.CacheReadCount,
		CacheCreationCount: outcome.CacheCreationCount,
		DurationAPIMS:      outcome.DurationAPIMS,
		DurationMS:         outcome.DurationMS,
		NumTurns:           outcome.NumTurns,
	}, r.now())
	_ = r.Log.Write(fmt.Sprintf("invocation purpose=%s seq=%d classification=%s finished", r.Purpose, seq, outcome.Classification))
	if settleErr != nil {
		_ = r.Log.Write(RedactLog(fmt.Sprintf("invocation purpose=%s seq=%d true-up: %v", r.Purpose, seq, settleErr)))
		return projected, settleErr
	}
	if projErr != nil {
		return nil, projErr
	}
	return projected, nil
}

// ProviderClient composes a provider.Adapter (Build/Parse) with an
// InvocationRunner seam. It is the local Provider implementation that carries
// a real, validated Work Packet through Build and Parse without starting a
// real Provider CLI in this task.
// HISTORICAL MEASUREMENT, 2026-08-25 (V2-077): this type carried a third
// field, Grant *Grant, documented as "merged into the built Invocation's
// Environment immediately before the seam runs it". Measured: no file in the
// tree -- test or otherwise -- ever assigned it, and the merge it guarded
// wrote a provider.Invocation field that SupervisedInvocationRunner never
// read.
//
// CURRENT MEASUREMENT, 2026-08-26 (V2-078): the field and the merge are gone
// (dp-v2-078 route (b)). This type composes an adapter with an
// InvocationRunner seam and contributes nothing to the child's environment;
// the child's environment has exactly one authority, the approved
// runtime invocation policy that SupervisedInvocationRunner.Run receives.
type ProviderClient struct {
	Adapter provider.Adapter
	Runner  InvocationRunner
}

func (c ProviderClient) Run(ctx context.Context, req ProviderRequest) (ProviderResult, error) {
	if c.Adapter == nil || c.Runner == nil {
		return ProviderResult{}, errors.New("provider client dependencies are incomplete")
	}
	if err := req.Packet.Validate(); err != nil {
		return ProviderResult{}, fmt.Errorf("work packet: %w", err)
	}
	inv, err := c.Adapter.Build(provider.Request{OperationID: req.OperationID, Workspace: req.Workspace, Packet: req.Packet})
	if err != nil {
		return ProviderResult{}, err
	}
	raw, err := c.Runner.Run(ctx, inv)
	if err != nil {
		return ProviderResult{}, err
	}
	res, err := c.Adapter.Parse(raw)
	if err != nil {
		return ProviderResult{}, err
	}
	out := ProviderResult{Succeeded: res.Succeeded, Checkpoint: res.Checkpoint, Output: res.OutputDigest}
	if !res.Succeeded {
		msg := "provider run did not succeed"
		if res.Failure != nil && res.Failure.Message != "" {
			msg = res.Failure.Message
		}
		return out, errors.New(msg)
	}
	return out, nil
}
