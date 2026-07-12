// Package guardrail composes migration safety analysis, declarative policy,
// approval/audit, and live prechecks into one fail-closed apply boundary.
package guardrail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"reflect"
	"sort"
	"strings"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/policy"
	"autosql/pkg/precheck"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
)

var (
	ErrBinding  = errors.New("guardrail binding failed")
	ErrSafety   = errors.New("guardrail safety gate failed")
	ErrPolicy   = errors.New("guardrail policy gate failed")
	ErrApproval = errors.New("guardrail approval gate failed")
	ErrPrecheck = errors.New("guardrail live precheck failed")
	ErrConfig   = errors.New("guardrail configuration invalid")
)

// BindingError identifies the failed binding without retaining caller-supplied
// values, which may be malformed or secret-bearing.
type BindingError struct{ Field string }

func (e *BindingError) Error() string {
	return fmt.Sprintf("%v: %s mismatch", ErrBinding, e.Field)
}
func (e *BindingError) Unwrap() error { return ErrBinding }

type SafetyError struct {
	Minimum     safety.Severity
	Diagnostics []safety.Diagnostic
}

func (e *SafetyError) Error() string {
	return fmt.Sprintf("%v: %d unsuppressed diagnostics at or above %s", ErrSafety, len(e.Diagnostics), e.Minimum)
}
func (e *SafetyError) Unwrap() error { return ErrSafety }

type PolicyError struct{ Count int }

func (e *PolicyError) Error() string {
	return fmt.Sprintf("%v: %d violations", ErrPolicy, e.Count)
}
func (e *PolicyError) Unwrap() error { return ErrPolicy }

type StageError struct {
	Kind     error
	Stage    string
	canceled bool
	deadline bool
}

func (e *StageError) Error() string { return fmt.Sprintf("%v during %s", e.Kind, e.Stage) }
func (e *StageError) Is(target error) bool {
	return target == e.Kind || e.canceled && target == context.Canceled || e.deadline && target == context.DeadlineExceeded
}
func stageError(kind error, stage string, cause error) *StageError {
	return &StageError{Kind: kind, Stage: stage, canceled: errors.Is(cause, context.Canceled), deadline: errors.Is(cause, context.DeadlineExceeded)}
}

// RiskConfig derives approval risk from trusted configuration and unsuppressed
// diagnostics. Missing severity entries use the conservative defaults.
type RiskConfig struct {
	Baseline   approval.Risk
	BySeverity map[safety.Severity]approval.Risk
}

type Config struct {
	Environment string
	FailOn      safety.Severity
	Risk        RiskConfig
}

type Guardrail struct {
	Config   Config
	Safety   safety.Runner
	Policy   policy.Evaluator
	Approval approval.Gate
}

type Input struct {
	Changes            schema.ChangeSet
	Safety             safety.Input
	Policy             policy.Document
	PolicyIdentity     string
	SchemaResources    []policy.Resource
	MigrationResources []policy.Resource
	Precheck           precheck.Plan
	Approval           approval.Request
	Database           precheck.DB
	StatementBindings  []StatementBinding
	// Mutation is invoked only inside the approval gate's authorized callback.
	// When set it owns phase-aware prechecks, mutation, and durable history.
	Mutation AuthorizedMutation
}

type AuthorizedMutation interface {
	ApplyAuthorized(context.Context, precheck.Plan) ([]precheck.Result, error)
}

// StatementBinding attributes one exact SQL command to one exact change.
type StatementBinding struct{ SQL, ChangeID, ChangeHash string }

type Result struct {
	ChangeDigest string
	BundleDigest string
	Risk         approval.Risk
	Diagnostics  []safety.Diagnostic
	Violations   []policy.Violation
	Checks       []precheck.Result
}

// ChangeDigest hashes the canonical, versioned ChangeSet representation.
func ChangeDigest(changes schema.ChangeSet) (string, error) {
	canonical, err := changes.MarshalCanonical()
	if err != nil {
		return "", fmt.Errorf("%w: canonical changes invalid", ErrBinding)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type enforcementBundle struct {
	Version            string               `json:"version"`
	ChangeDigest       string               `json:"change_digest"`
	PrecheckDigest     string               `json:"precheck_digest"`
	Environment        string               `json:"environment"`
	Author             string               `json:"author"`
	Requester          string               `json:"requester"`
	PlanExpiry         canonicalExpiry      `json:"plan_expiry"`
	Override           canonicalOverride    `json:"emergency_override"`
	PolicyIdentity     string               `json:"policy_identity"`
	Policy             policy.Document      `json:"policy"`
	PolicyLimits       policy.Limits        `json:"policy_limits"`
	SchemaResources    []policy.Resource    `json:"schema_resources"`
	MigrationResources []policy.Resource    `json:"migration_resources"`
	FailOn             safety.Severity      `json:"fail_on"`
	Risk               RiskConfig           `json:"risk"`
	Target             safety.Target        `json:"target"`
	Thresholds         safety.Thresholds    `json:"thresholds"`
	Analyzers          []analyzerIdentity   `json:"analyzers"`
	Suppressions       []safety.Suppression `json:"suppressions"`
	ApprovalPolicy     approval.Policy      `json:"approval_policy"`
	Statements         []StatementBinding   `json:"statements"`
}
type canonicalExpiry struct {
	Set bool   `json:"set"`
	UTC string `json:"utc"`
}
type canonicalOverride struct {
	Set      bool   `json:"set"`
	Identity string `json:"identity"`
	Reason   string `json:"reason"`
}
type analyzerIdentity struct{ Name, Concrete, Version, ConfigDigest string }

// BundleDigest returns the one digest that approval.Plan.Digest must equal.
func (g Guardrail) BundleDigest(in Input) (string, error) {
	if err := validateProduction(g, in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.PolicyIdentity) == "" {
		return "", fmt.Errorf("%w: policy identity is required", ErrConfig)
	}
	if strings.TrimSpace(in.Approval.Plan.Author) == "" || strings.TrimSpace(in.Approval.RequestedBy) == "" {
		return "", fmt.Errorf("%w: author and requester are required", ErrConfig)
	}
	names, err := analyzerIdentities(g.Safety.Analyzers)
	if err != nil {
		return "", err
	}
	changeDigest, err := ChangeDigest(in.Changes)
	if err != nil {
		return "", err
	}
	bundle := enforcementBundle{Version: "autosql.guardrail.bundle/v1", ChangeDigest: changeDigest, PrecheckDigest: in.Precheck.Digest, Environment: g.Config.Environment, Author: in.Approval.Plan.Author, Requester: in.Approval.RequestedBy, PlanExpiry: canonicalPlanExpiry(in.Approval.Plan.ExpiresAt), Override: canonicalEmergencyOverride(in.Approval.Override), PolicyIdentity: in.PolicyIdentity, Policy: in.Policy, PolicyLimits: g.Policy.Limits, SchemaResources: in.SchemaResources, MigrationResources: in.MigrationResources, FailOn: g.Config.FailOn, Risk: g.Config.Risk, Target: in.Safety.Target, Thresholds: in.Safety.Thresholds, Analyzers: names, Suppressions: g.Safety.Suppressions, ApprovalPolicy: g.Approval.Policy, Statements: in.StatementBindings}
	raw, err := json.Marshal(bundle)
	if err != nil {
		return "", fmt.Errorf("%w: bundle is not canonical JSON", ErrBinding)
	}
	h := sha256.New()
	writeField(h, "autosql.guardrail.bundle-digest/v1")
	writeField(h, string(raw))
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
func canonicalPlanExpiry(value time.Time) canonicalExpiry {
	if value.IsZero() {
		return canonicalExpiry{}
	}
	return canonicalExpiry{Set: true, UTC: value.UTC().Format(time.RFC3339Nano)}
}
func canonicalEmergencyOverride(value *approval.EmergencyOverride) canonicalOverride {
	if value == nil {
		return canonicalOverride{}
	}
	return canonicalOverride{Set: true, Identity: value.Identity, Reason: value.Reason}
}

func analyzerIdentities(analyzers []safety.Analyzer) ([]analyzerIdentity, error) {
	if len(analyzers) == 0 {
		return nil, fmt.Errorf("%w: at least one analyzer is required", ErrConfig)
	}
	identities := make([]analyzerIdentity, 0, len(analyzers))
	seen := map[string]bool{}
	for _, analyzer := range analyzers {
		if analyzer == nil {
			return nil, fmt.Errorf("%w: analyzer is required", ErrConfig)
		}
		first := strings.TrimSpace(analyzer.Name())
		second := strings.TrimSpace(analyzer.Name())
		if first == "" || first != second {
			return nil, fmt.Errorf("%w: analyzer identity is empty or unstable", ErrConfig)
		}
		if seen[first] {
			return nil, fmt.Errorf("%w: duplicate analyzer identity", ErrConfig)
		}
		seen[first] = true
		attested, ok := analyzer.(safety.AttestedAnalyzer)
		if !ok {
			return nil, fmt.Errorf("%w: analyzer lacks production attestation", ErrConfig)
		}
		t := reflect.TypeOf(analyzer)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.PkgPath() == "" || t.Name() == "" {
			return nil, fmt.Errorf("%w: analyzer concrete identity is unavailable", ErrConfig)
		}
		concrete := t.PkgPath() + "." + t.Name()
		att := attested.Attestation()
		computedConfig, configErr := safety.ConfigDigest(analyzer)
		if configErr != nil || att.Implementation != concrete || strings.TrimSpace(att.Version) == "" || !validSHA256(att.ConfigDigest) || att.ConfigDigest != computedConfig {
			return nil, fmt.Errorf("%w: analyzer attestation is invalid", ErrConfig)
		}
		identities = append(identities, analyzerIdentity{Name: first, Concrete: concrete, Version: att.Version, ConfigDigest: att.ConfigDigest})
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].Name < identities[j].Name })
	return identities, nil
}

func validSHA256(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func validateProduction(g Guardrail, in Input) error {
	if err := validateConfig(g.Config); err != nil {
		return err
	}
	if len(in.Policy.Rules) == 0 {
		return fmt.Errorf("%w: policy must contain at least one rule", ErrConfig)
	}
	if g.Safety.Now != nil || g.Policy.Now != nil || g.Approval.Now != nil {
		return fmt.Errorf("%w: injectable clocks are not allowed in production", ErrConfig)
	}
	return nil
}

// BuildStatementBindings canonically attributes every safety statement.
func BuildStatementBindings(changes schema.ChangeSet, statements []safety.Statement) ([]StatementBinding, error) {
	if err := changes.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid changes", ErrBinding)
	}
	byID := map[string]schema.Change{}
	for _, change := range changes.Changes {
		byID[change.ID] = change
	}
	out := make([]StatementBinding, len(statements))
	for i, statement := range statements {
		change, ok := byID[statement.ChangeID]
		if !ok {
			return nil, &BindingError{Field: fmt.Sprintf("statement %d change", i+1)}
		}
		hash, err := changeHash(change)
		if err != nil {
			return nil, err
		}
		out[i] = StatementBinding{SQL: statement.SQL, ChangeID: statement.ChangeID, ChangeHash: hash}
	}
	return out, nil
}
func changeHash(change schema.Change) (string, error) {
	raw, err := json.Marshal(change)
	if err != nil {
		return "", fmt.Errorf("%w: change hash", ErrBinding)
	}
	h := sha256.New()
	writeField(h, "autosql.guardrail.statement-change/v1")
	_, _ = h.Write(raw)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func writeField(h hash.Hash, value string) {
	_, _ = fmt.Fprintf(h, "%d:", len(value))
	_, _ = h.Write([]byte(value))
}

// Apply evaluates immutable/static controls first. Approval.Gate durably audits
// authorization before invoking its callback; only that callback may begin the
// database transaction, acquire the lock, run checks, and execute statements.
func (g Guardrail) Apply(ctx context.Context, in Input) (Result, error) {
	var result Result
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := validateConfig(g.Config); err != nil {
		return result, err
	}
	changeDigest, err := ChangeDigest(in.Changes)
	if err != nil {
		return result, err
	}
	result.ChangeDigest = changeDigest
	if in.Precheck.ChangeDigest != changeDigest {
		return result, &BindingError{Field: "precheck change digest"}
	}
	wantPrecheck, err := precheck.Digest(in.Precheck)
	if err != nil {
		return result, stageError(ErrBinding, "precheck digest", err)
	}
	if in.Precheck.Digest != wantPrecheck {
		return result, &BindingError{Field: "precheck plan digest"}
	}
	for i, a := range in.Precheck.Assertions {
		if a.ChangeDigest != changeDigest {
			return result, &BindingError{Field: fmt.Sprintf("assertion %d change digest", i+1)}
		}
		if a.PlanDigest != wantPrecheck {
			return result, &BindingError{Field: fmt.Sprintf("assertion %d plan digest", i+1)}
		}
	}
	if err := validateSafetyStatements(in.StatementBindings, in.Safety.Statements, in.Precheck.Statements, in.Changes); err != nil {
		return result, err
	}
	wantApproval, err := g.BundleDigest(in)
	if err != nil {
		return result, err
	}
	result.BundleDigest = wantApproval
	if in.Approval.Plan.Digest != wantApproval {
		return result, &BindingError{Field: "approval plan digest"}
	}
	if in.Approval.Plan.Environment != g.Config.Environment {
		return result, &BindingError{Field: "approval environment"}
	}

	safetyInput := in.Safety
	safetyInput.Changes = in.Changes
	diagnostics, err := g.Safety.Run(ctx, safetyInput)
	if err != nil {
		return result, stageError(ErrSafety, "analysis", err)
	}
	publicDiagnostics, err := redactedDiagnostics(diagnostics)
	if err != nil {
		return result, stageError(ErrSafety, "diagnostic redaction", err)
	}
	result.Diagnostics = publicDiagnostics
	result.Risk = deriveRisk(g.Config.Risk, publicDiagnostics)
	blocked := blockingDiagnostics(publicDiagnostics, g.Config.FailOn)
	if len(blocked) > 0 {
		return result, &SafetyError{Minimum: g.Config.FailOn, Diagnostics: blocked}
	}

	violations, err := g.Policy.Evaluate(ctx, in.Policy, in.SchemaResources, in.MigrationResources)
	result.Violations = violations
	if err != nil {
		return result, stageError(ErrPolicy, "evaluation", err)
	}
	if len(violations) > 0 {
		return result, &PolicyError{Count: len(violations)}
	}
	if in.Database == nil && in.Mutation == nil {
		return result, stageError(ErrConfig, "database", nil)
	}

	req := in.Approval
	// Caller-provided risk is never authoritative.
	req.Plan.Risk = result.Risk
	callbackRan := false
	gateErr := g.Approval.GuardedApply(ctx, req, func(applyCtx context.Context) error {
		callbackRan = true
		var checks []precheck.Result
		var checkErr error
		if in.Mutation != nil {
			checks, checkErr = in.Mutation.ApplyAuthorized(applyCtx, in.Precheck)
		} else {
			checks, checkErr = precheck.GuardedApply(applyCtx, in.Database, in.Precheck)
		}
		result.Checks = checks
		return checkErr
	})
	if gateErr != nil {
		if callbackRan {
			return result, stageError(ErrPrecheck, "live checks or mutation", gateErr)
		}
		return result, stageError(ErrApproval, "authorization or audit", gateErr)
	}
	return result, nil
}

func validateSafetyStatements(bindings []StatementBinding, got []safety.Statement, statements []string, changes schema.ChangeSet) error {
	if len(got) != len(statements) {
		return &BindingError{Field: "safety statement count"}
	}
	expected, err := BuildStatementBindings(changes, got)
	if err != nil {
		return err
	}
	if len(bindings) != len(expected) {
		return &BindingError{Field: "canonical statement binding count"}
	}
	for i, statement := range got {
		if statement.SQL != statements[i] {
			return &BindingError{Field: fmt.Sprintf("safety statement %d SQL", i+1)}
		}
		if bindings[i] != expected[i] {
			return &BindingError{Field: fmt.Sprintf("canonical statement binding %d", i+1)}
		}
	}
	return nil
}

func redactedDiagnostics(diagnostics []safety.Diagnostic) ([]safety.Diagnostic, error) {
	var encoded bytes.Buffer
	if err := safety.WriteJSON(&encoded, diagnostics); err != nil {
		return nil, err
	}
	var out []safety.Diagnostic
	if err := json.Unmarshal(encoded.Bytes(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateConfig(c Config) error {
	if strings.TrimSpace(c.Environment) == "" {
		return fmt.Errorf("%w: environment is required", ErrConfig)
	}
	if _, ok := severityRank(c.FailOn); !ok {
		return fmt.Errorf("%w: invalid safety threshold", ErrConfig)
	}
	if !validRisk(c.Risk.Baseline) {
		return fmt.Errorf("%w: invalid baseline risk", ErrConfig)
	}
	for sev, risk := range c.Risk.BySeverity {
		if _, ok := severityRank(sev); !ok || !validRisk(risk) {
			return fmt.Errorf("%w: invalid risk mapping", ErrConfig)
		}
	}
	return nil
}
func validRisk(r approval.Risk) bool { return r >= approval.RiskLow && r <= approval.RiskCritical }
func severityRank(s safety.Severity) (int, bool) {
	switch s {
	case safety.SeverityInfo:
		return 1, true
	case safety.SeverityWarning:
		return 2, true
	case safety.SeverityError:
		return 3, true
	default:
		return 0, false
	}
}
func blockingDiagnostics(ds []safety.Diagnostic, min safety.Severity) []safety.Diagnostic {
	rank, _ := severityRank(min)
	var out []safety.Diagnostic
	for _, d := range ds {
		r, ok := severityRank(d.Severity)
		if d.Suppressed == nil && ok && r >= rank {
			out = append(out, d)
		}
	}
	return out
}
func deriveRisk(c RiskConfig, ds []safety.Diagnostic) approval.Risk {
	risk := c.Baseline
	defaults := map[safety.Severity]approval.Risk{safety.SeverityInfo: approval.RiskLow, safety.SeverityWarning: approval.RiskMedium, safety.SeverityError: approval.RiskHigh}
	for _, d := range ds {
		mapped, ok := c.BySeverity[d.Severity]
		if !ok {
			mapped = defaults[d.Severity]
		}
		if mapped > risk {
			risk = mapped
		}
	}
	return risk
}
