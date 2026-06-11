// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
)

func useTestCowStorage(t *testing.T, engine *fakeCowEngine) {
	t.Helper()
	previousLocalStorage := localStorage
	localStorage = &local{
		config:     &Config{StorageBackend: "cubecow"},
		cowManager: &CowVolumeManager{engine: engine},
	}
	t.Cleanup(func() {
		localStorage = previousLocalStorage
	})
}

func TestCleanupCowTemplateObjectsDispatchesByKind(t *testing.T) {
	engine := &fakeCowEngine{
		deleteSnapshotErr: &cubecow.CowError{Code: cubecow.SemNotFound, RawRC: int32(cubecow.SemNotFound)},
	}
	useTestCowStorage(t, engine)

	err := CleanupCowTemplateObjects(context.Background(), []CowObjectRef{
		{Name: "tpl-1-rootfs", Kind: CowKindSnapshot, Role: "rootfs"},
		{Name: "tpl-1-memory", Kind: CowKindVolume, Role: "memory"},
		{Name: "tpl-1-build-rootfs", Kind: CowKindVolume, Role: "build_rootfs"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"tpl-1-rootfs"}, engine.deletedSnapshots)
	assert.Equal(t, []string{"tpl-1-memory", "tpl-1-build-rootfs"}, engine.deletedVolumes)
}

func TestInspectCowObjectsReportsExistingAndMissing(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-1-rootfs": {DevicePath: "/dev/mapper/tpl-1-rootfs", SizeBytes: 4096},
		},
	}
	useTestCowStorage(t, engine)

	statuses, err := InspectCowObjects(context.Background(), []CowObjectRef{
		{Name: "tpl-1-rootfs", Kind: CowKindSnapshot, Role: "rootfs"},
		{Name: "tpl-1-memory", Kind: CowKindVolume, Role: "memory"},
	})
	require.NoError(t, err)
	require.Len(t, statuses, 2)
	assert.True(t, statuses[0].Exists)
	assert.Equal(t, "/dev/mapper/tpl-1-rootfs", statuses[0].DevicePath)
	assert.Equal(t, uint64(4096), statuses[0].SizeBytes)
	assert.False(t, statuses[1].Exists)
	assert.Empty(t, statuses[1].DevicePath)
}

func TestGetCowMetricsValidatesRequiredKeys(t *testing.T) {
	engine := &fakeCowEngine{
		metrics: map[string]uint64{
			"total_bytes":  100,
			"used_bytes":   70,
			"volume_count": 4,
		},
	}
	useTestCowStorage(t, engine)

	metrics, err := GetCowMetrics(context.Background())
	require.Nil(t, metrics)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot_count")
}

func TestResolveSnapshotForRollbackHonorsMemoryKindSnapshot(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-1-rootfs": {DevicePath: "/dev/mapper/tpl-1-rootfs", SizeBytes: 4096},
			"tpl-1-memory": {DevicePath: "/dev/mapper/tpl-1-memory", SizeBytes: 8192},
		},
	}
	useTestCowStorage(t, engine)

	refs, err := ResolveSnapshotForRollback(context.Background(), "tpl-1-rootfs", "tpl-1-memory", CowKindSnapshot)
	require.NoError(t, err)
	require.NotNil(t, refs.Memory)
	// Snapshot-kind memory blob is what the incremental CommitSandbox path
	// records, and rollback must round-trip it as-is so DeleteSnapshot (not
	// DeleteVolume) is the eventual cleanup verb.
	assert.Equal(t, CowKindSnapshot, refs.Memory.Kind)
	assert.Equal(t, "/dev/mapper/tpl-1-memory", refs.Memory.DevPath)
}

func TestResolveSnapshotForRollbackDefaultsMemoryKindToVolume(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-1-rootfs": {DevicePath: "/dev/mapper/tpl-1-rootfs", SizeBytes: 4096},
			"tpl-1-memory": {DevicePath: "/dev/mapper/tpl-1-memory", SizeBytes: 8192},
		},
	}
	useTestCowStorage(t, engine)

	// Legacy callers (and historical on-disk catalog entries) leave kind
	// empty; defaulting to volume preserves the pre-incremental behavior.
	refs, err := ResolveSnapshotForRollback(context.Background(), "tpl-1-rootfs", "tpl-1-memory", "")
	require.NoError(t, err)
	require.NotNil(t, refs.Memory)
	assert.Equal(t, CowKindVolume, refs.Memory.Kind)
}

func TestCommitTemplateMemoryFromBaseProducesSnapshotObject(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotPath: "/dev/mapper/tpl-snap-memory",
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-memory": {DevicePath: "/dev/mapper/tpl-snap-memory", SizeBytes: 4096},
		},
	}
	useTestCowStorage(t, engine)

	obj, err := CommitTemplateMemoryFromBase(context.Background(), &CowSnapshotObject{Name: "tpl-base-memory", Kind: CowKindVolume}, "snap", 4096)
	require.NoError(t, err)
	require.NotNil(t, obj)
	assert.Equal(t, "tpl-snap-memory", obj.Name)
	assert.Equal(t, CowKindSnapshot, obj.Kind)
	assert.Equal(t, "/dev/mapper/tpl-snap-memory", obj.DevPath)
	assert.Equal(t, uint64(4096), obj.SizeBytes)
	assert.Equal(t, [][2]string{{"tpl-base-memory", "tpl-snap-memory"}}, engine.createSnapshots)
}

func TestCommitTemplateMemoryFromBaseRejectsMissingSource(t *testing.T) {
	useTestCowStorage(t, &fakeCowEngine{})

	obj, err := CommitTemplateMemoryFromBase(context.Background(), nil, "snap", 4096)
	require.Error(t, err)
	assert.Nil(t, obj)
	assert.Contains(t, err.Error(), "source memory object is required")
}

func TestGetCowMetricsSuccess(t *testing.T) {
	engine := &fakeCowEngine{
		metrics: map[string]uint64{
			"total_bytes":    100,
			"used_bytes":     70,
			"volume_count":   4,
			"snapshot_count": 3,
		},
	}
	useTestCowStorage(t, engine)

	metrics, err := GetCowMetrics(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(3), metrics["snapshot_count"])
}
