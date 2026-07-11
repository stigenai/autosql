// Package schema defines AutoSQL's database-independent canonical schema and
// change representations.
package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	// SchemaVersion is the only schema document version understood by this release.
	SchemaVersion = "autosql.schema/v1"
	// ChangeVersion is the only change document version understood by this release.
	ChangeVersion = "autosql.changes/v1"
)

// Kind identifies a resource type. Kind values are wire-format API and must
// not be renamed within a document version.
type Kind string

const (
	KindDatabase         Kind = "database"
	KindSchema           Kind = "schema"
	KindExtension        Kind = "extension"
	KindEnum             Kind = "enum"
	KindDomain           Kind = "domain"
	KindComposite        Kind = "composite_type"
	KindSequence         Kind = "sequence"
	KindTable            Kind = "table"
	KindColumn           Kind = "column"
	KindPrimaryKey       Kind = "primary_key"
	KindUniqueConstraint Kind = "unique_constraint"
	KindCheckConstraint  Kind = "check_constraint"
	KindForeignKey       Kind = "foreign_key"
	KindIndex            Kind = "index"
	KindView             Kind = "view"
	KindMaterializedView Kind = "materialized_view"
	KindFunction         Kind = "function"
	KindProcedure        Kind = "procedure"
	KindTrigger          Kind = "trigger"
	KindPolicy           Kind = "policy"
	KindRole             Kind = "role"
	KindGrant            Kind = "grant"
	KindReferenceData    Kind = "reference_data"
)

var knownKinds = map[Kind]struct{}{
	KindDatabase: {}, KindSchema: {}, KindExtension: {}, KindEnum: {}, KindDomain: {},
	KindComposite: {}, KindSequence: {}, KindTable: {}, KindColumn: {}, KindPrimaryKey: {},
	KindUniqueConstraint: {}, KindCheckConstraint: {}, KindForeignKey: {}, KindIndex: {},
	KindView: {}, KindMaterializedView: {}, KindFunction: {}, KindProcedure: {}, KindTrigger: {},
	KindPolicy: {}, KindRole: {}, KindGrant: {}, KindReferenceData: {},
}

// IsKnownKind reports whether this library can safely interpret kind.
func IsKnownKind(kind Kind) bool { _, ok := knownKinds[kind]; return ok }

// Name is a portable qualified resource name. Parent contains the stable ID
// of the containing resource for objects such as columns and constraints.
type Name struct {
	Catalog string                     `json:"catalog,omitempty"`
	Schema  string                     `json:"schema,omitempty"`
	Name    string                     `json:"name"`
	Parent  string                     `json:"parent,omitempty"`
	Extra   map[string]json.RawMessage `json:"-"`
}

// String returns a human-readable, unquoted qualified name.
func (n Name) String() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{n.Catalog, n.Schema, n.Name} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ".")
}

// StableID derives a stable opaque ID from a kind and logical identity. Names
// are case-sensitive because quoted PostgreSQL identifiers are case-sensitive.
func StableID(kind Kind, name Name) string {
	identity := strings.Join([]string{string(kind), name.Catalog, name.Schema, name.Parent, name.Name}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return string(kind) + ":" + hex.EncodeToString(sum[:12])
}

// SourceLocation points diagnostics back to desired-state input.
type SourceLocation struct {
	URI       string                     `json:"uri"`
	Line      int                        `json:"line,omitempty"`
	Column    int                        `json:"column,omitempty"`
	EndLine   int                        `json:"end_line,omitempty"`
	EndColumn int                        `json:"end_column,omitempty"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// DependencyType describes why an ordering edge exists.
type DependencyType string

const (
	DependencyContains   DependencyType = "contains"
	DependencyReferences DependencyType = "references"
	DependencyUses       DependencyType = "uses"
	DependencyOwns       DependencyType = "owns"
)

// Dependency is a directed edge from the enclosing resource to Target.
type Dependency struct {
	Target string                     `json:"target"`
	Type   DependencyType             `json:"type"`
	Extra  map[string]json.RawMessage `json:"-"`
}

// Resource is the canonical unit of desired or inspected state. Spec is a
// kind-specific JSON object. Unknown resource fields are retained in Extra so
// newer producers can safely round-trip through an older consumer.
type Resource struct {
	ID           string                     `json:"id"`
	Kind         Kind                       `json:"kind"`
	Name         Name                       `json:"name"`
	Dependencies []Dependency               `json:"dependencies,omitempty"`
	Annotations  map[string]string          `json:"annotations,omitempty"`
	Source       *SourceLocation            `json:"source,omitempty"`
	Spec         json.RawMessage            `json:"spec,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
}

// Graph contains resources. Edges are stored on resources to make ownership
// and references portable without relying on array position.
type Graph struct {
	Resources []Resource                 `json:"resources"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// Document is the versioned schema wire format.
type Document struct {
	Version     string                     `json:"version"`
	Graph       Graph                      `json:"graph"`
	Annotations map[string]string          `json:"annotations,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

// Operation is a canonical change operation.
type Operation string

const (
	OperationCreate Operation = "create"
	OperationAlter  Operation = "alter"
	OperationDrop   Operation = "drop"
	OperationRename Operation = "rename"
)

// Change describes one transition. Before and After are complete resource
// snapshots when applicable; Details carries operation-specific information.
type Change struct {
	ID          string                     `json:"id"`
	Operation   Operation                  `json:"operation"`
	ResourceID  string                     `json:"resource_id"`
	Before      *Resource                  `json:"before,omitempty"`
	After       *Resource                  `json:"after,omitempty"`
	DependsOn   []string                   `json:"depends_on,omitempty"`
	Details     json.RawMessage            `json:"details,omitempty"`
	Annotations map[string]string          `json:"annotations,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

// ChangeSet is the versioned change-plan wire format.
type ChangeSet struct {
	Version     string                     `json:"version"`
	Changes     []Change                   `json:"changes"`
	Annotations map[string]string          `json:"annotations,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

var (
	ErrUnsupportedVersion = errors.New("unsupported document version")
	ErrUnsupportedKind    = errors.New("unsupported resource kind")
	ErrInvalidDocument    = errors.New("invalid schema document")
)

// Validate verifies identity, references, kinds, source positions, specs, and
// dependency acyclicity. Unknown fields are intentionally not rejected.
func (d Document) Validate() error {
	if d.Version != SchemaVersion {
		return fmt.Errorf("%w: %q (supported: %q)", ErrUnsupportedVersion, d.Version, SchemaVersion)
	}
	ids := make(map[string]Resource, len(d.Graph.Resources))
	for i, r := range d.Graph.Resources {
		if !IsKnownKind(r.Kind) {
			return fmt.Errorf("%w: resources[%d] %q", ErrUnsupportedKind, i, r.Kind)
		}
		if r.Name.Name == "" {
			return fmt.Errorf("%w: resources[%d].name.name is required", ErrInvalidDocument, i)
		}
		if r.ID == "" {
			return fmt.Errorf("%w: resources[%d].id is required", ErrInvalidDocument, i)
		}
		if r.ID != StableID(r.Kind, r.Name) {
			return fmt.Errorf("%w: resources[%d].id %q does not match stable identity %q", ErrInvalidDocument, i, r.ID, StableID(r.Kind, r.Name))
		}
		if _, exists := ids[r.ID]; exists {
			return fmt.Errorf("%w: duplicate resource id %q", ErrInvalidDocument, r.ID)
		}
		if len(r.Spec) > 0 && !validJSONObject(r.Spec) {
			return fmt.Errorf("%w: resource %q spec must be a JSON object", ErrInvalidDocument, r.ID)
		}
		if r.Source != nil && (r.Source.URI == "" || r.Source.Line < 0 || r.Source.Column < 0 || r.Source.EndLine < 0 || r.Source.EndColumn < 0) {
			return fmt.Errorf("%w: resource %q has invalid source location", ErrInvalidDocument, r.ID)
		}
		ids[r.ID] = r
	}
	adj := make(map[string][]string, len(ids))
	for _, r := range d.Graph.Resources {
		if r.Name.Parent != "" {
			if _, ok := ids[r.Name.Parent]; !ok {
				return fmt.Errorf("%w: resource %q parent %q does not exist", ErrInvalidDocument, r.ID, r.Name.Parent)
			}
		}
		seen := map[string]bool{}
		for _, dep := range r.Dependencies {
			switch dep.Type {
			case DependencyContains, DependencyReferences, DependencyUses, DependencyOwns:
			default:
				return fmt.Errorf("%w: resource %q has unsupported dependency type %q", ErrInvalidDocument, r.ID, dep.Type)
			}
			if _, ok := ids[dep.Target]; !ok {
				return fmt.Errorf("%w: resource %q dependency %q does not exist", ErrInvalidDocument, r.ID, dep.Target)
			}
			if dep.Target == r.ID {
				return fmt.Errorf("%w: resource %q depends on itself", ErrInvalidDocument, r.ID)
			}
			key := string(dep.Type) + "\x00" + dep.Target
			if seen[key] {
				return fmt.Errorf("%w: resource %q has duplicate dependency on %q", ErrInvalidDocument, r.ID, dep.Target)
			}
			seen[key] = true
			adj[r.ID] = append(adj[r.ID], dep.Target)
		}
	}
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("%w: dependency cycle at %q", ErrInvalidDocument, id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, next := range adj[id] {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func validJSONObject(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	_, ok := v.(map[string]any)
	return ok
}

// Validate verifies the change document shape and resource snapshots.
func (c ChangeSet) Validate() error {
	if c.Version != ChangeVersion {
		return fmt.Errorf("%w: %q (supported: %q)", ErrUnsupportedVersion, c.Version, ChangeVersion)
	}
	ids := map[string]bool{}
	for i, ch := range c.Changes {
		if ch.ID == "" || ch.ResourceID == "" {
			return fmt.Errorf("%w: changes[%d] id and resource_id are required", ErrInvalidDocument, i)
		}
		if ids[ch.ID] {
			return fmt.Errorf("%w: duplicate change id %q", ErrInvalidDocument, ch.ID)
		}
		ids[ch.ID] = true
		switch ch.Operation {
		case OperationCreate:
			if ch.After == nil || ch.Before != nil {
				return fmt.Errorf("%w: create %q requires only after", ErrInvalidDocument, ch.ID)
			}
		case OperationDrop:
			if ch.Before == nil || ch.After != nil {
				return fmt.Errorf("%w: drop %q requires only before", ErrInvalidDocument, ch.ID)
			}
		case OperationAlter, OperationRename:
			if ch.Before == nil || ch.After == nil {
				return fmt.Errorf("%w: %s %q requires before and after", ErrInvalidDocument, ch.Operation, ch.ID)
			}
		default:
			return fmt.Errorf("%w: change %q has unsupported operation %q", ErrInvalidDocument, ch.ID, ch.Operation)
		}
		for _, r := range []*Resource{ch.Before, ch.After} {
			if r != nil {
				if !IsKnownKind(r.Kind) {
					return fmt.Errorf("%w: change %q: %q", ErrUnsupportedKind, ch.ID, r.Kind)
				}
				if r.Name.Name == "" || r.ID == "" || r.ID != StableID(r.Kind, r.Name) {
					return fmt.Errorf("%w: change %q contains invalid resource identity", ErrInvalidDocument, ch.ID)
				}
				if ch.ResourceID != r.ID {
					return fmt.Errorf("%w: change %q resource_id does not match snapshot", ErrInvalidDocument, ch.ID)
				}
				if len(r.Spec) > 0 && !validJSONObject(r.Spec) {
					return fmt.Errorf("%w: change %q resource spec must be a JSON object", ErrInvalidDocument, ch.ID)
				}
			}
		}
		if len(ch.Details) > 0 && !validJSONObject(ch.Details) {
			return fmt.Errorf("%w: change %q details must be a JSON object", ErrInvalidDocument, ch.ID)
		}
	}
	for _, ch := range c.Changes {
		for _, dep := range ch.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("%w: change %q depends on missing change %q", ErrInvalidDocument, ch.ID, dep)
			}
		}
	}
	return nil
}

// Normalize sorts set-like collections. It does not reorder change sequences,
// which may carry intentional execution order.
func (d *Document) Normalize() {
	for i := range d.Graph.Resources {
		r := &d.Graph.Resources[i]
		sort.Slice(r.Dependencies, func(i, j int) bool {
			if r.Dependencies[i].Type == r.Dependencies[j].Type {
				return r.Dependencies[i].Target < r.Dependencies[j].Target
			}
			return r.Dependencies[i].Type < r.Dependencies[j].Type
		})
	}
	sort.Slice(d.Graph.Resources, func(i, j int) bool { return d.Graph.Resources[i].ID < d.Graph.Resources[j].ID })
}

// MarshalCanonical emits compact, deterministic JSON with a trailing newline.
func (d Document) MarshalCanonical() ([]byte, error) {
	d.Normalize()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// MarshalCanonical emits deterministic JSON while preserving change order.
func (c ChangeSet) MarshalCanonical() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
