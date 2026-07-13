// Package backfill runs bounded, resumable shadow-column backfills.
package backfill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const Version = "autosql.zdm.backfill/v1"
const DefaultSchema = "autosql_zdm_backfill"

var ErrInvalid = errors.New("invalid online backfill")
var ErrBusy = errors.New("backfill worker already active")
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
var cast = regexp.MustCompile(`^value::([a-z_][a-z0-9_]*)$`)

type Config struct {
	URL, Schema, Target, Environment                                           string
	BatchSize, MaxRetries, LockTimeoutMS, StatementTimeoutMS, MaxRowsPerSecond int
	Delay, Backoff                                                             time.Duration
}
type Spec struct {
	Version           string `json:"version"`
	ArtifactDigest    string `json:"artifact_digest"`
	JobID             string `json:"job_id"`
	PhysicalSchema    string `json:"physical_schema"`
	Table             string `json:"table"`
	KeyColumn         string `json:"key_column"`
	SourceColumn      string `json:"source_column"`
	DestinationColumn string `json:"destination_column"`
	Transform         string `json:"transform"`
	Digest            string `json:"digest"`
}
type Status struct {
	JobID, State                  string
	Processed, Remaining, Retries int64
	ThroughputRowsPerSecond       float64
	LagSeconds                    int64
	LastError                     string
	StartedAt, UpdatedAt          time.Time
	LastBatchRows                 int
}

func New(artifact, job, schema, table, key, source, dest, transform string) (Spec, error) {
	s := Spec{Version: Version, ArtifactDigest: artifact, JobID: job, PhysicalSchema: schema, Table: table, KeyColumn: key, SourceColumn: source, DestinationColumn: dest, Transform: transform}
	d, e := digest(s)
	if e != nil {
		return Spec{}, e
	}
	s.Digest = d
	if e = s.Validate(); e != nil {
		return Spec{}, e
	}
	return s, nil
}
func (s Spec) Validate() error {
	if s.Version != Version || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.ArtifactDigest) || !identifier.MatchString(s.JobID) || !identifier.MatchString(s.PhysicalSchema) || !identifier.MatchString(s.Table) || !identifier.MatchString(s.KeyColumn) || !identifier.MatchString(s.SourceColumn) || !identifier.MatchString(s.DestinationColumn) || s.SourceColumn == s.DestinationColumn {
		return fmt.Errorf("%w: identity fields", ErrInvalid)
	}
	if _, e := transformSQL(s.Transform, "x"); e != nil {
		return fmt.Errorf("%w: transform: %v", ErrInvalid, e)
	}
	d, e := digest(s)
	if e != nil || d != s.Digest {
		return fmt.Errorf("%w: digest mismatch", ErrInvalid)
	}
	return nil
}
func digest(s Spec) (string, error) {
	s.Digest = ""
	b, e := json.Marshal(s)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func (s Spec) MarshalJSONCanonical() ([]byte, error) {
	if e := s.Validate(); e != nil {
		return nil, e
	}
	return json.Marshal(s)
}
func ParseJSON(b []byte) (Spec, error) {
	var s Spec
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e := d.Decode(&s); e != nil {
		return Spec{}, e
	}
	if e := d.Decode(&struct{}{}); e != io.EOF {
		return Spec{}, fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	if e := s.Validate(); e != nil {
		return Spec{}, e
	}
	return s, nil
}
func q(x ...string) string { return pgx.Identifier(x).Sanitize() }
func lit(s string) string  { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func transformSQL(x, value string) (string, error) {
	switch x {
	case "value":
		return value, nil
	case "lower(value)":
		return "pg_catalog.lower(" + value + ")", nil
	case "upper(value)":
		return "pg_catalog.upper(" + value + ")", nil
	case "btrim(value)":
		return "pg_catalog.btrim(" + value + ")", nil
	}
	if m := cast.FindStringSubmatch(x); m != nil {
		return "(" + value + ")::" + q(m[1]), nil
	}
	return "", errors.New("unsupported transform")
}
func defaults(c Config) (Config, error) {
	if c.Schema == "" {
		c.Schema = DefaultSchema
	}
	if c.URL == "" || c.Target == "" || c.Environment == "" || !identifier.MatchString(c.Schema) || c.BatchSize <= 0 || c.BatchSize > 10000 || c.MaxRetries < 0 || c.LockTimeoutMS <= 0 || c.StatementTimeoutMS <= 0 || c.MaxRowsPerSecond < 0 || c.Delay < 0 || c.Backoff < 0 {
		return c, fmt.Errorf("%w: configuration", ErrInvalid)
	}
	return c, nil
}
func scope(c Config, s Spec) string {
	b, _ := json.Marshal([]string{Version, c.Target, c.Environment, s.ArtifactDigest, s.Digest})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func ensure(ctx context.Context, c *pgx.Conn, cfg Config) error {
	comment := "autosql:zdm:backfill:v1"
	var exists bool
	var cm *string
	e := c.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1),obj_description(to_regnamespace($1),'pg_namespace')`, cfg.Schema).Scan(&exists, &cm)
	if e != nil {
		return e
	}
	if exists && (cm == nil || *cm != comment) {
		return fmt.Errorf("%w: control schema collision", ErrInvalid)
	}
	if !exists {
		if _, e = c.Exec(ctx, "create schema "+q(cfg.Schema)); e != nil {
			return e
		}
		if _, e = c.Exec(ctx, "comment on schema "+q(cfg.Schema)+" is "+lit(comment)); e != nil {
			return e
		}
	}
	var tableExists bool
	var tableComment *string
	if e = c.QueryRow(ctx, `select to_regclass($1) is not null,obj_description(to_regclass($1),'pg_class')`, cfg.Schema+".jobs").Scan(&tableExists, &tableComment); e != nil {
		return e
	}
	marker := comment + ":jobs"
	if tableExists && (tableComment == nil || *tableComment != marker) {
		return fmt.Errorf("%w: jobs table collision", ErrInvalid)
	}
	if !tableExists {
		if _, e = c.Exec(ctx, "create table "+q(cfg.Schema, "jobs")+`(scope text primary key,job_id text not null,spec_digest text not null,state text not null check(state in ('running','paused','cancelled','complete','failed')),processed bigint not null default 0,remaining bigint not null default 0,retries bigint not null default 0,last_error text not null default '',started_at timestamptz not null,updated_at timestamptz not null)`); e != nil {
			return e
		}
		if _, e = c.Exec(ctx, "comment on table "+q(cfg.Schema, "jobs")+" is "+lit(marker)); e != nil {
			return e
		}
	}
	return nil
}
func validateExisting(ctx context.Context, c *pgx.Conn, cfg Config) error {
	var cm, tm *string
	if e := c.QueryRow(ctx, `select obj_description(to_regnamespace($1),'pg_namespace'),obj_description(to_regclass($1||'.jobs'),'pg_class')`, cfg.Schema).Scan(&cm, &tm); e != nil {
		return e
	}
	if cm == nil || *cm != "autosql:zdm:backfill:v1" || tm == nil || *tm != "autosql:zdm:backfill:v1:jobs" {
		return fmt.Errorf("%w: backfill control state absent or untrusted", ErrInvalid)
	}
	return nil
}

func Run(ctx context.Context, cfg Config, s Spec) (Status, error) {
	cfg, e := defaults(cfg)
	if e != nil {
		return Status{}, e
	}
	if e = s.Validate(); e != nil {
		return Status{}, e
	}
	for {
		st, e := RunBatch(ctx, cfg, s)
		if e != nil {
			return st, e
		}
		if st.State != "running" || st.Remaining == 0 {
			return st, nil
		}
		wait := cfg.Delay
		if cfg.MaxRowsPerSecond > 0 && st.LastBatchRows > 0 {
			min := time.Duration(float64(time.Second) * float64(st.LastBatchRows) / float64(cfg.MaxRowsPerSecond))
			if min > wait {
				wait = min
			}
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return st, ctx.Err()
			case <-time.After(wait):
			}
		}
	}
}

func RunBatch(ctx context.Context, cfg Config, s Spec) (Status, error) {
	cfg, e := defaults(cfg)
	if e != nil {
		return Status{}, e
	}
	if e = s.Validate(); e != nil {
		return Status{}, e
	}
	c, e := pgx.Connect(ctx, cfg.URL)
	if e != nil {
		return Status{}, e
	}
	defer c.Close(context.WithoutCancel(ctx))
	for _, stmt := range []string{"set lock_timeout=" + lit(fmt.Sprintf("%dms", cfg.LockTimeoutMS)), "set statement_timeout=" + lit(fmt.Sprintf("%dms", cfg.StatementTimeoutMS))} {
		if _, e = c.Exec(ctx, stmt); e != nil {
			return Status{}, e
		}
	}
	if e = validateLive(ctx, c, s); e != nil {
		return Status{}, e
	}
	if e = ensure(ctx, c, cfg); e != nil {
		return Status{}, e
	}
	domain := "autosql.zdm.backfill/v1/" + scope(cfg, s)
	var locked bool
	if e = c.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1,0::bigint))`, domain).Scan(&locked); e != nil {
		return Status{}, e
	}
	if !locked {
		return Status{}, ErrBusy
	}
	defer c.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, domain)
	if e = ensureJob(ctx, c, cfg, s); e != nil {
		return Status{}, e
	}
	for attempt := 0; ; attempt++ {
		n, e := batch(ctx, c, cfg, s)
		if e == nil {
			st, se := StatusOf(ctx, cfg, s)
			st.LastBatchRows = n
			return st, se
		}
		if !transient(e) || attempt >= cfg.MaxRetries {
			_ = recordError(ctx, c, cfg, s, e, attempt)
			st, _ := StatusOf(ctx, cfg, s)
			return st, safeError(e)
		}
		_, _ = c.Exec(ctx, "update "+q(cfg.Schema, "jobs")+" set retries=retries+1,updated_at=clock_timestamp() where scope=$1", scope(cfg, s))
		select {
		case <-ctx.Done():
			return Status{}, ctx.Err()
		case <-time.After(cfg.Backoff * time.Duration(attempt+1)):
		}
	}
}
func validateLive(ctx context.Context, c *pgx.Conn, s Spec) error {
	rel := q(s.PhysicalSchema, s.Table)
	var unique, columns, privileges bool
	e := c.QueryRow(ctx, `select exists(select 1 from pg_index i join pg_attribute a on a.attrelid=i.indrelid and a.attnum=i.indkey[0] where i.indrelid=$1::regclass and i.indisunique and i.indisvalid and i.indisready and i.indnkeyatts=1 and i.indpred is null and i.indexprs is null and a.attname=$2), (select count(*)=3 from pg_attribute where attrelid=$1::regclass and attname=any($3) and attnum>0 and not attisdropped), has_table_privilege(current_user,$1,'SELECT,UPDATE')`, rel, s.KeyColumn, []string{s.KeyColumn, s.SourceColumn, s.DestinationColumn}).Scan(&unique, &columns, &privileges)
	if e != nil {
		return e
	}
	if !unique {
		return fmt.Errorf("%w: key column must have an exact valid single-column unique index", ErrInvalid)
	}
	if !columns {
		return fmt.Errorf("%w: key/source/destination columns missing", ErrInvalid)
	}
	if !privileges {
		return fmt.Errorf("%w: SELECT and UPDATE privileges required", ErrInvalid)
	}
	return nil
}
func ensureJob(ctx context.Context, c *pgx.Conn, cfg Config, s Spec) error {
	sc := scope(cfg, s)
	_, e := c.Exec(ctx, "insert into "+q(cfg.Schema, "jobs")+"(scope,job_id,spec_digest,state,remaining,started_at,updated_at) values($1,$2,$3,'running',-1,clock_timestamp(),clock_timestamp()) on conflict(scope) do nothing", sc, s.JobID, s.Digest)
	if e != nil {
		return e
	}
	var d string
	if e = c.QueryRow(ctx, "select spec_digest from "+q(cfg.Schema, "jobs")+" where scope=$1", sc).Scan(&d); e != nil {
		return e
	}
	if d != s.Digest {
		return fmt.Errorf("%w: durable job spec mismatch", ErrInvalid)
	}
	return nil
}
func safeError(e error) error {
	var p *pgconn.PgError
	if errors.As(e, &p) {
		return fmt.Errorf("backfill database error SQLSTATE %s", p.Code)
	}
	return errors.New("backfill batch failed")
}
func batch(ctx context.Context, c *pgx.Conn, cfg Config, s Spec) (int, error) {
	tx, e := c.Begin(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, stmt := range []string{"set local lock_timeout=" + lit(fmt.Sprintf("%dms", cfg.LockTimeoutMS)), "set local statement_timeout=" + lit(fmt.Sprintf("%dms", cfg.StatementTimeoutMS)), "set local autosql.zdm.backfill='on'"} {
		if _, e = tx.Exec(ctx, stmt); e != nil {
			return 0, e
		}
	}
	sc := scope(cfg, s)
	var state, savedDigest string
	var durableRemaining int64
	e = tx.QueryRow(ctx, "select state,spec_digest,remaining from "+q(cfg.Schema, "jobs")+" where scope=$1 for update", sc).Scan(&state, &savedDigest, &durableRemaining)
	if e != nil {
		return 0, e
	}
	if savedDigest != "" && savedDigest != s.Digest {
		return 0, fmt.Errorf("%w: durable job spec mismatch", ErrInvalid)
	}
	if state != "running" {
		return 0, tx.Commit(ctx)
	}
	expr, _ := transformSQL(s.Transform, "src."+q(s.SourceColumn))
	table := q(s.PhysicalSchema, s.Table)
	key := q(s.KeyColumn)
	dest := q(s.DestinationColumn)
	source := q(s.SourceColumn)
	if durableRemaining < 0 {
		if e = tx.QueryRow(ctx, "select count(*) from "+table+" where "+dest+" is null and "+source+" is not null").Scan(&durableRemaining); e != nil {
			return 0, e
		}
		if _, e = tx.Exec(ctx, "update "+q(cfg.Schema, "jobs")+" set remaining=$2 where scope=$1", sc, durableRemaining); e != nil {
			return 0, e
		}
	}
	sql := fmt.Sprintf(`with batch as (select %[1]s from %[2]s where %[3]s is null and %[4]s is not null order by %[1]s for update skip locked limit $1),changed as (update %[2]s dst set %[3]s=%[5]s from %[2]s src,batch where dst.%[1]s=batch.%[1]s and src.%[1]s=batch.%[1]s and dst.%[3]s is null returning 1) select count(*) from changed`, key, table, dest, source, expr)
	var n int
	if e = tx.QueryRow(ctx, sql, cfg.BatchSize).Scan(&n); e != nil {
		return 0, e
	}
	if _, e = tx.Exec(ctx, "update "+q(cfg.Schema, "jobs")+" set processed=processed+$2,remaining=greatest(remaining-$2,0),last_error='',updated_at=clock_timestamp() where scope=$1", sc, n); e != nil {
		return 0, e
	}
	var more bool
	if e = tx.QueryRow(ctx, "select exists(select 1 from "+table+" where "+dest+" is null and "+source+" is not null limit 1)").Scan(&more); e != nil {
		return 0, e
	}
	if !more {
		if _, e = tx.Exec(ctx, "update "+q(cfg.Schema, "jobs")+" set state='complete',remaining=0,updated_at=clock_timestamp() where scope=$1", sc); e != nil {
			return 0, e
		}
	}
	return n, tx.Commit(ctx)
}
func transient(e error) bool {
	var p *pgconn.PgError
	if errors.As(e, &p) {
		return p.Code == "40001" || p.Code == "40P01" || p.Code == "55P03" || p.Code == "57014"
	}
	return false
}
func recordError(ctx context.Context, c *pgx.Conn, cfg Config, s Spec, cause error, retries int) error {
	msg := safeError(cause).Error()
	_, e := c.Exec(ctx, "update "+q(cfg.Schema, "jobs")+" set state='failed',last_error=$2,updated_at=clock_timestamp() where scope=$1", scope(cfg, s), msg)
	return e
}

func Control(ctx context.Context, cfg Config, s Spec, action string) (Status, error) {
	cfg, e := defaults(cfg)
	if e != nil {
		return Status{}, e
	}
	if e = s.Validate(); e != nil {
		return Status{}, e
	}
	c, e := pgx.Connect(ctx, cfg.URL)
	if e != nil {
		return Status{}, e
	}
	defer c.Close(context.WithoutCancel(ctx))
	if e = validateExisting(ctx, c, cfg); e != nil {
		return Status{}, e
	}
	state := map[string]string{"pause": "paused", "resume": "running", "cancel": "cancelled"}[action]
	if state == "" {
		return Status{}, fmt.Errorf("%w: action", ErrInvalid)
	}
	tag, e := c.Exec(ctx, "update "+q(cfg.Schema, "jobs")+" set state=$2,updated_at=clock_timestamp() where scope=$1 and state not in ('complete','cancelled')", scope(cfg, s), state)
	if e != nil {
		return Status{}, e
	}
	if tag.RowsAffected() == 0 {
		return Status{}, fmt.Errorf("%w: job absent or terminal", ErrInvalid)
	}
	return StatusOf(ctx, cfg, s)
}
func StatusOf(ctx context.Context, cfg Config, s Spec) (Status, error) {
	cfg, e := defaults(cfg)
	if e != nil {
		return Status{}, e
	}
	if e = s.Validate(); e != nil {
		return Status{}, e
	}
	c, e := pgx.Connect(ctx, cfg.URL)
	if e != nil {
		return Status{}, e
	}
	defer c.Close(context.WithoutCancel(ctx))
	if _, e = c.Exec(ctx, "set lock_timeout="+lit(fmt.Sprintf("%dms", cfg.LockTimeoutMS))+";set statement_timeout="+lit(fmt.Sprintf("%dms", cfg.StatementTimeoutMS))); e != nil {
		return Status{}, e
	}
	if e = validateExisting(ctx, c, cfg); e != nil {
		return Status{}, e
	}
	var st Status
	st.JobID = s.JobID
	e = c.QueryRow(ctx, "select state,processed,remaining,retries,last_error,started_at,updated_at from "+q(cfg.Schema, "jobs")+" where scope=$1", scope(cfg, s)).Scan(&st.State, &st.Processed, &st.Remaining, &st.Retries, &st.LastError, &st.StartedAt, &st.UpdatedAt)
	if e != nil {
		return Status{}, e
	}
	elapsed := st.UpdatedAt.Sub(st.StartedAt).Seconds()
	if elapsed > 0 {
		st.ThroughputRowsPerSecond = float64(st.Processed) / elapsed
	}
	st.LagSeconds = int64(time.Since(st.UpdatedAt).Seconds())
	return st, nil
}
