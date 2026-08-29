# Module: foundation-state.sh. Sourced by bin/agentic-loop.
# shellcheck shell=bash
# Foundation installation state lives beside the common Git directory, never
# in the repository worktree.  The file is deliberately JSON so shell clients
# and yq can inspect it without introducing another runtime dependency.
foundation_state_path() { printf '%s/foundation-state.json\n' "$STATE_ROOT"; }

foundation_state_write() {
  local target=$1 mode=$2 repository=$3 revision=$4 revision_ref=$5 level=$6 entries=$7 history=$8
  local out tmp path class hash sep=''
  out=$(foundation_state_path); mkdir -p "$(dirname "$out")"
  tmp="$out.tmp.$$"
  {
    printf '{"schema_version":1,"source":{"repository":"%s","revision":"%s","revision_ref":"%s"},"installed_at":%s,"mode":"%s","migration_level":%s,"files":[' \
      "$(foundation_json_escape "$repository")" "$(foundation_json_escape "$revision")" "$(foundation_json_escape "$revision_ref")" \
      "$(date +%s)" "$(foundation_json_escape "$mode")" "$level"
    while IFS=$'\t' read -r path class hash; do
      [[ -n $path ]] || continue
      [[ -n $hash ]] || hash=$(foundation_sha256 "$target/$path")
      printf '%s{"path":"%s","class":"%s","sha256":"%s"}' "$sep" "$(foundation_json_escape "$path")" "$class" "$hash"; sep=,
    done <<< "$entries"
    printf '],"history":[%s]}\n' "$history"
  } > "$tmp"
  mv -f -- "$tmp" "$out"
}

foundation_state_or_legacy() {
  local legacy=${1:-$REPO_ROOT/.agentic-loop/manifest.json} state
  state=$(foundation_state_path)
  if [[ -r $state ]]; then printf '%s\n' "$state"; elif [[ -r $legacy ]]; then printf '%s\n' "$legacy"; fi
}
