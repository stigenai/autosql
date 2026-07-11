// Package safety analyzes database changes before execution.
//
// Analyzers are deliberately pure: each receives the same immutable input and
// returns diagnostics without depending on another analyzer's output. Runner
// gives the result a stable order suitable for policy gates and golden files.
package safety

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"autosql/pkg/schema"
)

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// LockLevel is ordered from least to most disruptive.
type LockLevel int

const (
	LockNone LockLevel = iota
	LockShare
	LockShareUpdateExclusive
	LockShareRowExclusive
	LockAccessExclusive
)

func (l LockLevel) String() string {
	switch l {
	case LockNone:
		return "none"
	case LockShare:
		return "share"
	case LockShareUpdateExclusive:
		return "share_update_exclusive"
	case LockShareRowExclusive:
		return "share_row_exclusive"
	case LockAccessExclusive:
		return "access_exclusive"
	default:
		return "unknown"
	}
}

// Diagnostic is the stable, portable result of a rule.
type Diagnostic struct {
	Rule        string                 `json:"rule"`
	Severity    Severity               `json:"severity"`
	Message     string                 `json:"message"`
	Object      Object                 `json:"object"`
	Source      *schema.SourceLocation `json:"source,omitempty"`
	Impact      string                 `json:"impact"`
	Remediation string                 `json:"remediation"`
	Confidence  Confidence             `json:"confidence"`
	Assumptions []string               `json:"assumptions,omitempty"`
	Properties  map[string]any         `json:"properties,omitempty"`
	Suppressed  *Suppression           `json:"suppressed,omitempty"`
}

type Object struct {
	ID   string      `json:"id"`
	Kind schema.Kind `json:"kind"`
	Name string      `json:"name"`
}

// Statement preserves rendered SQL and its source mapping. SQL is not emitted
// by reports because it may contain credentials or sensitive literals.
type Statement struct {
	ChangeID string                 `json:"change_id"`
	SQL      string                 `json:"-"`
	Source   *schema.SourceLocation `json:"source,omitempty"`
}

// TableStatistics is optional target metadata. An absent entry intentionally
// causes conservative static analysis rather than suppressing a risk.
type TableStatistics struct {
	EstimatedRows int64 `json:"estimated_rows,omitempty"`
	TotalBytes    int64 `json:"total_bytes,omitempty"`
}

type Target struct {
	Engine     string                     `json:"engine"`
	Version    int                        `json:"version"`              // PostgreSQL major version.
	Statistics map[string]TableStatistics `json:"statistics,omitempty"` // keyed by resource ID.
}

type Thresholds struct {
	MaxLockLevel    LockLevel `json:"max_lock_level,omitempty"`
	MaxRowsScanned  int64     `json:"max_rows_scanned,omitempty"`
	MaxRewriteBytes int64     `json:"max_rewrite_bytes,omitempty"`
}

type Input struct {
	Changes    schema.ChangeSet
	Statements []Statement
	Target     Target
	Thresholds Thresholds
}

type Analyzer interface {
	Name() string
	Analyze(context.Context, Input) ([]Diagnostic, error)
}

type AnalyzerFunc struct {
	ID string
	Fn func(context.Context, Input) ([]Diagnostic, error)
}

func (a AnalyzerFunc) Name() string { return a.ID }
func (a AnalyzerFunc) Analyze(ctx context.Context, in Input) ([]Diagnostic, error) {
	return a.Fn(ctx, in)
}

type Suppression struct {
	Rule      string     `json:"rule"`
	ObjectID  string     `json:"object_id"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (s Suppression) Validate() error {
	if strings.TrimSpace(s.Rule) == "" || strings.TrimSpace(s.ObjectID) == "" || strings.TrimSpace(s.Reason) == "" {
		return errors.New("suppression requires rule, object_id, and reason")
	}
	return nil
}

type Runner struct {
	Analyzers    []Analyzer
	Suppressions []Suppression
	Now          func() time.Time
}

func (r Runner) Run(ctx context.Context, in Input) ([]Diagnostic, error) {
	if err := in.Changes.Validate(); err != nil {
		return nil, fmt.Errorf("validate changes: %w", err)
	}
	for _, s := range r.Suppressions {
		if err := s.Validate(); err != nil {
			return nil, err
		}
	}
	analyzers := append([]Analyzer(nil), r.Analyzers...)
	sort.Slice(analyzers, func(i, j int) bool { return analyzers[i].Name() < analyzers[j].Name() })
	seen := map[string]bool{}
	var out []Diagnostic
	for _, a := range analyzers {
		if a == nil || strings.TrimSpace(a.Name()) == "" {
			return nil, errors.New("analyzer and analyzer name are required")
		}
		if seen[a.Name()] {
			return nil, fmt.Errorf("duplicate analyzer %q", a.Name())
		}
		seen[a.Name()] = true
		isolated, err := cloneInput(in)
		if err != nil {
			return nil, fmt.Errorf("clone analyzer input: %w", err)
		}
		ds, err := a.Analyze(ctx, isolated)
		if err != nil {
			return nil, fmt.Errorf("analyzer %s: %w", a.Name(), err)
		}
		for i := range ds {
			if err := validateDiagnostic(ds[i]); err != nil {
				return nil, fmt.Errorf("analyzer %s: %w", a.Name(), err)
			}
			applySuppression(&ds[i], r.Suppressions, now(r.Now))
		}
		out = append(out, ds...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Object.ID != b.Object.ID {
			return a.Object.ID < b.Object.ID
		}
		if a.Message != b.Message {
			return a.Message < b.Message
		}
		return a.Severity < b.Severity
	})
	return out, nil
}

// cloneInput creates a complete ownership boundary for an analyzer. This is
// intentionally done once per invocation: third-party analyzers may mutate any
// map, slice, raw JSON buffer, or source pointer they receive without affecting
// another analyzer or the caller's plan.
func cloneInput(in Input) (Input, error) {
	var out Input
	raw, err := json.Marshal(in.Changes)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out.Changes); err != nil {
		return out, err
	}
	out.Statements = make([]Statement, len(in.Statements))
	for i, st := range in.Statements {
		out.Statements[i] = st
		if st.Source != nil {
			source := *st.Source
			if st.Source.Extra != nil {
				source.Extra = make(map[string]json.RawMessage, len(st.Source.Extra))
				for key, value := range st.Source.Extra {
					source.Extra[key] = append(json.RawMessage(nil), value...)
				}
			}
			out.Statements[i].Source = &source
		}
	}
	out.Target.Engine = in.Target.Engine
	out.Target.Version = in.Target.Version
	if in.Target.Statistics != nil {
		out.Target.Statistics = make(map[string]TableStatistics, len(in.Target.Statistics))
		for key, value := range in.Target.Statistics {
			out.Target.Statistics[key] = value
		}
	}
	out.Thresholds = in.Thresholds
	return out, nil
}

func now(f func() time.Time) time.Time {
	if f != nil {
		return f().UTC()
	}
	return time.Now().UTC()
}

func applySuppression(d *Diagnostic, ss []Suppression, at time.Time) {
	for i := range ss {
		s := ss[i]
		if s.Rule == d.Rule && s.ObjectID == d.Object.ID && (s.ExpiresAt == nil || s.ExpiresAt.After(at)) {
			copy := s
			d.Suppressed = &copy
			return
		}
	}
}

func validateDiagnostic(d Diagnostic) error {
	if d.Rule == "" || d.Object.ID == "" || d.Object.Kind == "" || d.Object.Name == "" || d.Impact == "" || d.Remediation == "" || d.Message == "" {
		return errors.New("diagnostic requires rule, object id/kind/name, message, impact, and remediation")
	}
	switch d.Severity {
	case SeverityInfo, SeverityWarning, SeverityError:
	default:
		return fmt.Errorf("invalid severity %q", d.Severity)
	}
	switch d.Confidence {
	case ConfidenceLow, ConfidenceMedium, ConfidenceHigh:
	default:
		return fmt.Errorf("invalid confidence %q", d.Confidence)
	}
	return nil
}

func objectFor(ch schema.Change) Object {
	r := ch.After
	if r == nil {
		r = ch.Before
	}
	return Object{ID: ch.ResourceID, Kind: r.Kind, Name: r.Name.String()}
}

func sourceFor(ch schema.Change) *schema.SourceLocation {
	if ch.After != nil && ch.After.Source != nil {
		return ch.After.Source
	}
	if ch.Before != nil {
		return ch.Before.Source
	}
	return nil
}
