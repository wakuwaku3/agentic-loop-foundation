package main

// V2-091 A16. cmd/runner had ZERO tests at 848d899 (`go test -list '.*'
// ./cmd/runner` printed nothing), so every assertion here is new and none is a
// rename.
//
// The refusals are asserted by calling runReal directly rather than by spawning
// the binary, so the whole file is deterministic: no process, no goroutine, no
// timer, no sleep, no wall clock and no network. The one thing that DOES need
// the real binary -- that `--version` still prints exactly the version string on
// stdout, which is what `make smoke` greps -- is asserted by building and
// running it, following the cmd/bootstrap/main_test.go idiom.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
)

// The fixture token is COMPUTED, not written down. Splitting a token into a
// prefix and a suffix is not enough on its own: `gitleaks git` flagged the
// suffix half of the earlier form, because a 16-hex quoted value sitting on
// a line whose identifier contains "Token" is exactly what generic-api-key
// looks for. The half was still secret-shaped and the name supplied the
// trigger keyword. So the high-entropy part is now built by repeating a
// short group, and no quoted value in this file has the shape of a
// credential. See docs/operations/v2-task-dag.md section 9.7.
const testFixtureLabel = "runner-session-"
const testFixtureGroup = "fe10"

func testToken() string { return testFixtureLabel + strings.Repeat(testFixtureGroup, 4) }

func writeTokenFile(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "session")
	if err := os.WriteFile(path, []byte(testToken()+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestVersionStillPrintsExactlyTheVersionStringOnStdout is the assertion that
// keeps `make smoke` unaffected: the Makefile greps
// '^[0-9]+\.[0-9]+\.[0-9]+-dev$' from this binary's stdout, so the --version
// branch must stay FIRST and must print that and nothing else.
func TestVersionStillPrintsExactlyTheVersionStringOnStdout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "runner-under-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/runner: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	if strings.TrimRight(string(out), "\n") != version {
		t.Fatalf("--version printed %q on stdout, want exactly %q; `make smoke` greps this line", string(out), version)
	}
	if strings.Count(string(out), "\n") != 1 {
		t.Fatalf("--version printed %d lines, want exactly 1: %q", strings.Count(string(out), "\n"), string(out))
	}
}

// TestTheStubRefusalMessagesArePreserved asserts the pre-existing --fake
// contract is byte-unchanged: the no-arguments refusal and the missing
// --runner-id refusal both still exist, verbatim, in the source.
//
// They are asserted as SOURCE STRINGS rather than by running the binary because
// both branches call os.Exit, which a test in the same process cannot survive,
// and spawning a process to read a message would add a process to a file whose
// determinism is acceptance.
func TestTheStubRefusalMessagesArePreserved(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"runner: no external control-plane wiring is enabled; use --fake explicitly for local mode",
		"runner: --runner-id is required with --fake",
		"WARNING: explicit --fake mode uses temporary memory/local state; no external provider is connected",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("the pre-existing refusal message %q is gone; --fake's contract must be preserved", want)
		}
	}
}

// TestTheRealModeRefusesEveryUnsafeInputWithItsOwnStatus is A16's central table.
// Each refusal has its OWN exit status and its OWN message, and NONE of the
// messages contains the token.
func TestTheRealModeRefusesEveryUnsafeInputWithItsOwnStatus(t *testing.T) {
	token := testToken()
	base := "http://127.0.0.1:65535"

	absRoot := t.TempDir()
	if err := os.Chmod(absRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	wideRoot := t.TempDir()
	if err := os.Chmod(wideRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	tokenDir := t.TempDir()
	goodToken := writeTokenFile(t, tokenDir, 0o600)
	wideTokenDir := t.TempDir()
	wideToken := writeTokenFile(t, wideTokenDir, 0o644)

	for _, tc := range []struct {
		name       string
		root       string
		base       string
		tokenPath  string
		maxClaims  int
		wantStatus int
	}{
		{"an empty data root", "", base, goodToken, 1, exitDataRoot},
		{"a relative data root", "relative/root", base, goodToken, 1, exitDataRoot},
		{"a data root wider than 0700", wideRoot, base, goodToken, 1, exitDataRoot},
		{"an empty base url", absRoot, "", goodToken, 1, exitControlPlane},
		{"an absent session token file", absRoot, base, filepath.Join(tokenDir, "missing"), 1, exitSessionToken},
		{"an empty session token file path", absRoot, base, "", 1, exitSessionToken},
		{"a session token file wider than 0600", absRoot, base, wideToken, 1, exitSessionToken},
		{"a non-positive claim bound", absRoot, base, goodToken, 0, exitControlPlane},
		{"a claim bound above the declared one", absRoot, base, goodToken, runner.MaxDriverClaims + 1, exitControlPlane},
	} {
		err := runReal(tc.root, tc.base, tc.tokenPath, tc.maxClaims)
		if err == nil {
			t.Fatalf("%s: runReal returned no error", tc.name)
		}
		if got := exitStatusFor(err); got != tc.wantStatus {
			t.Fatalf("%s: exit status = %d, want %d (error %v)", tc.name, got, tc.wantStatus, err)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("%s: the refusal message contains the session token: %v", tc.name, err)
		}
	}
	// Every declared status is distinct, so a caller can actually tell them
	// apart. A status reused for two different refusals is the same as having
	// one status.
	statuses := map[int]string{
		exitUsage: "usage", exitDataRoot: "data root", exitSessionToken: "session token",
		exitControlPlane: "control plane", exitRunnerInternal: "internal",
	}
	if len(statuses) != 5 {
		t.Fatalf("the declared exit statuses collide: %v", statuses)
	}
}

// TestTheRealModeRefusesAControlPlaneItCannotReachWithoutLeakingTheToken drives
// the ONE path that actually builds the client and the driver, against a port
// nothing is listening on, and asserts the refusal is classified as a
// control-plane refusal and names no token.
//
// It makes no assertion about elapsed time and starts no timer: the connection
// to a closed local port is refused by the kernel synchronously.
func TestTheRealModeRefusesAControlPlaneItCannotReachWithoutLeakingTheToken(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := writeTokenFile(t, t.TempDir(), 0o600)
	err := runReal(root, "http://127.0.0.1:1", tokenPath, 1)
	if err == nil {
		t.Fatal("runReal against a closed port returned no error")
	}
	if got := exitStatusFor(err); got != exitControlPlane {
		t.Fatalf("exit status = %d, want %d (error %v)", got, exitControlPlane, err)
	}
	if strings.Contains(err.Error(), testToken()) {
		t.Fatalf("the refusal names the session token: %v", err)
	}
	// The data root really was prepared: the workspace and journal directories
	// exist at 0700, so a later successful pass has somewhere to work.
	for _, name := range []string{"workspaces", "journal"} {
		info, statErr := os.Stat(filepath.Join(root, name))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("%s was not created under the data root: %v", name, statErr)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s permissions = %04o, want 0700", name, info.Mode().Perm())
		}
	}
}

// TestNoStoreIsImportedByTheRunnerBinary is C11's tripwire in test form.
// Measured in ci/components.json: store-firestore declares `runner` among its
// dependencies, so runner -> store-firestore would be a CYCLE; and
// internal/store/memory in a shipped binary is the fake this task refuses.
func TestNoStoreIsImportedByTheRunnerBinary(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, forbidden := range []string{"internal/store", "internal/provider", "internal/api", "internal/application"} {
		if strings.Contains(text, `"`+forbidden) {
			t.Fatalf("cmd/runner imports %q", forbidden)
		}
	}
	if !strings.Contains(text, "internal/runner\"") {
		t.Fatal("cmd/runner does not import internal/runner; the real mode cannot exist without it")
	}
}
