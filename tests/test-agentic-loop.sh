#!/usr/bin/env bash
# shellcheck disable=SC2155
set -euo pipefail

readonly PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
readonly FAKE_BIN="$TEST_ROOT/bin"
readonly FAKE_GH_ROOT="$TEST_ROOT/gh"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
assert_contains() { grep -Fq -- "$2" "$1" || fail "$3"; }

mkdir -p "$FAKE_BIN" "$FAKE_GH_ROOT"

if env -u DEV_ENVIRONMENT "$PROJECT_ROOT/scripts/check-environment.sh" >/dev/null 2>&1; then
  fail 'environment guard accepted an unpinned host environment'
fi

cat > "$FAKE_BIN/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
slug="acme/$(basename "$PWD")"
key=$(printf '%s' "$PWD" | tr '/' '_')
state="$FAKE_GH_ROOT/$key.state"
project="$FAKE_GH_ROOT/$key.project"
project_items="$FAKE_GH_ROOT/$key.project-items"
comments="$FAKE_GH_ROOT/$key.comments"
views="$FAKE_GH_ROOT/$key.views"
diagnosis_issues="$FAKE_GH_ROOT/$key.diagnosis-issues"
printf '%s\t%s\n' "$PWD" "$*" >> "$FAKE_GH_ROOT/calls"
if [[ $* == *--slurp* && $* == *--jq* ]]; then
  printf 'the `--slurp` option is not supported with `--jq`\n' >&2
  exit 1
fi
case "${1:-} ${2:-}" in
  'auth status') exit 0 ;;
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
    method=GET wanted='' form_state='' input_file=''
    for ((i=1; i<=$#; i++)); do
      case ${!i} in
        --method) j=$((i+1)); method=${!j} ;;
        labels=*) wanted=${!i#labels=} ;;
        state=*) form_state=${!i#state=} ;;
        --input) j=$((i+1)); input_file=${!j} ;;
      esac
    done
    if [[ $endpoint == issues && $method == GET && $form_state == all ]]; then
      awk -v slug="$slug" '{print "https://github.example/" slug "/issues/" $1}' "$state" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET ]]; then
      case $wanted in
        agent:queued)
          if [[ $* == *updated_at* ]]; then
            awk '$2 == "queued" && $3 != "closed" && $6 != "" {print $1 "\t" $6}' "$state"
          elif [[ $* == *created_at* ]]; then
            awk '$2 == "queued" && $3 != "closed" {
              rank=4; if ($4 ~ /(^|,)critical(,|$)/) rank=0; else if ($4 ~ /(^|,)high(,|$)/) rank=1; else if ($4 ~ /(^|,)medium(,|$)/) rank=2; else if ($4 ~ /(^|,)low(,|$)/) rank=3
              created=($5 == "" ? $1 : $5); print rank "\t" created "\t" $1
            }' "$state"
            if [[ -n ${FAKE_STALE_QUEUED_ISSUE:-} ]] && ! awk -v n="$FAKE_STALE_QUEUED_ISSUE" '$1 == n && $2 == "queued" {found=1} END {exit !found}' "$state"; then
              awk -v n="$FAKE_STALE_QUEUED_ISSUE" '$1 == n {print "0\t" ($5 == "" ? $1 : $5) "\t" $1}' "$state"
            fi
          else awk '$2 == "queued" && $3 != "closed" {print $1}' "$state"; fi ;;
        agent:running)
          if [[ $* == *title* ]]; then awk '$2 == "running" && $3 != "closed" {print "#" $1 " Fake issue " $1}' "$state"; else awk '$2 == "running" && $3 != "closed" {print $1}' "$state"; fi ;;
        agent:needs-input) awk '$2 == "needs-input" && $3 != "closed" {print $1}' "$state" ;;
      esac
    elif [[ $endpoint =~ ^issues/([0-9]+)/labels$ && $method == PUT ]]; then
      issue=${BASH_REMATCH[1]}; payload=$(if [[ -n $input_file && $input_file != - ]]; then cat "$input_file"; else cat; fi); target=$(sed -n 's/.*"agent:\([^"]*\)".*/\1/p' <<< "$payload")
      ( flock 9; awk -v n="$issue" -v s="$target" '{if ($1 == n) $2=s; print}' "$state" > "$state.$$.tmp" && mv "$state.$$.tmp" "$state" ) 9> "$state.lock"
    elif [[ $endpoint =~ ^issues/([0-9]+)/comments$ ]]; then
      issue=${BASH_REMATCH[1]}
      if [[ $method == POST ]]; then
        body=''; for arg in "$@"; do [[ $arg == body=* ]] && body=${arg#body=}; done
        printf '%s %s\n' "$issue" "$body" >> "$comments"
      elif [[ $* == *needs-input* ]]; then
        if tail -n 1 "$comments" 2>/dev/null | grep -Fq USER_REPLY; then printf 'true\n'; else printf 'false\n'; fi
      else tail -n 1 "$comments" 2>/dev/null || true; fi
    elif [[ $endpoint =~ ^issues/([0-9]+)$ ]]; then
      issue=${BASH_REMATCH[1]}
      if [[ $method == PATCH && $form_state == closed ]]; then
        ( flock 9; awk -v n="$issue" '{if ($1 == n) $3="closed"; print}' "$state" > "$state.$$.tmp" && mv "$state.$$.tmp" "$state" ) 9> "$state.lock"
      elif [[ $* == *starts*with* ]]; then
        target=$(sed -n 's/.*\["agent:\([^"]*\)"\].*/\1/p' <<< "$*"); printf '["agent:%s"]\n' "$target"
      else awk -v n="$issue" '$1 == n {print "agent:" $2}' "$state"; fi
    elif [[ $endpoint == pulls ]]; then
      if [[ $form_state == closed ]]; then
        if [[ ${FAKE_PR_MERGED:-1} == 1 ]]; then
          head=''; for arg in "$@"; do [[ $arg == head=* ]] && head=${arg#head=}; done; head=${head#*:}
          oid=${FAKE_PR_HEAD_OID:-$(git rev-parse "refs/heads/$head" 2>/dev/null || true)}
          printf 'https://github.example/%s/pull/merged\t%s\n' "$slug" "$oid"
        fi
      elif [[ $* == *head=* && $* == *html_url* ]]; then printf 'https://github.example/%s/pull/6\n' "$slug"
      elif [[ $form_state == all && $* == *html_url* ]]; then printf 'https://github.example/%s/pull/1\nhttps://github.example/%s/pull/2\n' "$slug" "$slug"
      elif [[ $* == *html_url* ]]; then printf 'https://github.example/%s/pull/6\n' "$slug"; fi
    elif [[ -z $endpoint ]]; then printf 'main\n'
    fi ;;
  'api rate_limit')
    printf '%s\t%s\n' "${FAKE_GRAPHQL_REMAINING:-5000}" "${FAKE_GRAPHQL_RESET:-$(($(date +%s) + 3600))}"
    ;;
  'api graphql')
    if [[ $* == *createProjectV2View* ]]; then
      name=''
      for ((i=1; i<=$#; i++)); do [[ ${!i} == name=* ]] && name=${!i#name=}; done
      id="PV_$(printf '%s' "$name" | tr ' ' '_')"
      printf '%s\t%s\n' "$id" "$name" >> "$views"
      printf '%s\n' "$id"
    elif [[ $* == *'views(first: 100)'* ]]; then
      cat "$views" 2>/dev/null || true
    fi ;;
  'repo view')
    if [[ $* == *defaultBranchRef* ]]; then printf 'main\n'; else printf '%s\n' "$slug"; fi ;;
  'label create') exit 0 ;;
  'project list')
    if [[ -e $project ]]; then printf '7\n'; fi ;;
  'project create') touch "$project"; printf '{"number":7}\n' ;;
  'project link'|'project field-create'|'project item-edit') exit 0 ;;
  'project item-list')
    if [[ $* == *content.number* ]]; then printf 'PVTI_fake\n'; else cat "$project_items" 2>/dev/null || true; fi ;;
  'project item-add')
    if (( ${FAKE_PROJECT_FAILURES:-0} > 0 )); then
      failure_file="$FAKE_GH_ROOT/$key.project-failures"
      failures=$(cat "$failure_file" 2>/dev/null || printf '0')
      if (( failures < FAKE_PROJECT_FAILURES )); then printf '%s\n' "$((failures + 1))" > "$failure_file"; exit 1; fi
    fi
    url=''; for ((i=1; i<=$#; i++)); do [[ ${!i} == --url ]] && { j=$((i+1)); url=${!j}; }; done
    grep -Fxq -- "$url" "$project_items" 2>/dev/null || printf '%s\n' "$url" >> "$project_items" ;;
  'project view') printf 'PVT_fake\n' ;;
  'project field-list') printf 'PVTF_fake\n' ;;
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
        awk '$2 == "queued" && $3 != "closed" {
          rank=4
          if ($4 ~ /(^|,)critical(,|$)/) rank=0
          else if ($4 ~ /(^|,)high(,|$)/) rank=1
          else if ($4 ~ /(^|,)medium(,|$)/) rank=2
          else if ($4 ~ /(^|,)low(,|$)/) rank=3
          created=($5 == "" ? $1 : $5)
          print rank "\t" created "\t" $1
        }' "$state" | sort -k1,1n -k2,2 -k3,3n | awk 'NR == 1 {print $3}'
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
    printf '%s %s\n' "$issue" "$*" >> "$comments" ;;
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
    gh issue create --title '診断所見' --body 'diagnosis-finding' --label diagnosis --label agent:queued >/dev/null
    "$worktree/bin/agentic-loop" sync-issue 99 >/dev/null
  fi
fi
if [[ ${FAKE_CODEX_GIT_OPERATIONS:-0} == 1 ]]; then
  expected=$(git -C "$worktree" rev-parse --path-format=absolute --git-common-dir)
  [[ " $add_dirs " == *" $expected "* ]] || { printf 'missing Git add-dir: %s (expected %s)\n' "$add_dirs" "$expected" >&2; exit 3; }
  [[ " $add_dirs " == *" $worktree/.agents "* ]] || { printf 'missing .agents add-dir: %s\n' "$add_dirs" >&2; exit 3; }
  git -C "$worktree" fetch origin main
  printf 'worker change\n' > "$worktree/worker.txt"
  git -C "$worktree" add worker.txt
  git -C "$worktree" commit --quiet -m 'worker change'
  git -C "$worktree" push --quiet origin HEAD:refs/heads/agent/issue-6
fi
sleep "${FAKE_CODEX_SLEEP:-0}"
printf '%s\n' "${FAKE_CODEX_RESULT:-AGENTIC_LOOP_RESULT=completed}" > "$output"
FAKE_CODEX
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
chmod +x "$FAKE_BIN/gh" "$FAKE_BIN/codex" "$FAKE_BIN/systemctl" "$FAKE_BIN/systemd-escape"
export PATH="$FAKE_BIN:$PATH" FAKE_GH_ROOT XDG_CONFIG_HOME="$TEST_ROOT/config"

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
  git -C "$target" remote add origin "$bare"
  printf '%s\n' "$target"
}

target=$(new_repository installed-project)
state_key=$(printf '%s' "$target" | tr '/' '_')
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
[[ -x $target/bin/agentic-loop ]] || fail 'install did not add the queue CLI'
[[ -x $target/bin/agentic-loop-diagnose ]] || fail 'install did not add the manual diagnosis CLI'
[[ -f $target/.agentic-loop/config ]] || fail 'install did not add safe defaults'
[[ -f $target/.agents/skills/diagnose-codebase/SKILL.md ]] || fail 'install did not add the diagnosis skill'
[[ $(cat "$target/.codex/config.toml") == 'approval_policy = "never"' ]] || fail 'install did not preserve external sandbox configuration'
[[ $(git -C "$target" config --get core.hooksPath) == .githooks ]] || fail 'install did not enable hooks'
[[ -f $target/docs/operations/issue-queue.md ]] || fail 'init did not install operations documentation'
[[ -f $target/docs/policies/testing.md ]] || fail 'init did not install the testing policy'
[[ -f $target/docs/policies/external-environment.md ]] || fail 'init did not install the external environment policy'
[[ -f $target/docs/policies/development-environment.md ]] || fail 'init did not install the development environment policy'
[[ -f $target/docs/policies/github-language.md ]] || fail 'init did not install the GitHub language policy'
[[ -f $target/docs/policies/validation-harness.md ]] || fail 'init did not install the validation harness policy'
assert_contains "$target/AGENTS.md" 'GitHub日本語運用ポリシー' 'installed agent instructions did not require Japanese GitHub content'
assert_contains "$target/AGENTS.md" '外部環境コード化ポリシー' 'installed agent instructions did not require reproducible external environments'
assert_contains "$target/docs/policies/external-environment.md" 'desired state' 'installed external environment policy lacks desired state management'
assert_contains "$target/docs/policies/external-environment.md" 'drift検出' 'installed external environment policy lacks drift detection'
assert_contains "$target/docs/policies/external-environment.md" '復旧および移行' 'installed external environment policy lacks recovery and migration requirements'
assert_contains "$target/AGENTS.md" '検証ハーネスポリシー' 'installed agent instructions did not reference the validation harness policy'
assert_contains "$target/docs/policies/validation-harness.md" 'local fast check' 'installed validation policy lacks the fast check layer'
assert_contains "$target/docs/policies/validation-harness.md" 'local full check' 'installed validation policy lacks the full check layer'
assert_contains "$target/docs/policies/validation-harness.md" 'private repository' 'installed validation policy lacks the private repository exception'
assert_contains "$target/.agentic-loop/config" 'GRAPHQL_RESERVE=500' 'installed configuration lacks the GraphQL reserve'
assert_contains "$target/.agentic-loop/config" 'API_RETRY_ATTEMPTS=3' 'installed configuration lacks bounded REST retries'
assert_contains "$target/docs/operations/issue-queue.md" 'GraphQLの残量・reset時刻' 'installed operations documentation lacks shared rate-limit handling'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'in Japanese' 'installed submission skill did not require Japanese GitHub content'
assert_contains "$target/.agents/skills/diagnose-codebase/SKILL.md" 'without modifying' 'diagnosis skill did not prohibit code changes'
assert_contains "$target/AGENTS.md" '通常のbuild・変更要求' 'installed AGENTS.md lacks queue-first routing'
assert_contains "$target/AGENTS.md" 'agent:running' 'installed AGENTS.md lacks the worker exception'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'Queue-first intake' 'installed skill lacks queue-first routing'
assert_contains "$target/.agents/skills/submit-requirement/SKILL.md" 'Non-recursive worker exception' 'installed skill lacks the worker exception'
assert_contains "$target/docs/operations/issue-queue.md" 'open Issueのtitleとbodyを検索' 'installed docs lack duplicate avoidance'
assert_contains "$target/docs/operations/issue-queue.md" '安全なfallback' 'installed docs lack safe fallback'
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
assert_contains "$FAKE_GH_ROOT/codex-calls" 'both diagnosis and agent:queued labels' 'diagnosis prompt did not queue findings'
assert_contains "$FAKE_GH_ROOT/calls" 'issue create --title 診断所見 --body diagnosis-finding --label diagnosis --label agent:queued' 'diagnosis did not create a queued finding with both labels'
first_diagnosis_creates=$(grep -c $'\tissue create ' "$FAKE_GH_ROOT/calls")
second_diagnosis_output=$("$target/bin/agentic-loop-diagnose")
[[ $second_diagnosis_output == AGENTIC_LOOP_RESULT=completed ]] || fail 'repeated diagnosis did not report the Codex result'
second_diagnosis_creates=$(grep -c $'\tissue create ' "$FAKE_GH_ROOT/calls")
[[ $second_diagnosis_creates -eq $first_diagnosis_creates ]] || fail 'repeated diagnosis created a duplicate Issue'
grep -Fq $'label create diagnosis' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the diagnosis label'
# shellcheck disable=SC2016 # Backticks are literal Markdown in installed documentation.
assert_contains "$target/.agents/skills/diagnose-codebase/SKILL.md" 'both the `diagnosis` and `agent:queued` labels' 'installed diagnosis skill did not queue findings'
# shellcheck disable=SC2016 # Backticks are literal Markdown in installed documentation.
assert_contains "$target/docs/operations/codebase-diagnosis.md" '`diagnosis` と `agent:queued`' 'installed diagnosis docs did not describe automatic queueing'
assert_contains "$target/.agentic-loop/diagnose-codebase.sh" 'both diagnosis and agent:queued labels' 'installed diagnosis prompt did not request automatic queueing'
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
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$target" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
project_creates=$(grep -c $'project create' "$FAKE_GH_ROOT/calls" || true)
[[ $project_creates -eq 1 ]] || { sed -n '1,120p' "$FAKE_GH_ROOT/calls" >&2; fail "reinstall created the Project $project_creates times"; }
view_creates=$(grep -c $'\tapi graphql -f query=mutation($projectId: ID!, $name: String!)' "$FAKE_GH_ROOT/calls" || true)
[[ $view_creates -eq 4 ]] || { sed -n '1,160p' "$FAKE_GH_ROOT/calls" >&2; fail "reinstall created the Project views $view_creates times"; }
for view in 'Open Issues' 'Closed Issues' 'Open PRs' 'Closed PRs'; do
  [[ $(awk -F '\t' -v name="$view" '$2 == name {count++} END {print count+0}' "$FAKE_GH_ROOT/$state_key.views") -eq 1 ]] || fail "Project view is not idempotent: $view"
done
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open' 'Open Issues view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:closed' 'Closed Issues view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:pr is:open' 'Open PRs view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:pr is:closed' 'Closed PRs view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" "project item-add 7 --owner acme --url https://github.example/acme/installed-project/pull/1" 'existing PR was not added to the Project'
assert_contains "$FAKE_GH_ROOT/calls" "project item-add 7 --owner acme --url https://github.example/acme/installed-project/issues/88" 'existing open Issue was not added to the Project'
assert_contains "$FAKE_GH_ROOT/calls" "project item-add 7 --owner acme --url https://github.example/acme/installed-project/issues/89" 'existing closed Issue was not added to the Project'

# Intake synchronization is immediate, idempotent, and persists temporary Projects failures for reconciliation.
rm -f "$FAKE_GH_ROOT/$state_key.project-failures" "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
FAKE_PROJECT_FAILURES=1 FAKE_GRAPHQL_REMAINING=5000 "$target/bin/agentic-loop" sync-issue 91 >/dev/null
pending_project="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/project-pending"
assert_contains "$pending_project" 'content https://github.com/acme/installed-project/issues/91' 'temporary Project failure was not persisted'
FAKE_PROJECT_FAILURES=1 "$target/bin/agentic-loop" sync-issue 91 >/dev/null
[[ $(grep -Fxc 'https://github.com/acme/installed-project/issues/91' "$FAKE_GH_ROOT/$state_key.project-items") -eq 1 ]] || fail 'repeated immediate synchronization duplicated the Issue item'
printf '91 inbox open none 2026-01-01T00:00:00Z\n' > "$FAKE_GH_ROOT/$state_key.state"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
[[ ! -e $pending_project ]] || fail 'successful reconciliation did not clear the Project retry queue'

printf 'POLL_SECONDS=1\nMAX_WORKERS=2\nLEASE_SECONDS=3\nSTOP_TIMEOUT=10\nSTALE_DAYS=30\n' > "$target/.agentic-loop/config"
"$target/bin/agentic-loop" start
first_pid="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/supervisor.pid"
first_pid=$(cat "$first_pid")
supervisor_service="$XDG_CONFIG_HOME/systemd/user/agentic-loop-supervisor-$(printf '%s' "${target#/}" | tr '/' '-').service"
[[ -f $supervisor_service ]] || fail 'start did not install the repository supervisor service'
assert_contains "$supervisor_service" 'Restart=on-failure' 'supervisor service does not restart after an unexpected exit'
assert_contains "$supervisor_service" "ExecStart=$target/bin/agentic-loop _service" 'supervisor service does not target the repository CLI'
assert_contains "$supervisor_service" "Environment=PATH=$FAKE_BIN:" 'supervisor service PATH does not include the verified Codex directory'
assert_contains "$FAKE_GH_ROOT/systemctl-calls" "enable $(basename "$supervisor_service")" 'supervisor service was not enabled'
"$target/bin/agentic-loop" start
[[ $(cat "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/supervisor.pid") == "$first_pid" ]] || fail 'start duplicated the supervisor'
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'running' <<< "$status_output" || fail 'status did not show the supervisor'
"$target/bin/agentic-loop" stop
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'stopped' <<< "$status_output" || fail 'stop did not drain the supervisor'

# A PID reused by an unrelated process cannot keep a stale supervisor lock alive.
state_root="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop"
mkdir -p "$state_root/supervisor.lock"
printf '%s\n' "$$" > "$state_root/supervisor.pid"
"$target/bin/agentic-loop" start
[[ $(cat "$state_root/supervisor.pid") != "$$" ]] || fail 'start trusted an unrelated process in a stale PID file'
"$target/bin/agentic-loop" stop

# An exhausted GraphQL budget only suppresses Projects; the REST queue still completes work.
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
printf '90 queued open none 2025-01-01T00:00:00Z\n' > "$FAKE_GH_ROOT/$state_key.state"
before_project_adds=$(grep -c $'\tproject item-add' "$FAKE_GH_ROOT/calls" || true)
FAKE_GRAPHQL_REMAINING=499 FAKE_GRAPHQL_RESET=$(date +%s) FAKE_REST_FAILURES=1 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
after_project_adds=$(grep -c $'\tproject item-add' "$FAKE_GH_ROOT/calls" || true)
grep -Eq '^90 completed closed' "$FAKE_GH_ROOT/$state_key.state" || fail 'GraphQL exhaustion stopped the REST Issue loop'
[[ $after_project_adds -eq $before_project_adds ]] || fail 'GraphQL exhaustion did not suppress Projects synchronization'
pending_project="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/project-pending"
[[ -s $pending_project ]] || fail 'suppressed Projects synchronization was not persisted'
rm -f "$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop/graphql-rate-limit"
FAKE_GRAPHQL_REMAINING=5000 AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
[[ ! -e $pending_project ]] || fail 'Projects synchronization did not resume after GraphQL recovery'

# Commit the runtime configuration so worker worktrees start from a realistic default branch.
git -C "$target" add .
git -C "$target" commit --quiet -m configure
git -C "$target" push --quiet
state="$FAKE_GH_ROOT/$state_key.state"
state_root="$(git -C "$target" rev-parse --absolute-git-dir)/agentic-loop"
printf '1 queued open low 2026-01-01T00:00:00Z\n2 queued open critical,low 2026-01-02T00:00:00Z\n3 queued open critical 2025-12-31T00:00:00Z\n4 queued open none 2025-01-01T00:00:00Z\n' > "$state"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 FAKE_STALE_QUEUED_ISSUE=3 "$target/bin/agentic-loop" _supervise
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
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'のハートビートです' 'supervisor did not write its Issue comments in Japanese'
if grep -Eq 'danger-full-access|OPENAI_API_KEY|--add-dir /($| )|--add-dir /home($| )' "$FAKE_GH_ROOT/codex-calls"; then fail 'worker used forbidden Codex configuration or a broad writable path'; fi
[[ ! -e $state_root/worktrees ]] || fail 'worker worktrees were placed inside Git metadata'

# Multiple priority labels use the highest rank; setup creates all priority and stale labels idempotently.
grep -Fq $'label create priority:critical' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the critical priority label'
grep -Fq $'label create priority:low' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the low priority label'
grep -Fq $'label create agent:stale' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the stale state label'

# Only inactive queued Issues are closed; the audit explains safe recovery.
old_date=$(date -u -d '40 days ago' +%Y-%m-%dT%H:%M:%SZ)
recent_date=$(date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%SZ)
printf '20 queued open none 2025-01-01T00:00:00Z %s\n21 queued open none 2025-01-02T00:00:00Z %s\n22 running open none 2025-01-01T00:00:00Z %s\n23 needs-input open none 2025-01-01T00:00:00Z %s\n24 failed open none 2025-01-01T00:00:00Z %s\n25 in-review open none 2025-01-01T00:00:00Z %s\n26 none open none 2025-01-01T00:00:00Z %s\n' "$old_date" "$recent_date" "$old_date" "$old_date" "$old_date" "$old_date" "$old_date" > "$state"
printf '22 <!-- agentic-loop:lease worker=active heartbeat=%s expires=%s -->\n' "$(date +%s)" "$(($(date +%s) + 3600))" > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^20 stale closed' "$state" || fail 'inactive queued Issue was not marked stale and closed'
grep -Eq '^21 completed closed' "$state" || fail 'recent queued Issue was not left available for claim'
for issue_state in '22 running open' '23 needs-input open' '24 failed open' '25 in-review open' '26 none open'; do
  grep -Eq "^$issue_state" "$state" || fail "excluded Issue changed state: $issue_state"
done
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:stale days=30' 'stale audit marker was not recorded'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'reopen' 'stale audit did not explain reopening'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agent:queued' 'stale audit did not explain requeueing'

# STALE_DAYS=0 disables automatic closure while preserving normal claiming.
printf 'POLL_SECONDS=1\nMAX_WORKERS=1\nLEASE_SECONDS=3\nSTOP_TIMEOUT=10\nSTALE_DAYS=0\n' > "$target/.agentic-loop/config"
printf '30 queued open none 2025-01-01T00:00:00Z %s\n31 queued open none 2025-01-02T00:00:00Z %s\n' "$old_date" "$old_date" > "$state"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
[[ $(awk '$2 == "stale" {count++} END {print count+0}' "$state") -eq 0 ]] || fail 'STALE_DAYS=0 marked an Issue stale'
[[ $(awk '$2 == "queued" {count++} END {print count+0}' "$state") -eq 1 ]] || fail 'disabled stale handling did not preserve the remaining queue'

printf 'POLL_SECONDS=1\nMAX_WORKERS=2\nLEASE_SECONDS=3\nSTOP_TIMEOUT=10\nSTALE_DAYS=30\n' > "$target/.agentic-loop/config"

# A linked-worktree worker can fetch, commit, and push through the constructed invocation boundary.
printf '6 queued open\n' > "$state"
FAKE_CODEX_GIT_OPERATIONS=1 "$target/bin/agentic-loop" _worker 6 linked-worktree-worker
git -C "$target" fetch --quiet origin agent/issue-6
git -C "$target" show 'origin/agent/issue-6:worker.txt' | grep -Fxq 'worker change' || fail 'linked-worktree Git metadata operations did not reach the remote'
assert_contains "$FAKE_GH_ROOT/calls" "project item-add 7 --owner acme --url https://github.example/acme/installed-project/pull/6" 'worker PR was not added to the Project'
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

empty=$(empty_repository empty-project)
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$empty" AGENTIC_LOOP_SKIP_START=1 "$PROJECT_ROOT/install.sh"
[[ -f $empty/devbox.json && -f $empty/devbox.lock ]] || fail 'empty repository did not get the pinned development environment'
[[ -x $empty/scripts/check-environment.sh ]] || fail 'empty repository did not get the environment guard'

secret_target="$TEST_ROOT/secret-project"
mkdir -p "$secret_target"
git -C "$secret_target" init --quiet
cp "$PROJECT_ROOT/.agentic-loop/guard-secrets.sh" "$secret_target/guard-secrets.sh"
printf 'token=ghp_%s%s\n' '123456789012345678' '901234567890123456' > "$secret_target/leak.txt"
git -C "$secret_target" add leak.txt
if (cd "$secret_target" && ./guard-secrets.sh --staged) >/dev/null 2>&1; then fail 'secret guard accepted a credential-like value'; fi

printf 'Tests passed.\n'
