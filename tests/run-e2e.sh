#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly RUN_ROOT="$(mktemp -d)"
readonly TEST_GROUPS=(queue lifecycle auxiliary upgrade)
declare -A pids=()

cleanup() {
  local pid
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$RUN_ROOT"
}
trap cleanup EXIT INT TERM

for group in "${TEST_GROUPS[@]}"; do
  AGENTIC_LOOP_TEST_GROUP="$group" "$PROJECT_ROOT/tests/test-agentic-loop.sh" >"$RUN_ROOT/$group.log" 2>&1 &
  pids[$group]=$!
done

failed=0
for group in "${TEST_GROUPS[@]}"; do
  if wait "${pids[$group]}"; then
    printf 'E2E group passed: %s\n' "$group"
  else
    printf 'E2E group failed: %s\n' "$group" >&2
    sed -n '1,240p' "$RUN_ROOT/$group.log" >&2
    failed=1
  fi
done

(( failed == 0 )) || exit 1
printf 'All E2E groups passed.\n'
