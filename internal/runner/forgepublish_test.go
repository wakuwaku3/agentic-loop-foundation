package runner

// Deterministic proofs for the first production EffectSink (V2-072).
//
// Nothing here starts a forge CLI, resolves a hostname or opens a socket. The
// executable is a real file named after the expected tool in t.TempDir(), so
// the absolute-path, basename and stat refusals are exercised against a real
// filesystem while no process is ever started; the transport is injected as a
// FUNCTION, which is the only injection this adapter admits.
//
// Determinism: no fixed sleep, no wall-clock timer and no goroutine. Every
// instant comes from an explicit value.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

const (
	publishTestOwner = "owner"
	publishTestName  = "name"
	publishTestRef   = domain.PublicationRefPrefix + "increment-1/execution-1"
)

// fakeExecutable creates a real regular file named after the expected tool, so
// ResolveExecutable's three checks run against a real filesystem while nothing
// is ever executed.
func fakeExecutable(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, forgeExecutableName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// recordedTransport records every ForgeCall and answers from a handler. It is a
// function, not a fake dispatcher and not a fake store.
type recordedTransport struct {
	calls   []ForgeCall
	handler func(ForgeCall) ([]byte, error)
}

func (r *recordedTransport) transport() ForgeTransport {
	return func(_ context.Context, call ForgeCall) ([]byte, error) {
		r.calls = append(r.calls, call)
		if r.handler == nil {
			return nil, fmt.Errorf("%w: no handler", ErrForgePublishIncomplete)
		}
		return r.handler(call)
	}
}

func (r *recordedTransport) count(operation ForgeAPIOperation) int {
	n := 0
	for _, call := range r.calls {
		if call.Operation == operation {
			n++
		}
	}
	return n
}

func (r *recordedTransport) writes() int {
	n := 0
	for _, call := range r.calls {
		if spec, err := forgeOperationSpec(call.Operation); err == nil && spec.writes {
			n++
		}
	}
	return n
}

func testPublisher(t *testing.T) ForgePublisher {
	t.Helper()
	publisher := NewForgePublisher("v2-072-test")
	publisher.ExecutablePath = fakeExecutable(t)
	publisher.Now = func() time.Time { return fixtureInstant }
	return publisher
}

// --- A2: no push argv is constructible, re-asserted --------------------------

func TestGitSubcommandAllowlistStillHasExactlyFourteenEntries(t *testing.T) {
	want := []string{
		"add", "cat-file", "checkout", "clone", "commit", "diff", "fsck", "init",
		"ls-files", "rev-parse", "status", "switch", "symbolic-ref", "version",
	}
	if len(gitSubcommandAllowlist) != 14 {
		got := make([]string, 0, len(gitSubcommandAllowlist))
		for name := range gitSubcommandAllowlist {
			got = append(got, name)
		}
		sort.Strings(got)
		t.Fatalf("the git subcommand allowlist has %d entries: %v", len(got), got)
	}
	for _, name := range want {
		if !allowedGitSubcommand(name) {
			t.Fatalf("the allowlist no longer names %q", name)
		}
	}
	got := make([]string, 0, len(gitSubcommandAllowlist))
	for name := range gitSubcommandAllowlist {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the allowlist is %v, want exactly %v", got, want)
	}
	// The two-key -c option allowlist is unchanged as well.
	if len(gitConfigOptionAllowlist) != 2 || !gitConfigOptionAllowlist["user.name"] || !gitConfigOptionAllowlist["user.email"] {
		t.Fatalf("the -c option allowlist is %v", gitConfigOptionAllowlist)
	}
}

func TestNoRemoteReachingGitArgvIsConstructible(t *testing.T) {
	// The executable is the real one, resolved to an absolute path, so the
	// refusal measured below is the ALLOWLIST's and not a missing binary. Note
	// the measured order inside buildGitArgv, which this task does not change:
	// the executable is resolved first and the subcommand allowlist second, so
	// both must hold for an argv to exist and either alone refuses it.
	adapter := GitSourceControl{ExecutablePath: resolveTool(gitExecutableName)}
	if _, err := adapter.ResolveExecutable(); err != nil {
		t.Fatalf("the real git binary could not be resolved: %v", err)
	}
	for _, subcommand := range []string{"push", "fetch", "remote", "config", "worktree", "credential", "submodule"} {
		if _, err := adapter.buildGitArgv(nil, subcommand); !errors.Is(err, ErrGitSubcommandNotAllowed) {
			t.Fatalf("an argv naming %q was constructible: %v", subcommand, err)
		}
	}
	// Every refusal happens before a process could exist: buildGitArgv only
	// builds a slice, and ProcessSupervisor is never reached.
	if _, err := adapter.buildGitArgv([]string{"-c", "credential.helper=anything"}, "clone"); !errors.Is(err, ErrGitOptionNotAllowed) {
		t.Fatalf("a credential.helper -c option was constructible: %v", err)
	}
	if _, err := adapter.buildGitArgv([]string{"--exec-path=/tmp"}, "clone"); !errors.Is(err, ErrGitOptionNotAllowed) {
		t.Fatalf("an arbitrary pre-subcommand option was constructible: %v", err)
	}
	// And the same refusal holds when the executable cannot be resolved at
	// all, so no combination reaches a process.
	unresolvable := GitSourceControl{ExecutablePath: "git"}
	for _, subcommand := range []string{"push", "clone"} {
		if _, err := unresolvable.buildGitArgv(nil, subcommand); err == nil {
			t.Fatalf("an argv naming %q was constructible with an unresolvable executable", subcommand)
		}
	}
}

// --- A3, A4: the executable, the environment, the empty grant set -----------

func TestForgePublisherRefusesEveryUnsafeExecutableBeforeAnyProcess(t *testing.T) {
	base := testPublisher(t)
	cases := map[string]struct {
		mutate func(*ForgePublisher)
		want   error
	}{
		"non-absolute path": {func(p *ForgePublisher) { p.ExecutablePath = "gh" }, ErrForgeExecutableMissing},
		"wrong basename": {func(p *ForgePublisher) {
			p.ExecutablePath = filepath.Join(filepath.Dir(base.ExecutablePath), "not-the-tool")
		}, ErrForgeExecutableMismatch},
		"missing file": {func(p *ForgePublisher) {
			p.ExecutablePath = filepath.Join(t.TempDir(), forgeExecutableName)
		}, ErrForgeExecutableMissing},
		"a directory": {func(p *ForgePublisher) {
			dir := filepath.Join(t.TempDir(), forgeExecutableName)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			p.ExecutablePath = dir
		}, ErrForgeExecutableMissing},
		"non-empty granted set": {func(p *ForgePublisher) { p.GrantSet = []string{"anything"} }, ErrForgeGrantSetNotEmpty},
	}
	for name, testCase := range cases {
		publisher := base
		testCase.mutate(&publisher)
		if _, err := publisher.ResolveExecutable(); !errors.Is(err, testCase.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, testCase.want)
		}
		// Every operation goes through the same refusal, so no argv exists.
		if _, err := publisher.Argv(ForgeCreateRef, publishTestOwner, publishTestName, ""); err == nil {
			t.Fatalf("%s: an argv was constructible", name)
		}
	}
	// The stat hook makes every refusal deterministic and filesystem-free.
	deterministic := base
	deterministic.Stat = func(string) (os.FileInfo, error) { return nil, errors.New("injected") }
	if _, err := deterministic.ResolveExecutable(); !errors.Is(err, ErrForgeExecutableMissing) {
		t.Fatalf("the injected stat hook was not consulted: %v", err)
	}
	if resolved, err := base.ResolveExecutable(); err != nil || resolved != base.ExecutablePath {
		t.Fatalf("the well-formed publisher was refused: %q %v", resolved, err)
	}
}

func TestForgePublisherChildEnvironmentIsExactlyHomeAndPath(t *testing.T) {
	publisher := testPublisher(t)
	env, err := publisher.ChildEnvironment()
	if err != nil {
		t.Fatalf("ChildEnvironment: %v", err)
	}
	names := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "HOME,PATH" {
		t.Fatalf("the child environment carries %v, want exactly HOME and PATH", names)
	}
	// No environment entry and no argv element may carry a credential-shaped
	// name. The granted set is empty by construction, so there is nothing for
	// the Secret Broker to hand over.
	if len(publisher.GrantSet) != 0 {
		t.Fatalf("the granted set is %v, want empty", publisher.GrantSet)
	}
	granted := publisher
	granted.GrantSet = []string{"FORGE_TOKEN"}
	if _, err = granted.ChildEnvironment(); !errors.Is(err, ErrForgeGrantSetNotEmpty) {
		t.Fatalf("a non-empty granted set built an environment: %v", err)
	}
	for _, entry := range env {
		lowered := strings.ToLower(entry)
		for _, forbidden := range []string{"token", "secret", "credential", "password", "authorization", "bearer"} {
			if strings.Contains(lowered, forbidden) {
				t.Fatalf("the child environment entry %q looks credential-shaped", entry)
			}
		}
	}
	// The git child, for its part, still receives no HOME at all.
	gitEnv, err := GitSourceControl{ExecutablePath: publisher.ExecutablePath}.ChildEnvironment()
	if err != nil {
		t.Fatalf("git ChildEnvironment: %v", err)
	}
	for _, entry := range gitEnv {
		if strings.HasPrefix(entry, "HOME=") {
			t.Fatal("the git child received HOME")
		}
	}
	if gitEnvironmentAllowlist()["HOME"] {
		t.Fatal("HOME entered the git environment allowlist")
	}
}

// --- A5, A8: the closed operation list, every argv, every body --------------

func TestForgeOperationListIsClosedAndNamesNoUpdateOrDelete(t *testing.T) {
	operations := ForgePublishOperations()
	if len(operations) != 7 || len(forgePublishOperations) != 7 {
		t.Fatalf("the closed list has %d/%d entries", len(operations), len(forgePublishOperations))
	}
	writes, reads := 0, 0
	for _, operation := range operations {
		spec, err := forgeOperationSpec(operation)
		if err != nil {
			t.Fatalf("%q is in the list but has no spec: %v", operation, err)
		}
		if spec.writes {
			writes++
			if spec.method != "POST" || !spec.body {
				t.Fatalf("%q writes but its spec is %+v", operation, spec)
			}
		} else {
			reads++
			if spec.method != "GET" || spec.body {
				t.Fatalf("%q reads but its spec is %+v", operation, spec)
			}
		}
		for _, forbidden := range []string{"PATCH", "PUT", "DELETE"} {
			if spec.method == forbidden {
				t.Fatalf("%q uses method %s", operation, forbidden)
			}
		}
	}
	if writes != 4 || reads != 3 {
		t.Fatalf("the list has %d writes and %d reads, want 4 and 3", writes, reads)
	}
	// Positive controls: an operation that would update, force or delete a ref
	// is not in the list, so it cannot be named at all.
	publisher := testPublisher(t)
	for _, absent := range []ForgeAPIOperation{"update-ref", "delete-ref", "force-ref", "patch-ref", "create-pull-request", "create-tag", "create-release", ""} {
		if _, err := forgeOperationSpec(absent); !errors.Is(err, ErrForgeOperationNotAllowed) {
			t.Fatalf("%q resolved to a spec", absent)
		}
		if _, err := publisher.Argv(absent, publishTestOwner, publishTestName, ""); !errors.Is(err, ErrForgeOperationNotAllowed) {
			t.Fatalf("an argv for %q was constructible: %v", absent, err)
		}
	}
}

func TestForgePublishArgvIsAssertedElementByElement(t *testing.T) {
	publisher := testPublisher(t)
	exe := publisher.ExecutablePath
	cases := []struct {
		operation ForgeAPIOperation
		argument  string
		want      []string
	}{
		{ForgeCreateBlob, "", []string{exe, "api", "--method", "POST", "repos/owner/name/git/blobs", "--input", "-"}},
		{ForgeCreateTree, "", []string{exe, "api", "--method", "POST", "repos/owner/name/git/trees", "--input", "-"}},
		{ForgeCreateCommit, "", []string{exe, "api", "--method", "POST", "repos/owner/name/git/commits", "--input", "-"}},
		{ForgeCreateRef, "", []string{exe, "api", "--method", "POST", "repos/owner/name/git/refs", "--input", "-"}},
		{ForgeReadRef, "heads/agentic-loop/increment-1/execution-1", []string{exe, "api", "--method", "GET", "repos/owner/name/git/matching-refs/heads/agentic-loop/increment-1/execution-1"}},
		{ForgeReadCommit, "abc", []string{exe, "api", "--method", "GET", "repos/owner/name/git/commits/abc"}},
		{ForgeCountWorkflowRuns, "agentic-loop/increment-1/execution-1", []string{exe, "api", "--method", "GET", "repos/owner/name/actions/runs?per_page=1&branch=agentic-loop/increment-1/execution-1"}},
	}
	for _, testCase := range cases {
		argv, err := publisher.Argv(testCase.operation, publishTestOwner, publishTestName, testCase.argument)
		if err != nil {
			t.Fatalf("%s: %v", testCase.operation, err)
		}
		if len(argv) != len(testCase.want) {
			t.Fatalf("%s: argv = %v, want %v", testCase.operation, argv, testCase.want)
		}
		for i := range argv {
			if argv[i] != testCase.want[i] {
				t.Fatalf("%s: argv[%d] = %q, want %q", testCase.operation, i, argv[i], testCase.want[i])
			}
		}
		// The method is stated explicitly, so the argv itself witnesses what
		// the call does, and no argv element carries a request body.
		if argv[2] != "--method" {
			t.Fatalf("%s: the method is not stated explicitly: %v", testCase.operation, argv)
		}
		for _, element := range argv {
			if strings.Contains(element, "{") || strings.Contains(element, "\"") {
				t.Fatalf("%s: argv element %q looks like a request body", testCase.operation, element)
			}
		}
	}
	// The version argv reads nothing from the forge.
	version, err := publisher.VersionArgv()
	if err != nil {
		t.Fatal(err)
	}
	if len(version) != 2 || version[0] != exe || version[1] != "--version" {
		t.Fatalf("version argv = %v", version)
	}
	// A coordinate that is not one path segment can never inject an API path.
	for _, pair := range [][2]string{{"own/er", "name"}, {"owner", "na/me"}, {"", "name"}, {"owner", ""}, {"-owner", "name"}, {"owner", "name?x=1"}, {"..", "name"}} {
		if _, err = publisher.Argv(ForgeCreateBlob, pair[0], pair[1], ""); !errors.Is(err, ErrForgeCoordinateInvalid) {
			t.Fatalf("owner=%q name=%q was accepted: %v", pair[0], pair[1], err)
		}
	}
	// An operation that takes a path argument refuses one that is missing or
	// option-shaped, and one that takes none refuses a stray argument.
	if _, err = publisher.Argv(ForgeReadCommit, publishTestOwner, publishTestName, ""); err == nil {
		t.Fatal("a read-commit argv with no object name was constructible")
	}
	if _, err = publisher.Argv(ForgeReadCommit, publishTestOwner, publishTestName, "--flag"); err == nil {
		t.Fatal("a read-commit argv with an option-shaped argument was constructible")
	}
	if _, err = publisher.Argv(ForgeCreateBlob, publishTestOwner, publishTestName, "stray"); err == nil {
		t.Fatal("a create-blob argv with a stray path argument was constructible")
	}
}

// samplePayload is the derived payload the body assertions are built from. It
// is a value, not a measurement: the measurement against a real commit is
// TestPublicationPayloadIsDerivedFromARealVerifiedCommit.
func samplePayload() PublicationPayload {
	return PublicationPayload{
		BaseCommit: strings.Repeat("a", 40),
		BaseTree:   strings.Repeat("b", 40),
		HeadCommit: strings.Repeat("c", 40),
		HeadTree:   strings.Repeat("d", 40),
		Files: []PublicationFile{
			{Path: "README.md", Mode: publicationModeFile, Object: strings.Repeat("e", 40), Content: "c2VlZAo="},
			{Path: "scripts/run.sh", Mode: publicationModeExecutable, Object: strings.Repeat("f", 40), Content: "IyEvYmluL3NoCg=="},
		},
	}
}

// bodyKeys collects every JSON object key in a document, at any depth.
func bodyKeys(t *testing.T, body []byte) []string {
	t.Helper()
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	keys := []string{}
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				keys = append(keys, key)
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	sort.Strings(keys)
	return keys
}

func TestForgePublishBodiesAreAssertedFieldByFieldAndNameNoForce(t *testing.T) {
	payload := samplePayload()
	blob, err := BlobBody(payload.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	var blobParsed forgeBlobRequest
	if err = json.Unmarshal(blob, &blobParsed); err != nil {
		t.Fatal(err)
	}
	if blobParsed.Content != payload.Files[0].Content || blobParsed.Encoding != "base64" {
		t.Fatalf("blob body = %+v", blobParsed)
	}
	if strings.Join(bodyKeys(t, blob), ",") != "content,encoding" {
		t.Fatalf("blob body keys = %v", bodyKeys(t, blob))
	}

	tree, err := TreeBody(payload)
	if err != nil {
		t.Fatal(err)
	}
	var treeParsed forgeTreeRequest
	if err = json.Unmarshal(tree, &treeParsed); err != nil {
		t.Fatal(err)
	}
	if treeParsed.BaseTree != payload.BaseTree {
		t.Fatalf("tree body base_tree = %q, want the base commit's tree %q", treeParsed.BaseTree, payload.BaseTree)
	}
	if len(treeParsed.Tree) != 2 {
		t.Fatalf("tree body carries %d entries", len(treeParsed.Tree))
	}
	for i, entry := range treeParsed.Tree {
		if entry.Path != payload.Files[i].Path || entry.Mode != payload.Files[i].Mode || entry.SHA != payload.Files[i].Object || entry.Type != "blob" {
			t.Fatalf("tree entry %d = %+v", i, entry)
		}
	}
	if strings.Join(bodyKeys(t, tree), ",") != "base_tree,mode,mode,path,path,sha,sha,tree,type,type" {
		t.Fatalf("tree body keys = %v", bodyKeys(t, tree))
	}

	message, err := PublicationCommitMessage(publishTestRef)
	if err != nil {
		t.Fatal(err)
	}
	if message != "publish the verified change of increment increment-1, execution execution-1" {
		t.Fatalf("commit message = %q", message)
	}
	commit, err := CommitBody(message, payload.HeadTree, payload.BaseCommit)
	if err != nil {
		t.Fatal(err)
	}
	var commitParsed forgeCommitRequest
	if err = json.Unmarshal(commit, &commitParsed); err != nil {
		t.Fatal(err)
	}
	if commitParsed.Message != message || commitParsed.Tree != payload.HeadTree {
		t.Fatalf("commit body = %+v", commitParsed)
	}
	if len(commitParsed.Parents) != 1 || commitParsed.Parents[0] != payload.BaseCommit {
		t.Fatalf("commit parents = %v, want exactly the one base commit", commitParsed.Parents)
	}
	if strings.Join(bodyKeys(t, commit), ",") != "message,parents,tree" {
		t.Fatalf("commit body keys = %v", bodyKeys(t, commit))
	}

	ref, err := RefBody(publishTestRef, payload.HeadCommit)
	if err != nil {
		t.Fatal(err)
	}
	var refParsed forgeRefRequest
	if err = json.Unmarshal(ref, &refParsed); err != nil {
		t.Fatal(err)
	}
	if refParsed.Ref != publishTestRef || refParsed.SHA != payload.HeadCommit {
		t.Fatalf("ref body = %+v", refParsed)
	}
	if strings.Join(bodyKeys(t, ref), ",") != "ref,sha" {
		t.Fatalf("ref body keys = %v", bodyKeys(t, ref))
	}

	// A8, the absolute rule: the string "force" appears as a key in NO write
	// body. There is no map-shaped body anywhere in this adapter, so the set of
	// keys a body can carry is the set of struct fields declared for it.
	for name, body := range map[string][]byte{"blob": blob, "tree": tree, "commit": commit, "ref": ref} {
		for _, key := range bodyKeys(t, body) {
			if strings.Contains(strings.ToLower(key), "force") {
				t.Fatalf("the %s body carries a key %q", name, key)
			}
		}
	}
}

func TestRefBodyRefusesAnyRefOutsideTheReservedPrefix(t *testing.T) {
	for _, ref := range []string{"refs/heads/main", "refs/heads/v2", "refs/tags/v1", "refs/heads/agentic-loop", "agentic-loop/a/b", ""} {
		if _, err := RefBody(ref, strings.Repeat("a", 40)); !errors.Is(err, ErrForgeRefNotReserved) {
			t.Fatalf("RefBody(%q) = %v, want ErrForgeRefNotReserved", ref, err)
		}
		if _, err := PublicationCommitMessage(ref); !errors.Is(err, ErrForgeRefNotReserved) {
			t.Fatalf("PublicationCommitMessage(%q) = %v", ref, err)
		}
	}
	if _, err := TreeBody(PublicationPayload{BaseTree: strings.Repeat("b", 40), Files: []PublicationFile{{Path: "link", Mode: publicationModeSymlink, Object: strings.Repeat("e", 40), Content: "eA=="}}}); !errors.Is(err, ErrPublicationModeUnrepresentable) {
		t.Fatalf("a symlink entry reached a tree body: %v", err)
	}
	if _, err := TreeBody(PublicationPayload{BaseTree: strings.Repeat("b", 40)}); !errors.Is(err, ErrForgePayloadDisagrees) {
		t.Fatalf("an empty tree body was built: %v", err)
	}
}

// --- the deterministic sink and observer ------------------------------------

// stubSource is the injected PublicationSource for the pure sink tests. It is
// a _test.go implementation, as the port requires, and the real derivation is
// measured against a real commit elsewhere in this package.
type stubSource struct {
	payload PublicationPayload
	err     error
	calls   int
}

func (s *stubSource) PublicationPayload(context.Context, WorkingCopy, string, string) (PublicationPayload, error) {
	s.calls++
	return s.payload, s.err
}

// stubTarget and stubRecords stand in for the application layer in the pure
// sink tests. The protocol test below uses the REAL Service for both.
type stubTarget struct {
	target application.PublicationTarget
	found  bool
	err    error
}

func (s stubTarget) PublicationTargetForOutbox(context.Context, string) (application.PublicationTarget, bool, error) {
	return s.target, s.found, s.err
}

type stubRecords struct {
	saved []domain.PublicationObservation
}

func (s *stubRecords) RecordPublication(_ context.Context, value domain.PublicationObservation) error {
	if err := domain.ValidatePublicationObservation(value); err != nil {
		return err
	}
	s.saved = append(s.saved, value)
	return nil
}

func (s *stubRecords) Publication(_ context.Context, operationID string) (domain.PublicationObservation, bool, error) {
	for _, value := range s.saved {
		if value.OperationID.String() == operationID {
			return value, true, nil
		}
	}
	return domain.PublicationObservation{}, false, nil
}

func sampleIntent() domain.PublicationIntent {
	payload := samplePayload()
	locator, err := domain.NormalizeSourceLocator(domain.SourceLocator{Owner: publishTestOwner, Name: publishTestName, DefaultBranch: "v2"})
	if err != nil {
		panic(err)
	}
	return domain.PublicationIntent{
		RepositoryID: "repository-1",
		Locator:      locator,
		Ref:          publishTestRef,
		BaseBranch:   "v2",
		BaseCommit:   payload.BaseCommit,
		HeadCommit:   payload.HeadCommit,
		HeadTree:     payload.HeadTree,
		ChangedPaths: len(payload.Files),
	}
}

func sampleDelivery(t *testing.T, intent domain.PublicationIntent) application.EffectDelivery {
	t.Helper()
	body, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	return application.EffectDelivery{OutboxID: "outbox-1", OperationID: "operation-1", IdempotencyKey: "operation-1", RequestID: "request-1", Kind: application.PublicationOutboxKind, Target: "execution-1", Payload: body}
}

func sampleTarget(intent domain.PublicationIntent) stubTarget {
	return stubTarget{found: true, target: application.PublicationTarget{
		RepositoryID: intent.RepositoryID.String(), Owner: intent.Locator.Owner, Name: intent.Locator.Name,
		Ref: intent.Ref, BaseCommit: intent.BaseCommit,
	}}
}

const publishedCommitName = "1234567890abcdef1234567890abcdef12345678"

// happyResponses is the transport answer set for a publication that lands: the
// ref is absent, every object name agrees, and the read-after-write holds.
func happyResponses(payload PublicationPayload, intent domain.PublicationIntent) map[ForgeAPIOperation][]string {
	refPresentJSON := fmt.Sprintf(`[{"ref":%q,"object":{"sha":%q,"type":"commit"}}]`, intent.Ref, publishedCommitName)
	commitJSON := fmt.Sprintf(`{"sha":%q,"tree":{"sha":%q},"parents":[{"sha":%q}]}`, publishedCommitName, intent.HeadTree, intent.BaseCommit)
	return map[ForgeAPIOperation][]string{
		ForgeReadRef:      {`[]`, refPresentJSON},
		ForgeCreateTree:   {fmt.Sprintf(`{"sha":%q}`, payload.HeadTree)},
		ForgeCreateCommit: {fmt.Sprintf(`{"sha":%q}`, publishedCommitName)},
		ForgeCreateRef:    {refPresentJSON},
		ForgeReadCommit:   {commitJSON},
	}
}

// scriptedTransport answers each operation from a per-operation queue, falling
// back to the queue's last entry once it is exhausted. Blob creations echo the
// local object name of the file whose content the body carries.
func scriptedTransport(t *testing.T, payload PublicationPayload, answers map[ForgeAPIOperation][]string, override func(ForgeCall) ([]byte, error, bool)) *recordedTransport {
	t.Helper()
	seen := map[ForgeAPIOperation]int{}
	recorder := &recordedTransport{}
	recorder.handler = func(call ForgeCall) ([]byte, error) {
		if override != nil {
			if raw, err, handled := override(call); handled {
				return raw, err
			}
		}
		if call.Operation == ForgeCreateBlob {
			var parsed forgeBlobRequest
			if err := json.Unmarshal(call.Body, &parsed); err != nil {
				return nil, err
			}
			for _, file := range payload.Files {
				if file.Content == parsed.Content {
					return []byte(fmt.Sprintf(`{"sha":%q}`, file.Object)), nil
				}
			}
			return nil, fmt.Errorf("%w: the body named no known blob", ErrForgePublishIncomplete)
		}
		queue := answers[call.Operation]
		if len(queue) == 0 {
			return nil, fmt.Errorf("%w: no scripted answer for %s", ErrForgePublishIncomplete, call.Operation)
		}
		index := seen[call.Operation]
		if index >= len(queue) {
			index = len(queue) - 1
		}
		seen[call.Operation]++
		return []byte(queue[index]), nil
	}
	return recorder
}

func sinkUnderTest(t *testing.T, payload PublicationPayload, transport *recordedTransport, target stubTarget, records *stubRecords) ForgePublisher {
	t.Helper()
	publisher := testPublisher(t)
	publisher.Source = &stubSource{payload: payload}
	publisher.Working = WorkingCopy{IncrementID: "increment-1", ExecutionID: "execution-1", Workspace: "/nonexistent/workspace", Root: "/nonexistent/workspace/source"}
	publisher.Target = target
	publisher.Records = records
	publisher.Transport = transport.transport()
	return publisher
}

func TestForgePublishConfirmsOnlyWhenAllFourEqualitiesHold(t *testing.T) {
	payload := samplePayload()
	intent := sampleIntent()
	delivery := sampleDelivery(t, intent)

	t.Run("all_four_hold", func(t *testing.T) {
		transport := scriptedTransport(t, payload, happyResponses(payload, intent), nil)
		records := &stubRecords{}
		publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), records)
		if err := publisher.Deliver(context.Background(), delivery); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if transport.count(ForgeCreateBlob) != len(payload.Files) {
			t.Fatalf("%d blob creations for %d files", transport.count(ForgeCreateBlob), len(payload.Files))
		}
		if transport.count(ForgeCreateTree) != 1 || transport.count(ForgeCreateCommit) != 1 || transport.count(ForgeCreateRef) != 1 {
			t.Fatalf("write calls = %d tree, %d commit, %d ref", transport.count(ForgeCreateTree), transport.count(ForgeCreateCommit), transport.count(ForgeCreateRef))
		}
		if len(records.saved) != 1 {
			t.Fatalf("%d Observations recorded", len(records.saved))
		}
		observation := records.saved[0]
		if observation.State != domain.PublicationPublishedAndObserved || !observation.TreesAgree {
			t.Fatalf("Observation = %+v", observation)
		}
		if observation.PublishedTree != intent.HeadTree || observation.LocalTree != intent.HeadTree {
			t.Fatalf("Observation trees = %q/%q", observation.PublishedTree, observation.LocalTree)
		}
		// The published and the local commit object name are both recorded and
		// are deliberately allowed to differ: the forge builds the commit.
		if observation.PublishedCommit != publishedCommitName || observation.LocalCommit != intent.HeadCommit {
			t.Fatalf("Observation commits = %q/%q", observation.PublishedCommit, observation.LocalCommit)
		}
		if observation.Ref != intent.Ref {
			t.Fatalf("Observation ref = %q", observation.Ref)
		}
		// No request body named force, on any call actually issued.
		for _, call := range transport.calls {
			if len(call.Body) == 0 {
				continue
			}
			for _, key := range bodyKeys(t, call.Body) {
				if strings.Contains(strings.ToLower(key), "force") {
					t.Fatalf("the %s body carried a key %q", call.Operation, key)
				}
			}
		}
	})

	violations := map[string]struct {
		override func(ForgeCall) ([]byte, error, bool)
		want     error
	}{
		"a blob object name disagrees": {func(call ForgeCall) ([]byte, error, bool) {
			if call.Operation == ForgeCreateBlob {
				return []byte(fmt.Sprintf(`{"sha":%q}`, strings.Repeat("9", 40))), nil, true
			}
			return nil, nil, false
		}, ErrForgeBlobDisagrees},
		"the created tree object name disagrees": {func(call ForgeCall) ([]byte, error, bool) {
			if call.Operation == ForgeCreateTree {
				return []byte(fmt.Sprintf(`{"sha":%q}`, strings.Repeat("8", 40))), nil, true
			}
			return nil, nil, false
		}, ErrForgeTreeDisagrees},
		"the published commit's tree disagrees": {func(call ForgeCall) ([]byte, error, bool) {
			if call.Operation == ForgeReadCommit {
				return []byte(fmt.Sprintf(`{"sha":%q,"tree":{"sha":%q},"parents":[{"sha":%q}]}`, publishedCommitName, strings.Repeat("7", 40), samplePayload().BaseCommit)), nil, true
			}
			return nil, nil, false
		}, ErrForgeCommitDisagrees},
		"the published commit's parent list disagrees": {func(call ForgeCall) ([]byte, error, bool) {
			if call.Operation == ForgeReadCommit {
				return []byte(fmt.Sprintf(`{"sha":%q,"tree":{"sha":%q},"parents":[{"sha":%q},{"sha":%q}]}`, publishedCommitName, samplePayload().HeadTree, samplePayload().BaseCommit, strings.Repeat("6", 40))), nil, true
			}
			return nil, nil, false
		}, ErrForgeCommitDisagrees},
		"the ref read back is another ref": {func(call ForgeCall) ([]byte, error, bool) {
			if call.Operation == ForgeReadRef {
				// An empty match list on the pre-read, then a list whose only
				// entry is a different ref: this ref is therefore absent after
				// the write, which is undecidable rather than confirmed.
				return []byte(fmt.Sprintf(`[{"ref":%q,"object":{"sha":%q,"type":"commit"}}]`, domain.PublicationRefPrefix+"increment-1/execution-9", publishedCommitName)), nil, true
			}
			return nil, nil, false
		}, application.ErrEffectUndecidable},
	}
	for name, violation := range violations {
		t.Run(name, func(t *testing.T) {
			answers := happyResponses(payload, intent)
			transport := scriptedTransport(t, payload, answers, violation.override)
			records := &stubRecords{}
			publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), records)
			err := publisher.Deliver(context.Background(), delivery)
			if !errors.Is(err, violation.want) {
				t.Fatalf("err = %v, want %v", err, violation.want)
			}
			for _, observation := range records.saved {
				if observation.State == domain.PublicationPublishedAndObserved {
					t.Fatalf("a violated equality was recorded as published and observed: %+v", observation)
				}
			}
		})
	}
}

func TestForgePublishRefusesBeforeAnyProcessCouldExist(t *testing.T) {
	payload := samplePayload()
	intent := sampleIntent()
	delivery := sampleDelivery(t, intent)

	// A18: a target that disagrees on any of the four fields refuses, and no
	// call is made at all.
	for name, target := range map[string]stubTarget{
		"no stored target": {found: false},
		"repository disagrees": {found: true, target: application.PublicationTarget{
			RepositoryID: "another-repository", Owner: intent.Locator.Owner, Name: intent.Locator.Name, Ref: intent.Ref, BaseCommit: intent.BaseCommit}},
		"owner disagrees": {found: true, target: application.PublicationTarget{
			RepositoryID: intent.RepositoryID.String(), Owner: "someone-else", Name: intent.Locator.Name, Ref: intent.Ref, BaseCommit: intent.BaseCommit}},
		"name disagrees": {found: true, target: application.PublicationTarget{
			RepositoryID: intent.RepositoryID.String(), Owner: intent.Locator.Owner, Name: "another-name", Ref: intent.Ref, BaseCommit: intent.BaseCommit}},
		"base commit disagrees": {found: true, target: application.PublicationTarget{
			RepositoryID: intent.RepositoryID.String(), Owner: intent.Locator.Owner, Name: intent.Locator.Name, Ref: intent.Ref, BaseCommit: strings.Repeat("5", 40)}},
	} {
		transport := scriptedTransport(t, payload, happyResponses(payload, intent), nil)
		publisher := sinkUnderTest(t, payload, transport, target, &stubRecords{})
		if err := publisher.Deliver(context.Background(), delivery); !errors.Is(err, ErrForgeTargetDisagrees) {
			t.Fatalf("%s: err = %v, want ErrForgeTargetDisagrees", name, err)
		}
		if len(transport.calls) != 0 {
			t.Fatalf("%s: %d calls were made; a target disagreement must refuse before any process could exist", name, len(transport.calls))
		}
	}

	// The three source-side refusals also happen before any call.
	for name, sourceErr := range map[string]error{
		"an empty change set": ErrPublicationChangeSetEmpty,
		"an unwritable mode":  ErrPublicationModeUnrepresentable,
		"a required deletion": ErrPublicationDeletionUnrepresentable,
		"unreadable content":  ErrPublicationContentUnreadable,
		"an escaping path":    ErrWorkingCopyPathEscapes,
	} {
		transport := scriptedTransport(t, payload, happyResponses(payload, intent), nil)
		publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), &stubRecords{})
		publisher.Source = &stubSource{err: sourceErr}
		if err := publisher.Deliver(context.Background(), delivery); !errors.Is(err, sourceErr) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if len(transport.calls) != 0 {
			t.Fatalf("%s: %d calls were made; the refusal must precede every forge call", name, len(transport.calls))
		}
	}

	// A payload that disagrees with the intent it is supposed to publish is
	// refused before any call as well.
	for name, mutate := range map[string]func(*PublicationPayload){
		"the derived head tree disagrees": func(p *PublicationPayload) { p.HeadTree = strings.Repeat("4", 40) },
		"the derived path count disagrees": func(p *PublicationPayload) {
			p.Files = p.Files[:1]
		},
	} {
		derived := samplePayload()
		mutate(&derived)
		transport := scriptedTransport(t, payload, happyResponses(payload, intent), nil)
		publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), &stubRecords{})
		publisher.Source = &stubSource{payload: derived}
		if err := publisher.Deliver(context.Background(), delivery); !errors.Is(err, ErrForgePayloadDisagrees) {
			t.Fatalf("%s: err = %v, want ErrForgePayloadDisagrees", name, err)
		}
		if len(transport.calls) != 0 {
			t.Fatalf("%s: %d calls were made", name, len(transport.calls))
		}
	}

	// A ref outside the reserved prefix cannot even be validated into an
	// intent, so a delivery carrying one is refused with no call at all.
	outside := intent
	outside.Ref = "refs/heads/main"
	transport := scriptedTransport(t, payload, happyResponses(payload, intent), nil)
	publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), &stubRecords{})
	if err := publisher.Deliver(context.Background(), sampleDelivery(t, outside)); !errors.Is(err, domain.ErrInvalidPublicationRef) {
		t.Fatalf("a ref outside the reserved prefix = %v", err)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("%d calls were made for a ref outside the reserved prefix", len(transport.calls))
	}

	// A publisher missing a collaborator refuses before anything happens.
	bare := testPublisher(t)
	if err := bare.Deliver(context.Background(), delivery); !errors.Is(err, ErrForgePublishNotConfigured) {
		t.Fatalf("an unconfigured publisher = %v", err)
	}
}

func TestForgePublishErrorsCarryNoChildOutput(t *testing.T) {
	// A10: the child's bytes never reach a caller. Every failure path is
	// constructed with a transport whose output and error both carry a marker,
	// and no returned error may contain it.
	const marker = "SENTINEL-CHILD-OUTPUT-MARKER"
	payload := samplePayload()
	intent := sampleIntent()
	delivery := sampleDelivery(t, intent)
	failing := map[string]func(ForgeCall) ([]byte, error, bool){
		"every call fails": func(ForgeCall) ([]byte, error, bool) {
			return []byte(marker), errors.New(marker), true
		},
		"every call answers unparsable bytes": func(ForgeCall) ([]byte, error, bool) {
			return []byte(marker), nil, true
		},
		"the pre-read fails": func(call ForgeCall) ([]byte, error, bool) {
			if call.Operation == ForgeReadRef {
				return []byte(marker), errors.New(marker), true
			}
			return nil, nil, false
		},
		"the ref creation fails and the read afterwards finds nothing": func(call ForgeCall) ([]byte, error, bool) {
			if call.Operation == ForgeCreateRef {
				return []byte(marker), errors.New(marker), true
			}
			if call.Operation == ForgeReadRef {
				return []byte(`[]`), nil, true
			}
			return nil, nil, false
		},
		"the ref creation fails and the read afterwards cannot complete": func(call ForgeCall) ([]byte, error, bool) {
			if call.Operation == ForgeCreateRef || call.Operation == ForgeReadRef {
				return []byte(marker), errors.New(marker), true
			}
			return nil, nil, false
		},
		"the read-after-write answers unparsable bytes": func(call ForgeCall) ([]byte, error, bool) {
			if call.Operation == ForgeReadCommit {
				return []byte(marker), nil, true
			}
			return nil, nil, false
		},
	}
	for name, override := range failing {
		transport := scriptedTransport(t, payload, happyResponses(payload, intent), override)
		records := &stubRecords{}
		publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), records)
		err := publisher.Deliver(context.Background(), delivery)
		if err == nil {
			t.Fatalf("%s: Deliver succeeded", name)
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("%s: the returned error carries the child's bytes: %v", name, err)
		}
		if strings.Contains(err.Error(), "/nonexistent/workspace") {
			t.Fatalf("%s: the returned error carries a workspace path: %v", name, err)
		}
		for _, observation := range records.saved {
			if strings.Contains(observation.Reason, marker) {
				t.Fatalf("%s: a recorded reason carries the child's bytes", name)
			}
		}
		// The observer path, on the same failure.
		observation, observeErr := publisher.Observe(context.Background(), delivery)
		if observeErr != nil && strings.Contains(observeErr.Error(), marker) {
			t.Fatalf("%s: Observe's error carries the child's bytes: %v", name, observeErr)
		}
		if observation == application.ObservationConfirmed && observeErr != nil {
			t.Fatalf("%s: a failed observation was confirmed", name)
		}
	}
}

func TestForgePublishConvergesOnAnExistingRefWithoutASecondCreate(t *testing.T) {
	// A11's second outcome: the ref is already present and its commit carries
	// the intended tree, so the operation converges as already-successful and
	// no create call is made at all.
	payload := samplePayload()
	intent := sampleIntent()
	delivery := sampleDelivery(t, intent)
	answers := happyResponses(payload, intent)
	answers[ForgeReadRef] = []string{fmt.Sprintf(`[{"ref":%q,"object":{"sha":%q,"type":"commit"}}]`, intent.Ref, publishedCommitName)}
	transport := scriptedTransport(t, payload, answers, nil)
	records := &stubRecords{}
	publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), records)
	if err := publisher.Deliver(context.Background(), delivery); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if transport.writes() != 0 {
		t.Fatalf("%d write calls were made on a convergent delivery", transport.writes())
	}
	if len(records.saved) != 1 || records.saved[0].State != domain.PublicationConvergedOnExistingRef {
		t.Fatalf("Observations = %+v", records.saved)
	}
}

func TestForgePublishReachesNeedsInputWhenTheRefDisagrees(t *testing.T) {
	// A11's third outcome: the ref exists and carries a different tree, so the
	// outcome is undecidable and reaches needs-input through the protocol's own
	// ambiguity route rather than through a guess or a force.
	payload := samplePayload()
	intent := sampleIntent()
	delivery := sampleDelivery(t, intent)
	answers := happyResponses(payload, intent)
	answers[ForgeReadRef] = []string{fmt.Sprintf(`[{"ref":%q,"object":{"sha":%q,"type":"commit"}}]`, intent.Ref, publishedCommitName)}
	answers[ForgeReadCommit] = []string{fmt.Sprintf(`{"sha":%q,"tree":{"sha":%q},"parents":[{"sha":%q}]}`, publishedCommitName, strings.Repeat("3", 40), intent.BaseCommit)}
	transport := scriptedTransport(t, payload, answers, nil)
	records := &stubRecords{}
	publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), records)
	err := publisher.Deliver(context.Background(), delivery)
	if !errors.Is(err, application.ErrEffectUndecidable) {
		t.Fatalf("err = %v, want application.ErrEffectUndecidable", err)
	}
	if transport.writes() != 0 {
		t.Fatalf("%d write calls were made on a disagreeing ref", transport.writes())
	}
	if len(records.saved) != 1 || records.saved[0].State != domain.PublicationRefDisagrees {
		t.Fatalf("Observations = %+v", records.saved)
	}
	if records.saved[0].TreesAgree {
		t.Fatal("a disagreeing ref was recorded with agreeing trees")
	}
	// The observer reaches the same answer, and it does not overwrite the
	// Observation the delivery already recorded.
	observation, err := publisher.Observe(context.Background(), delivery)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observation != application.ObservationUnknown {
		t.Fatalf("Observe = %q, want unknown so the protocol reaches needs-input", observation)
	}
	if len(records.saved) != 1 {
		t.Fatalf("the observer wrote a second Observation: %+v", records.saved)
	}
}

func TestForgePublishObserverAnswersOnlyFromReads(t *testing.T) {
	payload := samplePayload()
	intent := sampleIntent()
	delivery := sampleDelivery(t, intent)
	t.Run("absent", func(t *testing.T) {
		answers := happyResponses(payload, intent)
		answers[ForgeReadRef] = []string{`[]`}
		transport := scriptedTransport(t, payload, answers, nil)
		records := &stubRecords{}
		publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), records)
		observation, err := publisher.Observe(context.Background(), delivery)
		if err != nil || observation != application.ObservationNotObserved {
			t.Fatalf("Observe = %q, %v", observation, err)
		}
		if len(records.saved) != 0 {
			t.Fatalf("absence recorded an Observation: %+v", records.saved)
		}
		if transport.writes() != 0 {
			t.Fatal("the observer made a write call")
		}
	})
	t.Run("present_and_agreeing", func(t *testing.T) {
		answers := happyResponses(payload, intent)
		answers[ForgeReadRef] = []string{fmt.Sprintf(`[{"ref":%q,"object":{"sha":%q,"type":"commit"}}]`, intent.Ref, publishedCommitName)}
		transport := scriptedTransport(t, payload, answers, nil)
		records := &stubRecords{}
		publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), records)
		observation, err := publisher.Observe(context.Background(), delivery)
		if err != nil || observation != application.ObservationConfirmed {
			t.Fatalf("Observe = %q, %v", observation, err)
		}
		if len(records.saved) != 1 || records.saved[0].State != domain.PublicationPublishedAndObserved {
			t.Fatalf("Observations = %+v", records.saved)
		}
		if transport.writes() != 0 {
			t.Fatal("the observer made a write call")
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		transport := scriptedTransport(t, payload, map[ForgeAPIOperation][]string{}, func(call ForgeCall) ([]byte, error, bool) {
			return nil, fmt.Errorf("%w: injected", ErrForgePublishIncomplete), true
		})
		records := &stubRecords{}
		publisher := sinkUnderTest(t, payload, transport, sampleTarget(intent), records)
		observation, err := publisher.Observe(context.Background(), delivery)
		if !errors.Is(err, ErrForgePublishIncomplete) {
			t.Fatalf("Observe = %q, %v; an unreadable ref must stay ambiguous rather than become needs-input", observation, err)
		}
		if len(records.saved) != 0 {
			t.Fatalf("an unreadable observation recorded something: %+v", records.saved)
		}
	})
}

// --- A19, A23: the measured environment facts -------------------------------

func TestReservedPrefixTriggersNeitherPushWorkflow(t *testing.T) {
	// A19: the measurement, recorded rather than argued. Both workflow files
	// are read here so the claim is re-measured on every run instead of being
	// a comment that can go stale.
	root := filepath.Join("..", "..")
	measured := map[string]string{}
	for _, file := range []string{"ci.yml", "deploy.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", file))
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		body := string(raw)
		index := strings.Index(body, "push:")
		if index < 0 {
			t.Fatalf("%s has no push trigger", file)
		}
		rest := body[index:]
		line := strings.SplitN(rest, "\n", 3)
		if len(line) < 2 || !strings.Contains(line[1], "branches:") {
			t.Fatalf("%s: the push trigger's next line is not a branch filter: %q", file, line)
		}
		measured[file] = strings.TrimSpace(line[1])
	}
	if measured["ci.yml"] != "branches: [v2]" || measured["deploy.yml"] != "branches: [main]" {
		t.Fatalf("measured branch filters = %v", measured)
	}
	t.Logf("measured: .github/workflows/ci.yml push trigger filters %s", measured["ci.yml"])
	t.Logf("measured: .github/workflows/deploy.yml push trigger filters %s", measured["deploy.yml"])
	ref, err := domain.PublicationRefName("increment-1", "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	for _, filtered := range []string{"v2", "main"} {
		if branch == filtered {
			t.Fatalf("the published branch %q is one of the filtered branches", branch)
		}
	}
	t.Logf("measured: the reserved prefix produces branch %q, which matches neither filter", branch)
}

func TestForgeCLIIsAbsentFromTheValidatedPurePath(t *testing.T) {
	// A23: absolute path resolution is required rather than defensive. git and
	// unshare are on the validated pure PATH and the forge CLI is not, so a
	// bare exec by name would work on the host and fail in the validated
	// environment. Recorded as a measurement; nothing is started.
	for _, name := range []string{gitExecutableName, "unshare"} {
		resolved := resolveTool(name)
		if !filepath.IsAbs(resolved) {
			t.Fatalf("%s did not resolve to an absolute path: %q", name, resolved)
		}
		t.Logf("measured: %s resolves to %s", name, resolved)
	}
	resolved := resolveTool(forgeExecutableName)
	t.Logf("measured: %s resolves to %q (PATH lookup first, then the conventional system locations)", forgeExecutableName, resolved)
	onPath := false
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, forgeExecutableName)); err == nil && !info.IsDir() {
			onPath = true
			t.Logf("measured: %s is on PATH at %s", forgeExecutableName, filepath.Join(dir, forgeExecutableName))
			break
		}
	}
	t.Logf("measured: %s present on this process's PATH = %v", forgeExecutableName, onPath)
	// Whatever the answer, no test in this package starts it.
	if resolved == forgeExecutableName {
		t.Logf("measured: %s could not be resolved at all in this environment, so every call would refuse before a process could exist", forgeExecutableName)
	}
}

// --- A21(i): the AST control ------------------------------------------------

func TestPublicationFilesSaveNoRequirementAndNoIncrement(t *testing.T) {
	// non_goal 2, structurally: a published branch is not a finished
	// Requirement, and no file this task adds contains a call that could move
	// one. The scan reads the AST rather than the file text, so it cannot be
	// defeated by a name that merely looks different.
	files := []string{"publishsource.go", "forgepublish.go", filepath.Join("..", "application", "publication.go")}
	forbidden := []string{"SaveRequirement", "SaveIncrement", "DecideRequirement", "DecideIncrement", "SaveRequirementText", "SaveExecution"}
	scanned, selectors := 0, 0
	for _, path := range files {
		parsed, err := parseOneFileForPublicationScan(t, path)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++
		for _, name := range parsed {
			selectors++
			for _, bad := range forbidden {
				if name == bad {
					t.Fatalf("%s calls %s; a publication changes no Requirement and no Increment", path, bad)
				}
			}
		}
	}
	if scanned != len(files) {
		t.Fatalf("scanned %d of %d files", scanned, len(files))
	}
	if selectors == 0 {
		t.Fatal("the AST walk found no call at all; the scan would pass vacuously")
	}
	// Negative control: the walk really does see the calls these files make.
	names, err := parseOneFileForPublicationScan(t, filepath.Join("..", "application", "publication.go"))
	if err != nil {
		t.Fatal(err)
	}
	sawRequirementRead := false
	for _, name := range names {
		if name == "Requirement" {
			sawRequirementRead = true
		}
	}
	if !sawRequirementRead {
		t.Fatal("the AST walk did not see the Requirement READ the target resolution performs; the scan is not looking at calls")
	}
	t.Logf("scanned files=%d call selectors=%d", scanned, selectors)
}

// parseOneFileForPublicationScan returns every called selector name in one
// file, e.g. "SaveRequirement" for u.SaveRequirement(...).
func parseOneFileForPublicationScan(t *testing.T, path string) ([]string, error) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	names := []string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			names = append(names, fun.Sel.Name)
		case *ast.Ident:
			names = append(names, fun.Name)
		}
		return true
	})
	return names, nil
}

// --- A20: the whole protocol, through the REAL dispatcher -------------------
//
// The dispatcher is application.OutboxDispatcher, the store is the real
// internal/store/memory adapter, the sink and the observer are the real
// ForgePublisher, the command is the real Service.PublishChange and the source
// is the real GitSourceControl reading a real commit made inside a real
// namespace from a local bare origin. The ONLY injection is the transport, and
// it is a function.

// protocolGraph is one Increment claimed by one Runner against one registered
// Repository, built through the real Service against a real memory store.
type protocolGraph struct {
	store      *memory.Store
	service    *application.Service
	clock      *mutableClock
	ownerCtx   context.Context
	runnerCtx  context.Context
	repository string
	increment  string
	execution  string
	lease      string
	fence      domain.FencingToken
	version    domain.Version
	revision   domain.Revision
}

func newProtocolGraph(t *testing.T, tag string) *protocolGraph {
	t.Helper()
	clock := newMutableClock(fixtureInstant)
	store := memory.New()
	service, err := application.NewServiceWithConfig(store, clock, &journeyIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner-1"})
	runnerID := tag + "-runner"
	runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-subject", RunnerID: runnerID})
	repository, err := service.RegisterRepository(ownerCtx, application.RegisterRepositoryRequest{RequestID: tag + ":register", SourceURL: "https://github.com/Owner/Name.git", DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("RegisterRepository: %v", err)
	}
	captured, err := service.Capture(ownerCtx, application.CaptureRequest{RequestID: tag + ":capture", Text: "publish", RepositoryID: repository.RepositoryID})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	planned, err := service.Plan(ownerCtx, application.PlanRequest{RequestID: tag + ":plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err = store.Transact(ownerCtx, func(u application.UnitOfWork) error {
		increment, _, e := u.Increment(ownerCtx, planned.IncrementID)
		if e != nil {
			return e
		}
		actor, _ := domain.NewActorID("owner-1")
		next, e := domain.DecideIncrement(increment, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: actor, At: clock.Now(), ExpectedVersion: increment.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ownerCtx, next, increment.Version)
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	claim, err := service.Claim(runnerCtx, application.ClaimRequest{RequestID: tag + ":claim", IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var version domain.Version
	if err = store.Transact(ownerCtx, func(u application.UnitOfWork) error {
		execution, _, e := u.Execution(ownerCtx, claim.ExecutionID)
		if e != nil {
			return e
		}
		version = execution.Version
		// Claim mints its own "claim-issued" Outbox Item, which this
		// publication sink is not the sink for. It is marked delivered here so
		// the dispatcher passes below reason about exactly one item: the
		// publication. Nothing about the publication protocol is bypassed.
		items, e := u.Outboxes(ownerCtx, clock.Now(), 100)
		if e != nil {
			return e
		}
		for _, item := range items {
			if item.Kind == application.PublicationOutboxKind {
				continue
			}
			item.Status = application.OutboxDelivered
			if e = u.SaveOutbox(ownerCtx, item, item.Version); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &protocolGraph{
		store: store, service: service, clock: clock, ownerCtx: ownerCtx, runnerCtx: runnerCtx,
		repository: repository.RepositoryID, increment: planned.IncrementID, execution: claim.ExecutionID,
		lease: claim.LeaseID, fence: claim.FencingToken, version: version,
	}
}

func (g *protocolGraph) publish(t *testing.T, tag string, observation IntegrityObservation, working WorkingCopy, changed int) application.PublishChangeResponse {
	t.Helper()
	out, err := g.service.PublishChange(g.runnerCtx, application.PublishChangeRequest{
		RequestID: tag, ExecutionID: g.execution, LeaseID: g.lease,
		ExpectedExecutionVersion: g.version, FencingToken: g.fence, ControlRevision: g.revision,
		BaseBranch: working.BaseBranch, BaseCommit: working.BaseCommit,
		HeadCommit: observation.HeadCommit, HeadTree: observation.TreeName, ChangedPaths: changed,
	})
	if err != nil {
		t.Fatalf("PublishChange: %v", err)
	}
	return out
}

func (g *protocolGraph) outboxStatus(t *testing.T, id string) application.OutboxItem {
	t.Helper()
	var item application.OutboxItem
	if err := g.store.Transact(g.ownerCtx, func(u application.UnitOfWork) error {
		value, ok, e := u.Outbox(g.ownerCtx, id)
		if e != nil {
			return e
		}
		if !ok {
			t.Fatalf("outbox item %q is absent", id)
		}
		item = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func (g *protocolGraph) dispatcher(t *testing.T, publisher ForgePublisher) *application.OutboxDispatcher {
	t.Helper()
	dispatcher, err := application.NewOutboxDispatcher(g.store, g.clock, publisher, application.DispatcherConfig{
		Owner: "publication-dispatcher", LeaseTTL: time.Minute, BatchSize: 10, Observer: publisher,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

// deliveries records the identifiers every delivery carried, so the claim that
// the operation identifier IS the idempotency key can be measured on every
// attempt rather than argued.
type deliveries struct {
	operation []string
	key       []string
}

func TestPublicationProtocolRunsThroughTheRealDispatcher(t *testing.T) {
	requireNamespace(t)
	adapter, working := publishFixtureCopy(t, "protocol-1")
	ctx := context.Background()
	change := ChangeSet{Subject: "publish a reviewable change", Files: []ChangeFile{
		{Path: "README.md", Content: []byte("seed\nreviewable\n")},
		{Path: "docs/note.md", Content: []byte("note\n")},
	}}
	if err := adapter.ApplyChange(ctx, working, change); err != nil {
		t.Fatalf("ApplyChange: %v", err)
	}
	if _, err := adapter.Commit(ctx, working, change); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	observation, err := adapter.VerifyIntegrity(ctx, working)
	if err != nil {
		t.Fatalf("VerifyIntegrity: %v", err)
	}
	payload, err := adapter.PublicationPayload(ctx, working, working.BaseCommit, observation.HeadCommit)
	if err != nil {
		t.Fatalf("PublicationPayload: %v", err)
	}
	t.Logf("execution fact: real commit derived %d changed paths, head tree measured locally", len(payload.Files))

	// publisherFor wires the real sink and observer over the real Service, with
	// only the transport injected.
	publisherFor := func(g *protocolGraph, transport *recordedTransport) ForgePublisher {
		publisher := testPublisher(t)
		publisher.Source = adapter
		publisher.Working = working
		publisher.Target = g.service
		publisher.Records = g.service
		publisher.Transport = transport.transport()
		return publisher
	}

	refFor := func(g *protocolGraph) string {
		ref, e := domain.PublicationRefName(domain.IncrementID(g.increment), domain.ExecutionID(g.execution))
		if e != nil {
			t.Fatal(e)
		}
		return ref
	}

	t.Run("created_and_confirmed", func(t *testing.T) {
		g := newProtocolGraph(t, "created")
		out := g.publish(t, "created:publish", observation, working, len(payload.Files))
		intent := domain.PublicationIntent{Ref: refFor(g), HeadTree: observation.TreeName, BaseCommit: working.BaseCommit}
		transport := scriptedTransport(t, payload, happyResponses(payload, intent), nil)
		publisher := publisherFor(g, transport)
		dispatcher := g.dispatcher(t, publisher)
		report, err := dispatcher.Dispatch(g.runnerCtx)
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if report.Delivered != 1 {
			t.Fatalf("report = %+v", report)
		}
		item := g.outboxStatus(t, out.OutboxID)
		if item.Status != application.OutboxDelivered {
			t.Fatalf("outbox status = %q, want delivered", item.Status)
		}
		if item.OperationID != out.OperationID {
			t.Fatalf("the delivered item's operation id moved: %q vs %q", item.OperationID, out.OperationID)
		}
		stored, found, err := g.service.Publication(g.runnerCtx, out.OperationID)
		if err != nil || !found {
			t.Fatalf("Publication = %v, %v", found, err)
		}
		if stored.State != domain.PublicationPublishedAndObserved {
			t.Fatalf("Observation state = %q", stored.State)
		}
		if stored.PublishedTree != observation.TreeName || stored.LocalTree != observation.TreeName || !stored.TreesAgree {
			t.Fatalf("Observation = %+v", stored)
		}
		if stored.LocalCommit != observation.HeadCommit {
			t.Fatalf("Observation local commit = %q", stored.LocalCommit)
		}
		// The local and published commit object names are recorded and their
		// agreement is NOT required.
		t.Logf("measured: local commit and published commit object names agree = %v", stored.LocalCommit == stored.PublishedCommit)
		if transport.count(ForgeCreateBlob) != len(payload.Files) || transport.count(ForgeCreateRef) != 1 {
			t.Fatalf("write calls = %d blobs, %d refs", transport.count(ForgeCreateBlob), transport.count(ForgeCreateRef))
		}
		for _, call := range transport.calls {
			if len(call.Body) == 0 {
				continue
			}
			for _, key := range bodyKeys(t, call.Body) {
				if strings.Contains(strings.ToLower(key), "force") {
					t.Fatalf("the %s body carried %q", call.Operation, key)
				}
			}
		}
	})

	t.Run("already_present_and_convergent_across_two_attempts", func(t *testing.T) {
		g := newProtocolGraph(t, "converge")
		out := g.publish(t, "converge:publish", observation, working, len(payload.Files))
		intent := domain.PublicationIntent{Ref: refFor(g), HeadTree: observation.TreeName, BaseCommit: working.BaseCommit}
		seen := &deliveries{}
		attempt := 0
		answers := happyResponses(payload, intent)
		refPresent := fmt.Sprintf(`[{"ref":%q,"object":{"sha":%q,"type":"commit"}}]`, intent.Ref, publishedCommitName)
		transport := scriptedTransport(t, payload, answers, func(call ForgeCall) ([]byte, error, bool) {
			if attempt == 0 {
				// First attempt: the ref is absent, every object agrees, and
				// the ref creation reports no success while the read afterwards
				// still finds nothing. That is a plain, retryable failure.
				if call.Operation == ForgeCreateRef {
					return nil, fmt.Errorf("%w: injected", ErrForgePublishIncomplete), true
				}
				if call.Operation == ForgeReadRef {
					return []byte(`[]`), nil, true
				}
				return nil, nil, false
			}
			// Second attempt: the ref is present carrying the intended tree.
			if call.Operation == ForgeReadRef {
				return []byte(refPresent), nil, true
			}
			return nil, nil, false
		})
		publisher := publisherFor(g, transport)
		sink := application.EffectSinkFunc(func(callCtx context.Context, delivery application.EffectDelivery) error {
			seen.operation = append(seen.operation, delivery.OperationID)
			seen.key = append(seen.key, delivery.IdempotencyKey)
			return publisher.Deliver(callCtx, delivery)
		})
		dispatcher, err := application.NewOutboxDispatcher(g.store, g.clock, sink, application.DispatcherConfig{
			Owner: "publication-dispatcher", LeaseTTL: time.Minute, BatchSize: 10, Observer: publisher,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = dispatcher.Dispatch(g.runnerCtx); err != nil {
			t.Fatalf("first Dispatch: %v", err)
		}
		if status := g.outboxStatus(t, out.OutboxID).Status; status != application.OutboxWaiting {
			t.Fatalf("after a retryable failure the status is %q, want waiting", status)
		}
		writesFirst := transport.writes()
		if writesFirst == 0 {
			t.Fatal("the first attempt made no write call")
		}
		// The second attempt converges. The clock is advanced past the retry
		// instant with an INJECTED clock: no sleep and no timer. The step is
		// two seconds rather than two minutes on purpose -- the retry policy's
		// base delay is one second and the Lease TTL is one minute, so a larger
		// step would expire the Lease and the item would be superseded instead
		// of retried, which is a different measurement.
		attempt = 1
		g.clock.Advance(2 * time.Second)
		if _, err = dispatcher.Dispatch(g.runnerCtx); err != nil {
			t.Fatalf("second Dispatch: %v", err)
		}
		if status := g.outboxStatus(t, out.OutboxID).Status; status != application.OutboxDelivered {
			t.Fatalf("after convergence the status is %q, want delivered", status)
		}
		if transport.writes() != writesFirst {
			t.Fatalf("the convergent attempt made %d more write calls; it must make none", transport.writes()-writesFirst)
		}
		if len(seen.operation) != 2 {
			t.Fatalf("%d deliveries were attempted, want 2", len(seen.operation))
		}
		for i := range seen.operation {
			if seen.operation[i] != out.OperationID || seen.key[i] != out.OperationID {
				t.Fatalf("attempt %d carried operation %q key %q, want %q for both", i, seen.operation[i], seen.key[i], out.OperationID)
			}
		}
		stored, found, err := g.service.Publication(g.runnerCtx, out.OperationID)
		if err != nil || !found {
			t.Fatalf("Publication = %v, %v", found, err)
		}
		if stored.State != domain.PublicationConvergedOnExistingRef {
			t.Fatalf("Observation state = %q, want the convergent one", stored.State)
		}
	})

	t.Run("present_and_disagreeing_reaches_needs_input", func(t *testing.T) {
		g := newProtocolGraph(t, "disagree")
		out := g.publish(t, "disagree:publish", observation, working, len(payload.Files))
		intent := domain.PublicationIntent{Ref: refFor(g), HeadTree: observation.TreeName, BaseCommit: working.BaseCommit}
		answers := happyResponses(payload, intent)
		answers[ForgeReadRef] = []string{fmt.Sprintf(`[{"ref":%q,"object":{"sha":%q,"type":"commit"}}]`, intent.Ref, publishedCommitName)}
		answers[ForgeReadCommit] = []string{fmt.Sprintf(`{"sha":%q,"tree":{"sha":%q},"parents":[{"sha":%q}]}`, publishedCommitName, strings.Repeat("2", 40), intent.BaseCommit)}
		transport := scriptedTransport(t, payload, answers, nil)
		publisher := publisherFor(g, transport)
		dispatcher := g.dispatcher(t, publisher)
		if _, err := dispatcher.Dispatch(g.runnerCtx); err != nil {
			t.Fatalf("first Dispatch: %v", err)
		}
		if status := g.outboxStatus(t, out.OutboxID).Status; status != application.OutboxAmbiguous {
			t.Fatalf("an undecidable delivery left the item %q, want ambiguous", status)
		}
		if transport.writes() != 0 {
			t.Fatalf("%d write calls were made against a disagreeing ref", transport.writes())
		}
		// The second pass observes and reaches needs-input.
		if _, err := dispatcher.Dispatch(g.runnerCtx); err != nil {
			t.Fatalf("second Dispatch: %v", err)
		}
		item := g.outboxStatus(t, out.OutboxID)
		if item.Status != application.OutboxNeedsInput {
			t.Fatalf("outbox status = %q, want needs-input", item.Status)
		}
		if item.Observation != application.ObservationUnknown {
			t.Fatalf("outbox observation = %q", item.Observation)
		}
		stored, found, err := g.service.Publication(g.runnerCtx, out.OperationID)
		if err != nil || !found {
			t.Fatalf("Publication = %v, %v", found, err)
		}
		if stored.State != domain.PublicationRefDisagrees || stored.TreesAgree {
			t.Fatalf("Observation = %+v", stored)
		}
		if transport.writes() != 0 {
			t.Fatal("the observer made a write call")
		}
	})

	for _, unknownCase := range []struct {
		name      string
		afterward string
		want      application.OutboxStatus
		state     domain.PublicationState
	}{
		{"unknown_then_confirmed", "agreeing", application.OutboxConfirmed, domain.PublicationPublishedAndObserved},
		{"unknown_then_needs_input", "disagreeing", application.OutboxNeedsInput, domain.PublicationRefDisagrees},
	} {
		t.Run(unknownCase.name, func(t *testing.T) {
			g := newProtocolGraph(t, unknownCase.name)
			out := g.publish(t, unknownCase.name+":publish", observation, working, len(payload.Files))
			intent := domain.PublicationIntent{Ref: refFor(g), HeadTree: observation.TreeName, BaseCommit: working.BaseCommit}
			answers := happyResponses(payload, intent)
			tree := observation.TreeName
			if unknownCase.afterward == "disagreeing" {
				tree = strings.Repeat("1", 40)
			}
			answers[ForgeReadCommit] = []string{fmt.Sprintf(`{"sha":%q,"tree":{"sha":%q},"parents":[{"sha":%q}]}`, publishedCommitName, tree, intent.BaseCommit)}
			attempt := 0
			reads := 0
			refPresent := fmt.Sprintf(`[{"ref":%q,"object":{"sha":%q,"type":"commit"}}]`, intent.Ref, publishedCommitName)
			transport := scriptedTransport(t, payload, answers, func(call ForgeCall) ([]byte, error, bool) {
				if attempt == 0 {
					// The write's outcome is unknown: the pre-read finds the ref
					// absent, the ref creation reports nothing, and the read
					// afterwards cannot complete either.
					if call.Operation == ForgeCreateRef {
						return nil, fmt.Errorf("%w: injected", ErrForgePublishIncomplete), true
					}
					if call.Operation == ForgeReadRef {
						reads++
						if reads == 1 {
							return []byte(`[]`), nil, true
						}
						return nil, fmt.Errorf("%w: injected", ErrForgePublishIncomplete), true
					}
					return nil, nil, false
				}
				if call.Operation == ForgeReadRef {
					return []byte(refPresent), nil, true
				}
				return nil, nil, false
			})
			publisher := publisherFor(g, transport)
			dispatcher := g.dispatcher(t, publisher)
			if _, err := dispatcher.Dispatch(g.runnerCtx); err != nil {
				t.Fatalf("first Dispatch: %v", err)
			}
			if status := g.outboxStatus(t, out.OutboxID).Status; status != application.OutboxAmbiguous {
				t.Fatalf("an unknown outcome left the item %q, want ambiguous", status)
			}
			attempt = 1
			if _, err := dispatcher.Dispatch(g.runnerCtx); err != nil {
				t.Fatalf("second Dispatch: %v", err)
			}
			item := g.outboxStatus(t, out.OutboxID)
			if item.Status != unknownCase.want {
				t.Fatalf("outbox status = %q, want %q", item.Status, unknownCase.want)
			}
			stored, found, err := g.service.Publication(g.runnerCtx, out.OperationID)
			if err != nil || !found {
				t.Fatalf("Publication = %v, %v", found, err)
			}
			if stored.State != unknownCase.state {
				t.Fatalf("Observation state = %q, want %q", stored.State, unknownCase.state)
			}
		})
	}

	t.Run("stale_fence_yields_superseded_with_no_transport_call", func(t *testing.T) {
		g := newProtocolGraph(t, "stale")
		out := g.publish(t, "stale:publish", observation, working, len(payload.Files))
		// A newer lease supersedes the one the item was minted under.
		if err := g.store.Transact(g.ownerCtx, func(u application.UnitOfWork) error {
			lease, _, e := u.Lease(g.ownerCtx, g.lease)
			if e != nil {
				return e
			}
			lease.FencingToken = g.fence + 1
			return u.SaveLease(g.ownerCtx, lease, lease.Version)
		}); err != nil {
			t.Fatal(err)
		}
		transport := scriptedTransport(t, payload, happyResponses(payload, domain.PublicationIntent{Ref: refFor(g), HeadTree: observation.TreeName, BaseCommit: working.BaseCommit}), nil)
		publisher := publisherFor(g, transport)
		dispatcher := g.dispatcher(t, publisher)
		if _, err := dispatcher.Dispatch(g.runnerCtx); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if status := g.outboxStatus(t, out.OutboxID).Status; status != application.OutboxSuperseded {
			t.Fatalf("outbox status = %q, want superseded", status)
		}
		if len(transport.calls) != 0 {
			t.Fatalf("%d transport calls were made under a stale fence; the answer is none at all", len(transport.calls))
		}
	})

	t.Run("a_control_intent_for_the_repository_scope_prevents_delivery", func(t *testing.T) {
		g := newProtocolGraph(t, "denied")
		out := g.publish(t, "denied:publish", observation, working, len(payload.Files))
		if _, err := g.service.Control(g.ownerCtx, application.ControlRequest{
			RequestID: "denied:control", Scope: domain.ControlScope{Kind: domain.ScopeRepository, Value: g.repository},
			Mode: domain.ControlImmediateStop, At: g.clock.Now(),
		}); err != nil {
			t.Fatalf("Control: %v", err)
		}
		transport := scriptedTransport(t, payload, happyResponses(payload, domain.PublicationIntent{Ref: refFor(g), HeadTree: observation.TreeName, BaseCommit: working.BaseCommit}), nil)
		publisher := publisherFor(g, transport)
		dispatcher := g.dispatcher(t, publisher)
		if _, err := dispatcher.Dispatch(g.runnerCtx); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if status := g.outboxStatus(t, out.OutboxID).Status; status == application.OutboxDelivered || status == application.OutboxConfirmed {
			t.Fatalf("a denied publication was delivered: status = %q", status)
		}
		if len(transport.calls) != 0 {
			t.Fatalf("%d transport calls were made under a denying Control Intent", len(transport.calls))
		}
	})
}
