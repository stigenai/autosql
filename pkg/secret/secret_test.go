package secret

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolverAndRedaction(t *testing.T) {
	r := NewResolver()
	r.Getenv = func(k string) (string, bool) { return "p@ss word", k == "DB_PASSWORD" }
	v, err := r.Resolve(context.Background(), Reference("env://DB_PASSWORD"))
	if err != nil || v != "p@ss word" {
		t.Fatalf("Resolve = %q, %v", v, err)
	}
	got := r.Redactor.String("dsn=p@ss word encoded=p%40ss+word")
	if strings.Contains(got, "p@ss") || strings.Contains(got, "p%40") {
		t.Fatalf("secret leaked: %s", got)
	}
}

func TestReferenceSerializesOnlyReference(t *testing.T) {
	b, _ := json.Marshal(Reference("env://TOKEN"))
	if string(b) != `"env://TOKEN"` {
		t.Fatalf("got %s", b)
	}
}

func TestInvalidReferences(t *testing.T) {
	for _, value := range []string{"TOKEN", "vault://key", "env://", "env://BAD-NAME", "env://GOOD/path", "env://TOKEN?x=y", "file://remote/path"} {
		if _, err := Parse(value); err == nil {
			t.Errorf("Parse(%q) succeeded", value)
		}
	}
}

func TestFileProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := NewResolver()
	got, err := r.Resolve(context.Background(), Reference("file://"+path))
	if err != nil || got != "file-secret" {
		t.Fatalf("got %q, %v", got, err)
	}
	if strings.Contains(r.Redactor.String("value=file-secret"), "file-secret") {
		t.Fatal("file secret leaked")
	}
}

func TestShortSecretsAreRedacted(t *testing.T) {
	r := NewRedactor("x")
	if got := r.String("token=x"); got != "token=[REDACTED]" {
		t.Fatalf("got %q", got)
	}
}
