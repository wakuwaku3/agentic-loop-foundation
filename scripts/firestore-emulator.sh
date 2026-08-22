#!/usr/bin/env bash
set -euo pipefail

if [[ "${APP_ENV:-development}" == "production" ]]; then
  echo "Firestore emulator is forbidden in production" >&2
  exit 1
fi
if [[ -n "${FIRESTORE_EMULATOR_HOST:-}" ]]; then
  echo "refusing to overwrite an existing FIRESTORE_EMULATOR_HOST" >&2
  exit 1
fi

project="${FIREBASE_PROJECT_ID:-agentic-loop-test}"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/agentic-firestore.XXXXXX")"
config="$tmp/firebase.json"
log="$tmp/firebase.log"
cleanup() {
  if [[ -n "${emulator_pid:-}" ]]; then
    kill -- "-$emulator_pid" 2>/dev/null || kill "$emulator_pid" 2>/dev/null || true
    wait "$emulator_pid" 2>/dev/null || true
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT INT TERM

port=""
for candidate in $(shuf -i 18080-18999 -n 32); do
  if ! (exec 9<>"/dev/tcp/127.0.0.1/$candidate") 2>/dev/null; then port="$candidate"; break; fi
done
if [[ -z "$port" ]]; then echo "no free Firestore emulator port" >&2; exit 1; fi
jq --argjson port "$port" '.emulators.firestore.port=$port' firebase.json >"$config"

setsid firebase emulators:start --only firestore --project "$project" --config "$config" >"$log" 2>&1 &
emulator_pid=$!
for _ in $(seq 1 60); do
  if curl -sS "http://127.0.0.1:$port" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$emulator_pid" 2>/dev/null; then cat "$log" >&2; exit 1; fi
  sleep 1
done
if ! curl -sS "http://127.0.0.1:$port" >/dev/null 2>&1; then cat "$log" >&2; exit 1; fi

export FIRESTORE_EMULATOR_HOST="127.0.0.1:$port"
if [[ "$#" -gt 0 ]]; then
  "$@"
  exit $?
fi
wait "$emulator_pid"
