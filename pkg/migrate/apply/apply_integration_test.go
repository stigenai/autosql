package apply

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"github.com/jackc/pgx/v5"
)

func liveStore(t *testing.T, url string) (*revision.Store, func()) {
	t.Helper()
	b := make([]byte, 6)
	if _, e := rand.Read(b); e != nil {
		t.Fatal(e)
	}
	schema := "autosql_apply_" + hex.EncodeToString(b)
	s, e := revision.Open(revision.Config{URL: url, Schema: schema})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Init(context.Background()); e != nil {
		t.Fatal(e)
	}
	return s, func() {
		c, e := pgx.Connect(context.Background(), url)
		if e == nil {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+pgx.Identifier{schema}.Sanitize()+" cascade")
			_ = c.Close(context.Background())
		}
	}
}

func TestLiveFirstApplyRetryBoundsAndBaselineThenApply(t *testing.T) {
	url := os.Getenv("AUTOSQL_POSTGRES_TEST_DSN")
	if url == "" {
		t.Skip("AUTOSQL_POSTGRES_TEST_DSN unset")
	}
	ctx := context.Background()
	snap, _ := fixture(t, 3)
	store, cleanup := liveStore(t, url)
	defer cleanup()
	calls := 0
	engine := Engine{Store: store, Apply: func(context.Context, migrate.Migration, []byte) (ArtifactResult, error) {
		calls++
		return ArtifactResult{Statements: 1}, nil
	}}
	out, e := engine.Run(ctx, Request{Snapshot: snap, Operator: "op", Transaction: "file"})
	if e != nil || calls != 3 || out.FinalVersion != "3.0.0" {
		t.Fatalf("first=%+v calls=%d err=%v", out, calls, e)
	}
	out, e = engine.Run(ctx, Request{Snapshot: snap, Operator: "op", Transaction: "file"})
	if e != nil || out.Status != "no_op" || calls != 3 {
		t.Fatalf("retry=%+v calls=%d err=%v", out, calls, e)
	}
	bounded, bclean := liveStore(t, url)
	defer bclean()
	one := 1
	bengine := Engine{Store: bounded, Apply: engine.Apply}
	out, e = bengine.Run(ctx, Request{Snapshot: snap, Count: &one, Operator: "op", Transaction: "file"})
	if e != nil || len(out.Files) != 1 || out.FinalVersion != "1.0.0" {
		t.Fatalf("bounded=%+v err=%v", out, e)
	}
	base, baclean := liveStore(t, url)
	defer baclean()
	two := 2
	baseEngine := Engine{Store: base, Apply: func(context.Context, migrate.Migration, []byte) (ArtifactResult, error) {
		t.Fatal("baseline executed")
		return ArtifactResult{}, nil
	}}
	out, e = baseEngine.Run(ctx, Request{Snapshot: snap, Count: &two, Baseline: true, Operator: "op", Transaction: "file"})
	if e != nil || out.Status != "baselined" || out.FinalVersion != "2.0.0" {
		t.Fatalf("baseline=%+v err=%v", out, e)
	}
	later := Engine{Store: base, Apply: engine.Apply}
	out, e = later.Run(ctx, Request{Snapshot: snap, Operator: "op", Transaction: "file"})
	if e != nil || len(out.Files) != 1 || out.FinalVersion != "3.0.0" {
		t.Fatalf("later=%+v err=%v", out, e)
	}
}
