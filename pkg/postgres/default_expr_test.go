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
		{"(extract(epoch from now()) * 1000)::bigint", defaultExpressionCast, func(t *testing.T, got defaultExpression) {
			if got.Cast == nil || got.Cast.Expression.Kind != defaultExpressionOperator || got.Cast.Expression.Operator == nil || got.Cast.Expression.Operator.Name != "*" {
				t.Fatalf("operator cast=%+v", got.Cast)
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
		"1 ^ 2",
		"1 = 2",
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
		{"text", "1 + 2"},
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
		{"cidr", "'10.0.0.0/8'::cidr"},
		{"cidr", "'2001:db8::/32'"},
		{"inet", "'192.0.2.1/24'::inet"},
		{"inet", "'192.0.2.1'"},
		{"inet", "'2001:db8::1'::inet"},
		{"macaddr", "'08:00:2b:01:02:03'::macaddr"},
		{"cidr[]", "'{10.0.0.0/8,2001:db8::/32}'::cidr[]"},
		{"inet[]", "ARRAY['192.0.2.1'::inet, '2001:db8::1'::inet]"},
		{"macaddr[]", "'{08:00:2b:01:02:03,08:00:2b:01:02:04}'::macaddr[]"},
		{"date", "CURRENT_DATE"},
		{"time(3)", "LOCALTIME(3)"},
		{"timestamp(2)", "LOCALTIMESTAMP(2)"},
		{"interval", "'00:05:00'::interval"},
		{"numeric(10,2)", "'12.30'::numeric(4,2)"},
		{"bigint", "'12'::integer"},
		{"boolean", "'true'::boolean"},
		{"text", "'pending'::text"},
		{"character(4)", "'x'::character(1)"},
		{"character(4)", "'test'::character(4)"},
		{"bit(4)", "'1010'::bit(4)"},
		{"text[]", "'{}'::text[]"},
		{"text[]", "ARRAY[]::text[]"},
		{"text[]", `'{"a,b","NULL"}'::text[]`},
		{"integer[]", "'{1,2}'::integer[]"},
		{"boolean[]", "'{t,f}'::boolean[]"},
		{"text[]", "ARRAY['a'::text, 'b'::text]"},
		{"integer[]", "ARRAY[1, 2]"},
		{"uuid", "pg_catalog.gen_random_uuid()"},
		{"text", "pg_catalog.gen_random_uuid()::text"},
		{"timestamp", "pg_catalog.timezone('utc'::text, CURRENT_TIMESTAMP)"},
	}
	for _, tc := range tests {
		t.Run(tc.typ+"_"+tc.value, func(t *testing.T) {
			out, err := renderDocumentWithDefault(tc.typ, tc.value)
			if err != nil || len(out) == 0 {
				t.Fatalf("statements=%+v err=%v", out, err)
			}
			desired, err := New().Normalize(context.Background(), documentWithDefault(tc.typ, tc.value))
			if err != nil {
				t.Fatal(err)
			}
			empty := desired
			empty.Graph.Resources = nil
			fresh, err := plan.Build(context.Background(), New(), empty, desired, plan.Options{})
			if err != nil || len(fresh.Steps) == 0 {
				t.Fatalf("fresh plan=%+v err=%v", fresh, err)
			}
			current := desired
			current.Graph.Resources = nil
			for _, resource := range desired.Graph.Resources {
				if resource.Kind != schema.KindColumn {
					current.Graph.Resources = append(current.Graph.Resources, resource)
				}
			}
			incremental, err := plan.Build(context.Background(), New(), current, desired, plan.Options{})
			if err != nil || len(incremental.Steps) == 0 {
				t.Fatalf("incremental plan=%+v err=%v", incremental, err)
			}
		})
	}
}

func TestCoreDefaultClassifierRejectsUnsafeExtendedForms(t *testing.T) {
	tests := []struct{ typ, value string }{
		{"numeric(3,2)", "12.345"},
		{"numeric(3,2)", "100"},
		{"jsonb", "'{bad}'::jsonb"},
		{"uuid", "'not-a-uuid'::uuid"},
		{"cidr", "'10.0.0.1/8'::cidr"},
		{"cidr", "'10.0.0.0/33'::cidr"},
		{"inet", "'192.0.2.999'::inet"},
		{"inet", "'fe80::1%eth0'::inet"},
		{"inet", "'10.0.0.0/8'::cidr"},
		{"macaddr", "'08:00:2b:01:02'::macaddr"},
		{"macaddr", "'08:00:2b:01:02:03:04:05'::macaddr"},
		{"macaddr[]", "'{08:00:2b:01:02:03,invalid}'::macaddr[]"},
		{"date", "'2025-02-30'::date"},
		{"interval", "'00:99:00'::interval"},
		{"text", "'x'::integer"},
		{"text[]", "ARRAY[lower('x')]"},
		{"text[]", "ARRAY[]"},
		{"text[]", "ARRAY[1]"},
		{"text[]", "ARRAY[ARRAY['x']]"},
		{"text[]", "'{{x}}'::text[]"},
		{"text[]", "'{NULL}'::text[]"},
		{"text[]", `'{"unterminated}'::text[]`},
		{"text[]", `'{"a"junk}'::text[]`},
		{"uuid", "gen_random_uuid()"},
		{"uuid", "app.gen_random_uuid()"},
		{"timestamp", "pg_catalog.timezone('est'::text, CURRENT_TIMESTAMP)"},
		{"timestamp", "pg_catalog.timezone('utc'::text, app.clock())"},
		{"timestamp", "pg_catalog.timezone('utc'::text, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"},
		{"uuid", "pg_catalog.gen_random_uuid(1)"},
		{"text", "gen_random_uuid()::text"},
		{"text", "app.gen_random_uuid()::text"},
		{"text", "pg_catalog.gen_random_uuid(1)::text"},
		{"text", "pg_catalog.random()::text"},
		{"text", "lower('value')::text"},
		{"uuid", "pg_catalog.gen_random_uuid()::text"},
		{"integer", "random()"},
		{"text", "concat(VARIADIC ARRAY['x'])"},
		{"date", "LOCALTIME"},
		{"text[]", "ARRAY['x']; SELECT 1"},
		{"text[][]", "'{}'::text[][]"},
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
		"1 ^ 2",
		"(SELECT 1)",
		"CASE WHEN true THEN 1 ELSE 2 END",
		"ARRAY[1,2]",
		"gen_random_uuid()",
		"app.value",
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

func TestBoundedOperatorDefaultsCanonicalizeAndRender(t *testing.T) {
	tests := []struct {
		typ, input, canonical string
	}{
		{"integer", "1+2*3", "(1 OPERATOR(pg_catalog.+) (2 OPERATOR(pg_catalog.*) 3))"},
		{"integer", "(1+2)*3", "((1 OPERATOR(pg_catalog.+) 2) OPERATOR(pg_catalog.*) 3)"},
		{"integer", "10/2%3", "((10 OPERATOR(pg_catalog./) 2) OPERATOR(pg_catalog.%) 3)"},
		{"integer", "10-(2-1)", "(10 OPERATOR(pg_catalog.-) (2 OPERATOR(pg_catalog.-) 1))"},
		{"integer", "-(1+2)", "(OPERATOR(pg_catalog.-) (1 OPERATOR(pg_catalog.+) 2))"},
		{"integer", "+1", "(OPERATOR(pg_catalog.+) 1)"},
		{"numeric", "1.5 + 2::numeric", "(1.5 OPERATOR(pg_catalog.+) 2::numeric)"},
		{"numeric", "5.5 % 2", "(5.5 OPERATOR(pg_catalog.%) 2)"},
		{"smallint", "32766 + 1", "(32766 OPERATOR(pg_catalog.+) 1)"},
		{"bigint", "2147483647::bigint + 1", "(2147483647::bigint OPERATOR(pg_catalog.+) 1)"},
		{"real", "1::real + 2::real", "(1::real OPERATOR(pg_catalog.+) 2::real)"},
		{"bigint", "(extract(epoch from now()) * 1000)::bigint", "(extract(epoch from CURRENT_TIMESTAMP) OPERATOR(pg_catalog.*) 1000)::bigint"},
		{"bigint", "((EXTRACT(epoch FROM now()) * (1000)::numeric))::bigint", "(extract(epoch from CURRENT_TIMESTAMP) OPERATOR(pg_catalog.*) 1000::numeric)::bigint"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			if got := postgresDefault(tc.input); got != tc.canonical {
				t.Fatalf("postgresDefault(%q)=%q want %q", tc.input, got, tc.canonical)
			}
			statements, err := renderDocumentWithDefault(tc.typ, tc.canonical)
			if err != nil || len(statements) == 0 {
				t.Fatalf("statements=%+v err=%v", statements, err)
			}
			if !strings.Contains(statements[len(statements)-1], " DEFAULT "+tc.canonical) {
				t.Fatalf("rendered statement=%q", statements[len(statements)-1])
			}
		})
	}
}

func TestBoundedOperatorDefaultsFailClosed(t *testing.T) {
	tests := []struct{ typ, value, diagnostic string }{
		{"integer", "1 / 0", "divisor evaluates to zero"},
		{"integer", "1 % 0.0", "divisor evaluates to zero"},
		{"integer", "1 / (2 - 2)", "divisor evaluates to zero"},
		{"integer", "1 % (3 * 0)", "divisor evaluates to zero"},
		{"integer", "1 + app.secret", "not numeric"},
		{"integer", "1 + lower('2')", "not allowlisted"},
		{"integer", "1 + random()", "not allowlisted"},
		{"integer", "1 + (SELECT secret FROM app.secrets)", "unsupported AST node"},
		{"integer", "1 || 2", "operator is not allowlisted"},
		{"integer", "1 OPERATOR(public.+) 2", "operator is not allowlisted"},
		{"integer", "extract(day from now()) + 1", "not allowlisted"},
		{"integer", "extract(epoch from clock_timestamp()) + 1", "not allowlisted"},
		{"integer", "pg_catalog.extract('epoch', now()) + 1", "not allowlisted"},
		{"numeric", "1e100 + 1", "canonical finite literal"},
		{"text", "1 + 2", "not a numeric core type"},
		{"integer", "1 / (1 / 2)", "divisor evaluates to zero"},
		{"integer", "1 / (0.4::integer)", "divisor evaluates to zero"},
		{"integer", "1 / (0.4::numeric(1,0))", "divisor evaluates to zero"},
		{"real", "1::real / (0.00000000000000000000000000000000000000000000000001::real)", "divisor evaluates to zero"},
		{"real", "1::real % 1::real", "modulo does not support"},
		{"smallint", "32767 + 1", "outside PostgreSQL type bounds"},
		{"integer", "2147483647 + 1", "outside PostgreSQL type bounds"},
		{"bigint", "(2147483647 + 1)::bigint", "outside PostgreSQL type bounds"},
		{"real", "1::real / ((16777216::real + 1::real) - 16777216::real)", "divisor evaluates to zero"},
		{"integer", "(-2147483648)::integer % (-1)::integer", "minimum modulo -1"},
		{"bigint", "(-9223372036854775808)::bigint % (-1)::bigint", "minimum modulo -1"},
		{"integer", "(-2147483648::integer) % (-1::integer)", "outside PostgreSQL type bounds"},
		{"bigint", "(-9223372036854775808::bigint) % (-1::bigint)", "outside PostgreSQL type bounds"},
		{"smallint", "extract(epoch from now()) + 0", "outside PostgreSQL type bounds"},
		{"numeric", "1 / extract(epoch from now())", "divisor range includes zero"},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			statements, err := renderDocumentWithDefault(tc.typ, tc.value)
			if err == nil || len(statements) != 0 {
				t.Fatalf("statements=%+v err=%v", statements, err)
			}
			if !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("diagnostic=%q want substring %q", err, tc.diagnostic)
			}
		})
	}
}

func TestBoundedOperatorDefaultNormalizationIsDeterministic(t *testing.T) {
	forms := []string{
		"(extract(epoch from now()) * 1000)::bigint",
		"((EXTRACT(epoch FROM transaction_timestamp()) * (1000)::bigint))::bigint",
	}
	want := []string{
		"(extract(epoch from CURRENT_TIMESTAMP) OPERATOR(pg_catalog.*) 1000)::bigint",
		"(extract(epoch from CURRENT_TIMESTAMP) OPERATOR(pg_catalog.*) 1000::bigint)::bigint",
	}
	for i, input := range forms {
		first := postgresDefault(input)
		second := postgresDefault(first)
		if first != want[i] || second != first {
			t.Fatalf("input=%q first=%q second=%q want=%q", input, first, second, want[i])
		}
	}
	a, err := New().Normalize(context.Background(), documentWithDefault("bigint", "(extract(epoch from now())*1000)::int8"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New().Normalize(context.Background(), documentWithDefault("bigint", "((EXTRACT(epoch FROM transaction_timestamp()) * 1000))::bigint"))
	if err != nil {
		t.Fatal(err)
	}
	af, err := schema.SemanticFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	bf, err := schema.SemanticFingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if af != bf {
		t.Fatalf("equivalent normalized defaults have different fingerprints: %s != %s", af, bf)
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
		return expr.Literal != nil && expr.Cast == nil && expr.Function == nil && expr.Operator == nil && expr.Reference == nil && expr.Array == nil && expr.Literal.Kind != defaultLiteralInvalid
	case defaultExpressionCast:
		return expr.Literal == nil && expr.Cast != nil && expr.Function == nil && expr.Operator == nil && expr.Reference == nil && expr.Array == nil && len(expr.Cast.Type.Name.Parts) > 0 && validDefaultExpressionShape(expr.Cast.Expression, depth+1)
	case defaultExpressionFunction:
		if expr.Literal != nil || expr.Cast != nil || expr.Function == nil || expr.Operator != nil || expr.Reference != nil || expr.Array != nil || len(expr.Function.Name.Parts) == 0 {
			return false
		}
		for _, arg := range expr.Function.Arguments {
			if !validDefaultExpressionShape(arg, depth+1) {
				return false
			}
		}
		return true
	case defaultExpressionOperator:
		if expr.Literal != nil || expr.Cast != nil || expr.Function != nil || expr.Operator == nil || expr.Reference != nil || expr.Array != nil || !isBoundedDefaultOperator(expr.Operator.Name) {
			return false
		}
		if expr.Operator.Left != nil && !validDefaultExpressionShape(*expr.Operator.Left, depth+1) {
			return false
		}
		return validDefaultExpressionShape(expr.Operator.Right, depth+1)
	case defaultExpressionReference:
		return expr.Literal == nil && expr.Cast == nil && expr.Function == nil && expr.Operator == nil && expr.Reference != nil && expr.Array == nil && len(expr.Reference.Parts) > 0
	case defaultExpressionArray:
		if expr.Literal != nil || expr.Cast != nil || expr.Function != nil || expr.Operator != nil || expr.Reference != nil || expr.Array == nil {
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
