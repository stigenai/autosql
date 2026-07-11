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
	"strings"

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

type PolicyError struct{ Violations []policy.Violation }

func (e *PolicyError) Error() string {
	return fmt.Sprintf("%v: %d violations", ErrPolicy, len(e.Violations))
}
func (e *PolicyError) Unwrap() error { return ErrPolicy }

type StageError struct {
	Kind  error
	Stage string
	Err   error
}

func (e *StageError) Error() string        { return fmt.Sprintf("%v during %s", e.Kind, e.Stage) }
func (e *StageError) Unwrap() error        { return e.Err }
func (e *StageError) Is(target error) bool { return target == e.Kind || errors.Is(e.Err, target) }

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
	SchemaResources    []policy.Resource
	MigrationResources []policy.Resource
	Precheck           precheck.Plan
	Approval           approval.Request
	Database           precheck.DB
}

type Result struct {
	ChangeDigest   string
	ApprovalDigest string
	Risk           approval.Risk
	Diagnostics    []safety.Diagnostic
	Violations     []policy.Violation
	Checks         []precheck.Result
}

// ChangeDigest hashes the canonical, versioned ChangeSet representation.
func ChangeDigest(changes schema.ChangeSet) (string, error) {
	canonical, err := changes.MarshalCanonical()
	if err != nil {
		return "", fmt.Errorf("%w: canonical changes: %v", ErrBinding, err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ApprovalDigest binds the exact changes, exact live-check plan, and deployment
// environment with an unambiguous length-prefixed, domain-separated encoding.
func ApprovalDigest(changeDigest, precheckDigest, environment string) (string, error) {
	if strings.TrimSpace(changeDigest) == "" || strings.TrimSpace(precheckDigest) == "" || strings.TrimSpace(environment) == "" {
		return "", fmt.Errorf("%w: approval digest inputs are required", ErrBinding)
	}
	h := sha256.New()
	writeField(h, "autosql.guardrail.approval/v1")
	writeField(h, changeDigest)
	writeField(h, precheckDigest)
	writeField(h, environment)
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
		return result, &StageError{Kind: ErrBinding, Stage: "precheck digest", Err: err}
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
	if err := validateSafetyStatements(in.Safety.Statements, in.Precheck.Statements, in.Changes); err != nil {
		return result, err
	}
	wantApproval, err := ApprovalDigest(changeDigest, wantPrecheck, g.Config.Environment)
	if err != nil {
		return result, err
	}
	result.ApprovalDigest = wantApproval
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
		return result, &StageError{Kind: ErrSafety, Stage: "analysis", Err: err}
	}
	publicDiagnostics, err := redactedDiagnostics(diagnostics)
	if err != nil {
		return result, &StageError{Kind: ErrSafety, Stage: "diagnostic redaction", Err: err}
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
		return result, &StageError{Kind: ErrPolicy, Stage: "evaluation", Err: err}
	}
	if len(violations) > 0 {
		return result, &PolicyError{Violations: violations}
	}
	if in.Database == nil {
		return result, &StageError{Kind: ErrConfig, Stage: "database", Err: errors.New("database is required")}
	}

	req := in.Approval
	// Caller-provided risk is never authoritative.
	req.Plan.Risk = result.Risk
	callbackRan := false
	gateErr := g.Approval.GuardedApply(ctx, req, func(applyCtx context.Context) error {
		callbackRan = true
		checks, checkErr := precheck.GuardedApply(applyCtx, in.Database, in.Precheck)
		result.Checks = checks
		return checkErr
	})
	if gateErr != nil {
		if callbackRan {
			return result, &StageError{Kind: ErrPrecheck, Stage: "live checks or mutation", Err: gateErr}
		}
		return result, &StageError{Kind: ErrApproval, Stage: "authorization or audit", Err: gateErr}
	}
	return result, nil
}

func validateSafetyStatements(got []safety.Statement, statements []string, changes schema.ChangeSet) error {
	if len(got) != len(statements) {
		return &BindingError{Field: "safety statement count"}
	}
	changeIDs := make(map[string]bool, len(changes.Changes))
	for _, change := range changes.Changes {
		changeIDs[change.ID] = true
	}
	for i, statement := range got {
		if statement.SQL != statements[i] {
			return &BindingError{Field: fmt.Sprintf("safety statement %d SQL", i+1)}
		}
		if !changeIDs[statement.ChangeID] {
			return &BindingError{Field: fmt.Sprintf("safety statement %d change", i+1)}
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
		return fmt.Errorf("%w: invalid safety threshold %q", ErrConfig, c.FailOn)
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
		if d.Suppressed != nil {
			continue
		}
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
