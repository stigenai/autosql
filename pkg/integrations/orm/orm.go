// Package orm provides versioned, language-neutral reference ORM adapter
// descriptors and conformance fixtures. Adapters emit canonical schema state
// through the plugin SourceProvider contract; they never connect to targets.
package orm

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

var ErrUnsupported = errors.New("unsupported ORM construct")

type Language string

const (
	Go         Language = "go"
	Python     Language = "python"
	TypeScript Language = "typescript"
	Java       Language = "java"
)

type Adapter struct {
	Name        string                                                               `json:"name"`
	Language    Language                                                             `json:"language"`
	Version     string                                                               `json:"version"`
	Supported   []schema.Kind                                                        `json:"supported_kinds"`
	Unsupported []string                                                             `json:"unsupported_constructs,omitempty"`
	LoadFunc    func(context.Context, plugin.SourceRequest) (schema.Document, error) `json:"-"`
}

func (a Adapter) Info() plugin.Info {
	caps := make([]plugin.Capability, 0, len(a.Supported))
	for _, k := range a.Supported {
		caps = append(caps, plugin.Capability{Kind: k, Mode: plugin.ReadOnly})
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].Kind < caps[j].Kind })
	return plugin.Info{Name: a.Name, Version: a.Version, APIVersion: plugin.HostAPIVersion, Capabilities: caps}
}
func (a Adapter) Load(ctx context.Context, req plugin.SourceRequest) (schema.Document, error) {
	if a.LoadFunc == nil {
		return schema.Document{}, ErrUnsupported
	}
	d, err := a.LoadFunc(ctx, req)
	if err != nil {
		return schema.Document{}, err
	}
	d.Normalize()
	if err := d.Validate(); err != nil {
		return schema.Document{}, err
	}
	return d, nil
}

type FeatureMatrix struct {
	Adapter, Language, Version string
	Supported                  []string `json:"supported,omitempty"`
	Unsupported                []string `json:"unsupported,omitempty"`
}

func (a Adapter) Matrix() FeatureMatrix {
	return FeatureMatrix{Adapter: a.Name, Language: string(a.Language), Version: a.Version, Supported: kinds(a.Supported), Unsupported: append([]string(nil), a.Unsupported...)}
}
func kinds(xs []schema.Kind) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	sort.Strings(out)
	return out
}
func (m FeatureMatrix) JSON() ([]byte, error) { return json.MarshalIndent(m, "", "  ") }

// ReferenceAdapters returns independently versioned descriptors for the four
// ecosystems covered by the conformance contract.
func ReferenceAdapters() []Adapter {
	all := []schema.Kind{schema.KindSchema, schema.KindTable, schema.KindColumn, schema.KindIndex, schema.KindForeignKey}
	return []Adapter{{Name: "autosql-go-orm", Language: Go, Version: "1.0.0", Supported: all}, {Name: "autosql-python-orm", Language: Python, Version: "1.0.0", Supported: all}, {Name: "autosql-typescript-orm", Language: TypeScript, Version: "1.0.0", Supported: all}, {Name: "autosql-java-orm", Language: Java, Version: "1.0.0", Supported: all}}
}
