// Package apply coordinates bounded versioned migration execution without exposing raw SQL mutation.
package apply

import (
	"context"
	"errors"
	"fmt"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
)

var ErrRefused = errors.New("versioned migration apply refused")

type ArtifactResult struct {
	Statements int
	Duration   time.Duration
	Status     string
}
type GuardedApply func(context.Context, migrate.Migration, []byte) (ArtifactResult, error)
type AtomicBatch func(context.Context, []Candidate) ([]ArtifactResult, error)
type RevisionStore interface {
	Status(context.Context, migrate.Manifest) (revision.Status, error)
	Insert(context.Context, revision.Revision) error
	InsertBatch(context.Context, []revision.Revision, []revision.Event) error
	UpdateState(context.Context, string, int, string, int, string, time.Duration, string, string) error
}
type Candidate struct {
	Entry    migrate.Migration
	Artifact []byte
}
type Request struct {
	Snapshot         migrate.Snapshot
	From, To         string
	Count            *int
	DryRun, Baseline bool
	Transaction      string
	Operator         string
	Now              func() time.Time
}
type Failure struct {
	Version  string `json:"version,omitempty"`
	File     string `json:"file,omitempty"`
	Position int    `json:"position,omitempty"`
	Recovery string `json:"recovery,omitempty"`
}
type Result struct {
	Status       string        `json:"status"`
	Files        []string      `json:"files"`
	Statements   int           `json:"statements"`
	Duration     time.Duration `json:"duration"`
	FinalVersion string        `json:"final_version,omitempty"`
	Failure      *Failure      `json:"failure,omitempty"`
}
type Engine struct {
	Store       RevisionStore
	Apply       GuardedApply
	ApplyAtomic AtomicBatch
}

func (e Engine) Run(ctx context.Context, r Request) (Result, error) {
	var out Result
	if e.Store == nil || r.Operator == "" || r.Snapshot.Manifest.Digest == "" {
		return out, ErrRefused
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.DryRun && r.Baseline {
		return out, ErrRefused
	}
	status, err := e.Store.Status(ctx, r.Snapshot.Manifest)
	if err != nil {
		return out, err
	}
	if status.Dirty || status.Drift {
		return out, fmt.Errorf("%w: dirty or drifted revision state", ErrRefused)
	}
	if r.Count != nil && *r.Count < 0 {
		return out, ErrRefused
	}
	byVersion := map[string]revision.StatusEntry{}
	for _, s := range status.Entries {
		if s.Unknown {
			return out, fmt.Errorf("%w: unknown applied revision", ErrRefused)
		}
		byVersion[s.Version] = s
	}
	pendingStarted := false
	var candidates []Candidate
	for _, m := range r.Snapshot.Manifest.Entries {
		s := byVersion[m.Version]
		applied := s.Classification == "applied" || s.Classification == "baseline" || s.Classification == "checkpoint"
		if applied {
			if pendingStarted {
				return out, fmt.Errorf("%w: applied revision gap at %s", ErrRefused, m.Version)
			}
			continue
		}
		if s.Classification != "pending" {
			return out, fmt.Errorf("%w: incomplete revision %s", ErrRefused, m.Version)
		}
		pendingStarted = true
		if r.Count != nil && *r.Count == 0 {
			break
		}
		if r.From != "" && len(candidates) == 0 {
			v, ve := migrate.ParseVersion(m.Version)
			from, fe := migrate.ParseVersion(r.From)
			if ve != nil || fe != nil || v.Compare(from) != 0 {
				return out, fmt.Errorf("%w: range start is not first pending", ErrRefused)
			}
		}
		raw := r.Snapshot.Files[m.ArtifactFile]
		if m.ArtifactFile == "" || len(raw) == 0 {
			return out, fmt.Errorf("%w: missing artifact for %s", ErrRefused, m.Version)
		}
		a, pe := artifact.Parse(raw)
		if pe != nil || a.Digest != m.ArtifactDigest || a.Plan.Digest != m.Directives.PlanDigest || a.Checks.Digest != m.Directives.CheckDigest || a.GuardrailDigest != m.Directives.BundleDigest {
			return out, fmt.Errorf("%w: artifact binding drift at %s", ErrRefused, m.Version)
		}
		candidates = append(candidates, Candidate{m, raw})
		if r.To != "" && m.Version == r.To {
			break
		}
		if r.Count != nil && len(candidates) >= *r.Count {
			break
		}
	}
	if r.To != "" && (len(candidates) == 0 || candidates[len(candidates)-1].Entry.Version != r.To) {
		return out, fmt.Errorf("%w: target version is not a contiguous pending boundary", ErrRefused)
	}
	for _, c := range candidates {
		out.Files = append(out.Files, c.Entry.File)
	}
	if len(candidates) > 0 {
		out.FinalVersion = candidates[len(candidates)-1].Entry.Version
	}
	if r.DryRun {
		out.Status = "dry_run"
		return out, nil
	}
	if len(candidates) == 0 {
		out.Status = "no_op"
		return out, nil
	}
	if r.Transaction == "all" {
		for _, c := range candidates {
			if c.Entry.Directives.Transaction != migrate.TransactionRequired {
				return out, fmt.Errorf("%w: all-in-one requires transaction=required", ErrRefused)
			}
		}
		if e.ApplyAtomic == nil {
			return out, fmt.Errorf("%w: atomic batch executor unavailable", ErrRefused)
		}
	}
	started := r.Now()
	results := make([]ArtifactResult, len(candidates))
	if r.Baseline {
		revs := make([]revision.Revision, 0, len(candidates))
		events := make([]revision.Event, 0, len(candidates))
		for _, c := range candidates {
			now := r.Now()
			revs = append(revs, revision.Revision{Version: c.Entry.Version, Description: c.Entry.Name, Kind: "baseline", FileName: c.Entry.File, FileDigest: c.Entry.SQLDigest, ManifestDigest: r.Snapshot.Manifest.Digest, ManifestGeneration: r.Snapshot.Manifest.Generation, ArtifactDigest: c.Entry.ArtifactDigest, PlanDigest: c.Entry.Directives.PlanDigest, ChecksDigest: c.Entry.Directives.CheckDigest, BundleDigest: c.Entry.Directives.BundleDigest, State: "baseline", StatementOrdinal: len(c.Entry.Statements), Attempt: 1, Operator: r.Operator, StartedAt: started, UpdatedAt: now, CompletedAt: &now, ToVersion: c.Entry.Version})
			events = append(events, revision.Event{Version: c.Entry.Version, Type: "baseline_recorded", Attempt: 1, Operator: r.Operator, At: now})
		}
		if err = e.Store.InsertBatch(ctx, revs, events); err != nil {
			return Result{Status: "failed", Files: out.Files, FinalVersion: out.FinalVersion}, err
		}
		out.Status = "baselined"
		out.Duration = r.Now().Sub(started)
		return out, nil
	}
	if !r.Baseline && r.Transaction == "all" {
		results, err = e.ApplyAtomic(ctx, candidates)
	}
	for i, c := range candidates {
		a, _ := artifact.Parse(c.Artifact)
		requestAt := r.Now()
		pending := revision.Revision{Version: c.Entry.Version, Description: c.Entry.Name, Kind: "migration", FileName: c.Entry.File, FileDigest: c.Entry.SQLDigest, ManifestDigest: r.Snapshot.Manifest.Digest, ManifestGeneration: r.Snapshot.Manifest.Generation, ArtifactDigest: c.Entry.ArtifactDigest, PlanDigest: c.Entry.Directives.PlanDigest, ChecksDigest: c.Entry.Directives.CheckDigest, BundleDigest: c.Entry.Directives.BundleDigest, State: "pending", Attempt: 1, Operator: r.Operator, StartedAt: requestAt, UpdatedAt: requestAt, ToVersion: c.Entry.Version}
		if a.Digest != "" {
			pending.FromVersion = a.Metadata["autosql.migration.from"]
		}
		if err = e.Store.InsertBatch(ctx, []revision.Revision{pending}, []revision.Event{{Version: c.Entry.Version, Type: "migration_requested", Attempt: 1, Operator: r.Operator, At: requestAt}}); err != nil {
			return out, err
		}
		if r.Transaction != "all" {
			if e.Apply == nil {
				return out, ErrRefused
			}
			results[i], err = e.Apply(ctx, c.Entry, c.Artifact)
		}
		if err != nil {
			state := "failed"
			if results[i].Status == "uncertain" || results[i].Status == "partial_failure" {
				state = "partial"
			}
			_ = e.Store.UpdateState(context.WithoutCancel(ctx), c.Entry.Version, 1, state, results[i].Statements, "guarded migration failed", results[i].Duration, "migration_"+state, r.Operator)
			out.Status = "failed"
			out.Failure = &Failure{c.Entry.Version, c.Entry.File, i + 1, "reconcile guarded executor and revision history before retry"}
			return out, err
		}
		if err = e.Store.UpdateState(ctx, c.Entry.Version, 1, "applied", len(c.Entry.Statements), "", results[i].Duration, "migration_applied", r.Operator); err != nil {
			out.Status = "failed"
			out.Failure = &Failure{c.Entry.Version, c.Entry.File, i + 1, "migration executed but revision recording failed; repair without rerunning SQL"}
			return out, err
		}
		out.Statements += results[i].Statements
		out.Duration += results[i].Duration
	}
	out.Status = "applied"
	out.Duration = r.Now().Sub(started)
	return out, nil
}
