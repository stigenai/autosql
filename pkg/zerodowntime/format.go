// Package zerodowntime defines the portable, offline-verifiable contract for
// expand/synchronize/contract migrations. It intentionally does not import a
// database driver: an invalid artifact can never cause a target connection.
package zerodowntime

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"gopkg.in/yaml.v3"
)

const Version = "autosql.zero-downtime/v1"
const LegacyVersion = "autosql.zero-downtime/v0"
const signatureDomain = "autosql.zero-downtime.signature/v1\x00"

var ErrInvalid = errors.New("invalid zero-downtime migration")

type OperationKind string

const (
	AddTable        OperationKind = "add_table"
	AddColumn       OperationKind = "add_column"
	RenameColumn    OperationKind = "rename_column"
	AlterColumnType OperationKind = "alter_column_type"
	SetNotNull      OperationKind = "set_not_null"
	DropColumn      OperationKind = "drop_column"
	DropTable       OperationKind = "drop_table"
	CreateIndex     OperationKind = "create_index"
	DropIndex       OperationKind = "drop_index"
)

type Operation struct {
	ID         string        `json:"id" yaml:"id"`
	Kind       OperationKind `json:"kind" yaml:"kind"`
	Table      string        `json:"table" yaml:"table"`
	Column     string        `json:"column,omitempty" yaml:"column,omitempty"`
	NewName    string        `json:"new_name,omitempty" yaml:"new_name,omitempty"`
	DataType   string        `json:"data_type,omitempty" yaml:"data_type,omitempty"`
	Index      string        `json:"index,omitempty" yaml:"index,omitempty"`
	Expression string        `json:"expression,omitempty" yaml:"expression,omitempty"`
	Ordering   *Ordering     `json:"ordering,omitempty" yaml:"ordering,omitempty"`
	BatchSize  int           `json:"batch_size,omitempty" yaml:"batch_size,omitempty"`
	Unique     bool          `json:"unique,omitempty" yaml:"unique,omitempty"`
	IndexMode  *IndexMode    `json:"index_mode,omitempty" yaml:"index_mode,omitempty"`
	Effects    PhaseEffect   `json:"effects" yaml:"effects"`
	Reversal   Reversal      `json:"reversal" yaml:"reversal"`
}

type Ordering struct {
	Columns []string       `json:"columns" yaml:"columns"`
	Unique  UniqueEvidence `json:"unique" yaml:"unique"`
}
type UniqueEvidence struct {
	Kind    string   `json:"kind" yaml:"kind"`
	Name    string   `json:"name" yaml:"name"`
	Columns []string `json:"columns" yaml:"columns"`
}
type IndexMode struct {
	Concurrent      bool `json:"concurrent" yaml:"concurrent"`
	Partitioned     bool `json:"partitioned" yaml:"partitioned"`
	BacksConstraint bool `json:"backs_constraint" yaml:"backs_constraint"`
}
type Reversal struct {
	Mode            string `json:"mode" yaml:"mode"`
	Expression      string `json:"expression,omitempty" yaml:"expression,omitempty"`
	BackupReference string `json:"backup_reference,omitempty" yaml:"backup_reference,omitempty"`
}

type VersionSchema struct {
	Name                string `json:"name" yaml:"name"`
	ExposeDuringExpand  bool   `json:"expose_during_expand" yaml:"expose_during_expand"`
	RetainAfterContract bool   `json:"retain_after_contract" yaml:"retain_after_contract"`
}

type Requirements struct {
	MinimumPostgres    int `json:"minimum_postgres" yaml:"minimum_postgres"`
	LockTimeoutMS      int `json:"lock_timeout_ms" yaml:"lock_timeout_ms"`
	StatementTimeoutMS int `json:"statement_timeout_ms" yaml:"statement_timeout_ms"`
}

type Migration struct {
	Version       string            `json:"version" yaml:"version"`
	Name          string            `json:"name" yaml:"name"`
	VersionSchema VersionSchema     `json:"version_schema" yaml:"version_schema"`
	Requirements  Requirements      `json:"requirements" yaml:"requirements"`
	Operations    []Operation       `json:"operations" yaml:"operations"`
	Metadata      map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Digest        string            `json:"digest" yaml:"digest"`
	Signature     *Signature        `json:"signature,omitempty" yaml:"signature,omitempty"`
}

type Signature struct {
	KeyID     string `json:"key_id" yaml:"key_id"`
	Algorithm string `json:"algorithm" yaml:"algorithm"`
	Value     string `json:"value" yaml:"value"`
}
type PhaseEffect struct {
	Expand      string `json:"expand" yaml:"expand"`
	Synchronize string `json:"synchronize" yaml:"synchronize"`
	Contract    string `json:"contract" yaml:"contract"`
	Reverse     string `json:"reverse" yaml:"reverse"`
}
type Availability string

const (
	Online              Availability = "online"
	Conditional         Availability = "conditional"
	MaintenanceRequired Availability = "maintenance-required"
)

type Capability struct {
	Operation    OperationKind `json:"operation"`
	Postgres     int           `json:"postgres"`
	Availability Availability  `json:"availability"`
	Lock         string        `json:"lock"`
	Rewrite      string        `json:"rewrite"`
	Reversible   bool          `json:"reversible"`
	Notes        string        `json:"notes"`
}

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func New(name string, schema VersionSchema, req Requirements, ops []Operation, metadata map[string]string) (Migration, error) {
	m := Migration{Version: Version, Name: name, VersionSchema: schema, Requirements: req, Operations: append([]Operation(nil), ops...), Metadata: clone(metadata)}
	d, err := digest(m)
	m.Digest = d
	if err != nil {
		return Migration{}, err
	}
	if err := m.Validate(); err != nil {
		return Migration{}, err
	}
	return m, nil
}

func (m Migration) Validate() error {
	if m.Version != Version {
		return invalid("unsupported version")
	}
	if !identifier.MatchString(m.Name) || !identifier.MatchString(m.VersionSchema.Name) {
		return invalid("name and version_schema.name must be safe identifiers")
	}
	if m.Requirements.MinimumPostgres < 14 || m.Requirements.MinimumPostgres > 18 {
		return invalid("minimum_postgres must be between 14 and 18")
	}
	if m.Requirements.LockTimeoutMS <= 0 || m.Requirements.StatementTimeoutMS <= 0 {
		return invalid("timeouts must be positive")
	}
	if len(m.Operations) == 0 {
		return invalid("operations must not be empty")
	}
	seen := map[string]bool{}
	previous := ""
	for i, op := range m.Operations {
		if op.ID == "" || seen[op.ID] {
			return invalid(fmt.Sprintf("operations[%d].id is empty or duplicated", i))
		}
		if previous != "" && op.ID <= previous {
			return invalid("operations must be sorted by unique id")
		}
		seen[op.ID], previous = true, op.ID
		if err := validateOperation(op, m.Requirements.MinimumPostgres); err != nil {
			return invalid(fmt.Sprintf("operation %q: %v", op.ID, err))
		}
	}
	for k := range m.Metadata {
		if !identifier.MatchString(k) {
			return invalid("metadata contains unsafe key")
		}
	}
	want, err := digest(m)
	if err != nil || m.Digest != want {
		return invalid("digest mismatch")
	}
	if m.Signature != nil && (m.Signature.KeyID == "" || m.Signature.Algorithm != "Ed25519" || m.Signature.Value == "") {
		return invalid("incomplete signature")
	}
	return nil
}

// ValidateForPostgres performs the only target-dependent compatibility check
// after offline validation has completed. Callers should invoke Validate first,
// then discover the server version, then call this method before execution.
func (m Migration) ValidateForPostgres(serverMajor int) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if serverMajor < m.Requirements.MinimumPostgres {
		return invalid(fmt.Sprintf("target PostgreSQL %d is older than required PostgreSQL %d", serverMajor, m.Requirements.MinimumPostgres))
	}
	if serverMajor < 14 || serverMajor > 18 {
		return invalid("target PostgreSQL version is outside the tested capability matrix (14-18)")
	}
	return nil
}

func validateOperation(op Operation, pg int) error {
	if !identifier.MatchString(op.Table) || op.Column != "" && !identifier.MatchString(op.Column) || op.NewName != "" && !identifier.MatchString(op.NewName) || op.Index != "" && !identifier.MatchString(op.Index) {
		return errors.New("object names must be safe identifiers")
	}
	_, ok := capability(op.Kind, pg)
	if !ok {
		return errors.New("unsupported operation or PostgreSQL version")
	}
	expected, _ := Effects(op.Kind)
	if op.Effects != expected {
		return errors.New("signed phase effects do not match operation capability")
	}
	if err := validateReversal(op); err != nil {
		return err
	}
	switch op.Kind {
	case AddTable:
		if hasColumnFields(op) || op.Index != "" || op.Ordering != nil || op.BatchSize != 0 || op.Unique || op.IndexMode != nil {
			return errors.New("table operation has incompatible fields")
		}
	case DropTable:
		if hasColumnFields(op) || op.Index != "" || op.Ordering != nil || op.BatchSize != 0 || op.Unique || op.IndexMode != nil {
			return errors.New("table operation has incompatible fields")
		}
	case AddColumn:
		if op.Column == "" || validateDataType(op.DataType) != nil {
			return errors.New("column and safe data_type are required")
		}
		if op.NewName != "" || op.Index != "" || op.Unique || op.IndexMode != nil {
			return errors.New("add_column has incompatible fields")
		}
		if op.Expression != "" {
			if err := validateBackfill(op); err != nil {
				return err
			}
		} else if op.Ordering != nil || op.BatchSize != 0 {
			return errors.New("ordering and batch_size require an expression")
		}
	case AlterColumnType:
		if op.Column == "" || validateDataType(op.DataType) != nil || op.Expression == "" {
			return errors.New("column, safe data_type, and transform expression are required")
		}
		if op.NewName != "" || op.Index != "" || op.Unique || op.IndexMode != nil {
			return errors.New("alter_column_type has incompatible fields")
		}
		if err := validateBackfill(op); err != nil {
			return err
		}
	case RenameColumn:
		if op.Column == "" || op.NewName == "" || op.Expression == "" {
			return errors.New("column, new_name, and source transform are required")
		}
		if op.DataType != "" || op.Index != "" || op.Unique || op.IndexMode != nil {
			return errors.New("rename_column has incompatible fields")
		}
		if err := validateBackfill(op); err != nil {
			return err
		}
	case SetNotNull:
		if op.Column == "" || op.Expression == "" {
			return errors.New("set_not_null requires column and fill expression")
		}
		if op.NewName != "" || op.DataType != "" || op.Index != "" || op.Unique || op.IndexMode != nil {
			return errors.New("set_not_null has incompatible fields")
		}
		if err := validateBackfill(op); err != nil {
			return err
		}
	case DropColumn:
		if op.Column == "" {
			return errors.New("column is required")
		}
		if op.NewName != "" || op.DataType != "" || op.Index != "" || op.Expression != "" || op.Ordering != nil || op.BatchSize != 0 || op.Unique || op.IndexMode != nil {
			return errors.New("column operation has incompatible fields")
		}
	case CreateIndex:
		if op.Index == "" || op.Expression == "" {
			return errors.New("index and expression are required")
		}
		if op.Column != "" || op.NewName != "" || op.DataType != "" || op.Ordering != nil || op.BatchSize != 0 {
			return errors.New("create_index has incompatible fields")
		}
		if op.IndexMode == nil || !op.IndexMode.Concurrent {
			return errors.New("create_index requires explicit concurrent index mode")
		}
		if op.IndexMode.Partitioned || op.IndexMode.BacksConstraint {
			return errors.New("concurrent index operation does not support partitioned indexes or constraint backing")
		}
	case DropIndex:
		if op.Index == "" {
			return errors.New("index is required")
		}
		if op.Column != "" || op.NewName != "" || op.DataType != "" || op.Expression != "" || op.Ordering != nil || op.BatchSize != 0 || op.Unique {
			return errors.New("drop_index has incompatible fields")
		}
		if op.IndexMode == nil || !op.IndexMode.Concurrent {
			return errors.New("drop_index requires explicit concurrent index mode")
		}
		if op.IndexMode.Partitioned || op.IndexMode.BacksConstraint {
			return errors.New("concurrent index operation does not support partitioned indexes or constraint backing")
		}
	default:
		return errors.New("unsupported operation")
	}
	if op.Expression != "" {
		if err := validateExpression(op.Expression); err != nil {
			return err
		}
	}
	if op.Reversal.Expression != "" {
		if err := validateExpression(op.Reversal.Expression); err != nil {
			return fmt.Errorf("reverse expression: %w", err)
		}
	}
	return nil
}

func hasColumnFields(op Operation) bool {
	return op.Column != "" || op.NewName != "" || op.DataType != "" || op.Expression != ""
}
func validateBackfill(op Operation) error {
	if op.Ordering == nil || len(op.Ordering.Columns) == 0 {
		return errors.New("backfill requires deterministic unique ordering evidence")
	}
	if op.BatchSize <= 0 {
		return errors.New("backfill requires a positive batch_size")
	}
	seen := map[string]bool{}
	for _, v := range op.Ordering.Columns {
		if !identifier.MatchString(v) || seen[v] {
			return errors.New("ordering columns must be unique safe identifiers")
		}
		seen[v] = true
	}
	e := op.Ordering.Unique
	if (e.Kind != "constraint" && e.Kind != "unique_index") || !identifier.MatchString(e.Name) || len(e.Columns) != len(op.Ordering.Columns) {
		return errors.New("unique evidence must identify a constraint or unique index over the exact ordering")
	}
	for i, c := range e.Columns {
		if c != op.Ordering.Columns[i] {
			return errors.New("unique evidence columns must exactly match ordering")
		}
	}
	return nil
}
func validateReversal(op Operation) error {
	switch op.Kind {
	case DropColumn, DropTable:
		if op.Reversal.Mode != "backup" || op.Reversal.BackupReference == "" || op.Reversal.Expression != "" {
			return errors.New("destructive operation requires backup reversal with reference")
		}
	case AlterColumnType:
		if op.Reversal.Mode != "lossless" || op.Reversal.Expression == "" || op.Reversal.BackupReference != "" {
			return errors.New("alter_column_type requires explicit lossless reverse expression")
		}
	default:
		if op.Reversal.Mode != "automatic" || op.Reversal.Expression != "" || op.Reversal.BackupReference != "" {
			return errors.New("operation requires automatic reversal without extra fields")
		}
	}
	return nil
}

var allowedNodes = map[string]bool{"SelectStmt": true, "ResTarget": true, "ColumnRef": true, "String": true, "A_Const": true, "Integer": true, "Float": true, "Boolean": true, "CoalesceExpr": true, "FuncCall": true, "TypeCast": true, "TypeName": true, "A_Expr": true, "BoolExpr": true, "NullTest": true, "CaseExpr": true, "CaseWhen": true}
var allowedFunctions = map[string]bool{"coalesce": true, "lower": true, "upper": true, "trim": true, "btrim": true, "ltrim": true, "rtrim": true, "abs": true, "round": true, "length": true, "substring": true}
var allowedCastTypes = map[string]bool{"text": true, "int2": true, "int4": true, "int8": true, "numeric": true, "bool": true, "date": true, "timestamp": true, "timestamptz": true, "uuid": true}

func validateExpression(expression string) error {
	if strings.Contains(expression, "/*") || strings.Contains(expression, "--") || strings.Contains(expression, "\"") {
		return errors.New("expression comments and quoted identifiers are forbidden")
	}
	parsed, err := pg_query.Parse("SELECT " + expression)
	if err != nil || len(parsed.Stmts) != 1 {
		return errors.New("expression is not a single valid PostgreSQL expression")
	}
	raw, err := pg_query.ParseToJSON("SELECT " + expression)
	if err != nil {
		return errors.New("expression AST unavailable")
	}
	var tree any
	if json.Unmarshal([]byte(raw), &tree) != nil {
		return errors.New("expression AST invalid")
	}
	if err := walkAST(tree); err != nil {
		return err
	}
	return nil
}
func walkAST(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if len(k) > 0 && k[0] >= 'A' && k[0] <= 'Z' && !allowedNodes[k] {
				return errors.New("expression contains an unapproved AST node")
			}
			if err := walkAST(v); err != nil {
				return err
			}
			if node, ok := v.(map[string]any); ok {
				switch k {
				case "FuncCall":
					names := astNames(node["funcname"])
					if len(names) != 1 || !allowedFunctions[names[0]] {
						return errors.New("expression function must be an unqualified immutable allowlisted name")
					}
				case "TypeCast":
					typeNode, _ := node["typeName"].(map[string]any)
					names := astNames(typeNode["names"])
					if len(names) == 2 && names[0] == "pg_catalog" && names[1] == "numeric" {
						names = names[1:]
					}
					if len(names) != 1 || !allowedCastTypes[names[0]] {
						return errors.New("expression cast must use an unqualified approved type")
					}
				case "A_Expr":
					names := astNames(node["name"])
					if len(names) != 1 || !allowedOperators[names[0]] {
						return errors.New("expression operator must be unqualified and approved")
					}
				case "ColumnRef":
					if len(astNames(node["fields"])) != 1 {
						return errors.New("expression column must be unqualified")
					}
				}
			}
		}
	case []any:
		for _, v := range x {
			if err := walkAST(v); err != nil {
				return err
			}
		}
	}
	return nil
}

var allowedOperators = map[string]bool{"+": true, "-": true, "*": true, "/": true, "=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true, "||": true}

func astNames(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		snode, ok := m["String"].(map[string]any)
		if !ok {
			return nil
		}
		s, ok := snode["sval"].(string)
		if !ok {
			return nil
		}
		out = append(out, strings.ToLower(s))
	}
	return out
}

func validateDataType(value string) error {
	if value == "" || strings.ContainsAny(value, ";\".") || strings.Contains(value, "/*") || strings.Contains(value, "--") {
		return errors.New("data_type contains forbidden syntax")
	}
	raw, err := pg_query.ParseToJSON("CREATE TABLE autosql_type_probe (value " + value + ")")
	if err != nil {
		return errors.New("data_type is not valid PostgreSQL type syntax")
	}
	var tree any
	if json.Unmarshal([]byte(raw), &tree) != nil {
		return errors.New("data_type AST invalid")
	}
	defs := findASTNodes(tree, "ColumnDef")
	if len(defs) != 1 {
		return errors.New("data_type did not produce exactly one column")
	}
	def := defs[0]
	if _, ok := def["constraints"]; ok {
		return errors.New("data_type must not contain column modifiers or constraints")
	}
	tn, ok := def["typeName"].(map[string]any)
	if !ok {
		return errors.New("data_type AST missing type")
	}
	names := astNames(tn["names"])
	if len(names) == 2 && names[0] == "pg_catalog" {
		names = names[1:]
	}
	if len(names) != 1 || !allowedCastTypes[names[0]] {
		return errors.New("data_type is not in approved type allowlist")
	}
	return nil
}
func findASTNodes(v any, name string) []map[string]any {
	var out []map[string]any
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if k == name {
					if m, ok := v.(map[string]any); ok {
						out = append(out, m)
					}
				}
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(v)
	return out
}

func Effects(kind OperationKind) (PhaseEffect, bool) {
	effects := map[OperationKind]PhaseEffect{
		AddTable:        {"create shadow-compatible table", "no data synchronization", "publish table", "drop table if unused"},
		AddColumn:       {"add nullable destination column", "backfill expression in stable batches and dual-write", "enforce final default/constraints", "remove destination column"},
		RenameColumn:    {"add destination column and compatibility view", "backfill and dual-write both names", "remove old column", "restore old name from destination"},
		AlterColumnType: {"add typed destination column", "transform, backfill, and dual-write", "swap compatibility surface and remove source", "reverse transform only when declared lossless"},
		SetNotNull:      {"install not-valid check", "backfill null values and validate check", "set not null and remove check", "drop not-null constraint"},
		DropColumn:      {"hide column from version schema", "stop writes and verify no readers", "drop column", "restore only from retained backup"},
		DropTable:       {"hide table from version schema", "stop writes and verify no readers", "drop table", "restore only from retained backup"},
		CreateIndex:     {"create index concurrently", "wait for valid index", "publish indexed path", "drop index concurrently"},
		DropIndex:       {"remove index from version schema", "verify no required query path", "drop index concurrently", "recreate index concurrently"},
	}
	e, ok := effects[kind]
	return e, ok
}

func capability(kind OperationKind, pg int) (Capability, bool) {
	if pg < 14 || pg > 18 {
		return Capability{}, false
	}
	c := Capability{Operation: kind, Postgres: pg, Lock: "brief ACCESS EXCLUSIVE", Rewrite: "none", Reversible: true}
	switch kind {
	case AddTable:
		c.Availability, c.Notes = Online, "metadata-only creation"
	case AddColumn:
		c.Availability, c.Notes = Online, "nullable expansion; backfill is batched"
	case RenameColumn:
		c.Availability, c.Notes = Conditional, "requires compatibility surface and dual writes"
	case AlterColumnType:
		c.Availability, c.Rewrite, c.Notes = Conditional, "shadow-column backfill", "transform must be immutable and reversible"
	case SetNotNull:
		c.Availability, c.Notes = Conditional, "NOT VALID check avoids validation lock; final lock is bounded"
	case CreateIndex:
		c.Availability, c.Lock, c.Notes = Conditional, "SHARE UPDATE EXCLUSIVE", "CONCURRENTLY outside a transaction; excludes partitioned and constraint-backing indexes"
	case DropIndex:
		c.Availability, c.Lock, c.Notes = Conditional, "SHARE UPDATE EXCLUSIVE", "CONCURRENTLY outside a transaction; excludes partitioned and constraint-backing indexes"
	case DropColumn, DropTable:
		c.Availability, c.Reversible, c.Notes = MaintenanceRequired, false, "destructive contract requires explicit maintenance approval and backup"
	default:
		return Capability{}, false
	}
	return c, true
}

func CapabilityMatrix() []Capability {
	var out []Capability
	kinds := []OperationKind{AddTable, AddColumn, RenameColumn, AlterColumnType, SetNotNull, CreateIndex, DropIndex, DropColumn, DropTable}
	for pg := 14; pg <= 18; pg++ {
		for _, k := range kinds {
			c, _ := capability(k, pg)
			out = append(out, c)
		}
	}
	return out
}

func (m Migration) MarshalJSONCanonical() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	return append(b, '\n'), err
}
func (m Migration) MarshalYAMLCanonical() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(m)
}

func ParseJSON(data []byte) (Migration, error) {
	if err := rejectDuplicateJSON(data); err != nil {
		return Migration{}, err
	}
	var m Migration
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, invalid("malformed JSON")
	}
	if err := ensureEOF(d); err != nil {
		return m, err
	}
	return m, m.Validate()
}
func ParseYAML(data []byte) (Migration, error) {
	if err := validateYAMLNode(data); err != nil {
		return Migration{}, err
	}
	var m Migration
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&m); err != nil {
		return m, invalid("malformed YAML")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return m, invalid("multiple YAML documents")
	}
	return m, m.Validate()
}

func (m *Migration) Sign(keyID string, key ed25519.PrivateKey) error {
	if len(key) != ed25519.PrivateKeySize || keyID == "" {
		return invalid("invalid signing key")
	}
	m.Signature = &Signature{KeyID: keyID, Algorithm: "Ed25519"}
	d, err := digest(*m)
	if err != nil {
		return err
	}
	m.Digest = d
	m.Signature.Value = base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, signaturePayload(*m.Signature, d)))
	return m.Validate()
}
func (m Migration) Verify(key ed25519.PublicKey) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Signature == nil || len(key) != ed25519.PublicKeySize {
		return invalid("signature is required")
	}
	sig, err := base64.RawStdEncoding.Strict().DecodeString(m.Signature.Value)
	if err != nil || !ed25519.Verify(key, signaturePayload(*m.Signature, m.Digest), sig) {
		return invalid("signature verification failed")
	}
	return nil
}

type legacyMigration struct {
	Version         string      `json:"version" yaml:"version"`
	Name            string      `json:"name" yaml:"name"`
	Schema          string      `json:"schema" yaml:"schema"`
	MinimumPostgres string      `json:"minimum_postgres" yaml:"minimum_postgres"`
	Operations      []Operation `json:"operations" yaml:"operations"`
}

func UpgradeLegacyJSON(data []byte) (Migration, error) {
	if err := rejectDuplicateJSON(data); err != nil {
		return Migration{}, err
	}
	var old legacyMigration
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&old); err != nil {
		return Migration{}, invalid("malformed legacy JSON")
	}
	if err := ensureEOF(d); err != nil {
		return Migration{}, err
	}
	if old.Version != LegacyVersion {
		return Migration{}, invalid("not a supported legacy artifact")
	}
	pg, err := strconv.Atoi(old.MinimumPostgres)
	if err != nil {
		return Migration{}, invalid("legacy minimum_postgres is invalid")
	}
	for i := range old.Operations {
		e, ok := Effects(old.Operations[i].Kind)
		if !ok {
			return Migration{}, invalid("legacy operation unsupported")
		}
		old.Operations[i].Effects = e
		switch old.Operations[i].Kind {
		case AlterColumnType, DropColumn, DropTable, CreateIndex, DropIndex:
			return Migration{}, invalid("legacy operation requires explicit v1 semantics and cannot be silently upgraded")
		default:
			old.Operations[i].Reversal = Reversal{Mode: "automatic"}
		}
	}
	return New(old.Name, VersionSchema{Name: old.Schema, ExposeDuringExpand: true}, Requirements{MinimumPostgres: pg, LockTimeoutMS: 5000, StatementTimeoutMS: 300000}, old.Operations, map[string]string{"upgraded_from": LegacyVersion})
}

func signaturePayload(s Signature, digest string) []byte {
	return []byte(signatureDomain + s.Algorithm + "\x00" + s.KeyID + "\x00" + digest)
}

func rejectDuplicateJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		tok, err := d.Token()
		if err != nil {
			return invalid("malformed JSON")
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return invalid("malformed JSON")
					}
					key, ok := k.(string)
					if !ok || seen[key] {
						return invalid("duplicate JSON object key")
					}
					seen[key] = true
					if err := walk(); err != nil {
						return err
					}
				}
				_, err = d.Token()
				return err
			case '[':
				for d.More() {
					if err := walk(); err != nil {
						return err
					}
				}
				_, err = d.Token()
				return err
			}
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid("trailing JSON content")
	}
	return nil
}
func validateYAMLNode(data []byte) error {
	var root yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&root); err != nil {
		return invalid("malformed YAML")
	}
	var walk func(*yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode || n.Anchor != "" {
			return invalid("YAML aliases and anchors are forbidden")
		}
		if n.Tag != "" && n.Tag != "!!map" && n.Tag != "!!seq" && n.Tag != "!!str" && n.Tag != "!!int" && n.Tag != "!!bool" && n.Tag != "!!null" {
			return invalid("custom YAML tags are forbidden")
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || seen[k.Value] {
					return invalid("duplicate or non-scalar YAML key")
				}
				seen[k.Value] = true
			}
		}
		for _, c := range n.Content {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(&root); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return invalid("multiple YAML documents")
	}
	return nil
}

func digest(m Migration) (string, error) {
	m.Digest = ""
	m.Signature = nil
	m.Metadata = clone(m.Metadata)
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func clone(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
func invalid(detail string) error { return fmt.Errorf("%w: %s", ErrInvalid, detail) }
func ensureEOF(d *json.Decoder) error {
	var v any
	if err := d.Decode(&v); err != io.EOF {
		return invalid("trailing JSON content")
	}
	return nil
}

// SortedOperations returns a stable copy useful to authors before New. New
// deliberately rejects unstable order so signing never changes semantics.
func SortedOperations(in []Operation) []Operation {
	out := append([]Operation(nil), in...)
	sort.Slice(out, func(i, j int) bool { return strings.Compare(out[i].ID, out[j].ID) < 0 })
	return out
}
