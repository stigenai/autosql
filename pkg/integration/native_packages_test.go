package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativePackage(t *testing.T, path ...string) string {
	t.Helper()
	parts := append([]string{"..", ".."}, path...)
	raw, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestNativePackagesPinIndependentReleaseIdentity(t *testing.T) {
	rootAction := nativePackage(t, "action.yml")
	action := nativePackage(t, "integrations", "github", "action.yml")
	runner := nativePackage(t, "integrations", "github", "run.sh")
	reusable := nativePackage(t, ".github", "workflows", "autosql-reusable.yml")
	gitlab := nativePackage(t, "templates", "autosql", "template.yml")
	circle := nativePackage(t, "integrations", "circleci", "orb.yml")
	pipe := nativePackage(t, "integrations", "bitbucket", "Dockerfile")
	pipeMetadata := nativePackage(t, "integrations", "bitbucket", "pipe.yml")
	azure := nativePackage(t, "integrations", "azure-devops", "tasks", "autosql", "task.json")

	for name, body := range map[string]string{
		"github marketplace action": rootAction,
		"github action":            action,
		"github runner":            runner,
		"github reusable workflow": reusable,
		"gitlab component":         gitlab,
		"circleci orb":             circle,
		"bitbucket pipe":           pipe,
		"bitbucket metadata":       pipeMetadata,
		"azure task":               azure,
	} {
		if strings.Contains(body, ":latest") {
			t.Fatalf("%s references a mutable latest image", name)
		}
	}
	if !strings.Contains(action, "binary-sha256") || !strings.Contains(runner, "INPUT_BINARY_SHA256") {
		t.Fatal("GitHub action does not require an independently reviewed archive digest")
	}
	if !strings.Contains(rootAction, "binary-sha256") || !strings.Contains(rootAction, "github.action_path }}/integrations/github/run.sh") {
		t.Fatal("root GitHub Marketplace action does not use the digest-bound first-party runner")
	}
	if strings.Contains(runner, `/${name}.sha256`) {
		t.Fatal("GitHub action trusts a checksum downloaded from the same mutable release reference")
	}
	if !strings.Contains(reusable, "uses: ./.autosql-action") || !strings.Contains(reusable, "persist-credentials: false") || !strings.Contains(reusable, "binary_sha256") {
		t.Fatal("reusable workflow does not isolate and bind the first-party action checkout")
	}
	if !strings.Contains(gitlab, "ghcr.io/stigenai/autosql@$[[ inputs.image_digest ]]") || strings.Contains(gitlab, "inputs.version") {
		t.Fatal("GitLab component does not require an immutable image digest")
	}
	if !strings.Contains(circle, "ghcr.io/stigenai/autosql@sha256:") {
		t.Fatal("CircleCI orb image is not digest pinned")
	}
	if !strings.Contains(pipe, "ghcr.io/stigenai/autosql@sha256:") {
		t.Fatal("Bitbucket Pipe base is not digest pinned")
	}
	if !strings.Contains(pipeMetadata, "image: ghcr.io/stigenai/autosql-bitbucket-pipe@sha256:") {
		t.Fatal("Bitbucket Pipe metadata is not bound to an immutable image")
	}
	if !strings.Contains(azure, "binarySha256") {
		t.Fatal("Azure task does not require the released archive digest")
	}
}
