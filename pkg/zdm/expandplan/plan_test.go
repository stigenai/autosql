package expandplan

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"autosql/pkg/zerodowntime"
)

func migration(t *testing.T, ops []zerodowntime.Operation) zerodowntime.Migration {
	t.Helper()
	m, err := zerodowntime.New("release_2", zerodowntime.VersionSchema{Name: "v2", ExposeDuringExpand: true}, zerodowntime.Requirements{MinimumPostgres: 14, LockTimeoutMS: 50, StatementTimeoutMS: 5000}, ops, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func effects(k zerodowntime.OperationKind) zerodowntime.PhaseEffect {
	e, _ := zerodowntime.Effects(k)
	return e
}
func addNoneEffects() zerodowntime.PhaseEffect {
	e, _ := zerodowntime.AddColumnEffects("none")
	return e
}
func base(m zerodowntime.Migration) Request {
	return Request{Migration: m, ExpectedFingerprint: "sha256:live", Target: "primary", Environment: "prod", Snapshot: Snapshot{Fingerprint: "sha256:live", Target: "primary", Environment: "prod", PostgresMajor: 16, Tables: map[string]Table{"users": {Schema: "public", Name: "users", Owner: "app", Columns: map[string]Column{"id": {Name: "id", Type: "bigint"}, "name": {Name: "name", Type: "text", Nullable: true}}}}, Indexes: map[string]Index{}, Mappings: map[string]Mapping{}, Schemas: []string{"public"}}, Policy: Policy{MaxLockMS: 100, MaxStatementMS: 10000, MaxTransactionMS: 10000, AllowTableScan: true, AllowValidationScan: true, AllowNonTransactional: true}, Verify: func(zerodowntime.Migration) error { return nil }}
}

func TestBuildAdditiveAndBreakingPlans(t *testing.T) {
	ops := []zerodowntime.Operation{
		{ID: "01", Kind: zerodowntime.AddColumn, Table: "users", Column: "email", DataType: "text", SynchronizationMode: "none", Effects: addNoneEffects(), Reversal: zerodowntime.Reversal{Mode: "automatic"}},
		{ID: "02", Kind: zerodowntime.RenameColumn, Table: "users", Column: "name", NewName: "display_name", Expression: "name", Ordering: &zerodowntime.Ordering{Columns: []string{"id"}, Unique: zerodowntime.UniqueEvidence{Kind: "constraint", Name: "users_pkey", Columns: []string{"id"}}}, BatchSize: 100, Effects: effects(zerodowntime.RenameColumn), Reversal: zerodowntime.Reversal{Mode: "automatic"}},
		{ID: "03", Kind: zerodowntime.DropColumn, Table: "users", Column: "name", Effects: effects(zerodowntime.DropColumn), Reversal: zerodowntime.Reversal{Mode: "backup", BackupReference: "s3://audit/drop"}},
	}
	r := base(migration(t, ops))
	p, err := Build(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 3 || p.Steps[2].SQL != "" || p.Steps[2].Kind != "defer_contract" {
		t.Fatalf("plan=%+v", p)
	}
	if !strings.Contains(p.Steps[0].SQL, "ADD COLUMN") || !strings.Contains(p.Steps[1].SQL, "autosql_display_name_") {
		t.Fatalf("sql=%q / %q", p.Steps[0].SQL, p.Steps[1].SQL)
	}
	if len(p.Mappings) != 1 || len(p.Mappings[0].Name) > 63 {
		t.Fatalf("mappings=%+v", p.Mappings)
	}
	b, _ := json.Marshal(p)
	var p2 Plan
	if json.Unmarshal(b, &p2) != nil || p2.Digest != p.Digest {
		t.Fatal("plan is not immutable JSON")
	}
	p3, err := Build(r)
	if err != nil || p3.Digest != p.Digest {
		t.Fatal("nondeterministic replay")
	}
	if err = p.Validate(); err != nil {
		t.Fatal(err)
	}
	tampered := p
	tampered.Target = "other"
	if err = tampered.Validate(); !errors.Is(err, ErrRefused) {
		t.Fatal("edited plan accepted")
	}
}

func TestBuildRefusesBeforeProducingPartialPlan(t *testing.T) {
	op := zerodowntime.Operation{ID: "01", Kind: zerodowntime.CreateIndex, Table: "users", Index: "users_name_idx", Expression: "name", IndexMode: &zerodowntime.IndexMode{Concurrent: true}, Effects: effects(zerodowntime.CreateIndex), Reversal: zerodowntime.Reversal{Mode: "automatic"}}
	r := base(migration(t, []zerodowntime.Operation{op}))
	cases := []func(*Request){
		func(r *Request) { r.Verify = nil }, func(r *Request) { r.ExpectedFingerprint = "stale" }, func(r *Request) { r.Environment = "other" }, func(r *Request) { r.Policy.AllowNonTransactional = false }, func(r *Request) {
			x := r.Snapshot.Tables["users"]
			x.Partitioned = true
			r.Snapshot.Tables["users"] = x
		},
	}
	for i, mut := range cases {
		x := r
		mut(&x)
		p, err := Build(x)
		if !errors.Is(err, ErrRefused) || len(p.Steps) != 0 {
			t.Fatalf("case %d plan=%+v err=%v", i, p, err)
		}
	}
}

func TestMappingTamperAndCollisionRefused(t *testing.T) {
	op := zerodowntime.Operation{ID: "01", Kind: zerodowntime.RenameColumn, Table: "users", Column: "name", NewName: "display_name", Expression: "name", Ordering: &zerodowntime.Ordering{Columns: []string{"id"}, Unique: zerodowntime.UniqueEvidence{Kind: "constraint", Name: "users_pkey", Columns: []string{"id"}}}, BatchSize: 10, Effects: effects(zerodowntime.RenameColumn), Reversal: zerodowntime.Reversal{Mode: "automatic"}}
	r := base(migration(t, []zerodowntime.Operation{op}))
	want := physicalName(r.Target, r.Migration.Name, op.ID, "column", "display_name")
	r.Snapshot.Mappings["01/column:display_name"] = Mapping{OperationID: "01", LogicalID: "column:display_name", Schema: "public", Name: "evil", Kind: "column"}
	if _, err := Build(r); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	r = base(r.Migration)
	r.Snapshot.Mappings["other"] = Mapping{OperationID: "other", LogicalID: "x", Schema: "public", Name: want, Kind: "column"}
	if _, err := Build(r); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
}

func TestEverySupportedOperationTranslatesWithoutDestructiveExpandSQL(t *testing.T) {
	// Format validation owns detailed field combinations; this assertion guards
	// the central expand invariant against future translators.
	for _, kind := range []zerodowntime.OperationKind{zerodowntime.DropColumn, zerodowntime.DropTable, zerodowntime.DropIndex} {
		op := zerodowntime.Operation{ID: "01", Kind: kind, Table: "users", Effects: effects(kind), Reversal: zerodowntime.Reversal{Mode: "backup", BackupReference: "backup://x"}}
		if kind == zerodowntime.DropColumn {
			op.Column = "name"
		}
		if kind == zerodowntime.DropIndex {
			op.Index = "old_idx"
			op.IndexMode = &zerodowntime.IndexMode{Concurrent: true}
			op.Reversal = zerodowntime.Reversal{Mode: "automatic"}
		}
		r := base(migration(t, []zerodowntime.Operation{op}))
		p, err := Build(r)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToUpper(p.Steps[0].SQL), "DROP") || strings.Contains(strings.ToUpper(p.Steps[0].SQL), "RENAME") {
			t.Fatalf("destructive expand: %q", p.Steps[0].SQL)
		}
	}
}
