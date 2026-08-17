# Module: worker_state.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034,SC2153



lease_body() {
  local worker=$1 now=$2 expires=$3
  printf '<!-- agentic-loop:claim worker=%s created=%s expires=%s -->\\n<!-- agentic-loop:lease worker=%s heartbeat=%s expires=%s -->\\nAgentic Loop worker `%s` のハートビートです（排他的claimを兼ねます）。リースは epoch %s に期限切れとなります。' "$worker" "$now" "$expires" "$worker" "$now" "$expires" "$worker" "$expires"
}


recent_lease_since() {
  local now=${1:-$(date +%s)}
  date -u -d "@$((now - LEASE_SECONDS - 60))" '+%Y-%m-%dT%H:%M:%SZ'
}


lease_file() { printf '%s\n' "$STATE_ROOT/workers/$1.lease"; }

worker_started_file() { printf '%s/workers/%s.started' "$STATE_ROOT" "$1"; }

worker_resume_file() { printf '%s/workers/%s.resume' "$STATE_ROOT" "$1"; }

worker_progress_file() { printf '%s/workers/%s.progress' "$STATE_ROOT" "$1"; }


# --- Local progress observability (see docs/decisions/0005) ---
# Everything here writes only to Git-common state (never GitHub or the work
# tree) and only persists the PROGRESS_STAGES enum, so the markers and the
# events stream cannot carry secrets or provider output.

progress_stage_valid() {
  local s
  for s in "${PROGRESS_STAGES[@]}"; do [[ $1 == "$s" ]] && return 0; done
  return 1
}


# Append one secret-safe event row to the shared append-only events.log:
# epoch<TAB>subject<TAB>code<TAB>stage-or-. subject is an Issue number or
# "supervisor"; code and stage are strict enums. Failure is never fatal.
event_append() {
  local subject=$1 code=$2 stage=${3:-}
  [[ $subject == supervisor || $subject =~ ^[0-9]+$ ]] || return 0
  case $code in progress | claim | recover | timeout | stop | start) ;; *) return 0 ;; esac
  [[ -z $stage || $stage == - ]] || progress_stage_valid "$stage" || return 0
  mkdir -p "$STATE_ROOT"
  printf '%s\t%s\t%s\t%s\n' "$(date +%s)" "$subject" "$code" "${stage:--}" >> "$EVENTS_LOG" 2>/dev/null || true
}


# Write the local progress marker for one Issue (epoch, stage, monotonic seq)
# and mirror the transition into events.log. seq grows on every touch so `tail`
# can detect a change even when a stage repeats. Never touches GitHub.
progress_write() {
  local issue=$1 stage=$2 file seq=0 now
  progress_stage_valid "$stage" || return 1
  file=$(worker_progress_file "$issue")
  if [[ -r $file ]]; then
    IFS=$'\t' read -r _ _ seq < "$file" 2>/dev/null || seq=0
    [[ $seq =~ ^[0-9]+$ ]] || seq=0
  fi
  seq=$((seq + 1))
  now=$(date +%s)
  mkdir -p "$(dirname "$file")"
  printf '%s\t%s\t%s\n' "$now" "$stage" "$seq" > "$file"
  event_append "$issue" progress "$stage"
}


# Best-effort convenience wrapper used by the worker at stage boundaries. The
# heartbeat loop deliberately does NOT touch progress: liveness and progress
# must stay independent so a stalled worker cannot keep itself healthy.
progress_touch() { progress_write "$1" "$2" || true; }


# Read the local progress marker into PROGRESS_EPOCH / PROGRESS_STAGE. Returns
# 1 (leaving both empty) when absent or corrupt.
progress_read() {
  local file
  PROGRESS_EPOCH='' PROGRESS_STAGE=''
  file=$(worker_progress_file "$1")
  [[ -r $file ]] || return 1
  IFS=$'\t' read -r PROGRESS_EPOCH PROGRESS_STAGE _ < "$file" 2>/dev/null || return 1
  [[ $PROGRESS_EPOCH =~ ^[0-9]+$ ]] || { PROGRESS_EPOCH=''; PROGRESS_STAGE=''; return 1; }
}


# Secondary progress signal: the worker log's mtime. The body is never read --
# `tail`/`status` only learn that the worker wrote recently, never what it
# wrote (secret boundary).
worker_log_mtime() {
  local mtime
  mtime=$(stat -c %Y "$STATE_ROOT/logs/issue-$1.log" 2>/dev/null) || return 1
  printf '%s\n' "$mtime"
}


# Seconds since this Issue's local worker began: prefers the .started marker
# (written right after claim), falling back to the pidfile's mtime when
# .started is missing or corrupt so a worker claimed moments ago is never
# misjudged as already over the timeout. Fails only when neither source is
# available (no local worker for this Issue).
worker_elapsed_seconds() {
  local issue=$1 started now pidfile mtime
  now=$(date +%s)
  if [[ -r $(worker_started_file "$issue") ]]; then
    read -r started < "$(worker_started_file "$issue")" || started=''
    if [[ $started =~ ^[0-9]+$ ]]; then
      printf '%s\n' "$((now - started))"
      return 0
    fi
  fi
  pidfile="$STATE_ROOT/workers/$issue.pid"
  mtime=$(stat -c %Y "$pidfile" 2>/dev/null) || return 1
  printf '%s\n' "$((now - mtime))"
}


# Remove a worker's local bookkeeping (pid + lease + phase + started + resume +
# progress caches) together. The handoff-comment id mapping is intentionally not
# cleared here: it must survive across worker lifecycles so the next worker
# keeps updating the same durable comment instead of creating a new one (see
# docs/decisions/0004).
clear_worker_local() {
  rm -f "$STATE_ROOT/workers/$1.pid" "$(lease_file "$1")" "$(worker_phase_file "$1")" "$(worker_started_file "$1")" "$(worker_resume_file "$1")" "$(worker_progress_file "$1")"
}

worker_phase_file() { printf '%s/workers/%s.phase' "$STATE_ROOT" "$1"; }


# Create the single lease comment for this Issue and remember its id, expiry
# and heartbeat time so later heartbeats update it in place and `status` can
# read the lease locally without an extra GitHub call. See docs/decisions/0003:
# one comment per Issue updated via PATCH keeps the Issue clean and is gentle
# on the secondary rate limit (bursty comment creation is what trips abuse
# detection).
lease_start() {
  local issue=$1 worker=$2 now expires id
  now=$(date +%s); expires=$((now + LEASE_SECONDS))
  id=$(repo_api "issues/$issue/comments" --method POST -f body="$(lease_body "$worker" "$now" "$expires")" --jq '.id' 2>/dev/null | tr -d '[:space:]' || true)
  if [[ $id =~ ^[0-9]+$ ]]; then
    mkdir -p "$STATE_ROOT/workers"
    printf '%s\t%s\t%s\n' "$id" "$expires" "$now" > "$(lease_file "$issue")"
  fi
}


# Acquire a repository-wide Issue claim before changing its Label. GitHub
# Label replacement is not compare-and-swap: two Supervisors can read the same
# queued snapshot and both otherwise conclude that they won. Claim comments
# have a repository-global, monotonically increasing id, so contenders publish
# a short lease and deterministically elect the lowest still-valid comment id.
# A loser never changes Labels or starts a worker. The winning comment is then
# reused as the ordinary heartbeat lease, avoiding an extra durable object.
claim_acquire() {
  local issue=$1 worker=$2 now expires id row cid body winner='' current state labels candidate_expires since
  now=$(date +%s); expires=$((now + LEASE_SECONDS))
  since=$(recent_lease_since "$now")
  current=$(repo_api "issues/$issue" --jq '[.state, ([.labels[].name] | join(","))] | @tsv' 2>/dev/null) || return 1
  IFS=$'\t' read -r state labels <<< "$current"
  [[ $state == open && ,$labels, == *,agent:queued,* ]] || return 1
  id=$(repo_api "issues/$issue/comments" --method POST -f body="$(lease_body "$worker" "$now" "$expires")" --jq '.id' 2>/dev/null | tr -d '[:space:]' || true)
  [[ $id =~ ^[0-9]+$ ]] || return 1
  # The total ordering by comment id decides ownership; no wall-clock ordering
  # or timing window is used. A contender posting after this read will observe
  # this lower id during its own read and lose the election.
  while IFS= read -r row; do
    [[ -n $row ]] || continue
    cid=$(base64 -d <<< "$row" 2>/dev/null | cut -f1)
    body=$(base64 -d <<< "$row" 2>/dev/null | cut -f2-)
    [[ $cid =~ ^[0-9]+$ ]] || continue
    [[ $body == *"agentic-loop:claim"* ]] || continue
    candidate_expires=$(sed -n 's/.*agentic-loop:claim[^>]*expires=\([0-9][0-9]*\).*/\1/p' <<< "$body" | head -n 1)
    [[ $candidate_expires =~ ^[0-9]+$ && $candidate_expires -ge $now ]] || continue
    [[ -z $winner || $cid -lt $winner ]] && winner=$cid
  done < <(repo_api "issues/$issue/comments" --method GET -f since="$since" -f per_page=100 --paginate --jq '.[] | select(.body | contains("agentic-loop:claim")) | [.id, .body] | @tsv | @base64' 2>/dev/null || true)
  if [[ $winner != "$id" ]]; then
    repo_api "issues/comments/$id" --method PATCH -f body="<!-- agentic-loop:claim worker=$worker created=$now expires=0 -->\\nclaim競合に敗れたため解放しました。" >/dev/null 2>&1 || true
    return 1
  fi
  mkdir -p "$STATE_ROOT/workers"
  printf '%s\t%s\t%s\n' "$id" "$expires" "$now" > "$(lease_file "$issue")"
  event_append "$issue" claim -
}


# End this host's durable ownership before deliberately re-queueing an Issue.
# The comment remains as audit history but cannot delay takeover by another
# host until the original lease timeout.
lease_release() {
  local issue=$1 worker=${2:-released} file id now
  file=$(lease_file "$issue")
  [[ -r $file ]] || return 0
  IFS=$'\t' read -r id _ < "$file" || return 0
  [[ $id =~ ^[0-9]+$ ]] || return 0
  now=$(date +%s)
  repo_api "issues/comments/$id" --method PATCH -f body="<!-- agentic-loop:claim worker=$worker created=$now expires=0 -->\\n<!-- agentic-loop:lease worker=$worker heartbeat=$now expires=0 -->\\nAgentic Loop worker \`$worker\` のハートビートです。処理終了に伴い、このホストはclaimを解放しました。" >/dev/null 2>&1 || true
}


# Refresh the lease by editing the existing comment (a single PATCH). Fall back
# to creating a fresh comment if we have no id yet, or the stored comment is gone.
lease_heartbeat() {
  local issue=$1 worker=$2 now expires id file rest
  file="$(lease_file "$issue")"
  now=$(date +%s); expires=$((now + LEASE_SECONDS))
  if [[ -r $file ]]; then
    IFS=$'\t' read -r id rest < "$file" || id=''
    if [[ $id =~ ^[0-9]+$ ]] && repo_api "issues/comments/$id" --method PATCH -f body="$(lease_body "$worker" "$now" "$expires")" >/dev/null 2>&1; then
      printf '%s\t%s\t%s\n' "$id" "$expires" "$now" > "$file"
      return 0
    fi
  fi
  lease_start "$issue" "$worker"
}


# Local-only lease read for `status`: id, expiry epoch and last-heartbeat
# epoch. Tolerates the pre-Issue-#42 single-line (id only) format, in which
# case expires/heartbeat are left empty rather than failing.
lease_read() {
  local file rest
  file="$(lease_file "$1")"
  [[ -r $file ]] || return 1
  IFS=$'\t' read -r LEASE_ID LEASE_EXPIRES LEASE_HEARTBEAT rest < "$file"
}


# Whether the local worker recorded for this Issue is still running: its pid is
# alive and its command line is this repository's worker for this Issue (a cmdline
# check defends against pid reuse). Used to adopt in-flight work and to recover
# fast when a local worker has died.
worker_alive() {
  local issue=$1 pidfile pid command_line
  pidfile="$STATE_ROOT/workers/$issue.pid"
  [[ -r $pidfile ]] || return 1
  read -r pid < "$pidfile" || return 1
  [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null || return 1
  [[ -r /proc/$pid/cmdline ]] || return 1
  command_line=$(tr '\0' ' ' < "/proc/$pid/cmdline") || return 1
  [[ $command_line == *"$SCRIPT_ROOT/bin/agentic-loop"* && $command_line == *" _worker $issue "* ]]
}


# Lighter local-worker liveness for scope-cache upkeep: the pidfile exists and
# its pid is alive. Unlike worker_alive it does not require the child to have
# exec'd into "_worker" yet, so a just-spawned worker's scope entry is honored
# immediately (no claim/exec race); a phantom entry (no pidfile) still fails.
worker_pid_live() {
  local pidfile="$STATE_ROOT/workers/$1.pid" pid
  [[ -r $pidfile ]] || return 1
  read -r pid < "$pidfile" || return 1
  [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null
}


# Return agent:running Issues whose worker is gone to the queue (see ADR 0003).
# Local-first: a live local worker is adopted; a dead local worker's Issue is
# recovered immediately without any GitHub call. When no local worker exists the
# Issue may belong to another machine, so the GitHub lease is honored and only an
# expired (or missing) lease is recovered. Reading the lease only for Issues
# without a local worker also cuts API usage.
recover_expired() {
  local issue body expires now pidfile count since
  now=$(date +%s)
  since=$(recent_lease_since "$now")
  while IFS= read -r issue; do
    [[ -n $issue ]] || continue
    worker_alive "$issue" && continue
    pidfile="$STATE_ROOT/workers/$issue.pid"
    if [[ -e $pidfile ]]; then
      clear_worker_local "$issue"
    else
      body=$(repo_api "issues/$issue/comments" --method GET -f since="$since" -f per_page=100 --paginate --jq '[.[].body | select(contains("agentic-loop:lease"))] | last // ""' 2>/dev/null | tail -n 1 || true)
      expires=$(printf '%s\n' "$body" | sed -n 's/.*expires=\([0-9][0-9]*\).*/\1/p' | head -n 1)
      [[ -n $expires && $expires -ge $now ]] && continue
    fi
    scope_cache_clear "$issue"
    clear_conflict_wait "$issue"
    # A worker that keeps dying before finishing (lease expiry / crash, never an
    # explicit AGENTIC_LOOP_RESULT=failed) would otherwise be requeued forever:
    # claim_next records an attempt per claim, but only retry_failed's
    # MAX_ATTEMPTS bound consults it, and that path only sees agent:failed. Once
    # the recorded attempts reach the limit, escalate to agent:failed so
    # retry_failed closes it as unresolvable instead of spinning the queue.
    count=$(attempt_count "$issue")
    if (( count >= MAX_ATTEMPTS )); then
      set_issue_state "$issue" failed
      comment_issue "$issue" "<!-- agentic-loop:recover-exhausted attempts=$count -->\n担当workerが完了前に繰り返し停止したため（試行 $count/$MAX_ATTEMPTS）、キューへ戻さず \`agent:failed\` へ移します。以降は解決不能として自動でcloseされます。再開が必要なら内容を確認のうえ \`agent:queued\` を付け直してください。"
    else
      set_issue_state "$issue" queued
      comment_issue "$issue" '<!-- agentic-loop:recovered -->\n担当workerの終了、またはリース期限切れを検出したため、Issueを安全にキューへ戻しました。'
    fi
    event_append "$issue" recover -
  done < <(snapshot_state_rows running | cut -f1 || repo_api issues --method GET -f state=open -f labels="$(state_label running)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | .number' 2>/dev/null || true)
}


# A lease heartbeat only proves the worker process is alive, not that it is
# making progress (see docs/decisions/0006): a worker hung on an unresponsive
# API call or an infinite retry keeps heartbeating forever and recover_expired
# never reclaims it, permanently occupying a slot. Kill this host's own
# workers (a live pidfile) that have run longer than WORKER_TIMEOUT_SECONDS,
# process-group-wide so no provider CLI child is orphaned, then fail the Issue
# so the bounded retry_failed cooldown/requeue takes over. Only pidfile-owning
# workers are touched, so a multi-host deployment never interferes with
# another host's Issue. Every step is best-effort: a transient GitHub failure
# here must not crash the supervisor (see docs/decisions/0003); the next poll
# retries.
enforce_worker_timeout() {
  (( WORKER_TIMEOUT_SECONDS > 0 )) || return 0
  local pidfile issue pid elapsed waited
  shopt -s nullglob
  for pidfile in "$STATE_ROOT"/workers/*.pid; do
    issue=$(basename "$pidfile" .pid)
    [[ $issue =~ ^[0-9]+$ ]] || continue
    worker_pid_live "$issue" || continue
    elapsed=$(worker_elapsed_seconds "$issue") || continue
    (( elapsed >= WORKER_TIMEOUT_SECONDS )) || continue
    read -r pid < "$pidfile" || continue
    [[ $pid =~ ^[0-9]+$ ]] || continue
    kill -TERM "-$pid" 2>/dev/null || true
    waited=0
    while (( waited < 5 )) && kill -0 "$pid" 2>/dev/null; do sleep 1; waited=$((waited + 1)); done
    kill -0 "$pid" 2>/dev/null && kill -KILL "-$pid" 2>/dev/null || true
    lease_release "$issue" timeout
    clear_worker_local "$issue"
    scope_cache_clear "$issue"
    clear_conflict_wait "$issue"
    comment_issue "$issue" "<!-- agentic-loop:worker-timeout issue=$issue elapsed=${elapsed}s limit=${WORKER_TIMEOUT_SECONDS}s -->\nこのIssueのworkerはlease heartbeatが有効なまま実行時間上限（${WORKER_TIMEOUT_SECONDS}秒）を超えたため、ハングしたと判断してプロセスグループごと停止しました（経過 ${elapsed}秒）。自動的な再試行キューへ戻します。誤検知が疑われる場合は \`.agentic-loop.toml\` の \`queue.worker_timeout_seconds\` を見直してください。" || true
    set_issue_state "$issue" failed || true
    project_sync_state "$issue" failed || true
    event_append "$issue" timeout -
  done
  shopt -u nullglob
}


requeue_answered() {
  local issue answered
  while IFS= read -r issue; do
    [[ -n $issue ]] || continue
    # shellcheck disable=SC2016 # This is a jq program, not a shell expression.
    answered=$(repo_api "issues/$issue/comments" --method GET -f per_page=100 --jq '. as $comments | ([range(0; $comments | length) | select($comments[.].body | contains("agentic-loop:needs-input"))] | last) as $marker | if $marker == null then false else any($comments[$marker + 1:][]; (.body | contains("<!-- agentic-loop:") | not)) end' 2>/dev/null || true)
    if [[ $answered == true ]]; then
      set_issue_state "$issue" queued
      comment_issue "$issue" '<!-- agentic-loop:answer-detected -->\n返信を検出したため、このIssueをキューへ戻しました。'
    fi
  done < <(snapshot_state_rows needs-input | cut -f1 || repo_api issues --method GET -f state=open -f labels="$(state_label needs-input)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | .number' 2>/dev/null || true)
}


triage_stale_queued() {
  (( STALE_DAYS > 0 )) || return 0
  local issue updated_at updated_epoch cutoff
  cutoff=$(($(date +%s) - STALE_DAYS * 86400))
  while IFS=$'\t' read -r issue updated_at; do
    [[ -n $issue && -n $updated_at ]] || continue
    updated_epoch=$(date -d "$updated_at" +%s 2>/dev/null) || continue
    (( updated_epoch <= cutoff )) || continue
    set_issue_state "$issue" stale
    project_sync_state "$issue" stale
    comment_issue "$issue" "<!-- agentic-loop:stale days=$STALE_DAYS updated=$updated_at -->\nこのIssueは \`agent:queued\` のまま$STALE_DAYS日間更新されなかったため、claim前のトリアージで \`agent:stale\` へ移し、クローズしました。再開するにはIssueをreopenし、内容を確認・更新してから \`agent:queued\` Labelを付けてください。"
    repo_api "issues/$issue" --method PATCH -f state=closed >/dev/null
  done < <(snapshot_state_rows queued | awk -F '\t' '{print $1 "\t" $3}' || repo_api issues --method GET -f state=open -f labels="$(state_label queued)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | [.number, .updated_at] | @tsv' 2>/dev/null || true)
}


# Per-Issue attempt tracking (count + last-attempt timestamp) under Git-external
# state, so a transient worker failure can be retried a bounded number of times.
attempts_file() { printf '%s/attempts/issue-%s' "$STATE_ROOT" "$1"; }

attempt_count() {
  local file count; file=$(attempts_file "$1")
  [[ -r $file ]] || { printf '0'; return; }
  IFS=$'\t' read -r count _ < "$file"
  [[ $count =~ ^[0-9]+$ ]] && printf '%s' "$count" || printf '0'
}

record_attempt() {
  local issue=$1 count file
  count=$(attempt_count "$issue"); file=$(attempts_file "$issue")
  mkdir -p "$(dirname "$file")"
  printf '%s\t%s\n' "$((count + 1))" "$(date +%s)" > "$file"
}

clear_attempts() { rm -f "$(attempts_file "$1")"; }

attempt_cooldown_elapsed() {
  local file ts now; file=$(attempts_file "$1")
  [[ -r $file ]] || return 0
  IFS=$'\t' read -r _ ts < "$file"
  [[ $ts =~ ^[0-9]+$ ]] || return 0
  now=$(date +%s); (( now - ts >= RETRY_COOLDOWN_SECONDS ))
}

# Re-queue failures this loop already attempted, so a transient failure (e.g. an
# exhausted token budget or a killed session) is retried instead of parked.
# Only Issues tracked by claim are managed; those that reach MAX_ATTEMPTS stay
# agent:failed for human review, and untracked/external failures are left alone.
# Automatically manage every failed Issue: retry it (bounded by MAX_ATTEMPTS with
# a cooldown) so a transient failure recovers without human action, and once the
# attempts are exhausted, close it as genuinely unresolvable instead of parking
# it in agent:failed forever. Pre-existing/untracked failures (attempt count 0)
# are retried too.
retry_failed() {
  local issue count
  while IFS= read -r issue; do
    [[ $issue =~ ^[0-9]+$ ]] || continue
    count=$(attempt_count "$issue")
    if (( count >= MAX_ATTEMPTS )); then
      comment_issue "$issue" "<!-- agentic-loop:unresolved attempts=$count -->\n$count 回試行しても解決できなかったため、解決不能とみなしてIssueをcloseします。再開が必要なら \`agent:queued\` を付け直してください。" || true
      repo_api "issues/$issue" --method PATCH -f state=closed >/dev/null 2>&1 || true
      clear_attempts "$issue"
      continue
    fi
    attempt_cooldown_elapsed "$issue" || continue
    set_issue_state "$issue" queued || continue
    comment_issue "$issue" "<!-- agentic-loop:retry attempt=$count -->\n一時的な失敗の可能性があるため自動的に再試行キューへ戻します（試行 $count/$MAX_ATTEMPTS）。上限に達したら解決不能とみなしてcloseします。" || true
  done < <(snapshot_state_rows failed | cut -f1 || repo_api issues --method GET -f state=open -f labels="$(state_label failed)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | .number' 2>/dev/null)
}
