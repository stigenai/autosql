package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"autosql/pkg/schema"
)

const (
	RuleDropObject       = "AUTOSQL001"
	RuleTruncate         = "AUTOSQL002"
	RuleNarrowType       = "AUTOSQL003"
	RuleNotNull          = "AUTOSQL004"
	RuleDefaultChange    = "AUTOSQL005"
	RuleConstraintChange = "AUTOSQL006"
	RuleRename           = "AUTOSQL007"
	RuleBlockingDDL      = "AUTOSQL101"
	RuleTableRewrite     = "AUTOSQL102"
	RuleIndexBuild       = "AUTOSQL103"
	RuleValidationScan   = "AUTOSQL104"
	RuleTransaction      = "AUTOSQL105"
)

func Builtins() []Analyzer { return []Analyzer{CompatibilityAnalyzer{}, PostgreSQLAnalyzer{}} }

// AllBuiltins includes the compatibility and engine analyzers plus the
// provider-neutral advanced lint and provenance checks.
func AllBuiltins() []Analyzer {
	return []Analyzer{CompatibilityAnalyzer{}, PostgreSQLAnalyzer{}, AdvancedAnalyzer{}}
}

type CompatibilityAnalyzer struct{}

func (CompatibilityAnalyzer) Name() string { return "compatibility" }
func (a CompatibilityAnalyzer) Attestation() AnalyzerAttestation {
	digest, _ := ConfigDigest(a)
	return AnalyzerAttestation{Implementation: "autosql/pkg/safety.CompatibilityAnalyzer", Version: "1", ConfigDigest: digest}
}
func (CompatibilityAnalyzer) Analyze(_ context.Context, in Input) ([]Diagnostic, error) {
	var out []Diagnostic
	for _, ch := range in.Changes.Changes {
		obj, src := objectFor(ch), sourceFor(ch)
		add := func(rule string, severity Severity, message, impact, remediation string, confidence Confidence, assumptions ...string) {
			out = append(out, Diagnostic{Rule: rule, Severity: severity, Message: message, Object: obj, Source: src, Impact: impact, Remediation: remediation, Confidence: confidence, Assumptions: assumptions})
		}
		switch ch.Operation {
		case schema.OperationDrop:
			add(RuleDropObject, SeverityError, "object is dropped", "Definite data loss or loss of a database API.", "Deprecate consumers, retain or archive data, then drop in a later release.", ConfidenceHigh)
		case schema.OperationRename:
			add(RuleRename, SeverityWarning, "object is renamed", "Existing queries using the old name will fail; a rename may also represent an ambiguous drop-and-create intent.", "Confirm rename intent, then use an expand/contract transition with a compatibility view or dual-read period.", ConfidenceHigh)
		}
		if ch.Operation == schema.OperationCreate && ch.After != nil {
			after := spec(ch.After.Spec)
			if ch.After.Kind == schema.KindColumn {
				if notNull, ok := isNotNull(after); ok && notNull {
					add(RuleNotNull, SeverityWarning, "new column is NOT NULL", "Existing rows require a value and older application versions may omit the column.", "Add the column as nullable, backfill it, then validate and set NOT NULL.", ConfidenceHigh)
				}
				if _, ok := after["default"]; ok {
					add(RuleDefaultChange, SeverityWarning, "new column has a default", "Old and new application versions may observe different write semantics.", "Verify the default is compatible with every deployed application version.", ConfidenceMedium, "Application write behavior is unavailable.")
				}
			}
			switch ch.After.Kind {
			case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
				add(RuleConstraintChange, SeverityWarning, "new constraint is added", "Existing rows or writes may violate the new condition.", "Add validation-capable constraints as NOT VALID, remediate data, then validate.", ConfidenceHigh)
			}
		}
		if ch.Operation != schema.OperationAlter || ch.Before == nil || ch.After == nil {
			continue
		}
		before, after := spec(ch.Before.Spec), spec(ch.After.Spec)
		bt, at := text(before, "type"), text(after, "type")
		if bt != "" && at != "" && bt != at && isNarrowing(bt, at) {
			add(RuleNarrowType, SeverityError, fmt.Sprintf("column type narrows from %s to %s", bt, at), "Existing values may be rejected, truncated, or lose precision.", "Prove values fit, backfill a new column, and switch consumers before removing the old column.", ConfidenceHigh)
		}
		bn, bok := isNotNull(before)
		an, aok := isNotNull(after)
		if bok && aok && !bn && an {
			add(RuleNotNull, SeverityWarning, "NOT NULL constraint is added", "The change fails when existing rows contain NULL and can block concurrent writes during validation.", "Backfill NULLs, validate with a NOT VALID check, then set NOT NULL.", ConfidenceHigh)
		}
		if !equalJSON(before["default"], after["default"]) {
			add(RuleDefaultChange, SeverityWarning, "column default changes", "Old and new application versions may observe different write semantics.", "Deploy application compatibility first and verify existing rows do not require a backfill.", ConfidenceMedium, "Application write behavior is unavailable.")
		}
		if !equalJSON(before["expression"], after["expression"]) || !equalJSON(before["definition"], after["definition"]) {
			add(RuleConstraintChange, SeverityWarning, "constraint definition changes", "Existing rows or writes may violate the new condition.", "Add the replacement as NOT VALID, remediate data, then validate and remove the old constraint.", ConfidenceMedium)
		}
	}
	for _, st := range in.Statements {
		if hasCommand(st.SQL, func(tokens []string) bool { return len(tokens) > 0 && tokens[0] == "truncate" }) {
			if ch, ok := findChange(in.Changes, st.ChangeID); ok {
				out = append(out, Diagnostic{Rule: RuleTruncate, Severity: SeverityError, Message: "table is truncated", Object: objectFor(ch), Source: st.Source, Impact: "Definite deletion of all table rows.", Remediation: "Use a reviewed, bounded data migration with a recoverable backup.", Confidence: ConfidenceHigh})
			}
		}
	}
	return out, nil
}

type PostgreSQLAnalyzer struct{}

func (PostgreSQLAnalyzer) Name() string { return "postgresql-operational" }
func (a PostgreSQLAnalyzer) Attestation() AnalyzerAttestation {
	digest, _ := ConfigDigest(a)
	return AnalyzerAttestation{Implementation: "autosql/pkg/safety.PostgreSQLAnalyzer", Version: "1", ConfigDigest: digest}
}
func (PostgreSQLAnalyzer) Analyze(_ context.Context, in Input) ([]Diagnostic, error) {
	if in.Target.Engine != "" && !strings.EqualFold(in.Target.Engine, "postgresql") && !strings.EqualFold(in.Target.Engine, "postgres") {
		return nil, nil
	}
	version, versionKnown := in.Target.Version, in.Target.Version > 0
	var out []Diagnostic
	for _, ch := range in.Changes.Changes {
		obj, src := objectFor(ch), sourceFor(ch)
		stats, haveStats := in.Target.Statistics[ch.ResourceID]
		assumptions, confidence := targetAssumptions(version, versionKnown, haveStats)
		add := func(rule string, severity Severity, msg, impact, fix string, lock LockLevel, scan, rewrite bool) {
			sev, props := assessRisk(severity, in.Thresholds, lock, scan, rewrite, stats, haveStats)
			out = append(out, Diagnostic{Rule: rule, Severity: sev, Message: msg, Object: obj, Source: src, Impact: impact, Remediation: fix, Confidence: confidence, Assumptions: assumptions, Properties: props})
		}
		if ch.Operation == schema.OperationDrop || ch.Operation == schema.OperationRename {
			add(RuleBlockingDDL, SeverityWarning, "DDL requires ACCESS EXCLUSIVE lock", "Concurrent reads or writes can wait behind the migration.", "Set lock_timeout and schedule the contract step after dependencies are removed.", LockAccessExclusive, false, false)
		}
		isAlter := ch.Operation == schema.OperationAlter && ch.Before != nil && ch.After != nil
		isNewColumn := ch.Operation == schema.OperationCreate && ch.After != nil && ch.After.Kind == schema.KindColumn
		if !isAlter && !isNewColumn {
			continue
		}
		b := map[string]json.RawMessage{}
		if ch.Before != nil {
			b = spec(ch.Before.Spec)
		}
		a := spec(ch.After.Spec)
		bt, at := text(b, "type"), text(a, "type")
		rewrite := bt != "" && at != "" && bt != at && !metadataOnlyCast(bt, at)
		_, hadDefault := b["default"]
		rawDefault, hasDefault := a["default"]
		if !hadDefault && hasDefault {
			kind := classifyDefault(rawDefault)
			// PostgreSQL 11 introduced the fast default path for non-volatile
			// expressions. Unknown versions and unknown/volatile expressions are
			// conservatively treated as rewrites.
			if !versionKnown || version < 11 || kind == defaultVolatile || kind == defaultUnknown {
				rewrite = true
			}
		}
		if rewrite {
			add(RuleTableRewrite, SeverityWarning, "ALTER TABLE may rewrite the table", "A rewrite scans and replaces the table while holding a strong lock.", "Backfill a new column in batches and swap it in a later migration.", LockAccessExclusive, true, true)
		}
		bn, bok := isNotNull(b)
		an, aok := isNotNull(a)
		if aok && an && ((bok && !bn) || isNewColumn) {
			add(RuleValidationScan, SeverityWarning, "NOT NULL validation scans the table", "Validation time grows with table size and holds a lock conflicting with schema changes.", "Validate an equivalent NOT VALID check constraint first.", LockShareUpdateExclusive, true, false)
		}
	}
	for _, st := range in.Statements {
		ch, ok := findChange(in.Changes, st.ChangeID)
		if !ok {
			continue
		}
		obj := objectFor(ch)
		stats, haveStats := in.Target.Statistics[ch.ResourceID]
		assumptions, confidence := targetAssumptions(version, versionKnown, haveStats)
		add := func(rule string, severity Severity, msg, impact, fix string, lock LockLevel, scan bool) {
			sev, props := assessRisk(severity, in.Thresholds, lock, scan, false, stats, haveStats)
			out = append(out, Diagnostic{Rule: rule, Severity: sev, Message: msg, Object: obj, Source: st.Source, Impact: impact, Remediation: fix, Confidence: confidence, Assumptions: assumptions, Properties: props})
		}
		blockingIndex, concurrentIndex := indexCommands(st.SQL)
		if blockingIndex {
			add(RuleIndexBuild, SeverityWarning, "index is built without CONCURRENTLY", "Writes are blocked for the duration of the index build.", "Use CREATE INDEX CONCURRENTLY outside a transaction and monitor progress.", LockShare, true)
		}
		if concurrentIndex {
			out = append(out, Diagnostic{Rule: RuleTransaction, Severity: SeverityError, Message: "concurrent index operation cannot run in a transaction block", Object: obj, Source: st.Source, Impact: "Execution in the migration transaction fails.", Remediation: "Run this statement in an explicitly non-transactional migration step.", Confidence: confidence, Assumptions: assumptions})
		}
		if hasCommand(st.SQL, enumAddValueCommand) {
			severity, message, impact, fix := SeverityWarning, "new enum value is unavailable until commit", "Using the new value in the same transaction fails.", "Commit the enum change before statements that use the new value."
			if !versionKnown || version < 12 {
				severity, message, impact, fix = SeverityError, "enum ADD VALUE cannot safely run in the migration transaction", "PostgreSQL versions before 12 reject this statement in a transaction block.", "Run the enum change in a non-transactional step; commit it before use."
			}
			out = append(out, Diagnostic{Rule: RuleTransaction, Severity: severity, Message: message, Object: obj, Source: st.Source, Impact: impact, Remediation: fix, Confidence: confidence, Assumptions: assumptions})
		}
		if hasCommand(st.SQL, validateConstraintCommand) {
			add(RuleValidationScan, SeverityWarning, "constraint validation scans the table", "A full scan consumes I/O and holds SHARE UPDATE EXCLUSIVE lock.", "Validate during a controlled window and cap statement duration.", LockShareUpdateExclusive, true)
		}
	}
	return out, nil
}

func targetAssumptions(version int, versionKnown, haveStats bool) ([]string, Confidence) {
	confidence := ConfidenceHigh
	var assumptions []string
	if versionKnown {
		assumptions = append(assumptions, fmt.Sprintf("PostgreSQL major version is %d.", version))
	} else {
		assumptions = append(assumptions, "PostgreSQL major version is unknown; oldest supported behavior is assumed.")
		confidence = ConfidenceLow
	}
	if !haveStats {
		assumptions = append(assumptions, "Target table statistics are unavailable; configured size thresholds cannot be proven.")
		if confidence == ConfidenceHigh {
			confidence = ConfidenceMedium
		}
	}
	return assumptions, confidence
}

func assessRisk(base Severity, t Thresholds, lock LockLevel, scan, rewrite bool, stats TableStatistics, haveStats bool) (Severity, map[string]any) {
	props := map[string]any{"lock_level": lock.String(), "table_rewrite": rewrite}
	var exceeded, unproven []string
	if t.MaxLockLevel > 0 && lock > t.MaxLockLevel {
		exceeded = append(exceeded, "lock_level")
	}
	if scan && t.MaxRowsScanned > 0 {
		if !haveStats {
			unproven = append(unproven, "estimated_rows")
		} else if stats.EstimatedRows > t.MaxRowsScanned {
			exceeded = append(exceeded, "estimated_rows")
		}
	}
	if rewrite && t.MaxRewriteBytes > 0 {
		if !haveStats {
			unproven = append(unproven, "rewrite_bytes")
		} else if stats.TotalBytes > t.MaxRewriteBytes {
			exceeded = append(exceeded, "rewrite_bytes")
		}
	}
	if haveStats {
		props["estimated_rows"] = stats.EstimatedRows
		props["estimated_bytes"] = stats.TotalBytes
	}
	if len(exceeded) > 0 {
		props["threshold_exceeded"] = exceeded
	}
	if len(unproven) > 0 {
		props["threshold_unproven"] = unproven
	}
	if len(exceeded) > 0 || len(unproven) > 0 {
		return SeverityError, props
	}
	return base, props
}

func spec(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return m
}
func text(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return strings.ToLower(strings.TrimSpace(s))
}
func boolean(m map[string]json.RawMessage, k string) (bool, bool) {
	raw, ok := m[k]
	if !ok {
		return false, false
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false, false
	}
	return b, true
}
func isNotNull(m map[string]json.RawMessage) (bool, bool) {
	if value, ok := boolean(m, "not_null"); ok {
		return value, true
	}
	if nullable, ok := boolean(m, "nullable"); ok {
		return !nullable, true
	}
	return false, false
}
func equalJSON(a, b json.RawMessage) bool {
	var x, y any
	if len(a) > 0 {
		_ = json.Unmarshal(a, &x)
	}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &y)
	}
	ax, _ := json.Marshal(x)
	ay, _ := json.Marshal(y)
	return string(ax) == string(ay)
}

type defaultClass uint8

const (
	defaultUnknown defaultClass = iota
	defaultLiteral
	defaultStable
	defaultVolatile
)

func classifyDefault(raw json.RawMessage) defaultClass {
	if len(raw) == 0 {
		return defaultUnknown
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return defaultUnknown
	}
	s, ok := v.(string)
	if !ok {
		return defaultLiteral
	}
	n := strings.ToLower(strings.TrimSpace(s))
	if n == "" {
		return defaultLiteral
	}
	for _, marker := range []string{"random(", "clock_timestamp(", "timeofday(", "nextval(", "gen_random_uuid(", "uuid_generate_v"} {
		if strings.Contains(n, marker) {
			return defaultVolatile
		}
	}
	for _, marker := range []string{"now(", "statement_timestamp(", "transaction_timestamp(", "current_timestamp", "current_date", "current_time", "localtimestamp", "localtime"} {
		if strings.Contains(n, marker) {
			return defaultStable
		}
	}
	if strings.ContainsAny(n, "()") || strings.Contains(n, "::") {
		return defaultUnknown
	}
	return defaultLiteral
}

type parsedType struct {
	base             string
	precision, scale int
	bounded          bool
}

var typeArgs = regexp.MustCompile(`^([a-z ]+)\s*\(\s*(\d+)\s*(?:,\s*(\d+)\s*)?\)$`)

func parseType(s string) parsedType {
	s = strings.ToLower(strings.TrimSpace(s))
	aliases := map[string]string{"character varying": "varchar", "decimal": "numeric", "int": "integer", "int4": "integer", "int8": "bigint", "int2": "smallint"}
	if a, ok := aliases[s]; ok {
		s = a
	}
	m := typeArgs.FindStringSubmatch(s)
	if len(m) == 0 {
		return parsedType{base: s}
	}
	base := strings.TrimSpace(m[1])
	if a, ok := aliases[base]; ok {
		base = a
	}
	p, _ := strconv.Atoi(m[2])
	scale := 0
	if m[3] != "" {
		scale, _ = strconv.Atoi(m[3])
	}
	return parsedType{base: base, precision: p, scale: scale, bounded: true}
}
func isNarrowing(from, to string) bool {
	f, t := parseType(from), parseType(to)
	if (f.base == "text" || f.base == "varchar") && t.base == "varchar" && t.bounded {
		return !f.bounded || t.precision < f.precision
	}
	if f.base == "varchar" && t.base == "varchar" && f.bounded && t.bounded {
		return t.precision < f.precision
	}
	if f.base == "numeric" && t.base == "numeric" {
		if !f.bounded {
			return t.bounded
		}
		if !t.bounded {
			return false
		}
		return t.scale < f.scale || t.precision-t.scale < f.precision-f.scale
	}
	ranks := map[string]int{"smallint": 1, "integer": 2, "bigint": 3, "numeric": 4}
	rf, of := ranks[f.base]
	rt, ot := ranks[t.base]
	return of && ot && rt < rf
}
func metadataOnlyCast(from, to string) bool {
	f, t := parseType(from), parseType(to)
	return f.base == t.base && f.precision == t.precision && f.scale == t.scale && f.bounded == t.bounded || (f.base == "varchar" && t.base == "text")
}
func findChange(cs schema.ChangeSet, id string) (schema.Change, bool) {
	for _, c := range cs.Changes {
		if c.ID == id {
			return c, true
		}
	}
	return schema.Change{}, false
}

// sqlCommands tokenizes each SQL statement independently. Comments and literal
// bodies are discarded, while quoted identifiers become an opaque identifier
// token. Command recognizers below inspect grammar positions rather than global
// keyword presence, preventing tokens from unrelated statements from combining.
func sqlCommands(sql string) [][]string {
	var commands [][]string
	var out strings.Builder
	flush := func() {
		fields := strings.Fields(out.String())
		if len(fields) > 0 {
			commands = append(commands, fields)
		}
		out.Reset()
	}
	for i := 0; i < len(sql); {
		switch {
		case i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-':
			i += 2
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
		case i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*':
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				if i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' {
					depth++
					i += 2
				} else if i+1 < len(sql) && sql[i] == '*' && sql[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
			out.WriteByte(' ')
		case sql[i] == '\'':
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			out.WriteByte(' ')
		case sql[i] == '"':
			i++
			for i < len(sql) {
				if sql[i] == '"' {
					if i+1 < len(sql) && sql[i+1] == '"' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			out.WriteString(" identifier ")
		case sql[i] == '$':
			tagEnd := i + 1
			for tagEnd < len(sql) && (unicode.IsLetter(rune(sql[tagEnd])) || unicode.IsDigit(rune(sql[tagEnd])) || sql[tagEnd] == '_') {
				tagEnd++
			}
			if tagEnd < len(sql) && sql[tagEnd] == '$' {
				tag := sql[i : tagEnd+1]
				i = tagEnd + 1
				if end := strings.Index(sql[i:], tag); end >= 0 {
					i += end + len(tag)
				} else {
					i = len(sql)
				}
				out.WriteByte(' ')
			} else {
				out.WriteByte(' ')
				i++
			}
		case sql[i] == ';':
			flush()
			i++
		default:
			r := rune(sql[i])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || sql[i] == '_' {
				out.WriteByte(byte(unicode.ToLower(r)))
			} else {
				out.WriteByte(' ')
			}
			i++
		}
	}
	flush()
	return commands
}

func hasCommand(sql string, match func([]string) bool) bool {
	for _, command := range sqlCommands(sql) {
		if match(command) {
			return true
		}
	}
	return false
}

func indexCommands(sql string) (blockingCreate, concurrentOperation bool) {
	for _, tokens := range sqlCommands(sql) {
		if len(tokens) < 2 {
			continue
		}
		switch tokens[0] {
		case "create":
			i := 1
			if i < len(tokens) && tokens[i] == "unique" {
				i++
			}
			if i < len(tokens) && tokens[i] == "index" {
				if i+1 < len(tokens) && tokens[i+1] == "concurrently" {
					concurrentOperation = true
				} else {
					blockingCreate = true
				}
			}
		case "drop":
			if tokens[1] == "index" && len(tokens) > 2 && tokens[2] == "concurrently" {
				concurrentOperation = true
			}
		}
	}
	return blockingCreate, concurrentOperation
}

func enumAddValueCommand(tokens []string) bool {
	if len(tokens) < 5 || tokens[0] != "alter" || tokens[1] != "type" {
		return false
	}
	for i := 2; i+1 < len(tokens); i++ {
		if tokens[i] == "add" && tokens[i+1] == "value" {
			return true
		}
	}
	return false
}

func validateConstraintCommand(tokens []string) bool {
	if len(tokens) < 5 || tokens[0] != "alter" || tokens[1] != "table" {
		return false
	}
	for i := 2; i+1 < len(tokens); i++ {
		if tokens[i] == "validate" && tokens[i+1] == "constraint" {
			return true
		}
	}
	return false
}
