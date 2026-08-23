# Module: config.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034,SC2153



# Emit the merged configuration as dotted "key = value" lines. The committed
# .agentic-loop.toml is overlaid by an optional .agentic-loop.local.toml, where
# individual keys in the local file win. yq is invoked directly so its argument
# is not re-globbed by any wrapper shell.
config_props() {
  if [[ -r $CONFIG_LOCAL ]]; then
    yq -p toml -o props eval-all 'select(fi==0) * select(fi==1)' "$CONFIG_FILE" "$CONFIG_LOCAL"
  else
    yq -p toml -o props "$CONFIG_FILE"
  fi
}


load_config() {
  local line key value props
  [[ -r $CONFIG_FILE ]] || return 0
  command -v yq >/dev/null 2>&1 || { CONFIG_ERROR="yq is required to read .agentic-loop.toml; the pinned runtime that provides it could not be restored. Recovery: run \`devbox run --config $RUNTIME_ROOT -- true\` to rebuild it, or re-run install.sh."; return 1; }
  props=$(config_props 2>/dev/null) || { CONFIG_ERROR='cannot parse .agentic-loop.toml'; return 1; }
  CONFIG_PROPS=$props
  while IFS= read -r line; do
    [[ -z $line ]] && continue
    key=${line%% = *}; value=${line#* = }
    case $key in
      queue.poll_seconds) key=POLL_SECONDS ;;
      queue.poll_max_seconds) key=POLL_MAX_SECONDS ;;
      queue.max_workers) key=MAX_WORKERS ;;
      queue.lease_seconds) key=LEASE_SECONDS ;;
      queue.stop_timeout) key=STOP_TIMEOUT ;;
      queue.pause_grace_seconds) key=PAUSE_GRACE_SECONDS ;;
      queue.stale_days) key=STALE_DAYS ;;
      queue.graphql_reserve) key=GRAPHQL_RESERVE ;;
      queue.core_reserve) key=CORE_RESERVE ;;
      queue.rate_limit_cache_seconds) key=RATE_LIMIT_CACHE_SECONDS ;;
      queue.api_retry_attempts) key=API_RETRY_ATTEMPTS ;;
      queue.api_retry_base_seconds) key=API_RETRY_BASE_SECONDS ;;
      queue.max_attempts) key=MAX_ATTEMPTS ;;
      queue.retry_cooldown_seconds) key=RETRY_COOLDOWN_SECONDS ;;
      queue.worker_timeout_seconds) key=WORKER_TIMEOUT_SECONDS ;;
      queue.worker_orphan_grace_seconds) key=WORKER_ORPHAN_GRACE_SECONDS ;;
      queue.stall_seconds) key=STALL_SECONDS ;;
      queue.provider_stall_seconds) key=PROVIDER_STALL_SECONDS ;;
      queue.unknown_scope) UNKNOWN_SCOPE=$value; continue ;;
      queue.exclusive_paths) EXCLUSIVE_PATHS=$value; continue ;;
      queue.traceability) TRACEABILITY=$value; continue ;;
      queue.preflight) PREFLIGHT=$value; continue ;;
      queue.workload) WORKLOAD=$value; continue ;;
      postmortem.auto_detect) POSTMORTEM_AUTO_DETECT=$value; continue ;;
      postmortem.max_auto_created_per_day) key=POSTMORTEM_MAX_AUTO_CREATED_PER_DAY ;;
      *) continue ;;
    esac
    if [[ ! $value =~ ^[0-9]+$ ]]; then
      CONFIG_ERROR="invalid configuration value for $key: $value"
      return 1
    fi
    printf -v "$key" '%s' "$value"
  done <<< "$props"
}


# --- AI provider abstraction (see docs/policies/ai-tool-neutrality.md) ---
# Provider, model, and reasoning effort are resolved per phase from
# .agentic-loop.toml ([agent], [agent.plan], [agent.exec]); a phase inherits
# [agent].provider when it sets none, and reasoning effort defaults to high for
# plan and low for exec. Environment variables of the same intent still override
# for quick local experiments. Each Issue runs a flagship plan stage (Codex
# read-only) then a cheaper exec stage, so expensive reasoning is spent on
# planning rather than on every edit.
config_value() { awk -F ' = ' -v k="$1" '$1 == k {print $2; exit}' <<< "$CONFIG_PROPS"; }

config_prefix_lines() { grep "^$1" <<< "$CONFIG_PROPS" || true; }

config_has_prefix() { [[ -n $(config_prefix_lines "$1") ]]; }


# --- time constant invariants and calibration (Issue #280, ADR 0032) -------
# var<->key mapping mirrors load_config's case statement above; kept as a
# separate lookup (rather than refactoring load_config to iterate it) so
# _timing-check's fallback-vs-toml comparison can reuse it without changing
# load_config's proven per-line parsing.
readonly -A CONFIG_NUMERIC_KEYS=(
  [POLL_SECONDS]=queue.poll_seconds
  [POLL_MAX_SECONDS]=queue.poll_max_seconds
  [MAX_WORKERS]=queue.max_workers
  [LEASE_SECONDS]=queue.lease_seconds
  [STOP_TIMEOUT]=queue.stop_timeout
  [PAUSE_GRACE_SECONDS]=queue.pause_grace_seconds
  [STALE_DAYS]=queue.stale_days
  [GRAPHQL_RESERVE]=queue.graphql_reserve
  [CORE_RESERVE]=queue.core_reserve
  [RATE_LIMIT_CACHE_SECONDS]=queue.rate_limit_cache_seconds
  [API_RETRY_ATTEMPTS]=queue.api_retry_attempts
  [API_RETRY_BASE_SECONDS]=queue.api_retry_base_seconds
  [MAX_ATTEMPTS]=queue.max_attempts
  [RETRY_COOLDOWN_SECONDS]=queue.retry_cooldown_seconds
  [WORKER_TIMEOUT_SECONDS]=queue.worker_timeout_seconds
  [WORKER_ORPHAN_GRACE_SECONDS]=queue.worker_orphan_grace_seconds
  [STALL_SECONDS]=queue.stall_seconds
  [PROVIDER_STALL_SECONDS]=queue.provider_stall_seconds
)


# Pure function: given the time-related config values (already resolved --
# either the live merged config, or a --committed-only reparse), print one
# name<TAB>detail<TAB>remedy row per violated invariant. No I/O, no GitHub
# calls, no mutation; a zero-second value keeps meaning "band disabled" for
# stall/orphan-grace/pause-grace/worker-timeout, matching validate_config.
timing_invariant_violations() {
  local stall=$1 provider_stall=$2 worker_timeout=$3 lease=$4 pause_grace=$5 poll=$6 poll_max=$7 orphan_grace=$8
  local hb=$(( lease / 3 + 1 ))

  if (( stall > 0 && worker_timeout > 0 && stall >= worker_timeout )); then
    printf 'stall-before-timeout\tstall_seconds(%s) が worker_timeout_seconds(%s) 以上で、stall観測が自動停止と同時か後に発火します。\tqueue.stall_seconds を queue.worker_timeout_seconds より小さくしてください。\n' "$stall" "$worker_timeout"
  fi
  if (( provider_stall > 0 && worker_timeout > 0 && provider_stall >= worker_timeout )); then
    printf 'provider-stall-before-timeout\tprovider_stall_seconds(%s) が worker_timeout_seconds(%s) 以上で、provider stageのstall観測が自動停止と同時か後に発火します。\tqueue.provider_stall_seconds を queue.worker_timeout_seconds より小さくしてください。\n' "$provider_stall" "$worker_timeout"
  fi
  if (( provider_stall > 0 && stall > 0 && provider_stall < stall )); then
    printf 'stall-band-order\tprovider_stall_seconds(%s) が stall_seconds(%s) より小さく、帯が逆転しています。\tqueue.provider_stall_seconds を queue.stall_seconds 以上にしてください。\n' "$provider_stall" "$stall"
  fi
  if (( worker_timeout > 0 && worker_timeout < WORKER_TIMEOUT_MIN_SAFE_SECONDS )); then
    printf 'worker-timeout-too-low\tworker_timeout_seconds(%s) が安全下限(%s秒)未満で、正常に進行中のworkerを誤って停止する恐れがあります。\tqueue.worker_timeout_seconds を%s秒以上、または実測の所要時間に基づく値へ見直してください。\n' "$worker_timeout" "$WORKER_TIMEOUT_MIN_SAFE_SECONDS" "$WORKER_TIMEOUT_MIN_SAFE_SECONDS"
  fi
  if (( 2 * hb >= lease )); then
    printf 'heartbeat-lease-margin\theartbeatを1回落としてもlease_seconds(%s)が切れない余裕がありません（heartbeat間隔=lease_seconds/3+1=%s秒）。\tqueue.lease_seconds を大きくするか、bin/lib/agentic-loop/worker.sh のheartbeat間隔導出を見直してください。\n' "$lease" "$hb"
  fi
  if (( pause_grace > 0 && worker_timeout > 0 && pause_grace >= worker_timeout )); then
    printf 'pause-grace-before-timeout\tpause_grace_seconds(%s) が worker_timeout_seconds(%s) 以上で、協調drainが完了する前にhard killの領域に入ります。\tqueue.pause_grace_seconds を queue.worker_timeout_seconds より小さくしてください。\n' "$pause_grace" "$worker_timeout"
  fi
  if (( poll > poll_max )); then
    printf 'poll-backoff-order\tpoll_seconds(%s) が poll_max_seconds(%s) を超えており、idle backoffが単調に増加しません。\tqueue.poll_seconds を queue.poll_max_seconds 以下にしてください。\n' "$poll" "$poll_max"
  fi
  if (( EXHAUSTION_PAUSE_SECONDS > EXHAUSTION_BACKOFF_MAX_SECONDS )); then
    printf 'exhaustion-backoff-order\tEXHAUSTION_PAUSE_SECONDS(%s) がEXHAUSTION_BACKOFF_MAX_SECONDS(%s)を超えており、backoffの上限がinitialを下回ります。\tbin/agentic-loop のEXHAUSTION_PAUSE_SECONDS/EXHAUSTION_BACKOFF_MAX_SECONDS定数を見直してください。\n' "$EXHAUSTION_PAUSE_SECONDS" "$EXHAUSTION_BACKOFF_MAX_SECONDS"
  fi
  if (( orphan_grace > 0 && worker_timeout > 0 && orphan_grace >= worker_timeout )); then
    printf 'orphan-grace-before-timeout\tworker_orphan_grace_seconds(%s) が worker_timeout_seconds(%s) 以上で、orphan reapがworker_timeout_secondsより前に発火しません。\tqueue.worker_orphan_grace_seconds を queue.worker_timeout_seconds より小さくしてください。\n' "$orphan_grace" "$worker_timeout"
  fi
  return 0
}


# --committed mode only: compare bin/agentic-loop's hardcoded fallback
# constants (the plain VAR=value lines assigned before load_config runs, used
# when .agentic-loop.toml omits a key) against the committed toml's value for
# the same key, via CONFIG_NUMERIC_KEYS. Prints one name<TAB>detail<TAB>remedy
# row per mismatch; a key absent from either side is not compared.
timing_fallback_mismatches() {
  local program_path=$1 props=$2 varname key fallback_value toml_value
  for varname in "${!CONFIG_NUMERIC_KEYS[@]}"; do
    key=${CONFIG_NUMERIC_KEYS[$varname]}
    fallback_value=$(awk -F= -v v="$varname" '$1 == v { print $2; exit }' "$program_path")
    [[ -n $fallback_value ]] || continue
    toml_value=$(awk -F ' = ' -v k="$key" '$1 == k { print $2; exit }' <<< "$props")
    [[ -n $toml_value ]] || continue
    if [[ $fallback_value != "$toml_value" ]]; then
      printf 'fallback-mismatch:%s\t%s のbin/agentic-loop既定値(%s)が.agentic-loop.tomlの%s(%s)と一致しません。\t両者を同じ値に揃えてください（bin/agentic-loopの%s=行、または.agentic-loop.tomlの%s）。\n' \
        "$varname" "$varname" "$fallback_value" "$key" "$toml_value" "$varname" "$key"
    fi
  done
}


# Internal command (see bin/agentic-loop's _reap-orphans/_prune-worktrees for
# the same pattern): a deterministic, GitHub-free boundary for lint and tests.
# Without --committed, evaluates the live merged config (CONFIG_FILE plus any
# git-ignored CONFIG_LOCAL override) -- the same values doctor sees. With
# --committed, re-parses CONFIG_FILE alone (ignoring CONFIG_LOCAL, so a
# developer's local override never fails a teammate's `make lint`) and also
# checks the bash-fallback-vs-toml mismatch above; this is what scripts/lint.sh
# runs as a hard gate.
cmd_timing_check() {
  shift
  local committed=0
  case ${1:-} in
    '') ;;
    --committed) committed=1; shift ;;
    *) usage; return 2 ;;
  esac
  [[ $# -eq 0 ]] || { usage; return 2; }

  local stall=$STALL_SECONDS provider_stall=$PROVIDER_STALL_SECONDS worker_timeout=$WORKER_TIMEOUT_SECONDS
  local lease=$LEASE_SECONDS pause_grace=$PAUSE_GRACE_SECONDS poll=$POLL_SECONDS poll_max=$POLL_MAX_SECONDS
  local orphan_grace=$WORKER_ORPHAN_GRACE_SECONDS props=''

  if (( committed )); then
    [[ -r $CONFIG_FILE ]] || { printf '%s is not readable.\n' "$CONFIG_FILE" >&2; return 1; }
    command -v yq >/dev/null 2>&1 || { printf 'yq is required to read %s.\n' "$CONFIG_FILE" >&2; return 1; }
    props=$(yq -p toml -o props "$CONFIG_FILE" 2>/dev/null) || { printf 'cannot parse %s\n' "$CONFIG_FILE" >&2; return 1; }
    local line key value
    while IFS= read -r line; do
      [[ -z $line ]] && continue
      key=${line%% = *}; value=${line#* = }
      case $key in
        queue.stall_seconds) stall=$value ;;
        queue.provider_stall_seconds) provider_stall=$value ;;
        queue.worker_timeout_seconds) worker_timeout=$value ;;
        queue.lease_seconds) lease=$value ;;
        queue.pause_grace_seconds) pause_grace=$value ;;
        queue.poll_seconds) poll=$value ;;
        queue.poll_max_seconds) poll_max=$value ;;
        queue.worker_orphan_grace_seconds) orphan_grace=$value ;;
      esac
    done <<< "$props"
  fi

  local violated=0 name detail remedy
  while IFS=$'\t' read -r name detail remedy; do
    [[ -n $name ]] || continue
    violated=1
    printf '[違反] %s\n  詳細: %s\n  対処: %s\n' "$name" "$detail" "$remedy"
  done < <(timing_invariant_violations "$stall" "$provider_stall" "$worker_timeout" "$lease" "$pause_grace" "$poll" "$poll_max" "$orphan_grace")

  if (( committed )); then
    while IFS=$'\t' read -r name detail remedy; do
      [[ -n $name ]] || continue
      violated=1
      printf '[違反] %s\n  詳細: %s\n  対処: %s\n' "$name" "$detail" "$remedy"
    done < <(timing_fallback_mismatches "$PROGRAM_PATH" "$props")
  fi

  (( violated == 0 ))
}


validate_config() {
  [[ $POLL_SECONDS =~ ^[1-9][0-9]*$ ]] || fail 'POLL_SECONDS must be a positive integer'
  [[ $POLL_MAX_SECONDS =~ ^[1-9][0-9]*$ ]] || fail 'POLL_MAX_SECONDS must be a positive integer'
  [[ $MAX_WORKERS =~ ^[1-9][0-9]*$ ]] || fail 'MAX_WORKERS must be a positive integer'
  [[ $LEASE_SECONDS =~ ^[1-9][0-9]*$ ]] || fail 'LEASE_SECONDS must be a positive integer'
  [[ $STOP_TIMEOUT =~ ^[0-9]+$ ]] || fail 'STOP_TIMEOUT must be a non-negative integer'
  [[ $PAUSE_GRACE_SECONDS =~ ^[0-9]+$ ]] || fail 'PAUSE_GRACE_SECONDS must be a non-negative integer'
  [[ $STALE_DAYS =~ ^[0-9]+$ ]] || fail 'STALE_DAYS must be a non-negative integer'
  [[ $GRAPHQL_RESERVE =~ ^[0-9]+$ ]] || fail 'GRAPHQL_RESERVE must be a non-negative integer'
  [[ $CORE_RESERVE =~ ^[0-9]+$ ]] || fail 'CORE_RESERVE must be a non-negative integer'
  [[ $RATE_LIMIT_CACHE_SECONDS =~ ^[1-9][0-9]*$ ]] || fail 'RATE_LIMIT_CACHE_SECONDS must be a positive integer'
  [[ $API_RETRY_ATTEMPTS =~ ^[1-9][0-9]*$ ]] || fail 'API_RETRY_ATTEMPTS must be a positive integer'
  [[ $API_RETRY_BASE_SECONDS =~ ^[0-9]+$ ]] || fail 'API_RETRY_BASE_SECONDS must be a non-negative integer'
  [[ $MAX_ATTEMPTS =~ ^[1-9][0-9]*$ ]] || fail 'MAX_ATTEMPTS must be a positive integer'
  [[ $RETRY_COOLDOWN_SECONDS =~ ^[0-9]+$ ]] || fail 'RETRY_COOLDOWN_SECONDS must be a non-negative integer'
  [[ $WORKER_TIMEOUT_SECONDS =~ ^[0-9]+$ ]] || fail 'WORKER_TIMEOUT_SECONDS must be a non-negative integer'
  [[ $WORKER_ORPHAN_GRACE_SECONDS =~ ^[0-9]+$ ]] || fail 'WORKER_ORPHAN_GRACE_SECONDS must be a non-negative integer'
  [[ $STALL_SECONDS =~ ^[0-9]+$ ]] || fail 'STALL_SECONDS must be a non-negative integer'
  [[ $PROVIDER_STALL_SECONDS =~ ^[0-9]+$ ]] || fail 'PROVIDER_STALL_SECONDS must be a non-negative integer'
  case $UNKNOWN_SCOPE in isolated | exclusive | open) ;; *) fail 'UNKNOWN_SCOPE must be isolated, exclusive, or open' ;; esac
  case $TRACEABILITY in require | warn | off) ;; *) fail 'TRACEABILITY must be require, warn, or off' ;; esac
  case $PREFLIGHT in require | warn | off) ;; *) fail 'PREFLIGHT must be require, warn, or off' ;; esac
  case $WORKLOAD in require | warn | off) ;; *) fail 'WORKLOAD must be require, warn, or off' ;; esac
  case $POSTMORTEM_AUTO_DETECT in on | off) ;; *) fail 'POSTMORTEM_AUTO_DETECT must be on or off' ;; esac
  [[ $POSTMORTEM_MAX_AUTO_CREATED_PER_DAY =~ ^[0-9]+$ && $POSTMORTEM_MAX_AUTO_CREATED_PER_DAY -ge 1 && $POSTMORTEM_MAX_AUTO_CREATED_PER_DAY -le 20 ]] || fail 'POSTMORTEM_MAX_AUTO_CREATED_PER_DAY must be an integer between 1 and 20'
}
