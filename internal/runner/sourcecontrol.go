package runner

// Source Control port (V2-071). This file declares WHAT an Increment's Git
// working copy is; internal/runner/git.go is the only implementation and is
// the only place that knows HOW (an absolute git binary, a closed argv
// allowlist, a guarded environment and a rootless user+mount namespace).
//
// The port lives in internal/runner rather than in a package of its own, and
// no Git type enters internal/domain, for three measured reasons (dp-v2-071
// d2):
//
//  1. ci/components.json is a prohibited path for this change and every
//     internal/* package is a declared component root, so a brand-new package
//     would be an undeclared root and V2-045's manifest surface check would
//     turn make check red on the first undeclared import edge.
//  2. internal/runner is already the only package permitted to start a child
//     process (internal/runner/forge.go states the same argument for the
//     forge CLI), and internal/runner/source_guard_test.go exists precisely
//     to stop a second uncontrolled spawn site appearing elsewhere.
//  3. docs/architecture/overview.md L153 requires that a Git working copy
//     not become a domain contract. It is honoured by this being an
//     interface in internal/runner: the domain change in this task is the
//     Requirement-to-Repository link and it names no Git concept at all.
//
// Nothing here reaches a remote. The argv allowlist below has no push, fetch,
// remote, submodule, config, credential or worktree entry, so no code path in
// this package can construct such an argv, and the kernel refusal measured
// inside the namespace is the second, independent guarantee (dp-v2-071 d6).

import (
	"context"
	"errors"
	"time"
)

// WorkingCopy is an Increment's Git working copy: an independent clone that
// lives inside one Execution's confinement.
//
// Its owner is the Execution, not the Increment, and that is what bounds its
// lifetime (dp-v2-071 d8). domain.Execution carries IncrementID, so an
// Execution belongs to exactly one Increment for its whole life; keying the
// directory on the Execution therefore makes "a working copy that outlives
// its Increment" unrepresentable rather than merely discouraged, and makes a
// retry a fresh clone at its own path rather than a reused directory.
type WorkingCopy struct {
	// IncrementID and ExecutionID identify the owner. RepositoryID names the
	// Repository the copy was cloned from.
	IncrementID  string
	ExecutionID  string
	RepositoryID string
	// Workspace is the absolute Execution workspace, <workspace_root>/<execution_id>.
	// It is the directory the confinement keeps writable, and nothing else is.
	Workspace string
	// Root is the absolute working copy root, <workspace_root>/<execution_id>/source.
	Root string
	// BaseBranch and BaseCommit record what the copy was branched off.
	// Branch is the branch the change is committed on.
	BaseBranch string
	BaseCommit string
	Branch     string
	CreatedAt  time.Time
}

// Recorded distinguishes a real working copy from a zero value.
func (c WorkingCopy) Recorded() bool { return c.ExecutionID != "" && c.Root != "" }

// MaterializeRequest asks for a working copy. Origin is the clone source: a
// local path inside make check, an owner-designated URL in the single live
// check. It is never a remote this Loop writes to, and no field here can
// become a push target, because no push argv is constructible.
type MaterializeRequest struct {
	IncrementID  string
	ExecutionID  string
	RepositoryID string
	Origin       string
	// BaseBranch, when empty, means "whatever the origin's HEAD points at".
	BaseBranch string
	// Branch is the branch created for this Increment's change.
	Branch string
	// Shallow bounds the clone to "--depth 1 --no-tags". It exists for the
	// single gated live clone, whose whole point is to reach a real remote
	// with the smallest possible read; inside make check the origin is a
	// local bare repository and no bound is needed.
	Shallow bool
}

// ChangeFile is one file of a bounded change. Path is relative to the working
// copy root and must stay inside it. Content is the whole new content: the
// port takes no patch format, so there is no partial-application state.
type ChangeFile struct {
	Path    string
	Content []byte
}

// ChangeSet is a bounded change plus the message the commit carries. The
// message lives here rather than on a separate commit request so the port
// declares exactly the value objects dp-v2-071 d3 names.
type ChangeSet struct {
	Subject string
	Files   []ChangeFile
}

// IntegrityObservation is the result of the Git-level verification, which is
// EXECUTED (dp-v2-071 d9). It carries parsed, bounded fields only: no process
// output, no path outside the workspace, and nothing credential-shaped.
//
// Clean reports that the working tree carries nothing uncommitted.
// ChangedPaths is the number of paths that differ between BaseCommit and
// HeadCommit, so before the commit it is zero and after it is the size of the
// committed change.
type IntegrityObservation struct {
	Branch       string
	HeadCommit   string
	TreeName     string
	BaseCommit   string
	Clean        bool
	ChangedPaths int
	ObservedAt   time.Time
}

// CommitObservation is what the commit produced, in the same bounded form.
type CommitObservation struct {
	Branch      string
	Commit      string
	TreeName    string
	Subject     string
	CommittedAt time.Time
}

// ProjectVerification is the DECLARED, fail-closed project-level stage
// (dp-v2-071 d9). Running the cloned project's own build or test command is
// unbounded in cost and duration and is the surface the CostLedger, the
// provider preflight and the standing-authorisation records exist to govern,
// so wiring it here would create a second execution path past those gates.
// The seam is declared and visible instead: Wired is false, Reason names what
// is missing, and RunProjectVerification starts no process.
type ProjectVerification struct {
	Stage  string
	Wired  bool
	Reason string
}

var (
	// ErrGitExecutableMissing refuses a call whose executable could not be
	// resolved to an existing regular file at an absolute path.
	ErrGitExecutableMissing = errors.New("sourcecontrol: the git executable was not found as a regular file at an absolute path")
	// ErrGitExecutableMismatch refuses a resolved path whose basename is not
	// the expected tool, mirroring internal/runner/forge.go: it is what stops
	// a configuration value substituting a different binary.
	ErrGitExecutableMismatch = errors.New("sourcecontrol: resolved executable basename does not match the expected tool")
	// ErrGitSubcommandNotAllowed refuses an argv whose subcommand is not in
	// the closed allowlist below. push, fetch, remote, submodule, config,
	// credential and worktree are absent from that allowlist, so this is the
	// refusal that makes "this adapter cannot reach a remote" a property of
	// the code rather than a promise.
	ErrGitSubcommandNotAllowed = errors.New("sourcecontrol: git subcommand is not in the closed allowlist")
	// ErrGitOptionNotAllowed refuses a pre-subcommand option outside the
	// closed set (-C and -c user.name/user.email), so no invocation can
	// smuggle in a configuration override.
	ErrGitOptionNotAllowed = errors.New("sourcecontrol: git pre-subcommand option is not in the closed allowlist")
	// ErrWorkingCopyRequestInvalid refuses a Materialize request that does
	// not name an Increment, an Execution and an origin, or whose branch name
	// is not a branch name.
	ErrWorkingCopyRequestInvalid = errors.New("sourcecontrol: the working copy request is incomplete or malformed")
	// ErrWorkingCopyPathEscapes refuses a working copy root, or a change
	// path, that does not stay inside the Execution's own workspace.
	ErrWorkingCopyPathEscapes = errors.New("sourcecontrol: path escapes the execution workspace")
	// ErrWorkingCopyExists refuses materialising over an existing copy: a
	// second Execution of the same Increment gets a fresh clone at its own
	// path, never a reused directory.
	ErrWorkingCopyExists = errors.New("sourcecontrol: a working copy already exists at that path")
	// ErrChangeSetInvalid refuses an empty change set or a file whose path is
	// not one relative path inside the working copy.
	ErrChangeSetInvalid = errors.New("sourcecontrol: the change set is empty or names a path outside the working copy")
	// ErrGitCommandFailed reports that a git child did not complete. Its
	// message deliberately carries none of the child's output.
	ErrGitCommandFailed = errors.New("sourcecontrol: the git command did not complete")
	// ErrGitOutputUnreadable reports that a child's output could not be
	// parsed into the bounded observation. Its message carries none of the
	// input either.
	ErrGitOutputUnreadable = errors.New("sourcecontrol: the git output could not be parsed into the bounded observation")
	// ErrIntegrityNotVerified reports that the Git-level verification ran and
	// did not hold. It is never returned for a check that was skipped.
	ErrIntegrityNotVerified = errors.New("sourcecontrol: the git-level integrity verification did not hold")
	// ErrProjectVerificationNotWired is the fail-closed refusal of the
	// declared project-level stage. It is returned without starting a
	// process, and the project-level stage is never reported as passed.
	ErrProjectVerificationNotWired = errors.New("sourcecontrol: project-level verification is declared but not wired; no build or test command is executed by this adapter")
)

// SourceControl is the port. Each method is one stage of the outcome, cut
// where a caller would want a fake and where the real thing is unavoidable
// (dp-v2-071 d3). The mechanism -- unshare, mount, the absolute git path, the
// guarded environment -- is not a caller concern and stays private to the
// adapter.
//
// Any fake implementation of this interface must live in a _test.go file. No
// non-test file in this package contains one, which
// TestSourceControlHasExactlyOneProductionImplementation asserts.
type SourceControl interface {
	// Version reports the git binary's own version line. It is an
	// execution-environment identifier, not repository data, and it is the
	// one call that writes nothing and therefore the one call that may run
	// unconfined.
	Version(ctx context.Context) (string, error)
	// Materialize produces an independent clone at
	// <workspace_root>/<execution_id>/source and creates the change branch.
	Materialize(ctx context.Context, req MaterializeRequest) (WorkingCopy, error)
	// ApplyChange writes a bounded ChangeSet inside the copy and stages it.
	ApplyChange(ctx context.Context, working WorkingCopy, change ChangeSet) error
	// VerifyIntegrity runs the Git-level verification and returns a bounded
	// observation. It is executed, never assumed.
	VerifyIntegrity(ctx context.Context, working WorkingCopy) (IntegrityObservation, error)
	// Commit records the staged change on the branch.
	Commit(ctx context.Context, working WorkingCopy, change ChangeSet) (CommitObservation, error)
	// Discard removes the working copy. It is idempotent and refuses any root
	// that fails the workspace escape check.
	Discard(ctx context.Context, working WorkingCopy) error
	// RunProjectVerification is declared and fails closed: it returns
	// ErrProjectVerificationNotWired and starts no process.
	RunProjectVerification(ctx context.Context, working WorkingCopy) (ProjectVerification, error)
}

// gitSubcommandAllowlist is the closed set of git subcommands this package
// can name. It is the single point of refusal: buildGitArgv consults it and
// nothing else does, so a caller cannot bypass it and adding a subcommand is
// an edit to this list rather than a new call site.
//
// push, fetch, remote, submodule, config, credential and worktree are absent
// deliberately. Their absence is the mechanism, not a comment:
//   - push/fetch/remote: no remote-mutating or remote-reaching argv exists.
//   - config: configuration is pinned per invocation with GIT_CONFIG_GLOBAL,
//     GIT_CONFIG_SYSTEM and -c options, never written at any scope.
//   - worktree: a linked worktree's .git is a file pointing into the outer
//     repository's .git, i.e. a write path outside the confined workspace,
//     whereas git clone was measured to give --git-common-dir == ".git", a
//     fully independent repository whose every write lands inside the
//     workspace.
var gitSubcommandAllowlist = map[string]bool{
	"version":      true,
	"init":         true,
	"clone":        true,
	"checkout":     true,
	"switch":       true,
	"add":          true,
	"commit":       true,
	"rev-parse":    true,
	"status":       true,
	"diff":         true,
	"fsck":         true,
	"ls-files":     true,
	"cat-file":     true,
	"symbolic-ref": true,
}

// allowedGitSubcommand is the allowlist's only reader. A refusal here happens
// before any process could exist.
func allowedGitSubcommand(name string) bool { return gitSubcommandAllowlist[name] }

// gitConfigOptionAllowlist is the closed set of -c keys an invocation may
// carry. Committer identity is passed this way rather than written into any
// config file, which is what "do not mutate git config at any scope" requires.
var gitConfigOptionAllowlist = map[string]bool{
	"user.name":  true,
	"user.email": true,
}
