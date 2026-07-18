package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/schema"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

const BootstrapAuthorizationInventoryVersion = "autosql.bootstrap-authorization-inventory/v1"

// BootstrapRoutineAuthorization is the review material required before a
// routine may be rendered. Definition is deliberately absent unless the
// caller explicitly requests it.
type BootstrapRoutineAuthorization struct {
	ResourceID                              string   `json:"resource_id"`
	Kind                                    string   `json:"kind"`
	Schema                                  string   `json:"schema"`
	Name                                    string   `json:"name"`
	Signature                               string   `json:"signature"`
	Language                                string   `json:"language"`
	SourceDigest                            string   `json:"source_digest"`
	DigestReviewRequired                    bool     `json:"digest_review_required"`
	UnsafeLanguageAuthorizationRequired     bool     `json:"unsafe_language_authorization_required"`
	PrivilegedRoutineAuthorizationRequired  bool     `json:"privileged_routine_authorization_required"`
	TransactionControlAuthorizationRequired bool     `json:"transaction_control_authorization_required"`
	Dependencies                            []string `json:"dependencies,omitempty"`
	Definition                              string   `json:"definition,omitempty"`
}

// BootstrapExtensionAuthorization describes every policy and server
// readiness assertion needed for CREATE EXTENSION. It contains requirements,
// not credentials or the contents of extension scripts.
type BootstrapExtensionAuthorization struct {
	ResourceID                              string   `json:"resource_id"`
	Name                                    string   `json:"name"`
	Version                                 string   `json:"version"`
	Schema                                  string   `json:"schema"`
	Requires                                []string `json:"requires,omitempty"`
	AllowlistRequired                       bool     `json:"allowlist_required"`
	ExactVersionRequired                    bool     `json:"exact_version_required"`
	SchemaPolicyRequired                    bool     `json:"schema_policy_required"`
	ServerPackageRequired                   bool     `json:"server_package_required"`
	Trusted                                 bool     `json:"trusted"`
	SuperuserRequired                       bool     `json:"superuser_required"`
	UntrustedExtensionAuthorizationRequired bool     `json:"untrusted_extension_authorization_required"`
	Relocatable                             bool     `json:"relocatable"`
}

// BootstrapAuthorizationInventory is a deterministic, credential-free review
// contract bound to the exact canonical bootstrap plan it authorizes.
type BootstrapAuthorizationInventory struct {
	Version      string                            `json:"version"`
	PlanDigest   string                            `json:"plan_digest"`
	SourceDigest string                            `json:"source_digest"`
	Database     string                            `json:"database"`
	PlanSummary  BootstrapAuthorizationPlanSummary `json:"plan_summary"`
	Routines     []BootstrapRoutineAuthorization   `json:"routines"`
	Extensions   []BootstrapExtensionAuthorization `json:"extensions"`
}

// BootstrapAuthorizationPlanSummary contains enough immutable identity and
// scale information to review prepared authority without carrying executable
// SQL, steps, phases, or dependency topology.
type BootstrapAuthorizationPlanSummary struct {
	SchemaPlanDigest string `json:"schema_plan_digest"`
	StepCount        int    `json:"step_count"`
	PhaseCount       int    `json:"phase_count"`
}

type BootstrapAuthorizationInventoryOptions struct {
	Render               map[string]string
	IncludeRoutineSource bool
}

// PrepareBootstrapAuthorizationInventory discovers all bootstrap safety gates
// in one pass. It internally plans with discovered authorizations only to
// compute immutable review identity, then discards that plan. Returning only
// an inventory makes it impossible to pass synthetic authority to the
// bootstrap executor; execution must build a new, explicitly authorized plan.
func PrepareBootstrapAuthorizationInventory(ctx context.Context, target bootstrap.DatabaseTarget, desired schema.Document, options BootstrapAuthorizationInventoryOptions) (BootstrapAuthorizationInventory, error) {
	// Discover from the same normalized graph the planner renders so source
	// digests cannot drift during canonical PostgreSQL normalization.
	normalized, err := New().Normalize(ctx, desired)
	if err != nil {
		return BootstrapAuthorizationInventory{}, err
	}
	desired = normalized
	sourceDigest, err := schema.SemanticFingerprint(desired)
	if err != nil {
		return BootstrapAuthorizationInventory{}, err
	}
	resources := make(map[string]schema.Resource, len(desired.Graph.Resources))
	for _, resource := range desired.Graph.Resources {
		resources[resource.ID] = resource
	}
	inventory := BootstrapAuthorizationInventory{Version: BootstrapAuthorizationInventoryVersion, SourceDigest: sourceDigest, Database: target.Normalize().Name, Routines: []BootstrapRoutineAuthorization{}, Extensions: []BootstrapExtensionAuthorization{}}
	render := cloneRenderOptions(options.Render)
	var routineDigests []string
	var extensionNames []string
	unsafeLanguageRequired := false
	privilegedRoutineRequired := false
	transactionControlRequired := false
	untrustedExtensionRequired := false
	for _, resource := range desired.Graph.Resources {
		values := spec(resource)
		switch resource.Kind {
		case schema.KindFunction, schema.KindProcedure:
			// Members installed by CREATE EXTENSION are not independently
			// rendered and therefore do not require source authorization.
			if stringValue(values, "extension") != "" {
				continue
			}
			digest := stringValue(values, "body_digest")
			if digest == "" && stringValue(values, "definition") == "" {
				continue
			}
			signature, err := routineSignature(resource, resources)
			if err != nil {
				return BootstrapAuthorizationInventory{}, err
			}
			definition := stringValue(values, "definition")
			language := strings.ToLower(stringValue(values, "language"))
			unsafeLanguage := language != "sql" && language != "plpgsql"
			privileged := privilegedRoutineSource.MatchString(definition)
			transactionControl := resource.Kind == schema.KindProcedure && procedureTransactionControl.MatchString(definition)
			item := BootstrapRoutineAuthorization{
				ResourceID: resource.ID, Kind: string(resource.Kind), Schema: resource.Name.Schema, Name: stringValue(values, "name"), Signature: signature,
				Language: language, SourceDigest: digest, DigestReviewRequired: true,
				UnsafeLanguageAuthorizationRequired: unsafeLanguage, PrivilegedRoutineAuthorizationRequired: privileged,
				TransactionControlAuthorizationRequired: transactionControl, Dependencies: dependencyContext(resource, resources),
			}
			if options.IncludeRoutineSource {
				item.Definition = definition
			}
			inventory.Routines = append(inventory.Routines, item)
			routineDigests = append(routineDigests, digest)
			unsafeLanguageRequired = unsafeLanguageRequired || unsafeLanguage
			privilegedRoutineRequired = privilegedRoutineRequired || privileged
			transactionControlRequired = transactionControlRequired || transactionControl
		case schema.KindExtension:
			name := resource.Name.Name
			requires := append([]string(nil), stringSlice(values, "requires")...)
			sort.Strings(requires)
			trusted, trustedPresent := values["trusted"].(bool)
			superuser, _ := values["superuser"].(bool)
			relocatable, _ := values["relocatable"].(bool)
			inventory.Extensions = append(inventory.Extensions, BootstrapExtensionAuthorization{
				ResourceID: resource.ID, Name: name, Version: stringValue(values, "version"), Schema: resource.Name.Schema, Requires: requires,
				AllowlistRequired: true, ExactVersionRequired: true, SchemaPolicyRequired: true, ServerPackageRequired: true,
				Trusted: trustedPresent && trusted, SuperuserRequired: extensionRequiresSuperuser(superuser, trustedPresent && trusted),
				UntrustedExtensionAuthorizationRequired: trustedPresent && !trusted, Relocatable: relocatable,
			})
			untrustedExtensionRequired = untrustedExtensionRequired || trustedPresent && !trusted
			extensionNames = append(extensionNames, name)
			render["extension_version."+name] = stringValue(values, "version")
			render["extension_schemas."+name] = resource.Name.Schema
		}
	}
	sort.Slice(inventory.Routines, func(i, j int) bool {
		if inventory.Routines[i].Signature == inventory.Routines[j].Signature {
			return inventory.Routines[i].Kind < inventory.Routines[j].Kind
		}
		return inventory.Routines[i].Signature < inventory.Routines[j].Signature
	})
	sort.Slice(inventory.Extensions, func(i, j int) bool {
		if inventory.Extensions[i].Name == inventory.Extensions[j].Name {
			if inventory.Extensions[i].Version == inventory.Extensions[j].Version {
				return inventory.Extensions[i].Schema < inventory.Extensions[j].Schema
			}
			return inventory.Extensions[i].Version < inventory.Extensions[j].Version
		}
		return inventory.Extensions[i].Name < inventory.Extensions[j].Name
	})
	routineDigests = uniqueNonEmptySorted(routineDigests)
	extensionNames = uniqueNonEmptySorted(extensionNames)
	render["reviewed_routine_digests"] = strings.Join(routineDigests, ",")
	render["extension_allowlist"] = strings.Join(extensionNames, ",")
	if unsafeLanguageRequired {
		render["allow_unsafe_routine_languages"] = "true"
	}
	if privilegedRoutineRequired {
		render["allow_privileged_routines"] = "true"
	}
	if transactionControlRequired {
		render["allow_transaction_control_procedures"] = "true"
	}
	if untrustedExtensionRequired {
		// This authority exists only in the synthetic render options used to
		// compute the inventory-bound plan. Execute callers must still provide
		// their own explicit authorization.
		render["allow_untrusted_extensions"] = "true"
	}
	whole, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: render})
	if err != nil {
		return BootstrapAuthorizationInventory{}, err
	}
	inventory.PlanDigest = whole.Digest
	inventory.PlanSummary = BootstrapAuthorizationPlanSummary{SchemaPlanDigest: whole.SchemaPlan.Digest, StepCount: len(whole.Steps), PhaseCount: len(whole.Phases)}
	return inventory, inventory.Validate()
}

func (i BootstrapAuthorizationInventory) Validate() error {
	if i.Version != BootstrapAuthorizationInventoryVersion || !canonicalSHA256Digest(i.PlanDigest) || !canonicalSHA256Digest(i.SourceDigest) || strings.TrimSpace(i.Database) == "" || !canonicalSHA256Digest(i.PlanSummary.SchemaPlanDigest) || i.PlanSummary.StepCount < 1 || i.PlanSummary.PhaseCount < 1 {
		return fmt.Errorf("invalid bootstrap authorization inventory identity")
	}
	for _, routine := range i.Routines {
		if routine.ResourceID == "" || routine.Kind == "" || routine.Signature == "" || routine.Language == "" || !routine.DigestReviewRequired || !canonicalSHA256Digest(routine.SourceDigest) {
			return fmt.Errorf("invalid bootstrap routine authorization for %q", routine.Signature)
		}
		if routine.Definition != "" && routineDefinitionDigest(routine.Definition) != routine.SourceDigest {
			return fmt.Errorf("bootstrap routine definition digest mismatch for %q", routine.Signature)
		}
	}
	for _, extension := range i.Extensions {
		if extension.ResourceID == "" || extension.Name == "" || extension.Version == "" || extension.Schema == "" {
			return fmt.Errorf("invalid bootstrap extension authorization for %q", extension.Name)
		}
	}
	return nil
}

func (i BootstrapAuthorizationInventory) MarshalCanonical() ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// MarshalHCL returns the same deterministic review contract in a form that
// can be checked into an HCL-based bootstrap repository.
func (i BootstrapAuthorizationInventory) MarshalHCL() ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	file := hclwrite.NewEmptyFile()
	root := hclwrite.NewBlock("bootstrap_authorization_inventory", nil)
	body := root.Body()
	body.SetAttributeValue("version", cty.StringVal(i.Version))
	body.SetAttributeValue("plan_digest", cty.StringVal(i.PlanDigest))
	body.SetAttributeValue("source_digest", cty.StringVal(i.SourceDigest))
	body.SetAttributeValue("database", cty.StringVal(i.Database))
	body.SetAttributeValue("schema_plan_digest", cty.StringVal(i.PlanSummary.SchemaPlanDigest))
	body.SetAttributeValue("step_count", cty.NumberIntVal(int64(i.PlanSummary.StepCount)))
	body.SetAttributeValue("phase_count", cty.NumberIntVal(int64(i.PlanSummary.PhaseCount)))
	for _, routine := range i.Routines {
		block := hclwrite.NewBlock("routine_review", []string{routine.ResourceID})
		value := block.Body()
		value.SetAttributeValue("kind", cty.StringVal(routine.Kind))
		value.SetAttributeValue("schema", cty.StringVal(routine.Schema))
		value.SetAttributeValue("name", cty.StringVal(routine.Name))
		value.SetAttributeValue("signature", cty.StringVal(routine.Signature))
		value.SetAttributeValue("language", cty.StringVal(routine.Language))
		value.SetAttributeValue("source_digest", cty.StringVal(routine.SourceDigest))
		value.SetAttributeValue("digest_review_required", cty.BoolVal(routine.DigestReviewRequired))
		value.SetAttributeValue("unsafe_language_authorization_required", cty.BoolVal(routine.UnsafeLanguageAuthorizationRequired))
		value.SetAttributeValue("privileged_routine_authorization_required", cty.BoolVal(routine.PrivilegedRoutineAuthorizationRequired))
		value.SetAttributeValue("transaction_control_authorization_required", cty.BoolVal(routine.TransactionControlAuthorizationRequired))
		if len(routine.Dependencies) > 0 {
			values := make([]cty.Value, len(routine.Dependencies))
			for index, dependency := range routine.Dependencies {
				values[index] = cty.StringVal(dependency)
			}
			value.SetAttributeValue("dependencies", cty.ListVal(values))
		}
		if routine.Definition != "" {
			value.SetAttributeValue("definition", cty.StringVal(routine.Definition))
		}
		body.AppendNewline()
		body.AppendBlock(block)
	}
	for _, extension := range i.Extensions {
		block := hclwrite.NewBlock("extension_authorization", []string{extension.ResourceID})
		value := block.Body()
		value.SetAttributeValue("name", cty.StringVal(extension.Name))
		value.SetAttributeValue("version", cty.StringVal(extension.Version))
		value.SetAttributeValue("schema", cty.StringVal(extension.Schema))
		if len(extension.Requires) > 0 {
			values := make([]cty.Value, len(extension.Requires))
			for index, required := range extension.Requires {
				values[index] = cty.StringVal(required)
			}
			value.SetAttributeValue("requires", cty.ListVal(values))
		}
		value.SetAttributeValue("allowlist_required", cty.BoolVal(extension.AllowlistRequired))
		value.SetAttributeValue("exact_version_required", cty.BoolVal(extension.ExactVersionRequired))
		value.SetAttributeValue("schema_policy_required", cty.BoolVal(extension.SchemaPolicyRequired))
		value.SetAttributeValue("server_package_required", cty.BoolVal(extension.ServerPackageRequired))
		value.SetAttributeValue("trusted", cty.BoolVal(extension.Trusted))
		value.SetAttributeValue("superuser_required", cty.BoolVal(extension.SuperuserRequired))
		value.SetAttributeValue("untrusted_extension_authorization_required", cty.BoolVal(extension.UntrustedExtensionAuthorizationRequired))
		value.SetAttributeValue("relocatable", cty.BoolVal(extension.Relocatable))
		body.AppendNewline()
		body.AppendBlock(block)
	}
	file.Body().AppendBlock(root)
	return hclwrite.Format(file.Bytes()), nil
}

var canonicalSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func canonicalSHA256Digest(value string) bool { return canonicalSHA256Pattern.MatchString(value) }

// A trusted extension may be installed by a database owner with CREATE even
// when its control file is superuser-only. An extension whose script is not
// superuser-only never requires superuser, regardless of trusted metadata.
func extensionRequiresSuperuser(superuser, trusted bool) bool { return superuser && !trusted }

func cloneRenderOptions(values map[string]string) map[string]string {
	copy := make(map[string]string, len(values)+8)
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

func uniqueNonEmptySorted(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			set[value] = true
		}
	}
	values = values[:0]
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func dependencyContext(resource schema.Resource, resources map[string]schema.Resource) []string {
	var context []string
	for _, dependency := range resource.Dependencies {
		target, ok := resources[dependency.Target]
		if !ok {
			context = append(context, string(dependency.Type)+":"+dependency.Target)
			continue
		}
		context = append(context, string(dependency.Type)+":"+string(target.Kind)+":"+target.Name.String())
	}
	return uniqueNonEmptySorted(context)
}
