#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly RUN_ROOT="$(mktemp -d)"
readonly ALL_GROUPS=(queue lifecycle auxiliary upgrade)
declare -a TEST_GROUPS=("${ALL_GROUPS[@]}")
report_file=''

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

declare -A pids=() starts=()

cleanup() {
  local pid
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$RUN_ROOT"
}
trap cleanup EXIT INT TERM

for group in "${TEST_GROUPS[@]}"; do
  starts[$group]=$SECONDS
  AGENTIC_LOOP_TEST_GROUP="$group" "$PROJECT_ROOT/tests/test-agentic-loop.sh" >"$RUN_ROOT/$group.log" 2>&1 &
  pids[$group]=$!
done

failed=0
: > "$RUN_ROOT/report.tsv"
for group in "${TEST_GROUPS[@]}"; do
  if wait "${pids[$group]}"; then
    result=passed
    printf 'E2E group passed: %s\n' "$group"
  else
    result=failed
    printf 'E2E group failed: %s\n' "$group" >&2
    sed -n '1,240p' "$RUN_ROOT/$group.log" >&2
    failed=1
  fi
  printf '%s\t%s\t%s\n' "$group" "$result" "$(( SECONDS - starts[$group] ))" >> "$RUN_ROOT/report.tsv"
done

[[ -z $report_file ]] || cp "$RUN_ROOT/report.tsv" "$report_file"

(( failed == 0 )) || exit 1
printf 'All E2E groups passed.\n'
