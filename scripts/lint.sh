#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

required=(AGENTS.md README.md Makefile .editorconfig .gitignore .env.example .codex/config.toml install.sh .agentic-loop/guard-secrets.sh .githooks/pre-commit .githooks/pre-push .agents/skills/submit-requirement/SKILL.md)
for file in "${required[@]}"; do
  [[ -f $file ]] || { printf 'Missing required file: %s\n' "$file" >&2; exit 1; }
done

grep -Fxq 'approval_policy = "never"' .codex/config.toml || { printf 'Invalid Codex approval policy.\n' >&2; exit 1; }
grep -Fxq 'sandbox_mode = "workspace-write"' .codex/config.toml || { printf 'Invalid Codex sandbox mode.\n' >&2; exit 1; }

while IFS= read -r -d '' file; do
  bash -n "$file"
done < <(find bin scripts tests .agentic-loop .githooks -type f \( -name '*.sh' -o -perm -u+x \) -print0)
bash -n bin/agentic-loop
./.agentic-loop/guard-secrets.sh --all

printf 'Lint passed.\n'
