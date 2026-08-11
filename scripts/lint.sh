#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

required=(AGENTS.md README.md Makefile .editorconfig .gitignore .env.example .codex/config.toml install.sh docs/policies/cost.md docs/decisions/0002-github-issue-queue.md docs/operations/issue-queue.md .agentic-loop/config .agentic-loop/guard-secrets.sh .githooks/pre-commit .githooks/pre-push .agents/skills/submit-requirement/SKILL.md bin/agentic-loop scripts/install-target.sh)
for file in "${required[@]}"; do
  [[ -f $file ]] || { printf 'Missing required file: %s\n' "$file" >&2; exit 1; }
done

grep -Fxq 'approval_policy = "never"' .codex/config.toml || { printf 'Invalid Codex approval policy.\n' >&2; exit 1; }
grep -Fxq 'sandbox_mode = "workspace-write"' .codex/config.toml || { printf 'Invalid Codex sandbox mode.\n' >&2; exit 1; }
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
grep -Fq 'MAX_WORKERS=2' .agentic-loop/config || { printf 'Unsafe worker default.\n' >&2; exit 1; }
grep -Fq -- '--sandbox workspace-write' bin/agentic-loop || { printf 'Unsafe Codex sandbox.\n' >&2; exit 1; }
if grep -Eq 'danger-full-access|OPENAI_API_KEY' bin/agentic-loop install.sh scripts/install-target.sh; then
  printf 'Forbidden Codex execution or API-key billing configuration.\n' >&2
  exit 1
fi
./.agentic-loop/guard-secrets.sh --all

printf 'Lint passed.\n'
