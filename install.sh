#!/usr/bin/env bash
set -euo pipefail

readonly REPOSITORY="${AGENTIC_LOOP_REPOSITORY:-wakuwaku3/agentic-loop-foundation}"
readonly REVISION="${AGENTIC_LOOP_REVISION:-main}"
readonly TARGET="${AGENTIC_LOOP_TARGET:-$PWD}"
readonly TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TEMP_DIR"' EXIT

command -v git >/dev/null || { printf 'git is required.\n' >&2; exit 1; }

run_installer() {
  local source_root=$1
  if command -v yq >/dev/null 2>&1; then
    "$source_root/scripts/install-target.sh" "$TARGET"
    return
  fi
  command -v devbox >/dev/null 2>&1 || {
    printf 'devbox is required to bootstrap the pinned installation environment (yq is not available).\n' >&2
    exit 1
  }
  devbox run --config "$source_root" -- "$source_root/scripts/install-target.sh" "$TARGET"
}

if [[ -n ${AGENTIC_LOOP_SOURCE:-} ]]; then
  run_installer "$AGENTIC_LOOP_SOURCE"
else
  command -v curl >/dev/null || { printf 'curl is required.\n' >&2; exit 1; }
  command -v tar >/dev/null || { printf 'tar is required.\n' >&2; exit 1; }
  curl -fsSL "https://github.com/$REPOSITORY/archive/$REVISION.tar.gz" |
    tar -xz -C "$TEMP_DIR" --strip-components=1
  run_installer "$TEMP_DIR"
fi
