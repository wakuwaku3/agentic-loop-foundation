# Module: scope.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



# --- Change-scope conflict avoidance (see docs/operations/issue-queue.md) ---
# An Issue may declare the paths and environments it touches with a marker
# comment `<!-- agentic-loop:scope paths=a,b env=c -->` in its body (the same
# HTML-comment convention as agentic-loop:lease). claim_next skips a queued
# Issue whose declared scope overlaps a running Issue's effective scope, and
# the worker itself refines the running Issue's cached scope from its plan
# output and, later, from the measured Git diff — so an initial under-estimate
# only ever grows into a safer (more conservative) scope, never shrinks it.
scope_cache_dir() { printf '%s/scope' "$STATE_ROOT"; }

scope_cache_file() { printf '%s/scope/issue-%s' "$STATE_ROOT" "$1"; }

scope_cache_write() { mkdir -p "$(scope_cache_dir)"; printf '%s' "$2" > "$(scope_cache_file "$1")"; }

scope_cache_read() { [[ -r $(scope_cache_file "$1") ]] && cat "$(scope_cache_file "$1")" || true; }

scope_cache_clear() { rm -f "$(scope_cache_file "$1")"; }


# Normalize a comma-separated raw declaration (bare path tokens plus env names
# already prefixed with "env:") into one canonical token per line: "*" (whole
# repository), "env:NAME", or "path:VALUE". Invalid characters, empty tokens,
# and a bare "." or "/" degrade to fewer or no tokens rather than failing the
# worker, since an incomplete declaration must still be safe (see
# resolve_issue_scope for the unknown-scope fallback).
scope_tokens_normalize() {
  local raw=$1 token trimmed
  local -a parts result=()
  IFS=',' read -ra parts <<< "$raw"
  for token in "${parts[@]}"; do
    trimmed=$(sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' <<< "$token")
    [[ -n $trimmed ]] || continue
    [[ $trimmed =~ ^[A-Za-z0-9._/*:-]+$ ]] || continue
    if [[ $trimmed == '*' ]]; then
      result+=('*')
    elif [[ $trimmed == env:* ]]; then
      [[ ${#trimmed} -gt 4 ]] && result+=("$trimmed")
    else
      trimmed=${trimmed%%\**}
      trimmed=${trimmed%/}
      [[ -z $trimmed || $trimmed == '.' ]] && trimmed='*'
      result+=("path:$trimmed")
    fi
  done
  (( ${#result[@]} == 0 )) && return 0
  printf '%s\n' "${result[@]}" | sort -u
}


# Extract the last agentic-loop:scope marker from an Issue body or comment and
# return its tokens as one bare (pre-normalization) comma-joined string:
# path tokens verbatim, env= names prefixed with "env:".
scope_marker_from_body() {
  local body=$1 marker paths env_val combined e
  local -a envs
  marker=$(grep -oE 'agentic-loop:scope[^>]*' <<< "$body" | tail -n 1) || true
  [[ -n $marker ]] || return 0
  paths=$(grep -oE '(^|[[:space:]])paths=[^[:space:]]*' <<< "$marker" | tail -n 1); paths=${paths#*paths=}
  env_val=$(grep -oE '(^|[[:space:]])env=[^[:space:]]*' <<< "$marker" | tail -n 1); env_val=${env_val#*env=}
  combined=$paths
  if [[ -n $env_val ]]; then
    IFS=',' read -ra envs <<< "$env_val"
    for e in "${envs[@]}"; do [[ -n $e ]] && combined+="${combined:+,}env:$e"; done
  fi
  printf '%s\n' "$combined"
}


# Escalate a normalized token set to "*" when it overlaps a configured
# exclusive_paths entry (shared infrastructure that must serialize with
# everything, even Issues that only declare a narrower scope).
scope_apply_exclusive_paths() {
  local tokens=$1 excl t e pt pe
  [[ -n $tokens ]] || return 0
  if [[ -n $EXCLUSIVE_PATHS ]]; then
    excl=$(scope_tokens_normalize "$EXCLUSIVE_PATHS")
    while IFS= read -r t; do
      [[ $t == path:* ]] || continue
      pt=${t#path:}
      while IFS= read -r e; do
        [[ $e == path:* ]] || continue
        pe=${e#path:}
        if [[ $pt == "$pe" || $pt == "$pe"/* || $pe == "$pt"/* ]]; then printf '*\n'; return 0; fi
      done <<< "$excl"
    done <<< "$tokens"
  fi
  printf '%s\n' "$tokens"
}


# Resolve an Issue's effective scope tokens from its body, falling back to
# UNKNOWN_SCOPE when no valid marker is present: open scope resolves to no
# tokens at all (this Issue is never blocked and never blocks anyone),
# exclusive resolves to "*" (touches the whole repository), and the default
# isolated resolves to the "unknown" sentinel, which only conflicts with other
# unresolved Issues so declared, independent scopes stay unaffected.
resolve_issue_scope() {
  local body=$1 raw tokens
  raw=$(scope_marker_from_body "$body")
  [[ -n $raw ]] && tokens=$(scope_tokens_normalize "$raw")
  if [[ -n ${tokens:-} ]]; then
    scope_apply_exclusive_paths "$tokens"
    return 0
  fi
  case $UNKNOWN_SCOPE in
    open) return 0 ;;
    exclusive) printf '*\n' ;;
    *) printf 'unknown\n' ;;
  esac
}


# Print the first token shared between two normalized token sets (one per
# line), or nothing if they do not overlap. "*" overlaps everything; two
# "unknown" sentinels overlap each other; env: tokens overlap only on an exact
# name match; path tokens overlap when one is a "/"-boundary prefix of the
# other (or equal). The shorter of the two overlapping paths is reported since
# it names the shared (blocking) scope.
scope_overlap_token() {
  local -a a b
  local ta tb pa pb
  readarray -t a <<< "$1"
  readarray -t b <<< "$2"
  for ta in "${a[@]}"; do
    [[ -n $ta ]] || continue
    for tb in "${b[@]}"; do
      [[ -n $tb ]] || continue
      if [[ $ta == '*' || $tb == '*' ]]; then printf '*\n'; return 0; fi
      if [[ $ta == unknown && $tb == unknown ]]; then printf 'unknown\n'; return 0; fi
      if [[ $ta == env:* && $ta == "$tb" ]]; then printf '%s\n' "$ta"; return 0; fi
      if [[ $ta == path:* && $tb == path:* ]]; then
        pa=${ta#path:}; pb=${tb#path:}
        if [[ $pa == "$pb" || $pb == "$pa"/* || $pa == "$pb"/* ]]; then
          if [[ ${#pa} -le ${#pb} ]]; then printf '%s\n' "$pa"; else printf '%s\n' "$pb"; fi
          return 0
        fi
      fi
    done
  done
  return 1
}

scope_conflicts() { scope_overlap_token "$1" "$2" >/dev/null; }


# The number of a currently cached (running) Issue whose scope conflicts with
# the given tokens, or nothing if none does.
scope_conflicting_issue() {
  local tokens=$1 file other
  shopt -s nullglob
  for file in "$(scope_cache_dir)"/issue-*; do
    other=${file##*/issue-}
    if scope_conflicts "$tokens" "$(cat "$file" 2>/dev/null || true)"; then
      printf '%s\n' "$other"
      shopt -u nullglob
      return 0
    fi
  done
  shopt -u nullglob
  return 1
}


conflict_wait_file() { printf '%s/conflict/issue-%s' "$STATE_ROOT" "$1"; }


# Record that an Issue is waiting for another Issue's change-scope to stop
# overlapping, and comment only on the transition into (or a change of) the
# wait, so a 30-second poll does not repost the same notice every cycle.
record_conflict_wait() {
  local issue=$1 other=$2 tokens=$3 file reason note
  reason=$(scope_overlap_token "$tokens" "$(scope_cache_read "$other")") || reason=unknown
  file=$(conflict_wait_file "$issue")
  note=$(printf '%s\t%s' "$other" "$reason")
  [[ -r $file && $(cat "$file") == "$note" ]] && return 0
  mkdir -p "$(dirname "$file")"
  printf '%s\n' "$note" > "$file"
  comment_issue "$issue" "<!-- agentic-loop:scope-conflict issue=$other token=$reason -->\n#$other と変更scopeが重なる（重複: $reason）ため、claimを待機します。相手Issueが完了するか、scopeが重ならなくなれば自動的にclaimされます。" || true
  project_sync_conflict "$issue" "#$other scope重複: $reason"
}


# Clear a conflict-wait once it no longer applies, commenting only when a wait
# was actually recorded (idempotent for Issues that never conflicted).
clear_conflict_wait() {
  local issue=$1 file
  file=$(conflict_wait_file "$issue")
  [[ -r $file ]] || return 0
  rm -f "$file"
  comment_issue "$issue" '<!-- agentic-loop:scope-resolved -->\n変更scopeの重複が解消したため、claim待機を解除しました。' || true
  project_sync_conflict "$issue" ''
}


# Reconcile machine-local scope/conflict caches with the successful Open Issue
# snapshot on every poll. GitHub Labels are the queue source of truth: a scope
# cache for a non-running Issue must never serialize queued work, and a wait
# file for a non-queued Issue or non-running counterpart is stale. This is
# local-only and therefore cannot delete another host's worker artifacts.
reconcile_scope_conflict_cache() {
  [[ -n $SUPERVISOR_SNAPSHOT && -r $SUPERVISOR_SNAPSHOT ]] || return 0
  local file issue other
  shopt -s nullglob
  for file in "$(scope_cache_dir)"/issue-*; do
    issue=${file##*/issue-}
    snapshot_issue_has_state "$issue" running || rm -f "$file"
  done
  for file in "$STATE_ROOT"/conflict/issue-*; do
    issue=${file##*/issue-}
    IFS=$'\t' read -r other _ < "$file" || { rm -f "$file"; continue; }
    if ! snapshot_issue_has_state "$issue" queued; then
      rm -f "$file"
      project_sync_conflict "$issue" '' || true
    elif ! snapshot_issue_has_state "$other" running; then
      clear_conflict_wait "$issue"
    fi
  done
  shopt -u nullglob
}


# Rebuild the running-Issue scope cache from GitHub at supervisor startup,
# since the local cache lives under Git-external state and may not survive a
# restart. Bounded by the number of currently running Issues.
# Whether a GitHub agent:running Issue is genuinely in progress — a live local
# worker, or a still-valid lease (from any machine) — rather than a phantom that
# GitHub's REST reflection lag still reports as running immediately after a
# requeue. Mirrors recover_expired's liveness test so the two agree.
issue_genuinely_running() {
  local issue=$1 body expires now since
  worker_pid_live "$issue" && return 0
  now=$(date +%s)
  since=$(recent_lease_since "$now")
  body=$(repo_api "issues/$issue/comments" --method GET -f since="$since" -f per_page=100 --paginate --jq '[.[].body | select(contains("agentic-loop:lease"))] | last // ""' 2>/dev/null | tail -n 1 || true)
  expires=$(printf '%s\n' "$body" | sed -n 's/.*expires=\([0-9][0-9]*\).*/\1/p' | head -n 1)
  [[ -n $expires && $expires -ge $now ]]
}


# Re-derive the effective scope of genuinely-running Issues at each Supervisor
# startup so a worker's refined scope marker is honored. Skips REST-lag phantoms
# (a just-requeued Issue still reported as running but with an expired lease):
# caching their scope would conflict with every undeclared ("unknown") Issue and
# deadlock the whole queue (see docs/decisions/0003).
rebuild_scope_cache() {
  local issue body
  rm -rf "$(scope_cache_dir)"
  while IFS=$'\t' read -r issue body; do
    [[ -n $issue ]] || continue
    issue_genuinely_running "$issue" || continue
    body=$(base64 -d <<< "$body" 2>/dev/null || true)
    scope_cache_write "$issue" "$(resolve_issue_scope "$body")"
  done < <(repo_api issues --method GET -f state=open -f labels="$(state_label running)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | [.number, (.body // "" | @base64)] | @tsv' 2>/dev/null || true)
}
