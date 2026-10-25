// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestRestartedSandboxHasNoMemoryBase(t *testing.T) {
	// createContainers builds the restarted sandbox from the original request,
	// which still names the template, and with an empty lineage.
	restarted := &cubeboxstore.CubeBox{
		MemoryLineage: &cubeboxstore.MemoryLineage{},
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
				constants.MasterAnnotationRuntimeSnapshotID:     "snap-before-restart",
			},
		},
	}
	assert.Equal(t, "", resolveBaseSnapshotID(restarted))
	assert.Equal(t, "", resolveRestoreBaseSnapshotID(restarted))
	assert.Equal(t, "", resolveLaunchAncestorSnapshotID(restarted))
	assert.False(t, launchAncestorIsLastRestore(restarted, "tpl-T"))

	installCommitMemoryTestHooks(t)
	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "stale-import", Kind: storage.CowKindVolume}, nil
	}
	commitMemoryFromBaseFor = func(context.Context, string, *storage.CowSnapshotObject, string, uint64) (*storage.CowSnapshotObject, error) {
		t.Fatal("restart must not clone a pre-restart memory base")
		return nil, nil
	}
	createMemoryVolumeFor = func(_ context.Context, _, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "full-" + id, Kind: storage.CowKindVolume}, nil
	}
	obj, snapType, err := prepareCommitMemoryArtifact(
		context.Background(), log.G(context.Background()), restarted, "snap-after", 4096, "xfs",
	)
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeFull, snapType)
	assert.Equal(t, "full-snap-after", obj.Name)
}

func TestRestoreCreateStampsLineageFromRequest(t *testing.T) {
	created := &cubeboxstore.CubeBox{
		MemoryLineage: &cubeboxstore.MemoryLineage{},
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}
	created.BindLaunchAncestorOnce(launchAncestorFromRequest(created))
	setRuntimeSnapshotBinding(created, "tpl-T")
	setRuntimeRestoreBase(created, "tpl-T")

	assert.Equal(t, "tpl-T", resolveBaseSnapshotID(created))
	assert.Equal(t, "tpl-T", resolveRestoreBaseSnapshotID(created))
	assert.Equal(t, "tpl-T", resolveLaunchAncestorSnapshotID(created))
	assert.True(t, launchAncestorIsLastRestore(created, "tpl-T"))
}

// A restarted sandbox keeps the pre-restart ImportedMemoryVol on disk (restart
// deliberately retains StorageInfo), but its guest memory was replaced. Pause
// must not clone that stale disk; it has to fall through to a full capture.
func TestRestartThenPauseDoesNotClonePreRestartOwnVolume(t *testing.T) {
	installCommitMemoryTestHooks(t)
	restarted := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			ID: "sb-restarted",
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
		MemoryLineage: &cubeboxstore.MemoryLineage{},
	}
	// Pause stamps the new pause id before preparing the artifact.
	priorPause := "snap-pause-before-restart"
	stampPauseSnapshotID(restarted, "snap-pause-after-restart")

	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "sb-restarted-memory", Kind: storage.CowKindVolume}, nil
	}
	commitMemoryFromBaseFor = func(context.Context, string, *storage.CowSnapshotObject, string, uint64) (*storage.CowSnapshotObject, error) {
		t.Fatal("Pause must not clone a stale pre-restart memory volume")
		return nil, nil
	}
	createMemoryVolumeFor = func(_ context.Context, _, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "full-" + id, Kind: storage.CowKindVolume}, nil
	}

	obj, snapType, err := preparePauseMemoryArtifact(
		context.Background(), log.G(context.Background()), restarted,
		"snap-pause-after-restart", priorPause, 4096, "s3",
	)
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeFull, snapType)
	assert.Equal(t, "full-snap-pause-after-restart", obj.Name)
}

// While the imported volume is still the restore base of a live pause/resume
// sandbox, Pause keeps using it so the resume package stays incremental.
func TestPauseUsesOwnVolumeWhileItIsCurrentRestore(t *testing.T) {
	installCommitMemoryTestHooks(t)
	live := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{ID: "sb-live"},
	}
	priorPause := "snap-pause-live"
	live.BindRestoreBase(priorPause)
	stampPauseSnapshotID(live, "snap-pause-next")

	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "sb-live-memory", Kind: storage.CowKindVolume}, nil
	}
	commitMemoryFromBaseFor = func(_ context.Context, _ string, src *storage.CowSnapshotObject, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "clone-" + src.Name + "-" + id, Kind: storage.CowKindVolume}, nil
	}

	obj, snapType, err := preparePauseMemoryArtifact(
		context.Background(), log.G(context.Background()), live,
		"snap-pause-next", priorPause, 4096, "s3",
	)
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeIncremental, snapType)
	assert.Equal(t, "clone-sb-live-memory-snap-pause-next", obj.Name)
}

// applyCreateMemoryLineage is the single decision point for a create's memory
// lineage. Cover every branch so a refactor cannot silently reintroduce the
// annotation fallback for restarts.
func TestApplyCreateMemoryLineage(t *testing.T) {
	restartRequest := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
				constants.MasterAnnotationRuntimeSnapshotID:     "snap-before-restart",
			},
		},
	}
	restart := &workflow.CreateContext{
		IsRestart: true,
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations:  restartRequest.Annotations,
		},
	}
	applyCreateMemoryLineage(restartRequest, restart)
	require.NotNil(t, restartRequest.MemoryLineage)
	assert.Equal(t, "", restartRequest.RuntimeBaseID(), "restart must not inherit pre-restart memory")
	assert.Equal(t, "", restartRequest.RestoreBaseID())
	assert.Equal(t, "", restartRequest.LaunchAncestorID())

	// Create-from-template restores memory: all three bindings come from the
	// request annotation.
	fromTemplate := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}
	createOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations:  fromTemplate.Annotations,
		},
	}
	applyCreateMemoryLineage(fromTemplate, createOpts)
	assert.Equal(t, "tpl-T", fromTemplate.RuntimeBaseID())
	assert.Equal(t, "tpl-T", fromTemplate.RestoreBaseID())
	assert.Equal(t, "tpl-T", fromTemplate.LaunchAncestorID())

	// A plain image-based create has no lineage at all.
	plain := &cubeboxstore.CubeBox{}
	applyCreateMemoryLineage(plain, &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
		},
	})
	require.NotNil(t, plain.MemoryLineage)
	assert.Equal(t, "", plain.RuntimeBaseID())
	assert.Equal(t, "", plain.RestoreBaseID())
	assert.Equal(t, "", plain.LaunchAncestorID())
}
