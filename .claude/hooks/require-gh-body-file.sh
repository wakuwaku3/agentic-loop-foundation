#!/usr/bin/env bash
# Gate `gh issue`/`gh pr` Bash calls that pass an unexpanded `gh --body
# "@path"` reference (Issue #272): unlike `--body-file`, `gh --body`/`-b`
# never reads the file -- the literal string `@path/to/file.md` becomes the
# whole Issue/PR body and the intended requirement is silently lost.
#
# This hook is deliberately FAIL-OPEN: Bash is this loop's most common tool,
# so any parse failure (stdin, JSON, tokenizing) must fall through to `exit
# 0` (allow) rather than deny -- a fail-closed Bash gate would stall the
# whole loop on the first malformed event. Contrast confirm-main-worktree-
# edit.sh (Edit/Write/NotebookEdit), which fails closed because a missed
# guard there risks an accidental main-worktree edit, a much rarer tool call.
set -uo pipefail

# --- Restore the loop's pinned toolchain PATH (yq) --------------------------
# Mirrors confirm-main-worktree-edit.sh's derivation (see Issue #160): resolve
# the common dir from this script's own location without any external tool,
# so it also works from a linked worktree where `.git` is a gitdir FILE.
hook_src=${BASH_SOURCE[0]}
hook_dir=${hook_src%/*}
project_dir=${hook_dir%/.claude/hooks}
resolve_common_dir() {
  local marker="$1/.git" line gitdir rel
  if [[ -d $marker ]]; then
    printf '%s\n' "$marker"
    return 0
  fi
  if [[ -f $marker ]]; then
    IFS= read -r line < "$marker" || return 1
    gitdir=${line#gitdir: }
    [[ -n $gitdir && $gitdir != "$line" ]] || return 1
    if [[ -r $gitdir/commondir ]]; then
      IFS= read -r rel < "$gitdir/commondir" || rel=''
      case $rel in
        /*) printf '%s\n' "$rel" ;;
        '') printf '%s\n' "$gitdir" ;;
        *) printf '%s\n' "$gitdir/$rel" ;;
      esac
      return 0
    fi
    printf '%s\n' "${gitdir%/worktrees/*}"
    return 0
  fi
  return 1
}
if common_dir=$(resolve_common_dir "$project_dir" 2>/dev/null); then
  runtime_path_file="$common_dir/agentic-loop/runtime.path"
  if [[ -r $runtime_path_file ]]; then
    IFS= read -r runtime_path < "$runtime_path_file" || runtime_path=''
    [[ -n $runtime_path ]] && PATH="$runtime_path:$PATH"
  fi
fi
export PATH

deny() {
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"%s"}}\n' "$1"
  exit 0
}

# Same predicate as body_unexpanded_file_reference (bin/lib/agentic-loop/
# common.sh), duplicated here because this hook runs outside that sourcing
# context. tests/test-agentic-loop.sh cross-checks both implementations
# against one fixture table so they cannot silently drift apart.
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

payload=$(cat 2>/dev/null) || exit 0
[[ -n $payload ]] || exit 0

tool_name=''
if command -v yq >/dev/null 2>&1; then
  tool_name=$(printf '%s' "$payload" | yq -p json -r '.tool_name // ""' - 2>/dev/null) || exit 0
fi
[[ $tool_name == Bash ]] || exit 0

command_str=''
if command -v yq >/dev/null 2>&1; then
  command_str=$(printf '%s' "$payload" | yq -p json -r '.tool_input.command // ""' - 2>/dev/null) || exit 0
fi
[[ -n $command_str ]] || exit 0

# A single-line-ish string is enough for this best-effort scan; a `gh`
# invocation with an embedded newline in the value is not our concern here.
[[ $command_str == *' gh '* || $command_str == gh\ * ]] || exit 0
[[ $command_str == *issue* || $command_str == *pr* ]] || exit 0

# --body-file (any spelling) is always safe: never deny a call that uses it,
# even if the same command line also happens to contain a `--body`/`-b`
# token (e.g. in a comment or an unrelated argument).
[[ $command_str != *--body-file* ]] || exit 0

# Scan for `--body VALUE` / `--body=VALUE` / `-b VALUE` occurrences with a
# plain regex, NOT a real shell tokenizer: this deliberately never `eval`s
# or otherwise re-interprets attacker/agent-controlled command text (that
# would defeat the point of a permission hook). A miss here just means a
# denial opportunity is skipped -- fail-open, not a correctness guarantee.
rest=$command_str
while [[ $rest =~ (--body=|--body[[:space:]]+|-b[[:space:]]+)([@\"\047][^[:space:]]*|[^[:space:]\"\047]+) ]]; do
  # Capture BASH_REMATCH before calling anything that runs its own `[[ =~
  # ]]` (body_unexpanded_file_reference does): a nested regex test
  # overwrites BASH_REMATCH, so reading it after that call would see the
  # inner match instead of this loop's.
  value=${BASH_REMATCH[2]}
  matched=${BASH_REMATCH[0]}
  value=${value#\"}; value=${value#\'}
  value=${value%\"}; value=${value%\'}
  if body_unexpanded_file_reference "$value"; then
    deny 'gh --body に @path 形式の値が渡されています。--body-file PATH を使ってください（--body/-b の @path は展開されず本文が失われます）。'
  fi
  # Advance past this match (literal removal, not regex re-application) so
  # a command with multiple gh calls/values is scanned to the end.
  rest=${rest#*"$matched"}
done

exit 0
