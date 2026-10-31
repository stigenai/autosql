package dbtest

import (
	"context"
	"strconv"

	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/source"
)

// SchemaProgram captures a renderable blank-to-desired schema before allocation.
// It is test-only data; execution still requires an owned DisposableTarget.
type SchemaProgram struct {
	ready   bool
	major   int
	digest  string
	batches []sqlBatch
}

func (p SchemaProgram) Digest() string { return p.digest }

func PrepareSchema(ctx context.Context, doc schema.Document, major int) (SchemaProgram, error) {
	if major < 14 || major > 18 {
		return SchemaProgram{}, selectionFailure("PostgreSQL major must be 14..18")
	}
	normalized, err := postgres.New().Normalize(ctx, doc)
	if err != nil {
		return SchemaProgram{}, selectionFailure("desired schema cannot be normalized")
	}
	doc = normalized
	baseline := schema.Document{Version: doc.Version, Graph: schema.Graph{Extra: doc.Graph.Extra}, Annotations: doc.Annotations, Extra: doc.Extra}
	// template0 already supplies public. Match the existing simulation baseline
	// without resolving any target or granting the fixture administrator access.
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindSchema && resource.Name.Name == "public" && resource.Name.Schema == "" {
			baseline.Graph.Resources = append(baseline.Graph.Resources, schema.Resource{ID: resource.ID, Kind: resource.Kind, Name: resource.Name, Spec: []byte(`{}`)})
			break
		}
	}
	p, err := plan.Build(ctx, postgres.New(), baseline, doc, plan.Options{Render: map[string]string{"postgres_version": strconv.Itoa(major)}})
	if err != nil {
		return SchemaProgram{}, &SuiteError{Failure: CaseFailure{Kind: "validation", Code: 5, Stage: "schema", Message: "desired schema cannot be rendered for the requested PostgreSQL major"}, cause: err}
	}
	return SchemaProgram{ready: true, major: major, digest: p.Digest, batches: schemaPlanBatches(p)}, nil
}

func schemaPlanBatches(p plan.Plan) []sqlBatch {
	steps := map[string]plan.Step{}
	for _, step := range p.Steps {
		steps[step.ID] = step
	}
	result := make([]sqlBatch, 0, len(p.Phases))
	for _, phase := range p.Phases {
		batch := sqlBatch{transactional: phase.Transaction == plan.TransactionRequired}
		for _, id := range phase.StepIDs {
			step := steps[id]
			if step.Kind == plan.StepExecutable {
				batch.statements = append(batch.statements, source.Statement{SQL: step.SQL, Position: source.Position{Line: 1, Column: 1}})
			}
		}
		result = append(result, batch)
	}
	return result
}

func (d *DisposableTarget) prepareSchema(ctx context.Context, program SchemaProgram) (*CaseFailure, []CaseFailure) {
	if !program.ready || program.major != d.PostgresMajor() {
		failure := testFailure("schema", Operation{}, ErrFactoryConfig)
		return &failure, nil
	}
	for _, batch := range program.batches {
		primary, cleanup := d.executeBatch(ctx, batch, "schema")
		if primary != nil || len(cleanup) > 0 {
			return primary, cleanup
		}
	}
	return nil, nil
}
