package migrate

import (
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

func checkpointLiveURLs(t *testing.T) (string, string) {
	t.Helper()
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("generation PostgreSQL URLs not configured")
	}
	return dev, prod
}

func longSchemaHistory() []File {
	out := make([]File, 0, 101)
	out = append(out, File{Name: "V1__schema.sql", SQL: []byte("CREATE SCHEMA checkpoint_live;")})
	for i := 2; i <= 101; i++ {
		out = append(out, File{Name: fmt.Sprintf("V%d__table.sql", i), SQL: []byte(fmt.Sprintf("CREATE TABLE checkpoint_live.t%03d(id bigint);", i))})
	}
	return out
}

func liveFingerprint(t *testing.T, adminURL string, sqls ...[]byte) string {
	t.Helper()
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, adminURL)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	name := "autosql_checkpoint_accept_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > 60 {
		name = name[:60]
	}
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}()
	u, _ := url.Parse(adminURL)
	u.Path = "/" + name
	c, e := pgx.Connect(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	for _, q := range sqls {
		if _, e = c.Exec(ctx, string(q)); e != nil {
			c.Close(ctx)
			t.Fatal(e)
		}
	}
	c.Close(ctx)
	doc, e := postgres.InspectURL(ctx, u.String(), postgres.Options{Schemas: []string{"checkpoint_live"}})
	if e != nil {
		t.Fatal(e)
	}
	doc, e = postgres.New().Normalize(ctx, doc)
	if e != nil {
		t.Fatal(e)
	}
	fp, e := schema.SemanticFingerprint(doc)
	if e != nil {
		t.Fatal(e)
	}
	return fp
}

func TestCheckpointLiveLongHistoryEquivalenceCASPolicyAndFaults(t *testing.T) {
	dev, prod := checkpointLiveURLs(t)
	d := t.TempDir()
	_ = os.Chmod(d, 0700)
	files := longSchemaHistory()
	if _, e := Update(d, UpdateRequest{Files: files}); e != nil {
		t.Fatal(e)
	}
	r := generationFixture(t, d, dev, prod)
	r.Version = "102"
	r.Label = "checkpoint"
	r.Desired = schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	result, e := (GenerateService{}).CreateCheckpoint(context.Background(), CheckpointRequest{GenerateRequest: r, DataPolicy: "schema_only"})
	if e != nil {
		t.Fatal(e)
	}
	snap, e := LoadSnapshot(d)
	if e != nil {
		t.Fatal(e)
	}
	cp := snap.Files[result.File]
	full := make([][]byte, 0, len(files)+1)
	for _, f := range files {
		full = append(full, f.SQL)
	}
	suffix := []byte("ALTER TABLE checkpoint_live.t101 ADD COLUMN name text;")
	full = append(full, suffix)
	fresh := liveFingerprint(t, dev, cp, suffix)
	replay := liveFingerprint(t, dev, full...)
	existing := liveFingerprint(t, dev, full...)
	if fresh != replay || existing != replay || result.SchemaFingerprint == "" {
		t.Fatalf("fingerprints fresh=%s replay=%s existing=%s checkpoint=%s", fresh, replay, existing, result.SchemaFingerprint)
	}
	if _, e = VerifyCheckpoints(d); e != nil {
		t.Fatal(e)
	}

	dataDir := t.TempDir()
	_ = os.Chmod(dataDir, 0700)
	dataFiles := append(longSchemaHistory(), File{Name: "V102__seed.sql", SQL: []byte("INSERT INTO checkpoint_live.t101(id) VALUES (1);")})
	if _, e = Update(dataDir, UpdateRequest{Files: dataFiles}); e != nil {
		t.Fatal(e)
	}
	dr := generationFixture(t, dataDir, dev, prod)
	dr.Version = "103"
	dr.Label = "checkpoint"
	dr.Desired = r.Desired
	if _, e = (GenerateService{}).CreateCheckpoint(context.Background(), CheckpointRequest{GenerateRequest: dr, DataPolicy: "schema_only"}); !errors.Is(e, ErrCheckpointPolicy) {
		t.Fatalf("seed omission accepted: %v", e)
	}
	if _, e = (GenerateService{}).CreateCheckpoint(context.Background(), CheckpointRequest{GenerateRequest: dr, DataPolicy: "declared_replay", DeclaredReplay: []string{"102.0.0"}, PolicyApproved: true}); e != nil {
		t.Fatal(e)
	}

	for _, stage := range []string{"replay", "render", "simulate", "sign", "publish"} {
		t.Run("fault_"+stage, func(t *testing.T) {
			fd := t.TempDir()
			_ = os.Chmod(fd, 0700)
			_, _ = Update(fd, UpdateRequest{Files: longSchemaHistory()})
			before := treeState(t, fd)
			fr := generationFixture(t, fd, dev, prod)
			fr.Version = "102"
			fr.Label = "checkpoint"
			fr.Desired = r.Desired
			fr.Stage = func(g string) error {
				if g == stage {
					return os.ErrPermission
				}
				return nil
			}
			if _, e := (GenerateService{}).CreateCheckpoint(context.Background(), CheckpointRequest{GenerateRequest: fr, DataPolicy: "schema_only"}); e == nil {
				t.Fatal("fault accepted")
			}
			if treeState(t, fd) != before {
				t.Fatal("fault published output")
			}
		})
	}

	cas := t.TempDir()
	_ = os.Chmod(cas, 0700)
	_, _ = Update(cas, UpdateRequest{Files: longSchemaHistory()})
	cr := generationFixture(t, cas, dev, prod)
	cr.Version = "102"
	cr.Label = "checkpoint"
	cr.Desired = r.Desired
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, x := (GenerateService{}).CreateCheckpoint(context.Background(), CheckpointRequest{GenerateRequest: cr, DataPolicy: "schema_only"})
			errs <- x
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for x := range errs {
		if x == nil {
			wins++
		} else if errors.Is(x, ErrGenerateConflict) {
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("checkpoint CAS wins=%d conflicts=%d", wins, conflicts)
	}

	// Any byte-level artifact tamper (including covered range, head chain, or
	// fingerprint metadata drift) invalidates the immutable generation before
	// checkpoint verification can trust it.
	artifactPath := filepath.Join(d, genDir, snap.Manifest.Generation, result.ArtifactFile)
	raw := append([]byte(nil), snap.Files[result.ArtifactFile]...)
	for _, needle := range []string{"autosql.checkpoint.covered_to", "autosql.checkpoint.head_chain", "autosql.checkpoint.schema_fingerprint"} {
		t.Run("tamper_"+needle, func(t *testing.T) {
			mutated := bytes.Replace(raw, []byte(needle), []byte(strings.Repeat("x", len(needle))), 1)
			if e := os.WriteFile(artifactPath, mutated, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e := VerifyCheckpoints(d); e == nil {
				t.Fatal("checkpoint metadata tamper accepted")
			}
			if e := os.WriteFile(artifactPath, raw, 0600); e != nil {
				t.Fatal(e)
			}
		})
	}
}
