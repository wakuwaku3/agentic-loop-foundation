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

dirty_beyond_manifest() {
  git -C "$1" status --porcelain
}

sync_main() {
  local repository=$1 lock_file
  command -v flock >/dev/null 2>&1 || fail 'flock is required'
  [[ $(git -C "$repository" branch --show-current) == main ]] || fail 'refusing to update a worktree not on main'
  lock_file="$(git -C "$repository" rev-parse --absolute-git-dir)/agentic-loop-main-sync.lock"
  mkdir -p "$(dirname "$lock_file")"
  exec 9> "$lock_file"
  flock -n 9 || fail 'another main update is already running'
  [[ -z $(dirty_beyond_manifest "$repository") ]] || fail 'refusing to update a dirty main worktree'
  git -C "$repository" fetch --quiet origin main
  [[ -z $(dirty_beyond_manifest "$repository") ]] || fail 'main worktree changed while fetching'
  git -C "$repository" merge-base --is-ancestor HEAD refs/remotes/origin/main ||
    fail 'refusing to update a main branch that is ahead of or diverged from origin/main'
  git -C "$repository" merge --quiet --ff-only refs/remotes/origin/main
  auto_upgrade "$repository"
}

# One bounded observation per timer cycle.  A missing/invalid configuration or
# an unavailable source is a safe no-op; the next cycle observes a new SHA.
auto_upgrade() {
  local repository=$1 source_repo revision tmp common state candidate installed was_running result
  [[ ${AGENTIC_LOOP_AUTO_UPDATE:-1} == 1 ]] || return 0
  command -v yq >/dev/null 2>&1 || return 0
  source_repo=$(yq -p toml -o yaml '.foundation.repository // "wakuwaku3/agentic-loop-foundation"' "$repository/.agentic-loop.toml" 2>/dev/null || true)
  [[ $source_repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || return 0
  common=$(git -C "$repository" rev-parse --path-format=absolute --git-common-dir) || return 0
  state="$common/agentic-loop/foundation-state.json"
  candidate="$common/agentic-loop/auto-update.json"
  mkdir -p "$(dirname "$candidate")"
  exec 8>"$common/agentic-loop/auto-update.lock"
  flock -n 8 || return 0
  if [[ -e $state ]] && ! yq -p json -e '.schema_version == 1 and (.files | type == "!!seq")' "$state" >/dev/null 2>&1; then
    printf '{"candidate":null,"result":"state-invalid","recovery":"bin/agentic-loop doctor"}\n' > "$candidate.tmp.$$"
    mv -f "$candidate.tmp.$$" "$candidate"
    return 0
  fi
  tmp=$(mktemp -d)
  if ! git init -q "$tmp" || ! git -C "$tmp" fetch -q --depth 1 "https://github.com/$source_repo" main; then
    result='fetch-failed'; printf '{"candidate":null,"result":"%s"}\n' "$result" > "$candidate.tmp.$$"; mv -f "$candidate.tmp.$$" "$candidate"; rm -rf "$tmp"; return 0
  fi
  revision=$(git -C "$tmp" rev-parse FETCH_HEAD)
  installed=$(yq -p json -o yaml '.source.revision // ""' "$state" 2>/dev/null || true)
  printf '{"candidate":"%s","result":"observed"}\n' "$revision" > "$candidate.tmp.$$"; mv -f "$candidate.tmp.$$" "$candidate"
  [[ $revision != "$installed" ]] || { rm -rf "$tmp"; return 0; }
  [[ -x $repository/bin/agentic-loop ]] || { rm -rf "$tmp"; return 0; }
  was_running=0
  if [[ -r $common/agentic-loop/supervisor.pid ]] && kill -0 "$(cat "$common/agentic-loop/supervisor.pid" 2>/dev/null)" 2>/dev/null; then was_running=1; "$repository/bin/agentic-loop" stop >/dev/null 2>&1 || true; fi
  if "$repository/bin/agentic-loop" upgrade --source "$tmp" --revision "$revision" --apply >/dev/null 2>&1; then
    result='applied'
    (( was_running )) && "$repository/bin/agentic-loop" start >/dev/null 2>&1 || true
  else
    result='blocked-or-failed'
    # A failed upgrade must never restart a potentially inconsistent Supervisor.
  fi
  printf '{"candidate":"%s","result":"%s"}\n' "$revision" "$result" > "$candidate.tmp.$$"; mv -f "$candidate.tmp.$$" "$candidate"
  rm -rf "$tmp"
}

case ${1:-} in
  install) [[ $# == 2 ]] || fail 'usage: update-main.sh install REPOSITORY'; install_timer "$2" ;;
  sync) [[ $# == 2 ]] || fail 'usage: update-main.sh sync REPOSITORY'; sync_main "$2" ;;
  *) fail 'usage: update-main.sh {install|sync} REPOSITORY' ;;
esac
