package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

// ProvisioningDiagnostic describes one independently actionable blocker found
// before attempting to render a complete PostgreSQL document from an empty
// database projection. Messages never include spec or annotation values.
type ProvisioningDiagnostic struct {
	ResourceID string      `json:"resource_id"`
	Kind       schema.Kind `json:"kind"`
	Name       string      `json:"name"`
	Class      string      `json:"class"`
	Field      string      `json:"field,omitempty"`
	External   bool        `json:"external,omitempty"`
	Message    string      `json:"message"`
}

// ProvisioningReport is a deterministic, aggregate fresh-provisioning check.
type ProvisioningReport struct {
	Supported            bool                     `json:"supported"`
	Diagnostics          []ProvisioningDiagnostic `json:"diagnostics"`
	Authority            []bootstrap.Binding      `json:"authority,omitempty"`
	AuthorityDiagnostics []string                 `json:"authority_diagnostics,omitempty"`
}

// PreflightBootstrapProvisioning performs the complete renderability and
// non-secret authority check for a fresh database. createDatabase controls
// whether cluster-level database creation is part of this run. The returned
// assignments state exactly which principal performs each required phase.
func PreflightBootstrapProvisioning(ctx context.Context, doc schema.Document, options map[string]string, contract bootstrap.Contract, createDatabase bool) (ProvisioningReport, error) {
	report, err := PreflightProvisioning(ctx, doc, options)
	if err != nil {
		return report, err
	}
	bindings, authorityErr := contract.Validate(bootstrapRequirements(doc, createDatabase))
	report.Authority = bindings
	if authorityErr != nil {
		for _, problem := range strings.Split(authorityErr.Error(), "\n") {
			problem = strings.TrimPrefix(problem, "- ")
			if problem != "" {
				report.AuthorityDiagnostics = append(report.AuthorityDiagnostics, problem)
			}
		}
		sort.Strings(report.AuthorityDiagnostics)
		report.Supported = false
	}
	return report, nil
}

// ValidateBootstrapAuthority validates only the non-secret phase-to-identity
// contract. Callers using a signed bootstrap authorization manifest validate
// routine and extension renderability when they materialize the exact
// manifest-bound plan, rather than weakening those gates with synthetic
// legacy render options during the earlier authority check.
func ValidateBootstrapAuthority(doc schema.Document, contract bootstrap.Contract, createDatabase bool) ([]bootstrap.Binding, error) {
	return contract.Validate(bootstrapRequirements(doc, createDatabase))
}

func bootstrapRequirements(doc schema.Document, createDatabase bool) []bootstrap.Requirement {
	required := map[bootstrap.Responsibility]bootstrap.Requirement{}
	add := func(responsibility bootstrap.Responsibility, capability bootstrap.Capability, reason string) {
		required[responsibility] = bootstrap.Requirement{Responsibility: responsibility, Capability: capability, Reason: reason}
	}
	if createDatabase {
		add(bootstrap.DatabaseCreation, bootstrap.CreateDatabase, "create the target database")
	}
	for _, resource := range doc.Graph.Resources {
		switch resource.Kind {
		case schema.KindDatabase:
			add(bootstrap.DatabaseCreation, bootstrap.CreateDatabase, "create the target database")
		case schema.KindRole, schema.KindMembership:
			add(bootstrap.RoleCreation, bootstrap.ManageRoles, "create roles and establish memberships")
		case schema.KindExtension:
			add(bootstrap.ExtensionSetup, bootstrap.ManageExtensions, "install required database extensions")
		case schema.KindGrant, schema.KindDefaultPrivilege:
			add(bootstrap.GrantSetup, bootstrap.ManageGrants, "apply grants and default privileges")
		default:
			add(bootstrap.SchemaObjects, bootstrap.ManageSchema, "create schemas and database objects")
		}
	}
	if len(doc.Graph.Resources) > 0 {
		add(bootstrap.OwnershipHandoff, bootstrap.TransferOwnership, "transfer final ownership to declared owners")
	}
	out := make([]bootstrap.Requirement, 0, len(required))
	for _, requirement := range required {
		out = append(out, requirement)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Responsibility < out[j].Responsibility })
	return out
}

// PreflightProvisioning inventories every known managed blocker and external
// prerequisite in doc without rendering or executing partial SQL.
func PreflightProvisioning(ctx context.Context, doc schema.Document, options map[string]string) (ProvisioningReport, error) {
	if err := ctx.Err(); err != nil {
		return ProvisioningReport{}, err
	}
	normalized, err := New().Normalize(ctx, doc)
	if err != nil {
		return ProvisioningReport{}, fmt.Errorf("preflight PostgreSQL provisioning: %w", err)
	}
	resources := resourceMapForRender(normalized)
	info := New().Info()
	diagnostics := make([]ProvisioningDiagnostic, 0)
	seen := map[string]bool{}
	add := func(resource schema.Resource, class, field, message string, external bool) {
		key := strings.Join([]string{resource.ID, class, field}, "\x00")
		if seen[key] {
			return
		}
		seen[key] = true
		diagnostics = append(diagnostics, ProvisioningDiagnostic{
			ResourceID: resource.ID,
			Kind:       resource.Kind,
			Name:       resource.Name.String(),
			Class:      class,
			Field:      field,
			External:   external,
			Message:    message,
		})
	}

	for _, resource := range normalized.Graph.Resources {
		capability := info.Capability(resource.Kind)
		if err := plugin.RequireManagedOperation(info, resource.Kind, schema.OperationCreate); err != nil {
			add(resource, "unsupported_operation", "create", "resource kind does not support fresh creation", capability.Mode != plugin.Managed)
		}
		if capability.Mode != plugin.Managed {
			add(resource, "external_prerequisite", "", "resource is inspectable but must be provisioned outside AutoSQL", true)
			if resource.Kind == schema.KindFunction || resource.Kind == schema.KindProcedure {
				allowed := provisioningSpecKeys(resource.Kind)
				for key := range spec(resource) {
					if !allowed[key] {
						add(resource, "unsupported_spec_key", key, "routine field is outside the canonical inspected model", true)
					}
				}
				if _, sourceErr := validateRoutineSource(resource, resources, options); sourceErr != nil {
					add(resource, "routine_source_trust", "definition", "routine source is not parser-proven and bound to explicit review authority", true)
				}
			}
			continue
		}
		for key := range resource.Annotations {
			if key != "comment" {
				add(resource, "unsupported_annotation", key, "managed annotation is not renderable", false)
			}
		}
		for key := range resource.Extra {
			add(resource, "extension_metadata", key, "resource extension metadata is not renderable", false)
		}
		for key := range resource.Name.Extra {
			add(resource, "extension_metadata", "name."+key, "name extension metadata is not renderable", false)
		}
		for index, dependency := range resource.Dependencies {
			for key := range dependency.Extra {
				add(resource, "dependency_metadata", fmt.Sprintf("dependencies[%d].%s", index, key), "dependency extension metadata is not renderable", false)
			}
		}

		values := spec(resource)
		allowed := provisioningSpecKeys(resource.Kind)
		for key := range values {
			if !allowed[key] {
				class := "unsupported_spec_key"
				message := "spec field is outside the managed provisioning grammar"
				add(resource, class, key, message, false)
			}
		}
		if err := validateCanonicalIdentity(resource, resources); err != nil {
			add(resource, "dependency", "identity", "resource identity or containment dependencies are noncanonical", false)
		}
		if err := validateSemanticDependencies(resource, resources); err != nil {
			add(resource, "dependency", "semantics", "declared dependencies do not exactly describe rendered semantics", false)
		}
		if resource.Kind == schema.KindColumn && stringValue(values, "generated") != "" {
			if err := validateGeneratedColumnCreate(resource, resources); err != nil {
				add(resource, "unsupported_semantic", "generated", "stored generated expression is outside the bounded provisioning policy", false)
			}
		}
		if !hasResourceDiagnostic(diagnostics, resource.ID) {
			if _, err := renderCreate(resource, resources, options); err != nil {
				add(resource, "renderability", "", "resource semantics are outside the managed provisioning grammar", false)
			}
		}
	}

	sort.Slice(diagnostics, func(i, j int) bool {
		a, b := diagnostics[i], diagnostics[j]
		left := strings.Join([]string{string(a.Kind), a.Name, a.ResourceID, a.Class, a.Field}, "\x00")
		right := strings.Join([]string{string(b.Kind), b.Name, b.ResourceID, b.Class, b.Field}, "\x00")
		return left < right
	})
	return ProvisioningReport{Supported: len(diagnostics) == 0, Diagnostics: diagnostics}, nil
}

func hasResourceDiagnostic(diagnostics []ProvisioningDiagnostic, resourceID string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.ResourceID == resourceID {
			return true
		}
	}
	return false
}

func provisioningSpecKeys(kind schema.Kind) map[string]bool {
	keys := map[schema.Kind][]string{
		schema.KindSchema:           {"owner"},
		schema.KindExtension:        {"version", "relocatable", "trusted", "superuser", "requires", "owner", "cascade"},
		schema.KindEnum:             {"values", "owner"},
		schema.KindDomain:           {"base_type", "default", "not_null", "constraints", "owner"},
		schema.KindComposite:        {"attributes", "owner"},
		schema.KindSequence:         {"start", "increment", "min", "max", "cache", "cycle", "owner"},
		schema.KindTable:            {"partitioned", "persistence", "row_security", "force_row_security", "owner"},
		schema.KindColumn:           {"type", "default", "not_null", "ordinal", "identity", "generated"},
		schema.KindPrimaryKey:       {"definition", "deferrable", "initially_deferred", "validated", "columns"},
		schema.KindUniqueConstraint: {"definition", "deferrable", "initially_deferred", "validated", "columns"},
		schema.KindCheckConstraint:  {"definition", "deferrable", "initially_deferred", "validated", "columns"},
		schema.KindForeignKey:       {"definition", "deferrable", "initially_deferred", "validated", "columns", "referenced_columns"},
		schema.KindIndex:            {"definition", "method", "unique", "valid", "ready", "columns"},
		schema.KindView:             {"definition", "owner"},
		schema.KindMaterializedView: {"definition", "owner"},
		schema.KindFunction:         {"name", "identity_arguments", "arguments", "result", "returns_set", "language", "volatility", "strict", "security_definer", "leakproof", "parallel", "cost", "rows", "configuration", "owner", "definition", "body_digest", "extension"},
		schema.KindProcedure:        {"name", "identity_arguments", "arguments", "result", "returns_set", "language", "volatility", "strict", "security_definer", "leakproof", "parallel", "cost", "rows", "configuration", "owner", "definition", "body_digest", "extension"},
		schema.KindTrigger:          {"definition", "enabled", "columns"},
		schema.KindPolicy:           {"command", "permissive", "roles", "using", "check"},
		schema.KindRole:             {"superuser", "inherit", "create_role", "create_database", "login", "replication", "bypass_rls", "connection_limit", "valid_until", "configuration"},
		schema.KindMembership:       {"parent", "member", "grantor", "admin", "inherit", "set"},
		schema.KindGrant:            {"grantor", "grantee", "privilege", "grantable"},
		schema.KindDefaultPrivilege: {"owner", "object_type", "schema", "grantee", "privilege", "grantable"},
	}
	out := map[string]bool{}
	for _, key := range keys[kind] {
		out[key] = true
	}
	return out
}

// MarshalCanonical returns the stable JSON representation used by CLI and CI
// consumers to compare complete provisioning inventories.
func (report ProvisioningReport) MarshalCanonical() ([]byte, error) {
	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
