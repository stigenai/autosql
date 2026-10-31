package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"autosql/pkg/secret"
)

func noProjectEnv(string) (string, bool) { return "", false }

func TestProjectTypedSourceOverride(t *testing.T) {
	path := writeProject(t, "autosql.json", projectJSON())
	sources := []SchemaSource{{Kind: "hcl", Path: "author.any"}, {Kind: "json", Path: "native.any"}}
	c, err := Load(path, noProjectEnv, Overrides{SourceInputs: sources})
	if err != nil {
		t.Fatal(err)
	}
	e := c.Effective()
	if !reflect.DeepEqual(e.Values.SchemaSources, sources) || e.Provenance["/schema_sources"].Kind != "flag" {
		t.Fatalf("typed override: %+v", e)
	}
	sources[0].Path = "changed"
	if c.Effective().Values.SchemaSources[0].Path != "author.any" {
		t.Fatal("override aliased caller data")
	}
	if _, err := Load(path, noProjectEnv, Overrides{SourceInputs: sources, SchemaSources: []string{"another.sql"}}); err == nil {
		t.Fatal("ambiguous source override forms were accepted")
	}
}

func projectJSON() string {
	return `{
 "version": 2,
 "environment": "dev",
 "variables": {"label":"dev"},
 "dev_factories":{"local":{"mode":"existing_admin","admin":"env://DEV_ADMIN","admin_classification":"development","postgres_version":17,"allowlist":["postgres://localhost:5432/postgres"]}},
 "environments":{"dev":{
  "classification":"development",
  "target":"env://PRODUCTION_TARGET",
  "dev_factory":"local",
  "schema_sources":[{"kind":"hcl","path":"schema.hcl"}],
  "migration_dir":"migrations",
  "tests":{"files":["tests/*.test.hcl"],"parallel":2,"timeout":"5m"},
  "policy_files":["policy.json"],
  "registry":{"url":"https://registry.example.test/project","lock":"registry.lock"},
  "reporting":{"json":true,"junit":"reports/results.xml"}
 }}
}`
}

func projectHCL() string {
	return `version = 2
environment = "dev"
variables = { label = "dev" }
dev_factory "local" {
 mode = "existing_admin"
 admin = "env://DEV_ADMIN"
 admin_classification = "development"
 postgres_version = 17
 allowlist = ["postgres://localhost:5432/postgres"]
}
environment "dev" {
 classification = "development"
 target = "env://PRODUCTION_TARGET"
 dev_factory = "local"
 schema_sources = [{kind = "hcl", path = "schema.hcl"}]
 migration_dir = "migrations"
 tests = {files = ["tests/*.test.hcl"], parallel = 2, timeout = "5m"}
 policy_files = ["policy.json"]
 registry = {url = "https://registry.example.test/project", lock = "registry.lock"}
 reporting = {json = true, junit = "reports/results.xml"}
}
`
}

func writeProject(t *testing.T, filename, body string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, filename)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectJSONAndHCLNormalizeIdentically(t *testing.T) {
	var values [][]byte
	for _, format := range []string{"json", "hcl"} {
		text := projectJSON()
		if format == "hcl" {
			text = projectHCL()
		}
		path := writeProject(t, "autosql."+format, text)
		c, err := Load(path, noProjectEnv, Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		value, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
		effective := c.Effective()
		source := effective.Provenance["/schema_sources/0/path"]
		if effective.Name != "dev" || source.Kind != "file" || source.Range == nil || source.Range.Filename != path || source.Range.StartLine < 2 {
			t.Fatalf("source provenance missing: %+v", effective)
		}
	}
	if string(values[0]) != string(values[1]) {
		t.Fatalf("different normalized project:\n%s\n%s", values[0], values[1])
	}
}

func TestProjectFiniteEnvironmentSetsAndSelection(t *testing.T) {
	text := `version=2
variables = { tenants = { blue = "BLUE", red = "RED" } }
environment_set "tenant" {
 for_each = var.tenants
 target = "env://${each.value}"
 schema_sources = [{kind="sql",path="schema.sql"}]
 migration_dir = "migrations/${each.key}"
}
`
	path := writeProject(t, "project.hcl", text)
	c, err := Load(path, noProjectEnv, Overrides{Environment: "tenant/red"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Environments) != 2 || c.Environments[c.Environment].Target != "env://RED" || c.Environments[c.Environment].MigrationDir != "migrations/red" {
		t.Fatalf("expansion: %+v", c)
	}
	if c.Effective().Provenance["/environment"].Kind != "flag" {
		t.Fatal("selection provenance missing")
	}
	for _, selection := range []string{"", "tenant", "tenant/unknown"} {
		if _, err := Load(path, noProjectEnv, Overrides{Environment: selection}); err == nil {
			t.Fatalf("ambiguous/absent selection accepted: %q", selection)
		}
	}
	duplicate := text + `environment "tenant/red" {
 target="env://OTHER"
 schema_sources=[{kind="sql",path="schema.sql"}]
 migration_dir="migrations"
}`
	if _, err := Parse("duplicate.hcl", []byte(duplicate)); err == nil {
		t.Fatal("explicit/expanded duplicate accepted")
	}
}

func TestProjectExpansionLimitAndNoProgramExpressions(t *testing.T) {
	for _, count := range []int{256, 257} {
		var entries []string
		for i := 0; i < count; i++ {
			entries = append(entries, fmt.Sprintf("key%d = %d", i, i))
		}
		text := fmt.Sprintf(`version=2
 environment="set/key0"
 environment_set "set" {
  for_each={%s}
  target="env://TARGET"
  schema_sources=[{kind="sql",path="schema.sql"}]
  migration_dir="migrations"
 }`, strings.Join(entries, ","))
		c, err := Parse("bounded.hcl", []byte(text))
		if count == 256 {
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("257 environments accepted")
		}
	}
	for _, expression := range []string{`env("SECRET")`, `file("secret.txt")`, `{for k,v in {a=1}: k=>v}`, `true ? {a=1} : {b=2}`, `[1,2]`, `null`, `var.undeclared`} {
		text := `version=2
environment_set "bad" { for_each=` + expression + ` }`
		if _, err := Parse("blocked.hcl", []byte(text)); err == nil {
			t.Fatalf("unsupported expansion accepted: %s", expression)
		}
	}
}

func TestProjectLocationsAndSafeFailures(t *testing.T) {
	for _, test := range []struct {
		name, text, field string
		line              int
	}{
		{"unknown.json", "{\n \"version\": 2,\n \"typo\": \"seeded-secret\"\n}", "/typo", 3},
		{"duplicate.json", "{\n \"version\": 2,\n \"version\": 2\n}", "/version", 3},
		{"unknown.hcl", "version=2\nsecret_typo=\"seeded-secret\"\n", "/secret_typo", 2},
		{"wrong-type.json", "{\n \"version\": \"seeded-secret\"\n}", "/version", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.name, []byte(test.text))
			var located *LocatedError
			if !errors.As(err, &located) || located.Field != test.field || located.Range.StartLine != test.line || located.Range.StartColumn < 1 || strings.Contains(err.Error(), "seeded-secret") {
				t.Fatalf("unsafe/unlocated error: %#v %v", located, err)
			}
		})
	}
	path := writeProject(t, "project.hcl", strings.Replace(projectHCL(), `"env://PRODUCTION_TARGET"`, `"postgres://user:seeded-secret@db/app"`, 1))
	_, err := Load(path, noProjectEnv, Overrides{})
	var located *LocatedError
	if !errors.As(err, &located) || !strings.HasSuffix(located.Field, "/target") || strings.Contains(err.Error(), "seeded-secret") {
		t.Fatalf("target value leaked or location lost: %v", err)
	}
}

func TestProjectOverrideOriginsAndListReplacement(t *testing.T) {
	path := writeProject(t, "project.json", projectJSON())
	env := map[string]string{"AUTOSQL_ENV": "dev", "AUTOSQL_TARGET": "never-copy-this-secret", "AUTOSQL_SCHEMA_SOURCES": "environment.sql", "AUTOSQL_INCLUDE": "public.*"}
	c, err := Load(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Overrides{Environment: "dev", SchemaSources: []string{"flag.hcl"}, Include: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	e := c.Effective()
	if e.Values.Target != "env://AUTOSQL_TARGET" || len(e.Values.SchemaSources) != 1 || e.Values.SchemaSources[0].Path != "flag.hcl" || e.Values.SchemaSources[0].Kind != "hcl" || len(e.Values.Include) != 0 {
		t.Fatalf("overrides: %+v", e)
	}
	if e.Provenance["/target"].Kind != "environment" || e.Provenance["/schema_sources"].Kind != "flag" || e.Provenance["/include"].Kind != "flag" {
		t.Fatalf("origins: %+v", e.Provenance)
	}
	if _, ok := e.Provenance["/schema_sources/0/path"]; ok {
		t.Fatal("overridden list retained misleading file origins")
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "never-copy-this-secret") {
		t.Fatal("environment secret copied into effective output")
	}
	e.Values.SchemaSources[0].Path = "changed"
	if c.Environments["dev"].SchemaSources[0].Path == "changed" {
		t.Fatal("effective result mutated project")
	}
}

func TestProjectValidationAndInspectionDoNotResolveTargets(t *testing.T) {
	path := writeProject(t, "project.json", projectJSON())
	root := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(root, "schema.hcl"), []byte(`schema "app" {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	var lookups []string
	c, err := Load(path, func(key string) (string, bool) { lookups = append(lookups, key); return "", false }, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.InspectLocal(); err != nil {
		t.Fatal(err)
	}
	for _, key := range lookups {
		if key == "PRODUCTION_TARGET" || key == "DEV_ADMIN" {
			t.Fatalf("validation resolved %s", key)
		}
	}
	var resolved []string
	resolver := secret.NewResolver()
	resolver.Getenv = func(key string) (string, bool) {
		resolved = append(resolved, key)
		return "sensitive-runtime-value", true
	}
	value, err := c.ResolveTarget(context.Background(), resolver)
	if err != nil || value != "sensitive-runtime-value" || !reflect.DeepEqual(resolved, []string{"PRODUCTION_TARGET"}) {
		t.Fatalf("purpose-specific resolution: value present=%v lookups=%v err=%v", value != "", resolved, err)
	}
	if resolver.BaseDir != "" {
		t.Fatal("target resolution mutated shared resolver")
	}
}

func TestProjectV1CompatibilityAndParserBounds(t *testing.T) {
	path := fixture(t)
	c, err := Load(path, noProjectEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"tests"`, `"reporting"`, `"registry"`, `"dev_factories"`} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("v1 wire output changed: %s", raw)
		}
	}
	v1 := strings.Replace(projectJSON(), `"version": 2`, `"version": 1`, 1)
	if _, err := Parse("legacy.json", []byte(v1)); err == nil {
		t.Fatal("v1 accepted v2 fields")
	}
	if _, err := Parse("huge.json", make([]byte, maxProjectBytes+1)); err == nil {
		t.Fatal("oversize project accepted")
	}
	deep := `{"version":2,"variables":{"deep":` + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40) + "}}"
	if _, err := Parse("deep.json", []byte(deep)); err == nil {
		t.Fatal("unbounded nesting accepted")
	}
}

func TestProjectEmptySetStillValidatesTemplate(t *testing.T) {
	base := projectHCL() + `
environment_set "empty" {
 for_each = {}
 target = "env://${each.value}"
 schema_sources = [{kind="sql", path="${each.key}.sql"}]
 migration_dir = "migrations/${each.key}"
 tests = {files=["tests/*.test.hcl"], parallel=1}
}
`
	if c, err := Parse("empty.hcl", []byte(base)); err != nil || len(c.Environments) != 1 {
		t.Fatalf("valid empty set refused: %v", err)
	}
	for _, test := range []struct{ from, to string }{
		{`tests = {files=["tests/*.test.hcl"], parallel=1}`, `tests = {typo=each.key}`},
		{`tests = {files=["tests/*.test.hcl"], parallel=1}`, `tests = {parallel=each.key}`},
		{`tests = {files=["tests/*.test.hcl"], parallel=1}`, `tests = {parallel=1.5}`},
		{`tests = {files=["tests/*.test.hcl"], parallel=1}`, `tests = null`},
		{`target = "env://${each.value}"`, `target = var.undeclared`},
		{`target = "env://${each.value}"`, `target = each.typo`},
		{`target = "env://${each.value}"`, `target = env("SECRET")`},
		{`target = "env://${each.value}"`, `typo = each.key`},
		{`target = "env://${each.value}"`, `target = 42`},
		{`target = "env://${each.value}"`, `target {}`},
	} {
		if _, err := Parse("empty.hcl", []byte(strings.Replace(base, test.from, test.to, 1))); err == nil {
			t.Errorf("empty set hid invalid template: %s", test.to)
		}
	}
	for _, text := range []string{
		strings.Replace(projectHCL(), `variables = { label = "dev" }`, `variables = { label = "dev", label = "prod" }`, 1),
		strings.Replace(projectHCL(), `parallel = 2`, `parallel = 2, parallel = 3`, 1),
	} {
		if _, err := Parse("duplicate.hcl", []byte(text)); err == nil {
			t.Fatal("duplicate HCL object field accepted")
		}
	}
}

func TestProjectDefaultsKeepOverrideOriginAndCloneFactory(t *testing.T) {
	path := writeProject(t, "project.json", projectJSON())
	c, err := Load(path, func(key string) (string, bool) { return "", key == "AUTOSQL_TIMEOUT" }, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	e := c.Effective()
	if e.Values.Timeout != "30s" || e.Values.RevisionSchema != "autosql_revision" || e.Values.Tests.Timeout != "5m0s" {
		t.Fatalf("defaults not normalized: %+v", e.Values)
	}
	if origin := e.Provenance["/timeout"]; origin.Kind != "environment" || !origin.Default || origin.Range != nil {
		t.Fatalf("default lost environment override: %+v", origin)
	}
	if f := e.DevelopmentFactory; f == nil || f.Isolation != "database" || f.StartupTimeout != "2m0s" || f.CaseTimeout != "5m0s" || f.CleanupTimeout != "10s" {
		t.Fatalf("factory defaults not normalized: %+v", f)
	}
	e.DevelopmentFactory.Allowlist[0] = "changed"
	if c.DevFactories["local"].Allowlist[0] == "changed" {
		t.Fatal("effective factory mutated project")
	}
}

func TestProjectHCLObjectReferencesPreserveFieldPresence(t *testing.T) {
	for _, parallel := range []int{0, 2} {
		body := strings.Replace(projectHCL(), `variables = { label = "dev" }`, fmt.Sprintf(`variables = { settings = {parallel=%d, files=["tests/*.test.hcl"]} }`, parallel), 1)
		body = strings.Replace(body, `tests = {files = ["tests/*.test.hcl"], parallel = 2, timeout = "5m"}`, `tests = var.settings`, 1)
		path := writeProject(t, "object.hcl", body)
		c, err := Load(path, noProjectEnv, Overrides{})
		if parallel == 0 {
			if err == nil {
				t.Fatal("object reference hid explicitly invalid parallelism")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		effective := c.Effective()
		for _, field := range []string{"/tests/parallel", "/tests/files/0"} {
			origin := effective.Provenance[field]
			if origin.Kind != "file" || origin.Range == nil || origin.Range.Filename != path || origin.Range.StartLine < 10 {
				t.Fatalf("object reference lost child provenance: %s %+v", field, origin)
			}
		}
	}
}

func TestProjectLocalInspectionRejectsEscapingSymlinks(t *testing.T) {
	for _, escaped := range []string{"source", "migrations"} {
		t.Run(escaped, func(t *testing.T) {
			path := writeProject(t, "project.json", projectJSON())
			root, outside := filepath.Dir(path), t.TempDir()
			for _, directory := range []string{root, outside} {
				if err := os.WriteFile(filepath.Join(directory, "schema.hcl"), []byte(`schema "app" {}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(directory, "migrations"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			name := "schema.hcl"
			if escaped == "migrations" {
				name = "migrations"
			}
			if err := os.Remove(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, name), filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path, noProjectEnv, Overrides{})
			if err != nil {
				t.Fatalf("ordinary parsing touched filesystem: %v", err)
			}
			if _, err := c.InspectLocal(); err == nil {
				t.Fatal("inspection allowed escaping symlink")
			}
			resolver := secret.NewResolver()
			resolver.Getenv = func(string) (string, bool) {
				t.Error("invalid local inputs resolved a secret")
				return "", false
			}
			if _, err := c.Preflight(context.Background(), resolver); err == nil {
				t.Fatal("v2 preflight bypassed local containment checks")
			}
		})
	}
}

func TestProjectV1KeepsOptionalNullCompatibility(t *testing.T) {
	text := `{"version":1,"environment":"dev","environments":{"dev":{"target":"env://TARGET","schema_sources":[{"kind":"sql","path":"schema.sql"}],"migration_dir":"migrations","include":null,"variables":null,"timeout":null,"dev_database":null}}}`
	if _, err := Load(writeProject(t, "v1.json", text), noProjectEnv, Overrides{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("v2.json", []byte(strings.Replace(text, `"version":1`, `"version":2`, 1))); err == nil {
		t.Fatal("v2 typed field accepted null")
	}
}

func TestProjectHCLRejectsOversizeEvaluationAndExpansion(t *testing.T) {
	for _, number := range []string{"1e10000000", "1e-10000000"} {
		if _, err := Parse("number.hcl", []byte("version=2\nvariables={large="+number+"}")); err == nil {
			t.Fatal("unbounded decimal expansion accepted")
		}
	}
	large := strings.Repeat("a", 300000)
	for _, template := range []string{
		`environment "dev" {
 target="env://TARGET"
 schema_sources=[{kind="sql",path="schema.sql"}]
 migration_dir="migrations"
 variables={large="` + strings.Repeat("${var.large}", 20) + `"}
}`,
		`environment_set "dev" {
 for_each={a=1,b=2,c=3,d=4,e=5,f=6,g=7,h=8,i=9,j=10,k=11,l=12,m=13,n=14,o=15,p=16}
 target="env://TARGET"
 schema_sources=[{kind="sql",path="schema.sql"}]
 migration_dir="migrations"
 variables={large=var.large}
}`,
	} {
		body := `version=2
variables={large="` + large + `"}
` + template
		if _, err := Parse("bounded.hcl", []byte(body)); err == nil || !strings.Contains(err.Error(), "exceeds 4 MiB") {
			t.Fatalf("unbounded HCL evaluation accepted: %v", err)
		}
	}
}

func FuzzProjectParse(f *testing.F) {
	for _, seed := range []string{projectJSON(), projectHCL(), "null", "[]", "", "version=2\nvariables={value=1e10000000}", "{\"version\":2,\"environments\":null}"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > maxProjectBytes {
			return
		}
		for _, name := range []string{"fuzz.json", "fuzz.hcl"} {
			c, err := Parse(name, []byte(input))
			if err == nil && c.Validate() == nil {
				if _, err := json.Marshal(c.Effective()); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}

func TestProjectFactoryConfigurationRefusals(t *testing.T) {
	container := DevelopmentFactory{Mode: "container", Endpoint: "unix:///var/run/docker.sock", PostgresVersion: 17, Image: "postgres@sha256:" + strings.Repeat("a", 64), Allowlist: []string{"unix:///var/run/docker.sock"}}
	for _, test := range []struct {
		name   string
		change func(*DevelopmentFactory)
	}{
		{"unpinned image", func(f *DevelopmentFactory) { f.Image = "postgres:17" }},
		{"implicit daemon", func(f *DevelopmentFactory) { f.Endpoint = "" }},
		{"missing allowlist", func(f *DevelopmentFactory) { f.Allowlist = nil }},
		{"URL credential", func(f *DevelopmentFactory) { f.Endpoint = "tcp://user:seeded-factory-secret@host:2375" }},
		{"unsupported version", func(f *DevelopmentFactory) { f.PostgresVersion = 13 }},
		{"negative timeout", func(f *DevelopmentFactory) { f.CleanupTimeout = "-1s" }},
		{"unsupported isolation", func(f *DevelopmentFactory) { f.Isolation = "shared" }},
		{"container admin", func(f *DevelopmentFactory) { f.Admin = "env://ADMIN" }},
		{"protected credential", func(f *DevelopmentFactory) {
			f.ProtectedTargets = []string{"postgres://user:seeded-factory-secret@prod/db"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, err := Parse("factory.json", []byte(projectJSON()))
			if err != nil {
				t.Fatal(err)
			}
			c.DevFactories["local"] = container
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			changed := container
			test.change(&changed)
			c.DevFactories["local"] = changed
			if err := c.Validate(); err == nil || strings.Contains(err.Error(), "seeded-factory-secret") {
				t.Fatalf("invalid or unsafe factory result: %v", err)
			}
		})
	}
	for _, classification := range []string{"", "production"} {
		body := strings.Replace(projectJSON(), `"admin_classification":"development"`, `"admin_classification":"`+classification+`"`, 1)
		if _, err := Load(writeProject(t, "project.json", body), noProjectEnv, Overrides{}); err == nil {
			t.Fatal("unclassified/production administration reference accepted")
		}
	}
}
