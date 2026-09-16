// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

const runtimeSnapshotBindingInvalidID = "invalid-runtime-restore-base"

type runtimeSnapshotBindingSyncer interface {
	SyncByID(context.Context, string, ...cubes.UpdateCubeboxOpt) error
}

// runtimeSnapshotBindingLabels returns the labels that bind a sandbox to a
// snapshot. v4: only the logical snapshot id (and attach timestamp) are
// recorded on the sandbox metadata. Physical memory volume / dev names are
// not propagated here because Cubelet's local snapshot catalog is the sole
// source of truth and is keyed by the snapshot id.
func runtimeSnapshotBindingLabels(snapshotID string, attachedAt time.Time) map[string]string {
	if snapshotID == "" {
		return nil
	}
	labels := map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID: snapshotID,
	}
	if !attachedAt.IsZero() {
		labels[constants.MasterAnnotationRuntimeSnapshotAttachedAt] = attachedAt.UTC().Format(time.RFC3339Nano)
	}
	return labels
}

func setRuntimeSnapshotBindingLabels(cb *cubeboxstore.CubeBox, snapshotID string, attachedAt time.Time) {
	if cb == nil {
		return
	}
	labels := runtimeSnapshotBindingLabels(snapshotID, attachedAt)
	if len(labels) == 0 {
		return
	}
	cb.Metadata.AddLabels(labels)
}

// persistRuntimeSnapshotBinding advances the durable baseline before a memory
// snapshot can clear the hypervisor's soft-dirty bitmap. If persistence fails,
// the in-memory labels are restored so both views keep referencing the same
// previous baseline.
func persistRuntimeSnapshotBinding(
	ctx context.Context,
	syncer runtimeSnapshotBindingSyncer,
	cb *cubeboxstore.CubeBox,
	snapshotID string,
	attachedAt time.Time,
) error {
	if syncer == nil {
		return fmt.Errorf("runtime snapshot binding syncer is required")
	}
	if cb == nil {
		return fmt.Errorf("cubebox is required")
	}
	previousLabels := copyCubeBoxLabels(cb)
	setRuntimeSnapshotBindingLabels(cb, snapshotID, attachedAt)
	if err := syncer.SyncByID(ctx, cb.ID); err != nil {
		restoreCubeBoxLabels(cb, previousLabels)
		return fmt.Errorf("persist runtime snapshot binding for %s: %w", cb.ID, err)
	}
	return nil
}

// runtimeRestoreBaseLabels records which snapshot's memory image the VM was
// last restored from. Unlike runtimeSnapshotBindingLabels, this binding is
// updated only at restore boundaries — Create's restore-from-snapshot path
// and Rollback — and is intentionally NOT touched by Commit. This is what
// makes the binding usable as a base for the pagemap_anon (incremental)
// snapshot path: pagemap_anon's dirty set is "anon pages currently dirty",
// i.e. everything written to since the *last restore*. The base file used
// for the FICLONE in that fallback path must therefore contain the VM's
// memory state at last restore, which is exactly the snapshot id stamped
// here.
func runtimeRestoreBaseLabels(snapshotID string, attachedAt time.Time) map[string]string {
	if snapshotID == "" {
		return nil
	}
	labels := map[string]string{
		constants.MasterAnnotationRuntimeRestoreSnapshotID: snapshotID,
	}
	if !attachedAt.IsZero() {
		labels[constants.MasterAnnotationRuntimeRestoreSnapshotAttachedAt] = attachedAt.UTC().Format(time.RFC3339Nano)
	}
	return labels
}

func setRuntimeRestoreBaseLabels(cb *cubeboxstore.CubeBox, snapshotID string, attachedAt time.Time) {
	if cb == nil {
		return
	}
	labels := runtimeRestoreBaseLabels(snapshotID, attachedAt)
	if len(labels) == 0 {
		return
	}
	cb.Metadata.AddLabels(labels)
}

// invalidateRuntimeSnapshotBindingsAfterOpaqueRestore marks both runtime
// memory bases as unusable after the VM has been restored from a source that
// Cubelet cannot later reflink from. CoW Pause/Resume now writes a catalog
// memory object like CommitSandbox; Create-from-pause-snap stamps a real
// restore-base id. This helper remains for leftover in-place resume
// convergence (TaskResumed) that still has no cubecow dest.
func invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(cb *cubeboxstore.CubeBox, attachedAt time.Time) {
	if cb == nil {
		return
	}
	labels := map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID:        runtimeSnapshotBindingInvalidID,
		constants.MasterAnnotationRuntimeRestoreSnapshotID: runtimeSnapshotBindingInvalidID,
	}
	if !attachedAt.IsZero() {
		ts := attachedAt.UTC().Format(time.RFC3339Nano)
		labels[constants.MasterAnnotationRuntimeSnapshotAttachedAt] = ts
		labels[constants.MasterAnnotationRuntimeRestoreSnapshotAttachedAt] = ts
	}
	cb.Metadata.AddLabels(labels)
}
