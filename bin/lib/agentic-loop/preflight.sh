# Module: preflight.sh. Sourced by bin/agentic-loop (see docs/decisions/
# 0013-agentic-loop-modules.md). See docs/decisions/0020-change-risk-preflight.md
# and docs/operations/preflight.md.
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034,SC2153



# --- Change-risk preflight (see ADR 0020, docs/operations/preflight.md) -----
# The plan stage may emit one fenced `agentic-loop:preflight` JSON block
# self-declaring risk on 10 fixed axes plus the intended scope/tests/external
# operations/rollback. Like `agentic-loop:traceability` (trace.sh) and
# `agentic-loop:decomposition` (worker.sh), this is untrusted provider output:
# preflight_gate never trusts the record's own claim of "low risk" -- it is
# cross-checked against a `signal` derived only from the repository's own
# capability manifest (.agentic-loop/capabilities.toml) and the Issue's
# already-cached change scope, both of which cost zero additional GitHub API
# calls. A record that is missing or contradicts that signal is treated as
# no less risky than one that honestly self-reports high risk.
readonly PREFLIGHT_SCHEMA_VERSION=1
readonly PREFLIGHT_RECORD_MAX_BYTES=4096
readonly PREFLIGHT_AXES=(security confidentiality integrity availability data_migration external_environment cost compatibility release_deploy rollback)
readonly PREFLIGHT_ALLOWED_LEVELS='low medium high unknown'
readonly PREFLIGHT_ALLOWED_TRIGGERS='destructive irreversible cost security permission external-deploy data-migration rollback-blocked'


# --- Record extraction (mirrors decomposition_manifest_from_plan) -----------
preflight_manifest_from_plan() {
  awk '/^```agentic-loop:preflight[[:space:]]*$/{on=1;next} on && /^```[[:space:]]*$/{exit} on{print}' "$1" 2>/dev/null
}


preflight_text_field_safe() {
  local text=$1 max=$2
  [[ ${#text} -le $max && $text != *$'\n'* && $text != *'`'* ]]
}


# --- Schema validation (never trusts the record; only bounds its shape) ----
# PREFLIGHT_INVALID_REASON is one of: missing-record, record-too-large,
# secret-like, guard-unavailable, schema-invalid, claim-mismatch.
preflight_validate_schema() {
  local manifest=$1 issue=$2 size
  PREFLIGHT_INVALID_REASON=''
  [[ -n $manifest ]] || { PREFLIGHT_INVALID_REASON='missing-record'; return 1; }
  size=$(printf '%s' "$manifest" | wc -c)
  [[ $size -le $PREFLIGHT_RECORD_MAX_BYTES ]] || { PREFLIGHT_INVALID_REASON='record-too-large'; return 1; }
  trace_secret_scan_clean "$manifest" || { PREFLIGHT_INVALID_REASON=${TRACE_INVALID_REASON:-secret-like}; return 1; }
  command -v yq >/dev/null 2>&1 || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }

  local top_keys k
  top_keys=$(yq -p json -r 'keys | .[]' <<< "$manifest" 2>/dev/null) || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
  while IFS= read -r k; do
    case $k in schema | issue | risks | change | approval) ;; *) PREFLIGHT_INVALID_REASON='schema-invalid'; return 1 ;; esac
  done <<< "$top_keys"
  [[ $(yq -p json -r '.schema // ""' <<< "$manifest" 2>/dev/null) == "$PREFLIGHT_SCHEMA_VERSION" ]] || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
  [[ $(yq -p json -r '.issue // ""' <<< "$manifest" 2>/dev/null) == "$issue" ]] || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }

  local count
  count=$(yq -p json -r '.risks | length' <<< "$manifest" 2>/dev/null) || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
  [[ $count == "${#PREFLIGHT_AXES[@]}" ]] || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }

  local item ikeys
  while IFS= read -r item; do
    [[ -n $item ]] || continue
    ikeys=$(yq -p json -r 'keys | .[]' <<< "$item" 2>/dev/null) || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
    while IFS= read -r k; do case $k in axis | level | reason | missing) ;; *) PREFLIGHT_INVALID_REASON='schema-invalid'; return 1 ;; esac; done <<< "$ikeys"
  done < <(yq -p json -o json -I=0 '.risks[]' <<< "$manifest" 2>/dev/null)

  local axis level reason missing seen=''
  while IFS=$'\t' read -r axis level reason missing; do
    grep -Fxq "$axis" <<< "$(printf '%s\n' "${PREFLIGHT_AXES[@]}")" || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
    grep -Fxq "$axis" <<< "$seen" && { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
    seen+="$axis"$'\n'
    grep -Fxq "$level" <<< "${PREFLIGHT_ALLOWED_LEVELS// /$'\n'}" || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
    if [[ $level != low ]]; then
      [[ -n $reason ]] || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
    fi
    preflight_text_field_safe "$reason" 200 || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
    if [[ $level == unknown ]]; then
      [[ -n $missing ]] || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
      preflight_text_field_safe "$missing" 200 || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
    fi
  done < <(yq -p json -r '.risks[] | [.axis, .level, (.reason // ""), (.missing // "")] | @tsv' <<< "$manifest" 2>/dev/null)
  [[ $(sort -u <<< "$seen" | sed '/^$/d' | wc -l) -eq ${#PREFLIGHT_AXES[@]} ]] || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }

  local change_keys
  change_keys=$(yq -p json -r '.change // {} | keys | .[]' <<< "$manifest" 2>/dev/null) || true
  while IFS= read -r k; do [[ -n $k ]] || continue; case $k in scope | tests | external_operations | rollback) ;; *) PREFLIGHT_INVALID_REASON='schema-invalid'; return 1 ;; esac; done <<< "$change_keys"
  preflight_text_field_safe "$(yq -p json -r '.change.scope // ""' <<< "$manifest" 2>/dev/null)" 200 || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
  preflight_text_field_safe "$(yq -p json -r '.change.rollback // ""' <<< "$manifest" 2>/dev/null)" 200 || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
  local list_count item_text
  for k in tests external_operations; do
    list_count=$(yq -p json -r ".change.$k // [] | length" <<< "$manifest" 2>/dev/null) || list_count=0
    [[ $list_count =~ ^[0-9]+$ && $list_count -le 10 ]] || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
    while IFS= read -r item_text; do
      [[ -n $item_text ]] || continue
      preflight_text_field_safe "$item_text" 200 || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
    done < <(yq -p json -r ".change.${k}[]? // \"\"" <<< "$manifest" 2>/dev/null)
  done

  local approval_keys required trig
  approval_keys=$(yq -p json -r '.approval // {} | keys | .[]' <<< "$manifest" 2>/dev/null) || true
  while IFS= read -r k; do [[ -n $k ]] || continue; case $k in required | triggers) ;; *) PREFLIGHT_INVALID_REASON='schema-invalid'; return 1 ;; esac; done <<< "$approval_keys"
  required=$(yq -p json -r '.approval.required // false' <<< "$manifest" 2>/dev/null)
  [[ $required == true || $required == false ]] || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
  while IFS= read -r trig; do
    [[ -n $trig ]] || continue
    grep -Fxq "$trig" <<< "${PREFLIGHT_ALLOWED_TRIGGERS// /$'\n'}" || { PREFLIGHT_INVALID_REASON='schema-invalid'; return 1; }
  done < <(yq -p json -r '.approval.triggers[]? // ""' <<< "$manifest" 2>/dev/null)

  local high_count trigger_count
  high_count=$(yq -p json -r '[.risks[] | select(.level == "high")] | length' <<< "$manifest" 2>/dev/null) || high_count=0
  trigger_count=$(yq -p json -r '.approval.triggers // [] | length' <<< "$manifest" 2>/dev/null) || trigger_count=0
  if [[ $required == false ]] && { [[ $high_count -gt 0 ]] || [[ $trigger_count -gt 0 ]]; }; then
    PREFLIGHT_INVALID_REASON='claim-mismatch'; return 1
  fi
  return 0
}


# --- Signal (repository capability manifest + already-cached change scope) -
# Whether a normalized scope token set (one per line, see scope.sh) touches a
# repository-relative path, honoring the "*" (whole repository) sentinel.
preflight_scope_touches_path() {
  local tokens=$1 target=$2 t p
  grep -Fxq '*' <<< "$tokens" && return 0
  while IFS= read -r t; do
    [[ $t == path:* ]] || continue
    p=${t#path:}
    [[ $p == "$target" || $target == "$p"/* || $p == "$target"/* ]] && return 0
  done <<< "$tokens"
  return 1
}


# Sets PREFLIGHT_SIGNAL_CLASS ("approval": a protected path requiring
# approval, or the whole repository, is in scope; "medium": a declared
# external environment is in scope; "none": otherwise) and PREFLIGHT_SIGNAL_
# REASON. A plain statement, never command substitution: both are globals a
# caller reads afterward (see docs/decisions/0020) -- printing the class via
# stdout and capturing it with $(...) would run this function in a subshell,
# silently discarding the very globals it exists to set. Costs zero GitHub API
# calls: capabilities.toml is read from the local worktree and scope tokens
# come from the already-populated scope cache (see scope.sh).
preflight_signal_class() {
  local repo_root=$1 issue=$2 tokens path requires env_name
  local -g PREFLIGHT_SIGNAL_CLASS PREFLIGHT_SIGNAL_REASON
  PREFLIGHT_SIGNAL_CLASS='none'
  PREFLIGHT_SIGNAL_REASON=''
  tokens=$(scope_cache_read "$issue")
  capability_load "$repo_root" >/dev/null 2>&1 || return 0
  while IFS=$'\t' read -r path requires; do
    [[ -n $path ]] || continue
    [[ $requires == approval ]] || continue
    if preflight_scope_touches_path "$tokens" "$path"; then
      PREFLIGHT_SIGNAL_CLASS='approval'; PREFLIGHT_SIGNAL_REASON="protected:$path"
      return 0
    fi
  done < <(capability_query '.protected[]? | [.path, (.change_requires // "")] | @tsv')
  if grep -Fxq '*' <<< "$tokens"; then
    PREFLIGHT_SIGNAL_CLASS='approval'; PREFLIGHT_SIGNAL_REASON='paths:*'
    return 0
  fi
  while IFS= read -r env_name; do
    [[ -n $env_name ]] || continue
    if grep -Fxq "env:$env_name" <<< "$tokens"; then
      PREFLIGHT_SIGNAL_CLASS='medium'; PREFLIGHT_SIGNAL_REASON="external_environment:$env_name"
      return 0
    fi
  done < <(capability_query '.external_environment[]?.name // ""')
}


# --- Verdict (combines the record, never trusted alone, with the signal) ---
# Sets PREFLIGHT_MANIFEST, PREFLIGHT_SIGNAL_CLASS, PREFLIGHT_VERDICT (one of
# autonomous/missing/invalid/undetermined/approval-required/signal-mismatch)
# and PREFLIGHT_DETAIL.
preflight_evaluate() {
  local repo_root=$1 issue=$2 plan_file=$3 signal unknown_count high_count trigger_count
  local -g PREFLIGHT_MANIFEST PREFLIGHT_MANIFEST_VALID PREFLIGHT_SIGNAL_CLASS PREFLIGHT_SIGNAL_REASON PREFLIGHT_VERDICT PREFLIGHT_DETAIL PREFLIGHT_INVALID_REASON
  PREFLIGHT_MANIFEST_VALID=0
  PREFLIGHT_MANIFEST=$(preflight_manifest_from_plan "$plan_file")
  preflight_signal_class "$repo_root" "$issue"
  signal=$PREFLIGHT_SIGNAL_CLASS
  if ! preflight_validate_schema "$PREFLIGHT_MANIFEST" "$issue"; then
    # The record failed validation (or is absent): never render its raw text
    # into a comment from here on (see preflight_render_gate_body /
    # preflight_render_audit_body, which gate on PREFLIGHT_MANIFEST_VALID
    # rather than on PREFLIGHT_MANIFEST's mere presence).
    if [[ $PREFLIGHT_INVALID_REASON == missing-record ]]; then
      if [[ $signal == approval ]]; then
        PREFLIGHT_VERDICT='signal-mismatch'; PREFLIGHT_DETAIL=$PREFLIGHT_SIGNAL_REASON
      else
        PREFLIGHT_VERDICT='missing'; PREFLIGHT_DETAIL='missing-record'
      fi
    else
      PREFLIGHT_VERDICT='invalid'; PREFLIGHT_DETAIL=$PREFLIGHT_INVALID_REASON
    fi
    return 0
  fi
  PREFLIGHT_MANIFEST_VALID=1
  unknown_count=$(yq -p json -r '[.risks[] | select(.level == "unknown")] | length' <<< "$PREFLIGHT_MANIFEST" 2>/dev/null) || unknown_count=0
  if [[ $unknown_count -gt 0 ]]; then
    PREFLIGHT_VERDICT='undetermined'
    PREFLIGHT_DETAIL=$(yq -p json -r '[.risks[] | select(.level == "unknown") | .axis] | join(",")' <<< "$PREFLIGHT_MANIFEST" 2>/dev/null)
    return 0
  fi
  high_count=$(yq -p json -r '[.risks[] | select(.level == "high")] | length' <<< "$PREFLIGHT_MANIFEST" 2>/dev/null) || high_count=0
  trigger_count=$(yq -p json -r '.approval.triggers // [] | length' <<< "$PREFLIGHT_MANIFEST" 2>/dev/null) || trigger_count=0
  if [[ $high_count -gt 0 || $trigger_count -gt 0 ]]; then
    PREFLIGHT_VERDICT='approval-required'
    PREFLIGHT_DETAIL=$(yq -p json -r '[.risks[] | select(.level == "high") | .axis] | join(",")' <<< "$PREFLIGHT_MANIFEST" 2>/dev/null)
    return 0
  fi
  if [[ $signal == approval ]]; then
    PREFLIGHT_VERDICT='signal-mismatch'
    PREFLIGHT_DETAIL=$PREFLIGHT_SIGNAL_REASON
    return 0
  fi
  PREFLIGHT_VERDICT='autonomous'
  PREFLIGHT_DETAIL=''
}


# --- Approval envelope token -------------------------------------------------
# A stable 12-hex identifier over the risk envelope (every non-low axis/level
# plus every declared trigger), so a re-plan whose prose differs but whose
# declared risk is identical still matches an already-granted approval, while
# any actual change to the declared risk mints a new token that must be
# re-approved.
preflight_token() {
  local issue=$1 manifest=$2 parts
  parts=$(
    {
      yq -p json -r '.risks[] | select(.level != "low") | "\(.axis):\(.level)"' <<< "$manifest" 2>/dev/null
      yq -p json -r '.approval.triggers[]? // ""' <<< "$manifest" 2>/dev/null
    } | sed '/^$/d' | sort -u
  )
  printf 'issue=%s\n%s' "$issue" "$parts" | sha256sum | cut -c1-12
}


preflight_signal_token() {
  local issue=$1 reason=$2
  printf 'issue=%s\nsignal=%s' "$issue" "$reason" | sha256sum | cut -c1-12
}


preflight_compute_token() {
  local issue=$1
  if [[ -n ${PREFLIGHT_MANIFEST:-} ]]; then
    preflight_token "$issue" "$PREFLIGHT_MANIFEST"
  else
    preflight_signal_token "$issue" "${PREFLIGHT_SIGNAL_REASON:-}"
  fi
}


# Whether an authorized operator has posted `bin/agentic-loop preflight
# --approve` for this exact envelope token (see cmd_preflight). One REST(core)
# read, paginated across every comment (the token, not recency, decides
# validity -- an approval never expires on its own).
preflight_approved() {
  local issue=$1 token=$2 found
  [[ $token =~ ^[0-9a-f]{12}$ ]] || return 1
  found=$(repo_api "issues/$issue/comments" --method GET -f per_page=100 --paginate --jq '[.[] | select((.body | contains("agentic-loop:preflight-approved")) and (.body | contains("token='"$token"'")))] | length' 2>/dev/null || printf 0)
  [[ $found =~ ^[1-9][0-9]*$ ]]
}


# --- Comment rendering (never transcribes an invalid/unvalidated record) ----
preflight_render_risks() {
  local manifest=$1 axis level rtext mtext
  while IFS=$'\t' read -r axis level rtext mtext; do
    [[ -n $axis ]] || continue
    printf -- '- %s: %s' "$axis" "$level"
    [[ -n $rtext ]] && printf ' 根拠: %s' "$rtext"
    [[ -n $mtext ]] && printf ' 不足情報: %s' "$mtext"
    printf '\n'
  done < <(yq -p json -r '.risks[] | [.axis, .level, (.reason // ""), (.missing // "")] | @tsv' <<< "$manifest" 2>/dev/null)
}


preflight_render_change_summary() {
  local manifest=$1 triggers scope tests extops rollback
  triggers=$(yq -p json -r '.approval.triggers // [] | join(", ")' <<< "$manifest" 2>/dev/null)
  scope=$(yq -p json -r '.change.scope // ""' <<< "$manifest" 2>/dev/null)
  tests=$(yq -p json -r '.change.tests // [] | join(", ")' <<< "$manifest" 2>/dev/null)
  extops=$(yq -p json -r '.change.external_operations // [] | join(", ")' <<< "$manifest" 2>/dev/null)
  rollback=$(yq -p json -r '.change.rollback // ""' <<< "$manifest" 2>/dev/null)
  [[ -n $triggers ]] && printf -- '- 承認trigger: %s\n' "$triggers"
  [[ -n $scope ]] && printf -- '- 対象scope: %s\n' "$scope"
  [[ -n $tests ]] && printf -- '- 必要test: %s\n' "$tests"
  [[ -n $extops ]] && printf -- '- 外部操作: %s\n' "$extops"
  [[ -n $rollback ]] && printf -- '- rollback案: %s\n' "$rollback"
}


preflight_render_audit_body() {
  local issue=$1 body
  body=$(
    printf '<!-- agentic-loop:preflight schema=1 issue=%s verdict=autonomous -->\n' "$issue"
    printf '### 変更影響とリスクのpreflight判定\n\n判定: 自律実行（全リスク軸が低〜中、承認triggerなし）\n\n'
    [[ ${PREFLIGHT_MANIFEST_VALID:-0} == 1 ]] && preflight_render_risks "$PREFLIGHT_MANIFEST"
  )
  printf '%s' "${body//$'\n'/\\n}"
}


preflight_render_advisory_body() {
  local issue=$1 body
  body=$(printf '<!-- agentic-loop:preflight schema=1 issue=%s verdict=%s detail=%s -->\npreflight recordを検証できませんでした（verdict=%s detail=%s）。record本体はIssueへ転記していません。設定（%s）により処理を継続します。\n' \
    "$issue" "$PREFLIGHT_VERDICT" "$PREFLIGHT_DETAIL" "$PREFLIGHT_VERDICT" "$PREFLIGHT_DETAIL" "${PREFLIGHT:-warn}")
  printf '%s' "${body//$'\n'/\\n}"
}


preflight_render_approved_body() {
  local issue=$1 token=$2 body
  body=$(printf '<!-- agentic-loop:preflight schema=1 issue=%s verdict=%s detail=%s token=%s approved=1 -->\n承認済みenvelope（token=%s）を確認したため、判定 %s（detail=%s）のまま処理を継続します。\n' \
    "$issue" "$PREFLIGHT_VERDICT" "$PREFLIGHT_DETAIL" "$token" "$token" "$PREFLIGHT_VERDICT" "$PREFLIGHT_DETAIL")
  printf '%s' "${body//$'\n'/\\n}"
}


preflight_render_gate_body() {
  local issue=$1 token=$2 reason=$3 body
  body=$(
    printf '<!-- agentic-loop:needs-input worker=preflight reason=%s token=%s -->\n' "$reason" "$token"
    printf '### 変更影響とリスクのpreflight判定\n\n判定: %s（detail=%s、token=%s）\n\n' "$PREFLIGHT_VERDICT" "$PREFLIGHT_DETAIL" "$token"
    if [[ ${PREFLIGHT_MANIFEST_VALID:-0} == 1 ]]; then
      preflight_render_risks "$PREFLIGHT_MANIFEST"
      preflight_render_change_summary "$PREFLIGHT_MANIFEST"
    else
      printf -- '- 詳細: %s\n' "$PREFLIGHT_DETAIL"
    fi
    printf '\n承認するには、認可済みの運用者が次を実行してください: `bin/agentic-loop preflight %s --approve --token %s`\n' "$issue" "$token"
    printf '完了処理は行わず、worktree・branch・PR（存在する場合）を保持したまま停止します。\n'
  )
  printf '%s' "${body//$'\n'/\\n}"
}


# --- Gate (called from worker() between the plan and exec stages) ----------
# Returns 0 when the worker may proceed to exec, 1 when it must stop (the
# Issue has already been set to agent:needs-input with an explanatory
# comment). `off` performs no evaluation and posts no comment at all.
preflight_gate() {
  local issue=$1 plan_file=$2 mode=${PREFLIGHT:-warn} token reason
  [[ $mode == off ]] && return 0
  preflight_evaluate "$REPO_ROOT" "$issue" "$plan_file"
  if [[ $PREFLIGHT_VERDICT == autonomous ]]; then
    comment_issue "$issue" "$(preflight_render_audit_body "$issue")" || true
    return 0
  fi
  if [[ $PREFLIGHT_VERDICT == missing || $PREFLIGHT_VERDICT == invalid ]] && [[ $mode != require ]]; then
    comment_issue "$issue" "$(preflight_render_advisory_body "$issue")" || true
    return 0
  fi
  token=$(preflight_compute_token "$issue")
  if preflight_approved "$issue" "$token"; then
    PREFLIGHT_APPROVAL_TOKEN=$token
    comment_issue "$issue" "$(preflight_render_approved_body "$issue" "$token")" || true
    return 0
  fi
  case $PREFLIGHT_VERDICT in
    undetermined) reason=preflight-undetermined ;;
    signal-mismatch) reason=preflight-signal-mismatch ;;
    missing) reason=preflight-missing ;;
    invalid) reason=preflight-invalid ;;
    *) reason=preflight-approval ;;
  esac
  progress_touch "$issue" needs-input
  set_issue_state "$issue" needs-input
  project_sync_state "$issue" needs-input
  comment_issue "$issue" "$(preflight_render_gate_body "$issue" "$token" "$reason")" || true
  return 1
}


# --- Escalation re-evaluation (called after the measured diff grows scope) --
# A mechanical backstop, not a pre-merge block: by the time this runs, exec
# may already have pushed/merged a PR in the same turn (see ADR 0020's
# discussion of this limitation). It only ever withholds the *completion*
# confirmation (cleanup + close), mirroring trace_gate's position in worker().
preflight_reevaluate_diff() {
  local issue=$1 head=$2 default_branch=$3 mode=${PREFLIGHT:-warn} measured signal token
  [[ $mode == off ]] && return 0
  measured=$(git -C "$REPO_ROOT" diff --name-only "origin/$default_branch" "$head" 2>/dev/null | sed 's#^#path:#' | sort -u)
  [[ -n $measured ]] && worker_update_scope "$issue" "$(scope_apply_exclusive_paths "$measured")"
  preflight_signal_class "$REPO_ROOT" "$issue"
  signal=$PREFLIGHT_SIGNAL_CLASS
  [[ $signal == approval ]] || return 0
  token=$(preflight_signal_token "$issue" "$PREFLIGHT_SIGNAL_REASON")
  preflight_approved "$issue" "$token" && return 0
  progress_touch "$issue" needs-input
  set_issue_state "$issue" needs-input
  project_sync_state "$issue" needs-input
  comment_issue "$issue" "<!-- agentic-loop:needs-input worker=preflight reason=preflight-escalation token=$token -->\n実装中に変更scopeが広がり、repository manifestが承認を必要とする対象（$PREFLIGHT_SIGNAL_REASON）に到達しました。完了処理は行わず、worktree・branch・PR（存在する場合）を保持したまま停止します。承認するには認可済みの運用者が次を実行してください: \`bin/agentic-loop preflight $issue --approve --token $token\`。承認後、このIssueを再度 \`agent:queued\` にすると処理を継続します。" || true
  return 1
}


# --- Read-only + approval CLI (bin/agentic-loop preflight) ------------------
cmd_preflight() {
  local issue=${1:-}
  [[ $issue =~ ^[1-9][0-9]*$ ]] || { usage; return 2; }
  shift || true
  local format=text approve=0 token='' note=''
  while (( $# > 0 )); do
    case $1 in
      --format) [[ ${2:-} == text || ${2:-} == json ]] || { usage; return 2; }; format=$2; shift 2 ;;
      --approve) approve=1; shift ;;
      --token) token=${2:-}; shift 2 ;;
      --note) note=${2:-}; shift 2 ;;
      *) usage; return 2 ;;
    esac
  done
  if (( approve )); then
    [[ $token =~ ^[0-9a-f]{12}$ ]] || fail 'preflight --approve requires --token <12桁16進>'
    local actor state labels
    actor=$(authorized_operator) || fail 'preflight --approve requires authenticated repository write, maintain, or admin permission'
    IFS=$'\t' read -r state labels < <(issue_agent_state "$issue") || fail "cannot read Issue #$issue"
    comment_issue "$issue" "<!-- agentic-loop:preflight-approved schema=1 actor=$actor token=$token at=$(date +%s) -->\n認可済みの承認操作により、preflight envelope（token=$token）を承認しました。${note:+ 備考: $note}" || fail 'could not post the approval marker'
    if [[ $state == open && ,$labels, == *,agent:needs-input,* ]]; then
      set_issue_state "$issue" queued
      project_sync_state "$issue" queued
      comment_issue "$issue" '<!-- agentic-loop:preflight-resume -->\npreflightの承認を確認したため、このIssueをキューへ戻しました。' || true
    fi
    say "Issue #$issue のpreflight envelope（token=$token）を承認しました。"
    return 0
  fi
  local tokens signal
  tokens=$(scope_cache_read "$issue")
  preflight_signal_class "$REPO_ROOT" "$issue"
  signal=$PREFLIGHT_SIGNAL_CLASS
  if [[ $format == json ]]; then
    printf '{"schema":1,"issue":%s,"scope":"%s","signal":"%s","signal_reason":"%s"}\n' "$issue" "$(json_escape "$tokens")" "$signal" "$(json_escape "${PREFLIGHT_SIGNAL_REASON:-}")"
  else
    printf 'Issue #%s / signal=%s' "$issue" "$signal"
    [[ -n ${PREFLIGHT_SIGNAL_REASON:-} ]] && printf ' (%s)' "$PREFLIGHT_SIGNAL_REASON"
    printf '\n'
    [[ -n $tokens ]] && printf '対象scope: %s\n' "$(paste -sd', ' - <<< "$tokens")"
  fi
  [[ $signal != approval ]]
}
