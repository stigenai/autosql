package projecttesting_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"autosql/pkg/config"
	"autosql/pkg/dbtest"
)

// The driver supplies an actual packaged binary. This test never substitutes
// cli.Run, and missing qualification inputs fail once a binary is selected.
func TestProjectExamplePackagedWorkflow(t *testing.T) {
	binary := os.Getenv("AUTOSQL_PROJECT_TEST_BINARY")
	if binary == "" {
		t.Skip("packaged workflow driver was not invoked")
	}
	q := newWorkflowQualification(t, binary)
	q.validateExamples(t)
	q.validateSourceFormats(t)
	for _, mode := range []string{"container", "existing_admin"} {
		t.Run(mode, func(t *testing.T) { q.qualifyFactory(t, mode) })
	}
	if t.Failed() {
		return
	}
	q.save(t, "identity.json", mustWorkflowJSON(t, map[string]any{
		"scope": os.Getenv("AUTOSQL_PROJECT_TEST_SCOPE"), "source_revision": os.Getenv("AUTOSQL_PROJECT_TEST_REVISION"),
		"binary_source_manifest_sha256": os.Getenv("AUTOSQL_PROJECT_TEST_SOURCE_MANIFEST_SHA256"),
		"binary_sha256":                 workflowHash(q.read(t, binary)), "binary_version": q.version,
		"goos": runtime.GOOS, "goarch": runtime.GOARCH, "postgres_major": q.major,
		"postgres_image": q.image, "example_inputs": q.exampleInputs,
		"executed_cases": q.executed, "independent_cleanup_checks": "passed", "secret_scan": "passed",
	}))
}

type workflowQualification struct {
	binary, version, endpoint, image, admin, output string
	major, executed                                 int
	secrets                                         []string
	exampleInputs                                   map[string]string
}

func requiredWorkflowEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("qualification requires %s", name)
	}
	return value
}

func newWorkflowQualification(t *testing.T, binary string) *workflowQualification {
	t.Helper()
	q := &workflowQualification{binary: binary,
		endpoint: requiredWorkflowEnv(t, "AUTOSQL_DBTEST_DOCKER_ENDPOINT"), image: requiredWorkflowEnv(t, "AUTOSQL_DBTEST_POSTGRES_IMAGE"),
		admin: requiredWorkflowEnv(t, "AUTOSQL_DBTEST_ADMIN_DSN"), output: requiredWorkflowEnv(t, "AUTOSQL_PROJECT_TEST_OUTPUT"),
		exampleInputs: map[string]string{}, secrets: []string{"autosql-qualification-canary"}}
	var err error
	q.major, err = strconv.Atoi(requiredWorkflowEnv(t, "AUTOSQL_DBTEST_POSTGRES_MAJOR"))
	if err != nil || q.major < 14 || q.major > 18 {
		t.Fatal("qualification requires a supported PostgreSQL major")
	}
	admin, err := url.Parse(q.admin)
	if err != nil || admin.User == nil {
		t.Fatal("qualification requires an approved development administrator URL")
	}
	password, _ := admin.User.Password()
	q.secrets = append(q.secrets, q.admin, password)
	if !filepath.IsAbs(binary) || !filepath.IsAbs(q.output) {
		t.Fatal("binary and evidence paths must be absolute")
	}
	if workflowHash(q.read(t, binary)) != requiredWorkflowEnv(t, "AUTOSQL_PROJECT_TEST_BINARY_SHA256") {
		t.Fatal("packaged binary checksum mismatch")
	}
	verifyWorkflowPlatform(t, binary)
	var version struct{ Data struct{ Version string } }
	q.decode(t, q.run(t, 0, "version", "--json"), &version)
	q.version = version.Data.Version
	if q.version != requiredWorkflowEnv(t, "AUTOSQL_PROJECT_TEST_VERSION") {
		t.Fatal("packaged binary version mismatch")
	}
	return q
}

func verifyWorkflowPlatform(t *testing.T, binary string) {
	t.Helper()
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH {
		t.Fatal("binary must run on its native platform")
	}
	supported := runtime.GOOS == "linux" && (runtime.GOARCH == "arm64" || runtime.GOARCH == "amd64") || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
	if !supported {
		t.Fatal("unsupported packaged workflow platform")
	}
}

func (q *workflowQualification) run(t *testing.T, expected int, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, q.binary, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	if err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			t.Fatal("packaged command could not finish; inspect owned leases before retrying")
		}
		code = exited.ExitCode()
	}
	q.scan(t, stdout.Bytes())
	q.scan(t, stderr.Bytes())
	if code != expected || stderr.Len() != 0 {
		t.Fatalf("command %v: expected exit %d, got %d; output %s %s", args, expected, code, stdout.Bytes(), stderr.Bytes())
	}
	return stdout.Bytes()
}

func (q *workflowQualification) validateExamples(t *testing.T) {
	for _, environment := range []string{"local", "ci/postgres", "production"} {
		var previous config.ProjectIdentity
		for _, name := range []string{"autosql.hcl", "autosql.json"} {
			q.exampleInputs[name] = workflowHash(q.read(t, name))
			raw := q.run(t, 0, "config", "validate", "--config", name, "--env", environment, "--json")
			var result struct {
				Data struct{ Identity config.ProjectIdentity }
			}
			q.decode(t, raw, &result)
			if result.Data.Identity.ConfigDigest == "" || previous.ConfigDigest != "" && result.Data.Identity != previous {
				t.Fatal("equivalent JSON/HCL configuration identities differ")
			}
			previous = result.Data.Identity
			q.save(t, strings.ReplaceAll(environment, "/", "-")+"-"+name+".json", raw)
		}
	}
}

func (q *workflowQualification) validateSourceFormats(t *testing.T) {
	var previous string
	for _, format := range []string{"hcl", "sql", "json"} {
		name := "schema." + format
		q.exampleInputs[name] = workflowHash(q.read(t, name))
		loaded := q.run(t, 0, "schema", "load", "--config", "autosql.hcl", "--env", "local", "--source", format+":"+name, "--json")
		q.save(t, "source-"+format+".json", loaded)
		raw := q.run(t, 0, "plan", "--config", "autosql.hcl", "--env", "local", "--from", "sql:current.sql", "--to", format+":"+name, "--json")
		var result struct {
			Data struct {
				Plan struct {
					To string `json:"to_fingerprint"`
				}
			}
		}
		q.decode(t, raw, &result)
		if result.Data.Plan.To == "" || previous != "" && result.Data.Plan.To != previous {
			t.Fatal("local SQL/JSON/HCL sources produce different target fingerprints")
		}
		previous = result.Data.Plan.To
		q.save(t, "plan-"+format+".json", raw)
	}
	q.exampleInputs["current.sql"] = workflowHash(q.read(t, "current.sql"))
}

func (q *workflowQualification) project(t *testing.T, mode string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"schema.hcl", "tests", "failures", "migrations"} {
		source, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if source.IsDir() {
			if err := os.CopyFS(filepath.Join(root, name), os.DirFS(name)); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(root, name), q.read(t, name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := config.Load("container.hcl", func(string) (string, bool) { return "", false }, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	factory := c.DevFactories["local_container"]
	factory.Endpoint, factory.Allowlist, factory.Image, factory.PostgresVersion = q.endpoint, []string{q.endpoint}, q.image, q.major
	if mode == "existing_admin" {
		admin, _ := url.Parse(q.admin)
		admin.User, admin.RawQuery = nil, ""
		factory.Mode, factory.Admin, factory.AdminClassification = mode, "env://AUTOSQL_DBTEST_ADMIN_DSN", "development"
		factory.Endpoint, factory.Image, factory.Allowlist = "", "", []string{admin.String()}
	}
	c.DevFactories["local_container"] = factory
	project := filepath.Join(root, "runtime.json")
	if err := os.WriteFile(project, mustWorkflowJSON(t, c), 0600); err != nil {
		t.Fatal(err)
	}
	q.save(t, mode+"-runtime.json", q.read(t, project))
	return project
}

func (q *workflowQualification) read(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (q *workflowQualification) decode(t *testing.T, raw []byte, value any) {
	t.Helper()
	if err := json.Unmarshal(raw, value); err != nil {
		t.Fatal(err)
	}
}

func (q *workflowQualification) scan(t *testing.T, raw []byte) {
	t.Helper()
	for _, secret := range q.secrets {
		if secret != "" && bytes.Contains(raw, []byte(secret)) {
			t.Fatal("qualification output leaked a secret or redaction canary")
		}
	}
}

func (q *workflowQualification) save(t *testing.T, name string, raw []byte) {
	t.Helper()
	q.scan(t, raw)
	if err := os.MkdirAll(q.output, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.output, name), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func mustWorkflowJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func workflowHash(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

type packagedReport struct {
	OK   bool `json:"ok"`
	Data struct {
		dbtest.SuiteResult
		InputDigest     string                 `json:"input_digest"`
		Project         config.ProjectIdentity `json:"project"`
		Inputs          []config.InputFile     `json:"inputs"`
		MigrationDigest string                 `json:"migration_digest"`
		SchemaDigest    string                 `json:"schema_digest"`
		BinaryVersion   string                 `json:"binary_version"`
	} `json:"data"`
}

func (q *workflowQualification) qualifyFactory(t *testing.T, mode string) {
	project := q.project(t, mode)
	for _, command := range []string{"schema", "migrate"} {
		var previous packagedReport
		for _, parallel := range []string{"1", "2"} {
			name := mode + "-" + command + "-parallel" + parallel
			result := q.suite(t, project, name, command, "local", parallel, 0)
			if err := validatePackagedCases(result, []string{requiredWorkflowCase(command)}, q.major, 0); err != nil {
				t.Fatal(err)
			}
			c := result.Data.Cases[0]
			if command == "schema" && c.Assertions != 3 || command == "migrate" && (c.Assertions != 2 || strings.Join(c.Versions, ",") != "1.0.0,2.0.0") {
				t.Fatal("required assertions or upgrade versions did not execute")
			}
			if previous.Data.InputDigest != "" {
				if result.Data.InputDigest == previous.Data.InputDigest {
					t.Fatal("parallel override did not change input identity")
				}
				if !bytes.Equal(semanticWorkflowJSON(t, result), semanticWorkflowJSON(t, previous)) {
					t.Fatal("parallel execution changed semantic results")
				}
			}
			previous = result
		}
	}
	regression := q.suite(t, project, mode+"-regression", "migrate", "regression", "1", 5)
	if err := validatePackagedCases(regression, []string{"failures/regression.test.hcl::intentional regression"}, q.major, 1); err != nil {
		t.Fatal(err)
	}
	c := regression.Data.Cases[0]
	if c.Primary == nil || c.Primary.Range.StartLine != 10 || c.Primary.Stage != "assert" || len(c.Cleanup) != 1 {
		t.Fatal("regression lost its intended assertion location or cleanup failure")
	}
	q.refusals(t, project, mode)
	human := q.run(t, 0, "schema", "test", "--config", project, "--env", "local", "--json=false", "--junit", "reports/human.xml")
	if !bytes.Contains(human, []byte("1 executed, 1 passed")) {
		t.Fatal("human report did not report executed cases")
	}
	q.save(t, mode+"-human.txt", human)
	q.save(t, mode+"-human.xml", q.read(t, filepath.Join(filepath.Dir(project), "reports", "human.xml")))
	q.executed++
}

func requiredWorkflowCase(command string) string {
	if command == "schema" {
		return "tests/schema.test.hcl::bound scalar assertions"
	}
	return "tests/upgrade.test.hcl::seed survives upgrade"
}

func (q *workflowQualification) suite(t *testing.T, project, name, command, environment, parallel string, code int) packagedReport {
	t.Helper()
	report := "reports/" + name + ".xml"
	raw := q.run(t, code, command, "test", "--config", project, "--env", environment, "--parallel", parallel, "--json", "--junit", report)
	var result packagedReport
	q.decode(t, raw, &result)
	if result.Data.BinaryVersion != q.version {
		t.Fatal("report binary version mismatch")
	}
	q.verifyCleanup(t, result)
	q.executed += result.Data.Executed
	q.save(t, name+".json", raw)
	xml := q.read(t, filepath.Join(filepath.Dir(project), report))
	verifyPackagedJUnit(t, result, xml)
	q.save(t, name+".xml", xml)
	q.save(t, name+"-semantic.json", semanticWorkflowJSON(t, result))
	for _, input := range result.Data.Inputs {
		q.exampleInputs[name+"/"+input.Path] = input.Digest
	}
	return result
}
