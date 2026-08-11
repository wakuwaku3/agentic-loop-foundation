#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

new_target="$TEST_ROOT/new-project"
AGENTIC_LOOP_SOURCE="$PROJECT_ROOT" AGENTIC_LOOP_TARGET="$new_target" "$PROJECT_ROOT/install.sh"
[[ -d "$new_target/.git" ]] || fail 'init did not initialize Git'
[[ -f "$new_target/AGENTS.md" ]] || fail 'init did not install AGENTS.md'
[[ $(git -C "$new_target" config --get core.hooksPath) == .githooks ]] || fail 'init did not enable hooks'
make -s -C "$new_target" lint >/dev/null

existing_target="$TEST_ROOT/existing-project"
mkdir -p "$existing_target"
printf 'existing\n' > "$existing_target/README.md"
"$PROJECT_ROOT/bin/agentic-loop" "$existing_target"
[[ $(cat "$existing_target/README.md") == existing ]] || fail 'install changed an existing file'
[[ -f "$existing_target/AGENTS.md" ]] || fail 'install did not add AGENTS.md'
[[ ! -e "$existing_target/Makefile" ]] || fail 'install added project tooling'
[[ -f "$existing_target/.agents/skills/submit-requirement/SKILL.md" ]] || fail 'install did not add the skill'
[[ -x "$existing_target/.githooks/pre-push" ]] || fail 'install did not add secret hooks'

conflict_target="$TEST_ROOT/conflict-project"
mkdir -p "$conflict_target"
printf 'keep\n' > "$conflict_target/AGENTS.md"
if "$PROJECT_ROOT/bin/agentic-loop" "$conflict_target" >/dev/null 2>&1; then
  fail 'install accepted a conflicting file'
fi
[[ $(cat "$conflict_target/AGENTS.md") == keep ]] || fail 'conflict changed an existing file'
[[ ! -e "$conflict_target/Makefile" ]] || fail 'conflict caused a partial install'

secret_target="$TEST_ROOT/secret-project"
mkdir -p "$secret_target"
git -C "$secret_target" init --quiet
cp "$PROJECT_ROOT/.agentic-loop/guard-secrets.sh" "$secret_target/guard-secrets.sh"
printf 'token=ghp_%s%s\n' '123456789012345678' '901234567890123456' > "$secret_target/leak.txt"
git -C "$secret_target" add leak.txt
if (cd "$secret_target" && ./guard-secrets.sh --staged) >/dev/null 2>&1; then
  fail 'secret guard accepted a credential-like value'
fi

printf 'Tests passed.\n'
