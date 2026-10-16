package cli

import (
	"autosql/pkg/plan"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/schema"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRead struct {
	from, to       schema.Document
	p              plan.Plan
	planErr        error
	loads          int
	planOptions    plan.Options
	planOptionsSet bool
}

func (f *fakeRead) Load(_ context.Context, r LoadRequest) (schema.Document, error) {
	f.loads++
	if r.Spec == "from" {
		return f.from, nil
	}
	return f.to, nil
}
func (f *fakeRead) Diff(_ context.Context, a, b schema.Document) (schema.ChangeSet, error) {
	return schema.Diff(a, b, schema.DiffOptions{})
}
func (f *fakeRead) Plan(_ context.Context, _, _ schema.Document, options plan.Options) (plan.Plan, error) {
	f.planOptions, f.planOptionsSet = options, true
	return f.p, f.planErr
}

type fakeApply struct {
	calls   int
	result  ApplyResult
	err     error
	request ApplyRequest
}

func (f *fakeApply) Apply(_ context.Context, r ApplyRequest) (ApplyResult, error) {
	f.calls++
	f.request = r
	return f.result, f.err
}
func workflowFixture(t *testing.T) (*fakeRead, plan.Plan) {
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	n := schema.Name{Name: "app"}
	to := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n, Spec: []byte(`{}`)}}}}
	p, e := plan.Build(context.Background(), sample.Driver{}, empty, to, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	return &fakeRead{from: empty, to: to, p: p}, p
}
func invoke(t *testing.T, args []string, in string, tty bool, s Services) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := RunWithServices(context.Background(), args, Streams{In: strings.NewReader(in), Out: &out, Err: &stderr, IsTTY: func() bool { return tty }}, s)
	return code, out.String(), stderr.String()
}
func TestReadPlanDryRunNeverReceiveMutationCapability(t *testing.T) {
	r, _ := workflowFixture(t)
	a := &fakeApply{}
	for _, args := range [][]string{{"schema", "diff", "--from", "from", "--to", "to", "--json"}, {"plan", "--from", "from", "--to", "to", "--json"}, {"apply", "--from", "from", "--to", "to", "--dry-run", "--json"}} {
		code, _, _ := invoke(t, args, "", false, Services{ReadPlan: r, Apply: a})
		if code != 0 {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}
	if a.calls != 0 {
		t.Fatalf("mutation called %d", a.calls)
	}
}

func TestPlanFailureSurfacesActionableReasonInHumanAndJSONOutput(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		r, _ := workflowFixture(t)
		r.planErr = errors.New(`unsupported schema transition: column app.widgets.state default rejected for normalized type "integer": function lower is not allowlisted password=hidden`)
		args := []string{"plan", "--from", "from", "--to", "to"}
		if jsonMode {
			args = append(args, "--json")
		}
		code, stdout, stderr := invoke(t, args, "", false, Services{ReadPlan: r})
		combined := stdout + stderr
		for _, want := range []string{"app.widgets.state", "normalized type", "function lower"} {
			if code != int(ExitMigration) || !strings.Contains(combined, want) {
				t.Fatalf("json=%v code=%d output=%s", jsonMode, code, combined)
			}
		}
		if strings.Contains(combined, "hidden") || !strings.Contains(combined, "password=[REDACTED]") {
			t.Fatalf("json=%v output was not redacted: %s", jsonMode, combined)
		}
	}
}
func TestPlanThreadsPostgresVersionToRenderOptions(t *testing.T) {
	r, _ := workflowFixture(t)
	code, _, stderr := invoke(t, []string{"plan", "--from", "from", "--to", "to", "--postgres-version", "17", "--json"}, "", false, Services{ReadPlan: r})
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !r.planOptionsSet {
		t.Fatal("Plan was never called")
	}
	if got := r.planOptions.Render["postgres_version"]; got != "17" {
		t.Fatalf("postgres_version=%q, want 17", got)
	}

	r2, _ := workflowFixture(t)
	code, _, stderr = invoke(t, []string{"plan", "--from", "from", "--to", "to", "--json"}, "", false, Services{ReadPlan: r2})
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !r2.planOptionsSet {
		t.Fatal("Plan was never called")
	}
	if _, ok := r2.planOptions.Render["postgres_version"]; ok {
		t.Fatalf("render=%v, want no postgres_version key when the flag is unset", r2.planOptions.Render)
	}
}

func TestPlanRejectsBadPostgresVersionBeforeLoad(t *testing.T) {
	for _, value := range []string{"abc", "13", "19"} {
		t.Run(value, func(t *testing.T) {
			r, _ := workflowFixture(t)
			code, _, stderr := invoke(t, []string{"plan", "--from", "from", "--to", "to", "--postgres-version", value, "--json"}, "", false, Services{ReadPlan: r})
			if code != int(ExitUsage) {
				t.Fatalf("value=%q code=%d stderr=%s, want ExitUsage", value, code, stderr)
			}
			if r.loads != 0 {
				t.Fatalf("value=%q loads=%d, want 0: a bad flag must refuse before any Load", value, r.loads)
			}
		})
	}
}

const inheritMembershipFromHCL = `schema "app" {}

role "app_reader" {
  login       = false
  superuser   = false
  create_role = false
  create_database = false
  inherit     = true
}

role "app_writer" {
  login       = false
  superuser   = false
  create_role = false
  create_database = false
  inherit     = true
}
`

const inheritMembershipToHCL = inheritMembershipFromHCL + `
resource "membership" "app_writer_to_reader" {
  spec_json = jsonencode({
    parent  = "app_reader"
    member  = "app_writer"
    admin   = false
    inherit = true
    set     = false
  })
  deps_json = jsonencode([
    references(resource_id("role", "", "", "app_reader")),
    references(resource_id("role", "", "", "app_writer")),
  ])
}
`

func writeHCLFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDefaultReadPlanRequiresPostgresVersionForInheritMembership exercises
// the real DefaultReadPlan end to end (HCL load, diff, and PostgreSQL
// render), not a fake. It reproduces stigen-flux-tc9: a new membership
// resource with inherit=true needs postgres_version>=16 to render. With no
// flag, or with --postgres-version 15, 'plan' must refuse with the same
// INHERIT error pkg/postgres/render.go's requirePostgresMajor raises.
// With --postgres-version 17, 'plan' must succeed with exactly one step.
func TestDefaultReadPlanRequiresPostgresVersionForInheritMembership(t *testing.T) {
	from := writeHCLFixture(t, "from.hcl", inheritMembershipFromHCL)
	to := writeHCLFixture(t, "to.hcl", inheritMembershipToHCL)
	for name, fixture := range map[string]struct {
		flags      []string
		wantCode   int
		wantSubstr string
	}{
		"unset":   {nil, int(ExitMigration), "INHERIT option requires postgres_version"},
		"too_old": {[]string{"--postgres-version", "15"}, int(ExitMigration), "INHERIT option requires postgres_version"},
		"ok":      {[]string{"--postgres-version", "17"}, int(ExitOK), `"status":"planned"`},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"plan", "--from", "hcl:" + from, "--to", "hcl:" + to, "--json"}, fixture.flags...)
			var out, stderr bytes.Buffer
			code := Run(context.Background(), args, Streams{Out: &out, Err: &stderr})
			combined := out.String() + stderr.String()
			if code != fixture.wantCode || !strings.Contains(combined, fixture.wantSubstr) || (fixture.wantCode == int(ExitMigration) && !strings.Contains(combined, "16")) {
				t.Fatalf("code=%d want=%d output=%s", code, fixture.wantCode, combined)
			}
			if fixture.wantCode == int(ExitOK) && !strings.Contains(combined, `"steps":1`) && !strings.Contains(out.String(), `"step_count\":1`) {
				var envelope struct {
					Data struct {
						Plan struct {
							Steps []any `json:"steps"`
						} `json:"plan"`
					} `json:"data"`
				}
				if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
					t.Fatalf("decode envelope: %v (%s)", err, out.String())
				}
				if len(envelope.Data.Plan.Steps) != 1 {
					t.Fatalf("steps=%d, want 1: %s", len(envelope.Data.Plan.Steps), out.String())
				}
			}
		})
	}
}

// TestPlanDigestIsUnaffectedByPostgresVersionWhenNoFeatureIsGated proves A4:
// for a diff that never reaches requirePostgresMajor, the computed plan
// digest, status and step count are identical whether or not
// --postgres-version is set. The flag only ever changes whether a
// version-gated render is refused; pkg/ itself does not change.
func TestPlanDigestIsUnaffectedByPostgresVersionWhenNoFeatureIsGated(t *testing.T) {
	from := writeHCLFixture(t, "from.hcl", `schema "app" {}
`)
	to := writeHCLFixture(t, "to.hcl", `schema "app" {}

table "widgets" {
  schema = "app"
  column "id" {
    type     = "bigint"
    nullable = false
    ordinal  = 1
  }
}
`)
	run := func(flags ...string) (int, map[string]any) {
		args := append([]string{"plan", "--from", "hcl:" + from, "--to", "hcl:" + to, "--json"}, flags...)
		var out, stderr bytes.Buffer
		code := Run(context.Background(), args, Streams{Out: &out, Err: &stderr})
		if code != int(ExitOK) {
			t.Fatalf("flags=%v code=%d stderr=%s", flags, code, stderr.String())
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatalf("decode envelope: %v (%s)", err, out.String())
		}
		return code, envelope.Data
	}
	_, withoutFlag := run()
	_, withFlag := run("--postgres-version", "17")
	planWithout, _ := withoutFlag["plan"].(map[string]any)
	planWith, _ := withFlag["plan"].(map[string]any)
	if planWithout["digest"] == "" || planWithout["digest"] != planWith["digest"] {
		t.Fatalf("digest without=%v with=%v, want equal and nonempty", planWithout["digest"], planWith["digest"])
	}
	if withoutFlag["status"] != withFlag["status"] {
		t.Fatalf("status without=%v with=%v, want equal", withoutFlag["status"], withFlag["status"])
	}
}

func TestUsageDocumentsPlanPostgresVersionFlag(t *testing.T) {
	if !strings.Contains(usage(), "plan --from source --to source [--postgres-version version]") {
		t.Fatalf("plan usage does not document --postgres-version: %s", usage())
	}
}

func TestApplyPromptsAndExplicitModes(t *testing.T) {
	for name, fixture := range map[string]struct {
		in              string
		tty             bool
		flags           []string
		wantCode, calls int
	}{"accept": {"DIGEST", true, nil, 0, 1}, "refuse": {"no", true, nil, 7, 0}, "eof": {"", true, nil, 7, 0}, "noninteractive": {"", false, nil, 7, 0}, "digest": {"", false, []string{"--approve-digest", "DIGEST"}, 0, 1}, "mismatch": {"", false, []string{"--approve-digest", "wrong"}, 7, 0}, "artifact": {"", false, []string{"--artifact", "signed.json"}, 0, 1}} {
		t.Run(name, func(t *testing.T) {
			r, p := workflowFixture(t)
			a := &fakeApply{result: ApplyResult{Status: "success"}}
			input := fixture.in
			if input == "DIGEST" {
				input = p.Digest + "\n"
			}
			args := []string{"apply", "--from", "from", "--to", "to", "--json"}
			args = append(args, fixture.flags...)
			for i := range args {
				if args[i] == "DIGEST" {
					args[i] = p.Digest
				}
			}
			code, out, _ := invoke(t, args, input, fixture.tty, Services{ReadPlan: r, Apply: a})
			if code != fixture.wantCode || a.calls != fixture.calls {
				t.Fatalf("code=%d calls=%d out=%s", code, a.calls, out)
			}
			if a.calls == 1 {
				wantMode := name
				if name == "accept" {
					wantMode = "interactive"
				}
				wantDigest := p.Digest
				if name == "artifact" {
					wantDigest = ""
				}
				if a.request.ApprovalMode != wantMode || a.request.AssertedDigest != wantDigest {
					t.Fatalf("request=%+v", a.request)
				}
			}
		})
	}
}
func TestApplyStatusesLimitsFailClosedAndRedaction(t *testing.T) {
	r, _ := workflowFixture(t)
	_, p := workflowFixture(t)
	code, out, _ := invoke(t, []string{"apply", "--from", "from", "--to", "to", "--approve-digest", p.Digest, "--json"}, "", false, Services{ReadPlan: r})
	if code != 7 || !strings.Contains(out, "refused") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	r, _ = workflowFixture(t)
	code, _, _ = invoke(t, []string{"plan", "--from", "from", "--to", "to", "--max-changes", "0"}, "", false, Services{ReadPlan: r})
	if code != int(ExitValidation) {
		t.Fatal(code)
	}
	r, _ = workflowFixture(t)
	a := &fakeApply{result: ApplyResult{Status: "partial_failure"}, err: errors.New("postgres://user:seeded-secret@db")}
	code, out, _ = invoke(t, []string{"apply", "--from", "from", "--to", "to", "--approve-digest", r.p.Digest, "--json"}, "", false, Services{ReadPlan: r, Apply: a})
	if code != 7 || strings.Contains(out, "seeded-secret") || !strings.Contains(out, "partial_failure") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	var envelope Envelope
	if e := json.Unmarshal([]byte(out), &envelope); e != nil {
		t.Fatal(e)
	}
	r, _ = workflowFixture(t)
	a = &fakeApply{err: errors.New("failed before mutation")}
	code, out, _ = invoke(t, []string{"apply", "--from", "from", "--to", "to", "--approve-digest", r.p.Digest, "--json"}, "", false, Services{ReadPlan: r, Apply: a})
	if code != int(ExitMigration) || strings.Contains(out, "partial_failure") {
		t.Fatalf("generic failure code=%d out=%s", code, out)
	}
}

func TestApplySanitizesSuccessAndRejectsConflictingModes(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		r, _ := workflowFixture(t)
		a := &fakeApply{result: ApplyResult{Status: "success", Message: "postgres://user:hunter2@db/app password=also-secret"}}
		args := []string{"apply", "--from", "from", "--to", "to", "--approve-digest", r.p.Digest}
		if jsonMode {
			args = append(args, "--json")
		}
		code, out, _ := invoke(t, args, "", false, Services{ReadPlan: r, Apply: a})
		if code != 0 || strings.Contains(out, "hunter2") || strings.Contains(out, "also-secret") {
			t.Fatalf("json=%v code=%d out=%s", jsonMode, code, out)
		}
	}
	r, _ := workflowFixture(t)
	a := &fakeApply{}
	code, _, _ := invoke(t, []string{"apply", "--from", "from", "--to", "to", "--dry-run", "--approve-digest", r.p.Digest}, "", false, Services{ReadPlan: r, Apply: a})
	if code != int(ExitUsage) || a.calls != 0 {
		t.Fatalf("code=%d calls=%d", code, a.calls)
	}
}

func TestApplyUncertainRecoveryFieldsSurviveFailureEnvelope(t *testing.T) {
	r, _ := workflowFixture(t)
	a := &fakeApply{result: ApplyResult{Status: "uncertain", PendingStep: "step-2", ExecutionID: "exec-1", RecoveryGuidance: "reconcile before retry"}, err: errors.New("unknown outcome")}
	code, out, _ := invoke(t, []string{"apply", "--from", "from", "--to", "to", "--approve-digest", r.p.Digest, "--json"}, "", false, Services{ReadPlan: r, Apply: a})
	for _, want := range []string{`"status":"uncertain"`, `"pending_step":"step-2"`, `"execution_id":"exec-1"`, `"recovery_guidance":"reconcile before retry"`} {
		if code != int(ExitMigration) || !strings.Contains(out, want) {
			t.Fatalf("code=%d out=%s", code, out)
		}
	}
}
func TestNoOpMaxLimitAndArtifactOnly(t *testing.T) {
	r, p := workflowFixture(t)
	p.Changes.Changes = append(p.Changes.Changes, p.Changes.Changes[0])
	r.p = p
	code, _, _ := invoke(t, []string{"plan", "--from", "from", "--to", "to", "--max-changes", "1"}, "", false, Services{ReadPlan: r})
	if code != 5 {
		t.Fatalf("max code=%d", code)
	}
	empty := r.from
	noop, e := plan.Build(context.Background(), sample.Driver{}, empty, empty, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	r.p = noop
	code, out, _ := invoke(t, []string{"apply", "--from", "from", "--to", "to", "--json"}, "", false, Services{ReadPlan: r})
	if code != 0 || !strings.Contains(out, "no_op") {
		t.Fatalf("noop code=%d out=%s", code, out)
	}
	r, _ = workflowFixture(t)
	a := &fakeApply{result: ApplyResult{Status: "success"}}
	code, _, _ = invoke(t, []string{"apply", "--artifact", "signed.json"}, "", false, Services{ReadPlan: r, Apply: a})
	if code != 0 || a.calls != 1 || r.loads != 0 {
		t.Fatalf("artifact code=%d calls=%d loads=%d", code, a.calls, r.loads)
	}
	a = &fakeApply{}
	code, _, _ = invoke(t, []string{"apply", "--artifact", "signed.json", "--dry-run"}, "", false, Services{ReadPlan: r, Apply: a})
	if code != int(ExitUsage) || a.calls != 0 {
		t.Fatalf("artifact dry-run code=%d calls=%d", code, a.calls)
	}
}
