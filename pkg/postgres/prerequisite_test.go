package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

func TestGeneratedRoutinePrerequisiteManifestAndRenderPreflight(t *testing.T) {
	doc, _, function, _ := generatedDependencyFixture()
	prerequisites, err := GeneratedRoutinePrerequisites(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(prerequisites) != 1 || prerequisites[0].ResourceID != function.ID || prerequisites[0].Classification != "application_external" || prerequisites[0].Fingerprint == "" || len(prerequisites[0].RequiredBy) != 1 {
		t.Fatalf("prerequisites=%+v", prerequisites)
	}

	extensionDoc := cloneGeneratedDocument(t, doc)
	for index := range extensionDoc.Graph.Resources {
		resource := &extensionDoc.Graph.Resources[index]
		if resource.ID == function.ID {
			values := spec(*resource)
			values["extension"] = "cell_helpers"
			resource.Spec, _ = json.Marshal(values)
		}
	}
	extensionPrerequisites, err := GeneratedRoutinePrerequisites(context.Background(), extensionDoc)
	if err != nil {
		t.Fatal(err)
	}
	if len(extensionPrerequisites) != 1 || extensionPrerequisites[0].Classification != "extension_external" || extensionPrerequisites[0].Extension != "cell_helpers" {
		t.Fatalf("extension prerequisites=%+v", extensionPrerequisites)
	}

	empty := schema.Document{Version: schema.SchemaVersion}
	doc, err = New().Normalize(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := schema.Diff(empty, doc, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: empty, Desired: doc})
	if err == nil || len(out) != 0 || !strings.Contains(err.Error(), "reviewed_routine_digests") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestEmptyGeneratedRoutinePrerequisitesAreSatisfied(t *testing.T) {
	doc := schema.Document{Version: schema.SchemaVersion}
	report, err := VerifyGeneratedRoutinePrerequisites(context.Background(), "", doc)
	if err != nil || !report.Satisfied || len(report.Prerequisites) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}
