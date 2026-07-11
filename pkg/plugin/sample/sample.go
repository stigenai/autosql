// Package sample provides a small deterministic plugin used as executable
// documentation and by the public conformance suite.
package sample

import (
	"context"
	"fmt"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

type Driver struct{ Document schema.Document }

func (Driver) Info() plugin.Info {
	return plugin.Info{Name: "sample-db", Version: "1.0.0", APIVersion: plugin.HostAPIVersion, Capabilities: []plugin.Capability{{Kind: schema.KindSchema, Mode: plugin.Managed}, {Kind: schema.KindTable, Mode: plugin.Managed}, {Kind: schema.KindView, Mode: plugin.ReadOnly}}}
}
func (d Driver) Inspect(ctx context.Context, _ plugin.InspectRequest) (schema.Document, error) {
	if err := ctx.Err(); err != nil {
		return schema.Document{}, err
	}
	return d.Document, nil
}
func (d Driver) Normalize(ctx context.Context, in schema.Document) (schema.Document, error) {
	if err := ctx.Err(); err != nil {
		return schema.Document{}, err
	}
	in.Normalize()
	return in, nil
}
func (d Driver) Render(ctx context.Context, r plugin.RenderRequest) ([]plugin.Statement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]plugin.Statement, 0, len(r.Changes.Changes))
	for _, ch := range r.Changes.Changes {
		var kind schema.Kind
		if ch.After != nil {
			kind = ch.After.Kind
		} else if ch.Before != nil {
			kind = ch.Before.Kind
		}
		if err := plugin.RequireManaged(d.Info(), kind); err != nil {
			return nil, err
		}
		out = append(out, plugin.Statement{SQL: fmt.Sprintf("-- sample %s %s", ch.Operation, ch.ResourceID), ChangeID: ch.ID, Transactional: true})
	}
	return out, nil
}

type Source struct{ Document schema.Document }

func (Source) Info() plugin.Info {
	return plugin.Info{Name: "sample-source", Version: "1.0.0", APIVersion: plugin.HostAPIVersion, Capabilities: []plugin.Capability{{Kind: schema.KindSchema, Mode: plugin.ReadOnly}, {Kind: schema.KindTable, Mode: plugin.ReadOnly}}}
}
func (s Source) Load(ctx context.Context, _ plugin.SourceRequest) (schema.Document, error) {
	if err := ctx.Err(); err != nil {
		return schema.Document{}, err
	}
	return s.Document, nil
}
