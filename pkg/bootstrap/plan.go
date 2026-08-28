package bootstrap

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/plan"
	"autosql/pkg/schema"
)

const PlanVersion = "autosql.bootstrap-plan/v1"

var ErrInvalidBootstrapPlan = errors.New("invalid bootstrap plan")

type ExecutionScope string
type ExecutionStage string
type BootstrapAction string

const (
	ScopeServer   ExecutionScope = "server"
	ScopeDatabase ExecutionScope = "database"

	StageDatabaseTarget ExecutionStage = "database_target"
	StageRoles          ExecutionStage = "roles"
	StageNamespaces     ExecutionStage = "namespaces"
	StageExtensions     ExecutionStage = "extensions"
	StageTypes          ExecutionStage = "types"
	StageRoutines       ExecutionStage = "routines"
	StageStorage        ExecutionStage = "storage"
	StageConstraints    ExecutionStage = "constraints"
	StageIndexes        ExecutionStage = "indexes"
	StageBehavior       ExecutionStage = "behavior"
	StageAccess         ExecutionStage = "access_handoff"

	ActionPrepareDatabase BootstrapAction = "prepare_database"
	ActionExecuteSchema   BootstrapAction = "execute_schema_step"
)

// BootstrapStep binds the server-level database boundary and each immutable
// schema-plan step into one dependency graph. SQL remains in SchemaPlan so it
// has a single digest-bound representation.
type BootstrapStep struct {
	ID           string               `json:"id"`
	Action       BootstrapAction      `json:"action"`
	Stage        ExecutionStage       `json:"stage"`
	Scope        ExecutionScope       `json:"scope"`
	Transaction  plan.TransactionMode `json:"transaction"`
	SchemaStepID string               `json:"schema_step_id,omitempty"`
	ChangeID     string               `json:"change_id,omitempty"`
	DependsOn    []string             `json:"depends_on,omitempty"`
}

type BootstrapPhase struct {
	ID          string               `json:"id"`
	Checkpoint  string               `json:"checkpoint"`
	Stage       ExecutionStage       `json:"stage"`
	Scope       ExecutionScope       `json:"scope"`
	Transaction plan.TransactionMode `json:"transaction"`
	StepIDs     []string             `json:"step_ids"`
}

// Plan is the complete, credential-free execution contract for preparing a
// database and converging every managed resource inside it.
type Plan struct {
	Version    string         `json:"version"`
	Target     DatabaseTarget `json:"target"`
	SchemaPlan plan.Plan      `json:"schema_plan"`
	// Preconditions are managed resources that must already exist exactly as
	// declared before the first in-database schema step. They are part of the
	// signed plan identity but are not rendered as mutations.
	Preconditions         []schema.Resource `json:"preconditions,omitempty"`
	Steps                 []BootstrapStep   `json:"steps"`
	Phases                []BootstrapPhase  `json:"phases"`
	Digest                string            `json:"digest"`
	runtimeAuthorizations []runtimeAuthorization
}

type runtimeAuthorization struct{ value []byte }

func (runtimeAuthorization) String() string   { return "[bound]" }
func (runtimeAuthorization) GoString() string { return "bootstrap.runtimeAuthorization{[bound]}" }

// WithRuntimeAuthorization attaches an opaque, process-bound capability. The
// bytes cannot be read back, serialized, printed through exported fields, or
// included in plan identity. Callers may attach bytes, but only the issuing
// driver can authenticate a capability it generated.
func (p Plan) WithRuntimeAuthorization(capability []byte) Plan {
	p.runtimeAuthorizations = nil
	if len(capability) > 0 {
		p.runtimeAuthorizations = []runtimeAuthorization{{value: append([]byte(nil), capability...)}}
	}
	return p
}

// AddRuntimeAuthorization adds an independently scoped capability without
// exposing any capability already attached to the plan.
func (p Plan) AddRuntimeAuthorization(capability []byte) Plan {
	if len(capability) > 0 {
		p.runtimeAuthorizations = append(p.runtimeAuthorizations, runtimeAuthorization{value: append([]byte(nil), capability...)})
	}
	return p
}

// RuntimeAuthorizationMatches compares a driver-generated capability without
// exposing the bearer value.
func (p Plan) RuntimeAuthorizationMatches(capability []byte) bool {
	matched := 0
	for _, current := range p.runtimeAuthorizations {
		if len(current.value) == len(capability) {
			matched |= subtle.ConstantTimeCompare(current.value, capability)
		}
	}
	return matched == 1
}

// ComposePlan lifts an already validated schema plan into the whole-database
// graph. Runtime URLs and credentials are deliberately not accepted.
func ComposePlan(target DatabaseTarget, schemaPlan plan.Plan) (Plan, error) {
	target = target.Normalize()
	if err := target.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: target: %v", ErrInvalidBootstrapPlan, err)
	}
	if err := schemaPlan.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: schema plan: %v", ErrInvalidBootstrapPlan, err)
	}
	result := compose(target, schemaPlan)
	var err error
	result.Digest, err = digestBootstrapPlan(result)
	if err != nil {
		return Plan{}, err
	}
	return result, result.Validate()
}

// WithPreconditions binds already-existing managed resources into the plan
// identity. Callers provide normalized resources; this method fixes their
// canonical order before signing.
func (p Plan) WithPreconditions(resources []schema.Resource) (Plan, error) {
	p.Preconditions = append([]schema.Resource(nil), resources...)
	sort.Slice(p.Preconditions, func(i, j int) bool {
		return p.Preconditions[i].ID < p.Preconditions[j].ID
	})
	p.Digest = ""
	digest, err := digestBootstrapPlan(p)
	if err != nil {
		return Plan{}, err
	}
	p.Digest = digest
	return p, p.Validate()
}

func (p Plan) Validate() error {
	if p.Version != PlanVersion || p.Digest == "" {
		return fmt.Errorf("%w: incomplete identity", ErrInvalidBootstrapPlan)
	}
	if err := p.Target.Validate(); err != nil {
		return fmt.Errorf("%w: target: %v", ErrInvalidBootstrapPlan, err)
	}
	if err := p.SchemaPlan.Validate(); err != nil {
		return fmt.Errorf("%w: schema plan: %v", ErrInvalidBootstrapPlan, err)
	}
	if len(p.Preconditions) > 0 {
		doc := schema.Document{
			Version: schema.SchemaVersion,
			Graph:   schema.Graph{Resources: append([]schema.Resource(nil), p.Preconditions...)},
		}
		if err := doc.Validate(); err != nil {
			return fmt.Errorf("%w: preconditions: %v", ErrInvalidBootstrapPlan, err)
		}
		for i := 1; i < len(p.Preconditions); i++ {
			if p.Preconditions[i-1].ID >= p.Preconditions[i].ID {
				return fmt.Errorf("%w: preconditions are not in unique stable ID order", ErrInvalidBootstrapPlan)
			}
		}
	}
	want := compose(p.Target, p.SchemaPlan)
	gotGraph, _ := json.Marshal(struct {
		Steps  []BootstrapStep  `json:"steps"`
		Phases []BootstrapPhase `json:"phases"`
	}{p.Steps, p.Phases})
	wantGraph, _ := json.Marshal(struct {
		Steps  []BootstrapStep  `json:"steps"`
		Phases []BootstrapPhase `json:"phases"`
	}{want.Steps, want.Phases})
	if string(gotGraph) != string(wantGraph) {
		return fmt.Errorf("%w: step, dependency, phase, or checkpoint topology", ErrInvalidBootstrapPlan)
	}
	digest, err := digestBootstrapPlan(Plan{Version: p.Version, Target: p.Target, SchemaPlan: p.SchemaPlan, Preconditions: p.Preconditions, Steps: p.Steps, Phases: p.Phases})
	if err != nil || digest != p.Digest {
		return fmt.Errorf("%w: digest mismatch", ErrInvalidBootstrapPlan)
	}
	return nil
}

func (p Plan) MarshalCanonical() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func compose(target DatabaseTarget, schemaPlan plan.Plan) Plan {
	target = target.Normalize()
	prepareID := stableBootstrapID("step", string(ActionPrepareDatabase), target.Name, string(target.Mode))
	steps := []BootstrapStep{{
		ID: prepareID, Action: ActionPrepareDatabase, Stage: StageDatabaseTarget,
		Scope: ScopeServer, Transaction: plan.TransactionProhibited,
	}}
	changes := map[string]schema.Change{}
	for _, change := range schemaPlan.Changes.Changes {
		changes[change.ID] = change
	}
	stepIDs := map[string]string{}
	for _, schemaStep := range schemaPlan.Steps {
		stepIDs[schemaStep.ID] = stableBootstrapID("step", string(ActionExecuteSchema), schemaStep.ID)
	}
	for _, schemaStep := range schemaPlan.Steps {
		change := changes[schemaStep.ChangeID]
		kind := changeKind(change)
		dependencies := []string{prepareID}
		for _, dependency := range schemaStep.DependsOn {
			dependencies = append(dependencies, stepIDs[dependency])
		}
		dependencies = uniqueSorted(dependencies)
		steps = append(steps, BootstrapStep{
			ID: stepIDs[schemaStep.ID], Action: ActionExecuteSchema, Stage: stageForKind(kind),
			Scope: scopeForKind(kind), Transaction: schemaStep.Transaction,
			SchemaStepID: schemaStep.ID, ChangeID: schemaStep.ChangeID, DependsOn: dependencies,
		})
	}
	result := Plan{Version: PlanVersion, Target: target, SchemaPlan: schemaPlan, Steps: steps}
	for _, step := range steps {
		if len(result.Phases) == 0 || !samePhase(result.Phases[len(result.Phases)-1], step) {
			result.Phases = append(result.Phases, BootstrapPhase{Stage: step.Stage, Scope: step.Scope, Transaction: step.Transaction})
		}
		last := len(result.Phases) - 1
		result.Phases[last].StepIDs = append(result.Phases[last].StepIDs, step.ID)
	}
	for index := range result.Phases {
		phase := &result.Phases[index]
		phase.ID = stableBootstrapID("phase", fmt.Sprint(index), string(phase.Stage), string(phase.Scope), string(phase.Transaction), strings.Join(phase.StepIDs, "\x00"))
		phase.Checkpoint = stableBootstrapID("checkpoint", phase.ID)
	}
	return result
}

func changeKind(change schema.Change) schema.Kind {
	if change.After != nil {
		return change.After.Kind
	}
	if change.Before != nil {
		return change.Before.Kind
	}
	return ""
}

func stageForKind(kind schema.Kind) ExecutionStage {
	switch kind {
	case schema.KindDatabase:
		return StageDatabaseTarget
	case schema.KindRole:
		return StageRoles
	case schema.KindSchema:
		return StageNamespaces
	case schema.KindExtension:
		return StageExtensions
	case schema.KindEnum, schema.KindDomain, schema.KindComposite:
		return StageTypes
	case schema.KindFunction, schema.KindProcedure:
		return StageRoutines
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
		return StageConstraints
	case schema.KindIndex:
		return StageIndexes
	case schema.KindTrigger, schema.KindPolicy:
		return StageBehavior
	case schema.KindGrant, schema.KindMembership, schema.KindDefaultPrivilege:
		return StageAccess
	default:
		return StageStorage
	}
}

func scopeForKind(kind schema.Kind) ExecutionScope {
	switch kind {
	case schema.KindDatabase, schema.KindRole, schema.KindMembership:
		return ScopeServer
	default:
		return ScopeDatabase
	}
}

func samePhase(phase BootstrapPhase, step BootstrapStep) bool {
	return phase.Stage == step.Stage && phase.Scope == step.Scope && phase.Transaction == step.Transaction
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	// prepare_database intentionally remains first; all other IDs sort for a
	// stable dependency set without weakening the graph.
	if len(out) > 1 {
		first := out[0]
		rest := append([]string(nil), out[1:]...)
		for i := 1; i < len(rest); i++ {
			for j := i; j > 0 && rest[j] < rest[j-1]; j-- {
				rest[j], rest[j-1] = rest[j-1], rest[j]
			}
		}
		out = append([]string{first}, rest...)
	}
	return out
}

func stableBootstrapID(domain string, values ...string) string {
	hash := sha256.New()
	hash.Write([]byte("autosql.bootstrap-plan." + domain + "/v1\x00"))
	for _, value := range values {
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	return domain + ":" + hex.EncodeToString(hash.Sum(nil))[:24]
}

func digestBootstrapPlan(p Plan) (string, error) {
	p.Digest = ""
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("autosql.bootstrap-plan.digest/v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
