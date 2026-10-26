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
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func writeLegacyVolume(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, reflinkVolumesDirName, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("vol"), 0o644))
	return dir
}

func writeLegacySnapshot(t *testing.T, root, origin, name string) string {
	t.Helper()
	dir := filepath.Join(root, reflinkVolumesDirName, origin)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, origin), []byte("origin"), 0o644))
	snap := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(snap, []byte("snap"), 0o644))
	return snap
}

func TestCleanupObjectsRemovesLegacyXfsVolumeWhenMissingFromNewPath(t *testing.T) {
	work := t.TempDir()
	legacy := filepath.Join(work, legacyReflinkDirName)
	current := filepath.Join(work, cow.BackendXFS, SnapshotObjectsDir)
	require.NoError(t, os.MkdirAll(filepath.Join(current, reflinkVolumesDirName), 0o755))
	volDir := writeLegacyVolume(t, legacy, "tpl-old-rootfs")

	engine := &fakeCowEngine{}
	useTestCowStorage(t, engine)
	localStorage.config.DataPath = work

	err := CleanupObjectsFor(context.Background(), cow.BackendXFS, []CowObjectRef{
		{Name: "tpl-old-rootfs", Kind: CowKindSnapshot, Role: "rootfs"},
	})
	require.NoError(t, err)
	_, statErr := os.Stat(volDir)
	require.Error(t, statErr)
	require.True(t, os.IsNotExist(statErr))
}

func TestCleanupObjectsRemovesLegacyXfsSnapshotFileWhenMissingFromNewPath(t *testing.T) {
	work := t.TempDir()
	legacy := filepath.Join(work, legacyReflinkDirName)
	current := filepath.Join(work, cow.BackendXFS, SnapshotObjectsDir)
	require.NoError(t, os.MkdirAll(filepath.Join(current, reflinkVolumesDirName), 0o755))
	originDir := filepath.Join(legacy, reflinkVolumesDirName, "tpl-old-memory")
	snap := writeLegacySnapshot(t, legacy, "tpl-old-memory", "tpl-old-memory-snap")

	engine := &fakeCowEngine{}
	useTestCowStorage(t, engine)
	localStorage.config.DataPath = work

	err := CleanupObjectsFor(context.Background(), cow.BackendXFS, []CowObjectRef{
		{Name: "tpl-old-memory-snap", Kind: CowKindSnapshot, Role: "memory"},
	})
	require.NoError(t, err)
	_, statErr := os.Stat(snap)
	require.True(t, os.IsNotExist(statErr))
	_, originErr := os.Stat(filepath.Join(originDir, "tpl-old-memory"))
	require.NoError(t, originErr, "origin volume must stay when only the snap is deleted")
}

func TestCleanupObjectsLeavesLegacyWhenPresentOnNewPath(t *testing.T) {
	work := t.TempDir()
	legacy := filepath.Join(work, legacyReflinkDirName)
	current := filepath.Join(work, cow.BackendXFS, SnapshotObjectsDir)
	legacyDir := writeLegacyVolume(t, legacy, "tpl-both-rootfs")
	currentDir := writeLegacyVolume(t, current, "tpl-both-rootfs")

	engine := &fakeCowEngine{}
	useTestCowStorage(t, engine)
	localStorage.config.DataPath = work

	err := CleanupObjectsFor(context.Background(), cow.BackendXFS, []CowObjectRef{
		{Name: "tpl-both-rootfs", Kind: CowKindSnapshot, Role: "rootfs"},
	})
	require.NoError(t, err)
	_, err = os.Stat(legacyDir)
	require.NoError(t, err, "legacy copy must stay while the new-path object still exists")
	_, err = os.Stat(currentDir)
	require.NoError(t, err)
}

func TestCleanupObjectsS3DoesNotTouchLegacyXfsPool(t *testing.T) {
	work := t.TempDir()
	legacy := filepath.Join(work, legacyReflinkDirName)
	volDir := writeLegacyVolume(t, legacy, "tpl-s3-rootfs")

	engine := &fakeCowEngine{}
	useTestCowStorage(t, engine)
	localStorage.config.DataPath = work

	err := CleanupObjectsFor(context.Background(), cow.BackendS3, []CowObjectRef{
		{Name: "tpl-s3-rootfs", Kind: CowKindSnapshot, Role: "rootfs"},
	})
	require.NoError(t, err)
	_, err = os.Stat(volDir)
	require.NoError(t, err)
}

func TestRemoveLegacyXfsCowObjectRejectsTraversalName(t *testing.T) {
	work := t.TempDir()
	engine := &fakeCowEngine{}
	useTestCowStorage(t, engine)
	localStorage.config.DataPath = work
	require.NoError(t, removeLegacyXfsCowObjectIfGone(context.Background(), "../etc"))
}
