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
	preconditions := managedBootstrapIntrinsicResources(target, desired)
	current := schema.Document{
		Version: desired.Version,
		Graph: schema.Graph{
			Resources: preconditions,
			Extra:     desired.Graph.Extra,
		},
		Annotations: desired.Annotations,
		Extra:       desired.Extra,
	}
	whole, err := PlanDatabaseTransition(ctx, target, current, desired, options)
	if err != nil {
		return bootstrap.Plan{}, err
	}
	if len(preconditions) == 0 {
		return whole, nil
	}
	return whole.WithPreconditions(preconditions)
}

// managedBootstrapIntrinsicResources models resources that must exist before
// in-database bootstrap begins. PostgreSQL creates public with CREATE DATABASE.
// A custom database owner must exist before CREATE DATABASE and, when declared
// as a managed role, is an exact signed prerequisite rather than a CREATE ROLE
// step that could only run after the database already exists.
func managedBootstrapIntrinsicResources(target bootstrap.DatabaseTarget, desired schema.Document) []schema.Resource {
	if target.Mode != bootstrap.ManagedDatabase {
		return nil
	}
	var intrinsic []schema.Resource
	for _, resource := range desired.Graph.Resources {
		switch {
		case resource.Kind == schema.KindSchema && resource.Name.Name == "public" && resource.Name.Schema == "":
			intrinsic = append(intrinsic, schema.Resource{
				ID:   schema.StableID(schema.KindSchema, resource.Name),
				Kind: schema.KindSchema,
				Name: resource.Name,
				Spec: []byte(`{}`),
			})
		case resource.Kind == schema.KindRole && resource.Name.Schema == "" && resource.Name.Name == target.Owner:
			intrinsic = append(intrinsic, resource)
		}
	}
	return intrinsic
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
	whole, err := bootstrap.ComposePlan(target, schemaPlan)
	if err != nil {
		return bootstrap.Plan{}, err
	}
	if enabled(options.Render, "allow_untrusted_extensions", false) {
		whole = whole.WithRuntimeAuthorization(sealBootstrapExtensionAuthorization(whole.Digest, "*"))
	}
	return whole, nil
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
