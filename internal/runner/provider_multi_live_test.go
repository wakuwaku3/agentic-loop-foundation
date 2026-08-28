package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

const multiProviderLiveGate = "AGENTIC_LOOP_LIVE_MULTI_PROVIDER"

func runInvocation(ctx context.Context, invocationRunner InvocationRunner, adapter provider.Adapter, req provider.Request) ([]byte, provider.Result, error) {
	if err := req.Validate(); err != nil {
		return nil, provider.Result{}, err
	}
	inv, err := adapter.Build(req)
	if err != nil {
		return nil, provider.Result{}, err
	}
	raw, err := invocationRunner.Run(ctx, inv)
	if err != nil {
		return raw, provider.Result{}, err
	}
	result, err := adapter.Parse(raw)
	return raw, result, err
}

func TestCodexAndOpenCodeLiveContract(t *testing.T) {
	if os.Getenv(multiProviderLiveGate) != "1" {
		t.Skip("Codex/OpenCode live contract is disabled")
	}
	cases := []struct {
		name, version, executable string
		adapter                   provider.Adapter
	}{
		{"codex", "0.149.1", "codex", provider.CodexAdapter{}},
		{"opencode", "1.18.18", "opencode", provider.OpenCodeAdapter{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executable, err := exec.LookPath(tc.executable)
			if err != nil {
				t.Fatal(err)
			}
			executable, err = filepath.Abs(executable)
			if err != nil {
				t.Fatal(err)
			}
			ledgerPath := filepath.Join(t.TempDir(), "cost.json")
			policy, err := NewInvocationPolicy(tc.name, executable, ledgerPath, CostLimits{MaxInvocations: 1, MaxTotalCostUSD: 10, WorstCaseReservationUSD: 1}, []string{"HOME", "PATH"})
			if err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			if err := os.Chmod(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			log, err := NewBoundedLog(t.TempDir(), tc.name, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			runner := SupervisedInvocationRunner{
				Supervisor: ProcessSupervisor{TermGrace: 3 * time.Second, Confine: &NamespaceConfinement{Workspace: workspace}},
				Log:        log, Policy: policy, Purpose: "live-" + tc.name,
				Ledger: &CostLedger{Path: ledgerPath, Provider: tc.name, TaskID: "live-session"},
			}
			packet := provider.WorkPacket{Version: provider.ContractVersion, RequirementID: "req-v2-028-" + tc.name, IncrementID: "inc-v2-028-" + tc.name, RequirementSummary: "Return a short acknowledgement without using tools or reading files."}
			request := provider.Request{OperationID: "op-v2-028-" + tc.name, Workspace: workspace, Packet: packet, CLIVersionDeclared: tc.version}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			raw, result, err := runInvocation(ctx, runner, tc.adapter, request)
			if err != nil {
				t.Fatalf("live invocation: %v", err)
			}
			if !result.Succeeded || result.OutputDigest == "" || !strings.HasPrefix(result.Checkpoint, tc.name+":") {
				t.Fatalf("projected result is incomplete: succeeded=%v checkpoint_prefix=%v output_digest_present=%v", result.Succeeded, strings.HasPrefix(result.Checkpoint, tc.name+":"), result.OutputDigest != "")
			}
			if len(raw) == 0 || strings.Contains(string(raw), "acknowledgement") {
				t.Fatal("projection is empty or contains response text instead of its digest")
			}
			snapshot, err := runner.Ledger.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.InvocationCount == 0 || snapshot.Halted {
				t.Fatalf("ledger state is not usable: invocations=%d halted=%v", snapshot.InvocationCount, snapshot.Halted)
			}
			t.Logf("provider=%s version=%s session_id_present=%v usage_reported=%v invocation_count=%d", tc.name, tc.version, snapshot.Entries[len(snapshot.Entries)-1].SessionID != "", result.UsageReported, snapshot.InvocationCount)
		})
	}
}
