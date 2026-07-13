package integration

import "testing"

func image() Image {
	return Image{Name: "ghcr.io/acme/autosql", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Version: "v1.0.0", Signature: "cosign", SBOM: "spdx.json", Scanned: true, Reproducible: true}
}
func TestImageAndCacheContracts(t *testing.T) {
	i := image()
	if err := i.Validate(); err != nil {
		t.Fatal(err)
	}
	i.Digest = "latest"
	if err := i.Validate(); err != ErrUnpinnedImage {
		t.Fatalf("expected pin error, got %v", err)
	}
	i = image()
	if err := VerifyCache(CacheRecord{Key: "k", Digest: i.Digest}, i.Digest); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCache(CacheRecord{Key: "k", Digest: i.Digest}, "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); err != ErrCacheMismatch {
		t.Fatalf("expected mismatch, got %v", err)
	}
}
func TestTrustBoundary(t *testing.T) {
	if err := (Stage{Name: "pr", Trust: UntrustedPR, Credentials: []string{"db"}}).Validate(); err == nil {
		t.Fatal("credentials crossed PR boundary")
	}
}
