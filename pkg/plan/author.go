package plan

import (
	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"encoding/json"
	"fmt"
	"strings"
)

type AuthorSQL struct {
	ID, SQL       string
	Transactional bool
}

// AppendAuthorSQL binds reviewed business/data reversal SQL into the immutable
// plan as synthetic reference-data changes. The statements therefore receive
// the same digest, safety, precheck, approval and executor treatment as DDL.
func AppendAuthorSQL(p Plan, input []AuthorSQL) (Plan, error) {
	if len(input) == 0 {
		return p, nil
	}
	changes := p.Changes
	rendered := p.renderedStatements()
	depends := []string{}
	if len(changes.Changes) > 0 {
		depends = []string{changes.Changes[len(changes.Changes)-1].ID}
	}
	for i, x := range input {
		if strings.TrimSpace(x.ID) == "" || strings.TrimSpace(x.SQL) == "" {
			return Plan{}, fmt.Errorf("%w: author SQL", ErrInvalidPlan)
		}
		name := schema.Name{Name: "down_reverse_" + stableID("author", x.ID)[7:]}
		resource := schema.Resource{ID: schema.StableID(schema.KindReferenceData, name), Kind: schema.KindReferenceData, Name: name, Spec: json.RawMessage(`{"state":"before"}`)}
		after := resource
		after.Spec = json.RawMessage(`{"state":"reversed"}`)
		change := schema.Change{ID: stableID("change", "author", x.ID, x.SQL), Operation: schema.OperationAlter, ResourceID: resource.ID, Before: &resource, After: &after, DependsOn: append([]string(nil), depends...), Details: json.RawMessage(`{"origin":"trusted_author_reverse"}`)}
		changes.Changes = append(changes.Changes, change)
		rendered = append(rendered, plugin.Statement{SQL: x.SQL, ChangeID: change.ID, Transactional: x.Transactional, Kind: plugin.StatementExecutable})
		depends = []string{change.ID}
		_ = i
	}
	steps, e := bindSteps(changes, rendered, p.Replay)
	if e != nil {
		return Plan{}, e
	}
	p.Changes, p.Steps = changes, steps
	p.Phases = phases(steps)
	p.Digest, e = digestPlan(p)
	if e != nil {
		return Plan{}, e
	}
	if e = p.Validate(); e != nil {
		return Plan{}, e
	}
	return p, nil
}
