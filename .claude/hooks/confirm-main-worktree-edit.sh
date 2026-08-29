#!/usr/bin/env bash
# Gate Claude's file-edit tools by the queue-first policy.
#
# The loop's normal path is: Issue -> supervisor -> a worker editing its OWN
# linked worktree.  Direct human edits, and any autonomous edit of the primary
# (main) worktree, are exceptions -- allowed only when a human has explicitly
# opened an escape hatch for that specific worktree.  Decisions for a TRACKED
# target file:
#
#   * autonomous agent (AGENTIC_LOOP_AGENT set), linked worktree -> allow (its job)
#   * autonomous agent, primary (main) worktree                  -> deny  (accident guard)
#   * human, escape-hatch flag present for the target worktree   -> allow
#   * human, primary worktree, no flag                           -> deny  (-> work in a worktree)
#   * human, linked worktree, no flag                            -> deny  (-> file an Issue)
#
# The escape hatch is a throwaway flag file named `agentic-loop-allow-edit` in
# the target worktree's own gitdir: `touch` it to open, remove it when done.
# Living in the per-worktree gitdir scopes it to exactly one worktree -- the
# primary's flag opens main; a linked worktree's flag opens only that worktree.
#
# Only tracked files are gated; scratch/untracked files pass through untouched.
# This is a confirmation/redirection gate, never a silent block: every deny
# explains the sanctioned path forward.
set -euo pipefail

flag_basename='agentic-loop-allow-edit'

# --- Restore the loop's pinned toolchain PATH (yq, git) -----------------------
# This hook runs in contexts (Claude Code PreToolUse, login shells) whose PATH
# may omit the Nix-pinned bins.  Without yq the JSON parse below fails.  The
# pinned PATH lives at <git-common-dir>/agentic-loop/runtime.path.  Resolve the
# common dir from this script's own location WITHOUT any external tool, so it
# also works from a linked worktree -- there `.git` is a gitdir FILE, not a
# directory, which is exactly why the previous derivation fail-closed and gated
# every linked-worktree edit.  See Issue #160.
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
if common_dir=$(resolve_common_dir "$project_dir"); then
  runtime_path_file="$common_dir/agentic-loop/runtime.path"
  if [[ -r $runtime_path_file ]]; then
    IFS= read -r runtime_path < "$runtime_path_file" || runtime_path=''
    [[ -n $runtime_path ]] && PATH="$runtime_path:$PATH"
  fi
fi
export PATH

# --- Decision emitters --------------------------------------------------------
# Reasons are single-line and free of `"`/`\` so the hand-built JSON stays valid
# without a serializer (which may be unavailable on the fail-closed path).
deny() {
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"%s"}}\n' "$1"
  exit 0
}

# A malformed event or missing toolchain must not silently become an unguarded
# edit: fail closed with a deny that points at the escape hatch.
payload=$(cat) || deny 'PreToolUseイベントを読み取れませんでした。編集は保留しました。意図的な直接編集なら対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'

tool_name=$(printf '%s' "$payload" | yq -p json -r '.tool_name // ""' - 2>/dev/null) || deny 'イベント解析に失敗したため編集を保留しました。対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'
case $tool_name in
  Edit|Write) path_query='.tool_input.file_path // ""' ;;
  NotebookEdit) path_query='.tool_input.notebook_path // ""' ;;
  Bash)
    # Bash has no structured path. Inspect only explicit write primitives and
    # use the final operand as a conservative candidate.
    command=$(printf '%s' "$payload" | yq -p json -r '.tool_input.command // ""' 2>/dev/null) || exit 0
    [[ -n $command && $command != *$'\n'* && $command != *$'\r'* ]] || exit 0
    case $command in
      *'sed '*'-i'*|*'perl '*'-i'*) target_path=$(printf '%s\n' "$command" | awk '{print $NF}') ;;
      *'tee '*|*'tee -a '*) target_path=$(printf '%s\n' "$command" | sed -E 's/.*tee[[:space:]]+(-a[[:space:]]+)?([^[:space:];|&]+).*/\2/') ;;
      *'>'*) target_path=$(printf '%s\n' "$command" | sed -E 's/.*>{1,2}[[:space:]]*([^[:space:];|&]+).*/\1/') ;;
      *' cp '*|cp\ *|*' mv '*|mv\ *|*' install '*) target_path=$(printf '%s\n' "$command" | awk '{print $NF}') ;;
      *) exit 0 ;;
    esac
    ;;
  *) exit 0 ;;
esac
if [[ -n ${path_query:-} ]]; then
  target_path=$(printf '%s' "$payload" | yq -p json -r "$path_query" 2>/dev/null) || deny 'イベント解析に失敗したため編集を保留しました。対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'
fi
cwd=$(printf '%s' "$payload" | yq -p json -r '.cwd // ""' - 2>/dev/null) || deny 'イベント解析に失敗したため編集を保留しました。対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'

# Shell operands are commonly relative to the Bash event's cwd.
if [[ $tool_name == Bash && $target_path != /* ]]; then
  target_path="$cwd/$target_path"
fi

# Reject data that would make the checks below unreliable.
[[ -n $target_path && -n $cwd && $target_path != *$'\n'* && $target_path != *$'\r'* && $cwd != *$'\n'* && $cwd != *$'\r'* ]] \
  || deny 'ファイルパスを検証できなかったため編集を保留しました。対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'
[[ $target_path == /* && -d $cwd ]] \
  || deny 'ファイルパスを検証できなかったため編集を保留しました。対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'

canonical_target=$(realpath -m -- "$target_path" 2>/dev/null) \
  || deny 'ファイルパスを正規化できなかったため編集を保留しました。対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'

# --- Locate the target's worktree --------------------------------------------
# `worktree list --porcelain` always lists the primary (main) worktree first,
# independently of its checked-out branch.  Pick the longest worktree root that
# is a prefix of the target so nested layouts resolve to the innermost worktree.
primary_root=''
target_root=''
worktree_list=''
if ! worktree_list=$(git -C "$cwd" worktree list --porcelain 2>/dev/null); then
  deny '対象worktreeを確認できなかったため編集を保留しました。Gitの実行環境とcwdを確認し、意図的な直接編集なら対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'
fi
while IFS= read -r line; do
  case $line in
    'worktree '*)
      root=${line#worktree }
      root=$(realpath -m -- "$root" 2>/dev/null) || continue
      [[ -n $primary_root ]] || primary_root=$root
      if [[ $canonical_target == "$root" || $canonical_target == "$root"/* ]]; then
        if [[ -z $target_root || ${#root} -gt ${#target_root} ]]; then
          target_root=$root
        fi
      fi
      ;;
  esac
done <<< "$worktree_list"

# Target outside every worktree (scratchpad, /tmp, ...) is never gated.
[[ -n $target_root ]] || exit 0

# Only tracked files are gated; a literal pathspec blocks glob magic in a
# tool-supplied filename.  Untracked/new files pass through.
relative_path=${canonical_target#"$target_root"/}
if git -C "$target_root" ls-files --error-unmatch -- ":(literal)$relative_path" >/dev/null 2>&1; then
  : # tracked; continue with the policy below
else
  ls_files_status=$?
  # Exit status 1 is the documented "path is not tracked" result.  Any
  # other failure means that the tracked-file decision is indeterminate.
  (( ls_files_status == 1 )) || deny '追跡ファイルか確認できなかったため編集を保留しました。Gitの実行環境を確認し、意図的な直接編集なら対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'
  exit 0
fi

# --- Policy -------------------------------------------------------------------
is_primary=0
[[ $target_root == "$primary_root" ]] && is_primary=1

# Escape-hatch flag lives in the TARGET worktree's own gitdir.
if ! target_gitdir=$(git -C "$target_root" rev-parse --absolute-git-dir 2>/dev/null); then
  deny '対象worktreeのgitdirを確認できなかったため編集を保留しました。Gitの実行環境を確認し、意図的な直接編集なら対象worktreeのgitdirに agentic-loop-allow-edit を立てて再実行してください。'
fi
flag_present=0
[[ -n $target_gitdir && -e $target_gitdir/$flag_basename ]] && flag_present=1

if [[ -n ${AGENTIC_LOOP_AGENT:-} ]]; then
  # Autonomous loop agent.  Editing its own linked worktree is the job; editing
  # the primary worktree is always an accident to be blocked, flag or not.
  (( is_primary == 0 )) && exit 0
  deny '自律エージェントはmain(primary)作業ツリーの追跡ファイルを編集できません。各Issueは専用のlinked worktree内でのみ実装してください。'
fi

# Human, interactive.  The escape hatch (if opened for this worktree) permits it.
(( flag_present == 1 )) && exit 0

if (( is_primary == 1 )); then
  deny 'main(primary)作業ツリーの直接編集は原則禁止です。通常の変更は submit-requirement で Issue 化し supervisor に linked worktree で実施させてください。root repository自体の変更や動作確認など正規の例外の場合のみ、この作業ツリーのgitdirに agentic-loop-allow-edit を立てて再実行し、完了後に削除してください。'
fi
deny 'このlinked worktreeの追跡ファイルを直接編集しようとしています。通常は Issue を作成し supervisor に自動実装させてください。手動で直接介入する正規の例外の場合のみ、この作業ツリーのgitdirに agentic-loop-allow-edit を立てて再実行し、完了後に削除してください。'
