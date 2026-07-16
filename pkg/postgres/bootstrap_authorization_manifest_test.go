package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/schema"
)

func TestBootstrapAuthorizationManifestVerifiesExactInventoryAndProducesGates(t *testing.T) {
	inventory, manifest, policy, _, desired, target := signedBootstrapAuthorizationFixture(t)
	verified, err := VerifyBootstrapAuthorizationManifest(manifest, inventory, policy)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := PlanDatabaseBootstrapAuthorized(context.Background(), target, desired, plan.Options{Render: map[string]string{"concurrent_indexes": "true"}}, verified)
	if err != nil {
		t.Fatal(err)
	}
	if whole.Digest != inventory.PlanDigest || whole.SchemaPlan.Digest != inventory.PlanSummary.SchemaPlanDigest {
		t.Fatalf("authorized plan=%s/%s inventory=%s/%s", whole.Digest, whole.SchemaPlan.Digest, inventory.PlanDigest, inventory.PlanSummary.SchemaPlanDigest)
	}
	authorizedResource := ""
	for _, extension := range manifest.Extensions {
		if extension.UntrustedAuthorized {
			authorizedResource = extension.ResourceID
		}
	}
	if authorizedResource == "" || !hasBootstrapExtensionAuthorization(whole, authorizedResource) {
		t.Fatal("verified manifest did not carry untrusted-extension authority into execution")
	}
	capabilityText := string(sealBootstrapExtensionAuthorization(whole.Digest, authorizedResource))
	if strings.Contains(fmt.Sprintf("%+v %#v", whole, whole), capabilityText) {
		t.Fatal("runtime authorization leaked through plan formatting")
	}
	for _, extension := range manifest.Extensions {
		if !extension.UntrustedAuthorized && hasBootstrapExtensionAuthorization(whole, extension.ResourceID) {
			t.Fatalf("untrusted authority leaked to signed-trusted extension %s", extension.Name)
		}
	}
	serialized, err := json.Marshal(whole)
	if err != nil {
		t.Fatal(err)
	}
	var deserialized bootstrap.Plan
	if err := json.Unmarshal(serialized, &deserialized); err != nil {
		t.Fatal(err)
	}
	if hasBootstrapExtensionAuthorization(deserialized, authorizedResource) {
		t.Fatal("serialized plan retained process-local runtime authority")
	}
	crossPlan := whole.WithRuntimeAuthorization(sealBootstrapExtensionAuthorization("sha256:"+strings.Repeat("f", 64), authorizedResource))
	if hasBootstrapExtensionAuthorization(crossPlan, authorizedResource) {
		t.Fatal("capability bound to another plan digest was accepted")
	}
	if _, err := PlanDatabaseBootstrapAuthorized(context.Background(), target, desired, plan.Options{}, VerifiedBootstrapAuthorization{}); err == nil {
		t.Fatal("zero verified token produced authorization")
	}
	otherTarget := target
	otherTarget.Name = "other_database"
	if _, err := PlanDatabaseBootstrapAuthorized(context.Background(), otherTarget, desired, plan.Options{}, verified); err == nil {
		t.Fatal("verified token authorized a different database target")
	}
	otherDesired := desired
	otherDesired.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources[:len(desired.Graph.Resources)-1]...)
	if _, err := PlanDatabaseBootstrapAuthorized(context.Background(), target, otherDesired, plan.Options{}, verified); err == nil {
		t.Fatal("verified token authorized a different source graph")
	}
	raw, err := manifest.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"CREATE FUNCTION", `"definition"`, `"schema_plan":`, `"steps":`, `"sql":`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("manifest contains executable or source material %q", forbidden)
		}
	}
}

func TestBootstrapAuthorizationManifestRejectsTamperingBindingsTimeEntriesAndSignature(t *testing.T) {
	inventory, manifest, policy, private, _, _ := signedBootstrapAuthorizationFixture(t)
	validDigest := func(char string) string { return "sha256:" + strings.Repeat(char, 64) }
	tests := map[string]struct {
		mutate func(*BootstrapAuthorizationManifest, *BootstrapAuthorizationVerifyPolicy)
		resign bool
		want   error
	}{
		"tampered content": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Extensions[0].Schema = "other"
		}},
		"wrong plan digest": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.PlanDigest = validDigest("e")
		}, resign: true, want: ErrStaleBootstrapAuthorization},
		"wrong schema plan digest": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.SchemaPlanDigest = validDigest("e")
		}, resign: true, want: ErrStaleBootstrapAuthorization},
		"wrong source digest": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.SourceDigest = validDigest("e")
		}, resign: true, want: ErrStaleBootstrapAuthorization},
		"wrong issuer metadata": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Issuer = "other"
		}, resign: true, want: ErrInvalidBootstrapAuthorization},
		"wrong signer metadata": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Signer = "other"
		}, resign: true, want: ErrInvalidBootstrapAuthorization},
		"wrong purpose metadata": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Purpose = "other"
		}, resign: true, want: ErrInvalidBootstrapAuthorization},
		"expired": {mutate: func(_ *BootstrapAuthorizationManifest, p *BootstrapAuthorizationVerifyPolicy) {
			p.Now = func() time.Time { return manifest.ExpiresAt }
		}, want: ErrExpiredBootstrapAuthorization},
		"not yet valid": {mutate: func(_ *BootstrapAuthorizationManifest, p *BootstrapAuthorizationVerifyPolicy) {
			p.Now = func() time.Time { return manifest.NotBefore.Add(-time.Second) }
		}, want: ErrExpiredBootstrapAuthorization},
		"unknown routine": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Routines = append(m.Routines, BootstrapRoutineApproval{ResourceID: "function:unknown", Kind: "function", Signature: `"app"."unknown"()`, SourceDigest: validDigest("e"), DigestReviewed: true})
		}, resign: true},
		"missing routine": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Routines = m.Routines[1:]
		}, resign: true},
		"missing digest approval": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Routines[0].DigestReviewed = false
		}, resign: true},
		"overbroad routine approval": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Routines[0].UnsafeLanguageAuthorized = true
		}, resign: true},
		"missing unsafe language approval": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			for index := range m.Routines {
				if m.Routines[index].UnsafeLanguageAuthorized {
					m.Routines[index].UnsafeLanguageAuthorized = false
					return
				}
			}
		}, resign: true},
		"unknown extension": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Extensions = append(m.Extensions, BootstrapExtensionApproval{ResourceID: "extension:unknown", Name: "unknown", Version: "1", Schema: "app", Allowlisted: true})
		}, resign: true},
		"missing extension": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Extensions = m.Extensions[1:]
		}, resign: true},
		"wrong extension version": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Extensions[0].Version = "99"
		}, resign: true},
		"wrong extension schema": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Extensions[0].Schema = "other"
		}, resign: true},
		"missing untrusted approval": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			for index := range m.Extensions {
				if m.Extensions[index].UntrustedAuthorized {
					m.Extensions[index].UntrustedAuthorized = false
					return
				}
			}
		}, resign: true},
		"signature failure": {mutate: func(m *BootstrapAuthorizationManifest, _ *BootstrapAuthorizationVerifyPolicy) {
			m.Signature.Value = strings.Repeat("A", len(m.Signature.Value))
		}},
		"unknown key": {mutate: func(_ *BootstrapAuthorizationManifest, p *BootstrapAuthorizationVerifyPolicy) {
			p.Keys = map[string]artifact.KeyRecord{}
		}},
		"wrong signer": {mutate: func(_ *BootstrapAuthorizationManifest, p *BootstrapAuthorizationVerifyPolicy) { p.Signer = "other" }},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := cloneBootstrapAuthorizationManifest(t, manifest)
			candidatePolicy := policy
			candidatePolicy.Keys = cloneBootstrapAuthorizationKeys(policy.Keys)
			test.mutate(&candidate, &candidatePolicy)
			if test.resign {
				if err := candidate.Sign(manifest.Signature.KeyID, private); err != nil && name != "missing digest approval" {
					t.Fatalf("resign: %v", err)
				}
			}
			_, err := VerifyBootstrapAuthorizationManifest(candidate, inventory, candidatePolicy)
			if err == nil {
				t.Fatal("invalid manifest verified")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
		})
	}
}

func TestBootstrapAuthorizationManifestStrictParsingAndCanonicalRoundTrip(t *testing.T) {
	_, manifest, _, _, _, _ := signedBootstrapAuthorizationFixture(t)
	raw, err := manifest.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseBootstrapAuthorizationManifest(raw)
	if err != nil || parsed.Digest != manifest.Digest {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSuffix(raw, []byte{'\n'}), "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["unknown"] = true
	unknown, _ := json.Marshal(value)
	unknown = append(unknown, '\n')
	delete(value, "unknown")
	reordered, _ := json.Marshal(value)
	reordered = append(reordered, '\n')
	issuedCanonical := manifest.IssuedAt.Format(time.RFC3339)
	issuedAlternate := manifest.IssuedAt.Format("2006-01-02T15:04:05+00:00")
	purpose := `"purpose":"` + manifest.Purpose + `"`
	digest := `"digest":"` + manifest.Digest + `"`
	deep := []byte(strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + "\n")
	tooLarge := bytes.Repeat([]byte{' '}, (4<<20)+1)
	invalidUTF8 := append(append([]byte(nil), raw...), 0xff)
	tests := map[string][]byte{
		"leading whitespace":        append([]byte{' '}, raw...),
		"trailing whitespace":       append(append([]byte(nil), raw...), ' '),
		"missing canonical newline": bytes.TrimSuffix(raw, []byte{'\n'}),
		"pretty JSON":               pretty.Bytes(),
		"reordered fields":          reordered,
		"alternate time spelling":   []byte(strings.Replace(string(raw), issuedCanonical, issuedAlternate, 1)),
		"duplicate purpose":         []byte(strings.Replace(string(raw), purpose, purpose+","+purpose, 1)),
		"duplicate digest":          []byte(strings.Replace(string(raw), digest, digest+","+digest, 1)),
		"unknown field":             unknown,
		"invalid UTF-8":             invalidUTF8,
		"excessive depth":           deep,
		"excessive size":            tooLarge,
		"invalid JSON structure":    []byte("{\n"),
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBootstrapAuthorizationManifest(candidate); err == nil {
				t.Fatalf("noncanonical or unsafe manifest accepted (%d bytes)", len(candidate))
			}
		})
	}
}

func signedBootstrapAuthorizationFixture(t *testing.T) (BootstrapAuthorizationInventory, BootstrapAuthorizationManifest, BootstrapAuthorizationVerifyPolicy, ed25519.PrivateKey, schema.Document, bootstrap.DatabaseTarget) {
	t.Helper()
	desired, target := authorizationInventoryFixture(t)
	for index := range desired.Graph.Resources {
		if desired.Graph.Resources[index].Kind != "extension" || desired.Graph.Resources[index].Name.Name != "pgcrypto" {
			continue
		}
		var spec map[string]any
		if err := json.Unmarshal(desired.Graph.Resources[index].Spec, &spec); err != nil {
			t.Fatal(err)
		}
		spec["trusted"] = false
		spec["superuser"] = true
		desired.Graph.Resources[index].Spec, _ = json.Marshal(spec)
	}
	inventory, err := PrepareBootstrapAuthorizationInventory(context.Background(), target, desired, BootstrapAuthorizationInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	manifest, err := NewBootstrapAuthorizationManifest(inventory, now.Add(-time.Minute), now.Add(-time.Minute), now.Add(time.Hour), "security", "dba-reviewer", "bootstrap-authorization")
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Sign("bootstrap-key", private); err != nil {
		t.Fatal(err)
	}
	policy := BootstrapAuthorizationVerifyPolicy{Now: func() time.Time { return now }, Keys: map[string]artifact.KeyRecord{"bootstrap-key": {PublicKey: public, Issuer: "security", Identity: "dba-reviewer", Purpose: "bootstrap-authorization", Status: "active", NotBefore: now.Add(-time.Hour), NotAfter: now.Add(2 * time.Hour)}}, Issuer: "security", Signer: "dba-reviewer", Purpose: "bootstrap-authorization"}
	return inventory, manifest, policy, private, desired, target
}

func cloneBootstrapAuthorizationManifest(t *testing.T, manifest BootstrapAuthorizationManifest) BootstrapAuthorizationManifest {
	t.Helper()
	raw, err := manifest.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	clone, err := ParseBootstrapAuthorizationManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return clone
}

func cloneBootstrapAuthorizationKeys(keys map[string]artifact.KeyRecord) map[string]artifact.KeyRecord {
	clone := make(map[string]artifact.KeyRecord, len(keys))
	for key, value := range keys {
		clone[key] = value
	}
	return clone
}
