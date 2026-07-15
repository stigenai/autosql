package operatorcontroller

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestRBACManifestCoversControllerOperations(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "operator", "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var role, binding, serviceAccount *unstructured.Unstructured
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		item := &unstructured.Unstructured{Object: object}
		switch item.GetKind() {
		case "Role":
			role = item
		case "RoleBinding":
			binding = item
		case "ServiceAccount":
			serviceAccount = item
		}
	}
	if role == nil || binding == nil || serviceAccount == nil {
		t.Fatalf("incomplete RBAC manifest: role=%t binding=%t serviceAccount=%t", role != nil, binding != nil, serviceAccount != nil)
	}
	rules, found, err := unstructured.NestedSlice(role.Object, "rules")
	if err != nil || !found {
		t.Fatalf("role rules: found=%t err=%v", found, err)
	}
	requireRule(t, rules, "autosql.io", "autosqlschemas", "get", "list", "watch", "patch", "update")
	requireRule(t, rules, "autosql.io", "autosqlschemas/status", "patch", "update")
	requireRule(t, rules, "", "secrets", "get")
	requireRule(t, rules, "", "configmaps", "get")
	requireRule(t, rules, "coordination.k8s.io", "leases", "get", "create", "update", "patch")
	subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
	if len(subjects) != 1 || subjects[0].(map[string]any)["name"] != serviceAccount.GetName() {
		t.Fatalf("role binding does not target service account %q", serviceAccount.GetName())
	}
}

func requireRule(t *testing.T, rules []any, apiGroup, resource string, verbs ...string) {
	t.Helper()
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok || !containsString(rule["apiGroups"], apiGroup) || !containsString(rule["resources"], resource) {
			continue
		}
		for _, verb := range verbs {
			if !containsString(rule["verbs"], verb) {
				t.Fatalf("RBAC resource %s missing verb %s", resource, verb)
			}
		}
		return
	}
	t.Fatalf("RBAC rule missing apiGroup=%q resource=%q", apiGroup, resource)
}

func containsString(value any, want string) bool {
	values, ok := value.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
