package simulate

import (
	"autosql/pkg/plan"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/schema"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeFactory struct {
	iso *fakeIsolation
	err error
}

func (f fakeFactory) Create(context.Context, Config) (Isolation, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.iso.event("create")
	return f.iso, nil
}

type fakeIsolation struct {
	mu                  sync.Mutex
	events              []string
	actual              schema.Document
	fail                string
	cleanupContextAlive bool
}

func (f *fakeIsolation) event(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, s)
}
func (f *fakeIsolation) Identity() string { return "dev/isolation" }
func (f *fakeIsolation) Materialize(context.Context, schema.Document) error {
	f.event("materialize")
	if f.fail == "materialize" {
		return errors.New("secret materialize")
	}
	return nil
}
func (f *fakeIsolation) Execute(ctx context.Context, _ plan.Plan) error {
	f.event("execute")
	if f.fail == "cancel" {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.fail == "execute" {
		return errors.New("secret execute")
	}
	return nil
}
func (f *fakeIsolation) Inspect(context.Context) (schema.Document, error) {
	f.event("inspect")
	if f.fail == "inspect" {
		return schema.Document{}, errors.New("secret inspect")
	}
	return f.actual, nil
}
func (f *fakeIsolation) Cleanup(ctx context.Context) error {
	f.event("cleanup")
	f.cleanupContextAlive = ctx.Err() == nil
	if f.fail == "cleanup" {
		return errors.New("secret cleanup")
	}
	return nil
}
func plans(t *testing.T) (schema.Document, schema.Document, plan.Plan) {
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	n := schema.Name{Name: "app"}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n, Spec: []byte(`{}`)}}}}
	p, e := plan.Build(context.Background(), sample.Driver{}, empty, desired, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	return empty, desired, p
}
func TestLifecycleAlwaysCleansUp(t *testing.T) {
	from, to, p := plans(t)
	for _, failure := range []string{"", "materialize", "execute", "inspect", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			iso := &fakeIsolation{actual: to, fail: failure}
			result, e := Run(context.Background(), fakeFactory{iso: iso}, Request{Config: Config{DevelopmentURL: "postgres://dev", ProductionIdentity: "prod/db", CleanupTimeout: time.Second}, From: from, Plan: p})
			if failure == "" && (e != nil || !result.Verified) {
				t.Fatalf("result=%+v err=%v", result, e)
			}
			if failure != "" && e == nil {
				t.Fatal("failure accepted")
			}
			if iso.events[len(iso.events)-1] != "cleanup" || !iso.cleanupContextAlive {
				t.Fatalf("events=%v alive=%v", iso.events, iso.cleanupContextAlive)
			}
		})
	}
}
func TestCancelStillCleansWithFreshContext(t *testing.T) {
	from, to, p := plans(t)
	iso := &fakeIsolation{actual: to, fail: "cancel"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, e := Run(ctx, fakeFactory{iso: iso}, Request{Config: Config{DevelopmentURL: "postgres://dev", ProductionIdentity: "prod/db", CleanupTimeout: time.Second}, From: from, Plan: p})
	if e == nil || !iso.cleanupContextAlive || iso.events[len(iso.events)-1] != "cleanup" {
		t.Fatalf("err=%v events=%v", e, iso.events)
	}
}
func TestFingerprintMismatchAndRedaction(t *testing.T) {
	from, _, p := plans(t)
	iso := &fakeIsolation{actual: from}
	_, e := Run(context.Background(), fakeFactory{iso: iso}, Request{Config: Config{DevelopmentURL: "postgres://user:seeded-secret@dev", ProductionIdentity: "prod/db"}, From: from, Plan: p})
	if !errors.Is(e, ErrFingerprint) || contains(e.Error(), "seeded-secret") {
		t.Fatalf("error=%v", e)
	}
}
func contains(s, x string) bool {
	for i := 0; i+len(x) <= len(s); i++ {
		if s[i:i+len(x)] == x {
			return true
		}
	}
	return false
}
