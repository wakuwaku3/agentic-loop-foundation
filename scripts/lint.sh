#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

required=(AGENTS.md README.md Makefile .editorconfig .gitignore .env.example)
for file in "${required[@]}"; do
  [[ -f $file ]] || { printf 'Missing required file: %s\n' "$file" >&2; exit 1; }
done

while IFS= read -r -d '' file; do
  bash -n "$file"
done < <(find bin scripts tests -type f -name '*.sh' -print0)
bash -n bin/agentic-loop

if git grep --no-index -nE -e '-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----' -- . ':!tests/' >/dev/null; then
  printf 'Potential private key found in tracked content.\n' >&2
  exit 1
fi

printf 'Lint passed.\n'
