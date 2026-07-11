// Package policy implements a bounded, declarative policy language for schema
// objects and migration operations. Policies are JSON documents; callers adapt
// their domain models to Resource so this package does not depend on a planner.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const LanguageVersion = "v1"

// Resource is one schema object or migration operation exposed to policy.
type Resource struct {
	Kind       string         `json:"kind"`
	Name       string         `json:"name"`
	Owner      string         `json:"owner,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type Document struct {
	Version    string                `json:"version"`
	Variables  map[string]any        `json:"variables,omitempty"`
	Predicates map[string]Expression `json:"predicates,omitempty"`
	Rules      []Rule                `json:"rules"`
}

type RulePack struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	Policies Document `json:"policies"`
}

type Rule struct {
	Name     string     `json:"name"`
	Target   string     `json:"target"` // schema, migration, or all
	Kinds    []string   `json:"kinds,omitempty"`
	Assert   Expression `json:"assert"`
	Message  string     `json:"message"`
	Severity string     `json:"severity,omitempty"`
}

// Expression is a compact JSON AST. Exactly one operation is required.
// Operands are values such as "resource.name", "resource.owner",
// "resource.attributes.encrypted", "variables.team", or JSON literals.
type Expression struct {
	All       []Expression `json:"all,omitempty"`
	Any       []Expression `json:"any,omitempty"`
	Not       *Expression  `json:"not,omitempty"`
	Predicate string       `json:"predicate,omitempty"`
	Eq        []any        `json:"eq,omitempty"`
	Ne        []any        `json:"ne,omitempty"`
	In        []any        `json:"in,omitempty"`
	Matches   []any        `json:"matches,omitempty"`
	Exists    any          `json:"exists,omitempty"`
}

type SourceError struct {
	Path   string
	Line   int
	Column int
	Msg    string
}

func (e *SourceError) Error() string {
	return fmt.Sprintf("policy %s at %d:%d: %s", e.Path, e.Line, e.Column, e.Msg)
}

type Violation struct {
	Rule, Target, Kind, Name, Message, Severity string
}

type Limits struct {
	MaxSteps     int
	MaxResources int
	Timeout      time.Duration
}

type Evaluator struct {
	Limits Limits
	Now    func() time.Time
}

var ErrLimitExceeded = errors.New("policy evaluation limit exceeded")

func Parse(data []byte) (*Document, error) {
	var doc Document
	if err := strictUnmarshal(data, &doc); err != nil {
		return nil, err
	}
	if err := validate(&doc); err != nil {
		if se, ok := err.(*SourceError); ok {
			se.Line, se.Column = locate(data, se.Path)
		}
		return nil, err
	}
	return &doc, nil
}

func ParsePack(data []byte) (*RulePack, error) {
	var pack RulePack
	if err := strictUnmarshal(data, &pack); err != nil {
		return nil, err
	}
	if strings.TrimSpace(pack.Name) == "" {
		line, col := locate(data, "$.name")
		return nil, &SourceError{Path: "$.name", Line: line, Column: col, Msg: "rule pack name is required"}
	}
	if strings.TrimSpace(pack.Version) == "" {
		line, col := locate(data, "$.version")
		return nil, &SourceError{Path: "$.version", Line: line, Column: col, Msg: "rule pack version is required"}
	}
	if err := validate(&pack.Policies); err != nil {
		if se, ok := err.(*SourceError); ok {
			se.Path = "$.policies" + strings.TrimPrefix(se.Path, "$")
			se.Line, se.Column = locate(data, se.Path)
		}
		return nil, err
	}
	return &pack, nil
}

func strictUnmarshal(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		offset := int(dec.InputOffset())
		path := "$"
		var syntax *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			offset = int(syntax.Offset)
		case errors.As(err, &typeErr):
			offset = int(typeErr.Offset)
			if typeErr.Field != "" {
				path = "$." + typeErr.Field
			}
		default:
			const prefix = "json: unknown field \""
			if strings.HasPrefix(err.Error(), prefix) {
				field := strings.TrimSuffix(strings.TrimPrefix(err.Error(), prefix), "\"")
				path = "$." + field
				if i := bytes.Index(data, []byte(`"`+field+`"`)); i >= 0 {
					offset = i + 1
				}
			}
		}
		if root, parseErr := parseJSONNode(data); parseErr == nil {
			if exact := root.pathAt(offset-1, "$"); exact != "$" {
				path = exact
				if keyOffset, ok := root.pathOffset(path); ok {
					offset = keyOffset + 1
				}
			}
		}
		line, col := position(data, offset)
		return &SourceError{Path: path, Line: line, Column: col, Msg: err.Error()}
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		line, col := position(data, int(dec.InputOffset()))
		if err == nil {
			err = errors.New("multiple JSON values are not allowed")
		}
		return &SourceError{Path: "$", Line: line, Column: col, Msg: err.Error()}
	}
	return nil
}

func validate(doc *Document) error {
	if doc.Version != LanguageVersion {
		return source("$.version", "unsupported language version %q", doc.Version)
	}
	seen := map[string]bool{}
	predicateNames := make([]string, 0, len(doc.Predicates))
	for name := range doc.Predicates {
		predicateNames = append(predicateNames, name)
	}
	sort.Strings(predicateNames)
	for _, name := range predicateNames {
		expr := doc.Predicates[name]
		if strings.TrimSpace(name) == "" {
			return source("$.predicates", "predicate names cannot be empty")
		}
		if err := validateExpr(expr, doc.Predicates, map[string]bool{name: true}); err != nil {
			return source("$.predicates."+name, "%v", err)
		}
	}
	for i, rule := range doc.Rules {
		path := fmt.Sprintf("$.rules[%d]", i)
		if rule.Name == "" || seen[rule.Name] {
			return source(path+".name", "rule name is empty or duplicated")
		}
		seen[rule.Name] = true
		if rule.Target != "schema" && rule.Target != "migration" && rule.Target != "all" {
			return source(path+".target", "target must be schema, migration, or all")
		}
		if rule.Message == "" {
			return source(path+".message", "message is required")
		}
		if err := validateExpr(rule.Assert, doc.Predicates, nil); err != nil {
			return source(path+".assert", "%v", err)
		}
	}
	return nil
}

func validateExpr(e Expression, predicates map[string]Expression, stack map[string]bool) error {
	n := 0
	if len(e.All) > 0 {
		n++
	}
	if len(e.Any) > 0 {
		n++
	}
	if e.Not != nil {
		n++
	}
	if e.Predicate != "" {
		n++
	}
	if e.Eq != nil {
		n++
	}
	if e.Ne != nil {
		n++
	}
	if e.In != nil {
		n++
	}
	if e.Matches != nil {
		n++
	}
	if e.Exists != nil {
		n++
	}
	if n != 1 {
		return fmt.Errorf("expression must contain exactly one operation")
	}
	for _, list := range [][]Expression{e.All, e.Any} {
		for _, x := range list {
			if err := validateExpr(x, predicates, stack); err != nil {
				return err
			}
		}
	}
	if e.Not != nil {
		if err := validateExpr(*e.Not, predicates, stack); err != nil {
			return err
		}
	}
	if e.Predicate != "" {
		p, ok := predicates[e.Predicate]
		if !ok {
			return fmt.Errorf("unknown predicate %q", e.Predicate)
		}
		if stack != nil && stack[e.Predicate] {
			return fmt.Errorf("predicate cycle involving %q", e.Predicate)
		}
		next := map[string]bool{}
		for k, v := range stack {
			next[k] = v
		}
		next[e.Predicate] = true
		if err := validateExpr(p, predicates, next); err != nil {
			return err
		}
	}
	if (e.Eq != nil && len(e.Eq) != 2) || (e.Ne != nil && len(e.Ne) != 2) || (e.In != nil && len(e.In) != 2) || (e.Matches != nil && len(e.Matches) != 2) {
		return fmt.Errorf("comparison operations require two operands")
	}
	if len(e.Matches) == 2 {
		if pattern, ok := e.Matches[1].(string); ok && !strings.HasPrefix(pattern, "resource.") && !strings.HasPrefix(pattern, "variables.") {
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("invalid regular expression: %v", err)
			}
		}
	}
	return nil
}

func (ev Evaluator) Evaluate(ctx context.Context, doc Document, schema, migrations []Resource) ([]Violation, error) {
	lim := ev.Limits
	if lim.MaxSteps <= 0 {
		lim.MaxSteps = 100000
	}
	if lim.MaxResources <= 0 {
		lim.MaxResources = 10000
	}
	if lim.Timeout <= 0 {
		lim.Timeout = 5 * time.Second
	}
	if len(schema)+len(migrations) > lim.MaxResources {
		return nil, fmt.Errorf("%w: resources (%d > %d)", ErrLimitExceeded, len(schema)+len(migrations), lim.MaxResources)
	}
	now := ev.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	m := &meter{ctx: ctx, limit: lim, start: start, now: now}
	if err := validate(&doc); err != nil {
		return nil, err
	}
	if err := m.charge(1 + len(doc.Rules) + len(doc.Predicates)); err != nil {
		return nil, err
	}
	var out []Violation
	sets := []struct {
		target string
		rs     []Resource
	}{{"schema", schema}, {"migration", migrations}}
	for _, set := range sets {
		for _, r := range set.rs {
			if err := m.charge(1); err != nil {
				return nil, err
			}
			for _, rule := range doc.Rules {
				if err := m.charge(1); err != nil { // rule traversal
					return nil, err
				}
				if err := m.charge(1); err != nil { // target comparison
					return nil, err
				}
				if rule.Target != "all" && rule.Target != set.target {
					continue
				}
				kindOK, err := meteredContains(m, rule.Kinds, r.Kind)
				if err != nil {
					return nil, err
				}
				if len(rule.Kinds) > 0 && !kindOK {
					continue
				}
				ok, err := eval(rule.Assert, r, doc.Variables, doc.Predicates, m)
				if err != nil {
					return nil, err
				}
				if !ok {
					sev := rule.Severity
					if sev == "" {
						sev = "error"
					}
					out = append(out, Violation{rule.Name, set.target, r.Kind, r.Name, rule.Message, sev})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule == out[j].Rule {
			return out[i].Name < out[j].Name
		}
		return out[i].Rule < out[j].Rule
	})
	return out, nil
}

type meter struct {
	ctx   context.Context
	limit Limits
	start time.Time
	now   func() time.Time
	steps int
}

func (m *meter) charge(n int) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	if m.now().Sub(m.start) > m.limit.Timeout {
		return fmt.Errorf("%w: timeout", ErrLimitExceeded)
	}
	m.steps += n
	if m.steps > m.limit.MaxSteps {
		return fmt.Errorf("%w: steps (%d > %d)", ErrLimitExceeded, m.steps, m.limit.MaxSteps)
	}
	return nil
}

func eval(e Expression, r Resource, vars map[string]any, preds map[string]Expression, m *meter) (bool, error) {
	if err := m.charge(1); err != nil {
		return false, err
	}
	if e.Predicate != "" {
		if err := m.charge(1); err != nil {
			return false, err
		}
		return eval(preds[e.Predicate], r, vars, preds, m)
	}
	if len(e.All) > 0 {
		for _, x := range e.All {
			ok, err := eval(x, r, vars, preds, m)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	}
	if len(e.Any) > 0 {
		for _, x := range e.Any {
			ok, err := eval(x, r, vars, preds, m)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	if e.Not != nil {
		ok, err := eval(*e.Not, r, vars, preds, m)
		return !ok, err
	}
	if e.Exists != nil {
		_, ok := resolve(e.Exists, r, vars)
		return ok, nil
	}
	cmp := func(vs []any) (any, any, bool) {
		a, aok := resolve(vs[0], r, vars)
		b, bok := resolve(vs[1], r, vars)
		return a, b, aok && bok
	}
	if e.Eq != nil {
		a, b, ok := cmp(e.Eq)
		return ok && equalValue(a, b), nil
	}
	if e.Ne != nil {
		a, b, ok := cmp(e.Ne)
		return ok && !equalValue(a, b), nil
	}
	if e.In != nil {
		a, b, ok := cmp(e.In)
		if !ok {
			return false, nil
		}
		switch x := b.(type) {
		case []any:
			for _, v := range x {
				if err := m.charge(1); err != nil {
					return false, err
				}
				if equalValue(a, v) {
					return true, nil
				}
			}
		case []string:
			as, ok := a.(string)
			if !ok {
				return false, nil
			}
			return meteredContains(m, x, as)
		}
		return false, nil
	}
	if e.Matches != nil {
		a, b, ok := cmp(e.Matches)
		text, textOK := a.(string)
		pattern, patternOK := b.(string)
		if !ok || !textOK || !patternOK {
			return false, nil
		}
		if err := m.charge(1); err != nil {
			return false, err
		}
		rx, err := regexp.Compile(pattern)
		if err != nil {
			return false, err
		}
		if err := m.charge(1); err != nil {
			return false, err
		}
		matched := rx.MatchString(text)
		if err := m.charge(1); err != nil {
			return false, err
		}
		return matched, nil
	}
	return false, nil
}

func resolve(v any, r Resource, vars map[string]any) (any, bool) {
	s, ok := v.(string)
	if !ok {
		return v, true
	}
	if strings.HasPrefix(s, "variables.") {
		name := strings.TrimPrefix(s, "variables.")
		if name == "" {
			return nil, false
		}
		x, ok := vars[name]
		return x, ok
	}
	if s == "resource.kind" {
		return r.Kind, true
	}
	if s == "resource.name" {
		return r.Name, true
	}
	if s == "resource.owner" {
		return r.Owner, r.Owner != ""
	}
	if strings.HasPrefix(s, "resource.attributes.") {
		name := strings.TrimPrefix(s, "resource.attributes.")
		if name == "" {
			return nil, false
		}
		x, ok := r.Attributes[name]
		return x, ok
	}
	if strings.HasPrefix(s, "resource.") {
		return nil, false
	}
	return v, true
}

func equalValue(a, b any) bool {
	if af, ok := number(a); ok {
		bf, bok := number(b)
		return bok && af.Cmp(bf) == 0
	}
	if _, ok := number(b); ok {
		return false
	}
	return reflect.TypeOf(a) == reflect.TypeOf(b) && reflect.DeepEqual(a, b)
}

func number(v any) (*big.Rat, bool) {
	switch n := v.(type) {
	case int:
		return new(big.Rat).SetInt64(int64(n)), true
	case int8:
		return new(big.Rat).SetInt64(int64(n)), true
	case int16:
		return new(big.Rat).SetInt64(int64(n)), true
	case int32:
		return new(big.Rat).SetInt64(int64(n)), true
	case int64:
		return new(big.Rat).SetInt64(n), true
	case uint:
		return new(big.Rat).SetUint64(uint64(n)), true
	case uint8:
		return new(big.Rat).SetUint64(uint64(n)), true
	case uint16:
		return new(big.Rat).SetUint64(uint64(n)), true
	case uint32:
		return new(big.Rat).SetUint64(uint64(n)), true
	case uint64:
		return new(big.Rat).SetUint64(n), true
	case float32:
		r := new(big.Rat).SetFloat64(float64(n))
		return r, r != nil
	case float64:
		r := new(big.Rat).SetFloat64(n)
		return r, r != nil
	default:
		return nil, false
	}
}

func meteredContains(m *meter, xs []string, s string) (bool, error) {
	for _, x := range xs {
		if err := m.charge(1); err != nil {
			return false, err
		}
		if x == s {
			return true, nil
		}
	}
	return false, nil
}
func source(path, format string, args ...any) error {
	return &SourceError{Path: path, Line: 1, Column: 1, Msg: fmt.Sprintf(format, args...)}
}
func position(data []byte, offset int) (int, int) {
	line, col := 1, 1
	for i, b := range data {
		if i >= offset-1 {
			break
		}
		if b == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}
func locate(data []byte, path string) (int, int) {
	root, err := parseJSONNode(data)
	if err != nil {
		return 1, 1
	}
	offset, ok := root.pathOffset(path)
	if !ok {
		return 1, 1
	}
	return position(data, offset+1)
}
