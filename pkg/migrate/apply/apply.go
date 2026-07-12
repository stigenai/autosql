// Package apply performs trusted, bounded migration application on one pinned
// PostgreSQL session. Revision state, executor evidence and transactional DDL
// are committed together; ambiguous non-transactional outcomes fail closed.
package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosql/pkg/artifact"
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
}

func (e Engine) Run(ctx context.Context, r Request) (out Result, err error) {
	if e.Store == nil || e.Verify == nil || r.Operator == "" || r.TargetIdentity == "" || (r.Transaction != "file" && r.Transaction != "all") || r.DryRun && r.Baseline || r.Count != nil && *r.Count < 0 {
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
	locked, err := s.Lock(ctx, "autosql/migrate/target")
	if err != nil {
		return out, errors.New("acquire migration lock")
	}
	if !locked {
		return out, ErrBusy
	}
	defer func() {
		if u := s.Unlock(context.WithoutCancel(ctx), "autosql/migrate/target"); u != nil && err == nil {
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
	records, err := s.Revisions(ctx)
	if err != nil {
		return out, err
	}
	candidates, err := selectTrusted(snap, records, r, e.Verify)
	if err != nil {
		return out, err
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
		err = baseline(ctx, s, candidates, snap.Manifest, r, r.Now)
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
		err = applyAtomic(ctx, s, candidates, snap.Manifest, r, &out)
	} else {
		err = applyPerFile(ctx, s, candidates, snap.Manifest, r, &out)
	}
	if err != nil {
		return out, err
	}
	out.Status = "applied"
	out.Duration = r.Now().Sub(started)
	return out, nil
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
	by := map[string]revision.Revision{}
	for _, x := range records {
		if _, ok := by[x.Version]; ok {
			return nil, fmt.Errorf("%w: duplicate revision", ErrRefused)
		}
		by[x.Version] = x
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
		if r.Count != nil && len(out) >= *r.Count {
			break
		}
		if r.From != "" && len(out) == 0 && m.Version != canonicalVersion(r.From) {
			return nil, fmt.Errorf("%w: range start is not first pending", ErrRefused)
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
				exec++
			}
		}
		if exec != len(m.Statements) {
			return nil, fmt.Errorf("%w: statement boundary drift", ErrRefused)
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

func baseRevision(c candidate, m migrate.Manifest, r Request, at time.Time, state, kind string) revision.Revision {
	return revision.Revision{Version: c.entry.Version, Description: c.entry.Name, Kind: kind, FileName: c.entry.File, FileDigest: c.entry.SQLDigest, ManifestDigest: m.Digest, ManifestGeneration: m.Generation, ArtifactDigest: c.entry.ArtifactDigest, PlanDigest: c.entry.Directives.PlanDigest, ChecksDigest: c.entry.Directives.CheckDigest, BundleDigest: c.entry.Directives.BundleDigest, State: state, Attempt: 1, Operator: r.Operator, StartedAt: at, UpdatedAt: at, FromVersion: c.payload.Metadata["autosql.migration.from"], ToVersion: c.entry.Version}
}
func baseline(ctx context.Context, s *revision.Session, cs []candidate, m migrate.Manifest, r Request, now func() time.Time) error {
	tx, e := s.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
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

func applyAtomic(ctx context.Context, s *revision.Session, cs []candidate, m migrate.Manifest, r Request, out *Result) error {
	tx, e := s.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for i, c := range cs {
		fr, fe := executeTransactional(ctx, s, tx, c, m, r)
		out.FileResults = append(out.FileResults, fr)
		out.Statements += fr.Statements
		if fe != nil {
			out.Status = "failed"
			out.Failure = failure(c, i, fr.Statements+1, fe)
			return fe
		}
	}
	if e = tx.Commit(ctx); e != nil {
		out.Status = "uncertain"
		out.Failure = &Failure{Recovery: "reconcile all-in-one transaction outcome before retry"}
		return ErrUncertain
	}
	return nil
}
func applyPerFile(ctx context.Context, s *revision.Session, cs []candidate, m migrate.Manifest, r Request, out *Result) error {
	for i, c := range cs {
		var fr FileResult
		var e error
		if c.entry.Directives.Transaction == migrate.TransactionForbidden {
			fr, e = executeNonTransactional(ctx, s, c, m, r)
		} else {
			tx, be := s.Begin(ctx)
			if be != nil {
				return be
			}
			fr, e = executeTransactional(ctx, s, tx, c, m, r)
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
		}
		out.FileResults = append(out.FileResults, fr)
		out.Statements += fr.Statements
		if e != nil {
			out.Status = "failed"
			if errors.Is(e, ErrUncertain) {
				out.Status = "uncertain"
			}
			out.Failure = failure(c, i, fr.Statements+1, e)
			return e
		}
	}
	return nil
}

func executeTransactional(ctx context.Context, s *revision.Session, tx pgx.Tx, c candidate, m migrate.Manifest, r Request) (FileResult, error) {
	start := r.Now()
	fr := FileResult{Version: c.entry.Version, File: c.entry.File}
	x := baseRevision(c, m, r, start.UTC(), "pending", "migration")
	if err := runChecks(ctx, tx, c.payload); err != nil {
		return fr, err
	}
	if err := s.EnsureExecutorHistory(ctx, tx); err != nil {
		return fr, err
	}
	if err := s.ExecRevision(ctx, tx, x); err != nil {
		return fr, err
	}
	if err := s.ExecEvent(ctx, tx, revision.Event{Version: x.Version, Attempt: 1, Type: "migration_requested", Operator: r.Operator, At: start.UTC()}); err != nil {
		return fr, err
	}
	ord := 0
	for _, st := range c.payload.Plan.Steps {
		if st.Kind != plan.StepExecutable {
			continue
		}
		ord++
		at := r.Now().UTC()
		if err := s.ExecStatement(ctx, tx, revision.StatementAttempt{Version: x.Version, Ordinal: ord, Attempt: 1, State: "intended", Digest: c.entry.Statements[ord-1].Digest, Operator: r.Operator, StartedAt: at}); err != nil {
			return fr, err
		}
		if _, err := tx.Exec(ctx, st.SQL); err != nil {
			return fr, errors.New("execute migration statement")
		}
		done := r.Now().UTC()
		phaseID, phaseMode := stepPhase(c.payload, st.ID)
		if err := s.ExecHistory(ctx, tx, c.payload.Digest, st.ID, stepHash(st), phaseID, phaseMode, "confirmed", st.ID, "", c.payload.DatabaseIdentity+"/"+c.payload.TargetEnvironment, c.payload.Plan.Digest, c.payload.GuardrailDigest); err != nil {
			return fr, err
		}
		if err := s.ConfirmStatement(ctx, tx, x.Version, ord, 1, done); err != nil {
			return fr, err
		}
		fr.Statements = ord
	}
	done := r.Now().UTC()
	fr.Duration = done.Sub(start)
	fr.Status = "applied"
	if err := s.FinalizeRevision(ctx, tx, x.Version, "applied", ord, fr.Duration, "", done); err != nil {
		return fr, err
	}
	if err := s.ExecEvent(ctx, tx, revision.Event{Version: x.Version, Attempt: 1, Type: "migration_applied", Ordinal: ord, Operator: r.Operator, At: done}); err != nil {
		return fr, err
	}
	return fr, nil
}

func executeNonTransactional(ctx context.Context, s *revision.Session, c candidate, m migrate.Manifest, r Request) (FileResult, error) {
	start := r.Now()
	fr := FileResult{Version: c.entry.Version, File: c.entry.File}
	x := baseRevision(c, m, r, start.UTC(), "pending", "migration")
	if err := runChecks(ctx, s, c.payload); err != nil {
		return fr, err
	}
	tx, e := s.Begin(ctx)
	if e != nil {
		return fr, e
	}
	if e = s.EnsureExecutorHistory(ctx, tx); e == nil {
		e = s.ExecRevision(ctx, tx, x)
	}
	if e == nil {
		e = s.ExecEvent(ctx, tx, revision.Event{Version: x.Version, Attempt: 1, Type: "migration_requested", Operator: r.Operator, At: start.UTC()})
	}
	if e == nil {
		e = tx.Commit(ctx)
	} else {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}
	if e != nil {
		return fr, e
	}
	ord := 0
	for _, st := range c.payload.Plan.Steps {
		if st.Kind != plan.StepExecutable {
			continue
		}
		ord++
		at := r.Now().UTC()
		itx, ie := s.Begin(ctx)
		if ie != nil {
			return fr, ie
		}
		ie = s.ExecStatement(ctx, itx, revision.StatementAttempt{Version: x.Version, Ordinal: ord, Attempt: 1, State: "intended", Digest: c.entry.Statements[ord-1].Digest, Operator: r.Operator, StartedAt: at})
		if ie == nil {
			ie = s.ExecEvent(ctx, itx, revision.Event{Version: x.Version, Attempt: 1, Type: "statement_intended", Ordinal: ord, Operator: r.Operator, At: at})
		}
		if ie == nil {
			phaseID, phaseMode := stepPhase(c.payload, st.ID)
			ie = s.ExecHistory(ctx, itx, c.payload.Digest, st.ID, stepHash(st), phaseID, phaseMode, "intended", "", "reconcile before retry", c.payload.DatabaseIdentity+"/"+c.payload.TargetEnvironment, c.payload.Plan.Digest, c.payload.GuardrailDigest)
		}
		if ie == nil {
			ie = itx.Commit(ctx)
		} else {
			_ = itx.Rollback(context.WithoutCancel(ctx))
		}
		if ie != nil {
			return fr, ie
		}
		if _, ie = s.Exec(ctx, st.SQL); ie != nil {
			return fr, markUncertain(ctx, s, x, r, ord, "nontransactional statement failed or disconnected")
		}
		done := r.Now().UTC()
		ctx2, ce := s.Begin(ctx)
		if ce != nil {
			return fr, ErrUncertain
		}
		if ce = s.ConfirmStatement(ctx, ctx2, x.Version, ord, 1, done); ce == nil {
			ce = s.ExecEvent(ctx, ctx2, revision.Event{Version: x.Version, Attempt: 1, Type: "statement_confirmed", Ordinal: ord, Operator: r.Operator, At: done})
		}
		if ce == nil {
			ce = s.ConfirmHistory(ctx, ctx2, c.payload.Digest, st.ID, done)
		}
		if ce == nil {
			ce = ctx2.Commit(ctx)
		} else {
			_ = ctx2.Rollback(context.WithoutCancel(ctx))
		}
		if ce != nil {
			return fr, ErrUncertain
		}
		fr.Statements = ord
	}
	done := r.Now().UTC()
	fr.Duration = done.Sub(start)
	ftx, e := s.Begin(ctx)
	if e != nil {
		return fr, ErrUncertain
	}
	e = s.FinalizeRevision(ctx, ftx, x.Version, "applied", ord, fr.Duration, "", done)
	if e == nil {
		e = s.ExecEvent(ctx, ftx, revision.Event{Version: x.Version, Attempt: 1, Type: "migration_applied", Ordinal: ord, Operator: r.Operator, At: done})
	}
	if e == nil {
		e = ftx.Commit(ctx)
	} else {
		_ = ftx.Rollback(context.WithoutCancel(ctx))
	}
	if e != nil {
		return fr, ErrUncertain
	}
	fr.Status = "applied"
	return fr, nil
}
func markUncertain(ctx context.Context, s *revision.Session, x revision.Revision, r Request, ord int, detail string) error {
	tx, e := s.Begin(context.WithoutCancel(ctx))
	if e != nil {
		return ErrUncertain
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	e = s.FinalizeRevision(context.WithoutCancel(ctx), tx, x.Version, "partial", ord, 0, "uncertain nontransactional outcome", r.Now().UTC())
	if e == nil {
		e = s.ExecEvent(context.WithoutCancel(ctx), tx, revision.Event{Version: x.Version, Attempt: 1, Type: "statement_uncertain", Ordinal: ord, Detail: detail, Operator: r.Operator, At: r.Now().UTC()})
	}
	if e == nil {
		e = tx.Commit(context.WithoutCancel(ctx))
	}
	return ErrUncertain
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

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func runChecks(ctx context.Context, q queryer, a artifact.Artifact) error {
	for _, c := range a.Checks.Assertions {
		cc, cancel := context.WithCancel(ctx)
		if c.Timeout > 0 {
			cc, cancel = context.WithTimeout(ctx, c.Timeout)
		}
		var got int64
		err := q.QueryRow(cc, c.Query, c.Args...).Scan(&got)
		cancel()
		if err != nil {
			return errors.New("migration precheck failed")
		}
		if got > c.MaxAllowed {
			return errors.New("migration precheck refused")
		}
	}
	return nil
}
func stepHash(s plan.Step) string {
	x := sha256.Sum256([]byte(s.ID + "\x00" + s.SQL + "\x00" + string(s.Transaction)))
	return "sha256:" + hex.EncodeToString(x[:])
}
func stepPhase(a artifact.Artifact, id string) (string, string) {
	for _, p := range a.Plan.Phases {
		for _, x := range p.StepIDs {
			if x == id {
				return p.ID, string(p.Transaction)
			}
		}
	}
	return "versioned-file", "forbidden"
}
