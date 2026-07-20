package operatorcontroller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autosql/internal/cli"
	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/executor"
	"autosql/pkg/operator"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/source"
	"github.com/jackc/pgx/v5"
)

var executeOperatorDatabaseBootstrapURL = postgres.ExecuteDatabaseBootstrapURL
var parseOperatorArtifact = artifact.Parse
var verifyOperatorDeclarativeResource = verifyDeclarativeResource
var verifyOperatorAdoptionResource = verifyAdoptionResource
var verifyOperatorReleaseBeforeReferences = verifyOperatorReleaseArtifact

var verifyProductionOperatorArtifact = func(databaseURL string, parsed artifact.Artifact) (artifact.Artifact, error) {
	_ = databaseURL // The release policy is credential-free.
	// Whole-database bootstrap is an operator-controlled apply boundary. Its
	// release artifact policy is therefore unconditionally no-edits, exactly
	// like the generic ApplyRequest{NoEdits:true} path, even if production
	// configuration attempts to opt out.
	verified, err := cli.VerifyProductionArtifactForApply(parsed, true)
	if err != nil {
		return artifact.Artifact{}, err
	}
	return verified.Payload()
}

func loadOperatorArtifact(digest string) (artifact.Artifact, error) {
	if digest == "" || !strings.HasPrefix(digest, "sha256:") {
		return artifact.Artifact{}, errors.New("operator migration requires a sha256 artifact digest")
	}
	directory := strings.TrimSpace(os.Getenv("AUTOSQL_OPERATOR_ARTIFACT_DIR"))
	if directory == "" {
		return artifact.Artifact{}, errors.New("AUTOSQL_OPERATOR_ARTIFACT_DIR is required")
	}
	raw, err := os.ReadFile(filepath.Join(directory, digest+".json"))
	if err != nil {
		return artifact.Artifact{}, errors.New("operator artifact unavailable")
	}
	a, err := parseOperatorArtifact(raw)
	if err != nil {
		return artifact.Artifact{}, errors.New("operator artifact invalid")
	}
	if a.Digest != digest {
		return artifact.Artifact{}, errors.New("operator artifact digest mismatch")
	}
	return a, nil
}

func verifyOperatorReleaseArtifact(digest string) (artifact.VerifiedArtifact, error) {
	a, err := loadOperatorArtifact(digest)
	if err != nil {
		return artifact.VerifiedArtifact{}, err
	}
	verified, err := cli.VerifyProductionArtifactForApply(a, true)
	if err != nil {
		return artifact.VerifiedArtifact{}, errors.New("operator artifact verification failed")
	}
	return verified, nil
}

type verifiedBootstrapPlan struct {
	Plan         bootstrap.Plan
	SourceDigest string
	ExpiresAt    time.Time
	Authorized   bool
}

// ArtifactApply resolves an immutable artifact from the mounted operator
// artifact directory and delegates to the same verified production service as
// the CLI. No raw SQL or database URL is accepted from the CR.
func ArtifactApply(ctx context.Context, resource operator.Resource, digest string) (operator.ApplyResult, error) {
	var a artifact.Artifact
	verifiedBeforeReferences, verifiedErr := resource.VerifiedReleaseArtifact.Payload()
	if verifiedErr == nil {
		if verifiedBeforeReferences.Digest != digest {
			return operator.ApplyResult{}, errors.New("operator verified artifact digest mismatch")
		}
		a = verifiedBeforeReferences
	} else {
		var err error
		a, err = loadOperatorArtifact(digest)
		if err != nil {
			return operator.ApplyResult{}, err
		}
	}
	if resource.Spec.AdoptionPolicy == operator.AdoptIfEquivalent {
		if verifiedErr != nil {
			verifiedArtifact, err := verifyProductionOperatorArtifact(resource.ResolvedDatabaseURL, a)
			if err != nil || verifiedArtifact.Digest != a.Digest || verifiedArtifact.Plan.Digest != a.Plan.Digest {
				return operator.ApplyResult{}, errors.New("operator artifact verification failed")
			}
			a = verifiedArtifact
		}
		adoption, err := verifyOperatorAdoptionResource(ctx, resource, a)
		outcome := operator.ApplyResult{Status: "adopted", PlanDigest: a.Plan.Digest, SourceDigest: adoption.SourceDigest, TargetIdentity: a.DatabaseIdentity, AppliedSteps: 0}
		if err != nil {
			outcome.Status = "failed"
			return outcome, err
		}
		return outcome, nil
	}
	if resource.Spec.Kind == operator.Declarative && resource.ResolvedSource != "" &&
		(resource.Spec.Source.Inline != "" || resource.Spec.Source.SecretRef != nil || resource.Spec.Source.ConfigMapRef != nil) {
		bootstrapResult, err := verifyOperatorDeclarativeResource(ctx, resource, a)
		if err != nil {
			return operator.ApplyResult{PlanDigest: a.Plan.Digest, TargetIdentity: a.DatabaseIdentity}, err
		}
		if resource.Spec.DatabaseTarget != nil {
			if !artifactIdentityMatchesBootstrapTarget(a.DatabaseIdentity, bootstrapResult.Plan.Target) {
				return operator.ApplyResult{PlanDigest: bootstrapResult.Plan.Digest, SourceDigest: bootstrapResult.SourceDigest, TargetIdentity: bootstrapResult.Plan.Target.Name}, errors.New("operator artifact database identity does not match bootstrap target")
			}
			if strings.TrimSpace(resource.ResolvedDatabaseURL) == "" || strings.TrimSpace(resource.ResolvedMaintenanceDatabaseURL) == "" {
				return operator.ApplyResult{}, errors.New("operator database references are unresolved")
			}
			if verifiedErr != nil {
				verifiedArtifact, err := verifyProductionOperatorArtifact(resource.ResolvedDatabaseURL, a)
				if err != nil || verifiedArtifact.Digest != a.Digest || verifiedArtifact.Plan.Digest != a.Plan.Digest || !artifactIdentityMatchesBootstrapTarget(verifiedArtifact.DatabaseIdentity, bootstrapResult.Plan.Target) {
					return operator.ApplyResult{}, errors.New("operator artifact verification failed")
				}
			}
			execution, err := executeOperatorDatabaseBootstrapURL(ctx, resource.ResolvedMaintenanceDatabaseURL, bootstrapResult.Plan, postgres.BootstrapExecutionHooks{})
			status := "applied"
			if execution.Completed && execution.AppliedSteps == 0 {
				status = "no_op"
			}
			outcome := operator.ApplyResult{Status: status, PlanDigest: bootstrapResult.Plan.Digest, SourceDigest: bootstrapResult.SourceDigest, TargetIdentity: bootstrapResult.Plan.Target.Name, ExecutionID: bootstrapResult.Plan.Digest, PendingStep: execution.PendingStep, RecoveryGuidance: execution.RecoveryGuidance, AppliedSteps: execution.AppliedSteps, AuthorizationExpiresAt: bootstrapResult.ExpiresAt}
			if bootstrapResult.Authorized {
				outcome.AuthorizationState = operator.AuthorizationAccepted
			}
			if err != nil {
				return outcome, err
			}
			return outcome, nil
		}
	}
	if strings.TrimSpace(resource.ResolvedDatabaseURL) == "" {
		return operator.ApplyResult{}, errors.New("operator database reference is unresolved")
	}
	directory := strings.TrimSpace(os.Getenv("AUTOSQL_OPERATOR_ARTIFACT_DIR"))
	artifactPath := filepath.Join(directory, digest+".json")
	services, err := cli.ProductionServicesForURL(resource.ResolvedDatabaseURL)
	if err != nil {
		return operator.ApplyResult{}, fmt.Errorf("load production apply configuration: %w", err)
	}
	if services.Apply == nil {
		return operator.ApplyResult{}, errors.New("production artifact apply service is unavailable")
	}
	result, err := services.Apply.Apply(ctx, cli.ApplyRequest{
		ArtifactPath: artifactPath,
		ApprovalMode: "artifact",
		NoEdits:      true,
	})
	outcome := operator.ApplyResult{Status: result.Status, PlanDigest: a.Plan.Digest, TargetIdentity: a.DatabaseIdentity, ExecutionID: result.ExecutionID, PendingStep: result.PendingStep, RecoveryGuidance: result.RecoveryGuidance, AppliedSteps: result.AppliedSteps}
	if err != nil {
		if result.Status == "uncertain" {
			outcome.Status = "uncertain"
		}
		return outcome, err
	}
	if result.Status != "success" && result.Status != "applied" && result.Status != "no_op" {
		return outcome, fmt.Errorf("artifact apply returned status %q", result.Status)
	}
	return outcome, nil
}

type verifiedAdoption struct {
	SourceDigest string
}

// verifyAdoptionResource proves equivalence without executing application SQL.
// The target advisory lock orders cooperating AutoSQL writers, while ACCESS
// SHARE locks fence DDL against every existing selected relation through the
// final catalog inspection and transaction commit.
func verifyAdoptionResource(ctx context.Context, resource operator.Resource, approved artifact.Artifact) (verifiedAdoption, error) {
	if resource.Spec.AdoptionPolicy != operator.AdoptIfEquivalent || resource.Spec.Kind != operator.Declarative {
		return verifiedAdoption{}, errors.New("operator adoption requires DeclarativeSchema IfEquivalent mode")
	}
	if strings.TrimSpace(resource.ResolvedDatabaseURL) == "" || strings.TrimSpace(resource.ResolvedSource) == "" {
		return verifiedAdoption{}, errors.New("operator adoption references are unresolved")
	}
	desired, err := loadOperatorDeclarativeSource(ctx, resource.ResolvedSource, resource.Spec.Source.Format)
	if err != nil {
		return verifiedAdoption{}, err
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		return verifiedAdoption{}, errors.New("normalize adoption desired schema")
	}
	schemas, err := adoptionSchemas(desired)
	if err != nil {
		return verifiedAdoption{}, err
	}
	sourceDigest, err := schema.SemanticFingerprint(desired)
	if err != nil {
		return verifiedAdoption{}, errors.New("fingerprint adoption desired schema")
	}
	if err = requireNoOpAdoptionPlan(approved.Plan); err != nil {
		return verifiedAdoption{}, err
	}
	lockKey, err := executor.LockKey(approved.DatabaseIdentity, approved.TargetEnvironment)
	if err != nil {
		return verifiedAdoption{}, errors.New("operator adoption artifact target is invalid")
	}
	conn, err := pgx.Connect(ctx, resource.ResolvedDatabaseURL)
	if err != nil {
		return verifiedAdoption{}, errors.New("connect adoption target")
	}
	defer conn.Close(context.WithoutCancel(ctx))
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return verifiedAdoption{}, errors.New("begin adoption inspection")
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var locked bool
	if err = tx.QueryRow(ctx, `select pg_try_advisory_xact_lock(hashtextextended($1,0::bigint))`, lockKey).Scan(&locked); err != nil {
		return verifiedAdoption{}, errors.New("lock adoption target")
	}
	if !locked {
		return verifiedAdoption{}, errors.New("operator adoption target is busy")
	}
	if err = lockAdoptionRelations(ctx, tx, schemas); err != nil {
		return verifiedAdoption{}, err
	}
	generated, err := buildAdoptionPlan(ctx, tx, schemas, desired, resource.Spec)
	if err != nil {
		return verifiedAdoption{}, err
	}
	if generated.Digest != approved.Plan.Digest {
		return verifiedAdoption{}, errors.New("live database does not match desired schema; adoption refused")
	}
	if err = requireNoOpAdoptionPlan(generated); err != nil {
		return verifiedAdoption{}, errors.New("live database does not match desired schema; adoption refused")
	}
	final, err := buildAdoptionPlan(ctx, tx, schemas, desired, resource.Spec)
	if err != nil {
		return verifiedAdoption{}, err
	}
	if final.Digest != generated.Digest || final.Digest != approved.Plan.Digest {
		return verifiedAdoption{}, errors.New("database schema changed during adoption; retry from a stable state")
	}
	if err = tx.Commit(ctx); err != nil {
		return verifiedAdoption{}, errors.New("commit adoption evidence")
	}
	return verifiedAdoption{SourceDigest: sourceDigest}, nil
}

func adoptionSchemas(desired schema.Document) ([]string, error) {
	seen := map[string]bool{}
	var schemas []string
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			return nil, errors.New("operator adoption does not accept a database block")
		}
		name := resource.Name.Schema
		if resource.Kind == schema.KindSchema {
			name = resource.Name.Name
		}
		if name != "" && !seen[name] {
			seen[name] = true
			schemas = append(schemas, name)
		}
	}
	if len(schemas) == 0 {
		return nil, errors.New("operator adoption requires at least one declared schema")
	}
	return schemas, nil
}

func lockAdoptionRelations(ctx context.Context, tx pgx.Tx, schemas []string) error {
	// PostgreSQL rejects LOCK TABLE for materialized views (relkind 'm').
	rows, err := tx.Query(ctx, `select n.nspname,c.relname from pg_class c join pg_namespace n on n.oid=c.relnamespace where n.nspname=any($1) and c.relkind in ('r','p','v','f') order by n.nspname,c.relname`, schemas)
	if err != nil {
		return errors.New("list adoption relations")
	}
	var names []string
	for rows.Next() {
		var namespace, name string
		if err = rows.Scan(&namespace, &name); err != nil {
			rows.Close()
			return errors.New("read adoption relation")
		}
		names = append(names, pgx.Identifier{namespace, name}.Sanitize())
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return errors.New("list adoption relations")
	}
	if len(names) > 0 {
		if _, err = tx.Exec(ctx, `lock table `+strings.Join(names, ",")+` in access share mode`); err != nil {
			return errors.New("lock adoption relations")
		}
	}
	return nil
}

func buildAdoptionPlan(ctx context.Context, tx pgx.Tx, schemas []string, desired schema.Document, spec operator.Spec) (plan.Plan, error) {
	current, err := postgres.InspectTx(ctx, tx, postgres.Options{Schemas: schemas})
	if err != nil {
		return plan.Plan{}, errors.New("inspect adoption target")
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		return plan.Plan{}, errors.New("normalize adoption target")
	}
	generated, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Render: operatorBootstrapRenderOptions(spec)})
	if err != nil {
		return plan.Plan{}, errors.New("compare adoption target with desired schema")
	}
	return generated, nil
}

func requireNoOpAdoptionPlan(candidate plan.Plan) error {
	if candidate.FromFingerprint == "" || candidate.FromFingerprint != candidate.ToFingerprint || len(candidate.Changes.Changes) != 0 || len(candidate.Steps) != 0 || len(candidate.Phases) != 0 || len(candidate.Replay) != 0 {
		return errors.New("adoption artifact must contain an exact no-op plan")
	}
	return nil
}

// artifactIdentityMatchesBootstrapTarget deliberately uses exact comparison.
// PostgreSQL database names are case-sensitive when quoted, and whitespace or
// Unicode normalization can identify a different database. DatabaseTarget
// normalization fills server defaults but never rewrites Name, so it cannot
// turn a differently approved identity into the execution target.
func artifactIdentityMatchesBootstrapTarget(identity string, target bootstrap.DatabaseTarget) bool {
	return identity != "" && identity == target.Normalize().Name
}

// verifyDeclarativePlan makes source-to-plan generation explicit while still
// keeping the signed artifact as the only mutation input. This catches drift
// between an inline/Kubernetes-backed desired schema and the approved plan.
func verifyDeclarativeResource(ctx context.Context, resource operator.Resource, a artifact.Artifact) (verifiedBootstrapPlan, error) {
	desired, err := loadOperatorDeclarativeSource(ctx, resource.ResolvedSource, resource.Spec.Source.Format)
	if err != nil {
		return verifiedBootstrapPlan{}, err
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		return verifiedBootstrapPlan{}, errors.New("declarative source normalization failed")
	}
	if resource.Spec.BootstrapAuthority != nil {
		if resource.Spec.BootstrapAuthorization != nil {
			if _, authorityErr := postgres.ValidateBootstrapAuthority(desired, *resource.Spec.BootstrapAuthority, resource.Spec.CreateDatabase); authorityErr != nil {
				return verifiedBootstrapPlan{}, errors.New("declarative bootstrap authority preflight failed")
			}
		} else {
			report, preflightErr := postgres.PreflightBootstrapProvisioning(ctx, desired, nil, *resource.Spec.BootstrapAuthority, resource.Spec.CreateDatabase)
			if preflightErr != nil || !report.Supported {
				return verifiedBootstrapPlan{}, errors.New("declarative bootstrap preflight failed")
			}
		}
	}
	if resource.Spec.DatabaseTarget != nil {
		return verifyBootstrapAuthorization(ctx, resource, desired, a)
	}
	schemas := make([]string, 0)
	seenSchemas := map[string]bool{}
	for _, resource := range desired.Graph.Resources {
		name := resource.Name.Schema
		if resource.Kind == schema.KindSchema {
			name = resource.Name.Name
		}
		if name != "" && !seenSchemas[name] {
			seenSchemas[name] = true
			schemas = append(schemas, name)
		}
	}
	target, err := postgres.InspectURL(ctx, resource.ResolvedDatabaseURL, postgres.Options{Schemas: schemas})
	if err != nil {
		return verifiedBootstrapPlan{}, errors.New("inspect declarative target")
	}
	target, err = postgres.New().Normalize(ctx, target)
	if err != nil {
		return verifiedBootstrapPlan{}, errors.New("normalize declarative target")
	}
	options := plan.Options{Render: operatorBootstrapRenderOptions(resource.Spec)}
	generated, err := plan.Build(ctx, postgres.New(), target, desired, options)
	for _, candidate := range desired.Graph.Resources {
		if candidate.Kind != schema.KindDatabase {
			continue
		}
		databaseTarget, targetErr := postgres.DatabaseTargetFromResource(candidate)
		if targetErr != nil {
			return verifiedBootstrapPlan{}, errors.New("declarative database target is invalid")
		}
		transition, transitionErr := postgres.PlanDatabaseTransition(ctx, databaseTarget, target, desired, options)
		if transitionErr != nil {
			return verifiedBootstrapPlan{}, errors.New("declarative database transition planning failed")
		}
		generated, err = transition.SchemaPlan, nil
		break
	}
	if err != nil || generated.Digest != a.Plan.Digest {
		return verifiedBootstrapPlan{}, errors.New("declarative source does not match approved plan")
	}
	return verifiedBootstrapPlan{}, nil
}

func loadOperatorDeclarativeSource(ctx context.Context, raw, declaredFormat string) (schema.Document, error) {
	load := func(format source.Format) (schema.Document, error) {
		return source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: format, Data: []byte(raw)})
	}
	switch strings.ToLower(strings.TrimSpace(declaredFormat)) {
	case "sql":
		document, err := load(source.FormatSQL)
		if err != nil {
			return schema.Document{}, errors.New("declarative source does not match declared sql format")
		}
		return document, nil
	case "hcl":
		document, err := load(source.FormatHCLSource)
		if err != nil {
			return schema.Document{}, errors.New("declarative source does not match declared hcl format")
		}
		return document, nil
	case "":
		sqlDocument, sqlErr := load(source.FormatSQL)
		hclDocument, hclErr := load(source.FormatHCLSource)
		if sqlErr == nil && hclErr != nil {
			return sqlDocument, nil
		}
		if hclErr == nil && sqlErr != nil {
			return hclDocument, nil
		}
		if sqlErr == nil && hclErr == nil {
			return schema.Document{}, errors.New("declarative source format is ambiguous; set source.format to sql or hcl")
		}
		return schema.Document{}, errors.New("declarative source is invalid; set source.format to sql or hcl")
	default:
		return schema.Document{}, errors.New("declarative source format must be sql or hcl")
	}
}

// verifyDeclarativePlan retains the narrow source-to-plan test seam used by
// existing callers. Bootstrap authorization uses verifyDeclarativeResource so
// it also receives runtime-only references and database target identity.
func verifyDeclarativePlan(ctx context.Context, desiredSQL, databaseURL string, a artifact.Artifact, authority *bootstrap.Contract, createDatabase bool) error {
	_, err := verifyDeclarativeResource(ctx, operator.Resource{
		Spec:           operator.Spec{BootstrapAuthority: authority, CreateDatabase: createDatabase},
		ResolvedSource: desiredSQL, ResolvedDatabaseURL: databaseURL,
	}, a)
	return err
}

func verifyBootstrapAuthorization(ctx context.Context, resource operator.Resource, desired schema.Document, a artifact.Artifact) (verifiedBootstrapPlan, error) {
	render := operatorBootstrapRenderOptions(resource.Spec)
	inventory, err := postgres.PrepareBootstrapAuthorizationInventory(ctx, *resource.Spec.DatabaseTarget, desired, postgres.BootstrapAuthorizationInventoryOptions{Render: render})
	if err != nil {
		return verifiedBootstrapPlan{}, &operator.AuthorizationError{State: operator.AuthorizationInvalid}
	}
	requiresAuthorization := len(inventory.Routines) > 0 || len(inventory.Extensions) > 0
	if resource.Spec.BootstrapAuthorization == nil {
		if requiresAuthorization {
			return verifiedBootstrapPlan{}, &operator.AuthorizationError{State: operator.AuthorizationMissing}
		}
		whole, planErr := postgres.PlanDatabaseBootstrap(ctx, *resource.Spec.DatabaseTarget, desired, plan.Options{Render: render})
		if planErr != nil || (whole.SchemaPlan.Digest != a.Plan.Digest && whole.Digest != a.Plan.Digest) {
			return verifiedBootstrapPlan{}, errors.New("declarative source does not match approved plan")
		}
		return verifiedBootstrapPlan{Plan: whole, SourceDigest: inventory.SourceDigest}, nil
	}
	manifest, err := postgres.ParseBootstrapAuthorizationManifest(resource.ResolvedAuthorizationManifest)
	if err != nil {
		return verifiedBootstrapPlan{}, &operator.AuthorizationError{State: operator.AuthorizationInvalid}
	}
	publicKey := resource.ResolvedAuthorizationPublicKey
	if len(publicKey) != ed25519.PublicKeySize {
		publicKey, err = base64.RawStdEncoding.Strict().DecodeString(strings.TrimSpace(string(publicKey)))
		if err != nil {
			publicKey, err = base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(resource.ResolvedAuthorizationPublicKey)))
		}
	}
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return verifiedBootstrapPlan{}, &operator.AuthorizationError{State: operator.AuthorizationInvalid}
	}
	ref := resource.Spec.BootstrapAuthorization
	verified, err := postgres.VerifyBootstrapAuthorizationManifest(manifest, inventory, postgres.BootstrapAuthorizationVerifyPolicy{
		Now:    time.Now,
		Keys:   map[string]artifact.KeyRecord{manifest.Signature.KeyID: {PublicKey: ed25519.PublicKey(publicKey), Issuer: ref.Issuer, Identity: ref.Signer, Purpose: ref.Purpose, Status: "active", NotBefore: manifest.NotBefore, NotAfter: manifest.ExpiresAt}},
		Issuer: ref.Issuer, Signer: ref.Signer, Purpose: ref.Purpose,
	})
	if err != nil {
		if errors.Is(err, postgres.ErrExpiredBootstrapAuthorization) || errors.Is(err, postgres.ErrStaleBootstrapAuthorization) {
			return verifiedBootstrapPlan{}, &operator.AuthorizationError{State: operator.AuthorizationStale}
		}
		return verifiedBootstrapPlan{}, &operator.AuthorizationError{State: operator.AuthorizationInvalid}
	}
	whole, err := postgres.PlanDatabaseBootstrapAuthorized(ctx, *resource.Spec.DatabaseTarget, desired, plan.Options{Render: render}, verified)
	if err != nil {
		if errors.Is(err, postgres.ErrStaleBootstrapAuthorization) {
			return verifiedBootstrapPlan{}, &operator.AuthorizationError{State: operator.AuthorizationStale}
		}
		return verifiedBootstrapPlan{}, &operator.AuthorizationError{State: operator.AuthorizationInvalid}
	}
	if whole.SchemaPlan.Digest != a.Plan.Digest && whole.Digest != a.Plan.Digest {
		return verifiedBootstrapPlan{}, &operator.AuthorizationError{State: operator.AuthorizationStale}
	}
	return verifiedBootstrapPlan{Plan: whole, SourceDigest: inventory.SourceDigest, ExpiresAt: manifest.ExpiresAt, Authorized: true}, nil
}

func operatorBootstrapRenderOptions(spec operator.Spec) map[string]string {
	render := map[string]string{}
	if spec.PostgresVersion > 0 {
		render["postgres_version"] = fmt.Sprint(spec.PostgresVersion)
	}
	if spec.ConcurrentIndexes {
		render["concurrent_indexes"] = "true"
	}
	return render
}
