package source

import (
	"context"
	"fmt"
	"testing"

	"autosql/pkg/schema"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func canonicalScaleHCL(tb testing.TB, count int) []byte {
	tb.Helper()
	doc := schema.Document{Version: schema.SchemaVersion}
	for index := 0; index < count; index++ {
		name := schema.Name{Name: fmt.Sprintf("schema_%04d", index)}
		doc.Graph.Resources = append(doc.Graph.Resources, schema.Resource{
			ID:   schema.StableID(schema.KindSchema, name),
			Kind: schema.KindSchema,
			Name: name,
			Spec: []byte(`{"comment":"cell-scale canonical fixture"}`),
		})
	}
	formatted, err := FormatHCL(doc)
	if err != nil {
		tb.Fatalf("format canonical scale fixture: %v", err)
	}
	return formatted
}

func TestExpressionValueSkipsUnreferencedSymbolTrees(t *testing.T) {
	data := []byte(`value = "literal"`)
	file, diagnostics := hclsyntaxParse(data, "literal.hcl")
	if diagnostics.HasErrors() {
		t.Fatalf("parse expression fixture: %s", diagnostics.Error())
	}
	expression := file.Body.(*hclsyntax.Body).Attributes["value"].Expr

	got, err := expressionValueWithSymbols(expression, data, nil, map[string]any{
		"unused": struct{}{}, // ctyValue rejects this if the unused tree is visited.
	})
	if err != nil {
		t.Fatalf("evaluate literal with unused symbols: %v", err)
	}
	if got != "literal" {
		t.Fatalf("literal = %#v, want %q", got, "literal")
	}
}

func TestParseHCLCanonicalScaleRemainsLinear(t *testing.T) {
	small := canonicalScaleHCL(t, 100)
	large := canonicalScaleHCL(t, 200)
	parse := func(data []byte) {
		if _, err := ParseHCLContext(context.Background(), "cell-scale.hcl", data, nil); err != nil {
			t.Fatalf("parse canonical scale fixture: %v", err)
		}
	}

	smallAllocs := testing.AllocsPerRun(1, func() { parse(small) })
	largeAllocs := testing.AllocsPerRun(1, func() { parse(large) })
	if largeAllocs > smallAllocs*3 {
		t.Fatalf("canonical parsing scales superlinearly: 100 resources = %.0f allocations, 200 resources = %.0f allocations", smallAllocs, largeAllocs)
	}
}

func BenchmarkParseHCLCanonicalCellScale(b *testing.B) {
	data := canonicalScaleHCL(b, 2_000)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		if _, err := ParseHCLContext(context.Background(), "cell-scale.hcl", data, nil); err != nil {
			b.Fatal(err)
		}
	}
}
