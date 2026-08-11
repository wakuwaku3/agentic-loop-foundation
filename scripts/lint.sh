#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

required=(AGENTS.md README.md Makefile .editorconfig .gitignore .env.example .codex/config.toml install.sh docs/policies/cost.md .agentic-loop/guard-secrets.sh .githooks/pre-commit .githooks/pre-push .agents/skills/submit-requirement/SKILL.md)
for file in "${required[@]}"; do
  [[ -f $file ]] || { printf 'Missing required file: %s\n' "$file" >&2; exit 1; }
done

grep -Fxq 'approval_policy = "never"' .codex/config.toml || { printf 'Invalid Codex approval policy.\n' >&2; exit 1; }
if grep -Eq '^[[:space:]]*sandbox_mode[[:space:]]*=' .codex/config.toml; then
  printf 'Codex sandbox mode must be configured outside the repository.\n' >&2
  exit 1
fi
grep -Fq '[費用ポリシー](docs/policies/cost.md)' AGENTS.md || {
  printf 'Missing cost policy invariant.\n' >&2
  exit 1
}
grep -Fq 'Codexサブスクリプションの既存契約料金だけ' docs/policies/cost.md || {
  printf 'Invalid cost policy.\n' >&2
  exit 1
}

while IFS= read -r -d '' file; do
  bash -n "$file"
done < <(find bin scripts tests .agentic-loop .githooks -type f \( -name '*.sh' -o -perm -u+x \) -print0)
bash -n bin/agentic-loop
./.agentic-loop/guard-secrets.sh --all

printf 'Lint passed.\n'
