package cli

import (
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// membershipVersionGateFixture builds a before/after document pair that
// mirrors schema/autosql/cell.hcl's infra_blocks_runtime->app@postgres
// membership: the "after" document adds that membership (which needs
// postgres_version >= 16 to render its INHERIT option, pkg/postgres/render.go
// requirePostgresMajor) plus an unrelated role (a version-independent
// change), so a real plan over the delta exercises both the version gate and
// an ordinary render path in the same plan.
func membershipVersionGateFixture() (before, after schema.Document) {
	app := schema.Resource{
		ID:   schema.StableID(schema.KindRole, schema.Name{Name: "app"}),
		Kind: schema.KindRole,
		Name: schema.Name{Name: "app"},
		Spec: json.RawMessage(`{"superuser":false,"inherit":true,"create_role":false,"create_database":false,"login":false,"replication":false,"bypass_rls":false,"connection_limit":-1}`),
	}
	runtime := schema.Resource{
		ID:   schema.StableID(schema.KindRole, schema.Name{Name: "infra_blocks_runtime"}),
		Kind: schema.KindRole,
		Name: schema.Name{Name: "infra_blocks_runtime"},
		Spec: json.RawMessage(`{"superuser":false,"inherit":true,"create_role":false,"create_database":false,"login":true,"replication":false,"bypass_rls":false,"connection_limit":200}`),
	}
	before = schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{app, runtime}}, Annotations: map[string]string{"dialect": "postgresql"}}

	viewer := schema.Resource{
		ID:   schema.StableID(schema.KindRole, schema.Name{Name: "infra_blocks_viewer"}),
		Kind: schema.KindRole,
		Name: schema.Name{Name: "infra_blocks_viewer"},
		Spec: json.RawMessage(`{"superuser":false,"inherit":true,"create_role":false,"create_database":false,"login":true,"replication":false,"bypass_rls":false,"connection_limit":5}`),
	}
	membership := schema.Resource{
		ID:   schema.StableID(schema.KindMembership, schema.Name{Name: "infra_blocks_runtime->app@postgres"}),
		Kind: schema.KindMembership,
		Name: schema.Name{Name: "infra_blocks_runtime->app@postgres"},
		Spec: json.RawMessage(`{"admin":false,"grantor":"postgres","inherit":true,"member":"infra_blocks_runtime","parent":"app","set":false}`),
		Dependencies: []schema.Dependency{
			{Target: app.ID, Type: schema.DependencyReferences},
			{Target: runtime.ID, Type: schema.DependencyReferences},
		},
	}
	after = schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{app, runtime, viewer, membership}}, Annotations: map[string]string{"dialect": "postgresql"}}
	return before, after
}

// membershipReadPlan is a ReadPlanService backed by the real postgres driver
// (not the sample driver used by other workflow tests), so Plan actually
// exercises pkg/postgres's membership INHERIT version gate.
type membershipReadPlan struct{ before, after schema.Document }

func (m membershipReadPlan) Load(_ context.Context, r LoadRequest) (schema.Document, error) {
	if r.Spec == "before" {
		return m.before, nil
	}
	return m.after, nil
}
func (m membershipReadPlan) Diff(_ context.Context, a, b schema.Document) (schema.ChangeSet, error) {
	return schema.Diff(a, b, schema.DiffOptions{})
}
func (m membershipReadPlan) Plan(ctx context.Context, a, b schema.Document, options plan.Options) (plan.Plan, error) {
	return plan.Build(ctx, postgres.New(), a, b, options)
}

func TestPlanRendersMembershipInheritOnlyWithPostgresVersionFlag(t *testing.T) {
	before, after := membershipVersionGateFixture()
	r := membershipReadPlan{before: before, after: after}

	// AC1 (today's bug): no --postgres-version renders the membership
	// INHERIT option fails exactly like the reported CI/local repro. Use the
	// non-JSON output here so the error text isn't JSON-escaped.
	code, out, stderr := invoke(t, []string{"plan", "--from", "before", "--to", "after"}, "", false, Services{ReadPlan: r})
	if code != int(ExitMigration) {
		t.Fatalf("expected ExitMigration without --postgres-version, got code=%d out=%s err=%s", code, out, stderr)
	}
	combined := out + stderr
	for _, want := range []string{"membership INHERIT option", "postgres_version >= 16"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("expected %q in output, got: %s", want, combined)
		}
	}

	// AC1: --postgres-version 17 (prod cell runs PostgreSQL 17.2) plans ok,
	// including the unrelated role-create change in the same delta.
	code, out, stderr = invoke(t, []string{"plan", "--from", "before", "--to", "after", "--postgres-version", "17", "--json"}, "", false, Services{ReadPlan: r})
	if code != 0 {
		t.Fatalf("expected ok with --postgres-version 17, got code=%d out=%s err=%s", code, out, stderr)
	}
	if !strings.Contains(out, `"status":"planned"`) {
		t.Fatalf("expected status planned, out=%s", out)
	}

	// apply --dry-run takes the same loadPlanInputs path; prove it also
	// threads the flag through.
	code, out, stderr = invoke(t, []string{"apply", "--from", "before", "--to", "after", "--postgres-version", "17", "--dry-run", "--json"}, "", false, Services{ReadPlan: r})
	if code != 0 || !strings.Contains(out, `"dry_run":true`) {
		t.Fatalf("apply dry-run code=%d out=%s err=%s", code, out, stderr)
	}
}

func TestPlanAndApplyRejectPostgresVersionOutsideSupportedRange(t *testing.T) {
	for _, bad := range []string{"13", "19", "abc"} {
		r, _ := workflowFixture(t)
		code, out, stderr := invoke(t, []string{"plan", "--from", "from", "--to", "to", "--postgres-version", bad, "--json"}, "", false, Services{ReadPlan: r})
		if code != int(ExitUsage) {
			t.Fatalf("bad=%q expected ExitUsage got code=%d out=%s err=%s", bad, code, out, stderr)
		}
		if r.planCalls != 0 || r.loads != 0 {
			t.Fatalf("bad=%q flag validation must precede loading any source, planCalls=%d loads=%d", bad, r.planCalls, r.loads)
		}

		r2, _ := workflowFixture(t)
		a := &fakeApply{}
		code, out, stderr = invoke(t, []string{"apply", "--from", "from", "--to", "to", "--postgres-version", bad, "--dry-run", "--json"}, "", false, Services{ReadPlan: r2, Apply: a})
		if code != int(ExitUsage) || a.calls != 0 {
			t.Fatalf("bad=%q apply expected ExitUsage/no calls, got code=%d calls=%d out=%s err=%s", bad, code, a.calls, out, stderr)
		}
	}
}

func TestPlanWithoutPostgresVersionFlagLeavesRenderOptionsUnset(t *testing.T) {
	// AC2: omitting the flag must not change behavior for callers (including
	// CI's existing gate invocations) that never pass it: the render options
	// handed to the service stay the zero value, exactly as before this flag
	// existed.
	r, _ := workflowFixture(t)
	code, _, stderr := invoke(t, []string{"plan", "--from", "from", "--to", "to", "--json"}, "", false, Services{ReadPlan: r})
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, stderr)
	}
	if r.planCalls != 1 {
		t.Fatalf("planCalls=%d", r.planCalls)
	}
	if r.lastOptions.Render != nil {
		t.Fatalf("expected nil Render options, got %#v", r.lastOptions.Render)
	}

	r, _ = workflowFixture(t)
	code, _, stderr = invoke(t, []string{"plan", "--from", "from", "--to", "to", "--postgres-version", "17", "--json"}, "", false, Services{ReadPlan: r})
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, stderr)
	}
	if r.lastOptions.Render["postgres_version"] != "17" {
		t.Fatalf("expected postgres_version=17 render option, got %#v", r.lastOptions.Render)
	}
}
