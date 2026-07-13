#!/usr/bin/env bash
set -euo pipefail

: "${AUTOSQL_TEST_POSTGRES_URL:?set AUTOSQL_TEST_POSTGRES_URL to a disposable PostgreSQL database}"
count="${AUTOSQL_LIVE_COUNT:-1}"
if [ "$#" -eq 0 ]; then
  set -- ./...
fi

# Catalog integration packages create/drop databases, schemas, roles, and
# indexes. Package-parallel catalog DDL is intentionally unsupported.
go test -race -p 1 -count="$count" "$@"
