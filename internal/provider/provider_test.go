package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

func packet() provider.WorkPacket {
	return provider.WorkPacket{Version: provider.ContractVersion, RequirementID: "req-1", RequirementSummary: "fix the queue", IncrementID: "inc-1", Constraints: []string{"bounded"}}
}
func TestAdaptersBuildSafeArgvAndParseMetadata(t *testing.T) {
	for _, a := range []provider.Adapter{provider.CodexAdapter{}, provider.ClaudeAdapter{}, provider.OpenCodeAdapter{}} {
		inv, err := a.Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet()})
		if err != nil {
			t.Fatal(err)
		}
		if len(inv.Argv) < 3 || strings.Contains(string(inv.Stdin), "credential") {
			t.Fatalf("unsafe invocation %#v", inv)
		}
		r, err := a.Parse([]byte(`{"status":"completed","checkpoint":"cp-1","output":"provider conversation omitted","usage":{"input_tokens":2,"output_tokens":3}}`))
		if err != nil || !r.Succeeded || r.OutputDigest == "" || r.Usage.TotalTokens != 0 {
			t.Fatalf("result=%#v err=%v", r, err)
		}
	}
}
func TestFixturesRejectSecretsAndMalformed(t *testing.T) {
	a := provider.CodexAdapter{}
	if _, err := a.Parse([]byte(`{"status":"success","output":"Bearer abcdefghijklmnop"}`)); err == nil {
		t.Fatal("secret fixture accepted")
	}
	if _, err := a.Parse([]byte(`{"status":"success"`)); err == nil {
		t.Fatal("malformed fixture accepted")
	}
}
func TestFailureClassificationAndHandoff(t *testing.T) {
	if f := provider.ClassifyError(context.Canceled); f.Class != provider.FailureCancelled || f.Retryable {
		t.Fatalf("cancel=%#v", f)
	}
	if f := provider.ClassifyError(context.DeadlineExceeded); f.Class != provider.FailureTimeout || !f.Ambiguous {
		t.Fatalf("timeout=%#v", f)
	}
	h, err := provider.PrepareHandoff("codex", "claude", packet(), provider.Result{Provider: "codex", Failure: &provider.Failure{Class: provider.FailureQuota}, Usage: provider.Usage{InputTokens: 1}})
	if err != nil || h.ToProvider != "claude" || h.Packet.RequirementID != "req-1" {
		t.Fatalf("handoff=%#v err=%v", h, err)
	}
	if _, err := provider.PrepareHandoff("codex", "claude", provider.WorkPacket{RequirementID: "r", IncrementID: "i", RequirementSummary: "credential"}, provider.Result{Provider: "codex"}); !errors.Is(err, provider.ErrInvalidPacket) {
		t.Fatalf("unsafe packet err=%v", err)
	}
	_ = time.Second
}
