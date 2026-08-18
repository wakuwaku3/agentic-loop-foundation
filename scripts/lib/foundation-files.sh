# Sourced by scripts/install-target.sh and scripts/upgrade-target.sh so both
# agree on exactly one definition of which files Foundation distributes.
#
# SHARED_FILES are Foundation-managed: install copies them, and `agentic-loop
# upgrade` may update them later (see docs/operations/upgrade.md). INIT_FILES
# are a one-time seed for brand-new repositories only; once copied they become
# the target's own files and upgrade never overwrites or removes them.
readonly SHARED_FILES=(
  AGENTS.md .codex/config.toml .claude/settings.json .claude/hooks/confirm-main-worktree-edit.sh docs/policies/cost.md docs/policies/testing.md docs/policies/external-environment.md docs/policies/development-environment.md docs/policies/ai-tool-neutrality.md
  docs/policies/github-language.md docs/policies/validation-harness.md docs/policies/continuous-delivery.md
  docs/decisions/0002-github-issue-queue.md docs/decisions/0003-supervisor-resilience-and-api-budget.md docs/decisions/0004-worker-resume-and-handoff.md docs/decisions/0005-status-observability.md docs/decisions/0006-worker-hang-timeout.md docs/decisions/0007-loop-metrics.md docs/decisions/0008-multi-host-claim.md docs/decisions/0009-foundation-upgrade.md docs/decisions/0010-authorized-issue-disposition.md docs/decisions/0011-installed-runtime-profile.md docs/decisions/0012-provider-pool-fallback.md docs/decisions/0013-agentic-loop-modules.md docs/decisions/0014-scope-structural-conflict.md docs/decisions/0015-numeric-priority-marker.md docs/operations/issue-queue.md docs/operations/codebase-diagnosis.md docs/operations/loop-metrics.md docs/operations/upgrade.md
  .agents/skills/submit-requirement/SKILL.md
  .agents/skills/submit-requirement/agents/openai.yaml
  .agents/skills/diagnose-codebase/SKILL.md
  .agents/skills/diagnose-codebase/agents/openai.yaml
  .claude/skills/submit-requirement/SKILL.md
  .claude/skills/diagnose-codebase/SKILL.md
  .agentic-loop/guard-secrets.sh .agentic-loop/update-main.sh .agentic-loop/diagnose-codebase.sh .agentic-loop.toml
  .githooks/pre-commit .githooks/pre-push bin/agentic-loop bin/agentic-loop-diagnose
  bin/lib/agentic-loop/common.sh bin/lib/agentic-loop/config.sh bin/lib/agentic-loop/api.sh bin/lib/agentic-loop/agent.sh
  bin/lib/agentic-loop/setup.sh bin/lib/agentic-loop/service.sh bin/lib/agentic-loop/status.sh bin/lib/agentic-loop/doctor.sh
  bin/lib/agentic-loop/upgrade.sh bin/lib/agentic-loop/metrics.sh bin/lib/agentic-loop/project.sh bin/lib/agentic-loop/dispose.sh
  bin/lib/agentic-loop/worker_state.sh bin/lib/agentic-loop/dependency.sh bin/lib/agentic-loop/scope.sh
  bin/lib/agentic-loop/priority.sh
  bin/lib/agentic-loop/supervisor.sh bin/lib/agentic-loop/worker.sh bin/lib/agentic-loop/trace.sh
)
readonly INIT_FILES=(
  README.md .editorconfig .gitignore Makefile install.sh devbox.json devbox.lock
  docs/decisions/0001-minimal-foundation.md scripts/format.sh scripts/lint.sh scripts/check-environment.sh scripts/install-target.sh
  tests/test-agentic-loop.sh .github/workflows/ci.yml
)

# Escape a value for embedding in a hand-built JSON string (shared with
# bin/agentic-loop's own copy so every --format json output agrees).
foundation_json_escape() {
  local value=$1
  value=${value//\\/\\\\}; value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}; value=${value//$'\r'/\\r}; value=${value//$'\t'/\\t}
  printf '%s' "$value"
}

foundation_sha256() { sha256sum "$1" | cut -d' ' -f1; }

# Write .agentic-loop/manifest.json recording what was just installed/upgraded:
# distribution revision, which files Foundation manages (class=shared, upgrade
# may update them) versus seeded once (class=init, upgrade never touches
# them), and the applied migration level. See docs/operations/upgrade.md.
#
# Args: target repository root, install|upgrade, repository slug, resolved
# revision (40-hex SHA or "unknown"), requested revision ref, migration level,
# newline-separated "path\tclass[\thash]" triples, and a JSON `history` array
# fragment (already-escaped objects, comma-joined). When hash is given it is
# recorded as-is (used by upgrade-target.sh to keep an unresolved conflict's
# last-known-delivered hash instead of capturing the user's own edit as if
# Foundation had just delivered it); otherwise it is computed from the file.
foundation_manifest_write() {
  local target=$1 mode=$2 repository=$3 revision=$4 revision_ref=$5 migration_level=$6 entries=$7 history=$8
  local out="$target/.agentic-loop/manifest.json" path class hash sep=''
  mkdir -p "$target/.agentic-loop"
  {
    printf '{"schema_version":1,"source":{"repository":"%s","revision":"%s","revision_ref":"%s"},"installed_at":%s,"mode":"%s","migration_level":%s,"files":[' \
      "$(foundation_json_escape "$repository")" "$(foundation_json_escape "$revision")" "$(foundation_json_escape "$revision_ref")" \
      "$(date +%s)" "$(foundation_json_escape "$mode")" "$migration_level"
    while IFS=$'\t' read -r path class hash; do
      [[ -n $path ]] || continue
      [[ -n $hash ]] || hash=$(foundation_sha256 "$target/$path")
      printf '%s{"path":"%s","class":"%s","sha256":"%s"}' "$sep" "$(foundation_json_escape "$path")" "$(foundation_json_escape "$class")" "$hash"
      sep=','
    done <<< "$entries"
    printf '],"history":[%s]}\n' "$history"
  } > "$out"
}
