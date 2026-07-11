package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

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

// Builtins returns the standard compatibility and PostgreSQL operational analyzers.
func Builtins() []Analyzer { return []Analyzer{CompatibilityAnalyzer{}, PostgreSQLAnalyzer{}} }

type CompatibilityAnalyzer struct{}

func (CompatibilityAnalyzer) Name() string { return "compatibility" }

func (CompatibilityAnalyzer) Analyze(_ context.Context, in Input) ([]Diagnostic, error) {
	var out []Diagnostic
	for _, ch := range in.Changes.Changes {
		obj, src := objectFor(ch), sourceFor(ch)
		add := func(rule string, severity Severity, message, impact, remediation string, confidence Confidence, assumptions ...string) {
			out = append(out, Diagnostic{Rule: rule, Severity: severity, Message: message, Object: obj, Source: src, Impact: impact, Remediation: remediation, Confidence: confidence, Assumptions: assumptions})
		}
		if ch.Operation == schema.OperationDrop {
			add(RuleDropObject, SeverityError, "object is dropped", "Definite data loss or loss of a database API.", "Deprecate consumers, retain or archive data, then drop in a later release.", ConfidenceHigh)
		}
		if ch.Operation == schema.OperationRename {
			add(RuleRename, SeverityWarning, "object is renamed", "Existing queries using the old name will fail.", "Use an expand/contract transition with a compatibility view or dual-read period.", ConfidenceHigh)
		}
		if ch.Operation != schema.OperationAlter || ch.Before == nil || ch.After == nil {
			continue
		}
		before, after := spec(ch.Before.Spec), spec(ch.After.Spec)
		bt, at := text(before, "type"), text(after, "type")
		if bt != "" && at != "" && bt != at && isNarrowing(bt, at) {
			add(RuleNarrowType, SeverityError, fmt.Sprintf("column type narrows from %s to %s", bt, at), "Existing values may be rejected, truncated, or lose precision.", "Prove values fit, backfill a new column, and switch consumers before removing the old column.", ConfidenceMedium, "Type compatibility is inferred from canonical PostgreSQL type names.")
		}
		bn, bok := boolean(before, "nullable")
		an, aok := boolean(after, "nullable")
		if bok && aok && bn && !an {
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
		if regexp.MustCompile(`(?i)\btruncate\b`).MatchString(st.SQL) {
			if ch, ok := findChange(in.Changes, st.ChangeID); ok {
				out = append(out, Diagnostic{Rule: RuleTruncate, Severity: SeverityError, Message: "table is truncated", Object: objectFor(ch), Source: st.Source, Impact: "Definite deletion of all table rows.", Remediation: "Use a reviewed, bounded data migration with a recoverable backup.", Confidence: ConfidenceHigh})
			}
		}
	}
	return out, nil
}

type PostgreSQLAnalyzer struct{}

func (PostgreSQLAnalyzer) Name() string { return "postgresql-operational" }

func (PostgreSQLAnalyzer) Analyze(_ context.Context, in Input) ([]Diagnostic, error) {
	if in.Target.Engine != "" && !strings.EqualFold(in.Target.Engine, "postgresql") && !strings.EqualFold(in.Target.Engine, "postgres") {
		return nil, nil
	}
	version := in.Target.Version
	if version == 0 {
		version = 14
	}
	var out []Diagnostic
	for _, ch := range in.Changes.Changes {
		obj, src := objectFor(ch), sourceFor(ch)
		stats, haveStats := in.Target.Statistics[ch.ResourceID]
		assumption := []string{fmt.Sprintf("PostgreSQL major version is %d.", version)}
		confidence := ConfidenceHigh
		if !haveStats {
			assumption = append(assumption, "Target table statistics are unavailable; size estimates are conservative.")
			confidence = ConfidenceMedium
		}
		add := func(rule string, severity Severity, msg, impact, fix string, lock LockLevel, rewrite bool, rows, bytes int64) {
			props := map[string]any{"lock_level": lock.String(), "table_rewrite": rewrite}
			if rows > 0 {
				props["estimated_rows"] = rows
			}
			if bytes > 0 {
				props["estimated_bytes"] = bytes
			}
			sev := severity
			if in.Thresholds.MaxLockLevel > 0 && lock > in.Thresholds.MaxLockLevel {
				sev = SeverityError
				props["threshold_exceeded"] = "lock_level"
			}
			if in.Thresholds.MaxRowsScanned > 0 && rows > in.Thresholds.MaxRowsScanned {
				sev = SeverityError
				props["threshold_exceeded"] = "estimated_rows"
			}
			if in.Thresholds.MaxRewriteBytes > 0 && bytes > in.Thresholds.MaxRewriteBytes {
				sev = SeverityError
				props["threshold_exceeded"] = "rewrite_bytes"
			}
			out = append(out, Diagnostic{Rule: rule, Severity: sev, Message: msg, Object: obj, Source: src, Impact: impact, Remediation: fix, Confidence: confidence, Assumptions: assumption, Properties: props})
		}
		if ch.Operation == schema.OperationDrop || ch.Operation == schema.OperationRename {
			add(RuleBlockingDDL, SeverityWarning, "DDL requires ACCESS EXCLUSIVE lock", "Concurrent reads or writes can wait behind the migration.", "Set lock_timeout and schedule the contract step after dependencies are removed.", LockAccessExclusive, false, 0, 0)
		}
		if ch.Operation == schema.OperationAlter && ch.Before != nil && ch.After != nil {
			b, a := spec(ch.Before.Spec), spec(ch.After.Spec)
			bt, at := text(b, "type"), text(a, "type")
			rewrite := bt != "" && at != "" && bt != at && !metadataOnlyCast(bt, at)
			_, hadDefault := b["default"]
			_, hasDefault := a["default"]
			// PostgreSQL 11+ avoids a rewrite for a new constant default.
			if !hadDefault && hasDefault && (version < 11 || !constantDefault(a["default"])) {
				rewrite = true
			}
			if rewrite {
				add(RuleTableRewrite, SeverityWarning, "ALTER TABLE may rewrite the table", "A rewrite scans and replaces the table while holding a strong lock.", "Backfill a new column in batches and swap it in a later migration.", LockAccessExclusive, true, stats.EstimatedRows, stats.TotalBytes)
			}
			bn, bok := boolean(b, "nullable")
			an, aok := boolean(a, "nullable")
			if bok && aok && bn && !an {
				add(RuleValidationScan, SeverityWarning, "NOT NULL validation scans the table", "Validation time grows with table size and holds a lock conflicting with schema changes.", "Validate an equivalent NOT VALID check constraint first.", LockShareUpdateExclusive, false, stats.EstimatedRows, stats.TotalBytes)
			}
		}
	}
	for _, st := range in.Statements {
		ch, ok := findChange(in.Changes, st.ChangeID)
		if !ok {
			continue
		}
		lower := strings.ToLower(st.SQL)
		obj := objectFor(ch)
		stats, have := in.Target.Statistics[ch.ResourceID]
		confidence := ConfidenceHigh
		assumptions := []string{fmt.Sprintf("PostgreSQL major version is %d.", version)}
		if !have {
			confidence = ConfidenceMedium
			assumptions = append(assumptions, "Target statistics are unavailable.")
		}
		if strings.Contains(lower, "create index") && !strings.Contains(lower, "concurrently") {
			out = append(out, Diagnostic{Rule: RuleIndexBuild, Severity: thresholdSeverity(SeverityWarning, in.Thresholds, LockShare, stats), Message: "index is built without CONCURRENTLY", Object: obj, Source: st.Source, Impact: "Writes are blocked for the duration of the index build.", Remediation: "Use CREATE INDEX CONCURRENTLY outside a transaction and monitor progress.", Confidence: confidence, Assumptions: assumptions, Properties: riskProps(LockShare, false, stats)})
		}
		if strings.Contains(lower, "create index concurrently") || strings.Contains(lower, "drop index concurrently") || strings.Contains(lower, "alter type") && strings.Contains(lower, "add value") {
			out = append(out, Diagnostic{Rule: RuleTransaction, Severity: SeverityError, Message: "statement has PostgreSQL transaction restrictions", Object: obj, Source: st.Source, Impact: "Execution inside a migration transaction can fail or make the new enum value unusable until commit.", Remediation: "Run this statement in an explicitly non-transactional migration step.", Confidence: ConfidenceHigh, Assumptions: assumptions})
		}
		if strings.Contains(lower, "validate constraint") {
			out = append(out, Diagnostic{Rule: RuleValidationScan, Severity: thresholdSeverity(SeverityWarning, in.Thresholds, LockShareUpdateExclusive, stats), Message: "constraint validation scans the table", Object: obj, Source: st.Source, Impact: "A full scan consumes I/O and holds SHARE UPDATE EXCLUSIVE lock.", Remediation: "Validate during a controlled window and cap statement duration.", Confidence: confidence, Assumptions: assumptions, Properties: riskProps(LockShareUpdateExclusive, false, stats)})
		}
	}
	return out, nil
}

func spec(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return m
}
func text(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return strings.ToLower(s)
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
func equalJSON(a, b json.RawMessage) bool {
	var x, y any
	if len(a) > 0 {
		_ = json.Unmarshal(a, &x)
	}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &y)
	}
	return fmt.Sprintf("%#v", x) == fmt.Sprintf("%#v", y)
}
func constantDefault(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64, bool, nil:
		return true
	}
	return false
}
func findChange(cs schema.ChangeSet, id string) (schema.Change, bool) {
	for _, c := range cs.Changes {
		if c.ID == id {
			return c, true
		}
	}
	return schema.Change{}, false
}
func width(t string) int {
	re := regexp.MustCompile(`\((\d+)`)
	m := re.FindStringSubmatch(t)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
func isNarrowing(from, to string) bool {
	f, t := width(from), width(to)
	if f > 0 && t > 0 {
		return t < f
	}
	ranks := map[string]int{"smallint": 1, "integer": 2, "int": 2, "bigint": 3, "numeric": 4}
	rf, of := ranks[from]
	rt, ot := ranks[to]
	return of && ot && rt < rf
}
func metadataOnlyCast(from, to string) bool {
	return from == to || (from == "varchar" && to == "text") || (strings.HasPrefix(from, "varchar(") && to == "text")
}
func thresholdSeverity(base Severity, t Thresholds, l LockLevel, s TableStatistics) Severity {
	if t.MaxLockLevel > 0 && l > t.MaxLockLevel || t.MaxRowsScanned > 0 && s.EstimatedRows > t.MaxRowsScanned || t.MaxRewriteBytes > 0 && s.TotalBytes > t.MaxRewriteBytes {
		return SeverityError
	}
	return base
}
func riskProps(l LockLevel, rewrite bool, s TableStatistics) map[string]any {
	m := map[string]any{"lock_level": l.String(), "table_rewrite": rewrite}
	if s.EstimatedRows > 0 {
		m["estimated_rows"] = s.EstimatedRows
	}
	if s.TotalBytes > 0 {
		m["estimated_bytes"] = s.TotalBytes
	}
	return m
}
