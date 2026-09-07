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
func resolveBaseMemoryObject(ctx context.Context, cb *cubeboxstore.CubeBox, backend string) (*storage.CowSnapshotObject, error) {
	return resolveMemoryObjectFromSnapshotID(ctx, backend, resolveBaseSnapshotID(cb))
}

// resolveRestoreBaseSnapshotID returns the snapshot id whose memory image
// the running VM was last *restored* from. Set by Create's restore path and
// by Rollback; intentionally NOT updated by Commit. The pagemap_anon
// fallback in prepareCommitMemoryArtifact uses this to find a base file
// whose contents match the VM's "everything not dirty since last restore"
// state, which is exactly the precondition pagemap_anon requires.
//
// Falls back to the create-time annotation as best-effort for sandboxes
// that predate the new label, so first-commit-after-upgrade still gets the
// pagemap_anon path instead of the full-rewrite path.
func resolveRestoreBaseSnapshotID(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	if v := strings.TrimSpace(cb.Labels[constants.MasterAnnotationRuntimeRestoreSnapshotID]); v != "" {
		return v
	}
	if v := strings.TrimSpace(cb.Annotations[constants.MasterAnnotationRuntimeRestoreSnapshotID]); v != "" {
		return v
	}
	// Best-effort backstop for sandboxes created before
	// MasterAnnotationRuntimeRestoreSnapshotID was introduced. Note that
	// this is only correct if the sandbox has not been rolled back since
	// create — once it has been rolled back, only the new label tracks
	// the post-rollback restore source. Caller has nothing better here,
	// so we return it and let the catalog lookup decide.
	if v := strings.TrimSpace(cb.Annotations[constants.MasterAnnotationRuntimeSnapshotID]); v != "" {
		return v
	}
	if v := strings.TrimSpace(cb.Annotations[constants.MasterAnnotationAppSnapshotTemplateID]); v != "" {
		return v
	}
	return ""
}

// resolveRestoreBaseMemoryObject is the pagemap_anon fallback's analogue
// of resolveBaseMemoryObject: resolves the cubecow memory object for the
// snapshot the VM was last restored from. Same sentinel-error contract.
func resolveRestoreBaseMemoryObject(ctx context.Context, cb *cubeboxstore.CubeBox, backend string) (*storage.CowSnapshotObject, error) {
	return resolveMemoryObjectFromSnapshotID(ctx, backend, resolveRestoreBaseSnapshotID(cb))
}

// resolveLaunchAncestorSnapshotID is the memory base Pause copies from:
// the template or customer snapshot this sandbox was first started from.
// It ignores last-commit / last-restore labels and the pause snap itself
// (those change across Pause/Resume/Commit).
func resolveLaunchAncestorSnapshotID(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	pauseID := stampedPauseSnapshotID(cb)
	for _, id := range []string{
		cb.Labels[constants.MasterAnnotationLaunchMemorySnapshotID],
		cb.Annotations[constants.MasterAnnotationLaunchMemorySnapshotID],
		cb.Annotations[constants.MasterAnnotationRuntimeSnapshotID],
		cb.Annotations[constants.MasterAnnotationAppSnapshotTemplateID],
	} {
		if isUsableLaunchAncestorID(id, pauseID) {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

// launchAncestorIsLastRestore reports whether an incremental overlay onto a
// clone of the launch ancestor is valid. pagemap_anon records anon pages
// dirty *since the last restore*; the dest must already hold every still-
// clean page from that restore. The template/first customer snap only
// satisfies that when the VM was last restored from the same image — the
// first Pause after Create-from-template.
//
// After Resume the restore-base is the pause package. XFS keeps that
// catalog so the next Pause clones it (S3 already has a live memory
// volume). Cloning the template and asking for incremental would drop
// every page that landed in the pause image and stayed clean. The next
// restore then comes up holey: agent health can still answer, then
// set_guest_date_time/reseed hang on ttrpc.
func launchAncestorIsLastRestore(cb *cubeboxstore.CubeBox, ancestorID string) bool {
	ancestorID = strings.TrimSpace(ancestorID)
	if ancestorID == "" {
		return false
	}
	restoreID := strings.TrimSpace(resolveRestoreBaseSnapshotID(cb))
	if restoreID == runtimeSnapshotBindingInvalidID {
		return false
	}
	if restoreID == "" {
		// No restore-base label: image-based start, or a sandbox that
		// predates the label. Ancestor incremental matches first Pause
		// after Create-from-template. Do not consult the in-progress
		// pause id — Pause stamps that *before* this function runs.
		return true
	}
	return restoreID == ancestorID
}

func isUsableLaunchAncestorID(id, pauseID string) bool {
	id = strings.TrimSpace(id)
	if id == "" || id == runtimeSnapshotBindingInvalidID {
		return false
	}
	if pauseID != "" && id == pauseID {
		return false
	}
	return true
}

func stampLaunchMemoryAncestorOnce(cb *cubeboxstore.CubeBox, id string) {
	if cb == nil {
		return
	}
	id = strings.TrimSpace(id)
	if !isUsableLaunchAncestorID(id, stampedPauseSnapshotID(cb)) {
		return
	}
	if strings.TrimSpace(cb.Labels[constants.MasterAnnotationLaunchMemorySnapshotID]) != "" ||
		strings.TrimSpace(cb.Annotations[constants.MasterAnnotationLaunchMemorySnapshotID]) != "" {
		return
	}
	cb.AddLabels(map[string]string{constants.MasterAnnotationLaunchMemorySnapshotID: id})
	cb.AddAnnotations(map[string]string{constants.MasterAnnotationLaunchMemorySnapshotID: id})
}

func resolveMemoryObjectFromSnapshotID(ctx context.Context, backend, snapshotID string) (*storage.CowSnapshotObject, error) {
	if snapshotID == "" {
		return nil, fmt.Errorf("%w: sandbox is not bound to any snapshot or template", ErrNoBaseMemoryForIncremental)
	}
	entry, err := storage.GetLocalSnapshotFor(ctx, backend, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("%w: catalog lookup for %s: %v", ErrNoBaseMemoryForIncremental, snapshotID, err)
	}
	memoryVol := strings.TrimSpace(entry.MemoryVol)
	if memoryVol == "" {
		return nil, fmt.Errorf("%w: catalog entry for %s has no memory_vol", ErrNoBaseMemoryForIncremental, snapshotID)
	}
	memoryKind := strings.TrimSpace(entry.MemoryKind)
	if memoryKind == "" {
		memoryKind = storage.CowKindVolume
	}
	devPath, err := storage.ResolveObjectPathFor(ctx, backend, memoryVol, memoryKind)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve %s/%s: %v", ErrNoBaseMemoryForIncremental, memoryVol, memoryKind, err)
	}
	return &storage.CowSnapshotObject{
		Name:    memoryVol,
		Kind:    memoryKind,
		DevPath: devPath,
	}, nil
}

// preparePauseMemoryArtifact builds the dest the VMM writes an incremental
// overlay into.
//
//  1. Own memory volume (S3 same-node resume clone or cross-node import).
//     S3 Resume deletes the pause package after cloning onto this disk.
//  2. Launch ancestor, but only when that image is the VM's last restore
//     (first Pause after Create-from-template). See
//     [launchAncestorIsLastRestore].
//  3. Last-restore catalog. XFS Resume keeps the pause package so this
//     Pause can incremental-overlay onto that image; Cubelet GCs the
//     old package after this Pause succeeds (or on Destroy).
//  4. Full dump into an empty volume when no catalog/volume base is
//     left (template gone, or an S3 own-volume clone that failed).
func preparePauseMemoryArtifact(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	cb *cubeboxstore.CubeBox,
	templateID string,
	memorySizeBytes uint64,
	backend string,
) (*storage.CowSnapshotObject, string, error) {
	stampLaunchMemoryAncestorOnce(cb, resolveLaunchAncestorSnapshotID(cb))
	sandboxID := ""
	if cb != nil {
		sandboxID = strings.TrimSpace(cb.ID)
		if sandboxID == "" {
			sandboxID = strings.TrimSpace(cb.SandboxID)
		}
	}
	if live, liveErr := storage.GetImportedSandboxMemoryFor(ctx, backend, sandboxID); liveErr != nil {
		stepLog.Warnf("pause memory artifact: own volume lookup failed (%v); trying a catalog base", liveErr)
	} else if live != nil {
		memoryObject, cloneErr := storage.CommitMemoryFromBaseFor(ctx, backend, live, templateID, memorySizeBytes)
		if cloneErr == nil {
			stepLog.Infof("pause memory artifact: cloned own volume %s/%s -> %s, snapshot type=%s",
				live.Name, live.Kind, memoryObject.Name, snapshotTypeIncremental)
			return memoryObject, snapshotTypeIncremental, nil
		}
		stepLog.Warnf("pause memory artifact: own volume %s clone failed (%v); trying a catalog base", live.Name, cloneErr)
	}

	ancestorID := resolveLaunchAncestorSnapshotID(cb)
	restoreID := resolveRestoreBaseSnapshotID(cb)
	if launchAncestorIsLastRestore(cb, ancestorID) {
		base, err := resolveMemoryObjectFromSnapshotID(ctx, backend, ancestorID)
		if err == nil {
			memoryObject, cloneErr := storage.CommitMemoryFromBaseFor(ctx, backend, base, templateID, memorySizeBytes)
			if cloneErr != nil {
				return nil, "", cloneErr
			}
			stepLog.Infof("pause memory artifact: cloned launch ancestor %s (%s/%s) -> %s, snapshot type=%s",
				ancestorID, base.Name, base.Kind, memoryObject.Name, snapshotTypeIncremental)
			return memoryObject, snapshotTypeIncremental, nil
		}
		if !errors.Is(err, ErrNoBaseMemoryForIncremental) {
			return nil, "", err
		}
		stepLog.Warnf("pause memory artifact: launch ancestor unavailable (%v); falling back to full snapshot", err)
	} else {
		stepLog.Warnf("pause memory artifact: launch ancestor %s is not last restore %s; skipping ancestor incremental",
			ancestorID, restoreID)
		if restoreID != "" && restoreID != runtimeSnapshotBindingInvalidID {
			base, err := resolveMemoryObjectFromSnapshotID(ctx, backend, restoreID)
			if err == nil {
				memoryObject, cloneErr := storage.CommitMemoryFromBaseFor(ctx, backend, base, templateID, memorySizeBytes)
				if cloneErr != nil {
					return nil, "", cloneErr
				}
				stepLog.Infof("pause memory artifact: cloned last restore %s (%s/%s) -> %s, snapshot type=%s",
					restoreID, base.Name, base.Kind, memoryObject.Name, snapshotTypeIncremental)
				return memoryObject, snapshotTypeIncremental, nil
			}
			if !errors.Is(err, ErrNoBaseMemoryForIncremental) {
				return nil, "", err
			}
			stepLog.Warnf("pause memory artifact: last restore %s unavailable (%v); falling back to full snapshot", restoreID, err)
		}
	}

	memoryObject, createErr := storage.CreateMemoryVolumeFor(ctx, backend, templateID, memorySizeBytes)
	if createErr != nil {
		return nil, "", createErr
	}
	stepLog.Infof("pause memory artifact: created empty memory volume %s/%s, snapshot type=%s",
		memoryObject.Name, memoryObject.Kind, snapshotTypeFull)
	return memoryObject, snapshotTypeFull, nil
}

// prepareCommitMemoryArtifact returns the cubecow memory object that
// cube-runtime (CommitSandbox) will write its memory snapshot into, plus
// the snapshot type flag for this capture.
//
// Three-tier degradation:
//
// Tier 1 — soft-dirty + reflink from previous snapshot (the happy path).
// resolveBaseMemoryObject returns the runtime-binding snapshot's memory
// file; we reflink-clone it as the destination for this commit and ask
// cube-runtime for a soft-dirty per-cycle delta. The cloned file already
// contains the previous snapshot's memory bytes, satisfying soft-dirty's
// "destination must hold every still-clean page" precondition; the
// kernel-side soft-dirty bitmap only writes the pages the guest actually
// dirtied since the previous snapshot operation, giving a true delta and
// minimum disk write amplification.
//
// Tier 2 — incremental(pagemap_anon) + reflink from last-restore base.
// Reached when the runtime-binding snapshot's memory file no longer exists
// (e.g. operator deleted the most recent commit between commits). We look
// up the snapshot the VM was last *restored* from (tracked by a separate
// label that Commit does not advance) and reflink from its memory file.
// pagemap_anon's bitmap captures every anon page dirty *since the last
// restore*, which is exactly the delta we need to overlay onto that base to
// produce a self-contained image. If the VM is later restored from an
// opaque source that Cubelet cannot reflink from, that restore path
// invalidates this label so this tier is skipped. Pause uses
// preparePauseMemoryArtifact: own vol, then ancestor only when it is
// the last restore, otherwise last-restore catalog or full.
//
// Tier 3 — full + fresh empty volume. The last-resort fallback when even
// the last-restore base file is gone (e.g. the source template was deleted
// while the VM kept running). cube-runtime writes the entire memory image
// onto a fresh sparse file; correct in all cases but the costliest, hence
// only used when neither delta path can resolve a base.
//
// Non-sentinel errors propagate unchanged so genuine infrastructure
// failures surface to the caller.
//
// The caller owns the returned cubecow object: any subsequent failure in
// CommitSandbox must delete it to avoid orphaned cubecow state.
func prepareCommitMemoryArtifact(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	cb *cubeboxstore.CubeBox,
	templateID string,
	memorySizeBytes uint64,
	backend string,
) (*storage.CowSnapshotObject, string, error) {
	// ─── Tier 1: soft-dirty over previous-snapshot base ───────────────
	baseMemoryObject, baseErr := resolveBaseMemoryObject(ctx, cb, backend)
	if baseErr == nil {
		memoryObject, err := storage.CommitMemoryFromBaseFor(ctx, backend, baseMemoryObject, templateID, memorySizeBytes)
		if err != nil {
			return nil, "", err
		}
		stepLog.Infof("memory artifact: reflink-cloned base memory %s/%s -> %s, snapshot type=%s",
			baseMemoryObject.Name, baseMemoryObject.Kind, memoryObject.Name, snapshotTypeSoftDirty)
		return memoryObject, snapshotTypeSoftDirty, nil
	}
	if !errors.Is(baseErr, ErrNoBaseMemoryForIncremental) {
		return nil, "", baseErr
	}

	// ─── Tier 2: pagemap_anon over last-restore base ──────────────────
	// soft-dirty's bitmap was last reset at the previous commit, so its
	// state would not match the older last-restore file we're reflinking
	// from. pagemap_anon's "anon pages currently dirty" set, on the other
	// hand, was last reset at the VM's last restore — exactly the moment
	// captured by the snapshot id stored in the restore-base label —
	// which makes its bitmap consistent with this base.
	restoreBase, restoreErr := resolveRestoreBaseMemoryObject(ctx, cb, backend)
	if restoreErr == nil {
		memoryObject, err := storage.CommitMemoryFromBaseFor(ctx, backend, restoreBase, templateID, memorySizeBytes)
		if err != nil {
			return nil, "", err
		}
		stepLog.Warnf("memory artifact: previous-snapshot base unavailable (%v); "+
			"falling back to incremental(pagemap_anon) over last-restore base %s/%s -> %s",
			baseErr, restoreBase.Name, restoreBase.Kind, memoryObject.Name)
		return memoryObject, snapshotTypeIncremental, nil
	}
	if !errors.Is(restoreErr, ErrNoBaseMemoryForIncremental) {
		return nil, "", restoreErr
	}

	// ─── Tier 3: full + fresh empty volume ────────────────────────────
	stepLog.Warnf("memory artifact: both previous-snapshot base (%v) and last-restore base (%v) "+
		"unavailable; falling back to full snapshot", baseErr, restoreErr)
	memoryObject, err := storage.CreateMemoryVolumeFor(ctx, backend, templateID, memorySizeBytes)
	if err != nil {
		return nil, "", err
	}
	stepLog.Infof("memory artifact: created empty memory volume %s/%s, snapshot type=%s",
		memoryObject.Name, memoryObject.Kind, snapshotTypeFull)
	return memoryObject, snapshotTypeFull, nil
}
