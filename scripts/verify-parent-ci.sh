#!/usr/bin/env bash
set -euo pipefail

repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
before="${GITHUB_EVENT_BEFORE:-}"
if [[ -z "$before" ]]; then
  before="$(git rev-parse HEAD^)"
fi
if [[ "$before" =~ ^0+$ ]]; then
  before="$(git rev-parse HEAD^)"
fi

# Changes to the CI/environment control plane invalidate every component key.
# The affected planner will execute the full graph, so no parent evidence is
# reused and a parent attestation is unnecessary.
while IFS= read -r path; do
  case "$path" in
    .github/*|ci/*|scripts/*|Makefile|devbox.json|devbox.lock|go.mod|go.sum)
      echo "full graph change; parent evidence will not be reused"
      exit 0
      ;;
  esac
done < <(git diff --name-only "$before" HEAD)

count="$(gh api --method GET "repos/$repo/actions/workflows/ci.yml/runs" \
  -f head_sha="$before" -f status=success --jq '.total_count')"
if [[ ! "$count" =~ ^[0-9]+$ ]] || (( count < 1 )); then
  echo "parent $before has no successful v2 selective CI attestation" >&2
  exit 1
fi
