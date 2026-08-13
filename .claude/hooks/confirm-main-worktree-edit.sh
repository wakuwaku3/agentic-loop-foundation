#!/usr/bin/env bash
# Ask before a Claude file-edit tool writes a tracked file in the primary
# worktree.  This is deliberately a confirmation gate, not a prohibition.
set -euo pipefail

ask() {
  printf '%s\n' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"メイン作業ツリーの追跡ファイルを直接編集しようとしています。通常の変更要求は submit-requirement の queue-first intake で Issue キューへ登録し、専用worker worktreeで実施してください。明示的な同期実装など正規の例外であれば、内容を確認して承認するとこの操作を実行できます。"}}'
}

# A malformed event must not accidentally turn a main-worktree edit into an
# unguarded one.  The settings matcher prevents this path for read-only tools.
payload=$(cat) || { ask; exit 0; }
tool_name=$(printf '%s' "$payload" | yq -p json -r '.tool_name // ""' - 2>/dev/null) || { ask; exit 0; }
case $tool_name in
  Edit|Write) path_query='.tool_input.file_path // ""' ;;
  NotebookEdit) path_query='.tool_input.notebook_path // ""' ;;
  *) exit 0 ;;
esac
target_path=$(printf '%s' "$payload" | yq -p json -r "$path_query" - 2>/dev/null) || { ask; exit 0; }
cwd=$(printf '%s' "$payload" | yq -p json -r '.cwd // ""' - 2>/dev/null) || { ask; exit 0; }

[[ -n $target_path && -n $cwd && $target_path != *$'\n'* && $target_path != *$'\r'* && $cwd != *$'\n'* && $cwd != *$'\r'* ]] || { ask; exit 0; }
[[ $target_path == /* && -d $cwd ]] || { ask; exit 0; }

# The shared git directory identifies all linked worktrees.  `worktree list`
# always lists its primary (main) worktree first, independently of its branch.
common_dir=$(git -C "$cwd" rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || { ask; exit 0; }
main_root=''
while IFS= read -r line; do
  case $line in
    'worktree '*) main_root=${line#worktree }; break ;;
  esac
done < <(git --git-dir="$common_dir" worktree list --porcelain 2>/dev/null)
[[ -n $main_root ]] || { ask; exit 0; }

main_root=$(realpath -e -- "$main_root" 2>/dev/null) || { ask; exit 0; }
canonical_target=$(realpath -m -- "$target_path" 2>/dev/null) || { ask; exit 0; }
case $canonical_target in
  "$main_root"/*) relative_path=${canonical_target#"$main_root"/} ;;
  *) exit 0 ;;
esac

# A literal pathspec prevents glob magic in a tool-supplied filename.  Only
# tracked files qualify: scratchpads and as-yet-untracked files pass through.
if git -C "$main_root" ls-files --error-unmatch -- ":(literal)$relative_path" >/dev/null 2>&1; then
  ask
fi
