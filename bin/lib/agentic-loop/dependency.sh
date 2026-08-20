# Module: dependency.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034



# --- Issue dependency gating (see docs/operations/issue-queue.md) ---
# An Issue may declare dependencies through GitHub's native issue-dependencies
# REST endpoint and/or a single body line `Blocked by: #12, #34`. claim_next
# treats the union of both as the effective dependency set and only claims an
# Issue once every dependency is closed AND verified complete (agent:completed,
# or for a human-managed Issue with no agent:* label, state_reason=completed).
# Errors fail closed (the Issue is not claimed); persistent errors move it to
# agent:blocked with a reason code instead of silently starving it forever.
dependency_state_dir() { printf '%s/dependency' "$STATE_ROOT"; }

dependency_failure_file() { printf '%s/failures-%s' "$(dependency_state_dir)" "$1"; }

dependency_clear_failure() { rm -f "$(dependency_failure_file "$1")"; }


# Increment the consecutive dependency-check API failure counter for `issue`
# and report (via exit status) whether it has now reached
# DEPENDENCY_FAILURE_TOLERANCE. Below tolerance the caller withholds claiming
# silently (no label/comment churn on a single poll's blip); at tolerance the
# caller moves the Issue to agent:blocked.
dependency_note_failure() {
  local issue=$1 file count
  file=$(dependency_failure_file "$issue")
  mkdir -p "$(dependency_state_dir)"
  count=$(( $(cat "$file" 2>/dev/null || printf 0) + 1 ))
  printf '%s\n' "$count" > "$file"
  (( count >= DEPENDENCY_FAILURE_TOLERANCE ))
}


# Parse the single allowed "Blocked by: #12, #34" body line into issue numbers
# on stdout (one per line; none if the line is absent). Exit 0 on a valid or
# absent line, 1 on invalid syntax (more than one such line, or a non-#N
# token), 2 when a token references another repository (e.g. "owner/repo#1"
# or a URL), which this feature does not support.
dependency_refs_from_body() {
  local body=$1 count line token
  # GitHub本文はWeb UI/API経由ではCRLFになることがある。行末CRを
  # 構文判定の前に除去し、同一repositoryの依存を誤ってcross-repo扱いしない。
  body=${body//$'\r'/}
  count=$(grep -c '^Blocked by:' <<< "$body" || true)
  (( count == 0 )) && return 0
  (( count == 1 )) || return 1
  line=$(grep '^Blocked by:' <<< "$body" | head -n 1)
  line=${line#Blocked by:}; line=${line//,/ }
  for token in $line; do
    [[ -n $token ]] || continue
    if [[ $token =~ ^#([0-9]+)$ ]]; then
      printf '%s\n' "${BASH_REMATCH[1]}"
    elif [[ $token == *#* || $token == http* ]]; then
      return 2
    else
      return 1
    fi
  done
  return 0
}


# The dependency Issue numbers native GitHub records for `issue`, one per
# line on stdout. Exit 0 on success (possibly empty), 2 when the native
# endpoint itself is unavailable (404, e.g. an older GitHub Enterprise or a
# repository without the feature enabled) so the caller falls back to the
# body syntax alone, 3 on missing permission (401/403), 1 on any other
# (transient) API failure.
dependency_native_refs() {
  local issue=$1 error out cache now fetched cached_rc
  mkdir -p "$(dependency_state_dir)"
  cache="$(dependency_state_dir)/native-$issue"
  now=$(date +%s)
  if [[ -r $cache ]]; then
    IFS=$'\t' read -r fetched cached_rc < "$cache" || true
    if [[ $fetched =~ ^[0-9]+$ && $cached_rc =~ ^[02]$ && $((now - fetched)) -lt 120 ]]; then
      tail -n +2 "$cache"
      return "$cached_rc"
    fi
  fi
  error="$(dependency_state_dir)/native-error.$$"
  # workload-unbounded: native依存一覧はIssueに紐づく有限のblocked_by件数を全件取得; bound=blocked_by count; track=#237
  if out=$(repo_api "issues/$issue/dependencies/blocked_by" --paginate --jq '.[].number' 2>"$error"); then
    rm -f "$error"
    { printf '%s\t0\n' "$now"; [[ -z $out ]] || printf '%s\n' "$out"; } > "$cache"
    printf '%s\n' "$out"; return 0
  fi
  if grep -Eqi 'HTTP 404' "$error"; then printf '%s\t2\n' "$now" > "$cache"; rm -f "$error"; return 2; fi
  if grep -Eqi 'HTTP 40[13]' "$error"; then rm -f "$error"; return 3; fi
  rm -f "$error"; return 1
}


# Best-effort (no failure counting, no fail-closed semantics) combined
# native+body dependency refs for an arbitrary Issue, used only to walk the
# graph for cycle detection.
dependency_all_refs_best_effort() {
  local issue=$1 body=$2 b n
  b=$(dependency_refs_from_body "$body" 2>/dev/null) || b=''
  n=$(dependency_native_refs "$issue" 2>/dev/null) || n=''
  printf '%s\n%s\n' "$b" "$n" | sed '/^$/d' | sort -un
}


# Whether a cycle passes back through `start` starting from its known `refs`,
# walking blocked_by transitively up to DEPENDENCY_MAX_DEPTH hops. This only
# affects which reason code is reported (`cycle` vs `incomplete`); a cycle a
# priori also fails the ordinary "closed and verified" check on every hop, so
# depth-limiting cannot cause an incorrect claim.
dependency_cycle_exists() {
  local start=$1 refs=$2 depth=$3 visited=$4 ref next_body next_refs
  for ref in $refs; do
    [[ $ref == "$start" ]] && return 0
    (( depth > DEPENDENCY_MAX_DEPTH )) && continue
    [[ ,$visited, == *,$ref,* ]] && continue
    next_body=$(repo_api "issues/$ref" --jq '.body // ""' 2>/dev/null) || continue
    next_refs=$(dependency_all_refs_best_effort "$ref" "$next_body")
    dependency_cycle_exists "$start" "$next_refs" "$((depth + 1))" "$visited,$ref" && return 0
  done
  return 1
}


# Whether a single dependency Issue is complete. Closing alone is not enough:
# an Issue this Supervisor manages must carry agent:completed (Supervisor-
# verified merge), and a human-managed Issue with no agent:* label must have
# been closed as state_reason=completed (not not_planned or duplicate).
# Returns 0 satisfied, 1 not satisfied, 2 missing (404), 3 permission (401/403),
# 4 other (transient) API error.
dependency_satisfied() {
  local dep=$1 error json state state_reason labels
  mkdir -p "$(dependency_state_dir)"
  error="$(dependency_state_dir)/sat-error.$$"
  # Joined with U+001F (not tab): bash `read` treats consecutive/leading tabs
  # as a single whitespace-class delimiter and silently collapses an empty
  # state_reason field, which is common (GitHub returns state_reason=null for
  # most closed Issues).
  if ! json=$(repo_api "issues/$dep" --jq '[.state, .state_reason // "", ([.labels[].name] | join(","))] | join("")' 2>"$error"); then
    if grep -Eqi 'HTTP 404' "$error"; then rm -f "$error"; return 2; fi
    if grep -Eqi 'HTTP 40[13]' "$error"; then rm -f "$error"; return 3; fi
    rm -f "$error"; return 4
  fi
  rm -f "$error"
  IFS=$'\x1f' read -r state state_reason labels <<< "$json"
  [[ $state == closed ]] || return 1
  [[ ,$labels, == *,agent:completed,* ]] && return 0
  [[ $labels =~ (^|,)agent:[a-zA-Z-]+(,|$) ]] && return 1
  [[ $state_reason == completed ]] && return 0
  return 1
}


# Determine whether `issue` (with body `body`, already fetched by the caller)
# may be claimed now. On success (0) it may. On failure (1) it must wait;
# DEPENDENCY_REASON is set to a reason code (syntax, cross-repo, missing,
# incomplete, cycle, permission, api) once failures cross
# DEPENDENCY_FAILURE_TOLERANCE or a deterministic block is found, with
# DEPENDENCY_DETAIL as a short Japanese note (no secrets). DEPENDENCY_REASON
# stays empty while a transient error is still within tolerance, so the
# caller withholds claiming without moving the Issue to agent:blocked yet.
dependency_status() {
  local issue=$1 body=$2
  DEPENDENCY_REASON=''; DEPENDENCY_DETAIL=''
  local body_out body_rc native_out native_rc combined ref
  body_out=$(dependency_refs_from_body "$body"); body_rc=$?
  case $body_rc in
    1) dependency_clear_failure "$issue"; DEPENDENCY_REASON=syntax
       DEPENDENCY_DETAIL='Issue本文の `Blocked by:` 構文が不正です（1行のみ、同一repositoryの #番号をカンマまたは空白区切りで指定してください）。'
       return 1 ;;
    2) dependency_clear_failure "$issue"; DEPENDENCY_REASON=cross-repo
       DEPENDENCY_DETAIL='Issue本文の `Blocked by:` が別repositoryのIssueを参照しています。同一repository内のIssueのみ依存として扱えます。'
       return 1 ;;
  esac
  native_out=$(dependency_native_refs "$issue"); native_rc=$?
  if (( native_rc == 3 )); then
    dependency_note_failure "$issue" && { DEPENDENCY_REASON=permission; DEPENDENCY_DETAIL='依存関係APIへのアクセス権限が不足しています。`gh auth refresh -s project,read:project` などで権限を確認してください。'; }
    return 1
  fi
  if (( native_rc == 1 )); then
    dependency_note_failure "$issue" && { DEPENDENCY_REASON=api; DEPENDENCY_DETAIL='依存関係APIへの接続が一時的に失敗しています。'; }
    return 1
  fi
  combined=$(printf '%s\n%s\n' "$body_out" "$native_out" | sed '/^$/d' | sort -un)
  if [[ -z $combined ]]; then dependency_clear_failure "$issue"; return 0; fi

  local -a missing=() incomplete=() sat_rc had_api=0 had_perm=0
  for ref in $combined; do
    dependency_satisfied "$ref"; sat_rc=$?
    case $sat_rc in
      0) : ;;
      1) incomplete+=("$ref") ;;
      2) missing+=("$ref") ;;
      3) had_perm=1 ;;
      4) had_api=1 ;;
    esac
  done
  if (( had_perm )); then
    dependency_note_failure "$issue" && { DEPENDENCY_REASON=permission; DEPENDENCY_DETAIL='依存Issueの確認に必要な権限が不足しています。'; }
    return 1
  fi
  if (( had_api )); then
    dependency_note_failure "$issue" && { DEPENDENCY_REASON=api; DEPENDENCY_DETAIL='依存Issueの確認に一時的に失敗しています。'; }
    return 1
  fi
  dependency_clear_failure "$issue"
  if (( ${#missing[@]} > 0 )); then
    DEPENDENCY_REASON=missing; DEPENDENCY_DETAIL="依存Issue $(printf '#%s ' "${missing[@]}")が見つかりません。参照しているIssue番号を確認してください。"
    return 1
  fi
  if (( ${#incomplete[@]} > 0 )); then
    if dependency_cycle_exists "$issue" "$combined" 1 ",$issue"; then
      DEPENDENCY_REASON=cycle
      DEPENDENCY_DETAIL="循環依存を検出しました（$(printf '#%s ' "${incomplete[@]}")などが関係します）。自動では解決できないため、いずれかのIssueの依存を人手で解消してください。"
    else
      DEPENDENCY_REASON=incomplete
      DEPENDENCY_DETAIL="依存Issue $(printf '#%s ' "${incomplete[@]}")が未完了です。依存Issueが完了すると自動的にqueuedへ戻ります。"
    fi
    return 1
  fi
  return 0
}


dependency_block_file() { printf '%s/blocked-%s' "$(dependency_state_dir)" "$1"; }


# Move a queued Issue to agent:blocked, recording the reason so a 30-second
# poll does not repost the same notice every cycle (mirrors
# record_conflict_wait). Only called from claim_next on an Issue still known
# to be agent:queued.
mark_dependency_blocked() {
  local issue=$1 reason=$2 detail=$3 file note
  file=$(dependency_block_file "$issue")
  note=$(printf '%s\t%s' "$reason" "$detail")
  [[ -r $file && $(cat "$file") == "$note" ]] && return 0
  mkdir -p "$(dependency_state_dir)"
  printf '%s\n' "$note" > "$file"
  set_issue_state "$issue" blocked
  project_sync_state "$issue" blocked
  project_sync_conflict "$issue" "依存: $detail"
  comment_issue "$issue" "<!-- agentic-loop:dependency-blocked reason=$reason -->\n依存Issueの検証によりclaimを保留しました（理由: $reason）。$detail" || true
}


# Clear a dependency block once it no longer applies, commenting only when a
# block was actually recorded (idempotent for Issues that were never blocked).
clear_dependency_block() {
  local issue=$1 file
  file=$(dependency_block_file "$issue")
  [[ -r $file ]] || return 0
  rm -f "$file"
  dependency_clear_failure "$issue"
  comment_issue "$issue" '<!-- agentic-loop:dependency-ready -->\n依存Issueがすべて完了したため、claim待機を解除しました。' || true
  project_sync_conflict "$issue" ''
}


# Scan agent:blocked Issues each poll and requeue any whose dependencies are
# now satisfied, so resolution never needs a human Label edit.
requeue_dependency_ready() {
  local issue body_b64 body
  while IFS=$'\t' read -r issue body_b64; do
    [[ -n $issue ]] || continue
    body=$(base64 -d <<< "$body_b64" 2>/dev/null || true)
    if dependency_status "$issue" "$body"; then
      clear_dependency_block "$issue"
      set_issue_state "$issue" queued
      project_sync_state "$issue" queued
    fi
  done < <(snapshot_state_rows blocked | awk -F '\t' '{print $1 "\t" $5}' || repo_api issues --method GET -f state=open -f labels="$(state_label blocked)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | [.number, (.body // "" | @base64)] | @tsv' 2>/dev/null || true)
}
