package migrate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/postgres"
)

func TestAutomationApprovalProviderBindsFreshProofToBundleAndEnvironment(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Truncate(time.Second)
	provider := AutomationApprovalProvider{KeyID: "ci-approval-2026-07", Identity: approval.Identity{ID: "github-actions", Roles: []string{"release-automation"}}, Actors: []approval.Identity{{ID: "schema-author"}, {ID: "release-requester"}}, PrivateKey: private, TTL: 10 * time.Minute}
	items, authority, err := provider.Issue(context.Background(), "sha256:"+strings.Repeat("a", 64), "production", created, created.Add(time.Hour))
	if err != nil || len(items) != 1 {
		t.Fatalf("issue items=%d err=%v", len(items), err)
	}
	verified, err := authority.VerifyApproval(context.Background(), items[0])
	if err != nil {
		t.Fatal(err)
	}
	if verified.Identity.ID != "github-actions" || verified.PlanDigest != "sha256:"+strings.Repeat("a", 64) || verified.Environment != "production" || !verified.ApprovedAt.Equal(created) || !verified.ExpiresAt.Equal(created.Add(10*time.Minute)) {
		t.Fatalf("unexpected claims: %+v", verified)
	}
	if actor, err := authority.ResolveActor(context.Background(), "schema-author"); err != nil || actor.ID != "schema-author" {
		t.Fatalf("author was not trusted: %+v %v", actor, err)
	}
	for name, mutate := range map[string]func(approval.Approval) approval.Approval{
		"signature": func(item approval.Approval) approval.Approval { item.Proof += "x"; return item },
		"proof":     func(item approval.Approval) approval.Approval { item.Proof = "invalid"; return item },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := authority.VerifyApproval(context.Background(), mutate(items[0])); err == nil {
				t.Fatal("tampered automation approval verified")
			}
		})
	}
}

func TestOperatorBootstrapArtifactRenderBindsCompleteInventory(t *testing.T) {
	inventory := postgres.BootstrapAuthorizationInventory{
		Routines: []postgres.BootstrapRoutineAuthorization{
			{SourceDigest: "sha256:b", UnsafeLanguageAuthorizationRequired: true},
			{SourceDigest: "sha256:a", PrivilegedRoutineAuthorizationRequired: true, TransactionControlAuthorizationRequired: true},
		},
		Extensions: []postgres.BootstrapExtensionAuthorization{
			{Name: "pgcrypto", Version: "1.3", Schema: "app"},
			{Name: "hstore", Version: "1.8", Schema: "app", UntrustedExtensionAuthorizationRequired: true},
		},
	}
	render := operatorBootstrapArtifactRender(inventory, map[string]string{"postgres_version": "16", "concurrent_indexes": "true"})
	want := map[string]string{
		"postgres_version": "16", "concurrent_indexes": "true",
		"reviewed_routine_digests": "sha256:a,sha256:b", "extension_allowlist": "hstore,pgcrypto",
		"extension_version.hstore": "1.8", "extension_schemas.hstore": "app",
		"extension_version.pgcrypto": "1.3", "extension_schemas.pgcrypto": "app",
		"allow_unsafe_routine_languages": "true", "allow_privileged_routines": "true",
		"allow_transaction_control_procedures": "true", "allow_untrusted_extensions": "true",
	}
	if len(render) != len(want) {
		t.Fatalf("render=%v want=%v", render, want)
	}
	for key, value := range want {
		if render[key] != value {
			t.Fatalf("render[%q]=%q want %q", key, render[key], value)
		}
	}
}
