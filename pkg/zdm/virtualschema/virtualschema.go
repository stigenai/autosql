// Package virtualschema installs application-facing version schemas backed by
// simple PostgreSQL views. Simple views remain natively updatable, preserving
// PostgreSQL INSERT/UPDATE/DELETE, DEFAULT and RETURNING semantics.
package virtualschema

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

const Version = "autosql.zdm.virtual-schema/v1"

var (
	ErrInvalid   = errors.New("invalid virtual schema specification")
	ErrCollision = errors.New("virtual schema collision")
	identifier   = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

type Config struct {
	URL, Target, Environment string
	LockTimeoutMS            int
}

type Spec struct {
	Version        string        `json:"version"`
	ArtifactDigest string        `json:"artifact_digest"`
	PhysicalSchema string        `json:"physical_schema"`
	Previous       SchemaVersion `json:"previous"`
	Current        SchemaVersion `json:"current"`
	Digest         string        `json:"digest"`
}

type SchemaVersion struct {
	Name   string      `json:"name"`
	Tables []TableView `json:"tables"`
}

type TableView struct {
	Name          string       `json:"name"`
	PhysicalTable string       `json:"physical_table"`
	Columns       []ColumnView `json:"columns"`
	Comment       string       `json:"comment,omitempty"`
}

type ColumnView struct {
	Name           string `json:"name"`
	PhysicalColumn string `json:"physical_column"`
}

type Diagnostic struct {
	Code, Object, Message string
}

type Status struct {
	SpecDigest  string       `json:"spec_digest"`
	Previous    SchemaStatus `json:"previous"`
	Current     SchemaStatus `json:"current"`
	Connection  Connections  `json:"connection"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

type SchemaStatus struct {
	Name, Comment string
	Exists, Exact bool
	Views         []string
}

type Connections struct {
	PreviousSearchPath string `json:"previous_search_path"`
	CurrentSearchPath  string `json:"current_search_path"`
}

func New(artifactDigest, physical string, previous, current SchemaVersion) (Spec, error) {
	s := Spec{Version: Version, ArtifactDigest: artifactDigest, PhysicalSchema: physical, Previous: previous, Current: current}
	d, err := digest(s)
	if err != nil {
		return Spec{}, err
	}
	s.Digest = d
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

func ParseJSON(b []byte) (Spec, error) {
	if err := rejectDuplicateKeys(b); err != nil {
		return Spec{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var s Spec
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return Spec{}, fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

func rejectDuplicateKeys(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func() error
	value = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate object key %q", key)
				}
				seen[key] = true
				if err = value(); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("unterminated object")
			}
		case '[':
			for d.More() {
				if err = value(); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("unterminated array")
			}
		default:
			return errors.New("unexpected delimiter")
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func (s Spec) MarshalJSONCanonical() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func (s Spec) Validate() error {
	if s.Version != Version || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.ArtifactDigest) || !identifier.MatchString(s.PhysicalSchema) {
		return fmt.Errorf("%w: version or physical schema", ErrInvalid)
	}
	if s.Previous.Name == s.Current.Name || !identifier.MatchString(s.Previous.Name) || !identifier.MatchString(s.Current.Name) {
		return fmt.Errorf("%w: distinct safe version schema names required", ErrInvalid)
	}
	for _, v := range []SchemaVersion{s.Previous, s.Current} {
		if len(v.Tables) == 0 {
			return fmt.Errorf("%w: %s has no tables", ErrInvalid, v.Name)
		}
		last := ""
		for _, t := range v.Tables {
			if !identifier.MatchString(t.Name) || !identifier.MatchString(t.PhysicalTable) || t.Name <= last || len(t.Columns) == 0 {
				return fmt.Errorf("%w: tables must be safely named, nonempty, and sorted", ErrInvalid)
			}
			last = t.Name
			clast := ""
			for _, c := range t.Columns {
				if !identifier.MatchString(c.Name) || !identifier.MatchString(c.PhysicalColumn) || c.Name <= clast {
					return fmt.Errorf("%w: columns must be safely named and sorted", ErrInvalid)
				}
				clast = c.Name
			}
		}
	}
	d, err := digest(s)
	if err != nil || d != s.Digest {
		return fmt.Errorf("%w: digest mismatch", ErrInvalid)
	}
	return nil
}

func digest(s Spec) (string, error) {
	s.Digest = ""
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func q(parts ...string) string { return pgx.Identifier(parts).Sanitize() }
func lit(s string) string      { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func scopeID(target, environment string) (string, error) {
	if target == "" || environment == "" || len(target) > 4096 || len(environment) > 4096 {
		return "", fmt.Errorf("%w: invalid target/environment", ErrInvalid)
	}
	b, err := json.Marshal(struct{ Version, Target, Environment string }{"autosql.zdm.virtual-schema.scope/v1", target, environment})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// Apply installs both versions atomically. Existing non-owned objects are
// refused; an exact prior installation is idempotent.
func Apply(ctx context.Context, cfg Config, spec Spec) (Status, error) {
	if err := spec.Validate(); err != nil {
		return Status{}, err
	}
	if cfg.URL == "" || cfg.Target == "" || cfg.Environment == "" || cfg.LockTimeoutMS <= 0 {
		return Status{}, fmt.Errorf("%w: URL, target, environment and positive lock timeout required", ErrInvalid)
	}
	scope, err := scopeID(cfg.Target, cfg.Environment)
	if err != nil {
		return Status{}, err
	}
	c, err := pgx.Connect(ctx, cfg.URL)
	if err != nil {
		return Status{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	tx, err := c.Begin(ctx)
	if err != nil {
		return Status{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, fmt.Sprintf("set local lock_timeout=%s", lit(fmt.Sprintf("%dms", cfg.LockTimeoutMS)))); err != nil {
		return Status{}, err
	}
	domain := "autosql.zdm.virtual-schema/v1/" + scope
	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1,0::bigint))`, domain); err != nil {
		return Status{}, err
	}
	for _, v := range []SchemaVersion{spec.Previous, spec.Current} {
		if err = installVersion(ctx, tx, spec, v, scope); err != nil {
			return Status{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Status{}, err
	}
	return Inspect(ctx, cfg, spec)
}

func installVersion(ctx context.Context, tx pgx.Tx, spec Spec, v SchemaVersion, scope string) error {
	comment := "autosql:zdm:virtual-schema:" + scope + ":" + spec.Digest + ":" + v.Name
	var exists bool
	var current *string
	if err := tx.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1), obj_description(to_regnamespace($1),'pg_namespace')`, v.Name).Scan(&exists, &current); err != nil {
		return err
	}
	if exists && (current == nil || *current != comment) {
		return fmt.Errorf("%w: schema %s already exists without matching ownership marker", ErrCollision, v.Name)
	}
	if !exists {
		if _, err := tx.Exec(ctx, "create schema "+q(v.Name)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "comment on schema "+q(v.Name)+" is "+lit(comment)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "revoke create on schema "+q(v.Name)+" from public"); err != nil {
		return err
	}
	for _, t := range v.Tables {
		viewSQL := expectedViewSQL(spec.PhysicalSchema, t)
		marker := "autosql:zdm:view:" + scope + ":" + spec.Digest + ":" + t.Comment
		var kind, existingComment *string
		_ = tx.QueryRow(ctx, `select c.relkind::text,obj_description(c.oid,'pg_class') from pg_class c join pg_namespace n on n.oid=c.relnamespace where n.nspname=$1 and c.relname=$2`, v.Name, t.Name).Scan(&kind, &existingComment)
		if kind != nil && *kind != "v" {
			return fmt.Errorf("%w: %s.%s is not a view", ErrCollision, v.Name, t.Name)
		}
		if kind != nil && (existingComment == nil || *existingComment != marker) {
			return fmt.Errorf("%w: view %s.%s lacks the exact ownership marker", ErrCollision, v.Name, t.Name)
		}
		if _, err := tx.Exec(ctx, "create or replace view "+q(v.Name, t.Name)+" as "+viewSQL); err != nil {
			return fmt.Errorf("install view %s.%s: %w", v.Name, t.Name, err)
		}
		if _, err := tx.Exec(ctx, "comment on view "+q(v.Name, t.Name)+" is "+lit(marker)); err != nil {
			return err
		}
	}
	return nil
}

func expectedViewSQL(physicalSchema string, t TableView) string {
	var cols []string
	for _, c := range t.Columns {
		cols = append(cols, q(c.PhysicalColumn)+" as "+q(c.Name))
	}
	return "select " + strings.Join(cols, ",") + " from " + q(physicalSchema, t.PhysicalTable)
}

func Inspect(ctx context.Context, cfg Config, spec Spec) (Status, error) {
	if err := spec.Validate(); err != nil {
		return Status{}, err
	}
	scope, err := scopeID(cfg.Target, cfg.Environment)
	if err != nil {
		return Status{}, err
	}
	c, err := pgx.Connect(ctx, cfg.URL)
	if err != nil {
		return Status{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	st := Status{SpecDigest: spec.Digest, Connection: Connections{PreviousSearchPath: q(spec.Previous.Name) + ", pg_catalog", CurrentSearchPath: q(spec.Current.Name) + ", pg_catalog"}}
	st.Previous, err = inspectVersion(ctx, c, spec, spec.Previous, scope)
	if err != nil {
		return Status{}, err
	}
	st.Current, err = inspectVersion(ctx, c, spec, spec.Current, scope)
	if err != nil {
		return Status{}, err
	}
	if !st.Previous.Exact || !st.Current.Exact {
		st.Diagnostics = append(st.Diagnostics, Diagnostic{Code: "virtual_schema_drift", Object: spec.Previous.Name + "," + spec.Current.Name, Message: "run virtual-schema apply only after reviewing collisions and drift"})
	}
	st.Diagnostics = append(st.Diagnostics, Diagnostic{Code: "grants_ownership_review", Object: spec.Previous.Name + "," + spec.Current.Name, Message: "version views are owned by the installer and PostgreSQL does not copy base-table grants or comments; review and grant schema USAGE plus view DML privileges explicitly"})
	return st, nil
}

func inspectVersion(ctx context.Context, c *pgx.Conn, spec Spec, v SchemaVersion, scope string) (SchemaStatus, error) {
	r := SchemaStatus{Name: v.Name}
	var comment *string
	err := c.QueryRow(ctx, `select obj_description(to_regnamespace($1),'pg_namespace')`, v.Name).Scan(&comment)
	if err != nil {
		return r, err
	}
	if comment == nil {
		return r, nil
	}
	r.Exists = true
	r.Comment = *comment
	rows, err := c.Query(ctx, `select c.relname from pg_class c join pg_namespace n on n.oid=c.relnamespace where n.nspname=$1 and c.relkind='v' order by c.relname`, v.Name)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var x string
		if err = rows.Scan(&x); err != nil {
			return r, err
		}
		r.Views = append(r.Views, x)
	}
	expected := make([]string, len(v.Tables))
	for i, t := range v.Tables {
		expected[i] = t.Name
	}
	sort.Strings(expected)
	r.Exact = *comment == "autosql:zdm:virtual-schema:"+scope+":"+spec.Digest+":"+v.Name && strings.Join(expected, "\x00") == strings.Join(r.Views, "\x00")
	for _, t := range v.Tables {
		var viewComment *string
		var def string
		if err := c.QueryRow(ctx, `select obj_description(c.oid,'pg_class'),pg_get_viewdef(c.oid,false) from pg_class c join pg_namespace n on n.oid=c.relnamespace where n.nspname=$1 and c.relname=$2 and c.relkind='v'`, v.Name, t.Name).Scan(&viewComment, &def); err != nil {
			r.Exact = false
			continue
		}
		if viewComment == nil || *viewComment != "autosql:zdm:view:"+scope+":"+spec.Digest+":"+t.Comment {
			r.Exact = false
		}
		wantFP, e1 := semanticViewFingerprint(expectedViewSQL(spec.PhysicalSchema, t))
		gotFP, e2 := semanticViewFingerprint(def)
		if e1 != nil || e2 != nil || wantFP != gotFP {
			r.Exact = false
		}
	}
	return r, rows.Err()
}

// PostgreSQL 14 qualifies simple-view column references in pg_get_viewdef,
// while newer releases may omit those redundant qualifiers. Normalize only
// the AST differences that are semantically irrelevant for our deliberately
// simple one-relation views; aliases that rename a column remain significant.
func semanticViewFingerprint(sql string) (string, error) {
	raw, err := pg_query.ParseToJSON(sql)
	if err != nil {
		return "", err
	}
	var tree any
	if err = json.Unmarshal([]byte(raw), &tree); err != nil {
		return "", err
	}
	fieldName := func(v any) string {
		m, _ := v.(map[string]any)
		s, _ := m["String"].(map[string]any)
		x, _ := s["sval"].(string)
		return x
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			delete(x, "location")
			delete(x, "stmt_len")
			if c, ok := x["ColumnRef"].(map[string]any); ok {
				if f, ok := c["fields"].([]any); ok && len(f) > 1 {
					c["fields"] = f[len(f)-1:]
				}
			}
			if r, ok := x["ResTarget"].(map[string]any); ok {
				name, has := r["name"].(string)
				if !has || name == "" {
					if val, ok := r["val"].(map[string]any); ok {
						if c, ok := val["ColumnRef"].(map[string]any); ok {
							if f, ok := c["fields"].([]any); ok && len(f) > 0 {
								r["name"] = fieldName(f[len(f)-1])
							}
						}
					}
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(tree)
	b, err := json.Marshal(tree)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
