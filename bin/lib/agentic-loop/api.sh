# Module: api.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



repo_api() {
  local endpoint=$1 attempt=0 delay=$API_RETRY_BASE_SECONDS rc output error input='' index api_endpoint
  shift
  local -a args=("$@")
  mkdir -p "$STATE_ROOT"
  output="$STATE_ROOT/api-output.$$"; error="$STATE_ROOT/api-error.$$"
  api_endpoint="repos/$(repo_name)"
  [[ -z $endpoint ]] || api_endpoint+="/$endpoint"
  for ((index=0; index<${#args[@]}; index++)); do
    if [[ ${args[$index]} == --input && ${args[$((index + 1))]:-} == - ]]; then
      input="$STATE_ROOT/api-input.$$"; cat > "$input"; args[index + 1]=$input; break
    fi
  done
  while :; do
    if gh api "$api_endpoint" "${args[@]}" > "$output" 2> "$error"; then
      cat "$output"; rm -f "$output" "$error" "$input"; return 0
    else
      rc=$?
    fi
    attempt=$((attempt + 1))
    if (( attempt >= API_RETRY_ATTEMPTS )) || ! grep -Eqi 'rate limit|secondary rate|HTTP (429|5[0-9][0-9])|timed? out|timeout|connection reset|temporary failure|service unavailable|bad gateway' "$error"; then
      cat "$error" >&2; rm -f "$output" "$error" "$input"; return "$rc"
    fi
    say "GitHub REST APIの一時障害を再試行します（attempt=$attempt/$API_RETRY_ATTEMPTS, wait=${delay}s）。" >&2
    sleep "$delay"
    delay=$((delay * 2))
  done
}


# The only two places an Issue/PR comment body reaches GitHub (Issue #110):
# every other module posts/updates a comment through these, never through
# repo_api directly, so unfold_body's `\n` expansion happens exactly once, at
# the write boundary. Extra args (e.g. --jq) are passed through untouched.
comment_post() { repo_api "issues/$1/comments" --method POST -f body="$(unfold_body "$2")" "${@:3}"; }
comment_patch() { repo_api "issues/comments/$1" --method PATCH -f body="$(unfold_body "$2")" "${@:3}"; }


graphql_budget_file() { printf '%s/graphql-rate-limit' "$STATE_ROOT"; }


# One cached snapshot of GitHub API budget backs both the GraphQL guard (Projects
# writes) and the REST(core) guard (claiming). rate_limit does not itself consume
# core quota, and caching for RATE_LIMIT_CACHE_SECONDS keeps the poll cheap.
# Stored as: fetched<TAB>graphql_remaining<TAB>graphql_reset<TAB>core_remaining.
refresh_graphql_budget() {
  local snapshot remaining reset core now tmp
  mkdir -p "$STATE_ROOT"
  snapshot=$(gh api rate_limit --jq '[.resources.graphql.remaining, .resources.graphql.reset, .resources.core.remaining] | @tsv' 2>/dev/null) || return 1
  IFS=$'\t' read -r remaining reset core <<< "$snapshot"
  [[ $remaining =~ ^[0-9]+$ && $reset =~ ^[0-9]+$ && $core =~ ^[0-9]+$ ]] || return 1
  now=$(date +%s)
  tmp="$(graphql_budget_file).$$"
  printf '%s\t%s\t%s\t%s\n' "$now" "$remaining" "$reset" "$core" > "$tmp"
  mv "$tmp" "$(graphql_budget_file)"
}


read_graphql_budget() {
  local fetched remaining reset core now
  if [[ -r $(graphql_budget_file) ]]; then
    IFS=$'\t' read -r fetched remaining reset core < "$(graphql_budget_file)" || true
  fi
  now=$(date +%s)
  if [[ ! ${fetched:-} =~ ^[0-9]+$ || ! ${remaining:-} =~ ^[0-9]+$ || ! ${reset:-} =~ ^[0-9]+$ || ! ${core:-} =~ ^[0-9]+$ || $((now - fetched)) -ge $RATE_LIMIT_CACHE_SECONDS ]]; then
    refresh_graphql_budget || return 1
    IFS=$'\t' read -r fetched remaining reset core < "$(graphql_budget_file)"
  fi
  printf '%s\t%s\t%s\n' "$remaining" "$reset" "$core"
}


graphql_budget_allows() {
  local minimum=${1:-1} budget remaining reset core
  budget=$(read_graphql_budget) || return 1
  IFS=$'\t' read -r remaining reset core <<< "$budget"
  (( remaining >= minimum ))
}


# Whether the remaining REST(core) quota is at least `minimum`. Claiming and its
# worker burn core quota (labels, comments, PR calls), so this gates claiming to
# keep a reserve for heartbeats, recovery and in-flight writes. Fails open (the
# caller treats an unreadable budget as "allowed") to avoid stalling on API blips.
core_budget_allows() {
  local minimum=${1:-1} budget remaining reset core
  budget=$(read_graphql_budget) || return 0
  IFS=$'\t' read -r remaining reset core <<< "$budget"
  [[ $core =~ ^[0-9]+$ ]] || return 0
  (( core >= minimum ))
}


preflight() {
  local provider
  for provider in $(agent_used_providers); do
    provider_valid "$provider" || fail "unsupported AI provider: $provider (use codex, claude, or opencode)"
    require_command "$(provider_command "$provider")"
  done
  require_command git
  require_command gh
  require_command yq
  require_command devbox
  git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1 || fail 'target is not a Git repository'
  git -C "$REPO_ROOT" remote get-url origin >/dev/null 2>&1 || fail 'origin remote is required'
  gh auth status >/dev/null 2>&1 || fail 'GitHub authentication is required; run gh auth login'
  if [[ ${AGENTIC_LOOP_INSTALL:-0} == 1 ]]; then
    : # install-target preflight already checked Projects access
  elif graphql_budget_allows 1; then
    gh api graphql -f query='query { viewer { login projectsV2(first: 1) { totalCount } } }' >/dev/null 2>&1 ||
      fail 'GitHub token needs repository access and project/read:project scopes; run gh auth refresh -s project,read:project'
  else
    say 'GraphQL rate limitが枯渇しているためProjects権限検査を延期します。Issueキューはreset後に再開します。' >&2
  fi
  repo_name >/dev/null || fail 'cannot access the target GitHub repository'
  for provider in $(agent_used_providers); do
    provider_ready "$provider" || fail "$provider CLI with non-interactive worker support is required"
  done
  validate_config
}
