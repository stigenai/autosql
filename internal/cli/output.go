package cli

import (
	"encoding/json"
	"fmt"
	"io"

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
	if o.json {
		return json.NewEncoder(o.streams.Out).Encode(Envelope{SchemaVersion: OutputSchemaVersion, Command: o.command, OK: true, Data: data})
	}
	_, err := fmt.Fprintln(o.streams.Out, human)
	return err
}

func (o output) failure(e *Error) {
	message := e.Message
	if o.redactor != nil {
		message = o.redactor.String(message)
	}
	if o.json {
		_ = json.NewEncoder(o.streams.Out).Encode(Envelope{SchemaVersion: OutputSchemaVersion, Command: o.command, OK: false, Error: &EnvelopeError{Kind: e.Kind, Message: message, ExitCode: int(e.Code), Status: e.Status}})
		return
	}
	fmt.Fprintf(o.streams.Err, "autosql: %s: %s\n", e.Kind, message)
}
