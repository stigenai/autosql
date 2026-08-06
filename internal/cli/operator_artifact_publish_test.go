package cli

import (
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/safety"
	"autosql/pkg/secret"
)

func TestOperatorAdoptPublishDoesNotRequireProductionCredential(t *testing.T) {
	concurrent := true
	now := time.Now().UTC()
	config := operatorPublishConfig{
		DevelopmentURL:                        secret.Reference("env://AUTOSQL_DEV"),
		Environment:                           "production",
		DatabaseIdentity:                      "orders",
		Author:                                "author",
		Requester:                             "requester",
		PostgresVersion:                       16,
		ConcurrentIndexes:                     &concurrent,
		Schemas:                               []string{"app"},
		PolicyFile:                            "policy.json",
		PolicyIdentity:                        "policy/v1",
		ApprovalPolicy:                        approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"production": {Allowed: true}}},
		AutomationApprovalKeyID:               "approval-key",
		AutomationApprovalIdentity:            "automation",
		AutomationApprovalRoles:               []string{"release-automation"},
		AutomationApprovalPrivateKeyReference: "env://AUTOSQL_APPROVAL_KEY",
		ApprovalTTL:                           "10m",
		ArtifactLifetime:                      "1h",
		GenerationApprovalAuditPath:           "generation-audit.jsonl",
		ApprovalAuditPath:                     "approval-audit.jsonl",
		LifecycleAuditPath:                    "lifecycle-audit.jsonl",
		GeneratorKeyID:                        "generator-key",
		GeneratorPurpose:                      "migration-generator",
		GeneratorPrivateKeyReference:          "env://AUTOSQL_GENERATOR_KEY",
		SigningKeyID:                          "signing-key",
		SigningIssuer:                         "release-issuer",
		SigningIdentity:                       "release-signer",
		SigningPurpose:                        "plan-artifact",
		SigningStatus:                         "active",
		SigningPrivateKeyReference:            "env://AUTOSQL_SIGNING_KEY",
		SigningNotBefore:                      now.Add(-time.Hour),
		SigningNotAfter:                       now.Add(24 * time.Hour),
		OperatorArtifactDirectory:             "artifacts",
	}
	if err := validateOperatorPublishConfig(config, false, true); err != nil {
		t.Fatalf("adoption publish required production credentials: %v", err)
	}
	if err := validateOperatorPublishConfig(config, false, false); err == nil {
		t.Fatal("ordinary transition accepted a missing production credential")
	}
	if err := validateOperatorPublishConfig(config, true, true); err == nil {
		t.Fatal("bootstrap and adoption modes were accepted together")
	}
}

func TestOperatorPublishConfigValidatesSafetySuppressions(t *testing.T) {
	concurrent := true
	now := time.Now().UTC()
	config := operatorPublishConfig{
		DevelopmentURL:                        secret.Reference("env://AUTOSQL_DEV"),
		ProductionURL:                         secret.Reference("env://AUTOSQL_TARGET"),
		Environment:                           "production",
		DatabaseIdentity:                      "orders",
		Author:                                "author",
		Requester:                             "requester",
		PostgresVersion:                       16,
		ConcurrentIndexes:                     &concurrent,
		Schemas:                               []string{"app"},
		PolicyFile:                            "policy.json",
		PolicyIdentity:                        "policy/v1",
		ApprovalPolicy:                        approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"production": {Allowed: true}}},
		AutomationApprovalKeyID:               "approval-key",
		AutomationApprovalIdentity:            "automation",
		AutomationApprovalRoles:               []string{"release-automation"},
		AutomationApprovalPrivateKeyReference: "env://AUTOSQL_APPROVAL_KEY",
		ApprovalTTL:                           "10m",
		ArtifactLifetime:                      "1h",
		GenerationApprovalAuditPath:           "generation-audit.jsonl",
		ApprovalAuditPath:                     "approval-audit.jsonl",
		LifecycleAuditPath:                    "lifecycle-audit.jsonl",
		GeneratorKeyID:                        "generator-key",
		GeneratorPurpose:                      "migration-generator",
		GeneratorPrivateKeyReference:          "env://AUTOSQL_GENERATOR_KEY",
		SigningKeyID:                          "signing-key",
		SigningIssuer:                         "release-issuer",
		SigningIdentity:                       "release-signer",
		SigningPurpose:                        "plan-artifact",
		SigningStatus:                         "active",
		SigningPrivateKeyReference:            "env://AUTOSQL_SIGNING_KEY",
		SigningNotBefore:                      now.Add(-time.Hour),
		SigningNotAfter:                       now.Add(24 * time.Hour),
		OperatorArtifactDirectory:             "artifacts",
	}
	config.SafetySuppressions = []safety.Suppression{{Rule: safety.RuleDropObject, ObjectID: "table:abc", Reason: "approved destructive change"}}
	if err := validateOperatorPublishConfig(config, false, false); err != nil {
		t.Fatalf("valid suppression rejected: %v", err)
	}
	config.SafetySuppressions = []safety.Suppression{{Rule: safety.RuleDropObject, ObjectID: "table:abc"}}
	if err := validateOperatorPublishConfig(config, false, false); err == nil {
		t.Fatal("a suppression without a reason was accepted")
	}
}
