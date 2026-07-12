// Package zdm owns the isolated PostgreSQL control plane for zero-downtime migrations.
package zdm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"github.com/jackc/pgx/v5"
)

const CurrentSchemaVersion = 3
const DefaultSchema = "autosql_zdm"

var (
	ErrIncompatible = errors.New("incompatible zero-downtime metadata")
	ErrCorrupt      = errors.New("corrupt zero-downtime metadata")
	ErrConflict     = errors.New("zero-downtime metadata conflict")
	identifier      = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

type Config struct{ URL, Schema string }
type Store struct{ cfg Config }

// InitHooks is intended for deterministic crash/rollback verification. A hook
// error aborts the complete initialization transaction.
type InitHooks struct{ AfterVersion func(int) error }

type Status struct {
	Initialized      bool      `json:"initialized"`
	Schema           string    `json:"schema"`
	SchemaVersion    int       `json:"schema_version"`
	ActiveVersion    string    `json:"active_version,omitempty"`
	CompletedVersion string    `json:"completed_version,omitempty"`
	Phase            string    `json:"phase,omitempty"`
	Progress         int       `json:"progress,omitempty"`
	RecoveryState    string    `json:"recovery_state,omitempty"`
	Baseline         *Baseline `json:"baseline,omitempty"`
}

type Baseline struct {
	ID              string          `json:"id"`
	Target          string          `json:"target"`
	Environment     string          `json:"environment"`
	Fingerprint     string          `json:"fingerprint"`
	CanonicalSchema json.RawMessage `json:"canonical_schema,omitempty"`
	Operator        string          `json:"operator"`
	CreatedAt       time.Time       `json:"created_at"`
}

type BaselineRequest struct {
	ID, Target, Environment, Operator, ExpectedFingerprint string
	Schemas                                                []string
}
type BaselineHooks struct{ BeforeFinalInspection func() error }

func Open(c Config) (*Store, error) {
	if c.URL == "" {
		return nil, errors.New("metadata URL is required")
	}
	if c.Schema == "" {
		c.Schema = DefaultSchema
	}
	if !identifier.MatchString(c.Schema) {
		return nil, errors.New("invalid metadata schema")
	}
	return &Store{cfg: c}, nil
}

func q(s ...string) string                                      { return pgx.Identifier(s).Sanitize() }
func (s *Store) connect(ctx context.Context) (*pgx.Conn, error) { return pgx.Connect(ctx, s.cfg.URL) }
func lockKey(schema string) string                              { return "autosql.zdm.metadata/v1/" + schema }

// Init creates or transactionally upgrades the reserved control namespace.
// It requires only CONNECT and CREATE on the current database; no superuser or
// server-wide role is assumed.
func (s *Store) Init(ctx context.Context) error {
	return s.InitWithHooks(ctx, InitHooks{})
}

func (s *Store) InitWithHooks(ctx context.Context, hooks InitHooks) error {
	c, err := s.connect(ctx)
	if err != nil {
		return fmt.Errorf("connect metadata database: %w", err)
	}
	defer c.Close(context.WithoutCancel(ctx))
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1,0::bigint))`, lockKey(s.cfg.Schema)); err != nil {
		return err
	}
	var exists bool
	err = tx.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1)`, s.cfg.Schema).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(ctx, `create schema `+q(s.cfg.Schema)); err != nil {
			return fmt.Errorf("create reserved metadata schema (grant CREATE on database to this role): %w", err)
		}
		if err = s.createV1(ctx, tx); err != nil {
			return err
		}
		if hooks.AfterVersion != nil {
			if err = hooks.AfterVersion(1); err != nil {
				return err
			}
		}
	} else {
		var meta *string
		if err = tx.QueryRow(ctx, `select to_regclass($1)::text`, s.cfg.Schema+".meta").Scan(&meta); err != nil {
			return err
		}
		if meta == nil {
			return fmt.Errorf("%w: reserved schema %s exists without meta table; rename it or restore metadata from audit backup", ErrCorrupt, s.cfg.Schema)
		}
	}
	version, err := s.readVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version < 1 || version > CurrentSchemaVersion {
		return fmt.Errorf("%w: schema version %d; use a compatible AutoSQL release or restore supported metadata", ErrIncompatible, version)
	}
	for version < CurrentSchemaVersion {
		version++
		if err = s.upgrade(ctx, tx, version); err != nil {
			return fmt.Errorf("%w: supported metadata upgrade to v%d failed: %v; restore the last audited schema before retry", ErrCorrupt, version, err)
		}
		if hooks.AfterVersion != nil {
			if err = hooks.AfterVersion(version); err != nil {
				return err
			}
		}
	}
	if err = s.validateLayout(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) createV1(ctx context.Context, tx pgx.Tx) error {
	ddl := []string{
		`create table ` + q(s.cfg.Schema, "meta") + ` (singleton boolean primary key default true check(singleton),schema_version integer not null,active_version text not null default '',completed_version text not null default '',phase text not null default '',progress integer not null default 0 check(progress between 0 and 100),recovery_state text not null default 'clean',updated_at timestamptz not null)`,
		`insert into ` + q(s.cfg.Schema, "meta") + ` values(true,1,'','','',0,'clean',clock_timestamp())`,
		`create table ` + q(s.cfg.Schema, "operations") + ` (operation_id text primary key,version text not null,phase text not null,progress integer not null check(progress between 0 and 100),state text not null,recovery_state text not null,created_at timestamptz not null,updated_at timestamptz not null)`,
		`create table ` + q(s.cfg.Schema, "object_mappings") + ` (operation_id text not null references ` + q(s.cfg.Schema, "operations") + `,logical_id text not null,physical_schema text not null,physical_name text not null,object_kind text not null,primary key(operation_id,logical_id))`,
	}
	for _, sql := range ddl {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("initialize metadata: %w", err)
		}
	}
	return nil
}

func (s *Store) upgrade(ctx context.Context, tx pgx.Tx, to int) error {
	var ddl []string
	switch to {
	case 2:
		ddl = []string{`create table ` + q(s.cfg.Schema, "baselines") + ` (baseline_id text primary key,target_identity text not null,environment text not null,fingerprint text not null,canonical_schema jsonb not null,operator_identity text not null,created_at timestamptz not null,unique(target_identity,environment))`, `create table ` + q(s.cfg.Schema, "audit") + ` (sequence bigint generated always as identity primary key,event_type text not null,subject_id text not null,target_identity text not null,environment text not null,fingerprint text not null,operator_identity text not null,detail jsonb not null,at timestamptz not null)`, `update ` + q(s.cfg.Schema, "meta") + ` set schema_version=2,updated_at=clock_timestamp() where singleton`}
	case 3:
		ddl = []string{`alter table ` + q(s.cfg.Schema, "baselines") + ` alter column canonical_schema type text using canonical_schema::text`, `update ` + q(s.cfg.Schema, "meta") + ` set schema_version=3,updated_at=clock_timestamp() where singleton`}
	default:
		return fmt.Errorf("%w: no upgrade path to %d", ErrIncompatible, to)
	}
	for _, sql := range ddl {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("upgrade metadata to v%d: %w", to, err)
		}
	}
	return nil
}

func (s *Store) readVersion(ctx context.Context, qry interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (int, error) {
	var v int
	err := qry.QueryRow(ctx, `select schema_version from `+q(s.cfg.Schema, "meta")+` where singleton=true`).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: meta singleton is missing; restore it from an audited backup", ErrCorrupt)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: read meta: %v", ErrCorrupt, err)
	}
	return v, nil
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	c, err := s.connect(ctx)
	if err != nil {
		return Status{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	var exists bool
	if err = c.QueryRow(ctx, `select to_regclass($1) is not null`, s.cfg.Schema+".meta").Scan(&exists); err != nil {
		return Status{}, err
	}
	if !exists {
		return Status{Schema: s.cfg.Schema}, nil
	}
	v, err := s.readVersion(ctx, c)
	if err != nil {
		return Status{}, err
	}
	if v > CurrentSchemaVersion {
		return Status{}, fmt.Errorf("%w: schema version %d is newer than supported %d", ErrIncompatible, v, CurrentSchemaVersion)
	}
	if v < CurrentSchemaVersion {
		return Status{}, fmt.Errorf("%w: schema version %d requires explicit migrate metadata-init upgrade to %d", ErrIncompatible, v, CurrentSchemaVersion)
	}
	if v == CurrentSchemaVersion {
		if err = s.validateLayout(ctx, c); err != nil {
			return Status{}, err
		}
	}
	st := Status{Initialized: true, Schema: s.cfg.Schema, SchemaVersion: v}
	if err = c.QueryRow(ctx, `select active_version,completed_version,phase,progress,recovery_state from `+q(s.cfg.Schema, "meta")+` where singleton`).Scan(&st.ActiveVersion, &st.CompletedVersion, &st.Phase, &st.Progress, &st.RecoveryState); err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if v >= 2 {
		var b Baseline
		var raw []byte
		err = c.QueryRow(ctx, `select baseline_id,target_identity,environment,fingerprint,canonical_schema,operator_identity,created_at from `+q(s.cfg.Schema, "baselines")+` order by created_at desc limit 1`).Scan(&b.ID, &b.Target, &b.Environment, &b.Fingerprint, &raw, &b.Operator, &b.CreatedAt)
		if err == nil {
			b.CanonicalSchema = raw
			st.Baseline = &b
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Status{}, err
		}
	}
	return st, nil
}

func (s *Store) Baseline(ctx context.Context, r BaselineRequest) (Baseline, error) {
	return s.BaselineWithHooks(ctx, r, BaselineHooks{})
}

func (s *Store) BaselineWithHooks(ctx context.Context, r BaselineRequest, hooks BaselineHooks) (Baseline, error) {
	if r.ID == "" || r.Target == "" || r.Environment == "" || r.Operator == "" || len(r.Schemas) == 0 {
		return Baseline{}, errors.New("baseline ID, target, environment, operator, and application schemas are required")
	}
	for _, name := range r.Schemas {
		if !identifier.MatchString(name) || name == s.cfg.Schema {
			return Baseline{}, errors.New("invalid or reserved application schema")
		}
	}
	c, err := s.connect(ctx)
	if err != nil {
		return Baseline{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	// READ COMMITTED deliberately gives the final catalog reinspection a new
	// snapshot. Relation locks fence changes to existing objects; cooperating
	// object creation is ordered by the target advisory lock.
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Baseline{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1,0::bigint))`, lockKey(s.cfg.Schema)); err != nil {
		return Baseline{}, err
	}
	v, err := s.readVersion(ctx, tx)
	if err != nil {
		return Baseline{}, err
	}
	if v != CurrentSchemaVersion {
		return Baseline{}, fmt.Errorf("%w: run metadata init/upgrade first", ErrIncompatible)
	}
	if err = s.validateLayout(ctx, tx); err != nil {
		return Baseline{}, err
	}
	var active, recovery string
	if err = tx.QueryRow(ctx, `select active_version,recovery_state from `+q(s.cfg.Schema, "meta")+` where singleton for update`).Scan(&active, &recovery); err != nil {
		return Baseline{}, err
	}
	if active != "" || recovery != "clean" {
		return Baseline{}, fmt.Errorf("%w: baseline refused while active version or recovery state exists", ErrConflict)
	}
	var operations int
	if err = tx.QueryRow(ctx, `select count(*) from `+q(s.cfg.Schema, "operations")+` where state not in ('completed','cancelled')`).Scan(&operations); err != nil {
		return Baseline{}, err
	}
	if operations != 0 {
		return Baseline{}, fmt.Errorf("%w: baseline refused with unfinished operations", ErrConflict)
	}
	if err = s.lockApplicationRelations(ctx, tx, r.Schemas); err != nil {
		return Baseline{}, err
	}
	doc, err := postgres.InspectTx(ctx, tx, postgres.Options{Schemas: r.Schemas})
	if err != nil {
		return Baseline{}, fmt.Errorf("inspect baseline schema: %w", err)
	}
	canonical, err := doc.MarshalCanonical()
	if err != nil {
		return Baseline{}, err
	}
	fp, err := schema.SemanticFingerprint(doc)
	if err != nil {
		return Baseline{}, err
	}
	if r.ExpectedFingerprint != "" && r.ExpectedFingerprint != fp {
		return Baseline{}, fmt.Errorf("%w: live schema fingerprint %s does not match expected %s", ErrConflict, fp, r.ExpectedFingerprint)
	}
	b := Baseline{ID: r.ID, Target: r.Target, Environment: r.Environment, Fingerprint: fp, CanonicalSchema: canonical, Operator: r.Operator}
	var existing Baseline
	var raw []byte
	err = tx.QueryRow(ctx, `select baseline_id,target_identity,environment,fingerprint,canonical_schema,operator_identity,created_at from `+q(s.cfg.Schema, "baselines")+` where target_identity=$1 and environment=$2`, r.Target, r.Environment).Scan(&existing.ID, &existing.Target, &existing.Environment, &existing.Fingerprint, &raw, &existing.Operator, &existing.CreatedAt)
	if err == nil {
		existing.CanonicalSchema = raw
		var stored schema.Document
		storedFP := ""
		if json.Unmarshal(raw, &stored) == nil {
			storedFP, _ = schema.SemanticFingerprint(stored)
		}
		if existing.ID != r.ID || existing.Target != r.Target || existing.Environment != r.Environment || existing.Operator != r.Operator || existing.Fingerprint != fp || storedFP != existing.Fingerprint || !bytes.Equal(existing.CanonicalSchema, canonical) {
			return Baseline{}, fmt.Errorf("%w: existing baseline is stale or live schema drifted; diagnose before retry", ErrConflict)
		}
		if err = s.validateBaselineAudit(ctx, tx, existing); err != nil {
			return Baseline{}, err
		}
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Baseline{}, err
	}
	if hooks.BeforeFinalInspection != nil {
		if err = hooks.BeforeFinalInspection(); err != nil {
			return Baseline{}, err
		}
	}
	finalDoc, err := postgres.InspectTx(ctx, tx, postgres.Options{Schemas: r.Schemas})
	if err != nil {
		return Baseline{}, fmt.Errorf("final baseline inspection: %w", err)
	}
	finalCanonical, err := finalDoc.MarshalCanonical()
	if err != nil {
		return Baseline{}, err
	}
	finalFP, err := schema.SemanticFingerprint(finalDoc)
	if err != nil {
		return Baseline{}, err
	}
	if finalFP != fp || !bytes.Equal(finalCanonical, canonical) {
		return Baseline{}, fmt.Errorf("%w: application schema changed during baseline; retry from a stable state", ErrConflict)
	}
	detail := baselineAuditDetail(b)
	err = tx.QueryRow(ctx, `insert into `+q(s.cfg.Schema, "baselines")+`(baseline_id,target_identity,environment,fingerprint,canonical_schema,operator_identity,created_at) values($1,$2,$3,$4,$5,$6,clock_timestamp()) returning created_at`, r.ID, r.Target, r.Environment, fp, canonical, r.Operator).Scan(&b.CreatedAt)
	if err != nil {
		return Baseline{}, fmt.Errorf("record baseline: %w", err)
	}
	if _, err = tx.Exec(ctx, `insert into `+q(s.cfg.Schema, "audit")+`(event_type,subject_id,target_identity,environment,fingerprint,operator_identity,detail,at) values('baseline_recorded',$1,$2,$3,$4,$5,$6::jsonb,clock_timestamp())`, r.ID, r.Target, r.Environment, fp, r.Operator, detail); err != nil {
		return Baseline{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Baseline{}, err
	}
	return b, nil
}

func baselineAuditDetail(b Baseline) []byte {
	digest := sha256.Sum256([]byte(b.ID + "\x00" + b.Target + "\x00" + b.Environment + "\x00" + b.Operator + "\x00" + b.Fingerprint + "\x00" + string(b.CanonicalSchema)))
	raw, _ := json.Marshal(map[string]string{"evidence_digest": hex.EncodeToString(digest[:]), "schema_version": fmt.Sprint(CurrentSchemaVersion)})
	return raw
}

func (s *Store) validateBaselineAudit(ctx context.Context, tx pgx.Tx, b Baseline) error {
	var count int
	if err := tx.QueryRow(ctx, `select count(*) from `+q(s.cfg.Schema, "audit")+` where event_type='baseline_recorded' and subject_id=$1`, b.ID).Scan(&count); err != nil || count != 1 {
		return fmt.Errorf("%w: baseline audit evidence missing or ambiguous", ErrConflict)
	}
	var event, subject, target, env, fp, operator string
	var detail []byte
	var at time.Time
	err := tx.QueryRow(ctx, `select event_type,subject_id,target_identity,environment,fingerprint,operator_identity,detail::text,at from `+q(s.cfg.Schema, "audit")+` where subject_id=$1`, b.ID).Scan(&event, &subject, &target, &env, &fp, &operator, &detail, &at)
	if err != nil {
		return fmt.Errorf("%w: baseline audit evidence missing or ambiguous", ErrConflict)
	}
	got, want := map[string]string{}, map[string]string{}
	_ = json.Unmarshal(detail, &got)
	_ = json.Unmarshal(baselineAuditDetail(b), &want)
	if len(got) != len(want) || got["evidence_digest"] != want["evidence_digest"] || got["schema_version"] != want["schema_version"] || event != "baseline_recorded" || subject != b.ID || target != b.Target || env != b.Environment || fp != b.Fingerprint || operator != b.Operator || at.Before(b.CreatedAt) {
		return fmt.Errorf("%w: baseline audit evidence is inconsistent or tampered", ErrConflict)
	}
	return nil
}

func (s *Store) lockApplicationRelations(ctx context.Context, tx pgx.Tx, schemas []string) error {
	rows, err := tx.Query(ctx, `select n.nspname,c.relname from pg_class c join pg_namespace n on n.oid=c.relnamespace where n.nspname=any($1) and c.relkind in ('r','p','v','m','f') order by n.nspname,c.relname`, schemas)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var ns, name string
		if err = rows.Scan(&ns, &name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, q(ns, name))
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(names) > 0 {
		if _, err = tx.Exec(ctx, `lock table `+strings.Join(names, ",")+` in access share mode`); err != nil {
			return fmt.Errorf("lock application relations for baseline: %w", err)
		}
	}
	return nil
}
