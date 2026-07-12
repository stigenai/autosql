package cli

import (
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/source"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type LoadRequest struct {
	Spec                      string
	Schemas, Include, Exclude []string
}
type ReadPlanService interface {
	Load(context.Context, LoadRequest) (schema.Document, error)
	Diff(context.Context, schema.Document, schema.Document) (schema.ChangeSet, error)
	Plan(context.Context, schema.Document, schema.Document) (plan.Plan, error)
}
type ApplyRequest struct {
	Plan           plan.Plan
	ArtifactPath   string
	AssertedDigest string
	ApprovalMode   string
}

type optionalInt struct {
	value int
	set   bool
}

func (v *optionalInt) String() string { return fmt.Sprint(v.value) }
func (v *optionalInt) Set(raw string) error {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return errors.New("must be a nonnegative integer")
	}
	v.value, v.set = n, true
	return nil
}

func hasSelectors(r LoadRequest) bool { return len(r.Schemas)+len(r.Include)+len(r.Exclude) > 0 }
func validateSelectors(from, to string, r LoadRequest) error {
	if hasSelectors(r) && (!strings.HasPrefix(from, "live:") || !strings.HasPrefix(to, "live:")) {
		return usageError(errors.New("--schema, --include, and --exclude are supported only when both sources are live"))
	}
	return nil
}

type ApplyResult struct {
	Status           string `json:"status"`
	AppliedSteps     int    `json:"applied_steps,omitempty"`
	Message          string `json:"message,omitempty"`
	PendingStep      string `json:"pending_step,omitempty"`
	ExecutionID      string `json:"execution_id,omitempty"`
	RecoveryGuidance string `json:"recovery_guidance,omitempty"`
}
type ApplyService interface {
	Apply(context.Context, ApplyRequest) (ApplyResult, error)
}
type Services struct {
	ReadPlan ReadPlanService
	Apply    ApplyService
}
type Provider interface {
	Load(context.Context, string) (schema.Document, error)
}
type DefaultReadPlan struct {
	Redactor  *secret.Redactor
	Providers map[string]Provider
}

func (d DefaultReadPlan) Load(ctx context.Context, r LoadRequest) (schema.Document, error) {
	kind, value, ok := strings.Cut(r.Spec, ":")
	if !ok || value == "" {
		return schema.Document{}, fmt.Errorf("invalid source")
	}
	var doc schema.Document
	var e error
	switch kind {
	case "sql", "native", "json":
		data, readErr := os.ReadFile(value)
		if readErr != nil {
			return doc, fmt.Errorf("read source")
		}
		format := source.FormatNative
		if kind == "sql" {
			format = source.FormatSQL
		}
		doc, e = source.LoadContext(ctx, source.Input{URI: value, Format: format, Data: data})
	case "live":
		ref := secret.Reference(value)
		if e = ref.Validate(); e != nil {
			return doc, fmt.Errorf("invalid live reference")
		}
		resolver := secret.NewResolver()
		resolver.Redactor = d.Redactor
		var url string
		url, e = resolver.Resolve(ctx, ref)
		if e == nil {
			doc, e = postgres.InspectURL(ctx, url, postgres.Options{Schemas: r.Schemas, Include: r.Include, Exclude: r.Exclude})
		}
	case "provider":
		name, ref, found := strings.Cut(value, ":")
		p := d.Providers[name]
		if !found || p == nil {
			return doc, fmt.Errorf("unknown provider")
		}
		doc, e = p.Load(ctx, ref)
	default:
		return doc, fmt.Errorf("unsupported source")
	}
	if e != nil {
		return schema.Document{}, e
	}
	return postgres.New().Normalize(ctx, doc)
}
func (DefaultReadPlan) Diff(_ context.Context, a, b schema.Document) (schema.ChangeSet, error) {
	return schema.Diff(a, b, schema.DiffOptions{})
}
func (DefaultReadPlan) Plan(ctx context.Context, a, b schema.Document) (plan.Plan, error) {
	return plan.Build(ctx, postgres.New(), a, b, plan.Options{})
}
func runSchemaDiff(ctx context.Context, args []string, o output, r ReadPlanService) error {
	fs := newFlags("schema diff", o.streams.Err)
	from := fs.String("from", "", "source spec")
	to := fs.String("to", "", "source spec")
	var max optionalInt
	fs.Var(&max, "max-changes", "maximum changes")
	jsonFlag := fs.Bool("json", false, "JSON")
	var schemas, include, exclude stringList
	fs.Var(&schemas, "schema", "schema")
	fs.Var(&include, "include", "include")
	fs.Var(&exclude, "exclude", "exclude")
	if e := fs.Parse(args); e != nil {
		return usageError(e)
	}
	if fs.NArg() != 0 {
		return usageError(errors.New("unexpected positional arguments"))
	}
	if *from == "" || *to == "" {
		return usageError(errors.New("--from and --to required"))
	}
	filter := LoadRequest{Schemas: schemas.value(), Include: include.value(), Exclude: exclude.value()}
	if e := validateSelectors(*from, *to, filter); e != nil {
		return e
	}
	o.json = *jsonFlag
	filter.Spec = *from
	a, e := r.Load(ctx, filter)
	if e != nil {
		return &Error{Kind: "validation", Message: "load source failed", Code: ExitValidation, Cause: e}
	}
	filter.Spec = *to
	b, e := r.Load(ctx, filter)
	if e != nil {
		return &Error{Kind: "validation", Message: "load target failed", Code: ExitValidation, Cause: e}
	}
	changes, e := r.Diff(ctx, a, b)
	if e != nil {
		return &Error{Kind: "migration", Message: "diff failed", Code: ExitMigration, Cause: e}
	}
	if max.set && len(changes.Changes) > max.value {
		return &Error{Kind: "validation", Message: "maximum change count exceeded", Code: ExitValidation, Cause: e}
	}
	status := "success"
	if len(changes.Changes) == 0 {
		status = "no_op"
	}
	data := map[string]any{"status": status, "changes": changes}
	raw, _ := changes.MarshalCanonical()
	return o.success(data, string(raw))
}
func loadPlanInputs(ctx context.Context, from, to string, filter LoadRequest, r ReadPlanService) (schema.Document, schema.Document, plan.Plan, error) {
	filter.Spec = from
	a, e := r.Load(ctx, filter)
	if e != nil {
		return a, schema.Document{}, plan.Plan{}, e
	}
	filter.Spec = to
	b, e := r.Load(ctx, filter)
	if e != nil {
		return a, b, plan.Plan{}, e
	}
	p, e := r.Plan(ctx, a, b)
	return a, b, p, e
}
func runPlan(ctx context.Context, args []string, o output, r ReadPlanService) error {
	fs := newFlags("plan", o.streams.Err)
	from := fs.String("from", "", "source")
	to := fs.String("to", "", "target")
	var max optionalInt
	fs.Var(&max, "max-changes", "maximum changes")
	jsonFlag := fs.Bool("json", false, "JSON")
	var schemas, include, exclude stringList
	fs.Var(&schemas, "schema", "schema")
	fs.Var(&include, "include", "include")
	fs.Var(&exclude, "exclude", "exclude")
	if e := fs.Parse(args); e != nil {
		return usageError(e)
	}
	if fs.NArg() != 0 {
		return usageError(errors.New("unexpected positional arguments"))
	}
	if *from == "" || *to == "" {
		return usageError(errors.New("--from and --to required"))
	}
	o.json = *jsonFlag
	filter := LoadRequest{Schemas: schemas.value(), Include: include.value(), Exclude: exclude.value()}
	if e := validateSelectors(*from, *to, filter); e != nil {
		return e
	}
	_, _, p, e := loadPlanInputs(ctx, *from, *to, filter, r)
	if e != nil {
		return &Error{Kind: "migration", Message: "planning failed", Code: ExitMigration, Cause: e}
	}
	if max.set && len(p.Changes.Changes) > max.value {
		return &Error{Kind: "validation", Message: "maximum change count exceeded", Code: ExitValidation}
	}
	status := "planned"
	if len(p.Changes.Changes) == 0 {
		status = "no_op"
	}
	b, _ := p.MarshalCanonical()
	return o.success(map[string]any{"status": status, "plan": p}, string(b))
}
func runApply(ctx context.Context, args []string, o output, s Services, tty bool) error {
	fs := newFlags("apply", o.streams.Err)
	from := fs.String("from", "", "source")
	to := fs.String("to", "", "target")
	artifact := fs.String("artifact", "", "signed artifact")
	dry := fs.Bool("dry-run", false, "plan only")
	approveDigest := fs.String("approve-digest", "", "assert the exact computed plan digest")
	var max optionalInt
	fs.Var(&max, "max-changes", "maximum changes")
	jsonFlag := fs.Bool("json", false, "JSON")
	var schemas, include, exclude stringList
	fs.Var(&schemas, "schema", "schema")
	fs.Var(&include, "include", "include")
	fs.Var(&exclude, "exclude", "exclude")
	if e := fs.Parse(args); e != nil {
		return usageError(e)
	}
	if fs.NArg() != 0 {
		return usageError(errors.New("unexpected positional arguments"))
	}
	o.json = *jsonFlag
	modeCount := 0
	if *dry {
		modeCount++
	}
	if *approveDigest != "" {
		modeCount++
	}
	if *artifact != "" {
		modeCount++
	}
	if modeCount > 1 {
		return usageError(errors.New("--dry-run, --approve-digest, and --artifact are mutually exclusive"))
	}
	if hasSelectors(LoadRequest{Schemas: schemas.value(), Include: include.value(), Exclude: exclude.value()}) && (*from == "" || *to == "") {
		return usageError(errors.New("selectors require --from and --to live sources"))
	}
	if *artifact != "" {
		if s.Apply == nil {
			return &Error{Kind: "migration", Message: "verified artifact apply service is not wired", Code: ExitMigration, Status: "refused"}
		}
		result, err := s.Apply.Apply(ctx, ApplyRequest{ArtifactPath: *artifact, ApprovalMode: "artifact"})
		if err != nil {
			return applyFailure(result, err)
		}
		if result.Status == "" {
			result.Status = "success"
		}
		return o.success(result, result.Status)
	}
	if *from == "" || *to == "" {
		return usageError(errors.New("--from and --to required unless --artifact is used"))
	}
	filter := LoadRequest{Schemas: schemas.value(), Include: include.value(), Exclude: exclude.value()}
	if e := validateSelectors(*from, *to, filter); e != nil {
		return e
	}
	_, _, p, e := loadPlanInputs(ctx, *from, *to, filter, s.ReadPlan)
	if e != nil {
		return &Error{Kind: "migration", Message: "planning failed", Code: ExitMigration, Cause: e}
	}
	if max.set && len(p.Changes.Changes) > max.value {
		return &Error{Kind: "validation", Message: "maximum change count exceeded", Code: ExitValidation}
	}
	if len(p.Changes.Changes) == 0 {
		return o.success(ApplyResult{Status: "no_op"}, "no changes")
	}
	if *dry {
		b, _ := p.MarshalCanonical()
		return o.success(map[string]any{"status": "dry_run", "dry_run": true, "plan": p}, string(b))
	}
	approved := *approveDigest != "" || *artifact != ""
	approvalMode := "interactive"
	if *approveDigest != "" {
		approvalMode = "digest"
	} else if *artifact != "" {
		approvalMode = "artifact"
	}
	if *approveDigest != "" && *approveDigest != p.Digest {
		return &Error{Kind: "migration", Message: "approved digest does not match computed plan", Code: ExitMigration, Status: "refused"}
	}
	if !approved {
		if !tty {
			return &Error{Kind: "migration", Message: "noninteractive apply requires --approve-digest or --artifact", Code: ExitMigration, Status: "refused"}
		}
		fmt.Fprintf(o.streams.Err, "type plan digest %s to apply: ", p.Digest)
		line, readErr := bufio.NewReader(o.streams.In).ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return &Error{Kind: "migration", Message: "approval prompt failed", Code: ExitMigration, Status: "refused"}
		}
		if strings.TrimSpace(line) != p.Digest {
			return &Error{Kind: "migration", Message: "approval refused", Code: ExitMigration, Status: "refused"}
		}
		approved = true
	}
	if s.Apply == nil {
		return &Error{Kind: "migration", Message: "apply service is not wired", Code: ExitMigration, Status: "refused"}
	}
	asserted := *approveDigest
	if approvalMode == "interactive" {
		asserted = p.Digest
	}
	result, e := s.Apply.Apply(ctx, ApplyRequest{Plan: p, ArtifactPath: *artifact, AssertedDigest: asserted, ApprovalMode: approvalMode})
	if e != nil {
		return applyFailure(result, e)
	}
	if result.Status == "" {
		result.Status = "success"
	}
	return o.success(result, result.Status)
}

func applyFailure(result ApplyResult, cause error) error {
	status := ""
	if result.Status == "partial_failure" || result.Status == "uncertain" {
		status = result.Status
	}
	return &Error{Kind: "migration", Message: "apply failed", Code: ExitMigration, Status: status, Cause: cause, PendingStep: result.PendingStep, ExecutionID: result.ExecutionID, RecoveryGuidance: result.RecoveryGuidance}
}
func schemaSQL(doc schema.Document) (string, error) {
	statements, e := postgres.RenderDocument(context.Background(), doc, nil)
	if e != nil {
		return "", e
	}
	var b strings.Builder
	for _, s := range statements {
		b.WriteString(s.SQL)
		if !strings.HasSuffix(s.SQL, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}
