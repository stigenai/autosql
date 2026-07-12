package apply

import (
	"context"
	"errors"
	"testing"
)

func TestRunRejectsIncompleteTrustAndBoundsBeforeConnection(t *testing.T) {
	e := Engine{}
	for _, r := range []Request{{}, {Operator: "op", TargetIdentity: "db", Transaction: "file"}, {Operator: "op", TargetIdentity: "db", Transaction: "all", DryRun: true, Baseline: true}} {
		if _, err := e.Run(context.Background(), r); !errors.Is(err, ErrRefused) {
			t.Fatalf("request %+v: %v", r, err)
		}
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
