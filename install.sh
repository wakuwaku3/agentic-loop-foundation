#!/usr/bin/env bash
set -euo pipefail

readonly REPOSITORY="${AGENTIC_LOOP_REPOSITORY:-wakuwaku3/agentic-loop-foundation}"
readonly REVISION="${AGENTIC_LOOP_REVISION:-main}"
readonly TARGET="${AGENTIC_LOOP_TARGET:-$PWD}"
readonly TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TEMP_DIR"' EXIT

command -v git >/dev/null || { printf 'git is required.\n' >&2; exit 1; }

# Runs install-target.sh, or upgrade-target.sh when the first remaining
# argument is "upgrade" (see docs/operations/upgrade.md). Falls back to
# bootstrapping the pinned Devbox environment when yq is not on host PATH.
run_installer() {
  local source_root=$1 target_script=install-target.sh
  shift
  if [[ ${1:-} == upgrade ]]; then
    shift
    target_script=upgrade-target.sh
  fi
  if command -v yq >/dev/null 2>&1; then
    "$source_root/scripts/$target_script" "$TARGET" "$@"
    return
  fi
  command -v devbox >/dev/null 2>&1 || {
    printf 'devbox is required to bootstrap the pinned installation environment (yq is not available).\n' >&2
    exit 1
  }
  devbox run --config "$source_root" -- "$source_root/scripts/$target_script" "$TARGET" "$@"
}

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

run_installer "$source_dir" "$@"
