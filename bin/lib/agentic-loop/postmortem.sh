# Module: postmortem.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034



# --- Closed-loop postmortem learning (Issue #132, ADR 0026, docs/policies/postmortem.md) ---
# Detection (auto or explicit) creates a `postmortem`-labeled Issue with a
# fingerprint marker; action items are linked as native GitHub sub-issues and
# `blocked_by` dependencies exactly like decomposition_materialize (worker.sh)
# links decomposition children, so the existing dependency.sh gate (claim_next/
# requeue_dependency_ready) re-queues the postmortem Issue with zero new code
# once every action item closes and verifies.

postmortem_state_dir() { printf '%s/postmortem' "$STATE_ROOT"; }

postmortem_fingerprint() { printf '%s:%s' "$1" "$2" | sha256sum | cut -c1-8; }

postmortem_marker() { printf 'agentic-loop:postmortem fingerprint=%s kind=%s' "$1" "$2"; }

postmortem_title_prefix() { printf 'ポストモーテム: '; }


# --- Turn-intent marker (worker.sh's terminal branch reads this; see
# docs/decisions/0026-postmortem-closed-loop.md "worker.sh終端分岐"). Written
# to STATE_ROOT so a provider invocation inside the dedicated worktree (whose
# git-common-dir resolves to the same absolute path, see resolve_worker_
# git_common_dir) and the Supervisor's own worker() process observe the same
# file regardless of which one wrote it. ---
postmortem_turn_marker_file() { printf '%s/turn-%s' "$(postmortem_state_dir)" "$1"; }

postmortem_turn_marker_write() {
  mkdir -p "$(postmortem_state_dir)"
  printf '%s\n' "$2" > "$(postmortem_turn_marker_file "$1")"
}

postmortem_turn_marker_read() {
  local file; file=$(postmortem_turn_marker_file "$1")
  [[ -r $file ]] && head -n1 "$file" || printf ''
}

postmortem_turn_marker_clear() { rm -f "$(postmortem_turn_marker_file "$1")"; }


# --- Daily creation cap (bounded, configurable: [postmortem].max_auto_created_per_day) ---
postmortem_daily_count_file() { printf '%s/created-%s' "$(postmortem_state_dir)" "$(date -u +%Y-%m-%d)"; }

postmortem_daily_count() {
  local file; file=$(postmortem_daily_count_file)
  [[ -r $file ]] && cat "$file" || printf '0'
}

postmortem_daily_cap_reached() { (( $(postmortem_daily_count) >= POSTMORTEM_MAX_AUTO_CREATED_PER_DAY )); }

postmortem_record_created() {
  local file count; file=$(postmortem_daily_count_file); count=$(postmortem_daily_count)
  mkdir -p "$(postmortem_state_dir)"
  printf '%s\n' "$((count + 1))" > "$file"
}


# --- Secret boundary (mirrors trace_secret_scan_clean; one shared definition
# of the guard, never a second regex) ---
postmortem_secret_scan_clean() {
  local text=$1 tmp rc=0
  [[ -n $text ]] || return 0
  [[ -x "$REPO_ROOT/.agentic-loop/guard-secrets.sh" ]] || return 1
  tmp=$(mktemp)
  printf '%s' "$text" > "$tmp"
  "$REPO_ROOT/.agentic-loop/guard-secrets.sh" --text "$tmp" >/dev/null 2>&1 || rc=1
  rm -f "$tmp"
  return $rc
}


# --- Dedup: an existing OPEN postmortem for the same kind+subject reuses that
# Issue instead of creating a duplicate. A previously CLOSED postmortem for
# the same fingerprint does not suppress creation: recurrence after a fix was
# believed complete is itself worth a new record. ---
postmortem_find_open() {
  local fingerprint=$1
  repo_api issues --method GET -f state=open -f labels="$POSTMORTEM_LABEL" -f per_page=100 --paginate \
    --jq '.[] | select((.body // "") | contains("fingerprint='"$fingerprint"'")) | .number' 2>/dev/null | head -n1
}


postmortem_category_for_kind() {
  case $1 in
    confidentiality-incident | integrity-incident | availability-incident) printf 'category:%s' "$1" ;;
    *) printf 'category:loop-continuity' ;;
  esac
}


postmortem_body() {
  local kind=$1 subject=$2 title=$3 summary=$4 evidence=$5 fingerprint=$6 body
  body="## 事象\n\n$title\n\n種別: $kind / 対象: $subject\n\n## 概要\n\n${summary:-（要調査）}\n\n"
  if [[ -n $evidence ]]; then
    body+="## evidence\n\n$evidence\n\n"
  else
    body+="## evidence\n\n（自動検出時点のevidenceは秘密走査により省略、または未提供）\n\n"
  fi
  body+="## 時系列\n\n（記入してください）\n\n## 影響範囲・検出方法・復旧内容\n\n（記入してください）\n\n"
  body+="## 直接の引き金\n\n（記入してください）\n\n## 構造的原因と寄与要因\n\n（記入してください）\n\n"
  body+="## なぜ事前検出できなかったか\n\n（記入してください）\n\n## 機能した防御・機能しなかった防御\n\n（記入してください）\n\n"
  body+="## action item\n\n（このIssueに対して \`bin/agentic-loop postmortem link ISSUE番号 ACTION_ISSUE...\` で紐付けてください）\n\n"
  body+="## 残余リスク\n\n（記入してください）\n\n<!-- $(postmortem_marker "$fingerprint" "$kind") subject=$subject -->"
  printf '%s' "$body"
}


# Create (or reuse, via dedup) a postmortem Issue. Prints the Issue number on
# success. kind/subject/title/summary/evidence never trust caller escaping:
# evidence is dropped (never partially redacted) unless it scans clean.
# Returns 1 without creating anything on a reached daily cap or a GitHub
# mutation failure; callers treat this as best-effort (never fatal).
postmortem_create() {
  local kind=$1 subject=$2 title=$3 summary=${4:-} evidence=${5:-} category=${6:-} fingerprint existing body issue clean_evidence=''
  [[ $title == "$(postmortem_title_prefix)"* ]] || title="$(postmortem_title_prefix)$title"
  fingerprint=$(postmortem_fingerprint "$kind" "$subject")
  existing=$(postmortem_find_open "$fingerprint") || existing=''
  if [[ $existing =~ ^[1-9][0-9]*$ ]]; then
    comment_issue "$existing" "<!-- agentic-loop:postmortem-recurrence fingerprint=$fingerprint -->\n同一事象（kind=$kind）の再発またはnear missを検出しました。既存のポストモーテムで追跡します。" || true
    printf '%s' "$existing"; return 0
  fi
  if [[ -n $evidence ]] && postmortem_secret_scan_clean "$evidence"; then clean_evidence=$evidence; fi
  [[ -n $summary ]] && postmortem_secret_scan_clean "$summary" || summary=''
  [[ -n $category ]] || category=$(postmortem_category_for_kind "$kind")
  body=$(postmortem_body "$kind" "$subject" "$title" "$summary" "$clean_evidence" "$fingerprint")
  issue=$(repo_api issues --method POST -f title="$title" -f body="$(unfold_body "$body")" --jq .number 2>/dev/null) || return 1
  [[ $issue =~ ^[1-9][0-9]*$ ]] || return 1
  repo_api "issues/$issue/labels" --method PUT --input - <<< "{\"labels\":[\"$POSTMORTEM_LABEL\",\"$category\",\"$(state_label queued)\"]}" >/dev/null 2>&1 || true
  project_add_issue "$issue" || true
  project_sync_state "$issue" queued || true
  postmortem_record_created
  printf '%s' "$issue"
}


# Best-effort auto-trigger for the two structurally cheap detection points
# (repeated-failure via park_issue, resource-exhaustion via
# exhaustion_note_pause). Never fails the caller: gated on
# [postmortem].auto_detect and swallows its own errors. The daily cap applies
# only to this automatic path -- an explicit `postmortem create` request (from
# a user or a worker) is never blocked by it, only counted toward it.
postmortem_consider_trigger() {
  [[ $POSTMORTEM_AUTO_DETECT == on ]] || return 0
  local kind=$1 subject=$2 title=$3 summary=${4:-}
  postmortem_daily_cap_reached && { say "postmortem: 1日あたりの自動作成上限（$POSTMORTEM_MAX_AUTO_CREATED_PER_DAY）に達したため自動作成を見送りました（kind=$kind subject=$subject）。" >&2; return 0; }
  postmortem_create "$kind" "$subject" "$title" "$summary" '' '' >/dev/null 2>&1 || true
}


# --- action item closed loop -------------------------------------------------
# Link one or more existing action-item Issues to a postmortem Issue as native
# GitHub sub-issues + blocked_by dependencies (the same primitives
# decomposition_materialize uses for its children), then move the postmortem
# Issue to agent:blocked. dependency.sh's existing requeue_dependency_ready
# already re-queues any agent:blocked Issue once every native/body dependency
# closes and verifies -- no new gating code is required for the postmortem to
# resume once its action items are done.
postmortem_link() {
  local postmortem_issue=$1; shift
  local action_issue child_id result
  (( $# > 0 )) || return 1
  for action_issue in "$@"; do
    [[ $action_issue =~ ^[1-9][0-9]*$ ]] || return 1
    # sub_issue_id/issue_id are typed-integer properties (gh api -f always
    # sends strings, which GitHub 422s: Issue #252) and both native
    # endpoints require the target's database id, not its Issue number
    # (verified against the REST docs and the number/id mixup in #252's
    # discussion) -- one lookup here covers both call sites below.
    child_id=$(repo_api "issues/$action_issue" --jq .id 2>/dev/null) || return 1
    if ! result=$(repo_api "issues/$postmortem_issue/sub_issues" --method POST -F sub_issue_id="$child_id" 2>&1); then
      say "postmortem_link: issues/$postmortem_issue/sub_issues への登録に失敗しました(sub_issue_id=$child_id): $result" >&2
      return 1
    fi
    if ! result=$(repo_api "issues/$postmortem_issue/dependencies/blocked_by" --method POST -F issue_id="$child_id" 2>&1); then
      say "postmortem_link: issues/$postmortem_issue/dependencies/blocked_by への登録に失敗しました(issue_id=$child_id): $result" >&2
      return 1
    fi
  done
  lease_release "$postmortem_issue" postmortem-link
  scope_cache_clear "$postmortem_issue"; clear_conflict_wait "$postmortem_issue"; clear_worker_local "$postmortem_issue"
  set_issue_state "$postmortem_issue" blocked
  project_sync_state "$postmortem_issue" blocked || true
  comment_issue "$postmortem_issue" "<!-- agentic-loop:postmortem-link action_items=$* -->\naction item（$*）をnative sub-issue・依存関係として紐付け、\`agent:blocked\`へ移しました。すべて完了・検証されると自動的に再キューされ、統合検証だけを行います。" || true
  # Tells worker.sh's terminal branch (running as the Supervisor's own
  # process, after this exec turn's provider call returns) that this Issue's
  # state transition is already complete and intentional: it must not
  # re-evaluate the exec result against the ordinary merged-PR completion path
  # (there is no PR for a link turn) and overwrite agent:blocked with failed.
  postmortem_turn_marker_write "$postmortem_issue" link
}


# Read-only summary of a postmortem Issue's linked action items and their
# completion state, reusing dependency.sh's existing satisfaction check.
postmortem_status() {
  local issue=$1 body native combined ref rc
  body=$(repo_api "issues/$issue" --jq '.body // ""' 2>/dev/null) || return 1
  native=$(dependency_native_refs "$issue" 2>/dev/null) || native=''
  combined=$(printf '%s\n%s\n' "$(dependency_refs_from_body "$body" 2>/dev/null || true)" "$native" | sed '/^$/d' | sort -un)
  [[ -n $combined ]] || { printf 'action itemはまだ紐付けられていません。\n'; return 0; }
  for ref in $combined; do
    dependency_satisfied "$ref"; rc=$?
    case $rc in
      0) printf '#%s: 完了・検証済み\n' "$ref" ;;
      1) printf '#%s: 未完了\n' "$ref" ;;
      *) printf '#%s: 確認できません(rc=%s)\n' "$ref" "$rc" ;;
    esac
  done
}


# Extract the plain-text body of one `## heading` markdown section (stops at
# the next `## `). Used only to check that a template placeholder was
# actually replaced, never to parse anything trusted as structured data.
postmortem_section_body() {
  local body=$1 heading=$2
  # GitHub本文のCRLFを正規化してからawkの完全一致で見出しを解析する。
  body=${body//$'\r'/}
  awk -v h="## $heading" '
    $0 == h { found=1; next }
    found && /^## / { exit }
    found { print }
  ' <<< "$body"
}


# Mechanical enforcement of the requirement "ポストモーテム本文だけをclose
# してaction itemを未追跡にしない": every linked action item (native +
# body `Blocked by:`, the same union dependency.sh already tracks) must be
# dependency_satisfied, and the body's fill-in-the-blank template placeholder
# and 残余リスク section must have been actually written. Prints a Japanese
# reason (no secrets, no log bodies) on stdout and returns 1 when any check
# fails; returns 0 silently when the postmortem may be marked complete.
postmortem_complete_gate() {
  local issue=$1 body native combined ref unresolved='' residual
  body=$(repo_api "issues/$issue" --jq '.body // ""' 2>/dev/null) || { printf 'Issue本文を取得できませんでした。\n'; return 1; }
  native=$(dependency_native_refs "$issue" 2>/dev/null) || native=''
  combined=$(printf '%s\n%s\n' "$(dependency_refs_from_body "$body" 2>/dev/null || true)" "$native" | sed '/^$/d' | sort -un)
  [[ -n $combined ]] || { printf 'action itemが1件も紐付けられていません。先に `postmortem link` で紐付けてください。\n'; return 1; }
  for ref in $combined; do
    dependency_satisfied "$ref" || unresolved+="#$ref "
  done
  [[ -z $unresolved ]] || { printf 'action item %sが未完了・未検証です。すべて完了・検証されるまでcompleteできません。\n' "$unresolved"; return 1; }
  if grep -Fq '（記入してください）' <<< "$body"; then
    printf '本文に雛形のプレースホルダ（記入してください）が残っています。分析内容を記入してから complete を実行してください。\n'
    return 1
  fi
  # The fingerprint/dedup marker (postmortem_body's trailing HTML comment) has
  # no heading of its own, so it always lands inside the last section (残余
  # リスク). Strip comment lines before checking for emptiness, or a body
  # whose residual-risk placeholder was deleted but never actually filled in
  # would still look "non-empty" and slip past this gate.
  residual=$(postmortem_section_body "$body" '残余リスク' | { grep -v '^<!--' || true; } | sed '/^[[:space:]]*$/d')
  [[ -n $residual ]] || { printf '残余リスク節が空です。実施しない項目の理由と受容する残余リスクを記入してください。\n'; return 1; }
  return 0
}


# Gate-and-mark a postmortem Issue as verified complete. Never closes the
# Issue itself: it only writes the turn marker worker.sh's terminal branch
# reads (see postmortem_link's identical marker) and leaves the actual close
# + worktree/branch cleanup to that already-existing, already-audited code
# path, so the same safety checks (clean worktree, branch not ahead of the
# default branch) apply here too.
postmortem_complete() {
  local issue=$1 reason
  reason=$(postmortem_complete_gate "$issue") && {
    postmortem_turn_marker_write "$issue" complete
    comment_issue "$issue" '<!-- agentic-loop:postmortem-verified -->\n全action itemの完了・検証、および本文の分析項目・残余リスクの記入を確認しました。次のturnで完了処理を行います。' || true
    return 0
  }
  printf '%s' "$reason"
  return 1
}


cmd_postmortem() {
  case ${1:-} in
    create)
      shift
      local kind='' subject='' title='' summary='' evidence_file='' category=''
      while (( $# > 0 )); do
        case $1 in
          --kind) kind=$2; shift 2 ;;
          --subject) subject=$2; shift 2 ;;
          --title) title=$2; shift 2 ;;
          --summary) summary=$2; shift 2 ;;
          --evidence-file) evidence_file=$2; shift 2 ;;
          --category) category=$2; shift 2 ;;
          *) fail "postmortem create: unknown argument $1" ;;
        esac
      done
      [[ -n $kind && -n $subject && -n $title ]] || fail 'postmortem create requires --kind --subject --title'
      local evidence=''
      [[ -z $evidence_file ]] || evidence=$(cat "$evidence_file" 2>/dev/null || true)
      local issue
      issue=$(postmortem_create "$kind" "$subject" "$title" "$summary" "$evidence" "$category") || fail 'postmortem create failed (GitHub API error; explicit requests are never blocked by the daily auto-create cap)'
      printf '%s\n' "$issue"
      ;;
    link)
      shift
      [[ $# -ge 2 ]] || fail 'postmortem link requires ISSUE ACTION_ISSUE...'
      postmortem_link "$@" || fail 'postmortem link failed'
      ;;
    status)
      shift
      [[ $# -ge 1 ]] || fail 'postmortem status requires ISSUE'
      postmortem_status "$1" || fail 'postmortem status failed'
      ;;
    complete)
      shift
      [[ $# -ge 1 ]] || fail 'postmortem complete requires ISSUE'
      local reason
      if reason=$(postmortem_complete "$1"); then
        printf '検証済み: postmortem #%s の完了条件を満たしました。次のturnでworkerが完了処理を行います。\n' "$1"
      else
        fail "postmortem complete failed: $reason"
      fi
      ;;
    *) fail 'usage: postmortem {create --kind K --subject S --title T [--summary S] [--evidence-file PATH] [--category C]|link ISSUE ACTION_ISSUE...|status ISSUE|complete ISSUE}' ;;
  esac
}
