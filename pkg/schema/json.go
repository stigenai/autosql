package schema

import (
	"bytes"
	"encoding/json"
)

func marshalWithExtra(known map[string]any, extra map[string]json.RawMessage) ([]byte, error) {
	all := make(map[string]any, len(known)+len(extra))
	for k, v := range extra {
		decoded, err := decodeRaw(v)
		if err != nil {
			return nil, err
		}
		all[k] = decoded
	}
	for k, v := range known {
		all[k] = v
	}
	return json.Marshal(all)
}

func decodeRaw(raw json.RawMessage) (any, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
func unmarshalObject(data []byte, dst any) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, dst); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	return all, nil
}
func remove(m map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	for _, k := range keys {
		delete(m, k)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func (n Name) MarshalJSON() ([]byte, error) {
	known := map[string]any{"name": n.Name}
	if n.Catalog != "" {
		known["catalog"] = n.Catalog
	}
	if n.Schema != "" {
		known["schema"] = n.Schema
	}
	if n.Parent != "" {
		known["parent"] = n.Parent
	}
	return marshalWithExtra(known, n.Extra)
}
func (n *Name) UnmarshalJSON(b []byte) error {
	type plain Name
	var p plain
	all, e := unmarshalObject(b, &p)
	if e != nil {
		return e
	}
	*n = Name(p)
	n.Extra = remove(all, "catalog", "schema", "name", "parent")
	return nil
}
func (s SourceLocation) MarshalJSON() ([]byte, error) {
	known := map[string]any{"uri": s.URI}
	if s.Line != 0 {
		known["line"] = s.Line
	}
	if s.Column != 0 {
		known["column"] = s.Column
	}
	if s.EndLine != 0 {
		known["end_line"] = s.EndLine
	}
	if s.EndColumn != 0 {
		known["end_column"] = s.EndColumn
	}
	return marshalWithExtra(known, s.Extra)
}
func (s *SourceLocation) UnmarshalJSON(b []byte) error {
	type plain SourceLocation
	var p plain
	all, e := unmarshalObject(b, &p)
	if e != nil {
		return e
	}
	*s = SourceLocation(p)
	s.Extra = remove(all, "uri", "line", "column", "end_line", "end_column")
	return nil
}
func (d Dependency) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(map[string]any{"target": d.Target, "type": d.Type}, d.Extra)
}
func (d *Dependency) UnmarshalJSON(b []byte) error {
	type plain Dependency
	var p plain
	all, e := unmarshalObject(b, &p)
	if e != nil {
		return e
	}
	*d = Dependency(p)
	d.Extra = remove(all, "target", "type")
	return nil
}
func (g Graph) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(map[string]any{"resources": g.Resources}, g.Extra)
}
func (g *Graph) UnmarshalJSON(b []byte) error {
	type plain Graph
	var p plain
	all, e := unmarshalObject(b, &p)
	if e != nil {
		return e
	}
	*g = Graph(p)
	g.Extra = remove(all, "resources")
	return nil
}

func (r Resource) MarshalJSON() ([]byte, error) {
	known := map[string]any{"id": r.ID, "kind": r.Kind, "name": r.Name}
	if len(r.Dependencies) > 0 {
		known["dependencies"] = r.Dependencies
	}
	if len(r.Annotations) > 0 {
		known["annotations"] = r.Annotations
	}
	if r.Source != nil {
		known["source"] = r.Source
	}
	if len(r.Spec) > 0 {
		v, err := decodeRaw(r.Spec)
		if err != nil {
			return nil, err
		}
		known["spec"] = v
	}
	return marshalWithExtra(known, r.Extra)
}
func (r *Resource) UnmarshalJSON(b []byte) error {
	type plain Resource
	var p plain
	all, err := unmarshalObject(b, &p)
	if err != nil {
		return err
	}
	*r = Resource(p)
	r.Extra = remove(all, "id", "kind", "name", "dependencies", "annotations", "source", "spec")
	return nil
}
func (d Document) MarshalJSON() ([]byte, error) {
	known := map[string]any{"version": d.Version, "graph": d.Graph}
	if len(d.Annotations) > 0 {
		known["annotations"] = d.Annotations
	}
	return marshalWithExtra(known, d.Extra)
}
func (d *Document) UnmarshalJSON(b []byte) error {
	type plain Document
	var p plain
	all, e := unmarshalObject(b, &p)
	if e != nil {
		return e
	}
	*d = Document(p)
	d.Extra = remove(all, "version", "graph", "annotations")
	return nil
}
func (c Change) MarshalJSON() ([]byte, error) {
	known := map[string]any{"id": c.ID, "operation": c.Operation, "resource_id": c.ResourceID}
	if c.Before != nil {
		known["before"] = c.Before
	}
	if c.After != nil {
		known["after"] = c.After
	}
	if len(c.DependsOn) > 0 {
		known["depends_on"] = c.DependsOn
	}
	if len(c.Details) > 0 {
		v, err := decodeRaw(c.Details)
		if err != nil {
			return nil, err
		}
		known["details"] = v
	}
	if len(c.Annotations) > 0 {
		known["annotations"] = c.Annotations
	}
	return marshalWithExtra(known, c.Extra)
}
func (c *Change) UnmarshalJSON(b []byte) error {
	type plain Change
	var p plain
	all, e := unmarshalObject(b, &p)
	if e != nil {
		return e
	}
	*c = Change(p)
	c.Extra = remove(all, "id", "operation", "resource_id", "before", "after", "depends_on", "details", "annotations")
	return nil
}
func (c ChangeSet) MarshalJSON() ([]byte, error) {
	known := map[string]any{"version": c.Version, "changes": c.Changes}
	if len(c.Annotations) > 0 {
		known["annotations"] = c.Annotations
	}
	return marshalWithExtra(known, c.Extra)
}
func (c *ChangeSet) UnmarshalJSON(b []byte) error {
	type plain ChangeSet
	var p plain
	all, e := unmarshalObject(b, &p)
	if e != nil {
		return e
	}
	*c = ChangeSet(p)
	c.Extra = remove(all, "version", "changes", "annotations")
	return nil
}

// DecodeDocument parses a document, retains unknown fields, and validates the
// known model. Unsupported resource kinds are rejected as unsafe to operate on.
func DecodeDocument(data []byte) (Document, error) {
	var d Document
	if err := json.Unmarshal(data, &d); err != nil {
		return d, err
	}
	if err := d.Validate(); err != nil {
		return d, err
	}
	return d, nil
}

// DecodeChangeSet parses and validates a versioned change set.
func DecodeChangeSet(data []byte) (ChangeSet, error) {
	var c ChangeSet
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}
