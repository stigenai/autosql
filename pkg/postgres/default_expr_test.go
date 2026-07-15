package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"autosql/pkg/plan"
	"autosql/pkg/schema"
)

func TestClassifyDefaultExpressionTypedForms(t *testing.T) {
	tests := []struct {
		input string
		kind  defaultExpressionKind
		check func(*testing.T, defaultExpression)
	}{
		{"-9223372036854775808", defaultExpressionLiteral, func(t *testing.T, got defaultExpression) {
			if got.Literal.Kind != defaultLiteralFloat || got.Literal.Text != "-9223372036854775808" {
				t.Fatalf("literal=%+v", got.Literal)
			}
		}},
		{"'pending'::job_status", defaultExpressionCast, func(t *testing.T, got defaultExpression) {
			if got.Cast.Expression.Literal.Text != "pending" || strings.Join(got.Cast.Type.Name.Parts, ".") != "job_status" {
				t.Fatalf("cast=%+v", got.Cast)
			}
		}},
		{"'{}'::integer[]", defaultExpressionCast, func(t *testing.T, got defaultExpression) {
			if got.Cast.Type.ArrayBounds != 1 || strings.Join(got.Cast.Type.Name.Parts, ".") != "pg_catalog.int4" {
				t.Fatalf("array cast=%+v", got.Cast)
			}
		}},
		{"gen_random_uuid()", defaultExpressionFunction, func(t *testing.T, got defaultExpression) {
			if strings.Join(got.Function.Name.Parts, ".") != "gen_random_uuid" || len(got.Function.Arguments) != 0 {
				t.Fatalf("function=%+v", got.Function)
			}
		}},
		{"nextval('app.widgets_id_seq'::regclass)", defaultExpressionFunction, func(t *testing.T, got defaultExpression) {
			if strings.Join(got.Function.Name.Parts, ".") != "nextval" || len(got.Function.Arguments) != 1 || got.Function.Arguments[0].Kind != defaultExpressionCast {
				t.Fatalf("function=%+v", got.Function)
			}
		}},
		{"app.sequence_name", defaultExpressionReference, func(t *testing.T, got defaultExpression) {
			if strings.Join(got.Reference.Parts, ".") != "app.sequence_name" {
				t.Fatalf("reference=%+v", got.Reference)
			}
		}},
		{"CURRENT_TIMESTAMP(3)", defaultExpressionFunction, func(t *testing.T, got defaultExpression) {
			if strings.Join(got.Function.Name.Parts, ".") != "current_timestamp_n" || got.Function.Precision == nil || *got.Function.Precision != 3 {
				t.Fatalf("function=%+v", got.Function)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := classifyDefaultExpression(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != tc.kind {
				t.Fatalf("kind=%v want=%v expression=%+v", got.Kind, tc.kind, got)
			}
			tc.check(t, got)
		})
	}
}

func TestClassifyDefaultExpressionRejectsStatementAndUnknownShapes(t *testing.T) {
	inputs := []string{
		"1 FROM app.secrets",
		"1, 2",
		"1 AS value",
		"1 WHERE true",
		"1; SELECT 2",
		"1;",
		"1 -- trailing",
		"1 /* comment */",
		"$$secret$$",
		"$tag$secret$tag$",
		"$é$secret$é$",
		"E'secret'",
		"U&'secret'",
		"B'1010'",
		"X'ff'",
		"'{}'::integer[5]",
		"'{}'::integer[][]",
		"1 + 2",
		"(SELECT 1)",
		"CASE WHEN true THEN 1 ELSE 2 END",
		"current_user",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			if got, err := classifyDefaultExpression(input); err == nil {
				t.Fatalf("classified unsafe expression: %+v", got)
			}
		})
	}
}

func TestCoreDefaultClassifierPreservesLegacyGrammar(t *testing.T) {
	tests := []struct{ typ, value string }{
		{"smallint", "-32768"},
		{"integer", "2147483647"},
		{"bigint", "9223372036854775807"},
		{"boolean", "true"},
		{"boolean", "false"},
		{"text", "'it''s safe'"},
		{"text", "'a;b'"},
		{"text", "'--'"},
		{"text", "'/*'"},
		{"text", "'$$'"},
		{"character varying(255)", "'pending'"},
		{"timestamp", "CURRENT_TIMESTAMP"},
		{"timestamptz(3)", "CURRENT_TIMESTAMP(3)"},
	}
	for _, tc := range tests {
		t.Run(tc.typ+"_"+tc.value, func(t *testing.T) {
			out, err := renderDocumentWithDefault(tc.typ, tc.value)
			if err != nil || len(out) == 0 {
				t.Fatalf("statements=%d err=%v", len(out), err)
			}
		})
	}
}

func TestCoreDefaultClassifierPolicyDeniesRecognizedFutureForms(t *testing.T) {
	for _, fixture := range []struct{ typ, value string }{
		{"text", "lower('VALUE')"},
		{"integer", "nextval('app.seq'::regclass)"},
		{"text", "app.value"},
		{"integer", "1 + 2"},
		{"integer", "1; SELECT 2"},
		{"timestamp", "CURRENT_TIMESTAMP(03)"},
	} {
		t.Run(fixture.value, func(t *testing.T) {
			out, err := renderDocumentWithDefault(fixture.typ, fixture.value)
			if err == nil || len(out) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

func TestCoreDefaultClassifierSupportsInspectedScalarArrayAndBuiltinForms(t *testing.T) {
	tests := []struct{ typ, value string }{
		{"numeric(10,2)", "0.00"},
		{"jsonb", "'{}'::jsonb"},
		{"jsonb", "'[]'::jsonb"},
		{"uuid", "'550e8400-e29b-41d4-a716-446655440000'::uuid"},
		{"date", "CURRENT_DATE"},
		{"time(3)", "LOCALTIME(3)"},
		{"timestamp(2)", "LOCALTIMESTAMP(2)"},
		{"interval", "'00:05:00'::interval"},
		{"character(4)", "'x'::character(1)"},
		{"character(4)", "'test'::character(4)"},
		{"bit(4)", "'1010'::bit(4)"},
		{"text[]", "'{}'::text[]"},
		{"text[]", "ARRAY['a'::text, 'b'::text]"},
		{"integer[]", "ARRAY[1, 2]"},
		{"uuid", "pg_catalog.gen_random_uuid()"},
		{"timestamp", "pg_catalog.timezone('utc'::text, CURRENT_TIMESTAMP)"},
	}
	for _, tc := range tests {
		t.Run(tc.typ+"_"+tc.value, func(t *testing.T) {
			out, err := renderDocumentWithDefault(tc.typ, tc.value)
			if err != nil || len(out) == 0 {
				t.Fatalf("statements=%+v err=%v", out, err)
			}
		})
	}
}

func TestCoreDefaultClassifierRejectsUnsafeExtendedForms(t *testing.T) {
	tests := []struct{ typ, value string }{
		{"numeric(3,2)", "12.345"},
		{"jsonb", "'{bad}'::jsonb"},
		{"uuid", "'not-a-uuid'::uuid"},
		{"date", "'2025-02-30'::date"},
		{"text", "'x'::integer"},
		{"text[]", "ARRAY[lower('x')]"},
		{"text[]", "ARRAY[ARRAY['x']]"},
		{"text[]", "'{{x}}'::text[]"},
		{"text[]", "'{NULL}'::text[]"},
		{"uuid", "gen_random_uuid()"},
		{"uuid", "app.gen_random_uuid()"},
		{"timestamp", "pg_catalog.timezone('est'::text, CURRENT_TIMESTAMP)"},
		{"timestamp", "pg_catalog.timezone('utc'::text, app.clock())"},
	}
	for _, tc := range tests {
		t.Run(tc.typ+"_"+tc.value, func(t *testing.T) {
			out, err := renderDocumentWithDefault(tc.typ, tc.value)
			if err == nil || len(out) != 0 {
				t.Fatalf("statements=%+v err=%v", out, err)
			}
		})
	}
}

func TestCoreDefaultClassifierAdversarialInputsProduceNoStatements(t *testing.T) {
	inputs := []string{
		"1 FROM app.secrets",
		"1, 2",
		"1 AS value",
		"1 WHERE true",
		"1; SELECT 2",
		"1 -- trailing",
		"1 /* comment */",
		"$$secret$$",
		"$tag$secret$tag$",
		"$é$secret$é$",
		"E'secret'",
		"U&'secret'",
		"B'1010'",
		"X'ff'",
		"1 + 2",
		"(SELECT 1)",
		"CASE WHEN true THEN 1 ELSE 2 END",
		"ARRAY[1,2]",
		"gen_random_uuid()",
		"app.value",
		"'1'::integer",
		"'unterminated",
		"f(",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			out, err := renderDocumentWithDefault("integer", input)
			if err == nil || len(out) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			desired := documentWithDefault("integer", input)
			planned, planErr := plan.Build(context.Background(), New(), schema.Document{Version: schema.SchemaVersion}, desired, plan.Options{})
			if planErr == nil || len(planned.Steps) != 0 {
				t.Fatalf("plan steps=%+v err=%v", planned.Steps, planErr)
			}
		})
	}
}

func TestCoreDefaultClassifierRejectsNoncanonicalAndOutOfRangeValues(t *testing.T) {
	tests := []struct{ typ, value string }{
		{"smallint", "-32769"},
		{"smallint", "32768"},
		{"integer", "-2147483649"},
		{"integer", "2147483648"},
		{"bigint", "-9223372036854775809"},
		{"bigint", "9223372036854775808"},
		{"integer", "00"},
		{"integer", "01"},
		{"integer", "-0"},
		{"integer", "+1"},
		{"timestamp", "CURRENT_TIMESTAMP(7)"},
		{"timestamp", "CURRENT_TIMESTAMP(03)"},
		{"timestamp", "CURRENT_TIMESTAMP()"},
		{"timestamp", "CURRENT_TIMESTAMP(1,2)"},
	}
	for _, tc := range tests {
		t.Run(tc.typ+"_"+tc.value, func(t *testing.T) {
			out, err := renderDocumentWithDefault(tc.typ, tc.value)
			if err == nil || len(out) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

func TestClassifyDefaultExpressionLimits(t *testing.T) {
	if _, err := classifyDefaultExpression("'" + strings.Repeat("x", maxDefaultExpressionBytes) + "'"); err == nil {
		t.Fatal("oversized default classified")
	}
	deep := "1"
	for range maxDefaultExpressionDepth + 2 {
		deep = "f(" + deep + ")"
	}
	if _, err := classifyDefaultExpression(deep); err == nil {
		t.Fatal("deep default classified")
	}
	args := strings.Repeat("1,", maxDefaultFunctionArgs) + "1"
	if _, err := classifyDefaultExpression("f(" + args + ")"); err == nil {
		t.Fatal("function with too many arguments classified")
	}
}

func FuzzClassifyDefaultExpressionFailsClosed(f *testing.F) {
	for _, seed := range []string{"0", "-1", "true", "'text'", "CURRENT_TIMESTAMP", "'{}'::jsonb", "gen_random_uuid()", "1; DROP TABLE users", "/*x*/1", "$$x$$", "1 FROM secrets"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		got, err := classifyDefaultExpression(value)
		if err != nil {
			return
		}
		if !validDefaultExpressionShape(got, 0) {
			t.Fatalf("classifier returned an invalid typed shape for %q: %+v", value, got)
		}
		if validateDefaultLexicalForm(value) != nil {
			t.Fatalf("classifier accepted forbidden source %q", value)
		}
	})
}

func validDefaultExpressionShape(expr defaultExpression, depth int) bool {
	if depth > maxDefaultExpressionDepth {
		return false
	}
	switch expr.Kind {
	case defaultExpressionLiteral:
		return expr.Literal != nil && expr.Cast == nil && expr.Function == nil && expr.Reference == nil && expr.Array == nil && expr.Literal.Kind != defaultLiteralInvalid
	case defaultExpressionCast:
		return expr.Literal == nil && expr.Cast != nil && expr.Function == nil && expr.Reference == nil && expr.Array == nil && len(expr.Cast.Type.Name.Parts) > 0 && validDefaultExpressionShape(expr.Cast.Expression, depth+1)
	case defaultExpressionFunction:
		if expr.Literal != nil || expr.Cast != nil || expr.Function == nil || expr.Reference != nil || expr.Array != nil || len(expr.Function.Name.Parts) == 0 {
			return false
		}
		for _, arg := range expr.Function.Arguments {
			if !validDefaultExpressionShape(arg, depth+1) {
				return false
			}
		}
		return true
	case defaultExpressionReference:
		return expr.Literal == nil && expr.Cast == nil && expr.Function == nil && expr.Reference != nil && expr.Array == nil && len(expr.Reference.Parts) > 0
	case defaultExpressionArray:
		if expr.Literal != nil || expr.Cast != nil || expr.Function != nil || expr.Reference != nil || expr.Array == nil {
			return false
		}
		for _, element := range expr.Array {
			if !validDefaultExpressionShape(element, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func renderDocumentWithDefault(typ, value string) ([]string, error) {
	doc := documentWithDefault(typ, value)
	statements, err := RenderDocument(context.Background(), doc, nil)
	out := make([]string, len(statements))
	for i := range statements {
		out[i] = statements[i].SQL
	}
	return out, err
}

func documentWithDefault(typ, value string) schema.Document {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "widgets", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	raw := fmt.Sprintf(`{"type":%q,"default":%q,"not_null":false,"ordinal":1}`, typ, value)
	column := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "value", Parent: table.ID}, raw, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, column}}}
}
