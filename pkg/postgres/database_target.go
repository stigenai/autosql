package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"autosql/pkg/bootstrap"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
)

type DatabaseTargetDiagnostic struct {
	Class, Field, Message string
}

type DatabaseTargetReport struct {
	Supported   bool
	Exists      bool
	ServerMajor int
	Diagnostics []DatabaseTargetDiagnostic
}

type PreparedDatabase struct {
	Connection *pgx.Conn
	Target     bootstrap.DatabaseTarget
	Created    bool
}

// DatabaseTargetFromResource decodes the direct HCL/native database resource
// into the out-of-band cluster target contract.
func DatabaseTargetFromResource(resource schema.Resource) (bootstrap.DatabaseTarget, error) {
	if resource.Kind != schema.KindDatabase || resource.Name.Name == "" {
		return bootstrap.DatabaseTarget{}, errors.New("database target requires one database resource")
	}
	var target bootstrap.DatabaseTarget
	if err := json.Unmarshal(resource.Spec, &target); err != nil {
		return bootstrap.DatabaseTarget{}, fmt.Errorf("decode database target: %w", err)
	}
	target.Name = resource.Name.Name
	target = target.Normalize()
	if err := target.Validate(); err != nil {
		return bootstrap.DatabaseTarget{}, err
	}
	return target, nil
}

// PreflightDatabaseTargetURL validates authority, collisions, prerequisites,
// versioned syntax, and external-target settings before any CREATE DATABASE or
// schema SQL is attempted.
func PreflightDatabaseTargetURL(ctx context.Context, maintenanceURL string, target bootstrap.DatabaseTarget) (DatabaseTargetReport, error) {
	target = target.Normalize()
	if err := target.Validate(); err != nil {
		return DatabaseTargetReport{}, err
	}
	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		return DatabaseTargetReport{}, safeError("preflight database target", maintenanceURL, err)
	}
	if config.Host != target.Endpoint.Host || uint16(config.Port) != target.Endpoint.Port || config.Database != target.MaintenanceDatabase {
		return DatabaseTargetReport{}, errors.New("runtime maintenance connection does not match declared endpoint and maintenance database")
	}
	conn, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		return DatabaseTargetReport{}, safeError("preflight database target", maintenanceURL, err)
	}
	defer conn.Close(context.Background())
	report := DatabaseTargetReport{}
	add := func(class, field, message string) {
		report.Diagnostics = append(report.Diagnostics, DatabaseTargetDiagnostic{Class: class, Field: field, Message: message})
	}
	var version int
	if err := conn.QueryRow(ctx, `select current_setting('server_version_num')::integer`).Scan(&version); err != nil {
		return report, err
	}
	report.ServerMajor = version / 10000
	if target.LocaleProvider != "" && report.ServerMajor < 15 {
		add("version", "locale_provider", "locale provider selection requires PostgreSQL 15 or newer")
	}
	if target.LocaleProvider == "builtin" && report.ServerMajor < 17 {
		add("version", "locale_provider", "builtin locale provider requires PostgreSQL 17 or newer")
	}
	var canCreate bool
	if err := conn.QueryRow(ctx, `select rolsuper or rolcreatedb from pg_roles where rolname=current_user`).Scan(&canCreate); err != nil {
		return report, err
	}
	if target.Mode == bootstrap.ManagedDatabase && !canCreate {
		add("permission", "create_database", "current maintenance identity lacks CREATEDB authority")
	}
	if err := conn.QueryRow(ctx, `select exists(select 1 from pg_database where datname=$1)`, target.Name).Scan(&report.Exists); err != nil {
		return report, err
	}
	if target.Mode == bootstrap.ManagedDatabase && report.Exists {
		add("collision", "name", "managed target database already exists")
	}
	if target.Mode == bootstrap.ExternalDatabase && !report.Exists {
		add("missing", "name", "external target database does not exist")
	}
	for field, prerequisite := range map[string]struct{ query, value string }{
		"owner":      {`select exists(select 1 from pg_roles where rolname=$1)`, target.Owner},
		"template":   {`select exists(select 1 from pg_database where datname=$1)`, target.Template},
		"tablespace": {`select exists(select 1 from pg_tablespace where spcname=$1)`, target.Tablespace},
	} {
		var exists bool
		if err := conn.QueryRow(ctx, prerequisite.query, prerequisite.value).Scan(&exists); err != nil {
			return report, err
		}
		if !exists {
			add("missing", field, field+" prerequisite does not exist")
		}
	}
	if target.Mode == bootstrap.ExternalDatabase && report.Exists {
		for _, diagnostic := range verifyExistingDatabase(ctx, conn, target, report.ServerMajor) {
			report.Diagnostics = append(report.Diagnostics, diagnostic)
		}
	}
	report.Supported = len(report.Diagnostics) == 0
	return report, nil
}

// PrepareDatabaseURL creates a managed database outside a transaction or
// verifies an external database, then returns a session connected to the
// declared target. The maintenance URL is runtime-only and redacted on error.
func PrepareDatabaseURL(ctx context.Context, maintenanceURL string, target bootstrap.DatabaseTarget) (PreparedDatabase, error) {
	target = target.Normalize()
	report, err := PreflightDatabaseTargetURL(ctx, maintenanceURL, target)
	if err != nil {
		return PreparedDatabase{}, err
	}
	if !report.Supported {
		return PreparedDatabase{}, databaseTargetReportError(report)
	}
	maintenance, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		return PreparedDatabase{}, safeError("prepare database target", maintenanceURL, err)
	}
	defer maintenance.Close(context.Background())
	created := false
	if target.Mode == bootstrap.ManagedDatabase {
		statement := renderCreateDatabase(target, report.ServerMajor, true)
		if _, err := maintenance.Exec(ctx, statement); err != nil {
			return PreparedDatabase{}, safeError("create database target", maintenanceURL, err)
		}
		created = true
	}
	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		return PreparedDatabase{}, safeError("connect database target", maintenanceURL, err)
	}
	config.Database = target.Name
	targetConnection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return PreparedDatabase{Target: target, Created: created}, safeError("connect database target", maintenanceURL, err)
	}
	if created && !target.AllowConnections {
		if _, err := maintenance.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{target.Name}.Sanitize()+" ALLOW_CONNECTIONS false"); err != nil {
			targetConnection.Close(context.Background())
			return PreparedDatabase{Target: target, Created: true}, err
		}
	}
	return PreparedDatabase{Connection: targetConnection, Target: target, Created: created}, nil
}

func RenameDatabaseURL(ctx context.Context, maintenanceURL, currentName, desiredName string) error {
	if strings.TrimSpace(currentName) == "" || strings.TrimSpace(desiredName) == "" || currentName == desiredName {
		return errors.New("database rename requires distinct non-empty names")
	}
	conn, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		return safeError("rename database target", maintenanceURL, err)
	}
	defer conn.Close(context.Background())
	var collision bool
	if err := conn.QueryRow(ctx, `select exists(select 1 from pg_database where datname=$1)`, desiredName).Scan(&collision); err != nil {
		return err
	}
	if collision {
		return errors.New("database rename target already exists")
	}
	_, err = conn.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{currentName}.Sanitize()+" RENAME TO "+pgx.Identifier{desiredName}.Sanitize())
	return err
}

func DropDatabaseURL(ctx context.Context, maintenanceURL, name string, force bool) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("database drop requires a name")
	}
	conn, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		return safeError("drop database target", maintenanceURL, err)
	}
	defer conn.Close(context.Background())
	statement := "DROP DATABASE " + pgx.Identifier{name}.Sanitize()
	if force {
		statement += " WITH (FORCE)"
	}
	_, err = conn.Exec(ctx, statement)
	return err
}

func renderCreateDatabase(target bootstrap.DatabaseTarget, serverMajor int, allowConnections bool) string {
	parts := []string{"CREATE DATABASE " + pgx.Identifier{target.Name}.Sanitize(), "WITH OWNER " + pgx.Identifier{target.Owner}.Sanitize(), "TEMPLATE " + pgx.Identifier{target.Template}.Sanitize(), "ENCODING " + sqlString(target.Encoding)}
	if target.LocaleProvider != "" && serverMajor >= 15 {
		parts = append(parts, "LOCALE_PROVIDER "+target.LocaleProvider)
	}
	if target.Collation != "" {
		parts = append(parts, "LC_COLLATE "+sqlString(target.Collation))
	}
	if target.CharacterType != "" {
		parts = append(parts, "LC_CTYPE "+sqlString(target.CharacterType))
	}
	if target.ICULocale != "" {
		parts = append(parts, "ICU_LOCALE "+sqlString(target.ICULocale))
	}
	parts = append(parts, "TABLESPACE "+pgx.Identifier{target.Tablespace}.Sanitize(), "CONNECTION LIMIT "+strconv.Itoa(target.ConnectionLimit), "ALLOW_CONNECTIONS "+strconv.FormatBool(allowConnections))
	return strings.Join(parts, " ")
}

func verifyExistingDatabase(ctx context.Context, conn *pgx.Conn, target bootstrap.DatabaseTarget, serverMajor int) []DatabaseTargetDiagnostic {
	var owner, encoding, collation, characterType, tablespace string
	var connectionLimit int
	var allowConnections bool
	err := conn.QueryRow(ctx, `select pg_get_userbyid(d.datdba),pg_encoding_to_char(d.encoding),d.datcollate,d.datctype,t.spcname,d.datconnlimit,d.datallowconn from pg_database d join pg_tablespace t on t.oid=d.dattablespace where d.datname=$1`, target.Name).Scan(&owner, &encoding, &collation, &characterType, &tablespace, &connectionLimit, &allowConnections)
	if err != nil {
		return []DatabaseTargetDiagnostic{{Class: "inspect", Message: "external database settings could not be inspected"}}
	}
	diagnostics := []DatabaseTargetDiagnostic{}
	check := func(field, actual, desired string, immutable bool) {
		if desired == "" || actual == desired {
			return
		}
		class := "drift"
		if immutable {
			class = "immutable_rebuild"
		}
		diagnostics = append(diagnostics, DatabaseTargetDiagnostic{Class: class, Field: field, Message: "external database setting does not match declared target"})
	}
	check("owner", owner, target.Owner, false)
	check("encoding", strings.ToUpper(encoding), strings.ToUpper(target.Encoding), true)
	check("collation", collation, target.Collation, true)
	check("character_type", characterType, target.CharacterType, true)
	check("tablespace", tablespace, target.Tablespace, false)
	if serverMajor >= 15 && target.LocaleProvider != "" {
		var provider string
		if err := conn.QueryRow(ctx, `select datlocprovider::text from pg_database where datname=$1`, target.Name).Scan(&provider); err != nil {
			diagnostics = append(diagnostics, DatabaseTargetDiagnostic{Class: "inspect", Field: "locale_provider", Message: "external database locale provider could not be inspected"})
		} else {
			providerNames := map[string]string{"c": "libc", "i": "icu", "b": "builtin"}
			check("locale_provider", providerNames[provider], target.LocaleProvider, true)
		}
	}
	if connectionLimit != target.ConnectionLimit {
		diagnostics = append(diagnostics, DatabaseTargetDiagnostic{Class: "drift", Field: "connection_limit", Message: "external database connection limit does not match declared target"})
	}
	if allowConnections != target.AllowConnections {
		diagnostics = append(diagnostics, DatabaseTargetDiagnostic{Class: "drift", Field: "allow_connections", Message: "external database connection policy does not match declared target"})
	}
	return diagnostics
}

func databaseTargetReportError(report DatabaseTargetReport) error {
	problems := make([]error, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
		problems = append(problems, fmt.Errorf("%s %s: %s", diagnostic.Class, diagnostic.Field, diagnostic.Message))
	}
	return errors.Join(problems...)
}

func sqlString(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
