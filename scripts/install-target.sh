#!/usr/bin/env bash
set -euo pipefail

readonly SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TARGET="${1:-.}"
# shellcheck source=lib/foundation-files.sh
source "$SOURCE_ROOT/scripts/lib/foundation-files.sh"

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
  local command_name provider provider_cli graphql_remaining
  for command_name in git gh yq devbox systemctl systemd-escape; do command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"; done
  provider=$(effective_provider)
  case $provider in codex) provider_cli=codex ;; claude) provider_cli=claude ;; opencode) provider_cli=opencode ;; *) fail 'agent.provider must be codex, claude, or opencode' ;; esac
  command -v "$provider_cli" >/dev/null 2>&1 || fail "$provider_cli is required"
  git -C "$TARGET" rev-parse --git-dir >/dev/null 2>&1 || fail 'target must be a Git repository'
  git -C "$TARGET" remote get-url origin >/dev/null 2>&1 || fail 'origin remote is required'
  gh auth status >/dev/null 2>&1 || fail 'GitHub authentication is required; run gh auth login'
  graphql_remaining=$(gh api rate_limit --jq '.resources.graphql.remaining' 2>/dev/null) || fail 'cannot read GitHub API rate limit'
  read -r graphql_remaining _ <<< "$graphql_remaining"
  if [[ $graphql_remaining =~ ^[0-9]+$ && $graphql_remaining -gt 0 ]]; then
    gh api graphql -f query='query { viewer { login projectsV2(first: 1) { totalCount } } }' >/dev/null 2>&1 ||
      fail 'GitHub token needs repository access and project/read:project scopes'
  else
    printf 'GraphQL rate limitが枯渇しているため、Projects権限検査とsetupを延期します。\n' >&2
  fi
  (cd "$TARGET" && gh repo view --json nameWithOwner --jq .nameWithOwner >/dev/null 2>&1) || fail 'cannot access the target GitHub repository'
  [[ $provider != codex ]] || codex exec --help >/dev/null 2>&1 || fail 'Codex CLI exec mode is required'
}

# Provision (or refresh) an install-owned Devbox virtenv at
# <state_root>/runtime, pinned to this Foundation source's own devbox.json /
# devbox.lock. Unlike the transient --config directory install.sh may have
# used to bootstrap yq for this very script (which can be a mktemp tree
# deleted when install.sh exits, or otherwise not owned by this target),
# state_root lives under the target's own git-common-dir and is never removed
# by install/uninstall, so the Devbox profile symlink created here is a nix
# GC root that survives indefinitely. Prints the profile's bin directory.
provision_runtime_virtenv() {
  local state_root=$1 runtime_dir profile_bin file
  runtime_dir="$state_root/runtime"
  mkdir -p "$runtime_dir"
  for file in devbox.json devbox.lock; do
    if [[ ! -f "$runtime_dir/$file" ]] || ! cmp -s "$SOURCE_ROOT/$file" "$runtime_dir/$file"; then
      cp "$SOURCE_ROOT/$file" "$runtime_dir/$file"
    fi
  done
  devbox run --config "$runtime_dir" -- true || fail "failed to provision the persistent Devbox runtime at $runtime_dir"
  profile_bin="$runtime_dir/.devbox/nix/profile/default/bin"
  [[ -x "$profile_bin/yq" ]] || fail "persistent Devbox runtime did not produce yq at $profile_bin (Devbox's internal profile layout may have changed)"
  printf '%s' "$profile_bin"
}

# Record the verified tool directories that bin/agentic-loop restores onto
# PATH at startup (see bin/agentic-loop's preamble). The install-owned
# virtenv's bin directory is recorded first so pinned tools (yq, git) resolve
# there and survive nix GC indefinitely. Remaining commands are recorded by
# their current logical directory (not resolved to a nix store realpath):
# doing so keeps host-installed tools (gh, devbox, provider CLIs) tracking
# whatever the host's own package manager or update mechanism points them at,
# rather than pinning to a store path that becomes stale the moment the host
# updates that tool. A command resolving only under the (possibly transient)
# --config directory this very script was bootstrapped from is never recorded
# directly; if the persistent virtenv cannot supply it either, install fails
# loudly instead of silently recording a directory likely to vanish.
record_runtime_path() {
  local state_root runtime_file command_name command_path command_dir runtime_path provider provider_cli profile_bin ephemeral_prefix
  state_root="$(git -C "$TARGET" rev-parse --path-format=absolute --git-common-dir)/agentic-loop"
  runtime_file="$state_root/runtime.path"
  profile_bin=$(provision_runtime_virtenv "$state_root")
  runtime_path="$profile_bin"
  ephemeral_prefix="$SOURCE_ROOT/.devbox"
  provider=$(effective_provider)
  case $provider in codex) provider_cli=codex ;; claude) provider_cli=claude ;; opencode) provider_cli=opencode ;; esac
  for command_name in git gh devbox systemctl systemd-escape "$provider_cli"; do
    command_path=$(command -v "$command_name")
    command_dir=$(cd "$(dirname "$command_path")" && pwd)
    case $command_dir in
      "$ephemeral_prefix"/*)
        [[ -x "$profile_bin/$command_name" ]] ||
          fail "cannot record a stable directory for $command_name: it only resolves under the transient bootstrap Devbox environment ($command_dir). Install $command_name on the host, or add it to devbox.json so the persistent runtime can supply it."
        continue
        ;;
    esac
    case ":$runtime_path:" in *":$command_dir:"*) ;; *) runtime_path="$runtime_path:$command_dir" ;; esac
  done
  [[ $runtime_path != *$'\n'* && $runtime_path != *$'\r'* ]] || fail 'runtime PATH contains an unsafe newline'
  mkdir -p "$state_root"
  printf '%s\n' "$runtime_path" > "$runtime_file.tmp"
  mv "$runtime_file.tmp" "$runtime_file"
}

main() {
  local target=$TARGET mode=install file hook_path repository revision revision_ref migration_level entries='' history
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

  repository=${AGENTIC_LOOP_REPOSITORY:-wakuwaku3/agentic-loop-foundation}
  revision=${AGENTIC_LOOP_RESOLVED_REVISION:-}
  [[ -n $revision ]] || revision=$(git -C "$SOURCE_ROOT" rev-parse HEAD 2>/dev/null || printf 'unknown')
  revision_ref=${AGENTIC_LOOP_REVISION:-main}
  migration_level=$(find "$SOURCE_ROOT/scripts/upgrade/migrations" -maxdepth 1 -name '[0-9][0-9][0-9][0-9]-*.sh' 2>/dev/null | wc -l | tr -d ' ')

  chmod +x "$target/bin/agentic-loop" "$target/bin/agentic-loop-diagnose" "$target/.agentic-loop/guard-secrets.sh" "$target/.agentic-loop/update-main.sh" "$target/.agentic-loop/diagnose-codebase.sh" "$target/.githooks/pre-commit" "$target/.githooks/pre-push" "$target/.claude/hooks/confirm-main-worktree-edit.sh"
  [[ $mode == init ]] && chmod +x "$target/install.sh" "$target/scripts/"*.sh "$target/tests/"*.sh
  git -C "$target" config --local core.hooksPath .githooks
  record_runtime_path
  # This preflight already verified authentication and Projects access. On a
  # reinstall, setup can trust the persisted Project identity instead of
  # repeating the expensive remote Project drift scan.
  AGENTIC_LOOP_INSTALL=1 "$target/bin/agentic-loop" setup
  "$target/.agentic-loop/update-main.sh" install "$target"
  "$target/.agentic-loop/diagnose-codebase.sh" install "$target"
  for file in "${SHARED_FILES[@]}"; do entries+="$file"$'\t'"shared"$'\n'; done
  if [[ $mode == init ]]; then for file in "${INIT_FILES[@]}"; do entries+="$file"$'\t'"init"$'\n'; done; fi
  history=$(printf '{"at":%s,"from_revision":"none","to_revision":"%s","from_level":0,"to_level":%s,"steps":[],"result":"installed"}' "$(date +%s)" "$(foundation_json_escape "$revision")" "$migration_level")
  foundation_manifest_write "$target" "$mode" "$repository" "$revision" "$revision_ref" "$migration_level" "$entries" "$history"

  if [[ ${AGENTIC_LOOP_SKIP_START:-0} != 1 ]]; then "$target/bin/agentic-loop" start; fi
  printf 'Agentic loop installed (%s) in %s\n' "$mode" "$(cd "$target" && pwd)"
}

main "$@"
