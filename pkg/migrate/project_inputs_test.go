package migrate

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/policy"
	"autosql/pkg/schema"
)

func TestProjectGenerationRefusesMissingOrForgedInputClaims(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, request := range []GenerateRequest{
		{ProjectInputDigest: digest},
		{ExpectedProjectMigrationDigest: digest},
		{ProjectInputDigest: "report", ExpectedProjectMigrationDigest: digest},
		{ProjectInputDigest: digest, ExpectedProjectMigrationDigest: "report"},
		{Metadata: map[string]string{artifact.ProjectInputsMetadata: "forged"}},
	} {
		if err := validateProjectGeneration(request); !errors.Is(err, ErrGenerateConfig) {
			t.Fatalf("incomplete project binding accepted: %v", err)
		}
	}
	for _, request := range []GenerateRequest{{}, {ProjectInputDigest: digest, ExpectedProjectMigrationDigest: digest}} {
		if err := validateProjectGeneration(request); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProjectGenerationChecksCapturedSnapshotBeforeReplay(t *testing.T) {
	dir := trustedDir(t)
	manifest, err := Update(dir, UpdateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := GenerateRequest{Directory: dir, Version: "1", Label: "demo", Format: "sql", DevelopmentURL: "must-not-connect", DevelopmentIdentity: "development", ProductionIdentity: "production", Environment: "prod", DatabaseIdentity: "production", SourceRevision: "git:test", Author: "author", Requester: "requester", PostgresVersion: 17, CreatedAt: now, ExpiresAt: now.Add(time.Hour), GeneratorKeyID: "generator", GeneratorPurpose: "migration", GeneratorPrivateKey: private, SigningKeyID: "release", SigningPrivateKey: private, PolicyIdentity: "policy", Policy: policy.Document{Version: policy.LanguageVersion, Rules: []policy.Rule{{Name: "allow", Target: "all", Assert: policy.Expression{Eq: []any{true, true}}}}}, Desired: schema.Document{Version: schema.SchemaVersion}, ApprovalProvider: AutomationApprovalProvider{}, ApprovalAudit: &approval.Chain{}, ProjectInputDigest: "sha256:" + strings.Repeat("a", 64), ExpectedProjectMigrationDigest: manifest.Digest}
	var stages []string
	request.Stage = func(stage string) error {
		stages = append(stages, stage)
		if stage == "replay" {
			return errors.New("stop before any database access")
		}
		return nil
	}
	_, err = (GenerateService{}).Generate(context.Background(), request)
	if !errors.Is(err, ErrGenerateStage) || strings.Join(stages, ",") != "snapshot,replay" {
		t.Fatalf("valid captured snapshot did not reach replay boundary: %v stages=%v", err, stages)
	}
	if _, err := Update(dir, UpdateRequest{ExpectedManifestDigest: manifest.Digest, Files: []File{{Name: "V1__new.sql", SQL: []byte("SELECT 1;")}}}); err != nil {
		t.Fatal(err)
	}
	stages = nil
	_, err = (GenerateService{}).Generate(context.Background(), request)
	if !errors.Is(err, ErrGenerateConflict) || strings.Join(stages, ",") != "snapshot" {
		t.Fatalf("changed migration input reached replay: %v stages=%v", err, stages)
	}
}
