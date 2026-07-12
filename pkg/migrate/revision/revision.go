// Package revision stores durable versioned-migration execution history in PostgreSQL.
package revision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"autosql/pkg/migrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const SchemaVersion = 5

var (
	ErrConfig = errors.New("invalid revision store configuration")
	ErrDirty  = errors.New("revision store is dirty")
	ident     = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

type Config struct {
	URL, Schema, MetaTable, RevisionsTable, StatementsTable, EventsTable, ManifestTable, OutboxTable, ExecutorHistorySchema, ExecutorHistoryTable string
}

func (c Config) normalized() (Config, error) {
	if c.URL == "" {
		return c, ErrConfig
	}
	defaults := map[*string]string{&c.Schema: "autosql_revision", &c.MetaTable: "meta", &c.RevisionsTable: "revisions", &c.StatementsTable: "statement_attempts", &c.EventsTable: "events", &c.ManifestTable: "manifest_ancestry", &c.OutboxTable: "lifecycle_outbox", &c.ExecutorHistorySchema: "public", &c.ExecutorHistoryTable: "autosql_migration_history"}
	for field, value := range defaults {
		if *field == "" {
			*field = value
		}
		if !ident.MatchString(*field) {
			return c, ErrConfig
		}
	}
	return c, nil
}
func q(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

type Store struct{ config Config }

// Session is a pinned target-database session. Callers use it to keep the
// advisory lock, revision snapshot, migration SQL and history writes on the
// same PostgreSQL backend. It deliberately does not expose the connection
// string or permit a second connection to be opened behind the caller's back.
type Session struct {
	conn   *pgx.Conn
	config Config
}

// OpenSession connects to the revision database. The caller owns Close.
func (s *Store) OpenSession(ctx context.Context) (*Session, error) {
	c, err := pgx.Connect(ctx, s.config.URL)
	if err != nil {
		return nil, errors.New("connect revision session")
	}
	return &Session{conn: c, config: s.config}, nil
}

func (s *Session) Close(ctx context.Context) error { return s.conn.Close(ctx) }
func (s *Session) Raw() *pgx.Conn                  { return s.conn }
func (s *Session) Lock(ctx context.Context, identity string) (bool, error) {
	if identity == "" {
		return false, ErrConfig
	}
	var ok bool
	err := s.conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1,0::bigint))`, identity).Scan(&ok)
	return ok, err
}
func (s *Session) Unlock(ctx context.Context, identity string) error {
	var ok bool
	if err := s.conn.QueryRow(ctx, `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, identity).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("revision advisory lock was lost")
	}
	return nil
}
func writerKey(c Config) string { return "autosql.revision-writers/v1/" + c.Schema }
func (s *Session) LockWriters(ctx context.Context) (bool, error) {
	return s.Lock(ctx, writerKey(s.config))
}
func (s *Session) UnlockWriters(ctx context.Context) error { return s.Unlock(ctx, writerKey(s.config)) }
func lockWriter(ctx context.Context, c *pgx.Conn, cfg Config) error {
	var ok bool
	return c.QueryRow(ctx, `select pg_advisory_lock(hashtextextended($1,0::bigint)) is null`, writerKey(cfg)).Scan(&ok)
}
func unlockWriter(ctx context.Context, c *pgx.Conn, cfg Config) {
	_, _ = c.Exec(ctx, `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, writerKey(cfg))
}
func (s *Session) BackendPID(ctx context.Context) (int32, error) {
	var pid int32
	err := s.conn.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid)
	return pid, err
}
func (s *Session) Begin(ctx context.Context) (pgx.Tx, error) { return s.conn.Begin(ctx) }

// LockMutationTables fences legacy/direct writers that do not participate in
// the canonical target advisory lock. The locks live until tx completion.
func (s *Session) LockMutationTables(ctx context.Context, tx pgx.Tx) error {
	for _, table := range []string{q(s.config.Schema, s.config.RevisionsTable), q(s.config.Schema, s.config.StatementsTable), q(s.config.Schema, s.config.EventsTable)} {
		if _, err := tx.Exec(ctx, `lock table `+table+` in share row exclusive mode`); err != nil {
			return errors.New("lock revision reconciliation tables")
		}
	}
	var reg *string
	if err := tx.QueryRow(ctx, `select to_regclass($1)::text`, s.config.ExecutorHistorySchema+"."+s.config.ExecutorHistoryTable).Scan(&reg); err != nil {
		return err
	}
	if reg != nil {
		if _, err := tx.Exec(ctx, `lock table `+q(s.config.ExecutorHistorySchema, s.config.ExecutorHistoryTable)+` in share row exclusive mode`); err != nil {
			return errors.New("lock executor reconciliation table")
		}
	}
	return nil
}

type OutboxRecord struct {
	ID      string
	Payload []byte
}

func (s *Session) EnqueueOutbox(ctx context.Context, tx pgx.Tx, id string, payload []byte) error {
	if id == "" || len(payload) == 0 {
		return ErrConfig
	}
	_, err := tx.Exec(ctx, `insert into `+q(s.config.Schema, s.config.OutboxTable)+`(event_id,payload) values($1,$2::jsonb) on conflict(event_id) do nothing`, id, payload)
	return err
}
func (s *Session) PendingOutbox(ctx context.Context) ([]OutboxRecord, error) {
	rows, err := s.conn.Query(ctx, `select event_id,payload::text from `+q(s.config.Schema, s.config.OutboxTable)+` where finalized_at is null order by event_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRecord
	for rows.Next() {
		var x OutboxRecord
		var p string
		if err = rows.Scan(&x.ID, &p); err != nil {
			return nil, err
		}
		x.Payload = []byte(p)
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Session) FinalizeOutbox(ctx context.Context, id string) error {
	tag, err := s.conn.Exec(ctx, `update `+q(s.config.Schema, s.config.OutboxTable)+` set finalized_at=clock_timestamp() where event_id=$1 and finalized_at is null`, id)
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("finalize lifecycle outbox")
	}
	return nil
}
func (s *Session) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return s.conn.Exec(ctx, sql, args...)
}
func (s *Session) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.conn.QueryRow(ctx, sql, args...)
}

func Open(c Config) (*Store, error) {
	n, e := c.normalized()
	if e != nil {
		return nil, e
	}
	return &Store{config: n}, nil
}

type InitHooks struct{ AfterMigration func(int) error }

func (s *Store) Init(ctx context.Context) error { return s.InitWithHooks(ctx, InitHooks{}) }
func (s *Store) InitWithHooks(ctx context.Context, hooks InitHooks) error {
	conn, err := pgx.Connect(ctx, s.config.URL)
	if err != nil {
		return errors.New("connect revision store")
	}
	defer conn.Close(context.WithoutCancel(ctx))
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	lock := fmt.Sprintf("%d:%s/%d:%s/%d:%s/%d:%s/%d:%s", len(s.config.Schema), s.config.Schema, len(s.config.MetaTable), s.config.MetaTable, len(s.config.RevisionsTable), s.config.RevisionsTable, len(s.config.StatementsTable), s.config.StatementsTable, len(s.config.EventsTable), s.config.EventsTable)
	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1,0::bigint))`, lock); err != nil {
		return fmt.Errorf("lock revision schema: %w", err)
	}
	if _, err = tx.Exec(ctx, `create schema if not exists `+q(s.config.Schema)); err != nil {
		return errors.New("create revision schema")
	}
	meta := q(s.config.Schema, s.config.MetaTable)
	if _, err = tx.Exec(ctx, `create table if not exists `+meta+` (singleton boolean primary key default true check(singleton), schema_version integer not null, updated_at timestamptz not null)`); err != nil {
		return errors.New("create revision metadata")
	}
	var version int
	err = tx.QueryRow(ctx, `select schema_version from `+meta+` where singleton=true for update`).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx, `insert into `+meta+`(singleton,schema_version,updated_at) values(true,0,clock_timestamp())`); err != nil {
			return err
		}
		version = 0
	} else if err != nil {
		return errors.New("read revision metadata")
	}
	if version < 0 || version > SchemaVersion {
		return fmt.Errorf("%w: unsupported schema version %d", ErrConfig, version)
	}
	for next := version + 1; next <= SchemaVersion; next++ {
		if err = migrateSchema(ctx, tx, s.config, next); err != nil {
			return err
		}
		if hooks.AfterMigration != nil {
			if err = hooks.AfterMigration(next); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `update `+meta+` set schema_version=$1,updated_at=clock_timestamp() where singleton=true`, next); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func migrateSchema(ctx context.Context, tx pgx.Tx, c Config, version int) error {
	r := q(c.Schema, c.RevisionsTable)
	st := q(c.Schema, c.StatementsTable)
	ev := q(c.Schema, c.EventsTable)
	switch version {
	case 1:
		_, err := tx.Exec(ctx, `create table `+r+` (
version text primary key, description text not null, kind text not null, file_name text not null,
file_digest text not null, manifest_digest text not null, artifact_digest text not null,
plan_digest text not null, checks_digest text not null, bundle_digest text not null,
state text not null, statement_ordinal integer not null default 0, attempt integer not null,
redacted_error text not null default '', operator_identity text not null,
started_at timestamptz not null, updated_at timestamptz not null, completed_at timestamptz,
from_version text not null default '', to_version text not null default '', supersedes text[] not null default '{}', reversal_of text not null default '',
check (kind in ('migration','baseline','checkpoint','reversal')), check (state in ('pending','applied','failed','partial','baseline','checkpoint')), check(attempt>0), check(statement_ordinal>=0))`)
		return err
	case 2:
		if _, err := tx.Exec(ctx, `create table `+st+` (version text not null references `+r+`(version), statement_ordinal integer not null, attempt integer not null, state text not null, statement_digest text not null, redacted_error text not null default '', operator_identity text not null, started_at timestamptz not null, completed_at timestamptz, primary key(version,statement_ordinal,attempt), check(state in ('intended','confirmed','failed','uncertain')))`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `create table `+ev+` (sequence bigint generated always as identity primary key, version text not null references `+r+`(version), attempt integer not null, event_type text not null, statement_ordinal integer not null default 0, redacted_detail text not null default '', operator_identity text not null, at timestamptz not null)`)
		return err
	case 3:
		_, err := tx.Exec(ctx, `alter table `+r+` add column duration_ns bigint not null default 0 check(duration_ns>=0), add column manifest_generation text not null default ''`)
		return err
	case 4:
		var existing *string
		if err := tx.QueryRow(ctx, `select to_regclass($1)::text`, c.Schema+"."+c.ManifestTable).Scan(&existing); err != nil {
			return err
		}
		if existing != nil {
			return fmt.Errorf("%w: unexpected manifest ancestry table at schema version 3", ErrConfig)
		}
		_, err := tx.Exec(ctx, `create table `+q(c.Schema, c.ManifestTable)+` (generation text primary key,digest text not null unique,parent_generation text not null,entry_count integer not null check(entry_count>=0),head_chain_digest text not null,recorded_at timestamptz not null)`)
		return err
	case 5:
		_, err := tx.Exec(ctx, `create table `+q(c.Schema, c.OutboxTable)+` (event_id text primary key,payload jsonb not null,finalized_at timestamptz)`)
		return err
	default:
		return ErrConfig
	}
}
func (s *Session) RecordManifest(ctx context.Context, tx pgx.Tx, m migrate.Manifest, parent string, at time.Time) error {
	count, head := migrate.ManifestHead(m)
	tag, err := tx.Exec(ctx, `insert into `+q(s.config.Schema, s.config.ManifestTable)+`(generation,digest,parent_generation,entry_count,head_chain_digest,recorded_at) values($1,$2,$3,$4,$5,$6) on conflict(generation) do nothing`, m.Generation, m.Digest, parent, count, head, at.UTC())
	if err != nil {
		return errors.New("record manifest ancestry")
	}
	if tag.RowsAffected() == 0 {
		var d, p, h string
		var n int
		if err = tx.QueryRow(ctx, `select digest,parent_generation,entry_count,head_chain_digest from `+q(s.config.Schema, s.config.ManifestTable)+` where generation=$1`, m.Generation).Scan(&d, &p, &n, &h); err != nil || d != m.Digest || n != count || h != head {
			return errors.New("manifest ancestry binding conflict")
		}
	}
	return nil
}
func (s *Session) ManifestDescendsFrom(ctx context.Context, current migrate.Manifest, ancestorGeneration, ancestorDigest string) (bool, error) {
	if current.Generation == ancestorGeneration && current.Digest == ancestorDigest {
		return true, nil
	}
	var count int
	var head string
	err := s.conn.QueryRow(ctx, `select entry_count,head_chain_digest from `+q(s.config.Schema, s.config.ManifestTable)+` where generation=$1 and digest=$2`, ancestorGeneration, ancestorDigest).Scan(&count, &head)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if count < 0 || count > len(current.Entries) {
		return false, nil
	}
	if count == 0 {
		return true, nil
	}
	return current.Entries[count-1].ChainDigest == head, nil
}

type Revision struct {
	Version, Description, Kind, FileName, FileDigest, ManifestDigest, ManifestGeneration, ArtifactDigest, PlanDigest, ChecksDigest, BundleDigest, State string
	StatementOrdinal, Attempt                                                                                                                           int
	RedactedError, Operator, FromVersion, ToVersion, ReversalOf                                                                                         string
	StartedAt, UpdatedAt                                                                                                                                time.Time
	CompletedAt                                                                                                                                         *time.Time
	Supersedes                                                                                                                                          []string
	Duration                                                                                                                                            time.Duration
}

// Revisions returns the authoritative rows visible on this pinned session.
func (s *Session) Revisions(ctx context.Context) ([]Revision, error) {
	rows, err := s.conn.Query(ctx, `select version,description,kind,file_name,file_digest,manifest_digest,manifest_generation,artifact_digest,plan_digest,checks_digest,bundle_digest,state,statement_ordinal,attempt,redacted_error,operator_identity,started_at,updated_at,completed_at,from_version,to_version,supersedes,reversal_of,duration_ns from `+q(s.config.Schema, s.config.RevisionsTable)+` order by version`)
	if err != nil {
		return nil, errors.New("read pinned revision snapshot")
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var r Revision
		var ns int64
		if err = rows.Scan(&r.Version, &r.Description, &r.Kind, &r.FileName, &r.FileDigest, &r.ManifestDigest, &r.ManifestGeneration, &r.ArtifactDigest, &r.PlanDigest, &r.ChecksDigest, &r.BundleDigest, &r.State, &r.StatementOrdinal, &r.Attempt, &r.RedactedError, &r.Operator, &r.StartedAt, &r.UpdatedAt, &r.CompletedAt, &r.FromVersion, &r.ToVersion, &r.Supersedes, &r.ReversalOf, &ns); err != nil {
			return nil, errors.New("scan pinned revision snapshot")
		}
		r.Duration = time.Duration(ns)
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, errors.New("iterate pinned revision snapshot")
	}
	return out, nil
}

// ExecRevision inserts a revision through the caller's transaction so the row,
// migration DDL and executor evidence share one commit boundary.
func (s *Session) ExecRevision(ctx context.Context, tx pgx.Tx, r Revision) error {
	if err := validateRevision(r); err != nil {
		return err
	}
	if r.Supersedes == nil {
		r.Supersedes = []string{}
	}
	_, err := tx.Exec(ctx, `insert into `+q(s.config.Schema, s.config.RevisionsTable)+`(version,description,kind,file_name,file_digest,manifest_digest,manifest_generation,artifact_digest,plan_digest,checks_digest,bundle_digest,state,statement_ordinal,attempt,redacted_error,operator_identity,started_at,updated_at,completed_at,from_version,to_version,supersedes,reversal_of,duration_ns) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, r.Version, r.Description, r.Kind, r.FileName, r.FileDigest, r.ManifestDigest, r.ManifestGeneration, r.ArtifactDigest, r.PlanDigest, r.ChecksDigest, r.BundleDigest, r.State, r.StatementOrdinal, r.Attempt, r.RedactedError, r.Operator, r.StartedAt.UTC(), r.UpdatedAt.UTC(), r.CompletedAt, r.FromVersion, r.ToVersion, r.Supersedes, r.ReversalOf, r.Duration.Nanoseconds())
	if err != nil {
		return errors.New("insert transactional revision")
	}
	return nil
}
func (s *Session) ExecEvent(ctx context.Context, tx pgx.Tx, e Event) error {
	_, err := tx.Exec(ctx, `insert into `+q(s.config.Schema, s.config.EventsTable)+`(version,attempt,event_type,statement_ordinal,redacted_detail,operator_identity,at) values($1,$2,$3,$4,$5,$6,$7)`, e.Version, e.Attempt, e.Type, e.Ordinal, e.Detail, e.Operator, e.At.UTC())
	if err != nil {
		return errors.New("insert transactional revision event")
	}
	return nil
}
func (s *Session) FinalizeRevision(ctx context.Context, tx pgx.Tx, version, state string, ordinal int, duration time.Duration, redacted string, at time.Time) error {
	tag, err := tx.Exec(ctx, `update `+q(s.config.Schema, s.config.RevisionsTable)+` set state=$2,statement_ordinal=$3,duration_ns=$4,redacted_error=$5,updated_at=$6::timestamptz,completed_at=case when $2 in ('applied','baseline','checkpoint') then $6::timestamptz else null::timestamptz end where version=$1`, version, state, ordinal, duration.Nanoseconds(), redacted, at.UTC())
	if err != nil {
		return fmt.Errorf("finalize transactional revision: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("finalize transactional revision: row missing")
	}
	return nil
}

// Repair performs a proposal-bound CAS and always leaves append-only evidence.
// Remove creates a reversal tombstone; it never deletes the original row.
func (s *Session) Repair(ctx context.Context, tx pgx.Tx, version, action, before, after, proposal, operator string, at time.Time) error {
	if action == "remove" {
		var r Revision
		var ns int64
		if err := tx.QueryRow(ctx, `select version,description,kind,file_name,file_digest,manifest_digest,manifest_generation,artifact_digest,plan_digest,checks_digest,bundle_digest,state,statement_ordinal,attempt,redacted_error,operator_identity,started_at,updated_at,completed_at,from_version,to_version,supersedes,reversal_of,duration_ns from `+q(s.config.Schema, s.config.RevisionsTable)+` where version=$1 and state=$2 for update`, version, before).Scan(&r.Version, &r.Description, &r.Kind, &r.FileName, &r.FileDigest, &r.ManifestDigest, &r.ManifestGeneration, &r.ArtifactDigest, &r.PlanDigest, &r.ChecksDigest, &r.BundleDigest, &r.State, &r.StatementOrdinal, &r.Attempt, &r.RedactedError, &r.Operator, &r.StartedAt, &r.UpdatedAt, &r.CompletedAt, &r.FromVersion, &r.ToVersion, &r.Supersedes, &r.ReversalOf, &ns); err != nil {
			return errors.New("repair stale revision")
		}
		r.Version = version + "~repair~" + strings.TrimPrefix(proposal, "sha256:")[:12]
		r.Description = "repair tombstone"
		r.Kind = "reversal"
		r.State = "applied"
		r.Attempt = 1
		r.Operator = operator
		r.StartedAt = at.UTC()
		r.UpdatedAt = at.UTC()
		r.CompletedAt = &at
		r.Supersedes = []string{version}
		r.ReversalOf = version
		r.Duration = 0
		if err := s.ExecRevision(ctx, tx, r); err != nil {
			return err
		}
		return s.ExecEvent(ctx, tx, Event{Version: r.Version, Attempt: 1, Type: "repair_applied", Detail: "proposal=" + proposal, Operator: operator, At: at})
	}
	tag, err := tx.Exec(ctx, `update `+q(s.config.Schema, s.config.RevisionsTable)+` set state=$3,operator_identity=$4,updated_at=$5,completed_at=case when $3 in ('applied','baseline','checkpoint') then $5 else completed_at end where version=$1 and state=$2`, version, before, after, operator, at.UTC())
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("repair stale revision")
	}
	return s.ExecEvent(ctx, tx, Event{Version: version, Attempt: 1, Type: "repair_applied", Detail: "proposal=" + proposal, Operator: operator, At: at})
}

func validateRevision(r Revision) error {
	if r.Version == "" || r.FileName == "" || r.FileDigest == "" || r.ManifestDigest == "" || r.ManifestGeneration == "" || r.Attempt < 1 || r.Operator == "" || r.StartedAt.IsZero() || r.UpdatedAt.IsZero() || r.Duration < 0 {
		return ErrConfig
	}
	if strings.Contains(strings.ToLower(r.RedactedError), "password=") || strings.Contains(r.RedactedError, "://") {
		return ErrConfig
	}
	return nil
}
func (s *Store) Insert(ctx context.Context, r Revision) error {
	if err := validateRevision(r); err != nil {
		return err
	}
	if r.Supersedes == nil {
		r.Supersedes = []string{}
	}
	c := s.config
	conn, err := pgx.Connect(ctx, c.URL)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if err = lockWriter(ctx, conn, c); err != nil {
		return err
	}
	defer unlockWriter(context.WithoutCancel(ctx), conn, c)
	_, err = conn.Exec(ctx, `insert into `+q(c.Schema, c.RevisionsTable)+`(version,description,kind,file_name,file_digest,manifest_digest,manifest_generation,artifact_digest,plan_digest,checks_digest,bundle_digest,state,statement_ordinal,attempt,redacted_error,operator_identity,started_at,updated_at,completed_at,from_version,to_version,supersedes,reversal_of,duration_ns) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, r.Version, r.Description, r.Kind, r.FileName, r.FileDigest, r.ManifestDigest, r.ManifestGeneration, r.ArtifactDigest, r.PlanDigest, r.ChecksDigest, r.BundleDigest, r.State, r.StatementOrdinal, r.Attempt, r.RedactedError, r.Operator, r.StartedAt.UTC(), r.UpdatedAt.UTC(), r.CompletedAt, r.FromVersion, r.ToVersion, r.Supersedes, r.ReversalOf, r.Duration.Nanoseconds())
	if err != nil {
		return errors.New("insert revision")
	}
	return nil
}

type StatementAttempt struct {
	Version, State, Digest, RedactedError, Operator string
	Ordinal, Attempt                                int
	StartedAt                                       time.Time
	CompletedAt                                     *time.Time
}

func (s *Store) InsertStatement(ctx context.Context, a StatementAttempt) error {
	c := s.config
	conn, e := pgx.Connect(ctx, c.URL)
	if e != nil {
		return e
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if e = lockWriter(ctx, conn, c); e != nil {
		return e
	}
	defer unlockWriter(context.WithoutCancel(ctx), conn, c)
	_, e = conn.Exec(ctx, `insert into `+q(c.Schema, c.StatementsTable)+`(version,statement_ordinal,attempt,state,statement_digest,redacted_error,operator_identity,started_at,completed_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9)`, a.Version, a.Ordinal, a.Attempt, a.State, a.Digest, a.RedactedError, a.Operator, a.StartedAt.UTC(), a.CompletedAt)
	return e
}

type Event struct {
	Version, Type, Detail, Operator string
	Attempt, Ordinal                int
	At                              time.Time
}

func (s *Store) AppendEvent(ctx context.Context, e Event) error {
	c := s.config
	conn, x := pgx.Connect(ctx, c.URL)
	if x != nil {
		return x
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if x = lockWriter(ctx, conn, c); x != nil {
		return x
	}
	defer unlockWriter(context.WithoutCancel(ctx), conn, c)
	_, x = conn.Exec(ctx, `insert into `+q(c.Schema, c.EventsTable)+`(version,attempt,event_type,statement_ordinal,redacted_detail,operator_identity,at) values($1,$2,$3,$4,$5,$6,$7)`, e.Version, e.Attempt, e.Type, e.Ordinal, e.Detail, e.Operator, e.At.UTC())
	return x
}

// InsertBatch atomically records a baseline/checkpoint prefix and its distinct events.
func (s *Store) InsertBatch(ctx context.Context, revisions []Revision, events []Event) error {
	c := s.config
	conn, e := pgx.Connect(ctx, c.URL)
	if e != nil {
		return e
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if e = lockWriter(ctx, conn, c); e != nil {
		return e
	}
	defer unlockWriter(context.WithoutCancel(ctx), conn, c)
	tx, e := conn.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, r := range revisions {
		if e = validateRevision(r); e != nil {
			return e
		}
		if r.Supersedes == nil {
			r.Supersedes = []string{}
		}
		_, e = tx.Exec(ctx, `insert into `+q(c.Schema, c.RevisionsTable)+`(version,description,kind,file_name,file_digest,manifest_digest,manifest_generation,artifact_digest,plan_digest,checks_digest,bundle_digest,state,statement_ordinal,attempt,redacted_error,operator_identity,started_at,updated_at,completed_at,from_version,to_version,supersedes,reversal_of,duration_ns) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, r.Version, r.Description, r.Kind, r.FileName, r.FileDigest, r.ManifestDigest, r.ManifestGeneration, r.ArtifactDigest, r.PlanDigest, r.ChecksDigest, r.BundleDigest, r.State, r.StatementOrdinal, r.Attempt, r.RedactedError, r.Operator, r.StartedAt.UTC(), r.UpdatedAt.UTC(), r.CompletedAt, r.FromVersion, r.ToVersion, r.Supersedes, r.ReversalOf, r.Duration.Nanoseconds())
		if e != nil {
			return errors.New("insert revision batch")
		}
	}
	for _, x := range events {
		_, e = tx.Exec(ctx, `insert into `+q(c.Schema, c.EventsTable)+`(version,attempt,event_type,statement_ordinal,redacted_detail,operator_identity,at) values($1,$2,$3,$4,$5,$6,$7)`, x.Version, x.Attempt, x.Type, x.Ordinal, x.Detail, x.Operator, x.At.UTC())
		if e != nil {
			return errors.New("insert revision event batch")
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) UpdateState(ctx context.Context, version string, attempt int, state string, ordinal int, redacted string, duration time.Duration, event string, operator string) error {
	c := s.config
	conn, e := pgx.Connect(ctx, c.URL)
	if e != nil {
		return e
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if e = lockWriter(ctx, conn, c); e != nil {
		return e
	}
	defer unlockWriter(context.WithoutCancel(ctx), conn, c)
	tx, e := conn.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, e := tx.Exec(ctx, `update `+q(c.Schema, c.RevisionsTable)+` set state=$3,statement_ordinal=$4,redacted_error=$5,duration_ns=$6,updated_at=clock_timestamp(),completed_at=case when $3='pending' then null else clock_timestamp() end where version=$1 and attempt=$2`, version, attempt, state, ordinal, redacted, duration.Nanoseconds())
	if e != nil || tag.RowsAffected() != 1 {
		return errors.New("update revision state")
	}
	if _, e = tx.Exec(ctx, `insert into `+q(c.Schema, c.EventsTable)+`(version,attempt,event_type,statement_ordinal,redacted_detail,operator_identity,at) values($1,$2,$3,$4,$5,$6,clock_timestamp())`, version, attempt, event, ordinal, redacted, operator); e != nil {
		return errors.New("insert revision state event")
	}
	return tx.Commit(ctx)
}

type ExecutorHistory struct{ ArtifactDigest, State string }
type ExecutorRecord struct {
	ArtifactDigest, StepID, StepHash, PhaseID, PhaseMode, State, ExecutionID, TargetIdentity, PlanDigest, BundleDigest string
	Attempt                                                                                                            int
}

func (s *Session) ExecutorRecords(ctx context.Context, target string) ([]ExecutorRecord, error) {
	if target == "" {
		return nil, ErrConfig
	}
	var reg *string
	if err := s.conn.QueryRow(ctx, `select to_regclass($1)::text`, s.config.ExecutorHistorySchema+"."+s.config.ExecutorHistoryTable).Scan(&reg); err != nil {
		return nil, err
	}
	if reg == nil {
		return nil, nil
	}
	rows, err := s.conn.Query(ctx, `select artifact_digest,step_id,step_hash,phase_id,phase_mode,state,execution_id,target_identity,plan_digest,bundle_digest,attempt from `+q(s.config.ExecutorHistorySchema, s.config.ExecutorHistoryTable)+` where target_identity=$1`, target)
	if err != nil {
		return nil, errors.New("read executor reconciliation history")
	}
	defer rows.Close()
	var out []ExecutorRecord
	for rows.Next() {
		var x ExecutorRecord
		if err = rows.Scan(&x.ArtifactDigest, &x.StepID, &x.StepHash, &x.PhaseID, &x.PhaseMode, &x.State, &x.ExecutionID, &x.TargetIdentity, &x.PlanDigest, &x.BundleDigest, &x.Attempt); err != nil {
			return nil, errors.New("scan executor reconciliation history")
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type StatusEntry struct {
	Version          string `json:"version"`
	File             string `json:"file,omitempty"`
	Classification   string `json:"classification"`
	RecordedState    string `json:"recorded_state,omitempty"`
	Attempt          int    `json:"attempt,omitempty"`
	StatementOrdinal int    `json:"statement_ordinal,omitempty"`
	DurationNS       int64  `json:"duration_ns,omitempty"`
	Drift            bool   `json:"drift,omitempty"`
	Dirty            bool   `json:"dirty,omitempty"`
	Unknown          bool   `json:"unknown,omitempty"`
	Guidance         string `json:"guidance,omitempty"`
}
type Status struct {
	ManifestDigest string         `json:"manifest_digest"`
	Entries        []StatusEntry  `json:"entries"`
	Counts         map[string]int `json:"counts"`
	Dirty          bool           `json:"dirty,omitempty"`
	Drift          bool           `json:"drift,omitempty"`
}

// Status is strictly read-only: it never calls Init or changes/reinterprets stored state.
func (s *Store) Status(ctx context.Context, manifest migrate.Manifest) (Status, error) {
	c := s.config
	conn, e := pgx.Connect(ctx, c.URL)
	if e != nil {
		return Status{}, errors.New("connect revision status")
	}
	defer conn.Close(context.WithoutCancel(ctx))
	tx, e := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return Status{}, fmt.Errorf("begin read-only revision status: %w", e)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, e := tx.Query(ctx, `select version,description,kind,file_name,file_digest,manifest_digest,manifest_generation,artifact_digest,plan_digest,checks_digest,bundle_digest,state,statement_ordinal,attempt,redacted_error,operator_identity,started_at,updated_at,completed_at,from_version,to_version,supersedes,reversal_of,duration_ns from `+q(c.Schema, c.RevisionsTable)+` order by version`)
	if e != nil {
		return Status{}, fmt.Errorf("read revision status: %w", e)
	}
	records := map[string]Revision{}
	for rows.Next() {
		var r Revision
		var durationNS int64
		if e = rows.Scan(&r.Version, &r.Description, &r.Kind, &r.FileName, &r.FileDigest, &r.ManifestDigest, &r.ManifestGeneration, &r.ArtifactDigest, &r.PlanDigest, &r.ChecksDigest, &r.BundleDigest, &r.State, &r.StatementOrdinal, &r.Attempt, &r.RedactedError, &r.Operator, &r.StartedAt, &r.UpdatedAt, &r.CompletedAt, &r.FromVersion, &r.ToVersion, &r.Supersedes, &r.ReversalOf, &durationNS); e != nil {
			rows.Close()
			return Status{}, e
		}
		r.Duration = time.Duration(durationNS)
		records[r.Version] = r
	}
	if e = rows.Err(); e != nil {
		return Status{}, e
	}
	rows.Close()
	history := map[string]map[string]bool{}
	var reg *string
	if e = tx.QueryRow(ctx, `select to_regclass($1)::text`, c.ExecutorHistorySchema+"."+c.ExecutorHistoryTable).Scan(&reg); e != nil {
		return Status{}, fmt.Errorf("locate executor history: %w", e)
	}
	artifacts := []string{}
	for _, r := range records {
		if r.ArtifactDigest != "" {
			artifacts = append(artifacts, r.ArtifactDigest)
		}
	}
	if reg != nil && len(artifacts) > 0 {
		hr, he := tx.Query(ctx, `select artifact_digest,state from `+q(c.ExecutorHistorySchema, c.ExecutorHistoryTable)+` where artifact_digest=any($1::text[])`, artifacts)
		if he != nil {
			return Status{}, fmt.Errorf("read executor history: %w", he)
		}
		for hr.Next() {
			var d, st string
			if he = hr.Scan(&d, &st); he != nil {
				hr.Close()
				return Status{}, fmt.Errorf("scan executor history: %w", he)
			}
			if d == "" || st == "" {
				hr.Close()
				return Status{}, errors.New("malformed executor history")
			}
			if history[d] == nil {
				history[d] = map[string]bool{}
			}
			history[d][st] = true
		}
		if he = hr.Err(); he != nil {
			hr.Close()
			return Status{}, fmt.Errorf("iterate executor history: %w", he)
		}
		hr.Close()
	}
	out := Status{ManifestDigest: manifest.Digest, Counts: map[string]int{}}
	seen := map[string]bool{}
	for index, m := range manifest.Entries {
		seen[m.Version] = true
		r, ok := records[m.Version]
		entry := StatusEntry{Version: m.Version, File: m.File, Classification: "pending"}
		if ok {
			entry.RecordedState = r.State
			entry.Attempt = r.Attempt
			entry.StatementOrdinal = r.StatementOrdinal
			entry.DurationNS = r.Duration.Nanoseconds()
			entry.Classification = r.State
			entry.Drift = r.FileName != m.File || r.FileDigest != m.SQLDigest || (index == len(manifest.Entries)-1 && (r.ManifestDigest != manifest.Digest || r.ManifestGeneration != manifest.Generation)) || r.PlanDigest != m.Directives.PlanDigest || r.ChecksDigest != m.Directives.CheckDigest || r.BundleDigest != m.Directives.BundleDigest
			if entry.Drift {
				entry.Classification = "drift"
				entry.Guidance = "restore the verified manifest or record an explicit repair"
			}
			if r.State == "failed" || r.State == "partial" {
				entry.Dirty = true
			}
			if history[r.ArtifactDigest]["intended"] || history[r.ArtifactDigest]["uncertain"] {
				entry.Dirty = true
				entry.Guidance = "reconcile incomplete executor history without rewriting revision state"
			}
		}
		out.Entries = append(out.Entries, entry)
		out.Counts[entry.Classification]++
		out.Dirty = out.Dirty || entry.Dirty
		out.Drift = out.Drift || entry.Drift
	}
	for v, r := range records {
		if seen[v] {
			continue
		}
		out.Entries = append(out.Entries, StatusEntry{Version: v, File: r.FileName, Classification: "unknown", RecordedState: r.State, Attempt: r.Attempt, StatementOrdinal: r.StatementOrdinal, Unknown: true, Dirty: true, Guidance: "revision is absent from the verified manifest"})
		out.Counts["unknown"]++
		out.Dirty = true
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, ae := migrate.ParseVersion(out.Entries[i].Version)
		b, be := migrate.ParseVersion(out.Entries[j].Version)
		if ae == nil && be == nil {
			return a.Compare(b) < 0
		}
		return out.Entries[i].Version < out.Entries[j].Version
	})
	if e = tx.Commit(ctx); e != nil {
		return Status{}, fmt.Errorf("finish read-only revision status: %w", e)
	}
	return out, nil
}

func (s Status) MarshalJSON() ([]byte, error) { type alias Status; return json.Marshal(alias(s)) }
