// Package start coordinates the idempotent start of a zero-downtime migration.
package start

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

const Version = "autosql.zdm.start/v1"
const DefaultSchema = "autosql_zdm_start"

var ErrInvalid = errors.New("invalid migration start")
var ErrBusy = errors.New("migration start already active")
var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

var phases = []string{"validate", "record_intent", "expand", "compatibility", "backfill", "publish"}

type Config struct {
	URL, Schema, Target, Environment string
	LockTimeoutMS                    int
}

// Spec binds durable state to the exact migration artifact and versions.
type Spec struct {
	Version         string `json:"version"`
	OperationID     string `json:"operation_id"`
	ArtifactDigest  string `json:"artifact_digest"`
	PreviousVersion string `json:"previous_version"`
	NewVersion      string `json:"new_version"`
	Digest          string `json:"digest"`
}

// Actions are deliberately capability-shaped: adapters call the already
// idempotent expand, virtual-schema, shadow-sync, and backfill packages.
type Actions struct {
	Validate, Expand, Compatibility, Backfill, Publish func(context.Context) error
}

type Hooks struct{ AfterPhase func(string) error }

type Status struct {
	OperationID     string    `json:"operation_id"`
	Phase           string    `json:"phase"`
	State           string    `json:"state"`
	PreviousVersion string    `json:"previous_version"`
	NewVersion      string    `json:"new_version"`
	Progress        int       `json:"progress_percent"`
	Blockers        []string  `json:"blockers,omitempty"`
	RecoveryActions []string  `json:"recovery_actions,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func ParseJSON(b []byte) (Spec, error) {
	var s Spec
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return Spec{}, err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return Spec{}, fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	return s, s.Validate()
}

func New(operation, artifact, previous, next string) (Spec, error) {
	s := Spec{Version: Version, OperationID: operation, ArtifactDigest: artifact, PreviousVersion: previous, NewVersion: next}
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	s.Digest = hex.EncodeToString(h[:])
	return s, s.Validate()
}

func (s Spec) Validate() error {
	if s.Version != Version || !ident.MatchString(s.OperationID) || !ident.MatchString(s.PreviousVersion) || !ident.MatchString(s.NewVersion) || s.PreviousVersion == s.NewVersion || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.ArtifactDigest) {
		return fmt.Errorf("%w: identity or versions", ErrInvalid)
	}
	d := s.Digest
	s.Digest = ""
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	if d != hex.EncodeToString(h[:]) {
		return fmt.Errorf("%w: digest mismatch", ErrInvalid)
	}
	return nil
}

func defaults(c Config) (Config, error) {
	if c.Schema == "" {
		c.Schema = DefaultSchema
	}
	if c.URL == "" || c.Target == "" || c.Environment == "" || !ident.MatchString(c.Schema) || c.LockTimeoutMS <= 0 {
		return c, fmt.Errorf("%w: configuration", ErrInvalid)
	}
	return c, nil
}

func q(v ...string) string { return pgx.Identifier(v).Sanitize() }
func scope(c Config) string {
	b, _ := json.Marshal([]string{c.Target, c.Environment})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func domain(c Config) string { return Version + "/" + c.Schema + "/" + scope(c) }

func ensure(ctx context.Context, c *pgx.Conn, cfg Config) error {
	marker := "autosql:zdm:start:v1"
	var exists bool
	var comment *string
	if err := c.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1),obj_description(to_regnamespace($1),'pg_namespace')`, cfg.Schema).Scan(&exists, &comment); err != nil {
		return err
	}
	if exists && (comment == nil || *comment != marker) {
		return fmt.Errorf("%w: control schema collision", ErrInvalid)
	}
	if !exists {
		if _, err := c.Exec(ctx, "create schema "+q(cfg.Schema)); err != nil {
			return err
		}
		if _, err := c.Exec(ctx, "comment on schema "+q(cfg.Schema)+" is '"+marker+"'"); err != nil {
			return err
		}
	}
	var tableExists bool
	var tableComment *string
	if err := c.QueryRow(ctx, `select to_regclass($1) is not null,obj_description(to_regclass($1),'pg_class')`, cfg.Schema+".operations").Scan(&tableExists, &tableComment); err != nil {
		return err
	}
	tableMarker := marker + ":operations"
	if tableExists && (tableComment == nil || *tableComment != tableMarker) {
		return fmt.Errorf("%w: operations table collision", ErrInvalid)
	}
	if !tableExists {
		if _, err := c.Exec(ctx, "create table "+q(cfg.Schema, "operations")+`(scope text primary key,operation_id text not null,spec_digest text not null,artifact_digest text not null,previous_version text not null,new_version text not null,phase text not null,state text not null,progress integer not null,last_error text not null default '',started_at timestamptz not null,updated_at timestamptz not null)`); err != nil {
			return err
		}
		if _, err := c.Exec(ctx, "comment on table "+q(cfg.Schema, "operations")+" is '"+tableMarker+"'"); err != nil {
			return err
		}
	}
	return nil
}

func Start(ctx context.Context, cfg Config, spec Spec, actions Actions) (Status, error) {
	return StartWithHooks(ctx, cfg, spec, actions, Hooks{})
}

func StartWithHooks(ctx context.Context, cfg Config, spec Spec, actions Actions, hooks Hooks) (Status, error) {
	var err error
	if cfg, err = defaults(cfg); err != nil {
		return Status{}, err
	}
	if err = spec.Validate(); err != nil {
		return Status{}, err
	}
	if actions.Validate == nil || actions.Expand == nil || actions.Compatibility == nil || actions.Backfill == nil || actions.Publish == nil {
		return Status{}, fmt.Errorf("%w: all phase actions required", ErrInvalid)
	}
	c, err := pgx.Connect(ctx, cfg.URL)
	if err != nil {
		return Status{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	if _, err = c.Exec(ctx, fmt.Sprintf("set lock_timeout='%dms'", cfg.LockTimeoutMS)); err != nil {
		return Status{}, err
	}
	var locked bool
	if err = c.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1,0::bigint))`, domain(cfg)).Scan(&locked); err != nil {
		return Status{}, err
	}
	if !locked {
		return Status{}, ErrBusy
	}
	defer c.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, domain(cfg))
	// Initialization is shared by every target using this control schema. Keep
	// its small DDL section behind a separate schema-scoped lock.
	initDomain := Version + "/" + cfg.Schema + "/init"
	if _, err = c.Exec(ctx, `select pg_advisory_lock(hashtextextended($1,0::bigint))`, initDomain); err != nil {
		return Status{}, err
	}
	if err = ensure(ctx, c, cfg); err != nil {
		_, _ = c.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, initDomain)
		return Status{}, err
	}
	if _, err = c.Exec(ctx, `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, initDomain); err != nil {
		return Status{}, err
	}

	sc := scope(cfg)
	_, err = c.Exec(ctx, "insert into "+q(cfg.Schema, "operations")+`(scope,operation_id,spec_digest,artifact_digest,previous_version,new_version,phase,state,progress,started_at,updated_at) values($1,$2,$3,$4,$5,$6,'validate','pending',0,clock_timestamp(),clock_timestamp()) on conflict(scope) do nothing`, sc, spec.OperationID, spec.Digest, spec.ArtifactDigest, spec.PreviousVersion, spec.NewVersion)
	if err != nil {
		return Status{}, err
	}
	var digest, state string
	var progress int
	if err = c.QueryRow(ctx, "select spec_digest,state,progress from "+q(cfg.Schema, "operations")+" where scope=$1", sc).Scan(&digest, &state, &progress); err != nil {
		return Status{}, err
	}
	if digest != spec.Digest {
		return Status{}, fmt.Errorf("%w: another migration owns target/environment", ErrInvalid)
	}
	if state == "complete" {
		return StatusOf(ctx, cfg, spec)
	}

	fs := []func(context.Context) error{actions.Validate, func(context.Context) error { return nil }, actions.Expand, actions.Compatibility, actions.Backfill, actions.Publish}
	for i := progress; i < len(phases); i++ {
		phase := phases[i]
		if _, err = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set phase=$2,state='running',last_error='',updated_at=clock_timestamp() where scope=$1", sc, phase); err != nil {
			return Status{}, err
		}
		if err = fs[i](ctx); err != nil {
			msg := classify(err)
			_, _ = c.Exec(context.WithoutCancel(ctx), "update "+q(cfg.Schema, "operations")+" set state='interrupted',last_error=$2,updated_at=clock_timestamp() where scope=$1", sc, msg)
			st, _ := StatusOf(context.WithoutCancel(ctx), cfg, spec)
			return st, err
		}
		if _, err = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set progress=$2,updated_at=clock_timestamp() where scope=$1", sc, i+1); err != nil {
			return Status{}, err
		}
		if hooks.AfterPhase != nil {
			if err = hooks.AfterPhase(phase); err != nil {
				_, _ = c.Exec(context.WithoutCancel(ctx), "update "+q(cfg.Schema, "operations")+" set state='interrupted',last_error=$2,updated_at=clock_timestamp() where scope=$1", sc, "orchestrator interrupted after "+phase)
				st, _ := StatusOf(context.WithoutCancel(ctx), cfg, spec)
				return st, err
			}
		}
	}
	_, err = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set phase='complete',state='complete',progress=$2,last_error='',updated_at=clock_timestamp() where scope=$1", sc, len(phases))
	if err != nil {
		return Status{}, err
	}
	return StatusOf(ctx, cfg, spec)
}

func classify(error) string {
	return "phase action failed; retry start with the identical specification"
}

func StatusOf(ctx context.Context, cfg Config, spec Spec) (Status, error) {
	var err error
	if cfg, err = defaults(cfg); err != nil {
		return Status{}, err
	}
	if err = spec.Validate(); err != nil {
		return Status{}, err
	}
	c, err := pgx.Connect(ctx, cfg.URL)
	if err != nil {
		return Status{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	var st Status
	var done int
	var digest string
	err = c.QueryRow(ctx, "select operation_id,spec_digest,phase,state,progress,previous_version,new_version,last_error,started_at,updated_at from "+q(cfg.Schema, "operations")+" where scope=$1", scope(cfg)).Scan(&st.OperationID, &digest, &st.Phase, &st.State, &done, &st.PreviousVersion, &st.NewVersion, &st.LastError, &st.StartedAt, &st.UpdatedAt)
	if err != nil {
		return Status{}, err
	}
	if digest != spec.Digest {
		return Status{}, fmt.Errorf("%w: specification mismatch", ErrInvalid)
	}
	st.Progress = done * 100 / len(phases)
	if st.State == "running" || st.State == "interrupted" {
		st.Blockers = []string{"phase " + st.Phase + " is incomplete"}
		st.RecoveryActions = []string{"retry start with the identical specification"}
	}
	if st.Phase == "backfill" {
		st.RecoveryActions = []string{"inspect backfill status", "resume or cancel the backfill", "retry start with the identical specification"}
	}
	return st, nil
}
