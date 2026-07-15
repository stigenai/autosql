package postgres

import (
	"context"
	"fmt"
	"sort"

	"autosql/pkg/schema"
)

type RoutinePrerequisite struct {
	ResourceID     string   `json:"resource_id"`
	Name           string   `json:"name"`
	Classification string   `json:"classification"`
	Extension      string   `json:"extension,omitempty"`
	Fingerprint    string   `json:"fingerprint"`
	RequiredBy     []string `json:"required_by"`
}

type RoutinePrerequisiteStatus struct {
	RoutinePrerequisite
	Status string `json:"status"`
}

type RoutinePrerequisiteReport struct {
	Satisfied     bool                        `json:"satisfied"`
	Prerequisites []RoutinePrerequisiteStatus `json:"prerequisites"`
}

func GeneratedRoutinePrerequisites(ctx context.Context, doc schema.Document) ([]RoutinePrerequisite, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	normalized, err := New().Normalize(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("classify generated routine prerequisites: %w", err)
	}
	resources := resourceMapForRender(normalized)
	byRoutine := map[string]*RoutinePrerequisite{}
	for _, column := range normalized.Graph.Resources {
		if column.Kind != schema.KindColumn || stringValue(spec(column), "generated") != "s" {
			continue
		}
		if err := validateGeneratedDependencies(column, resources); err != nil {
			return nil, err
		}
		for _, dependency := range column.Dependencies {
			routine, ok := resources[dependency.Target]
			if dependency.Type != schema.DependencyReferences || !ok || routine.Kind != schema.KindFunction {
				continue
			}
			entry := byRoutine[routine.ID]
			if entry == nil {
				fingerprint, err := schema.ResourceFingerprint(routine)
				if err != nil {
					return nil, err
				}
				classification := "application_external"
				extension := stringValue(spec(routine), "extension")
				if extension != "" {
					classification = "extension_external"
				}
				entry = &RoutinePrerequisite{ResourceID: routine.ID, Name: routine.Name.String(), Classification: classification, Extension: extension, Fingerprint: fingerprint}
				byRoutine[routine.ID] = entry
			}
			entry.RequiredBy = append(entry.RequiredBy, column.ID)
		}
	}
	out := make([]RoutinePrerequisite, 0, len(byRoutine))
	for _, prerequisite := range byRoutine {
		sort.Strings(prerequisite.RequiredBy)
		out = append(out, *prerequisite)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	return out, nil
}

func VerifyGeneratedRoutinePrerequisites(ctx context.Context, url string, desired schema.Document) (RoutinePrerequisiteReport, error) {
	prerequisites, err := GeneratedRoutinePrerequisites(ctx, desired)
	if err != nil {
		return RoutinePrerequisiteReport{}, err
	}
	if len(prerequisites) == 0 {
		return RoutinePrerequisiteReport{Satisfied: true}, nil
	}
	schemaSet := map[string]bool{}
	desiredResources := resourceMapForRender(desired)
	for _, prerequisite := range prerequisites {
		if routine, ok := desiredResources[prerequisite.ResourceID]; ok && routine.Name.Schema != "" {
			schemaSet[routine.Name.Schema] = true
		}
	}
	schemas := make([]string, 0, len(schemaSet))
	for name := range schemaSet {
		schemas = append(schemas, name)
	}
	sort.Strings(schemas)
	actual, err := InspectURL(ctx, url, Options{Schemas: schemas})
	if err != nil {
		return RoutinePrerequisiteReport{}, fmt.Errorf("verify generated routine prerequisites: %w", err)
	}
	actual, err = New().Normalize(ctx, actual)
	if err != nil {
		return RoutinePrerequisiteReport{}, fmt.Errorf("verify generated routine prerequisites: %w", err)
	}
	actualResources := resourceMapForRender(actual)
	report := RoutinePrerequisiteReport{Satisfied: true}
	for _, prerequisite := range prerequisites {
		status := RoutinePrerequisiteStatus{RoutinePrerequisite: prerequisite, Status: "missing"}
		if resource, ok := actualResources[prerequisite.ResourceID]; ok {
			fingerprint, fingerprintErr := schema.ResourceFingerprint(resource)
			if fingerprintErr != nil {
				return RoutinePrerequisiteReport{}, fingerprintErr
			}
			status.Status = "version_mismatch"
			if fingerprint == prerequisite.Fingerprint {
				status.Status = "satisfied"
			}
		}
		if status.Status != "satisfied" {
			report.Satisfied = false
		}
		report.Prerequisites = append(report.Prerequisites, status)
	}
	return report, nil
}
