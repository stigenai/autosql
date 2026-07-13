// Package shadowsync installs deterministic, non-recursive PostgreSQL triggers
// that keep old and shadow physical columns synchronized during expand/contract.
package shadowsync

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
)

const Version = "autosql.zdm.shadow-sync/v1"

var ErrInvalid = errors.New("invalid shadow synchronization specification")
var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
var cast = regexp.MustCompile(`^value::([a-z_][a-z0-9_]*)$`)

type Config struct {
	URL, Target, Environment string
	LockTimeoutMS            int
}
type Policy struct{ AllowLossy, AllowNonReversible bool }
type Spec struct {
	Version        string  `json:"version"`
	ArtifactDigest string  `json:"artifact_digest"`
	PhysicalSchema string  `json:"physical_schema"`
	Digest         string  `json:"digest"`
	Tables         []Table `json:"tables"`
}
type Table struct {
	Name  string `json:"name"`
	Pairs []Pair `json:"pairs"`
}
type Pair struct {
	ID        string `json:"id"`
	OldColumn string `json:"old_column"`
	NewColumn string `json:"new_column"`
	Forward   string `json:"forward"`
	Reverse   string `json:"reverse,omitempty"`
	Lossy     bool   `json:"lossy"`
}
type Status struct {
	Digest           string        `json:"digest"`
	Installed        bool          `json:"installed"`
	RollbackEligible bool          `json:"rollback_eligible"`
	Tables           []TableStatus `json:"tables"`
}
type TableStatus struct {
	Table, Trigger, Function   string
	Installed, Exact           bool
	Pairs, AssignmentsPerWrite int
}

func New(artifactDigest, schema string, tables []Table) (Spec, error) {
	s := Spec{Version: Version, ArtifactDigest: artifactDigest, PhysicalSchema: schema, Tables: append([]Table(nil), tables...)}
	d, e := digest(s)
	if e != nil {
		return Spec{}, e
	}
	s.Digest = d
	if e = s.Validate(Policy{AllowLossy: true, AllowNonReversible: true}); e != nil {
		return Spec{}, e
	}
	return s, nil
}
func (s Spec) Validate(p Policy) error {
	if s.Version != Version || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.ArtifactDigest) || !ident.MatchString(s.PhysicalSchema) || len(s.Tables) == 0 {
		return fmt.Errorf("%w: version, artifact, schema, or tables", ErrInvalid)
	}
	last := ""
	ids := map[string]bool{}
	for _, t := range s.Tables {
		if !ident.MatchString(t.Name) || t.Name <= last || len(t.Pairs) == 0 {
			return fmt.Errorf("%w: sorted safe nonempty tables required", ErrInvalid)
		}
		last = t.Name
		pl := ""
		cols := map[string]bool{}
		for _, x := range t.Pairs {
			if !ident.MatchString(x.ID) || x.ID <= pl || !ident.MatchString(x.OldColumn) || !ident.MatchString(x.NewColumn) || x.OldColumn == x.NewColumn || ids[x.ID] || cols[x.OldColumn] || cols[x.NewColumn] {
				return fmt.Errorf("%w: pair ids/columns must be sorted, unique, and safe", ErrInvalid)
			}
			pl = x.ID
			ids[x.ID] = true
			cols[x.OldColumn] = true
			cols[x.NewColumn] = true
			if _, e := renderTransform(x.Forward, "x"); e != nil {
				return fmt.Errorf("%w: pair %s forward: %v", ErrInvalid, x.ID, e)
			}
			if transformMayBeLossy(x.Forward) || (x.Reverse != "" && transformMayBeLossy(x.Reverse)) {
				if !x.Lossy {
					return fmt.Errorf("%w: pair %s must explicitly declare potentially lossy transform", ErrInvalid, x.ID)
				}
			}
			if x.Reverse != "" {
				if _, e := renderTransform(x.Reverse, "x"); e != nil {
					return fmt.Errorf("%w: pair %s reverse: %v", ErrInvalid, x.ID, e)
				}
			}
			if x.Lossy && !p.AllowLossy {
				return fmt.Errorf("%w: lossy pair %s requires policy", ErrInvalid, x.ID)
			}
			if x.Reverse == "" && !p.AllowNonReversible {
				return fmt.Errorf("%w: pair %s is non-reversible", ErrInvalid, x.ID)
			}
		}
	}
	d, e := digest(s)
	if e != nil || d != s.Digest {
		return fmt.Errorf("%w: digest mismatch", ErrInvalid)
	}
	return nil
}
func transformMayBeLossy(expr string) bool { return expr != "value" }
func digest(s Spec) (string, error) {
	s.Digest = ""
	b, e := json.Marshal(s)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func ParseJSON(b []byte) (Spec, error) {
	if e := rejectDuplicateKeys(b); e != nil {
		return Spec{}, fmt.Errorf("%w: %v", ErrInvalid, e)
	}
	var s Spec
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(&s); e != nil {
		return Spec{}, e
	}
	if e := d.Decode(&struct{}{}); e != io.EOF {
		return Spec{}, fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	return s, nil
}
func rejectDuplicateKeys(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	var value func() error
	value = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		x, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch x {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return errors.New("duplicate or invalid object key")
				}
				seen[s] = true
				if e = value(); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		case '[':
			for d.More() {
				if e = value(); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		}
		return errors.New("unexpected delimiter")
	}
	if e := value(); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func (s Spec) MarshalJSONCanonical() ([]byte, error) {
	if e := s.Validate(Policy{true, true}); e != nil {
		return nil, e
	}
	return json.Marshal(s)
}
func q(x ...string) string { return pgx.Identifier(x).Sanitize() }
func lit(s string) string  { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func renderTransform(expr, value string) (string, error) {
	switch expr {
	case "value":
		return value, nil
	case "lower(value)":
		return "pg_catalog.lower(" + value + ")", nil
	case "upper(value)":
		return "pg_catalog.upper(" + value + ")", nil
	case "btrim(value)":
		return "pg_catalog.btrim(" + value + ")", nil
	}
	if m := cast.FindStringSubmatch(expr); m != nil {
		return "(" + value + ")::" + q(m[1]), nil
	}
	return "", errors.New("only value, lower/upper/btrim(value), and safe casts are supported")
}
func scope(c Config, s Spec) string {
	b, _ := json.Marshal([]string{Version, c.Target, c.Environment, s.ArtifactDigest, s.Digest})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func names(c Config, s Spec, t Table) (string, string) {
	h := sha256.Sum256([]byte(scope(c, s) + "\x00" + t.Name))
	x := hex.EncodeToString(h[:8])
	return "autosql_sync_" + x, "autosql_sync_fn_" + x
}

func Apply(ctx context.Context, cfg Config, s Spec, p Policy) (Status, error) {
	return mutate(ctx, cfg, s, p, false)
}
func Remove(ctx context.Context, cfg Config, s Spec, p Policy) (Status, error) {
	return mutate(ctx, cfg, s, p, true)
}
func mutate(ctx context.Context, cfg Config, s Spec, p Policy, remove bool) (Status, error) {
	if e := s.Validate(p); e != nil {
		return Status{}, e
	}
	if cfg.URL == "" || cfg.Target == "" || cfg.Environment == "" || cfg.LockTimeoutMS <= 0 {
		return Status{}, fmt.Errorf("%w: config", ErrInvalid)
	}
	c, e := pgx.Connect(ctx, cfg.URL)
	if e != nil {
		return Status{}, e
	}
	defer c.Close(context.WithoutCancel(ctx))
	tx, e := c.Begin(ctx)
	if e != nil {
		return Status{}, e
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	_, e = tx.Exec(ctx, "set local lock_timeout="+lit(fmt.Sprintf("%dms", cfg.LockTimeoutMS)))
	if e != nil {
		return Status{}, e
	}
	_, e = tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1,0::bigint))`, "autosql.zdm.shadow-sync/v1/"+scope(cfg, s))
	if e != nil {
		return Status{}, e
	}
	for _, t := range s.Tables {
		tr, fn := names(cfg, s, t)
		marker := "autosql:zdm:shadow-sync:" + scope(cfg, s) + ":" + t.Name
		for _, pair := range t.Pairs {
			for _, col := range []string{pair.OldColumn, pair.NewColumn} {
				var exists bool
				if e = tx.QueryRow(ctx, `select exists(select 1 from pg_attribute where attrelid=$1::regclass and attname=$2 and attnum>0 and not attisdropped)`, q(s.PhysicalSchema, t.Name), col).Scan(&exists); e != nil {
					return Status{}, e
				}
				if !exists {
					return Status{}, fmt.Errorf("%w: missing physical column %s.%s.%s", ErrInvalid, s.PhysicalSchema, t.Name, col)
				}
			}
		}
		var trComment, triggerFunction *string
		var triggerExists bool
		var triggerType *int16
		var triggerEnabled *string
		if e = tx.QueryRow(ctx, `select exists(select 1 from pg_trigger where tgrelid=$1::regclass and tgname=$2 and not tgisinternal),(select obj_description(t.oid,'pg_trigger') from pg_trigger t where t.tgrelid=$1::regclass and t.tgname=$2 and not t.tgisinternal limit 1),(select p.proname from pg_trigger t join pg_proc p on p.oid=t.tgfoid where t.tgrelid=$1::regclass and t.tgname=$2 and not t.tgisinternal limit 1),(select t.tgtype from pg_trigger t where t.tgrelid=$1::regclass and t.tgname=$2 and not t.tgisinternal limit 1),(select t.tgenabled::text from pg_trigger t where t.tgrelid=$1::regclass and t.tgname=$2 and not t.tgisinternal limit 1)`, q(s.PhysicalSchema, t.Name), tr).Scan(&triggerExists, &trComment, &triggerFunction, &triggerType, &triggerEnabled); e != nil {
			return Status{}, e
		}
		if triggerExists && (trComment == nil || *trComment != marker) {
			return Status{}, fmt.Errorf("%w: trigger collision on %s", ErrInvalid, t.Name)
		}
		if triggerExists && (triggerFunction == nil || *triggerFunction != fn || triggerType == nil || *triggerType != 23 || triggerEnabled == nil || *triggerEnabled != "O") {
			return Status{}, fmt.Errorf("%w: trigger definition drift on %s", ErrInvalid, t.Name)
		}
		var fnComment *string
		var fnExists bool
		if e = tx.QueryRow(ctx, `select exists(select 1 from pg_proc p join pg_namespace n on n.oid=p.pronamespace where n.nspname=$1 and p.proname=$2 and p.pronargs=0),(select obj_description(p.oid,'pg_proc') from pg_proc p join pg_namespace n on n.oid=p.pronamespace where n.nspname=$1 and p.proname=$2 and p.pronargs=0 limit 1)`, s.PhysicalSchema, fn).Scan(&fnExists, &fnComment); e != nil {
			return Status{}, e
		}
		if fnExists && (fnComment == nil || *fnComment != marker) {
			return Status{}, fmt.Errorf("%w: function collision on %s", ErrInvalid, t.Name)
		}
		if remove {
			if !triggerExists {
				if fnExists {
					return Status{}, fmt.Errorf("%w: orphan synchronization function", ErrInvalid)
				}
				continue
			}
			if !fnExists || fnComment == nil || *fnComment != marker {
				return Status{}, fmt.Errorf("%w: function ownership marker missing", ErrInvalid)
			}
			if _, e = tx.Exec(ctx, "drop trigger "+q(tr)+" on "+q(s.PhysicalSchema, t.Name)); e != nil {
				return Status{}, e
			}
			if _, e = tx.Exec(ctx, "drop function "+q(s.PhysicalSchema, fn)+"()"); e != nil {
				return Status{}, e
			}
			continue
		}
		body, e := triggerBody(t)
		if e != nil {
			return Status{}, e
		}
		sql := "create or replace function " + q(s.PhysicalSchema, fn) + "() returns trigger language plpgsql security invoker set search_path=pg_catalog as " + lit(body)
		if _, e = tx.Exec(ctx, sql); e != nil {
			return Status{}, fmt.Errorf("create sync function: %w", e)
		}
		if !triggerExists {
			if _, e = tx.Exec(ctx, "create trigger "+q(tr)+" before insert or update on "+q(s.PhysicalSchema, t.Name)+" for each row execute function "+q(s.PhysicalSchema, fn)+"()"); e != nil {
				return Status{}, e
			}
		}
		if _, e = tx.Exec(ctx, "comment on trigger "+q(tr)+" on "+q(s.PhysicalSchema, t.Name)+" is "+lit(marker)); e != nil {
			return Status{}, e
		}
		if _, e = tx.Exec(ctx, "comment on function "+q(s.PhysicalSchema, fn)+"() is "+lit(marker)); e != nil {
			return Status{}, e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return Status{}, e
	}
	return Inspect(ctx, cfg, s, p)
}

func triggerBody(t Table) (string, error) {
	var b strings.Builder
	b.WriteString("begin\nif pg_trigger_depth() > 1 then raise exception 'recursive AutoSQL synchronization'; end if;\n")
	for _, x := range t.Pairs {
		old := "NEW." + q(x.OldColumn)
		nw := "NEW." + q(x.NewColumn)
		f, _ := renderTransform(x.Forward, old)
		rev := ""
		if x.Reverse != "" {
			rev, _ = renderTransform(x.Reverse, nw)
		}
		fmt.Fprintf(&b, "if TG_OP='INSERT' then\n if %s is not null and %s is null then %s := %s;", old, nw, nw, f)
		if rev != "" {
			fmt.Fprintf(&b, " elsif %s is not null and %s is null then %s := %s;", nw, old, old, rev)
		} else {
			fmt.Fprintf(&b, " elsif %s is not null and %s is null then raise exception 'reverse write refused for non-reversible pair %s';", nw, old, x.ID)
		}
		fmt.Fprintf(&b, " elsif %s is not null and %s is not null and %s is distinct from %s then raise exception 'conflicting values for pair %s'; end if;\n", old, nw, nw, f, x.ID)
		fmt.Fprintf(&b, "else\n if %s is distinct from OLD.%s and %s is not distinct from OLD.%s then %s := %s; elsif %s is distinct from OLD.%s and %s is not distinct from OLD.%s then ", old, q(x.OldColumn), nw, q(x.NewColumn), nw, f, nw, q(x.NewColumn), old, q(x.OldColumn))
		if rev != "" {
			fmt.Fprintf(&b, "%s := %s;", old, rev)
		} else {
			fmt.Fprintf(&b, "raise exception 'reverse write refused for non-reversible pair %s';", x.ID)
		}
		fmt.Fprintf(&b, " elsif %s is distinct from OLD.%s and %s is distinct from OLD.%s and %s is distinct from %s then raise exception 'conflicting concurrent values for pair %s'; end if;\nend if;\n", old, q(x.OldColumn), nw, q(x.NewColumn), nw, f, x.ID)
	}
	b.WriteString("return NEW;\nend")
	return b.String(), nil
}

func Inspect(ctx context.Context, cfg Config, s Spec, p Policy) (Status, error) {
	if e := s.Validate(p); e != nil {
		return Status{}, e
	}
	c, e := pgx.Connect(ctx, cfg.URL)
	if e != nil {
		return Status{}, e
	}
	defer c.Close(context.WithoutCancel(ctx))
	st := Status{Digest: s.Digest, Installed: true, RollbackEligible: true}
	for _, t := range s.Tables {
		tr, fn := names(cfg, s, t)
		marker := "autosql:zdm:shadow-sync:" + scope(cfg, s) + ":" + t.Name
		ts := TableStatus{Table: t.Name, Trigger: tr, Function: fn, Pairs: len(t.Pairs), AssignmentsPerWrite: len(t.Pairs)}
		var cm, fnComment, fnSource, fnLanguage, fnReturn, triggerFunction *string
		var triggerType *int16
		var triggerEnabled *string
		var securityDefiner *bool
		var fnConfig []string
		e = c.QueryRow(ctx, `select obj_description(t.oid,'pg_trigger'),p.proname,t.tgtype,t.tgenabled::text,obj_description(p.oid,'pg_proc'),p.prosrc,l.lanname,p.prosecdef,p.proconfig,p.prorettype::regtype::text from pg_trigger t join pg_proc p on p.oid=t.tgfoid join pg_language l on l.oid=p.prolang where t.tgrelid=$1::regclass and t.tgname=$2 and not t.tgisinternal`, q(s.PhysicalSchema, t.Name), tr).Scan(&cm, &triggerFunction, &triggerType, &triggerEnabled, &fnComment, &fnSource, &fnLanguage, &securityDefiner, &fnConfig, &fnReturn)
		if e != nil && e != pgx.ErrNoRows {
			return Status{}, e
		}
		ts.Installed = cm != nil
		body, _ := triggerBody(t)
		ts.Exact = cm != nil && *cm == marker && triggerFunction != nil && *triggerFunction == fn && triggerType != nil && *triggerType == 23 && triggerEnabled != nil && *triggerEnabled == "O" && fnComment != nil && *fnComment == marker && fnSource != nil && *fnSource == body && fnLanguage != nil && *fnLanguage == "plpgsql" && securityDefiner != nil && !*securityDefiner && len(fnConfig) == 1 && fnConfig[0] == "search_path=pg_catalog" && fnReturn != nil && *fnReturn == "trigger"
		st.Installed = st.Installed && ts.Installed
		for _, x := range t.Pairs {
			if x.Lossy || x.Reverse == "" {
				st.RollbackEligible = false
			}
		}
		st.Tables = append(st.Tables, ts)
	}
	return st, nil
}

func SortedTables(x []Table) []Table {
	out := append([]Table(nil), x...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
