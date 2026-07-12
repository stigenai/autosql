package down

import (
	"autosql/pkg/migrate"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"context"
	"os"
	"strings"
	"testing"
)

func TestReplayPriorUsesDistinctLivePostgresAndStopsExactly(t *testing.T) {
	dev := os.Getenv("AUTOSQL_DOWN_DEV_DSN")
	prod := os.Getenv("AUTOSQL_DOWN_PROD_DSN")
	if dev == "" || prod == "" {
		t.Skip("down PostgreSQL URLs not configured")
	}
	devID, e := simulate.ResolvePostgresIdentity(context.Background(), dev)
	if e != nil {
		t.Fatal(e)
	}
	prodID, e := simulate.ResolvePostgresIdentity(context.Background(), prod)
	if e != nil {
		t.Fatal(e)
	}
	d := t.TempDir()
	_ = os.Chmod(d, 0700)
	files := []migrate.File{{Name: "V1__base.sql", SQL: []byte("CREATE SCHEMA down_replay; CREATE TABLE down_replay.items(id bigint);\n")}, {Name: "V2__later.sql", SQL: []byte("ALTER TABLE down_replay.items ADD COLUMN secret text;\n")}}
	if _, e = migrate.Update(d, migrate.UpdateRequest{Files: files}); e != nil {
		t.Fatal(e)
	}
	snap, e := migrate.LoadSnapshot(d)
	if e != nil {
		t.Fatal(e)
	}
	doc, e := ReplayPrior(context.Background(), snap, "1", dev, devID, prodID, []string{"down_replay"})
	if e != nil {
		t.Fatal(e)
	}
	foundTable, foundSecret := false, false
	for _, r := range doc.Graph.Resources {
		foundTable = foundTable || r.Kind == schema.KindTable && r.Name.Name == "items"
		foundSecret = foundSecret || r.Kind == schema.KindColumn && r.Name.Name == "secret"
	}
	if !foundTable || foundSecret {
		t.Fatalf("incorrect prior replay table=%v secret=%v", foundTable, foundSecret)
	}
	_, e = ReplayPrior(context.Background(), snap, "1", dev, prodID, prodID, []string{"down_replay"})
	if e == nil || strings.Contains(e.Error(), dev) {
		t.Fatal("identity separation failed or URL leaked")
	}
}
