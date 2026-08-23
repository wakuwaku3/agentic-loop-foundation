#!/usr/bin/env bash
# shellcheck disable=SC2155,SC2209,SC2016,SC2030,SC2031
set -euo pipefail

readonly PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
readonly FAKE_BIN="$TEST_ROOT/bin"
readonly FAKE_GH_ROOT="$TEST_ROOT/gh"
readonly TEST_HOST_PATH="$PATH"
readonly TEST_GROUP="${AGENTIC_LOOP_TEST_GROUP:-all}"
trap 'rm -rf "$TEST_ROOT"' EXIT

case "$TEST_GROUP" in all|queue|lifecycle|auxiliary|upgrade) ;; *) printf 'Unknown test group: %s\n' "$TEST_GROUP" >&2; exit 2 ;; esac

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
assert_contains() { grep -Fq -- "$2" "$1" || fail "$3"; }

# Wait-for-condition helpers (Issue #327): E2E assertions about supervisor/
# worker state can't use a fixed sleep budget, because the state transition
# being awaited (e.g. a poll that forks/execs a fake gh and fake provider many
# times) takes CPU-bound wall-clock time that a busy CI runner can inflate far
# past what a quiet workstation needs. All non-negative-assertion waits below
# poll on a wall-clock deadline instead of a fixed iteration count, so a slow
# machine gets more polls rather than a premature "did not happen" failure.
# scripts/lint.sh forbids reintroducing fixed-iteration-count `seq`-based wait
# loops in this file; use wait_for/wait_gone/wait_polls instead.
readonly WAIT_TIMEOUT_SECONDS=60

# wait_for DESC [--timeout N] CMD... -> poll CMD (a command or function name,
# args allowed) until it exits 0 or the deadline elapses. Runs in the current
# shell (no subshell), so a predicate function that assigns a variable leaves
# that assignment visible to the caller. Returns 0/1; never calls fail itself,
# so callers keep their own cleanup-then-fail (e.g. killing a supervisor pid)
# on timeout. On timeout, diagnostics go to stderr *before* any caller's fail
# message, so scripts/flaky.sh's sha256-of-first-line fingerprint (see
# docs/operations/flaky-tests.md) stays keyed on the unchanged `fail` text.
wait_for() {
  local desc=$1 budget=$WAIT_TIMEOUT_SECONDS
  shift
  if [[ ${1:-} == --timeout ]]; then budget=$2; shift 2; fi
  local start=$EPOCHSECONDS elapsed=0 interval
  while :; do
    if "$@" >/dev/null 2>&1; then
      [[ -z ${AGENTIC_LOOP_TEST_WAIT_TRACE:-} ]] || printf 'wait: %s %ds\n' "$desc" "$elapsed" >&2
      return 0
    fi
    elapsed=$(( EPOCHSECONDS - start ))
    (( elapsed < budget )) || break
    interval=0.1; (( elapsed < 5 )) || interval=0.5
    sleep "$interval"
  done
  {
    printf 'timed out after %ds waiting for: %s\n' "$budget" "$desc"
    printf 'predicate: %s\n' "$*"
    "$@" || true
    printf -- '--- state ---\n'; tail -n 20 "${state:-/dev/null}" 2>/dev/null || true
    printf -- '--- workers ---\n'; ls -l "${state_root:-}/workers" 2>/dev/null || true
    printf -- '--- poll-interval ---\n'; cat "${state_root:-}/poll-interval" 2>/dev/null || true
    printf -- '--- recent logs ---\n'; tail -n 20 "${state_root:-}"/logs/issue-*.log 2>/dev/null || true
    printf -- '--- load ---\n'; nproc 2>/dev/null || true; cat /proc/loadavg 2>/dev/null || true
  } >&2
  return 1
}

# wait_gone DESC PID [--timeout N] -> poll until PID no longer answers `kill -0`.
__wait_gone_check() { ! kill -0 "$1" 2>/dev/null; }
wait_gone() {
  local desc=$1 pid=$2
  shift 2
  wait_for "$desc" "$@" __wait_gone_check "$pid"
}

# wait_polls DESC [N=2] -> wait for N full supervisor poll cycles to complete,
# using the poll-interval marker supervisor.sh writes at the tail of every
# poll (after any claim attempt). Deleting it and waiting for it to reappear N
# times (default 2, since the first reappearance may belong to a poll that was
# already underway before the delete) proves N complete poll bodies ran,
# without relying on a fixed sleep to "probably" cover that many polls.
wait_polls() {
  local desc=$1 n=${2:-2} i
  for (( i = 0; i < n; i++ )); do
    rm -f "$state_root/poll-interval"
    wait_for "$desc (poll $((i + 1))/$n)" test -e "$state_root/poll-interval" || return 1
  done
  return 0
}

# --- wait_for/wait_gone/wait_polls self-test (Issue #327), runs in every group ---
{
  __selftest_flag="$TEST_ROOT/selftest-wait-flag"
  rm -f "$__selftest_flag"
  ( sleep 0.3; : > "$__selftest_flag" ) &
  __selftest_bg=$!
  __selftest_flag_check() { [[ -e $__selftest_flag ]]; }
  wait_for 'self-test: delayed condition' --timeout 5 __selftest_flag_check \
    || fail 'wait_for self-test: a condition satisfied after a short delay was not observed within its budget'
  wait "$__selftest_bg" 2>/dev/null || true

  __selftest_capture=''
  __selftest_capture_check() { __selftest_capture=ok; return 0; }
  wait_for 'self-test: predicate side effect visibility' --timeout 5 __selftest_capture_check \
    || fail 'wait_for self-test: predicate did not run'
  [[ $__selftest_capture == ok ]] || fail 'wait_for self-test: a predicate function'"'"'s variable assignment did not persist to the caller (it ran in a subshell)'

  __selftest_never() { return 1; }
  if wait_for 'self-test: condition that never becomes true' --timeout 1 __selftest_never; then
    fail 'wait_for self-test: a predicate that never succeeds returned success'
  fi

  setsid sleep 60 & __selftest_alive_pid=$!
  if wait_gone 'self-test: alive pid falsely reported gone' "$__selftest_alive_pid" --timeout 1; then
    fail 'wait_gone self-test: a live pid was reported as gone'
  fi
  kill -TERM "-$__selftest_alive_pid" 2>/dev/null || true
  wait "$__selftest_alive_pid" 2>/dev/null || true

  ( exit 0 ) & __selftest_gone_pid=$!
  wait "$__selftest_gone_pid" 2>/dev/null || true
  wait_gone 'self-test: exited pid observed as gone' "$__selftest_gone_pid" --timeout 5 \
    || fail 'wait_gone self-test: an already-exited pid was not observed as gone'
}

# write_queue_config FILE KEY=VAL ... -> render a [queue] TOML config. A
# purely numeric VAL is emitted bare (existing numeric queue.* keys); any
# other VAL (e.g. TRACEABILITY=require) is TOML-quoted as a string, since
# bare unquoted words are not valid TOML and yq would reject the whole file.
write_queue_config() {
  local file=$1 kv key val
  shift
  printf '[queue]\n' > "$file"
  for kv in "$@"; do
    key=${kv%%=*}
    val=${kv#*=}
    if [[ $val =~ ^[0-9]+$ ]]; then
      printf '%s = %s\n' "${key,,}" "$val" >> "$file"
    else
      printf '%s = "%s"\n' "${key,,}" "$val" >> "$file"
    fi
  done
}

# scope_field ARGS -> base64 of an agentic-loop:scope marker for state column 8
# (the fake gh's simulated Issue body), e.g. scope_field 'paths=bin/agentic-loop'
scope_field() { printf '<!-- agentic-loop:scope %s -->' "$1" | base64 -w0; }

# dependency_field REFS -> base64 of a "Blocked by:" body line for state column
# 8, e.g. dependency_field '#12, #34'
dependency_field() { printf 'Blocked by: %s' "$1" | base64 -w0; }

# criteria_body FIELDS... -> base64 of an Issue body with a 受け入れ条件
# heading and one bullet per argument, for trace.sh's trace_derive_criteria
# (state column 8, see docs/decisions/0017-requirement-traceability.md).
criteria_body() {
  local body=$'## 受け入れ条件\n'
  local c
  for c in "$@"; do body+="- $c"$'\n'; done
  printf '%s' "$body" | base64 -w0
}

# criterion_id TEXT -> the ac-XXXXXXXX id trace.sh's trace_criterion_id
# derives for a "- TEXT" bullet produced by criteria_body. trace_normalize_
# criterion's final `tr -s '[:space:]' ' ' <<< "$text"` here-string appends an
# implicit trailing newline that tr then folds into a single trailing space
# (command substitution only strips trailing newlines, not that space), so
# every normalized criterion trace.sh hashes carries one trailing space; this
# must reproduce that exactly or the derived and hand-built ids will never match.
criterion_id() { printf 'ac-%s' "$(printf '%s ' "$1" | sha256sum | cut -c1-8)"; }

# trace_change_json PATH [ANCHOR] -> a {"path":...} JSON object for one
# element of a trace_criterion_json "changes" argument.
trace_change_json() {
  if [[ -n ${2:-} ]]; then printf '{"path":"%s","anchor":"%s"}' "$1" "$2"
  else printf '{"path":"%s"}' "$1"; fi
}

# trace_check_json NAME RESULT -> a {"name":...,"result":...} JSON object
# for one element of a trace_criterion_json "checks" argument.
trace_check_json() { printf '{"name":"%s","result":"%s"}' "$1" "$2"; }

# trace_criterion_json ID SOURCE STATUS [VERIFICATION] [REASON] [SUPERSEDED_BY] [CHANGES_JSON] [CHECKS_JSON]
# -> one "criteria[]" element of an agentic-loop:traceability record.
# CHANGES_JSON/CHECKS_JSON are already comma-joined trace_change_json/
# trace_check_json outputs, e.g. "$(trace_change_json a.sh),$(trace_change_json b.sh)".
trace_criterion_json() {
  local id=$1 source=$2 status=$3 verification=${4:-} reason=${5:-} superseded_by=${6:-} changes=${7:-} checks=${8:-}
  local out="{\"id\":\"$id\",\"source\":\"$source\",\"status\":\"$status\""
  [[ -z $verification ]] || out+=",\"verification\":\"$verification\""
  [[ -z $reason ]] || out+=",\"reason\":\"$reason\""
  [[ -z $superseded_by ]] || out+=",\"superseded_by\":\"$superseded_by\""
  out+=",\"changes\":[$changes],\"checks\":[$checks]}"
  printf '%s' "$out"
}

# trace_record_json ISSUE CRITERION_JSON... -> the full agentic-loop:
# traceability record object (schema=1) wrapping the given criteria.
trace_record_json() {
  local issue=$1 joined='' c
  shift
  for c in "$@"; do joined+="${joined:+,}$c"; done
  printf '{"schema":1,"issue":%s,"criteria":[%s]}' "$issue" "$joined"
}

# trace_pr_body RECORD_JSON -> a PR body wrapping RECORD_JSON in the fenced
# ```agentic-loop:traceability code block trace.sh's trace_manifest_from_pr_body
# parses.
# shellcheck disable=SC2016 # Backticks are literal Markdown fencing in the fabricated PR body.
trace_pr_body() { printf '## Summary\n\n```agentic-loop:traceability\n%s\n```\n' "$1"; }

# preflight_risk_json AXIS LEVEL [REASON] [MISSING] -> one "risks[]" element of
# an agentic-loop:preflight record (see docs/decisions/0020-change-risk-preflight.md).
preflight_risk_json() {
  local axis=$1 level=$2 reason=${3:-} missing=${4:-}
  local out="{\"axis\":\"$axis\",\"level\":\"$level\""
  [[ -z $reason ]] || out+=",\"reason\":\"$reason\""
  [[ -z $missing ]] || out+=",\"missing\":\"$missing\""
  out+='}'
  printf '%s' "$out"
}

# preflight_risks_json [OVERRIDE_AXIS OVERRIDE_JSON] -> the full 10-axis
# "risks[]" array, every axis "low" except the given override (already built
# by preflight_risk_json).
preflight_risks_json() {
  local override_axis=${1:-} override_json=${2:-} axis joined='' sep=''
  for axis in security confidentiality integrity availability data_migration external_environment cost compatibility release_deploy rollback; do
    if [[ $axis == "$override_axis" ]]; then joined+="$sep$override_json"; else joined+="$sep$(preflight_risk_json "$axis" low)"; fi
    sep=','
  done
  printf '[%s]' "$joined"
}

# preflight_record_json ISSUE RISKS_JSON [CHANGE_JSON] [APPROVAL_JSON] -> the
# full agentic-loop:preflight record object (schema=1).
preflight_record_json() {
  local issue=$1 risks=$2 change=${3:-'{"scope":"","tests":[],"external_operations":[],"rollback":""}'} approval=${4:-'{"required":false,"triggers":[]}'}
  printf '{"schema":1,"issue":%s,"risks":%s,"change":%s,"approval":%s}' "$issue" "$risks" "$change" "$approval"
}

# preflight_plan_body RECORD_JSON -> a plan-stage output (FAKE_CODEX_RESULT)
# embedding RECORD_JSON in the fenced code block preflight_manifest_from_plan
# parses, with an unrelated scope marker (README.md only; never a protected
# path) so tests control the capability-manifest signal independently. The
# fake codex/claude/opencode harness reuses this same value as the exec
# stage's response whenever no per-call FAKE_CODEX_EXEC_RESULT_<n> override is
# set, so the terminal marker is appended as the last line (mirrors the
# existing scope-refine fixture's FAKE_CODEX_RESULT=$'<!-- ... -->\nAGENTIC_LOOP_RESULT=completed'
# convention): harmless for fixtures where the preflight gate never lets exec
# run, and required for the ones where it does.
# shellcheck disable=SC2016 # Backticks are literal Markdown fencing in the fabricated plan body.
preflight_plan_body() { printf '## 計画\n\n```agentic-loop:preflight\n%s\n```\n\n<!-- agentic-loop:scope paths=README.md -->\nAGENTIC_LOOP_RESULT=completed\n' "$1"; }

# preflight_envelope_token_for ISSUE RECORD_JSON SIGNAL_CLASS SIGNAL_REASON ->
# the 12-hex approval envelope token a real worker (preflight_gate or
# preflight_reevaluate_diff) would compute for RECORD_JSON's declared parts
# (pass "" for no record) combined with a given signal (see preflight.sh's
# preflight_envelope_token). Pass SIGNAL_CLASS=none SIGNAL_REASON='' for a
# declared-risk-only envelope, or RECORD_JSON='' SIGNAL_CLASS=approval for a
# record-absent, signal-derived one.
preflight_envelope_token_for() { ( source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"; source "$PROJECT_ROOT/bin/lib/agentic-loop/preflight.sh"; preflight_envelope_token "$1" "$(preflight_declared_parts "$2")" "$3" "$4" ); }

# workload_unit_json OPERATION PER_UNIT GROWTH STOP_CONDITION REUSE -> one
# "units[]" element of an agentic-loop:workload record (see
# docs/decisions/0025-resource-scalability-budget.md).
workload_unit_json() { printf '{"operation":"%s","per_unit":"%s","growth":"%s","stop_condition":"%s","reuse":"%s"}' "$1" "$2" "$3" "$4" "$5"; }

# workload_record_json ISSUE EXTERNAL_IO [UNITS_JSON] [VERIFICATION_JSON] ->
# the full agentic-loop:workload record object (schema=1).
workload_record_json() {
  local issue=$1 external_io=$2 units=${3:-[]} verification=${4:-[]}
  printf '{"schema":1,"issue":%s,"external_io":"%s","units":%s,"verification":%s,"exceptions":[]}' "$issue" "$external_io" "$units" "$verification"
}

# workload_plan_body RECORD_JSON -> a plan-stage output (FAKE_CODEX_RESULT)
# embedding RECORD_JSON in the fenced code block workload_manifest_from_plan
# parses, mirroring preflight_plan_body's conventions (see there).
# shellcheck disable=SC2016 # Backticks are literal Markdown fencing in the fabricated plan body.
workload_plan_body() { printf '## 計画\n\n```agentic-loop:workload\n%s\n```\n\n<!-- agentic-loop:scope paths=README.md -->\nAGENTIC_LOOP_RESULT=completed\n' "$1"; }

mkdir -p "$FAKE_BIN" "$FAKE_GH_ROOT"

# External CLI contract lint: detect known-invalid forms while allowing
# typed flags, body files, literal @ text, and Markdown examples.
cli_contract_fixture="$TEST_ROOT/cli-contracts.sh"
cat > "$cli_contract_fixture" <<'EOF'
yq --arg k a '.children[]'
gh issue create --body "@/tmp/issue_body.md"
gh api -f count=3
gh issue create --body-file body.txt
printf '%s\n' 'email@example.test and @path are literal text'
```sh
gh issue create --body "@path"
```
gh issue create --body="@x.md"
gh pr comment 1 -b "@x.md"
gh issue comment 1 --body "こんにちは、これは正当な本文です"
EOF
cli_contract_output=$("$PROJECT_ROOT/scripts/lint-cli-contracts.sh" "$cli_contract_fixture")
grep -Fq 'yq flag --arg' <<< "$cli_contract_output" || fail 'CLI contract lint missed yq --arg fixture'
grep -Fq 'gh --body の @path' <<< "$cli_contract_output" || fail 'CLI contract lint missed gh --body @path fixture'
grep -Fq 'gh api の型付き値' <<< "$cli_contract_output" || fail 'CLI contract lint missed gh api -f typed value fixture'
if grep -Fq "$cli_contract_fixture:5:" <<< "$cli_contract_output"; then
  fail 'CLI contract lint falsely flagged valid @ text or body-file'
fi
if grep -Fq "$cli_contract_fixture:7:" <<< "$cli_contract_output"; then
  fail 'CLI contract lint falsely flagged Markdown code block'
fi
grep -Fq "$cli_contract_fixture:9:" <<< "$cli_contract_output" || fail 'CLI contract lint missed gh --body=@path (no space)'
grep -Fq "$cli_contract_fixture:10:" <<< "$cli_contract_output" || fail 'CLI contract lint missed gh -b "@path"'
if grep -Fq "$cli_contract_fixture:11:" <<< "$cli_contract_output"; then
  fail 'CLI contract lint falsely flagged an ordinary --body value'
fi

# Disable git auto-maintenance globally for the whole harness: commit, push,
# and receive-pack spawn `git maintenance run --auto --detach` in the
# background, which repacks fixture repos while a later step clones them and
# intermittently fails the clone with missing-object errors.
mkdir -p "$TEST_ROOT/config/git"
cat > "$TEST_ROOT/config/git/config" <<'GITCONFIG'
[maintenance]
	auto = false
[gc]
	auto = 0
[receive]
	autogc = false
GITCONFIG

if env -u DEV_ENVIRONMENT "$PROJECT_ROOT/scripts/check-environment.sh" >/dev/null 2>&1; then
  fail 'environment guard accepted an unpinned host environment'
fi

# unfold_body (Issue #110): expands a literal `\n` into a real newline, and
# only that exact two-character sequence (a lone backslash, or a real
# newline that was already real, must pass through unchanged).
unfold_body_out=$(source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"; unfold_body '<!-- m -->\nbody')
[[ $(wc -l <<< "$unfold_body_out") -eq 2 ]] || fail 'unfold_body did not expand a literal \n into a real newline'
[[ $unfold_body_out == $'<!-- m -->\nbody' ]] || fail 'unfold_body produced an unexpected result'
unfold_body_plain=$(source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"; unfold_body 'no newline here')
[[ $unfold_body_plain == 'no newline here' ]] || fail 'unfold_body altered a body with no \n shorthand'

cat > "$FAKE_BIN/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
# Identify "the repo" by the main worktree's root (git-common-dir's parent),
# not by $PWD: a postmortem `link`/`complete` turn (Issue #132) invokes this
# fake gh from inside a dedicated worktree's own root (bin/agentic-loop cds
# to its own --show-toplevel), which is a DIFFERENT directory than $target
# even though it is the same logical repo. Real `gh api` targets an explicit
# repos/OWNER/REPO endpoint and does not care about local cwd at all; this
# resolves the same way so every worktree of one repo shares one fake state.
repo_root=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) && repo_root=${repo_root%/.git} || repo_root=$PWD
slug="acme/$(basename "$repo_root")"
key=$(printf '%s' "$repo_root" | tr '/' '_')
state="$FAKE_GH_ROOT/$key.state"
project="$FAKE_GH_ROOT/$key.project"
project_items="$FAKE_GH_ROOT/$key.project-items"
project_values="$FAKE_GH_ROOT/$key.project-values"
project_page2="$FAKE_GH_ROOT/$key.project-page2"
project_fields="$FAKE_GH_ROOT/$key.project-fields"
project_link="$FAKE_GH_ROOT/$key.project-link"
comments="$FAKE_GH_ROOT/$key.comments"
# Append-only audit trail of every comment body this fixture ever recorded,
# survives the frequent `: > "$comments"` truncations between fixtures (Issue
# #110). Encoded the same way as $comments (see encode_comment_body below), so
# a real newline is a literal 2-char `\n` and a literal `\n` bug is a 3-char
# `\\n`.
comment_bodies_log="$FAKE_GH_ROOT/comment-bodies.log"
# Encode a comment body the same way the fake gh's one-comment-per-line
# $comments/$comment_bodies_log records require: escape a literal backslash
# first (so a real one is never confused with the encoding below), then fold
# a real newline into a literal `\n`. This is the fixture's own encoding, kept
# deliberately independent of common.sh's json_escape/unfold_body so it
# exercises the production code path rather than assuming it.
encode_comment_body() {
  local v=$1
  v=${v//\\/\\\\}
  v=${v//$'\n'/\\n}
  printf '%s' "$v"
}
closes="$FAKE_GH_ROOT/$key.closes"
views="$FAKE_GH_ROOT/$key.views"
labels="$FAKE_GH_ROOT/$key.labels"
titles="$FAKE_GH_ROOT/$key.titles"
diagnosis_issues="$FAKE_GH_ROOT/$key.diagnosis-issues"
metrics_issues="$FAKE_GH_ROOT/$key.metrics-issues"
metrics_events="$FAKE_GH_ROOT/$key.metrics-events"
metrics_pulls="$FAKE_GH_ROOT/$key.metrics-pulls"
# Never record a bearer token in the call log: the opencode-go usage API key
# must not leak even into the test harness transcript.
log_args=("$@")
for ((i=0; i<${#log_args[@]}; i++)); do
  if [[ ${log_args[$i]} == -H && ${log_args[$((i+1))]:-} == 'Authorization: Bearer '* ]]; then
    log_args[$((i+1))]='Authorization: Bearer [REDACTED]'
  fi
done
printf '%s\t%s\n' "$PWD" "${log_args[*]}" >> "$FAKE_GH_ROOT/calls"
if [[ $* == *--slurp* && $* == *--jq* ]]; then
  printf 'the `--slurp` option is not supported with `--jq`\n' >&2
  exit 1
fi
case "${1:-} ${2:-}" in
  'auth status') [[ ${FAKE_GH_AUTH_FAIL:-0} == 0 ]] ;;
  'api user') printf 'test-operator\n' ;;
  'api https://'*)
    # OpenCode Go usage API (Issue #155). Tests control the response with
    # FAKE_GO_USAGE_FILE (default: a modest, non-exhausted usage).
    if [[ ${FAKE_GO_USAGE_FAIL:-0} == 1 ]]; then printf 'HTTP 500: Service Unavailable\n' >&2; exit 1; fi
    if [[ -n ${FAKE_GO_USAGE_FILE:-} && -r $FAKE_GO_USAGE_FILE ]]; then cat "$FAKE_GO_USAGE_FILE"
    else printf '%s\n' '{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-08-16T14:00:07.328Z"},"weekly":{"status":"ok","percent":4,"resetsAt":"2026-08-17T00:00:00.328Z"},"monthly":{"status":"ok","percent":2,"resetsAt":"2026-09-16T08:12:28.328Z"}}}'; fi
    ;;
  'api repos/'*)
    [[ $2 != */ ]] || { printf 'HTTP 404: Not Found\n' >&2; exit 1; }
    rest_failures="$FAKE_GH_ROOT/$key.rest-failures"
    current_failures=$(cat "$rest_failures" 2>/dev/null || printf '0')
    if (( current_failures < ${FAKE_REST_FAILURES:-0} )); then
      printf '%s\n' "$((current_failures + 1))" > "$rest_failures"
      printf 'HTTP 503: Service Unavailable\n' >&2
      exit 1
    fi
    endpoint=${2#repos/}; endpoint=${endpoint#*/}
    if [[ $endpoint == */* ]]; then endpoint=${endpoint#*/}; else endpoint=''; fi
    method=GET wanted='' form_state='' form_state_reason='' input_file=''
    for ((i=1; i<=$#; i++)); do
      case ${!i} in
        --method) j=$((i+1)); method=${!j} ;;
        labels=*) wanted=${!i#labels=} ;;
        state=*) form_state=${!i#state=} ;;
        state_reason=*) form_state_reason=${!i#state_reason=} ;;
        --input) j=$((i+1)); input_file=${!j} ;;
      esac
    done
    if [[ $endpoint == labels && $method == GET ]]; then
      cat "$labels" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == POST ]]; then
      # bin/agentic-loop postmortem create (Issue #132, ADR 0026): a bare
      # POST to issues creates a new fixture row (state=queued open) and
      # returns its number for `--jq .number`, exactly like a real Issue
      # create. body= is stored base64-encoded in column 8, same as every
      # other row, so a later GET .body // "" round-trips it.
      new_body_b64=''
      for arg in "$@"; do
        [[ $arg == body=* ]] && new_body_b64=$(printf '%s' "${arg#body=}" | base64 -w0)
      done
      ( flock 9
        next=$(( $(awk '{n=$1+0; if (n>max) max=n} END{print (max=="" ? 8999 : max)}' "$state" 2>/dev/null || printf 8999) + 1 ))
        printf '%s queued open none 2026-01-01T00:00:00Z none none %s\n' "$next" "$new_body_b64" >> "$state"
        printf '%s\n' "$next"
      ) 9> "$state.lock"
    elif [[ $endpoint == issues && $method == GET && $* == *fromdateiso8601* ]]; then
      # bin/agentic-loop metrics collection A: hand-authored fixture rows are
      # returned verbatim (the fake gh never runs real jq), keyed off the
      # fromdateiso8601 call that only this query makes.
      [[ ${FAKE_METRICS_ISSUES_FAIL:-0} == 0 ]] || { printf 'HTTP 503: Service Unavailable\n' >&2; exit 1; }
      cat "$metrics_issues" 2>/dev/null || true
    elif [[ $endpoint == issues/comments && $* == *'agentic-loop:traceability'* ]]; then
      # bin/agentic-loop trace --audit (Issue #53): distinct from metrics
      # collection B's repo-wide comments read matched just below (this
      # branch must come first, since both share the issues/comments endpoint).
      [[ -n ${FAKE_TRACE_AUDIT_EVENTS:-} ]] && cat "$FAKE_TRACE_AUDIT_EVENTS" 2>/dev/null || true
    elif [[ $endpoint == issues/comments && $* == *'(.body // "" | @base64)'* ]]; then
      # bin/agentic-loop doctor's repo-wide, single-page (no --paginate)
      # body_unexpanded_file_reference comment scan (Issue #272): distinct
      # from metrics collection B matched just below, which shares the
      # issues/comments endpoint but uses a different --jq shape.
      [[ -n ${FAKE_DOCTOR_COMMENTS:-} ]] && cat "$FAKE_DOCTOR_COMMENTS" 2>/dev/null || true
    elif [[ $endpoint == issues/comments ]]; then
      # bin/agentic-loop metrics collection B (repo-wide comments): distinct
      # from the per-Issue issues/N/comments endpoint matched further below.
      [[ ${FAKE_METRICS_EVENTS_FAIL:-0} == 0 ]] || { printf 'HTTP 503: Service Unavailable\n' >&2; exit 1; }
      cat "$metrics_events" 2>/dev/null || true
    elif [[ $endpoint == pulls && $* == *fromdateiso8601* ]]; then
      # bin/agentic-loop metrics collection C.
      [[ ${FAKE_METRICS_PULLS_FAIL:-0} == 0 ]] || { printf 'HTTP 503: Service Unavailable\n' >&2; exit 1; }
      cat "$metrics_pulls" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET && $form_state == all && $wanted == agent:stale ]]; then
      awk '$2 == "stale" {print $1 "\t" "Fake issue " $1}' "$state" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET && $form_state == all && $* == *'.pull_request == null) | .number'* ]]; then
      # bin/agentic-loop hint reconstruction (rebuild_project_hints) parses
      # issue numbers only; the generic state=all branch below emits URLs for
      # callers that ask for html_url instead.
      awk '{print $1}' "$state" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET && $form_state == all ]]; then
      awk -v slug="$slug" '{print "https://github.example/" slug "/issues/" $1}' "$state" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET && $form_state == open && $* == *html_url* ]]; then
      awk -v slug="$slug" '$3 != "closed" {print "https://github.example/" slug "/issues/" $1}' "$state" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET && $form_state == open && $* == *'startswith("priority:")'* ]]; then
      # bin/agentic-loop setup's legacy priority label migration: emit
      # number<TAB>body(base64)<TAB>priority-labels for open Issues that still
      # carry a priority:* label.
      if [[ -r $state ]]; then
        awk '
        $3 != "closed" {
          split($4,p,","); plist=""
          for(i in p) if(p[i] != "" && p[i] != "none") plist=(plist=="" ? p[i] : plist "," p[i])
          if (plist == "") next
          printf "%s\t%s\t%s\n", $1, ($8 == "" || $8 == "-" ? "" : $8), plist
        }' "$state" 2>/dev/null || true
      fi
    elif [[ $endpoint == issues && $method == GET && -z $wanted && $* == *'then "-" else'* ]]; then
      awk 'function prio(b,   body, val, max, rest, tok, cmd) {
        max = 0
        if (b == "" || b == "-" || b == "none") return 0
        cmd = "printf %s \"" b "\" | base64 -d 2>/dev/null"
        while ((cmd | getline body) > 0) {
          rest = body
          while (match(rest, /agentic-loop:priority[[:space:]]+[0-9]+([[:space:]]|--)/)) {
            tok = substr(rest, RSTART, RLENGTH)
            val = 0
            if (match(tok, /[0-9]+/)) val = substr(tok, RSTART, RLENGTH) + 0
            if (val >= 0 && val <= 100 && val > max) max = val
            rest = substr(rest, RSTART + RLENGTH)
          }
        }
        close(cmd)
        return max
      }
      $3 != "closed" {
        category=6; if ($7 ~ /(^|,)loop-continuity(,|$)/) category=0; else if ($7 ~ /(^|,)confidentiality-incident(,|$)/) category=1; else if ($7 ~ /(^|,)integrity-incident(,|$)/) category=2; else if ($7 ~ /(^|,)availability-incident(,|$)/) category=3; else if ($7 ~ /(^|,)bug(,|$)/) category=4; else if ($7 ~ /(^|,)feature(,|$)/) category=5
        priority=prio($8)
        created=($5 == "" ? $1 : $5); updated=($6 == "" ? "-" : $6)
        body=($8 == "" ? "-" : $8); categories=$7; gsub(/,/, ",category:", categories); categories=(categories == "" || categories == "none" ? "-" : "category:" categories)
        print $1 "\t" $2 "\t" updated "\t" created "\t" body "\t" categories "\t" category "\t" priority
      }' "$state" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET && -z $wanted ]]; then
      # status-snapshot: every open Issue, classified purely by its own state
      # word (see bin/agentic-loop's status_snapshot_fetch), with the same
      # numeric priority / category ranks as the agent:queued created_at query.
      awk 'function prio(b,   body, val, max, rest, tok, cmd) {
        max = 0
        if (b == "" || b == "-" || b == "none") return 0
        cmd = "printf %s \"" b "\" | base64 -d 2>/dev/null"
        while ((cmd | getline body) > 0) {
          rest = body
          while (match(rest, /agentic-loop:priority[[:space:]]+[0-9]+([[:space:]]|--)/)) {
            tok = substr(rest, RSTART, RLENGTH)
            val = 0
            if (match(tok, /[0-9]+/)) val = substr(tok, RSTART, RLENGTH) + 0
            if (val >= 0 && val <= 100 && val > max) max = val
            rest = substr(rest, RSTART + RLENGTH)
          }
        }
        close(cmd)
        return max
      }
      $3 != "closed" {
        category=6; if ($7 ~ /(^|,)loop-continuity(,|$)/) category=0; else if ($7 ~ /(^|,)confidentiality-incident(,|$)/) category=1; else if ($7 ~ /(^|,)integrity-incident(,|$)/) category=2; else if ($7 ~ /(^|,)availability-incident(,|$)/) category=3; else if ($7 ~ /(^|,)bug(,|$)/) category=4; else if ($7 ~ /(^|,)feature(,|$)/) category=5
        priority=prio($8)
        created=($5 == "" ? $1 : $5)
        state="other"
        if ($2 == "running") state="running"
        else if ($2 == "queued") state="queued"
        else if ($2 == "needs-input") state="needs-input"
        else if ($2 == "failed") state="failed"
        else if ($2 == "parked") state="parked"
        else if ($2 == "in-review") state="in-review"
        else if ($2 == "blocked") state="blocked"
        body_b64=($8 == "" ? "-" : $8)
        print $1 "\t" "Fake issue " $1 "\t" state "\t" priority "\t" category "\t" created "\t" body_b64
      }' "$state" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET ]]; then
      case $wanted in
        agent:queued)
          if [[ $* == *updated_at* ]]; then
            awk '$2 == "queued" && $3 != "closed" && $6 != "" {print $1 "\t" $6}' "$state"
          elif [[ $* == *created_at* ]]; then
            awk 'function prio(b,   body, val, max, rest, tok, cmd) {
              max = 0
              if (b == "" || b == "-" || b == "none") return 0
              cmd = "printf %s \"" b "\" | base64 -d 2>/dev/null"
              while ((cmd | getline body) > 0) {
                rest = body
                while (match(rest, /agentic-loop:priority[[:space:]]+[0-9]+([[:space:]]|--)/)) {
                  tok = substr(rest, RSTART, RLENGTH)
                  val = 0
                  if (match(tok, /[0-9]+/)) val = substr(tok, RSTART, RLENGTH) + 0
                  if (val >= 0 && val <= 100 && val > max) max = val
                  rest = substr(rest, RSTART + RLENGTH)
                }
              }
              close(cmd)
              return max
            }
            $2 == "queued" && $3 != "closed" {
              category=6; if ($7 ~ /(^|,)loop-continuity(,|$)/) category=0; else if ($7 ~ /(^|,)confidentiality-incident(,|$)/) category=1; else if ($7 ~ /(^|,)integrity-incident(,|$)/) category=2; else if ($7 ~ /(^|,)availability-incident(,|$)/) category=3; else if ($7 ~ /(^|,)bug(,|$)/) category=4; else if ($7 ~ /(^|,)feature(,|$)/) category=5
              priority=prio($8)
              created=($5 == "" ? $1 : $5); print priority "\t" category "\t" created "\t" $1 "\t" $8
            }' "$state"
            if [[ -n ${FAKE_STALE_QUEUED_ISSUE:-} ]] && ! awk -v n="$FAKE_STALE_QUEUED_ISSUE" '$1 == n && $2 == "queued" {found=1} END {exit !found}' "$state"; then
              awk -v n="$FAKE_STALE_QUEUED_ISSUE" '$1 == n {print "0\t5\t" ($5 == "" ? $1 : $5) "\t" $1 "\t" $8}' "$state"
            fi
          else awk '$2 == "queued" && $3 != "closed" {print $1}' "$state"; fi ;;
        agent:running)
          if [[ $* == *title* ]]; then awk '$2 == "running" && $3 != "closed" {print "#" $1 " Fake issue " $1}' "$state"
          elif [[ $* == *'.body'* ]]; then awk '$2 == "running" && $3 != "closed" {print $1 "\t" $8}' "$state"
          else awk '$2 == "running" && $3 != "closed" {print $1}' "$state"; fi ;;
        agent:needs-input) awk '$2 == "needs-input" && $3 != "closed" {print $1}' "$state" ;;
        agent:failed) awk '$2 == "failed" && $3 != "closed" {print $1}' "$state" ;;
        agent:parked) awk '$2 == "parked" && $3 != "closed" {print $1}' "$state" ;;
        agent:blocked) awk '$2 == "blocked" && $3 != "closed" {print $1 "\t" $8}' "$state" ;;
        postmortem)
          # bin/agentic-loop postmortem create's dedup search
          # (postmortem_find_open, Issue #132): an open Issue whose body
          # (column 8, base64) contains the requested fingerprint marker.
          fp=$(grep -oE 'fingerprint=[0-9a-f]{8}' <<< "$*" | head -n1 | cut -d= -f2)
          if [[ -n $fp ]]; then
            awk -v fp="$fp" '$3 != "closed" {
              body=""
              if ($8 != "" && $8 != "none") {
                cmd = "printf %s \"" $8 "\" | base64 -d 2>/dev/null"
                while ((cmd | getline line) > 0) body = body line "\n"
                close(cmd)
              }
              if (index(body, "fingerprint=" fp) > 0) print $1
            }' "$state"
          fi ;;
      esac
    elif [[ $endpoint =~ ^issues/([0-9]+)/labels$ && $method == PUT ]]; then
      issue=${BASH_REMATCH[1]}; payload=$(if [[ -n $input_file && $input_file != - ]]; then cat "$input_file"; else cat; fi); target=$(sed -n 's/.*"agent:\([^"]*\)".*/\1/p' <<< "$payload"); category=$(grep -o 'category:[a-z-]*' <<< "$payload" | head -n 1 | cut -d: -f2 || true); priority_labels=$(grep -o 'priority:[a-z-]*' <<< "$payload" | sed 's/priority://' | sort -u | paste -sd, - || true)
      ( flock 9; awk -v n="$issue" -v s="$target" -v c="$category" -v p="$priority_labels" '{if ($1 == n) {if (s != "") $2=s; if (c != "") $7=c; if (NF >= 4) {if (p != "") $4=p; else $4="none"}} print}' "$state" > "$state.$$.tmp" && mv "$state.$$.tmp" "$state" ) 9> "$state.lock"
    elif [[ $endpoint =~ ^issues/comments/([0-9]+)$ && $method == PATCH ]]; then
      cid=${BASH_REMATCH[1]}
      body=''; for arg in "$@"; do [[ $arg == body=* ]] && body=${arg#body=}; done
      body=$(encode_comment_body "$body")
      printf '%s\n' "$body" >> "$comment_bodies_log"
      ( flock 9
        mapfile -t comment_lines < "$comments" 2>/dev/null || comment_lines=()
        if (( cid >= 1 && cid <= ${#comment_lines[@]} )); then
          comment_lines[cid-1]="${comment_lines[cid-1]%% *} $body"
          printf '%s\n' "${comment_lines[@]}" > "$comments"
        else
          # Real GitHub 404s a PATCH for a comment that no longer exists; the
          # scripts' PATCH-in-place callers then fall back to creating a fresh
          # durable comment. A silent no-op here would mask a stale cached id
          # (e.g. a resume after an earlier worker's lifecycle) and drop the
          # new handoff/lease comment entirely.
          printf 'HTTP 404: Not Found\n' >&2
          exit 1
        fi
      ) 9> "$comments.lock"
    elif [[ $endpoint =~ ^issues/([0-9]+)/comments$ ]]; then
      issue=${BASH_REMATCH[1]}
      if [[ $method == POST ]]; then
        body=''; for arg in "$@"; do [[ $arg == body=* ]] && body=${arg#body=}; done
        body=$(encode_comment_body "$body")
        printf '%s\n' "$body" >> "$comment_bodies_log"
        ( flock 9
          printf '%s %s\n' "$issue" "$body" >> "$comments"
          if [[ $* == *"--jq .id"* ]]; then wc -l < "$comments" | tr -d '[:space:]'; printf '\n'; fi
        ) 9> "$comments.lock"
      elif [[ $method == GET && $* == *'agentic-loop:traceability'* ]]; then
        # trace_verdict_upsert's fallback search for an existing verdict
        # comment when the local $STATE_ROOT/workers/ISSUE.trace cache file
        # is missing or stale (Issue #53).
        awk -v n="$issue" '$1 == n && index($0, "agentic-loop:traceability schema=1") {id=NR} END{if (id) print id}' "$comments" 2>/dev/null || true
      elif [[ $method == GET && $* == *'agentic-loop:preflight-approved'* ]]; then
        # preflight.sh's preflight_approved (Issue #197): emulate the real
        # `since`-bounded scan. Each comment's mock `updated_at` is a real
        # ISO-8601 timestamp derived from its 1-based line number in
        # $comments, one minute apart, so lexical string comparison sorts
        # the same as chronological order -- exactly like the production
        # `-f since=` filter, which also compares ISO-8601 strings. Rows
        # at/after the passed `since` cursor are included (inclusive, the
        # safe-side choice regardless of whether the real GitHub API treats
        # `since` as inclusive or exclusive), so a warm cache (a `since`
        # from an earlier call) only ever re-scans genuinely new comments,
        # not the Issue's full history.
        since_arg=''
        for arg in "$@"; do [[ $arg == since=* ]] && since_arg=${arg#since=}; done
        [[ $since_arg =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || since_arg='1970-01-01T00:00:00Z'
        pf_rows=$(awk -v n="$issue" -v since="$since_arg" '
          $1 == n {
            ts = sprintf("1970-01-01T%02d:%02d:00Z", int(NR / 60), NR % 60)
            if (ts < since) next
            body = $0
            sub(/^[^ ]+ /, "", body)
            print "u\t" ts
            if (index(body, "agentic-loop:preflight-approved") > 0 && match(body, /token=[0-9a-f]{12}/)) {
              print "t\t" substr(body, RSTART + 6, 12)
            }
          }
        ' "$comments" 2>/dev/null)
        # Test-only observability (Issue #197): one line per call recording
        # how many comment rows this scan actually processed, the direct
        # proxy for the transferred data volume a real GitHub response would
        # carry for the same `since` boundary.
        if [[ -z $pf_rows ]]; then pf_row_count=0; else pf_row_count=$(wc -l <<< "$pf_rows"); fi
        printf '%s\n' "$pf_row_count" >> "$FAKE_GH_ROOT/preflight-approved-rows.log"
        [[ -z $pf_rows ]] || printf '%s\n' "$pf_rows"
      elif [[ $* == *agentic-loop:claim* ]]; then
        awk -v n="$issue" '$1 == n && index($0, "agentic-loop:claim") {body=$0; sub(/^[^ ]+ /, "", body); printf "%s\t%s", NR, body | "base64 -w0"; close("base64 -w0"); printf "\n"}' "$comments" 2>/dev/null || true
      elif [[ $* == *needs-input* ]]; then
        # requeue_answered (Issue #192): classify each of this Issue's
        # comments as MARKER/REPLY/OTHER, exactly like the real per-page jq
        # program. Without --paginate, only the first per_page comments are
        # visible (the pre-fix truncation bug); with --paginate, every
        # comment across every page is, matching real `gh api --paginate`
        # (verified: it re-runs --jq once per page, in page order, rather
        # than on one concatenated array).
        per_page=30
        for arg in "$@"; do [[ $arg == per_page=* ]] && per_page=${arg#per_page=}; done
        rows=$(awk -v n="$issue" '$1 == n {sub(/^[^ ]+ /, ""); print}' "$comments" 2>/dev/null)
        [[ $* == *--paginate* ]] || rows=$(printf '%s\n' "$rows" | head -n "$per_page")
        [[ -z $rows ]] || awk -v marker='agentic-loop:needs-input' -v ref='<!-- agentic-loop:' '
          {
            if (index($0, marker)) print "MARKER"
            else if (index($0, ref) == 0) print "REPLY"
            else print "OTHER"
          }' <<< "$rows"
      elif [[ $method == GET && $* == *'[.[].body]'* ]]; then
        # triage_issue_content (Issue #167): every existing comment body for
        # this Issue, oldest first, the same content-classification input the
        # real `[.[].body] | join("\n")` jq would produce. Distinct from the
        # single-latest-comment `else` fallback below, which other callers rely on.
        awk -v n="$issue" '$1 == n {sub(/^[^ ]+ /, ""); print}' "$comments" 2>/dev/null || true
      else tail -n 1 "$comments" 2>/dev/null || true; fi
    elif [[ $endpoint =~ ^issues/([0-9]+)/dependencies/blocked_by$ && $method == GET ]]; then
      issue=${BASH_REMATCH[1]}
      if [[ ${FAKE_DEPENDENCIES_FORBIDDEN:-0} == 1 ]]; then printf 'HTTP 403: Forbidden\n' >&2; exit 1; fi
      if [[ ${FAKE_DEPENDENCIES_UNAVAILABLE:-0} == 1 ]]; then printf 'HTTP 404: Not Found\n' >&2; exit 1; fi
      if [[ ${FAKE_DEPENDENCIES_TRANSIENT_FAIL:-0} == 1 ]]; then printf 'HTTP 503: Service Unavailable\n' >&2; exit 1; fi
      links=$(awk -v n="$issue" '$1 == n {print $2}' "$FAKE_GH_ROOT/$key.dep-links" 2>/dev/null || true)
      [[ -n $links ]] && tr ',' '\n' <<< "$links"
      true
    elif [[ $endpoint =~ ^issues/([0-9]+)/dependencies/blocked_by$ && $method == POST ]]; then
      # Issue #252: this endpoint's `issue_id` is a typed-integer property
      # requiring the blocking Issue's *database id*, not its Issue number.
      # This fixture's `--jq .id` convention below returns number+900000, so
      # accepting only that shape here (a) fails a caller that still sends
      # the raw number or an untyped string, and (b) lets dispose.sh's own
      # immediate GET-back verification see the transferred dependency by
      # resolving the id back to a number.
      issue=${BASH_REMATCH[1]}
      issue_id_arg=''
      for arg in "$@"; do [[ $arg == issue_id=* ]] && issue_id_arg=${arg#issue_id=}; done
      dep_number=$((issue_id_arg - 900000))
      if [[ ! $issue_id_arg =~ ^[0-9]+$ ]] || (( dep_number < 1 )); then
        printf 'HTTP 422: Unprocessable Entity\n' >&2; exit 1
      fi
      ( flock 9
        existing_line=$(awk -v n="$issue" '$1 == n {print $2}' "$FAKE_GH_ROOT/$key.dep-links" 2>/dev/null || true)
        new_line=$dep_number; [[ -z $existing_line ]] || new_line="$existing_line,$dep_number"
        awk -v n="$issue" '$1 != n' "$FAKE_GH_ROOT/$key.dep-links" 2>/dev/null > "$FAKE_GH_ROOT/$key.dep-links.$$.tmp" || true
        printf '%s %s\n' "$issue" "$new_line" >> "$FAKE_GH_ROOT/$key.dep-links.$$.tmp"
        mv "$FAKE_GH_ROOT/$key.dep-links.$$.tmp" "$FAKE_GH_ROOT/$key.dep-links"
      ) 9> "$FAKE_GH_ROOT/$key.dep-links.lock"
    elif [[ $endpoint =~ ^issues/([0-9]+)/sub_issues$ && $method == POST ]]; then
      # Issue #252: sub_issue_id is a typed-integer property; reject a
      # non-numeric value the way GitHub's 422 would.
      if [[ ${FAKE_SUB_ISSUES_FAIL:-0} == 1 ]]; then
        printf '{"message":"Invalid request.\\n\\nInvalid property /sub_issue_id: forced-test-failure","status":"422"}\n' >&2
        exit 1
      fi
      sub_issue_id_arg=''
      for arg in "$@"; do [[ $arg == sub_issue_id=* ]] && sub_issue_id_arg=${arg#sub_issue_id=}; done
      [[ $sub_issue_id_arg =~ ^[0-9]+$ ]] || { printf 'HTTP 422: Unprocessable Entity\n' >&2; exit 1; }
    elif [[ $endpoint =~ ^issues/([0-9]+)$ && $* == *'--jq .id'* ]]; then
      # This fixture's database id is deliberately distinct from the Issue
      # number (id = number + 900000) so a test can tell apart a call that
      # correctly resolved to the database id from one that (Issue #252)
      # mistakenly passed the Issue number straight through.
      printf '%s\n' "$((BASH_REMATCH[1] + 900000))"
    elif [[ $endpoint =~ ^issues/([0-9]+)$ ]]; then
      issue=${BASH_REMATCH[1]}
      if [[ $method == PATCH && $form_state == closed ]]; then
        ( flock 9; awk -v n="$issue" '{if ($1 == n) $3="closed"; print}' "$state" > "$state.$$.tmp" && mv "$state.$$.tmp" "$state" ) 9> "$state.lock"
        # Audit log of every Issue close this fake gh ever performs, so tests
        # can assert an Issue was NEVER closed (positive assertions on closes
        # can be spoofed by a state-file peek; a negative assertion here can't).
        printf '%s\t%s\n' "$issue" "${form_state_reason:-none}" >> "$closes"
      elif [[ $method == PATCH && $* == *body=* ]]; then
        body=''; for arg in "$@"; do [[ $arg == body=* ]] && body=${arg#body=}; done
        b64=$(printf '%s' "$body" | base64 -w0)
        ( flock 9; awk -v n="$issue" -v b="$b64" '{if ($1 == n) $8=b; print}' "$state" > "$state.$$.tmp" && mv "$state.$$.tmp" "$state" ) 9> "$state.lock"
      elif [[ $* == *'[(.body // "" | @base64)'* ]]; then
        tmp=$(awk -v n="$issue" '$1 == n {printf "%s\t", $8; split($4,p,","); labels="agent:" $2; for(i in p) if(p[i] != "" && p[i] != "none") labels=labels ",priority:" p[i]; split($7,c,","); for(i in c) if(c[i] != "" && c[i] != "none") labels=labels ",category:" c[i]; print labels}' "$state")
        body_b64=${tmp%%$'\t'*}; labels=${tmp#*$'\t'}
        printf '%s\t%s\n' "${body_b64:-}" "$labels"
      elif [[ $* == *'.updated_at'* && $* == *'.body // ""'* ]]; then
        awk -v n="$issue" '$1 == n {
          labels="agent:" $2; split($4,p,","); for(i in p) if(p[i] != "" && p[i] != "none") labels=labels ",priority:" p[i]; split($7,c,","); for(i in c) if(c[i] != "" && c[i] != "none") labels=labels ",category:" c[i]
          body=""; if ($8 != "" && $8 != "none") { cmd="printf %s \"" $8 "\" | base64 -d"; cmd | getline body; close(cmd) }
          printf "%s\037%s\037%s\037%s\n", $3, ($6 == "" || $6 == "none" ? "2026-01-01T00:00:00Z" : $6), labels, body
        }' "$state"
      elif [[ $* == *'[.state, ([.labels[].name]'* ]]; then
        awk -v n="$issue" '$1 == n {printf "%s\tagent:%s\n", $3, $2}' "$state"
      elif [[ $* == *'select(startswith("agent:"))'* ]]; then
        # bin/lib/agentic-loop/dispose.sh's issue_agent_state: [.state, agent:*
        # labels joined]. The fixture state file only ever carries one agent:*
        # state word per Issue, so it is equivalent to the single-label form above.
        awk -v n="$issue" '$1 == n {printf "%s\tagent:%s\n", $3, $2}' "$state"
      elif [[ $* == *'.state_reason // ""'* ]]; then
        if ! awk -v n="$issue" '$1 == n {found=1} END{exit !found}' "$state" 2>/dev/null; then
          printf 'HTTP 404: Not Found\n' >&2; exit 1
        fi
        awk -v n="$issue" '$1 == n {
          labels = ($2 == "none" ? "" : "agent:" $2)
          printf "%s\037%s\037%s\n", $3, ($9 == "" ? "" : $9), labels
        }' "$state"
      elif [[ $* == *'.body // ""'* ]]; then
        if ! awk -v n="$issue" '$1 == n {found=1} END{exit !found}' "$state" 2>/dev/null; then
          printf 'HTTP 404: Not Found\n' >&2; exit 1
        fi
        awk -v n="$issue" '$1 == n {print $8}' "$state" | base64 -d 2>/dev/null || true
      elif [[ $* == *'.title'* ]]; then
        # triage_issue_content (Issue #167): the state fixture has no title
        # column, so this defaults to a fixed, keyword-free placeholder
        # ("agent:$2"-style text would spuriously contain "queue" and skew
        # content-based category triage). A test that needs title-driven
        # classification overrides one Issue via $titles (number<TAB>title).
        if grep -Fq "$issue"$'\t' "$titles" 2>/dev/null; then
          awk -F '\t' -v n="$issue" '$1 == n {print $2}' "$titles"
        else
          awk -v n="$issue" '$1 == n {print "Fake issue " n}' "$state"
        fi
      elif [[ $* == *'join(",")'* ]]; then
        awk -v n="$issue" '$1 == n {split($7,c,","); out=""; for(i in c) if(c[i] != "" && c[i] != "none") out=out (out=="" ? "" : ",") "category:" c[i]; print out}' "$state"
      elif [[ $* == *'startswith("category:") | not'* ]]; then
        category=$(grep -o 'category:[a-z-]*' <<< "$*" | tail -n 1); printf '["agent:queued","%s"]\n' "$category"
      elif [[ $* == *'startswith("priority:") | not'* ]]; then
        awk -v n="$issue" '$1 == n {printf "[\"agent:%s\"", $2; split($7,c,","); for(i in c) if(c[i] != "" && c[i] != "none") printf ",\"category:%s\"", c[i]; print "]"}' "$state"
      elif [[ $* == *'[.labels[].name]'* ]]; then
        awk -v n="$issue" '$1 == n {printf "[\"agent:%s\"", $2; split($4,p,","); for(i in p) if(p[i] != "" && p[i] != "none") printf ",\"priority:%s\"",p[i]; split($7,c,","); for(i in c) if(c[i] != "" && c[i] != "none") printf ",\"category:%s\"",c[i]; print "]"}' "$state"
      elif [[ $* == *starts*with* ]]; then
        target=$(sed -n 's/.*\["agent:\([^"]*\)"\].*/\1/p' <<< "$*"); printf '["agent:%s"]\n' "$target"
      else awk -v n="$issue" '$1 == n {print "agent:" $2}' "$state"; fi
    elif [[ $endpoint =~ ^commits/.+/check-runs$ ]]; then
      jqarg=''
      for ((i = 1; i <= $#; i++)); do [[ ${!i} == --jq ]] && { j=$((i + 1)); jqarg=${!j}; }; done
      if [[ $jqarg == '.check_runs' ]]; then
        # trace.sh's trace_evaluate (Issue #53): distinct from resume_probe's
        # multi-line check-status jq matched in the else branch below.
        if [[ -n ${FAKE_CHECK_RUNS_FILE:-} ]]; then cat "$FAKE_CHECK_RUNS_FILE"; else printf '[]\n'; fi
      else
        [[ -n ${FAKE_RESUME_CHECKS:-} ]] && printf '%s\n' "$FAKE_RESUME_CHECKS"
      fi
    elif [[ $endpoint =~ ^pulls/[0-9]+/files$ ]]; then
      # trace.sh's trace_evaluate PR changed-files read (Issue #53).
      [[ -n ${FAKE_PR_FILES:-} ]] && printf '%s\n' "$FAKE_PR_FILES"
    elif [[ $endpoint =~ ^pulls/[0-9]+$ ]]; then
      jqarg=''
      for ((i = 1; i <= $#; i++)); do [[ ${!i} == --jq ]] && { j=$((i + 1)); jqarg=${!j}; }; done
      if [[ $jqarg == '.body // ""' ]]; then
        # trace.sh's trace_evaluate PR-body read (Issue #53): distinct from
        # resume_probe's open-PR detail jq matched in the else branch below.
        [[ -n ${FAKE_PR_BODY_FILE:-} && -r $FAKE_PR_BODY_FILE ]] && cat "$FAKE_PR_BODY_FILE"
      else
        # resume_probe must substitute SEP in its jq program before executing
        # it. Returning literal SEP for an unconverted program makes this
        # fixture preserve the production parser's empty-field regression.
        detail_sep=SEP
        [[ $jqarg == *$'\x1f'* ]] && detail_sep=$'\x1f'
        printf '%s%s%s%s%s%s%s%s%s\n' \
          "${FAKE_RESUME_BASE_REF:-main}" "$detail_sep" "${FAKE_RESUME_BASE_SHA:-}" "$detail_sep" "${FAKE_RESUME_HEAD_SHA:-}" "$detail_sep" \
          "${FAKE_RESUME_MERGEABLE:-null}" "$detail_sep" "${FAKE_RESUME_MERGEABLE_STATE:-unknown}"
      fi
    elif [[ $endpoint == pulls && $* == *'resume-probe-prs'* ]]; then
      printf '%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\n' \
        "${FAKE_RESUME_MERGED_PR:-}" "${FAKE_RESUME_MERGED_SHA:-}" "${FAKE_RESUME_MERGED_URL:-}" \
        "${FAKE_RESUME_OPEN_PR:-}" "${FAKE_RESUME_OPEN_URL:-}" "${FAKE_RESUME_MERGE_COMMIT:-}" "${FAKE_RESUME_BASE:-}"
    elif [[ $endpoint == pulls ]]; then
      if [[ $form_state == closed ]]; then
        if [[ ${FAKE_PR_MERGED:-1} == 1 ]]; then
          head=''; for arg in "$@"; do [[ $arg == head=* ]] && head=${arg#head=}; done; head=${head#*:}
          oid=${FAKE_PR_HEAD_OID:-$(git rev-parse "refs/heads/$head" 2>/dev/null || true)}
          printf 'https://github.example/%s/pull/merged\t%s\t%s\t%s\t%s\n' "$slug" "$oid" \
            "${FAKE_PR_MERGED_NUMBER:-99}" "${FAKE_PR_MERGED_BASE:-main}" "${FAKE_PR_MERGE_COMMIT:-$oid}"
        fi
      elif [[ $* == *head=* && $* == *html_url* ]]; then printf 'https://github.example/%s/pull/6\n' "$slug"
      elif [[ $form_state == all && $* == *html_url* ]]; then printf 'https://github.example/%s/pull/1\nhttps://github.example/%s/pull/2\n' "$slug" "$slug"
      elif [[ $form_state == open && $* == *html_url* ]]; then printf 'https://github.example/%s/pull/1\n' "$slug"
      elif [[ $* == *'$m // $o'* ]]; then
        # bin/agentic-loop trace's trace_find_pr (Issue #53): distinct from
        # resume_probe's resume-probe-prs jq matched above and every
        # html_url-returning branch here.
        if [[ -n ${FAKE_TRACE_PR:-} ]]; then
          printf '%s\t%s\t%s\t%s\n' "$FAKE_TRACE_PR" "${FAKE_TRACE_HEAD:-}" "${FAKE_TRACE_MERGE_COMMIT:-${FAKE_TRACE_HEAD:-}}" "${FAKE_TRACE_BASE:-main}"
        fi
      elif [[ $* == *html_url* ]]; then printf 'https://github.example/%s/pull/6\n' "$slug"; fi
    elif [[ $endpoint == collaborators/* ]]; then
      printf '%s\n' "${FAKE_COLLABORATOR_PERMISSION:-admin}"
    elif [[ -z $endpoint ]]; then
      if [[ $* == *permissions.push* ]]; then printf 'true\n'; else printf 'main\n'; fi
    fi ;;
  'api rate_limit')
    printf '%s\t%s\t%s\n' "${FAKE_GRAPHQL_REMAINING:-5000}" "${FAKE_GRAPHQL_RESET:-$(($(date +%s) + 3600))}" "${FAKE_CORE_REMAINING:-5000}"
    ;;
  'api graphql')
    if [[ $* == *'projectItems(first:20'* ]]; then
      number=''; cursor=''; jq_expr='.'; read_jq=0
      for arg in "$@"; do [[ $arg == number=* ]] && number=${arg#number=}; [[ $arg == cursor=* ]] && cursor=${arg#cursor=}; done
      for arg in "$@"; do
        if (( read_jq )); then jq_expr=$arg; read_jq=0; elif [[ $arg == --jq ]]; then read_jq=1; fi
      done
      if [[ ${FAKE_PROJECT_CONTENT_FAIL:-0} == 1 || ( -n ${FAKE_PROJECT_CONTENT_FAIL_ISSUE:-} && $number == "$FAKE_PROJECT_CONTENT_FAIL_ISSUE" ) ]]; then
        printf '{"errors":[{"message":"forced GraphQL failure"}]}\n'
        exit 1
      fi
      if grep -Eq "/(issues|pull)/$number$" "$project_items" 2>/dev/null; then
        # A member is resolved immediately (possibly after one cursor hop when
        # the fixture lists it on the second page).
        if [[ -r $project_page2 ]] && grep -Fxq "$number" "$project_page2" && [[ -z $cursor ]]; then
          printf '{"data":{"repository":{"content":{"projectItems":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"page2cursor"}}}}}}\n' | jq -r "$jq_expr"
        else
          status=''; category=''; blocked_b64=''
          IFS=$'\t' read -r status category blocked_b64 < <(awk -F '\t' -v n="$number" '$1 == n {print $2 "\t" $3 "\t" $4; exit}' "$project_values" 2>/dev/null || true)
          blocked=''; [[ -z $blocked_b64 ]] || blocked=$(base64 -d <<< "$blocked_b64")
          printf '{"data":{"repository":{"content":{"projectItems":{"nodes":[{"id":"PVTI_%s","project":{"id":"PVT_fake"},"fieldValues":{"nodes":[{},{"name":"%s","field":{"name":"Title"}},{"name":"%s","field":{"name":"Agent status"}},{"name":"%s","field":{"name":"Category"}},{"text":"%s","field":{"name":"Blocked by"}}]}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}\n' \
            "$number" "issue $number" "$status" "$category" "$blocked" | jq -r "$jq_expr"
        fi
      else
        # A confirmed non-member is a successful empty result (END), distinct
        # from the forced failure above.  The real jq always emits this trailer.
        printf '{"data":{"repository":{"content":{"projectItems":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}\n' | jq -r "$jq_expr"
      fi
    elif [[ $* == *'repositories(first:100)'* ]]; then
      [[ -e $project_link ]] && cat "$project_link"
    elif [[ $* == *'fields(first: 100)'* ]]; then
      if [[ ${FAKE_PROJECT_FAIL:-0} == 0 && $* == *'"All open issues"'* && $* == *'"All closed issues"'* ]]; then
        printf 'true\n'
      fi
    elif [[ $* == *createProjectV2View* ]]; then
      name=''
      for ((i=1; i<=$#; i++)); do [[ ${!i} == name=* ]] && name=${!i#name=}; done
      id="PV_$(printf '%s' "$name" | tr ' ' '_')"
      printf '%s\t%s\t\n' "$id" "$name" >> "$views"
      printf '%s\n' "$id"
    elif [[ $* == *'views(first: 100)'* ]]; then
      cat "$views" 2>/dev/null || true
    elif [[ $* == *updateProjectV2View* && ${FAKE_PROJECT_VIEW_UPDATE_FAILURE:-0} == 1 ]]; then
      exit 1
    elif [[ $* == *updateProjectV2View* ]]; then
      view_id=''; filter=''
      for ((i=1; i<=$#; i++)); do
        [[ ${!i} == viewId=* ]] && view_id=${!i#viewId=}
        [[ ${!i} == filter=* ]] && filter=${!i#filter=}
      done
      awk -F '\t' -v id="$view_id" -v filter="$filter" 'BEGIN{OFS="\t"} $1 == id {$3=filter} {print}' "$views" > "$views.$$.tmp" && mv "$views.$$.tmp" "$views"
    fi ;;
  'repo view')
    if [[ $* == *defaultBranchRef* ]]; then printf 'main\n'; else printf '%s\n' "$slug"; fi ;;
  'label create')
    label_name=${3:-}; color=''; description=''
    for ((i=1; i<=$#; i++)); do
      [[ ${!i} == --color ]] && { j=$((i+1)); color=${!j}; }
      [[ ${!i} == --description ]] && { j=$((i+1)); description=${!j}; }
    done
    awk -F '\t' -v name="$label_name" '$1 != name' "$labels" 2>/dev/null > "$labels.$$.tmp" || true
    printf '%s\t%s\t%s\n' "$label_name" "$color" "$description" >> "$labels.$$.tmp"
    mv "$labels.$$.tmp" "$labels" ;;
  'label delete')
    label_name=${3:-}
    awk -F '\t' -v name="$label_name" '$1 != name' "$labels" 2>/dev/null > "$labels.$$.tmp" || true
    mv "$labels.$$.tmp" "$labels" ;;
  'project list')
    if [[ -e $project ]]; then printf '7\n'; fi ;;
  'project create') touch "$project"; printf '{"number":7}\n' ;;
  'project link') printf '%s\n' "$slug" > "$project_link" ;;
  'project field-create')
    name=''; for ((i=1; i<=$#; i++)); do [[ ${!i} == --name ]] && { j=$((i+1)); name=${!j}; }; done
    grep -Fxq "$name" "$project_fields" 2>/dev/null || printf '%s\n' "$name" >> "$project_fields" ;;
  'project item-edit')
    item_id='' option_id='' text='' clear=0 text_given=0
    for ((i=1; i<=$#; i++)); do
      case ${!i} in
        --id) j=$((i+1)); item_id=${!j} ;;
        --single-select-option-id) j=$((i+1)); option_id=${!j} ;;
        --text) j=$((i+1)); text=${!j}; text_given=1 ;;
        --clear) clear=1 ;;
      esac
    done
    # Real gh rejects an empty --text with "no changes to make" (exit 1);
    # removing a text value requires --clear.  Accepting it here hid #268's
    # "blocker cleared" reconciliation failure completely.
    if (( text_given )) && [[ -z $text ]]; then printf 'error: no changes to make\n' >&2; exit 1; fi
    item_number=${item_id#PVTI_}
    status=''; category=''; blocked_b64=''
    IFS=$'\t' read -r status category blocked_b64 < <(awk -F '\t' -v n="$item_number" '$1 == n {print $2 "\t" $3 "\t" $4; exit}' "$project_values" 2>/dev/null || true)
    case $option_id in
      PVTFO_queued) status=Queued ;; PVTFO_running) status=Running ;;
      PVTFO_needs_input) status='Needs input' ;; PVTFO_in_review) status='In review' ;;
      PVTFO_done) status=Done ;; PVTFO_failed) status=Failed ;; PVTFO_stale) status=Stale ;;
      PVTFO_blocked) status=Blocked ;; PVTFO_improvement) category=Improvement ;;
      PVTFO_feature) category=Feature ;;
    esac
    if (( clear )); then blocked_b64=''; elif [[ -n $text ]]; then blocked_b64=$(printf '%s' "$text" | base64 -w0); fi
    awk -F '\t' -v n="$item_number" '$1 != n' "$project_values" 2>/dev/null > "$project_values.$$.tmp" || true
    printf '%s\t%s\t%s\t%s\n' "$item_number" "$status" "$category" "$blocked_b64" >> "$project_values.$$.tmp"
    mv "$project_values.$$.tmp" "$project_values" ;;
  'project item-list')
    if [[ $* == *'.items[] | [('* || $* == *'field("agentstatus")'* ]]; then
      while IFS= read -r item_url; do
        [[ -n $item_url ]] || continue
        item_number=${item_url##*/}
        status=''; category=''; blocked_b64=''
        IFS=$'\t' read -r status category blocked_b64 < <(awk -F '\t' -v n="$item_number" '$1 == n {print $2 "\t" $3 "\t" $4; exit}' "$project_values" 2>/dev/null || true)
        blocked=''; [[ -z $blocked_b64 ]] || blocked=$(base64 -d <<< "$blocked_b64")
        printf '%s\x1f%s\x1fPVTI_%s\x1f%s\x1f%s\x1f%s\n' "$item_number" "$item_url" "$item_number" "$status" "$category" "$blocked"
      done < <(cat "$project_items" 2>/dev/null || true)
    elif [[ $* == *content.number* ]]; then printf 'PVTI_fake\n'; else cat "$project_items" 2>/dev/null || true; fi ;;
  'project item-add')
    if (( ${FAKE_PROJECT_FAILURES:-0} > 0 )); then
      failure_file="$FAKE_GH_ROOT/$key.project-failures"
      failures=$(cat "$failure_file" 2>/dev/null || printf '0')
      if (( failures < FAKE_PROJECT_FAILURES )); then printf '%s\n' "$((failures + 1))" > "$failure_file"; exit 1; fi
    fi
    url=''; for ((i=1; i<=$#; i++)); do [[ ${!i} == --url ]] && { j=$((i+1)); url=${!j}; }; done
    grep -Fxq -- "$url" "$project_items" 2>/dev/null || printf '%s\n' "$url" >> "$project_items"
    item_number=${url##*/}
    if ! awk -F '\t' -v n="$item_number" '$1 == n {found=1} END {exit !found}' "$project_values" 2>/dev/null; then
      printf '%s\t\t\t\n' "$item_number" >> "$project_values"
    fi
    [[ $* == *'--jq .id'* ]] && printf 'PVTI_%s\n' "${url##*/}" ;;
  'project view')
    if [[ ${FAKE_PROJECT_SCOPE_MISSING:-0} == 1 ]]; then
      printf 'GraphQL: Your token has not been granted the required scopes to execute this query. The %s scope is required. (viewer)\n' "'read:project'" >&2
      exit 1
    fi
    [[ ${FAKE_PROJECT_FAIL:-0} == 0 ]] && printf 'PVT_fake\n' ;;
  'project field-list')
    if [[ $* == *'.fields[].name'* ]]; then
      cat "$project_fields" 2>/dev/null || true
    elif [[ $* == *'$field.name'* ]]; then
      printf 'Agent status\tPVTF_status\tQueued\tPVTFO_queued\n'
      printf 'Agent status\tPVTF_status\tRunning\tPVTFO_running\n'
      printf 'Agent status\tPVTF_status\tNeeds input\tPVTFO_needs_input\n'
      printf 'Agent status\tPVTF_status\tIn review\tPVTFO_in_review\n'
      printf 'Agent status\tPVTF_status\tDone\tPVTFO_done\n'
      printf 'Agent status\tPVTF_status\tFailed\tPVTFO_failed\n'
      printf 'Agent status\tPVTF_status\tStale\tPVTFO_stale\n'
      printf 'Agent status\tPVTF_status\tBlocked\tPVTFO_blocked\n'
      printf 'Category\tPVTF_category\tImprovement\tPVTFO_improvement\n'
      printf 'Category\tPVTF_category\tFeature\tPVTFO_feature\n'
      printf 'Blocked by\tPVTF_blocked_by\t\t\n'
    else
      printf 'PVTF_fake\n'
    fi ;;
  'pr list')
    if [[ $* == *'--state merged'* ]]; then
      if [[ ${FAKE_PR_MERGED:-1} == 1 ]]; then
        head=''
        for ((i=1; i<=$#; i++)); do [[ ${!i} == --head ]] && { j=$((i+1)); head=${!j}; }; done
        oid=${FAKE_PR_HEAD_OID:-$(git rev-parse "refs/heads/$head" 2>/dev/null || true)}
        printf 'https://github.example/%s/pull/merged\t%s\n' "$slug" "$oid"
      fi
    elif [[ $* == *'--head agent/issue-6'* ]]; then
      printf 'https://github.example/%s/pull/6\n' "$slug"
    elif [[ $* == *'--state all'* ]]; then
      printf 'https://github.example/%s/pull/1\nhttps://github.example/%s/pull/2\n' "$slug" "$slug"
    fi ;;
  'issue list')
    if [[ $* == *diagnosis-finding* ]]; then
      [[ -e $diagnosis_issues ]] && printf 'https://github.example/%s/issues/diagnosis\n' "$slug"
      exit 0
    fi
    wanted=''
    for ((i=1; i<=$#; i++)); do [[ ${!i} == --label ]] && { j=$((i+1)); wanted=${!j}; }; done
    [[ -e $state ]] || exit 0
    if [[ $wanted == agent:queued ]]; then
      if [[ $* == *updatedAt* ]]; then
        awk '$2 == "queued" && $3 != "closed" && $6 != "" {print $1 "\t" $6}' "$state"
      elif [[ $* == *createdAt* ]]; then
        awk 'function prio(b,   body, val, max, rest, tok, cmd) {
          max = 0
          if (b == "" || b == "-" || b == "none") return 0
          cmd = "printf %s \"" b "\" | base64 -d 2>/dev/null"
          while ((cmd | getline body) > 0) {
            rest = body
            while (match(rest, /agentic-loop:priority[[:space:]]+[0-9]+([[:space:]]|--)/)) {
              tok = substr(rest, RSTART, RLENGTH)
              val = 0
              if (match(tok, /[0-9]+/)) val = substr(tok, RSTART, RLENGTH) + 0
              if (val >= 0 && val <= 100 && val > max) max = val
              rest = substr(rest, RSTART + RLENGTH)
            }
          }
          close(cmd)
          return max
        }
        $2 == "queued" && $3 != "closed" {
          created=($5 == "" ? $1 : $5)
          print prio($8) "\t" created "\t" $1
        }' "$state" | sort -k1,1nr -k2,2 -k3,3n | awk 'NR == 1 {print $3}'
      else
        awk '$2 == "queued" && $3 != "closed" {print $1}' "$state"
      fi
    elif [[ $wanted == agent:running ]]; then
      if [[ $* == *template* ]]; then awk '$2 == "running" && $3 != "closed" {print "#" $1 " Fake issue " $1}' "$state"; else awk '$2 == "running" && $3 != "closed" {print $1}' "$state"; fi
    elif [[ $wanted == agent:needs-input ]]; then
      awk '$2 == "needs-input" && $3 != "closed" {print $1}' "$state"
    fi ;;
  'issue create') touch "$diagnosis_issues"; printf 'https://github.example/%s/issues/diagnosis\n' "$slug" ;;
  'issue edit')
    issue=$3 target=''
    for ((i=1; i<=$#; i++)); do [[ ${!i} == --add-label ]] && { j=$((i+1)); target=${!j#agent:}; }; done
    (
      flock 9
      awk -v n="$issue" -v s="$target" '{if ($1 == n) $2=s; print}' "$state" > "$state.$$.tmp" && mv "$state.$$.tmp" "$state"
    ) 9> "$state.lock" ;;
  'issue view')
    issue=$3
    if [[ $* == *labels* ]]; then awk -v n="$issue" '$1 == n {print "agent:" $2}' "$state"
    elif [[ $* == *comments* && $* == *needs-input* ]]; then
      if tail -n 1 "$comments" 2>/dev/null | grep -Fq USER_REPLY; then printf 'true\n'; else printf 'false\n'; fi
    elif [[ $* == *comments* ]]; then tail -n 1 "$comments" 2>/dev/null || true
    elif [[ $* == *url* ]]; then printf 'https://github.example/%s/issues/%s\n' "$slug" "$issue"
    fi ;;
  'issue comment')
    issue=$3; shift 3
    body=$(encode_comment_body "$*")
    printf '%s\n' "$body" >> "$comment_bodies_log"
    printf '%s %s\n' "$issue" "$body" >> "$comments" ;;
  'issue close')
    issue=$3
    (
      flock 9
      awk -v n="$issue" '{if ($1 == n) $3="closed"; print}' "$state" > "$state.$$.tmp" && mv "$state.$$.tmp" "$state"
    ) 9> "$state.lock" ;;
  *) printf 'unexpected fake gh call: %s\n' "$*" >&2; exit 1 ;;
esac
FAKE_GH
cat > "$FAKE_BIN/codex" <<'FAKE_CODEX'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_GH_ROOT/codex-calls"
[[ ${1:-} == exec && ${2:-} == --help ]] && { printf '%s\n' '      --add-dir <DIR>'; exit 0; }
output='' worktree='' add_dirs=''
for ((i=1; i<=$#; i++)); do
  case ${!i} in
    --output-last-message) j=$((i+1)); output=${!j} ;;
    -C) j=$((i+1)); worktree=${!j} ;;
    --add-dir) j=$((i+1)); add_dirs+=" ${!j}" ;;
  esac
done
[[ -n $output ]] || exit 2
if [[ $* == *'Use $diagnose-codebase'* ]]; then
  existing=$(gh issue list --search diagnosis-finding --state all --limit 100 --json url --jq '.[].url')
  if [[ -z $existing ]]; then
    gh issue create --title '診断所見' --body 'diagnosis-finding' --label diagnosis --label category:improvement --label agent:queued >/dev/null
    "$worktree/bin/agentic-loop" sync-issue 99 >/dev/null
  fi
fi
# --add-dir is only ever passed on the workspace-write (exec) call (see
# agent.sh's plan/exec codex_args split); the plan call is --sandbox
# read-only and cannot perform Git writes in real Codex either. Gate this
# fixture the same way so a plan-stage call never falsely trips the
# missing-add-dir check as a plan failure.
plan_or_exec_workspace_write=0
for ((gi=1; gi<=$#; gi++)); do
  if [[ ${!gi} == --sandbox ]] && (( gi < $# )); then
    gj=$((gi + 1))
    [[ ${!gj} == workspace-write ]] && plan_or_exec_workspace_write=1
  fi
done
if [[ ${FAKE_CODEX_GIT_OPERATIONS:-0} == 1 ]] && (( plan_or_exec_workspace_write )); then
  expected=$(git -C "$worktree" rev-parse --path-format=absolute --git-common-dir)
  [[ " $add_dirs " == *" $expected "* ]] || { printf 'missing Git add-dir: %s (expected %s)\n' "$add_dirs" "$expected" >&2; exit 3; }
  [[ " $add_dirs " == *" $worktree/.agents "* ]] || { printf 'missing .agents add-dir: %s\n' "$add_dirs" >&2; exit 3; }
  git -C "$worktree" fetch origin main
  printf 'worker change\n' > "$worktree/worker.txt"
  git -C "$worktree" add worker.txt
  git -C "$worktree" commit --quiet -m 'worker change'
  git -C "$worktree" push --quiet origin HEAD:refs/heads/agent/issue-6
fi
if [[ ${FAKE_CODEX_GIT_RENAME:-0} == 1 ]]; then
  git -C "$worktree" mv seed.txt seed-renamed.txt 2>/dev/null || true
  git -C "$worktree" commit --quiet -m 'worker rename' 2>/dev/null || true
fi
if [[ ${FAKE_CODEX_COMMIT_ALL:-0} == 1 ]]; then
  git -C "$worktree" add -A
  git -C "$worktree" commit --quiet --allow-empty -m 'exec committed pending changes'
fi
# Per-Issue sleep override (FAKE_CODEX_SLEEP_ISSUE_<n>, keyed by the worktree's
# "issue-<n>" suffix) lets a test give one Issue a short sleep while another
# Issue claimed by the same supervisor process keeps the long FAKE_CODEX_SLEEP,
# without changing default behavior for tests that never set the override.
sleep_value=${FAKE_CODEX_SLEEP:-0}
if [[ $worktree =~ /issue-([0-9]+)$ ]]; then
  override_var="FAKE_CODEX_SLEEP_ISSUE_${BASH_REMATCH[1]}"
  sleep_value=${!override_var:-$sleep_value}
fi
# Most scenarios only need the fake provider to overlap briefly with the
# supervisor; a real second per plan and exec stage adds no coverage. Keep the
# longer values used by heartbeat, shutdown, and timeout scenarios unchanged.
[[ $sleep_value == 1 ]] && sleep_value=0.1
sleep "$sleep_value"
# Tests that exercise the completion protocol need distinct exec responses
# without making plan calls observable implementation details.  Number only
# workspace-write calls; unset slots retain the ordinary default response.
result_var=FAKE_CODEX_RESULT
workspace_write=0
for ((i=1; i<=$#; i++)); do
  if [[ ${!i} == --sandbox ]] && (( i < $# )); then
    j=$((i + 1))
    [[ ${!j} == workspace-write ]] && workspace_write=1
  fi
done
if (( workspace_write )); then
  exec_count_file="$FAKE_GH_ROOT/codex-exec-count"
  exec_count=$(($(cat "$exec_count_file" 2>/dev/null || printf 0) + 1))
  printf '%s\n' "$exec_count" > "$exec_count_file"
  result_var="FAKE_CODEX_EXEC_RESULT_$exec_count"
fi
# Runs only on the exec (workspace-write) call, never the plan call, so it
# fires exactly once per worker() run regardless of retries (Issue #132's
# postmortem worker.sh terminal-branch fixtures): the exec turn "does" the
# postmortem link/complete the way a real provider would from inside its own
# sandboxed shell, then reports completion the same way it always does.
if (( workspace_write )) && [[ ${FAKE_CODEX_POSTMORTEM_LINK:-0} == 1 ]]; then
  # shellcheck disable=SC2086 # intentional word-splitting: "ISSUE ACTION..."
  "$worktree/bin/agentic-loop" postmortem link $FAKE_CODEX_POSTMORTEM_LINK_ARGS >/dev/null
fi
if (( workspace_write )) && [[ ${FAKE_CODEX_POSTMORTEM_COMPLETE:-0} == 1 ]]; then
  "$worktree/bin/agentic-loop" postmortem complete "$FAKE_CODEX_POSTMORTEM_COMPLETE_ISSUE" >/dev/null
fi
if [[ -v $result_var ]]; then result=${!result_var}; else result=${FAKE_CODEX_RESULT:-AGENTIC_LOOP_RESULT=completed}; fi
# Realistic Codex usage-limit path: the CLI exits non-zero and writes the
# diagnostic only to stderr, leaving --output-last-message empty. Tests set
# FAKE_CODEX_EXIT (or FAKE_CODEX_EXEC_EXIT_<n> for numbered exec calls) and
# FAKE_CODEX_STDERR to exercise the real classification path.
exit_var=FAKE_CODEX_EXIT
stderr_var=FAKE_CODEX_STDERR
if (( workspace_write )); then
  if [[ -v FAKE_CODEX_EXEC_EXIT_$exec_count ]]; then exit_var="FAKE_CODEX_EXEC_EXIT_$exec_count"; fi
  if [[ -v FAKE_CODEX_EXEC_STDERR_$exec_count ]]; then stderr_var="FAKE_CODEX_EXEC_STDERR_$exec_count"; fi
fi
fake_exit=0
if [[ -v $exit_var ]]; then fake_exit=${!exit_var}; fi
if [[ -v $stderr_var && -n ${!stderr_var} ]]; then
  printf '%s\n' "${!stderr_var}" >&2
  # Empty last-message file matches real Codex on hard failure.
  : > "$output"
else
  printf '%s\n' "$result" > "$output"
fi
exit "$fake_exit"
FAKE_CODEX
cat > "$FAKE_BIN/claude" <<'FAKE_CLAUDE'
#!/usr/bin/env bash
set -euo pipefail
# claude_probe_usage_percent (Issue #251) sets AGENTIC_LOOP_PROBE=1 to
# distinguish its zero-side-effect health check from an ordinary plan/exec
# stage call. Logged separately so it never perturbs the plan/exec
# claude-json-count sequencing (FAKE_CLAUDE_RESULT_<n>) other tests rely on.
# Default mode is "unreadable" (empty output, exit 1) so every existing test
# that does not opt in keeps today's "claude has no working probe" behavior
# unchanged; a test exercising the new probe sets FAKE_CLAUDE_PROBE_MODE.
if [[ ${AGENTIC_LOOP_PROBE:-0} == 1 ]]; then
  printf '%s\n' "$*" >> "$FAKE_GH_ROOT/claude-probe-calls"
  case ${FAKE_CLAUDE_PROBE_MODE:-unreadable} in
    ok) printf '{"type":"result","is_error":false,"api_error_status":null,"result":"OK"}\n'; exit 0 ;;
    exhausted) printf '{"type":"result","is_error":true,"api_error_status":"%s","result":"%s"}\n' \
        "${FAKE_CLAUDE_PROBE_API_ERROR_STATUS:-429}" "${FAKE_CLAUDE_PROBE_RESULT:-individual spend limit}"
      exit 0 ;;
    *) exit 1 ;;
  esac
fi
printf '%s\n' "$*" >> "$FAKE_GH_ROOT/claude-calls"
[[ ${1:-} == --version || ${1:-} == --help ]] && { printf 'claude 1.0.0\n'; exit 0; }
[[ $* == *--print* ]] || { printf 'claude worker must run non-interactively\n' >&2; exit 2; }
[[ $* == *--dangerously-skip-permissions* ]] || { printf 'claude worker must not block on permissions\n' >&2; exit 2; }
# Real claude CLI contract (measured directly, Issue #273): stream-json under
# --print requires --verbose, and fails fast with no stdout otherwise.
if [[ $* == *'--output-format stream-json'* && $* != *--verbose* ]]; then
  printf 'Error: When using --print, --output-format=stream-json requires --verbose\n' >&2
  exit 1
fi
sleep "${FAKE_CLAUDE_SLEEP:-0}"
# Per-invocation result override so a test can give the plan stage and the exec
# stage different outputs (e.g. a plan that discusses rate limits, then an exec
# that completes). Each --output-format stream-json call increments a counter
# and prefers FAKE_CLAUDE_RESULT_<n>, falling back to the scalar
# FAKE_CLAUDE_RESULT.
claude_result=${FAKE_CLAUDE_RESULT:-AGENTIC_LOOP_RESULT=completed}
if [[ $* == *'--output-format stream-json'* ]]; then
  claude_count_file="$FAKE_GH_ROOT/claude-json-count"
  claude_count=$(($(cat "$claude_count_file" 2>/dev/null || printf 0) + 1))
  printf '%s\n' "$claude_count" > "$claude_count_file"
  seq_var="FAKE_CLAUDE_RESULT_$claude_count"
  if [[ -v $seq_var ]]; then claude_result=${!seq_var}; fi
fi
# Stage-level transport failures model provider failures observed in production:
# empty output, stderr-only non-zero exit, and a successful process that
# violates the JSON output contract.
case ${FAKE_CLAUDE_STAGE_MODE:-} in
  empty-nonzero) : > "$FAKE_GH_ROOT/claude-stage-mode"; exit 1 ;;
  stderr-nonzero) printf '%s\n' "${FAKE_CLAUDE_STAGE_STDERR:-provider failed}" >&2; : > "$FAKE_GH_ROOT/claude-stage-mode"; exit 1 ;;
  nonjson-zero) printf '%s\n' "${FAKE_CLAUDE_STAGE_OUTPUT:-not json}"; : > "$FAKE_GH_ROOT/claude-stage-mode"; exit 0 ;;
esac
# The Claude worker captures the final message from stdout into the result file.
# With --output-format stream-json the sentinel stays inside the terminal
# event's .result and usage fields accompany it for the token analysis record.
#
# Real Claude usage-limit path: the CLI still exits zero under --print
# --output-format stream-json but sets is_error true on the terminal event and
# reports the limit through api_error_status plus the message in .result.
# FAKE_CLAUDE_IS_ERROR=1 models that envelope so tests exercise the
# structured-error classification instead of substring-matching the plan text.
if [[ $* == *'--output-format stream-json'* ]]; then
  # Model the real stream shape (measured directly, Issue #273): an init
  # event, an assistant event, a rate_limit_event whose text would trip the
  # pool-exhaustion regex if ever copied whole into result_file, a non-JSON
  # noise line (observed in practice from MCP tool logging), then the
  # terminal result event -- the only line the worker must parse.
  printf '{"type":"system","subtype":"init"}\n'
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}\n'
  printf '{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}\n'
  printf 'Client.listTools() called but server is not initialized\n'
  # FAKE_CLAUDE_OMIT_TERMINAL=1 models a stream killed mid-run (or a CLI that
  # never emits a terminal event): stdout has only non-terminal lines, and the
  # failure surfaces on stderr instead, exercising agent_run_stage's existing
  # empty-result_file-falls-back-to-stderr path with no raw stream ever
  # entering result_file (Issue #273).
  if [[ ${FAKE_CLAUDE_OMIT_TERMINAL:-0} == 1 ]]; then
    printf 'stage killed before completion\n' >&2
    exit 1
  fi
  if [[ ${FAKE_CLAUDE_IS_ERROR:-0} == 1 ]]; then
    printf '{"type":"result","is_error":true,"api_error_status":"%s","result":"%s","usage":{"input_tokens":123,"output_tokens":45,"cache_read_input_tokens":10},"total_cost_usd":0.0123}\n' "${FAKE_CLAUDE_API_ERROR_STATUS:-429}" "$claude_result"
  else
    printf '{"type":"result","is_error":false,"api_error_status":null,"result":"%s","usage":{"input_tokens":123,"output_tokens":45,"cache_read_input_tokens":10},"total_cost_usd":0.0123}\n' "$claude_result"
  fi
else
  printf '%s\n' "$claude_result"
fi
FAKE_CLAUDE
cat > "$FAKE_BIN/opencode" <<'FAKE_OPENCODE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_GH_ROOT/opencode-calls"
[[ ${1:-} == --version ]] && { printf 'opencode 1.0.0\n'; exit 0; }
[[ ${1:-} == run ]] || { printf 'opencode: unknown command\n' >&2; exit 2; }
[[ $* == *--auto* ]] || { printf 'opencode worker must auto-approve permissions\n' >&2; exit 2; }
sleep "${FAKE_OPENCODE_SLEEP:-0}"
# Per-invocation result override for --auto runs (numbered like the Codex
# fake), so a test can sequence e.g. "overloaded" then "completed". Unset slots
# keep the ordinary default response.
result_var=FAKE_OPENCODE_RESULT
if [[ $* == *--auto* ]]; then
  auto_count_file="$FAKE_GH_ROOT/opencode-auto-count"
  auto_count=$(($(cat "$auto_count_file" 2>/dev/null || printf 0) + 1))
  printf '%s\n' "$auto_count" > "$auto_count_file"
  result_var="FAKE_OPENCODE_EXEC_RESULT_$auto_count"
  if [[ -v $result_var ]]; then
    opencode_result=${!result_var}
  else
    opencode_result=${FAKE_OPENCODE_RESULT:-AGENTIC_LOOP_RESULT=completed}
  fi
else
  opencode_result=${FAKE_OPENCODE_RESULT:-AGENTIC_LOOP_RESULT=completed}
fi
# With --format json the worker reads the sentinel from a text part and token
# telemetry from a step-finish part; otherwise it prints the plain final message.
if [[ $* == *'--format json'* ]]; then
  printf '{"part":{"type":"text","text":"%s"}}\n' "$opencode_result"
  printf '{"part":{"type":"step-finish","tokens":{"input":200,"output":50,"reasoning":5,"cache":{"read":10,"write":0}},"cost":0.02}}\n'
else
  printf '%s\n' "$opencode_result"
fi
exit_var=FAKE_OPENCODE_EXIT
if [[ $* == *--auto* && -v FAKE_OPENCODE_EXEC_EXIT_$auto_count ]]; then
  exit_var="FAKE_OPENCODE_EXEC_EXIT_$auto_count"
fi
exit "${!exit_var:-0}"
FAKE_OPENCODE
cat > "$FAKE_BIN/systemctl" <<'FAKE_SYSTEMCTL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_GH_ROOT/systemctl-calls"
FAKE_SYSTEMCTL
cat > "$FAKE_BIN/systemd-escape" <<'FAKE_SYSTEMD_ESCAPE'
#!/usr/bin/env bash
set -euo pipefail
[[ ${1:-} == --path && $# == 2 ]] || exit 2
printf '%s\n' "${2#/}" | tr '/' '-'
FAKE_SYSTEMD_ESCAPE
cat > "$FAKE_BIN/devbox" <<'FAKE_DEVBOX'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_GH_ROOT/devbox-calls"
if [[ ${1:-} == run && ${2:-} == --config && ${4:-} == -- ]]; then
  [[ ${FAKE_DEVBOX_FAIL:-0} == 0 ]] || exit 1
  config_dir=$3
  run_path="$FAKE_BIN:$TEST_HOST_PATH"
  # Simulate a real Devbox virtenv's on-disk shape (a profile symlink chain
  # pointing at a "store" directory) for any --config directory that carries
  # its own devbox.json, except PROJECT_ROOT itself: install-target.sh's
  # bootstrap of *this* script legitimately uses --config PROJECT_ROOT (see
  # install.sh), and this repository's own working tree must stay untouched by
  # the test suite. Deleting the store directory (not the symlink chain) lets
  # tests simulate nix GC realistically: the profile symlink dangles exactly
  # as it would for real.
  if [[ -f $config_dir/devbox.json && $config_dir != "$PROJECT_ROOT" ]]; then
    store_dir="$FAKE_GH_ROOT/devbox-store/${config_dir//\//_}"
    if [[ ! -d $store_dir/bin ]]; then
      real_bin=$(PATH="$TEST_HOST_PATH" command -v yq); real_bin=${real_bin%/*}
      mkdir -p "$store_dir/bin"
      for real_tool in "$real_bin"/*; do
        [[ -e $real_tool ]] || continue
        ln -sfn "$real_tool" "$store_dir/bin/${real_tool##*/}"
      done
    fi
    mkdir -p "$config_dir/.devbox/nix/profile"
    ln -sfn "$store_dir" "$config_dir/.devbox/nix/profile/default-1-link"
    ln -sfn default-1-link "$config_dir/.devbox/nix/profile/default"
    run_path="$config_dir/.devbox/nix/profile/default/bin:$run_path"
  fi
  shift 4
  PATH="$run_path" exec "$@"
fi
[[ ${FAKE_DEVBOX_FAIL:-0} == 0 ]] || exit 1
exit 0
FAKE_DEVBOX
chmod +x "$FAKE_BIN/gh" "$FAKE_BIN/codex" "$FAKE_BIN/claude" "$FAKE_BIN/opencode" "$FAKE_BIN/systemctl" "$FAKE_BIN/systemd-escape" "$FAKE_BIN/devbox"
export PATH="$FAKE_BIN:$PATH" FAKE_BIN FAKE_GH_ROOT TEST_HOST_PATH PROJECT_ROOT XDG_CONFIG_HOME="$TEST_ROOT/config"

new_repository() {
  local name=$1 target bare
  target="$TEST_ROOT/$name"
  bare="$TEST_ROOT/$name.git"
  git init --bare --quiet "$bare"
  git init --quiet -b main "$target"
  git -C "$target" config user.name Test
  git -C "$target" config user.email test@example.invalid
  git -C "$target" remote add origin "$bare"
  printf 'seed\n' > "$target/seed.txt"
  git -C "$target" add seed.txt
  git -C "$target" commit --quiet -m seed
  git -C "$target" push --quiet -u origin main
  printf '%s\n' "$target"
}

empty_repository() {
  local name=$1 target bare
  target="$TEST_ROOT/$name"
  bare="$TEST_ROOT/$name.git"
  git init --bare --quiet "$bare"
  git init --quiet -b main "$target"
  git -C "$target" config user.name Test
  git -C "$target" config user.email test@example.invalid
  git -C "$target" remote add origin "$bare"
  printf '%s\n' "$target"
}

target=$(new_repository installed-project)
state_key=$(printf '%s' "$target" | tr '/' '_')
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
[[ -x $target/bin/agentic-loop ]] || fail 'install did not add the queue CLI'
[[ -x $target/bin/agentic-loop-diagnose ]] || fail 'install did not add the manual diagnosis CLI'
[[ -f $target/.agentic-loop.toml ]] || fail 'install did not add safe defaults'
[[ -f $target/.agents/skills/diagnose-codebase/SKILL.md ]] || fail 'install did not add the diagnosis skill'
[[ $(cat "$target/.codex/config.toml") == 'approval_policy = "never"' ]] || fail 'install did not preserve external sandbox configuration'
[[ $(git -C "$target" config --get core.hooksPath) == .githooks ]] || fail 'install did not enable hooks'
[[ -f $target/docs/operations/issue-queue.md ]] || fail 'init did not install operations documentation'
[[ -f $target/docs/policies/testing.md ]] || fail 'init did not install the testing policy'
[[ -f $target/docs/policies/external-environment.md ]] || fail 'init did not install the external environment policy'
[[ -f $target/docs/policies/development-environment.md ]] || fail 'init did not install the development environment policy'
[[ -f $target/docs/policies/github-language.md ]] || fail 'init did not install the GitHub language policy'
[[ -f $target/docs/policies/validation-harness.md ]] || fail 'init did not install the validation harness policy'
[[ -f $target/docs/policies/continuous-delivery.md ]] || fail 'init did not install the continuous delivery policy'
assert_contains "$target/AGENTS.md" 'GitHub日本語運用ポリシー' 'installed agent instructions did not require Japanese GitHub content'
assert_contains "$target/AGENTS.md" '外部環境コード化ポリシー' 'installed agent instructions did not require reproducible external environments'
assert_contains "$target/docs/policies/external-environment.md" 'desired state' 'installed external environment policy lacks desired state management'
assert_contains "$target/docs/policies/external-environment.md" 'drift検出' 'installed external environment policy lacks drift detection'
assert_contains "$target/docs/policies/external-environment.md" '復旧および移行' 'installed external environment policy lacks recovery and migration requirements'
assert_contains "$target/AGENTS.md" '検証ハーネスポリシー' 'installed agent instructions did not reference the validation harness policy'
assert_contains "$target/docs/policies/validation-harness.md" 'local fast check' 'installed validation policy lacks the fast check layer'
assert_contains "$target/docs/policies/validation-harness.md" 'local full check' 'installed validation policy lacks the full check layer'
assert_contains "$target/docs/policies/validation-harness.md" 'private repository' 'installed validation policy lacks the private repository exception'
assert_contains "$target/AGENTS.md" '継続的デリバリーポリシー' 'installed agent instructions did not reference the continuous delivery policy'
# shellcheck disable=SC2016 # Backticks are literal Markdown in installed documentation.
assert_contains "$target/docs/policies/continuous-delivery.md" '`main`へのmergeを唯一の通常release trigger' 'installed delivery policy lacks the main merge trigger'
assert_contains "$target/docs/policies/continuous-delivery.md" '後方互換性を壊す変更はmajor' 'installed delivery policy lacks interface-based SemVer'
assert_contains "$target/docs/policies/continuous-delivery.md" '検証した同一artifact' 'installed delivery policy lacks artifact promotion'
assert_contains "$target/docs/policies/continuous-delivery.md" 'GitHub Release' 'installed delivery policy lacks GitHub Releases'
assert_contains "$target/docs/policies/continuous-delivery.md" '段階的反映' 'installed delivery policy lacks staged production deployment'
assert_contains "$target/docs/policies/continuous-delivery.md" 'rollback' 'installed delivery policy lacks rollback requirements'
assert_contains "$target/docs/policies/continuous-delivery.md" '短期credential' 'installed delivery policy lacks short-lived credentials'
assert_contains "$target/docs/policies/continuous-delivery.md" '二重release' 'installed delivery policy lacks idempotency requirements'
assert_contains "$target/docs/policies/continuous-delivery.md" '追加課金' 'installed delivery policy lacks cost restrictions'
assert_contains "$target/docs/policies/continuous-delivery.md" '監査証跡' 'installed delivery policy lacks audit evidence'
assert_contains "$target/docs/policies/continuous-delivery.md" '空のpipelineを要求しない' 'installed delivery policy lacks the no-op exception'
assert_contains "$target/.agentic-loop.toml" 'provider = "codex"' 'installed configuration lacks the default AI provider'
assert_contains "$target/.agentic-loop.toml" 'graphql_reserve = 500' 'installed configuration lacks the GraphQL reserve'
assert_contains "$target/.agentic-loop.toml" 'core_reserve = 500' 'installed configuration lacks the REST(core) reserve'
assert_contains "$target/.agentic-loop.toml" 'poll_max_seconds = 120' 'installed configuration lacks the idle poll backoff ceiling'
assert_contains "$target/.agentic-loop.toml" 'api_retry_attempts = 3' 'installed configuration lacks bounded REST retries'
assert_contains "$target/.agentic-loop.toml" 'worker_timeout_seconds = 14400' 'installed configuration lacks the worker hang-timeout default'
[[ -f $target/docs/decisions/0006-worker-hang-timeout.md ]] || fail 'installed repository lacks the worker hang-timeout ADR'
assert_contains "$target/docs/operations/issue-queue.md" 'GraphQLの残量・reset時刻' 'installed operations documentation lacks shared rate-limit handling'
for provider_neutral_doc in docs/operations/issue-queue.md docs/operations/codebase-diagnosis.md; do
  assert_contains "$target/$provider_neutral_doc" 'opencode' "installed $provider_neutral_doc does not document opencode as a supported provider"
done
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'in Japanese' 'installed submission skill did not require Japanese GitHub content'
assert_contains "$target/.agents/skills/diagnose-codebase/SKILL.md" 'without modifying' 'diagnosis skill did not prohibit code changes'
assert_contains "$target/AGENTS.md" '通常のbuild・変更要求' 'installed AGENTS.md lacks queue-first routing'
assert_contains "$target/AGENTS.md" 'git管理外（gitignore対象）' 'installed AGENTS.md lacks the local overlay intake exception'
assert_contains "$target/AGENTS.md" 'agent:running' 'installed AGENTS.md lacks the worker exception'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'Queue-first intake' 'installed skill lacks queue-first routing'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'git-ignored, repository-local overlay' 'installed submission skill lacks the local overlay intake exception'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'Non-recursive worker exception' 'installed skill lacks the worker exception'
assert_contains "$target/AGENTS.md" '新規Issue作成を明示した場合は重複検索を省略' 'installed AGENTS.md does not bypass duplicate search for explicit Issue creation'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'do not search for duplicates and create a new Issue' 'installed skill does not bypass duplicate search for explicit Issue creation'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'inspect only active Issues' 'installed skill lacks minimal active-Issue matching for automatic intake'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'search both open and closed Issues' 'installed skill does not preserve diagnosis duplicate checks'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'do not require it to be running for intake' 'installed skill still requires a running Supervisor for intake'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'will not be claimed until the Supervisor is started' 'installed skill does not report the stopped-Supervisor claim condition'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'final open/category/state invariants cannot be verified' 'installed skill lacks a safe fallback for unverifiable Issue state'
assert_contains "$target/docs/operations/issue-queue.md" '重複検索を呼ばず、新規作成する' 'installed docs do not bypass duplicate-search API calls for explicit Issue creation'
assert_contains "$target/docs/operations/issue-queue.md" 'open・closed Issueを検索' 'installed docs do not preserve diagnosis duplicate checks'
assert_contains "$target/docs/operations/issue-queue.md" 'Supervisorが停止中であること' 'installed docs do not require stopped-Supervisor reporting'
assert_contains "$target/docs/operations/issue-queue.md" 'Supervisor停止はこのfallbackと区別する' 'installed docs conflate a stopped Supervisor with GitHub verification failure'
assert_contains "$target/docs/operations/issue-queue.md" '安全なfallback' 'installed docs lack safe fallback'
assert_contains "$target/docs/operations/issue-queue.md" 'repository-local設定overlayの扱い' 'installed docs lack local overlay routing guidance'
timer="$XDG_CONFIG_HOME/systemd/user/agentic-loop-main-sync-$(printf '%s' "${target#/}" | tr '/' '-').timer"
service=${timer%.timer}.service
[[ -f $timer && -f $service ]] || fail 'install did not create the periodic main update units'
assert_contains "$timer" 'OnUnitActiveSec=15min' 'main update timer does not run every 15 minutes'
assert_contains "$service" "ExecStart=\"$target/.agentic-loop/update-main.sh\" sync \"$target\"" 'main update service targets the installed main worktree'
assert_contains "$FAKE_GH_ROOT/systemctl-calls" "enable --now $(basename "$timer")" 'install did not enable the main update timer'
diagnosis_timer="$XDG_CONFIG_HOME/systemd/user/agentic-loop-diagnosis-$(printf '%s' "${target#/}" | tr '/' '-').timer"
diagnosis_service=${diagnosis_timer%.timer}.service
[[ -f $diagnosis_timer && -f $diagnosis_service ]] || fail 'install did not create the periodic diagnosis units'
assert_contains "$diagnosis_timer" 'OnUnitActiveSec=7d' 'codebase diagnosis timer does not run weekly'
assert_contains "$diagnosis_service" "ExecStart=\"$target/.agentic-loop/diagnose-codebase.sh\" run \"$target\"" 'diagnosis service targets the installed repository'
assert_contains "$FAKE_GH_ROOT/systemctl-calls" "enable --now $(basename "$diagnosis_timer")" 'install did not enable the diagnosis timer'
before_diagnosis=$(git -C "$target" status --porcelain)
diagnosis_output=$("$target/bin/agentic-loop-diagnose")
[[ $diagnosis_output == AGENTIC_LOOP_RESULT=completed ]] || fail 'manual diagnosis did not report the Codex result'
[[ $(git -C "$target" status --porcelain) == "$before_diagnosis" ]] || fail 'manual diagnosis modified the repository'
assert_contains "$FAKE_GH_ROOT/codex-calls" '--sandbox read-only' 'diagnosis did not use the read-only sandbox'
# shellcheck disable=SC2016 # The dollar-prefixed value is a literal skill invocation.
assert_contains "$FAKE_GH_ROOT/codex-calls" 'Use $diagnose-codebase' 'manual diagnosis did not invoke the diagnosis skill'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'diagnosis, category:improvement, and agent:queued labels' 'diagnosis prompt did not categorize queued findings'
assert_contains "$FAKE_GH_ROOT/calls" 'issue create --title 診断所見 --body diagnosis-finding --label diagnosis --label category:improvement --label agent:queued' 'diagnosis did not create a categorized queued finding'
first_diagnosis_creates=$(grep -c $'\tissue create ' "$FAKE_GH_ROOT/calls")
second_diagnosis_output=$("$target/bin/agentic-loop-diagnose")
[[ $second_diagnosis_output == AGENTIC_LOOP_RESULT=completed ]] || fail 'repeated diagnosis did not report the Codex result'
second_diagnosis_creates=$(grep -c $'\tissue create ' "$FAKE_GH_ROOT/calls")
[[ $second_diagnosis_creates -eq $first_diagnosis_creates ]] || fail 'repeated diagnosis created a duplicate Issue'
grep -Fq $'label create diagnosis' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the diagnosis label'
# shellcheck disable=SC2016 # Backticks are literal Markdown in installed documentation.
assert_contains "$target/.agents/skills/diagnose-codebase/SKILL.md" '`diagnosis`, `category:improvement`, and `agent:queued` labels' 'installed diagnosis skill did not categorize findings'
# shellcheck disable=SC2016 # Backticks are literal Markdown in installed documentation.
assert_contains "$target/docs/operations/codebase-diagnosis.md" '`diagnosis`、`category:improvement`、`agent:queued`' 'installed diagnosis docs did not describe categorized queueing'
assert_contains "$target/.agentic-loop/diagnose-codebase.sh" 'diagnosis, category:improvement, and agent:queued labels' 'installed diagnosis prompt did not request categorized queueing'

# Diagnosis honors the configured provider (agent.diagnose.provider).
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.bak"
printf '[agent.diagnose]\nprovider = "opencode"\n' > "$target/.agentic-loop.toml"
: > "$FAKE_GH_ROOT/opencode-calls"
"$target/bin/agentic-loop-diagnose" >/dev/null
# shellcheck disable=SC2016 # The dollar-prefixed value is a literal skill invocation.
assert_contains "$FAKE_GH_ROOT/opencode-calls" 'Use $diagnose-codebase' 'diagnosis did not honor the configured opencode provider'
mv "$target/.agentic-loop.toml.bak" "$target/.agentic-loop.toml"

git -C "$target" add .
git -C "$target" commit --quiet -m install
git -C "$target" push --quiet

publisher="$TEST_ROOT/publisher"
git clone --quiet --branch main "$TEST_ROOT/installed-project.git" "$publisher"
git -C "$publisher" config user.name Test
git -C "$publisher" config user.email test@example.invalid
printf 'remote update\n' > "$publisher/remote.txt"
git -C "$publisher" add remote.txt
git -C "$publisher" commit --quiet -m update
git -C "$publisher" push --quiet
"$target/.agentic-loop/update-main.sh" sync "$target"
[[ -f $target/remote.txt ]] || fail 'periodic updater did not fast-forward main'
same_head=$(git -C "$target" rev-parse HEAD)
"$target/.agentic-loop/update-main.sh" sync "$target"
[[ $(git -C "$target" rev-parse HEAD) == "$same_head" ]] || fail 'periodic updater changed an already synchronized main'
# A machine-generated manifest rewrite (what install/upgrade leave behind) must
# not stall main sync: fast-forward proceeds and preserves the rewritten
# manifest, so the installed-revision record is never silently dropped.
yq -p json -o json '.installed_at = 1' "$target/.agentic-loop/manifest.json" > "$target/.agentic-loop/manifest.json.new"
mv "$target/.agentic-loop/manifest.json.new" "$target/.agentic-loop/manifest.json"
[[ $(git -C "$target" status --porcelain) == ' M .agentic-loop/manifest.json' ]] || fail 'fixture: expected a manifest-only dirty state'
printf 'second remote update\n' > "$publisher/remote2.txt"
git -C "$publisher" add remote2.txt
git -C "$publisher" commit --quiet -m 'second update'
git -C "$publisher" push --quiet
manifest_before=$(cat "$target/.agentic-loop/manifest.json")
"$target/.agentic-loop/update-main.sh" sync "$target"
[[ -f $target/remote2.txt ]] || fail 'periodic updater did not fast-forward main with a manifest-only dirty state'
[[ $(cat "$target/.agentic-loop/manifest.json") == "$manifest_before" ]] || fail 'sync dropped the locally rewritten manifest'
# A manifest rewrite combined with any other user change is still refused.
printf 'user work\n' > "$target/user.txt"
if "$target/.agentic-loop/update-main.sh" sync "$target" >/dev/null 2>&1; then fail 'periodic updater accepted user changes alongside a rewritten manifest'; fi
rm "$target/user.txt"
# Incoming manifest changes over a locally rewritten manifest have no safe
# automatic resolution: refused without touching HEAD.
printf '\n# upstream rewrite\n' >> "$publisher/.agentic-loop/manifest.json"
git -C "$publisher" add .agentic-loop/manifest.json
git -C "$publisher" commit --quiet -m 'upstream manifest rewrite'
git -C "$publisher" push --quiet
before_head=$(git -C "$target" rev-parse HEAD)
if "$target/.agentic-loop/update-main.sh" sync "$target" >/dev/null 2>&1; then fail 'periodic updater merged an incoming manifest rewrite over a locally rewritten manifest'; fi
[[ $(git -C "$target" rev-parse HEAD) == "$before_head" ]] || fail 'refused sync still changed HEAD'
printf 'local work\n' > "$target/local.txt"
if "$target/.agentic-loop/update-main.sh" sync "$target" >/dev/null 2>&1; then fail 'periodic updater accepted a dirty main worktree'; fi
rm "$target/local.txt"
printf 'local commit\n' > "$target/local.txt"
git -C "$target" add local.txt
git -C "$target" commit --quiet -m 'local update'
before_head=$(git -C "$target" rev-parse HEAD)
before_status=$(git -C "$target" status --porcelain)
if "$target/.agentic-loop/update-main.sh" sync "$target" >/dev/null 2>&1; then fail 'periodic updater accepted a main branch ahead of origin/main'; fi
[[ $(git -C "$target" rev-parse HEAD) == "$before_head" ]] || fail 'periodic updater changed HEAD for a main branch ahead of origin/main'
[[ $(git -C "$target" status --porcelain) == "$before_status" ]] || fail 'periodic updater changed the worktree for a main branch ahead of origin/main'
printf 'diverging remote update\n' > "$publisher/diverged.txt"
git -C "$publisher" add diverged.txt
git -C "$publisher" commit --quiet -m 'diverging update'
git -C "$publisher" push --quiet
before_head=$(git -C "$target" rev-parse HEAD)
before_status=$(git -C "$target" status --porcelain)
if "$target/.agentic-loop/update-main.sh" sync "$target" >/dev/null 2>&1; then fail 'periodic updater accepted a main branch diverged from origin/main'; fi
[[ $(git -C "$target" rev-parse HEAD) == "$before_head" ]] || fail 'periodic updater changed HEAD for a main branch diverged from origin/main'
[[ $(git -C "$target" status --porcelain) == "$before_status" ]] || fail 'periodic updater changed the worktree for a main branch diverged from origin/main'
git -C "$target" reset --quiet --hard refs/remotes/origin/main
printf '88 inbox open none 2025-01-01T00:00:00Z\n89 inbox closed none 2025-01-02T00:00:00Z\n' > "$FAKE_GH_ROOT/$state_key.state"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
# A reinstall on a clean main leaves exactly one machine-generated worktree
# change: the manifest recording the applied revision. That is the state main
# sync tolerates (verified above), so install never silently stalls sync.
[[ $(git -C "$target" status --porcelain) == ' M .agentic-loop/manifest.json' ]] || fail 'reinstall left unexpected worktree changes beyond the manifest'
[[ $(yq -p json -o yaml '.source.revision // ""' "$target/.agentic-loop/manifest.json" 2>/dev/null) =~ ^[0-9a-f]{40}$ ]] || fail 'reinstall manifest did not record the applied revision'
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/reinstall-calls.log"
[[ $(grep -c $'\tapi graphql ' "$TEST_ROOT/reinstall-calls.log" || true) -eq 1 ]] || fail 'reinstall repeated GraphQL work beyond its permission check'
[[ $(grep -Ec $'\tproject (list|view|field-list|link|field-create|item-add|item-edit)' "$TEST_ROOT/reinstall-calls.log" || true) -eq 0 ]] || fail 'reinstall scanned or mutated the existing Project'
[[ $(grep -c $'\tapi repos/.*/issues/[0-9]*/comments ' "$TEST_ROOT/reinstall-calls.log" || true) -eq 0 ]] || fail 'reinstall reconciled queued Issue comments synchronously'
project_creates=$(grep -c $'project create' "$FAKE_GH_ROOT/calls" || true)
[[ $project_creates -eq 1 ]] || { sed -n '1,120p' "$FAKE_GH_ROOT/calls" >&2; fail "reinstall created the Project $project_creates times"; }
view_creates=$(grep -c $'\tapi graphql -f query=mutation($projectId: ID!, $name: String!)' "$FAKE_GH_ROOT/calls" || true)
[[ $view_creates -eq 14 ]] || { sed -n '1,220p' "$FAKE_GH_ROOT/calls" >&2; fail "reinstall created the Project views $view_creates times"; }
for view in 'Triage' 'Queue' 'Hierarchy' 'Active' 'Paused' 'Needs input' 'Recovery' 'Recently completed' 'Open PRs' 'Closed PRs' 'All open issues' 'All closed issues'; do
  [[ $(awk -F '\t' -v name="$view" '$2 == name {count++} END {print count+0}' "$FAKE_GH_ROOT/$state_key.views") -eq 1 ]] || fail "Project view is not idempotent: $view"
done
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open no:category' 'Triage view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open label:"agent:queued"' 'Queue view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open' 'Hierarchy view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open label:"agent:running","agent:in-review"' 'Active view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open label:"agent:needs-input"' 'Needs input view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue label:"agent:failed","agent:stale"' 'Recovery view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue label:"agent:completed" updated:@today-30d' 'Recently completed view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:pr is:open' 'Open PRs view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:pr is:closed' 'Closed PRs view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open' 'All open issues view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:closed' 'All closed issues view filter was not configured'
if grep -Fq $'\tproject item-list ' "$FAKE_GH_ROOT/calls"; then fail 'install scanned every Project item'; fi

# A converged setup reads all views once and performs no mutation or content
# backfill.
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
AGENTIC_LOOP_SKIP_START=1 "$target/bin/agentic-loop" setup >/dev/null
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/converged-setup-calls.log"
[[ $(grep -c 'views(first: 100)' "$TEST_ROOT/converged-setup-calls.log" || true) -eq 1 ]] || fail 'converged setup did not consolidate the Project view query'
[[ $(grep -c 'updateProjectV2View' "$TEST_ROOT/converged-setup-calls.log" || true) -eq 0 ]] || fail 'converged setup rewrote unchanged Project view filters'
[[ $(grep -c $'\tlabel create ' "$TEST_ROOT/converged-setup-calls.log" || true) -eq 0 ]] || fail 'converged setup rewrote unchanged Labels'
[[ $(grep -c $'\tproject item-list ' "$TEST_ROOT/converged-setup-calls.log" || true) -eq 0 ]] || fail 'converged setup scanned every Project item'
[[ $(grep -Ec $'\tproject (link|field-create|item-add|item-edit)' "$TEST_ROOT/converged-setup-calls.log" || true) -eq 0 ]] || fail 'converged setup performed a Project mutation'

# setup_project_migrate_status_options (Issue #153): the migration must call
# the GraphQL mutation that actually exists (updateProjectV2Field with a
# singleSelectOptions list), not the nonexistent updateProjectV2SingleSelectField,
# and it must resubmit every pre-existing option's id so the rewrite does not
# reset item field values. This isolates the function with its own fake `gh`
# (shadowing the FAKE_GH_ROOT harness for the duration of the subshell only)
# since neither the option-identity payload nor a legacy pre-migration option
# set is exercised by the install.sh-driven scenarios above.
(
  set -euo pipefail
  # shellcheck source=bin/lib/agentic-loop/setup.sh
  . "$PROJECT_ROOT/bin/lib/agentic-loop/setup.sh"

  migrate_calls=$(mktemp)
  current_options=''
  # shellcheck disable=SC2317
  gh() {
    printf '%s\n' "$*" >> "$migrate_calls"
    case "$1 $2" in
      'project view') printf 'PVT_test\n' ;;
      'project field-list') printf 'PVTF_status\n' ;;
      'api graphql')
        if [[ $* == *updateProjectV2Field* ]]; then :
        elif [[ $* == *'options { id name color description }'* ]]; then printf '%s\n' "$current_options"; fi ;;
    esac
  }

  # Legacy Project: only the original 9 options exist (Blocked already healed,
  # the other 7 required statuses missing), mirroring the drift Issue #153
  # reports.
  current_options=$(printf '%s\n' \
    'PVTFO_1	Inbox	GRAY	' \
    'PVTFO_2	Queued	GRAY	' \
    'PVTFO_3	Running	GRAY	' \
    'PVTFO_4	Needs input	GRAY	' \
    'PVTFO_5	In review	GRAY	' \
    'PVTFO_6	Done	GRAY	' \
    'PVTFO_7	Failed	GRAY	' \
    'PVTFO_8	Stale	GRAY	' \
    'PVTFO_9	Blocked	GRAY	Waiting on unresolved Issue dependencies')
  : > "$migrate_calls"
  setup_project_migrate_status_options 42 acme

  grep -Fq 'updateProjectV2SingleSelectField' "$migrate_calls" && fail 'status option migration still calls the nonexistent updateProjectV2SingleSelectField mutation'
  mutation_call=$(grep -F 'updateProjectV2Field(input:' "$migrate_calls") || fail 'status option migration did not call updateProjectV2Field for a Project missing required options'
  grep -Fq 'singleSelectOptions:' <<< "$mutation_call" || fail 'status option migration mutation did not use the singleSelectOptions input field'
  for existing_id in PVTFO_1 PVTFO_2 PVTFO_3 PVTFO_4 PVTFO_5 PVTFO_6 PVTFO_7 PVTFO_8 PVTFO_9; do
    grep -Fq "id: \"$existing_id\"" <<< "$mutation_call" || fail "status option migration dropped the existing option id $existing_id (would reset item field values)"
  done
  for required in Stopping Blocked Paused Parked Cancelled Superseded Duplicate Merged; do
    grep -Fq "name: \"$required\"" <<< "$mutation_call" || fail "status option migration mutation is missing the required option $required"
  done

  # Converged Project: already has the full 16-option set setup_project
  # creates, so a repeat run must not attempt any mutation (idempotent).
  current_options=$(printf '%s\n' \
    'PVTFO_1	Inbox	GRAY	' \
    'PVTFO_2	Queued	GRAY	' \
    'PVTFO_3	Running	GRAY	' \
    'PVTFO_4	Needs input	GRAY	' \
    'PVTFO_5	In review	GRAY	' \
    'PVTFO_6	Stopping	YELLOW	Authorized disposal is draining an active worker' \
    'PVTFO_7	Done	GRAY	' \
    'PVTFO_8	Failed	GRAY	' \
    'PVTFO_9	Parked	RED	Retry budget exhausted; waiting for human triage' \
    'PVTFO_10	Stale	GRAY	' \
    'PVTFO_11	Blocked	GRAY	Waiting on unresolved Issue dependencies' \
    'PVTFO_12	Paused	BLUE	Execution paused by an authorized operator' \
    'PVTFO_13	Cancelled	GRAY	Requirement was withdrawn' \
    'PVTFO_14	Superseded	PURPLE	Continued by a successor Issue' \
    'PVTFO_15	Duplicate	BLUE	Duplicates another Issue' \
    'PVTFO_16	Merged	GREEN	Consolidated into another Issue')
  : > "$migrate_calls"
  setup_project_migrate_status_options 42 acme
  grep -Fq 'updateProjectV2Field' "$migrate_calls" && fail 'status option migration mutated an already-converged 16-option Project'
  rm -f "$migrate_calls"
  true
)

# An active Project member is reread on an idle supervisor poll so external
# field drift can be repaired. A converged member still performs no mutation.
printf '92 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
printf 'https://github.com/acme/installed-project/issues/92\n' >> "$FAKE_GH_ROOT/$state_key.project-items"
printf '92\tNeeds input\tImprovement\t\n' >> "$FAKE_GH_ROOT/$state_key.project-values"
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
FAKE_CORE_REMAINING=499 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/converged-idle-project-calls.log"
if [[ $(grep -c $'\tproject item-edit ' "$TEST_ROOT/converged-idle-project-calls.log" || true) -ne 0 ]]; then
  cat "$TEST_ROOT/converged-idle-project-calls.log" >&2
  fail 'idle supervisor rewrote converged Project fields'
fi

[[ $(grep -c 'projectItems(first:20' "$TEST_ROOT/converged-idle-project-calls.log" || true) -ge 1 ]] || fail 'idle supervisor did not reread Project membership for drift detection'
[[ $(grep -Ec $'\tproject (item-add|item-edit)' "$TEST_ROOT/converged-idle-project-calls.log" || true) -eq 0 ]] || fail 'idle supervisor mutated Project data for a valid queued Issue'

# Installation remains available while GraphQL is exhausted and does not make
# a doomed permission query or enter Project setup.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
FAKE_GRAPHQL_REMAINING=0 AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/exhausted-install-calls.log"
[[ $(grep -c $'\tapi graphql ' "$TEST_ROOT/exhausted-install-calls.log" || true) -eq 0 ]] || fail 'install queried GraphQL after observing exhausted quota'

# Intake synchronization is immediate, idempotent, and persists temporary Projects failures for reconciliation.
rm -f "$FAKE_GH_ROOT/$state_key.project-failures" "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
FAKE_PROJECT_FAILURES=1 FAKE_GRAPHQL_REMAINING=5000 "$target/bin/agentic-loop" sync-issue 91 >/dev/null
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/sync-issue-project-calls.log"
[[ $(grep -c $'\tproject item-list ' "$TEST_ROOT/sync-issue-project-calls.log" || true) -eq 0 ]] || fail 'one Issue sync fetched the complete Project item list'
[[ $(grep -c 'projectItems(first:20' "$TEST_ROOT/sync-issue-project-calls.log" || true) -le 1 ]] || fail 'one Issue sync repeated its bounded Project membership query'
[[ $(grep -c $'\tproject field-list ' "$TEST_ROOT/sync-issue-project-calls.log" || true) -le 1 ]] || fail 'one Issue sync fetched Project fields repeatedly'
pending_project="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/project-pending"
assert_contains "$pending_project" '91' 'temporary Project failure was not persisted as an Issue hint'
pending_project_since="${pending_project}.since"
[[ -r $pending_project_since ]] || fail 'the first pending Project retry did not record a .since timestamp'
first_since=$(cat "$pending_project_since")
[[ $first_since =~ ^[0-9]+$ ]] || fail '.since did not record a numeric epoch'
FAKE_PROJECT_FAILURES=1 "$target/bin/agentic-loop" sync-issue 91 >/dev/null
# .since must keep recording the oldest unresolved retry, not reset on
# every touch of project-pending (regression: status used to read
# project-pending's own mtime, which reconcile_pending_project's awk+mv ack
# and every enqueue both reset).
[[ $(cat "$pending_project_since") == "$first_since" ]] || fail '.since advanced while the pending entry was still unresolved'
[[ $(grep -Fxc 'https://github.com/acme/installed-project/issues/91' "$FAKE_GH_ROOT/$state_key.project-items") -eq 1 ]] || fail 'repeated immediate synchronization duplicated the Issue item'
printf '91 needs-input open none 2026-01-01T00:00:00Z none improvement\n92 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
if [[ -e $pending_project ]] && grep -Fxq '91' "$pending_project"; then
  fail 'successful reconciliation did not acknowledge the Project retry hint'
fi

# Crash-safety (E3): a failed reconciliation never bulk-moves the queue.  The
# offending entry survives, and a later successful pass acknowledges it.  A
# re-enqueue that arrives while an Issue is in flight is also retained (never
# deduplicated away), so an event between read and ack cannot be lost.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit" "$pending_project"
printf '93 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
printf 'https://github.com/acme/installed-project/issues/93\n' >> "$FAKE_GH_ROOT/$state_key.project-items"
printf '93\tQueued\tImprovement\t\n' >> "$FAKE_GH_ROOT/$state_key.project-values"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
FAKE_PROJECT_CONTENT_FAIL=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/crash-fail-calls.log"
grep -Fxq '93' "$pending_project" || fail 'failed reconciliation dropped the pending entry'
[[ $(grep -c $'\tproject item-edit ' "$TEST_ROOT/crash-fail-calls.log" || true) -eq 0 ]] || fail 'failed reconciliation still mutated Project data'
# A duplicate appended while the entry is in flight is kept for the next pass.
printf '93\n' > "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/project-pending.inflight"
"$target/bin/agentic-loop" sync-issue 93 >/dev/null
[[ $(grep -c '^93$' "$pending_project") -ge 2 ]] || fail 'in-flight re-enqueue was deduplicated away'
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
if [[ -e $pending_project ]] && grep -Fxq '93' "$pending_project"; then
  cat "$pending_project" >&2
  fail 'recovered reconciliation did not acknowledge the pending entry'
fi

# Legacy hint lines (issue #268): before #115 the queue held whole events
# ("conflict 93 <b64>", "state 93 running") instead of bare Issue numbers.
# Those lines matched nothing, were never acknowledged, and kept
# project-sync-pending displayed forever even once every real entry drained.
# A line carrying no Issue at all is unactionable and must not pin the queue
# either.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit" "$pending_project"
printf '93 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
printf 'conflict 93 Cg==\nstate 93 running\nnot-an-issue\n' > "$pending_project"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
if [[ -e $pending_project ]]; then
  cat "$pending_project" >&2
  fail 'legacy/unactionable Project hints were never acknowledged and pinned project-sync-pending on'
fi

# Clearing a Blocked by (issue #268): once an Issue's blocker is gone the
# desired text is empty, and gh accepts a removal only as --clear ("--text ''"
# is rejected with "no changes to make").  Such an Issue therefore failed
# reconciliation on every poll and stayed in project-pending forever.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit" "$pending_project"
printf '98 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
printf 'https://github.com/acme/installed-project/issues/98\n' >> "$FAKE_GH_ROOT/$state_key.project-items"
printf '98\tNeeds input\tImprovement\t%s\n' "$(printf '依存: #7' | base64 -w0)" >> "$FAKE_GH_ROOT/$state_key.project-values"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/blocked-clear-calls.log"
[[ $(grep -c -- '--clear' "$TEST_ROOT/blocked-clear-calls.log" || true) -ge 1 ]] || fail 'a blocker that no longer applies was not removed with --clear'
last_98_blocked=$(awk -F '\t' -v n=98 '$1 == n { v = $4 } END { print v }' "$FAKE_GH_ROOT/$state_key.project-values")
[[ -z $last_98_blocked ]] || fail 'the stale Blocked by text survived reconciliation'
if [[ -e $pending_project ]] && grep -Fxq '98' "$pending_project"; then
  cat "$pending_project" >&2
  fail 'an Issue whose blocker cleared was never acknowledged and stayed pending forever'
fi
# Restore the snapshot the following E4 case reads.
printf '93 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"

# API failure is distinct from "not a member" (E4): a GraphQL error performs no
# item-add/item-edit and the hint is retried, not acknowledged as a no-op.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit" "$pending_project"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
FAKE_PROJECT_CONTENT_FAIL=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/api-fail-vs-nonmember-calls.log"
[[ $(grep -c $'\tproject item-add ' "$TEST_ROOT/api-fail-vs-nonmember-calls.log" || true) -eq 0 ]] || fail 'GraphQL error triggered an item-add'
[[ $(grep -c $'\tproject item-edit ' "$TEST_ROOT/api-fail-vs-nonmember-calls.log" || true) -eq 0 ]] || fail 'GraphQL error triggered an item-edit'
grep -Fxq '93' "$pending_project" || fail 'GraphQL error was not persisted for retry'
# A successful empty ("not a member") result is acknowledged as a no-op: it is
# not item-added and the hint is not retried forever.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit" "$pending_project"
printf '94 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/nonmember-calls.log"
[[ $(grep -c $'\tproject item-add ' "$TEST_ROOT/nonmember-calls.log" || true) -eq 0 ]] || fail 'field reconciler item-added a non-member'
if [[ -e $pending_project ]] && grep -Fxq '94' "$pending_project"; then
  fail 'not-a-member result was not acknowledged as a no-op'
fi

# Pagination (E5): a member whose item only appears on the second page is
# resolved through the cursor without ever scanning the whole Project.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit" "$pending_project" "$FAKE_GH_ROOT/$state_key.project-page2"
printf '95 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
printf 'https://github.com/acme/installed-project/issues/95\n' >> "$FAKE_GH_ROOT/$state_key.project-items"
printf '95\tQueued\tImprovement\t\n' >> "$FAKE_GH_ROOT/$state_key.project-values"
printf '95\n' > "$FAKE_GH_ROOT/$state_key.project-page2"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/paging-calls.log"
[[ $(grep -c $'\tproject item-list ' "$TEST_ROOT/paging-calls.log" || true) -eq 0 ]] || fail 'paginated resolve scanned the whole Project item list'
[[ $(grep -c 'cursor=page2cursor' "$TEST_ROOT/paging-calls.log" || true) -ge 1 ]] || fail 'page-2 cursor was not followed'
[[ $(grep -c $'\tproject item-edit ' "$TEST_ROOT/paging-calls.log" || true) -ge 1 ]] || fail 'page-2 member was not reconciled'

# External drift (E6): a Project field diverged outside the loop is repaired on
# the next supervisor pass and not touched again once converged.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit" "$pending_project" "$FAKE_GH_ROOT/$state_key.project-page2"
printf '96 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
printf 'https://github.com/acme/installed-project/issues/96\n' >> "$FAKE_GH_ROOT/$state_key.project-items"
printf '96\tQueued\tImprovement\t\n' >> "$FAKE_GH_ROOT/$state_key.project-values"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/drift-repair-calls.log"
[[ $(grep -c $'\tproject item-edit ' "$TEST_ROOT/drift-repair-calls.log" || true) -ge 1 ]] || fail 'external Project drift was not repaired'
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/drift-converged-calls.log"
[[ $(grep -c $'\tproject item-edit ' "$TEST_ROOT/drift-converged-calls.log" || true) -eq 0 ]] || fail 'converged member was edited again'

# Hint reconstruction (E7): deleting the local project-pending does not lose
# convergence; the supervisor rebuilds hints from the Label source of truth.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit" "$pending_project"
printf '97 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
printf 'https://github.com/acme/installed-project/issues/97\n' >> "$FAKE_GH_ROOT/$state_key.project-items"
printf '97\tQueued\tImprovement\t\n' >> "$FAKE_GH_ROOT/$state_key.project-values"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/hints-rebuild-calls.log"
[[ $(grep -c $'\tproject item-edit ' "$TEST_ROOT/hints-rebuild-calls.log" || true) -ge 1 ]] || fail 'deleted hint was not rebuilt and reconverged'
[[ ! -e "$pending_project" ]] || fail 'a converged open-Issue drift probe leaked into project-pending'
[[ ! -s "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/project-probes" ]] || fail 'a completed drift probe was not acknowledged'

# Bounded degradation (issue #195): while one Issue's Projects reconciliation
# fails permanently, project-pending must stay bounded (not grow every poll),
# a permanently failing Issue must not block Project reconciliation of other
# Issues in the same poll, and once the permanent failure clears the bounded
# backlog must converge within very few polls (not one ack per historical
# duplicate).
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit" "$pending_project"
printf '910 needs-input open none 2026-01-01T00:00:00Z none improvement\n911 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
printf 'https://github.com/acme/installed-project/issues/910\nhttps://github.com/acme/installed-project/issues/911\n' >> "$FAKE_GH_ROOT/$state_key.project-items"
printf '910\tNeeds input\tImprovement\t\n911\tQueued\tImprovement\t\n' >> "$FAKE_GH_ROOT/$state_key.project-values"
poll=0
while (( poll < 4 )); do
  FAKE_PROJECT_CONTENT_FAIL_ISSUE=910 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise >/dev/null
  poll=$((poll + 1))
done
occurrences_910=$(grep -Fxc -- '910' "$pending_project" 2>/dev/null); occurrences_910=${occurrences_910:-0}
(( occurrences_910 <= 2 )) || fail "a permanently failing Issue made project-pending grow without bound (910 occurrences=$occurrences_910 after $poll polls)"
last_911_status=$(awk -F '\t' -v n=911 '$1 == n { v = $2 } END { print v }' "$FAKE_GH_ROOT/$state_key.project-values")
[[ $last_911_status == 'Needs input' ]] || fail 'a permanently failing Issue blocked Project reconciliation of another Issue in the same poll'
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise >/dev/null
if [[ -e $pending_project ]] && grep -Fxq -- '910' "$pending_project"; then
  cat "$pending_project" >&2
  fail 'recovered Projects synchronization did not converge the bounded backlog within one poll'
fi
printf '97 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"
rm -f "$pending_project"

# Projects APIの一時的または権限制約による失敗はIssueキューのsetupを停止しない。
FAKE_PROJECT_VIEW_UPDATE_FAILURE=1 AGENTIC_LOOP_SKIP_START=1 "$target/bin/agentic-loop" setup >/dev/null

write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
"$target/bin/agentic-loop" start
first_pid="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/supervisor.pid"
first_pid=$(cat "$first_pid")
supervisor_service="$XDG_CONFIG_HOME/systemd/user/agentic-loop-supervisor-$(printf '%s' "${target#/}" | tr '/' '-').service"
[[ -f $supervisor_service ]] || fail 'start did not install the repository supervisor service'
assert_contains "$supervisor_service" 'Restart=on-failure' 'supervisor service does not restart after an unexpected exit'
assert_contains "$supervisor_service" "ExecStart=$FAKE_BIN/devbox run -- $target/bin/agentic-loop _service" 'supervisor service does not enter the pinned Devbox environment'
assert_contains "$supervisor_service" "Environment=PATH=$FAKE_BIN:" 'supervisor service PATH does not include the verified Codex directory'
assert_contains "$FAKE_GH_ROOT/systemctl-calls" "enable $(basename "$supervisor_service")" 'supervisor service was not enabled'
"$target/bin/agentic-loop" start
[[ $(cat "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/supervisor.pid") == "$first_pid" ]] || fail 'start duplicated the supervisor'
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'running' <<< "$status_output" || fail 'status did not show the supervisor'

# Doctor is read-only, reports a healthy installation, and provides valid JSON.
before_doctor=$(git -C "$target" status --porcelain)
doctor_output=$("$target/bin/agentic-loop" doctor)
grep -Fq '[成功] GitHub認証' <<< "$doctor_output" || fail 'doctor did not report healthy GitHub authentication'
grep -Fq '失敗=0' <<< "$doctor_output" || fail 'doctor did not report the healthy state'
grep -Fq '[成功] GitHub Project同期' <<< "$doctor_output" || fail 'doctor false-warned about Project sync on a healthy (superset option, real view names) configuration'
doctor_json=$("$target/bin/agentic-loop" doctor --format json)
[[ $doctor_json == '{"schema_version":1,"summary":{"success":'*'"failure":0},"checks":['*']}' ]] || fail 'doctor JSON is not machine-readable'
[[ $(git -C "$target" status --porcelain) == "$before_doctor" ]] || fail 'doctor modified the repository'

if FAKE_GH_AUTH_FAIL=1 "$target/bin/agentic-loop" doctor >/tmp/agentic-loop-doctor-auth.$$ 2>/dev/null; then fail 'doctor accepted missing GitHub authentication'; fi
grep -Fq '[失敗] GitHub認証' /tmp/agentic-loop-doctor-auth.$$ || fail 'doctor did not classify missing authentication'
rm -f /tmp/agentic-loop-doctor-auth.$$

FAKE_PROJECT_FAIL=1 "$target/bin/agentic-loop" doctor > /tmp/agentic-loop-doctor-project.$$ || fail 'optional Project drift failed doctor'
grep -Fq '[警告] GitHub Project同期' /tmp/agentic-loop-doctor-project.$$ || fail 'doctor did not warn about Project drift'
grep -Fq 'bin/agentic-loop setup' /tmp/agentic-loop-doctor-project.$$ || fail 'doctor did not recommend setup for Project drift'
grep -Fq 'gh auth refresh' /tmp/agentic-loop-doctor-project.$$ && fail 'doctor recommended a reauth command for a non-scope Project drift'
rm -f /tmp/agentic-loop-doctor-project.$$

# Field/View drift and a missing Projects scope are distinguishable causes
# (issue #194): only the scope case names the exact interactive command.
FAKE_PROJECT_SCOPE_MISSING=1 "$target/bin/agentic-loop" doctor > /tmp/agentic-loop-doctor-scope.$$ || fail 'missing Projects scope failed doctor'
grep -Fq '[警告] GitHub Project同期' /tmp/agentic-loop-doctor-scope.$$ || fail 'doctor did not warn about the missing Projects scope'
grep -Fq 'gh auth refresh -s project,read:project --hostname github.com' /tmp/agentic-loop-doctor-scope.$$ || fail 'doctor did not name the exact interactive reauth command'
grep -Fq '非対話' /tmp/agentic-loop-doctor-scope.$$ || fail 'doctor did not note that the reauth command needs an interactive terminal'
grep -Fq 'bin/agentic-loop setup' /tmp/agentic-loop-doctor-scope.$$ || fail 'doctor did not follow the reauth command with the setup step'
rm -f /tmp/agentic-loop-doctor-scope.$$

# `status` deliberately stays a cheap, pure-read command (see the idle/
# progress status scenarios below: at most 2 REST(core) reads, no GraphQL/
# Projects/rate_limit calls even when idle), so the cause-specific Project
# sync remediation lives in `doctor` only, not `status`.

mv "$diagnosis_timer" "$diagnosis_timer.missing"
"$target/bin/agentic-loop" doctor > /tmp/agentic-loop-doctor-timer.$$ || fail 'optional missing timer failed doctor'
grep -Fq '[警告] 定期診断timer' /tmp/agentic-loop-doctor-timer.$$ || fail 'doctor did not warn about a missing timer'
mv "$diagnosis_timer.missing" "$diagnosis_timer"
rm -f /tmp/agentic-loop-doctor-timer.$$

residual_root="$(dirname "$target")/$(basename "$target")-worktrees"
git -C "$target" branch agent/issue-999
git -C "$target" worktree add --quiet "$residual_root/issue-999" agent/issue-999
"$target/bin/agentic-loop" doctor > /tmp/agentic-loop-doctor-residual.$$ || fail 'residual state warning failed doctor'
grep -Fq '[警告] 残存状態' /tmp/agentic-loop-doctor-residual.$$ || fail 'doctor did not warn about a residual worktree'
git -C "$target" worktree remove "$residual_root/issue-999"
git -C "$target" branch -d agent/issue-999 >/dev/null
rm -f /tmp/agentic-loop-doctor-residual.$$

"$target/bin/agentic-loop" stop
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'stopped' <<< "$status_output" || fail 'stop did not drain the supervisor'
if "$target/bin/agentic-loop" doctor > /tmp/agentic-loop-doctor-stopped.$$; then fail 'doctor accepted a stopped Supervisor'; fi
grep -Fq '[失敗] Supervisor' /tmp/agentic-loop-doctor-stopped.$$ || fail 'doctor did not classify a stopped Supervisor'
rm -f /tmp/agentic-loop-doctor-stopped.$$

cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.valid"
write_queue_config "$target/.agentic-loop.toml" MAX_WORKERS=invalid
if "$target/bin/agentic-loop" doctor > /tmp/agentic-loop-doctor-config.$$; then fail 'doctor accepted invalid configuration'; fi
grep -Fq '[失敗] 設定ファイル' /tmp/agentic-loop-doctor-config.$$ || fail 'doctor did not classify invalid configuration'
mv "$target/.agentic-loop.toml.valid" "$target/.agentic-loop.toml"
rm -f /tmp/agentic-loop-doctor-config.$$

# A PID reused by an unrelated process cannot keep a stale supervisor lock alive.
state_root="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop"
mkdir -p "$state_root/supervisor.lock"
printf '%s\n' "$$" > "$state_root/supervisor.pid"
"$target/bin/agentic-loop" start
[[ $(cat "$state_root/supervisor.pid") != "$$" ]] || fail 'start trusted an unrelated process in a stale PID file'
"$target/bin/agentic-loop" stop

# A freshly created lock without a published pid is treated as mid-startup and
# not stolen, so two concurrent starts cannot both begin supervising.
rm -f "$state_root/supervisor.pid"
mkdir -p "$state_root/supervisor.lock"
if "$target/bin/agentic-loop" start >/dev/null 2>&1; then fail 'start stole a fresh mid-startup supervisor lock'; fi
[[ -d $state_root/supervisor.lock ]] || fail 'a fresh mid-startup lock was removed by a racing start'
rmdir "$state_root/supervisor.lock"

# An exhausted GraphQL budget only suppresses Projects; the REST queue still completes work.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
pending_project="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/project-pending"
rm -f "$pending_project"
printf '90 queued open none 2025-01-01T00:00:00Z\n' > "$FAKE_GH_ROOT/$state_key.state"
before_project_adds=$(grep -c $'\tproject item-add' "$FAKE_GH_ROOT/calls" || true)
FAKE_GRAPHQL_REMAINING=499 FAKE_GRAPHQL_RESET=$(date +%s) FAKE_REST_FAILURES=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
after_project_adds=$(grep -c $'\tproject item-add' "$FAKE_GH_ROOT/calls" || true)
if ! grep -Eq '^90 completed closed' "$FAKE_GH_ROOT/$state_key.state"; then
  tail -n 80 "$FAKE_GH_ROOT/calls" >&2
  cat "$FAKE_GH_ROOT/$state_key.state" >&2
  fail 'GraphQL exhaustion stopped the REST Issue loop'
fi
[[ $after_project_adds -eq $before_project_adds ]] || fail 'GraphQL exhaustion did not suppress Projects synchronization'
[[ -s $pending_project ]] || fail 'suppressed Projects synchronization was not persisted'
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
FAKE_GRAPHQL_REMAINING=5000 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
if [[ -e $pending_project ]]; then
  cat "$pending_project" >&2
  fail 'Projects synchronization did not resume after GraphQL recovery'
fi

# Commit the runtime configuration so worker worktrees start from a realistic default branch.
git -C "$target" add .
git -C "$target" commit --quiet -m configure
git -C "$target" push --quiet
state="$FAKE_GH_ROOT/$state_key.state"
state_root="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop"
closes="$FAKE_GH_ROOT/$state_key.closes"

if [[ $TEST_GROUP == all || $TEST_GROUP == queue ]]; then

# Numeric priority is the primary queue key (desc), then category rank,
# creation time, and Issue number. A priority-90 improvement must therefore
# beat every priority-0 feature. unknown_scope=open disables change-scope
# conflict avoidance here: this fixture declares no scope for any Issue and
# exercises only the ordering, which the scope filter must never reorder.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=12 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
prio90=$(printf '<!-- agentic-loop:priority 90 -->' | base64 -w0)
printf '101 queued open none 2026-01-01T00:00:00Z none loop-continuity %s\n102 queued open none 2026-01-01T00:00:00Z none confidentiality-incident %s\n103 queued open none 2026-01-01T00:00:00Z none integrity-incident\n104 queued open none 2026-01-01T00:00:00Z none availability-incident\n105 queued open none 2026-01-01T00:00:00Z none feature\n106 queued open none 2026-01-01T00:00:00Z none improvement %s\n107 queued open none 2026-01-02T00:00:00Z none improvement %s\n108 queued open none 2025-01-01T00:00:00Z none improvement\n109 queued open none 2026-01-02T00:00:00Z none improvement %s\n110 queued open none 2026-01-03T00:00:00Z none none\n111 queued open none 2026-01-01T00:00:00Z none feature,availability-incident\n112 queued open none 2026-01-01T00:00:00Z none bug\n' "$prio90" "$prio90" "$prio90" "$prio90" "$prio90" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
claim_order=$(sed -n 's/^\([0-9][0-9]*\) .*agentic-loop:lease.*/\1/p' "$FAKE_GH_ROOT/$state_key.comments" | awk '!seen[$0]++' | paste -sd, -)
[[ $claim_order == 101,102,106,107,109,103,104,111,112,105,108,110 ]] || fail "queue order was incorrect (priority first, then category/created/number): $claim_order"
grep -Eq '^110 completed closed .* improvement$' "$state" || fail 'missing category was not repaired to improvement'
grep -Eq '^111 completed closed .* availability-incident$' "$state" || fail 'multiple categories did not retain only the highest-ranked category'
grep -Eq '^112 completed closed .* bug$' "$state" || fail 'a manually assigned bug category was not preserved'
[[ $(awk '$1 == 112' "$FAKE_GH_ROOT/$state_key.comments" | grep -c 'category-reconciled' || true) -eq 0 ]] || fail 'reconcile touched an Issue that already had exactly one category'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'category-reconciled reason=missing' 'missing category repair was not audited'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'category-reconciled reason=multiple selected=availability-incident' 'multiple category repair was not audited'
# comment_issue's plain double-quoted-string call sites (Issue #110): the
# marker must be followed by a real newline, encoded here as the 2-char `\n`.
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=missing -->\nカテゴリが未設定' 'comment_issue (single-quoted call site) did not render a real newline after its marker'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'selected=availability-incident -->\n複数のカテゴリ' 'comment_issue (double-quoted call site) did not render a real newline after its marker'

# reconcile_queued_categories' content-based triage (Issue #167): a queued
# Issue with no category:* Label is classified from its body before falling
# back to category:improvement. bug/loop-continuity keywords take priority
# over the generic default; a body with none of the known keyword families
# still falls back to reason=missing exactly as before.
bug_body=$(printf 'ログイン画面が誤った日付を表示するバグを修正してほしい。' | base64 -w0)
loop_body=$(printf 'Supervisorのclaimがqueueで詰まって進まない問題を直したい。' | base64 -w0)
feature_body=$(printf 'ダッシュボードに新機能としてグラフ表示を追加したい。' | base64 -w0)
plain_body=$(printf '既存の文書の誤字を整理したい。' | base64 -w0)
printf '201 queued open none 2026-02-01T00:00:00Z none none %s\n202 queued open none 2026-02-01T00:00:00Z none none %s\n203 queued open none 2026-02-01T00:00:00Z none none %s\n204 queued open none 2026-02-01T00:00:00Z none none %s\n' \
  "$bug_body" "$loop_body" "$feature_body" "$plain_body" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
# awk (not `grep '...$'`): these fixture rows carry a body (column 8), so the
# category (column 7) never lands at the end of the line.
[[ $(awk '$1 == 201 {print $2, $7}' "$state") == 'completed bug' ]] || fail 'a body mentioning バグ/不具合 was not content-classified as category:bug'
[[ $(awk '$1 == 202 {print $2, $7}' "$state") == 'completed loop-continuity' ]] || fail 'a body mentioning Supervisor/queue/claim was not content-classified as category:loop-continuity'
[[ $(awk '$1 == 203 {print $2, $7}' "$state") == 'completed feature' ]] || fail 'a body mentioning 新機能 was not content-classified as category:feature'
[[ $(awk '$1 == 204 {print $2, $7}' "$state") == 'completed improvement' ]] || fail 'a body with no known keyword family did not fall back to category:improvement'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'category-reconciled reason=content selected=bug' 'content-based bug classification was not audited'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'category-reconciled reason=content selected=loop-continuity' 'content-based loop-continuity classification was not audited'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'category-reconciled reason=content selected=feature' 'content-based feature classification was not audited'
[[ $(awk '$1 == 204' "$FAKE_GH_ROOT/$state_key.comments" | grep -c 'reason=missing' || true) -ge 1 ]] || fail 'an unclassifiable body did not fall back to the reason=missing comment'

# triage_category_from_text unit coverage (Issue #167): a pure function, so
# an incident-shaped free-text body is proven to never auto-classify as an
# incident (that stays a human/postmortem-workflow decision) without needing
# the fake-gh harness. source=/dev/null: project.sh's snapshot_state_rows/
# refresh_supervisor_snapshot declare `local state=`/`local target=`, and
# following the real source into this subshell makes the static analyzer
# conflate those with this outer script's own (genuinely different) top-level
# $state/$target, misreporting every later read of either as "modified in a
# subshell" for the rest of the file.
(
  set -euo pipefail
  # shellcheck source=/dev/null
  . "$PROJECT_ROOT/bin/lib/agentic-loop/project.sh"

  [[ $(triage_category_from_text 'Supervisorのclaimがqueueで詰まって進まない') == loop-continuity ]] || fail 'triage did not classify a Supervisor/queue/claim Issue as loop-continuity'
  [[ $(triage_category_from_text 'ログイン画面で誤った日付を表示するバグを修正する') == bug ]] || fail 'triage did not classify a バグ Issue as bug'
  [[ $(triage_category_from_text 'a regression crashes the login flow') == bug ]] || fail 'triage did not classify a regression/crash Issue as bug'
  [[ $(triage_category_from_text 'ダッシュボードに新機能としてグラフ表示を追加する') == feature ]] || fail 'triage did not classify a 新機能 Issue as feature'
  [[ $(triage_category_from_text '既存の文書の誤字を整理する') == '' ]] || fail 'triage guessed a category for text with no keyword family'
  [[ $(triage_category_from_text '個人情報が漏洩した疑いがある') == '' ]] || fail 'triage guessed an incident category from free text (incidents require verified harm, never a keyword guess)'
)

# Project membership query structure (issue #268).  `pageInfo` is a field of
# the projectItems *connection*; selecting it inside nodes{} makes GitHub
# answer "Field 'pageInfo' doesn't exist on type 'ProjectV2Item'", and an
# outer if/else without `end` makes the reducer fail to compile.  Either one
# turns every load_project_content call into rc=1, so project-pending never
# drains and Agent status/Category/Blocked by stop converging.  jq is now
# pinned and the fake-gh fixture below evaluates the real --jq reducer against
# a schema-shaped fixture, but the fake still cannot validate GitHub's actual
# GraphQL schema (e.g. that pageInfo really is unavailable inside nodes{}), so
# this structural guard on the query/reducer text stays as a second,
# fake-independent layer alongside `bin/agentic-loop smoke`'s real boundary.
(
  set -euo pipefail
  # shellcheck source=/dev/null
  . "$PROJECT_ROOT/bin/lib/agentic-loop/project.sh"

  # Depth of each selection's parent: the brace nesting immediately before the
  # named field opens its own selection set (or its arguments).
  selection_depth_of() {
    awk -v q="$1" -v field="$2" '
      BEGIN {
        depth = 0
        for (i = 1; i <= length(q); i++) {
          c = substr(q, i, 1)
          if (c == "{") { depth++; continue }
          if (c == "}") { depth--; continue }
          if (i > 1 && index("{} ", substr(q, i - 1, 1)) == 0) continue
          if (substr(q, i, length(field)) != field) continue
          following = substr(q, i + length(field), 1)
          if (following == "{" || following == "(") print depth
        }
      }'
  }

  content_query=$(project_content_query issue)
  [[ $content_query == *'content:issue(number:$number)'* ]] || fail 'the Project content query no longer selects the Issue by number'
  [[ $(project_content_query pullRequest) == *'content:pullRequest(number:$number)'* ]] || fail 'the Project content query no longer supports pull requests'
  balance=$(awk -v q="$content_query" 'BEGIN { d = 0; for (i = 1; i <= length(q); i++) { c = substr(q, i, 1); if (c == "{") d++; else if (c == "}") d-- } print d }')
  [[ $balance == 0 ]] || fail "the Project content query has unbalanced braces (depth=$balance)"
  items_depth=$(selection_depth_of "$content_query" projectItems)
  page_depth=$(selection_depth_of "$content_query" pageInfo)
  [[ $items_depth =~ ^[0-9]+$ ]] || fail 'the Project content query no longer selects projectItems'
  [[ $page_depth =~ ^[0-9]+$ ]] || fail 'the Project content query no longer paginates projectItems (pageInfo is gone)'
  (( page_depth == items_depth + 1 )) || fail "pageInfo is not a direct child of the projectItems connection (projectItems parent depth=$items_depth, pageInfo parent depth=$page_depth)"

  content_jq=$(project_content_jq PVT_fixture)
  [[ $content_jq == *'.project.id == "PVT_fixture"'* ]] || fail 'the Project content reducer no longer filters items by the Project id'
  [[ $content_jq == *'$items.pageInfo.hasNextPage'* && $content_jq == *'$items.pageInfo.endCursor'* ]] || fail 'the Project content reducer no longer reads the connection-level pageInfo the query selects'
  [[ $content_jq == *'.errors'* ]] || fail 'the Project content reducer no longer turns a GraphQL errors payload into a retryable failure'
  jq_ifs=$(grep -o '\bif\b' <<< "$content_jq" | wc -l)
  jq_ends=$(grep -o '\bend\b' <<< "$content_jq" | wc -l)
  (( jq_ifs > 0 && jq_ifs == jq_ends )) || fail "the Project content reducer has unterminated if statements (if=$jq_ifs end=$jq_ends)"
)

# triage_issue_content end-to-end via the real fake-gh harness (title, body,
# and empty comments all feed the same classifier): Issue 205's body has no
# keyword family at all, so only a custom fixture title (an escape hatch the
# main fake gh has no other caller for) can drive its classification.
printf '205 queued open none 2026-02-01T00:00:00Z none none %s\n' "$plain_body" > "$state"
printf '205\tSupervisorのworktree cleanupが終わらない\n' > "$FAKE_GH_ROOT/$state_key.titles"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
rm -f "$FAKE_GH_ROOT/$state_key.titles"
[[ $(awk '$1 == 205 {print $2, $7}' "$state") == 'completed loop-continuity' ]] || fail 'an Issue title mentioning Supervisor/worktree was not content-classified as category:loop-continuity'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'category-reconciled reason=content selected=loop-continuity' 'title-driven loop-continuity classification was not audited'

# Numeric priority semantics: descending order, unset=0, multiple markers take
# the maximum, and out-of-range/non-numeric markers are ignored. All Issues
# share one category and created_at so only priority decides the order.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
b100=$(printf '<!-- agentic-loop:priority 100 -->' | base64 -w0)
b90=$(printf '<!-- agentic-loop:priority 90 -->' | base64 -w0)
bmix=$(printf '<!-- agentic-loop:priority 50 -->\n<!-- agentic-loop:priority 75 -->' | base64 -w0)
b75=$(printf '<!-- agentic-loop:priority 75 -->' | base64 -w0)
binvalid=$(printf '<!-- agentic-loop:priority 200 -->' | base64 -w0)
printf '41 queued open none 2026-01-01T00:00:00Z none improvement %s\n42 queued open none 2026-01-01T00:00:00Z none improvement %s\n43 queued open none 2026-01-01T00:00:00Z none improvement %s\n44 queued open none 2026-01-01T00:00:00Z none improvement\n45 queued open none 2026-01-01T00:00:00Z none improvement %s\n46 queued open none 2026-01-01T00:00:00Z none improvement %s\n' "$b90" "$bmix" "$b75" "$binvalid" "$b100" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
queue_json=$("$target/bin/agentic-loop" status --format json)
candidate_order=$(printf '%s' "$queue_json" | yq -p json '.queue.candidates[].issue' | paste -sd, -)
[[ $candidate_order == 46,41,42,43,44,45 ]] || fail "priority semantics order was incorrect: $candidate_order"
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.candidates[0].priority') -eq 100 ]] || fail 'priority 100 candidate was not ranked first'
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.candidates[1].priority') -eq 90 ]] || fail 'priority 90 candidate did not sort after 100'
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.candidates[2].priority') -eq 75 ]] || fail 'multiple markers did not take the maximum (50 vs 75)'
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.candidates[4].priority') -eq 0 ]] || fail 'an unset priority was not treated as 0'
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.candidates[5].priority') -eq 0 ]] || fail 'an out-of-range marker was not treated as unset'

# unknown_scope=open: this fixture also declares no scope and exercises the
# worker limit and priority ordering, not scope conflict avoidance. The two
# highest-priority Issues (90, 90) are claimed first; the lower ones stay queued.
#
# The installed-project fixture is a detection-only seed with no Makefile
# (see the capability manifest scenario below, which asserts that absence).
# Issue #266's worker-prompt assertions need a declared affected_check/
# impact_map, so overlay them onto capabilities.toml just for this block and
# restore the detection-only manifest immediately after (same pattern as the
# preflight capability_orig backup/restore further down).
queue_capabilities_orig="$TEST_ROOT/queue-capabilities-orig.toml"
cp "$target/.agentic-loop/capabilities.toml" "$queue_capabilities_orig"
sed -i '/^\[validation\]$/a affected_check = "devbox run --pure affected"\nimpact_map = "tests/impact-map.toml"' "$target/.agentic-loop/capabilities.toml"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
prio90=$(printf '<!-- agentic-loop:priority 90 -->' | base64 -w0)
prio25=$(printf '<!-- agentic-loop:priority 25 -->' | base64 -w0)
printf '1 queued open none 2026-01-01T00:00:00Z none none %s\n2 queued open none 2026-01-02T00:00:00Z none none %s\n3 queued open none 2025-12-31T00:00:00Z none none %s\n4 queued open none 2025-01-01T00:00:00Z none none\n' "$prio25" "$prio90" "$prio90" > "$state"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
completed_count=$(awk '$2 == "completed" {count++} END {print count+0}' "$state")
if [[ $completed_count -ne 2 ]]; then
  cat "$state" >&2
  find "$state_root" -type f -maxdepth 3 -print -exec sed -n '1,160p' {} \; >&2 || true
  fail "supervisor completed $completed_count Issues instead of 2"
fi
[[ $(awk '$2 == "queued" {count++} END {print count+0}' "$state") -eq 2 ]] || fail 'run-once claimed more than the worker limit'
grep -Eq '^3 completed closed([[:space:]]|$)' "$state" || fail 'oldest critical Issue was not claimed first'
grep -Eq '^2 completed closed([[:space:]]|$)' "$state" || fail 'second critical Issue was not claimed before lower priorities'
for issue in 2 3; do
  [[ ! -e $target-worktrees/issue-$issue ]] || fail "completed worker worktree remained: issue-$issue"
  ! git -C "$target" show-ref --verify --quiet "refs/heads/agent/issue-$issue" || fail "completed worker local branch remained: agent/issue-$issue"
done
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'remote branchは復旧' 'completed cleanup did not record the remote branch policy'
assert_contains "$FAKE_GH_ROOT/codex-calls" '--sandbox workspace-write' 'worker did not use workspace-write'
assert_contains "$FAKE_GH_ROOT/codex-calls" "--add-dir $target/.git" 'worker did not grant its exact Git common directory'
assert_contains "$FAKE_GH_ROOT/codex-calls" "--add-dir $target-worktrees/issue-3/.agents" 'worker did not grant its exact protected .agents directory'
assert_contains "$FAKE_GH_ROOT/codex-calls" "-C $target-worktrees/issue-3" 'worker did not use the repository-adjacent worktree root'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'approval_policy="never"' 'worker can block on approval'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'GitHubのIssue、PR' 'worker prompt did not require Japanese GitHub content'
assert_contains "$FAKE_GH_ROOT/codex-calls" '実在しない承認コマンド・設定・権限フローを創作して停止してはいけません' 'worker prompt did not forbid fabricating nonexistent approval mechanisms (Issue #191)'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'devbox.lock' 'worker prompt did not clarify that toolchain lock changes need no worker pre-approval (Issue #191)'
assert_contains "$FAKE_GH_ROOT/codex-calls" '変更の重大さについての自己判断だけを理由にaxisをhighにしたりapproval.required=trueにしないでください' 'plan prompt did not forbid self-invented approval requirements (Issue #191)'
assert_contains "$FAKE_GH_ROOT/codex-calls" '影響範囲検証（反復の短縮専用、push/merge gateではない）' 'worker prompt did not expose the declared affected check (Issue #266)'
assert_contains "$FAKE_GH_ROOT/codex-calls" '影響範囲map: tests/impact-map.toml' 'worker prompt did not expose the declared impact map (Issue #266)'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'このworkerの実行時間予算: 上限=' 'worker prompt did not expose the observed time budget (Issue #266)'
assert_contains "$FAKE_GH_ROOT/codex-calls" '本題外のflaky・CI固有失敗を見つけたら' 'worker prompt did not define the out-of-scope failure stop rule (Issue #266)'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'のハートビートです' 'supervisor did not write its Issue comments in Japanese'
if grep -Eq 'danger-full-access|OPENAI_API_KEY|--add-dir /($| )|--add-dir /home($| )' "$FAKE_GH_ROOT/codex-calls"; then fail 'worker used forbidden Codex configuration or a broad writable path'; fi
[[ ! -e $state_root/worktrees ]] || fail 'worker worktrees were placed inside Git metadata'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'Token使用量（分析用）: provider=codex' 'codex worker did not record a token usage analysis entry'
cp "$queue_capabilities_orig" "$target/.agentic-loop/capabilities.toml"

# AGENT_PROVIDER=claude routes the worker to the Claude CLI, confining writes to the worktree.
[[ -f $target/.claude/skills/submit-requirement/SKILL.md ]] || fail 'install did not add the Claude submit-requirement skill'
[[ -f $target/.claude/skills/diagnose-codebase/SKILL.md ]] || fail 'install did not add the Claude diagnosis skill'
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '7 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/claude-calls"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 "$target/bin/agentic-loop" _supervise
wait_for 'claude provider completed the Issue' --timeout 15 grep -Eq '^7 completed closed' "$state" \
  || fail 'claude provider did not complete the Issue'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--print' 'claude worker did not run non-interactively'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--dangerously-skip-permissions' 'claude worker did not skip permission prompts'
assert_contains "$FAKE_GH_ROOT/claude-calls" "--add-dir $target/.git" 'claude worker did not grant its exact Git common directory'
assert_contains "$FAKE_GH_ROOT/claude-calls" "--add-dir $target-worktrees/issue-7/.agents" 'claude worker did not grant its exact protected .agents directory'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--output-format stream-json' 'claude worker did not request a machine-readable streaming usage payload'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--verbose' 'claude worker did not pass --verbose, required by --print --output-format stream-json'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'Token使用量（分析用）: provider=claude' 'claude worker did not record its token usage on the Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '入力=123tok' 'claude usage record lacked input token count'
# shellcheck disable=SC2016 # The dollar sign is a literal currency prefix in the recorded usage line.
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'cost=$0.0123' 'claude usage record lacked the reported cost'

# Residual `.raw.*`/`.final.*` stage output from a killed prior run of the
# same stage (e.g. a worker_timeout kill mid-stream, before agent_run_stage's
# own cleanup could run) must be cleared at the next run of that stage,
# keeping disk usage bounded to the stage currently in flight (Issue #273).
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '270 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/claude-calls"
rm -f "$FAKE_GH_ROOT/claude-json-count"
printf 'stale raw content with rate limit\n' > "$state_root/issue-270-result.txt.raw.999999"
printf '{"type":"result"}\n' > "$state_root/issue-270-result.txt.final.999999"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 "$target/bin/agentic-loop" _supervise
wait_for 'claude provider completed after stale stage cleanup' --timeout 15 grep -Eq '^270 completed closed' "$state" \
  || fail 'claude provider with stale stage output residue did not complete'
[[ ! -e $state_root/issue-270-result.txt.raw.999999 ]] || fail 'stale .raw output from a prior killed stage run was not cleared at stage start'
[[ ! -e $state_root/issue-270-result.txt.final.999999 ]] || fail 'stale .final output from a prior killed stage run was not cleared at stage start'

# The fake claude CLI mirrors the real CLI's measured contract (Issue #273):
# --print --output-format stream-json requires --verbose and fails fast with
# no stdout otherwise. Exercised directly against the harness so a future
# change to agent.sh's claude_args cannot silently drop --verbose without a
# test noticing (the worker-level test above only proves --verbose is present
# when the fake CLI already accepts stream-json).
if "$FAKE_BIN/claude" --print --output-format stream-json --dangerously-skip-permissions 'hi' >"$TEST_ROOT/claude-no-verbose.out" 2>&1; then
  fail 'fake claude accepted --output-format stream-json without --verbose'
fi
[[ ! -s $TEST_ROOT/claude-no-verbose.out ]] || grep -Fq 'requires --verbose' "$TEST_ROOT/claude-no-verbose.out" || fail 'fake claude rejection did not mention the --verbose requirement'
rm -f "$TEST_ROOT/claude-no-verbose.out"

# A stream with no terminal `{"type":"result",...}` event (killed mid-run, or
# a CLI exiting before ever emitting one) must not be re-parsed as a whole:
# the extraction leaves final_result empty, and agent_run_stage's existing
# empty-result_file fallback copies stderr in instead (Issue #273) -- the
# Issue is retried through the ordinary transient-failure path, not
# misclassified as pool exhaustion (no rate-limit/quota text on stderr here).
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=0
printf '271 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/pools/claude/exhausted" "$FAKE_GH_ROOT/claude-json-count"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 FAKE_CLAUDE_OMIT_TERMINAL=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^271 (queued open|failed)' "$state" || fail 'a claude stream with no terminal event was not treated as an ordinary stage failure'
[[ ! -r $state_root/pools/claude/exhausted ]] || fail 'a claude stream with no terminal event must not mark the claude pool exhausted'
assert_contains "$state_root/issue-271-plan.txt" 'stage killed before completion' 'the missing-terminal-event fallback did not surface the stderr content'
rm -f "$state_root/pools/claude/exhausted"

# --- Declared reasoning effort and per-stage cost ceiling reach claude (Issue #265) ---
# A declared reasoning_effort must arrive as the claude CLI's --effort (it was
# previously assembled only for Codex and silently dropped here), be observable
# on the Issue's usage comment, and budget.max_stage_cost_usd must arrive as
# --max-budget-usd. The fake claude records every argument it was given.
cat > "$target/.agentic-loop.toml" <<'EFFORT_TOML'
[agent]
provider = "claude"

[agent.plan]
model = "opus"
reasoning_effort = "xhigh"

[agent.exec]
model = "sonnet"
reasoning_effort = "medium"

[budget]
max_stage_cost_usd = 3.5

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
EFFORT_TOML
printf '265 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/claude-calls"
rm -f "$FAKE_GH_ROOT/claude-json-count"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^265 completed closed' "$state" || fail 'claude effort Issue did not complete'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--effort xhigh' 'plan stage did not pass its declared reasoning effort to claude'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--effort medium' 'exec stage did not pass its declared reasoning effort to claude'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--max-budget-usd 3.5' 'claude stage did not pass the configured per-stage cost ceiling'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reasoning_effort=xhigh' 'claude usage record did not show the applied plan effort'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reasoning_effort=medium' 'claude usage record did not show the applied exec effort'

# Reaching the cost ceiling is the operator's own cap, not a spent
# subscription: it must not mark the pool exhausted or pause claiming, and the
# Issue must follow the ordinary bounded failure/retry path instead.
printf '267 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/claude-calls"
rm -rf "$state_root/pools"
rm -f "$state_root/agent-exhausted" "$state_root/all-pools-paused" "$FAKE_GH_ROOT/claude-json-count"
FAKE_CLAUDE_IS_ERROR=1 FAKE_CLAUDE_API_ERROR_STATUS='' \
  FAKE_CLAUDE_RESULT='Reached the max budget for this session (--max-budget-usd 3.5); stopping.' \
  AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 "$target/bin/agentic-loop" _supervise
[[ ! -e $state_root/pools/claude/exhausted ]] || fail 'a cost-ceiling stop must not mark the claude pool exhausted'
[[ ! -e $state_root/all-pools-paused ]] || fail 'a cost-ceiling stop must not pause claiming'
[[ ! -e $state_root/agent-exhausted ]] || fail 'a cost-ceiling stop must not set the global exhaustion marker'
grep -Eq '^267 (failed|queued) open' "$state" || fail 'a cost-ceiling stop did not follow the ordinary failure path'

# An undeclared reasoning_effort and an unset ceiling add no arguments at all,
# leaving the claude CLI defaults in place (the Codex plan=high / exec=low
# fallback stays Codex-only).
cat > "$target/.agentic-loop.toml" <<'NOEFFORT_TOML'
[agent]
provider = "claude"

[agent.plan]
model = "opus"

[agent.exec]
model = "sonnet"

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
NOEFFORT_TOML
printf '266 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/claude-calls"
rm -f "$FAKE_GH_ROOT/claude-json-count"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^266 completed closed' "$state" || fail 'claude Issue without a declared effort did not complete'
if grep -Fq -- '--effort' "$FAKE_GH_ROOT/claude-calls"; then fail 'an undeclared reasoning effort must leave the claude CLI default in place'; fi
if grep -Fq -- '--max-budget-usd' "$FAKE_GH_ROOT/claude-calls"; then fail 'an unset cost ceiling must not pass --max-budget-usd'; fi
if grep -Fq 'reasoning_effort=' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'an undeclared reasoning effort must not be recorded as applied'; fi

# doctor treats a level the claude CLI does not accept, and a non-positive cost
# ceiling, as configuration errors instead of silently ignoring them.
cat > "$target/.agentic-loop.toml" <<'BAD_EFFORT_TOML'
[agent]
provider = "claude"

[agent.plan]
reasoning_effort = "ultra"

[budget]
max_stage_cost_usd = 0

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
BAD_EFFORT_TOML
doctor_effort_out=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '[失敗] tiers設定 (plan)' <<< "$doctor_effort_out" || fail 'doctor did not flag an unsupported claude reasoning_effort'
grep -Fq '[失敗] stageコスト上限' <<< "$doctor_effort_out" || fail 'doctor did not flag an invalid per-stage cost ceiling'

# AGENT_PROVIDER=opencode routes the worker to `opencode run`, scoped to the worktree.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '30 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/opencode-calls"
AGENT_PROVIDER=opencode AGENTIC_LOOP_RUN_ONCE=1 FAKE_OPENCODE_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^30 completed closed' "$state" || fail 'opencode provider did not complete the Issue'
assert_contains "$FAKE_GH_ROOT/opencode-calls" 'run --auto' 'opencode worker did not run headless with auto-approval'
assert_contains "$FAKE_GH_ROOT/opencode-calls" '--format json' 'opencode worker did not request machine-readable events'
assert_contains "$FAKE_GH_ROOT/opencode-calls" "--dir $target-worktrees/issue-30" 'opencode worker did not confine work to the worktree'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'provider=opencode' 'opencode worker did not record a token usage analysis entry'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '入力=200tok' 'opencode usage record lacked input tokens'
# shellcheck disable=SC2016 # The dollar sign is a literal currency prefix in the recorded usage line.
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'cost=$0.02' 'opencode usage record lacked the reported cost'

# Per-phase provider selection: config routes the plan phase and exec phase to
# different providers (plan=codex read-only, exec=opencode) within one Issue.
{
  printf '[agent.plan]\nprovider = "codex"\n'
  printf '[agent.exec]\nprovider = "opencode"\n'
  printf '[queue]\npoll_seconds = 1\nmax_workers = 1\nlease_seconds = 3\nstop_timeout = 10\nstale_days = 30\n'
} > "$target/.agentic-loop.toml"
printf '31 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
: > "$FAKE_GH_ROOT/opencode-calls"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_OPENCODE_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^31 completed closed' "$state" || fail 'mixed-provider Issue did not complete'
assert_contains "$FAKE_GH_ROOT/codex-calls" '--sandbox read-only' 'plan phase did not run on codex'
assert_contains "$FAKE_GH_ROOT/opencode-calls" 'run --auto' 'exec phase did not run on opencode'
[[ $(grep -c -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls") -eq 0 ]] || fail 'exec phase unexpectedly ran on codex'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'provider=codex stage=plan' 'plan usage did not record the codex provider'

# Each Issue runs a read-only plan stage before the workspace-write exec stage.
# A plan whose every candidate fails must never fall through to exec with an
# empty plan (that would bypass scope refinement, preflight, workload, and
# decomposition simultaneously). It is retried in-turn (bounded by
# agent.retry.plan_max) before being treated as a genuine task failure, so the
# preflight/workload gate markers are never emitted for this turn.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=0
printf '20 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
: > "$state_root/events.log"
AGENT_PLAN_MAX_RETRIES=1 FAKE_CODEX_EXIT=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^20 failed' "$state" || fail 'a plan failure exhausting its in-turn retries did not fail the Issue'
# A terminal plan failure records its own progress stage (Issue #273): before
# `plan-failed` joined PROGRESS_STAGES, progress_write silently discarded this
# marker (the stage failed its enum validation, per progress_stage_valid), so
# neither the events.log row nor the workers/<issue>.progress marker was ever
# written for it -- the worker's own eventual clear_worker_local removes the
# marker file itself, but the append-only events.log survives and is the
# observable proof here.
grep -Eq $'\t20\tprogress\tplan-failed$' "$state_root/events.log" || fail 'a terminal plan failure did not record a plan-failed progress event'
plan_passes=$(grep -c -- '--sandbox read-only' "$FAKE_GH_ROOT/codex-calls" || true)
[[ $plan_passes -eq 2 ]] || fail "expected the initial planning pass plus one in-turn retry, got $plan_passes"
exec_passes=$(grep -c -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || true)
[[ $exec_passes -eq 0 ]] || fail 'plan failure unexpectedly started an empty-plan exec'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=plan-failure' 'plan failure was not observable on the Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'stage=plan' 'plan stage usage was not recorded'
if grep -Fq 'agentic-loop:preflight' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'a plan failure unexpectedly reached the preflight gate'; fi
if grep -Fq 'agentic-loop:workload' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'a plan failure unexpectedly reached the workload gate'; fi
git -C "$target" worktree remove "$target-worktrees/issue-20" --force 2>/dev/null || true
git -C "$target" branch -D agent/issue-20 >/dev/null 2>&1 || true

# Repeated plan failures still converge on the ordinary retry_failed/parked
# path (no new state and no infinite requeue loop): with MAX_ATTEMPTS=1 the
# single claim already used the whole retry budget, so the next poll parks
# the Issue directly instead of reclaiming it for another plan attempt.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=1 RETRY_COOLDOWN_SECONDS=0
printf '21 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
rm -f "$closes"
AGENT_PLAN_MAX_RETRIES=1 FAKE_CODEX_EXIT=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^21 failed' "$state" || fail 'a plan failure with no retry budget left did not fail the Issue'
calls_before_park=$(wc -l < "$FAKE_GH_ROOT/codex-calls")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^21 parked open' "$state" || fail 'a plan failure at MAX_ATTEMPTS was not parked instead of requeued indefinitely'
[[ ! -r "$closes" ]] || ! grep -Fq $'^21\t' "$closes" || fail 'a plan-failure Issue reaching MAX_ATTEMPTS must never be closed'
calls_after_park=$(wc -l < "$FAKE_GH_ROOT/codex-calls")
[[ $calls_after_park -eq $calls_before_park ]] || fail 'parking a retry-exhausted Issue unexpectedly reclaimed it for another plan attempt'
git -C "$target" worktree remove "$target-worktrees/issue-21" --force 2>/dev/null || true
git -C "$target" branch -D agent/issue-21 >/dev/null 2>&1 || true

# A plan failure while a DIFFERENT declared pool is marked exhausted is
# treated like pool exhaustion (requeued without consuming an Issue attempt),
# not like a genuine task failure -- even though the plan stage itself
# reported an ordinary per-candidate error (STAGE_RC=3) rather than
# STAGE_RC=1/2, and even though the exhausted pool is not the one plan itself
# used (agent_any_pool_marker_active is a weaker, correlation-only signal;
# only marking every declared pool would also block a fresh claim).
{
  printf '[agent.plan]\nprovider = "codex"\n'
  printf '[agent.exec]\nprovider = "opencode"\n'
  printf '[queue]\npoll_seconds = 1\nmax_workers = 1\nlease_seconds = 30\nstop_timeout = 10\nstale_days = 30\nmax_attempts = 1\nretry_cooldown_seconds = 0\n'
} > "$target/.agentic-loop.toml"
printf '22 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
rm -rf "$state_root/pools"
mkdir -p "$state_root/pools/opencode"
printf '%s\n' "$(( $(date +%s) + 1800 ))" > "$state_root/pools/opencode/exhausted"
AGENT_PLAN_MAX_RETRIES=1 FAKE_CODEX_EXIT=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^22 queued' "$state" || fail 'a plan failure correlated with another pool exhaustion was not requeued'
if grep -Eq '^22 failed' "$state"; then fail 'a plan failure correlated with pool exhaustion must not fail the Issue'; fi
[[ ! -e "$state_root/attempts/issue-22" ]] || fail 'a plan failure correlated with pool exhaustion consumed an Issue attempt'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:exhausted' 'pool-exhaustion-correlated plan failure was not recorded as exhausted'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=plan-failure' 'pool-exhaustion-correlated plan failure did not record its origin'
rm -rf "$state_root/pools"
git -C "$target" worktree remove "$target-worktrees/issue-22" --force 2>/dev/null || true
git -C "$target" branch -D agent/issue-22 >/dev/null 2>&1 || true

# A clean exec exit without a terminal marker is a protocol retry, not a
# flagship replan. The second response uses the same plan and completes.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
printf '201 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
rm -f "$FAKE_GH_ROOT/codex-exec-count"
AGENT_PLAN_MAX_RETRIES=1 FAKE_CODEX_EXEC_RESULT_1='CI monitorの報告待ちです' FAKE_CODEX_EXEC_RESULT_2='AGENTIC_LOOP_RESULT=completed' AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^201 completed closed' "$state" || fail 'marker-missing exec retry did not complete after a valid second response'
[[ $(grep -c -- '--sandbox read-only' "$FAKE_GH_ROOT/codex-calls") -eq 1 ]] || fail 'protocol retry unexpectedly replanned'
[[ $(grep -c -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls") -eq 2 ]] || fail 'protocol retry did not limit exec to the initial call plus one retry'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:protocol-retry' 'protocol retry was not audited on the Issue'
if grep -Fq 'agentic-loop:replan' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'marker-missing exec triggered a flagship replan'; fi

# Two clean exits without a valid terminal marker are deterministic failure;
# text containing a marker, a trailing non-empty line, and unknown values must
# receive the same treatment rather than being mistaken for completion. No PR
# is merged for this branch (FAKE_PR_MERGED=0), so the markerless-merged
# rescue (Issue #262) must not turn any of these into a false completion.
for malformed in \
  $'CI monitorの報告待ちです' \
  $'説明中の AGENTIC_LOOP_RESULT=completed です' \
  $'AGENTIC_LOOP_RESULT=completed\n続きの説明' \
  'AGENTIC_LOOP_RESULT=unknown'; do
  write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
  printf '202 queued open none 2026-01-01T00:00:00Z\n' > "$state"
  : > "$FAKE_GH_ROOT/$state_key.comments"
  : > "$FAKE_GH_ROOT/codex-calls"
  rm -f "$FAKE_GH_ROOT/codex-exec-count"
  FAKE_PR_MERGED=0 FAKE_CODEX_EXEC_RESULT_1="$malformed" FAKE_CODEX_EXEC_RESULT_2="$malformed" AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
  grep -Eq '^202 failed open' "$state" || fail "malformed terminal marker was accepted: $malformed"
  [[ $(grep -c -- '--sandbox read-only' "$FAKE_GH_ROOT/codex-calls") -eq 1 ]] || fail 'malformed terminal marker triggered replan'
  [[ $(grep -c -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls") -eq 2 ]] || fail 'malformed terminal marker did not stop after one retry'
done

# Budget guard: pause claiming while the weekly Codex usage exceeds the reserve,
# then resume once usage recovers (usage is read from the newest session log).
codex_home="$TEST_ROOT/codex-home"
mkdir -p "$codex_home/sessions/2026/01/01"
{
  printf '[budget]\nweekly_reserve_percent = 20\n'
  printf '[queue]\npoll_seconds = 1\nmax_workers = 1\nlease_seconds = 3\nstop_timeout = 10\nstale_days = 30\n'
} > "$target/.agentic-loop.toml"
printf '40 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
printf '{"type":"event_msg","timestamp":"2026-01-01T00:00:00Z","payload":{"type":"token_count","rate_limits":{"secondary":{"used_percent":95,"window_minutes":10079}}}}\n' > "$codex_home/sessions/2026/01/01/rollout-over.jsonl"
CODEX_HOME="$codex_home" AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^40 queued open' "$state" || fail 'budget guard did not pause claiming while over the reserve'
printf '{"type":"event_msg","timestamp":"2026-01-01T00:10:00Z","payload":{"type":"token_count","rate_limits":{"secondary":{"used_percent":10,"window_minutes":10079}}}}\n' > "$codex_home/sessions/2026/01/01/rollout-under.jsonl"
CODEX_HOME="$codex_home" AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^40 completed closed' "$state" || fail 'budget guard did not resume claiming after usage recovered'

# A transient worker failure is auto-retried (bounded) by the supervisor instead
# of parked, so a temporary problem like token exhaustion recovers on its own.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=0
printf '50 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_RESULT='AGENTIC_LOOP_RESULT=failed' "$target/bin/agentic-loop" _supervise
grep -Eq '^50 failed' "$state" || fail 'first transient failure was not recorded'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^50 completed closed' "$state" || fail 'transient failure was not auto-retried to completion'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:retry' 'automatic retry was not recorded on the Issue'

# A pre-existing (untracked) agent:failed Issue is auto-retried by the supervisor
# to completion, with no manual re-queue.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=0
rm -rf "$state_root/attempts"
printf '70 failed open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^70 completed closed' "$state" || fail 'untracked failed backlog was not auto-retried to completion'

# After MAX_ATTEMPTS the failed Issue is parked (open, non-claim), never closed
# (see docs/decisions/0016): a worker using up its retry budget does not mean
# the requirement is unresolvable.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=1 RETRY_COOLDOWN_SECONDS=0
printf '80 failed open none 2026-01-01T00:00:00Z\n' > "$state"
mkdir -p "$state_root/attempts"; printf '1\t0\n' > "$state_root/attempts/issue-80"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$closes"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^80 parked open' "$state" || fail 'retry-exhausted Issue was not parked (open, non-claim)'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:parked' 'park was not recorded'
[[ ! -r "$closes" ]] || ! grep -Fq $'^80\t' "$closes" || fail 'retry-exhausted Issue must never be closed'
[[ -r "$state_root/attempts/issue-80" ]] && fail 'park did not clear the attempts record'
true

# park does not block a concurrently queued Issue from being claimed: the
# parked Issue keeps its scope reservation cleared so it cannot conflict-wait
# another Issue either.
printf '180 queued open none 2026-01-01T00:00:00Z\n' >> "$state"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^80 parked open' "$state" || fail 'parked Issue changed state on an unrelated poll'
grep -Eq '^180 completed closed' "$state" || fail 'a parked Issue blocked an unrelated queued Issue from being claimed'
[[ ! -e "$state_root/conflict/issue-180" ]] || fail 'parked Issue left a stale conflict-wait entry for an unrelated Issue'

# park is stable across further polls: no repeated comments, no close, no
# reclaim (a restarted supervisor must not re-touch a parked Issue).
comments_before=$(wc -l < "$FAKE_GH_ROOT/$state_key.comments")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^80 parked open' "$state" || fail 'parked Issue was not stable across further supervisor polls'
comments_after=$(wc -l < "$FAKE_GH_ROOT/$state_key.comments")
[[ $comments_before == "$comments_after" ]] || fail 'parked Issue accumulated repeat comments across polls'
[[ ! -r "$closes" ]] || ! grep -Fq $'^80\t' "$closes" || fail 'a parked Issue was closed on a later poll'

# An authorized resume re-queues a parked Issue directly (no reopen: it was
# never closed), and the next poll claims it to completion.
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" resume 80
grep -Eq '^80 queued open' "$state" || fail 'resume did not re-queue a parked Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:resume' 'resume of a parked Issue was not recorded'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^80 completed closed' "$state" || fail 'a resumed park Issue was not claimed and completed on the next poll'
rm -f "$state_root/attempts/issue-80"

# A worker that declines an Issue (unnecessary/impossible) moves it to
# needs-input; the worker itself never closes it (see docs/decisions/0016 and
# ADR 0010: only an authorized operator's dispose may close it).
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '71 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$closes"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_RESULT='AGENTIC_LOOP_RESULT=declined' "$target/bin/agentic-loop" _supervise
grep -Eq '^71 needs-input open' "$state" || fail 'declined Issue was not moved to needs-input for authorized disposition'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:declined' 'decline was not recorded'
[[ ! -r "$closes" ]] || fail 'a declined Issue must never be closed by the worker'

# Authorized dispose is the only path that closes an Issue outside completion
# and stale triage; it must record state_reason=not_planned (see
# docs/decisions/0016's allowlist).
printf '72 needs-input open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$closes"
"$target/bin/agentic-loop" dispose 72 --reason cancelled
grep -Eq '^72 cancelled closed' "$state" || fail 'authorized dispose did not close the Issue as cancelled'
assert_contains "$closes" $'72\tnot_planned' 'dispose close did not record state_reason=not_planned'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dispose' 'dispose did not record its audit marker'

# dispose --target transfers the source Issue's native blocked_by
# dependencies to the consolidation target. Regression guard for Issue #252:
# issue_id must be the blocking Issue's *database id* (this fixture's
# id=number+900000), not its Issue number -- `gh api -f` (untyped string) or
# passing the raw number both 422 against the real endpoint, and here the
# fixture rejects a raw-number id the same way, so a regression fails the
# whole dispose instead of silently mis-transferring the dependency.
printf '73 needs-input open\n74 needs-input open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$closes"
printf '73 810,811\n' > "$FAKE_GH_ROOT/$state_key.dep-links"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
"$target/bin/agentic-loop" dispose 73 --reason superseded --target 74
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/dispose-transfer-calls.log"
grep -Eq '^73 superseded closed' "$state" || fail 'dispose --target did not close the source Issue as superseded'
assert_contains "$TEST_ROOT/dispose-transfer-calls.log" 'issues/74/dependencies/blocked_by --method POST -F issue_id=900810' 'dependency transfer did not POST the blocking Issue database id (810 -> 900810) as a typed parameter'
assert_contains "$TEST_ROOT/dispose-transfer-calls.log" 'issues/74/dependencies/blocked_by --method POST -F issue_id=900811' 'dependency transfer did not POST the blocking Issue database id (811 -> 900811) as a typed parameter'
grep -q -- '-f issue_id=' "$TEST_ROOT/dispose-transfer-calls.log" && fail 'dependency transfer still sends issue_id as an untyped -f string parameter'
grep -Eq -- 'issue_id=(810|811)([^0-9]|$)' "$TEST_ROOT/dispose-transfer-calls.log" && fail 'dependency transfer sent the blocking Issue number instead of its database id'
rm -f "$FAKE_GH_ROOT/$state_key.dep-links"

# A token/rate-limit exhaustion re-queues the Issue (never failed) and pauses
# the supervisor; claiming resumes once the pool's exhaustion clears. The real
# Codex usage-limit path exits non-zero and reports only on stderr, so drive it
# that way (a plain exit-0 result that merely *mentions* a rate limit is a
# successful stage and must not be read as exhaustion -- see the plan-output
# regression tests below).
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
rm -f "$state_root/agent-exhausted" "$state_root/pools/codex/exhausted" "$state_root/all-pools-paused" "$FAKE_GH_ROOT/codex-exec-count"
printf '60 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_EXIT=1 FAKE_CODEX_STDERR='rate limit reached' "$target/bin/agentic-loop" _supervise
grep -Eq '^60 queued open' "$state" || { echo '---DEBUG STATE---'; cat "$state"; echo '---DEBUG STATE_ROOT---'; find "$state_root" -maxdepth 3 -type f 2>/dev/null; echo '---DEBUG LOG---'; cat "$state_root/logs/issue-60.log" 2>/dev/null | tail -40; echo '---DEBUG COMMENTS---'; cat "$FAKE_GH_ROOT/$state_key.comments" 2>/dev/null; fail 'exhaustion should re-queue the Issue, not fail it'; }
if grep -Eq '^60 failed' "$state"; then fail 'exhaustion must not mark the Issue failed'; fi
[[ -r $state_root/pools/codex/exhausted ]] || fail 'pool exhaustion pause marker was not written'
[[ ! -r $state_root/agent-exhausted ]] || fail 'partial pool exhaustion must not write the global marker'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:exhausted' 'exhaustion was not recorded'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'pool=codex' 'exhaustion comment did not record the exhausted pool'
# Paused: with the only pool exhausted the next pass does not claim.
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^60 queued open' "$state" || fail 'exhaustion pause did not hold the Issue in queue'
[[ -e $state_root/all-pools-paused ]] || fail 'all-pools pause was not synthesized from the pool markers'
# Recovery: clearing the pool marker resumes claiming to completion.
rm -f "$state_root/pools/codex/exhausted"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^60 completed closed' "$state" || fail 'claiming did not resume after exhaustion cleared'

# Postmortem auto-trigger on resource exhaustion (Issue #132, ADR 0026): the
# same all-pools-paused transition synthesized above creates a dedup-checked
# postmortem candidate once [postmortem].auto_detect = "on" (code default:
# off, see config.sh); a further steady-state paused poll never duplicates it.
printf '\n[postmortem]\nauto_detect = "on"\nmax_auto_created_per_day = 5\n' >> "$target/.agentic-loop.toml"
rm -rf "$state_root/postmortem"
rm -f "$state_root/agent-exhausted" "$state_root/pools/codex/exhausted" "$state_root/all-pools-paused" "$FAKE_GH_ROOT/codex-exec-count"
printf '61 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_EXIT=1 FAKE_CODEX_STDERR='rate limit reached' "$target/bin/agentic-loop" _supervise
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
[[ -e $state_root/all-pools-paused ]] || fail 'postmortem exhaustion fixture setup did not reach all-pools-paused'
pm_issue=$(awk '$2 == "queued" && $1 != 61 {print $1}' "$state" | tail -n1)
[[ $pm_issue =~ ^[0-9]+$ ]] || fail 'resource-exhaustion transition did not auto-create a postmortem candidate Issue'
pm_body=$(awk -v n="$pm_issue" '$1 == n {print $8}' "$state" | base64 -d)
grep -Fq 'kind=resource-exhaustion' <<< "$pm_body" || fail 'auto-created postmortem Issue does not record kind=resource-exhaustion'
created_before=$(awk '$2 == "queued" && $1 != 61' "$state" | wc -l)
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
created_after=$(awk '$2 == "queued" && $1 != 61' "$state" | wc -l)
[[ $created_before -eq $created_after ]] || fail 'a steady-state paused poll created a duplicate postmortem candidate'
rm -f "$state_root/pools/codex/exhausted"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
rm -rf "$state_root/postmortem"

# Regression (Issue #158/#130): a SUCCESSFUL Claude plan whose output legitimately
# discusses rate limits / quota / 429 -- e.g. a scalability or finite-resource
# Issue -- must NOT be misread as pool exhaustion. Claude reports a real limit
# through the --output-format json envelope (is_error/api_error_status), not by
# echoing those words in the plan text; before the structured-error gate the
# plan prose matched the quota signatures, marked the whole claude pool spent,
# and blocked the entire queue for 30 minutes. The plan stage (first claude
# call) returns prose that mentions every signature; the exec stage completes.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
rm -rf "$state_root/pools"
rm -f "$state_root/all-pools-paused" "$state_root/agent-exhausted" "$FAKE_GH_ROOT/claude-json-count"
printf '62 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
plan_rate_limit_prose='この計画は有限資源を前提に処理量を設計する。rate limit、quota exceeded、HTTP 429、usage limit、too many requests、credit balance の安全弁を defense in depth として論じる。'
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_RESULT='AGENTIC_LOOP_RESULT=completed' \
  FAKE_CLAUDE_RESULT_1="$plan_rate_limit_prose" \
  "$target/bin/agentic-loop" _supervise
grep -Eq '^62 completed closed' "$state" || { echo '---DEBUG STATE---'; cat "$state"; echo '---DEBUG LOG---'; tail -40 "$state_root/logs/issue-62.log" 2>/dev/null; fail 'a plan that merely discusses rate limits was misclassified as pool exhaustion'; }
[[ ! -r $state_root/pools/claude/exhausted ]] || fail 'a successful plan mentioning rate limits must not mark the claude pool exhausted'
[[ ! -e $state_root/all-pools-paused ]] || fail 'a successful plan mentioning rate limits must not pause the supervisor'
if grep -Fq 'agentic-loop:exhausted' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'plan-text rate-limit mention was recorded as exhaustion'; fi

# Regression (Issue #226): a successful stage whose output discusses model
# failures must remain successful. Model-failure matching is gated by the
# provider exit/error state just like pool-exhaustion matching.
model_text_result="$TEST_ROOT/model-text-result"
printf '%s\n' 'The report explains how to detect an invalid model or overloaded provider.' > "$model_text_result"
if ( source "$PROJECT_ROOT/bin/lib/agentic-loop/agent.sh"; agent_result_is_model_failure "$model_text_result" 0 0 ); then
  fail 'successful output mentioning model failure was misclassified'
fi
if ! ( source "$PROJECT_ROOT/bin/lib/agentic-loop/agent.sh"; agent_result_is_model_failure "$model_text_result" 1 0 ); then
  fail 'non-zero model failure was not classified'
fi

# Complement (Issue #158): a genuine Claude usage-limit response -- is_error true
# with api_error_status set, the way the CLI really reports it under
# --output-format json -- IS pool exhaustion. The Issue is re-queued (never
# failed), the claude pool is marked spent, and claiming pauses so retries are
# not burned.
rm -rf "$state_root/pools"
rm -f "$state_root/all-pools-paused" "$state_root/agent-exhausted" "$FAKE_GH_ROOT/claude-json-count"
printf '63 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_IS_ERROR=1 FAKE_CLAUDE_API_ERROR_STATUS=429 \
  FAKE_CLAUDE_RESULT='Claude AI usage limit reached' \
  "$target/bin/agentic-loop" _supervise
grep -Eq '^63 queued open' "$state" || { echo '---DEBUG STATE---'; cat "$state"; echo '---DEBUG LOG---'; tail -40 "$state_root/logs/issue-63.log" 2>/dev/null; fail 'a real claude usage limit should re-queue the Issue, not fail it'; }
if grep -Eq '^63 failed' "$state"; then fail 'a claude usage limit must not mark the Issue failed'; fi
[[ -r $state_root/pools/claude/exhausted ]] || fail 'a real claude usage limit did not mark the claude pool exhausted'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:exhausted' 'claude usage limit was not recorded as exhaustion'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'pool=claude' 'claude exhaustion comment did not record the exhausted pool'
rm -rf "$state_root/pools"
rm -f "$state_root/all-pools-paused" "$FAKE_GH_ROOT/claude-json-count"

# API budget governor: while remaining REST(core) quota is below the reserve,
# claiming pauses (keeping budget for heartbeats/recovery); it resumes on recovery.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
printf '61 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested" "$state_root/core-budget-paused" "$state_root/graphql-rate-limit"
FAKE_CORE_REMAINING=100 AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^61 queued open' "$state" || fail 'low REST(core) budget did not pause claiming'
[[ -e $state_root/core-budget-paused ]] || fail 'core budget pause marker was not written'
rm -f "$state_root/graphql-rate-limit"
FAKE_CORE_REMAINING=5000 AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^61 completed closed' "$state" || fail 'claiming did not resume after REST(core) budget recovered'
[[ ! -e $state_root/core-budget-paused ]] || fail 'core budget pause marker was not cleared on recovery'

# --- Pool/tier priority fallback and per-pool exhaustion (Issue #155) ---
# Test fixtures: an opencode-go auth key (value must never leak) and a usage
# fixture dir the fake gh serves as the OpenCode Go usage API.
mkdir -p "$TEST_ROOT/godata/opencode"
printf '{"opencode-go":{"type":"api","key":"fake-gosecret-key-0123456789"}}\n' > "$TEST_ROOT/godata/opencode/auth.json"

# Tier priority: plan runs the first tier's model; exec keeps the scalar model,
# and the selected pool/model is recorded on the usage comment.
cat > "$target/.agentic-loop.toml" <<'TIER_TOML'
[agent]
provider = "codex"

[agent.plan]
[[agent.plan.tiers]]
pool = "plus"
provider = "codex"
reasoning_effort = "high"
models = [{ model = "gpt-5.6-sol", max_usage_percent = 60 }]

[[agent.plan.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "high"
models = [{ model = "opencode-go/gpt-5.6-luna", max_usage_percent = 60 }]

[agent.exec]
provider = "codex"
model = "gpt-5.6-terra"
reasoning_effort = "low"

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
TIER_TOML
printf '301 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
rm -f "$FAKE_GH_ROOT/codex-exec-count"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^301 completed closed' "$state" || fail 'tier-priority Issue did not complete'
assert_contains "$FAKE_GH_ROOT/codex-calls" '-c model=gpt-5.6-sol' 'plan did not use the first tier model'
assert_contains "$FAKE_GH_ROOT/codex-calls" '-c model=gpt-5.6-terra' 'exec did not keep the scalar model'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'pool=plus' 'plan usage did not record the selected pool'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'model=gpt-5.6-sol' 'plan usage did not record the selected model'

# Pool exhaustion skips only that pool: with the plus pool exhausted, plan falls
# back to the opencode (gogo) tier and the Issue still completes (no pause).
printf '302 queued open none 2026-01-01T00:00:00Z\n' > "$state"
rm -rf "$state_root/pools"
mkdir -p "$state_root/pools/plus"
printf '%s\n' "$(( $(date +%s) + 600 ))" > "$state_root/pools/plus/exhausted"
: > "$FAKE_GH_ROOT/codex-calls"
: > "$FAKE_GH_ROOT/opencode-calls"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_OPENCODE_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^302 completed closed' "$state" || fail 'pool-exhausted fallback Issue did not complete'
[[ $(grep -c -- '--sandbox read-only' "$FAKE_GH_ROOT/codex-calls") -eq 0 ]] || fail 'exhausted plan pool was not skipped'
assert_contains "$FAKE_GH_ROOT/opencode-calls" 'run --auto' 'plan did not fall back to the opencode tier'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'stage=plan' 'fallback plan usage was not recorded'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'provider=opencode' 'fallback plan did not record the opencode provider'
[[ ! -e $state_root/all-pools-paused ]] || fail 'partial pool exhaustion must not pause the supervisor'
rm -rf "$state_root/pools"

# Model-specific failure (overloaded) moves to the next model in the same pool
# instead of marking the pool exhausted or pausing globally.
cat > "$target/.agentic-loop.toml" <<'MODEL_TOML'
[agent.plan]
provider = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agent.exec]
[[agent.exec.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "low"
models = [
  { model = "opencode-go/gpt-5.6-luna", max_usage_percent = 60 },
  { model = "opencode-go/deepseek-v4-pro" },
]

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
MODEL_TOML
printf '303 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/opencode-calls"
rm -f "$FAKE_GH_ROOT/opencode-auto-count" "$FAKE_GH_ROOT/codex-exec-count"
FAKE_OPENCODE_EXEC_RESULT_1='overloaded' FAKE_OPENCODE_EXEC_EXIT_1=1 FAKE_OPENCODE_EXEC_RESULT_2='AGENTIC_LOOP_RESULT=completed' AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^303 completed closed' "$state" || fail 'model-failure fallback Issue did not complete'
assert_contains "$FAKE_GH_ROOT/opencode-calls" '--model opencode-go/gpt-5.6-luna' 'model-failure retry did not start on the first model'
assert_contains "$FAKE_GH_ROOT/opencode-calls" '--model opencode-go/deepseek-v4-pro' 'model-failure retry did not switch to the next model'
[[ ! -r $state_root/pools/gogo/exhausted ]] || fail 'model-specific failure must not mark the pool exhausted'
[[ ! -r $state_root/agent-exhausted ]] || fail 'model-specific failure must not write the global exhaustion marker'
if grep -Fq 'agentic-loop:replan' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'model-failure fallback triggered a replan'; fi

# Usage-threshold demotion and recovery: above max_usage_percent the pool's next
# model runs; when usage drops the preferred model returns (read via the fake
# OpenCode Go usage API).
cat > "$target/.agentic-loop.toml" <<'USAGE_TOML'
[agent.plan]
provider = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agent.exec]
[[agent.exec.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "low"
models = [
  { model = "opencode-go/gpt-5.6-luna", max_usage_percent = 60 },
  { model = "opencode-go/deepseek-v4-pro" },
]

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
USAGE_TOML
cat > "$TEST_ROOT/go-usage-high.json" <<'USAGE_HIGH'
{"usage":{"rolling":{"status":"ok","percent":90,"resetsAt":"2026-08-16T14:00:07.328Z"},"weekly":{"status":"ok","percent":4,"resetsAt":"2026-08-17T00:00:00.328Z"},"monthly":{"status":"ok","percent":2,"resetsAt":"2026-09-16T08:12:28.328Z"}}}
USAGE_HIGH
cat > "$TEST_ROOT/go-usage-low.json" <<'USAGE_LOW'
{"usage":{"rolling":{"status":"ok","percent":10,"resetsAt":"2026-08-16T14:00:07.328Z"},"weekly":{"status":"ok","percent":4,"resetsAt":"2026-08-17T00:00:00.328Z"},"monthly":{"status":"ok","percent":2,"resetsAt":"2026-09-16T08:12:28.328Z"}}}
USAGE_LOW
printf '304 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/opencode-calls"
rm -f "$FAKE_GH_ROOT/opencode-auto-count" "$FAKE_GH_ROOT/codex-exec-count" "$state_root/pools/usage-opencode"
XDG_DATA_HOME="$TEST_ROOT/godata" FAKE_GO_USAGE_FILE="$TEST_ROOT/go-usage-high.json" FAKE_OPENCODE_EXEC_RESULT_1='AGENTIC_LOOP_RESULT=completed' AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^304 completed closed' "$state" || fail 'usage-demoted Issue did not complete'
assert_contains "$FAKE_GH_ROOT/opencode-calls" '--model opencode-go/deepseek-v4-pro' 'usage over threshold did not demote to the cheaper model'
if grep -Fq -- '--model opencode-go/gpt-5.6-luna' "$FAKE_GH_ROOT/opencode-calls"; then fail 'usage over threshold still ran the preferred model'; fi
# Secret guard: the usage API key never leaks into comments, state, or the gh
# call transcript (which redacts the Authorization header).
if grep -rFq 'fake-gosecret-key-0123456789' "$state_root" "$FAKE_GH_ROOT/$state_key.comments" 2>/dev/null; then fail 'opencode-go usage key leaked into state or Issue comments'; fi
if grep -Fq 'fake-gosecret-key-0123456789' "$FAKE_GH_ROOT/calls"; then fail 'opencode-go usage key leaked into the gh call log'; fi
assert_contains "$FAKE_GH_ROOT/calls" 'Authorization: Bearer [REDACTED]' 'usage API Authorization header was not redacted in the gh call log'
[[ -s $state_root/pools/usage-opencode ]] || fail 'usage cache was not written'
if grep -Fq 'fake-gosecret-key-0123456789' "$state_root/pools/usage-opencode"; then fail 'usage cache contains the API key'; fi
# Recovery: with usage back under the threshold the preferred model returns.
printf '305 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/opencode-calls"
rm -f "$FAKE_GH_ROOT/opencode-auto-count" "$FAKE_GH_ROOT/codex-exec-count" "$state_root/pools/usage-opencode"
XDG_DATA_HOME="$TEST_ROOT/godata" FAKE_GO_USAGE_FILE="$TEST_ROOT/go-usage-low.json" FAKE_OPENCODE_EXEC_RESULT_1='AGENTIC_LOOP_RESULT=completed' AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^305 completed closed' "$state" || fail 'usage-recovered Issue did not complete'
assert_contains "$FAKE_GH_ROOT/opencode-calls" '--model opencode-go/gpt-5.6-luna' 'usage recovery did not restore the preferred model'

# All pools unavailable: only then is claiming globally paused and the Issue
# stays queued (never failed); clearing the pools resumes to completion.
cat > "$target/.agentic-loop.toml" <<'ALLPOOLS_TOML'
[agent]
provider = "codex"

[agent.plan]
[[agent.plan.tiers]]
pool = "plus"
provider = "codex"
reasoning_effort = "high"
models = [{ model = "gpt-5.6-sol" }]

[[agent.plan.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "high"
models = [{ model = "opencode-go/gpt-5.6-luna" }]

[agent.exec]
provider = "codex"
model = "gpt-5.6-terra"

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
ALLPOOLS_TOML
cat > "$TEST_ROOT/go-usage-exhausted.json" <<'USAGE_EXHAUSTED'
{"usage":{"rolling":{"status":"exhausted","percent":100,"resetsAt":"2026-08-16T14:00:07.328Z"},"weekly":{"status":"ok","percent":4,"resetsAt":"2026-08-17T00:00:00.328Z"},"monthly":{"status":"ok","percent":2,"resetsAt":"2026-09-16T08:12:28.328Z"}}}
USAGE_EXHAUSTED
printf '306 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -rf "$state_root/pools"
mkdir -p "$state_root/pools/plus" "$state_root/pools/gogo" "$state_root/pools/codex"
for pool in plus gogo codex; do printf '%s\n' "$(( $(date +%s) + 600 ))" > "$state_root/pools/$pool/exhausted"; done
XDG_DATA_HOME="$TEST_ROOT/godata" FAKE_GO_USAGE_FILE="$TEST_ROOT/go-usage-exhausted.json" AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^306 queued open' "$state" || fail 'all-pools exhaustion did not keep the Issue queued'
[[ -e $state_root/all-pools-paused ]] || fail 'all-pools pause was not synthesized from the pool markers'
XDG_DATA_HOME="$TEST_ROOT/godata" FAKE_GO_USAGE_FILE="$TEST_ROOT/go-usage-exhausted.json" AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^306 queued open' "$state" || fail 'all-pools pause did not hold the Issue in queue'
rm -rf "$state_root/pools"
rm -f "$state_root/agent-exhausted" "$state_root/all-pools-paused" "$state_root/pools/usage-opencode"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^306 completed closed' "$state" || fail 'claiming did not resume after all pools recovered'

# Backward compatibility: a scalar-only config normalizes to a single implicit
# tier whose pool is the provider name, and still completes plan/exec.
cat > "$target/.agentic-loop.toml" <<'SCALAR_TOML'
[agent]
provider = "codex"

[agent.plan]
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agent.exec]
model = "gpt-5.6-terra"
reasoning_effort = "low"

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
SCALAR_TOML
scalar_pick=$("$target/bin/agentic-loop" _pick-tier plan)
if ! grep -Fq 'pool=codex' <<< "$scalar_pick"; then fail 'scalar config did not normalize to the provider pool'; fi
if ! grep -Fq 'model=gpt-5.6-sol' <<< "$scalar_pick"; then fail 'scalar config did not keep the scalar model'; fi
if ! grep -Fq 'reasoning_effort=high' <<< "$scalar_pick"; then fail 'scalar config did not keep the plan reasoning effort'; fi
printf '307 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$FAKE_GH_ROOT/codex-exec-count"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^307 completed closed' "$state" || fail 'scalar-only Issue did not complete'

# Diagnosis honors tiers: the first diagnose tier's provider/model is used.
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.bak"
cat > "$target/.agentic-loop.toml" <<'DTOML'
[agent.diagnose]
[[agent.diagnose.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "low"
models = [{ model = "opencode-go/gpt-5.6-luna" }]
DTOML
: > "$FAKE_GH_ROOT/opencode-calls"
"$target/bin/agentic-loop-diagnose" >/dev/null
# shellcheck disable=SC2016 # The dollar-prefixed value is a literal skill invocation.
assert_contains "$FAKE_GH_ROOT/opencode-calls" 'Use $diagnose-codebase' 'diagnosis did not use the first diagnose tier'
assert_contains "$FAKE_GH_ROOT/opencode-calls" '--model opencode-go/gpt-5.6-luna' 'diagnosis did not pass the tier model'
mv "$target/.agentic-loop.toml.bak" "$target/.agentic-loop.toml"

# doctor validates the tiers schema: unknown providers and out-of-range
# max_usage_percent are reported as failures.
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.bak"
cat > "$target/.agentic-loop.toml" <<'BADTOML'
[agent.plan]
[[agent.plan.tiers]]
pool = "plus"
provider = "bogus"
models = [{ model = "m1", max_usage_percent = 120 }]
BADTOML
doctor_out=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '[失敗] tiers設定 (plan)' <<< "$doctor_out" || fail 'doctor did not flag the invalid tiers schema'
mv "$target/.agentic-loop.toml.bak" "$target/.agentic-loop.toml"

# Runtime pool exhaustion falls through to the next tier inside the same stage
# (plan and exec). Codex's real usage-limit path exits non-zero and writes the
# diagnostic only to stderr with an empty --output-last-message; the worker
# must still classify it, mark the plus pool exhausted, and finish on gogo
# without requeueing or failing the Issue.
cat > "$target/.agentic-loop.toml" <<'RUNTIME_FALLBACK_TOML'
[agent]
provider = "codex"

[agent.plan]
[[agent.plan.tiers]]
pool = "plus"
provider = "codex"
reasoning_effort = "high"
models = [{ model = "gpt-5.6-sol" }]

[[agent.plan.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "high"
models = [{ model = "opencode-go/gpt-5.6-luna" }]

[agent.exec]
[[agent.exec.tiers]]
pool = "plus"
provider = "codex"
reasoning_effort = "low"
models = [{ model = "gpt-5.6-terra" }]

[[agent.exec.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "low"
models = [{ model = "opencode-go/deepseek-v4-flash" }]

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
RUNTIME_FALLBACK_TOML
printf '308 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
: > "$FAKE_GH_ROOT/opencode-calls"
rm -rf "$state_root/pools"
rm -f "$FAKE_GH_ROOT/codex-exec-count" "$FAKE_GH_ROOT/opencode-auto-count" \
  "$state_root/agent-exhausted" "$state_root/all-pools-paused"
FAKE_CODEX_EXIT=1 \
FAKE_CODEX_STDERR="ERROR: You've hit your usage limit. Upgrade to Pro or try again later." \
FAKE_OPENCODE_EXEC_RESULT_1='plan body from gogo
<!-- agentic-loop:scope paths=bin/agentic-loop -->' \
FAKE_OPENCODE_EXEC_RESULT_2='AGENTIC_LOOP_RESULT=completed' \
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^308 completed closed' "$state" || fail 'runtime pool exhaustion did not fall back and complete'
[[ -r $state_root/pools/plus/exhausted ]] || fail 'runtime exhaustion did not mark the plus pool'
[[ ! -r $state_root/pools/gogo/exhausted ]] || fail 'runtime exhaustion must not mark the fallback pool'
[[ ! -e $state_root/all-pools-paused ]] || fail 'partial runtime exhaustion must not pause the supervisor'
if grep -Fq 'agentic-loop:failed' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'runtime exhaustion must not fail the Issue'; fi
if grep -Fq 'agentic-loop:exhausted' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'in-stage fallback must not requeue as exhausted'; fi
assert_contains "$FAKE_GH_ROOT/opencode-calls" '--model opencode-go/gpt-5.6-luna' 'plan did not fall back to gogo after runtime exhaustion'
assert_contains "$FAKE_GH_ROOT/opencode-calls" '--model opencode-go/deepseek-v4-flash' 'exec did not fall back to gogo after runtime exhaustion'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'provider=opencode' 'fallback usage did not record the opencode provider'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'pool=gogo' 'fallback usage did not record the gogo pool'
# Codex was attempted (and failed) before the fallback; it must not be the
# provider of the successful completion path.
[[ $(grep -c -- '--sandbox read-only' "$FAKE_GH_ROOT/codex-calls" || true) -ge 1 ]] || fail 'runtime exhaustion never attempted the preferred plan pool'
[[ $(grep -c -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || true) -eq 0 ]] || fail 'exec still ran on the exhausted plus pool after plan marked it'

# status next-candidate preview must honor pool exhaustion markers (network-
# free, same contract as agent_pick_tier) so operators see the real fallback.
rm -rf "$state_root/pools"
mkdir -p "$state_root/pools/plus"
printf '%s\n' "$(( $(date +%s) + 600 ))" > "$state_root/pools/plus/exhausted"
status_out=$("$target/bin/agentic-loop" status)
grep -Fq '次のplan候補: pool=gogo provider=opencode model=opencode-go/gpt-5.6-luna' <<< "$status_out" \
  || fail "status did not show the fallback plan candidate after plus exhaustion (got: $(printf '%s' "$status_out" | head -n 5 | tr '\n' '|'))"
grep -Fq '次のexec候補: pool=gogo provider=opencode model=opencode-go/deepseek-v4-flash' <<< "$status_out" \
  || fail 'status did not show the fallback exec candidate after plus exhaustion'
status_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$status_json" | yq -p json '.next_candidates.plan.pool') == gogo ]] \
  || fail 'status JSON did not report the fallback plan pool'
[[ $(printf '%s' "$status_json" | yq -p json '.next_candidates.plan.provider') == opencode ]] \
  || fail 'status JSON did not report the fallback plan provider'
[[ $(printf '%s' "$status_json" | yq -p json '.next_candidates.plan.model') == opencode-go/gpt-5.6-luna ]] \
  || fail 'status JSON did not report the fallback plan model'
[[ $(printf '%s' "$status_json" | yq -p json '.next_candidates.exec.pool') == gogo ]] \
  || fail 'status JSON did not report the fallback exec pool'
[[ $(printf '%s' "$status_json" | yq -p json '.next_candidates.exec.model') == opencode-go/deepseek-v4-flash ]] \
  || fail 'status JSON did not report the fallback exec model'

# A single exhausted pool is degraded capacity, not a global claim pause. The
# same fixture already proved above that _supervise actually claims via gogo;
# status must report the same decision in both text and JSON (Issue #288).
printf '309 queued open none 2026-01-01T00:00:00Z\n' > "$state"
status_out=$("$target/bin/agentic-loop" status)
grep -Fq '利用不可pool: pool=plus 枯渇' <<< "$status_out" \
  || fail 'status text did not expose a partially exhausted pool as degraded capacity'
! grep -Fq 'claim-paused supervisor' <<< "$status_out" \
  || fail 'status globally paused claims when only one of two pools was exhausted'
grep -Fq '#309 Fake issue 309 (claimable)' <<< "$status_out" \
  || fail 'status did not agree with the supervisor that the queued Issue can use the fallback pool'
status_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$status_json" | yq -p json '.queue.claimable') -eq 1 ]] \
  || fail 'status JSON hid a claimable Issue during partial pool exhaustion'
[[ $(printf '%s' "$status_json" | yq -p json '[.anomalies[] | select(.code == "claim-paused")] | length') -eq 0 ]] \
  || fail 'status JSON reported global claim-paused during partial pool exhaustion'

# Even a stale transition marker must not override the currently usable pool.
: > "$state_root/all-pools-paused"
status_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$status_json" | yq -p json '.queue.claimable') -eq 1 ]] \
  || fail 'a stale all-pools-paused marker hid a currently usable fallback pool'
rm -f "$state_root/all-pools-paused"

# When every configured pool is exhausted, status retains the global gate.
for exhausted_pool in gogo codex; do
  mkdir -p "$state_root/pools/$exhausted_pool"
  printf '%s\n' "$(( $(date +%s) + 600 ))" > "$state_root/pools/$exhausted_pool/exhausted"
done
status_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$status_json" | yq -p json '.queue.claimable') -eq 0 ]] \
  || fail 'status JSON exposed a claimable Issue while every pool was exhausted'
[[ $(printf '%s' "$status_json" | yq -p json '.queue.candidates[0].withheld') == claim-paused ]] \
  || fail 'all-pool exhaustion was not classified as claim-paused'
[[ $(printf '%s' "$status_json" | yq -p json '[.anomalies[] | select(.code == "claim-paused")] | length') -eq 1 ]] \
  || fail 'all-pool exhaustion did not produce the global claim-paused anomaly'
rm -rf "$state_root/pools/gogo" "$state_root/pools/codex"

# A full worker pool is a slot constraint, never a provider-exhaustion claim.
mkdir -p "$state_root/workers"
sleep 30 & status_slot_pid=$!
printf '%s\n' "$status_slot_pid" > "$state_root/workers/999.pid"
status_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$status_json" | yq -p json '.queue.candidates[0].withheld') == worker-slots-full ]] \
  || fail 'max_workers saturation was not reported as worker-slots-full'
kill "$status_slot_pid" 2>/dev/null || true
wait "$status_slot_pid" 2>/dev/null || true
rm -f "$state_root/workers/999.pid"
# Clearing the marker restores the preferred candidate.
rm -rf "$state_root/pools"
status_out=$("$target/bin/agentic-loop" status)
grep -Fq '次のplan候補: pool=plus provider=codex model=gpt-5.6-sol' <<< "$status_out" \
  || fail 'status did not restore the preferred plan candidate after recovery'
grep -Fq '次のexec候補: pool=plus provider=codex model=gpt-5.6-terra' <<< "$status_out" \
  || fail 'status did not restore the preferred exec candidate after recovery'
# Empty-result + non-zero exit (Codex hard failure with no last message) is
# still classified as pool exhaustion even without a matching body, so a
# single-pool config requeues rather than failing.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '309 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -rf "$state_root/pools"
rm -f "$state_root/agent-exhausted" "$state_root/all-pools-paused" "$FAKE_GH_ROOT/codex-exec-count"
FAKE_CODEX_EXIT=1 FAKE_CODEX_STDERR="ERROR: You've hit your usage limit." \
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^309 queued open' "$state" || fail 'empty-result usage-limit exit did not requeue the Issue'
if grep -Eq '^309 failed' "$state"; then fail 'empty-result usage-limit exit must not fail the Issue'; fi
[[ -r $state_root/pools/codex/exhausted ]] || fail 'empty-result usage-limit exit did not mark the codex pool'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:exhausted' 'empty-result usage-limit exit was not recorded as exhausted'
# Provider-stated multi-day reset must outlive the short default pause: Codex
# "try again at Aug 20th, 2026 9:27 PM" is stored as resume_epoch, status keeps
# showing the fallback candidate, and a premature usage "recovered" reading
# must not clear the marker before that epoch.
cat > "$target/.agentic-loop.toml" <<'RESET_TOML'
[agent]
provider = "codex"

[agent.plan]
[[agent.plan.tiers]]
pool = "plus"
provider = "codex"
reasoning_effort = "high"
models = [{ model = "gpt-5.6-sol" }]

[[agent.plan.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "high"
models = [{ model = "opencode-go/gpt-5.6-luna" }]

[agent.exec]
[[agent.exec.tiers]]
pool = "plus"
provider = "codex"
reasoning_effort = "low"
models = [{ model = "gpt-5.6-terra" }]

[[agent.exec.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "low"
models = [{ model = "opencode-go/deepseek-v4-flash" }]

[queue]
poll_seconds = 1
max_workers = 1
lease_seconds = 3
stop_timeout = 10
stale_days = 30
RESET_TOML
printf '310 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
: > "$FAKE_GH_ROOT/opencode-calls"
rm -rf "$state_root/pools"
rm -f "$FAKE_GH_ROOT/codex-exec-count" "$FAKE_GH_ROOT/opencode-auto-count" \
  "$state_root/agent-exhausted" "$state_root/all-pools-paused"
expected_reset=$(date -d 'Aug 20, 2099 9:27 PM' +%s)
FAKE_CODEX_EXIT=1 \
FAKE_CODEX_STDERR="2026-08-20T05:12:33Z ERROR: You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Aug 20th, 2099 9:27 PM." \
FAKE_OPENCODE_EXEC_RESULT_1='plan body from gogo
<!-- agentic-loop:scope paths=bin/agentic-loop -->' \
FAKE_OPENCODE_EXEC_RESULT_2='AGENTIC_LOOP_RESULT=completed' \
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^310 completed closed' "$state" || fail 'reset-epoch exhaustion did not fall back and complete'
[[ -r $state_root/pools/plus/exhausted ]] || fail 'reset-epoch exhaustion did not mark the plus pool'
resume_epoch=$(cat "$state_root/pools/plus/exhausted")
[[ $resume_epoch =~ ^[0-9]+$ ]] || fail 'plus exhausted marker is not a resume epoch'
(( resume_epoch >= expected_reset - 120 && resume_epoch <= expected_reset + 120 )) \
  || fail "plus exhausted marker did not honor provider reset (got $resume_epoch want ~$expected_reset)"
(( resume_epoch > $(date +%s) + 1800 )) \
  || fail 'provider reset epoch must outlive the short default pause'
# status must keep advertising the fallback while the long marker is held.
status_out=$("$target/bin/agentic-loop" status)
grep -Fq '次のplan候補: pool=gogo provider=opencode model=opencode-go/gpt-5.6-luna' <<< "$status_out" \
  || fail 'status dropped the fallback plan candidate before the provider reset epoch'
grep -Fq '次のexec候補: pool=gogo provider=opencode model=opencode-go/deepseek-v4-flash' <<< "$status_out" \
  || fail 'status dropped the fallback exec candidate before the provider reset epoch'
# A second claim must not re-attempt the spent plus pool (marker still binding
# even if a stale codex session log would read as "recovered").
printf '311 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/codex-calls"
: > "$FAKE_GH_ROOT/opencode-calls"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$FAKE_GH_ROOT/codex-exec-count" "$FAKE_GH_ROOT/opencode-auto-count"
FAKE_OPENCODE_EXEC_RESULT_1='plan body from gogo
<!-- agentic-loop:scope paths=bin/agentic-loop -->' \
FAKE_OPENCODE_EXEC_RESULT_2='AGENTIC_LOOP_RESULT=completed' \
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^311 completed closed' "$state" || fail 'second claim under long reset epoch did not complete on fallback'
[[ $(grep -c -- '--sandbox read-only' "$FAKE_GH_ROOT/codex-calls" || true) -eq 0 ]] \
  || fail 'long reset epoch still attempted the spent plus plan pool'
[[ $(grep -c -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || true) -eq 0 ]] \
  || fail 'long reset epoch still attempted the spent plus exec pool'
assert_contains "$FAKE_GH_ROOT/opencode-calls" '--model opencode-go/gpt-5.6-luna' 'second claim did not stay on gogo plan'
# After resume_epoch, measured recovery clears the marker and restores plus.
printf '%s\n' "$(( $(date +%s) - 10 ))" > "$state_root/pools/plus/exhausted"
# codex weekly usage is unreadable in the fixture (no session log) so the
# post-epoch path treats it as a safe retry clear; force a recovered reading
# by writing a synthetic session log under a temporary CODEX_HOME.
codex_home_reset="$TEST_ROOT/codex-home-reset"
mkdir -p "$codex_home_reset/sessions"
printf '%s\n' '{"payload":{"type":"token_count","rate_limits":{"secondary":{"used_percent":10}}}}' \
  > "$codex_home_reset/sessions/rollout-reset.jsonl"
export CODEX_HOME="$codex_home_reset"
# status is marker-only (no usage probe); pick-tier is what clears. Drive it.
pick=$("$target/bin/agentic-loop" _pick-tier plan)
grep -Fq 'pool=plus' <<< "$pick" || fail 'post-epoch recovered usage did not restore the preferred plan pool'
[[ ! -r $state_root/pools/plus/exhausted ]] || fail 'post-epoch recovered usage did not clear the plus marker'
# Still-exhausted measurement after resume_epoch must extend, not clear.
printf '%s\n' "$(( $(date +%s) - 10 ))" > "$state_root/pools/plus/exhausted"
printf '%s\n' '{"payload":{"type":"token_count","rate_limits":{"secondary":{"used_percent":100}}}}' \
  > "$codex_home_reset/sessions/rollout-reset.jsonl"
before_extend=$(date +%s)
pick=$("$target/bin/agentic-loop" _pick-tier plan)
grep -Fq 'pool=gogo' <<< "$pick" || fail 'post-epoch still-exhausted usage did not keep the fallback pool'
[[ -r $state_root/pools/plus/exhausted ]] || fail 'post-epoch still-exhausted usage cleared the plus marker'
extended=$(cat "$state_root/pools/plus/exhausted")
(( extended >= before_extend + 1800 - 5 )) \
  || fail "post-epoch still-exhausted usage did not extend the marker (got $extended)"
unset CODEX_HOME
rm -rf "$state_root/pools" "$codex_home_reset"
rm -f "$state_root/agent-exhausted" "$state_root/all-pools-paused"

# --- Exponential backoff for a pool no provider can measure (Issue #158) ---
# claude does have a usage probe (claude_probe_usage_percent, Issue #251), but
# this fixture leaves FAKE_CLAUDE_PROBE_MODE unset, so the fake CLI reports the
# probe itself as unreadable (mirrors a real environment where the probe can
# never get a clean reading, e.g. offline/misconfigured) -- exactly the
# pre-#251 "no case at all" behavior. With no usage signal either way, a
# repeated re-exhaustion must grow the pause (not repeat the flat
# EXHAUSTION_PAUSE_SECONDS forever), capped, and reset by a genuine success.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
rm -rf "$state_root/pools"
rm -f "$state_root/all-pools-paused" "$state_root/agent-exhausted" "$FAKE_GH_ROOT/claude-json-count" "$FAKE_GH_ROOT/claude-probe-calls"
printf '330 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_IS_ERROR=1 FAKE_CLAUDE_API_ERROR_STATUS=429 \
  FAKE_CLAUDE_RESULT='Claude AI usage limit reached' \
  "$target/bin/agentic-loop" _supervise
[[ -r $state_root/pools/claude/exhausted ]] || fail 'first claude exhaustion did not mark the pool'
first_resume=$(cat "$state_root/pools/claude/exhausted")
now1=$(date +%s)
(( first_resume >= now1 + 1800 - 5 && first_resume <= now1 + 1800 + 30 )) \
  || fail "first claude exhaustion should use the flat default pause (got $first_resume, now $now1)"
[[ -r $state_root/pools/claude/basis && $(cat "$state_root/pools/claude/basis") == backoff ]] \
  || fail 'first claude exhaustion did not record basis=backoff'
# Rewind the marker so resume_epoch is already past, then re-exhaust: the
# streak must grow the NEXT pause instead of repeating the flat 1800s.
printf '%s\n' "$(( $(date +%s) - 10 ))" > "$state_root/pools/claude/exhausted"
printf '331 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_IS_ERROR=1 FAKE_CLAUDE_API_ERROR_STATUS=429 \
  FAKE_CLAUDE_RESULT='Claude AI usage limit reached' \
  "$target/bin/agentic-loop" _supervise
[[ -r $state_root/pools/claude/streak && $(cat "$state_root/pools/claude/streak") == 2 ]] \
  || fail 'consecutive claude re-exhaustion did not grow the streak to 2'
second_resume=$(cat "$state_root/pools/claude/exhausted")
now2=$(date +%s)
(( second_resume >= now2 + 3600 - 5 )) \
  || fail "second consecutive claude exhaustion did not back off beyond the flat pause (got $second_resume, now $now2)"
# A large existing streak must never push the pause past the configured cap.
printf '20\n' > "$state_root/pools/claude/streak"
printf '%s\n' "$(( $(date +%s) - 10 ))" > "$state_root/pools/claude/exhausted"
printf '332 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_IS_ERROR=1 FAKE_CLAUDE_API_ERROR_STATUS=429 \
  FAKE_CLAUDE_RESULT='Claude AI usage limit reached' \
  "$target/bin/agentic-loop" _supervise
capped_resume=$(cat "$state_root/pools/claude/exhausted")
now3=$(date +%s)
(( capped_resume <= now3 + 21600 + 30 )) \
  || fail "claude backoff exceeded the configured ceiling (got $capped_resume, now $now3)"
# A genuine success on the pool is real evidence it works, so it resets the
# streak -- an unrelated later exhaustion must start from the short default
# pause again, not continue growing from where a past, unrelated streak left
# off.
rm -f "$state_root/pools/claude/exhausted"
printf '333 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_RESULT='AGENTIC_LOOP_RESULT=completed' \
  "$target/bin/agentic-loop" _supervise
grep -Eq '^333 completed closed' "$state" || fail 'claude success did not complete'
[[ ! -r $state_root/pools/claude/streak ]] || fail 'a genuine success did not clear the backoff streak'
rm -rf "$state_root/pools"
rm -f "$state_root/all-pools-paused" "$state_root/agent-exhausted" "$FAKE_GH_ROOT/claude-json-count" "$FAKE_GH_ROOT/claude-probe-calls"

# --- Claude pool recovery probe (Issue #251) ---
# claude has no passive usage telemetry, so recovery can only be confirmed by
# actually attempting a minimal headless call (claude_probe_usage_percent,
# fronted by claude_probe_usage_cached). The fake claude CLI models this via
# AGENTIC_LOOP_PROBE=1 + FAKE_CLAUDE_PROBE_MODE, independent of the ordinary
# plan/exec stage envelope (FAKE_CLAUDE_IS_ERROR/FAKE_CLAUDE_RESULT).

# 1) A recovered probe ends the pause immediately, even before resume_epoch --
# this is what actually fixes the reported incident: exponential backoff alone
# cannot know a spend-limit 429 has already cleared.
printf '340 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_IS_ERROR=1 FAKE_CLAUDE_API_ERROR_STATUS=429 \
  FAKE_CLAUDE_RESULT='individual spend limit' \
  "$target/bin/agentic-loop" _supervise
[[ -r $state_root/pools/claude/exhausted ]] || fail 'probe-recovery setup: first claude exhaustion did not mark the pool'
resume_before=$(cat "$state_root/pools/claude/exhausted")
now_before=$(date +%s)
(( resume_before > now_before + 1000 )) || fail 'probe-recovery setup: resume_epoch is not safely in the future'
printf '341 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 FAKE_CLAUDE_PROBE_MODE=ok \
  FAKE_CLAUDE_RESULT='AGENTIC_LOOP_RESULT=completed' \
  "$target/bin/agentic-loop" _supervise
grep -Eq '^341 completed closed' "$state" \
  || fail 'a recovered usage probe did not resume claiming before resume_epoch'
[[ -r $FAKE_GH_ROOT/claude-probe-calls ]] || fail 'probe-recovery test did not actually invoke the probe'
[[ ! -r $state_root/pools/claude/exhausted ]] || fail 'a recovered usage probe did not clear the marker before resume_epoch'
[[ ! -r $state_root/pools/claude/streak ]] || fail 'a recovered usage probe did not clear the backoff streak'
rm -rf "$state_root/pools"
rm -f "$FAKE_GH_ROOT/claude-json-count" "$FAKE_GH_ROOT/claude-probe-calls"

# 2) A probe that confirms the pool is still exhausted switches basis to
# "probe" and leaves the backoff streak frozen (cleared, never bumped): once a
# real probe exists, exponential backoff must not keep growing.
printf '342 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_IS_ERROR=1 FAKE_CLAUDE_API_ERROR_STATUS=429 \
  FAKE_CLAUDE_RESULT='individual spend limit' \
  "$target/bin/agentic-loop" _supervise
[[ -r $state_root/pools/claude/exhausted ]] || fail 'probe-frozen-streak setup: first claude exhaustion did not mark the pool'
[[ $(cat "$state_root/pools/claude/streak") == 1 ]] || fail 'probe-frozen-streak setup: streak did not start at 1'
# Rewind resume_epoch to the past so the still-exhausted probe path (not the
# pre-epoch active-probe bypass) is what re-confirms exhaustion.
printf '%s\n' "$(( $(date +%s) - 10 ))" > "$state_root/pools/claude/exhausted"
printf '343 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 FAKE_CLAUDE_PROBE_MODE=exhausted \
  "$target/bin/agentic-loop" _supervise
grep -Eq '^343 queued open' "$state" \
  || fail 'a probe-confirmed still-exhausted pool must keep claiming paused'
[[ -r $FAKE_GH_ROOT/claude-probe-calls ]] || fail 'probe-confirmed-exhausted test did not actually invoke the probe'
[[ $(cat "$state_root/pools/claude/basis") == probe ]] \
  || fail 'a probe-confirmed still-exhausted pool did not switch basis to probe'
[[ ! -r $state_root/pools/claude/streak ]] \
  || fail 'a probe-confirmed still-exhausted pool must not grow (or keep) the backoff streak'
rm -rf "$state_root/pools"
rm -f "$FAKE_GH_ROOT/claude-json-count" "$FAKE_GH_ROOT/claude-probe-calls"

# 3) When the probe itself cannot get a reading, the pool falls back exactly
# to the pre-#251 backoff/blind-retry behavior -- an unreadable probe must
# never be guessed as recovered or exhausted.
printf '344 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_IS_ERROR=1 FAKE_CLAUDE_API_ERROR_STATUS=429 \
  FAKE_CLAUDE_RESULT='individual spend limit' \
  "$target/bin/agentic-loop" _supervise
[[ -r $state_root/pools/claude/exhausted ]] || fail 'probe-unreadable setup: first claude exhaustion did not mark the pool'
resume_before=$(cat "$state_root/pools/claude/exhausted")
now_before=$(date +%s)
(( resume_before > now_before + 1000 )) || fail 'probe-unreadable setup: resume_epoch is not safely in the future'
printf '345 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 FAKE_CLAUDE_PROBE_MODE=unreadable \
  "$target/bin/agentic-loop" _supervise
grep -Eq '^345 queued open' "$state" \
  || fail 'an unreadable probe must not end the pause before resume_epoch'
[[ -r $FAKE_GH_ROOT/claude-probe-calls ]] || fail 'probe-unreadable test did not actually attempt the probe'
[[ -r $state_root/pools/claude/exhausted ]] || fail 'an unreadable probe must not clear the pre-epoch marker'
[[ $(cat "$state_root/pools/claude/basis") == backoff ]] \
  || fail 'an unreadable probe must not change the basis away from backoff'
rm -rf "$state_root/pools"
rm -f "$FAKE_GH_ROOT/claude-json-count" "$FAKE_GH_ROOT/claude-probe-calls"

# 4) A pool a real probe keeps confirming as exhausted (no provider-stated
# reset) escalates once, after PROBE_ESCALATE_AFTER consecutive confirmations,
# instead of backing off silently forever (Issue #251 completion criterion 5:
# a spend-limit-type 429 never resolves by elapsed time, so a human must be
# told). auto_detect defaults to off, so no postmortem Issue is actually
# created here (covered separately below); the local escalation marker is
# still written unconditionally once the threshold is crossed.
printf '346 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 \
  FAKE_CLAUDE_IS_ERROR=1 FAKE_CLAUDE_API_ERROR_STATUS=429 \
  FAKE_CLAUDE_RESULT='individual spend limit' \
  "$target/bin/agentic-loop" _supervise
[[ -r $state_root/pools/claude/exhausted ]] || fail 'escalation setup: first claude exhaustion did not mark the pool'
[[ ! -e $state_root/pools/claude/probe-escalated ]] || fail 'escalation setup: pool was escalated too early'
escalate_issue=347
for _ in 1 2 3; do
  printf '%s\n' "$(( $(date +%s) - 10 ))" > "$state_root/pools/claude/exhausted"
  printf '%s queued open none 2026-01-01T00:00:00Z\n' "$escalate_issue" > "$state"
  : > "$FAKE_GH_ROOT/$state_key.comments"
  AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 FAKE_CLAUDE_PROBE_MODE=exhausted \
    "$target/bin/agentic-loop" _supervise
  escalate_issue=$((escalate_issue + 1))
done
[[ -r $state_root/pools/claude/probe-confirmed && $(cat "$state_root/pools/claude/probe-confirmed") == 3 ]] \
  || fail 'escalation did not count 3 consecutive probe-confirmed re-exhaustions'
[[ -e $state_root/pools/claude/probe-escalated ]] \
  || fail 'a pool confirmed exhausted 3 times running did not escalate for human attention'
rm -rf "$state_root/pools"
rm -f "$state_root/all-pools-paused" "$state_root/agent-exhausted" "$FAKE_GH_ROOT/claude-json-count" "$FAKE_GH_ROOT/claude-probe-calls"

# --- status shows pool recovery ETA and basis (Issue #158) ---
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
rm -rf "$state_root/pools"
mkdir -p "$state_root/pools/codex"
status_resume_at=$(( $(date +%s) + 5400 ))
printf '%s\n' "$status_resume_at" > "$state_root/pools/codex/exhausted"
printf 'backoff\n' > "$state_root/pools/codex/basis"
status_pool_text=$("$target/bin/agentic-loop" status)
status_resume_human=$(date -d "@$status_resume_at" '+%Y-%m-%d %H:%M:%S')
grep -Fq "pool=codex 枯渇（回復予定=$status_resume_human, 根拠=実測不能のため指数backoff）" <<< "$status_pool_text" \
  || fail 'status text did not show the pool recovery ETA and basis'
status_pool_json=$("$target/bin/agentic-loop" status --format json)
grep -Fq "{\"pool\":\"codex\",\"exhausted\":true,\"resume_at\":$status_resume_at,\"basis\":\"backoff\",\"probed_at\":null}" <<< "$status_pool_json" \
  || fail 'status --format json did not report the pool recovery ETA and basis'
rm -rf "$state_root/pools"

# --- status also shows the last real usage measurement time, distinguishing a
# real probe/telemetry reading from a blind backoff guess (Issue #251
# completion criterion 6). Reuses the already-declared codex pool (see the
# active USAGE_TOML config above) so agent_all_pools recognizes it; the
# generic pools/usage-<provider> cache file this reads is the same one
# opencode_go_usage_cached/claude_probe_usage_cached maintain.
mkdir -p "$state_root/pools/codex"
status_resume_at=$(( $(date +%s) + 1800 ))
status_probed_at=$(( $(date +%s) - 120 ))
printf '%s\n' "$status_resume_at" > "$state_root/pools/codex/exhausted"
printf 'probe\n' > "$state_root/pools/codex/basis"
printf '%s\t100\n' "$status_probed_at" > "$state_root/pools/usage-codex"
status_pool_text=$("$target/bin/agentic-loop" status)
status_resume_human=$(date -d "@$status_resume_at" '+%Y-%m-%d %H:%M:%S')
status_probed_human=$(date -d "@$status_probed_at" '+%Y-%m-%d %H:%M:%S')
grep -Fq "pool=codex 枯渇（回復予定=$status_resume_human, 根拠=使用率再probeの実測, 直近実測=$status_probed_human）" <<< "$status_pool_text" \
  || fail 'status text did not show the last usage-probe measurement time'
status_pool_json=$("$target/bin/agentic-loop" status --format json)
grep -Fq "{\"pool\":\"codex\",\"exhausted\":true,\"resume_at\":$status_resume_at,\"basis\":\"probe\",\"probed_at\":$status_probed_at}" <<< "$status_pool_json" \
  || fail 'status --format json did not report the last usage-probe measurement time'
rm -rf "$state_root/pools"

# --- Change-scope conflict avoidance (Issue #44) ---

write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=3 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# Same file without a structural flag: two Issues declaring the same path are a
# soft overlap and run in parallel (any real conflict converges through the
# existing rebase/re-validation path) instead of serializing; an independent-
# scope Issue is claimed alongside them in the same pass.
printf '201 queued open none 2026-01-01T00:00:00Z none none %s\n202 queued open none 2026-01-02T00:00:00Z none none %s\n203 queued open none 2026-01-03T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=bin/agentic-loop')" "$(scope_field 'paths=bin/agentic-loop')" "$(scope_field 'paths=docs/operations')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^201 completed closed' "$state" || fail 'same-path parallel: the first declared Issue was not claimed'
grep -Eq '^202 completed closed' "$state" || fail 'same-path parallel: a same-path Issue stayed serialized behind the first (soft overlap must run in parallel)'
grep -Eq '^203 completed closed' "$state" || fail 'independent-scope Issue was not claimed alongside the same-path pair'
if grep -Fq 'agentic-loop:scope-conflict' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'soft same-path overlap was wrongly serialized as a scope conflict'; fi
[[ ! -e $state_root/conflict/issue-202 ]] || fail 'soft same-path overlap wrongly persisted a conflict-wait state'

# Multi-host convergence: status must not display a machine-local wait after
# another host has claimed the waiting Issue and completed its counterpart.
printf '54 completed closed none 2026-01-01T00:00:00Z none none %s\n91 running open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=README.md')" "$(scope_field 'paths=README.md')" > "$state"
mkdir -p "$state_root/conflict" "$state_root/scope"
printf '54\tREADME.md\n' > "$state_root/conflict/issue-91"
printf 'path:README.md\n' > "$state_root/scope/issue-54"
status_output=$("$target/bin/agentic-loop" status)
if grep -Fq '#91 競合相手 #54' <<< "$status_output"; then fail 'status displayed a stale multi-host conflict for a non-queued Issue'; fi
[[ -e $state_root/conflict/issue-91 ]] || fail 'read-only status mutated a stale conflict cache file'

# A long-running Supervisor must converge after another host completes the
# blocker; startup-only rebuilding is insufficient because the transition can
# happen between polls. The queued Issue must be claimed without a restart.
printf '54 running open none 2026-01-01T00:00:00Z none none %s\n70 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=bin/agentic-loop structural=1')" "$(scope_field 'paths=bin/agentic-loop')" > "$state"
printf '54 <!-- agentic-loop:lease worker=remote-scope-fixture heartbeat=%s expires=%s -->\n' "$(date +%s)" "$(($(date +%s) + 3600))" > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/conflict/issue-91" "$state_root/stop.requested"
"$target/bin/agentic-loop" _supervise &
scope_supervisor_pid=$!
scope_wait_seen=0
wait_for 'scope wait seen' test -r "$state_root/conflict/issue-70" && scope_wait_seen=1
(( scope_wait_seen == 1 )) || fail 'multi-host fixture did not first persist the legitimate structural scope conflict'
printf '54 completed closed none 2026-01-01T00:00:00Z none none %s\n70 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=bin/agentic-loop')" "$(scope_field 'paths=bin/agentic-loop')" > "$state.transition"
mv "$state.transition" "$state"
scope_claimed=0
wait_for 'scope claimed' grep -Eq '^70 completed closed' "$state" && scope_claimed=1
: > "$state_root/stop.requested"
wait "$scope_supervisor_pid"
(( scope_claimed == 1 )) || fail 'queued Issue stayed blocked after the remote blocker completed'
[[ ! -e $state_root/scope/issue-54 ]] || fail 'poll reconciliation retained a non-running scope cache'
[[ ! -e $state_root/conflict/issue-70 ]] || fail 'poll reconciliation retained a resolved conflict cache'

# A stale scope cache entry with no live local worker must never block claiming.
# Regression: after recover_expired requeued Issues, REST reflection lag made
# rebuild_scope_cache cache a phantom scope for them; since undeclared Issues all
# resolve to "unknown" (which self-conflicts), every queued Issue conflict-waited
# on the phantom and the whole queue deadlocked. The cache is local-worker state,
# so an entry with no live worker is purged instead of causing a conflict.
# (Inherits the section's queue config so it does not disturb later tests.)
printf '70 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested" "$state_root/conflict/issue-70"
mkdir -p "$state_root/scope"
printf 'unknown' > "$state_root/scope/issue-71"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^70 completed closed' "$state" || fail 'a stale scope cache entry deadlocked claiming'
[[ ! -e $state_root/conflict/issue-70 ]] || fail 'an undeclared Issue was wrongly held in conflict-wait by a phantom entry'
[[ ! -e $state_root/scope/issue-71 ]] || fail 'a stale scope cache entry was not purged'

# Same directory without a structural flag: a file nested under a declared
# directory scope is a soft overlap (the "/"-boundary prefix check still finds
# it) and runs in parallel, since a directory/prefix overlap only serializes a
# structural change.
printf '210 queued open none 2026-01-01T00:00:00Z none none %s\n211 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=docs/')" "$(scope_field 'paths=docs/operations/issue-queue.md')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^210 completed closed' "$state" || fail 'same-directory parallel: the directory-scoped Issue was not claimed'
grep -Eq '^211 completed closed' "$state" || fail 'same-directory parallel: the nested-file Issue stayed serialized (soft overlap must run in parallel)'
if grep -Fq 'agentic-loop:scope-conflict' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'soft directory overlap was wrongly serialized as a scope conflict'; fi

# One Issue declares structural=1 on the same path as another: the pair
# serializes (hard conflict) with a structural reason, and the loser claims once
# the structural change completes (no permanent starvation).
printf '230 queued open none 2026-01-01T00:00:00Z none none %s\n231 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=docs structural=1')" "$(scope_field 'paths=docs')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^230 completed closed' "$state" || fail 'structural scope: the declaring Issue was not claimed first'
grep -Eq '^231 queued open' "$state" || fail 'structural scope: an overlapping-path Issue was not serialized behind the structural change'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=230 token=structural:docs' 'structural conflict was not recorded with its reason token'
[[ -r $state_root/conflict/issue-231 ]] || fail 'structural conflict-wait state was not persisted for status/Project visibility'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^231 completed closed' "$state" || fail 'structural scope: the waiting Issue was not claimed once the structural change completed'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-resolved' 'resolved structural conflict was not recorded on the Issue'
[[ ! -e $state_root/conflict/issue-231 ]] || fail 'resolved structural conflict-wait state was not cleared'

# Unknown scope: the safe default (isolated) allows only one undeclared-scope
# Issue to run at a time, without needing to serialize the whole repository.
printf '220 queued open none 2026-01-01T00:00:00Z\n221 queued open none 2026-01-02T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^220 completed closed' "$state" || fail 'unknown scope: the first undeclared-scope Issue was not claimed'
grep -Eq '^221 queued open' "$state" || fail 'default unknown_scope=isolated did not serialize undeclared-scope Issues'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=220 token=unknown' 'unknown-scope conflict was not recorded with its reason'

# paths=* (whole repository) still serializes with everything, including a
# narrower declared scope; the loser claims once the whole-repository Issue
# completes.
printf '225 queued open none 2026-01-01T00:00:00Z none none %s\n226 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=*')" "$(scope_field 'paths=bin/agentic-loop')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^225 completed closed' "$state" || fail 'paths=*: the whole-repository Issue was not claimed'
grep -Eq '^226 queued open' "$state" || fail 'paths=*: a narrower-scope Issue was not serialized behind the whole-repository Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=225 token=*' 'whole-repository conflict was not recorded with its reason'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^226 completed closed' "$state" || fail 'paths=*: the waiting Issue was not claimed once the whole-repository Issue completed'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-resolved' 'resolved whole-repository conflict was not recorded'

# exclusive_paths escalation: a declared scope overlapping a configured shared
# path is promoted to "*" and serializes against a narrower unrelated-scope
# Issue.
printf 'exclusive_paths = "docs/shared.md"\n' >> "$target/.agentic-loop.toml"
printf '227 queued open none 2026-01-01T00:00:00Z none none %s\n228 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=docs/shared.md')" "$(scope_field 'paths=bin/agentic-loop')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^227 completed closed' "$state" || fail 'exclusive_paths: the shared-path Issue was not claimed'
grep -Eq '^228 queued open' "$state" || fail 'exclusive_paths: an unrelated Issue was not serialized behind the escalated shared path'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=227 token=*' 'exclusive_paths escalation was not recorded with a * reason'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^228 completed closed' "$state" || fail 'exclusive_paths: the waiting Issue was not claimed once the escalated shared path completed'

# env: name match: an external environment cannot be rebased, so an exact
# env: match always serializes, independent of any path overlap; the loser
# claims once the environment-holding Issue completes.
printf '229 queued open none 2026-01-01T00:00:00Z none none %s\n232 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=bin env=github-project')" "$(scope_field 'paths=lib env=github-project')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^229 completed closed' "$state" || fail 'env: the environment-holding Issue was not claimed'
grep -Eq '^232 queued open' "$state" || fail 'env: an Issue sharing an exact env name was not serialized behind the environment-holding Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=229 token=env:github-project' 'exact env: conflict was not recorded with its reason token'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^232 completed closed' "$state" || fail 'env: the waiting Issue was not claimed once the environment-holding Issue completed'

# unknown_scope=open disables conflict avoidance for undeclared-scope Issues.
{
  printf '[queue]\npoll_seconds = 1\nmax_workers = 2\nlease_seconds = 3\nstop_timeout = 10\nstale_days = 30\n'
  printf 'unknown_scope = "open"\n'
} > "$target/.agentic-loop.toml"
printf '222 queued open none 2026-01-01T00:00:00Z\n223 queued open none 2026-01-02T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
[[ $(awk '$2 == "completed" {count++} END {print count+0}' "$state") -eq 2 ]] || fail 'unknown_scope=open did not let undeclared-scope Issues run in parallel'
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=3 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# Running-scope re-evaluation: a queued Issue is blocked while a currently
# running Issue's declared scope structurally conflicts with it. rebuild_
# scope_cache re-derives every running Issue's effective scope from GitHub at
# each Supervisor startup, so a later change to the running Issue's declared
# scope (exactly what a real worker drives by posting a refined
# agentic-loop:scope marker while it runs) is picked up automatically on the
# next poll cycle.
printf '999 running open none 2026-01-01T00:00:00Z none none %s\n241 queued open none 2026-01-01T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=docs structural=1')" "$(scope_field 'paths=docs/operations')" > "$state"
printf '999 <!-- agentic-loop:lease worker=scope-running-fixture heartbeat=%s expires=%s -->\n' "$(date +%s)" "$(($(date +%s) + 3600))" > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^241 queued open' "$state" || fail "queued Issue was claimed despite a structural conflict with a running Issue's declared scope"
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=999 token=structural:docs' "structural conflict against a running Issue's declared scope was not recorded with its reason"
printf '999 running open none 2026-01-01T00:00:00Z none none %s\n241 queued open none 2026-01-01T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=bin')" "$(scope_field 'paths=docs/operations')" > "$state"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^241 completed closed' "$state" || fail "queued Issue was not reconsidered after the running Issue's declared scope changed"
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-resolved' 'resolved running-scope conflict was not recorded'

# The worker itself refines a running Issue's scope from its plan-stage
# declaration, recording a single audit comment and clearing the cache once
# the Issue leaves the running state.
printf '250 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT=$'<!-- agentic-loop:scope paths=docs/operations -->\nAGENTIC_LOOP_RESULT=completed' "$target/bin/agentic-loop" _worker 250 scope-refine-worker
grep -Eq '^250 completed closed' "$state" || fail 'plan-stage scope declaration test Issue did not complete'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope tokens=path:docs/operations' 'plan-stage scope declaration was not recorded on the Issue'
[[ ! -e $state_root/scope/issue-250 ]] || fail 'completed worker left a stale scope cache entry'

# A rename measured from the worker's exec diff promotes the scope to
# structural: the cache gains the `structural` sentinel plus the affected path,
# so a subsequent Issue touching the renamed file is serialized behind it.
printf '251 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_GIT_RENAME=1 FAKE_CODEX_RESULT='AGENTIC_LOOP_RESULT=completed' "$target/bin/agentic-loop" _worker 251 rename-scope-worker
grep -Eq '^251 completed closed' "$state" || fail 'rename-diff scope test Issue did not complete'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope tokens=path:seed-renamed.txt,structural' 'the worker did not record the renamed path and structural sentinel in its refined scope'

# decomposition_materialize registers each freshly created child Issue as a
# native sub-issue of the parent and a native blocked_by dependency (parent
# blocked by child, and a later child blocked by an earlier one it depends
# on). Regression guard for Issue #252: sub_issue_id/issue_id must be typed
# integers (`-F`, not `-f`) carrying each child's *database id*
# (id=number+900000 in this fixture), not its Issue number.
decomposition_manifest=$(printf '{"schema":1,"mode":"children","integration_acceptance_criteria":"integration ok","children":[{"key":"child-a","title":"Child A","purpose":"purpose a","acceptance_criteria":"crit a","scope":"paths=docs/child-a","depends_on":[]},{"key":"child-b","title":"Child B","purpose":"purpose b","acceptance_criteria":"crit b","scope":"paths=docs/child-b","depends_on":["child-a"]}]}')
printf '700 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
FAKE_CODEX_RESULT=$'計画: 2子への分解\n```agentic-loop:decomposition\n'"$decomposition_manifest"$'\n```' \
  "$target/bin/agentic-loop" _worker 700 decomposition-worker
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/decomposition-calls.log"
grep -Eq '^700 blocked open' "$state" || fail 'decomposition_materialize did not move the parent Issue to agent:blocked'
child_a=$(awk '$1 > 700 {print $1; exit}' "$state")
child_b=$(awk -v a="$child_a" '$1 > a {print $1; exit}' "$state")
[[ $child_a =~ ^[0-9]+$ && $child_b =~ ^[0-9]+$ ]] || fail 'decomposition_materialize did not create two child Issues'
assert_contains "$TEST_ROOT/decomposition-calls.log" "issues/700/sub_issues --method POST -F sub_issue_id=$((child_a + 900000))" 'decomposition_materialize did not register child A as a native sub-issue with a typed database id'
assert_contains "$TEST_ROOT/decomposition-calls.log" "issues/700/sub_issues --method POST -F sub_issue_id=$((child_b + 900000))" 'decomposition_materialize did not register child B as a native sub-issue with a typed database id'
assert_contains "$TEST_ROOT/decomposition-calls.log" "issues/700/dependencies/blocked_by --method POST -F issue_id=$((child_a + 900000))" 'decomposition_materialize did not register the parent as blocked_by child A with a typed database id'
assert_contains "$TEST_ROOT/decomposition-calls.log" "issues/700/dependencies/blocked_by --method POST -F issue_id=$((child_b + 900000))" 'decomposition_materialize did not register the parent as blocked_by child B with a typed database id'
assert_contains "$TEST_ROOT/decomposition-calls.log" "issues/$child_b/dependencies/blocked_by --method POST -F issue_id=$((child_a + 900000))" 'decomposition_materialize did not register child B as blocked_by child A (depends_on) with a typed database id'
grep -q -- '-f sub_issue_id=\|-f issue_id=' "$TEST_ROOT/decomposition-calls.log" && fail 'decomposition_materialize still sends sub_issue_id/issue_id as an untyped -f string parameter'
grep -Eq -- "issue_id=($child_a|$child_b)([^0-9]|\$)" "$TEST_ROOT/decomposition-calls.log" && fail 'decomposition_materialize sent a child Issue number instead of its database id'

# Invalid dependency graphs must fail before native sub-issue/dependency calls.
for invalid_decomposition_manifest in \
  '{"schema":1,"mode":"children","integration_acceptance_criteria":"integration ok","children":[{"key":"child-a","title":"Child A","purpose":"purpose a","acceptance_criteria":"crit a","scope":"paths=docs/child-a","depends_on":["child-b"]},{"key":"child-b","title":"Child B","purpose":"purpose b","acceptance_criteria":"crit b","scope":"paths=docs/child-b","depends_on":["child-a"]}]}' \
  '{"schema":1,"mode":"children","integration_acceptance_criteria":"integration ok","children":[{"key":"child-a","title":"Child A","purpose":"purpose a","acceptance_criteria":"crit a","scope":"paths=docs/child-a","depends_on":["child-a"]},{"key":"child-b","title":"Child B","purpose":"purpose b","acceptance_criteria":"crit b","scope":"paths=docs/child-b","depends_on":[]}]}'; do
  printf '701 running open\n' > "$state"
  : > "$FAKE_GH_ROOT/$state_key.comments"
  calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
  if FAKE_CODEX_RESULT=$'計画: 無効な分解\n```agentic-loop:decomposition\n'"$invalid_decomposition_manifest"$'\n```' \
    "$target/bin/agentic-loop" _worker 701 decomposition-invalid-worker; then
    fail 'invalid decomposition manifest did not fail the worker'
  fi
  grep -Eq '^701 failed open' "$state" || fail 'invalid decomposition manifest was accepted'
  tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/decomposition-invalid-calls.log"
  grep -q '/sub_issues\|/dependencies/blocked_by' "$TEST_ROOT/decomposition-invalid-calls.log" && fail 'invalid decomposition manifest reached native dependency materialization'
done

# decomposition_validate is a rejecting validator: every malformed dependency
# or scope must stop before GitHub child-Issue mutations.  Keep these cases
# observable through the worker boundary rather than testing an implementation
# detail by sourcing the module.
decomposition_case_index=0
for decomposition_case in cycle self-dependency forward-reference too-few too-many invalid-scope duplicate-key; do
  decomposition_case_index=$((decomposition_case_index + 1))
  decomposition_issue=$((780 + decomposition_case_index))
  case $decomposition_case in
    cycle) children='[{"key":"a","title":"A","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/a","depends_on":["b"]},{"key":"b","title":"B","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/b","depends_on":["a"]}]' ;;
    self-dependency) children='[{"key":"a","title":"A","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/a","depends_on":["a"]},{"key":"b","title":"B","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/b","depends_on":[]}]' ;;
    forward-reference) children='[{"key":"a","title":"A","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/a","depends_on":["b"]},{"key":"b","title":"B","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/b","depends_on":[]}]' ;;
    too-few) children='[{"key":"a","title":"A","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/a","depends_on":[]}]' ;;
    too-many) children='[{"key":"a","title":"A","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/a","depends_on":[]},{"key":"b","title":"B","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/b","depends_on":[]},{"key":"c","title":"C","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/c","depends_on":[]},{"key":"d","title":"D","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/d","depends_on":[]},{"key":"e","title":"E","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/e","depends_on":[]},{"key":"f","title":"F","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/f","depends_on":[]},{"key":"g","title":"G","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/g","depends_on":[]}]' ;;
    invalid-scope) children='[{"key":"a","title":"A","purpose":"p","acceptance_criteria":"c","scope":"paths=bad$path","depends_on":[]},{"key":"b","title":"B","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/b","depends_on":[]}]' ;;
    duplicate-key) children='[{"key":"a","title":"A","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/a","depends_on":[]},{"key":"a","title":"A2","purpose":"p","acceptance_criteria":"c","scope":"paths=docs/a2","depends_on":[]}]' ;;
  esac
  printf '%s running open\n' "$decomposition_issue" > "$state"
  : > "$FAKE_GH_ROOT/$state_key.comments"
  bad_manifest=$(printf '{"schema":1,"mode":"children","integration_acceptance_criteria":"integration","children":%s}' "$children")
  FAKE_CODEX_RESULT=$'```agentic-loop:decomposition\n'"$bad_manifest"$'\n```' "$target/bin/agentic-loop" _worker "$decomposition_issue" "decomposition-$decomposition_case" >/dev/null 2>&1 || true
  grep -Eq "^$decomposition_issue needs-input open" "$state" || fail "decomposition $decomposition_case was not rejected"
  [[ ! -s $FAKE_GH_ROOT/issues-created ]] || fail "decomposition $decomposition_case created a child before rejection"
done

# Claude transport failures must remain provider-stage failures. In particular,
# empty non-zero output is not quota evidence and must not create a pool marker.
for claude_stage_mode in empty-nonzero stderr-nonzero nonjson-zero; do
  printf '780 running open\n' > "$state"
  rm -f "$state_root/pools/claude/exhausted" "$state_root/agent-exhausted"
  rm -f "$FAKE_GH_ROOT/claude-stage-mode"
  : > "$FAKE_GH_ROOT/$state_key.comments"
  AGENT_PROVIDER=claude AGENT_PLAN_MAX_RETRIES=0 FAKE_CLAUDE_STAGE_MODE="$claude_stage_mode" \
    AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _worker 780 "claude-stage-$claude_stage_mode" >/dev/null 2>&1 || true
  [[ ! -e "$state_root/pools/claude/exhausted" ]] || fail "Claude $claude_stage_mode was misclassified as pool exhaustion"
  grep -Eq '^780 failed' "$state" || fail "Claude $claude_stage_mode did not produce a terminal stage failure"
done

# doctor rejects an invalid unknown_scope value.
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.valid"
printf '[queue]\nunknown_scope = "sometimes"\n' > "$target/.agentic-loop.toml"
if "$target/bin/agentic-loop" doctor > /tmp/agentic-loop-doctor-scope.$$; then fail 'doctor accepted an invalid unknown_scope value'; fi
grep -Fq '[失敗] 設定値: UNKNOWN_SCOPE' /tmp/agentic-loop-doctor-scope.$$ || fail 'doctor did not classify the invalid unknown_scope value'
mv "$target/.agentic-loop.toml.valid" "$target/.agentic-loop.toml"
rm -f /tmp/agentic-loop-doctor-scope.$$

# status surfaces each running Issue's effective scope (including a structural
# sentinel) and any hard conflict wait with its reason. The Supervisor is
# deliberately left stopped: a live start would call rebuild_scope_cache and
# overwrite this manually seeded fixture.
printf '260 running open\n261 queued open\n262 queued open\n' > "$state"
mkdir -p "$state_root/scope" "$state_root/conflict"
printf 'path:bin/agentic-loop\nstructural' > "$state_root/scope/issue-260"
printf '260\tbin/agentic-loop\n' > "$state_root/conflict/issue-261"
printf '260\tstructural:bin/agentic-loop\n' > "$state_root/conflict/issue-262"
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'scope: path:bin/agentic-loop,structural' <<< "$status_output" || fail 'status did not show the running Issue effective scope including structural'
grep -Fq '競合待ちIssue:' <<< "$status_output" || fail 'status did not show a conflict-wait section'
grep -Fq '#261' <<< "$status_output" || fail 'status did not name the waiting Issue'
grep -Fq 'bin/agentic-loop' <<< "$status_output" || fail 'status did not name the overlapping token'
grep -Fq 'structural:bin/agentic-loop' <<< "$status_output" || fail 'status did not show the structural serialization reason'
status_json=$("$target/bin/agentic-loop" status --format json)
grep -Fq '"issue":262,"other":260,"reason":"structural:bin/agentic-loop"' <<< "$status_json" || fail 'status JSON did not report the structural serialization reason'
rm -f "$state_root/scope/issue-260" "$state_root/conflict/issue-261" "$state_root/conflict/issue-262"

# --- Issue dependency gating (Issue #41) ---

# GitHub Web UI/API may return CRLF bodies. Normalize it before parsing so a
# same-repository dependency is not classified as cross-repository.
crlf_dependency_body=$(printf 'Blocked by: #300\r\n' | base64 -w0)
printf '300 completed closed\n305 queued open none 2026-01-01T00:00:00Z none none %s\n' "$crlf_dependency_body" > "$state"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^305 completed closed' "$state" || fail 'CRLF dependency body was not normalized'

write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=3 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
rm -f "$FAKE_GH_ROOT/$state_key.dep-links"

# A dependency verified complete (agent:completed, closed) lets the Issue be
# claimed alongside it in the same pass; a dependency merely closed without
# agent:completed (here, agent:failed) does not count as complete and blocks
# claiming with a reason code instead of silently starving the Issue.
printf '300 completed closed\n301 failed closed\n310 queued open none 2026-01-01T00:00:00Z none none %s\n311 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(dependency_field '#300')" "$(dependency_field '#301')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^310 completed closed' "$state" || fail 'dependency satisfied by agent:completed did not let the Issue be claimed'
grep -Eq '^311 blocked open' "$state" || fail 'a dependency closed without agent:completed was accepted as satisfied'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dependency-blocked reason=incomplete' 'incomplete-dependency block was not recorded with its reason'
[[ -r $state_root/dependency/blocked-311 ]] || fail 'dependency block state was not persisted for status/Project visibility'
assert_contains "$FAKE_GH_ROOT/calls" '--single-select-option-id PVTFO_blocked' 'blocked state was not synchronized to the Project Agent status field'
assert_contains "$FAKE_GH_ROOT/calls" '--field-id PVTF_blocked_by' 'blocked reason was not written to the Project Blocked by field'

# Once the blocking dependency itself completes, the blocked Issue is
# automatically requeued and claimed in the same poll, with no manual Label edit.
printf '300 completed closed\n301 completed closed\n310 completed closed none 2026-01-01T00:00:00Z none none %s\n311 blocked open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(dependency_field '#300')" "$(dependency_field '#301')" > "$state"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^311 completed closed' "$state" || fail 'a resolved dependency did not auto-requeue and claim the blocked Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dependency-ready' 'dependency resolution was not recorded on the Issue'
[[ ! -e $state_root/dependency/blocked-311 ]] || fail 'resolved dependency block state was not cleared'

# A dependency referencing a nonexistent Issue, another repository, or an
# invalid `Blocked by:` token (a bad reference, or more than one such line)
# each block with a distinct, correct reason code.
multi_line_body=$(printf 'Blocked by: #1\nBlocked by: #2' | base64 -w0)
printf '370 queued open none 2026-01-01T00:00:00Z none none %s\n371 queued open none 2026-01-02T00:00:00Z none none %s\n372 queued open none 2026-01-03T00:00:00Z none none %s\n373 queued open none 2026-01-04T00:00:00Z none none %s\n' \
  "$(dependency_field '#9999')" "$(dependency_field 'other/repo#5')" "$(dependency_field 'abc')" "$multi_line_body" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^370 blocked open' "$state" || fail 'a dependency referencing a nonexistent Issue was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dependency-blocked reason=missing' 'missing-dependency reason was not recorded'
grep -Eq '^371 blocked open' "$state" || fail 'a cross-repository dependency reference was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dependency-blocked reason=cross-repo' 'cross-repository reason was not recorded'
grep -Eq '^372 blocked open' "$state" || fail 'an invalid Blocked by: token was not blocked'
grep -Eq '^373 blocked open' "$state" || fail 'more than one Blocked by: line was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dependency-blocked reason=syntax' 'invalid-syntax reason was not recorded'

# A circular dependency (A blocked by B, B blocked by A) is reported with its
# own reason code rather than the generic "incomplete", since it can never
# resolve automatically and needs a human to break the cycle.
printf '330 queued open none 2026-01-01T00:00:00Z none none %s\n331 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(dependency_field '#331')" "$(dependency_field '#330')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^330 blocked open' "$state" || fail 'circularly dependent Issue 330 was not blocked'
grep -Eq '^331 blocked open' "$state" || fail 'circularly dependent Issue 331 was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dependency-blocked reason=cycle' 'circular-dependency reason was not recorded'

# Native GitHub issue dependencies and the body's `Blocked by:` line are
# unioned: an Issue blocks if EITHER source names an unmet dependency, so
# both are consulted even when only one of them is declared.
printf '350 completed closed\n351 queued open\n352 queued open\n360 queued open none 2026-01-01T00:00:00Z none none %s\n361 queued open none 2026-01-02T00:00:00Z\n' \
  "$(dependency_field '#351')" > "$state"
printf '360 350\n361 352\n' > "$FAKE_GH_ROOT/$state_key.dep-links"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^360 blocked open' "$state" || fail 'a body-declared dependency was not consulted alongside a satisfied native dependency'
grep -Eq '^361 blocked open' "$state" || fail 'a native-declared dependency was not consulted when no body dependency exists'

# When the native dependency endpoint itself is unavailable (404, e.g. an
# older GitHub Enterprise or a repository without the feature enabled), the
# body syntax alone still gates claiming correctly, both for a satisfied and
# an unsatisfied dependency.
rm -f "$FAKE_GH_ROOT/$state_key.dep-links"
printf '353 completed closed\n354 queued open\n362 queued open none 2026-01-01T00:00:00Z none none %s\n363 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(dependency_field '#353')" "$(dependency_field '#354')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_DEPENDENCIES_UNAVAILABLE=1 AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^362 completed closed' "$state" || fail 'native-unavailable fallback did not let a satisfied body dependency claim'
grep -Eq '^363 blocked open' "$state" || fail 'native-unavailable fallback did not block on an unsatisfied body dependency'

# A dependency-check API failure withholds claiming from the very first poll,
# but only moves the Issue to agent:blocked once it persists across
# DEPENDENCY_FAILURE_TOLERANCE consecutive polls, so a single blip does not
# flap the Label; the Issue recovers automatically once the API does.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=3 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 API_RETRY_ATTEMPTS=1
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
printf '380 queued open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_DEPENDENCIES_TRANSIENT_FAIL=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^380 queued open' "$state" || fail 'the first dependency-check API failure moved the Issue to agent:blocked too eagerly'
FAKE_DEPENDENCIES_TRANSIENT_FAIL=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^380 queued open' "$state" || fail 'a second consecutive dependency-check API failure moved the Issue to agent:blocked too eagerly'
FAKE_DEPENDENCIES_TRANSIENT_FAIL=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^380 blocked open' "$state" || fail 'a persistent dependency-check API failure did not move the Issue to agent:blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dependency-blocked reason=api' 'persistent API-failure reason was not recorded'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^380 completed closed' "$state" || fail 'the Issue did not auto-recover once the dependency-check API stopped failing'
[[ ! -e $state_root/dependency/failures-380 ]] || fail 'the dependency-check failure counter was not cleared on recovery'

# A permission error against the dependency API (401/403) is a distinct,
# tolerated-then-blocked reason from a generic API failure.
printf '381 queued open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_DEPENDENCIES_FORBIDDEN=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
FAKE_DEPENDENCIES_FORBIDDEN=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
FAKE_DEPENDENCIES_FORBIDDEN=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^381 blocked open' "$state" || fail 'a persistent dependency-API permission error did not move the Issue to agent:blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dependency-blocked reason=permission' 'persistent permission-failure reason was not recorded'

# The dependency gate runs before change-scope conflict avoidance: an Issue
# that is both dependency-incomplete and scope-conflicted with a running
# Issue is reported only as dependency-blocked, never as scope-conflicted.
combined_body=$(printf '<!-- agentic-loop:scope paths=bin/agentic-loop -->\nBlocked by: #401' | base64 -w0)
printf '401 queued open\n402 running open none 2026-01-01T00:00:00Z none none %s\n400 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=bin/agentic-loop')" "$combined_body" > "$state"
printf '402 <!-- agentic-loop:lease worker=dep-order-fixture heartbeat=%s expires=%s -->\n' "$(date +%s)" "$(($(date +%s) + 3600))" > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^400 blocked open' "$state" || fail 'a dependency-incomplete, scope-conflicted Issue was not blocked by the dependency gate'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:dependency-blocked' 'the dependency gate did not record its block'
if grep -Fq 'agentic-loop:scope-conflict' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'the dependency gate did not run before the scope-conflict check'; fi

# status surfaces dependency-blocked Issues with their reason code.
rm -rf "$state_root/dependency"
mkdir -p "$state_root/dependency"
printf 'incomplete\t依存Issue #501 が未完了です。\n' > "$state_root/dependency/blocked-500"
status_output=$("$target/bin/agentic-loop" status)
grep -Fq '依存待ちIssue:' <<< "$status_output" || fail 'status did not show a dependency-wait section'
grep -Fq '#500' <<< "$status_output" || fail 'status did not name the dependency-blocked Issue'
grep -Fq 'incomplete' <<< "$status_output" || fail 'status did not show the dependency-block reason code'
rm -f "$state_root/dependency/blocked-500"
rm -f "$FAKE_GH_ROOT/$state_key.dep-links"

# --- gh --body "@path" silent-loss detection and pre-claim diversion (Issue #272, ADR 0030) ---

# claim_next diverts a queued Issue whose whole body is nothing but an
# unexpanded `gh --body "@path"` reference to agent:needs-input before ever
# handing it to a worker, so no exec is spent on a lost requirement. A
# sibling Issue with an ordinary body claims normally in the same poll.
unexpanded_body_b64=$(printf '@/tmp/issue_body.md' | base64 -w0)
ordinary_body_b64=$(printf 'これは正当な要求本文です。' | base64 -w0)
printf '910 queued open none 2026-01-01T00:00:00Z none none %s\n911 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$unexpanded_body_b64" "$ordinary_body_b64" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^910 needs-input open' "$state" || fail 'a lost-requirement Issue was not diverted to agent:needs-input before claim'
grep -Eq '^911 completed closed' "$state" || fail 'a sibling Issue with an ordinary body was not claimed in the same poll'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:body-unexpanded' 'the diversion did not leave an explanatory comment'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '--body-file' 'the diversion comment did not point at --body-file recovery'
if grep -q ' 910 ' "$FAKE_GH_ROOT/codex-calls"; then fail 'a worker was started for a lost-requirement Issue'; fi

# The diversion is idempotent: a second poll does not repost the comment or
# touch the Label again (queued->needs-input already moved it out of the
# queued bucket, so claim_next never re-evaluates it, but this also guards
# any future direct re-check of a still-needs-input Issue).
comments_before=$(wc -l < "$FAKE_GH_ROOT/$state_key.comments")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
comments_after=$(wc -l < "$FAKE_GH_ROOT/$state_key.comments")
[[ $comments_before == "$comments_after" ]] || fail 'a second poll reposted the body-unexpanded comment'
grep -Eq '^910 needs-input open' "$state" || fail 'a second poll changed the diverted Issue state'

# --- Closed-loop postmortem learning (Issue #132, ADR 0026, docs/policies/postmortem.md) ---

# auto_detect defaults to off (code-level fallback, see config.sh) until an
# Issue's own [postmortem] section turns it on: a parked (repeated-failure)
# Issue creates no candidate.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=1 RETRY_COOLDOWN_SECONDS=0
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
rm -rf "$state_root/postmortem"
printf '900 failed open none 2026-01-01T00:00:00Z\n' > "$state"
mkdir -p "$state_root/attempts"; printf '1\t0\n' > "$state_root/attempts/issue-900"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^900 parked open' "$state" || fail 'postmortem off-by-default fixture setup did not park Issue 900'
[[ $(awk '$2 == "queued"' "$state" | wc -l) -eq 0 ]] || fail 'postmortem auto_detect=off (code default) unexpectedly created a candidate Issue'

# auto_detect=on: a parked (repeated-failure) Issue auto-creates a dedup-
# checked postmortem candidate, labeled category:loop-continuity + agent:queued,
# carrying a fingerprint marker and its trigger kind.
printf '\n[postmortem]\nauto_detect = "on"\nmax_auto_created_per_day = 5\n' >> "$target/.agentic-loop.toml"
printf '901 failed open none 2026-01-01T00:00:00Z\n' > "$state"
mkdir -p "$state_root/attempts"; printf '1\t0\n' > "$state_root/attempts/issue-901"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^901 parked open' "$state" || fail 'Issue 901 was not parked'
# Unlike the resource-exhaustion fixture above, the provider pool here stays
# healthy, so this same single-worker run-once pass also claims and (via the
# default fake provider) completes the newly created candidate before this
# assertion runs. Match on Issue number, not on it still being "queued".
pm_issue=$(awk '$1 != 901 {print $1}' "$state" | tail -n1)
[[ $pm_issue =~ ^[0-9]+$ ]] || fail 'repeated-failure park did not auto-create a postmortem candidate Issue'
[[ $(awk -v n="$pm_issue" '$1 == n {print $7}' "$state") == loop-continuity ]] || fail 'auto-created postmortem Issue is missing category:loop-continuity'
pm_body=$(awk -v n="$pm_issue" '$1 == n {print $8}' "$state" | base64 -d)
grep -Fq 'agentic-loop:postmortem fingerprint=' <<< "$pm_body" || fail 'auto-created postmortem Issue is missing its fingerprint marker'
grep -Fq 'kind=repeated-failure' <<< "$pm_body" || fail 'auto-created postmortem Issue does not record its trigger kind'
rm -rf "$state_root/postmortem"

# Non-activation: even with auto_detect=on, a single failure that still has
# retry budget left is requeued (not parked) and creates no candidate --
# "not every single failure demands a heavyweight postmortem".
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=2 RETRY_COOLDOWN_SECONDS=0
printf 'unknown_scope = "open"\n\n[postmortem]\nauto_detect = "on"\nmax_auto_created_per_day = 5\n' >> "$target/.agentic-loop.toml"
printf '907 failed open none 2026-01-01T00:00:00Z\n' > "$state"
mkdir -p "$state_root/attempts"; printf '1\t0\n' > "$state_root/attempts/issue-907"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
# The healthy single-worker pool claims and (via the default fake provider)
# completes the requeued Issue within this same run-once pass -- what matters
# here is that retry budget remaining meant it was requeued rather than
# parked, and that no candidate Issue was created alongside it.
grep -Eq '^907 parked' "$state" && fail 'a single failure with retry budget remaining was unexpectedly parked'
[[ $(awk '$1 != 907' "$state" | wc -l) -eq 0 ]] || fail 'a single failure with retry budget remaining (auto_detect=on) unexpectedly created a postmortem candidate'
rm -rf "$state_root/postmortem" "$state_root/attempts"

# Explicit request (bin/agentic-loop postmortem create) works independent of
# auto_detect, dedups an in-flight (open) event by kind+subject fingerprint
# (a second explicit request for the same event reuses the same Issue instead
# of creating a duplicate), and never writes secret-like evidence into the
# Issue body (fail-closed: evidence is omitted, not partially redacted).
pm1=$("$target/bin/agentic-loop" postmortem create --kind manual --subject dedup-test-event --title 'テスト事象A' --summary '概要')
[[ $pm1 =~ ^[0-9]+$ ]] || fail 'explicit postmortem create did not print a new Issue number'
assert_contains "$FAKE_GH_ROOT/calls" 'title=ポストモーテム: テスト事象A' 'created postmortem Issue title is missing the ポストモーテム: prefix (queue-list readability)'
pm2=$("$target/bin/agentic-loop" postmortem create --kind manual --subject dedup-test-event --title 'テスト事象A(再)' --summary '概要2')
[[ $pm1 == "$pm2" ]] || fail 'a second explicit request for the same kind+subject did not dedup to the existing open Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:postmortem-recurrence' 'dedup reuse was not recorded as a recurrence comment'

# near-miss: an explicit --kind near-miss request is filed under
# category:loop-continuity (no confidentiality/integrity/availability harm
# actually occurred, only a near miss).
pm_nearmiss=$("$target/bin/agentic-loop" postmortem create --kind near-miss --subject near-miss-test-event --title 'near miss事象')
[[ $pm_nearmiss =~ ^[0-9]+$ ]] || fail 'explicit near-miss postmortem create did not print a new Issue number'
[[ $(awk -v n="$pm_nearmiss" '$1 == n {print $7}' "$state") == loop-continuity ]] || fail 'near-miss postmortem Issue is missing category:loop-continuity'
nearmiss_body=$(awk -v n="$pm_nearmiss" '$1 == n {print $8}' "$state" | base64 -d)
grep -Fq 'kind=near-miss' <<< "$nearmiss_body" || fail 'near-miss postmortem Issue does not record its trigger kind'

# Built by concatenation (never a contiguous literal in this source file, see
# the flaky_secret_msg note above): this file is scanned whole by the
# repository's own secret guard.
postmortem_secret_literal=$(printf '%s%s' 'AKIA' 'ABCDEFGHIJKLMNOP')
secret_file="$TEST_ROOT/postmortem-secret-evidence.txt"
printf '%s\n' "$postmortem_secret_literal" > "$secret_file"
pm3=$("$target/bin/agentic-loop" postmortem create --kind manual --subject secret-test-event --title 'シークレットテスト' --evidence-file "$secret_file")
[[ $pm3 =~ ^[0-9]+$ ]] || fail 'postmortem create with secret-like evidence did not still create the Issue'
pm3_body=$(awk -v n="$pm3" '$1 == n {print $8}' "$state" | base64 -d)
grep -Fq "$postmortem_secret_literal" <<< "$pm3_body" && fail 'secret-like evidence was written into the postmortem Issue body'
grep -Fq '省略' <<< "$pm3_body" || fail 'postmortem body did not note evidence omission'

# max_auto_created_per_day (bounded, configurable) caps only the AUTOMATIC
# path: once reached, a further auto-detect candidate is skipped without
# stopping the supervisor poll, but an explicit request (user or worker) is
# never blocked by it -- it only counts toward the same daily counter.
rm -rf "$state_root/postmortem" "$state_root/attempts"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=1 RETRY_COOLDOWN_SECONDS=0
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
printf '\n[postmortem]\nauto_detect = "on"\nmax_auto_created_per_day = 1\n' >> "$target/.agentic-loop.toml"
printf '905 failed open none 2026-01-01T00:00:00Z\n' > "$state"
mkdir -p "$state_root/attempts"; printf '1\t0\n' > "$state_root/attempts/issue-905"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^905 parked open' "$state" || fail 'daily-cap fixture setup did not park Issue 905'
[[ $(awk '$1 != 905' "$state" | wc -l) -eq 1 ]] || fail 'the first auto-create under the daily cap did not create exactly one candidate Issue'
rm -rf "$state_root/attempts"

printf '906 failed open none 2026-01-01T00:00:00Z\n' > "$state"
mkdir -p "$state_root/attempts"; printf '1\t0\n' > "$state_root/attempts/issue-906"
: > "$FAKE_GH_ROOT/$state_key.comments"
cap_run_rc=0
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise || cap_run_rc=$?
(( cap_run_rc == 0 )) || fail 'reaching the daily auto-create cap unexpectedly stopped the supervisor poll'
grep -Eq '^906 parked open' "$state" || fail 'Issue 906 was not parked'
[[ $(awk '$1 != 906 && $2 == "queued"' "$state" | wc -l) -eq 0 ]] || fail 'auto-create beyond the daily cap unexpectedly created a second candidate Issue'

pm_explicit=$("$target/bin/agentic-loop" postmortem create --kind manual --subject cap-test-explicit --title '明示要求') || fail 'an explicit postmortem create was blocked by the daily auto-create cap'
[[ $pm_explicit =~ ^[0-9]+$ ]] || fail 'explicit postmortem create beyond the daily cap did not print a new Issue number'
rm -rf "$state_root/postmortem" "$state_root/attempts"
rm -rf "$state_root/postmortem"

# postmortem link: an action item Issue is linked as a native sub-issue +
# dependency, and the postmortem Issue moves to agent:blocked (releasing any
# lease/scope/conflict state) instead of being left agent:running.
printf '90 running open\n91 queued open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
mkdir -p "$state_root/workers"
printf '1\t%s\t%s\n' "$(($(date +%s) + 3600))" "$(date +%s)" > "$state_root/workers/90.lease"
printf '90 <!-- agentic-loop:claim worker=postmortem-link-fixture created=%s expires=%s -->\n' "$(date +%s)" "$(($(date +%s) + 3600))" > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" postmortem link 90 91
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/postmortem-link-calls.log"
grep -Eq '^90 blocked open' "$state" || fail 'postmortem link did not move the postmortem Issue to agent:blocked'
assert_contains "$TEST_ROOT/postmortem-link-calls.log" 'issues/90/sub_issues' 'postmortem link did not register the action item as a native sub-issue'
assert_contains "$TEST_ROOT/postmortem-link-calls.log" 'issues/90/dependencies/blocked_by' 'postmortem link did not register the native blocked_by dependency'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:postmortem-link action_items=91' 'postmortem link did not record its audit marker'
[[ ! -r "$state_root/workers/90.lease" ]] || fail 'postmortem link did not release the postmortem Issue lease'
# Regression guard for Issue #252: `gh api -f` always sends a string, which
# GitHub 422s for these typed-integer properties, and both endpoints require
# action item #91's *database id* (this fixture's id=901991... i.e.
# number+900000, deliberately distinct from the Issue number) rather than its
# Issue number.
assert_contains "$TEST_ROOT/postmortem-link-calls.log" 'issues/90/sub_issues --method POST -F sub_issue_id=900091' 'postmortem link did not send sub_issue_id as a typed integer using the action item database id'
assert_contains "$TEST_ROOT/postmortem-link-calls.log" 'issues/90/dependencies/blocked_by --method POST -F issue_id=900091' 'postmortem link did not send issue_id as a typed integer using the action item database id'
grep -q -- '-f sub_issue_id=\|-f issue_id=' "$TEST_ROOT/postmortem-link-calls.log" && fail 'postmortem link still sends sub_issue_id/issue_id as an untyped -f string parameter'
grep -Eq -- 'issue_id=91([^0-9]|$)' "$TEST_ROOT/postmortem-link-calls.log" && fail 'postmortem link sent the action item Issue number instead of its database id'

# When the native sub-issue/dependency POST itself fails (e.g. the 422 this
# Issue reports), the response body must reach a diagnosable place instead of
# a silent `return` (Issue #252 completion criterion 4).
printf '92 running open\n93 queued open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
printf '92 <!-- agentic-loop:claim worker=postmortem-link-fail-fixture created=%s expires=%s -->\n' "$(date +%s)" "$(($(date +%s) + 3600))" > "$FAKE_GH_ROOT/$state_key.comments"
link_fail_rc=0
FAKE_SUB_ISSUES_FAIL=1 "$target/bin/agentic-loop" postmortem link 92 93 > "$TEST_ROOT/postmortem-link-fail.log" 2>&1 || link_fail_rc=$?
(( link_fail_rc != 0 )) || fail 'postmortem link did not fail when the sub_issues POST was rejected'
grep -Eq '^92 running open' "$state" || fail 'a failed postmortem link unexpectedly changed the postmortem Issue state'
assert_contains "$TEST_ROOT/postmortem-link-fail.log" 'forced-test-failure' 'a failed sub_issues POST did not surface its response body for diagnosis'

# Closed loop: once every linked action item Issue closes and verifies
# (agent:completed), the EXISTING dependency.sh requeue mechanism (no new
# gating code) automatically returns the postmortem Issue to agent:queued --
# this is ADR 0026's central claim, that action-item tracking reuses
# dependency.sh rather than a new bespoke waiting mechanism.
blocked_body=$(printf 'Blocked by: #91' | base64 -w0)
printf '90 blocked open none 2026-01-02T00:00:00Z none none %s\n91 running open\n' "$blocked_body" > "$state"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^90 blocked open' "$state" || fail 'a postmortem Issue with an incomplete action item was requeued too early'
printf '90 blocked open none 2026-01-02T00:00:00Z none none %s\n91 completed closed\n' "$blocked_body" > "$state"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
# The healthy single-worker pool may claim and (via the default fake
# provider) complete Issue 90 within this same run-once pass once it is
# requeued; what matters here is that it left agent:blocked, not that it is
# still literally "queued" by the time this assertion runs.
grep -Eq '^90 blocked open' "$state" && fail 'a postmortem Issue was not automatically requeued once its action item completed'

# postmortem status reports each linked action item's completion state.
status_body=$(printf 'Blocked by: #91' | base64 -w0)
printf '92 queued open none 2026-01-01T00:00:00Z none none %s\n91 completed closed\n' "$status_body" > "$state"
pm_status=$("$target/bin/agentic-loop" postmortem status 92)
grep -Fq '#91: 完了・検証済み' <<< "$pm_status" || fail 'postmortem status did not report a completed, verified action item'

# postmortem complete: the machine-checked gate for "postmortem本文だけを
# closeしてaction itemを未追跡にしない". It never closes the Issue itself
# (that is worker.sh's job once the marker it writes is observed); it only
# fails closed -- non-zero exit, Japanese reason, no marker written -- when
# any of its three checks does not hold.
gate_body_incomplete=$(printf 'Blocked by: #91\n\n## 残余リスク\n\nなし。' | base64 -w0)
printf '93 queued open none 2026-01-01T00:00:00Z none none %s\n91 running open\n' "$gate_body_incomplete" > "$state"
gate_err=$("$target/bin/agentic-loop" postmortem complete 93 2>&1) && fail 'postmortem complete succeeded with an unverified action item'
grep -Fq '未完了' <<< "$gate_err" || fail 'postmortem complete did not explain the unresolved action item in Japanese'
[[ ! -e "$state_root/postmortem/turn-93" ]] || fail 'postmortem complete wrote the completion marker despite an unresolved action item'

gate_body_placeholder=$(printf 'Blocked by: #91\n\n## 残余リスク\n\n（記入してください）' | base64 -w0)
printf '93 queued open none 2026-01-01T00:00:00Z none none %s\n91 completed closed\n' "$gate_body_placeholder" > "$state"
gate_err=$("$target/bin/agentic-loop" postmortem complete 93 2>&1) && fail 'postmortem complete succeeded with an unfilled template placeholder'
grep -Fq 'プレースホルダ' <<< "$gate_err" || fail 'postmortem complete did not explain the unfilled placeholder in Japanese'

gate_body_empty_risk=$(printf 'Blocked by: #91\n\n## 残余リスク\n\n' | base64 -w0)
printf '93 queued open none 2026-01-01T00:00:00Z none none %s\n91 completed closed\n' "$gate_body_empty_risk" > "$state"
gate_err=$("$target/bin/agentic-loop" postmortem complete 93 2>&1) && fail 'postmortem complete succeeded with an empty residual-risk section'
grep -Fq '残余リスク' <<< "$gate_err" || fail 'postmortem complete did not explain the empty residual-risk section in Japanese'

gate_body_ready=$(printf 'Blocked by: #91\n\n## 残余リスク\n\n実施しない項目はない。' | base64 -w0)
printf '93 queued open none 2026-01-01T00:00:00Z none none %s\n91 completed closed\n' "$gate_body_ready" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" postmortem complete 93 || fail 'postmortem complete failed despite all gate conditions being satisfied'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:postmortem-verified' 'postmortem complete did not record its verification comment'
[[ $(cat "$state_root/postmortem/turn-93" 2>/dev/null) == complete ]] || fail 'postmortem complete did not write the complete turn marker consumed by worker.sh'

# CRLF headings must be recognized by the postmortem completion gate.
gate_body_crlf=$(printf 'Blocked by: #91\r\n\r\n## 残余リスク\r\n\r\n実施しない項目はない。\r\n' | base64 -w0)
printf '94 queued open none 2026-01-01T00:00:00Z none none %s\n91 completed closed\n' "$gate_body_crlf" > "$state"
"$target/bin/agentic-loop" postmortem complete 94 || fail 'postmortem complete did not normalize CRLF headings'
[[ $(cat "$state_root/postmortem/turn-94" 2>/dev/null) == complete ]] || fail 'CRLF postmortem did not write the complete marker'

rm -rf "$state_root/postmortem" "$state_root/attempts" "$state_root/workers/90.lease"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# Setup creates no priority:* label (numeric priority lives in the body marker
# now) and creates the state and category labels idempotently.
if grep -Fq $'label create priority:critical' "$FAKE_GH_ROOT/calls" || grep -Fq $'label create priority:high' "$FAKE_GH_ROOT/calls" || grep -Fq $'label create priority:medium' "$FAKE_GH_ROOT/calls" || grep -Fq $'label create priority:low' "$FAKE_GH_ROOT/calls"; then fail 'setup created a legacy priority label'; fi
grep -Fq $'label create agent:stale' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the stale state label'
grep -Fq $'label create agent:parked' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the parked state label'
grep -Fq $'label create agent:paused' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the paused state label'
for category in loop-continuity confidentiality-incident integrity-incident availability-incident bug feature improvement; do
  grep -Fq "label create category:$category" "$FAKE_GH_ROOT/calls" || fail "setup did not create category:$category"
done
assert_contains "$FAKE_GH_ROOT/calls" 'project field-create 7 --owner acme --name Category --data-type SINGLE_SELECT' 'setup did not create the Project Category field'

# Setup migrates legacy priority:* labels on open Issues into numeric body
# markers (the highest label wins; an existing valid marker stays authoritative),
# removes the Issue labels, and deletes the repository-level priority labels.
migration_body=$(printf 'title\nbody' | base64 -w0)
legacy_marker_body=$(printf 'title\n<!-- agentic-loop:priority 30 -->' | base64 -w0)
printf '301 queued open critical 2026-01-01T00:00:00Z none improvement %s\n302 queued open low 2026-01-02T00:00:00Z none improvement %s\n303 running open critical,low 2026-01-03T00:00:00Z none improvement\n' "$migration_body" "$legacy_marker_body" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_SKIP_START=1 "$target/bin/agentic-loop" setup >/dev/null
decoded_301=$(awk -v n=301 '$1 == n {print $8}' "$state" | base64 -d)
decoded_302=$(awk -v n=302 '$1 == n {print $8}' "$state" | base64 -d)
grep -Fq 'agentic-loop:priority 90' <<< "$decoded_301" || fail 'setup migration did not convert critical to a priority-90 body marker'
grep -Fq 'agentic-loop:priority 30' <<< "$decoded_302" || fail 'setup migration overwrote an existing valid body marker'
[[ $(awk -v n=301 '$1 == n {print $4}' "$state") == none ]] || fail 'setup migration did not remove the legacy label from Issue 301'
[[ $(awk -v n=302 '$1 == n {print $4}' "$state") == none ]] || fail 'setup migration did not remove the legacy label from Issue 302'
[[ $(awk -v n=303 '$1 == n {print $4}' "$state") == none ]] || fail 'setup migration did not remove the legacy labels from Issue 303'
if grep -Fq $'label create priority:' "$FAKE_GH_ROOT/calls"; then fail 'setup created a priority label during migration'; fi
for legacy in critical high medium low; do
  grep -Fq "label delete priority:$legacy" "$FAKE_GH_ROOT/calls" || fail "setup migration did not delete the repository priority:$legacy label"
done

# The priority CLI upserts the body marker (replacing any old one), records an
# audit comment, drops a lingering legacy label, and the value is re-readable
# through the same parser the queue uses.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
priority_cli_body=$(printf 'title\nold <!-- agentic-loop:priority 25 -->\nmore' | base64 -w0)
printf '401 queued open critical 2026-01-01T00:00:00Z none improvement %s\n' "$priority_cli_body" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" priority 401 90 >/dev/null
decoded_401=$(awk -v n=401 '$1 == n {print $8}' "$state" | base64 -d)
grep -Fq 'agentic-loop:priority 90' <<< "$decoded_401" || fail 'priority CLI did not upsert the body marker'
grep -Fq 'agentic-loop:priority 25' <<< "$decoded_401" && fail 'priority CLI left the previous body marker behind'
grep -Fq 'title' <<< "$decoded_401" || fail 'priority CLI did not preserve the rest of the body'
[[ $(awk -v n=401 '$1 == n {print $4}' "$state") == none ]] || fail 'priority CLI did not drop the lingering legacy priority label'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:priority-set schema=1 actor=test-operator issue=401 value=90' 'priority CLI did not record its audit marker'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'priority を 90（0-100）に設定' 'priority CLI did not explain the change in Japanese'
priority_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$priority_json" | yq -p json '.queue.candidates[0].priority') -eq 90 ]] || fail 'priority CLI value was not re-readable by status'

# Only inactive queued Issues are closed; the audit explains safe recovery.
# unknown_scope=open: this fixture declares no scope for any Issue and
# exercises stale triage, not scope conflict avoidance.
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
old_date=$(date -u -d '40 days ago' +%Y-%m-%dT%H:%M:%SZ)
recent_date=$(date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%SZ)
# agent:failed is deliberately excluded here: it is actively managed by retry_failed, not stale closure.
printf '20 queued open none 2025-01-01T00:00:00Z %s\n21 queued open none 2025-01-02T00:00:00Z %s\n22 running open none 2025-01-01T00:00:00Z %s\n23 needs-input open none 2025-01-01T00:00:00Z %s\n25 in-review open none 2025-01-01T00:00:00Z %s\n26 none open none 2025-01-01T00:00:00Z %s\n' "$old_date" "$recent_date" "$old_date" "$old_date" "$old_date" "$old_date" > "$state"
printf '22 <!-- agentic-loop:lease worker=active heartbeat=%s expires=%s -->\n' "$(date +%s)" "$(($(date +%s) + 3600))" > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$closes"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^20 stale closed' "$state" || fail 'inactive queued Issue was not marked stale and closed'
assert_contains "$closes" $'20\tnot_planned' 'stale close did not record state_reason=not_planned'
grep -Eq '^21 completed closed' "$state" || fail 'recent queued Issue was not left available for claim'
for issue_state in '22 running open' '23 needs-input open' '25 in-review open' '26 none open'; do
  grep -Eq "^$issue_state" "$state" || fail "excluded Issue changed state: $issue_state"
done
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:stale days=30' 'stale audit marker was not recorded'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reopen' 'stale audit did not explain reopening'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agent:queued' 'stale audit did not explain requeueing'

# STALE_DAYS=0 disables automatic closure while preserving normal claiming.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=0
printf '30 queued open none 2025-01-01T00:00:00Z %s\n31 queued open none 2025-01-02T00:00:00Z %s\n' "$old_date" "$old_date" > "$state"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
[[ $(awk '$2 == "stale" {count++} END {print count+0}' "$state") -eq 0 ]] || fail 'STALE_DAYS=0 marked an Issue stale'
[[ $(awk '$2 == "queued" {count++} END {print count+0}' "$state") -eq 1 ]] || fail 'disabled stale handling did not preserve the remaining queue'

write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# A linked-worktree worker can fetch, commit, and push through the constructed invocation boundary.
printf '6 running open\n' > "$state"
FAKE_CODEX_GIT_OPERATIONS=1 "$target/bin/agentic-loop" _worker 6 linked-worktree-worker
git -C "$target" fetch --quiet origin agent/issue-6
git -C "$target" show 'origin/agent/issue-6:worker.txt' | grep -Fxq 'worker change' || fail 'linked-worktree Git metadata operations did not reach the remote'
assert_contains "$FAKE_GH_ROOT/calls" "project item-add 7 --owner acme --url https://github.example/acme/installed-project/pull/6" 'worker PR was not added to the Project'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope tokens=path:worker.txt' 'measured exec-stage Git diff did not refine the change-scope cache'
[[ ! -e $target-worktrees/issue-6 ]] || fail 'completed linked worker worktree remained'
! git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-6 || fail 'completed linked worker local branch remained'
git -C "$target" ls-remote --exit-code --heads origin refs/heads/agent/issue-6 >/dev/null || fail 'Supervisor deleted the remote worker branch'

# A worker completion report cannot close an Issue unless GitHub confirms a merged PR for its branch.
printf '7 running open\n' > "$state"
FAKE_PR_MERGED=0 "$target/bin/agentic-loop" _worker 7 unmerged-worker
grep -Eq '^7 failed open$' "$state" || fail 'unmerged worker self-report was accepted as completed'
[[ -e $target-worktrees/issue-7 ]] || fail 'unmerged worker worktree was removed'
# shellcheck disable=SC2016 # Backticks are literal Markdown in the expected Issue comment.
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '安全なcleanupを完了できませんでした' 'unmerged completion did not record objective failure evidence'

# A merged PR whose commit does not match the dedicated branch cannot trigger destructive cleanup.
printf '8 running open\n' > "$state"
FAKE_PR_HEAD_OID=0000000000000000000000000000000000000000 "$target/bin/agentic-loop" _worker 8 unexpected-ref-worker
grep -Eq '^8 failed open$' "$state" || fail 'unexpected merged PR ref was accepted'
[[ -e $target-worktrees/issue-8 ]] || fail 'unexpected ref worker worktree was removed'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-8 || fail 'unexpected ref local branch was removed'

# A clean provider exit without a terminal marker can race with GitHub merging
# the branch. The independently observed merge must enter the normal
# completion/cleanup path after the single protocol retry.
printf '9 running open\n' > "$state"
: > "$FAKE_GH_ROOT/calls"
rm -f "$FAKE_GH_ROOT/codex-exec-count"
FAKE_CODEX_EXEC_RESULT_1='実行結果のみ' \
FAKE_CODEX_EXEC_RESULT_2='実行結果のみ' \
"$target/bin/agentic-loop" _worker 9 markerless-merged-worker
grep -Eq '^9 completed closed$' "$state" || fail 'a markerless worker with a merged PR was not completed'
[[ ! -e $target-worktrees/issue-9 ]] || fail 'a markerless merged worker worktree remained'
! git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-9 || fail 'a markerless merged worker local branch remained'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:protocol-retry' 'markerless merged worker did not record the protocol retry'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:markerless-merged' 'markerless merged worker did not record the markerless-merged observation comment'
merged_pr_queries=$(grep -c '/pulls --method GET.*state=closed.*head=.*issue-9' "$FAKE_GH_ROOT/calls" || true)
[[ $merged_pr_queries -eq 1 ]] || fail "markerless merged worker queried the branch's merged PR $merged_pr_queries times, expected exactly 1"

# The same markerless clean exit without any independently observed merge
# (FAKE_PR_MERGED=0) must keep following the ordinary failure path: no false
# completion, and the worktree/branch preserved for a retried claim.
printf '23 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$FAKE_GH_ROOT/codex-exec-count"
FAKE_PR_MERGED=0 FAKE_CODEX_EXEC_RESULT_1='実行結果のみ' \
FAKE_CODEX_EXEC_RESULT_2='実行結果のみ' \
"$target/bin/agentic-loop" _worker 23 markerless-unmerged-worker
grep -Eq '^23 failed open$' "$state" || fail 'a markerless worker without a merged PR was not left failed'
[[ -e $target-worktrees/issue-23 ]] || fail 'a markerless unmerged worker worktree was removed'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-23 || fail 'a markerless unmerged worker local branch was removed'
if grep -Fq 'agentic-loop:markerless-merged' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'a markerless worker without a merged PR recorded a markerless-merged observation'; fi

# Closed-loop postmortem worker.sh terminal branch (Issue #132, ADR 0026): a
# `postmortem link` turn, executed from inside the exec turn's own sandboxed
# shell (exactly as a real provider would run it), must leave the postmortem
# Issue at agent:blocked -- worker.sh must NOT re-evaluate this exec turn's
# own AGENTIC_LOOP_RESULT=completed marker against the ordinary merged-PR
# completion path (there is no PR for a link turn) and overwrite the
# already-made agent:blocked with agent:failed.
printf '150 running open\n151 queued open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_POSTMORTEM_LINK=1 FAKE_CODEX_POSTMORTEM_LINK_ARGS='150 151' "$target/bin/agentic-loop" _worker 150 postmortem-link-worker
grep -Eq '^150 blocked open' "$state" || fail 'a postmortem link turn was not left at agent:blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:postmortem-link' 'a postmortem link turn did not record its link comment'
if grep -Fq 'agentic-loop:failed' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'a postmortem link turn was overwritten by the ordinary merged-PR-required completion path'; fi
[[ -e $target-worktrees/issue-150 ]] || fail 'a postmortem link turn unexpectedly removed its worktree'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-150 || fail 'a postmortem link turn unexpectedly removed its local branch'
[[ ! -e "$state_root/postmortem/turn-150" ]] || fail 'the link turn marker was not consumed by worker.sh'

# Closed-loop postmortem worker.sh terminal branch, completion half: once
# `postmortem complete`'s gate has verified every action item and the body,
# it writes the `complete` marker instead of closing the Issue itself.
# worker.sh's terminal branch, observing that this turn made no commit (the
# branch never advanced past the fetched default branch tip), completes and
# closes the Issue and removes the worktree/branch WITHOUT requiring a merged
# PR -- there is none for this turn, unlike the ordinary completion path.
gate_ready_body=$(printf 'Blocked by: #161\n\n## 残余リスク\n\n実施しない項目はない。' | base64 -w0)
printf '160 running open none 2026-01-01T00:00:00Z none none %s\n161 completed closed\n' "$gate_ready_body" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_POSTMORTEM_COMPLETE=1 FAKE_CODEX_POSTMORTEM_COMPLETE_ISSUE=160 "$target/bin/agentic-loop" _worker 160 postmortem-complete-worker
grep -Eq '^160 completed closed' "$state" || fail 'a postmortem complete turn (no leftover commit) was not closed as completed'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:completed postmortem=1' 'a postmortem complete turn did not record its completion comment'
[[ ! -e $target-worktrees/issue-160 ]] || fail 'a postmortem complete turn did not remove its worktree'
! git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-160 || fail 'a postmortem complete turn did not remove its local branch'
[[ ! -e "$state_root/postmortem/turn-160" ]] || fail 'the complete turn marker was not consumed by worker.sh'

# If the branch DID advance during this turn (a real commit happened, not
# just GitHub API calls), the complete-marker fast path's oid-match safety
# check fails closed and this falls through to the ordinary merged-PR-
# required completion path instead of silently completing/closing unmerged
# work -- with no merged PR simulated here, that fallback correctly reports
# failed and preserves the worktree/branch for investigation.
printf '162 running open none 2026-01-01T00:00:00Z none none %s\n161 completed closed\n' "$gate_ready_body" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_POSTMORTEM_COMPLETE=1 FAKE_CODEX_POSTMORTEM_COMPLETE_ISSUE=162 FAKE_CODEX_COMMIT_ALL=1 FAKE_PR_MERGED=0 "$target/bin/agentic-loop" _worker 162 postmortem-leftover-worker
grep -Eq '^162 failed open' "$state" || fail 'a postmortem complete turn with a leftover commit was not routed to the ordinary merged-PR completion path'
[[ -e $target-worktrees/issue-162 ]] || fail 'a postmortem complete turn with a leftover commit had its worktree destroyed without a merged PR'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-162 || fail 'a postmortem complete turn with a leftover commit had its branch destroyed without a merged PR'
rm -rf "$state_root/postmortem"

# A branch already checked out by another worktree is retained without replacing that worktree.
printf '10 running open\n' > "$state"
other_worktree="$TEST_ROOT/other-issue-10"
git -C "$target" worktree add --quiet -b agent/issue-10 "$other_worktree" origin/main
"$target/bin/agentic-loop" _worker 10 other-worktree-worker
grep -Eq '^10 failed open$' "$state" || fail 'branch used by another worktree was accepted'
[[ -e $other_worktree ]] || fail 'other worktree was removed'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-10 || fail 'branch used by another worktree was removed'
[[ ! -e $target-worktrees/issue-10 ]] || fail 'worker replaced the expected path while the branch was in use'
git -C "$target" worktree remove "$other_worktree"
git -C "$target" branch -D agent/issue-10 >/dev/null

# Resolver failures stop before Codex starts and explain the safe recovery in Japanese.
printf '11 running open\n' > "$state"
mkdir -p "$target-worktrees/issue-11/.agents"
before_codex_calls=$(wc -l < "$FAKE_GH_ROOT/codex-calls")
"$target/bin/agentic-loop" _worker 11 invalid-git-common-dir-worker
grep -Eq '^11 failed open$' "$state" || fail 'unsafe Git common directory was accepted'
[[ $(wc -l < "$FAKE_GH_ROOT/codex-calls") -eq $before_codex_calls ]] || fail 'Git resolver failure started Codex'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:failed worker=invalid-git-common-dir-worker' 'Git resolver failure did not preserve its machine-readable marker'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'Git common directoryを安全に解決できなかったため、workerを起動しませんでした' 'Git resolver failure did not explain the reason and impact in Japanese'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '追加の書き込み可能pathは許可していません' 'Git resolver failure did not explain its safety measure in Japanese'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'Git metadataとworktreeの整合性を確認・復旧した後、このIssueを安全に再キューしてください' 'Git resolver failure did not explain recovery in Japanese'
rm -r "$target-worktrees/issue-11"

printf '12 running open\n' > "$state"
git -C "$target" worktree add --quiet -b agent/issue-12 "$target-worktrees/issue-12" origin/main
rm -r "$target-worktrees/issue-12/.agents"
before_codex_calls=$(wc -l < "$FAKE_GH_ROOT/codex-calls")
"$target/bin/agentic-loop" _worker 12 missing-agents-dir-worker
grep -Eq '^12 failed open$' "$state" || fail 'unsafe .agents directory was accepted'
[[ $(wc -l < "$FAKE_GH_ROOT/codex-calls") -eq $before_codex_calls ]] || fail '.agents resolver failure started Codex'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:failed worker=missing-agents-dir-worker' '.agents resolver failure did not preserve its machine-readable marker'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '.agents directoryを安全に解決できなかったため、workerを起動しませんでした' '.agents resolver failure did not explain the reason and impact in Japanese'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '保護対象のrepository pathは書き込み可能にしていません' '.agents resolver failure did not explain its safety measure in Japanese'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '.agents directoryを安全な通常directoryとして復旧した後、このIssueを安全に再キューしてください' '.agents resolver failure did not explain recovery in Japanese'
git -C "$target" worktree remove --force "$target-worktrees/issue-12"
git -C "$target" branch -D agent/issue-12 >/dev/null

# --- Workload scaling invariant (Issue #130, ADR 0025 T1/T2) ---------------
# One poll fetches the open-Issue snapshot at most twice (once at the top of
# the loop, once more before claiming so that queued transitions made by
# maintenance earlier in the same poll stay same-poll observable; see the
# refresh_supervisor_snapshot call sites in supervisor.sh), and that call
# count does not grow with the number of queued Issues
# (refresh_supervisor_snapshot aggregates every state-maintenance path into
# one list per fetch; see docs/operations/workload-budget.md). unknown_scope
# =open and a large enough MAX_WORKERS let every fixture Issue be claimed in
# a single run-once poll, so comparing the snapshot-fetch call count isolates
# the aggregation invariant from the (expected, unrelated) growth of
# per-claimed-Issue writes.
workload_scale_list_calls() {
  local n=$1 i
  : > "$state"
  for ((i = 1; i <= n; i++)); do
    printf '%d queued open none 2026-01-01T00:00:00Z\n' $((7300 + i)) >> "$state"
  done
  write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=8 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
  printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
  : > "$FAKE_GH_ROOT/$state_key.comments"
  local calls_before delta
  calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
  AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
  delta=$(tail -n +"$((calls_before + 1))" "$FAKE_GH_ROOT/calls")
  [[ $(awk '$2 == "completed" {count++} END {print count+0}' "$state") -eq $n ]] || fail "workload scaling fixture (n=$n) did not claim and complete every queued Issue"
  grep -Fc -- '-f state=open -f per_page=100 --paginate' <<< "$delta"
}
list_calls_n2=$(workload_scale_list_calls 2)
list_calls_n8=$(workload_scale_list_calls 8)
[[ $list_calls_n2 -le 2 ]] || fail "1 poll issued $list_calls_n2 open-Issue list calls for N=2 (expected at most 2)"
[[ $list_calls_n8 -le 2 ]] || fail "1 poll issued $list_calls_n8 open-Issue list calls for N=8 (expected at most 2)"
[[ $list_calls_n2 -eq $list_calls_n8 ]] || fail "open-Issue list call count grew with input size (N=2: $list_calls_n2, N=8: $list_calls_n8)"

fi

if [[ $TEST_GROUP == all || $TEST_GROUP == lifecycle ]]; then

# --- Worker resume from existing artifacts (see docs/decisions/0004) ---

# Regression: a worktree at this Issue's expected path but registered to a
# DIFFERENT Issue's branch must never be reused (previously only existence was
# checked, not branch identity, so the provider could be launched inside
# another Issue's worktree).
printf '13 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
git -C "$target" branch agent/issue-999-foreign
git -C "$target" worktree add --quiet "$target-worktrees/issue-13" agent/issue-999-foreign
before_codex_calls=$(wc -l < "$FAKE_GH_ROOT/codex-calls")
"$target/bin/agentic-loop" _worker 13 foreign-worker
grep -Eq '^13 failed open$' "$state" || fail 'foreign worktree/branch mismatch was accepted'
[[ $(wc -l < "$FAKE_GH_ROOT/codex-calls") -eq $before_codex_calls ]] || fail 'foreign worktree/branch mismatch started a provider'
[[ -e $target-worktrees/issue-13 ]] || fail 'foreign worktree was removed'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-999-foreign || fail 'foreign branch was removed'
! git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-13 || fail 'a new branch was created despite the foreign worktree conflict'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=foreign-artifact' 'foreign worktree/branch mismatch was not recorded with its reason code'
git -C "$target" worktree remove --force "$target-worktrees/issue-13"
git -C "$target" branch -D agent/issue-999-foreign >/dev/null

# Resuming an Issue whose PR is already merged (e.g. a prior worker crashed
# after GitHub merged the PR but before its own cleanup ran) completes and
# cleans up without ever starting a provider, so a duplicate PR or merge
# cannot be created.
printf '14 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
git -C "$target" worktree add --quiet -b agent/issue-14 "$target-worktrees/issue-14" origin/main
git -C "$target-worktrees/issue-14" commit --quiet --allow-empty -m 'resumed work'
merged_sha=$(git -C "$target-worktrees/issue-14" rev-parse HEAD)
before_codex_calls=$(wc -l < "$FAKE_GH_ROOT/codex-calls")
FAKE_RESUME_MERGED_PR=99 FAKE_RESUME_MERGED_SHA=$merged_sha FAKE_RESUME_MERGED_URL="https://github.example/acme/installed-project/pull/99" \
  "$target/bin/agentic-loop" _worker 14 resume-merged-worker
grep -Eq '^14 completed closed' "$state" || fail 'resuming an already-merged PR did not complete the Issue'
[[ $(wc -l < "$FAKE_GH_ROOT/codex-calls") -eq $before_codex_calls ]] || fail 'resuming an already-merged PR unnecessarily started a provider'
[[ ! -e $target-worktrees/issue-14 ]] || fail 'resuming an already-merged PR did not remove the worktree'
! git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-14 || fail 'resuming an already-merged PR did not remove the local branch'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:completed pr=99' 'resumed merged-PR completion was not recorded'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'resumed=1' 'resumed merged-PR completion did not mark itself as a resume fast path'

# A local branch at an ancestor of the merged PR head is also safe to
# complete: it has no local-only commits, so exact SHA equality is unnecessary
# (Issue #283).
printf '16 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
git -C "$target" worktree add --quiet -b agent/issue-16 "$target-worktrees/issue-16" origin/main
git -C "$target-worktrees/issue-16" commit --quiet --allow-empty -m 'resume ancestor base'
git -C "$target-worktrees/issue-16" branch agent/issue-16-merged
ancestor_head=$(git -C "$target-worktrees/issue-16" rev-parse HEAD)
git -C "$target-worktrees/issue-16" commit --quiet --allow-empty -m 'merged PR follow-up'
merged_head=$(git -C "$target-worktrees/issue-16" rev-parse HEAD)
git -C "$target-worktrees/issue-16" reset --quiet --hard "$ancestor_head"
FAKE_RESUME_MERGED_PR=100 FAKE_RESUME_MERGED_SHA=$merged_head FAKE_RESUME_MERGED_URL="https://github.example/acme/installed-project/pull/100" \
  "$target/bin/agentic-loop" _worker 16 resume-ancestor-worker
grep -Eq '^16 completed closed' "$state" || fail 'a merged PR whose local branch is an ancestor did not complete the Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:completed pr=100' 'ancestor resume completion was not recorded'
git -C "$target" branch -D agent/issue-16-merged >/dev/null 2>&1 || true

# A branch that has diverged from its remote (both sides carry commits the
# other lacks) is routed to needs-input; no force-push, reset, or deletion of
# either branch is attempted, and no provider is started.
printf '15 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
git -C "$target" worktree add --quiet -b agent/issue-15 "$target-worktrees/issue-15" origin/main
git -C "$target-worktrees/issue-15" commit --quiet --allow-empty -m 'remote side'
git -C "$target-worktrees/issue-15" push --quiet origin agent/issue-15
git -C "$target-worktrees/issue-15" reset --quiet --hard HEAD~1
git -C "$target-worktrees/issue-15" commit --quiet --allow-empty -m 'local side'
before_codex_calls=$(wc -l < "$FAKE_GH_ROOT/codex-calls")
"$target/bin/agentic-loop" _worker 15 diverged-worker
grep -Eq '^15 needs-input open$' "$state" || fail 'a diverged branch was not routed to needs-input'
[[ $(wc -l < "$FAKE_GH_ROOT/codex-calls") -eq $before_codex_calls ]] || fail 'a diverged branch unnecessarily started a provider'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-15 || fail 'a diverged local branch was removed'
git -C "$target" ls-remote --exit-code --heads origin refs/heads/agent/issue-15 >/dev/null || fail 'a diverged remote branch was removed'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=resume-diverged' 'a diverged branch was not recorded with its reason code'
git -C "$target" worktree remove --force "$target-worktrees/issue-15"
git -C "$target" branch -D agent/issue-15 >/dev/null
git -C "$target" push --quiet origin --delete agent/issue-15

# An uncommitted change in an existing worktree is not itself an anomaly: the
# worker proceeds through the normal plan/exec flow (here the provider commits
# the pending change) and completes, with the dirty flag preserved for audit
# in the handoff comment.
printf '16 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
git -C "$target" worktree add --quiet -b agent/issue-16 "$target-worktrees/issue-16" origin/main
printf 'uncommitted change\n' > "$target-worktrees/issue-16/dirty.txt"
FAKE_CODEX_COMMIT_ALL=1 "$target/bin/agentic-loop" _worker 16 dirty-worker
grep -Eq '^16 completed closed' "$state" || fail 'a dirty worktree resume did not proceed to normal completion'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'dirty=1' 'a dirty worktree resume was not recorded in the handoff'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'phase=worktree-ready' 'a dirty worktree resume phase was not recorded in the handoff'
[[ ! -e $target-worktrees/issue-16 ]] || fail 'a completed dirty-resume worktree remained'

# Resuming an Issue with an existing open PR injects the observed phase, PR
# number, and check status into the provider prompt so it reuses the existing
# branch/PR instead of proposing a new one, and the handoff comment records
# the same observed facts.
printf '17 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
git -C "$target" worktree add --quiet -b agent/issue-17 "$target-worktrees/issue-17" origin/main
git -C "$target-worktrees/issue-17" commit --quiet --allow-empty -m 'in-progress work'
FAKE_RESUME_OPEN_PR=42 FAKE_RESUME_OPEN_URL="https://github.example/acme/installed-project/pull/42" FAKE_RESUME_CHECKS=in_progress \
  "$target/bin/agentic-loop" _worker 17 resume-open-worker
# shellcheck disable=SC2016 # Backticks are literal Markdown in the expected provider prompt.
assert_contains "$FAKE_GH_ROOT/codex-calls" '既存のbranch `agent/issue-17` とPR #42 を再利用してください' 'an open-PR resume did not inject reuse instructions into the provider prompt'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'phase: pr-open' 'an open-PR resume did not inject the observed phase into the provider prompt'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'phase=pr-open' 'an open-PR resume phase was not recorded in the handoff'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'checks=in_progress' 'an open-PR resume check status was not recorded in the handoff'
git -C "$target" worktree remove --force "$target-worktrees/issue-17" 2>/dev/null || true
git -C "$target" branch -D agent/issue-17 >/dev/null 2>&1 || true

# An open PR that is behind the fetched default branch must be distinguished
# from an ahead-only PR even if its checks are already green.  The provider is
# instructed to merge (not rebase) the default branch, resolve conflicts,
# normally push, re-check, merge the existing PR, and verify default branch.
printf '18 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
git -C "$target" worktree add --quiet -b agent/issue-18 "$target-worktrees/issue-18" origin/main
git -C "$target-worktrees/issue-18" commit --quiet --allow-empty -m 'PR work behind main'
resume_head=$(git -C "$target-worktrees/issue-18" rev-parse HEAD)
git -C "$target-worktrees/issue-18" push --quiet origin agent/issue-18
git -C "$target" checkout --quiet main
git -C "$target" commit --quiet --allow-empty -m 'advanced default branch'
git -C "$target" push --quiet origin main
base_head=$(git -C "$target" rev-parse HEAD)
: > "$FAKE_GH_ROOT/calls"
FAKE_RESUME_OPEN_PR=43 FAKE_RESUME_OPEN_URL="https://github.example/acme/installed-project/pull/43" FAKE_RESUME_CHECKS=success \
  FAKE_RESUME_BASE_SHA=$base_head FAKE_RESUME_HEAD_SHA=$resume_head FAKE_RESUME_MERGEABLE=true FAKE_RESUME_MERGEABLE_STATE=clean \
  "$target/bin/agentic-loop" _worker 18 resume-behind-worker
assert_contains "$FAKE_GH_ROOT/codex-calls" 'phase: needs-rebase' 'a behind open PR was not routed to needs-rebase'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'git merge origin/main' 'needs-rebase did not instruct a normal default-branch merge'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'rebase、reset、force-push、履歴書き換えは禁止です' 'needs-rebase did not prohibit history rewriting'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'phase=needs-rebase' 'needs-rebase was not recorded in the handoff'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'behind=1' 'the handoff did not record the default-branch behind count'
assert_contains "$FAKE_GH_ROOT/calls" 'pulls/43' 'open-PR resume did not use the REST PR-detail endpoint'
git -C "$target" worktree remove --force "$target-worktrees/issue-18" 2>/dev/null || true
git -C "$target" branch -D agent/issue-18 >/dev/null 2>&1 || true

# GitHub's definitive false result also enters the convergence route even when
# the branch is not behind, and an asynchronous unknown result falls back to
# read-only merge-tree using the fetched commits.
printf '19 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
git -C "$target" worktree add --quiet -b agent/issue-19 "$target-worktrees/issue-19" origin/main
printf 'base\n' > "$target-worktrees/issue-19/resume-conflict.txt"
git -C "$target-worktrees/issue-19" add resume-conflict.txt
git -C "$target-worktrees/issue-19" commit --quiet -m 'branch side conflict'
resume_head=$(git -C "$target-worktrees/issue-19" rev-parse HEAD)
git -C "$target-worktrees/issue-19" push --quiet origin agent/issue-19
printf 'default\n' > "$target/resume-conflict.txt"
git -C "$target" add resume-conflict.txt
git -C "$target" commit --quiet -m 'default side conflict'
git -C "$target" push --quiet origin main
base_head=$(git -C "$target" rev-parse HEAD)
FAKE_RESUME_OPEN_PR=44 FAKE_RESUME_OPEN_URL="https://github.example/acme/installed-project/pull/44" FAKE_RESUME_CHECKS=success \
  FAKE_RESUME_BASE_SHA=$base_head FAKE_RESUME_HEAD_SHA=$resume_head FAKE_RESUME_MERGEABLE=false FAKE_RESUME_MERGEABLE_STATE=dirty \
  "$target/bin/agentic-loop" _worker 19 resume-conflict-worker
assert_contains "$FAKE_GH_ROOT/codex-calls" 'phase: needs-rebase' 'an explicitly unmergeable PR was not routed to needs-rebase'
git -C "$target" worktree remove --force "$target-worktrees/issue-19" 2>/dev/null || true
git -C "$target" branch -D agent/issue-19 >/dev/null 2>&1 || true

printf '20 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
git -C "$target" worktree add --quiet -b agent/issue-20 "$target-worktrees/issue-20" origin/main
printf 'branch again\n' > "$target-worktrees/issue-20/resume-conflict.txt"
git -C "$target-worktrees/issue-20" add resume-conflict.txt
git -C "$target-worktrees/issue-20" commit --quiet -m 'second branch conflict'
resume_head=$(git -C "$target-worktrees/issue-20" rev-parse HEAD)
git -C "$target-worktrees/issue-20" push --quiet origin agent/issue-20
printf 'default again\n' > "$target/resume-conflict.txt"
git -C "$target" add resume-conflict.txt
git -C "$target" commit --quiet -m 'second default conflict'
git -C "$target" push --quiet origin main
base_head=$(git -C "$target" rev-parse HEAD)
FAKE_RESUME_OPEN_PR=45 FAKE_RESUME_OPEN_URL="https://github.example/acme/installed-project/pull/45" FAKE_RESUME_CHECKS=success \
  FAKE_RESUME_BASE_SHA=$base_head FAKE_RESUME_HEAD_SHA=$resume_head FAKE_RESUME_MERGEABLE=null FAKE_RESUME_MERGEABLE_STATE=unknown \
  "$target/bin/agentic-loop" _worker 20 resume-unknown-worker
assert_contains "$FAKE_GH_ROOT/codex-calls" 'phase: needs-rebase' 'merge-tree did not classify an unknown conflicting PR as needs-rebase'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'mergeable=false' 'merge-tree fallback result was not recorded in the handoff'
git -C "$target" worktree remove --force "$target-worktrees/issue-20" 2>/dev/null || true
git -C "$target" branch -D agent/issue-20 >/dev/null 2>&1 || true

# Resuming an Issue with an unpushed local commit (worker crashed after
# committing but before push, with no PR yet) injects the observed phase and
# a reuse-the-branch instruction (without a PR to reuse) into the provider
# prompt, and the handoff comment records the same observed facts. The
# branch is not pushed by the resume probe itself.
printf '5001 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
git -C "$target" worktree add --quiet -b agent/issue-5001 "$target-worktrees/issue-5001" origin/main
git -C "$target-worktrees/issue-5001" commit --quiet --allow-empty -m 'unpushed local work'
"$target/bin/agentic-loop" _worker 5001 resume-committed-worker
assert_contains "$FAKE_GH_ROOT/codex-calls" 'phase: committed-unpushed' 'an unpushed-commit resume did not inject the observed phase into the provider prompt'
# shellcheck disable=SC2016 # Backticks are literal Markdown in the expected provider prompt.
assert_contains "$FAKE_GH_ROOT/codex-calls" '既存のbranch `agent/issue-5001` を再利用してください（新規branchは作成しないでください）' 'an unpushed-commit resume did not instruct the provider to reuse the existing branch'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'phase=committed-unpushed' 'an unpushed-commit resume phase was not recorded in the handoff'
! git -C "$target" ls-remote --exit-code --heads origin refs/heads/agent/issue-5001 >/dev/null 2>&1 || fail 'an unpushed-commit resume unexpectedly pushed the branch'
git -C "$target" worktree remove --force "$target-worktrees/issue-5001" 2>/dev/null || true
git -C "$target" branch -D agent/issue-5001 >/dev/null 2>&1 || true

# Resuming an Issue whose commit was pushed but never got a PR (worker
# crashed between push and PR creation) injects the observed phase and the
# same reuse-the-branch instruction, and the handoff comment records the
# same observed facts.
printf '5002 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
git -C "$target" worktree add --quiet -b agent/issue-5002 "$target-worktrees/issue-5002" origin/main
git -C "$target-worktrees/issue-5002" commit --quiet --allow-empty -m 'pushed work, no PR yet'
git -C "$target-worktrees/issue-5002" push --quiet origin agent/issue-5002
"$target/bin/agentic-loop" _worker 5002 resume-pushed-worker
assert_contains "$FAKE_GH_ROOT/codex-calls" 'phase: pushed-no-pr' 'a pushed-but-no-PR resume did not inject the observed phase into the provider prompt'
# shellcheck disable=SC2016 # Backticks are literal Markdown in the expected provider prompt.
assert_contains "$FAKE_GH_ROOT/codex-calls" '既存のbranch `agent/issue-5002` を再利用してください（新規branchは作成しないでください）' 'a pushed-but-no-PR resume did not instruct the provider to reuse the existing branch'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'phase=pushed-no-pr' 'a pushed-but-no-PR resume phase was not recorded in the handoff'
git -C "$target" worktree remove --force "$target-worktrees/issue-5002" 2>/dev/null || true
git -C "$target" branch -D agent/issue-5002 >/dev/null 2>&1 || true
git -C "$target" push --quiet origin --delete agent/issue-5002

# Resuming an Issue whose local branch is gone (a different host progressed
# it, or a local cleanup removed it) but whose remote agent branch still
# carries push-completed work must resume from that remote tip, not silently
# discard the pushed work and start over from the default branch (Issue
# #210). The materialized local branch feeds the same observed-phase
# machinery as every other resume case, so the handoff/prompt report
# `pushed-no-pr` (no PR exists yet) rather than `fresh`.
printf '5004 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
git -C "$target" worktree add --quiet -b agent/issue-5004 "$target-worktrees/issue-5004" origin/main
git -C "$target-worktrees/issue-5004" commit --quiet --allow-empty -m 'remote-only prior work'
remote_only_head=$(git -C "$target-worktrees/issue-5004" rev-parse HEAD)
git -C "$target-worktrees/issue-5004" push --quiet origin agent/issue-5004
git -C "$target" worktree remove --force "$target-worktrees/issue-5004"
git -C "$target" branch -D agent/issue-5004 >/dev/null
! git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-5004 \
  || fail 'the local branch fixture setup for the remote-only resume test did not remove the local branch'
# needs-input keeps the worktree/branch untouched (unlike the default
# completed-and-cleaned-up path) so the created worktree can be inspected below.
FAKE_CODEX_RESULT=AGENTIC_LOOP_RESULT=needs-input "$target/bin/agentic-loop" _worker 5004 resume-remote-only-worker
[[ -e $target-worktrees/issue-5004 ]] || fail 'a local-branch-missing resume with a remote agent branch did not create a worktree'
worktree_head=$(git -C "$target-worktrees/issue-5004" rev-parse HEAD)
[[ $worktree_head == "$remote_only_head" ]] \
  || fail "a local-branch-missing resume with a remote agent branch did not reuse the remote branch tip (got $worktree_head, want $remote_only_head)"
default_head=$(git -C "$target" rev-parse origin/main)
[[ $worktree_head != "$default_head" ]] \
  || fail 'a local-branch-missing resume with a remote agent branch fell back to the default branch instead'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'phase: pushed-no-pr' 'a remote-only resume did not inject the observed phase into the provider prompt'
# shellcheck disable=SC2016 # Backticks are literal Markdown in the expected provider prompt.
assert_contains "$FAKE_GH_ROOT/codex-calls" '既存のbranch `agent/issue-5004` を再利用してください（新規branchは作成しないでください）' 'a remote-only resume did not instruct the provider to reuse the materialized branch'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'phase=pushed-no-pr' 'a remote-only resume phase was not recorded in the handoff'
git -C "$target" worktree remove --force "$target-worktrees/issue-5004" 2>/dev/null || true
git -C "$target" branch -D agent/issue-5004 >/dev/null 2>&1 || true
git -C "$target" push --quiet origin --delete agent/issue-5004

# When neither the local branch nor a remote agent branch exists, resume
# stays a true fresh start: the best-effort remote fetch finds nothing and
# the worktree/branch is created from the default branch exactly as before
# (Issue #210 acceptance criterion 2 -- no regression to the ordinary fresh
# path).
printf '5005 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
! git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-5005 \
  || fail 'the branch fixture precondition for the true-fresh resume test was not met'
! git -C "$target" ls-remote --exit-code --heads origin refs/heads/agent/issue-5005 >/dev/null 2>&1 \
  || fail 'the remote branch fixture precondition for the true-fresh resume test was not met'
FAKE_CODEX_RESULT=AGENTIC_LOOP_RESULT=needs-input "$target/bin/agentic-loop" _worker 5005 resume-true-fresh-worker
[[ -e $target-worktrees/issue-5005 ]] || fail 'a true-fresh resume did not create a worktree'
fresh_head=$(git -C "$target-worktrees/issue-5005" rev-parse HEAD)
default_head=$(git -C "$target" rev-parse origin/main)
[[ $fresh_head == "$default_head" ]] \
  || fail "a true-fresh resume (no local or remote branch) did not create the worktree from the default branch (got $fresh_head, want $default_head)"
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'phase=fresh' 'a true-fresh resume phase was not recorded in the handoff'
git -C "$target" worktree remove --force "$target-worktrees/issue-5005" 2>/dev/null || true
git -C "$target" branch -D agent/issue-5005 >/dev/null 2>&1 || true

# A worktree path that exists but is not a registered Git worktree at all
# (corrupted metadata, e.g. a plain directory left over from a filesystem
# restore) is not misclassified by resume_probe as a foreign-artifact
# conflict; it falls through to the git-common-dir resolver immediately
# after, which fails safely: the Issue is marked failed and nothing under
# the path is deleted, so a human can inspect and repair it.
printf '5003 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
before_codex_calls=$(wc -l < "$FAKE_GH_ROOT/codex-calls")
mkdir -p "$target-worktrees/issue-5003"
printf 'leftover artifact\n' > "$target-worktrees/issue-5003/marker.txt"
"$target/bin/agentic-loop" _worker 5003 corrupt-metadata-worker
grep -Eq '^5003 failed open$' "$state" || fail 'a corrupted worktree path was not marked failed'
[[ $(wc -l < "$FAKE_GH_ROOT/codex-calls") -eq $before_codex_calls ]] || fail 'a corrupted worktree path started a provider'
[[ -f $target-worktrees/issue-5003/marker.txt ]] || fail 'a corrupted worktree path had its contents deleted'
! git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-5003 || fail 'a corrupted worktree path unexpectedly created a branch'
! grep -Fq 'reason=foreign-artifact' "$FAKE_GH_ROOT/$state_key.comments" || fail 'a corrupted worktree path was misclassified as a foreign-artifact conflict'
rm -rf "$target-worktrees/issue-5003"

# A stale or duplicate _worker invocation whose Issue is no longer
# agent:running (e.g. it already raced a requeue) makes no Git, Label, or
# comment change at all.
printf '18 queued open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
before_codex_calls=$(wc -l < "$FAKE_GH_ROOT/codex-calls")
"$target/bin/agentic-loop" _worker 18 stale-duplicate-worker
grep -Eq '^18 queued open$' "$state" || fail 'a duplicate worker invocation changed the Issue label'
[[ $(wc -l < "$FAKE_GH_ROOT/codex-calls") -eq $before_codex_calls ]] || fail 'a duplicate worker invocation started a provider'
[[ ! -s $FAKE_GH_ROOT/$state_key.comments ]] || fail 'a duplicate worker invocation posted a comment'
[[ ! -e $target-worktrees/issue-18 ]] || fail 'a duplicate worker invocation created a worktree'

# `status` surfaces the observed resume phase for a running Issue from the
# worker's local phase cache, with no additional GitHub API calls of its own.
printf '19 running open\n' > "$state"
git -C "$target" worktree add --quiet -b agent/issue-19 "$target-worktrees/issue-19" origin/main
FAKE_CODEX_SLEEP=2 "$target/bin/agentic-loop" _worker 19 phase-status-worker &
worker_bg_pid=$!
status_output=''
__phase_status_check() { status_output=$("$target/bin/agentic-loop" status); grep -Fq 'phase:' <<< "$status_output"; }
wait_for 'status displayed the observed resume phase' __phase_status_check || true
wait "$worker_bg_pid"
grep -Fq '#19' <<< "$status_output" || fail 'status did not list the running Issue'
grep -Fq 'phase: worktree-ready' <<< "$status_output" || fail 'status did not display the observed resume phase'

# needs-input and failure are isolated state transitions; a later Issue reply requeues only that Issue.
printf '4 running open\n5 running open\n' > "$state"
FAKE_CODEX_RESULT=AGENTIC_LOOP_RESULT=needs-input "$target/bin/agentic-loop" _worker 4 test-worker
grep -Eq '^4 needs-input open$' "$state" || fail 'needs-input result was not recorded'
FAKE_CODEX_RESULT=AGENTIC_LOOP_RESULT=failed "$target/bin/agentic-loop" _worker 5 test-worker
grep -Eq '^5 failed open$' "$state" || fail 'failed result was not isolated'
printf '4 USER_REPLY\n' >> "$FAKE_GH_ROOT/$state_key.comments"
: > "$state_root/stop.requested"
"$target/bin/agentic-loop" _supervise
grep -Eq '^4 queued open$' "$state" || fail 'Issue reply did not requeue needs-input work'
grep -Eq '^5 failed open$' "$state" || fail 'one Issue reply changed another failed Issue'

# requeue_answered reads every page of comments, not just the first 100
# (Issue #192): a needs-input Issue whose marker and human reply both live
# past the 100th comment is still detected and requeued. The post-marker
# filler comments carry the agentic-loop marker prefix too (like the
# worker's own heartbeat/usage comments would), so only the final,
# marker-free comment counts as a human reply.
printf '6 needs-input open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
for i in $(seq 1 90); do printf '6 filler comment %s\n' "$i" >> "$FAKE_GH_ROOT/$state_key.comments"; done
printf '6 <!-- agentic-loop:needs-input worker=test-worker -->\\n人手の判断が必要です。\n' >> "$FAKE_GH_ROOT/$state_key.comments"
for i in $(seq 92 105); do printf '6 <!-- agentic-loop:usage worker=test-worker seq=%s -->\n' "$i" >> "$FAKE_GH_ROOT/$state_key.comments"; done
printf '6 USER_REPLY 対応しました\n' >> "$FAKE_GH_ROOT/$state_key.comments"
[[ $(wc -l < "$FAKE_GH_ROOT/$state_key.comments") -gt 100 ]] || fail 'fixture did not exceed 100 comments'
: > "$state_root/stop.requested"
"$target/bin/agentic-loop" _supervise
grep -Eq '^6 queued open$' "$state" || fail 'a reply past the 100th comment on a needs-input Issue was not detected'

# The same Issue, still unanswered, stays in needs-input (100-or-fewer-comment
# behavior is unchanged): the marker is present but nothing follows it.
printf '7 needs-input open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
printf '7 <!-- agentic-loop:needs-input worker=test-worker -->\\n人手の判断が必要です。\n' >> "$FAKE_GH_ROOT/$state_key.comments"
: > "$state_root/stop.requested"
"$target/bin/agentic-loop" _supervise
grep -Eq '^7 needs-input open$' "$state" || fail 'an unanswered needs-input Issue was requeued'

# A crashed worker's expired lease returns to the queue on the next supervisor start.
printf '9 running open\n' > "$state"
printf '9 <!-- agentic-loop:lease worker=dead heartbeat=1 expires=1 -->\n' > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root"
: > "$state_root/stop.requested"
"$target/bin/agentic-loop" _supervise
grep -Eq '^9 queued open$' "$state" || fail 'expired running Issue was not recovered'

# A worker that keeps dying before finishing (lease expiry / crash, never an
# explicit AGENTIC_LOOP_RESULT=failed) must not requeue forever: once its recorded
# claim attempts reach MAX_ATTEMPTS, recover_expired escalates it to agent:failed,
# and retry_failed parks it (open, non-claim) instead of closing it as
# unresolvable (see docs/decisions/0016) or looping in the queue.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=2 RETRY_COOLDOWN_SECONDS=0
printf '91 running open\n' > "$state"
printf '91 <!-- agentic-loop:lease worker=dead heartbeat=1 expires=1 -->\n' > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root/attempts"; printf '2\t0\n' > "$state_root/attempts/issue-91"
rm -f "$state_root/stop.requested"
rm -f "$closes"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^91 parked open$' "$state" || fail 'a worker that kept dying before finishing was not parked after MAX_ATTEMPTS'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:recover-exhausted' 'lease-death escalation was not recorded on the Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:parked' 'escalated Issue was not parked'
[[ ! -e $state_root/attempts/issue-91 ]] || fail 'attempts counter was not cleared after park'
[[ ! -r "$closes" ]] || ! grep -Fq $'^91\t' "$closes" || fail 'a lease-death-escalated Issue must never be closed'

# Under the attempt limit, a crashed worker's Issue still returns to the queue
# (no premature escalation) so ordinary transient deaths keep retrying.
printf '92 running open\n' > "$state"
printf '92 <!-- agentic-loop:lease worker=dead heartbeat=1 expires=1 -->\n' > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root/attempts"; printf '1\t0\n' > "$state_root/attempts/issue-92"
: > "$state_root/stop.requested"
"$target/bin/agentic-loop" _supervise
grep -Eq '^92 queued open$' "$state" || fail 'a crashed worker under the attempt limit was not requeued'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:recovered' 'under-limit recovery was not recorded as a normal requeue'
rm -f "$state_root/attempts/issue-91" "$state_root/attempts/issue-92"

# A lease-death that coincides with an active pool-exhaustion marker is
# treated as environment-caused (provider stalling/erroring under its own
# rate/usage limit), never as proof the task itself is unresolvable: attempts
# are cleared and the Issue is requeued even past MAX_ATTEMPTS, instead of
# being escalated to agent:failed -> agent:parked (Issue #158 root cause:
# "crash / timeout は枯渇保護を通らない").
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=2 RETRY_COOLDOWN_SECONDS=0
printf '93 running open\n' > "$state"
printf '93 <!-- agentic-loop:lease worker=dead heartbeat=1 expires=1 -->\n' > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root/attempts"; printf '2\t0\n' > "$state_root/attempts/issue-93"
mkdir -p "$state_root/pools/codex"
printf '%s\n' "$(( $(date +%s) + 1800 ))" > "$state_root/pools/codex/exhausted"
rm -f "$state_root/stop.requested"
rm -f "$closes"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^93 queued open' "$state" || fail 'a lease-death correlated with pool exhaustion was parked instead of requeued'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:recovered pool-exhaustion=1' 'pool-exhaustion-correlated recovery was not recorded distinctly'
[[ ! -e $state_root/attempts/issue-93 ]] || fail 'attempts counter was not cleared for a pool-exhaustion-correlated lease death'
[[ ! -r "$closes" ]] || ! grep -Fq $'^93\t' "$closes" || fail 'a pool-exhaustion-correlated lease death must never be closed'
rm -rf "$state_root/pools"

# Recovery also runs inside the poll loop, not only at startup: a running Issue
# whose worker died (expired lease) is recovered and processed while the
# supervisor keeps running, instead of remaining stuck at agent:running forever.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '19 running open none 2026-01-01T00:00:00Z\n' > "$state"
printf '19 <!-- agentic-loop:lease worker=dead heartbeat=1 expires=1 -->\n' > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^19 completed closed' "$state" || fail 'stuck running Issue was not recovered and processed by the active loop'

# Graceful shutdown: SIGTERM to the supervisor terminates the worker's process
# group and requeues its in-flight Issue, leaving nothing orphaned.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
printf '15 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
FAKE_CODEX_SLEEP=30 "$target/bin/agentic-loop" _supervise &
sup_pid=$!
graceful_claimed=0
wait_for 'graceful claimed' test -e "$state_root/workers/15.pid" && graceful_claimed=1
[[ $graceful_claimed == 1 ]] || { kill "$sup_pid" 2>/dev/null; fail 'worker was not claimed before the shutdown test'; }
kill -TERM "$sup_pid" 2>/dev/null
wait "$sup_pid" 2>/dev/null || true
grep -Eq '^15 queued' "$state" || fail 'graceful shutdown did not requeue the in-flight Issue'
[[ ! -e $state_root/workers/15.pid ]] || fail 'graceful shutdown left a worker pidfile'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:shutdown' 'graceful shutdown was not recorded on the Issue'

# A graceful shutdown marker must not disable the next service boot.  The
# systemd/manual entrypoint is _service, so exercise that path directly.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
: > "$state"
: > "$state_root/stop.requested"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _service
[[ ! -e $state_root/stop.requested ]] || fail 'service startup did not clear a stale stop.requested marker'

# --- .pid loss must not defeat max_workers or graceful shutdown (Issue #219) ---
# Root cause 1: worker_count() (and therefore claim_next's `< MAX_WORKERS`
# gate) counted only workers/*.pid files. A lost/mismatched pidfile made a
# genuinely running worker invisible to that count, letting the supervisor
# claim a second Issue past max_workers=1. reconcile_worker_pidfiles() now
# repairs a missing pidfile from a live /proc scan before every count, so
# deleting a still-live worker's pidfile must not let a second queued Issue
# be claimed alongside it.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30
printf '2190 queued open none 2026-01-01T00:00:00Z\n2191 queued open none 2026-01-01T00:00:01Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
FAKE_CODEX_SLEEP=30 "$target/bin/agentic-loop" _supervise &
pidloss_sup_pid=$!
pidloss_claimed=0
wait_for 'pidloss claimed' test -r "$state_root/workers/2190.pid" && pidloss_claimed=1
[[ $pidloss_claimed == 1 ]] || { kill "$pidloss_sup_pid" 2>/dev/null; wait "$pidloss_sup_pid" 2>/dev/null; fail 'a worker was not claimed before the pidfile-loss max_workers test'; }
pidloss_worker_pid=$(cat "$state_root/workers/2190.pid")
rm -f "$state_root/workers/2190.pid"
# Give the supervisor several full poll cycles to reconcile the lost pidfile
# from its live /proc scan and to (incorrectly, if the fix regressed) claim
# the second queued Issue. wait_polls proves N complete poll bodies ran
# instead of hoping a fixed sleep covered that many at poll_seconds=1.
wait_polls 'pidfile-loss reconcile polls'
pidloss_second_claimed=0
grep -Eq '^2191 running' "$state" && pidloss_second_claimed=1
pidloss_reconciled=0
[[ -r $state_root/workers/2190.pid ]] && pidloss_reconciled=1
kill -TERM "$pidloss_sup_pid" 2>/dev/null || true
wait "$pidloss_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
[[ $pidloss_second_claimed == 0 ]] || fail 'worker_count() undercounted a live worker after its pidfile was lost, letting claim_next exceed max_workers'
[[ $pidloss_reconciled == 1 ]] || fail 'a lost pidfile for a still-live worker was not reconciled from the live process scan'
kill -0 "$pidloss_worker_pid" 2>/dev/null && { kill -TERM "-$pidloss_worker_pid" 2>/dev/null || true; wait "$pidloss_worker_pid" 2>/dev/null || true; }
rm -f "$state_root/workers/2190.pid"

# Root cause 3: graceful shutdown / stop iterated only workers/*.pid, so a
# worker whose pidfile had already gone missing was left running (never
# TERM'd) and its Issue was never requeued. reconcile_worker_pidfiles() runs
# first inside supervisor_graceful_shutdown() too, so shutdown must still
# find, stop, and requeue a worker whose pidfile was lost moments earlier.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30
printf '2192 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
FAKE_CODEX_SLEEP=30 "$target/bin/agentic-loop" _supervise &
shutpid_sup_pid=$!
shutpid_claimed=0
wait_for 'shutpid claimed' test -r "$state_root/workers/2192.pid" && shutpid_claimed=1
[[ $shutpid_claimed == 1 ]] || { kill "$shutpid_sup_pid" 2>/dev/null; wait "$shutpid_sup_pid" 2>/dev/null; fail 'a worker was not claimed before the pidfile-loss shutdown test'; }
shutpid_worker_pid=$(cat "$state_root/workers/2192.pid")
rm -f "$state_root/workers/2192.pid"
kill -TERM "$shutpid_sup_pid" 2>/dev/null
wait "$shutpid_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
grep -Eq '^2192 queued' "$state" || fail 'a worker whose pidfile was lost was not requeued by shutdown (Issue #219 requirement 3)'
[[ ! -e $state_root/workers/2192.pid ]] || fail 'shutdown left a pidfile behind for the pidfile-loss scenario'
shutpid_worker_gone=0
wait_gone 'shutpid worker gone' "$shutpid_worker_pid" && shutpid_worker_gone=1
[[ $shutpid_worker_gone == 1 ]] || fail 'a worker whose pidfile was lost was left running after shutdown (orphaned process)'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:shutdown' 'graceful shutdown was not recorded on the Issue for the pidfile-loss scenario'

# Root cause 2: supervisor/worker liveness (service.sh's pid_alive,
# worker_state.sh's worker_alive) matched cmdline against an absolute path.
# A supervisor started with a relative path (the real-world trigger:
# `./bin/agentic-loop _service`) recorded a relative argv[0], so the check
# failed even though the process was alive, misreporting a live supervisor
# as having a stale pid. bin/agentic-loop now re-execs itself through its
# canonical absolute path on startup, so this must no longer misfire.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested" "$state_root/supervisor.pid"
( cd "$target" && exec ./bin/agentic-loop _supervise ) &
relpath_sup_pid=$!
relpath_seen=0
wait_for 'relpath seen' test -r "$state_root/supervisor.pid" && relpath_seen=1
[[ $relpath_seen == 1 ]] || { kill "$relpath_sup_pid" 2>/dev/null; wait "$relpath_sup_pid" 2>/dev/null; fail 'a supervisor started via a relative path did not publish its pid'; }
relpath_status=$("$target/bin/agentic-loop" status)
if grep -Fq 'supervisor-stale-pid' <<< "$relpath_status"; then fail 'a supervisor started via a relative path was misreported as having a stale pid'; fi
grep -Fq 'running (pid' <<< "$relpath_status" || fail 'a supervisor started via a relative path was not recognized as running'
relpath_worktree="$target-worktrees/issue-261"
mkdir -p "$(dirname "$relpath_worktree")"
git -C "$target" worktree add --quiet --detach "$relpath_worktree"
worktree_status=$("$relpath_worktree/bin/agentic-loop" status)
grep -Fq 'running (pid' <<< "$worktree_status" || fail 'status from a dedicated worktree did not recognize the main-worktree supervisor'
"$relpath_worktree/bin/agentic-loop" start
[[ $(cat "$state_root/supervisor.pid") == "$relpath_sup_pid" ]] || fail 'start from a dedicated worktree duplicated the supervisor'
git -C "$target" worktree remove --force "$relpath_worktree"
kill -TERM "$relpath_sup_pid" 2>/dev/null || true
wait "$relpath_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"

# Restart recovery fast path: a running Issue whose LOCAL worker has died is
# requeued immediately from local state and reprocessed, without depending on the
# GitHub lease.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
printf '16 running open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
mkdir -p "$state_root/workers"
sh -c 'exit 0' & deadpid=$!
wait "$deadpid" 2>/dev/null || true
printf '%s\n' "$deadpid" > "$state_root/workers/16.pid"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^16 completed closed' "$state" || fail 'a dead local worker Issue was not recovered and reprocessed'

# Cheap lease: heartbeats update a single lease comment in place (PATCH) instead
# of posting a new comment each beat, so only one lease comment exists per Issue.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '20 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=3 "$target/bin/agentic-loop" _supervise
lease_lines=$(grep -c 'agentic-loop:lease' "$FAKE_GH_ROOT/$state_key.comments" || true)
[[ ${lease_lines:-0} -eq 1 ]] || fail "lease heartbeat should keep a single comment, found ${lease_lines:-0}"
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'のハートビートです' 'lease comment was not written'
# lease_body's printf path (Issue #110): its two markers and the trailing
# human-readable text must be separated by real newlines, encoded here as the
# 2-char `\n`.
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '-->\n<!-- agentic-loop:lease' 'lease_body did not render a real newline between its claim/lease markers'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '-->\nAgentic Loop worker' 'lease_body did not render a real newline before its human-readable text'

# Two hosts may observe the same queued snapshot. A still-valid claim created
# by the other host wins by comment id, so this Supervisor must neither change
# the Label nor start a duplicate worker. Once that claim expires, the Issue is
# claimable and completes normally.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '21 queued open none 2026-01-01T00:00:00Z\n' > "$state"
now=$(date +%s)
printf '21 <!-- agentic-loop:claim worker=other-host created=%s expires=%s -->\\n<!-- agentic-loop:lease worker=other-host heartbeat=%s expires=%s -->\n' "$now" "$((now + 30))" "$now" "$((now + 30))" > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^21 queued open' "$state" || fail 'a second host stole an Issue with a valid distributed claim'
sed -i 's/expires=[0-9][0-9]*/expires=1/g' "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^21 completed closed' "$state" || fail 'an Issue did not become claimable after the other host claim expired'

# Adaptive idle backoff: with an empty queue the poll interval grows beyond the
# base toward the ceiling, cutting idle GitHub API reads; a live worker resets it.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 POLL_MAX_SECONDS=8 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested" "$state_root/poll-interval"
"$target/bin/agentic-loop" _supervise &
backoff_sup=$!
backoff_val=0
__backoff_check() { backoff_val=$(cat "$state_root/poll-interval" 2>/dev/null || printf '0'); [[ $backoff_val =~ ^[0-9]+$ ]] && (( backoff_val > 1 )); }
wait_for 'idle backoff lengthened' __backoff_check || true
kill -TERM "$backoff_sup" 2>/dev/null || true
wait "$backoff_sup" 2>/dev/null || true
rm -f "$state_root/stop.requested"
(( backoff_val > 1 )) || fail "idle backoff did not lengthen the poll interval (got $backoff_val)"
(( backoff_val <= 8 )) || fail "idle backoff exceeded the configured ceiling (got $backoff_val)"

# --- per-worker hang timeout (Issue #108, ADR 0006) ---
# A lease heartbeat only proves the worker process is alive; it does not prove
# the work is progressing. A worker that hangs internally (e.g. an unresponsive
# API call) is killed process-group-wide once it exceeds worker_timeout_seconds,
# even though its lease is still valid, and the freed max_workers=1 slot lets
# the rest of the queue keep moving instead of stalling behind the hang.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=8
printf '50 queued open none 2026-01-01T00:00:00Z\n51 queued open none 2026-01-01T00:00:01Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
FAKE_CODEX_SLEEP=60 FAKE_CODEX_SLEEP_ISSUE_51=1 "$target/bin/agentic-loop" _supervise &
hang_sup_pid=$!
hang_worker_pid=''
__hang_worker_pid_check() { [[ -r $state_root/workers/50.pid ]] && hang_worker_pid=$(cat "$state_root/workers/50.pid"); }
wait_for 'hung worker claimed' __hang_worker_pid_check
[[ -n $hang_worker_pid ]] || { kill "$hang_sup_pid" 2>/dev/null; wait "$hang_sup_pid" 2>/dev/null; fail 'hung worker was not claimed before the timeout test'; }
# Drive the elapsed-time boundary through its persisted clock input instead of
# waiting eight wall-clock seconds. The next supervisor poll must enforce the
# same configured timeout against this already-expired start timestamp.
printf '%s\n' "$(($(date +%s) - 9))" > "$state_root/workers/50.started"
hang_timed_out=0
wait_for 'hung worker timed out' grep -Eq '^50 failed' "$state" && hang_timed_out=1
[[ $hang_timed_out == 1 ]] || { kill "$hang_sup_pid" 2>/dev/null; wait "$hang_sup_pid" 2>/dev/null; fail 'a hung worker was not detected and failed within the configured timeout'; }
[[ ! -e $state_root/workers/50.pid ]] || fail 'a timed-out worker pidfile was not cleared'
# kill -0 also succeeds against a not-yet-reaped zombie, so poll briefly
# instead of asserting on a single sample right after the state flip.
hang_worker_gone=0
wait_gone 'hung worker process group gone' "$hang_worker_pid" && hang_worker_gone=1
[[ $hang_worker_gone == 1 ]] || { kill "$hang_sup_pid" 2>/dev/null; wait "$hang_sup_pid" 2>/dev/null; fail 'a timed-out worker process group left an orphan process behind'; }
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:worker-timeout' 'the worker-timeout disposition was not audited on the Issue'
# lease_release's PATCH body (Issue #110): the claim/lease markers and the
# human-readable text must be separated by a real newline, encoded here as
# the 2-char `\n` (a regression would encode as the 3-char `\\n`, see
# encode_comment_body).
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'expires=0 -->\n<!-- agentic-loop:lease' 'lease_release did not render a real newline between its claim/lease markers'
hang_queue_progressed=0
wait_for 'queue progressed past the hung worker' grep -Eq '^51 completed closed' "$state" && hang_queue_progressed=1
kill -TERM "$hang_sup_pid" 2>/dev/null || true
wait "$hang_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
[[ $hang_queue_progressed == 1 ]] || fail 'a hung worker under max_workers=1 blocked the rest of the queue instead of freeing the slot'
grep -Eq '^50 failed' "$state" || fail 'the hung Issue was reclaimed before its retry cooldown elapsed'

# A hang that coincides with an active pool-exhaustion marker is treated as
# environment-caused (the provider likely stalling under its own rate/usage
# limit, e.g. a blocked long-poll that never returns to hit the ordinary
# post-hoc exhaustion classification), not as proof this Issue's task itself
# hung: it is requeued directly without burning attempts, instead of being
# failed (Issue #158 root cause: "crash / timeout は枯渇保護を通らない"). The
# pool marker is written only after the worker is claimed, so the claim itself
# is unaffected by the exhaustion pause gate.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=8
printf '54 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
rm -f "$state_root/stop.requested"
rm -rf "$state_root/pools"
FAKE_CODEX_SLEEP=60 "$target/bin/agentic-loop" _supervise &
pool_timeout_sup_pid=$!
# Wait until the plan stage's codex call has actually started (logged before
# its sleep, see the fake codex harness) -- not merely until the worker's
# pidfile exists -- so the pool is guaranteed unmarked at candidate-pick time
# on any runner speed. Writing the marker earlier would race worktree/branch
# setup and let the plan stage's OWN (already-correct, Issue #155) pool-pick
# see it first, never actually exercising the crash/timeout path under test.
pool_timeout_call_seen=0
wait_for 'plan stage provider call started' grep -Fq -- '--sandbox read-only' "$FAKE_GH_ROOT/codex-calls" && pool_timeout_call_seen=1
[[ $pool_timeout_call_seen == 1 ]] || { kill "$pool_timeout_sup_pid" 2>/dev/null; wait "$pool_timeout_sup_pid" 2>/dev/null; fail 'the plan stage never reached its (now sleeping) provider call before the pool-exhaustion timeout test'; }
mkdir -p "$state_root/pools/codex"
printf '%s\n' "$(( $(date +%s) + 1800 ))" > "$state_root/pools/codex/exhausted"
printf '%s\n' "$(($(date +%s) - 9))" > "$state_root/workers/54.started"
pool_timeout_requeued=0
wait_for 'pool-exhaustion-correlated hang requeued' grep -Eq '^54 queued open' "$state" && pool_timeout_requeued=1
kill -TERM "$pool_timeout_sup_pid" 2>/dev/null || true
wait "$pool_timeout_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
[[ $pool_timeout_requeued == 1 ]] || fail 'a hang correlated with pool exhaustion was not requeued directly'
if grep -Eq '^54 failed' "$state"; then fail 'a hang correlated with pool exhaustion must not fail the Issue'; fi
# Either enforce_worker_timeout's own kill (agentic-loop:worker-timeout) or, if
# the same poll's recover_expired reclaims the pidfile first (both are called
# out by Issue #158: "crash / timeout は枯渇保護を通らない"), its
# agentic-loop:recovered path may be the one that actually observes the
# correlation first; both record pool-exhaustion=1 and neither burns attempts,
# which is the guarantee under test here -- not which internal path wins.
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'pool-exhaustion=1' 'pool-exhaustion-correlated timeout was not recorded as environment-caused'
[[ ! -e $state_root/attempts/issue-54 ]] || fail 'attempts counter was not cleared for a pool-exhaustion-correlated hang'
rm -rf "$state_root/pools"

# A worker that legitimately completes within the timeout is never flagged or
# killed: a large default must not misfire on ordinary work.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30 WORKER_TIMEOUT_SECONDS=3600
printf '52 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^52 completed closed' "$state" || fail 'a normally completing worker was blocked by the hang-timeout feature'
if grep -Fq 'agentic-loop:worker-timeout' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'a normally completing worker was falsely flagged as hung'; fi

# worker_timeout_seconds=0 disables enforcement entirely, even for a worker
# whose recorded start time is far in the past. This must be observed while
# the supervisor is still running: supervisor_graceful_shutdown
# unconditionally requeues every pidfile-owning Issue on SIGTERM (see the
# graceful shutdown scenario above), so asserting after stopping the
# supervisor would conflate "timeout enforcement" with "shutdown requeue"
# and fail regardless of whether the timeout is actually disabled. The
# worker is started via setsid, mirroring production (see supervise()), so
# its pid is a process group leader and cleanup below can terminate it the
# same way enforce_worker_timeout would if it (incorrectly) fired.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 WORKER_TIMEOUT_SECONDS=0
printf '53 running open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
mkdir -p "$state_root/workers"
FAKE_CODEX_SLEEP=30 setsid "$target/bin/agentic-loop" _worker 53 disabled-timeout-worker &
disabled_worker_pid=$!
disabled_started_seen=0
wait_for 'disabled started seen' test -r "$state_root/workers/53.started" && disabled_started_seen=1
[[ $disabled_started_seen == 1 ]] || { kill -TERM "-$disabled_worker_pid" 2>/dev/null; wait "$disabled_worker_pid" 2>/dev/null; fail 'test setup did not observe the worker start marker'; }
printf '%s\n' "$disabled_worker_pid" > "$state_root/workers/53.pid"
printf '%s\n' "$(($(date +%s) - 100000))" > "$state_root/workers/53.started"
rm -f "$state_root/poll-interval"
"$target/bin/agentic-loop" _supervise &
disabled_sup_pid=$!
disabled_poll_seen=0
wait_for 'disabled-timeout supervisor poll seen' test -r "$state_root/poll-interval" && disabled_poll_seen=1
[[ $disabled_poll_seen == 1 ]] || { kill -TERM "$disabled_sup_pid" 2>/dev/null; wait "$disabled_sup_pid" 2>/dev/null; fail 'disabled-timeout supervisor did not complete a poll'; }
disabled_still_alive=0
kill -0 "$disabled_worker_pid" 2>/dev/null && disabled_still_alive=1
disabled_still_running=0
grep -Eq '^53 running' "$state" && disabled_still_running=1
disabled_no_timeout_comment=1
grep -Fq 'agentic-loop:worker-timeout' "$FAKE_GH_ROOT/$state_key.comments" 2>/dev/null && disabled_no_timeout_comment=0
kill -TERM "$disabled_sup_pid" 2>/dev/null || true
wait "$disabled_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
kill -TERM "-$disabled_worker_pid" 2>/dev/null || true
wait "$disabled_worker_pid" 2>/dev/null || true
rm -f "$state_root/workers/53.pid" "$state_root/workers/53.started"
[[ $disabled_still_alive == 1 ]] || fail 'worker_timeout_seconds=0 did not disable the hang timeout (process was killed)'
[[ $disabled_still_running == 1 ]] || fail 'worker_timeout_seconds=0 did not disable the hang timeout (Issue state changed)'
[[ $disabled_no_timeout_comment == 1 ]] || fail 'worker_timeout_seconds=0 did not disable the hang timeout (worker-timeout comment was posted)'

# doctor classifies an unsafely small worker_timeout_seconds as a warning, not
# a failure, since it only risks killing a still-legitimately-running worker
# rather than making the Supervisor unsafe to run. The Supervisor is left
# stopped at this point in the suite (see below), so doctor's overall exit
# code is not asserted here -- only the classification of this specific check.
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.valid"
write_queue_config "$target/.agentic-loop.toml" WORKER_TIMEOUT_SECONDS=60
doctor_small_timeout=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '[警告] 設定値: WORKER_TIMEOUT_SECONDS' <<< "$doctor_small_timeout" || fail 'doctor did not warn about an unsafely small worker_timeout_seconds'
if grep -Fq '[失敗] 設定値: WORKER_TIMEOUT_SECONDS' <<< "$doctor_small_timeout"; then fail 'an unsafely small worker_timeout_seconds was misclassified as a failure'; fi
mv "$target/.agentic-loop.toml.valid" "$target/.agentic-loop.toml"

# doctor cross-checks the new stage-stall config keys (Issue #273): a band
# inversion against worker_timeout_seconds or against the other stall band is
# a warning (never a failure -- both bands stay independently tunable).
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.valid"
write_queue_config "$target/.agentic-loop.toml" WORKER_TIMEOUT_SECONDS=1800 PROVIDER_STALL_SECONDS=1800
doctor_band_timeout=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '[警告] 設定値: PROVIDER_STALL_SECONDS' <<< "$doctor_band_timeout" || fail 'doctor did not warn when provider_stall_seconds reaches worker_timeout_seconds'
mv "$target/.agentic-loop.toml.valid" "$target/.agentic-loop.toml"

cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.valid"
write_queue_config "$target/.agentic-loop.toml" STALL_SECONDS=1000 PROVIDER_STALL_SECONDS=500
doctor_band_inverted=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '[警告] 設定値: PROVIDER_STALL_SECONDS' <<< "$doctor_band_inverted" || fail 'doctor did not warn when provider_stall_seconds is smaller than stall_seconds'
mv "$target/.agentic-loop.toml.valid" "$target/.agentic-loop.toml"

# --- time constant invariants (Issue #280, ADR 0032) ------------------------
# timing_invariant_violations is the single source of truth for both the two
# doctor warnings above and scripts/lint.sh's hard `_timing-check --committed`
# gate; exercise the CLI boundary directly (not just doctor's warning text).
timing_pristine="$TEST_ROOT/agentic-loop.toml.timing-pristine"
# $target/.agentic-loop.toml has accumulated write_queue_config overrides from
# earlier sections in this suite (several intentionally left unrestored, e.g.
# the worker_timeout_seconds=0 disabled-timeout scenario above), so it is not
# the genuine installed default at this point -- reconstruct it from the
# source repository's own .agentic-loop.toml instead.
cp "$PROJECT_ROOT/.agentic-loop.toml" "$timing_pristine"
cp "$timing_pristine" "$target/.agentic-loop.toml"
"$target/bin/agentic-loop" _timing-check --committed || fail '_timing-check --committed rejected the installed default configuration'

assert_timing_violation() {
  local expect_name=$1; shift
  cp "$timing_pristine" "$target/.agentic-loop.toml"
  write_queue_config "$target/.agentic-loop.toml" "$@"
  local out rc
  if out=$("$target/bin/agentic-loop" _timing-check --committed 2>&1); then rc=0; else rc=$?; fi
  [[ $rc -eq 1 ]] || fail "_timing-check --committed did not fail for $expect_name (rc=$rc): $out"
  grep -Fq "[違反] $expect_name" <<< "$out" || fail "_timing-check --committed output was missing $expect_name: $out"
}

assert_timing_violation stall-before-timeout STALL_SECONDS=20000 WORKER_TIMEOUT_SECONDS=100
assert_timing_violation provider-stall-before-timeout PROVIDER_STALL_SECONDS=20000 WORKER_TIMEOUT_SECONDS=100
assert_timing_violation stall-band-order STALL_SECONDS=1000 PROVIDER_STALL_SECONDS=500
assert_timing_violation worker-timeout-too-low WORKER_TIMEOUT_SECONDS=60
assert_timing_violation heartbeat-lease-margin LEASE_SECONDS=2
assert_timing_violation pause-grace-before-timeout PAUSE_GRACE_SECONDS=20000 WORKER_TIMEOUT_SECONDS=100
assert_timing_violation poll-backoff-order POLL_SECONDS=200 POLL_MAX_SECONDS=100
assert_timing_violation orphan-grace-before-timeout WORKER_ORPHAN_GRACE_SECONDS=20000 WORKER_TIMEOUT_SECONDS=100

# EXHAUSTION_PAUSE_SECONDS/EXHAUSTION_BACKOFF_MAX_SECONDS are bin/agentic-loop
# constants, not .agentic-loop.toml keys, so this relation cannot be driven
# through write_queue_config -- call timing_invariant_violations directly.
exhaustion_violation=$(EXHAUSTION_PAUSE_SECONDS=99999 EXHAUSTION_BACKOFF_MAX_SECONDS=100 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/config.sh"; timing_invariant_violations 300 3600 14400 300 120 30 120 120' "$PROJECT_ROOT")
grep -Fq 'exhaustion-backoff-order' <<< "$exhaustion_violation" || fail 'timing_invariant_violations did not detect an inverted exhaustion backoff order'
exhaustion_ok=$(EXHAUSTION_PAUSE_SECONDS=1800 EXHAUSTION_BACKOFF_MAX_SECONDS=21600 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/config.sh"; timing_invariant_violations 300 3600 14400 300 120 30 120 120' "$PROJECT_ROOT")
[[ -z $exhaustion_ok ]] || fail 'timing_invariant_violations reported a violation for the healthy exhaustion backoff order'

# bash-fallback-vs-toml drift (acceptance criterion 3): editing only the
# committed toml value, leaving bin/agentic-loop's own fallback constant
# behind, must also fail the gate -- this is a distinct failure mode from any
# single-source invariant above.
cp "$timing_pristine" "$target/.agentic-loop.toml"
sed -i 's/^provider_stall_seconds = 3600$/provider_stall_seconds = 100/' "$target/.agentic-loop.toml"
if mismatch_out=$("$target/bin/agentic-loop" _timing-check --committed 2>&1); then mismatch_rc=0; else mismatch_rc=$?; fi
[[ $mismatch_rc -eq 1 ]] || fail '_timing-check --committed did not fail when the toml diverges from the bin/agentic-loop fallback default'
grep -Fq 'fallback-mismatch:PROVIDER_STALL_SECONDS' <<< "$mismatch_out" || fail '_timing-check --committed did not report the fallback/toml mismatch by name'

cp "$timing_pristine" "$target/.agentic-loop.toml"
"$target/bin/agentic-loop" _timing-check --committed || fail 'restoring the pristine config did not clear _timing-check violations'

# --- stall閾値の実測整合 (Issue #280, ADR 0032) -----------------------------
# doctor cross-checks the currently applied stall threshold against real
# stage-duration samples reconstructed from events.log (host-local, zero
# GitHub calls) -- not a fabricated progress epoch.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
: > "$state_root/events.log"
calib_t=1700000000
for calib_i in $(seq 0 4); do
  printf '%s\t9991\tprogress\tplan\n' "$((calib_t + calib_i * 100))" >> "$state_root/events.log"
done
doctor_calib_sparse=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '[成功] stall閾値の実測整合（provider stage）' <<< "$doctor_calib_sparse" || fail 'doctor treated fewer than CALIBRATION_MIN_SAMPLES provider samples as calibrated'
grep -Fq 'sampleが20件未満' <<< "$doctor_calib_sparse" || fail 'doctor did not explain the insufficient-sample verdict'

: > "$state_root/events.log"
for calib_i in $(seq 0 21); do
  printf '%s\t9992\tprogress\tplan\n' "$((calib_t + calib_i * 5000))" >> "$state_root/events.log"
done
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30 STALL_SECONDS=100 PROVIDER_STALL_SECONDS=100
doctor_calib_under=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '[警告] stall閾値の実測整合（provider stage）' <<< "$doctor_calib_under" || fail 'doctor did not warn when the real p90 exceeds provider_stall_seconds'
grep -Eq 'p90=5000s' <<< "$doctor_calib_under" || fail 'doctor calibration warning did not include the observed p90'

write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30 STALL_SECONDS=6000 PROVIDER_STALL_SECONDS=6000
doctor_calib_ok=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '[成功] stall閾値の実測整合（provider stage）' <<< "$doctor_calib_ok" || fail 'doctor did not report calibration success once the threshold exceeds the observed p90'

: > "$state_root/events.log"
cp "$timing_pristine" "$target/.agentic-loop.toml"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# --- real (not fabricated-epoch) stall transition (Issue #280 acceptance
# criterion 8) -- a live worker whose fake provider CLI actually sleeps past
# provider_stall_seconds must be observed as stalled through its own real
# progress markers; raising the threshold above the sleep keeps it healthy.
rm -rf "$state_root/workers"
printf '921 queued open none 2026-01-01T00:00:00Z none feature\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30 STALL_SECONDS=1 PROVIDER_STALL_SECONDS=1
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=2 "$target/bin/agentic-loop" _supervise &
real_stall_sup_pid=$!
real_stall_seen=0
__real_stall_check() {
  real_status_json=$("$target/bin/agentic-loop" status --format json 2>/dev/null) || true
  if [[ $(yq -p json '.workers[] | select(.issue == 921) | .health' <<< "$real_status_json" 2>/dev/null) == stalled ]]; then
    real_stall_seen=1
    return 0
  fi
  kill -0 "$real_stall_sup_pid" 2>/dev/null || return 0
  return 1
}
wait_for 'real fake-provider worker stalled, or supervisor exited' __real_stall_check
wait "$real_stall_sup_pid" 2>/dev/null || true
[[ $real_stall_seen == 1 ]] || fail 'a real fake-provider worker exceeding provider_stall_seconds was not observed as stalled without a fabricated progress epoch'

rm -rf "$state_root/workers"
printf '922 queued open none 2026-01-01T00:00:00Z none feature\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30 STALL_SECONDS=1 PROVIDER_STALL_SECONDS=60
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=2 "$target/bin/agentic-loop" _supervise &
ctrl_stall_sup_pid=$!
ctrl_stall_seen=0
__ctrl_stall_check() {
  ctrl_status_json=$("$target/bin/agentic-loop" status --format json 2>/dev/null) || true
  if [[ $(yq -p json '.workers[] | select(.issue == 922) | .health' <<< "$ctrl_status_json" 2>/dev/null) == stalled ]]; then
    ctrl_stall_seen=1
    return 0
  fi
  kill -0 "$ctrl_stall_sup_pid" 2>/dev/null || return 0
  return 1
}
wait_for 'control worker never observed as stalled, or supervisor exited' __ctrl_stall_check
wait "$ctrl_stall_sup_pid" 2>/dev/null || true
[[ $ctrl_stall_seen == 0 ]] || fail 'raising provider_stall_seconds above the fake providers sleep time still misclassified a real worker as stalled'

rm -rf "$state_root/workers"
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
cp "$timing_pristine" "$target/.agentic-loop.toml"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# --- worker-orphan grace-based reap (Issue #193, ADR 0029) ---
# A worker-orphan (this host's live pidfile whose Issue GitHub no longer
# reports as agent:running, e.g. reverted to queued while the local provider
# CLI process kept running -- the real Issue #132 scenario) falls into a gap:
# recover_expired only touches agent:running Issues, and enforce_worker_timeout
# only fires after worker_timeout_seconds (set enormous below to prove this
# path is independent of it). reap_orphan_workers must instead kill it once
# the Label mismatch has persisted for worker_orphan_grace_seconds, without
# ever killing on the very first observation (that would misfire on the brief
# window between a normal completion's Label update and process exit).
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=999999 WORKER_ORPHAN_GRACE_SECONDS=5
printf '70 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
FAKE_CODEX_SLEEP=60 "$target/bin/agentic-loop" _supervise &
orphan_sup_pid=$!
orphan_worker_pid=''
__orphan_worker_pid_check() { [[ -r $state_root/workers/70.pid ]] && orphan_worker_pid=$(cat "$state_root/workers/70.pid"); }
wait_for 'worker-orphan test worker claimed' __orphan_worker_pid_check
[[ -n $orphan_worker_pid ]] || { kill "$orphan_sup_pid" 2>/dev/null; wait "$orphan_sup_pid" 2>/dev/null; fail 'worker-orphan test: the worker was not claimed before the Label was reverted'; }
grep -Eq '^70 running' "$state" || { kill "$orphan_sup_pid" 2>/dev/null; wait "$orphan_sup_pid" 2>/dev/null; fail 'worker-orphan test: claim did not set agent:running before the Label was reverted'; }
# worker() re-confirms the running Label exactly once, at its very start
# (worker_confirm_running_label), before doing anything else (including the
# fake provider CLI, which sleeps FAKE_CODEX_SLEEP seconds on every stage, so
# waiting for a specific later stage would block past this test's budget).
# Flipping the Label before that one-time check has run would make the
# worker see its own claim as already lost and exit silently and immediately
# -- a false "reap", never reaching reap_orphan_workers at all. Wait for the
# first progress marker (written right after that check passes) so the
# still-live process under test is genuinely the one reap_orphan_workers must
# catch, matching the real Issue #132 race where the Label diverges from an
# already-running provider CLI process.
orphan_claimed_seen=0
wait_for 'worker-orphan test running-Label check passed' test -r "$state_root/workers/70.progress" && orphan_claimed_seen=1
[[ $orphan_claimed_seen == 1 ]] || { kill "$orphan_sup_pid" 2>/dev/null; wait "$orphan_sup_pid" 2>/dev/null; fail 'worker-orphan test: the worker never passed its startup running-Label check before the Label was reverted'; }
# Simulate the Issue #132 scenario: the Label diverges from the still-live
# local worker (e.g. reverted to failed through another path -- failed rather
# than queued so claim_next's MAX_WORKERS=1 slot, freed the instant this
# worker is reaped, can never reclaim Issue 70 out from under this scenario:
# retry_failed only re-queues a failed Issue after RETRY_COOLDOWN_SECONDS
# (600s here), far past this test's budget).
sed -i 's/^70 running/70 failed/' "$state"
orphan_since_seen=0
wait_for 'worker-orphan Label mismatch observed' test -r "$state_root/workers/70.orphan-since" && orphan_since_seen=1
[[ $orphan_since_seen == 1 ]] || { kill "$orphan_sup_pid" 2>/dev/null; wait "$orphan_sup_pid" 2>/dev/null; fail 'worker-orphan test: the Label mismatch was never observed (orphan-since marker missing)'; }
kill -0 "$orphan_worker_pid" 2>/dev/null || { kill "$orphan_sup_pid" 2>/dev/null; wait "$orphan_sup_pid" 2>/dev/null; fail 'worker-orphan test: the worker was killed on the very first observation instead of waiting out the grace period'; }
# Drive the grace boundary through its persisted clock input instead of
# waiting worker_orphan_grace_seconds wall-clock seconds (mirrors the
# per-worker hang timeout test's use of workers/<issue>.started above).
printf '%s\n' "$(($(date +%s) - 6))" > "$state_root/workers/70.orphan-since"
# kill -0 also succeeds against a not-yet-reaped zombie, so poll briefly
# instead of asserting on a single sample right after the state flip. The
# original pid dying (not "the pidfile is now empty") is the right completion
# signal.
orphan_reaped=0
wait_gone 'worker-orphan reaped past grace period' "$orphan_worker_pid" && orphan_reaped=1
[[ $orphan_reaped == 1 ]] || { kill "$orphan_sup_pid" 2>/dev/null; wait "$orphan_sup_pid" 2>/dev/null; fail 'a worker-orphan persisting past worker_orphan_grace_seconds was not reaped'; }
orphan_pidfile_cleared=0
__orphan_pidfile_gone() { [[ ! -e $state_root/workers/70.pid ]]; }
wait_for 'reaped pidfile removed' __orphan_pidfile_gone && orphan_pidfile_cleared=1
[[ $orphan_pidfile_cleared == 1 ]] || fail 'clear_worker_local did not remove the reaped pidfile'
[[ ! -e $state_root/workers/70.orphan-since ]] || fail 'clear_worker_local did not remove the orphan-since marker'
[[ ! -e $state_root/workers/70.lease ]] || fail 'clear_worker_local did not remove the lease file'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:worker-orphan-reaped' 'the worker-orphan reap was not audited on the Issue'
kill -TERM "$orphan_sup_pid" 2>/dev/null || true
wait "$orphan_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"

# A worker-orphan observation that resolves before the grace period elapses
# (e.g. the Label mismatch was transient, mirroring the brief window a normal
# completion passes through between its own Label update and process exit)
# must not be killed, and its grace marker must be cleared rather than
# lingering to poison a later, unrelated mismatch. Driving this through a
# live, continuously-polling Supervisor (as the grace-exceeded scenario above
# does) raced real wall-clock time between the Label flipping away and back
# against a shared, possibly contended CI runner where a single poll -- and
# even just detecting the mismatch -- can stretch far past POLL_SECONDS=1
# (observed in CI repeatedly, at every grace value and wait budget tried).
# This scenario instead uses `_reap-orphans`, a single synchronous
# reap_orphan_workers pass with no polling loop, to drive the two observations
# deterministically: the background Supervisor below only claims the Issue and
# is then killed (SIGKILL, not TERM, so its graceful-shutdown drain never
# fires and the still-running worker is left untouched), and every state
# transition after that is followed immediately by exactly one `_reap-orphans`
# call and a direct assertion, with no real-time race of any kind.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=999999 WORKER_ORPHAN_GRACE_SECONDS=30
printf '72 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
FAKE_CODEX_SLEEP=60 "$target/bin/agentic-loop" _supervise &
orphan2_sup_pid=$!
orphan2_worker_pid=''
__orphan2_worker_pid_check() { [[ -r $state_root/workers/72.pid ]] && orphan2_worker_pid=$(cat "$state_root/workers/72.pid"); }
wait_for 'worker-orphan grace-reset test worker claimed' __orphan2_worker_pid_check
[[ -n $orphan2_worker_pid ]] || { kill "$orphan2_sup_pid" 2>/dev/null; wait "$orphan2_sup_pid" 2>/dev/null; fail 'worker-orphan grace-reset test: the worker was not claimed'; }
# See the matching wait above: the Label must not flip until the worker's
# one-time startup confirm-running-label check has already passed, or the
# worker exits on its own (silently, no reap) instead of exercising the
# grace-reset path under test.
orphan2_claimed_seen=0
wait_for 'worker-orphan grace-reset test running-Label check passed' test -r "$state_root/workers/72.progress" && orphan2_claimed_seen=1
[[ $orphan2_claimed_seen == 1 ]] || { kill "$orphan2_sup_pid" 2>/dev/null; wait "$orphan2_sup_pid" 2>/dev/null; fail 'worker-orphan grace-reset test: the worker never passed its startup running-Label check before the Label was reverted'; }
# The claim is done; hand control of the reap cadence over to _reap-orphans
# below (SIGKILL so supervisor_graceful_shutdown never runs against the
# still-running worker).
kill -KILL "$orphan2_sup_pid" 2>/dev/null || true
wait "$orphan2_sup_pid" 2>/dev/null || true
# The mismatch is simulated as agent:failed, not agent:queued: the still-live
# worker's own heartbeat loop (see worker_reassert_running, Issue #208's
# self-heal for a foreign supervisor's queued-revert) would otherwise silently
# flip an exactly-agent:queued Issue back to agent:running on its own ~20s
# tick, racing this deterministic reap sequence. worker_reassert_running is
# restricted to that exact queued signature, so agent:failed is untouched by
# it while still being (correctly) absent from the running-Issue list reap_
# orphan_workers checks against.
sed -i 's/^72 running/72 failed/' "$state"
"$target/bin/agentic-loop" _reap-orphans
[[ -r $state_root/workers/72.orphan-since ]] || fail 'worker-orphan grace-reset test: the Label mismatch was not observed on the first reap pass'
kill -0 "$orphan2_worker_pid" 2>/dev/null || fail 'worker-orphan grace-reset test: the worker was killed on the very first observation instead of waiting out the grace period'
# The mismatch resolves (Label restored) before worker_orphan_grace_seconds
# has elapsed; the very next reap pass must clear the marker without killing
# anything (reap_orphan_workers matches the running-Issue list before ever
# consulting elapsed/grace).
sed -i 's/^72 failed/72 running/' "$state"
"$target/bin/agentic-loop" _reap-orphans
[[ ! -e $state_root/workers/72.orphan-since ]] || fail 'worker-orphan grace-reset test: the orphan-since marker was not cleared once the Label mismatch resolved'
kill -0 "$orphan2_worker_pid" 2>/dev/null || fail 'worker-orphan grace-reset test: a transient Label mismatch killed the worker before its grace period elapsed'
if grep -Fq 'agentic-loop:worker-orphan-reaped' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'worker-orphan grace-reset test: a transient Label mismatch was falsely reaped'; fi
kill -TERM "-$orphan2_worker_pid" 2>/dev/null || true
wait "$orphan2_worker_pid" 2>/dev/null || true
rm -f "$state_root/workers/72.pid" "$state_root/workers/72.started" "$state_root/workers/72.progress" "$state_root/workers/72.orphan-since"
rm -f "$state_root/stop.requested"

# --- a worker whose pidfile records a non-leader pid is still stopped (Issue #292) ---
# Every worker-stop path signals a process *group* so the provider CLI child
# goes down with the worker. The pidfile's pid used to be passed straight to
# `kill -TERM "-$pid"`, which assumes it is its own group leader. That holds for
# a freshly claimed worker (claim spawns it via setsid) but not after
# reconcile_worker_pidfiles repairs a lost pidfile from the live /proc scan
# (Issue #219): it legitimately records a *descendant* of the leader, and
# `-$pid` then names a nonexistent group, fails with ESRCH, and is swallowed by
# the call site's `|| true`. The worker survived every reap and held its
# max_workers slot forever (real incident: #251 was "reaped" once per poll for
# 8h50m while 46 queued Issues were never claimed).
#
# The worker under test is a dedicated session (setsid: leader + child, exactly
# the shape claim produces) rather than a real claimed worker, and the pidfile
# records the *child*. A real worker also spawns short-lived helpers whose pids
# come and go, so picking one of those as "the non-leader pid" would race the
# reap's own worker_pid_live check; this fixes the process shape the reap must
# handle without depending on provider-CLI timing at all.
pgid_of() {
  local pid=$1 stat_line rest
  local -a fields
  [[ -r /proc/$pid/stat ]] || return 1
  read -r stat_line < "/proc/$pid/stat" || return 1
  rest=${stat_line##*') '}
  read -r -a fields <<< "$rest"
  [[ ${fields[2]:-} =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "${fields[2]}"
}
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=999999 WORKER_ORPHAN_GRACE_SECONDS=30
# agent:failed, not agent:queued: worker_reassert_running would otherwise
# self-heal an exactly-agent:queued Issue back to running on its own tick and
# race this deterministic reap (see the grace-reset scenario above).
printf '74 failed open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
rm -rf "$state_root/workers"; mkdir -p "$state_root/workers"
nonleader_dir="$state_root/nonleader"; rm -rf "$nonleader_dir"; mkdir -p "$nonleader_dir"
setsid bash -c '
  sleep 120 & printf "%s\n" "$!" > "$1/child"
  printf "%s\n" "$$" > "$1/leader"
  wait
' bash "$nonleader_dir" &
nonleader_session_pid=$!
nonleader_ready=0
__nonleader_ready_check() { [[ -r $nonleader_dir/child && -r $nonleader_dir/leader ]]; }
wait_for 'non-leader pidfile test worker session started' __nonleader_ready_check && nonleader_ready=1
[[ $nonleader_ready == 1 ]] || { kill -KILL "$nonleader_session_pid" 2>/dev/null; fail 'non-leader pidfile test: the worker session did not start'; }
nonleader_leader=$(cat "$nonleader_dir/leader")
nonleader_child=$(cat "$nonleader_dir/child")
[[ $(pgid_of "$nonleader_child") == "$nonleader_leader" ]] || { kill -KILL "-$nonleader_leader" 2>/dev/null; fail 'non-leader pidfile test: the child is not in the leader'"'"'s process group'; }
[[ $(pgid_of "$nonleader_child") != "$nonleader_child" ]] || { kill -KILL "-$nonleader_leader" 2>/dev/null; fail 'non-leader pidfile test: the recorded pid is its own group leader, so the regression would not be exercised'; }
printf '%s\n' "$nonleader_child" > "$state_root/workers/74.pid"
printf '%s\n' "$(($(date +%s) - 600))" > "$state_root/workers/74.started"
"$target/bin/agentic-loop" _reap-orphans
[[ -r $state_root/workers/74.orphan-since ]] || { kill -KILL "-$nonleader_leader" 2>/dev/null; fail 'non-leader pidfile test: the Label mismatch was not observed'; }
kill -0 "$nonleader_child" 2>/dev/null || { kill -KILL "-$nonleader_leader" 2>/dev/null; fail 'non-leader pidfile test: the worker was killed on the very first observation instead of waiting out the grace period'; }
printf '%s\n' "$(($(date +%s) - 31))" > "$state_root/workers/74.orphan-since"
"$target/bin/agentic-loop" _reap-orphans
nonleader_child_gone=0
wait_gone 'nonleader child gone' "$nonleader_child" && nonleader_child_gone=1
if [[ $nonleader_child_gone != 1 ]]; then kill -KILL "-$nonleader_leader" 2>/dev/null || true; fail 'a worker whose pidfile recorded a non-leader pid survived the orphan reap (its process group was never signalled)'; fi
nonleader_leader_gone=0
wait_gone 'nonleader leader gone' "$nonleader_leader" && nonleader_leader_gone=1
if [[ $nonleader_leader_gone != 1 ]]; then kill -KILL "-$nonleader_leader" 2>/dev/null || true; fail 'the reap signalled only the recorded pid, leaving the rest of the worker'"'"'s process group (its provider CLI) running'; fi
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:worker-orphan-reaped' 'the non-leader-pid reap was not audited on the Issue'
[[ ! -e $state_root/workers/74.pid ]] || fail 'clear_worker_local did not remove the reaped pidfile (non-leader pid)'
wait "$nonleader_session_pid" 2>/dev/null || true
rm -rf "$nonleader_dir"
rm -rf "$state_root/workers"; mkdir -p "$state_root/workers"
rm -f "$state_root/stop.requested"

# --- the reap never signals its own process group (Issue #292) ---
# Resolving the real group means a worker that was never setsid'd into its own
# group (its pidfile pid shares the supervisor's group -- the observed
# consequence of a relative-path start, Issue #219 root cause 2) would take the
# supervisor down with it if the group were signalled blindly. signal_process_tree
# must fall back to signalling the single pid whenever the resolved group is its
# own. The reap therefore runs inside a dedicated session (setsid) that also
# holds two sleepers: the one recorded in the pidfile must die, the witness --
# same group, not the pidfile's pid -- must survive.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=999999 WORKER_ORPHAN_GRACE_SECONDS=30
printf '76 failed open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
rm -rf "$state_root/workers"; mkdir -p "$state_root/workers"
selfgroup_dir="$state_root/selfgroup"; rm -rf "$selfgroup_dir"; mkdir -p "$selfgroup_dir"
setsid bash -c '
  sleep 120 & printf "%s\n" "$!" > "$1/target"
  sleep 120 & printf "%s\n" "$!" > "$1/witness"
  while [[ ! -e $1/go ]]; do sleep 0.1; done
  "$2" _reap-orphans
  printf "done\n" > "$1/reaped"
' bash "$selfgroup_dir" "$target/bin/agentic-loop" &
selfgroup_sid_pid=$!
selfgroup_ready=0
__selfgroup_ready_check() { [[ -r $selfgroup_dir/target && -r $selfgroup_dir/witness ]]; }
wait_for 'self-group guard test sleepers started' __selfgroup_ready_check && selfgroup_ready=1
[[ $selfgroup_ready == 1 ]] || { kill -KILL "$selfgroup_sid_pid" 2>/dev/null; fail 'self-group guard test: the dedicated session did not start its sleepers'; }
selfgroup_target=$(cat "$selfgroup_dir/target")
selfgroup_witness=$(cat "$selfgroup_dir/witness")
printf '%s\n' "$selfgroup_target" > "$state_root/workers/76.pid"
printf '%s\n' "$(($(date +%s) - 600))" > "$state_root/workers/76.started"
printf '%s\n' "$(($(date +%s) - 31))" > "$state_root/workers/76.orphan-since"
: > "$selfgroup_dir/go"
selfgroup_done=0
wait_for 'selfgroup done' test -r "$selfgroup_dir/reaped" && selfgroup_done=1
[[ $selfgroup_done == 1 ]] || { kill -KILL "-$selfgroup_sid_pid" 2>/dev/null; fail 'self-group guard test: the reap inside the dedicated session never completed (it signalled its own group)'; }
selfgroup_target_gone=0
wait_gone 'selfgroup target gone' "$selfgroup_target" && selfgroup_target_gone=1
[[ $selfgroup_target_gone == 1 ]] || { kill -KILL "-$selfgroup_sid_pid" 2>/dev/null; fail 'a worker sharing the reaper'"'"'s process group was not stopped at all (the single-pid fallback did not fire)'; }
kill -0 "$selfgroup_witness" 2>/dev/null || fail 'the reap signalled its own process group, killing a process it does not own'
kill -KILL "-$selfgroup_sid_pid" 2>/dev/null || true
wait "$selfgroup_sid_pid" 2>/dev/null || true
rm -rf "$selfgroup_dir"
rm -rf "$state_root/workers"; mkdir -p "$state_root/workers"
rm -f "$state_root/stop.requested"

# --- worker-orphan: other-host Issues are structurally untouched, and the
# running-Issue list is fetched at most once per poll (Issue #193 hardening) ---
# reap_orphan_workers iterates this host's own workers/*.pid glob, never an
# Issue's state row, so an Issue GitHub reports as agent:running (another
# host's genuinely running worker) or as some other non-running state
# (another host's queued/needs-input Issue) must never grow an orphan-since
# marker or an audit comment when this host has no pidfile for it. The
# running-Issue list itself (running_issue_numbers, worker_state.sh) is now a
# poll-scoped memo shared with recover_expired (see docs/decisions/
# 0029-worker-orphan-reap.md and project.sh's clear_supervisor_snapshot): one
# _supervise pass must fetch it at most once (the unavoidable startup call
# made before the first snapshot exists), never twice for recover_expired and
# reap_orphan_workers separately.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=999999 WORKER_ORPHAN_GRACE_SECONDS=5
now=$(date +%s)
printf '83 running open none 2026-01-01T00:00:00Z\n84 needs-input open none 2026-01-01T00:00:00Z\n' > "$state"
# A valid, unexpired lease comment makes Issue 83 look like another host's
# genuinely still-running worker to recover_expired (see ADR 0003); without
# one, recover_expired would (correctly, but irrelevantly to this scenario)
# reclaim it as an abandoned remote claim before reap_orphan_workers ever
# gets a chance to run against it.
printf '83 <!-- agentic-loop:lease worker=other-host heartbeat=%s expires=%s -->\n' "$now" "$((now + 300))" > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
rm -rf "$state_root/workers"
mkdir -p "$state_root/workers"
otherhost_calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^83 running' "$state" || fail 'another host'"'"'s genuinely running Issue (valid lease, no local pidfile) was reclaimed by recover_expired'
[[ ! -e $state_root/workers/83.orphan-since ]] || fail 'a local pidfile-less running Issue on another host grew an orphan-since marker'
[[ ! -e $state_root/workers/84.orphan-since ]] || fail 'a local pidfile-less non-running Issue on another host grew an orphan-since marker'
if grep -Fq 'agentic-loop:worker-orphan-reaped' "$FAKE_GH_ROOT/$state_key.comments"; then fail 'a pidfile-less Issue on another host was audited as a worker-orphan reap'; fi
# running_issue_numbers (worker_state.sh) is the memo shared by recover_expired
# and reap_orphan_workers specifically; anchor on its distinctive jq tail
# (bare `.number`, no tsv/body columns) so rebuild_scope_cache's unrelated
# `agent:running` list call (a different jq shape, kept separate on purpose)
# is not miscounted as a second fetch of the same list.
otherhost_running_list_calls=$(tail -n "+$((otherhost_calls_before + 1))" "$FAKE_GH_ROOT/calls" | grep -cE -- '-f labels=agent:running .*\.number$' || true)
(( otherhost_running_list_calls <= 1 )) || { tail -n "+$((otherhost_calls_before + 1))" "$FAKE_GH_ROOT/calls" >&2; fail "the running-Issue list was fetched $otherhost_running_list_calls times in one poll with no local pidfile (expected at most 1)"; }
rm -rf "$state_root/workers"

# --- worker-orphan guard: an active critical section is never reaped ---
# worker.sh's worker_critical_begin/_end bracket the completed-state Label
# write through the Issue close (cleanup_completed_worker) and the equivalent
# resume-completion path. GitHub's Label can already read completed/closed
# there while this host's process is still finishing that write sequence,
# which looks exactly like an orphan to reap_orphan_workers. Killing
# mid-critical-section would interrupt exactly the write sequence the marker
# exists to protect, so the guard must hold even once grace has already
# elapsed, without clearing the orphan-since marker, and release once the
# section ends.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=999999 WORKER_ORPHAN_GRACE_SECONDS=5
printf '85 needs-input open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
rm -rf "$state_root/workers"
mkdir -p "$state_root/workers"
setsid sleep 60 &
critguard_pid=$!
printf '%s\n' "$critguard_pid" > "$state_root/workers/85.pid"
printf '%s\n' "$(($(date +%s) - 300))" > "$state_root/workers/85.started"
printf '%s\n' "$(($(date +%s) - 10))" > "$state_root/workers/85.orphan-since"
: > "$state_root/workers/85.critical"
# _supervise's own RUN_ONCE exit condition waits for worker_count (any live
# workers/*.pid) to reach 0, which this decoy pidfile always satisfies as
# "still running" -- so a guarded (never-killed) decoy would make RUN_ONCE
# block for the full 60s sleep instead of returning once a poll has run.
# Poll a real backgrounded supervisor instead (mirrors the disabled-timeout
# hang-timeout scenario above), driven by the poll-interval marker it writes
# once per completed poll.
rm -f "$state_root/poll-interval"
"$target/bin/agentic-loop" _supervise &
critguard_sup_pid=$!
critguard_poll_seen=0
wait_for 'critguard poll seen' test -r "$state_root/poll-interval" && critguard_poll_seen=1
[[ $critguard_poll_seen == 1 ]] || { kill -TERM "$critguard_sup_pid" 2>/dev/null; wait "$critguard_sup_pid" 2>/dev/null; kill -TERM "-$critguard_pid" 2>/dev/null; fail 'critical-section guard test: the supervisor did not complete a poll'; }
kill -0 "$critguard_pid" 2>/dev/null || { kill -TERM "$critguard_sup_pid" 2>/dev/null; wait "$critguard_sup_pid" 2>/dev/null; fail 'reap_orphan_workers killed a worker with an active critical section despite grace already having elapsed'; }
[[ -e $state_root/workers/85.orphan-since ]] || { kill -TERM "$critguard_sup_pid" 2>/dev/null; wait "$critguard_sup_pid" 2>/dev/null; kill -TERM "-$critguard_pid" 2>/dev/null; fail 'the orphan-since marker was cleared while the critical-section guard was active'; }
if grep -Fq 'agentic-loop:worker-orphan-reaped' "$FAKE_GH_ROOT/$state_key.comments"; then kill -TERM "$critguard_sup_pid" 2>/dev/null; wait "$critguard_sup_pid" 2>/dev/null; kill -TERM "-$critguard_pid" 2>/dev/null; fail 'a worker-orphan reap was audited despite an active critical section'; fi
rm -f "$state_root/workers/85.critical"
critguard_reaped=0
wait_gone 'critguard reaped' "$critguard_pid" && critguard_reaped=1
# The process dying (kill -0 failing) only proves reap_orphan_workers reached
# its kill; clear_worker_local/comment_issue still run afterward in the same
# function body. Give the still-live supervisor a brief window to finish that
# tail before it is terminated, or the audit-comment assertion below races
# against its own cleanup.
if [[ $critguard_reaped == 1 ]]; then
  wait_for 'orphan-reaped comment recorded' --timeout 10 grep -Fq 'agentic-loop:worker-orphan-reaped' "$FAKE_GH_ROOT/$state_key.comments" || true
fi
kill -TERM "$critguard_sup_pid" 2>/dev/null || true
wait "$critguard_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
if [[ $critguard_reaped != 1 ]]; then kill -TERM "-$critguard_pid" 2>/dev/null || true; fail 'reap_orphan_workers never reaped once the critical-section guard cleared'; fi
[[ ! -e $state_root/workers/85.orphan-since ]] || fail 'clear_worker_local did not remove the orphan-since marker after the guarded reap'
[[ $(grep -Fc 'agentic-loop:worker-orphan-reaped' "$FAKE_GH_ROOT/$state_key.comments" 2>/dev/null || true) -eq 1 ]] || fail 'the guarded reap did not audit exactly one comment'
rm -rf "$state_root/workers"

# --- worker-orphan guard: a pending stop-request (pause/abort drain) is
# never reaped ---
# control.sh's control_drain_local_worker (pause/abort) writes workers/
# <issue>.stop-requested before waiting out any critical section and only
# then writing control_checkpoint, the durable record resume depends on to
# restore the pre-pause state. Reaping mid-drain would kill the process
# before that checkpoint is written, exactly like the critical-section guard
# above but for the cooperative-stop path instead of normal completion.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=999999 WORKER_ORPHAN_GRACE_SECONDS=5
printf '86 needs-input open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
rm -rf "$state_root/workers"
mkdir -p "$state_root/workers"
setsid sleep 60 &
stopguard_pid=$!
printf '%s\n' "$stopguard_pid" > "$state_root/workers/86.pid"
printf '%s\n' "$(($(date +%s) - 300))" > "$state_root/workers/86.started"
printf '%s\n' "$(($(date +%s) - 10))" > "$state_root/workers/86.orphan-since"
: > "$state_root/workers/86.stop-requested"
rm -f "$state_root/poll-interval"
"$target/bin/agentic-loop" _supervise &
stopguard_sup_pid=$!
stopguard_poll_seen=0
wait_for 'stopguard poll seen' test -r "$state_root/poll-interval" && stopguard_poll_seen=1
[[ $stopguard_poll_seen == 1 ]] || { kill -TERM "$stopguard_sup_pid" 2>/dev/null; wait "$stopguard_sup_pid" 2>/dev/null; kill -TERM "-$stopguard_pid" 2>/dev/null; fail 'stop-request guard test: the supervisor did not complete a poll'; }
kill -0 "$stopguard_pid" 2>/dev/null || { kill -TERM "$stopguard_sup_pid" 2>/dev/null; wait "$stopguard_sup_pid" 2>/dev/null; fail 'reap_orphan_workers killed a worker with a pending stop-request despite grace already having elapsed'; }
[[ -e $state_root/workers/86.orphan-since ]] || { kill -TERM "$stopguard_sup_pid" 2>/dev/null; wait "$stopguard_sup_pid" 2>/dev/null; kill -TERM "-$stopguard_pid" 2>/dev/null; fail 'the orphan-since marker was cleared while the stop-request guard was active'; }
if grep -Fq 'agentic-loop:worker-orphan-reaped' "$FAKE_GH_ROOT/$state_key.comments"; then kill -TERM "$stopguard_sup_pid" 2>/dev/null; wait "$stopguard_sup_pid" 2>/dev/null; kill -TERM "-$stopguard_pid" 2>/dev/null; fail 'a worker-orphan reap was audited despite a pending stop-request'; fi
rm -f "$state_root/workers/86.stop-requested"
stopguard_reaped=0
wait_gone 'stopguard reaped' "$stopguard_pid" && stopguard_reaped=1
if [[ $stopguard_reaped == 1 ]]; then
  wait_for 'orphan-reaped comment recorded' --timeout 10 grep -Fq 'agentic-loop:worker-orphan-reaped' "$FAKE_GH_ROOT/$state_key.comments" || true
fi
kill -TERM "$stopguard_sup_pid" 2>/dev/null || true
wait "$stopguard_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
if [[ $stopguard_reaped != 1 ]]; then kill -TERM "-$stopguard_pid" 2>/dev/null || true; fail 'reap_orphan_workers never reaped once the stop-request guard cleared'; fi
[[ ! -e $state_root/workers/86.orphan-since ]] || fail 'clear_worker_local did not remove the orphan-since marker after the guarded reap'
rm -rf "$state_root/workers"

# --- worker-orphan guard: a stale orphan-since marker left by a prior worker
# on the same Issue restarts grace instead of firing instantly ---
# workers/<issue>.orphan-since can outlive clear_worker_local when the prior
# worker crashed. If the same Issue is reclaimed immediately, the new
# worker's very first observation would otherwise inherit the old epoch and
# be killed with none of its own grace ever having elapsed. A marker older
# than workers/<issue>.started (this worker's own start) must be treated as
# absent and rewritten, restarting the count from the current worker's
# perspective.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=3 RETRY_COOLDOWN_SECONDS=600 WORKER_TIMEOUT_SECONDS=999999 WORKER_ORPHAN_GRACE_SECONDS=5
printf '87 needs-input open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
rm -rf "$state_root/workers"
mkdir -p "$state_root/workers"
setsid sleep 60 &
stale_pid=$!
printf '%s\n' "$stale_pid" > "$state_root/workers/87.pid"
printf '%s\n' "$(date +%s)" > "$state_root/workers/87.started"
printf '%s\n' "$(($(date +%s) - 100))" > "$state_root/workers/87.orphan-since"
# See the critical-section guard test above: this decoy pidfile always counts
# as a live worker, so RUN_ONCE (which waits for worker_count==0) would block
# for the decoy's full sleep instead of returning after one poll. Poll a
# backgrounded supervisor's poll-interval marker instead.
rm -f "$state_root/poll-interval"
"$target/bin/agentic-loop" _supervise &
stale_sup_pid=$!
stale_poll_seen=0
wait_for 'stale poll seen' test -r "$state_root/poll-interval" && stale_poll_seen=1
[[ $stale_poll_seen == 1 ]] || { kill -TERM "$stale_sup_pid" 2>/dev/null; wait "$stale_sup_pid" 2>/dev/null; kill -TERM "-$stale_pid" 2>/dev/null; fail 'stale-marker test: the supervisor did not complete a poll'; }
kill -0 "$stale_pid" 2>/dev/null || { kill -TERM "$stale_sup_pid" 2>/dev/null; wait "$stale_sup_pid" 2>/dev/null; fail 'a stale orphan-since marker predating the current worker triggered an immediate kill'; }
stale_since_after=''
read -r stale_since_after < "$state_root/workers/87.orphan-since" || true
[[ $stale_since_after =~ ^[0-9]+$ ]] || { kill -TERM "$stale_sup_pid" 2>/dev/null; wait "$stale_sup_pid" 2>/dev/null; kill -TERM "-$stale_pid" 2>/dev/null; fail 'a stale orphan-since marker was not rewritten with a numeric epoch'; }
if (( $(date +%s) - stale_since_after >= 5 )); then kill -TERM "$stale_sup_pid" 2>/dev/null; wait "$stale_sup_pid" 2>/dev/null; kill -TERM "-$stale_pid" 2>/dev/null; fail 'a stale orphan-since marker predating the current worker was not reset to the current time'; fi
if grep -Fq 'agentic-loop:worker-orphan-reaped' "$FAKE_GH_ROOT/$state_key.comments"; then kill -TERM "$stale_sup_pid" 2>/dev/null; wait "$stale_sup_pid" 2>/dev/null; kill -TERM "-$stale_pid" 2>/dev/null; fail 'a stale orphan-since marker was falsely reaped instead of restarting grace'; fi
# Grace is now genuinely counted from the reset marker: backdating it past
# worker_orphan_grace_seconds must reap normally, proving the reset restarts
# (rather than permanently disables) the grace countdown.
printf '%s\n' "$(($(date +%s) - 6))" > "$state_root/workers/87.orphan-since"
stale_reaped=0
wait_gone 'stale reaped' "$stale_pid" && stale_reaped=1
if [[ $stale_reaped == 1 ]]; then
  wait_for 'orphan-reaped comment recorded' --timeout 10 grep -Fq 'agentic-loop:worker-orphan-reaped' "$FAKE_GH_ROOT/$state_key.comments" || true
fi
kill -TERM "$stale_sup_pid" 2>/dev/null || true
wait "$stale_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
if [[ $stale_reaped != 1 ]]; then kill -TERM "-$stale_pid" 2>/dev/null || true; fail 'grace was never recomputed after a stale marker was reset'; fi
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:worker-orphan-reaped' 'the reap following a recomputed grace period was not audited'
rm -rf "$state_root/workers"

# --- status: worker-orphan warning shows the remaining grace, and grace=0
# reports the safety net as disabled ---
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 WORKER_ORPHAN_GRACE_SECONDS=120
printf '90 needs-input open none 2026-01-01T00:00:00Z\n' > "$state"
rm -rf "$state_root/workers"
mkdir -p "$state_root/workers"
printf '%s\n' "$$" > "$state_root/workers/90.pid"
printf '%s\n' "$(($(date +%s) - 30))" > "$state_root/workers/90.orphan-since"
statusgrace_output=$("$target/bin/agentic-loop" status)
grep -Fq '#90' <<< "$statusgrace_output" || fail 'status did not list the worker-orphan Issue'
grep -Fq 'worker-orphan' <<< "$statusgrace_output" || fail 'status did not report a worker-orphan anomaly'
grep -Fq 'grace 120秒' <<< "$statusgrace_output" || fail 'status did not show the configured grace period'
grep -Eq '観測 [0-9]+秒' <<< "$statusgrace_output" || fail 'status did not show the elapsed observation time'
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30 WORKER_ORPHAN_GRACE_SECONDS=0
statusgrace_disabled_output=$("$target/bin/agentic-loop" status)
grep -Fq '自動停止は無効です' <<< "$statusgrace_disabled_output" || fail 'status did not report worker_orphan_grace_seconds=0 as disabling the automatic stop'
rm -rf "$state_root/workers"

# --- Residual worktree/branch auto-prune (Issue #211) ---
# prune_residual_worktrees is exercised through the deterministic
# `_prune-worktrees` boundary (mirrors `_reap-orphans`, see bin/agentic-loop)
# rather than a live poll loop: it never spawns a provider worker, so there is
# nothing for supervise()'s RUN_ONCE worker_count() wait to synchronize on.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=300 STOP_TIMEOUT=10 STALE_DAYS=30
rm -rf "$state_root/workers"
bare="$TEST_ROOT/installed-project.git"

# 1) A closed Issue's residual worktree/branch is discarded even with an
# unpushed commit: the work is done or abandoned, so nothing here is worth
# preserving.
git -C "$target" worktree add --quiet -b agent/issue-9990 "$target-worktrees/issue-9990" origin/main
printf 'unpushed\n' > "$target-worktrees/issue-9990/prune-closed.txt"
git -C "$target-worktrees/issue-9990" add prune-closed.txt
git -C "$target-worktrees/issue-9990" commit --quiet -m 'unpushed work on a closed Issue'
printf '9990 completed closed\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _prune-worktrees
[[ ! -e $target-worktrees/issue-9990 ]] || fail 'prune did not remove a residual worktree left over from a closed Issue'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-9990 && fail 'prune did not remove a residual local branch left over from a closed Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:pruned reason=closed' 'the closed-Issue residual prune was not audited on the Issue'

# 2) An open Issue whose local branch is fully reflected on
# `origin/agent/issue-N` has its local worktree/branch discarded, but the
# remote branch is kept for another host or a future resume (Issue #210).
git -C "$target" worktree add --quiet -b agent/issue-9991 "$target-worktrees/issue-9991" origin/main
printf 'pushed\n' > "$target-worktrees/issue-9991/prune-pushed.txt"
git -C "$target-worktrees/issue-9991" add prune-pushed.txt
git -C "$target-worktrees/issue-9991" commit --quiet -m 'fully pushed work'
git -C "$target-worktrees/issue-9991" push --quiet origin agent/issue-9991
printf '9991 parked open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _prune-worktrees
[[ ! -e $target-worktrees/issue-9991 ]] || fail 'prune did not remove a residual worktree fully reflected on its remote branch'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-9991 && fail 'prune did not remove the residual local branch once fully pushed'
git -C "$bare" show-ref --verify --quiet refs/heads/agent/issue-9991 || fail 'prune deleted the remote branch of a fully pushed open Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:pruned reason=pushed' 'the fully-pushed open-Issue prune was not audited on the Issue'

# 3) An open Issue with an unpushed local commit is left untouched, silently
# (no comment): an operator cannot act on local-only work they cannot observe
# from GitHub anyway.
git -C "$target" worktree add --quiet -b agent/issue-9992 "$target-worktrees/issue-9992" origin/main
git -C "$target-worktrees/issue-9992" push --quiet origin agent/issue-9992
printf 'unpushed\n' > "$target-worktrees/issue-9992/prune-unpushed.txt"
git -C "$target-worktrees/issue-9992" add prune-unpushed.txt
git -C "$target-worktrees/issue-9992" commit --quiet -m 'unpushed follow-up work'
printf '9992 parked open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _prune-worktrees
[[ -e $target-worktrees/issue-9992 ]] || fail 'prune removed a residual worktree that still had an unpushed commit'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-9992 || fail 'prune removed a residual local branch that still had an unpushed commit'
[[ ! -s $FAKE_GH_ROOT/$state_key.comments ]] || fail 'prune commented on an Issue with unpushed local work still present'
git -C "$target" worktree remove --force "$target-worktrees/issue-9992"
git -C "$target" branch -D agent/issue-9992 >/dev/null

# 4) An Issue with a live local worker on this host is never touched, even
# when GitHub reports it closed.
git -C "$target" worktree add --quiet -b agent/issue-9993 "$target-worktrees/issue-9993" origin/main
mkdir -p "$state_root/workers"
printf '%s\n' "$$" > "$state_root/workers/9993.pid"
printf '9993 completed closed\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _prune-worktrees
[[ -e $target-worktrees/issue-9993 ]] || fail 'prune removed a worktree despite a live local worker pidfile'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-9993 || fail 'prune removed a branch despite a live local worker pidfile'
[[ ! -s $FAKE_GH_ROOT/$state_key.comments ]] || fail 'prune commented on an Issue guarded by a live local worker'
rm -f "$state_root/workers/9993.pid"
git -C "$target" worktree remove --force "$target-worktrees/issue-9993"
git -C "$target" branch -D agent/issue-9993 >/dev/null

# 5) A residual branch with no matching worktree (e.g. left by a prior,
# interrupted prune, or removed by hand) is pruned the same way once its
# Issue closes.
git -C "$target" branch --quiet agent/issue-9994 origin/main
printf '9994 completed closed\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _prune-worktrees
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-9994 && fail 'prune did not remove a branch-only residual once its Issue closed'

: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"

# --- Requirement traceability gate (Issue #53, ADR 0017) ---
# These scenarios exercise trace_gate through the ordinary (non-resume)
# completion path: a fresh "running" Issue with no worktree yet, so worker()
# creates the worktree/branch, runs the (default-completing) fake provider,
# and then discovers its own merged PR via the `pulls?state=closed` fixture
# (FAKE_PR_MERGED defaults to 1), exactly as production does after a real
# provider opens and merges a PR. This is far simpler than staging a resume,
# and reuses the exact same trace_gate call site.

# 1) Multiple acceptance criteria: satisfied + not-applicable all covered.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_id1=$(criterion_id 'API endpoint responds within 200ms')
trace_id2=$(criterion_id 'Structured logs are emitted for each request')
trace_id3=$(criterion_id 'Existing clients remain backward compatible')
trace_c1=$(trace_criterion_json "$trace_id1" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-a.sh)" "$(trace_check_json check success)")
trace_c2=$(trace_criterion_json "$trace_id2" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-b.sh)" "$(trace_check_json check success)")
trace_c3=$(trace_criterion_json "$trace_id3" issue-body not-applicable '' 'No client-facing change in this Issue' '' '' '')
trace_record=$(trace_record_json 7001 "$trace_c1" "$trace_c2" "$trace_c3")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '7001 running open none 2026-01-01T00:00:00Z none none %s\n' \
  "$(criteria_body 'API endpoint responds within 200ms' 'Structured logs are emitted for each request' 'Existing clients remain backward compatible')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES=$'bin/trace-a.sh\nbin/trace-b.sh' \
  "$target/bin/agentic-loop" _worker 7001 trace-multi-criteria-worker
grep -Eq '^7001 completed closed' "$state" || fail 'a fully-covered multi-criterion traceability record did not complete the Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'criteria=3 satisfied=2 partial=0 not-applicable=1' 'the traceability verdict did not tally satisfied/not-applicable criteria correctly'

# 2) Multiple changed paths across multiple criteria, all present in the PR's
# own file list: pass, with every changed path attributed to a criterion.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_id1=$(criterion_id 'Feature A endpoint is implemented')
trace_id2=$(criterion_id 'Feature B endpoint is implemented')
trace_c1=$(trace_criterion_json "$trace_id1" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-a.sh),$(trace_change_json bin/trace-b.sh)" "$(trace_check_json check success)")
trace_c2=$(trace_criterion_json "$trace_id2" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-c.sh)" "$(trace_check_json check success)")
trace_record=$(trace_record_json 7002 "$trace_c1" "$trace_c2")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '7002 running open none 2026-01-01T00:00:00Z none none %s\n' \
  "$(criteria_body 'Feature A endpoint is implemented' 'Feature B endpoint is implemented')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES=$'bin/trace-a.sh\nbin/trace-b.sh\nbin/trace-c.sh' \
  "$target/bin/agentic-loop" _worker 7002 trace-multi-path-worker
grep -Eq '^7002 completed closed' "$state" || fail 'a multi-path traceability record covering every changed file did not complete the Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'unreferenced_paths=0' 'a fully-attributed multi-path traceability record reported unreferenced paths'

# 3) A changed path the PR's own file list does not contain is never trusted
# from the record: evidence-mismatch, and in require mode the Issue is left
# open/failed with its worktree and branch intact for investigation.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_id1=$(criterion_id 'Feature C endpoint is implemented')
trace_c1=$(trace_criterion_json "$trace_id1" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-a.sh),$(trace_change_json bin/trace-missing.sh)" "$(trace_check_json check success)")
trace_record=$(trace_record_json 7003 "$trace_c1")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '7003 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'Feature C endpoint is implemented')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES='bin/trace-a.sh' \
  "$target/bin/agentic-loop" _worker 7003 trace-missing-path-worker
grep -Eq '^7003 failed open' "$state" || fail 'a traceability record claiming an unobserved changed path was not blocked in require mode'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=traceability-invalid detail=evidence-mismatch' 'a record claiming an unobserved changed path was not classified as evidence-mismatch'
[[ -e $target-worktrees/issue-7003 ]] || fail 'require mode removed the worktree despite blocking completion on evidence-mismatch'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-7003 || fail 'require mode removed the branch despite blocking completion on evidence-mismatch'

# 4) A record's check claim that matches the observed check-run conclusion
# passes.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_id1=$(criterion_id 'CI passes for this change')
trace_c1=$(trace_criterion_json "$trace_id1" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-ci.sh)" "$(trace_check_json ci success)")
trace_record=$(trace_record_json 7004 "$trace_c1")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '[{"name":"ci","conclusion":"success"}]' > "$TEST_ROOT/trace-check-runs.json"
printf '7004 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'CI passes for this change')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES='bin/trace-ci.sh' FAKE_CHECK_RUNS_FILE="$TEST_ROOT/trace-check-runs.json" \
  "$target/bin/agentic-loop" _worker 7004 trace-ci-match-worker
grep -Eq '^7004 completed closed' "$state" || fail 'a check claim matching the observed check-run conclusion was not accepted'

# 5) The record's own claim about a check's result is never trusted: an
# observed failing check-run beats a record that claims success (this is the
# core self-attestation regression this gate exists to prevent).
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_id1=$(criterion_id 'CI passes for this bugfix')
trace_c1=$(trace_criterion_json "$trace_id1" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-ci-fix.sh)" "$(trace_check_json ci success)")
trace_record=$(trace_record_json 7005 "$trace_c1")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '[{"name":"ci","conclusion":"failure"}]' > "$TEST_ROOT/trace-check-runs.json"
printf '7005 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'CI passes for this bugfix')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES='bin/trace-ci-fix.sh' FAKE_CHECK_RUNS_FILE="$TEST_ROOT/trace-check-runs.json" \
  "$target/bin/agentic-loop" _worker 7005 trace-ci-mismatch-worker
grep -Eq '^7005 failed open' "$state" || fail 'a record self-attesting a success the observed check-run contradicts was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=traceability-invalid detail=evidence-mismatch' 'a self-attested check result contradicting the observed check-run was not classified as evidence-mismatch'
[[ -e $target-worktrees/issue-7005 ]] || fail 'require mode removed the worktree despite blocking completion on a self-attested check mismatch'

# 6) The Issue's acceptance criteria changed (new wording -> new id) and the
# record does not cover the new id at all: criteria-missing.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_old_id=$(criterion_id 'Support v1 authentication flow')
trace_c_old=$(trace_criterion_json "$trace_old_id" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-auth.sh)" "$(trace_check_json check success)")
trace_record=$(trace_record_json 7006 "$trace_c_old")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '7006 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'Support v2 authentication flow')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES='bin/trace-auth.sh' \
  "$target/bin/agentic-loop" _worker 7006 trace-criteria-changed-worker
grep -Eq '^7006 failed open' "$state" || fail 'a record that omits a changed criterion id was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=traceability-invalid detail=criteria-missing' 'an omitted changed criterion id was not classified as criteria-missing'

# 7) The same wording change, this time with the record properly declaring
# the old id `superseded` (with `superseded_by` pointing at the new id) while
# also covering the new id: passes.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_old_id=$(criterion_id 'Support v1 authentication flow')
trace_new_id=$(criterion_id 'Support v2 authentication flow')
trace_c_new=$(trace_criterion_json "$trace_new_id" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-auth.sh)" "$(trace_check_json check success)")
trace_c_old=$(trace_criterion_json "$trace_old_id" issue-body superseded '' 'Superseded by the v2 authentication flow criterion' "$trace_new_id" '' '')
trace_record=$(trace_record_json 7007 "$trace_c_new" "$trace_c_old")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '7007 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'Support v2 authentication flow')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES='bin/trace-auth.sh' \
  "$target/bin/agentic-loop" _worker 7007 trace-superseded-worker
grep -Eq '^7007 completed closed' "$state" || fail 'a record that supersedes the old criterion id while covering the new one was blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'superseded=1' 'a superseded criterion was not reflected in the traceability verdict tally'

# 8) Manual/external verification with no automatable checks or changes is a
# legitimate way to satisfy a criterion.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_id1=$(criterion_id 'Manual QA sign-off obtained')
trace_id2=$(criterion_id 'External security review completed')
trace_c1=$(trace_criterion_json "$trace_id1" issue-body satisfied manual '' '' '' '')
trace_c2=$(trace_criterion_json "$trace_id2" issue-body satisfied external '' '' '' '')
trace_record=$(trace_record_json 7008 "$trace_c1" "$trace_c2")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '7008 running open none 2026-01-01T00:00:00Z none none %s\n' \
  "$(criteria_body 'Manual QA sign-off obtained' 'External security review completed')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" "$target/bin/agentic-loop" _worker 7008 trace-manual-external-worker
grep -Eq '^7008 completed closed' "$state" || fail 'manually/externally verified criteria with no checks or changes were not accepted'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'manual=1 external=1' 'the traceability verdict did not tally manual/external verification counts'

# 9) Squash merge regression (the bug this gate must never reintroduce): the
# merge commit recorded on the verdict must be the PR's merge_commit_sha
# (which differs from the PR head under squash merge), while check-runs are
# still looked up against the PR's own head sha, never the merge commit.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_id1=$(criterion_id 'Squash-merged change behaves correctly')
trace_c1=$(trace_criterion_json "$trace_id1" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-squash.sh)" "$(trace_check_json check success)")
trace_record=$(trace_record_json 7009 "$trace_c1")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '7009 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'Squash-merged change behaves correctly')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/calls"
squash_merge_commit='sq11111111111111111111111111111111111111'
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES='bin/trace-squash.sh' FAKE_PR_MERGE_COMMIT=$squash_merge_commit \
  "$target/bin/agentic-loop" _worker 7009 trace-squash-worker
grep -Eq '^7009 completed closed' "$state" || fail 'a squash-merge traceability record was not accepted'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" "merge_commit=$squash_merge_commit" 'the traceability verdict did not record the squash merge_commit_sha'
! grep -Fq "commits/$squash_merge_commit/check-runs" "$FAKE_GH_ROOT/calls" || fail 'squash-merge traceability queried check-runs against the merge commit instead of the PR head sha'
assert_contains "$FAKE_GH_ROOT/calls" '/check-runs' 'squash-merge traceability never queried check-runs at all'
[[ ! -e $target-worktrees/issue-7009 ]] || fail 'a passing squash-merge traceability record did not clean up the worktree'

# 10) off (the shipped default): completion never even reads the PR's file
# list, let alone requires a record.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=off
printf '7010 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/calls"
"$target/bin/agentic-loop" _worker 7010 trace-off-worker
grep -Eq '^7010 completed closed' "$state" || fail 'traceability off mode blocked completion despite having no record'
! grep -Fq '/files' "$FAKE_GH_ROOT/calls" || fail 'traceability off mode unexpectedly read the PR file list'

# 11) warn: a missing record never blocks completion, but an advisory verdict
# comment naming the failure reason is posted.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=warn
printf '7011 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _worker 7011 trace-warn-worker
grep -Eq '^7011 completed closed' "$state" || fail 'traceability warn mode blocked completion despite having no record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'verdict=warn reason=missing-record' 'traceability warn mode did not post an advisory verdict for a missing record'

# 12) require: a missing record blocks completion; the Issue is left
# open/failed with its worktree and branch intact.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
printf '7012 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _worker 7012 trace-require-missing-worker
grep -Eq '^7012 failed open$' "$state" || fail 'traceability require mode completed an Issue despite having no record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=traceability-invalid detail=missing-record' 'traceability require mode did not record the missing-record reason'
[[ -e $target-worktrees/issue-7012 ]] || fail 'traceability require mode removed the worktree despite blocking completion'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-7012 || fail 'traceability require mode removed the branch despite blocking completion'

# 13) A record containing credential-like content is rejected before it is
# ever trusted, and the raw secret text never reaches a posted comment.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_secret="ghp_$(printf '%s%s' 'abcdefghijklmnopqrst' 'uvwxyz0123456789')"
trace_id1=$(criterion_id 'Rotate the leaked token')
trace_c1=$(trace_criterion_json "$trace_id1" issue-body unmet '' "blocked pending rotation of $trace_secret" '' '' '')
trace_record=$(trace_record_json 7013 "$trace_c1")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '7013 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'Rotate the leaked token')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" "$target/bin/agentic-loop" _worker 7013 trace-secret-worker
grep -Eq '^7013 failed open' "$state" || fail 'a credential-like traceability record was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=traceability-invalid detail=secret-like' 'a credential-like traceability record was not classified as secret-like'
! grep -Fq "$trace_secret" "$FAKE_GH_ROOT/$state_key.comments" || fail 'a credential-like traceability record leaked its secret text into a posted comment'

# 14) A record exceeding the size cap is rejected outright, before any
# structural or secret validation of its content.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_big_filler=$(printf '%9000s' '' | tr ' ' x)
printf '%s' "$(trace_pr_body "$trace_big_filler")" > "$TEST_ROOT/trace-pr-body"
printf '7014 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'Any criterion at all')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" "$target/bin/agentic-loop" _worker 7014 trace-oversized-worker
grep -Eq '^7014 failed open' "$state" || fail 'an oversized traceability record was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=traceability-invalid detail=record-too-large' 'an oversized traceability record was not classified as record-too-large'

# 15) Upsert idempotence: passing the gate for the same Issue twice (e.g. a
# second convergent worker run) must update the single existing verdict
# comment in place, never create a second one.
write_queue_config "$target/.agentic-loop.toml" TRACEABILITY=require
trace_id1=$(criterion_id 'Idempotent verdict criterion')
trace_c1=$(trace_criterion_json "$trace_id1" issue-body satisfied automated '' '' "$(trace_change_json bin/trace-idem.sh)" "$(trace_check_json check success)")
trace_record=$(trace_record_json 7015 "$trace_c1")
printf '%s' "$(trace_pr_body "$trace_record")" > "$TEST_ROOT/trace-pr-body"
printf '7015 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'Idempotent verdict criterion')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES='bin/trace-idem.sh' \
  "$target/bin/agentic-loop" _worker 7015 trace-idempotent-worker-1
grep -Eq '^7015 completed closed' "$state" || fail 'idempotent-upsert setup: the first gate pass did not complete the Issue'
[[ $(grep -c 'agentic-loop:traceability schema=1' "$FAKE_GH_ROOT/$state_key.comments") -eq 1 ]] || fail 'idempotent-upsert setup: the first gate pass did not post exactly one verdict comment'
# trace_render_verdict (Issue #110): the marker must be followed by a real
# newline, encoded here as the 2-char `\n`, and the table itself must render
# with real newlines between rows (not folded into literal `\n`).
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'verdict=pass -->\n### トレーサビリティ検証結果' 'trace_render_verdict did not render a real newline after its marker'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '|---|---|---|---|---|\n|' 'trace_render_verdict did not render its table rows with real newlines'
printf '7015 running open none 2026-01-01T00:00:00Z none none %s\n' "$(criteria_body 'Idempotent verdict criterion')" > "$state"
FAKE_PR_BODY_FILE="$TEST_ROOT/trace-pr-body" FAKE_PR_FILES='bin/trace-idem.sh' \
  "$target/bin/agentic-loop" _worker 7015 trace-idempotent-worker-2
grep -Eq '^7015 completed closed' "$state" || fail 'idempotent-upsert: the second gate pass did not complete the Issue'
[[ $(grep -c 'agentic-loop:traceability schema=1' "$FAKE_GH_ROOT/$state_key.comments") -eq 1 ]] || fail 'a second traceability gate pass for the same Issue posted a second verdict comment instead of updating the existing one in place'

# 16) `bin/agentic-loop trace ISSUE --format json` (read-only CLI): a failing
# record (here, a missing one) is reported with verdict/reason/criteria and a
# non-zero exit code.
printf '7016 completed closed none 2026-01-01T00:00:00Z none none none\n' > "$state"
trace_cli_out="$TEST_ROOT/trace-cli-output.json"
if FAKE_TRACE_PR=816 FAKE_TRACE_HEAD=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef FAKE_TRACE_BASE=main \
  "$target/bin/agentic-loop" trace 7016 --format json > "$trace_cli_out"; then
  fail 'bin/agentic-loop trace did not exit non-zero for a failing traceability record'
fi
assert_contains "$trace_cli_out" '"verdict":"fail"' 'bin/agentic-loop trace json output did not report a fail verdict'
assert_contains "$trace_cli_out" '"reason":"missing-record"' 'bin/agentic-loop trace json output did not report the failure reason'
assert_contains "$trace_cli_out" '"criteria":[]' 'bin/agentic-loop trace json output did not include a criteria array'
yq -p json '.' "$trace_cli_out" >/dev/null 2>&1 || fail 'bin/agentic-loop trace --format json did not produce valid JSON'

# 17) `bin/agentic-loop trace --audit --format json`: a valid JSON summary,
# exit 0, surfacing a flagged Issue from the repository-wide verdict-comment
# sweep.
trace_audit_events_file="$TEST_ROOT/trace-audit-events.tsv"
printf '7017\t schema=1 issue=7017 pr=1 merge_commit=abc base=main checks=success criteria=1 satisfied=0 partial=0 not-applicable=0 unmet=1 superseded=0 manual=0 external=0 unreferenced_paths=0 verdict=pass \n' > "$trace_audit_events_file"
trace_audit_out="$TEST_ROOT/trace-audit-output.json"
FAKE_TRACE_AUDIT_EVENTS="$trace_audit_events_file" "$target/bin/agentic-loop" trace --audit --format json > "$trace_audit_out" \
  || fail 'bin/agentic-loop trace --audit did not exit 0 for a successful sweep'
yq -p json '.' "$trace_audit_out" >/dev/null 2>&1 || fail 'bin/agentic-loop trace --audit --format json did not produce valid JSON'
assert_contains "$trace_audit_out" '"issue":7017' 'bin/agentic-loop trace --audit did not flag an Issue with an unmet criterion'

# doctor rejects an invalid traceability value.
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.valid"
printf '[queue]\ntraceability = "loose"\n' > "$target/.agentic-loop.toml"
if "$target/bin/agentic-loop" doctor > "$TEST_ROOT/trace-doctor-output.txt"; then fail 'doctor accepted an invalid traceability value'; fi
grep -Fq '[失敗] 設定値: TRACEABILITY' "$TEST_ROOT/trace-doctor-output.txt" || fail 'doctor did not classify the invalid traceability value'
mv "$target/.agentic-loop.toml.valid" "$target/.agentic-loop.toml"

# --- Change-risk preflight (Issue #58, ADR 0020) ---
# These scenarios exercise preflight_gate/preflight_reevaluate_diff through
# the same "fresh running Issue" completion path the traceability scenarios
# above use, plus the resume pr-merged path for the escalation backstop.
capability_orig="$TEST_ROOT/preflight-capabilities-orig.toml"
cp "$target/.agentic-loop/capabilities.toml" "$capability_orig"

# 1) A normal, low-risk change: verdict=autonomous, an audit comment is
# posted, and exec actually runs (the gate does not block low risk).
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
pf_record=$(preflight_record_json 7100 "$(preflight_risks_json)")
printf '7100 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker 7100 preflight-autonomous-worker
grep -Eq '^7100 completed closed' "$state" || fail 'a normal low-risk preflight record blocked completion'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:preflight schema=1 issue=7100 verdict=autonomous' 'an autonomous preflight verdict was not recorded as an audit comment'
assert_contains "$FAKE_GH_ROOT/codex-calls" '--sandbox workspace-write' 'an autonomous preflight verdict did not let exec run'
# preflight_render_audit_body (Issue #110): the marker must be followed by a
# real newline, encoded here as the 2-char `\n`.
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'verdict=autonomous -->\n### 変更影響とリスクのpreflight判定' 'preflight_render_audit_body did not render a real newline after its marker'

# 2) Approval-required gates: one axis at "high" with its matching trigger,
# for each of the Issue's named scenarios (migration, permission change,
# external deploy, cost, rollback-blocked). None of these ever start exec.
assert_preflight_approval_gate() {
  local issue=$1 axis=$2 trigger=$3 label=$4 risks record
  risks=$(preflight_risks_json "$axis" "$(preflight_risk_json "$axis" high "テスト用の高リスク理由")")
  record=$(preflight_record_json "$issue" "$risks" '{"scope":"","tests":[],"external_operations":[],"rollback":"revertで復旧"}' "{\"required\":true,\"triggers\":[\"$trigger\"]}")
  write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
  printf '%s running open\n' "$issue" > "$state"
  : > "$FAKE_GH_ROOT/$state_key.comments"
  : > "$FAKE_GH_ROOT/codex-calls"
  FAKE_CODEX_RESULT="$(preflight_plan_body "$record")" "$target/bin/agentic-loop" _worker "$issue" "preflight-$label-worker"
  grep -Eq "^$issue needs-input open" "$state" || fail "a $label preflight record ($axis=high, trigger=$trigger) was not gated to needs-input"
  assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=preflight-approval' "the $label gate did not record reason=preflight-approval"
  assert_contains "$FAKE_GH_ROOT/$state_key.comments" "$axis: high" "the $label gate comment did not surface the risky axis"
  ! grep -Fq -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || fail "a $label preflight gate unexpectedly started exec"
}
assert_preflight_approval_gate 7101 data_migration data-migration migration
assert_preflight_approval_gate 7102 security permission permission-change
assert_preflight_approval_gate 7103 release_deploy external-deploy external-deploy
assert_preflight_approval_gate 7104 cost cost cost
assert_preflight_approval_gate 7105 rollback rollback-blocked rollback-blocked

# 3) undetermined: an axis marked "unknown" is never silently treated as low
# risk; the gate comment carries the declared reason and missing information.
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
pf_risks=$(preflight_risks_json external_environment "$(preflight_risk_json external_environment unknown '対象環境が未確定' 'どの外部環境に触れるか未確認')")
pf_record=$(preflight_record_json 7106 "$pf_risks")
printf '7106 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker 7106 preflight-undetermined-worker
grep -Eq '^7106 needs-input open' "$state" || fail 'an undetermined preflight axis was not gated to needs-input'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=preflight-undetermined' 'the undetermined gate did not record its reason'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '不足情報: どの外部環境に触れるか未確認' 'the undetermined gate did not surface the declared missing information'
! grep -Fq -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || fail 'an undetermined preflight verdict unexpectedly started exec'

# 4) invalid (one axis missing -> schema-invalid): warn mode never blocks
# completion, but posts an advisory comment naming the failure; require mode
# blocks completion (open/needs-input) instead.
pf_incomplete_risks='[{"axis":"security","level":"low"},{"axis":"confidentiality","level":"low"},{"axis":"integrity","level":"low"},{"axis":"availability","level":"low"},{"axis":"data_migration","level":"low"},{"axis":"external_environment","level":"low"},{"axis":"cost","level":"low"},{"axis":"compatibility","level":"low"},{"axis":"release_deploy","level":"low"}]'
pf_record=$(preflight_record_json 7107 "$pf_incomplete_risks")
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7107 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker 7107 preflight-invalid-warn-worker
grep -Eq '^7107 completed closed' "$state" || fail 'warn mode blocked completion for an invalid (incomplete-axes) preflight record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'verdict=invalid detail=schema-invalid' 'warn mode did not post an advisory verdict for an invalid preflight record'

pf_record2=$(preflight_record_json 7108 "$pf_incomplete_risks")
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=require TRACEABILITY=off
printf '7108 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record2")" "$target/bin/agentic-loop" _worker 7108 preflight-invalid-require-worker
grep -Eq '^7108 needs-input open' "$state" || fail 'require mode did not block completion for an invalid (incomplete-axes) preflight record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=preflight-invalid' 'require mode did not record reason=preflight-invalid'
! grep -Fq -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || fail 'require mode let exec run despite an invalid preflight record'

# 5) claim-mismatch: a record that leaves one axis "high" but self-attests
# approval.required=false is an internally-contradictory claim, never
# downgraded to "low" by trusting the approval block over the risk axis.
pf_risks=$(preflight_risks_json security "$(preflight_risk_json security high 'high軸だがrequired=falseの矛盾')")
pf_record=$(preflight_record_json 7109 "$pf_risks" '{"scope":"","tests":[],"external_operations":[],"rollback":""}' '{"required":false,"triggers":[]}')
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=require TRACEABILITY=off
printf '7109 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker 7109 preflight-claim-mismatch-worker
grep -Eq '^7109 needs-input open' "$state" || fail 'a claim-mismatch preflight record was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=preflight-invalid' 'a claim-mismatch preflight record was not classified via reason=preflight-invalid'

# 6) A credential-like value anywhere in the record is rejected before it is
# ever trusted, and the raw secret text never reaches a posted comment.
pf_secret="ghp_$(printf '%s%s' 'abcdefghijklmnopqrst' 'uvwxyz0123456789')"
pf_risks=$(preflight_risks_json rollback "$(preflight_risk_json rollback medium "rollback手順に $pf_secret を使う")")
pf_record=$(preflight_record_json 7110 "$pf_risks")
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=require TRACEABILITY=off
printf '7110 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker 7110 preflight-secret-worker
grep -Eq '^7110 needs-input open' "$state" || fail 'a credential-like preflight record was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=preflight-invalid' 'a credential-like preflight record was not classified via reason=preflight-invalid'
! grep -Fq "$pf_secret" "$FAKE_GH_ROOT/$state_key.comments" || fail 'a credential-like preflight record leaked its secret text into a posted comment'

# 7) A missing record with no risky signal: warn posts an advisory comment
# and continues; require blocks completion instead.
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7111 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _worker 7111 preflight-missing-warn-worker
grep -Eq '^7111 completed closed' "$state" || fail 'warn mode blocked completion despite no risky signal for a missing preflight record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'verdict=missing detail=missing-record' 'warn mode did not post an advisory verdict for a missing preflight record'

write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=require TRACEABILITY=off
printf '7112 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
"$target/bin/agentic-loop" _worker 7112 preflight-missing-require-worker
grep -Eq '^7112 needs-input open' "$state" || fail 'require mode completed an Issue despite a missing preflight record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=preflight-missing' 'require mode did not record reason=preflight-missing'
! grep -Fq -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || fail 'require mode let exec run despite a missing preflight record'

# 8) off: no evaluation and no comment at all, even for a record that would
# otherwise gate -- and completion still proceeds.
pf_risks=$(preflight_risks_json security "$(preflight_risk_json security high 'offなら評価されない')")
pf_record=$(preflight_record_json 7113 "$pf_risks" '{"scope":"","tests":[],"external_operations":[],"rollback":""}' '{"required":true,"triggers":["security"]}')
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=off TRACEABILITY=off
printf '7113 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker 7113 preflight-off-worker
grep -Eq '^7113 completed closed' "$state" || fail 'preflight off mode blocked completion despite an otherwise-gating record'
! grep -Fq 'agentic-loop:preflight' "$FAKE_GH_ROOT/$state_key.comments" || fail 'preflight off mode evaluated and commented despite being disabled'

# 9) Approval CLI round-trip: a gated Issue is approved by token, requeued,
# and a re-run of the same plan (same declared risk, same token) completes.
pf_risks=$(preflight_risks_json security "$(preflight_risk_json security high '承認往復test')")
pf_record=$(preflight_record_json 7114 "$pf_risks" '{"scope":"","tests":[],"external_operations":[],"rollback":""}' '{"required":true,"triggers":["security"]}')
pf_token=$(preflight_envelope_token_for 7114 "$pf_record" none '')
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7114 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker 7114 preflight-cli-gate-worker
grep -Eq '^7114 needs-input open' "$state" || fail 'CLI round-trip setup: the initial gate did not fire'
"$target/bin/agentic-loop" preflight 7114 --approve --token "$pf_token" --note 'テスト承認' >/dev/null || fail 'preflight --approve failed'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" "agentic-loop:preflight-approved schema=1 actor=test-operator token=$pf_token" 'preflight --approve did not post the approval marker'
grep -Eq '^7114 queued open' "$state" || fail 'preflight --approve did not requeue a gated Issue'
# The real Supervisor's claim_next would set agent:running before invoking
# _worker again; this harness calls _worker directly, so simulate that claim.
printf '7114 running open\n' > "$state"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker 7114 preflight-cli-approved-worker
grep -Eq '^7114 completed closed' "$state" || fail 'an approved preflight envelope was not allowed to complete on the next run'
assert_contains "$FAKE_GH_ROOT/codex-calls" '--sandbox workspace-write' 'an approved preflight envelope did not let exec run'

# 10) bin/agentic-loop preflight ISSUE --format json (read-only, no approval):
# reports the current scope/signal and never writes to GitHub.
printf '7115 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
before_calls=$(wc -l < "$FAKE_GH_ROOT/calls")
pf_cli_out=$("$target/bin/agentic-loop" preflight 7115 --format json) || fail 'preflight read-only CLI exited non-zero for a no-signal Issue'
grep -Fq '"signal":"none"' <<< "$pf_cli_out" || fail 'preflight read-only CLI did not report signal=none'
yq -p json '.' <<< "$pf_cli_out" >/dev/null 2>&1 || fail 'preflight --format json did not produce valid JSON'
[[ $(wc -l < "$FAKE_GH_ROOT/calls") -eq $before_calls ]] || fail 'preflight read-only CLI made a GitHub API call'

# 10b) Regression (Issue #191): this repository's own real capability
# manifest (not a test fixture) must not require approval for devbox.lock/
# devbox.json toolchain changes -- normal PR review is enough (see
# docs/policies/development-environment.md). A plan whose declared scope
# only touches those files, with no preflight record at all, must complete
# without a needs-input gate under the repository's own shipped PREFLIGHT=warn.
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7119 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT='## 計画

<!-- agentic-loop:scope paths=devbox.json,devbox.lock -->
AGENTIC_LOOP_RESULT=completed' "$target/bin/agentic-loop" _worker 7119 devbox-lock-no-approval-worker
grep -Eq '^7119 completed closed' "$state" || fail 'devbox.lock/devbox.json alone triggered an approval gate under the real repository capability manifest (Issue #191)'
! grep -Fq 'agentic-loop:needs-input' "$FAKE_GH_ROOT/$state_key.comments" || fail 'devbox.lock/devbox.json changes were still treated as requiring approval (Issue #191)'

# --- signal-mismatch and the escalation backstop (both need a capability
# manifest declaring a protected, approval-requiring path) ---
cat > "$target/.agentic-loop/capabilities.toml" <<'CAPTOML'
schema_version = 1
undetermined = []
[[protected]]
path = "devbox.lock"
reason = "preflight test fixture"
change_requires = "approval"
CAPTOML

# 11) A missing record is never a free pass: when the declared scope touches a
# protected, approval-requiring path, even warn mode gates (signal-mismatch).
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7116 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT='## 計画

<!-- agentic-loop:scope paths=devbox.lock -->' "$target/bin/agentic-loop" _worker 7116 preflight-signal-mismatch-worker
grep -Eq '^7116 needs-input open' "$state" || fail 'a missing record touching a protected path was not gated in warn mode (signal-mismatch)'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=preflight-signal-mismatch' 'a signal-mismatch gate did not record its reason'
! grep -Fq -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || fail 'a signal-mismatch verdict unexpectedly started exec'

# 12) Escalation backstop: scope grows during exec to touch a protected path
# without prior approval, discovered via the pr-merged resume path (mirrors
# the resumed-merged-PR fixture above, but with a real devbox.lock-touching
# commit). Completion is refused; the worktree/branch/PR are preserved and the
# Issue is never closed.
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7117 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
git -C "$target" worktree add --quiet -b agent/issue-7117 "$target-worktrees/issue-7117" origin/main
printf 'preflight-escalation-test\n' >> "$target-worktrees/issue-7117/devbox.lock"
git -C "$target-worktrees/issue-7117" add devbox.lock
git -C "$target-worktrees/issue-7117" commit --quiet -m 'touch protected path'
escalation_sha=$(git -C "$target-worktrees/issue-7117" rev-parse HEAD)
FAKE_RESUME_MERGED_PR=7117 FAKE_RESUME_MERGED_SHA=$escalation_sha FAKE_RESUME_MERGED_URL="https://github.example/acme/installed-project/pull/7117" \
  "$target/bin/agentic-loop" _worker 7117 preflight-escalation-worker
grep -Eq '^7117 needs-input open' "$state" || fail 'a protected-path escalation was not gated to needs-input'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=preflight-escalation' 'the escalation gate did not record its reason'
[[ -e $target-worktrees/issue-7117 ]] || fail 'an escalation gate removed the worktree despite blocking completion'
git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-7117 || fail 'an escalation gate removed the branch despite blocking completion'

# 13) The same escalation, this time already approved: completion proceeds
# and cleanup runs as normal.
escalation_token=$(preflight_envelope_token_for 7118 '' approval 'protected:devbox.lock')
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7118 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
git -C "$target" worktree add --quiet -b agent/issue-7118 "$target-worktrees/issue-7118" origin/main
printf 'preflight-escalation-test\n' >> "$target-worktrees/issue-7118/devbox.lock"
git -C "$target-worktrees/issue-7118" add devbox.lock
git -C "$target-worktrees/issue-7118" commit --quiet -m 'touch protected path'
escalation_sha2=$(git -C "$target-worktrees/issue-7118" rev-parse HEAD)
"$target/bin/agentic-loop" preflight 7118 --approve --token "$escalation_token" >/dev/null || fail 'approving an escalation signal token failed'
FAKE_RESUME_MERGED_PR=7118 FAKE_RESUME_MERGED_SHA=$escalation_sha2 FAKE_RESUME_MERGED_URL="https://github.example/acme/installed-project/pull/7118" \
  "$target/bin/agentic-loop" _worker 7118 preflight-escalation-approved-worker
grep -Eq '^7118 completed closed' "$state" || fail 'an approved escalation signal did not allow completion'
[[ ! -e $target-worktrees/issue-7118 ]] || fail 'an approved escalation did not clean up its worktree'
! git -C "$target" show-ref --verify --quiet refs/heads/agent/issue-7118 || fail 'an approved escalation did not remove its branch'

# 15) Token consistency across stages (Issue #218): for a *mixed* envelope --
# a declared high-risk axis together with a scope that also touches a
# protected path -- preflight_gate (before exec) and preflight_reevaluate_diff
# (the post-exec escalation backstop) must derive the byte-identical token,
# because both now read the same persisted plan record through
# preflight_envelope_token. Before this fix, the gate used a manifest-derived
# token while the escalation used a signal-only token, so an operator's
# gate-time approval silently stopped covering the same change once exec
# reached the escalation re-evaluation.
pf_risks=$(preflight_risks_json security "$(preflight_risk_json security high '混在envelope token一致test')")
pf_record=$(preflight_record_json 7122 "$pf_risks" '{"scope":"","tests":[],"external_operations":[],"rollback":""}' '{"required":true,"triggers":["security"]}')
pf_mixed_plan=$(printf '## 計画\n\n```agentic-loop:preflight\n%s\n```\n\n<!-- agentic-loop:scope paths=devbox.lock -->\nAGENTIC_LOOP_RESULT=completed\n' "$pf_record")
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7122 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT="$pf_mixed_plan" "$target/bin/agentic-loop" _worker 7122 preflight-mixed-gate-worker
grep -Eq '^7122 needs-input open' "$state" || fail 'a mixed declared-risk + protected-path envelope was not gated'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=preflight-approval' 'a mixed envelope with a declared high-risk axis was not classified via reason=preflight-approval'
gate_token=$(grep -oE 'reason=preflight-approval token=[0-9a-f]{12}' "$FAKE_GH_ROOT/$state_key.comments" | tail -n1 | grep -oE '[0-9a-f]{12}')
[[ -n $gate_token ]] || fail 'could not extract the gate token for the mixed envelope scenario'
expected_mixed_token=$(preflight_envelope_token_for 7122 "$pf_record" approval 'protected:devbox.lock')
[[ $gate_token == "$expected_mixed_token" ]] || fail "the gate token ($gate_token) for a mixed envelope did not match the envelope formula ($expected_mixed_token)"
! grep -Fq -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || fail 'a mixed envelope gate unexpectedly started exec'
[[ -e $target-worktrees/issue-7122 ]] || fail 'a preflight gate removed the worktree despite blocking completion'

printf 'preflight-escalation-test\n' >> "$target-worktrees/issue-7122/devbox.lock"
git -C "$target-worktrees/issue-7122" add devbox.lock
git -C "$target-worktrees/issue-7122" commit --quiet -m 'touch protected path'
mixed_escalation_sha=$(git -C "$target-worktrees/issue-7122" rev-parse HEAD)
printf '7122 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_RESUME_MERGED_PR=7122 FAKE_RESUME_MERGED_SHA=$mixed_escalation_sha FAKE_RESUME_MERGED_URL="https://github.example/acme/installed-project/pull/7122" \
  "$target/bin/agentic-loop" _worker 7122 preflight-mixed-escalation-worker
grep -Eq '^7122 needs-input open' "$state" || fail 'the escalation backstop did not re-gate the still-unapproved mixed envelope'
escalation_token=$(grep -oE 'reason=preflight-escalation token=[0-9a-f]{12}' "$FAKE_GH_ROOT/$state_key.comments" | tail -n1 | grep -oE '[0-9a-f]{12}')
[[ -n $escalation_token ]] || fail 'could not extract the escalation token for the mixed envelope scenario'
[[ $escalation_token == "$gate_token" ]] || fail "preflight_gate's token ($gate_token) and preflight_reevaluate_diff's token ($escalation_token) diverged for the same declared-risk + protected-path envelope (Issue #218)"

# 16) ...and once that shared token is approved once, at the gate, the later
# escalation re-evaluation of the identical envelope never re-blocks:
# completion proceeds and cleanup runs as normal (Issue #218 acceptance
# criterion 2).
"$target/bin/agentic-loop" preflight 7122 --approve --token "$gate_token" >/dev/null || fail 'approving the mixed envelope gate token failed'
grep -Eq '^7122 queued open' "$state" || fail 'approving the mixed envelope token did not requeue the Issue'
printf '7122 running open\n' > "$state"
# Comments are intentionally NOT cleared here: preflight_approved's cache-miss
# scan (Issue #197) needs the approval marker just posted above still present
# in this fixture's comment log to find it. A byte offset, not a clear, is
# used below to isolate only what this next _worker call newly posts.
comments_before_approved_run=$(wc -c < "$FAKE_GH_ROOT/$state_key.comments")
FAKE_RESUME_MERGED_PR=7122 FAKE_RESUME_MERGED_SHA=$mixed_escalation_sha FAKE_RESUME_MERGED_URL="https://github.example/acme/installed-project/pull/7122" \
  "$target/bin/agentic-loop" _worker 7122 preflight-mixed-approved-worker
grep -Eq '^7122 completed closed' "$state" || fail 'a gate-approved mixed envelope was re-blocked by the escalation backstop instead of completing'
! tail -c "+$((comments_before_approved_run + 1))" "$FAKE_GH_ROOT/$state_key.comments" | grep -Fq 'reason=preflight-escalation' || fail 'an approved mixed envelope still posted a new escalation gate comment'
[[ ! -e $target-worktrees/issue-7122 ]] || fail 'an approved mixed envelope did not clean up its worktree'

# 17) An envelope that actually grows must never be covered by a narrower
# approval (Issue #218 acceptance criterion 4): approve a security-only
# high-risk envelope, then re-plan the same Issue with an additional
# cost=high axis -- the gate must fire again, carrying its own distinct
# token, and the already-approved narrower token must not satisfy it.
pf_risks_narrow=$(preflight_risks_json security "$(preflight_risk_json security high '承認範囲test-narrow')")
pf_record_narrow=$(preflight_record_json 7123 "$pf_risks_narrow" '{"scope":"","tests":[],"external_operations":[],"rollback":""}' '{"required":true,"triggers":["security"]}')
narrow_token=$(preflight_envelope_token_for 7123 "$pf_record_narrow" none '')
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7123 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record_narrow")" "$target/bin/agentic-loop" _worker 7123 preflight-narrow-gate-worker
grep -Eq '^7123 needs-input open' "$state" || fail 'setup: the narrow envelope gate did not fire'
"$target/bin/agentic-loop" preflight 7123 --approve --token "$narrow_token" >/dev/null || fail 'approving the narrow envelope token failed'
grep -Eq '^7123 queued open' "$state" || fail 'setup: approving the narrow envelope token did not requeue the Issue'

pf_risks_wide='[{"axis":"security","level":"high","reason":"承認範囲test-wide"},{"axis":"confidentiality","level":"low"},{"axis":"integrity","level":"low"},{"axis":"availability","level":"low"},{"axis":"data_migration","level":"low"},{"axis":"external_environment","level":"low"},{"axis":"cost","level":"high","reason":"追加費用の発生"},{"axis":"compatibility","level":"low"},{"axis":"release_deploy","level":"low"},{"axis":"rollback","level":"low"}]'
pf_record_wide=$(preflight_record_json 7123 "$pf_risks_wide" '{"scope":"","tests":[],"external_operations":[],"rollback":""}' '{"required":true,"triggers":["security","cost"]}')
wide_token=$(preflight_envelope_token_for 7123 "$pf_record_wide" none '')
[[ $wide_token != "$narrow_token" ]] || fail 'a wider declared-risk envelope collided with the already-approved narrower one'
printf '7123 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record_wide")" "$target/bin/agentic-loop" _worker 7123 preflight-wide-gate-worker
grep -Eq '^7123 needs-input open' "$state" || fail 'an envelope that grew beyond an already-approved one was not re-gated with a new token (Issue #218)'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" "token=$wide_token" 'the re-gated wider envelope did not carry its own distinct token'
! grep -Fq "token=$narrow_token" "$FAKE_GH_ROOT/$state_key.comments" || fail 'the re-gated wider envelope was incorrectly satisfied by the narrower approved token'

# 18) missing↔invalid convergence, at the escalation stage (Issue #218
# acceptance criterion 3): preflight_reevaluate_diff derives the identical
# token whether the persisted plan record is entirely absent or present but
# schema-invalid (one axis missing here, mirroring scenario 4 above), for the
# same protected-path signal.
write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7124 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
git -C "$target" worktree add --quiet -b agent/issue-7124 "$target-worktrees/issue-7124" origin/main
printf 'preflight-escalation-test\n' >> "$target-worktrees/issue-7124/devbox.lock"
git -C "$target-worktrees/issue-7124" add devbox.lock
git -C "$target-worktrees/issue-7124" commit --quiet -m 'touch protected path'
invalid_escalation_sha=$(git -C "$target-worktrees/issue-7124" rev-parse HEAD)
rm -f "$state_root/issue-7124-plan.txt"
FAKE_RESUME_MERGED_PR=7124 FAKE_RESUME_MERGED_SHA=$invalid_escalation_sha FAKE_RESUME_MERGED_URL="https://github.example/acme/installed-project/pull/7124" \
  "$target/bin/agentic-loop" _worker 7124 preflight-escalation-missing-plan-worker
grep -Eq '^7124 needs-input open' "$state" || fail 'setup: the no-plan-file escalation did not gate'
missing_plan_token=$(grep -oE 'reason=preflight-escalation token=[0-9a-f]{12}' "$FAKE_GH_ROOT/$state_key.comments" | tail -n1 | grep -oE '[0-9a-f]{12}')
[[ -n $missing_plan_token ]] || fail 'could not extract the no-plan-file escalation token'

printf '## 計画\n\n```agentic-loop:preflight\n%s\n```\n' "$(preflight_record_json 7124 "$pf_incomplete_risks")" > "$state_root/issue-7124-plan.txt"
: > "$FAKE_GH_ROOT/$state_key.comments"
printf '7124 running open\n' > "$state"
FAKE_RESUME_MERGED_PR=7124 FAKE_RESUME_MERGED_SHA=$invalid_escalation_sha FAKE_RESUME_MERGED_URL="https://github.example/acme/installed-project/pull/7124" \
  "$target/bin/agentic-loop" _worker 7124 preflight-escalation-invalid-plan-worker
grep -Eq '^7124 needs-input open' "$state" || fail 'setup: the invalid-plan-file escalation did not gate'
invalid_plan_token=$(grep -oE 'reason=preflight-escalation token=[0-9a-f]{12}' "$FAKE_GH_ROOT/$state_key.comments" | tail -n1 | grep -oE '[0-9a-f]{12}')
[[ -n $invalid_plan_token ]] || fail 'could not extract the invalid-plan-file escalation token'
[[ $missing_plan_token == "$invalid_plan_token" ]] || fail "the escalation token differed between a missing plan record ($missing_plan_token) and a schema-invalid one ($invalid_plan_token) under the same signal (Issue #218)"

cp "$capability_orig" "$target/.agentic-loop/capabilities.toml"

# 19) preflight_approved's comment scan does not grow with the Issue's
# lifetime comment count (Issue #197): a re-check after approval only ever
# re-scans comments posted since the last check, not the whole history.
# `preflight-approved-rows.log` (populated by the fake gh's mock, one line per
# `issues/N/comments` GET this endpoint handles) reports how many comment rows
# each call actually processed -- the direct proxy for transferred data volume
# a real paginated GitHub response would carry.
pf_scan_scaling_scenario() {
  local issue=$1 filler_count=$2 i
  local rows_log="$FAKE_GH_ROOT/preflight-approved-rows.log"
  pf_risks=$(preflight_risks_json security "$(preflight_risk_json security high 'scan境界test')")
  pf_record=$(preflight_record_json "$issue" "$pf_risks" '{"scope":"","tests":[],"external_operations":[],"rollback":""}' '{"required":true,"triggers":["security"]}')
  pf_token=$(preflight_envelope_token_for "$issue" "$pf_record" none '')
  write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
  printf '%s running open\n' "$issue" > "$state"
  : > "$FAKE_GH_ROOT/$state_key.comments"
  for ((i = 1; i <= filler_count; i++)); do
    printf '%s filler comment %s of a long Issue history\n' "$issue" "$i" >> "$FAKE_GH_ROOT/$state_key.comments"
  done
  : > "$rows_log"
  FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker "$issue" "preflight-scan-scaling-worker-$issue" >/dev/null 2>&1
  grep -Eq "^$issue needs-input open" "$state" || fail "scan-scaling setup: the initial gate for issue $issue ($filler_count filler comments) did not fire"
  cold_rows=$(cat "$rows_log") || fail "scan-scaling setup: no preflight_approved scan was recorded for issue $issue's initial gate"
  [[ $cold_rows -ge $filler_count ]] || fail "scan-scaling setup: the initial (cache-cold) scan for issue $issue processed fewer rows ($cold_rows) than its $filler_count filler comments"
  "$target/bin/agentic-loop" preflight "$issue" --approve --token "$pf_token" >/dev/null 2>&1 || fail "scan-scaling setup: preflight --approve failed for issue $issue"
  printf '%s running open\n' "$issue" > "$state"
  : > "$rows_log"
  FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record")" "$target/bin/agentic-loop" _worker "$issue" "preflight-scan-scaling-approved-worker-$issue" >/dev/null 2>&1
  grep -Eq "^$issue completed closed" "$state" || fail "an approved envelope did not complete for issue $issue after $filler_count filler comments"
  warm_rows=$(cat "$rows_log") || fail "scan-scaling: no preflight_approved scan was recorded for issue $issue's post-approval re-check"
  printf '%s\n' "$warm_rows"
}
pf_cold_and_warm_10=$(pf_scan_scaling_scenario 7120 10) || fail 'preflight_approved scan-scaling scenario (10 filler comments) failed'
pf_cold_and_warm_100=$(pf_scan_scaling_scenario 7121 100) || fail 'preflight_approved scan-scaling scenario (100 filler comments) failed'
[[ $pf_cold_and_warm_10 -le 10 ]] || fail "preflight_approved's post-approval re-check processed $pf_cold_and_warm_10 rows for only 10 filler comments -- it re-scanned more than the Issue's small delta"
[[ $pf_cold_and_warm_100 -le 10 ]] || fail "preflight_approved's post-approval re-check processed $pf_cold_and_warm_100 rows for 100 filler comments -- it re-scanned close to the full Issue history instead of only the delta since its last check"
[[ $pf_cold_and_warm_10 -eq $pf_cold_and_warm_100 ]] || fail "preflight_approved's post-approval re-check scaled with the Issue's comment count (10 filler comments -> $pf_cold_and_warm_10 rows, 100 filler comments -> $pf_cold_and_warm_100 rows); it should cost the same regardless of history size"

# 20) Prose invariance (Issue #218 acceptance criterion 7): re-planning the
# same Issue with different free-text prose (risk reason, change scope,
# tests, rollback) but the identical declared risk axes and approval
# triggers must derive the byte-identical envelope token, both via the
# formula directly and through a real gate/re-plan cycle -- a previously
# approved token must still satisfy the re-planned envelope with no new
# gate comment posted.
pf_risks_prose_a=$(preflight_risks_json security "$(preflight_risk_json security high 'prose-a: 初回の説明文')")
pf_record_prose_a=$(preflight_record_json 7125 "$pf_risks_prose_a" '{"scope":"scope A","tests":["test A"],"external_operations":[],"rollback":"rollback A"}' '{"required":true,"triggers":["security"]}')
pf_risks_prose_b=$(preflight_risks_json security "$(preflight_risk_json security high 'prose-b: 書き直した別の説明文')")
pf_record_prose_b=$(preflight_record_json 7125 "$pf_risks_prose_b" '{"scope":"scope B (再計画で変更)","tests":["test B","test C"],"external_operations":[],"rollback":"rollback B"}' '{"required":true,"triggers":["security"]}')
prose_token_a=$(preflight_envelope_token_for 7125 "$pf_record_prose_a" none '')
prose_token_b=$(preflight_envelope_token_for 7125 "$pf_record_prose_b" none '')
[[ $prose_token_a == "$prose_token_b" ]] || fail "re-planning with only prose changes (reason/scope/tests/rollback text) produced a different token ($prose_token_a vs $prose_token_b) despite identical declared risk axes and approval triggers (Issue #218)"

write_queue_config "$target/.agentic-loop.toml" PREFLIGHT=warn TRACEABILITY=off
printf '7125 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record_prose_a")" "$target/bin/agentic-loop" _worker 7125 preflight-prose-gate-worker
grep -Eq '^7125 needs-input open' "$state" || fail 'setup: the initial prose-a plan did not gate'
prose_gate_token=$(grep -oE 'reason=preflight-approval token=[0-9a-f]{12}' "$FAKE_GH_ROOT/$state_key.comments" | tail -n1 | grep -oE '[0-9a-f]{12}')
[[ -n $prose_gate_token ]] || fail 'could not extract the gate token for the prose-invariance scenario'
[[ $prose_gate_token == "$prose_token_a" ]] || fail "the gate token ($prose_gate_token) did not match the envelope formula ($prose_token_a)"
"$target/bin/agentic-loop" preflight 7125 --approve --token "$prose_gate_token" >/dev/null || fail 'approving the prose-a envelope token failed'
grep -Eq '^7125 queued open' "$state" || fail 'setup: approving the prose-a token did not requeue the Issue'

printf '7125 running open\n' > "$state"
comments_before_prose_replan=$(wc -c < "$FAKE_GH_ROOT/$state_key.comments")
FAKE_CODEX_RESULT="$(preflight_plan_body "$pf_record_prose_b")" "$target/bin/agentic-loop" _worker 7125 preflight-prose-replan-worker
grep -Eq '^7125 completed closed' "$state" || fail 'a re-plan that changed only prose text (same declared risk + triggers) was re-blocked instead of completing (Issue #218)'
! tail -c "+$((comments_before_prose_replan + 1))" "$FAKE_GH_ROOT/$state_key.comments" | grep -Fq 'reason=preflight-approval' || fail 'a prose-only re-plan unexpectedly posted a new gate comment despite an identical declared-risk envelope already being approved'
[[ ! -e $target-worktrees/issue-7125 ]] || fail 'a prose-only re-planned, already-approved envelope did not clean up its worktree'

# doctor rejects an invalid preflight value.
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.valid"
printf '[queue]\npreflight = "loose"\n' > "$target/.agentic-loop.toml"
if "$target/bin/agentic-loop" doctor > "$TEST_ROOT/preflight-doctor-output.txt"; then fail 'doctor accepted an invalid preflight value'; fi
grep -Fq '[失敗] 設定値: PREFLIGHT' "$TEST_ROOT/preflight-doctor-output.txt" || fail 'doctor did not classify the invalid preflight value'
mv "$target/.agentic-loop.toml.valid" "$target/.agentic-loop.toml"

# --- Resource scalability workload budget gate (Issue #130, ADR 0025) ------
# These scenarios exercise workload_gate through the same "fresh running
# Issue" completion path the preflight scenarios above use. PREFLIGHT=off
# throughout so only the workload verdict/mode drives the outcome.

# 1) external_io="none": exec runs, completes, and posts no workload comment
# at all (a "not-applicable" verdict is not itself audit-worthy).
write_queue_config "$target/.agentic-loop.toml" WORKLOAD=warn PREFLIGHT=off TRACEABILITY=off
wl_record=$(workload_record_json 7200 none)
printf '7200 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT="$(workload_plan_body "$wl_record")" "$target/bin/agentic-loop" _worker 7200 workload-none-worker
grep -Eq '^7200 completed closed' "$state" || fail 'external_io=none blocked completion'
assert_contains "$FAKE_GH_ROOT/codex-calls" '--sandbox workspace-write' 'external_io=none did not let exec run'
! grep -Fq 'agentic-loop:workload' "$FAKE_GH_ROOT/$state_key.comments" || fail 'external_io=none (not-applicable) unexpectedly posted an audit comment'

# 2) external_io="added" with a complete unit: verdict=declared, exec runs,
# and an audit comment records the declaration.
write_queue_config "$target/.agentic-loop.toml" WORKLOAD=warn PREFLIGHT=off TRACEABILITY=off
wl_unit=$(workload_unit_json 'supervisor 1 poll' 'REST list 1回' 'O(1) in N(queued件数)' 'per_page=100 + --paginate' 'snapshotを共有')
wl_record=$(workload_record_json 7201 added "[$wl_unit]" '["queue group T1"]')
printf '7201 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT="$(workload_plan_body "$wl_record")" "$target/bin/agentic-loop" _worker 7201 workload-declared-worker
grep -Eq '^7201 completed closed' "$state" || fail 'a complete workload record blocked completion'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:workload schema=1 issue=7201 verdict=declared' 'a declared workload verdict was not recorded as an audit comment'
assert_contains "$FAKE_GH_ROOT/codex-calls" '--sandbox workspace-write' 'a declared workload verdict did not let exec run'

# 3) missing record: warn posts an advisory comment and continues; require
# blocks completion instead (mirrors the preflight missing-record scenarios).
write_queue_config "$target/.agentic-loop.toml" WORKLOAD=warn PREFLIGHT=off TRACEABILITY=off
printf '7202 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _worker 7202 workload-missing-warn-worker
grep -Eq '^7202 completed closed' "$state" || fail 'warn mode blocked completion for a missing workload record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:workload schema=1 issue=7202 verdict=missing' 'warn mode did not post an advisory verdict for a missing workload record'

write_queue_config "$target/.agentic-loop.toml" WORKLOAD=require PREFLIGHT=off TRACEABILITY=off
printf '7203 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
"$target/bin/agentic-loop" _worker 7203 workload-missing-require-worker
grep -Eq '^7203 needs-input open' "$state" || fail 'require mode completed an Issue despite a missing workload record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=workload-missing' 'require mode did not record reason=workload-missing'
! grep -Fq -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || fail 'require mode let exec run despite a missing workload record'

# 4) invalid record (external_io="added" but no units -> schema-invalid):
# warn posts an advisory comment and continues; require blocks completion.
write_queue_config "$target/.agentic-loop.toml" WORKLOAD=warn PREFLIGHT=off TRACEABILITY=off
wl_record=$(workload_record_json 7204 added '[]' '[]')
printf '7204 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT="$(workload_plan_body "$wl_record")" "$target/bin/agentic-loop" _worker 7204 workload-invalid-warn-worker
grep -Eq '^7204 completed closed' "$state" || fail 'warn mode blocked completion for an invalid (unit-less) workload record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'verdict=invalid detail=schema-invalid' 'warn mode did not post an advisory verdict for an invalid workload record'

wl_record2=$(workload_record_json 7205 added '[]' '[]')
write_queue_config "$target/.agentic-loop.toml" WORKLOAD=require PREFLIGHT=off TRACEABILITY=off
printf '7205 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
FAKE_CODEX_RESULT="$(workload_plan_body "$wl_record2")" "$target/bin/agentic-loop" _worker 7205 workload-invalid-require-worker
grep -Eq '^7205 needs-input open' "$state" || fail 'require mode did not block completion for an invalid (unit-less) workload record'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=workload-invalid' 'require mode did not record reason=workload-invalid'
! grep -Fq -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls" || fail 'require mode let exec run despite an invalid workload record'

# 5) A credential-like value anywhere in the record is rejected before it is
# ever trusted, and the raw secret text never reaches a posted comment.
wl_secret="ghp_$(printf '%s%s' 'abcdefghijklmnopqrst' 'uvwxyz0123456789')"
wl_unit=$(workload_unit_json "leaks $wl_secret" x x x x)
wl_record=$(workload_record_json 7206 added "[$wl_unit]" '["x"]')
write_queue_config "$target/.agentic-loop.toml" WORKLOAD=require PREFLIGHT=off TRACEABILITY=off
printf '7206 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT="$(workload_plan_body "$wl_record")" "$target/bin/agentic-loop" _worker 7206 workload-secret-worker
grep -Eq '^7206 needs-input open' "$state" || fail 'a credential-like workload record was not blocked'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reason=workload-invalid' 'a credential-like workload record was not classified via reason=workload-invalid'
! grep -Fq "$wl_secret" "$FAKE_GH_ROOT/$state_key.comments" || fail 'a credential-like workload record leaked its secret text into a posted comment'

# 6) off: no evaluation and no comment at all, even for a record that would
# otherwise gate -- and completion still proceeds.
write_queue_config "$target/.agentic-loop.toml" WORKLOAD=off PREFLIGHT=off TRACEABILITY=off
printf '7207 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" _worker 7207 workload-off-worker
grep -Eq '^7207 completed closed' "$state" || fail 'workload off mode blocked completion despite an otherwise-gating (missing) record'
! grep -Fq 'agentic-loop:workload' "$FAKE_GH_ROOT/$state_key.comments" || fail 'workload off mode evaluated and commented despite being disabled'

# doctor rejects an invalid workload value.
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.valid"
printf '[queue]\nworkload = "loose"\n' > "$target/.agentic-loop.toml"
if "$target/bin/agentic-loop" doctor > "$TEST_ROOT/workload-doctor-output.txt"; then fail 'doctor accepted an invalid workload value'; fi
grep -Fq '[失敗] 設定値: WORKLOAD' "$TEST_ROOT/workload-doctor-output.txt" || fail 'doctor did not classify the invalid workload value'
mv "$target/.agentic-loop.toml.valid" "$target/.agentic-loop.toml"

# --- status observability (Issue #42) ---
# The Supervisor is deliberately left stopped for the manual-state scenarios
# below: a live start would call rebuild_scope_cache/recover_expired and
# overwrite these manually seeded fixtures (mirrors the existing scope/
# conflict status test above).
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
rm -f "$state_root/supervisor.pid" "$state_root/stop.requested" "$state_root/project-pending" \
      "$state_root/budget-paused" "$state_root/core-budget-paused" "$state_root/agent-exhausted" "$state_root/all-pools-paused"
rmdir "$state_root/supervisor.lock" 2>/dev/null || true
rm -rf "$state_root/pools" "$state_root/workers" "$state_root/scope" "$state_root/conflict" "$state_root/dependency" "$state_root/attempts"
# Earlier scenarios in this file intentionally leave worktrees/branches behind
# (they test worker() resume/cleanup behavior); clear them so the idle
# scenario below starts from a genuinely anomaly-free baseline.
while IFS= read -r leftover_worktree; do
  [[ -n $leftover_worktree ]] || continue
  git -C "$target" worktree remove --force "$leftover_worktree" 2>/dev/null || true
done < <(git -C "$target" worktree list --porcelain | awk -v root="$target-worktrees/issue-" '$1 == "worktree" && index($2, root) == 1 {print $2}')
while IFS= read -r leftover_branch; do
  [[ -n $leftover_branch ]] || continue
  git -C "$target" branch -D "$leftover_branch" >/dev/null 2>&1 || true
done < <(git -C "$target" for-each-ref --format='%(refname:short)' refs/heads/agent/)

# Scenario: idle (nothing running or queued). status stays a pure read (no
# repository change, no GitHub write), costs at most 2 REST(core) reads (the
# open-Issue snapshot and the closed agent:stale summary), and exits 0.
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
before_status=$(git -C "$target" status --porcelain)
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls" 2>/dev/null || printf 0)
status_output=$("$target/bin/agentic-loop" status); status_rc=$?
(( status_rc == 0 )) || fail 'idle status did not exit 0'
grep -Fq 'stopped' <<< "$status_output" || fail 'idle status did not report the supervisor as stopped'
grep -Fq 'Running Issues: none' <<< "$status_output" || fail 'idle status did not report no running Issues'
grep -Fq '競合待ちIssue: none' <<< "$status_output" || fail 'idle status did not report no conflict waits'
grep -Fq 'キュー: 0件' <<< "$status_output" || fail 'idle status did not report an empty queue'
grep -Fq '状態サマリ:' <<< "$status_output" || fail 'idle status did not show the state summary section'
grep -Fq '自動回復中: none' <<< "$status_output" || fail 'idle status unexpectedly reported automatic recovery'
grep -Fq '要対応: none' <<< "$status_output" || fail 'idle status unexpectedly reported an action item'
[[ $(git -C "$target" status --porcelain) == "$before_status" ]] || fail 'idle status modified the repository working tree'
status_delta=$(tail -n +"$((calls_before + 1))" "$FAKE_GH_ROOT/calls")
idle_core_reads=$(grep -c $'\tapi repos/' <<< "$status_delta" || true)
idle_mutations=$(grep -Ec -- '--method (POST|PUT|PATCH)|	api graphql|	project |	api rate_limit' <<< "$status_delta" || true)
(( idle_core_reads <= 2 )) || fail "idle status made more than 2 REST(core) reads (got $idle_core_reads)"
(( idle_mutations == 0 )) || fail 'idle status made a write or GraphQL/Projects/rate_limit call'

idle_json=$("$target/bin/agentic-loop" status --format json)
printf '%s' "$idle_json" | yq -p json -o json >/dev/null || fail 'idle status --format json did not produce valid JSON'
[[ $(printf '%s' "$idle_json" | yq -p json '.schema_version') == 1 ]] || fail 'status --format json did not report schema_version 1'
[[ $(printf '%s' "$idle_json" | yq -p json '.github_available') == true ]] || fail 'idle status --format json did not report github_available'
[[ $(printf '%s' "$idle_json" | yq -p json '.supervisor.state') == stopped ]] || fail 'idle status --format json did not report a stopped supervisor'
[[ $(printf '%s' "$idle_json" | yq -p json '.workers | length') -eq 0 ]] || fail 'idle status --format json listed a running worker'
[[ $(printf '%s' "$idle_json" | yq -p json '.queue.queued') -eq 0 ]] || fail 'idle status --format json reported a non-empty queue'
[[ $(printf '%s' "$idle_json" | yq -p json '.anomalies | length') -eq 0 ]] || fail 'idle status --format json reported an anomaly'
if "$target/bin/agentic-loop" status --format yaml >/dev/null 2>&1; then fail 'status accepted an invalid --format value'; fi
if "$target/bin/agentic-loop" status extra-argument >/dev/null 2>&1; then fail 'status accepted an unexpected extra argument'; fi

# Scenario: multiple running workers, each with rich local state (started,
# phase, lease, worktree/PR via the .resume cache) shown with zero additional
# GitHub calls beyond the one open-Issue snapshot.
printf '30 running open none 2026-01-01T00:00:00Z\n31 running open none 2026-01-02T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root/workers"
now=$(date +%s)
printf '%s\n' "$((now - 120))" > "$state_root/workers/30.started"
printf 'worktree-ready\n' > "$state_root/workers/30.phase"
printf '111\t%s\t%s\n' "$((now + 300))" "$now" > "$state_root/workers/30.lease"
printf 'pr-open\tagent/issue-30\t6\thttps://github.example/acme/installed-project/pull/6\topen\tsuccess\t0\t0\n' > "$state_root/workers/30.resume"
printf '%s\n' "$((now - 10))" > "$state_root/workers/31.started"
printf 'fresh\n' > "$state_root/workers/31.phase"
status_output=$("$target/bin/agentic-loop" status)
grep -Fq '#30' <<< "$status_output" || fail 'multi-worker status did not list Issue #30'
grep -Fq '#31' <<< "$status_output" || fail 'multi-worker status did not list Issue #31'
grep -Fq 'phase: worktree-ready' <<< "$status_output" || fail 'multi-worker status did not show the observed resume phase'
grep -Fq 'pr: #6 state=open checks=success' <<< "$status_output" || fail 'multi-worker status did not show the cached PR info'
grep -Fq 'elapsed:' <<< "$status_output" || fail 'multi-worker status did not show elapsed time since worker start'
grep -Fq 'heartbeat:' <<< "$status_output" || fail 'multi-worker status did not show the last heartbeat'
grep -Fq 'timeout_at:' <<< "$status_output" || fail 'multi-worker status did not show the worker-timeout deadline'
multi_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$multi_json" | yq -p json '.workers | length') -eq 2 ]] || fail 'multi-worker status --format json did not list both running Issues'
[[ $(printf '%s' "$multi_json" | yq -p json '.workers[] | select(.issue == 30) | .pr') -eq 6 ]] || fail 'multi-worker status --format json did not report the cached PR number'
[[ $(printf '%s' "$multi_json" | yq -p json '.workers[] | select(.issue == 30) | .timeout_at') -eq $((now - 120 + 14400)) ]] || fail 'multi-worker status --format json did not compute the worker-timeout deadline from the default worker_timeout_seconds'
[[ $(printf '%s' "$multi_json" | yq -p json '.workers[] | select(.issue == 30) | .timeout_exceeded') == false ]] || fail 'multi-worker status --format json falsely reported the worker-timeout deadline as exceeded'
rm -rf "$state_root/workers"

# Scenario: queued Issues are counted, ordered exactly like claim_next
# (numeric priority desc, then category rank, then created_at, then number),
# and the ordering is cross-checked against an actual claim.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
prio90=$(printf '<!-- agentic-loop:priority 90 -->' | base64 -w0)
prio25=$(printf '<!-- agentic-loop:priority 25 -->' | base64 -w0)
printf '40 queued open none 2026-01-03T00:00:00Z none none %s\n41 queued open none 2026-01-01T00:00:00Z none none %s\n' "$prio25" "$prio90" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
queue_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.queued') -eq 2 ]] || fail 'queued status did not count both Issues'
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.claimable') -eq 2 ]] || fail 'queued status did not report both Issues as claimable'
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.candidates[0].issue') -eq 41 ]] || fail 'queue candidate preview did not rank the higher-priority Issue first'
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.candidates[0].priority') -eq 90 ]] || fail 'queue candidate preview did not expose the numeric priority value'
status_output=$("$target/bin/agentic-loop" status)
grep -Fq '#41 Fake issue 41 (claimable)' <<< "$status_output" || fail 'text status did not show the top claim candidate as claimable'
rm -f "$state_root/workers/41.pid"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^41 completed closed' "$state" || fail 'claim_next did not claim the Issue the status preview ranked first'
grep -Eq '^40 queued open' "$state" || fail 'claim_next claimed more than MAX_WORKERS allowed in one pass'

# Scenario: needs-input, failed, blocked and stale Issues are summarized with
# counts and constructed Issue URLs (no extra GitHub call is needed for the
# URL: see project_add_content's identical https://github.com/OWNER/REPO
# convention).
printf '50 needs-input open\n51 failed open\n52 blocked open\n53 stale closed\n54 parked open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'needs-input: 1件 (https://github.com/acme/installed-project/issues/50)' <<< "$status_output" || fail 'status did not summarize the needs-input Issue with its URL'
grep -Fq 'failed: 1件 (https://github.com/acme/installed-project/issues/51)' <<< "$status_output" || fail 'status did not summarize the failed Issue with its URL'
grep -Fq 'blocked: 1件 (https://github.com/acme/installed-project/issues/52)' <<< "$status_output" || fail 'status did not summarize the blocked Issue with its URL'
grep -Fq 'stale: 1件 (https://github.com/acme/installed-project/issues/53)' <<< "$status_output" || fail 'status did not summarize the closed agent:stale Issue with its URL'
grep -Fq 'parked: 1件 (https://github.com/acme/installed-project/issues/54)' <<< "$status_output" || fail 'status did not summarize the parked Issue with its URL'
status_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$status_json" | yq -p json '.states.parked.count') -eq 1 ]] || fail 'status --format json did not report the parked count'
[[ $(printf '%s' "$status_json" | yq -p json '.states.parked.issues[0].number') -eq 54 ]] || fail 'status --format json did not list the parked Issue number'

# Scenario: an expired lease is surfaced as a warning, both in the running
# Issue's own line and as a structured anomaly, without mutating any state
# (the supervisor's own recover_expired poll is what actually recovers it).
printf '60 running open\n' > "$state"
mkdir -p "$state_root/workers"
now=$(date +%s)
printf '222\t%s\t%s\n' "$((now - 100))" "$((now - 400))" > "$state_root/workers/60.lease"
before_status=$(git -C "$target" status --porcelain)
status_output=$("$target/bin/agentic-loop" status); status_rc=$?
(( status_rc == 0 )) || fail 'lease-expired status did not exit 0'
grep -Fq '#60' <<< "$status_output" || fail 'lease-expired status did not list the running Issue'
grep -Fq '期限切れ' <<< "$status_output" || fail 'lease-expired status did not mark the running Issue lease as expired'
grep -Fq 'lease-expired' <<< "$status_output" || fail 'lease-expired status did not report a lease-expired anomaly'
grep -Fq '自動回復中:' <<< "$status_output" || fail 'lease-expired status did not separate automatic recovery'
grep -Eq '経過[0-9]+(秒|分[0-9]+秒|時間[0-9]+分|日[0-9]+時間)' <<< "$status_output" || fail 'lease-expired status did not show a human-readable elapsed time'
grep -Fq '次pollで安全にqueueへ戻します。' <<< "$status_output" || fail 'lease-expired status did not show the next automatic action'
[[ $(git -C "$target" status --porcelain) == "$before_status" ]] || fail 'lease-expired status modified the repository working tree'
lease_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$lease_json" | yq -p json '.workers[0].lease_expired') == true ]] || fail 'status --format json did not mark the worker lease as expired'
[[ $(printf '%s' "$lease_json" | yq -p json '.anomalies[] | select(.code == "lease-expired") | .subject') == '#60' ]] || fail 'status --format json did not report the lease-expired anomaly'
[[ $(printf '%s' "$lease_json" | yq -p json '.anomalies[] | select(.code == "lease-expired") | .classification') == recovering ]] || fail 'status JSON did not classify lease-expired as recovering'
[[ $(printf '%s' "$lease_json" | yq -p json '.anomalies[] | select(.code == "lease-expired") | .elapsed') -ge 1 ]] || fail 'status JSON did not include elapsed for lease-expired'
[[ -n $(printf '%s' "$lease_json" | yq -p json '.anomalies[] | select(.code == "lease-expired") | .action') ]] || fail 'status JSON did not include action for lease-expired'
rm -rf "$state_root/workers"

# Scenario: corrupted local-state files (a non-numeric .started, a malformed
# .lease) and a stale supervisor.pid never crash `status` and are reported as
# warnings instead, and the repository is left untouched.
printf '70 running open\n' > "$state"
mkdir -p "$state_root/workers"
printf 'not-a-number\n' > "$state_root/workers/70.started"
printf 'not-numeric\n' > "$state_root/workers/70.lease"
sh -c 'exit 0' & dead_pid=$!
wait "$dead_pid" 2>/dev/null || true
printf '%s\n' "$dead_pid" > "$state_root/supervisor.pid"
before_status=$(git -C "$target" status --porcelain)
status_output=$("$target/bin/agentic-loop" status); status_rc=$?
(( status_rc == 0 )) || fail 'status crashed on corrupted local state'
grep -Fq 'local-state-corrupt' <<< "$status_output" || fail 'status did not report the corrupted local-state files'
grep -Fq 'supervisor-stale-pid' <<< "$status_output" || fail 'status did not report the stale supervisor.pid'
grep -Fq '要対応:' <<< "$status_output" || fail 'status did not separate human action items'
grep -Fq '対応: bin/agentic-loop start' <<< "$status_output" || fail 'status did not show the stale supervisor recovery action'
[[ $(git -C "$target" status --porcelain) == "$before_status" ]] || fail 'status modified the repository working tree on corrupted local state'
printf '1\t1700000030\tabrupt\tunknown\t%s\t1700000000\t1700000020\tpoll\t00000000-0000-0000-0000-000000000000\t12345\n' "$dead_pid" > "$state_root/supervisor.last-exit"
status_output=$("$target/bin/agentic-loop" status)
status_json=$("$target/bin/agentic-loop" status --format json)
grep -Fq '前回のSupervisor終了: kind=abrupt detail=unknown' <<< "$status_output" || fail 'status did not render the previous abrupt supervisor termination'
[[ $(printf '%s' "$status_json" | yq -p json '.supervisor.last_exit.kind') == abrupt ]] || fail 'status JSON did not expose the previous supervisor termination kind'
[[ $(printf '%s' "$status_json" | yq -p json '.supervisor.last_exit.stage') == poll ]] || fail 'status JSON did not expose the last supervisor stage'
rm -f "$state_root/supervisor.pid" "$state_root/workers/70.started" "$state_root/workers/70.lease"

# Scenario: project-sync-pending's elapsed tracks the oldest unresolved
# retry via project-pending.since, not project-pending's own mtime
# (regression: an ack rewrites the file via awk+mv and a later enqueue
# appends to it, both of which reset a plain mtime-based elapsed to near
# zero even though the retry queue never actually drained).
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
printf '95\n' > "$state_root/project-pending"
printf '%s\n' "$(($(date +%s) - 120))" > "$state_root/project-pending.since"
sleep 1
printf '96\n' >> "$state_root/project-pending"
pending_json=$("$target/bin/agentic-loop" status --format json)
pending_elapsed=$(printf '%s' "$pending_json" | yq -p json '.anomalies[] | select(.code == "project-sync-pending") | .elapsed')
(( pending_elapsed >= 120 )) || fail "project-sync-pending elapsed did not reflect the oldest pending entry despite a later append touching the file's mtime (got $pending_elapsed)"
rm -f "$state_root/project-pending" "$state_root/project-pending.since"

# Scenario: claim-paused elapsed reflects whichever pause marker is actually
# in effect (regression: it used to be hardcoded to budget-paused's mtime and
# read as 0 -- via a stat that always failed -- whenever some other reason,
# e.g. a stop-request drain, was the actual cause).
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/budget-paused" "$state_root/core-budget-paused" "$state_root/agent-exhausted" "$state_root/all-pools-paused"
: > "$state_root/stop.requested"
sleep 1
claimpause_output=$("$target/bin/agentic-loop" status)
grep -Fq 'claim-paused' <<< "$claimpause_output" || fail 'status did not report a claim-paused anomaly for a stop-request drain'
claimpause_json=$("$target/bin/agentic-loop" status --format json)
claimpause_elapsed=$(printf '%s' "$claimpause_json" | yq -p json '.anomalies[] | select(.code == "claim-paused") | .elapsed')
(( claimpause_elapsed >= 1 )) || fail "claim-paused elapsed was not derived from the stop.requested marker when budget-paused was absent (got $claimpause_elapsed)"
rm -f "$state_root/stop.requested"

# Scenario: worker-orphan is needs-attention (not recovering) and says the
# automatic stop is disabled when queue.worker_orphan_grace_seconds=0, but
# recovering when grace is positive (regression: status used to always claim
# the supervisor would eventually auto-stop the worker, which is false at
# grace=0 -- worker_orphan_reap itself no-ops when grace is 0).
printf '93 needs-input open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root/workers"
printf '%s\n' "$$" > "$state_root/workers/93.pid"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30 WORKER_ORPHAN_GRACE_SECONDS=0
orphan0_output=$("$target/bin/agentic-loop" status)
grep -Fq '自動停止は無効です' <<< "$orphan0_output" || fail 'worker-orphan with grace=0 did not say the automatic stop is disabled'
orphan0_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$orphan0_json" | yq -p json '.anomalies[] | select(.code == "worker-orphan") | .classification') == needs-attention ]] || fail 'worker-orphan with grace=0 was not classified as needs-attention'
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
orphan1_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$orphan1_json" | yq -p json '.anomalies[] | select(.code == "worker-orphan") | .classification') == recovering ]] || fail 'worker-orphan with grace>0 was not classified as recovering'
rm -rf "$state_root/workers"
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"

# Structural check: every anomaly a running scenario produces carries both a
# non-empty action and classification (Issue #212's requirement that every
# displayed line has a next step), checked mechanically instead of per-code.
printf '94 running open\n' > "$state"
mkdir -p "$state_root/workers"
now=$(date +%s)
printf '%s\n' "$((now - 100))" > "$state_root/workers/94.started"
printf '222\t%s\t%s\n' "$((now - 100))" "$((now - 400))" > "$state_root/workers/94.lease"
structural_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$structural_json" | yq -p json '.anomalies | length') -ge 1 ]] || fail 'structural anomaly check scenario produced no anomalies to verify'
[[ $(printf '%s' "$structural_json" | yq -p json '[.anomalies[] | select(.action == "" or .classification == "")] | length') -eq 0 ]] || fail 'some anomaly is missing an action or a classification'
rm -rf "$state_root/workers"
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"

# Scenario: a worktree/branch left over from an Issue no longer agent:running
# is left to Issue #211's supervisor prune and never adds status noise.
git -C "$target" worktree add --quiet -b agent/issue-9999 "$target-worktrees/issue-9999" origin/main
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'residual-worktree' <<< "$status_output" && fail 'status reported a residual worktree despite automatic prune ownership'
grep -Fq 'residual-branch' <<< "$status_output" && fail 'status reported a residual branch despite automatic prune ownership'
git -C "$target" worktree remove --force "$target-worktrees/issue-9999"
git -C "$target" branch -D agent/issue-9999 >/dev/null
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'residual-worktree' <<< "$status_output" && fail 'status reported a removed worktree'
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -rf "$state_root/workers"

# --- local progress observability (Issue #159) ---
# A running Issue with a fresh progress marker shows stage / seconds-ago /
# health in text and JSON, with zero additional GitHub calls.
printf '80 running open\n81 running open\n82 running open\n83 running open\n' > "$state"
mkdir -p "$state_root/workers" "$state_root/logs"
now=$(date +%s)
printf '%s\n' "$((now - 60))" > "$state_root/workers/80.started"
printf '%s\tplan\t5\n' "$((now - 10))" > "$state_root/workers/80.progress"
printf '%s\n' "$((now - 100))" > "$state_root/workers/81.started"
# stage=checks is a non-provider stage, so it uses the narrower STALL_SECONDS
# band (default 300s) rather than PROVIDER_STALL_SECONDS -- see the dedicated
# stage-band tests below for exec/plan/replan using the wider band.
printf '%s\tchecks\t9\n' "$((now - 400))" > "$state_root/workers/81.progress"
printf '%s\n' "$((now - 20000))" > "$state_root/workers/82.started"
# Issue 83 intentionally has no local state at all (other-host scenario).
progress_status_before=$(git -C "$target" status --porcelain)
progress_calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
progress_output=$("$target/bin/agentic-loop" status); progress_rc=$?
(( progress_rc == 0 )) || fail 'progress status did not exit 0'
grep -Fq '#80' <<< "$progress_output" || fail 'progress status did not list the fresh-worker Issue'
grep -Fq 'stage: plan' <<< "$progress_output" || fail 'progress status did not show the stage from the progress marker'
grep -Fq 'progress: 1' <<< "$progress_output" || fail 'progress status did not show seconds since last progress'
grep -Fq 'health: healthy' <<< "$progress_output" || fail 'progress status did not mark a fresh worker healthy'
grep -Fq '#81' <<< "$progress_output" || fail 'progress status did not list the stalled Issue'
grep -Fq 'health: stalled' <<< "$progress_output" || fail 'progress status did not mark an idle worker stalled'
grep -Fq 'worker-stalled' <<< "$progress_output" || fail 'progress status did not report the worker-stalled anomaly'
grep -Fq '#82' <<< "$progress_output" || fail 'progress status did not list the timed-out Issue'
grep -Fq 'health: timeout' <<< "$progress_output" || fail 'progress status did not mark an over-timeout worker as timed out'
grep -Fq '#83' <<< "$progress_output" || fail 'progress status did not list the other-host Issue'
grep -Fq 'health: unknown' <<< "$progress_output" || fail 'progress status did not leave a stateless Issue as unknown'
[[ $(git -C "$target" status --porcelain) == "$progress_status_before" ]] || fail 'progress status modified the repository working tree'
progress_delta=$(tail -n +"$((progress_calls_before + 1))" "$FAKE_GH_ROOT/calls")
(( $(grep -c $'\tapi repos/' <<< "$progress_delta" || true) <= 2 )) || fail 'progress status exceeded the 2-read REST budget'
(( $(grep -Ec -- '--method (POST|PUT|PATCH)|	api graphql|	api rate_limit|	project ' <<< "$progress_delta" || true) == 0 )) || fail 'progress status made a write or GraphQL/Projects/rate_limit call'

progress_json=$("$target/bin/agentic-loop" status --format json)
printf '%s' "$progress_json" | yq -p json -o json >/dev/null || fail 'progress status --format json did not produce valid JSON'
[[ $(printf '%s' "$progress_json" | yq -p json '.workers[] | select(.issue == 80) | .stage') == plan ]] || fail 'progress status JSON did not report the stage'
[[ $(printf '%s' "$progress_json" | yq -p json '.workers[] | select(.issue == 80) | .progress_age_seconds') -ge 8 ]] || fail 'progress status JSON did not report the seconds since last progress'
[[ $(printf '%s' "$progress_json" | yq -p json '.workers[] | select(.issue == 80) | .health') == healthy ]] || fail 'progress status JSON did not mark the fresh worker healthy'
[[ $(printf '%s' "$progress_json" | yq -p json '.workers[] | select(.issue == 80) | .progress_at') =~ ^[0-9]+$ ]] || fail 'progress status JSON did not report the progress timestamp'
[[ $(printf '%s' "$progress_json" | yq -p json '.workers[] | select(.issue == 81) | .health') == stalled ]] || fail 'progress status JSON did not mark the idle worker stalled'
[[ $(printf '%s' "$progress_json" | yq -p json '.workers[] | select(.issue == 82) | .health') == timeout ]] || fail 'progress status JSON did not mark the over-timeout worker timed out'
[[ $(printf '%s' "$progress_json" | yq -p json '.workers[] | select(.issue == 83) | .health') == unknown ]] || fail 'progress status JSON did not leave the stateless Issue unknown'
[[ $(printf '%s' "$progress_json" | yq -p json '.anomalies[] | select(.code == "worker-stalled") | .subject') == '#81' ]] || fail 'progress status JSON did not report the worker-stalled anomaly'
[[ $(printf '%s' "$progress_json" | yq -p json '.workers[] | select(.issue == 80) | .phase') == '' ]] || fail 'progress status JSON changed the existing phase field contract'

# --- stage-based, configurable stall thresholds (Issue #273) ---
# Provider stages (plan/exec/replan) block on one long CLI call and must not
# flag as stalled within ordinary reasoning/exec time; other stages keep the
# narrower default. Both bands are separately configurable, and status
# reports which threshold it actually applied.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 STALL_SECONDS=300 PROVIDER_STALL_SECONDS=3600
printf '84 running open\n85 running open\n86 running open\n' > "$state"
mkdir -p "$state_root/workers" "$state_root/logs"
now=$(date +%s)
printf '%s\n' "$((now - 5000))" > "$state_root/workers/84.started"
printf '%s\texec\t1\n' "$((now - 1000))" > "$state_root/workers/84.progress"
printf '%s\n' "$((now - 5000))" > "$state_root/workers/85.started"
printf '%s\texec\t1\n' "$((now - 4000))" > "$state_root/workers/85.progress"
printf '%s\n' "$((now - 5000))" > "$state_root/workers/86.started"
printf '%s\tplan\t1\n' "$((now - 500))" > "$state_root/workers/86.progress"
band_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$band_json" | yq -p json '.workers[] | select(.issue == 84) | .health') == healthy ]] || fail 'exec stage within provider_stall_seconds was misclassified as stalled'
[[ $(printf '%s' "$band_json" | yq -p json '.workers[] | select(.issue == 84) | .stall_threshold_seconds') == 3600 ]] || fail 'status JSON did not report the applied provider stall threshold'
[[ $(printf '%s' "$band_json" | yq -p json '.workers[] | select(.issue == 85) | .health') == stalled ]] || fail 'exec stage beyond provider_stall_seconds was not detected as a genuine stall'
[[ $(printf '%s' "$band_json" | yq -p json '.workers[] | select(.issue == 86) | .health') == healthy ]] || fail 'plan stage within provider_stall_seconds was misclassified as stalled'
[[ $(printf '%s' "$band_json" | yq -p json '.workers[] | select(.issue == 86) | .stall_threshold_seconds') == 3600 ]] || fail 'plan stage did not report the provider stall threshold'
band_text=$("$target/bin/agentic-loop" status)
grep -Fq 'stage=exec' <<< "$band_text" || fail 'worker-stalled anomaly text did not include the stage'
grep -Eq '適用閾値 3600秒' <<< "$band_text" || fail 'worker-stalled anomaly text did not include the applied threshold'

# Configurability: lowering provider_stall_seconds makes the same 1000s-old
# exec worker stall, and setting it to 0 disables the band entirely.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 STALL_SECONDS=300 PROVIDER_STALL_SECONDS=500
tuned_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$tuned_json" | yq -p json '.workers[] | select(.issue == 84) | .health') == stalled ]] || fail 'lowering queue.provider_stall_seconds via config did not take effect'
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 STALL_SECONDS=300 PROVIDER_STALL_SECONDS=0
disabled_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$disabled_json" | yq -p json '.workers[] | select(.issue == 85) | .health') == healthy ]] || fail 'queue.provider_stall_seconds=0 did not disable stall detection for provider stages'
rm -rf "$state_root/workers"
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# A stage output file's mtime (agent_run_stage's plan/result file, including
# its transient `.raw.*`/`.final.*` siblings while streaming) is a secondary
# progress signal independent of the progress marker (Issue #273): a worker
# whose marker looks old but is still visibly writing output must stay
# healthy.
printf '87 running open\n' > "$state"
mkdir -p "$state_root/workers" "$state_root/logs"
now=$(date +%s)
printf '%s\n' "$((now - 5000))" > "$state_root/workers/87.started"
printf '%s\texec\t1\n' "$((now - 4000))" > "$state_root/workers/87.progress"
: > "$state_root/issue-87-result.txt"
touch -d "@$((now - 10))" "$state_root/issue-87-result.txt"
output_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$output_json" | yq -p json '.workers[] | select(.issue == 87) | .health') == healthy ]] || fail 'a fresh stage output file mtime did not keep an old-progress-marker worker healthy'
rm -f "$state_root/issue-87-result.txt"
rm -rf "$state_root/workers"
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"

# `status --watch` on a non-TTY (pipe/redirect) prints the recent events once
# and exits without a follow loop: later appends are not streamed, and the run
# makes zero GitHub calls and leaves the repository untouched.
: > "$state_root/events.log"
printf '%s\t80\tprogress\tplan\n' "$((now - 10))" >> "$state_root/events.log"
printf '%s\tsupervisor\tstart\t-\n' "$((now - 20))" >> "$state_root/events.log"
watch_once_status_before=$(git -C "$target" status --porcelain)
watch_once_calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
"$target/bin/agentic-loop" status --watch > "$TEST_ROOT/watch-once.out" 2>&1; watch_once_rc=$?
(( watch_once_rc == 0 )) || fail 'status --watch did not exit 0 on a non-TTY'
grep -Fq 'plan' "$TEST_ROOT/watch-once.out" || fail 'status --watch did not print existing events once'
grep -Fq 'supervisor' "$TEST_ROOT/watch-once.out" || fail 'status --watch did not print all existing events once'
"$target/bin/agentic-loop" status --watch > "$TEST_ROOT/watch-once.out" 2>&1 &
watch_once_pid=$!
wait_gone 'status --watch (non-TTY) exited without following' "$watch_once_pid" \
  || { kill -TERM "$watch_once_pid" 2>/dev/null || true; wait "$watch_once_pid" 2>/dev/null || true; fail 'status --watch kept following on a non-TTY'; }
printf '%s\t84\tprogress\tmerge\n' "$((now))" >> "$state_root/events.log"
! grep -Fq 'merge' "$TEST_ROOT/watch-once.out" || fail 'status --watch streamed an event appended after its non-TTY exit'
watch_once_delta=$(tail -n +"$((watch_once_calls_before + 1))" "$FAKE_GH_ROOT/calls")
[[ -z $watch_once_delta ]] || fail 'status --watch made a GitHub call'
[[ $(git -C "$target" status --porcelain) == "$watch_once_status_before" ]] || fail 'status --watch modified the repository working tree'
rm -f "$TEST_ROOT/watch-once.out"

# `status --watch` on a TTY follows events.log like tail -f: existing events
# print first, appended events stream in, and SIGTERM exits 0 (REST 0, repo
# untouched). A pseudo-terminal via `script` makes stdout a TTY.
watch_tty_status_before=$(git -C "$target" status --porcelain)
watch_tty_calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
script -q -c "$target/bin/agentic-loop status --watch" /dev/null > "$TEST_ROOT/watch-tty.out" 2>&1 &
watch_tty_pid=$!
wait_for 'status --watch (TTY) printed existing events' grep -Fq 'plan' "$TEST_ROOT/watch-tty.out" || true
printf '%s\t85\tprogress\tcleanup\n' "$((now))" >> "$state_root/events.log"
wait_for 'status --watch (TTY) streamed the appended event' grep -Fq 'cleanup' "$TEST_ROOT/watch-tty.out" || true
kill -TERM "$watch_tty_pid" 2>/dev/null || true
wait "$watch_tty_pid" 2>/dev/null || true; watch_tty_rc=$?
(( watch_tty_rc == 0 )) || fail 'status --watch (TTY) did not exit 0 on SIGTERM'
grep -Fq 'plan' "$TEST_ROOT/watch-tty.out" || fail 'status --watch (TTY) did not print existing events'
grep -Fq 'cleanup' "$TEST_ROOT/watch-tty.out" || fail 'status --watch (TTY) did not stream newly appended events'
watch_tty_delta=$(tail -n +"$((watch_tty_calls_before + 1))" "$FAKE_GH_ROOT/calls")
[[ -z $watch_tty_delta ]] || fail 'status --watch (TTY) made a GitHub call'
[[ $(git -C "$target" status --porcelain) == "$watch_tty_status_before" ]] || fail 'status --watch (TTY) modified the repository working tree'
rm -f "$TEST_ROOT/watch-tty.out"

# Argument validation: the legacy watch interval must still be a positive
# integer and watch is text-only; unknown flags exit 2 without rendering.
if "$target/bin/agentic-loop" status --watch -1 >/dev/null 2>&1; then fail 'status --watch accepted a negative interval'; fi
if "$target/bin/agentic-loop" status --watch 0 >/dev/null 2>&1; then fail 'status --watch accepted a zero interval'; fi
if "$target/bin/agentic-loop" status --watch abc >/dev/null 2>&1; then fail 'status --watch accepted a non-numeric interval'; fi
if "$target/bin/agentic-loop" status --format json --watch 1 >/dev/null 2>&1; then fail 'status --format json --watch was not rejected'; fi
if "$target/bin/agentic-loop" status --bogus >/dev/null 2>&1; then fail 'status accepted an unknown flag'; fi
rm -f "$state_root/events.log"

# `tail` streams the append-only events log (timestamp, subject, code, stage)
# with zero REST reads and never shows log/Issue bodies.
: > "$state_root/events.log"
{
  printf '%s\t80\tprogress\tplan\n' "$((now - 10))"
  printf '%s\tsupervisor\tstart\t-\n' "$((now - 20))"
  printf '%s\t81\tprogress\texec\n' "$((now - 5))"
} >> "$state_root/events.log"
tail_calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
tail_output=$("$target/bin/agentic-loop" tail); tail_rc=$?
(( tail_rc == 0 )) || fail 'tail did not exit 0'
grep -Fq 'progress' <<< "$tail_output" || fail 'tail did not show progress events'
grep -Fq 'supervisor' <<< "$tail_output" || fail 'tail did not show supervisor events'
grep -Fq 'plan' <<< "$tail_output" || fail 'tail did not show the plan stage'
grep -Fq 'exec' <<< "$tail_output" || fail 'tail did not show the exec stage'
grep -Fq '2026' <<< "$tail_output" || fail 'tail did not format event timestamps'
tail_issue_output=$("$target/bin/agentic-loop" tail --issue 80)
grep -Fq 'plan' <<< "$tail_issue_output" || fail 'tail --issue did not show the matching Issue events'
! grep -Fq 'exec' <<< "$tail_issue_output" || fail 'tail --issue leaked another Issue events'
tail_delta=$(tail -n +"$((tail_calls_before + 1))" "$FAKE_GH_ROOT/calls")
[[ -z $tail_delta ]] || fail 'tail made a GitHub call'
if "$target/bin/agentic-loop" tail --issue abc >/dev/null 2>&1; then fail 'tail --issue accepted a non-numeric value'; fi
if "$target/bin/agentic-loop" tail --bogus >/dev/null 2>&1; then fail 'tail accepted an unknown flag'; fi

# `tail --follow` prints existing events and streams newly appended ones, then
# exits 0 on SIGTERM (the whole run is REST 0).
tail_follow_calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
"$target/bin/agentic-loop" tail --follow > "$TEST_ROOT/tail-follow.out" 2>&1 &
tail_follow_pid=$!
wait_for 'tail --follow printed existing events' grep -Fq 'progress' "$TEST_ROOT/tail-follow.out" || true
printf '%s\t84\tprogress\tmerge\n' "$((now))" >> "$state_root/events.log"
wait_for 'tail --follow streamed the appended event' grep -Fq 'merge' "$TEST_ROOT/tail-follow.out" || true
kill -TERM "$tail_follow_pid" 2>/dev/null || true
wait "$tail_follow_pid" 2>/dev/null || true
grep -Fq 'progress' "$TEST_ROOT/tail-follow.out" || fail 'tail --follow did not print existing events'
grep -Fq 'merge' "$TEST_ROOT/tail-follow.out" || fail 'tail --follow did not stream newly appended events'
[[ $(tail -n +"$((tail_follow_calls_before + 1))" "$FAKE_GH_ROOT/calls") == '' ]] || fail 'tail --follow made a GitHub call'
rm -f "$state_root/events.log" "$TEST_ROOT/tail-follow.out"

# A finished worker's progress marker is removed together with its other local
# state (clear_worker_local), so no stale marker survives the lifecycle.
printf '4 running open\n5 running open\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
FAKE_CODEX_RESULT=AGENTIC_LOOP_RESULT=needs-input "$target/bin/agentic-loop" _worker 4 test-worker
FAKE_CODEX_RESULT=AGENTIC_LOOP_RESULT=failed "$target/bin/agentic-loop" _worker 5 test-worker
[[ ! -e $state_root/workers/4.progress ]] || fail 'a finished worker left its .progress marker behind'
[[ ! -e $state_root/workers/5.progress ]] || fail 'a failed worker left its .progress marker behind'
rm -rf "$state_root/workers" "$state_root/logs"

# --- Issue-level execution control: pause / resume / abort (Issue #57, ADR 0019) ---
# This is a separate, non-terminal layer from dispose (ADR 0010): pause/abort
# never close an Issue, and only an authenticated repository write-or-better
# operator (never an Issue comment, PR body, or provider self-report) may call
# them.
mkdir -p "$state_root/workers" "$state_root/logs"
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30 PAUSE_GRACE_SECONDS=1

# 1) pause on a queued Issue moves it straight to agent:paused (open,
# non-claim) and never blocks an unrelated queued Issue from completing on
# the same poll; pause is stable across further polls (no repeat comments,
# no reclaim); resume re-verifies and returns a queued-origin pause straight
# to agent:queued, which the next poll claims to completion.
printf '190 queued open none 2026-01-01T00:00:00Z\n191 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
"$target/bin/agentic-loop" pause 190 --reason 'manual test pause'
grep -Eq '^190 paused open' "$state" || fail 'pause did not move a queued Issue to agent:paused'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:pause schema=1' 'pause did not record its audit marker'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'from=queued' 'pause did not record the pre-pause state'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^190 paused open' "$state" || fail 'paused Issue was reclaimed by the supervisor'
grep -Eq '^191 completed closed' "$state" || fail 'a paused Issue blocked an unrelated queued Issue from completing'
[[ ! -e "$state_root/conflict/issue-191" ]] || fail 'a paused Issue left a stale conflict-wait entry for an unrelated Issue'
comments_before=$(wc -l < "$FAKE_GH_ROOT/$state_key.comments")
# An ordinary Issue comment whose text merely resembles a pause marker must
# never be treated as a command: there is no comment-parsing trigger at all.
printf '190 someone posts: please /pause this <!-- agentic-loop:pause schema=1 actor=impostor issue=190 from=queued at=1 -->\n' >> "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^190 paused open' "$state" || fail 'a paused Issue changed state after an impersonated comment'
[[ $(wc -l < "$FAKE_GH_ROOT/$state_key.comments") -eq $((comments_before + 1)) ]] || fail 'a paused Issue accumulated supervisor comments across polls'
"$target/bin/agentic-loop" resume 190
grep -Eq '^190 queued open' "$state" || fail 'resume did not return a queued-origin pause to agent:queued'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:resume schema=1' 'resume did not record its audit marker'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^190 completed closed' "$state" || fail 'a resumed paused Issue was not claimed and completed on the next poll'

# 2) pause on a genuinely running Issue drains its local worker (a
# cooperative stop-request, then TERM/KILL bounded by pause_grace_seconds)
# and leaves it agent:paused with the worktree/branch/in-progress commit
# untouched, recording a resume checkpoint (the same agentic-loop:handoff
# comment worker resume already uses); resume of a running-origin pause
# re-verifies lease/worktree/PR/checks before returning it to agent:queued
# (never straight back to agent:running), and the next poll claims and
# completes it, reusing the preserved worktree/branch.
printf '192 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested"
FAKE_CODEX_SLEEP=30 "$target/bin/agentic-loop" _supervise &
pause_sup_pid=$!
pause_claimed=0
wait_for 'pause claimed' test -e "$state_root/workers/192.pid" && pause_claimed=1
[[ $pause_claimed == 1 ]] || { kill "$pause_sup_pid" 2>/dev/null; wait "$pause_sup_pid" 2>/dev/null; fail 'worker was not claimed before the running-pause test'; }
"$target/bin/agentic-loop" pause 192 --reason 'incident freeze'
kill "$pause_sup_pid" 2>/dev/null; wait "$pause_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
grep -Eq '^192 paused open' "$state" || fail 'pause did not drain a running worker to agent:paused'
[[ ! -e $state_root/workers/192.pid ]] || fail 'pause left a worker pidfile behind'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:pause schema=1' 'pause of a running Issue did not record its audit marker'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'from=running' 'pause did not record running as the pre-pause state'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:handoff' 'pause did not leave a resume checkpoint'
# resume_handoff_body's printf path (Issue #110): the marker must be followed
# by a real newline, encoded here as the 2-char `\n`.
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '-->\n### 再開のための引き継ぎ' 'resume_handoff_body did not render a real newline after its marker'
[[ -e "$target-worktrees/issue-192" ]] || fail 'pause deleted the in-progress worktree'
git -C "$target-worktrees/issue-192" symbolic-ref -q HEAD | grep -Fq 'agent/issue-192' || fail 'pause disturbed the in-progress branch'
"$target/bin/agentic-loop" resume 192
grep -Eq '^192 queued open' "$state" || fail 'resume did not return a running-origin pause to agent:queued (not straight back to running)'
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^192 completed closed' "$state" || fail 'a resumed running-origin paused Issue was not claimed and completed on the next poll'
[[ ! -e "$target-worktrees/issue-192" ]] || fail 'completed resumed Issue left its worktree behind'

# 3) pause preserves the pre-pause state through resume for needs-input,
# blocked, and failed (never promoting them to queued, and pausing a failed
# Issue stops its automatic retry in the meantime).
printf '193 needs-input open none 2026-01-01T00:00:00Z\n194 blocked open none 2026-01-01T00:00:00Z\n195 failed open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root/attempts"; printf '1\t0\n' > "$state_root/attempts/issue-195"
"$target/bin/agentic-loop" pause 193
"$target/bin/agentic-loop" pause 194
"$target/bin/agentic-loop" pause 195
grep -Eq '^193 paused open' "$state" || fail 'pause from needs-input did not move to agent:paused'
grep -Eq '^194 paused open' "$state" || fail 'pause from blocked did not move to agent:paused'
grep -Eq '^195 paused open' "$state" || fail 'pause from failed did not move to agent:paused'
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^195 paused open' "$state" || fail 'a paused failed-origin Issue was auto-retried by retry_failed while paused'
"$target/bin/agentic-loop" resume 193
"$target/bin/agentic-loop" resume 194
"$target/bin/agentic-loop" resume 195
grep -Eq '^193 needs-input open' "$state" || fail 'resume promoted a needs-input-origin pause to queued instead of restoring needs-input'
grep -Eq '^194 blocked open' "$state" || fail 'resume promoted a blocked-origin pause to queued instead of restoring blocked'
grep -Eq '^195 queued open' "$state" || fail 'resume did not return a failed-origin pause to agent:queued'
rm -f "$state_root/attempts/issue-195"

# 4) authorization, idempotence, and cross-operation guards: pause/abort/
# resume require repository write-or-better permission and change nothing on
# denial; a second pause/abort is a harmless no-op; abort after pause moves
# straight to agent:parked; pause after abort (already parked) is refused.
printf '196 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$closes"
if FAKE_COLLABORATOR_PERMISSION=read "$target/bin/agentic-loop" pause 196 2>/dev/null; then fail 'pause succeeded without repository write permission'; fi
grep -Eq '^196 queued open' "$state" || fail 'a denied pause changed Issue state'
[[ ! -s $FAKE_GH_ROOT/$state_key.comments ]] || fail 'a denied pause posted a comment'
if FAKE_COLLABORATOR_PERMISSION=read "$target/bin/agentic-loop" abort 196 2>/dev/null; then fail 'abort succeeded without repository write permission'; fi
grep -Eq '^196 queued open' "$state" || fail 'a denied abort changed Issue state'
"$target/bin/agentic-loop" pause 196
if FAKE_COLLABORATOR_PERMISSION=read "$target/bin/agentic-loop" resume 196 2>/dev/null; then fail 'resume succeeded without repository write permission'; fi
grep -Eq '^196 paused open' "$state" || fail 'a denied resume changed Issue state'
comments_before=$(wc -l < "$FAKE_GH_ROOT/$state_key.comments")
"$target/bin/agentic-loop" pause 196
[[ $(wc -l < "$FAKE_GH_ROOT/$state_key.comments") -eq $comments_before ]] || fail 'a repeated pause on an already-paused Issue was not a harmless no-op'
"$target/bin/agentic-loop" abort 196 --reason 'cutting over to a manual fix'
grep -Eq '^196 parked open' "$state" || fail 'abort after pause did not move the Issue to agent:parked'
[[ ! -r "$closes" ]] || ! grep -Fq $'^196\t' "$closes" || fail 'abort must never close an Issue'
if "$target/bin/agentic-loop" pause 196 2>/dev/null; then fail 'pause succeeded on an already-parked (aborted) Issue'; fi
grep -Eq '^196 parked open' "$state" || fail 'a refused pause changed an aborted Issue'
"$target/bin/agentic-loop" abort 196
grep -Eq '^196 parked open' "$state" || fail 'a repeated abort on an already-parked Issue was not a harmless no-op'
"$target/bin/agentic-loop" resume 196
grep -Eq '^196 queued open' "$state" || fail 'resume did not re-queue an aborted (agent:parked) Issue through the existing dispose.sh path'

fi

if [[ $TEST_GROUP == all || $TEST_GROUP == auxiliary ]]; then

# Repositories use separate gh/project state and Git state directories.
second=$(new_repository second-project)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$second" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
[[ -e $FAKE_GH_ROOT/$(printf '%s' "$second" | tr '/' '_').project ]] || fail 'second repository did not get its own Project'
[[ $(git -C "$target" rev-parse --absolute-git-dir) != $(git -C "$second" rev-parse --absolute-git-dir) ]] || fail 'repository state is not isolated'

# Install resolves the provider from agent.provider (config), not only from
# AGENT_PROVIDER: with codex absent but the config selecting opencode, install
# still succeeds because it checks the configured provider's CLI.
provider_repo=$(new_repository provider-config)
printf '[agent]\nprovider = "opencode"\n' > "$provider_repo/.agentic-loop.local.toml"
mv "$FAKE_BIN/codex" "$FAKE_BIN/codex.disabled"
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$provider_repo" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null 2>&1 || { mv "$FAKE_BIN/codex.disabled" "$FAKE_BIN/codex"; fail 'install did not honor agent.provider=opencode from config (still required codex)'; }
mv "$FAKE_BIN/codex.disabled" "$FAKE_BIN/codex"

# Preconditions and conflicts cause no partial copy.
bad="$TEST_ROOT/no-origin"
git init --quiet "$bad"
if AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$bad" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null 2>&1; then fail 'install accepted a repository without origin'; fi
[[ ! -e $bad/AGENTS.md ]] || fail 'failed preflight left partial files'

conflict=$(new_repository conflict-project)
printf 'keep\n' > "$conflict/AGENTS.md"
if AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$conflict" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null 2>&1; then fail 'install overwrote a conflict'; fi
[[ $(cat "$conflict/AGENTS.md") == keep ]] || fail 'conflict changed an existing file'
[[ ! -e $conflict/bin/agentic-loop ]] || fail 'conflict caused a partial copy'

empty=$(empty_repository empty-project)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$empty" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
[[ -f $empty/devbox.json && -f $empty/devbox.lock ]] || fail 'empty repository did not get the pinned development environment'
[[ -x $empty/scripts/check-environment.sh ]] || fail 'empty repository did not get the environment guard'
assert_contains "$empty/README.md" 'opencode' 'installed README.md does not document opencode as a supported provider'
assert_contains "$empty/docs/operations/codebase-diagnosis.md" 'category:improvement' 'installed codebase-diagnosis.md does not document the diagnosis category label'

# README.md is scoped to basic users (Issue #64, ADR 0023): it must not leak
# development/internal content, and must carry the reader's essential links.
for readme_forbidden in 'devbox run --pure' 'make check' 'tests/' 'bin/lib/' 'docs/decisions/' '```toml' 'reasoning_effort' 'worker_timeout_seconds' 'systemctl --user' 'runtime.path' '.git/agentic-loop'; do
  if grep -Fq "$readme_forbidden" "$empty/README.md"; then fail "installed README.md leaked development/internal content: $readme_forbidden"; fi
done
for readme_required in '## できること' '## 導入' '## 要求を出す' '## 日常の操作' '## 困ったときは' '## 文書の案内' 'docs/development.md'; do
  assert_contains "$empty/README.md" "$readme_required" "installed README.md is missing a basic-user element: $readme_required"
done

# Documentation policy (Issue #64, ADR 0023): distributed as shared, referenced
# from AGENTS.md, and docs/development.md is seeded once as init (target-owned).
assert_contains "$empty/docs/policies/documentation.md" '基本利用者' 'installed documentation policy lacks reader/responsibility boundaries'
assert_contains "$empty/AGENTS.md" '[文書ポリシー](docs/policies/documentation.md)' 'installed AGENTS.md does not reference the documentation policy invariant'
[[ -f $empty/docs/development.md ]] || fail 'init did not seed docs/development.md'
assert_contains "$empty/docs/development.md" '要求の伝え方' 'installed docs/development.md is missing the human requirement-submission responsibility'
[[ $(yq -p json -r '.files[] | select(.path == "docs/policies/documentation.md") | .class' "$empty/.agentic-loop/manifest.json") == shared ]] || fail 'manifest did not classify docs/policies/documentation.md as shared'
[[ $(yq -p json -r '.files[] | select(.path == "docs/development.md") | .class' "$empty/.agentic-loop/manifest.json") == init ]] || fail 'manifest did not classify docs/development.md as init (target-owned)'

# init (empty repository) receives this Foundation's own capability manifest
# verbatim: it also received this Foundation's own devbox.json/Makefile/CI, so
# the declarations are facts for it too (see docs/decisions/0018). It must
# fully declare itself (no undetermined items) and never be touched by upgrade.
cmp -s "$PROJECT_ROOT/.agentic-loop/capabilities.toml" "$empty/.agentic-loop/capabilities.toml" || fail 'init did not distribute this capability manifest verbatim'
[[ $(yq -p json -r '.files[] | select(.path == ".agentic-loop/capabilities.toml") | .class' "$empty/.agentic-loop/manifest.json") == init ]] || fail 'manifest did not classify the init-seeded capability manifest as init (target-owned)'
empty_cap_json=$("$empty/bin/agentic-loop" capabilities --format json)
[[ $(yq -p json -r '.valid' <<< "$empty_cap_json") == true ]] || fail 'the distributed capability manifest failed its own validation'
[[ $(yq -p json -r '.data.undetermined | length' <<< "$empty_cap_json") -eq 0 ]] || fail 'the distributed capability manifest left items undetermined for a repository that received this whole Foundation'

# The documented one-command install bootstraps yq through the downloaded
# Devbox definition instead of requiring an unpinned host installation.
bootstrap_bin="$TEST_ROOT/bootstrap-bin"
mkdir -p "$bootstrap_bin"
# mkdir/ln are not part of the yq-bootstrap contract being tested here; they
# are only needed so the fake devbox test double (tests/test-agentic-loop.sh
# itself, not the real devbox binary) can materialize its simulated profile
# directory when invoked under this restricted PATH.
for command_name in bash dirname git devbox mktemp rm mkdir ln; do ln -s "$(command -v "$command_name")" "$bootstrap_bin/$command_name"; done
bootstrap=$(new_repository bootstrap-project)
env PATH="$bootstrap_bin" FAKE_BIN="$FAKE_BIN" TEST_HOST_PATH="$TEST_HOST_PATH" FAKE_GH_ROOT="$FAKE_GH_ROOT" \
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$bootstrap" AGENTIC_LOOP_SKIP_START=1 \
  "$PROJECT_ROOT/install.sh"
assert_contains "$FAKE_GH_ROOT/devbox-calls" "run --config $PROJECT_ROOT -- $PROJECT_ROOT/scripts/install-target.sh $bootstrap" 'install did not bootstrap missing yq through the pinned Devbox environment'
[[ -x $bootstrap/bin/agentic-loop ]] || fail 'Devbox bootstrap did not complete installation'
bootstrap_state="$(git -C "$bootstrap" rev-parse --absolute-git-dir)/agentic-loop"
[[ -s $bootstrap_state/runtime.path ]] || fail 'install did not record the verified runtime PATH'
bootstrap_status=$(env PATH="$bootstrap_bin" FAKE_BIN="$FAKE_BIN" TEST_HOST_PATH="$TEST_HOST_PATH" FAKE_GH_ROOT="$FAKE_GH_ROOT" \
  XDG_CONFIG_HOME="$XDG_CONFIG_HOME" "$bootstrap/bin/agentic-loop" status)
grep -Eq '^(running|stopped)$' <<< "$bootstrap_status" || fail 'installed CLI did not restore its runtime dependencies from an ordinary shell'

# install owns a persistent Devbox virtenv under state_root/runtime (distinct
# from whatever --config directory bootstrapped this very install), and
# records its bin directory first so pinned tools (yq, git) resolve there
# indefinitely instead of through a directory nix GC might reclaim (#150).
bootstrap_runtime_dir="$bootstrap_state/runtime"
bootstrap_profile_bin="$bootstrap_runtime_dir/.devbox/nix/profile/default/bin"
bootstrap_runtime_path=$(cat "$bootstrap_state/runtime.path")
[[ ${bootstrap_runtime_path%%:*} == "$bootstrap_profile_bin" ]] || fail 'runtime.path does not start with the persistent Devbox profile bin directory'
[[ -x $bootstrap_profile_bin/yq ]] || fail 'the persistent Devbox profile did not provide yq'
assert_contains "$FAKE_GH_ROOT/devbox-calls" "run --config $bootstrap_runtime_dir -- true" 'install did not provision the persistent Devbox runtime'

# The persistent runtime must survive even if the (possibly transient) source
# tree install-target.sh itself ran from is deleted after install, since that
# is exactly what the documented curl|bash install does to its mktemp
# extraction directory. Reproduce that by pointing AGENTIC_LOOP_SOURCE at a
# disposable copy of this project, then deleting it once install is done.
ephemeral_source="$TEST_ROOT/ephemeral-source"
mkdir -p "$ephemeral_source"
cp -r "$PROJECT_ROOT/." "$ephemeral_source/"
rm -rf "$ephemeral_source/.git"
git -C "$ephemeral_source" init --quiet
git -C "$ephemeral_source" config user.name Test
git -C "$ephemeral_source" config user.email test@example.invalid
git -C "$ephemeral_source" add -A
git -C "$ephemeral_source" commit --quiet -m seed
ephemeral=$(new_repository ephemeral-project)
env PATH="$bootstrap_bin" FAKE_BIN="$FAKE_BIN" TEST_HOST_PATH="$TEST_HOST_PATH" FAKE_GH_ROOT="$FAKE_GH_ROOT" \
AGENTIC_LOOP_SOURCE="$ephemeral_source" AGENTIC_LOOP_TARGET="$ephemeral" AGENTIC_LOOP_SKIP_START=1 \
  "$PROJECT_ROOT/install.sh"
ephemeral_state="$(git -C "$ephemeral" rev-parse --absolute-git-dir)/agentic-loop"
[[ -s $ephemeral_state/runtime.path ]] || fail 'install did not record runtime.path when bootstrapped from a transient source tree'
if grep -Fq "$ephemeral_source" "$ephemeral_state/runtime.path"; then
  fail 'runtime.path recorded a directory under the transient bootstrap source tree instead of the persistent runtime'
fi
rm -rf "$ephemeral_source"
ephemeral_status=$(env PATH="$bootstrap_bin" FAKE_BIN="$FAKE_BIN" TEST_HOST_PATH="$TEST_HOST_PATH" FAKE_GH_ROOT="$FAKE_GH_ROOT" \
  XDG_CONFIG_HOME="$XDG_CONFIG_HOME" "$ephemeral/bin/agentic-loop" status)
grep -Eq '^(running|stopped)$' <<< "$ephemeral_status" || fail 'installed CLI could not restore yq after its transient bootstrap source tree was deleted'

# An "ordinary shell" has everyday coreutils/git but never installed yq
# directly (that is the whole premise of this Foundation's Devbox pinning);
# model it precisely instead of reusing the narrower bootstrap_bin fixture,
# since the self-heal tests below need real mkdir/ln to still work.
ordinary_bin="$TEST_ROOT/ordinary-bin"
mkdir -p "$ordinary_bin"
ordinary_real_bin=$(command -v yq); ordinary_real_bin=${ordinary_real_bin%/*}
for real_tool in "$ordinary_real_bin"/*; do
  tool_name=${real_tool##*/}
  [[ $tool_name == yq ]] || ln -sf "$real_tool" "$ordinary_bin/$tool_name"
done
for fake_tool in gh devbox codex claude opencode systemctl systemd-escape; do ln -sf "$FAKE_BIN/$fake_tool" "$ordinary_bin/$fake_tool"; done

# Even the persistent, gcroot-protected profile can in principle be destroyed
# by a sufficiently aggressive external nix GC. Simulate that by deleting the
# fake "store" directory the persistent profile symlinks resolve through
# (leaving the profile symlink dangling, exactly like real nix) and confirm
# a single self-heal re-provision recovers it from an ordinary shell.
ephemeral_runtime_dir="$ephemeral_state/runtime"
ephemeral_store_dir="$FAKE_GH_ROOT/devbox-store/${ephemeral_runtime_dir//\//_}"
[[ -d $ephemeral_store_dir ]] || fail 'internal test error: simulated persistent Devbox store not found'
rm -rf "$ephemeral_store_dir"
devbox_calls_before_heal=$(wc -l < "$FAKE_GH_ROOT/devbox-calls")
healed_status=$(env PATH="$ordinary_bin" FAKE_BIN="$FAKE_BIN" TEST_HOST_PATH="$TEST_HOST_PATH" FAKE_GH_ROOT="$FAKE_GH_ROOT" \
  XDG_CONFIG_HOME="$XDG_CONFIG_HOME" "$ephemeral/bin/agentic-loop" status)
grep -Eq '^(running|stopped)$' <<< "$healed_status" || fail 'self-heal did not restore yq after the persistent Devbox runtime was garbage-collected'
tail -n "+$((devbox_calls_before_heal + 1))" "$FAKE_GH_ROOT/devbox-calls" | grep -Fq "run --config $ephemeral_runtime_dir -- true" ||
  fail 'a broken persistent runtime did not trigger a self-heal re-provision attempt'

# When self-heal itself cannot succeed (Devbox fails to reprovision), the CLI
# must fail loudly with recovery guidance rather than a bare "yq is required".
rm -rf "$ephemeral_store_dir"
if env PATH="$ordinary_bin" FAKE_BIN="$FAKE_BIN" TEST_HOST_PATH="$TEST_HOST_PATH" FAKE_GH_ROOT="$FAKE_GH_ROOT" FAKE_DEVBOX_FAIL=1 \
  XDG_CONFIG_HOME="$XDG_CONFIG_HOME" "$ephemeral/bin/agentic-loop" status >"$TEST_ROOT/self-heal-failure.out" 2>&1; then
  fail 'status succeeded despite an unrecoverable persistent Devbox runtime'
fi
assert_contains "$TEST_ROOT/self-heal-failure.out" 'devbox run --config' 'unrecoverable runtime failure did not include a recovery command'

# doctor reports the pinned runtime as healthy right after install, and flags
# it (without modifying anything or crashing) once the profile is severed.
bootstrap_doctor_json=$(env PATH="$ordinary_bin" FAKE_BIN="$FAKE_BIN" TEST_HOST_PATH="$TEST_HOST_PATH" FAKE_GH_ROOT="$FAKE_GH_ROOT" \
  XDG_CONFIG_HOME="$XDG_CONFIG_HOME" "$bootstrap/bin/agentic-loop" doctor --format json || true)
grep -Fq '{"level":"success","name":"固定runtime"' <<< "$bootstrap_doctor_json" || fail 'doctor did not report the pinned runtime as healthy after install'
bootstrap_profile_link="$bootstrap_runtime_dir/.devbox/nix/profile/default"
bootstrap_profile_store="$FAKE_GH_ROOT/devbox-store/${bootstrap_runtime_dir//\//_}"
rm -rf "$bootstrap_profile_store"
[[ -L $bootstrap_profile_link && ! -e $bootstrap_profile_link ]] || fail 'internal test error: profile link is not dangling as expected'
# FAKE_DEVBOX_FAIL=1 keeps this doctor call's own self-heal attempt from
# quietly repairing the profile before doctor_collect inspects it, so the
# failure classification below reflects doctor's own check, not self-heal.
if env PATH="$ordinary_bin" FAKE_BIN="$FAKE_BIN" TEST_HOST_PATH="$TEST_HOST_PATH" FAKE_GH_ROOT="$FAKE_GH_ROOT" FAKE_DEVBOX_FAIL=1 \
  XDG_CONFIG_HOME="$XDG_CONFIG_HOME" "$bootstrap/bin/agentic-loop" doctor >"$TEST_ROOT/severed-doctor.out"; then
  fail 'doctor accepted a severed persistent Devbox profile'
fi
grep -Fq '[失敗] 固定runtime' "$TEST_ROOT/severed-doctor.out" || fail 'doctor did not classify the severed persistent Devbox profile as a failure'

no_devbox_bin="$TEST_ROOT/no-devbox-bin"
mkdir -p "$no_devbox_bin"
for command_name in bash git mktemp rm; do ln -s "$(command -v "$command_name")" "$no_devbox_bin/$command_name"; done
missing_devbox=$(new_repository missing-devbox-project)
if env PATH="$no_devbox_bin" AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$missing_devbox" AGENTIC_LOOP_SKIP_START=1 \
  "$PROJECT_ROOT/install.sh" >"$TEST_ROOT/missing-devbox.out" 2>&1; then
  fail 'install succeeded without either yq or Devbox'
fi
assert_contains "$TEST_ROOT/missing-devbox.out" 'devbox is required to bootstrap' 'missing Devbox error does not explain the bootstrap requirement'

secret_target="$TEST_ROOT/secret-project"
mkdir -p "$secret_target"
git -C "$secret_target" init --quiet
cp "$PROJECT_ROOT/.agentic-loop/guard-secrets.sh" "$secret_target/guard-secrets.sh"
printf 'token=ghp_%s%s\n' '123456789012345678' '901234567890123456' > "$secret_target/leak.txt"
git -C "$secret_target" add leak.txt
if (cd "$secret_target" && ./guard-secrets.sh --staged) >/dev/null 2>&1; then fail 'secret guard accepted a credential-like value'; fi

# --- secret scanning: gitleaks scanner layer (Issue #61, ADR 0024) ---

# A curated PATH with every ordinary tool this Foundation's own Devbox
# profile provides EXCEPT gitleaks, so "unresolved scanner" fixtures below
# cannot accidentally see a real gitleaks through PATH search.
no_gitleaks_bin="$TEST_ROOT/no-gitleaks-bin"
mkdir -p "$no_gitleaks_bin"
no_gitleaks_real_bin=$(command -v yq); no_gitleaks_real_bin=${no_gitleaks_real_bin%/*}
for real_tool in "$no_gitleaks_real_bin"/*; do
  tool_name=${real_tool##*/}
  [[ $tool_name == gitleaks ]] || ln -sf "$real_tool" "$no_gitleaks_bin/$tool_name"
done

gitleaks_target="$TEST_ROOT/gitleaks-project"
mkdir -p "$gitleaks_target/.agentic-loop"
git -C "$gitleaks_target" init --quiet
git -C "$gitleaks_target" config user.email t@example.com; git -C "$gitleaks_target" config user.name t
cp "$PROJECT_ROOT/.agentic-loop/guard-secrets.sh" "$gitleaks_target/.agentic-loop/guard-secrets.sh"
cp "$PROJECT_ROOT/.agentic-loop/gitleaks.toml" "$gitleaks_target/.agentic-loop/gitleaks.toml"
# Derived from a benign seed at runtime (never a Stripe-shaped literal in
# this file) so this very file's own secret guard scan of its tracked-file
# content, and gitleaks' default rules over this repository, never trip on
# the fixture itself; see the pre-existing ghp_ fixture above for the same
# general convention (a non-matching source form that only becomes the real
# shape once written to a throwaway fixture file).
stripe_secret_value="sk_live_$(printf 'stripe-fixture-seed' | sha256sum | cut -c1-48)"
gitleaks_stripe_fixture="stripe_test = \"$stripe_secret_value\""

# Additive coverage: the baseline regex does not know the Stripe token shape,
# so a clean baseline scan of the same content proves the scanner layer (not
# baseline) is what catches it.
if grep -Eq '(AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-(proj-)?[A-Za-z0-9_-]{20,}|BEGIN [A-Z ]*PRIVATE KEY|xox[baprs]-[A-Za-z0-9-]{10,})' <<< "$gitleaks_stripe_fixture"; then
  fail 'internal test error: the Stripe fixture unexpectedly matches the baseline pattern'
fi
printf '%s\n' "$gitleaks_stripe_fixture" > "$gitleaks_target/stripe.txt"
git -C "$gitleaks_target" add stripe.txt
if gitleaks_out=$(cd "$gitleaks_target" && ./.agentic-loop/guard-secrets.sh --staged 2>&1); then
  fail 'secret guard accepted a Stripe-shaped credential the scanner layer should have caught'
fi
grep -Fq 'stripe-access-token' <<< "$gitleaks_out" || fail 'scanner-layer failure did not identify the matched gitleaks rule'
grep -Fq "$stripe_secret_value" <<< "$gitleaks_out" && fail 'scanner-layer failure leaked the raw secret value'
grep -Fq '失効' <<< "$gitleaks_out" || grep -Fiq 'rotate' <<< "$gitleaks_out" || fail 'scanner-layer failure did not include credential-rotation guidance'
git -C "$gitleaks_target" reset --quiet stripe.txt
rm -f "$gitleaks_target/stripe.txt"

# Allowlist: a described, narrow allowlist entry lets the exact fixture value
# through (and only that value; --audit accepts the config unchanged).
printf '[extend]\nuseDefault = true\n\n[[allowlists]]\ndescription = "test fixture value used by tests/test-agentic-loop.sh"\nregexes = ["%s"]\n' \
  "$stripe_secret_value" > "$gitleaks_target/.agentic-loop/gitleaks.toml"
printf '%s\n' "$gitleaks_stripe_fixture" > "$gitleaks_target/stripe.txt"
git -C "$gitleaks_target" add stripe.txt
(cd "$gitleaks_target" && ./.agentic-loop/guard-secrets.sh --staged) || fail 'a described, narrow allowlist entry did not permit its exact fixture value'
(cd "$gitleaks_target" && ./.agentic-loop/guard-secrets.sh --audit) || fail 'a valid, narrow allowlist entry failed --audit'
git -C "$gitleaks_target" reset --quiet stripe.txt
rm -f "$gitleaks_target/stripe.txt"

# --audit governance: every rejection reason is enforced independently.
audit_reject() {
  local label=$1 config=$2
  printf '%s' "$config" > "$gitleaks_target/.agentic-loop/gitleaks.toml"
  if (cd "$gitleaks_target" && ./.agentic-loop/guard-secrets.sh --audit) >/dev/null 2>&1; then
    fail "gitleaks config audit accepted: $label"
  fi
}
audit_reject 'missing description' '[extend]
useDefault = true

[[allowlists]]
regexes = ["abc"]
'
audit_reject 'overly broad pattern' '[extend]
useDefault = true

[[allowlists]]
description = "too broad"
regexes = [".*"]
'
audit_reject 'useDefault disabled' '[extend]
useDefault = false
'
too_many_entries='[extend]
useDefault = true
'
for n in 1 2 3 4 5 6 7 8 9; do
  too_many_entries+=$'\n[[allowlists]]\ndescription = "entry '"$n"'"\nregexes = ["FIXTURE_TOKEN_'"$n"'"]\n'
done
audit_reject 'more than 8 allowlist entries' "$too_many_entries"
printf '[extend]\nuseDefault = true\n' > "$gitleaks_target/.agentic-loop/gitleaks.toml"
printf 'do not use fingerprint ignores\n' > "$gitleaks_target/.gitleaksignore"
(cd "$gitleaks_target" && ./.agentic-loop/guard-secrets.sh --audit) >/dev/null 2>&1 && fail 'gitleaks config audit accepted a repository with .gitleaksignore present'
rm -f "$gitleaks_target/.gitleaksignore"
cp "$PROJECT_ROOT/.agentic-loop/gitleaks.toml" "$gitleaks_target/.agentic-loop/gitleaks.toml"
(cd "$gitleaks_target" && ./.agentic-loop/guard-secrets.sh --audit) || fail 'internal test error: the restored gitleaks.toml should pass --audit'

# --history reaches secrets already removed from the working tree; --all (and
# --staged/--push) only ever see the current tree, so a secret committed and
# then deleted is invisible to --all but must still be caught by --history.
printf 'clean\n' > "$gitleaks_target/clean.txt"
git -C "$gitleaks_target" add clean.txt
git -C "$gitleaks_target" commit --quiet -m 'clean commit'
printf '%s\n' "$gitleaks_stripe_fixture" > "$gitleaks_target/history-secret.txt"
git -C "$gitleaks_target" add history-secret.txt
git -C "$gitleaks_target" commit --quiet -m 'secret commit'
git -C "$gitleaks_target" rm --quiet history-secret.txt
git -C "$gitleaks_target" commit --quiet -m 'remove secret'
(cd "$gitleaks_target" && ./.agentic-loop/guard-secrets.sh --all) || fail '--all must not see a secret already removed from the working tree'
if (cd "$gitleaks_target" && ./.agentic-loop/guard-secrets.sh --history) >/dev/null 2>&1; then
  fail '--history did not find a secret still present in Git history'
fi

# fail-closed vs. degrade: with the scanner unresolved, the pinned-environment
# marker (or AGENTIC_LOOP_SECRET_SCAN=required) must block even harmless
# input, while its absence must degrade to baseline-only with a warning
# (never a silent pass with no diagnostic).
harmless_target="$TEST_ROOT/harmless-project"
mkdir -p "$harmless_target/.agentic-loop"
git -C "$harmless_target" init --quiet
git -C "$harmless_target" config user.email t@example.com; git -C "$harmless_target" config user.name t
cp "$PROJECT_ROOT/.agentic-loop/guard-secrets.sh" "$harmless_target/.agentic-loop/guard-secrets.sh"
cp "$PROJECT_ROOT/.agentic-loop/gitleaks.toml" "$harmless_target/.agentic-loop/gitleaks.toml"
printf 'nothing secret here\n' > "$harmless_target/a.txt"
git -C "$harmless_target" add a.txt

if env PATH="$no_gitleaks_bin" AGENTIC_LOOP_GITLEAKS=/nonexistent/gitleaks AGENTIC_LOOP_SECRET_SCAN=required \
  bash -c 'cd "$0" && ./.agentic-loop/guard-secrets.sh --staged' "$harmless_target" >/dev/null 2>&1; then
  fail 'AGENTIC_LOOP_SECRET_SCAN=required did not fail-close when gitleaks was unresolved'
fi
degrade_out=$(env -u DEV_ENVIRONMENT PATH="$no_gitleaks_bin" AGENTIC_LOOP_GITLEAKS=/nonexistent/gitleaks \
  bash -c 'cd "$0" && ./.agentic-loop/guard-secrets.sh --staged' "$harmless_target" 2>&1) ||
  fail 'an unresolved scanner outside the pinned environment must degrade to baseline-only, not fail'
grep -Fq 'degraded' <<< "$degrade_out" || fail 'a degraded secret scan did not warn that gitleaks was unresolved'

# --- repository capability manifest (Issue #56, ADR 0018) ---

# $target (an "existing repository" install: new_repository seeds a tracked
# file, so install-target.sh takes the non-init branch) got a detection-only
# seed. .githooks/pre-commit and .agentic-loop/guard-secrets.sh are always
# distributed as shared files, so fast_check/secret_guard are detected; this
# fixture has no devbox.json/Makefile, so full_check is left undetermined
# instead of guessed (never fabricated).
cap_json=$("$target/bin/agentic-loop" capabilities --format json)
[[ $(yq -p json -r '.installed' <<< "$cap_json") == true ]] || fail 'capabilities did not report an installed manifest for an existing-repository install'
[[ $(yq -p json -r '.valid' <<< "$cap_json") == true ]] || fail 'capabilities reported an invalid manifest for a detection-only seed'
[[ $(yq -p json -r '.data.validation.fast_check' <<< "$cap_json") == '.githooks/pre-commit' ]] || fail 'capabilities did not detect the fast check entry point'
[[ $(yq -p json -r '.data.validation.secret_guard' <<< "$cap_json") == '.agentic-loop/guard-secrets.sh' ]] || fail 'capabilities did not detect the secret guard entry point'
grep -Fxq 'validation.full_check' <(yq -p json -r '.data.undetermined[]' <<< "$cap_json") || fail 'capabilities fabricated a full_check value for a repository without Devbox or Make'
[[ $(yq -p json -o yaml '.files[] | select(.path == ".agentic-loop/capabilities.toml") | .class' "$target/.agentic-loop/manifest.json") == init ]] || fail 'manifest did not classify the generated capability manifest as init (target-owned)'
doctor_cap_out=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '[成功] 能力manifest' <<< "$doctor_cap_out" || fail 'doctor did not report the detection-only capability manifest as healthy'

# Unsafe declarations (workspace escape, shell metacharacters) are rejected by
# the exact same shared validator doctor/capabilities/lint all call, and no
# command from an unsafe manifest is ever executed -- capability.sh only ever
# tokenizes and pattern-matches command strings, never eval's or shells out to
# them (see ADR 0018). Exercised by sourcing the module directly since this
# fixture is not itself an installed Foundation checkout.
unsafe_target="$TEST_ROOT/capability-unsafe"
mkdir -p "$unsafe_target/.agentic-loop"
pwned_marker="$TEST_ROOT/capability-unsafe-pwned-marker"
rm -f "$pwned_marker"
cat > "$unsafe_target/.agentic-loop/capabilities.toml" <<EOF
schema_version = 1
undetermined = []
[validation]
full_check = "make check; touch $pwned_marker"
[[protected]]
path = "../outside"
reason = "x"
change_requires = "approval"
EOF
cap_rc=0
( source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"; source "$PROJECT_ROOT/bin/lib/agentic-loop/capability.sh"; capability_validate "$unsafe_target" ) || cap_rc=$?
[[ $cap_rc -ne 0 ]] || fail 'capability_validate accepted an unsafe path/command manifest'
[[ ! -e $pwned_marker ]] || fail 'an unsafe capabilities.toml command was actually executed'

# An unknown (or missing) schema_version fails closed: no implicit fallback
# to defaults.
schema_target="$TEST_ROOT/capability-schema"
mkdir -p "$schema_target/.agentic-loop"
printf 'schema_version = 99\n' > "$schema_target/.agentic-loop/capabilities.toml"
cap_rc=0
( source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"; source "$PROJECT_ROOT/bin/lib/agentic-loop/capability.sh"; capability_validate "$schema_target" ) || cap_rc=$?
[[ $cap_rc -ne 0 ]] || fail 'capability_validate accepted an unknown schema_version instead of failing closed'

# --- local affected check (Issue #59, ADR 0021, docs/operations/affected-checks.md) ---
affected_sh="$PROJECT_ROOT/scripts/affected-check.sh"
real_impact_map="$PROJECT_ROOT/tests/impact-map.toml"

affected_plan() {
  printf '%s\n' "$1" | "$affected_sh" --files - --print-plan --format json
}

# 独立component: 一致したruleのunitsだけが選択され、拡大しない。
plan_json=$(affected_plan 'bin/lib/agentic-loop/scope.sh')
[[ $(yq -p json -r '.selected_units | join(",")' <<< "$plan_json") == 'e2e:queue' ]] || fail 'affected-check did not narrow an independent component change to its own unit'
[[ $(yq -p json -r '.widened' <<< "$plan_json") == false ]] || fail 'affected-check widened an independent component change'

# 共有依存: 常に全単位へ拡大する。
plan_json=$(affected_plan 'bin/lib/agentic-loop/common.sh')
[[ $(yq -p json -r '.widened' <<< "$plan_json") == true ]] || fail 'affected-check did not widen a shared-dependency change'
[[ $(yq -p json -r '.widen_reasons | join(",")' <<< "$plan_json") == 'shared-dependency' ]] || fail 'affected-check reported the wrong widen reason for a shared dependency'
[[ $(yq -p json -r '.selected_units | length' <<< "$plan_json") == 4 ]] || fail 'affected-check did not select all units for a shared-dependency change'

# 設定変更: build/runtime設定は常に拡大する。
plan_json=$(affected_plan '.agentic-loop.toml')
[[ $(yq -p json -r '.widened' <<< "$plan_json") == true ]] || fail 'affected-check did not widen a runtime-config change'
[[ $(yq -p json -r '.widen_reasons | join(",")' <<< "$plan_json") == 'runtime-config' ]] || fail 'affected-check reported the wrong widen reason for a runtime-config change'

# 未知file: unmatchedへ計上した上で拡大する。
plan_json=$(affected_plan 'services/unknown/main.go')
[[ $(yq -p json -r '.widened' <<< "$plan_json") == true ]] || fail 'affected-check did not widen an unmatched change'
[[ $(yq -p json -r '.unmatched | join(",")' <<< "$plan_json") == 'services/unknown/main.go' ]] || fail 'affected-check did not record the unmatched path'
[[ $(yq -p json -r '.widen_reasons | join(",")' <<< "$plan_json") == 'unmatched' ]] || fail 'affected-check reported the wrong widen reason for an unmatched change'

# test基盤自体の変更: diff行推定はせず常に拡大する。
plan_json=$(affected_plan 'tests/test-agentic-loop.sh')
[[ $(yq -p json -r '.widened' <<< "$plan_json") == true ]] || fail 'affected-check did not widen a test-harness change'
[[ $(yq -p json -r '.widen_reasons | join(",")' <<< "$plan_json") == 'test-harness' ]] || fail 'affected-check reported the wrong widen reason for a test-harness change'

# docs専用変更: E2E単位を1つも選ばない（environment/lintは常時実行のため対象外）。
plan_json=$(affected_plan 'docs/policies/testing.md')
[[ $(yq -p json -r '.selected_units | length' <<< "$plan_json") == 0 ]] || fail 'affected-check selected an E2E unit for a docs-only change'
[[ $(yq -p json -r '.widened' <<< "$plan_json") == false ]] || fail 'affected-check widened a docs-only change'

# 除外用のinterfaceは存在しない: 未知optionは終了code 2で拒否する。
affected_rc=0
"$affected_sh" --exclude e2e:queue >/dev/null 2>&1 || affected_rc=$?
[[ $affected_rc -eq 2 ]] || fail 'affected-check.sh did not reject --exclude with exit code 2'
grep -Eiq 'flaky|skip-group|known-failure' "$affected_sh" && fail 'affected-check.sh source mentions a flaky/skip-based exclusion token'
grep -Fq -- '--exclude)' "$affected_sh" && fail 'affected-check.sh source implements an --exclude case branch'

# 実repositoryのimpact-map.tomlはaudit自体を通過する(schema/到達可能性/path実在/全fileの明示的分類/full check同等性)。
"$affected_sh" --audit >/dev/null || fail 'affected-check.sh --audit failed against the real tests/impact-map.toml'

# --audit は壊れたmapを、それぞれ異なる理由で検出する。
map_root="$TEST_ROOT/impact-map"
mkdir -p "$map_root"

cat > "$map_root/mismatch.toml" <<'EOF'
schema_version = 1
units = ["e2e:queue", "e2e:lifecycle"]
always = ["environment", "lint"]
[[rule]]
path = "bin"
units = ["e2e:queue"]
reason = "x"
[[rule]]
path = "bin/lib/agentic-loop"
units = ["e2e:lifecycle"]
reason = "x"
EOF
audit_out=$("$affected_sh" --audit --map "$map_root/mismatch.toml" 2>&1) && fail 'affected-audit accepted a unit-set mismatched against run-e2e.sh/test-agentic-loop.sh'
assert_contains <(printf '%s' "$audit_out") 'と一致しません' 'affected-audit did not report the unit-set mismatch'

cat > "$map_root/orphan.toml" <<'EOF'
schema_version = 1
units = ["e2e:queue", "e2e:lifecycle", "e2e:auxiliary", "e2e:upgrade"]
always = ["environment", "lint"]
[[rule]]
path = "bin"
units = ["e2e:queue", "e2e:lifecycle", "e2e:auxiliary"]
reason = "x"
EOF
audit_out=$("$affected_sh" --audit --map "$map_root/orphan.toml" 2>&1) && fail 'affected-audit accepted a map with an unreachable (orphan) unit'
assert_contains <(printf '%s' "$audit_out") '孤立群' 'affected-audit did not report the orphan unit'

cat > "$map_root/nonexistent.toml" <<'EOF'
schema_version = 1
units = ["e2e:queue", "e2e:lifecycle", "e2e:auxiliary", "e2e:upgrade"]
always = ["environment", "lint"]
[[rule]]
path = "bin/lib/agentic-loop/scope.sh"
units = ["e2e:queue"]
reason = "x"
[[rule]]
path = "bin/lib/agentic-loop/worker.sh"
units = ["e2e:lifecycle"]
reason = "x"
[[rule]]
path = "bin/lib/agentic-loop/preflight.sh"
units = ["e2e:auxiliary"]
reason = "x"
[[rule]]
path = "bin/lib/agentic-loop/upgrade.sh"
units = ["e2e:upgrade"]
reason = "x"
[[rule]]
path = "this/path/does/not/exist.sh"
units = ["e2e:queue"]
reason = "x"
EOF
audit_out=$("$affected_sh" --audit --map "$map_root/nonexistent.toml" 2>&1) && fail 'affected-audit accepted a map referencing a nonexistent path'
assert_contains <(printf '%s' "$audit_out") '存在しないpathを参照しています' 'affected-audit did not report the nonexistent rule path'

cat > "$map_root/unclassified.toml" <<'EOF'
schema_version = 1
units = ["e2e:queue", "e2e:lifecycle", "e2e:auxiliary", "e2e:upgrade"]
always = ["environment", "lint"]
[[rule]]
path = "bin/lib/agentic-loop/scope.sh"
units = ["e2e:queue"]
reason = "x"
[[rule]]
path = "bin/lib/agentic-loop/worker.sh"
units = ["e2e:lifecycle"]
reason = "x"
[[rule]]
path = "bin/lib/agentic-loop/preflight.sh"
units = ["e2e:auxiliary"]
reason = "x"
[[rule]]
path = "bin/lib/agentic-loop/upgrade.sh"
units = ["e2e:upgrade"]
reason = "x"
EOF
audit_out=$("$affected_sh" --audit --map "$map_root/unclassified.toml" 2>&1) && fail 'affected-audit accepted a map that leaves tracked files unclassified'
assert_contains <(printf '%s' "$audit_out") 'unmatchedのまま' 'affected-audit did not report an unclassified tracked file'
[[ -f $real_impact_map ]] || fail 'the real tests/impact-map.toml went missing during the affected-check fixtures'

# capability manifestとの連携: Makefileにaffected: targetがあり impact map が
# 実在すればcapability_generateが検出し、無ければundeterminedへ落とす。
affected_cap_with="$TEST_ROOT/capability-affected-with"
mkdir -p "$affected_cap_with/tests"
printf 'check:\n\t/bin/true\naffected:\n\t/bin/true\n' > "$affected_cap_with/Makefile"
printf 'schema_version = 1\n' > "$affected_cap_with/tests/impact-map.toml"
( source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"; source "$PROJECT_ROOT/bin/lib/agentic-loop/capability.sh"; capability_generate "$affected_cap_with" )
grep -Fq 'affected_check = "devbox run --pure affected"' "$affected_cap_with/.agentic-loop/capabilities.toml" || fail 'capability_generate did not detect validation.affected_check'
grep -Fq 'impact_map = "tests/impact-map.toml"' "$affected_cap_with/.agentic-loop/capabilities.toml" || fail 'capability_generate did not detect validation.impact_map'

affected_cap_without="$TEST_ROOT/capability-affected-without"
mkdir -p "$affected_cap_without"
printf 'check:\n\t/bin/true\n' > "$affected_cap_without/Makefile"
( source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"; source "$PROJECT_ROOT/bin/lib/agentic-loop/capability.sh"; capability_generate "$affected_cap_without" )
grep -Fxq 'validation.affected_check' <(yq -p toml -r '.undetermined[]' "$affected_cap_without/.agentic-loop/capabilities.toml") || fail 'capability_generate fabricated validation.affected_check without a Makefile affected: target'
grep -Fxq 'validation.impact_map' <(yq -p toml -r '.undetermined[]' "$affected_cap_without/.agentic-loop/capabilities.toml") || fail 'capability_generate fabricated validation.impact_map without an impact map file'

# Worker prompt capability rendering is declaration-driven: affected details
# appear only when both values are present in a valid manifest (Issue #266).
summary_with=$(source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"; source "$PROJECT_ROOT/bin/lib/agentic-loop/capability.sh"; capability_summary_block "$affected_cap_with")
grep -Fq '影響範囲検証（反復の短縮専用、push/merge gateではない）' <<< "$summary_with" || fail 'capability summary omitted a declared affected check'
grep -Fq '影響範囲map: tests/impact-map.toml' <<< "$summary_with" || fail 'capability summary omitted a declared impact map'
summary_without=$(source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"; source "$PROJECT_ROOT/bin/lib/agentic-loop/capability.sh"; capability_summary_block "$affected_cap_without")
if grep -Fq '影響範囲検証（反復の短縮専用、push/merge gateではない）' <<< "$summary_without" || grep -Fq '影響範囲map:' <<< "$summary_without"; then
  fail 'capability summary fabricated undeclared affected details'
fi

# worker_time_budget_block (Issue #266): observed, not provider-claimed, and
# opt-in via queue.worker_timeout_seconds. Absent/zero timeout or a missing/
# unreadable started marker must render nothing (never a fabricated budget).
time_budget_root="$TEST_ROOT/worker-time-budget"
mkdir -p "$time_budget_root/workers"
date +%s > "$time_budget_root/workers/9001.started"
time_budget_disabled=$(STATE_ROOT="$time_budget_root" WORKER_TIMEOUT_SECONDS=0 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/worker_state.sh"; source "$0/bin/lib/agentic-loop/worker.sh"; worker_time_budget_block 9001' "$PROJECT_ROOT")
[[ -z $time_budget_disabled ]] || fail 'worker_time_budget_block rendered a budget with worker_timeout_seconds=0'
time_budget_no_started=$(STATE_ROOT="$time_budget_root" WORKER_TIMEOUT_SECONDS=300 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/worker_state.sh"; source "$0/bin/lib/agentic-loop/worker.sh"; worker_time_budget_block 9002' "$PROJECT_ROOT")
[[ -z $time_budget_no_started ]] || fail 'worker_time_budget_block rendered a budget without a started marker'
time_budget_enabled=$(STATE_ROOT="$time_budget_root" WORKER_TIMEOUT_SECONDS=300 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/worker_state.sh"; source "$0/bin/lib/agentic-loop/worker.sh"; worker_time_budget_block 9001' "$PROJECT_ROOT")
grep -Fq 'このworkerの実行時間予算: 上限=300秒' <<< "$time_budget_enabled" || fail 'worker_time_budget_block did not render an observed budget when enabled'

# drift検出: affected_check_secondsがfull_check_seconds以上になると警告する
# (bin/lib/agentic-loop/doctor.shはdrift:*をwildcardで既に処理する)。
drift_target="$TEST_ROOT/capability-affected-drift"
mkdir -p "$drift_target/.agentic-loop"
cat > "$drift_target/.agentic-loop/capabilities.toml" <<'EOF'
schema_version = 1
undetermined = []
[validation]
full_check = "make check"
affected_check_seconds = 999
full_check_seconds = 300
EOF
cap_rc=0
drift_findings=$(
  source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"
  source "$PROJECT_ROOT/bin/lib/agentic-loop/capability.sh"
  capability_validate "$drift_target" || true
  for cap_i in "${!CAPABILITY_CODES[@]}"; do printf '%s\n' "${CAPABILITY_CODES[$cap_i]}"; done
)
grep -Fxq 'drift:affected_check_seconds' <<< "$drift_findings" || fail 'capability_validate did not warn when affected_check_seconds >= full_check_seconds'

# --- stall calibration primitives (Issue #280, ADR 0032) -------------------
# metrics_calibration_verdict is a pure function; test it directly rather than
# only indirectly through a fleet-wide fixture below.
calib_verdict_ok=$(CALIBRATION_MIN_SAMPLES=20 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/metrics.sh"; metrics_calibration_verdict "$1" "$2"' \
  "$PROJECT_ROOT" $'25\t100\t100\t100\t200' 300)
[[ $calib_verdict_ok == ok ]] || fail 'metrics_calibration_verdict misclassified a healthy distribution as not ok'
calib_verdict_under=$(CALIBRATION_MIN_SAMPLES=20 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/metrics.sh"; metrics_calibration_verdict "$1" "$2"' \
  "$PROJECT_ROOT" $'25\t3600\t3600\t4000\t5000' 3600)
[[ $calib_verdict_under == undersized ]] || fail 'metrics_calibration_verdict did not flag a threshold at or below the observed p90 as undersized'
calib_verdict_sparse=$(CALIBRATION_MIN_SAMPLES=20 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/metrics.sh"; metrics_calibration_verdict "$1" "$2"' \
  "$PROJECT_ROOT" $'5\t100\t100\t100\t100' 300)
[[ $calib_verdict_sparse == insufficient-samples ]] || fail 'metrics_calibration_verdict did not flag too few samples as insufficient-samples'
calib_verdict_disabled=$(CALIBRATION_MIN_SAMPLES=20 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/metrics.sh"; metrics_calibration_verdict "$1" "$2"' \
  "$PROJECT_ROOT" $'25\t9999\t9999\t9999\t9999' 0)
[[ $calib_verdict_disabled == ok ]] || fail 'metrics_calibration_verdict did not treat threshold=0 (disabled band) as ok'

# events_stage_duration_samples reconstructs stage durations from real
# progress transitions (no fabricated epoch): the departing stage of each
# consecutive same-subject progress pair is sampled, the final stage of a
# subject is never sampled, and a gap beyond worker_timeout_seconds is
# dropped as host/timeout-attributable rather than a natural stage duration.
events_calib_log="$TEST_ROOT/events-calibration.log"
printf '1000\t42\tprogress\tplan\n2000\t42\tprogress\texec\n2100\t42\tprogress\tchecks\n20000\t42\tprogress\tdone\n' > "$events_calib_log"
events_calib_out=$(EVENTS_LOG="$events_calib_log" EVENTS_CALIBRATION_MAX_LINES=2000 WORKER_TIMEOUT_SECONDS=5000 bash -c \
  'source "$0/bin/lib/agentic-loop/common.sh"; source "$0/bin/lib/agentic-loop/worker_state.sh"; events_stage_duration_samples' "$PROJECT_ROOT")
[[ $(grep -c '^provider	' <<< "$events_calib_out") -eq 2 ]] || fail 'events_stage_duration_samples did not attribute the plan/exec departures to the provider band'
grep -Fxq $'provider\t1000' <<< "$events_calib_out" || fail 'events_stage_duration_samples miscalculated the plan-stage duration'
grep -Fxq $'provider\t100' <<< "$events_calib_out" || fail 'events_stage_duration_samples miscalculated the exec-stage duration'
[[ $(grep -c '^non-provider	' <<< "$events_calib_out") -eq 0 ]] || fail 'events_stage_duration_samples produced a sample for a gap beyond worker_timeout_seconds'

# --- bin/agentic-loop metrics (see docs/decisions/0007, docs/operations/loop-metrics.md) ---
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
metrics_issues="$FAKE_GH_ROOT/$state_key.metrics-issues"
metrics_events="$FAKE_GH_ROOT/$state_key.metrics-events"
metrics_pulls="$FAKE_GH_ROOT/$state_key.metrics-pulls"
rm -f "$metrics_issues" "$metrics_events" "$metrics_pulls"

as_of=1700000000
days=30
window_start=$((as_of - days * 86400))
t0=$((window_start + 100000))   # 501: normal completion, plus a merged PR
t0b=$((window_start + 110000))  # 502: transient failure then retry then completion
t0c=$((window_start + 120000))  # 503: needs-input left open, plus an unmerged PR
t0d=$((window_start + 130000))  # 504: worker-timeout, retry, then a silent-crash recovery
t0e=$((window_start + 140000))  # 505: closed+completed with zero marker history
t0f=$((window_start + 150000))  # 506: claimed but never closed (still running)
t0g=$((window_start + 160000))  # 507: closed with a stale agent:running label and no marker (mismatch)
t0h=$((window_start + 170000))  # 508: closed with a stale agent:running label but a real declined marker
t0i=$((window_start + 180000))  # 509: scope-conflict left open
t0j=$((window_start + 190000))  # 510: dependency-blocked left open
t0k=$((window_start + 200000))  # 511: needs-input answered, then completed

t0l=$((window_start + 210000)) # 512: closed+completed with priority 0 (unset; jq TSV null-field regression)
t0m=$((window_start + 220000)) # 513: open, but its only lease event and its only PR postdate --as-of (out-of-window event/PR regression)
t0n=$((window_start + 230000)) # 514: retry-budget exhausted and parked (open, non-claim; see docs/decisions/0016), never closed

cat > "$metrics_issues" <<TSV
501	$t0	$((t0 + 400))	closed	category:feature	75	agent:completed
502	$t0b	$((t0b + 500))	closed	category:improvement	50	agent:completed
503	$t0c	0	open	category:improvement	25	agent:needs-input
504	$t0d	0	open	category:feature	75	agent:running
505	$t0e	$((t0e + 50))	closed	category:improvement	50	agent:completed
506	$t0f	0	open	category:improvement	25	agent:running
507	$t0g	$((t0g + 30))	closed	category:feature	75	agent:running
508	$t0h	$((t0h + 100))	closed	category:feature	75	agent:running
509	$t0i	0	open	category:improvement	25	agent:queued
510	$t0j	0	open	category:improvement	25	agent:blocked
511	$t0k	$((t0k + 7300))	closed	category:feature	50	agent:completed
512	$t0l	$((t0l + 40))	closed	category:feature	0	agent:completed
513	$t0m	0	open	category:improvement	25	agent:queued
514	$t0n	0	open	category:improvement	25	agent:parked
TSV

cat > "$metrics_events" <<TSV
501	$((t0 + 100))	lease worker=w
501	$((t0 + 150))	usage worker=w stage=plan provider=codex seconds=50 exit=0
501	$((t0 + 300))	usage worker=w stage=exec provider=codex seconds=120 exit=0
501	$((t0 + 310))	handoff schema=1 phase=fresh branch=agent/issue-501 head=none remote=none pr=none pr_state=none checks=none dirty=0 diverged=0 updated=$((t0 + 310))
501	$((t0 + 400))	completed pr=https://github.example/acme/x/pull/9
502	$((t0b + 10))	lease worker=w
502	$((t0b + 200))	failed worker=w exit=1
502	$((t0b + 250))	retry attempt=2
502	$((t0b + 300))	lease worker=w
502	$((t0b + 500))	completed pr=https://github.example/acme/x/pull/10
503	$((t0c + 10))	lease worker=w
503	$((t0c + 500))	needs-input worker=w
504	$((t0d + 10))	lease worker=w
504	$((t0d + 900))	worker-timeout issue=504 elapsed=890s limit=14400s
504	$((t0d + 950))	retry attempt=1
504	$((t0d + 1000))	lease worker=w
504	$((t0d + 1400))	recovered
504	$((t0d + 1450))	lease worker=w
506	$((t0f + 10))	lease worker=w
508	$((t0h + 50))	declined worker=w
509	$((t0i + 10))	scope-conflict issue=999 token=path:bin
510	$((t0j + 10))	dependency-blocked reason=unresolved
511	$((t0k + 5))	lease worker=w
511	$((t0k + 10))	needs-input worker=w
511	$((t0k + 7210))	answer-detected
511	$((t0k + 7250))	lease worker=w
511	$((t0k + 7300))	completed pr=https://github.example/acme/x/pull/11
513	$((as_of + 100))	lease worker=w
514	$((t0n + 10))	lease worker=w
514	$((t0n + 200))	failed worker=w reason=merge-or-cleanup
514	$((t0n + 250))	parked attempts=3 reason=retry-exhausted
TSV

cat > "$metrics_pulls" <<TSV
9	$((t0 + 120))	$((t0 + 400))	agent/issue-501
10	$((t0c + 20))	0	agent/issue-503
11	$((t0k + 20))	$((t0k + 7300))	agent/issue-511
12	$((as_of + 20))	0	agent/issue-513
TSV

metrics_json=$("$target/bin/agentic-loop" metrics --format json --days "$days" --as-of "$as_of")
printf '%s' "$metrics_json" | yq -p json -o json >/dev/null || fail 'metrics --format json did not produce valid JSON'
mj() { printf '%s' "$metrics_json" | yq -p json "$1"; }

[[ $(mj '.schema_version') == 1 ]] || fail 'metrics --format json did not report schema_version 1'
[[ $(mj '.github_available') == true ]] || fail 'metrics --format json did not report github_available'
[[ $(mj '.window.start') -eq $window_start && $(mj '.window.end') -eq $as_of && $(mj '.window.days') -eq $days ]] || fail 'metrics --format json computed the wrong window'

# Dispositions: label is authoritative except when a `declined` marker is
# present (the worker never relabels on decline), which must still resolve to
# "declined" rather than being misread from the stale agent:running label.
[[ $(mj '.dispositions.completed') -eq 5 ]] || fail 'metrics miscounted completed dispositions (501, 502, 505 by label alone despite zero marker history, 511, 512 with priority 0)'
[[ $(mj '.dispositions.declined') -eq 1 ]] || fail 'metrics did not detect a declined Issue behind a stale agent:running label'
[[ $(mj '.dispositions.other') -eq 1 ]] || fail 'metrics did not flag the genuinely unclassifiable closed Issue (507) as other, or an unset priority on 512 shifted a later TSV column and corrupted its agent-label disposition'
[[ $(mj '.dispositions.open') -eq 6 ]] || fail 'metrics miscounted still-open Issues'
[[ $(mj '.dispositions.parked') -eq 1 ]] || fail 'metrics did not classify the open agent:parked Issue (514) as parked, separately from open'
[[ $(mj '.dispositions.unresolved') -eq 0 && $(mj '.dispositions.stale') -eq 0 ]] || fail 'metrics fabricated an unresolved or stale disposition'
[[ $(mj '.warnings[]') == *label_marker_mismatch* ]] || fail 'metrics did not warn about the label/marker mismatch'

[[ $(mj '.counters.attempts') -eq 11 ]] || fail 'metrics miscounted attempts (one per lease marker, including 514; the lease for Issue 513 postdates --as-of and must not count)'
[[ $(mj '.counters.retry') -eq 2 && $(mj '.counters.recovered') -eq 1 && $(mj '.counters.worker_timeout') -eq 1 ]] || fail 'metrics miscounted the retry/recovered/worker-timeout requeue path (Issue 504)'
[[ $(mj '.counters.scope_conflict') -eq 1 && $(mj '.counters.dependency_block') -eq 1 ]] || fail 'metrics miscounted the still-open scope-conflict/dependency-blocked waits (509, 510)'
[[ $(mj '.counters.needs_input_round') -eq 2 ]] || fail 'metrics miscounted needs-input rounds (503 open, 511 answered)'
[[ $(mj '.counters.requeue') -eq 4 ]] || fail 'metrics miscounted total requeues (2 retry + 1 recovered + 1 answer-detected)'
[[ $(mj '.counters.open_attempts') -eq 2 ]] || fail 'metrics miscounted attempts with no terminal marker yet (504 attempt 3, 506; an out-of-window lease must not open an attempt for 513; 514'"'"'s attempt is closed by its failed marker before parking)'
[[ $(mj '.counters.unmerged_pr') -eq 1 ]] || fail 'metrics miscounted unmerged pull requests (PR 10; PR 12 postdates --as-of and must not count)'
[[ $(mj '.counters.exhausted') -eq 0 && $(mj '.counters.replan') -eq 0 && $(mj '.counters.resume') -eq 0 ]] || fail 'metrics fabricated an exhausted, replan, or resume count'
[[ $(mj '.counters.parked') -eq 1 ]] || fail 'metrics did not count the park of retry-exhausted Issue 514'

[[ $(mj '.failures.unspecified') -eq 1 ]] || fail 'metrics did not classify the reasonless failed marker (502) as unspecified'
[[ $(mj '."failures"."worker-timeout"') -eq 1 ]] || fail 'metrics did not tally the worker-timeout failure reason (504)'
[[ $(mj '."failures"."merge-or-cleanup"') -eq 1 ]] || fail 'metrics did not tally the merge-or-cleanup failure reason (514, before it was parked)'

[[ $(mj '.durations.queue_wait.n') -eq 11 && $(mj '.durations.queue_wait.max') -eq 100 ]] || fail 'metrics computed the wrong queue_wait distribution'
[[ $(mj '.durations.attempt_duration.n') -eq 9 && $(mj '.durations.attempt_duration.max') -eq 890 ]] || fail 'metrics computed the wrong attempt_duration distribution'
[[ $(mj '.durations.open_queue_wait.n') -eq 0 ]] || fail 'metrics fabricated an open_queue_wait sample'
[[ $(mj '.durations.open_needs_input_wait.n') -eq 1 ]] || fail 'metrics did not report the still-open needs-input wait (503)'
[[ $(mj '.durations.open_conflict_wait.n') -eq 1 ]] || fail 'metrics did not report the still-open scope-conflict wait (509)'
[[ $(mj '.durations.open_dependency_wait.n') -eq 1 ]] || fail 'metrics did not report the still-open dependency-blocked wait (510)'
[[ $(mj '.durations.needs_input_wait.n') -eq 1 && $(mj '.durations.needs_input_wait.max') -eq 7200 ]] || fail 'metrics computed the wrong closed needs_input_wait sample (511: 7200s)'
[[ $(mj '.durations.plan_seconds.n') -eq 1 && $(mj '.durations.plan_seconds.max') -eq 50 ]] || fail 'metrics did not read plan_seconds from the enriched usage marker'
[[ $(mj '.durations.exec_seconds.n') -eq 1 && $(mj '.durations.exec_seconds.max') -eq 120 ]] || fail 'metrics did not read exec_seconds from the enriched usage marker'

# stall_calibration (Issue #280, ADR 0032): this fixture's plan/exec sample
# counts (1 each) are far below CALIBRATION_MIN_SAMPLES, so the verdict must
# stay insufficient-samples rather than fabricate a judgement from one point.
[[ $(mj '.stall_calibration.verdict') == insufficient-samples ]] || fail 'metrics did not report insufficient-samples when plan/exec sample counts are below CALIBRATION_MIN_SAMPLES'
[[ $(mj '.stall_calibration.plan_p90') -eq 50 && $(mj '.stall_calibration.exec_p90') -eq 120 ]] || fail 'metrics stall_calibration did not surface the observed p90s'
[[ $(mj '.stall_calibration.provider_stall_seconds') -eq 3600 ]] || fail 'metrics stall_calibration did not echo the applied provider_stall_seconds'
[[ $(mj '.durations.pr_review_wait.n') -eq 2 && $(mj '.durations.pr_review_wait.max') -eq 7280 ]] || fail 'metrics computed the wrong pr_review_wait distribution (PR 9: 400-120=280, PR 11: 7300-20=7280; PR 10 is unmerged and excluded)'
[[ $(mj '.durations.lead_time.n') -eq 3 && $(mj '.durations.lead_time.max') -eq 7300 ]] || fail 'metrics computed the wrong lead_time distribution (505 is excluded: no completed marker despite the label)'

[[ $(mj '.by_category.feature') -eq 6 && $(mj '.by_category.improvement') -eq 8 ]] || fail 'metrics miscounted by_category'
[[ $(mj '.by_priority."75"') -eq 4 && $(mj '.by_priority."50"') -eq 3 && $(mj '.by_priority."25"') -eq 6 && $(mj '.by_priority."0"') -eq 1 ]] || fail 'metrics miscounted by_priority (numeric body-marker keys)'
[[ $(mj '.utilization.busy_seconds') -eq 2715 && $(mj '.utilization.max_workers') -eq 2 ]] || fail 'metrics computed the wrong worker-utilization busy_seconds'

# Privacy: only enum/numeric marker fields, Issue numbers, and label names may
# ever reach the output -- never a title, a comment body, or a worker id.
[[ $metrics_json != *'Fake issue'* ]] || fail 'metrics output leaked an Issue title'
[[ $metrics_json != *'worker=w'* ]] || fail 'metrics output leaked a worker identifier'
grep -Fq '"by_worker"' <<< "$metrics_json" && fail 'metrics output exposes a per-worker breakdown'
true

# Determinism: the same --as-of over the same GitHub state reproduces the same
# report, since metrics is a pure read that never mutates state. generated_at
# records real wall-clock time (when the aggregation ran, not the --as-of
# window), so it is excluded from the comparison to avoid a flaky failure when
# the two runs straddle a second boundary.
metrics_json_again=$("$target/bin/agentic-loop" metrics --format json --days "$days" --as-of "$as_of")
strip_generated_at() { printf '%s' "$1" | yq -p json 'del(.generated_at)' -o json; }
# The right-hand side of `[[ == ]]`/`!=` is matched as a glob pattern when
# unquoted, and this JSON output always contains a `[...]` array (e.g. an
# empty "warnings":[]), so both sides must be quoted here to force a literal
# string comparison instead.
[[ "$(strip_generated_at "$metrics_json")" == "$(strip_generated_at "$metrics_json_again")" ]] || fail 'metrics --as-of is not reproducible across repeated runs'

# Argument validation.
"$target/bin/agentic-loop" metrics --format xml >/dev/null 2>&1 && fail 'metrics accepted an invalid --format'
"$target/bin/agentic-loop" metrics --days 0 >/dev/null 2>&1 && fail 'metrics accepted a non-positive --days'
"$target/bin/agentic-loop" metrics --as-of not-a-number >/dev/null 2>&1 && fail 'metrics accepted a non-numeric --as-of'

# metrics is read-only: it must never write an Issue/PR/label/comment, and must
# never call GraphQL or Projects, regardless of format.
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
# The working tree itself must be untouched by a read-only report command. An
# earlier write_queue_config call in this section already left the tree with
# uncommitted changes of its own, so compare a before/after snapshot around
# this one invocation rather than asserting a globally clean tree.
worktree_before=$(git -C "$target" status --porcelain)
"$target/bin/agentic-loop" metrics --days "$days" --as-of "$as_of" >/dev/null
[[ $(git -C "$target" status --porcelain) == "$worktree_before" ]] || fail 'metrics modified the working tree'
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/metrics-calls.log"
grep -Eq $'\t(api graphql|project |issue (create|close|comment)|--method (POST|PATCH|PUT|DELETE))' "$TEST_ROOT/metrics-calls.log" && fail 'metrics performed a write, GraphQL, or Projects call'

# REST(core) budget guard: below core_reserve, metrics must not spend a single
# request on its own collections, and must say so plainly (never a crash or a
# silent empty report indistinguishable from "no history"). read_graphql_budget
# caches the rate_limit snapshot for RATE_LIMIT_CACHE_SECONDS, so the cache must
# be cleared immediately before (a stale high remaining would mask the guard)
# and immediately after (a stale low remaining would poison later collections).
rm -f "$state_root/graphql-rate-limit"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
low_budget_json=$(FAKE_CORE_REMAINING=10 "$target/bin/agentic-loop" metrics --format json --days "$days" --as-of "$as_of")
[[ $(printf '%s' "$low_budget_json" | yq -p json '.github_available') == false ]] || fail 'metrics did not report github_available:false under a REST(core) budget shortfall'
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/metrics-budget-calls.log"
grep -Fq 'fromdateiso8601' "$TEST_ROOT/metrics-budget-calls.log" && fail 'metrics queried GitHub despite an insufficient REST(core) reserve'
rm -f "$state_root/graphql-rate-limit"

# Degraded collection: a comment-fetch failure must not abort the whole report.
degraded_json=$(FAKE_METRICS_EVENTS_FAIL=1 "$target/bin/agentic-loop" metrics --format json --days "$days" --as-of "$as_of")
[[ $(printf '%s' "$degraded_json" | yq -p json '.github_available') == true ]] || fail 'a degraded comment collection should not fail the whole report'
[[ $(printf '%s' "$degraded_json" | yq -p json '.warnings | length') -gt 0 ]] || fail 'a degraded comment collection was not reported as a warning'

# Text format renders without error and stays free of the same private data.
metrics_text=$("$target/bin/agentic-loop" metrics --days "$days" --as-of "$as_of")
[[ $metrics_text == *'転帰:'* ]] || fail 'metrics text format did not render a summary'
[[ $metrics_text != *'Fake issue'* && $metrics_text != *'worker=w'* ]] || fail 'metrics text format leaked private data'

# stall_calibration undersized: append 25 exec_seconds samples at 4000s (>=
# the fixture's provider_stall_seconds=3600) so the real-execution p90 is at
# or above the applied threshold and metrics must report verdict=undersized
# with a matching warning. Appended after every assertion above that depends
# on this fixture's exact counts/reproducibility, since this mutates the
# shared metrics_issues/metrics_events files those assertions already read.
calib_undersized_issues="" calib_undersized_events=""
for calib_n in $(seq 601 625); do
  calib_undersized_issues+="$calib_n	$((window_start + 5000))	0	open	category:improvement	25	agent:running"$'\n'
  calib_undersized_events+="$calib_n	$((window_start + 5010))	usage worker=w stage=exec provider=codex seconds=4000 exit=0"$'\n'
done
printf '%s' "$calib_undersized_issues" >> "$metrics_issues"
printf '%s' "$calib_undersized_events" >> "$metrics_events"
metrics_json_undersized=$("$target/bin/agentic-loop" metrics --format json --days "$days" --as-of "$as_of")
[[ $(printf '%s' "$metrics_json_undersized" | yq -p json '.stall_calibration.verdict') == undersized ]] || fail 'metrics did not report undersized once exec_seconds p90 reached provider_stall_seconds'
[[ $(printf '%s' "$metrics_json_undersized" | yq -p json '.warnings[]') == *stall_calibration* ]] || fail 'metrics did not emit a stall_calibration warning when undersized'

# --- flaky test detection and quarantine (Issue #60, ADR 0022, docs/operations/flaky-tests.md) ---
flaky_sh="$PROJECT_ROOT/scripts/flaky.sh"
flaky_ok_log="$TEST_ROOT/flaky-ok.log"
flaky_fail_log="$TEST_ROOT/flaky-fail.log"
flaky_crash_log="$TEST_ROOT/flaky-crash.log"
printf 'ok\n' > "$flaky_ok_log"
printf 'garbage\nFAIL: something broke\n' > "$flaky_fail_log"
printf 'boom (no FAIL: line)\n' > "$flaky_crash_log"
flaky_fp=$(printf '%s' 'FAIL: something broke' | sha256sum | cut -c1-12)

# 常時成功: attempt1で成功しretryが起きない(attempts長=1)。
classify_json=$("$flaky_sh" classify --unit e2e:queue --attempt "co-run:0:$flaky_ok_log")
[[ $(yq -p json -r '.verdict' <<< "$classify_json") == passed ]] || fail 'flaky classify did not report passed for a clean attempt1'
[[ $(yq -p json -r '.attempts | length' <<< "$classify_json") -eq 1 ]] || fail 'flaky classify retried an always-passing attempt1'

# 常時失敗: attempt2も失敗し verdict=failing。既知flakyの隔離entryがあっても
# 決定的失敗は常に非ゼロ(隔離不可)。
registry_known="$TEST_ROOT/flaky-registry-known.toml"
cat > "$registry_known" <<EOF
schema_version = 1
[[entry]]
unit = "e2e:queue"
fingerprint = "$flaky_fp"
message = "known flaky"
issue = 60
owner = "wakuwaku3"
first_seen = "$(date -u +%Y-%m-%d)"
until = "$(date -u -d '+7 days' +%Y-%m-%d)"
EOF
"$flaky_sh" classify --unit e2e:queue --registry "$registry_known" --attempt "co-run:1:$flaky_fail_log" --attempt "isolated:1:$flaky_fail_log" >/dev/null 2>&1 \
  && fail 'flaky classify quarantined a decisive (non-flaky) failure despite a matching registry entry'
classify_json=$("$flaky_sh" classify --unit e2e:queue --registry "$registry_known" --attempt "co-run:1:$flaky_fail_log" --attempt "isolated:1:$flaky_fail_log" 2>/dev/null || true)
[[ $(yq -p json -r '.verdict' <<< "$classify_json") == failing ]] || fail 'flaky classify did not report failing for two decisive failures'
[[ $(yq -p json -r '.quarantined' <<< "$classify_json") == false ]] || fail 'flaky classify quarantined a decisive failure'
[[ $(yq -p json -r '.attempts[0].fail_line' <<< "$classify_json") == 'FAIL: something broke' ]] || fail 'flaky classify did not preserve the first failure log line'

# 断続失敗: attempt2成功・attempt3失敗はflaky/intermittent。隔離が無ければ非ゼロ。
"$flaky_sh" classify --unit e2e:queue --attempt "co-run:1:$flaky_fail_log" --attempt "isolated:0:$flaky_ok_log" --attempt "isolated:1:$flaky_fail_log" >/dev/null 2>&1 \
  && fail 'flaky classify exited 0 for an unquarantined intermittent flaky unit'
classify_json=$("$flaky_sh" classify --unit e2e:queue --attempt "co-run:1:$flaky_fail_log" --attempt "isolated:0:$flaky_ok_log" --attempt "isolated:1:$flaky_fail_log" 2>/dev/null || true)
[[ $(yq -p json -r '.verdict' <<< "$classify_json") == flaky ]] || fail 'flaky classify did not report flaky for an attempt2-pass/attempt3-fail pattern'
[[ $(yq -p json -r '.hints[0]' <<< "$classify_json") == intermittent ]] || fail 'flaky classify did not hint intermittent'

# 順序依存: attempt2・attempt3とも成功はflaky/isolation-sensitive。
classify_json=$("$flaky_sh" classify --unit e2e:queue --attempt "co-run:1:$flaky_fail_log" --attempt "isolated:0:$flaky_ok_log" --attempt "isolated:0:$flaky_ok_log" 2>/dev/null || true)
[[ $(yq -p json -r '.verdict' <<< "$classify_json") == flaky ]] || fail 'flaky classify did not report flaky for a co-run-fail/isolated-success pattern'
[[ $(yq -p json -r '.hints[0]' <<< "$classify_json") == isolation-sensitive ]] || fail 'flaky classify did not hint isolation-sensitive'

# 既知flaky: registryのunit+fingerprintに一致すればexit 0で、隔離対象・
# Issue番号・責任者を明示する。
classify_out=$("$flaky_sh" classify --unit e2e:queue --registry "$registry_known" --attempt "co-run:1:$flaky_fail_log" --attempt "isolated:0:$flaky_ok_log" --attempt "isolated:0:$flaky_ok_log") \
  || fail 'flaky classify did not exit 0 for a matching quarantine entry'
assert_contains <(printf '%s' "$classify_out") 'issue=#60' 'flaky classify did not disclose the quarantining Issue number'
assert_contains <(printf '%s' "$classify_out") 'owner=wakuwaku3' 'flaky classify did not disclose the quarantine owner'
[[ $(yq -p json -r '.quarantined' <<< "$(head -n1 <<< "$classify_out")") == true ]] || fail 'flaky classify did not mark quarantined:true'

# fingerprintが1文字違えば隔離は効かない。
registry_wrong="$TEST_ROOT/flaky-registry-wrong.toml"
sed "s/$flaky_fp/${flaky_fp:0:11}0/" "$registry_known" > "$registry_wrong"
"$flaky_sh" classify --unit e2e:queue --registry "$registry_wrong" --attempt "co-run:1:$flaky_fail_log" --attempt "isolated:0:$flaky_ok_log" --attempt "isolated:0:$flaky_ok_log" >/dev/null 2>&1 \
  && fail 'flaky classify quarantined a mismatched fingerprint'

# 修復後: entryが残っていてもattempt1が成功すれば厳格動作(verdict=passed)へ
# 戻り、除去候補として報告する。
classify_out=$("$flaky_sh" classify --unit e2e:queue --registry "$registry_known" --attempt "co-run:0:$flaky_ok_log")
classify_json=$(head -n1 <<< "$classify_out")
[[ $(yq -p json -r '.verdict' <<< "$classify_json") == passed ]] || fail 'flaky classify did not report passed after repair'
[[ $(yq -p json -r '.removal_candidate' <<< "$classify_json") == true ]] || fail 'flaky classify did not flag a removal candidate for a repaired unit'

# FAIL:行を抽出できない異常終了はfingerprint=unknownとして常に隔離不可。
classify_json=$("$flaky_sh" classify --unit e2e:queue --attempt "co-run:1:$flaky_crash_log" --attempt "isolated:1:$flaky_crash_log" 2>/dev/null || true)
[[ $(yq -p json -r '.verdict' <<< "$classify_json") == failing-unknown ]] || fail 'flaky classify did not classify a FAIL:-less crash as failing-unknown'
[[ $(yq -p json -r '.fingerprint' <<< "$classify_json") == unknown ]] || fail 'flaky classify did not report fingerprint=unknown for a FAIL:-less crash'

# 抜け道なし: 除外・skip用のoptionは存在しない。
grep -Eiq 'flaky|skip-group|known-failure' "$PROJECT_ROOT/scripts/affected-check.sh" && fail 'affected-check.sh source mentions a flaky-based exclusion token'
grep -Fq -- '--exclude)' "$flaky_sh" "$PROJECT_ROOT/tests/run-e2e.sh" && fail 'flaky.sh/run-e2e.sh implement an --exclude case branch'
grep -Fq -- '--skip)' "$flaky_sh" "$PROJECT_ROOT/tests/run-e2e.sh" && fail 'flaky.sh/run-e2e.sh implement a --skip case branch'
"$flaky_sh" classify --exclude e2e:queue >/dev/null 2>&1 && fail 'flaky.sh classify accepted an --exclude argument'

# --- scripts/flaky.sh audit --------------------------------------------------
"$flaky_sh" audit --registry "$PROJECT_ROOT/tests/flaky-registry.toml" || fail 'flaky audit failed against the real tests/flaky-registry.toml'

flaky_registry_root="$TEST_ROOT/flaky-registries"
mkdir -p "$flaky_registry_root"

cat > "$flaky_registry_root/expired.toml" <<EOF
schema_version = 1
[[entry]]
unit = "e2e:queue"
fingerprint = "$flaky_fp"
message = "expired"
issue = 1
owner = "o"
first_seen = "2020-01-01"
until = "2020-01-05"
EOF
audit_out=$("$flaky_sh" audit --registry "$flaky_registry_root/expired.toml" 2>&1) && fail 'flaky audit accepted an expired entry'
assert_contains <(printf '%s' "$audit_out") '期限切れです' 'flaky audit did not report the expired entry'

cat > "$flaky_registry_root/toolong.toml" <<EOF
schema_version = 1
[[entry]]
unit = "e2e:queue"
fingerprint = "$flaky_fp"
message = "too long"
issue = 1
owner = "o"
first_seen = "$(date -u +%Y-%m-%d)"
until = "$(date -u -d '+30 days' +%Y-%m-%d)"
EOF
audit_out=$("$flaky_sh" audit --registry "$flaky_registry_root/toolong.toml" 2>&1) && fail 'flaky audit accepted an entry spanning more than 14 days'
assert_contains <(printf '%s' "$audit_out") '14日を超えています' 'flaky audit did not report the too-long span'

cat > "$flaky_registry_root/incomplete.toml" <<EOF
schema_version = 1
[[entry]]
unit = "e2e:queue"
fingerprint = "$flaky_fp"
message = "missing owner"
issue = 1
owner = ""
first_seen = "$(date -u +%Y-%m-%d)"
until = "$(date -u -d '+5 days' +%Y-%m-%d)"
EOF
audit_out=$("$flaky_sh" audit --registry "$flaky_registry_root/incomplete.toml" 2>&1) && fail 'flaky audit accepted an entry missing owner'
assert_contains <(printf '%s' "$audit_out") '必須fieldが不足' 'flaky audit did not report the incomplete entry'

# Built by concatenation (never a contiguous literal in this source file):
# this file itself is an INIT_FILE that later upgrade fixtures below copy and
# `git commit`, which would otherwise trip the repository's own pre-commit
# secret guard on this very file.
flaky_secret_msg=$(printf '%s%s leaked' 'AKIA' 'ABCDEFGHIJKLMNOPQRS')
cat > "$flaky_registry_root/secret.toml" <<EOF
schema_version = 1
[[entry]]
unit = "e2e:queue"
fingerprint = "$flaky_fp"
message = "$flaky_secret_msg"
issue = 1
owner = "o"
first_seen = "$(date -u +%Y-%m-%d)"
until = "$(date -u -d '+5 days' +%Y-%m-%d)"
EOF
audit_out=$("$flaky_sh" audit --registry "$flaky_registry_root/secret.toml" 2>&1) && fail 'flaky audit accepted a secret-like message'
assert_contains <(printf '%s' "$audit_out") '秘密様の文字列' 'flaky audit did not report the secret-like message'

{
  printf 'schema_version = 1\n'
  for flaky_u in a b c d; do
    flaky_fp_u=$(printf '%s' "FAIL: $flaky_u" | sha256sum | cut -c1-12)
    printf '[[entry]]\nunit = "%s"\nfingerprint = "%s"\nmessage = "m-%s"\nissue = 1\nowner = "o"\nfirst_seen = "%s"\nuntil = "%s"\n' \
      "$flaky_u" "$flaky_fp_u" "$flaky_u" "$(date -u +%Y-%m-%d)" "$(date -u -d '+5 days' +%Y-%m-%d)"
  done
} > "$flaky_registry_root/toomany.toml"
audit_out=$("$flaky_sh" audit --registry "$flaky_registry_root/toomany.toml" 2>&1) && fail 'flaky audit accepted more than 3 active entries'
assert_contains <(printf '%s' "$audit_out") '3件を超えています' 'flaky audit did not report too many active entries'
[[ -f $PROJECT_ROOT/tests/flaky-registry.toml ]] || fail 'the real tests/flaky-registry.toml went missing during the flaky fixtures'

# --- tests/run-e2e.sh retry orchestration (fake runner, no fake gh needed) --
flaky_fake_runner="$TEST_ROOT/flaky-fake-runner.sh"
cat > "$flaky_fake_runner" <<'FAKE_RUNNER'
#!/usr/bin/env bash
set -uo pipefail
group=${AGENTIC_LOOP_TEST_GROUP:-all}
state_dir=${FAKE_RUNNER_STATE:?}
mkdir -p "$state_dir"
counter_file="$state_dir/$group.count"
n=0
[[ -f $counter_file ]] && n=$(cat "$counter_file")
n=$((n + 1))
printf '%s' "$n" > "$counter_file"
behavior=$(cat "$state_dir/$group.behavior" 2>/dev/null || echo pass)
IFS=',' read -ra steps <<< "$behavior"
idx=$((n - 1))
(( idx >= ${#steps[@]} )) && idx=$((${#steps[@]} - 1))
step=${steps[$idx]}
if [[ $step == fail ]]; then
  printf 'FAIL: simulated failure for %s\n' "$group" >&2
  exit 1
fi
printf 'ok\n'
FAKE_RUNNER
chmod +x "$flaky_fake_runner"

run_e2e_sh="$PROJECT_ROOT/tests/run-e2e.sh"
flaky_empty_registry="$TEST_ROOT/flaky-registry-empty.toml"
printf 'schema_version = 1\n' > "$flaky_empty_registry"

# 常時成功: attempt1で成功しretryが起きない(record.attempts長=1)。
run_state1="$TEST_ROOT/run-e2e-state-1"
mkdir -p "$run_state1"
echo pass > "$run_state1/queue.behavior"
run_record1="$TEST_ROOT/run-e2e-record-1.json"
FAKE_RUNNER_STATE="$run_state1" "$run_e2e_sh" --groups queue --runner "$flaky_fake_runner" --registry "$flaky_empty_registry" --record "$run_record1" >/dev/null 2>&1 \
  || fail 'run-e2e.sh failed for an always-passing group'
[[ $(yq -p json -r '.verdicts[0].verdict' "$run_record1") == passed ]] || fail 'run-e2e.sh record did not mark an always-passing group as passed'
[[ $(yq -p json -r '.verdicts[0].attempts | length' "$run_record1") -eq 1 ]] || fail 'run-e2e.sh retried an always-passing group'

# CI matrixは共通入口を変えず、環境変数でrun-e2e.shの既定群を1群へ絞る。
# 明示的な--groupsを渡さない経路を固定し、各matrix jobが全4群を重複実行する
# regressionを防ぐ。
run_state_env="$TEST_ROOT/run-e2e-state-env"
mkdir -p "$run_state_env"
echo pass > "$run_state_env/lifecycle.behavior"
run_report_env="$TEST_ROOT/run-e2e-report-env.tsv"
AGENTIC_LOOP_TEST_GROUP=lifecycle FAKE_RUNNER_STATE="$run_state_env" "$run_e2e_sh" --runner "$flaky_fake_runner" --registry "$flaky_empty_registry" --report "$run_report_env" --no-record >/dev/null 2>&1 \
  || fail 'run-e2e.sh failed for the CI matrix environment-selected group'
[[ $(wc -l < "$run_report_env") -eq 1 ]] || fail 'run-e2e.sh ran more than one group for the CI matrix environment selection'
grep -Eq '^lifecycle[[:space:]]+passed[[:space:]]+' "$run_report_env" || fail 'run-e2e.sh did not run the CI matrix environment-selected group'

# 常時失敗: 隔離entryがあっても決定的失敗は非ゼロで終了する。
run_state2="$TEST_ROOT/run-e2e-state-2"
mkdir -p "$run_state2"
echo fail > "$run_state2/queue.behavior"
run_fp_always=$(printf '%s' 'FAIL: simulated failure for queue' | sha256sum | cut -c1-12)
run_registry_always="$TEST_ROOT/run-e2e-registry-always.toml"
cat > "$run_registry_always" <<EOF
schema_version = 1
[[entry]]
unit = "e2e:queue"
fingerprint = "$run_fp_always"
message = "should never quarantine a decisive failure"
issue = 60
owner = "wakuwaku3"
first_seen = "$(date -u +%Y-%m-%d)"
until = "$(date -u -d '+5 days' +%Y-%m-%d)"
EOF
run_record2="$TEST_ROOT/run-e2e-record-2.json"
FAKE_RUNNER_STATE="$run_state2" "$run_e2e_sh" --groups queue --runner "$flaky_fake_runner" --registry "$run_registry_always" --record "$run_record2" >/dev/null 2>&1 \
  && fail 'run-e2e.sh exited 0 for a decisively-failing group despite a matching quarantine entry'
[[ $(yq -p json -r '.verdicts[0].verdict' "$run_record2") == failing ]] || fail 'run-e2e.sh record did not mark a decisive failure as failing'

# 既知flaky(順序依存パターン): 隔離entryに一致すればexit 0。
run_state3="$TEST_ROOT/run-e2e-state-3"
mkdir -p "$run_state3"
echo 'fail,pass,pass' > "$run_state3/queue.behavior"
run_record3="$TEST_ROOT/run-e2e-record-3.json"
FAKE_RUNNER_STATE="$run_state3" "$run_e2e_sh" --groups queue --runner "$flaky_fake_runner" --registry "$run_registry_always" --record "$run_record3" >/dev/null 2>&1 \
  || fail 'run-e2e.sh did not exit 0 for a quarantined flaky group'
[[ $(yq -p json -r '.verdicts[0].verdict' "$run_record3") == flaky ]] || fail 'run-e2e.sh record did not mark the quarantined group as flaky'
[[ $(yq -p json -r '.verdicts[0].quarantined' "$run_record3") == true ]] || fail 'run-e2e.sh record did not mark the group as quarantined'
[[ $(yq -p json -r '.commit' "$run_record3") =~ ^([0-9a-f]{40}|unknown)$ ]] || fail 'run-e2e.sh record did not capture a commit identifier'
[[ $(yq -p json -r '.env_marker' "$run_record3") != '' ]] || fail 'run-e2e.sh record did not capture an environment marker'

# 除外・skip用のinterfaceは存在しない: 未知optionは終了code 2、--attemptsは1-3のみ。
run_rc=0
"$run_e2e_sh" --exclude e2e:queue >/dev/null 2>&1 || run_rc=$?
[[ $run_rc -eq 2 ]] || fail 'run-e2e.sh did not reject --exclude with exit code 2'
run_rc=0
"$run_e2e_sh" --attempts 5 >/dev/null 2>&1 || run_rc=$?
[[ $run_rc -eq 2 ]] || fail 'run-e2e.sh did not reject an out-of-range --attempts with exit code 2'

# --- bin/agentic-loop flaky (read-only) and doctor integration -------------
# $target was installed in "install" (not "init") mode, so tests/flaky-
# registry.toml -- an INIT_FILE -- was never seeded there; a missing registry
# is a warning, not a failure (see flaky_registry_validate's not-installed
# case), so registry_valid must still be true.
flaky_cli_json=$("$target/bin/agentic-loop" flaky --format json) || fail 'bin/agentic-loop flaky failed against a missing registry'
[[ $(yq -p json -r '.registry_valid' <<< "$flaky_cli_json") == true ]] || fail 'bin/agentic-loop flaky reported an invalid missing registry'

flaky_target_registry="$target/tests/flaky-registry.toml"
flaky_target_existed=0
[[ -f $flaky_target_registry ]] && flaky_target_existed=1
flaky_target_backup="$TEST_ROOT/target-flaky-registry-backup.toml"
(( flaky_target_existed )) && cp "$flaky_target_registry" "$flaky_target_backup"
mkdir -p "$(dirname "$flaky_target_registry")"
cat > "$flaky_target_registry" <<EOF
schema_version = 1
[[entry]]
unit = "e2e:queue"
fingerprint = "0123456789ab"
message = "expired for doctor/flaky CLI fixture"
issue = 1
owner = "o"
first_seen = "2020-01-01"
until = "2020-01-05"
EOF
"$target/bin/agentic-loop" flaky >/dev/null 2>&1 && fail 'bin/agentic-loop flaky did not exit 1 for an expired registry entry'
flaky_doctor_json=$("$target/bin/agentic-loop" doctor --format json || true)
[[ $(yq -p json -r '[.checks[] | select(.name == "flaky test registry") | select(.level == "failure")] | length' <<< "$flaky_doctor_json") -ge 1 ]] \
  || fail 'doctor did not report the expired flaky entry as a failure'
if (( flaky_target_existed )); then cp "$flaky_target_backup" "$flaky_target_registry"; else rm -f "$flaky_target_registry"; fi

# --- bin/agentic-loop flaky report (repair-Issue creation, direct function
# stubbing -- avoids re-deriving the shared fake gh dispatcher's protocol for
# a write path that never runs from a make check invocation) ---------------
flaky_report_state="$TEST_ROOT/flaky-report-state"
mkdir -p "$flaky_report_state"
flaky_report_record="$TEST_ROOT/flaky-report-record.json"
cat > "$flaky_report_record" <<'EOF'
{"schema":1,"verdicts":[
  {"unit":"e2e:queue","fingerprint":"aaaaaaaaaaaa","verdict":"passed"},
  {"unit":"e2e:lifecycle","fingerprint":"bbbbbbbbbbbb","verdict":"failing"},
  {"unit":"e2e:auxiliary","fingerprint":"cccccccccccc","verdict":"flaky"},
  {"unit":"e2e:upgrade","fingerprint":"dddddddddddd","verdict":"flaky-unknown"}
]}
EOF
flaky_report_out=$(
  # shellcheck source=bin/lib/agentic-loop/common.sh
  source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"
  say() { printf '%s\n' "$*"; }
  fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
  repo_name() { printf 'acme/example\n'; }
  project_add_issue() { return 0; }
  project_sync_state() { return 0; }
  comment_issue() { printf 'comment_issue %s\n' "$1" >> "$flaky_report_state/calls"; }
  search_issues_api() {
    local joined=" $* "
    printf 'search %s\n' "$*" >> "$flaky_report_state/calls"
    case $joined in
      *cccccccccccc*) printf '[{"number":901,"state":"open"}]\n' ;;
      *dddddddddddd*) printf '[{"number":902,"state":"closed"}]\n' ;;
      *) printf '[]\n' ;;
    esac
  }
  repo_api() {
    local joined=" $* "
    printf '%s\n' "$*" >> "$flaky_report_state/calls"
    case $1 in
      issues)
        if [[ $joined == *'--method POST'* ]]; then
          printf 'created content:\n%s\n' "$*" >> "$flaky_report_state/created"
          printf '903\n'
        fi
        ;;
      */labels) : ;;
    esac
  }
  # shellcheck disable=SC2030 # deliberately scoped to this subshell only
  REPO_ROOT=''
  source "$PROJECT_ROOT/bin/lib/agentic-loop/flaky.sh"
  cmd_flaky_report --record "$flaky_report_record"
)
assert_contains <(printf '%s' "$flaky_report_out") '2件の修復Issue' 'flaky report did not process exactly the 2 flaky/flaky-unknown units'
assert_contains <(printf '%s' "$flaky_report_out") '既存のflaky修復Issue #901' 'flaky report did not reuse an existing open Issue for a recurring fingerprint'
assert_contains <(printf '%s' "$flaky_report_out") 'flaky修復Issue #903' 'flaky report did not create a new Issue when only a closed Issue previously matched'
call_log="$flaky_report_state/calls"
grep -Fq 'search ' "$call_log" || fail 'flaky report did not search for an existing dedup marker before creating a new Issue'
grep -Fq 'in:body' "$call_log" || fail 'flaky report search query did not scope to the Issue body'
grep -Fq 'cccccccccccc' "$call_log" || fail 'flaky report did not search using the flaky verdict fingerprint'
grep -Fq 'bbbbbbbbbbbb' "$call_log" && fail 'flaky report searched GitHub for a decisive (failing) unit, which must never be reported'
grep -Fq 'aaaaaaaaaaaa' "$call_log" && fail 'flaky report searched GitHub for a passed unit, which must never be reported'
grep -Eq '^issues --method GET' "$call_log" && fail 'flaky report dedup lookup fell back to a paginated issues listing instead of search_issues_api'
[[ -f $flaky_report_state/created ]] || fail 'flaky report did not create a new Issue for the closed-only match'
grep -Fq 'agentic-loop:flaky unit=e2e:upgrade fingerprint=dddddddddddd' "$flaky_report_state/created" || fail 'flaky report new-Issue body is missing the dedup marker'
# Issue #110: flaky_report_one's new-Issue body must reach GitHub with real
# newlines, never a literal `\n`.
! grep -F '\n' "$flaky_report_state/created" || fail 'flaky report new-Issue body contained a literal \n instead of a real newline'
# Labels are sent via stdin (not captured by this repo_api stub); presence of
# exactly one /labels endpoint call, for the newly created Issue only, is
# what this fixture can observe.
grep -c '/labels' "$call_log" | grep -Fxq 1 || fail 'flaky report did not attach labels to exactly the newly created Issue'

# --- Issue #198: flaky repair-Issue dedup no longer walks every open/closed
# Issue; one search/issues query (a search-index lookup, not an enumeration)
# replaces the two full paginated listings. The dedup lookup's call count
# must stay flat regardless of the repository's cumulative Issue count
# (fixture: 10 vs 100 accumulated closed Issues) -- unlike the removed
# `-f state=open/closed --paginate` calls, whose transfer volume grew with
# that count.
flaky_scale_scenario() {
  local state_dir="$TEST_ROOT/flaky-scale-$1"
  mkdir -p "$state_dir"
  : > "$state_dir/search-calls"
  (
    # shellcheck source=bin/lib/agentic-loop/common.sh
    source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"
    say() { :; }
    repo_name() { printf 'acme/example\n'; }
    comment_issue() { :; }
    # A repository that has accumulated many closed Issues over time (the
    # $1 fixture parameter, e.g. 10 vs 100) must never change the shape of
    # this one query/response pair.
    search_issues_api() { printf '%s\n' "$*" >> "$state_dir/search-calls"; printf '[{"number":960,"state":"closed"}]\n'; }
    repo_api() {
      case $1 in
        issues) [[ $* == *'--method POST'* ]] && { printf '961\n'; return 0; } ;;
        */labels) return 0 ;;
      esac
    }
    project_add_issue() { return 0; }
    project_sync_state() { return 0; }
    # shellcheck disable=SC2030 # deliberately scoped to this subshell only
    REPO_ROOT=''
    source "$PROJECT_ROOT/bin/lib/agentic-loop/flaky.sh"
    flaky_report_one 'e2e:scale' 'eeeeeeeeeeee' flaky >/dev/null
  )
  wc -l < "$state_dir/search-calls"
}
flaky_scale_calls_10=$(flaky_scale_scenario 10) || fail 'flaky dedup scale scenario (10 cumulative closed Issues) failed'
flaky_scale_calls_100=$(flaky_scale_scenario 100) || fail 'flaky dedup scale scenario (100 cumulative closed Issues) failed'
[[ $flaky_scale_calls_10 -eq 1 ]] || fail "flaky_report_one issued $flaky_scale_calls_10 dedup search calls for a 10-closed-Issue fixture (expected exactly 1)"
[[ $flaky_scale_calls_100 -eq 1 ]] || fail "flaky_report_one issued $flaky_scale_calls_100 dedup search calls for a 100-closed-Issue fixture (expected exactly 1)"
[[ $flaky_scale_calls_10 -eq $flaky_scale_calls_100 ]] || fail "flaky_report_one's dedup lookup call count scaled with cumulative closed Issue count (10 -> $flaky_scale_calls_10, 100 -> $flaky_scale_calls_100); it must cost the same regardless of repository size"

# --- Retry attempt ceiling (Issue #130, ADR 0025 T4) -----------------------
# repo_api's REST retry loop stops at API_RETRY_ATTEMPTS regardless of how
# many times the underlying call keeps failing: retry never amplifies a
# single logical operation into unbounded GitHub calls.
retry_ceiling_dir="$TEST_ROOT/retry-ceiling"
retry_ceiling_calls_log="$TEST_ROOT/retry-ceiling-calls.log"
mkdir -p "$retry_ceiling_dir/bin" "$retry_ceiling_dir/state"
: > "$retry_ceiling_calls_log"
printf 'acme/example\n' > "$retry_ceiling_dir/state/repository"
cat > "$retry_ceiling_dir/bin/gh" <<GHFAKE
#!/usr/bin/env bash
printf '1\n' >> "$retry_ceiling_calls_log"
printf 'HTTP 503: Service Unavailable\n' >&2
exit 1
GHFAKE
chmod +x "$retry_ceiling_dir/bin/gh"
(
  # shellcheck disable=SC2030,SC2031 # intentionally subshell-local retry fixture environment
  export PATH="$retry_ceiling_dir/bin:$TEST_HOST_PATH"
  # shellcheck disable=SC2030 # intentionally subshell-local: read by the api.sh sourced just below, within this same subshell
  export STATE_ROOT="$retry_ceiling_dir/state"
  # shellcheck disable=SC2030 # intentionally subshell-local (see above)
  export PROGRAM_NAME=retry-ceiling-test
  export API_RETRY_ATTEMPTS=3
  export API_RETRY_BASE_SECONDS=0
  # shellcheck source=bin/lib/agentic-loop/common.sh
  source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"
  # shellcheck source=bin/lib/agentic-loop/api.sh
  source "$PROJECT_ROOT/bin/lib/agentic-loop/api.sh"
  repo_api issues --method GET >/dev/null 2>&1
) || true
retry_ceiling_calls=$(wc -l < "$retry_ceiling_calls_log")
[[ $retry_ceiling_calls -eq 3 ]] || fail "repo_api made $retry_ceiling_calls underlying calls for API_RETRY_ATTEMPTS=3 against a persistently failing endpoint (expected exactly 3, no unbounded retry amplification)"

# A timeout is ambiguous for writes: the server may have committed the
# request before the client lost its response. Do not blindly resend POST,
# while preserving timeout retry for idempotent GET reads.
timeout_method_dir="$TEST_ROOT/timeout-method"
timeout_method_calls_log="$TEST_ROOT/timeout-method-calls.log"
mkdir -p "$timeout_method_dir/bin" "$timeout_method_dir/state"
: > "$timeout_method_calls_log"
printf 'acme/example\n' > "$timeout_method_dir/state/repository"
cat > "$timeout_method_dir/bin/gh" <<GHFAKE
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$timeout_method_calls_log"
printf 'curl: (28) Operation timed out\n' >&2
exit 1
GHFAKE
chmod +x "$timeout_method_dir/bin/gh"
(
  # shellcheck disable=SC2030,SC2031 # intentionally subshell-local timeout fixture environment
  export PATH="$timeout_method_dir/bin:$TEST_HOST_PATH"
  export STATE_ROOT="$timeout_method_dir/state" PROGRAM_NAME=timeout-method-test
  export API_RETRY_ATTEMPTS=3 API_RETRY_BASE_SECONDS=0
  # shellcheck source=bin/lib/agentic-loop/common.sh
  source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"
  # shellcheck source=bin/lib/agentic-loop/api.sh
  source "$PROJECT_ROOT/bin/lib/agentic-loop/api.sh"
  repo_api issues --method POST >/dev/null 2>&1 || true
  repo_api issues --method GET >/dev/null 2>&1 || true
)
timeout_post_calls=$(grep -c -- '--method POST' "$timeout_method_calls_log" || true)
timeout_get_calls=$(grep -c -- '--method GET' "$timeout_method_calls_log" || true)
[[ $timeout_post_calls -eq 1 ]] || fail "timeout POST was retried $timeout_post_calls times (expected exactly once)"
[[ $timeout_get_calls -eq 3 ]] || fail "timeout GET was retried $timeout_get_calls times (expected API_RETRY_ATTEMPTS=3)"

# --- bin/agentic-loop workload static scan (Issue #130, ADR 0025 T7) -------
# The scanner itself is local-only (zero GitHub calls) and detects each of
# W1 (bypassed common boundary), W2 (unbounded pagination), and W3
# (loop-nested listing) independently, clearing once annotated.
workload_scan_fixture="$TEST_ROOT/workload-scan-fixture"
rm -rf "$workload_scan_fixture"
mkdir -p "$workload_scan_fixture/bin/lib/agentic-loop"
cp "$target/bin/agentic-loop" "$workload_scan_fixture/bin/agentic-loop"
cp "$target"/bin/lib/agentic-loop/*.sh "$workload_scan_fixture/bin/lib/agentic-loop/"
cat > "$workload_scan_fixture/bin/lib/agentic-loop/zzz-fixture.sh" <<'FIXTURE'
# shellcheck shell=bash
fixture_w1() {
  local x
  x=$(gh api "repos/example/example" --jq .id 2>/dev/null)
}
fixture_w2() {
  repo_api issues --method GET -f state=open -f per_page=100 --paginate --jq '.[].number'
}
fixture_w3() {
  while IFS= read -r x; do
    repo_api issues --method GET -f state=open -f per_page=100 --paginate --jq '.[].number'
  done < <(printf '1\n2\n')
}
FIXTURE
workload_calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
if "$workload_scan_fixture/bin/agentic-loop" workload > "$TEST_ROOT/workload-scan-unannotated.txt" 2>&1; then
  fail 'workload static scan did not detect unannotated violations'
fi
[[ $(wc -l < "$FAKE_GH_ROOT/calls") -eq $workload_calls_before ]] || fail 'workload static scan made a GitHub API call'
grep -Fq 'bin/lib/agentic-loop/zzz-fixture.sh' "$TEST_ROOT/workload-scan-unannotated.txt" || fail 'workload static scan did not report the fixture violations'
workload_json=$("$workload_scan_fixture/bin/agentic-loop" workload --format json || true)
yq -p json '.' <<< "$workload_json" >/dev/null 2>&1 || fail 'workload --format json did not produce valid JSON'
[[ $(yq -p json -r '[.violations[] | select(.type == "boundary")] | length' <<< "$workload_json") -ge 1 ]] || fail 'workload static scan (json) did not report a W1 boundary violation'
[[ $(yq -p json -r '[.violations[] | select(.type == "unbounded")] | length' <<< "$workload_json") -ge 1 ]] || fail 'workload static scan (json) did not report a W2 unbounded-pagination violation'
[[ $(yq -p json -r '[.violations[] | select(.type == "aggregation")] | length' <<< "$workload_json") -ge 1 ]] || fail 'workload static scan (json) did not report a W3 loop-nested listing violation'
cat > "$workload_scan_fixture/bin/lib/agentic-loop/zzz-fixture.sh" <<'FIXTURE'
# shellcheck shell=bash
fixture_w1() {
  local x
  # workload-boundary: fixture annotation
  x=$(gh api "repos/example/example" --jq .id 2>/dev/null)
}
fixture_w2() {
  # workload-unbounded: fixture annotation bound=fixture
  repo_api issues --method GET -f state=open -f per_page=100 --paginate --jq '.[].number'
}
FIXTURE
"$workload_scan_fixture/bin/agentic-loop" workload > "$TEST_ROOT/workload-scan-annotated.txt" 2>&1 || fail 'workload static scan still failed after annotating every violation'
rm -rf "$workload_scan_fixture"

# --- worker_reassert_running: self-heal agent:running when a foreign supervisor
# (a stale / provider-exhausted / clock-skewed instance on another host) reverts
# a live worker's Issue to agent:queued out from under it (loop-continuity).
# Directly exercises the heartbeat guard with stubbed collaborators, mirroring
# the flaky-report unit test's source-and-stub style. ------------------------
reassert_state="$TEST_ROOT/reassert-state"
mkdir -p "$reassert_state"
reassert_run() {
  # REASSERT_LABELS / REASSERT_STOP are deliberately NOT named `labels`/etc.:
  # worker_reassert_running declares a `local labels`, and bash dynamic scoping
  # would let that empty local shadow a same-named stub variable when repo_api
  # runs. worker_stop_requested is stubbed (its own file-based behavior is
  # covered elsewhere) so this unit needs no shared STATE_ROOT, which keeps it
  # from coupling with other subshells' STATE_ROOT under shellcheck SC2030/31.
  local REASSERT_LABELS=$1 REASSERT_STOP=${2:-}
  (
    # Not following these sources (SC1091 suppressed locally): following
    # worker_state.sh would couple its STATE_ROOT reads with other subshells'
    # STATE_ROOT under SC2030/SC2031. The functions under test are still real
    # (this is a live source at runtime), only shellcheck's static follow is off.
    # shellcheck disable=SC1091
    source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh"
    # shellcheck disable=SC1091
    source "$PROJECT_ROOT/bin/lib/agentic-loop/worker_state.sh"
    repo_api() { printf '%s\n' "$REASSERT_LABELS"; }
    set_issue_state() { printf 'set %s %s\n' "$1" "$2" >> "$reassert_state/calls"; }
    project_sync_state() { printf 'sync %s %s\n' "$1" "$2" >> "$reassert_state/calls"; return 0; }
    worker_stop_requested() { [[ $REASSERT_STOP == stop ]]; }
    worker_reassert_running 77
  )
}

# 1. reverted to exactly agent:queued while working, no stop -> re-assert running
: > "$reassert_state/calls"
reassert_run 'agent:queued'
assert_contains "$reassert_state/calls" 'set 77 running' 'worker_reassert_running did not re-assert agent:running after a foreign revert to agent:queued'
assert_contains "$reassert_state/calls" 'sync 77 running' 'worker_reassert_running did not sync Project state to running after self-heal'

# 2. a pending stop request must suppress self-heal (the operator owns the outcome)
: > "$reassert_state/calls"
reassert_run 'agent:queued' stop
if grep -Fq 'set 77 running' "$reassert_state/calls"; then fail 'worker_reassert_running fought an operator stop request'; fi

# 3. a legitimate non-queued transition (paused) must never be overridden
: > "$reassert_state/calls"
reassert_run 'agent:paused'
if grep -Fq 'set 77 running' "$reassert_state/calls"; then fail 'worker_reassert_running overrode a non-queued agent state'; fi

# 4. already running -> no-op (no redundant writes on the healthy path)
: > "$reassert_state/calls"
reassert_run 'agent:running'
if grep -Fq 'set 77 running' "$reassert_state/calls"; then fail 'worker_reassert_running wrote redundantly while already agent:running'; fi
rm -rf "$reassert_state"

# --- bin/agentic-loop smoke (Issue #279) ---
# This only proves the CLI's wiring to the real-boundary entry points (right
# functions, right order, bounded call counts) using the fake gh/claude test
# doubles; it is not a substitute for the real GraphQL/provider boundary,
# which only `make smoke` against genuine `gh`/provider CLIs exercises (see
# docs/policies/validation-harness.md).
printf '279 needs-input open none 2026-01-01T00:00:00Z none improvement\n' >> "$FAKE_GH_ROOT/$state_key.state"
printf 'https://github.com/acme/installed-project/issues/279\n' >> "$FAKE_GH_ROOT/$state_key.project-items"
printf '279\tNeeds input\tImprovement\t\n' >> "$FAKE_GH_ROOT/$state_key.project-values"
rm -f "$FAKE_GH_ROOT/claude-probe-calls"
calls_before=$(wc -l < "$FAKE_GH_ROOT/calls")
smoke_out="$TEST_ROOT/smoke-success.out"
AGENT_PROVIDER=claude FAKE_CLAUDE_PROBE_MODE=ok "$target/bin/agentic-loop" smoke --issue 279 > "$smoke_out" 2>&1 \
  || fail "smoke did not exit 0 on a healthy boundary: $(cat "$smoke_out")"
assert_contains "$smoke_out" 'github-graphql: ok' 'smoke did not report the GraphQL boundary as ok'
assert_contains "$smoke_out" 'github-rest: ok' 'smoke did not report the REST boundary as ok'
assert_contains "$smoke_out" 'provider(claude): ok' 'smoke did not report the provider boundary as ok'
# Issue #273: smoke also runs a claude-only real-CLI stream-json boundary
# check (docs/decisions/0031), a second billable probe call distinct from the
# usage-percent probe above -- so claude-probe-calls now holds exactly two
# lines (one --output-format json, one --output-format stream-json) instead
# of one.
assert_contains "$smoke_out" 'stream-json boundary: ok' 'smoke did not report the claude stream-json boundary as ok'
tail -n "+$((calls_before + 1))" "$FAKE_GH_ROOT/calls" > "$TEST_ROOT/smoke-calls.log"
[[ $(grep -c 'projectItems(first:20' "$TEST_ROOT/smoke-calls.log" || true) -le 1 ]] || fail 'smoke issued more than one projectItems query for one Issue'
[[ $(grep -Ec $'\tissues/[0-9]+ ' "$TEST_ROOT/smoke-calls.log" || true) -le 2 ]] || fail 'smoke issued more than two read-only REST reads'
[[ $(wc -l < "$FAKE_GH_ROOT/claude-probe-calls" 2>/dev/null || printf 0) -eq 2 ]] || fail 'smoke did not launch exactly the usage probe and the stream-json boundary probe'
assert_contains "$FAKE_GH_ROOT/claude-probe-calls" '--output-format json' 'smoke did not run the usage-percent probe with --output-format json'
assert_contains "$FAKE_GH_ROOT/claude-probe-calls" '--output-format stream-json' 'smoke did not run the stream-json boundary probe with --output-format stream-json'
assert_contains "$FAKE_GH_ROOT/claude-probe-calls" '--verbose' 'smoke did not pass --verbose to the stream-json boundary probe'

# A GraphQL boundary failure is reported before the (billable) provider probe
# ever launches.
rm -f "$FAKE_GH_ROOT/claude-probe-calls"
smoke_fail_out="$TEST_ROOT/smoke-failure.out"
if FAKE_PROJECT_CONTENT_FAIL_ISSUE=279 AGENT_PROVIDER=claude FAKE_CLAUDE_PROBE_MODE=ok \
  "$target/bin/agentic-loop" smoke --issue 279 > "$smoke_fail_out" 2>&1; then
  fail 'smoke exited 0 despite a forced GraphQL boundary failure'
fi
assert_contains "$smoke_fail_out" 'github-graphql: failed' 'smoke did not report the GraphQL boundary failure'
[[ -s $FAKE_GH_ROOT/claude-probe-calls ]] && fail 'smoke launched the provider probe after a GitHub boundary failure'
true

# --- gh --body "@path" silent-loss observability: status/doctor (Issue #272, ADR 0030) ---

unexpanded_body_b64=$(printf '@/tmp/issue_body.md' | base64 -w0)
multiline_body_b64=$(printf '@/tmp/issue_body.md にも触れています。\n続きの行です。' | base64 -w0)
mention_no_path_b64=$(printf '@wakuwaku3' | base64 -w0)
mention_with_space_b64=$(printf '@wakuwaku3 ご確認ください' | base64 -w0)
fenced_path_b64=$(printf '説明:\n```\n@/tmp/issue_body.md\n```\n' | base64 -w0)
printf '920 queued open none 2026-01-01T00:00:00Z none none %s\n921 queued open none 2026-01-02T00:00:00Z none none %s\n922 queued open none 2026-01-03T00:00:00Z none none %s\n923 queued open none 2026-01-04T00:00:00Z none none %s\n924 queued open none 2026-01-05T00:00:00Z none none %s\n' \
  "$unexpanded_body_b64" "$multiline_body_b64" "$mention_no_path_b64" "$mention_with_space_b64" "$fenced_path_b64" > "$FAKE_GH_ROOT/$state_key.state"
calls_before_status=$(wc -l < "$FAKE_GH_ROOT/calls")
status_body_output=$("$target/bin/agentic-loop" status)
calls_after_status=$(wc -l < "$FAKE_GH_ROOT/calls")
[[ $calls_after_status -gt $calls_before_status ]] || fail 'status made no GitHub call while checking body-unexpanded'
grep -Fq '要対応:' <<< "$status_body_output" || fail 'status did not show a needs-attention section for the lost-requirement Issue'
grep -Fq 'issue-body-unexpanded #920:' <<< "$status_body_output" || fail 'status did not report the issue-body-unexpanded anomaly for the lost-requirement Issue'
if grep -Fq '/tmp/issue_body.md' <<< "$status_body_output"; then fail 'status printed the Issue body content instead of a fixed anomaly message'; fi
for negative_issue in 921 922 923 924; do
  grep -Fq "issue-body-unexpanded #$negative_issue:" <<< "$status_body_output" && fail "status false-positived a legitimate body as issue-body-unexpanded (#$negative_issue)"
done
status_body_json=$("$target/bin/agentic-loop" status --format json)
grep -Fq '"code":"issue-body-unexpanded"' <<< "$status_body_json" || fail 'status --format json did not report the issue-body-unexpanded anomaly'
grep -Fq '"subject":"#920"' <<< "$status_body_json" || fail 'status --format json did not name the lost-requirement Issue'

# doctor reuses the same open-Issue snapshot (no extra open-Issue list call)
# and additionally does a single, unpaginated repo-wide recent-comments scan.
calls_before_doctor=$(wc -l < "$FAKE_GH_ROOT/calls")
: > "$FAKE_GH_ROOT/doctor-comments"
export FAKE_DOCTOR_COMMENTS="$FAKE_GH_ROOT/doctor-comments"
doctor_body_out=$("$target/bin/agentic-loop" doctor || true)
open_issue_list_call_count=$(tail -n "+$((calls_before_doctor + 1))" "$FAKE_GH_ROOT/calls" | grep -Fc 'issues --method GET -f state=open' || true)
[[ $open_issue_list_call_count -eq 1 ]] || fail "doctor issued $open_issue_list_call_count open-Issue list calls, expected exactly 1"
grep -Fq '要求本文の欠落' <<< "$doctor_body_out" || fail 'doctor did not warn about the lost-requirement Issue body'
if grep -Fq '/tmp/issue_body.md' <<< "$doctor_body_out"; then fail 'doctor printed the Issue body content instead of a fixed warning message'; fi

# A broken comment (repo-wide, best-effort, single page) is reported distinctly.
printf '%s\n' "$unexpanded_body_b64" > "$FAKE_GH_ROOT/doctor-comments"
doctor_comment_out=$("$target/bin/agentic-loop" doctor || true)
grep -Fq '要求コメントの欠落' <<< "$doctor_comment_out" || fail 'doctor did not warn about the lost-requirement comment'
: > "$FAKE_GH_ROOT/doctor-comments"
doctor_clean_out=$("$target/bin/agentic-loop" doctor || true)
if grep -Fq '要求コメントの欠落' <<< "$doctor_clean_out"; then
  grep -Fq '対応は不要です。' <<< "$doctor_clean_out" || fail 'doctor did not report success once the lost-requirement comment was fixed'
fi
unset FAKE_DOCTOR_COMMENTS
printf '279 needs-input open none 2026-01-01T00:00:00Z none improvement\n' > "$FAKE_GH_ROOT/$state_key.state"

fi

if [[ $TEST_GROUP == all || $TEST_GROUP == upgrade ]]; then

# --- Foundation upgrade (bin/agentic-loop upgrade, scripts/upgrade-target.sh) ---
# See docs/operations/upgrade.md / docs/decisions/0009-foundation-upgrade.md.

# --- Claude Code edit-guard hook (queue-first policy) ----------------------
# A direct edit to a TRACKED file is gated by three things: who is editing
# (autonomous loop vs human), which worktree the target lives in (primary/main
# vs a linked worktree), and whether the target worktree's own gitdir holds the
# escape-hatch flag `agentic-loop-allow-edit`. Untracked/scratch/outside files
# always pass, and the hook never edits anything itself.
hook_main="$TEST_ROOT/hook main"
hook_worker="$TEST_ROOT/hook worker"
hook_outside="$TEST_ROOT/hook outside"
mkdir -p "$hook_main" "$hook_outside"
git -C "$hook_main" init --quiet
git -C "$hook_main" config user.email test@example.invalid
git -C "$hook_main" config user.name test
printf 'tracked\n' > "$hook_main/tracked file.txt"
git -C "$hook_main" add 'tracked file.txt'
git -C "$hook_main" commit --quiet -m tracked
git -C "$hook_main" worktree add --quiet -b hook-worker "$hook_worker"
ln -s "$hook_outside" "$hook_main/outside-link"
ln -s "$hook_main" "$hook_outside/main-link"
hook_main_flag="$(git -C "$hook_main" rev-parse --absolute-git-dir)/agentic-loop-allow-edit"
hook_worker_flag="$(git -C "$hook_worker" rev-parse --absolute-git-dir)/agentic-loop-allow-edit"

run_edit_hook() { # [--agent] tool path cwd
  local marker=env
  [[ $1 == --agent ]] && { marker=agent; shift; }
  local tool=$1 path=$2 cwd=$3 field=file_path
  [[ $tool == NotebookEdit ]] && field=notebook_path
  local input; input=$(printf '{"tool_name":"%s","tool_input":{"%s":"%s"},"cwd":"%s"}' "$tool" "$field" "$path" "$cwd")
  if [[ $marker == agent ]]; then
    printf '%s' "$input" | env AGENTIC_LOOP_AGENT=1 "$PROJECT_ROOT/.claude/hooks/confirm-main-worktree-edit.sh"
  else
    printf '%s' "$input" | env -u AGENTIC_LOOP_AGENT "$PROJECT_ROOT/.claude/hooks/confirm-main-worktree-edit.sh"
  fi
}
hook_before=$(git -C "$hook_main" status --porcelain)
rm -f "$hook_main_flag" "$hook_worker_flag"
# Human, no flag: both a main and a linked-worktree tracked edit are denied, and
# each deny names the escape hatch as the sanctioned way forward.
for edit_tool in Edit Write NotebookEdit; do
  hook_result=$(run_edit_hook "$edit_tool" "$hook_main/tracked file.txt" "$hook_main")
  [[ $hook_result == *'"permissionDecision":"deny"'* ]] || fail "$edit_tool did not deny a human main-worktree tracked edit"
  [[ $hook_result == *'原則禁止'* ]] || fail "$edit_tool main deny did not explain the primary-worktree rule"
  [[ $hook_result == *'agentic-loop-allow-edit'* ]] || fail "$edit_tool main deny did not name the escape hatch"
done
[[ $(git -C "$hook_main" status --porcelain) == "$hook_before" ]] || fail 'edit hook changed a file itself'
hook_result=$(run_edit_hook Edit "$hook_worker/tracked file.txt" "$hook_worker")
[[ $hook_result == *'"permissionDecision":"deny"'* ]] || fail 'human linked-worktree tracked edit was not denied without a flag'
[[ $hook_result == *'agentic-loop-allow-edit'* ]] || fail 'linked deny did not name the escape hatch'
# Autonomous loop: a linked worktree is its job and passes; the primary worktree
# is always blocked, flag or not.
[[ -z $(run_edit_hook --agent Edit "$hook_worker/tracked file.txt" "$hook_worker") ]] || fail 'autonomous linked worktree edit was gated'
hook_result=$(run_edit_hook --agent Edit "$hook_main/tracked file.txt" "$hook_main")
[[ $hook_result == *'"permissionDecision":"deny"'* ]] || fail 'autonomous main-worktree edit was not denied'
[[ $hook_result == *'自律エージェント'* ]] || fail 'autonomous main deny did not identify the loop'
# Escape hatch: the flag opens exactly its own worktree, and only for a human.
touch "$hook_main_flag"
[[ -z $(run_edit_hook Edit "$hook_main/tracked file.txt" "$hook_main") ]] || fail 'primary flag did not permit a human main edit'
[[ $(run_edit_hook --agent Edit "$hook_main/tracked file.txt" "$hook_main") == *'"deny"'* ]] || fail 'primary flag wrongly let an autonomous main edit through'
rm -f "$hook_main_flag"
touch "$hook_worker_flag"
[[ -z $(run_edit_hook Edit "$hook_worker/tracked file.txt" "$hook_worker") ]] || fail 'worker flag did not permit a human linked edit'
[[ $(run_edit_hook Edit "$hook_main/tracked file.txt" "$hook_main") == *'"deny"'* ]] || fail 'worker flag leaked to the primary worktree'
rm -f "$hook_worker_flag"
# Untracked / outside / non-edit targets always pass.
printf 'scratch\n' > "$hook_main/scratchpad.txt"
[[ -z $(run_edit_hook Edit "$hook_main/scratchpad.txt" "$hook_main") ]] || fail 'untracked main-worktree scratchpad was unexpectedly gated'
[[ -z $(run_edit_hook Edit /tmp/agentic-loop-hook-scratch.txt "$hook_main") ]] || fail '/tmp edit was unexpectedly gated'
[[ -z $(run_edit_hook Edit "$hook_outside/outside.txt" "$hook_main") ]] || fail 'outside edit was unexpectedly gated'
[[ -z $(run_edit_hook Read "$hook_main/tracked file.txt" "$hook_main") ]] || fail 'read tool was unexpectedly gated'
# Path traversal and symlinks must not smuggle a main-worktree edit past the gate.
hook_result=$(run_edit_hook Edit "$hook_main/subdir/../tracked file.txt" "$hook_main")
[[ $hook_result == *'"permissionDecision":"deny"'* ]] || fail 'path traversal bypassed the edit hook'
hook_result=$(run_edit_hook Edit "$hook_outside/main-link/tracked file.txt" "$hook_main")
[[ $hook_result == *'"permissionDecision":"deny"'* ]] || fail 'symlink path bypassed the edit hook'
hook_result=$(printf '{"tool_name":"Edit","tool_input":{},"cwd":"%s"}' "$hook_main" | env -u AGENTIC_LOOP_AGENT "$PROJECT_ROOT/.claude/hooks/confirm-main-worktree-edit.sh")
[[ $hook_result == *'"permissionDecision":"deny"'* ]] || fail 'malformed edit input did not fail safely'

# --- gh --body "@path" Bash PreToolUse hook (Issue #272, ADR 0030) ---------
# Fail-open by design: only a Bash tool_input.command containing an unexpanded
# `gh --body "@path"`-shaped value is denied; everything else (including a
# broken payload) passes through unchanged, since Bash is this loop's most
# common tool and a fail-closed gate here would stall it entirely.
run_body_hook() { # COMMAND
  local cmd=$1 escaped
  escaped=${cmd//\\/\\\\}
  escaped=${escaped//\"/\\\"}
  printf '{"tool_name":"Bash","tool_input":{"command":"%s"},"cwd":"%s"}' "$escaped" "$PROJECT_ROOT" \
    | "$PROJECT_ROOT/.claude/hooks/require-gh-body-file.sh"
}
positive_body_commands=(
  'gh issue create --title x --body "@/tmp/issue_body.md"'
  'gh issue create --title x --body=@/tmp/issue_body.md'
  'gh pr comment 12 -b "@/tmp/a/b.md"'
  'gh pr create --title x --body "@notes.md"'
)
for cmd in "${positive_body_commands[@]}"; do
  hook_result=$(run_body_hook "$cmd")
  [[ $hook_result == *'"permissionDecision":"deny"'* ]] || fail "gh --body hook did not deny: $cmd"
  [[ $hook_result == *'--body-file'* ]] || fail "gh --body hook deny did not point at --body-file: $cmd"
done
negative_body_commands=(
  'gh issue create --title x --body-file /tmp/issue_body.md'
  'gh issue create --title x --body "通常の本文です"'
  'gh issue comment 1 --body "@wakuwaku3 ご確認ください"'
  'gh api repos/o/r/issues -f body=@x.md'
  'ls -la /tmp'
)
for cmd in "${negative_body_commands[@]}"; do
  [[ -z $(run_body_hook "$cmd") ]] || fail "gh --body hook wrongly denied: $cmd"
done
# Non-Bash tool calls are never inspected, even if the command text matches.
non_bash_result=$(printf '{"tool_name":"Edit","tool_input":{"command":"gh issue create --body \\"@x.md\\""},"cwd":"%s"}' "$PROJECT_ROOT" | "$PROJECT_ROOT/.claude/hooks/require-gh-body-file.sh")
[[ -z $non_bash_result ]] || fail 'gh --body hook inspected a non-Bash tool call'
# yq missing from PATH: fall back to the raw-payload string scan rather than
# fail-closed (a Bash tool call must not be blanket-denied just because the
# JSON parser is unavailable).
[[ -z $(printf '{"tool_name":"Bash","tool_input":{"command":"ls -la"},"cwd":"%s"}' "$PROJECT_ROOT" | PATH=/usr/bin:/bin "$PROJECT_ROOT/.claude/hooks/require-gh-body-file.sh") ]] \
  || fail 'gh --body hook denied an unrelated Bash command with yq unavailable'
hook_result=$(printf '{"tool_name":"Bash","tool_input":{"command":"gh issue create --title x --body \\"@/tmp/issue_body.md\\""},"cwd":"%s"}' "$PROJECT_ROOT" | PATH=/usr/bin:/bin "$PROJECT_ROOT/.claude/hooks/require-gh-body-file.sh")
[[ $hook_result == *'"permissionDecision":"deny"'* ]] || fail 'gh --body hook did not deny with yq unavailable (raw fallback)'
# body_unexpanded_file_reference in the hook must agree with the shell
# common.sh implementation for the same fixture table, so the two cannot
# drift. common.sh is sourced in an isolated subshell (it defines its own
# fail/say, which must not clobber this test script's own).
common_predicate() { ( source "$PROJECT_ROOT/bin/lib/agentic-loop/common.sh" >/dev/null 2>&1; body_unexpanded_file_reference "$1" ); }
for cmd_body in '@/tmp/issue_body.md' '通常の本文です' '@wakuwaku3 ご確認ください' '@x.md'; do
  common_verdict=1; common_predicate "$cmd_body" && common_verdict=0
  hook_verdict=1
  [[ $(run_body_hook "gh issue create --body \"$cmd_body\"") == *'"deny"'* ]] && hook_verdict=0
  [[ $common_verdict == "$hook_verdict" ]] || fail "common.sh and the Bash hook disagree on: $cmd_body"
done

# Regression (Issue #160 + linked-worktree PATH restore): the hook must resolve
# its JSON parser (yq) from the recorded runtime.path even when invoked with a
# PATH that omits the Nix-pinned bins -- Claude Code PreToolUse and login shells
# are such contexts. runtime.path lives beside the git COMMON dir. In a linked
# worktree `.git` is a gitdir FILE, not a directory, so the resolver must follow
# it to the common dir; without that the parse fail-closes and denies every
# linked-worktree edit -- including the autonomous worker edits that must pass.
# Build an installed-shaped repo whose runtime.path points at the pinned
# toolchain, mirror the hook into every worktree (it is a tracked file in real
# checkouts), then invoke each with yq stripped from PATH.
hook_rt_main="$TEST_ROOT/hook runtime main"
hook_rt_worker="$TEST_ROOT/hook runtime worker"
mkdir -p "$hook_rt_main"
git -C "$hook_rt_main" init --quiet
git -C "$hook_rt_main" config user.email test@example.invalid
git -C "$hook_rt_main" config user.name test
printf 'tracked\n' > "$hook_rt_main/tracked.txt"
git -C "$hook_rt_main" add tracked.txt
git -C "$hook_rt_main" commit --quiet -m tracked
git -C "$hook_rt_main" worktree add --quiet -b hook-rt-worker "$hook_rt_worker"
for rt_root in "$hook_rt_main" "$hook_rt_worker"; do
  mkdir -p "$rt_root/.claude/hooks"
  cp "$PROJECT_ROOT/.claude/hooks/confirm-main-worktree-edit.sh" "$rt_root/.claude/hooks/confirm-main-worktree-edit.sh"
  chmod +x "$rt_root/.claude/hooks/confirm-main-worktree-edit.sh"
done
# runtime.path records the pinned toolchain directories (where git and yq really
# live), mirroring what install writes. It lives beside the common Git metadata.
mkdir -p "$hook_rt_main/.git/agentic-loop"
printf '%s:%s\n' "$(dirname "$(command -v git)")" "$(dirname "$(command -v yq)")" > "$hook_rt_main/.git/agentic-loop/runtime.path"
run_edit_hook_nopath() { # script tool path cwd [--agent]
  local script=$1 tool=$2 path=$3 cwd=$4 marker=${5:-}
  local input; input=$(printf '{"tool_name":"%s","tool_input":{"file_path":"%s"},"cwd":"%s"}' "$tool" "$path" "$cwd")
  if [[ $marker == --agent ]]; then
    printf '%s' "$input" | env AGENTIC_LOOP_AGENT=1 PATH=/usr/bin:/bin "$script"
  else
    printf '%s' "$input" | env -u AGENTIC_LOOP_AGENT PATH=/usr/bin:/bin "$script"
  fi
}
# Primary worktree: yq is absent from PATH; only runtime.path restoration lets
# the hook parse and reach the human main-edit deny instead of fail-closing.
hook_rt_result=$(run_edit_hook_nopath "$hook_rt_main/.claude/hooks/confirm-main-worktree-edit.sh" Edit "$hook_rt_main/tracked.txt" "$hook_rt_main")
[[ $hook_rt_result == *'"permissionDecision":"deny"'* && $hook_rt_result == *'原則禁止'* ]] || fail 'hook could not resolve yq from runtime.path for a main-worktree edit (Issue #160)'
# Linked worktree: `.git` is a file, so the resolver must follow it to the common
# dir to find runtime.path; only then does an autonomous worker edit parse and pass.
[[ -z $(run_edit_hook_nopath "$hook_rt_worker/.claude/hooks/confirm-main-worktree-edit.sh" Edit "$hook_rt_worker/tracked.txt" "$hook_rt_worker" --agent) ]] || fail 'hook gated an autonomous linked worktree because yq was unresolved in a gitdir-file worktree (Issue #160)'

# The project settings and executable hook are Foundation-managed shared files.
settings_conflict_target=$(new_repository settings-conflict-target)
mkdir -p "$settings_conflict_target/.claude"
printf '{"custom":"keep"}\n' > "$settings_conflict_target/.claude/settings.json"
if AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$settings_conflict_target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null 2>&1; then
  fail 'install silently overwrote an existing Claude settings file'
fi
[[ $(<"$settings_conflict_target/.claude/settings.json") == '{"custom":"keep"}' ]] || fail 'install changed an existing Claude settings file'

upgrade_target=$(new_repository upgrade-target)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$upgrade_target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null
[[ -x $upgrade_target/.claude/hooks/confirm-main-worktree-edit.sh ]] || fail 'install did not distribute an executable Claude edit hook'
[[ $(yq -p json -r '.hooks.PreToolUse[0].matcher' "$upgrade_target/.claude/settings.json") == 'Edit|Write|NotebookEdit' ]] || fail 'install did not distribute Claude hook settings'
[[ $(yq -p json -r '.files[] | select(.path == ".claude/hooks/confirm-main-worktree-edit.sh") | .class' "$upgrade_target/.agentic-loop/manifest.json") == shared ]] || fail 'manifest did not classify the Claude edit hook as shared'
[[ -x $upgrade_target/.claude/hooks/require-gh-body-file.sh ]] || fail 'install did not distribute an executable gh --body Bash hook'
[[ $(yq -p json -r '.hooks.PreToolUse[1].matcher' "$upgrade_target/.claude/settings.json") == Bash ]] || fail 'install did not distribute the gh --body Bash hook settings'
[[ $(yq -p json -r '.files[] | select(.path == ".claude/hooks/require-gh-body-file.sh") | .class' "$upgrade_target/.agentic-loop/manifest.json") == shared ]] || fail 'manifest did not classify the gh --body Bash hook as shared'
[[ -f $upgrade_target/.agentic-loop/manifest.json ]] || fail 'install did not write a Foundation manifest'
[[ $(yq -p json -o yaml '.mode' "$upgrade_target/.agentic-loop/manifest.json") == install ]] || fail 'manifest recorded the wrong install mode'
[[ $(yq -p json -o yaml '.source.repository' "$upgrade_target/.agentic-loop/manifest.json") == 'wakuwaku3/agentic-loop-foundation' ]] || fail 'manifest recorded the wrong source repository'
[[ $(yq -p json -o yaml '.source.revision' "$upgrade_target/.agentic-loop/manifest.json") =~ ^[0-9a-f]{40}$ ]] || fail 'manifest did not record a resolved 40-hex revision'
[[ $(yq -p json -o yaml '.files | map(select(.class == "shared")) | length' "$upgrade_target/.agentic-loop/manifest.json") -gt 0 ]] || fail 'manifest recorded no shared files'
# The one deliberate exception (Issue #56, ADR 0018): an install-mode target
# still gets its own detection-only capability manifest, recorded class=init
# (target-owned; upgrade never removes or overwrites it) precisely because it
# is not one of the brand-new-repository INIT_FILES. No other path may be
# misclassified this way.
[[ $(yq -p json -r '[.files[] | select(.class == "init") | .path] | join(",")' "$upgrade_target/.agentic-loop/manifest.json") == '.agentic-loop/capabilities.toml' ]] || fail 'an install-mode manifest recorded an unexpected init-class file'
git -C "$upgrade_target" add -A && git -C "$upgrade_target" commit --quiet -m 'install foundation' && git -C "$upgrade_target" push --quiet

# A "new" Foundation revision: this checkout, plus a shared-file update and a
# brand-new shared file, built by copying the working tree (so uncommitted
# work in this checkout is exercised as the upgrade target, same as every
# other install test above using AGENTIC_LOOP_SOURCE="$PROJECT_ROOT").
new_source="$TEST_ROOT/foundation-v2"
cp -a "$PROJECT_ROOT" "$new_source"
rm -rf "$new_source/.git"
printf '\n# upgraded\n' >> "$new_source/AGENTS.md"
printf 'new doc\n' > "$new_source/docs/operations/new-feature.md"
sed -i 's#docs/operations/upgrade.md#docs/operations/upgrade.md docs/operations/new-feature.md#' "$new_source/scripts/lib/foundation-files.sh"
printf '\n# upgraded documentation policy\n' >> "$new_source/docs/policies/documentation.md"
printf '\n# upgraded development doc\n' >> "$new_source/docs/development.md"

# Pristine: no upstream changes -> no-op dry-run, zero writes.
before_status=$(git -C "$upgrade_target" status --porcelain)
pristine_out=$("$upgrade_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT")
[[ $pristine_out == *'変更はありません'* ]] || fail 'a pristine upgrade dry-run reported spurious changes'
[[ $(git -C "$upgrade_target" status --porcelain) == "$before_status" ]] || fail 'a dry-run upgrade modified the working tree'

# Normal update: dry-run reports it, changes nothing, --apply reflects it.
update_out=$("$upgrade_target/bin/agentic-loop" upgrade --source "$new_source")
[[ $update_out == *'[update] AGENTS.md'* ]] || fail 'dry-run did not report the AGENTS.md update'
[[ $update_out == *'[add] docs/operations/new-feature.md'* ]] || fail 'dry-run did not report the new shared file as an addition'
[[ $(git -C "$upgrade_target" status --porcelain) == "$before_status" ]] || fail 'a dry-run upgrade with pending changes modified the working tree'
"$upgrade_target/bin/agentic-loop" upgrade --source "$new_source" --apply --skip-verify >/dev/null || fail 'apply of a safe update failed'
assert_contains "$upgrade_target/AGENTS.md" '# upgraded' 'apply did not update AGENTS.md'
[[ -f $upgrade_target/docs/operations/new-feature.md ]] || fail 'apply did not add the new shared file'
# docs/policies/documentation.md (Issue #64, ADR 0023) is Foundation-managed
# (SHARED_FILES) and upstream updates apply through the ordinary shared-file path.
assert_contains "$upgrade_target/docs/policies/documentation.md" '# upgraded documentation policy' 'apply did not update docs/policies/documentation.md as a shared file'
[[ $(yq -p json -o yaml '.source.revision' "$upgrade_target/.agentic-loop/manifest.json") == unknown ]] || fail 'manifest should record "unknown" for an unpinned --source without its own Git history'
git -C "$upgrade_target" add -A && git -C "$upgrade_target" commit --quiet -m 'apply update'
rerun_out=$("$upgrade_target/bin/agentic-loop" upgrade --source "$new_source")
[[ $rerun_out == *'変更はありません'* ]] || fail 'rerunning upgrade after a completed update was not a no-op'

# revision must be explicit: no --revision/--source and an unpinned config -> exit 2.
rc=0
"$upgrade_target/bin/agentic-loop" upgrade >/dev/null 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "expected exit 2 when no revision is configured or given, got $rc"

# Supervisor running -> --apply is refused, nothing changes. A live pid file
# is faked directly (rather than a real `start`/`stop` cycle) to keep this
# scenario fast; upgrade-target.sh only checks $STATE_ROOT/supervisor.pid.
mkdir -p "$upgrade_target/.git/agentic-loop"
printf '%s\n' "$$" > "$upgrade_target/.git/agentic-loop/supervisor.pid"
rc=0
"$upgrade_target/bin/agentic-loop" upgrade --source "$new_source" --apply --skip-verify >/dev/null 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "expected exit 2 while the Supervisor is running, got $rc"
rm -f "$upgrade_target/.git/agentic-loop/supervisor.pid"

# User-edited conflict: never silently overwritten; --overwrite adopts it explicitly.
conflict_target=$(new_repository conflict-target)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$conflict_target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null
git -C "$conflict_target" add -A && git -C "$conflict_target" commit --quiet -m install && git -C "$conflict_target" push --quiet
printf '\n# locally customized\n' >> "$conflict_target/AGENTS.md"
git -C "$conflict_target" add AGENTS.md && git -C "$conflict_target" commit --quiet -m 'local edit'
conflict_out=$("$conflict_target/bin/agentic-loop" upgrade --source "$new_source")
[[ $conflict_out == *'[conflict] AGENTS.md'* ]] || fail 'dry-run did not report the user-edited AGENTS.md as a conflict'
"$conflict_target/bin/agentic-loop" upgrade --source "$new_source" --apply --skip-verify >/dev/null || fail 'apply with a conflict failed unexpectedly'
assert_contains "$conflict_target/AGENTS.md" 'locally customized' 'apply silently overwrote a user-edited file'
[[ -f $conflict_target/AGENTS.md.agentic-loop-new ]] || fail 'apply did not stage the new content alongside a conflicting file'
git -C "$conflict_target" add -A && git -C "$conflict_target" commit --quiet -m 'after conflict apply'
"$conflict_target/bin/agentic-loop" upgrade --source "$new_source" --apply --overwrite AGENTS.md --skip-verify >/dev/null || fail 'apply with --overwrite failed'
assert_contains "$conflict_target/AGENTS.md" '# upgraded' '--overwrite did not adopt the new content'
[[ -f $conflict_target/AGENTS.md.agentic-loop-new ]] && fail '--overwrite left a stale .agentic-loop-new file behind'

# class:init files are seeded once and never touched by upgrade, even on drift.
init_target=$(empty_repository init-target)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$init_target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null
git -C "$init_target" add -A && git -C "$init_target" commit --quiet -m 'init install' && git -C "$init_target" push --quiet -u origin main
printf '\n# upstream readme change\n' >> "$new_source/README.md"
init_out=$("$init_target/bin/agentic-loop" upgrade --source "$new_source")
[[ $init_out == *'[init-notice] README.md'* ]] || fail 'dry-run did not report an init-owned file with upstream drift'
"$init_target/bin/agentic-loop" upgrade --source "$new_source" --apply --skip-verify >/dev/null || fail 'apply with an init-owned drift failed'
if grep -Fq 'upstream readme change' "$init_target/README.md"; then fail 'apply modified a user-owned init file'; fi
# docs/development.md (Issue #64, ADR 0023) is seeded once (INIT_FILES) and is
# never overwritten by upgrade even when upstream changes it.
if grep -Fq 'upgraded development doc' "$init_target/docs/development.md"; then fail 'apply overwrote the init-owned docs/development.md'; fi

# Old install predating manifest.json: a pending config migration is detected,
# applied idempotently, pins the applied revision, and bumps migration_level.
old_source="$TEST_ROOT/foundation-v0"
cp -a "$PROJECT_ROOT" "$old_source"
sed -i '/^\[foundation\]$/,$d' "$old_source/.agentic-loop.toml"
migration_target=$(new_repository migration-target)
AGENTIC_LOOP_SOURCE="$old_source" AGENTIC_LOOP_TARGET="$migration_target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null
if grep -Fq '[foundation]' "$migration_target/.agentic-loop.toml"; then fail 'pre-upgrade fixture unexpectedly already has a [foundation] section'; fi
git -C "$migration_target" add -A && git -C "$migration_target" commit --quiet -m 'pre-upgrade install' && git -C "$migration_target" push --quiet
migration_out=$("$migration_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT")
[[ $migration_out == *'[migration] 0001-foundation-config-section'* ]] || fail 'dry-run did not report the pending config migration'
"$migration_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT" --apply --skip-verify >/dev/null || fail 'apply of a pending migration failed'
grep -Fq '[foundation]' "$migration_target/.agentic-loop.toml" || fail 'migration did not add the [foundation] section'
resolved_revision=$(git -C "$PROJECT_ROOT" rev-parse HEAD)
assert_contains "$migration_target/.agentic-loop.toml" "revision = \"$resolved_revision\"" 'migration apply did not pin the applied revision'
[[ $(yq -p json -o yaml '.source.revision' "$migration_target/.agentic-loop/manifest.json") == "$resolved_revision" ]] || fail 'upgrade manifest did not record the applied revision'
[[ $(yq -p json -o yaml '.migration_level' "$migration_target/.agentic-loop/manifest.json") -eq 7 ]] || fail 'manifest migration_level was not bumped after applying the migration'
migration_rerun=$("$migration_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT")
[[ $migration_rerun == *'変更はありません'* ]] || fail 'rerunning upgrade after a completed migration was not a no-op'

# Migration 0003 (Issue #56, ADR 0018): an install predating the capability
# manifest feature (simulated, like 0001/0002 above, by removing the feature
# being migrated from an old Foundation source -- here, install-target.sh's
# generation step) has no capabilities.toml; upgrade detects it as pending,
# applies it idempotently via the same detection-only capability_generate
# used by install, and a rerun is a no-op.
cap_old_source="$TEST_ROOT/foundation-pre-capability"
cp -a "$PROJECT_ROOT" "$cap_old_source"
# shellcheck disable=SC2016 # Single-quoted on purpose: a literal sed pattern, not shell expansion.
sed -i '/capability_generate "\$target"/d' "$cap_old_source/scripts/install-target.sh"
cap_migration_target=$(new_repository capability-migration-target)
AGENTIC_LOOP_SOURCE="$cap_old_source" AGENTIC_LOOP_TARGET="$cap_migration_target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null
[[ ! -e $cap_migration_target/.agentic-loop/capabilities.toml ]] || fail 'pre-upgrade fixture unexpectedly already has a capability manifest'
git -C "$cap_migration_target" add -A && git -C "$cap_migration_target" commit --quiet -m 'pre-capability install' && git -C "$cap_migration_target" push --quiet
cap_migration_out=$("$cap_migration_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT")
[[ $cap_migration_out == *'[migration] 0003-capability-manifest'* ]] || fail 'dry-run did not report the pending capability-manifest migration'
"$cap_migration_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT" --apply --skip-verify >/dev/null || fail 'apply of the pending capability-manifest migration failed'
[[ -f $cap_migration_target/.agentic-loop/capabilities.toml ]] || fail 'migration did not generate a capability manifest'
cap_migration_json=$("$cap_migration_target/bin/agentic-loop" capabilities --format json)
[[ $(yq -p json -r '.data.validation.secret_guard' <<< "$cap_migration_json") == '.agentic-loop/guard-secrets.sh' ]] || fail 'migration-generated capability manifest did not detect the secret guard entry point'
cap_migration_rerun=$("$cap_migration_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT")
[[ $cap_migration_rerun == *'変更はありません'* ]] || fail 'rerunning upgrade after the capability-manifest migration was not a no-op'

# Migration 0006 (Issue #130, ADR 0025): an install predating the workload
# budget gate (simulated by removing the `workload = "warn"` line from an old
# Foundation source's own .agentic-loop.toml) has no queue.workload key;
# upgrade detects it as pending, applies it idempotently, and a rerun (both
# of the migration script directly and of upgrade as a whole) is a no-op.
workload_old_source="$TEST_ROOT/foundation-pre-workload"
cp -a "$PROJECT_ROOT" "$workload_old_source"
sed -i '/^workload = "warn"$/d' "$workload_old_source/.agentic-loop.toml"
if grep -Fq 'workload = "warn"' "$workload_old_source/.agentic-loop.toml"; then fail 'pre-upgrade fixture unexpectedly already has queue.workload'; fi
workload_migration_target=$(new_repository workload-migration-target)
AGENTIC_LOOP_SOURCE="$workload_old_source" AGENTIC_LOOP_TARGET="$workload_migration_target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null
if grep -Fq 'workload = "warn"' "$workload_migration_target/.agentic-loop.toml"; then fail 'pre-upgrade install unexpectedly already has queue.workload'; fi
git -C "$workload_migration_target" add -A && git -C "$workload_migration_target" commit --quiet -m 'pre-workload install' && git -C "$workload_migration_target" push --quiet
workload_migration_out=$("$workload_migration_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT")
[[ $workload_migration_out == *'[migration] 0006-workload-config'* ]] || fail 'dry-run did not report the pending workload-config migration'
"$workload_migration_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT" --apply --skip-verify >/dev/null || fail 'apply of the pending workload-config migration failed'
grep -Eq '^workload = "warn"$' "$workload_migration_target/.agentic-loop.toml" || fail 'migration did not add workload = "warn"'
workload_migration_rerun=$("$workload_migration_target/bin/agentic-loop" upgrade --source "$PROJECT_ROOT")
[[ $workload_migration_rerun == *'変更はありません'* ]] || fail 'rerunning upgrade after the workload-config migration was not a no-op'
# Direct check/apply/apply idempotency of the migration script itself.
workload_migration_direct="$TEST_ROOT/workload-migration-direct"
mkdir -p "$workload_migration_direct"
printf '[queue]\npoll_seconds = 30\n' > "$workload_migration_direct/.agentic-loop.toml"
if "$PROJECT_ROOT/scripts/upgrade/migrations/0006-workload-config.sh" "$workload_migration_direct" check; then fail 'migration 0006 check reported already-applied for a config missing workload'; fi
"$PROJECT_ROOT/scripts/upgrade/migrations/0006-workload-config.sh" "$workload_migration_direct" apply || fail 'migration 0006 direct apply failed'
"$PROJECT_ROOT/scripts/upgrade/migrations/0006-workload-config.sh" "$workload_migration_direct" check || fail 'migration 0006 check did not report already-applied after apply'
workload_direct_config_after_first="$(cat "$workload_migration_direct/.agentic-loop.toml")"
"$PROJECT_ROOT/scripts/upgrade/migrations/0006-workload-config.sh" "$workload_migration_direct" apply || fail 'migration 0006 direct re-apply (idempotent no-op) failed'
[[ "$(cat "$workload_migration_direct/.agentic-loop.toml")" == "$workload_direct_config_after_first" ]] || fail 'migration 0006 direct re-apply was not idempotent (duplicated the workload line)'

# Approval gate: a breaking/irreversible migration blocks --apply until --approve.
approval_source="$TEST_ROOT/foundation-breaking"
cp -a "$PROJECT_ROOT" "$approval_source"
rm -rf "$approval_source/.git"
cat > "$approval_source/scripts/upgrade/migrations/9999-test-breaking.sh" <<'BREAKING_MIGRATION'
#!/usr/bin/env bash
# id: 9999-test-breaking
# risk: breaking
# reversible: no
# approval: required
# summary: テスト専用のbreaking migration。
# recovery: 対応不要(テスト専用)。
set -euo pipefail
target=$1
mode=$2
marker="$target/.agentic-loop-test-breaking-applied"
case $mode in
  check) [[ -f $marker ]] && exit 0 || exit 1 ;;
  apply) : > "$marker" ;;
esac
BREAKING_MIGRATION
chmod +x "$approval_source/scripts/upgrade/migrations/9999-test-breaking.sh"
approval_target=$(new_repository approval-target)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$approval_target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null
git -C "$approval_target" add -A && git -C "$approval_target" commit --quiet -m install && git -C "$approval_target" push --quiet
approval_out=$("$approval_target/bin/agentic-loop" upgrade --source "$approval_source")
[[ $approval_out == *'承認が必要'* ]] || fail 'dry-run did not surface the pending approval requirement'
rc=0
"$approval_target/bin/agentic-loop" upgrade --source "$approval_source" --apply --skip-verify >/dev/null 2>&1 || rc=$?
[[ $rc -eq 3 ]] || fail "expected exit 3 when a breaking migration is not approved, got $rc"
[[ -f $approval_target/.agentic-loop-test-breaking-applied ]] && fail 'apply changed state despite a missing approval'
[[ -z $(git -C "$approval_target" status --porcelain) ]] || fail 'a rejected apply left the working tree dirty'
"$approval_target/bin/agentic-loop" upgrade --source "$approval_source" --apply --approve --skip-verify >/dev/null || fail 'apply with --approve failed'
[[ -f $approval_target/.agentic-loop-test-breaking-applied ]] || fail '--approve did not apply the breaking migration'

# Verify failure: applied state is kept (not silently reverted), doctor flags
# the unfinished upgrade, and --rollback restores the pre-apply state via Git.
verify_target=$(new_repository verify-target)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$verify_target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh" >/dev/null
git -C "$verify_target" add -A && git -C "$verify_target" commit --quiet -m install && git -C "$verify_target" push --quiet
rc=0
FAKE_DEVBOX_FAIL=1 "$verify_target/bin/agentic-loop" upgrade --source "$new_source" --apply >/dev/null 2>&1 || rc=$?
[[ $rc -eq 1 ]] || fail "expected exit 1 when post-apply verification fails, got $rc"
assert_contains "$verify_target/AGENTS.md" '# upgraded' 'a verify failure should still leave the applied content in place'
[[ -f $verify_target/.git/agentic-loop/upgrade-last-apply.json ]] || fail 'a verify failure did not leave a resumable apply record'
interrupted_check=$( ("$verify_target/bin/agentic-loop" doctor --format json || true) | yq -p json '.checks[] | select(.name == "中断したupgrade") | .level')
[[ $interrupted_check == failure ]] || fail 'doctor should report the unfinished upgrade as a failure'
"$verify_target/bin/agentic-loop" upgrade --rollback || fail 'rollback failed'
git -C "$verify_target" diff --quiet -- AGENTS.md || fail 'rollback did not restore AGENTS.md'
git -C "$verify_target" diff --quiet -- .agentic-loop/manifest.json || fail 'rollback did not restore the installed-revision manifest'
[[ ! -f $verify_target/docs/operations/new-feature.md ]] || fail 'rollback did not remove a newly added file'
[[ ! -f $verify_target/.git/agentic-loop/upgrade-last-apply.json ]] || fail 'rollback did not clear the apply record'
interrupted_check_after=$( ("$verify_target/bin/agentic-loop" doctor --format json || true) | yq -p json '.checks[] | select(.name == "中断したupgrade") | .level')
[[ $interrupted_check_after == success ]] || fail 'doctor should report the upgrade record cleared after rollback'

fi

# --- Global comment-body newline regression guard (Issue #110) -------------
# Across every Issue/PR comment body any test group in this run posted (see
# comment_bodies_log/encode_comment_body above): a real newline re-encodes as
# the 2-char `\n`; a literal `\n` bug re-encodes as the 3-char `\\n`. This
# covers every comment-posting path exercised by this file, not just the ones
# with a dedicated positive assertion above.
if [[ -s $FAKE_GH_ROOT/comment-bodies.log ]] && grep -Fq '\\n' "$FAKE_GH_ROOT/comment-bodies.log"; then
  fail 'a posted comment body contained a literal \n instead of a real newline (Issue #110 regression)'
fi

printf 'Tests passed (%s).\n' "$TEST_GROUP"
