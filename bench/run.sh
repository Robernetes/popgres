#!/usr/bin/env bash
#
# Runs the popgres benchmarks against PocketBase v0.40.6 (built from the
# upstream tag of the repository history) in Docker (bench/docker-compose.yml)
# and writes the JSON results to bench/results/<date>.jsonl.
#
# Requires go, git and docker.
#
#   ./bench/run.sh
#
# Options (env variables): BENCH_DURATION (20s), BENCH_CONCURRENCY (50),
# BENCH_RECORDS (10000), BENCH_CLIENTS (500), BENCH_EVENTS (200), BENCH_RATE (20).

set -euo pipefail

cd "$(dirname "$0")"

DURATION="${BENCH_DURATION:-20s}"
CONCURRENCY="${BENCH_CONCURRENCY:-50}"
RECORDS="${BENCH_RECORDS:-10000}"
CLIENTS="${BENCH_CLIENTS:-500}"
EVENTS="${BENCH_EVENTS:-200}"
RATE="${BENCH_RATE:-20}"
EMAIL="bench@example.com"
PASSWORD="bench-password-123"

mkdir -p .bin results
RESULTS="results/$(date +%Y%m%d-%H%M%S).jsonl"

compose() {
  docker compose -f docker-compose.yml "$@"
}

cleanup() {
  compose down -v >/dev/null 2>&1 || true
  git -C .. worktree remove --force "$PWD/.bin/pocketbase-src" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== building the binaries"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64

(cd .. && go build -trimpath -ldflags "-s -w" -o bench/.bin/popgres ./examples/base)
(cd .. && go build -trimpath -ldflags "-s -w" -o bench/.bin/bench ./bench)

# PocketBase v0.40.6 from the upstream tag
rm -rf .bin/pocketbase-src
git -C .. worktree add --detach "$PWD/.bin/pocketbase-src" v0.40.6 >/dev/null
(cd .bin/pocketbase-src && go build -trimpath -ldflags "-s -w" -o ../pocketbase ./examples/base)
git -C .. worktree remove --force "$PWD/.bin/pocketbase-src"

unset CGO_ENABLED GOOS GOARCH

echo "== starting the services"
compose down -v >/dev/null 2>&1 || true
compose up -d postgres
for _ in $(seq 1 60); do
  compose exec -T postgres pg_isready -U postgres -q && break
  sleep 1
done
compose exec -T postgres psql -U postgres -qc "CREATE DATABASE single" -c "CREATE DATABASE cluster"

# the superusers (the popgres command also applies the migrations)
compose run --rm -T pocketbase /bench/pocketbase superuser upsert "$EMAIL" "$PASSWORD" --dir=/data >/dev/null
compose run --rm -T popgres /bench/popgres superuser upsert "$EMAIL" "$PASSWORD" --dir=/data --dev=false >/dev/null
compose run --rm -T node1 /bench/popgres superuser upsert "$EMAIL" "$PASSWORD" --dir=/data --dev=false >/dev/null

compose up -d pocketbase popgres node1 node2 node3 nginx runner

wait_http() {
  for _ in $(seq 1 60); do
    if compose exec -T runner sh -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "timeout waiting for $1:$2" && return 1
}
for host in pocketbase popgres node1 node2 node3; do wait_http "$host" 8090; done
wait_http nginx 80

bench() {
  compose exec -T runner /bench/bench "$@"
}

run_target() {
  local name="$1" url="$2" realtime_urls="$3"

  echo "== $name: setup"
  bench -scenario setup -url "$url" -email "$EMAIL" -password "$PASSWORD" -records "$RECORDS" -c "$CONCURRENCY"

  # let the replication leader catch up
  sleep 5

  echo "== $name: reads"
  bench -target "$name" -scenario reads -url "$url" -c "$CONCURRENCY" -d "$DURATION" | tee -a "$RESULTS"

  echo "== $name: writes"
  bench -target "$name" -scenario writes -url "$url" -c "$CONCURRENCY" -d "$DURATION" | tee -a "$RESULTS"

  echo "== $name: realtime"
  bench -target "$name" -scenario realtime -url "$realtime_urls" -writeUrl "$url" \
    -clients "$CLIENTS" -events "$EVENTS" -rate "$RATE" | tee -a "$RESULTS"
}

run_target "pocketbase" "http://pocketbase:8090" "http://pocketbase:8090"
run_target "popgres" "http://popgres:8090" "http://popgres:8090"
run_target "popgres-3-nodes" "http://nginx" "http://node1:8090,http://node2:8090,http://node3:8090"

echo "== results: bench/$RESULTS"
