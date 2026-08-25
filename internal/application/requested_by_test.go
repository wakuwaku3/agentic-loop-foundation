package application_test

import (
	"context"
	"errors"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// loop returns a context whose Caller represents the Loop deciding on its
// own, mirroring how owner()/runner() already model the other two
// authenticated principals in this package's tests.
func loop(ctx context.Context, subject string) context.Context {
	return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleScheduler, Subject: subject})
}

// TestCaptureRecordsOwnerRequestedBy proves an owner-issued Requirement
// intake is attributed to the owner, using the same authenticated-session
// subject callerActor already resolves for every other owner-only command
// (local dev session subject today, IAP subject in production).
func TestCaptureRecordsOwnerRequestedBy(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	out, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-owner", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestedBy.ActorType != domain.ActorTypeOwner || out.RequestedBy.Subject == "" {
		t.Fatalf("capture response requested_by = %+v, want owner with a non-empty subject", out.RequestedBy)
	}
	detail, ok, err := s.GetRequirementDetail(owner(context.Background()), out.RequirementID)
	if err != nil || !ok {
		t.Fatalf("detail lookup failed: ok=%v err=%v", ok, err)
	}
	if detail.RequestedBy == nil || detail.RequestedBy.ActorType != domain.ActorTypeOwner {
		t.Fatalf("stored requirement requested_by = %+v, want owner", detail.RequestedBy)
	}
}

// TestCaptureRecordsLoopRequestedBy proves that when the Loop itself is the
// origin (RoleScheduler), the record is distinguishable from an owner
// request: same operation, different actor_type, and the subject carries the
// Loop's own component identifier rather than an owner session subject.
func TestCaptureRecordsLoopRequestedBy(t *testing.T) {
	s, _ := service()
	out, err := s.Capture(loop(context.Background(), "reconciler.self-intake"), application.CaptureRequest{RequestID: "cap-loop", Text: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestedBy.ActorType != domain.ActorTypeLoop || out.RequestedBy.Subject != "reconciler.self-intake" {
		t.Fatalf("capture response requested_by = %+v, want loop/reconciler.self-intake", out.RequestedBy)
	}
	detail, ok, err := s.GetRequirementDetail(owner(context.Background()), out.RequirementID)
	if err != nil || !ok {
		t.Fatalf("detail lookup failed: ok=%v err=%v", ok, err)
	}
	if detail.RequestedBy == nil || detail.RequestedBy.ActorType != domain.ActorTypeLoop || detail.RequestedBy.Subject != "reconciler.self-intake" {
		t.Fatalf("stored requirement requested_by = %+v, want loop/reconciler.self-intake", detail.RequestedBy)
	}
}

// TestCaptureStillRejectsRunnerRole proves widening Capture to accept the
// Loop's own RoleScheduler did not also open it to RoleRunner: a claimed
// Runner session remains unable to intake Requirements.
func TestCaptureStillRejectsRunnerRole(t *testing.T) {
	s, _ := service()
	if _, err := s.Capture(runner(context.Background(), "r1"), application.CaptureRequest{RequestID: "cap-runner"}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("got %v, want ErrForbidden", err)
	}
}

// TestControlRecordsOwnerAndLoopRequestedBy proves both origins are
// distinguishable for Control Intents too. domain.ControlIntent itself
// (internal/domain/control.go) is unmodified M1-closed surface, so this also
// exercises the side-table path (SaveControlRequestedBy/ControlRequestedBy)
// that carries the value across the same transaction as SaveControl.
func TestControlRecordsOwnerAndLoopRequestedBy(t *testing.T) {
	s, _ := service()
	now := clock{}.Now()
	ownerOut, err := s.Control(owner(context.Background()), application.ControlRequest{RequestID: "ctl-owner", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlPauseIntake, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if ownerOut.RequestedBy.ActorType != domain.ActorTypeOwner {
		t.Fatalf("owner control requested_by = %+v", ownerOut.RequestedBy)
	}
	loopOut, err := s.Control(loop(context.Background(), "reconciler.runaway-guard"), application.ControlRequest{RequestID: "ctl-loop", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlAllow, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if loopOut.RequestedBy.ActorType != domain.ActorTypeLoop || loopOut.RequestedBy.Subject != "reconciler.runaway-guard" {
		t.Fatalf("loop control requested_by = %+v", loopOut.RequestedBy)
	}
	rows, err := s.ListControls(owner(context.Background()), 25)
	if err != nil {
		t.Fatal(err)
	}
	found := map[domain.Revision]*domain.RequestedBy{}
	for i := range rows {
		found[rows[i].Revision] = rows[i].RequestedBy
	}
	if rb := found[ownerOut.Revision]; rb == nil || rb.ActorType != domain.ActorTypeOwner {
		t.Fatalf("listed control revision %d requested_by = %+v, want owner", ownerOut.Revision, rb)
	}
	if rb := found[loopOut.Revision]; rb == nil || rb.ActorType != domain.ActorTypeLoop || rb.Subject != "reconciler.runaway-guard" {
		t.Fatalf("listed control revision %d requested_by = %+v, want loop/reconciler.runaway-guard", loopOut.Revision, rb)
	}
}

// TestLegacyRequirementWithoutRequestedByStillReads proves a pre-existing
// record saved before this field existed (RequestedBy is its Go zero value)
// remains valid and readable: Validate accepts it, and the owner read model
// omits requested_by entirely rather than surfacing an empty placeholder.
func TestLegacyRequirementWithoutRequestedByStillReads(t *testing.T) {
	s, st := service()
	ctx := context.Background()
	legacy := domain.Requirement{ID: domain.RequirementID("legacy-1"), Status: domain.RequirementCaptured, Version: 1}
	if err := domain.Validate(legacy); err != nil {
		t.Fatalf("legacy record (no requested_by) should validate: %v", err)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveRequirement(ctx, legacy, 0); err != nil {
			return err
		}
		return u.SaveRequirementText(ctx, "legacy-1", "pre-existing")
	}); err != nil {
		t.Fatal(err)
	}
	detail, ok, err := s.GetRequirementDetail(owner(ctx), "legacy-1")
	if err != nil || !ok {
		t.Fatalf("legacy requirement should still be readable: ok=%v err=%v", ok, err)
	}
	if detail.RequestedBy != nil {
		t.Fatalf("legacy requirement should have no requested_by, got %+v", detail.RequestedBy)
	}
	page, err := s.ListRequirementsPage(owner(ctx), "", 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Requirements {
		if r.RequirementID == "legacy-1" && r.RequestedBy != nil {
			t.Fatalf("legacy requirement in page should have no requested_by, got %+v", r.RequestedBy)
		}
	}
}

// TestRequestedByActorTypeIsAClosedEnum proves the domain layer rejects an
// actor_type outside {owner, loop} while continuing to accept the absent
// (legacy) value, using domain.Validate directly since RequestedBy's
// closed-enum guard lives in internal/domain/model.go.
func TestRequestedByActorTypeIsAClosedEnum(t *testing.T) {
	base := domain.Requirement{ID: domain.RequirementID("r-enum"), Status: domain.RequirementCaptured, Version: 1}

	base.RequestedBy = domain.RequestedBy{}
	if err := domain.Validate(base); err != nil {
		t.Fatalf("absent requested_by should validate: %v", err)
	}

	base.RequestedBy = domain.RequestedBy{ActorType: domain.ActorTypeOwner, Subject: "s"}
	if err := domain.Validate(base); err != nil {
		t.Fatalf("owner requested_by should validate: %v", err)
	}

	base.RequestedBy = domain.RequestedBy{ActorType: domain.ActorTypeLoop, Subject: "s"}
	if err := domain.Validate(base); err != nil {
		t.Fatalf("loop requested_by should validate: %v", err)
	}

	base.RequestedBy = domain.RequestedBy{ActorType: domain.ActorType("superuser"), Subject: "s"}
	if err := domain.Validate(base); err == nil {
		t.Fatal("an actor_type outside {owner, loop} should be rejected")
	}
}
