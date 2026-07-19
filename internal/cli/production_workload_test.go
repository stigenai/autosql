package cli

import (
	"context"
	"strings"
	"testing"

	"autosql/pkg/secret"
	"autosql/pkg/workloadidentity"
)

func TestProductionApplyResolvesOnlyOneCredentialSource(t *testing.T) {
	previous := resolveProductionWorkloadIdentity
	t.Cleanup(func() { resolveProductionWorkloadIdentity = previous })
	resolveProductionWorkloadIdentity = func(_ context.Context, binding workloadidentity.Binding) (string, error) {
		if binding.Provider != workloadidentity.AzurePG || binding.Audience != "api://AzureADTokenExchange" {
			t.Fatalf("binding=%+v", binding)
		}
		return "postgresql://autosql:super-secret-token@orders.postgres.database.azure.com:5432/orders?sslmode=verify-full", nil
	}
	binding := &workloadidentity.Binding{Provider: workloadidentity.AzurePG, Host: "orders.postgres.database.azure.com", Port: 5432, User: "autosql", Database: "orders", TLSMode: "verify-full", Audience: "api://AzureADTokenExchange", Subject: "system:serviceaccount:autosql:operator"}
	resolver := secret.NewResolver()
	url, err := resolveApplyDatabaseURL(context.Background(), applyConfig{WorkloadIdentity: binding}, "", resolver)
	if err != nil || !strings.Contains(url, "super-secret-token") {
		t.Fatalf("url resolved=%t err=%v", strings.Contains(url, "super-secret-token"), err)
	}
	if strings.Contains(resolver.Redactor.String("failure "+url), "super-secret-token") {
		t.Fatal("workload token was not redacted")
	}
	if _, err := resolveApplyDatabaseURL(context.Background(), applyConfig{DatabaseURL: "env://DATABASE_URL", WorkloadIdentity: binding}, "", resolver); err == nil {
		t.Fatal("multiple credential sources accepted")
	}
	if _, err := resolveApplyDatabaseURL(context.Background(), applyConfig{}, "", resolver); err == nil {
		t.Fatal("missing credential source accepted")
	}
}
