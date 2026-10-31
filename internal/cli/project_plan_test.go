package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"autosql/pkg/config"
	"autosql/pkg/plan"
	"autosql/pkg/schema"
)

type projectPlanReader struct {
	DefaultReadPlan
	loads       []LoadRequest
	current     schema.Document
	loadErr     error
	remaining   time.Duration
	currentPlan schema.Document
	desiredPlan schema.Document
}

func (r *projectPlanReader) Load(ctx context.Context, request LoadRequest) (schema.Document, error) {
	r.loads = append(r.loads, request)
	if deadline, ok := ctx.Deadline(); ok {
		r.remaining = time.Until(deadline)
	}
	return r.current, r.loadErr
}

func (r *projectPlanReader) Plan(ctx context.Context, current, desired schema.Document, options plan.Options) (plan.Plan, error) {
	r.currentPlan, r.desiredPlan = current, desired
	return r.DefaultReadPlan.Plan(ctx, current, desired, options)
}

type projectPlanOutput struct {
	Data struct {
		Plan     plan.Plan               `json:"plan"`
		Identity *config.ProjectIdentity `json:"identity"`
		Inputs   []config.InputFile      `json:"source_inputs"`
	} `json:"data"`
}

func decodeProjectPlan(t *testing.T, code int, raw string) projectPlanOutput {
	t.Helper()
	var result projectPlanOutput
	if code != 0 {
		t.Fatalf("plan code=%d: %s", code, raw)
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestProjectPlanLocalInputsNeverInspectTarget(t *testing.T) {
	root := t.TempDir()
	path := projectCLIConfig(t, root, 2, "sql", "desired.sql")
	writeProjectCLIFile(t, root, "current.sql", "CREATE SCHEMA demo;")
	writeProjectCLIFile(t, root, "desired.sql", "CREATE SCHEMA demo; CREATE TABLE demo.accounts(id integer);")
	r := &projectPlanReader{loadErr: errors.New("must not inspect target")}
	a := &fakeApply{}
	args := []string{"plan", "--config", path, "--from", "sql:current.sql", "--postgres-version", "17", "--json"}
	code, raw, _ := invoke(t, args, "", false, Services{ReadPlan: r, Apply: a})
	result := decodeProjectPlan(t, code, raw)
	if len(r.loads) != 0 || a.calls != 0 || result.Data.Identity == nil || len(result.Data.Plan.Steps) == 0 {
		t.Fatalf("project plan used a target or omitted a plan: %s loads=%v", raw, r.loads)
	}
	var names []string
	for _, input := range result.Data.Inputs {
		names = append(names, input.Path)
	}
	if !reflect.DeepEqual(names, []string{"current.sql", "desired.sql"}) || strings.Contains(raw, root) {
		t.Fatalf("current or desired content missing: %s", raw)
	}
	if err := result.Data.Plan.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProjectPlanInspectsOnlySelectedTargetWithProjectTimeout(t *testing.T) {
	root := t.TempDir()
	path := projectCLIConfig(t, root, 2, "hcl", "desired.hcl")
	writeProjectCLIFile(t, root, "desired.hcl", `schema "demo" {}`)
	t.Setenv("AUTOSQL_TIMEOUT", "2m")
	t.Setenv("AUTOSQL_TARGET", "postgres://must-not-resolve")
	r := &projectPlanReader{current: schema.Document{Version: schema.SchemaVersion}}
	args := []string{"plan", "--config", path, "--env", "prod", "--json"}
	code, raw, _ := invoke(t, args, "", false, Services{ReadPlan: r})
	decodeProjectPlan(t, code, raw)
	if len(r.loads) != 1 || r.loads[0].Spec != "live:env://AUTOSQL_TARGET" || r.loads[0].BaseDir != root || r.remaining < time.Minute || r.remaining > 2*time.Minute {
		t.Fatalf("wrong target selection or timeout: loads=%+v deadline=%v", r.loads, r.remaining)
	}
	if strings.Contains(raw, "must-not-resolve") {
		t.Fatal("resolved a production credential while selecting source")
	}
	// Bad desired input must fail before even the read-only target inspection.
	if err := os.Remove(filepath.Join(root, "desired.hcl")); err != nil {
		t.Fatal(err)
	}
	r.loads = nil
	code, raw, _ = invoke(t, args, "", false, Services{ReadPlan: r})
	if code != int(ExitValidation) || len(r.loads) != 0 {
		t.Fatalf("inspection before local validation: %d %s", code, raw)
	}
}

func TestProjectPlanOverridesSourcesAndFiltersBothGraphs(t *testing.T) {
	root := t.TempDir()
	path := projectCLIConfig(t, root, 2, "sql", "missing-configured.sql")
	writeProjectCLIFile(t, root, "current.sql", "CREATE SCHEMA demo; CREATE SCHEMA ignored;")
	writeProjectCLIFile(t, root, "override.sql", "CREATE SCHEMA demo; CREATE SCHEMA ignored; CREATE TABLE demo.accounts(id integer); CREATE TABLE ignored.private(id integer);")
	t.Setenv("AUTOSQL_INCLUDE", "ignored.*")
	t.Setenv("AUTOSQL_EXCLUDE", "*")
	r := &projectPlanReader{}
	args := []string{"plan", "--config", path, "--from", "sql:current.sql", "--to", "sql:override.sql", "--include", "demo*", "--exclude", "not_selected*", "--json"}
	code, raw, _ := invoke(t, args, "", false, Services{ReadPlan: r})
	result := decodeProjectPlan(t, code, raw)
	if strings.Contains(raw, "ignored.private") || len(result.Data.Plan.Steps) == 0 || len(result.Data.Inputs) != 2 {
		t.Fatalf("sources or filters were appended: %s", raw)
	}
	from, _ := schema.SemanticFingerprint(r.currentPlan)
	to, _ := schema.SemanticFingerprint(r.desiredPlan)
	if result.Data.Plan.FromFingerprint != from || result.Data.Plan.ToFingerprint != to {
		t.Fatal("plan fingerprints did not describe the selected graphs")
	}
	for _, doc := range []schema.Document{r.currentPlan, r.desiredPlan} {
		for _, resource := range doc.Graph.Resources {
			if resource.Name.Name == "ignored" || resource.Name.Schema == "ignored" {
				t.Fatalf("unselected resource reached planner: %+v", resource.Name)
			}
		}
	}
}

func TestProjectPlanRefusalsAndLegacyCompatibility(t *testing.T) {
	root := t.TempDir()
	path := projectCLIConfig(t, root, 2, "sql", "desired.sql")
	writeProjectCLIFile(t, root, "desired.sql", "CREATE SCHEMA demo;")
	writeProjectCLIFile(t, root, "empty.sql", "")
	for _, extra := range [][]string{
		{"--schema", "demo"},
		{"--to", "provider:remote"},
		{"--from", "provider:remote"},
		{"--from", "live:postgres://user:seeded-source-secret@host/db"},
		{"--from", "sql:../outside.sql"},
		{"--from", "sql:empty.sql", "--max-changes", "0"},
	} {
		r := &projectPlanReader{loadErr: errors.New("unexpected live load")}
		args := append([]string{"plan", "--config", path, "--json"}, extra...)
		code, raw, _ := invoke(t, args, "", false, Services{ReadPlan: r})
		if code == 0 || len(r.loads) != 0 || strings.Contains(raw, "seeded-source-secret") {
			t.Fatalf("unsafe plan input: code=%d out=%s loads=%v", code, raw, r.loads)
		}
	}
	legacy := projectCLIConfig(t, root, 1, "sql", "desired.sql")
	code, raw, _ := invoke(t, []string{"plan", "--config", legacy, "--from", "sql:empty.sql", "--json"}, "", false, Services{})
	result := decodeProjectPlan(t, code, raw)
	if result.Data.Identity != nil || result.Data.Inputs != nil {
		t.Fatalf("legacy plan claimed project binding: %s", raw)
	}
}
