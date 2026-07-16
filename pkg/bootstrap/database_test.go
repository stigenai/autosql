package bootstrap

import "testing"

func TestDatabaseTargetNormalizeAndValidate(t *testing.T) {
	target := DatabaseTarget{Mode: ManagedDatabase, Endpoint: ServerEndpoint{Host: "db.internal"}, Name: "cell", Owner: "cell_owner", ConnectionLimit: -1, AllowConnections: true}.Normalize()
	if target.Endpoint.Port != 5432 || target.Endpoint.TLSMode != "require" || target.MaintenanceDatabase != "postgres" || target.Encoding != "UTF8" || target.Template != "template0" || target.Tablespace != "pg_default" {
		t.Fatalf("target=%+v", target)
	}
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
	target.ICULocale = "en-US"
	if err := target.Validate(); err == nil {
		t.Fatal("ICU locale without ICU provider passed")
	}
}
