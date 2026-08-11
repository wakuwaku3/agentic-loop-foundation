#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

[[ ${DEV_ENVIRONMENT:-} == agentic-loop-foundation-v1 ]] || {
  printf "Run checks in the pinned environment: nix --extra-experimental-features 'nix-command flakes' develop --ignore-environment --command make check\n" >&2
  exit 1
}

for command_name in bash awk find git grep make sed flock; do
  command_path=$(command -v "$command_name")
  [[ $command_path == /nix/store/* ]] || {
    printf 'Unpinned command in development environment: %s=%s\n' "$command_name" "$command_path" >&2
    exit 1
  }
done

printf 'Development environment is pinned.\n'
