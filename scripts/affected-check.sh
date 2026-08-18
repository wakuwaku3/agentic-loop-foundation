#!/usr/bin/env bash
# shellcheck disable=SC2155
# 変更影響に応じた段階的テスト選択（Issue #59, ADR 0021）。
#
# これは push gate でも merge gate でもない。gate は常に
# `devbox run --pure check`（local full check / CI）のままであり、この
# scriptは編集中のfeedbackを短縮するための local affected check だけを
# 提供する。判定規則は tests/impact-map.toml の唯一のsourceに従い、
# 判定不能・共有依存・設定変更・test基盤変更・未一致の変更は必ず安全側
# （全単位）へ拡大する。除外・skip用のinterfaceは意図的に持たない
# （docs/policies/validation-harness.mdおよびdocs/decisions/0021参照）。
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
readonly PROJECT_ROOT="$PWD"
map_path="$PROJECT_ROOT/tests/impact-map.toml"

usage() {
  cat >&2 <<'EOF'
使い方:
  scripts/affected-check.sh [--changed-from REF] [--files FILE|-] [--print-plan]
                             [--format text|json] [--record PATH|--no-record]
                             [--audit] [--map PATH]

除外用のoptionは存在しない（実行が不安定であることや過去の失敗を理由にした除外を許さないため）。
EOF
}

format=text
changed_from=''
files_source=''
print_plan=0
record_path='tmp/affected-last.json'
audit_mode=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --changed-from)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      changed_from=$2; shift 2 ;;
    --files)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      files_source=$2; shift 2 ;;
    --print-plan)
      print_plan=1; shift ;;
    --format)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      format=$2; shift 2
      case "$format" in text | json) ;; *) usage; exit 2 ;; esac ;;
    --record)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      record_path=$2; shift 2 ;;
    --no-record)
      record_path=''; shift ;;
    --audit)
      audit_mode=1; shift ;;
    --map)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      map_path=$2; shift 2 ;;
    -h | --help)
      usage; exit 0 ;;
    *)
      printf 'affected-check.sh: unknown argument: %s\n' "$1" >&2
      usage
      exit 2 ;;
  esac
done
readonly IMPACT_MAP="$map_path"

command -v yq >/dev/null 2>&1 || { printf 'affected-check.sh: yq is required\n' >&2; exit 1; }
[[ -r $IMPACT_MAP ]] || { printf 'affected-check.sh: missing %s\n' "$IMPACT_MAP" >&2; exit 1; }

MAP_JSON=$(yq -p toml -o json '.' "$IMPACT_MAP") || { printf 'affected-check.sh: cannot parse %s\n' "$IMPACT_MAP" >&2; exit 1; }
map_query() { yq -p json -r "$1" <<< "$MAP_JSON" 2>/dev/null; }

schema_version=$(map_query '.schema_version // ""')
[[ $schema_version == 1 ]] || { printf 'affected-check.sh: unsupported impact-map schema_version: %s\n' "$schema_version" >&2; exit 1; }

mapfile -t ALL_UNITS < <(map_query '.units[]?')
mapfile -t ALWAYS_UNITS < <(map_query '.always[]?')

declare -a RULE_PATHS=() RULE_REASONS=() RULE_UNITS=()
while IFS=$'\t' read -r rpath rreason runits; do
  [[ -n $rpath ]] || continue
  RULE_PATHS+=("$rpath"); RULE_REASONS+=("$rreason"); RULE_UNITS+=("$runits")
done < <(map_query '.rule[]? | [.path, .reason, (.units // [] | join(","))] | @tsv')

declare -a WIDEN_PATHS=() WIDEN_REASONS=()
while IFS=$'\t' read -r wpath wreason; do
  [[ -n $wpath ]] || continue
  WIDEN_PATHS+=("$wpath"); WIDEN_REASONS+=("$wreason")
done < <(map_query '.widen[]? | [.path, .reason] | @tsv')

# path_matches CANDIDATE PATH -> セグメント境界を尊重したprefix一致
# (bin/agentic-loop-diagnose は bin/agentic-loop に一致しない)。
path_matches() {
  local candidate=$1 path=$2
  [[ $path == "$candidate" || $path == "$candidate"/* ]]
}

# gate_full_check -> capabilities.toml から full_check を読む。既定値へは
# 推測でfallbackせず、常に「これはgateではない」ことも併記する。
gate_full_check() {
  local value=''
  if [[ -r $PROJECT_ROOT/.agentic-loop/capabilities.toml ]]; then
    value=$(yq -p toml -r '.validation.full_check // ""' "$PROJECT_ROOT/.agentic-loop/capabilities.toml" 2>/dev/null || true)
  fi
  [[ -n $value ]] || value='devbox run --pure check'
  printf '%s' "$value"
}
GATE_FULL_CHECK=$(gate_full_check)
readonly GATE_FULL_CHECK

json_escape() {
  local value=$1
  value=${value//\\/\\\\}; value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}
  printf '%s' "$value"
}

# --- audit mode ---------------------------------------------------------
if (( audit_mode == 1 )); then
  audit_fail=0
  audit_error() { printf 'affected-audit: %s\n' "$1" >&2; audit_fail=1; }

  # 1) schema: 宣言単位が run-e2e.sh / test-agentic-loop.sh の群集合と完全一致する
  mapfile -t RUNNER_GROUPS < <(grep -oE 'readonly ALL_GROUPS=\(([^)]*)\)' "$PROJECT_ROOT/tests/run-e2e.sh" | sed -E 's/readonly ALL_GROUPS=\((.*)\)/\1/' | tr ' ' '\n' | sed '/^$/d')
  mapfile -t ACCEPTED_GROUPS < <(grep -oE 'all\|[a-z|]+\)' "$PROJECT_ROOT/tests/test-agentic-loop.sh" | head -1 | tr -d ')' | tr '|' '\n' | grep -v '^all$')
  declare -a map_groups=()
  for u in "${ALL_UNITS[@]}"; do map_groups+=("${u#e2e:}"); done
  map_groups_sorted=$(printf '%s\n' "${map_groups[@]}" | sort -u)
  if [[ "$(printf '%s\n' "${RUNNER_GROUPS[@]}" | sort -u)" != "$map_groups_sorted" ]]; then
    audit_error "impact-map.toml の units（${map_groups[*]}）が tests/run-e2e.sh の群集合（${RUNNER_GROUPS[*]}）と一致しません"
  fi
  if [[ "$(printf '%s\n' "${ACCEPTED_GROUPS[@]}" | sort -u)" != "$map_groups_sorted" ]]; then
    audit_error "impact-map.toml の units（${map_groups[*]}）が tests/test-agentic-loop.sh の群集合（${ACCEPTED_GROUPS[*]}）と一致しません"
  fi

  # 2) 到達可能性: すべての単位が少なくとも1つのruleから到達可能
  for u in "${map_groups[@]}"; do
    reachable=0
    for ru in "${RULE_UNITS[@]}"; do
      IFS=',' read -ra parts <<< "$ru"
      for p in "${parts[@]}"; do [[ "${p#e2e:}" == "$u" ]] && { reachable=1; break; }; done
      (( reachable == 1 )) && break
    done
    (( reachable == 1 )) || audit_error "単位 e2e:$u はどのruleからも到達できません（孤立群）"
  done

  # 3) rule/widenのpathがすべて実在する
  for p in "${RULE_PATHS[@]}" "${WIDEN_PATHS[@]}"; do
    [[ -e $PROJECT_ROOT/$p ]] || audit_error "存在しないpathを参照しています: $p"
  done

  # 4/5) 追跡済みfileはすべて明示的にrule/widenへ一致し、全変更→全単位に退化する
  declare -a all_tracked=()
  while IFS= read -r p; do all_tracked+=("$p"); done < <(git -C "$PROJECT_ROOT" ls-files)
  any_widen_hit=0
  for p in "${all_tracked[@]}"; do
    best_len=-1; best_kind=''
    for c in "${RULE_PATHS[@]}"; do path_matches "$c" "$p" && (( ${#c} > best_len )) && { best_len=${#c}; best_kind=rule; }; done
    for c in "${WIDEN_PATHS[@]}"; do path_matches "$c" "$p" && (( ${#c} > best_len )) && { best_len=${#c}; best_kind=widen; }; done
    if [[ -z $best_kind ]]; then
      audit_error "追跡済みfileがどのrule/widenにも一致しません（unmatchedのまま）: $p"
    elif [[ $best_kind == widen ]]; then
      any_widen_hit=1
    fi
  done
  (( any_widen_hit == 1 )) || audit_error "全変更集合でもwidenに一度も一致しません（full check同等性を保証できません）"

  if (( audit_fail == 0 )); then
    printf 'affected-audit: passed (units=%s)\n' "${map_groups[*]}"
    exit 0
  fi
  exit 1
fi

# --- normal mode: determine changed files -------------------------------
declare -a CHANGED_FILES=()
base_sha=''
base_unknown=0

if [[ -n $files_source ]]; then
  if [[ $files_source == '-' ]]; then
    while IFS= read -r line; do [[ -n $line ]] && CHANGED_FILES+=("$line"); done
  else
    while IFS= read -r line; do [[ -n $line ]] && CHANGED_FILES+=("$line"); done < "$files_source"
  fi
else
  base_ref=$changed_from
  if [[ -z $base_ref ]]; then
    for candidate in origin/main main; do
      if git -C "$PROJECT_ROOT" rev-parse --verify --quiet "$candidate" >/dev/null; then
        base_ref=$candidate
        break
      fi
    done
  fi
  if [[ -n $base_ref ]] && base_sha=$(git -C "$PROJECT_ROOT" merge-base "$base_ref" HEAD 2>/dev/null); then
    while IFS=$'\t' read -r status path1 path2; do
      [[ -n $status ]] || continue
      CHANGED_FILES+=("$path1")
      [[ $status == R* && -n $path2 ]] && CHANGED_FILES+=("$path2")
    done < <(git -C "$PROJECT_ROOT" diff --name-status -M --find-renames "$base_sha" HEAD)
    while IFS=$'\t' read -r status path1 path2; do
      [[ -n $status ]] || continue
      CHANGED_FILES+=("$path1")
      [[ $status == R* && -n $path2 ]] && CHANGED_FILES+=("$path2")
    done < <(git -C "$PROJECT_ROOT" diff --name-status -M --find-renames HEAD)
    while IFS= read -r path; do [[ -n $path ]] && CHANGED_FILES+=("$path"); done < <(git -C "$PROJECT_ROOT" ls-files --others --exclude-standard)
  else
    base_unknown=1
  fi
fi

# 重複除去（順序維持は不要）
if (( ${#CHANGED_FILES[@]} > 0 )); then
  mapfile -t CHANGED_FILES < <(printf '%s\n' "${CHANGED_FILES[@]}" | sort -u)
fi

declare -a changed_json=() unmatched_list=() widen_reason_seen=()
declare -A selected_units_set=()
widened=0

add_widen_reason() {
  local r=$1 seen
  for seen in "${widen_reason_seen[@]}"; do [[ $seen == "$r" ]] && return; done
  widen_reason_seen+=("$r")
}

if (( base_unknown == 1 )); then
  widened=1
  add_widen_reason 'git-base-unknown'
fi

for path in "${CHANGED_FILES[@]}"; do
  best_len=-1; best_kind=''; best_idx=-1
  for i in "${!RULE_PATHS[@]}"; do
    if path_matches "${RULE_PATHS[$i]}" "$path" && (( ${#RULE_PATHS[$i]} > best_len )); then
      best_len=${#RULE_PATHS[$i]}; best_kind=rule; best_idx=$i
    fi
  done
  for i in "${!WIDEN_PATHS[@]}"; do
    if path_matches "${WIDEN_PATHS[$i]}" "$path" && (( ${#WIDEN_PATHS[$i]} > best_len )); then
      best_len=${#WIDEN_PATHS[$i]}; best_kind=widen; best_idx=$i
    fi
  done
  case $best_kind in
    rule)
      rule_path=${RULE_PATHS[$best_idx]}
      IFS=',' read -ra units_for_rule <<< "${RULE_UNITS[$best_idx]}"
      units_json=''
      sep=''
      for u in "${units_for_rule[@]}"; do
        [[ -n $u ]] || continue
        selected_units_set[$u]=1
        units_json+="${sep}\"$(json_escape "$u")\""
        sep=','
      done
      changed_json+=("{\"path\":\"$(json_escape "$path")\",\"rule\":\"$(json_escape "$rule_path")\",\"units\":[${units_json}]}")
      ;;
    widen)
      widened=1
      add_widen_reason "${WIDEN_REASONS[$best_idx]}"
      ;;
    *)
      widened=1
      unmatched_list+=("$path")
      add_widen_reason 'unmatched'
      ;;
  esac
done

declare -a selected_units=()
if (( widened == 1 )); then
  selected_units=("${ALL_UNITS[@]}")
else
  for u in "${ALL_UNITS[@]}"; do [[ -n ${selected_units_set[$u]:-} ]] && selected_units+=("$u"); done
fi

declare -a skipped_units=()
if (( widened == 0 )); then
  for u in "${ALL_UNITS[@]}"; do
    [[ -n ${selected_units_set[$u]:-} ]] || skipped_units+=("$u")
  done
fi

render_plan_json() {
  local sep
  printf '{"schema":1,"base":%s,"gate":{"full_check":"%s"},' \
    "$([[ -n $base_sha ]] && printf '"%s"' "$base_sha" || printf 'null')" "$(json_escape "$GATE_FULL_CHECK")"
  printf '"changed":['
  sep=''
  for c in "${changed_json[@]}"; do [[ -n $c ]] && { printf '%s%s' "$sep" "$c"; sep=','; }; done
  printf '],"unmatched":['
  sep=''
  for u in "${unmatched_list[@]}"; do printf '%s"%s"' "$sep" "$(json_escape "$u")"; sep=','; done
  printf '],"widened":%s,"widen_reasons":[' "$([[ $widened == 1 ]] && printf true || printf false)"
  sep=''
  for r in "${widen_reason_seen[@]}"; do printf '%s"%s"' "$sep" "$(json_escape "$r")"; sep=','; done
  printf '],"selected_units":['
  sep=''
  for u in "${selected_units[@]}"; do printf '%s"%s"' "$sep" "$(json_escape "$u")"; sep=','; done
  printf '],"skipped_units":['
  sep=''
  for u in "${skipped_units[@]}"; do printf '%s{"unit":"%s","reason":"no-matching-change"}' "$sep" "$(json_escape "$u")"; sep=','; done
  printf ']'
}

render_plan_text() {
  printf 'gate（変更しない）: %s\n' "$GATE_FULL_CHECK"
  printf 'これは push/merge gate ではありません。\n'
  if [[ -n $base_sha ]]; then printf 'base: %s\n' "$base_sha"; fi
  printf '常時実行: %s\n' "${ALWAYS_UNITS[*]:-}"
  printf '選択単位: %s\n' "${selected_units[*]:-(なし)}"
  if (( ${#skipped_units[@]} > 0 )); then printf 'skip: %s\n' "${skipped_units[*]}"; fi
  if [[ $widened == 1 ]]; then printf '拡大: %s\n' "${widen_reason_seen[*]}"; fi
  if (( ${#unmatched_list[@]} > 0 )); then printf '未一致: %s\n' "${unmatched_list[*]}"; fi
  return 0
}

if (( print_plan == 1 )); then
  if [[ $format == json ]]; then
    printf '%s,"results":[],"total_seconds":null}\n' "$(render_plan_json)"
  else
    render_plan_text
  fi
  exit 0
fi

# --- normal mode: execute --------------------------------------------------
printf 'これは push/merge gate ではありません。gateは常に「%s」です。\n' "$GATE_FULL_CHECK" >&2

declare -a result_units=() result_status=() result_seconds=()
overall_rc=0

run_unit() {
  local unit=$1 start elapsed rc=0
  shift
  start=$SECONDS
  if "$@"; then rc=0; else rc=1; fi
  elapsed=$(( SECONDS - start ))
  result_units+=("$unit"); result_seconds+=("$elapsed")
  if (( rc == 0 )); then result_status+=(passed); else result_status+=(failed); overall_rc=1; fi
  return $rc
}

run_unit environment "$PROJECT_ROOT/scripts/check-environment.sh" || true
run_unit lint "$PROJECT_ROOT/scripts/lint.sh" || true

if (( ${#selected_units[@]} > 0 )); then
  declare -a groups=()
  for u in "${selected_units[@]}"; do groups+=("${u#e2e:}"); done
  groups_csv=$(IFS=,; printf '%s' "${groups[*]}")
  report_tmp=$(mktemp)
  e2e_start=$SECONDS
  e2e_rc=0
  "$PROJECT_ROOT/tests/run-e2e.sh" --groups "$groups_csv" --report "$report_tmp" || e2e_rc=1
  if [[ -s $report_tmp ]]; then
    while IFS=$'\t' read -r group gresult gseconds; do
      [[ -n $group ]] || continue
      result_units+=("e2e:$group"); result_status+=("$gresult"); result_seconds+=("$gseconds")
    done < "$report_tmp"
  else
    result_units+=("e2e"); result_status+=("$([[ $e2e_rc == 0 ]] && printf passed || printf failed)"); result_seconds+=("$(( SECONDS - e2e_start ))")
  fi
  rm -f "$report_tmp"
  (( e2e_rc == 0 )) || overall_rc=1
fi

total_seconds=0
for s in "${result_seconds[@]}"; do total_seconds=$(( total_seconds + s )); done

if [[ -n $record_path ]]; then
  mkdir -p "$(dirname "$record_path")"
  {
    printf '%s,"results":[' "$(render_plan_json)"
    sep=''
    for i in "${!result_units[@]}"; do
      printf '%s{"unit":"%s","result":"%s","seconds":%s}' "$sep" "$(json_escape "${result_units[$i]}")" "${result_status[$i]}" "${result_seconds[$i]}"
      sep=','
    done
    printf '],"total_seconds":%s}\n' "$total_seconds"
  } > "$record_path"
fi

if [[ $format == json ]]; then
  printf '%s,"results":[' "$(render_plan_json)"
  sep=''
  for i in "${!result_units[@]}"; do
    printf '%s{"unit":"%s","result":"%s","seconds":%s}' "$sep" "$(json_escape "${result_units[$i]}")" "${result_status[$i]}" "${result_seconds[$i]}"
    sep=','
  done
  printf '],"total_seconds":%s}\n' "$total_seconds"
else
  render_plan_text
  printf -- '--- 実行結果 ---\n'
  for i in "${!result_units[@]}"; do
    printf '%s: %s (%ss)\n' "${result_units[$i]}" "${result_status[$i]}" "${result_seconds[$i]}"
  done
  printf '合計: %ss\n' "$total_seconds"
fi

exit $overall_rc
