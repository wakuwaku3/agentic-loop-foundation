# Module: project.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034



set_issue_state() {
  local issue=$1 target=$2 labels
  labels=$(repo_api "issues/$issue" --jq '[.labels[].name | select(startswith("agent:") | not)] + ["agent:'"$target"'"]') || return 1
  printf '%s\n' "$labels" | repo_api "issues/$issue/labels" --method PUT --input - >/dev/null
}


comment_issue() { comment_post "$1" "$2" >/dev/null; }


queue_project_sync() {
  # This file is deliberately only a wake-up hint.  Desired Project values are
  # never persisted locally: they are derived from a fresh GitHub Issue read
  # immediately before a mutation.
  local hint=$1 issue
  if [[ $hint =~ ^([a-z-]+[[:space:]]+)?([1-9][0-9]*) ]]; then issue=${BASH_REMATCH[2]};
  elif [[ $hint =~ /issues/([1-9][0-9]*)$ ]]; then issue=${BASH_REMATCH[1]};
  else return 0; fi
  mkdir -p "$STATE_ROOT"
  ( flock 9
    # An entry being reconciled remains in project-pending.  A second enqueue
    # for it is deliberately retained, so an event that arrives between the
    # read and the ack cannot be mistaken for the entry we are acknowledging.
    if grep -Fxq -- "$issue" "$STATE_ROOT/project-pending.inflight" 2>/dev/null || ! grep -Fxq -- "$issue" "$STATE_ROOT/project-pending" 2>/dev/null; then
      printf '%s\n' "$issue" >> "$STATE_ROOT/project-pending"
    fi
  ) 9> "$STATE_ROOT/project-pending.lock"
}


PROJECT_METADATA_LOADED=0

PROJECT_ID=''

declare -A PROJECT_FIELD_IDS=() PROJECT_OPTION_IDS=() PROJECT_ITEM_IDS=() PROJECT_ITEM_VALUES=()


invalidate_graphql_budget() { rm -f "$(graphql_budget_file)"; }


load_project_context() {
  (( PROJECT_METADATA_LOADED == 0 )) || return 0
  [[ -r $STATE_ROOT/project.env ]] || return 1
  local PROJECT_OWNER='' PROJECT_NUMBER='' key value row name id option option_id
  while IFS='=' read -r key value; do
    case $key in PROJECT_OWNER) PROJECT_OWNER=$value ;; PROJECT_NUMBER) PROJECT_NUMBER=$value ;; esac
  done < "$STATE_ROOT/project.env"
  [[ -n $PROJECT_OWNER && $PROJECT_NUMBER =~ ^[0-9]+$ ]] || return 1
  PROJECT_ID=$(gh project view "$PROJECT_NUMBER" --owner "$PROJECT_OWNER" --format json --jq .id 2>/dev/null) || return 1
  while IFS=$'\t' read -r name id option option_id; do
    [[ -n $name && -n $id ]] || continue
    PROJECT_FIELD_IDS[$name]=$id
    [[ -n $option && -n $option_id ]] && PROJECT_OPTION_IDS["$name:$option"]=$option_id
  done < <(gh project field-list "$PROJECT_NUMBER" --owner "$PROJECT_OWNER" --format json --jq '.fields[] | . as $field | if (.options // [] | length) == 0 then [.name, .id, "", ""] | @tsv else .options[] | [$field.name, $field.id, .name, .id] | @tsv end' 2>/dev/null || true)
  [[ -n $PROJECT_ID ]] || return 1
  PROJECT_METADATA_LOADED=1
}


load_project_content() {
  local url=$1 kind number query row item_id agent_status category blocked_by cursor=''
  load_project_context || return 1
  if [[ $url =~ /issues/([0-9]+)$ ]]; then
    kind=issue; number=${BASH_REMATCH[1]}
  elif [[ $url =~ /pull/([0-9]+)$ ]]; then
    kind=pullRequest; number=${BASH_REMATCH[1]}
  else
    return 1
  fi
  # content.projectItems is paginated.  A successful empty result means "not
  # a member" (return 2); malformed/GraphQL-error responses are failures.
  while :; do
    query='query($owner:String!,$repo:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$repo){content:'"$kind"'(number:$number){projectItems(first:20,after:$cursor,includeArchived:false){nodes{id project{id} fieldValues(first:20){nodes{... on ProjectV2ItemFieldSingleSelectValue{name field{... on ProjectV2SingleSelectField{name}}} ... on ProjectV2ItemFieldTextValue{text field{... on ProjectV2Field{name}}}}} pageInfo{hasNextPage endCursor}}}}}}'
    row=$(gh api graphql -f query="$query" -F owner="$(repo_name | cut -d/ -f1)" -F repo="$(repo_name | cut -d/ -f2)" -F number="$number" -f cursor="$cursor" --jq 'if (.errors or .data.repository == null or .data.repository.content == null) then error("Project content query failed") else .data.repository.content.projectItems as $items | ($items.nodes[] | select(.project.id == "'"$PROJECT_ID"'") | [.id, ([.fieldValues.nodes[] | select(.field.name == "Agent status") | .name][0] // ""), ([.fieldValues.nodes[] | select(.field.name == "Category") | .name][0] // ""), ([.fieldValues.nodes[] | select(.field.name == "Blocked by") | .text][0] // "")] | join("\u001f")), (if $items.pageInfo.hasNextPage then "NEXT\u001f" + ($items.pageInfo.endCursor // "") else "END" end)' 2>/dev/null) || return 1
    item_id=$(head -n 1 <<< "$row")
    if [[ $item_id != END && $item_id != NEXT$'\x1f'* ]]; then row=$item_id; break; fi
    cursor=$(tail -n 1 <<< "$row"); [[ $cursor == NEXT$'\x1f'* ]] || return 2
    cursor=${cursor#*$'\x1f'}; [[ -n $cursor ]] || return 1
  done
  invalidate_graphql_budget
  IFS=$'\x1f' read -r item_id agent_status category blocked_by <<< "$row"
  PROJECT_ITEM_IDS[$url]=$item_id
  PROJECT_ITEM_VALUES["$url:Agent status"]=$agent_status
  PROJECT_ITEM_VALUES["$url:Category"]=$category
  PROJECT_ITEM_VALUES["$url:Blocked by"]=$blocked_by
}


project_add_content() {
  local url=$1
  graphql_budget_allows "$((GRAPHQL_RESERVE + PROJECT_OPERATION_BUDGET))" || { queue_project_sync "content $url"; return 0; }
  [[ -r $STATE_ROOT/project.env ]] || return 0
  local PROJECT_OWNER='' PROJECT_NUMBER='' key value added_id
  while IFS='=' read -r key value; do
    case $key in PROJECT_OWNER) PROJECT_OWNER=$value ;; PROJECT_NUMBER) PROJECT_NUMBER=$value ;; esac
  done < "$STATE_ROOT/project.env"
  [[ -n $PROJECT_OWNER && $PROJECT_NUMBER =~ ^[0-9]+$ ]] || return 0
  local content_rc=0
  load_project_content "$url" || content_rc=$?
  # rc 1 = retryable failure (queue a hint); rc 2 = a confirmed non-member,
  # which is exactly the state item-add exists to fix.  Only rc 1 suppresses
  # the membership mutation.
  if (( content_rc == 1 )); then queue_project_sync "content $url"; return 0; fi
  [[ -n ${PROJECT_ITEM_IDS[$url]:-} ]] && return 0
  if ! added_id=$(gh project item-add "$PROJECT_NUMBER" --owner "$PROJECT_OWNER" --url "$url" --format json --jq .id 2>/dev/null); then
    invalidate_graphql_budget
    queue_project_sync "content $url"
    return 0
  fi
  invalidate_graphql_budget
  PROJECT_ITEM_IDS[$url]=$added_id
  PROJECT_ITEM_VALUES["$url:Agent status"]=''
  PROJECT_ITEM_VALUES["$url:Category"]=''
  PROJECT_ITEM_VALUES["$url:Blocked by"]=''
}


project_option_for_state() { case $1 in queued) printf Queued;; running) printf Running;; needs-input) printf 'Needs input';; in-review) printf 'In review';; completed) printf Done;; failed) printf Failed;; parked) printf Parked;; stale) printf Stale;; blocked) printf Blocked;; paused) printf Paused;; stopping) printf Stopping;; cancelled) printf Cancelled;; superseded) printf Superseded;; duplicate) printf Duplicate;; merged) printf Merged;; *) printf Inbox;; esac; }

project_option_for_category() { case $1 in loop-continuity) printf 'Loop continuity';; confidentiality-incident) printf 'Confidentiality incident';; integrity-incident) printf 'Integrity incident';; availability-incident) printf 'Availability incident';; feature) printf Feature;; improvement) printf Improvement;; *) return 1;; esac; }


project_issue_snapshot() {
  local issue=$1 row
  row=$(repo_api "issues/$issue" --jq '[.state, .updated_at, ([.labels[].name] | join(",")), (.body // "")] | join("\u001f")') || return 1
  IFS=$'\x1f' read -r PROJECT_ISSUE_STATE PROJECT_ISSUE_UPDATED PROJECT_ISSUE_LABELS PROJECT_ISSUE_BODY <<< "$row"
  [[ -n $PROJECT_ISSUE_STATE && -n $PROJECT_ISSUE_UPDATED ]] || return 1
  local -a states=() categories=(); local l
  IFS=, read -ra _project_labels <<< "$PROJECT_ISSUE_LABELS"
  for l in "${_project_labels[@]}"; do [[ $l == agent:* ]] && states+=("${l#agent:}"); [[ $l == category:* ]] && categories+=("${l#category:}"); done
  (( ${#states[@]} == 1 && ${#categories[@]} == 1 )) || return 2
  PROJECT_DESIRED_STATE=$(project_option_for_state "${states[0]}")
  PROJECT_DESIRED_CATEGORY=$(project_option_for_category "${categories[0]}") || return 2
}


project_desired_blocked_by() {
  local issue=$1 refs body_refs native_refs other reason
  if control_pause_record_read "$issue"; then
    printf '一時停止: @%s %s（再開: bin/agentic-loop resume %s）' "$CONTROL_PAUSE_ACTOR" "$(date -u -d "@$CONTROL_PAUSE_AT" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || printf '%s' "$CONTROL_PAUSE_AT")" "$issue"
    return 0
  fi
  if [[ -r $(conflict_wait_file "$issue") ]]; then IFS=$'\t' read -r other reason < "$(conflict_wait_file "$issue")" || return 1; printf '#%s scope重複: %s' "$other" "$reason"; return 0; fi
  body_refs=$(dependency_refs_from_body "$PROJECT_ISSUE_BODY") || return 1
  local native_rc=0
  native_refs=$(dependency_native_refs "$issue" 2>/dev/null) || native_rc=$?
  (( native_rc == 0 || native_rc == 2 )) || return 1
  refs=$(printf '%s\n%s\n' "$body_refs" "$native_refs" | sed '/^$/d' | sort -un | paste -sd, -)
  [[ -z $refs ]] || printf '依存: #%s' "${refs//,/, #}"
}


project_reconcile_issue() {
  local issue=$1 url project_id item_id field_id option_id desired note before_updated
  graphql_budget_allows "$((GRAPHQL_RESERVE + PROJECT_OPERATION_BUDGET))" || return 1
  [[ -r $STATE_ROOT/project.env ]] || return 0
  project_issue_snapshot "$issue" || return $?
  before_updated=$PROJECT_ISSUE_UPDATED; url=$(repo_issue_url "$issue")
  # Never add membership here: #114/Auto-add owns it.
  local content_rc=0
  load_project_content "$url" || content_rc=$?
  (( content_rc == 0 )) || return $content_rc
  note=$(project_desired_blocked_by "$issue") || return 1
  project_id=$PROJECT_ID; item_id=${PROJECT_ITEM_IDS[$url]:-}
  for field_id in 'Agent status' Category 'Blocked by'; do
    case $field_id in 'Agent status') desired=$PROJECT_DESIRED_STATE;; Category) desired=$PROJECT_DESIRED_CATEGORY;; *) desired=$note;; esac
    [[ ${PROJECT_ITEM_VALUES["$url:$field_id"]:-} == "$desired" ]] && continue
    if [[ $field_id == 'Blocked by' ]]; then
      gh project item-edit --id "$item_id" --project-id "$project_id" --field-id "${PROJECT_FIELD_IDS[$field_id]:-}" --text "$desired" >/dev/null 2>&1 || return 1
    else
      option_id=${PROJECT_OPTION_IDS["$field_id:$desired"]:-}; [[ -n $option_id ]] || return 1
      gh project item-edit --id "$item_id" --project-id "$project_id" --field-id "${PROJECT_FIELD_IDS[$field_id]:-}" --single-select-option-id "$option_id" >/dev/null 2>&1 || return 1
    fi
    invalidate_graphql_budget
    unset 'PROJECT_ITEM_IDS[$url]' 'PROJECT_ITEM_VALUES[$url:Agent status]' 'PROJECT_ITEM_VALUES[$url:Category]' 'PROJECT_ITEM_VALUES[$url:Blocked by]'
    load_project_content "$url" || return 1
    [[ ${PROJECT_ITEM_VALUES["$url:$field_id"]:-} == "$desired" ]] || return 1
  done
  project_issue_snapshot "$issue" || return $?
  [[ $PROJECT_ISSUE_UPDATED == "$before_updated" ]] || return 1
  return 0
}


project_add_issue() {
  project_add_content "$(repo_issue_url "$1")"
}


project_add_pull_requests() {
  graphql_budget_allows "$((GRAPHQL_RESERVE + 1))" || { queue_project_sync "pulls $1"; return 0; }
  [[ -r $STATE_ROOT/project.env ]] || return 0
  local branch=$1 url PROJECT_OWNER='' PROJECT_NUMBER='' key value
  while IFS='=' read -r key value; do
    case $key in PROJECT_OWNER) PROJECT_OWNER=$value ;; PROJECT_NUMBER) PROJECT_NUMBER=$value ;; esac
  done < "$STATE_ROOT/project.env"
  [[ -n $PROJECT_OWNER && $PROJECT_NUMBER =~ ^[0-9]+$ ]] || return 0
  while IFS= read -r url; do
    [[ -n $url ]] || continue
    project_add_content "$url"
  done < <(repo_api pulls --method GET -f state=all -f head="${PROJECT_OWNER}:$branch" -f per_page=100 --paginate --jq '.[].html_url' 2>/dev/null || true)
}


reconcile_pending_project() {
  [[ -s $STATE_ROOT/project-pending ]] || return 0
  graphql_budget_allows "$((GRAPHQL_RESERVE + PROJECT_OPERATION_BUDGET))" || return 0
  local issue processed=0 rc snapshot="$STATE_ROOT/project-pending.snapshot.$$" inflight="$STATE_ROOT/project-pending.inflight"
  ( flock 9; cp "$STATE_ROOT/project-pending" "$snapshot"; : > "$inflight" ) 9> "$STATE_ROOT/project-pending.lock"
  while read -r issue _; do
    [[ $issue =~ ^[1-9][0-9]*$ ]] || continue
    if (( processed >= PROJECT_RECONCILE_BATCH )); then continue; fi
    ( flock 9; printf '%s\n' "$issue" >> "$inflight" ) 9> "$STATE_ROOT/project-pending.lock"
    rc=0
    project_reconcile_issue "$issue" || rc=$?
    # rc 0 (converged) and rc 2 (confirmed non-member / invalid labels, no
    # mutation to make) are both acknowledgement-worthy; only rc 1 is a
    # retryable failure that must remain so a crash cannot drop the entry.
    if (( rc == 0 || rc == 2 )); then
      # Remove only the snapshot occurrence.  Any duplicate appended while
      # this Issue was in-flight remains as the next reconciliation hint.
      ( flock 9
        awk -v n="$issue" 'BEGIN { removed=0 } $0 == n && !removed { removed=1; next } { print }' "$STATE_ROOT/project-pending" > "$STATE_ROOT/project-pending.ack.$$" && mv "$STATE_ROOT/project-pending.ack.$$" "$STATE_ROOT/project-pending"
        sed -i "0,/^$issue$/d" "$inflight"
      ) 9> "$STATE_ROOT/project-pending.lock"
    fi
    processed=$((processed + 1))
  done < "$snapshot"
  rm -f "$snapshot"
  [[ -s $STATE_ROOT/project-pending ]] || rm -f "$STATE_ROOT/project-pending"
}


# All field call-sites use one reconciliation path.  The former arguments are
# retained only for call-site compatibility; Label state is read afresh.  The
# explicit legacy membership conduit remains separate from the field
# reconciler until #114's Auto-add owns membership everywhere.
project_sync_state() { queue_project_sync "issue $1"; project_add_issue "$1" || true; project_reconcile_issue "$1" || true; }

project_sync_category() { queue_project_sync "issue $1"; project_add_issue "$1" || true; project_reconcile_issue "$1" || true; }

project_sync_conflict() { queue_project_sync "issue $1"; project_add_issue "$1" || true; project_reconcile_issue "$1" || true; }


PROJECT_HINTS_REBUILT=0

rebuild_project_hints() {
  local issue source=${1:-all}
  # GitHub is the source of truth; this reconstructs hints after loss of local
  # state.  It deliberately includes closed Issues, whose terminal labels are
  # also projected when they are already Project members.
  if [[ $source == open ]]; then
    [[ -n $SUPERVISOR_SNAPSHOT && -r $SUPERVISOR_SNAPSHOT ]] || return 1
    while IFS=$'\t' read -r issue _; do [[ $issue =~ ^[1-9][0-9]*$ ]] && queue_project_sync "issue $issue"; done < "$SUPERVISOR_SNAPSHOT"
    return 0
  fi
  local rows
  rows=$(repo_api issues --method GET -f state=all -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | .number') || return 1
  while read -r issue; do [[ $issue =~ ^[1-9][0-9]*$ ]] && queue_project_sync "issue $issue"; done <<< "$rows"
  PROJECT_HINTS_REBUILT=1
}


# One open-Issue snapshot backs all label-state maintenance in a supervisor
# poll. Without it, an idle poll performs separate list requests for running,
# queued, needs-input, failed and blocked Issues. Rows are:
# number<TAB>agent-state<TAB>updated_at<TAB>created_at<TAB>body(base64)<TAB>categories<TAB>category_rank<TAB>priority_value.
# agent:parked deliberately falls into "other" here (no dedicated branch): it
# must never be claimed, retried, or recovered by any automatic path (see
# docs/decisions/0016), and "other" is consulted by none of those paths.
# agent:paused gets its own "paused" branch instead, but it is consulted by
# exactly one caller (drain_paused_workers, see docs/decisions/0019): every
# other automatic path (claim_next, retry_failed, recover_expired,
# triage_stale_queued, reconcile_queued_categories, requeue_answered,
# requeue_dependency_ready) reads a different bucket, so a paused Issue is
# structurally excluded from all of them, the same guarantee agent:parked has.
SUPERVISOR_SNAPSHOT=''

refresh_supervisor_snapshot() {
  local target="$STATE_ROOT/open-issues.$$"
  if repo_api issues --method GET -f state=open -f per_page=100 --paginate --jq '
    .[] | select(.pull_request == null) |
    [.number,
     (if any(.labels[]; .name == "agent:running") then "running"
      elif any(.labels[]; .name == "agent:queued") then "queued"
      elif any(.labels[]; .name == "agent:needs-input") then "needs-input"
      elif any(.labels[]; .name == "agent:failed") then "failed"
      elif any(.labels[]; .name == "agent:blocked") then "blocked"
      elif any(.labels[]; .name == "agent:paused") then "paused"
      else "other" end),
     (.updated_at // "-"), .created_at,
     (if (.body // "") == "" then "-" else ((.body // "") | @base64) end),
     (([.labels[].name | select(startswith("category:"))] | join(",")) as $categories | if $categories == "" then "-" else $categories end),
     (if any(.labels[]; .name == "category:loop-continuity") then 0 elif any(.labels[]; .name == "category:confidentiality-incident") then 1 elif any(.labels[]; .name == "category:integrity-incident") then 2 elif any(.labels[]; .name == "category:availability-incident") then 3 elif any(.labels[]; .name == "category:feature") then 4 else 5 end),
     '"$(queue_priority_jq)"'
    ] | @tsv' > "$target"; then
    SUPERVISOR_SNAPSHOT=$target
    return 0
  fi
  rm -f "$target"
  SUPERVISOR_SNAPSHOT=''
  return 1
}


snapshot_state_rows() {
  local state=$1
  [[ -n $SUPERVISOR_SNAPSHOT && -r $SUPERVISOR_SNAPSHOT ]] || return 1
  awk -F '\t' -v state="$state" '$2 == state' "$SUPERVISOR_SNAPSHOT"
}


snapshot_issue_has_state() {
  local issue=$1 state=$2
  [[ -n $SUPERVISOR_SNAPSHOT && -r $SUPERVISOR_SNAPSHOT ]] || return 2
  awk -F '\t' -v issue="$issue" -v state="$state" '$1 == issue && $2 == state { found=1; exit } END { exit !found }' "$SUPERVISOR_SNAPSHOT"
}


clear_supervisor_snapshot() {
  [[ -z $SUPERVISOR_SNAPSHOT ]] || rm -f "$SUPERVISOR_SNAPSHOT"
  SUPERVISOR_SNAPSHOT=''
}


reconcile_queued_categories() {
  local issue categories category found name payload
  while IFS=$'\t' read -r issue _ _ _ _ categories _; do
    [[ -n $issue ]] || continue
    category=''; found=0
    for name in "${CATEGORY_LABELS[@]}"; do
      if [[ ,$categories, == *,category:$name,* ]]; then
        found=$((found + 1)); [[ -n $category ]] || category=$name
      fi
    done
    (( found == 1 )) && continue
    [[ -n $category ]] || category=improvement
    payload=$(repo_api "issues/$issue" --jq '[.labels[].name | select(startswith("category:") | not)] + ["category:'"$category"'"]') || continue
    printf '%s\n' "$payload" | repo_api "issues/$issue/labels" --method PUT --input - >/dev/null
    project_sync_category "$issue" "$category"
    if (( found == 0 )); then
      comment_issue "$issue" '<!-- agentic-loop:category-reconciled reason=missing -->\nカテゴリが未設定だったため、安全な既定値 `category:improvement` を付与しました。要求の実態が別カテゴリに該当する場合は、queued中にカテゴリLabelを1つだけ残して再トリアージしてください。incidentの詳細や秘密情報はLabel、Issue本文、Projectへ転記しないでください。'
    else
      comment_issue "$issue" "<!-- agentic-loop:category-reconciled reason=multiple selected=$category -->\n複数のカテゴリを検出したため、定義済みの最上位カテゴリ \`category:$category\` だけを残しました。必要ならqueued中にカテゴリLabelを1つだけ残して再トリアージしてください。"
    fi
  done < <(snapshot_state_rows queued || repo_api issues --method GET -f state=open -f labels="$(state_label queued)" -f per_page=100 --paginate --jq '.[] | select(.pull_request == null) | [.number, "queued", "", "", "", ([.labels[].name | select(startswith("category:"))] | join(","))] | @tsv' 2>/dev/null || true)
}


cmd_sync_issue() {
  [[ ${1:-} =~ ^[1-9][0-9]*$ ]] || fail 'sync-issue requires a positive Issue number'
  project_sync_state "$1" queued
  say "Issue #$1 のProject同期を実行しました（障害時は再試行queueへ保存済みです）。"
}
