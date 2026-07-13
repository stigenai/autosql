// Package ide exposes editor-neutral local workflow requests and diagnostics.
package ide

import (
	"context"
	"errors"
	"time"
)

var ErrProductionTarget = errors.New("local helper cannot use a production target")

type Operation string

const (
	Format      Operation = "format"
	Validate    Operation = "validate"
	Diff        Operation = "diff"
	Generate    Operation = "generate"
	DevDatabase Operation = "dev_database"
)

type Request struct {
	Operation                         Operation
	SourceURI, Environment, TargetRef string
	Input                             []byte
}
type Diagnostic struct {
	URI      string `json:"uri"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Severity string `json:"severity"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}
type Response struct {
	Operation   Operation
	Output      []byte
	Diagnostics []Diagnostic
	Generated   []string
	At          time.Time
}
type Handler interface {
	Handle(context.Context, Request) (Response, error)
}
type Local struct{ Engine Handler }

func (l Local) Run(ctx context.Context, r Request) (Response, error) {
	if r.Environment == "production" || r.Environment == "prod" || r.TargetRef == "production" || r.TargetRef == "prod" {
		return Response{}, ErrProductionTarget
	}
	if l.Engine == nil {
		return Response{}, errors.New("local engine required")
	}
	return l.Engine.Handle(ctx, r)
}
