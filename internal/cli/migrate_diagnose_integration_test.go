package cli

import (
	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/source"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"testing"
	"time"
)

type diagnoseVerifier struct{ policy artifact.VerifyPolicy }

func (d diagnoseVerifier) Apply(context.Context, ApplyRequest) (ApplyResult, error) {
	return ApplyResult{}, errors.New("unused")
}
func (d diagnoseVerifier) VerifyArtifact(a artifact.Artifact) (artifact.VerifiedArtifact, error) {
	return a.VerifyTrusted(d.policy)
}

func TestMigrateDiagnoseLiveProductionCLIMatrix(t *testing.T) {
	url := os.Getenv("AUTOSQL_REPAIR_TEST_DSN")
	if url == "" {
		t.Skip("AUTOSQL_REPAIR_TEST_DSN unset")
	}
	ctx := context.Background()
	conn, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, `drop schema if exists repair_diag cascade; drop table if exists autosql_migration_history`)
	doc, e := source.LoadContext(ctx, source.Input{URI: "desired.sql", Format: source.FormatSQL, Data: []byte(`CREATE SCHEMA repair_diag; CREATE TABLE repair_diag.widgets(id bigint);`)})
	if e != nil {
		t.Fatal(e)
	}
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	empty, _ = postgres.New().Normalize(ctx, empty)
	doc, _ = postgres.New().Normalize(ctx, doc)
	p, e := plan.Build(ctx, postgres.New(), empty, doc, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	change, e := guardrail.ChangeDigest(p.Changes)
	if e != nil {
		t.Fatal(e)
	}
	var sqls []string
	for _, st := range p.Steps {
		if st.Kind == plan.StepExecutable {
			sqls = append(sqls, st.SQL)
		}
	}
	checks := precheck.Plan{ID: "diagnose", ChangeDigest: change, Statements: sqls}
	checks.Digest, e = precheck.Digest(checks)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Add(-time.Minute)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	a, e := artifact.New(p, checks, now, now.Add(time.Hour), "repair-rev", "test", "repair-db", "sha256:"+strings.Repeat("b", 64), artifact.Approval{Identity: "repair-release", ApprovedAt: now}, map[string]string{})
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Sign("repair-key", key); e != nil {
		t.Fatal(e)
	}
	raw, _ := a.MarshalCanonical()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	var sql strings.Builder
	checkDirective := checks.Digest
	if !strings.HasPrefix(checkDirective, "sha256:") {
		checkDirective = "sha256:" + checkDirective
	}
	fmt.Fprintf(&sql, "-- autosql:transaction=required\n-- autosql:plan-digest=%s\n-- autosql:check-digest=%s\n-- autosql:bundle-digest=%s\n", p.Digest, checkDirective, a.GuardrailDigest)
	for _, q := range sqls {
		sql.WriteString(q)
		if !strings.HasSuffix(strings.TrimSpace(q), ";") {
			sql.WriteByte(';')
		}
		sql.WriteByte('\n')
	}
	man, e := migrate.Update(dir, migrate.UpdateRequest{Files: []migrate.File{{Name: "V1__widgets.sql", SQL: []byte(sql.String()), ArtifactName: "V1__widgets.sql.artifact.json", Artifact: raw}}})
	if e != nil {
		t.Fatal(e)
	}
	entry := man.Entries[0]
	policy := artifact.VerifyPolicy{Now: time.Now, Expected: artifact.ExpectedBindings{PlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity}, Keys: map[string]artifact.KeyRecord{"repair-key": {PublicKey: pub, Issuer: "issuer", Identity: "signer", Environment: "test", Purpose: "release", Status: "active", NotBefore: now.Add(-time.Hour), NotAfter: now.Add(2 * time.Hour)}}, Issuer: "issuer", Identity: "signer", Purpose: "release"}
	services := Services{Apply: diagnoseVerifier{policy: policy}}
	resetLive := func(drift bool) {
		_, _ = conn.Exec(ctx, `drop schema if exists repair_diag cascade`)
		for _, q := range sqls {
			if _, e = conn.Exec(ctx, q); e != nil {
				t.Fatal(e)
			}
		}
		if drift {
			_, e = conn.Exec(ctx, `alter table repair_diag.widgets add column manual_secret text`)
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	historyTable := func() {
		_, _ = conn.Exec(ctx, `drop table if exists autosql_migration_history`)
		_, e = conn.Exec(ctx, `create table autosql_migration_history(artifact_digest text,step_id text,step_hash text,phase_id text,phase_mode text,state text,execution_id text,target_identity text,plan_digest text,bundle_digest text,attempt integer)`)
		if e != nil {
			t.Fatal(e)
		}
	}
	base := func(state string) revision.Revision {
		return revision.Revision{Version: entry.Version, Description: entry.Name, Kind: "migration", FileName: entry.File, FileDigest: entry.SQLDigest, ManifestDigest: man.Digest, ManifestGeneration: man.Generation, ArtifactDigest: entry.ArtifactDigest, PlanDigest: entry.Directives.PlanDigest, ChecksDigest: entry.Directives.CheckDigest, BundleDigest: entry.Directives.BundleDigest, State: state, StatementOrdinal: len(entry.Statements), Attempt: 1, Operator: "executor", StartedAt: now, UpdatedAt: now}
	}
	insertHistory := func(target, artifactDigest, step, state, execution, planDigest, bundle string, attempt int) {
		_, e = conn.Exec(ctx, `insert into autosql_migration_history values($1,$2,'step-hash','phase','required',$3,$4,$5,$6,$7,$8)`, artifactDigest, step, state, execution, target, planDigest, bundle, attempt)
		if e != nil {
			t.Fatal(e)
		}
	}
	firstStep := ""
	for _, st := range p.Steps {
		if st.Kind == plan.StepExecutable {
			firstStep = st.ID
			break
		}
	}
	cases := []struct {
		name, want string
		setup      func(*revision.Store)
	}{
		{"dirty", "dirty", func(s *revision.Store) {
			if e = s.Insert(ctx, base("pending")); e != nil {
				t.Fatal(e)
			}
		}},
		{"partial", "dirty", func(s *revision.Store) {
			if e = s.Insert(ctx, base("partial")); e != nil {
				t.Fatal(e)
			}
		}},
		{"unknown_applied", "unknown", func(s *revision.Store) {
			r := base("applied")
			r.Version = "9.0.0"
			r.FileName = "password=unknown-secret.sql"
			if e = s.Insert(ctx, r); e != nil {
				t.Fatal(e)
			}
		}},
		{"checksum", "checksum", func(s *revision.Store) {
			r := base("applied")
			r.FileDigest = "sha256:" + strings.Repeat("f", 64)
			if e = s.Insert(ctx, r); e != nil {
				t.Fatal(e)
			}
		}},
		{"manual_drift", "manual_drift", func(s *revision.Store) {
			if e = s.Insert(ctx, base("applied")); e != nil {
				t.Fatal(e)
			}
		}},
		{"executor_unknown_artifact", "executor_unknown", func(s *revision.Store) {
			if e = s.Insert(ctx, base("applied")); e != nil {
				t.Fatal(e)
			}
			insertHistory("repair-db/test", "sha256:"+strings.Repeat("9", 64), firstStep, "confirmed", "unknown", p.Digest, a.GuardrailDigest, 1)
		}},
		{"executor_duplicate_attempt", "executor_duplicate", func(s *revision.Store) {
			if e = s.Insert(ctx, base("applied")); e != nil {
				t.Fatal(e)
			}
			insertHistory("repair-db/test", a.Digest, firstStep, "confirmed", a.Digest, p.Digest, a.GuardrailDigest, 1)
			insertHistory("repair-db/test", a.Digest, firstStep, "confirmed", a.Digest, p.Digest, a.GuardrailDigest, 1)
		}},
		{"executor_unknown_step", "executor_linkage", func(s *revision.Store) {
			if e = s.Insert(ctx, base("applied")); e != nil {
				t.Fatal(e)
			}
			insertHistory("repair-db/test", a.Digest, "unknown-step", "confirmed", a.Digest, p.Digest, a.GuardrailDigest, 1)
		}},
		{"executor_linkage", "executor_linkage", func(s *revision.Store) {
			if e = s.Insert(ctx, base("applied")); e != nil {
				t.Fatal(e)
			}
			insertHistory("repair-db/test", a.Digest, firstStep, "confirmed", "wrong-execution", p.Digest, a.GuardrailDigest, 1)
		}},
		{"applied_unconfirmed", "executor_partial", func(s *revision.Store) {
			if e = s.Insert(ctx, base("applied")); e != nil {
				t.Fatal(e)
			}
			insertHistory("repair-db/test", a.Digest, firstStep, "intended", a.Digest, p.Digest, a.GuardrailDigest, 1)
		}},
		{"pending_executor_intended", "executor_uncertain", func(s *revision.Store) {
			if e = s.Insert(ctx, base("pending")); e != nil {
				t.Fatal(e)
			}
			insertHistory("repair-db/test", a.Digest, firstStep, "intended", a.Digest, p.Digest, a.GuardrailDigest, 1)
		}},
		{"partial_executor_intended", "executor_uncertain", func(s *revision.Store) {
			if e = s.Insert(ctx, base("partial")); e != nil {
				t.Fatal(e)
			}
			insertHistory("repair-db/test", a.Digest, firstStep, "intended", a.Digest, p.Digest, a.GuardrailDigest, 1)
		}},
		{"target_isolation", "manual_drift", func(s *revision.Store) {
			if e = s.Insert(ctx, base("applied")); e != nil {
				t.Fatal(e)
			}
			insertHistory("other-db/other-env", "sha256:"+strings.Repeat("9", 64), "bad", "intended", "bad", "bad", "bad", 7)
		}},
		{"precedence_dirty_before_executor", "dirty", func(s *revision.Store) {
			if e = s.Insert(ctx, base("partial")); e != nil {
				t.Fatal(e)
			}
			insertHistory("repair-db/test", "sha256:"+strings.Repeat("9", 64), "bad", "intended", "bad", "bad", "bad", 7)
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLive(tc.name == "manual_drift" || tc.name == "target_isolation")
			historyTable()
			rs := fmt.Sprintf("autosql_diag_%d_%d", time.Now().UnixNano(), i)
			store, er := revision.Open(revision.Config{URL: url, Schema: rs})
			if er != nil {
				t.Fatal(er)
			}
			if er = store.Init(ctx); er != nil {
				t.Fatal(er)
			}
			tc.setup(store)
			t.Setenv("AUTOSQL_REPAIR_DIAG_URL", url)
			red := secret.NewRedactor()
			for _, jsonMode := range []bool{false, true} {
				var out, stderr bytes.Buffer
				args := []string{"--url", "env://AUTOSQL_REPAIR_DIAG_URL", "--migration-dir", dir, "--revision-schema", rs, "--schema", "repair_diag"}
				if jsonMode {
					args = append(args, "--json")
				}
				er = runMigrateDiagnose(ctx, args, output{streams: Streams{Out: &out, Err: &stderr}, json: jsonMode, command: "migrate diagnose", redactor: red}, services, red)
				if er != nil {
					t.Fatalf("mode json=%v: %v", jsonMode, er)
				}
				text := out.String()
				wantText := tc.want
				if !jsonMode {
					switch tc.want {
					case "dirty":
						wantText = "incomplete revision evidence"
					case "unknown":
						wantText = "absent from verified manifest"
					case "checksum":
						wantText = "differs from verified manifest"
					case "manual_drift":
						wantText = "live canonical schema differs"
					case "executor_unknown":
						wantText = "untrusted artifact"
					case "executor_duplicate":
						wantText = "duplicate executor step evidence"
					case "executor_linkage":
						wantText = "linkage is inconsistent"
					case "executor_partial":
						wantText = "unconfirmed executor step"
					case "executor_uncertain":
						wantText = "outcome is uncertain"
					}
				}
				if !strings.Contains(text, wantText) {
					t.Fatalf("want %s output=%s", wantText, text)
				}
				if strings.Contains(text, "unknown-secret") || strings.Contains(text, "postgres://user") {
					t.Fatalf("secret leaked: %s", text)
				}
				if tc.want != "manual_drift" && tc.want != "executor_unknown" && !strings.Contains(text, "signed-proposal.json") {
					t.Fatalf("unsafe/missing suggested command: %s", text)
				}
			}
		})
	}
}
