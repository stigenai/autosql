package operatorcontroller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"autosql/pkg/executor"
	"autosql/pkg/operator"
	"github.com/go-logr/logr"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// This helper runs in separate OS processes so each replica has its own metric
// registry, retry state and controller-runtime manager, just like deployed pods.
func TestOperatorDiagnosticsManagerProcess(t *testing.T) {
	if os.Getenv("AUTOSQL_DIAGNOSTICS_PROCESS") != "1" {
		t.Skip("two-process envtest helper")
	}
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil)))
	processMetrics()
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("AUTOSQL_DIAGNOSTICS_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	lease, renew, retry := 4*time.Second, 3*time.Second, 500*time.Millisecond
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: NewScheme(), LeaderElection: true, LeaderElectionID: "autosql-diagnostics-test", LeaderElectionNamespace: "default", LeaderElectionReleaseOnCancel: true, LeaseDuration: &lease, RenewDeadline: &renew, RetryPeriod: &retry, Metrics: metricsserver.Options{BindAddress: os.Getenv("AUTOSQL_DIAGNOSTICS_METRICS")}, HealthProbeBindAddress: os.Getenv("AUTOSQL_DIAGNOSTICS_HEALTH")})
	if err != nil {
		t.Fatal(err)
	}
	if err := setupHealth(mgr); err != nil {
		t.Fatal(err)
	}
	c := &Controller{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Store: operator.NewMemoryStore(), Recorder: mgr.GetEventRecorderFor("autosql-diagnostics-test"),
		Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
			if err := os.WriteFile(os.Getenv("AUTOSQL_DIAGNOSTICS_APPLY_MARKER"), []byte("apply invoked"), 0600); err != nil {
				t.Errorf("record unexpected apply: %v", err)
			}
			return operator.ApplyResult{}, errors.New("unexpected SQL apply: seeded-secret")
		},
		Probe: func(context.Context, operator.Resource) executor.ExecutionStartCheck {
			return executor.ExecutionStartCheck{Started: true}
		},
		Observe: func(context.Context, operator.Resource) operator.CompletionResult {
			return operator.CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionRefused, Reason: "history_invalid"}}
		},
	}
	if err := c.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		t.Fatal(err)
	}
}

type diagnosticsProcess struct {
	command              *exec.Cmd
	done                 chan error
	metrics, health, log string
	stopped              bool
}

func diagnosticsAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func startDiagnosticsProcess(t *testing.T, config, directory string, index int) *diagnosticsProcess {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &diagnosticsProcess{metrics: diagnosticsAddress(t), health: diagnosticsAddress(t), log: filepath.Join(directory, fmt.Sprintf("manager-%d.log", index)), done: make(chan error, 1)}
	output, err := os.Create(p.log)
	if err != nil {
		t.Fatal(err)
	}
	p.command = exec.Command(binary, "-test.run=^TestOperatorDiagnosticsManagerProcess$", "-test.v")
	p.command.Env = append(os.Environ(), "AUTOSQL_DIAGNOSTICS_PROCESS=1", "AUTOSQL_DIAGNOSTICS_CONFIG="+config, "AUTOSQL_DIAGNOSTICS_METRICS="+p.metrics, "AUTOSQL_DIAGNOSTICS_HEALTH="+p.health, "AUTOSQL_DIAGNOSTICS_APPLY_MARKER="+p.log+".apply")
	p.command.Stdout, p.command.Stderr = output, output
	if err := p.command.Start(); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	go func() { err := p.command.Wait(); _ = output.Close(); p.done <- err }()
	t.Cleanup(func() {
		p.stop(t)
		if t.Failed() {
			raw, _ := os.ReadFile(p.log)
			t.Log(string(raw))
		}
	})
	return p
}

func (p *diagnosticsProcess) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	_ = p.command.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-p.done:
		if err != nil {
			t.Errorf("manager process failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		_ = p.command.Process.Kill()
		<-p.done
		t.Error("manager process did not stop")
	}
}

func diagnosticsHTTP(address, path string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + address + path)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("endpoint status %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 4<<20))
}

func diagnosticsCounter(address, metric, label, value string) (float64, error) {
	raw, err := diagnosticsHTTP(address, "/metrics")
	if err != nil {
		return 0, err
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(raw)))
	if err != nil {
		return 0, err
	}
	family := families[metric]
	if family == nil {
		return 0, errors.New("metric absent")
	}
	var sum float64
	for _, sample := range family.Metric {
		for _, pair := range sample.Label {
			if pair.GetName() == label && pair.GetValue() == value {
				sum += sample.GetCounter().GetValue()
			}
		}
	}
	return sum, nil
}

func waitDiagnostics(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(description)
}

func TestOperatorDiagnosticsTwoReplicas(t *testing.T) {
	if os.Getenv("AUTOSQL_OPERATOR_ENVTEST") != "1" {
		t.Skip("set AUTOSQL_OPERATOR_ENVTEST=1 for two-manager qualification")
	}
	crd, err := os.ReadFile(filepath.Join("..", "..", "deploy", "operator", "crd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	crdDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(crdDirectory, "schema.yaml"), crd, 0600); err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{crdDirectory}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	directory := t.TempDir()
	configPath := filepath.Join(directory, "kubeconfig")
	kubeconfig := clientcmdapi.Config{Clusters: map[string]*clientcmdapi.Cluster{"test": {Server: config.Host, CertificateAuthorityData: config.CAData}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"test": {ClientCertificateData: config.CertData, ClientKeyData: config.KeyData}}, Contexts: map[string]*clientcmdapi.Context{"test": {Cluster: "test", AuthInfo: "test"}}, CurrentContext: "test"}
	if err := clientcmd.WriteToFile(kubeconfig, configPath); err != nil {
		t.Fatal(err)
	}
	first := startDiagnosticsProcess(t, configPath, directory, 1)
	second := startDiagnosticsProcess(t, configPath, directory, 2)
	for _, process := range []*diagnosticsProcess{first, second} {
		waitDiagnostics(t, "replica did not expose healthy synchronized endpoints", func() bool {
			_, liveness := diagnosticsHTTP(process.health, "/healthz")
			_, readiness := diagnosticsHTTP(process.health, "/readyz")
			_, metrics := diagnosticsCounter(process.metrics, "autosql_operator_completion_observations_total", "outcome", "refused")
			return liveness == nil && readiness == nil && metrics == nil
		})
	}
	cl, err := client.New(config, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatal(err)
	}
	obj, secret := completionObject()
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetGeneration(0)
	obj.SetNamespace("default")
	secret.SetNamespace("default")
	if err := cl.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	var firstCount, secondCount float64
	waitDiagnostics(t, "leader did not emit refusal", func() bool {
		firstCount, _ = diagnosticsCounter(first.metrics, "autosql_operator_completion_observations_total", "outcome", "refused")
		secondCount, _ = diagnosticsCounter(second.metrics, "autosql_operator_completion_observations_total", "outcome", "refused")
		return firstCount+secondCount == 1
	})
	leader, standby := first, second
	if secondCount == 1 {
		leader, standby = second, first
	}
	// Scraping both endpoints repeatedly neither enqueues work nor executes SQL.
	for i := 0; i < 3; i++ {
		for _, process := range []*diagnosticsProcess{leader, standby} {
			if _, err := diagnosticsHTTP(process.health, "/readyz"); err != nil {
				t.Fatal("blocked resource made manager unready")
			}
			if _, err := diagnosticsHTTP(process.health, "/healthz"); err != nil {
				t.Fatal("blocked resource made manager unhealthy")
			}
			if _, err := diagnosticsHTTP(process.metrics, "/metrics"); err != nil {
				t.Fatal(err)
			}
		}
	}
	leaderCount, _ := diagnosticsCounter(leader.metrics, "autosql_operator_completion_observations_total", "outcome", "refused")
	standbyCount, _ := diagnosticsCounter(standby.metrics, "autosql_operator_completion_observations_total", "outcome", "refused")
	if leaderCount != 1 || standbyCount != 0 {
		t.Fatalf("duplicate work across replicas: leader=%g standby=%g", leaderCount, standbyCount)
	}
	leader.stop(t)
	waitDiagnostics(t, "standby did not take leadership and observe", func() bool {
		count, _ := diagnosticsCounter(standby.metrics, "autosql_operator_completion_observations_total", "outcome", "refused")
		return count == 1
	})
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(GroupVersionKind)
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(obj), current); err != nil {
		t.Fatal(err)
	}
	if status := statusFromObject(current); recoveredReady(status) || status.RetryCount < 2 {
		t.Fatalf("handover lost failed state: %+v", status)
	}
	if _, err := diagnosticsHTTP(standby.health, "/readyz"); err != nil {
		t.Fatal(err)
	}
	standby.stop(t)
	for _, process := range []*diagnosticsProcess{first, second} {
		if _, err := os.Stat(process.log + ".apply"); !os.IsNotExist(err) {
			t.Fatal("observation called Apply")
		}
		raw, err := os.ReadFile(process.log)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "seeded-secret") || strings.Contains(string(raw), "unexpected SQL apply") || strings.Contains(string(raw), "postgres://") || strings.Contains(string(raw), "log.SetLogger(...) was never called") {
			t.Fatal("process diagnostics leaked data or called apply")
		}
	}
}
