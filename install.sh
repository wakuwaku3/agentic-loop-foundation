#!/usr/bin/env bash
set -euo pipefail

readonly REPOSITORY="${AGENTIC_LOOP_REPOSITORY:-wakuwaku3/agentic-loop-foundation}"
readonly REVISION="${AGENTIC_LOOP_REVISION:-main}"
readonly TARGET="${AGENTIC_LOOP_TARGET:-$PWD}"
readonly TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TEMP_DIR"' EXIT

command -v git >/dev/null || { printf 'git is required.\n' >&2; exit 1; }

if [[ -n ${AGENTIC_LOOP_SOURCE:-} ]]; then
  source_dir="$AGENTIC_LOOP_SOURCE"
else
  command -v curl >/dev/null || { printf 'curl is required.\n' >&2; exit 1; }
  command -v tar >/dev/null || { printf 'tar is required.\n' >&2; exit 1; }
  curl -fsSL "https://github.com/$REPOSITORY/archive/$REVISION.tar.gz" |
    tar -xz -C "$TEMP_DIR" --strip-components=1
  source_dir="$TEMP_DIR"
fi

# Record the immutable commit actually applied, not a moving ref, so upgrade
# can compare against exactly what was installed instead of implicitly
# following `main` (see docs/operations/upgrade.md).
if [[ $REVISION =~ ^[0-9a-fA-F]{40}$ ]]; then
  resolved_revision=$REVISION
elif [[ -n ${AGENTIC_LOOP_SOURCE:-} ]]; then
  resolved_revision=$(git -C "$AGENTIC_LOOP_SOURCE" rev-parse HEAD)
else
  resolved_revision=$(git ls-remote "https://github.com/$REPOSITORY" "$REVISION" | cut -f1 | head -n1)
fi
export AGENTIC_LOOP_REPOSITORY="$REPOSITORY" AGENTIC_LOOP_REVISION="$REVISION" AGENTIC_LOOP_RESOLVED_REVISION="$resolved_revision"

if [[ ${1:-} == upgrade ]]; then
  shift
  exec "$source_dir/scripts/upgrade-target.sh" "$TARGET" "$@"
fi
"$source_dir/scripts/install-target.sh" "$TARGET"
