#!/usr/bin/env bash
set -euo pipefail

readonly REPOSITORY="${AGENTIC_LOOP_REPOSITORY:-wakuwaku3/agentic-loop-foundation}"
readonly REVISION="${AGENTIC_LOOP_REVISION:-main}"
readonly TARGET="${AGENTIC_LOOP_TARGET:-$PWD}"
readonly TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TEMP_DIR"' EXIT

command -v git >/dev/null || { printf 'git is required.\n' >&2; exit 1; }
if [[ -n ${AGENTIC_LOOP_SOURCE:-} ]]; then
  "${AGENTIC_LOOP_SOURCE}/scripts/install-target.sh" "$TARGET"
else
  command -v curl >/dev/null || { printf 'curl is required.\n' >&2; exit 1; }
  command -v tar >/dev/null || { printf 'tar is required.\n' >&2; exit 1; }
  curl -fsSL "https://github.com/$REPOSITORY/archive/$REVISION.tar.gz" |
    tar -xz -C "$TEMP_DIR" --strip-components=1
  "$TEMP_DIR/scripts/install-target.sh" "$TARGET"
fi
