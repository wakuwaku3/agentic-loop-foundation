package runner

// The real-git, real-kernel proof required by docs/architecture/validation.md
// L84 (V2-071 A19/A20/A21/A23/A24/A25). Nothing here is faked: the git binary
// is the real one resolved at an absolute path, the namespace is a real
// rootless user+mount namespace created by the real unshare, and every
// refusal asserted below is the kernel's own.
//
// The origin is always a bare repository created at test time by the real git
// binary inside t.TempDir(). No git fixture, pack file or bare directory is
// committed to this repository: docs/architecture/validation.md L187 records
// that gitleaks scans every ref, so a probe commit that reached this
// repository's refs would keep the gate red for every later commit. Nothing
// here resolves a hostname or opens a socket; the single live clone is in
// git_live_test.go, gated and outside make check.
//
// Determinism: there is no fixed sleep, no wall-clock timer and no goroutine
// in this file. ProcessSupervisor.Run is synchronous, so every step is already
// ordered by its return.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fixtureInstant is the one instant every fixture commit and every adapter
// commit in this file is stamped with, so a commit is reproducible and no
// wall clock is read.
var fixtureInstant = time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)

// requireNamespace fails -- it does not skip -- when this kernel cannot
// provide an unprivileged user+mount namespace. C13 is explicit: a skip is
// never a pass, and a kernel that refuses the namespace is a stop-and-escalate
// with the kernel identifier and Probe's own reason, which is exactly what
// this message carries.
func requireNamespace(t *testing.T) {
	t.Helper()
	if err := (NamespaceConfinement{}).Probe(context.Background()); err != nil {
		t.Fatalf("rootless user+mount namespace confinement is unavailable on this kernel/environment; this is a stop-and-escalate, never a skip: kernel=%s GOOS/GOARCH=%s/%s probe reason: %v",
			kernelRelease(), runtime.GOOS, runtime.GOARCH, err)
	}
}

func kernelRelease() string {
	var uname syscall.Utsname
	if err := syscall.Uname(&uname); err != nil {
		return "unknown"
	}
	return utsnameToString(uname.Release)
}

// fixtureGitEnvironment is the environment the fixture git commands run with:
// the same hermetic pinning the adapter uses, and no HOME, so the fixture
// cannot accidentally depend on the invoking user's own configuration either.
func fixtureGitEnvironment() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_DATE=" + fixtureInstant.Format(time.RFC3339),
		"GIT_COMMITTER_DATE=" + fixtureInstant.Format(time.RFC3339),
	}
}

// rawGit runs the real git binary directly, outside the adapter and outside
// any namespace. It is used for exactly two things: building the fixture
// origin (which must exist before the subject under test can clone it) and
// reading a fact the port deliberately does not expose, such as
// --git-common-dir. It is never used to stand in for a step the adapter is
// supposed to perform.
func rawGit(t *testing.T, args ...string) string {
	t.Helper()
	exe := resolveTool(gitExecutableName)
	cmd := exec.Command(exe, args...)
	cmd.Env = fixtureGitEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// seedBareOrigin builds a bare origin with one commit on main, entirely inside
// dir, using only the real git binary. It never pushes: the bare repository is
// produced by cloning a seed working tree with --bare, so no ref is ever
// written to anything this repository owns.
func seedBareOrigin(t *testing.T, dir string) string {
	t.Helper()
	seed := filepath.Join(dir, "seed")
	origin := filepath.Join(dir, "origin.git")
	if err := os.MkdirAll(seed, 0700); err != nil {
		t.Fatal(err)
	}
	rawGit(t, "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, "-C", seed, "add", "--", "README.md")
	rawGit(t, "-C", seed, "-c", "user.name=fixture", "-c", "user.email=fixture@loop.invalid", "commit", "--quiet", "-m", "seed")
	rawGit(t, "clone", "--quiet", "--bare", seed, origin)
	return origin
}

// confinedProbe runs one shell command with the confinement applied, and
// unconfinedProbe runs the identical command without it. The pair is what
// validation.md L84 demands by name: without the second, the first's EROFS
// result proves nothing about the namespace.
func confinedProbe(workspace, script string) error {
	supervisor := ProcessSupervisor{Confine: &NamespaceConfinement{Workspace: workspace}}
	return supervisor.Run(context.Background(), []string{resolveTool("sh"), "-c", script})
}

func unconfinedProbe(script string) error {
	return ProcessSupervisor{}.Run(context.Background(), []string{resolveTool("sh"), "-c", script})
}

func newTestWorkspace(t *testing.T, parent string) *Workspace {
	t.Helper()
	// NewWorkspace requires 0700 and t.TempDir() is created wider, so the
	// root is a child NewWorkspace creates itself.
	workspace, err := NewWorkspace(filepath.Join(parent, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func newTestAdapter(t *testing.T, workspace *Workspace) GitSourceControl {
	t.Helper()
	adapter := NewGitSourceControl(workspace)
	adapter.Now = func() time.Time { return fixtureInstant }
	return adapter
}

// TestGitWorkingCopyIsRealGitInsideARealNamespace is the gate-bearing test.
// It clones a real bare origin with the real git binary into
// <workspace_root>/<execution_id>/source inside a real rootless user+mount
// namespace, proves the clone is an independent repository rather than a
// linked worktree, creates the branch, applies a ChangeSet, runs the executed
// Git-level verification, commits, and then measures the kernel's refusals --
// with the positive control validation.md L84 requires by name.
func TestGitWorkingCopyIsRealGitInsideARealNamespace(t *testing.T) {
	requireNamespace(t)
	parent := t.TempDir()
	origin := seedBareOrigin(t, parent)
	workspace := newTestWorkspace(t, parent)
	adapter := newTestAdapter(t, workspace)
	ctx := context.Background()

	// Execution-environment identifiers. A claim about a real binary on a
	// real kernel has to name the binary and the kernel.
	resolved, err := adapter.ResolveExecutable()
	if err != nil {
		t.Fatalf("resolving the real git binary: %v", err)
	}
	version, err := adapter.Version(ctx)
	if err != nil {
		t.Fatalf("git --version: %v", err)
	}
	sealed := topLevelAncestor(workspace.root)
	t.Logf("execution fact: resolved absolute git path = %s", resolved)
	t.Logf("execution fact: %s", version)
	t.Logf("execution fact: uname -srm equivalent = %s %s %s", runtime.GOOS, kernelRelease(), runtime.GOARCH)
	t.Logf("execution fact: workspace root = %s", workspace.root)
	t.Logf("execution fact: confinement seals the top-level ancestor %s read-only", sealed)
	t.Logf("execution fact: bare origin = %s (created at test time by the real git binary, never committed to this repository)", origin)
	if !strings.HasPrefix(version, "git version ") {
		t.Fatalf("git version line = %q", version)
	}

	// 1. Materialize: clone plus branch, inside the namespace.
	working, err := adapter.Materialize(ctx, MaterializeRequest{
		IncrementID: "increment-1", ExecutionID: "execution-1", RepositoryID: "repository-1",
		Origin: origin, BaseBranch: "main", Branch: "loop/increment-1",
	})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if working.Root != filepath.Join(workspace.root, "execution-1", workingCopyDirName) {
		t.Fatalf("working copy root = %s", working.Root)
	}
	if working.Workspace != filepath.Join(workspace.root, "execution-1") {
		t.Fatalf("confined workspace = %s", working.Workspace)
	}

	// 2. It is an independent repository, not a linked worktree: git worktree
	// add would give a .git FILE pointing into the outer repository's .git,
	// i.e. a write path outside the confinement.
	if common := rawGit(t, "-C", working.Root, "rev-parse", "--git-common-dir"); common != ".git" {
		t.Fatalf("--git-common-dir = %q, want %q: the copy is not an independent repository", common, ".git")
	}
	if info, e := os.Stat(filepath.Join(working.Root, ".git")); e != nil || !info.IsDir() {
		t.Fatalf(".git is not a directory in the working copy: %v", e)
	}
	if working.BaseBranch != "main" || working.Branch != "loop/increment-1" || len(working.BaseCommit) != 40 {
		t.Fatalf("working copy = %+v", working)
	}

	// 3. The descriptor beside the copy names the four identifiers and
	// nothing else.
	descriptor, found, err := adapter.ReadDescriptor("execution-1")
	if err != nil || !found {
		t.Fatalf("descriptor: found=%v err=%v", found, err)
	}
	if descriptor["increment_id"] != "increment-1" || descriptor["execution_id"] != "execution-1" || descriptor["repository_id"] != "repository-1" || descriptor["created_at"] == "" {
		t.Fatalf("descriptor = %+v", descriptor)
	}

	// 4. Before the change, the verification says the tree is clean and no
	// path differs from the base.
	before, err := adapter.VerifyIntegrity(ctx, working)
	if err != nil {
		t.Fatalf("VerifyIntegrity before the change: %v", err)
	}
	if !before.Clean || before.ChangedPaths != 0 || before.HeadCommit != working.BaseCommit || before.Branch != "loop/increment-1" {
		t.Fatalf("pre-change observation = %+v", before)
	}

	// 5. Apply a bounded ChangeSet and verify again: now the tree is dirty.
	change := ChangeSet{
		Subject: "add the loop's own change",
		Files: []ChangeFile{
			{Path: "LOOP.md", Content: []byte("written by the loop\n")},
			{Path: "nested/dir/file.txt", Content: []byte("nested\n")},
		},
	}
	if err = adapter.ApplyChange(ctx, working, change); err != nil {
		t.Fatalf("ApplyChange: %v", err)
	}
	dirty, err := adapter.VerifyIntegrity(ctx, working)
	if err != nil {
		t.Fatalf("VerifyIntegrity after the change: %v", err)
	}
	if dirty.Clean {
		t.Fatalf("the working tree reports clean with a staged change: %+v", dirty)
	}

	// 6. Commit, then run the executed Git-level verification over the result.
	commit, err := adapter.Commit(ctx, working, change)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(commit.Commit) != 40 || len(commit.TreeName) != 40 || commit.Branch != "loop/increment-1" || commit.Subject != change.Subject {
		t.Fatalf("commit observation = %+v", commit)
	}
	if !commit.CommittedAt.Equal(fixtureInstant) {
		t.Fatalf("the commit instant did not come from the injected clock: %v", commit.CommittedAt)
	}
	after, err := adapter.VerifyIntegrity(ctx, working)
	if err != nil {
		t.Fatalf("VerifyIntegrity after the commit: %v", err)
	}
	if !after.Clean {
		t.Fatalf("post-commit observation is not clean: %+v", after)
	}
	if after.HeadCommit != commit.Commit || after.TreeName != commit.TreeName {
		t.Fatalf("post-commit observation = %+v, commit = %+v", after, commit)
	}
	if after.HeadCommit == working.BaseCommit {
		t.Fatal("the commit did not advance HEAD off the base")
	}
	if after.ChangedPaths != 2 {
		t.Fatalf("changed-path count = %d, want 2", after.ChangedPaths)
	}
	if after.BaseCommit != working.BaseCommit {
		t.Fatalf("the observation lost the base commit: %+v", after)
	}
	t.Logf("measured: branch=%s head=%s tree=%s base=%s clean=%v changed_paths=%d",
		after.Branch, after.HeadCommit, after.TreeName, after.BaseCommit, after.Clean, after.ChangedPaths)

	// 7. The project-level stage is declared and fails closed. It is never
	// reported as passed.
	verification, err := adapter.RunProjectVerification(ctx, working)
	if !errors.Is(err, ErrProjectVerificationNotWired) || verification.Wired {
		t.Fatalf("project-level verification = %+v err=%v", verification, err)
	}

	// 8. The kernel's refusals, measured inside the same confinement the
	// adapter uses, with the positive control beside each one.
	sibling, err := workspace.Create("execution-sibling")
	if err != nil {
		t.Fatal(err)
	}
	probes := []struct {
		name string
		path string
	}{
		{"sealed ancestor", filepath.Join(parent, "outside-probe")},
		{"origin directory", filepath.Join(origin, "outside-probe")},
		{"sibling execution workspace", filepath.Join(sibling, "outside-probe")},
	}
	for _, probe := range probes {
		script := "echo probe > " + shQuote(probe.path)
		if err = confinedProbe(working.Workspace, script); err == nil {
			t.Fatalf("a confined write into the %s SUCCEEDED (%s); the confinement is not sealing", probe.name, probe.path)
		}
		if _, statErr := os.Stat(probe.path); statErr == nil {
			t.Fatalf("the refused write into the %s created the file anyway", probe.name)
		}
		t.Logf("measured: confined write into the %s was refused by the kernel", probe.name)
	}
	// A write inside the Execution's own workspace succeeds, so the refusals
	// above are about the location and not about writing at all.
	insideProbe := filepath.Join(working.Workspace, "inside-probe")
	if err = confinedProbe(working.Workspace, "echo probe > "+shQuote(insideProbe)); err != nil {
		t.Fatalf("a confined write inside the Execution workspace was refused: %v", err)
	}
	if _, err = os.Stat(insideProbe); err != nil {
		t.Fatalf("the confined write inside the workspace produced no file: %v", err)
	}
	t.Log("measured: confined write inside the Execution workspace succeeded")

	// 9. The positive control validation.md L84 demands by name: the
	// IDENTICAL outside writes, without the namespace, succeed. Without this,
	// the EROFS results above would prove nothing about the namespace.
	for _, probe := range probes {
		control := probe.path + "-no-namespace"
		if err = unconfinedProbe("echo probe > " + shQuote(control)); err != nil {
			t.Fatalf("positive control: the identical write into the %s FAILED without the namespace (%v); the refusals above prove nothing about the confinement", probe.name, err)
		}
		if _, err = os.Stat(control); err != nil {
			t.Fatalf("positive control: no file was created at %s: %v", control, err)
		}
		t.Logf("positive control: the identical write into the %s SUCCEEDED with no namespace applied", probe.name)
	}

	// 10. The kernel refuses the remote write independently of the adapter's
	// argv allowlist. This argv is built BY HAND here precisely because the
	// adapter cannot construct it: "push" is absent from
	// gitSubcommandAllowlist, which TestSourceControlSubcommandAllowlist...
	// asserts separately. This step measures the second, independent
	// guarantee.
	pushScript := strings.Join([]string{
		"cd " + shQuote(working.Root),
		strings.Join([]string{shQuote(resolved), "push", shQuote(origin), "loop/increment-1"}, " "),
	}, " && ")
	if err = confinedProbe(working.Workspace, pushScript); err == nil {
		t.Fatal("a hand-built git push to the file-path origin SUCCEEDED inside the confinement; the kernel guarantee is gone")
	}
	t.Logf("measured: a hand-built git push into the confined origin was refused (the adapter cannot even construct this argv)")
	// The origin gained no ref from the refused push.
	if refs := rawGit(t, "-C", origin, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "loop/increment-1") {
		t.Fatalf("the refused push created a ref in the origin: %s", refs)
	}

	// 11. Discard removes the copy, is idempotent, and is what ends the
	// working copy's life with the Execution's.
	if err = adapter.Discard(ctx, working); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err = os.Stat(working.Root); !os.IsNotExist(err) {
		t.Fatalf("Discard left the working copy behind: %v", err)
	}
	if err = adapter.Discard(ctx, working); err != nil {
		t.Fatalf("Discard is not idempotent: %v", err)
	}
}

// TestGitWorkingCopyIsNeverSharedBetweenExecutions is acceptance A23's
// lifetime claim: the copy's owner is the Execution, so a retry -- a second
// Execution of the same Increment -- gets a fresh clone at its own path, and
// nothing keyed by increment id alone is ever created on disk.
func TestGitWorkingCopyIsNeverSharedBetweenExecutions(t *testing.T) {
	requireNamespace(t)
	parent := t.TempDir()
	origin := seedBareOrigin(t, parent)
	workspace := newTestWorkspace(t, parent)
	adapter := newTestAdapter(t, workspace)
	ctx := context.Background()

	request := MaterializeRequest{IncrementID: "increment-1", RepositoryID: "repository-1", Origin: origin, BaseBranch: "main", Branch: "loop/increment-1"}
	request.ExecutionID = "execution-1"
	first, err := adapter.Materialize(ctx, request)
	if err != nil {
		t.Fatalf("first Execution: %v", err)
	}
	request.ExecutionID = "execution-2"
	second, err := adapter.Materialize(ctx, request)
	if err != nil {
		t.Fatalf("second Execution: %v", err)
	}
	if first.Root == second.Root {
		t.Fatalf("two Executions of the same Increment share a working copy: %s", first.Root)
	}
	if filepath.Base(filepath.Dir(first.Root)) != "execution-1" || filepath.Base(filepath.Dir(second.Root)) != "execution-2" {
		t.Fatalf("the copies are not keyed by the Execution: %s / %s", first.Root, second.Root)
	}
	// Nothing keyed by the increment id alone exists in the workspace root.
	entries, err := os.ReadDir(workspace.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "increment-1" {
			t.Fatal("a directory keyed by the increment id alone was created")
		}
	}
	// Materialising a second time over the SAME Execution is refused rather
	// than silently reusing the directory.
	request.ExecutionID = "execution-1"
	if _, err = adapter.Materialize(ctx, request); !errors.Is(err, ErrWorkingCopyExists) {
		t.Fatalf("re-materialising over an existing copy = %v, want ErrWorkingCopyExists", err)
	}
	// The two copies are genuinely independent: a commit in one does not
	// appear in the other.
	change := ChangeSet{Subject: "first execution only", Files: []ChangeFile{{Path: "FIRST.md", Content: []byte("first\n")}}}
	if err = adapter.ApplyChange(ctx, first, change); err != nil {
		t.Fatal(err)
	}
	committed, err := adapter.Commit(ctx, first, change)
	if err != nil {
		t.Fatal(err)
	}
	secondObservation, err := adapter.VerifyIntegrity(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if secondObservation.HeadCommit == committed.Commit {
		t.Fatal("the second Execution's copy sees the first Execution's commit")
	}
}

// TestGitWorkingCopySweepRemovesOnlyTheInactiveExecution is acceptance A24:
// crash cleanup is a deterministic sweep -- no goroutine, no sleep, no timer --
// performed by a freshly constructed provider over the same workspace root,
// which is exactly the state a Runner restarting after a crash is in.
func TestGitWorkingCopySweepRemovesOnlyTheInactiveExecution(t *testing.T) {
	requireNamespace(t)
	parent := t.TempDir()
	origin := seedBareOrigin(t, parent)
	workspace := newTestWorkspace(t, parent)
	adapter := newTestAdapter(t, workspace)
	ctx := context.Background()

	request := MaterializeRequest{IncrementID: "increment-1", RepositoryID: "repository-1", Origin: origin, BaseBranch: "main", Branch: "loop/increment-1"}
	request.ExecutionID = "execution-active"
	active, err := adapter.Materialize(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.ExecutionID = "execution-crashed"
	crashed, err := adapter.Materialize(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	originRefsBefore := rawGit(t, "-C", origin, "rev-parse", "--all")
	originListingBefore := rawGit(t, "-C", origin, "for-each-ref", "--format=%(refname) %(objectname)")

	// A freshly constructed provider over the same workspace root: nothing is
	// carried over in memory from the process that made the copies.
	restarted := newTestAdapter(t, newTestWorkspace(t, parent))
	removed, err := restarted.SweepWorkingCopies([]string{"execution-active"})
	if err != nil {
		t.Fatalf("SweepWorkingCopies: %v", err)
	}
	if len(removed) != 1 || removed[0] != "execution-crashed" {
		t.Fatalf("sweep removed %v, want exactly [execution-crashed]", removed)
	}
	if _, err = os.Stat(crashed.Workspace); !os.IsNotExist(err) {
		t.Fatalf("the inactive Execution's tree survived the sweep: %v", err)
	}
	if info, e := os.Stat(active.Root); e != nil || !info.IsDir() {
		t.Fatalf("the active Execution's working copy was damaged by the sweep: %v", e)
	}
	// The active copy is not merely present but still a working repository.
	if observation, e := restarted.VerifyIntegrity(ctx, active); e != nil || observation.Branch != "loop/increment-1" {
		t.Fatalf("the active copy no longer verifies after the sweep: %+v %v", observation, e)
	}
	// The origin is byte-identical: the sweep touched nothing outside the
	// workspace root.
	if after := rawGit(t, "-C", origin, "rev-parse", "--all"); after != originRefsBefore {
		t.Fatalf("the origin's refs changed across the sweep:\nbefore=%s\nafter=%s", originRefsBefore, after)
	}
	if after := rawGit(t, "-C", origin, "for-each-ref", "--format=%(refname) %(objectname)"); after != originListingBefore {
		t.Fatalf("the origin's ref listing changed across the sweep:\nbefore=%s\nafter=%s", originListingBefore, after)
	}
	// A second sweep with the same active set is a no-op.
	if removed, err = restarted.SweepWorkingCopies([]string{"execution-active"}); err != nil || len(removed) != 0 {
		t.Fatalf("the second sweep removed %v (err=%v); it must be a no-op", removed, err)
	}
}
