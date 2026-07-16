package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/bootstrap"
	"autosql/pkg/schema"
	"github.com/jackc/pgx/v5"
)

type ExtensionAvailability struct {
	Name, Version, Schema, Comment  string
	Trusted, Superuser, Relocatable bool
	Requires                        []string
}

type ExtensionCatalog struct {
	Versions  []ExtensionAvailability
	Paths     []ExtensionUpdatePath
	Installed []ExtensionInstallation
	Schemas   []ExtensionSchemaPrivilege
	Privilege ExtensionPrivilegeContext
}

type ExtensionPolicy struct {
	Allowed        map[string]bool
	Versions       map[string][]string
	Schemas        map[string][]string
	RequireTrusted bool
	AllowUntrusted bool
	// UntrustedResources carries exact independently authorized resource IDs.
	UntrustedResources map[string]bool
}

type ExtensionInstallation struct {
	Name, Version, Schema, Owner string
	OwnerUsable                  bool
	MemberObjectsOwned           bool
}

type ExtensionSchemaPrivilege struct {
	Name      string
	CanCreate bool
}

type ExtensionPrivilegeContext struct {
	ServerMajor       int
	CurrentUser       string
	CurrentDatabase   string
	Superuser         bool
	DatabaseOwner     bool
	CanCreateDatabase bool
}

type ExtensionReadinessStatus string

const (
	ExtensionReady              ExtensionReadinessStatus = "ready"
	ExtensionMissingPackage     ExtensionReadinessStatus = "missing_package_control_file"
	ExtensionUnavailableVersion ExtensionReadinessStatus = "unavailable_requested_version"
	ExtensionSchemaConflicted   ExtensionReadinessStatus = "schema_conflicted"
	ExtensionPrivilegeBlocked   ExtensionReadinessStatus = "privilege_blocked"
	ExtensionUnauthorized       ExtensionReadinessStatus = "unauthorized"
)

type ExtensionReadiness struct {
	ResourceID         string                   `json:"resource_id"`
	Name               string                   `json:"name"`
	RequestedVersion   string                   `json:"requested_version"`
	RequestedSchema    string                   `json:"requested_schema"`
	Status             ExtensionReadinessStatus `json:"status"`
	InstalledVersion   string                   `json:"installed_version,omitempty"`
	InstalledSchema    string                   `json:"installed_schema,omitempty"`
	AvailableVersions  []string                 `json:"available_versions"`
	Trusted            bool                     `json:"trusted"`
	SuperuserRequired  bool                     `json:"superuser_required"`
	Relocatable        bool                     `json:"relocatable"`
	MemberObjectsOwned bool                     `json:"member_objects_owned,omitempty"`
	Reason             string                   `json:"reason"`
	Remediation        string                   `json:"remediation"`
}

type ExtensionReadinessReport struct {
	Version     string               `json:"version"`
	Ready       bool                 `json:"ready"`
	ServerMajor int                  `json:"server_major"`
	Extensions  []ExtensionReadiness `json:"extensions"`
}

const ExtensionReadinessReportVersion = "autosql.extension-readiness/v1"

// ExtensionUpdatePath is one server-advertised path between installed
// extension versions. An empty Path means PostgreSQL cannot perform the
// transition with ALTER EXTENSION UPDATE.
type ExtensionUpdatePath struct {
	Name, Source, Target, Path string
}

type ExtensionDiagnostic struct {
	Name, Class, Message string
}

type ExtensionReport struct {
	Supported   bool
	Diagnostics []ExtensionDiagnostic
}

// InspectExtensionCatalogURL inventories server-installed extension packages;
// it does not execute any extension control or installation script.
func InspectExtensionCatalogURL(ctx context.Context, url string) (ExtensionCatalog, error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return ExtensionCatalog{}, safeError("inspect extension catalog", url, err)
	}
	defer conn.Close(context.Background())
	return inspectExtensionCatalogConn(ctx, conn)
}

func inspectExtensionCatalogConn(ctx context.Context, conn *pgx.Conn) (ExtensionCatalog, error) {
	rows, err := conn.Query(ctx, `select name,version,coalesce(schema,''),superuser,trusted,relocatable,coalesce(requires,'{}'::name[]),coalesce(comment,'') from pg_available_extension_versions order by name,version`)
	if err != nil {
		return ExtensionCatalog{}, err
	}
	defer rows.Close()
	catalog := ExtensionCatalog{}
	for rows.Next() {
		var value ExtensionAvailability
		if err = rows.Scan(&value.Name, &value.Version, &value.Schema, &value.Superuser, &value.Trusted, &value.Relocatable, &value.Requires, &value.Comment); err != nil {
			return ExtensionCatalog{}, err
		}
		catalog.Versions = append(catalog.Versions, value)
	}
	if err := rows.Err(); err != nil {
		return ExtensionCatalog{}, err
	}
	pathRows, err := conn.Query(ctx, `select names.name,p.source,p.target,coalesce(p.path,'') from (select distinct name from pg_available_extension_versions) names cross join lateral pg_extension_update_paths(names.name) p order by names.name,p.source,p.target`)
	if err != nil {
		return ExtensionCatalog{}, err
	}
	defer pathRows.Close()
	for pathRows.Next() {
		var path ExtensionUpdatePath
		if err := pathRows.Scan(&path.Name, &path.Source, &path.Target, &path.Path); err != nil {
			return ExtensionCatalog{}, err
		}
		catalog.Paths = append(catalog.Paths, path)
	}
	if err := pathRows.Err(); err != nil {
		return ExtensionCatalog{}, err
	}
	if err := conn.QueryRow(ctx, `select current_setting('server_version_num')::integer/10000,current_user,current_database(),r.rolsuper,pg_get_userbyid(d.datdba)=current_user,has_database_privilege(current_database(),'CREATE') from pg_roles r join pg_database d on d.datname=current_database() where r.rolname=current_user`).Scan(&catalog.Privilege.ServerMajor, &catalog.Privilege.CurrentUser, &catalog.Privilege.CurrentDatabase, &catalog.Privilege.Superuser, &catalog.Privilege.DatabaseOwner, &catalog.Privilege.CanCreateDatabase); err != nil {
		return ExtensionCatalog{}, err
	}
	installed, err := conn.Query(ctx, `
select e.extname,e.extversion,n.nspname,pg_get_userbyid(e.extowner),pg_has_role(current_user,e.extowner,'USAGE'),not exists (
  select 1 from pg_depend d
  where d.refclassid='pg_extension'::regclass and d.refobjid=e.oid and d.deptype='e'
    and (
      (d.classid='pg_type'::regclass and exists (select 1 from pg_type o where o.oid=d.objid and not pg_has_role(current_user,o.typowner,'USAGE')))
      or (d.classid='pg_proc'::regclass and exists (select 1 from pg_proc o where o.oid=d.objid and not pg_has_role(current_user,o.proowner,'USAGE')))
      or (d.classid='pg_class'::regclass and exists (select 1 from pg_class o where o.oid=d.objid and not pg_has_role(current_user,o.relowner,'USAGE')))
      or (d.classid='pg_operator'::regclass and exists (select 1 from pg_operator o where o.oid=d.objid and not pg_has_role(current_user,o.oprowner,'USAGE')))
      or (d.classid='pg_collation'::regclass and exists (select 1 from pg_collation o where o.oid=d.objid and not pg_has_role(current_user,o.collowner,'USAGE')))
      or (d.classid='pg_conversion'::regclass and exists (select 1 from pg_conversion o where o.oid=d.objid and not pg_has_role(current_user,o.conowner,'USAGE')))
      or (d.classid='pg_opclass'::regclass and exists (select 1 from pg_opclass o where o.oid=d.objid and not pg_has_role(current_user,o.opcowner,'USAGE')))
      or (d.classid='pg_opfamily'::regclass and exists (select 1 from pg_opfamily o where o.oid=d.objid and not pg_has_role(current_user,o.opfowner,'USAGE')))
      or (d.classid='pg_namespace'::regclass and exists (select 1 from pg_namespace o where o.oid=d.objid and not pg_has_role(current_user,o.nspowner,'USAGE')))
      or (d.classid='pg_language'::regclass and exists (select 1 from pg_language o where o.oid=d.objid and not pg_has_role(current_user,o.lanowner,'USAGE')))
      or (d.classid='pg_ts_config'::regclass and exists (select 1 from pg_ts_config o where o.oid=d.objid and not pg_has_role(current_user,o.cfgowner,'USAGE')))
      or (d.classid='pg_ts_dict'::regclass and exists (select 1 from pg_ts_dict o where o.oid=d.objid and not pg_has_role(current_user,o.dictowner,'USAGE')))
      or (d.classid='pg_foreign_data_wrapper'::regclass and exists (select 1 from pg_foreign_data_wrapper o where o.oid=d.objid and not pg_has_role(current_user,o.fdwowner,'USAGE')))
      or (d.classid='pg_foreign_server'::regclass and exists (select 1 from pg_foreign_server o where o.oid=d.objid and not pg_has_role(current_user,o.srvowner,'USAGE')))
      or d.classid not in (
        'pg_type'::regclass,'pg_proc'::regclass,'pg_class'::regclass,'pg_operator'::regclass,'pg_collation'::regclass,'pg_conversion'::regclass,
        'pg_opclass'::regclass,'pg_opfamily'::regclass,'pg_namespace'::regclass,'pg_language'::regclass,'pg_ts_config'::regclass,'pg_ts_dict'::regclass,
        'pg_foreign_data_wrapper'::regclass,'pg_foreign_server'::regclass,
        'pg_cast'::regclass,'pg_am'::regclass,'pg_amop'::regclass,'pg_amproc'::regclass,'pg_attribute'::regclass,'pg_attrdef'::regclass,
        'pg_constraint'::regclass,'pg_rewrite'::regclass,'pg_trigger'::regclass,'pg_transform'::regclass,'pg_enum'::regclass,'pg_range'::regclass
      )
    )
) from pg_extension e join pg_namespace n on n.oid=e.extnamespace order by e.extname`)
	if err != nil {
		return ExtensionCatalog{}, err
	}
	for installed.Next() {
		var value ExtensionInstallation
		if err := installed.Scan(&value.Name, &value.Version, &value.Schema, &value.Owner, &value.OwnerUsable, &value.MemberObjectsOwned); err != nil {
			installed.Close()
			return ExtensionCatalog{}, err
		}
		catalog.Installed = append(catalog.Installed, value)
	}
	if err := installed.Err(); err != nil {
		installed.Close()
		return ExtensionCatalog{}, err
	}
	installed.Close()
	schemas, err := conn.Query(ctx, `select nspname,has_schema_privilege(nspname,'CREATE') from pg_namespace order by nspname`)
	if err != nil {
		return ExtensionCatalog{}, err
	}
	defer schemas.Close()
	for schemas.Next() {
		var value ExtensionSchemaPrivilege
		if err := schemas.Scan(&value.Name, &value.CanCreate); err != nil {
			return ExtensionCatalog{}, err
		}
		catalog.Schemas = append(catalog.Schemas, value)
	}
	return catalog, schemas.Err()
}

// ValidateExtensionTransition verifies that PostgreSQL advertises a complete
// update path before a plan containing ALTER EXTENSION UPDATE is published.
func ValidateExtensionTransition(name, currentVersion, desiredVersion string, catalog ExtensionCatalog) error {
	if currentVersion == desiredVersion {
		return nil
	}
	for _, path := range catalog.Paths {
		if path.Name == name && path.Source == currentVersion && path.Target == desiredVersion && path.Path != "" {
			return nil
		}
	}
	return fmt.Errorf("extension %s has no server-advertised update path from %s to %s", name, currentVersion, desiredVersion)
}

// PreflightExtensions aggregates package, allowlist, version, schema, trust,
// dependency, and authority-policy blockers before CREATE EXTENSION is planned.
func PreflightExtensions(document schema.Document, catalog ExtensionCatalog, policy ExtensionPolicy) (ExtensionReport, error) {
	available := map[string][]ExtensionAvailability{}
	for _, value := range catalog.Versions {
		available[value.Name] = append(available[value.Name], value)
	}
	diagnostics := []ExtensionDiagnostic{}
	add := func(name, class, message string) {
		diagnostics = append(diagnostics, ExtensionDiagnostic{Name: name, Class: class, Message: message})
	}
	for _, resource := range document.Graph.Resources {
		if resource.Kind != schema.KindExtension {
			continue
		}
		s := spec(resource)
		name, version, targetSchema := resource.Name.Name, stringValue(s, "version"), resource.Name.Schema
		if !policy.Allowed[name] {
			add(name, "allowlist", "extension is not approved by policy")
		}
		matches := []ExtensionAvailability{}
		for _, candidate := range available[name] {
			if candidate.Version == version {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 0 {
			add(name, "package", "requested extension version is not installed on the server")
			continue
		}
		candidate := matches[0]
		if allowed := policy.Versions[name]; len(allowed) > 0 && !containsString(allowed, version) {
			add(name, "version", "extension version is outside policy")
		}
		if allowed := policy.Schemas[name]; len(allowed) > 0 && !containsString(allowed, targetSchema) {
			add(name, "schema", "extension target schema is outside policy")
		}
		if candidate.Schema != "" && candidate.Schema != targetSchema {
			add(name, "schema", "non-relocatable extension requires its control-file schema")
		}
		if policy.RequireTrusted && !candidate.Trusted {
			add(name, "authority", "extension requires superuser authority")
		}
		for _, required := range candidate.Requires {
			if !extensionResourcePresent(document, required) {
				add(name, "dependency", fmt.Sprintf("required extension %s is absent", required))
			}
		}
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].Name == diagnostics[j].Name {
			return diagnostics[i].Class < diagnostics[j].Class
		}
		return diagnostics[i].Name < diagnostics[j].Name
	})
	return ExtensionReport{Supported: len(diagnostics) == 0, Diagnostics: diagnostics}, nil
}

// PreflightExtensionReadinessURL performs only catalog reads. It never invokes
// CREATE EXTENSION or an operating-system package manager.
func PreflightExtensionReadinessURL(ctx context.Context, maintenanceURL string, target bootstrap.DatabaseTarget, document schema.Document, policy ExtensionPolicy) (ExtensionReadinessReport, error) {
	target = target.Normalize()
	if err := target.Validate(); err != nil {
		return ExtensionReadinessReport{}, err
	}
	maintenance, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		return ExtensionReadinessReport{}, safeError("preflight extension readiness", maintenanceURL, err)
	}
	var targetExists bool
	if err := maintenance.QueryRow(ctx, `select exists(select 1 from pg_database where datname=$1)`, target.Name).Scan(&targetExists); err != nil {
		maintenance.Close(context.Background())
		return ExtensionReadinessReport{}, err
	}
	catalogConn := maintenance
	if targetExists {
		maintenance.Close(context.Background())
		config, err := pgx.ParseConfig(maintenanceURL)
		if err != nil {
			return ExtensionReadinessReport{}, safeError("preflight extension readiness", maintenanceURL, err)
		}
		config.Database = target.Name
		catalogConn, err = pgx.ConnectConfig(ctx, config)
		if err != nil {
			return ExtensionReadinessReport{}, safeError("preflight extension readiness", maintenanceURL, err)
		}
	}
	defer catalogConn.Close(context.Background())
	catalog, err := inspectExtensionCatalogConn(ctx, catalogConn)
	if err != nil {
		return ExtensionReadinessReport{}, safeError("preflight extension readiness", maintenanceURL, err)
	}
	if !targetExists {
		catalog.Installed = nil
		catalog.Schemas = nil
		catalog.Privilege.CurrentDatabase = target.Name
		catalog.Privilege.DatabaseOwner = catalog.Privilege.Superuser || catalog.Privilege.CurrentUser == target.Owner
		catalog.Privilege.CanCreateDatabase = catalog.Privilege.DatabaseOwner
	}
	return EvaluateExtensionReadiness(document, catalog, policy), nil
}

func EvaluateExtensionReadiness(document schema.Document, catalog ExtensionCatalog, policy ExtensionPolicy) ExtensionReadinessReport {
	available := map[string][]ExtensionAvailability{}
	installed := map[string]ExtensionInstallation{}
	schemaPrivileges := map[string]bool{}
	for _, value := range catalog.Versions {
		available[value.Name] = append(available[value.Name], value)
	}
	for _, value := range catalog.Installed {
		installed[value.Name] = value
	}
	for _, value := range catalog.Schemas {
		schemaPrivileges[value.Name] = value.CanCreate
	}
	plannedSchemas := map[string]bool{}
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindSchema {
			plannedSchemas[resource.Name.Name] = true
		}
	}
	report := ExtensionReadinessReport{Version: ExtensionReadinessReportVersion, Ready: true, ServerMajor: catalog.Privilege.ServerMajor, Extensions: []ExtensionReadiness{}}
	for _, resource := range document.Graph.Resources {
		if resource.Kind != schema.KindExtension {
			continue
		}
		values := spec(resource)
		entry := ExtensionReadiness{ResourceID: resource.ID, Name: resource.Name.Name, RequestedVersion: stringValue(values, "version"), RequestedSchema: resource.Name.Schema, AvailableVersions: []string{}}
		for _, candidate := range available[entry.Name] {
			entry.AvailableVersions = append(entry.AvailableVersions, candidate.Version)
		}
		entry.AvailableVersions = uniqueNonEmptySorted(entry.AvailableVersions)
		if current, ok := installed[entry.Name]; ok {
			entry.InstalledVersion, entry.InstalledSchema = current.Version, current.Schema
			entry.MemberObjectsOwned = current.MemberObjectsOwned
		}
		set := func(status ExtensionReadinessStatus, reason, remediation string) {
			entry.Status, entry.Reason, entry.Remediation = status, reason, remediation
			if status != ExtensionReady {
				report.Ready = false
			}
		}
		if !policy.Allowed[entry.Name] {
			set(ExtensionUnauthorized, "extension name is not independently allowlisted", "add "+entry.Name+" to the reviewed extension allowlist")
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		if pins := policy.Versions[entry.Name]; len(pins) != 1 || pins[0] != entry.RequestedVersion {
			set(ExtensionUnauthorized, "requested version is not bound by one exact policy pin", "set extension_version."+entry.Name+"="+entry.RequestedVersion)
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		if schemas := policy.Schemas[entry.Name]; len(schemas) == 0 || !containsString(schemas, entry.RequestedSchema) {
			set(ExtensionUnauthorized, "requested schema is outside the reviewed schema policy", "allow exactly the reviewed target schema "+entry.RequestedSchema)
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		if len(available[entry.Name]) == 0 {
			set(ExtensionMissingPackage, "server does not advertise an extension control file", "install the PostgreSQL server package providing "+entry.Name+".control on every target host; AutoSQL does not install OS packages")
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		var candidate *ExtensionAvailability
		for index := range available[entry.Name] {
			if available[entry.Name][index].Version == entry.RequestedVersion {
				candidate = &available[entry.Name][index]
				break
			}
		}
		if candidate == nil {
			set(ExtensionUnavailableVersion, "server control metadata does not advertise the requested version", "install extension control/update files advertising version "+entry.RequestedVersion+"; available versions: "+strings.Join(entry.AvailableVersions, ","))
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		entry.Trusted, entry.SuperuserRequired, entry.Relocatable = candidate.Trusted, candidate.Superuser && !candidate.Trusted, candidate.Relocatable
		if policy.RequireTrusted && !candidate.Trusted || !candidate.Trusted && !policy.AllowUntrusted && !policy.UntrustedResources[entry.ResourceID] {
			set(ExtensionUnauthorized, "extension control metadata is untrusted and lacks explicit authorization", "review the server package and authorize untrusted extension execution independently")
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		if candidate.Schema != "" && candidate.Schema != entry.RequestedSchema {
			set(ExtensionSchemaConflicted, "non-relocatable control metadata requires schema "+candidate.Schema, "change the declaration to schema "+candidate.Schema+" or install compatible relocatable control metadata")
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		current, isInstalled := installed[entry.Name]
		if isInstalled && current.Version != entry.RequestedVersion {
			if err := ValidateExtensionTransition(entry.Name, current.Version, entry.RequestedVersion, catalog); err != nil {
				set(ExtensionUnavailableVersion, "installed version has no server-advertised update path to the requested version", "install the missing extension update scripts or choose an advertised target version")
				report.Extensions = append(report.Extensions, entry)
				continue
			}
		}
		if isInstalled && current.Schema != entry.RequestedSchema && !candidate.Relocatable {
			set(ExtensionSchemaConflicted, "installed extension is in schema "+current.Schema+" and control metadata is not relocatable", "keep schema "+current.Schema+" or reinstall through an explicitly reviewed procedure")
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		schemaExists := schemaKnown(catalog.Schemas, entry.RequestedSchema)
		if !schemaExists && ((!isInstalled && !plannedSchemas[entry.RequestedSchema]) || (isInstalled && current.Schema != entry.RequestedSchema)) {
			reason := "target schema does not exist and is not declared"
			remediation := "declare schema " + entry.RequestedSchema + " before the extension"
			if isInstalled && current.Schema != entry.RequestedSchema {
				reason = "extension relocation destination schema does not exist"
				remediation = "create schema " + entry.RequestedSchema + " and grant CREATE on it before relocating the extension"
			}
			set(ExtensionSchemaConflicted, reason, remediation)
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		createRequired := !isInstalled
		updateRequired := isInstalled && current.Version != entry.RequestedVersion
		moveRequired := isInstalled && current.Schema != entry.RequestedSchema
		if createRequired {
			databaseAllowed := catalog.Privilege.Superuser || catalog.Privilege.DatabaseOwner || catalog.Privilege.CanCreateDatabase
			schemaAllowed := schemaPrivileges[entry.RequestedSchema] || plannedSchemas[entry.RequestedSchema] && databaseAllowed
			if entry.SuperuserRequired && !catalog.Privilege.Superuser {
				set(ExtensionPrivilegeBlocked, "untrusted superuser extension requires a superuser session", "run the reviewed bootstrap with a PostgreSQL superuser; role grants cannot substitute for this control-file requirement")
			} else if !databaseAllowed {
				set(ExtensionPrivilegeBlocked, "current role lacks CREATE on database "+catalog.Privilege.CurrentDatabase, "grant CREATE on database "+catalog.Privilege.CurrentDatabase+" or run as its owner")
			} else if !schemaAllowed {
				set(ExtensionPrivilegeBlocked, "current role lacks CREATE on target schema "+entry.RequestedSchema, "grant CREATE on schema "+entry.RequestedSchema+" to "+catalog.Privilege.CurrentUser)
			}
		} else if updateRequired || moveRequired {
			ownerAllowed := catalog.Privilege.Superuser || current.OwnerUsable || current.Owner == catalog.Privilege.CurrentUser
			if !ownerAllowed {
				set(ExtensionPrivilegeBlocked, "current role does not own the installed extension", "run as extension owner "+current.Owner+" or a PostgreSQL superuser")
			} else if updateRequired && moveRequired && candidate.Trusted && !catalog.Privilege.Superuser {
				set(ExtensionPrivilegeBlocked, "trusted extension update may add bootstrap-superuser-owned members before relocation", "apply the reviewed update first, re-run readiness and transfer any new member ownership, then relocate; or run the combined operation as a PostgreSQL superuser")
			} else if moveRequired && !schemaPrivileges[entry.RequestedSchema] && !catalog.Privilege.Superuser {
				set(ExtensionPrivilegeBlocked, "current role lacks CREATE on the existing relocation destination schema "+entry.RequestedSchema, "grant CREATE on schema "+entry.RequestedSchema+" to "+catalog.Privilege.CurrentUser)
			} else if moveRequired && !current.MemberObjectsOwned && !catalog.Privilege.Superuser {
				set(ExtensionPrivilegeBlocked, "current role does not own every relocatable extension member object", "run the relocation as a PostgreSQL superuser or transfer ownership of all extension member objects through a reviewed procedure")
			}
		}
		if entry.Status != "" {
			report.Extensions = append(report.Extensions, entry)
			continue
		}
		for _, required := range candidate.Requires {
			if !extensionResourcePresent(document, required) {
				if _, ok := installed[required]; !ok {
					set(ExtensionUnavailableVersion, "required extension "+required+" is neither installed nor declared", "declare and authorize required extension "+required+" before "+entry.Name)
					break
				}
			}
		}
		if entry.Status == "" {
			set(ExtensionReady, "server package, exact version, schema, trust, dependencies, and privileges are ready", "none")
		}
		report.Extensions = append(report.Extensions, entry)
	}
	sort.Slice(report.Extensions, func(i, j int) bool {
		if report.Extensions[i].Name == report.Extensions[j].Name {
			return report.Extensions[i].ResourceID < report.Extensions[j].ResourceID
		}
		return report.Extensions[i].Name < report.Extensions[j].Name
	})
	return report
}

func schemaKnown(values []ExtensionSchemaPrivilege, name string) bool {
	for _, value := range values {
		if value.Name == name {
			return true
		}
	}
	return false
}

func PreflightBootstrapExtensionsURL(ctx context.Context, maintenanceURL string, whole bootstrap.Plan) (ExtensionReadinessReport, error) {
	document := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	policy := ExtensionPolicy{Allowed: map[string]bool{}, Versions: map[string][]string{}, Schemas: map[string][]string{}, UntrustedResources: map[string]bool{}}
	seen := map[string]bool{}
	for _, change := range whole.SchemaPlan.Changes.Changes {
		if change.After == nil || (change.After.Kind != schema.KindExtension && change.After.Kind != schema.KindSchema) || seen[change.After.ID] {
			continue
		}
		seen[change.After.ID] = true
		document.Graph.Resources = append(document.Graph.Resources, *change.After)
		if change.After.Kind == schema.KindExtension {
			name := change.After.Name.Name
			policy.Allowed[name] = true
			policy.Versions[name] = []string{stringValue(spec(*change.After), "version")}
			policy.Schemas[name] = []string{change.After.Name.Schema}
			policy.UntrustedResources[change.After.ID] = hasBootstrapExtensionAuthorization(whole, change.After.ID)
		}
	}
	document.Normalize()
	if len(policy.Allowed) == 0 {
		return ExtensionReadinessReport{Version: ExtensionReadinessReportVersion, Ready: true, Extensions: []ExtensionReadiness{}}, nil
	}
	return PreflightExtensionReadinessURL(ctx, maintenanceURL, whole.Target, document, policy)
}

func extensionReadinessError(report ExtensionReadinessReport) error {
	var blockers []string
	for _, extension := range report.Extensions {
		if extension.Status != ExtensionReady {
			blockers = append(blockers, extension.Name+"="+string(extension.Status)+": "+extension.Remediation)
		}
	}
	if len(blockers) == 0 {
		return nil
	}
	return errors.New("extension readiness preflight failed: " + strings.Join(blockers, "; "))
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func extensionResourcePresent(document schema.Document, name string) bool {
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindExtension && resource.Name.Name == name {
			return true
		}
	}
	return false
}

func extensionRequirementSpec(availability ExtensionAvailability, owner string) map[string]any {
	return map[string]any{"version": availability.Version, "relocatable": availability.Relocatable, "trusted": availability.Trusted, "superuser": availability.Superuser, "requires": availability.Requires, "owner": owner}
}

func validateExtensionRequirement(resource schema.Resource) error {
	s := spec(resource)
	if !allowedKeys(s, "version", "relocatable", "trusted", "superuser", "requires", "owner", "cascade") {
		return unsupported(resource, "unknown extension requirement metadata")
	}
	if stringValue(s, "version") == "" {
		return unsupported(resource, "extension version is required")
	}
	for _, key := range []string{"relocatable", "trusted", "superuser"} {
		if value, exists := s[key]; exists {
			if _, ok := value.(bool); ok {
				continue
			}
			return unsupported(resource, "extension "+key+" must be boolean")
		}
	}
	if value, exists := s["cascade"]; exists {
		if _, ok := value.(bool); !ok {
			return unsupported(resource, "extension cascade must be boolean")
		}
	}
	if strings.TrimSpace(resource.Name.Schema) == "" {
		return unsupported(resource, "extension target schema is required")
	}
	return nil
}

func validateExtensionOptions(resource schema.Resource, options map[string]string) error {
	if err := validateExtensionRequirement(resource); err != nil {
		return err
	}
	name := resource.Name.Name
	if !containsString(splitPatterns(options["extension_allowlist"]), name) {
		return unsupported(resource, "extension is not present in extension_allowlist")
	}
	if exact := strings.TrimSpace(options["extension_version."+name]); exact != "" && exact != stringValue(spec(resource), "version") {
		return unsupported(resource, "extension version is outside the configured exact policy")
	}
	if allowed := splitPatterns(options["extension_schemas."+name]); len(allowed) > 0 && !containsString(allowed, resource.Name.Schema) {
		return unsupported(resource, "extension schema is outside the configured policy")
	}
	if trusted, present := spec(resource)["trusted"].(bool); present && !trusted && !enabled(options, "allow_untrusted_extensions", false) {
		return unsupported(resource, "untrusted extension requires allow_untrusted_extensions=true")
	}
	return nil
}
