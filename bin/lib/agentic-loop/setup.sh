# Module: setup.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



setup_labels() {
  local name color description current
  current=$(repo_api labels --method GET -f per_page=100 --paginate \
    --jq '.[] | [.name, .color, (.description // "")] | @tsv' 2>/dev/null || true)
  for name in "${LABELS[@]}"; do
    case $name in
      queued) color=1D76DB; description='Ready for Agentic Loop' ;;
      running) color=FBCA04; description='Claimed by an Agentic Loop worker' ;;
      needs-input) color=D93F0B; description='Blocked on a consequential user decision' ;;
      in-review) color=5319E7; description='Pull request under review' ;;
      completed) color=0E8A16; description='Merged and verified' ;;
      failed) color=B60205; description='Worker failed; safe to inspect or retry' ;;
      parked) color=E99695; description='Retry budget exhausted; waiting for human triage' ;;
      stale) color=6E7781; description='Queued work closed after the configured inactivity period' ;;
      blocked) color=C5DEF5; description='Waiting for verified Issue dependencies' ;;
      paused) color=BFD4F2; description='Execution paused by an authorized operator' ;;
      stopping) color=FBCA04; description='Authorized disposal is draining an active worker' ;;
      cancelled) color=6E7781; description='Requirement was withdrawn by an authorized operator' ;;
      superseded) color=5319E7; description='Requirement is continued by a successor Issue' ;;
      duplicate) color=C5DEF5; description='Requirement duplicates another Issue' ;;
      merged) color=0E8A16; description='Requirement is consolidated into another Issue' ;;
    esac
    setup_label "$(state_label "$name")" "$color" "$description" "$current"
  done
  for name in "${CATEGORY_LABELS[@]}"; do
    case $name in
      loop-continuity) color=5319E7; description='Agentic Loop continuity and maintenance' ;;
      confidentiality-incident) color=B60205; description='Confidentiality incident' ;;
      integrity-incident) color=D93F0B; description='Integrity incident' ;;
      availability-incident) color=FBCA04; description='Availability incident' ;;
      feature) color=1D76DB; description='New user-facing value or capability' ;;
      improvement) color=0E8A16; description='Quality, performance, maintenance, documentation, or operations improvement' ;;
    esac
    setup_label "category:$name" "$color" "$description" "$current"
  done
  setup_label "$DIAGNOSIS_LABEL" D4C5F9 'Actionable finding from periodic codebase diagnosis' "$current"
}


setup_label() {
  local name=$1 color=$2 description=$3 current=$4 existing_color existing_description
  IFS=$'\t' read -r existing_color existing_description < <(
    awk -F '\t' -v name="$name" '$1 == name {print toupper($2) "\t" $3; exit}' <<< "$current"
  ) || true
  [[ $existing_color == "$color" && $existing_description == "$description" ]] && return 0
  gh label create "$name" --color "$color" --description "$description" --force >/dev/null
}


# One-time migration of the legacy priority:critical|high|medium|low labels
# (see docs/decisions/0015-numeric-priority-marker.md). For every open Issue
# still carrying one of them: convert the highest label to its numeric value
# (90/75/50/25) and append it as a body marker only when the body has no valid
# marker yet (an existing marker stays authoritative), then drop the labels.
# Finally delete the repository-level priority:* label definitions. Setup no
# longer creates these labels, so this is the only path that removes them.
# Idempotent and bounded: open Issues only (closed Issues keep a now-harmless
# label; the repository-level delete stops new assignments).
migrate_priority_labels() {
  local rows issue body_b64 body legacy value existing new_body labels_json name
  rows=$(repo_api issues --method GET -f state=open -f per_page=100 --paginate --jq '
    .[] | select(.pull_request == null) | select(any(.labels[]; .name | startswith("priority:"))) |
    [.number, ((.body // "") | @base64), ([.labels[].name | select(startswith("priority:"))] | join(","))] | @tsv' 2>/dev/null) || return 0
  [[ -n $rows ]] || return 0
  while IFS=$'\t' read -r issue body_b64 legacy; do
    [[ $issue =~ ^[1-9][0-9]*$ ]] || continue
    body=$(base64 -d <<< "$body_b64" 2>/dev/null || true)
    value=0
    for name in critical high medium low; do
      if [[ ,$legacy, == *",$name,"* ]] && (( ${PRIORITY_LEGACY_VALUES[$name]} > value )); then value=${PRIORITY_LEGACY_VALUES[$name]}; fi
    done
    existing=$(body_priority_value "$body")
    if (( existing == 0 && value > 0 )); then
      new_body=$(printf '%s\n<!-- agentic-loop:priority %s -->' "$body" "$value")
      repo_api "issues/$issue" --method PATCH -f body="$new_body" >/dev/null 2>&1 || continue
    fi
    labels_json=$(repo_api "issues/$issue" --jq '[.labels[].name | select(startswith("priority:") | not)]' 2>/dev/null) || continue
    printf '%s\n' "$labels_json" | repo_api "issues/$issue/labels" --method PUT --input - >/dev/null 2>&1 || continue
  done <<< "$rows"
  for name in critical high medium low; do
    gh label delete "priority:$name" --yes >/dev/null 2>&1 || true
  done
}


setup_project() {
  local repository owner title number project_json project_id linked fields
  repository=$(repo_name)
  owner=${repository%%/*}
  title="Agentic Loop - $repository"
  mkdir -p "$STATE_ROOT"
  number=$(gh project list --owner "$owner" --format json --limit 100 --jq ".projects[] | select(.title == \"$title\") | .number" 2>/dev/null | head -n 1 || true)
  if [[ -z $number ]]; then
    project_json=$(gh project create --owner "$owner" --title "$title" --format json)
    number=$(printf '%s\n' "$project_json" | sed -n 's/.*"number"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -n 1)
  fi
  [[ -n $number ]] || fail 'could not create or identify the repository Project'
  printf 'PROJECT_OWNER=%q\nPROJECT_NUMBER=%q\n' "$owner" "$number" > "$STATE_ROOT/project.env"
  project_id=$(gh project view "$number" --owner "$owner" --format json --jq .id 2>/dev/null || true)
  linked=$(gh api graphql -f query='query($project:ID!){node(id:$project){... on ProjectV2{repositories(first:100){nodes{nameWithOwner}}}}}' -F project="$project_id" --jq '.data.node.repositories.nodes[].nameWithOwner' 2>/dev/null || true)
  grep -Fxq "$repository" <<< "$linked" || gh project link "$number" --owner "$owner" --repo "$repository" >/dev/null 2>&1 || true
  fields=$(gh project field-list "$number" --owner "$owner" --format json --jq '.fields[].name' 2>/dev/null || true)
  if ! grep -Fxq 'Agent status' <<< "$fields"; then
    gh project field-create "$number" --owner "$owner" --name 'Agent status' --data-type SINGLE_SELECT \
      --single-select-options 'Inbox,Queued,Running,Needs input,In review,Stopping,Done,Failed,Parked,Stale,Blocked,Paused,Cancelled,Superseded,Duplicate,Merged' >/dev/null 2>&1 || true
  fi
  if ! grep -Fxq 'Category' <<< "$fields"; then
    gh project field-create "$number" --owner "$owner" --name 'Category' --data-type SINGLE_SELECT \
      --single-select-options 'Loop continuity,Confidentiality incident,Integrity incident,Availability incident,Feature,Improvement' >/dev/null 2>&1 || true
  fi
  if ! grep -Fxq 'Blocked by' <<< "$fields"; then
    gh project field-create "$number" --owner "$owner" --name 'Blocked by' --data-type TEXT >/dev/null 2>&1 || true
  fi
  if ! grep -Fxq 'Integration progress' <<< "$fields"; then
    gh project field-create "$number" --owner "$owner" --name 'Integration progress' --data-type TEXT >/dev/null 2>&1 || true
  fi
  setup_project_migrate_status_options "$number" "$owner"
  setup_project_views "$number" "$owner"
}


# field-create fails silently (via || true above) once "Agent status" already
# exists, so a Project created before the Blocked option was introduced never
# gains it. Append the option in place through GraphQL, preserving every
# existing option's name/color/description exactly as read back, so re-running
# `setup` heals older Projects without losing item placements. Best-effort: a
# schema surprise or missing scope just leaves the prior drift warning in
# `doctor`, since Issue Labels remain the source of truth for queue state.
setup_project_migrate_status_options() {
  local number=$1 owner=$2 project_id field_id options_tsv mutation_options name color description
  project_id=$(gh project view "$number" --owner "$owner" --format json --jq .id 2>/dev/null) || return 0
  field_id=$(gh project field-list "$number" --owner "$owner" --format json --jq '.fields[] | select(.name == "Agent status") | .id' 2>/dev/null | head -n 1) || return 0
  [[ -n $project_id && -n $field_id ]] || return 0
  options_tsv=$(gh api graphql -f query='query($field: ID!) {
    node(id: $field) { ... on ProjectV2SingleSelectField { options { name color description } } }
  }' -F field="$field_id" --jq '.data.node.options[] | [.name, .color, .description] | @tsv' 2>/dev/null) || return 0
  [[ -n $options_tsv ]] || return 0
  local required_statuses=(Stopping Blocked Paused Parked Cancelled Superseded Duplicate Merged) required missing=''
  for required in "${required_statuses[@]}"; do grep -Fq "${required}"$'\t' <<< "$options_tsv" || missing+=" $required"; done
  [[ -z $missing ]] && return 0
  mutation_options=''
  while IFS=$'\t' read -r name color description; do
    [[ -n $name ]] || continue
    mutation_options+="{name: \"$name\", color: $color, description: \"$description\"}, "
  done <<< "$options_tsv"
  for required in "${required_statuses[@]}"; do
    grep -Fq "${required}"$'\t' <<< "$options_tsv" && continue
    case $required in
      Stopping) mutation_options+='{name: "Stopping", color: YELLOW, description: "Authorized disposal is draining an active worker"}, ' ;;
      Blocked) mutation_options+='{name: "Blocked", color: GRAY, description: "Waiting on unresolved Issue dependencies"}, ' ;;
      Paused) mutation_options+='{name: "Paused", color: BLUE, description: "Execution paused by an authorized operator"}, ' ;;
      Parked) mutation_options+='{name: "Parked", color: RED, description: "Retry budget exhausted; waiting for human triage"}, ' ;;
      Cancelled) mutation_options+='{name: "Cancelled", color: GRAY, description: "Requirement was withdrawn"}, ' ;;
      Superseded) mutation_options+='{name: "Superseded", color: PURPLE, description: "Continued by a successor Issue"}, ' ;;
      Duplicate) mutation_options+='{name: "Duplicate", color: BLUE, description: "Duplicates another Issue"}, ' ;;
      Merged) mutation_options+='{name: "Merged", color: GREEN, description: "Consolidated into another Issue"}' ;;
    esac
  done
  mutation_options=${mutation_options%, }
  gh api graphql -f query="mutation(\$field: ID!) {
    updateProjectV2SingleSelectField(input: {fieldId: \$field, options: [$mutation_options]}) { projectV2SingleSelectField { id } }
  }" -F field="$field_id" >/dev/null 2>&1 || true
}


setup_project_view() {
  local project_id=$1 name=$2 filter=$3 views_tsv=${4:-} view_id current_filter
  IFS=$'\t' read -r view_id current_filter < <(
    awk -F '\t' -v name="$name" '$2 == name {print $1 "\t" $3; exit}' <<< "$views_tsv"
  ) || true
  if [[ -z $view_id ]]; then
    local create_mutation='mutation($projectId: ID!, $name: String!) {
      createProjectV2View(input: {projectId: $projectId, name: $name, layout: TABLE_LAYOUT}) {
        projectV2View { id }
      }
    }'
    view_id=$(gh api graphql -f query="$create_mutation" -F projectId="$project_id" -f name="$name" \
      --jq '.data.createProjectV2View.projectV2View.id' 2>/dev/null || true)
    current_filter=''
  fi
  [[ $current_filter == "$filter" ]] && return 0
  if [[ -z $view_id ]]; then
    say "Project viewを作成または特定できませんでした（best-effort）: $name" >&2
    return 0
  fi
  local update_mutation='mutation($viewId: ID!, $filter: String!) {
    updateProjectV2View(input: {viewId: $viewId, filter: $filter}) { projectV2View { id } }
  }'
  gh api graphql -f query="$update_mutation" -F viewId="$view_id" -f filter="$filter" >/dev/null 2>&1 ||
    say "Project viewのfilterを修復できませんでした（best-effort）: $name" >&2
}


setup_project_views() {
  local number=$1 owner=$2 project_id views_tsv
  project_id=$(gh project view "$number" --owner "$owner" --format json --jq .id)
  [[ -n $project_id ]] || fail 'could not identify the repository Project ID'
  views_tsv=$(gh api graphql -f query='query($projectId: ID!) {
    node(id: $projectId) {
      ... on ProjectV2 { views(first: 100) { nodes { id name filter } } }
    }
  }' -F projectId="$project_id" --jq '.data.node.views.nodes[] | [.id, .name, (.filter // "")] | @tsv' 2>/dev/null || true)
  setup_project_view "$project_id" 'Triage' 'is:issue is:open no:category' "$views_tsv"
  setup_project_view "$project_id" 'Queue' 'is:issue is:open label:"agent:queued"' "$views_tsv"
  setup_project_view "$project_id" 'Hierarchy' 'is:issue is:open' "$views_tsv"
  setup_project_view "$project_id" 'Active' 'is:issue is:open label:"agent:running","agent:in-review"' "$views_tsv"
  setup_project_view "$project_id" 'Stopping' 'is:issue is:open label:"agent:stopping"' "$views_tsv"
  setup_project_view "$project_id" 'Paused' 'is:issue is:open label:"agent:paused"' "$views_tsv"
  setup_project_view "$project_id" 'Needs input' 'is:issue is:open label:"agent:needs-input"' "$views_tsv"
  setup_project_view "$project_id" 'Recovery' 'is:issue label:"agent:failed","agent:stale","agent:parked"' "$views_tsv"
  setup_project_view "$project_id" 'Recently completed' 'is:issue label:"agent:completed" updated:@today-30d' "$views_tsv"
  setup_project_view "$project_id" 'Disposed' 'is:issue label:"agent:cancelled","agent:superseded","agent:duplicate","agent:merged" updated:@today-30d' "$views_tsv"
  setup_project_view "$project_id" 'Open PRs' 'is:pr is:open' "$views_tsv"
  setup_project_view "$project_id" 'Closed PRs' 'is:pr is:closed' "$views_tsv"
  setup_project_view "$project_id" 'All open issues' 'is:issue is:open' "$views_tsv"
  setup_project_view "$project_id" 'All closed issues' 'is:issue is:closed' "$views_tsv"
}


cmd_setup() {
  preflight
  setup_labels
  migrate_priority_labels
  if [[ ${AGENTIC_LOOP_INSTALL:-0} == 1 && -r $STATE_ROOT/project.env ]]; then
    : # Reinstall: keep the persisted Project identity; explicit setup repairs drift.
  elif graphql_budget_allows "$((GRAPHQL_RESERVE + 50))"; then
    setup_project
  else
    say 'GraphQL残量がsetup用reserveを下回るためProject構成を延期します。Labelキューは利用できます。' >&2
  fi
  # Queue-content reconciliation belongs to bounded supervisor polls. Running
  # it synchronously here makes install cost proportional to the queue size.
  [[ ${AGENTIC_LOOP_INSTALL:-0} == 1 ]] || reconcile_queued_categories
  say "Agentic Loop GitHub queue is ready for $(repo_name)."
}
