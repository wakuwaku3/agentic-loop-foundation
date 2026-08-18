# Module: common.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



say() { printf '%s\n' "$*"; }

fail() { printf '%s: %s\n' "$PROGRAM_NAME" "$*" >&2; exit 1; }

require_command() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required"; }

repo_name() {
  local url slug
  if [[ -r $STATE_ROOT/repository ]]; then
    read -r slug < "$STATE_ROOT/repository"
    [[ $slug =~ ^[^/]+/[^/]+$ ]] && { printf '%s\n' "$slug"; return 0; }
  fi
  url=$(git -C "$REPO_ROOT" remote get-url origin 2>/dev/null) || return 1
  case $url in
    https://github.com/*) slug=${url#https://github.com/} ;;
    ssh://git@github.com/*) slug=${url#ssh://git@github.com/} ;;
    git@github.com:*) slug=${url#git@github.com:} ;;
    *) slug=$(gh repo view --json nameWithOwner --jq .nameWithOwner) || return 1 ;;
  esac
  slug=${slug%.git}; slug=${slug%/}
  [[ $slug =~ ^[^/]+/[^/]+$ ]] || return 1
  mkdir -p "$STATE_ROOT"
  printf '%s\n' "$slug" > "$STATE_ROOT/repository"
  printf '%s\n' "$slug"
}


repo_issue_url() {
  local issue=$1 remote host path
  remote=$(git -C "$REPO_ROOT" remote get-url origin 2>/dev/null || true)
  case $remote in
    https://*/*) host=${remote#https://}; host=${host%%/*}; path=${remote#https://"$host"/} ;;
    ssh://git@*/*) host=${remote#ssh://git@}; host=${host%%/*}; path=${remote#ssh://git@"$host"/} ;;
    git@*:*) host=${remote#git@}; host=${host%%:*}; path=${remote#git@"$host":} ;;
    *) host=github.com; path=$(repo_name) ;;
  esac
  path=${path%.git}; path=${path%/}
  printf 'https://%s/%s/issues/%s\n' "$host" "$path" "$issue"
}

state_label() { printf 'agent:%s' "$1"; }


# Escape a value for embedding in a hand-built JSON string. Shared by every
# --format json output (doctor, status) so all of them agree on one escaping
# rule (jq is not a dependency of this script).
json_escape() {
  local value=$1
  value=${value//\\/\\\\}; value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}; value=${value//$'\r'/\\r}; value=${value//$'\t'/\\t}
  printf '%s' "$value"
}


# Inverse of json_escape's newline direction, and the only transform
# comment_post/comment_patch (api.sh) apply to a comment body: expand a
# literal two-character `\n` (never a real newline) into one. Every
# agentic-loop:* comment body in this codebase is written with that `\n`
# shorthand for readability (see Issue #110); this is the single point where
# the shorthand becomes the real newline GitHub renders.
unfold_body() { printf '%s' "${1//\\n/$'\n'}"; }
