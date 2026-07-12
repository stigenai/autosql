package cli

import (
	"autosql/pkg/migrate/down"
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeDown struct {
	p       down.DownPlan
	err     error
	applied bool
}

func (f *fakeDown) PlanDown(context.Context, string) (down.DownPlan, error) { return f.p, f.err }
func (f *fakeDown) ApplyDown(context.Context, down.DownPlan) (string, error) {
	f.applied = true
	return "reversed", f.err
}
func TestMigrateDownDryRunNeverAppliesAndFailureGuides(t *testing.T) {
	s := &fakeDown{p: down.DownPlan{Digest: "sha256:plan", Impacts: []down.Impact{{Destructive: true, Preconditions: []string{"empty"}}}}}
	out := &strings.Builder{}
	if err := runMigrateDown(context.Background(), []string{"--to", "1", "--dry-run"}, output{streams: Streams{Out: out, Err: &strings.Builder{}}}, s); err != nil || s.applied {
		t.Fatalf("err=%v applied=%v", err, s.applied)
	}
	if !strings.Contains(out.String(), "DESTRUCTIVE") || !strings.Contains(out.String(), "precondition: empty") {
		t.Fatal("human dry run omitted impacts or preconditions")
	}
	s.err = errors.New("secret postgres://u:p@db")
	err := runMigrateDown(context.Background(), []string{"--to", "1", "--dry-run"}, output{streams: Streams{Out: &strings.Builder{}, Err: &strings.Builder{}}}, s)
	var ce *Error
	if !errors.As(err, &ce) || ce.RecoveryGuidance == "" {
		t.Fatalf("missing recovery guidance: %v", err)
	}
}
