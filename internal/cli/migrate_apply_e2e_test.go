package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/migrate"
	migrateapply "autosql/pkg/migrate/apply"
)

type coordinatorCLIService struct{}

func (coordinatorCLIService) Apply(context.Context, ApplyRequest) (ApplyResult, error) {
	return ApplyResult{}, errors.New("unused")
}
func (coordinatorCLIService) VerifyArtifact(artifact.Artifact) (artifact.VerifiedArtifact, error) {
	return artifact.VerifiedArtifact{}, errors.New("unused")
}
func (coordinatorCLIService) ApplyVersioned(context.Context, artifact.VerifiedArtifact, executor.Session, executor.Tx) (executor.ExternalExecution, error) {
	return executor.ExternalExecution{}, errors.New("unused")
}
func (coordinatorCLIService) DrainLifecycle(context.Context, executor.LifecycleEvent) error {
	return nil
}

func TestMigrateApplyBaselineRealCLIHumanJSONStatusesAndRedaction(t *testing.T) {
	url := os.Getenv("AUTOSQL_POSTGRES_TEST_DSN")
	if url == "" {
		t.Skip("live database unset")
	}
	t.Setenv("AUTOSQL_CLI_SECRET_DSN", url)
	dir := t.TempDir()
	if _, e := migrate.Update(dir, migrate.UpdateRequest{ManifestVersion: migrate.ManifestVersion}); e != nil {
		t.Fatal(e)
	}
	old := executeVersionedMigration
	defer func() { executeVersionedMigration = old }()
	secretSQL := "select 'seeded-sql-literal'"
	proof := "seeded-approval-proof"
	precheck := "seeded-precheck-arg-result"
	backend := "backend password=seeded-backend"
	cases := []struct {
		name   string
		args   []string
		result migrateapply.Result
		err    error
		want   string
	}{{"human_success", []string{"migrate", "apply", "--url", "env://AUTOSQL_CLI_SECRET_DSN", "--migration-dir", dir, "--operator", "op", "--dry-run"}, migrateapply.Result{Status: "applied", FinalVersion: "1.0.0"}, nil, "applied"}, {"json_noop", []string{"migrate", "apply", "--url", "env://AUTOSQL_CLI_SECRET_DSN", "--migration-dir", dir, "--operator", "op", "--dry-run", "--json"}, migrateapply.Result{Status: "no_op"}, nil, "no_op"}, {"human_baseline", []string{"migrate", "baseline", "--url", "env://AUTOSQL_CLI_SECRET_DSN", "--migration-dir", dir, "--revision-schema", "autosql_cli_baseline", "--operator", "op"}, migrateapply.Result{Status: "baselined", FinalVersion: "1.0.0"}, nil, "baselined"}, {"json_partial", []string{"migrate", "apply", "--url", "env://AUTOSQL_CLI_SECRET_DSN", "--migration-dir", dir, "--operator", "op", "--dry-run", "--json"}, migrateapply.Result{Status: "partial_failure", Failure: &migrateapply.Failure{Recovery: "repair audit"}}, errors.New(secretSQL + proof + precheck + backend + url), "partial_failure"}, {"human_uncertain", []string{"migrate", "apply", "--url", "env://AUTOSQL_CLI_SECRET_DSN", "--migration-dir", dir, "--operator", "op", "--dry-run"}, migrateapply.Result{Status: "uncertain", Failure: &migrateapply.Failure{Recovery: "reconcile outcome"}}, errors.New(secretSQL + proof + precheck + backend + url), "uncertain"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executeVersionedMigration = func(migrateapply.Engine, context.Context, migrateapply.Request) (migrateapply.Result, error) {
				return tc.result, tc.err
			}
			var out, er bytes.Buffer
			code := RunWithServices(context.Background(), tc.args, Streams{Out: &out, Err: &er}, Services{Apply: coordinatorCLIService{}})
			if tc.err == nil && code != 0 || tc.err != nil && code == 0 {
				t.Fatalf("code=%d out=%s err=%s", code, out.String(), er.String())
			}
			combined := out.String() + er.String()
			if !strings.Contains(combined, tc.want) {
				t.Fatalf("missing %q: %s", tc.want, combined)
			}
			for _, seed := range []string{url, secretSQL, proof, precheck, "seeded-backend"} {
				if strings.Contains(combined, seed) {
					t.Fatalf("leaked %q: %s", seed, combined)
				}
			}
		})
	}
}
