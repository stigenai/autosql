package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"autosql/pkg/integration"
	"autosql/pkg/integrations/deploy"
	"autosql/pkg/integrations/gitops"
)

func conformanceImage() integration.Image {
	return integration.Image{Name: "ghcr.io/acme/autosql", Digest: "sha256:" + strings.Repeat("d", 64), Version: "v1", Signature: "sig", SBOM: "sbom", Scanned: true, Reproducible: true}
}

type conformanceRunner struct{}

func (conformanceRunner) Run(context.Context, deploy.Request) (string, error) { return "ok", nil }
func (conformanceRunner) Observe(context.Context, string) (deploy.Result, error) {
	return deploy.Result{}, nil
}
func (conformanceRunner) Cancel(context.Context, string) error { return nil }

func TestProviderDeliveryConformance(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	p := "sha256:" + strings.Repeat("b", 64)
	target := "sha256:" + strings.Repeat("c", 64)
	platforms := []gitops.Platform{gitops.CircleCI, gitops.Bitbucket, gitops.AzureDevOps, gitops.ArgoCD, gitops.GitHub, gitops.GitLab}
	for _, platform := range platforms {
		t.Run(string(platform), func(t *testing.T) {
			c := gitops.Contract{Platform: platform, Version: "v1", Mode: gitops.Deploy, ArtifactRef: "env://AUTOSQL_ARTIFACT", ArtifactDigest: d, PolicyDigest: p, TargetSnapshot: target, ApprovalRef: "env://AUTOSQL_APPROVAL", OIDC: true, Image: conformanceImage(), Retry: gitops.RetryPolicy{MaxAttempts: 2, Backoff: time.Second}}
			if _, err := gitops.Render(c); err != nil {
				t.Fatal(err)
			}
			digest, err := c.BindingDigest()
			if err != nil {
				t.Fatal(err)
			}
			if err := (gitops.Check{ContractDigest: digest, Status: gitops.StatusPassed}).Validate(c); err != nil {
				t.Fatal(err)
			}
			bad := c
			bad.ArtifactDigest = "sha256:" + strings.Repeat("f", 64)
			if err := (gitops.Check{ContractDigest: digest, Status: gitops.StatusPassed}).Validate(bad); err == nil {
				t.Fatal("stale digest accepted")
			}
			bad = c
			bad.ArtifactRef = "https://user:password@example/db"
			if err := bad.Validate(); err == nil {
				t.Fatal("credential reference accepted")
			}
		})
	}
	request := deploy.Request{DeploymentID: "run-1", ArtifactDigest: d, TargetSnapshot: target, TargetID: "db-1", Environment: "prod", ConnectionRef: "env://DATABASE_URL", Action: deploy.Apply}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	plans := deploy.NewStore()
	first, err := plans.Apply(context.Background(), request, conformanceRunner{})
	if err != nil || first.ArtifactDigest != d {
		t.Fatalf("plan=%+v err=%v", first, err)
	}
	if _, err := plans.Apply(context.Background(), deploy.Request{DeploymentID: "run-1", ArtifactDigest: "sha256:" + strings.Repeat("f", 64), TargetSnapshot: target, TargetID: "db-1", Environment: "prod", Action: deploy.Apply}, conformanceRunner{}); err == nil {
		t.Fatal("stale deployment accepted")
	}
	if err := (deploy.Request{DeploymentID: "run-2", ArtifactDigest: d, Environment: "prod", Action: deploy.Apply, ConnectionRef: "postgres://secret"}).Validate(); err == nil {
		t.Fatal("resolved credential accepted")
	}
}
