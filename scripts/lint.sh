#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

./scripts/lint-cli-contracts.sh

required=(AGENTS.md README.md Makefile .editorconfig .gitignore .codex/config.toml .claude/settings.json .claude/hooks/confirm-main-worktree-edit.sh install.sh devbox.json devbox.lock docs/policies/cost.md docs/policies/testing.md docs/policies/external-environment.md docs/policies/development-environment.md docs/policies/ai-tool-neutrality.md docs/policies/github-language.md docs/policies/validation-harness.md docs/policies/continuous-delivery.md docs/decisions/0002-github-issue-queue.md docs/decisions/0003-supervisor-resilience-and-api-budget.md docs/decisions/0004-worker-resume-and-handoff.md docs/decisions/0005-status-observability.md docs/decisions/0006-worker-hang-timeout.md docs/decisions/0007-loop-metrics.md docs/decisions/0009-foundation-upgrade.md docs/decisions/0010-authorized-issue-disposition.md docs/decisions/0012-provider-pool-fallback.md docs/decisions/0013-agentic-loop-modules.md docs/decisions/0014-scope-structural-conflict.md docs/decisions/0015-numeric-priority-marker.md docs/decisions/0016-failure-park-not-close.md docs/operations/issue-queue.md docs/operations/codebase-diagnosis.md docs/operations/loop-metrics.md docs/operations/upgrade.md .agentic-loop.toml .agentic-loop/guard-secrets.sh .agentic-loop/update-main.sh .agentic-loop/diagnose-codebase.sh .githooks/pre-commit .githooks/pre-push .agents/skills/submit-requirement/SKILL.md .agents/skills/diagnose-codebase/SKILL.md .claude/skills/submit-requirement/SKILL.md .claude/skills/diagnose-codebase/SKILL.md bin/agentic-loop bin/agentic-loop-diagnose bin/lib/agentic-loop/common.sh bin/lib/agentic-loop/config.sh bin/lib/agentic-loop/api.sh bin/lib/agentic-loop/agent.sh bin/lib/agentic-loop/setup.sh bin/lib/agentic-loop/service.sh bin/lib/agentic-loop/status.sh bin/lib/agentic-loop/doctor.sh bin/lib/agentic-loop/upgrade.sh bin/lib/agentic-loop/metrics.sh bin/lib/agentic-loop/project.sh bin/lib/agentic-loop/dispose.sh bin/lib/agentic-loop/worker_state.sh bin/lib/agentic-loop/dependency.sh bin/lib/agentic-loop/scope.sh bin/lib/agentic-loop/priority.sh bin/lib/agentic-loop/supervisor.sh bin/lib/agentic-loop/worker.sh bin/lib/agentic-loop/trace.sh scripts/check-environment.sh scripts/install-target.sh scripts/lib/foundation-files.sh scripts/upgrade-target.sh scripts/upgrade/migrations/0001-foundation-config-section.sh scripts/upgrade/migrations/0002-traceability-config.sh
  docs/decisions/0017-requirement-traceability.md docs/operations/traceability.md .github/PULL_REQUEST_TEMPLATE.md
  docs/decisions/0018-repository-capability-manifest.md docs/operations/capability-manifest.md bin/lib/agentic-loop/capability.sh
  .agentic-loop/capabilities.toml scripts/upgrade/migrations/0003-capability-manifest.sh
  docs/decisions/0019-issue-level-execution-control.md bin/lib/agentic-loop/control.sh scripts/upgrade/migrations/0004-pause-control-config.sh
  docs/decisions/0020-change-risk-preflight.md docs/operations/preflight.md bin/lib/agentic-loop/preflight.sh scripts/upgrade/migrations/0005-preflight-config.sh
  docs/decisions/0021-affected-check-selection.md docs/operations/affected-checks.md scripts/affected-check.sh tests/impact-map.toml
  docs/decisions/0022-flaky-test-detection-and-quarantine.md docs/operations/flaky-tests.md scripts/flaky.sh tests/flaky-registry.toml bin/lib/agentic-loop/flaky.sh
  docs/policies/documentation.md docs/decisions/0023-documentation-readership-boundaries.md docs/development.md
  docs/decisions/0024-secret-scanning.md docs/operations/secret-scanning.md .agentic-loop/gitleaks.toml
  docs/policies/resource-scalability.md docs/decisions/0025-resource-scalability-budget.md docs/operations/workload-budget.md
  bin/lib/agentic-loop/workload.sh scripts/upgrade/migrations/0006-workload-config.sh
  docs/policies/postmortem.md docs/decisions/0026-postmortem-closed-loop.md bin/lib/agentic-loop/postmortem.sh docs/operations/postmortem.md
  scripts/upgrade/migrations/0007-postmortem-config.sh .agents/skills/postmortem/SKILL.md .agents/skills/postmortem/agents/openai.yaml .claude/skills/postmortem/SKILL.md
  docs/decisions/0030-lost-requirement-detection.md .claude/hooks/require-gh-body-file.sh
  docs/decisions/0031-stage-stall-threshold-and-provider-progress-signal.md
  docs/decisions/0032-time-constant-invariants-and-calibration.md)
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
grep -Fq 'permissionDecision":"deny"' .claude/hooks/confirm-main-worktree-edit.sh || { printf 'Claude edit hook must gate edits with a deny decision.\n' >&2; exit 1; }
# The hook must never use "ask": a PreToolUse ask is overridden by
# bypassPermissions, so it cannot actually gate anything. The queue-first gate
# denies (with guidance to the escape hatch) instead. See .claude/skills/direct-edit.
if grep -Fq 'permissionDecision":"ask"' .claude/hooks/confirm-main-worktree-edit.sh; then
  printf 'Claude edit hook must not use ask (bypassPermissions overrides it); deny instead.\n' >&2
  exit 1
fi
grep -Fq 'AGENTIC_LOOP_AGENT' .claude/hooks/confirm-main-worktree-edit.sh bin/lib/agentic-loop/agent.sh || { printf 'Edit guard cannot distinguish autonomous runs (AGENTIC_LOOP_AGENT marker missing).\n' >&2; exit 1; }
grep -Fq 'agentic-loop-allow-edit' .claude/hooks/confirm-main-worktree-edit.sh .claude/skills/direct-edit/SKILL.md || { printf 'Edit guard escape hatch (agentic-loop-allow-edit) is not implemented or not documented.\n' >&2; exit 1; }
grep -Fq '.claude/skills/direct-edit/SKILL.md' scripts/lib/foundation-files.sh || { printf 'direct-edit skill is not distributed as a shared file.\n' >&2; exit 1; }
grep -Fq '.claude/settings.json' scripts/lib/foundation-files.sh || { printf 'Claude settings are not distributed as a shared file.\n' >&2; exit 1; }
grep -Fq '.claude/hooks/confirm-main-worktree-edit.sh' scripts/lib/foundation-files.sh || { printf 'Claude edit hook is not distributed as a shared file.\n' >&2; exit 1; }
grep -Fq 'confirm-main-worktree-edit.sh' scripts/install-target.sh || { printf 'Claude edit hook is not made executable on install.\n' >&2; exit 1; }

[[ $(yq -p json -r '.hooks.PreToolUse[1].matcher // ""' .claude/settings.json) == 'Bash' ]] || { printf 'gh --body Bash hook matcher is invalid.\n' >&2; exit 1; }
[[ $(yq -p json -r '.hooks.PreToolUse[1].hooks[0].command // ""' .claude/settings.json) == '${CLAUDE_PROJECT_DIR}/.claude/hooks/require-gh-body-file.sh' ]] || { printf 'gh --body Bash hook command is invalid.\n' >&2; exit 1; }
grep -Fq 'permissionDecision":"deny"' .claude/hooks/require-gh-body-file.sh || { printf 'gh --body Bash hook must gate matches with a deny decision.\n' >&2; exit 1; }
if grep -Fq 'permissionDecision":"ask"' .claude/hooks/require-gh-body-file.sh; then
  printf 'gh --body Bash hook must not use ask (bypassPermissions overrides it); deny instead.\n' >&2
  exit 1
fi
grep -Fq -- '--body-file' .claude/hooks/require-gh-body-file.sh || { printf 'gh --body Bash hook must point operators at --body-file.\n' >&2; exit 1; }
grep -Fq '.claude/hooks/require-gh-body-file.sh' scripts/lib/foundation-files.sh || { printf 'gh --body Bash hook is not distributed as a shared file.\n' >&2; exit 1; }
grep -Fq 'require-gh-body-file.sh' scripts/install-target.sh || { printf 'gh --body Bash hook is not made executable on install.\n' >&2; exit 1; }

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
for requirement in 'local fast check' 'local full check' 'public repository' 'private repository' 'push gate' 'merge gate' 'commit SHA' 'hook bypass' 'AI review' 'make smoke' 'merge gateへ自動的に組み込まない'; do
  grep -Fq "$requirement" docs/policies/validation-harness.md || {
    printf 'Validation harness policy lacks requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
grep -Fq 'docs/policies/validation-harness.md' scripts/lib/foundation-files.sh || {
  printf 'Validation harness policy is not distributed.\n' >&2
  exit 1
}
# The explicit smoke check (Issue #279) must exist as a CLI entry point and
# must never become a merge-gate prerequisite: it touches the real GitHub API
# and provider CLI, which is non-deterministic and billable.
grep -Fq 'smoke) cmd_smoke' bin/agentic-loop || {
  printf 'bin/agentic-loop smoke entry point is missing.\n' >&2
  exit 1
}
awk '/^check:/{print; exit}' Makefile | grep -Fq 'smoke' && {
  printf 'Makefile check target must not depend on smoke (network/provider quota, not a merge gate).\n' >&2
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
grep -Fq 'devbox run --pure -- env AGENTIC_LOOP_TEST_GROUP=${{ matrix.group }} make check' .github/workflows/ci.yml || {
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
# be one of the 5 allowlisted dispositions (worker.sh completed x2, worker.sh
# postmortem-complete x1 (Issue #132, gated by postmortem_complete_gate, never
# by retry exhaustion), worker_state.sh stale x1, dispose.sh x1). A 6th call
# site, or one appearing inside retry_failed/recover_expired, means a failure
# path started closing Issues again.
close_pattern='issues/$issue" --method PATCH -f state=closed'
close_total=0
for src in "${AGENTIC_LOOP_SOURCES[@]}"; do
  count=$(grep -Fc -- "$close_pattern" "$src" 2>/dev/null || true)
  close_total=$((close_total + count))
done
if (( close_total != 5 )); then
  printf 'Issue-close call sites changed: expected exactly 5 (worker.sh completed x2, worker.sh postmortem-complete x1, worker_state.sh stale x1, dispose.sh x1), found %s.\n' "$close_total" >&2
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
grep -Fq 'issues/comments/$1" --method PATCH' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Lease heartbeat does not update a single comment in place.\n' >&2; exit 1; }

# ADR 0029 (Issue #193): worker-orphan reap must run on every Supervisor poll,
# and must never itself move an Issue's agent:* Label -- that responsibility
# stays with the existing recovery paths it is deliberately kept separate
# from (see the ADR's rejected "integrate into worker_timeout" alternative).
grep -Fq 'reap_orphan_workers || true' bin/lib/agentic-loop/supervisor.sh || {
  printf 'reap_orphan_workers is not wired into the Supervisor poll loop.\n' >&2
  exit 1
}
reap_orphan_workers_body=$(awk '
  $0 ~ "^reap_orphan_workers\\(\\) \\{" { capture=1; next }
  capture && /^}/ { capture=0 }
  capture { print }
' bin/lib/agentic-loop/worker_state.sh)
if grep -Fq 'set_issue_state' <<< "$reap_orphan_workers_body"; then
  printf 'reap_orphan_workers must not change an Issue Label; it only stops the local process and clears local state (see docs/decisions/0029-worker-orphan-reap.md).\n' >&2
  exit 1
fi
grep -Fq 'comment_patch "$id" "$(lease_body' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Lease heartbeat does not update a single comment in place.\n' >&2; exit 1; }
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
if grep -Eq 'danger-full-access|OPENAI_API_KEY' "${AGENTIC_LOOP_SOURCES[@]}" bin/agentic-loop-diagnose .agentic-loop/diagnose-codebase.sh install.sh scripts/install-target.sh scripts/upgrade-target.sh scripts/lib/foundation-files.sh scripts/upgrade/migrations/0001-foundation-config-section.sh scripts/upgrade/migrations/0002-traceability-config.sh scripts/upgrade/migrations/0003-capability-manifest.sh; then
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

# --- repository capability manifest (Issue #56, ADR 0018) ---
grep -Fq 'capabilities) cmd_capabilities' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Capabilities command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'capabilities --format json' docs/operations/issue-queue.md || { printf 'Capabilities machine-readable interface is not documented.\n' >&2; exit 1; }
grep -Fq 'docs/operations/capability-manifest.md' scripts/lib/foundation-files.sh || { printf 'Capability manifest documentation is not distributed.\n' >&2; exit 1; }
grep -Fq 'docs/decisions/0018-repository-capability-manifest.md' scripts/lib/foundation-files.sh || { printf 'Capability manifest ADR is not distributed.\n' >&2; exit 1; }
grep -Fq 'capability_generate' scripts/install-target.sh scripts/upgrade/migrations/0003-capability-manifest.sh || { printf 'Capability manifest is not seeded on install/upgrade.\n' >&2; exit 1; }
grep -Fq '.agentic-loop/capabilities.toml' .agentic-loop/update-main.sh || { printf 'Main-sync does not tolerate a freshly generated, uncommitted capability manifest.\n' >&2; exit 1; }
# shellcheck source=bin/lib/agentic-loop/capability.sh
source bin/lib/agentic-loop/capability.sh
if ! capability_validate "$PWD" "$(yq -p toml -r '.foundation.verify_command // ""' .agentic-loop.toml 2>/dev/null)"; then
  printf 'This repository'"'"'s own capability manifest failed validation:\n' >&2
  for cap_i in "${!CAPABILITY_LEVELS[@]}"; do
    [[ ${CAPABILITY_LEVELS[$cap_i]} == failure ]] && printf '  [%s] %s\n' "${CAPABILITY_CODES[$cap_i]}" "${CAPABILITY_MESSAGES[$cap_i]}" >&2
  done
  exit 1
fi
[[ $(yq -p toml -o yaml '.undetermined | length' .agentic-loop/capabilities.toml) -eq 0 ]] || {
  printf 'This Foundation repository knows all of its own capabilities; capabilities.toml must not leave anything undetermined for itself.\n' >&2
  exit 1
}

# --- Issue-level execution control: pause/resume/abort (Issue #57, ADR 0019) ---
grep -Fq 'pause) cmd_pause' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Pause command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'abort) cmd_abort' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Abort command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'control_resume_paused' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Paused-Issue resume is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:pause schema=1' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Pause audit marker is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:abort schema=1' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Abort audit marker is missing.\n' >&2; exit 1; }
grep -Fq 'drain_paused_workers' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Paused-Issue worker drain on restart/poll is missing.\n' >&2; exit 1; }
grep -Fq 'worker_stop_requested' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Cooperative worker stop-request check is missing.\n' >&2; exit 1; }
grep -Fq 'worker_critical_active' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Unsafe-to-interrupt critical-section guard is missing.\n' >&2; exit 1; }
grep -Eq '^pause_grace_seconds[[:space:]]*=[[:space:]]*120$' .agentic-loop.toml || { printf 'Unsafe pause_grace_seconds default.\n' >&2; exit 1; }
grep -Fq 'pause ISSUE' docs/operations/issue-queue.md || { printf 'pause command is not documented.\n' >&2; exit 1; }
grep -Fq '一時停止' docs/operations/issue-queue.md || { printf 'Pause/resume/abort documentation is missing.\n' >&2; exit 1; }
grep -Fq 'docs/decisions/0019-issue-level-execution-control.md' scripts/lib/foundation-files.sh || { printf 'Issue-level execution control ADR is not distributed.\n' >&2; exit 1; }
grep -Fq 'bin/lib/agentic-loop/control.sh' scripts/lib/foundation-files.sh || { printf 'control.sh module is not distributed.\n' >&2; exit 1; }
grep -Fq '追加費用ゼロ' docs/decisions/0019-issue-level-execution-control.md || { printf 'Execution control cost-neutrality is not documented.\n' >&2; exit 1; }
# Pause/abort/resume must never close an Issue (see docs/decisions/0016): the
# same allowlisted 4 call sites as before must remain the only ones, and the
# new control.sh functions must not introduce a 5th.
for guarded_fn in cmd_pause cmd_abort control_resume_paused; do
  fn_body=$(awk -v fn="$guarded_fn" '
    $0 ~ "^" fn "\\(\\) \\{" { capture=1; next }
    capture && /^}/ { capture=0 }
    capture { print }
  ' bin/lib/agentic-loop/control.sh)
  if grep -Fq 'state=closed' <<< "$fn_body"; then
    printf '%s must not close Issues; pause/abort are execution control, not disposal (see docs/decisions/0019).\n' "$guarded_fn" >&2
    exit 1
  fi
done

# --- change-risk preflight gate (Issue #58, ADR 0020) ---
grep -Fq 'preflight) cmd_preflight' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Preflight command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'preflight_gate' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Change-risk preflight gate is missing.\n' >&2; exit 1; }
grep -Fq 'preflight_reevaluate_diff' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Change-risk preflight escalation re-evaluation is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:preflight' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Preflight audit marker is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:preflight-approved' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Preflight approval marker is missing.\n' >&2; exit 1; }
grep -Fq 'reason=preflight-approval' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Preflight approval-required gate marker is missing.\n' >&2; exit 1; }
grep -Fq 'reason=preflight-escalation' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Preflight escalation gate marker is missing.\n' >&2; exit 1; }
grep -Eq '^preflight[[:space:]]*=[[:space:]]*"warn"$' .agentic-loop.toml || { printf 'Unsafe preflight default.\n' >&2; exit 1; }
grep -Fq 'preflight ISSUE' docs/operations/issue-queue.md || { printf 'preflight command is not documented.\n' >&2; exit 1; }
grep -Fq '追加費用ゼロ' docs/decisions/0020-change-risk-preflight.md || { printf 'Preflight cost-neutrality is not documented.\n' >&2; exit 1; }
grep -Fq 'docs/decisions/0020-change-risk-preflight.md' scripts/lib/foundation-files.sh || { printf 'Preflight ADR is not distributed.\n' >&2; exit 1; }
grep -Fq 'docs/operations/preflight.md' scripts/lib/foundation-files.sh || { printf 'Preflight documentation is not distributed.\n' >&2; exit 1; }
for requirement in 'category' 'needs-input' '検証ハーネスポリシー' '継続的デリバリーポリシー'; do
  grep -Fq "$requirement" docs/operations/preflight.md || {
    printf 'Preflight documentation lacks requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
# Preflight is execution control, never disposal (see docs/decisions/0016):
# it must not close Issues itself.
preflight_fn_body=$(awk '
  $0 ~ "^preflight_gate\\(\\) \\{" || $0 ~ "^preflight_reevaluate_diff\\(\\) \\{" || $0 ~ "^cmd_preflight\\(\\) \\{" { capture=1; next }
  capture && /^}/ { capture=0 }
  capture { print }
' bin/lib/agentic-loop/preflight.sh)
if grep -Fq 'state=closed' <<< "$preflight_fn_body"; then
  printf 'Preflight gate/CLI must not close Issues (see docs/decisions/0016).\n' >&2
  exit 1
fi
# The gate and the post-exec escalation re-evaluation must derive their token
# from the single preflight_envelope_token function, never from their own
# sha256sum call, or the two stages can silently diverge on the same envelope
# (Issue #218).
if [[ $(grep -c '| sha256sum' bin/lib/agentic-loop/preflight.sh) -ne 1 ]]; then
  printf 'Preflight token derivation must have exactly one sha256sum call site (see docs/decisions/0020, Issue #218).\n' >&2
  exit 1
fi
reevaluate_diff_body=$(awk '
  $0 ~ "^preflight_reevaluate_diff\\(\\) \\{" { capture=1; next }
  capture && /^}/ { capture=0 }
  capture { print }
' bin/lib/agentic-loop/preflight.sh)
grep -Fq 'preflight_envelope_token' <<< "$reevaluate_diff_body" || {
  printf 'preflight_reevaluate_diff must derive its token via preflight_envelope_token, the same function preflight_gate uses (Issue #218).\n' >&2
  exit 1
}

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

# --- local affected check (Issue #59, ADR 0021) ---
grep -Fq 'local affected check' docs/policies/validation-harness.md || {
  printf 'Missing local affected check invariant.\n' >&2
  exit 1
}
grep -Fq 'docs/decisions/0021-affected-check-selection.md' scripts/lib/foundation-files.sh || {
  printf 'Affected-check ADR is not distributed.\n' >&2
  exit 1
}
grep -Fq 'docs/operations/affected-checks.md' scripts/lib/foundation-files.sh || {
  printf 'Affected-check documentation is not distributed.\n' >&2
  exit 1
}
grep -Fq 'scripts/affected-check.sh' scripts/lib/foundation-files.sh || {
  printf 'affected-check.sh is not distributed.\n' >&2
  exit 1
}
grep -Fq 'tests/impact-map.toml' scripts/lib/foundation-files.sh || {
  printf 'impact-map.toml is not distributed.\n' >&2
  exit 1
}
grep -Fq 'tests/run-e2e.sh' scripts/lib/foundation-files.sh || {
  printf 'run-e2e.sh is not distributed.\n' >&2
  exit 1
}
grep -Eq '^affected:' Makefile || { printf 'affected Make target is missing.\n' >&2; exit 1; }
grep -Eq '^affected-audit:' Makefile || { printf 'affected-audit Make target is missing.\n' >&2; exit 1; }
grep -Fq '"affected": "make affected"' devbox.json || { printf 'affected devbox script is missing.\n' >&2; exit 1; }
grep -Fq '"affected-audit": "make affected-audit"' devbox.json || { printf 'affected-audit devbox script is missing.\n' >&2; exit 1; }
# affected-check.sh must never gain an exclusion/skip interface: flaky or
# past-failure-based test exclusion is not allowed (see ADR 0021).
if grep -Eiq 'flaky|skip-group|known-failure' scripts/affected-check.sh; then
  printf 'affected-check.sh must not gain a flaky/skip-based exclusion interface.\n' >&2
  exit 1
fi
if grep -Fq -- '--exclude)' scripts/affected-check.sh; then
  printf 'affected-check.sh must not gain an --exclude interface.\n' >&2
  exit 1
fi
grep -Eq '^check: environment lint test$' Makefile || {
  printf 'check target prerequisites must remain "environment lint test" (affected must never become part of the gate).\n' >&2
  exit 1
}
if ! grep -A1 -E '^test:' Makefile | grep -Fxq $'\t./tests/run-e2e.sh'; then
  printf 'test target must invoke run-e2e.sh without group flags (affected check must not narrow the gate).\n' >&2
  exit 1
fi
if grep -Fq 'affected' .github/workflows/ci.yml; then
  printf 'CI must not reference the local affected check (see ADR 0021).\n' >&2
  exit 1
fi
./scripts/affected-check.sh --audit || { printf 'tests/impact-map.toml failed --audit.\n' >&2; exit 1; }

# --- flaky test detection and quarantine (Issue #60, ADR 0022) ---
grep -Fq 'flaky) cmd_flaky' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'flaky command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'flaky_registry_validate' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'flaky registry validation is missing.\n' >&2; exit 1; }
grep -Fq 'cmd_flaky_report' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'flaky report Issue creation is missing.\n' >&2; exit 1; }
grep -Fq 'docs/decisions/0022-flaky-test-detection-and-quarantine.md' scripts/lib/foundation-files.sh || {
  printf 'Flaky-test ADR is not distributed.\n' >&2
  exit 1
}
grep -Fq 'docs/operations/flaky-tests.md' scripts/lib/foundation-files.sh || {
  printf 'Flaky-test documentation is not distributed.\n' >&2
  exit 1
}
grep -Fq 'scripts/flaky.sh' scripts/lib/foundation-files.sh || {
  printf 'flaky.sh is not distributed.\n' >&2
  exit 1
}
grep -Fq 'tests/flaky-registry.toml' scripts/lib/foundation-files.sh || {
  printf 'flaky-registry.toml is not distributed.\n' >&2
  exit 1
}
grep -Fq 'bin/lib/agentic-loop/flaky.sh' scripts/lib/foundation-files.sh || {
  printf 'flaky.sh module is not distributed.\n' >&2
  exit 1
}
# retry is diagnostic-only: quarantine must never become a general skip/
# exclusion interface, on either the retry orchestrator or the classifier
# (extends the ADR 0021 affected-check.sh invariant above; see ADR 0022).
if grep -Fq -- '--exclude)' scripts/flaky.sh tests/run-e2e.sh; then
  printf 'flaky.sh/run-e2e.sh must not gain an --exclude interface.\n' >&2
  exit 1
fi
if grep -Fq -- '--skip)' scripts/flaky.sh tests/run-e2e.sh; then
  printf 'flaky.sh/run-e2e.sh must not gain a --skip interface.\n' >&2
  exit 1
fi
grep -Eq '^[[:space:]]*timeout-minutes:[[:space:]]*30$' .github/workflows/ci.yml || {
  printf 'CI timeout-minutes was not raised for isolated flaky retries (see ADR 0022).\n' >&2
  exit 1
}
ci_push_block=$(sed -n '/^  push:$/,/^  pull_request:$/p' .github/workflows/ci.yml)
[[ $ci_push_block == $'  push:\n    branches:\n      - main\n  pull_request:' ]] || {
  printf 'CI push trigger must be restricted to main to avoid duplicate PR checks.\n' >&2
  exit 1
}
grep -Eq '^[[:space:]]*pull_request:[[:space:]]*$' .github/workflows/ci.yml || {
  printf 'CI pull_request trigger is required for PR checks.\n' >&2
  exit 1
}
for group in queue lifecycle auxiliary upgrade; do
  grep -Eq "^[[:space:]]+-[[:space:]]+$group[[:space:]]*$" .github/workflows/ci.yml || {
    printf 'CI matrix is missing required E2E group: %s\n' "$group" >&2
    exit 1
  }
done
grep -Fq 'devbox run --pure -- env AGENTIC_LOOP_TEST_GROUP=${{ matrix.group }} make check' .github/workflows/ci.yml || {
  printf 'CI matrix group is not connected to the common check entry point.\n' >&2
  exit 1
}
grep -Fq '"flaky"' bin/lib/agentic-loop/setup.sh || { printf 'flaky label is not created by setup.\n' >&2; exit 1; }
grep -Fq 'label:"agent:failed","agent:stale","agent:parked","flaky"' bin/lib/agentic-loop/setup.sh || {
  printf 'Recovery view does not include the flaky label.\n' >&2
  exit 1
}
grep -Fq 'flaky test registry' bin/lib/agentic-loop/doctor.sh || { printf 'doctor does not surface flaky registry findings.\n' >&2; exit 1; }
for requirement in '再実行は検知・診断目的に限定' '最長14日'; do
  grep -Fq "$requirement" docs/policies/testing.md || {
    printf 'testing.md lacks a flaky-test requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
grep -Fq '明示的な隔離は例外である' docs/policies/validation-harness.md || {
  printf 'Missing flaky-quarantine exception invariant in validation-harness.md.\n' >&2
  exit 1
}
grep -Fq 'bin/agentic-loop flaky' .agents/skills/diagnose-codebase/SKILL.md || {
  printf 'diagnose-codebase skill does not reference the flaky registry as an evidence source.\n' >&2
  exit 1
}
grep -Fq 'bin/agentic-loop workload' .agents/skills/diagnose-codebase/SKILL.md || {
  printf 'diagnose-codebase skill does not reference the workload budget scan as an evidence source.\n' >&2
  exit 1
}
./scripts/flaky.sh audit || { printf 'tests/flaky-registry.toml failed audit.\n' >&2; exit 1; }

# --- documentation readership boundaries (Issue #64, ADR 0023) ---
grep -Fq '[文書ポリシー](docs/policies/documentation.md)' AGENTS.md || {
  printf 'Missing documentation policy invariant.\n' >&2
  exit 1
}
for requirement in '基本利用者' '第一読者' '正本' '重複' '更新責務' 'Agentic Loop（AI）' 'README.md' 'docs/development.md' 'docs/operations' 'docs/policies' 'docs/decisions' 'AGENTS.md' 'machine-readable'; do
  grep -Fq "$requirement" docs/policies/documentation.md || {
    printf 'Documentation policy lacks requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
for shared_doc in docs/policies/documentation.md docs/decisions/0023-documentation-readership-boundaries.md; do
  grep -Fq "$shared_doc" scripts/lib/foundation-files.sh || {
    printf '%s is not distributed.\n' "$shared_doc" >&2
    exit 1
  }
done
if awk '/^readonly SHARED_FILES=\(/,/^\)/' scripts/lib/foundation-files.sh | grep -Fq 'docs/development.md'; then
  printf 'docs/development.md must be an INIT_FILES seed, not a SHARED_FILES distribution.\n' >&2
  exit 1
fi
awk '/^readonly INIT_FILES=\(/,/^\)/' scripts/lib/foundation-files.sh | grep -Fq 'docs/development.md' || {
  printf 'docs/development.md is not seeded as an INIT_FILES entry.\n' >&2
  exit 1
}
readme_forbidden_pattern='devbox run --pure|make check|tests/|scripts/|bin/lib/|docs/decisions/|```toml|reasoning_effort|worker_timeout_seconds|systemctl --user|runtime\.path|\.git/agentic-loop'
if grep -Eq "$readme_forbidden_pattern" README.md; then
  printf 'README.md leaked development/internal content across the documentation readership boundary.\n' >&2
  exit 1
fi
for requirement in '## できること' '## 導入' '## 要求を出す' '## 日常の操作' '## 困ったときは' '## 文書の案内' 'curl -fsSL' 'submit-requirement' 'bin/agentic-loop status' 'bin/agentic-loop doctor' 'docs/development.md' 'docs/operations/issue-queue.md'; do
  grep -Fq "$requirement" README.md || {
    printf 'README.md lacks required basic-user element: %s\n' "$requirement" >&2
    exit 1
  }
done
for requirement in '要求の伝え方' 'レビュー' '復旧'; do
  grep -Fq "$requirement" docs/development.md || {
    printf 'docs/development.md lacks requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
for requirement in 'policies/development-environment.md' 'operations/issue-queue.md'; do
  grep -Fq "$requirement" docs/development.md || {
    printf 'docs/development.md does not link to its authoritative source: %s\n' "$requirement" >&2
    exit 1
  }
done
if grep -Fq '```toml' docs/development.md || grep -Fq 'worker_timeout_seconds' docs/development.md; then
  printf 'docs/development.md must not duplicate operations/policy rule bodies (TOML examples or config values).\n' >&2
  exit 1
fi
grep -Fq 'agentic-loop-main-sync-' docs/operations/upgrade.md || {
  printf 'Main-sync timer documentation was lost from docs/operations/upgrade.md.\n' >&2
  exit 1
}
grep -Fq 'plan_max' docs/operations/issue-queue.md || {
  printf 'Two-stage plan/exec documentation was lost from docs/operations/issue-queue.md.\n' >&2
  exit 1
}
grep -Fq 'runtime.path' docs/operations/issue-queue.md || {
  printf 'Fixed runtime PATH documentation was lost from docs/operations/issue-queue.md.\n' >&2
  exit 1
}

# --- Issue comment newline choke point (Issue #110) ---
# Every Issue/PR comment body must reach GitHub through comment_post/
# comment_patch (api.sh), the only place `\n` shorthand is expanded into a
# real newline (see common.sh's unfold_body). A new "-f body=" call site
# against a comments endpoint outside api.sh would silently reintroduce the
# literal-`\n` bug for that one path.
if grep -n -- '-f body=' "${AGENTIC_LOOP_SOURCES[@]}" | grep -Fv 'bin/lib/agentic-loop/api.sh:' | grep -F 'comments'; then
  printf 'Issue/PR comment bodies must be posted through comment_post/comment_patch.\n' >&2
  exit 1
fi
grep -Fq 'unfold_body' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'unfold_body is missing.\n' >&2; exit 1; }
grep -Fq 'comment_post()' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'comment_post is missing.\n' >&2; exit 1; }
grep -Fq 'comment_patch()' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'comment_patch is missing.\n' >&2; exit 1; }
if grep -F '//$'"'"'\n'"'"'/\\n' bin/lib/agentic-loop/preflight.sh bin/lib/agentic-loop/trace.sh; then
  printf 'preflight.sh/trace.sh must not fold real newlines back into literal \\n (that belongs only at the comment_post/comment_patch boundary).\n' >&2
  exit 1
fi

# --- secret scanning (Issue #61, ADR 0024) ---
grep -Fq 'gitleaks@' devbox.json || { printf 'devbox.json does not pin a gitleaks version.\n' >&2; exit 1; }
grep -Fq 'gitleaks' scripts/check-environment.sh || { printf 'scripts/check-environment.sh does not verify a pinned gitleaks.\n' >&2; exit 1; }
grep -Fq '"--history"' .agentic-loop/capabilities.toml || { printf 'capabilities.toml secret_guard_modes is missing --history.\n' >&2; exit 1; }
grep -Fq '"--audit"' .agentic-loop/capabilities.toml || { printf 'capabilities.toml secret_guard_modes is missing --audit.\n' >&2; exit 1; }
for shared_secret_file in .agentic-loop/gitleaks.toml docs/decisions/0024-secret-scanning.md docs/operations/secret-scanning.md; do
  grep -Fq "$shared_secret_file" scripts/lib/foundation-files.sh || {
    printf '%s is not distributed.\n' "$shared_secret_file" >&2
    exit 1
  }
done
grep -Fq 'gitleaks' bin/lib/agentic-loop/doctor.sh || { printf 'doctor does not surface secret scanner (gitleaks) resolution.\n' >&2; exit 1; }
for requirement in 'baseline層' 'fail-closed' '許可list'; do
  grep -Fq "$requirement" docs/policies/validation-harness.md || {
    printf 'validation-harness.md lacks a secret-scanning requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
if [[ -e .gitleaksignore ]]; then
  printf '.gitleaksignore is not permitted (use .agentic-loop/gitleaks.toml allowlists instead).\n' >&2
  exit 1
fi
./.agentic-loop/guard-secrets.sh --audit

# --- Closed-loop postmortem learning (Issue #132, ADR 0026) ---
grep -Fq '[ポストモーテムポリシー](docs/policies/postmortem.md)' AGENTS.md || {
  printf 'Missing postmortem policy invariant in AGENTS.md.\n' >&2
  exit 1
}
for requirement in '非難' '起動基準' '重大度' '分析項目' 'action item' '完了条件' 'max_auto_created_per_day'; do
  grep -Fq "$requirement" docs/policies/postmortem.md || {
    printf 'Postmortem policy lacks requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
grep -Fq '追加費用ゼロ' docs/decisions/0026-postmortem-closed-loop.md || {
  printf 'Postmortem cost-neutrality is not documented.\n' >&2
  exit 1
}
grep -Fq 'postmortem) cmd_postmortem' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Postmortem command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'POSTMORTEM_LABEL' bin/lib/agentic-loop/setup.sh || { printf 'postmortem label is not created by setup.\n' >&2; exit 1; }
grep -Fq 'postmortem_consider_trigger' bin/lib/agentic-loop/worker_state.sh || { printf 'Repeated-failure postmortem auto-trigger is missing from park_issue.\n' >&2; exit 1; }
grep -Fq 'postmortem_consider_trigger' bin/lib/agentic-loop/agent.sh || { printf 'Resource-exhaustion postmortem auto-trigger is missing from exhaustion_note_pause.\n' >&2; exit 1; }
grep -Fq 'postmortem_link' bin/lib/agentic-loop/postmortem.sh || { printf 'Action-item closed-loop linking is missing.\n' >&2; exit 1; }
grep -Fq 'postmortem_complete_gate' bin/lib/agentic-loop/postmortem.sh || { printf 'The postmortem complete mechanical completion gate is missing.\n' >&2; exit 1; }
grep -Fq 'dependency_satisfied' bin/lib/agentic-loop/postmortem.sh || { printf 'The postmortem complete gate does not reuse dependency_satisfied.\n' >&2; exit 1; }
grep -Fq 'postmortem_turn_marker_read' bin/lib/agentic-loop/worker.sh || { printf "worker.sh's terminal branch is missing the postmortem link/complete marker check.\n" >&2; exit 1; }
grep -Fq 'submit-requirement' .agents/skills/postmortem/SKILL.md || { printf 'The postmortem skill does not route action items through submit-requirement queue-first intake.\n' >&2; exit 1; }
grep -Fq '`AGENTS.md`、`docs/policies/`、skill、worker prompt、共通検証入口（`devbox run --pure check`）' docs/policies/postmortem.md || {
  printf 'Postmortem policy does not name all four generalization-reflection targets.\n' >&2
  exit 1
}
for cli_detail in 'postmortem create' 'postmortem link' 'postmortem status' 'postmortem complete' 'auto_detect' 'max_auto_created_per_day'; do
  grep -Fq "$cli_detail" docs/operations/postmortem.md || {
    printf 'Postmortem operations doc is missing CLI/config detail: %s\n' "$cli_detail" >&2
    exit 1
  }
done
cmp -s .agents/skills/postmortem/SKILL.md .claude/skills/postmortem/SKILL.md || { printf 'Codex and Claude postmortem skills diverged.\n' >&2; exit 1; }
for shared_doc in docs/policies/postmortem.md docs/decisions/0026-postmortem-closed-loop.md docs/operations/postmortem.md bin/lib/agentic-loop/postmortem.sh .agents/skills/postmortem/SKILL.md .claude/skills/postmortem/SKILL.md; do
  grep -Fq "$shared_doc" scripts/lib/foundation-files.sh || {
    printf '%s is not distributed.\n' "$shared_doc" >&2
    exit 1
  }
done

./.agentic-loop/guard-secrets.sh --all

# --- resource scalability and workload budget (Issue #130, ADR 0025) --------
grep -Fq '[有限資源とスケーラビリティのポリシー](docs/policies/resource-scalability.md)' AGENTS.md || {
  printf 'Missing resource scalability policy invariant.\n' >&2
  exit 1
}
for requirement in '増加率' '停止条件' '集約' '例外の記録方法' '安全弁'; do
  grep -Fq "$requirement" docs/policies/resource-scalability.md || {
    printf 'Resource scalability policy lacks requirement: %s\n' "$requirement" >&2
    exit 1
  }
done
grep -Fq 'workload) cmd_workload' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Workload command is not distributed through the queue CLI.\n' >&2; exit 1; }
grep -Fq 'workload_gate' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Workload budget gate is missing.\n' >&2; exit 1; }
grep -Fq 'workload_scan' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Workload static scan is missing.\n' >&2; exit 1; }
grep -Fq 'agentic-loop:workload' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Workload record marker is missing.\n' >&2; exit 1; }
grep -Fq 'reason=workload-missing' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Workload missing-record gate marker is missing.\n' >&2; exit 1; }
grep -Fq 'reason=workload-invalid' "${AGENTIC_LOOP_SOURCES[@]}" || { printf 'Workload invalid-record gate marker is missing.\n' >&2; exit 1; }
grep -Eq '^workload[[:space:]]*=[[:space:]]*"warn"$' .agentic-loop.toml || { printf 'Unsafe workload default.\n' >&2; exit 1; }
grep -Fq 'docs/decisions/0025-resource-scalability-budget.md' scripts/lib/foundation-files.sh || { printf 'Workload ADR is not distributed.\n' >&2; exit 1; }
grep -Fq 'docs/operations/workload-budget.md' scripts/lib/foundation-files.sh || { printf 'Workload documentation is not distributed.\n' >&2; exit 1; }
grep -Fq 'docs/policies/resource-scalability.md' scripts/lib/foundation-files.sh || { printf 'Resource scalability policy is not distributed.\n' >&2; exit 1; }
# Workload budget is execution control, never disposal (see docs/decisions/0016).
workload_fn_body=$(awk '
  $0 ~ "^workload_gate\\(\\) \\{" { capture=1; next }
  capture && /^}/ { capture=0 }
  capture { print }
' bin/lib/agentic-loop/workload.sh)
if grep -Fq 'state=closed' <<< "$workload_fn_body"; then
  printf 'Workload gate must not close Issues (see docs/decisions/0016).\n' >&2
  exit 1
fi
./bin/agentic-loop workload || { printf 'bin/agentic-loop workload detected an unannotated finite-resource/scalability violation.\n' >&2; exit 1; }

# --- time constant invariants (Issue #280, ADR 0032) ------------------------
grep -Fq 'timing_invariant_violations' bin/lib/agentic-loop/config.sh || { printf 'Time constant invariant check is missing.\n' >&2; exit 1; }
./bin/agentic-loop _timing-check --committed || { printf '.agentic-loop.toml violates a time constant invariant (see bin/agentic-loop _timing-check --committed).\n' >&2; exit 1; }

printf 'Lint passed.\n'
