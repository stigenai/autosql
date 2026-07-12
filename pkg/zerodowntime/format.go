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
	OrderBy    []string      `json:"order_by,omitempty" yaml:"order_by,omitempty"`
	BatchSize  int           `json:"batch_size,omitempty" yaml:"batch_size,omitempty"`
	Unique     bool          `json:"unique,omitempty" yaml:"unique,omitempty"`
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
type PhaseEffect struct{ Expand, Synchronize, Contract, Reverse string }
type Availability string

const (
	Online              Availability = "online"
	Conditional         Availability = "conditional"
	MaintenanceRequired Availability = "maintenance-required"
)

type Capability struct {
	Operation    OperationKind
	Postgres     int
	Availability Availability
	Lock         string
	Rewrite      string
	Reversible   bool
	Notes        string
}

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
var dataType = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_ ]*(?:\([0-9]+(?:,[0-9]+)?\))?$`)
var unsafeExpression = regexp.MustCompile(`(?i)(;|--|/\*|\b(random|clock_timestamp|statement_timestamp|transaction_timestamp|timeofday|nextval|setval|pg_sleep|dblink|lo_import|lo_export)\s*\()`)

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
	cap, ok := capability(op.Kind, pg)
	if !ok {
		return errors.New("unsupported operation or PostgreSQL version")
	}
	_ = cap
	switch op.Kind {
	case AddTable, DropTable:
		if op.Column != "" || op.Expression != "" {
			return errors.New("table operation has incompatible fields")
		}
	case AddColumn, AlterColumnType:
		if op.Column == "" || !dataType.MatchString(op.DataType) {
			return errors.New("column and safe data_type are required")
		}
	case RenameColumn:
		if op.Column == "" || op.NewName == "" {
			return errors.New("column and new_name are required")
		}
	case SetNotNull, DropColumn:
		if op.Column == "" {
			return errors.New("column is required")
		}
	case CreateIndex:
		if op.Index == "" || op.Expression == "" {
			return errors.New("index and expression are required")
		}
	case DropIndex:
		if op.Index == "" {
			return errors.New("index is required")
		}
	default:
		return errors.New("unsupported operation")
	}
	if op.Expression != "" {
		if unsafeExpression.MatchString(op.Expression) {
			return errors.New("expression contains volatile or injection-prone construct")
		}
		if _, err := pg_query.Parse("SELECT " + op.Expression); err != nil {
			return errors.New("expression is not valid PostgreSQL syntax")
		}
	}
	if len(op.OrderBy) > 0 {
		seen := map[string]bool{}
		for _, v := range op.OrderBy {
			if !identifier.MatchString(v) || seen[v] {
				return errors.New("order_by must contain unique safe identifiers")
			}
			seen[v] = true
		}
	}
	if op.BatchSize < 0 {
		return errors.New("batch_size cannot be negative")
	}
	return nil
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
		c.Availability, c.Lock, c.Notes = Online, "SHARE UPDATE EXCLUSIVE", "uses CONCURRENTLY outside a transaction"
	case DropIndex:
		c.Availability, c.Lock, c.Notes = Online, "SHARE UPDATE EXCLUSIVE", "uses CONCURRENTLY outside a transaction"
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
	m.Signature.Value = base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, []byte(signatureDomain+d)))
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
	if err != nil || !ed25519.Verify(key, []byte(signatureDomain+m.Digest), sig) {
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
	var old legacyMigration
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&old); err != nil {
		return Migration{}, invalid("malformed legacy JSON")
	}
	if old.Version != LegacyVersion {
		return Migration{}, invalid("not a supported legacy artifact")
	}
	pg, err := strconv.Atoi(old.MinimumPostgres)
	if err != nil {
		return Migration{}, invalid("legacy minimum_postgres is invalid")
	}
	return New(old.Name, VersionSchema{Name: old.Schema, ExposeDuringExpand: true}, Requirements{MinimumPostgres: pg, LockTimeoutMS: 5000, StatementTimeoutMS: 300000}, old.Operations, map[string]string{"upgraded_from": LegacyVersion})
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
