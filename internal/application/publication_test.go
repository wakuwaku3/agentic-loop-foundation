package application_test

// Tests for the publication command, the pre-write target read and the
// write-once Observation write (V2-072). Every assertion is deterministic:
// no sleep, no wall-clock timer and no goroutine, and every instant comes
// from the injected clock the fixture already uses.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// publishFixture is the smallest graph a publication needs: a registered
// Repository, a Requirement linked to it, a prepared Increment, and a claimed
// Execution owned by one Runner.
type publishFixture struct {
	service     *application.Service
	store       *memory.Store
	ownerCtx    context.Context
	runnerCtx   context.Context
	runnerID    string
	requirement string
	repository  string
	increment   string
	execution   string
	lease       string
	fence       domain.FencingToken
	version     domain.Version
}

const (
	publishBase = "1111111111111111111111111111111111111111"
	publishHead = "2222222222222222222222222222222222222222"
	publishTree = "3333333333333333333333333333333333333333"
)

func newPublishFixture(t *testing.T, tag string) *publishFixture {
	t.Helper()
	s, st := service()
	ctx := owner(context.Background())
	now := clock{}.Now()
	repository, err := s.RegisterRepository(ctx, application.RegisterRepositoryRequest{RequestID: tag + ":register", SourceURL: "https://github.com/Owner/Name.git", DefaultBranch: "v2"})
	if err != nil {
		t.Fatalf("RegisterRepository: %v", err)
	}
	captured, err := s.Capture(ctx, application.CaptureRequest{RequestID: tag + ":capture", Text: "publish me", RepositoryID: repository.RepositoryID})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// V2-089: a claim is refused unless the parent Requirement is in a
	// status that admits work. This fixture claims, so the parent is moved
	// to domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- and the Plan below carries
	// the POST-seed version, because the seed bumps the Requirement's
	// Version and a dropped or zeroed ExpectedRequirementVersion would
	// delete a real assertion.
	readyVersion := seedRequirementStatus(t, st, captured.RequirementID, domain.RequirementReady)
	planned, err := s.Plan(ctx, application.PlanRequest{RequestID: tag + ":plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		increment, _, e := u.Increment(ctx, planned.IncrementID)
		if e != nil {
			return e
		}
		actor, _ := domain.NewActorID("a")
		next, e := domain.DecideIncrement(increment, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: actor, At: now, ExpectedVersion: increment.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, next, increment.Version)
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	runnerID := tag + "-runner"
	claim, err := s.Claim(runner(ctx, runnerID), application.ClaimRequest{RequestID: tag + ":claim", IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var version domain.Version
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		execution, _, e := u.Execution(ctx, claim.ExecutionID)
		if e != nil {
			return e
		}
		version = execution.Version
		return nil
	}); err != nil {
		t.Fatalf("read execution: %v", err)
	}
	return &publishFixture{
		service: s, store: st, ownerCtx: ctx, runnerCtx: runner(ctx, runnerID), runnerID: runnerID,
		requirement: captured.RequirementID, repository: repository.RepositoryID,
		increment: planned.IncrementID, execution: claim.ExecutionID, lease: claim.LeaseID,
		fence: claim.FencingToken, version: version,
	}
}

func (f *publishFixture) request(tag string) application.PublishChangeRequest {
	return application.PublishChangeRequest{
		RequestID:                tag,
		ExecutionID:              f.execution,
		LeaseID:                  f.lease,
		ExpectedExecutionVersion: f.version,
		FencingToken:             f.fence,
		BaseBranch:               "v2",
		BaseCommit:               publishBase,
		HeadCommit:               publishHead,
		HeadTree:                 publishTree,
		ChangedPaths:             2,
	}
}

func TestPublishChangeCreatesTheIntentAndTheOutboxItemInOneTransaction(t *testing.T) {
	f := newPublishFixture(t, "one-transaction")
	before := len(f.store.Outbox())
	out, err := f.service.PublishChange(f.runnerCtx, f.request("publish-1"))
	if err != nil {
		t.Fatalf("PublishChange: %v", err)
	}
	if out.OperationID == "" || out.OutboxID == "" {
		t.Fatalf("response = %#v", out)
	}
	if out.RepositoryID != f.repository {
		t.Fatalf("the response names repository %q, not the registered %q", out.RepositoryID, f.repository)
	}
	wantRef, err := domain.PublicationRefName(domain.IncrementID(f.increment), domain.ExecutionID(f.execution))
	if err != nil {
		t.Fatal(err)
	}
	if out.Ref != wantRef {
		t.Fatalf("ref = %q, want %q", out.Ref, wantRef)
	}
	if !strings.HasPrefix(out.Ref, domain.PublicationRefPrefix) {
		t.Fatalf("ref %q is outside the reserved prefix", out.Ref)
	}
	items := f.store.Outbox()
	if len(items) != before+1 {
		t.Fatalf("outbox grew by %d, want exactly 1", len(items)-before)
	}
	item := items[len(items)-1]
	if item.Kind != application.PublicationOutboxKind {
		t.Fatalf("outbox kind = %q", item.Kind)
	}
	if item.OperationID != out.OperationID {
		t.Fatalf("outbox operation %q != response operation %q", item.OperationID, out.OperationID)
	}
	if item.FencingToken != f.fence || item.ExpectedVersion == 0 || item.IncrementID != f.increment {
		t.Fatalf("outbox item = %#v", item)
	}
	var intent domain.PublicationIntent
	if err = json.Unmarshal(item.Payload, &intent); err != nil {
		t.Fatalf("the payload is not a publication intent: %v", err)
	}
	if err = domain.ValidatePublicationIntent(intent); err != nil {
		t.Fatalf("the payload is not a valid intent: %v", err)
	}
	if intent.RepositoryID.String() != f.repository || intent.Locator.Owner != "owner" || intent.Locator.Name != "name" {
		t.Fatalf("intent = %#v", intent)
	}
	if intent.HeadTree != publishTree || intent.BaseCommit != publishBase || intent.ChangedPaths != 2 {
		t.Fatalf("intent = %#v", intent)
	}
	// The event is recorded in the same transaction as the Outbox Item.
	events := f.store.Events()
	if len(events) == 0 || events[len(events)-1].Type != "execution.publication-requested" {
		t.Fatalf("last event = %#v", events[len(events)-1])
	}
	// Idempotent replay: the same request id returns the same response and
	// adds no second Outbox Item.
	again, err := f.service.PublishChange(f.runnerCtx, f.request("publish-1"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again != out {
		t.Fatalf("replay changed the response: %#v vs %#v", again, out)
	}
	if len(f.store.Outbox()) != before+1 {
		t.Fatalf("the replay created a second outbox item")
	}
	// A differing fingerprint on the same request id is a conflict.
	conflicting := f.request("publish-1")
	conflicting.ChangedPaths = 3
	if _, err = f.service.PublishChange(f.runnerCtx, conflicting); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay = %v, want ErrIdempotencyConflict", err)
	}
}

func TestPublishChangeRefusesEveryTargetThatIsNotARegisteredRepository(t *testing.T) {
	// A17's five refusals. Each must create no Outbox Item at all.
	t.Run("requirement_with_no_link", func(t *testing.T) {
		s, st := service()
		ctx := owner(context.Background())
		now := clock{}.Now()
		captured, err := s.Capture(ctx, application.CaptureRequest{RequestID: "nolink:capture", Text: "x"})
		if err != nil {
			t.Fatal(err)
		}
		// V2-089: a claim is refused unless the parent Requirement is in a
		// status that admits work. This fixture claims, so the parent is moved
		// to domain.RequirementReady -- '優先順位評価済みで実行可能',
		// docs/architecture/domain-model.md:265 -- and the Plan below carries
		// the POST-seed version, because the seed bumps the Requirement's
		// Version and a dropped or zeroed ExpectedRequirementVersion would
		// delete a real assertion.
		readyVersion := seedRequirementStatus(t, st, captured.RequirementID, domain.RequirementReady)
		planned, err := s.Plan(ctx, application.PlanRequest{RequestID: "nolink:plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: readyVersion})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.Transact(ctx, func(u application.UnitOfWork) error {
			increment, _, e := u.Increment(ctx, planned.IncrementID)
			if e != nil {
				return e
			}
			actor, _ := domain.NewActorID("a")
			next, e := domain.DecideIncrement(increment, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: actor, At: now, ExpectedVersion: increment.Version})
			if e != nil {
				return e
			}
			return u.SaveIncrement(ctx, next, increment.Version)
		}); err != nil {
			t.Fatal(err)
		}
		claim, err := s.Claim(runner(ctx, "nolink-runner"), application.ClaimRequest{RequestID: "nolink:claim", IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
		if err != nil {
			t.Fatal(err)
		}
		before := len(st.Outbox())
		_, err = s.PublishChange(runner(ctx, "nolink-runner"), application.PublishChangeRequest{
			RequestID: "nolink:publish", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID,
			ExpectedExecutionVersion: 1, FencingToken: claim.FencingToken,
			BaseBranch: "v2", BaseCommit: publishBase, HeadCommit: publishHead, HeadTree: publishTree, ChangedPaths: 1,
		})
		if !errors.Is(err, application.ErrRequirementHasNoRepository) {
			t.Fatalf("err = %v, want ErrRequirementHasNoRepository", err)
		}
		if len(st.Outbox()) != before {
			t.Fatal("a refused publication created an outbox item")
		}
	})
	t.Run("repository_retired", func(t *testing.T) {
		f := newPublishFixture(t, "retired")
		if _, err := f.service.RetireRepository(f.ownerCtx, application.RetireRepositoryRequest{RequestID: "retired:retire", RepositoryID: f.repository, ExpectedVersion: 1}); err != nil {
			t.Fatalf("RetireRepository: %v", err)
		}
		before := len(f.store.Outbox())
		if _, err := f.service.PublishChange(f.runnerCtx, f.request("retired:publish")); !errors.Is(err, application.ErrRepositoryNotAvailable) {
			t.Fatalf("err = %v, want ErrRepositoryNotAvailable", err)
		}
		if len(f.store.Outbox()) != before {
			t.Fatal("a refused publication created an outbox item")
		}
	})
	t.Run("repository_does_not_exist", func(t *testing.T) {
		// The link is write-once, so the only way this inconsistency can arise
		// is a link written before -- or without -- a registration. No request
		// field can name a coordinate, which is exactly the point: the store is
		// the only place a link comes from.
		s, st := service()
		ctx := owner(context.Background())
		now := clock{}.Now()
		captured, err := s.Capture(ctx, application.CaptureRequest{RequestID: "gone:capture", Text: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.Transact(ctx, func(u application.UnitOfWork) error {
			return u.SaveRequirementRepositoryLink(ctx, domain.RequirementRepositoryLink{
				RequirementID: domain.RequirementID(captured.RequirementID),
				RepositoryID:  "repository-that-was-never-registered",
				AssignedAt:    now,
			})
		}); err != nil {
			t.Fatalf("writing the dangling link: %v", err)
		}
		// V2-089: a claim is refused unless the parent Requirement is in a
		// status that admits work. This fixture claims, so the parent is moved
		// to domain.RequirementReady -- '優先順位評価済みで実行可能',
		// docs/architecture/domain-model.md:265 -- and the Plan below carries
		// the POST-seed version, because the seed bumps the Requirement's
		// Version and a dropped or zeroed ExpectedRequirementVersion would
		// delete a real assertion.
		readyVersion := seedRequirementStatus(t, st, captured.RequirementID, domain.RequirementReady)
		planned, err := s.Plan(ctx, application.PlanRequest{RequestID: "gone:plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: readyVersion})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.Transact(ctx, func(u application.UnitOfWork) error {
			increment, _, e := u.Increment(ctx, planned.IncrementID)
			if e != nil {
				return e
			}
			actor, _ := domain.NewActorID("a")
			next, e := domain.DecideIncrement(increment, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: actor, At: now, ExpectedVersion: increment.Version})
			if e != nil {
				return e
			}
			return u.SaveIncrement(ctx, next, increment.Version)
		}); err != nil {
			t.Fatal(err)
		}
		claim, err := s.Claim(runner(ctx, "gone-runner"), application.ClaimRequest{RequestID: "gone:claim", IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
		if err != nil {
			t.Fatal(err)
		}
		before := len(st.Outbox())
		_, err = s.PublishChange(runner(ctx, "gone-runner"), application.PublishChangeRequest{
			RequestID: "gone:publish", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID,
			ExpectedExecutionVersion: 1, FencingToken: claim.FencingToken,
			BaseBranch: "v2", BaseCommit: publishBase, HeadCommit: publishHead, HeadTree: publishTree, ChangedPaths: 1,
		})
		if !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound naming the unregistered repository", err)
		}
		if len(st.Outbox()) != before {
			t.Fatal("a refused publication created an outbox item")
		}
	})
	t.Run("execution_not_owned_by_the_calling_runner", func(t *testing.T) {
		f := newPublishFixture(t, "cross-runner")
		before := len(f.store.Outbox())
		if _, err := f.service.PublishChange(runner(f.ownerCtx, "another-runner"), f.request("cross:publish")); !errors.Is(err, domain.ErrLeaseNotOwned) {
			t.Fatalf("err = %v, want ErrLeaseNotOwned", err)
		}
		if len(f.store.Outbox()) != before {
			t.Fatal("a refused publication created an outbox item")
		}
	})
	t.Run("stale_fencing_token", func(t *testing.T) {
		f := newPublishFixture(t, "stale-fence")
		request := f.request("stale:publish")
		request.FencingToken = f.fence + 7
		before := len(f.store.Outbox())
		if _, err := f.service.PublishChange(f.runnerCtx, request); !errors.Is(err, domain.ErrStaleFence) {
			t.Fatalf("err = %v, want ErrStaleFence", err)
		}
		if len(f.store.Outbox()) != before {
			t.Fatal("a refused publication created an outbox item")
		}
	})
	t.Run("stale_execution_version", func(t *testing.T) {
		f := newPublishFixture(t, "stale-version")
		request := f.request("stale-version:publish")
		request.ExpectedExecutionVersion = f.version + 5
		if _, err := f.service.PublishChange(f.runnerCtx, request); !errors.Is(err, domain.ErrStaleVersion) {
			t.Fatalf("err = %v, want ErrStaleVersion", err)
		}
	})
}

func TestPublishChangeRequestCarriesNoForgeCoordinate(t *testing.T) {
	// A17: the coordinate is unrepresentable in the request, not merely
	// forbidden. Reading the struct's own field names is what makes that a
	// property rather than a review habit.
	request := application.PublishChangeRequest{}
	value := reflectPublishRequestFields(request)
	if len(value) == 0 {
		t.Fatal("no fields were read from PublishChangeRequest")
	}
	for _, name := range value {
		lowered := strings.ToLower(name)
		for _, forbidden := range []string{"owner", "locator", "url", "host", "forge", "repository", "coordinate", "remote"} {
			if strings.Contains(lowered, forbidden) {
				t.Fatalf("PublishChangeRequest.%s lets a caller name a coordinate", name)
			}
		}
	}
}

func TestPublicationTargetForOutboxRefusesADisagreeingPayload(t *testing.T) {
	f := newPublishFixture(t, "target-read")
	out, err := f.service.PublishChange(f.runnerCtx, f.request("target:publish"))
	if err != nil {
		t.Fatalf("PublishChange: %v", err)
	}
	target, found, err := f.service.PublicationTargetForOutbox(f.runnerCtx, out.OutboxID)
	if err != nil || !found {
		t.Fatalf("PublicationTargetForOutbox = %v, %v, %v", target, found, err)
	}
	if target.RepositoryID != f.repository || target.Owner != "owner" || target.Name != "name" {
		t.Fatalf("target = %#v", target)
	}
	if target.Ref != out.Ref || target.BaseCommit != publishBase {
		t.Fatalf("target = %#v", target)
	}
	items := f.store.Outbox()
	var intent domain.PublicationIntent
	if err = json.Unmarshal(items[len(items)-1].Payload, &intent); err != nil {
		t.Fatal(err)
	}
	if !target.Agrees(intent) {
		t.Fatal("the stored target disagrees with the stored payload")
	}
	// Each of the four fields A18 names, disagreed individually.
	for name, apply := range map[string]func(*domain.PublicationIntent){
		"repository":  func(i *domain.PublicationIntent) { i.RepositoryID = "another-repository" },
		"owner":       func(i *domain.PublicationIntent) { i.Locator.Owner = "someone-else" },
		"name":        func(i *domain.PublicationIntent) { i.Locator.Name = "another-name" },
		"base commit": func(i *domain.PublicationIntent) { i.BaseCommit = publishHead },
		"ref":         func(i *domain.PublicationIntent) { i.Ref = domain.PublicationRefPrefix + "other/other" },
	} {
		candidate := intent
		apply(&candidate)
		if target.Agrees(candidate) {
			t.Fatalf("a payload disagreeing on the %s was accepted", name)
		}
	}
	// An unknown outbox id is absent, not an error and not a fabricated target.
	if _, found, err = f.service.PublicationTargetForOutbox(f.runnerCtx, "no-such-outbox"); err != nil || found {
		t.Fatalf("unknown outbox = %v, %v", found, err)
	}
	// An outbox item that is not a publication is refused rather than read.
	for _, item := range items {
		if item.Kind == application.PublicationOutboxKind {
			continue
		}
		if _, _, err = f.service.PublicationTargetForOutbox(f.runnerCtx, item.ID); !errors.Is(err, application.ErrInvalidOutbox) {
			t.Fatalf("a %q outbox item was read as a publication: %v", item.Kind, err)
		}
		break
	}
	// The read is Runner-authenticated like every other Runner command.
	if _, _, err = f.service.PublicationTargetForOutbox(f.ownerCtx, out.OutboxID); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("an owner caller read the runner-side target: %v", err)
	}
}

func TestRecordPublicationIsWriteOncePerOperation(t *testing.T) {
	f := newPublishFixture(t, "record")
	out, err := f.service.PublishChange(f.runnerCtx, f.request("record:publish"))
	if err != nil {
		t.Fatalf("PublishChange: %v", err)
	}
	observation := domain.PublicationObservation{
		OperationID:     domain.OperationID(out.OperationID),
		RepositoryID:    domain.RepositoryID(f.repository),
		Ref:             out.Ref,
		PublishedCommit: "4444444444444444444444444444444444444444",
		PublishedTree:   publishTree,
		LocalCommit:     publishHead,
		LocalTree:       publishTree,
		TreesAgree:      true,
		State:           domain.PublicationPublishedAndObserved,
		Reason:          "the ref was created and all four content-addressed equalities held",
		ObservedAt:      clock{}.Now(),
	}
	if err = f.service.RecordPublication(f.runnerCtx, observation); err != nil {
		t.Fatalf("RecordPublication: %v", err)
	}
	if err = f.service.RecordPublication(f.runnerCtx, observation); err != nil {
		t.Fatalf("the identical re-write was not an idempotent replay: %v", err)
	}
	different := observation
	different.State = domain.PublicationConvergedOnExistingRef
	if err = f.service.RecordPublication(f.runnerCtx, different); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a differing re-write = %v, want domain.ErrStaleVersion", err)
	}
	stored, found, err := f.service.Publication(f.runnerCtx, out.OperationID)
	if err != nil || !found {
		t.Fatalf("Publication = %v, %v, %v", stored, found, err)
	}
	if stored != observation {
		t.Fatalf("stored = %#v, want %#v", stored, observation)
	}
	if _, found, err = f.service.Publication(f.runnerCtx, "no-such-operation"); err != nil || found {
		t.Fatalf("unknown operation = %v, %v", found, err)
	}
	// An invalid Observation never reaches the store.
	invalid := observation
	invalid.State = "completed"
	if err = f.service.RecordPublication(f.runnerCtx, invalid); err == nil {
		t.Fatal("an Observation with an unknown state was written")
	}
}

func TestConfirmedPublicationChangesNoRequirementAndNoIncrement(t *testing.T) {
	// A21(ii), the behavioural half of non_goal 2: after a confirmed
	// publication the Requirement's status and version are exactly what they
	// were, and no requirement event was recorded.
	f := newPublishFixture(t, "no-requirement-change")
	var beforeRequirement domain.Requirement
	var beforeIncrement domain.Increment
	if err := f.store.Transact(f.ownerCtx, func(u application.UnitOfWork) error {
		requirement, _, e := u.Requirement(f.ownerCtx, f.requirement)
		if e != nil {
			return e
		}
		beforeRequirement = requirement
		increment, _, e := u.Increment(f.ownerCtx, f.increment)
		if e != nil {
			return e
		}
		beforeIncrement = increment
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requirementEventsBefore := 0
	for _, event := range f.store.Events() {
		if event.AggregateType == "requirement" {
			requirementEventsBefore++
		}
	}
	out, err := f.service.PublishChange(f.runnerCtx, f.request("norc:publish"))
	if err != nil {
		t.Fatalf("PublishChange: %v", err)
	}
	if err = f.service.RecordPublication(f.runnerCtx, domain.PublicationObservation{
		OperationID: domain.OperationID(out.OperationID), RepositoryID: domain.RepositoryID(f.repository), Ref: out.Ref,
		PublishedCommit: "5555555555555555555555555555555555555555", PublishedTree: publishTree,
		LocalCommit: publishHead, LocalTree: publishTree, TreesAgree: true,
		State: domain.PublicationPublishedAndObserved, Reason: "confirmed", ObservedAt: clock{}.Now(),
	}); err != nil {
		t.Fatalf("RecordPublication: %v", err)
	}
	var afterRequirement domain.Requirement
	var afterIncrement domain.Increment
	if err = f.store.Transact(f.ownerCtx, func(u application.UnitOfWork) error {
		requirement, _, e := u.Requirement(f.ownerCtx, f.requirement)
		if e != nil {
			return e
		}
		afterRequirement = requirement
		increment, _, e := u.Increment(f.ownerCtx, f.increment)
		if e != nil {
			return e
		}
		afterIncrement = increment
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if afterRequirement.Status != beforeRequirement.Status || afterRequirement.Version != beforeRequirement.Version {
		t.Fatalf("the Requirement moved: %v/%d -> %v/%d", beforeRequirement.Status, beforeRequirement.Version, afterRequirement.Status, afterRequirement.Version)
	}
	if afterIncrement.Status != beforeIncrement.Status || afterIncrement.Version != beforeIncrement.Version {
		t.Fatalf("the Increment moved: %v/%d -> %v/%d", beforeIncrement.Status, beforeIncrement.Version, afterIncrement.Status, afterIncrement.Version)
	}
	requirementEventsAfter := 0
	for _, event := range f.store.Events() {
		if event.AggregateType == "requirement" {
			requirementEventsAfter++
		}
	}
	if requirementEventsAfter != requirementEventsBefore {
		t.Fatalf("requirement events %d -> %d; a publication records none", requirementEventsBefore, requirementEventsAfter)
	}
}

func TestAcceptResultOutboxItemIsUnchangedByThePayloadParameter(t *testing.T) {
	// A16: effectOutbox gained a payload parameter and every pre-existing call
	// site passes nil, so the item those call sites produce is exactly what it
	// was. Payload is the only field that could have moved; assert it is nil
	// and that the rest of the item is fully populated.
	f := newPublishFixture(t, "accept-unchanged")
	if err := f.store.Transact(f.ownerCtx, func(u application.UnitOfWork) error {
		execution, _, e := u.Execution(f.ownerCtx, f.execution)
		if e != nil {
			return e
		}
		execution.Status = domain.ExecutionRunning
		return u.SaveExecution(f.ownerCtx, execution, execution.Version)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.AcceptResult(f.runnerCtx, application.AcceptResultRequest{RequestID: "accept-unchanged:result", ExecutionID: f.execution, LeaseID: f.lease, ExpectedExecutionVersion: 1, FencingToken: f.fence, Succeeded: true}); err != nil {
		t.Fatalf("AcceptResult: %v", err)
	}
	items := f.store.Outbox()
	found := false
	for _, item := range items {
		if item.Kind != "result-accepted" {
			continue
		}
		found = true
		if item.Payload != nil {
			t.Fatalf("the result-accepted item carries a payload of %d bytes; the existing call site passes nil", len(item.Payload))
		}
		if item.OperationID == "" || item.ExpectedVersion == 0 || item.FencingToken == 0 || item.PermitKind == "" || item.IncrementID == "" || item.LeaseID == "" {
			t.Fatalf("result-accepted item = %#v", item)
		}
	}
	if !found {
		t.Fatal("no result-accepted outbox item was created")
	}
	// The claim-issued and lease-renewed items are the other two nil call
	// sites; assert the claim one the same way.
	for _, item := range items {
		if item.Kind == "claim-issued" && item.Payload != nil {
			t.Fatalf("the claim-issued item carries a payload of %d bytes", len(item.Payload))
		}
	}
}

func TestUndecidableSentinelIsTheOnlyAdditionToTheAmbiguityVocabulary(t *testing.T) {
	// A12: the new sentinel exists, it is exported, and it is recognised as
	// ambiguous by the same predicate the deadline and cancellation cases go
	// through. The predicate itself is unexported, so it is exercised through
	// the dispatcher's own failure path in the runner-side protocol test; here
	// the assertion is that the sentinel is a distinct, matchable error.
	if application.ErrEffectUndecidable == nil {
		t.Fatal("ErrEffectUndecidable is nil")
	}
	wrapped := errors.New("wrapped: " + application.ErrEffectUndecidable.Error())
	if errors.Is(wrapped, application.ErrEffectUndecidable) {
		t.Fatal("a merely textually similar error matches the sentinel")
	}
	if !errors.Is(errors.Join(application.ErrEffectUndecidable, context.Canceled), application.ErrEffectUndecidable) {
		t.Fatal("the sentinel does not match through errors.Join")
	}
	if strings.Contains(strings.ToLower(application.ErrEffectUndecidable.Error()), "timeout") {
		t.Fatal("the undecidable sentinel must not describe itself as a timeout")
	}
}

// reflectPublishRequestFields reads PublishChangeRequest's field names through
// the type system rather than by parsing a file, so the assertion holds for the
// compiled type a caller actually sees.
func reflectPublishRequestFields(value application.PublishChangeRequest) []string {
	t := reflect.TypeOf(value)
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		names = append(names, t.Field(i).Name)
	}
	return names
}
