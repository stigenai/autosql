package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/migrate"
	"autosql/pkg/secret"
)

func TestGenerationAutomationApprovalRequiresSeparateCurrentAuthority(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROJECT_APPROVAL_KEY", base64.RawStdEncoding.EncodeToString(private))
	base := generationConfig{Author: "author", Requester: "requester", GeneratorKeyID: "generator", SigningKeyID: "release", AutomationApproval: &generationAutomationApproval{KeyID: "approval", Identity: "ci", Roles: []string{"release-automation"}, TTL: "10m", PrivateKeyReference: "env://PROJECT_APPROVAL_KEY"}}
	if err := validateGenerationApproval(base, time.Hour); err != nil {
		t.Fatal(err)
	}
	provider, err := resolveGenerationApproval(context.Background(), base, secret.NewResolver())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	items, _, err := provider.Issue(context.Background(), "bundle", "test", now, now.Add(time.Hour))
	if err != nil || len(items) != 1 {
		t.Fatalf("approval failed: %v", err)
	}
	signed, err := migrate.VerifyAutomationApproval(items[0], "approval", public, approval.Identity{ID: "ci", Roles: base.AutomationApproval.Roles})
	if err != nil || signed.PlanDigest != "bundle" || signed.Environment != "test" || !signed.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("approval is not bound to the exact bundle and lifetime: %v", err)
	}
	for name, mutate := range map[string]func(*generationConfig){
		"author":            func(c *generationConfig) { c.AutomationApproval.Identity = c.Author },
		"requester":         func(c *generationConfig) { c.AutomationApproval.Identity = c.Requester },
		"generator key":     func(c *generationConfig) { c.AutomationApproval.KeyID = c.GeneratorKeyID },
		"release key":       func(c *generationConfig) { c.AutomationApproval.KeyID = c.SigningKeyID },
		"long lifetime":     func(c *generationConfig) { c.AutomationApproval.TTL = "1h" },
		"invalid reference": func(c *generationConfig) { c.AutomationApproval.PrivateKeyReference = "raw-key" },
		"roles":             func(c *generationConfig) { c.AutomationApproval.Roles = nil },
		"mixed approval":    func(c *generationConfig) { c.Approvals = []approval.Approval{{Proof: "other"}} },
	} {
		t.Run(name, func(t *testing.T) {
			c, a := base, *base.AutomationApproval
			c.AutomationApproval = &a
			mutate(&c)
			if err := validateGenerationApproval(c, time.Hour); err == nil {
				t.Fatal("invalid automation authority accepted")
			}
		})
	}
	if provider, err := resolveGenerationApproval(context.Background(), generationConfig{}, nil); provider != nil || err != nil {
		t.Fatal("legacy approval configuration changed")
	}
}
