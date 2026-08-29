# Module: workload.sh. Sourced by bin/agentic-loop (see docs/decisions/
# 0013-agentic-loop-modules.md). See docs/decisions/0025-resource-scalability-
# budget.md and docs/operations/workload-budget.md.
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034,SC2153



# --- Workload budget record (see ADR 0025, docs/operations/workload-budget.md)
# The plan stage may emit one fenced `agentic-loop:workload` JSON block,
# self-declaring the processing-volume model for any external I/O it adds or
# changes (call counts per unit of work, growth rate against input size N,
# stop conditions, reuse/aggregation, and non-amplification under idle/
# failure/multi-host). Unlike `agentic-loop:preflight` (an approval gate),
# this never requires human approval on its own: `require` only blocks a
# missing or invalid record, and a re-plan that simply re-declares the record
# clears the gate. Evaluation costs zero additional GitHub API calls (the
# record is read from the plan-stage output file already on disk); only a
# gate/audit comment, when one fires, costs one write.
readonly WORKLOAD_SCHEMA_VERSION=1
readonly WORKLOAD_RECORD_MAX_BYTES=4096
readonly WORKLOAD_ALLOWED_EXTERNAL_IO='none added changed'
readonly WORKLOAD_AMPLIFICATION_KEYS='idle failure multi_host'
readonly WORKLOAD_UNIT_FIELDS='operation per_unit growth stop_condition reuse'


# --- Record extraction (mirrors preflight_manifest_from_plan) ---------------
workload_manifest_from_plan() {
  awk '/^```agentic-loop:workload[[:space:]]*$/{on=1;next} on && /^```[[:space:]]*$/{exit} on{print}' "$1" 2>/dev/null
}


workload_text_field_safe() {
  local text=$1 max=$2
  [[ ${#text} -le $max && $text != *$'\n'* && $text != *'`'* ]]
}


# --- Schema validation (never trusts the record; only bounds its shape) -----
# WORKLOAD_INVALID_REASON is one of: missing-record, record-too-large,
# secret-like, guard-unavailable, schema-invalid.
workload_validate_schema() {
  local manifest=$1 issue=$2 size external_io
  WORKLOAD_INVALID_REASON=''
  [[ -n $manifest ]] || { WORKLOAD_INVALID_REASON='missing-record'; return 1; }
  size=$(printf '%s' "$manifest" | wc -c)
  [[ $size -le $WORKLOAD_RECORD_MAX_BYTES ]] || { WORKLOAD_INVALID_REASON='record-too-large'; return 1; }
  trace_secret_scan_clean "$manifest" || { WORKLOAD_INVALID_REASON=${TRACE_INVALID_REASON:-secret-like}; return 1; }
  command -v yq >/dev/null 2>&1 || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }

  local top_keys k
  top_keys=$(yq -p json -r 'keys | .[]' <<< "$manifest" 2>/dev/null) || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  while IFS= read -r k; do
    case $k in schema | issue | external_io | units | amplification | verification | exceptions) ;; *) WORKLOAD_INVALID_REASON='schema-invalid'; return 1 ;; esac
  done <<< "$top_keys"
  [[ $(yq -p json -r '.schema // ""' <<< "$manifest" 2>/dev/null) == "$WORKLOAD_SCHEMA_VERSION" ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  [[ $(yq -p json -r '.issue // ""' <<< "$manifest" 2>/dev/null) == "$issue" ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }

  external_io=$(yq -p json -r '.external_io // ""' <<< "$manifest" 2>/dev/null)
  grep -Fxq "$external_io" <<< "${WORKLOAD_ALLOWED_EXTERNAL_IO// /$'\n'}" || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }

  local unit_count verification_count
  unit_count=$(yq -p json -r '.units // [] | length' <<< "$manifest" 2>/dev/null) || unit_count=0
  [[ $unit_count =~ ^[0-9]+$ && $unit_count -le 10 ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  verification_count=$(yq -p json -r '.verification // [] | length' <<< "$manifest" 2>/dev/null) || verification_count=0
  [[ $verification_count =~ ^[0-9]+$ && $verification_count -le 10 ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  if [[ $external_io != none ]]; then
    [[ $unit_count -ge 1 ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
    [[ $verification_count -ge 1 ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  fi

  local item ikeys
  while IFS= read -r item; do
    [[ -n $item ]] || continue
    ikeys=$(yq -p json -r 'keys | .[]' <<< "$item" 2>/dev/null) || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
    while IFS= read -r k; do case $k in operation | per_unit | growth | stop_condition | reuse) ;; *) WORKLOAD_INVALID_REASON='schema-invalid'; return 1 ;; esac; done <<< "$ikeys"
  done < <(yq -p json -o json -I=0 '.units[]?' <<< "$manifest" 2>/dev/null)

  local operation per_unit growth stop_condition reuse field
  while IFS= read -r item; do
    operation=$(yq -p json -r '.operation // ""' <<< "$item" 2>/dev/null)
    per_unit=$(yq -p json -r '.per_unit // ""' <<< "$item" 2>/dev/null)
    growth=$(yq -p json -r '.growth // ""' <<< "$item" 2>/dev/null)
    stop_condition=$(yq -p json -r '.stop_condition // ""' <<< "$item" 2>/dev/null)
    reuse=$(yq -p json -r '.reuse // ""' <<< "$item" 2>/dev/null)
    for field in "$operation" "$per_unit" "$growth" "$stop_condition" "$reuse"; do
      workload_text_field_safe "$field" 200 || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
    done
    [[ -n $operation && -n $per_unit && -n $growth && -n $stop_condition && -n $reuse ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  done < <(yq -p json -o json -I=0 '.units[]?' <<< "$manifest" 2>/dev/null)

  local verification_text
  while IFS= read -r verification_text; do
    [[ -n $verification_text ]] || continue
    workload_text_field_safe "$verification_text" 200 || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  done < <(yq -p json -r '.verification[]? // ""' <<< "$manifest" 2>/dev/null)

  local amp_keys amp_val
  amp_keys=$(yq -p json -r '.amplification // {} | keys | .[]' <<< "$manifest" 2>/dev/null) || true
  while IFS= read -r k; do [[ -n $k ]] || continue; grep -Fxq "$k" <<< "${WORKLOAD_AMPLIFICATION_KEYS// /$'\n'}" || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }; done <<< "$amp_keys"
  for k in idle failure multi_host; do
    amp_val=$(yq -p json -r ".amplification.$k // \"\"" <<< "$manifest" 2>/dev/null)
    workload_text_field_safe "$amp_val" 200 || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  done

  local exception_count
  exception_count=$(yq -p json -r '.exceptions // [] | length' <<< "$manifest" 2>/dev/null) || exception_count=0
  [[ $exception_count =~ ^[0-9]+$ && $exception_count -le 20 ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  local ekeys site reason track
  while IFS= read -r item; do
    [[ -n $item ]] || continue
    ekeys=$(yq -p json -r 'keys | .[]' <<< "$item" 2>/dev/null) || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
    while IFS= read -r k; do case $k in site | reason | track) ;; *) WORKLOAD_INVALID_REASON='schema-invalid'; return 1 ;; esac; done <<< "$ekeys"
  done < <(yq -p json -o json -I=0 '.exceptions[]?' <<< "$manifest" 2>/dev/null)
  while IFS= read -r item; do
    site=$(yq -p json -r '.site // ""' <<< "$item" 2>/dev/null)
    reason=$(yq -p json -r '.reason // ""' <<< "$item" 2>/dev/null)
    track=$(yq -p json -r '.track // ""' <<< "$item" 2>/dev/null)
    [[ -n $site ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
    workload_text_field_safe "$site" 200 || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
    workload_text_field_safe "$reason" 200 || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
    [[ -z $track || $track =~ ^#[0-9]+$ ]] || { WORKLOAD_INVALID_REASON='schema-invalid'; return 1; }
  done < <(yq -p json -o json -I=0 '.exceptions[]?' <<< "$manifest" 2>/dev/null)

  return 0
}


# --- Verdict ------------------------------------------------------------
# Sets WORKLOAD_MANIFEST, WORKLOAD_MANIFEST_VALID, WORKLOAD_VERDICT (one of
# declared/not-applicable/missing/invalid) and WORKLOAD_DETAIL.
workload_evaluate() {
  local issue=$1 plan_file=$2
  local -g WORKLOAD_MANIFEST WORKLOAD_MANIFEST_VALID WORKLOAD_VERDICT WORKLOAD_DETAIL WORKLOAD_INVALID_REASON
  WORKLOAD_MANIFEST_VALID=0
  WORKLOAD_MANIFEST=$(workload_manifest_from_plan "$plan_file")
  if ! workload_validate_schema "$WORKLOAD_MANIFEST" "$issue"; then
    if [[ $WORKLOAD_INVALID_REASON == missing-record ]]; then WORKLOAD_VERDICT='missing'; else WORKLOAD_VERDICT='invalid'; fi
    WORKLOAD_DETAIL=$WORKLOAD_INVALID_REASON
    return 0
  fi
  WORKLOAD_MANIFEST_VALID=1
  if [[ $(yq -p json -r '.external_io // ""' <<< "$WORKLOAD_MANIFEST" 2>/dev/null) == none ]]; then
    WORKLOAD_VERDICT='not-applicable'
  else
    WORKLOAD_VERDICT='declared'
  fi
  WORKLOAD_DETAIL=''
}


# --- Comment rendering (never transcribes an invalid/unvalidated record) ----
workload_render_declared_body() {
  local issue=$1
  printf '<!-- agentic-loop:workload schema=1 issue=%s verdict=declared -->\n### 有限資源とスケーラビリティのworkload判定\n\n判定: 宣言あり（external_io=%s）\n' \
    "$issue" "$(yq -p json -r '.external_io // ""' <<< "$WORKLOAD_MANIFEST" 2>/dev/null)"
}


workload_render_advisory_body() {
  local issue=$1
  printf '<!-- agentic-loop:workload schema=1 issue=%s verdict=%s detail=%s -->\nworkload recordを検証できませんでした（verdict=%s detail=%s）。record本体はIssueへ転記していません。設定（%s）により処理を継続します。\n' \
    "$issue" "$WORKLOAD_VERDICT" "$WORKLOAD_DETAIL" "$WORKLOAD_VERDICT" "$WORKLOAD_DETAIL" "${WORKLOAD:-warn}"
}


workload_render_gate_body() {
  local issue=$1 reason=$2
  printf '<!-- agentic-loop:needs-input worker=workload reason=%s -->\n### 有限資源とスケーラビリティのworkload判定\n\n判定: %s（detail=%s）\n\n計画段階で `agentic-loop:workload` recordを宣言してください（docs/operations/workload-budget.md参照）。完了処理は行わず、worktree・branch・PR（存在する場合）を保持したまま停止します。このIssueを再度 `agent:queued` にすると処理を継続します。\n' \
    "$reason" "$WORKLOAD_VERDICT" "$WORKLOAD_DETAIL"
}


# --- Gate (called from worker() between the preflight gate and exec) -------
workload_gate() {
  local issue=$1 plan_file=$2 mode=${WORKLOAD:-warn} reason
  [[ $mode == off ]] && return 0
  workload_evaluate "$issue" "$plan_file"
  if [[ $WORKLOAD_VERDICT == declared || $WORKLOAD_VERDICT == not-applicable ]]; then
    [[ $WORKLOAD_VERDICT == declared ]] && comment_issue "$issue" "$(workload_render_declared_body "$issue")" || true
    return 0
  fi
  if [[ $mode != require ]]; then
    comment_issue "$issue" "$(workload_render_advisory_body "$issue")" || true
    return 0
  fi
  case $WORKLOAD_VERDICT in
    missing) reason=workload-missing ;;
    *) reason=workload-invalid ;;
  esac
  progress_touch "$issue" needs-input
  set_issue_state "$issue" needs-input
  project_sync_state "$issue" needs-input
  comment_issue "$issue" "$(workload_render_gate_body "$issue" "$reason")" || true
  return 1
}


# --- Static detection (W1/W2/W3, see docs/operations/workload-budget.md) ---
# Read-only: scans local source files only, zero GitHub API calls. Prints
# "type\tfile\tline" rows on stdout; returns 1 if any row was printed.
workload_scan_targets() {
  local root=$1 f
  for f in "$root"/bin/agentic-loop "$root"/bin/agentic-loop-diagnose "$root"/bin/lib/agentic-loop/*.sh "$root"/.agentic-loop/*.sh; do
    [[ -f $f ]] || continue
    # workload.sh's own source necessarily contains the detection literals
    # (gh api /--paginate patterns) as scanner code, not real invocations.
    [[ $(basename "$f") == workload.sh ]] && continue
    printf '%s\n' "$f"
  done
}


workload_scan() {
  local root=$1 f base check_boundary found=0
  while IFS= read -r f; do
    base=$(basename "$f")
    check_boundary=1
    [[ $base == api.sh ]] && check_boundary=0
    while IFS=$'\t' read -r kind num; do
      [[ -n $kind ]] || continue
      printf '%s\t%s\t%s\n' "$kind" "${f#"$root"/}" "$num"
      found=1
    done < <(awk -v check_boundary="$check_boundary" '
      /^[[:space:]]*$/ { next }
      /^[[:space:]]*#/ { prev = $0; next }
      {
        if ($0 ~ /^[[:space:]]*done([[:space:]]|$)/) depth--
        if (check_boundary && $0 ~ /gh api /) {
          if (prev !~ /# workload-boundary:/) print "boundary\t" FNR
        }
        if ($0 ~ /--paginate/) {
          bounded = ($0 ~ /-f labels=/ || $0 ~ /-f since=/ || $0 ~ /-f sha=/ || $0 ~ /-f head=/)
          annotated = (prev ~ /# workload-unbounded:/)
          if (!bounded && !annotated) print "unbounded\t" FNR
          if (depth > 0 && !annotated) print "aggregation\t" FNR
        }
        if ($0 ~ /^[[:space:]]*(while|for)[[:space:]].*[[:space:]]do[[:space:]]*$/) depth++
        else if ($0 ~ /;[[:space:]]*(while|for)[[:space:]].*[[:space:]]do[[:space:]]*$/) depth++
        prev = $0
      }
    ' "$f")
  done < <(workload_scan_targets "$root")
  return $((found ? 1 : 0))
}


# Human-readable inventory of already-tracked exceptions (`track=#N`
# annotations), for visibility into existing deferred optimizations.
workload_scan_tracked() {
  local root=$1
  local -a targets=()
  [[ -f $root/bin/agentic-loop ]] && targets+=("$root/bin/agentic-loop")
  [[ -d $root/bin/lib/agentic-loop ]] && targets+=("$root/bin/lib/agentic-loop")
  [[ -d $root/.agentic-loop ]] && targets+=("$root/.agentic-loop")
  (( ${#targets[@]} > 0 )) || return 0
  grep -RnoE '# workload-unbounded:.*track=#[0-9]+' "${targets[@]}" 2>/dev/null | sed "s#^$root/##" || true
}


# --- Read-only CLI (bin/agentic-loop workload) ------------------------------
cmd_workload() {
  local format=text
  [[ $# -eq 0 ]] || { [[ $# -eq 2 && $1 == --format && $2 == json ]] || { usage; return 2; }; format=$2; }
  local rows tracked rc=0
  rows=$(workload_scan "$REPO_ROOT") || rc=1
  tracked=$(workload_scan_tracked "$REPO_ROOT")
  if [[ $format == json ]]; then
    local sep='' out='{"schema":1,"violations":['
    local type path line
    while IFS=$'\t' read -r type path line; do
      [[ -n $type ]] || continue
      out+="$sep{\"type\":\"$(json_escape "$type")\",\"path\":\"$(json_escape "$path")\",\"line\":$line}"
      sep=','
    done <<< "$rows"
    out+='],"tracked":['
    sep=''
    while IFS= read -r line; do
      [[ -n $line ]] || continue
      out+="$sep\"$(json_escape "$line")\""
      sep=','
    done <<< "$tracked"
    out+=']}'
    printf '%s\n' "$out"
  else
    if [[ -n $rows ]]; then
      printf '有限資源とスケーラビリティの静的検査で違反を検出しました:\n'
      printf '%s\n' "$rows" | awk -F '\t' '{printf "  [%s] %s:%s\n", $1, $2, $3}'
    else
      printf '有限資源とスケーラビリティの静的検査で違反は見つかりませんでした。\n'
    fi
    if [[ -n $tracked ]]; then
      printf '追跡中の例外（track=#N）:\n'
      printf '%s\n' "$tracked" | sed 's/^/  /'
    fi
  fi
  return $rc
}
