package config

import (
	"reflect"
	"strings"
	"testing"
)

func projectIdentity(t *testing.T, filename, body string, overrides Overrides, getenv func(string) (string, bool)) ProjectIdentity {
	t.Helper()
	c, err := Load(writeProject(t, filename, body), getenv, overrides)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := c.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if len(identity.ConfigDigest) != 71 || len(identity.ExecutionConfigDigest) != 71 || identity.ConfigDigest == identity.ExecutionConfigDigest {
		t.Fatalf("missing or non-domain-separated identity: %+v", identity)
	}
	return identity
}

func TestProjectIdentityIgnoresCheckoutAndFormatting(t *testing.T) {
	jsonIdentity := projectIdentity(t, "first.json", projectJSON(), Overrides{}, noProjectEnv)
	hclIdentity := projectIdentity(t, "second.hcl", projectHCL(), Overrides{}, noProjectEnv)
	spacedIdentity := projectIdentity(t, "relocated.hcl", "\n# formatting only\n"+projectHCL(), Overrides{}, noProjectEnv)
	if jsonIdentity != hclIdentity || jsonIdentity != spacedIdentity {
		t.Fatalf("equivalent project identity differs: JSON=%+v HCL=%+v relocated=%+v", jsonIdentity, hclIdentity, spacedIdentity)
	}
	canonical := strings.ReplaceAll(projectJSON(), `"5m"`, `"300s"`)
	canonical = strings.ReplaceAll(canonical, `"schema.hcl"`, `"./schema.hcl"`)
	if got := projectIdentity(t, "canonical.json", canonical, Overrides{}, noProjectEnv); got != jsonIdentity {
		t.Fatalf("equivalent duration/path changed identity: %+v", got)
	}
}

func TestProjectIdentityBindsExecutionSettingsAndOrigins(t *testing.T) {
	base := projectIdentity(t, "base.json", projectJSON(), Overrides{}, noProjectEnv)
	for _, change := range []struct{ from, to string }{
		{`env://PRODUCTION_TARGET`, `env://ANOTHER_TARGET`},
		{`"parallel":2`, `"parallel":3`},
		{`"5m"`, `"6m"`},
		{`"schema.hcl"`, `"other.hcl"`},
		{`"policy.json"`, `"other-policy.json"`},
		{`"registry.lock"`, `"other.lock"`},
		{`env://DEV_ADMIN`, `env://OTHER_ADMIN`},
		{`"postgres_version":17`, `"postgres_version":18`},
	} {
		changed := projectIdentity(t, "changed.json", strings.Replace(projectJSON(), change.from, change.to, 1), Overrides{}, noProjectEnv)
		if changed.ConfigDigest == base.ConfigDigest || changed.ExecutionConfigDigest == base.ExecutionConfigDigest {
			t.Errorf("execution edit not bound: %s", change.from)
		}
	}
	flag := projectIdentity(t, "flag.json", projectJSON(), Overrides{Target: "env://PRODUCTION_TARGET"}, noProjectEnv)
	if flag.ConfigDigest == base.ConfigDigest || flag.ExecutionConfigDigest == base.ExecutionConfigDigest {
		t.Fatal("explicit same-value override origin was not bound")
	}
	first := projectIdentity(t, "first.json", projectJSON(), Overrides{}, func(key string) (string, bool) { return "first-secret-value", key == "AUTOSQL_TARGET" })
	second := projectIdentity(t, "second.json", projectJSON(), Overrides{}, func(key string) (string, bool) { return "rotated-secret-value", key == "AUTOSQL_TARGET" })
	if first != second {
		t.Fatal("resolved secret value entered identity")
	}
}

func TestProjectIdentitySeparatesReportSettingsAndPreservesSourceOrder(t *testing.T) {
	base := projectIdentity(t, "base.json", projectJSON(), Overrides{}, noProjectEnv)
	changed := projectIdentity(t, "report.json", strings.Replace(projectJSON(), `reports/results.xml`, `elsewhere/results.xml`, 1), Overrides{}, noProjectEnv)
	if base.ConfigDigest == changed.ConfigDigest || base.ExecutionConfigDigest != changed.ExecutionConfigDigest {
		t.Fatal("report path did not have its own configuration identity")
	}
	first := projectIdentity(t, "one.json", projectJSON(), Overrides{SchemaSources: []string{"one.sql", "two.sql"}}, noProjectEnv)
	second := projectIdentity(t, "two.json", projectJSON(), Overrides{SchemaSources: []string{"two.sql", "one.sql"}}, noProjectEnv)
	if first.ExecutionConfigDigest == second.ExecutionConfigDigest {
		t.Fatal("ordered source inputs were sorted away")
	}
}

func TestProjectIdentityIsPureAndRejectsInvalidSelection(t *testing.T) {
	c, err := Parse("project.json", []byte(projectJSON()))
	if err != nil {
		t.Fatal(err)
	}
	before := c.Effective()
	if _, err := c.Identity(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, c.Effective()) {
		t.Fatal("identity mutated effective configuration")
	}
	c.Environment = "missing"
	if _, err := c.Identity(); err == nil {
		t.Fatal("invalid selection received an identity")
	}
	legacy, err := Load(fixture(t), noProjectEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Identity(); err == nil {
		t.Fatal("version 1 received an unsupported project binding")
	}
}
