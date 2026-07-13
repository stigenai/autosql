package expandplan

import (
	"crypto/ed25519"
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
	return Request{Migration: m, ExpectedFingerprint: "sha256:live", Target: "primary", Environment: "prod", Snapshot: Snapshot{Fingerprint: "sha256:live", Target: "primary", Environment: "prod", MetadataSchema: "autosql_zdm", PostgresMajor: 16, Tables: map[string]Table{"users": {Schema: "public", Name: "users", Owner: "app", CanAlter: true, Columns: map[string]Column{"id": {Name: "id", Type: "bigint"}, "name": {Name: "name", Type: "text", Nullable: true}}}}, Indexes: map[string]Index{}, Mappings: map[string]Mapping{}, Schemas: []string{"public"}, SchemaCreate: map[string]bool{"public": true}, ExistingObjects: map[string]string{}, UniqueEvidence: map[string]UniqueEvidence{"users_pkey": {Name: "users_pkey", Table: "users", Columns: []string{"id"}, Constraint: true, Valid: true, Ready: true}}}, Policy: Policy{MaxLockMS: 100, MaxLockHoldMS: 10000, MaxStatementMS: 10000, MaxTransactionMS: 10000, AllowTableScan: true, AllowValidationScan: true, AllowNonTransactional: true}, Verify: func(zerodowntime.Migration) error { return nil }}
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
		func(r *Request) { r.Snapshot.SchemaCreate["public"] = false },
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
	scope := mappingScope(r.Target, r.Environment, r.Migration.Digest)
	r.Snapshot.Mappings[scope+"/01/column:display_name"] = Mapping{OperationID: "01", LogicalID: "column:display_name", Schema: "public", Name: "evil", Kind: "column", Scope: scope}
	if _, err := Build(r); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	r = base(r.Migration)
	r.Snapshot.Mappings["other"] = Mapping{OperationID: "other", LogicalID: "x", Schema: "public", Name: want, Kind: "column"}
	if _, err := Build(r); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	r = base(r.Migration)
	p, err := Build(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Mappings) != 1 || !strings.HasPrefix(p.Mappings[0].StorageLogicalID(), mappingScope(r.Target, r.Environment, r.Migration.Digest)+"/") {
		t.Fatal("mapping is not target/environment/artifact scoped")
	}
	other := r
	other.Target = "secondary"
	other.Snapshot.Target = "secondary"
	p2, err := Build(other)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Mappings[0].Scope == p.Mappings[0].Scope || p2.Mappings[0].Name == p.Mappings[0].Name {
		t.Fatal("target mapping isolation failed")
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

func TestTrustedPlanRejectsEveryMaterialTamper(t *testing.T) {
	op := zerodowntime.Operation{ID: "01", Kind: zerodowntime.AddColumn, Table: "users", Column: "email", DataType: "text", SynchronizationMode: "none", Effects: addNoneEffects(), Reversal: zerodowntime.Reversal{Mode: "automatic"}}
	r := base(migration(t, []zerodowntime.Operation{op}))
	pub, priv, _ := ed25519.GenerateKey(nil)
	r.PlanKeyID = "planner"
	r.PlanSigner = priv
	p, err := Build(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.VerifyTrusted(r, pub); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Plan){func(x *Plan) { x.Target = "evil" }, func(x *Plan) { x.Steps[0].SQL = "DROP TABLE users" }, func(x *Plan) { x.Steps[0].Locks[0].Mode = "NONE" }, func(x *Plan) { x.Steps[0].TableScan = true }, func(x *Plan) { x.Steps[0].Locks[0].AcquisitionTimeoutMS++ }, func(x *Plan) { x.Steps[0].Locks[0].MaximumHoldMS++ }, func(x *Plan) { x.Steps[0].SessionSetup[0] = "SET search_path=attacker,pg_catalog" }, func(x *Plan) { x.Steps[0].Preconditions[0].Expected = "stale" }, func(x *Plan) { x.Mappings = append(x.Mappings, Mapping{OperationID: "x"}) }, func(x *Plan) { x.Attestation.Signature = "AAAA" }, func(x *Plan) { x.Attestation.KeyID = "other" }, func(x *Plan) { x.Attestation.Algorithm = "evil" }}
	for i, mut := range mutations {
		x := p
		x.Steps = append([]Step(nil), p.Steps...)
		x.Steps[0].Preconditions = append([]Condition(nil), p.Steps[0].Preconditions...)
		x.Steps[0].Locks = append([]LockEvidence(nil), p.Steps[0].Locks...)
		x.Steps[0].SessionSetup = append([]string(nil), p.Steps[0].SessionSetup...)
		x.Mappings = append([]Mapping(nil), p.Mappings...)
		a := *p.Attestation
		x.Attestation = &a
		mut(&x)
		if err = x.VerifyTrusted(r, pub); !errors.Is(err, ErrRefused) {
			t.Fatalf("tamper %d accepted: %v", i, err)
		}
	}
	x := p
	x.Attestation = nil
	x.Steps = append([]Step(nil), p.Steps...)
	x.Steps[0].SQL = "DROP TABLE users"
	x.Digest = ""
	x.Digest = digest(x)
	if err = x.Validate(); !errors.Is(err, ErrRefused) {
		t.Fatal("structural DROP accepted")
	}
}

func TestStructuredLockEvidenceSeparatesAcquisitionAndHold(t *testing.T) {
	op := zerodowntime.Operation{ID: "01", Kind: zerodowntime.AddColumn, Table: "users", Column: "email", DataType: "text", SynchronizationMode: "none", Effects: addNoneEffects(), Reversal: zerodowntime.Reversal{Mode: "automatic"}}
	r := base(migration(t, []zerodowntime.Operation{op}))
	r.Policy.MaxLockMS = 50
	r.Policy.MaxLockHoldMS = 700
	p, err := Build(r)
	if err != nil {
		t.Fatal(err)
	}
	l := p.Steps[0].Locks[0]
	if l.AcquisitionTimeoutMS != 50 || l.MaximumHoldMS != 500 || l.EstimatedHoldMS > l.MaximumHoldMS || l.TransactionBoundary != p.Steps[0].TransactionGroup {
		t.Fatalf("lock=%+v", l)
	}
	if len(p.PlanningLocks) < 2 || p.PlanningLocks[0].Phase != "planning" {
		t.Fatalf("planning locks=%+v", p.PlanningLocks)
	}
}

func TestConservativeIndexEstimateAndTinyBudgetRefusal(t *testing.T) {
	op := zerodowntime.Operation{ID: "01", Kind: zerodowntime.CreateIndex, Table: "users", Index: "users_name_idx", Expression: "name", IndexMode: &zerodowntime.IndexMode{Concurrent: true}, Effects: effects(zerodowntime.CreateIndex), Reversal: zerodowntime.Reversal{Mode: "automatic"}}
	r := base(migration(t, []zerodowntime.Operation{op}))
	r.Policy.MaxLockHoldMS = 100
	if _, err := Build(r); !errors.Is(err, ErrRefused) {
		t.Fatal("tiny index hold budget accepted")
	}
	r = base(r.Migration)
	x := r.Snapshot.Tables["users"]
	x.EstimatedBytes = 100 << 20
	r.Snapshot.Tables["users"] = x
	p, err := Build(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].EstimatedDurationMS <= 1000 || p.Steps[0].Locks[0].MaximumHoldMS <= p.Steps[0].EstimatedDurationMS {
		t.Fatalf("size estimate=%+v", p.Steps[0])
	}
}

func TestDeferredLockHoldBoundaries(t *testing.T) {
	ordering := &zerodowntime.Ordering{Columns: []string{"id"}, Unique: zerodowntime.UniqueEvidence{Kind: "constraint", Name: "users_pkey", Columns: []string{"id"}}}
	cases := []zerodowntime.Operation{{ID: "01", Kind: zerodowntime.AddColumn, Table: "users", Column: "copy", DataType: "text", Expression: "name", SynchronizationMode: "backfill", Ordering: ordering, BatchSize: 10, Effects: effects(zerodowntime.AddColumn), Reversal: zerodowntime.Reversal{Mode: "automatic"}}, {ID: "01", Kind: zerodowntime.SetNotNull, Table: "users", Column: "name", Expression: "coalesce(name, 'x')", Ordering: ordering, BatchSize: 10, Effects: effects(zerodowntime.SetNotNull), Reversal: zerodowntime.Reversal{Mode: "automatic"}}}
	for _, op := range cases {
		r := base(migration(t, []zerodowntime.Operation{op}))
		r.Policy.MaxLockHoldMS = 1999
		if _, err := Build(r); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s below-bound accepted", op.Kind)
		}
		r.Policy.MaxLockHoldMS = 2000
		if _, err := Build(r); err != nil {
			t.Fatalf("%s exact boundary refused: %v", op.Kind, err)
		}
	}
}

func TestMetadataSchemaBindsAdvisoryDomainAndPlanIdentity(t *testing.T) {
	op := zerodowntime.Operation{ID: "01", Kind: zerodowntime.AddColumn, Table: "users", Column: "email", DataType: "text", SynchronizationMode: "none", Effects: addNoneEffects(), Reversal: zerodowntime.Reversal{Mode: "automatic"}}
	r := base(migration(t, []zerodowntime.Operation{op}))
	p, err := Build(r)
	if err != nil {
		t.Fatal(err)
	}
	want, err := PlanningAdvisoryDomain("autosql_zdm", "primary", "prod")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range p.PlanningLocks {
		if l.Kind == "advisory" {
			found = true
			if l.Object != want {
				t.Fatalf("reported=%q want=%q", l.Object, want)
			}
		}
	}
	if !found {
		t.Fatal("advisory evidence missing")
	}
	tampered := p
	tampered.MetadataSchema = "evil"
	if err = tampered.Validate(); !errors.Is(err, ErrRefused) {
		t.Fatal("metadata tamper accepted")
	}
	other := r
	other.Snapshot.MetadataSchema = "other_zdm"
	p2, err := Build(other)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Digest == p.Digest || p2.BindingsDigest == p.BindingsDigest {
		t.Fatal("metadata schema is not bound into plan identity")
	}
}

func TestPlanningAdvisoryDomainCanonicalAdversarialInputs(t *testing.T) {
	a, err := PlanningAdvisoryDomain("a/b", "c", "d")
	if err != nil {
		t.Fatal(err)
	}
	b, err := PlanningAdvisoryDomain("a", "b/c", "d")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("delimiter collision")
	}
	u, err := PlanningAdvisoryDomain("元数据", "目标/α", "生产")
	if err != nil || !strings.HasPrefix(u, "autosql.zdm.expand-plan-lock/v1/sha256:") {
		t.Fatalf("unicode=%q err=%v", u, err)
	}
	boundary := strings.Repeat("x", 4096)
	if _, err = PlanningAdvisoryDomain(boundary, "t", "e"); err != nil {
		t.Fatal(err)
	}
	for _, parts := range [][3]string{{"", "t", "e"}, {"m", "", "e"}, {"m", "t", ""}, {strings.Repeat("x", 4097), "t", "e"}, {string([]byte{0xff}), "t", "e"}} {
		if _, err = PlanningAdvisoryDomain(parts[0], parts[1], parts[2]); !errors.Is(err, ErrRefused) {
			t.Fatalf("invalid accepted %#v err=%v", parts, err)
		}
	}
}

func TestBudgetAndEvidenceBoundaries(t *testing.T) {
	op := zerodowntime.Operation{ID: "01", Kind: zerodowntime.RenameColumn, Table: "users", Column: "name", NewName: "display_name", Expression: "name", Ordering: &zerodowntime.Ordering{Columns: []string{"id"}, Unique: zerodowntime.UniqueEvidence{Kind: "constraint", Name: "users_pkey", Columns: []string{"id"}}}, BatchSize: 10, Effects: effects(zerodowntime.RenameColumn), Reversal: zerodowntime.Reversal{Mode: "automatic"}}
	r := base(migration(t, []zerodowntime.Operation{op}))
	if _, err := Build(r); err != nil {
		t.Fatal(err)
	}
	r.Policy.MaxTransactionMS = r.Migration.Requirements.StatementTimeoutMS - 1
	if _, err := Build(r); !errors.Is(err, ErrRefused) {
		t.Fatal("transaction boundary accepted")
	}
	r = base(r.Migration)
	r.Policy.AllowTableScan = false
	if _, err := Build(r); !errors.Is(err, ErrRefused) {
		t.Fatal("scan policy bypassed")
	}
	r = base(r.Migration)
	e := r.Snapshot.UniqueEvidence["users_pkey"]
	e.Ready = false
	r.Snapshot.UniqueEvidence["users_pkey"] = e
	if _, err := Build(r); !errors.Is(err, ErrRefused) {
		t.Fatal("unready uniqueness evidence accepted")
	}
}
