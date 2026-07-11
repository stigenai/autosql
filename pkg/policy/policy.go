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
	"regexp"
	"sort"
	"strconv"
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
	MaxSteps           int
	MaxResources       int
	MaxPatternBytes    int
	MaxTextBytes       int
	MaxValueDepth      int
	MaxValueItems      int
	MaxValueBytes      int
	MaxNumericDigits   int
	MaxNumericExponent int
	MaxNumericBytes    int
	Timeout            time.Duration
}

const (
	defaultMaxPatternBytes    = 4096
	defaultMaxTextBytes       = 1 << 20
	defaultMaxValueDepth      = 32
	defaultMaxValueItems      = 10000
	defaultMaxValueBytes      = 1 << 20
	defaultMaxNumericDigits   = 1024
	defaultMaxNumericExponent = 1024
	defaultMaxNumericBytes    = 4096
	hardMaxNumericBytes       = 4096
)

type Evaluator struct {
	Limits Limits
	Now    func() time.Time
}

var ErrLimitExceeded = errors.New("policy evaluation limit exceeded")
var ErrUnsupportedValue = errors.New("unsupported policy value")

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
	dec.UseNumber()
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
	return validateBounded(doc, nil, defaultMaxPatternBytes)
}

func validateBounded(doc *Document, m *meter, maxPatternBytes int) error {
	step := func(n int) error {
		if m == nil {
			return nil
		}
		return m.charge(n)
	}
	if err := step(1); err != nil {
		return err
	}
	if doc.Version != LanguageVersion {
		return source("$.version", "unsupported language version %q", doc.Version)
	}
	seen := map[string]bool{}
	predicateNames := make([]string, 0, len(doc.Predicates))
	for name := range doc.Predicates {
		if err := step(1); err != nil {
			return err
		}
		predicateNames = append(predicateNames, name)
	}
	if err := step(1); err != nil {
		return err
	}
	sort.Strings(predicateNames)
	if err := step(1); err != nil {
		return err
	}
	for _, name := range predicateNames {
		if err := step(1); err != nil {
			return err
		}
		expr := doc.Predicates[name]
		if strings.TrimSpace(name) == "" {
			return source("$.predicates", "predicate names cannot be empty")
		}
		if err := validateExprBounded(expr, doc.Predicates, map[string]bool{name: true}, m, maxPatternBytes); err != nil {
			if errors.Is(err, ErrLimitExceeded) || (m != nil && m.ctx.Err() != nil) {
				return err
			}
			return source("$.predicates."+name, "%v", err)
		}
	}
	for i, rule := range doc.Rules {
		if err := step(1); err != nil {
			return err
		}
		path := fmt.Sprintf("$.rules[%d]", i)
		if rule.Name == "" || seen[rule.Name] {
			return source(path+".name", "rule name is empty or duplicated")
		}
		seen[rule.Name] = true
		if rule.Target != "schema" && rule.Target != "migration" && rule.Target != "all" {
			return source(path+".target", "target must be schema, migration, or all")
		}
		if err := step(1); err != nil {
			return err
		}
		for range rule.Kinds {
			if err := step(1); err != nil {
				return err
			}
		}
		if rule.Message == "" {
			return source(path+".message", "message is required")
		}
		if err := validateExprBounded(rule.Assert, doc.Predicates, nil, m, maxPatternBytes); err != nil {
			if errors.Is(err, ErrLimitExceeded) || (m != nil && m.ctx.Err() != nil) {
				return err
			}
			return source(path+".assert", "%v", err)
		}
	}
	return nil
}

func validateExpr(e Expression, predicates map[string]Expression, stack map[string]bool) error {
	return validateExprBounded(e, predicates, stack, nil, defaultMaxPatternBytes)
}

func validateExprBounded(e Expression, predicates map[string]Expression, stack map[string]bool, m *meter, maxPatternBytes int) error {
	if m != nil {
		if err := m.charge(1); err != nil {
			return err
		}
	}
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
			if err := validateExprBounded(x, predicates, stack, m, maxPatternBytes); err != nil {
				return err
			}
		}
	}
	if e.Not != nil {
		if err := validateExprBounded(*e.Not, predicates, stack, m, maxPatternBytes); err != nil {
			return err
		}
	}
	if e.Predicate != "" {
		if m != nil {
			if err := m.charge(1); err != nil {
				return err
			}
		}
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
		if err := validateExprBounded(p, predicates, next, m, maxPatternBytes); err != nil {
			return err
		}
	}
	if (e.Eq != nil && len(e.Eq) != 2) || (e.Ne != nil && len(e.Ne) != 2) || (e.In != nil && len(e.In) != 2) || (e.Matches != nil && len(e.Matches) != 2) {
		return fmt.Errorf("comparison operations require two operands")
	}
	if len(e.Matches) == 2 {
		if pattern, ok := e.Matches[1].(string); ok && !strings.HasPrefix(pattern, "resource.") && !strings.HasPrefix(pattern, "variables.") {
			if len(pattern) > maxPatternBytes {
				if m != nil {
					return fmt.Errorf("%w: regex pattern bytes (%d > %d)", ErrLimitExceeded, len(pattern), maxPatternBytes)
				}
				return fmt.Errorf("regular expression exceeds %d bytes", maxPatternBytes)
			}
			if m != nil {
				if err := m.charge(1); err != nil {
					return err
				}
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("invalid regular expression: %v", err)
			}
			if m != nil {
				if err := m.charge(1); err != nil {
					return err
				}
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
	if lim.MaxPatternBytes <= 0 {
		lim.MaxPatternBytes = defaultMaxPatternBytes
	}
	if lim.MaxTextBytes <= 0 {
		lim.MaxTextBytes = defaultMaxTextBytes
	}
	if lim.MaxValueDepth <= 0 {
		lim.MaxValueDepth = defaultMaxValueDepth
	}
	if lim.MaxValueItems <= 0 {
		lim.MaxValueItems = defaultMaxValueItems
	}
	if lim.MaxValueBytes <= 0 {
		lim.MaxValueBytes = defaultMaxValueBytes
	}
	if lim.MaxNumericDigits <= 0 {
		lim.MaxNumericDigits = defaultMaxNumericDigits
	}
	if lim.MaxNumericExponent <= 0 {
		lim.MaxNumericExponent = defaultMaxNumericExponent
	}
	if lim.MaxNumericBytes <= 0 || lim.MaxNumericBytes > hardMaxNumericBytes {
		lim.MaxNumericBytes = defaultMaxNumericBytes
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
	if err := validateBounded(&doc, m, lim.MaxPatternBytes); err != nil {
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
	ctx        context.Context
	limit      Limits
	start      time.Time
	now        func() time.Time
	steps      int
	valueItems int
	valueBytes int
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
		_, ok, err := resolve(e.Exists, r, vars, m)
		return ok, err
	}
	cmp := func(vs []any) (any, any, bool, error) {
		a, aok, err := resolve(vs[0], r, vars, m)
		if err != nil {
			return nil, nil, false, err
		}
		b, bok, err := resolve(vs[1], r, vars, m)
		if err != nil {
			return nil, nil, false, err
		}
		return a, b, aok && bok, nil
	}
	if e.Eq != nil {
		a, b, ok, err := cmp(e.Eq)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		return equalValue(m, a, b, 0)
	}
	if e.Ne != nil {
		a, b, ok, err := cmp(e.Ne)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		equal, err := equalValue(m, a, b, 0)
		return !equal, err
	}
	if e.In != nil {
		a, b, ok, err := cmp(e.In)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		switch x := b.(type) {
		case []any:
			for _, v := range x {
				if err := m.charge(1); err != nil {
					return false, err
				}
				equal, err := equalValue(m, a, v, 0)
				if err != nil {
					return false, err
				}
				if equal {
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
		a, b, ok, err := cmp(e.Matches)
		if err != nil {
			return false, err
		}
		text, textOK := a.(string)
		pattern, patternOK := b.(string)
		if !ok || !textOK || !patternOK {
			return false, nil
		}
		if len(pattern) > m.limit.MaxPatternBytes {
			return false, fmt.Errorf("%w: regex pattern bytes (%d > %d)", ErrLimitExceeded, len(pattern), m.limit.MaxPatternBytes)
		}
		if len(text) > m.limit.MaxTextBytes {
			return false, fmt.Errorf("%w: regex text bytes (%d > %d)", ErrLimitExceeded, len(text), m.limit.MaxTextBytes)
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

func resolve(v any, r Resource, vars map[string]any, m *meter) (any, bool, error) {
	s, ok := v.(string)
	if !ok {
		return v, true, validateValue(m, v, 0)
	}
	if strings.HasPrefix(s, "variables.") {
		name := strings.TrimPrefix(s, "variables.")
		if name == "" {
			return nil, false, nil
		}
		x, ok := vars[name]
		if !ok {
			return nil, false, nil
		}
		return x, true, validateValue(m, x, 0)
	}
	if s == "resource.kind" {
		return r.Kind, true, validateValue(m, r.Kind, 0)
	}
	if s == "resource.name" {
		return r.Name, true, validateValue(m, r.Name, 0)
	}
	if s == "resource.owner" {
		if r.Owner == "" {
			return nil, false, nil
		}
		return r.Owner, true, validateValue(m, r.Owner, 0)
	}
	if strings.HasPrefix(s, "resource.attributes.") {
		name := strings.TrimPrefix(s, "resource.attributes.")
		if name == "" {
			return nil, false, nil
		}
		x, ok := r.Attributes[name]
		if !ok {
			return nil, false, nil
		}
		return x, true, validateValue(m, x, 0)
	}
	if strings.HasPrefix(s, "resource.") {
		return nil, false, nil
	}
	return v, true, validateValue(m, v, 0)
}

func number(m *meter, v any) (*big.Rat, bool, error) {
	if err := m.charge(1); err != nil {
		return nil, false, err
	}
	switch n := v.(type) {
	case int:
		return new(big.Rat).SetInt64(int64(n)), true, nil
	case int8:
		return new(big.Rat).SetInt64(int64(n)), true, nil
	case int16:
		return new(big.Rat).SetInt64(int64(n)), true, nil
	case int32:
		return new(big.Rat).SetInt64(int64(n)), true, nil
	case int64:
		return new(big.Rat).SetInt64(n), true, nil
	case uint:
		return new(big.Rat).SetUint64(uint64(n)), true, nil
	case uint8:
		return new(big.Rat).SetUint64(uint64(n)), true, nil
	case uint16:
		return new(big.Rat).SetUint64(uint64(n)), true, nil
	case uint32:
		return new(big.Rat).SetUint64(uint64(n)), true, nil
	case uint64:
		return new(big.Rat).SetUint64(n), true, nil
	case float32:
		r := new(big.Rat).SetFloat64(float64(n))
		return r, r != nil, nil
	case float64:
		r := new(big.Rat).SetFloat64(n)
		return r, r != nil, nil
	case json.Number:
		s := string(n)
		if err := m.charge(1); err != nil {
			return nil, false, err
		}
		if len(s) > m.limit.MaxNumericBytes {
			return nil, false, fmt.Errorf("%w: numeric literal bytes (%d > %d)", ErrLimitExceeded, len(s), m.limit.MaxNumericBytes)
		}
		digits, exp, ok, err := numericShape(m, s)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, fmt.Errorf("%w: invalid number", ErrUnsupportedValue)
		}
		if digits > m.limit.MaxNumericDigits || exp > m.limit.MaxNumericExponent || digits+exp > m.limit.MaxNumericDigits {
			return nil, false, fmt.Errorf("%w: numeric literal exceeds bounds", ErrLimitExceeded)
		}
		if err := m.consumeValue(len(s)); err != nil {
			return nil, false, err
		}
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			return nil, false, fmt.Errorf("%w: invalid number", ErrUnsupportedValue)
		}
		return r, true, nil
	default:
		return nil, false, nil
	}
}

func (m *meter) consumeValue(bytes int) error {
	if err := m.charge(1); err != nil {
		return err
	}
	m.valueItems++
	m.valueBytes += bytes
	if m.valueItems > m.limit.MaxValueItems || m.valueBytes > m.limit.MaxValueBytes {
		return fmt.Errorf("%w: policy value resources", ErrLimitExceeded)
	}
	return nil
}

func validateValue(m *meter, v any, depth int) error {
	if depth > m.limit.MaxValueDepth {
		return fmt.Errorf("%w: policy value depth", ErrLimitExceeded)
	}
	if err := m.consumeValue(0); err != nil {
		return err
	}
	switch x := v.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return nil
	case json.Number:
		_, _, err := number(m, x)
		return err
	case string:
		return m.consumeValue(len(x))
	case []any:
		for _, item := range x {
			if err := validateValue(m, item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case []string:
		for _, item := range x {
			if err := validateValue(m, item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		keys := make([]string, 0, min(len(x), m.limit.MaxValueItems))
		for k := range x {
			if err := m.consumeValue(len(k)); err != nil {
				return err
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := validateValue(m, x[k], depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]string:
		keys := make([]string, 0, min(len(x), m.limit.MaxValueItems))
		for k := range x {
			if err := m.consumeValue(len(k) + len(x[k])); err != nil {
				return err
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		_ = keys
		return nil
	default:
		return fmt.Errorf("%w: %T", ErrUnsupportedValue, v)
	}
}

func equalValue(m *meter, a, b any, depth int) (bool, error) {
	if depth > m.limit.MaxValueDepth {
		return false, fmt.Errorf("%w: equality depth", ErrLimitExceeded)
	}
	if err := m.charge(1); err != nil {
		return false, err
	}
	af, aok, err := number(m, a)
	if err != nil {
		return false, err
	}
	bf, bok, err := number(m, b)
	if err != nil {
		return false, err
	}
	if aok || bok {
		return aok && bok && af.Cmp(bf) == 0, nil
	}
	switch x := a.(type) {
	case nil:
		return b == nil, nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y, nil
	case string:
		y, ok := b.(string)
		return ok && x == y, nil
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false, nil
		}
		for i := range x {
			eq, err := equalValue(m, x[i], y[i], depth+1)
			if err != nil || !eq {
				return eq, err
			}
		}
		return true, nil
	case []string:
		y, ok := b.([]string)
		if !ok || len(x) != len(y) {
			return false, nil
		}
		for i := range x {
			if err := m.consumeValue(len(x[i]) + len(y[i])); err != nil {
				return false, err
			}
			if x[i] != y[i] {
				return false, nil
			}
		}
		return true, nil
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false, nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := m.charge(1); err != nil {
				return false, err
			}
			yv, ok := y[k]
			if !ok {
				return false, nil
			}
			eq, err := equalValue(m, x[k], yv, depth+1)
			if err != nil || !eq {
				return eq, err
			}
		}
		return true, nil
	case map[string]string:
		y, ok := b.(map[string]string)
		if !ok || len(x) != len(y) {
			return false, nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := m.charge(1); err != nil {
				return false, err
			}
			if y[k] != x[k] {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("%w: %T", ErrUnsupportedValue, a)
	}
}

func numericShape(m *meter, s string) (digits, absExp int, ok bool, err error) {
	e := strings.IndexAny(s, "eE")
	mant, exp := s, ""
	if e >= 0 {
		mant, exp = s[:e], s[e+1:]
	}
	for i, c := range mant {
		if i%64 == 0 {
			if err := m.charge(1); err != nil {
				return 0, 0, false, err
			}
		}
		if c >= '0' && c <= '9' {
			digits++
		} else if c != '-' && c != '.' {
			return 0, 0, false, nil
		}
	}
	if digits == 0 {
		return 0, 0, false, nil
	}
	if exp != "" {
		if len(exp) > 1 && (exp[0] == '+' || exp[0] == '-') {
			exp = exp[1:]
		}
		if len(exp) == 0 || len(exp) > 6 {
			return 0, 0, false, nil
		}
		for i, c := range exp {
			if i%64 == 0 {
				if err := m.charge(1); err != nil {
					return 0, 0, false, err
				}
			}
			if c < '0' || c > '9' {
				return 0, 0, false, nil
			}
		}
		n, err := strconv.Atoi(exp)
		if err != nil {
			return 0, 0, false, nil
		}
		absExp = n
	}
	return digits, absExp, true, nil
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
