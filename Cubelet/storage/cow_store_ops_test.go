// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow/xfscow"
)

func TestXfsCowName(t *testing.T) {
	t.Parallel()
	require.Equal(t, xfscow.Name, (&XfsCow{}).Name())
	require.Equal(t, cow.NameXfsCow, xfscow.Name)
	require.Equal(t, cow.NameS3, "s3")
}

func TestRequireCowStoreNotInitialized(t *testing.T) {
	prev := localStorage
	localStorage = nil
	t.Cleanup(func() { localStorage = prev })

	_, err := requireCowStore()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not initialized")
}

func TestRequireCowStoreWrongBackend(t *testing.T) {
	prev := localStorage
	localStorage = &local{config: &Config{StorageBackend: "other"}}
	t.Cleanup(func() { localStorage = prev })

	_, err := requireCowStore()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not cubecow")
}

func TestCommitRootfsUsesActiveStore(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotPath: "/dev/mapper/tpl-snap-pause1-rootfs",
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-pause1-rootfs": {DevicePath: "/dev/mapper/tpl-snap-pause1-rootfs", SizeBytes: 1 << 20},
		},
	}
	useTestCowStorage(t, engine)

	source := &CowSnapshotObject{Name: "sb-1-rootfs-gen0", Kind: cow.KindSnapshot}
	obj, err := CommitRootfs(context.Background(), source, "snap-pause1")
	require.NoError(t, err)
	require.Equal(t, "tpl-snap-pause1-rootfs", obj.Name)
	require.Equal(t, cow.KindSnapshot, obj.Kind)
	require.Equal(t, [][2]string{{"sb-1-rootfs-gen0", "tpl-snap-pause1-rootfs"}}, engine.createSnapshots)
	require.Equal(t, cow.NameXfsCow, ActiveCowStore().Name())
}

func TestCommitRootfsRejectsMissingSource(t *testing.T) {
	useTestCowStorage(t, &fakeCowEngine{})
	_, err := CommitRootfs(context.Background(), nil, "snap-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "source rootfs is required")
}

func TestCommitRootfsFromBuildUsesActiveStore(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotPath: "/dev/mapper/tpl-t1-rootfs",
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-t1-rootfs": {DevicePath: "/dev/mapper/tpl-t1-rootfs", SizeBytes: 4096},
		},
	}
	useTestCowStorage(t, engine)

	obj, err := CommitRootfsFromBuild(context.Background(), "t1")
	require.NoError(t, err)
	require.Equal(t, "tpl-t1-rootfs", obj.Name)
	require.Equal(t, [][2]string{{"tpl-t1-build-rootfs", "tpl-t1-rootfs"}}, engine.createSnapshots)
}

func TestCreateMemoryVolumeUsesActiveStore(t *testing.T) {
	engine := &fakeCowEngine{createVolumePath: "/dev/mapper/tpl-snap-pause1-memory"}
	useTestCowStorage(t, engine)

	obj, err := CreateMemoryVolume(context.Background(), "snap-pause1", 1<<20)
	require.NoError(t, err)
	require.Equal(t, "tpl-snap-pause1-memory", obj.Name)
	require.Equal(t, cow.KindVolume, obj.Kind)
	require.Equal(t, []string{"tpl-snap-pause1-memory"}, engine.createVolumes)
}

func TestDeleteObjectAndDeactivateObject(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-x-memory": {DevicePath: "/dev/mapper/tpl-x-memory", SizeBytes: 1024},
			"tpl-x-rootfs": {DevicePath: "/dev/mapper/tpl-x-rootfs", SizeBytes: 2048},
		},
	}
	useTestCowStorage(t, engine)

	require.NoError(t, DeactivateObject(context.Background(), "tpl-x-memory", cow.KindVolume))
	require.Equal(t, []string{"tpl-x-memory"}, engine.deactivatedVolumes)

	require.NoError(t, DeleteObject(context.Background(), "tpl-x-memory", cow.KindVolume))
	require.Equal(t, []string{"tpl-x-memory"}, engine.deletedVolumes)

	require.NoError(t, DeleteObject(context.Background(), "tpl-x-rootfs", cow.KindSnapshot))
	require.Equal(t, []string{"tpl-x-rootfs"}, engine.deletedSnapshots)
}

func TestResolveObjectPath(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-x-memory": {DevicePath: "", SizeBytes: 1024},
		},
		activatePaths: map[string]string{"tpl-x-memory": "/dev/mapper/tpl-x-memory"},
	}
	useTestCowStorage(t, engine)

	path, err := ResolveObjectPath(context.Background(), "tpl-x-memory", cow.KindVolume)
	require.NoError(t, err)
	require.Equal(t, "/dev/mapper/tpl-x-memory", path)
	require.Equal(t, []string{"tpl-x-memory"}, engine.activatedVolumes)
}

func TestLegacyAliasesDelegateToStoreOps(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotPath: "/dev/mapper/tpl-alias-rootfs",
		createVolumePath:   "/dev/mapper/tpl-alias-memory",
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-alias-rootfs": {DevicePath: "/dev/mapper/tpl-alias-rootfs", SizeBytes: 4096},
		},
	}
	useTestCowStorage(t, engine)

	src := &CowSnapshotObject{Name: "sb-1-rootfs-gen0", Kind: cow.KindSnapshot}
	obj, err := CommitTemplateRootfs(context.Background(), src, "alias")
	require.NoError(t, err)
	require.Equal(t, "tpl-alias-rootfs", obj.Name)

	mem, err := CreateTemplateMemoryVolume(context.Background(), "alias", 1<<20)
	require.NoError(t, err)
	require.Equal(t, "tpl-alias-memory", mem.Name)

	require.NoError(t, DeleteCowObject(context.Background(), "tpl-alias-memory", cow.KindVolume))
	require.NoError(t, DeactivateCowObject(context.Background(), "tpl-alias-rootfs", cow.KindSnapshot))
}

func TestGetSandboxRootfsFromStorageInfo(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"sb-sb1-rootfs-gen0": {DevicePath: "/dev/mapper/sb-sb1-rootfs-gen0", SizeBytes: 8192},
		},
		activatePaths: map[string]string{"sb-sb1-rootfs-gen0": "/dev/mapper/sb-sb1-rootfs-gen0"},
	}
	useTestCowStorage(t, engine)

	info := &StorageInfo{
		SandboxID: "sb1",
		Volumes: map[string]*BackendFileInfo{
			"rootfs": {
				Name:       "rootfs",
				VolumeName: "sb-sb1-rootfs-gen0",
				Kind:       cow.KindSnapshot,
				Gen:        0,
				FilePath:   "/dev/mapper/sb-sb1-rootfs-gen0",
				SizeLimit:  8192,
			},
		},
	}
	rootfs, err := selectSnapshotRootfs(info, "rootfs")
	require.NoError(t, err)
	obj, err := backendFileInfoToSnapshotObject(context.Background(), localStorage.cowManager, rootfs)
	require.NoError(t, err)
	require.Equal(t, "sb-sb1-rootfs-gen0", obj.Name)
	require.Equal(t, uint64(8192), obj.SizeBytes)
}
