// Package postgres implements PostgreSQL live database inspection.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

const version = "0.1.0"

// Driver inspects PostgreSQL databases. It is safe for concurrent use.
type Driver struct{}

// New returns a PostgreSQL inspection driver.
func New() *Driver { return &Driver{} }

// Options controls the subset and capability level inspected by InspectURL.
// Include and Exclude use path.Match-style patterns against names such as
// "public.users" and "table:public.users". Advanced enables role and grant
// inspection, which can require broader catalog privileges.
type Options struct {
	Schemas  []string
	Include  []string
	Exclude  []string
	Advanced bool
}

// InspectURL is the convenient public API for inspecting a live database.
func InspectURL(ctx context.Context, url string, opts Options) (schema.Document, error) {
	request := plugin.InspectRequest{
		URL:     url,
		Schemas: append([]string(nil), opts.Schemas...),
		Options: map[string]string{
			"include": strings.Join(opts.Include, ","),
			"exclude": strings.Join(opts.Exclude, ","),
		},
	}
	if opts.Advanced {
		request.Options["roles"] = "true"
		request.Options["grants"] = "true"
	}
	return inspect(ctx, request)
}

func (*Driver) Info() plugin.Info {
	kinds := []schema.Kind{
		schema.KindSchema, schema.KindExtension, schema.KindEnum,
		schema.KindDomain, schema.KindComposite, schema.KindSequence, schema.KindTable,
		schema.KindColumn, schema.KindPrimaryKey, schema.KindUniqueConstraint,
		schema.KindCheckConstraint, schema.KindForeignKey, schema.KindIndex, schema.KindView,
		schema.KindMaterializedView, schema.KindFunction, schema.KindProcedure,
		schema.KindTrigger, schema.KindPolicy, schema.KindRole, schema.KindGrant,
	}
	caps := make([]plugin.Capability, 0, len(kinds))
	for _, kind := range kinds {
		caps = append(caps, plugin.Capability{Kind: kind, Mode: plugin.ReadOnly})
	}
	return plugin.Info{Name: "postgres", Version: version, APIVersion: plugin.HostAPIVersion, Capabilities: caps}
}

func (d *Driver) Inspect(ctx context.Context, req plugin.InspectRequest) (schema.Document, error) {
	return inspect(ctx, req)
}

func (*Driver) Normalize(_ context.Context, doc schema.Document) (schema.Document, error) {
	doc.Normalize()
	if err := doc.Validate(); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	return doc, nil
}

func (*Driver) Render(context.Context, plugin.RenderRequest) ([]plugin.Statement, error) {
	return nil, fmt.Errorf("render PostgreSQL changes: %w", plugin.ErrUnsupported)
}

func enabled(options map[string]string, key string, defaultValue bool) bool {
	v, ok := options[key]
	if !ok {
		return defaultValue
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return defaultValue
	}
}

var ErrPermission = errors.New("PostgreSQL inspection permission denied")

// PermissionError describes an object that the connected role cannot inspect.
type PermissionError struct {
	Resource  string
	Privilege string
	Cause     error
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("%v: cannot inspect %s; grant %s to the inspection role", ErrPermission, e.Resource, e.Privilege)
}
func (e *PermissionError) Unwrap() error        { return e.Cause }
func (e *PermissionError) Is(target error) bool { return target == ErrPermission }
