#!/usr/bin/env bash
set -euo pipefail

# Static, warning-only checks for contracts that shellcheck cannot know about.
# shellcheck disable=SC2016,SC2094
# Arguments are files to inspect; with none, inspect repository shell sources.
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
files=("$@")
if ((${#files[@]} == 0)); then
  while IFS= read -r -d '' file; do files+=("$file"); done < <(
    find "$root/bin" "$root/scripts" "$root/tests" "$root/.githooks" "$root/.agentic-loop" \
      -type f -name '*.sh' -print0
  )
fi

warnings=0
report() { printf 'CLI contract warning: %s:%s: %s\n' "$1" "$2" "$3"; warnings=$((warnings + 1)); }

# Keep comments and Markdown code fences out of the signal. This deliberately
# remains line-oriented: it is a lint aid, not a shell parser.
scan_file() {
  local file=$1 line_no=0 in_fence=0 line
  local help flags token body_token body_rest
  help=$(yq --help 2>/dev/null || true)
  flags=$(printf '%s\n' "$help" | sed -nE 's/^[[:space:]]+(-[A-Za-z0-9], )?--([a-z0-9-]+).*/--\2/p' | sort -u)
  while IFS= read -r line || [[ -n $line ]]; do
    line_no=$((line_no + 1))
    if [[ $line == '```'* ]]; then in_fence=$((1 - in_fence)); continue; fi
    ((in_fence == 0)) || continue
    [[ $line =~ ^[[:space:]]*# ]] && continue

    if [[ $line =~ yq[[:space:]] ]]; then
      while read -r token; do
        [[ $token == --* ]] || continue
        if ! grep -Fxq -- "$token" <<< "$flags" && [[ $token != --help ]]; then
          report "$file" "$line_no" "yq flag $token is absent from yq --help"
        fi
      done < <(grep -oE -- '--[a-zA-Z0-9-]+' <<< "$line" || true)
      [[ $line == *'if-then-else'* ]] && report "$file" "$line_no" 'jq式 if-then-else は mikefarah/yq 式ではない'
      [[ $line == *'// empty'* ]] && report "$file" "$line_no" 'jq式 // empty は mikefarah/yq 式ではない'
      [[ $line =~ yq[^\n]*\&\& ]] || [[ $line =~ yq[^\n]*\|\| ]] ||
        [[ $line == *'< <(yq '* || $line == *'$(yq '* ]] && report "$file" "$line_no" 'yq失敗が呼び出し元へ伝播しない可能性がある'
    fi
    if [[ $line =~ gh[[:space:]] ]]; then
      # Extract the value token following --body/--body=/-b (quotes
      # stripped) and only warn when it looks like an unexpanded `gh
      # --body "@path"` reference: starts with `@`, has no embedded
      # whitespace after `@`, and looks path-shaped (contains `/` or ends
      # in `.extension`). This mirrors body_unexpanded_file_reference
      # (common.sh) closely enough to avoid false positives on an ordinary
      # body such as `--body "こんにちは"`, while still catching
      # `--body=@x.md` and `-b "@x.md"` that the old bare regex missed.
      body_token=''
      if [[ $line =~ (--body=|-b[[:space:]]+|--body[[:space:]]+)([@\"\047][^[:space:]]*|[^[:space:]\"\047]+) ]]; then
        body_token=${BASH_REMATCH[2]}
      fi
      body_token=${body_token#\"}; body_token=${body_token#\'}
      if [[ $body_token == @* ]]; then
        body_rest=${body_token#@}
        if [[ -n $body_rest && $body_rest != *[[:space:]]* && ( $body_rest == */* || $body_rest =~ \.[A-Za-z0-9]+[\"\047]?$ ) ]]; then
          report "$file" "$line_no" 'gh --body の @path は展開されないため --body-file を使用する'
        fi
      fi
      if [[ $line =~ gh[[:space:]]+api && $line =~ -f[[:space:]][^[:space:]]+=(-?[0-9]+|true|false)([[:space:]]|$) ]]; then
        report "$file" "$line_no" 'gh api の型付き値に -f ではなく -F を使用する'
      fi
    fi
  done < "$file"
}

for file in "${files[@]}"; do scan_file "$file"; done
printf 'CLI contract lint: %d warning(s) (warning-only rollout)\n' "$warnings" >&2
exit 0
