package runner

// Real git, real kernel, real local bare origin (V2-072 A6). Nothing here is
// faked: the git binary is the real one at an absolute path, the namespace is
// a real rootless user+mount namespace, the origin is a bare repository the
// real git binary builds in t.TempDir() at test time, and the commit whose
// payload is derived is a real commit made through the real adapter.
//
// Nothing here resolves a hostname, opens a socket or starts a forge CLI. No
// git fixture is committed to this repository.
//
// Determinism: no fixed sleep, no wall-clock timer and no goroutine. Every
// step is ordered by a synchronous return, and every commit is stamped with
// fixtureInstant.

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// publishFixtureCopy materialises one working copy from a fresh bare origin
// and returns the adapter and the copy. Each caller gets its own Execution id,
// so no two scenarios share a directory.
func publishFixtureCopy(t *testing.T, executionID string) (GitSourceControl, WorkingCopy) {
	t.Helper()
	requireNamespace(t)
	parent := t.TempDir()
	origin := seedBareOrigin(t, parent)
	workspace := newTestWorkspace(t, parent)
	adapter := newTestAdapter(t, workspace)
	working, err := adapter.Materialize(context.Background(), MaterializeRequest{
		IncrementID: "increment-" + executionID, ExecutionID: executionID, RepositoryID: "repository-1",
		Origin: origin, BaseBranch: "main", Branch: "agentic-loop/increment/" + executionID,
	})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return adapter, working
}

func TestPublicationPayloadIsDerivedFromARealVerifiedCommit(t *testing.T) {
	adapter, working := publishFixtureCopy(t, "derive-1")
	ctx := context.Background()
	resolved, err := adapter.ResolveExecutable()
	if err != nil {
		t.Fatalf("resolving the real git binary: %v", err)
	}
	version, err := adapter.Version(ctx)
	if err != nil {
		t.Fatalf("git --version: %v", err)
	}
	t.Logf("execution fact: resolved absolute git path = %s", resolved)
	t.Logf("execution fact: %s", version)
	t.Logf("execution fact: uname -srm equivalent = %s %s %s / go %s", runtime.GOOS, kernelRelease(), runtime.GOARCH, runtime.Version())
	t.Logf("execution fact: confinement seals the top-level ancestor %s read-only", topLevelAncestor(working.Workspace))

	// A change with three shapes: plain text, a nested path, and bytes that
	// are not valid UTF-8 (which is why content travels base64).
	binary := []byte{0x00, 0x01, 0xff, 0xfe, 0x0a, 0x7f}
	change := ChangeSet{Subject: "publish a reviewable change", Files: []ChangeFile{
		{Path: "README.md", Content: []byte("seed\nchanged\n")},
		{Path: "docs/nested/note.txt", Content: []byte("nested\n")},
		{Path: "assets/blob.bin", Content: binary},
	}}
	if err = adapter.ApplyChange(ctx, working, change); err != nil {
		t.Fatalf("ApplyChange: %v", err)
	}
	// One path is made executable so both representable modes are exercised.
	// The chmod and the restage are fixture setup for a MODE, not a stand-in
	// for a step the adapter performs.
	if err = os.Chmod(filepath.Join(working.Root, "docs/nested/note.txt"), 0700); err != nil {
		t.Fatal(err)
	}
	rawGit(t, "-C", working.Root, "add", "--", "docs/nested/note.txt")

	commit, err := adapter.Commit(ctx, working, change)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	observation, err := adapter.VerifyIntegrity(ctx, working)
	if err != nil {
		t.Fatalf("VerifyIntegrity: %v", err)
	}
	if !observation.Clean || observation.HeadCommit != commit.Commit {
		t.Fatalf("integrity observation = %+v", observation)
	}
	if observation.ChangedPaths != 3 {
		t.Fatalf("changed paths = %d, want 3", observation.ChangedPaths)
	}

	payload, err := adapter.PublicationPayload(ctx, working, working.BaseCommit, observation.HeadCommit)
	if err != nil {
		t.Fatalf("PublicationPayload: %v", err)
	}
	if payload.HeadTree != observation.TreeName {
		t.Fatalf("derived head tree %q != the locally verified tree %q", payload.HeadTree, observation.TreeName)
	}
	if want := rawGit(t, "-C", working.Root, "rev-parse", "--verify", working.BaseCommit+"^{tree}"); payload.BaseTree != want {
		t.Fatalf("derived base tree %q != %q", payload.BaseTree, want)
	}
	if payload.BaseCommit != working.BaseCommit || payload.HeadCommit != observation.HeadCommit {
		t.Fatalf("payload commits = %q/%q", payload.BaseCommit, payload.HeadCommit)
	}
	if len(payload.Files) != 3 {
		t.Fatalf("derived %d files, want 3", len(payload.Files))
	}
	modes := map[string]int{}
	for _, file := range payload.Files {
		// The derived mode and blob object name must equal what the LOCAL
		// repository reports for the same path. This is the equality the
		// forge's returned blob name is later required to match.
		line := rawGit(t, "-C", working.Root, "ls-files", "--stage", "--", file.Path)
		entry, ok := parseStagedEntry(line)
		if !ok {
			t.Fatalf("the local repository reported an unparsable index entry for %s", file.Path)
		}
		if file.Mode != entry.mode {
			t.Fatalf("%s: derived mode %q, local repository reports %q", file.Path, file.Mode, entry.mode)
		}
		if file.Object != entry.object {
			t.Fatalf("%s: derived blob object name disagrees with the local repository", file.Path)
		}
		decoded, e := base64.StdEncoding.DecodeString(file.Content)
		if e != nil {
			t.Fatalf("%s: content is not base64: %v", file.Path, e)
		}
		onDisk, e := os.ReadFile(filepath.Join(working.Root, file.Path))
		if e != nil {
			t.Fatal(e)
		}
		if string(decoded) != string(onDisk) {
			t.Fatalf("%s: decoded content differs from the bytes in the working copy", file.Path)
		}
		modes[file.Mode]++
	}
	if modes[publicationModeFile] != 2 || modes[publicationModeExecutable] != 1 {
		t.Fatalf("derived modes = %v, want two %s and one %s", modes, publicationModeFile, publicationModeExecutable)
	}
	// The binary path really is not valid UTF-8, so the base64 encoding is
	// load-bearing rather than decorative.
	for _, file := range payload.Files {
		if file.Path != "assets/blob.bin" {
			continue
		}
		decoded, _ := base64.StdEncoding.DecodeString(file.Content)
		if string(decoded) != string(binary) {
			t.Fatal("the binary path did not round-trip through base64")
		}
	}
	// An empty change is refused: base equal to head means diff reports
	// nothing at all.
	if _, err = adapter.PublicationPayload(ctx, working, observation.HeadCommit, observation.HeadCommit); !errors.Is(err, ErrPublicationChangeSetEmpty) {
		t.Fatalf("an empty change set = %v, want ErrPublicationChangeSetEmpty", err)
	}
	// A payload derivation for another Execution's copy is refused by the same
	// workspace escape check every other method applies.
	other := working
	other.ExecutionID = "someone-else"
	if _, err = adapter.PublicationPayload(ctx, other, working.BaseCommit, observation.HeadCommit); err == nil {
		t.Fatal("a foreign working copy was accepted")
	}
	if _, err = adapter.PublicationPayload(ctx, working, "not-an-object", observation.HeadCommit); !errors.Is(err, ErrGitOutputUnreadable) {
		t.Fatalf("a malformed base object name = %v", err)
	}
}

func TestPublicationPayloadRefusesUnrepresentableEntries(t *testing.T) {
	t.Run("symlink_mode", func(t *testing.T) {
		adapter, working := publishFixtureCopy(t, "symlink-1")
		ctx := context.Background()
		if err := os.Symlink("README.md", filepath.Join(working.Root, "link")); err != nil {
			t.Fatal(err)
		}
		rawGit(t, "-C", working.Root, "add", "--", "link")
		commit, err := adapter.Commit(ctx, working, ChangeSet{Subject: "add a symlink"})
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if mode := strings.Fields(rawGit(t, "-C", working.Root, "ls-files", "--stage", "--", "link"))[0]; mode != publicationModeSymlink {
			t.Fatalf("the fixture did not produce a symlink entry: mode=%s", mode)
		}
		_, err = adapter.PublicationPayload(ctx, working, working.BaseCommit, commit.Commit)
		if !errors.Is(err, ErrPublicationModeUnrepresentable) {
			t.Fatalf("a symlink entry = %v, want ErrPublicationModeUnrepresentable", err)
		}
		if strings.Contains(err.Error(), working.Root) {
			t.Fatal("the refusal quotes a path outside the bounded vocabulary")
		}
	})
	t.Run("submodule_gitlink_mode", func(t *testing.T) {
		adapter, working := publishFixtureCopy(t, "gitlink-1")
		ctx := context.Background()
		// A gitlink is staged directly with update-index --cacheinfo, which
		// needs no submodule and no configuration write. update-index is a
		// FIXTURE command: it is deliberately absent from the adapter's own
		// fourteen-entry allowlist, which the assertion below re-states.
		if _, err := adapter.buildGitArgv([]string{"-C", working.Root}, "update-index"); !errors.Is(err, ErrGitSubcommandNotAllowed) {
			t.Fatalf("update-index is constructible through the adapter: %v", err)
		}
		rawGit(t, "-C", working.Root, "update-index", "--add", "--cacheinfo", "160000,"+working.BaseCommit+",vendored")
		commit, err := adapter.Commit(ctx, working, ChangeSet{Subject: "add a gitlink"})
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if mode := strings.Fields(rawGit(t, "-C", working.Root, "ls-files", "--stage", "--", "vendored"))[0]; mode != publicationModeGitlink {
			t.Fatalf("the fixture did not produce a gitlink entry: mode=%s", mode)
		}
		if _, err = adapter.PublicationPayload(ctx, working, working.BaseCommit, commit.Commit); !errors.Is(err, ErrPublicationModeUnrepresentable) {
			t.Fatalf("a gitlink entry = %v, want ErrPublicationModeUnrepresentable", err)
		}
	})
	t.Run("deletion", func(t *testing.T) {
		adapter, working := publishFixtureCopy(t, "deletion-1")
		ctx := context.Background()
		if err := os.Remove(filepath.Join(working.Root, "README.md")); err != nil {
			t.Fatal(err)
		}
		rawGit(t, "-C", working.Root, "add", "--all", "--", "README.md")
		commit, err := adapter.Commit(ctx, working, ChangeSet{Subject: "delete the seed file"})
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if line := rawGit(t, "-C", working.Root, "ls-files", "--stage", "--", "README.md"); line != "" {
			t.Fatalf("the fixture did not produce a deletion: index still holds %q", line)
		}
		if _, err = adapter.PublicationPayload(ctx, working, working.BaseCommit, commit.Commit); !errors.Is(err, ErrPublicationDeletionUnrepresentable) {
			t.Fatalf("a deletion = %v, want ErrPublicationDeletionUnrepresentable", err)
		}
	})
}
