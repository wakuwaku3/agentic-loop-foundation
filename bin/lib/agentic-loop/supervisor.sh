# Module: supervisor.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



# jq expression fragment (evaluated with an Issue object as `.`) producing the
# two numeric ranks -- numeric priority then category -- used to order
# agent:queued Issues. Shared by claim_next and cmd_status's claim-candidate
# preview so the two orderings cannot drift apart. The priority value is the
# body marker's maximum in-range N (0-100, unset=0); see docs/decisions/
# 0015-numeric-priority-marker.md.
queue_rank_jq() {
  printf '%s' '('"$(queue_priority_jq)"'), (if any(.labels[]; .name == "category:loop-continuity") then 0 elif any(.labels[]; .name == "category:confidentiality-incident") then 1 elif any(.labels[]; .name == "category:integrity-incident") then 2 elif any(.labels[]; .name == "category:availability-incident") then 3 elif any(.labels[]; .name == "category:bug") then 4 elif any(.labels[]; .name == "category:feature") then 5 else 6 end)'
}


claim_next() {
  local issue pid body body_b64 tokens conflict_with worker
  # shellcheck disable=SC2016 # This is a jq program, not a shell expression.
  while IFS=$'\t' read -r issue body_b64; do
    [[ -n $issue ]] || continue
    if [[ -r $STATE_ROOT/workers/$issue.pid ]]; then
      read -r pid < "$STATE_ROOT/workers/$issue.pid" || true
      [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null && continue
    fi
    body=$(base64 -d <<< "$body_b64" 2>/dev/null || true)
    if ! dependency_status "$issue" "$body"; then
      [[ -n $DEPENDENCY_REASON ]] && mark_dependency_blocked "$issue" "$DEPENDENCY_REASON" "$DEPENDENCY_DETAIL"
      continue
    fi
    tokens=$(resolve_issue_scope "$body")
    if [[ -n $tokens ]] && conflict_with=$(scope_conflicting_issue "$tokens"); then
      record_conflict_wait "$issue" "$conflict_with" "$tokens"
      continue
    fi
    clear_conflict_wait "$issue"
    worker="$(repo_name | tr '/.' '-')-$issue-$(date +%s)-$$-$RANDOM"
    claim_acquire "$issue" "$worker" || continue
    set_issue_state "$issue" running
    if repo_api "issues/$issue" --jq '.labels[].name' | grep -Fxq "$(state_label running)"; then
      record_attempt "$issue"
      scope_cache_write "$issue" "$tokens"
      printf '%s\t%s\n' "$issue" "$worker"
      return 0
    fi
    lease_release "$issue" "$worker"
    clear_worker_local "$issue"
  done < <(
    if [[ -n $SUPERVISOR_SNAPSHOT && -r $SUPERVISOR_SNAPSHOT ]]; then
      # Snapshot rows are number, state, updated, created, body, categories,
      # category_rank, priority_value. Reorder to claim_next's comparator:
      # priority (desc), category, created_at, number, then body.
      snapshot_state_rows queued | awk -F '\t' '{print $8 "\t" $7 "\t" $4 "\t" $1 "\t" $5}' | sort -k1,1nr -k2,2n -k3,3 -k4,4n | cut -f4,5
    else
      repo_api issues --method GET -f state=open -f labels="$(state_label queued)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | ['"$(queue_rank_jq)"', .created_at, .number, (.body // "" | @base64)] | @tsv' 2>/dev/null | sort -k1,1nr -k2,2n -k3,3 -k4,4n | awk -F '\t' '{print $4 "\t" $5}'
    fi
  )
  return 1
}


# Drain this host's local worker for any Issue an operator paused since the
# last poll (see docs/decisions/0019-issue-level-execution-control.md and
# control.sh). `pause` itself already drains a worker it can see directly;
# this closes the remaining race where claim/worker-start happened on this
# host between the operator's read and its own drain attempt, or where the
# operator issued `pause` against a snapshot owned by a different host. Reads
# only the snapshot refresh_supervisor_snapshot already fetched this poll, so
# this adds no extra GitHub API calls.
drain_paused_workers() {
  local issue
  while IFS= read -r issue; do
    [[ -n $issue ]] || continue
    worker_pid_live "$issue" || continue
    control_drain_local_worker "$issue"
  done < <(snapshot_state_rows paused | cut -f1 || true)
}


worker_count() {
  local count=0 pid
  shopt -s nullglob
  for pidfile in "$STATE_ROOT"/workers/*.pid; do
    pid=''
    read -r pid < "$pidfile" 2>/dev/null || true
    if [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then count=$((count + 1)); else rm -f "$pidfile"; fi
  done
  shopt -u nullglob
  printf '%s\n' "$count"
}


# Terminate every worker's process group and requeue its Issue so a shutdown
# leaves no orphaned process and no Issue stuck at agent:running. Best-effort:
# if the requeue API fails, the lease expiry and startup recovery still recover
# the Issue on the next supervisor start.
supervisor_graceful_shutdown() {
  : > "$STATE_ROOT/stop.requested" 2>/dev/null || true
  event_append supervisor stop -
  local pidfile issue pid
  shopt -s nullglob
  for pidfile in "$STATE_ROOT"/workers/*.pid; do
    issue=$(basename "$pidfile" .pid)
    read -r pid < "$pidfile" 2>/dev/null || true
    lease_release "$issue" shutdown
    clear_worker_local "$issue"
    if [[ $pid =~ ^[0-9]+$ ]]; then kill -TERM "-$pid" 2>/dev/null || true; fi
    if [[ $issue =~ ^[0-9]+$ ]]; then
      set_issue_state "$issue" queued 2>/dev/null || true
      comment_issue "$issue" "<!-- agentic-loop:shutdown -->\nSupervisorが停止したため、進行中のIssueを安全にキューへ戻しました。次回のclaimで再開します。" 2>/dev/null || true
    fi
  done
  shopt -u nullglob
}


# Exponential idle backoff (see docs/decisions/0003): the next poll interval
# doubles from POLL_SECONDS toward POLL_MAX_SECONDS for each consecutive idle
# poll, cutting GitHub API reads while the queue is empty. Backoff is disabled
# when POLL_MAX_SECONDS <= POLL_SECONDS.
next_poll_interval() {
  local streak=$1 interval=$POLL_SECONDS b
  if (( POLL_MAX_SECONDS > POLL_SECONDS )); then
    for ((b = 0; b < streak && interval < POLL_MAX_SECONDS; b++)); do interval=$((interval * 2)); done
    (( interval > POLL_MAX_SECONDS )) && interval=$POLL_MAX_SECONDS
  fi
  printf '%s\n' "$interval"
}


supervise() {
  trap 'clear_supervisor_snapshot; rm -f "$STATE_ROOT/supervisor.pid"; rmdir "$STATE_ROOT/supervisor.lock" 2>/dev/null || true' EXIT
  # Graceful shutdown (see docs/decisions/0003): on SIGTERM/SIGINT (systemctl
  # stop, kill, OS/WSL shutdown) stop claiming, terminate each worker's process
  # group so nothing is orphaned, and requeue its Issue so the work is not lost.
  trap 'supervisor_graceful_shutdown; exit 0' TERM INT
  mkdir -p "$STATE_ROOT/workers"
  printf '%s\n' "$$" > "$STATE_ROOT/supervisor.pid"
  event_append supervisor start -
  local idle_streak=0 poll_interval=$POLL_SECONDS
  # Best-effort maintenance must never crash the supervisor (see docs/decisions/
  # 0003): a transient GitHub error (secondary rate limit / HTTP 403 / 5xx)
  # should skip this cycle and be retried next poll, not kill the loop under
  # set -e.
  recover_expired || true
  enforce_worker_timeout || true
  reap_orphan_workers || true
  prune_residual_worktrees || true
  rebuild_scope_cache || true
  rebuild_project_hints || true
  while :; do
    clear_supervisor_snapshot
    refresh_supervisor_snapshot || true
    # A failed initial all-Issue reconstruction is not an empty snapshot.
    # Retry it on later polls; after it succeeds the already-fetched open
    # snapshot continuously re-enqueues active Issues without another REST
    # scan. Closed-Issue drift is therefore a startup/sync-issue boundary.
    if (( PROJECT_HINTS_REBUILT == 0 )); then
      rebuild_project_hints || true
    else
      rebuild_project_hints open || true
    fi
    reconcile_scope_conflict_cache || true
    requeue_answered || true
    requeue_dependency_ready || true
    if [[ -e $STATE_ROOT/stop.requested ]]; then
      enforce_worker_timeout || true
      reap_orphan_workers || true
      [[ $(worker_count) -eq 0 ]] && break
      sleep "$POLL_SECONDS"
      continue
    else
      recover_expired || true
      drain_paused_workers || true
      enforce_worker_timeout || true
      reap_orphan_workers || true
      prune_residual_worktrees || true
      reconcile_pending_project || true
      reconcile_queued_categories || true
      triage_stale_queued || true
      retry_failed || true
      if exhaustion_note_pause && budget_note_pause && core_budget_note_pause; then
        # Maintenance above may have moved needs-input/failed/blocked Issues
        # into queued. Refresh once before claiming so those transitions remain
        # same-poll observable while still replacing the former seven lists
        # with at most two consolidated snapshots.
        clear_supervisor_snapshot
        refresh_supervisor_snapshot || true
        reconcile_scope_conflict_cache || true
        while [[ $(worker_count) -lt $MAX_WORKERS ]]; do
          local issue worker claim
          claim=$(claim_next) || break
          IFS=$'\t' read -r issue worker <<< "$claim"
          project_add_issue "$issue" || true
          project_sync_state "$issue" running || true
          # setsid gives each worker its own process group so shutdown can
          # terminate the worker and its provider CLI child together.
          setsid "$0" _worker "$issue" "$worker" >> "$STATE_ROOT/logs/issue-$issue.log" 2>&1 &
          printf '%s\n' "$!" > "$STATE_ROOT/workers/$issue.pid"
        done
      fi
      if [[ ${AGENTIC_LOOP_RUN_ONCE:-0} == 1 ]]; then
        # RUN_ONCE is primarily a synchronous boundary: return as soon as the
        # workers from this poll finish. A one-second polling interval adds a
        # full second of avoidable latency to every short worker (and to every
        # E2E scenario using this boundary), so poll only while needed.
        while [[ $(worker_count) -gt 0 ]]; do sleep 0.1; done
        break
      fi
    fi
    # Reset to the base interval while work is in flight; otherwise back off.
    if [[ $(worker_count) -gt 0 ]]; then
      idle_streak=0
    elif (( idle_streak < 32 )); then
      idle_streak=$((idle_streak + 1))
    fi
    poll_interval=$(next_poll_interval "$idle_streak")
    printf '%s\n' "$poll_interval" > "$STATE_ROOT/poll-interval"
    sleep "$poll_interval"
  done
  rm -f "$STATE_ROOT/stop.requested" "$STATE_ROOT/poll-interval"
}
