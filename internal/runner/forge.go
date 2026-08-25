package runner

// Forge adapter (V2-064). The forge client is the gh CLI as a subprocess, not
// an HTTP client and not a Go forge library: go.mod and go.sum are prohibited
// paths for this change, so no dependency can be added at all, and an HTTP
// client would require the forge token to enter this process's environment --
// the one thing the whole credential design exists to prevent.
//
// Credential isolation is therefore proven as an absence, following the
// V2-017 d8 doctrine: the child receives the guarded base environment only,
// the granted set is the empty slice, and the Secret Broker is never
// consulted, because the CLI reads its own configuration store and takes no
// credential from argv or from the environment. Nothing in this file names
// that store's path, and internal/runner's own source guard proves no
// non-test file here references a path under the user's home directory.
//
// This adapter is read-only. It reads repository existence, default branch
// and viewer push permission, and it has no code path that creates,
// modifies or deletes anything on the forge.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// forgeExecutableName is the argv[0] basename this adapter will accept. The
// assertion filepath.Base(resolved) == forgeExecutableName runs before any
// process starts, mirroring V2-017 d6: it is what stops an attacker-supplied
// path from substituting a different binary behind a legitimate-looking
// configuration value.
const forgeExecutableName = "gh"

// forgeAPIPathPrefix is the read-only REST path this adapter reads.
const forgeAPIPathPrefix = "repos/"

var (
	// ErrForgeGrantSetNotEmpty refuses a call that was handed a non-empty
	// granted set. The forge adapter is defined by having none: a grant
	// would mean a credential had entered this process, which is exactly
	// what the design forbids.
	ErrForgeGrantSetNotEmpty = errors.New("forge: the granted credential set must be empty; the forge CLI reads its own store and takes no credential from argv or environment")
	// ErrForgeExecutableMissing refuses a call whose executable could not be
	// resolved to an existing regular file.
	ErrForgeExecutableMissing = errors.New("forge: the CLI executable was not found at an absolute path")
	// ErrForgeExecutableMismatch refuses a resolved path whose basename is
	// not the expected argv[0].
	ErrForgeExecutableMismatch = errors.New("forge: resolved executable basename does not match the expected tool")
	// ErrForgeCoordinateInvalid refuses an owner or name that is not a single
	// path segment, so a coordinate can never inject an extra API path.
	ErrForgeCoordinateInvalid = errors.New("forge: owner and name must each be one non-empty path segment")
	// ErrForgeResponseUnreadable reports that the CLI's output could not be
	// parsed into the bounded observation. Its message deliberately carries
	// none of that output.
	ErrForgeResponseUnreadable = errors.New("forge: the CLI response could not be parsed into the bounded observation")
	// ErrForgeUnreachable reports that the read did not complete. Its
	// message carries no process output either.
	ErrForgeUnreachable = errors.New("forge: the read-only repository read did not complete")
)

// ForgeObservation is everything this adapter is willing to return: parsed,
// bounded fields. No stdout, no stderr, no status line, no header and no
// response body is ever carried out of this package.
type ForgeObservation struct {
	Owner          string
	Name           string
	Exists         bool
	DefaultBranch  string
	ViewerCanPush  bool
	NodeID         string
	AdapterVersion string
}

// ForgeClient reads a repository's existence, default branch and viewer push
// permission through the gh CLI.
//
// ExecutablePath, when empty, is resolved with the same resolveTool helper
// confinement.go uses: the caller's PATH first, then the conventional system
// locations. That fallback is load-bearing rather than defensive: it is
// measured that the validated `devbox run --pure` environment has git on
// PATH but not gh, while the distribution's own gh is installed at its usual
// place, so a bare exec by name would work on the host and fail in the
// validated environment.
//
// GrantSet must stay empty. BaseEnvironmentNames names the variables that
// cross into the child; their values are read from this process and guarded.
type ForgeClient struct {
	ExecutablePath       string
	GrantSet             []string
	BaseEnvironmentNames []string
	AdapterVersion       string
	// Stat, when non-nil, replaces os.Stat. It exists so the refusal paths
	// can be exercised deterministically without touching the filesystem.
	Stat func(string) (os.FileInfo, error)
}

// DefaultForgeBaseEnvironmentNames is the guarded base environment a forge
// read needs: the home directory, so the CLI can find its own configuration
// store without this code naming it, and PATH.
var DefaultForgeBaseEnvironmentNames = []string{"HOME", "PATH"}

// NewForgeClient returns a client with the measured defaults.
func NewForgeClient(adapterVersion string) ForgeClient {
	return ForgeClient{GrantSet: nil, BaseEnvironmentNames: append([]string(nil), DefaultForgeBaseEnvironmentNames...), AdapterVersion: adapterVersion}
}

func (c ForgeClient) stat(path string) (os.FileInfo, error) {
	if c.Stat != nil {
		return c.Stat(path)
	}
	return os.Stat(path)
}

// ResolveExecutable resolves and validates the CLI path without starting
// anything. Every refusal here happens before a process could exist.
func (c ForgeClient) ResolveExecutable() (string, error) {
	if len(c.GrantSet) != 0 {
		return "", fmt.Errorf("%w: %d granted name(s) supplied", ErrForgeGrantSetNotEmpty, len(c.GrantSet))
	}
	candidate := strings.TrimSpace(c.ExecutablePath)
	if candidate == "" {
		candidate = resolveTool(forgeExecutableName)
	}
	if !filepath.IsAbs(candidate) {
		return "", fmt.Errorf("%w: %q is not an absolute path", ErrForgeExecutableMissing, candidate)
	}
	if filepath.Base(candidate) != forgeExecutableName {
		return "", fmt.Errorf("%w: basename(%s) != %s", ErrForgeExecutableMismatch, candidate, forgeExecutableName)
	}
	info, err := c.stat(candidate)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrForgeExecutableMissing, candidate)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: %s is a directory", ErrForgeExecutableMissing, candidate)
	}
	return candidate, nil
}

// validForgeSegment reports whether value is one non-empty path segment. It
// is deliberately stricter than the forge's own rules: no separator, no
// traversal, no leading dash (which would be read as a flag), no whitespace.
func validForgeSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	if strings.ContainsAny(value, "/\\ \t\r\n:?#&=%@") {
		return false
	}
	return !strings.HasPrefix(value, "-")
}

// ReadArgv is the complete argv of the read-only repository read, built
// without starting anything so it can be asserted directly. The method is
// stated explicitly rather than relied on as a default, so the argv itself
// witnesses that the call is a read.
func (c ForgeClient) ReadArgv(owner, name string) ([]string, error) {
	resolved, err := c.ResolveExecutable()
	if err != nil {
		return nil, err
	}
	if !validForgeSegment(owner) || !validForgeSegment(name) {
		return nil, fmt.Errorf("%w: owner=%q name=%q", ErrForgeCoordinateInvalid, owner, name)
	}
	return []string{resolved, "api", "--method", "GET", forgeAPIPathPrefix + owner + "/" + name}, nil
}

// VersionArgv is the argv that reports the CLI's own version, recorded as an
// execution-environment identifier. It reads nothing from the forge.
func (c ForgeClient) VersionArgv() ([]string, error) {
	resolved, err := c.ResolveExecutable()
	if err != nil {
		return nil, err
	}
	return []string{resolved, "--version"}, nil
}

// ChildEnvironment builds the guarded base environment the child receives. It
// is the guarded base environment and nothing else: the granted channel is
// empty by construction, so there is nothing for the Secret Broker to hand
// over and nothing for a grant to leak.
func (c ForgeClient) ChildEnvironment() ([]string, error) {
	if len(c.GrantSet) != 0 {
		return nil, fmt.Errorf("%w: %d granted name(s) supplied", ErrForgeGrantSetNotEmpty, len(c.GrantSet))
	}
	names := c.BaseEnvironmentNames
	if len(names) == 0 {
		names = DefaultForgeBaseEnvironmentNames
	}
	env, err := buildEnvironmentFromBaseNames(names)
	if err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}
	if err := GuardEnvironment(env, allowlistFromNames(names)); err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}
	return env, nil
}

// forgeRepositoryResponse is the only shape this adapter reads out of the
// CLI's output. Every other field of the response is discarded unparsed: the
// struct is the bound.
type forgeRepositoryResponse struct {
	FullName      string `json:"full_name"`
	NodeID        string `json:"node_id"`
	DefaultBranch string `json:"default_branch"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

// parseForgeResponse projects the CLI's output onto the bounded observation.
// On any failure it returns ErrForgeResponseUnreadable, whose message
// contains none of the input: no raw output is ever returned, journaled or
// logged verbatim.
func parseForgeResponse(raw []byte, owner, name, adapterVersion string) (ForgeObservation, error) {
	var parsed forgeRepositoryResponse
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&parsed); err != nil {
		return ForgeObservation{}, ErrForgeResponseUnreadable
	}
	if parsed.DefaultBranch == "" || parsed.NodeID == "" {
		return ForgeObservation{}, ErrForgeResponseUnreadable
	}
	if !strings.EqualFold(parsed.FullName, owner+"/"+name) {
		return ForgeObservation{}, ErrForgeResponseUnreadable
	}
	return ForgeObservation{
		Owner:          owner,
		Name:           name,
		Exists:         true,
		DefaultBranch:  parsed.DefaultBranch,
		ViewerCanPush:  parsed.Permissions.Push,
		NodeID:         parsed.NodeID,
		AdapterVersion: adapterVersion,
	}, nil
}

// ReadRepository performs the read-only forge read and returns the bounded
// observation. It starts exactly one process, with the resolved absolute
// path, the guarded base environment and no granted credential, and it
// returns no process output on any path.
func (c ForgeClient) ReadRepository(ctx context.Context, owner, name string) (ForgeObservation, error) {
	argv, err := c.ReadArgv(owner, name)
	if err != nil {
		return ForgeObservation{}, err
	}
	env, err := c.ChildEnvironment()
	if err != nil {
		return ForgeObservation{}, err
	}
	if err = GuardCommand(argv, env); err != nil {
		return ForgeObservation{}, fmt.Errorf("forge: %w", err)
	}
	raw, err := runForgeProcess(ctx, argv, env)
	if err != nil {
		// The child's own output is deliberately discarded here rather than
		// wrapped: a forge error body can carry a repository name, a scope
		// list or a rate-limit header, none of which belongs in a record.
		return ForgeObservation{}, fmt.Errorf("%w: %s/%s", ErrForgeUnreachable, owner, name)
	}
	return parseForgeResponse(raw, owner, name, c.AdapterVersion)
}

// ReadVersion returns the CLI's own reported version line, trimmed to its
// first line. It is an execution-environment identifier, not forge data.
func (c ForgeClient) ReadVersion(ctx context.Context) (string, error) {
	argv, err := c.VersionArgv()
	if err != nil {
		return "", err
	}
	env, err := c.ChildEnvironment()
	if err != nil {
		return "", err
	}
	raw, err := runForgeProcess(ctx, argv, env)
	if err != nil {
		return "", fmt.Errorf("%w: version read", ErrForgeUnreachable)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	return line, nil
}

// runForgeProcess starts exactly one child process with the already-resolved
// absolute argv and the already-guarded environment, captures its standard
// output into a bounded buffer, and returns either those bytes or an error.
// The child's standard error is discarded rather than captured, so there is
// no path by which a forge error body could reach a caller, a log or a
// record.
func runForgeProcess(ctx context.Context, argv []string, env []string) ([]byte, error) {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) || filepath.Base(argv[0]) != forgeExecutableName {
		return nil, ErrForgeExecutableMismatch
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append([]string(nil), env...)
	cmd.Stdin = nil
	cmd.Stderr = nil
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, remaining: maxForgeResponseBytes}
	if err := cmd.Run(); err != nil {
		return nil, ErrForgeUnreachable
	}
	return stdout.Bytes(), nil
}

// maxForgeResponseBytes bounds how much of the child's standard output this
// adapter is willing to hold. A repository read is a few kilobytes; the cap
// exists so a pathological response cannot become unbounded memory.
const maxForgeResponseBytes = 1 << 20

// limitedWriter drops everything past a hard byte cap. It never returns an
// error for the overflow, so a capped read fails at the parse step (which
// carries no output) rather than by surfacing the child's bytes in an error.
type limitedWriter struct {
	w         *bytes.Buffer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	take := p
	if len(take) > l.remaining {
		take = take[:l.remaining]
	}
	n, err := l.w.Write(take)
	l.remaining -= n
	if err != nil {
		return n, err
	}
	return len(p), nil
}
