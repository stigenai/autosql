package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"autosql/pkg/secret"
)

const OutputSchemaVersion = "autosql.output/v1"

type Streams struct {
	In       io.Reader
	Out, Err io.Writer
	IsTTY    func() bool
}

type Envelope struct {
	SchemaVersion string         `json:"schema_version"`
	Command       string         `json:"command"`
	OK            bool           `json:"ok"`
	Data          any            `json:"data,omitempty"`
	Error         *EnvelopeError `json:"error,omitempty"`
}

type EnvelopeError struct {
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	ExitCode int    `json:"exit_code"`
	Status   string `json:"status,omitempty"`
}

type output struct {
	streams  Streams
	json     bool
	command  string
	redactor *secret.Redactor
}

func (o output) success(data any, human string) error {
	human = sanitizeOutput(o.redactor, human)
	if o.json {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		var safe any
		if err := json.Unmarshal(raw, &safe); err != nil {
			return err
		}
		data = sanitizeJSON(o.redactor, safe)
		return json.NewEncoder(o.streams.Out).Encode(Envelope{SchemaVersion: OutputSchemaVersion, Command: o.command, OK: true, Data: data})
	}
	_, err := fmt.Fprintln(o.streams.Out, human)
	return err
}

func sanitizeJSON(redactor *secret.Redactor, value any) any {
	switch v := value.(type) {
	case string:
		return sanitizeOutput(redactor, v)
	case []any:
		for i := range v {
			v[i] = sanitizeJSON(redactor, v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = sanitizeJSON(redactor, v[k])
		}
	}
	return value
}

var (
	urlCredential    = regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:/@\s]+:)[^@/\s]+(@)`)
	secretAssignment = regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret)(\s*[=:]\s*)[^\s,;]+`)
)

func sanitizeOutput(redactor *secret.Redactor, value string) string {
	if redactor != nil {
		value = redactor.String(value)
	}
	value = urlCredential.ReplaceAllString(value, `${1}[REDACTED]${2}`)
	return secretAssignment.ReplaceAllString(value, `${1}${2}[REDACTED]`)
}

func (o output) failure(e *Error) {
	message := sanitizeOutput(o.redactor, e.Message)
	if o.json {
		_ = json.NewEncoder(o.streams.Out).Encode(Envelope{SchemaVersion: OutputSchemaVersion, Command: o.command, OK: false, Error: &EnvelopeError{Kind: e.Kind, Message: message, ExitCode: int(e.Code), Status: e.Status}})
		return
	}
	fmt.Fprintf(o.streams.Err, "autosql: %s: %s\n", e.Kind, message)
}
