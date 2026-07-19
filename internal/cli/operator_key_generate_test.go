package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorKeyGenerateWritesExclusiveNonLeakingKeyFiles(t *testing.T) {
	directory := t.TempDir()
	privatePath, publicPath := filepath.Join(directory, "private.key"), filepath.Join(directory, "public.key")
	var stdout, stderr bytes.Buffer
	args := []string{"operator", "key", "generate", "--private-output", privatePath, "--public-output", publicPath, "--json"}
	if code := Run(context.Background(), args, Streams{Out: &stdout, Err: &stderr}); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	privateText, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	publicText, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	private, privateErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(privateText)))
	public, publicErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(publicText)))
	if privateErr != nil || publicErr != nil || len(private) != 64 || len(public) != 32 {
		t.Fatalf("invalid key sizes private=%d public=%d errors=%v/%v", len(private), len(public), privateErr, publicErr)
	}
	if strings.Contains(stdout.String(), strings.TrimSpace(string(privateText))) || strings.Contains(stderr.String(), strings.TrimSpace(string(privateText))) {
		t.Fatal("private key leaked to command output")
	}
	if code := Run(context.Background(), args, Streams{Out: &stdout, Err: &stderr}); code == 0 {
		t.Fatal("existing key files were overwritten")
	}
}
