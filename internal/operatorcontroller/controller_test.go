package operatorcontroller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/guardrail"
	"autosql/pkg/operator"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	autosqlschema "autosql/pkg/schema"
	"autosql/pkg/workloadidentity"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func testObject() *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autosql.io/v1alpha1", "kind": "AutoSQLSchema",
		"metadata": map[string]any{"name": "orders", "namespace": "app"},
		"spec": map[string]any{
			"kind": "VersionedMigration", "generation": int64(1), "requireApproval": true,
			"artifactDigest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"source":         map[string]any{"registryDigest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			"databaseURL":    map[string]any{"name": "db", "key": "url"},
		},
	}}
	o.SetGroupVersionKind(GroupVersionKind)
	return o
}

func TestResolveReferencesUsesTransientWorkloadIdentity(t *testing.T) {
	previous := resolveWorkloadIdentityURL
	t.Cleanup(func() { resolveWorkloadIdentityURL = previous })
	resolveWorkloadIdentityURL = func(_ context.Context, binding workloadidentity.Binding) (string, time.Time, error) {
		if binding.Provider != workloadidentity.AWSRDS || binding.Audience != "sts.amazonaws.com" || binding.Host != "db.example.test" {
			t.Fatalf("binding=%+v", binding)
		}
		return "postgresql://autosql:super-secret-token@db.example.test:5432/app?sslmode=verify-full", time.Now().Add(15 * time.Minute), nil
	}
	identity := workloadidentity.Binding{Provider: workloadidentity.AWSRDS, Host: "db.example.test", Port: 5432, User: "autosql", Database: "app", TLSMode: "verify-full", Region: "us-east-1", Audience: "sts.amazonaws.com", Subject: "system:serviceaccount:autosql:operator"}
	resource := operator.Resource{Spec: operator.Spec{Kind: operator.Versioned, Source: operator.Source{RegistryDigest: "sha256:" + strings.Repeat("a", 64)}, DatabaseIdentity: &identity}}
	if err := (&Controller{}).resolveReferences(context.Background(), "autosql", &resource); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resource.ResolvedDatabaseURL, "super-secret-token") {
		t.Fatal("transient connection was not resolved")
	}
	raw, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-token") {
		t.Fatalf("token persisted: %s", raw)
	}
	if resource.DatabaseBinding.ContentDigest == "" || strings.Contains(resource.DatabaseBinding.ContentDigest, "super-secret-token") {
		t.Fatalf("unsafe binding=%+v", resource.DatabaseBinding)
	}
}

func TestReconcileUpdatesStatusAndHonorsApproval(t *testing.T) {
	obj := testObject()
	scheme := NewScheme()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app"}, Data: map[string][]byte{"url": []byte("postgres://operator:secret@db/app")}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj, secret).WithStatusSubresource(obj).Build()
	calls := 0
	c := &Controller{Client: cl, Store: operator.NewMemoryStore(), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		calls++
		return operator.ApplyResult{PlanDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetIdentity: "prod/orders", ExecutionID: "exec-1"}, nil
	}}
	ctx := context.Background()
	if _, err := c.Reconcile(ctx, requestFor(obj)); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("approval gate bypassed: %d calls", calls)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GroupVersionKind)
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
		t.Fatal(err)
	}
	conditions, _, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	approvalFound := false
	for _, raw := range conditions {
		if condition, ok := raw.(map[string]any); ok && condition["type"] == string(operator.Approval) && condition["status"] == "True" {
			approvalFound = true
		}
	}
	if err != nil || !approvalFound {
		t.Fatalf("approval status: %#v err=%v", conditions, err)
	}
	annotations := got.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["autosql.io/approved"] = "true"
	got.SetAnnotations(annotations)
	if err := cl.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reconcile(ctx, requestFor(got)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("apply calls=%d", calls)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
		t.Fatal(err)
	}
	if got := got.Object["status"].(map[string]any)["executionID"]; got != "exec-1" {
		t.Fatalf("execution ID status=%v", got)
	}
}

func TestSuspendedReconcileDoesNotResolveRuntimeReferences(t *testing.T) {
	obj := testObject()
	obj.Object["spec"].(map[string]any)["suspend"] = true
	scheme := NewScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).WithStatusSubresource(obj).Build()
	calls := 0
	c := &Controller{Client: cl, Store: operator.NewMemoryStore(), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		calls++
		return operator.ApplyResult{}, nil
	}}
	if _, err := c.Reconcile(context.Background(), requestFor(obj)); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("suspended reconciliation applied %d times", calls)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GroupVersionKind)
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
		t.Fatal(err)
	}
	conditions, _, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	if err != nil || len(conditions) != 1 {
		t.Fatalf("suspended conditions=%#v err=%v", conditions, err)
	}
	condition, ok := conditions[0].(map[string]any)
	if !ok || condition["type"] != string(operator.Ready) || condition["reason"] != "Suspended" || condition["status"] != "True" {
		t.Fatalf("suspended condition=%#v", conditions[0])
	}
}

func requestFor(obj *unstructured.Unstructured) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}
}

func acceptReleaseForControllerUnitTest(string) (artifact.VerifiedArtifact, error) {
	return artifact.VerifiedArtifact{}, nil
}

func TestResourceFromObjectParsesOIDCBootstrapAuthority(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["createDatabase"] = true
	spec["bootstrapAuthority"] = map[string]any{
		"identities": []any{map[string]any{
			"name": "operator", "subject": "system:serviceaccount:autosql:operator", "authentication": "oidc",
			"capabilities": []any{"create_database", "manage_schema_objects", "transfer_ownership"},
		}},
		"assignments": []any{
			map[string]any{"responsibility": "database_creation", "identity": "operator"},
			map[string]any{"responsibility": "schema_objects", "identity": "operator"},
			map[string]any{"responsibility": "ownership_handoff", "identity": "operator"},
		},
	}
	resource, _, err := resourceFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !resource.Spec.CreateDatabase || resource.Spec.BootstrapAuthority == nil || resource.Spec.BootstrapAuthority.Identities[0].Authentication != bootstrap.OIDC {
		t.Fatalf("resource=%+v", resource.Spec)
	}
}

func TestResourceFromObjectParsesDatabaseTargetContract(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["maintenanceDatabaseURL"] = map[string]any{"name": "maintenance-db", "key": "url"}
	spec["databaseTarget"] = map[string]any{
		"mode": "managed", "name": "cell", "owner": "cell_owner", "maintenanceDatabase": "postgres",
		"endpoint":        map[string]any{"host": "postgres.internal", "port": int64(5432), "tlsMode": "require"},
		"connectionLimit": int64(20), "allowConnections": true,
	}
	resource, _, err := resourceFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if resource.Spec.DatabaseTarget == nil || resource.Spec.DatabaseTarget.Name != "cell" || resource.Spec.MaintenanceDatabaseURL == nil || resource.Spec.MaintenanceDatabaseURL.Name != "maintenance-db" {
		t.Fatalf("resource=%+v", resource.Spec)
	}
}

func TestResourceFromObjectParsesExplicitSourceFormat(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["kind"] = "DeclarativeSchema"
	spec["source"] = map[string]any{"format": "hcl", "inline": `schema "app" {}`}
	spec["postgresVersion"] = int64(18)
	spec["concurrentIndexes"] = true
	spec["adoptionPolicy"] = "IfEquivalent"
	resource, _, err := resourceFromObject(obj)
	if err != nil || resource.Spec.Source.Format != "hcl" || resource.Spec.PostgresVersion != 18 || !resource.Spec.ConcurrentIndexes || resource.Spec.AdoptionPolicy != operator.AdoptIfEquivalent {
		t.Fatalf("resource=%+v err=%v", resource.Spec.Source, err)
	}
}

func TestResourceFromObjectRejectsNonStringAdoptionPolicy(t *testing.T) {
	obj := testObject()
	obj.Object["spec"].(map[string]any)["adoptionPolicy"] = true
	if _, _, err := resourceFromObject(obj); err == nil || !strings.Contains(err.Error(), "adoptionPolicy must be a string") {
		t.Fatalf("non-string adoption policy error=%v", err)
	}
}

func TestResourceFromObjectRejectsInvalidSourceContracts(t *testing.T) {
	tests := map[string]map[string]any{
		"format on URL":      {"format": "sql", "url": "https://schemas.example.test/app.sql"},
		"format on registry": {"format": "hcl", "registryDigest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		"unknown format":     {"format": "json", "inline": "{}"},
		"unknown field":      {"inline": "create schema app", "privateSource": "forbidden"},
		"multiple variants":  {"inline": "create schema app", "url": "https://schemas.example.test/app.sql"},
		"nested unknown":     {"secretRef": map[string]any{"name": "source", "key": "schema", "namespace": "other"}},
		"missing ref key":    {"configMapRef": map[string]any{"name": "source"}},
		"boolean format":     {"inline": "create schema app", "format": true},
		"numeric URL":        {"inline": "create schema app", "url": int64(42)},
		"null inline":        {"inline": nil},
		"array registry":     {"registryDigest": []any{"sha256:bad"}},
		"boolean secret ref": {"secretRef": false},
		"array ConfigMap ref": {"configMapRef": []any{
			map[string]any{"name": "source", "key": "schema"},
		}},
		"numeric ref name": {"secretRef": map[string]any{"name": int64(7), "key": "schema"}},
		"null ref key":     {"configMapRef": map[string]any{"name": "source", "key": nil}},
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			obj := testObject()
			obj.Object["spec"].(map[string]any)["source"] = source
			if _, _, err := resourceFromObject(obj); err == nil {
				t.Fatalf("invalid source accepted: %#v", source)
			}
		})
	}
}

type runtimeReferenceReadSpy struct {
	client.Client
	reads int
}

func TestInvalidBootstrapReleasePerformsNoKubernetesReferenceReadsOrMutation(t *testing.T) {
	ctx := context.Background()
	namespace := autosqlschema.Resource{Kind: autosqlschema.KindSchema, Name: autosqlschema.Name{Name: "app"}, Spec: []byte(`{}`)}
	namespace.ID = autosqlschema.StableID(namespace.Kind, namespace.Name)
	desired := autosqlschema.Document{Version: autosqlschema.SchemaVersion, Graph: autosqlschema.Graph{Resources: []autosqlschema.Resource{namespace}}}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", Port: 5432, TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "cell", Owner: "postgres", Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true}
	whole, err := postgres.PlanDatabaseBootstrap(ctx, target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	changeDigest, err := guardrail.ChangeDigest(whole.SchemaPlan.Changes)
	if err != nil {
		t.Fatal(err)
	}
	checks := precheck.Plan{ID: "pre-reference-release", ChangeDigest: changeDigest}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	releasePublic, releasePrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	generatorPublic, generatorPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	guardrailDigest := "sha256:" + strings.Repeat("9", 64)
	generated := func() artifact.Artifact {
		a, createErr := artifact.NewGenerated(whole.SchemaPlan, checks, now, now.Add(time.Hour), "source", "production", target.Name, guardrailDigest, artifact.Approval{Identity: "release", ApprovedAt: now}, map[string]string{}, "generator-key", "migration-generator", generatorPrivate)
		if createErr == nil {
			createErr = a.Sign("release-key", releasePrivate)
		}
		if createErr != nil {
			t.Fatal(createErr)
		}
		return a
	}
	unattested, err := artifact.New(whole.SchemaPlan, checks, now, now.Add(time.Hour), "source", "production", target.Name, guardrailDigest, artifact.Approval{Identity: "release", ApprovedAt: now}, map[string]string{})
	if err == nil {
		err = unattested.Sign("release-key", releasePrivate)
	}
	if err != nil {
		t.Fatal(err)
	}
	edited := generated()
	edited.MarkEditedOrigin("review-editor")
	if err := edited.ResetAuthorization(); err == nil {
		err = edited.Sign("release-key", releasePrivate)
	}
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	config := map[string]any{
		"DatabaseURL": "env://AUTOSQL_PRECHECK_MUST_NOT_READ", "Environment": "production", "DatabaseIdentity": target.Name, "SourceRevision": "source",
		"KeyID": "release-key", "PublicKey": base64.RawStdEncoding.EncodeToString(releasePublic), "Issuer": "release-issuer", "Signer": "release-signer", "Author": "author", "Requester": "requester",
		"ApprovalAuditPath": filepath.Join(directory, "approval.jsonl"), "LifecycleAuditPath": filepath.Join(directory, "lifecycle.jsonl"), "ArtifactDirectory": directory,
		"PostgresVersion": 18, "Schemas": []string{"app"}, "ExpectedPlanDigest": whole.SchemaPlan.Digest, "ExpectedChecksDigest": checks.Digest, "ExpectedGuardrailDigest": guardrailDigest,
		"ExpectedApprovalIdentity": "release", "KeyStatus": "active", "KeyPurpose": "plan-artifact", "KeyNotBefore": now.Add(-time.Hour), "KeyNotAfter": now.Add(2 * time.Hour), "NoEdits": false,
		"GeneratorKeyID": "generator-key", "GeneratorPublicKey": base64.RawStdEncoding.EncodeToString(generatorPublic), "GeneratorPurpose": "migration-generator",
	}
	configBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "apply-config.json")
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_APPLY_CONFIG", configPath)
	t.Setenv("AUTOSQL_OPERATOR_ARTIFACT_DIR", directory)
	t.Setenv("AUTOSQL_PRECHECK_MUST_NOT_READ", "")

	for name, candidate := range map[string]artifact.Artifact{"unattested": unattested, "edited": edited, "missing provenance": generated()} {
		t.Run(name, func(t *testing.T) {
			raw, marshalErr := candidate.MarshalCanonical()
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if name == "missing provenance" {
				var object map[string]any
				if err := json.Unmarshal(raw, &object); err != nil {
					t.Fatal(err)
				}
				delete(object, "origin")
				raw, err = json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(directory, candidate.Digest+".json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			obj := testObject()
			spec := obj.Object["spec"].(map[string]any)
			spec["kind"] = "DeclarativeSchema"
			spec["artifactDigest"] = candidate.Digest
			spec["source"] = map[string]any{"secretRef": map[string]any{"name": "source", "key": "schema"}, "format": "hcl"}
			spec["maintenanceDatabaseURL"] = map[string]any{"name": "maintenance", "key": "url"}
			spec["databaseTarget"] = map[string]any{"mode": "managed", "name": target.Name, "owner": target.Owner, "maintenanceDatabase": target.MaintenanceDatabase, "endpoint": map[string]any{"host": target.Endpoint.Host, "port": int64(target.Endpoint.Port), "tlsMode": target.Endpoint.TLSMode}, "connectionLimit": int64(-1), "allowConnections": true}
			spec["bootstrapAuthorization"] = map[string]any{"manifestSecretRef": map[string]any{"name": "authorization", "key": "manifest"}, "publicKeySecretRef": map[string]any{"name": "authorization", "key": "public"}, "issuer": "security", "signer": "dba", "purpose": "bootstrap-authorization"}
			base := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj).WithStatusSubresource(obj).Build()
			spy := &runtimeReferenceReadSpy{Client: base}
			mutations := 0
			controller := &Controller{Client: spy, Reader: spy, Store: operator.NewMemoryStore(), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
				mutations++
				return operator.ApplyResult{}, nil
			}}
			if _, reconcileErr := controller.Reconcile(ctx, requestFor(obj)); reconcileErr == nil {
				t.Fatal("invalid release accepted")
			}
			if spy.reads != 0 || mutations != 0 {
				t.Fatalf("invalid release reached Kubernetes references or mutation: reads=%d mutations=%d", spy.reads, mutations)
			}
		})
	}
}

func (s *runtimeReferenceReadSpy) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	switch object.(type) {
	case *corev1.Secret, *corev1.ConfigMap:
		s.reads++
	}
	return s.Client.Get(ctx, key, object, options...)
}

func TestAdoptionAuthenticatesReleaseBeforeDatabaseSecret(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["kind"] = "DeclarativeSchema"
	spec["source"] = map[string]any{"format": "hcl", "inline": `schema "app" {}`}
	spec["adoptionPolicy"] = "IfEquivalent"
	spec["requireApproval"] = false
	base := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj).WithStatusSubresource(obj).Build()
	spy := &runtimeReferenceReadSpy{Client: base}
	verified := 0
	controller := &Controller{Client: spy, Reader: spy, Store: operator.NewMemoryStore(), VerifyRelease: func(string) (artifact.VerifiedArtifact, error) {
		verified++
		return artifact.VerifiedArtifact{}, fmt.Errorf("invalid adoption release")
	}, Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		t.Fatal("invalid adoption release reached apply")
		return operator.ApplyResult{}, nil
	}}
	if _, err := controller.Reconcile(context.Background(), requestFor(obj)); err == nil || !strings.Contains(err.Error(), "invalid adoption release") {
		t.Fatalf("adoption verification error=%v", err)
	}
	if verified != 1 || spy.reads != 0 {
		t.Fatalf("verified=%d runtime reference reads=%d", verified, spy.reads)
	}
}

func TestInvalidSourceContractPerformsNoRuntimeReferenceReadsOrMutation(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["kind"] = "DeclarativeSchema"
	spec["source"] = map[string]any{"inline": "create schema app", "format": true}
	spec["maintenanceDatabaseURL"] = map[string]any{"name": "maintenance", "key": "url"}
	spec["bootstrapAuthorization"] = map[string]any{
		"manifestSecretRef": map[string]any{"name": "authorization", "key": "manifest"}, "publicKeySecretRef": map[string]any{"name": "authorization", "key": "public"},
		"issuer": "security", "signer": "dba", "purpose": "bootstrap-authorization",
	}
	base := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj).WithStatusSubresource(obj).Build()
	spy := &runtimeReferenceReadSpy{Client: base}
	mutations := 0
	controller := &Controller{Client: spy, Reader: spy, Store: operator.NewMemoryStore(), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		mutations++
		return operator.ApplyResult{}, nil
	}}
	if _, err := controller.Reconcile(context.Background(), requestFor(obj)); err == nil || !strings.Contains(err.Error(), "source format must be a string") {
		t.Fatalf("invalid source contract error=%v", err)
	}
	if spy.reads != 0 || mutations != 0 {
		t.Fatalf("invalid source reached runtime references or mutation: reads=%d mutations=%d", spy.reads, mutations)
	}
}

func TestInvalidInlineSourceFailsBeforeDatabaseCredentialResolution(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["kind"] = "DeclarativeSchema"
	spec["source"] = map[string]any{"format": "hcl", "inline": "create schema app; -- SQL declared as HCL"}
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj).WithStatusSubresource(obj).Build()
	controller := &Controller{Client: cl, Store: operator.NewMemoryStore(), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		t.Fatal("apply reached")
		return operator.ApplyResult{}, nil
	}}
	_, err := controller.Reconcile(context.Background(), requestFor(obj))
	if err == nil || !strings.Contains(err.Error(), "declared hcl format") || strings.Contains(err.Error(), "database Secret") {
		t.Fatalf("format mismatch did not precede credential resolution: %v", err)
	}
}

func TestBootstrapAuthorizationReferencesAreRuntimeOnlyAndStatusIsPrecise(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      operator.AuthorizationState
		withSecret bool
	}{
		{name: "missing", state: operator.AuthorizationMissing},
		{name: "invalid", state: operator.AuthorizationInvalid, withSecret: true},
		{name: "stale", state: operator.AuthorizationStale, withSecret: true},
		{name: "accepted", state: operator.AuthorizationAccepted, withSecret: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := testObject()
			spec := obj.Object["spec"].(map[string]any)
			spec["kind"] = "DeclarativeSchema"
			spec["source"] = map[string]any{"inline": "create schema app"}
			spec["databaseTarget"] = map[string]any{
				"mode": "external", "name": "cell", "owner": "postgres", "maintenanceDatabase": "postgres",
				"endpoint":        map[string]any{"host": "db.internal", "port": int64(5432), "tlsMode": "verify-full"},
				"connectionLimit": int64(-1), "allowConnections": true,
			}
			spec["requireApproval"] = false
			spec["bootstrapAuthorization"] = map[string]any{
				"manifestSecretRef":  map[string]any{"name": "bootstrap-auth", "key": "manifest"},
				"publicKeySecretRef": map[string]any{"name": "bootstrap-auth", "key": "public-key"},
				"issuer":             "security", "signer": "dba", "purpose": "bootstrap-authorization",
			}
			database := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app"}, Data: map[string][]byte{"url": []byte("postgres://operator:do-not-leak@db/app")}}
			objects := []client.Object{obj, database}
			if tc.withSecret {
				objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-auth", Namespace: "app"}, Data: map[string][]byte{"manifest": []byte("manifest-do-not-leak"), "public-key": []byte("public-key-do-not-leak")}})
			}
			cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(objects...).WithStatusSubresource(obj).Build()
			recorder := record.NewFakeRecorder(4)
			controller := &Controller{Client: cl, Store: operator.NewMemoryStore(), Recorder: recorder, VerifyRelease: acceptReleaseForControllerUnitTest, Apply: func(_ context.Context, resource operator.Resource, _ string) (operator.ApplyResult, error) {
				if string(resource.ResolvedAuthorizationManifest) != "manifest-do-not-leak" || string(resource.ResolvedAuthorizationPublicKey) != "public-key-do-not-leak" {
					t.Fatal("authorization Secret values were not resolved transiently")
				}
				if tc.state == operator.AuthorizationAccepted {
					return operator.ApplyResult{AuthorizationState: operator.AuthorizationAccepted}, nil
				}
				return operator.ApplyResult{}, &operator.AuthorizationError{State: tc.state}
			}}
			_, reconcileErr := controller.Reconcile(context.Background(), requestFor(obj))
			if tc.state == operator.AuthorizationAccepted && reconcileErr != nil {
				t.Fatal(reconcileErr)
			}
			if tc.state != operator.AuthorizationAccepted && reconcileErr == nil {
				t.Fatal("authorization failure did not fail closed")
			}
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(GroupVersionKind)
			if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
				t.Fatal(err)
			}
			raw, _ := got.MarshalJSON()
			text := string(raw)
			for _, forbidden := range []string{"do-not-leak", "manifest-do-not-leak", "public-key-do-not-leak"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("resolved secret leaked into object/status: %s", forbidden)
				}
			}
			conditions, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
			found := false
			for _, rawCondition := range conditions {
				condition, _ := rawCondition.(map[string]any)
				if condition["type"] == string(operator.Authorization) && condition["status"] == "True" && condition["reason"] == string(tc.state) {
					found = true
				}
			}
			if !found {
				t.Fatalf("authorization condition %s missing: %#v", tc.state, conditions)
			}
			if tc.state == operator.AuthorizationAccepted {
				replacementCalls := 0
				replacement := &Controller{Client: cl, Store: operator.NewMemoryStore(), VerifyRelease: acceptReleaseForControllerUnitTest, Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
					replacementCalls++
					return operator.ApplyResult{AuthorizationState: operator.AuthorizationAccepted}, nil
				}}
				if _, err := replacement.Reconcile(context.Background(), requestFor(got)); err != nil {
					t.Fatal(err)
				}
				if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
					t.Fatal(err)
				}
				conditions, _, _ = unstructured.NestedSlice(got.Object, "status", "conditions")
				found = false
				for _, rawCondition := range conditions {
					condition, _ := rawCondition.(map[string]any)
					found = found || (condition["type"] == string(operator.Authorization) && condition["reason"] == string(operator.AuthorizationAccepted))
				}
				if !found || replacementCalls != 1 {
					t.Fatalf("replacement did not reverify and preserve accepted authorization: calls=%d conditions=%#v", replacementCalls, conditions)
				}
			}
			select {
			case event := <-recorder.Events:
				if !strings.Contains(event, "BootstrapAuthorization"+string(tc.state)) || strings.Contains(event, "do-not-leak") {
					t.Fatalf("unsafe or imprecise event: %s", event)
				}
			default:
				t.Fatal("authorization event was not emitted")
			}
		})
	}
}

func TestResourceFromObjectRejectsAuthorizationPrivateKeyFields(t *testing.T) {
	obj := testObject()
	obj.Object["spec"].(map[string]any)["bootstrapAuthorization"] = map[string]any{
		"manifestSecretRef":  map[string]any{"name": "auth", "key": "manifest"},
		"publicKeySecretRef": map[string]any{"name": "auth", "key": "public"},
		"issuer":             "security", "signer": "dba", "purpose": "bootstrap-authorization", "privateKey": "forbidden",
	}
	if _, _, err := resourceFromObject(obj); err == nil {
		t.Fatal("private signing material was accepted by operator spec parser")
	}
}

func TestWriteFailureMergesAuthorizationConditionWithInjectedClock(t *testing.T) {
	obj := testObject()
	transition := "2026-07-16T10:00:00Z"
	obj.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": string(operator.Ready), "status": "True", "reason": "Applied", "message": "converged", "observedGeneration": int64(1), "lastTransitionTime": transition},
		map[string]any{"type": string(operator.Authorization), "status": "True", "reason": string(operator.AuthorizationAccepted), "message": "accepted", "observedGeneration": int64(1), "lastTransitionTime": transition},
	}}
	spec := obj.Object["spec"].(map[string]any)
	spec["kind"] = "DeclarativeSchema"
	spec["source"] = map[string]any{"inline": "create schema app"}
	spec["databaseTarget"] = map[string]any{"mode": "external", "name": "cell", "owner": "postgres", "maintenanceDatabase": "postgres", "endpoint": map[string]any{"host": "db.internal"}, "connectionLimit": int64(-1), "allowConnections": true}
	spec["bootstrapAuthorization"] = map[string]any{"manifestSecretRef": map[string]any{"name": "missing", "key": "manifest"}, "publicKeySecretRef": map[string]any{"name": "missing", "key": "public"}, "issuer": "security", "signer": "dba", "purpose": "bootstrap-authorization"}
	database := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app", ResourceVersion: "7"}, Data: map[string][]byte{"url": []byte("postgres://runtime")}}
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj, database).WithStatusSubresource(obj).Build()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	controller := &Controller{Client: cl, Store: operator.NewMemoryStore(), VerifyRelease: acceptReleaseForControllerUnitTest, Now: func() time.Time { return now }, Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		t.Fatal("apply reached")
		return operator.ApplyResult{}, nil
	}}
	if _, err := controller.Reconcile(context.Background(), requestFor(obj)); err == nil {
		t.Fatal("missing authorization did not fail")
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GroupVersionKind)
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
		t.Fatal(err)
	}
	conditions, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	ready, missing := false, false
	for _, raw := range conditions {
		condition := raw.(map[string]any)
		ready = ready || condition["type"] == string(operator.Ready) && condition["status"] == "True"
		missing = missing || condition["type"] == string(operator.Authorization) && condition["reason"] == string(operator.AuthorizationMissing) && condition["lastTransitionTime"] == now.Format(time.RFC3339Nano)
	}
	if !ready || !missing {
		t.Fatalf("conditions were replaced or used wall clock: %#v", conditions)
	}
}

func TestSecretWatchMapsAuthorizationReferences(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["bootstrapAuthorization"] = map[string]any{"manifestSecretRef": map[string]any{"name": "authorization", "key": "manifest"}, "publicKeySecretRef": map[string]any{"name": "authorization", "key": "public"}, "issuer": "security", "signer": "dba", "purpose": "bootstrap-authorization"}
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj).Build()
	controller := &Controller{Client: cl}
	requests := controller.requestsForSecret(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "authorization", Namespace: "app"}})
	if len(requests) != 1 || requests[0].Name != "orders" || requests[0].Namespace != "app" {
		t.Fatalf("secret watch requests=%+v", requests)
	}
}

func TestConfigMapWatchMapsSourceReference(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["kind"] = "DeclarativeSchema"
	spec["source"] = map[string]any{"configMapRef": map[string]any{"name": "desired", "key": "schema.hcl"}}
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj).Build()
	controller := &Controller{Client: cl}
	requests := controller.requestsForConfigMap(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "desired", Namespace: "app"}})
	if len(requests) != 1 || requests[0].Name != "orders" || requests[0].Namespace != "app" {
		t.Fatalf("ConfigMap watch requests=%+v", requests)
	}
	if requests := controller.requestsForConfigMap(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "app"}}); len(requests) != 0 {
		t.Fatalf("unreferenced ConfigMap requests=%+v", requests)
	}
}

func TestSourceConfigMapRotationDeletionAndRestartForceReverification(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["kind"] = "DeclarativeSchema"
	spec["source"] = map[string]any{"configMapRef": map[string]any{"name": "desired", "key": "schema.hcl"}}
	spec["requireApproval"] = false
	database := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app"}, Data: map[string][]byte{"url": []byte("postgres://runtime")}}
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "desired", Namespace: "app"}, Data: map[string]string{"schema.hcl": "# schema-v1-secret-marker\nschema \"app\" {}"}}
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj, database, configMap).WithStatusSubresource(obj).Build()
	calls := 0
	apply := func(_ context.Context, resource operator.Resource, _ string) (operator.ApplyResult, error) {
		calls++
		if strings.Contains(resource.ResolvedSource, "secret-marker") && resource.ResolvedSource != "# schema-v1-secret-marker\nschema \"app\" {}" && resource.ResolvedSource != "# schema-v2-secret-marker\nschema \"app\" {}" {
			t.Fatalf("unexpected resolved source %q", resource.ResolvedSource)
		}
		return operator.ApplyResult{PlanDigest: fmt.Sprintf("sha256:%064d", calls), SourceDigest: fmt.Sprintf("sha256:%064d", calls+10)}, nil
	}
	ctx := context.Background()
	controller := &Controller{Client: cl, Store: operator.NewMemoryStore(), Apply: apply}
	if _, err := controller.Reconcile(ctx, requestFor(obj)); err != nil {
		t.Fatal(err)
	}
	current := &corev1.ConfigMap{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "app", Name: "desired"}, current); err != nil {
		t.Fatal(err)
	}
	current.Data["schema.hcl"] = "# schema-v2-secret-marker\nschema \"app\" {}"
	if err := cl.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reconcile(ctx, requestFor(obj)); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("ConfigMap rotation did not force reverify: calls=%d", calls)
	}
	// A replacement controller has no trustworthy local apply state and must
	// re-resolve/reverify even when Kubernetes status contains prior digests.
	controller = &Controller{Client: cl, Store: operator.NewMemoryStore(), Apply: apply}
	if _, err := controller.Reconcile(ctx, requestFor(obj)); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("controller restart trusted status for ConfigMap source: calls=%d", calls)
	}
	if err := cl.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reconcile(ctx, requestFor(obj)); err == nil || strings.Contains(err.Error(), "secret-marker") {
		t.Fatalf("deleted ConfigMap error leaked content or was accepted: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GroupVersionKind)
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
		t.Fatal(err)
	}
	statusJSON, err := json.Marshal(got.Object["status"])
	if err != nil || strings.Contains(string(statusJSON), "secret-marker") {
		t.Fatalf("status leaked ConfigMap content: %s err=%v", statusJSON, err)
	}
}

func TestAuthorizationSecretRotationAndDeletionForceReverification(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["kind"] = "DeclarativeSchema"
	spec["source"] = map[string]any{"inline": "create schema app"}
	spec["requireApproval"] = false
	spec["databaseTarget"] = map[string]any{"mode": "external", "name": "cell", "owner": "postgres", "maintenanceDatabase": "postgres", "endpoint": map[string]any{"host": "db.internal"}, "connectionLimit": int64(-1), "allowConnections": true}
	spec["bootstrapAuthorization"] = map[string]any{"manifestSecretRef": map[string]any{"name": "authorization", "key": "manifest"}, "publicKeySecretRef": map[string]any{"name": "authorization", "key": "public"}, "issuer": "security", "signer": "dba", "purpose": "bootstrap-authorization"}
	database := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app"}, Data: map[string][]byte{"url": []byte("postgres://runtime")}}
	authorization := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "authorization", Namespace: "app"}, Data: map[string][]byte{"manifest": []byte("manifest-v1"), "public": []byte("public-v1")}}
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj, database, authorization).WithStatusSubresource(obj).Build()
	calls := 0
	controller := &Controller{Client: cl, Store: operator.NewMemoryStore(), VerifyRelease: acceptReleaseForControllerUnitTest, Apply: func(_ context.Context, resource operator.Resource, _ string) (operator.ApplyResult, error) {
		calls++
		return operator.ApplyResult{AuthorizationState: operator.AuthorizationAccepted, AuthorizationExpiresAt: time.Now().Add(time.Hour), SourceDigest: fmt.Sprintf("sha256:%064d", calls)}, nil
	}}
	ctx := context.Background()
	if _, err := controller.Reconcile(ctx, requestFor(obj)); err != nil {
		t.Fatal(err)
	}
	currentSecret := &corev1.Secret{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "app", Name: "authorization"}, currentSecret); err != nil {
		t.Fatal(err)
	}
	currentSecret.Data["manifest"] = []byte("manifest-v2")
	if err := cl.Update(ctx, currentSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reconcile(ctx, requestFor(obj)); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("Secret rotation did not force reverify: calls=%d", calls)
	}
	if err := cl.Delete(ctx, currentSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Reconcile(ctx, requestFor(obj)); err == nil {
		t.Fatal("deleted authorization Secret remained accepted")
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GroupVersionKind)
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
		t.Fatal(err)
	}
	conditions, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	missing := false
	for _, raw := range conditions {
		condition := raw.(map[string]any)
		missing = missing || condition["type"] == string(operator.Authorization) && condition["reason"] == string(operator.AuthorizationMissing)
	}
	if !missing {
		t.Fatalf("deleted Secret status=%#v", conditions)
	}
}
