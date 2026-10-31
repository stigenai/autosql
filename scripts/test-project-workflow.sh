#!/bin/sh
# Run one PostgreSQL/platform tuple through a checksum-verified packaged binary.
# Approved development daemon, pinned image, major and admin URL use the same
# AUTOSQL_DBTEST_* variables as test-dbtest-postgres-matrix.sh.
set -eu

for command in go git jq docker; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "$command is required" >&2
    exit 2
  }
done

if [ "$#" -lt 4 ] || [ "$#" -gt 5 ]; then
  echo 'usage: test-project-workflow.sh /absolute/autosql SHA256 VERSION /absolute/new-evidence-dir [--candidate]' >&2
  exit 2
fi
binary=$1
expected=$2
version=$3
results=$4
scope=released
if [ "$#" = 5 ]; then
  [ "$5" = --candidate ] || {
    echo 'unknown option' >&2
    exit 2
  }
  scope=local-candidate
  : "${AUTOSQL_PROJECT_TEST_SOURCE_MANIFEST_SHA256:?identify the source snapshot used to build the candidate binary}"
fi
case "$binary:$results" in /*:/*) ;; *)
  echo 'binary and evidence paths must be absolute' >&2
  exit 2
  ;;
esac
[ -x "$binary" ] || {
  echo 'binary must be executable' >&2
  exit 2
}
[ "${#expected}" = 64 ] || {
  echo 'expected SHA256 must have 64 hexadecimal characters' >&2
  exit 2
}
case "$expected" in *[!a-f0-9]*)
  echo 'invalid expected SHA256' >&2
  exit 2
  ;;
esac
: "${AUTOSQL_DBTEST_DOCKER_ENDPOINT:?set the explicitly approved development daemon}"
: "${AUTOSQL_DBTEST_POSTGRES_IMAGE:?set a digest-pinned PostgreSQL image}"
: "${AUTOSQL_DBTEST_POSTGRES_MAJOR:?set PostgreSQL major 14..18}"
: "${AUTOSQL_DBTEST_ADMIN_DSN:?set the approved disposable development administrator URL}"
repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"
revision=$(git rev-parse HEAD)
if [ "$scope" = released ]; then
  [ -z "$(git status --porcelain)" ] || {
    echo 'released qualification requires a clean checkout' >&2
    exit 2
  }
  [ "$(git rev-list -n 1 "$version")" = "$revision" ] || {
    echo 'checkout must match the released version tag' >&2
    exit 2
  }
fi
umask 077
mkdir "$results"
export AUTOSQL_PROJECT_TEST_BINARY="$binary" AUTOSQL_PROJECT_TEST_BINARY_SHA256="$expected"
export AUTOSQL_PROJECT_TEST_VERSION="$version" AUTOSQL_PROJECT_TEST_OUTPUT="$results/reports"
export AUTOSQL_PROJECT_TEST_SCOPE="$scope" AUTOSQL_PROJECT_TEST_REVISION="$revision"
go test -json ./examples/project-testing -run '^TestProjectExamplePackagedWorkflow$' -count=1 -timeout 15m >"$results/commands.jsonl"
jq -s -e '[.[] | select(.Test == "TestProjectExamplePackagedWorkflow" and .Action == "pass")] | length == 1' "$results/commands.jsonl" >/dev/null
if [ "$AUTOSQL_DBTEST_POSTGRES_MAJOR" = 17 ]; then
  # This is separate real-signature library/CLI integration evidence, not a
  # claim that fixture SQL bypasses or qualifies production release authority.
  export AUTOSQL_PROJECT_TEST_DSN="$AUTOSQL_DBTEST_ADMIN_DSN"
  go test -json ./internal/cli -run '^TestProjectReleaseLiveGenerationApplyAndChangedInputRefusal$' -count=1 -timeout 5m >"$results/signed-integration.jsonl"
  jq -s -e '[.[] | select(.Test == "TestProjectReleaseLiveGenerationApplyAndChangedInputRefusal" and .Action == "pass")] | length == 1' "$results/signed-integration.jsonl" >/dev/null
fi
printf 'Packaged project workflow passed (%s, PostgreSQL %s); evidence: %s\n' "$scope" "$AUTOSQL_DBTEST_POSTGRES_MAJOR" "$results"
