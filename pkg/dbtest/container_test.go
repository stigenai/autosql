package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"autosql/pkg/config"
)

func testContainerFactory() ContainerFactory {
	return ContainerFactory{Name: "container", Classification: "development", Definition: config.DevelopmentFactory{Mode: "container", Endpoint: "unix:///unavailable/docker.sock", Allowlist: []string{"unix:///unavailable/docker.sock"}, Image: "postgres@sha256:" + strings.Repeat("a", 64), PostgresVersion: 17}}
}

func TestContainerFactoryRefusesUnsafeConfigurationBeforeRuntime(t *testing.T) {
	for name, mutate := range map[string]func(*ContainerFactory){
		"production":              func(f *ContainerFactory) { f.Classification = "production" },
		"no allowlist":            func(f *ContainerFactory) { f.Definition.Allowlist = nil },
		"wrong daemon":            func(f *ContainerFactory) { f.Definition.Allowlist = []string{"unix:///unapproved/docker.sock"} },
		"mutable image":           func(f *ContainerFactory) { f.Definition.Image = "postgres:17" },
		"administrator reference": func(f *ContainerFactory) { f.Definition.Admin = "env://PRODUCTION" },
		"schema isolation":        func(f *ContainerFactory) { f.Definition.Isolation = "schema" },
		"unresolvable protected target": func(f *ContainerFactory) {
			f.Definition.ProtectedTargets = []string{"postgres://production.invalid/db"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			factory := testContainerFactory()
			mutate(&factory)
			calls := 0
			factory.command = func(context.Context, []string, []string) ([]byte, error) { calls++; return nil, errDockerCommand }
			if _, err := factory.Create(context.Background(), "case"); !errors.Is(err, ErrFactoryConfig) {
				t.Fatalf("unsafe configuration accepted: %v", err)
			}
			if calls != 0 {
				t.Fatal("configuration refusal invoked Docker")
			}
		})
	}
}

func TestContainerFactoryReportsMissingDaemonBeforeLease(t *testing.T) {
	factory := testContainerFactory()
	factory.LeaseDirectory = t.TempDir()
	factory.command = func(context.Context, []string, []string) ([]byte, error) { return nil, errDockerUnavailable }
	_, err := factory.Create(context.Background(), "case")
	var factoryError *FactoryError
	if !errors.Is(err, ErrFactoryConnection) || !errors.As(err, &factoryError) || factoryError.Stage != "container_daemon" {
		t.Fatalf("missing daemon lost diagnosis: %v", err)
	}
	entries, err := os.ReadDir(factory.LeaseDirectory)
	if err != nil || len(entries) != 0 {
		t.Fatal("unavailable daemon created a lease")
	}
}

func TestDockerCommandBoundsOutputAndPinsDaemon(t *testing.T) {
	output := boundedDockerOutput{limit: 8}
	if n, err := output.Write([]byte(strings.Repeat("secret", 100))); err != nil || n != 600 || output.Len() != 8 || !output.overflow {
		t.Fatal("Docker output not bounded/drained")
	}
	output = boundedDockerOutput{limit: 8}
	_, err := io.Copy(&output, struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 600))})
	if err != nil || output.Len() != 8 || !output.overflow {
		t.Fatal("stream copy bypassed the Docker output bound")
	}
	t.Setenv("DOCKER_HOST", "tcp://unapproved:2375")
	t.Setenv("DOCKER_CONTEXT", "production")
	for _, value := range dockerEnvironment(nil) {
		if strings.HasPrefix(value, "DOCKER_HOST=") || strings.HasPrefix(value, "DOCKER_CONTEXT=") {
			t.Fatal("inherited daemon selector retained")
		}
	}
	var got []string
	d := dockerCLI{endpoint: "unix:///explicit.sock", command: func(_ context.Context, args, env []string) ([]byte, error) { got = args; return nil, nil }}
	_, _ = d.run(context.Background(), nil, "info")
	if !reflect.DeepEqual(got, []string{"--host", "unix:///explicit.sock", "info"}) {
		t.Fatal("explicit daemon missing")
	}
}

func containerLeaseFixture(t *testing.T) (*leaseFile, leaseResource, dockerContainer) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	factory := testContainerFactory()
	metadata := leaseFactory{Mode: "container", Name: factory.Name, Endpoint: factory.Definition.Endpoint, DaemonID: "test-daemon-identity", Image: factory.Definition.Image, PostgresMajor: 17}
	lease, err := createLease(root, metadata, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.close)
	resource := leaseResource{Kind: "container", Name: lease.record.resourceName(), Marker: lease.record.marker(), State: "planned", ImageID: "sha256:" + strings.Repeat("b", 64)}
	next := lease.snapshot()
	next.Resources = append(next.Resources, resource)
	if err := lease.save(next); err != nil {
		t.Fatal(err)
	}
	container := dockerContainer{ID: strings.Repeat("c", 64), Name: "/" + resource.Name, ImageID: resource.ImageID, Image: metadata.Image, Labels: map[string]string{ownershipLabel: resource.Marker}, Status: "created", Network: "bridge", Tmpfs: map[string]string{containerDataPath(17): containerTmpfsOptions}}
	return lease, resource, container
}

func TestContainerOwnershipChecksExactIdentityAndIsolation(t *testing.T) {
	lease, resource, container := containerLeaseFixture(t)
	if err := verifyDockerContainer(container, lease.record, resource); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*dockerContainer){
		"foreign marker":        func(c *dockerContainer) { c.Labels = map[string]string{ownershipLabel: "foreign"} },
		"wrong name":            func(c *dockerContainer) { c.Name = "/foreign" },
		"different image":       func(c *dockerContainer) { c.ImageID = "sha256:" + strings.Repeat("d", 64) },
		"tag instead of digest": func(c *dockerContainer) { c.Image = "postgres:17" },
		"host mount":            func(c *dockerContainer) { c.Binds = []string{"/private:/data"} },
		"foreign volume": func(c *dockerContainer) {
			c.Mounts = []dockerMount{{Type: "volume", Destination: containerDataPath(17)}}
		},
		"privileged":      func(c *dockerContainer) { c.Privileged = true },
		"host networking": func(c *dockerContainer) { c.Network = "host" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := container
			mutate(&candidate)
			if err := verifyDockerContainer(candidate, lease.record, resource); !errors.Is(err, ErrOwnership) {
				t.Fatal("foreign container authorized")
			}
		})
	}
	resource.ID = strings.Repeat("d", 64)
	if err := verifyDockerContainer(container, lease.record, resource); !errors.Is(err, ErrOwnership) {
		t.Fatal("recreated same-name container accepted old lease")
	}
}

func TestContainerCleanupRecoversUnacknowledgedCreateAndDoesNotUseNameForDelete(t *testing.T) {
	lease, _, container := containerLeaseFixture(t)
	removed := false
	d := dockerCLI{endpoint: lease.record.Factory.Endpoint, command: func(_ context.Context, args, env []string) ([]byte, error) {
		args = args[2:]
		switch strings.Join(args[:2], " ") {
		case "info --format":
			return json.Marshal(lease.record.Factory.DaemonID)
		case "container ls":
			if removed {
				return nil, nil
			}
			return []byte(container.ID + "\n"), nil
		case "container inspect":
			return json.Marshal(container)
		case "container rm":
			if len(args) != 4 || args[2] != "--force" || args[3] != container.ID {
				t.Fatal("cleanup did not use exact verified container ID")
			}
			removed = true
			return []byte(container.ID), nil
		}
		t.Fatalf("unexpected Docker command: %v", args)
		return nil, nil
	}}
	if err := cleanupContainerAllocation(context.Background(), lease, d, time.Second); err != nil {
		t.Fatal(err)
	}
	if !removed || lease.record.State != "cleaned" || lease.record.Resources[0].State != "released" {
		t.Fatal("unacknowledged creation was not recovered")
	}
}

func TestDockerCachedImageAndCreateKeepStateDisposable(t *testing.T) {
	lease, resource, _ := containerLeaseFixture(t)
	calls := 0
	d := dockerCLI{endpoint: lease.record.Factory.Endpoint, command: func(_ context.Context, args, environment []string) ([]byte, error) {
		calls++
		if args[2] != "image" || args[3] != "inspect" {
			t.Fatalf("cached image requested other operation: %v", args)
		}
		return json.Marshal(dockerImage{ID: resource.ImageID, Volumes: map[string]any{containerDataPath(17): map[string]any{}}})
	}}
	if _, err := d.image(context.Background(), lease.record.Factory.Image, 17); err != nil || calls != 1 {
		t.Fatal("pinned cached image was not reused")
	}
	args := containerCreateArgs(lease.record, "127.0.0.1")
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"--volume", "--mount", "--privileged", "--rm", "POSTGRES_PASSWORD="} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unsafe or secret-bearing creation argument: %s", forbidden)
		}
	}
	if !strings.Contains(joined, "--tmpfs "+containerDataPath(17)+":"+containerTmpfsOptions) || !strings.Contains(joined, "--publish 127.0.0.1::5432") {
		t.Fatal("fresh temporary data and restricted bind missing")
	}
}
