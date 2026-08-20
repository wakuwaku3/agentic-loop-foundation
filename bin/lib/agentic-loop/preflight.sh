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


# The plan stage's own record, persisted at $STATE_ROOT (git-common-dir, so it
# survives across worktrees and is never removed by clear_worker_local). Both
# the gate (preflight_gate, via the caller-supplied plan_file) and the later
# escalation re-evaluation (preflight_reevaluate_diff, which has no plan_file
# argument at some call sites) resolve to this same path, so an envelope's
# declared parts are computed from one record no matter which stage asks.
preflight_plan_file() { printf '%s/issue-%s-plan.txt' "$STATE_ROOT" "$1"; }


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
# A stable 12-hex identifier over the risk envelope, so a re-plan whose prose
# differs but whose declared risk is identical still matches an already-
# granted approval, while an actual change to the declared risk or the
# repository signal mints a new token that must be re-approved. Every caller
# funnels through preflight_envelope_token below -- the only sha256sum call
# site in this file (scripts/lint.sh enforces this) -- so the gate
# (preflight_gate) and the post-exec escalation re-evaluation
# (preflight_reevaluate_diff) always derive the same token for the same
# envelope instead of drifting between a manifest-derived and a
# signal-derived value (see docs/decisions/0020, Issue #218).
#
# The envelope is the union of:
#   - declared parts (preflight_declared_parts): every non-"low" axis:level
#     plus every declared approval.triggers entry, taken ONLY from a record
#     that has passed preflight_validate_schema. A missing record and an
#     invalid record both contribute no declared parts, so they converge on
#     the same envelope for a given signal instead of moving the token.
#   - a signal part (`signal=<reason>`), included only when the signal class
#     is "approval" (a protected path, or the whole repository, is in scope).
preflight_declared_parts() {
  local manifest=$1
  [[ -n $manifest ]] || return 0
  {
    yq -p json -r '.risks[] | select(.level != "low") | "\(.axis):\(.level)"' <<< "$manifest" 2>/dev/null
    yq -p json -r '.approval.triggers[]? // ""' <<< "$manifest" 2>/dev/null
  } | sed '/^$/d'
}


preflight_envelope_token() {
  local issue=$1 declared=$2 signal_class=$3 signal_reason=$4 parts
  parts=$(
    {
      [[ -n $declared ]] && printf '%s\n' "$declared"
      [[ $signal_class == approval ]] && printf 'signal=%s\n' "$signal_reason"
    } | sort -u
  )
  printf 'issue=%s\n%s' "$issue" "$parts" | sha256sum | cut -c1-12
}


preflight_compute_token() {
  local issue=$1 declared=''
  [[ ${PREFLIGHT_MANIFEST_VALID:-0} == 1 ]] && declared=$(preflight_declared_parts "$PREFLIGHT_MANIFEST")
  preflight_envelope_token "$issue" "$declared" "${PREFLIGHT_SIGNAL_CLASS:-none}" "${PREFLIGHT_SIGNAL_REASON:-}"
}


# Whether an authorized operator has posted `bin/agentic-loop preflight
# --approve` for this exact envelope token (see cmd_preflight). The token,
# not recency, decides validity -- an approval never expires on its own --
# but re-scanning the Issue's full comment history on every check grows with
# the Issue's lifetime comment count (Issue #197). A local cache
# (preflight_approval_cache_*) remembers every envelope token already found
# approved (a cache hit costs zero API calls) plus a `since` cursor derived
# from the newest `updated_at` observed so far, so a cache miss only ever
# re-scans comments posted after the last check, not the whole Issue. The
# cache never prunes tokens or rewinds the cursor on its own -- doing so
# could silently un-approve an envelope, breaking ADR 0020's "a token never
# expires" guarantee. Deleting the cache file is always safe: it just falls
# back to a fresh full-history scan on the next check.
preflight_approval_cache_file() { printf '%s/preflight-approvals/issue-%s' "$STATE_ROOT" "$1"; }


preflight_approval_cache_read() {
  local file=$1
  local -g PREFLIGHT_CACHE_SINCE PREFLIGHT_CACHE_TOKENS
  PREFLIGHT_CACHE_SINCE='1970-01-01T00:00:00Z'
  PREFLIGHT_CACHE_TOKENS=''
  [[ -r $file ]] || return 0
  local kind value
  while IFS=$'\t' read -r kind value; do
    case $kind in
      since) [[ -n $value ]] && PREFLIGHT_CACHE_SINCE=$value ;;
      token) PREFLIGHT_CACHE_TOKENS+="$value"$'\n' ;;
    esac
  done < "$file"
  return 0
}


# GitHub's `since` filter is exclusive of the exact instant supplied; storing
# the raw max `updated_at` as the cursor but querying with it verbatim could
# permanently miss an approval comment posted the same second as the cursor.
# Query 60s behind the stored cursor (never below the epoch default) so a
# handful of comments are safely re-scanned instead of an approval going
# missing forever (ADR 0020: a token never expires on its own).
preflight_approval_query_since() {
  local cursor=$1
  [[ $cursor == '1970-01-01T00:00:00Z' ]] && { printf '%s' "$cursor"; return 0; }
  date -u -d "$cursor - 60 seconds" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || printf '%s' "$cursor"
}


preflight_approval_cache_write() {
  local file=$1 since=$2 tokens=$3 tmp
  mkdir -p "$(dirname "$file")"
  tmp="$file.$$"
  {
    printf 'since\t%s\n' "$since"
    sed '/^$/d' <<< "$tokens" | sort -u | sed 's/^/token\t/'
  } > "$tmp"
  mv "$tmp" "$file"
}


preflight_approved() {
  local issue=$1 token=$2
  [[ $token =~ ^[0-9a-f]{12}$ ]] || return 1
  local cache_file since query_since tokens rows kind value new_since='' new_tokens=''
  cache_file=$(preflight_approval_cache_file "$issue")
  preflight_approval_cache_read "$cache_file"
  since=$PREFLIGHT_CACHE_SINCE
  tokens=$PREFLIGHT_CACHE_TOKENS
  grep -Fxq "$token" <<< "$tokens" && return 0
  query_since=$(preflight_approval_query_since "$since")
  rows=$(repo_api "issues/$issue/comments" --method GET -f per_page=100 -f since="$query_since" --paginate --jq '.[] | ("u\t" + (.updated_at // "")), (select(.body // "" | contains("agentic-loop:preflight-approved")) | (.body | scan("token=[0-9a-f]{12}")) | "t\t" + .[6:])' 2>/dev/null) || return 1
  while IFS=$'\t' read -r kind value; do
    case $kind in
      u) [[ -n $value && ( -z $new_since || $value > $new_since ) ]] && new_since=$value ;;
      t) new_tokens+="$value"$'\n' ;;
    esac
  done <<< "$rows"
  [[ -n $new_since ]] || new_since=$since
  tokens=$(printf '%s\n%s' "$tokens" "$new_tokens" | sed '/^$/d' | sort -u)
  preflight_approval_cache_write "$cache_file" "$new_since" "$tokens"
  grep -Fxq "$token" <<< "$tokens"
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
  printf '%s' "$body"
}


preflight_render_advisory_body() {
  local issue=$1 body
  body=$(printf '<!-- agentic-loop:preflight schema=1 issue=%s verdict=%s detail=%s -->\npreflight recordを検証できませんでした（verdict=%s detail=%s）。record本体はIssueへ転記していません。設定（%s）により処理を継続します。\n' \
    "$issue" "$PREFLIGHT_VERDICT" "$PREFLIGHT_DETAIL" "$PREFLIGHT_VERDICT" "$PREFLIGHT_DETAIL" "${PREFLIGHT:-warn}")
  printf '%s' "$body"
}


preflight_render_approved_body() {
  local issue=$1 token=$2 body
  body=$(printf '<!-- agentic-loop:preflight schema=1 issue=%s verdict=%s detail=%s token=%s approved=1 -->\n承認済みenvelope（token=%s）を確認したため、判定 %s（detail=%s）のまま処理を継続します。\n' \
    "$issue" "$PREFLIGHT_VERDICT" "$PREFLIGHT_DETAIL" "$token" "$token" "$PREFLIGHT_VERDICT" "$PREFLIGHT_DETAIL")
  printf '%s' "$body"
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
  printf '%s' "$body"
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
  local issue=$1 head=$2 default_branch=$3 mode=${PREFLIGHT:-warn} measured signal token declared plan_manifest
  [[ $mode == off ]] && return 0
  measured=$(git -C "$REPO_ROOT" diff --name-only "origin/$default_branch" "$head" 2>/dev/null | sed 's#^#path:#' | sort -u)
  [[ -n $measured ]] && worker_update_scope "$issue" "$(scope_apply_exclusive_paths "$measured")"
  preflight_signal_class "$REPO_ROOT" "$issue"
  signal=$PREFLIGHT_SIGNAL_CLASS
  [[ $signal == approval ]] || return 0
  declared=''
  plan_manifest=$(preflight_manifest_from_plan "$(preflight_plan_file "$issue")")
  preflight_validate_schema "$plan_manifest" "$issue" && declared=$(preflight_declared_parts "$plan_manifest")
  token=$(preflight_envelope_token "$issue" "$declared" "$signal" "$PREFLIGHT_SIGNAL_REASON")
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
