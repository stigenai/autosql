package migrate

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/policy"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"

	"github.com/jackc/pgx/v5"
)

const automationApprovalDomain = "autosql.approval.automation/v1\x00"

// AutomationApprovalProvider signs a fresh approval after the exact guardrail
// digest is known. The signed claims bind the CI identity, environment,
// approval window, and bundle, so a proof cannot authorize another release.
type AutomationApprovalProvider struct {
	KeyID      string
	Identity   approval.Identity
	Actors     []approval.Identity
	PrivateKey ed25519.PrivateKey
	TTL        time.Duration
}

type automationApprovalClaims struct {
	Version     string    `json:"version"`
	KeyID       string    `json:"key_id"`
	Identity    string    `json:"identity"`
	PlanDigest  string    `json:"plan_digest"`
	Environment string    `json:"environment"`
	ApprovedAt  time.Time `json:"approved_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type automationApprovalAuthority struct {
	keyID    string
	identity approval.Identity
	actors   map[string]approval.Identity
	public   ed25519.PublicKey
}

func (p AutomationApprovalProvider) Issue(_ context.Context, digest, environment string, created, artifactExpiry time.Time) ([]approval.Approval, approval.IdentityAuthority, error) {
	if p.KeyID == "" || p.Identity.ID == "" || len(p.PrivateKey) != ed25519.PrivateKeySize || p.TTL <= 0 || digest == "" || environment == "" || created.IsZero() || !artifactExpiry.After(created) {
		return nil, nil, errors.New("automation approval configuration is incomplete")
	}
	expires := created.UTC().Add(p.TTL)
	if expires.After(artifactExpiry.UTC()) {
		expires = artifactExpiry.UTC()
	}
	claims := automationApprovalClaims{Version: "autosql.approval.automation/v1", KeyID: p.KeyID, Identity: p.Identity.ID, PlanDigest: digest, Environment: environment, ApprovedAt: created.UTC(), ExpiresAt: expires}
	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, nil, err
	}
	signature := ed25519.Sign(p.PrivateKey, append([]byte(automationApprovalDomain), payload...))
	proof := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
	actors := map[string]approval.Identity{p.Identity.ID: p.Identity}
	for _, actor := range p.Actors {
		if actor.ID != "" {
			actors[actor.ID] = actor
		}
	}
	authority := automationApprovalAuthority{keyID: p.KeyID, identity: p.Identity, actors: actors, public: p.PrivateKey.Public().(ed25519.PublicKey)}
	return []approval.Approval{{Approver: p.Identity.ID, ApprovedAt: created.UTC(), ExpiresAt: expires, Proof: proof}}, authority, nil
}

func (a automationApprovalAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	identity, ok := a.actors[id]
	if !ok {
		return approval.Identity{}, errors.New("untrusted automation actor")
	}
	return identity, nil
}

func (a automationApprovalAuthority) VerifyApproval(_ context.Context, item approval.Approval) (approval.VerifiedApproval, error) {
	parts := strings.Split(item.Proof, ".")
	if len(parts) != 2 {
		return approval.VerifiedApproval{}, errors.New("invalid automation approval proof")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return approval.VerifiedApproval{}, errors.New("invalid automation approval proof")
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || !ed25519.Verify(a.public, append([]byte(automationApprovalDomain), payload...), signature) {
		return approval.VerifiedApproval{}, errors.New("untrusted automation approval proof")
	}
	var claims automationApprovalClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&claims); err != nil || claims.Version != "autosql.approval.automation/v1" || claims.KeyID != a.keyID || claims.Identity != a.identity.ID || claims.PlanDigest == "" || claims.Environment == "" || claims.ApprovedAt.IsZero() || !claims.ExpiresAt.After(claims.ApprovedAt) {
		return approval.VerifiedApproval{}, errors.New("invalid automation approval claims")
	}
	return approval.VerifiedApproval{Identity: a.identity, PlanDigest: claims.PlanDigest, Environment: claims.Environment, ApprovedAt: claims.ApprovedAt.UTC(), ExpiresAt: claims.ExpiresAt.UTC()}, nil
}

type OperatorArtifactRequest struct {
	Generation       GenerateRequest
	Current, Desired schema.Document
	BootstrapTarget  *bootstrap.DatabaseTarget
	Adopt            bool
	Render           map[string]string
}

type OperatorArtifactResult struct {
	Artifact                                        artifact.Artifact
	Bytes                                           []byte
	SchemaPolicyResources, MigrationPolicyResources []policy.Resource
}

type generationPlanMutation struct {
	url   string
	plan  plan.Plan
	cause error
}

func (m *generationPlanMutation) ApplyAuthorized(ctx context.Context, checks precheck.Plan) (results []precheck.Result, err error) {
	defer func() {
		if err != nil {
			m.cause = simulate.RedactedCause(err)
		}
	}()
	validation := checks
	validation.Statements = nil
	for index := range validation.Assertions {
		validation.Assertions[index].PlanDigest = ""
	}
	digest, err := precheck.Digest(validation)
	if err != nil {
		return nil, err
	}
	validation.Digest = digest
	for index := range validation.Assertions {
		validation.Assertions[index].PlanDigest = digest
	}
	results, err = precheck.GuardedApply(ctx, replayDB{url: m.url}, validation)
	if err != nil {
		return results, err
	}
	conn, err := pgx.Connect(ctx, m.url)
	if err != nil {
		return results, err
	}
	defer conn.Close(context.Background())
	for _, phase := range m.plan.Phases {
		if phase.Transaction == plan.TransactionProhibited {
			for _, id := range phase.StepIDs {
				if err = executeOperatorStep(ctx, conn, nil, m.plan.Steps, id); err != nil {
					return results, err
				}
			}
			continue
		}
		tx, beginErr := conn.Begin(ctx)
		if beginErr != nil {
			return results, beginErr
		}
		for _, id := range phase.StepIDs {
			if err = executeOperatorStep(ctx, conn, tx, m.plan.Steps, id); err != nil {
				_ = tx.Rollback(ctx)
				return results, err
			}
		}
		if err = tx.Commit(ctx); err != nil {
			return results, err
		}
	}
	return results, nil
}

// BuildOperatorArtifact produces the same generated, simulated, policy-bound,
// approved, and release-signed artifact as migration generation, but without a
// versioned migration directory or interactive plan-edit ceremony.
func (s GenerateService) BuildOperatorArtifact(ctx context.Context, request OperatorArtifactRequest) (OperatorArtifactResult, error) {
	r := request.Generation
	r.Directory, r.Version, r.Label, r.Format = ".", "1", "operator", "sql"
	r.Desired = request.Desired
	if err := validateGenerateRequest(r); err != nil {
		return OperatorArtifactResult{}, err
	}
	current, desired := request.Current, request.Desired
	var prebuilt *plan.Plan
	options := plan.Options{Render: cloneStrings(request.Render)}
	if request.Adopt {
		if request.BootstrapTarget != nil || hasDatabaseResource(desired) {
			return OperatorArtifactResult{}, generationFailure("adoption_target", ErrGenerateConfig)
		}
		// Adoption artifacts are intentionally published without production
		// access. Planning desired -> desired produces the signed no-op assertion
		// that the operator must reproduce from its locked live inspection.
		current = desired
	}
	if request.BootstrapTarget != nil {
		inventory, err := postgres.PrepareBootstrapAuthorizationInventory(ctx, *request.BootstrapTarget, desired, postgres.BootstrapAuthorizationInventoryOptions{Render: options.Render})
		if err != nil {
			return OperatorArtifactResult{}, generationFailure("bootstrap_inventory", ErrGenerateStage)
		}
		options.Render = operatorBootstrapArtifactRender(inventory, options.Render)
		whole, err := postgres.PlanDatabaseBootstrap(ctx, *request.BootstrapTarget, desired, options)
		if err != nil {
			return OperatorArtifactResult{}, generationFailure("bootstrap_plan", ErrGenerateStage)
		}
		if whole.Digest != inventory.PlanDigest || whole.SchemaPlan.Digest != inventory.PlanSummary.SchemaPlanDigest {
			return OperatorArtifactResult{}, generationFailure("bootstrap_inventory_binding", ErrGenerateStage)
		}
		current = bootstrapIntrinsicDocument(*request.BootstrapTarget, desired)
		desired = documentWithoutDatabase(desired)
		prebuilt = &whole.SchemaPlan
	} else if hasDatabaseResource(desired) {
		target, err := singleDatabaseTarget(desired)
		if err != nil {
			return OperatorArtifactResult{}, generationFailure("database_target", ErrGenerateConfig)
		}
		whole, err := postgres.PlanDatabaseTransition(ctx, target, current, desired, options)
		if err != nil {
			return OperatorArtifactResult{}, generationFailure("transition_plan", ErrGenerateStage)
		}
		current, desired = documentWithoutDatabase(current), documentWithoutDatabase(desired)
		prebuilt = &whole.SchemaPlan
	}
	current, err := postgres.New().Normalize(ctx, current)
	if err != nil {
		return OperatorArtifactResult{}, generationFailure("source", ErrGenerateConfig)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		return OperatorArtifactResult{}, generationFailure("desired", ErrGenerateConfig)
	}
	if request.Adopt {
		options.Render = operatorAdoptionArtifactRender(desired, options.Render)
	}
	// Both replay paths below have to CREATE whatever the CURRENT schema already
	// contains -- extensions, reviewed routines -- before the forward plan can be
	// replayed on top of it. Bootstrap mode never reaches this (current is empty)
	// and adopt mode is authorized just above, so transition mode was the only
	// path handing the renderer an empty extension_allowlist, which then rejects
	// any existing database that uses an extension.
	//
	// Authorize from `current` into a SEPARATE render map. `options` must keep
	// rendering the forward plan with exactly the operator's own options
	// (postgres_version + concurrent_indexes) or the plan digest stops matching
	// what the operator recomputes at apply time, and it refuses the artifact.
	replayRender := operatorAdoptionArtifactRender(current, options.Render)
	factory := operatorSimulationFactory(request.BootstrapTarget, desired)
	factory.Render = cloneStrings(replayRender)
	replayFactory := factory
	replayFactory.NamePrefix = "autosql_sim_gen_replay"
	workspace, err := createReplayWorkspace(ctx, r, replayFactory)
	if err != nil {
		return OperatorArtifactResult{}, generationFailureCause("workspace", ErrGenerateStage, simulate.Redacted(err))
	}
	defer workspace.Close()
	materializeOptions := options
	materializeOptions.Render = replayRender
	if err = materializeOperatorCurrent(ctx, workspace.URL, current, materializeOptions); err != nil {
		return OperatorArtifactResult{}, generationFailureCause("materialize", ErrGenerateStage, simulate.RedactedCause(err))
	}
	metadata := cloneStrings(r.Metadata)
	metadata["autosql.operator.gitops"] = "v1"
	metadata["autosql.operator.mode"] = "transition"
	if request.BootstrapTarget != nil {
		metadata["autosql.operator.mode"] = "bootstrap"
	} else if request.Adopt {
		metadata["autosql.operator.mode"] = "adopt"
	}
	built, err := s.buildGeneratedArtifact(ctx, r, current, desired, workspace.URL, nil, options, prebuilt, metadata, factory)
	if err != nil {
		return OperatorArtifactResult{}, err
	}
	if request.Adopt && (built.Artifact.Plan.FromFingerprint != built.Artifact.Plan.ToFingerprint || len(built.Artifact.Plan.Changes.Changes) != 0 || len(built.Artifact.Plan.Steps) != 0 || len(built.Artifact.Plan.Phases) != 0 || len(built.Artifact.Plan.Replay) != 0) {
		return OperatorArtifactResult{}, generationFailure("adoption_plan", ErrGenerateStage)
	}
	return OperatorArtifactResult{Artifact: built.Artifact, Bytes: built.Bytes, SchemaPolicyResources: built.SchemaPolicyResources, MigrationPolicyResources: built.MigrationPolicyResources}, nil
}

func operatorSimulationFactory(target *bootstrap.DatabaseTarget, desired schema.Document) simulate.PostgresFactory {
	factory := simulate.PostgresFactory{NamePrefix: "autosql_sim_generate"}
	if target != nil && target.Normalize().Mode == bootstrap.ExternalDatabase {
		factory.DropPublicSchema = true
	}
	factory.RequiredRoles = operatorRequiredOwnerRoles(target, desired)
	return factory
}

func operatorRequiredOwnerRoles(target *bootstrap.DatabaseTarget, desired schema.Document) []string {
	declared := map[string]bool{}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindRole {
			declared[resource.Name.Name] = true
		}
	}
	owners := map[string]bool{}
	if target != nil {
		owner := strings.TrimSpace(target.Normalize().Owner)
		if owner != "" && !declared[owner] {
			owners[owner] = true
		}
	}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindRole || resource.Kind == schema.KindDatabase {
			continue
		}
		var specification map[string]any
		if json.Unmarshal(resource.Spec, &specification) != nil {
			continue
		}
		owner, _ := specification["owner"].(string)
		owner = strings.TrimSpace(owner)
		if owner != "" && !declared[owner] {
			owners[owner] = true
		}
	}
	roles := make([]string, 0, len(owners))
	for owner := range owners {
		roles = append(roles, owner)
	}
	sort.Strings(roles)
	return roles
}

// operatorBootstrapArtifactRender reconstructs only the deterministic render
// inputs bound by a prepared inventory. Keeping this inside the signed
// artifact pipeline preserves postgres.PrepareBootstrapAuthorizationInventory's
// review-only public contract: it still cannot be passed to the executor.
func operatorBootstrapArtifactRender(inventory postgres.BootstrapAuthorizationInventory, base map[string]string) map[string]string {
	render := cloneStrings(base)
	routineDigests, extensionNames := map[string]bool{}, map[string]bool{}
	for _, routine := range inventory.Routines {
		routineDigests[routine.SourceDigest] = true
		if routine.UnsafeLanguageAuthorizationRequired {
			render["allow_unsafe_routine_languages"] = "true"
		}
		if routine.PrivilegedRoutineAuthorizationRequired {
			render["allow_privileged_routines"] = "true"
		}
		if routine.TransactionControlAuthorizationRequired {
			render["allow_transaction_control_procedures"] = "true"
		}
	}
	for _, extension := range inventory.Extensions {
		extensionNames[extension.Name] = true
		render["extension_version."+extension.Name] = extension.Version
		render["extension_schemas."+extension.Name] = extension.Schema
		if extension.UntrustedExtensionAuthorizationRequired {
			render["allow_untrusted_extensions"] = "true"
		}
	}
	render["reviewed_routine_digests"] = sortedOperatorAuthorizationValues(routineDigests)
	render["extension_allowlist"] = sortedOperatorAuthorizationValues(extensionNames)
	return render
}

// operatorAdoptionArtifactRender authorizes only the exact safe provisioning
// inputs already bound into the normalized desired document. The normal
// PostgreSQL renderer still rejects unsafe routine languages, privileged
// routine bodies, and untrusted extensions unless those separate controls are
// explicitly enabled.
func operatorAdoptionArtifactRender(desired schema.Document, base map[string]string) map[string]string {
	render := cloneStrings(base)
	routineDigests, extensionNames := map[string]bool{}, map[string]bool{}
	for _, resource := range desired.Graph.Resources {
		var specification map[string]any
		if json.Unmarshal(resource.Spec, &specification) != nil {
			continue
		}
		value := func(key string) string {
			text, _ := specification[key].(string)
			return strings.TrimSpace(text)
		}
		switch resource.Kind {
		case schema.KindFunction, schema.KindProcedure:
			if digest := value("body_digest"); digest != "" {
				routineDigests[digest] = true
			}
		case schema.KindExtension:
			name := strings.TrimSpace(resource.Name.Name)
			if name == "" {
				continue
			}
			extensionNames[name] = true
			render["extension_version."+name] = value("version")
			render["extension_schemas."+name] = strings.TrimSpace(resource.Name.Schema)
		}
	}
	render["reviewed_routine_digests"] = sortedOperatorAuthorizationValues(routineDigests)
	render["extension_allowlist"] = sortedOperatorAuthorizationValues(extensionNames)
	return render
}

func sortedOperatorAuthorizationValues(values map[string]bool) string {
	items := make([]string, 0, len(values))
	for value := range values {
		if strings.TrimSpace(value) != "" {
			items = append(items, value)
		}
	}
	sort.Strings(items)
	return strings.Join(items, ",")
}

func materializeOperatorCurrent(ctx context.Context, databaseURL string, desired schema.Document, options plan.Options) error {
	baseline := schema.Document{Version: desired.Version, Graph: schema.Graph{Extra: desired.Graph.Extra}, Annotations: desired.Annotations, Extra: desired.Extra}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindSchema && resource.Name.Name == "public" && resource.Name.Schema == "" {
			baseline.Graph.Resources = append(baseline.Graph.Resources, schema.Resource{ID: resource.ID, Kind: resource.Kind, Name: resource.Name, Spec: []byte(`{}`)})
			break
		}
	}
	pl, err := plan.Build(ctx, postgres.New(), baseline, desired, options)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, phase := range pl.Phases {
		if phase.Transaction == plan.TransactionProhibited {
			for _, id := range phase.StepIDs {
				if err = executeOperatorStep(ctx, conn, nil, pl.Steps, id); err != nil {
					return err
				}
			}
			continue
		}
		tx, beginErr := conn.Begin(ctx)
		if beginErr != nil {
			return beginErr
		}
		for _, id := range phase.StepIDs {
			if err = executeOperatorStep(ctx, conn, tx, pl.Steps, id); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func executeOperatorStep(ctx context.Context, conn *pgx.Conn, tx pgx.Tx, steps []plan.Step, id string) error {
	for _, step := range steps {
		if step.ID != id || step.Kind != plan.StepExecutable {
			continue
		}
		if tx != nil {
			_, err := tx.Exec(ctx, step.SQL)
			return err
		}
		_, err := conn.Exec(ctx, step.SQL)
		return err
	}
	return nil
}

func hasDatabaseResource(document schema.Document) bool {
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			return true
		}
	}
	return false
}

func singleDatabaseTarget(document schema.Document) (bootstrap.DatabaseTarget, error) {
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
		return target, errors.New("exactly one database resource is required")
	}
	return target, nil
}

func documentWithoutDatabase(document schema.Document) schema.Document {
	resources := make([]schema.Resource, 0, len(document.Graph.Resources))
	for _, resource := range document.Graph.Resources {
		if resource.Kind != schema.KindDatabase {
			resources = append(resources, resource)
		}
	}
	document.Graph.Resources = resources
	return document
}

func bootstrapIntrinsicDocument(target bootstrap.DatabaseTarget, desired schema.Document) schema.Document {
	current := schema.Document{Version: desired.Version, Graph: schema.Graph{Extra: desired.Graph.Extra}, Annotations: desired.Annotations, Extra: desired.Extra}
	if target.Normalize().Mode != bootstrap.ManagedDatabase {
		return current
	}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindSchema && resource.Name.Name == "public" && resource.Name.Schema == "" {
			current.Graph.Resources = append(current.Graph.Resources, schema.Resource{ID: resource.ID, Kind: resource.Kind, Name: resource.Name, Spec: []byte(`{}`)})
			break
		}
	}
	return current
}
