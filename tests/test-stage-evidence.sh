#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TEST_ROOT=$(mktemp -d)
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_ROOT/bin" "$TEST_ROOT/state"
cat > "$TEST_ROOT/bin/codex" <<'EOF'
#!/usr/bin/env bash
printf 'provider startup diagnostic\n' >&2
exit 23
EOF
chmod +x "$TEST_ROOT/bin/codex"
PATH="$TEST_ROOT/bin:$PATH"

source "$ROOT/bin/lib/agentic-loop/agent.sh"
STATE_ROOT="$TEST_ROOT/state"
config_value() { :; }
agent_stage_max_cost_usd() { :; }
agent_usage_from_codex_sessions() { :; }

result_file="$TEST_ROOT/state/issue-270-plan.txt"
usage_file="$TEST_ROOT/state/issue-270-plan-usage.txt"
if agent_run_stage plan "$TEST_ROOT" "$TEST_ROOT" "$TEST_ROOT" "$result_file" "$usage_file" prompt pool codex model effort; then
  exit 1
fi

evidence_dir="$TEST_ROOT/state/stage-evidence"
[[ $(find "$evidence_dir" -type f | wc -l) -eq 2 ]]
grep -Fq 'provider startup diagnostic' "$evidence_dir"/*.stderr
grep -Eq '^provider_exit=23$' "$usage_file"
grep -Eq '^duration_ms=[0-9]+$' "$usage_file"
grep -Eq '^stdout_bytes=0$' "$usage_file"
grep -Eq '^stderr_bytes=[1-9][0-9]*$' "$usage_file"

for i in $(seq 1 25); do
  agent_run_stage plan "$TEST_ROOT" "$TEST_ROOT" "$TEST_ROOT" "$TEST_ROOT/state/issue-270-plan-$i.txt" "$TEST_ROOT/state/issue-270-plan-$i-usage.txt" prompt pool codex model effort || true
done
[[ $(find "$evidence_dir" -type f | wc -l) -le 40 ]]
printf 'stage evidence tests passed.\n'
