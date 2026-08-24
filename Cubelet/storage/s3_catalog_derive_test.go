// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func useS3CatalogRoot(t *testing.T, root string) {
	t.Helper()
	SetSnapshotCatalogRootsFor(cow.BackendS3, root)
	t.Cleanup(func() {
		SetSnapshotCatalogRootsFor(cow.BackendS3)
	})
}

func TestGetLocalSnapshotForDerivesSealedS3Template(t *testing.T) {
	root := filepath.Join(t.TempDir(), SnapshotKindNormal)
	id := "tpl-abc"
	require.NoError(t, os.MkdirAll(filepath.Join(root, id, SnapshotMetadataDir), 0o755))
	useS3CatalogRoot(t, root)
	useTestCowStorage(t, &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-tpl-abc-rootfs":      {SizeBytes: 1 << 30},
			"tpl-tpl-abc-memory-snap": {SizeBytes: 64 << 20},
			"s3-meta-tpl-abc-snap":    {SizeBytes: 8 << 20},
		},
	})

	entry, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, id)
	require.NoError(t, err)
	require.Equal(t, id, entry.SnapshotID)
	require.Equal(t, filepath.Join(root, id), entry.SnapshotPath)
	require.Equal(t, filepath.Join(root, id, SnapshotMetadataDir), entry.MetaDir)
	require.Equal(t, "tpl-tpl-abc-rootfs", entry.RootfsVol)
	require.Equal(t, cowKindSnapshot, entry.RootfsKind)
	require.Equal(t, uint64(1<<30), entry.RootfsSizeBytes)
	require.Equal(t, "tpl-tpl-abc-memory-snap", entry.MemoryVol)
	require.Equal(t, cowKindSnapshot, entry.MemoryKind)
	require.Equal(t, "s3-meta-tpl-abc-snap", entry.MetadataVol)
	require.Equal(t, cowKindSnapshot, entry.MetadataKind)
	require.Equal(t, CatalogKindTemplate, entry.Kind)
	require.Equal(t, cow.BackendS3, entry.Backend)
}

func TestGetLocalSnapshotForDerivesPauseKindFromPackageDir(t *testing.T) {
	work := t.TempDir()
	root := filepath.Join(work, SnapshotKindPause)
	id := "snap-paused-1"
	require.NoError(t, os.MkdirAll(filepath.Join(root, id, SnapshotMetadataDir), 0o755))
	useS3CatalogRoot(t, root)
	useTestCowStorage(t, &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-paused-1-rootfs": {SizeBytes: 1 << 30},
			"tpl-snap-paused-1-memory": {SizeBytes: 64 << 20},
		},
	})

	entry, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, id)
	require.NoError(t, err)
	require.Equal(t, CatalogKindPauseSnapshot, entry.Kind)
	require.Equal(t, "tpl-snap-paused-1-memory", entry.MemoryVol)
	require.Equal(t, cowKindVolume, entry.MemoryKind)
	require.Empty(t, entry.MetadataVol)
}

func TestGetLocalSnapshotForKeepsMissWithoutRootfsObject(t *testing.T) {
	useS3CatalogRoot(t, t.TempDir())
	useTestCowStorage(t, &fakeCowEngine{})

	_, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, "tpl-gone")
	require.ErrorIs(t, err, ErrSnapshotCatalogNotFound)
}

func TestGetLocalSnapshotForPrefersCatalogFileOverDerived(t *testing.T) {
	root := filepath.Join(t.TempDir(), SnapshotKindNormal)
	id := "tpl-onduty"
	home := filepath.Join(root, id)
	useS3CatalogRoot(t, root)
	useTestCowStorage(t, &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-tpl-onduty-rootfs": {SizeBytes: 1 << 30},
		},
	})
	require.NoError(t, WriteSnapshotCatalogFor(cow.BackendS3, &SnapshotCatalogEntry{
		SnapshotID:   id,
		SnapshotPath: home,
		RootfsVol:    "custom-rootfs",
		RootfsKind:   cowKindSnapshot,
		SpecDir:      "cube-runtime-1.2.3",
	}))
	DeleteSnapshotCatalogFor(cow.BackendS3, id)

	entry, err := GetLocalSnapshotFor(context.Background(), cow.BackendS3, id)
	require.NoError(t, err)
	require.Equal(t, "custom-rootfs", entry.RootfsVol)
	require.Equal(t, "cube-runtime-1.2.3", entry.SpecDir)
}
