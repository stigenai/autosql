package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/source"
)

var planDatabaseBootstrap = postgres.PlanDatabaseBootstrap
var prepareBootstrapAuthorizationInventory = postgres.PrepareBootstrapAuthorizationInventory
var executeDatabaseBootstrapURL = postgres.ExecuteDatabaseBootstrapURL
var preflightExtensionReadinessURL = postgres.PreflightExtensionReadinessURL

func runDatabaseBootstrap(parent context.Context, args []string, output output, redactor *secret.Redactor) error {
	prepareOnly := len(args) > 0 && args[0] == "prepare"
	authorizeOnly := len(args) > 0 && args[0] == "authorize"
	preflightOnly := len(args) > 0 && args[0] == "preflight"
	if prepareOnly || authorizeOnly || preflightOnly {
		args = args[1:]
	}
	reviewOnly := prepareOnly || authorizeOnly || preflightOnly
	flags := newFlags("database bootstrap", output.streams.Err)
	file := flags.String("file", "", "HCL file containing one database block and the complete desired graph")
	maintenanceRef := flags.String("maintenance-url", "", "maintenance database URL secret reference")
	postgresVersion := flags.String("postgres-version", "", "target PostgreSQL major version")
	extensionAllowlist := flags.String("extension-allowlist", "", "comma-separated reviewed extension names")
	concurrentIndexes := flags.Bool("concurrent-indexes", true, "create standalone indexes concurrently")
	includeRoutineSource := flags.Bool("include-routine-source", false, "include routine definitions in prepare output")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum command duration")
	jsonMode := flags.Bool("json", false, "emit JSON envelope")
	hclMode := flags.Bool("hcl", false, "emit canonical HCL inventory in prepare mode")
	manifestPath := flags.String("authorization-manifest", "", "signed bootstrap authorization manifest")
	manifestPublicKey := flags.String("authorization-public-key", "", "Ed25519 public key secret reference")
	manifestIssuer := flags.String("authorization-issuer", "", "trusted authorization issuer")
	manifestSigner := flags.String("authorization-signer", "", "trusted authorization signer identity")
	manifestPurpose := flags.String("authorization-purpose", "bootstrap-authorization", "authorization signing-key purpose")
	signingKeyRef := flags.String("authorization-signing-key", "", "Ed25519 private signing key secret reference")
	signingKeyID := flags.String("authorization-key-id", "", "authorization signing key ID")
	validFor := flags.Duration("valid-for", time.Hour, "authorization validity duration")
	manifestOutput := flags.String("output", "", "authorization manifest output path")
	var routineDigests stringList
	var extensionVersions, extensionSchemas stringList
	flags.Var(&routineDigests, "reviewed-routine-digest", "reviewed routine body digest (repeatable)")
	flags.Var(&extensionVersions, "extension-version", "exact extension version authorization name=version (repeatable)")
	flags.Var(&extensionSchemas, "extension-schema", "reviewed extension target schema name=schema (repeatable)")
	if err := flags.Parse(args); err != nil {
		return usageError(err)
	}
	if flags.NArg() != 0 || *file == "" || *timeout <= 0 {
		return usageError(errors.New("--file and a positive --timeout are required"))
	}
	if *jsonMode && *hclMode {
		return usageError(errors.New("--json and --hcl are mutually exclusive"))
	}
	if (!reviewOnly || preflightOnly) && *maintenanceRef == "" {
		return usageError(errors.New("--maintenance-url is required for execution"))
	}
	if !prepareOnly && (*hclMode || *includeRoutineSource) {
		return usageError(errors.New("--hcl and --include-routine-source are valid only with database bootstrap prepare"))
	}
	if prepareOnly && *includeRoutineSource && !*jsonMode && !*hclMode {
		return usageError(errors.New("--include-routine-source requires --json or --hcl so source integrity is machine-verifiable"))
	}
	if authorizeOnly && (*signingKeyRef == "" || *signingKeyID == "" || *manifestIssuer == "" || *manifestSigner == "" || *manifestPurpose == "" || *manifestOutput == "" || *validFor <= 0) {
		return usageError(errors.New("authorize requires --authorization-signing-key, --authorization-key-id, --authorization-issuer, --authorization-signer, --authorization-purpose, --output, and positive --valid-for"))
	}
	output.json = *jsonMode
	raw, err := os.ReadFile(*file)
	if err != nil {
		return &Error{Kind: "config", Message: "read bootstrap HCL", Code: ExitConfig, Cause: err}
	}
	desired, err := source.LoadContext(parent, source.Input{URI: *file, Format: source.FormatHCLSource, Data: raw})
	if err != nil {
		return &Error{Kind: "config", Message: "load bootstrap HCL", Code: ExitConfig, Cause: err}
	}
	var databaseResources []schema.Resource
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			databaseResources = append(databaseResources, resource)
		}
	}
	if len(databaseResources) != 1 {
		return &Error{Kind: "config", Message: "bootstrap HCL must contain exactly one database block", Code: ExitConfig}
	}
	target, err := postgres.DatabaseTargetFromResource(databaseResources[0])
	if err != nil {
		return &Error{Kind: "validation", Message: err.Error(), Code: ExitValidation, Cause: err}
	}
	hclAuthorization, err := postgres.BootstrapAuthorizationReferenceFromResource(databaseResources[0])
	if err != nil {
		return &Error{Kind: "validation", Message: err.Error(), Code: ExitValidation, Cause: err}
	}
	if hclAuthorization != nil {
		if authorizeOnly {
			if *manifestIssuer != hclAuthorization.Issuer || *manifestSigner != hclAuthorization.Signer || *manifestPurpose != hclAuthorization.Purpose {
				return usageError(errors.New("authorize identity flags must match HCL bootstrap_authorization"))
			}
		} else if *manifestPath != "" || *manifestPublicKey != "" || *manifestIssuer != "" || *manifestSigner != "" {
			return usageError(errors.New("HCL bootstrap_authorization cannot be combined with authorization manifest flags"))
		}
		*manifestPath = hclAuthorization.Manifest
		*manifestPublicKey = hclAuthorization.PublicKey
		*manifestIssuer = hclAuthorization.Issuer
		*manifestSigner = hclAuthorization.Signer
		*manifestPurpose = hclAuthorization.Purpose
	}
	manifestSelected := *manifestPath != ""
	legacyAuthorizationSelected := routineDigests.set || strings.TrimSpace(*extensionAllowlist) != "" || extensionVersions.set || extensionSchemas.set
	if manifestSelected && (authorizeOnly && hclAuthorization == nil || legacyAuthorizationSelected) {
		return usageError(errors.New("bootstrap authorization manifest cannot be combined with authorize or legacy routine/extension authorization flags"))
	}
	if manifestSelected && (*manifestPublicKey == "" || *manifestIssuer == "" || *manifestSigner == "" || *manifestPurpose == "") {
		return usageError(errors.New("bootstrap authorization manifest requires public key, issuer, signer, and purpose"))
	}
	render := map[string]string{
		"postgres_version":         strings.TrimSpace(*postgresVersion),
		"extension_allowlist":      strings.TrimSpace(*extensionAllowlist),
		"reviewed_routine_digests": strings.Join(routineDigests.value(), ","),
	}
	versionPolicy, err := parseExtensionPolicyFlags(extensionVersions.value(), false)
	if err != nil {
		return usageError(err)
	}
	schemaPolicy, err := parseExtensionPolicyFlags(extensionSchemas.value(), true)
	if err != nil {
		return usageError(err)
	}
	for name, values := range versionPolicy {
		render["extension_version."+name] = values[0]
	}
	for name, values := range schemaPolicy {
		render["extension_schemas."+name] = strings.Join(values, ",")
	}
	if *concurrentIndexes {
		render["concurrent_indexes"] = "true"
	}
	if preflightOnly {
		reference := secret.Reference(*maintenanceRef)
		if err := reference.Validate(); err != nil {
			return &Error{Kind: "secret", Message: "--maintenance-url must be an env:// or file:// secret reference", Code: ExitSecret, Cause: err}
		}
		resolver := secret.NewResolver()
		resolver.Redactor = redactor
		ctx, cancel := context.WithTimeout(parent, *timeout)
		defer cancel()
		maintenanceURL, err := resolver.Resolve(ctx, reference)
		if err != nil {
			return &Error{Kind: "secret", Message: redactor.String(err.Error()), Code: ExitSecret, Cause: err}
		}
		allowed := map[string]bool{}
		for _, name := range splitCommaValues(*extensionAllowlist) {
			allowed[name] = true
		}
		report, err := preflightExtensionReadinessURL(ctx, maintenanceURL, target, desired, postgres.ExtensionPolicy{Allowed: allowed, Versions: versionPolicy, Schemas: schemaPolicy, AllowUntrusted: render["allow_untrusted_extensions"] == "true"})
		if err != nil {
			return &Error{Kind: "database", Message: redactor.String(err.Error()), Code: ExitConnection, Cause: err}
		}
		return output.success(report, humanExtensionReadiness(report))
	}
	if prepareOnly {
		inventory, err := prepareBootstrapAuthorizationInventory(parent, target, desired, postgres.BootstrapAuthorizationInventoryOptions{Render: render, IncludeRoutineSource: *includeRoutineSource})
		if err != nil {
			return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
		}
		if *hclMode {
			raw, err := inventory.MarshalHCL()
			if err != nil {
				return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
			}
			_, err = output.streams.Out.Write(raw)
			return err
		}
		if *includeRoutineSource {
			// The source bytes are explicitly requested and are integrity-bound
			// to source_digest. Broad text redaction would silently mutate valid
			// SQL and break that binding, so encode this credential-free model
			// directly rather than passing it through generic sanitization.
			return json.NewEncoder(output.streams.Out).Encode(Envelope{SchemaVersion: OutputSchemaVersion, Command: output.command, OK: true, Data: inventory})
		}
		return output.success(inventory, humanBootstrapAuthorizationInventory(inventory))
	}
	if authorizeOnly {
		inventory, err := prepareBootstrapAuthorizationInventory(parent, target, desired, postgres.BootstrapAuthorizationInventoryOptions{Render: render})
		if err != nil {
			return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
		}
		resolver := secret.NewResolver()
		resolver.Redactor = redactor
		private, err := resolveBootstrapAuthorizationKey(parent, resolver, *signingKeyRef, ed25519.PrivateKeySize)
		if err != nil {
			return &Error{Kind: "secret", Message: redactor.String(err.Error()), Code: ExitSecret, Cause: err}
		}
		now := time.Now().UTC()
		manifest, err := postgres.NewBootstrapAuthorizationManifest(inventory, now, now, now.Add(*validFor), *manifestIssuer, *manifestSigner, *manifestPurpose)
		if err == nil {
			err = manifest.Sign(*signingKeyID, ed25519.PrivateKey(private))
		}
		if err != nil {
			return &Error{Kind: "validation", Message: "create bootstrap authorization manifest", Code: ExitValidation, Cause: err}
		}
		encoded, err := manifest.MarshalCanonical()
		if err != nil {
			return &Error{Kind: "validation", Message: "encode bootstrap authorization manifest", Code: ExitValidation, Cause: err}
		}
		if err := os.WriteFile(*manifestOutput, encoded, 0o600); err != nil {
			return &Error{Kind: "config", Message: "write bootstrap authorization manifest", Code: ExitConfig, Cause: err}
		}
		return output.success(map[string]any{"status": "authorized", "manifest": *manifestOutput, "plan_digest": inventory.PlanDigest, "source_digest": inventory.SourceDigest, "expires_at": manifest.ExpiresAt}, "bootstrap authorization manifest created")
	}
	var whole bootstrap.Plan
	authorizedPlan := false
	if *manifestPath != "" {
		manifestBytes, err := resolveBootstrapAuthorizationArtifact(parent, redactor, *manifestPath)
		if err != nil {
			return &Error{Kind: "config", Message: "read bootstrap authorization manifest", Code: ExitConfig, Cause: err}
		}
		manifest, err := postgres.ParseBootstrapAuthorizationManifest(manifestBytes)
		if err != nil {
			return &Error{Kind: "validation", Message: "invalid bootstrap authorization manifest", Code: ExitValidation, Cause: err}
		}
		inventory, err := prepareBootstrapAuthorizationInventory(parent, target, desired, postgres.BootstrapAuthorizationInventoryOptions{Render: render})
		if err != nil {
			return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
		}
		resolver := secret.NewResolver()
		resolver.Redactor = redactor
		public, err := resolveBootstrapAuthorizationKey(parent, resolver, *manifestPublicKey, ed25519.PublicKeySize)
		if err != nil {
			return &Error{Kind: "secret", Message: redactor.String(err.Error()), Code: ExitSecret, Cause: err}
		}
		verified, err := postgres.VerifyBootstrapAuthorizationManifest(manifest, inventory, postgres.BootstrapAuthorizationVerifyPolicy{Now: time.Now, Keys: map[string]artifact.KeyRecord{manifest.Signature.KeyID: {PublicKey: ed25519.PublicKey(public), Issuer: *manifestIssuer, Identity: *manifestSigner, Purpose: *manifestPurpose, Status: "active", NotBefore: manifest.NotBefore, NotAfter: manifest.ExpiresAt}}, Issuer: *manifestIssuer, Signer: *manifestSigner, Purpose: *manifestPurpose})
		if err != nil {
			return &Error{Kind: "validation", Message: "bootstrap authorization verification failed", Code: ExitValidation, Cause: err}
		}
		whole, err = postgres.PlanDatabaseBootstrapAuthorized(parent, target, desired, plan.Options{Render: render}, verified)
		if err != nil {
			return &Error{Kind: "validation", Message: "bootstrap authorization plan binding failed", Code: ExitValidation, Cause: err}
		}
		authorizedPlan = true
	}
	if !authorizedPlan {
		whole, err = planDatabaseBootstrap(parent, target, desired, plan.Options{Render: render})
		if err != nil {
			return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
		}
	}
	reference := secret.Reference(*maintenanceRef)
	if err := reference.Validate(); err != nil {
		return &Error{Kind: "secret", Message: "--maintenance-url must be an env:// or file:// secret reference", Code: ExitSecret, Cause: err}
	}
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	maintenanceURL, err := resolver.Resolve(ctx, reference)
	if err != nil {
		return &Error{Kind: "secret", Message: redactor.String(err.Error()), Code: ExitSecret, Cause: err}
	}
	result, err := executeDatabaseBootstrapURL(ctx, maintenanceURL, whole, postgres.BootstrapExecutionHooks{})
	if err != nil {
		return &Error{Kind: "database", Message: redactor.String(err.Error()), Code: ExitConnection, Cause: err}
	}
	safe := struct {
		Status         string `json:"status"`
		PlanDigest     string `json:"plan_digest"`
		Database       string `json:"database"`
		Created        bool   `json:"created"`
		Resumed        bool   `json:"resumed"`
		AppliedSteps   int    `json:"applied_steps"`
		LastCheckpoint string `json:"last_checkpoint,omitempty"`
		LastConfirmed  string `json:"last_confirmed_step,omitempty"`
	}{"completed", result.PlanDigest, target.Name, result.CreatedDatabase, result.Resumed, result.AppliedSteps, result.LastCheckpoint, result.LastConfirmed}
	return output.success(safe, "database "+target.Name+" bootstrapped")
}

func resolveBootstrapAuthorizationArtifact(ctx context.Context, redactor *secret.Redactor, reference string) ([]byte, error) {
	if strings.HasPrefix(reference, "file://") {
		ref := secret.Reference(reference)
		if err := ref.Validate(); err != nil {
			return nil, err
		}
		parsed, _ := url.Parse(reference)
		value, err := os.ReadFile(parsed.Path)
		if err == nil && redactor != nil {
			redactor.Add(string(value))
		}
		return value, err
	}
	if strings.HasPrefix(reference, "env://") {
		resolver := secret.NewResolver()
		resolver.Redactor = redactor
		value, err := resolver.Resolve(ctx, secret.Reference(reference))
		return []byte(value), err
	}
	return os.ReadFile(reference)
}

func resolveBootstrapAuthorizationKey(ctx context.Context, resolver *secret.Resolver, reference string, size int) ([]byte, error) {
	ref := secret.Reference(reference)
	if err := ref.Validate(); err != nil {
		return nil, errors.New("authorization key must be an env:// or file:// secret reference")
	}
	value, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawStdEncoding.Strict().DecodeString(strings.TrimSpace(value))
	if err != nil {
		raw, err = base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(value))
	}
	if err != nil || len(raw) != size {
		return nil, errors.New("authorization key has invalid encoding or length")
	}
	return raw, nil
}

func humanBootstrapAuthorizationInventory(inventory postgres.BootstrapAuthorizationInventory) string {
	var out strings.Builder
	fmt.Fprintf(&out, "bootstrap authorization inventory for database %s\nplan digest: %s\n", inventory.Database, inventory.PlanDigest)
	fmt.Fprintf(&out, "routine reviews (%d):\n", len(inventory.Routines))
	for _, routine := range inventory.Routines {
		fmt.Fprintf(&out, "- %s %s language=%s digest=%s", routine.Kind, routine.Signature, routine.Language, routine.SourceDigest)
		var gates []string
		if routine.UnsafeLanguageAuthorizationRequired {
			gates = append(gates, "unsafe_language")
		}
		if routine.PrivilegedRoutineAuthorizationRequired {
			gates = append(gates, "privileged_routine")
		}
		if routine.TransactionControlAuthorizationRequired {
			gates = append(gates, "transaction_control")
		}
		if len(gates) > 0 {
			fmt.Fprintf(&out, " additional_authorizations=%s", strings.Join(gates, ","))
		}
		if len(routine.Dependencies) > 0 {
			fmt.Fprintf(&out, " dependencies=%s", strings.Join(routine.Dependencies, ","))
		}
		out.WriteByte('\n')
	}
	fmt.Fprintf(&out, "extension authorizations (%d):\n", len(inventory.Extensions))
	for _, extension := range inventory.Extensions {
		fmt.Fprintf(&out, "- %s version=%s schema=%s allowlist=required server_package=required", extension.Name, extension.Version, extension.Schema)
		if extension.UntrustedExtensionAuthorizationRequired {
			out.WriteString(" additional_authorization=untrusted_extension")
		}
		if extension.SuperuserRequired {
			out.WriteString(" authority=superuser")
		}
		if len(extension.Requires) > 0 {
			fmt.Fprintf(&out, " requires=%s", strings.Join(extension.Requires, ","))
		}
		out.WriteByte('\n')
	}
	return strings.TrimSpace(out.String())
}

func parseExtensionPolicyFlags(values []string, multiple bool) (map[string][]string, error) {
	result := map[string][]string{}
	for _, value := range values {
		name, setting, ok := strings.Cut(value, "=")
		name, setting = strings.TrimSpace(name), strings.TrimSpace(setting)
		if !ok || name == "" || setting == "" {
			return nil, errors.New("extension policy flags require name=value")
		}
		if !multiple && len(result[name]) != 0 {
			return nil, fmt.Errorf("extension %s has more than one exact version pin", name)
		}
		result[name] = append(result[name], setting)
	}
	for name := range result {
		sort.Strings(result[name])
		result[name] = uniqueCLIStrings(result[name])
	}
	return result, nil
}

func splitCommaValues(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return uniqueCLIStrings(out)
}

func uniqueCLIStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func humanExtensionReadiness(report postgres.ExtensionReadinessReport) string {
	var out strings.Builder
	fmt.Fprintf(&out, "extension readiness: ready=%t postgres=%d\n", report.Ready, report.ServerMajor)
	for _, extension := range report.Extensions {
		fmt.Fprintf(&out, "- %s version=%s schema=%s status=%s\n  reason: %s\n  remediation: %s\n", extension.Name, extension.RequestedVersion, extension.RequestedSchema, extension.Status, extension.Reason, extension.Remediation)
	}
	return strings.TrimSpace(out.String())
}
