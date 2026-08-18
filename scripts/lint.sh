#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

required=(AGENTS.md README.md Makefile .editorconfig .gitignore .codex/config.toml .claude/settings.json .claude/hooks/confirm-main-worktree-edit.sh install.sh devbox.json devbox.lock docs/policies/cost.md docs/policies/testing.md docs/policies/external-environment.md docs/policies/development-environment.md docs/policies/ai-tool-neutrality.md docs/policies/github-language.md docs/policies/validation-harness.md docs/policies/continuous-delivery.md docs/decisions/0002-github-issue-queue.md docs/decisions/0003-supervisor-resilience-and-api-budget.md docs/decisions/0004-worker-resume-and-handoff.md docs/decisions/0005-status-observability.md docs/decisions/0006-worker-hang-timeout.md docs/decisions/0007-loop-metrics.md docs/decisions/0009-foundation-upgrade.md docs/decisions/0010-authorized-issue-disposition.md docs/decisions/0012-provider-pool-fallback.md docs/decisions/0013-agentic-loop-modules.md docs/decisions/0014-scope-structural-conflict.md docs/decisions/0015-numeric-priority-marker.md docs/decisions/0016-failure-park-not-close.md docs/operations/issue-queue.md docs/operations/codebase-diagnosis.md docs/operations/loop-metrics.md docs/operations/upgrade.md .agentic-loop.toml .agentic-loop/guard-secrets.sh .agentic-loop/update-main.sh .agentic-loop/diagnose-codebase.sh .githooks/pre-commit .githooks/pre-push .agents/skills/submit-requirement/SKILL.md .agents/skills/diagnose-codebase/SKILL.md .claude/skills/submit-requirement/SKILL.md .claude/skills/diagnose-codebase/SKILL.md bin/agentic-loop bin/agentic-loop-diagnose bin/lib/agentic-loop/common.sh bin/lib/agentic-loop/config.sh bin/lib/agentic-loop/api.sh bin/lib/agentic-loop/agent.sh bin/lib/agentic-loop/setup.sh bin/lib/agentic-loop/service.sh bin/lib/agentic-loop/status.sh bin/lib/agentic-loop/doctor.sh bin/lib/agentic-loop/upgrade.sh bin/lib/agentic-loop/metrics.sh bin/lib/agentic-loop/project.sh bin/lib/agentic-loop/dispose.sh bin/lib/agentic-loop/worker_state.sh bin/lib/agentic-loop/dependency.sh bin/lib/agentic-loop/scope.sh bin/lib/agentic-loop/priority.sh bin/lib/agentic-loop/supervisor.sh bin/lib/agentic-loop/worker.sh bin/lib/agentic-loop/trace.sh scripts/check-environment.sh scripts/install-target.sh scripts/lib/foundation-files.sh scripts/upgrade-target.sh scripts/upgrade/migrations/0001-foundation-config-section.sh scripts/upgrade/migrations/0002-traceability-config.sh
  docs/decisions/0017-requirement-traceability.md docs/operations/traceability.md .github/PULL_REQUEST_TEMPLATE.md)
for file in "${required[@]}"; do
  [[ -f $file ]] || { printf 'Missing required file: %s\n' "$file" >&2; exit 1; }
done

# The queue CLI is the thin entry script plus its implementation modules (see
# ADR 0013). Invariants that used to live in one file must now hold somewhere
# in this set, so every symbol check below scans the whole set.
readonly AGENTIC_LOOP_SOURCES=(bin/agentic-loop bin/lib/agentic-loop/*.sh)
for module in bin/lib/agentic-loop/*.sh; do
  grep -Fq "source \"\$SCRIPT_ROOT/bin/lib/agentic-loop/$(basename "$module")\"" bin/agentic-loop || {
    printf 'Module %s is not sourced by the queue CLI entry.\n' "$module" >&2
    exit 1
  }
  grep -Fq "$module" scripts/lib/foundation-files.sh || {
    printf 'Module %s is not distributed (missing from SHARED_FILES).\n' "$module" >&2
    exit 1
  }
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
grep -Fq 'doctor) cmd_doctor' "${AGENTIC_LOOP_SOURCES[@]}" || {
  printf 'Doctor command is not distributed through the queue CLI.\n' >&2
  exit 1
}
grep -Fq 'status --format json' docs/operations/issue-queue.md || {
  printf 'Status machine-readable interface is not documented.\n' >&2
  exit 1
}
grep -Fq 'status) cmd_status' "${AGENTIC_LOOP_SOURCES[@]}" || {
  printf 'Status command is not distributed through the queue CLI.\n' >&2
  exit 1
}
grep -Fq 'status_snapshot_fetch' "${AGENTIC_LOOP_SOURCES[@]}" || {
  printf 'Status observability snapshot is missing.\n' >&2
  exit 1
}
grep -Fq 'queue_rank_jq' "${AGENTIC_LOOP_SOURCES[@]}" || {
  printf 'Queue candidate ordering does not share claim_next\x27s rank expression.\n' >&2
  exit 1
}
grep -Fq 'queue_priority_jq' "${AGENTIC_LOOP_SOURCES[@]}" || {
  printf 'Numeric priority is not extracted by a single shared expression.\n' >&2
  exit 1
}
grep -Fq 'priority) cmd_priority' "${AGENTIC_LOOP_SOURCES[@]}" || {
  printf 'The priority command is not distributed through the queue CLI.\n' >&2
  exit 1
}
grep -Fq 'migrate_priority_labels' "${AGENTIC_LOOP_SOURCES[@]}" || {
  printf 'Legacy priority label migration is missing.\n' >&2
  exit 1
}
grep -Fq 'docs/decisions/0015-numeric-priority-marker.md' scripts/lib/foundation-files.sh || {
  printf 'Numeric-priority ADR is not distributed.\n' >&2
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
shellcheck bin/agentic-loop bin/agentic-loop-diagnose .agentic-loop/diagnose-codebase.sh tests/test-agentic-loop.sh "${AGENTIC_LOOP_SOURCES[@]}"
grep -Eq '^max_workers[[:space:]]*=[[:space:]]*4$' .agentic-loop.toml || { printf 'Unsafe worker default.\n' >&2; exit 1; }
grep -Fq -- '--sandbox workspace-write' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Unsafe Codex sandbox.\n' >&2; exit 1; }
grep -Fq 'AGENT_PROVIDER' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'AI provider is not selectable.\n' >&2; exit 1; }
grep -Fq -- '--dangerously-skip-permissions' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Claude worker isolation is not configured.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:usage' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Token usage is not recorded for analysis.\n' >&2; exit 1; }
cmp -s .agents/skills/submit-requirement/SKILL.md .claude/skills/submit-requirement/SKILL.md || { printf 'Codex and Claude submit-requirement skills diverged.\n' >&2; exit 1; }
grep -Fq 'exec終了プロトコルと外部待機' docs/operations/issue-queue.md || { printf 'Exec completion protocol is not documented.\n' >&2; exit 1; }
grep -Fq -- '--sandbox read-only' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Plan stage does not run read-only.\n' >&2; exit 1; }
grep -Fq 'agent_phase_effort' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Plan and exec reasoning effort is not tiered.\n' >&2; exit 1; }
grep -Fq 'agent_phase_provider' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Per-phase provider selection is missing.\n' >&2; exit 1; }
grep -Fq 'agent_pick_tier' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Pool/tier priority selection is missing.\n' >&2; exit 1; }
grep -Fq 'agent_mark_pool_exhausted' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Per-pool exhaustion marking is missing.\n' >&2; exit 1; }
grep -Fq 'agent_result_is_model_failure' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Model-specific failure classification is missing.\n' >&2; exit 1; }
grep -Fq 'opencode.ai/zen/go/v1/usage' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'OpenCode Go usage API measurement is missing.\n' >&2; exit 1; }
grep -Fq '_pick-tier' "${AGENTIC_LOOP_SOURCES[@]}" .agentic-loop/diagnose-codebase.sh || { printf 'Shared tier picker is not wired into diagnosis.\n' >&2; exit 1; }
grep -Fq 'docs/decisions/0012-provider-pool-fallback.md' scripts/lib/foundation-files.sh || { printf 'Pool-fallback ADR is not distributed.\n' >&2; exit 1; }
grep -Fq 'budget_allows_claim' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Budget guard is missing.\n' >&2; exit 1; }
grep -Fq 'retry_failed' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Transient-failure retry is missing.\n' >&2; exit 1; }
grep -Fq 'exhaustion_note_pause' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Token-exhaustion pause is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:parked' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Retry-exhausted park disposition is missing.\n' >&2; exit 1; }
grep -Fq 'park_issue' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'park_issue is missing.\n' >&2; exit 1; }
if grep -Fq 'agentic-loop:unresolved' "${AGENTIC_LOOP_SOURCES[@]}"; then
  printf 'Retry-budget exhaustion must never close an Issue (see docs/decisions/0016).\n' >&2
  exit 1
fi

# Retry-budget exhaustion is never allowed to close an Issue (docs/decisions/
# 0016): every `issues/$issue" --method PATCH -f state=closed` call site must
# be one of the 4 allowlisted dispositions (worker.sh completed x2,
# worker_state.sh stale x1, dispose.sh x1). A 5th call site, or one appearing
# inside retry_failed/recover_expired, means a failure path started closing
# Issues again.
close_pattern='issues/$issue" --method PATCH -f state=closed'
close_total=0
for src in "${AGENTIC_LOOP_SOURCES[@]}"; do
  count=$(grep -Fc -- "$close_pattern" "$src" 2>/dev/null || true)
  close_total=$((close_total + count))
done
if (( close_total != 4 )); then
  printf 'Issue-close call sites changed: expected exactly 4 (worker.sh completed x2, worker_state.sh stale x1, dispose.sh x1), found %s.\n' "$close_total" >&2
  exit 1
fi
for guarded_fn in retry_failed recover_expired; do
  fn_body=$(awk -v fn="$guarded_fn" '
    $0 ~ "^" fn "\\(\\) \\{" { capture=1; next }
    capture && /^}/ { capture=0 }
    capture { print }
  ' bin/lib/agentic-loop/worker_state.sh)
  if grep -Fq 'state=closed' <<< "$fn_body"; then
    printf '%s must not close Issues directly; retry-budget exhaustion must park, not close (see docs/decisions/0016).\n' "$guarded_fn" >&2
    exit 1
  fi
done
grep -Fq 'state=closed -f state_reason=not_planned' bin/lib/agentic-loop/worker_state.sh || {
  printf 'Stale close must record state_reason=not_planned.\n' >&2
  exit 1
}
grep -Fq 'state=closed -f state_reason=not_planned' bin/lib/agentic-loop/dispose.sh || {
  printf 'Dispose close must record state_reason=not_planned.\n' >&2
  exit 1
}
grep -Fq 'docs/decisions/0016-failure-park-not-close.md' scripts/lib/foundation-files.sh || {
  printf 'Failure-park ADR is not distributed.\n' >&2
  exit 1
}
grep -Fq 'recover_expired || true' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Supervisor poll is not resilient to transient API errors.\n' >&2; exit 1; }
grep -Fq 'supervisor_graceful_shutdown' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Graceful shutdown handler is missing.\n' >&2; exit 1; }
grep -Fq 'setsid "$0" _worker' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Workers are not started in their own process group.\n' >&2; exit 1; }
grep -Fq 'worker_alive "$issue" && continue' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Restart recovery lacks the local worker fast path.\n' >&2; exit 1; }
grep -Fq 'issues/comments/$id" --method PATCH' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Lease heartbeat does not update a single comment in place.\n' >&2; exit 1; }
grep -Fq 'core_budget_note_pause' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'REST(core) budget governor is missing from the claim gate.\n' >&2; exit 1; }
grep -Fq 'next_poll_interval' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Adaptive idle poll backoff is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:dependency-blocked' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Issue dependency gating is missing.\n' >&2; exit 1; }
grep -Fq 'resume_probe() {' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Worker resume phase detection is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:handoff' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Worker resume handoff comment is missing.\n' >&2; exit 1; }
grep -Fq 'worker_confirm_running_label' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Worker resume ownership re-check is missing.\n' >&2; exit 1; }
grep -Fq '中断からの再開' docs/operations/issue-queue.md || { printf 'Worker resume documentation is missing.\n' >&2; exit 1; }
grep -Fq 'enforce_worker_timeout' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Per-worker hang timeout enforcement is missing.\n' >&2; exit 1; }
grep -Eq '^worker_timeout_seconds[[:space:]]*=[[:space:]]*14400$' .agentic-loop.toml || { printf 'Unsafe worker_timeout_seconds default.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:worker-timeout' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Worker-timeout audit comment marker is missing.\n' >&2; exit 1; }
grep -Fq 'ハング' docs/operations/issue-queue.md || { printf 'Worker hang timeout documentation is missing.\n' >&2; exit 1; }
grep -Fq 'metrics) cmd_metrics' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Metrics command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq '[--days N] [--as-of EPOCH] [--format json]' docs/operations/loop-metrics.md || { printf 'Metrics machine-readable interface is not documented.\n' >&2; exit 1; }
grep -Fq 'metrics_close_attempt' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Metrics attempt-lifecycle aggregation is missing.\n' >&2; exit 1; }
grep -Fq 'worker単位の内訳・ランキングは出力しない' docs/operations/loop-metrics.md || { printf 'Metrics privacy guarantee is not documented.\n' >&2; exit 1; }
grep -Fq '追加費用ゼロ' docs/decisions/0007-loop-metrics.md || { printf 'Metrics cost-neutrality is not documented.\n' >&2; exit 1; }
grep -Fq 'docs/operations/loop-metrics.md' scripts/lib/foundation-files.sh || { printf 'Metrics documentation is not distributed.\n' >&2; exit 1; }
if grep -Eq 'danger-full-access|OPENAI_API_KEY' "${AGENTIC_LOOP_SOURCES[@]}" bin/agentic-loop-diagnose .agentic-loop/diagnose-codebase.sh install.sh scripts/install-target.sh scripts/upgrade-target.sh scripts/lib/foundation-files.sh scripts/upgrade/migrations/0001-foundation-config-section.sh scripts/upgrade/migrations/0002-traceability-config.sh; then
  printf 'Forbidden Codex execution or API-key billing configuration.\n' >&2
  exit 1
fi

grep -Fq 'upgrade) cmd_upgrade' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Upgrade command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'docs/operations/upgrade.md' scripts/lib/foundation-files.sh || { printf 'Upgrade documentation is not distributed.\n' >&2; exit 1; }
grep -Fq 'docs/decisions/0009-foundation-upgrade.md' scripts/lib/foundation-files.sh || { printf 'Upgrade ADR is not distributed.\n' >&2; exit 1; }
grep -Fq 'upgrade --format json' docs/operations/upgrade.md || { printf 'Upgrade machine-readable interface is not documented.\n' >&2; exit 1; }
grep -Fq '無断上書き' docs/operations/upgrade.md || { printf 'Upgrade documentation lacks the no-silent-overwrite invariant.\n' >&2; exit 1; }
grep -Fq '暗黙' docs/operations/upgrade.md || { printf 'Upgrade documentation lacks the no-implicit-main-tracking invariant.\n' >&2; exit 1; }
grep -Fq '追加費用ゼロ' docs/decisions/0009-foundation-upgrade.md || { printf 'Upgrade cost-neutrality is not documented.\n' >&2; exit 1; }
grep -Fq 'foundation_manifest_write' scripts/lib/foundation-files.sh || { printf 'Manifest generation is missing from the shared distribution library.\n' >&2; exit 1; }

grep -Fq 'trace) cmd_trace' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Trace command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'trace_gate' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Requirement traceability gate is missing.\n' >&2; exit 1; }
grep -Fq 'trace --audit [--days N] [--format json]' docs/operations/traceability.md || { printf 'Traceability audit CLI is not documented.\n' >&2; exit 1; }
grep -Fq '追加費用ゼロ' docs/decisions/0017-requirement-traceability.md || { printf 'Traceability cost-neutrality is not documented.\n' >&2; exit 1; }
grep -Fq 'docs/operations/traceability.md' scripts/lib/foundation-files.sh || { printf 'Traceability documentation is not distributed.\n' >&2; exit 1; }
grep -Fq 'docs/decisions/0017-requirement-traceability.md' scripts/lib/foundation-files.sh || { printf 'Traceability ADR is not distributed.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:traceability' .github/PULL_REQUEST_TEMPLATE.md || { printf 'PR template lacks the traceability record template.\n' >&2; exit 1; }
cmp -s .agents/skills/diagnose-codebase/SKILL.md .claude/skills/diagnose-codebase/SKILL.md || { printf 'Codex and Claude diagnose-codebase skills diverged.\n' >&2; exit 1; }
grep -Fq 'trace --audit' .agents/skills/diagnose-codebase/SKILL.md || { printf 'diagnose-codebase skill does not reference the traceability audit as an evidence source.\n' >&2; exit 1; }

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
