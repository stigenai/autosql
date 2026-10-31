#!/bin/sh
# Qualify both owned factories using explicit, digest-pinned PostgreSQL images.
# Usage: AUTOSQL_DBTEST_DOCKER_ENDPOINT=unix:///... \
#        AUTOSQL_DBTEST_MATRIX_DIR=/absolute/results \
#        scripts/test-dbtest-postgres-matrix.sh images.tsv
# images.tsv contains exactly five ascending "major repository@sha256:digest"
# lines, one each for PostgreSQL 14 through 18. No credentials belong in it.
set -eu

: "${AUTOSQL_DBTEST_DOCKER_ENDPOINT:?set an explicitly approved development daemon}"
: "${AUTOSQL_DBTEST_MATRIX_DIR:?set an absolute directory for qualification evidence}"
[ "$#" = 1 ] || {
        echo 'one image-list path is required' >&2
        exit 2
}
case "$AUTOSQL_DBTEST_MATRIX_DIR" in /*) ;; *)
        echo 'matrix directory must be absolute' >&2
        exit 2
        ;;
esac
repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
results=$AUTOSQL_DBTEST_MATRIX_DIR
mkdir -p "$results"
work=$(mktemp -d)
admin_matrix_container=''
cleanup() {
        if [ -n "$admin_matrix_container" ]; then
                docker --host "$AUTOSQL_DBTEST_DOCKER_ENDPOINT" container rm --force "$admin_matrix_container" >/dev/null
        fi
        rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM
cp -f "$1" "$work/images.tsv"
expected=14
while read -r major image extra; do
        [ "$major" = "$expected" ] && [ -z "$extra" ] || {
                echo 'image majors must be 14..18 in order' >&2
                exit 2
        }
        case "$image" in *@sha256:*) digest=${image##*@sha256:} ;; *)
                echo 'image digest required' >&2
                exit 2
                ;;
        esac
        [ "${#digest}" = 64 ] || {
                echo 'invalid image digest length' >&2
                exit 2
        }
        case "$digest" in *[!a-f0-9]*)
                echo 'invalid image digest' >&2
                exit 2
                ;;
        esac
        expected=$((expected + 1))
done <"$work/images.tsv"
[ "$expected" = 19 ] || {
        echo 'exactly five image majors are required' >&2
        exit 2
}
cp -f "$work/images.tsv" "$results/images.tsv"
cd "$repo"
go test -race -c -o "$work/dbtest.test" ./pkg/dbtest
go test -race -c -o "$work/cli.test" ./internal/cli
go test -race -c -o "$work/examples.test" ./examples/project-testing
{
        go version
        go env GOOS GOARCH
        docker --host "$AUTOSQL_DBTEST_DOCKER_ENDPOINT" info --format '{{json .ID}} {{json .ServerVersion}}'
        if command -v sha256sum >/dev/null 2>&1; then
                sha256sum "$work/dbtest.test" "$work/cli.test" "$work/examples.test"
        else
                shasum -a 256 "$work/dbtest.test" "$work/cli.test" "$work/examples.test"
        fi
} >"$results/runtime.txt"
while read -r major image; do
        export AUTOSQL_DBTEST_POSTGRES_IMAGE="$image" AUTOSQL_DBTEST_POSTGRES_MAJOR="$major"
        (
                cd "$repo/pkg/dbtest"
                "$work/dbtest.test" -test.run '^TestContainerFactoryLive' -test.v -test.timeout 5m
        ) >"$results/container-pg$major.log" 2>&1
        (
                cd "$repo/internal/cli"
                "$work/cli.test" -test.run '^TestDatabaseDevLiveContainerLifecycle$' -test.v -test.timeout 2m
        ) >"$results/container-cli-pg$major.log" 2>&1
        data_path=/var/lib/postgresql/data
        if [ "$major" = 18 ]; then data_path=/var/lib/postgresql; fi
        POSTGRES_PASSWORD=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
        export POSTGRES_PASSWORD
        admin_matrix_container=$(docker --host "$AUTOSQL_DBTEST_DOCKER_ENDPOINT" container run --detach --label autosql.task=admin-factory-matrix --tmpfs "$data_path:rw,nosuid,nodev,size=1073741824" --publish 127.0.0.1::5432 --env POSTGRES_PASSWORD --env POSTGRES_HOST_AUTH_METHOD=scram-sha-256 --env "PGDATA=$data_path/autosql" "$image")
        attempt=0
        until docker --host "$AUTOSQL_DBTEST_DOCKER_ENDPOINT" exec "$admin_matrix_container" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; do
                attempt=$((attempt + 1))
                [ "$attempt" -lt 60 ] || {
                        echo 'administration server readiness timeout' >&2
                        exit 124
                }
                sleep 1
        done
        actual=$(docker --host "$AUTOSQL_DBTEST_DOCKER_ENDPOINT" exec "$admin_matrix_container" psql -U postgres -X -At -c "SELECT current_setting('server_version_num')::integer / 10000")
        [ "$actual" = "$major" ] || {
                echo 'administration image major mismatch' >&2
                exit 3
        }
        binding=$(docker --host "$AUTOSQL_DBTEST_DOCKER_ENDPOINT" port "$admin_matrix_container" 5432/tcp)
        port=${binding##*:}
        export AUTOSQL_DBTEST_ADMIN_DSN="postgres://postgres:$POSTGRES_PASSWORD@127.0.0.1:$port/postgres?sslmode=disable"
        (
                cd "$repo/pkg/dbtest"
                "$work/dbtest.test" -test.run '^TestAdminFactoryLive' -test.v -test.timeout 5m
        ) >"$results/admin-pg$major.log" 2>&1
        (
                cd "$repo/internal/cli"
                "$work/cli.test" -test.run '^TestDatabaseDevLive(CLIOutputAndRecovery|OutputFailureCleansAllocation)$' -test.v -test.timeout 2m
        ) >"$results/admin-cli-pg$major.log" 2>&1
        (
                cd "$repo/pkg/dbtest"
                "$work/dbtest.test" -test.run '^(TestLanguageLive|TestSuiteLive)' -test.v -test.timeout 5m
        ) >"$results/suite-pg$major.log" 2>&1
        (
                cd "$repo/internal/cli"
                "$work/cli.test" -test.run '^TestProjectTestLive' -test.v -test.timeout 5m
        ) >"$results/suite-cli-pg$major.log" 2>&1
        (
                cd "$repo/examples/project-testing"
                "$work/examples.test" -test.run '^TestProjectExampleLive' -test.v -test.timeout 3m
        ) >"$results/suite-example-pg$major.log" 2>&1
        docker --host "$AUTOSQL_DBTEST_DOCKER_ENDPOINT" exec "$admin_matrix_container" psql -U postgres -X -At -c "SELECT datname FROM pg_database WHERE datname LIKE 'autosql_%'; SELECT rolname FROM pg_roles WHERE rolname LIKE 'autosql_%'" >"$results/admin-pg$major-remaining.log"
        [ ! -s "$results/admin-pg$major-remaining.log" ] || {
                echo 'administration resources remain' >&2
                exit 7
        }
        docker --host "$AUTOSQL_DBTEST_DOCKER_ENDPOINT" container rm --force "$admin_matrix_container" >/dev/null
        admin_matrix_container=''
        unset AUTOSQL_DBTEST_ADMIN_DSN POSTGRES_PASSWORD
        printf 'PostgreSQL %s: factories, suites, CLI and examples passed; administrator resources absent\n' "$major"
done <"$work/images.tsv"
