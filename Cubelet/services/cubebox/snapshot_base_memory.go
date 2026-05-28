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
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
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
// CommitSandbox will reflink-clone for a soft-dirty / pagemap_anon
// incremental memory snapshot.
//
// Returns ErrNoBaseMemoryForIncremental wrapped with context on any of:
//   - the sandbox is not bound to any snapshot/template,
//   - the local catalog entry is missing or has no memory_vol recorded,
//   - the cubecow object can no longer be resolved on the host.
//
// Callers (notably prepareCommitMemoryArtifact) are expected to recognize
// the sentinel via errors.Is and gracefully fall back to a full snapshot;
// previous incarnations of CommitSandbox hard-failed instead, but with the
// soft-dirty path live we prefer "produce a slightly larger but correct
// snapshot" over "fail the user-facing commit when the lineage breaks".
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

// prepareCommitMemoryArtifact returns the cubecow memory object that
// cube-runtime will write its memory snapshot into, plus the snapshot type
// flag to pass to cube-runtime for this commit.
//
// Fast path (sandbox lineage intact): reflink-clone the binding base memory
// into a new cubecow snapshot keyed by templateID and ask cube-runtime for a
// soft-dirty per-cycle delta. The cloned file already contains the previous
// snapshot's memory bytes, satisfying soft-dirty's "destination must hold
// every still-clean page" precondition; the kernel-side soft-dirty bitmap
// only writes the pages the guest actually dirtied since the previous
// snapshot operation, giving a true delta and minimum disk write amplification.
//
// Fallback path (errors.Is(err, ErrNoBaseMemoryForIncremental)): no usable
// base could be resolved (sandbox not bound to a snapshot, catalog entry
// purged, upstream cubecow volume gone, etc.). Soft-dirty would be unsafe
// here — for a *restored* VM, untouched-since-restore pages are not in the
// soft-dirty bitmap and there is no base file to read them from on restore.
// Instead we create a fresh empty memory volume and ask cube-runtime for a
// full snapshot, which is self-contained regardless of soft-dirty bit state.
//
// Non-sentinel errors from resolveBaseMemoryObject (currently unreachable —
// resolveBaseMemoryObject always wraps with the sentinel — kept as
// defense-in-depth) propagate unchanged so genuine infrastructure failures
// surface to the caller.
//
// The caller owns the returned cubecow object: any subsequent failure in the
// CommitSandbox flow must call DeleteCowObject to avoid orphaned cubecow
// state.
func prepareCommitMemoryArtifact(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	cb *cubeboxstore.CubeBox,
	templateID string,
	memorySizeBytes uint64,
) (*storage.CowSnapshotObject, string, error) {
	baseMemoryObject, baseErr := resolveBaseMemoryObject(ctx, cb)
	if baseErr == nil {
		memoryObject, err := storage.CommitTemplateMemoryFromBase(ctx, baseMemoryObject, templateID, memorySizeBytes)
		if err != nil {
			return nil, "", err
		}
		stepLog.Infof("CommitSandbox: reflink-cloned base memory %s/%s -> %s, snapshot type=%s",
			baseMemoryObject.Name, baseMemoryObject.Kind, memoryObject.Name, snapshotTypeSoftDirty)
		return memoryObject, snapshotTypeSoftDirty, nil
	}
	if !errors.Is(baseErr, ErrNoBaseMemoryForIncremental) {
		return nil, "", baseErr
	}
	stepLog.Warnf("CommitSandbox: base memory unavailable (%v), falling back to full snapshot", baseErr)
	memoryObject, err := storage.CreateTemplateMemoryVolume(ctx, templateID, memorySizeBytes)
	if err != nil {
		return nil, "", err
	}
	stepLog.Infof("CommitSandbox: created empty memory volume %s/%s, snapshot type=%s",
		memoryObject.Name, memoryObject.Kind, snapshotTypeFull)
	return memoryObject, snapshotTypeFull, nil
}
