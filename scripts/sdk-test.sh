#!/usr/bin/env bash
#
# Runs the official PocketBase JS SDK compatibility tests (tests/sdk/js)
# against a popgres server started from the current sources.
#
# Requires go and node (>= 22). The target database must be empty or
# a popgres database (the tests create and delete their own collection):
#
#   DATABASE_URL=postgres://user:pass@localhost:5432/mydb ./scripts/sdk-test.sh

set -euo pipefail

cd "$(dirname "$0")/.."

: "${DATABASE_URL:?set DATABASE_URL (the database of the test server)}"

PORT="${SDK_TEST_PORT:-8095}"
WORK="$(mktemp -d)"
BIN="$WORK/popgres"
DATA="$WORK/popgres_data"
EMAIL="sdk_$(date +%s)@example.com"
PASSWORD="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"

cleanup() {
  if [ -n "${SERVER_PID:-}" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

go build -o "$BIN" ./examples/base

"$BIN" superuser upsert "$EMAIL" "$PASSWORD" --dir "$DATA" --dev=false >/dev/null

"$BIN" serve --http "127.0.0.1:$PORT" --dir "$DATA" --dev=false > "$WORK/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 60); do
  if curl -fs "http://127.0.0.1:$PORT/api/health" >/dev/null; then
    break
  fi
  sleep 1
done
curl -fs "http://127.0.0.1:$PORT/api/health" >/dev/null || { cat "$WORK/server.log"; exit 1; }

npm --prefix tests/sdk/js ci --no-audit --no-fund >/dev/null

status=0
POPGRES_URL="http://127.0.0.1:$PORT" POPGRES_EMAIL="$EMAIL" POPGRES_PASSWORD="$PASSWORD" \
  npm --prefix tests/sdk/js test || status=$?

# delete the test superuser
"$BIN" superuser delete "$EMAIL" --dir "$DATA" --dev=false >/dev/null 2>&1 || true

if [ "$status" -ne 0 ]; then
  echo "server log:"
  tail -50 "$WORK/server.log"
fi

exit "$status"
