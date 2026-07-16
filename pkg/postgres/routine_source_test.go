package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"autosql/pkg/schema"
)

func reviewedRoutine(t *testing.T, definition, language string, security bool, configuration []string) schema.Resource {
	t.Helper()
	specification := map[string]any{"name": "run", "identity_arguments": "value integer", "arguments": "value integer", "result": "integer", "returns_set": false, "language": language, "volatility": "v", "strict": false, "security_definer": security, "leakproof": false, "parallel": "u", "cost": 100.0, "rows": 0.0, "configuration": configuration, "owner": "postgres", "definition": definition}
	raw, _ := json.Marshal(specification)
	resource := schema.Resource{Kind: schema.KindFunction, Name: schema.Name{Schema: "app", Name: "run(value integer)"}, Spec: raw}
	resource.ID = schema.StableID(resource.Kind, resource.Name)
	doc, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{resource}}})
	if err != nil {
		t.Fatal(err)
	}
	return doc.Graph.Resources[0]
}

func TestRoutineSourceRequiresExactReviewDigest(t *testing.T) {
	routine := reviewedRoutine(t, `CREATE FUNCTION app.run(value integer) RETURNS integer LANGUAGE sql AS $$ SELECT value + 1 $$`, "sql", false, nil)
	digest := stringValue(spec(routine), "body_digest")
	if _, err := validateRoutineSource(routine, nil); err == nil {
		t.Fatal("unreviewed catalog/authored routine was executable")
	}
	if _, err := validateRoutineSource(routine, map[string]string{"reviewed_routine_digests": "sha256:wrong"}); err == nil {
		t.Fatal("mismatched digest was executable")
	}
	parsed, err := validateRoutineSource(routine, map[string]string{"reviewed_routine_digests": digest})
	if err != nil || parsed.statement == nil {
		t.Fatalf("reviewed routine=%+v err=%v", parsed, err)
	}
	tampered := routine
	tamperedValues := spec(tampered)
	tamperedValues["definition"] = strings.Replace(stringValue(tamperedValues, "definition"), "+ 1", "+ 2", 1)
	tampered.Spec, _ = json.Marshal(tamperedValues)
	if _, err := validateRoutineSource(tampered, map[string]string{"reviewed_routine_digests": digest}); err == nil {
		t.Fatal("tampered routine was executable")
	}
}

func TestRoutineDefinitionNormalizationIsCatalogWhitespaceStable(t *testing.T) {
	oneLine := reviewedRoutine(t, `CREATE FUNCTION app.run(value integer) RETURNS integer LANGUAGE sql AS $$ SELECT value + 1 $$`, "sql", false, nil)
	multiLine := reviewedRoutine(t, "CREATE FUNCTION app.run(value integer)\nRETURNS integer\nLANGUAGE sql\nAS $$ SELECT value + 1 $$", "sql", false, nil)
	if stringValue(spec(oneLine), "definition") != stringValue(spec(multiLine), "definition") || stringValue(spec(oneLine), "body_digest") != stringValue(spec(multiLine), "body_digest") {
		t.Fatalf("routine catalog whitespace was not canonicalized")
	}
}

func TestRoutineSourcePolicyRejectsHazardsBeforeSQL(t *testing.T) {
	cases := map[string]schema.Resource{
		"statement smuggling": reviewedRoutine(t, `CREATE FUNCTION app.run(value integer) RETURNS integer LANGUAGE sql AS $$ SELECT value $$; DROP TABLE app.users`, "sql", false, nil),
		"unsafe language":     reviewedRoutine(t, `CREATE FUNCTION app.run(value integer) RETURNS integer LANGUAGE plpython3u AS $$ return value $$`, "plpython3u", false, nil),
		"definer search path": reviewedRoutine(t, `CREATE FUNCTION app.run(value integer) RETURNS integer LANGUAGE sql SECURITY DEFINER AS $$ SELECT value $$`, "sql", true, nil),
		"secret literal":      reviewedRoutine(t, `CREATE FUNCTION app.run(value integer) RETURNS integer LANGUAGE plpgsql AS $$ BEGIN password := 'cleartext'; RETURN value; END $$`, "plpgsql", false, nil),
		"privileged body":     reviewedRoutine(t, `CREATE FUNCTION app.run(value integer) RETURNS integer LANGUAGE sql AS $$ SELECT pg_read_file('/etc/passwd')::integer $$`, "sql", false, nil),
	}
	for name, routine := range cases {
		t.Run(name, func(t *testing.T) {
			digest := stringValue(spec(routine), "body_digest")
			if _, err := validateRoutineSource(routine, map[string]string{"reviewed_routine_digests": digest}); err == nil {
				t.Fatal("hazardous routine was executable")
			}
		})
	}
	secure := reviewedRoutine(t, `CREATE FUNCTION app.run(value integer) RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path TO pg_catalog, app AS $$ BEGIN RETURN value; END $$`, "plpgsql", true, []string{"search_path=pg_catalog, app"})
	if _, err := validateRoutineSource(secure, map[string]string{"reviewed_routine_digests": stringValue(spec(secure), "body_digest")}); err != nil {
		t.Fatalf("bounded PL/pgSQL routine rejected: %v", err)
	}
}

func TestProcedureTransactionControlRequiresExplicitAuthority(t *testing.T) {
	definition := `CREATE PROCEDURE app.run(value integer) LANGUAGE plpgsql AS $$ BEGIN COMMIT; END $$`
	routine := reviewedRoutine(t, definition, "plpgsql", false, nil)
	routine.Kind = schema.KindProcedure
	routine.ID = schema.StableID(routine.Kind, routine.Name)
	digest := stringValue(spec(routine), "body_digest")
	options := map[string]string{"reviewed_routine_digests": digest}
	if _, err := validateRoutineSource(routine, options); err == nil {
		t.Fatal("procedure transaction control was accepted without authority")
	}
	options["allow_transaction_control_procedures"] = "true"
	if _, err := validateRoutineSource(routine, options); err != nil {
		t.Fatalf("authorized transaction control rejected: %v", err)
	}
}
