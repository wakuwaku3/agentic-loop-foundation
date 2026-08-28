package runner

import (
	"encoding/json"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

func TestProjectCodexJSONLResult(t *testing.T) {
	raw := []byte("{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}\n" +
		"{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}\n" +
		"{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":12,\"cached_input_tokens\":3,\"output_tokens\":5}}\n")
	projected, outcome, err := projectRealCLIResult("codex", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertProjectedResult(t, projected, "codex:thread-1", provider.DigestOutput("done"), 12, 5)
	if outcome.CacheReadCount != 3 {
		t.Fatalf("cache read = %d", outcome.CacheReadCount)
	}
}

func TestProjectOpenCodeJSONLResult(t *testing.T) {
	raw := []byte("{\"type\":\"step_start\",\"sessionID\":\"session-1\",\"part\":{\"type\":\"step-start\"}}\n" +
		"{\"type\":\"text\",\"sessionID\":\"session-1\",\"part\":{\"type\":\"text\",\"text\":\"done\"}}\n" +
		"{\"type\":\"step_finish\",\"sessionID\":\"session-1\",\"part\":{\"type\":\"step-finish\",\"cost\":0.01,\"tokens\":{\"input\":7,\"output\":4,\"reasoning\":2,\"cache\":{\"read\":1,\"write\":3}}}}\n")
	projected, outcome, err := projectRealCLIResult("opencode", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertProjectedResult(t, projected, "opencode:session-1", provider.DigestOutput("done"), 7, 6)
	if outcome.TotalCostUSD != 0.01 || outcome.CacheReadCount != 1 || outcome.CacheCreationCount != 3 {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func assertProjectedResult(t *testing.T, raw []byte, checkpoint, output string, input, outputTokens int64) {
	t.Helper()
	var got struct {
		Status     string         `json:"status"`
		Checkpoint string         `json:"checkpoint"`
		Output     string         `json:"output"`
		Usage      provider.Usage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" || got.Checkpoint != checkpoint || got.Output != output || got.Usage.InputTokens != input || got.Usage.OutputTokens != outputTokens {
		t.Fatalf("projection = %#v", got)
	}
}
