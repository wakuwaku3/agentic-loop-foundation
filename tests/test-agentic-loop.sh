#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
readonly FAKE_BIN="$TEST_ROOT/bin"
readonly FAKE_GH_ROOT="$TEST_ROOT/gh"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
assert_contains() { grep -Fq -- "$2" "$1" || fail "$3"; }

mkdir -p "$FAKE_BIN" "$FAKE_GH_ROOT"

cat > "$FAKE_BIN/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
slug="acme/$(basename "$PWD")"
key=$(printf '%s' "$PWD" | tr '/' '_')
state="$FAKE_GH_ROOT/$key.state"
project="$FAKE_GH_ROOT/$key.project"
comments="$FAKE_GH_ROOT/$key.comments"
printf '%s\t%s\n' "$PWD" "$*" >> "$FAKE_GH_ROOT/calls"
case "${1:-} ${2:-}" in
  'auth status'|'api graphql') exit 0 ;;
  'repo view')
    if [[ $* == *defaultBranchRef* ]]; then printf 'main\n'; else printf '%s\n' "$slug"; fi ;;
  'label create') exit 0 ;;
  'project list')
    if [[ -e $project ]]; then printf '7\n'; fi ;;
  'project create') touch "$project"; printf '{"number":7}\n' ;;
  'project link'|'project field-create'|'project item-add') exit 0 ;;
  'issue list')
    wanted=''
    for ((i=1; i<=$#; i++)); do [[ ${!i} == --label ]] && { j=$((i+1)); wanted=${!j}; }; done
    [[ -e $state ]] || exit 0
    if [[ $wanted == agent:queued ]]; then
      if [[ $* == *'--limit 1'* ]]; then awk '$2 == "queued" && $3 != "closed" {print $1; exit}' "$state"; else awk '$2 == "queued" && $3 != "closed" {print $1}' "$state"; fi
    elif [[ $wanted == agent:running ]]; then
      if [[ $* == *template* ]]; then awk '$2 == "running" && $3 != "closed" {print "#" $1 " Fake issue " $1}' "$state"; else awk '$2 == "running" && $3 != "closed" {print $1}' "$state"; fi
    elif [[ $wanted == agent:needs-input ]]; then
      awk '$2 == "needs-input" && $3 != "closed" {print $1}' "$state"
    fi ;;
  'issue edit')
    issue=$3 target=''
    for ((i=1; i<=$#; i++)); do [[ ${!i} == --add-label ]] && { j=$((i+1)); target=${!j#agent:}; }; done
    awk -v n="$issue" -v s="$target" '{if ($1 == n) $2=s; print}' "$state" > "$state.tmp" && mv "$state.tmp" "$state" ;;
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
    printf '%s %s\n' "$issue" "$*" >> "$comments" ;;
  'issue close')
    issue=$3; awk -v n="$issue" '{if ($1 == n) $3="closed"; print}' "$state" > "$state.tmp" && mv "$state.tmp" "$state" ;;
  *) printf 'unexpected fake gh call: %s\n' "$*" >&2; exit 1 ;;
esac
FAKE_GH
cat > "$FAKE_BIN/codex" <<'FAKE_CODEX'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_GH_ROOT/codex-calls"
[[ ${1:-} == exec && ${2:-} == --help ]] && exit 0
output=''
for ((i=1; i<=$#; i++)); do [[ ${!i} == --output-last-message ]] && { j=$((i+1)); output=${!j}; }; done
[[ -n $output ]] || exit 2
sleep "${FAKE_CODEX_SLEEP:-0}"
printf '%s\n' "${FAKE_CODEX_RESULT:-AGENTIC_LOOP_RESULT=completed}" > "$output"
FAKE_CODEX
chmod +x "$FAKE_BIN/gh" "$FAKE_BIN/codex"
export PATH="$FAKE_BIN:$PATH" FAKE_GH_ROOT

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

target=$(new_repository installed-project)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
[[ -x $target/bin/agentic-loop ]] || fail 'install did not add the queue CLI'
[[ -f $target/.agentic-loop/config ]] || fail 'install did not add safe defaults'
[[ $(cat "$target/.codex/config.toml") == 'approval_policy = "never"' ]] || fail 'install did not preserve external sandbox configuration'
[[ $(git -C "$target" config --get core.hooksPath) == .githooks ]] || fail 'install did not enable hooks'
[[ -f $target/docs/operations/issue-queue.md ]] || fail 'init did not install operations documentation'
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
project_creates=$(grep -c $'project create' "$FAKE_GH_ROOT/calls" || true)
[[ $project_creates -eq 1 ]] || { sed -n '1,120p' "$FAKE_GH_ROOT/calls" >&2; fail "reinstall created the Project $project_creates times"; }

printf 'POLL_SECONDS=1\nMAX_WORKERS=2\nLEASE_SECONDS=3\nSTOP_TIMEOUT=10\n' > "$target/.agentic-loop/config"
"$target/bin/agentic-loop" start
first_pid="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/supervisor.pid"
first_pid=$(cat "$first_pid")
"$target/bin/agentic-loop" start
[[ $(cat "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/supervisor.pid") == "$first_pid" ]] || fail 'start duplicated the supervisor'
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'running' <<< "$status_output" || fail 'status did not show the supervisor'
"$target/bin/agentic-loop" stop
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'stopped' <<< "$status_output" || fail 'stop did not drain the supervisor'

# Commit the installed runtime so worker worktrees start from a realistic default branch.
git -C "$target" add .
git -C "$target" commit --quiet -m install
git -C "$target" push --quiet
state_key=$(printf '%s' "$target" | tr '/' '_')
state="$FAKE_GH_ROOT/$state_key.state"
state_root="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop"
printf '1 queued open\n2 queued open\n3 queued open\n' > "$state"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
completed_count=$(awk '$2 == "completed" {count++} END {print count+0}' "$state")
if [[ $completed_count -ne 2 ]]; then
  cat "$state" >&2
  find "$state_root" -type f -maxdepth 3 -print -exec sed -n '1,160p' {} \; >&2 || true
  fail "supervisor completed $completed_count Issues instead of 2"
fi
[[ $(awk '$2 == "queued" {count++} END {print count+0}' "$state") -eq 1 ]] || fail 'run-once claimed more than the worker limit'
assert_contains "$FAKE_GH_ROOT/codex-calls" '--sandbox workspace-write' 'worker did not use workspace-write'
assert_contains "$FAKE_GH_ROOT/codex-calls" 'approval_policy="never"' 'worker can block on approval'
if grep -Eq 'danger-full-access|OPENAI_API_KEY' "$FAKE_GH_ROOT/codex-calls"; then fail 'worker used forbidden Codex configuration'; fi

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

# A crashed worker's expired lease returns to the queue on the next supervisor start.
printf '9 running open\n' > "$state"
printf '9 <!-- agentic-loop:lease worker=dead heartbeat=1 expires=1 -->\n' > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root"
: > "$state_root/stop.requested"
"$target/bin/agentic-loop" _supervise
grep -Eq '^9 queued open$' "$state" || fail 'expired running Issue was not recovered'

# Repositories use separate gh/project state and Git state directories.
second=$(new_repository second-project)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$second" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
[[ -e $FAKE_GH_ROOT/$(printf '%s' "$second" | tr '/' '_').project ]] || fail 'second repository did not get its own Project'
[[ $(git -C "$target" rev-parse --absolute-git-dir) != $(git -C "$second" rev-parse --absolute-git-dir) ]] || fail 'repository state is not isolated'

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

secret_target="$TEST_ROOT/secret-project"
mkdir -p "$secret_target"
git -C "$secret_target" init --quiet
cp "$PROJECT_ROOT/.agentic-loop/guard-secrets.sh" "$secret_target/guard-secrets.sh"
printf 'token=ghp_%s%s\n' '123456789012345678' '901234567890123456' > "$secret_target/leak.txt"
git -C "$secret_target" add leak.txt
if (cd "$secret_target" && ./guard-secrets.sh --staged) >/dev/null 2>&1; then fail 'secret guard accepted a credential-like value'; fi

printf 'Tests passed.\n'
