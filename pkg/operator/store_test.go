package operator

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStoreRecordsAreIsolated(t *testing.T) {
	for _, kind := range []string{"memory", "file"} {
		t.Run(kind, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if kind == "file" {
				var err error
				store, err = NewFileStore(filepath.Join(t.TempDir(), "state.json"))
				if err != nil {
					t.Fatal(err)
				}
			}
			want := Record{AppliedKey: "committed", Status: Status{
				Conditions: []Condition{{Type: Ready, Status: "True", Reason: "Applied"}},
			}}
			input := want
			input.Status.Conditions = append([]Condition(nil), want.Status.Conditions...)
			if err := store.Save("orders", input); err != nil {
				t.Fatal(err)
			}
			input.Status.Conditions[0].Reason = "changed after Save"
			got, ok := store.Load("orders")
			if !ok || !reflect.DeepEqual(got, want) {
				t.Errorf("Save retained caller-owned conditions: %#v", got)
			}
			got.Status.Conditions[0].Reason = "changed after Load"
			again, ok := store.Load("orders")
			if !ok || !reflect.DeepEqual(again, want) {
				t.Errorf("Load exposed committed conditions: %#v", again)
			}
		})
	}
}

func TestFileStoreFailedSavePreservesRecords(t *testing.T) {
	for _, failure := range []string{"encode", "create_directory", "rename"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			store, err := NewFileStore(path)
			if err != nil {
				t.Fatal(err)
			}
			want := Record{AppliedKey: "committed", Status: Status{AppliedDigest: "previous"}}
			if err := store.Save("orders", want); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			candidate := Record{AppliedKey: "not committed", Status: Status{AppliedDigest: "next"}}
			switch failure {
			case "encode":
				candidate.Status.Conditions = []Condition{{LastTransitionTime: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}
			case "create_directory":
				// A regular file cannot be used as the state directory.
				store.path = filepath.Join(path, "child.json")
			case "rename":
				// Writing the temporary file succeeds, but replacing a directory fails.
				store.path = filepath.Join(filepath.Dir(path), "directory")
				if err := os.Mkdir(store.path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range []string{"orders", "new-resource"} {
				if err := store.Save(key, candidate); err == nil {
					t.Fatalf("expected %s failure for %s", failure, key)
				}
			}
			got, ok := store.Load("orders")
			if !ok || !reflect.DeepEqual(got, want) {
				t.Errorf("failed save changed committed record: %#v", got)
			}
			if _, ok := store.Load("new-resource"); ok {
				t.Error("failed save inserted an uncommitted record")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed save changed durable state: %v", err)
			}
			if leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".operator-state-*")); err != nil || len(leftovers) != 0 {
				t.Errorf("temporary state files leaked: %v, %v", leftovers, err)
			}
			// Restoring the destination must not persist any earlier failed insert.
			store.path = path
			if err := store.Save("orders", want); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewFileStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := reopened.Load("orders"); !ok || !reflect.DeepEqual(got, want) {
				t.Errorf("reopened checkpoint changed: %#v", got)
			}
			if _, ok := reopened.Load("new-resource"); ok {
				t.Error("later save persisted the earlier failed insert")
			}
		})
	}
}
