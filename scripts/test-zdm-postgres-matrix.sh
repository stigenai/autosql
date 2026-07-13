#!/usr/bin/env bash
set -euo pipefail

versions="${AUTOSQL_POSTGRES_VERSIONS:-14 15 16 17 18}"
count="${AUTOSQL_LIVE_COUNT:-1}"
report_dir="${AUTOSQL_MATRIX_REPORT_DIR:-artifacts/zdm-matrix}"
mkdir -p "$report_dir"

cleanup() {
  if [ -n "${container:-}" ]; then docker rm -f "$container" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT INT TERM

for version in $versions; do
  container="autosql-zdm-pg${version}-$$"
  docker run -d --name "$container" -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=autosql -p 127.0.0.1::5432 "postgres:${version}" >/dev/null
	ready=0
  for _ in $(seq 1 60); do
    if docker exec "$container" psql -U postgres -d autosql -Atqc 'select 1' >/dev/null 2>&1; then
	  sleep 1
	  if docker exec "$container" psql -U postgres -d autosql -Atqc 'select 1' >/dev/null 2>&1; then ready=1; break; fi
	fi
    sleep 1
  done
	if [ "$ready" -ne 1 ]; then echo "PostgreSQL $version did not become ready" >&2; exit 1; fi
  port="$(docker port "$container" 5432/tcp | awk -F: 'NR==1{print $NF}')"
  export AUTOSQL_TEST_POSTGRES_URL="postgres://postgres:postgres@127.0.0.1:${port}/autosql?sslmode=disable"
  {
    echo "postgres_version=$version"
    scripts/test-live-serial.sh ./pkg/zdm/... ./internal/cli
    go test -run '^$' -bench 'BenchmarkZDM' -benchtime=1s -count=1 ./pkg/zdm/compatmatrix
  } 2>&1 | tee "$report_dir/postgres-${version}.txt"
  docker rm -f "$container" >/dev/null
  container=""
done
