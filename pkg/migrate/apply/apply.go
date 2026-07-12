// Package apply performs trusted, bounded migration application on one pinned
// PostgreSQL session. Revision state, executor evidence and transactional DDL
// are committed together; ambiguous non-transactional outcomes fail closed.
package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/plan"
	"github.com/jackc/pgx/v5"
)

var ErrRefused = errors.New("versioned migration apply refused")
var ErrBusy = errors.New("versioned migration target is locked")
var ErrUncertain = errors.New("migration outcome is uncertain; reconcile before retry")

type SessionStore interface {
	OpenSession(context.Context) (*revision.Session, error)
}
type VerifyArtifact func(artifact.Artifact) (artifact.VerifiedArtifact, error)
type GuardedApply func(context.Context, artifact.VerifiedArtifact, executor.Session, executor.Tx) (executor.ExternalExecution, error)
type Request struct {
	Directory                string
	Snapshot                 migrate.Snapshot // tests/offline callers; production should use Directory
	From, To                 string
	Count                    *int
	DryRun, Baseline         bool
	Transaction              string
	Operator, TargetIdentity string
	Now                      func() time.Time
}
type FileResult struct {
	Version, File, Status string
	Statements            int
	Duration              time.Duration
	finalize              func(context.Context, bool) error
}
type Failure struct {
	Version, File                                 string `json:",omitempty"`
	FilePosition, StatementPosition, Line, Column int    `json:",omitempty"`
	Recovery                                      string `json:",omitempty"`
}
type Result struct {
	Status       string        `json:"status"`
	Files        []string      `json:"files"`
	FileResults  []FileResult  `json:"file_results,omitempty"`
	Statements   int           `json:"statements"`
	Duration     time.Duration `json:"duration"`
	FinalVersion string        `json:"final_version,omitempty"`
	Failure      *Failure      `json:"failure,omitempty"`
	BackendPID   int32         `json:"backend_pid,omitempty"`
}
type candidate struct {
	entry    migrate.Migration
	raw      []byte
	verified artifact.VerifiedArtifact
	payload  artifact.Artifact
}
type Engine struct {
	Store  SessionStore
	Verify VerifyArtifact
	Apply  GuardedApply
}

func (e Engine) Run(ctx context.Context, r Request) (out Result, err error) {
	if e.Store == nil || e.Verify == nil || (!r.DryRun && !r.Baseline && e.Apply == nil) || r.Operator == "" || (r.Transaction != "file" && r.Transaction != "all") || r.DryRun && r.Baseline || r.Count != nil && *r.Count < 0 {
		return out, ErrRefused
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	s, err := e.Store.OpenSession(ctx)
	if err != nil {
		return out, err
	}
	defer s.Close(context.WithoutCancel(ctx))
	pre := r.Snapshot
	if r.Directory != "" {
		pre, err = migrate.LoadSnapshot(r.Directory)
		if err != nil {
			return out, ErrRefused
		}
	}
	identity, environment, err := trustedTarget(pre, e.Verify)
	if err != nil {
		return out, err
	}
	lockKey, err := executor.LockKey(identity, environment)
	if err != nil {
		return out, ErrRefused
	}
	locked, err := s.Lock(ctx, lockKey)
	if err != nil {
		return out, errors.New("acquire migration lock")
	}
	if !locked {
		return out, ErrBusy
	}
	defer func() {
		if u := s.Unlock(context.WithoutCancel(ctx), lockKey); u != nil && err == nil {
			err = errors.Join(ErrUncertain, u)
		}
	}()
	out.BackendPID, _ = s.BackendPID(ctx)
	// Reload only after acquiring the target lock. This closes the TOCTOU window
	// between directory verification and revision selection.
	snap := r.Snapshot
	if r.Directory != "" {
		snap, err = migrate.LoadSnapshot(r.Directory)
		if err != nil {
			return out, fmt.Errorf("%w: verify locked migration snapshot", ErrRefused)
		}
	}
	if snap.Manifest.Digest == "" || snap.Manifest.Generation == "" {
		return out, ErrRefused
	}
	lockedDB, lockedEnv, targetErr := trustedTarget(snap, e.Verify)
	if targetErr != nil {
		return out, targetErr
	}
	lockedKey, keyErr := executor.LockKey(lockedDB, lockedEnv)
	if keyErr != nil || lockedKey != lockKey {
		return out, fmt.Errorf("%w: signed target changed while acquiring lock", ErrRefused)
	}
	records, err := s.Revisions(ctx)
	if err != nil {
		return out, err
	}
	if len(records) > 0 {
		head := records[len(records)-1]
		ok, ae := s.ManifestDescendsFrom(ctx, snap.Manifest, head.ManifestGeneration, head.ManifestDigest)
		if ae != nil || !ok {
			return out, fmt.Errorf("%w: manifest is not an append-only descendant of applied head", ErrRefused)
		}
	}
	candidates, err := selectTrusted(snap, records, r, e.Verify)
	if err != nil {
		return out, err
	}
	history, he := s.ExecutorRecords(ctx)
	if he != nil {
		return out, he
	}
	if he = reconcileHistory(snap, records, history, e.Verify); he != nil {
		return out, he
	}
	for _, c := range candidates {
		out.Files = append(out.Files, c.entry.File)
	}
	if len(candidates) > 0 {
		out.FinalVersion = candidates[len(candidates)-1].entry.Version
	}
	if r.DryRun {
		out.Status = "dry_run"
		return out, nil
	}
	if len(candidates) == 0 {
		out.Status = "no_op"
		return out, nil
	}
	started := r.Now()
	if r.Baseline {
		err = baseline(ctx, s, candidates, snap.Manifest, r, r.Now, parentGeneration(records))
		if err != nil {
			return failed(out, nil, err)
		}
		out.Status = "baselined"
		out.Duration = r.Now().Sub(started)
		return out, nil
	}
	if r.Transaction == "all" {
		for _, c := range candidates {
			if c.entry.Directives.Transaction == migrate.TransactionForbidden || !artifactTransactional(c.payload) {
				return out, fmt.Errorf("%w: all transaction requires transaction=required", ErrRefused)
			}
		}
		err = applyAtomic(ctx, s, candidates, snap.Manifest, r, &out, e.Apply, parentGeneration(records))
	} else {
		err = applyPerFile(ctx, s, candidates, snap.Manifest, r, &out, e.Apply, parentGeneration(records))
	}
	if err != nil {
		return out, err
	}
	out.Status = "applied"
	out.Duration = r.Now().Sub(started)
	return out, nil
}
func reconcileHistory(snap migrate.Snapshot, revs []revision.Revision, rows []revision.ExecutorRecord, verify VerifyArtifact) error {
	arts := map[string]artifact.Artifact{}
	for _, m := range snap.Manifest.Entries {
		raw := snap.Files[m.ArtifactFile]
		a, e := artifact.Parse(raw)
		if e != nil {
			return ErrRefused
		}
		v, e := verify(a)
		if e != nil {
			return ErrRefused
		}
		p, e := v.Payload()
		if e != nil {
			return ErrRefused
		}
		arts[m.ArtifactDigest] = p
	}
	by := map[string][]revision.ExecutorRecord{}
	seen := map[string]bool{}
	for _, x := range rows {
		k := fmt.Sprintf("%s\x00%s\x00%d", x.ArtifactDigest, x.StepID, x.Attempt)
		if seen[k] {
			return fmt.Errorf("%w: duplicate executor history", ErrRefused)
		}
		seen[k] = true
		by[x.ArtifactDigest] = append(by[x.ArtifactDigest], x)
	}
	known := map[string]bool{}
	for _, r := range revs {
		a, ok := arts[r.ArtifactDigest]
		if !ok {
			return fmt.Errorf("%w: revision artifact absent", ErrRefused)
		}
		known[a.Digest] = true
		if r.State == "baseline" || r.State == "checkpoint" {
			if len(by[a.Digest]) != 0 {
				return fmt.Errorf("%w: baseline has executor history", ErrRefused)
			}
			continue
		}
		expected := map[string]plan.Phase{}
		for _, p := range a.Plan.Phases {
			for _, id := range p.StepIDs {
				st := stepByID(a, id)
				if st.Kind == plan.StepExecutable || p.Transaction == plan.TransactionRequired {
					expected[id] = p
				}
			}
		}
		if len(by[a.Digest]) != len(expected) {
			return fmt.Errorf("%w: incomplete executor history", ErrRefused)
		}
		for _, x := range by[a.Digest] {
			p, ok := expected[x.StepID]
			st := stepByID(a, x.StepID)
			if !ok || x.Attempt != 1 || x.State != "confirmed" || x.StepHash != executor.StepHash(st) || x.PhaseID != p.ID || x.PhaseMode != string(p.Transaction) || x.ExecutionID != a.Digest || x.TargetIdentity != a.DatabaseIdentity+"/"+a.TargetEnvironment || x.PlanDigest != a.Plan.Digest || x.BundleDigest != a.GuardrailDigest {
				return fmt.Errorf("%w: executor history binding conflict", ErrRefused)
			}
		}
	}
	for d := range by {
		if !known[d] {
			return fmt.Errorf("%w: executor history without revision", ErrRefused)
		}
	}
	return nil
}
func stepByID(a artifact.Artifact, id string) plan.Step {
	for _, s := range a.Plan.Steps {
		if s.ID == id {
			return s
		}
	}
	return plan.Step{}
}
func trustedTarget(s migrate.Snapshot, verify VerifyArtifact) (string, string, error) {
	var db, env string
	for _, m := range s.Manifest.Entries {
		raw := s.Files[m.ArtifactFile]
		if len(raw) == 0 || migrate.ArtifactDigest(raw) != m.ArtifactDigest {
			return "", "", ErrRefused
		}
		a, e := artifact.Parse(raw)
		if e != nil {
			return "", "", ErrRefused
		}
		v, e := verify(a)
		if e != nil {
			return "", "", ErrRefused
		}
		p, e := v.Payload()
		if e != nil {
			return "", "", ErrRefused
		}
		if db == "" {
			db, env = p.DatabaseIdentity, p.TargetEnvironment
		}
		if p.DatabaseIdentity != db || p.TargetEnvironment != env {
			return "", "", fmt.Errorf("%w: mixed migration targets", ErrRefused)
		}
	}
	if db == "" {
		return "", "", ErrRefused
	}
	return db, env, nil
}
func artifactTransactional(a artifact.Artifact) bool {
	for _, p := range a.Plan.Phases {
		if p.Transaction != plan.TransactionRequired {
			return false
		}
	}
	return true
}

func selectTrusted(snap migrate.Snapshot, records []revision.Revision, r Request, verify VerifyArtifact) ([]candidate, error) {
	from, to := canonicalVersion(r.From), canonicalVersion(r.To)
	if r.From != "" && from == "" || r.To != "" && to == "" {
		return nil, fmt.Errorf("%w: invalid range version", ErrRefused)
	}
	if from != "" && to != "" {
		fv, _ := migrate.ParseVersion(from)
		tv, _ := migrate.ParseVersion(to)
		if fv.Compare(tv) > 0 {
			return nil, fmt.Errorf("%w: reversed range", ErrRefused)
		}
	}
	by := map[string]revision.Revision{}
	for _, x := range records {
		if _, ok := by[x.Version]; ok {
			return nil, fmt.Errorf("%w: duplicate revision", ErrRefused)
		}
		by[x.Version] = x
	}
	applied := map[string]bool{}
	for _, m := range snap.Manifest.Entries {
		_, isApplied := by[m.Version]
		for _, p := range m.Parents {
			if !applied[p] {
				return nil, fmt.Errorf("%w: migration parent closure is incomplete", ErrRefused)
			}
		}
		if isApplied {
			applied[m.Version] = true
			continue
		}
		if m.NonLinear || len(m.Parents) > 1 {
			return nil, fmt.Errorf("%w: branching pending migrations require an explicit merge workflow", ErrRefused)
		}
		applied[m.Version] = true
	}
	manifestVersions := map[string]bool{}
	for _, m := range snap.Manifest.Entries {
		manifestVersions[m.Version] = true
	}
	for _, x := range records {
		if !manifestVersions[x.Version] {
			return nil, fmt.Errorf("%w: unknown applied revision", ErrRefused)
		}
		if x.State == "failed" || x.State == "partial" || x.State == "pending" {
			return nil, fmt.Errorf("%w: dirty revision state", ErrRefused)
		}
	}
	started := false
	var out []candidate
	for _, m := range snap.Manifest.Entries {
		if x, ok := by[m.Version]; ok {
			if started {
				return nil, fmt.Errorf("%w: applied revision gap", ErrRefused)
			}
			if x.FileName != m.File || x.FileDigest != m.SQLDigest || x.ArtifactDigest != m.ArtifactDigest || x.PlanDigest != m.Directives.PlanDigest || x.ChecksDigest != m.Directives.CheckDigest || x.BundleDigest != m.Directives.BundleDigest {
				return nil, fmt.Errorf("%w: recorded revision drift", ErrRefused)
			}
			continue
		}
		started = true
		if r.From != "" && len(out) == 0 && m.Version != canonicalVersion(r.From) {
			return nil, fmt.Errorf("%w: range start is not first pending", ErrRefused)
		}
		if r.Count != nil && len(out) >= *r.Count {
			break
		}
		raw := snap.Files[m.ArtifactFile]
		if m.ArtifactFile == "" || len(raw) == 0 || migrate.ArtifactDigest(raw) != m.ArtifactDigest {
			return nil, fmt.Errorf("%w: raw artifact binding drift", ErrRefused)
		}
		a, err := artifact.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: parse artifact", ErrRefused)
		}
		v, err := verify(a)
		if err != nil {
			return nil, fmt.Errorf("%w: trusted artifact verification", ErrRefused)
		}
		p, err := v.Payload()
		if err != nil {
			return nil, ErrRefused
		}
		if p.Digest != a.Digest || boundDigest(p.Plan.Digest) != m.Directives.PlanDigest || boundDigest(p.Checks.Digest) != m.Directives.CheckDigest || boundDigest(p.GuardrailDigest) != m.Directives.BundleDigest {
			return nil, fmt.Errorf("%w: artifact semantic binding drift", ErrRefused)
		}
		exec := 0
		for _, st := range p.Plan.Steps {
			if st.Kind == plan.StepExecutable {
				if exec >= len(m.Statements) || migrate.StatementDigest(strings.TrimSuffix(strings.TrimSpace(st.SQL), ";")) != m.Statements[exec].Digest {
					return nil, fmt.Errorf("%w: canonical statement digest drift", ErrRefused)
				}
				exec++
			}
		}
		if exec != len(m.Statements) {
			return nil, fmt.Errorf("%w: statement boundary drift", ErrRefused)
		}
		if !directiveCompatible(m.Directives.Transaction, p) {
			return nil, fmt.Errorf("%w: transaction directive conflicts with signed phases", ErrRefused)
		}
		out = append(out, candidate{m, raw, v, p})
		if r.To != "" && m.Version == canonicalVersion(r.To) {
			break
		}
	}
	if r.To != "" && (len(out) == 0 || out[len(out)-1].entry.Version != canonicalVersion(r.To)) {
		return nil, fmt.Errorf("%w: target is not contiguous pending boundary", ErrRefused)
	}
	return out, nil
}
func directiveCompatible(d migrate.TransactionMode, a artifact.Artifact) bool {
	allReq, allNo := true, true
	for _, p := range a.Plan.Phases {
		allReq = allReq && p.Transaction == plan.TransactionRequired
		allNo = allNo && p.Transaction == plan.TransactionProhibited
	}
	switch d {
	case migrate.TransactionRequired:
		return allReq
	case migrate.TransactionForbidden:
		return allNo
	case migrate.TransactionAuto:
		return true
	}
	return false
}
func canonicalVersion(v string) string {
	x, e := migrate.ParseVersion(v)
	if e != nil {
		return ""
	}
	return x.String()
}
func boundDigest(v string) string {
	if strings.HasPrefix(v, "sha256:") {
		return v
	}
	return "sha256:" + v
}
func parentGeneration(rs []revision.Revision) string {
	if len(rs) == 0 {
		return ""
	}
	return rs[len(rs)-1].ManifestGeneration
}

func baseRevision(c candidate, m migrate.Manifest, r Request, at time.Time, state, kind string) revision.Revision {
	return revision.Revision{Version: c.entry.Version, Description: c.entry.Name, Kind: kind, FileName: c.entry.File, FileDigest: c.entry.SQLDigest, ManifestDigest: m.Digest, ManifestGeneration: m.Generation, ArtifactDigest: c.entry.ArtifactDigest, PlanDigest: c.entry.Directives.PlanDigest, ChecksDigest: c.entry.Directives.CheckDigest, BundleDigest: c.entry.Directives.BundleDigest, State: state, Attempt: 1, Operator: r.Operator, StartedAt: at, UpdatedAt: at, FromVersion: c.payload.Metadata["autosql.migration.from"], ToVersion: c.entry.Version}
}
func baseline(ctx context.Context, s *revision.Session, cs []candidate, m migrate.Manifest, r Request, now func() time.Time, parent string) error {
	tx, e := s.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if e = s.RecordManifest(ctx, tx, m, parent, now()); e != nil {
		return e
	}
	for _, c := range cs {
		at := now().UTC()
		x := baseRevision(c, m, r, at, "baseline", "baseline")
		x.StatementOrdinal = len(c.entry.Statements)
		x.CompletedAt = &at
		if e = s.ExecRevision(ctx, tx, x); e != nil {
			return e
		}
		if e = s.ExecEvent(ctx, tx, revision.Event{Version: c.entry.Version, Attempt: 1, Type: "baseline_recorded", Ordinal: x.StatementOrdinal, Operator: r.Operator, At: at}); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

func applyAtomic(ctx context.Context, s *revision.Session, cs []candidate, m migrate.Manifest, r Request, out *Result, apply GuardedApply, parent string) error {
	tx, e := s.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for i, c := range cs {
		fr, fe := executeGuarded(ctx, s, tx, c, m, r, apply, parent)
		out.FileResults = append(out.FileResults, fr)
		out.Statements += fr.Statements
		if fe != nil {
			out.Status = "failed"
			out.Failure = failure(c, i, fr.Statements+1, fe)
			return fe
		}
	}
	if e = tx.Commit(ctx); e != nil {
		for _, fr := range out.FileResults {
			if fr.finalize != nil {
				_ = fr.finalize(context.WithoutCancel(ctx), false)
			}
		}
		out.Status = "uncertain"
		out.Failure = &Failure{Recovery: "reconcile all-in-one transaction outcome before retry"}
		return ErrUncertain
	}
	for _, fr := range out.FileResults {
		if fr.finalize != nil {
			if fe := fr.finalize(context.WithoutCancel(ctx), true); fe != nil {
				out.Status = "partial_failure"
				out.Failure = &Failure{Recovery: "database committed; repair durable lifecycle audit without rerunning SQL"}
				return fe
			}
		}
	}
	return nil
}
func applyPerFile(ctx context.Context, s *revision.Session, cs []candidate, m migrate.Manifest, r Request, out *Result, apply GuardedApply, parent string) error {
	for i, c := range cs {
		var fr FileResult
		var e error
		if !artifactTransactional(c.payload) {
			fr, e = executeGuarded(ctx, s, nil, c, m, r, apply, parent)
		} else {
			tx, be := s.Begin(ctx)
			if be != nil {
				return be
			}
			fr, e = executeGuarded(ctx, s, tx, c, m, r, apply, parent)
			if e == nil {
				if ce := tx.Commit(ctx); ce != nil {
					e = ErrUncertain
				}
			} else {
				_ = tx.Rollback(context.WithoutCancel(ctx))
			}
			if e != nil && !errors.Is(e, ErrUncertain) {
				_ = tx.Rollback(context.WithoutCancel(ctx))
			}
			if fr.finalize != nil {
				if e == nil {
					if fe := fr.finalize(context.WithoutCancel(ctx), true); fe != nil {
						e = fe
						out.Status = "partial_failure"
					}
				} else {
					_ = fr.finalize(context.WithoutCancel(ctx), false)
				}
			}
		}
		out.FileResults = append(out.FileResults, fr)
		out.Statements += fr.Statements
		if e != nil {
			if out.Status == "" {
				out.Status = "failed"
			}
			if errors.Is(e, ErrUncertain) {
				out.Status = "uncertain"
			}
			out.Failure = failure(c, i, fr.Statements+1, e)
			return e
		}
	}
	return nil
}
func executeGuarded(ctx context.Context, s *revision.Session, tx pgx.Tx, c candidate, m migrate.Manifest, r Request, apply GuardedApply, parent string) (FileResult, error) {
	start := r.Now()
	fr := FileResult{Version: c.entry.Version, File: c.entry.File}
	x := baseRevision(c, m, r, start.UTC(), "pending", "migration")
	if tx == nil && !artifactTransactional(c.payload) {
		initial, e := s.Begin(ctx)
		if e != nil {
			return fr, e
		}
		if e = s.RecordManifest(ctx, initial, m, parent, start); e == nil {
			e = s.ExecRevision(ctx, initial, x)
		}
		if e == nil {
			e = s.ExecEvent(ctx, initial, revision.Event{Version: x.Version, Attempt: 1, Type: "migration_requested", Operator: r.Operator, At: start.UTC()})
		}
		if e == nil {
			e = initial.Commit(ctx)
		} else {
			_ = initial.Rollback(context.WithoutCancel(ctx))
		}
		if e != nil {
			return fr, e
		}
		eres, e := apply(ctx, c.verified, executor.WrapPGX(s.Raw()), nil)
		fr.Statements = eres.Result.AppliedSteps
		fr.finalize = eres.Finalize
		done := r.Now().UTC()
		final, be := s.Begin(context.WithoutCancel(ctx))
		if be != nil {
			return fr, ErrUncertain
		}
		state, event, redacted := "applied", "migration_applied", ""
		if e != nil {
			state, event, redacted = "partial", "migration_uncertain", "uncertain executor outcome"
		}
		be = s.FinalizeRevision(context.WithoutCancel(ctx), final, x.Version, state, fr.Statements, done.Sub(start), redacted, done)
		if be == nil {
			be = s.ExecEvent(context.WithoutCancel(ctx), final, revision.Event{Version: x.Version, Attempt: 1, Type: event, Ordinal: fr.Statements, Operator: r.Operator, At: done})
		}
		if be == nil {
			be = final.Commit(context.WithoutCancel(ctx))
		} else {
			_ = final.Rollback(context.WithoutCancel(ctx))
		}
		if be != nil {
			return fr, ErrUncertain
		}
		if e != nil {
			return fr, e
		}
		fr.Duration, fr.Status = done.Sub(start), "applied"
		return fr, nil
	}
	owned := tx == nil
	if owned {
		var e error
		tx, e = s.Begin(ctx)
		if e != nil {
			return fr, e
		}
		defer tx.Rollback(context.WithoutCancel(ctx))
	}
	if err := s.RecordManifest(ctx, tx, m, parent, start); err != nil {
		return fr, err
	}
	if err := s.ExecRevision(ctx, tx, x); err != nil {
		return fr, err
	}
	if err := s.ExecEvent(ctx, tx, revision.Event{Version: x.Version, Attempt: 1, Type: "migration_requested", Operator: r.Operator, At: start.UTC()}); err != nil {
		return fr, err
	}
	eres, err := apply(ctx, c.verified, executor.WrapPGX(s.Raw()), executor.WrapPGXTx(tx))
	fr.Statements = eres.Result.AppliedSteps
	fr.finalize = eres.Finalize
	if err != nil {
		return fr, err
	}
	done := r.Now().UTC()
	fr.Duration = done.Sub(start)
	fr.Status = "applied"
	if err := s.FinalizeRevision(ctx, tx, x.Version, "applied", fr.Statements, fr.Duration, "", done); err != nil {
		return fr, err
	}
	if err := s.ExecEvent(ctx, tx, revision.Event{Version: x.Version, Attempt: 1, Type: "migration_applied", Ordinal: fr.Statements, Operator: r.Operator, At: done}); err != nil {
		return fr, err
	}
	if owned {
		if err := tx.Commit(ctx); err != nil {
			return fr, ErrUncertain
		}
	}
	return fr, nil
}
func failure(c candidate, i, statement int, err error) *Failure {
	f := &Failure{Version: c.entry.Version, File: c.entry.File, FilePosition: i + 1, StatementPosition: statement, Recovery: "repair revision and executor evidence before retry"}
	if statement > 0 && statement <= len(c.entry.Statements) {
		f.Line = c.entry.Statements[statement-1].Line
		f.Column = c.entry.Statements[statement-1].Column
	}
	if errors.Is(err, ErrUncertain) {
		f.Recovery = "reconcile uncertain database outcome before retry"
	}
	return f
}
func failed(out Result, f *Failure, e error) (Result, error) {
	out.Status = "failed"
	out.Failure = f
	return out, e
}
