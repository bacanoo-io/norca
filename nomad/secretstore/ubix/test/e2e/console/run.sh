#!/usr/bin/env bash
# Console end-to-end test: builds the server, starts a fresh uninitialized one on
# a free loopback port with a throwaway data dir, and drives /ui/ in headless
# Chrome (console-e2e.mjs): initialize, unseal share by share, sign in with a
# token and with userpass, sign out (checking revocation on the server).
# Screenshots land in $OUT (default: a temp dir, printed at the end).
set -euo pipefail
cd "$(dirname "$0")/../../.."

WORK="$(mktemp -d)"
OUT="${OUT:-$WORK/screenshots}"
mkdir -p "$OUT"
PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"

go build -o "$WORK/ubixvault" ./cmd/ubixvault
"$WORK/ubixvault" server -listen "127.0.0.1:$PORT" -data "$WORK/data" >"$WORK/server.log" 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true' EXIT

for _ in $(seq 1 50); do
  curl -s -o /dev/null "http://127.0.0.1:$PORT/v1/sys/seal-status" && break
  sleep 0.1
done

node test/e2e/console/console-e2e.mjs "http://127.0.0.1:$PORT" "$OUT"
echo "screenshots: $OUT"
