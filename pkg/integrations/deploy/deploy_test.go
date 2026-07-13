package deploy

import "testing"

func valid() Request {
	return Request{DeploymentID: "d1", ArtifactDigest: "sha256:artifact", TargetID: "db1", TargetSnapshot: "sha256:snapshot", Environment: "dev", Action: Plan}
}

func TestTerraformPlanIdempotencyAndDestroyGuard(t *testing.T) {
	r := valid()
	p, err := TerraformPlan(r, nil)
	if err != nil || p.NoOp {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	q, err := TerraformPlan(r, &p)
	if err != nil || !q.NoOp {
		t.Fatalf("q=%+v err=%v", q, err)
	}
	r = valid()
	r.Action = Destroy
	if _, err = TerraformPlan(r, nil); err != ErrDestroy {
		t.Fatalf("destroy guard=%v", err)
	}
}

func TestRequestDoesNotStoreConnectionSecret(t *testing.T) {
	r := valid()
	r.ConnectionRef = "postgres://user:secret@example"
	if err := r.Validate(); err == nil {
		t.Fatal("resolved connection accepted")
	}
}
