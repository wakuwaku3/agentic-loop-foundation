#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

while IFS= read -r -d '' file; do
  sed -i 's/[[:space:]]\+$//' "$file"
  [[ -s $file ]] && [[ $(tail -c 1 "$file" | wc -l) -eq 1 ]] || printf '\n' >> "$file"
done < <(git ls-files -z -- '*.md' '*.sh' Makefile .editorconfig .gitignore .env.example .agentic-loop/config .githooks/pre-commit .githooks/pre-push)

printf 'Formatting complete.\n'
