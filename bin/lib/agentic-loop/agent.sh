# Module: agent.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034



agent_default_provider() {
  local provider=${AGENT_PROVIDER:-}
  [[ -n $provider ]] || provider=$(config_value 'agent.provider')
  printf '%s' "${provider:-codex}"
}

agent_phase_provider() {
  local provider
  provider=$(config_value "agent.$1.provider")
  [[ -n $provider ]] && { printf '%s' "$provider"; return; }
  agent_default_provider
}

agent_phase_model() { config_value "agent.$1.model"; }

agent_phase_effort() {
  local effort
  effort=$(config_value "agent.$1.reasoning_effort")
  [[ -n $effort ]] && { printf '%s' "$effort"; return; }
  case $1 in plan) printf 'high' ;; exec) printf 'low' ;; esac
}

agent_plan_max_retries() {
  local n=${AGENT_PLAN_MAX_RETRIES:-}
  [[ -n $n ]] || n=$(config_value 'agent.retry.plan_max')
  [[ $n =~ ^[0-9]+$ ]] && printf '%s' "$n" || printf '1'
}

provider_valid() { case $1 in codex | claude | opencode) return 0 ;; *) return 1 ;; esac; }

provider_command() { case $1 in claude) printf 'claude' ;; opencode) printf 'opencode' ;; *) printf 'codex' ;; esac; }

# Report whether a provider CLI can launch a non-interactive worker.
provider_ready() {
  case $1 in
    codex) command -v codex >/dev/null 2>&1 && codex exec --help 2>/dev/null | grep -Fq -- '--add-dir' ;;
    claude) command -v claude >/dev/null 2>&1 ;;
    opencode) command -v opencode >/dev/null 2>&1 ;;
    *) return 1 ;;
  esac
}


# --- Pool/tier priority selection (Issue #155) ---
#
# A phase may declare ordered `tiers`. Each tier binds a subscription pool
# (quota boundary) to one provider and an ordered `models` list; a model's
# `max_usage_percent` demotes it to the next model when the pool usage crosses
# the threshold, and an exhausted pool skips the whole tier. Phases without
# tiers keep the scalar provider/model/reasoning_effort keys, normalized below
# to a single implicit tier whose pool is the provider name (backward
# compatible). `yq -p toml -o props` renders the nested arrays as
# `agent.<phase>.tiers.<i>.pool`, `...tiers.<i>.models.<j>.model`, etc.; this
# normalization is the single place those dotted keys are interpreted.
agent_pool_sanitize() { printf '%s' "$1" | tr -c 'A-Za-z0-9_-' '_'; }

agent_phase_has_tiers() { config_has_prefix "agent.$1.tiers."; }

agent_tier_indices() {
  local phase=$1
  config_prefix_lines "agent.$phase.tiers." | sed -E 's/^agent\.[^.]+\.tiers\.([0-9]+)\..*/\1/' | sort -un
}

agent_model_indices() {
  local phase=$1 tier=$2
  config_prefix_lines "agent.$phase.tiers.$tier.models." | sed -E 's/^agent\.[^.]+\.tiers\.[0-9]+\.models\.([0-9]+)\..*/\1/' | sort -un
}


# Emit every candidate for a phase, ordered by (tier_index, model_index), one
# row per line, fields separated by a non-whitespace control character so empty
# fields (unset model / max_usage_percent) survive read without IFS collapsing:
#   tier_index<FS>model_index<FS>pool<FS>provider<FS>model<FS>effort<FS>max_usage_percent
readonly CAND_FS=$'\x1f'

agent_phase_tiers() {
  local phase=$1 tier mi pool provider model effort max
  if agent_phase_has_tiers "$phase"; then
    for tier in $(agent_tier_indices "$phase"); do
      pool=$(config_value "agent.$phase.tiers.$tier.pool")
      provider=$(config_value "agent.$phase.tiers.$tier.provider")
      [[ -n $provider ]] || provider=$(agent_default_provider)
      [[ -n $pool ]] || pool=$provider
      effort=$(config_value "agent.$phase.tiers.$tier.reasoning_effort")
      [[ -n $effort ]] || effort=$(agent_phase_effort "$phase")
      for mi in $(agent_model_indices "$phase" "$tier"); do
        model=$(config_value "agent.$phase.tiers.$tier.models.$mi.model")
        max=$(config_value "agent.$phase.tiers.$tier.models.$mi.max_usage_percent")
        printf '%s%s%s%s%s%s%s%s%s%s%s%s%s\n' "$tier" "$CAND_FS" "$mi" "$CAND_FS" "$pool" "$CAND_FS" "$provider" "$CAND_FS" "$model" "$CAND_FS" "$effort" "$CAND_FS" "$max"
      done
    done
  else
    provider=$(agent_phase_provider "$phase")
    printf '%s%s%s%s%s%s%s%s%s%s%s%s%s\n' "0" "$CAND_FS" "0" "$CAND_FS" "$provider" "$CAND_FS" "$provider" "$CAND_FS" "$(agent_phase_model "$phase")" "$CAND_FS" "$(agent_phase_effort "$phase")" "$CAND_FS" ""
  fi
}


agent_candidate_count() { agent_phase_tiers "$1" | grep -c . || true; }


# The distinct providers actually used across the worker phases, including
# every fallback tier, so install/preflight/systemd PATH cover all CLIs.
agent_used_providers() {
  local phase provider
  for phase in plan exec diagnose; do
    while IFS="$CAND_FS" read -r _ _ _ provider _ _ _; do
      [[ -n $provider ]] && printf '%s\n' "$provider"
    done < <(agent_phase_tiers "$phase")
  done | sort -u
}


# Weekly (secondary) rate-limit usage percent from the newest Codex session log.
# Codex writes token_count events to ~/.codex/sessions; the schema is
# .payload.rate_limits.secondary.used_percent on event_msg records. Returns
# non-zero when no readable telemetry exists so the caller can fail open.
codex_weekly_used_percent() {
  local sessions="${CODEX_HOME:-$HOME/.codex}/sessions" file pct
  [[ -d $sessions ]] || return 1
  file=$(find "$sessions" -type f -name 'rollout-*.jsonl' -printf '%T@\t%p\n' 2>/dev/null | sort -rn | head -n 1 | cut -f2-)
  [[ -n $file && -r $file ]] || return 1
  pct=$(yq -p json -o tsv 'select(.payload.type == "token_count") | .payload.rate_limits.secondary.used_percent' "$file" 2>/dev/null | grep -E '^[0-9]+(\.[0-9]+)?$' | tail -n 1)
  [[ -n $pct ]] || return 1
  printf '%s' "$pct"
}


# Best-effort usage percent (0-100) for the OpenCode Go subscription from its
# read-only usage API. The API key is read from ~/.local/share/opencode/auth.json
# (`opencode-go` entry) and is never written to logs, comments, or state files.
# The pinned runtime has no curl, so the request rides `gh api` (always present,
# which can proxy any HTTPS URL). Returns non-zero when unreadable so callers
# fail open. Results are cached to keep the supervisor poll cheap.
opencode_go_usage_percent() {
  local auth_file key response percent status
  auth_file="${XDG_DATA_HOME:-$HOME/.local/share}/opencode/auth.json"
  [[ -r $auth_file ]] || return 1
  key=$(yq -p json -r '."opencode-go".key // ""' "$auth_file" 2>/dev/null)
  [[ -n $key ]] || return 1
  command -v gh >/dev/null 2>&1 || return 1
  if command -v timeout >/dev/null 2>&1; then
    # workload-boundary: non-GitHub host (opencode usage API) riding `gh api` as an HTTPS client; repo_api targets GitHub REST only
    response=$(timeout 20 gh api "https://opencode.ai/zen/go/v1/usage" -H "Authorization: Bearer $key" 2>/dev/null) || return 1
  else
    # workload-boundary: non-GitHub host (opencode usage API) riding `gh api` as an HTTPS client; repo_api targets GitHub REST only
    response=$(gh api "https://opencode.ai/zen/go/v1/usage" -H "Authorization: Bearer $key" 2>/dev/null) || return 1
  fi
  percent=$(printf '%s\n' "$response" | yq -p json -o tsv '[.usage.rolling.percent // 0, .usage.weekly.percent // 0, .usage.monthly.percent // 0] | max' - 2>/dev/null)
  [[ $percent =~ ^[0-9]+(\.[0-9]+)?$ ]] || return 1
  status=$(printf '%s\n' "$response" | yq -p json -o tsv '[.usage.rolling.status // "", .usage.weekly.status // "", .usage.monthly.status // ""] | join(",")' - 2>/dev/null)
  # An explicitly exhausted window is the binding constraint regardless of the
  # numeric percent (treat as fully spent).
  if grep -qiE '(^|,)exhausted(,|$)' <<< "$status"; then printf '100\n'; else printf '%s\n' "$percent"; fi
}


# Cached wrapper around opencode_go_usage_percent: the usage API is read at most
# once per USAGE_CACHE_SECONDS, and a failure is remembered so a network outage
# does not hammer the API every poll. The cache holds only the numeric percent
# (no key/token) under STATE_ROOT/pools/.
readonly USAGE_CACHE_SECONDS=300

opencode_go_usage_cached() {
  local cache_file="$STATE_ROOT/pools/usage-opencode" now fetched percent
  now=$(date +%s)
  if [[ -r $cache_file ]]; then
    IFS=$'\t' read -r fetched percent < "$cache_file" 2>/dev/null || true
    if [[ $fetched =~ ^[0-9]+$ ]] && (( now - fetched < USAGE_CACHE_SECONDS )); then
      [[ $percent =~ ^[0-9]+(\.[0-9]+)?$ ]] || return 1
      printf '%s' "$percent"
      return 0
    fi
  fi
  if ! percent=$(opencode_go_usage_percent); then
    mkdir -p "$STATE_ROOT/pools"
    printf '%s\t\n' "$now" > "$cache_file.tmp" 2>/dev/null && mv "$cache_file.tmp" "$cache_file" 2>/dev/null || true
    return 1
  fi
  mkdir -p "$STATE_ROOT/pools"
  printf '%s\t%s\n' "$now" "$percent" > "$cache_file.tmp" 2>/dev/null && mv "$cache_file.tmp" "$cache_file" 2>/dev/null || true
  printf '%s' "$percent"
}


# Measure one provider's pool usage: 0-100 on stdout, non-zero when unreadable.
agent_provider_usage_percent() {
  case $1 in
    codex) codex_weekly_used_percent || return 1 ;;
    opencode) opencode_go_usage_cached || return 1 ;;
    *) return 1 ;;
  esac
}


# Tri-state usage health for pool recovery: recovered|exhausted|unreadable.
agent_provider_usage_state() {
  local provider=$1 pct
  pct=$(agent_provider_usage_percent "$provider") || { printf 'unreadable\n'; return 0; }
  if awk -v p="$pct" 'BEGIN { exit !(p >= 100) }'; then printf 'exhausted\n'; else printf 'recovered\n'; fi
}


# --- Pool exhaustion and recovery (Issue #155) ---
# Exhaustion is recorded per pool (subscription) under STATE_ROOT/pools/<pool>/
# (Git-ignored, key/token-free). The marker holds a resume_epoch. When the
# provider names a concrete reset (Codex "try again at ...", OpenCode
# resetsAt), that epoch is stored so a multi-day weekly limit is not cleared
# after the short EXHAUSTION_PAUSE_SECONDS default. Recovery never clears a
# marker before resume_epoch; after it, measured recovery or a safe retry
# (unreadable usage) may clear, while a still-exhausted measurement extends
# the marker by the default pause.

agent_pool_marker() { printf '%s/pools/%s/exhausted' "$STATE_ROOT" "$(agent_pool_sanitize "$1")"; }

# Human-facing recovery basis for the current marker: reset (provider-stated
# instant honored as-is), probe (usage measurement is available and still says
# exhausted, so the flat re-check cadence is a real re-probe, not a guess), or
# backoff (neither is available, so the pause grows -- see agent_pool_streak_*
# below). status reads this to show *why* a resume time is what it is
# (Issue #158 completion criterion: show recovery ETA/basis).
agent_pool_basis_file() { printf '%s/pools/%s/basis' "$STATE_ROOT" "$(agent_pool_sanitize "$1")"; }

agent_pool_basis_get() {
  local file basis; file=$(agent_pool_basis_file "$1")
  [[ -r $file ]] && read -r basis < "$file"
  printf '%s' "${basis:-backoff}"
}

agent_pool_basis_set() {
  local pool=$1 basis=$2 file; file=$(agent_pool_basis_file "$pool")
  mkdir -p "$(dirname "$file")"
  printf '%s\n' "$basis" > "$file"
}

# Consecutive same-pool re-exhaustion count (Git-ignored, numeric only), used
# solely to grow the pause when a fresh mark has neither a provider-stated
# reset nor a usage measurement to confirm real recovery (Issue #158): a
# provider with no agent_provider_usage_percent case (e.g. claude today) would
# otherwise repeat the flat EXHAUSTION_PAUSE_SECONDS forever -- mark, one
# blind retry, still exhausted, remark, forever -- instead of backing off.
# Cleared on a provider-stated reset, a measured recovery, or a genuine
# successful stage run on the pool; never cleared by the blind retry itself,
# so a retry that fails again keeps growing the pause.
agent_pool_streak_file() { printf '%s/pools/%s/streak' "$STATE_ROOT" "$(agent_pool_sanitize "$1")"; }

agent_pool_streak_get() {
  local file n; file=$(agent_pool_streak_file "$1")
  [[ -r $file ]] && read -r n < "$file"
  [[ $n =~ ^[0-9]+$ ]] && printf '%s' "$n" || printf '0'
}

agent_pool_streak_clear() { rm -f "$(agent_pool_streak_file "$1")"; }

agent_pool_streak_bump() {
  local pool=$1 n file
  n=$(agent_pool_streak_get "$pool")
  n=$((n + 1))
  file=$(agent_pool_streak_file "$pool")
  mkdir -p "$(dirname "$file")"
  printf '%s\n' "$n" > "$file"
  printf '%s' "$n"
}

# Best-effort parse of a provider-stated reset instant into epoch seconds.
# Accepts ISO-8601 (…Z) and Codex-style "try again at Aug 20th, 2026 9:27 PM".
# Returns non-zero when nothing parseable is found.
agent_parse_reset_epoch() {
  local text=${1:-} iso human epoch
  [[ -n $text ]] || return 1
  iso=$(grep -oiE '[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z?' <<< "$text" | head -n 1 || true)
  if [[ -n $iso ]]; then
    epoch=$(date -d "$iso" +%s 2>/dev/null) || true
    [[ $epoch =~ ^[0-9]+$ ]] && { printf '%s\n' "$epoch"; return 0; }
  fi
  human=$(grep -oiE 'try again at[[:space:]]+[^.;\n]+' <<< "$text" | head -n 1 | sed -E 's/^[Tt]ry again at[[:space:]]+//; s/([0-9]+)(st|nd|rd|th)/\1/g' || true)
  if [[ -n $human ]]; then
    epoch=$(date -d "$human" +%s 2>/dev/null) || true
    [[ $epoch =~ ^[0-9]+$ ]] && { printf '%s\n' "$epoch"; return 0; }
  fi
  return 1
}


# The distinct pools referenced by any phase (sanitized, for `for` loops).
agent_all_pools() {
  local phase pool
  for phase in plan exec diagnose; do
    while IFS="$CAND_FS" read -r _ _ pool _ _ _ _; do
      [[ -n $pool ]] && printf '%s\n' "$(agent_pool_sanitize "$pool")"
    done < <(agent_phase_tiers "$phase")
  done | sort -u
}


# The providers that consume a pool's shared quota (for usage-based recovery).
agent_pool_providers() {
  local wanted=$1 phase pool provider
  for phase in plan exec diagnose; do
    while IFS="$CAND_FS" read -r _ _ pool provider _ _ _; do
      [[ -n $provider && $(agent_pool_sanitize "$pool") == "$wanted" ]] && printf '%s\n' "$provider"
    done < <(agent_phase_tiers "$phase")
  done | sort -u
}


# Whether the pool is currently marked exhausted, without attempting recovery
# (pure marker check; status uses this so it never touches the usage API).
agent_pool_marker_active() {
  local pool=$1 resume now marker
  marker=$(agent_pool_marker "$pool")
  [[ -r $marker ]] || return 1
  read -r resume < "$marker" || true
  now=$(date +%s)
  [[ $resume =~ ^[0-9]+$ ]] && (( now >= resume )) && return 1
  return 0
}


# Whether ANY declared pool is currently marked exhausted -- a weaker signal
# than exhaustion_note_pause's "every candidate pool is unavailable" (which
# gates new claims), but enough to treat a correlated worker crash/hang/lease
# expiry as environment-caused rather than a genuine task failure, so it does
# not burn the Issue's retry budget (Issue #158 root cause: "crash / timeout
# は枯渇保護を通らない"). Never touches the usage API (pure marker reads).
agent_any_pool_marker_active() {
  local pool
  for pool in $(agent_all_pools); do
    agent_pool_marker_active "$pool" && return 0
  done
  return 1
}


# Usage-based recovery probe: true when at least one provider of the pool is
# measurably recovered and none is measurably exhausted. Unmeasurable providers
# do not count as recovered (fail closed while a marker is held).
agent_pool_usage_recovered() {
  local pool=$1 provider recovered=0
  for provider in $(agent_pool_providers "$pool"); do
    case $(agent_provider_usage_state "$provider") in
      recovered) recovered=1 ;;
      exhausted) return 1 ;;
    esac
  done
  (( recovered == 1 ))
}

# True when any provider of the pool is measurably still exhausted.
agent_pool_usage_still_exhausted() {
  local pool=$1 provider
  for provider in $(agent_pool_providers "$pool"); do
    case $(agent_provider_usage_state "$provider") in
      exhausted) return 0 ;;
    esac
  done
  return 1
}


# Whether the pool is currently exhausted. A marker before its resume_epoch is
# always binding (provider-stated multi-day resets must not be cut short by a
# stale session-log "recovered" reading). After resume_epoch: clear on measured
# recovery or unreadable usage (retry); if usage is still exhausted, extend the
# marker by EXHAUSTION_PAUSE_SECONDS and stay exhausted. A real usage probe
# (still-exhausted or recovered) is never a blind guess, so it also clears the
# backoff streak: growth is reserved for pools no provider can measure
# (Issue #158). Returns 0 when exhausted.
agent_pool_exhausted() {
  local pool=$1 marker resume now
  marker=$(agent_pool_marker "$pool")
  [[ -r $marker ]] || return 1
  read -r resume < "$marker" || true
  now=$(date +%s)
  if [[ $resume =~ ^[0-9]+$ ]] && (( now < resume )); then
    return 0
  fi
  if agent_pool_usage_still_exhausted "$pool"; then
    printf '%s\n' "$(( now + EXHAUSTION_PAUSE_SECONDS ))" > "$marker"
    agent_pool_basis_set "$pool" probe
    agent_pool_streak_clear "$pool"
    return 0
  fi
  if agent_pool_usage_recovered "$pool"; then
    rm -f "$marker" "$(agent_pool_basis_file "$pool")"
    agent_pool_streak_clear "$pool"
    return 1
  fi
  # resume_epoch reached and usage unreadable: allow a single retry attempt.
  # The streak (if any) is left untouched -- it is only cleared by a genuine
  # success or a real usage probe above, so a retry that fails again still
  # grows the next pause (see agent_mark_pool_exhausted).
  rm -f "$marker" "$(agent_pool_basis_file "$pool")"
  return 1
}


# Record that a pool's quota is spent so the picker skips it and the supervisor
# pauses claiming only when every pool becomes unavailable. Optional second
# argument is a result file path or free-form diagnostic text; when it names a
# concrete reset instant, that epoch is preferred and honored as-is (no
# backoff -- the provider already told us when). Otherwise back off
# exponentially from EXHAUSTION_PAUSE_SECONDS by the pool's consecutive
# re-exhaustion streak, capped at EXHAUSTION_BACKOFF_MAX_SECONDS, so a pool no
# provider can measure (e.g. claude has no agent_provider_usage_percent case)
# does not repeat the same short pause forever after every blind retry fails
# again (Issue #158; see docs/decisions/0012, 0027). An existing later
# resume_epoch is never shortened.
agent_mark_pool_exhausted() {
  local pool=$1 source=${2:-} marker resume now parsed='' existing='' text='' basis streak shift
  marker=$(agent_pool_marker "$pool")
  mkdir -p "$(dirname "$marker")"
  now=$(date +%s)
  if [[ -n $source ]]; then
    if [[ -f $source && -r $source ]]; then
      text=$(cat "$source" 2>/dev/null || true)
    else
      text=$source
    fi
    parsed=$(agent_parse_reset_epoch "$text" 2>/dev/null || true)
  fi
  if [[ $parsed =~ ^[0-9]+$ ]]; then
    resume=$parsed
    basis=reset
    agent_pool_streak_clear "$pool"
  else
    streak=$(agent_pool_streak_bump "$pool")
    shift=$(( streak - 1 )); (( shift > 10 )) && shift=10
    resume=$(( now + EXHAUSTION_PAUSE_SECONDS * (1 << shift) ))
    (( resume > now + EXHAUSTION_BACKOFF_MAX_SECONDS )) && resume=$(( now + EXHAUSTION_BACKOFF_MAX_SECONDS ))
    basis=backoff
  fi
  if [[ -r $marker ]]; then
    read -r existing < "$marker" || true
    if [[ $existing =~ ^[0-9]+$ ]] && (( existing > resume )); then
      resume=$existing
    fi
  fi
  printf '%s\n' "$resume" > "$marker"
  agent_pool_basis_set "$pool" "$basis"
}


# Pick the best currently-usable candidate for a phase: the first tier whose
# pool is not exhausted, then the first model whose measured pool usage does not
# exceed its max_usage_percent (unset = unlimited, unmeasurable = fail open).
# `tried` is a comma-separated list of "tier:model" keys already attempted in
# this stage, so a model-specific failure moves on instead of looping. Emits
# key=value lines and returns 0, or returns 1 when no candidate is usable.
agent_pick_tier() {
  local phase=$1 tried=${2:-} pool provider model effort tier midx max row key pct
  while IFS="$CAND_FS" read -r tier midx pool provider model effort max; do
    [[ -n $provider ]] || continue
    key="$tier:$midx"
    [[ $tried =~ (^|,)"$key"(,|$) ]] && continue
    agent_pool_exhausted "$pool" && continue
    if [[ -n $max && $max =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
      if pct=$(agent_provider_usage_percent "$provider"); then
        if awk -v p="$pct" -v m="$max" 'BEGIN { exit !(p > m) }'; then continue; fi
      fi
    fi
    printf 'pool=%s\nprovider=%s\nmodel=%s\nreasoning_effort=%s\ntier_index=%s\nmodel_index=%s\n' \
      "$pool" "$provider" "$model" "$effort" "$tier" "$midx"
    return 0
  done < <(agent_phase_tiers "$phase")
  return 1
}


# Load key=value candidate lines (agent_pick_tier output, on stdin) into globals.
agent_candidate_load() {
  local line key value
  CAND_POOL='' CAND_PROVIDER='' CAND_MODEL='' CAND_EFFORT='' CAND_TIER='' CAND_MODEL_IDX=''
  while IFS='=' read -r key value; do
    case $key in
      pool) CAND_POOL=$value ;;
      provider) CAND_PROVIDER=$value ;;
      model) CAND_MODEL=$value ;;
      reasoning_effort) CAND_EFFORT=$value ;;
      tier_index) CAND_TIER=$value ;;
      model_index) CAND_MODEL_IDX=$value ;;
    esac
  done
}


# Best-effort "next likely candidate" for a phase without live usage
# measurement (status must stay network-free): the first candidate whose pool is
# not currently marked exhausted. Sets NC_POOL/NC_PROVIDER/NC_MODEL/NC_TIER/
# NC_MODEL_IDX and returns 0, or returns 1 when none is available.
agent_next_candidate_nominal() {
  local phase=$1 tier midx pool provider model effort max
  NC_POOL='' NC_PROVIDER='' NC_MODEL='' NC_TIER='' NC_MODEL_IDX=''
  while IFS="$CAND_FS" read -r tier midx pool provider model effort max; do
    [[ -n $provider ]] || continue
    agent_pool_marker_active "$pool" && continue
    NC_POOL=$pool; NC_PROVIDER=$provider; NC_MODEL=$model; NC_TIER=$tier; NC_MODEL_IDX=$midx
    return 0
  done < <(agent_phase_tiers "$phase")
  return 1
}


# Decide whether a new Issue may be claimed while leaving an emergency reserve.
# Only Codex exposes usage headlessly; for other providers, or when telemetry is
# unreadable, claiming continues (fail open). budget.weekly_reserve_percent <= 0
# disables the guard.
budget_allows_claim() {
  local reserve pct limit
  reserve=$(config_value 'budget.weekly_reserve_percent')
  [[ $reserve =~ ^[0-9]+$ ]] || return 0
  (( reserve <= 0 )) && return 0
  agent_used_providers | grep -qx codex || return 0
  pct=$(codex_weekly_used_percent) || return 0
  limit=$((100 - reserve))
  awk -v p="$pct" -v l="$limit" 'BEGIN { exit !(p > l) }' && return 1
  return 0
}


# Announce only the transitions into and out of a budget pause so the local
# supervisor log is not spammed every poll.
budget_note_pause() {
  local marker="$STATE_ROOT/budget-paused"
  if budget_allows_claim; then
    [[ -e $marker ]] && { rm -f "$marker"; say '週次利用率がreserve閾値を下回ったため、Issueのclaimを再開します。' >&2; }
    return 0
  fi
  [[ -e $marker ]] || { mkdir -p "$STATE_ROOT"; : > "$marker"; say '週次利用率がreserve閾値を超えたため、緊急枠を確保するためIssueのclaimを一時停止します。' >&2; }
  return 1
}


# Pause claiming while remaining REST(core) quota is at/below CORE_RESERVE, so a
# reserve is kept for lease heartbeats, restart recovery and in-flight worker
# writes (see docs/decisions/0003). Logs the transition once. CORE_RESERVE <= 0
# disables the guard.
core_budget_note_pause() {
  local marker="$STATE_ROOT/core-budget-paused"
  if (( CORE_RESERVE <= 0 )) || core_budget_allows "$((CORE_RESERVE + 1))"; then
    [[ -e $marker ]] && { rm -f "$marker"; say 'GitHub REST APIの残量が回復したため、Issueのclaimを再開します。' >&2; }
    return 0
  fi
  [[ -e $marker ]] || { mkdir -p "$STATE_ROOT"; : > "$marker"; say 'GitHub REST APIの残量がreserve閾値を下回ったため、必須操作の枠を確保するためIssueのclaimを一時停止します。' >&2; }
  return 1
}


# Classify a stage result: pool-quota exhaustion vs model-specific failure.
# Pool exhaustion (quota / 429 / usage limit / insufficient_quota / credit
# balance, or a non-zero exit with no output -- the latter kept for backward
# compatibility) pauses that pool and re-queues the Issue. `overloaded` and
# model-resolution failures are model-specific: the stage moves to the next
# model in the pool instead of treating the whole subscription as spent.
#
# The quota signatures are matched only when the provider actually reported a
# failure (a non-zero exit, or a structured error flag such as Claude's
# --output-format json is_error). A stage that SUCCEEDED must never be read as
# exhausted just because its OUTPUT discusses rate limits or quotas -- a plan
# about scalability, finite-resource design, or the loop's own exhaustion
# handling legitimately contains those words, and matching them there falsely
# spends the whole pool and blocks the queue (see the regression tests in
# tests/test-agentic-loop.sh). $3 is the provider-error flag from agent_run_stage
# (STAGE_PROVIDER_ERROR); it defaults to 0 so existing two-argument callers keep
# the exit-code-only behavior.
agent_result_is_pool_exhausted() {
  local result_file=$1 exit_code=$2 provider_error=${3:-0}
  (( exit_code != 0 )) || (( provider_error == 1 )) || return 1
  [[ -r $result_file ]] && grep -qiE 'usage limit|rate.?limit|too many requests|(^|[^0-9])429([^0-9]|$)|insufficient_quota|quota exceeded|credit balance' "$result_file" && return 0
  (( exit_code != 0 )) && [[ ! -s $result_file ]] && return 0
  return 1
}

agent_result_is_model_failure() {
  local result_file=$1
  [[ -r $result_file ]] && grep -qiE 'overloaded|model.*not found|unknown model|invalid model|model.*not supported' "$result_file"
}

# Backward-compatible alias: pool exhaustion is the terminal "exhausted"
# outcome (Issue re-queued; the supervisor pauses only when every pool is
# unavailable).
agent_result_is_exhausted() { agent_result_is_pool_exhausted "$@"; }


# Record that the provider budget is exhausted so the supervisor stops claiming
# until it likely recovers, instead of burning Issues into agent:failed. This
# global marker records the "every candidate pool is unavailable" case; per-pool
# exhaustion uses agent_mark_pool_exhausted.
agent_mark_exhausted() {
  mkdir -p "$STATE_ROOT"
  printf '%s\n' "$(( $(date +%s) + EXHAUSTION_PAUSE_SECONDS ))" > "$STATE_ROOT/agent-exhausted"
}


# Pause claiming while no pool is usable: a fresh global marker (written when a
# worker found no candidate at all), or every referenced pool currently
# exhausted. A pool recovers through usage measurement or its fixed cooldown, so
# a single spent pool never stops claiming -- the picker simply uses another
# pool. Announce transitions once to keep the supervisor log quiet.
exhaustion_note_pause() {
  local marker="$STATE_ROOT/agent-exhausted" resume now pool all_pools=1 global_active=0 pools=''
  if [[ -r $marker ]]; then
    read -r resume < "$marker" || true
    now=$(date +%s)
    if [[ $resume =~ ^[0-9]+$ ]] && (( now < resume )); then global_active=1; else rm -f "$marker"; fi
  fi
  pools=$(agent_all_pools)
  if [[ -n $pools ]]; then
    for pool in $pools; do
      agent_pool_exhausted "$pool" || all_pools=0
    done
  else
    all_pools=0
  fi
  if (( global_active == 1 || all_pools == 1 )); then
    if [[ ! -e $STATE_ROOT/all-pools-paused ]]; then
      mkdir -p "$STATE_ROOT"
      : > "$STATE_ROOT/all-pools-paused"
      say '全プールが利用不可のため、Issueのclaimを一時停止します。' >&2
      postmortem_consider_trigger resource-exhaustion all-pools '全provider poolが利用不可になり、Issueのclaimを一時停止した' || true
    fi
    return 1
  fi
  if [[ -e $STATE_ROOT/all-pools-paused ]]; then
    rm -f "$STATE_ROOT/all-pools-paused"
    say 'いずれかのプールで枠が回復したため、Issueのclaimを再開します。' >&2
  fi
  return 0
}

# Run one worker stage confined to the dedicated worktree. stage=plan only
# investigates and returns a plan (read-only for Codex); stage=exec implements
# it with workspace-write, granting only the resolved Git common directory and
# protected .agents directory. Never bypass secret-guard hooks. The final agent
# message is written to result_file, where the caller reads the plan text or the
# AGENTIC_LOOP_RESULT sentinel. The provider's exit status is preserved (usage
# extraction is best-effort and must not mask it): callers classify pool
# exhaustion from that status plus result_file contents. Providers that emit
# hard failures only on stderr (notably Codex usage-limit) leave an empty
# --output-last-message file; stderr is folded into result_file when empty so
# the pool/model matchers still see the text.
agent_run_stage() {
  local stage=$1 worktree=$2 git_common_dir=$3 agents_dir=$4 result_file=$5 usage_file=$6 prompt=$7 pool=$8 provider=$9 model=${10} effort=${11} raw_result stderr_file provider_rc=0
  raw_result="${result_file}.raw.$$"
  stderr_file="${result_file}.stderr.$$"
  # Structured "the provider reported a failure" flag, consumed by
  # agent_result_is_pool_exhausted so a SUCCESSFUL stage whose output merely
  # mentions rate limits/quota is never misread as the pool being spent.  A
  # non-zero provider exit always counts; Claude also exits zero on an API error
  # under --output-format json, so its is_error flag is folded in below.
  STAGE_PROVIDER_ERROR=0
  : > "$usage_file"
  : > "$result_file"
  : > "$stderr_file"
  printf 'provider=%s\nstage=%s\n' "$provider" "$stage" >> "$usage_file"
  [[ -n $pool ]] && printf 'pool=%s\n' "$pool" >> "$usage_file"
  [[ -n $model ]] && printf 'model=%s\n' "$model" >> "$usage_file"
  # Mark every provider invocation as an autonomous-loop run.  The Claude Code
  # edit-guard hook reads AGENTIC_LOOP_AGENT to distinguish this loop from an
  # interactive human session: an autonomous run may edit its own linked
  # worktree freely but is always blocked from touching the primary (main)
  # worktree.  Set inline so the marker reaches only the provider process, not
  # the rest of the worker.
  case $provider in
    claude)
      # --output-format json returns one object; the final message (plan text or
      # the AGENTIC_LOOP_RESULT sentinel) stays inside .result and is matched as
      # a substring, so the raw JSON is the result file. Token/cost fields are
      # extracted for the usage record without adding a jq dependency. Claude has
      # no OS sandbox, so the plan stage relies on the prompt to avoid writes.
      local -a claude_args=(--print --output-format json --dangerously-skip-permissions --add-dir "$git_common_dir" --add-dir "$agents_dir")
      [[ -n $model ]] && claude_args+=(--model "$model")
      (cd "$worktree" && AGENTIC_LOOP_AGENT=1 claude "${claude_args[@]}" "$prompt") > "$raw_result" 2>"$stderr_file" || provider_rc=$?
      agent_usage_from_claude_json "$raw_result" >> "$usage_file" || true
      # Claude exits zero even on an API failure under --output-format json and
      # reports it through is_error/api_error_status in the envelope, so read that
      # structured flag before extracting the plan text.  Without it, a
      # successful plan whose .result discusses rate limits/quota would be
      # misread as pool exhaustion; with it, only a real provider error is.
      local claude_is_error claude_api_error
      claude_is_error=$(yq -r '.is_error // false' "$raw_result" 2>/dev/null || printf 'false')
      claude_api_error=$(yq -r '.api_error_status // ""' "$raw_result" 2>/dev/null || printf '')
      if [[ $claude_is_error == true || ( -n $claude_api_error && $claude_api_error != null ) ]]; then
        STAGE_PROVIDER_ERROR=1
      fi
      # Claude's final assistant response is the JSON result field, not the
      # surrounding transport object.  On a provider error keep the whole
      # envelope so the classifier's quota signatures see api_error_status and
      # any error text, not just .result.
      if (( STAGE_PROVIDER_ERROR == 1 )); then
        cp "$raw_result" "$result_file"
      else
        yq -r '.result // ""' "$raw_result" > "$result_file" 2>/dev/null || cp "$raw_result" "$result_file"
      fi
      rm -f "$raw_result"
      ;;
    opencode)
      # opencode has no OS sandbox; --dir scopes work to the worktree and --auto
      # avoids interactive approval. The plan stage relies on the prompt to avoid
      # writes. --format json streams events; the final message (plan text or the
      # AGENTIC_LOOP_RESULT sentinel) stays in a text part and is matched as a
      # substring, and step-finish parts carry token/cost telemetry.
      local -a opencode_args=(run --auto --format json --dir "$worktree")
      [[ -n $model ]] && opencode_args+=(--model "$model")
      AGENTIC_LOOP_AGENT=1 opencode "${opencode_args[@]}" "$prompt" > "$raw_result" 2>"$stderr_file" || provider_rc=$?
      agent_usage_from_opencode_json "$raw_result" >> "$usage_file" || true
      # opencode streams several JSON events; only the last text part is the
      # final assistant response, while step-finish is telemetry.
      local event message parse_failed=0
      : > "$result_file"
      while IFS= read -r event; do
        message=$(printf '%s\n' "$event" | yq -r 'select(.part.type == "text") | .part.text // ""' -) || { parse_failed=1; break; }
        [[ -n $message ]] && printf '%s\n' "$message" > "$result_file"
      done < "$raw_result"
      (( parse_failed == 0 )) || cp "$raw_result" "$result_file"
      rm -f "$raw_result"
      ;;
    *)
      printf 'reasoning_effort=%s\n' "$effort" >> "$usage_file"
      local -a codex_args
      if [[ $stage == plan ]]; then
        codex_args=(exec --sandbox read-only -c 'approval_policy="never"')
      else
        codex_args=(exec --sandbox workspace-write --add-dir "$git_common_dir" --add-dir "$agents_dir" -c 'approval_policy="never"' -c 'sandbox_workspace_write.network_access=true')
      fi
      [[ -n $model ]] && codex_args+=(-c "model=$model")
      [[ -n $effort ]] && codex_args+=(-c "model_reasoning_effort=$effort")
      # Codex puts the assistant last message in --output-last-message; hard
      # failures such as usage limit go only to stderr with a non-zero exit.
      AGENTIC_LOOP_AGENT=1 codex "${codex_args[@]}" -C "$worktree" --output-last-message "$result_file" "$prompt" 2>"$stderr_file" || provider_rc=$?
      agent_usage_from_codex_sessions "$worktree" >> "$usage_file" || true
      ;;
  esac
  if [[ ! -s $result_file && -s $stderr_file ]]; then
    cp "$stderr_file" "$result_file"
  fi
  rm -f "$stderr_file"
  return "$provider_rc"
}


# Extract token and cost fields from a Claude --output-format json payload using
# grep only, so no jq dependency enters the pinned environment.
agent_usage_from_claude_json() {
  local file=$1 field key value
  [[ -r $file ]] || return 0
  for field in input_tokens:tokens_input output_tokens:tokens_output cache_read_input_tokens:tokens_cache_read; do
    key=${field%%:*}; value=$(grep -oE "\"$key\":[0-9]+" "$file" | head -n 1 | grep -oE '[0-9]+$' || true)
    [[ -n $value ]] && printf '%s=%s\n' "${field#*:}" "$value"
  done
  value=$(grep -oE '"total_cost_usd":[0-9.]+' "$file" | head -n 1 | grep -oE '[0-9.]+$' || true)
  [[ -n $value ]] && printf 'cost_usd=%s\n' "$value"
  return 0
}


# Best-effort token and rate-limit percentages for the Codex session that ran in
# this worktree. Codex writes token_count events to ~/.codex/sessions and the
# session log references the worktree path, so the newest matching rollout is
# this worker's. Missing data is skipped; usage logging never fails the worker.
agent_usage_from_codex_sessions() {
  local worktree=$1 sessions="${CODEX_HOME:-$HOME/.codex}/sessions" file line value
  [[ -d $sessions ]] || return 0
  file=$(grep -rlF -- "$worktree" "$sessions" 2>/dev/null | while IFS= read -r candidate; do
    printf '%s\t%s\n' "$(stat -c '%Y' "$candidate" 2>/dev/null || printf 0)" "$candidate"
  done | sort -rn | head -n 1 | cut -f2-)
  [[ -n $file && -r $file ]] || return 0
  line=$(grep -E '"(input_tokens|used_percent)"' "$file" | tail -n 1)
  [[ -n $line ]] || return 0
  for value in input_tokens:tokens_input output_tokens:tokens_output cached_input_tokens:tokens_cache_read reasoning_output_tokens:tokens_reasoning; do
    local number; number=$(grep -oE "\"${value%%:*}\":[0-9]+" <<< "$line" | head -n 1 | grep -oE '[0-9]+$' || true)
    [[ -n $number ]] && printf '%s=%s\n' "${value#*:}" "$number"
  done
  return 0
}


# Sum token and cost telemetry from an opencode --format json event stream. Each
# step-finish part carries .part.tokens.{input,output,reasoning,cache} and
# .part.cost; the final step may be missing (known opencode issue), so any total
# is best-effort and simply omitted when unavailable.
agent_usage_from_opencode_json() {
  local file=$1 sums ti to tr cost
  [[ -r $file ]] || return 0
  sums=$(yq -p json -o tsv 'select(.part.type == "step-finish") | [.part.tokens.input // 0, .part.tokens.output // 0, .part.tokens.reasoning // 0, .part.cost // 0] | @tsv' "$file" 2>/dev/null \
    | awk '{ i += $1; o += $2; r += $3; c += $4 } END { if (NR > 0) printf "%d\t%d\t%d\t%s", i, o, r, c }')
  [[ -n $sums ]] || return 0
  IFS=$'\t' read -r ti to tr cost <<< "$sums"
  ((ti > 0)) && printf 'tokens_input=%s\n' "$ti"
  ((to > 0)) && printf 'tokens_output=%s\n' "$to"
  ((tr > 0)) && printf 'tokens_reasoning=%s\n' "$tr"
  awk -v c="$cost" 'BEGIN { exit !(c > 0) }' && printf 'cost_usd=%s\n' "$cost"
  return 0
}


# Record one session's token usage on the Issue for later cost analysis. Fields
# absent from the provider output are simply omitted; posting never fails work.
agent_post_usage() {
  local issue=$1 worker=$2 usage_file=$3 exit_code=$4 seconds=$5 key value summary
  local -A usage=()
  [[ -r $usage_file ]] || return 0
  while IFS='=' read -r key value; do [[ -n $key ]] && usage[$key]=$value; done < "$usage_file"
  # stage/provider/seconds/exit are enum or numeric only (never free-form model
  # names or costs), so they are safe to embed in the marker itself; metrics
  # collection reads only the marker, never this comment's Japanese body, so
  # plan/exec duration must be reconstructable from the marker alone.
  summary="<!-- agentic-loop:usage worker=$worker stage=${usage[stage]:-unknown} provider=${usage[provider]:-unknown} seconds=$seconds exit=$exit_code -->\nToken使用量（分析用）: provider=${usage[provider]:-unknown}"
  [[ -n ${usage[stage]:-} ]] && summary+=" stage=${usage[stage]}"
  [[ -n ${usage[pool]:-} ]] && summary+=" pool=${usage[pool]}"
  [[ -n ${usage[model]:-} ]] && summary+=" model=${usage[model]}"
  [[ -n ${usage[reasoning_effort]:-} ]] && summary+=" reasoning_effort=${usage[reasoning_effort]}"
  [[ -n ${usage[tokens_input]:-} ]] && summary+=" 入力=${usage[tokens_input]}tok"
  [[ -n ${usage[tokens_output]:-} ]] && summary+=" 出力=${usage[tokens_output]}tok"
  [[ -n ${usage[tokens_cache_read]:-} ]] && summary+=" cache_read=${usage[tokens_cache_read]}tok"
  [[ -n ${usage[tokens_reasoning]:-} ]] && summary+=" 推論=${usage[tokens_reasoning]}tok"
  [[ -n ${usage[cost_usd]:-} ]] && summary+=" cost=\$${usage[cost_usd]}"
  summary+=" 所要=${seconds}s exit=${exit_code}"
  comment_issue "$issue" "$summary" || true
}


# Run one stage with automatic tier/pool/model fallback (Issue #155). Rounds
# across every candidate until one succeeds or the failure is classified; a
# pool-quota exhaustion marks that pool and continues to the next pool/model in
# the same stage (so a spent plus pool falls through to gogo without requeueing
# the Issue first), while a model-specific failure (e.g. overloaded) moves to
# the next candidate in the same pool first. Sets the globals STAGE_RC
# (0 normal / 1 pool exhausted with no remaining candidate / 2 no candidate
# usable at all / 3 every candidate failed with a model-specific error) and
# STAGE_EXIT_CODE (last provider exit; result_file keeps the last output).
run_stage_candidates() {
  local phase=$1 worktree=$2 git_common_dir=$3 agents_dir=$4 result_file=$5 usage_file=$6 prompt=$7
  local tried='' picked key max_tries tries pool_saw_exhausted=0
  max_tries=$(agent_candidate_count "$phase")
  (( max_tries >= 1 )) || max_tries=1
  tries=0
  while :; do
    STAGE_RC=0
    if ! picked=$(agent_pick_tier "$phase" "$tried"); then
      STAGE_EXIT_CODE=1
      if (( tries == 0 )); then STAGE_RC=2
      elif (( pool_saw_exhausted == 1 )); then STAGE_RC=1
      else STAGE_RC=3; fi
      return $STAGE_RC
    fi
    agent_candidate_load <<< "$picked"
    key="$CAND_TIER:$CAND_MODEL_IDX"
    tried+="${tried:+,}$key"
    tries=$((tries + 1))
    STAGE_EXIT_CODE=0
    STAGE_PROVIDER_ERROR=0
    agent_run_stage "$phase" "$worktree" "$git_common_dir" "$agents_dir" "$result_file" "$usage_file" "$prompt" \
      "$CAND_POOL" "$CAND_PROVIDER" "$CAND_MODEL" "$CAND_EFFORT" || STAGE_EXIT_CODE=$?
    if agent_result_is_pool_exhausted "$result_file" "$STAGE_EXIT_CODE" "$STAGE_PROVIDER_ERROR"; then
      LAST_EXHAUSTED_POOL=$CAND_POOL
      agent_mark_pool_exhausted "$CAND_POOL" "$result_file"
      pool_saw_exhausted=1
      say "プール枠枯渇のため次の候補へ切り替えます: pool=$CAND_POOL provider=$CAND_PROVIDER model=$CAND_MODEL" >&2
      continue
    fi
    if agent_result_is_model_failure "$result_file" "$STAGE_EXIT_CODE"; then
      if (( tries < max_tries )); then
        say "モデル固有の失敗のため次の候補へ切り替えます: pool=$CAND_POOL provider=$CAND_PROVIDER model=$CAND_MODEL" >&2
        continue
      fi
      STAGE_RC=3
      return 3
    fi
    # Non-zero exit that is neither pool exhaustion nor a model-specific
    # failure is still a stage failure: keep trying remaining candidates so a
    # transient provider crash does not skip the fallback chain, and only give
    # up once every candidate has been attempted.
    if (( STAGE_EXIT_CODE != 0 )); then
      if (( tries < max_tries )); then
        say "候補が非0終了したため次の候補へ切り替えます: pool=$CAND_POOL provider=$CAND_PROVIDER model=$CAND_MODEL exit=$STAGE_EXIT_CODE" >&2
        continue
      fi
      STAGE_RC=3
      return 3
    fi
    # A genuine success is real, positive evidence the pool works right now --
    # reset its backoff streak so a later, unrelated exhaustion starts from the
    # short default pause again instead of continuing to grow (Issue #158).
    agent_pool_streak_clear "$CAND_POOL"
    return 0
  done
}
