package source

import (
	"strings"
	"testing"

	"autosql/pkg/schema"
)

func TestTypedVariablesLocalsAndForEachUseStableKeys(t *testing.T) {
	fixture := func(grants string) string {
		return `
variable "environment" {
  type = string
  default = "dev"
  validation {
    condition = var.environment != ""
    error_message = "environment must not be empty"
  }
}
locals {
  grants = ` + grants + `
}
schema "app" {}
role "reader" {}
table "users" { schema = schema.app }
table "events" { schema = schema.app }
grant "instance" {
  for_each = local.grants
  name = each.key
  target = table[each.value.table]
  grantee = role.reader
  grantor = "postgres"
  privilege = each.value.privilege
}
`
	}
	first, err := ParseHCL("computed.hcl", []byte(fixture(`{
    users_select = { table = "users", privilege = "select" }
    events_select = { table = "events", privilege = "select" }
  }`)), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseHCL("computed.hcl", []byte(fixture(`{
    events_select = { table = "events", privilege = "select" }
    users_select = { table = "users", privilege = "select" }
  }`)), nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(doc schema.Document) map[string]string {
		result := map[string]string{}
		for _, resource := range doc.Graph.Resources {
			if resource.Kind == schema.KindGrant {
				result[resource.Name.Name] = resource.ID
				if resource.Source == nil || resource.Source.URI != "computed.hcl" {
					t.Fatalf("expanded instance lost declaration provenance: %+v", resource.Source)
				}
				if string(resource.Source.Extra["for_each_key"]) != `"`+resource.Name.Name+`"` {
					t.Fatalf("expanded instance lost stable key provenance: %+v", resource.Source)
				}
			}
		}
		return result
	}
	left, right := ids(first), ids(second)
	if len(left) != 2 || left["users_select"] != right["users_select"] || left["events_select"] != right["events_select"] {
		t.Fatalf("for_each identities depend on map order: left=%v right=%v", left, right)
	}
}

func TestVariableValidationAggregatesAndFailsClosed(t *testing.T) {
	_, err := ParseHCL("variables.hcl", []byte(`
variable "environment" {
  type = string
  validation {
    condition = var.environment != ""
    error_message = "environment is required"
  }
}
variable "region" {
  type = string
  validation {
    condition = var.region != ""
    error_message = "region is required"
  }
}
schema "app" {}
`), HCLVariables{"environment": "", "region": ""})
	if err == nil || !strings.Contains(err.Error(), "environment is required") || !strings.Contains(err.Error(), "region is required") {
		t.Fatalf("validation error was not aggregated: %v", err)
	}
}

func TestForEachRejectsPositionBasedCollections(t *testing.T) {
	_, err := ParseHCL("unstable.hcl", []byte(`
schema "app" {}
table "instance" {
  for_each = ["users", "events"]
  name = each.value
  schema = schema.app
}
`), nil)
	if err == nil || !strings.Contains(err.Error(), "stable string keys") {
		t.Fatalf("position-based for_each did not fail closed: %v", err)
	}
}
