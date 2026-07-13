package dialect

import (
	"autosql/pkg/schema"
	"context"
	"testing"
)

func TestDescriptorsCapabilityGateAndNormalize(t *testing.T) {
	d := MySQLDescriptor()
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindTable, schema.Name{Name: "users", Schema: "public"}), Kind: schema.KindTable, Name: schema.Name{Name: "users", Schema: "public"}}}}}
	norm, e := d.Normalize(context.Background(), doc)
	if e != nil || norm.Annotations["dialect"] != "mysql" {
		t.Fatalf("norm=%+v err=%v", norm, e)
	}
	if e = d.Require(schema.KindPolicy, schema.OperationCreate); e == nil {
		t.Fatal("unsupported policy accepted")
	}
	if e = d.Require(schema.KindTable, schema.OperationCreate); e != nil {
		t.Fatal(e)
	}
}
