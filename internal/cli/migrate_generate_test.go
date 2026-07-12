package cli

import (
	"autosql/pkg/migrate"
	"autosql/pkg/plan"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/schema"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateGeneratePublishesAndNoOpPreservesBytes(t *testing.T) {
	d := t.TempDir()
	_ = os.Chmod(d, 0755)
	base := []migrate.File{{Name: "V1__base.sql", SQL: []byte("SELECT 1;\n")}}
	if _, e := migrate.Update(d, migrate.UpdateRequest{Files: base}); e != nil {
		t.Fatal(e)
	}
	from := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	to := from
	n := schema.Name{Name: "app"}
	to.Graph.Resources = []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n, Spec: []byte(`{}`)}}
	p, e := plan.Build(context.Background(), sample.Driver{}, from, to, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	r := &fakeRead{from: from, to: to, p: p}
	o := output{streams: Streams{Out: &strings.Builder{}, Err: &strings.Builder{}}}
	if e = runMigrateGenerate(context.Background(), []string{"--dir", d, "--from", "from", "--to", "to", "--version", "2", "--label", "add_app"}, o, r); e == nil {
		t.Fatal("untrusted generation published")
	}
	s, e := migrate.LoadSnapshot(d)
	if e != nil || len(s.Manifest.Entries) != 1 {
		t.Fatalf("snapshot=%+v err=%v", s, e)
	}
	before, _ := os.ReadFile(filepath.Join(d, migrate.ManifestFile))
	after, _ := os.ReadFile(filepath.Join(d, migrate.ManifestFile))
	if string(before) != string(after) {
		t.Fatal("no-op changed manifest bytes")
	}
}
