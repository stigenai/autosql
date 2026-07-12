// Package plan builds immutable, deterministic database execution plans.
package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"autosql/pkg/plugin"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
)

const Version = "autosql.plan/v1"
const PlannerVersion = "0.1.0"

var ErrInvalidPlan = errors.New("invalid migration plan")
var ErrUnsupportedTransition = errors.New("unsupported schema transition")

type TransactionMode string
type StepKind string

const (
	TransactionRequired   TransactionMode = "required"
	TransactionProhibited TransactionMode = "prohibited"
)
const (
	StepExecutable StepKind = "executable"
	StepTopology   StepKind = "topology"
)

type LockLevel string

const (
	LockNone      LockLevel = "none"
	LockShare     LockLevel = "share"
	LockExclusive LockLevel = "exclusive"
)

type Impact struct {
	Destructive bool `json:"destructive,omitempty"`
	Rewrites    bool `json:"rewrites,omitempty"`
	Scans       bool `json:"scans,omitempty"`
}

type DriverIdentity struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Step struct {
	ID          string          `json:"id"`
	ChangeID    string          `json:"change_id"`
	SQL         string          `json:"sql"`
	Kind        StepKind        `json:"kind"`
	Transaction TransactionMode `json:"transaction"`
	Lock        LockLevel       `json:"lock"`
	Impact      Impact          `json:"impact"`
	DependsOn   []string        `json:"depends_on,omitempty"`
}

type Phase struct {
	ID          string          `json:"id"`
	Transaction TransactionMode `json:"transaction"`
	StepIDs     []string        `json:"step_ids"`
}

type Plan struct {
	Version         string           `json:"version"`
	PlannerVersion  string           `json:"planner_version"`
	Driver          DriverIdentity   `json:"driver"`
	FromFingerprint string           `json:"from_fingerprint"`
	ToFingerprint   string           `json:"to_fingerprint"`
	Changes         schema.ChangeSet `json:"changes"`
	Steps           []Step           `json:"steps"`
	Phases          []Phase          `json:"phases"`
	Digest          string           `json:"digest"`
}

type Options struct {
	Diff   schema.DiffOptions
	Render map[string]string
}

// Build validates both graphs, computes the exact change set, renders all SQL,
// and publishes a plan only after every transition has succeeded.
func Build(ctx context.Context, driver plugin.Driver, current, desired schema.Document, options Options) (Plan, error) {
	if err := current.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: current graph: %v", ErrInvalidPlan, err)
	}
	if err := desired.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: desired graph: %v", ErrInvalidPlan, err)
	}
	if !documentMetadataEqual(current, desired) {
		return Plan{}, fmt.Errorf("%w: document or graph metadata differs and has no renderable transition", ErrUnsupportedTransition)
	}
	from, err := schema.SemanticFingerprint(current)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: current fingerprint: %v", ErrInvalidPlan, err)
	}
	to, err := schema.SemanticFingerprint(desired)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: desired fingerprint: %v", ErrInvalidPlan, err)
	}
	changes, err := schema.Diff(current, desired, options.Diff)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: diff: %v", ErrInvalidPlan, err)
	}
	statements, err := driver.Render(ctx, plugin.RenderRequest{Changes: changes, Current: current, Desired: desired, Options: cloneMap(options.Render)})
	if err != nil {
		return Plan{}, fmt.Errorf("%w: %w", ErrUnsupportedTransition, err)
	}
	steps, err := bindSteps(changes, statements)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Version: Version, PlannerVersion: PlannerVersion, Driver: DriverIdentity{Name: driver.Info().Name, Version: driver.Info().Version}, FromFingerprint: from, ToFingerprint: to, Changes: changes, Steps: steps}
	p.Phases = phases(steps)
	p.Digest, err = digestPlan(p)
	if err != nil {
		return Plan{}, err
	}
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return Plan{}, err
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return Plan{}, err
	}
	return p, nil
}

func (p Plan) Validate() error {
	if p.Version != Version || p.PlannerVersion != PlannerVersion || p.Driver.Name == "" || p.Driver.Version == "" || !validFingerprint(p.FromFingerprint) || !validFingerprint(p.ToFingerprint) || p.Digest == "" {
		return fmt.Errorf("%w: incomplete identity", ErrInvalidPlan)
	}
	if err := p.Changes.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPlan, err)
	}
	changeIDs := map[string]bool{}
	for _, c := range p.Changes.Changes {
		changeIDs[c.ID] = true
	}
	stepIDs := map[string]bool{}
	for _, s := range p.Steps {
		if s.ID == "" || !changeIDs[s.ChangeID] || stepIDs[s.ID] || s.Kind == StepExecutable && strings.TrimSpace(s.SQL) == "" || s.Kind == StepTopology && s.SQL != "" {
			return fmt.Errorf("%w: invalid step", ErrInvalidPlan)
		}
		if s.Transaction != TransactionRequired && s.Transaction != TransactionProhibited {
			return fmt.Errorf("%w: transaction mode", ErrInvalidPlan)
		}
		if s.Kind != StepExecutable && s.Kind != StepTopology {
			return fmt.Errorf("%w: step kind", ErrInvalidPlan)
		}
		if s.Lock != LockNone && s.Lock != LockShare && s.Lock != LockExclusive {
			return fmt.Errorf("%w: lock level", ErrInvalidPlan)
		}
		stepIDs[s.ID] = true
	}
	rebuilt, err := bindSteps(p.Changes, p.renderedStatements())
	if err != nil || !reflect.DeepEqual(rebuilt, p.Steps) {
		return fmt.Errorf("%w: step identity, coverage, order, or topology", ErrInvalidPlan)
	}
	if expected := phases(p.Steps); !reflect.DeepEqual(expected, p.Phases) {
		return fmt.Errorf("%w: phase identity, coverage, or order", ErrInvalidPlan)
	}
	for _, s := range p.Steps {
		for _, dep := range s.DependsOn {
			if !stepIDs[dep] {
				return fmt.Errorf("%w: missing step dependency", ErrInvalidPlan)
			}
		}
	}
	want, err := digestPlan(Plan{Version: p.Version, PlannerVersion: p.PlannerVersion, Driver: p.Driver, FromFingerprint: p.FromFingerprint, ToFingerprint: p.ToFingerprint, Changes: p.Changes, Steps: p.Steps, Phases: p.Phases})
	if err != nil || want != p.Digest {
		return fmt.Errorf("%w: digest mismatch", ErrInvalidPlan)
	}
	return nil
}

func validFingerprint(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}
func documentMetadataEqual(a, b schema.Document) bool {
	type metadata struct {
		Annotations   map[string]string          `json:"annotations,omitempty"`
		DocumentExtra map[string]json.RawMessage `json:"document_extra,omitempty"`
		GraphExtra    map[string]json.RawMessage `json:"graph_extra,omitempty"`
	}
	am, _ := json.Marshal(metadata{a.Annotations, a.Extra, a.Graph.Extra})
	bm, _ := json.Marshal(metadata{b.Annotations, b.Extra, b.Graph.Extra})
	return string(am) == string(bm)
}

func (p Plan) MarshalCanonical() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Statements returns a defensive copy in the existing plugin/guardrail
// binding shape. It does not execute the plan.
func (p Plan) Statements() []plugin.Statement {
	var out []plugin.Statement
	for _, step := range p.Steps {
		if step.Kind == StepTopology {
			continue
		}
		out = append(out, plugin.Statement{SQL: step.SQL, ChangeID: step.ChangeID, Transactional: step.Transaction == TransactionRequired, Kind: plugin.StatementExecutable})
	}
	return out
}
func (p Plan) renderedStatements() []plugin.Statement {
	out := make([]plugin.Statement, len(p.Steps))
	for i, step := range p.Steps {
		kind := plugin.StatementExecutable
		if step.Kind == StepTopology {
			kind = plugin.StatementTopology
		}
		out[i] = plugin.Statement{SQL: step.SQL, ChangeID: step.ChangeID, Transactional: step.Transaction == TransactionRequired, Kind: kind}
	}
	return out
}

// SafetyStatements returns exact SQL/change bindings accepted by the existing
// guardrail safety and statement-binding pipeline.
func (p Plan) SafetyStatements() []safety.Statement {
	var out []safety.Statement
	for _, step := range p.Steps {
		if step.Kind == StepTopology {
			continue
		}
		out = append(out, safety.Statement{SQL: step.SQL, ChangeID: step.ChangeID})
	}
	return out
}

// EditSQL replaces executable SQL in order, rebuilds step/phase identities and
// returns a new unsigned plan digest. It cannot add, remove, or reorder the
// renderer's exact statement/change bindings.
func EditSQL(p Plan, sql []string) (Plan, error) {
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	count := 0
	for _, s := range p.Steps {
		if s.Kind == StepExecutable {
			count++
		}
	}
	if len(sql) != count {
		return Plan{}, fmt.Errorf("%w: edited statement count", ErrInvalidPlan)
	}
	oldToNew := map[string]string{}
	idx := 0
	for i := range p.Steps {
		old := p.Steps[i].ID
		if p.Steps[i].Kind == StepExecutable {
			if strings.TrimSpace(sql[idx]) == "" {
				return Plan{}, fmt.Errorf("%w: empty edited SQL", ErrInvalidPlan)
			}
			p.Steps[i].SQL = sql[idx]
			idx++
		}
		p.Steps[i].ID = stableID("step", p.Steps[i].ChangeID, fmt.Sprint(i), string(p.Steps[i].Kind), p.Steps[i].SQL, string(p.Steps[i].Transaction))
		oldToNew[old] = p.Steps[i].ID
	}
	for i := range p.Steps {
		for j, d := range p.Steps[i].DependsOn {
			n := oldToNew[d]
			if n == "" {
				return Plan{}, fmt.Errorf("%w: edited dependency", ErrInvalidPlan)
			}
			p.Steps[i].DependsOn[j] = n
		}
		sort.Strings(p.Steps[i].DependsOn)
	}
	p.Phases = phases(p.Steps)
	p.Digest = ""
	d, err := digestPlan(p)
	if err != nil {
		return Plan{}, err
	}
	p.Digest = d
	if err = p.Validate(); err != nil {
		return Plan{}, err
	}
	return p, nil
}

func bindSteps(changes schema.ChangeSet, statements []plugin.Statement) ([]Step, error) {
	byChange := map[string]schema.Change{}
	for _, c := range changes.Changes {
		byChange[c.ID] = c
	}
	if len(changes.Changes) > 0 && len(statements) == 0 {
		return nil, fmt.Errorf("%w: renderer returned zero statements", ErrUnsupportedTransition)
	}
	lastByChange := map[string]string{}
	seenChange := map[string]bool{}
	steps := make([]Step, 0, len(statements))
	for idx, statement := range statements {
		change, ok := byChange[statement.ChangeID]
		kind := StepExecutable
		if statement.Kind == plugin.StatementTopology {
			kind = StepTopology
		}
		if !ok || kind == StepExecutable && strings.TrimSpace(statement.SQL) == "" || kind == StepTopology && statement.SQL != "" {
			return nil, fmt.Errorf("%w: unbound renderer statement", ErrInvalidPlan)
		}
		mode := TransactionRequired
		if !statement.Transactional {
			mode = TransactionProhibited
		}
		id := stableID("step", statement.ChangeID, fmt.Sprint(idx), string(kind), statement.SQL, string(mode))
		deps := []string{}
		for _, dep := range change.DependsOn {
			step := lastByChange[dep]
			if step == "" {
				return nil, fmt.Errorf("%w: change dependency %s is not planned earlier", ErrInvalidPlan, dep)
			}
			deps = append(deps, step)
		}
		if previous := lastByChange[change.ID]; previous != "" {
			deps = append(deps, previous)
		}
		sort.Strings(deps)
		lock, impact := lockFor(change, statement.SQL), impactFor(change, statement.SQL)
		if kind == StepTopology {
			lock = LockNone
			impact = Impact{}
		}
		steps = append(steps, Step{ID: id, ChangeID: change.ID, SQL: statement.SQL, Kind: kind, Transaction: mode, Lock: lock, Impact: impact, DependsOn: deps})
		lastByChange[change.ID] = id
		seenChange[change.ID] = true
	}
	for id := range byChange {
		if !seenChange[id] {
			return nil, fmt.Errorf("%w: change %s has no statement", ErrUnsupportedTransition, id)
		}
	}
	return steps, nil
}

func phases(steps []Step) []Phase {
	var out []Phase
	for _, s := range steps {
		if len(out) == 0 || out[len(out)-1].Transaction != s.Transaction {
			out = append(out, Phase{Transaction: s.Transaction})
		}
		out[len(out)-1].StepIDs = append(out[len(out)-1].StepIDs, s.ID)
	}
	for i := range out {
		out[i].ID = stableID("phase", fmt.Sprint(i), string(out[i].Transaction), strings.Join(out[i].StepIDs, "\x00"))
	}
	return out
}

func lockFor(c schema.Change, sql string) LockLevel {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "-- AUTOSQL TOPOLOGY:") {
		return LockNone
	}
	if strings.Contains(strings.ToUpper(sql), "CONCURRENTLY") {
		return LockShare
	}
	if c.Operation == schema.OperationCreate && c.After != nil && c.After.Kind == schema.KindSchema {
		return LockNone
	}
	return LockExclusive
}
func impactFor(c schema.Change, sql string) Impact {
	u := strings.ToUpper(sql)
	impact := Impact{Destructive: c.Operation == schema.OperationDrop || strings.HasPrefix(strings.TrimSpace(u), "DROP "), Rewrites: strings.Contains(u, " TYPE ") || strings.Contains(u, "SET DATA TYPE"), Scans: strings.Contains(u, "VALIDATE") || strings.Contains(u, "CREATE INDEX")}
	r := c.After
	if r == nil {
		r = c.Before
	}
	if r != nil {
		s := map[string]any{}
		_ = json.Unmarshal(r.Spec, &s)
		if r.Kind == schema.KindColumn {
			if d, _ := s["default"].(string); d != "" && !simpleDefault(d) {
				impact.Rewrites = true
			}
			if c.Before != nil && c.After != nil {
				b := map[string]any{}
				a := map[string]any{}
				_ = json.Unmarshal(c.Before.Spec, &b)
				_ = json.Unmarshal(c.After.Spec, &a)
				bn, _ := b["not_null"].(bool)
				an, _ := a["not_null"].(bool)
				if !bn && an {
					impact.Scans = true
				}
			}
		}
		if r.Kind == schema.KindMaterializedView && (c.Operation == schema.OperationCreate || c.Operation == schema.OperationAlter) {
			impact.Scans = true
		}
		switch r.Kind {
		case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
			if c.Operation == schema.OperationCreate || c.Operation == schema.OperationAlter {
				impact.Scans = true
			}
		}
	}
	return impact
}
func simpleDefault(value string) bool {
	value = strings.TrimSpace(value)
	if value == "true" || value == "false" || value == "NULL" {
		return true
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' && !strings.Contains(value[1:len(value)-1], "'") {
		return true
	}
	for _, r := range value {
		if (r < '0' || r > '9') && r != '.' && r != '-' {
			return false
		}
	}
	return value != ""
}
func stableID(domain string, values ...string) string {
	h := sha256.New()
	h.Write([]byte("autosql.plan." + domain + "/v1\x00"))
	for _, v := range values {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return domain + ":" + hex.EncodeToString(h.Sum(nil))[:24]
}
func digestPlan(p Plan) (string, error) {
	p.Digest = ""
	b, e := json.Marshal(p)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(append([]byte("autosql.plan.digest/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
