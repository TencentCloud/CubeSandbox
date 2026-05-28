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
