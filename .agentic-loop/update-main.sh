#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'update-main: %s\n' "$1" >&2; exit 1; }

main_worktree() {
  local repository=$1 path='' branch=''
  while IFS= read -r line || [[ -n $line ]]; do
    case $line in
      'worktree '*) path=${line#worktree } ;;
      'branch '*) branch=${line#branch } ;;
      '')
        if [[ $branch == refs/heads/main ]]; then printf '%s\n' "$path"; return; fi
        path=''; branch=''
        ;;
    esac
  done < <(git -C "$repository" worktree list --porcelain; printf '\n')
  fail 'a worktree on branch main is required'
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
  local repository=$1 main_root unit_dir script_path quoted_root quoted_script unit_name
  command -v systemctl >/dev/null 2>&1 || fail 'systemctl is required to schedule main updates'
  command -v systemd-escape >/dev/null 2>&1 || fail 'systemd-escape is required to schedule main updates'
  main_root=$(main_worktree "$repository")
  main_root=$(cd "$main_root" && pwd -P)
  script_path="$main_root/.agentic-loop/update-main.sh"
  [[ -x $script_path ]] || fail "updater is not executable: $script_path"
  unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$unit_dir"
  quoted_root=$(systemd_quote "$main_root")
  quoted_script=$(systemd_quote "$script_path")
  unit_name="agentic-loop-main-sync-$(systemd-escape --path "$main_root")"
  cat > "$unit_dir/$unit_name.service" <<EOF
[Unit]
Description=Fast-forward the Agentic Loop main worktree

[Service]
Type=oneshot
ExecStart="$quoted_script" sync "$quoted_root"
NoNewPrivileges=true
EOF
  cat > "$unit_dir/$unit_name.timer" <<EOF
[Unit]
Description=Periodically update the Agentic Loop main worktree

[Timer]
OnBootSec=5min
OnUnitActiveSec=15min
RandomizedDelaySec=2min
Persistent=true
Unit=$unit_name.service

[Install]
WantedBy=timers.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now "$unit_name.timer"
  printf 'Periodic main updates enabled for %s\n' "$main_root"
}

sync_main() {
  local repository=$1 lock_file
  command -v flock >/dev/null 2>&1 || fail 'flock is required'
  [[ $(git -C "$repository" branch --show-current) == main ]] || fail 'refusing to update a worktree not on main'
  lock_file="$(git -C "$repository" rev-parse --absolute-git-dir)/agentic-loop-main-sync.lock"
  mkdir -p "$(dirname "$lock_file")"
  exec 9> "$lock_file"
  flock -n 9 || fail 'another main update is already running'
  [[ -z $(git -C "$repository" status --porcelain) ]] || fail 'refusing to update a dirty main worktree'
  git -C "$repository" fetch --quiet origin main
  [[ -z $(git -C "$repository" status --porcelain) ]] || fail 'main worktree changed while fetching'
  git -C "$repository" merge-base --is-ancestor HEAD refs/remotes/origin/main ||
    fail 'refusing to update a main branch that is ahead of or diverged from origin/main'
  git -C "$repository" merge --quiet --ff-only refs/remotes/origin/main
}

case ${1:-} in
  install) [[ $# == 2 ]] || fail 'usage: update-main.sh install REPOSITORY'; install_timer "$2" ;;
  sync) [[ $# == 2 ]] || fail 'usage: update-main.sh sync REPOSITORY'; sync_main "$2" ;;
  *) fail 'usage: update-main.sh {install|sync} REPOSITORY' ;;
esac
