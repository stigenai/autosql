package artifact

import (
	"context"
	"errors"
	"testing"
)

type recordingAuthorizer struct {
	actions []Action
	deny    Action
}

func (a *recordingAuthorizer) Authorize(_ context.Context, action Action, _, _ string) error {
	a.actions = append(a.actions, action)
	if action == a.deny {
		return errors.New("denied")
	}
	return nil
}

func TestManagedRegistryPushTagsAndHistory(t *testing.T) {
	a, pub, _ := fixture(t)
	v, err := a.VerifyTrusted(trustedPolicy(a, pub, a.CreatedAt))
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewIntegrityManifest(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := &recordingAuthorizer{}
	r := NewManagedRegistry(NewMemoryRegistry(), auth, nil)
	ctx := context.Background()
	if err := r.Push(ctx, PushRequest{Artifact: v, Manifest: m, Actor: "builder"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Promote(ctx, "prod/latest", a.Digest, "release"); err != nil {
		t.Fatal(err)
	}
	if _, rec, err := r.ResolveTag(ctx, "prod/latest", "reader"); err != nil || rec.Digest != a.Digest || rec.Sequence != 1 {
		t.Fatalf("resolve=%+v err=%v", rec, err)
	}
	if err := r.Promote(ctx, "prod/latest", a.Digest, "release"); err != nil {
		t.Fatal(err)
	}
	history, err := r.TagHistory(ctx, "prod/latest", "reader")
	if err != nil || len(history) != 2 || history[1].PreviousDigest != a.Digest {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	if len(auth.actions) != 5 || auth.actions[0] != ActionPush || auth.actions[1] != ActionPromotion || auth.actions[2] != ActionRead {
		t.Fatalf("authorization actions=%v", auth.actions)
	}
}

func TestManagedRegistryRejectsIntegrityAndRequiredAttestationFailures(t *testing.T) {
	a, pub, _ := fixture(t)
	v, err := a.VerifyTrusted(trustedPolicy(a, pub, a.CreatedAt))
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewIntegrityManifest(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := NewManagedRegistry(NewMemoryRegistry(), nil, nil)
	m.ArtifactDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := r.Push(context.Background(), PushRequest{Artifact: v, Manifest: m}); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	m, _ = NewIntegrityManifest(v, nil)
	r = NewManagedRegistry(NewMemoryRegistry(), nil, []string{"safety"})
	if err := r.Push(context.Background(), PushRequest{Artifact: v, Manifest: m}); err == nil {
		t.Fatal("missing required attestation accepted")
	}
	m, _ = NewIntegrityManifest(v, []string{"safety"})
	r = NewManagedRegistry(NewMemoryRegistry(), nil, nil)
	if err := r.Push(context.Background(), PushRequest{Artifact: v, Manifest: m}); err == nil {
		t.Fatal("manifest-declared attestation accepted")
	}
}

func TestManagedRegistryReadAuthorizationIsSeparate(t *testing.T) {
	a, pub, _ := fixture(t)
	v, err := a.VerifyTrusted(trustedPolicy(a, pub, a.CreatedAt))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := NewIntegrityManifest(v, nil)
	auth := &recordingAuthorizer{deny: ActionRead}
	r := NewManagedRegistry(NewMemoryRegistry(), auth, nil)
	if err := r.Push(context.Background(), PushRequest{Artifact: v, Manifest: m, Actor: "builder"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(context.Background(), a.Digest, "reader"); err == nil {
		t.Fatal("read authorization bypassed")
	}
}
