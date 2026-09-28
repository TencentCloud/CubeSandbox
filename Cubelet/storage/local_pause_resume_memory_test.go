// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"testing"

	"github.com/containerd/plugin"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func pauseResumeCreateContext(sandboxID string, ann map[string]string) *workflow.CreateContext {
	return &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sandboxID},
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations:  ann,
		},
	}
}

func TestPauseResumeMemoryOwnerID(t *testing.T) {
	t.Parallel()

	pauseAnn := map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
	}
	require.Equal(t, "sb-1", pauseResumeMemoryOwnerID(pauseResumeCreateContext("sb-1", pauseAnn)))
	require.Equal(t, "sb-desired", pauseResumeMemoryOwnerID(pauseResumeCreateContext("", map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
		constants.MasterAnnotationDesiredSandboxID:  "sb-desired",
	})))
	require.Empty(t, pauseResumeMemoryOwnerID(pauseResumeCreateContext("", pauseAnn)))
	require.Empty(t, pauseResumeMemoryOwnerID(nil))
	require.Empty(t, pauseResumeMemoryOwnerID(pauseResumeCreateContext("sb-1", map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID: "snap-customer",
	})))
	cross := crossNodeAnnotations()
	cross[constants.MasterAnnotationPauseSnapshotID] = "snap-pause1"
	cross[constants.MasterAnnotationRuntimeSnapshotID] = "snap-pause1"
	require.Empty(t, pauseResumeMemoryOwnerID(pauseResumeCreateContext("sb-1", cross)))
}

func TestPauseResumeClonesLiveMemory(t *testing.T) {
	t.Parallel()
	require.False(t, pauseResumeClonesLiveMemory(""))
	require.False(t, pauseResumeClonesLiveMemory(cow.BackendXFS))
	require.False(t, pauseResumeClonesLiveMemory("cubecow"))
	require.True(t, pauseResumeClonesLiveMemory(cow.BackendS3))
}

func TestPrefetchRestoreMemoryVolURLS3FailsClosedWithoutSandboxID(t *testing.T) {
	l := &local{config: &Config{StorageBackend: "cubecow"}}
	_, _, err := l.prefetchRestoreMemoryVolURL(context.Background(), pauseResumeCreateContext("", map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
		constants.MasterAnnotationStorageBackend:    cow.BackendS3,
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "sandbox id")
}

func TestPrefetchRestoreMemoryVolURLXFSPauseResumeDoesNotRequireSandboxID(t *testing.T) {
	l := &local{config: &Config{StorageBackend: "cubecow"}}
	_, imported, err := l.prefetchRestoreMemoryVolURL(context.Background(), pauseResumeCreateContext("", map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
	}))
	// Catalog miss is expected; XFS mmap's the original snap file and
	// must not fail closed waiting to clone.
	if err != nil {
		require.NotContains(t, err.Error(), "sandbox id")
	}
	require.Empty(t, imported)
}

func TestPrefetchRestoreMemoryVolURLFromSnapDoesNotClone(t *testing.T) {
	l := &local{config: &Config{StorageBackend: "cubecow"}}
	_, imported, err := l.prefetchRestoreMemoryVolURL(context.Background(), pauseResumeCreateContext("sb-1", map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID: "snap-customer",
	}))
	// Catalog miss is expected; the point is FromSnap must not fail closed
	// as a pause resume, and must not record a cloned memory disk.
	if err != nil {
		require.NotContains(t, err.Error(), "sandbox id")
	}
	require.Empty(t, imported)
}

func TestPrefetchRestoreMemoryVolURLXFSPauseResumeUsesOriginalSnapshot(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fake := &fakeCowVolumeManager{
		resolvePaths: map[string]string{"tpl-snap-pause1-memory": "/data/cow/tpl-snap-pause1-memory"},
	}
	l := &local{config: cfg, cowManager: fake}
	require.NoError(t, l.init(&plugin.InitContext{Context: context.Background()}))
	seedTestSnapshotCatalog(t, "snap-pause1", "tpl-snap-pause1-memory", CowKindSnapshot)

	url, imported, err := l.prefetchRestoreMemoryVolURL(context.Background(), pauseResumeCreateContext("sb-1", map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
	}))
	require.NoError(t, err)
	require.Empty(t, imported)
	require.Equal(t, "file:///data/cow/tpl-snap-pause1-memory", url)
	require.Empty(t, fake.cloneMemoryCalls)
	require.Equal(t, []fakeCowResolveCall{{name: "tpl-snap-pause1-memory", kind: CowKindSnapshot}}, fake.resolveCalls)
}

func TestPrefetchRestoreMemoryVolURLS3PauseResumeClones(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	xfs := &fakeCowVolumeManager{}
	s3 := &fakeCowVolumeManager{
		resolvePaths: map[string]string{
			SandboxMemoryName("sb-1"): "/dev/mapper/sb-sb-1-memory",
		},
	}
	l := &local{config: cfg, cowManager: xfs, s3CowManager: s3}
	require.NoError(t, l.init(&plugin.InitContext{Context: context.Background()}))
	seedTestSnapshotCatalogFor(t, cow.BackendS3, "snap-pause1", "tpl-snap-pause1-memory", CowKindSnapshot)

	url, imported, err := l.prefetchRestoreMemoryVolURL(context.Background(), pauseResumeCreateContext("sb-1", map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause1",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause1",
		constants.MasterAnnotationStorageBackend:    cow.BackendS3,
	}))
	require.NoError(t, err)
	require.Equal(t, SandboxMemoryName("sb-1"), imported)
	require.Equal(t, "file:///dev/mapper/sb-sb-1-memory", url)
	require.Equal(t, []fakeCowCloneMemoryCall{{sandboxID: "sb-1", sourceName: "tpl-snap-pause1-memory"}}, s3.cloneMemoryCalls)
	require.Empty(t, xfs.cloneMemoryCalls)
}
