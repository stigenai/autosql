package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestVerifyCurrentRefusesChangesWithoutRefreshingSnapshot(t *testing.T) {
	_, files := projectInputFixture(t, context.Background())
	writeProjectInput(t, files.RootPath(), "input.sql", "original")
	if _, err := files.ReadFile("input.sql"); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			if err := files.VerifyCurrent(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	writeProjectInput(t, files.RootPath(), "input.sql", "changed")
	if err := files.VerifyCurrent(context.Background()); err == nil {
		t.Fatal("changed input accepted")
	}
	raw, _ := files.ReadFile("input.sql")
	if string(raw) != "original" {
		t.Fatal("verification refreshed the snapshot")
	}
	writeProjectInput(t, files.RootPath(), "input.sql", "original")
	if err := files.VerifyCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(files.VerifyCurrent(ctx), context.Canceled) {
		t.Fatal("cancellation lost")
	}
	if err := os.Remove(filepath.Join(files.RootPath(), "input.sql")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(files.RootPath(), "input.sql")); err != nil {
		t.Fatal(err)
	}
	if err := files.VerifyCurrent(context.Background()); err == nil {
		t.Fatal("redirected input accepted")
	}
}

func TestVerifyCurrentRefusesReplacedRoot(t *testing.T) {
	_, files := projectInputFixture(t, context.Background())
	root := files.RootPath()
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root + "-moved") })
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := files.VerifyCurrent(context.Background()); err == nil {
		t.Fatal("replaced root accepted")
	}
}

func TestCaptureConfigurationClosesParseToCaptureGap(t *testing.T) {
	c, files := projectInputFixture(t, context.Background())
	if err := c.CaptureConfiguration(files); err != nil {
		t.Fatal(err)
	}
	writeProjectInput(t, files.RootPath(), filepath.Base(c.path), projectJSON()+"\n")
	if err := files.VerifyCurrent(context.Background()); err == nil {
		t.Fatal("configuration mutation accepted")
	}
	fresh, err := c.OpenInputFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := c.CaptureConfiguration(fresh); err == nil {
		t.Fatal("capture bound bytes different from parsed configuration")
	}
}
