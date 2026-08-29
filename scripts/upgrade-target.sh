#!/usr/bin/env bash
# Applies (or previews) a Foundation upgrade to an already-installed target
# repository. Always executed from the NEW revision's tree (fetched fresh by
# bin/agentic-loop's `upgrade` front-end, or given directly via --source) so
# it never has to rewrite the file it is currently running from. See
# docs/operations/upgrade.md for the full design and docs/decisions/0008.
set -euo pipefail

readonly NEW_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TARGET="${1:-}"
[[ -n $TARGET ]] || { printf 'upgrade-target: target repository path is required\n' >&2; exit 2; }
shift || true
# shellcheck source=lib/foundation-files.sh
source "$NEW_ROOT/scripts/lib/foundation-files.sh"

FORMAT=text
APPLY=0
APPROVE=0
SKIP_VERIFY=0
declare -a OVERWRITE_PATHS=()
while (($#)); do
  case $1 in
    --apply) APPLY=1; shift ;;
    --approve) APPROVE=1; shift ;;
    --skip-verify) SKIP_VERIFY=1; shift ;;
    --format) FORMAT=${2:-text}; shift 2 ;;
    --overwrite) OVERWRITE_PATHS+=("${2:-}"); shift 2 ;;
    # Already resolved by the front-end into AGENTIC_LOOP_RESOLVED_REVISION /
    # AGENTIC_LOOP_REPOSITORY; accepted and ignored here so passthrough works.
    --source | --revision | --repository) shift 2 ;;
    *) printf 'upgrade-target: unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ $FORMAT == text || $FORMAT == json ]] || { printf 'upgrade-target: --format must be text or json\n' >&2; exit 2; }

fail() { printf 'upgrade-target: %s\n' "$1" >&2; exit 2; }
overwrite_requested() { local p=$1 x; for x in "${OVERWRITE_PATHS[@]:-}"; do [[ $x == "$p" ]] && return 0; done; return 1; }
in_shared_files() { local p=$1 x; for x in "${SHARED_FILES[@]}"; do [[ $x == "$p" ]] && return 0; done; return 1; }

doctor_failures_relevant_to_upgrade() {
  # Exclude two checks that upgrade itself intentionally triggers and is not
  # a signal of anything wrong: "Supervisor" always fails while stopped
  # (which --apply already requires below), and "中断したupgrade" reports the
  # very apply-in-progress record this run writes before verifying (see
  # LAST_APPLY below) - both would otherwise make every apply self-block.
  # doctor itself exits non-zero on any failure; that must not leak through
  # pipefail as this function's own exit status, only yq's does.
  ( "$TARGET/bin/agentic-loop" doctor --format json 2>/dev/null || true ) |
    yq -p json '[.checks[] | select(.level == "failure" and .name != "Supervisor" and .name != "中断したupgrade")] | length' 2>/dev/null
}

readonly REPOSITORY="${AGENTIC_LOOP_REPOSITORY:-wakuwaku3/agentic-loop-foundation}"
readonly NEW_REVISION="${AGENTIC_LOOP_RESOLVED_REVISION:-unknown}"
readonly STATE_ROOT="$(git -C "$TARGET" rev-parse --path-format=absolute --git-common-dir 2>/dev/null || printf '%s/.git' "$TARGET")/agentic-loop"
MANIFEST="$STATE_ROOT/foundation-state.json"
readonly LAST_APPLY="$STATE_ROOT/upgrade-last-apply.json"

git -C "$TARGET" rev-parse --git-dir >/dev/null 2>&1 || fail 'target must be a Git repository'

declare -A OLD_HASH=() OLD_CLASS=()
OLD_MODE=''
OLD_REVISION=''
OLD_MIGRATION_LEVEL=0
if [[ ! -r $MANIFEST && -r "$TARGET/.agentic-loop/manifest.json" ]]; then
  MANIFEST="$TARGET/.agentic-loop/manifest.json"
fi
if [[ -r $MANIFEST ]]; then
  OLD_MODE=$(yq -p json -o yaml '.mode // ""' "$MANIFEST" 2>/dev/null || true)
  OLD_REVISION=$(yq -p json -o yaml '.source.revision // ""' "$MANIFEST" 2>/dev/null || true)
  OLD_MIGRATION_LEVEL=$(yq -p json -o yaml '.migration_level // 0' "$MANIFEST" 2>/dev/null || printf 0)
  [[ $OLD_MIGRATION_LEVEL =~ ^[0-9]+$ ]] || OLD_MIGRATION_LEVEL=0
  while IFS=$'\t' read -r p c h; do [[ -n $p ]] && OLD_HASH[$p]=$h && OLD_CLASS[$p]=$c; done < <(yq -p json -o tsv '.files[] | [.path, .class, .sha256] | @tsv' "$MANIFEST" 2>/dev/null || true)
fi

# --- Plan: classify every Foundation-managed path and every migration -----
declare -a ACT_KIND=() ACT_PATH=() ACT_RISK=() ACT_SUMMARY=()
plan_add() { ACT_KIND+=("$1"); ACT_PATH+=("$2"); ACT_RISK+=("$3"); ACT_SUMMARY+=("$4"); }

for path in "${SHARED_FILES[@]}"; do
  [[ $path == .agentic-loop.toml ]] && continue # config keys are migrated, not file-copied
  new_file="$NEW_ROOT/$path"
  [[ -f $new_file ]] || continue
  new_hash=$(foundation_sha256 "$new_file")
  target_file="$TARGET/$path"
  if [[ ! -f $target_file ]]; then
    plan_add add "$path" safe 'Foundationの新規fileを追加します。'
    continue
  fi
  target_hash=$(foundation_sha256 "$target_file")
  [[ $target_hash == "$new_hash" ]] && continue
  old_hash=${OLD_HASH[$path]:-}
  if [[ -n $old_hash && $target_hash == "$old_hash" ]]; then
    plan_add update "$path" safe 'Foundationの更新内容を反映します。'
  else
    plan_add conflict "$path" review "利用者が変更しているため上書きしません。新内容は $path.agentic-loop-new に保存し、--overwrite $path を指定すると反映します。"
  fi
done

for path in "${!OLD_HASH[@]}"; do
  [[ ${OLD_CLASS[$path]:-shared} == shared ]] || continue # class:init is never removed by upgrade
  in_shared_files "$path" && continue
  [[ -f $TARGET/$path ]] || continue
  target_hash=$(foundation_sha256 "$TARGET/$path")
  if [[ $target_hash == "${OLD_HASH[$path]}" ]]; then
    plan_add remove "$path" safe 'Foundationから削除されたfileを削除します。'
  else
    plan_add remove-candidate "$path" review '利用者が変更している可能性があるため削除しません。手動で削除するか保持するか判断してください。'
  fi
done

if [[ $OLD_MODE == init ]]; then
  for path in "${INIT_FILES[@]}"; do
    [[ -f $TARGET/$path && -f $NEW_ROOT/$path ]] || continue
    [[ $(foundation_sha256 "$TARGET/$path") == "$(foundation_sha256 "$NEW_ROOT/$path")" ]] && continue
    plan_add init-notice "$path" info '利用者所有のfileです。upgradeは変更しません。必要なら差分を確認して手動で反映してください。'
  done
fi

declare -a PENDING_MIGRATIONS=()
declare -A MIG_RISK=() MIG_REVERSIBLE=() MIG_APPROVAL=() MIG_SUMMARY=() MIG_RECOVERY=()
migration_meta() { sed -n "s/^# $2: //p" "$1" | head -n1; }
if [[ -d $NEW_ROOT/scripts/upgrade/migrations ]]; then
  for migration in "$NEW_ROOT"/scripts/upgrade/migrations/[0-9][0-9][0-9][0-9]-*.sh; do
    [[ -e $migration ]] || continue
    id=$(basename "$migration" .sh)
    "$migration" "$TARGET" check && continue
    PENDING_MIGRATIONS+=("$id")
    MIG_RISK[$id]=$(migration_meta "$migration" risk); [[ -n ${MIG_RISK[$id]} ]] || MIG_RISK[$id]=safe
    MIG_REVERSIBLE[$id]=$(migration_meta "$migration" reversible)
    MIG_APPROVAL[$id]=$(migration_meta "$migration" approval)
    MIG_SUMMARY[$id]=$(migration_meta "$migration" summary)
    MIG_RECOVERY[$id]=$(migration_meta "$migration" recovery)
  done
fi

APPROVAL_REQUIRED=0
for path in "${!MIG_RISK[@]}"; do
  case ${MIG_RISK[$path]} in breaking | irreversible | cost | permission) APPROVAL_REQUIRED=1 ;; esac
done

# --- Render the plan (dry-run output and the preview before --apply) ------
render_text() {
  local i
  for i in "${!ACT_KIND[@]}"; do
    printf '[%s] %s\n  影響: %s\n' "${ACT_KIND[$i]}" "${ACT_PATH[$i]}" "${ACT_SUMMARY[$i]}"
  done
  for id in "${PENDING_MIGRATIONS[@]}"; do
    printf '[migration] %s (risk=%s)\n  影響: %s\n' "$id" "${MIG_RISK[$id]}" "${MIG_SUMMARY[$id]}"
  done
  if (( ${#ACT_KIND[@]} == 0 && ${#PENDING_MIGRATIONS[@]} == 0 )); then
    printf '変更はありません。導入済みのFoundationは最新です。\n'
  fi
  if (( APPROVAL_REQUIRED )); then
    printf '破壊的・不可逆・追加費用または権限変更を伴う項目があり、承認が必要です。--approve なしでは適用しません。\n'
  fi
}
render_json() {
  local sep='' i out
  out='{"schema_version":1,'
  out+="\"installed\":{\"revision\":$( [[ -n $OLD_REVISION ]] && printf '"%s"' "$(foundation_json_escape "$OLD_REVISION")" || printf null ),\"migration_level\":$OLD_MIGRATION_LEVEL},"
  out+="\"target_revision\":\"$(foundation_json_escape "$NEW_REVISION")\","
  out+='"actions":['
  for i in "${!ACT_KIND[@]}"; do
    out+="$sep{\"kind\":\"${ACT_KIND[$i]}\",\"path\":\"$(foundation_json_escape "${ACT_PATH[$i]}")\",\"risk\":\"${ACT_RISK[$i]}\",\"summary\":\"$(foundation_json_escape "${ACT_SUMMARY[$i]}")\"}"
    sep=','
  done
  out+='],"migrations":['
  sep=''
  for id in "${PENDING_MIGRATIONS[@]}"; do
    out+="$sep{\"id\":\"$id\",\"risk\":\"${MIG_RISK[$id]}\",\"reversible\":\"${MIG_REVERSIBLE[$id]}\",\"summary\":\"$(foundation_json_escape "${MIG_SUMMARY[$id]}")\"}"
    sep=','
  done
  out+="],\"approval_required\":$( ((APPROVAL_REQUIRED)) && printf true || printf false )}"
  printf '%s\n' "$out"
}

if [[ $APPLY == 0 ]]; then
  [[ $FORMAT == json ]] && render_json || render_text
  exit 0
fi

# --- Apply preconditions (never partially change the target) --------------
[[ -z $(git -C "$TARGET" status --porcelain 2>/dev/null) ]] || fail '作業treeがcleanではありません。commitまたはstashしてから再実行してください。'
if [[ -r $STATE_ROOT/supervisor.pid ]]; then
  pid=$(cat "$STATE_ROOT/supervisor.pid" 2>/dev/null || true)
  if [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then
    fail 'Supervisorが稼働中です。bin/agentic-loop stop で停止してから再実行してください。'
  fi
fi
pre_doctor_failures=$(doctor_failures_relevant_to_upgrade)
[[ $pre_doctor_failures =~ ^[0-9]+$ ]] && (( pre_doctor_failures == 0 )) || fail 'upgrade前のdoctorが失敗を報告しています。bin/agentic-loop doctor で内容を確認し解消してから再実行してください。'

if (( APPROVAL_REQUIRED )) && [[ $APPROVE == 0 ]]; then
  [[ $FORMAT == json ]] && render_json || render_text
  printf '承認が必要なため何も適用していません。--approve を付けて再実行してください。\n' >&2
  exit 3
fi

# --- Apply: every write is atomic (temp file + rename) so a process still --
# reading the old bin/agentic-loop never observes a half-written file. -----
declare -a TOUCHED_ADDED=() TOUCHED_CHANGED=()
declare -A PRESERVE_OLD_HASH=()
atomic_write() {
  local src=$1 dest=$2 tmp
  mkdir -p "$(dirname "$dest")"
  tmp="$dest.agentic-loop-tmp.$$"
  cp "$src" "$tmp"
  [[ ! -x $src ]] || chmod +x "$tmp"
  mv -f "$tmp" "$dest"
}

for i in "${!ACT_KIND[@]}"; do
  path=${ACT_PATH[$i]}
  case ${ACT_KIND[$i]} in
    add) atomic_write "$NEW_ROOT/$path" "$TARGET/$path"; TOUCHED_ADDED+=("$path") ;;
    update) atomic_write "$NEW_ROOT/$path" "$TARGET/$path"; TOUCHED_CHANGED+=("$path") ;;
    remove) rm -f "$TARGET/$path"; TOUCHED_CHANGED+=("$path") ;;
    conflict)
      if overwrite_requested "$path"; then
        atomic_write "$NEW_ROOT/$path" "$TARGET/$path"; TOUCHED_CHANGED+=("$path")
        rm -f "$TARGET/$path.agentic-loop-new"
      else
        # Left untouched: keep manifest recording the last Foundation-delivered
        # hash (not the user's current content) so this still reads as a
        # conflict on the next run instead of silently becoming the new baseline.
        atomic_write "$NEW_ROOT/$path" "$TARGET/$path.agentic-loop-new"
        PRESERVE_OLD_HASH[$path]=1
      fi
      ;;
    remove-candidate) PRESERVE_OLD_HASH[$path]=1 ;;
    init-notice) : ;;
  esac
done

applied_migrations=()
for id in "${PENDING_MIGRATIONS[@]}"; do
  migration="$NEW_ROOT/scripts/upgrade/migrations/$id.sh"
  if ! "$migration" "$TARGET" apply; then
    fail "migration $id の適用に失敗しました。原因を解消し、再実行してください（冪等なため未適用分のみ再試行されます）。"
  fi
  applied_migrations+=("$id")
done
if (( ${#applied_migrations[@]} > 0 )); then
  TOUCHED_CHANGED+=(.agentic-loop.toml)
  if [[ $NEW_REVISION =~ ^[0-9a-fA-F]{40}$ ]] && grep -Eq '^\[foundation\]' "$TARGET/.agentic-loop.toml"; then
    sed -i -E "s/^revision = \".*\"\$/revision = \"$NEW_REVISION\"/" "$TARGET/.agentic-loop.toml"
  fi
fi

# --- Record what changed before verifying, so a verify failure still leaves
# a clear, git-native rollback path (see docs/operations/upgrade.md). -------
mkdir -p "$STATE_ROOT"
{
  printf '{"schema_version":1,"before_revision":"%s","after_revision":"%s","added":[' "$(foundation_json_escape "$OLD_REVISION")" "$(foundation_json_escape "$NEW_REVISION")"
  sep=''; for p in "${TOUCHED_ADDED[@]}"; do printf '%s"%s"' "$sep" "$(foundation_json_escape "$p")"; sep=','; done
  printf '],"changed":['
  sep=''; for p in "${TOUCHED_CHANGED[@]}"; do printf '%s"%s"' "$sep" "$(foundation_json_escape "$p")"; sep=','; done
  printf ']}\n'
} > "$LAST_APPLY"

# --- Rebuild the manifest from what is now actually on disk ----------------
new_migration_level=0
if [[ -d $NEW_ROOT/scripts/upgrade/migrations ]]; then
  for migration in "$NEW_ROOT"/scripts/upgrade/migrations/[0-9][0-9][0-9][0-9]-*.sh; do
    [[ -e $migration ]] || continue
    "$migration" "$TARGET" check && new_migration_level=$((new_migration_level + 1))
  done
fi
entries=''
for path in "${SHARED_FILES[@]}"; do
  [[ -f $TARGET/$path ]] || continue
  if [[ -n ${PRESERVE_OLD_HASH[$path]:-} && -n ${OLD_HASH[$path]:-} ]]; then entries+="$path"$'\t'"shared"$'\t'"${OLD_HASH[$path]}"$'\n'
  elif [[ -n ${PRESERVE_OLD_HASH[$path]:-} ]]; then : # unresolved first-seen conflicts have no baseline
  else entries+="$path"$'\t'"shared"$'\n'; fi
done
for path in "${!PRESERVE_OLD_HASH[@]}"; do
  in_shared_files "$path" && continue # already emitted above with its preserved hash
  [[ -f $TARGET/$path ]] || continue
  entries+="$path"$'\t'"shared"$'\t'"${OLD_HASH[$path]:-}"$'\n'
done
if [[ $OLD_MODE == init ]]; then for path in "${INIT_FILES[@]}"; do [[ -f $TARGET/$path ]] && entries+="$path"$'\t'"init"$'\n'; done; fi
steps='['; sep=''
for id in "${applied_migrations[@]}"; do steps+="$sep\"$id\""; sep=','; done
steps+=']'
old_history=$(yq -p json -o json -I0 '(.history // [])[]' "$MANIFEST" 2>/dev/null | paste -sd, - || true)
new_history_entry=$(printf '{"at":%s,"from_revision":"%s","to_revision":"%s","from_level":%s,"to_level":%s,"steps":%s,"result":"applied"}' \
  "$(date +%s)" "$(foundation_json_escape "$OLD_REVISION")" "$(foundation_json_escape "$NEW_REVISION")" "$OLD_MIGRATION_LEVEL" "$new_migration_level" "$steps")
if [[ -n $old_history ]]; then history="$old_history,$new_history_entry"; else history="$new_history_entry"; fi
foundation_state_write "$TARGET" "${OLD_MODE:-install}" "$REPOSITORY" "$NEW_REVISION" "${AGENTIC_LOOP_REVISION:-$NEW_REVISION}" "$new_migration_level" "$entries" "$history"

# Verify the rewritten manifest records exactly the revision this upgrade
# applied, so a broken write never leaves the installed-revision record stale
# (main sync tolerates this manifest-only change; upgrade/rollback rely on it).
[[ $(yq -p json -o yaml '.source.revision // ""' "$STATE_ROOT/foundation-state.json" 2>/dev/null) == "$NEW_REVISION" ]] ||
  fail "upgrade manifest does not record the applied revision: $NEW_REVISION"

# --- Post-apply verification: doctor, then the configured full check ------
verify_failed=0
post_doctor_failures=$(doctor_failures_relevant_to_upgrade)
[[ $post_doctor_failures =~ ^[0-9]+$ ]] && (( post_doctor_failures == 0 )) || verify_failed=1
verify_command=$(yq -p toml -o yaml '.foundation.verify_command // "devbox run --pure check"' "$TARGET/.agentic-loop.toml" 2>/dev/null || printf 'devbox run --pure check')
if (( SKIP_VERIFY == 0 )) && (( verify_failed == 0 )); then
  (cd "$TARGET" && eval "$verify_command") || verify_failed=1
fi

if (( verify_failed )); then
  printf 'upgradeの適用後検証に失敗しました。適用状態は保持されています。\n' >&2
  printf '  復旧: %s upgrade --rollback を実行して元に戻すか、原因を修正して再実行してください（migrationは冪等です）。\n' "$(basename "$TARGET/bin/agentic-loop")" >&2
  exit 1
fi

rm -f "$LAST_APPLY"
printf 'Foundationを %s から %s へ更新しました。\n' "${OLD_REVISION:-未記録}" "$NEW_REVISION"
