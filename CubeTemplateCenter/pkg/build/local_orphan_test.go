// SPDX-License-Identifier: Apache-2.0

package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupLocalOrphansGuards(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for _, id := range []string{"rfs-old", "rfs-live", "rfs-recent", "rfs-building", "layer-cache"} {
		dir := filepath.Join(root, id)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data.ext4"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		if id == "rfs-building" {
			if err := os.WriteFile(filepath.Join(dir, ".cube-artifact-build-in-progress"), []byte("active"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if id != "rfs-recent" {
			if err := os.Chtimes(dir, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "rfs-symlink")); err != nil {
		t.Fatal(err)
	}
	err := cleanupLocalOrphans(t.Context(), []string{root, root}, now, func(_ context.Context, id string) (bool, error) { return id == "rfs-live", nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"rfs-live", "rfs-recent", "rfs-building", "layer-cache", "rfs-symlink"} {
		if _, err := os.Lstat(filepath.Join(root, id)); err != nil {
			t.Fatalf("preserve %s: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "rfs-old")); !os.IsNotExist(err) {
		t.Fatalf("orphan remained: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupLocalOrphansFailsClosed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rfs-orphan")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(dir, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	want := errors.New("database unavailable")
	if err := cleanupLocalOrphans(t.Context(), []string{root}, now, func(context.Context, string) (bool, error) { return false, want }); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("query failure removed directory: %v", err)
	}
	if err := NewArtifactDeleter(nil, nil).CleanupLocalOrphans(t.Context()); err == nil {
		t.Fatal("missing database accepted")
	}
}

func TestCleanupLocalOrphansRechecksBuildMarker(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rfs-new-owner")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(dir, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := cleanupLocalOrphans(t.Context(), []string{root}, now, func(context.Context, string) (bool, error) {
		if err := os.WriteFile(filepath.Join(dir, ".cube-artifact-build-in-progress"), []byte("active"), 0o644); err != nil {
			return false, err
		}
		return false, os.Chtimes(dir, now.Add(-time.Hour), now.Add(-time.Hour))
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("active build removed: %v", err)
	}
}
