# Module: smoke.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



# Explicit, host-shell-only check against the real GitHub API and the real
# provider CLI (Issue #279): every layer below this (fake gh, structural unit
# tests) can pass while a schema mistake like #268's misplaced `pageInfo`
# breaks the actual GraphQL boundary, because the fake never validates GitHub's
# schema. This calls the production project.sh functions unchanged so the
# query, the --jq reducer, and the cursor-following loop it runs here are
# exactly what the supervisor runs. Never part of `make check` (network, real
# credentials, and provider quota make it non-deterministic and billable; see
# docs/policies/validation-harness.md). Prints only a success/failure line per
# boundary -- never response bodies, tokens, or secrets.
cmd_smoke() {
  local issue=''
  while [[ $# -gt 0 ]]; do
    case $1 in
      --issue) issue=${2:-}; shift 2 ;;
      *) printf 'smoke: unknown argument %s\n' "$1" >&2; return 2 ;;
    esac
  done
  command -v gh >/dev/null 2>&1 || { printf 'smoke: gh command not found\n' >&2; return 2; }
  [[ -r $STATE_ROOT/project.env ]] || { printf 'smoke: %s/project.env missing (run setup first)\n' "$STATE_ROOT" >&2; return 2; }

  if [[ -z $issue ]]; then
    # workload-boundary: one bounded REST read (per_page=1) to pick a default
    # target when the caller does not name one; never a full Issue listing.
    issue=$(repo_api 'issues?state=open&per_page=1' --jq '.[0].number // ""' 2>/dev/null) || issue=''
  fi
  [[ $issue =~ ^[1-9][0-9]*$ ]] || { printf 'smoke: no open Issue to target; pass --issue N\n' >&2; return 2; }

  load_project_context || { printf 'smoke: github-graphql: failed (project.env / gh project scopes)\n' >&2; return 1; }
  local content_rc=0
  load_project_content "$(repo_issue_url "$issue")" || content_rc=$?
  # rc 1 is the only failure: the query/reducer boundary itself broke. rc 0
  # (member) and rc 2 (confirmed non-member) both prove the boundary works.
  if (( content_rc == 1 )); then
    printf 'smoke: github-graphql: failed (projectItems query/reducer boundary)\n' >&2
    return 1
  fi
  printf 'github-graphql: ok\n'

  repo_api "issues/$issue" --jq '.number' >/dev/null 2>&1 || { printf 'smoke: github-rest: failed\n' >&2; return 1; }
  printf 'github-rest: ok\n'

  local provider; provider=$(agent_default_provider)
  provider_ready "$provider" || { printf 'smoke: provider(%s): not installed\n' "$provider" >&2; return 2; }
  if agent_provider_probe_once "$provider" "$REPO_ROOT"; then
    printf 'provider(%s): ok\n' "$provider"
  else
    printf 'smoke: provider(%s): failed\n' "$provider" >&2
    return 1
  fi
}
