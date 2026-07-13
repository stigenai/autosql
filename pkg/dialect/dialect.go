// Package dialect defines capability-driven database dialect contracts and
// initial MySQL and SQL Server descriptors.
package dialect

import (
	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrUnsupported = errors.New("dialect capability unsupported")

type Name string

const (
	MySQL     Name = "mysql"
	SQLServer Name = "sqlserver"
)

type Descriptor struct {
	Name     Name
	Info     plugin.Info
	Features map[string]bool
}

func MySQLDescriptor() Descriptor {
	return descriptor(MySQL, []schema.Kind{schema.KindSchema, schema.KindTable, schema.KindColumn, schema.KindIndex, schema.KindView, schema.KindRole, schema.KindGrant, schema.KindReferenceData}, []string{"table.generated_columns", "index.prefix", "security.host_scoped_users", "data.upsert"})
}
func SQLServerDescriptor() Descriptor {
	return descriptor(SQLServer, []schema.Kind{schema.KindSchema, schema.KindTable, schema.KindColumn, schema.KindIndex, schema.KindView, schema.KindFunction, schema.KindRole, schema.KindGrant, schema.KindReferenceData}, []string{"schema.scoped_roles", "execute.grants", "data.merge"})
}
func descriptor(n Name, kinds []schema.Kind, features []string) Descriptor {
	caps := make([]plugin.Capability, 0, len(kinds))
	for _, k := range kinds {
		caps = append(caps, plugin.Capability{Kind: k, Mode: plugin.Managed, Operations: []schema.Operation{schema.OperationCreate, schema.OperationAlter, schema.OperationDrop, schema.OperationRename}})
	}
	fm := map[string]bool{}
	for _, f := range features {
		fm[f] = true
	}
	return Descriptor{Name: n, Info: plugin.Info{Name: string(n), Version: "0.1.0", APIVersion: plugin.HostAPIVersion, Capabilities: caps}, Features: fm}
}
func (d Descriptor) Supports(kind schema.Kind, op schema.Operation) bool {
	c := d.Info.Capability(kind)
	for _, x := range c.Operations {
		if x == op {
			return c.Mode == plugin.Managed
		}
	}
	return false
}
func (d Descriptor) Require(kind schema.Kind, op schema.Operation) error {
	if !d.Supports(kind, op) {
		return fmt.Errorf("%w: %s does not support %s %s", ErrUnsupported, d.Name, op, kind)
	}
	return nil
}
func (d Descriptor) Normalize(_ context.Context, doc schema.Document) (schema.Document, error) {
	if doc.Annotations == nil {
		doc.Annotations = map[string]string{}
	}
	if old := doc.Annotations["dialect"]; old != "" && !strings.EqualFold(old, string(d.Name)) {
		return schema.Document{}, fmt.Errorf("%w: document dialect is %s", ErrUnsupported, old)
	}
	doc.Annotations["dialect"] = string(d.Name)
	doc.Normalize()
	if err := doc.Validate(); err != nil {
		return schema.Document{}, err
	}
	for _, r := range doc.Graph.Resources {
		if !d.Supports(r.Kind, schema.OperationCreate) {
			return schema.Document{}, fmt.Errorf("%w: resource kind %s", ErrUnsupported, r.Kind)
		}
	}
	return doc, nil
}

type Statement struct {
	SQL           string
	ChangeID      string
	Transactional bool
}

func (d Descriptor) Render(_ context.Context, cs schema.ChangeSet) ([]Statement, error) {
	if cs.Version != schema.ChangeVersion {
		return nil, errors.New("unsupported changeset version")
	}
	out := []Statement{}
	for _, c := range cs.Changes {
		var kind schema.Kind
		if c.After != nil {
			kind = c.After.Kind
		} else if c.Before != nil {
			kind = c.Before.Kind
		}
		if err := d.Require(kind, c.Operation); err != nil {
			return nil, err
		}
		verb := strings.ToUpper(string(c.Operation))
		out = append(out, Statement{SQL: fmt.Sprintf("-- %s %s %s", verb, kind, c.ResourceID), ChangeID: c.ID, Transactional: true})
	}
	return out, nil
}
func RoundTrip(doc schema.Document) (schema.Document, error) {
	b, e := json.Marshal(doc)
	if e != nil {
		return schema.Document{}, e
	}
	var out schema.Document
	if e = json.Unmarshal(b, &out); e != nil {
		return schema.Document{}, e
	}
	return out, nil
}
