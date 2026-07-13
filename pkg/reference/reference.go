// Package reference reconciles bounded, application-owned reference rows.
package reference

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

var (
	ErrInvalid     = errors.New("invalid reference data declaration")
	ErrBounds      = errors.New("reference data exceeds policy bounds")
	ErrDestructive = errors.New("reference data deletion requires destructive approval")
)

type Mode string

const (
	Insert Mode = "insert"
	Upsert Mode = "upsert"
	Sync   Mode = "sync"
)

type Table struct {
	Schema   string   `json:"schema"`
	Name     string   `json:"name"`
	Key      []string `json:"key"`
	Columns  []string `json:"columns"`
	SkipDiff []string `json:"skip_diff,omitempty"`
	Mode     Mode     `json:"mode"`
	MaxRows  int      `json:"max_rows"`
	MaxBytes int      `json:"max_bytes"`
	Managed  bool     `json:"managed"`
}

type Row map[string]any
type Declaration struct {
	Table  Table  `json:"table"`
	Rows   []Row  `json:"rows"`
	Source string `json:"source,omitempty"`
}

func (t Table) Validate() error {
	if t.Schema == "" || t.Name == "" || !t.Managed {
		return fmt.Errorf("%w: schema, name, and managed=true are required", ErrInvalid)
	}
	if len(t.Key) == 0 {
		return fmt.Errorf("%w: stable primary key columns are required", ErrInvalid)
	}
	if len(t.Columns) == 0 {
		return fmt.Errorf("%w: explicit managed columns are required", ErrInvalid)
	}
	cols := map[string]bool{}
	for _, c := range t.Columns {
		if c == "" || cols[c] {
			return fmt.Errorf("%w: duplicate or empty column", ErrInvalid)
		}
		cols[c] = true
	}
	for _, k := range t.Key {
		if !cols[k] {
			return fmt.Errorf("%w: key column %q is not managed", ErrInvalid, k)
		}
	}
	for _, c := range t.SkipDiff {
		if !cols[c] {
			return fmt.Errorf("%w: skip_diff column %q is not managed", ErrInvalid, c)
		}
	}
	if t.Mode != Insert && t.Mode != Upsert && t.Mode != Sync {
		return fmt.Errorf("%w: mode must be insert, upsert, or sync", ErrInvalid)
	}
	if t.MaxRows <= 0 || t.MaxBytes <= 0 {
		return fmt.Errorf("%w: positive max_rows and max_bytes are required", ErrInvalid)
	}
	return nil
}
func (d Declaration) Validate() error {
	if err := d.Table.Validate(); err != nil {
		return err
	}
	if len(d.Rows) > d.Table.MaxRows {
		return fmt.Errorf("%w: %d rows exceeds %d", ErrBounds, len(d.Rows), d.Table.MaxRows)
	}
	seen := map[string]bool{}
	for i, r := range d.Rows {
		for _, c := range d.Table.Columns {
			if _, ok := r[c]; !ok {
				return fmt.Errorf("%w: row %d missing managed column %q", ErrInvalid, i, c)
			}
		}
		k, err := key(r, d.Table.Key)
		if err != nil {
			return err
		}
		if seen[k] {
			return fmt.Errorf("%w: duplicate primary key %s", ErrInvalid, k)
		}
		seen[k] = true
	}
	b, _ := json.Marshal(d.Rows)
	if len(b) > d.Table.MaxBytes {
		return fmt.Errorf("%w: serialized rows exceed %d bytes", ErrBounds, d.Table.MaxBytes)
	}
	return nil
}
func key(r Row, cols []string) (string, error) {
	var out []any
	for _, c := range cols {
		v, ok := r[c]
		if !ok || v == nil {
			return "", fmt.Errorf("%w: primary key %q is null or missing", ErrInvalid, c)
		}
		out = append(out, v)
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}
func (d Declaration) Digest() (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	b, _ := json.Marshal(d)
	h := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", h[:]), nil
}

type ActionKind string

const (
	InsertRow ActionKind = "insert"
	UpdateRow ActionKind = "update"
	DeleteRow ActionKind = "delete"
)

type Action struct {
	Kind   ActionKind `json:"kind"`
	Key    string     `json:"key"`
	Before Row        `json:"before,omitempty"`
	After  Row        `json:"after,omitempty"`
}
type Plan struct {
	Table            Table    `json:"table"`
	Actions          []Action `json:"actions"`
	DeleteCount      int      `json:"delete_count"`
	RequiresApproval bool     `json:"requires_approval"`
}

func PlanChanges(d Declaration, observed []Row) (Plan, error) {
	if err := d.Validate(); err != nil {
		return Plan{}, err
	}
	old := map[string]Row{}
	for _, r := range observed {
		k, e := key(r, d.Table.Key)
		if e != nil {
			return Plan{}, e
		}
		old[k] = r
	}
	want := map[string]Row{}
	for _, r := range d.Rows {
		k, _ := key(r, d.Table.Key)
		want[k] = r
	}
	p := Plan{Table: d.Table}
	for k, r := range want {
		before, ok := old[k]
		if !ok {
			if d.Table.Mode != Sync && d.Table.Mode != Upsert && d.Table.Mode != Insert {
				continue
			}
			p.Actions = append(p.Actions, Action{Kind: InsertRow, Key: k, After: r})
		} else if d.Table.Mode == Upsert && !equalManaged(before, r, d.Table) {
			p.Actions = append(p.Actions, Action{Kind: UpdateRow, Key: k, Before: before, After: r})
		}
	}
	if d.Table.Mode == Sync {
		for k, r := range old {
			if _, ok := want[k]; !ok {
				p.Actions = append(p.Actions, Action{Kind: DeleteRow, Key: k, Before: r})
				p.DeleteCount++
			}
		}
		p.RequiresApproval = p.DeleteCount > 0
	}
	sort.Slice(p.Actions, func(i, j int) bool { return p.Actions[i].Key < p.Actions[j].Key })
	return p, nil
}
func equalManaged(a, b Row, t Table) bool {
	for _, c := range t.Columns {
		skip := false
		for _, x := range t.SkipDiff {
			if x == c {
				skip = true
			}
		}
		if !skip && fmt.Sprint(a[c]) != fmt.Sprint(b[c]) {
			return false
		}
	}
	return true
}

type Store interface {
	Begin(context.Context) (Tx, error)
}
type Tx interface {
	Insert(context.Context, Table, Row) error
	Update(context.Context, Table, Row) error
	Delete(context.Context, Table, Row) error
	Commit(context.Context) error
	Rollback(context.Context) error
}

func Apply(ctx context.Context, s Store, p Plan, destructiveApproved bool) error {
	if p.RequiresApproval && !destructiveApproved {
		return ErrDestructive
	}
	tx, err := s.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, a := range p.Actions {
		var e error
		switch a.Kind {
		case InsertRow:
			e = tx.Insert(ctx, p.Table, a.After)
		case UpdateRow:
			e = tx.Update(ctx, p.Table, a.After)
		case DeleteRow:
			e = tx.Delete(ctx, p.Table, a.Before)
		}
		if e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
