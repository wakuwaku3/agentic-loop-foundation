#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

required=(AGENTS.md README.md Makefile .editorconfig .gitignore .codex/config.toml .claude/settings.json .claude/hooks/confirm-main-worktree-edit.sh install.sh devbox.json devbox.lock docs/policies/cost.md docs/policies/testing.md docs/policies/external-environment.md docs/policies/development-environment.md docs/policies/ai-tool-neutrality.md docs/policies/github-language.md docs/policies/validation-harness.md docs/policies/continuous-delivery.md docs/decisions/0002-github-issue-queue.md docs/decisions/0003-supervisor-resilience-and-api-budget.md docs/decisions/0004-worker-resume-and-handoff.md docs/decisions/0005-status-observability.md docs/decisions/0006-worker-hang-timeout.md docs/decisions/0007-loop-metrics.md docs/decisions/0009-foundation-upgrade.md docs/operations/issue-queue.md docs/operations/codebase-diagnosis.md docs/operations/loop-metrics.md docs/operations/upgrade.md .agentic-loop.toml .agentic-loop/guard-secrets.sh .agentic-loop/update-main.sh .agentic-loop/diagnose-codebase.sh .githooks/pre-commit .githooks/pre-push .agents/skills/submit-requirement/SKILL.md .agents/skills/diagnose-codebase/SKILL.md .claude/skills/submit-requirement/SKILL.md .claude/skills/diagnose-codebase/SKILL.md bin/agentic-loop bin/agentic-loop-diagnose scripts/check-environment.sh scripts/install-target.sh scripts/lib/foundation-files.sh scripts/upgrade-target.sh scripts/upgrade/migrations/0001-foundation-config-section.sh)
for file in "${required[@]}"; do
  [[ -f $file ]] || { printf 'Missing required file: %s\n' "$file" >&2; exit 1; }
done

yq -p json -o json '.' .claude/settings.json >/dev/null || { printf 'Invalid Claude settings JSON.\n' >&2; exit 1; }
[[ $(yq -p json -r '.hooks.PreToolUse[0].matcher // ""' .claude/settings.json) == 'Edit|Write|NotebookEdit' ]] || { printf 'Claude edit hook matcher is invalid.\n' >&2; exit 1; }
[[ $(yq -p json -r '.hooks.PreToolUse[0].hooks[0].command // ""' .claude/settings.json) == '${CLAUDE_PROJECT_DIR}/.claude/hooks/confirm-main-worktree-edit.sh' ]] || { printf 'Claude edit hook command is invalid.\n' >&2; exit 1; }
grep -Fq 'permissionDecision":"ask"' .claude/hooks/confirm-main-worktree-edit.sh || { printf 'Claude edit hook must request confirmation.\n' >&2; exit 1; }
if grep -Fq 'permissionDecision":"deny"' .claude/hooks/confirm-main-worktree-edit.sh; then
  printf 'Claude edit hook must not deny edits.\n' >&2
  exit 1
fi
grep -Fq '.claude/settings.json' scripts/lib/foundation-files.sh || { printf 'Claude settings are not distributed as a shared file.\n' >&2; exit 1; }
grep -Fq '.claude/hooks/confirm-main-worktree-edit.sh' scripts/lib/foundation-files.sh || { printf 'Claude edit hook is not distributed as a shared file.\n' >&2; exit 1; }
grep -Fq 'confirm-main-worktree-edit.sh' scripts/install-target.sh || { printf 'Claude edit hook is not made executable on install.\n' >&2; exit 1; }

grep -Fxq 'approval_policy = "never"' .codex/config.toml || { printf 'Invalid Codex approval policy.\n' >&2; exit 1; }
if grep -Eq '^[[:space:]]*sandbox_mode[[:space:]]*=' .codex/config.toml; then
  printf 'Codex sandbox mode must be configured outside the repository.\n' >&2
  exit 1
fi
grep -Fq '[費用ポリシー](docs/policies/cost.md)' AGENTS.md || {
  printf 'Missing cost policy invariant.\n' >&2
  exit 1
}
grep -Fq 'サブスクリプションの既存契約料金だけとする' docs/policies/cost.md || {
  printf 'Invalid cost policy.\n' >&2
  exit 1
}
grep -Fq '[AIツール非依存ポリシー](docs/policies/ai-tool-neutrality.md)' AGENTS.md || {
  printf 'Missing AI tool neutrality invariant.\n' >&2
  exit 1
}
for requirement in 'AGENT_PROVIDER' 'プロバイダアダプタ' 'reasoning effort'; do
  grep -Fq "$requirement" docs/policies/ai-tool-neutrality.md || {
    printf 'AI tool neutrality policy lacks requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
grep -Fq '[テストポリシー](docs/policies/testing.md)' AGENTS.md || {
  printf 'Missing testing policy invariant.\n' >&2
  exit 1
}
grep -Fq '[開発環境ポリシー](docs/policies/development-environment.md)' AGENTS.md || {
  printf 'Missing development environment invariant.\n' >&2
  exit 1
}
grep -Fq '[外部環境コード化ポリシー](docs/policies/external-environment.md)' AGENTS.md || {
  printf 'Missing external environment policy invariant.\n' >&2
  exit 1
}
for requirement in 'GitHub Repository' 'GitHub Projects' 'CI workflow' 'デプロイ先' '権限と秘密情報' 'desired state' 'drift検出' '復旧および移行' 'コード化できない設定' '例外と破棄'; do
  grep -Fq "$requirement" docs/policies/external-environment.md || {
    printf 'External environment policy lacks requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
grep -Fq '[外部環境コード化ポリシー](external-environment.md)' docs/policies/development-environment.md || {
  printf 'Development environment policy does not identify its parent policy.\n' >&2
  exit 1
}
grep -Fq '[検証ハーネスポリシー](docs/policies/validation-harness.md)' AGENTS.md || {
  printf 'Missing validation harness invariant.\n' >&2
  exit 1
}
for requirement in 'local fast check' 'local full check' 'public repository' 'private repository' 'push gate' 'merge gate' 'commit SHA' 'hook bypass' 'AI review'; do
  grep -Fq "$requirement" docs/policies/validation-harness.md || {
    printf 'Validation harness policy lacks requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
grep -Fq 'docs/policies/validation-harness.md' scripts/lib/foundation-files.sh || {
  printf 'Validation harness policy is not distributed.\n' >&2
  exit 1
}
grep -Fq 'devbox run --pure check' docs/policies/development-environment.md || {
  printf 'Invalid development environment policy.\n' >&2
  exit 1
}
grep -Fq 'doctor --format json' docs/operations/issue-queue.md || {
  printf 'Doctor machine-readable interface is not documented.\n' >&2
  exit 1
}
grep -Fq 'doctor) cmd_doctor' bin/agentic-loop || {
  printf 'Doctor command is not distributed through the queue CLI.\n' >&2
  exit 1
}
grep -Fq 'status --format json' docs/operations/issue-queue.md || {
  printf 'Status machine-readable interface is not documented.\n' >&2
  exit 1
}
grep -Fq 'status) cmd_status' bin/agentic-loop || {
  printf 'Status command is not distributed through the queue CLI.\n' >&2
  exit 1
}
grep -Fq 'status_snapshot_fetch' bin/agentic-loop || {
  printf 'Status observability snapshot is missing.\n' >&2
  exit 1
}
grep -Fq 'queue_rank_jq' bin/agentic-loop || {
  printf 'Queue candidate ordering does not share claim_next\x27s rank expression.\n' >&2
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
shellcheck bin/agentic-loop bin/agentic-loop-diagnose .agentic-loop/diagnose-codebase.sh tests/test-agentic-loop.sh
grep -Eq '^max_workers[[:space:]]*=[[:space:]]*4$' .agentic-loop.toml || { printf 'Unsafe worker default.\n' >&2; exit 1; }
grep -Fq -- '--sandbox workspace-write' bin/agentic-loop || { printf 'Unsafe Codex sandbox.\n' >&2; exit 1; }
grep -Fq 'AGENT_PROVIDER' bin/agentic-loop || { printf 'AI provider is not selectable.\n' >&2; exit 1; }
grep -Fq -- '--dangerously-skip-permissions' bin/agentic-loop || { printf 'Claude worker isolation is not configured.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:usage' bin/agentic-loop || { printf 'Token usage is not recorded for analysis.\n' >&2; exit 1; }
grep -Fq -- '--sandbox read-only' bin/agentic-loop || { printf 'Plan stage does not run read-only.\n' >&2; exit 1; }
grep -Fq 'agent_phase_effort' bin/agentic-loop || { printf 'Plan and exec reasoning effort is not tiered.\n' >&2; exit 1; }
grep -Fq 'agent_phase_provider' bin/agentic-loop || { printf 'Per-phase provider selection is missing.\n' >&2; exit 1; }
grep -Fq 'budget_allows_claim' bin/agentic-loop || { printf 'Budget guard is missing.\n' >&2; exit 1; }
grep -Fq 'retry_failed' bin/agentic-loop || { printf 'Transient-failure retry is missing.\n' >&2; exit 1; }
grep -Fq 'exhaustion_note_pause' bin/agentic-loop || { printf 'Token-exhaustion pause is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:unresolved' bin/agentic-loop || { printf 'Unresolvable-close disposition is missing.\n' >&2; exit 1; }
grep -Fq 'recover_expired || true' bin/agentic-loop || { printf 'Supervisor poll is not resilient to transient API errors.\n' >&2; exit 1; }
grep -Fq 'supervisor_graceful_shutdown' bin/agentic-loop || { printf 'Graceful shutdown handler is missing.\n' >&2; exit 1; }
grep -Fq 'setsid "$0" _worker' bin/agentic-loop || { printf 'Workers are not started in their own process group.\n' >&2; exit 1; }
grep -Fq 'worker_alive "$issue" && continue' bin/agentic-loop || { printf 'Restart recovery lacks the local worker fast path.\n' >&2; exit 1; }
grep -Fq 'issues/comments/$id" --method PATCH' bin/agentic-loop || { printf 'Lease heartbeat does not update a single comment in place.\n' >&2; exit 1; }
grep -Fq 'core_budget_note_pause' bin/agentic-loop || { printf 'REST(core) budget governor is missing from the claim gate.\n' >&2; exit 1; }
grep -Fq 'next_poll_interval' bin/agentic-loop || { printf 'Adaptive idle poll backoff is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:dependency-blocked' bin/agentic-loop || { printf 'Issue dependency gating is missing.\n' >&2; exit 1; }
grep -Fq 'resume_probe() {' bin/agentic-loop || { printf 'Worker resume phase detection is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:handoff' bin/agentic-loop || { printf 'Worker resume handoff comment is missing.\n' >&2; exit 1; }
grep -Fq 'worker_confirm_running_label' bin/agentic-loop || { printf 'Worker resume ownership re-check is missing.\n' >&2; exit 1; }
grep -Fq '中断からの再開' docs/operations/issue-queue.md || { printf 'Worker resume documentation is missing.\n' >&2; exit 1; }
grep -Fq 'enforce_worker_timeout' bin/agentic-loop || { printf 'Per-worker hang timeout enforcement is missing.\n' >&2; exit 1; }
grep -Eq '^worker_timeout_seconds[[:space:]]*=[[:space:]]*14400$' .agentic-loop.toml || { printf 'Unsafe worker_timeout_seconds default.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:worker-timeout' bin/agentic-loop || { printf 'Worker-timeout audit comment marker is missing.\n' >&2; exit 1; }
grep -Fq 'ハング' docs/operations/issue-queue.md || { printf 'Worker hang timeout documentation is missing.\n' >&2; exit 1; }
grep -Fq 'metrics) cmd_metrics' bin/agentic-loop || { printf 'Metrics command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq '[--days N] [--as-of EPOCH] [--format json]' docs/operations/loop-metrics.md || { printf 'Metrics machine-readable interface is not documented.\n' >&2; exit 1; }
grep -Fq 'metrics_close_attempt' bin/agentic-loop || { printf 'Metrics attempt-lifecycle aggregation is missing.\n' >&2; exit 1; }
grep -Fq 'worker単位の内訳・ランキングは出力しない' docs/operations/loop-metrics.md || { printf 'Metrics privacy guarantee is not documented.\n' >&2; exit 1; }
grep -Fq '追加費用ゼロ' docs/decisions/0007-loop-metrics.md || { printf 'Metrics cost-neutrality is not documented.\n' >&2; exit 1; }
grep -Fq 'docs/operations/loop-metrics.md' scripts/lib/foundation-files.sh || { printf 'Metrics documentation is not distributed.\n' >&2; exit 1; }
if grep -Eq 'danger-full-access|OPENAI_API_KEY' bin/agentic-loop bin/agentic-loop-diagnose .agentic-loop/diagnose-codebase.sh install.sh scripts/install-target.sh scripts/upgrade-target.sh scripts/lib/foundation-files.sh scripts/upgrade/migrations/0001-foundation-config-section.sh; then
  printf 'Forbidden Codex execution or API-key billing configuration.\n' >&2
  exit 1
fi

grep -Fq 'upgrade) cmd_upgrade' bin/agentic-loop || { printf 'Upgrade command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'docs/operations/upgrade.md' scripts/lib/foundation-files.sh || { printf 'Upgrade documentation is not distributed.\n' >&2; exit 1; }
grep -Fq 'docs/decisions/0009-foundation-upgrade.md' scripts/lib/foundation-files.sh || { printf 'Upgrade ADR is not distributed.\n' >&2; exit 1; }
grep -Fq 'upgrade --format json' docs/operations/upgrade.md || { printf 'Upgrade machine-readable interface is not documented.\n' >&2; exit 1; }
grep -Fq '無断上書き' docs/operations/upgrade.md || { printf 'Upgrade documentation lacks the no-silent-overwrite invariant.\n' >&2; exit 1; }
grep -Fq '暗黙' docs/operations/upgrade.md || { printf 'Upgrade documentation lacks the no-implicit-main-tracking invariant.\n' >&2; exit 1; }
grep -Fq '追加費用ゼロ' docs/decisions/0009-foundation-upgrade.md || { printf 'Upgrade cost-neutrality is not documented.\n' >&2; exit 1; }
grep -Fq 'foundation_manifest_write' scripts/lib/foundation-files.sh || { printf 'Manifest generation is missing from the shared distribution library.\n' >&2; exit 1; }

for doc in README.md docs/operations/issue-queue.md docs/operations/codebase-diagnosis.md; do
  grep -Fq 'opencode' "$doc" || {
    printf 'Provider-neutrality drift: %s does not mention opencode.\n' "$doc" >&2
    exit 1
  }
done
codex_only_patterns=(
  'Codex CLI、GitHub'
  '選択中のAI CLI（Codex/Claude）'
  'origin/default branch、Codex CLI、Devbox'
  'だけをCodex CLIの'
  'CodexサブスクリプションのCodex CLI'
)
for pattern in "${codex_only_patterns[@]}"; do
  if grep -Fq "$pattern" README.md docs/operations/issue-queue.md docs/operations/codebase-diagnosis.md; then
    printf 'Provider-neutrality drift: found Codex-only phrasing "%s".\n' "$pattern" >&2
    exit 1
  fi
done
./.agentic-loop/guard-secrets.sh --all

printf 'Lint passed.\n'
