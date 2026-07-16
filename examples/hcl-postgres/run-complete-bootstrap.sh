#!/usr/bin/env bash
set -euo pipefail

MAINTENANCE_URL="${AUTOSQL_TEST_POSTGRES_URL:?Set AUTOSQL_TEST_POSTGRES_URL to a disposable PostgreSQL maintenance database}"
export AUTOSQL_TEST_POSTGRES_URL="$MAINTENANCE_URL"

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
WORKDIR="${AUTOSQL_EXAMPLE_WORKDIR:-$(mktemp -d)}"
KEEP_WORKDIR="${AUTOSQL_KEEP_WORKDIR:-0}"
cleanup() {
  if [[ "$KEEP_WORKDIR" != 1 ]]; then
    rm -rf "$WORKDIR"
  else
    printf 'Complete HCL artifact kept in %s\n' "$WORKDIR"
  fi
}
trap cleanup EXIT

export AUTOSQL_COMPLETE_HCL_OUTPUT="$WORKDIR/complete-bootstrap.hcl"
cd "$ROOT"
go test ./pkg/postgres -run '^TestCanonicalCompleteBootstrapInventoryManifest$' -count=1 -v
printf 'Verified complete HCL bootstrap: %s\n' "$AUTOSQL_COMPLETE_HCL_OUTPUT"
