// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

// TestErrNoBaseMemoryForIncrementalIsSentinel locks in that
// resolveBaseMemoryObject's wrapped errors must remain detectable via
// errors.Is. prepareCommitMemoryArtifact's fallback-to-full branch keys off
// this exact sentinel, so a future refactor that accidentally drops the
// %w wrapping would silently change the failure semantic from "produce a
// larger but correct snapshot" back to "fail the user-facing commit".
func TestErrNoBaseMemoryForIncrementalIsSentinel(t *testing.T) {
	wrapped := fmt.Errorf("%w: catalog lookup for snap-x: not found", ErrNoBaseMemoryForIncremental)
	assert.True(t, errors.Is(wrapped, ErrNoBaseMemoryForIncremental),
		"resolveBaseMemoryObject's wrapped sentinel must satisfy errors.Is")

	other := errors.New("some unrelated infrastructure failure")
	assert.False(t, errors.Is(other, ErrNoBaseMemoryForIncremental),
		"non-sentinel errors must not be misclassified as 'no base'")
}

// TestResolveBaseSnapshotIDFollowsCommitChain is the regression test for the
// rollback scenario:
//
//	VM 从 T 启动 -> commit A -> commit B -> rollback to A -> commit C
//
// In the old code path CommitSandbox never updated cb.Labels, so
// resolveBaseSnapshotID always returned T. That happened to be safe with
// the cumulative pagemap_anon "incremental" snapshot type, but is *unsafe*
// with the soft-dirty per-cycle delta: each commit's delta only covers
// "writes since the previous clear_soft_dirty()", so picking the wrong
// base silently drops bytes.
//
// This test verifies that as long as the success path stamps
// MasterAnnotationRuntimeSnapshotID after every successful commit (and the
// rollback path keeps doing the same, unchanged), resolveBaseSnapshotID
// follows the full ancestor chain — and in particular collapses back to
// the rolled-back-to snapshot after a rollback, which is the only base that
// matches the post-rollback CH process's just-armed soft-dirty window.
func TestResolveBaseSnapshotIDFollowsCommitChain(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		MemoryLineage: &cubeboxstore.MemoryLineage{},
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}

	// A create-request annotation is not a memory base. Create-from-template
	// stamps the lineage; a restarted sandbox keeps this empty struct.
	assert.Equal(t, "", resolveBaseSnapshotID(cb), "annotations alone are not a memory base")

	// 2. CommitSandbox(target=A) success: stamp the new commit so that
	//    the *next* commit knows to clone A as its base.
	setRuntimeSnapshotBinding(cb, "snap-A")
	assert.Equal(t, "snap-A", resolveBaseSnapshotID(cb), "after commit A: bound to A")

	// 3. CommitSandbox(target=B) success.
	setRuntimeSnapshotBinding(cb, "snap-B")
	assert.Equal(t, "snap-B", resolveBaseSnapshotID(cb), "after commit B: bound to B")

	// 4. RollbackSandbox(snapshot_id=A): rollback.go already stamps the
	//    runtime-snapshot label to the rolled-back-to snapshot id, so we
	//    just simulate the same setter call here.
	setRuntimeSnapshotBinding(cb, "snap-A")
	assert.Equal(t, "snap-A", resolveBaseSnapshotID(cb),
		"after rollback to A: binding must collapse to A so next commit clones A as base")

	// 5. CommitSandbox(target=C) success: this is the user-facing concern
	//    — without the binding update on the prior commits, this step
	//    would have inherited "tpl-T" from step 1.
	setRuntimeSnapshotBinding(cb, "snap-C")
	assert.Equal(t, "snap-C", resolveBaseSnapshotID(cb), "after commit C: bound to C")
}

// TestRuntimeBindingLabelOverridesCreateAnnotation guards the priority
// order in resolveBaseSnapshotID: the runtime label written by every
// successful commit / rollback must outrank the create-time annotations,
// otherwise a fresh sandbox that gets committed once would still resolve
// back to its original template on the next commit.
func TestRuntimeBindingLabelOverridesCreateAnnotation(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationRuntimeSnapshotID:     "create-time-snap",
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}
	setRuntimeSnapshotBinding(cb, "snap-after-commit")
	assert.Equal(t, "snap-after-commit", resolveBaseSnapshotID(cb),
		"the per-commit Labels stamp must beat both create-time annotations")
}

// TestResolveRestoreBaseSnapshotIDIgnoresMetadata pins that create-request
// labels and annotations are never a restore base. Only the lineage counts,
// so a restarted sandbox cannot inherit the pre-restart memory image.
func TestResolveRestoreBaseSnapshotIDIgnoresMetadata(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			Labels: map[string]string{
				constants.MasterAnnotationRuntimeRestoreSnapshotID: "label-restore",
			},
			Annotations: map[string]string{
				constants.MasterAnnotationRuntimeRestoreSnapshotID: "ann-restore",
				constants.MasterAnnotationRuntimeSnapshotID:        "ann-runtime",
				constants.MasterAnnotationAppSnapshotTemplateID:    "tpl-T",
			},
		},
	}
	assert.Equal(t, "", resolveRestoreBaseSnapshotID(cb))
	assert.Equal(t, "", resolveBaseSnapshotID(cb))
	assert.Equal(t, "", resolveLaunchAncestorSnapshotID(cb))

	cb.BindRestoreBase("snap-true")
	assert.Equal(t, "snap-true", resolveRestoreBaseSnapshotID(cb))
}

// TestRestoreBaseLabelDoesNotAdvanceOnCommit is the core invariant of the
// two-label split:
//
//   - setRuntimeSnapshotBindingLabels (called by Create-restore, Rollback,
//     AND Commit) bumps Labels[RuntimeSnapshotID] every time. This is what
//     drives the soft-dirty chain forward.
//   - setRuntimeRestoreBaseLabels (called by Create-restore and Rollback,
//     NEVER by Commit) freezes Labels[RuntimeRestoreSnapshotID] at the most
//     recent VM-restart point. This is what the pagemap_anon fallback
//     reaches for when the runtime-snapshot base is gone.
//
// If a future refactor accidentally calls setRuntimeRestoreBaseLabels from
// the commit path, pagemap_anon's reflink base would be a snapshot that
// was committed *after* the VM's actual last restore — and pagemap_anon's
// "anon dirty since restore" delta would be inconsistent with that base,
// silently corrupting the produced memory image. This test pins the call
// pattern so that drift is caught at compile/test time.
func TestRestoreBaseLabelDoesNotAdvanceOnCommit(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}

	// Step 1: Create-restore from tpl-T sets BOTH bindings.
	setRuntimeSnapshotBinding(cb, "tpl-T")
	setRuntimeRestoreBase(cb, "tpl-T")
	assert.Equal(t, "tpl-T", cb.RuntimeBaseID())
	assert.Equal(t, "tpl-T", cb.RestoreBaseID())

	// Step 2: Commit A advances the runtime-snapshot binding only.
	setRuntimeSnapshotBinding(cb, "snap-A")
	assert.Equal(t, "snap-A", cb.RuntimeBaseID())
	assert.Equal(t, "tpl-T", cb.RestoreBaseID(),
		"commit must not advance the restore-base")

	// Step 3: Commit B — same invariant.
	setRuntimeSnapshotBinding(cb, "snap-B")
	assert.Equal(t, "snap-B", cb.RuntimeBaseID())
	assert.Equal(t, "tpl-T", cb.RestoreBaseID())

	// Step 4: Rollback to A: rollback restarts the VM, so it bumps BOTH.
	setRuntimeSnapshotBinding(cb, "snap-A")
	setRuntimeRestoreBase(cb, "snap-A")
	assert.Equal(t, "snap-A", cb.RuntimeBaseID())
	assert.Equal(t, "snap-A", cb.RestoreBaseID())

	// Step 5: Commit C after rollback — runtime-snapshot advances; the
	//         restore-base label remains pinned at the rollback target.
	setRuntimeSnapshotBinding(cb, "snap-C")
	assert.Equal(t, "snap-C", cb.RuntimeBaseID())
	assert.Equal(t, "snap-A", cb.RestoreBaseID(),
		"after rollback-then-commit, the restore-base label must still point at A "+
			"(otherwise pagemap_anon's reflink source would point at the just-committed "+
			"C, whose memory file does not match the VM's pre-commit memory state — "+
			"feeding pagemap_anon a base that's already 'past' the bitmap)")

	// And the resolveX helpers must each pick their own label, not cross-wire.
	assert.Equal(t, "snap-C", resolveBaseSnapshotID(cb))
	assert.Equal(t, "snap-A", resolveRestoreBaseSnapshotID(cb))
}

// TestSetRuntimeRestoreBaseLabelsEmptyIDIsNoop guards against accidentally
// blanking the restore-base label by passing an empty snapshot id (e.g.
// from a failed lookup). Treating empty as a no-op preserves whatever was
// last successfully set.
func TestSetRuntimeRestoreBaseLabelsEmptyIDIsNoop(t *testing.T) {
	cb := &cubeboxstore.CubeBox{}
	setRuntimeRestoreBase(cb, "snap-A")
	assert.Equal(t, "snap-A", cb.RestoreBaseID())

	setRuntimeRestoreBase(cb, "")
	assert.Equal(t, "snap-A", cb.RestoreBaseID(),
		"empty snapshot id must be a no-op, not a clear")
}

// TestOpaqueRestoreInvalidatesIncrementalBases covers pause/resume. CubeShim's
// pause/resume path restores the VM from an internal full snapshot under
// /data/cubelet/root/pausevm/<sandbox>, not from a cubecow template/snapshot
// object that Cubelet can later resolve. Both runtime base labels must be
// invalidated so the next CommitSandbox re-anchors with a full snapshot instead
// of incorrectly overlaying pagemap_anon pages onto an older catalog entry.
func TestOpaqueRestoreInvalidatesIncrementalBases(t *testing.T) {
	cb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}
	setRuntimeSnapshotBinding(cb, "snap-B")
	setRuntimeRestoreBase(cb, "snap-A")

	invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(cb)

	assert.Equal(t, "", resolveBaseSnapshotID(cb))
	assert.Equal(t, "", resolveRestoreBaseSnapshotID(cb))

	// A successful full CommitSandbox after the opaque restore re-establishes
	// the direct runtime base for future soft-dirty commits, but it must not
	// pretend to be the VM's last restore source.
	setRuntimeSnapshotBinding(cb, "snap-full-after-resume")
	assert.Equal(t, "snap-full-after-resume", resolveBaseSnapshotID(cb))
	assert.Equal(t, "", resolveRestoreBaseSnapshotID(cb))
}

func TestResolveLaunchAncestorKeepsStartImage(t *testing.T) {
	fromTpl := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}
	fromTpl.BindLaunchAncestorOnce("tpl-T")
	assert.Equal(t, "tpl-T", resolveLaunchAncestorSnapshotID(fromTpl))

	fromSnap := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationRuntimeSnapshotID:     "snap-customer",
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}
	fromSnap.BindLaunchAncestorOnce("snap-customer")
	assert.Equal(t, "snap-customer", resolveLaunchAncestorSnapshotID(fromSnap))

	setRuntimeSnapshotBinding(fromTpl, "snap-after-commit")
	invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(fromTpl)
	assert.Equal(t, "tpl-T", resolveLaunchAncestorSnapshotID(fromTpl),
		"clearing the runtime bases must keep the launch ancestor")

	stampLaunchMemoryAncestorOnce(fromSnap, "snap-customer")
	stampPauseSnapshotID(fromSnap, "snap-pause-1")
	invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(fromSnap)
	assert.Equal(t, "snap-customer", resolveLaunchAncestorSnapshotID(fromSnap),
		"Resume must not replace the FromSnap ancestor with the pause package")

	stampLaunchMemoryAncestorOnce(fromSnap, "tpl-T")
	assert.Equal(t, "snap-customer", fromSnap.LaunchAncestorID(),
		"launch ancestor is written once and never overwritten")
}

func TestLaunchAncestorIsLastRestore(t *testing.T) {
	fromTpl := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}
	assert.False(t, launchAncestorIsLastRestore(resolveRestoreBaseSnapshotID(fromTpl), "tpl-T"),
		"an annotation without a restore base is not a license to incremental-overlay")

	setRuntimeRestoreBase(fromTpl, "tpl-T")
	assert.True(t, launchAncestorIsLastRestore(resolveRestoreBaseSnapshotID(fromTpl), "tpl-T"),
		"Create-from-template stamps restore-base to the same ancestor")

	setRuntimeRestoreBase(fromTpl, "snap-pause-1")
	stampPauseSnapshotID(fromTpl, "snap-pause-1")
	assert.False(t, launchAncestorIsLastRestore(resolveRestoreBaseSnapshotID(fromTpl), "tpl-T"),
		"after Resume the restore-base is the pause package, not the template")

	invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(fromTpl)
	assert.False(t, launchAncestorIsLastRestore(resolveRestoreBaseSnapshotID(fromTpl), "tpl-T"),
		"opaque restore must not incremental-overlay onto the ancestor")

	assert.False(t, launchAncestorIsLastRestore(resolveRestoreBaseSnapshotID(fromTpl), ""),
		"empty ancestor is never a valid incremental dest")
}

func TestImportedMemoryIsCurrentRestore(t *testing.T) {
	pauseID := "snap-pause-keep-2b"
	cb := newCubeboxWithStatusForTest("sb-2b", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(cb, pauseID)
	setRuntimeRestoreBase(cb, pauseID)
	assert.True(t, importedMemoryIsCurrentRestore(cb),
		"Resume stamps restore-base to the pause id; 2b may use the import")

	setRuntimeRestoreBase(cb, "snap-rollback-target")
	assert.False(t, importedMemoryIsCurrentRestore(cb),
		"Rollback restamps restore-base; stale ImportedMemoryVol must not be 2b")

	invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(cb)
	assert.False(t, importedMemoryIsCurrentRestore(cb),
		"opaque resume invalidates restore-base; stale import must not be 2b")

	fromSnap := newCubeboxWithStatusForTest("sb-fromsnap", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	setRuntimeRestoreBase(fromSnap, "snap-customer")
	assert.False(t, importedMemoryIsCurrentRestore(fromSnap),
		"FromSnap without a pause binding stays on catalog / full")

	forged := newCubeboxWithStatusForTest("sb-forged-2b", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	forged.AddAnnotations(map[string]string{
		constants.MasterAnnotationPauseSnapshotID:          pauseID,
		constants.MasterAnnotationRuntimeRestoreSnapshotID: pauseID,
	})
	assert.False(t, importedMemoryIsCurrentRestore(forged),
		"user Create annotations must not open Tier 2b")
}

func TestShouldUseImportedMemoryForCommit(t *testing.T) {
	pauseID := "snap-pause-keep-2b-vol"
	cb := newCubeboxWithStatusForTest("sb-2b-vol", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(cb, pauseID)
	setRuntimeRestoreBase(cb, pauseID)

	assert.True(t, shouldUseImportedMemoryForCommit(cb, &storage.CowSnapshotObject{
		Name: "sb-2b-vol-memory",
		Kind: storage.CowKindVolume,
	}), "imported vol + current restore → incremental 2b")
	assert.False(t, shouldUseImportedMemoryForCommit(cb, nil),
		"empty import must fall through to full")
	assert.False(t, shouldUseImportedMemoryForCommit(cb, &storage.CowSnapshotObject{}),
		"nameless import must fall through to full")

	setRuntimeRestoreBase(cb, "snap-after-rollback")
	assert.False(t, shouldUseImportedMemoryForCommit(cb, &storage.CowSnapshotObject{
		Name: "sb-2b-vol-memory",
		Kind: storage.CowKindVolume,
	}), "stale import after rollback must full-dump")
}

func installCommitMemoryTestHooks(t *testing.T) {
	t.Helper()
	origBase := resolveBaseMemoryObjectFn
	origRestore := resolveRestoreBaseMemoryObjectFn
	origCommit := commitMemoryFromBaseFor
	origCreate := createMemoryVolumeFor
	origImport := getImportedSandboxMemoryFor
	t.Cleanup(func() {
		resolveBaseMemoryObjectFn = origBase
		resolveRestoreBaseMemoryObjectFn = origRestore
		commitMemoryFromBaseFor = origCommit
		createMemoryVolumeFor = origCreate
		getImportedSandboxMemoryFor = origImport
	})
	resolveBaseMemoryObjectFn = func(context.Context, *cubeboxstore.CubeBox, string) (*storage.CowSnapshotObject, error) {
		return nil, ErrNoBaseMemoryForIncremental
	}
	resolveRestoreBaseMemoryObjectFn = func(context.Context, *cubeboxstore.CubeBox, string) (*storage.CowSnapshotObject, error) {
		return nil, ErrNoBaseMemoryForIncremental
	}
}

func resumeBoxForCommit2b(id, pauseID string) *cubeboxstore.CubeBox {
	cb := newCubeboxWithStatusForTest(id, cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(cb, pauseID)
	setRuntimeRestoreBase(cb, pauseID)
	return cb
}

func TestPrepareCommitMemoryArtifactTier2bIncremental(t *testing.T) {
	installCommitMemoryTestHooks(t)
	pauseID := "snap-commit-2b-000000000000001"
	cb := resumeBoxForCommit2b("sb-commit-2b", pauseID)
	getImportedSandboxMemoryFor = func(_ context.Context, _, sandboxID string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "sb-" + sandboxID + "-memory", Kind: storage.CowKindVolume}, nil
	}
	var clonedFrom string
	commitMemoryFromBaseFor = func(_ context.Context, _ string, src *storage.CowSnapshotObject, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		clonedFrom = src.Name
		return &storage.CowSnapshotObject{Name: "clone-" + id, Kind: storage.CowKindVolume}, nil
	}
	createMemoryVolumeFor = func(context.Context, string, string, uint64) (*storage.CowSnapshotObject, error) {
		t.Fatal("catalog miss + imported vol must not full-dump")
		return nil, errors.New("unexpected full")
	}

	obj, snapType, err := prepareCommitMemoryArtifact(context.Background(), log.G(context.Background()), cb, "snap-new", 4096, "s3")
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeIncremental, snapType)
	assert.Equal(t, "sb-sb-commit-2b-memory", clonedFrom)
	assert.Equal(t, "clone-snap-new", obj.Name)
}

func TestPrepareCommitMemoryArtifactFullWhenImportedEmpty(t *testing.T) {
	installCommitMemoryTestHooks(t)
	cb := resumeBoxForCommit2b("sb-commit-full", "snap-commit-full-00000000001")
	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
		return nil, nil
	}
	commitMemoryFromBaseFor = func(context.Context, string, *storage.CowSnapshotObject, string, uint64) (*storage.CowSnapshotObject, error) {
		t.Fatal("empty import must not clone")
		return nil, errors.New("unexpected clone")
	}
	createMemoryVolumeFor = func(_ context.Context, _, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "empty-" + id, Kind: storage.CowKindVolume}, nil
	}

	obj, snapType, err := prepareCommitMemoryArtifact(context.Background(), log.G(context.Background()), cb, "snap-new", 4096, "s3")
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeFull, snapType)
	assert.Equal(t, "empty-snap-new", obj.Name)
}

func TestPrepareCommitMemoryArtifact2bCloneFailFallsToFull(t *testing.T) {
	installCommitMemoryTestHooks(t)
	cb := resumeBoxForCommit2b("sb-commit-2b-fail", "snap-commit-2bfail-00000001")
	getImportedSandboxMemoryFor = func(context.Context, string, string) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "sb-commit-2b-fail-memory", Kind: storage.CowKindVolume}, nil
	}
	commitMemoryFromBaseFor = func(context.Context, string, *storage.CowSnapshotObject, string, uint64) (*storage.CowSnapshotObject, error) {
		return nil, errors.New("clone busy")
	}
	createMemoryVolumeFor = func(_ context.Context, _, id string, _ uint64) (*storage.CowSnapshotObject, error) {
		return &storage.CowSnapshotObject{Name: "empty-" + id, Kind: storage.CowKindVolume}, nil
	}

	obj, snapType, err := prepareCommitMemoryArtifact(context.Background(), log.G(context.Background()), cb, "snap-new", 4096, "s3")
	require.NoError(t, err)
	assert.Equal(t, snapshotTypeFull, snapType, "2b clone failure must fall through like Pause")
	assert.Equal(t, "empty-snap-new", obj.Name)
}
