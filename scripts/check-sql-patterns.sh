#!/usr/bin/env bash
#
# Fails if the Go sources (excluding the tests and the SQLite importer) contain
# SQLite only SQL: json_each, sqlite_master/sqlite_schema, _rowid_, PRAGMA,
# VACUUM INTO and strftime calls in SQL strings.
#
#   ./scripts/check-sql-patterns.sh

set -uo pipefail

cd "$(dirname "$0")/.."

# the SQLite importer is the only place allowed to work with SQLite SQL
EXCLUDES=(
  ':!*_test.go'
  ':!cmd/popgres-import/**'
)

status=0

check() {
  local description="$1"
  local pattern="$2"
  shift 2

  local found
  found=$(git grep -nE "$pattern" -- '*.go' "${EXCLUDES[@]}" "$@" || true)
  if [ -n "$found" ]; then
    echo "SQLite only SQL ($description):"
    echo "$found"
    echo
    status=1
  fi
}

check "json_each" 'json_each\('
check "sqlite_master/sqlite_schema" 'sqlite_(master|schema)'
check "_rowid_" '_rowid_'
check "PRAGMA" '"[^"]*PRAGMA |`[^`]*PRAGMA '
check "VACUUM INTO" 'VACUUM INTO'
# the filter function of the API is also named strftime (tools/search/token_functions.go)
check "strftime SQL call" "strftime\\('%" ':!tools/search/token_functions.go'

if [ "$status" -eq 0 ]; then
  echo "no SQLite only SQL found"
fi

exit "$status"
