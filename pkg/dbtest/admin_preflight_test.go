package dbtest

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"autosql/pkg/config"
	"autosql/pkg/secret"
)

func testAdminFactory() AdminFactory {
	return AdminFactory{Name: "dev", Classification: "development", Definition: config.DevelopmentFactory{Mode: "existing_admin", Admin: "env://TEST_ADMIN", AdminClassification: "development", PostgresVersion: 17, Allowlist: []string{"postgres://localhost:5432/postgres"}}}
}

func TestAdminFactoryRefusesProductionAndMissingAuthority(t *testing.T) {
	base := testAdminFactory()
	budgets, err := base.validate()
	if err != nil || budgets != (factoryBudgets{2 * time.Minute, 5 * time.Minute, 10 * time.Second}) {
		t.Fatalf("default factory: %+v %v", budgets, err)
	}
	for name, mutate := range map[string]func(*AdminFactory){
		"production environment":   func(f *AdminFactory) { f.Classification = "production" },
		"unclassified environment": func(f *AdminFactory) { f.Classification = "" },
		"production administrator": func(f *AdminFactory) { f.Definition.AdminClassification = "production" },
		"protected reference":      func(f *AdminFactory) { f.ProtectedReferences = []secret.Reference{f.Definition.Admin} },
		"no allowlist":             func(f *AdminFactory) { f.Definition.Allowlist = nil },
		"inline credential":        func(f *AdminFactory) { f.Definition.Admin = "postgres://admin:password@localhost/postgres" },
		"wrong version":            func(f *AdminFactory) { f.Definition.PostgresVersion = 13 },
		"schema isolation":         func(f *AdminFactory) { f.Definition.Isolation = "schema" },
		"zero timeout":             func(f *AdminFactory) { f.Definition.CleanupTimeout = "0s" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			if _, err := candidate.validate(); !errors.Is(err, ErrFactoryConfig) {
				t.Fatal("unsafe factory accepted")
			}
		})
	}
}

func TestAdminFactoryNormalizesAliasesAndProtectsWholeServer(t *testing.T) {
	ctx := context.Background()
	allowed, err := factoryEndpointList(ctx, []string{"postgres://localhost/postgres"})
	if err != nil {
		t.Fatal(err)
	}
	_, endpoint, err := parseFactoryURL("postgresql://admin:secret@127.0.0.1:5432/postgres?sslmode=disable", true)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err = resolveFactoryEndpoint(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := approveFactoryEndpoint(endpoint, allowed, nil); err != nil {
		t.Fatal("localhost alias refused")
	}
	protected, err := factoryEndpointList(ctx, []string{"postgres://localhost/production"})
	if err != nil {
		t.Fatal(err)
	}
	if err := approveFactoryEndpoint(endpoint, allowed, protected); !errors.Is(err, ErrFactoryConfig) {
		t.Fatal("administration DB bypassed protected server alias")
	}
	endpoint.database = "unapproved"
	if err := approveFactoryEndpoint(endpoint, allowed, nil); !errors.Is(err, ErrFactoryConfig) {
		t.Fatal("unlisted database accepted")
	}
	endpoint.database = "postgres"
	endpoint.ips = []netip.Addr{netip.MustParseAddr("192.0.2.10")}
	if err := approveFactoryEndpoint(endpoint, allowed, nil); !errors.Is(err, ErrFactoryConfig) {
		t.Fatal("unapproved resolved endpoint accepted")
	}
}

func TestAdminFactoryURLCannotRedirectApprovedConnection(t *testing.T) {
	for _, raw := range []string{
		"host=localhost dbname=postgres password=secret",
		"postgres://admin:secret@localhost/postgres?host=production",
		"postgres://admin:secret@localhost/postgres?options=-crole%3Dpostgres",
		"postgres://admin:secret@localhost/postgres?sslmode=disable&sslmode=require",
		"postgres://admin:secret@localhost/postgres#production",
		"postgres://admin:secret@localhost/",
		"postgres://admin:secret@localhost/a%2Fb",
	} {
		if _, _, err := parseFactoryURL(raw, true); !errors.Is(err, ErrFactoryConfig) {
			t.Fatal("ambiguous administration URL accepted")
		}
	}
	if _, _, err := parseFactoryURL("postgres://admin:secret@localhost/postgres", false); !errors.Is(err, ErrFactoryConfig) {
		t.Fatal("allowlist embedded a credential")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveFactoryEndpoint(ctx, databaseEndpoint{host: "must-not-resolve.invalid"}); err == nil {
		t.Fatal("canceled identity lookup succeeded")
	}
}

func TestAdminFactoryRefusalDoesNotResolveSecret(t *testing.T) {
	for name, mutate := range map[string]func(*AdminFactory){
		"production":      func(f *AdminFactory) { f.Classification = "production" },
		"protected alias": func(f *AdminFactory) { f.ProtectedReferences = []secret.Reference{"env:/TEST_ADMIN"} },
		"unapproved":      func(f *AdminFactory) { f.Definition.Allowlist = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := testAdminFactory()
			mutate(&f)
			calls := 0
			f.Resolver = &secret.Resolver{Getenv: func(string) (string, bool) { calls++; return "must-not-resolve", true }}
			if _, err := f.Create(context.Background(), "refused"); !errors.Is(err, ErrFactoryConfig) {
				t.Fatalf("expected configuration refusal: %v", err)
			}
			if calls != 0 {
				t.Fatal("refused configuration resolved a secret")
			}
		})
	}
}
