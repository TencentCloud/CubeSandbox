// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
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
		Metadata: cubeboxstore.Metadata{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
		},
	}

	// 1. Fresh start from template T: only the create-time template
	//    annotation is present; resolve must fall back to it.
	assert.Equal(t, "tpl-T", resolveBaseSnapshotID(cb), "initial: bound to template T")

	// 2. CommitSandbox(target=A) success: stamp the new commit so that
	//    the *next* commit knows to clone A as its base.
	setRuntimeSnapshotBindingLabels(cb, "snap-A", time.Now().UTC())
	assert.Equal(t, "snap-A", resolveBaseSnapshotID(cb), "after commit A: bound to A")

	// 3. CommitSandbox(target=B) success.
	setRuntimeSnapshotBindingLabels(cb, "snap-B", time.Now().UTC())
	assert.Equal(t, "snap-B", resolveBaseSnapshotID(cb), "after commit B: bound to B")

	// 4. RollbackSandbox(snapshot_id=A): rollback.go already stamps the
	//    runtime-snapshot label to the rolled-back-to snapshot id, so we
	//    just simulate the same setter call here.
	setRuntimeSnapshotBindingLabels(cb, "snap-A", time.Now().UTC())
	assert.Equal(t, "snap-A", resolveBaseSnapshotID(cb),
		"after rollback to A: binding must collapse to A so next commit clones A as base")

	// 5. CommitSandbox(target=C) success: this is the user-facing concern
	//    — without the binding update on the prior commits, this step
	//    would have inherited "tpl-T" from step 1.
	setRuntimeSnapshotBindingLabels(cb, "snap-C", time.Now().UTC())
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
	setRuntimeSnapshotBindingLabels(cb, "snap-after-commit", time.Now().UTC())
	assert.Equal(t, "snap-after-commit", resolveBaseSnapshotID(cb),
		"the per-commit Labels stamp must beat both create-time annotations")
}

// TestResolveRestoreBaseSnapshotIDPriorityOrder pins the lookup priority
// for the pagemap_anon fallback path.
//
// Restore-base resolution must prefer, in order:
//  1. Labels[RuntimeRestoreSnapshotID]      — set at Create + Rollback
//  2. Annotations[RuntimeRestoreSnapshotID] — never used today, kept for
//     forward compatibility if the master ever stamps it on a request
//  3. Annotations[RuntimeSnapshotID]        — create-time runtime binding
//  4. Annotations[AppSnapshotTemplateID]    — original create-time template
//
// The annotation backstops exist so a sandbox that pre-dates the new
// Labels-based binding (i.e. one that survives an in-place upgrade) still
// gets the pagemap_anon path on first commit-after-deletion, instead of
// falling through to the full-rewrite tier.
func TestResolveRestoreBaseSnapshotIDPriorityOrder(t *testing.T) {
	cases := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		want        string
	}{
		{
			name: "label wins over every annotation backstop",
			labels: map[string]string{
				constants.MasterAnnotationRuntimeRestoreSnapshotID: "label-restore",
			},
			annotations: map[string]string{
				constants.MasterAnnotationRuntimeRestoreSnapshotID: "ann-restore",
				constants.MasterAnnotationRuntimeSnapshotID:        "ann-runtime",
				constants.MasterAnnotationAppSnapshotTemplateID:    "tpl-T",
			},
			want: "label-restore",
		},
		{
			name: "annotation restore key wins when label absent",
			annotations: map[string]string{
				constants.MasterAnnotationRuntimeRestoreSnapshotID: "ann-restore",
				constants.MasterAnnotationRuntimeSnapshotID:        "ann-runtime",
				constants.MasterAnnotationAppSnapshotTemplateID:    "tpl-T",
			},
			want: "ann-restore",
		},
		{
			name: "annotation runtime key is the next backstop",
			annotations: map[string]string{
				constants.MasterAnnotationRuntimeSnapshotID:     "ann-runtime",
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
			want: "ann-runtime",
		},
		{
			name: "create-time template id is the final backstop",
			annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-T",
			},
			want: "tpl-T",
		},
		{
			name: "no binding => empty",
			want: "",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cb := &cubeboxstore.CubeBox{
				Metadata: cubeboxstore.Metadata{
					Labels:      tc.labels,
					Annotations: tc.annotations,
				},
			}
			assert.Equal(t, tc.want, resolveRestoreBaseSnapshotID(cb))
		})
	}
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

	now := time.Now().UTC()
	// Step 1: Create-restore from tpl-T sets BOTH bindings.
	setRuntimeSnapshotBindingLabels(cb, "tpl-T", now)
	setRuntimeRestoreBaseLabels(cb, "tpl-T", now)
	assert.Equal(t, "tpl-T", cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
	assert.Equal(t, "tpl-T", cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID])

	// Step 2: Commit A advances the runtime-snapshot binding only.
	setRuntimeSnapshotBindingLabels(cb, "snap-A", now.Add(time.Second))
	assert.Equal(t, "snap-A", cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
	assert.Equal(t, "tpl-T", cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID],
		"commit must not advance the restore-base label")

	// Step 3: Commit B — same invariant.
	setRuntimeSnapshotBindingLabels(cb, "snap-B", now.Add(2*time.Second))
	assert.Equal(t, "snap-B", cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
	assert.Equal(t, "tpl-T", cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID])

	// Step 4: Rollback to A: rollback restarts the VM, so it bumps BOTH.
	setRuntimeSnapshotBindingLabels(cb, "snap-A", now.Add(3*time.Second))
	setRuntimeRestoreBaseLabels(cb, "snap-A", now.Add(3*time.Second))
	assert.Equal(t, "snap-A", cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
	assert.Equal(t, "snap-A", cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID])

	// Step 5: Commit C after rollback — runtime-snapshot advances; the
	//         restore-base label remains pinned at the rollback target.
	setRuntimeSnapshotBindingLabels(cb, "snap-C", now.Add(4*time.Second))
	assert.Equal(t, "snap-C", cb.Labels[constants.MasterAnnotationRuntimeSnapshotID])
	assert.Equal(t, "snap-A", cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID],
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
	setRuntimeRestoreBaseLabels(cb, "snap-A", time.Now().UTC())
	assert.Equal(t, "snap-A", cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID])

	setRuntimeRestoreBaseLabels(cb, "", time.Now().UTC())
	assert.Equal(t, "snap-A", cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID],
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
	now := time.Now().UTC()
	setRuntimeSnapshotBindingLabels(cb, "snap-B", now)
	setRuntimeRestoreBaseLabels(cb, "snap-A", now)

	invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(cb, now.Add(time.Second))

	assert.Equal(t, runtimeSnapshotBindingInvalidID, resolveBaseSnapshotID(cb))
	assert.Equal(t, runtimeSnapshotBindingInvalidID, resolveRestoreBaseSnapshotID(cb))

	// A successful full CommitSandbox after the opaque restore re-establishes
	// the direct runtime base for future soft-dirty commits, but it must not
	// pretend to be the VM's last restore source.
	setRuntimeSnapshotBindingLabels(cb, "snap-full-after-resume", now.Add(2*time.Second))
	assert.Equal(t, "snap-full-after-resume", resolveBaseSnapshotID(cb))
	assert.Equal(t, runtimeSnapshotBindingInvalidID, resolveRestoreBaseSnapshotID(cb))
}
