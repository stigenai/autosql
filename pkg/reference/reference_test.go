package reference

import (
	"context"
	"errors"
	"testing"
)

func decl(mode Mode) Declaration {
	return Declaration{Table: Table{Schema: "app", Name: "statuses", Key: []string{"code"}, Columns: []string{"code", "label", "updated_at"}, SkipDiff: []string{"updated_at"}, Mode: mode, MaxRows: 10, MaxBytes: 4096, Managed: true}, Rows: []Row{{"code": "active", "label": "Active", "updated_at": "one"}, {"code": "paused", "label": "Paused", "updated_at": "two"}}}
}
func TestBoundedDeclarationAndModes(t *testing.T) {
	d := decl(Insert)
	p, err := PlanChanges(d, []Row{{"code": "active", "label": "Old", "updated_at": "other"}, {"code": "retired", "label": "Retired"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 1 || p.Actions[0].Kind != InsertRow {
		t.Fatalf("insert plan=%+v", p)
	}
	d.Table.Mode = Upsert
	p, err = PlanChanges(d, []Row{{"code": "active", "label": "Old", "updated_at": "other"}})
	if err != nil || len(p.Actions) != 2 {
		t.Fatalf("upsert plan=%+v err=%v", p, err)
	}
	d.Table.Mode = Sync
	p, err = PlanChanges(d, []Row{{"code": "retired", "label": "Retired", "updated_at": "x"}})
	if err != nil || p.DeleteCount != 1 || !p.RequiresApproval {
		t.Fatalf("sync plan=%+v err=%v", p, err)
	}
	if _, err = PlanChanges(decl(Insert), []Row{{"code": nil, "label": "x"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad key error=%v", err)
	}
}

type fakeStore struct{ committed bool }
type fakeTx struct{ *fakeStore }

func (f *fakeStore) Begin(context.Context) (Tx, error)     { return &fakeTx{f}, nil }
func (f *fakeTx) Insert(context.Context, Table, Row) error { return nil }
func (f *fakeTx) Update(context.Context, Table, Row) error { return nil }
func (f *fakeTx) Delete(context.Context, Table, Row) error { return nil }
func (f *fakeTx) Commit(context.Context) error             { f.committed = true; return nil }
func (f *fakeTx) Rollback(context.Context) error           { return nil }
func TestSyncRequiresDestructiveApproval(t *testing.T) {
	d := decl(Sync)
	p, _ := PlanChanges(d, []Row{{"code": "retired", "label": "Retired"}})
	s := &fakeStore{}
	if err := Apply(context.Background(), s, p, false); !errors.Is(err, ErrDestructive) {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), s, p, true); err != nil || !s.committed {
		t.Fatalf("apply=%v committed=%v", err, s.committed)
	}
}
