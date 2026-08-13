#!/usr/bin/env bash
set -euo pipefail

readonly SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TARGET="${1:-.}"
readonly SHARED_FILES=(
  AGENTS.md .codex/config.toml docs/policies/cost.md docs/policies/testing.md docs/policies/external-environment.md docs/policies/development-environment.md docs/policies/ai-tool-neutrality.md
  docs/policies/github-language.md docs/policies/validation-harness.md docs/policies/continuous-delivery.md
  docs/decisions/0002-github-issue-queue.md docs/decisions/0003-supervisor-resilience-and-api-budget.md docs/decisions/0004-worker-resume-and-handoff.md docs/decisions/0005-status-observability.md docs/decisions/0006-worker-hang-timeout.md docs/decisions/0007-loop-metrics.md docs/decisions/0008-multi-host-claim.md docs/operations/issue-queue.md docs/operations/codebase-diagnosis.md docs/operations/loop-metrics.md
  .agents/skills/submit-requirement/SKILL.md
  .agents/skills/submit-requirement/agents/openai.yaml
  .agents/skills/diagnose-codebase/SKILL.md
  .agents/skills/diagnose-codebase/agents/openai.yaml
  .claude/skills/submit-requirement/SKILL.md
  .claude/skills/diagnose-codebase/SKILL.md
  .agentic-loop/guard-secrets.sh .agentic-loop/update-main.sh .agentic-loop/diagnose-codebase.sh .agentic-loop.toml
  .githooks/pre-commit .githooks/pre-push bin/agentic-loop bin/agentic-loop-diagnose
)
readonly INIT_FILES=(
  README.md .editorconfig .gitignore Makefile install.sh devbox.json devbox.lock
  docs/decisions/0001-minimal-foundation.md scripts/format.sh scripts/lint.sh scripts/check-environment.sh scripts/install-target.sh
  tests/test-agentic-loop.sh .github/workflows/ci.yml
)

fail() { printf 'install-target: %s\n' "$1" >&2; exit 1; }

# Resolve the AI provider the installed loop will use: the AGENT_PROVIDER
# environment overrides (matching runtime), otherwise agent.provider from the
# effective .agentic-loop.toml (the target's if present, else the source's),
# overlaid by the target's git-ignored .agentic-loop.local.toml.
effective_provider() {
  local base local_file provider
  [[ -n ${AGENT_PROVIDER:-} ]] && { printf '%s' "$AGENT_PROVIDER"; return; }
  base="$TARGET/.agentic-loop.toml"; [[ -r $base ]] || base="$SOURCE_ROOT/.agentic-loop.toml"
  local_file="$TARGET/.agentic-loop.local.toml"
  if [[ -r $base && -r $local_file ]]; then
    provider=$(yq -p toml -o tsv eval-all 'select(fi==0) * select(fi==1) | .agent.provider // ""' "$base" "$local_file" 2>/dev/null)
  elif [[ -r $base ]]; then
    provider=$(yq -p toml -o tsv '.agent.provider // ""' "$base" 2>/dev/null)
  fi
  printf '%s' "${provider:-codex}"
}

preflight() {
  local command_name provider provider_cli
  for command_name in git gh yq devbox systemctl systemd-escape; do command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"; done
  provider=$(effective_provider)
  case $provider in codex) provider_cli=codex ;; claude) provider_cli=claude ;; opencode) provider_cli=opencode ;; *) fail 'agent.provider must be codex, claude, or opencode' ;; esac
  command -v "$provider_cli" >/dev/null 2>&1 || fail "$provider_cli is required"
  git -C "$TARGET" rev-parse --git-dir >/dev/null 2>&1 || fail 'target must be a Git repository'
  git -C "$TARGET" remote get-url origin >/dev/null 2>&1 || fail 'origin remote is required'
  gh auth status >/dev/null 2>&1 || fail 'GitHub authentication is required; run gh auth login'
  gh api graphql -f query='query { viewer { login projectsV2(first: 1) { totalCount } } }' >/dev/null 2>&1 ||
    fail 'GitHub token needs repository access and project/read:project scopes'
  (cd "$TARGET" && gh repo view --json nameWithOwner --jq .nameWithOwner >/dev/null 2>&1) || fail 'cannot access the target GitHub repository'
  [[ $provider != codex ]] || codex exec --help >/dev/null 2>&1 || fail 'Codex CLI exec mode is required'
}

record_runtime_path() {
  local state_root runtime_file command_name command_path command_dir runtime_path='' provider provider_cli
  state_root="$(git -C "$TARGET" rev-parse --path-format=absolute --git-common-dir)/agentic-loop"
  runtime_file="$state_root/runtime.path"
  for command_name in git gh yq devbox systemctl systemd-escape; do
    command_path=$(command -v "$command_name")
    command_dir=$(cd "$(dirname "$command_path")" && pwd)
    case ":$runtime_path:" in *":$command_dir:"*) ;; *) runtime_path="${runtime_path:+$runtime_path:}$command_dir" ;; esac
  done
  provider=$(effective_provider)
  case $provider in codex) provider_cli=codex ;; claude) provider_cli=claude ;; opencode) provider_cli=opencode ;; esac
  command_path=$(command -v "$provider_cli")
  command_dir=$(cd "$(dirname "$command_path")" && pwd)
  case ":$runtime_path:" in *":$command_dir:"*) ;; *) runtime_path="$runtime_path:$command_dir" ;; esac
  [[ $runtime_path != *$'\n'* && $runtime_path != *$'\r'* ]] || fail 'runtime PATH contains an unsafe newline'
  mkdir -p "$state_root"
  printf '%s\n' "$runtime_path" > "$runtime_file.tmp"
  mv "$runtime_file.tmp" "$runtime_file"
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
  record_runtime_path
  "$target/bin/agentic-loop" setup
  "$target/.agentic-loop/update-main.sh" install "$target"
  "$target/.agentic-loop/diagnose-codebase.sh" install "$target"
  if [[ ${AGENTIC_LOOP_SKIP_START:-0} != 1 ]]; then "$target/bin/agentic-loop" start; fi
  printf 'Agentic loop installed (%s) in %s\n' "$mode" "$(cd "$target" && pwd)"
}

main "$@"
