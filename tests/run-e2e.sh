#!/usr/bin/env bash
# flaky検知・限定的隔離のretryオーケストレーション（Issue #60, ADR 0022）。
#
# testは常に実行する。retryは検知・診断目的に限定し、`passed`への昇格は行わ
# ない: attempt1（全群並列のco-run文脈）が失敗した群だけを、最大2回、単独
# （isolated文脈）で追加試行し、scripts/flaky.sh classifyへ委ねてverdictと
# 終了codeを決める。隔離（quarantine）が一致しない限りverdict=flakyは常に
# 非ゼロで終了するため、merge gateは弱まらない。除外・skip用のoptionは持た
# ない（docs/policies/validation-harness.md、docs/decisions/0022参照）。
set -euo pipefail

readonly PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly RUN_ROOT="$(mktemp -d)"
readonly ALL_GROUPS=(queue lifecycle auxiliary upgrade)
declare -a TEST_GROUPS=("${ALL_GROUPS[@]}")
report_file=''
runner="$PROJECT_ROOT/tests/test-agentic-loop.sh"
attempts_max=3
registry_path="$PROJECT_ROOT/tests/flaky-registry.toml"
record_path="$PROJECT_ROOT/tmp/flaky-last.json"
readonly FLAKY_SH="$PROJECT_ROOT/scripts/flaky.sh"

usage() {
  cat >&2 <<'EOF'
使い方:
  tests/run-e2e.sh [--groups g1,g2] [--report PATH]
                    [--runner PATH] [--attempts N] [--registry PATH]
                    [--record PATH|--no-record]

除外・skip用のoptionは存在しない（flakyや過去の失敗を理由にした除外を許さないため）。
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --groups)
      [[ $# -ge 2 ]] || { printf 'run-e2e.sh: --groups requires a value\n' >&2; exit 2; }
      IFS=',' read -ra TEST_GROUPS <<< "${2//[[:space:]]/}"
      shift 2
      ;;
    --report)
      [[ $# -ge 2 ]] || { printf 'run-e2e.sh: --report requires a value\n' >&2; exit 2; }
      report_file=$2
      shift 2
      ;;
    --runner)
      [[ $# -ge 2 ]] || { printf 'run-e2e.sh: --runner requires a value\n' >&2; exit 2; }
      runner=$2
      shift 2
      ;;
    --attempts)
      [[ $# -ge 2 ]] || { printf 'run-e2e.sh: --attempts requires a value\n' >&2; exit 2; }
      attempts_max=$2
      shift 2
      ;;
    --registry)
      [[ $# -ge 2 ]] || { printf 'run-e2e.sh: --registry requires a value\n' >&2; exit 2; }
      registry_path=$2
      shift 2
      ;;
    --record)
      [[ $# -ge 2 ]] || { printf 'run-e2e.sh: --record requires a value\n' >&2; exit 2; }
      record_path=$2
      shift 2
      ;;
    --no-record)
      record_path=''
      shift
      ;;
    -h | --help)
      usage; exit 0
      ;;
    *)
      printf 'run-e2e.sh: unknown argument: %s\n' "$1" >&2
      exit 2
      ;;
  esac
done

(( ${#TEST_GROUPS[@]} > 0 )) || { printf 'run-e2e.sh: no groups selected\n' >&2; exit 2; }
for group in "${TEST_GROUPS[@]}"; do
  case " ${ALL_GROUPS[*]} " in
    *" $group "*) ;;
    *) printf 'run-e2e.sh: unknown group: %s\n' "$group" >&2; exit 2 ;;
  esac
done
[[ $attempts_max =~ ^[1-3]$ ]] || { printf 'run-e2e.sh: --attempts must be 1, 2, or 3\n' >&2; exit 2; }
[[ -x $runner ]] || { printf 'run-e2e.sh: --runner is not executable: %s\n' "$runner" >&2; exit 2; }

# A per-run seed, exported for any fixture that wants a reproducible random
# draw (see docs/operations/flaky-tests.md); never used by run-e2e.sh itself
# to alter retry/quarantine decisions.
export AGENTIC_LOOP_TEST_SEED="${AGENTIC_LOOP_TEST_SEED:-$$-$RANDOM}"

declare -A pids=()
cleanup() {
  local pid
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$RUN_ROOT"
}
trap cleanup EXIT INT TERM

run_attempt() {
  local group=$1 log=$2 start elapsed rc=0
  start=$SECONDS
  if AGENTIC_LOOP_TEST_GROUP="$group" "$runner" >"$log" 2>&1; then rc=0; else rc=1; fi
  elapsed=$(( SECONDS - start ))
  printf '%s\n' "$elapsed"
  return $rc
}

declare -A attempt1_exit=() attempt1_seconds=() attempt2_exit=() attempt2_seconds=() attempt3_exit=() attempt3_seconds=()

# --- attempt 1: co-run (all selected groups in parallel, as before) --------
for group in "${TEST_GROUPS[@]}"; do
  AGENTIC_LOOP_TEST_GROUP="$group" "$runner" >"$RUN_ROOT/$group.attempt1.log" 2>&1 &
  pids[$group]=$!
done
declare -A starts=()
for group in "${TEST_GROUPS[@]}"; do starts[$group]=$SECONDS; done
for group in "${TEST_GROUPS[@]}"; do
  if wait "${pids[$group]}"; then attempt1_exit[$group]=0; else attempt1_exit[$group]=1; fi
  attempt1_seconds[$group]=$(( SECONDS - starts[$group] ))
done
pids=()

declare -a failed_groups=()
for group in "${TEST_GROUPS[@]}"; do
  [[ ${attempt1_exit[$group]} == 0 ]] || failed_groups+=("$group")
done

# --- attempt 2: isolated re-run of attempt1 failures only (sequential, so
# no other group's process shares the machine at the same time) ------------
if (( attempts_max >= 2 )); then
  for group in "${failed_groups[@]}"; do
    attempt2_seconds[$group]=$(run_attempt "$group" "$RUN_ROOT/$group.attempt2.log") && attempt2_exit[$group]=0 || attempt2_exit[$group]=1
  done
fi

# --- attempt 3: confirmation re-run, isolated, only where attempt2 passed -
if (( attempts_max >= 3 )); then
  for group in "${failed_groups[@]}"; do
    [[ ${attempt2_exit[$group]:-} == 0 ]] || continue
    attempt3_seconds[$group]=$(run_attempt "$group" "$RUN_ROOT/$group.attempt3.log") && attempt3_exit[$group]=0 || attempt3_exit[$group]=1
  done
fi

# --- classify every selected group and decide the final pass/fail ---------
overall_failed=0
: > "$RUN_ROOT/report.tsv"
declare -a verdict_json=()

for group in "${TEST_GROUPS[@]}"; do
  unit="e2e:$group"
  declare -a classify_args=(classify --unit "$unit" --registry "$registry_path")
  classify_args+=(--attempt "co-run:${attempt1_exit[$group]}:$RUN_ROOT/$group.attempt1.log")
  total_seconds=${attempt1_seconds[$group]}
  if [[ -n ${attempt2_exit[$group]:-} ]]; then
    classify_args+=(--attempt "isolated:${attempt2_exit[$group]}:$RUN_ROOT/$group.attempt2.log")
    total_seconds=$(( total_seconds + attempt2_seconds[$group] ))
  fi
  if [[ -n ${attempt3_exit[$group]:-} ]]; then
    classify_args+=(--attempt "isolated:${attempt3_exit[$group]}:$RUN_ROOT/$group.attempt3.log")
    total_seconds=$(( total_seconds + attempt3_seconds[$group] ))
  fi

  classify_out=$("$FLAKY_SH" "${classify_args[@]}" 2>>"$RUN_ROOT/$group.classify.stderr") && classify_rc=0 || classify_rc=$?
  # scripts/flaky.sh classify prints exactly one JSON line on stdout followed
  # by any human-readable disclosure lines; the first line is always the record.
  verdict_line=$(head -n1 <<< "$classify_out")
  verdict_json+=("$verdict_line")
  tail -n +2 <<< "$classify_out"
  cat "$RUN_ROOT/$group.classify.stderr" >&2 2>/dev/null || true

  if (( classify_rc == 0 )); then
    result=passed
    printf 'E2E group passed: %s\n' "$group"
  else
    result=failed
    printf 'E2E group failed: %s\n' "$group" >&2
    sed -n '1,240p' "$RUN_ROOT/$group.attempt1.log" >&2
    overall_failed=1
  fi
  printf '%s\t%s\t%s\n' "$group" "$result" "$total_seconds" >> "$RUN_ROOT/report.tsv"
done

[[ -z $report_file ]] || cp "$RUN_ROOT/report.tsv" "$report_file"

json_escape() {
  local value=$1
  value=${value//\\/\\\\}; value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}
  printf '%s' "$value"
}

if [[ -n $record_path ]]; then
  mkdir -p "$(dirname "$record_path")"
  commit=$(git -C "$PROJECT_ROOT" rev-parse HEAD 2>/dev/null || printf unknown)
  env_marker=$(command -v yq >/dev/null 2>&1 && yq -p json -r '.env.DEV_ENVIRONMENT // ""' "$PROJECT_ROOT/devbox.json" 2>/dev/null || true)
  [[ -n $env_marker ]] || env_marker=unknown
  lock_sha256=unknown
  [[ -r $PROJECT_ROOT/devbox.lock ]] && lock_sha256=$(sha256sum "$PROJECT_ROOT/devbox.lock" | cut -d' ' -f1)
  tz=${TZ:-$(date +%Z 2>/dev/null || true)}
  locale=${LANG:-${LC_ALL:-unknown}}
  nproc_count=$(nproc 2>/dev/null || printf unknown)
  uname_sr=$(uname -sr 2>/dev/null || printf unknown)
  fake_gh=$([[ -n ${FAKE_GH_ROOT:-} ]] && printf true || printf false)
  verdicts_json=$(IFS=,; printf '%s' "${verdict_json[*]:-}")
  {
    printf '{"schema":1,"commit":"%s","env_marker":"%s","devbox_lock_sha256":"%s","tz":"%s","locale":"%s","nproc":"%s","uname":"%s","seed":"%s","runner":"%s","fake_gh":%s,"verdicts":[%s]}\n' \
      "$(json_escape "$commit")" "$(json_escape "$env_marker")" "$(json_escape "$lock_sha256")" "$(json_escape "$tz")" "$(json_escape "$locale")" \
      "$(json_escape "$nproc_count")" "$(json_escape "$uname_sr")" "$(json_escape "$AGENTIC_LOOP_TEST_SEED")" "$(json_escape "$runner")" "$fake_gh" "$verdicts_json"
  } > "$record_path"
fi

(( overall_failed == 0 )) || exit 1
printf 'All E2E groups passed.\n'
