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

# write_queue_config FILE KEY=VAL ... -> render a [queue] TOML config
write_queue_config() {
  local file=$1 kv key val
  shift
  printf '[queue]\n' > "$file"
  for kv in "$@"; do
    key=${kv%%=*}
    val=${kv#*=}
    printf '%s = %s\n' "${key,,}" "$val" >> "$file"
  done
}

# scope_field ARGS -> base64 of an agentic-loop:scope marker for state column 8
# (the fake gh's simulated Issue body), e.g. scope_field 'paths=bin/agentic-loop'
scope_field() { printf '<!-- agentic-loop:scope %s -->' "$1" | base64 -w0; }

# dependency_field REFS -> base64 of a "Blocked by:" body line for state column
# 8, e.g. dependency_field '#12, #34'
dependency_field() { printf 'Blocked by: %s' "$1" | base64 -w0; }

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
metrics_issues="$FAKE_GH_ROOT/$key.metrics-issues"
metrics_events="$FAKE_GH_ROOT/$key.metrics-events"
metrics_pulls="$FAKE_GH_ROOT/$key.metrics-pulls"
printf '%s\t%s\n' "$PWD" "$*" >> "$FAKE_GH_ROOT/calls"
if [[ $* == *--slurp* && $* == *--jq* ]]; then
  printf 'the `--slurp` option is not supported with `--jq`\n' >&2
  exit 1
fi
case "${1:-} ${2:-}" in
  'auth status') [[ ${FAKE_GH_AUTH_FAIL:-0} == 0 ]] ;;
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
    if [[ $endpoint == issues && $method == GET && $* == *fromdateiso8601* ]]; then
      # bin/agentic-loop metrics collection A: hand-authored fixture rows are
      # returned verbatim (the fake gh never runs real jq), keyed off the
      # fromdateiso8601 call that only this query makes.
      [[ ${FAKE_METRICS_ISSUES_FAIL:-0} == 0 ]] || { printf 'HTTP 503: Service Unavailable\n' >&2; exit 1; }
      cat "$metrics_issues" 2>/dev/null || true
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
    elif [[ $endpoint == issues && $method == GET && $form_state == all ]]; then
      awk -v slug="$slug" '{print "https://github.example/" slug "/issues/" $1}' "$state" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET && -z $wanted ]]; then
      # status-snapshot: every open Issue, classified purely by its own state
      # word (see bin/agentic-loop's status_snapshot_fetch), with the same
      # category/priority ranks as the agent:queued created_at query above.
      awk '$3 != "closed" {
        category=5; if ($7 ~ /(^|,)loop-continuity(,|$)/) category=0; else if ($7 ~ /(^|,)confidentiality-incident(,|$)/) category=1; else if ($7 ~ /(^|,)integrity-incident(,|$)/) category=2; else if ($7 ~ /(^|,)availability-incident(,|$)/) category=3; else if ($7 ~ /(^|,)feature(,|$)/) category=4
        priority=4; if ($4 ~ /(^|,)critical(,|$)/) priority=0; else if ($4 ~ /(^|,)high(,|$)/) priority=1; else if ($4 ~ /(^|,)medium(,|$)/) priority=2; else if ($4 ~ /(^|,)low(,|$)/) priority=3
        created=($5 == "" ? $1 : $5)
        state="other"
        if ($2 == "running") state="running"
        else if ($2 == "queued") state="queued"
        else if ($2 == "needs-input") state="needs-input"
        else if ($2 == "failed") state="failed"
        else if ($2 == "in-review") state="in-review"
        else if ($2 == "blocked") state="blocked"
        print $1 "\t" "Fake issue " $1 "\t" state "\t" category "\t" priority "\t" created
      }' "$state" 2>/dev/null || true
    elif [[ $endpoint == issues && $method == GET ]]; then
      case $wanted in
        agent:queued)
          if [[ $* == *updated_at* ]]; then
            awk '$2 == "queued" && $3 != "closed" && $6 != "" {print $1 "\t" $6}' "$state"
          elif [[ $* == *created_at* ]]; then
            awk '$2 == "queued" && $3 != "closed" {
              category=5; if ($7 ~ /(^|,)loop-continuity(,|$)/) category=0; else if ($7 ~ /(^|,)confidentiality-incident(,|$)/) category=1; else if ($7 ~ /(^|,)integrity-incident(,|$)/) category=2; else if ($7 ~ /(^|,)availability-incident(,|$)/) category=3; else if ($7 ~ /(^|,)feature(,|$)/) category=4
              priority=4; if ($4 ~ /(^|,)critical(,|$)/) priority=0; else if ($4 ~ /(^|,)high(,|$)/) priority=1; else if ($4 ~ /(^|,)medium(,|$)/) priority=2; else if ($4 ~ /(^|,)low(,|$)/) priority=3
              created=($5 == "" ? $1 : $5); print category "\t" priority "\t" created "\t" $1 "\t" $8
            }' "$state"
            if [[ -n ${FAKE_STALE_QUEUED_ISSUE:-} ]] && ! awk -v n="$FAKE_STALE_QUEUED_ISSUE" '$1 == n && $2 == "queued" {found=1} END {exit !found}' "$state"; then
              awk -v n="$FAKE_STALE_QUEUED_ISSUE" '$1 == n {print "0\t" ($5 == "" ? $1 : $5) "\t" $1}' "$state"
            fi
          else awk '$2 == "queued" && $3 != "closed" {print $1}' "$state"; fi ;;
        agent:running)
          if [[ $* == *title* ]]; then awk '$2 == "running" && $3 != "closed" {print "#" $1 " Fake issue " $1}' "$state"
          elif [[ $* == *'.body'* ]]; then awk '$2 == "running" && $3 != "closed" {print $1 "\t" $8}' "$state"
          else awk '$2 == "running" && $3 != "closed" {print $1}' "$state"; fi ;;
        agent:needs-input) awk '$2 == "needs-input" && $3 != "closed" {print $1}' "$state" ;;
        agent:failed) awk '$2 == "failed" && $3 != "closed" {print $1}' "$state" ;;
        agent:blocked) awk '$2 == "blocked" && $3 != "closed" {print $1 "\t" $8}' "$state" ;;
      esac
    elif [[ $endpoint =~ ^issues/([0-9]+)/labels$ && $method == PUT ]]; then
      issue=${BASH_REMATCH[1]}; payload=$(if [[ -n $input_file && $input_file != - ]]; then cat "$input_file"; else cat; fi); target=$(sed -n 's/.*"agent:\([^"]*\)".*/\1/p' <<< "$payload"); category=$(grep -o 'category:[a-z-]*' <<< "$payload" | head -n 1 | cut -d: -f2 || true)
      ( flock 9; awk -v n="$issue" -v s="$target" -v c="$category" '{if ($1 == n) {if (s != "") $2=s; if (c != "") $7=c} print}' "$state" > "$state.$$.tmp" && mv "$state.$$.tmp" "$state" ) 9> "$state.lock"
    elif [[ $endpoint =~ ^issues/comments/([0-9]+)$ && $method == PATCH ]]; then
      cid=${BASH_REMATCH[1]}
      body=''; for arg in "$@"; do [[ $arg == body=* ]] && body=${arg#body=}; done
      ( flock 9
        mapfile -t comment_lines < "$comments" 2>/dev/null || comment_lines=()
        if (( cid >= 1 && cid <= ${#comment_lines[@]} )); then
          comment_lines[cid-1]="${comment_lines[cid-1]%% *} $body"
          printf '%s\n' "${comment_lines[@]}" > "$comments"
        fi
      ) 9> "$comments.lock"
    elif [[ $endpoint =~ ^issues/([0-9]+)/comments$ ]]; then
      issue=${BASH_REMATCH[1]}
      if [[ $method == POST ]]; then
        body=''; for arg in "$@"; do [[ $arg == body=* ]] && body=${arg#body=}; done
        ( flock 9
          printf '%s %s\n' "$issue" "$body" >> "$comments"
          if [[ $* == *"--jq .id"* ]]; then wc -l < "$comments" | tr -d '[:space:]'; printf '\n'; fi
        ) 9> "$comments.lock"
      elif [[ $* == *needs-input* ]]; then
        if tail -n 1 "$comments" 2>/dev/null | grep -Fq USER_REPLY; then printf 'true\n'; else printf 'false\n'; fi
      else tail -n 1 "$comments" 2>/dev/null || true; fi
    elif [[ $endpoint =~ ^issues/([0-9]+)/dependencies/blocked_by$ && $method == GET ]]; then
      issue=${BASH_REMATCH[1]}
      if [[ ${FAKE_DEPENDENCIES_FORBIDDEN:-0} == 1 ]]; then printf 'HTTP 403: Forbidden\n' >&2; exit 1; fi
      if [[ ${FAKE_DEPENDENCIES_UNAVAILABLE:-0} == 1 ]]; then printf 'HTTP 404: Not Found\n' >&2; exit 1; fi
      if [[ ${FAKE_DEPENDENCIES_TRANSIENT_FAIL:-0} == 1 ]]; then printf 'HTTP 503: Service Unavailable\n' >&2; exit 1; fi
      links=$(awk -v n="$issue" '$1 == n {print $2}' "$FAKE_GH_ROOT/$key.dep-links" 2>/dev/null || true)
      [[ -n $links ]] && tr ',' '\n' <<< "$links"
      true
    elif [[ $endpoint =~ ^issues/([0-9]+)$ ]]; then
      issue=${BASH_REMATCH[1]}
      if [[ $method == PATCH && $form_state == closed ]]; then
        ( flock 9; awk -v n="$issue" '{if ($1 == n) $3="closed"; print}' "$state" > "$state.$$.tmp" && mv "$state.$$.tmp" "$state" ) 9> "$state.lock"
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
      elif [[ $* == *'join(",")'* ]]; then
        awk -v n="$issue" '$1 == n {split($7,c,","); out=""; for(i in c) if(c[i] != "" && c[i] != "none") out=out (out=="" ? "" : ",") "category:" c[i]; print out}' "$state"
      elif [[ $* == *'startswith("category:") | not'* ]]; then
        category=$(grep -o 'category:[a-z-]*' <<< "$*" | tail -n 1); printf '["agent:queued","%s"]\n' "$category"
      elif [[ $* == *'[.labels[].name]'* ]]; then
        awk -v n="$issue" '$1 == n {printf "[\"agent:%s\"", $2; split($4,p,","); for(i in p) if(p[i] != "" && p[i] != "none") printf ",\"priority:%s\"",p[i]; split($7,c,","); for(i in c) if(c[i] != "" && c[i] != "none") printf ",\"category:%s\"",c[i]; print "]"}' "$state"
      elif [[ $* == *starts*with* ]]; then
        target=$(sed -n 's/.*\["agent:\([^"]*\)"\].*/\1/p' <<< "$*"); printf '["agent:%s"]\n' "$target"
      else awk -v n="$issue" '$1 == n {print "agent:" $2}' "$state"; fi
    elif [[ $endpoint =~ ^commits/.+/check-runs$ ]]; then
      [[ -n ${FAKE_RESUME_CHECKS:-} ]] && printf '%s\n' "$FAKE_RESUME_CHECKS"
    elif [[ $endpoint == pulls && $* == *'resume-probe-prs'* ]]; then
      printf '%s\x1f%s\x1f%s\x1f%s\x1f%s\n' \
        "${FAKE_RESUME_MERGED_PR:-}" "${FAKE_RESUME_MERGED_SHA:-}" "${FAKE_RESUME_MERGED_URL:-}" \
        "${FAKE_RESUME_OPEN_PR:-}" "${FAKE_RESUME_OPEN_URL:-}"
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
    elif [[ -z $endpoint ]]; then
      if [[ $* == *permissions.push* ]]; then printf 'true\n'; else printf 'main\n'; fi
    fi ;;
  'api rate_limit')
    printf '%s\t%s\t%s\n' "${FAKE_GRAPHQL_REMAINING:-5000}" "${FAKE_GRAPHQL_RESET:-$(($(date +%s) + 3600))}" "${FAKE_CORE_REMAINING:-5000}"
    ;;
  'api graphql')
    if [[ $* == *'fields(first: 100)'* ]]; then
      [[ ${FAKE_PROJECT_FAIL:-0} == 0 ]] && printf 'true\n'
    elif [[ $* == *createProjectV2View* ]]; then
      name=''
      for ((i=1; i<=$#; i++)); do [[ ${!i} == name=* ]] && name=${!i#name=}; done
      id="PV_$(printf '%s' "$name" | tr ' ' '_')"
      printf '%s\t%s\n' "$id" "$name" >> "$views"
      printf '%s\n' "$id"
    elif [[ $* == *'views(first: 100)'* ]]; then
      cat "$views" 2>/dev/null || true
    elif [[ $* == *updateProjectV2View* && ${FAKE_PROJECT_VIEW_UPDATE_FAILURE:-0} == 1 ]]; then
      exit 1
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
  'project view') [[ ${FAKE_PROJECT_FAIL:-0} == 0 ]] && printf 'PVT_fake\n' ;;
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
    gh issue create --title '診断所見' --body 'diagnosis-finding' --label diagnosis --label category:improvement --label agent:queued >/dev/null
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
sleep "$sleep_value"
printf '%s\n' "${FAKE_CODEX_RESULT:-AGENTIC_LOOP_RESULT=completed}" > "$output"
FAKE_CODEX
cat > "$FAKE_BIN/claude" <<'FAKE_CLAUDE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_GH_ROOT/claude-calls"
[[ ${1:-} == --version || ${1:-} == --help ]] && { printf 'claude 1.0.0\n'; exit 0; }
[[ $* == *--print* ]] || { printf 'claude worker must run non-interactively\n' >&2; exit 2; }
[[ $* == *--dangerously-skip-permissions* ]] || { printf 'claude worker must not block on permissions\n' >&2; exit 2; }
sleep "${FAKE_CLAUDE_SLEEP:-0}"
# The Claude worker captures the final message from stdout into the result file.
# With --output-format json the sentinel stays inside .result and usage fields
# accompany it for the token analysis record.
if [[ $* == *'--output-format json'* ]]; then
  printf '{"type":"result","result":"%s","usage":{"input_tokens":123,"output_tokens":45,"cache_read_input_tokens":10},"total_cost_usd":0.0123}\n' "${FAKE_CLAUDE_RESULT:-AGENTIC_LOOP_RESULT=completed}"
else
  printf '%s\n' "${FAKE_CLAUDE_RESULT:-AGENTIC_LOOP_RESULT=completed}"
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
# With --format json the worker reads the sentinel from a text part and token
# telemetry from a step-finish part; otherwise it prints the plain final message.
if [[ $* == *'--format json'* ]]; then
  printf '{"part":{"type":"text","text":"%s"}}\n' "${FAKE_OPENCODE_RESULT:-AGENTIC_LOOP_RESULT=completed}"
  printf '{"part":{"type":"step-finish","tokens":{"input":200,"output":50,"reasoning":5,"cache":{"read":10,"write":0}},"cost":0.02}}\n'
else
  printf '%s\n' "${FAKE_OPENCODE_RESULT:-AGENTIC_LOOP_RESULT=completed}"
fi
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
exit 0
FAKE_DEVBOX
chmod +x "$FAKE_BIN/gh" "$FAKE_BIN/codex" "$FAKE_BIN/claude" "$FAKE_BIN/opencode" "$FAKE_BIN/systemctl" "$FAKE_BIN/systemd-escape" "$FAKE_BIN/devbox"
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
[[ $view_creates -eq 10 ]] || { sed -n '1,220p' "$FAKE_GH_ROOT/calls" >&2; fail "reinstall created the Project views $view_creates times"; }
for view in 'Triage' 'Queue' 'Active' 'Needs input' 'Recovery' 'Recently completed' 'Open PRs' 'Closed PRs' 'All open issues' 'All closed issues'; do
  [[ $(awk -F '\t' -v name="$view" '$2 == name {count++} END {print count+0}' "$FAKE_GH_ROOT/$state_key.views") -eq 1 ]] || fail "Project view is not idempotent: $view"
done
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open no:category' 'Triage view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open label:"agent:queued"' 'Queue view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open label:"agent:running","agent:in-review"' 'Active view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open label:"agent:needs-input"' 'Needs input view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue label:"agent:failed","agent:stale"' 'Recovery view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue label:"agent:completed" updated:@today-30d' 'Recently completed view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:pr is:open' 'Open PRs view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:pr is:closed' 'Closed PRs view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:open' 'All open issues view filter was not configured'
assert_contains "$FAKE_GH_ROOT/calls" 'filter=is:issue is:closed' 'All closed issues view filter was not configured'
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

# Projects APIの一時的または権限制約による失敗はIssueキューのsetupを停止しない。
FAKE_PROJECT_VIEW_UPDATE_FAILURE=1 AGENTIC_LOOP_SKIP_START=1 "$target/bin/agentic-loop" setup >/dev/null

write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
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

# Doctor is read-only, reports a healthy installation, and provides valid JSON.
before_doctor=$(git -C "$target" status --porcelain)
doctor_output=$("$target/bin/agentic-loop" doctor)
grep -Fq '[成功] GitHub認証' <<< "$doctor_output" || fail 'doctor did not report healthy GitHub authentication'
grep -Fq '失敗=0' <<< "$doctor_output" || fail 'doctor did not report the healthy state'
doctor_json=$("$target/bin/agentic-loop" doctor --format json)
[[ $doctor_json == '{"schema_version":1,"summary":{"success":'*'"failure":0},"checks":['*']}' ]] || fail 'doctor JSON is not machine-readable'
[[ $(git -C "$target" status --porcelain) == "$before_doctor" ]] || fail 'doctor modified the repository'

if FAKE_GH_AUTH_FAIL=1 "$target/bin/agentic-loop" doctor >/tmp/agentic-loop-doctor-auth.$$ 2>/dev/null; then fail 'doctor accepted missing GitHub authentication'; fi
grep -Fq '[失敗] GitHub認証' /tmp/agentic-loop-doctor-auth.$$ || fail 'doctor did not classify missing authentication'
rm -f /tmp/agentic-loop-doctor-auth.$$

FAKE_PROJECT_FAIL=1 "$target/bin/agentic-loop" doctor > /tmp/agentic-loop-doctor-project.$$ || fail 'optional Project drift failed doctor'
grep -Fq '[警告] GitHub Project同期' /tmp/agentic-loop-doctor-project.$$ || fail 'doctor did not warn about Project drift'
rm -f /tmp/agentic-loop-doctor-project.$$

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

# Category is the primary queue key, followed by priority, creation time, and Issue number.
# unknown_scope=open disables change-scope conflict avoidance here: this fixture
# declares no scope for any Issue and exercises only the ordering, which the
# scope filter must never reorder (a dedicated set of scope tests covers
# conflict avoidance itself, further below).
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=11 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
printf '101 queued open low 2026-01-01T00:00:00Z none loop-continuity\n102 queued open critical 2026-01-01T00:00:00Z none confidentiality-incident\n103 queued open none 2026-01-01T00:00:00Z none integrity-incident\n104 queued open none 2026-01-01T00:00:00Z none availability-incident\n105 queued open none 2026-01-01T00:00:00Z none feature\n106 queued open none 2026-01-01T00:00:00Z none improvement\n107 queued open critical 2026-01-02T00:00:00Z none improvement\n108 queued open low 2025-01-01T00:00:00Z none improvement\n109 queued open critical 2026-01-02T00:00:00Z none improvement\n110 queued open none 2026-01-03T00:00:00Z none none\n111 queued open critical 2026-01-01T00:00:00Z none feature,availability-incident\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
claim_order=$(sed -n 's/^\([0-9][0-9]*\) .*agentic-loop:lease.*/\1/p' "$FAKE_GH_ROOT/$state_key.comments" | awk '!seen[$0]++' | paste -sd, -)
[[ $claim_order == 101,102,103,111,104,105,107,109,108,106,110 ]] || fail "category queue order was incorrect: $claim_order"
grep -Eq '^110 completed closed .* improvement$' "$state" || fail 'missing category was not repaired to improvement'
grep -Eq '^111 completed closed .* availability-incident$' "$state" || fail 'multiple categories did not retain only the highest-ranked category'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'category-reconciled reason=missing' 'missing category repair was not audited'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'category-reconciled reason=multiple selected=availability-incident' 'multiple category repair was not audited'

# unknown_scope=open: this fixture also declares no scope and exercises the
# worker limit and priority ordering, not scope conflict avoidance.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
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
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'Token使用量（分析用）: provider=codex' 'codex worker did not record a token usage analysis entry'

# AGENT_PROVIDER=claude routes the worker to the Claude CLI, confining writes to the worktree.
[[ -f $target/.claude/skills/submit-requirement/SKILL.md ]] || fail 'install did not add the Claude submit-requirement skill'
[[ -f $target/.claude/skills/diagnose-codebase/SKILL.md ]] || fail 'install did not add the Claude diagnosis skill'
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '7 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/claude-calls"
AGENT_PROVIDER=claude AGENTIC_LOOP_RUN_ONCE=1 FAKE_CLAUDE_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^7 completed closed' "$state" || fail 'claude provider did not complete the Issue'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--print' 'claude worker did not run non-interactively'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--dangerously-skip-permissions' 'claude worker did not skip permission prompts'
assert_contains "$FAKE_GH_ROOT/claude-calls" "--add-dir $target/.git" 'claude worker did not grant its exact Git common directory'
assert_contains "$FAKE_GH_ROOT/claude-calls" "--add-dir $target-worktrees/issue-7/.agents" 'claude worker did not grant its exact protected .agents directory'
assert_contains "$FAKE_GH_ROOT/claude-calls" '--output-format json' 'claude worker did not request a machine-readable usage payload'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'Token使用量（分析用）: provider=claude' 'claude worker did not record its token usage on the Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" '入力=123tok' 'claude usage record lacked input token count'
# shellcheck disable=SC2016 # The dollar sign is a literal currency prefix in the recorded usage line.
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'cost=$0.0123' 'claude usage record lacked the reported cost'

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

# Each Issue runs a read-only plan stage before the workspace-write exec stage,
# and an exec that never satisfies completion triggers bounded re-planning.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
printf '20 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
: > "$FAKE_GH_ROOT/codex-calls"
AGENT_PLAN_MAX_RETRIES=1 FAKE_CODEX_RESULT='AGENTIC_LOOP_RESULT=failed' AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^20 failed' "$state" || fail 'exhausted re-planning did not fail the Issue'
plan_passes=$(grep -c -- '--sandbox read-only' "$FAKE_GH_ROOT/codex-calls")
[[ $plan_passes -eq 2 ]] || fail "expected 2 planning passes (initial + 1 retry), got $plan_passes"
exec_passes=$(grep -c -- '--sandbox workspace-write' "$FAKE_GH_ROOT/codex-calls")
[[ $exec_passes -eq 2 ]] || fail "expected 2 exec passes (initial + 1 retry), got $exec_passes"
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'flagshipモデルで計画を見直して再実行' 're-planning was not announced on the Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'stage=plan' 'plan stage usage was not recorded'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'stage=exec' 'exec stage usage was not recorded'
git -C "$target" worktree remove "$target-worktrees/issue-20" --force 2>/dev/null || true
git -C "$target" branch -D agent/issue-20 >/dev/null 2>&1 || true

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

# After MAX_ATTEMPTS the failed Issue is closed as unresolvable, not left parked.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=1 RETRY_COOLDOWN_SECONDS=0
printf '80 failed open none 2026-01-01T00:00:00Z\n' > "$state"
mkdir -p "$state_root/attempts"; printf '1\t0\n' > "$state_root/attempts/issue-80"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^80 failed closed' "$state" || fail 'retry-exhausted Issue was not closed as unresolvable'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:unresolved' 'unresolvable closure was not recorded'
rm -f "$state_root/attempts/issue-80"

# A worker that declines an Issue (unnecessary/impossible) closes it.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
printf '71 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_RESULT='AGENTIC_LOOP_RESULT=declined' "$target/bin/agentic-loop" _supervise
grep -Eq '^71 .* closed' "$state" || fail 'declined Issue was not closed'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:declined' 'decline was not recorded'

# A token/rate-limit exhaustion re-queues the Issue (never failed) and pauses the
# supervisor; claiming resumes once the exhaustion clears.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30
rm -f "$state_root/agent-exhausted"
printf '60 queued open none 2026-01-01T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_RESULT='rate limit reached' "$target/bin/agentic-loop" _supervise
grep -Eq '^60 queued open' "$state" || fail 'exhaustion should re-queue the Issue, not fail it'
if grep -Eq '^60 failed' "$state"; then fail 'exhaustion must not mark the Issue failed'; fi
[[ -r $state_root/agent-exhausted ]] || fail 'exhaustion pause marker was not written'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:exhausted' 'exhaustion was not recorded'
# Paused: the next pass does not claim while exhausted.
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^60 queued open' "$state" || fail 'exhaustion pause did not hold the Issue in queue'
# Recovery: clearing the marker resumes claiming to completion.
rm -f "$state_root/agent-exhausted"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^60 completed closed' "$state" || fail 'claiming did not resume after exhaustion cleared'

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

# --- Change-scope conflict avoidance (Issue #44) ---

write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# Same file: two Issues declaring the same path conflict, so only the
# higher-priority one is claimed; an independent-scope Issue is claimed
# alongside it in the same pass instead of waiting its turn (no unnecessary
# whole-repository serialization).
printf '201 queued open none 2026-01-01T00:00:00Z none none %s\n202 queued open none 2026-01-02T00:00:00Z none none %s\n203 queued open none 2026-01-03T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=bin/agentic-loop')" "$(scope_field 'paths=bin/agentic-loop')" "$(scope_field 'paths=docs/operations')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^201 completed closed' "$state" || fail 'same-file scope: the first declared Issue was not claimed'
grep -Eq '^203 completed closed' "$state" || fail 'independent-scope Issue was not claimed alongside a conflicting one'
grep -Eq '^202 queued open' "$state" || fail 'same-file scope conflict was not detected'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=201 token=bin/agentic-loop' 'scope conflict was not recorded with the counterpart Issue and overlapping token'
[[ -r $state_root/conflict/issue-202 ]] || fail 'conflict-wait state was not persisted for status/Project visibility'

# The conflict resolves once the blocking Issue completes: no permanent
# demotion (starvation) of the Issue that lost the earlier race.
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^202 completed closed' "$state" || fail 'same-file scope conflict was not retried to completion once resolved'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-resolved' 'conflict resolution was not recorded on the Issue'
[[ ! -e $state_root/conflict/issue-202 ]] || fail 'resolved conflict-wait state was not cleared'

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

# Same directory: a file nested under a declared directory scope conflicts
# with it on a "/" path boundary, not merely a shared string prefix.
printf '210 queued open none 2026-01-01T00:00:00Z none none %s\n211 queued open none 2026-01-02T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=docs/')" "$(scope_field 'paths=docs/operations/issue-queue.md')" > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^210 completed closed' "$state" || fail 'same-directory scope: the directory-scoped Issue was not claimed'
grep -Eq '^211 queued open' "$state" || fail 'same-directory scope conflict (nested file under a declared directory) was not detected'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=210' 'directory-scope conflict was not recorded'

# Unknown scope: the safe default (isolated) allows only one undeclared-scope
# Issue to run at a time, without needing to serialize the whole repository.
printf '220 queued open none 2026-01-01T00:00:00Z\n221 queued open none 2026-01-02T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^220 completed closed' "$state" || fail 'unknown scope: the first undeclared-scope Issue was not claimed'
grep -Eq '^221 queued open' "$state" || fail 'default unknown_scope=isolated did not serialize undeclared-scope Issues'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=220 token=unknown' 'unknown-scope conflict was not recorded with its reason'

# unknown_scope=open disables conflict avoidance for undeclared-scope Issues.
{
  printf '[queue]\npoll_seconds = 1\nmax_workers = 2\nlease_seconds = 3\nstop_timeout = 10\nstale_days = 30\n'
  printf 'unknown_scope = "open"\n'
} > "$target/.agentic-loop.toml"
printf '222 queued open none 2026-01-01T00:00:00Z\n223 queued open none 2026-01-02T00:00:00Z\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 FAKE_CODEX_SLEEP=1 "$target/bin/agentic-loop" _supervise
[[ $(awk '$2 == "completed" {count++} END {print count+0}' "$state") -eq 2 ]] || fail 'unknown_scope=open did not let undeclared-scope Issues run in parallel'
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# Running-scope re-evaluation: a queued Issue is blocked while a currently
# running Issue's declared scope overlaps it. rebuild_scope_cache re-derives
# every running Issue's effective scope from GitHub at each Supervisor
# startup, so a later change to the running Issue's declared scope (exactly
# what a real worker drives by posting a refined agentic-loop:scope marker
# while it runs) is picked up automatically on the next poll cycle.
printf '999 running open none 2026-01-01T00:00:00Z none none %s\n241 queued open none 2026-01-01T00:00:00Z none none %s\n' \
  "$(scope_field 'paths=docs')" "$(scope_field 'paths=docs/operations')" > "$state"
printf '999 <!-- agentic-loop:lease worker=scope-running-fixture heartbeat=%s expires=%s -->\n' "$(date +%s)" "$(($(date +%s) + 3600))" > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^241 queued open' "$state" || fail "queued Issue was claimed despite overlapping a running Issue's declared scope"
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:scope-conflict issue=999 token=docs' "conflict against a running Issue's declared scope was not recorded"
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

# doctor rejects an invalid unknown_scope value.
cp "$target/.agentic-loop.toml" "$target/.agentic-loop.toml.valid"
printf '[queue]\nunknown_scope = "sometimes"\n' > "$target/.agentic-loop.toml"
if "$target/bin/agentic-loop" doctor > /tmp/agentic-loop-doctor-scope.$$; then fail 'doctor accepted an invalid unknown_scope value'; fi
grep -Fq '[失敗] 設定値: UNKNOWN_SCOPE' /tmp/agentic-loop-doctor-scope.$$ || fail 'doctor did not classify the invalid unknown_scope value'
mv "$target/.agentic-loop.toml.valid" "$target/.agentic-loop.toml"
rm -f /tmp/agentic-loop-doctor-scope.$$

# status surfaces each running Issue's effective scope and any conflict wait.
# The Supervisor is deliberately left stopped: a live start would call
# rebuild_scope_cache and overwrite this manually seeded fixture.
printf '260 running open\n' > "$state"
mkdir -p "$state_root/scope" "$state_root/conflict"
printf 'path:bin/agentic-loop' > "$state_root/scope/issue-260"
printf '260\tbin/agentic-loop\n' > "$state_root/conflict/issue-261"
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'scope: path:bin/agentic-loop' <<< "$status_output" || fail 'status did not show the running Issue effective scope'
grep -Fq '競合待ちIssue:' <<< "$status_output" || fail 'status did not show a conflict-wait section'
grep -Fq '#261' <<< "$status_output" || fail 'status did not name the waiting Issue'
grep -Fq 'bin/agentic-loop' <<< "$status_output" || fail 'status did not name the overlapping token'
rm -f "$state_root/scope/issue-260" "$state_root/conflict/issue-261"

# --- Issue dependency gating (Issue #41) ---

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
assert_contains "$FAKE_GH_ROOT/calls" 'select(.name == "Blocked") | .id' 'blocked state was not synchronized to the Project Agent status field'
assert_contains "$FAKE_GH_ROOT/calls" 'select(.name == "Blocked by") | .id' 'blocked reason was not written to the Project Blocked by field'

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
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30

# Multiple priority labels use the highest rank; setup creates all priority and stale labels idempotently.
grep -Fq $'label create priority:critical' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the critical priority label'
grep -Fq $'label create priority:low' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the low priority label'
grep -Fq $'label create agent:stale' "$FAKE_GH_ROOT/calls" || fail 'setup did not create the stale state label'
for category in loop-continuity confidentiality-incident integrity-incident availability-incident feature improvement; do
  grep -Fq "label create category:$category" "$FAKE_GH_ROOT/calls" || fail "setup did not create category:$category"
done
assert_contains "$FAKE_GH_ROOT/calls" 'project field-create 7 --owner acme --name Category --data-type SINGLE_SELECT' 'setup did not create the Project Category field'

# Only inactive queued Issues are closed; the audit explains safe recovery.
# unknown_scope=open: this fixture declares no scope for any Issue and
# exercises stale triage, not scope conflict avoidance.
printf 'unknown_scope = "open"\n' >> "$target/.agentic-loop.toml"
old_date=$(date -u -d '40 days ago' +%Y-%m-%dT%H:%M:%SZ)
recent_date=$(date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%SZ)
# agent:failed is deliberately excluded here: it is actively managed by retry_failed, not stale closure.
printf '20 queued open none 2025-01-01T00:00:00Z %s\n21 queued open none 2025-01-02T00:00:00Z %s\n22 running open none 2025-01-01T00:00:00Z %s\n23 needs-input open none 2025-01-01T00:00:00Z %s\n25 in-review open none 2025-01-01T00:00:00Z %s\n26 none open none 2025-01-01T00:00:00Z %s\n' "$old_date" "$recent_date" "$old_date" "$old_date" "$old_date" "$old_date" > "$state"
printf '22 <!-- agentic-loop:lease worker=active heartbeat=%s expires=%s -->\n' "$(date +%s)" "$(($(date +%s) + 3600))" > "$FAKE_GH_ROOT/$state_key.comments"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^20 stale closed' "$state" || fail 'inactive queued Issue was not marked stale and closed'
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
for _ in 1 2 3 4 5 6 7 8 9 10; do
  status_output=$("$target/bin/agentic-loop" status)
  grep -Fq 'phase:' <<< "$status_output" && break
  sleep 0.3
done
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

# A crashed worker's expired lease returns to the queue on the next supervisor start.
printf '9 running open\n' > "$state"
printf '9 <!-- agentic-loop:lease worker=dead heartbeat=1 expires=1 -->\n' > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root"
: > "$state_root/stop.requested"
"$target/bin/agentic-loop" _supervise
grep -Eq '^9 queued open$' "$state" || fail 'expired running Issue was not recovered'

# A worker that keeps dying before finishing (lease expiry / crash, never an
# explicit AGENTIC_LOOP_RESULT=failed) must not requeue forever: once its recorded
# claim attempts reach MAX_ATTEMPTS, recover_expired escalates it to agent:failed
# so retry_failed closes it as unresolvable instead of looping in the queue.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=3 STOP_TIMEOUT=10 STALE_DAYS=30 MAX_ATTEMPTS=2 RETRY_COOLDOWN_SECONDS=0
printf '91 running open\n' > "$state"
printf '91 <!-- agentic-loop:lease worker=dead heartbeat=1 expires=1 -->\n' > "$FAKE_GH_ROOT/$state_key.comments"
mkdir -p "$state_root/attempts"; printf '2\t0\n' > "$state_root/attempts/issue-91"
rm -f "$state_root/stop.requested"
AGENTIC_LOOP_RUN_ONCE=1 "$target/bin/agentic-loop" _supervise
grep -Eq '^91 failed closed$' "$state" || fail 'a worker that kept dying before finishing was not escalated and closed after MAX_ATTEMPTS'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:recover-exhausted' 'lease-death escalation was not recorded on the Issue'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:unresolved' 'escalated Issue was not closed as unresolvable'
[[ ! -e $state_root/attempts/issue-91 ]] || fail 'attempts counter was not cleared after unresolvable closure'

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
for _ in $(seq 1 40); do [[ -e $state_root/workers/15.pid ]] && { graceful_claimed=1; break; }; sleep 0.5; done
[[ $graceful_claimed == 1 ]] || { kill "$sup_pid" 2>/dev/null; fail 'worker was not claimed before the shutdown test'; }
kill -TERM "$sup_pid" 2>/dev/null
wait "$sup_pid" 2>/dev/null || true
grep -Eq '^15 queued' "$state" || fail 'graceful shutdown did not requeue the in-flight Issue'
[[ ! -e $state_root/workers/15.pid ]] || fail 'graceful shutdown left a worker pidfile'
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:shutdown' 'graceful shutdown was not recorded on the Issue'

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

# Adaptive idle backoff: with an empty queue the poll interval grows beyond the
# base toward the ceiling, cutting idle GitHub API reads; a live worker resets it.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 POLL_MAX_SECONDS=8 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -f "$state_root/stop.requested" "$state_root/poll-interval"
"$target/bin/agentic-loop" _supervise &
backoff_sup=$!
backoff_val=0
for _ in $(seq 1 20); do
  backoff_val=$(cat "$state_root/poll-interval" 2>/dev/null || printf '0')
  [[ $backoff_val =~ ^[0-9]+$ ]] && (( backoff_val > 1 )) && break
  sleep 0.5
done
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
for _ in $(seq 1 40); do
  if [[ -r $state_root/workers/50.pid ]]; then hang_worker_pid=$(cat "$state_root/workers/50.pid"); break; fi
  sleep 0.5
done
[[ -n $hang_worker_pid ]] || { kill "$hang_sup_pid" 2>/dev/null; wait "$hang_sup_pid" 2>/dev/null; fail 'hung worker was not claimed before the timeout test'; }
hang_timed_out=0
for _ in $(seq 1 40); do
  grep -Eq '^50 failed' "$state" && { hang_timed_out=1; break; }
  sleep 0.5
done
[[ $hang_timed_out == 1 ]] || { kill "$hang_sup_pid" 2>/dev/null; wait "$hang_sup_pid" 2>/dev/null; fail 'a hung worker was not detected and failed within the configured timeout'; }
[[ ! -e $state_root/workers/50.pid ]] || fail 'a timed-out worker pidfile was not cleared'
# kill -0 also succeeds against a not-yet-reaped zombie, so poll briefly
# instead of asserting on a single sample right after the state flip.
hang_worker_gone=0
for _ in $(seq 1 20); do
  kill -0 "$hang_worker_pid" 2>/dev/null || { hang_worker_gone=1; break; }
  sleep 0.5
done
[[ $hang_worker_gone == 1 ]] || { kill "$hang_sup_pid" 2>/dev/null; wait "$hang_sup_pid" 2>/dev/null; fail 'a timed-out worker process group left an orphan process behind'; }
assert_contains "$FAKE_GH_ROOT/$state_key.comments" 'agentic-loop:worker-timeout' 'the worker-timeout disposition was not audited on the Issue'
hang_queue_progressed=0
for _ in $(seq 1 40); do
  grep -Eq '^51 completed closed' "$state" && { hang_queue_progressed=1; break; }
  sleep 0.5
done
kill -TERM "$hang_sup_pid" 2>/dev/null || true
wait "$hang_sup_pid" 2>/dev/null || true
rm -f "$state_root/stop.requested"
[[ $hang_queue_progressed == 1 ]] || fail 'a hung worker under max_workers=1 blocked the rest of the queue instead of freeing the slot'
grep -Eq '^50 failed' "$state" || fail 'the hung Issue was reclaimed before its retry cooldown elapsed'

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
for _ in $(seq 1 40); do [[ -r $state_root/workers/53.started ]] && { disabled_started_seen=1; break; }; sleep 0.2; done
[[ $disabled_started_seen == 1 ]] || { kill -TERM "-$disabled_worker_pid" 2>/dev/null; wait "$disabled_worker_pid" 2>/dev/null; fail 'test setup did not observe the worker start marker'; }
printf '%s\n' "$disabled_worker_pid" > "$state_root/workers/53.pid"
printf '%s\n' "$(($(date +%s) - 100000))" > "$state_root/workers/53.started"
"$target/bin/agentic-loop" _supervise &
disabled_sup_pid=$!
sleep 3
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

# --- status observability (Issue #42) ---
# The Supervisor is deliberately left stopped for the manual-state scenarios
# below: a live start would call rebuild_scope_cache/recover_expired and
# overwrite these manually seeded fixtures (mirrors the existing scope/
# conflict status test above).
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=2 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
rm -f "$state_root/supervisor.pid" "$state_root/stop.requested" "$state_root/project-pending" \
      "$state_root/budget-paused" "$state_root/core-budget-paused" "$state_root/agent-exhausted"
rmdir "$state_root/supervisor.lock" 2>/dev/null || true
rm -rf "$state_root/workers" "$state_root/scope" "$state_root/conflict" "$state_root/dependency" "$state_root/attempts"
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
grep -Fq '警告: none' <<< "$status_output" || fail 'idle status unexpectedly reported an anomaly'
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
# (category rank, then priority rank, then created_at, then number), and the
# ordering is cross-checked against an actual claim.
write_queue_config "$target/.agentic-loop.toml" POLL_SECONDS=1 MAX_WORKERS=1 LEASE_SECONDS=30 STOP_TIMEOUT=10 STALE_DAYS=30
printf '40 queued open low 2026-01-03T00:00:00Z none none\n41 queued open critical 2026-01-01T00:00:00Z none none\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
queue_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.queued') -eq 2 ]] || fail 'queued status did not count both Issues'
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.claimable') -eq 2 ]] || fail 'queued status did not report both Issues as claimable'
[[ $(printf '%s' "$queue_json" | yq -p json '.queue.candidates[0].issue') -eq 41 ]] || fail 'queue candidate preview did not rank the higher-priority Issue first'
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
printf '50 needs-input open\n51 failed open\n52 blocked open\n53 stale closed\n' > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'needs-input: 1件 (https://github.com/acme/installed-project/issues/50)' <<< "$status_output" || fail 'status did not summarize the needs-input Issue with its URL'
grep -Fq 'failed: 1件 (https://github.com/acme/installed-project/issues/51)' <<< "$status_output" || fail 'status did not summarize the failed Issue with its URL'
grep -Fq 'blocked: 1件 (https://github.com/acme/installed-project/issues/52)' <<< "$status_output" || fail 'status did not summarize the blocked Issue with its URL'
grep -Fq 'stale: 1件 (https://github.com/acme/installed-project/issues/53)' <<< "$status_output" || fail 'status did not summarize the closed agent:stale Issue with its URL'

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
[[ $(git -C "$target" status --porcelain) == "$before_status" ]] || fail 'lease-expired status modified the repository working tree'
lease_json=$("$target/bin/agentic-loop" status --format json)
[[ $(printf '%s' "$lease_json" | yq -p json '.workers[0].lease_expired') == true ]] || fail 'status --format json did not mark the worker lease as expired'
[[ $(printf '%s' "$lease_json" | yq -p json '.anomalies[] | select(.code == "lease-expired") | .subject') == '#60' ]] || fail 'status --format json did not report the lease-expired anomaly'
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
[[ $(git -C "$target" status --porcelain) == "$before_status" ]] || fail 'status modified the repository working tree on corrupted local state'
rm -f "$state_root/supervisor.pid" "$state_root/workers/70.started" "$state_root/workers/70.lease"

# Scenario: a worktree/branch left over from an Issue no longer agent:running
# is flagged, and clears once removed.
git -C "$target" worktree add --quiet -b agent/issue-9999 "$target-worktrees/issue-9999" origin/main
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'residual-worktree' <<< "$status_output" || fail 'status did not flag a residual worktree'
grep -Fq 'residual-branch' <<< "$status_output" || fail 'status did not flag a residual branch'
git -C "$target" worktree remove --force "$target-worktrees/issue-9999"
git -C "$target" branch -D agent/issue-9999 >/dev/null
status_output=$("$target/bin/agentic-loop" status)
grep -Fq 'residual-worktree' <<< "$status_output" && fail 'status kept flagging a removed worktree'
: > "$state"
: > "$FAKE_GH_ROOT/$state_key.comments"
rm -rf "$state_root/workers"

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

secret_target="$TEST_ROOT/secret-project"
mkdir -p "$secret_target"
git -C "$secret_target" init --quiet
cp "$PROJECT_ROOT/.agentic-loop/guard-secrets.sh" "$secret_target/guard-secrets.sh"
printf 'token=ghp_%s%s\n' '123456789012345678' '901234567890123456' > "$secret_target/leak.txt"
git -C "$secret_target" add leak.txt
if (cd "$secret_target" && ./guard-secrets.sh --staged) >/dev/null 2>&1; then fail 'secret guard accepted a credential-like value'; fi

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

t0l=$((window_start + 210000)) # 512: closed+completed but missing a priority: label (jq TSV null-field regression)
t0m=$((window_start + 220000)) # 513: open, but its only lease event and its only PR postdate --as-of (out-of-window event/PR regression)

cat > "$metrics_issues" <<TSV
501	$t0	$((t0 + 400))	closed	category:feature	priority:high	agent:completed
502	$t0b	$((t0b + 500))	closed	category:improvement	priority:medium	agent:completed
503	$t0c	0	open	category:improvement	priority:low	agent:needs-input
504	$t0d	0	open	category:feature	priority:high	agent:running
505	$t0e	$((t0e + 50))	closed	category:improvement	priority:medium	agent:completed
506	$t0f	0	open	category:improvement	priority:low	agent:running
507	$t0g	$((t0g + 30))	closed	category:feature	priority:high	agent:running
508	$t0h	$((t0h + 100))	closed	category:feature	priority:high	agent:running
509	$t0i	0	open	category:improvement	priority:low	agent:queued
510	$t0j	0	open	category:improvement	priority:low	agent:blocked
511	$t0k	$((t0k + 7300))	closed	category:feature	priority:medium	agent:completed
512	$t0l	$((t0l + 40))	closed	category:feature	none	agent:completed
513	$t0m	0	open	category:improvement	priority:low	agent:queued
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
[[ $(mj '.dispositions.completed') -eq 5 ]] || fail 'metrics miscounted completed dispositions (501, 502, 505 by label alone despite zero marker history, 511, 512 with a missing priority label)'
[[ $(mj '.dispositions.declined') -eq 1 ]] || fail 'metrics did not detect a declined Issue behind a stale agent:running label'
[[ $(mj '.dispositions.other') -eq 1 ]] || fail 'metrics did not flag the genuinely unclassifiable closed Issue (507) as other, or a missing priority: label on 512 shifted a later TSV column and corrupted its agent-label disposition'
[[ $(mj '.dispositions.open') -eq 6 ]] || fail 'metrics miscounted still-open Issues'
[[ $(mj '.dispositions.unresolved') -eq 0 && $(mj '.dispositions.stale') -eq 0 ]] || fail 'metrics fabricated an unresolved or stale disposition'
[[ $(mj '.warnings[]') == *label_marker_mismatch* ]] || fail 'metrics did not warn about the label/marker mismatch'

[[ $(mj '.counters.attempts') -eq 10 ]] || fail 'metrics miscounted attempts (one per lease marker; the lease for Issue 513 postdates --as-of and must not count)'
[[ $(mj '.counters.retry') -eq 2 && $(mj '.counters.recovered') -eq 1 && $(mj '.counters.worker_timeout') -eq 1 ]] || fail 'metrics miscounted the retry/recovered/worker-timeout requeue path (Issue 504)'
[[ $(mj '.counters.scope_conflict') -eq 1 && $(mj '.counters.dependency_block') -eq 1 ]] || fail 'metrics miscounted the still-open scope-conflict/dependency-blocked waits (509, 510)'
[[ $(mj '.counters.needs_input_round') -eq 2 ]] || fail 'metrics miscounted needs-input rounds (503 open, 511 answered)'
[[ $(mj '.counters.requeue') -eq 4 ]] || fail 'metrics miscounted total requeues (2 retry + 1 recovered + 1 answer-detected)'
[[ $(mj '.counters.open_attempts') -eq 2 ]] || fail 'metrics miscounted attempts with no terminal marker yet (504 attempt 3, 506; an out-of-window lease must not open an attempt for 513)'
[[ $(mj '.counters.unmerged_pr') -eq 1 ]] || fail 'metrics miscounted unmerged pull requests (PR 10; PR 12 postdates --as-of and must not count)'
[[ $(mj '.counters.exhausted') -eq 0 && $(mj '.counters.replan') -eq 0 && $(mj '.counters.resume') -eq 0 ]] || fail 'metrics fabricated an exhausted, replan, or resume count'

[[ $(mj '.failures.unspecified') -eq 1 ]] || fail 'metrics did not classify the reasonless failed marker (502) as unspecified'
[[ $(mj '."failures"."worker-timeout"') -eq 1 ]] || fail 'metrics did not tally the worker-timeout failure reason (504)'

[[ $(mj '.durations.queue_wait.n') -eq 10 && $(mj '.durations.queue_wait.max') -eq 100 ]] || fail 'metrics computed the wrong queue_wait distribution'
[[ $(mj '.durations.attempt_duration.n') -eq 8 && $(mj '.durations.attempt_duration.max') -eq 890 ]] || fail 'metrics computed the wrong attempt_duration distribution'
[[ $(mj '.durations.open_queue_wait.n') -eq 0 ]] || fail 'metrics fabricated an open_queue_wait sample'
[[ $(mj '.durations.open_needs_input_wait.n') -eq 1 ]] || fail 'metrics did not report the still-open needs-input wait (503)'
[[ $(mj '.durations.open_conflict_wait.n') -eq 1 ]] || fail 'metrics did not report the still-open scope-conflict wait (509)'
[[ $(mj '.durations.open_dependency_wait.n') -eq 1 ]] || fail 'metrics did not report the still-open dependency-blocked wait (510)'
[[ $(mj '.durations.needs_input_wait.n') -eq 1 && $(mj '.durations.needs_input_wait.max') -eq 7200 ]] || fail 'metrics computed the wrong closed needs_input_wait sample (511: 7200s)'
[[ $(mj '.durations.plan_seconds.n') -eq 1 && $(mj '.durations.plan_seconds.max') -eq 50 ]] || fail 'metrics did not read plan_seconds from the enriched usage marker'
[[ $(mj '.durations.exec_seconds.n') -eq 1 && $(mj '.durations.exec_seconds.max') -eq 120 ]] || fail 'metrics did not read exec_seconds from the enriched usage marker'
[[ $(mj '.durations.pr_review_wait.n') -eq 2 && $(mj '.durations.pr_review_wait.max') -eq 7280 ]] || fail 'metrics computed the wrong pr_review_wait distribution (PR 9: 400-120=280, PR 11: 7300-20=7280; PR 10 is unmerged and excluded)'
[[ $(mj '.durations.lead_time.n') -eq 3 && $(mj '.durations.lead_time.max') -eq 7300 ]] || fail 'metrics computed the wrong lead_time distribution (505 is excluded: no completed marker despite the label)'

[[ $(mj '.by_category.feature') -eq 6 && $(mj '.by_category.improvement') -eq 7 ]] || fail 'metrics miscounted by_category'
[[ $(mj '.by_priority.high') -eq 4 && $(mj '.by_priority.medium') -eq 3 && $(mj '.by_priority.low') -eq 5 ]] || fail 'metrics miscounted by_priority'
[[ $(mj '.utilization.busy_seconds') -eq 2525 && $(mj '.utilization.max_workers') -eq 2 ]] || fail 'metrics computed the wrong worker-utilization busy_seconds'

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

printf 'Tests passed.\n'
