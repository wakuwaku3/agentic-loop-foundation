#!/usr/bin/env bash
set -euo pipefail

# Explicit opt-in: this touches GitHub and the configured provider and may
# consume provider quota. It is never part of the merge gate.
# shellcheck disable=SC1091
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
readonly ROOT
source "$ROOT/bin/lib/agentic-loop/common.sh"
source "$ROOT/bin/lib/agentic-loop/config.sh"
source "$ROOT/bin/lib/agentic-loop/project.sh"
command -v gh >/dev/null || { echo 'smoke: gh が必要です' >&2; exit 1; }
repo=$(repo_name); owner=${repo%%/*}; name=${repo#*/}
issue=${AGENTIC_LOOP_SMOKE_ISSUE:-279}; project_id=${AGENTIC_LOOP_SMOKE_PROJECT_ID:-}
[[ $issue =~ ^[1-9][0-9]*$ && -n $project_id ]] || {
  echo 'smoke: AGENTIC_LOOP_SMOKE_PROJECT_ID と整数のIssue番号が必要です' >&2; exit 2;
}
query=$(project_content_query issue)
result=$(gh api graphql -f query="$query" -F owner="$owner" -F repo="$name" \
  -F number="$issue" -f cursor='' --jq "$(project_content_jq "$project_id")")
[[ -n $result ]] || { echo 'smoke: Project query が空の結果を返しました' >&2; exit 1; }
gh api "repos/$repo" --jq '.full_name' | grep -Fx "$repo" >/dev/null
provider=$(agent_default_provider)
case $provider in
  claude)
    response=$(AGENTIC_LOOP_PROBE=1 timeout 30 claude --print --output-format json --dangerously-skip-permissions 'say OK' 2>/dev/null)
    [[ $(printf '%s' "$response" | yq -r '.is_error') == false ]] ;;
  codex)
    response=$(AGENTIC_LOOP_PROBE=1 timeout 30 codex exec --json 'say OK' 2>/dev/null)
    printf '%s\n' "$response" | yq -e 'select(.type == "turn.completed" or .type == "message")' >/dev/null ;;
  opencode)
    response=$(AGENTIC_LOOP_PROBE=1 timeout 30 opencode run --format json 'say OK' 2>/dev/null)
    printf '%s\n' "$response" | yq -e '.' >/dev/null ;;
  *) echo "smoke: 未対応provider: $provider" >&2; exit 2 ;;
esac
printf 'smoke: GitHub (GraphQL+REST) と provider=%s の実物境界を検証しました\n' "$provider"
