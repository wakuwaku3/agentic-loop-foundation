package application_test

// Tests for the owner-readable release surface (V2-066).
//
// Determinism: every timestamp is injected (injectedReleaseInstant), no test
// sleeps, starts a goroutine, or waits on a timer, and every observer is
// constructed with an explicit root, repository, environment class and
// assembly instant.
//
// Each test uses its own memory store. A bounded owner read reserves
// quota.ReadTransactionUsage inside the same transaction and fails closed at
// 80% of the daily budget, so a handful of owner reads against one store
// exhausts it; that is real production behaviour, not a test artefact.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/release"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

var injectedReleaseInstant = time.Unix(1_700_000_500, 0).UTC()

const (
	releaseTestRepository = "repo-under-observation"
	releaseTestEnvClass   = "preview-local"
)

func repoRoot() string { return filepath.Join("..", "..") }

func newReleaseService(t *testing.T) (*application.Service, *memory.Store) {
	t.Helper()
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.DetachReleaseObserver)
	return svc, st
}

// newObserver builds an observer over root with no fabricated capability
// evidence: the real Foundation contract's twelve baseline capabilities all
// carry no repository evidence references, and this surface does not invent any.
func newObserver(t *testing.T, root string, router *release.Router) *application.SourceReleaseObserver {
	t.Helper()
	id, err := domain.NewReleaseID("candidate-v2-066-surface")
	if err != nil {
		t.Fatal(err)
	}
	observer, err := application.NewSourceReleaseObserver(application.ReleaseObserverConfig{
		Root:             root,
		Repository:       releaseTestRepository,
		EnvironmentClass: releaseTestEnvClass,
		Router:           router,
		AssembledAt:      injectedReleaseInstant,
		Candidate: release.CandidateInput{
			ReleaseID: id, CandidateID: id, CandidateDigest: "candidate-v2-066-surface",
			Version: 1, Status: domain.ReleaseExercising,
			RollbackEvidence: false, ResumeEvidence: false,
			ExpectedControlRevision: 1, FencingToken: 1,
		},
	})
	if err != nil {
		t.Fatalf("construct the release observer: %v", err)
	}
	return observer
}

// --- gating and the not-configured answer ----------------------------------

func TestReleaseStateRequiresTheOwnerRole(t *testing.T) {
	svc, _ := newReleaseService(t)
	if err := svc.AttachReleaseObserver(newObserver(t, repoRoot(), nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReleaseState(context.Background()); err == nil {
		t.Fatal("an unauthenticated caller read the release surface")
	}
	if _, err := svc.ReleaseState(runner(context.Background(), "runner-1")); err == nil {
		t.Fatal("a runner read the owner-only release surface")
	}
}

// TestReleaseStateWithoutAnObserverIsDistinguishableAndReportsNoVersion is
// what the route maps to 503: a process with no explicitly configured release
// source root reports no version at all.
func TestReleaseStateWithoutAnObserverIsDistinguishableAndReportsNoVersion(t *testing.T) {
	svc, _ := newReleaseService(t)
	view, err := svc.ReleaseState(owner(context.Background()))
	if !errors.Is(err, application.ErrReleaseObserverNotConfigured) {
		t.Fatalf("err = %v, want ErrReleaseObserverNotConfigured", err)
	}
	if view.ReleaseVersion != "" || view.Candidate.BundleDigest != "" || len(view.Conditions) != 0 {
		t.Fatalf("the not-configured answer reported release state anyway: %+v", view)
	}
}

func TestAttachReleaseObserverRefusesNilAndAConflictingObserver(t *testing.T) {
	svc, _ := newReleaseService(t)
	if err := svc.AttachReleaseObserver(nil); err == nil {
		t.Fatal("a nil observer was attached")
	}
	first := newObserver(t, repoRoot(), nil)
	if err := svc.AttachReleaseObserver(first); err != nil {
		t.Fatal(err)
	}
	if err := svc.AttachReleaseObserver(first); err != nil {
		t.Fatalf("re-attaching the same observer must be idempotent: %v", err)
	}
	if err := svc.AttachReleaseObserver(newObserver(t, repoRoot(), nil)); err == nil {
		t.Fatal("a second, different observer replaced the first silently")
	}
}

func TestNewSourceReleaseObserverRequiresExplicitConfiguration(t *testing.T) {
	base := application.ReleaseObserverConfig{Root: repoRoot(), Repository: releaseTestRepository, EnvironmentClass: releaseTestEnvClass, AssembledAt: injectedReleaseInstant}
	for name, mutate := range map[string]func(*application.ReleaseObserverConfig){
		"no root":              func(c *application.ReleaseObserverConfig) { c.Root = "   " },
		"no repository":        func(c *application.ReleaseObserverConfig) { c.Repository = "" },
		"no environment class": func(c *application.ReleaseObserverConfig) { c.EnvironmentClass = "" },
		"no assembly instant":  func(c *application.ReleaseObserverConfig) { c.AssembledAt = time.Time{} },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := application.NewSourceReleaseObserver(cfg); err == nil {
			t.Fatalf("%s: an observer was constructed without it", name)
		}
	}
}

// --- the eight conditions, tri-state ---------------------------------------

func TestReleaseStateReportsTheEightConditionsTriStateAndIsNotPromotable(t *testing.T) {
	svc, _ := newReleaseService(t)
	if err := svc.AttachReleaseObserver(newObserver(t, repoRoot(), nil)); err != nil {
		t.Fatal(err)
	}
	view, err := svc.ReleaseState(owner(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	wantConditions := len(release.AllConditionIDs())
	if wantConditions == 0 {
		t.Fatal("the release package declares zero conditions; this assertion must not pass vacuously")
	}
	if len(view.Conditions) != wantConditions {
		t.Fatalf("view carries %d conditions, want %d", len(view.Conditions), wantConditions)
	}
	states := map[string]int{}
	for _, c := range view.Conditions {
		switch c.State {
		case string(release.ConditionMet), string(release.ConditionUnmet), string(release.ConditionNotObservableHere):
		default:
			t.Fatalf("condition %q has state %q", c.ID, c.State)
		}
		states[c.State]++
		if strings.TrimSpace(c.Reason) == "" {
			t.Fatalf("condition %q carries no reason", c.ID)
		}
		if len(c.DecidedBy) == 0 {
			t.Fatalf("condition %q names no deciding source", c.ID)
		}
		if strings.TrimSpace(c.ContractText) == "" {
			t.Fatalf("condition %q carries no contract text", c.ID)
		}
	}
	if states[string(release.ConditionNotObservableHere)] != 2 {
		t.Fatalf("%d conditions are not-observable-here, want 2 (conditions 1 and 5)", states[string(release.ConditionNotObservableHere)])
	}
	if view.Promotable {
		t.Fatal("the surface claims promotable while two conditions are not observable here and twelve capabilities have no evidence")
	}
	if view.ReleaseVersion == "" || view.Candidate.BundleDigest == "" || view.Candidate.ContractDigest == "" || view.Candidate.DocsDigest == "" {
		t.Fatalf("the surface reports no assembled version: %+v", view)
	}
	if view.AssembledAt != injectedReleaseInstant.Format(time.RFC3339Nano) {
		t.Fatalf("assembled_at = %q, want the injected instant %q", view.AssembledAt, injectedReleaseInstant.Format(time.RFC3339Nano))
	}
	if len(view.CapabilitiesWithoutEvidence) != len(view.DeclaredCapabilities) {
		t.Fatalf("%d of %d declared capabilities have no evidence; the real contract records evidence for none", len(view.CapabilitiesWithoutEvidence), len(view.DeclaredCapabilities))
	}
	// A refusal must be readable as its own kind, not as one collapsed message.
	kinds := map[string]int{}
	for _, c := range view.Conditions {
		for _, rj := range c.Refusals {
			kinds[rj.Kind]++
		}
	}
	for _, want := range []string{"capability-evidence-missing", "rollback-evidence-missing", "resume-evidence-missing"} {
		if kinds[want] == 0 {
			t.Fatalf("the surface reports no refusal of kind %q: %v", want, kinds)
		}
	}
	t.Logf("release %s: %d conditions (%v), %d capabilities without evidence, promotable=%v", view.ReleaseVersion, len(view.Conditions), states, len(view.CapabilitiesWithoutEvidence), view.Promotable)
}

// TestReleaseStateNamesTheD1ExclusionsAndTheResidualGaps is A9/A15 at the
// serialised boundary.
func TestReleaseStateNamesTheD1ExclusionsAndTheResidualGaps(t *testing.T) {
	svc, _ := newReleaseService(t)
	if err := svc.AttachReleaseObserver(newObserver(t, repoRoot(), nil)); err != nil {
		t.Fatal(err)
	}
	view, err := svc.ReleaseState(owner(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if view.EnvironmentClass != releaseTestEnvClass {
		t.Fatalf("environment_class = %q, want %q", view.EnvironmentClass, releaseTestEnvClass)
	}
	if len(view.NotObserved) == 0 {
		t.Fatal("not_observed is empty")
	}
	for _, want := range []string{"cloud-run-running-revision", "deployed-image-digest", "deploy-path", "iap-authentication-boundary", "scale-to-zero"} {
		found := false
		for _, got := range view.NotObserved {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("not_observed %v does not name %q", view.NotObserved, want)
		}
	}
	if len(view.ResidualGaps) != 2 {
		t.Fatalf("residual_gaps has %d entries, want 2", len(view.ResidualGaps))
	}
	joined := strings.ToLower(strings.Join(view.ResidualGaps, "\n"))
	for _, forbidden := range []string{"capability exercised", "preview journey passed"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("residual_gaps contains the forbidden claim %q", forbidden)
		}
	}
	if !strings.Contains(view.VersionSource, "assembled") {
		t.Fatalf("version_source does not state where the version came from: %q", view.VersionSource)
	}
	// Nothing in the payload may assert a Cloud Run revision or a deployed
	// image as a measured fact: those names appear only inside not_observed.
	for _, forbidden := range []string{"revision", "image"} {
		if strings.Contains(strings.ToLower(view.VersionSource), forbidden) && !strings.Contains(strings.ToLower(view.VersionSource), "no cloud run revision") {
			t.Fatalf("version_source appears to assert something about %q: %q", forbidden, view.VersionSource)
		}
	}
}

// --- no per-request tree walk ----------------------------------------------

// copyReleaseTree copies every assembled bundle member of src into a fresh
// directory, so a test can delete the copy without touching the repository.
func copyReleaseTree(t *testing.T, src string) string {
	t.Helper()
	assembled, err := release.AssembleFromRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.MkdirTemp("", "v2-066-release-tree")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dst) })
	for _, m := range assembled.Members {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(m.Path)))
		if err != nil {
			t.Fatal(err)
		}
		full := filepath.Join(dst, filepath.FromSlash(m.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// TestReleaseStateDoesNotWalkTheSourceTreePerRequest proves the snapshot is
// cached by deleting the source tree after construction: a per-request walk
// would fail or change its answer, a cached snapshot does neither.
func TestReleaseStateDoesNotWalkTheSourceTreePerRequest(t *testing.T) {
	root := copyReleaseTree(t, repoRoot())
	svc, _ := newReleaseService(t)
	if err := svc.AttachReleaseObserver(newObserver(t, root, nil)); err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	before, err := svc.ReleaseState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(root); statErr == nil {
		t.Fatal("the source tree still exists; the fixture is vacuous")
	}
	after, err := svc.ReleaseState(ctx)
	if err != nil {
		t.Fatalf("the read failed once the tree was gone, so it walks the tree per request: %v", err)
	}
	if after.Candidate.BundleDigest != before.Candidate.BundleDigest || after.ReleaseVersion != before.ReleaseVersion || after.AssembledAt != before.AssembledAt {
		t.Fatal("the second read differs from the first, so the snapshot is not the one assembled at construction")
	}
	if len(after.Conditions) != len(before.Conditions) {
		t.Fatal("the condition set changed between reads")
	}
}

// --- route, rollback target and bounded retention --------------------------

func TestReleaseStateReportsNoRouteRecordedWhenTheProcessRecordedNone(t *testing.T) {
	svc, _ := newReleaseService(t)
	if err := svc.AttachReleaseObserver(newObserver(t, repoRoot(), release.NewRouter())); err != nil {
		t.Fatal(err)
	}
	view, err := svc.ReleaseState(owner(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if view.Route.Recorded {
		t.Fatalf("a route was reported for a process that recorded none: %+v", view.Route)
	}
	if !strings.Contains(view.Route.Note, "no route recorded") {
		t.Fatalf("route note does not say no route was recorded: %q", view.Route.Note)
	}
	if view.Route.PreviewDigest != "" || view.Route.StableDigest != "" || view.Route.RollbackTargetDigest != "" || view.Route.RollbackAvailable {
		t.Fatalf("an unrecorded route was reported with inferred values: %+v", view.Route)
	}
	if !strings.Contains(view.Route.Source, "this process's own recorded route") {
		t.Fatalf("route source does not state whose route it is: %q", view.Route.Source)
	}
	if len(view.RollbackHistory) != 0 {
		t.Fatalf("rollback history is non-empty for a process that never rolled back: %+v", view.RollbackHistory)
	}
}

// TestReleaseStateRollbackTargetIsMonotonicAndRetentionScanIsBounded is A8:
// after one rollback the target is empty and rollback-available is false, a
// second consecutive rollback is refused, and the Requirement scan behind the
// target is bounded and reports how many it examined.
func TestReleaseStateRollbackTargetIsMonotonicAndRetentionScanIsBounded(t *testing.T) {
	svc, _ := newReleaseService(t)
	router := release.NewRouter()
	const first, second = "digest-stable-1", "digest-stable-2"
	for _, digest := range []string{first, second} {
		if err := router.SetPreview(releaseTestRepository, digest); err != nil {
			t.Fatal(err)
		}
		if err := router.Promote(releaseTestRepository, digest); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AttachReleaseObserver(newObserver(t, repoRoot(), router)); err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())

	// A Requirement whose StableSnapshot references the rollback target must
	// be found by the bounded scan. domain.StableReleaseSnapshot records a
	// release id and a bundle digest, so the reference is written on the
	// release-id field, which is what the route's digest is compared against.
	if _, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "req-1", RequirementID: "req-1", Text: "referencing requirement"}); err != nil {
		t.Fatal(err)
	}

	view, err := svc.ReleaseState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Route.Recorded {
		t.Fatal("the recorded route was not reported")
	}
	if view.Route.StableDigest != second {
		t.Fatalf("stable digest = %q, want %q", view.Route.StableDigest, second)
	}
	if view.Route.RollbackTargetDigest != first {
		t.Fatalf("rollback target = %q, want %q", view.Route.RollbackTargetDigest, first)
	}
	if !view.Route.RollbackAvailable {
		t.Fatal("rollback is reported unavailable while a previous stable route exists")
	}
	if !view.Retention.ScanBounded || view.Retention.PageSize != application.MaxPageSize {
		t.Fatalf("retention scan is not reported as bounded: %+v", view.Retention)
	}
	if view.Retention.RequirementsExamined != 1 {
		t.Fatalf("retention examined %d Requirements, want 1", view.Retention.RequirementsExamined)
	}
	if len(view.Retention.TargetComparedAgainst) != 2 {
		t.Fatalf("retention does not name what the target was compared against: %+v", view.Retention)
	}

	// One rollback: the target is cleared as the route moves (monotonic), so
	// the surface must report no target and rollback unavailable.
	record, err := router.RollbackWithReason(releaseTestRepository, "preview regression", injectedReleaseInstant)
	if err != nil {
		t.Fatal(err)
	}
	if record.From != second || record.To != first {
		t.Fatalf("rollback record = %+v", record)
	}
	view, err = svc.ReleaseState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Route.RollbackTargetDigest != "" || view.Route.RollbackAvailable {
		t.Fatalf("the rollback target survived a rollback: %+v", view.Route)
	}
	if view.Route.StableDigest != first {
		t.Fatalf("stable digest after rollback = %q, want %q", view.Route.StableDigest, first)
	}
	if len(view.RollbackHistory) != 1 {
		t.Fatalf("rollback history has %d entries, want 1", len(view.RollbackHistory))
	}
	h := view.RollbackHistory[0]
	if h.Repository != releaseTestRepository || h.From != second || h.To != first || h.Reason != "preview regression" || h.At != injectedReleaseInstant.Format(time.RFC3339Nano) {
		t.Fatalf("rollback history entry = %+v", h)
	}

	// A second consecutive rollback is refused: moving forward again requires
	// SetPreview plus a full gated Promote.
	if _, err := router.RollbackWithReason(releaseTestRepository, "again", injectedReleaseInstant); err == nil {
		t.Fatal("a second consecutive rollback was accepted")
	}
}

// TestRetentionFindsARequirementThatStillReferencesTheRollbackTarget uses a
// Requirement carrying a real StableSnapshot, so the scan's positive answer is
// measured and not only its negative one.
func TestRetentionFindsARequirementThatStillReferencesTheRollbackTarget(t *testing.T) {
	svc, st := newReleaseService(t)
	router := release.NewRouter()
	const target, current = "digest-target", "digest-current"
	for _, digest := range []string{target, current} {
		if err := router.SetPreview(releaseTestRepository, digest); err != nil {
			t.Fatal(err)
		}
		if err := router.Promote(releaseTestRepository, digest); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AttachReleaseObserver(newObserver(t, repoRoot(), router)); err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirement(ctx, domain.Requirement{
			ID: "req-referencing", Status: domain.RequirementCompleted, Version: 1,
			StableSnapshot: domain.StableReleaseSnapshot{ReleaseID: domain.ReleaseID(target), ReleaseVersion: 1, BundleDigest: "bundle", EvidenceDigest: "evidence"},
		}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	view, err := svc.ReleaseState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Route.RollbackTargetDigest != target {
		t.Fatalf("rollback target = %q, want %q", view.Route.RollbackTargetDigest, target)
	}
	if !view.Retention.TargetReferenced {
		t.Fatalf("the scan did not find the referencing Requirement: %+v", view.Retention)
	}
	if len(view.Retention.ReferencingRequirementIDs) != 1 || view.Retention.ReferencingRequirementIDs[0] != "req-referencing" {
		t.Fatalf("referencing ids = %v", view.Retention.ReferencingRequirementIDs)
	}
	if view.Retention.RequirementsExamined != 1 {
		t.Fatalf("examined %d Requirements, want 1", view.Retention.RequirementsExamined)
	}
}

// TestReleaseStateSerialisesExactlyTheDeclaredResponseShape pins the
// serialised top-level key set of GET /v1/release/state against the list
// declared in contracts/openapi/openapi-v1.yaml's ReleaseStateResponse
// (additionalProperties false, every key required). A field added or renamed
// here without the contract, or the other way round, fails this test.
func TestReleaseStateSerialisesExactlyTheDeclaredResponseShape(t *testing.T) {
	svc, _ := newReleaseService(t)
	if err := svc.AttachReleaseObserver(newObserver(t, repoRoot(), release.NewRouter())); err != nil {
		t.Fatal(err)
	}
	view, err := svc.ReleaseState(owner(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schema_version", "environment_class", "release_version", "candidate",
		"assembled_at", "version_source", "conditions", "promotable",
		"declared_capabilities", "capabilities_without_evidence", "route",
		"rollback_history", "retention", "not_observed", "residual_gaps",
	}
	if len(got) != len(want) {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("response has %d top-level keys %v, want %d %v", len(got), keys, len(want), want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("response is missing the declared key %q: %s", k, raw)
		}
	}
	// No credential-shaped or provider-output field may appear anywhere in the
	// serialised surface.
	lowered := strings.ToLower(string(raw))
	for _, forbidden := range []string{"\"token\"", "\"secret\"", "\"credential\"", "\"password\"", "\"stdout\"", "\"stderr\"", "\"raw_"} {
		if strings.Contains(lowered, forbidden) {
			t.Fatalf("the serialised release surface carries a forbidden field %s", forbidden)
		}
	}
}
