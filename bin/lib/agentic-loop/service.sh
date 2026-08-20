# Module: service.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2155



pid_alive() {
  [[ -r $STATE_ROOT/supervisor.pid ]] || return 1
  local pid command_line
  read -r pid < "$STATE_ROOT/supervisor.pid"
  [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null || return 1
  [[ -r /proc/$pid/cmdline ]] || return 1
  command_line=$(tr '\0' ' ' < "/proc/$pid/cmdline") || return 1
  process_repo_matches "$pid" || return 1
  [[ $command_line == *' _service'* || $command_line == *' _supervise'* ]]
}


# Persist only bounded, secret-safe supervisor lifecycle evidence.  The context
# intentionally survives SIGKILL and host/session teardown; a later start can
# then turn the unclosed run into an explicit `abrupt` termination record.
supervisor_stage_valid() {
  case $1 in startup | maintenance | poll | claim | sleep | shutdown) return 0 ;; *) return 1 ;; esac
}


supervisor_boot_id() {
  local id=unknown
  [[ -r /proc/sys/kernel/random/boot_id ]] && read -r id < /proc/sys/kernel/random/boot_id
  [[ $id =~ ^[0-9a-f-]{36}$ ]] || id=unknown
  printf '%s\n' "$id"
}


supervisor_proc_start_ticks() {
  local pid=${1:-$$} rest ticks
  [[ -r /proc/$pid/stat ]] || { printf '0\n'; return 0; }
  rest=$(sed 's/^[0-9][0-9]* ([^)]*) //' "/proc/$pid/stat" 2>/dev/null) || rest=''
  ticks=$(awk '{ print $20 }' <<< "$rest")
  [[ $ticks =~ ^[0-9]+$ ]] && printf '%s\n' "$ticks" || printf '0\n'
}


supervisor_context_write() {
  local stage=$1 now tmp started=${SUPERVISOR_STARTED_AT:-$(date +%s)}
  supervisor_stage_valid "$stage" || return 0
  now=$(date +%s); mkdir -p "$STATE_ROOT"
  tmp="$SUPERVISOR_CONTEXT_FILE.tmp.$$"
  printf '1\t%s\t%s\t%s\t%s\t%s\t%s\n' "$$" "$started" "$now" "$stage" \
    "$(supervisor_boot_id)" "$(supervisor_proc_start_ticks $$)" > "$tmp" 2>/dev/null || return 0
  mv -f "$tmp" "$SUPERVISOR_CONTEXT_FILE" 2>/dev/null || true
}


supervisor_context_read() {
  SUPERVISOR_CTX_PID='' SUPERVISOR_CTX_STARTED='' SUPERVISOR_CTX_SEEN=''
  SUPERVISOR_CTX_STAGE='' SUPERVISOR_CTX_BOOT='' SUPERVISOR_CTX_TICKS=''
  [[ -r $SUPERVISOR_CONTEXT_FILE ]] || return 1
  local schema
  IFS=$'\t' read -r schema SUPERVISOR_CTX_PID SUPERVISOR_CTX_STARTED SUPERVISOR_CTX_SEEN \
    SUPERVISOR_CTX_STAGE SUPERVISOR_CTX_BOOT SUPERVISOR_CTX_TICKS < "$SUPERVISOR_CONTEXT_FILE" || return 1
  [[ $schema == 1 && $SUPERVISOR_CTX_PID =~ ^[0-9]+$ && $SUPERVISOR_CTX_STARTED =~ ^[0-9]+$ && \
     $SUPERVISOR_CTX_SEEN =~ ^[0-9]+$ && $SUPERVISOR_CTX_TICKS =~ ^[0-9]+$ ]] || return 1
  supervisor_stage_valid "$SUPERVISOR_CTX_STAGE" || return 1
}


supervisor_termination_append() {
  local kind=$1 detail=$2 pid=$3 started=$4 seen=$5 stage=$6 boot=$7 ticks=$8 now tmp
  case $kind in exit | signal | abrupt) ;; *) return 0 ;; esac
  [[ $detail =~ ^(0|[1-9][0-9]{0,2}|TERM|INT|unknown)$ ]] || detail=unknown
  [[ $pid =~ ^[0-9]+$ && $started =~ ^[0-9]+$ && $seen =~ ^[0-9]+$ && $ticks =~ ^[0-9]+$ ]] || return 0
  supervisor_stage_valid "$stage" || return 0
  [[ $boot =~ ^[0-9a-f-]{36}$ ]] || boot=unknown
  now=$(date +%s); mkdir -p "$STATE_ROOT"
  printf '1\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$now" "$kind" "$detail" "$pid" \
    "$started" "$seen" "$stage" "$boot" "$ticks" > "$SUPERVISOR_LAST_EXIT_FILE" 2>/dev/null || true
  printf '1\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$now" "$kind" "$detail" "$pid" \
    "$started" "$seen" "$stage" "$boot" "$ticks" >> "$SUPERVISOR_TERMINATIONS_LOG" 2>/dev/null || return 0
  if (( $(wc -l < "$SUPERVISOR_TERMINATIONS_LOG" 2>/dev/null || printf '0') > SUPERVISOR_TERMINATIONS_MAX_LINES )); then
    tmp="$SUPERVISOR_TERMINATIONS_LOG.tmp.$$"
    tail -n "$SUPERVISOR_TERMINATIONS_MAX_LINES" "$SUPERVISOR_TERMINATIONS_LOG" > "$tmp" 2>/dev/null && \
      mv -f "$tmp" "$SUPERVISOR_TERMINATIONS_LOG"
  fi
}


supervisor_last_exit_read() {
  SUPERVISOR_EXIT_AT='' SUPERVISOR_EXIT_KIND='' SUPERVISOR_EXIT_DETAIL=''
  SUPERVISOR_EXIT_PID='' SUPERVISOR_EXIT_STARTED='' SUPERVISOR_EXIT_SEEN=''
  SUPERVISOR_EXIT_STAGE='' SUPERVISOR_EXIT_BOOT='' SUPERVISOR_EXIT_TICKS=''
  [[ -r $SUPERVISOR_LAST_EXIT_FILE ]] || return 1
  local schema
  IFS=$'\t' read -r schema SUPERVISOR_EXIT_AT SUPERVISOR_EXIT_KIND SUPERVISOR_EXIT_DETAIL \
    SUPERVISOR_EXIT_PID SUPERVISOR_EXIT_STARTED SUPERVISOR_EXIT_SEEN SUPERVISOR_EXIT_STAGE \
    SUPERVISOR_EXIT_BOOT SUPERVISOR_EXIT_TICKS < "$SUPERVISOR_LAST_EXIT_FILE" || return 1
  [[ $schema == 1 && $SUPERVISOR_EXIT_AT =~ ^[0-9]+$ && $SUPERVISOR_EXIT_PID =~ ^[0-9]+$ && \
     $SUPERVISOR_EXIT_STARTED =~ ^[0-9]+$ && $SUPERVISOR_EXIT_SEEN =~ ^[0-9]+$ && \
     $SUPERVISOR_EXIT_TICKS =~ ^[0-9]+$ ]] || return 1
  case $SUPERVISOR_EXIT_KIND in exit | signal | abrupt) ;; *) return 1 ;; esac
  [[ $SUPERVISOR_EXIT_DETAIL =~ ^(0|[1-9][0-9]{0,2}|TERM|INT|unknown)$ ]] || return 1
  [[ $SUPERVISOR_EXIT_BOOT == unknown || $SUPERVISOR_EXIT_BOOT =~ ^[0-9a-f-]{36}$ ]] || return 1
  supervisor_stage_valid "$SUPERVISOR_EXIT_STAGE" || return 1
}


supervisor_record_stale_context() {
  supervisor_context_read || return 0
  if kill -0 "$SUPERVISOR_CTX_PID" 2>/dev/null && \
     [[ $(supervisor_boot_id) == "$SUPERVISOR_CTX_BOOT" && \
        $(supervisor_proc_start_ticks "$SUPERVISOR_CTX_PID") == "$SUPERVISOR_CTX_TICKS" ]]; then
    return 0
  fi
  supervisor_termination_append abrupt unknown "$SUPERVISOR_CTX_PID" "$SUPERVISOR_CTX_STARTED" \
    "$SUPERVISOR_CTX_SEEN" "$SUPERVISOR_CTX_STAGE" "$SUPERVISOR_CTX_BOOT" "$SUPERVISOR_CTX_TICKS"
  rm -f "$SUPERVISOR_CONTEXT_FILE"
}


clear_stale_lock() {
  [[ -d $STATE_ROOT/supervisor.lock ]] || return 0
  pid_alive && return 1
  # No live supervisor pid: the lock is stale or still mid-startup. A lock that
  # has not yet published a pid may belong to a supervisor that just acquired it
  # and is about to write its pid; leave such a fresh lock alone until a grace
  # period passes, so two concurrent starts do not tear down each other's lock
  # and both begin supervising. A lock whose pid is published but not a live
  # supervisor (crashed or PID reused) is reclaimed immediately.
  if [[ ! -e $STATE_ROOT/supervisor.pid ]]; then
    local now mtime
    now=$(date +%s)
    mtime=$(stat -c %Y "$STATE_ROOT/supervisor.lock" 2>/dev/null || printf '%s' "$now")
    (( now - mtime < SUPERVISOR_LOCK_GRACE_SECONDS )) && return 1
  fi
  supervisor_record_stale_context
  rmdir "$STATE_ROOT/supervisor.lock" 2>/dev/null || return 1
  rm -f "$STATE_ROOT/supervisor.pid"
}


supervisor_unit_name() {
  printf 'agentic-loop-supervisor-%s.service\n' "$(systemd-escape --path "$REPO_ROOT")"
}


systemd_path_value() {
  local value=$1
  [[ $value != *$'\n'* && $value != *$'\r'* ]] || return 1
  value=${value//\\/\\x5c}; value=${value// /\\x20}; value=${value//$'\t'/\\x09}; value=${value//\"/\\x22}
  printf '%s\n' "$value"
}


install_supervisor_unit() {
  local unit_dir unit_file escaped_root escaped_program escaped_devbox service_path escaped_path provider provider_dirs=''
  unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  unit_file="$unit_dir/$(supervisor_unit_name)"
  escaped_root=$(systemd_path_value "$REPO_ROOT") || fail 'repository path cannot be encoded safely for systemd'
  escaped_program=$(systemd_path_value "$SCRIPT_ROOT") || fail 'program path cannot be encoded safely for systemd'
  escaped_devbox=$(systemd_path_value "$(command -v devbox)") || fail 'devbox path cannot be encoded safely for systemd'
  for provider in $(agent_used_providers); do provider_dirs+="$(dirname "$(command -v "$(provider_command "$provider")")"):"; done
  service_path="${provider_dirs}$(dirname "$(command -v gh)"):$(dirname "$(command -v git)"):$(dirname "$(command -v yq)"):/usr/local/bin:/usr/bin:/bin"
  escaped_path=$(systemd_path_value "$service_path") || fail 'command PATH cannot be encoded safely for systemd'
  mkdir -p "$unit_dir"
  {
    printf '%s\n' '[Unit]' 'Description=Agentic Loop repository supervisor' 'After=network-online.target'
    printf '\n%s\n' '[Service]' 'Type=simple'
    printf 'WorkingDirectory=%s\n' "$escaped_root"
    # Resolve pinned tools (notably yq) at service start instead of baking a
    # transient Nix-store profile path into the unit. This survives Devbox
    # garbage collection and host reboot while preserving provider/gh PATH.
    printf 'ExecStart=%s run -- %s/bin/agentic-loop _service\n' "$escaped_devbox" "$escaped_program"
    printf 'Environment=PATH=%s\n' "$escaped_path"
    printf '%s\n' 'Restart=on-failure' 'RestartSec=5s'
    printf '\n%s\n' '[Install]' 'WantedBy=default.target'
  } > "$unit_file"
  if command -v systemd-analyze >/dev/null 2>&1; then systemd-analyze --user verify "$unit_file"; fi
  systemctl --user daemon-reload
  systemctl --user enable "$(supervisor_unit_name)" >/dev/null
}


cmd_service() {
  mkdir -p "$STATE_ROOT/logs" "$WORKTREE_ROOT"
  supervisor_record_stale_context
  clear_stale_lock || return 0
  mkdir "$STATE_ROOT/supervisor.lock" 2>/dev/null || return 0
  # Publish identity immediately so a racing starter sees a live supervisor and
  # backs off instead of reclaiming the lock during startup.
  printf '%s\n' "$$" > "$STATE_ROOT/supervisor.pid"
  supervise
}


cmd_start() {
  preflight
  mkdir -p "$STATE_ROOT/logs" "$WORKTREE_ROOT"
  rm -f "$STATE_ROOT/stop.requested"
  if pid_alive; then
    say "Agentic Loop supervisor is already running (pid $(cat "$STATE_ROOT/supervisor.pid"))."
    return 0
  fi
  supervisor_record_stale_context
  clear_stale_lock || fail 'stale supervisor state could not be repaired'
  install_supervisor_unit
  systemctl --user restart "$(supervisor_unit_name)"
  local waited=0
  while ! pid_alive && (( waited < 50 )); do sleep 0.1; waited=$((waited + 1)); done
  if ! pid_alive; then
    # Allows deterministic test doubles and environments whose user manager is
    # temporarily unavailable; the installed unit remains the crash-recovery path.
    nohup "$0" _service >> "$STATE_ROOT/supervisor.log" 2>&1 </dev/null &
    waited=0
    while ! pid_alive && (( waited < 50 )); do sleep 0.1; waited=$((waited + 1)); done
  fi
  pid_alive || fail 'supervisor failed to start'
  say "Agentic Loop supervisor started (pid $(cat "$STATE_ROOT/supervisor.pid"), workers $MAX_WORKERS)."
}


cmd_stop() {
  mkdir -p "$STATE_ROOT"
  if ! pid_alive; then
    clear_stale_lock || true
    say 'Agentic Loop supervisor is not running.'
    return 0
  fi
  : > "$STATE_ROOT/stop.requested"
  local pid waited=0
  read -r pid < "$STATE_ROOT/supervisor.pid"
  while kill -0 "$pid" 2>/dev/null; do
    if (( STOP_TIMEOUT > 0 && waited >= STOP_TIMEOUT )); then
      fail 'supervisor is still draining workers'
    fi
    sleep 1
    waited=$((waited + 1))
  done
  systemctl --user stop "$(supervisor_unit_name)" >/dev/null 2>&1 || true
  say 'Agentic Loop supervisor stopped after draining workers.'
}
