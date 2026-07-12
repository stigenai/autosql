package cli

import (
	"autosql/pkg/plan"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/schema"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeRead struct {
	from, to schema.Document
	p        plan.Plan
	loads    int
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
func (f *fakeRead) Plan(context.Context, schema.Document, schema.Document) (plan.Plan, error) {
	return f.p, nil
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
func TestApplyPromptsAndExplicitModes(t *testing.T) {
	for name, fixture := range map[string]struct {
		in              string
		tty             bool
		flags           []string
		wantCode, calls int
	}{"accept": {"DIGEST", true, nil, 0, 1}, "refuse": {"no", true, nil, 7, 0}, "eof": {"", true, nil, 7, 0}, "noninteractive": {"", false, nil, 7, 0}, "digest": {"", false, []string{"--approve-digest", "DIGEST"}, 0, 1}, "mismatch": {"", false, []string{"--approve-digest", "wrong"}, 7, 0}, "artifact": {"", false, []string{"--artifact", "signed.json"}, 7, 0}} {
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
				if a.request.ApprovalMode != wantMode || a.request.AssertedDigest != p.Digest {
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
	if code != int(ExitMigration) || a.calls != 0 || r.loads != 0 {
		t.Fatalf("artifact code=%d calls=%d loads=%d", code, a.calls, r.loads)
	}
	a = &fakeApply{}
	code, _, _ = invoke(t, []string{"apply", "--artifact", "signed.json", "--dry-run"}, "", false, Services{ReadPlan: r, Apply: a})
	if code != int(ExitUsage) || a.calls != 0 {
		t.Fatalf("artifact dry-run code=%d calls=%d", code, a.calls)
	}
}
