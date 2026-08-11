#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'diagnose-codebase: %s\n' "$1" >&2; exit 1; }

repository_root() {
  git -C "$1" rev-parse --show-toplevel 2>/dev/null || fail 'target must be a Git repository'
}

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
  cat > "$unit_dir/$unit_name.service" <<EOF
[Unit]
Description=Diagnose the Agentic Loop codebase and file GitHub Issues

[Service]
Type=oneshot
ExecStart="$quoted_script" run "$quoted_root"
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
  local root state_root lock_file result_file repository
  command -v codex >/dev/null 2>&1 || fail 'codex is required'
  command -v gh >/dev/null 2>&1 || fail 'gh is required'
  command -v flock >/dev/null 2>&1 || fail 'flock is required'
  root=$(repository_root "$1")
  repository=$(cd "$root" && gh repo view --json nameWithOwner --jq .nameWithOwner) || fail 'GitHub authentication and repository access are required'
  state_root="$(git -C "$root" rev-parse --path-format=absolute --git-common-dir)/agentic-loop"
  mkdir -p "$state_root"
  lock_file="$state_root/diagnosis.lock"
  result_file="$state_root/diagnosis-last-message.txt"
  exec 9> "$lock_file"
  flock -n 9 || fail 'another diagnosis is already running'
  codex exec --sandbox read-only --config 'approval_policy="never"' -C "$root" --output-last-message "$result_file" \
    "Use \$diagnose-codebase to audit this repository now. The GitHub repository is $repository. You may use gh only to search Issues and create non-queued diagnosis Issues. Do not modify any repository file or other GitHub state."
  [[ -s $result_file ]] && cat "$result_file"
}

case ${1:-} in
  install) [[ $# == 2 ]] || fail 'usage: diagnose-codebase.sh install REPOSITORY'; install_timer "$2" ;;
  run) [[ $# == 2 ]] || fail 'usage: diagnose-codebase.sh run REPOSITORY'; run_diagnosis "$2" ;;
  *) fail 'usage: diagnose-codebase.sh {install|run} REPOSITORY' ;;
esac
