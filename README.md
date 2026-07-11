# AutoSQL

AutoSQL is a PostgreSQL-first schema management foundation. It provides a
versioned canonical schema graph, local SQL/native desired-state loading,
live database inspection, environment-aware configuration, runtime-only
secret resolution, and stable CLI/JSON contracts.

## Build and test

```sh
go test ./...
go build ./cmd/autosql
```

The PostgreSQL integration test is opt-in:

```sh
AUTOSQL_TEST_POSTGRES_URL='postgres://…' go test ./pkg/postgres -run Integration
```

## CLI

```sh
autosql version --json
autosql config validate --config autosql.json --preflight
autosql schema load --source sql:schema.sql --json
AUTOSQL_DATABASE_URL='postgres://…' \
  autosql schema inspect --url env://AUTOSQL_DATABASE_URL --schema public --json
```

Database URLs are supplied to the CLI through `env://` or `file://` secret
references. Resolved values are redacted and never included in serialized
output. See [the CLI contract](docs/cli-contract.md) and
[schema-source contract](docs/schema-sources.md).
