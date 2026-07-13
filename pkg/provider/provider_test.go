package provider_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"autosql/pkg/provider"
	"autosql/pkg/schema"
)

func document() schema.Document {
	n := schema.Name{Name: "public"}
	return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n}}}}
}

type fake struct {
	m     provider.Metadata
	d     schema.Document
	delay time.Duration
}

func (f fake) Metadata() provider.Metadata { return f.m }
func (f fake) Extract(ctx context.Context, _ provider.Request) (schema.Document, []provider.Diagnostic, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return schema.Document{}, nil, ctx.Err()
		}
	}
	return f.d, []provider.Diagnostic{{Severity: "warning", Code: "source-note", Message: "generated", Source: &schema.SourceLocation{URI: "file:///models.go", Line: 3, Column: 1}}}, nil
}

func TestRunCanonicalAndDiagnostics(t *testing.T) {
	p := fake{m: provider.Metadata{Name: "go-fixture", Version: "1.2.0", Protocol: provider.ProtocolVersion, ReadOnly: true, Kinds: []schema.Kind{schema.KindSchema}, Languages: []string{"go"}}, d: document()}
	a, err := provider.Run(context.Background(), p, provider.Request{SourceURI: "file:///models.go", Parameters: map[string]string{"dialect": "postgres"}, CacheKey: "models-v1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := provider.Run(context.Background(), p, provider.Request{SourceURI: "file:///models.go", Parameters: map[string]string{"dialect": "postgres"}, CacheKey: "models-v1"})
	if err != nil || a.StateDigest != b.StateDigest || a.ProviderHash != b.ProviderHash {
		t.Fatalf("non-deterministic response: %#v %#v err=%v", a, b, err)
	}
	if len(a.Diagnostics) != 1 || a.Diagnostics[0].Source.Line != 3 {
		t.Fatalf("source diagnostic missing: %#v", a.Diagnostics)
	}
}

func TestRunRejectsMutatingAndTimesOut(t *testing.T) {
	p := fake{m: provider.Metadata{Name: "bad", Version: "1", Protocol: provider.ProtocolVersion, ReadOnly: false, Kinds: []schema.Kind{schema.KindSchema}}, d: document()}
	if _, err := provider.Run(context.Background(), p, provider.Request{SourceURI: "x"}); !errors.Is(err, provider.ErrMutating) {
		t.Fatalf("got %v", err)
	}
	p.m.ReadOnly = true
	p.delay = time.Second
	if _, err := provider.Run(context.Background(), p, provider.Request{SourceURI: "x", Timeout: time.Millisecond}); !errors.Is(err, provider.ErrTimeout) {
		t.Fatalf("timeout got %v", err)
	}
}

type failing struct{ fake }

func (f failing) Extract(context.Context, provider.Request) (schema.Document, []provider.Diagnostic, error) {
	return schema.Document{}, []provider.Diagnostic{{Severity: "error", Code: "parse", Message: "invalid model", Source: &schema.SourceLocation{URI: "file:///models.go", Line: 8}}}, errors.New("parse failed")
}

func TestRunPreservesDiagnosticsOnFailure(t *testing.T) {
	f := failing{fake{m: provider.Metadata{Name: "failing", Version: "1", Protocol: provider.ProtocolVersion, ReadOnly: true, Kinds: []schema.Kind{schema.KindSchema}}}}
	_, err := provider.Run(context.Background(), f, provider.Request{SourceURI: "file:///models.go"})
	var pe *provider.Error
	if !errors.As(err, &pe) || len(pe.Diagnostics) != 1 || pe.Diagnostics[0].Source.Line != 8 {
		t.Fatalf("diagnostics lost: %v", err)
	}
}
