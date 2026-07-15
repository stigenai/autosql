#!/usr/bin/env bash
set -euo pipefail

# Run from the repository root or set AUTOSQL_BIN explicitly.
DATABASE_URL="${AUTOSQL_DATABASE_URL:?Set AUTOSQL_DATABASE_URL to a disposable PostgreSQL database}"
export AUTOSQL_DATABASE_URL="$DATABASE_URL"

if [[ -n "${AUTOSQL_BIN:-}" ]]; then
  AUTOSQL=("$AUTOSQL_BIN")
elif [[ -x "$(pwd)/autosql" ]]; then
  AUTOSQL=("$(pwd)/autosql")
else
  AUTOSQL=(go run ./cmd/autosql)
fi

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
WORKDIR="${AUTOSQL_EXAMPLE_WORKDIR:-$(mktemp -d)}"
KEEP_WORKDIR="${AUTOSQL_KEEP_WORKDIR:-0}"
cleanup() {
  if [[ "$KEEP_WORKDIR" != 1 ]]; then
    rm -rf "$WORKDIR"
  else
    printf 'Artifacts kept in %s\n' "$WORKDIR"
  fi
}
trap cleanup EXIT

printf '== 1. Load desired SQL into the canonical schema graph ==\n'
"${AUTOSQL[@]}" schema load --source "sql:$ROOT/schema-v1.sql" --json >"$WORKDIR/load-v1.json"

printf '== 2. Inspect the live PostgreSQL schema (read-only) ==\n'
"${AUTOSQL[@]}" schema inspect --url env://AUTOSQL_DATABASE_URL --schema app --format json --json >"$WORKDIR/inspect-before.json"

printf '== 3. Diff and plan v1 -> v2 ==\n'
"${AUTOSQL[@]}" schema diff --from "sql:$ROOT/schema-v1.sql" --to "sql:$ROOT/schema-v2.sql" --json >"$WORKDIR/diff.json"
"${AUTOSQL[@]}" plan --from "sql:$ROOT/schema-v1.sql" --to "sql:$ROOT/schema-v2.sql" --json >"$WORKDIR/plan.json"

printf '== 4. Apply the approved SQL change through PostgreSQL ==\n'
psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --file "$ROOT/apply-v1.sql" >/dev/null
psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --file "$ROOT/seed.sql" >/dev/null

printf '== 5. Verify the live database with a second inspection ==\n'
"${AUTOSQL[@]}" schema inspect --url env://AUTOSQL_DATABASE_URL --schema app --format json --json >"$WORKDIR/inspect-after.json"

printf '== 6. Apply the planned v2 upgrade and verify convergence ==\n'
psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --file "$ROOT/apply-v2.sql" >/dev/null
"${AUTOSQL[@]}" schema load --source "sql:$ROOT/schema-v2.sql" --json >"$WORKDIR/load-v2.json"
"${AUTOSQL[@]}" schema inspect --url env://AUTOSQL_DATABASE_URL --schema app --format json --json >"$WORKDIR/inspect-v2.json"

printf '== 7. Initialize and inspect zero-downtime metadata ==\n'
"${AUTOSQL[@]}" migrate metadata-init --url env://AUTOSQL_DATABASE_URL --metadata-schema autosql_zdm --json >"$WORKDIR/metadata-init.json"
"${AUTOSQL[@]}" migrate metadata-status --url env://AUTOSQL_DATABASE_URL --metadata-schema autosql_zdm --json >"$WORKDIR/metadata-status.json"

python3 - "$WORKDIR" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
def read(name):
    value = json.loads((root / name).read_text())
    if not value.get("ok"):
        raise SystemExit(f"{name}: command failed: {value}")
    return value
load = read("load-v1.json")
load_v2 = read("load-v2.json")
diff = read("diff.json")
plan = read("plan.json")
inspect = read("inspect-after.json")
inspect_v2 = read("inspect-v2.json")
metadata = read("metadata-status.json")
changes = diff["data"]["changes"]["changes"]
steps = plan["data"]["plan"]["steps"]
resources = inspect["data"]["graph"]["resources"]
assert len(changes) >= 2, changes
assert len(steps) >= 2, steps
assert any(r["kind"] == "table" and r["name"]["name"] == "customers" for r in resources)
desired_resources = load_v2["data"]["graph"]["resources"]
live_resources = inspect_v2["data"]["graph"]["resources"]
def stable_identity(resource):
    name = resource["name"]
    return (resource["kind"], name.get("schema"), name.get("parent"), name["name"])
stable_kinds = {"schema", "table", "column"}
assert {stable_identity(r) for r in desired_resources if r["kind"] in stable_kinds} == {
    stable_identity(r) for r in live_resources if r["kind"] in stable_kinds
}
assert metadata["data"]["initialized"] is True
print(f"verified: loaded={load['ok']} changes={len(changes)} plan_steps={len(steps)} resources={len(resources)} converged=true metadata_initialized=true")
PY

printf 'Example completed successfully.\n'
