package migrate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
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
