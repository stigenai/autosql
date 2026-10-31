package dbtest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLease(t *testing.T) *leaseFile {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := createLease(root, leaseFactory{Mode: "existing_admin", Name: "development", Admin: "env://DBTEST_ADMIN", AdminIdentity: "127.0.0.1:5432/development#123", PostgresMajor: 17}, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.close)
	return f
}

func TestLeasePersistsIncrementalOwnershipAndRefusesConcurrentCleanup(t *testing.T) {
	f := newTestLease(t)
	if _, err := openLeaseFile(f.path); !errors.Is(err, ErrLease) {
		t.Fatalf("active lease allowed a concurrent cleanup handle: %v", err)
	}
	next := f.snapshot()
	next.Resources = []leaseResource{{Kind: "role", Name: next.resourceName(), Marker: next.marker(), State: "planned"}}
	if err := f.save(next); err != nil {
		t.Fatal(err)
	}
	next.Resources[0].State, next.Resources[0].OID = "acquired", 123
	if err := f.save(next); err != nil {
		t.Fatal(err)
	}
	next.Resources[0].OID = 999 // published state must not alias the caller
	if f.snapshot().Resources[0].OID != 123 {
		t.Fatal("caller mutated published lease ownership")
	}
	if err := f.save(next); !errors.Is(err, ErrLease) {
		t.Fatal("acquired object identity was rewritten")
	}
	f.close()
	reopened, err := openLeaseFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if reopened.snapshot().Resources[0].OID != 123 {
		t.Fatal("durable ownership lost across reopen")
	}
	next = reopened.snapshot()
	next.State = "cleaning"
	if err := reopened.save(next); err != nil {
		t.Fatal(err)
	}
	next.Resources[0].State, next.State = "released", "cleaned"
	if err := reopened.save(next); err != nil {
		t.Fatal(err)
	}
	next.State = "allocating"
	if err := reopened.save(next); !errors.Is(err, ErrLease) {
		t.Fatal("cleaned lease was recycled")
	}
}

func TestLeaseRefusesIncompleteReadyAndLostOwnershipRecords(t *testing.T) {
	f := newTestLease(t)
	for _, mutate := range []func(*leaseRecord){
		func(r *leaseRecord) { r.State = "ready" },
		func(r *leaseRecord) { r.CredentialRef = "../credentials.json" },
		func(r *leaseRecord) { r.RunID = strings.Repeat("a", 32) },
		func(r *leaseRecord) { r.Factory.Admin = "env://OTHER_ADMIN" },
		func(r *leaseRecord) {
			r.Resources = []leaseResource{{Kind: "role", Name: "existing_user", Marker: r.marker(), State: "planned"}}
		},
	} {
		next := f.snapshot()
		mutate(&next)
		if err := f.save(next); !errors.Is(err, ErrLease) {
			t.Fatalf("invalid next allocation accepted: %v", err)
		}
	}
	next := f.snapshot()
	next.Resources = []leaseResource{{Kind: "role", Name: next.resourceName(), Marker: next.marker(), State: "planned"}}
	if err := f.save(next); err != nil {
		t.Fatal(err)
	}
	next.Resources = nil
	if err := f.save(next); !errors.Is(err, ErrLease) {
		t.Fatal("recorded resource was forgotten before cleanup")
	}
}

func TestLeaseRefusesTamperingLinksAndUnsafePermissions(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, string){
		"corrupt digest": func(t *testing.T, path string) {
			raw, _ := os.ReadFile(path)
			raw = bytes.Replace(raw, []byte(`"postgres_major":17`), []byte(`"postgres_major":18`), 1)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"duplicate key": func(t *testing.T, path string) {
			raw, _ := os.ReadFile(path)
			raw = bytes.Replace(raw, []byte(`"state":"allocating"`), []byte(`"state":"allocating","state":"allocating"`), 1)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"public file": func(t *testing.T, path string) {
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(t *testing.T, path string) {
			if err := os.Link(path, path+".copy"); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, path string) {
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".original", path); err != nil {
				t.Fatal(err)
			}
		},
		"public directory": func(t *testing.T, path string) {
			if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTestLease(t)
			f.close()
			mutate(t, f.path)
			if reopened, err := openLeaseFile(f.path); !errors.Is(err, ErrLease) {
				if reopened != nil {
					reopened.close()
				}
				t.Fatalf("unsafe lease reopened: %v", err)
			}
		})
	}
}

func TestLeaseAllocationsAreUniqueAndContainNoCredentialValues(t *testing.T) {
	a, b := newTestLease(t), newTestLease(t)
	if a.record.CaseID == b.record.CaseID || a.record.RunID == b.record.RunID || a.record.resourceName() == b.record.resourceName() {
		t.Fatal("independent allocations shared an ownership identity")
	}
	raw, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("password")) || !bytes.Contains(raw, []byte("env://DBTEST_ADMIN")) {
		t.Fatal("lease failed opaque credential-reference contract")
	}
	if _, err := createLease(t.TempDir(), a.record.Factory, time.Now().Add(-time.Minute)); !errors.Is(err, ErrLease) {
		t.Fatal("expired new allocation accepted")
	}
}

func TestLeaseFailedPersistenceDoesNotPublishCandidate(t *testing.T) {
	f := newTestLease(t)
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(f.path)
	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0700) })
	next := f.snapshot()
	next.State = "cleaning"
	if err := f.save(next); !errors.Is(err, ErrLease) {
		t.Fatalf("unwritable lease directory accepted update: %v", err)
	}
	after, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || f.snapshot().State != "allocating" {
		t.Fatal("failed write published or aliased the candidate")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.save(next); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseRejectsCredentialValuesInFactoryMetadata(t *testing.T) {
	base := newTestLease(t).snapshot().Factory
	for _, mutate := range []func(*leaseFactory){
		func(f *leaseFactory) { f.Admin = "postgres://admin:password@host/db" },
		func(f *leaseFactory) { f.AdminIdentity = "postgres://admin:password@host/db" },
		func(f *leaseFactory) {
			f.Mode, f.Admin, f.Endpoint, f.Image = "container", "", "tcp://admin:password@host:2375", "postgres@sha256:"+strings.Repeat("a", 64)
		},
		func(f *leaseFactory) {
			f.Mode, f.Admin, f.Endpoint, f.Image = "container", "", "unix:///var/run/docker.sock", "postgres:17"
		},
	} {
		factory := base
		mutate(&factory)
		if err := factory.validate(); !errors.Is(err, ErrLease) {
			t.Fatal("unsafe factory metadata would be serialized")
		}
	}
}

func TestLeaseAcceptsPostgresNetworkPrefixIdentity(t *testing.T) {
	for _, identity := range []string{"127.0.0.1/32:5432/development#123", "[::1/128]:5432/development#123", "127.0.0.1:5432/development#123"} {
		if !leaseServerIdentity.MatchString(identity) {
			t.Errorf("runtime identity refused: %s", identity)
		}
	}
	for _, identity := range []string{"postgres://admin:password@host/db", "host/db?password=x#123", "host/db#not-a-cluster-id"} {
		if leaseServerIdentity.MatchString(identity) {
			t.Error("unsafe identity accepted")
		}
	}
}
