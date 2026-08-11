#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'diagnose-codebase: %s\n' "$1" >&2; exit 1; }

repository_root() {
  git -C "$1" rev-parse --show-toplevel 2>/dev/null || fail 'target must be a Git repository'
}

# Emit the merged .agentic-loop.toml (overlaid by .agentic-loop.local.toml) as
# dotted "key = value" lines, so the diagnose provider can be read without
# duplicating the queue CLI's configuration parser.
diagnose_props() {
  local root=$1
  local base="$root/.agentic-loop.toml" local_file="$root/.agentic-loop.local.toml"
  [[ -r $base ]] || return 0
  if [[ -r $local_file ]]; then
    yq -p toml -o props eval-all 'select(fi==0) * select(fi==1)' "$base" "$local_file" 2>/dev/null
  else
    yq -p toml -o props "$base" 2>/dev/null
  fi
}
diagnose_value() { awk -F ' = ' -v k="$2" '$1 == k {print $2; exit}' <<< "$1"; }

systemd_quote() {
  local value=$1
  [[ $value != *$'\n'* && $value != *$'\r'* ]] || fail 'repository paths cannot contain newlines'
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//%/%%}
  printf '%s' "$value"
}

install_timer() {
  local root unit_dir unit_name quoted_root quoted_script script_path
  command -v systemctl >/dev/null 2>&1 || fail 'systemctl is required to schedule diagnosis'
  command -v systemd-escape >/dev/null 2>&1 || fail 'systemd-escape is required to schedule diagnosis'
  root=$(repository_root "$1")
  root=$(cd "$root" && pwd -P)
  script_path="$root/.agentic-loop/diagnose-codebase.sh"
  [[ -x $script_path ]] || fail "diagnosis script is not executable: $script_path"
  unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$unit_dir"
  unit_name="agentic-loop-diagnosis-$(systemd-escape --path "$root")"
  quoted_root=$(systemd_quote "$root")
  quoted_script=$(systemd_quote "$script_path")
  local command_name service_path='' quoted_path
  for command_name in codex claude opencode; do command -v "$command_name" >/dev/null 2>&1 && service_path+="$(dirname "$(command -v "$command_name")"):"; done
  for command_name in yq gh git; do command -v "$command_name" >/dev/null 2>&1 && service_path+="$(dirname "$(command -v "$command_name")"):"; done
  service_path+="/usr/local/bin:/usr/bin:/bin"
  quoted_path=$(systemd_quote "$service_path")
  cat > "$unit_dir/$unit_name.service" <<EOF
[Unit]
Description=Diagnose the Agentic Loop codebase and file GitHub Issues

[Service]
Type=oneshot
ExecStart="$quoted_script" run "$quoted_root"
Environment=PATH=$quoted_path
NoNewPrivileges=true
EOF
  cat > "$unit_dir/$unit_name.timer" <<EOF
[Unit]
Description=Periodically diagnose the Agentic Loop codebase

[Timer]
OnBootSec=30min
OnUnitActiveSec=7d
RandomizedDelaySec=6h
Persistent=true
Unit=$unit_name.service

[Install]
WantedBy=timers.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now "$unit_name.timer"
  printf 'Periodic codebase diagnosis enabled for %s\n' "$root"
}

run_diagnosis() {
  local root state_root lock_file result_file repository props provider model effort prompt
  command -v yq >/dev/null 2>&1 || fail 'yq is required'
  command -v gh >/dev/null 2>&1 || fail 'gh is required'
  command -v flock >/dev/null 2>&1 || fail 'flock is required'
  root=$(repository_root "$1")
  # Resolve the diagnose provider from .agentic-loop.toml ([agent.diagnose],
  # falling back to [agent].provider then codex). Only Codex enforces read-only
  # through its sandbox; other providers rely on the prompt below.
  props=$(diagnose_props "$root")
  provider=$(diagnose_value "$props" 'agent.diagnose.provider')
  [[ -n $provider ]] || provider=$(diagnose_value "$props" 'agent.provider')
  provider=${provider:-codex}
  case $provider in codex | claude | opencode) ;; *) fail "unsupported diagnose provider: $provider" ;; esac
  command -v "$provider" >/dev/null 2>&1 || fail "$provider is required"
  model=$(diagnose_value "$props" 'agent.diagnose.model')
  effort=$(diagnose_value "$props" 'agent.diagnose.reasoning_effort'); effort=${effort:-low}
  repository=$(cd "$root" && gh repo view --json nameWithOwner --jq .nameWithOwner) || fail 'GitHub authentication and repository access are required'
  state_root="$(git -C "$root" rev-parse --path-format=absolute --git-common-dir)/agentic-loop"
  mkdir -p "$state_root"
  lock_file="$state_root/diagnosis.lock"
  result_file="$state_root/diagnosis-last-message.txt"
  exec 9> "$lock_file"
  flock -n 9 || fail 'another diagnosis is already running'
  prompt="Use \$diagnose-codebase to audit this repository now. The GitHub repository is $repository. You may use gh only to search Issues and create non-duplicate diagnosis Issues with diagnosis, category:improvement, and agent:queued labels, then run bin/agentic-loop sync-issue ISSUE_NUMBER for every created Issue. Do not modify any repository file or other GitHub state."
  case $provider in
    codex)
      local -a args=(exec --sandbox read-only --config 'approval_policy="never"')
      [[ -n $model ]] && args+=(--config "model=$model")
      [[ -n $effort ]] && args+=(--config "model_reasoning_effort=$effort")
      codex "${args[@]}" -C "$root" --output-last-message "$result_file" "$prompt"
      ;;
    claude)
      local -a args=(--print --dangerously-skip-permissions)
      [[ -n $model ]] && args+=(--model "$model")
      (cd "$root" && claude "${args[@]}" "$prompt") > "$result_file"
      ;;
    opencode)
      local -a args=(run --auto --dir "$root")
      [[ -n $model ]] && args+=(--model "$model")
      opencode "${args[@]}" "$prompt" > "$result_file"
      ;;
  esac
  [[ -s $result_file ]] && cat "$result_file"
}

case ${1:-} in
  install) [[ $# == 2 ]] || fail 'usage: diagnose-codebase.sh install REPOSITORY'; install_timer "$2" ;;
  run) [[ $# == 2 ]] || fail 'usage: diagnose-codebase.sh run REPOSITORY'; run_diagnosis "$2" ;;
  *) fail 'usage: diagnose-codebase.sh {install|run} REPOSITORY' ;;
esac
