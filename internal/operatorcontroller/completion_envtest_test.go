package operatorcontroller

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/operator"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnvtestCompletionTimestampSchema(t *testing.T) {
	if os.Getenv("AUTOSQL_OPERATOR_ENVTEST") != "1" {
		t.Skip("set AUTOSQL_OPERATOR_ENVTEST=1 to run the Kubernetes API-server integration test")
	}
	root := filepath.Join("..", "..", "deploy")
	standalone, err := os.ReadFile(filepath.Join(root, "operator", "crd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	helm, err := os.ReadFile(filepath.Join(root, "helm", "autosql-operator", "crds", "autosqlschema.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(standalone, helm) {
		t.Fatal("standalone and Helm CRDs differ; both require qualification")
	}
	crds := t.TempDir()
	if err := os.WriteFile(filepath.Join(crds, "autosqlschema.yaml"), standalone, 0600); err != nil {
		t.Fatal(err)
	}
	testEnv := &envtest.Environment{CRDDirectoryPaths: []string{crds}}
	config, err := testEnv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Error(err)
		}
	})
	cl, err := client.New(config, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	obj := testObject()
	obj.SetNamespace("default")
	if err := cl.Create(ctx, obj); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	if err := cl.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	if !statusFromObject(obj).CompletionObservedAt.IsZero() {
		t.Fatal("legacy object acquired a timestamp")
	}
	now := time.Now().UTC()
	status := operator.UpdateCondition(operator.Status{ObservedGeneration: obj.GetGeneration(), AppliedDigest: "sha256:" + strings.Repeat("a", 64), CompletionObservedAt: now}, operator.Ready, "CompletionRecovered", "observed", obj.GetGeneration(), now)
	if err := writeStatus(ctx, cl, obj, status); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	if !statusFromObject(obj).CompletionObservedAt.Equal(now) {
		t.Fatal("API server pruned or changed completionObservedAt")
	}
	if err := unstructured.SetNestedField(obj.Object, "not-a-timestamp", "status", "completionObservedAt"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Status().Update(ctx, obj); !apierrors.IsInvalid(err) {
		t.Fatalf("API server accepted invalid timestamp: %v", err)
	}
}
