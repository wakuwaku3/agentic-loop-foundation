# Module: config.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034



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
  case $UNKNOWN_SCOPE in isolated | exclusive | open) ;; *) fail 'UNKNOWN_SCOPE must be isolated, exclusive, or open' ;; esac
  case $TRACEABILITY in require | warn | off) ;; *) fail 'TRACEABILITY must be require, warn, or off' ;; esac
  case $PREFLIGHT in require | warn | off) ;; *) fail 'PREFLIGHT must be require, warn, or off' ;; esac
  case $WORKLOAD in require | warn | off) ;; *) fail 'WORKLOAD must be require, warn, or off' ;; esac
  case $POSTMORTEM_AUTO_DETECT in on | off) ;; *) fail 'POSTMORTEM_AUTO_DETECT must be on or off' ;; esac
  [[ $POSTMORTEM_MAX_AUTO_CREATED_PER_DAY =~ ^[0-9]+$ && $POSTMORTEM_MAX_AUTO_CREATED_PER_DAY -ge 1 && $POSTMORTEM_MAX_AUTO_CREATED_PER_DAY -le 20 ]] || fail 'POSTMORTEM_MAX_AUTO_CREATED_PER_DAY must be an integer between 1 and 20'
}
