// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// Not parallel: SetSnapshotCatalogRootsFor is process-global, so two of
// these running at once read each other's roots (or a Cleanup that already
// cleared them).
func TestSnapshotCatalogNamespacesAreIsolated(t *testing.T) {
	xfsRoot := t.TempDir()
	s3Root := t.TempDir()
	SetSnapshotCatalogRootsFor(cow.BackendXFS, xfsRoot)
	SetSnapshotCatalogRootsFor(cow.BackendS3, s3Root)
	t.Cleanup(func() {
		SetSnapshotCatalogRootsFor(cow.BackendXFS)
		SetSnapshotCatalogRootsFor(cow.BackendS3)
	})

	xfsEntry := &SnapshotCatalogEntry{
		SnapshotID:   "snap-xfs",
		SnapshotPath: filepath.Join(xfsRoot, "snap-xfs"),
		RootfsVol:    "xfs-rootfs",
		MemoryVol:    "xfs-mem",
	}
	s3Entry := &SnapshotCatalogEntry{
		SnapshotID:   "snap-s3",
		SnapshotPath: filepath.Join(s3Root, "snap-s3"),
		RootfsVol:    "s3-rootfs",
		MemoryVol:    "s3-mem",
	}
	if err := WriteSnapshotCatalogFor(cow.BackendXFS, xfsEntry); err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshotCatalogFor(cow.BackendS3, s3Entry); err != nil {
		t.Fatal(err)
	}

	if _, err := GetLocalSnapshotFor(context.Background(), cow.BackendXFS, "snap-s3"); err == nil {
		t.Fatal("xfs namespace must not see s3 catalog")
	}
	if _, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, "snap-xfs"); err == nil {
		t.Fatal("s3 namespace must not see xfs catalog")
	}
	got, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, "snap-s3")
	if err != nil {
		t.Fatal(err)
	}
	if got.RootfsVol != "s3-rootfs" {
		t.Fatalf("s3 catalog rootfs=%q", got.RootfsVol)
	}
	if got.Backend != cow.BackendS3 {
		t.Fatalf("s3 catalog backend=%q, want s3", got.Backend)
	}
}

func TestS3CatalogLivesUnderMetadata(t *testing.T) {
	s3Root := t.TempDir()
	SetSnapshotCatalogRootsFor(cow.BackendS3, s3Root)
	t.Cleanup(func() {
		SetSnapshotCatalogRootsFor(cow.BackendS3)
	})

	home := filepath.Join(s3Root, "snap-s3")
	if err := WriteSnapshotCatalogFor(cow.BackendS3, &SnapshotCatalogEntry{
		SnapshotID:   "snap-s3",
		SnapshotPath: home,
		RootfsVol:    "s3-rootfs",
		MemoryVol:    "s3-mem",
	}); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(home, S3SnapshotMetadataDir, snapshotCatalogFileName)
	if _, err := os.Stat(catalogPath); err != nil {
		t.Fatalf("expected catalog at %s: %v", catalogPath, err)
	}
	if _, err := os.Stat(filepath.Join(home, snapshotCatalogFileName)); err == nil {
		t.Fatal("s3 catalog must not sit at snapshot home root")
	}

	SetSnapshotCatalogRootsFor(cow.BackendS3, s3Root)
	got, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, "snap-s3")
	if err != nil {
		t.Fatal(err)
	}
	if got.SnapshotPath != home {
		t.Fatalf("snapshot_path=%q want %q", got.SnapshotPath, home)
	}
	if got.MetaDir != filepath.Join(home, S3SnapshotMetadataDir) {
		t.Fatalf("meta_dir=%q", got.MetaDir)
	}
}

func TestXFSCatalogLivesUnderMetadata(t *testing.T) {
	xfsRoot := t.TempDir()
	SetSnapshotCatalogRootsFor(cow.BackendXFS, xfsRoot)
	t.Cleanup(func() {
		SetSnapshotCatalogRootsFor(cow.BackendXFS)
	})

	home := filepath.Join(xfsRoot, "snap-xfs")
	if err := WriteSnapshotCatalogFor(cow.BackendXFS, &SnapshotCatalogEntry{
		SnapshotID:   "snap-xfs",
		SnapshotPath: home,
		RootfsVol:    "xfs-rootfs",
		MemoryVol:    "xfs-mem",
	}); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(home, SnapshotMetadataDir, snapshotCatalogFileName)
	if _, err := os.Stat(catalogPath); err != nil {
		t.Fatalf("expected catalog at %s: %v", catalogPath, err)
	}
	if _, err := os.Stat(filepath.Join(home, snapshotCatalogFileName)); err == nil {
		t.Fatal("xfs catalog must not sit at snapshot home root")
	}
}
