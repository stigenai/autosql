package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/executor"
	"autosql/pkg/migrate"
	"autosql/pkg/policy"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/simulate"
	"autosql/pkg/source"
)

type operatorPublishConfig struct {
	DevelopmentURL, ProductionURL                       secret.Reference
	Environment, DatabaseIdentity, Author, Requester    string
	PostgresVersion                                     int
	ConcurrentIndexes                                   *bool
	Schemas                                             []string
	PolicyFile, PolicyIdentity                          string
	ApprovalPolicy                                      approval.Policy
	AutomationApprovalKeyID, AutomationApprovalIdentity string
	AutomationApprovalRoles                             []string
	AutomationApprovalPrivateKeyReference               string
	ApprovalTTL, ArtifactLifetime                       string
	GenerationApprovalAuditPath                         string
	ApprovalAuditPath, LifecycleAuditPath               string
	GeneratorKeyID, GeneratorPurpose                    string
	GeneratorPrivateKeyReference                        string
	SigningKeyID, SigningIssuer, SigningIdentity        string
	SigningPurpose, SigningStatus                       string
	SigningPrivateKeyReference                          string
	SigningNotBefore, SigningNotAfter                   time.Time
	OperatorArtifactDirectory                           string
	Metadata                                            map[string]string
}

type operatorReleaseManifest struct {
	Version           string   `json:"version"`
	ArtifactDigest    string   `json:"artifact_digest"`
	RegistryDigest    string   `json:"registry_digest"`
	ArtifactFile      string   `json:"artifact_file"`
	ApplyConfigFile   string   `json:"apply_config_file"`
	OCITag            string   `json:"oci_tag"`
	Environment       string   `json:"environment"`
	DatabaseIdentity  string   `json:"database_identity"`
	SourceRevision    string   `json:"source_revision"`
	Schemas           []string `json:"schemas"`
	PostgresVersion   int      `json:"postgres_version"`
	ConcurrentIndexes bool     `json:"concurrent_indexes"`
	AdoptionPolicy    string   `json:"adoption_policy,omitempty"`
}

func runOperatorArtifactPublish(ctx context.Context, args []string, o output, reader ReadPlanService) error {
	fs := newFlags("operator artifact publish", o.streams.Err)
	file := fs.String("file", "", "desired HCL file")
	configPath := fs.String("config", "", "trusted operator publishing configuration")
	outputDir := fs.String("output-dir", "", "new release bundle directory")
	sourceRevision := fs.String("source-revision", "", "immutable source revision (for example the Git commit SHA)")
	bootstrapMode := fs.Bool("bootstrap", false, "build an empty-database bootstrap artifact")
	adoptMode := fs.Bool("adopt", false, "build a credential-free adoption artifact for an equivalent existing database")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *file == "" || *configPath == "" || *outputDir == "" || *sourceRevision == "" {
		return usageError(errors.New("--file, --config, --output-dir, and --source-revision are required"))
	}
	if *bootstrapMode && *adoptMode {
		return usageError(errors.New("--bootstrap and --adopt are mutually exclusive"))
	}
	if reader == nil {
		return &Error{Kind: "config", Message: "schema reader is unavailable", Code: ExitConfig}
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return &Error{Kind: "config", Message: "read operator publish configuration failed", Code: ExitConfig}
	}
	var config operatorPublishConfig
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		return &Error{Kind: "config", Message: "parse operator publish configuration failed", Code: ExitConfig}
	}
	if err = validateOperatorPublishConfig(config, *bootstrapMode, *adoptMode); err != nil {
		return &Error{Kind: "config", Message: err.Error(), Code: ExitConfig}
	}
	policyRaw, err := os.ReadFile(config.PolicyFile)
	if err != nil {
		return &Error{Kind: "config", Message: "read operator publishing policy failed", Code: ExitConfig}
	}
	policyDocument, err := policy.Parse(policyRaw)
	if err != nil {
		return &Error{Kind: "config", Message: "parse operator publishing policy failed", Code: ExitConfig}
	}
	desiredRaw, err := os.ReadFile(*file)
	if err != nil {
		return &Error{Kind: "config", Message: "read desired operator schema failed", Code: ExitConfig}
	}
	desired, err := source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: source.FormatHCLSource, Data: desiredRaw})
	if err != nil {
		return &Error{Kind: "validation", Message: "load desired operator schema failed", Code: ExitValidation, Cause: err}
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		return &Error{Kind: "validation", Message: "normalize desired operator schema failed", Code: ExitValidation, Cause: err}
	}
	configuredSchemas := map[string]bool{}
	for _, name := range config.Schemas {
		configuredSchemas[name] = true
	}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindSchema && !configuredSchemas[resource.Name.Name] {
			return &Error{Kind: "config", Message: "Schemas must include every schema declared by the HCL", Code: ExitConfig}
		}
		if resource.Kind == schema.KindDatabase {
			target, targetErr := postgres.DatabaseTargetFromResource(resource)
			if targetErr != nil || target.Name != config.DatabaseIdentity {
				return &Error{Kind: "config", Message: "DatabaseIdentity must equal the database block name", Code: ExitConfig}
			}
		}
	}
	resolver := secret.NewResolver()
	developmentURL, err := resolver.Resolve(ctx, config.DevelopmentURL)
	if err != nil {
		return &Error{Kind: "secret", Message: "resolve development database failed", Code: ExitSecret}
	}
	developmentIdentity, err := simulate.ResolvePostgresIdentity(ctx, developmentURL)
	if err != nil {
		return &Error{Kind: "connection", Message: "resolve development database identity failed", Code: ExitConnection}
	}
	productionIdentity := "operator-bootstrap/" + config.DatabaseIdentity
	var current schema.Document
	var bootstrapTarget *bootstrap.DatabaseTarget
	if *bootstrapMode {
		target, targetErr := operatorDatabaseTarget(desired)
		if targetErr != nil {
			return &Error{Kind: "config", Message: targetErr.Error(), Code: ExitConfig}
		}
		if config.DatabaseIdentity != target.Name {
			return &Error{Kind: "config", Message: "bootstrap DatabaseIdentity must equal the database block name", Code: ExitConfig}
		}
		bootstrapTarget = &target
	} else if *adoptMode {
		productionIdentity = "operator-adopt/" + config.DatabaseIdentity
		current = desired
	} else {
		productionURL, resolveErr := resolver.Resolve(ctx, config.ProductionURL)
		if resolveErr != nil {
			return &Error{Kind: "secret", Message: "resolve production database failed", Code: ExitSecret}
		}
		productionIdentity, err = simulate.ResolvePostgresIdentity(ctx, productionURL)
		if err != nil {
			return &Error{Kind: "connection", Message: "resolve production database identity failed", Code: ExitConnection}
		}
		current, err = postgres.InspectURL(ctx, productionURL, postgres.Options{Schemas: config.Schemas})
		if err != nil {
			return &Error{Kind: "connection", Message: "inspect production database failed", Code: ExitConnection}
		}
		// The executor's migration-history relation is runtime bookkeeping,
		// never part of the desired schema; without this the transition plan
		// would drop it on every publish after the first verified apply.
		current = executor.ExcludeBookkeeping(current)
	}
	generatorKey, err := resolveOperatorPrivateKey(ctx, resolver, config.GeneratorPrivateKeyReference, "generator")
	if err != nil {
		return err
	}
	signingKey, err := resolveOperatorPrivateKey(ctx, resolver, config.SigningPrivateKeyReference, "release signing")
	if err != nil {
		return err
	}
	approvalKey, err := resolveOperatorPrivateKey(ctx, resolver, config.AutomationApprovalPrivateKeyReference, "automation approval")
	if err != nil {
		return err
	}
	lifetime, _ := time.ParseDuration(config.ArtifactLifetime)
	approvalTTL, _ := time.ParseDuration(config.ApprovalTTL)
	now := time.Now().UTC()
	if now.Before(config.SigningNotBefore.UTC()) || !now.Before(config.SigningNotAfter.UTC()) || now.Add(lifetime).After(config.SigningNotAfter.UTC()) {
		return &Error{Kind: "config", Message: "release key validity window must contain the complete artifact lifetime", Code: ExitConfig}
	}
	if err = os.MkdirAll(filepath.Dir(config.GenerationApprovalAuditPath), 0o700); err != nil {
		return &Error{Kind: "config", Message: "create generation approval audit directory failed", Code: ExitConfig}
	}
	generation := migrate.GenerateRequest{Desired: desired, DevelopmentURL: developmentURL, DevelopmentIdentity: developmentIdentity, ProductionIdentity: productionIdentity, Environment: config.Environment, DatabaseIdentity: config.DatabaseIdentity, SourceRevision: *sourceRevision, Author: config.Author, Requester: config.Requester, PostgresVersion: config.PostgresVersion, Policy: *policyDocument, PolicyIdentity: config.PolicyIdentity, ApprovalPolicy: config.ApprovalPolicy, ApprovalProvider: migrate.AutomationApprovalProvider{KeyID: config.AutomationApprovalKeyID, Identity: approval.Identity{ID: config.AutomationApprovalIdentity, Roles: append([]string(nil), config.AutomationApprovalRoles...)}, Actors: []approval.Identity{{ID: config.Author}, {ID: config.Requester}}, PrivateKey: approvalKey, TTL: approvalTTL}, ApprovalAudit: &approval.Chain{Sink: &approval.FileSink{Path: config.GenerationApprovalAuditPath}}, CreatedAt: now, ExpiresAt: now.Add(lifetime), GeneratorKeyID: config.GeneratorKeyID, GeneratorPurpose: config.GeneratorPurpose, SigningKeyID: config.SigningKeyID, GeneratorPrivateKey: generatorKey, SigningPrivateKey: signingKey, Metadata: config.Metadata}
	render := map[string]string{"postgres_version": fmt.Sprint(config.PostgresVersion)}
	if *config.ConcurrentIndexes {
		render["concurrent_indexes"] = "true"
	}
	result, err := (migrate.GenerateService{}).BuildOperatorArtifact(ctx, migrate.OperatorArtifactRequest{Generation: generation, Current: current, Desired: desired, BootstrapTarget: bootstrapTarget, Adopt: *adoptMode, Render: render})
	if err != nil {
		return &Error{Kind: "migration", Message: err.Error(), Code: ExitMigration, Cause: err}
	}
	contexts, attestations := map[string]string{}, map[string]artifact.ValidationAttestation{}
	for _, attestation := range result.Artifact.ValidationAttestations {
		contexts[attestation.Stage] = attestation.ConfigDigest
		attestations[attestation.Stage] = attestation
	}
	trust := migrationTrust{Expected: artifact.ExpectedBindings{PlanDigest: result.Artifact.Plan.Digest, GeneratedPlanDigest: result.Artifact.Plan.Digest, ChecksDigest: result.Artifact.Checks.Digest, GuardrailDigest: result.Artifact.GuardrailDigest, SourceRevision: result.Artifact.SourceRevision, Environment: result.Artifact.TargetEnvironment, DatabaseIdentity: result.Artifact.DatabaseIdentity, ApprovalIdentity: result.Artifact.Approval.Identity, ApprovalProofDigest: result.Artifact.Approval.ProofDigest}, ValidationContextDigests: contexts, ValidationAttestations: attestations, Schemas: append([]string(nil), config.Schemas...), Policy: *policyDocument, PolicyIdentity: config.PolicyIdentity, SchemaPolicyResources: result.SchemaPolicyResources, MigrationPolicyResources: result.MigrationPolicyResources, ApprovalIdentities: map[string]approval.Identity{result.Artifact.Approval.ProofDigest: {ID: config.AutomationApprovalIdentity, Roles: append([]string(nil), config.AutomationApprovalRoles...)}}}
	apply := applyConfig{DatabaseURL: string(config.ProductionURL), Environment: config.Environment, KeyID: config.SigningKeyID, PublicKey: base64.RawStdEncoding.EncodeToString(signingKey.Public().(ed25519.PublicKey)), Issuer: config.SigningIssuer, Signer: config.SigningIdentity, Author: config.Author, Requester: config.Requester, ApprovalAuditPath: config.ApprovalAuditPath, LifecycleAuditPath: config.LifecycleAuditPath, ArtifactDirectory: config.OperatorArtifactDirectory, PostgresVersion: config.PostgresVersion, KeyStatus: config.SigningStatus, KeyPurpose: config.SigningPurpose, KeyNotBefore: config.SigningNotBefore.UTC(), KeyNotAfter: config.SigningNotAfter.UTC(), NoEdits: true, GeneratorKeyID: config.GeneratorKeyID, GeneratorPublicKey: base64.RawStdEncoding.EncodeToString(generatorKey.Public().(ed25519.PublicKey)), GeneratorPurpose: config.GeneratorPurpose, TrustedMigrations: map[string]migrationTrust{result.Artifact.Digest: trust}, ApprovalPolicy: config.ApprovalPolicy}
	manifest := operatorReleaseManifest{Version: "autosql.operator-release/v1", ArtifactDigest: result.Artifact.Digest, RegistryDigest: result.Artifact.Digest, ArtifactFile: filepath.ToSlash(filepath.Join("artifacts", result.Artifact.Digest+".json")), ApplyConfigFile: "apply-config.json", OCITag: strings.Replace(result.Artifact.Digest, ":", "-", 1), Environment: config.Environment, DatabaseIdentity: config.DatabaseIdentity, SourceRevision: *sourceRevision, Schemas: append([]string(nil), config.Schemas...), PostgresVersion: config.PostgresVersion, ConcurrentIndexes: *config.ConcurrentIndexes}
	if *adoptMode {
		manifest.AdoptionPolicy = "IfEquivalent"
	}
	if err = writeOperatorReleaseBundle(*outputDir, result.Bytes, apply, manifest); err != nil {
		return &Error{Kind: "conflict", Message: "publish operator release bundle failed", Code: ExitConflict, Cause: err}
	}
	o.json = *jsonFlag
	return o.success(manifest, result.Artifact.Digest)
}

func validateOperatorPublishConfig(config operatorPublishConfig, bootstrapMode, adoptMode bool) error {
	approvalTTL, approvalErr := time.ParseDuration(config.ApprovalTTL)
	lifetime, lifetimeErr := time.ParseDuration(config.ArtifactLifetime)
	if bootstrapMode && adoptMode || config.DevelopmentURL.Validate() != nil || (!bootstrapMode && !adoptMode && config.ProductionURL.Validate() != nil) || config.Environment == "" || config.DatabaseIdentity == "" || config.Author == "" || config.Requester == "" || config.Author == config.Requester || config.PostgresVersion < 14 || config.PostgresVersion > 18 || config.ConcurrentIndexes == nil || len(config.Schemas) == 0 || config.PolicyFile == "" || config.PolicyIdentity == "" || len(config.ApprovalPolicy.Environments) == 0 || config.AutomationApprovalKeyID == "" || config.AutomationApprovalIdentity == "" || config.AutomationApprovalIdentity == config.Author || config.AutomationApprovalIdentity == config.Requester || len(config.AutomationApprovalRoles) == 0 || config.AutomationApprovalPrivateKeyReference == "" || approvalErr != nil || approvalTTL <= 0 || lifetimeErr != nil || lifetime <= 0 || approvalTTL >= lifetime || config.GenerationApprovalAuditPath == "" || config.ApprovalAuditPath == "" || config.LifecycleAuditPath == "" || config.GeneratorKeyID == "" || config.GeneratorPurpose == "" || config.GeneratorPrivateKeyReference == "" || config.SigningKeyID == "" || config.SigningIssuer == "" || config.SigningIdentity == "" || config.SigningPurpose == "" || config.SigningStatus != "active" || config.SigningPrivateKeyReference == "" || config.SigningNotBefore.IsZero() || !config.SigningNotAfter.After(config.SigningNotBefore) || config.OperatorArtifactDirectory == "" {
		return errors.New("operator publish configuration is incomplete or invalid")
	}
	if config.GeneratorKeyID == config.SigningKeyID || config.GeneratorPurpose == config.SigningPurpose || config.AutomationApprovalKeyID == config.GeneratorKeyID || config.AutomationApprovalKeyID == config.SigningKeyID {
		return errors.New("approval, generator, and release key IDs and purposes must be role-distinct")
	}
	if _, ok := config.ApprovalPolicy.Environments[config.Environment]; !ok {
		return errors.New("approval policy must define the configured environment")
	}
	return nil
}

func resolveOperatorPrivateKey(ctx context.Context, resolver *secret.Resolver, reference, name string) (ed25519.PrivateKey, error) {
	value, err := resolver.Resolve(ctx, secret.Reference(reference))
	if err != nil {
		return nil, &Error{Kind: "secret", Message: "resolve " + name + " private key failed", Code: ExitSecret}
	}
	key, err := decodePrivate(value)
	if err != nil {
		return nil, &Error{Kind: "config", Message: "decode " + name + " private key failed", Code: ExitConfig}
	}
	return key, nil
}

func operatorDatabaseTarget(document schema.Document) (bootstrap.DatabaseTarget, error) {
	var target bootstrap.DatabaseTarget
	count := 0
	for _, resource := range document.Graph.Resources {
		if resource.Kind != schema.KindDatabase {
			continue
		}
		count++
		var err error
		target, err = postgres.DatabaseTargetFromResource(resource)
		if err != nil {
			return target, err
		}
	}
	if count != 1 {
		return target, errors.New("bootstrap publishing requires exactly one database block")
	}
	return target, nil
}

func writeOperatorReleaseBundle(directory string, artifactBytes []byte, apply applyConfig, manifest operatorReleaseManifest) error {
	parent := filepath.Dir(filepath.Clean(directory))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, ".autosql-operator-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	artifacts := filepath.Join(temporary, "artifacts")
	if err = os.Mkdir(artifacts, 0o700); err != nil {
		return err
	}
	applyBytes, err := json.MarshalIndent(apply, "", "  ")
	if err != nil {
		return err
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	files := map[string][]byte{filepath.Join(artifacts, manifest.ArtifactDigest+".json"): artifactBytes, filepath.Join(temporary, "apply-config.json"): append(applyBytes, '\n'), filepath.Join(temporary, "release.json"): append(manifestBytes, '\n')}
	for path, data := range files {
		if err = os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
	}
	return os.Rename(temporary, directory)
}
