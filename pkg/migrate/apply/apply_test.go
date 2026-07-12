package apply

import (
	"autosql/pkg/artifact"
	"autosql/pkg/migrate"
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunRejectsIncompleteTrustAndBoundsBeforeConnection(t *testing.T) {
	e := Engine{}
	for _, r := range []Request{{}, {Operator: "op", TargetIdentity: "db", Transaction: "file"}, {Operator: "op", TargetIdentity: "db", Transaction: "all", DryRun: true, Baseline: true}} {
		if _, err := e.Run(context.Background(), r); !errors.Is(err, ErrRefused) {
			t.Fatalf("request %+v: %v", r, err)
		}
	}
}

func TestCheckpointRevisionDurablySupersedesCoveredRange(t *testing.T) {
	m := migrate.Manifest{Entries: []migrate.Migration{{Version: "1.0.0"}, {Version: "2.0.0"}, {Version: "3.0.0", Kind: "checkpoint", CoveredFrom: "1.0.0", CoveredTo: "2.0.0"}}}
	c := candidate{entry: m.Entries[2], payload: artifact.Artifact{Metadata: map[string]string{"autosql.migration.from": "sha256:from"}}}
	x := baseRevision(c, m, Request{Operator: "op"}, time.Now().UTC(), "pending", "checkpoint")
	if len(x.Supersedes) != 2 || x.Supersedes[0] != "1.0.0" || x.Supersedes[1] != "2.0.0" {
		t.Fatalf("coverage not persisted: %+v", x.Supersedes)
	}
}

func TestCanonicalVersion(t *testing.T) {
	if got := canonicalVersion("2"); got != "2.0.0" {
		t.Fatal(got)
	}
	if canonicalVersion("bad") != "" {
		t.Fatal("accepted invalid version")
	}
}
