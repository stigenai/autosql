package executor

import (
	"context"
	"strings"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
)

// PostgreSQLCompletionConfig must come from the trusted release configuration
// and resolved resource references, never from the artifact being inspected.
// DatabaseIdentity is the same logical target identity used by execution.
type PostgreSQLCompletionConfig struct {
	URL, DatabaseIdentity, Environment, SourceRevision string
	Schemas                                            []string
	Managed                                            schema.Document
	Audit                                              LifecycleReader
	Now                                                Clock
	// RequireExistingAudit is used only when checking a possibly started execution.
	RequireExistingAudit bool
}

// ObservePostgreSQLCompletion owns a bounded read-only transaction. It never
// initializes history or invokes an application statement. Current approval and
// resource/reference authorization remain the caller's responsibility.
func ObservePostgreSQLCompletion(ctx context.Context, verified artifact.VerifiedObservation, config PostgreSQLCompletionConfig) CompletionEvidence {
	a, err := verified.Payload()
	if err != nil || a.DatabaseIdentity != config.DatabaseIdentity || a.TargetEnvironment != config.Environment || a.SourceRevision != config.SourceRevision {
		return completionFailure(CompletionRefused, "authorization_invalid")
	}
	if !completionEligible(a) {
		return completionFailure(CompletionIneligible, "unsupported_plan")
	}
	if strings.TrimSpace(config.URL) == "" || len(config.Schemas) == 0 || config.Audit == nil {
		return completionFailure(CompletionRefused, "evidence_reader_missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, config.URL)
	if err != nil {
		return completionFailure(CompletionRetryable, "database_unavailable")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}()
	return observeCompletionConnection(ctx, conn, verified, config, a)
}

func observeCompletionConnection(ctx context.Context, conn *pgx.Conn, verified artifact.VerifiedObservation, config PostgreSQLCompletionConfig, a artifact.Artifact) CompletionEvidence {
	now := time.Now
	if config.Now != nil {
		now = config.Now
	}
	result := withCompletionTransaction(ctx, conn, a, config, func(reader *postgresCompletionReader) CompletionEvidence {
		return ObserveCompletion(ctx, verified, reader, config.Audit, now().UTC())
	})
	if result.Outcome == CompletionProven {
		result.ObservedAt = now().UTC()
	}
	return result
}

func withCompletionTransaction(ctx context.Context, conn *pgx.Conn, a artifact.Artifact, config PostgreSQLCompletionConfig, inspect func(*postgresCompletionReader) CompletionEvidence) CompletionEvidence {
	// Read committed makes the second catalog read fresh. Target and relation
	// locks fence cooperating writers and DDL against existing relations.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return completionFailure(CompletionRetryable, "database_unavailable")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if failed := coordinateCompletion(ctx, tx, a, config.Schemas); failed.Outcome != "" {
		return failed
	}
	result := inspect(&postgresCompletionReader{tx: tx, config: config})
	if result.Outcome != "" && result.Outcome != CompletionProven {
		return result
	}
	if err := tx.Commit(ctx); err != nil {
		return completionFailure(CompletionRetryable, "database_unavailable")
	}
	return result
}

func coordinateCompletion(ctx context.Context, tx pgx.Tx, a artifact.Artifact, schemas []string) CompletionEvidence {
	if _, err := tx.Exec(ctx, `set local lock_timeout='1s'; set local statement_timeout='10s'; set local idle_in_transaction_session_timeout='15s'`); err != nil {
		return completionFailure(CompletionRetryable, "database_unavailable")
	}
	key, err := LockKey(a.DatabaseIdentity, a.TargetEnvironment)
	if err != nil {
		return completionFailure(CompletionRefused, "authorization_invalid")
	}
	var locked bool
	if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock(hashtextextended($1,0::bigint))`, key).Scan(&locked); err != nil {
		return completionFailure(CompletionRetryable, "database_unavailable")
	}
	if !locked {
		return completionFailure(CompletionRetryable, "target_busy")
	}
	if err := lockCompletionRelations(ctx, tx, schemas); err != nil {
		return completionFailure(CompletionRetryable, "state_unavailable")
	}
	return CompletionEvidence{}
}

func lockCompletionRelations(ctx context.Context, tx pgx.Tx, schemas []string) error {
	// ACCESS SHARE is permitted in read-only transactions. PostgreSQL does not
	// support LOCK TABLE on materialized views, so retain the adoption boundary.
	rows, err := tx.Query(ctx, `select n.nspname,c.relname from pg_class c join pg_namespace n on n.oid=c.relnamespace where (n.nspname=any($1) or c.oid=to_regclass('autosql_migration_history')) and c.relkind in ('r','p','v','f') order by n.nspname,c.relname`, schemas)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var namespace, name string
		if err := rows.Scan(&namespace, &name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, pgx.Identifier{namespace, name}.Sanitize())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) > 0 {
		_, err = tx.Exec(ctx, `lock table `+strings.Join(names, ",")+` in access share mode`)
	}
	return err
}

type postgresCompletionReader struct {
	tx     pgx.Tx
	config PostgreSQLCompletionConfig
}

func (r *postgresCompletionReader) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	return r.tx.Query(ctx, sql, args...)
}

func (r *postgresCompletionReader) State(ctx context.Context) (RuntimeState, error) {
	doc, err := postgres.InspectManagedTx(ctx, r.tx, postgres.Options{Schemas: r.config.Schemas}, r.config.Managed)
	if err != nil {
		return RuntimeState{}, err
	}
	doc, err = postgres.New().Normalize(ctx, doc)
	if err != nil {
		return RuntimeState{}, err
	}
	fingerprint, err := schema.SemanticFingerprint(ExcludeBookkeeping(doc))
	return RuntimeState{Fingerprint: fingerprint, SourceRevision: r.config.SourceRevision, Environment: r.config.Environment, DatabaseIdentity: r.config.DatabaseIdentity}, err
}
