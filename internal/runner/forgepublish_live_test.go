package runner

// The single gated live publication (V2-072 A24). It is gated on
// AGENTIC_LOOP_LIVE_FORGE_WRITE=1 together with an owner-supplied
// AGENTIC_LOOP_LIVE_FORGE_WRITE_REPOSITORY of the form <owner>/<name>, so a
// plain `go test ./...` -- and therefore `make check` -- never resolves a
// hostname, never opens a socket and never starts the forge CLI.
// `devbox run --pure` strips the environment, so the gate cannot fire by
// accident and -e is required to set it.
//
// The target is NOT hardcoded and there is deliberately no default, in
// particular no default to the Loop's own registered repository.
// docs/architecture/validation.md requires an owner-designated target, and for
// a WRITE the argument is stronger than for a read: a hardcoded default would
// make this check's meaning depend on the unmeasured fact that the owner
// accepts agent-written refs there. With the gate set and the designation
// absent this check FAILS naming the missing designation; it does not skip.
//
// What it leaves behind is the artefact: one branch under the reserved prefix.
// It does NOT delete the ref. The exact owner-run delete command is printed,
// together with the two residues deleting the ref does not remove.
//
// Determinism: no fixed sleep, no wall-clock timer and no goroutine. The
// child's lifetime is bounded by a context deadline.

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

const (
	liveForgeWriteGate       = "AGENTIC_LOOP_LIVE_FORGE_WRITE"
	liveForgeWriteRepository = "AGENTIC_LOOP_LIVE_FORGE_WRITE_REPOSITORY"
	// liveForgeWriteOrigin is the owner-supplied clone source for the live
	// working copy. It is optional: when it is absent the live copy is built
	// from a local bare origin created at test time, which is enough to produce
	// a real commit, and the WRITE target still comes only from the
	// designation above.
	liveForgeWriteOrigin = "AGENTIC_LOOP_LIVE_FORGE_WRITE_ORIGIN"
)

// liveForgeWriteDeadline bounds the whole check. It is a context deadline, not
// a sleep and not a timer.
const liveForgeWriteDeadline = 180 * time.Second

// TestForgePublicationLive publishes exactly one branch to the owner-designated
// repository through the real forge CLI, measures the four content-addressed
// equalities against the real forge, measures the workflow-run count for the
// branch and requires zero, and records what a person needs to find and delete
// the branch.
func TestForgePublicationLive(t *testing.T) {
	gate := os.Getenv(liveForgeWriteGate)
	designation := strings.TrimSpace(os.Getenv(liveForgeWriteRepository))
	if gate != "1" {
		t.Logf("live forge write gate: %s=%q, want \"1\"; recorded as skipped with this reason, never as a pass", liveForgeWriteGate, gate)
		t.Skip("live forge write check is disabled")
	}
	if designation == "" {
		// The gate was explicitly requested, so an absent designation is a
		// measured failure of this check and not a skip. There is no default,
		// and in particular no default to the Loop's own repository.
		t.Fatalf("live forge write check requested (%s=1) but %s is not set: the write target must be designated by the owner as <owner>/<name>, because hardcoding one would make this check's meaning depend on the unmeasured fact that the owner accepts agent-written refs in that repository", liveForgeWriteGate, liveForgeWriteRepository)
	}
	owner, name, found := strings.Cut(designation, "/")
	if !found || !validForgeSegment(owner) || !validForgeSegment(name) {
		t.Fatalf("%s=%q is not one <owner>/<name> designation", liveForgeWriteRepository, designation)
	}

	requireNamespace(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveForgeWriteDeadline)
	defer cancel()

	// --- execution environment identifiers ---------------------------------
	parent := t.TempDir()
	origin := strings.TrimSpace(os.Getenv(liveForgeWriteOrigin))
	remoteOrigin := origin != ""
	if !remoteOrigin {
		origin = seedBareOrigin(t, parent)
		t.Logf("live origin: a bare repository created at test time by the real git binary (%s); the WRITE target is the designation only", origin)
	} else {
		t.Logf("live origin: owner-supplied through %s", liveForgeWriteOrigin)
	}
	workspace := newTestWorkspace(t, parent)
	adapter := newTestAdapter(t, workspace)
	resolvedGit, err := adapter.ResolveExecutable()
	if err != nil {
		t.Fatalf("live forge write requested but git could not be resolved: %v", err)
	}
	gitVersion, err := adapter.Version(ctx)
	if err != nil {
		t.Fatalf("live forge write requested but git --version failed: %v", err)
	}
	publisher := NewForgePublisher("v2-072")
	publisher.Now = func() time.Time { return time.Now().UTC() }
	resolvedForge, err := publisher.ResolveExecutable()
	if err != nil {
		t.Fatalf("live forge write requested but the forge CLI could not be resolved: %v", err)
	}
	forgeVersion, err := ForgeClient{ExecutablePath: resolvedForge, BaseEnvironmentNames: append([]string(nil), DefaultForgeBaseEnvironmentNames...)}.ReadVersion(ctx)
	if err != nil {
		t.Fatalf("live forge write requested but the forge CLI version could not be read: %v", err)
	}
	t.Logf("execution environment: uname -srm equivalent = %s %s %s", runtime.GOOS, kernelRelease(), runtime.GOARCH)
	t.Logf("execution environment: go = %s", runtime.Version())
	t.Logf("execution environment: resolved absolute git path = %s, %s", resolvedGit, gitVersion)
	t.Logf("execution environment: resolved absolute forge CLI path = %s, %s", resolvedForge, forgeVersion)
	t.Logf("execution environment: owner-designated repository = %s/%s (supplied through %s)", owner, name, liveForgeWriteRepository)

	// --- no push argv is constructible, on the live path too ---------------
	for _, subcommand := range []string{"push", "fetch", "remote", "config", "worktree"} {
		if _, e := adapter.buildGitArgv(nil, subcommand); !errors.Is(e, ErrGitSubcommandNotAllowed) {
			t.Fatalf("an argv naming %q was constructible on the live path: %v", subcommand, e)
		}
	}
	for _, absent := range []ForgeAPIOperation{"update-ref", "delete-ref", "force-ref"} {
		if _, e := publisher.Argv(absent, owner, name, ""); !errors.Is(e, ErrForgeOperationNotAllowed) {
			t.Fatalf("an argv for %q was constructible on the live path: %v", absent, e)
		}
	}

	// --- a real commit through the real adapter inside the real namespace --
	graph := newProtocolGraph(t, "live")
	if err = graph.store.Transact(graph.ownerCtx, func(u application.UnitOfWork) error {
		repository, _, e := u.Repository(graph.ownerCtx, graph.repository)
		if e != nil {
			return e
		}
		locator, e := domain.NormalizeSourceLocator(domain.SourceLocator{Owner: owner, Name: name, DefaultBranch: "main"})
		if e != nil {
			return e
		}
		repository.Locator = locator
		return u.SaveRepository(graph.ownerCtx, repository, repository.Version)
	}); err != nil {
		t.Fatalf("pointing the registered Repository at the designated target: %v", err)
	}
	working, err := adapter.Materialize(ctx, MaterializeRequest{
		IncrementID: graph.increment, ExecutionID: "live-publication", RepositoryID: graph.repository,
		Origin: origin, Branch: "agentic-loop/live-publication", Shallow: remoteOrigin,
	})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	change := ChangeSet{Subject: "agentic-loop: publish one verified change", Files: []ChangeFile{
		{Path: "agentic-loop-publication.md", Content: []byte("This branch was published by the agentic loop's external side-effect protocol.\n")},
	}}
	if err = adapter.ApplyChange(ctx, working, change); err != nil {
		t.Fatalf("ApplyChange: %v", err)
	}
	if _, err = adapter.Commit(ctx, working, change); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	observation, err := adapter.VerifyIntegrity(ctx, working)
	if err != nil {
		t.Fatalf("VerifyIntegrity: %v", err)
	}
	if !observation.Clean {
		t.Fatalf("the live working copy is not clean: %+v", observation)
	}

	// --- publish through the real dispatcher -------------------------------
	out := graph.publish(t, "live:publish", observation, working, observation.ChangedPaths)
	publisher.Source = adapter
	publisher.Working = working
	publisher.Target = graph.service
	publisher.Records = graph.service
	dispatcher := graph.dispatcher(t, publisher)
	report, err := dispatcher.Dispatch(graph.runnerCtx)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	item := graph.outboxStatus(t, out.OutboxID)
	if item.Status != application.OutboxDelivered && item.Status != application.OutboxConfirmed {
		t.Fatalf("the live publication reached status %q (report %+v); the branch was not published", item.Status, report)
	}

	// --- the four equalities, measured against the real forge --------------
	stored, ok, err := graph.service.Publication(graph.runnerCtx, out.OperationID)
	if err != nil || !ok {
		t.Fatalf("the live publication recorded no Observation: %v %v", ok, err)
	}
	if stored.State != domain.PublicationPublishedAndObserved {
		t.Fatalf("the live Observation state is %q", stored.State)
	}
	if !stored.TreesAgree || stored.PublishedTree != observation.TreeName || stored.LocalTree != observation.TreeName {
		t.Fatalf("the tree equality did not hold: %+v", stored)
	}
	if stored.Ref != out.Ref {
		t.Fatalf("the published ref is %q, want %q", stored.Ref, out.Ref)
	}
	t.Logf("live measurement: ref = %s", stored.Ref)
	t.Logf("live measurement: published commit object name recorded, published tree object name recorded, observed_at = %s", stored.ObservedAt.UTC().Format(time.RFC3339))
	t.Logf("live measurement: the local commit object name and the published commit object name agree = %v (recorded, NOT required: the forge constructs the commit object)", stored.LocalCommit == stored.PublishedCommit)

	// --- the workflow-run count for the branch, measured -------------------
	branch := strings.TrimPrefix(stored.Ref, "refs/heads/")
	runs, err := publisher.WorkflowRunCount(ctx, owner, name, branch)
	if err != nil {
		t.Fatalf("the workflow-run count for the published branch could not be read: %v", err)
	}
	if runs != 0 {
		t.Fatalf("the published branch triggered %d workflow run(s); the reserved prefix must trigger none", runs)
	}
	t.Logf("live measurement: workflow runs for the published branch = %d", runs)

	// --- what a person needs to find and remove the branch ----------------
	t.Logf("owner action to delete the published ref: gh api --method DELETE repos/%s/%s/git/%s", owner, name, branch)
	t.Logf("NOTE: deleting the ref removes the reviewable artefact but NOT two residues: the blob, tree and commit objects created remain reachable by object name, and the ref-creation entry in the repository's own activity record cannot be deleted at all")
	t.Log("this check deliberately does NOT delete the ref: the branch is the artefact a person reads")
}
