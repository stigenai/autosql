package zerodowntime

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func valid(t *testing.T) Migration {
	t.Helper()
	m, err := New("add_accounts", VersionSchema{Name: "v_accounts", ExposeDuringExpand: true}, Requirements{MinimumPostgres: 14, LockTimeoutMS: 1000, StatementTimeoutMS: 60000}, []Operation{
		{ID: "01", Kind: AddColumn, Table: "accounts", Column: "display_name", DataType: "text", Expression: "coalesce(name, 'unknown')", Ordering: &Ordering{Columns: []string{"id"}, Unique: UniqueEvidence{Kind: "constraint", Name: "accounts_pkey", Columns: []string{"id"}}}, BatchSize: 500, Effects: mustEffects(AddColumn), Reversal: Reversal{Mode: "automatic"}},
		{ID: "02", Kind: CreateIndex, Table: "accounts", Index: "accounts_display_name_idx", Expression: "display_name", IndexMode: &IndexMode{Concurrent: true}, Effects: mustEffects(CreateIndex), Reversal: Reversal{Mode: "automatic"}},
	}, map[string]string{"owner": "database"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustEffects(kind OperationKind) PhaseEffect { e, _ := Effects(kind); return e }

func TestJSONYAMLRoundTripsAreDeterministic(t *testing.T) {
	m := valid(t)
	j1, err := m.MarshalJSONCanonical()
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := ParseJSON(j1)
	if err != nil {
		t.Fatal(err)
	}
	j2, _ := fromJSON.MarshalJSONCanonical()
	if !bytes.Equal(j1, j2) {
		t.Fatal("JSON is not deterministic")
	}
	y1, err := m.MarshalYAMLCanonical()
	if err != nil {
		t.Fatal(err)
	}
	fromYAML, err := ParseYAML(y1)
	if err != nil {
		t.Fatalf("parse YAML: %v\n%s", err, y1)
	}
	y2, _ := fromYAML.MarshalYAMLCanonical()
	if !bytes.Equal(y1, y2) {
		t.Fatal("YAML is not deterministic")
	}
	if fromYAML.Digest != fromJSON.Digest {
		t.Fatal("representations changed content identity")
	}
}

func TestSignedArtifactDetectsEverySemanticTamper(t *testing.T) {
	m := valid(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	if err := m.Sign("release", priv); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(pub); err != nil {
		t.Fatal(err)
	}
	tests := []func(*Migration){
		func(x *Migration) { x.Version = "other" },
		func(x *Migration) { x.Name = "other" }, func(x *Migration) { x.Requirements.MinimumPostgres = 15 },
		func(x *Migration) { x.Operations[0].Expression = "lower(name)" }, func(x *Migration) { x.VersionSchema.Name = "other" },
		func(x *Migration) { x.Metadata["owner"] = "attacker" },
		func(x *Migration) { x.Operations[0].Effects.Expand = "evil" },
		func(x *Migration) { x.Operations[0].Reversal.Mode = "backup" },
		func(x *Migration) { x.Signature.KeyID = "other" },
		func(x *Migration) { x.Signature.Algorithm = "RSA" },
		func(x *Migration) { x.Signature.Value = "AAAA" },
		func(x *Migration) { x.Digest = "sha256:" + strings.Repeat("0", 64) },
	}
	for i, mutate := range tests {
		x := m
		x.Operations = append([]Operation(nil), m.Operations...)
		x.Metadata = clone(m.Metadata)
		mutate(&x)
		if err := x.Verify(pub); err == nil {
			t.Fatalf("tamper %d accepted", i)
		}
	}
	_, wrong, _ := ed25519.GenerateKey(nil)
	if err := m.Verify(wrong.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestExpressionASTAllowlistRejectsAdversarialForms(t *testing.T) {
	bad := []string{"pg_read_file('/etc/passwd')", `"pg_read_file"('/etc/passwd')`, "pg_read_file/**/('/etc/passwd')", "pg_catalog.lower(name)", "pg_notify('x','y')", "set_config('x','y',false)", "gen_random_uuid()", "nextval('s')", "(SELECT secret FROM users)", "current_user", "name::regclass", "name::pg_catalog.text", "name OPERATOR(pg_catalog.+) 1", "name; SELECT 1", "$1"}
	for _, expression := range bad {
		t.Run(expression, func(t *testing.T) {
			m := valid(t)
			m.Operations[0].Expression = expression
			d, _ := digest(m)
			m.Digest = d
			if err := m.Validate(); err == nil {
				t.Fatal("adversarial expression accepted")
			}
		})
	}
	good := []string{"lower(name)", "coalesce(name, 'unknown')", "id::text", "CASE WHEN name IS NULL THEN 'x' ELSE name END"}
	for _, expression := range good {
		if err := validateExpression(expression); err != nil {
			t.Errorf("safe expression %q: %v", expression, err)
		}
	}
}

func TestDataTypeUsesStandaloneASTPolicy(t *testing.T) {
	good := []string{"text", "int8", "numeric(10,2)", "timestamp(3)"}
	for _, v := range good {
		if err := validateDataType(v); err != nil {
			t.Errorf("safe type %q: %v", v, err)
		}
	}
	bad := []string{"text default current_user", "text not null", "int generated always as identity", "text; drop table users", "regclass", "pg_catalog.text", "\"text\""}
	for _, v := range bad {
		if err := validateDataType(v); err == nil {
			t.Errorf("unsafe type %q accepted", v)
		}
	}
}

func TestEveryBackfillClaimRequiresExecutionEvidence(t *testing.T) {
	for _, kind := range []OperationKind{RenameColumn, SetNotNull} {
		m := valid(t)
		op := m.Operations[0]
		op.Kind = kind
		op.Effects = mustEffects(kind)
		op.DataType = ""
		switch kind {
		case RenameColumn:
			op.NewName = "display_name_v2"
		case SetNotNull:
			op.NewName = ""
		}
		m.Operations = []Operation{op}
		d, _ := digest(m)
		m.Digest = d
		if err := m.Validate(); err != nil {
			t.Fatalf("%s complete evidence: %v", kind, err)
		}
		m.Operations[0].Ordering = nil
		d, _ = digest(m)
		m.Digest = d
		if err := m.Validate(); err == nil {
			t.Fatalf("%s accepted without ordering", kind)
		}
	}
}

func TestBackfillReversalAndIndexContracts(t *testing.T) {
	mutations := []func(*Migration){
		func(m *Migration) { m.Operations[0].Ordering = nil }, func(m *Migration) { m.Operations[0].Ordering.Unique.Name = "" }, func(m *Migration) { m.Operations[0].Ordering.Unique.Kind = "claimed" }, func(m *Migration) { m.Operations[0].BatchSize = 0 }, func(m *Migration) { m.Operations[0].Ordering.Unique.Columns = []string{"other"} },
		func(m *Migration) { m.Operations[1].IndexMode.Concurrent = false }, func(m *Migration) { m.Operations[1].IndexMode.Partitioned = true }, func(m *Migration) { m.Operations[1].IndexMode.BacksConstraint = true }, func(m *Migration) { m.Operations[1].Column = "illegal" },
	}
	for i, mutate := range mutations {
		m := valid(t)
		mutate(&m)
		d, _ := digest(m)
		m.Digest = d
		if err := m.Validate(); err == nil {
			t.Fatalf("invalid contract %d accepted", i)
		}
	}
	e := mustEffects(AlterColumnType)
	op := Operation{ID: "01", Kind: AlterColumnType, Table: "items", Column: "amount", DataType: "numeric", Expression: "amount::numeric", Ordering: &Ordering{Columns: []string{"id"}, Unique: UniqueEvidence{Kind: "constraint", Name: "items_pkey", Columns: []string{"id"}}}, BatchSize: 100, Effects: e, Reversal: Reversal{Mode: "lossless", Expression: "amount::int8"}}
	if _, err := New("alter_items", VersionSchema{Name: "v_items"}, Requirements{MinimumPostgres: 14, LockTimeoutMS: 1, StatementTimeoutMS: 2}, []Operation{op}, nil); err != nil {
		t.Fatal(err)
	}
	op.Reversal.Expression = ""
	if _, err := New("alter_items", VersionSchema{Name: "v_items"}, Requirements{MinimumPostgres: 14, LockTimeoutMS: 1, StatementTimeoutMS: 2}, []Operation{op}, nil); err == nil {
		t.Fatal("missing reverse transform accepted")
	}
}

func TestOfflineValidationRejectsUnsafeAndUnstableInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Migration)
	}{
		{"unsupported", func(m *Migration) { m.Operations[0].Kind = "execute_sql" }},
		{"volatile", func(m *Migration) { m.Operations[0].Expression = "random()" }},
		{"injection", func(m *Migration) { m.Operations[0].Expression = "name; DROP TABLE accounts" }},
		{"duplicate", func(m *Migration) { m.Operations[1].ID = m.Operations[0].ID }},
		{"unstable", func(m *Migration) { m.Operations[0], m.Operations[1] = m.Operations[1], m.Operations[0] }},
		{"old postgres", func(m *Migration) { m.Requirements.MinimumPostgres = 13 }},
		{"unsafe object", func(m *Migration) { m.Operations[0].Table = "accounts;drop" }},
		{"duplicate order", func(m *Migration) { m.Operations[0].Ordering.Columns = []string{"id", "id"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid(t)
			tt.mutate(&m)
			d, _ := digest(m)
			m.Digest = d
			if err := m.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestStrictDecodersAndRedactedErrors(t *testing.T) {
	m := valid(t)
	b, _ := m.MarshalJSONCanonical()
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	obj["password"] = "super-secret"
	bad, _ := json.Marshal(obj)
	_, err := ParseJSON(bad)
	if err == nil {
		t.Fatal("unknown field accepted")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatal("error leaked input")
	}
	y, _ := m.MarshalYAMLCanonical()
	y = append(y, []byte("password: super-secret\n")...)
	_, err = ParseYAML(y)
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("unsafe YAML error: %v", err)
	}
}

func TestStrictInputRejectsDuplicatesAliasesTagsAndTrailingDocuments(t *testing.T) {
	m := valid(t)
	b, _ := m.MarshalJSONCanonical()
	duplicate := bytes.Replace(b, []byte(`"name":"add_accounts"`), []byte(`"name":"add_accounts","name":"other"`), 1)
	if _, err := ParseJSON(duplicate); err == nil {
		t.Fatal("duplicate JSON key accepted")
	}
	if _, err := ParseJSON(append(b, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	y, _ := m.MarshalYAMLCanonical()
	cases := [][]byte{append([]byte(nil), append(y, []byte("name: duplicate\n")...)...), []byte("version: &v autosql.zero-downtime/v1\nname: *v\n"), []byte("version: !evil autosql.zero-downtime/v1\n"), append(append([]byte(nil), y...), []byte("---\n{}\n")...)}
	for i, input := range cases {
		if _, err := ParseYAML(input); err == nil {
			t.Fatalf("unsafe YAML %d accepted", i)
		}
	}
	legacy := `{"version":"autosql.zero-downtime/v0","version":"autosql.zero-downtime/v0"}`
	if _, err := UpgradeLegacyJSON([]byte(legacy)); err == nil {
		t.Fatal("legacy duplicate accepted")
	}
}

func TestCapabilityMatrixAndEffectsCoverAllVersionsAndOperations(t *testing.T) {
	matrix := CapabilityMatrix()
	if len(matrix) != 45 {
		t.Fatalf("got %d capabilities", len(matrix))
	}
	counts := map[OperationKind]int{}
	for _, c := range matrix {
		if c.Postgres < 14 || c.Postgres > 18 || c.Availability == "" || c.Lock == "" || c.Rewrite == "" {
			t.Fatalf("incomplete capability: %+v", c)
		}
		counts[c.Operation]++
	}
	for kind, n := range counts {
		if n != 5 {
			t.Fatalf("%s has %d versions", kind, n)
		}
		e, ok := Effects(kind)
		if !ok || e.Expand == "" || e.Synchronize == "" || e.Contract == "" || e.Reverse == "" {
			t.Fatalf("incomplete effects: %s", kind)
		}
	}
}

func TestTargetVersionCompatibility(t *testing.T) {
	m := valid(t)
	if err := m.ValidateForPostgres(14); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{13, 19} {
		if err := m.ValidateForPostgres(version); !errors.Is(err, ErrInvalid) {
			t.Fatalf("PostgreSQL %d: %v", version, err)
		}
	}
}

func TestLegacyUpgradeIsExplicitAndStable(t *testing.T) {
	legacy := `{"version":"autosql.zero-downtime/v0","name":"add_accounts","schema":"v_accounts","minimum_postgres":"14","operations":[{"id":"01","kind":"add_column","table":"accounts","column":"nickname","data_type":"text"}]}`
	m, err := UpgradeLegacyJSON([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if m.Metadata["upgraded_from"] != LegacyVersion || m.Version != Version {
		t.Fatalf("upgrade evidence missing: %+v", m)
	}
	a, _ := m.MarshalJSONCanonical()
	m2, err := UpgradeLegacyJSON([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m2.MarshalJSONCanonical()
	if !bytes.Equal(a, b) {
		t.Fatal("upgrade is not deterministic")
	}
	if _, err := UpgradeLegacyJSON([]byte(strings.Replace(legacy, LegacyVersion, "unknown", 1))); err == nil {
		t.Fatal("unknown legacy version accepted")
	}
}

func TestSortedOperationsDoesNotMutateInput(t *testing.T) {
	in := []Operation{{ID: "b"}, {ID: "a"}}
	out := SortedOperations(in)
	if out[0].ID != "a" || in[0].ID != "b" {
		t.Fatal("sort contract violated")
	}
}
