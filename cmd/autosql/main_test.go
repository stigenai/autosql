package main

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestLocalReadCommandsKeepTrustedPlanSubcommands(t *testing.T) {
	for _, args := range [][]string{{"config", "validate"}, {"schema", "load"}, {"plan"}, {"plan", "--config", "project.hcl"}, {"migrate", "generate"}} {
		if !localReadCommand(args) {
			t.Fatalf("local command initialized production services: %v", args)
		}
	}
	for _, args := range [][]string{nil, {"apply"}, {"plan", "edit"}, {"plan", "revalidate"}, {"plan", "publish"}, {"operator", "artifact", "publish"}} {
		if localReadCommand(args) {
			t.Fatalf("trusted command bypassed service initialization: %v", args)
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
	t.Run("configuration validation ignores production setup", func(t *testing.T) {
		project := filepath.Join(directory, "project.hcl")
		body := `version=2
environment="prod"
environment "prod" {
 classification="production"
 target="file:///this-secret-must-not-be-opened"
 schema_sources=[{kind="hcl",path="missing-schema.hcl"}]
 migration_dir="missing-migrations"
}`
		if err := os.WriteFile(project, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		validate := exec.CommandContext(ctx, binary, "config", "validate", "--config", project, "--json")
		validate.Env = append(os.Environ(), "AUTOSQL_APPLY_CONFIG="+filepath.Join(directory, "missing-production-apply.json"))
		output, err := validate.CombinedOutput()
		if err != nil || !strings.Contains(string(output), `"ok":true`) || !strings.Contains(string(output), `"effective":`) {
			t.Fatalf("validation initialized unrelated runtime: %v\n%s", err, output)
		}
	})
	t.Run("development lifecycle ignores production setup", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "database", "dev", "cleanup", "--lease", filepath.Join(directory, "missing", "lease.json"), "--json")
		command.Env = append(os.Environ(), "AUTOSQL_APPLY_CONFIG="+filepath.Join(directory, "missing-production-apply.json"))
		output, err := command.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 8 || !strings.Contains(string(output), `"command":"database dev cleanup"`) || strings.Contains(string(output), "production apply") {
			t.Fatalf("development command initialized production services: %v %s", err, output)
		}
	})
	t.Run("generation owns its trusted configuration", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "migrate", "generate", "--config", "project.json", "--version", "1", "--label", "init", "--format", "sql", "--generation-config", filepath.Join(directory, "missing-generation.json"), "--json")
		command.Env = append(os.Environ(), "AUTOSQL_APPLY_CONFIG="+filepath.Join(directory, "missing-production-apply.json"))
		output, err := command.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 3 || !strings.Contains(string(output), "read generation configuration") || strings.Contains(string(output), "read apply configuration") {
			t.Fatalf("generation initialized unrelated Apply configuration: %v\n%s", err, output)
		}
	})
	t.Run("test commands ignore production setup", func(t *testing.T) {
		for _, kind := range []string{"schema", "migrate"} {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			command := exec.CommandContext(ctx, binary, kind, "test", "--help", "--json")
			command.Env = append(os.Environ(), "AUTOSQL_APPLY_CONFIG="+filepath.Join(directory, "missing-production-apply.json"))
			output, err := command.CombinedOutput()
			cancel()
			if err != nil || !strings.Contains(string(output), `"ok":true`) || !strings.Contains(string(output), "--junit") {
				t.Fatalf("test help initialized production services: %v %s", err, output)
			}
		}
	})
	t.Run("schema loading ignores production setup", func(t *testing.T) {
		project := filepath.Join(directory, "source-project.json")
		source := filepath.Join(directory, "schema.sql")
		body := `{"version":2,"environment":"prod","environments":{"prod":{"target":"file:///this-secret-must-not-be-opened","schema_sources":[{"kind":"sql","path":"schema.sql"}],"migration_dir":"missing-migrations"}}}`
		if err := os.WriteFile(project, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte("CREATE SCHEMA demo;"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, inputs := range [][]string{{"--config", project}, {"--source", "sql:" + source}} {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			args := append([]string{"schema", "load", "--json"}, inputs...)
			load := exec.CommandContext(ctx, binary, args...)
			load.Env = append(os.Environ(), "AUTOSQL_APPLY_CONFIG="+filepath.Join(directory, "missing-production-apply.json"))
			output, err := load.CombinedOutput()
			if err != nil || !strings.Contains(string(output), `"ok":true`) || !strings.Contains(string(output), "demo") {
				t.Fatalf("schema loading initialized unrelated runtime: %v\n%s", err, output)
			}
		}
		current := filepath.Join(directory, "empty.sql")
		if err := os.WriteFile(current, nil, 0600); err != nil {
			t.Fatal(err)
		}
		for _, inputs := range [][]string{{"--config", project, "--from", "sql:empty.sql"}, {"--from", "sql:" + current, "--to", "sql:" + source}} {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, append([]string{"plan", "--json"}, inputs...)...)
			command.Env = append(os.Environ(), "AUTOSQL_APPLY_CONFIG="+filepath.Join(directory, "missing-production-apply.json"))
			output, err := command.CombinedOutput()
			if err != nil || !strings.Contains(string(output), `"ok":true`) || !strings.Contains(string(output), "CREATE SCHEMA") {
				t.Fatalf("planning initialized unrelated runtime: %v\n%s", err, output)
			}
		}
	})

	help := exec.Command(binary, "operator", "--help")
	helpOutput, helpErr := help.CombinedOutput()
	if helpErr != nil {
		t.Fatalf("operator help: %v", helpErr)
	}
	for _, flag := range []string{"leader-election", "metrics-bind-address", "health-probe-bind-address", ":8080", ":8081", "0 disables"} {
		if !strings.Contains(string(helpOutput), flag) {
			t.Fatalf("operator help missing %s", flag)
		}
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
