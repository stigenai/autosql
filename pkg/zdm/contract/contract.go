// Package contract completes a zero-downtime migration behind explicit gates.
package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

const Version = "autosql.zdm.contract/v1"
const DefaultSchema = "autosql_zdm_contract"

var ErrRefused = errors.New("contract completion refused")
var ErrInvalid = errors.New("invalid contract plan")
var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

type Config struct {
	URL, Schema, Target, Environment string
	LockTimeoutMS                    int
}
type Step struct {
	ID, Summary, SQL, Recovery string
	CheckSQL                   string
	Transactional              bool
}
type Spec struct {
	Version, OperationID, ArtifactDigest, PreviousVersion, NewVersion string
	Steps                                                             []Step
	Digest                                                            string
}
type Evidence struct {
	OldVersionSessions                         int64
	BackfillsComplete, ChecksPassed, DriftFree bool
	Blockers                                   []string
}
type Approval struct {
	PlanDigest, Approver, Reason string
	ApprovedAt, ExpiresAt        time.Time
}
type Preview struct {
	Digest          string `json:"digest"`
	PreviousVersion string `json:"previous_version"`
	NewVersion      string `json:"new_version"`
	Steps           []Step `json:"steps"`
}
type Status struct {
	State                      string
	CompletedSteps, TotalSteps int
	CurrentStep                string
	Blockers, RecoveryActions  []string
	LastError                  string
	UpdatedAt                  time.Time
}
type Inspector func(context.Context) (Evidence, error)
type Executor func(context.Context, Step) error
type FinalVerifier func(context.Context) error

func New(operation, artifact, previous, next string, steps []Step) (Spec, error) {
	s := Spec{Version: Version, OperationID: operation, ArtifactDigest: artifact, PreviousVersion: previous, NewVersion: next, Steps: append([]Step(nil), steps...)}
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
	if s.Version != Version || !ident.MatchString(s.OperationID) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.ArtifactDigest) || !ident.MatchString(s.PreviousVersion) || !ident.MatchString(s.NewVersion) || s.PreviousVersion == s.NewVersion || len(s.Steps) == 0 {
		return fmt.Errorf("%w: identity, versions, artifact, or steps", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, x := range s.Steps {
		if !ident.MatchString(x.ID) || seen[x.ID] || x.Summary == "" || x.SQL == "" || x.CheckSQL == "" || x.Recovery == "" {
			return fmt.Errorf("%w: complete unique steps required", ErrInvalid)
		}
		seen[x.ID] = true
	}
	if digest(s) != s.Digest {
		return fmt.Errorf("%w: digest mismatch", ErrInvalid)
	}
	return nil
}
func PreviewOf(s Spec) (Preview, error) {
	if err := s.Validate(); err != nil {
		return Preview{}, err
	}
	return Preview{Digest: s.Digest, PreviousVersion: s.PreviousVersion, NewVersion: s.NewVersion, Steps: append([]Step(nil), s.Steps...)}, nil
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
	marker := "autosql:zdm:contract:v1"
	var exists bool
	var cm *string
	if err := c.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1),obj_description(to_regnamespace($1),'pg_namespace')`, cfg.Schema).Scan(&exists, &cm); err != nil {
		return err
	}
	if exists && (cm == nil || *cm != marker) {
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
		if _, err := c.Exec(ctx, "create table "+q(cfg.Schema, "operations")+`(scope text primary key,spec_digest text not null,state text not null,completed_steps integer not null,current_step text not null,last_error text not null,updated_at timestamptz not null)`); err != nil {
			return err
		}
		if _, err := c.Exec(ctx, "comment on table "+q(cfg.Schema, "operations")+" is '"+tableMarker+"'"); err != nil {
			return err
		}
	}
	return nil
}
func gate(e Evidence, a Approval, s Spec, now time.Time) []string {
	b := append([]string(nil), e.Blockers...)
	if e.OldVersionSessions != 0 {
		b = append(b, fmt.Sprintf("old version has %d active sessions", e.OldVersionSessions))
	}
	if !e.BackfillsComplete {
		b = append(b, "backfill incomplete")
	}
	if !e.ChecksPassed {
		b = append(b, "checks failed")
	}
	if !e.DriftFree {
		b = append(b, "compatibility or physical objects drifted")
	}
	if a.PlanDigest != s.Digest {
		b = append(b, "destructive approval is not bound to contract plan")
	}
	if a.Approver == "" || a.Reason == "" {
		b = append(b, "approver and reason required")
	}
	if a.ApprovedAt.After(now) || !a.ExpiresAt.After(now) {
		b = append(b, "destructive approval is not currently valid")
	}
	return b
}

func Complete(ctx context.Context, cfg Config, s Spec, inspect Inspector, execute Executor, verify FinalVerifier, a Approval) (Status, error) {
	var err error
	if cfg, err = defaults(cfg); err != nil {
		return Status{}, err
	}
	if err = s.Validate(); err != nil {
		return Status{}, err
	}
	if inspect == nil || execute == nil || verify == nil {
		return Status{}, fmt.Errorf("%w: inspector, executor, and final verifier required", ErrInvalid)
	}
	c, err := pgx.Connect(ctx, cfg.URL)
	if err != nil {
		return Status{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	var locked bool
	if err = c.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1,0::bigint))`, domain(cfg)).Scan(&locked); err != nil {
		return Status{}, err
	}
	if !locked {
		return Status{}, fmt.Errorf("%w: completion already active", ErrRefused)
	}
	defer c.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, domain(cfg))
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
	_, err = c.Exec(ctx, "insert into "+q(cfg.Schema, "operations")+` values($1,$2,'pending',0,'','',clock_timestamp()) on conflict(scope) do nothing`, sc, s.Digest)
	if err != nil {
		return Status{}, err
	}
	var saved, state string
	var done int
	if err = c.QueryRow(ctx, "select spec_digest,state,completed_steps from "+q(cfg.Schema, "operations")+" where scope=$1", sc).Scan(&saved, &state, &done); err != nil {
		return Status{}, err
	}
	if saved != s.Digest {
		return Status{}, fmt.Errorf("%w: different contract plan owns target", ErrRefused)
	}
	if state == "complete" {
		return StatusOf(ctx, cfg, s)
	}
	for i := done; i < len(s.Steps); i++ {
		ev, e := inspect(ctx)
		if e != nil {
			return Status{}, e
		}
		if blockers := gate(ev, a, s, time.Now().UTC()); len(blockers) > 0 {
			_, _ = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set state='blocked',current_step=$2,last_error='gates refused',updated_at=clock_timestamp() where scope=$1", sc, s.Steps[i].ID)
			st, _ := StatusOf(ctx, cfg, s)
			st.Blockers = blockers
			st.RecoveryActions = []string{"resolve every blocker", "collect a fresh approval for contract digest " + s.Digest, "retry completion"}
			return st, ErrRefused
		}
		var alreadyComplete bool
		if err = c.QueryRow(ctx, s.Steps[i].CheckSQL).Scan(&alreadyComplete); err != nil {
			return Status{}, fmt.Errorf("verify contract step %s precondition: %w", s.Steps[i].ID, err)
		}
		if _, err = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set state='running',current_step=$2,last_error='',updated_at=clock_timestamp() where scope=$1", sc, s.Steps[i].ID); err != nil {
			return Status{}, err
		}
		if !alreadyComplete {
			err = execute(ctx, s.Steps[i])
		}
		if err != nil {
			_, _ = c.Exec(context.WithoutCancel(ctx), "update "+q(cfg.Schema, "operations")+" set state='interrupted',last_error='contract step failed',updated_at=clock_timestamp() where scope=$1", sc)
			st, _ := StatusOf(context.WithoutCancel(ctx), cfg, s)
			st.RecoveryActions = []string{s.Steps[i].Recovery, "reinspect gates and retry completion"}
			return st, err
		}
		var postcondition bool
		if err = c.QueryRow(ctx, s.Steps[i].CheckSQL).Scan(&postcondition); err != nil || !postcondition {
			if err == nil {
				err = errors.New("approved step postcondition is false")
			}
			_, _ = c.Exec(context.WithoutCancel(ctx), "update "+q(cfg.Schema, "operations")+" set state='interrupted',last_error='contract step postcondition failed',updated_at=clock_timestamp() where scope=$1", sc)
			return Status{}, fmt.Errorf("verify contract step %s: %w", s.Steps[i].ID, err)
		}
		if _, err = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set completed_steps=$2,updated_at=clock_timestamp() where scope=$1", sc, i+1); err != nil {
			return Status{}, err
		}
	}
	if err = verify(ctx); err != nil {
		_, _ = c.Exec(context.WithoutCancel(ctx), "update "+q(cfg.Schema, "operations")+" set state='interrupted',current_step='final_inspection',last_error='final inspection failed',updated_at=clock_timestamp() where scope=$1", sc)
		st, _ := StatusOf(context.WithoutCancel(ctx), cfg, s)
		st.Blockers = []string{"final canonical inspection does not match the desired schema"}
		st.RecoveryActions = []string{"repair the reported final drift", "retry completion with the identical approved plan"}
		return st, err
	}
	_, err = c.Exec(ctx, "update "+q(cfg.Schema, "operations")+" set state='complete',current_step='',last_error='',updated_at=clock_timestamp() where scope=$1", sc)
	if err != nil {
		return Status{}, err
	}
	return StatusOf(ctx, cfg, s)
}

func StatusOf(ctx context.Context, cfg Config, s Spec) (Status, error) {
	var err error
	if cfg, err = defaults(cfg); err != nil {
		return Status{}, err
	}
	if err = s.Validate(); err != nil {
		return Status{}, err
	}
	c, err := pgx.Connect(ctx, cfg.URL)
	if err != nil {
		return Status{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	var st Status
	var saved string
	if err = c.QueryRow(ctx, "select spec_digest,state,completed_steps,current_step,last_error,updated_at from "+q(cfg.Schema, "operations")+" where scope=$1", scope(cfg)).Scan(&saved, &st.State, &st.CompletedSteps, &st.CurrentStep, &st.LastError, &st.UpdatedAt); err != nil {
		return Status{}, err
	}
	if saved != s.Digest {
		return Status{}, fmt.Errorf("%w: plan mismatch", ErrRefused)
	}
	st.TotalSteps = len(s.Steps)
	if st.State == "running" || st.State == "interrupted" {
		st.Blockers = []string{"contract step " + st.CurrentStep + " incomplete"}
		st.RecoveryActions = []string{"perform the step recovery action from the approved preview", "reinspect gates and retry completion"}
	}
	return st, nil
}
