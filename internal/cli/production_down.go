package cli

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
	"autosql/pkg/migrate"
	migratedown "autosql/pkg/migrate/down"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type downConfig struct {
	MigrationDirectory, RevisionSchema, DevelopmentURLReference, DevelopmentIdentity, PlanSigningKeyReference, PlanSigningKeyID, ArtifactDirectory, Operator string
	ReleaseKeyReference, ReleaseKeyID, GeneratorKeyReference, GeneratorKeyID, GeneratorPurpose, Issuer, Signer, Purpose                                      string
	PlanTTL                                                                                                                                                  time.Duration
	Reverse                                                                                                                                                  []migratedown.ReverseStatement
	Checks                                                                                                                                                   []precheck.Assertion
	Override                                                                                                                                                 *migratedown.Override
	OverridePublicKeys                                                                                                                                       map[string]string
	ApprovalPolicy                                                                                                                                           approval.Policy
	Actors                                                                                                                                                   map[string]approval.Identity
	VerifiedApprovals                                                                                                                                        map[string]approval.VerifiedApproval
	Approvals                                                                                                                                                []approval.Approval
	ApprovalAuditPath                                                                                                                                        string
}

func (s *productionDownService) artifactApproval(ctx context.Context, bundle string, now time.Time) (artifact.Approval, error) {
	authority := downAuthority{actors: s.cfg.Actors, verified: s.cfg.VerifiedApprovals}
	ids, proofs := []string{}, []string{}
	latest := time.Time{}
	for _, item := range s.cfg.Approvals {
		item.PlanDigest, item.Environment = bundle, s.environment
		v, e := authority.VerifyApproval(ctx, item)
		if e != nil || v.PlanDigest != bundle || v.Environment != s.environment || !v.ExpiresAt.After(now) {
			return artifact.Approval{}, approval.ErrDenied
		}
		ids = append(ids, v.Identity.ID)
		proofs = append(proofs, item.Proof)
		if v.ApprovedAt.After(latest) {
			latest = v.ApprovedAt
		}
	}
	sort.Strings(ids)
	sort.Strings(proofs)
	sum := sha256.Sum256([]byte(strings.Join(proofs, "\x00")))
	return artifact.Approval{Identity: strings.Join(ids, ","), ApprovedAt: latest.UTC(), ProofDigest: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

type downAuthority struct {
	actors   map[string]approval.Identity
	verified map[string]approval.VerifiedApproval
}
type downPlanMutation struct{}

func (downPlanMutation) ApplyAuthorized(context.Context, precheck.Plan) ([]precheck.Result, error) {
	return nil, nil
}

func (a downAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	v, ok := a.actors[id]
	if !ok {
		return v, errors.New("untrusted down actor")
	}
	return v, nil
}
func (a downAuthority) VerifyApproval(_ context.Context, p approval.Approval) (approval.VerifiedApproval, error) {
	v, ok := a.verified[p.Proof]
	if !ok {
		return v, errors.New("untrusted down approval")
	}
	return v, nil
}

type productionDownService struct {
	cfg                              downConfig
	url                              string
	schemas                          []string
	identity, environment            string
	devURL                           string
	private                          ed25519.PrivateKey
	public                           ed25519.PublicKey
	releasePrivate, generatorPrivate ed25519.PrivateKey
	releasePublic, generatorPublic   ed25519.PublicKey
	overrideKeys                     map[string]ed25519.PublicKey
	verified                         VerifiedArtifactApplyService
	store                            *revision.Store
	mu                               sync.Mutex
	plans                            map[string]migratedown.DownPlan
	policies                         map[string]artifact.VerifyPolicy
	policyMu                         *sync.RWMutex
	originalVerify                   func(artifact.Artifact) (artifact.VerifiedArtifact, error)
}

func newProductionDownService(path string, resolver *secret.Resolver, v VerifiedArtifactApplyService, url string, base applyConfig) (DownService, error) {
	if path == "" {
		return nil, nil
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, errors.New("read down configuration")
	}
	var c downConfig
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return nil, errors.New("parse down configuration")
	}
	if c.MigrationDirectory == "" || c.Operator == "" || c.PlanTTL <= 0 || c.ArtifactDirectory == "" || c.ReleaseKeyID == "" || c.GeneratorKeyID == "" || c.GeneratorPurpose == "" || c.Issuer == "" || c.Signer == "" || c.Purpose == "" {
		return nil, errors.New("invalid down configuration")
	}
	dev, e := resolver.Resolve(context.Background(), secret.Reference(c.DevelopmentURLReference))
	if e != nil {
		return nil, errors.New("resolve down development database")
	}
	keyText, e := resolver.Resolve(context.Background(), secret.Reference(c.PlanSigningKeyReference))
	if e != nil {
		return nil, errors.New("resolve down signing key")
	}
	private, e := decodePrivate(keyText)
	if e != nil {
		return nil, e
	}
	releaseText, e := resolver.Resolve(context.Background(), secret.Reference(c.ReleaseKeyReference))
	if e != nil {
		return nil, errors.New("resolve down release key")
	}
	releasePrivate, e := decodePrivate(releaseText)
	if e != nil {
		return nil, e
	}
	generatorText, e := resolver.Resolve(context.Background(), secret.Reference(c.GeneratorKeyReference))
	if e != nil {
		return nil, errors.New("resolve down generator key")
	}
	generatorPrivate, e := decodePrivate(generatorText)
	if e != nil {
		return nil, e
	}
	keys := map[string]ed25519.PublicKey{}
	for id, raw := range c.OverridePublicKeys {
		b, de := base64.RawStdEncoding.Strict().DecodeString(raw)
		if de != nil || len(b) != ed25519.PublicKeySize {
			return nil, errors.New("invalid down override key")
		}
		keys[id] = b
	}
	store, e := revision.Open(revision.Config{URL: url, Schema: c.RevisionSchema})
	if e != nil {
		return nil, e
	}
	if len(c.Approvals) == 0 || c.ApprovalAuditPath == "" {
		return nil, errors.New("trusted down approval configuration required")
	}
	originalInput := v.Input
	if originalInput == nil {
		return nil, errors.New("down guardrail input unavailable")
	}
	v.Guardrail.Approval.Policy = c.ApprovalPolicy
	v.Guardrail.Approval.Authority = downAuthority{actors: c.Actors, verified: c.VerifiedApprovals}
	v.Guardrail.Approval.Audit = &approval.Chain{Sink: &approval.FileSink{Path: c.ApprovalAuditPath}}
	v.Input = func(a artifact.Artifact) (guardrail.Input, error) {
		in, e := originalInput(a)
		if e != nil {
			return in, e
		}
		in.Approval.Approvals = append([]approval.Approval(nil), c.Approvals...)
		for i := range in.Approval.Approvals {
			in.Approval.Approvals[i].PlanDigest = a.GuardrailDigest
			in.Approval.Approvals[i].Environment = a.TargetEnvironment
		}
		return in, nil
	}
	originalVerify := v.VerifyArtifact
	policies := map[string]artifact.VerifyPolicy{}
	policyMu := &sync.RWMutex{}
	originalPolicyFor := v.PolicyFor
	v.PolicyFor = func(a artifact.Artifact) (artifact.VerifyPolicy, error) {
		policyMu.RLock()
		p, ok := policies[a.Digest]
		policyMu.RUnlock()
		if ok {
			return p, nil
		}
		if originalPolicyFor != nil {
			return originalPolicyFor(a)
		}
		return v.Policy, nil
	}
	return &productionDownService{cfg: c, url: url, schemas: append([]string(nil), base.Schemas...), identity: base.DatabaseIdentity, environment: base.Environment, devURL: dev, private: private, public: private.Public().(ed25519.PublicKey), releasePrivate: releasePrivate, generatorPrivate: generatorPrivate, releasePublic: releasePrivate.Public().(ed25519.PublicKey), generatorPublic: generatorPrivate.Public().(ed25519.PublicKey), overrideKeys: keys, verified: v, store: store, plans: map[string]migratedown.DownPlan{}, policies: policies, policyMu: policyMu, originalVerify: originalVerify}, nil
}
func (s *productionDownService) PlanDown(ctx context.Context, to string) (migratedown.DownPlan, error) {
	snap, e := migrate.LoadSnapshot(s.cfg.MigrationDirectory)
	if e != nil {
		return migratedown.DownPlan{}, migratedown.ErrRefused
	}
	session, e := s.store.OpenSession(ctx)
	if e != nil {
		return migratedown.DownPlan{}, e
	}
	defer session.Close(context.WithoutCancel(ctx))
	key, e := executor.LockKey(s.identity, s.environment)
	if e != nil {
		return migratedown.DownPlan{}, e
	}
	ok, e := session.Lock(ctx, key)
	if e != nil || !ok {
		return migratedown.DownPlan{}, executor.ErrBusy
	}
	defer session.Unlock(context.WithoutCancel(ctx), key)
	if e = s.drainOutbox(ctx, session); e != nil {
		return migratedown.DownPlan{}, errors.New("drain pending down lifecycle audit")
	}
	records, e := session.Revisions(ctx)
	if e != nil {
		return migratedown.DownPlan{}, e
	}
	for _, r := range records {
		if r.State != "applied" && r.State != "baseline" && r.State != "checkpoint" {
			return migratedown.DownPlan{}, errors.New("dirty revision history requires reconciliation")
		}
	}
	history, e := session.ExecutorRecords(ctx, s.identity+"/"+s.environment)
	if e != nil {
		return migratedown.DownPlan{}, e
	}
	for _, h := range history {
		if h.State != "confirmed" {
			return migratedown.DownPlan{}, errors.New("uncertain executor history requires reconciliation")
		}
	}
	live, e := postgres.InspectConn(ctx, session.Raw(), postgres.Options{Schemas: s.schemas})
	if e != nil {
		return migratedown.DownPlan{}, errors.New("inspect locked down target")
	}
	live, e = postgres.New().Normalize(ctx, live)
	if e != nil {
		return migratedown.DownPlan{}, e
	}
	prior, e := migratedown.ReplayPrior(ctx, snap, to, s.devURL, s.cfg.DevelopmentIdentity, s.identity, s.schemas)
	if e != nil {
		return migratedown.DownPlan{}, e
	}
	now := time.Now().UTC()
	p, e := migratedown.Build(ctx, migratedown.Request{Snapshot: snap, Revisions: records, TargetVersion: to, LockedLive: live, ReplayedPrior: prior, Reverse: s.cfg.Reverse, Checks: s.cfg.Checks, Override: s.cfg.Override, OverrideKeys: s.overrideKeys, Now: now, ExpiresAt: now.Add(s.cfg.PlanTTL), SignerKeyID: s.cfg.PlanSigningKeyID, Signer: s.private, VerifyOriginal: s.originalVerify, Executor: history, TargetIdentity: s.identity + "/" + s.environment})
	if e != nil {
		return p, e
	}
	for _, phase := range p.Plan.Phases {
		if phase.Transaction == plan.TransactionProhibited {
			return p, errors.New("production down refuses nontransactional reversal; no reliable atomic revision outcome")
		}
	}
	draft, e := artifact.NewGenerated(p.Plan, p.Checks, now, now.Add(s.cfg.PlanTTL), "down:"+p.HeadVersion+":"+p.TargetVersion, s.environment, s.identity, "sha256:"+strings.Repeat("0", 64), artifact.Approval{Identity: "pending", ApprovedAt: now}, map[string]string{"down_plan": p.Digest}, s.cfg.GeneratorKeyID, s.cfg.GeneratorPurpose, s.generatorPrivate)
	if e != nil {
		return p, e
	}
	in, e := s.verified.Input(draft)
	if e != nil {
		return p, e
	}
	bundle, e := s.verified.Guardrail.BundleDigest(in)
	if e != nil {
		return p, e
	}
	in.Approval.Plan.Digest = bundle
	in.Mutation = downPlanMutation{}
	if _, e = s.verified.Guardrail.Apply(ctx, in); e != nil {
		return p, errors.New("down safety, policy, approval, or guardrail refused")
	}
	approved, e := s.artifactApproval(ctx, bundle, now)
	if e != nil {
		return p, e
	}
	a, e := artifact.NewGenerated(p.Plan, p.Checks, now, now.Add(s.cfg.PlanTTL), "down:"+p.HeadVersion+":"+p.TargetVersion, s.environment, s.identity, bundle, approved, map[string]string{"down_plan": p.Digest, "manifest": p.ManifestDigest}, s.cfg.GeneratorKeyID, s.cfg.GeneratorPurpose, s.generatorPrivate)
	if e != nil {
		return p, e
	}
	if e = a.Sign(s.cfg.ReleaseKeyID, s.releasePrivate); e != nil {
		return p, e
	}
	raw, e := a.MarshalCanonical()
	if e != nil {
		return p, e
	}
	path := filepath.Join(s.cfg.ArtifactDirectory, a.Digest+".json")
	if e = atomicCreate(path, raw); e != nil {
		return p, e
	}
	policy := artifact.VerifyPolicy{Now: time.Now, NoEdits: true, Expected: artifact.ExpectedBindings{PlanDigest: a.Plan.Digest, GeneratedPlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity, ApprovalProofDigest: a.Approval.ProofDigest}, Keys: map[string]artifact.KeyRecord{s.cfg.ReleaseKeyID: {PublicKey: s.releasePublic, Issuer: s.cfg.Issuer, Identity: s.cfg.Signer, Environment: s.environment, Purpose: s.cfg.Purpose, Status: "active", NotBefore: now.Add(-time.Minute), NotAfter: now.Add(s.cfg.PlanTTL + time.Minute)}}, Issuer: s.cfg.Issuer, Identity: s.cfg.Signer, Purpose: s.cfg.Purpose, GeneratorKeys: map[string]artifact.KeyRecord{s.cfg.GeneratorKeyID: {PublicKey: s.generatorPublic, Purpose: s.cfg.GeneratorPurpose}}, GeneratorPurpose: s.cfg.GeneratorPurpose}
	p, e = p.BindArtifact(path, a.Digest, bundle, s.private)
	if e != nil {
		return p, e
	}
	s.mu.Lock()
	s.policyMu.Lock()
	s.policies[a.Digest] = policy
	s.policyMu.Unlock()
	s.plans[p.Digest] = p
	s.mu.Unlock()
	return p, nil
}
func (s *productionDownService) ApplyDown(ctx context.Context, p migratedown.DownPlan) (status string, err error) {
	s.mu.Lock()
	stored, ok := s.plans[p.Digest]
	s.mu.Unlock()
	if !ok || stored.Digest != p.Digest {
		return "refused", migratedown.ErrRefused
	}
	snap, e := migrate.LoadSnapshot(s.cfg.MigrationDirectory)
	if e != nil {
		return "refused", e
	}
	session, e := s.store.OpenSession(ctx)
	if e != nil {
		return "refused", e
	}
	defer session.Close(context.WithoutCancel(ctx))
	key, e := executor.LockKey(s.identity, s.environment)
	if e != nil {
		return "refused", e
	}
	locked, e := session.Lock(ctx, key)
	if e != nil || !locked {
		return "busy", executor.ErrBusy
	}
	defer session.Unlock(context.WithoutCancel(ctx), key)
	writers, e := session.LockWriters(ctx)
	if e != nil || !writers {
		return "busy", executor.ErrBusy
	}
	defer session.UnlockWriters(context.WithoutCancel(ctx))
	if e = s.drainOutbox(ctx, session); e != nil {
		return "applied_audit_pending", e
	}
	records, e := session.Revisions(ctx)
	if e != nil || len(records) == 0 {
		return "refused", migratedown.ErrRefused
	}
	for _, r := range records {
		if r.State != "applied" && r.State != "baseline" && r.State != "checkpoint" {
			return "refused", migratedown.ErrRefused
		}
	}
	history, e := session.ExecutorRecords(ctx, s.identity+"/"+s.environment)
	if e != nil {
		return "refused", e
	}
	for _, h := range history {
		if h.State != "confirmed" {
			return "refused", migratedown.ErrRefused
		}
	}
	head := records[len(records)-1]
	if e = p.Verify(s.public, time.Now().UTC(), snap.Manifest, head); e != nil {
		return "refused", e
	}
	if p.ArtifactPath == "" || filepath.Dir(p.ArtifactPath) != filepath.Clean(s.cfg.ArtifactDirectory) {
		return "refused", migratedown.ErrRefused
	}
	raw, e := os.ReadFile(p.ArtifactPath)
	if e != nil {
		return "refused", errors.New("read authorized down artifact")
	}
	a, e := artifact.Parse(raw)
	if e != nil {
		return "refused", e
	}
	v, e := s.verified.VerifyArtifact(a)
	if e != nil || a.Digest != p.ArtifactDigest || a.GuardrailDigest != p.GuardrailDigest {
		return "refused", e
	}
	payload, e := v.Payload()
	if e != nil || payload.Plan.Digest != p.Plan.Digest || payload.Checks.Digest != p.Checks.Digest || payload.SourceRevision != "down:"+p.HeadVersion+":"+p.TargetVersion {
		return "refused", migratedown.ErrRefused
	}
	tx, e := session.Begin(ctx)
	if e != nil {
		return "failed", e
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if e = session.LockMutationTables(ctx, tx); e != nil {
		return "failed", e
	}
	external, e := s.verified.ApplyVersioned(ctx, v, executor.WrapPGX(session.Raw()), executor.WrapPGXTx(tx))
	if e != nil {
		if external.Finalize != nil {
			_ = external.Finalize(context.WithoutCancel(ctx), false)
		}
		return outcome(external), e
	}
	finalized := false
	defer func() {
		if !finalized && external.Finalize != nil {
			_ = external.Finalize(context.WithoutCancel(ctx), false)
		}
	}()
	now := time.Now().UTC()
	version := "zz_down_" + now.Format("20060102T150405.000000000Z")
	originals := make([]string, len(p.Originals))
	for i, x := range p.Originals {
		originals[i] = x.ArtifactDigest
	}
	done := now
	r := revision.Revision{Version: version, Description: "controlled down to " + p.TargetVersion, Kind: "reversal", FileName: "DOWN__" + p.TargetVersion, FileDigest: p.Digest, ManifestDigest: p.ManifestDigest, ManifestGeneration: p.ManifestGeneration, ArtifactDigest: payload.Digest, PlanDigest: payload.Plan.Digest, ChecksDigest: payload.Checks.Digest, BundleDigest: payload.GuardrailDigest, State: "applied", StatementOrdinal: external.Result.AppliedSteps, Attempt: 1, Operator: s.cfg.Operator, StartedAt: now, UpdatedAt: now, CompletedAt: &done, FromVersion: p.HeadVersion, ToVersion: p.TargetVersion, ReversalOf: strings.Join(originals, ",")}
	if e = session.ExecReversal(ctx, tx, r, originals); e != nil {
		return "failed", e
	}
	for _, event := range external.Events {
		b, _ := json.Marshal(event)
		if e = session.EnqueueOutbox(ctx, tx, event.EventID, b); e != nil {
			return "failed", e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return "uncertain", errors.New("down transaction outcome uncertain")
	}
	committed = true
	if external.Finalize != nil {
		if e = external.Finalize(ctx, true); e != nil {
			return "applied_audit_pending", e
		}
	}
	finalized = true
	for _, event := range external.Events {
		if e = s.verified.DrainLifecycle(ctx, event); e != nil {
			return "applied_audit_pending", e
		}
		if e = session.FinalizeOutbox(ctx, event.EventID); e != nil {
			return "applied_audit_pending", e
		}
	}
	return "reversed", nil
}
func (s *productionDownService) drainOutbox(ctx context.Context, session *revision.Session) error {
	pending, e := session.PendingOutbox(ctx)
	if e != nil {
		return e
	}
	for _, x := range pending {
		var event executor.LifecycleEvent
		if json.Unmarshal(x.Payload, &event) != nil || event.EventID != x.ID {
			return errors.New("malformed lifecycle outbox")
		}
		if e = s.verified.DrainLifecycle(ctx, event); e != nil {
			return e
		}
		if e = session.FinalizeOutbox(ctx, x.ID); e != nil {
			return e
		}
	}
	return nil
}
func outcome(x executor.ExternalExecution) string {
	if x.Result.Uncertain {
		return "uncertain"
	}
	if x.Result.Partial {
		return "partial_failure"
	}
	return "failed"
}

var _ = fmt.Sprint
var _ = schema.SchemaVersion
