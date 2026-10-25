#!/usr/bin/env bash
#
# Runs the whole test suite against POPGRES_TEST_URL and checks the parity with
# the PocketBase v0.40.6 tests (tests/cmd/parity).
#
# The go test -json output is saved in test-output.jsonl.
#
#   POPGRES_TEST_URL=postgres://... ./scripts/ci-test.sh

set -uo pipefail

cd "$(dirname "$0")/.."

OUT="test-output.jsonl"

go test -count=1 -p 4 -timeout 20m -json ./... > "$OUT"
test_status=$?

# print the failed tests and their messages
if [ "$test_status" -ne 0 ]; then
  echo "failed tests:"
  jq -r 'select(.Action == "fail" and .Test != null) | "  \(.Package) \(.Test)"' "$OUT"
  echo
  jq -r 'select(.Action == "output" and .Test != null) | .Output' "$OUT" |
    grep -E -- '--- FAIL|_test\.go:[0-9]+:|golden\.go:[0-9]+:|api\.go:[0-9]+:|panic:' || true
  # the build failures and package level errors
  jq -r 'select(.Action == "output" and .Test == null) | .Output' "$OUT" |
    grep -vE '^(ok|PASS|FAIL|coverage:)|^\s*$|^\[' || true
fi

go run ./tests/cmd/parity -in "$OUT"
parity_status=$?

if [ "$test_status" -ne 0 ] || [ "$parity_status" -ne 0 ]; then
  exit 1
fi
