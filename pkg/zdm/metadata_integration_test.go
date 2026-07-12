package zdm

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func liveStore(t *testing.T) (*Store, *pgx.Conn, string, string) {
	t.Helper()
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	suffix := strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	if len(suffix) > 24 {
		suffix = suffix[len(suffix)-24:]
	}
	n := fmt.Sprintf("zdm_%d_%s", time.Now().UnixNano()%1e8, suffix)
	app := n + "_app"
	_, err = c.Exec(ctx, "create schema "+q(app)+`; create table `+q(app, "accounts")+`(id bigint primary key,balance integer not null); insert into `+q(app, "accounts")+` values(7,42)`)
	if err != nil {
		c.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "drop schema if exists "+q(n)+" cascade; drop schema if exists "+q(app)+" cascade")
		c.Close(context.Background())
	})
	s, err := Open(Config{URL: url, Schema: n})
	if err != nil {
		t.Fatal(err)
	}
	return s, c, n, app
}

func TestLiveInitConcurrentIdempotentAndIncompatible(t *testing.T) {
	s, c, n, _ := liveStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Init(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.Status(ctx)
	if err != nil || !st.Initialized || st.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("status=%+v err=%v", st, err)
	}
	var count int
	if err = c.QueryRow(ctx, `select count(*) from `+q(n, "meta")).Scan(&count); err != nil || count != 1 {
		t.Fatalf("meta rows=%d err=%v", count, err)
	}
	_, err = c.Exec(ctx, `update `+q(n, "meta")+` set schema_version=999`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Status(ctx)
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("want incompatible, got %v", err)
	}
}

func TestLiveInitCrashRollsBackAndRetryRecovers(t *testing.T) {
	s, c, n, _ := liveStore(t)
	ctx := context.Background()
	boom := errors.New("simulated crash")
	err := s.InitWithHooks(ctx, InitHooks{AfterVersion: func(v int) error {
		if v == 2 {
			return boom
		}
		return nil
	}})
	if !errors.Is(err, boom) {
		t.Fatalf("want crash, got %v", err)
	}
	var exists bool
	if err = c.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1)`, n).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("partial namespace survived rollback")
	}
	if err = s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx)
	if err != nil || st.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("retry status=%+v err=%v", st, err)
	}
}

func TestLiveExplicitUpgradeFromSupportedV1AndCorruptLayoutRefusal(t *testing.T) {
	s, c, n, _ := liveStore(t)
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `create schema `+q(n)); err != nil {
		t.Fatal(err)
	}
	if err = s.createV1(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx)
	if err != nil || st.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("upgraded status=%+v err=%v", st, err)
	}
	if _, err = c.Exec(ctx, `alter table `+q(n, "baselines")+` drop column fingerprint`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Status(ctx)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt layout accepted: %v", err)
	}
}

func TestLiveExplicitUpgradeFromSupportedV2PreservesEvidence(t *testing.T) {
	s, c, n, _ := liveStore(t)
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `create schema `+q(n)); err != nil {
		t.Fatal(err)
	}
	if err = s.createV1(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err = s.upgrade(ctx, tx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `insert into `+q(n, "baselines")+`(baseline_id,target_identity,environment,fingerprint,canonical_schema,operator_identity,created_at) values('old','t','e','fp','{}'::jsonb,'o',clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	var typ, value string
	if err = c.QueryRow(ctx, `select schema_version from `+q(n, "meta")+` where singleton`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = c.QueryRow(ctx, `select format_type(a.atttypid,a.atttypmod) from pg_attribute a where a.attrelid=$1::regclass and a.attname='canonical_schema'`, n+".baselines").Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if err = c.QueryRow(ctx, `select canonical_schema from `+q(n, "baselines")+` where baseline_id='old'`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion || typ != "text" || value != "{}" {
		t.Fatalf("upgrade version=%d type=%s value=%q", version, typ, value)
	}
}

func TestLiveTrustedManifestRejectsEveryMaterialDeviation(t *testing.T) {
	cases := map[string]string{
		"type":             `alter table %s alter column active_version type varchar`,
		"default":          `alter table %s alter column active_version drop default`,
		"nullability":      `alter table %s alter column active_version drop not null`,
		"progress_check":   `alter table %s drop constraint operations_progress_check`,
		"omitted_operator": `alter table %s drop column operator_identity`,
		"extra_column":     `alter table %s add column attacker text`,
		"extra_index":      `create index attacker_index on %s(active_version)`,
		"partial_index":    `create index attacker_partial on %s(active_version) where active_version<>''`,
		"expression_index": `create index attacker_expression on %s((lower(active_version)))`,
		"persistence":      `alter table %s set unlogged`,
		"rls":              `alter table %s enable row level security`,
	}
	for name, format := range cases {
		t.Run(name, func(t *testing.T) {
			s, c, n, _ := liveStore(t)
			ctx := context.Background()
			if err := s.Init(ctx); err != nil {
				t.Fatal(err)
			}
			table := "meta"
			if name == "progress_check" {
				table = "operations"
			}
			if name == "omitted_operator" {
				table = "baselines"
			}
			sql := fmt.Sprintf(format, q(n, table))
			if _, err := c.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("mutation accepted: %v", err)
			}
		})
	}
	t.Run("relkind", func(t *testing.T) {
		s, c, n, _ := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `drop table `+q(n, "audit")+` cascade; create view `+q(n, "audit")+` as select 1::bigint as sequence`); err != nil {
			t.Fatal(err)
		}
		if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("relkind accepted: %v", err)
		}
	})
	t.Run("ownership", func(t *testing.T) {
		s, c, n, _ := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		role := n + "_owner"
		if _, err := c.Exec(ctx, `create role `+q(role)+`; alter table `+q(n, "meta")+` owner to `+q(role)); err != nil {
			t.Skip(err)
		}
		err := s.Init(ctx)
		_, _ = c.Exec(ctx, `alter table `+q(n, "meta")+` owner to current_user; drop role `+q(role))
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("ownership accepted: %v", err)
		}
	})
	t.Run("attacker_precreated_namespace", func(t *testing.T) {
		s, c, n, _ := liveStore(t)
		ctx := context.Background()
		if _, err := c.Exec(ctx, `create schema `+q(n)+`; create table `+q(n, "meta")+`(singleton boolean primary key,schema_version integer not null); insert into `+q(n, "meta")+` values(true,2)`); err != nil {
			t.Fatal(err)
		}
		if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("precreated namespace accepted: %v", err)
		}
	})
	t.Run("check_or_true_bypass", func(t *testing.T) {
		s, c, n, _ := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `alter table `+q(n, "operations")+` drop constraint operations_progress_check,add constraint operations_progress_check check (((progress>=0) and (progress<=100)) or true)`); err != nil {
			t.Fatal(err)
		}
		if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("semantic bypass accepted: %v", err)
		}
	})
	for name, suffix := range map[string]string{"fk_delete_action": " on delete cascade", "fk_deferrable": " deferrable initially deferred"} {
		t.Run(name, func(t *testing.T) {
			s, c, n, _ := liveStore(t)
			ctx := context.Background()
			if err := s.Init(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(ctx, `alter table `+q(n, "object_mappings")+` drop constraint object_mappings_operation_id_fkey,add constraint object_mappings_operation_id_fkey foreign key(operation_id) references `+q(n, "operations")+`(operation_id)`+suffix); err != nil {
				t.Fatal(err)
			}
			if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("FK mutation accepted: %v", err)
			}
		})
	}
	t.Run("constraint_not_valid", func(t *testing.T) {
		s, c, n, _ := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `alter table `+q(n, "operations")+` drop constraint operations_progress_check,add constraint operations_progress_check check ((progress>=0) and (progress<=100)) not valid`); err != nil {
			t.Fatal(err)
		}
		if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("unvalidated constraint accepted: %v", err)
		}
	})
	t.Run("invalid_index", func(t *testing.T) {
		s, c, n, _ := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `set allow_system_table_mods=on`); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `update pg_index set indisvalid=false where indexrelid=$1::regclass`, n+".meta_pkey"); err != nil {
			t.Fatal(err)
		}
		if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("invalid index accepted: %v", err)
		}
	})
	t.Run("rule", func(t *testing.T) {
		s, c, n, _ := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `create rule attacker_rule as on insert to `+q(n, "meta")+` do also nothing`); err != nil {
			t.Fatal(err)
		}
		if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("rule accepted: %v", err)
		}
	})
	t.Run("trigger", func(t *testing.T) {
		s, c, n, app := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `create function `+q(app, "attacker_trigger")+`() returns trigger language plpgsql as $$ begin return new; end $$; create trigger attacker before insert on `+q(n, "meta")+` for each row execute function `+q(app, "attacker_trigger")+`() `); err != nil {
			t.Fatal(err)
		}
		if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("trigger accepted: %v", err)
		}
	})
	for name, alter := range map[string]string{"increment": "increment by 7", "min": "minvalue 0", "max": "maxvalue 1000", "start": "start with 2", "cache": "cache 7", "cycle": "cycle", "type": "as integer"} {
		t.Run("sequence_"+name, func(t *testing.T) {
			s, c, n, _ := liveStore(t)
			ctx := context.Background()
			if err := s.Init(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(ctx, `alter sequence `+q(n, "audit_sequence_seq")+` `+alter); err != nil {
				t.Fatal(err)
			}
			if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("sequence mutation accepted: %v", err)
			}
		})
	}
	t.Run("sequence_binding", func(t *testing.T) {
		s, c, n, _ := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `set allow_system_table_mods=on`); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `update pg_depend set deptype='a' where objid=$1::regclass and deptype='i'`, n+".audit_sequence_seq"); err != nil {
			t.Fatal(err)
		}
		if err := s.Init(ctx); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("sequence binding mutation accepted: %v", err)
		}
	})
}

func TestLiveLeastPrivilegeRoleCanInitializeAndBaseline(t *testing.T) {
	s, admin, n, app := liveStore(t)
	_ = s
	ctx := context.Background()
	role := n + "_role"
	var db, current string
	if err := admin.QueryRow(ctx, `select current_database(),current_user`).Scan(&db, &current); err != nil {
		t.Fatal(err)
	}
	_, err := admin.Exec(ctx, `create role `+q(role)+` nologin nosuperuser nocreatedb nocreaterole noinherit; grant `+q(role)+` to `+q(current)+`; grant create on database `+q(db)+` to `+q(role)+`; grant usage on schema `+q(app)+` to `+q(role)+`; grant select on all tables in schema `+q(app)+` to `+q(role))
	if err != nil {
		t.Skipf("test role setup requires CREATEROLE: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `revoke `+q(role)+` from `+q(current)+`; drop owned by `+q(role)+`; drop role `+q(role))
	})
	u, err := url.Parse(os.Getenv("AUTOSQL_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal(err)
	}
	values := u.Query()
	values.Set("options", "-c role="+role)
	u.RawQuery = values.Encode()
	limited, err := Open(Config{URL: u.String(), Schema: n})
	if err != nil {
		t.Fatal(err)
	}
	if err = limited.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = limited.Baseline(ctx, BaselineRequest{ID: "least", Target: "least-target", Environment: "test", Operator: "limited-role", Schemas: []string{app}}); err != nil {
		t.Fatal(err)
	}
	var super, createDB, createRole bool
	if err = admin.QueryRow(ctx, `select rolsuper,rolcreatedb,rolcreaterole from pg_roles where rolname=$1`, role).Scan(&super, &createDB, &createRole); err != nil {
		t.Fatal(err)
	}
	if super || createDB || createRole {
		t.Fatal("test role had elevated privileges")
	}
}

func TestLiveBaselineRetryDriftTamperIsolationAndNoMutation(t *testing.T) {
	s, c, n, app := liveStore(t)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	req := BaselineRequest{ID: "adopt-1", Target: "customer-primary", Environment: "production", Operator: "release-bot", Schemas: []string{app}}
	b, err := s.Baseline(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if b.Fingerprint == "" || len(b.CanonicalSchema) == 0 {
		t.Fatalf("incomplete evidence: %+v", b)
	}
	var balance int
	if err = c.QueryRow(ctx, `select balance from `+q(app, "accounts")+` where id=7`).Scan(&balance); err != nil || balance != 42 {
		t.Fatalf("user data changed: %d %v", balance, err)
	}
	again, err := s.Baseline(ctx, req)
	if err != nil || again.Fingerprint != b.Fingerprint {
		t.Fatalf("retry=%+v err=%v", again, err)
	}
	_, err = s.Baseline(ctx, BaselineRequest{ID: "other", Target: req.Target, Environment: req.Environment, Operator: req.Operator, Schemas: req.Schemas})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("target binding accepted: %v", err)
	}
	if _, err = c.Exec(ctx, `alter table `+q(app, "accounts")+` add column note text`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Baseline(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("drift accepted: %v", err)
	}
	if _, err = c.Exec(ctx, `update `+q(n, "baselines")+` set canonical_schema='{}'`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Baseline(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("tamper accepted: %v", err)
	}
}

func TestLiveBaselineRetryBindsOperatorDocumentAndAudit(t *testing.T) {
	t.Run("operator", func(t *testing.T) {
		s, _, _, app := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		r := BaselineRequest{ID: "b", Target: "t", Environment: "prod", Operator: "operator_a", Schemas: []string{app}}
		if _, err := s.Baseline(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Operator = "operator_b"
		if _, err := s.Baseline(ctx, r); !errors.Is(err, ErrConflict) {
			t.Fatalf("operator mismatch accepted: %v", err)
		}
	})
	t.Run("document", func(t *testing.T) {
		s, c, n, app := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		r := BaselineRequest{ID: "b", Target: "t", Environment: "prod", Operator: "o", Schemas: []string{app}}
		if _, err := s.Baseline(ctx, r); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `update `+q(n, "baselines")+` set canonical_schema=canonical_schema||' '`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Baseline(ctx, r); !errors.Is(err, ErrConflict) {
			t.Fatalf("document tamper accepted: %v", err)
		}
	})
	t.Run("audit", func(t *testing.T) {
		s, c, n, app := liveStore(t)
		ctx := context.Background()
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		r := BaselineRequest{ID: "b", Target: "t", Environment: "prod", Operator: "o", Schemas: []string{app}}
		if _, err := s.Baseline(ctx, r); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `update `+q(n, "audit")+` set operator_identity='attacker'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Baseline(ctx, r); !errors.Is(err, ErrConflict) {
			t.Fatalf("audit tamper accepted: %v", err)
		}
	})
}

func TestLiveBaselineLocksConcurrentDDLThroughCommit(t *testing.T) {
	s, _, _, app := liveStore(t)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	ddlDone := make(chan error, 1)
	started := make(chan struct{})
	hook := BaselineHooks{BeforeFinalInspection: func() error {
		go func() {
			close(started)
			c, err := pgx.Connect(ctx, os.Getenv("AUTOSQL_TEST_POSTGRES_URL"))
			if err == nil {
				_, err = c.Exec(ctx, `alter table `+q(app, "accounts")+` add column concurrent_change text`)
				c.Close(ctx)
			}
			ddlDone <- err
		}()
		<-started
		time.Sleep(150 * time.Millisecond)
		select {
		case err := <-ddlDone:
			return fmt.Errorf("DDL crossed baseline lock early: %v", err)
		default:
			return nil
		}
	}}
	r := BaselineRequest{ID: "locked", Target: "t", Environment: "prod", Operator: "o", Schemas: []string{app}}
	if _, err := s.BaselineWithHooks(ctx, r, hook); err != nil {
		t.Fatal(err)
	}
	if err := <-ddlDone; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Baseline(ctx, r); !errors.Is(err, ErrConflict) {
		t.Fatalf("post-commit DDL drift accepted: %v", err)
	}
}

func TestLiveBaselineRefusesActiveAndMismatchAtomically(t *testing.T) {
	s, c, n, app := liveStore(t)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	req := BaselineRequest{ID: "b", Target: "t", Environment: "e", Operator: "o", Schemas: []string{app}, ExpectedFingerprint: "wrong"}
	_, err := s.Baseline(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatch accepted: %v", err)
	}
	var count int
	_ = c.QueryRow(ctx, `select count(*) from `+q(n, "baselines")).Scan(&count)
	if count != 0 {
		t.Fatal("failed baseline was not atomic")
	}
	_, _ = c.Exec(ctx, `update `+q(n, "meta")+` set active_version='v2'`)
	req.ExpectedFingerprint = ""
	_, err = s.Baseline(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("active state accepted: %v", err)
	}
}
