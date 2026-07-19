package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorServerCommandYieldsToCLISubcommands(t *testing.T) {
	for _, fixture := range []struct {
		args []string
		want bool
	}{
		{args: []string{"operator"}, want: true},
		{args: []string{"operator", "--leader-election=false"}, want: true},
		{args: []string{"operator", "key", "generate"}, want: false},
		{args: []string{"operator", "artifact", "publish"}, want: false},
		{args: []string{"operator", "future-subcommand"}, want: false},
		{args: []string{"version"}, want: false},
	} {
		if got := operatorServerCommand(fixture.args); got != fixture.want {
			t.Fatalf("operatorServerCommand(%q)=%t want %t", fixture.args, got, fixture.want)
		}
	}
}

func TestBuiltBinaryRunsDocumentedOperatorCLISubcommands(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "autosql")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build autosql: %v\n%s", err, output)
	}

	privatePath := filepath.Join(directory, "private.key")
	publicPath := filepath.Join(directory, "public.key")
	key := exec.Command(binary, "operator", "key", "generate", "--private-output", privatePath, "--public-output", publicPath, "--json")
	output, err := key.CombinedOutput()
	if err != nil {
		t.Fatalf("operator key generate: %v\n%s", err, output)
	}
	privateText, privateErr := os.ReadFile(privatePath)
	publicText, publicErr := os.ReadFile(publicPath)
	private, privateDecodeErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(privateText)))
	public, publicDecodeErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(publicText)))
	if privateErr != nil || publicErr != nil || privateDecodeErr != nil || publicDecodeErr != nil || len(private) != 64 || len(public) != 32 {
		t.Fatalf("built command did not write valid keys: read=%v/%v decode=%v/%v sizes=%d/%d", privateErr, publicErr, privateDecodeErr, publicDecodeErr, len(private), len(public))
	}

	publish := exec.Command(binary, "operator", "artifact", "publish", "--file", filepath.Join(directory, "missing.hcl"), "--config", filepath.Join(directory, "missing.json"), "--output-dir", filepath.Join(directory, "release"), "--source-revision", "git:test")
	output, err = publish.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 3 || !strings.Contains(string(output), "read operator publish configuration failed") || strings.Contains(string(output), "leader-election") {
		t.Fatalf("operator artifact publish did not reach CLI dispatcher: %v\n%s", err, output)
	}
}
