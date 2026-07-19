package workloadidentity

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autosql/pkg/security"
)

func binding(provider Provider) Binding {
	region := ""
	audience := "api://AzureADTokenExchange"
	if provider == AWSRDS {
		region, audience = "us-east-1", "sts.amazonaws.com"
	}
	if provider == GCPCloud {
		audience = "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/test/providers/kubernetes"
	}
	return Binding{Provider: provider, Host: "db.example.test", Port: 5432, User: "autosql", Database: "app", TLSMode: "verify-full", Region: region, Audience: audience, Subject: "workload:test"}
}

func TestProviderBindingsAreTargetAndAudienceBound(t *testing.T) {
	for _, provider := range []Provider{AWSRDS, GCPCloud, AzurePG} {
		t.Run(string(provider), func(t *testing.T) {
			first := binding(provider)
			d1, err := first.Digest()
			if err != nil {
				t.Fatal(err)
			}
			first.Host = "other.example.test"
			d2, err := first.Digest()
			if err != nil || d1 == d2 {
				t.Fatalf("digest=%q changed=%q err=%v", d1, d2, err)
			}
			first.Audience = ""
			if _, err := first.Digest(); err == nil {
				t.Fatal("empty audience accepted")
			}
		})
	}
}

func TestFreshnessRotationRevocationAndRedaction(t *testing.T) {
	var calls atomic.Int32
	revoked := atomic.Bool{}
	source, err := NewSource(binding(AWSRDS), func(context.Context) (string, time.Time, error) {
		calls.Add(1)
		if revoked.Load() {
			return "super-secret-token", time.Time{}, errors.New("provider leaked super-secret-token")
		}
		return "token", time.Now().Add(31 * time.Second), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	session := &security.Session{Source: source, RefreshBefore: 30*time.Second + 900*time.Millisecond}
	if _, err := session.Credential(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := session.Credential(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() < 2 {
		t.Fatalf("token was not rotated: calls=%d", calls.Load())
	}
	revoked.Store(true)
	session.Clear()
	_, err = session.Credential(context.Background())
	if !errors.Is(err, ErrIdentity) || strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("unsafe revocation error: %v", err)
	}
}

func TestRejectsExpiredAndImplausiblyLongTokens(t *testing.T) {
	for _, expiry := range []time.Time{time.Now().Add(-time.Minute), time.Now().Add(3 * time.Hour)} {
		source, err := NewSource(binding(GCPCloud), func(context.Context) (string, time.Time, error) { return "token", expiry, nil })
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := source.Token(context.Background()); !errors.Is(err, ErrIdentity) {
			t.Fatalf("expiry=%v err=%v", expiry, err)
		}
	}
}
