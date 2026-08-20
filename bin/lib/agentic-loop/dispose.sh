# Module: dispose.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



# Disposal is deliberately a separate state machine.  A worker result is never
# accepted as authority: only the identity authenticated to gh with repository
# write-or-better permission may close, replace, or consolidate a requirement.
authorized_operator() {
  local login permission
  # workload-boundary: gh api user reads the caller's own identity, not a repository resource; repo_api only targets repos/OWNER/REPO
  login=$(gh api user --jq .login 2>/dev/null | tr -d '[:space:]') || return 1
  [[ -n $login ]] || return 1
  permission=$(repo_api "collaborators/$login/permission" --jq .permission 2>/dev/null | tr -d '[:space:]') || return 1
  case $permission in write|maintain|admin) printf '%s\n' "$login" ;; *) return 1 ;; esac
}


issue_agent_state() {
  repo_api "issues/$1" --jq '[.state, ([.labels[].name | select(startswith("agent:"))] | join(","))] | @tsv'
}


dispose_marker() { printf '<!-- agentic-loop:dispose schema=1 actor=%s reason=%s issue=%s target=%s at=%s -->' "$1" "$2" "$3" "${4:-none}" "$5"; }


dispose_stop_local_worker() {
  local issue=$1 pidfile="$STATE_ROOT/workers/$1.pid" pid waited=0
  [[ -r $pidfile ]] || return 0
  read -r pid < "$pidfile" || return 0
  [[ $pid =~ ^[0-9]+$ ]] || return 0
  kill -TERM "-$pid" 2>/dev/null || true
  while kill -0 "$pid" 2>/dev/null && (( waited < STOP_TIMEOUT )); do sleep 1; waited=$((waited + 1)); done
  kill -0 "$pid" 2>/dev/null && kill -KILL "-$pid" 2>/dev/null || true
  lease_release "$issue" stopping
  scope_cache_clear "$issue"; clear_conflict_wait "$issue"; clear_worker_local "$issue"
}


dispose_validate_target() {
  local issue=$1 reason=$2 target=$3 state labels
  [[ $reason == cancelled ]] && [[ -z $target ]] && return 0
  [[ $target =~ ^[1-9][0-9]*$ && $target != "$issue" ]] || return 1
  IFS=$'\t' read -r state labels < <(issue_agent_state "$target" 2>/dev/null) || return 1
  [[ $state == open ]] || return 1
  case ,$labels, in *,agent:cancelled,*|*,agent:superseded,*|*,agent:duplicate,*|*,agent:merged,*) return 1 ;; esac
}


# Native dependency transfer is fail-closed: consolidation must never make a
# previously blocked requirement claimable merely because its source was closed.
dispose_transfer_dependencies() {
  local source=$1 target=$2 dependency dependency_id target_dependencies result
  [[ -n $target ]] || return 0
  # GitHub's issue-dependencies REST API returns the Issues blocking source.
  # An unavailable endpoint/scope is not treated as an empty list.
  # workload-unbounded: manually-curated per-Issue dependency lists stay small by construction; bound=blocked_by count
  dependencies=$(repo_api "issues/$source/dependencies/blocked_by" --method GET -f per_page=100 --paginate --jq '.[].number' 2>/dev/null) || return 1
  while IFS= read -r dependency; do
    [[ -z $dependency ]] && continue
    [[ $dependency =~ ^[1-9][0-9]*$ && $dependency != "$target" ]] || return 1
    # issue_id is a typed-integer property requiring the blocking Issue's
    # database id, not its Issue number (Issue #252): the GET above returns
    # numbers, so resolve each to its id before the POST.
    dependency_id=$(repo_api "issues/$dependency" --jq .id 2>/dev/null) || return 1
    if ! result=$(repo_api "issues/$target/dependencies/blocked_by" --method POST -F issue_id="$dependency_id" 2>&1); then
      say "dispose_transfer_dependencies: issues/$target/dependencies/blocked_by への登録に失敗しました(issue_id=$dependency_id): $result" >&2
      return 1
    fi
  done <<< "$dependencies"
  # workload-unbounded: same as above, bound=blocked_by count
  target_dependencies=$(repo_api "issues/$target/dependencies/blocked_by" --method GET -f per_page=100 --paginate --jq '.[].number' 2>/dev/null) || return 1
  while IFS= read -r dependency; do
    [[ -z $dependency ]] && continue
    grep -Fxq "$dependency" <<< "$target_dependencies" || return 1
  done <<< "$dependencies"
}


cmd_dispose() {
  local issue=${1:-} reason='' target='' actor state labels now marker existing
  [[ $issue =~ ^[1-9][0-9]*$ ]] || fail 'dispose requires a positive Issue number'
  shift
  while (( $# > 0 )); do case $1 in --reason) reason=${2:-}; shift 2 ;; --target) target=${2:-}; shift 2 ;; *) fail 'dispose accepts only --reason and --target' ;; esac; done
  case $reason in cancelled) [[ -z $target ]] || fail 'cancelled does not accept --target' ;; superseded|duplicate|merged) ;; *) fail 'invalid disposal reason' ;; esac
  actor=$(authorized_operator) || fail 'dispose requires authenticated repository write, maintain, or admin permission'
  IFS=$'\t' read -r state labels < <(issue_agent_state "$issue") || fail "cannot read Issue #$issue"
  [[ $state == open ]] || fail "Issue #$issue is already closed; use resume only for an authorized disposition"
  # Existing terminal labels are immutable evidence. Repeating the same exact
  # operation is harmless; changing its reason/target requires a new Issue.
  for existing in "${DISPOSITION_LABELS[@]}"; do
    if [[ ,$labels, == *",agent:$existing,"* ]]; then
      [[ $existing == "$reason" ]] && { say "Issue #$issue は既に $reason です。"; return 0; }
      fail "Issue #$issue already has an immutable disposition: $existing"
    fi
  done
  dispose_validate_target "$issue" "$reason" "$target" || fail 'target must be a distinct, open, non-disposed Issue in this repository'
  # A merged implementation is never silently discarded.
  if [[ $labels == *agent:completed* ]] && repo_api pulls --method GET -f state=closed -f head="$(repo_name | cut -d/ -f1):agent/issue-$issue" --jq 'any(.[]; .merged_at != null)' 2>/dev/null | grep -Fxq true; then
    fail 'a completed Issue with a merged pull request cannot be disposed; create a revert/follow-up Issue instead'
  fi
  now=$(date +%s); marker=$(dispose_marker "$actor" "$reason" "$issue" "$target" "$now")
  case ,$labels, in *,agent:running,*|*,agent:in-review,*)
    set_issue_state "$issue" stopping; project_sync_state "$issue" stopping
    comment_issue "$issue" "$marker\n認可済みの終了操作を受け付け、workerとPRの安全な停止を開始しました。未保存または未pushの成果物は削除しません。"
    dispose_stop_local_worker "$issue"
    ;;
  esac
  if [[ -n $target ]]; then
    dispose_transfer_dependencies "$issue" "$target" || fail 'dependency transfer could not be verified; Issue remains stopping/open for safe recovery'
    comment_issue "$target" "$marker\nIssue #$issue をこのIssueへ統合します。元Issueの本文・全コメント・依存関係も要求として確認してください。出典は $(repo_issue_url "$issue") に保持します。" || fail 'could not write the required target audit record'
  fi
  set_issue_state "$issue" "$reason"; project_sync_state "$issue" "$reason"
  comment_issue "$issue" "$marker\n認可済みの操作により Issue を \`agent:$reason\` として終了します。${target:+統合先: $(repo_issue_url "$target")。} Issue本文・コメント・既存成果物は保持します。"
  repo_api "issues/$issue" --method PATCH -f state=closed -f state_reason=not_planned >/dev/null || fail 'could not close disposed Issue with state_reason=not_planned'
  say "Issue #$issue を $reason として終了しました。"
}


cmd_resume() {
  local issue=${1:-} actor state labels
  [[ $issue =~ ^[1-9][0-9]*$ ]] || fail 'resume requires a positive Issue number'
  actor=$(authorized_operator) || fail 'resume requires authenticated repository write, maintain, or admin permission'
  IFS=$'\t' read -r state labels < <(issue_agent_state "$issue") || fail "cannot read Issue #$issue"
  if [[ $state == closed ]]; then
    case ,$labels, in *,agent:cancelled,*|*,agent:superseded,*|*,agent:duplicate,*|*,agent:merged,*) ;; *) fail 'resume requires a disposition label' ;; esac
    repo_api "issues/$issue" --method PATCH -f state=open >/dev/null || fail 'could not reopen Issue'
    set_issue_state "$issue" queued; project_sync_state "$issue" queued
    comment_issue "$issue" "<!-- agentic-loop:resume schema=1 actor=$actor issue=$issue at=$(date +%s) -->\n認可済みの再開操作により、終了履歴を保持したままIssueを \`agent:queued\` として再開しました。"
    say "Issue #$issue を再開しました。"
  elif [[ $state == open ]] && [[ ,$labels, == *,agent:parked,* ]]; then
    set_issue_state "$issue" queued; project_sync_state "$issue" queued
    comment_issue "$issue" "<!-- agentic-loop:resume schema=1 actor=$actor issue=$issue at=$(date +%s) -->\n認可済みの再開操作により、\`agent:parked\` のIssueを \`agent:queued\` として再投入しました。"
    say "Issue #$issue を再投入しました。"
  elif [[ $state == open ]] && [[ ,$labels, == *,agent:paused,* ]]; then
    control_resume_paused "$issue" "$actor"
  else
    fail 'resume is only for a closed disposed Issue, an open agent:parked Issue, or an open agent:paused Issue'
  fi
}
