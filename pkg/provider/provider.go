// Package provider defines the language-neutral external desired-state
// provider protocol. Providers are extraction-only: they receive source
// inputs and return a canonical schema document, never a database handle.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

const ProtocolVersion = "autosql.provider/v1"

var (
	ErrInvalidRequest = errors.New("invalid provider request")
	ErrMutating       = errors.New("schema providers are extraction-only")
	ErrTimeout        = errors.New("provider timeout")
)

// Metadata is wire-compatible with process and in-process SDKs. ReadOnly is
// mandatory: a provider that can mutate a target database is not conformant.
type Metadata struct {
	Name      string        `json:"name"`
	Version   string        `json:"version"`
	Protocol  string        `json:"protocol"`
	Languages []string      `json:"languages,omitempty"`
	ReadOnly  bool          `json:"read_only"`
	Kinds     []schema.Kind `json:"kinds"`
	Features  []string      `json:"features,omitempty"`
}

// Request contains only source data. Parameters are copied before invocation
// and Timeout is enforced by the SDK; no target URL or write capability exists.
type Request struct {
	SourceURI   string            `json:"source_uri"`
	Environment string            `json:"environment,omitempty"`
	Parameters  map[string]string `json:"parameters,omitempty"`
	Timeout     time.Duration     `json:"timeout,omitempty"`
	CacheKey    string            `json:"cache_key,omitempty"`
}

type Diagnostic struct {
	Severity string                 `json:"severity"`
	Code     string                 `json:"code"`
	Message  string                 `json:"message"`
	Source   *schema.SourceLocation `json:"source,omitempty"`
	Details  map[string]string      `json:"details,omitempty"`
}

func (d Diagnostic) Validate() error {
	if strings.TrimSpace(d.Code) == "" || strings.TrimSpace(d.Message) == "" {
		return fmt.Errorf("%w: diagnostic code and message are required", ErrInvalidRequest)
	}
	if d.Severity == "" {
		return fmt.Errorf("%w: diagnostic severity is required", ErrInvalidRequest)
	}
	return nil
}

type Response struct {
	Document     schema.Document `json:"document"`
	Diagnostics  []Diagnostic    `json:"diagnostics,omitempty"`
	CacheKey     string          `json:"cache_key,omitempty"`
	ProviderHash string          `json:"provider_hash"`
	StateDigest  string          `json:"state_digest"`
}

// Error preserves source-located diagnostics when extraction fails. Callers
// can render Diagnostics in terminal/JSON/SARIF/PR reports while retaining
// the underlying cause with errors.Is/As.
type Error struct {
	Err         error
	Diagnostics []Diagnostic
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Provider is the SDK contract implemented by Go, Python, TypeScript, and
// process adapters. Implementations must not mutate databases in Extract.
type Provider interface {
	Metadata() Metadata
	Extract(context.Context, Request) (schema.Document, []Diagnostic, error)
}

func ValidateMetadata(m Metadata) error {
	if m.Name == "" || m.Version == "" || m.Protocol == "" {
		return fmt.Errorf("%w: provider identity is required", ErrInvalidRequest)
	}
	if m.Protocol != ProtocolVersion {
		return fmt.Errorf("%w: unsupported protocol %q", ErrInvalidRequest, m.Protocol)
	}
	if !m.ReadOnly {
		return ErrMutating
	}
	if len(m.Kinds) == 0 {
		return fmt.Errorf("%w: at least one kind is required", ErrInvalidRequest)
	}
	seen := map[schema.Kind]bool{}
	for _, k := range m.Kinds {
		if !schema.IsKnownKind(k) || seen[k] {
			return fmt.Errorf("%w: invalid or duplicate kind %q", ErrInvalidRequest, k)
		}
		seen[k] = true
	}
	return nil
}

func ValidateRequest(r Request) error {
	if strings.TrimSpace(r.SourceURI) == "" {
		return fmt.Errorf("%w: source_uri is required", ErrInvalidRequest)
	}
	if r.Timeout < 0 {
		return fmt.Errorf("%w: timeout cannot be negative", ErrInvalidRequest)
	}
	return nil
}

// Run executes a provider with a deadline, validates diagnostics and canonical
// output, and computes stable provider/state digests for cache and attestations.
func Run(ctx context.Context, p Provider, r Request) (Response, error) {
	if p == nil {
		return Response{}, fmt.Errorf("%w: nil provider", ErrInvalidRequest)
	}
	if err := ValidateMetadata(p.Metadata()); err != nil {
		return Response{}, err
	}
	if err := ValidateRequest(r); err != nil {
		return Response{}, err
	}
	params := map[string]string{}
	for k, v := range r.Parameters {
		params[k] = v
	}
	r.Parameters = params
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	doc, diagnostics, err := p.Extract(ctx, r)
	if err != nil {
		cause := err
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cause = fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		if len(diagnostics) > 0 {
			return Response{Diagnostics: sortDiagnostics(diagnostics)}, &Error{Err: cause, Diagnostics: sortDiagnostics(diagnostics)}
		}
		if cause != err {
			return Response{}, cause
		}
		return Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	for _, d := range diagnostics {
		if err := d.Validate(); err != nil {
			return Response{}, err
		}
	}
	canonical, err := doc.MarshalCanonical()
	if err != nil {
		return Response{}, fmt.Errorf("validate provider document: %w", err)
	}
	ph := digestJSON(p.Metadata())
	return Response{Document: doc, Diagnostics: sortDiagnostics(diagnostics), ProviderHash: ph, StateDigest: digest(canonical), CacheKey: r.CacheKey}, nil
}

func digest(b []byte) string  { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }
func digestJSON(v any) string { b, _ := json.Marshal(v); return digest(b) }
func sortDiagnostics(in []Diagnostic) []Diagnostic {
	out := append([]Diagnostic(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source == nil || out[j].Source == nil {
			return out[i].Code < out[j].Code
		}
		if out[i].Source.URI != out[j].Source.URI {
			return out[i].Source.URI < out[j].Source.URI
		}
		if out[i].Source.Line != out[j].Source.Line {
			return out[i].Source.Line < out[j].Source.Line
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// PluginAdapter lets an existing read-only plugin.SourceProvider participate
// in the richer protocol while retaining source-located diagnostics.
type PluginAdapter struct{ Source plugin.SourceProvider }

func (a PluginAdapter) Metadata() Metadata {
	i := a.Source.Info()
	kinds := make([]schema.Kind, 0, len(i.Capabilities))
	for _, c := range i.Capabilities {
		if c.Mode == plugin.ReadOnly || c.Mode == plugin.Managed {
			kinds = append(kinds, c.Kind)
		}
	}
	return Metadata{Name: i.Name, Version: i.Version, Protocol: ProtocolVersion, ReadOnly: true, Kinds: kinds, Languages: []string{"go"}}
}
func (a PluginAdapter) Extract(ctx context.Context, r Request) (schema.Document, []Diagnostic, error) {
	d, err := (plugin.GuardSource{Source: a.Source}).Load(ctx, plugin.SourceRequest{URI: r.SourceURI, Environment: r.Environment, Variables: r.Parameters})
	return d, nil, err
}
