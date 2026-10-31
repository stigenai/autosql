package dbtest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func liveContainerFactory(t *testing.T) ContainerFactory {
	t.Helper()
	endpoint, image := os.Getenv("AUTOSQL_DBTEST_DOCKER_ENDPOINT"), os.Getenv("AUTOSQL_DBTEST_POSTGRES_IMAGE")
	if endpoint == "" || image == "" {
		t.Skip("AUTOSQL_DBTEST_DOCKER_ENDPOINT and AUTOSQL_DBTEST_POSTGRES_IMAGE unset; requires approved daemon and pinned PostgreSQL image")
	}
	major, err := strconv.Atoi(os.Getenv("AUTOSQL_DBTEST_POSTGRES_MAJOR"))
	if err != nil {
		t.Fatal("AUTOSQL_DBTEST_POSTGRES_MAJOR must name the pinned image major")
	}
	f := testContainerFactory()
	f.Definition.Endpoint, f.Definition.Allowlist, f.Definition.Image, f.Definition.PostgresVersion = endpoint, []string{endpoint}, image, major
	if helperRoot := os.Getenv("AUTOSQL_CONTAINER_HELPER_ROOT"); helperRoot != "" {
		f.LeaseDirectory = helperRoot
		return f
	}
	f.LeaseDirectory, err = os.MkdirTemp("", "autosql-container-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			_ = os.RemoveAll(f.LeaseDirectory)
		} else {
			t.Logf("recovery leases retained at %s", f.LeaseDirectory)
		}
	})
	if err := os.Chmod(f.LeaseDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestContainerFactoryLiveRestrictedExecutionAndCleanup(t *testing.T) {
	factory := liveContainerFactory(t)
	target, err := factory.Create(context.Background(), "container case")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := target.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := target.Exec(context.Background(), "CREATE TABLE fixture(id integer); INSERT INTO fixture VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	count, err := target.QueryCount(context.Background(), "SELECT count(*) FROM fixture")
	if err != nil || count != 1 {
		t.Fatalf("restricted query failed: %d %v", count, err)
	}
	if err := target.Exec(context.Background(), "CREATE ROLE forbidden_container_admin SUPERUSER"); err == nil {
		t.Fatal("fixture retained administrator privilege")
	}
	path := target.LeasePath()
	if err := target.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := InspectLease(path)
	if err != nil || info.State != "cleaned" {
		t.Fatal("container cleanup not recorded")
	}
	if err := CleanupLease(context.Background(), path, nil); err != nil {
		t.Fatal(err)
	}
}

func requireContainerLeaseCleaned(t *testing.T, path string) {
	t.Helper()
	info, err := InspectLease(path)
	if err != nil || info.State != "cleaned" {
		t.Fatalf("cleanup state unavailable: %v", err)
	}
	if err := CleanupLease(context.Background(), path, nil); err != nil {
		t.Fatal(err)
	}
}

func TestContainerFactoryLivePartialSetupCancellationAndTimeout(t *testing.T) {
	for _, stage := range []string{"container_planned", "container_created", "container_recorded", "container_started", "role_planned", "role_created", "database_created", "ready"} {
		t.Run(stage, func(t *testing.T) {
			factory := liveContainerFactory(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached := false
			factory.stage = func(current string, _ leaseRecord) error {
				if current == stage {
					reached = true
					cancel()
					return ctx.Err()
				}
				return nil
			}
			target, err := factory.Create(ctx, "interrupted")
			if target != nil || !reached || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation did not reach %s: %v", stage, err)
			}
			var failure *FactoryError
			if !errors.As(err, &failure) || failure.LeasePath == "" {
				t.Fatal("interrupted setup lost lease path")
			}
			requireContainerLeaseCleaned(t, failure.LeasePath)
		})
	}
	t.Run("startup timeout", func(t *testing.T) {
		factory := liveContainerFactory(t)
		factory.Definition.StartupTimeout = "2s"
		factory.stage = func(stage string, _ leaseRecord) error {
			if stage == "container_recorded" {
				time.Sleep(2200 * time.Millisecond)
			}
			return nil
		}
		target, err := factory.Create(context.Background(), "timeout")
		if target != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("startup deadline lost: %v", err)
		}
		var failure *FactoryError
		if !errors.As(err, &failure) || failure.LeasePath == "" {
			t.Fatal("timeout lost recorded allocation")
		}
		requireContainerLeaseCleaned(t, failure.LeasePath)
	})
}

func TestContainerFactoryLiveTenConcurrentSameNamedCases(t *testing.T) {
	factory := liveContainerFactory(t)
	type allocation struct {
		target *DisposableTarget
		err    error
	}
	results := make(chan allocation, 10)
	for range 10 {
		go func() {
			target, err := factory.Create(context.Background(), "same case")
			results <- allocation{target, err}
		}()
	}
	names := map[string]bool{}
	allocations := make([]allocation, 0, 10)
	for range 10 {
		result := <-results
		allocations = append(allocations, result)
		if result.target != nil {
			t.Cleanup(func() {
				if err := result.target.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
		}
	}
	for _, result := range allocations {
		if result.err != nil {
			t.Error(result.err)
			continue
		}
		target := result.target
		record := target.lease.snapshot()
		if names[record.resourceName()] {
			t.Error("concurrent case reused identity")
		}
		names[record.resourceName()] = true
		if err := target.Exec(context.Background(), "CREATE SCHEMA fixed_schema; CREATE TABLE fixed_schema.fixture(id int); INSERT INTO fixed_schema.fixture VALUES(1)"); err != nil {
			t.Error(err)
		}
		if count, err := target.QueryCount(context.Background(), "SELECT count(*) FROM fixed_schema.fixture"); err != nil || count != 1 {
			t.Errorf("concurrent fixture interference: %d %v", count, err)
		}
		if err := target.Close(context.Background()); err != nil {
			t.Error(err)
		}
		requireContainerLeaseCleaned(t, target.LeasePath())
	}
	if len(names) != 10 {
		t.Fatalf("created %d of 10 concurrent targets", len(names))
	}
}

func TestContainerFactoryLiveReplacedContainerIsPreserved(t *testing.T) {
	factory := liveContainerFactory(t)
	target, err := factory.Create(context.Background(), "replaced container")
	if err != nil {
		t.Fatal(err)
	}
	record, path := target.lease.snapshot(), target.LeasePath()
	resource, _ := leasedContainer(record)
	if err := target.Detach(); err != nil {
		t.Fatal(err)
	}
	d := dockerCLI{endpoint: record.Factory.Endpoint}
	ctx := context.Background()
	if _, err := d.run(ctx, nil, "container", "rm", "--force", resource.ID); err != nil {
		t.Fatal(err)
	}
	password, err := targetPassword()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := d.run(ctx, []string{"POSTGRES_PASSWORD=" + password}, containerCreateArgs(record, "127.0.0.1")...)
	if err != nil {
		t.Fatal(err)
	}
	replacement := strings.TrimSpace(string(raw))
	if !leaseContainerID.MatchString(replacement) || replacement == resource.ID {
		t.Fatal("replacement identity missing")
	}
	if err := CleanupLease(ctx, path, nil); !errors.Is(err, ErrOwnership) {
		t.Fatalf("replacement accepted old lease: %v", err)
	}
	ids, err := d.containerIDs(ctx, "id="+replacement)
	if err != nil || len(ids) != 1 || ids[0] != replacement {
		t.Fatal("cleanup deleted a same-name replacement")
	}
	if _, err := d.run(ctx, nil, "container", "rm", "--force", replacement); err != nil {
		t.Fatal(err)
	}
	if err := CleanupLease(ctx, path, nil); err != nil {
		t.Fatal(err)
	}
	requireContainerLeaseCleaned(t, path)
}

func TestContainerFactoryLiveChangedDaemonIdentityRefusesCleanup(t *testing.T) {
	factory := liveContainerFactory(t)
	target, err := factory.Create(context.Background(), "changed daemon")
	if err != nil {
		t.Fatal(err)
	}
	record, path := target.lease.snapshot(), target.LeasePath()
	if err := target.Detach(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := record
	changed.Factory.DaemonID = "different-daemon-identity"
	raw, err := encodeLease(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupLease(context.Background(), path, nil); !errors.Is(err, ErrOwnership) {
		t.Fatal("changed daemon identity accepted")
	}
	resource, _ := leasedContainer(record)
	_, exists, err := (dockerCLI{endpoint: record.Factory.Endpoint}).ownedContainer(context.Background(), record, resource)
	if err != nil || !exists {
		t.Fatal("changed daemon identity removed owned container")
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupLease(context.Background(), path, nil); err != nil {
		t.Fatal(err)
	}
}

func TestContainerFactoryProcessHelper(t *testing.T) {
	if os.Getenv("AUTOSQL_CONTAINER_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	factory := liveContainerFactory(t)
	factory.LeaseDirectory = os.Getenv("AUTOSQL_CONTAINER_HELPER_ROOT")
	factory.stage = func(stage string, record leaseRecord) error {
		if stage == "database_recorded" {
			fmt.Println("LEASE=" + filepath.Join(factory.LeaseDirectory, record.RunID+"-"+record.CaseID, leaseFilename))
			timer := time.NewTimer(time.Hour)
			defer timer.Stop()
			<-timer.C
		}
		return nil
	}
	if _, err := factory.Create(context.Background(), "killed process"); err != nil {
		t.Fatal(err)
	}
}

func TestContainerFactoryLiveKilledProcessRecovers(t *testing.T) {
	factory := liveContainerFactory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestContainerFactoryProcessHelper$", "-test.v")
	command.Env = append(os.Environ(), "AUTOSQL_CONTAINER_HELPER=1", "AUTOSQL_CONTAINER_HELPER_ROOT="+factory.LeaseDirectory)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			if path, ok := strings.CutPrefix(scanner.Text(), "LEASE="); ok {
				found <- path
				return
			}
		}
		found <- ""
	}()
	var path string
	select {
	case path = <-found:
	case <-ctx.Done():
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	if path == "" {
		t.Fatal("child did not persist an acquired container/database lease")
	}
	if err := CleanupLease(context.Background(), path, nil); err != nil {
		t.Fatal(err)
	}
	requireContainerLeaseCleaned(t, path)
}

func TestContainerFactoryLiveTCPDaemonAlias(t *testing.T) {
	factory := liveContainerFactory(t)
	endpoint, err := url.Parse(factory.Definition.Endpoint)
	if err != nil || endpoint.Scheme != "unix" {
		t.Skip("TCP alias qualification proxies an explicitly approved Unix daemon")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go proxyDockerConnection(client, endpoint.Path)
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	factory.Definition.Endpoint = "tcp://127.0.0.1:" + port
	factory.Definition.Allowlist = []string{"tcp://localhost:" + port}
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_TLS", "")
	target, err := factory.Create(context.Background(), "TCP daemon alias")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := target.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := target.Exec(context.Background(), "CREATE TABLE tcp_fixture(id int)"); err != nil {
		t.Fatal(err)
	}
	path := target.LeasePath()
	if err := target.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireContainerLeaseCleaned(t, path)
}

func proxyDockerConnection(client net.Conn, socket string) {
	defer client.Close()
	upstream, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close(); _ = client.Close() }()
	_, _ = io.Copy(client, upstream)
}

func TestContainerFactoryLiveMajorMismatchRefusesFixtures(t *testing.T) {
	factory := liveContainerFactory(t)
	if factory.Definition.PostgresVersion == 14 {
		factory.Definition.PostgresVersion = 15
	} else {
		factory.Definition.PostgresVersion = 14
	}
	factory.stage = func(stage string, _ leaseRecord) error {
		if stage == "role_planned" {
			t.Error("major mismatch reached restricted target provisioning")
		}
		return nil
	}
	target, err := factory.Create(context.Background(), "wrong image major")
	if target != nil || !errors.Is(err, ErrFactoryConfig) {
		t.Fatalf("wrong image major accepted: %v", err)
	}
	var failure *FactoryError
	if !errors.As(err, &failure) || failure.LeasePath == "" {
		t.Fatal("major mismatch lost lease record")
	}
	requireContainerLeaseCleaned(t, failure.LeasePath)
}
