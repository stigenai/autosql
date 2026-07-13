// Package devflow exposes editor-neutral local developer operations. The same
// source parser and semantic diff engine are used by CI and IDE clients.
package devflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"autosql/pkg/schema"
	"autosql/pkg/source"
)

var ErrProductionTarget = errors.New("local developer helper cannot target production")

type Request struct {
	URI                         string
	Data                        []byte
	Format                      source.Format
	Environment, DatabaseTarget string
}
type Diagnostic struct {
	URI      string `json:"uri"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}
type Preview struct {
	Changes     schema.ChangeSet `json:"changes"`
	Diagnostics []Diagnostic     `json:"diagnostics,omitempty"`
}
type Result struct {
	Document    schema.Document `json:"document"`
	Preview     *Preview        `json:"preview,omitempty"`
	Generated   []byte          `json:"generated,omitempty"`
	Diagnostics []Diagnostic    `json:"diagnostics,omitempty"`
}

func ensureLocal(r Request) error {
	env := strings.ToLower(strings.TrimSpace(r.Environment))
	target := strings.ToLower(strings.TrimSpace(r.DatabaseTarget))
	if env == "production" || env == "prod" || target == "production" || target == "prod" {
		return ErrProductionTarget
	}
	if r.URI == "" {
		return errors.New("source URI required")
	}
	return nil
}

func Parse(ctx context.Context, r Request) (schema.Document, []Diagnostic, error) {
	if err := ensureLocal(r); err != nil {
		return schema.Document{}, nil, err
	}
	d, err := source.LoadContext(ctx, source.Input{URI: r.URI, Format: r.Format, Data: r.Data})
	if err != nil {
		return d, []Diagnostic{{URI: r.URI, Message: err.Error(), Severity: "error"}}, err
	}
	return d, nil, nil
}
func Format(ctx context.Context, r Request) (Result, error) {
	d, diags, err := Parse(ctx, r)
	if err != nil {
		return Result{Document: d, Diagnostics: diags}, err
	}
	b, _ := d.MarshalJSON()
	return Result{Document: d, Generated: b, Diagnostics: diags}, nil
}
func Validate(ctx context.Context, r Request) (Result, error) {
	d, diags, err := Parse(ctx, r)
	if err != nil {
		return Result{Document: d, Diagnostics: diags}, err
	}
	if e := d.Validate(); e != nil {
		diags = append(diags, Diagnostic{URI: r.URI, Message: e.Error(), Severity: "error"})
		return Result{Document: d, Diagnostics: diags}, e
	}
	return Result{Document: d, Diagnostics: diags}, nil
}
func PreviewDiff(ctx context.Context, current, desired Request) (Result, error) {
	cur, diags, err := Parse(ctx, current)
	if err != nil {
		return Result{Diagnostics: diags}, err
	}
	want, more, err := Parse(ctx, desired)
	diags = append(diags, more...)
	if err != nil {
		return Result{Diagnostics: diags}, err
	}
	changes, err := schema.Diff(cur, want, schema.DiffOptions{})
	if err != nil {
		return Result{Diagnostics: diags}, err
	}
	return Result{Preview: &Preview{Changes: changes, Diagnostics: diags}}, nil
}
func Generate(ctx context.Context, r Request) (Result, error) {
	res, err := Validate(ctx, r)
	if err != nil {
		return res, err
	}
	b, err := json.Marshal(res.Document.Graph)
	if err != nil {
		return res, fmt.Errorf("generate: %w", err)
	}
	res.Generated = b
	return res, nil
}

type LocalHelper struct {
	Environment    string
	DatabaseTarget string
}

func (h LocalHelper) Validate() error {
	return ensureLocal(Request{URI: "local", Environment: h.Environment, DatabaseTarget: h.DatabaseTarget})
}
func (h LocalHelper) ConnectionReference() (string, error) {
	if err := h.Validate(); err != nil {
		return "", err
	}
	return "local://autosql-dev", nil
}
