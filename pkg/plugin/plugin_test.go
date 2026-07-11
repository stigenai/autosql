package plugin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/plugin/plugintest"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/schema"
)

func doc() schema.Document {
	n := schema.Name{Name: "public"}
	return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n}}}}
}

func TestSamplePluginsConform(t *testing.T) {
	d := doc()
	plugintest.Driver(t, sample.Driver{Document: d}, plugin.InspectRequest{URL: "sample://db"})
	plugintest.Source(t, sample.Source{Document: d}, plugin.SourceRequest{URI: "sample://desired"})
}

func TestCapabilityModes(t *testing.T) {
	i := sample.Driver{}.Info()
	if err := plugin.RequireManaged(i, schema.KindTable); err != nil {
		t.Fatal(err)
	}
	if err := plugin.RequireManaged(i, schema.KindView); !errors.Is(err, plugin.ErrReadOnly) {
		t.Fatalf("got %v", err)
	}
	if err := plugin.RequireManaged(i, schema.KindRole); !errors.Is(err, plugin.ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestVersionNegotiation(t *testing.T) {
	for _, tc := range []struct {
		host, plugin string
		ok           bool
	}{{"1.2", "1.0", true}, {"v1.2.3", "1.2.9", true}, {"1.1", "1.2", false}, {"2.0", "1.0", false}, {"bad", "1.0", false}} {
		err := plugin.Negotiate(tc.host, tc.plugin)
		if (err == nil) != tc.ok {
			t.Errorf("Negotiate(%q,%q)=%v", tc.host, tc.plugin, err)
		}
	}
}

type panicDriver struct{}

func (panicDriver) Info() plugin.Info {
	return plugin.Info{Name: "panic", Version: "1", APIVersion: "1.0"}
}
func (panicDriver) Inspect(context.Context, plugin.InspectRequest) (schema.Document, error) {
	panic("secret implementation detail")
}
func (panicDriver) Normalize(context.Context, schema.Document) (schema.Document, error) {
	return schema.Document{}, context.DeadlineExceeded
}
func (panicDriver) Render(context.Context, plugin.RenderRequest) ([]plugin.Statement, error) {
	return nil, plugin.ErrUnsupported
}

func TestGuardIsolatesPanicsAndClassifiesFailures(t *testing.T) {
	g := plugin.GuardDriver{Driver: panicDriver{}}
	_, err := g.Inspect(context.Background(), plugin.InspectRequest{})
	var diagnostic *plugin.DiagnosticError
	if !errors.As(err, &diagnostic) || diagnostic.Code != "panic" || len(diagnostic.Stack) == 0 {
		t.Fatalf("unexpected panic error: %#v", err)
	}
	if strings.Contains(err.Error(), "secret implementation detail") {
		t.Fatalf("panic internals leaked to user: %v", err)
	}
	_, err = g.Normalize(context.Background(), doc())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &diagnostic) || diagnostic.Code != "deadline_exceeded" {
		t.Fatalf("unexpected deadline error: %v", err)
	}
	_, err = g.Render(context.Background(), plugin.RenderRequest{})
	if !errors.Is(err, plugin.ErrUnsupported) || !errors.As(err, &diagnostic) || diagnostic.Code != "unsupported" {
		t.Fatalf("unexpected unsupported error: %v", err)
	}
}

func TestMetadataValidation(t *testing.T) {
	if err := plugin.ValidateDriver(sample.Driver{}); err != nil {
		t.Fatal(err)
	}
	bad := badInfoDriver{panicDriver{}}
	if err := plugin.ValidateDriver(bad); !errors.Is(err, plugin.ErrIncompatibleVersion) {
		t.Fatalf("got %v", err)
	}
}

type badInfoDriver struct{ panicDriver }

func (badInfoDriver) Info() plugin.Info {
	return plugin.Info{Name: "future", Version: "1", APIVersion: "2.0"}
}
