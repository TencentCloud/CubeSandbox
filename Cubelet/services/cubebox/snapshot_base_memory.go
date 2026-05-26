// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

// ErrNoBaseMemoryForIncremental is returned when CommitSandbox cannot
// determine a base memory object for the running sandbox. Without a base the
// hypervisor's incremental memory snapshot cannot be produced (it would have
// nothing to overlay anonymous CoW pages onto), so callers must surface this
// to the user instead of silently degrading to a full snapshot.
var ErrNoBaseMemoryForIncremental = errors.New("no base memory object for incremental snapshot")

// resolveBaseSnapshotID returns the logical snapshot id the running sandbox is
// currently bound to, in priority order:
//
//  1. cb.Labels[MasterAnnotationRuntimeSnapshotID]: stamped by RollbackSandbox
//     after a successful rollback, so it always reflects the most recent
//     runtime-snapshot ancestor.
//  2. cb.Annotations[MasterAnnotationRuntimeSnapshotID]: present when the
//     sandbox was directly created from a runtime snapshot and never rolled
//     back; this is what the create flow stamps into the request annotations.
//  3. cb.Annotations[MasterAnnotationAppSnapshotTemplateID]: the original
//     template id used at create time; this is the lowest-priority fallback
//     because a more recent runtime snapshot supersedes it.
//
// Returns "" when none of these are set (e.g. fresh image-based sandbox with
// no template lineage), which the caller must treat as "no base available".
func resolveBaseSnapshotID(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	if v := strings.TrimSpace(cb.Labels[constants.MasterAnnotationRuntimeSnapshotID]); v != "" {
		return v
	}
	if v := strings.TrimSpace(cb.Annotations[constants.MasterAnnotationRuntimeSnapshotID]); v != "" {
		return v
	}
	if v := strings.TrimSpace(cb.Annotations[constants.MasterAnnotationAppSnapshotTemplateID]); v != "" {
		return v
	}
	return ""
}

// resolveBaseMemoryObject looks up the cubecow memory object that backs the
// snapshot the sandbox is currently bound to. This is the source that
// CommitSandbox will reflink-clone for an incremental memory snapshot.
//
// Hard-fails (rather than degrading to a full snapshot) on any of:
//   - the sandbox is not bound to any snapshot/template,
//   - the local catalog entry is missing or has no memory_vol recorded,
//   - the cubecow object can no longer be resolved on the host.
//
// Hard-failing keeps the user-observable contract crisp: when CommitSandbox
// claims an incremental snapshot, the workload truly produced an incremental
// snapshot. Silent fallback would lead to surprising "why is my snapshot
// twice the size?" reports.
func resolveBaseMemoryObject(ctx context.Context, cb *cubeboxstore.CubeBox) (*storage.CowSnapshotObject, error) {
	baseSnapshotID := resolveBaseSnapshotID(cb)
	if baseSnapshotID == "" {
		return nil, fmt.Errorf("%w: sandbox is not bound to any snapshot or template", ErrNoBaseMemoryForIncremental)
	}
	entry, err := storage.GetLocalSnapshot(ctx, baseSnapshotID)
	if err != nil {
		return nil, fmt.Errorf("%w: catalog lookup for %s: %v", ErrNoBaseMemoryForIncremental, baseSnapshotID, err)
	}
	memoryVol := strings.TrimSpace(entry.MemoryVol)
	if memoryVol == "" {
		return nil, fmt.Errorf("%w: catalog entry for %s has no memory_vol", ErrNoBaseMemoryForIncremental, baseSnapshotID)
	}
	memoryKind := strings.TrimSpace(entry.MemoryKind)
	if memoryKind == "" {
		memoryKind = storage.CowKindVolume
	}
	devPath, err := storage.ResolveCowDevPath(ctx, memoryVol, memoryKind)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve %s/%s: %v", ErrNoBaseMemoryForIncremental, memoryVol, memoryKind, err)
	}
	return &storage.CowSnapshotObject{
		Name:    memoryVol,
		Kind:    memoryKind,
		DevPath: devPath,
	}, nil
}
