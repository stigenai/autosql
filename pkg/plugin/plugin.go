// Package plugin defines stable contracts for database drivers and desired
// schema providers. Plugins are ordinary Go values; process/RPC adapters can
// implement the same interfaces without changing callers.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"

	"autosql/pkg/schema"
)

const HostAPIVersion = "1.0"

// Mode is the level of control a plugin provides for a resource kind.
type Mode string

const (
	Unsupported Mode = "unsupported"
	ReadOnly    Mode = "read_only"
	Managed     Mode = "managed"
)

// Capability declares support for one canonical resource kind. Plugins must
// explicitly list every kind they understand; omitted kinds are unsupported.
type Capability struct {
	Kind     schema.Kind `json:"kind"`
	Mode     Mode        `json:"mode"`
	Features []string    `json:"features,omitempty"`
}

// Info is immutable plugin identity and negotiation metadata.
type Info struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	APIVersion   string       `json:"api_version"`
	Capabilities []Capability `json:"capabilities"`
}

func (i Info) Capability(kind schema.Kind) Capability {
	for _, c := range i.Capabilities {
		if c.Kind == kind {
			return c
		}
	}
	return Capability{Kind: kind, Mode: Unsupported}
}

var (
	ErrIncompatibleVersion = errors.New("incompatible plugin API version")
	ErrUnsupported         = errors.New("operation unsupported by plugin")
	ErrReadOnly            = errors.New("resource kind is read-only")
	ErrPluginFailure       = errors.New("plugin failure")
	ErrInvalidPlugin       = errors.New("invalid plugin metadata")
)

// Negotiate enforces semantic API compatibility: major versions must match,
// and a plugin's required minor must not exceed the host minor. Patch suffixes
// are accepted and ignored. This allows additive host evolution within v1.
func Negotiate(host, required string) error {
	hMaj, hMin, e := parseVersion(host)
	if e != nil {
		return fmt.Errorf("host API: %w", e)
	}
	pMaj, pMin, e := parseVersion(required)
	if e != nil {
		return fmt.Errorf("plugin API: %w", e)
	}
	if hMaj != pMaj || pMin > hMin {
		return fmt.Errorf("%w: host %s cannot satisfy plugin %s", ErrIncompatibleVersion, host, required)
	}
	return nil
}
func parseVersion(v string) (int, int, error) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return 0, 0, fmt.Errorf("invalid version %q", v)
	}
	major, e := strconv.Atoi(parts[0])
	if e != nil {
		return 0, 0, fmt.Errorf("invalid version %q", v)
	}
	minor := 0
	if len(parts) > 1 {
		minor, e = strconv.Atoi(parts[1])
		if e != nil {
			return 0, 0, fmt.Errorf("invalid version %q", v)
		}
	}
	return major, minor, nil
}

// InspectRequest selects the database and optional subset to inspect.
type InspectRequest struct {
	URL     string
	Schemas []string
	Options map[string]string
}

// RenderRequest asks a driver to render canonical changes for its dialect.
type RenderRequest struct {
	Changes schema.ChangeSet
	Current schema.Document
	Desired schema.Document
	Options map[string]string
}

// Statement is one rendered database statement with its originating change.
type Statement struct {
	SQL           string        `json:"sql"`
	ChangeID      string        `json:"change_id"`
	Transactional bool          `json:"transactional"`
	Kind          StatementKind `json:"kind,omitempty"`
}

type StatementKind string

const (
	StatementExecutable StatementKind = "executable"
	StatementTopology   StatementKind = "topology"
)

// Driver inspects live state, normalizes dialect values, and renders changes.
type Driver interface {
	Info() Info
	Inspect(context.Context, InspectRequest) (schema.Document, error)
	Normalize(context.Context, schema.Document) (schema.Document, error)
	Render(context.Context, RenderRequest) ([]Statement, error)
}

// SourceRequest identifies desired state for an external source provider.
type SourceRequest struct {
	URI         string
	Environment string
	Variables   map[string]string
}

// SourceProvider supplies a canonical desired-state graph.
type SourceProvider interface {
	Info() Info
	Load(context.Context, SourceRequest) (schema.Document, error)
}

// DiagnosticError is an actionable, machine-classifiable boundary error. The
// cause remains available through errors.Is/As; panic stacks are intentionally
// not exposed in Error but are retained for logs.
type DiagnosticError struct {
	Plugin, Operation, Code, Message string
	Cause                            error
	Stack                            []byte
}

func (e *DiagnosticError) Error() string {
	return fmt.Sprintf("plugin %q %s failed [%s]: %s", e.Plugin, e.Operation, e.Code, e.Message)
}
func (e *DiagnosticError) Unwrap() error { return e.Cause }

func validateInfo(i Info) error {
	if i.Name == "" || i.Version == "" || i.APIVersion == "" {
		return fmt.Errorf("%w: name, version, and api_version are required", ErrInvalidPlugin)
	}
	if err := Negotiate(HostAPIVersion, i.APIVersion); err != nil {
		return err
	}
	seen := map[schema.Kind]bool{}
	for _, c := range i.Capabilities {
		if !schema.IsKnownKind(c.Kind) {
			return fmt.Errorf("%w: unknown capability kind %q", ErrInvalidPlugin, c.Kind)
		}
		if seen[c.Kind] {
			return fmt.Errorf("%w: duplicate capability %q", ErrInvalidPlugin, c.Kind)
		}
		seen[c.Kind] = true
		switch c.Mode {
		case Unsupported, ReadOnly, Managed:
		default:
			return fmt.Errorf("%w: capability %q has mode %q", ErrInvalidPlugin, c.Kind, c.Mode)
		}
	}
	return nil
}

// ValidateDriver performs metadata negotiation before a driver is used.
func ValidateDriver(d Driver) error {
	if d == nil {
		return fmt.Errorf("%w: nil driver", ErrInvalidPlugin)
	}
	return validateInfo(d.Info())
}

// ValidateSource performs metadata negotiation before a provider is used.
func ValidateSource(s SourceProvider) error {
	if s == nil {
		return fmt.Errorf("%w: nil source provider", ErrInvalidPlugin)
	}
	return validateInfo(s.Info())
}

// GuardDriver contains panics and annotates ordinary errors at the plugin
// boundary. Context cancellation remains discoverable with errors.Is.
type GuardDriver struct{ Driver Driver }

func (g GuardDriver) Info() Info { return g.Driver.Info() }
func (g GuardDriver) Inspect(ctx context.Context, r InspectRequest) (out schema.Document, err error) {
	defer recoverError(g.Info().Name, "inspect", &err)
	out, err = g.Driver.Inspect(ctx, r)
	if err != nil {
		err = wrap(g.Info().Name, "inspect", err)
	}
	return
}
func (g GuardDriver) Normalize(ctx context.Context, d schema.Document) (out schema.Document, err error) {
	defer recoverError(g.Info().Name, "normalize", &err)
	out, err = g.Driver.Normalize(ctx, d)
	if err != nil {
		err = wrap(g.Info().Name, "normalize", err)
	}
	return
}
func (g GuardDriver) Render(ctx context.Context, r RenderRequest) (out []Statement, err error) {
	defer recoverError(g.Info().Name, "render", &err)
	out, err = g.Driver.Render(ctx, r)
	if err != nil {
		err = wrap(g.Info().Name, "render", err)
	}
	return
}

// GuardSource provides the same isolation at a source-provider boundary.
type GuardSource struct{ Source SourceProvider }

func (g GuardSource) Info() Info { return g.Source.Info() }
func (g GuardSource) Load(ctx context.Context, r SourceRequest) (out schema.Document, err error) {
	defer recoverError(g.Info().Name, "load", &err)
	out, err = g.Source.Load(ctx, r)
	if err != nil {
		err = wrap(g.Info().Name, "load", err)
	}
	return
}

func recoverError(name, op string, errp *error) {
	if p := recover(); p != nil {
		cause := fmt.Errorf("%w: panic: %v", ErrPluginFailure, p)
		*errp = &DiagnosticError{Plugin: name, Operation: op, Code: "panic", Message: "plugin panicked; inspect host logs for stack trace", Cause: cause, Stack: debug.Stack()}
	}
}
func wrap(name, op string, err error) error {
	code := "operation_failed"
	switch {
	case errors.Is(err, context.Canceled):
		code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline_exceeded"
	case errors.Is(err, ErrUnsupported):
		code = "unsupported"
	case errors.Is(err, ErrReadOnly):
		code = "read_only"
	}
	return &DiagnosticError{Plugin: name, Operation: op, Code: code, Message: err.Error(), Cause: err}
}

// RequireManaged checks whether a driver may apply changes to a kind.
func RequireManaged(i Info, k schema.Kind) error {
	switch i.Capability(k).Mode {
	case Managed:
		return nil
	case ReadOnly:
		return fmt.Errorf("%w: plugin %q cannot manage %q", ErrReadOnly, i.Name, k)
	default:
		return fmt.Errorf("%w: plugin %q does not support %q", ErrUnsupported, i.Name, k)
	}
}
