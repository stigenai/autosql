#!/bin/sh
# Compare two successful tuple evidence directories, preserving raw identities.
set -eu
[ "$#" = 2 ] || {
  echo 'usage: compare-project-workflow.sh EVIDENCE_A EVIDENCE_B' >&2
  exit 2
}
left=$1
right=$2
for evidence in "$left" "$right"; do
  jq -s -e '[.[] | select(.Test == "TestProjectExamplePackagedWorkflow" and .Action == "pass")] | length == 1' "$evidence/commands.jsonl" >/dev/null
done
for mode in container existing_admin; do
  for scenario in schema-parallel1 schema-parallel2 migrate-parallel1 migrate-parallel2 regression; do
    cmp "$left/reports/$mode-$scenario-semantic.json" "$right/reports/$mode-$scenario-semantic.json"
  done
done
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for environment in local ci-postgres production; do
  for format in hcl json; do
    jq -S '.data.identity' "$left/reports/$environment-autosql.$format.json" >"$work/left"
    jq -S '.data.identity' "$right/reports/$environment-autosql.$format.json" >"$work/right"
    cmp "$work/left" "$work/right"
  done
done
# Runtime factory endpoints/images are intentionally different across tuples.
# Their complete config and test-input digests remain in each report. Every
# other captured example input must still agree byte-for-byte.
for evidence in left right; do
  if [ "$evidence" = left ]; then directory=$left; else directory=$right; fi
  jq -S '.example_inputs | with_entries(select(.key | endswith("/runtime.json") | not))' "$directory/reports/identity.json" >"$work/$evidence"
done
cmp "$work/left" "$work/right"
echo 'Required case semantics, reference-only configuration identities and example input bytes agree.'
