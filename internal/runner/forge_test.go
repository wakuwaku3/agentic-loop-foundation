package runner

// Deterministic closure of the forge adapter (V2-064 A14). Not one test in
// this file starts the gh CLI, reaches a network, sleeps, starts a goroutine
// or reads a wall clock: every refusal path and the argv construction are
// asserted from values alone. The single live reachability check lives in
// forge_live_test.go behind its own gate.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFileInfo lets the refusal paths be exercised without touching the
// filesystem.
type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o755 }
func (f fakeFileInfo) ModTime() time.Time { return time.Unix(1700000000, 0).UTC() }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

func presentFile(path string) (os.FileInfo, error) {
	return fakeFileInfo{name: filepath.Base(path)}, nil
}
func presentDirectory(path string) (os.FileInfo, error) {
	return fakeFileInfo{name: filepath.Base(path), dir: true}, nil
}
func absentFile(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

func testForgeClient(path string, stat func(string) (os.FileInfo, error)) ForgeClient {
	client := NewForgeClient("forge-test")
	client.ExecutablePath = path
	client.Stat = stat
	return client
}

// TestForgeReadArgvIsAReadOnlyRepositoryRead pins the complete argv. The
// method appears explicitly in it, so the argv itself is the evidence that
// the call is a read and cannot create, modify or delete anything.
func TestForgeReadArgvIsAReadOnlyRepositoryRead(t *testing.T) {
	client := testForgeClient("/usr/bin/gh", presentFile)
	argv, err := client.ReadArgv("Wakuwaku3", "agentic-loop-foundation")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/gh", "api", "--method", "GET", "repos/Wakuwaku3/agentic-loop-foundation"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}
	// The argv names no mutating verb, sends no field and carries no
	// credential-shaped value. Mutating verbs and flags are compared per
	// argument, because a repository name legitimately contains substrings
	// like "-f".
	for _, arg := range argv {
		for _, forbidden := range []string{"POST", "PUT", "PATCH", "DELETE", "--field", "-f", "-F", "--input", "--header", "-H"} {
			if arg == forbidden {
				t.Fatalf("argv %v contains the argument %q", argv, forbidden)
			}
		}
	}
	joined := strings.Join(argv, " ")
	for _, forbidden := range []string{"token", "Authorization", "auth", "login"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("argv %v contains %q", argv, forbidden)
		}
	}
	if err = GuardCommand(argv, nil); err != nil {
		t.Fatalf("argv fails the secret guard: %v", err)
	}
	version, err := client.VersionArgv()
	if err != nil || len(version) != 2 || version[1] != "--version" {
		t.Fatalf("version argv = %v err=%v", version, err)
	}
}

// TestForgeRefusesANonEmptyGrantSet is A14(d)'s first refusal: the forge
// adapter is defined by holding no credential, so a grant is a hard error
// before anything is resolved.
func TestForgeRefusesANonEmptyGrantSet(t *testing.T) {
	client := testForgeClient("/usr/bin/gh", presentFile)
	client.GrantSet = []string{"SOME_NAME"}
	if _, err := client.ResolveExecutable(); !errors.Is(err, ErrForgeGrantSetNotEmpty) {
		t.Fatalf("ResolveExecutable with a grant = %v, want ErrForgeGrantSetNotEmpty", err)
	}
	if _, err := client.ReadArgv("o", "n"); !errors.Is(err, ErrForgeGrantSetNotEmpty) {
		t.Fatalf("ReadArgv with a grant = %v, want ErrForgeGrantSetNotEmpty", err)
	}
	if _, err := client.ChildEnvironment(); !errors.Is(err, ErrForgeGrantSetNotEmpty) {
		t.Fatalf("ChildEnvironment with a grant = %v, want ErrForgeGrantSetNotEmpty", err)
	}
	// The default client has an empty grant set, which is the positive form
	// of the same claim.
	if len(NewForgeClient("v").GrantSet) != 0 {
		t.Fatal("the default forge client carries a non-empty grant set")
	}
}

// TestForgeRefusesAMissingExecutable is A14(d)'s second refusal.
func TestForgeRefusesAMissingExecutable(t *testing.T) {
	if _, err := testForgeClient("/nonexistent/bin/gh", absentFile).ResolveExecutable(); !errors.Is(err, ErrForgeExecutableMissing) {
		t.Fatalf("missing executable = %v, want ErrForgeExecutableMissing", err)
	}
	if _, err := testForgeClient("/nonexistent/bin/gh", presentDirectory).ResolveExecutable(); !errors.Is(err, ErrForgeExecutableMissing) {
		t.Fatalf("directory executable = %v, want ErrForgeExecutableMissing", err)
	}
	// A relative path is refused as well: only an absolute path is admissible.
	if _, err := testForgeClient("bin/gh", presentFile).ResolveExecutable(); !errors.Is(err, ErrForgeExecutableMissing) {
		t.Fatalf("relative path = %v, want ErrForgeExecutableMissing", err)
	}
}

// TestForgeRefusesABasenameMismatch is A14(d)'s third refusal and A14(b): the
// resolved path's basename must equal the expected tool, or no process starts.
func TestForgeRefusesABasenameMismatch(t *testing.T) {
	for _, path := range []string{"/usr/bin/not-gh", "/usr/bin/ghost", "/tmp/gh.sh", "/usr/bin/git"} {
		if _, err := testForgeClient(path, presentFile).ResolveExecutable(); !errors.Is(err, ErrForgeExecutableMismatch) {
			t.Fatalf("basename mismatch for %q = %v, want ErrForgeExecutableMismatch", path, err)
		}
	}
	// The refusal happens before the stat, so a substituted binary is
	// rejected whether or not it exists.
	if _, err := testForgeClient("/usr/bin/not-gh", absentFile).ResolveExecutable(); !errors.Is(err, ErrForgeExecutableMismatch) {
		t.Fatalf("mismatch must be reported before existence: %v", err)
	}
	// runForgeProcess re-checks the same two properties, so no caller can
	// reach a process by constructing an argv by hand.
	for _, argv := range [][]string{{}, {"gh", "api"}, {"/usr/bin/not-gh", "api"}} {
		if _, err := runForgeProcess(nil, argv, nil); !errors.Is(err, ErrForgeExecutableMismatch) {
			t.Fatalf("runForgeProcess(%v) = %v, want ErrForgeExecutableMismatch", argv, err)
		}
	}
}

// TestForgeRefusesACoordinateThatIsNotOneSegment stops an owner or a name
// from injecting an extra API path segment or a flag.
func TestForgeRefusesACoordinateThatIsNotOneSegment(t *testing.T) {
	client := testForgeClient("/usr/bin/gh", presentFile)
	for _, tc := range [][2]string{
		{"", "n"}, {"o", ""},
		{"o/x", "n"}, {"o", "n/x"},
		{`o\x`, "n"}, {"o", `n\x`},
		{"..", "n"}, {"o", ".."},
		{".", "n"}, {"o", "."},
		{"-flag", "n"}, {"o", "-flag"},
		{"o x", "n"}, {"o", "n?x"}, {"o", "n#x"}, {"o", "n&x"}, {"o", "n%x"}, {"o", "n@x"}, {"o", "n:x"},
	} {
		if _, err := client.ReadArgv(tc[0], tc[1]); !errors.Is(err, ErrForgeCoordinateInvalid) {
			t.Fatalf("ReadArgv(%q,%q) = %v, want ErrForgeCoordinateInvalid", tc[0], tc[1], err)
		}
	}
}

// TestForgeChildEnvironmentIsTheGuardedBaseOnly is A14(c): the child receives
// the guarded base environment and nothing else. The granted channel is
// empty by construction, so the Secret Broker has nothing to hand over.
func TestForgeChildEnvironmentIsTheGuardedBaseOnly(t *testing.T) {
	t.Setenv("FORGE_TEST_BASE", "harmless")
	client := testForgeClient("/usr/bin/gh", presentFile)
	client.BaseEnvironmentNames = []string{"FORGE_TEST_BASE"}
	env, err := client.ChildEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != "FORGE_TEST_BASE=harmless" {
		t.Fatalf("child environment = %v", env)
	}
	// A secret-shaped name in the declared base set is refused by the guard,
	// so the base channel cannot be used to smuggle a credential in.
	t.Setenv("FORGE_TEST_TOKEN", "value")
	client.BaseEnvironmentNames = []string{"FORGE_TEST_TOKEN"}
	if _, err = client.ChildEnvironment(); err == nil {
		t.Fatal("a secret-shaped base environment name was accepted")
	}
	// A declared name that is not set in this process is a hard failure
	// rather than a silently empty value.
	client.BaseEnvironmentNames = []string{"FORGE_TEST_ABSENT_NAME"}
	if _, err = client.ChildEnvironment(); err == nil {
		t.Fatal("an unset declared base environment name was accepted")
	}
	// The measured default set is exactly the home directory and PATH: the
	// CLI finds its own configuration store from the former without this
	// package naming it.
	if len(DefaultForgeBaseEnvironmentNames) != 2 || DefaultForgeBaseEnvironmentNames[0] != "HOME" || DefaultForgeBaseEnvironmentNames[1] != "PATH" {
		t.Fatalf("default base environment names = %v", DefaultForgeBaseEnvironmentNames)
	}
}

// TestForgeResponseProjectionKeepsNoRawOutput is A14(e): only the parsed
// bounded fields leave the adapter, and no error carries the input.
func TestForgeResponseProjectionKeepsNoRawOutput(t *testing.T) {
	raw := []byte(`{"full_name":"Wakuwaku3/Agentic-Loop-Foundation","node_id":"NODE","default_branch":"main","private":false,"permissions":{"admin":true,"push":true,"pull":true},"description":"SENTINEL-DESCRIPTION","clone_url":"https://github.com/Wakuwaku3/Agentic-Loop-Foundation.git"}`)
	got, err := parseForgeResponse(raw, "Wakuwaku3", "Agentic-Loop-Foundation", "forge-test")
	if err != nil {
		t.Fatal(err)
	}
	want := ForgeObservation{Owner: "Wakuwaku3", Name: "Agentic-Loop-Foundation", Exists: true, DefaultBranch: "main", ViewerCanPush: true, NodeID: "NODE", AdapterVersion: "forge-test"}
	if got != want {
		t.Fatalf("observation = %+v, want %+v", got, want)
	}
	// Fields outside the bound are discarded rather than carried.
	rendered := got.Owner + got.Name + got.DefaultBranch + got.NodeID + got.AdapterVersion
	for _, sentinel := range []string{"SENTINEL-DESCRIPTION", "clone_url", "https://"} {
		if strings.Contains(rendered, sentinel) {
			t.Fatalf("observation carried the unbounded field %q", sentinel)
		}
	}
	// A viewer without push permission is reported as such, not omitted.
	noPush, err := parseForgeResponse([]byte(`{"full_name":"o/n","node_id":"NODE","default_branch":"main","permissions":{"push":false}}`), "o", "n", "v")
	if err != nil || noPush.ViewerCanPush {
		t.Fatalf("no-push projection = %+v err=%v", noPush, err)
	}

	// Every unparseable shape is refused with an error carrying none of the
	// input.
	for _, bad := range [][]byte{
		nil,
		[]byte(``),
		[]byte(`SENTINEL-NOT-JSON`),
		[]byte(`{`),
		[]byte(`{"full_name":"o/n","node_id":"NODE"}`),
		[]byte(`{"full_name":"o/n","default_branch":"main"}`),
		[]byte(`{"full_name":"other/repo","node_id":"NODE","default_branch":"main"}`),
		[]byte(`{"message":"Not Found","documentation_url":"SENTINEL-DOCS"}`),
	} {
		_, err := parseForgeResponse(bad, "o", "n", "v")
		if !errors.Is(err, ErrForgeResponseUnreadable) {
			t.Fatalf("parseForgeResponse(%q) = %v, want ErrForgeResponseUnreadable", bad, err)
		}
		for _, sentinel := range []string{"SENTINEL-NOT-JSON", "SENTINEL-DOCS", "Not Found", "other/repo"} {
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("error message leaked the input: %q", err.Error())
			}
		}
	}
	// The unreachable error names only the coordinate the caller already had.
	unreachable := ErrForgeUnreachable
	if strings.Contains(unreachable.Error(), "stdout") || strings.Contains(unreachable.Error(), "stderr") {
		t.Fatalf("unreachable error mentions process output: %q", unreachable.Error())
	}
}

// TestForgeOutputIsCappedWithoutSurfacingBytes proves the bounded read: past
// the cap the writer keeps accepting bytes (so the child is not broken by a
// short write) but stores none of them, and the parse then fails without the
// oversized input appearing anywhere.
func TestForgeOutputIsCappedWithoutSurfacingBytes(t *testing.T) {
	buf := &limitedWriter{w: new(bytes.Buffer), remaining: 4}
	n, err := buf.Write([]byte("abcdefgh"))
	if err != nil || n != 8 {
		t.Fatalf("write = %d %v", n, err)
	}
	if buf.w.String() != "abcd" {
		t.Fatalf("buffered = %q, want %q", buf.w.String(), "abcd")
	}
	n, err = buf.Write([]byte("ijkl"))
	if err != nil || n != 4 || buf.w.String() != "abcd" {
		t.Fatalf("post-cap write = %d %v buffered=%q", n, err, buf.w.String())
	}
	if maxForgeResponseBytes != 1<<20 {
		t.Fatalf("response cap = %d", maxForgeResponseBytes)
	}
}

// TestForgeResolvesFromTheConventionalLocationsWhenPathLacksIt is A14(a): the
// PATH-first, system-locations-second resolution is what makes this adapter
// work in the validated `devbox run --pure` environment, where git is on
// PATH and gh is not.
func TestForgeResolvesFromTheConventionalLocationsWhenPathLacksIt(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, forgeExecutableName)
	if err := os.WriteFile(planted, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	resolved := resolveTool(forgeExecutableName)
	if resolved != planted {
		t.Fatalf("resolveTool = %q, want the PATH entry %q", resolved, planted)
	}
	client := NewForgeClient("v")
	client.Stat = presentFile
	got, err := client.ResolveExecutable()
	if err != nil || got != planted {
		t.Fatalf("ResolveExecutable = %q err=%v, want %q", got, err, planted)
	}
	// With PATH emptied, resolveTool falls back to the conventional system
	// locations. Whatever it returns must still satisfy the basename
	// assertion or be refused; it must never silently become a bare name.
	t.Setenv("PATH", "")
	fallback := resolveTool(forgeExecutableName)
	if fallback != forgeExecutableName && !filepath.IsAbs(fallback) {
		t.Fatalf("fallback resolution = %q; it must be absolute or the bare name", fallback)
	}
	if fallback == forgeExecutableName {
		client.ExecutablePath = ""
		if _, err = client.ResolveExecutable(); !errors.Is(err, ErrForgeExecutableMissing) {
			t.Fatalf("an unresolvable bare name must be refused, got %v", err)
		}
	}
}
