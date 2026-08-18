# Module: trace.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034



# --- Requirement traceability (see docs/decisions/0016, docs/operations/traceability.md) ---
# A merged PR may carry one fenced `agentic-loop:traceability` JSON code block
# in its body, mapping each acceptance criterion to the changes and checks
# that satisfied it. This is untrusted provider output: it is only ever a
# claim. trace_evaluate re-observes GitHub (PR body, check-runs on the PR's
# head commit, and the PR's changed-file list) and never trusts the record's
# own assertions about check results or touched paths. trace_gate wraps this
# with the configured enforcement mode (require/warn/off) and never mutates
# GitHub before validation succeeds (or, in warn mode, before it has decided
# to only advise rather than block).
readonly TRACE_RECORD_MAX_BYTES=8192
readonly TRACE_ALLOWED_STATUS='satisfied partial unmet not-applicable superseded'
readonly TRACE_ALLOWED_VERIFICATION='automated manual external none'
readonly TRACE_ALLOWED_CHECK_RESULT='success failure skipped'


# --- Criterion identification (stable across squash merges and line drift) --
# An acceptance criterion is identified by ac-<8 hex chars of sha256> over its
# own normalized text, never by line number or position: reordering or
# renumbering the Issue body's list leaves every id unchanged, while editing a
# criterion's wording produces a new id (the mechanism by which a "conditions
# changed" edit is detected, see trace_validate_coverage).
trace_normalize_criterion() {
  local text=$1
  text=$(sed -E 's/^[[:space:]]*[-*][[:space:]]+(\[[ xX]\][[:space:]]+)?//; s/^[[:space:]]*[0-9]+\.[[:space:]]+//' <<< "$text")
  text=$(sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' <<< "$text")
  tr -s '[:space:]' ' ' <<< "$text"
}


trace_criterion_id() {
  printf 'ac-%s' "$(printf '%s' "$1" | sha256sum | cut -c1-8)"
}


# Extract the bullet/checkbox/numbered lines under a "完了条件" / "受け入れ条件"
# / "Acceptance Criteria" heading in an Issue body, stopping at the next
# heading. An Issue with no such heading derives zero criteria, which relaxes
# trace_validate_coverage to only require that a record exists (see docs/
# operations/traceability.md's legacy-Issue fallback).
trace_derive_criteria() {
  awk '
    /^#{1,6}[[:space:]]*(完了条件|受け入れ条件|Acceptance Criteria)/ { insec=1; next }
    /^#{1,6}/ { if (insec) exit }
    insec && /^[[:space:]]*([-*]|[0-9]+\.)[[:space:]]+/ { print; next }
  ' <<< "$1"
}


trace_derived_ids() {
  local body=$1 line norm
  while IFS= read -r line; do
    [[ -n $line ]] || continue
    norm=$(trace_normalize_criterion "$line")
    [[ -n $norm ]] || continue
    trace_criterion_id "$norm"
  done < <(trace_derive_criteria "$body")
}


# --- PR-body record extraction -----------------------------------------------
trace_manifest_from_pr_body() {
  awk '/^```agentic-loop:traceability[[:space:]]*$/{on=1;next} on && /^```[[:space:]]*$/{exit} on{print}' <<< "$1"
}


# --- Secret boundary ---------------------------------------------------------
# Reuse guard-secrets.sh's one definition of SECRET_PATTERN rather than
# duplicating the regex; the record never reaches a GitHub write until this
# passes.
trace_secret_scan_clean() {
  local text=$1 tmp rc=0
  if [[ ! -x "$REPO_ROOT/.agentic-loop/guard-secrets.sh" ]]; then
    TRACE_INVALID_REASON='guard-unavailable'
    return 1
  fi
  tmp=$(mktemp)
  printf '%s' "$text" > "$tmp"
  "$REPO_ROOT/.agentic-loop/guard-secrets.sh" --text "$tmp" >/dev/null 2>&1 || rc=1
  rm -f "$tmp"
  return $rc
}


# --- Schema validation (never trusts the record; only bounds its shape) -----
# TRACE_INVALID_REASON is one of: missing-record, record-too-large,
# secret-like, guard-unavailable, schema-invalid, criteria-missing,
# evidence-mismatch.
trace_validate_schema() {
  local manifest=$1 issue=$2 size
  TRACE_INVALID_REASON=''
  [[ -n $manifest ]] || { TRACE_INVALID_REASON='missing-record'; return 1; }
  size=$(printf '%s' "$manifest" | wc -c)
  [[ $size -le $TRACE_RECORD_MAX_BYTES ]] || { TRACE_INVALID_REASON='record-too-large'; return 1; }
  trace_secret_scan_clean "$manifest" || { TRACE_INVALID_REASON=${TRACE_INVALID_REASON:-secret-like}; return 1; }
  command -v yq >/dev/null 2>&1 || { TRACE_INVALID_REASON='schema-invalid'; return 1; }

  local top_keys k
  top_keys=$(yq -p json -r 'keys | .[]' <<< "$manifest" 2>/dev/null) || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
  while IFS= read -r k; do
    case $k in schema | issue | criteria) ;; *) TRACE_INVALID_REASON='schema-invalid'; return 1 ;; esac
  done <<< "$top_keys"
  [[ $(yq -p json -r '.schema // ""' <<< "$manifest" 2>/dev/null) == 1 ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
  [[ $(yq -p json -r '.issue // ""' <<< "$manifest" 2>/dev/null) == "$issue" ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }

  local count
  count=$(yq -p json -r '.criteria | length' <<< "$manifest" 2>/dev/null) || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
  [[ $count =~ ^[0-9]+$ && $count -ge 1 ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }

  local ids
  ids=$(yq -p json -r '.criteria[].id' <<< "$manifest" 2>/dev/null) || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
  [[ $(sort -u <<< "$ids" | sed '/^$/d' | wc -l) -eq $count ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }

  local item ikeys id source status verification reason superseded_by
  while IFS= read -r item; do
    ikeys=$(yq -p json -r 'keys | .[]' <<< "$item" 2>/dev/null) || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    while IFS= read -r k; do
      case $k in id | source | status | verification | changes | checks | reason | superseded_by) ;; *) TRACE_INVALID_REASON='schema-invalid'; return 1 ;; esac
    done <<< "$ikeys"
    id=$(yq -p json -r '.id // ""' <<< "$item")
    [[ $id =~ ^ac-[0-9a-f]{8}$ ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    source=$(yq -p json -r '.source // ""' <<< "$item")
    case $source in issue-body | plan) ;; *) TRACE_INVALID_REASON='schema-invalid'; return 1 ;; esac
    status=$(yq -p json -r '.status // ""' <<< "$item")
    grep -Fxq "$status" <<< "${TRACE_ALLOWED_STATUS// /$'\n'}" || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    verification=$(yq -p json -r '.verification // "none"' <<< "$item")
    grep -Fxq "$verification" <<< "${TRACE_ALLOWED_VERIFICATION// /$'\n'}" || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    reason=$(yq -p json -r '.reason // ""' <<< "$item")
    if [[ $status != satisfied ]]; then [[ -n $reason ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }; fi
    [[ ${#reason} -le 300 && $reason != *$'\n'* && $reason != *'`'* ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    superseded_by=$(yq -p json -r '.superseded_by // ""' <<< "$item")
    if [[ $status == superseded ]]; then
      [[ $superseded_by =~ ^ac-[0-9a-f]{8}$ ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    fi

    local ccount ci citem cikeys cpath canchor
    ccount=$(yq -p json -r '.changes // [] | length' <<< "$item" 2>/dev/null) || ccount=0
    [[ $ccount =~ ^[0-9]+$ ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    for ((ci = 0; ci < ccount; ci++)); do
      citem=$(yq -p json -c ".changes[$ci]" <<< "$item" 2>/dev/null) || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
      cikeys=$(yq -p json -r 'keys | .[]' <<< "$citem" 2>/dev/null) || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
      while IFS= read -r k; do case $k in path | anchor) ;; *) TRACE_INVALID_REASON='schema-invalid'; return 1 ;; esac; done <<< "$cikeys"
      cpath=$(yq -p json -r '.path // ""' <<< "$citem")
      [[ $cpath =~ ^[A-Za-z0-9._/-]{1,200}$ ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
      canchor=$(yq -p json -r '.anchor // ""' <<< "$citem")
      [[ -z $canchor || ($canchor =~ ^[A-Za-z0-9._/:-]{1,200}$) ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    done

    local kcount kitem kikeys cname cresult
    kcount=$(yq -p json -r '.checks // [] | length' <<< "$item" 2>/dev/null) || kcount=0
    [[ $kcount =~ ^[0-9]+$ ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    for ((ci = 0; ci < kcount; ci++)); do
      kitem=$(yq -p json -c ".checks[$ci]" <<< "$item" 2>/dev/null) || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
      kikeys=$(yq -p json -r 'keys | .[]' <<< "$kitem" 2>/dev/null) || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
      while IFS= read -r k; do case $k in name | result) ;; *) TRACE_INVALID_REASON='schema-invalid'; return 1 ;; esac; done <<< "$kikeys"
      cname=$(yq -p json -r '.name // ""' <<< "$kitem")
      [[ $cname =~ ^[A-Za-z0-9._/:\ -]{1,100}$ ]] || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
      cresult=$(yq -p json -r '.result // ""' <<< "$kitem")
      grep -Fxq "$cresult" <<< "${TRACE_ALLOWED_CHECK_RESULT// /$'\n'}" || { TRACE_INVALID_REASON='schema-invalid'; return 1; }
    done
  done < <(yq -p json -c '.criteria[]' <<< "$manifest" 2>/dev/null)
  return 0
}


# Every criterion id currently derivable from the Issue body must appear
# somewhere in the record (as any status). A record that omits a changed
# condition's new id -- without marking the old id superseded -- fails with
# criteria-missing (see docs/operations/traceability.md, "条件変更の検出").
trace_validate_coverage() {
  local manifest=$1 issue_body=$2 ids id
  ids=$(yq -p json -r '.criteria[].id' <<< "$manifest" 2>/dev/null)
  while IFS= read -r id; do
    [[ -n $id ]] || continue
    grep -Fxq "$id" <<< "$ids" || { TRACE_INVALID_REASON='criteria-missing'; return 1; }
  done < <(trace_derived_ids "$issue_body")
  return 0
}


# --- GitHub reconciliation (observed facts, never the record's own claims) --
# name<TAB>verdict per observed check-run, mapped onto the record's three-way
# result enum: success/neutral -> success, skipped -> skipped, anything else
# (failure/timed_out/cancelled/action_required/stale, or still in progress) ->
# failure. Only the check-runs attached to the PR's own head commit are ever
# read, so a failing run that was later fixed and re-run at the same head is
# reconciled by its final conclusion only.
trace_checkrun_verdict() {
  yq -p json -r '.[] | [.name, (if (.conclusion == "success" or .conclusion == "neutral") then "success" elif .conclusion == "skipped" then "skipped" else "failure" end)] | @tsv' <<< "$1" 2>/dev/null
}


trace_checks_overall() {
  yq -p json -r '
    ([.[].conclusion, .[].status] | map(select(. != null))) as $s |
    if ($s | any(. == "failure" or . == "timed_out" or . == "cancelled")) then "failure"
    elif ($s | any(. == "in_progress" or . == "queued")) then "in_progress"
    elif (($s | length) > 0 and ($s | all(. == "success" or . == "neutral" or . == "skipped"))) then "success"
    else "unknown" end
  ' <<< "$1" 2>/dev/null
}


# A record may name the shared entry point ("check", meaning `devbox run
# --pure check`/CI as a whole) which has no corresponding check-run to look up
# and is accepted as self-attested; every other name must match an observed
# check-run whose final conclusion equals the claimed result exactly.
trace_reconcile_checks() {
  local manifest=$1 checkruns_tsv=$2 name result observed
  while IFS=$'\t' read -r name result; do
    [[ -n $name ]] || continue
    [[ $name == check ]] && continue
    observed=$(awk -F '\t' -v n="$name" '$1 == n {print $2; exit}' <<< "$checkruns_tsv")
    [[ -n $observed ]] || { TRACE_INVALID_REASON='evidence-mismatch'; return 1; }
    [[ $observed == "$result" ]] || { TRACE_INVALID_REASON='evidence-mismatch'; return 1; }
  done < <(yq -p json -r '.criteria[].checks[]? | [.name, .result] | @tsv' <<< "$manifest" 2>/dev/null)
  return 0
}


# Every path a criterion claims to have changed must be among the PR's own
# observed changed files (up to the first 100, GitHub's default page size).
trace_reconcile_paths() {
  local manifest=$1 files_list=$2 path
  while IFS= read -r path; do
    [[ -n $path ]] || continue
    grep -Fxq "$path" <<< "$files_list" || { TRACE_INVALID_REASON='evidence-mismatch'; return 1; }
  done < <(yq -p json -r '.criteria[].changes[]?.path // empty' <<< "$manifest" 2>/dev/null)
  return 0
}


# Changed files the record does not attribute to any criterion (the reverse
# direction of self-diagnosis: implementation with no requirement). This never
# blocks the gate; it is reported so `trace --audit` and the human summary can
# surface it.
trace_unreferenced_paths_count() {
  local manifest=$1 files_list=$2 referenced f count=0
  referenced=$(yq -p json -r '.criteria[].changes[]?.path // empty' <<< "$manifest" 2>/dev/null | sort -u)
  while IFS= read -r f; do
    [[ -n $f ]] || continue
    grep -Fxq "$f" <<< "$referenced" || count=$((count + 1))
  done <<< "$files_list"
  printf '%s' "$count"
}


trace_status_count() { yq -p json -r --arg s "$2" '[.criteria[] | select(.status == $s)] | length' <<< "$1" 2>/dev/null || printf 0; }
trace_verification_count() { yq -p json -r --arg v "$2" '[.criteria[] | select((.verification // "none") == $v)] | length' <<< "$1" 2>/dev/null || printf 0; }


# --- Evaluation (read-only; never mutates GitHub or comments) ---------------
# Sets TRACE_MANIFEST, TRACE_CHECKRUNS, TRACE_FILES, TRACE_CHECKS_OVERALL,
# TRACE_UNREF, TRACE_INVALID_REASON. Returns 0 only when a schema-valid record
# covers every currently-derivable criterion and reconciles against GitHub's
# observed check-run conclusions (on the PR's head commit) and changed-file
# list (from the PR itself, independent of merge strategy -- this is why
# squash merges need no branch-commit ancestry at all).
trace_evaluate() {
  local issue=$1 pr=$2 head_sha=$3 issue_body
  local -g TRACE_MANIFEST TRACE_CHECKRUNS TRACE_FILES TRACE_CHECKS_OVERALL TRACE_UNREF TRACE_INVALID_REASON
  TRACE_MANIFEST=$(trace_manifest_from_pr_body "$(repo_api "pulls/$pr" --jq '.body // ""' 2>/dev/null || true)")
  issue_body=$(repo_api "issues/$issue" --jq '.body // ""' 2>/dev/null || true)
  if ! trace_validate_schema "$TRACE_MANIFEST" "$issue" || ! trace_validate_coverage "$TRACE_MANIFEST" "$issue_body"; then
    return 1
  fi
  TRACE_CHECKRUNS=$(repo_api "commits/$head_sha/check-runs" --jq '.check_runs' 2>/dev/null || printf '[]')
  TRACE_FILES=$(repo_api "pulls/$pr/files" -f per_page=100 --jq '.[].filename' 2>/dev/null || true)
  if ! trace_reconcile_checks "$TRACE_MANIFEST" "$(trace_checkrun_verdict "$TRACE_CHECKRUNS")" || ! trace_reconcile_paths "$TRACE_MANIFEST" "$TRACE_FILES"; then
    return 1
  fi
  TRACE_CHECKS_OVERALL=$(trace_checks_overall "$TRACE_CHECKRUNS")
  TRACE_UNREF=$(trace_unreferenced_paths_count "$TRACE_MANIFEST" "$TRACE_FILES")
  return 0
}


# --- Rendering ---------------------------------------------------------------
trace_status_ja() { case $1 in satisfied) printf 充足 ;; partial) printf 部分充足 ;; unmet) printf 未充足 ;; not-applicable) printf 対象外 ;; superseded) printf 条件変更 ;; *) printf '%s' "$1" ;; esac; }
trace_verification_ja() { case $1 in automated) printf 自動 ;; manual) printf 手動 ;; external) printf 外部環境 ;; *) printf なし ;; esac; }


trace_render_table() {
  local manifest=$1
  printf '| 条件ID | 状態 | 検証方法 | 対象path | 理由 |\n|---|---|---|---|---|\n'
  while IFS=$'\t' read -r id status verification paths reason; do
    [[ -n $id ]] || continue
    printf '| %s | %s | %s | %s | %s |\n' "$id" "$(trace_status_ja "$status")" "$(trace_verification_ja "$verification")" "${paths:-—}" "${reason:-—}"
  done < <(yq -p json -r '.criteria[] | [.id, .status, (.verification // "none"), ([.changes[]?.path] | join(", ")), (.reason // "")] | @tsv' <<< "$manifest" 2>/dev/null)
}


# The single verdict comment upserted onto the Issue: a machine-readable
# marker line (parsed the same way as every other agentic-loop:* marker, see
# metrics_field) followed by a Japanese human-readable table.
trace_render_verdict() {
  local issue=$1 pr=$2 merge_commit=$3 base=$4 checks=$5 manifest=$6 unref=$7
  local criteria satisfied partial na unmet superseded manual external marker
  criteria=$(yq -p json -r '.criteria | length' <<< "$manifest" 2>/dev/null || printf 0)
  satisfied=$(trace_status_count "$manifest" satisfied)
  partial=$(trace_status_count "$manifest" partial)
  na=$(trace_status_count "$manifest" not-applicable)
  unmet=$(trace_status_count "$manifest" unmet)
  superseded=$(trace_status_count "$manifest" superseded)
  manual=$(trace_verification_count "$manifest" manual)
  external=$(trace_verification_count "$manifest" external)
  marker=$(printf '<!-- agentic-loop:traceability schema=1 issue=%s pr=%s merge_commit=%s base=%s checks=%s criteria=%s satisfied=%s partial=%s not-applicable=%s unmet=%s superseded=%s manual=%s external=%s unreferenced_paths=%s verdict=pass -->' \
    "$issue" "$pr" "$merge_commit" "$base" "$checks" "$criteria" "$satisfied" "$partial" "$na" "$unmet" "$superseded" "$manual" "$external" "$unref")
  printf '%s\n### トレーサビリティ検証結果\n\n%s\n' "$marker" "$(trace_render_table "$manifest")"
}


# --- Verdict comment upsert (PATCH-in-place, mirrors resume_handoff_upsert) -
trace_verdict_file() { printf '%s/workers/%s.trace' "$STATE_ROOT" "$1"; }


trace_verdict_upsert() {
  local issue=$1 body=$2 file id
  file=$(trace_verdict_file "$issue")
  if [[ -r $file ]]; then
    read -r id < "$file" || id=''
    if [[ $id =~ ^[0-9]+$ ]] && repo_api "issues/comments/$id" --method PATCH -f body="$body" >/dev/null 2>&1; then
      return 0
    fi
  fi
  # The local state file is per-host; a different host may have already
  # posted the verdict comment for this Issue. Look for it once (one extra
  # REST(core) read, only paid when the file is missing) before creating a
  # second comment.
  id=$(repo_api "issues/$issue/comments" --method GET -f per_page=100 --jq '[.[] | select(.body | test("<!-- agentic-loop:traceability schema=1"))] | last.id // ""' 2>/dev/null | tr -d '[:space:]' || true)
  if [[ $id =~ ^[0-9]+$ ]] && repo_api "issues/comments/$id" --method PATCH -f body="$body" >/dev/null 2>&1; then
    mkdir -p "$STATE_ROOT/workers"; printf '%s\n' "$id" > "$file"
    return 0
  fi
  id=$(repo_api "issues/$issue/comments" --method POST -f body="$body" --jq '.id' 2>/dev/null | tr -d '[:space:]' || true)
  [[ $id =~ ^[0-9]+$ ]] && { mkdir -p "$STATE_ROOT/workers"; printf '%s\n' "$id" > "$file"; }
}


# --- Completion gate (called from worker() at both completion sites) --------
# off: no evaluation, no comment, no REST calls beyond what the caller already
# made. warn: evaluates and always returns 0 (never blocks completion), but
# posts an advisory comment when the record does not pass. require (the
# strictest mode, not the shipped default -- see docs/decisions/0016 for why):
# evaluates and returns 1 on failure so the caller can fail the Issue without
# closing it or deleting its worktree/branch.
trace_gate() {
  local issue=$1 pr=$2 head_sha=$3 merge_commit=$4 base=$5
  local mode=${TRACEABILITY:-off}
  [[ $mode == off ]] && return 0
  if trace_evaluate "$issue" "$pr" "$head_sha"; then
    trace_verdict_upsert "$issue" "$(trace_render_verdict "$issue" "$pr" "$merge_commit" "$base" "$TRACE_CHECKS_OVERALL" "$TRACE_MANIFEST" "$TRACE_UNREF")"
    return 0
  fi
  if [[ $mode == warn ]]; then
    comment_issue "$issue" "<!-- agentic-loop:traceability schema=1 issue=$issue pr=$pr verdict=warn reason=$TRACE_INVALID_REASON -->\nトレーサビリティ記録を検証しましたが要件を満たしていません（reason=$TRACE_INVALID_REASON）。設定が \`warn\` のため完了処理は継続します。PR本文の \`agentic-loop:traceability\` code blockの修正を推奨します。" || true
    return 0
  fi
  return 1
}


# --- Read-only CLI (bin/agentic-loop trace) ---------------------------------
# The PR most relevant to an Issue: the most recently merged one if any exists,
# else the most recently created open one. Used only by the CLI, never by the
# gate (which always has the PR number from the caller's own observation).
trace_find_pr() {
  local issue=$1 repository branch
  branch="agent/issue-$issue"
  repository=$(repo_name)
  repo_api pulls --method GET -f state=all -f head="${repository%%/*}:$branch" -f per_page=100 --jq '
    (map(select(.merged_at != null)) | sort_by(.merged_at) | last) as $m |
    (map(select(.state == "open")) | sort_by(.created_at) | last) as $o |
    ($m // $o) as $p |
    if $p == null then "" else [($p.number | tostring), ($p.head.sha // ""), ($p.merge_commit_sha // $p.head.sha // ""), ($p.base.ref // "")] | @tsv end
  ' 2>/dev/null || true
}


trace_render_output() {
  local issue=$1 pr=$2 merge_commit=$3 verdict=$4 format=$5
  if [[ $format == json ]]; then
    local criteria_json
    criteria_json=$(yq -p json -o json '.criteria // []' <<< "${TRACE_MANIFEST:-{}}" 2>/dev/null || printf '[]')
    printf '{"issue":%s,"pr":%s,"merge_commit":"%s","checks":"%s","unreferenced_paths":%s,"verdict":"%s","reason":"%s","criteria":%s}\n' \
      "$issue" "$pr" "$merge_commit" "${TRACE_CHECKS_OVERALL:-unknown}" "${TRACE_UNREF:-0}" "$verdict" "${TRACE_INVALID_REASON:-}" "$criteria_json"
  else
    printf 'Issue #%s / PR #%s / merge_commit=%s / checks=%s / verdict=%s\n' "$issue" "$pr" "$merge_commit" "${TRACE_CHECKS_OVERALL:-unknown}" "$verdict"
    if [[ $verdict == fail ]]; then
      printf '理由: %s\n' "${TRACE_INVALID_REASON:-}"
    else
      trace_render_table "$TRACE_MANIFEST"
    fi
  fi
}


trace_cmd_issue() {
  local issue=$1 format=$2 tsv pr head_sha merge_commit base
  tsv=$(trace_find_pr "$issue")
  if [[ -z $tsv ]]; then
    if [[ $format == json ]]; then printf '{"issue":%s,"pr":null,"verdict":"unknown","reason":"no-pr"}\n' "$issue"
    else printf 'Issue #%s に対応するPRが見つかりません。\n' "$issue"; fi
    return 1
  fi
  IFS=$'\t' read -r pr head_sha merge_commit base <<< "$tsv"
  if trace_evaluate "$issue" "$pr" "$head_sha"; then
    trace_render_output "$issue" "$pr" "$merge_commit" pass "$format"
    return 0
  fi
  trace_render_output "$issue" "$pr" "$merge_commit" fail "$format"
  return 1
}


# Repository-wide sweep over recent traceability verdict comments (same
# REST(core) shape as metrics_fetch_events: one paginated repo-wide comments
# read, filtered locally). Flags, in both directions, "requirement without
# implementation/verification" (unmet/partial, or an empty changes/checks
# criterion) and "implementation without requirement" (unreferenced_paths>0).
trace_audit_events() {
  local since=$1
  repo_api issues/comments --method GET -f since="$since" -f sort=created -f direction=asc -f per_page=100 --paginate --jq '
    .[] | select(.body | test("<!-- agentic-loop:traceability schema=1")) |
    [(.issue_url | split("/") | last),
     (.body | capture("<!-- agentic-loop:traceability(?<m>[^>]*)-->").m | gsub("[\t\n\r]"; " "))
    ] | @tsv' 2>/dev/null
}


cmd_trace_audit() {
  local days=$1 format=$2 since row issue marker unmet partial unref checks verdict flagged=0 sep=''
  since=$(metrics_since_iso "$(($(date +%s) - days * 86400))")
  [[ $format == json ]] && printf '{"schema":1,"days":%s,"issues":[' "$days"
  while IFS=$'\t' read -r issue marker; do
    [[ -n $issue ]] || continue
    unmet=$(metrics_field "$marker" unmet); partial=$(metrics_field "$marker" partial)
    unref=$(metrics_field "$marker" unreferenced_paths); checks=$(metrics_field "$marker" checks)
    [[ ${unmet:-0} =~ ^[0-9]+$ ]] || unmet=0; [[ ${partial:-0} =~ ^[0-9]+$ ]] || partial=0; [[ ${unref:-0} =~ ^[0-9]+$ ]] || unref=0
    if (( unmet > 0 || partial > 0 || unref > 0 )) || [[ -n $checks && $checks != success ]]; then
      flagged=1
      if [[ $format == json ]]; then
        printf '%s{"issue":%s,"unmet":%s,"partial":%s,"unreferenced_paths":%s,"checks":"%s"}' "$sep" "$issue" "$unmet" "$partial" "$unref" "$checks"
        sep=','
      else
        printf 'Issue #%s: unmet=%s partial=%s unreferenced_paths=%s checks=%s\n' "$issue" "$unmet" "$partial" "$unref" "$checks"
      fi
    fi
  done < <(trace_audit_events "$since")
  if [[ $format == json ]]; then printf ']}\n'
  elif (( ! flagged )); then printf '対象期間に要求・実装・検証の不整合は見つかりませんでした。\n'; fi
  return 0
}


cmd_trace() {
  local format=text days=30
  if [[ ${1:-} == --audit ]]; then
    shift
    while (( $# > 0 )); do
      case $1 in
        --days) [[ ${2:-} =~ ^[1-9][0-9]*$ ]] || { usage; return 2; }; days=$2; shift 2 ;;
        --format) [[ ${2:-} == text || ${2:-} == json ]] || { usage; return 2; }; format=$2; shift 2 ;;
        *) usage; return 2 ;;
      esac
    done
    cmd_trace_audit "$days" "$format"
    return $?
  fi
  local issue=${1:-}
  [[ $issue =~ ^[1-9][0-9]*$ ]] || { usage; return 2; }
  shift || true
  while (( $# > 0 )); do
    case $1 in
      --format) [[ ${2:-} == text || ${2:-} == json ]] || { usage; return 2; }; format=$2; shift 2 ;;
      *) usage; return 2 ;;
    esac
  done
  trace_cmd_issue "$issue" "$format"
}
