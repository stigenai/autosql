package cli

import (
	"autosql/pkg/approval"
	"autosql/pkg/migrate/down"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeDown struct {
	p       down.DownPlan
	err     error
	applied bool
}

func TestDownAuthorityBindsTrustedExternalProofToFreshBundle(t *testing.T) {
	now := time.Now().UTC()
	a := downAuthority{verified: map[string]approval.VerifiedApproval{"opaque-proof": {Identity: approval.Identity{ID: "reviewer", Roles: []string{"dba"}}, ApprovedAt: now, ExpiresAt: now.Add(time.Hour)}}}
	v, err := a.VerifyApproval(context.Background(), approval.Approval{Approver: "reviewer", Proof: "opaque-proof", PlanDigest: "sha256:fresh", Environment: "production"})
	if err != nil || v.PlanDigest != "sha256:fresh" || v.Environment != "production" {
		t.Fatalf("verified=%+v err=%v", v, err)
	}
	if _, err = a.VerifyApproval(context.Background(), approval.Approval{Approver: "other", Proof: "opaque-proof", PlanDigest: "sha256:fresh", Environment: "production"}); err == nil {
		t.Fatal("trusted proof replayed by another approver")
	}
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

func TestMigrateDownHumanAndJSONErrorsRedactSecrets(t *testing.T) {
	secretValue := "postgres://down-user:down-password@production/down"
	for _, jsonMode := range []bool{false, true} {
		args := []string{"migrate", "down", "--to", "1", "--dry-run"}
		if jsonMode {
			args = append(args, "--json")
		}
		code, stdout, stderr := invoke(t, args, "", false, Services{Down: &fakeDown{err: errors.New(secretValue)}})
		if code == 0 || strings.Contains(stdout, secretValue) || strings.Contains(stderr, secretValue) || strings.Contains(stdout, "down-password") || strings.Contains(stderr, "down-password") {
			t.Fatalf("json=%v code=%d stdout=%q stderr=%q", jsonMode, code, stdout, stderr)
		}
	}
}
