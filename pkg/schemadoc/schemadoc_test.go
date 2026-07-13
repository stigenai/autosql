package schemadoc

import (
	"strings"
	"testing"
	"time"

	"autosql/pkg/schema"
)

func bundle(name string) Bundle {
	n := schema.Name{Schema: "app", Name: name}
	r := schema.Resource{ID: schema.StableID(schema.KindTable, n), Kind: schema.KindTable, Name: n}
	d := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{r}}}
	d.Normalize()
	return Bundle{ArtifactDigest: "sha256:artifact", ObservedAt: time.Unix(100, 0).UTC(), Document: d}
}

func TestBundlePortableHTMLERDAndCompare(t *testing.T) {
	b := bundle("users")
	if raw, err := b.ExportJSON(); err != nil || !strings.Contains(string(raw), "artifact_digest") {
		t.Fatalf("json: %v", err)
	}
	if html, err := b.HTML(); err != nil || !strings.Contains(string(html), "Search objects") {
		t.Fatalf("html: %v", err)
	}
	erd, err := b.ERDMermaid()
	if err != nil || !strings.Contains(erd, "erDiagram") || !strings.Contains(erd, "users") {
		t.Fatalf("erd: %v %s", err, erd)
	}
	changes, err := Compare(b, bundle("accounts"))
	if err != nil || len(changes.Changes) == 0 {
		t.Fatalf("compare: %v %#v", err, changes)
	}
}
