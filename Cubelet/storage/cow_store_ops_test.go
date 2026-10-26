// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
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

func TestS3CowName(t *testing.T) {
	t.Parallel()
	require.Equal(t, cow.NameS3, (&S3Cow{}).Name())
}

func TestRefuseS3PackageObjectDelete(t *testing.T) {
	t.Parallel()
	const sandboxID = "b1c1d1e1f1"
	const snapshotID = "snap-4c0c3e1f"

	for _, tc := range []struct {
		name    string
		backend string
		object  string
		refuse  bool
	}{
		{name: "own rootfs generation", backend: cow.BackendS3, object: SandboxRootfsName(sandboxID, 3)},
		{name: "own imported memory", backend: cow.BackendS3, object: SandboxMemoryName(sandboxID)},
		{name: "own metadata disk", backend: cow.BackendS3, object: S3MetadataVolumeName(sandboxID)},
		{name: "own rollback metadata copy", backend: cow.BackendS3, object: S3MetadataVolumeName(RollbackMetadataOwnerID(sandboxID))},
		{name: "package rootfs snapshot", backend: cow.BackendS3, object: "tpl-" + snapshotID + "-rootfs", refuse: true},
		{name: "package memory snapshot", backend: cow.BackendS3, object: "tpl-" + snapshotID + "-memory-snap", refuse: true},
		{name: "package metadata snapshot", backend: cow.BackendS3, object: S3MetadataSnapshotName(snapshotID), refuse: true},
		{name: "template rootfs", backend: cow.BackendS3, object: "tpl-tpl-a8862a17-rootfs", refuse: true},
		// A package named after the sandbox it was captured from is still a
		// package: a substring test would have let this one through.
		{name: "package captured from this sandbox", backend: cow.BackendS3, object: "tpl-snap-" + sandboxID + "-rootfs", refuse: true},
		{name: "xfs is never refused", backend: cow.BackendXFS, object: "tpl-" + snapshotID + "-rootfs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.refuse, refuseS3PackageObjectDelete(tc.backend, tc.object, sandboxID))
		})
	}

	require.False(t, refuseS3PackageObjectDelete(cow.BackendS3, "", sandboxID))
	require.False(t, refuseS3PackageObjectDelete(cow.BackendS3, "tpl-x-rootfs", ""))
}

func TestStoreForSelectsXfsAndS3(t *testing.T) {
	engine := &fakeCowEngine{}
	useTestCowStorage(t, engine)

	xfs, err := StoreFor(cow.BackendXFS)
	require.NoError(t, err)
	require.Equal(t, cow.NameXfsCow, xfs.Name())

	s3Store, err := StoreFor(cow.BackendS3)
	require.NoError(t, err)
	require.Equal(t, cow.NameS3, s3Store.Name())

	// Both coexist; selecting one does not replace the other.
	require.Equal(t, cow.NameXfsCow, ActiveCowStore().Name())
	require.Equal(t, cow.NameS3, ActiveS3CowStore().Name())
}

func TestCommitRootfsForUsesS3Store(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotPath: "/dev/mapper/tpl-snap-s3-rootfs",
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-s3-rootfs": {DevicePath: "/dev/mapper/tpl-snap-s3-rootfs", SizeBytes: 1 << 20},
		},
	}
	useTestCowStorage(t, engine)

	source := &CowSnapshotObject{Name: "sb-1-rootfs-gen0", Kind: cow.KindSnapshot}
	obj, err := CommitRootfsFor(context.Background(), cow.BackendS3, source, "snap-s3")
	require.NoError(t, err)
	require.Equal(t, "tpl-snap-s3-rootfs", obj.Name)
	require.Equal(t, [][2]string{{"sb-1-rootfs-gen0", "tpl-snap-s3-rootfs"}}, engine.createSnapshots)
}

func TestUploadSnapshotExportsRealUUIDs(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs": {SizeBytes: 1 << 20},
			"tpl-snap-1-memory": {SizeBytes: 64 << 20},
		},
	}
	useTestCowStorage(t, engine)

	st, err := SnapshotUploadStatus(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.Equal(t, cow.RemoteStateRunning, st.State)

	uuids, err := UploadSnapshot(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.False(t, uuids.Empty())
	require.Equal(t, "export-tpl-snap-1-rootfs", uuids.Rootfs)
	require.Equal(t, "export-tpl-snap-1-memory", uuids.Memory)
	require.Equal(t, []string{"tpl-snap-1-rootfs", "tpl-snap-1-memory"}, engine.exportSnapshots)

	st, err = SnapshotUploadStatus(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	// Volumes exist but export_status is empty → still running until DONE.
	require.Equal(t, cow.RemoteStateRunning, st.State)
	require.Equal(t, "snap-1", st.SnapshotID)
	require.Equal(t, uuids.Rootfs, st.RemoteUUIDs.Rootfs)

	_, err = UploadSnapshot(context.Background(), cow.BackendXFS, "snap-1")
	require.NoError(t, err)
	st, err = SnapshotUploadStatus(context.Background(), cow.BackendXFS, "snap-1")
	require.NoError(t, err)
	require.Equal(t, cow.RemoteStateReady, st.State)
}

func TestUploadSnapshotFailsWithoutMintingUUID(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs": {SizeBytes: 1 << 20},
			"tpl-snap-1-memory": {SizeBytes: 64 << 20},
		},
		exportErr: fmt.Errorf("export denied"),
	}
	useTestCowStorage(t, engine)

	uuids, err := UploadSnapshot(context.Background(), cow.BackendS3, "snap-1")
	require.Error(t, err)
	require.Nil(t, uuids)
	require.Contains(t, err.Error(), "export denied")

	st, err := SnapshotUploadStatus(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.Equal(t, cow.RemoteStateFailed, st.State)
}

func TestActivateSnapshotActivatesLocalObjects(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs": {SizeBytes: 1 << 20},
			"tpl-snap-1-memory": {SizeBytes: 64 << 20},
		},
	}
	useTestCowStorage(t, engine)

	require.NoError(t, ActivateSnapshot(context.Background(), cow.BackendS3, "snap-1"))
	require.Equal(t, []string{"tpl-snap-1-rootfs", "tpl-snap-1-memory"}, engine.activatedVolumes)
}

func TestActivateSnapshotSkipsMetadataSnap(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs":              {SizeBytes: 1 << 20},
			"tpl-snap-1-memory":              {SizeBytes: 64 << 20},
			S3MetadataSnapshotName("snap-1"): {SizeBytes: 8 << 20},
		},
	}
	useTestCowStorage(t, engine)

	require.NoError(t, ActivateSnapshot(context.Background(), cow.BackendS3, "snap-1"))
	require.Equal(t, []string{"tpl-snap-1-rootfs", "tpl-snap-1-memory"}, engine.activatedVolumes)
	require.NotContains(t, engine.activatedVolumes, S3MetadataSnapshotName("snap-1"))
}

func TestActivateSnapshotFailsWhenMissing(t *testing.T) {
	useTestCowStorage(t, &fakeCowEngine{})

	err := ActivateSnapshot(context.Background(), cow.BackendS3, "snap-missing")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCowObjectMissing)
}

func TestActivateSnapshotWorksOnXFS(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs": {SizeBytes: 1 << 20},
			"tpl-snap-1-memory": {SizeBytes: 64 << 20},
		},
	}
	useTestCowStorage(t, engine)

	require.NoError(t, ActivateSnapshot(context.Background(), cow.BackendXFS, "snap-1"))
	require.Equal(t, []string{"tpl-snap-1-rootfs", "tpl-snap-1-memory"}, engine.activatedVolumes)
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

func TestReleaseRollbackReplacedVolumesDeletesOldRootfs(t *testing.T) {
	engine := &fakeCowEngine{}
	useTestCowStorage(t, engine)

	sandboxID := "aaaa"
	old := SandboxRootfsName(sandboxID, 0)
	err := ReleaseRollbackReplacedVolumes(context.Background(), cow.BackendS3, sandboxID, &CowSnapshotObject{
		Name: old,
		Kind: cow.KindVolume,
	})
	require.NoError(t, err)
	require.Equal(t, []string{old}, engine.deactivatedVolumes)
	require.Equal(t, []string{old}, engine.deletedVolumes)
	require.Empty(t, engine.deletedSnapshots)
}

func TestReleaseRollbackReplacedVolumesDefersDeleteFailure(t *testing.T) {
	engine := &fakeCowEngine{
		deleteErrByName: map[string]error{
			"sb-aaaa-rootfs-gen0": errors.New("device busy"),
		},
	}
	useTestCowStorage(t, engine)

	err := ReleaseRollbackReplacedVolumes(context.Background(), cow.BackendS3, "aaaa", &CowSnapshotObject{
		Name: SandboxRootfsName("aaaa", 0),
		Kind: cow.KindVolume,
	})
	require.NoError(t, err)
	require.Equal(t, []string{SandboxRootfsName("aaaa", 0)}, engine.deletedVolumes)
}

func TestReleaseRollbackReplacedVolumesSkipsNilRootfs(t *testing.T) {
	engine := &fakeCowEngine{}
	useTestCowStorage(t, engine)

	require.NoError(t, ReleaseRollbackReplacedVolumes(context.Background(), cow.BackendS3, "sb-1", nil))
	require.Empty(t, engine.deletedVolumes)
	require.Empty(t, engine.deactivatedVolumes)
}
