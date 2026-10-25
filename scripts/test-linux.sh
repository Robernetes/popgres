#!/usr/bin/env bash
#
# Runs "go test" in a Linux container (like the CI) against the Postgres
# server of POPGRES_TEST_URL, reached from the container through host.docker.internal.
#
# The arguments are passed to "go test" (default: ./...).
#
#   POPGRES_TEST_URL=postgres://user:pass@localhost:5433/popgres_test?sslmode=disable \
#     ./scripts/test-linux.sh -count=1 ./tests/e2e/...

set -euo pipefail

cd "$(dirname "$0")/.."

: "${POPGRES_TEST_URL:?set POPGRES_TEST_URL (eg. postgres://user:pass@localhost:5433/popgres_test?sslmode=disable)}"

IMAGE="popgres-test-runner:local"

export MSYS_NO_PATHCONV=1

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  docker build -t "$IMAGE" scripts/test-runner
fi

# localhost inside the container is the container itself
CONTAINER_URL="${POPGRES_TEST_URL//localhost/host.docker.internal}"
CONTAINER_URL="${CONTAINER_URL//127.0.0.1/host.docker.internal}"

SRC="$(pwd)"
if command -v cygpath >/dev/null 2>&1; then
  SRC="$(cygpath -w "$SRC")"
fi

ARGS=("$@")
if [ ${#ARGS[@]} -eq 0 ]; then
  ARGS=(./...)
fi

# the tests run on a copy because some of them write in the working directory
docker run --rm \
  --add-host host.docker.internal:host-gateway \
  -v "$SRC:/src:ro" \
  -v popgres_gomod:/go/pkg/mod \
  -v popgres_gocache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  -e POPGRES_TEST_URL="$CONTAINER_URL" \
  -e POPGRES_RLS_STRICT="${POPGRES_RLS_STRICT:-}" \
  "$IMAGE" \
  sh -c 'mkdir /work && cd /src && tar --exclude=./.git -cf - . | tar -C /work -xf - && cd /work && go test "$@"' -- "${ARGS[@]}"
