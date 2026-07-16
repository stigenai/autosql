package postgres_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/internal/cli"
	"autosql/internal/operatorcontroller"
	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/guardrail"
	"autosql/pkg/operator"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"autosql/pkg/source"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestSemanticCellCLISignedBootstrapAgainstNewPostgresDatabase(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	const namespace = "autosql_semantic_cli_cell"
	desired, config, owner, major, cleanup := semanticCellFixture(t, ctx, url, namespace)
	defer cleanup()
	target := completeCellExecutionTarget(config, owner, "autosql_complete_cli")
	defer postgres.DropDatabaseURL(context.Background(), url, target.Name, true)

	document := desired
	document.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
	targetSpec, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	document.Graph.Resources = append(document.Graph.Resources, schema.Resource{ID: schema.StableID(schema.KindDatabase, schema.Name{Name: target.Name}), Kind: schema.KindDatabase, Name: schema.Name{Name: target.Name}, Spec: targetSpec})
	document.Normalize()
	hcl, err := source.FormatHCL(document)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	hclPath := filepath.Join(directory, "complete-cell.hcl")
	manifestPath := filepath.Join(directory, "authorization.json")
	if err := os.WriteFile(hclPath, hcl, 0o600); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_COMPLETE_CLI_PRIVATE", base64.RawStdEncoding.EncodeToString(private))
	t.Setenv("AUTOSQL_COMPLETE_CLI_PUBLIC", base64.RawStdEncoding.EncodeToString(public))
	t.Setenv("AUTOSQL_COMPLETE_CLI_MAINTENANCE", url)
	var stdout, stderr bytes.Buffer
	common := []string{"--file", hclPath, "--postgres-version", fmt.Sprint(major), "--json"}
	prepare := append([]string{"database", "bootstrap", "prepare"}, common...)
	if code := cli.Run(ctx, prepare, cli.Streams{Out: &stdout, Err: &stderr}); code != 0 {
		t.Fatalf("complete CLI prepare code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var prepareEnvelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &prepareEnvelope); err != nil || !prepareEnvelope.OK {
		t.Fatalf("decode complete CLI prepare envelope: ok=%v err=%v stdout=%s", prepareEnvelope.OK, err, stdout.String())
	}
	var prepared postgres.BootstrapAuthorizationInventory
	if err := json.Unmarshal(prepareEnvelope.Data, &prepared); err != nil {
		t.Fatalf("decode complete CLI authorization inventory: %v", err)
	}
	if err := prepared.Validate(); err != nil {
		t.Fatalf("validate complete CLI prepared inventory: %v", err)
	}
	if prepared.Database != target.Name {
		t.Fatalf("CLI prepared database=%q want=%q", prepared.Database, target.Name)
	}
	assertSemanticAuthorization(t, prepared)
	if prepared.PlanSummary.StepCount < len(desired.Graph.Resources) || prepared.PlanSummary.PhaseCount < 10 {
		t.Fatalf("semantic CLI prepared plan is unexpectedly small: %+v resources=%d", prepared.PlanSummary, len(desired.Graph.Resources))
	}
	stdout.Reset()
	stderr.Reset()
	authorize := append([]string{"database", "bootstrap", "authorize"}, common...)
	authorize = append(authorize, "--authorization-signing-key", "env://AUTOSQL_COMPLETE_CLI_PRIVATE", "--authorization-key-id", "complete-cli-key", "--authorization-issuer", "complete-cell-security", "--authorization-signer", "complete-cell-reviewers", "--output", manifestPath)
	if code := cli.Run(ctx, authorize, cli.Streams{Out: &stdout, Err: &stderr}); code != 0 {
		t.Fatalf("complete CLI authorize code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var authorizeEnvelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Status       string `json:"status"`
			Manifest     string `json:"manifest"`
			PlanDigest   string `json:"plan_digest"`
			SourceDigest string `json:"source_digest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &authorizeEnvelope); err != nil || !authorizeEnvelope.OK {
		t.Fatalf("decode complete CLI authorize envelope: ok=%v err=%v stdout=%s", authorizeEnvelope.OK, err, stdout.String())
	}
	if authorizeEnvelope.Data.Status != "authorized" || authorizeEnvelope.Data.Manifest != manifestPath || authorizeEnvelope.Data.PlanDigest != prepared.PlanDigest || authorizeEnvelope.Data.SourceDigest != prepared.SourceDigest {
		t.Fatalf("CLI authorize output is not bound to prepared inventory: prepared=%+v authorized=%+v", prepared.PlanSummary, authorizeEnvelope.Data)
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	signedManifest, err := postgres.ParseBootstrapAuthorizationManifest(manifestBytes)
	if err != nil {
		t.Fatalf("parse complete CLI signed authorization manifest: %v", err)
	}
	if signedManifest.Database != prepared.Database || signedManifest.PlanDigest != prepared.PlanDigest || signedManifest.SourceDigest != prepared.SourceDigest || signedManifest.SchemaPlanDigest != prepared.PlanSummary.SchemaPlanDigest {
		t.Fatalf("CLI signed manifest identity does not match prepared inventory: database=%q/%q plan=%q/%q source=%q/%q schema_plan=%q/%q", signedManifest.Database, prepared.Database, signedManifest.PlanDigest, prepared.PlanDigest, signedManifest.SourceDigest, prepared.SourceDigest, signedManifest.SchemaPlanDigest, prepared.PlanSummary.SchemaPlanDigest)
	}
	if signedManifest.Issuer != "complete-cell-security" || signedManifest.Signer != "complete-cell-reviewers" || signedManifest.Purpose != "bootstrap-authorization" || signedManifest.Signature.KeyID != "complete-cli-key" {
		t.Fatalf("CLI signed manifest authority identity=%q/%q/%q key=%q", signedManifest.Issuer, signedManifest.Signer, signedManifest.Purpose, signedManifest.Signature.KeyID)
	}
	execute := append([]string{"database", "bootstrap"}, common...)
	execute = append(execute, "--maintenance-url", "env://AUTOSQL_COMPLETE_CLI_MAINTENANCE", "--authorization-manifest", manifestPath, "--authorization-public-key", "env://AUTOSQL_COMPLETE_CLI_PUBLIC", "--authorization-issuer", "complete-cell-security", "--authorization-signer", "complete-cell-reviewers")
	for attempt := 1; attempt <= 2; attempt++ {
		stdout.Reset()
		stderr.Reset()
		if code := cli.Run(ctx, execute, cli.Streams{Out: &stdout, Err: &stderr}); code != 0 {
			t.Fatalf("complete CLI execute attempt=%d code=%d stdout=%s stderr=%s", attempt, code, stdout.String(), stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), config.Password) || strings.Contains(stdout.String()+stderr.String(), base64.RawStdEncoding.EncodeToString(private)) {
			t.Fatal("complete CLI output leaked credentials")
		}
	}
	assertSemanticTargetZeroChange(t, ctx, config, target, desired, namespace, major, "cli")
}

func TestSemanticCellProductionOperatorSignedBootstrapAgainstNewPostgresDatabase(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	const namespace = "autosql_semantic_operator_cell"
	desired, config, owner, major, cleanup := semanticCellFixture(t, ctx, url, namespace)
	defer cleanup()
	target := completeCellExecutionTarget(config, owner, "autosql_complete_operator")
	defer postgres.DropDatabaseURL(context.Background(), url, target.Name, true)
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	operatorDesired, err := source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	operatorDesired, err = postgres.New().Normalize(ctx, operatorDesired)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, operatorDesired, desired)
	desired = operatorDesired
	render := map[string]string{"postgres_version": fmt.Sprint(major), "concurrent_indexes": "true"}
	inventory, err := postgres.PrepareBootstrapAuthorizationInventory(ctx, target, desired, postgres.BootstrapAuthorizationInventoryOptions{Render: render})
	if err != nil {
		t.Fatal(err)
	}
	assertSemanticAuthorization(t, inventory)
	authorization := signCompleteBootstrapAuthorization(t, inventory)
	whole, err := postgres.PlanDatabaseBootstrapAuthorized(ctx, target, desired, plan.Options{Render: render}, authorization.Verified)
	if err != nil {
		t.Fatal(err)
	}
	assertSemanticPlan(t, whole)
	checks := completeCellArtifactChecks(t, whole.SchemaPlan)
	releasePublic, releasePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	generatorPublic, generatorPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	guardrailDigest := "sha256:" + strings.Repeat("9", 64)
	release, err := artifact.NewGenerated(whole.SchemaPlan, checks, now, now.Add(time.Hour), inventory.SourceDigest, "complete-cell", target.Name, guardrailDigest, artifact.Approval{Identity: "complete-cell-release", ApprovedAt: now}, map[string]string{}, "complete-generator-key", "migration-generator", generatorPrivate)
	if err == nil {
		err = release.Sign("complete-release-key", releasePrivate)
	}
	if err != nil {
		t.Fatal(err)
	}
	releaseBytes, err := release.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	parsedRelease, err := artifact.Parse(releaseBytes)
	if err != nil {
		t.Fatalf("parse semantic operator release before publication: bytes=%d err=%v", len(releaseBytes), err)
	}
	if parsedRelease.Digest != release.Digest {
		t.Fatalf("semantic operator release digest=%q want=%q", parsedRelease.Digest, release.Digest)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, release.Digest+".json"), releaseBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	applyConfig := map[string]any{
		"DatabaseURL": "env://AUTOSQL_COMPLETE_OPERATOR_MAINTENANCE", "Environment": "complete-cell", "DatabaseIdentity": target.Name, "SourceRevision": inventory.SourceDigest,
		"KeyID": "complete-release-key", "PublicKey": base64.RawStdEncoding.EncodeToString(releasePublic), "Issuer": "release-issuer", "Signer": "release-signer", "Author": "author", "Requester": "requester",
		"ApprovalAuditPath": filepath.Join(directory, "approval.jsonl"), "LifecycleAuditPath": filepath.Join(directory, "lifecycle.jsonl"), "ArtifactDirectory": directory,
		"PostgresVersion": major, "Schemas": []string{namespace}, "ExpectedPlanDigest": whole.SchemaPlan.Digest, "ExpectedChecksDigest": checks.Digest, "ExpectedGuardrailDigest": guardrailDigest,
		"ExpectedApprovalIdentity": "complete-cell-release", "KeyStatus": "active", "KeyPurpose": "plan-artifact", "KeyNotBefore": now.Add(-time.Hour), "KeyNotAfter": now.Add(2 * time.Hour), "NoEdits": false,
		"GeneratorKeyID": "complete-generator-key", "GeneratorPublicKey": base64.RawStdEncoding.EncodeToString(generatorPublic), "GeneratorPurpose": "migration-generator",
	}
	configBytes, err := json.Marshal(applyConfig)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "apply-config.json")
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_OPERATOR_ARTIFACT_DIR", directory)
	t.Setenv("AUTOSQL_APPLY_CONFIG", configPath)
	t.Setenv("AUTOSQL_COMPLETE_OPERATOR_MAINTENANCE", url)
	targetConfig := config.Copy()
	targetConfig.Database = target.Name
	authority := completeCellAuthority(owner)
	resource := operator.Resource{Name: "complete/cell", Spec: operator.Spec{
		Kind: operator.Declarative, Generation: 1, Source: operator.Source{Format: "hcl", Inline: string(hcl)}, ArtifactDigest: release.Digest,
		DatabaseURL: &operator.SecretKeyRef{Name: "target", Key: "url"}, MaintenanceDatabaseURL: &operator.SecretKeyRef{Name: "maintenance", Key: "url"},
		DatabaseTarget: &target, BootstrapAuthority: &authority, PostgresVersion: major, ConcurrentIndexes: true, BootstrapAuthorization: &operator.BootstrapAuthorizationRef{
			ManifestSecretRef: operator.SecretKeyRef{Name: "authorization", Key: "manifest"}, PublicKeySecretRef: operator.SecretKeyRef{Name: "authorization", Key: "public"},
			Issuer: "complete-cell-security", Signer: "complete-cell-reviewers", Purpose: "bootstrap-authorization",
		}, CreateDatabase: true,
	}, ResolvedSource: string(hcl), ResolvedDatabaseURL: targetConfig.ConnString(), ResolvedMaintenanceDatabaseURL: url, ResolvedAuthorizationManifest: authorization.Manifest, ResolvedAuthorizationPublicKey: authorization.Public}
	specBytes, err := json.Marshal(resource.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var specMap map[string]any
	if err := json.Unmarshal(specBytes, &specMap); err != nil {
		t.Fatal(err)
	}
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "autosql.io/v1alpha1", "kind": "AutoSQLSchema", "metadata": map[string]any{"name": "cell", "namespace": "complete", "annotations": map[string]any{"autosql.io/approved": "true"}}, "spec": specMap}}
	object.SetGroupVersionKind(operatorcontroller.GroupVersionKind)
	targetSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "complete"}, Data: map[string][]byte{"url": []byte(targetConfig.ConnString())}}
	maintenanceSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "maintenance", Namespace: "complete"}, Data: map[string][]byte{"url": []byte(url)}}
	authorizationSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "authorization", Namespace: "complete"}, Data: map[string][]byte{"manifest": authorization.Manifest, "public": authorization.Public}}
	client := fake.NewClientBuilder().WithScheme(operatorcontroller.NewScheme()).WithObjects(object, targetSecret, maintenanceSecret, authorizationSecret).WithStatusSubresource(object).Build()
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "complete", Name: "cell"}}
	for attempt := 1; attempt <= 2; attempt++ {
		controller := &operatorcontroller.Controller{Client: client, Store: operator.NewMemoryStore(), Apply: operatorcontroller.ArtifactApply}
		if _, err := controller.Reconcile(ctx, request); err != nil {
			t.Fatalf("complete Kubernetes operator attempt=%d err=%v", attempt, err)
		}
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(operatorcontroller.GroupVersionKind)
		if err := client.Get(ctx, request.NamespacedName, current); err != nil {
			t.Fatal(err)
		}
		planDigest, _, _ := unstructured.NestedString(current.Object, "status", "planDigest")
		targetIdentity, _, _ := unstructured.NestedString(current.Object, "status", "targetIdentity")
		fingerprint, _, _ := unstructured.NestedString(current.Object, "status", "appliedFingerprint")
		if planDigest != whole.Digest || targetIdentity != target.Name || fingerprint == "" {
			t.Fatalf("complete Kubernetes operator attempt=%d status=%+v", attempt, current.Object["status"])
		}
	}
	assertSemanticTargetZeroChange(t, ctx, config, target, desired, namespace, major, "operator")
}

func completeCellExecutionTarget(config *pgx.ConnConfig, owner, prefix string) bootstrap.DatabaseTarget {
	return bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: config.Host, Port: uint16(config.Port), TLSMode: "disable"}, MaintenanceDatabase: config.Database, Name: fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()), Owner: owner, Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true}
}

func completeCellArtifactChecks(t testing.TB, schemaPlan plan.Plan) precheck.Plan {
	t.Helper()
	changeDigest, err := guardrail.ChangeDigest(schemaPlan.Changes)
	if err != nil {
		t.Fatal(err)
	}
	statements := make([]string, 0, len(schemaPlan.Steps))
	for _, step := range schemaPlan.Steps {
		if step.Kind == plan.StepExecutable {
			statements = append(statements, step.SQL)
		}
	}
	checks := precheck.Plan{ID: "complete-cell-operator-checks", ChangeDigest: changeDigest, Statements: statements}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	return checks
}

func completeCellAuthority(owner string) bootstrap.Contract {
	capabilities := []bootstrap.Capability{bootstrap.CreateDatabase, bootstrap.ManageRoles, bootstrap.ManageExtensions, bootstrap.ManageSchema, bootstrap.ManageGrants, bootstrap.TransferOwnership}
	responsibilities := []bootstrap.Responsibility{bootstrap.DatabaseCreation, bootstrap.RoleCreation, bootstrap.ExtensionSetup, bootstrap.SchemaObjects, bootstrap.GrantSetup, bootstrap.OwnershipHandoff}
	assignments := make([]bootstrap.Assignment, 0, len(responsibilities))
	for _, responsibility := range responsibilities {
		assignments = append(assignments, bootstrap.Assignment{Responsibility: responsibility, Identity: "operator"})
	}
	return bootstrap.Contract{Identities: []bootstrap.Identity{{Name: "operator", Subject: owner, Authentication: bootstrap.CurrentSession, Capabilities: capabilities}}, Assignments: assignments}
}
