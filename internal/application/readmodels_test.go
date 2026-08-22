package application_test

import (
	"context"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

func TestRequirementPageUsesOpaqueBoundedCursor(t *testing.T) {
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	for i := 0; i < 3; i++ {
		if _, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "page-" + string(rune('a'+i)), RequirementID: "req-" + string(rune('a'+i)), Text: "original"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := svc.ListRequirementsPage(ctx, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Requirements) != 2 || first.NextCursor == "" {
		t.Fatalf("first=%#v", first)
	}
	if strings.Contains(first.NextCursor, "req-") {
		t.Fatalf("cursor leaked key: %q", first.NextCursor)
	}
	second, err := svc.ListRequirementsPage(ctx, first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Requirements) != 1 || second.NextCursor != "" {
		t.Fatalf("second=%#v", second)
	}
	if _, err := svc.ListRequirementsPage(ctx, "not-a-cursor", 2); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

func TestRequirementDetailIncludesExecutionAndNextAction(t *testing.T) {
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	if _, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "detail", RequirementID: "r-detail", Text: "original request"}); err != nil {
		t.Fatal(err)
	}
	inc, _ := domain.NewIncrementID("inc-detail")
	rid, _ := domain.NewRequirementID("r-detail")
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: rid, Status: domain.IncrementReady, Version: 1}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	v, ok, err := svc.GetRequirementDetail(ctx, "r-detail")
	if err != nil || !ok {
		t.Fatalf("detail %v %v", ok, err)
	}
	if v.OriginalText != "original request" || v.NextAction != "run next increment" || len(v.Increments) != 1 {
		t.Fatalf("detail=%#v", v)
	}
}

func TestControlReadModelDoesNotInferObservation(t *testing.T) {
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	if _, err := svc.Control(ctx, application.ControlRequest{RequestID: "control-read", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "i"}, Mode: domain.ControlPauseIntake}); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListControls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Requested || rows[0].Acknowledged || rows[0].Effective || rows[0].Verified {
		t.Fatalf("progress=%#v", rows)
	}
	if _, err := svc.Heartbeat(application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner", RunnerID: "r"}), application.HeartbeatRequest{RequestID: "observe", ControlRevision: rows[0].Revision}); err != nil {
		t.Fatal(err)
	}
	rows, err = svc.ListControls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].Acknowledged || rows[0].Effective || rows[0].Verified {
		t.Fatalf("ack progress=%#v", rows)
	}
}
