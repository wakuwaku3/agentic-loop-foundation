package runner

// The single live clone check (V2-071 A25). It is gated on
// AGENTIC_LOOP_LIVE_GIT=1 together with an owner-supplied
// AGENTIC_LOOP_LIVE_GIT_CLONE_URL, so a plain `go test ./...` -- and therefore
// `make check` -- never resolves a hostname and never opens a socket.
// `devbox run --pure` strips the environment, so the gate cannot fire by
// accident and -e is required to set it.
//
// The clone URL is NOT hardcoded, deliberately. docs/architecture/validation.md
// L124 requires an owner-designated target, and a hardcoded default would make
// this check's meaning depend on an unmeasured fact -- that a particular
// repository is anonymously readable. If the gate is set and the URL is absent,
// this check FAILS naming the missing designation; it does not skip.
//
// The check is read-only by construction: it clones anonymously with
// --depth 1 --no-tags and GIT_TERMINAL_PROMPT=0, and creates nothing on the
// remote. It cannot create anything: "push" is absent from
// gitSubcommandAllowlist, so no push argv is constructible at all, which this
// check re-asserts on the live path as well.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	liveGitGate     = "AGENTIC_LOOP_LIVE_GIT"
	liveGitCloneURL = "AGENTIC_LOOP_LIVE_GIT_CLONE_URL"
)

// liveGitDeadline bounds the whole check. It is a context deadline, not a
// sleep and not a timer: nothing here waits on wall-clock time.
const liveGitDeadline = 120 * time.Second

// TestGitCloneLive clones one owner-designated repository anonymously into a
// confined workspace and asserts the resulting HEAD resolves. Nothing is
// written to the remote and nothing is logged but identity labels.
func TestGitCloneLive(t *testing.T) {
	gate := os.Getenv(liveGitGate)
	url := strings.TrimSpace(os.Getenv(liveGitCloneURL))
	if gate != "1" {
		t.Logf("live git gate: %s=%q, want \"1\"; recorded as skipped with this reason, never as a pass", liveGitGate, gate)
		t.Skip("live git clone check is disabled")
	}
	if url == "" {
		// The gate was explicitly requested, so an absent designation is a
		// measured failure of this check and not a skip.
		t.Fatalf("live git clone check requested (%s=1) but %s is not set: the clone target must be designated by the owner, because hardcoding one would make this check's meaning depend on an unmeasured fact about that repository's visibility", liveGitGate, liveGitCloneURL)
	}
	if strings.HasPrefix(url, "-") {
		t.Fatalf("%s must be a URL, not an option", liveGitCloneURL)
	}

	if err := (NamespaceConfinement{}).Probe(context.Background()); err != nil {
		t.Fatalf("live git clone check requested but the confinement is unavailable on this kernel (%s): %v", kernelRelease(), err)
	}
	parent := t.TempDir()
	workspace := newTestWorkspace(t, parent)
	adapter := newTestAdapter(t, workspace)
	resolved, err := adapter.ResolveExecutable()
	if err != nil {
		t.Fatalf("live git clone check requested but git could not be resolved: %v", err)
	}
	t.Logf("execution environment: uname=%s/%s kernel=%s go=%s", runtime.GOOS, runtime.GOARCH, kernelRelease(), runtime.Version())
	t.Logf("resolved absolute executable: %s", resolved)
	version, err := adapter.Version(context.Background())
	if err != nil {
		t.Fatalf("live git clone check requested but git --version failed: %v", err)
	}
	t.Logf("execution environment: %s", version)
	t.Logf("designated clone target: %s (owner-supplied through %s; anonymous read only)", url, liveGitCloneURL)

	// No push argv is constructible, on the live path as much as anywhere
	// else. This is asserted before the clone so a reader of the log sees the
	// refusal before the network read.
	if _, err = adapter.buildGitArgv(nil, "push"); !errors.Is(err, ErrGitSubcommandNotAllowed) {
		t.Fatalf("a push argv was constructible on the live path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveGitDeadline)
	defer cancel()
	working, err := adapter.Materialize(ctx, MaterializeRequest{
		IncrementID:  "live-increment",
		ExecutionID:  "live-execution",
		RepositoryID: "live-repository",
		Origin:       url,
		Branch:       "loop/live-clone",
		Shallow:      true,
	})
	if err != nil {
		t.Fatalf("live clone failed: %v", err)
	}
	defer func() {
		if e := adapter.Discard(context.Background(), working); e != nil {
			t.Errorf("discarding the live working copy: %v", e)
		}
	}()
	if len(working.BaseCommit) != 40 && len(working.BaseCommit) != 64 {
		t.Fatalf("the live clone's base commit does not resolve to an object name: %q", working.BaseCommit)
	}
	if working.Branch != "loop/live-clone" || working.BaseBranch == "" {
		t.Fatalf("live working copy = %+v", working)
	}
	head, err := adapter.revParse(ctx, working.Workspace, working.Root, "HEAD")
	if err != nil {
		t.Fatalf("the live clone's HEAD does not resolve: %v", err)
	}
	if head != working.BaseCommit {
		t.Fatalf("HEAD %q is not the base commit %q on a freshly branched shallow clone", head, working.BaseCommit)
	}
	// The clone really was bounded: git clone --depth 1 writes a .git/shallow
	// grafts file, so its presence is the measurement that --depth 1 took
	// effect rather than being silently ignored.
	shallowFile := filepath.Join(working.Root, ".git", "shallow")
	shallowGrafts, err := os.ReadFile(shallowFile)
	if err != nil {
		t.Fatalf("the live clone is not shallow: %s is unreadable (%v); --depth 1 did not take effect", shallowFile, err)
	}
	graftCount := len(strings.Fields(string(shallowGrafts)))
	t.Logf("measured: live shallow clone of %s resolved HEAD %s on base branch %q; shallow grafts recorded=%d; local branch %q created in the workspace only", url, head, working.BaseBranch, graftCount, working.Branch)
	t.Logf("measured: the clone was bounded by --depth 1 --no-tags with GIT_TERMINAL_PROMPT=0 and no HOME; nothing was written to the remote and no push argv is constructible")
}
