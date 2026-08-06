package source

import (
	"encoding/json"
	"strings"
	"testing"

	"autosql/pkg/schema"
)

func TestNativePostgresTypeAndTableChildrenLowerCanonically(t *testing.T) {
	doc, err := ParseHCL("native.hcl", []byte(`
schema "app" {}
role "owner" {}

enum "state" {
  schema = schema.app
  values = ["pending", "active"]
  owner  = role.owner
}

domain "positive_int" {
  schema = schema.app
  type = "integer"
  check "positive" { expr = sql("VALUE > 0") }
}

composite "address" {
  schema = schema.app
  attribute "street" { type = "text" }
  attribute "zip" { type = domain.positive_int }
}

table "organizations" {
  schema = schema.app
  column "id" { type = "bigint" }
  primary_key { columns = [column.id] }
}

table "accounts" {
  schema = schema.app
  owner = role.owner

  column "id" {
    type = "bigint"
    identity { mode = "always" }
  }
  column "organization_id" { type = "bigint" }
  column "state" {
    type = enum.state
    default = enum_value(enum.state, "pending")
    null = false
  }
  column "display" {
    type = "text"
    generated { expr = sql("id::text") }
  }
  column "network" { type = "cidr" }

  primary_key { columns = [column.id] }
  unique "accounts_org_key" { columns = [column.organization_id] }
  check "accounts_id_check" { expr = sql("id > 0") }
  foreign_key "accounts_org_fkey" {
    columns = [column.organization_id]
    ref_columns = [table.organizations.column.id]
    on_delete = "cascade"
  }
  index "accounts_state_idx" {
    columns = [column.state]
    where = sql("state = 'active'::app.state")
  }
  index "accounts_network_idx" {
    method = "gist"
    on {
      column = column.network
      opclass = "inet_ops"
      order = "desc"
      nulls = "last"
    }
    include = [column.id]
    with = { fillfactor = 80 }
  }
}
`), nil)
	if err != nil {
		t.Fatal(err)
	}

	byKindName := map[schema.Kind]map[string]schema.Resource{}
	for _, resource := range doc.Graph.Resources {
		if byKindName[resource.Kind] == nil {
			byKindName[resource.Kind] = map[string]schema.Resource{}
		}
		byKindName[resource.Kind][resource.Name.Name] = resource
	}
	accounts := byKindName[schema.KindTable]["accounts"]
	orgs := byKindName[schema.KindTable]["organizations"]
	state := byKindName[schema.KindEnum]["state"]
	owner := byKindName[schema.KindRole]["owner"]
	stateColumn := byKindName[schema.KindColumn]["state"]
	var accountID schema.Resource
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindColumn && resource.Name.Name == "id" && resource.Name.Parent == accounts.ID {
			accountID = resource
		}
	}
	if accounts.ID == "" || orgs.ID == "" || state.ID == "" || owner.ID == "" {
		t.Fatalf("missing native resources: %+v", byKindName)
	}
	assertHCLDependency(t, accounts, owner.ID, schema.DependencyOwns)
	assertHCLDependency(t, stateColumn, state.ID, schema.DependencyUses)
	assertHCLDependency(t, byKindName[schema.KindForeignKey]["accounts_org_fkey"], orgs.ID, schema.DependencyReferences)

	assertSpec := func(kind schema.Kind, name string, want map[string]any) {
		t.Helper()
		resource := byKindName[kind][name]
		var got map[string]any
		if err := json.Unmarshal(resource.Spec, &got); err != nil {
			t.Fatal(err)
		}
		for key, value := range want {
			if gotValue, ok := got[key]; !ok || !jsonScalarEqual(gotValue, value) {
				t.Fatalf("%s %s spec[%s]=%#v want %#v; full=%v", kind, name, key, gotValue, value, got)
			}
		}
	}
	var accountIDSpec map[string]any
	if err := json.Unmarshal(accountID.Spec, &accountIDSpec); err != nil {
		t.Fatal(err)
	}
	if accountIDSpec["ordinal"] != float64(1) || accountIDSpec["identity"] != "a" {
		t.Fatalf("account id spec=%v", accountIDSpec)
	}
	assertSpec(schema.KindColumn, "state", map[string]any{"ordinal": float64(3), "type": "app.state", "default": "'pending'::app.state", "not_null": true})
	assertSpec(schema.KindColumn, "display", map[string]any{"generated": "s", "default": "id::text"})
	assertSpec(schema.KindPrimaryKey, "accounts_pkey", map[string]any{"definition": `PRIMARY KEY ("id")`})
	assertSpec(schema.KindForeignKey, "accounts_org_fkey", map[string]any{"definition": `FOREIGN KEY ("organization_id") REFERENCES "app"."organizations" ("id") ON DELETE CASCADE`})
	assertSpec(schema.KindIndex, "accounts_state_idx", map[string]any{"method": "btree", "definition": `CREATE INDEX "accounts_state_idx" ON "app"."accounts" USING btree ("state") WHERE state = 'active'::app.state`})
	assertSpec(schema.KindIndex, "accounts_network_idx", map[string]any{"method": "gist", "definition": `CREATE INDEX "accounts_network_idx" ON "app"."accounts" USING gist ("network" inet_ops DESC NULLS LAST) INCLUDE ("id") WITH (fillfactor=80)`})
	assertSpec(schema.KindDomain, "positive_int", map[string]any{"base_type": "integer"})
}

func jsonScalarEqual(got, want any) bool {
	if want == nil {
		return got == nil
	}
	return got == want
}

func TestHCLRenameIntentSurvivesAuthorFormatting(t *testing.T) {
	doc, err := ParseHCL("rename.hcl", []byte(`
schema "app" {}
table "accounts" {
  schema       = schema.app
  renamed_from = "table:old-stable-id"
  column "id" { type = "bigint" }
}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	hints, err := schema.DocumentRenameHints(doc)
	if err != nil || len(hints) != 1 || hints[0].From != "table:old-stable-id" {
		t.Fatalf("hints=%+v err=%v", hints, err)
	}
	formatted, err := FormatAuthorHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formatted), "moved {") || !strings.Contains(string(formatted), `to   = table["app"]["accounts"]`) {
		t.Fatalf("author format lost rename intent:\n%s", formatted)
	}
	back, err := ParseHCL("rename-formatted.hcl", formatted, nil)
	if err != nil {
		t.Fatal(err)
	}
	backHints, err := schema.DocumentRenameHints(back)
	if err != nil || len(backHints) != 1 || backHints[0] != hints[0] {
		t.Fatalf("round-trip hints=%+v want=%+v err=%v", backHints, hints, err)
	}
}

func TestHCLMovedAddressAndConflictsFailClosed(t *testing.T) {
	doc, err := ParseHCL("moved.hcl", []byte(`
schema "app" {}
table "accounts" { schema = schema.app }
moved {
  from = table["app"]["old_accounts"]
  to   = table["app"]["accounts"]
}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	hints, err := schema.DocumentRenameHints(doc)
	if err != nil || len(hints) != 1 || hints[0].From == "" || hints[0].To == "" {
		t.Fatalf("hints=%+v err=%v", hints, err)
	}
	_, err = ParseHCL("conflicting-moves.hcl", []byte(`
table "accounts" {}
moved {
  from = "old-a"
  to   = table.accounts
}
moved {
  from = "old-b"
  to   = table.accounts
}
`), nil)
	if err == nil || !strings.Contains(err.Error(), "conflicting rename target") {
		t.Fatalf("conflict error=%v", err)
	}
}

func TestNativeRoutineViewAndSecurityBlocksInferExactReferences(t *testing.T) {
	doc, err := ParseHCL("security.hcl", []byte(`
schema "app" {}
role "owner" {}
role "app" { login = true }

table "accounts" {
  schema = schema.app
  row_security = true
  column "id" { type = "bigint" }
  column "tenant_id" { type = "bigint" }

  policy "tenant_read" {
    command = "select"
    roles = [role.app]
    using = sql("tenant_id > 0")
    depends_on = [column.tenant_id]
  }

  trigger "accounts_touch" {
    timing = "before"
    events = ["update"]
    for_each = "row"
    function = function["touch()"]
  }
}

view "active_accounts" {
  schema = schema.app
  query = <<-SQL
    SELECT id, tenant_id FROM app.accounts
  SQL
  depends_on = [table.accounts]
}

function "touch()" {
  schema = schema.app
  name = "touch"
  identity_arguments = ""
  arguments = ""
  result = "trigger"
  language = "plpgsql"
  body = <<-SQL
    BEGIN
      RETURN NEW;
    END
  SQL
  owner = role.owner
}

membership "owner_app" {
  parent = role.owner
  member = role.app
  grantor = "postgres"
  admin = false
}

grant "accounts_select" {
  target = table.accounts
  grantee = role.app
  grantor = "postgres"
  privilege = "select"
}

default_privilege "owner_tables_select" {
  owner = role.owner
  in_schema = schema.app
  object_type = "r"
  grantee = role.app
  privilege = "select"
}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	resources := map[schema.Kind]map[string]schema.Resource{}
	for _, resource := range doc.Graph.Resources {
		if resources[resource.Kind] == nil {
			resources[resource.Kind] = map[string]schema.Resource{}
		}
		resources[resource.Kind][resource.Name.Name] = resource
	}
	accounts := resources[schema.KindTable]["accounts"]
	touch := resources[schema.KindFunction]["touch()"]
	app := resources[schema.KindRole]["app"]
	owner := resources[schema.KindRole]["owner"]
	trigger := resources[schema.KindTrigger]["accounts_touch"]
	policy := resources[schema.KindPolicy]["tenant_read"]
	grant := resources[schema.KindGrant]["accounts_select"]
	if touch.ID == "" || trigger.ID == "" || policy.ID == "" || grant.Name.Parent != accounts.ID {
		t.Fatalf("missing or misidentified security resources: %+v", resources)
	}
	assertHCLDependency(t, touch, owner.ID, schema.DependencyOwns)
	assertHCLDependency(t, trigger, touch.ID, schema.DependencyReferences)
	assertHCLDependency(t, policy, app.ID, schema.DependencyReferences)
	assertHCLDependency(t, grant, accounts.ID, schema.DependencyReferences)
	assertHCLDependency(t, grant, app.ID, schema.DependencyReferences)
	var triggerSpec map[string]any
	if err := json.Unmarshal(trigger.Spec, &triggerSpec); err != nil {
		t.Fatal(err)
	}
	if got := triggerSpec["definition"]; got != `CREATE TRIGGER "accounts_touch" BEFORE UPDATE ON "app"."accounts" FOR EACH ROW EXECUTE FUNCTION "app"."touch"()` {
		t.Fatalf("trigger definition=%v", got)
	}
	var routineSpec map[string]any
	if err := json.Unmarshal(touch.Spec, &routineSpec); err != nil {
		t.Fatal(err)
	}
	if definition, _ := routineSpec["definition"].(string); !strings.Contains(definition, "\n  RETURN NEW;\n") {
		t.Fatalf("routine heredoc newlines were not preserved: %q", definition)
	}
	formatted, err := FormatAuthorHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formatted), "<<-AUTOSQL_HCL") || strings.Contains(string(formatted), `spec_json`) {
		t.Fatalf("security author form did not use readable heredocs/native blocks:\n%s", formatted)
	}
	back, err := ParseHCL("security-formatted.hcl", formatted, nil)
	if err != nil {
		t.Fatalf("parse formatted security HCL: %v\n%s", err, formatted)
	}
	want, _ := json.Marshal(doc)
	got, _ := json.Marshal(back)
	if string(want) != string(got) {
		t.Fatalf("security author form was lossy\nwant=%s\ngot=%s\n%s", want, got, formatted)
	}
	again, err := FormatAuthorHCL(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(formatted) {
		t.Fatalf("author formatting is not stable\nfirst:\n%s\nsecond:\n%s", formatted, again)
	}
}

func TestAuthorHCLFormatterIsReadableAndLosslessWithCanonicalFallback(t *testing.T) {
	namespace := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "app"}, Spec: json.RawMessage(`{}`), Annotations: map[string]string{"comment": "application schema"}, Source: &schema.SourceLocation{URI: "original.hcl", Line: 2, Column: 1}}
	namespace.ID = schema.StableID(namespace.Kind, namespace.Name)
	table := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: "app", Name: "accounts", Parent: namespace.ID}, Spec: json.RawMessage(`{"force_row_security":false,"partitioned":false,"persistence":"p","row_security":false}`), Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}}}
	table.ID = schema.StableID(table.Kind, table.Name)
	column := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "app", Name: "id", Parent: table.ID}, Spec: json.RawMessage(`{"not_null":true,"ordinal":1,"type":"bigint"}`), Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}}
	column.ID = schema.StableID(column.Kind, column.Name)
	sequence := schema.Resource{Kind: schema.KindSequence, Name: schema.Name{Schema: "app", Name: "ids", Parent: namespace.ID, Extra: map[string]json.RawMessage{"future_name": json.RawMessage(`true`)}}, Spec: json.RawMessage(`{"cache":1,"cycle":false,"increment":1,"start":1}`), Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}}, Extra: map[string]json.RawMessage{"future_resource": json.RawMessage(`{"enabled":true}`)}}
	sequence.ID = schema.StableID(sequence.Kind, sequence.Name)
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{column, sequence, namespace, table}, Extra: map[string]json.RawMessage{"future_graph": json.RawMessage(`1`)}}, Annotations: map[string]string{"dialect": "postgresql"}, Extra: map[string]json.RawMessage{"future_document": json.RawMessage(`"kept"`)}}
	doc.Normalize()

	formatted, err := FormatAuthorHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	text := string(formatted)
	if !strings.Contains(text, `table "accounts"`) || !strings.Contains(text, `column "id"`) || !strings.Contains(text, `resource "sequence" "ids"`) {
		t.Fatalf("author/mixed formatter output is not readable or lacks fallback:\n%s", text)
	}
	back, err := ParseHCL("formatted-author.hcl", formatted, nil)
	if err != nil {
		t.Fatalf("reload author HCL: %v\n%s", err, text)
	}
	want, _ := json.Marshal(doc)
	got, _ := json.Marshal(back)
	if string(want) != string(got) {
		t.Fatalf("author HCL was lossy\nwant=%s\ngot =%s\n%s", want, got, text)
	}
	canonical, err := FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	canonicalBack, err := ParseHCL("formatted-canonical.hcl", canonical, nil)
	if err != nil {
		t.Fatal(err)
	}
	canonicalGot, _ := json.Marshal(canonicalBack)
	if string(want) != string(canonicalGot) {
		t.Fatalf("canonical HCL was lossy\nwant=%s\ngot=%s\n%s", want, canonicalGot, canonical)
	}
}
