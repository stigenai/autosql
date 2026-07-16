package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"autosql/pkg/source"

	"github.com/jackc/pgx/v5"
)

func TestExtensionPreflightAggregatesPolicyAndAvailability(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	pgcrypto := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "pgcrypto", Parent: ns.ID}, `{"version":"1.3","relocatable":true,"trusted":true,"superuser":true,"requires":[],"owner":"postgres"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	hstore := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "hstore", Parent: ns.ID}, `{"version":"9.9","relocatable":true,"trusted":true,"superuser":true,"requires":[],"owner":"postgres"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}, Graph: schema.Graph{Resources: []schema.Resource{ns, pgcrypto, hstore}}}
	catalog := ExtensionCatalog{Versions: []ExtensionAvailability{
		{Name: "pgcrypto", Version: "1.3", Relocatable: true, Superuser: true, Trusted: false, Requires: []string{"plpgsql"}},
		{Name: "hstore", Version: "1.8", Schema: "public", Superuser: true, Trusted: true},
	}}
	policy := ExtensionPolicy{
		Allowed:        map[string]bool{"pgcrypto": true},
		Versions:       map[string][]string{"pgcrypto": {"1.2"}, "hstore": {"1.8"}},
		Schemas:        map[string][]string{"pgcrypto": {"public"}, "hstore": {"public"}},
		RequireTrusted: true,
	}
	report, err := PreflightExtensions(doc, catalog, policy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Supported {
		t.Fatal("unsupported extension inventory passed preflight")
	}
	got := make([]string, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
		got = append(got, diagnostic.Name+":"+diagnostic.Class)
	}
	want := []string{"hstore:allowlist", "hstore:package", "pgcrypto:authority", "pgcrypto:dependency", "pgcrypto:schema", "pgcrypto:version"}
	if !slices.Equal(got, want) {
		t.Fatalf("diagnostics=%v want=%v", got, want)
	}
}

func TestExtensionReadinessClassifiesEveryRequestedExtensionDeterministically(t *testing.T) {
	makeDocument := func(name, version, namespace string) schema.Document {
		ns := renderResource(schema.KindSchema, schema.Name{Name: namespace}, `{}`)
		extension := renderResource(schema.KindExtension, schema.Name{Schema: namespace, Name: name, Parent: ns.ID}, fmt.Sprintf(`{"version":%q,"relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, version), schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
		return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, extension}}}
	}
	base := ExtensionCatalog{
		Versions:  []ExtensionAvailability{{Name: "demo", Version: "1.0", Relocatable: true, Trusted: true, Superuser: true}},
		Paths:     []ExtensionUpdatePath{{Name: "demo", Source: "0.9", Target: "1.0", Path: "0.9--1.0"}},
		Privilege: ExtensionPrivilegeContext{ServerMajor: 18, CurrentUser: "owner", CurrentDatabase: "cell", DatabaseOwner: true, CanCreateDatabase: true},
	}
	policy := ExtensionPolicy{Allowed: map[string]bool{"demo": true}, Versions: map[string][]string{"demo": {"1.0"}}, Schemas: map[string][]string{"demo": {"app"}}}
	tests := []struct {
		name    string
		doc     schema.Document
		catalog ExtensionCatalog
		policy  ExtensionPolicy
		want    ExtensionReadinessStatus
	}{
		{"ready trusted owner", makeDocument("demo", "1.0", "app"), base, policy, ExtensionReady},
		{"missing package", makeDocument("missing", "1.0", "app"), base, ExtensionPolicy{Allowed: map[string]bool{"missing": true}, Versions: map[string][]string{"missing": {"1.0"}}, Schemas: map[string][]string{"missing": {"app"}}}, ExtensionMissingPackage},
		{"unavailable version", makeDocument("demo", "2.0", "app"), base, ExtensionPolicy{Allowed: map[string]bool{"demo": true}, Versions: map[string][]string{"demo": {"2.0"}}, Schemas: map[string][]string{"demo": {"app"}}}, ExtensionUnavailableVersion},
		{"fixed schema conflict", makeDocument("demo", "1.0", "app"), ExtensionCatalog{Versions: []ExtensionAvailability{{Name: "demo", Version: "1.0", Schema: "public", Trusted: true, Superuser: true}}, Privilege: base.Privilege}, policy, ExtensionSchemaConflicted},
		{"privilege blocked untrusted", makeDocument("demo", "1.0", "app"), ExtensionCatalog{Versions: []ExtensionAvailability{{Name: "demo", Version: "1.0", Relocatable: true, Superuser: true}}, Privilege: base.Privilege}, ExtensionPolicy{Allowed: policy.Allowed, Versions: policy.Versions, Schemas: policy.Schemas, AllowUntrusted: true}, ExtensionPrivilegeBlocked},
		{"unauthorized allowlist", makeDocument("demo", "1.0", "app"), base, ExtensionPolicy{Allowed: map[string]bool{}, Versions: policy.Versions, Schemas: policy.Schemas}, ExtensionUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := EvaluateExtensionReadiness(test.doc, test.catalog, test.policy)
			second := EvaluateExtensionReadiness(test.doc, test.catalog, test.policy)
			if len(first.Extensions) != 1 || first.Extensions[0].Status != test.want || first.Extensions[0].Remediation == "" {
				t.Fatalf("report=%+v want=%s", first, test.want)
			}
			firstJSON, _ := json.Marshal(first)
			secondJSON, _ := json.Marshal(second)
			if string(firstJSON) != string(secondJSON) || strings.Contains(string(firstJSON), "postgres://") || strings.Contains(string(firstJSON), "password") {
				t.Fatalf("readiness is nondeterministic or leaked a secret: %s != %s", firstJSON, secondJSON)
			}
		})
	}
}

func TestExtensionReadinessInstalledAndTrustedPrivilegeSemantics(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "demo", Parent: ns.ID}, `{"version":"1.0","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, extension}}}
	policy := ExtensionPolicy{Allowed: map[string]bool{"demo": true}, Versions: map[string][]string{"demo": {"1.0"}}, Schemas: map[string][]string{"demo": {"app"}}}
	for major := 14; major <= 18; major++ {
		catalog := ExtensionCatalog{Versions: []ExtensionAvailability{{Name: "demo", Version: "1.0", Relocatable: true, Trusted: true, Superuser: true}}, Privilege: ExtensionPrivilegeContext{ServerMajor: major, CurrentUser: "owner", DatabaseOwner: true, CanCreateDatabase: true}}
		if got := EvaluateExtensionReadiness(doc, catalog, policy); !got.Ready || got.Extensions[0].SuperuserRequired {
			t.Fatalf("PostgreSQL %d trusted extension report=%+v", major, got)
		}
		catalog.Installed = []ExtensionInstallation{{Name: "demo", Version: "1.0", Schema: "app", Owner: "other"}}
		catalog.Privilege = ExtensionPrivilegeContext{ServerMajor: major, CurrentUser: "reader"}
		if got := EvaluateExtensionReadiness(doc, catalog, policy); !got.Ready {
			t.Fatalf("PostgreSQL %d no-op installed extension required mutation privilege: %+v", major, got)
		}
	}
}

func TestExtensionReadinessUsesOperationSpecificPrivileges(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "destination"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: "destination", Name: "demo", Parent: ns.ID}, `{"version":"2.0","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, extension}}}
	policy := ExtensionPolicy{Allowed: map[string]bool{"demo": true}, Versions: map[string][]string{"demo": {"2.0"}}, Schemas: map[string][]string{"demo": {"destination"}}}
	base := ExtensionCatalog{
		Versions:  []ExtensionAvailability{{Name: "demo", Version: "2.0", Relocatable: true, Trusted: true, Superuser: true}},
		Paths:     []ExtensionUpdatePath{{Name: "demo", Source: "1.0", Target: "2.0", Path: "1.0--2.0"}},
		Schemas:   []ExtensionSchemaPrivilege{{Name: "destination", CanCreate: true}},
		Privilege: ExtensionPrivilegeContext{ServerMajor: 18, CurrentUser: "extension_owner", CurrentDatabase: "cell"},
	}

	update := base
	update.Installed = []ExtensionInstallation{{Name: "demo", Version: "1.0", Schema: "destination", Owner: "owner_group", OwnerUsable: true, MemberObjectsOwned: true}}
	if got := EvaluateExtensionReadiness(doc, update, policy); !got.Ready {
		t.Fatalf("extension owner update was incorrectly blocked by database CREATE: %+v", got)
	}

	move := base
	move.Installed = []ExtensionInstallation{{Name: "demo", Version: "2.0", Schema: "source", Owner: "owner_group", OwnerUsable: true, MemberObjectsOwned: true}}
	if got := EvaluateExtensionReadiness(doc, move, policy); !got.Ready {
		t.Fatalf("owned relocation with destination CREATE was incorrectly blocked: %+v", got)
	}
	combined := base
	combined.Installed = []ExtensionInstallation{{Name: "demo", Version: "1.0", Schema: "source", Owner: "owner_group", OwnerUsable: true, MemberObjectsOwned: true}}
	if got := EvaluateExtensionReadiness(doc, combined, policy); got.Extensions[0].Status != ExtensionPrivilegeBlocked || !strings.Contains(got.Extensions[0].Reason, "update may add") {
		t.Fatalf("trusted update plus move was not conservatively split: %+v", got)
	}
	move.Installed[0].MemberObjectsOwned = false
	if got := EvaluateExtensionReadiness(doc, move, policy); got.Extensions[0].Status != ExtensionPrivilegeBlocked || !strings.Contains(got.Extensions[0].Reason, "member object") {
		t.Fatalf("member ownership relocation report=%+v", got)
	}
	move.Installed[0].MemberObjectsOwned = true
	move.Schemas = nil
	move.Privilege.CanCreateDatabase = true
	if got := EvaluateExtensionReadiness(doc, move, policy); got.Extensions[0].Status != ExtensionSchemaConflicted {
		t.Fatalf("planned destination incorrectly substituted database CREATE for actual schema: %+v", got)
	}
}

func TestExtensionRequirementsRoundTripHCL(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	first := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "pgcrypto", Parent: ns.ID}, `{"version":"1.3","relocatable":true,"trusted":true,"superuser":true,"requires":[],"owner":"postgres"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	first.Annotations = map[string]string{"comment": "cryptographic functions"}
	second := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "uuid-ossp", Parent: ns.ID}, `{"version":"1.1","relocatable":true,"trusted":true,"superuser":true,"requires":["plpgsql"],"owner":"postgres"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}, Graph: schema.Graph{Resources: []schema.Resource{ns, first, second}}}
	hcl, err := source.FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := source.LoadContext(context.Background(), source.Input{URI: "extensions.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := schema.Diff(doc, reloaded, schema.DiffOptions{})
	if err != nil || len(diff.Changes) != 0 {
		t.Fatalf("extension HCL round trip drifted: changes=%d err=%v\n%s", len(diff.Changes), err, hcl)
	}
}

func TestValidateExtensionTransitionUsesServerAdvertisedPath(t *testing.T) {
	catalog := ExtensionCatalog{Paths: []ExtensionUpdatePath{{Name: "hstore", Source: "1.7", Target: "1.8", Path: "1.7--1.8"}}}
	if err := ValidateExtensionTransition("hstore", "1.7", "1.8", catalog); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExtensionTransition("hstore", "1.8", "1.7", catalog); err == nil {
		t.Fatal("unavailable downgrade path passed")
	}
}

func TestInspectExtensionCatalogURL(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	catalog, err := InspectExtensionCatalogURL(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, version := range catalog.Versions {
		if version.Name == "pgcrypto" && version.Version != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("server catalog did not expose pgcrypto")
	}
}

func TestExtensionReadinessURLIsReadOnlyAndVersionAware(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var owner string
	var beforeExtensions, beforeSchemas int
	if err := conn.QueryRow(ctx, `select pg_get_userbyid(datdba),(select count(*) from pg_extension),(select count(*) from pg_namespace) from pg_database where datname=current_database()`).Scan(&owner, &beforeExtensions, &beforeSchemas); err != nil {
		t.Fatal(err)
	}
	catalog, err := InspectExtensionCatalogURL(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	for _, available := range catalog.Versions {
		if available.Name == "pgcrypto" {
			version = available.Version
		}
	}
	if version == "" {
		t.Fatal("pgcrypto control file is unavailable in matrix image")
	}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ExternalDatabase, Endpoint: bootstrap.ServerEndpoint{Host: config.Host, Port: uint16(config.Port), TLSMode: "disable"}, MaintenanceDatabase: config.Database, Name: config.Database, Owner: owner, ConnectionLimit: -1}.Normalize()
	ns := renderResource(schema.KindSchema, schema.Name{Name: "public"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: "public", Name: "pgcrypto", Parent: ns.ID}, fmt.Sprintf(`{"version":%q,"relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, version), schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, extension}}}
	policy := ExtensionPolicy{Allowed: map[string]bool{"pgcrypto": true}, Versions: map[string][]string{"pgcrypto": {version}}, Schemas: map[string][]string{"pgcrypto": {"public"}}, AllowUntrusted: true}
	report, err := PreflightExtensionReadinessURL(ctx, url, target, doc, policy)
	if err != nil || !report.Ready || len(report.Extensions) != 1 || report.Extensions[0].Status != ExtensionReady {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	var afterExtensions, afterSchemas int
	if err := conn.QueryRow(ctx, `select (select count(*) from pg_extension),(select count(*) from pg_namespace)`).Scan(&afterExtensions, &afterSchemas); err != nil {
		t.Fatal(err)
	}
	if beforeExtensions != afterExtensions || beforeSchemas != afterSchemas {
		t.Fatalf("readiness mutated catalogs: extensions %d->%d schemas %d->%d", beforeExtensions, afterExtensions, beforeSchemas, afterSchemas)
	}
	missing := extension
	missing.Name.Name = "autosql_missing_control_file"
	missing.ID = schema.StableID(missing.Kind, missing.Name)
	missing.Spec = json.RawMessage(`{"version":"1.0","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`)
	missingDoc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, missing}}}
	missingPolicy := ExtensionPolicy{Allowed: map[string]bool{missing.Name.Name: true}, Versions: map[string][]string{missing.Name.Name: {"1.0"}}, Schemas: map[string][]string{missing.Name.Name: {"public"}}}
	missingReport, err := PreflightExtensionReadinessURL(ctx, url, target, missingDoc, missingPolicy)
	if err != nil || missingReport.Extensions[0].Status != ExtensionMissingPackage || !strings.Contains(missingReport.Extensions[0].Remediation, ".control") {
		t.Fatalf("missing report=%+v err=%v", missingReport, err)
	}
}

func TestExtensionReadinessDetectsTrustedMemberOwnershipRelocationBlock(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	role, database, destination := "autosql_ext_owner_"+suffix, "autosql_ext_move_"+suffix, "locked_destination"
	password := "autosql-extension-readiness"
	defer func() {
		_, _ = admin.Exec(context.Background(), `select pg_terminate_backend(pid) from pg_stat_activity where datname=$1`, database)
		_, _ = admin.Exec(context.Background(), "drop database if exists "+pgx.Identifier{database}.Sanitize())
		_, _ = admin.Exec(context.Background(), "drop role if exists "+pgx.Identifier{role}.Sanitize())
	}()
	if _, err := admin.Exec(ctx, "create role "+pgx.Identifier{role}.Sanitize()+" login password '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{database}.Sanitize()+" owner "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.Database, config.User, config.Password = database, role, password
	roleURL := extensionTestRoleURL(t, url, database, role, password)
	member, err := pgx.Connect(ctx, roleURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.Exec(ctx, `create extension hstore with schema public; create schema `+pgx.Identifier{destination}.Sanitize()+` authorization `+pgx.Identifier{role}.Sanitize()); err != nil {
		member.Close(ctx)
		t.Fatal(err)
	}
	catalog, err := InspectExtensionCatalogURL(ctx, roleURL)
	if err != nil {
		member.Close(ctx)
		t.Fatal(err)
	}
	version := ""
	for _, installed := range catalog.Installed {
		if installed.Name == "hstore" {
			version = installed.Version
			if installed.MemberObjectsOwned {
				member.Close(ctx)
				t.Skip("server installed trusted hstore members as the invoking role; no bootstrap-owner disagreement to reproduce")
			}
		}
	}
	if version == "" {
		member.Close(ctx)
		t.Fatal("hstore was not installed")
	}
	ns := renderResource(schema.KindSchema, schema.Name{Name: destination}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: destination, Name: "hstore", Parent: ns.ID}, fmt.Sprintf(`{"version":%q,"relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, version), schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, extension}}}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ExternalDatabase, Endpoint: bootstrap.ServerEndpoint{Host: config.Host, Port: uint16(config.Port), TLSMode: "disable"}, MaintenanceDatabase: database, Name: database, Owner: role, ConnectionLimit: -1}.Normalize()
	policy := ExtensionPolicy{Allowed: map[string]bool{"hstore": true}, Versions: map[string][]string{"hstore": {version}}, Schemas: map[string][]string{"hstore": {destination}}}
	report, err := PreflightExtensionReadinessURL(ctx, roleURL, target, doc, policy)
	if err != nil || len(report.Extensions) != 1 || report.Extensions[0].Status != ExtensionPrivilegeBlocked || !strings.Contains(report.Extensions[0].Reason, "member object") {
		member.Close(ctx)
		t.Fatalf("relocation report=%+v err=%v", report, err)
	}
	_, moveErr := member.Exec(ctx, `alter extension hstore set schema `+pgx.Identifier{destination}.Sanitize())
	member.Close(ctx)
	if moveErr == nil || !strings.Contains(moveErr.Error(), "must be owner") {
		t.Fatalf("expected PostgreSQL member ownership failure, got %v", moveErr)
	}
}

func TestExtensionReadinessAcceptsInheritedOwnerUpdateAndMovePG18(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var major int
	if err := admin.QueryRow(ctx, `select current_setting('server_version_num')::integer/10000`).Scan(&major); err != nil {
		t.Fatal(err)
	}
	if major != 18 {
		t.Skip("PG18 inherited-owner proof")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	ownerRole, memberRole := "autosql_ext_group_"+suffix, "autosql_ext_member_"+suffix
	database, destination, password := "autosql_ext_inherit_"+suffix, "inherited_destination", "autosql-extension-membership"
	ownerPassword := "autosql-extension-owner"
	defer func() {
		_, _ = admin.Exec(context.Background(), `select pg_terminate_backend(pid) from pg_stat_activity where datname=$1`, database)
		_, _ = admin.Exec(context.Background(), "drop database if exists "+pgx.Identifier{database}.Sanitize())
		_, _ = admin.Exec(context.Background(), "drop role if exists "+pgx.Identifier{memberRole}.Sanitize())
		_, _ = admin.Exec(context.Background(), "drop role if exists "+pgx.Identifier{ownerRole}.Sanitize())
	}()
	if _, err := admin.Exec(ctx, "create role "+pgx.Identifier{ownerRole}.Sanitize()+" login password '"+ownerPassword+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "create role "+pgx.Identifier{memberRole}.Sanitize()+" login password '"+password+"' in role "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{database}.Sanitize()+" owner "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	ownerConfig, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	ownerConfig.Database = database
	ownerConfig.User, ownerConfig.Password = ownerRole, ownerPassword
	ownerTarget, err := pgx.ConnectConfig(ctx, ownerConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTarget.Exec(ctx, `create extension hstore with schema public version '1.4'`); err != nil {
		ownerTarget.Close(ctx)
		t.Fatal(err)
	}
	if _, err := ownerTarget.Exec(ctx, `create schema `+pgx.Identifier{destination}.Sanitize()+" authorization "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
		ownerTarget.Close(ctx)
		t.Fatal(err)
	}
	ownerTarget.Close(ctx)
	adminConfig, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.Database = database
	adminTarget, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	// Trusted installation intentionally assigns member objects to the
	// bootstrap superuser. Rebind only this extension's ownable members to the
	// extension owner to model a reviewed ownership handoff, then prove that a
	// member of that owner role can update and relocate it.
	ownershipStatements := []string{
		`update pg_extension set extowner=(select oid from pg_roles where rolname=$1) where extname='hstore'`,
		`update pg_type set typowner=(select oid from pg_roles where rolname=$1) where oid in (select objid from pg_depend where refclassid='pg_extension'::regclass and refobjid=(select oid from pg_extension where extname='hstore') and classid='pg_type'::regclass and deptype='e')`,
		`update pg_proc set proowner=(select oid from pg_roles where rolname=$1) where oid in (select objid from pg_depend where refclassid='pg_extension'::regclass and refobjid=(select oid from pg_extension where extname='hstore') and classid='pg_proc'::regclass and deptype='e')`,
		`update pg_operator set oprowner=(select oid from pg_roles where rolname=$1) where oid in (select objid from pg_depend where refclassid='pg_extension'::regclass and refobjid=(select oid from pg_extension where extname='hstore') and classid='pg_operator'::regclass and deptype='e')`,
		`update pg_opclass set opcowner=(select oid from pg_roles where rolname=$1) where oid in (select objid from pg_depend where refclassid='pg_extension'::regclass and refobjid=(select oid from pg_extension where extname='hstore') and classid='pg_opclass'::regclass and deptype='e')`,
		`update pg_opfamily set opfowner=(select oid from pg_roles where rolname=$1) where oid in (select objid from pg_depend where refclassid='pg_extension'::regclass and refobjid=(select oid from pg_extension where extname='hstore') and classid='pg_opfamily'::regclass and deptype='e')`,
	}
	for _, statement := range ownershipStatements {
		if _, err := adminTarget.Exec(ctx, statement, ownerRole); err != nil {
			adminTarget.Close(ctx)
			t.Fatal(err)
		}
	}
	adminTarget.Close(ctx)
	memberConfig, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	memberConfig.Database, memberConfig.User, memberConfig.Password = database, memberRole, password
	memberURL := extensionTestRoleURL(t, url, database, memberRole, password)
	catalog, err := InspectExtensionCatalogURL(ctx, memberURL)
	if err != nil {
		t.Fatal(err)
	}
	installed := ExtensionInstallation{}
	for _, item := range catalog.Installed {
		if item.Name == "hstore" {
			installed = item
		}
	}
	if installed.Version != "1.4" || !installed.OwnerUsable || !installed.MemberObjectsOwned {
		t.Fatalf("inherited catalog ownership=%+v all=%+v", installed, catalog.Installed)
	}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ExternalDatabase, Endpoint: bootstrap.ServerEndpoint{Host: memberConfig.Host, Port: uint16(memberConfig.Port), TLSMode: "disable"}, MaintenanceDatabase: database, Name: database, Owner: ownerRole, ConnectionLimit: -1}.Normalize()
	publicNS := renderResource(schema.KindSchema, schema.Name{Name: "public"}, `{}`)
	updateExtension := renderResource(schema.KindExtension, schema.Name{Schema: "public", Name: "hstore", Parent: publicNS.ID}, `{"version":"1.8","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, schema.Dependency{Target: publicNS.ID, Type: schema.DependencyContains})
	updateDoc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{publicNS, updateExtension}}}
	updatePolicy := ExtensionPolicy{Allowed: map[string]bool{"hstore": true}, Versions: map[string][]string{"hstore": {"1.8"}}, Schemas: map[string][]string{"hstore": {"public"}}}
	report, err := PreflightExtensionReadinessURL(ctx, memberURL, target, updateDoc, updatePolicy)
	if err != nil || !report.Ready {
		t.Fatalf("inherited owner update readiness=%+v err=%v", report, err)
	}
	member, err := pgx.Connect(ctx, memberURL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = member.Exec(ctx, "set role "+pgx.Identifier{ownerRole}.Sanitize()+`; alter extension hstore update to '1.8'; reset role`)
	if err != nil {
		member.Close(ctx)
		t.Fatalf("PostgreSQL rejected inherited-owner update after ready preflight: %v", err)
	}
	adminTarget, err = pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		member.Close(ctx)
		t.Fatal(err)
	}
	for _, statement := range ownershipStatements {
		if _, err := adminTarget.Exec(ctx, statement, ownerRole); err != nil {
			adminTarget.Close(ctx)
			member.Close(ctx)
			t.Fatal(err)
		}
	}
	adminTarget.Close(ctx)
	destinationNS := renderResource(schema.KindSchema, schema.Name{Name: destination}, `{}`)
	moveExtension := renderResource(schema.KindExtension, schema.Name{Schema: destination, Name: "hstore", Parent: destinationNS.ID}, `{"version":"1.8","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, schema.Dependency{Target: destinationNS.ID, Type: schema.DependencyContains})
	moveDoc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{destinationNS, moveExtension}}}
	movePolicy := ExtensionPolicy{Allowed: map[string]bool{"hstore": true}, Versions: map[string][]string{"hstore": {"1.8"}}, Schemas: map[string][]string{"hstore": {destination}}}
	report, err = PreflightExtensionReadinessURL(ctx, memberURL, target, moveDoc, movePolicy)
	if err != nil || !report.Ready {
		member.Close(ctx)
		t.Fatalf("inherited owner move readiness=%+v err=%v", report, err)
	}
	_, err = member.Exec(ctx, "set role "+pgx.Identifier{ownerRole}.Sanitize()+`; alter extension hstore set schema `+pgx.Identifier{destination}.Sanitize()+`; reset role`)
	member.Close(ctx)
	if err != nil {
		t.Fatalf("PostgreSQL rejected inherited-owner move after ready preflight: %v", err)
	}
}

func extensionTestRoleURL(t *testing.T, raw, database, user, password string) string {
	t.Helper()
	parsed, err := neturl.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		t.Fatalf("test PostgreSQL URL must use URI form: %v", err)
	}
	parsed.User = neturl.UserPassword(user, password)
	parsed.Path = "/" + database
	return parsed.String()
}

func TestExtensionCreateUpdateSchemaMoveAndGuardedDrop(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, `drop extension if exists hstore cascade; drop schema if exists autosql_extension_a cascade; drop schema if exists autosql_extension_b cascade`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop extension if exists hstore cascade; drop schema if exists autosql_extension_a cascade; drop schema if exists autosql_extension_b cascade`)

	nsA := renderResource(schema.KindSchema, schema.Name{Name: "autosql_extension_a"}, `{}`)
	nsB := renderResource(schema.KindSchema, schema.Name{Name: "autosql_extension_b"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: nsA.Name.Name, Name: "hstore", Parent: nsA.ID}, `{"version":"1.7","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, schema.Dependency{Target: nsA.ID, Type: schema.DependencyContains})
	extension.Annotations = map[string]string{"comment": "managed hstore"}
	current := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}}
	desired := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}, Graph: schema.Graph{Resources: []schema.Resource{nsA, nsB, extension}}}
	desired.Normalize()
	options := map[string]string{"extension_allowlist": "hstore", "extension_version.hstore": "1.7", "extension_schemas.hstore": "autosql_extension_a,autosql_extension_b"}
	applyExtensionTransition(t, ctx, conn, current, desired, schema.DiffOptions{}, options)
	assertInstalledExtension(t, ctx, conn, "hstore", "1.7", "autosql_extension_a")

	current = desired
	updated := desired
	updated.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
	for index := range updated.Graph.Resources {
		if updated.Graph.Resources[index].Kind == schema.KindExtension {
			updated.Graph.Resources[index].Spec = []byte(`{"version":"1.8","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`)
		}
	}
	options["extension_version.hstore"] = "1.8"
	applyExtensionTransition(t, ctx, conn, current, updated, schema.DiffOptions{}, options)
	assertInstalledExtension(t, ctx, conn, "hstore", "1.8", "autosql_extension_a")

	current = updated
	moved := updated
	moved.Graph.Resources = append([]schema.Resource(nil), updated.Graph.Resources...)
	var oldID, newID string
	for index := range moved.Graph.Resources {
		if moved.Graph.Resources[index].Kind != schema.KindExtension {
			continue
		}
		oldID = moved.Graph.Resources[index].ID
		moved.Graph.Resources[index].Name.Schema = nsB.Name.Name
		moved.Graph.Resources[index].Name.Parent = nsB.ID
		moved.Graph.Resources[index].Dependencies = []schema.Dependency{{Target: nsB.ID, Type: schema.DependencyContains}}
		moved.Graph.Resources[index].ID = schema.StableID(schema.KindExtension, moved.Graph.Resources[index].Name)
		newID = moved.Graph.Resources[index].ID
	}
	moved.Normalize()
	applyExtensionTransition(t, ctx, conn, current, moved, schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldID, To: newID}}}, options)
	assertInstalledExtension(t, ctx, conn, "hstore", "1.8", "autosql_extension_b")

	withoutExtension := moved
	withoutExtension.Graph.Resources = nil
	for _, resource := range moved.Graph.Resources {
		if resource.Kind != schema.KindExtension {
			withoutExtension.Graph.Resources = append(withoutExtension.Graph.Resources, resource)
		}
	}
	if _, err := renderExtensionTransition(ctx, moved, withoutExtension, schema.DiffOptions{}, options); err == nil || !strings.Contains(err.Error(), "allow_extension_drop") {
		t.Fatalf("unguarded extension drop err=%v", err)
	}
	options["allow_extension_drop"] = "true"
	applyExtensionTransition(t, ctx, conn, moved, withoutExtension, schema.DiffOptions{}, options)
	var installed bool
	if err := conn.QueryRow(ctx, `select exists(select 1 from pg_extension where extname='hstore')`).Scan(&installed); err != nil || installed {
		t.Fatalf("installed=%v err=%v", installed, err)
	}
}

func TestExtensionOwnedMembersDoNotProduceDuplicateLifecycle(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, `drop extension if exists pgcrypto cascade; drop schema if exists autosql_extension_members cascade; create schema autosql_extension_members; create extension pgcrypto with schema autosql_extension_members; create table autosql_extension_members.tokens(id uuid default autosql_extension_members.gen_random_uuid())`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop extension if exists pgcrypto cascade; drop schema if exists autosql_extension_members cascade`)

	doc, err := InspectURL(ctx, url, Options{Schemas: []string{"autosql_extension_members"}})
	if err != nil {
		t.Fatal(err)
	}
	resources := resourceMapForRender(doc)
	var extension schema.Resource
	ownedRoutines := 0
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindExtension && resource.Name.Name == "pgcrypto" {
			extension = resource
		}
		if (resource.Kind == schema.KindFunction || resource.Kind == schema.KindProcedure) && stringValue(spec(resource), "extension") == "pgcrypto" {
			ownedRoutines++
			if extensionOwnerID(resource, resources) == "" {
				t.Fatalf("retained member lacks extension ownership: %s", resource.Name.String())
			}
		}
	}
	if extension.ID == "" {
		t.Fatal("pgcrypto extension missing from inspection")
	}
	// pgcrypto creates many functions; only an application-referenced member
	// may remain in the graph as an inert prerequisite.
	if ownedRoutines > 1 {
		t.Fatalf("unreferenced extension members leaked into graph: %d", ownedRoutines)
	}

	hcl, err := source.FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := source.LoadContext(ctx, source.Input{URI: "extension-members.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := schema.Diff(doc, reloaded, schema.DiffOptions{})
	if err != nil || len(diff.Changes) != 0 {
		t.Fatalf("extension member HCL drifted: changes=%d err=%v", len(diff.Changes), err)
	}

	if ownedRoutines == 1 {
		mutated := doc
		mutated.Graph.Resources = append([]schema.Resource(nil), doc.Graph.Resources...)
		for index := range mutated.Graph.Resources {
			resource := &mutated.Graph.Resources[index]
			if stringValue(spec(*resource), "extension") != "pgcrypto" {
				continue
			}
			values := spec(*resource)
			values["definition"] = stringValue(values, "definition") + " "
			resource.Spec, _ = json.Marshal(values)
		}
		changes, err := schema.Diff(doc, mutated, schema.DiffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		statements, renderErr := New().Render(ctx, plugin.RenderRequest{Changes: changes, Current: doc, Desired: mutated})
		if renderErr == nil || len(statements) != 0 || !strings.Contains(renderErr.Error(), "extension-owned resource") {
			t.Fatalf("independent member change statements=%v err=%v", statements, renderErr)
		}
	}
}

func TestExtensionOwnedMemberChangesRequireOwningExtensionTransition(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "hstore", Parent: ns.ID}, `{"version":"1.7","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	function := renderResource(schema.KindFunction, schema.Name{Schema: "app", Name: "member(text)", Parent: ns.ID}, `{"name":"member","identity_arguments":"text","arguments":"text","result":"text","language":"sql","volatility":"i","strict":false,"security_definer":false,"leakproof":false,"parallel":"s","cost":1,"rows":0,"configuration":[],"definition":"select $1","body_digest":"one","extension":"hstore"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains}, schema.Dependency{Target: extension.ID, Type: schema.DependencyOwns})
	current := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}, Graph: schema.Graph{Resources: []schema.Resource{ns, extension, function}}}
	desired := current
	desired.Graph.Resources = append([]schema.Resource(nil), current.Graph.Resources...)
	for index := range desired.Graph.Resources {
		if desired.Graph.Resources[index].Kind == schema.KindFunction {
			desired.Graph.Resources[index].Spec = []byte(`{"name":"member","identity_arguments":"text","arguments":"text","result":"text","language":"sql","volatility":"i","strict":false,"security_definer":false,"leakproof":false,"parallel":"s","cost":1,"rows":0,"configuration":[],"definition":"select upper($1)","body_digest":"two","extension":"hstore"}`)
		}
	}
	changes, err := schema.Diff(current, desired, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if statements, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired}); err == nil || len(statements) != 0 {
		t.Fatalf("independent member statements=%v err=%v", statements, err)
	}

	for index := range desired.Graph.Resources {
		if desired.Graph.Resources[index].Kind == schema.KindExtension {
			desired.Graph.Resources[index].Spec = []byte(`{"version":"1.8","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`)
		}
	}
	changes, err = schema.Diff(current, desired, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	statements, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired, Options: map[string]string{"extension_allowlist": "hstore", "extension_version.hstore": "1.8", "extension_schemas.hstore": "app"}})
	if err != nil {
		t.Fatal(err)
	}
	executable, topology := 0, 0
	for _, statement := range statements {
		if statement.Kind == plugin.StatementTopology {
			topology++
		} else if strings.Contains(statement.SQL, `ALTER EXTENSION "hstore" UPDATE TO '1.8'`) {
			executable++
		}
	}
	if executable != 1 || topology != 1 {
		t.Fatalf("statements=%+v", statements)
	}
}

func renderExtensionTransition(ctx context.Context, current, desired schema.Document, diffOptions schema.DiffOptions, options map[string]string) ([]plugin.Statement, error) {
	changes, err := schema.Diff(current, desired, diffOptions)
	if err != nil {
		return nil, err
	}
	return New().Render(ctx, plugin.RenderRequest{Changes: changes, Current: current, Desired: desired, Options: options})
}

func applyExtensionTransition(t *testing.T, ctx context.Context, conn *pgx.Conn, current, desired schema.Document, diffOptions schema.DiffOptions, options map[string]string) {
	t.Helper()
	statements, err := renderExtensionTransition(ctx, current, desired, diffOptions, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if statement.Kind == plugin.StatementTopology {
			continue
		}
		if _, err := conn.Exec(ctx, statement.SQL); err != nil {
			t.Fatalf("%s: %v", statement.SQL, err)
		}
	}
}

func assertInstalledExtension(t *testing.T, ctx context.Context, conn *pgx.Conn, name, version, namespace string) {
	t.Helper()
	var gotVersion, gotSchema string
	err := conn.QueryRow(ctx, `select e.extversion,n.nspname from pg_extension e join pg_namespace n on n.oid=e.extnamespace where e.extname=$1`, name).Scan(&gotVersion, &gotSchema)
	if err != nil || gotVersion != version || gotSchema != namespace {
		t.Fatalf("extension %s version=%s schema=%s err=%v", name, gotVersion, gotSchema, err)
	}
}
