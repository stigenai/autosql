// Package expandplan produces immutable, read-only plans for the expand phase
// of a zero-downtime migration.
package expandplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/zerodowntime"
	"github.com/jackc/pgx/v5"
)

const Version = "autosql.zero-downtime-expand-plan/v1"

var ErrRefused = errors.New("expand planning refused")

type Policy struct {
	MaxLockMS                int  `json:"max_lock_ms"`
	MaxStatementMS           int  `json:"max_statement_ms"`
	MaxTransactionMS         int  `json:"max_transaction_ms"`
	AllowRewrite             bool `json:"allow_rewrite"`
	AllowTableScan           bool `json:"allow_table_scan"`
	AllowValidationScan      bool `json:"allow_validation_scan"`
	AllowNonTransactional    bool `json:"allow_non_transactional"`
	AllowMaintenanceRequired bool `json:"allow_maintenance_required"`
}

type Column struct {
	Name, Type string
	Nullable   bool
}
type Table struct {
	Schema, Name, Owner string
	Columns             map[string]Column
	Partitioned         bool
}
type Index struct {
	Schema, Name, Table, Owner    string
	ConstraintBacked, Partitioned bool
}
type Snapshot struct {
	Fingerprint, Target, Environment string
	PostgresMajor                    int
	Tables                           map[string]Table
	Indexes                          map[string]Index
	Mappings                         map[string]Mapping
	Schemas                          []string
}
type Mapping struct {
	OperationID string `json:"operation_id"`
	LogicalID   string `json:"logical_id"`
	Schema      string `json:"physical_schema"`
	Name        string `json:"physical_name"`
	Kind        string `json:"object_kind"`
}

type Condition struct {
	Kind     string `json:"kind"`
	Object   string `json:"object,omitempty"`
	Expected string `json:"expected"`
}
type Step struct {
	Ordinal           int         `json:"ordinal"`
	OperationID       string      `json:"operation_id"`
	Kind              string      `json:"kind"`
	SQL               string      `json:"sql"`
	TransactionGroup  string      `json:"transaction_group"`
	LockMode          string      `json:"lock_mode"`
	Recovery          string      `json:"recovery"`
	LockBudgetMS      int         `json:"lock_budget_ms"`
	StatementBudgetMS int         `json:"statement_budget_ms"`
	Rewrite           bool        `json:"rewrite"`
	ValidationScan    bool        `json:"validation_scan"`
	TableScan         bool        `json:"table_scan"`
	NonTransactional  bool        `json:"nontransactional"`
	Reversible        bool        `json:"reversible"`
	Preconditions     []Condition `json:"preconditions"`
	Postconditions    []Condition `json:"postconditions"`
	Handoff           []string    `json:"handoff,omitempty"`
}
type Plan struct {
	Version          string    `json:"version"`
	ArtifactDigest   string    `json:"artifact_digest"`
	FromFingerprint  string    `json:"from_fingerprint"`
	Target           string    `json:"target"`
	Environment      string    `json:"environment"`
	PostgresMajor    int       `json:"postgres_major"`
	CapabilityDigest string    `json:"capability_digest"`
	BindingsDigest   string    `json:"bindings_digest"`
	PolicyDigest     string    `json:"policy_digest"`
	Steps            []Step    `json:"steps"`
	Mappings         []Mapping `json:"mappings"`
	Digest           string    `json:"digest"`
}

type Request struct {
	Migration                                zerodowntime.Migration
	Snapshot                                 Snapshot
	ExpectedFingerprint, Target, Environment string
	Policy                                   Policy
	Verify                                   func(zerodowntime.Migration) error
}
type bindings struct {
	Artifact    string `json:"artifact"`
	From        string `json:"from"`
	Target      string `json:"target"`
	Environment string `json:"environment"`
	Capability  string `json:"capability"`
	Policy      string `json:"policy"`
}

func Build(r Request) (Plan, error) {
	if r.Verify == nil {
		return Plan{}, refuse("artifact signature verifier is required")
	}
	if err := r.Migration.Validate(); err != nil {
		return Plan{}, refuse("artifact: %v", err)
	}
	if err := r.Verify(r.Migration); err != nil {
		return Plan{}, refuse("artifact signature: %v", err)
	}
	if err := r.Migration.ValidateForPostgres(r.Snapshot.PostgresMajor); err != nil {
		return Plan{}, refuse("capability: %v", err)
	}
	if r.ExpectedFingerprint == "" || r.ExpectedFingerprint != r.Snapshot.Fingerprint {
		return Plan{}, refuse("stale live schema fingerprint")
	}
	if r.Target == "" || r.Environment == "" || r.Target != r.Snapshot.Target || r.Environment != r.Snapshot.Environment {
		return Plan{}, refuse("target or environment identity mismatch")
	}
	if r.Policy.MaxLockMS <= 0 || r.Policy.MaxStatementMS <= 0 || r.Policy.MaxTransactionMS <= 0 {
		return Plan{}, refuse("positive availability budgets are required")
	}
	if r.Migration.Requirements.LockTimeoutMS > r.Policy.MaxLockMS || r.Migration.Requirements.StatementTimeoutMS > r.Policy.MaxStatementMS {
		return Plan{}, refuse("artifact timeout exceeds policy")
	}
	p := Plan{Version: Version, ArtifactDigest: r.Migration.Digest, FromFingerprint: r.Snapshot.Fingerprint, Target: r.Target, Environment: r.Environment, PostgresMajor: r.Snapshot.PostgresMajor, CapabilityDigest: digest(struct {
		PostgresMajor int `json:"postgres_major"`
	}{r.Snapshot.PostgresMajor}), PolicyDigest: digest(r.Policy)}
	p.BindingsDigest = digest(bindings{p.ArtifactDigest, p.FromFingerprint, p.Target, p.Environment, p.CapabilityDigest, p.PolicyDigest})
	used := map[string]string{}
	for _, m := range r.Snapshot.Mappings {
		key := m.Schema + "." + m.Name
		if prior, ok := used[key]; ok && prior != m.OperationID+"/"+m.LogicalID {
			return Plan{}, refuse("tampered duplicate physical mapping %s", key)
		}
		used[key] = m.OperationID + "/" + m.LogicalID
	}
	for _, op := range r.Migration.Operations {
		steps, mappings, err := translate(r, op, used)
		if err != nil {
			return Plan{}, err
		}
		p.Steps = append(p.Steps, steps...)
		p.Mappings = append(p.Mappings, mappings...)
	}
	for i := range p.Steps {
		p.Steps[i].Ordinal = i + 1
	}
	sort.Slice(p.Mappings, func(i, j int) bool {
		a, b := p.Mappings[i], p.Mappings[j]
		return a.OperationID+"/"+a.LogicalID < b.OperationID+"/"+b.LogicalID
	})
	p.Digest = digest(p)
	return p, nil
}

// Validate proves that a serialized plan is canonical and has not been edited.
func (p Plan) Validate() error {
	if p.Version != Version || p.ArtifactDigest == "" || p.FromFingerprint == "" || p.Target == "" || p.Environment == "" || p.PostgresMajor < 14 || p.PostgresMajor > 18 || p.CapabilityDigest == "" || p.BindingsDigest == "" || p.PolicyDigest == "" || p.Digest == "" {
		return refuse("incomplete plan bindings")
	}
	wantBindings := digest(bindings{p.ArtifactDigest, p.FromFingerprint, p.Target, p.Environment, p.CapabilityDigest, p.PolicyDigest})
	if wantBindings != p.BindingsDigest {
		return refuse("bindings digest mismatch")
	}
	got := p.Digest
	p.Digest = ""
	if digest(p) != got {
		return refuse("plan digest mismatch")
	}
	for i, s := range p.Steps {
		if s.Ordinal != i+1 || s.OperationID == "" || s.Kind == "" || s.LockMode == "" || s.LockBudgetMS <= 0 || s.StatementBudgetMS <= 0 {
			return refuse("invalid step %d", i+1)
		}
		upper := strings.ToUpper(s.SQL)
		if strings.Contains(upper, " DROP ") || strings.Contains(upper, " RENAME ") || strings.Contains(upper, " ALTER COLUMN ") {
			return refuse("destructive expand SQL")
		}
	}
	return nil
}

func translate(r Request, op zerodowntime.Operation, used map[string]string) ([]Step, []Mapping, error) {
	table, exists := r.Snapshot.Tables[op.Table]
	base := Step{OperationID: op.ID, TransactionGroup: "expand-" + op.ID, LockBudgetMS: r.Migration.Requirements.LockTimeoutMS, StatementBudgetMS: r.Migration.Requirements.StatementTimeoutMS, Reversible: true, Recovery: "drop the additive physical object before synchronization", Preconditions: []Condition{{Kind: "fingerprint", Expected: r.Snapshot.Fingerprint}}}
	if op.Kind != zerodowntime.AddTable && !exists {
		return nil, nil, refuse("operation %s references missing or ambiguous table %s", op.ID, op.Table)
	}
	if exists && (table.Owner == "" || table.Schema == "") {
		return nil, nil, refuse("operation %s has ambiguous ownership or schema", op.ID)
	}
	qtable := qi(table.Schema, table.Name)
	switch op.Kind {
	case zerodowntime.AddTable:
		if exists {
			return nil, nil, refuse("table %s already exists", op.Table)
		}
		if len(r.Snapshot.Schemas) != 1 {
			return nil, nil, refuse("add_table requires exactly one unambiguous application schema")
		}
		name, m, err := physical(r, op, r.Snapshot.Schemas[0], "table", op.Table, used)
		if err != nil {
			return nil, nil, err
		}
		base.Kind = "create_shadow_table"
		base.LockMode = "ACCESS EXCLUSIVE"
		base.SQL = "CREATE TABLE " + qi(m.Schema, name) + " ()"
		base.Postconditions = []Condition{{Kind: "relation_exists", Object: m.Schema + "." + name, Expected: "table"}}
		return []Step{base}, []Mapping{m}, nil
	case zerodowntime.AddColumn:
		if _, ok := table.Columns[op.Column]; ok {
			return nil, nil, refuse("column %s.%s already exists", op.Table, op.Column)
		}
		base.Kind = "add_column"
		base.LockMode = "ACCESS EXCLUSIVE"
		base.SQL = "ALTER TABLE " + qtable + " ADD COLUMN " + qi(op.Column) + " " + op.DataType
		base.Postconditions = []Condition{{Kind: "column_exists", Object: table.Schema + "." + table.Name + "." + op.Column, Expected: op.DataType}}
		if op.SynchronizationMode == "backfill" {
			base.Handoff = []string{"backfill:" + op.Expression, "ordering:" + strings.Join(op.Ordering.Columns, ","), fmt.Sprintf("batch_size:%d", op.BatchSize)}
		}
		return []Step{base}, nil, nil
	case zerodowntime.CreateIndex:
		if _, ok := r.Snapshot.Indexes[op.Index]; ok {
			return nil, nil, refuse("index %s already exists", op.Index)
		}
		if op.IndexMode == nil || !op.IndexMode.Concurrent || op.IndexMode.Partitioned || op.IndexMode.BacksConstraint || table.Partitioned {
			return nil, nil, refuse("concurrent index capability is unavailable for partitioned or constraint-backed objects")
		}
		name, m, err := physical(r, op, table.Schema, "index", op.Index, used)
		if err != nil {
			return nil, nil, err
		}
		base.Kind = "create_index_concurrently"
		base.LockMode = "SHARE UPDATE EXCLUSIVE"
		base.NonTransactional = true
		base.TableScan = true
		base.TransactionGroup = "none"
		base.SQL = "CREATE " + map[bool]string{true: "UNIQUE ", false: ""}[op.Unique] + "INDEX CONCURRENTLY " + qi(table.Schema, name) + " ON " + qtable + " (" + op.Expression + ")"
		base.Postconditions = []Condition{{Kind: "valid_index", Object: table.Schema + "." + name, Expected: "true"}}
		if !r.Policy.AllowNonTransactional || !r.Policy.AllowTableScan {
			return nil, nil, refuse("concurrent index exceeds nontransactional or table-scan policy")
		}
		return []Step{base}, []Mapping{m}, nil
	case zerodowntime.DropColumn, zerodowntime.DropTable, zerodowntime.DropIndex:
		// Destruction is deliberately deferred to contract; expand records no SQL.
		base.Kind = "defer_contract"
		base.LockMode = "NONE"
		base.SQL = ""
		base.Recovery = "no expand mutation"
		base.Preconditions = append(base.Preconditions, Condition{Kind: "object_preserved", Object: op.Table, Expected: "true"})
		base.Postconditions = base.Preconditions
		return []Step{base}, nil, nil
	case zerodowntime.SetNotNull:
		col, ok := table.Columns[op.Column]
		if !ok {
			return nil, nil, refuse("source column %s.%s is missing", op.Table, op.Column)
		}
		if !col.Nullable {
			return nil, nil, refuse("column %s.%s is already not null", op.Table, op.Column)
		}
		name, m, err := physical(r, op, table.Schema, "constraint", op.Column+"_not_null", used)
		if err != nil {
			return nil, nil, err
		}
		base.Kind = "add_not_valid_check"
		base.LockMode = "ACCESS EXCLUSIVE"
		base.SQL = "ALTER TABLE " + qtable + " ADD CONSTRAINT " + qi(name) + " CHECK (" + qi(op.Column) + " IS NOT NULL) NOT VALID"
		base.Handoff = []string{"backfill:" + op.Expression, "ordering:" + strings.Join(op.Ordering.Columns, ","), fmt.Sprintf("batch_size:%d", op.BatchSize), "validate_constraint:" + name}
		base.Postconditions = []Condition{{Kind: "constraint_exists_not_valid", Object: table.Schema + "." + table.Name + "." + name, Expected: "true"}}
		return []Step{base}, []Mapping{m}, nil
	case zerodowntime.RenameColumn, zerodowntime.AlterColumnType:
		if op.Column == "" {
			return nil, nil, refuse("operation %s requires a source column", op.ID)
		}
		col, ok := table.Columns[op.Column]
		if !ok {
			return nil, nil, refuse("source column %s.%s is missing", op.Table, op.Column)
		}
		logical := op.NewName
		if logical == "" {
			logical = op.Column + "_shadow"
		}
		name, m, err := physical(r, op, table.Schema, "column", logical, used)
		if err != nil {
			return nil, nil, err
		}
		typ := op.DataType
		if typ == "" {
			typ = col.Type
		}
		base.Kind = "add_shadow_column"
		base.LockMode = "ACCESS EXCLUSIVE"
		base.SQL = "ALTER TABLE " + qtable + " ADD COLUMN " + qi(name) + " " + typ
		base.Handoff = []string{"source:" + op.Column, "transform:" + op.Expression, "ordering:" + strings.Join(op.Ordering.Columns, ","), fmt.Sprintf("batch_size:%d", op.BatchSize)}
		base.Postconditions = []Condition{{Kind: "column_exists", Object: table.Schema + "." + table.Name + "." + name, Expected: typ}}
		return []Step{base}, []Mapping{m}, nil
	default:
		return nil, nil, refuse("unsupported operation %s", op.Kind)
	}
}

func physical(r Request, op zerodowntime.Operation, physicalSchema, kind, logical string, used map[string]string) (string, Mapping, error) {
	key := op.ID + "/" + kind + ":" + logical
	want := physicalName(r.Target, r.Migration.Name, op.ID, kind, logical)
	m := Mapping{OperationID: op.ID, LogicalID: kind + ":" + logical, Schema: physicalSchema, Name: want, Kind: kind}
	if got, ok := r.Snapshot.Mappings[key]; ok {
		if got != m {
			return "", Mapping{}, refuse("stored physical mapping is tampered for %s", key)
		}
	}
	full := m.Schema + "." + m.Name
	if owner, ok := used[full]; ok && owner != op.ID+"/"+m.LogicalID {
		return "", Mapping{}, refuse("physical name collision %s", full)
	}
	used[full] = op.ID + "/" + m.LogicalID
	return want, m, nil
}
func physicalName(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	base := "autosql_" + safe(parts[len(parts)-1])
	suffix := "_" + hex.EncodeToString(h[:8])
	if len(base)+len(suffix) > 63 {
		base = base[:63-len(suffix)]
	}
	return base + suffix
}
func safe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 || b.String()[0] >= '0' && b.String()[0] <= '9' {
		return "x_" + b.String()
	}
	return b.String()
}
func qi(s ...string) string { return pgx.Identifier(s).Sanitize() }
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}
func refuse(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(f, a...)) }
