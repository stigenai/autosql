// Package rollback withdraws active zero-downtime migrations and audits repairs.
package rollback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"regexp"
	"time"
)

const Version = "autosql.zdm.rollback/v1"
const DefaultSchema = "autosql_zdm_rollback"

var ErrRefused = errors.New("rollback refused")
var ErrInvalid = errors.New("invalid rollback request")
var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

type Config struct {
	URL, Schema, Target, Environment string
	LockTimeoutMS                    int
}
type Spec struct{ Version, OperationID, ArtifactDigest, PreviousVersion, NewVersion, Digest string }
type Eligibility struct {
	Active, PreviousWritable, Completed, Lossy, ReverseAvailable bool
	Blockers                                                     []string
}
type Authorization struct {
	Operator, Reason string
	AcknowledgeLossy bool
	At               time.Time
}
type Actions struct{ WithdrawNew, RemoveCompatibility, VerifyPrevious func(context.Context) error }
type Status struct {
	State, Phase                string
	PreviousVersion, NewVersion string
	Blockers, RecoveryActions   []string
	LastError                   string
	UpdatedAt                   time.Time
}

func New(op, artifact, previous, next string) (Spec, error) {
	s := Spec{Version: Version, OperationID: op, ArtifactDigest: artifact, PreviousVersion: previous, NewVersion: next}
	s.Digest = digest(s)
	return s, s.Validate()
}
func digest(s Spec) string {
	s.Digest = ""
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s Spec) Validate() error {
	if s.Version != Version || !ident.MatchString(s.OperationID) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.ArtifactDigest) || !ident.MatchString(s.PreviousVersion) || !ident.MatchString(s.NewVersion) || s.PreviousVersion == s.NewVersion || digest(s) != s.Digest {
		return fmt.Errorf("%w: specification", ErrInvalid)
	}
	return nil
}
func defaults(c Config) (Config, error) {
	if c.Schema == "" {
		c.Schema = DefaultSchema
	}
	if c.URL == "" || c.Target == "" || c.Environment == "" || !ident.MatchString(c.Schema) || c.LockTimeoutMS <= 0 {
		return c, fmt.Errorf("%w: config", ErrInvalid)
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
	marker := "autosql:zdm:rollback:v1"
	var exists bool
	var cm *string
	if e := c.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1),obj_description(to_regnamespace($1),'pg_namespace')`, cfg.Schema).Scan(&exists, &cm); e != nil {
		return e
	}
	if exists && (cm == nil || *cm != marker) {
		return fmt.Errorf("%w: control schema collision", ErrInvalid)
	}
	if !exists {
		if _, e := c.Exec(ctx, "create schema "+q(cfg.Schema)); e != nil {
			return e
		}
		if _, e := c.Exec(ctx, "comment on schema "+q(cfg.Schema)+" is '"+marker+"'"); e != nil {
			return e
		}
	}
	if _, e := c.Exec(ctx, "create table if not exists "+q(cfg.Schema, "operations")+`(scope text primary key,spec_digest text not null,state text not null,phase text not null,last_error text not null,updated_at timestamptz not null)`); e != nil {
		return e
	}
	_, e := c.Exec(ctx, "create table if not exists "+q(cfg.Schema, "audit")+`(sequence bigint generated always as identity primary key,event_type text not null,scope text not null,subject_digest text not null,operator text not null,reason text not null,detail jsonb not null,at timestamptz not null)`)
	return e
}
func eligibilityBlockers(e Eligibility, a Authorization) []string {
	b := append([]string(nil), e.Blockers...)
	if !e.Active {
		b = append(b, "migration is not active")
	}
	if e.Completed {
		b = append(b, "completed migration requires a forward repair, not rollback")
	}
	if !e.PreviousWritable {
		b = append(b, "previous version is not writable")
	}
	if e.Lossy && !e.ReverseAvailable && !a.AcknowledgeLossy {
		b = append(b, "lossy transformation risk requires explicit acknowledgement")
	}
	if a.Operator == "" || a.Reason == "" || a.At.IsZero() {
		b = append(b, "operator, reason, and authorization time required")
	}
	return b
}
func Rollback(ctx context.Context, cfg Config, s Spec, inspect func(context.Context) (Eligibility, error), actions Actions, a Authorization) (Status, error) {
	var e error
	if cfg, e = defaults(cfg); e != nil {
		return Status{}, e
	}
	if e = s.Validate(); e != nil {
		return Status{}, e
	}
	if inspect == nil || actions.WithdrawNew == nil || actions.RemoveCompatibility == nil || actions.VerifyPrevious == nil {
		return Status{}, fmt.Errorf("%w: inspection and actions required", ErrInvalid)
	}
	c, e := pgx.Connect(ctx, cfg.URL)
	if e != nil {
		return Status{}, e
	}
	defer c.Close(context.WithoutCancel(ctx))
	var locked bool
	if e = c.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1,0::bigint))`, domain(cfg)).Scan(&locked); e != nil {
		return Status{}, e
	}
	if !locked {
		return Status{}, fmt.Errorf("%w: another lifecycle action is active", ErrRefused)
	}
	defer c.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, domain(cfg))
	if e = ensure(ctx, c, cfg); e != nil {
		return Status{}, e
	}
	sc := scope(cfg)
	var existingDigest, existingState string
	e = c.QueryRow(ctx, "select spec_digest,state from "+q(cfg.Schema, "operations")+" where scope=$1", sc).Scan(&existingDigest, &existingState)
	if e == nil {
		if existingDigest != s.Digest {
			return Status{}, fmt.Errorf("%w: different operation owns target", ErrRefused)
		}
		if existingState == "complete" {
			return StatusOf(ctx, cfg, s)
		}
	} else if !errors.Is(e, pgx.ErrNoRows) {
		return Status{}, e
	}
	ev, e := inspect(ctx)
	if e != nil {
		return Status{}, e
	}
	if b := eligibilityBlockers(ev, a); len(b) > 0 {
		st := Status{State: "blocked", Phase: "eligibility", PreviousVersion: s.PreviousVersion, NewVersion: s.NewVersion, Blockers: b, RecoveryActions: []string{"resolve every blocker", "reinspect rollback eligibility", "retry with a reasoned authorization"}}
		return st, ErrRefused
	}
	_, e = c.Exec(ctx, "insert into "+q(cfg.Schema, "operations")+` values($1,$2,'running','withdraw_new','',clock_timestamp()) on conflict(scope) do nothing`, sc, s.Digest)
	if e != nil {
		return Status{}, e
	}
	var saved, state, phase string
	if e = c.QueryRow(ctx, "select spec_digest,state,phase from "+q(cfg.Schema, "operations")+" where scope=$1", sc).Scan(&saved, &state, &phase); e != nil {
		return Status{}, e
	}
	if saved != s.Digest {
		return Status{}, fmt.Errorf("%w: different operation owns target", ErrRefused)
	}
	if state == "complete" {
		return StatusOf(ctx, cfg, s)
	}
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{{"withdraw_new", actions.WithdrawNew}, {"remove_compatibility", actions.RemoveCompatibility}, {"verify_previous", actions.VerifyPrevious}}
	start := 0
	for i, x := range steps {
		if x.name == phase {
			start = i
		}
	}
	for i := start; i < len(steps); i++ {
		_, _ = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set state='running',phase=$2,updated_at=clock_timestamp() where scope=$1", sc, steps[i].name)
		if e = steps[i].fn(ctx); e != nil {
			cause := e
			_, _ = c.Exec(context.WithoutCancel(ctx), "update "+q(cfg.Schema, "operations")+" set state='interrupted',last_error='rollback phase failed',updated_at=clock_timestamp() where scope=$1", sc)
			st, _ := StatusOf(context.WithoutCancel(ctx), cfg, s)
			return st, cause
		}
		next := "complete"
		if i+1 < len(steps) {
			next = steps[i+1].name
		}
		_, e = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set phase=$2,updated_at=clock_timestamp() where scope=$1", sc, next)
		if e != nil {
			return Status{}, e
		}
	}
	detail, _ := json.Marshal(map[string]any{"lossy": ev.Lossy, "reverse_available": ev.ReverseAvailable, "backfill_reversed": false})
	if _, e = c.Exec(ctx, "insert into "+q(cfg.Schema, "audit")+"(event_type,scope,subject_digest,operator,reason,detail,at) values('rollback_completed',$1,$2,$3,$4,$5,clock_timestamp())", sc, s.Digest, a.Operator, a.Reason, detail); e != nil {
		return Status{}, e
	}
	_, e = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set state='complete',phase='complete',last_error='',updated_at=clock_timestamp() where scope=$1", sc)
	if e != nil {
		return Status{}, e
	}
	return StatusOf(ctx, cfg, s)
}
func StatusOf(ctx context.Context, cfg Config, s Spec) (Status, error) {
	var e error
	if cfg, e = defaults(cfg); e != nil {
		return Status{}, e
	}
	c, e := pgx.Connect(ctx, cfg.URL)
	if e != nil {
		return Status{}, e
	}
	defer c.Close(context.WithoutCancel(ctx))
	st := Status{PreviousVersion: s.PreviousVersion, NewVersion: s.NewVersion}
	var saved string
	if e = c.QueryRow(ctx, "select spec_digest,state,phase,last_error,updated_at from "+q(cfg.Schema, "operations")+" where scope=$1", scope(cfg)).Scan(&saved, &st.State, &st.Phase, &st.LastError, &st.UpdatedAt); e != nil {
		return Status{}, e
	}
	if saved != s.Digest {
		return Status{}, ErrRefused
	}
	if st.State == "interrupted" {
		st.Blockers = []string{"rollback phase " + st.Phase + " incomplete"}
		st.RecoveryActions = []string{"inspect the phase's owned objects", "retry rollback with identical specification"}
	}
	return st, nil
}

type RepairProposal struct {
	ID, SubjectDigest, Observed, Action, Expected string
	Digest                                        string
}

func NewRepair(id, subject, observed, action, expected string) (RepairProposal, error) {
	p := RepairProposal{ID: id, SubjectDigest: subject, Observed: observed, Action: action, Expected: expected}
	p.Digest = repairDigest(p)
	if !ident.MatchString(id) || subject == "" || observed == "" || action == "" || expected == "" {
		return RepairProposal{}, fmt.Errorf("%w: repair proposal", ErrInvalid)
	}
	return p, nil
}
func repairDigest(p RepairProposal) string {
	p.Digest = ""
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Repair(ctx context.Context, cfg Config, p RepairProposal, a Authorization, apply func(context.Context) error, verify func(context.Context) (string, error)) error {
	var e error
	if cfg, e = defaults(cfg); e != nil {
		return e
	}
	if repairDigest(p) != p.Digest || a.Operator == "" || a.Reason == "" || apply == nil || verify == nil {
		return fmt.Errorf("%w: repair binding", ErrInvalid)
	}
	c, e := pgx.Connect(ctx, cfg.URL)
	if e != nil {
		return e
	}
	defer c.Close(context.WithoutCancel(ctx))
	if e = ensure(ctx, c, cfg); e != nil {
		return e
	}
	before, e := verify(ctx)
	if e != nil {
		return e
	}
	if before != p.Observed {
		return fmt.Errorf("%w: observed state changed", ErrRefused)
	}
	detail, _ := json.Marshal(map[string]string{"observed": p.Observed, "action": p.Action, "expected": p.Expected})
	if _, e = c.Exec(ctx, "insert into "+q(cfg.Schema, "audit")+"(event_type,scope,subject_digest,operator,reason,detail,at) values('repair_requested',$1,$2,$3,$4,$5,clock_timestamp())", scope(cfg), p.Digest, a.Operator, a.Reason, detail); e != nil {
		return e
	}
	if e = apply(ctx); e != nil {
		return e
	}
	after, e := verify(ctx)
	if e != nil {
		return e
	}
	if after != p.Expected {
		return fmt.Errorf("%w: repair postcondition mismatch", ErrRefused)
	}
	_, e = c.Exec(ctx, "insert into "+q(cfg.Schema, "audit")+"(event_type,scope,subject_digest,operator,reason,detail,at) values('repair_completed',$1,$2,$3,$4,$5,clock_timestamp())", scope(cfg), p.Digest, a.Operator, a.Reason, detail)
	return e
}
