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


# `gh api --paginate --jq` evaluates jq once per response page. Keep the
# per-comment filter, then fold the page outputs in the shell so the newest
# lease is selected across the complete response rather than only the last
# page (Issue #238).
latest_lease_body() {
  local issue=$1 since=$2 output
  # workload-unbounded: Issueに紐づく有限のコメント件数からleaseを全件走査; bound=Issue comment count; track=#238
  output=$(repo_api "issues/$issue/comments" --method GET -f since="$since" -f per_page=100 --paginate --jq '.[] | .body | select(contains("agentic-loop:lease"))' 2>/dev/null) || return 1
  awk 'NF { value=$0 } END { if (value != "") print value }' <<< "$output"
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
  case $code in progress | claim | recover | timeout | orphan | prune | stop | start) ;; *) return 0 ;; esac
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


# --- stall-threshold calibration against real stage durations (Issue #280,
# ADR 0032) -- host-local read of the shared events.log, zero GitHub calls.
# For each subject, the gap between two consecutive `progress` rows is the
# duration status's stall detection measures for the *departing* stage (its
# last progress marker's age at the moment of the next transition). Bounded
# to the newest EVENTS_CALIBRATION_MAX_LINES lines. A subject's final observed
# stage is never sampled (it has no next transition, so its duration is
# unknown -- still running or the log rotated mid-attempt). A gap exceeding
# worker_timeout_seconds is dropped: it is host/timeout-attributable, not a
# stage's natural duration, and would otherwise corrupt p90. Prints
# band<TAB>seconds, one row per sample; band is "provider" for the stages
# status_stall_threshold widens (plan/exec/replan), "non-provider" otherwise.
events_stage_duration_samples() {
  [[ -r $EVENTS_LOG ]] || return 0
  local epoch subject code stage band delta
  local -A last_epoch=() last_stage=()
  while IFS=$'\t' read -r epoch subject code stage; do
    [[ $code == progress ]] || continue
    [[ $epoch =~ ^[0-9]+$ ]] || continue
    if [[ -n ${last_epoch[$subject]:-} ]]; then
      delta=$((epoch - last_epoch[$subject]))
      if (( delta > 0 )) && { (( WORKER_TIMEOUT_SECONDS == 0 )) || (( delta <= WORKER_TIMEOUT_SECONDS )); }; then
        case ${last_stage[$subject]} in
          plan | exec | replan) band=provider ;;
          *) band=non-provider ;;
        esac
        printf '%s\t%s\n' "$band" "$delta"
      fi
    fi
    last_epoch[$subject]=$epoch
    last_stage[$subject]=$stage
  done < <(tail -n "$EVENTS_CALIBRATION_MAX_LINES" "$EVENTS_LOG")
}


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


# Secondary progress signal (see docs/decisions/0031): the newest mtime among
# this Issue's stage output files -- the worker log plus the plan/result
# files agent_run_stage writes, including their in-flight `.raw.*`/`.final.*`
# siblings written while a provider CLI is still streaming a stage. Only
# mtimes are read, never content, so a provider's in-progress output can never
# leak into status/tail (secret boundary, same as worker_log_mtime). Returns
# failure only when none of these paths exist yet.
worker_stage_output_mtime() {
  local issue=$1 latest='' mtime path
  shopt -s nullglob
  local -a paths=(
    "$STATE_ROOT/logs/issue-$issue.log"
    "$STATE_ROOT/issue-$issue-plan.txt"
    "$STATE_ROOT/issue-$issue-result.txt"
    "$STATE_ROOT/issue-$issue-plan.txt".raw.*
    "$STATE_ROOT/issue-$issue-plan.txt".final.*
    "$STATE_ROOT/issue-$issue-result.txt".raw.*
    "$STATE_ROOT/issue-$issue-result.txt".final.*
  )
  shopt -u nullglob
  for path in "${paths[@]}"; do
    mtime=$(stat -c %Y "$path" 2>/dev/null) || continue
    if [[ -z $latest ]] || (( mtime > latest )); then latest=$mtime; fi
  done
  [[ -n $latest ]] || return 1
  printf '%s\n' "$latest"
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
  rm -f "$STATE_ROOT/workers/$1.pid" "$(lease_file "$1")" "$(worker_phase_file "$1")" "$(worker_started_file "$1")" "$(worker_resume_file "$1")" "$(worker_progress_file "$1")" "$(worker_stop_request_file "$1")" "$(worker_critical_file "$1")" "$(worker_orphan_since_file "$1")"
}


# --- Cooperative stop / critical-section markers (Issue #57) ---
# A stop request is a plain marker file the worker checks only at its own
# stage boundaries (loop top, pre-replan): it can never interrupt mid-provider-
# call, only skip starting the next stage. `control.sh`'s drain function waits
# out an active critical section before escalating to TERM/KILL, so a write
# sequence the worker itself marked unsafe-to-interrupt (see worker_critical_
# begin call sites in worker.sh) gets a bounded chance to finish first.
worker_stop_request_file() { printf '%s/workers/%s.stop-requested' "$STATE_ROOT" "$1"; }

worker_request_stop() { mkdir -p "$STATE_ROOT/workers"; : > "$(worker_stop_request_file "$1")"; }

worker_stop_requested() { [[ -e $(worker_stop_request_file "$1") ]]; }

worker_critical_file() { printf '%s/workers/%s.critical' "$STATE_ROOT" "$1"; }

worker_critical_begin() { mkdir -p "$STATE_ROOT/workers"; : > "$(worker_critical_file "$1")"; }

worker_critical_end() { rm -f "$(worker_critical_file "$1")"; }

worker_critical_active() { [[ -e $(worker_critical_file "$1") ]]; }

worker_phase_file() { printf '%s/workers/%s.phase' "$STATE_ROOT" "$1"; }

# First-observed epoch of this Issue's local worker looking orphaned (live
# pidfile, GitHub no longer reports agent:running); see reap_orphan_workers.
worker_orphan_since_file() { printf '%s/workers/%s.orphan-since' "$STATE_ROOT" "$1"; }


# Create the single lease comment for this Issue and remember its id, expiry
# and heartbeat time so later heartbeats update it in place and `status` can
# read the lease locally without an extra GitHub call. See docs/decisions/0003:
# one comment per Issue updated via PATCH keeps the Issue clean and is gentle
# on the secondary rate limit (bursty comment creation is what trips abuse
# detection).
lease_start() {
  local issue=$1 worker=$2 now expires id
  now=$(date +%s); expires=$((now + LEASE_SECONDS))
  id=$(comment_post "$issue" "$(lease_body "$worker" "$now" "$expires")" --jq '.id' 2>/dev/null | tr -d '[:space:]' || true)
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
  id=$(comment_post "$issue" "$(lease_body "$worker" "$now" "$expires")" --jq '.id' 2>/dev/null | tr -d '[:space:]' || true)
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
    comment_patch "$id" "<!-- agentic-loop:claim worker=$worker created=$now expires=0 -->\\nclaim競合に敗れたため解放しました。" >/dev/null 2>&1 || true
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
  comment_patch "$id" "<!-- agentic-loop:claim worker=$worker created=$now expires=0 -->\\n<!-- agentic-loop:lease worker=$worker heartbeat=$now expires=0 -->\\nAgentic Loop worker \`$worker\` のハートビートです。処理終了に伴い、このホストはclaimを解放しました。" >/dev/null 2>&1 || true
}


# Refresh the lease by editing the existing comment (a single PATCH). Fall back
# to creating a fresh comment if we have no id yet, or the stored comment is gone.
lease_heartbeat() {
  local issue=$1 worker=$2 now expires id file rest
  file="$(lease_file "$issue")"
  now=$(date +%s); expires=$((now + LEASE_SECONDS))
  if [[ -r $file ]]; then
    IFS=$'\t' read -r id rest < "$file" || id=''
    if [[ $id =~ ^[0-9]+$ ]] && comment_patch "$id" "$(lease_body "$worker" "$now" "$expires")" >/dev/null 2>&1; then
      printf '%s\t%s\t%s\n' "$id" "$expires" "$now" > "$file"
      return 0
    fi
  fi
  lease_start "$issue" "$worker"
}


# Self-heal the Issue's agent status while this host's worker is genuinely
# working it. A foreign supervisor (a stale, provider-exhausted, or clock-
# skewed instance on another host whose recover_expired misjudges the lease --
# see docs/decisions and the loop-continuity Issues) can revert
# agent:running -> agent:queued out from under a live worker, so `status`
# reports "Running Issues: none" and Project state desyncs even though the work
# is progressing. Re-assert agent:running when -- and only when -- the Issue is
# sitting at exactly agent:queued while we hold it and no stop was requested.
# Restricted to the queued signature so legitimate operator transitions
# (paused / needs-input / blocked) and terminal states are never overridden;
# quiet (no comment) so it does not amplify the foreign supervisor's
# recovered-comment churn. Best-effort: never fatal to the heartbeat loop, and
# a no-op on the common healthy path where the label is already running.
worker_reassert_running() {
  local issue=$1 labels
  worker_stop_requested "$issue" && return 0
  labels=$(repo_api "issues/$issue" --jq '[.labels[].name | select(startswith("agent:"))] | join(",")' 2>/dev/null) || return 0
  [[ $labels == "$(state_label queued)" ]] || return 0
  set_issue_state "$issue" running 2>/dev/null || return 0
  project_sync_state "$issue" running 2>/dev/null || true
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
  # A worker started through the script's shebang has argv[0]=bash and the
  # absolute program path in argv[1].  Accept both forms while keeping the
  # program path and complete Issue number as token boundaries.
  [[ $command_line == "$PROGRAM_PATH _worker $issue "* ||
    $command_line == *"$PROGRAM_PATH _worker $issue "* ]]
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

# A pidfile is actionable only while the recorded process is this repository's
# worker for the same Issue.  A live but unrelated pid is a stale pidfile
# (including after host restart and pid reuse), never a worker to stop or count.
worker_pid_owned() {
  local issue=$1 pid pidfile pid_pgid worker_pid worker_pgid
  worker_alive "$issue" && return 0
  pidfile="$STATE_ROOT/workers/$issue.pid"
  [[ -r $pidfile ]] || return 1
  read -r pid < "$pidfile" || return 1
  [[ $pid =~ ^[0-9]+$ ]] || return 1
  pid_pgid=$(process_group_of "$pid") || return 1
  [[ $pid_pgid =~ ^[0-9]+$ ]] || return 1
  while IFS=$'\t' read -r worker_issue worker_pid; do
    [[ $worker_issue == "$issue" ]] || continue
    worker_pgid=$(process_group_of "$worker_pid") || continue
    [[ $worker_pgid == "$pid_pgid" ]] && return 0
  done < <(live_worker_processes)
  return 1
}

# Remove stale local state before maintenance can derive elapsed time or
# reserve a worker slot from an unrelated process.  This is deliberately
# limited to pidfiles whose Issue number is local bookkeeping; it never sends
# a signal to the recorded pid.
quarantine_stale_worker_pidfiles() {
  local pidfile issue
  shopt -s nullglob
  for pidfile in "$STATE_ROOT"/workers/*.pid; do
    issue=$(basename "$pidfile" .pid)
    [[ $issue =~ ^[0-9]+$ ]] || continue
    worker_pid_owned "$issue" && continue
    clear_worker_local "$issue"
  done
  shopt -u nullglob
}


# The process-group id of `pid`, read from /proc at call time (procps is not a
# pinned dependency of this repository, so `ps -o pgid=` is deliberately not
# used). Field 2 of /proc/<pid>/stat is the parenthesized comm, which may
# itself contain spaces and ')', so the remainder is taken after the *last*
# ') ' -- what follows is "state ppid pgrp ...".
process_group_of() {
  local pid=$1 stat rest
  local -a fields
  [[ $pid =~ ^[0-9]+$ ]] || return 1
  [[ -r /proc/$pid/stat ]] || return 1
  read -r stat < "/proc/$pid/stat" || return 1
  rest=${stat##*') '}
  read -r -a fields <<< "$rest"
  [[ ${fields[2]:-} =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "${fields[2]}"
}


# Signal a worker's whole process tree, given the pid recorded in its pidfile.
#
# Every worker-stop path (graceful shutdown, timeout enforcement, orphan reap,
# pause/abort drain, dispose) must take the provider CLI child down with the
# worker, so the signal goes to a process *group*. The pidfile's pid used to be
# passed straight to `kill -TERM "-$pid"`, which assumes that pid is its own
# group leader. It usually is (claim spawns workers via `setsid`), but not
# always: reconcile_worker_pidfiles repairs a lost pidfile from the live /proc
# scan (Issue #219) and legitimately records a *descendant* of the group
# leader. `-$pid` then names a process group that does not exist, kill fails
# with ESRCH, and every call site swallows it with `|| true` -- so the worker
# survives every stop attempt and holds its MAX_WORKERS slot forever (Issue
# #292: #251's pid 2688428 / pgid 2688254 was "reaped" once per poll for
# 8h50m while 46 queued Issues were never claimed).
#
# Resolve the real group at signal time instead, and never group-signal our own
# group: a worker that was not setsid'd into its own group would otherwise take
# the supervisor (or this CLI) down with it. When the group is unknown or is
# ours, fall back to signalling the single pid.
signal_process_tree() {
  local pid=$1 signal=$2 pgid self_pgid
  [[ $pid =~ ^[0-9]+$ ]] || return 0
  pgid=$(process_group_of "$pid") || pgid=''
  self_pgid=$(process_group_of "$$") || self_pgid=''
  if [[ $pgid =~ ^[0-9]+$ && $pgid != "$self_pgid" ]]; then
    kill "-$signal" "-$pgid" 2>/dev/null && return 0
  fi
  kill "-$signal" "$pid" 2>/dev/null || true
}


# Enumerate this repository's actually-live `_worker <issue> <worker-id>`
# processes straight from /proc, independent of workers/*.pid bookkeeping
# (Issue #219): a pidfile is this host's own record of what it spawned, and
# can go missing or point at the wrong pid without the underlying worker
# process itself dying. Matched against the exact PROGRAM_PATH argv[0] (see
# bin/agentic-loop's startup re-exec) so a sibling agentic-loop deployment's
# workers, or an unrelated process that merely mentions "_worker", is never
# counted. Emits "issue<TAB>pid" per live match; best-effort (an unreadable
# /proc/<pid>/cmdline -- permission, already-exited between listing and read
# -- is skipped, never fatal).
live_worker_processes() {
  local pid_dir pid command_line issue
  shopt -s nullglob
  for pid_dir in /proc/[0-9]*; do
    pid=${pid_dir#/proc/}
    [[ -r $pid_dir/cmdline ]] || continue
    command_line=$(tr '\0' ' ' 2>/dev/null < "$pid_dir/cmdline") || continue
    # Substring match, not a prefix match: a script invoked directly (as
    # every caller here does) is exec'd by the kernel's #!/usr/bin/env bash
    # shebang handling, which rewrites argv[0] to the interpreter and shifts
    # PROGRAM_PATH into argv[1] -- so cmdline reads "bash <PROGRAM_PATH>
    # _worker ..." rather than starting with PROGRAM_PATH itself. The
    # repository cwd/common-dir match below supplies the repository identity.
    [[ $command_line == *"$PROGRAM_PATH"' _worker '* ]] || continue
    issue=${command_line#*"$PROGRAM_PATH"' _worker '}
    issue=${issue%% *}
    [[ $issue =~ ^[0-9]+$ ]] || continue
    printf '%s\t%s\n' "$issue" "$pid"
  done
  shopt -u nullglob
}


# Repair workers/<issue>.pid from the live /proc scan when it is missing or
# stale (points at a pid that is no longer alive), so worker_count() and
# every pidfile-driven maintenance pass (graceful shutdown, timeout
# enforcement, orphan reap) see a worker that is genuinely still running even
# when this host's own bookkeeping lost track of it (Issue #219). Never
# invents a pidfile for an issue with no live worker process, and never
# overwrites a pidfile whose recorded pid is still alive (that pid remains
# the authoritative one even if a second live process also matched, which
# should not happen in practice since claim_next serializes one worker per
# issue).
reconcile_worker_pidfiles() {
  local issue pid pidfile existing
  while IFS=$'\t' read -r issue pid; do
    [[ -n $issue && -n $pid ]] || continue
    pidfile="$STATE_ROOT/workers/$issue.pid"
    existing=''
    if [[ -r $pidfile ]]; then read -r existing < "$pidfile" 2>/dev/null || true; fi
    if [[ $existing =~ ^[0-9]+$ ]] && kill -0 "$existing" 2>/dev/null; then continue; fi
    mkdir -p "$STATE_ROOT/workers"
    printf '%s\n' "$pid" > "$pidfile"
  done < <(live_worker_processes)
}


# Poll-scoped memo of open agent:running Issue numbers, shared by
# recover_expired and reap_orphan_workers so a poll issues at most one GitHub
# list call for this set (or none when refresh_supervisor_snapshot's snapshot
# already has it). Cleared alongside the snapshot by clear_supervisor_snapshot
# (see project.sh) so a later poll re-fetches instead of reusing a stale list.
# Returns failure (no output) only when a fresh fetch was required and failed
# -- callers must not treat that the same as a genuinely empty running set.
RUNNING_ISSUE_NUMBERS_STATE=0
RUNNING_ISSUE_NUMBERS_VALUE=''

running_issue_numbers() {
  if (( RUNNING_ISSUE_NUMBERS_STATE == 0 )); then
    if [[ -n $SUPERVISOR_SNAPSHOT && -r $SUPERVISOR_SNAPSHOT ]]; then
      RUNNING_ISSUE_NUMBERS_VALUE=$(snapshot_state_rows running | cut -f1)
      RUNNING_ISSUE_NUMBERS_STATE=1
    elif RUNNING_ISSUE_NUMBERS_VALUE=$(
      # workload-unbounded: one aggregate agent:running list call per poll,
      # only when no snapshot is available yet (startup, or a prior refresh
      # failure); bound=1 call per poll, shared by every caller via this memo.
      repo_api issues --method GET -f state=open -f labels="$(state_label running)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | .number' 2>/dev/null
    ); then
      RUNNING_ISSUE_NUMBERS_STATE=1
    else
      RUNNING_ISSUE_NUMBERS_STATE=2
      RUNNING_ISSUE_NUMBERS_VALUE=''
    fi
  fi
  (( RUNNING_ISSUE_NUMBERS_STATE == 1 )) || return 1
  [[ -n $RUNNING_ISSUE_NUMBERS_VALUE ]] && printf '%s\n' "$RUNNING_ISSUE_NUMBERS_VALUE"
  return 0
}

clear_running_issue_numbers_cache() {
  RUNNING_ISSUE_NUMBERS_STATE=0
  RUNNING_ISSUE_NUMBERS_VALUE=''
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
      # workload-unbounded: one per-Issue lease lookup per running Issue lacking a local pidfile, no batched multi-Issue endpoint exists; bound=running Issue count
      if ! body=$(latest_lease_body "$issue" "$since"); then
        # An unreadable lease must be honored conservatively; retry next poll.
        continue
      fi
      expires=$(printf '%s\n' "$body" | sed -n 's/.*expires=\([0-9][0-9]*\).*/\1/p' | head -n 1)
      [[ -n $expires && $expires -ge $now ]] && continue
    fi
    scope_cache_clear "$issue"
    clear_conflict_wait "$issue"
    # A crash/lease-expiry that coincides with an active pool-exhaustion marker
    # is very likely the provider stalling/erroring under its own rate/usage
    # limit, not this Issue's task looping -- charging its retry budget for
    # that would cascade exhaustion into agent:parked the same way an
    # undetected exhausted-result misclassification would (Issue #158 root
    # cause: "crash / timeout は枯渇保護を通らない"). Clear the attempt
    # unconditionally in that case, the same way an in-worker exhaustion
    # detection already does (see worker.sh's `elif (( exhausted ))` branch).
    if agent_any_pool_marker_active; then
      clear_attempts "$issue"
      set_issue_state "$issue" queued
      comment_issue "$issue" '<!-- agentic-loop:recovered pool-exhaustion=1 -->\n担当workerの終了、またはリース期限切れを検出しました。provider poolが枯渇中のため環境要因の可能性が高いと判断し、試行回数を消費せずIssueを安全にキューへ戻しました。'
    else
      # A worker that keeps dying before finishing (lease expiry / crash, never
      # an explicit AGENTIC_LOOP_RESULT=failed) would otherwise be requeued
      # forever: claim_next records an attempt per claim, but only
      # retry_failed's MAX_ATTEMPTS bound consults it, and that path only sees
      # agent:failed. Once the recorded attempts reach the limit, escalate to
      # agent:failed so retry_failed closes it as unresolvable instead of
      # spinning the queue.
      count=$(attempt_count "$issue")
      if (( count >= MAX_ATTEMPTS )); then
        set_issue_state "$issue" failed
        comment_issue "$issue" "<!-- agentic-loop:recover-exhausted attempts=$count -->\n担当workerが完了前に繰り返し停止したため（試行 $count/$MAX_ATTEMPTS）、キューへ戻さず \`agent:failed\` へ移します。以降はcloseせず \`agent:parked\`（人間トリアージ待ち）へ移します。再開が必要なら内容を確認のうえ \`bin/agentic-loop resume $issue\` を実行してください。"
      else
        set_issue_state "$issue" queued
        comment_issue "$issue" '<!-- agentic-loop:recovered -->\n担当workerの終了、またはリース期限切れを検出したため、Issueを安全にキューへ戻しました。'
      fi
    fi
    event_append "$issue" recover -
  done < <(running_issue_numbers || true)
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
    worker_pid_owned "$issue" || continue
    elapsed=$(worker_elapsed_seconds "$issue") || continue
    (( elapsed >= WORKER_TIMEOUT_SECONDS )) || continue
    read -r pid < "$pidfile" || continue
    [[ $pid =~ ^[0-9]+$ ]] || continue
    signal_process_tree "$pid" TERM
    waited=0
    while (( waited < 5 )) && kill -0 "$pid" 2>/dev/null; do sleep 1; waited=$((waited + 1)); done
    kill -0 "$pid" 2>/dev/null && signal_process_tree "$pid" KILL || true
    lease_release "$issue" timeout
    clear_worker_local "$issue"
    scope_cache_clear "$issue"
    clear_conflict_wait "$issue"
    # A hang that coincides with an active pool-exhaustion marker is very
    # likely the provider stalling under its own rate/usage limit -- e.g. a
    # blocked long-poll that never returns to hit the ordinary post-hoc
    # exhaustion classification -- not this Issue's task looping. Charging its
    # retry budget for that would cascade exhaustion into agent:parked the
    # same way an undetected exhausted-result misclassification would (Issue
    # #158 root cause: "crash / timeout は枯渇保護を通らない"). Requeue
    # directly without burning attempts, the same way an in-worker exhaustion
    # detection already does (see worker.sh's `elif (( exhausted ))` branch).
    if agent_any_pool_marker_active; then
      clear_attempts "$issue"
      comment_issue "$issue" "<!-- agentic-loop:worker-timeout issue=$issue elapsed=${elapsed}s limit=${WORKER_TIMEOUT_SECONDS}s pool-exhaustion=1 -->\nこのIssueのworkerはlease heartbeatが有効なまま実行時間上限（${WORKER_TIMEOUT_SECONDS}秒）を超えたため、ハングしたと判断してプロセスグループごと停止しました（経過 ${elapsed}秒）。provider poolが枯渇中のため環境要因の可能性が高いと判断し、試行回数を消費せずキューへ戻します。誤検知が疑われる場合は \`.agentic-loop.toml\` の \`queue.worker_timeout_seconds\` を見直してください。" || true
      set_issue_state "$issue" queued || true
      project_sync_state "$issue" queued || true
    else
      comment_issue "$issue" "<!-- agentic-loop:worker-timeout issue=$issue elapsed=${elapsed}s limit=${WORKER_TIMEOUT_SECONDS}s -->\nこのIssueのworkerはlease heartbeatが有効なまま実行時間上限（${WORKER_TIMEOUT_SECONDS}秒）を超えたため、ハングしたと判断してプロセスグループごと停止しました（経過 ${elapsed}秒）。自動的な再試行キューへ戻します。誤検知が疑われる場合は \`.agentic-loop.toml\` の \`queue.worker_timeout_seconds\` を見直してください。" || true
      set_issue_state "$issue" failed || true
      project_sync_state "$issue" failed || true
    fi
    event_append "$issue" timeout -
  done
  shopt -u nullglob
}


# A worker-orphan (Issue #193) is this host's live pidfile whose Issue GitHub
# no longer reports as agent:running (e.g. it fell back to agent:queued/failed
# while the local provider CLI process kept running). Unlike
# enforce_worker_timeout, GitHub's Label -- not elapsed runtime -- is the
# signal, so this can reap an orphan long before WORKER_TIMEOUT_SECONDS (up to
# 4h by default) would. A worker's own normal teardown (comment/Label update,
# then process exit) makes it look orphaned for a brief window too, so a
# single observation never kills: the first poll that sees an issue as
# orphaned only records worker_orphan_since_file's epoch, and only a
# WORKER_ORPHAN_GRACE_SECONDS-persistent observation across later polls
# actually kills it (mirrors enforce_worker_timeout's process-group TERM then
# KILL so no provider CLI child is left orphaned). A worker_critical section
# (a completion/close write sequence the worker itself marked unsafe to
# interrupt) or a pending stop-request (control.sh's cooperative pause/abort
# drain) skip the kill for as long as they are active, without clearing the
# marker, so the same brief teardown/drain window is protected even when it
# outlasts the grace period; enforce_worker_timeout remains the backstop for a
# guard that never clears. A marker older than the current worker's own
# .started epoch is treated as a stale leftover from a prior worker on this
# Issue and reset rather than honored, so grace is never skipped for a worker
# that never had a chance to be observed. Only pidfile-owning workers are
# touched, so a multi-host deployment never interferes with another host's
# Issue. The running-Issue set (running_issue_numbers, shared with
# recover_expired) must come from a successful read (the snapshot this poll
# already fetched, or a fresh GitHub call): a failed read must never be
# treated as "nothing is running", which would misclassify every genuinely
# running worker as an orphan.
reap_orphan_workers() {
  (( WORKER_ORPHAN_GRACE_SECONDS > 0 )) || return 0
  local pidfile issue now elapsed since started since_file pid waited running_list
  shopt -s nullglob
  local -a pidfiles=("$STATE_ROOT"/workers/*.pid)
  shopt -u nullglob
  (( ${#pidfiles[@]} > 0 )) || return 0
  now=$(date +%s)
  running_list=$(running_issue_numbers) || return 0
  for pidfile in "${pidfiles[@]}"; do
    issue=$(basename "$pidfile" .pid)
    [[ $issue =~ ^[0-9]+$ ]] || continue
    if ! worker_pid_owned "$issue"; then
      rm -f "$(worker_orphan_since_file "$issue")"
      continue
    fi
    if grep -Fxq "$issue" <<< "$running_list"; then
      rm -f "$(worker_orphan_since_file "$issue")"
      continue
    fi
    since_file=$(worker_orphan_since_file "$issue")
    since=''
    [[ -r $since_file ]] && read -r since < "$since_file"
    # A marker older than this worker's own .started epoch is a stale
    # leftover from a prior worker on this Issue (crash before
    # clear_worker_local ran) rather than a live observation of the current
    # one; treat it as absent so grace restarts instead of firing instantly.
    started=''
    [[ -r $(worker_started_file "$issue") ]] && read -r started < "$(worker_started_file "$issue")"
    if [[ ! $since =~ ^[0-9]+$ ]] || { [[ $started =~ ^[0-9]+$ ]] && (( since < started )); }; then
      mkdir -p "$STATE_ROOT/workers"
      printf '%s\n' "$now" > "$since_file"
      continue
    fi
    elapsed=$((now - since))
    (( elapsed >= WORKER_ORPHAN_GRACE_SECONDS )) || continue
    # A worker's own normal teardown (critical section: Label/close update
    # then process exit) or a cooperative pause/abort drain (control.sh's
    # control_drain_local_worker, which itself waits out a critical section
    # first) both make a healthy worker look orphaned for a while. Reaping
    # mid-teardown would double-act on an already-settled Issue; reaping
    # mid-drain would kill the checkpoint pause/resume depends on. Skip this
    # poll without clearing the marker -- the guard is re-checked every poll,
    # and enforce_worker_timeout remains the final backstop if a guard never
    # clears.
    if worker_critical_active "$issue" || worker_stop_requested "$issue"; then
      continue
    fi
    read -r pid < "$pidfile" || continue
    [[ $pid =~ ^[0-9]+$ ]] || continue
    signal_process_tree "$pid" TERM
    waited=0
    while (( waited < 5 )) && kill -0 "$pid" 2>/dev/null; do sleep 1; waited=$((waited + 1)); done
    kill -0 "$pid" 2>/dev/null && signal_process_tree "$pid" KILL || true
    lease_release "$issue" orphan
    clear_worker_local "$issue"
    scope_cache_clear "$issue"
    clear_conflict_wait "$issue"
    comment_issue "$issue" "<!-- agentic-loop:worker-orphan-reaped issue=$issue elapsed=${elapsed}s grace=${WORKER_ORPHAN_GRACE_SECONDS}s -->\nこのホストのlocal workerがGitHub上 \`agent:running\` ではない状態を ${elapsed}秒（grace ${WORKER_ORPHAN_GRACE_SECONDS}秒）観測したため、実行時間上限を待たずprocess groupごと安全に停止し、local stateを掃除しました。Issueの状態は変更していません。誤検知が疑われる場合は \`.agentic-loop.toml\` の \`queue.worker_orphan_grace_seconds\` を見直してください。" || true
    event_append "$issue" orphan -
  done
}


# --- Residual worktree/branch auto-prune (Issue #211) ---
# A worker's own completion path already removes its own worktree/branch
# (cleanup_completed_worker / cleanup_completed_branch_only in worker.sh).
# This is the supervisor-side backstop for everything that path never
# reaches: a crashed/killed worker, a worker that stopped tracking its own
# Issue, or a worktree/branch left over from an Issue this host no longer has
# a local worker for (including after a restart or on a re-attached host).
# Multi-host safe: only this host's local worktree/branch is ever touched
# (a remote branch is never deleted here), and only for an Issue with no live
# local worker on this host (worker_pid_live). Every decision is derived from
# a fresh Git/GitHub observation, never from a worker's self-report:
#   - Issue closed: discard the local worktree + branch outright, even with
#     unpushed commits (the work is done or abandoned; see docs/decisions/0016
#     -- retry-budget exhaustion already parks rather than closes, so a
#     closed Issue here genuinely means completed/disposed).
#   - Issue open and the local branch has no commit that is not already on
#     `origin/agent/issue-N` (and that remote branch exists): discard the
#     local worktree + branch only, leaving the remote branch for another
#     host or a future resume (Issue #210's remote-agent-branch resume).
#   - Anything else (open with unpushed/unverifiable local state, no matching
#     remote branch, or a GitHub read failure): leave it untouched and retry
#     on a later poll. No comment, no anomaly -- an operator cannot act on
#     unpushed local work they cannot see anyway.
prune_target_issues() {
  git -C "$REPO_ROOT" for-each-ref --format='%(refname:short)' refs/heads/agent/ 2>/dev/null |
    sed -E 's#^agent/issue-##' | grep -E '^[0-9]+$' | sort -un
}


# Remove worktree_root (if present) and the local branch ref together, only
# after re-verifying the worktree is actually registered to this exact branch
# (mirrors cleanup_completed_worker's safety check in worker.sh) so a foreign/
# unexpected worktree at the same path is never touched. force=1 discards
# uncommitted/unpushed local state (the closed-Issue case); force=0 aborts the
# whole removal if the worktree is dirty, so uncommitted work is never
# silently lost even when the branch itself is fully pushed.
prune_worktree_and_branch() {
  local worktree_root=$1 branch=$2 force=$3 registered_branch local_oid
  local branch_ref="refs/heads/$branch"
  local_oid=$(git -C "$REPO_ROOT" rev-parse --verify -q "$branch_ref" 2>/dev/null) || return 1
  if [[ -e $worktree_root ]]; then
    [[ ! -L $worktree_root ]] || return 1
    # Must not `exit` on first match (Issue #218, see worker.sh's
    # resume_probe/cleanup_completed_worker for the full explanation): an
    # early exit here can SIGPIPE-kill `git worktree list` while it is still
    # writing, and under this script's `set -o pipefail` that silently kills
    # the whole calling process with exit 141.
    registered_branch=$(git -C "$REPO_ROOT" worktree list --porcelain 2>/dev/null | awk -v path="$worktree_root" '
      $1 == "worktree" {matched=($2 == path)}
      matched && $1 == "branch" && !found {print $2; found=1}
    ')
    [[ $registered_branch == "$branch_ref" ]] || return 1
    if [[ $force == 1 ]]; then
      git -C "$REPO_ROOT" worktree remove --force "$worktree_root" 2>/dev/null || return 1
    else
      [[ -z $(git -C "$worktree_root" status --porcelain 2>/dev/null) ]] || return 1
      git -C "$REPO_ROOT" worktree remove "$worktree_root" 2>/dev/null || return 1
    fi
  fi
  [[ ! -e $worktree_root ]] || return 1
  git -C "$REPO_ROOT" update-ref -d "$branch_ref" "$local_oid" 2>/dev/null || return 1
  ! git -C "$REPO_ROOT" show-ref --verify --quiet "$branch_ref"
}


prune_residual_worktrees() {
  local issue branch worktree_root current state ahead
  while IFS= read -r issue; do
    [[ -n $issue ]] || continue
    worker_pid_owned "$issue" && continue
    branch="agent/issue-$issue"
    worktree_root="$WORKTREE_ROOT/issue-$issue"
    # workload-unbounded: one per-Issue state lookup per residual worktree/
    # branch candidate lacking a live local worker; bound=residual worktree/
    # branch count on this host
    current=$(repo_api "issues/$issue" --jq '[.state, ([.labels[].name] | join(","))] | @tsv' 2>/dev/null) || continue
    state=${current%%$'\t'*}
    if [[ $state == closed ]]; then
      prune_worktree_and_branch "$worktree_root" "$branch" 1 || continue
      comment_issue "$issue" "<!-- agentic-loop:pruned reason=closed -->\nこのIssueはcloseされていますが、このホストに残存する専用worktreeとlocal branch \`$branch\`（未pushの変更を含む可能性があります）を検出したため、保守処理として削除しました。" || true
      event_append "$issue" prune -
    elif [[ $state == open ]]; then
      git -C "$REPO_ROOT" rev-parse --verify -q "refs/remotes/origin/$branch" >/dev/null 2>&1 || continue
      ahead=$(git -C "$REPO_ROOT" rev-list --count "refs/remotes/origin/$branch..refs/heads/$branch" 2>/dev/null) || continue
      [[ $ahead =~ ^[0-9]+$ ]] || continue
      (( ahead == 0 )) || continue
      prune_worktree_and_branch "$worktree_root" "$branch" 0 || continue
      comment_issue "$issue" "<!-- agentic-loop:pruned reason=pushed -->\nこのホストに残存する専用worktreeとlocal branch \`$branch\` は \`origin/$branch\` へ変更が全て反映済みであることを確認したため、local分のみ保守処理として削除しました。remote branchは再開のためこのホストからは削除していません。" || true
      event_append "$issue" prune -
    fi
  done < <(prune_target_issues)
}


requeue_answered() {
  local issue kind saw_marker answered
  while IFS= read -r issue; do
    [[ -n $issue ]] || continue
    # `gh api --paginate --jq` runs the jq program once per page (verified
    # against the real API), so a whole-array `last`/slice jq program cannot
    # see across a page boundary. Classify each comment individually instead
    # and fold the per-page-but-in-order output into "was there a reply after
    # the last needs-input marker" in the shell, which stays correct no
    # matter how many pages the comment history spans.
    saw_marker=false answered=false
    while IFS= read -r kind; do
      case $kind in
        MARKER) saw_marker=true; answered=false ;;
        REPLY) [[ $saw_marker == true ]] && answered=true ;;
      esac
    # workload-unbounded: walks every comment on this Issue every poll while it stays needs-input, growth proportional to its comment count; bound=comment count on this Issue; track=#192
    done < <(repo_api "issues/$issue/comments" --method GET -f per_page=100 --paginate --jq '.[] | if (.body | contains("agentic-loop:needs-input")) then "MARKER" elif (.body | contains("<!-- agentic-loop:") | not) then "REPLY" else "OTHER" end' 2>/dev/null || true)
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
    repo_api "issues/$issue" --method PATCH -f state=closed -f state_reason=not_planned >/dev/null
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

# Move a retry-budget-exhausted Issue to a non-claim, open "human triage"
# state instead of closing it (see docs/decisions/0016): a worker using up its
# retry budget means the model/attempt count was insufficient, not that the
# requirement is invalid. If the Label PUT fails, this returns failure and the
# Issue is left in agent:failed for the next poll to retry -- it is never
# closed as a side effect of a failed park attempt (fail-safe).
park_issue() {
  local issue=$1 count=$2 reason=$3
  set_issue_state "$issue" parked || return 1
  project_sync_state "$issue" parked || true
  comment_issue "$issue" "<!-- agentic-loop:parked attempts=$count reason=$reason -->\n$count 回試行しても完了できなかったため、closeせず \`agent:parked\`（人間トリアージ待ち）へ移しました。要求は無効になったわけではありません。内容を確認し、要求の精緻化・より小さな単位への分解のうえ \`bin/agentic-loop resume $issue\` で再投入するか、不要であれば \`bin/agentic-loop dispose $issue --reason cancelled\` 等で終了してください。" || true
  clear_attempts "$issue"
  scope_cache_clear "$issue"
  clear_conflict_wait "$issue"
  clear_worker_local "$issue"
  event_append "$issue" recover -
  postmortem_consider_trigger repeated-failure "$issue" "Issue #$issue が $count 回の試行後もagent:parkedへ移った（reason=$reason）" || true
}

# Re-queue failures this loop already attempted, so a transient failure (e.g. an
# exhausted token budget or a killed session) is retried instead of parked.
# Only Issues tracked by claim are managed; those that reach MAX_ATTEMPTS stay
# agent:failed for human review, and untracked/external failures are left alone.
# Automatically manage every failed Issue: retry it (bounded by MAX_ATTEMPTS with
# a cooldown) so a transient failure recovers without human action, and once the
# attempts are exhausted, park it instead of closing -- retry-budget exhaustion
# is never treated as proof the requirement is unresolvable (see docs/decisions/
# 0016). Pre-existing/untracked failures (attempt count 0) are retried too.
retry_failed() {
  local issue count
  while IFS= read -r issue; do
    [[ $issue =~ ^[0-9]+$ ]] || continue
    count=$(attempt_count "$issue")
    if (( count >= MAX_ATTEMPTS )); then
      park_issue "$issue" "$count" retry-exhausted
      continue
    fi
    attempt_cooldown_elapsed "$issue" || continue
    set_issue_state "$issue" queued || continue
    comment_issue "$issue" "<!-- agentic-loop:retry attempt=$count -->\n一時的な失敗の可能性があるため自動的に再試行キューへ戻します（試行 $count/$MAX_ATTEMPTS）。上限に達したらcloseせず \`agent:parked\`（人間トリアージ待ち）へ移します。" || true
  done < <(snapshot_state_rows failed | cut -f1 || repo_api issues --method GET -f state=open -f labels="$(state_label failed)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | .number' 2>/dev/null)
}
