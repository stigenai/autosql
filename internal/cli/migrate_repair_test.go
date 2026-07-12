package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairCLIProposalAndSecretRedaction(t *testing.T) {
	secret := "password=repair-secret postgres://user:repair-secret@db/x"
	d := t.TempDir()
	p := filepath.Join(d, "proposal.json")
	if e := os.WriteFile(p, []byte(`{"Action":"mark","Reason":"`+secret+`"}`), 0600); e != nil {
		t.Fatal(e)
	}
	var out, err bytes.Buffer
	code := RunWithServices(context.Background(), []string{"migrate", "repair", "mark", "--url", "env://AUTOSQL_MISSING_REPAIR_URL", "--proposal", p, "--operator-public-key", "env://AUTOSQL_MISSING_REPAIR_KEY", "--audit", filepath.Join(d, "audit"), "--json"}, Streams{Out: &out, Err: &err}, Services{})
	if code == 0 {
		t.Fatal("invalid proposal accepted")
	}
	combined := out.String() + err.String()
	if strings.Contains(combined, "repair-secret") || strings.Contains(combined, "postgres://") || strings.Contains(combined, "password=") {
		t.Fatalf("secret leaked: %s", combined)
	}
	if !strings.Contains(combined, "resolve operator key failed") {
		t.Fatalf("unexpected output: %s", combined)
	}
}
