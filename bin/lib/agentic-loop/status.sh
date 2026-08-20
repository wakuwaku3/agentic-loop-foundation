# Module: status.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



running_issues() {
  repo_api issues --method GET -f state=open -f labels="$(state_label running)" -f per_page=100 --paginate \
    --jq '.[] | select(.pull_request == null) | "#\(.number) \(.title)"' 2>/dev/null || true
}


status_conflict_waits() {
  local file issue other reason
  shopt -s nullglob
  for file in "$STATE_ROOT"/conflict/issue-*; do
    issue=${file##*/issue-}
    IFS=$'\t' read -r other reason < "$file" || continue
    status_array_contains "$issue" QUEUE_NUM || continue
    status_array_contains "$other" RUN_NUM || continue
    printf '#%s 競合相手 #%s 理由 scope重複: %s\n' "$issue" "$other" "$reason"
  done
  shopt -u nullglob
}


status_array_contains() {
  local wanted=$1 array_name=$2 item
  local -n values=$array_name
  for item in "${values[@]}"; do [[ $item == "$wanted" ]] && return 0; done
  return 1
}


status_dependency_waits() {
  local file issue reason detail
  shopt -s nullglob
  for file in "$(dependency_state_dir)"/blocked-*; do
    issue=${file##*/blocked-}
    IFS=$'\t' read -r reason detail < "$file" || continue
    printf '#%s 理由 %s: %s\n' "$issue" "$reason" "$detail"
  done
  shopt -u nullglob
}



# --- status observability (see docs/decisions/0005) ---

# One TSV row per open Issue, classified purely by its own agent:* Label into
# running/queued/needs-input/failed/in-review/blocked/other, plus the
# queue_rank_jq ranks (numeric priority, then category) and created_at (used
# only to order queued rows the same way claim_next does). A single REST(core)
# call backs every section below except the closed agent:stale summary (see
# status_stale_fetch).
status_snapshot_fetch() {
  # workload-unbounded: on-demand human-facing read, not a per-poll path (see refresh_supervisor_snapshot for the poll-side aggregate); bound=open Issue count
  repo_api issues --method GET -f state=open -f per_page=100 --paginate --jq '
    .[] | select(.pull_request == null) |
    [.number, .title,
     (if any(.labels[]; .name == "agent:running") then "running"
      elif any(.labels[]; .name == "agent:queued") then "queued"
      elif any(.labels[]; .name == "agent:needs-input") then "needs-input"
      elif any(.labels[]; .name == "agent:failed") then "failed"
      elif any(.labels[]; .name == "agent:parked") then "parked"
      elif any(.labels[]; .name == "agent:in-review") then "in-review"
      elif any(.labels[]; .name == "agent:blocked") then "blocked"
      elif any(.labels[]; .name == "agent:paused") then "paused"
      else "other" end),
     '"$(queue_rank_jq)"', .created_at] | @tsv'
}


# Up to 100 closed agent:stale Issues (no --paginate: a bounded, best-effort
# summary rather than an exhaustive audit -- see docs/operations/issue-queue.md).
status_stale_fetch() {
  repo_api issues --method GET -f state=all -f labels="$(state_label stale)" -f per_page=100 --jq \
    '.[] | select(.pull_request == null) | [.number, .title] | @tsv'
}


# Human label for a pool's recovery basis (agent_pool_basis_get): what grounds
# the resume time shown next to it, so an operator can tell "the provider told
# us exactly when" from "no signal is available and this is a backing-off
# guess" (Issue #158 completion criterion: show which provider/resource is
# paused and the basis for its expected recovery).
status_pool_basis_label() {
  case $1 in
    reset) printf 'provider提示のreset時刻' ;;
    probe) printf '使用率再probeの実測' ;;
    *) printf '実測不能のため指数backoff' ;;
  esac
}

# Epoch of the pool's most recent usage measurement (max across its
# providers' pools/usage-<provider> cache files, from opencode_go_usage_cached
# / claude_probe_usage_cached), or empty when none is available (e.g. codex,
# whose session-log read has no persisted cache file, or an environment where
# no provider's usage is ever readable). Never touches the network itself --
# a pure read of the same cache files the pick/exhaustion path already
# maintains, so status stays a read-only, network-free command.
status_pool_last_probed() {
  local pool=$1 provider file fetched latest=''
  for provider in $(agent_pool_providers "$pool"); do
    file="$STATE_ROOT/pools/usage-$provider"
    [[ -r $file ]] || continue
    IFS=$'\t' read -r fetched _ < "$file" 2>/dev/null || continue
    [[ $fetched =~ ^[0-9]+$ ]] || continue
    if [[ -z $latest ]] || (( fetched > latest )); then latest=$fetched; fi
  done
  # Always exit 0: callers capture this via a plain `x=$(status_pool_last_probed
  # ...)` assignment, and under -e a non-zero exit there would abort the whole
  # status command just because no measurement was found (the normal case for
  # codex, or any pool before its first probe/telemetry read).
  [[ -n $latest ]] && printf '%s' "$latest"
  return 0
}

# One pool's pause line with its resume ETA and basis, or a bare exhaustion
# notice when the marker exists but its epoch cannot be parsed. Appends the
# most recent usage measurement time when one is available (Issue #251
# completion criterion 6: let an operator tell a real probe/telemetry reading
# apart from a blind backoff guess).
status_pool_pause_detail() {
  local pool=$1 resume='' basis when probed probed_when
  read -r resume < "$(agent_pool_marker "$pool")" 2>/dev/null || true
  if [[ $resume =~ ^[0-9]+$ ]]; then
    basis=$(agent_pool_basis_get "$pool")
    when=$(date -d "@$resume" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || printf '%s' "$resume")
    probed=$(status_pool_last_probed "$pool")
    if [[ -n $probed ]]; then
      probed_when=$(date -d "@$probed" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || printf '%s' "$probed")
      printf 'pool=%s 枯渇（回復予定=%s, 根拠=%s, 直近実測=%s）' "$pool" "$when" "$(status_pool_basis_label "$basis")" "$probed_when"
    else
      printf 'pool=%s 枯渇（回復予定=%s, 根拠=%s）' "$pool" "$when" "$(status_pool_basis_label "$basis")"
    fi
  else
    printf 'pool=%s 枯渇（回復待ち）' "$pool"
  fi
}

# Human-readable (Japanese) reason claiming is currently paused, or empty if
# it is not. Purely local: reads the same marker files the supervisor's own
# poll loop checks (see docs/decisions/0003), so `status` never queries
# rate_limit itself. Sets STATUS_PAUSE_TEXT and (as a side effect consumed by
# status_oldest_marker_elapsed) STATUS_PAUSE_MARKERS -- called directly, never
# via `$(...)`, since a command substitution subshell would discard both.
status_claim_pause_reasons() {
  local -a reasons=()
  STATUS_PAUSE_TEXT='' STATUS_PAUSE_MARKERS=()
  if [[ -e $STATE_ROOT/budget-paused ]]; then reasons+=('provider週次利用率のreserve'); STATUS_PAUSE_MARKERS+=("$STATE_ROOT/budget-paused"); fi
  if [[ -e $STATE_ROOT/core-budget-paused ]]; then reasons+=('REST(core)のreserve'); STATUS_PAUSE_MARKERS+=("$STATE_ROOT/core-budget-paused"); fi
  if [[ -r $STATE_ROOT/agent-exhausted ]]; then
    local resume=''
    read -r resume < "$STATE_ROOT/agent-exhausted" || true
    if [[ $resume =~ ^[0-9]+$ ]] && (( $(date +%s) < resume )); then
      reasons+=('全プール利用不可')
      STATUS_PAUSE_MARKERS+=("$STATE_ROOT/agent-exhausted")
    fi
  fi
  if [[ -e $STATE_ROOT/all-pools-paused ]]; then reasons+=('全プール利用不可'); STATUS_PAUSE_MARKERS+=("$STATE_ROOT/all-pools-paused"); fi
  local pool
  for pool in $(agent_all_pools); do
    if agent_pool_marker_active "$pool"; then
      reasons+=("$(status_pool_pause_detail "$pool")")
      STATUS_PAUSE_MARKERS+=("$(agent_pool_marker "$pool")")
    fi
  done
  if [[ -e $STATE_ROOT/stop.requested ]]; then reasons+=('stop要求によるdrain中'); STATUS_PAUSE_MARKERS+=("$STATE_ROOT/stop.requested"); fi
  (( ${#reasons[@]} == 0 )) && return 0
  local IFS=,
  STATUS_PAUSE_TEXT="${reasons[*]}"
}

# Oldest mtime among the pause markers status_claim_pause_reasons actually
# matched (set as its side effect in STATUS_PAUSE_MARKERS), so elapsed
# reflects whichever reason is truly in effect instead of assuming
# budget-paused specifically (see docs/decisions/0005).
status_oldest_marker_elapsed() {
  local marker mtime oldest=''
  for marker in "${STATUS_PAUSE_MARKERS[@]}"; do
    mtime=$(stat -c %Y "$marker" 2>/dev/null) || continue
    if [[ -z $oldest ]] || (( mtime < oldest )); then oldest=$mtime; fi
  done
  status_elapsed_since "$oldest"
}


# Read-only counterpart of clear_stale_lock's staleness test: whether
# supervisor.lock is held but no supervisor is actually alive to hold it.
# Never mutates state -- status must stay a pure read.
status_supervisor_lock_stale() {
  [[ -d $STATE_ROOT/supervisor.lock ]] || return 1
  pid_alive && return 1
  if [[ ! -e $STATE_ROOT/supervisor.pid ]]; then
    local now mtime
    now=$(date +%s)
    mtime=$(stat -c %Y "$STATE_ROOT/supervisor.lock" 2>/dev/null || printf '%s' "$now")
    (( now - mtime < SUPERVISOR_LOCK_GRACE_SECONDS )) && return 1
  fi
  return 0
}


# Classify a running Issue's progress into healthy/stalled/timeout/unknown from
# purely local state (see docs/decisions/0005). timeout wins over everything
# once STATUS_RUN_TIMEOUT_EXCEEDED is set (mirroring the existing worker-timeout
# anomaly); a worker without any local progress signal stays unknown. The worker
# log's mtime is used as a secondary signal (its body is never read), so a
# provider thinking inside a stage boundary does not flip to stalled. Sets
# STATUS_RUN_STAGE / STATUS_RUN_PROGRESS_AGE / STATUS_RUN_PROGRESS_AT /
# STATUS_RUN_HEALTH.
status_progress_health() {
  local issue=$1 timeout_exceeded=${2:-0} now age log_mtime
  now=$(date +%s)
  STATUS_RUN_STAGE='' STATUS_RUN_PROGRESS_AGE='' STATUS_RUN_PROGRESS_AT='' STATUS_RUN_HEALTH=unknown
  if (( timeout_exceeded )); then
    STATUS_RUN_HEALTH=timeout
    return 0
  fi
  age=''
  if progress_read "$issue"; then
    STATUS_RUN_STAGE=$PROGRESS_STAGE
    age=$((now - PROGRESS_EPOCH))
  fi
  if log_mtime=$(worker_log_mtime "$issue"); then
    if [[ -z $age ]] || (( now - log_mtime < age )); then age=$((now - log_mtime)); fi
  fi
  [[ -n $age ]] || return 0
  STATUS_RUN_PROGRESS_AGE=$age
  STATUS_RUN_PROGRESS_AT=$((now - age))
  if (( age < STATUS_STALL_SECONDS )); then STATUS_RUN_HEALTH=healthy; else STATUS_RUN_HEALTH=stalled; fi
  return 0
}


# ANSI color for a health band, only on a TTY and never with NO_COLOR set;
# piped text, JSON and CI output stay colorless.
status_health_color() {
  [[ -t 1 && -z ${NO_COLOR:-} ]] || { printf '%s' "$1"; return 0; }
  case $1 in
    healthy) printf '\033[32m%s\033[0m' "$1" ;;
    stalled) printf '\033[33m%s\033[0m' "$1" ;;
    timeout) printf '\033[31m%s\033[0m' "$1" ;;
    *) printf '%s' "$1" ;;
  esac
}


# Whether the GitHub snapshot cache (in-memory raw TSV + fetched_at) is still
# within STATUS_GITHUB_TTL_SECONDS. `status --watch` relies on this so ticks
# between refreshes cost zero REST(core) reads (see docs/decisions/0005).
status_cache_fresh() {
  local fetched=$1
  [[ $fetched =~ ^[0-9]+$ ]] || return 1
  (( $(date +%s) - fetched < STATUS_GITHUB_TTL_SECONDS ))
}


# Populate the STATUS_RUN_* globals for one running Issue purely from local
# state (phase cache, scope cache, .started, .lease, .resume): no additional
# REST call. Missing local state (e.g. another host owns the Issue) leaves
# the fields empty rather than failing.
status_running_detail() {
  local issue=$1 now started
  now=$(date +%s)
  STATUS_RUN_PHASE=$(cat "$(worker_phase_file "$issue")" 2>/dev/null || true)
  STATUS_RUN_SCOPE=$(scope_cache_read "$issue")
  STATUS_RUN_STARTED=''
  if [[ -r $(worker_started_file "$issue") ]]; then
    read -r started < "$(worker_started_file "$issue")" || started=''
    [[ $started =~ ^[0-9]+$ ]] && STATUS_RUN_STARTED=$started
  fi
  STATUS_RUN_ELAPSED=''
  [[ -n $STATUS_RUN_STARTED ]] && STATUS_RUN_ELAPSED=$((now - STATUS_RUN_STARTED))
  STATUS_RUN_TIMEOUT_AT='' STATUS_RUN_TIMEOUT_EXCEEDED=0
  if (( WORKER_TIMEOUT_SECONDS > 0 )) && [[ -n $STATUS_RUN_STARTED ]]; then
    STATUS_RUN_TIMEOUT_AT=$((STATUS_RUN_STARTED + WORKER_TIMEOUT_SECONDS))
    (( now >= STATUS_RUN_TIMEOUT_AT )) && STATUS_RUN_TIMEOUT_EXCEEDED=1
  fi
  STATUS_RUN_STAGE='' STATUS_RUN_PROGRESS_AGE='' STATUS_RUN_PROGRESS_AT='' STATUS_RUN_HEALTH=unknown
  status_progress_health "$issue" "$STATUS_RUN_TIMEOUT_EXCEEDED"
  LEASE_ID='' LEASE_EXPIRES='' LEASE_HEARTBEAT=''
  lease_read "$issue" || true
  STATUS_RUN_LEASE_EXPIRES=$LEASE_EXPIRES
  STATUS_RUN_HEARTBEAT=$LEASE_HEARTBEAT
  STATUS_RUN_LEASE_EXPIRED=0
  [[ $LEASE_EXPIRES =~ ^[0-9]+$ ]] && (( now > LEASE_EXPIRES )) && STATUS_RUN_LEASE_EXPIRED=1
  STATUS_RUN_WORKTREE="$WORKTREE_ROOT/issue-$issue"
  STATUS_RUN_WORKTREE_EXISTS=0
  [[ -e $STATUS_RUN_WORKTREE ]] && STATUS_RUN_WORKTREE_EXISTS=1
  RESUME_LOCAL_PHASE='' RESUME_LOCAL_BRANCH='' RESUME_LOCAL_PR='' RESUME_LOCAL_PR_URL='' RESUME_LOCAL_PR_STATE='' RESUME_LOCAL_CHECKS='' RESUME_LOCAL_DIRTY='' RESUME_LOCAL_DIVERGED=''
  worker_resume_read "$issue" || true
  STATUS_RUN_BRANCH=${RESUME_LOCAL_BRANCH:-agent/issue-$issue}
  STATUS_RUN_PR=$RESUME_LOCAL_PR
  STATUS_RUN_PR_URL=$RESUME_LOCAL_PR_URL
  STATUS_RUN_PR_STATE=$RESUME_LOCAL_PR_STATE
  STATUS_RUN_CHECKS=$RESUME_LOCAL_CHECKS
  STATUS_RUN_DIRTY=${RESUME_LOCAL_DIRTY:-0}
  STATUS_RUN_DIVERGED=${RESUME_LOCAL_DIVERGED:-0}
  STATUS_RUN_LOCAL=0
  { [[ -n $STATUS_RUN_STARTED ]] || [[ -n $RESUME_LOCAL_PHASE ]] || [[ -r $STATE_ROOT/workers/$issue.pid ]]; } && STATUS_RUN_LOCAL=1
  return 0
}


# Why an agent:queued Issue is (or is not) claimable right now, derived purely
# from local state -- the same files claim_next itself consults -- rather than
# re-running GitHub dependency/scope checks (a best-effort preview, not a
# claim decision; see docs/operations/issue-queue.md). Sets
# STATUS_CANDIDATE_REASON/STATUS_CANDIDATE_DETAIL.
status_queue_candidate_reason() {
  local issue=$1 file
  file=$(conflict_wait_file "$issue")
  if [[ -r $file ]]; then
    local other reason
    IFS=$'\t' read -r other reason < "$file" || true
    if status_array_contains "$other" RUN_NUM; then
      STATUS_CANDIDATE_REASON='scope-conflict'
      STATUS_CANDIDATE_DETAIL="#$other と重複: $reason"
      return 0
    fi
  fi
  if [[ -r $(attempts_file "$issue") ]] && ! attempt_cooldown_elapsed "$issue"; then
    STATUS_CANDIDATE_REASON='retry-cooldown'
    STATUS_CANDIDATE_DETAIL='直前の失敗からの再試行待機時間内です。'
    return 0
  fi
  status_claim_pause_reasons
  if [[ -n $STATUS_PAUSE_TEXT ]]; then
    STATUS_CANDIDATE_REASON='claim-paused'
    STATUS_CANDIDATE_DETAIL="$STATUS_PAUSE_TEXT"
    return 0
  fi
  STATUS_CANDIDATE_REASON=claimable
  STATUS_CANDIDATE_DETAIL=''
}


# An anomaly always says whether the supervisor is recovering it or an
# operator must act.  Keep these as parallel arrays to preserve the small,
# dependency-free JSON renderer below.
anomaly_add() {
  local elapsed=$6
  [[ $elapsed =~ ^[0-9]+$ ]] || elapsed=0
  ANOMALY_LEVEL+=("$1"); ANOMALY_CODE+=("$2"); ANOMALY_SUBJECT+=("$3"); ANOMALY_DETAIL+=("$4")
  ANOMALY_ACTION+=("$5"); ANOMALY_ELAPSED+=("$elapsed"); ANOMALY_CLASSIFICATION+=("$7")
}

# Return a non-negative elapsed duration from a local-state epoch.  Invalid or
# unavailable timestamps are represented as zero so every displayed anomaly
# retains the required elapsed field without adding a GitHub read.
status_elapsed_since() {
  local since=$1 now
  now=$(date +%s)
  [[ $since =~ ^[0-9]+$ ]] || { printf '0'; return 0; }
  (( since > now )) && since=$now
  printf '%s' "$((now - since))"
}

status_file_elapsed() {
  local file=$1 since
  since=$(stat -c %Y "$file" 2>/dev/null || true)
  status_elapsed_since "$since"
}

# Like status_file_elapsed, but the epoch comes from the marker's own first
# line (as written by `date +%s > file`) rather than the file's mtime. Use
# this for markers such as project-pending.since whose whole purpose is to
# remember a timestamp that must survive later touches (renames, appends) of
# other files -- an mtime would just track "last written", not "since when".
status_marker_content_elapsed() {
  local file=$1 since=''
  read -r since < "$file" 2>/dev/null || true
  status_elapsed_since "$since"
}

# Render a seconds count as a human-readable Japanese duration for text
# output (e.g. `12分30秒`, `3時間5分`, `2日4時間`) so a long-stuck anomaly is
# legible without reading digit-by-digit. `--format json`'s elapsed stays a
# raw integer second count; only the text renderer calls this.
status_format_duration() {
  local seconds=$1
  [[ $seconds =~ ^[0-9]+$ ]] || seconds=0
  if (( seconds < 60 )); then
    printf '%s秒' "$seconds"
  elif (( seconds < 3600 )); then
    printf '%s分%s秒' "$((seconds / 60))" "$((seconds % 60))"
  elif (( seconds < 86400 )); then
    printf '%s時間%s分' "$((seconds / 3600))" "$(((seconds % 3600) / 60))"
  else
    printf '%s日%s時間' "$((seconds / 86400))" "$(((seconds % 86400) / 3600))"
  fi
}


# Detect operational anomalies from purely local state (see docs/decisions/
# 0005): a stale supervisor pid/lock, an expired lease, a running Issue with
# no local worker state (may be legitimate on a multi-host setup), a local
# worker whose Issue GitHub no longer reports as agent:running, residual
# worktrees/branches, corrupted local-state files, a pending Project sync
# retry queue, and an active claim pause. Never mutates state.
status_collect_anomalies() {
  local i issue started running_csv=',' count pause orphan_since orphan_elapsed
  if [[ -r $STATE_ROOT/supervisor.pid ]] && ! pid_alive; then
    anomaly_add warning supervisor-stale-pid supervisor.pid 'supervisor.pid が残っていますが、対応するprocessは生存していません。' 'bin/agentic-loop start を実行してください（start/stop が stale state を自動整理します）。' "$(status_file_elapsed "$STATE_ROOT/supervisor.pid")" needs-attention
  fi
  status_supervisor_lock_stale && anomaly_add warning supervisor-stale-pid supervisor.lock 'supervisor.lock が残っていますが、生存しているsupervisorがありません。' 'bin/agentic-loop start を実行してください（start/stop が stale state を自動整理します）。' "$(status_file_elapsed "$STATE_ROOT/supervisor.lock")" needs-attention

  for i in "${!RUN_NUM[@]}"; do
    issue=${RUN_NUM[$i]}
    running_csv+="$issue,"
    if [[ -r $(worker_started_file "$issue") ]]; then
      read -r started < "$(worker_started_file "$issue")" || started=''
      [[ $started =~ ^[0-9]+$ ]] || anomaly_add warning local-state-corrupt "#$issue" "workers/$issue.started の内容が不正です。" "workers/$issue.started を確認し、worker停止後に問題の workers/$issue.* state を除去してください。" "$(status_file_elapsed "$(worker_started_file "$issue")")" needs-attention
    fi
    local to_exceeded=0 elapsed
    if (( WORKER_TIMEOUT_SECONDS > 0 )) && worker_pid_live "$issue"; then
      elapsed=$(worker_elapsed_seconds "$issue" 2>/dev/null || true)
      if [[ $elapsed =~ ^[0-9]+$ ]] && (( elapsed >= WORKER_TIMEOUT_SECONDS )); then
        to_exceeded=1
        anomaly_add info worker-timeout "#$issue" "実行時間上限（${WORKER_TIMEOUT_SECONDS}秒）を超過しています。" '次pollでworkerを停止し、安全に再試行します。' "$elapsed" recovering
      fi
    fi
    status_progress_health "$issue" "$to_exceeded"
    if [[ $STATUS_RUN_HEALTH == stalled ]]; then
      anomaly_add warning worker-stalled "#$issue" "最後の進行から ${STATUS_RUN_PROGRESS_AGE}秒経過しています（stall閾値 ${STATUS_STALL_SECONDS}秒）。" 'status --watch または tail で進行を確認し、必要なら worker_timeout_seconds 到達後の再試行を確認してください。' "${STATUS_RUN_PROGRESS_AGE}" needs-attention
    fi
    LEASE_ID='' LEASE_EXPIRES='' LEASE_HEARTBEAT=''
    if lease_read "$issue"; then
      if [[ -n $LEASE_ID && ! $LEASE_ID =~ ^[0-9]+$ ]]; then
        anomaly_add warning local-state-corrupt "#$issue" "workers/$issue.lease の内容が不正です。" "workers/$issue.lease を確認し、worker停止後に問題の workers/$issue.* state を除去してください。" "$(status_file_elapsed "$STATE_ROOT/workers/$issue.lease")" needs-attention
      elif [[ $LEASE_EXPIRES =~ ^[0-9]+$ ]] && (( $(date +%s) > LEASE_EXPIRES )); then
        anomaly_add info lease-expired "#$issue" "リースが期限切れです（expires=$LEASE_EXPIRES）。" '次pollで安全にqueueへ戻します。' "$(status_elapsed_since "$LEASE_EXPIRES")" recovering
      fi
    fi
  done

  shopt -s nullglob
  local pidfile
  for pidfile in "$STATE_ROOT"/workers/*.pid; do
    issue=$(basename "$pidfile" .pid)
    if worker_pid_live "$issue" && [[ $running_csv != *",$issue,"* ]]; then
      if (( WORKER_ORPHAN_GRACE_SECONDS == 0 )); then
        anomaly_add warning worker-orphan "#$issue" "local workerは生存していますが、GitHub上ではagent:runningではありません。queue.worker_orphan_grace_seconds=0 のため自動停止は無効です。" 'queue.worker_orphan_grace_seconds を設定して自動停止を有効化するか、対象workerを停止してください（worker_timeout_seconds 到達までは停止しません）。' "$(status_file_elapsed "$pidfile")" needs-attention
        continue
      fi
      orphan_since=''
      if [[ -r $(worker_orphan_since_file "$issue") ]]; then
        read -r orphan_since < "$(worker_orphan_since_file "$issue")" || orphan_since=''
      fi
      if [[ $orphan_since =~ ^[0-9]+$ ]]; then
        orphan_elapsed=$(status_elapsed_since "$orphan_since")
        anomaly_add info worker-orphan "#$issue" "local workerは生存していますが、GitHub上ではagent:runningではありません（grace ${WORKER_ORPHAN_GRACE_SECONDS}秒、観測 ${orphan_elapsed}秒）。" "grace ${WORKER_ORPHAN_GRACE_SECONDS}秒を超えた次pollでworkerを自動停止します。" "$orphan_elapsed" recovering
      else
        anomaly_add info worker-orphan "#$issue" "local workerは生存していますが、GitHub上ではagent:runningではありません（grace ${WORKER_ORPHAN_GRACE_SECONDS}秒、観測 0秒）。" "次pollからgrace ${WORKER_ORPHAN_GRACE_SECONDS}秒の観測を開始します。" 0 recovering
      fi
    fi
  done
  shopt -u nullglob

  if [[ -r $STATE_ROOT/project-pending ]]; then
    count=$(wc -l < "$STATE_ROOT/project-pending" 2>/dev/null || printf 0)
    (( count > 0 )) && anomaly_add info project-sync-pending project "Project同期の再試行待ちが $count 件あります。" '次pollで保留中のProject同期を再試行します。' "$(status_marker_content_elapsed "$STATE_ROOT/project-pending.since")" recovering
  fi

  for i in "${!PAUSED_NUM[@]}"; do
    issue=${PAUSED_NUM[$i]}
    worker_pid_live "$issue" && anomaly_add info paused-worker-live "#$issue" 'agent:pausedですが、このホストのlocal workerがまだ生存しています。' '次pollのdrain_paused_workersでworkerを停止します。' "$(status_file_elapsed "$STATE_ROOT/workers/$issue.pid")" recovering
  done

  status_claim_pause_reasons
  pause=$STATUS_PAUSE_TEXT
  [[ -n $pause ]] && anomaly_add info claim-paused supervisor "claimを一時停止しています: $pause" '次pollで利用可能なprovider・予算・停止要求を再評価し、可能になればclaimを再開します。' "$(status_oldest_marker_elapsed)" recovering
  return 0
}


status_collect_snapshot() {
  STATUS_GITHUB_OK=1
  local raw num title state priority catrank created
  if status_cache_fresh "$STATUS_SNAPSHOT_FETCHED"; then
    raw=$STATUS_SNAPSHOT_RAW
  elif ! raw=$(status_snapshot_fetch); then
    STATUS_GITHUB_OK=0
    return 0
  else
    STATUS_SNAPSHOT_RAW=$raw
    STATUS_SNAPSHOT_FETCHED=$(date +%s)
  fi
  while IFS=$'\t' read -r num title state priority catrank created; do
    [[ -n $num ]] || continue
    case $state in
      running) RUN_NUM+=("$num"); RUN_TITLE+=("$title") ;;
      queued) QUEUE_NUM+=("$num"); QUEUE_TITLE+=("$title"); QUEUE_PRIORITY+=("$priority"); QUEUE_CATRANK+=("$catrank"); QUEUE_CREATED+=("$created") ;;
      needs-input) NEEDSINPUT_NUM+=("$num"); NEEDSINPUT_TITLE+=("$title") ;;
      failed) FAILED_NUM+=("$num"); FAILED_TITLE+=("$title") ;;
      parked) PARKED_NUM+=("$num"); PARKED_TITLE+=("$title") ;;
      in-review) INREVIEW_NUM+=("$num"); INREVIEW_TITLE+=("$title") ;;
      blocked) BLOCKED_NUM+=("$num"); BLOCKED_TITLE+=("$title") ;;
      paused) PAUSED_NUM+=("$num"); PAUSED_TITLE+=("$title") ;;
    esac
  done <<< "$raw"
  return 0
}


status_collect_stale() {
  local raw num title count=0
  if status_cache_fresh "$STATUS_STALE_FETCHED"; then
    raw=$STATUS_STALE_RAW
  elif ! raw=$(status_stale_fetch); then
    return 0
  else
    STATUS_STALE_RAW=$raw
    STATUS_STALE_FETCHED=$(date +%s)
  fi
  while IFS=$'\t' read -r num title; do
    [[ -n $num ]] || continue
    STALE_NUM+=("$num"); STALE_TITLE+=("$title")
    count=$((count + 1))
  done <<< "$raw"
  (( count >= 100 )) && STATUS_STALE_TRUNCATED=1
  return 0
}


# The agent:queued snapshot rows, sorted with claim_next's exact comparator
# (numeric priority desc, category rank, created_at, number) so the candidate
# preview matches the order Issues will actually be claimed in.
status_queue_sorted() {
  local i
  for i in "${!QUEUE_NUM[@]}"; do
    printf '%s\t%s\t%s\t%s\t%s\n' "${QUEUE_PRIORITY[$i]}" "${QUEUE_CATRANK[$i]}" "${QUEUE_CREATED[$i]}" "${QUEUE_NUM[$i]}" "${QUEUE_TITLE[$i]}"
  done | sort -k1,1nr -k2,2n -k3,3 -k4,4n
}


status_render_text_state_line() {
  local label=$1 numname=$2
  local -n nums=$numname
  local url_list='' n
  for n in "${nums[@]}"; do url_list+="${url_list:+, }https://github.com/$(repo_name)/issues/$n"; done
  if (( ${#nums[@]} > 0 )); then
    printf '  %s: %s件 (%s)\n' "$label" "${#nums[@]}" "$url_list"
  else
    printf '  %s: 0件\n' "$label"
  fi
}


status_render_text() {
  if pid_alive; then say "running (pid $(cat "$STATE_ROOT/supervisor.pid"), max workers $MAX_WORKERS)"; else say 'stopped'; fi

  local phase
  for phase in plan exec; do
    if agent_next_candidate_nominal "$phase"; then
      printf '次の%s候補: pool=%s provider=%s model=%s (tier=%s, model_index=%s)\n' \
        "$phase" "$NC_POOL" "$NC_PROVIDER" "$NC_MODEL" "$NC_TIER" "$NC_MODEL_IDX"
    else
      printf '次の%s候補: なし（全プール利用不可）\n' "$phase"
    fi
  done

  local i issue title suffix
  if (( STATUS_GITHUB_OK == 0 )); then
    say 'Running Issues: 不明（GitHub取得不能）'
  elif (( ${#RUN_NUM[@]} > 0 )); then
    printf 'Running Issues:\n'
    for i in "${!RUN_NUM[@]}"; do
      issue=${RUN_NUM[$i]}; title=${RUN_TITLE[$i]}
      status_running_detail "$issue"
      suffix=''
      [[ -n $STATUS_RUN_STAGE ]] && suffix+=" (stage: $STATUS_RUN_STAGE)"
      [[ -n $STATUS_RUN_PROGRESS_AGE ]] && suffix+=" (progress: ${STATUS_RUN_PROGRESS_AGE}s ago)"
      [[ -n $STATUS_RUN_HEALTH ]] && suffix+=" (health: $(status_health_color "$STATUS_RUN_HEALTH"))"
      [[ -n $STATUS_RUN_PHASE ]] && suffix+=" (phase: $STATUS_RUN_PHASE)"
      [[ -n $STATUS_RUN_SCOPE ]] && suffix+=" (scope: $(paste -sd', ' - <<< "$STATUS_RUN_SCOPE"))"
      [[ -n $STATUS_RUN_STARTED ]] && suffix+=" (started: $STATUS_RUN_STARTED, elapsed: ${STATUS_RUN_ELAPSED}s)"
      if [[ -n $STATUS_RUN_TIMEOUT_AT ]]; then
        if (( STATUS_RUN_TIMEOUT_EXCEEDED )); then suffix+=" (timeout_at: $STATUS_RUN_TIMEOUT_AT 超過)"
        else suffix+=" (timeout_at: $STATUS_RUN_TIMEOUT_AT)"; fi
      fi
      [[ -n $STATUS_RUN_HEARTBEAT ]] && suffix+=" (heartbeat: $STATUS_RUN_HEARTBEAT)"
      if [[ -n $STATUS_RUN_LEASE_EXPIRES ]]; then
        if (( STATUS_RUN_LEASE_EXPIRED )); then suffix+=" (lease_expires: $STATUS_RUN_LEASE_EXPIRES 期限切れ)"
        else suffix+=" (lease_expires: $STATUS_RUN_LEASE_EXPIRES)"; fi
      fi
      if (( STATUS_RUN_WORKTREE_EXISTS )); then suffix+=" (worktree: $STATUS_RUN_WORKTREE, dirty=${STATUS_RUN_DIRTY:-0}, diverged=${STATUS_RUN_DIVERGED:-0})"
      else suffix+=" (worktree: $STATUS_RUN_WORKTREE なし)"; fi
      [[ -n $STATUS_RUN_PR ]] && suffix+=" (pr: #$STATUS_RUN_PR state=${STATUS_RUN_PR_STATE:-unknown} checks=${STATUS_RUN_CHECKS:-unknown})"
      (( STATUS_RUN_LOCAL )) || suffix+=' (worker: 不明。別ホストが担当している可能性があります)'
      printf '#%s %s%s\n' "$issue" "$title" "$suffix"
    done
  else
    say 'Running Issues: none'
  fi

  local conflicts
  conflicts=$(status_conflict_waits)
  if [[ -n $conflicts ]]; then printf '競合待ちIssue:\n%s\n' "$conflicts"; else say '競合待ちIssue: none'; fi
  local dependencies
  dependencies=$(status_dependency_waits)
  if [[ -n $dependencies ]]; then printf '依存待ちIssue:\n%s\n' "$dependencies"; else say '依存待ちIssue: none'; fi

  if (( STATUS_GITHUB_OK == 0 )); then
    say 'GitHub: 取得できません（localの情報のみ表示します）。'
  else
    local claimable=0 rank1 rank2 created num shown=0
    while IFS=$'\t' read -r rank1 rank2 created num title; do
      [[ -n $num ]] || continue
      status_queue_candidate_reason "$num"
      [[ $STATUS_CANDIDATE_REASON == claimable ]] && claimable=$((claimable + 1))
    done < <(status_queue_sorted)
    printf 'キュー: %s件（claim可能 %s件）\n' "${#QUEUE_NUM[@]}" "$claimable"
    if (( ${#QUEUE_NUM[@]} > 0 )); then
      printf '次のclaim候補:\n'
      while IFS=$'\t' read -r rank1 rank2 created num title; do
        [[ -n $num ]] || continue
        (( shown >= 5 )) && break
        status_queue_candidate_reason "$num"
        if [[ $STATUS_CANDIDATE_REASON == claimable ]]; then
          printf '#%s %s (claimable)\n' "$num" "$title"
        else
          printf '#%s %s (withheld: %s %s)\n' "$num" "$title" "$STATUS_CANDIDATE_REASON" "$STATUS_CANDIDATE_DETAIL"
        fi
        shown=$((shown + 1))
      done < <(status_queue_sorted)
    fi

    printf '状態サマリ:\n'
    status_render_text_state_line needs-input NEEDSINPUT_NUM
    status_render_text_state_line failed FAILED_NUM
    status_render_text_state_line parked PARKED_NUM
    status_render_text_state_line in-review INREVIEW_NUM
    status_render_text_state_line blocked BLOCKED_NUM
    status_render_text_state_line paused PAUSED_NUM
    for i in "${!PAUSED_NUM[@]}"; do
      if control_pause_record_read "${PAUSED_NUM[$i]}"; then
        printf '    #%s (paused: @%s, at: %s, from: %s)\n' "${PAUSED_NUM[$i]}" "$CONTROL_PAUSE_ACTOR" "$CONTROL_PAUSE_AT" "$CONTROL_PAUSE_FROM"
      fi
    done
    status_render_text_state_line stale STALE_NUM
    (( STATUS_STALE_TRUNCATED )) && say '  stale は直近100件までの表示です（打ち切りあり）。'
  fi

  if (( ${#ANOMALY_LEVEL[@]} > 0 )); then
    local recovering=0 needs_attention=0
    for i in "${!ANOMALY_LEVEL[@]}"; do
      [[ ${ANOMALY_CLASSIFICATION[$i]} == recovering ]] && recovering=$((recovering + 1)) || needs_attention=$((needs_attention + 1))
    done
    if (( recovering > 0 )); then
      printf '自動回復中:\n'
      for i in "${!ANOMALY_LEVEL[@]}"; do
        [[ ${ANOMALY_CLASSIFICATION[$i]} == recovering ]] || continue
        printf '  %s %s: %s — 経過%s, %s\n' "${ANOMALY_CODE[$i]}" "${ANOMALY_SUBJECT[$i]}" "${ANOMALY_DETAIL[$i]}" "$(status_format_duration "${ANOMALY_ELAPSED[$i]}")" "${ANOMALY_ACTION[$i]}"
      done
    fi
    if (( needs_attention > 0 )); then
      printf '要対応:\n'
      for i in "${!ANOMALY_LEVEL[@]}"; do
        [[ ${ANOMALY_CLASSIFICATION[$i]} == needs-attention ]] || continue
        printf '  %s %s: %s — 経過%s → 対応: %s\n' "${ANOMALY_CODE[$i]}" "${ANOMALY_SUBJECT[$i]}" "${ANOMALY_DETAIL[$i]}" "$(status_format_duration "${ANOMALY_ELAPSED[$i]}")" "${ANOMALY_ACTION[$i]}"
      done
    fi
  else
    say '自動回復中: none'
    say '要対応: none'
  fi
}


status_render_json_state_group() {
  local label=$1 numname=$2 titlename=$3
  local -n nums=$numname
  local -n titles=$titlename
  local sep='' i
  printf '"%s":{"count":%s,"issues":[' "$label" "${#nums[@]}"
  for i in "${!nums[@]}"; do
    printf '%s{"number":%s,"title":"%s"}' "$sep" "${nums[$i]}" "$(json_escape "${titles[$i]}")"
    sep=','
  done
  printf ']}'
}


status_render_json() {
  local i sep
  printf '{"schema_version":1,"generated_at":%s,"github_available":%s,' "$(date +%s)" "$( ((STATUS_GITHUB_OK)) && printf true || printf false )"
  if pid_alive; then
    printf '"supervisor":{"state":"running","pid":%s,"max_workers":%s},' "$(cat "$STATE_ROOT/supervisor.pid")" "$MAX_WORKERS"
  else
    printf '"supervisor":{"state":"stopped","pid":null,"max_workers":%s},' "$MAX_WORKERS"
  fi

  printf '"next_candidates":{'
  local phase sep=''
  for phase in plan exec; do
    if agent_next_candidate_nominal "$phase"; then
      printf '%s"%s":{"pool":"%s","provider":"%s","model":"%s","tier":%s,"model_index":%s}' "$sep" "$phase" \
        "$(json_escape "$NC_POOL")" "$(json_escape "$NC_PROVIDER")" "$(json_escape "$NC_MODEL")" "$NC_TIER" "$NC_MODEL_IDX"
    else
      printf '%s"%s":{"pool":null,"provider":null,"model":null,"tier":null,"model_index":null}' "$sep" "$phase"
    fi
    sep=','
  done
  printf '},'

  printf '"pools":['
  sep=''
  local pool resume='' basis probed=''
  for pool in $(agent_all_pools); do
    if agent_pool_marker_active "$pool"; then
      resume=''; read -r resume < "$(agent_pool_marker "$pool")" 2>/dev/null || true
      [[ $resume =~ ^[0-9]+$ ]] || resume=''
      basis=$(agent_pool_basis_get "$pool")
      probed=$(status_pool_last_probed "$pool")
      printf '%s{"pool":"%s","exhausted":true,"resume_at":%s,"basis":"%s","probed_at":%s}' \
        "$sep" "$(json_escape "$pool")" "${resume:-null}" "$(json_escape "$basis")" "${probed:-null}"
    else
      printf '%s{"pool":"%s","exhausted":false,"resume_at":null,"basis":null,"probed_at":null}' "$sep" "$(json_escape "$pool")"
    fi
    sep=','
  done
  printf '],'

  printf '"workers":['
  sep=''
  for i in "${!RUN_NUM[@]}"; do
    status_running_detail "${RUN_NUM[$i]}"
    printf '%s{"issue":%s,"title":"%s","phase":"%s","scope":[' "$sep" "${RUN_NUM[$i]}" "$(json_escape "${RUN_TITLE[$i]}")" "$(json_escape "$STATUS_RUN_PHASE")"
    if [[ -n $STATUS_RUN_SCOPE ]]; then
      local scope_sep='' scope_tok
      while IFS= read -r scope_tok; do
        [[ -n $scope_tok ]] || continue
        printf '%s"%s"' "$scope_sep" "$(json_escape "$scope_tok")"
        scope_sep=','
      done <<< "$STATUS_RUN_SCOPE"
    fi
    printf '],"started_at":%s,"elapsed_seconds":%s,"timeout_at":%s,"timeout_exceeded":%s,"heartbeat_at":%s,"lease_expires_at":%s,"lease_expired":%s,"worktree":"%s","worktree_exists":%s,"dirty":%s,"diverged":%s,"branch":"%s","pr":%s,"pr_url":"%s","pr_state":"%s","checks":"%s","local_state":%s,"stage":"%s","progress_at":%s,"progress_age_seconds":%s,"health":"%s"}' \
      "${STATUS_RUN_STARTED:-null}" "${STATUS_RUN_ELAPSED:-null}" "${STATUS_RUN_TIMEOUT_AT:-null}" \
      "$( ((STATUS_RUN_TIMEOUT_EXCEEDED)) && printf true || printf false )" \
      "${STATUS_RUN_HEARTBEAT:-null}" "${STATUS_RUN_LEASE_EXPIRES:-null}" \
      "$( ((STATUS_RUN_LEASE_EXPIRED)) && printf true || printf false )" "$(json_escape "$STATUS_RUN_WORKTREE")" \
      "$( ((STATUS_RUN_WORKTREE_EXISTS)) && printf true || printf false )" \
      "$( ((STATUS_RUN_DIRTY)) && printf true || printf false )" "$( ((STATUS_RUN_DIVERGED)) && printf true || printf false )" \
      "$(json_escape "$STATUS_RUN_BRANCH")" \
      "${STATUS_RUN_PR:-null}" "$(json_escape "$STATUS_RUN_PR_URL")" "$(json_escape "$STATUS_RUN_PR_STATE")" "$(json_escape "$STATUS_RUN_CHECKS")" \
      "$( ((STATUS_RUN_LOCAL)) && printf true || printf false )" \
      "$(json_escape "$STATUS_RUN_STAGE")" "${STATUS_RUN_PROGRESS_AT:-null}" "${STATUS_RUN_PROGRESS_AGE:-null}" "$(json_escape "$STATUS_RUN_HEALTH")"
    sep=','
  done
  printf '],'

  local claimable=0 rank1 rank2 created num title
  while IFS=$'\t' read -r rank1 rank2 created num title; do
    [[ -n $num ]] || continue
    status_queue_candidate_reason "$num"
    [[ $STATUS_CANDIDATE_REASON == claimable ]] && claimable=$((claimable + 1))
  done < <(status_queue_sorted)
  printf '"queue":{"queued":%s,"claimable":%s,"candidates":[' "${#QUEUE_NUM[@]}" "$claimable"
  sep=''
  while IFS=$'\t' read -r rank1 rank2 created num title; do
    [[ -n $num ]] || continue
    status_queue_candidate_reason "$num"
    printf '%s{"issue":%s,"title":"%s","priority":%s,"category_rank":%s,"created_at":"%s","claimable":%s,"withheld":"%s","withheld_detail":"%s"}' \
      "$sep" "$num" "$(json_escape "$title")" "$rank1" "$rank2" "$(json_escape "$created")" \
      "$( [[ $STATUS_CANDIDATE_REASON == claimable ]] && printf true || printf false )" \
      "$(json_escape "$STATUS_CANDIDATE_REASON")" "$(json_escape "$STATUS_CANDIDATE_DETAIL")"
    sep=','
  done < <(status_queue_sorted)
  printf ']},'

  printf '"waits":{"scope":['
  sep=''
  local file issue other reason detail
  shopt -s nullglob
  for file in "$STATE_ROOT"/conflict/issue-*; do
    issue=${file##*/issue-}
    IFS=$'\t' read -r other reason < "$file" || continue
    printf '%s{"issue":%s,"other":%s,"reason":"%s"}' "$sep" "$issue" "$other" "$(json_escape "$reason")"
    sep=','
  done
  shopt -u nullglob
  printf '],"dependency":['
  sep=''
  shopt -s nullglob
  for file in "$(dependency_state_dir)"/blocked-*; do
    issue=${file##*/blocked-}
    IFS=$'\t' read -r reason detail < "$file" || continue
    printf '%s{"issue":%s,"reason":"%s","detail":"%s"}' "$sep" "$issue" "$(json_escape "$reason")" "$(json_escape "$detail")"
    sep=','
  done
  shopt -u nullglob
  printf ']},'

  printf '"states":{'
  status_render_json_state_group needs-input NEEDSINPUT_NUM NEEDSINPUT_TITLE
  printf ','
  status_render_json_state_group failed FAILED_NUM FAILED_TITLE
  printf ','
  status_render_json_state_group parked PARKED_NUM PARKED_TITLE
  printf ','
  status_render_json_state_group in-review INREVIEW_NUM INREVIEW_TITLE
  printf ','
  status_render_json_state_group blocked BLOCKED_NUM BLOCKED_TITLE
  printf ','
  status_render_json_state_group paused PAUSED_NUM PAUSED_TITLE
  printf ','
  printf '"stale":{"count":%s,"truncated":%s,"issues":[' "${#STALE_NUM[@]}" "$( ((STATUS_STALE_TRUNCATED)) && printf true || printf false )"
  sep=''
  for i in "${!STALE_NUM[@]}"; do
    printf '%s{"number":%s,"title":"%s"}' "$sep" "${STALE_NUM[$i]}" "$(json_escape "${STALE_TITLE[$i]}")"
    sep=','
  done
  printf ']}},'

  printf '"anomalies":['
  sep=''
  for i in "${!ANOMALY_LEVEL[@]}"; do
    printf '%s{"level":"%s","code":"%s","subject":"%s","detail":"%s","action":"%s","elapsed":%s,"classification":"%s"}' "$sep" "${ANOMALY_LEVEL[$i]}" "${ANOMALY_CODE[$i]}" "$(json_escape "${ANOMALY_SUBJECT[$i]}")" "$(json_escape "${ANOMALY_DETAIL[$i]}")" "$(json_escape "${ANOMALY_ACTION[$i]}")" "${ANOMALY_ELAPSED[$i]}" "${ANOMALY_CLASSIFICATION[$i]}"
    sep=','
  done
  printf ']}\n'
}


# `status --watch [N]`: stream events.log instead of re-rendering the snapshot
# (see docs/decisions/0005). On a TTY this is `tail --follow` over all Issues,
# so worker stage/progress events flow by as they are appended; on a pipe or
# redirect it prints the recent events once and exits (no loop). The legacy
# interval [N] is accepted for backward compatibility and ignored. SIGINT/
# SIGTERM exit 0 (inherited from tail_follow). GitHub/working tree untouched.
status_watch() {
  if [[ -t 1 ]]; then
    tail_follow ''
  else
    tail_recent ''
  fi
  return 0
}


cmd_status() {
  local format=text watch=0 watch_seconds=$STATUS_WATCH_DEFAULT_SECONDS
  while (( $# > 0 )); do
    case $1 in
      --format)
        [[ $2 == json ]] || { usage; return 2; }
        format=json; shift 2 ;;
      --watch)
        watch=1; shift
        if (( $# > 0 )) && [[ $1 =~ ^[0-9]+$ ]]; then watch_seconds=$1; shift; fi ;;
      *) usage; return 2 ;;
    esac
  done
  (( watch_seconds >= 1 )) || { usage; return 2; }
  if (( watch )) && [[ $format == json ]]; then
    usage; return 2
  fi
  if (( watch )); then
    status_watch
    return 0
  fi
  declare -ga RUN_NUM=() RUN_TITLE=()
  declare -ga QUEUE_NUM=() QUEUE_TITLE=() QUEUE_PRIORITY=() QUEUE_CATRANK=() QUEUE_CREATED=()
  declare -ga NEEDSINPUT_NUM=() NEEDSINPUT_TITLE=() FAILED_NUM=() FAILED_TITLE=() PARKED_NUM=() PARKED_TITLE=() INREVIEW_NUM=() INREVIEW_TITLE=() BLOCKED_NUM=() BLOCKED_TITLE=() PAUSED_NUM=() PAUSED_TITLE=()
  declare -ga STALE_NUM=() STALE_TITLE=()
  declare -ga ANOMALY_LEVEL=() ANOMALY_CODE=() ANOMALY_SUBJECT=() ANOMALY_DETAIL=() ANOMALY_ACTION=() ANOMALY_ELAPSED=() ANOMALY_CLASSIFICATION=()
  declare -ga STATUS_PAUSE_MARKERS=()
  declare -g STATUS_PAUSE_TEXT=''
  declare -g STATUS_GITHUB_OK=1 STATUS_STALE_TRUNCATED=0 STATUS_SNAPSHOT_RAW='' STATUS_SNAPSHOT_FETCHED='' STATUS_STALE_RAW='' STATUS_STALE_FETCHED=''
  status_collect_snapshot
  status_collect_stale
  status_collect_anomalies
  if [[ $format == json ]]; then status_render_json; else status_render_text; fi
  return 0
}


# --- tail: stream supervisor/worker progress events (see docs/decisions/0005) ---
# Reads only the append-only events.log in Git-common state: REST(core) 0,
# no GitHub/worktree writes, and never reads log or Issue bodies (secret-safe).
tail_event_line() {
  local epoch=$1 subject=$2 code=$3 stage=$4 when
  [[ $epoch =~ ^[0-9]+$ ]] || return 0
  when=$(date -d "@$epoch" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || printf '%s' "$epoch")
  printf '%s  %-11s  %-8s  %s\n' "$when" "$subject" "$code" "${stage:--}"
}


# Print events from $offset bytes onward in $file (empty file/missing -> no-op),
# optionally filtered to one Issue. Events go straight to stdout; the new byte
# offset is published in TAIL_NEW_OFFSET so the caller never captures (and thus
# swallows) the display lines in a command substitution.
tail_print_events() {
  local file=$1 offset=$2 issue=$3 size content line epoch subject code stage
  TAIL_NEW_OFFSET=0
  [[ -r $file ]] || return 0
  size=$(wc -c < "$file" 2>/dev/null || printf 0)
  (( size < offset )) && offset=0
  if (( size > offset )); then
    content=$(tail -c +"$((offset + 1))" "$file" 2>/dev/null || true)
    while IFS= read -r line; do
      IFS=$'\t' read -r epoch subject code stage <<< "$line"
      [[ $epoch =~ ^[0-9]+$ ]] || continue
      if [[ -z $issue || $subject == "$issue" ]]; then
        tail_event_line "$epoch" "$subject" "$code" "${stage:--}"
      fi
    done <<< "$content"
  fi
  TAIL_NEW_OFFSET=$size
}


# `tail --follow`: print new events as they are appended (rotated logs are
# detected by inode change; the offset then restarts). SIGINT/SIGTERM exit 0.
tail_follow() {
  local issue=$1 file=$EVENTS_LOG offset=0 inode='' new_inode interrupted=0
  trap 'interrupted=1' INT TERM
  while (( interrupted == 0 )); do
    tail_print_events "$file" "$offset" "$issue"
    offset=$TAIL_NEW_OFFSET
    new_inode=$(stat -c %i "$file" 2>/dev/null || printf 0)
    if [[ -n $inode && $new_inode != "$inode" ]]; then inode=$new_inode; offset=0; fi
    [[ -z $inode ]] && inode=$new_inode
    sleep 1
  done
  return 0
}


# Print the most recent TAIL_MAX_LINES events once and return, optionally
# filtered to one Issue: the non-follow half of `tail`, also used by
# `status --watch` on a pipe/redirect so it never enters a follow loop.
tail_recent() {
  local issue=$1
  [[ -r $EVENTS_LOG ]] || return 0
  tail -n "$TAIL_MAX_LINES" "$EVENTS_LOG" 2>/dev/null | while IFS=$'\t' read -r epoch subject code stage; do
    if [[ -z $issue || $subject == "$issue" ]]; then
      tail_event_line "$epoch" "$subject" "$code" "${stage:--}"
    fi
  done
}


cmd_tail() {
  local issue='' follow=0
  while (( $# > 0 )); do
    case $1 in
      --issue) [[ $# -ge 2 && $2 =~ ^[0-9]+$ ]] || { usage; return 2; }; issue=$2; shift 2 ;;
      --follow | -f) follow=1; shift ;;
      *) usage; return 2 ;;
    esac
  done
  if (( follow )); then
    tail_follow "$issue"
    return 0
  fi
  tail_recent "$issue"
  return 0
}
