package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func dynamicPolicy() DynamicPolicy {
	return DynamicPolicy{AllowedPrograms: map[string]bool{"emit": true}, MaxBytes: 4096, MaxRows: 10, Timeout: time.Second}
}
func TestDynamicResolverProvenanceAndLock(t *testing.T) {
	r := NewResolver()
	r.ReadFile = func(string) ([]byte, error) { return []byte(`[ {"id": 1, "name": "US"} ]`), nil }
	s := DynamicSource{URI: "file://countries.json", Kind: KindFile, Format: FormatNative, MaxBytes: 100, Timeout: time.Second, Allowlisted: true}
	a, err := r.Resolve(context.Background(), s, dynamicPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if a.Provenance.Digest == "" || a.Provenance.URI != s.URI || a.Provenance.Locked {
		t.Fatalf("provenance=%+v", a.Provenance)
	}
	p := dynamicPolicy()
	p.Locks = map[string]string{s.URI: a.Provenance.Digest}
	a, err = r.Resolve(context.Background(), s, p)
	if err != nil || !a.Provenance.Locked {
		t.Fatalf("locked=%+v err=%v", a.Provenance, err)
	}
	p.Locks[s.URI] = "sha256:" + "0"
	if _, err = r.Resolve(context.Background(), s, p); !errors.Is(err, ErrLockMismatch) {
		t.Fatalf("lock error=%v", err)
	}
}
func TestDynamicOfflineCacheAndBounds(t *testing.T) {
	r := NewResolver()
	r.Cache = map[string][]byte{"file://x.csv": []byte("id,name\n1,one\n2,two\n")}
	p := dynamicPolicy()
	p.Offline = true
	s := DynamicSource{URI: "file://x.csv", Kind: KindFile, Format: Format("csv"), MaxBytes: 100, Timeout: time.Second, Allowlisted: true}
	a, err := r.Resolve(context.Background(), s, p)
	if err != nil || !a.Provenance.Locked {
		t.Fatalf("offline=%+v err=%v", a, err)
	}
	rows, err := DecodeRows(a.Data, s.Format, 2, 100)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if _, err = DecodeRows(a.Data, s.Format, 1, 100); !errors.Is(err, ErrDynamicLimit) {
		t.Fatalf("row bound=%v", err)
	}
	p.MaxBytes = 2
	if _, err = r.Resolve(context.Background(), s, p); !errors.Is(err, ErrDynamicLimit) {
		t.Fatalf("byte bound=%v", err)
	}
}
func TestProgramAllowlistAndTemplateComposition(t *testing.T) {
	r := NewResolver()
	r.Run = func(_ context.Context, c []string) ([]byte, error) {
		if c[0] != "emit" {
			t.Fatal(c)
		}
		return []byte(`{"a":1}`), nil
	}
	s := DynamicSource{URI: "program://emit", Kind: KindProgram, Format: FormatNative, Command: []string{"emit"}, MaxBytes: 100, Timeout: time.Second, Allowlisted: true}
	if _, err := r.Resolve(context.Background(), s, dynamicPolicy()); err != nil {
		t.Fatal(err)
	}
	out, err := RenderTemplates([]TemplateInput{{Name: "02.sql", Text: "select {{.name}};"}, {Name: "01.sql", Text: "select 1;"}}, map[string]string{"name": "users"}, 100)
	if err != nil || string(out) != "-- source: 01.sql\nselect 1;\n-- source: 02.sql\nselect users;\n" {
		t.Fatalf("template=%q err=%v", out, err)
	}
	merged, err := ComposeJSON([]byte(`{"graph":{"resources":[1]},"x":1}`), []byte(`{"graph":{"resources":[2]},"y":2}`))
	if err != nil || string(merged) != "{\"graph\":{\"resources\":[2]},\"x\":1,\"y\":2}" {
		t.Fatalf("merge=%s err=%v", merged, err)
	}
}

func TestTemplateDirectorySource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "01.sql"), []byte("select 1;"), 0600); err != nil {
		t.Fatal(err)
	}
	r := NewResolver()
	s := DynamicSource{URI: "file://" + dir, Kind: KindTemplate, Format: FormatSQL, MaxBytes: 100, Timeout: time.Second, Allowlisted: true, Variables: map[string]string{}}
	a, err := r.Resolve(context.Background(), s, dynamicPolicy())
	if err != nil || string(a.Data) != "-- source: 01.sql\nselect 1;\n" {
		t.Fatalf("artifact=%q err=%v", a.Data, err)
	}
}
