#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

new_target="$TEST_ROOT/new-project"
"$PROJECT_ROOT/bin/agentic-loop" init "$new_target"
[[ -d "$new_target/.git" ]] || fail 'init did not initialize Git'
[[ -f "$new_target/AGENTS.md" ]] || fail 'init did not install AGENTS.md'
make -s -C "$new_target" lint >/dev/null

existing_target="$TEST_ROOT/existing-project"
mkdir -p "$existing_target"
printf 'existing\n' > "$existing_target/README.md"
"$PROJECT_ROOT/bin/agentic-loop" install "$existing_target"
[[ $(cat "$existing_target/README.md") == existing ]] || fail 'install changed an existing file'
[[ -f "$existing_target/AGENTS.md" ]] || fail 'install did not add AGENTS.md'
[[ ! -e "$existing_target/Makefile" ]] || fail 'install added project tooling'

conflict_target="$TEST_ROOT/conflict-project"
mkdir -p "$conflict_target"
printf 'keep\n' > "$conflict_target/AGENTS.md"
if "$PROJECT_ROOT/bin/agentic-loop" install "$conflict_target" >/dev/null 2>&1; then
  fail 'install accepted a conflicting file'
fi
[[ $(cat "$conflict_target/AGENTS.md") == keep ]] || fail 'conflict changed an existing file'
[[ ! -e "$conflict_target/Makefile" ]] || fail 'conflict caused a partial install'

printf 'Tests passed.\n'
