package runner

// Deterministic closure of the SourceControl port (V2-071). Nothing here
// starts a process, touches the filesystem or reads a clock: every refusal is
// exercised through the injected Stat hook and the argv/environment builders,
// which run entirely before a child could exist. The real-git,
// real-namespace proof lives in git_test.go, and the single gated live clone
// in git_live_test.go.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// modedFileInfo is a synthetic os.FileInfo whose mode is chosen by the test,
// so the executable refusals can be exercised without touching the filesystem
// at all. forge_test.go's fakeFileInfo is deliberately not reused: it hardcodes
// a regular 0o755 mode, and the refusals below need a directory and a symlink.
type modedFileInfo struct {
	name string
	mode fs.FileMode
}

func (f modedFileInfo) Name() string       { return f.name }
func (f modedFileInfo) Size() int64        { return 0 }
func (f modedFileInfo) Mode() fs.FileMode  { return f.mode }
func (f modedFileInfo) ModTime() time.Time { return time.Unix(1700000000, 0).UTC() }
func (f modedFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f modedFileInfo) Sys() any           { return nil }

func regularGit(path string) (os.FileInfo, error) {
	return modedFileInfo{name: filepath.Base(path), mode: 0o755}, nil
}

// fakeSourceControl is the port's fake. It lives in a _test.go file and
// nowhere else: TestSourceControlHasExactlyOneProductionImplementation proves
// no non-test file in this package contains a second implementation.
type fakeSourceControl struct {
	materialized WorkingCopy
	discarded    int
}

func (f *fakeSourceControl) Version(context.Context) (string, error) { return "git version fake", nil }
func (f *fakeSourceControl) Materialize(context.Context, MaterializeRequest) (WorkingCopy, error) {
	return f.materialized, nil
}
func (f *fakeSourceControl) ApplyChange(context.Context, WorkingCopy, ChangeSet) error { return nil }
func (f *fakeSourceControl) VerifyIntegrity(context.Context, WorkingCopy) (IntegrityObservation, error) {
	return IntegrityObservation{}, nil
}
func (f *fakeSourceControl) Commit(context.Context, WorkingCopy, ChangeSet) (CommitObservation, error) {
	return CommitObservation{}, nil
}
func (f *fakeSourceControl) Discard(context.Context, WorkingCopy) error { f.discarded++; return nil }
func (f *fakeSourceControl) RunProjectVerification(context.Context, WorkingCopy) (ProjectVerification, error) {
	return ProjectVerification{}, ErrProjectVerificationNotWired
}

var _ SourceControl = (*fakeSourceControl)(nil)
var _ SourceControl = GitSourceControl{}

// TestSourceControlResolvesAnAbsoluteGitBeforeAnyProcessExists is acceptance
// A15. Every refusal below happens before a child could be started, and none
// of them touches the filesystem.
func TestSourceControlResolvesAnAbsoluteGitBeforeAnyProcessExists(t *testing.T) {
	if _, err := (GitSourceControl{ExecutablePath: "git", Stat: regularGit}).ResolveExecutable(); !errors.Is(err, ErrGitExecutableMissing) {
		t.Fatalf("a relative executable path = %v, want ErrGitExecutableMissing", err)
	}
	if _, err := (GitSourceControl{ExecutablePath: "../git", Stat: regularGit}).ResolveExecutable(); !errors.Is(err, ErrGitExecutableMissing) {
		t.Fatalf("a traversal executable path = %v, want ErrGitExecutableMissing", err)
	}
	if _, err := (GitSourceControl{ExecutablePath: "/usr/bin/gh", Stat: regularGit}).ResolveExecutable(); !errors.Is(err, ErrGitExecutableMismatch) {
		t.Fatalf("a wrong basename = %v, want ErrGitExecutableMismatch", err)
	}
	if _, err := (GitSourceControl{ExecutablePath: "/usr/bin/git-not-git", Stat: regularGit}).ResolveExecutable(); !errors.Is(err, ErrGitExecutableMismatch) {
		t.Fatalf("a lookalike basename = %v, want ErrGitExecutableMismatch", err)
	}
	missing := GitSourceControl{ExecutablePath: "/usr/bin/git", Stat: func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist }}
	if _, err := missing.ResolveExecutable(); !errors.Is(err, ErrGitExecutableMissing) {
		t.Fatalf("an absent executable = %v, want ErrGitExecutableMissing", err)
	}
	directory := GitSourceControl{ExecutablePath: "/usr/bin/git", Stat: func(path string) (os.FileInfo, error) {
		return modedFileInfo{name: filepath.Base(path), mode: fs.ModeDir | 0o755}, nil
	}}
	if _, err := directory.ResolveExecutable(); !errors.Is(err, ErrGitExecutableMissing) {
		t.Fatalf("a directory = %v, want ErrGitExecutableMissing", err)
	}
	symlink := GitSourceControl{ExecutablePath: "/usr/bin/git", Stat: func(path string) (os.FileInfo, error) {
		return modedFileInfo{name: filepath.Base(path), mode: fs.ModeSymlink | 0o777}, nil
	}}
	if _, err := symlink.ResolveExecutable(); !errors.Is(err, ErrGitExecutableMissing) {
		t.Fatalf("a non-regular file = %v, want ErrGitExecutableMissing", err)
	}
	// The positive control: an absolute path whose basename is git and which
	// stats to a regular file is accepted.
	resolved, err := (GitSourceControl{ExecutablePath: "/nix/store/x/bin/git", Stat: regularGit}).ResolveExecutable()
	if err != nil || resolved != "/nix/store/x/bin/git" {
		t.Fatalf("positive control: resolved=%q err=%v", resolved, err)
	}
}

// TestSourceControlSubcommandAllowlistRefusesEveryRemoteReachingCommand is
// acceptance A16. It asserts both halves: the allowlist data itself does not
// contain the forbidden names, and the argv builder refuses them -- so the
// refusal is the allowlist's, not a caller-side check's.
func TestSourceControlSubcommandAllowlistRefusesEveryRemoteReachingCommand(t *testing.T) {
	forbidden := []string{"push", "fetch", "remote", "submodule", "config", "credential", "worktree"}
	adapter := GitSourceControl{ExecutablePath: "/usr/bin/git", Stat: regularGit}
	for _, name := range forbidden {
		if gitSubcommandAllowlist[name] {
			t.Fatalf("the closed allowlist contains %q", name)
		}
		if allowedGitSubcommand(name) {
			t.Fatalf("allowedGitSubcommand(%q) = true", name)
		}
		if _, err := adapter.buildGitArgv(nil, name); !errors.Is(err, ErrGitSubcommandNotAllowed) {
			t.Fatalf("buildGitArgv(%q) = %v, want ErrGitSubcommandNotAllowed", name, err)
		}
	}
	// The allowlist is exactly the declared set: a fifteenth entry would be a
	// widening, and this assertion is what makes it a deliberate one.
	want := []string{"add", "cat-file", "checkout", "clone", "commit", "diff", "fsck", "init", "ls-files", "rev-parse", "status", "switch", "symbolic-ref", "version"}
	got := make([]string, 0, len(gitSubcommandAllowlist))
	for name := range gitSubcommandAllowlist {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("allowlist = %v, want exactly %v", got, want)
	}
	// The positive control: an allowlisted subcommand builds.
	if _, err := adapter.buildGitArgv(nil, "status", "--porcelain=v2"); err != nil {
		t.Fatalf("positive control: an allowlisted subcommand was refused: %v", err)
	}
	// No non-test file in this package may contain the literal argv of a
	// remote-reaching git command either.
	for _, pf := range parseRunnerPackage(t) {
		if pf.isTest {
			continue
		}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value := strings.Trim(lit.Value, `"`)
			for _, name := range forbidden {
				if value == name {
					t.Fatalf("%s: string literal %q names a forbidden git subcommand", pf.path, value)
				}
			}
			return true
		})
	}
}

// TestSourceControlArgvCarriesTheAbsoluteWorkingCopyRoot is acceptance A18's
// argv half: ProcessSupervisor has no Dir field and supervisor.go is
// untouchable, so the working directory has to be expressed as -C in argv.
func TestSourceControlArgvCarriesTheAbsoluteWorkingCopyRoot(t *testing.T) {
	adapter := GitSourceControl{ExecutablePath: "/usr/bin/git", Stat: regularGit}
	root := "/workspace/execution-1/source"
	argv, err := adapter.buildGitArgv([]string{"-C", root}, "status", "--porcelain=v2")
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "/usr/bin/git" || argv[1] != "-C" || argv[2] != root || argv[3] != "status" {
		t.Fatalf("argv = %v", argv)
	}
	if _, err = adapter.buildGitArgv([]string{"-C", "relative/path"}, "status"); !errors.Is(err, ErrGitOptionNotAllowed) {
		t.Fatalf("-C with a relative path = %v, want ErrGitOptionNotAllowed", err)
	}
	if _, err = adapter.buildGitArgv([]string{"-C"}, "status"); !errors.Is(err, ErrGitOptionNotAllowed) {
		t.Fatalf("-C with no value = %v, want ErrGitOptionNotAllowed", err)
	}
	// Only user.name and user.email may be pinned with -c, and no other
	// pre-subcommand option is admitted at all.
	if _, err = adapter.buildGitArgv([]string{"-c", "user.name=loop"}, "commit", "-m", "x"); err != nil {
		t.Fatalf("-c user.name was refused: %v", err)
	}
	for _, bad := range [][]string{
		{"-c", "credential.helper=store"},
		{"-c", "remote.origin.url=x"},
		{"-c", "protocol.allow=always"},
		{"-c", "noequals"},
		{"--exec-path=/tmp"},
		{"--git-dir", "/tmp"},
	} {
		if _, err = adapter.buildGitArgv(bad, "status"); !errors.Is(err, ErrGitOptionNotAllowed) {
			t.Fatalf("global option %v = %v, want ErrGitOptionNotAllowed", bad, err)
		}
	}
}

// TestSourceControlChildEnvironmentExcludesHome is acceptance A17. HOME is
// not merely unset by accident: it is absent from the allowlist, so no caller
// can add it, and that absence is what proves this adapter cannot reach the
// invoking user's own tool configuration store even in principle.
func TestSourceControlChildEnvironmentExcludesHome(t *testing.T) {
	adapter := GitSourceControl{ExecutablePath: "/usr/bin/git", Stat: regularGit}
	env, err := adapter.ChildEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		names = append(names, key)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "GIT_CONFIG_GLOBAL,GIT_CONFIG_SYSTEM,GIT_TERMINAL_PROMPT,PATH" {
		t.Fatalf("child environment names = %v", names)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "HOME=") {
			t.Fatal("the git child environment carries HOME")
		}
	}
	if gitEnvironmentAllowlist()["HOME"] {
		t.Fatal("HOME is in the git child environment allowlist; the absence proof is gone")
	}
	pinned := map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null", "GIT_TERMINAL_PROMPT": "0"}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if want, ok := pinned[key]; ok && value != want {
			t.Fatalf("%s = %q, want %q", key, value, want)
		}
	}
	// The two date names are allowlisted, so the commit call can add them.
	dated, err := adapter.ChildEnvironment([2]string{"GIT_AUTHOR_DATE", "2026-08-25T00:00:00Z"}, [2]string{"GIT_COMMITTER_DATE", "2026-08-25T00:00:00Z"})
	if err != nil {
		t.Fatalf("the commit-only date variables were refused: %v", err)
	}
	if len(dated) != len(env)+2 {
		t.Fatalf("dated environment = %v", dated)
	}
	// A non-allowlisted name is refused, including HOME itself.
	for _, bad := range []string{"HOME", "GIT_SSH_COMMAND", "GIT_ASKPASS", "SSH_AUTH_SOCK"} {
		if _, err = adapter.ChildEnvironment([2]string{bad, "x"}); err == nil {
			t.Fatalf("a non-allowlisted environment name %q was accepted", bad)
		}
	}
}

// TestSourceControlRefusesAnEscapingChangePath is the ChangeSet half of the
// confinement argument: a path that leaves the working copy is refused in Go,
// before the kernel is ever asked, and the kernel refusal in git_test.go is
// the independent second guarantee.
func TestSourceControlRefusesAnEscapingChangePath(t *testing.T) {
	root := "/workspace/execution-1/source"
	for _, bad := range []string{"", "/etc/passwd", "..", "../outside", "a/../../outside", ".git", ".git/config", "./.git/hooks/pre-commit"} {
		if _, err := changePath(root, bad); err == nil {
			t.Fatalf("change path %q was accepted", bad)
		}
	}
	for _, good := range []string{"README.md", "a/b/c.txt", "./nested/file"} {
		absolute, err := changePath(root, good)
		if err != nil {
			t.Fatalf("change path %q was refused: %v", good, err)
		}
		if !strings.HasPrefix(absolute, root+"/") {
			t.Fatalf("change path %q resolved outside the root: %s", good, absolute)
		}
	}
}

// TestSourceControlRefusesAMalformedMaterializeRequest closes the request
// surface without starting anything.
func TestSourceControlRefusesAMalformedMaterializeRequest(t *testing.T) {
	// NewWorkspace requires 0700; t.TempDir() is created with wider
	// permissions, so the root is a child it creates itself.
	root := filepath.Join(t.TempDir(), "workspace")
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	adapter := GitSourceControl{ExecutablePath: "/usr/bin/git", Stat: regularGit, Workspace: workspace}
	ctx := context.Background()
	cases := []struct {
		name string
		req  MaterializeRequest
	}{
		{"no increment", MaterializeRequest{ExecutionID: "e1", Origin: "/o", Branch: "b"}},
		{"no execution", MaterializeRequest{IncrementID: "i1", Origin: "/o", Branch: "b"}},
		{"no origin", MaterializeRequest{IncrementID: "i1", ExecutionID: "e1", Branch: "b"}},
		{"dash origin", MaterializeRequest{IncrementID: "i1", ExecutionID: "e1", Origin: "--upload-pack=x", Branch: "b"}},
		{"no branch", MaterializeRequest{IncrementID: "i1", ExecutionID: "e1", Origin: "/o"}},
		{"dash branch", MaterializeRequest{IncrementID: "i1", ExecutionID: "e1", Origin: "/o", Branch: "-b"}},
		{"traversal branch", MaterializeRequest{IncrementID: "i1", ExecutionID: "e1", Origin: "/o", Branch: "a/../b"}},
		{"escaping execution id", MaterializeRequest{IncrementID: "i1", ExecutionID: "../escape", Origin: "/o", Branch: "b"}},
		{"nested execution id", MaterializeRequest{IncrementID: "i1", ExecutionID: "a/b", Origin: "/o", Branch: "b"}},
	}
	for _, tc := range cases {
		if _, err := adapter.Materialize(ctx, tc.req); err == nil {
			t.Fatalf("%s: a malformed request was accepted", tc.name)
		}
	}
	if entries, e := os.ReadDir(root); e != nil || len(entries) != 0 {
		t.Fatalf("a refused request created %d workspace entries", len(entries))
	}
}

// TestProjectVerificationFailsClosedAndConstructsNoExecCall is acceptance
// A21's second half. The refusal is asserted, and an AST control proves the
// method's body constructs no exec call -- with a positive control showing the
// same detector firing on a method that does.
func TestProjectVerificationFailsClosedAndConstructsNoExecCall(t *testing.T) {
	adapter := GitSourceControl{ExecutablePath: "/usr/bin/git", Stat: regularGit}
	verification, err := adapter.RunProjectVerification(context.Background(), WorkingCopy{ExecutionID: "e1", Root: "/x/source"})
	if !errors.Is(err, ErrProjectVerificationNotWired) {
		t.Fatalf("RunProjectVerification = %v, want ErrProjectVerificationNotWired", err)
	}
	if verification.Wired {
		t.Fatal("the project-level stage reported itself wired")
	}
	if verification.Stage == "" || len(verification.Reason) < 40 {
		t.Fatalf("the fail-closed verification carries no stated stage and reason: %+v", verification)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "git.go", nil, 0)
	if err != nil {
		t.Fatalf("parse git.go: %v", err)
	}
	// executionNames is the set of identifiers that can only appear in a body
	// that is about to start, or build the argv of, a child process.
	executionNames := map[string]bool{
		"run": true, "runWithEnvironment": true, "buildGitArgv": true,
		"ProcessSupervisor": true, "Command": true, "CommandContext": true,
		"Run": true, "Start": true, "Output": true,
	}
	bodyNamesExecution := func(name string) (bool, bool) {
		found, seen := false, false
		ast.Inspect(file, func(n ast.Node) bool {
			decl, ok := n.(*ast.FuncDecl)
			if !ok || decl.Name == nil || decl.Name.Name != name || decl.Body == nil {
				return true
			}
			seen = true
			ast.Inspect(decl.Body, func(inner ast.Node) bool {
				ident, ok := inner.(*ast.Ident)
				if ok && executionNames[ident.Name] {
					found = true
				}
				if sel, ok := inner.(*ast.SelectorExpr); ok && sel.Sel != nil && executionNames[sel.Sel.Name] {
					found = true
				}
				return true
			})
			return false
		})
		return found, seen
	}
	names, seen := bodyNamesExecution("RunProjectVerification")
	if !seen {
		t.Fatal("RunProjectVerification was not found in git.go; the AST control would pass vacuously")
	}
	if names {
		t.Fatal("RunProjectVerification's body constructs an exec call; the project-level stage must start no process")
	}
	// The positive control: the same detector fires on a method that really
	// does start a child.
	names, seen = bodyNamesExecution("Version")
	if !seen || !names {
		t.Fatalf("positive control failed: the detector did not fire on Version (seen=%v names=%v)", seen, names)
	}
}

// TestSourceControlHasExactlyOneProductionImplementation is acceptance A14's
// structural half: the port has one production implementation and every fake
// lives in a _test.go file.
func TestSourceControlHasExactlyOneProductionImplementation(t *testing.T) {
	methodNames := map[string]bool{
		"Version": true, "Materialize": true, "ApplyChange": true, "VerifyIntegrity": true,
		"Commit": true, "Discard": true, "RunProjectVerification": true,
	}
	// receiverMethods maps a receiver type name to the port methods it
	// declares, per file class (test vs non-test).
	nonTest := map[string]map[string]bool{}
	inTest := map[string]map[string]bool{}
	for _, pf := range parseRunnerPackage(t) {
		bucket := nonTest
		if pf.isTest {
			bucket = inTest
		}
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Name == nil || !methodNames[fn.Name.Name] {
				continue
			}
			receiver := ""
			switch expr := fn.Recv.List[0].Type.(type) {
			case *ast.Ident:
				receiver = expr.Name
			case *ast.StarExpr:
				if ident, ok := expr.X.(*ast.Ident); ok {
					receiver = ident.Name
				}
			}
			if receiver == "" {
				continue
			}
			if bucket[receiver] == nil {
				bucket[receiver] = map[string]bool{}
			}
			bucket[receiver][fn.Name.Name] = true
		}
	}
	complete := func(bucket map[string]map[string]bool) []string {
		out := []string{}
		for receiver, methods := range bucket {
			if len(methods) == len(methodNames) {
				out = append(out, receiver)
			}
		}
		sort.Strings(out)
		return out
	}
	production := complete(nonTest)
	if len(production) != 1 || production[0] != "GitSourceControl" {
		t.Fatalf("non-test implementations of SourceControl = %v, want exactly [GitSourceControl]", production)
	}
	if fakes := complete(inTest); len(fakes) == 0 {
		t.Fatal("no fake implementation was found in a _test.go file; the port's fake must exist and must live only there")
	}
}

// TestSweepRefusesToRemoveAnythingOutsideTheWorkspaceRoot is A24's refusal
// half. It is deterministic and starts no process.
func TestSweepRefusesToRemoveAnythingOutsideTheWorkspaceRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	adapter := GitSourceControl{ExecutablePath: "/usr/bin/git", Stat: regularGit, Workspace: workspace}
	// A symlink in the root pointing outside must not be followed and must
	// not be removed: only a plain directory child is ever swept.
	if err = os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	removed, err := adapter.SweepWorkingCopies(nil)
	if err != nil {
		t.Fatalf("sweep over a root containing a symlink: %v", err)
	}
	for _, id := range removed {
		if id == "escape" {
			t.Fatal("the sweep removed a symlink child; only a plain directory may be swept")
		}
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatalf("the sweep reached outside the workspace root: %v", err)
	}
	// Discard refuses an id that is not one validated path segment.
	for _, bad := range []string{"", "..", "a/b", "/absolute"} {
		if err = adapter.Discard(context.Background(), WorkingCopy{ExecutionID: bad}); err == nil {
			t.Fatalf("Discard accepted execution id %q", bad)
		}
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatalf("a refused Discard reached outside the workspace root: %v", err)
	}
}
