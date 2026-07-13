package safety

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"autosql/pkg/schema"
)

const (
	RuleNaming          = "AUTOSQL201"
	RuleDynamicSQL      = "AUTOSQL202"
	RuleTableCopy       = "AUTOSQL203"
	RuleMergeConflict   = "AUTOSQL204"
	RuleAgentProvenance = "AUTOSQL205"
	RuleGeneratedTest   = "AUTOSQL206"
)

// AdvancedAnalyzer covers policy checks that are independent of a particular
// SQL engine. It deliberately reports evidence and a deterministic test token,
// never making an approval decision or executing generated SQL.
type AdvancedAnalyzer struct {
	NamingPrefix string
}

func (a AdvancedAnalyzer) Name() string { return "advanced-lint" }
func (a AdvancedAnalyzer) Attestation() AnalyzerAttestation {
	digest, _ := ConfigDigest(a)
	return AnalyzerAttestation{Implementation: "autosql/pkg/safety.AdvancedAnalyzer", Version: "1", ConfigDigest: digest}
}

func (a AdvancedAnalyzer) Analyze(_ context.Context, in Input) ([]Diagnostic, error) {
	var out []Diagnostic
	add := func(rule string, sev Severity, ch schema.Change, message, impact, remediation string, confidence Confidence, props map[string]any) {
		out = append(out, Diagnostic{Rule: rule, Severity: sev, Message: message, Object: objectFor(ch), Source: sourceFor(ch), Impact: impact, Remediation: remediation, Confidence: confidence, Properties: props})
	}
	seen := map[string]schema.Change{}
	for _, ch := range in.Changes.Changes {
		if old, ok := seen[ch.ResourceID]; ok && (old.Operation != ch.Operation || old.ID != ch.ID) {
			add(RuleMergeConflict, SeverityError, ch, "multiple changes target the same resource", "Merge order is ambiguous and can produce a plan different from the reviewed intent.", "Squash or explicitly order the changes and review the resulting plan digest.", ConfidenceHigh, map[string]any{"resource_id": ch.ResourceID, "conflicting_change": old.ID})
		}
		seen[ch.ResourceID] = ch
		name := objectFor(ch).Name
		leaf := name
		if i := strings.LastIndex(leaf, "."); i >= 0 {
			leaf = leaf[i+1:]
		}
		if !snakeCase(leaf) || (a.NamingPrefix != "" && !strings.HasPrefix(leaf, a.NamingPrefix)) {
			message := "object name does not follow the configured naming convention"
			if a.NamingPrefix == "" {
				message = "object name is not lower_snake_case"
			}
			add(RuleNaming, SeverityWarning, ch, message, "Inconsistent names make ownership, drift review, and generated SQL harder to reason about.", "Rename through an expand/contract migration or add an explicit naming exception to policy.", ConfidenceHigh, map[string]any{"name": name})
		}
	}
	for _, st := range in.Statements {
		ch, ok := findChange(in.Changes, st.ChangeID)
		if !ok {
			continue
		}
		sql := strings.ToLower(strings.Join(strings.Fields(st.SQL), " "))
		if dynamicSQL(sql) {
			add(RuleDynamicSQL, SeverityError, ch, "statement constructs dynamic SQL from concatenated or formatted input", "Untrusted identifiers or values can alter the executed command and bypass review.", "Use typed identifier quoting and fixed statement templates; bind values as parameters.", ConfidenceHigh, map[string]any{"evidence": dynamicEvidence(sql)})
		}
		if tableCopy(sql) {
			add(RuleTableCopy, SeverityWarning, ch, "statement copies table data or depends on a full-table query", "The operation can consume unbounded I/O and may create a second copy of production data.", "Use a bounded, resumable backfill with row limits, checkpoints, and a verified rollback path.", ConfidenceHigh, map[string]any{"evidence": copyEvidence(sql)})
		}
		if requiresGeneratedTest(sql) {
			token := testToken(st.SQL, ch.ResourceID)
			add(RuleGeneratedTest, SeverityInfo, ch, "change requires a reproducible simulation test before approval", "A generated migration must be exercised against a disposable schema without production credentials.", "Run the emitted simulation fixture and bind its result digest to the approval.", ConfidenceHigh, map[string]any{"test_id": token, "sandbox": true, "production_credentials": false})
		}
	}
	if in.Provenance.Agent != "" || in.Provenance.Untrusted {
		props := map[string]any{"untrusted": true}
		if in.Provenance.Agent != "" {
			props["agent"] = in.Provenance.Agent
		}
		if in.Provenance.AgentVersion != "" {
			props["agent_version"] = in.Provenance.AgentVersion
		}
		if in.Provenance.PromptDigest != "" {
			props["prompt_digest"] = in.Provenance.PromptDigest
		}
		if in.Provenance.OutputDigest != "" {
			props["output_digest"] = in.Provenance.OutputDigest
		}
		for _, ch := range in.Changes.Changes {
			add(RuleAgentProvenance, SeverityWarning, ch, "change was authored by an untrusted agent and requires ordinary human review", "Agent output is not evidence of safety or approval.", "Run all analyzers, reproduce simulation tests, and obtain policy-compliant approval.", ConfidenceHigh, props)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Object.ID < out[j].Object.ID
	})
	return out, nil
}

func snakeCase(s string) bool {
	if s == "" || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "__") {
		return false
	}
	for _, r := range s {
		if unicode.IsUpper(r) || !(unicode.IsLower(r) || unicode.IsDigit(r) || r == '_') {
			return false
		}
	}
	return true
}

var dynamicRE = regexp.MustCompile(`\b(execute|exec)\b.*(\|\||format\s*\()`)

func dynamicSQL(s string) bool {
	return dynamicRE.MatchString(s) || (strings.Contains(s, "execute") && strings.Contains(s, "quote_literal"))
}
func dynamicEvidence(s string) string {
	if strings.Contains(s, "format(") {
		return "format"
	}
	if strings.Contains(s, "||") {
		return "concatenation"
	}
	return "execute"
}
func tableCopy(s string) bool {
	return strings.Contains(s, "create table") && (strings.Contains(s, " as select") || strings.Contains(s, " like ")) || strings.Contains(s, "select ") && strings.Contains(s, " into ")
}
func copyEvidence(s string) string {
	if strings.Contains(s, "select ") && strings.Contains(s, " into ") {
		return "select_into"
	}
	if strings.Contains(s, " like ") {
		return "create_like"
	}
	return "create_as_select"
}
func requiresGeneratedTest(s string) bool {
	return tableCopy(s) || strings.Contains(s, "update ") || strings.Contains(s, "delete ") || strings.Contains(s, "insert ")
}
func testToken(sql, resource string) string {
	h := sha256.Sum256([]byte("autosql.simulation-test/v1\x00" + resource + "\x00" + sql))
	return "sha256:" + hex.EncodeToString(h[:])
}
