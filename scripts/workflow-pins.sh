#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflows="$root/.github/workflows"
fail() { printf 'workflow-pins: %s\n' "$1" >&2; exit 1; }

[[ -d "$workflows" ]] || fail '.github/workflows is missing'

# actionlint validates workflow YAML and expression syntax; it has no idea
# whether a pinned commit sha exists upstream, and it does not notice when
# two workflows pin the *same* action at the *same* version comment to
# *different* shas (exactly the class of defect the mistyped
# jetify-com/devbox-install-action revision was: a sha that does not exist
# upstream, silently different from the correct one ci.yml already used).
# This script makes that class of defect fail closed: for every
# "uses: owner/repo@<40-hex-sha> # <version>" line across every workflow,
# the mapping from (owner/repo, version) to sha must be unique.
mapping="$(rg -I -No --pcre2 'uses:\s*([\w.-]+/[\w.-]+)@([0-9a-f]{40})\s*#\s*(\S+)' -r '$1 $3 $2' "$workflows"/*.yml | sort -u)"

[[ -n "$mapping" ]] || fail 'no pinned "uses:" lines were found'

conflicts="$(printf '%s\n' "$mapping" | awk '{print $1, $2}' | sort | uniq -d)"
if [[ -n "$conflicts" ]]; then
  fail "action pin disagreement (owner/repo version): $(printf '%s' "$conflicts" | paste -sd';' -)"
fi

printf 'workflow-pins: %d unique (owner/repo, version) -> sha pins, no disagreement\n' "$(printf '%s\n' "$mapping" | wc -l | tr -d ' ')"
