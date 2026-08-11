#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

required=(AGENTS.md README.md Makefile .editorconfig .gitignore .env.example .codex/config.toml install.sh devbox.json devbox.lock docs/policies/cost.md docs/policies/testing.md docs/policies/development-environment.md docs/decisions/0002-github-issue-queue.md docs/operations/issue-queue.md .agentic-loop/config .agentic-loop/guard-secrets.sh .githooks/pre-commit .githooks/pre-push .agents/skills/submit-requirement/SKILL.md bin/agentic-loop scripts/check-environment.sh scripts/install-target.sh)
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
grep -Fq '[テストポリシー](docs/policies/testing.md)' AGENTS.md || {
  printf 'Missing testing policy invariant.\n' >&2
  exit 1
}
grep -Fq '[開発環境ポリシー](docs/policies/development-environment.md)' AGENTS.md || {
  printf 'Missing development environment invariant.\n' >&2
  exit 1
}
grep -Fq 'devbox run --pure check' docs/policies/development-environment.md || {
  printf 'Invalid development environment policy.\n' >&2
  exit 1
}
grep -Fq 'devbox run --pure check' .github/workflows/ci.yml || {
  printf 'CI does not use the common Devbox entry point.\n' >&2
  exit 1
}
if grep -ERn 'nix develop|nix-shell|flake\.nix|flake\.lock' README.md docs scripts tests .github install.sh --exclude=lint.sh; then
  printf 'Direct Nix development entry point remains.\n' >&2
  exit 1
fi
if grep -Eq 'uses: [^ ]+@v[0-9]' .github/workflows/ci.yml; then
  printf 'CI actions must be pinned to immutable commit SHAs.\n' >&2
  exit 1
fi
grep -Fq '外部影響を伴わない要求は、原則としてエンドツーエンド（E2E）テストでカバーする。' docs/policies/testing.md || {
  printf 'Invalid E2E testing policy.\n' >&2
  exit 1
}
grep -Fq 'すべての自動テストはリポジトリ共通の検証入口から実行でき' docs/policies/testing.md || {
  printf 'Missing CI testing requirement.\n' >&2
  exit 1
}

while IFS= read -r -d '' file; do
  bash -n "$file"
done < <(find bin scripts tests .agentic-loop .githooks -type f \( -name '*.sh' -o -perm -u+x \) -print0)
bash -n bin/agentic-loop
shellcheck bin/agentic-loop tests/test-agentic-loop.sh
grep -Fq 'MAX_WORKERS=2' .agentic-loop/config || { printf 'Unsafe worker default.\n' >&2; exit 1; }
grep -Fq -- '--sandbox workspace-write' bin/agentic-loop || { printf 'Unsafe Codex sandbox.\n' >&2; exit 1; }
if grep -Eq 'danger-full-access|OPENAI_API_KEY' bin/agentic-loop install.sh scripts/install-target.sh; then
  printf 'Forbidden Codex execution or API-key billing configuration.\n' >&2
  exit 1
fi
./.agentic-loop/guard-secrets.sh --all

printf 'Lint passed.\n'
