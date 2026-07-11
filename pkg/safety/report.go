package safety

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

var sensitive = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret|api[_-]?key)\s*[:=]\s*[^\s,;]+`),
	regexp.MustCompile(`(?i)postgres(?:ql)?://[^\s/@:]+:[^\s/@]+@`),
}

func redact(s string) string {
	for _, re := range sensitive {
		s = re.ReplaceAllStringFunc(s, func(v string) string {
			if strings.HasPrefix(strings.ToLower(v), "postgres") {
				i := strings.Index(v, "://")
				at := strings.LastIndex(v, "@")
				return v[:i+3] + "[REDACTED]" + v[at:]
			}
			i := strings.IndexAny(v, "=:")
			if i < 0 {
				return "[REDACTED]"
			}
			return v[:i+1] + "[REDACTED]"
		})
	}
	return s
}

func sanitized(in []Diagnostic) []Diagnostic {
	out := append([]Diagnostic(nil), in...)
	for i := range out {
		d := &out[i]
		d.Message = redact(d.Message)
		d.Impact = redact(d.Impact)
		d.Remediation = redact(d.Remediation)
		d.Object.Name = redact(d.Object.Name)
		if d.Source != nil {
			source := *d.Source
			source.URI = redact(source.URI)
			d.Source = &source
		}
		d.Properties = redactMap(d.Properties)
		for j := range d.Assumptions {
			d.Assumptions[j] = redact(d.Assumptions[j])
		}
		if d.Suppressed != nil {
			s := *d.Suppressed
			s.Reason = redact(s.Reason)
			d.Suppressed = &s
		}
	}
	return out
}

func redactMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch x := v.(type) {
		case string:
			out[k] = redact(x)
		case map[string]any:
			out[k] = redactMap(x)
		case []string:
			copy := append([]string(nil), x...)
			for i := range copy {
				copy[i] = redact(copy[i])
			}
			out[k] = copy
		default:
			out[k] = v
		}
	}
	return out
}

// WriteJSON writes deterministic, indented JSON after secret redaction.
func WriteJSON(w io.Writer, diagnostics []Diagnostic) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(sanitized(diagnostics))
}

// WriteHuman writes one stable, grep-friendly diagnostic block per result.
func WriteHuman(w io.Writer, diagnostics []Diagnostic) error {
	for _, d := range sanitized(diagnostics) {
		state := ""
		if d.Suppressed != nil {
			state = " (suppressed: " + d.Suppressed.Reason + ")"
		}
		loc := ""
		if d.Source != nil {
			loc = fmt.Sprintf(" %s:%d:%d", d.Source.URI, d.Source.Line, d.Source.Column)
		}
		if _, err := fmt.Fprintf(w, "%s [%s] %s%s%s\n  %s\n  impact: %s\n  remediation: %s\n", d.Severity, d.Rule, d.Object.Name, loc, state, d.Message, d.Impact, d.Remediation); err != nil {
			return err
		}
	}
	return nil
}

type sarifLog struct {
	Version string     `json:"version"`
	Schema  string     `json:"$schema"`
	Runs    []sarifRun `json:"runs"`
}
type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}
type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}
type sarifDriver struct {
	Name  string      `json:"name"`
	Rules []sarifRule `json:"rules"`
}
type sarifRule struct {
	ID               string       `json:"id"`
	ShortDescription sarifMessage `json:"shortDescription"`
}
type sarifMessage struct {
	Text string `json:"text"`
}
type sarifResult struct {
	RuleID       string             `json:"ruleId"`
	Level        string             `json:"level"`
	Message      sarifMessage       `json:"message"`
	Locations    []sarifLocation    `json:"locations,omitempty"`
	Suppressions []sarifSuppression `json:"suppressions,omitempty"`
	Properties   map[string]any     `json:"properties,omitempty"`
}
type sarifLocation struct {
	PhysicalLocation sarifPhysical `json:"physicalLocation"`
}
type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
	Region           sarifRegion   `json:"region,omitempty"`
}
type sarifArtifact struct {
	URI string `json:"uri"`
}
type sarifRegion struct {
	StartLine   int `json:"startLine,omitempty"`
	StartColumn int `json:"startColumn,omitempty"`
	EndLine     int `json:"endLine,omitempty"`
	EndColumn   int `json:"endColumn,omitempty"`
}
type sarifSuppression struct {
	Kind          string `json:"kind"`
	Justification string `json:"justification"`
}

// WriteSARIF emits SARIF 2.1.0 suitable for code-scanning integrations.
func WriteSARIF(w io.Writer, diagnostics []Diagnostic) error {
	ds := sanitized(diagnostics)
	ids := map[string]bool{}
	for _, d := range ds {
		ids[d.Rule] = true
	}
	names := make([]string, 0, len(ids))
	for id := range ids {
		names = append(names, id)
	}
	sort.Strings(names)
	rules := make([]sarifRule, 0, len(names))
	for _, id := range names {
		rules = append(rules, sarifRule{ID: id, ShortDescription: sarifMessage{Text: "AutoSQL database migration safety rule"}})
	}
	results := make([]sarifResult, 0, len(ds))
	for _, d := range ds {
		r := sarifResult{RuleID: d.Rule, Level: sarifLevel(d.Severity), Message: sarifMessage{Text: d.Message + " Impact: " + d.Impact + " Remediation: " + d.Remediation}, Properties: map[string]any{"objectId": d.Object.ID, "objectKind": d.Object.Kind, "confidence": d.Confidence}}
		if d.Source != nil {
			r.Locations = []sarifLocation{{PhysicalLocation: sarifPhysical{ArtifactLocation: sarifArtifact{URI: d.Source.URI}, Region: sarifRegion{StartLine: d.Source.Line, StartColumn: d.Source.Column, EndLine: d.Source.EndLine, EndColumn: d.Source.EndColumn}}}}
		}
		if d.Suppressed != nil {
			r.Suppressions = []sarifSuppression{{Kind: "external", Justification: d.Suppressed.Reason}}
		}
		results = append(results, r)
	}
	log := sarifLog{Version: "2.1.0", Schema: "https://json.schemastore.org/sarif-2.1.0.json", Runs: []sarifRun{{Tool: sarifTool{Driver: sarifDriver{Name: "AutoSQL", Rules: rules}}, Results: results}}}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(log)
}
func sarifLevel(s Severity) string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	default:
		return "note"
	}
}

// ReportString is convenient for CLI adapters and tests.
func ReportString(format string, ds []Diagnostic) (string, error) {
	var b bytes.Buffer
	var err error
	switch format {
	case "human":
		err = WriteHuman(&b, ds)
	case "json":
		err = WriteJSON(&b, ds)
	case "sarif":
		err = WriteSARIF(&b, ds)
	default:
		return "", fmt.Errorf("unsupported report format %q", format)
	}
	return b.String(), err
}
