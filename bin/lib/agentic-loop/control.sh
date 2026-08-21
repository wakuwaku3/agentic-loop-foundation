# Module: control.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



# --- Issue-level execution control: pause / resume / abort (see docs/
# decisions/0019-issue-level-execution-control.md) ---
#
# This is deliberately a separate layer from ADR 0010's disposal state machine:
# pause/abort control *execution*, never the requirement's lifecycle. Neither
# closes an Issue, and abort routes into the existing agent:parked state
# (ADR 0016) instead of inventing a new terminal disposition. Only an
# authenticated operator with repository write-or-better permission (the same
# gate as dispose/resume) may call them; there is no comment-driven or
# provider-driven trigger, so accidental text in an Issue comment can never be
# mistaken for a command.


control_pause_record_file() { printf '%s/paused/issue-%s' "$STATE_ROOT" "$1"; }


# Record only enum/identity data locally (epoch, actor login, pre-pause
# state): the operator's free-form --reason text is posted to the Issue
# comment but never persisted here, keeping this file inside the same secret
# boundary as every other local-state cache (see docs/decisions/0005).
control_pause_record_write() {
  local issue=$1 actor=$2 from=$3
  mkdir -p "$STATE_ROOT/paused"
  printf '%s\t%s\t%s\n' "$(date +%s)" "$actor" "$from" > "$(control_pause_record_file "$issue")"
}


# Sets CONTROL_PAUSE_AT / CONTROL_PAUSE_ACTOR / CONTROL_PAUSE_FROM on success.
control_pause_record_read() {
  local file
  file=$(control_pause_record_file "$1")
  [[ -r $file ]] || return 1
  IFS=$'\t' read -r CONTROL_PAUSE_AT CONTROL_PAUSE_ACTOR CONTROL_PAUSE_FROM < "$file" || return 1
  [[ $CONTROL_PAUSE_AT =~ ^[0-9]+$ && -n $CONTROL_PAUSE_ACTOR && -n $CONTROL_PAUSE_FROM ]]
}


control_pause_record_clear() { rm -f "$(control_pause_record_file "$1")"; }


# Re-run the same Git/GitHub observation worker() uses on every resume
# (resume_probe) and refresh the one durable handoff comment from it. This is
# the "checkpoint" pause/abort leave behind: never a self-report, always
# derived from observable branch/PR/check state, so it carries no secrets and
# needs no extra guard pass.
control_checkpoint() {
  local issue=$1 repository default_branch branch worktree
  mkdir -p "$STATE_ROOT/workers"
  repository=$(repo_name) || return 0
  default_branch=$(repo_api '' --jq .default_branch 2>/dev/null) || return 0
  [[ -n $default_branch ]] || return 0
  branch="agent/issue-$issue"
  worktree="$WORKTREE_ROOT/issue-$issue"
  git -C "$REPO_ROOT" fetch origin "$default_branch" >/dev/null 2>&1 || true
  resume_probe "$issue" "$branch" "$worktree" "$default_branch" "$repository" || true
  resume_handoff_write "$issue" "$branch" || true
}


# Cooperatively then forcibly stop this host's local worker for `issue`, if
# any, and release every piece of local ownership state. A stop request is a
# marker the worker checks only at its own stage boundaries (see worker_state.
# sh); an active worker_critical section (an in-flight write sequence the
# worker itself marked unsafe to interrupt) is waited out first, bounded by
# PAUSE_GRACE_SECONDS, before any signal is sent at all. Mirrors dispose.sh's
# dispose_stop_local_worker, but cooperative-first and reusable by both pause
# and abort.
control_drain_local_worker() {
  local issue=$1 pidfile="$STATE_ROOT/workers/$1.pid" pid waited=0
  worker_request_stop "$issue"
  while worker_critical_active "$issue" && (( waited < PAUSE_GRACE_SECONDS )); do sleep 1; waited=$((waited + 1)); done
  if [[ -r $pidfile ]] && read -r pid < "$pidfile" && [[ $pid =~ ^[0-9]+$ ]]; then
    waited=0
    while kill -0 "$pid" 2>/dev/null && (( waited < PAUSE_GRACE_SECONDS )); do sleep 1; waited=$((waited + 1)); done
    if kill -0 "$pid" 2>/dev/null; then
      signal_process_tree "$pid" TERM
      waited=0
      while kill -0 "$pid" 2>/dev/null && (( waited < 5 )); do sleep 1; waited=$((waited + 1)); done
      kill -0 "$pid" 2>/dev/null && signal_process_tree "$pid" KILL || true
    fi
  fi
  lease_release "$issue" stopping
  control_checkpoint "$issue"
  scope_cache_clear "$issue"; clear_conflict_wait "$issue"; clear_worker_local "$issue"
}


# The pre-pause state a resumed Issue returns to (see the transition table in
# docs/decisions/0019): running/in-review/failed resume through the ordinary
# queue instead of resuming straight back into a worker; needs-input/blocked
# preserve what they were waiting on; queued and an unreadable/corrupt record
# both fall back to the safe default (queued).
control_pause_resume_target() {
  case $1 in
    needs-input | blocked) printf '%s\n' "$1" ;;
    *) printf 'queued\n' ;;
  esac
}


control_pause_allowed_states=(queued running in-review needs-input blocked failed)


control_pause_current_state() {
  local labels=$1 s
  for s in "${control_pause_allowed_states[@]}"; do
    [[ ,$labels, == *",agent:$s,"* ]] && { printf '%s\n' "$s"; return 0; }
  done
  return 1
}


cmd_pause() {
  local issue=${1:-} reason='' actor state labels from
  [[ $issue =~ ^[1-9][0-9]*$ ]] || fail 'pause requires a positive Issue number'
  shift
  while (( $# > 0 )); do case $1 in --reason) reason=${2:-}; shift 2 ;; *) fail 'pause accepts only --reason' ;; esac; done
  actor=$(authorized_operator) || fail 'pause requires authenticated repository write, maintain, or admin permission'
  IFS=$'\t' read -r state labels < <(issue_agent_state "$issue") || fail "cannot read Issue #$issue"
  [[ $state == open ]] || fail "Issue #$issue is closed; pause only applies to an open Issue"
  if [[ ,$labels, == *,agent:paused,* ]]; then say "Issue #$issue は既に一時停止しています。"; return 0; fi
  if [[ ,$labels, == *,agent:parked,* ]]; then fail "Issue #$issue is agent:parked (already non-claim); use abort or resume instead"; fi
  if [[ ,$labels, == *,agent:stopping,* ]]; then fail "Issue #$issue is already draining (agent:stopping); wait for it to settle"; fi
  for state in cancelled superseded duplicate merged; do
    [[ ,$labels, == *",agent:$state,"* ]] && fail "Issue #$issue is disposed ($state); use resume for a disposition, not pause"
  done
  from=$(control_pause_current_state "$labels") || fail "Issue #$issue has no recognized agent:* state to pause"

  local now marker
  now=$(date +%s)
  marker="<!-- agentic-loop:pause schema=1 actor=$actor issue=$issue from=$from at=$now -->"
  case $from in
    running | in-review)
      set_issue_state "$issue" stopping; project_sync_state "$issue" stopping
      comment_issue "$issue" "$marker\n認可済みの一時停止操作を受け付け、workerの安全な停止を開始しました。未保存または未pushの成果物は削除しません。${reason:+ 理由: $reason}"
      control_drain_local_worker "$issue"
      ;;
    *) control_checkpoint "$issue" ;;
  esac
  control_pause_record_write "$issue" "$actor" "$from"
  set_issue_state "$issue" paused; project_sync_state "$issue" paused
  comment_issue "$issue" "$marker\n認可済みの操作によりIssueを \`agent:paused\`（open・非claim）として一時停止しました（一時停止前の状態: $from）。${reason:+ 理由: $reason}Supervisorのqueueとworker slotは塞ぎません。再開するには \`bin/agentic-loop resume $issue\` を実行してください。"
  say "Issue #$issue を一時停止しました。"
}


cmd_abort() {
  local issue=${1:-} reason='' actor state labels
  [[ $issue =~ ^[1-9][0-9]*$ ]] || fail 'abort requires a positive Issue number'
  shift
  while (( $# > 0 )); do case $1 in --reason) reason=${2:-}; shift 2 ;; *) fail 'abort accepts only --reason' ;; esac; done
  actor=$(authorized_operator) || fail 'abort requires authenticated repository write, maintain, or admin permission'
  IFS=$'\t' read -r state labels < <(issue_agent_state "$issue") || fail "cannot read Issue #$issue"
  [[ $state == open ]] || fail "Issue #$issue is closed; abort only applies to an open Issue"
  if [[ ,$labels, == *,agent:parked,* ]]; then say "Issue #$issue は既に agent:parked です。"; return 0; fi
  if [[ ,$labels, == *,agent:stopping,* ]]; then fail "Issue #$issue is already draining (agent:stopping); wait for it to settle"; fi
  for state in cancelled superseded duplicate merged; do
    [[ ,$labels, == *",agent:$state,"* ]] && fail "Issue #$issue is disposed ($state); resume it first if it must be aborted"
  done

  local now marker
  now=$(date +%s)
  marker="<!-- agentic-loop:abort schema=1 actor=$actor issue=$issue at=$now -->"
  case ,$labels, in
    *,agent:running,*|*,agent:in-review,*)
      set_issue_state "$issue" stopping; project_sync_state "$issue" stopping
      comment_issue "$issue" "$marker\n認可済みの中止操作を受け付け、workerの安全な停止を開始しました。未保存または未pushの成果物は削除しません。${reason:+ 理由: $reason}"
      control_drain_local_worker "$issue"
      ;;
    *) control_checkpoint "$issue" ;;
  esac
  control_pause_record_clear "$issue"
  set_issue_state "$issue" parked; project_sync_state "$issue" parked
  comment_issue "$issue" "$marker\n認可済みの操作によりIssueを \`agent:parked\`（open・非claim・人間トリアージ待ち）へ中止しました。${reason:+ 理由: $reason}worktree・branch・PR・commitは削除していません。実行を再開するには \`bin/agentic-loop resume $issue\`、要求そのものを終了するには \`bin/agentic-loop dispose $issue --reason ...\` を使用してください。"
  clear_attempts "$issue"
  scope_cache_clear "$issue"; clear_conflict_wait "$issue"; clear_worker_local "$issue"
  say "Issue #$issue を中止し、agent:parked へ移しました。"
}


# Called from dispose.sh's cmd_resume for an open agent:paused Issue. Re-
# verifies lease/worktree/branch/PR/checks before resuming (see docs/
# decisions/0019): a still-active worker (local process or a valid lease from
# any host) or an unsafe/undecided Git observation both refuse the resume
# without touching Label/Git state.
control_resume_paused() {
  local issue=$1 actor=$2 from target repository default_branch branch worktree
  control_pause_record_read "$issue" && from=$CONTROL_PAUSE_FROM || from=queued
  issue_genuinely_running "$issue" && fail "Issue #$issue still appears to have an active worker (local process or a valid lease); wait for it to stop before resuming"
  mkdir -p "$STATE_ROOT/workers"
  repository=$(repo_name) || fail 'cannot resolve the target repository'
  default_branch=$(repo_api '' --jq .default_branch) || fail 'cannot resolve the default branch'
  branch="agent/issue-$issue"
  worktree="$WORKTREE_ROOT/issue-$issue"
  git -C "$REPO_ROOT" fetch origin "$default_branch" >/dev/null 2>&1 || true
  resume_probe "$issue" "$branch" "$worktree" "$default_branch" "$repository"
  case $RESUME_PHASE in
    unsafe-foreign) fail "Issue #$issue's dedicated worktree/branch is in an unsafe state; inspect \`git worktree list\` / \`git branch -a\` before resuming" ;;
    needs-decision) fail "Issue #$issue's branch/PR observation needs a human decision (see the agentic-loop:handoff comment) before resuming" ;;
  esac
  resume_handoff_write "$issue" "$branch"
  target=$(control_pause_resume_target "$from")
  control_pause_record_clear "$issue"
  set_issue_state "$issue" "$target"; project_sync_state "$issue" "$target"
  comment_issue "$issue" "<!-- agentic-loop:resume schema=1 actor=$actor issue=$issue at=$(date +%s) -->\n認可済みの再開操作により、lease・worktree・branch・PR・checksを再検証したうえで \`agent:paused\` のIssueを \`agent:$target\` として再開しました（一時停止前の状態: $from）。"
  say "Issue #$issue を再開しました（agent:$target）。"
}
