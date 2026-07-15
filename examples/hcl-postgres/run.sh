#!/usr/bin/env bash
set -euo pipefail

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

printf '== 1. Load both HCL schema versions ==\n'
"${AUTOSQL[@]}" schema load --source "hcl:$ROOT/schema-v1.hcl" --json >"$WORKDIR/load-v1.json"
"${AUTOSQL[@]}" schema load --source "hcl:$ROOT/schema-v2.hcl" --json >"$WORKDIR/load-v2.json"

printf '== 2. Diff and plan the HCL upgrade ==\n'
"${AUTOSQL[@]}" schema diff --from "hcl:$ROOT/schema-v1.hcl" --to "hcl:$ROOT/schema-v2.hcl" --json >"$WORKDIR/diff.json"
"${AUTOSQL[@]}" plan --from "hcl:$ROOT/schema-v1.hcl" --to "hcl:$ROOT/schema-v2.hcl" --json >"$WORKDIR/plan.json"

printf '== 3. Create the v1 schema in PostgreSQL ==\n'
psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --file "$ROOT/apply-v1.sql" >/dev/null

printf '== 4. Inspect PostgreSQL as HCL and load it back ==\n'
"${AUTOSQL[@]}" schema inspect --url env://AUTOSQL_DATABASE_URL --schema hcl_demo --format hcl >"$WORKDIR/live-v1.hcl"
"${AUTOSQL[@]}" schema load --source "hcl:$WORKDIR/live-v1.hcl" --json >"$WORKDIR/live-v1.json"

printf '== 5. Apply the planned v2 expansion ==\n'
psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --file "$ROOT/apply-v2.sql" >/dev/null

printf '== 6. Round-trip v2 from PostgreSQL to HCL and verify convergence ==\n'
"${AUTOSQL[@]}" schema inspect --url env://AUTOSQL_DATABASE_URL --schema hcl_demo --format hcl >"$WORKDIR/live-v2.hcl"
"${AUTOSQL[@]}" schema load --source "hcl:$WORKDIR/live-v2.hcl" --json >"$WORKDIR/live-v2.json"

printf '== 7. Load the comprehensive PostgreSQL HCL catalog ==\n'
"${AUTOSQL[@]}" schema load --source "hcl:$ROOT/advanced.hcl" --json >"$WORKDIR/advanced-checked-in.json"
"${AUTOSQL[@]}" schema load --source "hcl:$ROOT/friendly-advanced.hcl" --json >"$WORKDIR/advanced-friendly.json"

printf '== 8. Create and inspect every supported PostgreSQL catalog kind ==\n'
psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --file "$ROOT/advanced.sql" >/dev/null
ADVANCED_FILTERS=(
  --include 'schema:hcl_advanced'
  --include 'role:hcl_advanced_*'
  --include 'grant:hcl_advanced.*'
  --include 'membership:*hcl_advanced*'
  --include 'default_privilege:*hcl_advanced*'
)
"${AUTOSQL[@]}" schema inspect --url env://AUTOSQL_DATABASE_URL --schema hcl_advanced \
  --advanced "${ADVANCED_FILTERS[@]}" --format hcl >"$WORKDIR/advanced-live.hcl"
"${AUTOSQL[@]}" schema load --source "hcl:$WORKDIR/advanced-live.hcl" --json >"$WORKDIR/advanced-live.json"

python3 - "$WORKDIR" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])

def read(name):
    value = json.loads((root / name).read_text())
    if not value.get("ok"):
        raise SystemExit(f"{name}: command failed: {value}")
    return value

desired_v1 = read("load-v1.json")
desired_v2 = read("load-v2.json")
live_v1 = read("live-v1.json")
live_v2 = read("live-v2.json")
diff = read("diff.json")
plan = read("plan.json")
advanced_checked_in = read("advanced-checked-in.json")
advanced_friendly = read("advanced-friendly.json")
advanced_live = read("advanced-live.json")

changes = diff["data"]["changes"]["changes"]
steps = plan["data"]["plan"]["steps"]
assert len(changes) == 2, changes
assert len(steps) == 2, steps
assert all(change["operation"] == "create" for change in changes), changes

def identities(envelope):
    resources = envelope["data"]["graph"]["resources"]
    return {
        (r["kind"], r["name"].get("schema"), r["name"].get("parent"), r["name"]["name"])
        for r in resources
        if r["kind"] in {"schema", "table", "column"}
    }

assert identities(desired_v1) == identities(live_v1)
assert identities(desired_v2) == identities(live_v2)

expected_advanced_kinds = {
    "schema", "extension", "enum", "domain", "composite_type", "sequence",
    "table", "column", "primary_key", "unique_constraint", "check_constraint",
    "foreign_key", "index", "view", "materialized_view", "function",
    "procedure", "trigger", "policy", "role", "grant", "membership",
    "default_privilege",
}

def kinds(envelope):
    return {r["kind"] for r in envelope["data"]["graph"]["resources"]}

assert expected_advanced_kinds <= kinds(advanced_checked_in), expected_advanced_kinds - kinds(advanced_checked_in)
assert expected_advanced_kinds <= kinds(advanced_live), expected_advanced_kinds - kinds(advanced_live)
assert {"primary_key", "unique_constraint", "check_constraint", "foreign_key", "index", "policy", "grant", "membership", "default_privilege"} <= kinds(advanced_friendly)
print("verified: hcl_load=true changes=2 plan_steps=2 postgres_round_trip=true converged=true advanced_kinds=23 helpers=true")
PY

printf 'Example completed successfully.\n'
