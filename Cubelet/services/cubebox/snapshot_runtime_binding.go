// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

type runtimeSnapshotBindingSyncer interface {
	SyncByID(context.Context, string, ...cubes.UpdateCubeboxOpt) error
}

// setRuntimeSnapshotBinding records the commit incremental base on the
// sandbox's memory lineage. An empty snapshot id is a no-op; use
// ClearRuntimeBase to drop it.
func setRuntimeSnapshotBinding(cb *cubeboxstore.CubeBox, snapshotID string) {
	if cb == nil {
		return
	}
	cb.BindRuntimeBase(snapshotID)
}

// persistRuntimeSnapshotBinding advances or clears the durable commit base
// before a memory snapshot can clear the hypervisor's soft-dirty bitmap.
// An empty snapshotID clears the base. If persistence fails, the in-memory
// lineage is restored so both views keep the previous base.
func persistRuntimeSnapshotBinding(
	ctx context.Context,
	syncer runtimeSnapshotBindingSyncer,
	cb *cubeboxstore.CubeBox,
	snapshotID string,
) error {
	if syncer == nil {
		return fmt.Errorf("runtime snapshot binding syncer is required")
	}
	if cb == nil {
		return fmt.Errorf("cubebox is required")
	}
	previous := cb.MemoryLineageCopy()
	if snapshotID == "" {
		cb.ClearRuntimeBase()
	} else {
		setRuntimeSnapshotBinding(cb, snapshotID)
	}
	if err := syncer.SyncByID(ctx, cb.ID); err != nil {
		cb.RestoreMemoryLineage(previous)
		return fmt.Errorf("persist runtime snapshot binding for %s: %w", cb.ID, err)
	}
	return nil
}

// setRuntimeRestoreBase records which snapshot's memory image the VM was
// last restored from. Commit does not call this. An empty snapshot id is a
// no-op.
func setRuntimeRestoreBase(cb *cubeboxstore.CubeBox, snapshotID string) {
	if cb == nil {
		return
	}
	cb.BindRestoreBase(snapshotID)
}

// invalidateRuntimeSnapshotBindingsAfterOpaqueRestore drops both memory
// bases after the VM has been restored from a source Cubelet cannot later
// reflink (opaque resume). Restart does not use this: a restarted sandbox
// is a new CubeBox with an empty lineage.
func invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(cb *cubeboxstore.CubeBox) {
	if cb == nil {
		return
	}
	cb.ClearRuntimeLineage()
}
