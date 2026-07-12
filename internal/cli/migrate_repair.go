package cli

import (
	"autosql/pkg/artifact"
	"autosql/pkg/migrate/repair"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

type repairAuditFile struct{ path string }

func (a repairAuditFile) AppendDurable(_ context.Context, r repair.AuditRecord) error {
	f, e := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return errors.New("open repair audit")
	}
	defer f.Close()
	raw, e := json.Marshal(r)
	if e != nil {
		return e
	}
	raw = append(raw, '\n')
	if _, e = f.Write(raw); e != nil {
		return errors.New("write repair audit")
	}
	return f.Sync()
}

func repairStore(ctx context.Context, urlRef, schemaName string, redactor *secret.Redactor) (*revision.Store, error) {
	ref := secret.Reference(urlRef)
	if ref.Validate() != nil {
		return nil, &Error{Kind: "secret", Message: "invalid database secret reference", Code: ExitSecret}
	}
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	u, e := resolver.Resolve(ctx, ref)
	if e != nil {
		return nil, &Error{Kind: "secret", Message: "resolve database failed", Code: ExitSecret}
	}
	if schemaName == "" {
		schemaName = "autosql_revision"
	}
	s, e := revision.Open(revision.Config{URL: u, Schema: schemaName})
	if e != nil {
		return nil, e
	}
	return s, nil
}

func runMigrateDiagnose(ctx context.Context, args []string, o output, services Services, redactor *secret.Redactor) error {
	fs := newFlags("migrate diagnose", o.streams.Err)
	urlRef := fs.String("url", "", "database URL secret reference")
	dir := fs.String("migration-dir", "", "verified migration directory")
	rs := fs.String("revision-schema", "autosql_revision", "revision schema")
	jf := fs.Bool("json", false, "JSON")
	var schemas stringList
	fs.Var(&schemas, "schema", "managed schema")
	if e := fs.Parse(args); e != nil || *urlRef == "" || *dir == "" || fs.NArg() != 0 {
		return usageError(errors.New("--url and --migration-dir required"))
	}
	store, e := repairStore(ctx, *urlRef, *rs, redactor)
	if e != nil {
		return e
	}
	v, ok := services.Apply.(interface {
		VerifyArtifact(artifact.Artifact) (artifact.VerifiedArtifact, error)
	})
	if !ok {
		return &Error{Kind: "config", Message: "trusted artifact verifier required", Code: ExitConfig}
	}
	ref := secret.Reference(*urlRef)
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	u, e := resolver.Resolve(ctx, ref)
	if e != nil {
		return e
	}
	fp := func(ctx context.Context) (string, error) {
		doc, e := postgres.InspectURL(ctx, u, postgres.Options{Schemas: schemas.value()})
		if e != nil {
			return "", e
		}
		doc, e = postgres.New().Normalize(ctx, doc)
		if e != nil {
			return "", e
		}
		return schema.SemanticFingerprint(doc)
	}
	result, e := (repair.Service{Store: store, Verify: v.VerifyArtifact, LiveFingerprint: fp, LockIdentity: "autosql.migrate.repair/v1/" + *rs}).Diagnose(ctx, *dir)
	if e != nil {
		return &Error{Kind: "migration", Message: "diagnosis failed", Code: ExitMigration, Cause: e}
	}
	o.json = *jf
	human := result.Status
	if result.First != nil {
		human = result.First.RootCause + "\n" + result.First.SuggestedCommand
	}
	return o.success(result, redactor.String(human))
}

func runMigrateRepair(ctx context.Context, action string, args []string, o output, services Services, redactor *secret.Redactor) error {
	fs := newFlags("migrate repair "+action, o.streams.Err)
	urlRef := fs.String("url", "", "database URL secret reference")
	rs := fs.String("revision-schema", "autosql_revision", "revision schema")
	proposalPath := fs.String("proposal", "", "signed proposal")
	keyRef := fs.String("operator-public-key", "", "public key secret reference")
	audit := fs.String("audit", "", "repair audit path")
	jf := fs.Bool("json", false, "JSON")
	if e := fs.Parse(args); e != nil || *urlRef == "" || *proposalPath == "" || *keyRef == "" || *audit == "" || fs.NArg() != 0 {
		return usageError(errors.New("--url, --proposal, --operator-public-key and --audit required"))
	}
	raw, e := os.ReadFile(*proposalPath)
	if e != nil {
		return &Error{Kind: "config", Message: "read repair proposal failed", Code: ExitConfig}
	}
	var p repair.Proposal
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if e = d.Decode(&p); e != nil || p.Action != action {
		return &Error{Kind: "config", Message: "invalid repair proposal", Code: ExitConfig}
	}
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	text, e := resolver.Resolve(ctx, secret.Reference(*keyRef))
	if e != nil {
		return &Error{Kind: "secret", Message: "resolve operator key failed", Code: ExitSecret}
	}
	pub, e := base64.RawStdEncoding.Strict().DecodeString(strings.TrimSpace(text))
	if e != nil || len(pub) != ed25519.PublicKeySize {
		return &Error{Kind: "config", Message: "invalid operator key", Code: ExitConfig}
	}
	store, e := repairStore(ctx, *urlRef, *rs, redactor)
	if e != nil {
		return e
	}
	authorizer, ok := services.Apply.(interface {
		AuthorizeRepair(context.Context, repair.Proposal, revision.Revision) error
	})
	if !ok {
		return &Error{Kind: "config", Message: "production repair authorization required", Code: ExitConfig}
	}
	svc := repair.Service{Store: store, Audit: repairAuditFile{*audit}, Keys: map[string]ed25519.PublicKey{p.Signature.KeyID: ed25519.PublicKey(pub)}, Now: time.Now, LockIdentity: "autosql.migrate.repair/v1/" + *rs, Authorize: authorizer.AuthorizeRepair}
	if e = svc.Apply(ctx, p); e != nil {
		return &Error{Kind: "migration", Message: "repair refused", Code: ExitMigration, Cause: e}
	}
	o.json = *jf
	return o.success(map[string]string{"status": "applied", "proposal_digest": p.Digest}, "repair applied")
}
