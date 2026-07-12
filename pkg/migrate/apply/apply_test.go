package apply

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/plan"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
)

type fakeStore struct {
	status   revision.Status
	inserted []revision.Revision
	events   []revision.Event
}

func (f *fakeStore) Status(context.Context, migrate.Manifest) (revision.Status, error) {
	return f.status, nil
}
func (f *fakeStore) Insert(_ context.Context, r revision.Revision) error {
	f.inserted = append(f.inserted, r)
	return nil
}
func (f *fakeStore) InsertBatch(_ context.Context, r []revision.Revision, e []revision.Event) error {
	f.inserted = append(f.inserted, r...)
	f.events = append(f.events, e...)
	return nil
}
func (f *fakeStore) UpdateState(_ context.Context, v string, a int, state string, o int, redacted string, d time.Duration, event string, operator string) error {
	for i := range f.inserted {
		if f.inserted[i].Version == v {
			f.inserted[i].State = state
			f.inserted[i].StatementOrdinal = o
			f.inserted[i].Duration = d
		}
	}
	return nil
}

func fixture(t *testing.T, n int) (migrate.Snapshot, revision.Status) {
	t.Helper()
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	name := schema.Name{Name: "app"}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, name), Kind: schema.KindSchema, Name: name, Spec: []byte(`{}`)}}}}
	p, e := plan.Build(context.Background(), sample.Driver{}, empty, desired, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	cd, _ := guardrail.ChangeDigest(p.Changes)
	checks := precheck.Plan{ID: "checks", ChangeDigest: cd}
	checks.Digest, _ = precheck.Digest(checks)
	now := time.Now().UTC()
	a, e := artifact.New(p, checks, now, now.Add(time.Hour), "rev", "test", "db", "sha256:"+strings.Repeat("b", 64), artifact.Approval{Identity: "release", ApprovedAt: now}, map[string]string{})
	if e != nil {
		t.Fatal(e)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if e = a.Sign("key", priv); e != nil {
		t.Fatal(e)
	}
	raw, _ := a.MarshalCanonical()
	snap := migrate.Snapshot{Manifest: migrate.Manifest{Version: migrate.ManifestVersion, Generation: "gen", Digest: "sha256:" + strings.Repeat("c", 64)}, Files: map[string][]byte{}}
	st := revision.Status{ManifestDigest: snap.Manifest.Digest, Counts: map[string]int{}}
	for i := 1; i <= n; i++ {
		v := migrate.Version{Major: uint64(i)}.String()
		file := "V" + v + "__x.sql"
		af := file + ".artifact.json"
		snap.Manifest.Entries = append(snap.Manifest.Entries, migrate.Migration{Version: v, File: file, Name: "x", SQLDigest: "sha256:" + strings.Repeat("a", 64), ArtifactFile: af, ArtifactDigest: a.Digest, Directives: migrate.Directives{Transaction: migrate.TransactionRequired, PlanDigest: p.Digest, CheckDigest: checks.Digest, BundleDigest: a.GuardrailDigest}, Statements: []migrate.Statement{{Ordinal: 1}}})
		snap.Files[af] = raw
		st.Entries = append(st.Entries, revision.StatusEntry{Version: v, File: file, Classification: "pending"})
		st.Counts["pending"]++
	}
	return snap, st
}

func TestBoundedApplyNeverRerunsAndBaselineIsSQLFree(t *testing.T) {
	snap, status := fixture(t, 4)
	status.Entries[0].Classification = "applied"
	status.Counts["pending"]--
	status.Counts["applied"]++
	store := &fakeStore{status: status}
	calls := 0
	count := 2
	engine := Engine{Store: store, Apply: func(context.Context, migrate.Migration, []byte) (ArtifactResult, error) {
		calls++
		return ArtifactResult{Statements: 1, Duration: time.Millisecond}, nil
	}}
	out, e := engine.Run(context.Background(), Request{Snapshot: snap, Count: &count, Operator: "op", Transaction: "file"})
	if e != nil || calls != 2 || len(store.inserted) != 2 || out.FinalVersion != "3.0.0" {
		t.Fatalf("out=%+v calls=%d rows=%d err=%v", out, calls, len(store.inserted), e)
	}
	baseStore := &fakeStore{status: revision.Status{Entries: status.Entries[1:], Counts: map[string]int{"pending": 3}}}
	baseEngine := Engine{Store: baseStore, Apply: func(context.Context, migrate.Migration, []byte) (ArtifactResult, error) {
		t.Fatal("baseline executed SQL")
		return ArtifactResult{}, nil
	}}
	to := "3.0.0"
	base, e := baseEngine.Run(context.Background(), Request{Snapshot: migrate.Snapshot{Manifest: migrate.Manifest{Version: snap.Manifest.Version, Generation: snap.Manifest.Generation, Digest: snap.Manifest.Digest, Entries: snap.Manifest.Entries[1:]}, Files: snap.Files}, To: to, Baseline: true, Operator: "op", Transaction: "file"})
	if e != nil || base.Status != "baselined" || len(baseStore.inserted) != 2 || len(baseStore.events) != 2 {
		t.Fatalf("baseline=%+v rows=%d events=%d err=%v", base, len(baseStore.inserted), len(baseStore.events), e)
	}
}

func TestDryRunAndPreflightFailuresAreZeroMutation(t *testing.T) {
	snap, status := fixture(t, 2)
	store := &fakeStore{status: status}
	calls := 0
	engine := Engine{Store: store, Apply: func(context.Context, migrate.Migration, []byte) (ArtifactResult, error) {
		calls++
		return ArtifactResult{}, nil
	}}
	zero := 0
	out, e := engine.Run(context.Background(), Request{Snapshot: snap, Count: &zero, DryRun: true, Operator: "op", Transaction: "file"})
	if e != nil || out.Status != "dry_run" || calls != 0 || len(store.inserted) != 0 {
		t.Fatalf("out=%+v err=%v", out, e)
	}
	store.status.Dirty = true
	if _, e = engine.Run(context.Background(), Request{Snapshot: snap, Operator: "op", Transaction: "file"}); e == nil || calls != 0 {
		t.Fatal("dirty state reached mutation")
	}
	store.status.Dirty = false
	store.status.Entries[1].Classification = "applied"
	if _, e = engine.Run(context.Background(), Request{Snapshot: snap, Operator: "op", Transaction: "file"}); e == nil || calls != 0 {
		t.Fatal("gap reached mutation")
	}
}

func TestUnknownArtifactDriftTransactionAndFailurePositionRefuseSafely(t *testing.T) {
	snap, status := fixture(t, 2)
	store := &fakeStore{status: status}
	calls := 0
	engine := Engine{Store: store, Apply: func(context.Context, migrate.Migration, []byte) (ArtifactResult, error) {
		calls++
		return ArtifactResult{}, errors.New("guarded failure")
	}}
	store.status.Entries = append(store.status.Entries, revision.StatusEntry{Version: "9.0.0", Classification: "unknown", Unknown: true})
	if _, e := engine.Run(context.Background(), Request{Snapshot: snap, Operator: "op", Transaction: "file"}); e == nil || calls != 0 {
		t.Fatal("unknown revision reached mutation")
	}
	store.status = status
	tampered := snap
	tampered.Files = map[string][]byte{}
	for k, v := range snap.Files {
		tampered.Files[k] = append([]byte(nil), v...)
	}
	tampered.Files[snap.Manifest.Entries[0].ArtifactFile][10] ^= 1
	if _, e := engine.Run(context.Background(), Request{Snapshot: tampered, Operator: "op", Transaction: "file"}); e == nil || calls != 0 {
		t.Fatal("artifact drift reached mutation")
	}
	snap.Manifest.Entries[0].Directives.Transaction = migrate.TransactionAuto
	if _, e := engine.Run(context.Background(), Request{Snapshot: snap, Operator: "op", Transaction: "all"}); e == nil || calls != 0 {
		t.Fatal("unsafe all-in-one reached mutation")
	}
	snap.Manifest.Entries[0].Directives.Transaction = migrate.TransactionRequired
	out, e := engine.Run(context.Background(), Request{Snapshot: snap, Operator: "op", Transaction: "file"})
	if e == nil || calls != 1 || out.Failure == nil || out.Failure.Position != 1 || out.Failure.Version != "1.0.0" {
		t.Fatalf("out=%+v calls=%d err=%v", out, calls, e)
	}
}
