package runner

// The one Git adapter (V2-071). It is the only implementation of the
// SourceControl port declared in internal/runner/sourcecontrol.go, and the
// only place in this repository that starts a git child.
//
// Four properties are structural rather than promised:
//
//  1. The executable is resolved to an absolute path whose filepath.Base is
//     "git" and which stats to a regular file, all before any process could
//     exist (the V2-017 d6 idiom internal/runner/forge.go already uses).
//  2. The subcommand comes from the closed allowlist in sourcecontrol.go.
//     push, fetch, remote, submodule, config, credential and worktree are
//     absent from it, so no argv naming them is constructible here.
//  3. Every child except `git --version` runs inside NamespaceConfinement,
//     whose Workspace is the Execution's own workspace directory. It was
//     measured (git 2.55.0, Linux WSL2 kernel, unshare --user
//     --map-root-user --mount) that inside that namespace a clone from a
//     local origin succeeds and lands entirely in the workspace, while a
//     write to the origin, a write to the sealed top-level ancestor and a
//     write into a sibling Execution's directory all fail with EROFS, and
//     `git push` to that same origin is refused by the kernel with "remote
//     unpack failed: unable to create temporary object directory". Probe
//     failure is a hard error: no child ever starts unconfined.
//  4. The child's environment is PATH plus GIT_CONFIG_GLOBAL=/dev/null,
//     GIT_CONFIG_SYSTEM=/dev/null and GIT_TERMINAL_PROMPT=0, with the two
//     date variables added on the commit call only. HOME is deliberately NOT
//     allowlisted: it was measured unnecessary for clone, checkout -b, add
//     and commit, and excluding it is what proves by absence that this
//     adapter cannot reach the invoking user's own tool configuration store,
//     even in principle. That is strictly stronger than forge.go, which must
//     allowlist HOME because the forge CLI reads its own store.
//
// No process output ever leaves this file. stdout is captured into a bounded
// buffer with a hard byte cap and stderr is discarded outright, so no error
// returned from here can carry a child's bytes.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// gitExecutableName is the argv[0] basename this adapter will accept.
const gitExecutableName = "git"

// workingCopyDirName is the single path segment the clone lands in, under the
// Execution's own workspace directory.
const workingCopyDirName = "source"

// workingCopyDescriptorName is the plain-text descriptor written beside the
// copy so a sweep can attribute an orphan to an Increment without consulting
// the Control Plane -- which is exactly the state a crashed Runner leaves
// behind. It carries four identifiers and nothing else: no locator, no URL and
// nothing credential-shaped.
const workingCopyDescriptorName = "working-copy.txt"

// maxGitOutputBytes bounds how much of a child's standard output this adapter
// will hold. Every command it runs reports a few bytes to a few kilobytes; the
// cap exists so a pathological output cannot become unbounded memory.
const maxGitOutputBytes = 1 << 20

// gitBaseEnvironmentNames are the variables whose values are read from this
// process. PATH and nothing else: HOME is excluded by construction.
var gitBaseEnvironmentNames = []string{"PATH"}

// gitPinnedEnvironment is the hermetic configuration, pinned per invocation.
// Pinning both config scopes to /dev/null gives a configuration-free git
// without writing to any config file at any scope.
var gitPinnedEnvironment = [][2]string{
	{"GIT_CONFIG_GLOBAL", "/dev/null"},
	{"GIT_CONFIG_SYSTEM", "/dev/null"},
	{"GIT_TERMINAL_PROMPT", "0"},
}

// gitCommitEnvironmentNames are the two names the commit call may add, so the
// commit instant comes from the injected clock rather than a wall-clock read
// inside the adapter.
var gitCommitEnvironmentNames = []string{"GIT_AUTHOR_DATE", "GIT_COMMITTER_DATE"}

// gitEnvironmentAllowlist is the complete set of names allowed to cross into
// a git child. HOME is absent, which is the point.
func gitEnvironmentAllowlist() map[string]bool {
	names := append([]string(nil), gitBaseEnvironmentNames...)
	for _, pair := range gitPinnedEnvironment {
		names = append(names, pair[0])
	}
	names = append(names, gitCommitEnvironmentNames...)
	return allowlistFromNames(names)
}

// GitSourceControl is the production SourceControl. It holds no credential of
// any kind and consults no Secret Broker: a clone from a local origin or an
// anonymous public URL needs none, and having none is what makes the absence
// provable.
type GitSourceControl struct {
	// ExecutablePath, when empty, is resolved with the same resolveTool helper
	// confinement.go and forge.go use: this process's PATH first, then the
	// conventional system locations. The fallback is load-bearing rather than
	// defensive: it is measured that the validated `devbox run --pure`
	// environment has git and unshare on PATH but not gh, so a bare exec by
	// name is not acceptable for a tool this adapter depends on.
	ExecutablePath string
	// Workspace owns the only validated per-Execution path in the codebase
	// (single path segment, no symlink, no escape, 0700). Reusing it means the
	// working copy inherits four already-proven checks instead of a second
	// implementation of them.
	Workspace *Workspace
	// CommitterName and CommitterEmail are passed as -c options in argv. They
	// are an identity for attribution, never an authentication factor.
	CommitterName  string
	CommitterEmail string
	// Now is the injected clock. A nil Now means time.Now().UTC(); tests set
	// it so a commit is reproducible.
	Now func() time.Time
	// TermGrace is handed to ProcessSupervisor.
	TermGrace time.Duration
	// Stat, when non-nil, replaces os.Stat. It exists so the executable
	// refusal paths can be exercised deterministically without touching the
	// filesystem, exactly as ForgeClient.Stat does.
	Stat func(string) (os.FileInfo, error)
}

// DefaultCommitterName and DefaultCommitterEmail are the Loop's own
// attribution. The domain is the reserved .invalid TLD, so the address can
// never resolve to a real mailbox.
const (
	DefaultCommitterName  = "agentic-loop"
	DefaultCommitterEmail = "agentic-loop@loop.invalid"
)

// NewGitSourceControl returns an adapter with the measured defaults.
func NewGitSourceControl(workspace *Workspace) GitSourceControl {
	return GitSourceControl{Workspace: workspace, CommitterName: DefaultCommitterName, CommitterEmail: DefaultCommitterEmail}
}

func (g GitSourceControl) stat(path string) (os.FileInfo, error) {
	if g.Stat != nil {
		return g.Stat(path)
	}
	return os.Stat(path)
}

func (g GitSourceControl) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

func (g GitSourceControl) committerName() string {
	if strings.TrimSpace(g.CommitterName) == "" {
		return DefaultCommitterName
	}
	return g.CommitterName
}

func (g GitSourceControl) committerEmail() string {
	if strings.TrimSpace(g.CommitterEmail) == "" {
		return DefaultCommitterEmail
	}
	return g.CommitterEmail
}

// ResolveExecutable resolves and validates the git path without starting
// anything. Every refusal here happens before a process could exist.
func (g GitSourceControl) ResolveExecutable() (string, error) {
	candidate := strings.TrimSpace(g.ExecutablePath)
	if candidate == "" {
		candidate = resolveTool(gitExecutableName)
	}
	if !filepath.IsAbs(candidate) {
		return "", fmt.Errorf("%w: %q is not an absolute path", ErrGitExecutableMissing, candidate)
	}
	if filepath.Base(candidate) != gitExecutableName {
		return "", fmt.Errorf("%w: basename(%s) != %s", ErrGitExecutableMismatch, candidate, gitExecutableName)
	}
	info, err := g.stat(candidate)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrGitExecutableMissing, candidate)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", ErrGitExecutableMissing, candidate)
	}
	return candidate, nil
}

// buildGitArgv is the single argv constructor. global carries the
// pre-subcommand options (only -C <path> and -c <allowlisted key>=<value>),
// sub is checked against the closed allowlist, and args follow. Because
// allowedGitSubcommand is the only reader of that allowlist and this is its
// only caller, the allowlist itself -- not a caller-side check -- is what
// refuses push, fetch, remote, config and worktree.
func (g GitSourceControl) buildGitArgv(global []string, sub string, args ...string) ([]string, error) {
	resolved, err := g.ResolveExecutable()
	if err != nil {
		return nil, err
	}
	if !allowedGitSubcommand(sub) {
		return nil, fmt.Errorf("%w: %q", ErrGitSubcommandNotAllowed, sub)
	}
	for i := 0; i < len(global); i++ {
		switch global[i] {
		case "-C":
			if i+1 >= len(global) || !filepath.IsAbs(global[i+1]) {
				return nil, fmt.Errorf("%w: -C requires an absolute path", ErrGitOptionNotAllowed)
			}
			i++
		case "-c":
			if i+1 >= len(global) {
				return nil, fmt.Errorf("%w: -c requires a key=value", ErrGitOptionNotAllowed)
			}
			key, _, found := strings.Cut(global[i+1], "=")
			if !found || !gitConfigOptionAllowlist[key] {
				return nil, fmt.Errorf("%w: -c %q", ErrGitOptionNotAllowed, key)
			}
			i++
		default:
			return nil, fmt.Errorf("%w: %q", ErrGitOptionNotAllowed, global[i])
		}
	}
	argv := make([]string, 0, 1+len(global)+1+len(args))
	argv = append(argv, resolved)
	argv = append(argv, global...)
	argv = append(argv, sub)
	return append(argv, args...), nil
}

// ChildEnvironment builds the guarded environment a git child receives:
// exactly the base names' values from this process, the pinned configuration,
// and whatever extra pairs the caller declares -- which GuardEnvironment then
// refuses unless the name is in gitEnvironmentAllowlist. HOME is not in that
// allowlist, so it cannot be added by any caller.
func (g GitSourceControl) ChildEnvironment(extra ...[2]string) ([]string, error) {
	env, err := buildEnvironmentFromBaseNames(gitBaseEnvironmentNames)
	if err != nil {
		return nil, fmt.Errorf("sourcecontrol: %w", err)
	}
	for _, pair := range gitPinnedEnvironment {
		env = append(env, pair[0]+"="+pair[1])
	}
	for _, pair := range extra {
		env = append(env, pair[0]+"="+pair[1])
	}
	if err = GuardEnvironment(env, gitEnvironmentAllowlist()); err != nil {
		return nil, fmt.Errorf("sourcecontrol: %w", err)
	}
	return env, nil
}

// run starts exactly one git child. workspace, when non-empty, is the
// directory NamespaceConfinement keeps writable; an empty workspace means the
// child runs unconfined and is only ever used for `git --version`, which
// writes nothing.
//
// The child's standard output is captured into a bounded buffer and its
// standard error is discarded rather than captured, so there is no path by
// which a child's bytes could reach a caller, a log or a record. The one error
// detail that is preserved is ErrNamespaceUnsupported's own reason, because it
// is the confinement machinery's diagnostic about this kernel -- not the git
// child's output -- and a Probe failure must be reportable (validation.md's
// "stop and escalate with Probe's reason").
func (g GitSourceControl) run(ctx context.Context, workspace string, argv []string) ([]byte, error) {
	return g.runWithEnvironment(ctx, workspace, argv, nil)
}

func (g GitSourceControl) runWithEnvironment(ctx context.Context, workspace string, argv []string, extra [][2]string) ([]byte, error) {
	env, err := g.ChildEnvironment(extra...)
	if err != nil {
		return nil, err
	}
	if err = GuardCommand(argv, env); err != nil {
		return nil, fmt.Errorf("sourcecontrol: %w", err)
	}
	var stdout bytes.Buffer
	supervisor := ProcessSupervisor{
		TermGrace: g.TermGrace,
		Env:       env,
		Stdout:    &limitedWriter{w: &stdout, remaining: maxGitOutputBytes},
	}
	if workspace != "" {
		supervisor.Confine = &NamespaceConfinement{Workspace: workspace}
	}
	if err = supervisor.Run(ctx, argv); err != nil {
		if IsNamespaceUnavailable(err) {
			return nil, err
		}
		// The child's own output is deliberately discarded rather than
		// wrapped: a git error line can carry a path, a ref name or a remote
		// URL, none of which belongs in a record.
		return nil, fmt.Errorf("%w: %s", ErrGitCommandFailed, gitSubcommandOf(argv))
	}
	return stdout.Bytes(), nil
}

// IsNamespaceUnavailable reports whether err is the confinement layer's
// fail-closed refusal. It exists so a caller can escalate a kernel that
// cannot provide the namespace instead of mistaking it for a git failure.
func IsNamespaceUnavailable(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrNamespaceUnsupported.Error())
}

// gitSubcommandOf reports the allowlisted subcommand an argv names, for an
// error message that identifies the stage without quoting any path or output.
func gitSubcommandOf(argv []string) string {
	for _, arg := range argv {
		if allowedGitSubcommand(arg) {
			return arg
		}
	}
	return "unknown"
}

// firstLine returns the first line of a bounded output, trimmed. It is the
// only projection this adapter applies to a single-value read.
func firstLine(raw []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	return strings.TrimSpace(line)
}

// nonEmptyLines splits a bounded output into its non-empty lines.
func nonEmptyLines(raw []byte) []string {
	out := make([]string, 0, 8)
	for _, line := range strings.Split(string(raw), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// validBranchName is deliberately stricter than git's own rules: one or more
// path segments of ordinary characters, no leading dash (which would be read
// as a flag), no whitespace, no traversal.
func validBranchName(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "/") {
		return false
	}
	if strings.ContainsAny(value, " \t\r\n\\:?*[~^") || strings.Contains(value, "..") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// Version reports the git binary's own version line. It is the one call that
// runs unconfined, and only because it writes nothing: it is an
// execution-environment identifier read, not a repository operation.
func (g GitSourceControl) Version(ctx context.Context) (string, error) {
	argv, err := g.buildGitArgv(nil, "version")
	if err != nil {
		return "", err
	}
	raw, err := g.run(ctx, "", argv)
	if err != nil {
		return "", err
	}
	line := firstLine(raw)
	if line == "" {
		return "", ErrGitOutputUnreadable
	}
	return line, nil
}

// workingCopyRoot validates that the copy's root is the "source" child of the
// Execution's own validated workspace directory, using the very same
// Workspace.Path check (single segment, no symlink, no escape) that produced
// it. It is the escape check Discard and the sweep also apply.
func (g GitSourceControl) workingCopyRoot(executionID string) (workspace string, root string, err error) {
	if g.Workspace == nil {
		return "", "", fmt.Errorf("%w: no workspace is configured", ErrWorkingCopyRequestInvalid)
	}
	workspace, err = g.Workspace.Path(executionID)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrWorkingCopyPathEscapes, err)
	}
	root = filepath.Join(workspace, workingCopyDirName)
	if filepath.Dir(root) != workspace || filepath.Base(root) != workingCopyDirName {
		return "", "", fmt.Errorf("%w: %s", ErrWorkingCopyPathEscapes, workingCopyDirName)
	}
	return workspace, root, nil
}

// Materialize produces the independent clone and the change branch.
//
// The copy is always produced by git clone, never by git worktree add: a
// linked worktree's .git is a file pointing into the outer repository's .git,
// which is a write path outside the confined workspace, whereas clone was
// measured to give --git-common-dir == ".git" -- a fully independent
// repository whose every write lands inside the workspace.
func (g GitSourceControl) Materialize(ctx context.Context, req MaterializeRequest) (WorkingCopy, error) {
	if strings.TrimSpace(req.IncrementID) == "" || strings.TrimSpace(req.ExecutionID) == "" || strings.TrimSpace(req.Origin) == "" {
		return WorkingCopy{}, fmt.Errorf("%w: increment id, execution id and origin are all required", ErrWorkingCopyRequestInvalid)
	}
	if strings.HasPrefix(req.Origin, "-") {
		return WorkingCopy{}, fmt.Errorf("%w: an origin may not begin with a dash", ErrWorkingCopyRequestInvalid)
	}
	if req.BaseBranch != "" && !validBranchName(req.BaseBranch) {
		return WorkingCopy{}, fmt.Errorf("%w: base branch", ErrWorkingCopyRequestInvalid)
	}
	if !validBranchName(req.Branch) {
		return WorkingCopy{}, fmt.Errorf("%w: branch", ErrWorkingCopyRequestInvalid)
	}
	if _, _, err := g.workingCopyRoot(req.ExecutionID); err != nil {
		return WorkingCopy{}, err
	}
	workspace, err := g.Workspace.Create(req.ExecutionID)
	if err != nil {
		return WorkingCopy{}, err
	}
	root := filepath.Join(workspace, workingCopyDirName)
	if _, err = os.Lstat(root); err == nil {
		// A second Execution of the same Increment gets a fresh clone at its
		// own path; a copy is never shared or reused.
		return WorkingCopy{}, fmt.Errorf("%w: %s", ErrWorkingCopyExists, workingCopyDirName)
	} else if !os.IsNotExist(err) {
		return WorkingCopy{}, err
	}

	cloneArgs := []string{"--no-hardlinks", "--quiet"}
	if req.Shallow {
		cloneArgs = append(cloneArgs, "--depth", "1", "--no-tags")
	}
	if req.BaseBranch != "" {
		cloneArgs = append(cloneArgs, "--branch", req.BaseBranch)
	}
	cloneArgs = append(cloneArgs, "--", req.Origin, root)
	argv, err := g.buildGitArgv(nil, "clone", cloneArgs...)
	if err != nil {
		return WorkingCopy{}, err
	}
	if _, err = g.run(ctx, workspace, argv); err != nil {
		// Discard on the error path too: a half-made copy is never left
		// behind for a later Execution to inherit.
		_ = g.discardTree(req.ExecutionID)
		return WorkingCopy{}, err
	}

	baseCommit, err := g.revParse(ctx, workspace, root, "HEAD")
	if err != nil {
		_ = g.discardTree(req.ExecutionID)
		return WorkingCopy{}, err
	}
	baseBranch := req.BaseBranch
	if baseBranch == "" {
		if argv, err = g.buildGitArgv([]string{"-C", root}, "symbolic-ref", "--short", "HEAD"); err != nil {
			_ = g.discardTree(req.ExecutionID)
			return WorkingCopy{}, err
		}
		raw, e := g.run(ctx, workspace, argv)
		if e != nil {
			_ = g.discardTree(req.ExecutionID)
			return WorkingCopy{}, e
		}
		baseBranch = firstLine(raw)
	}
	if argv, err = g.buildGitArgv([]string{"-C", root}, "checkout", "-b", req.Branch); err != nil {
		_ = g.discardTree(req.ExecutionID)
		return WorkingCopy{}, err
	}
	if _, err = g.run(ctx, workspace, argv); err != nil {
		_ = g.discardTree(req.ExecutionID)
		return WorkingCopy{}, err
	}

	working := WorkingCopy{
		IncrementID:  req.IncrementID,
		ExecutionID:  req.ExecutionID,
		RepositoryID: req.RepositoryID,
		Workspace:    workspace,
		Root:         root,
		BaseBranch:   baseBranch,
		BaseCommit:   baseCommit,
		Branch:       req.Branch,
		CreatedAt:    g.now(),
	}
	if err = g.writeDescriptor(working); err != nil {
		_ = g.discardTree(req.ExecutionID)
		return WorkingCopy{}, err
	}
	return working, nil
}

// writeDescriptor records the four identifiers a sweep needs to attribute an
// orphan, in plain text, beside the copy. It carries no locator, no URL and
// no credential-shaped field, so internal/runner's AST source guard stays
// green by construction rather than by review.
func (g GitSourceControl) writeDescriptor(working WorkingCopy) error {
	body := strings.Join([]string{
		"increment_id=" + working.IncrementID,
		"execution_id=" + working.ExecutionID,
		"repository_id=" + working.RepositoryID,
		"created_at=" + working.CreatedAt.UTC().Format(time.RFC3339Nano),
		"",
	}, "\n")
	return os.WriteFile(filepath.Join(working.Workspace, workingCopyDescriptorName), []byte(body), 0600)
}

// ReadDescriptor reads the descriptor an Execution's workspace carries, as a
// name/value map. It is how a sweep attributes an orphan without consulting
// the Control Plane. A missing descriptor is reported as absent, not guessed.
func (g GitSourceControl) ReadDescriptor(executionID string) (map[string]string, bool, error) {
	workspace, _, err := g.workingCopyRoot(executionID)
	if err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(filepath.Join(workspace, workingCopyDescriptorName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	out := map[string]string{}
	for _, line := range nonEmptyLines(raw) {
		key, value, found := strings.Cut(line, "=")
		if found {
			out[key] = value
		}
	}
	return out, true, nil
}

// changePath validates one ChangeFile path and returns its absolute form. A
// path that is absolute, empty, or that leaves the working copy root under
// any spelling is refused before anything is written.
func changePath(root, candidate string) (string, error) {
	if candidate == "" || filepath.IsAbs(candidate) {
		return "", fmt.Errorf("%w: %q is not a relative path", ErrChangeSetInvalid, candidate)
	}
	cleaned := filepath.Clean(candidate)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrChangeSetInvalid, candidate)
	}
	if strings.HasPrefix(cleaned, ".git"+string(filepath.Separator)) || cleaned == ".git" {
		return "", fmt.Errorf("%w: a change may not write into the repository's own .git directory", ErrChangeSetInvalid)
	}
	absolute := filepath.Join(root, cleaned)
	rel, err := filepath.Rel(root, absolute)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrWorkingCopyPathEscapes, candidate)
	}
	return absolute, nil
}

// ApplyChange writes the bounded change inside the copy and stages it. The
// files are written by this process directly into its own workspace; the
// staging is a git child and is therefore confined like every other one.
func (g GitSourceControl) ApplyChange(ctx context.Context, working WorkingCopy, change ChangeSet) error {
	if !working.Recorded() {
		return fmt.Errorf("%w: no working copy", ErrWorkingCopyRequestInvalid)
	}
	workspace, root, err := g.workingCopyRoot(working.ExecutionID)
	if err != nil {
		return err
	}
	if root != working.Root || workspace != working.Workspace {
		return fmt.Errorf("%w: the working copy root is not this Execution's own", ErrWorkingCopyPathEscapes)
	}
	if len(change.Files) == 0 {
		return fmt.Errorf("%w: no file", ErrChangeSetInvalid)
	}
	paths := make([]string, 0, len(change.Files))
	for _, file := range change.Files {
		absolute, e := changePath(root, file.Path)
		if e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(absolute), 0700); e != nil {
			return e
		}
		if e = os.WriteFile(absolute, file.Content, 0600); e != nil {
			return e
		}
		paths = append(paths, filepath.Clean(file.Path))
	}
	argv, err := g.buildGitArgv([]string{"-C", root}, "add", append([]string{"--"}, paths...)...)
	if err != nil {
		return err
	}
	_, err = g.run(ctx, workspace, argv)
	return err
}

// revParse reads one object name. It is the only single-value read helper.
func (g GitSourceControl) revParse(ctx context.Context, workspace, root, revision string) (string, error) {
	argv, err := g.buildGitArgv([]string{"-C", root}, "rev-parse", "--verify", revision)
	if err != nil {
		return "", err
	}
	raw, err := g.run(ctx, workspace, argv)
	if err != nil {
		return "", err
	}
	name := firstLine(raw)
	if len(name) != 40 && len(name) != 64 {
		return "", fmt.Errorf("%w: object name", ErrGitOutputUnreadable)
	}
	return name, nil
}

// VerifyIntegrity is the EXECUTED Git-level verification (dp-v2-071 d9). It
// actually runs status --porcelain=v2, diff --exit-code HEAD, rev-parse for
// HEAD and the base, fsck --no-progress --connectivity-only and cat-file on
// the committed tree, and returns the bounded observation. A commit that was
// never verified to be complete and clean would be exactly the
// plausible-looking unmeasured value the policies forbid.
func (g GitSourceControl) VerifyIntegrity(ctx context.Context, working WorkingCopy) (IntegrityObservation, error) {
	if !working.Recorded() {
		return IntegrityObservation{}, fmt.Errorf("%w: no working copy", ErrWorkingCopyRequestInvalid)
	}
	workspace, root, err := g.workingCopyRoot(working.ExecutionID)
	if err != nil {
		return IntegrityObservation{}, err
	}
	if root != working.Root {
		return IntegrityObservation{}, fmt.Errorf("%w: the working copy root is not this Execution's own", ErrWorkingCopyPathEscapes)
	}
	out := IntegrityObservation{BaseCommit: working.BaseCommit, ObservedAt: g.now()}

	// 1. status --porcelain=v2: what the working tree still carries.
	argv, err := g.buildGitArgv([]string{"-C", root}, "status", "--porcelain=v2")
	if err != nil {
		return IntegrityObservation{}, err
	}
	raw, err := g.run(ctx, workspace, argv)
	if err != nil {
		return IntegrityObservation{}, err
	}
	statusLines := nonEmptyLines(raw)

	// 2. diff --exit-code HEAD: a non-zero exit is a dirty tree, which is a
	// measurement rather than a failure, so it is recorded and not returned
	// as an error.
	if argv, err = g.buildGitArgv([]string{"-C", root}, "diff", "--exit-code", "HEAD"); err != nil {
		return IntegrityObservation{}, err
	}
	_, diffErr := g.run(ctx, workspace, argv)
	if diffErr != nil && IsNamespaceUnavailable(diffErr) {
		return IntegrityObservation{}, diffErr
	}
	out.Clean = len(statusLines) == 0 && diffErr == nil

	// 3. rev-parse for HEAD, its tree, and the base.
	if out.HeadCommit, err = g.revParse(ctx, workspace, root, "HEAD"); err != nil {
		return IntegrityObservation{}, err
	}
	if out.TreeName, err = g.revParse(ctx, workspace, root, "HEAD^{tree}"); err != nil {
		return IntegrityObservation{}, err
	}
	if working.BaseCommit != "" {
		verified, e := g.revParse(ctx, workspace, root, working.BaseCommit+"^{commit}")
		if e != nil {
			return IntegrityObservation{}, e
		}
		if verified != working.BaseCommit {
			return IntegrityObservation{}, fmt.Errorf("%w: the base commit does not resolve to itself", ErrIntegrityNotVerified)
		}
	}

	// 4. symbolic-ref: the branch the commit is on.
	if argv, err = g.buildGitArgv([]string{"-C", root}, "symbolic-ref", "--short", "HEAD"); err != nil {
		return IntegrityObservation{}, err
	}
	if raw, err = g.run(ctx, workspace, argv); err != nil {
		return IntegrityObservation{}, err
	}
	out.Branch = firstLine(raw)
	if working.Branch != "" && out.Branch != working.Branch {
		return IntegrityObservation{}, fmt.Errorf("%w: HEAD is not on the change branch", ErrIntegrityNotVerified)
	}

	// 5. fsck --no-progress --connectivity-only: the object graph is whole.
	if argv, err = g.buildGitArgv([]string{"-C", root}, "fsck", "--no-progress", "--connectivity-only"); err != nil {
		return IntegrityObservation{}, err
	}
	if _, err = g.run(ctx, workspace, argv); err != nil {
		return IntegrityObservation{}, err
	}

	// 6. cat-file on the committed tree: the tree this commit names is really
	// a tree and is really present.
	if argv, err = g.buildGitArgv([]string{"-C", root}, "cat-file", "-t", out.TreeName); err != nil {
		return IntegrityObservation{}, err
	}
	if raw, err = g.run(ctx, workspace, argv); err != nil {
		return IntegrityObservation{}, err
	}
	if firstLine(raw) != "tree" {
		return IntegrityObservation{}, fmt.Errorf("%w: the committed tree object is not a tree", ErrIntegrityNotVerified)
	}

	// 7. the changed-path count between the base and HEAD.
	if working.BaseCommit != "" && working.BaseCommit != out.HeadCommit {
		if argv, err = g.buildGitArgv([]string{"-C", root}, "diff", "--name-only", working.BaseCommit, out.HeadCommit); err != nil {
			return IntegrityObservation{}, err
		}
		if raw, err = g.run(ctx, workspace, argv); err != nil {
			return IntegrityObservation{}, err
		}
		out.ChangedPaths = len(nonEmptyLines(raw))
	}
	return out, nil
}

// Commit records the staged change on the branch. Committer identity is
// passed as -c options in argv and the dates come from the injected clock, so
// no git config is written at any scope and no wall clock is read here.
func (g GitSourceControl) Commit(ctx context.Context, working WorkingCopy, change ChangeSet) (CommitObservation, error) {
	if !working.Recorded() {
		return CommitObservation{}, fmt.Errorf("%w: no working copy", ErrWorkingCopyRequestInvalid)
	}
	subject := strings.TrimSpace(change.Subject)
	if subject == "" || strings.ContainsAny(subject, "\r\n") || strings.HasPrefix(subject, "-") {
		return CommitObservation{}, fmt.Errorf("%w: the commit subject must be one non-empty line and may not begin with a dash", ErrChangeSetInvalid)
	}
	workspace, root, err := g.workingCopyRoot(working.ExecutionID)
	if err != nil {
		return CommitObservation{}, err
	}
	if root != working.Root {
		return CommitObservation{}, fmt.Errorf("%w: the working copy root is not this Execution's own", ErrWorkingCopyPathEscapes)
	}
	at := g.now()
	stamp := at.UTC().Format(time.RFC3339)
	argv, err := g.buildGitArgv(
		[]string{"-C", root, "-c", "user.name=" + g.committerName(), "-c", "user.email=" + g.committerEmail()},
		"commit", "--quiet", "-m", subject,
	)
	if err != nil {
		return CommitObservation{}, err
	}
	if _, err = g.runWithEnvironment(ctx, workspace, argv, [][2]string{
		{"GIT_AUTHOR_DATE", stamp},
		{"GIT_COMMITTER_DATE", stamp},
	}); err != nil {
		return CommitObservation{}, err
	}
	commit, err := g.revParse(ctx, workspace, root, "HEAD")
	if err != nil {
		return CommitObservation{}, err
	}
	tree, err := g.revParse(ctx, workspace, root, "HEAD^{tree}")
	if err != nil {
		return CommitObservation{}, err
	}
	return CommitObservation{Branch: working.Branch, Commit: commit, TreeName: tree, Subject: subject, CommittedAt: at}, nil
}

// discardTree removes one Execution's workspace child after re-applying the
// same escape check Workspace.Path applies. It is idempotent.
func (g GitSourceControl) discardTree(executionID string) error {
	workspace, _, err := g.workingCopyRoot(executionID)
	if err != nil {
		return err
	}
	if g.Workspace == nil || filepath.Dir(workspace) != g.Workspace.root {
		return fmt.Errorf("%w: %s is not a direct child of the workspace root", ErrWorkingCopyPathEscapes, executionID)
	}
	return os.RemoveAll(workspace)
}

// Discard removes the working copy and its descriptor. It is idempotent, it
// refuses any root that fails the Workspace.Path escape check, and it is
// called on the error path of Materialize too.
func (g GitSourceControl) Discard(ctx context.Context, working WorkingCopy) error {
	_ = ctx
	if strings.TrimSpace(working.ExecutionID) == "" {
		return fmt.Errorf("%w: no execution id", ErrWorkingCopyRequestInvalid)
	}
	if working.Root != "" {
		_, root, err := g.workingCopyRoot(working.ExecutionID)
		if err != nil {
			return err
		}
		if root != working.Root {
			return fmt.Errorf("%w: the working copy root is not this Execution's own", ErrWorkingCopyPathEscapes)
		}
	}
	return g.discardTree(working.ExecutionID)
}

// SweepWorkingCopies removes every workspace child whose execution id is not
// in active, and returns the ids it removed, sorted by the order the
// directory was read. It is the crash-cleanup path: a Runner calls it at
// start, so a copy a crashed Runner left behind is gone before any new work
// begins.
//
// It uses no goroutine, no sleep and no timer: it is a single pass over one
// directory level, and each candidate is re-validated with the same
// Workspace.Path check, so an entry that is not a plain single-segment
// directory child is refused rather than removed.
func (g GitSourceControl) SweepWorkingCopies(active []string) ([]string, error) {
	if g.Workspace == nil {
		return nil, fmt.Errorf("%w: no workspace is configured", ErrWorkingCopyRequestInvalid)
	}
	keep := allowlistFromNames(active)
	entries, err := os.ReadDir(g.Workspace.root)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] {
			continue
		}
		if !entry.IsDir() {
			// Only an Execution workspace directory is ever swept. Anything
			// else in the root is left exactly as it was found.
			continue
		}
		if _, _, e := g.workingCopyRoot(name); e != nil {
			return removed, e
		}
		if e := g.discardTree(name); e != nil {
			return removed, e
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// RunProjectVerification is DECLARED and fails closed. It starts no process:
// there is no exec construction, no ProcessSupervisor and no argv in this
// method's body, which TestProjectVerificationConstructsNoExecCall proves
// from the AST rather than by reading it.
//
// Running the cloned project's own build or test command is unbounded in cost
// and duration and is the surface the CostLedger, the provider preflight and
// the standing-authorisation records exist to govern; wiring it here would
// create a second execution path past those gates. The project-level stage is
// therefore never reported as passed.
func (g GitSourceControl) RunProjectVerification(ctx context.Context, working WorkingCopy) (ProjectVerification, error) {
	_ = ctx
	_ = working
	return ProjectVerification{
		Stage:  "project-level",
		Wired:  false,
		Reason: "project-level verification is declared on the port and deliberately not wired in this Increment: running the cloned project's own build or test command is unbounded in cost and duration and is governed by the CostLedger, the provider preflight and the standing-authorisation records, so wiring it here would create a second execution path past those gates",
	}, ErrProjectVerificationNotWired
}
