package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

const maxProjectBytes = 4 << 20
const maxProjectNodes = 100000

type projectNode struct {
	value  any
	at     SourceRange
	keyAt  SourceRange
	fields map[string]*projectNode
	items  []*projectNode
}

type jsonProjectParser struct {
	data      []byte
	filename  string
	decoder   *json.Decoder
	nodes     int
	positions []sourcePoint
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func parseJSONProject(data []byte, filename string) (*Config, error) {
	p := &jsonProjectParser{data: data, filename: filename, decoder: json.NewDecoder(bytes.NewReader(data)), positions: sourcePositions(data)}
	p.decoder.UseNumber()
	node, err := p.parse("", 0)
	if err != nil {
		return nil, err
	}
	if _, err := p.decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, p.failure("", int(p.decoder.InputOffset()), "configuration must contain exactly one JSON object")
	}
	legacy := node.fields["version"] != nil && node.fields["version"].value == json.Number("1")
	if err := validateNode(node, reflect.TypeFor[Config](), "", legacy); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(node.value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var c Config
	if err := decoder.Decode(&c); err != nil {
		return nil, p.failure("", 0, "configuration has an invalid value")
	}
	c.path = filename
	c.locations = map[string]SourceRange{}
	c.origins = map[string]string{}
	collectNodeLocations(node, "", c.locations)
	if c.Version == LegacyVersion {
		if err := validateLegacyFields(node); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

func (p *jsonProjectParser) parse(field string, depth int) (*projectNode, error) {
	start := p.nextOffset()
	p.nodes++
	if depth > 32 || p.nodes > maxProjectNodes {
		return nil, p.failure(field, start, "project structure exceeds parsing limits")
	}
	token, err := p.decoder.Token()
	if err != nil {
		return nil, p.failure(field, start, "invalid JSON syntax")
	}
	n := &projectNode{value: token}
	switch token {
	case json.Delim('{'):
		if err := p.object(n, field, depth); err != nil {
			return nil, err
		}
	case json.Delim('['):
		if err := p.array(n, field, depth); err != nil {
			return nil, err
		}
	case json.Delim('}'), json.Delim(']'):
		return nil, p.failure(field, start, "unexpected JSON delimiter")
	}
	n.at = p.sourceRange(start, int(p.decoder.InputOffset()))
	return n, nil
}

func (p *jsonProjectParser) object(n *projectNode, field string, depth int) error {
	n.fields = map[string]*projectNode{}
	values := map[string]any{}
	for p.decoder.More() {
		start := p.nextOffset()
		token, err := p.decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return p.failure(field, start, "object field name must be a string")
		}
		childField := field + "/" + pointerPart(key)
		if _, exists := n.fields[key]; exists {
			return p.failure(childField, start, "duplicate object field")
		}
		keyAt := p.sourceRange(start, int(p.decoder.InputOffset()))
		child, err := p.parse(childField, depth+1)
		if err != nil {
			return err
		}
		child.keyAt = keyAt
		n.fields[key] = child
		values[key] = child.value
	}
	if _, err := p.decoder.Token(); err != nil {
		return p.failure(field, p.nextOffset(), "object is not closed")
	}
	n.value = values
	return nil
}

func (p *jsonProjectParser) array(n *projectNode, field string, depth int) error {
	values := []any{}
	for p.decoder.More() {
		child, err := p.parse(fmt.Sprintf("%s/%d", field, len(values)), depth+1)
		if err != nil {
			return err
		}
		n.items = append(n.items, child)
		values = append(values, child.value)
	}
	if _, err := p.decoder.Token(); err != nil {
		return p.failure(field, p.nextOffset(), "array is not closed")
	}
	n.value = values
	return nil
}

func (p *jsonProjectParser) nextOffset() int {
	offset := int(p.decoder.InputOffset())
	for offset < len(p.data) && strings.ContainsRune(" \t\r\n,:", rune(p.data[offset])) {
		offset++
	}
	return offset
}

type sourcePoint struct{ line, column int32 }

// Index positions once. Repeated prefix scans make a large single-line JSON
// object quadratic; the bounded input needs at most 32 MiB for this index.
func sourcePositions(data []byte) []sourcePoint {
	positions := make([]sourcePoint, len(data)+1)
	line, column := int32(1), int32(1)
	for offset := 0; offset < len(data); {
		_, size := utf8.DecodeRune(data[offset:])
		for i := 0; i < size; i++ {
			positions[offset+i] = sourcePoint{line, column}
		}
		if data[offset] == '\n' {
			line++
			column = 1
		} else {
			column++
		}
		offset += size
	}
	positions[len(data)] = sourcePoint{line, column}
	return positions
}

func (p *jsonProjectParser) sourceRange(start, end int) SourceRange {
	a := p.positions[min(max(start, 0), len(p.data))]
	b := p.positions[min(max(end, 0), len(p.data))]
	return SourceRange{Filename: p.filename, StartLine: int(a.line), StartColumn: int(a.column), EndLine: int(b.line), EndColumn: int(b.column)}
}

func (p *jsonProjectParser) failure(field string, offset int, message string) error {
	return &LocatedError{Code: "invalid_config", Field: field, Range: p.sourceRange(offset, offset+1), Message: message}
}

func nodeFailure(node *projectNode, field, message string) error {
	return &LocatedError{Code: "invalid_config", Field: field, Range: node.at, Message: message}
}

func validateNode(node *projectNode, typ reflect.Type, field string, legacy bool) error {
	// Version 1 used encoding/json directly, where null leaves these fields at
	// their zero values. Version 2 rejects null in typed configuration fields.
	if legacy && node.value == nil {
		return nil
	}
	switch typ.Kind() {
	case reflect.Interface:
		return nil
	case reflect.Struct:
		return validateStructNode(node, typ, field, legacy)
	case reflect.Map:
		return validateMapNode(node, typ, field, legacy)
	case reflect.Slice:
		return validateListNode(node, typ, field, legacy)
	default:
		return validateScalarNode(node, typ, field)
	}
}

func validateMapNode(node *projectNode, typ reflect.Type, field string, legacy bool) error {
	if node.fields == nil {
		return nodeFailure(node, field, "expected an object")
	}
	for _, key := range sortedKeys(node.fields) {
		if err := validateNode(node.fields[key], typ.Elem(), field+"/"+pointerPart(key), legacy); err != nil {
			return err
		}
	}
	return nil
}

func validateListNode(node *projectNode, typ reflect.Type, field string, legacy bool) error {
	if _, ok := node.value.([]any); !ok {
		return nodeFailure(node, field, "expected a list")
	}
	for i, child := range node.items {
		if err := validateNode(child, typ.Elem(), fmt.Sprintf("%s/%d", field, i), legacy); err != nil {
			return err
		}
	}
	return nil
}

func structFields(typ reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for _, field := range reflect.VisibleFields(typ) {
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields[name] = field.Type
		}
	}
	return fields
}

func validateStructNode(node *projectNode, typ reflect.Type, field string, legacy bool) error {
	if node.fields == nil {
		return nodeFailure(node, field, "expected an object")
	}
	allowed := structFields(typ)
	for _, name := range sortedKeys(node.fields) {
		child := node.fields[name]
		childField := field + "/" + pointerPart(name)
		childType, ok := allowed[name]
		if !ok {
			return &LocatedError{Code: "unknown_field", Field: childField, Range: child.keyAt, Message: "unknown configuration field"}
		}
		if err := validateNode(child, childType, childField, legacy); err != nil {
			return err
		}
	}
	return nil
}

func validateScalarNode(node *projectNode, typ reflect.Type, field string) error {
	valid := false
	switch typ.Kind() {
	case reflect.String:
		_, valid = node.value.(string)
	case reflect.Bool:
		_, valid = node.value.(bool)
	case reflect.Int:
		if number, ok := node.value.(json.Number); ok {
			_, err := number.Int64()
			valid = err == nil
		}
	}
	if !valid {
		return nodeFailure(node, field, "expected "+typ.Kind().String())
	}
	return nil
}

func collectNodeLocations(node *projectNode, field string, locations map[string]SourceRange) {
	locations[field] = node.at
	for key, child := range node.fields {
		collectNodeLocations(child, field+"/"+pointerPart(key), locations)
	}
	for i, child := range node.items {
		collectNodeLocations(child, fmt.Sprintf("%s/%d", field, i), locations)
	}
}

func validateLegacyFields(root *projectNode) error {
	allowed := map[string]bool{"version": true, "environment": true, "environments": true}
	for _, key := range sortedKeys(root.fields) {
		if !allowed[key] {
			return nodeFailure(root.fields[key], "/"+pointerPart(key), "field requires project version 2")
		}
	}
	environments := root.fields["environments"]
	if environments == nil {
		return nil
	}
	legacy := map[string]bool{"target": true, "dev_database": true, "schema_sources": true, "migration_dir": true, "revision_schema": true, "include": true, "exclude": true, "variables": true, "timeout": true}
	for _, name := range sortedKeys(environments.fields) {
		env := environments.fields[name]
		for _, key := range sortedKeys(env.fields) {
			if !legacy[key] {
				return nodeFailure(env.fields[key], environmentPath(name)+"/"+pointerPart(key), "field requires project version 2")
			}
		}
	}
	return nil
}
