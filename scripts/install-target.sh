#!/usr/bin/env bash
set -euo pipefail

readonly SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TARGET="${1:-.}"
readonly SHARED_FILES=(
  AGENTS.md .codex/config.toml docs/policies/cost.md docs/policies/testing.md docs/policies/external-environment.md docs/policies/development-environment.md docs/policies/ai-tool-neutrality.md
  docs/policies/github-language.md docs/policies/validation-harness.md docs/policies/continuous-delivery.md
  docs/decisions/0002-github-issue-queue.md docs/operations/issue-queue.md docs/operations/codebase-diagnosis.md
  .agents/skills/submit-requirement/SKILL.md
  .agents/skills/submit-requirement/agents/openai.yaml
  .agents/skills/diagnose-codebase/SKILL.md
  .agents/skills/diagnose-codebase/agents/openai.yaml
  .claude/skills/submit-requirement/SKILL.md
  .claude/skills/diagnose-codebase/SKILL.md
  .agentic-loop/guard-secrets.sh .agentic-loop/update-main.sh .agentic-loop/diagnose-codebase.sh .agentic-loop/config
  .githooks/pre-commit .githooks/pre-push bin/agentic-loop bin/agentic-loop-diagnose
)
readonly INIT_FILES=(
  README.md .editorconfig .env.example .gitignore Makefile install.sh devbox.json devbox.lock
  docs/decisions/0001-minimal-foundation.md scripts/format.sh scripts/lint.sh scripts/check-environment.sh scripts/install-target.sh
  tests/test-agentic-loop.sh .github/workflows/ci.yml
)

fail() { printf 'install-target: %s\n' "$1" >&2; exit 1; }

preflight() {
  local command_name provider=${AGENT_PROVIDER:-codex} provider_cli
  case $provider in codex) provider_cli=codex ;; claude) provider_cli=claude ;; *) fail 'AGENT_PROVIDER must be codex or claude' ;; esac
  for command_name in git gh "$provider_cli" systemctl systemd-escape; do command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"; done
  git -C "$TARGET" rev-parse --git-dir >/dev/null 2>&1 || fail 'target must be a Git repository'
  git -C "$TARGET" remote get-url origin >/dev/null 2>&1 || fail 'origin remote is required'
  gh auth status >/dev/null 2>&1 || fail 'GitHub authentication is required; run gh auth login'
  gh api graphql -f query='query { viewer { login projectsV2(first: 1) { totalCount } } }' >/dev/null 2>&1 ||
    fail 'GitHub token needs repository access and project/read:project scopes'
  (cd "$TARGET" && gh repo view --json nameWithOwner --jq .nameWithOwner >/dev/null 2>&1) || fail 'cannot access the target GitHub repository'
  [[ $provider != codex ]] || codex exec --help >/dev/null 2>&1 || fail 'Codex CLI exec mode is required'
}

main() {
  local target=$TARGET mode=install file hook_path
  local -a files=("${SHARED_FILES[@]}")
  [[ -d $target ]] || fail "target is not a directory: $target"
  preflight
  if ! find "$target" -mindepth 1 -maxdepth 1 ! -name .git -print -quit | grep -q .; then mode=init; files+=("${INIT_FILES[@]}"); fi
  hook_path=$(git -C "$target" config --local --get core.hooksPath || true)
  [[ -z $hook_path || $hook_path == .githooks ]] || fail "existing core.hooksPath must be integrated manually: $hook_path"
  for file in "${files[@]}"; do
    [[ ! -e $target/$file ]] || cmp -s "$SOURCE_ROOT/$file" "$target/$file" || fail "refusing to overwrite: $target/$file"
  done
  for file in "${files[@]}"; do
    if [[ ! -e $target/$file ]]; then mkdir -p "$target/$(dirname "$file")"; cp "$SOURCE_ROOT/$file" "$target/$file"; fi
  done
  chmod +x "$target/bin/agentic-loop" "$target/bin/agentic-loop-diagnose" "$target/.agentic-loop/guard-secrets.sh" "$target/.agentic-loop/update-main.sh" "$target/.agentic-loop/diagnose-codebase.sh" "$target/.githooks/pre-commit" "$target/.githooks/pre-push"
  [[ $mode == init ]] && chmod +x "$target/install.sh" "$target/scripts/"*.sh "$target/tests/"*.sh
  git -C "$target" config --local core.hooksPath .githooks
  "$target/bin/agentic-loop" setup
  "$target/.agentic-loop/update-main.sh" install "$target"
  "$target/.agentic-loop/diagnose-codebase.sh" install "$target"
  if [[ ${AGENTIC_LOOP_SKIP_START:-0} != 1 ]]; then "$target/bin/agentic-loop" start; fi
  printf 'Agentic loop installed (%s) in %s\n' "$mode" "$(cd "$target" && pwd)"
}

main "$@"
