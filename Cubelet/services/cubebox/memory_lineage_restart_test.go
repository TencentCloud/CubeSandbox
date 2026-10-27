// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

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
	assert.False(t, launchAncestorIsLastRestore(resolveRestoreBaseSnapshotID(restarted), "tpl-T"))

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
	assert.True(t, launchAncestorIsLastRestore(resolveRestoreBaseSnapshotID(created), "tpl-T"))
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

// crossNodeFromSnapBox mirrors what Create leaves behind for a cross-node
// FromSnap: no pause package exists yet, the customer snapshot is both the
// launch ancestor and the restore base, and Create imported a sandbox-private
// memory disk. Pause stamps its own id before preparing the artifact.
func crossNodeFromSnapBox(id, snapshotID string) *cubeboxstore.CubeBox {
	cb := newCubeboxWithStatusForTest(id, cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	cb.BindLaunchAncestorOnce(snapshotID)
	cb.BindRuntimeBase(snapshotID)
	cb.BindRestoreBase(snapshotID)
	stampPauseSnapshotID(cb, "snap-pause-new-00000000000001")
	return cb
}

// Pause after a cross-node FromSnap must stay incremental. The imported disk is
// the customer snapshot's memory image and the VM is still sitting on it, so it
// is a valid pagemap_anon base even though no pause package has ever existed.
// Regression guard: requiring a prior pause id here degraded every cross-node
// restore to a full capture.
func TestPauseAfterCrossNodeFromSnapStaysIncremental(t *testing.T) {
	installCommitMemoryTestHooks(t)
	cb := crossNodeFromSnapBox("sb-crossnode", "snap-customer-0000000001")

	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "sb-crossnode-memory", Kind: storage.CowKindVolume}, nil
	}
	var clonedFrom string
	commitMemoryFromBaseFor = func(_ context.Context, _ string, src *storage.CowSnapshotObject, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		clonedFrom = src.Name
		return &storage.CowSnapshotObject{Name: "clone-" + id, Kind: storage.CowKindVolume}, nil
	}
	createMemoryVolumeFor = func(context.Context, string, string, uint64) (*storage.CowSnapshotObject, error) {
		t.Fatal("a cross-node FromSnap still on its ancestor must not full-dump")
		return nil, errors.New("unexpected full")
	}

	obj, snapType, err := preparePauseMemoryArtifact(
		context.Background(), log.G(context.Background()), cb,
		"snap-pause-new-00000000000001", "", 4096, "s3",
	)
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeIncremental, snapType)
	assert.Equal(t, "sb-crossnode-memory", clonedFrom)
	assert.Equal(t, "clone-snap-pause-new-00000000000001", obj.Name)
}

// A Commit between the restore and the Pause does not move Restore, so the
// second capture in that window must stay incremental too.
func TestPauseAfterCrossNodeFromSnapAndCommitStaysIncremental(t *testing.T) {
	installCommitMemoryTestHooks(t)
	cb := crossNodeFromSnapBox("sb-crossnode-commit", "snap-customer-0000000002")
	setRuntimeSnapshotBinding(cb, "snap-commit-00000000000001") // Commit advanced Runtime only

	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "sb-crossnode-commit-memory", Kind: storage.CowKindVolume}, nil
	}
	commitMemoryFromBaseFor = func(_ context.Context, _ string, src *storage.CowSnapshotObject, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "clone-" + id, Kind: storage.CowKindVolume}, nil
	}

	_, snapType, err := preparePauseMemoryArtifact(
		context.Background(), log.G(context.Background()), cb,
		"snap-pause-new-00000000000002", "", 4096, "s3",
	)
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeIncremental, snapType)
}

// Every shape that replaced guest memory while leaving the imported disk behind
// must still be rejected. These all share "restore base no longer describes the
// running guest".
func TestPauseRejectsImportedVolumeWhenRestoreBaseIsNotLiveBase(t *testing.T) {
	cases := []struct {
		name        string
		lineage     func(*cubeboxstore.CubeBox)
		priorPause  string
		description string
	}{
		{
			name:        "restart",
			lineage:     func(cb *cubeboxstore.CubeBox) {}, // empty lineage
			description: "restart rebuilds the CubeBox with no lineage",
		},
		{
			name: "opaque resume",
			lineage: func(cb *cubeboxstore.CubeBox) {
				cb.BindLaunchAncestorOnce("tpl-T")
				cb.ClearRuntimeLineage()
			},
			description: "opaque resume clears both live bases",
		},
		{
			name: "rollback to a non-ancestor snapshot",
			lineage: func(cb *cubeboxstore.CubeBox) {
				cb.BindLaunchAncestorOnce("tpl-T")
				cb.BindRuntimeBase("snap-rollback-target")
				cb.BindRestoreBase("snap-rollback-target")
			},
			description: "rollback target is not the launch ancestor",
		},
		{
			name: "create from image with no ancestor",
			lineage: func(cb *cubeboxstore.CubeBox) {
				cb.BindRuntimeBase("snap-x")
				cb.BindRestoreBase("snap-x")
			},
			description: "no ancestor to compare against",
		},
		{
			// Rollback to the launch ancestor makes restoreID == ancestorID
			// again, so the ids alone look exactly like a cross-node FromSnap.
			// The stale import is only released best-effort, so this arm must
			// also refuse anything that followed a rollback.
			name: "rollback back to the launch ancestor",
			lineage: func(cb *cubeboxstore.CubeBox) {
				cb.BindLaunchAncestorOnce("tpl-T")
				cb.BindRuntimeBase("tpl-T")
				cb.BindRestoreBase("tpl-T")
				if err := cb.PrepareRollbackGuestMetricsEpoch(time.Now().UTC()); err != nil {
					panic(err)
				}
			},
			description: "a rollback to the ancestor leaves a possibly stale import",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installCommitMemoryTestHooks(t)
			cb := newCubeboxWithStatusForTest("sb-reject", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
			cb.RestoreMemoryLineage(&cubeboxstore.MemoryLineage{})
			tc.lineage(cb)
			stampPauseSnapshotID(cb, "snap-pause-reject")

			getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
				return &storage.CowSnapshotObject{Name: "stale-import", Kind: storage.CowKindVolume}, nil
			}
			commitMemoryFromBaseFor = func(context.Context, string, *storage.CowSnapshotObject, string, uint64) (*storage.CowSnapshotObject, error) {
				t.Fatalf("%s: must not clone the stale import", tc.description)
				return nil, nil
			}
			createMemoryVolumeFor = func(_ context.Context, _, id string, _ uint64) (*storage.CowSnapshotObject, error) {
				return &storage.CowSnapshotObject{Name: "full-" + id, Kind: storage.CowKindVolume}, nil
			}

			obj, snapType, err := preparePauseMemoryArtifact(
				context.Background(), log.G(context.Background()), cb,
				"snap-pause-reject", tc.priorPause, 4096, "s3",
			)
			require.NoError(t, err)
			assert.Equal(t, snapshotTypeFull, snapType, tc.description)
			assert.Equal(t, "full-snap-pause-reject", obj.Name)
		})
	}
}

// A rollback whose target happens to equal the prior pause id must still be
// refused: the import was minted from the pre-rollback image.
func TestPauseRejectsRolledBackSandboxEvenWhenRestoreMatchesPause(t *testing.T) {
	installCommitMemoryTestHooks(t)
	cb := newCubeboxWithStatusForTest("sb-rb-pause", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	const target = "snap-pause-rb-00000000001"
	cb.BindLaunchAncestorOnce("tpl-T")
	cb.BindRuntimeBase(target)
	cb.BindRestoreBase(target)
	// Rollback target == the pause id this Pause is replacing.
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(time.Now().UTC()))

	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "stale-import", Kind: storage.CowKindVolume}, nil
	}
	commitMemoryFromBaseFor = func(context.Context, string, *storage.CowSnapshotObject, string, uint64) (*storage.CowSnapshotObject, error) {
		t.Fatal("a rolled-back sandbox must not clone its pre-rollback import")
		return nil, nil
	}
	createMemoryVolumeFor = func(_ context.Context, _, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "full-" + id, Kind: storage.CowKindVolume}, nil
	}

	obj, snapType, err := preparePauseMemoryArtifact(
		context.Background(), log.G(context.Background()), cb,
		target, target, 4096, "s3",
	)
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeFull, snapType)
	assert.Equal(t, "full-"+target, obj.Name)
}

// The rollback guard must survive the create that a pause/resume performs. That
// create resets the epoch to fresh_create and re-mints the import disk, so a
// resume after a rollback starts a new window and the guard stops applying.
func TestRollbackGuardScopeMatchesImportLifetime(t *testing.T) {
	installCommitMemoryTestHooks(t)
	cb := newCubeboxWithStatusForTest("sb-rb-resume", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	cb.BindLaunchAncestorOnce("tpl-T")
	cb.BindRestoreBase("tpl-T")
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(time.Now().UTC()))
	require.True(t, sandboxRolledBackSinceCreate(cb))

	// Resume re-creates the row: fresh epoch, freshly cloned import.
	require.NoError(t, cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochFreshCreate, time.Now().UTC()))
	assert.False(t, sandboxRolledBackSinceCreate(cb),
		"resume re-mints the import disk, so the previous rollback no longer describes it")

	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "resumed-import", Kind: storage.CowKindVolume}, nil
	}
	commitMemoryFromBaseFor = func(_ context.Context, _ string, src *storage.CowSnapshotObject, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "clone-" + id, Kind: storage.CowKindVolume}, nil
	}

	_, snapType, err := preparePauseMemoryArtifact(
		context.Background(), log.G(context.Background()), cb,
		"snap-pause-after-resume", "", 4096, "s3",
	)
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeIncremental, snapType,
		"the resumed import is the live restore image for the new incarnation")
}
