package operatorcontroller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/internal/cli"
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/operator"
	"github.com/jackc/pgx/v5"
)

// Runs in the real publish/apply fixture, sharing its generated proof and
// owned target. No synthetic history or successful execution audit is seeded.
func assertPublishedCompletionRecovery(t *testing.T, ctx context.Context, resource operator.Resource, published artifact.Artifact, configPath string) {
	t.Helper()
	prepared, err := cli.PrepareProductionCompletion(ctx, published)
	if err != nil {
		t.Fatalf("published release failed current authorization: %v", err)
	}
	if prepared.Database.URL != "" || !prepared.ValidUntil.Before(published.ExpiresAt) {
		t.Fatal("completion resolved credentials or lost the signed approval expiry")
	}
	if err := RevalidateArtifactCompletion(ctx, resource); err != nil {
		t.Fatalf("source binding: %v", err)
	}
	conn, err := pgx.Connect(ctx, resource.ResolvedDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	history := func() string {
		t.Helper()
		var rows string
		if err := conn.QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(h) order by step_id)::text,'[]') from autosql_migration_history h`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := history()
	auditBefore, err := os.ReadFile(prepared.Audit.Path)
	if err != nil {
		t.Fatal(err)
	}
	store := &failSecondCheckpoint{Store: operator.NewMemoryStore(), saves: 1}
	r := operator.Reconciler{Store: store, Observe: ObserveArtifactCompletion, Probe: ProbeArtifactExecution, Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		t.Fatal("completion recovery replayed Apply")
		return operator.ApplyResult{}, nil
	}}
	started := time.Now()
	status, err := r.Reconcile(ctx, resource, true)
	var persistence *operator.StatePersistenceError
	if !errors.As(err, &persistence) || recoveredReady(status) {
		t.Fatalf("failed checkpoint became Ready: %+v %v", status, err)
	}
	status, err = r.Reconcile(ctx, resource, true)
	if err != nil || !recoveredReady(status) || status.AppliedDigest != published.Digest || status.AppliedFingerprint != published.Plan.ToFingerprint || status.CompletionObservedAt.IsZero() || time.Since(started) > 2*time.Minute {
		t.Fatalf("completion not recovered: %+v %v", status, err)
	}
	if history() != before {
		t.Fatal("observation changed database execution history")
	}
	auditAfter, err := os.ReadFile(prepared.Audit.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(auditAfter, auditBefore) || bytes.Count(auditAfter, []byte("\n")) != bytes.Count(auditBefore, []byte("\n"))+1 {
		t.Fatal("recovery did not append exactly one observation")
	}
	events, err := prepared.Audit.ReadExecution(ctx, published.Digest)
	if err != nil || events[len(events)-1].Type != "completion_observed" {
		t.Fatalf("observation audit missing: %v", err)
	}
	beforeControllerAudit := append([]byte(nil), auditAfter...)
	assertControllerRecoversLostStores(t, resource)
	if history() != before {
		t.Fatal("controller recovery changed history")
	}
	auditAfter, err = os.ReadFile(prepared.Audit.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(auditAfter, beforeControllerAudit) || bytes.Count(auditAfter, []byte("\n")) != bytes.Count(beforeControllerAudit, []byte("\n"))+1 {
		t.Fatal("controller replacement/conflicts/reference rotation duplicated the same observation")
	}
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	assertPublishedReleaseExpiresForObservation(t, published, configBytes)
	for _, kind := range []string{"legacy approval", "revoked key", "removed role", "changed source", "unresolved URL source", "missing audit"} {
		t.Run(kind, func(t *testing.T) {
			current := resource
			var config map[string]any
			if err := json.Unmarshal(configBytes, &config); err != nil {
				t.Fatal(err)
			}
			trust := config["TrustedMigrations"].(map[string]any)[published.Digest].(map[string]any)
			switch kind {
			case "legacy approval":
				delete(trust, "ApprovalEvidence")
			case "revoked key":
				config["KeyStatus"] = "revoked"
			case "removed role":
				identities := trust["ApprovalIdentities"].(map[string]any)
				identity := identities[published.Approval.ProofDigest].(map[string]any)
				identity["roles"] = []string{}
			case "changed source":
				current.ResolvedSource = "CREATE SCHEMA different;"
				current.Spec.Source.Format = "sql"
			case "unresolved URL source":
				current.Spec.Source = operator.Source{URL: "https://schema.example.invalid/desired.sql"}
				current.ResolvedSource = current.Spec.Source.URL
			case "missing audit":
				if err := os.Remove(prepared.Audit.Path); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(prepared.Audit.Path, auditAfter, 0600); err != nil {
						t.Error(err)
					}
				})
			}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
					t.Error(err)
				}
			})
			result := ObserveArtifactCompletion(ctx, current)
			if result.Evidence.Outcome == executor.CompletionProven || !result.AuthorizationExpiresAt.IsZero() {
				t.Fatalf("invalid current authorization/evidence accepted: %+v", result)
			}
			if history() != before {
				t.Fatal("refused observation changed history")
			}
		})
	}
}

func assertPublishedReleaseExpiresForObservation(t *testing.T, published artifact.Artifact, raw []byte) {
	t.Helper()
	var c struct {
		KeyID, PublicKey, Issuer, Signer, KeyPurpose, KeyStatus string
		KeyNotBefore, KeyNotAfter                               time.Time
		GeneratorKeyID, GeneratorPublicKey, GeneratorPurpose    string
		TrustedMigrations                                       map[string]struct {
			Expected                 artifact.ExpectedBindings
			ValidationContextDigests map[string]string
			ValidationAttestations   map[string]artifact.ValidationAttestation
		}
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(c.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	generator, err := base64.RawStdEncoding.Strict().DecodeString(c.GeneratorPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	trust := c.TrustedMigrations[published.Digest]
	policy := artifact.VerifyPolicy{Now: time.Now, NoEdits: true, Expected: trust.Expected, ExpectedValidationContextDigests: trust.ValidationContextDigests, ExpectedValidationAttestations: trust.ValidationAttestations,
		Issuer: c.Issuer, Identity: c.Signer, Purpose: c.KeyPurpose, GeneratorPurpose: c.GeneratorPurpose,
		Keys:          map[string]artifact.KeyRecord{c.KeyID: {PublicKey: ed25519.PublicKey(key), Issuer: c.Issuer, Identity: c.Signer, Environment: published.TargetEnvironment, Purpose: c.KeyPurpose, Status: c.KeyStatus, NotBefore: c.KeyNotBefore, NotAfter: c.KeyNotAfter}},
		GeneratorKeys: map[string]artifact.KeyRecord{c.GeneratorKeyID: {PublicKey: ed25519.PublicKey(generator), Purpose: c.GeneratorPurpose}},
	}
	if _, err := published.VerifyForObservation(policy); err != nil {
		t.Fatalf("current published release refused: %v", err)
	}
	// Exercise the exact published bytes and trusted attestations at the expiry
	// boundary. Only the library clock is advanced; no production clock seam.
	policy.Now = func() time.Time { return published.ExpiresAt }
	if _, err := published.VerifyForObservation(policy); err == nil || !strings.Contains(err.Error(), "attestation_time") {
		t.Fatalf("expired published validation evidence accepted: %v", err)
	}
}
