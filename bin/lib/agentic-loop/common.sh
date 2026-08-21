# Module: common.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



say() { printf '%s\n' "$*"; }

fail() { printf '%s: %s\n' "$PROGRAM_NAME" "$*" >&2; exit 1; }

# A process argv contains the path spelling used at startup (relative,
# absolute, worktree, or symlink).  Its canonical Git common directory is the
# repository identity that remains stable across those spellings.
process_repo_matches() {
  local pid=$1 process_cwd process_common expected_common
  process_cwd=$(readlink -f "/proc/$pid/cwd" 2>/dev/null) || return 1
  process_common=$(git -C "$process_cwd" rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || return 1
  expected_common=$(readlink -f "${STATE_ROOT%/agentic-loop}") || return 1
  [[ $process_common == "$expected_common" ]]
}

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


# Whether `body` (an Issue or comment body, already decoded) is nothing but
# an unexpanded `gh --body "@path/to/file"` reference: `gh --body` (unlike
# `--body-file`) never reads the file, so the requirement it was meant to
# carry is silently lost and the literal `@path` string becomes the whole
# body (Issue #272). True only when the WHOLE body (after trimming
# surrounding whitespace) is a single line starting with `@`, the part after
# `@` has no whitespace, and it looks like a path (contains `/` or ends in a
# `.extension`) -- so an ordinary body, a multi-line body that merely
# mentions `@path` somewhere, a `@mention` reply such as `@wakuwaku3 ご確認
# ください`, and a bare `@name` with no path shape are all negatives.
body_unexpanded_file_reference() {
  local body=$1
  body="${body#"${body%%[![:space:]]*}"}"
  body="${body%"${body##*[![:space:]]}"}"
  [[ -n $body ]] || return 1
  [[ $body != *$'\n'* ]] || return 1
  [[ $body == @* ]] || return 1
  local rest=${body#@}
  [[ -n $rest ]] || return 1
  [[ $rest != *[[:space:]]* ]] || return 1
  [[ $rest == */* || $rest =~ \.[A-Za-z0-9]+$ ]] || return 1
  return 0
}


# Move a queued Issue whose body is nothing but an unexpanded `gh --body
# "@path"` reference (body_unexpanded_file_reference) to agent:needs-input
# before claim_next ever hands it to a worker, mirroring mark_dependency_
# blocked's shape (dependency.sh). Idempotent via a state marker file so a
# later poll does not repost the same explanatory comment. Never closes the
# Issue (see docs/decisions/0016-failure-park-not-close.md): a human must
# re-post the real body and re-queue it.
mark_body_unexpanded() {
  local issue=$1 file
  file="$STATE_ROOT/body-unexpanded/issue-$issue"
  [[ -e $file ]] && return 0
  mkdir -p "$STATE_ROOT/body-unexpanded"
  : > "$file"
  set_issue_state "$issue" needs-input
  project_sync_state "$issue" needs-input || true
  comment_issue "$issue" "<!-- agentic-loop:body-unexpanded -->\nIssue本文が単一行のファイル参照のみで、要求が失われているため claim を保留しました。\`gh --body \"@path\"\` は展開されません。\`gh issue edit $issue --body-file <正しい本文のファイル>\` で本文を再投稿し、その後 \`agent:queued\` Labelを再付与してください。" || true
}
