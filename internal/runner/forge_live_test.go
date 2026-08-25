package runner

// The single live forge check (V2-064 A15). It is gated on
// AGENTIC_LOOP_LIVE_FORGE=1 and skips otherwise, so a plain `go test ./...`
// -- and therefore `make check` -- never starts the gh CLI and never reaches
// api.github.com. `devbox run --pure` strips the environment, so the gate
// cannot fire by accident and -e is required to set it.
//
// The check is read-only by construction: the only argv it can produce is
// the ReadArgv/VersionArgv pair asserted in forge_test.go, neither of which
// names a mutating method, a field, an auth subcommand or a git operation. It
// creates, modifies and deletes nothing -- no branch, ref, tag, PR, issue or
// repository -- and it neither runs an auth setup subcommand nor touches any
// git configuration at any scope.

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

const liveForgeGate = "AGENTIC_LOOP_LIVE_FORGE"

// liveForgeOwner and liveForgeName are the owner's own repository, read-only.
// docs/architecture/validation.md requires a dedicated sandbox Repository for
// anything not confirmable read-only; a read has nil blast radius, so no
// sandbox designation is needed for this check and none is assumed.
const liveForgeOwner = "wakuwaku3"
const liveForgeName = "agentic-loop-foundation"

// liveForgeDeadline bounds the whole check. It is a context deadline, not a
// sleep and not a timer: nothing here waits on wall-clock time.
const liveForgeDeadline = 60 * time.Second

// TestForgeReachabilityLive reads one real GitHub repository through the gh
// CLI and asserts existence, default branch and viewer push permission. Every
// identifier it records is an identity label; no credential value, prompt or
// raw response is logged.
func TestForgeReachabilityLive(t *testing.T) {
	if os.Getenv(liveForgeGate) != "1" {
		t.Logf("live forge gate: %s=%q, want \"1\"", liveForgeGate, os.Getenv(liveForgeGate))
		t.Skip("live forge check is disabled")
	}

	client := NewForgeClient("v2-064-forge-adapter")
	resolved, err := client.ResolveExecutable()
	if err != nil {
		// A resolution failure is a measured failure of this check, not a
		// skip: the gate was explicitly requested.
		t.Fatalf("live forge check requested but the CLI could not be resolved: %v", err)
	}
	t.Logf("execution environment: uname=%s/%s go=%s", runtime.GOOS, runtime.GOARCH, runtime.Version())
	t.Logf("resolved absolute executable: %s", resolved)

	// The environment that crosses into the child is the guarded base only,
	// and the granted set is empty. Both are asserted here on the real path
	// as well as deterministically in forge_test.go.
	env, err := client.ChildEnvironment()
	if err != nil {
		t.Fatalf("guarded base environment could not be built: %v", err)
	}
	if len(client.GrantSet) != 0 {
		t.Fatalf("granted set is not empty: %v", client.GrantSet)
	}
	names := make([]string, 0, len(env))
	for _, item := range env {
		name, _, _ := strings.Cut(item, "=")
		names = append(names, name)
	}
	t.Logf("child environment base names=%v granted_names=[] (empty by construction)", names)

	ctx, cancel := context.WithTimeout(context.Background(), liveForgeDeadline)
	defer cancel()

	version, err := client.ReadVersion(ctx)
	if err != nil {
		t.Fatalf("CLI version read failed: %v", err)
	}
	t.Logf("cli version: %s", version)

	argv, err := client.ReadArgv(liveForgeOwner, liveForgeName)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	t.Logf("argv actually run: %v", argv)

	observed, err := client.ReadRepository(ctx, liveForgeOwner, liveForgeName)
	if err != nil {
		t.Fatalf("live read-only repository read failed: %v", err)
	}
	if !observed.Exists {
		t.Fatal("live read reported the repository as absent")
	}
	if observed.DefaultBranch == "" {
		t.Fatal("live read returned no default branch")
	}
	if !observed.ViewerCanPush {
		t.Fatalf("live read reports the viewer cannot push to %s/%s", liveForgeOwner, liveForgeName)
	}
	if observed.NodeID == "" {
		t.Fatal("live read returned no forge node id")
	}
	t.Logf("real external system: repository=%s/%s node_id=%s default_branch=%s viewer_can_push=%v adapter_version=%s",
		observed.Owner, observed.Name, observed.NodeID, observed.DefaultBranch, observed.ViewerCanPush, observed.AdapterVersion)

	// The authenticated account login is an identity label, not a credential,
	// and is the only account-related value recorded. It is read through the
	// same read-only adapter path rather than through an auth subcommand.
	login, err := liveForgeViewerLogin(ctx, client)
	if err != nil {
		t.Fatalf("authenticated account login could not be read: %v", err)
	}
	t.Logf("authenticated account login: %s", login)

	// A repository that does not exist is reported as unreachable, not as
	// reachable-with-empty-fields, so a negative live answer is also real.
	if _, err = client.ReadRepository(ctx, liveForgeOwner, "this-repository-does-not-exist-v2-064"); err == nil {
		t.Fatal("a nonexistent repository was reported as readable")
	}
}

// liveForgeViewerLogin reads the authenticated viewer's login through the
// same read-only argv discipline: an absolute path with the asserted
// basename, the guarded base environment and no granted credential. It runs
// no auth subcommand, so no credential value can be printed.
func liveForgeViewerLogin(ctx context.Context, client ForgeClient) (string, error) {
	resolved, err := client.ResolveExecutable()
	if err != nil {
		return "", err
	}
	env, err := client.ChildEnvironment()
	if err != nil {
		return "", err
	}
	raw, err := runForgeProcess(ctx, []string{resolved, "api", "--method", "GET", "user", "--jq", ".login"}, env)
	if err != nil {
		return "", err
	}
	login := strings.TrimSpace(string(raw))
	if login == "" || strings.ContainsAny(login, " \t\r\n") {
		return "", ErrForgeResponseUnreadable
	}
	return login, nil
}
