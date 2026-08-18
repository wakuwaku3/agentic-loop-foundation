# Module: priority.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155


# jq expression fragment producing the numeric priority (0-100, 0 when unset)
# from an Issue object's body marker `<!-- agentic-loop:priority N -->`. Valid
# markers are the maximum of every N in range; out-of-range, non-numeric and
# malformed markers are ignored. The same regex is mirrored by
# body_priority_value() for the shell side, so the two readers cannot drift.
queue_priority_jq() {
  printf '%s' '([.body // "" | match("agentic-loop:priority[ \t]+(?<n>[0-9]+)([ \t]|--)"; "g") | .captures[0].string | tonumber | select(. >= 0 and . <= 100)] | max // 0)'
}


# Shell counterpart of queue_priority_jq over a raw body string. Reads only the
# numeric marker values; no other body content is ever extracted or stored.
body_priority_value() {
  local body=$1 max=0 tok n
  while IFS= read -r tok; do
    [[ -n $tok ]] || continue
    [[ $tok =~ 'agentic-loop:priority'[[:space:]]+([0-9]+)([[:space:]]|--) ]] || continue
    n=${BASH_REMATCH[1]}
    (( n >= 0 && n <= 100 && n > max )) && max=$n
  done < <(grep -oE 'agentic-loop:priority[[:space:]]+[0-9]+([[:space:]]|--)' <<< "$body" || true)
  printf '%s\n' "$max"
}


# `priority ISSUE N`: set (or update) an Issue's numeric priority by upserting
# the single body marker line and dropping any legacy priority:* label. Same
# authorization as dispose/resume: an authenticated repository write, maintain,
# or admin operator. REST(core) only; the audit comment records the operation.
cmd_priority() {
  local issue=${1:-} value=${2:-} actor body_b64 body new_body labels kept now
  [[ $issue =~ ^[1-9][0-9]*$ ]] || fail 'priority requires a positive Issue number'
  [[ $value =~ ^[0-9]+$ ]] || fail 'priority value must be an integer between 0 and 100'
  (( 10#$value <= 100 )) || fail 'priority value must be an integer between 0 and 100'
  actor=$(authorized_operator) || fail 'priority requires authenticated repository write, maintain, or admin permission'
  IFS=$'\t' read -r body_b64 labels < <(repo_api "issues/$issue" --jq '[(.body // "" | @base64), ([.labels[].name] | join(","))] | @tsv') || fail "cannot read Issue #$issue"
  body=$(base64 -d <<< "$body_b64" 2>/dev/null || true)
  # Upsert only the marker line: delete a whole-line marker, strip an inline
  # one, then append the fresh marker. The rest of the body is preserved.
  new_body=$(sed -E \
    -e '/^[[:space:]]*<!--[[:space:]]*agentic-loop:priority[[:space:]]+[0-9]+[[:space:]]*-->[[:space:]]*$/d' \
    -e 's/<!--[[:space:]]*agentic-loop:priority[[:space:]]+[0-9]+[[:space:]]*-->//g' \
    <<< "$body")
  new_body=$(printf '%s' "$new_body" | sed -e :a -e '/^\n*$/{$d;N;ba}')
  printf -v new_body '%s\n\n<!-- agentic-loop:priority %s -->\n' "$new_body" "$value"
  repo_api "issues/$issue" --method PATCH -f body="$new_body" >/dev/null || fail 'could not update Issue body'
  # Migration-era sweep: if a legacy priority:* label still lingers, remove it.
  if grep -qE '(^|,)priority:(critical|high|medium|low)(,|$)' <<< ",$labels,"; then
    kept=$(repo_api "issues/$issue" --jq '[.labels[].name | select(startswith("priority:") | not)]') || fail "cannot read Issue #$issue labels"
    printf '%s\n' "$kept" | repo_api "issues/$issue/labels" --method PUT --input - >/dev/null
  fi
  now=$(date +%s)
  comment_issue "$issue" "<!-- agentic-loop:priority-set schema=1 actor=$actor issue=$issue value=$value at=$now -->\nIssue の priority を $value（0-100）に設定し、本文marker \`agentic-loop:priority $value\` を更新しました。旧 \`priority:*\` labelは削除しました。"
  say "Issue #$issue の priority を $value に設定しました。"
}
