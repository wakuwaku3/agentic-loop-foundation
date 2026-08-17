# Module: upgrade.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



# --- Foundation upgrade (see docs/operations/upgrade.md, docs/decisions/0008) ---
# This front-end only resolves which revision to compare/apply against and
# then delegates to that revision's own scripts/upgrade-target.sh, so the
# script that rewrites this repository's files is never the one currently
# executing (see scripts/upgrade-target.sh's header for why that matters).
# --rollback is the one exception: it only needs local Git state, so it is
# handled here directly without fetching any source tree.
upgrade_rollback() {
  local last_apply="$STATE_ROOT/upgrade-last-apply.json" path
  [[ -r $last_apply ]] || fail 'rollback対象がありません（未完了のupgrade適用が記録されていません）。'
  command -v yq >/dev/null 2>&1 || fail 'yq is required'
  while IFS= read -r path; do [[ -n $path ]] && rm -f "$REPO_ROOT/$path"; done < <(yq -p json -o tsv '.added[]' "$last_apply" 2>/dev/null)
  while IFS= read -r path; do
    [[ -n $path ]] || continue
    git -C "$REPO_ROOT" checkout -q HEAD -- "$path" 2>/dev/null || rm -f "$REPO_ROOT/$path"
  done < <(yq -p json -o tsv '.changed[]' "$last_apply" 2>/dev/null)
  # The apply precondition guarantees a clean worktree, so HEAD holds the
  # pre-apply manifest. Restore it too, or the installed-revision record would
  # keep claiming the rolled-back revision while the files are back at the old
  # one (and a later upgrade would misclassify every shared file as a conflict).
  git -C "$REPO_ROOT" checkout -q HEAD -- .agentic-loop/manifest.json 2>/dev/null || rm -f "$REPO_ROOT/.agentic-loop/manifest.json"
  rm -f "$last_apply"
  say 'upgrade適用前の状態にrollbackしました。git status で内容を確認してください。'
}


cmd_upgrade() {
  shift
  local -a passthrough=("$@")
  local source_path='' revision='' repository='' arg i=0 tmp=''
  # Argument/precondition errors here exit 2 (matching scripts/upgrade-target.sh's
  # own exit code for the same class of failure), not the generic fail()'s exit 1.
  arg_fail() { printf '%s: %s\n' "$PROGRAM_NAME" "$1" >&2; exit 2; }
  while (( i < ${#passthrough[@]} )); do
    arg=${passthrough[$i]}
    case $arg in
      --rollback) [[ ${#passthrough[@]} -eq 1 ]] || arg_fail '--rollback は他の引数と併用できません'; upgrade_rollback; return 0 ;;
      --source) source_path=${passthrough[$((i + 1))]:-}; i=$((i + 2)) ;;
      --revision) revision=${passthrough[$((i + 1))]:-}; i=$((i + 2)) ;;
      --repository) repository=${passthrough[$((i + 1))]:-}; i=$((i + 2)) ;;
      *) i=$((i + 1)) ;;
    esac
  done
  [[ -n $repository ]] || repository=$(config_value 'foundation.repository')
  [[ -n $repository ]] || repository=wakuwaku3/agentic-loop-foundation
  if [[ -n $source_path ]]; then
    [[ -d $source_path ]] || arg_fail "--source のpathが存在しません: $source_path"
    [[ -n $revision ]] || revision=$(git -C "$source_path" rev-parse HEAD 2>/dev/null || printf 'unknown')
  else
    [[ -n $revision ]] || revision=$(config_value 'foundation.revision')
    [[ -n $revision ]] || arg_fail 'upgrade対象のrevisionが指定されていません。--revision <40桁SHA>、--source <path>、または .agentic-loop.toml の [foundation].revision を設定してください（mainへの暗黙追従は行いません）。'
    [[ $revision =~ ^[0-9a-fA-F]{40}$ ]] || arg_fail 'revisionは40桁のcommit SHAで指定してください（branch名やtagは受け付けません）。'
    require_command git
    tmp=$(mktemp -d)
    if ! git init -q "$tmp" ||
      ! git -C "$tmp" fetch -q --depth 1 "https://github.com/$repository" "$revision" ||
      ! (cd "$tmp" && git archive --format=tar FETCH_HEAD | tar -x); then
      rm -rf "$tmp"
      arg_fail "revision $revision を $repository から取得できませんでした。"
    fi
    source_path=$tmp
  fi
  [[ -x $source_path/scripts/upgrade-target.sh ]] || arg_fail "取得したFoundation source ($source_path) に scripts/upgrade-target.sh がありません。"
  local rc=0
  AGENTIC_LOOP_REPOSITORY="$repository" AGENTIC_LOOP_RESOLVED_REVISION="$revision" "$source_path/scripts/upgrade-target.sh" "$REPO_ROOT" "${passthrough[@]}" || rc=$?
  [[ -z $tmp ]] || rm -rf "$tmp"
  return $rc
}
