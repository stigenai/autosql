package cli

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
	"autosql/pkg/migrate"
	migratedown "autosql/pkg/migrate/down"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type downConfig struct {
	MigrationDirectory, RevisionSchema, DevelopmentURLReference, DevelopmentIdentity, PlanSigningKeyReference, PlanSigningKeyID, ArtifactPath, Operator string
	PlanTTL                                                                                                                                             time.Duration
	Reverse                                                                                                                                             []migratedown.ReverseStatement
	Checks                                                                                                                                              []precheck.Assertion
	Override                                                                                                                                            *migratedown.Override
	OverridePublicKeys                                                                                                                                  map[string]string
	ApprovalPolicy                                                                                                                                      approval.Policy
	Actors                                                                                                                                              map[string]approval.Identity
	VerifiedApprovals                                                                                                                                   map[string]approval.VerifiedApproval
	Approvals                                                                                                                                           []approval.Approval
	ApprovalAuditPath                                                                                                                                   string
}
type downAuthority struct {
	actors   map[string]approval.Identity
	verified map[string]approval.VerifiedApproval
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
	cfg                   downConfig
	url                   string
	schemas               []string
	identity, environment string
	devURL                string
	private               ed25519.PrivateKey
	public                ed25519.PublicKey
	overrideKeys          map[string]ed25519.PublicKey
	verified              VerifiedArtifactApplyService
	store                 *revision.Store
	mu                    sync.Mutex
	plans                 map[string]migratedown.DownPlan
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
	if c.MigrationDirectory == "" || c.Operator == "" || c.PlanTTL <= 0 || c.ArtifactPath == "" {
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
	return &productionDownService{cfg: c, url: url, schemas: append([]string(nil), base.Schemas...), identity: base.DatabaseIdentity, environment: base.Environment, devURL: dev, private: private, public: private.Public().(ed25519.PublicKey), overrideKeys: keys, verified: v, store: store, plans: map[string]migratedown.DownPlan{}}, nil
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
	p, e := migratedown.Build(ctx, migratedown.Request{Snapshot: snap, Revisions: records, TargetVersion: to, LockedLive: live, ReplayedPrior: prior, Reverse: s.cfg.Reverse, Checks: s.cfg.Checks, Override: s.cfg.Override, OverrideKeys: s.overrideKeys, Now: now, ExpiresAt: now.Add(s.cfg.PlanTTL), SignerKeyID: s.cfg.PlanSigningKeyID, Signer: s.private, VerifyOriginal: s.verified.VerifyArtifact})
	if e != nil {
		return p, e
	}
	s.mu.Lock()
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
	raw, e := os.ReadFile(s.cfg.ArtifactPath)
	if e != nil {
		return "refused", errors.New("read authorized down artifact")
	}
	a, e := artifact.Parse(raw)
	if e != nil {
		return "refused", e
	}
	v, e := s.verified.VerifyArtifact(a)
	if e != nil {
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
