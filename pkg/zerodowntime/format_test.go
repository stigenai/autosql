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
		{ID: "01", Kind: AddColumn, Table: "accounts", Column: "display_name", DataType: "text", Expression: "coalesce(name, 'unknown')", OrderBy: []string{"id"}, BatchSize: 500},
		{ID: "02", Kind: CreateIndex, Table: "accounts", Index: "accounts_display_name_idx", Expression: "display_name"},
	}, map[string]string{"owner": "database"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

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
		func(x *Migration) { x.Name = "other" }, func(x *Migration) { x.Requirements.MinimumPostgres = 15 },
		func(x *Migration) { x.Operations[0].Expression = "lower(name)" }, func(x *Migration) { x.VersionSchema.Name = "other" },
		func(x *Migration) { x.Metadata["owner"] = "attacker" },
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
		{"duplicate order", func(m *Migration) { m.Operations[0].OrderBy = []string{"id", "id"} }},
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
