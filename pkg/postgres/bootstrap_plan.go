package postgres

import (
	"context"
	"fmt"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/schema"
)

// PlanDatabaseBootstrap creates one immutable plan for the out-of-transaction
// database preparation boundary and the complete desired resource graph. A
// matching database resource may be present in desired HCL; it is consumed as
// the target contract rather than sent to the in-database renderer.
func PlanDatabaseBootstrap(ctx context.Context, target bootstrap.DatabaseTarget, desired schema.Document, options plan.Options) (bootstrap.Plan, error) {
	target = target.Normalize()
	if err := target.Validate(); err != nil {
		return bootstrap.Plan{}, err
	}
	var err error
	desired, err = schemaDocumentWithoutDatabase(desired, target)
	if err != nil {
		return bootstrap.Plan{}, err
	}
	current := schema.Document{
		Version:     desired.Version,
		Graph:       schema.Graph{Extra: desired.Graph.Extra},
		Annotations: desired.Annotations,
		Extra:       desired.Extra,
	}
	return PlanDatabaseTransition(ctx, target, current, desired, options)
}

// PlanDatabaseTransition lifts any in-database convergence or teardown into
// the same whole-database phase/checkpoint contract used for first bootstrap.
func PlanDatabaseTransition(ctx context.Context, target bootstrap.DatabaseTarget, current, desired schema.Document, options plan.Options) (bootstrap.Plan, error) {
	target = target.Normalize()
	if err := target.Validate(); err != nil {
		return bootstrap.Plan{}, err
	}
	var err error
	current, err = schemaDocumentWithoutDatabase(current, target)
	if err != nil {
		return bootstrap.Plan{}, fmt.Errorf("current graph: %w", err)
	}
	desired, err = schemaDocumentWithoutDatabase(desired, target)
	if err != nil {
		return bootstrap.Plan{}, fmt.Errorf("desired graph: %w", err)
	}
	driver := New()
	current, err = driver.Normalize(ctx, current)
	if err != nil {
		return bootstrap.Plan{}, fmt.Errorf("current graph: %w", err)
	}
	desired, err = driver.Normalize(ctx, desired)
	if err != nil {
		return bootstrap.Plan{}, fmt.Errorf("desired graph: %w", err)
	}
	schemaPlan, err := plan.Build(ctx, driver, current, desired, options)
	if err != nil {
		return bootstrap.Plan{}, err
	}
	return bootstrap.ComposePlan(target, schemaPlan)
}

func schemaDocumentWithoutDatabase(document schema.Document, target bootstrap.DatabaseTarget) (schema.Document, error) {
	graph := document.Graph.Resources[:0:0]
	databaseCount := 0
	for _, resource := range document.Graph.Resources {
		if resource.Kind != schema.KindDatabase {
			graph = append(graph, resource)
			continue
		}
		databaseCount++
		declared, err := DatabaseTargetFromResource(resource)
		if err != nil {
			return schema.Document{}, fmt.Errorf("database resource: %w", err)
		}
		if !databaseTargetsEqual(declared, target) {
			return schema.Document{}, fmt.Errorf("database resource does not match bootstrap target")
		}
	}
	if databaseCount > 1 {
		return schema.Document{}, fmt.Errorf("whole-database plan accepts at most one database resource")
	}
	document.Graph.Resources = graph
	return document, nil
}

func databaseTargetsEqual(a, b bootstrap.DatabaseTarget) bool {
	a, b = a.Normalize(), b.Normalize()
	return a == b
}
