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

// Test hooks so prepareCommitMemoryArtifact's ladder can be driven without cubecow.
var (
	resolveBaseMemoryObjectFn        = resolveBaseMemoryObject
	resolveRestoreBaseMemoryObjectFn = resolveRestoreBaseMemoryObject
	commitMemoryFromBaseFor          = storage.CommitMemoryFromBaseFor
	createMemoryVolumeFor            = storage.CreateMemoryVolumeFor
	getImportedSandboxMemoryFor      = storage.GetImportedSandboxMemoryFor
)

// ErrNoBaseMemoryForIncremental is returned when CommitSandbox cannot
// determine a base memory object for the running sandbox. Without a base the
// hypervisor's incremental memory snapshot cannot be produced (it would have
// nothing to overlay anonymous CoW pages onto), so callers must surface this
// to the user instead of silently degrading to a full snapshot.
var ErrNoBaseMemoryForIncremental = errors.New("no base memory object for incremental snapshot")

// resolveBaseSnapshotID returns the snapshot the next soft-dirty commit
// clones. Only the sandbox's memory lineage is consulted. Create-request
// annotations are not a memory base: a restart rebuilds the CubeBox with an
// empty lineage, and reading the original template id here would overlay
// the new VM's dirty pages onto pre-restart memory.
func resolveBaseSnapshotID(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	return cb.RuntimeBaseID()
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

// resolveRestoreBaseSnapshotID returns the snapshot whose memory image the
// running VM was last restored from. Set by Create's restore path and by
// Rollback; Commit does not advance it. Empty means there is no pagemap_anon
// base. Create-request annotations are never a fallback.
func resolveRestoreBaseSnapshotID(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	return cb.RestoreBaseID()
}

// resolveRestoreBaseMemoryObject is the pagemap_anon fallback's analogue
// of resolveBaseMemoryObject: resolves the cubecow memory object for the
// snapshot the VM was last restored from. Same sentinel-error contract.
func resolveRestoreBaseMemoryObject(ctx context.Context, cb *cubeboxstore.CubeBox, backend string) (*storage.CowSnapshotObject, error) {
	return resolveMemoryObjectFromSnapshotID(ctx, backend, resolveRestoreBaseSnapshotID(cb))
}

// resolveLaunchAncestorSnapshotID is the memory base Pause copies from:
// the template or customer snapshot this sandbox was first started from.
// It ignores the last-commit / last-restore bindings and the pause snap
// itself (those change across Pause/Resume/Commit).
func resolveLaunchAncestorSnapshotID(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	return cb.LaunchAncestorID()
}

// launchAncestorFromRequest is the create-time translation of the request
// into a launch ancestor. It is the only place a create annotation may
// become a memory base, and only when this create actually restores memory.
// Restart does not call it.
func launchAncestorFromRequest(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	pauseID := stampedPauseSnapshotID(cb)
	for _, id := range []string{
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
// After Resume the restore-base is the pause package. Resume keeps that
// catalog so the next Pause can clone it (S3 also has a live memory
// volume). Cloning the template and asking for incremental would drop
// every page that landed in the pause image and stayed clean. The next
// restore then comes up holey: agent health can still answer, then
// set_guest_date_time/reseed hang on ttrpc.
func launchAncestorIsLastRestore(restoreID, ancestorID string) bool {
	ancestorID = strings.TrimSpace(ancestorID)
	if ancestorID == "" {
		return false
	}
	// No restore base means this VM's memory is not the ancestor image.
	// Create-from-template stamps both together. Opaque resume and restart
	// leave the restore base empty on purpose; treating that as "ancestor
	// incremental is fine" would overlay fresh pages onto stale memory.
	return strings.TrimSpace(restoreID) == ancestorID
}

func isUsableLaunchAncestorID(id, pauseID string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
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
	if !isUsableLaunchAncestorID(id, stampedPauseSnapshotID(cb)) {
		return
	}
	cb.BindLaunchAncestorOnce(id)
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
//     Resume keeps the pause catalog; this volume is the live clone of
//     that image (or the cross-node import when no local catalog exists).
//  2. Launch ancestor, but only when that image is the VM's last restore
//     (first Pause after Create-from-template). See
//     [launchAncestorIsLastRestore].
//  3. Last-restore catalog. Resume keeps the pause package so this Pause
//     can incremental-overlay onto that image; Cubelet GCs the old
//     package after this Pause succeeds (or on Destroy).
//  4. Full dump into an empty volume when no catalog/volume base is
//     left (template gone, or an S3 own-volume clone that failed).
func preparePauseMemoryArtifact(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	cb *cubeboxstore.CubeBox,
	templateID, priorPauseID string,
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
	// Resolve the lineage once for the decision below. Pause already holds the
	// sandbox lifecycle lock, so this is about one decision reading one sample
	// rather than bracing for concurrent writers.
	ancestorID := resolveLaunchAncestorSnapshotID(cb)
	restoreID := resolveRestoreBaseSnapshotID(cb)
	if live, liveErr := getImportedSandboxMemoryFor(ctx, backend, sandboxID); liveErr != nil {
		stepLog.Warnf("pause memory artifact: own volume lookup failed (%v); trying a catalog base", liveErr)
	} else if live != nil {
		// The imported/cloned disk is only a valid pagemap_anon base while it
		// is still the image this VM was restored from. Restart and opaque
		// resume leave the disk behind but replace guest memory, so cloning it
		// would overlay the new VM's anon pages onto pre-restart bytes. Commit
		// applies the same gate through importedMemoryIsCurrentRestore; Pause
		// must not skip it.
		switch {
		case !importedMemoryWasLivePauseBase(restoreID, priorPauseID, ancestorID, sandboxRolledBackSinceCreate(cb)):
			stepLog.Warnf("pause memory artifact: own volume %s is not the live restore base for %s; skipping it", live.Name, sandboxID)
		default:
			memoryObject, cloneErr := commitMemoryFromBaseFor(ctx, backend, live, templateID, memorySizeBytes)
			if cloneErr == nil {
				stepLog.Infof("pause memory artifact: cloned own volume %s/%s -> %s, snapshot type=%s",
					live.Name, live.Kind, memoryObject.Name, snapshotTypeIncremental)
				return memoryObject, snapshotTypeIncremental, nil
			}
			stepLog.Warnf("pause memory artifact: own volume %s clone failed (%v); trying a catalog base", live.Name, cloneErr)
		}
	}

	if launchAncestorIsLastRestore(restoreID, ancestorID) {
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
		if restoreID != "" {
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

	memoryObject, createErr := createMemoryVolumeFor(ctx, backend, templateID, memorySizeBytes)
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
// Four-tier degradation:
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
// up the snapshot the VM was last *restored* from (a separate lineage
// binding that Commit does not advance) and reflink from its memory file.
// pagemap_anon's bitmap captures every anon page dirty *since the last
// restore*, which is exactly the delta we need to overlay onto that base to
// produce a self-contained image. If the VM is later restored from an
// opaque source that Cubelet cannot reflink from, that restore path clears
// this binding so this tier is skipped. Pause uses
// preparePauseMemoryArtifact: own vol, then ancestor only when it is
// the last restore, otherwise last-restore catalog or full.
//
// Tier 2b — incremental(pagemap_anon) + clone of the S3 own volume.
// Cross-node Resume never writes a local pause catalog; the imported
// sb-*-memory disk is the restore image. Same-node Resume keeps the
// pause catalog (tier 1/2); 2b is also the fallback if that catalog is
// later lost. Only used when ImportedMemoryVol is still the last
// restore (restore-base binding equals the pause id). Rollback restamps
// restore-base and opaque resume clears it; both leave a stale import that
// must not become a pagemap_anon dest.
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
	baseMemoryObject, baseErr := resolveBaseMemoryObjectFn(ctx, cb, backend)
	if baseErr == nil {
		memoryObject, err := commitMemoryFromBaseFor(ctx, backend, baseMemoryObject, templateID, memorySizeBytes)
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
	// captured by the snapshot id stored in the restore-base binding —
	// which makes its bitmap consistent with this base.
	restoreBase, restoreErr := resolveRestoreBaseMemoryObjectFn(ctx, cb, backend)
	if restoreErr == nil {
		memoryObject, err := commitMemoryFromBaseFor(ctx, backend, restoreBase, templateID, memorySizeBytes)
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

	// ─── Tier 2b: S3 own volume when the pause catalog is gone ────────
	// Cross-node Resume imports onto sb-*-memory and never writes a
	// local pause package. Same-node catalog miss uses the same volume
	// only while it is still the last restore (see
	// importedMemoryIsCurrentRestore).
	if live, liveErr := importedSandboxMemoryForCommit(ctx, cb, backend); liveErr != nil {
		stepLog.Warnf("memory artifact: own volume lookup failed (%v); falling back to full snapshot", liveErr)
	} else if live != nil {
		memoryObject, err := commitMemoryFromBaseFor(ctx, backend, live, templateID, memorySizeBytes)
		if err == nil {
			stepLog.Warnf("memory artifact: catalog bases unavailable (%v; %v); "+
				"falling back to incremental(pagemap_anon) over own volume %s/%s -> %s",
				baseErr, restoreErr, live.Name, live.Kind, memoryObject.Name)
			return memoryObject, snapshotTypeIncremental, nil
		}
		stepLog.Warnf("memory artifact: own volume %s clone failed (%v); falling back to full snapshot",
			live.Name, err)
	}

	// ─── Tier 3: full + fresh empty volume ────────────────────────────
	stepLog.Warnf("memory artifact: both previous-snapshot base (%v) and last-restore base (%v) "+
		"unavailable; falling back to full snapshot", baseErr, restoreErr)
	memoryObject, err := createMemoryVolumeFor(ctx, backend, templateID, memorySizeBytes)
	if err != nil {
		return nil, "", err
	}
	stepLog.Infof("memory artifact: created empty memory volume %s/%s, snapshot type=%s",
		memoryObject.Name, memoryObject.Kind, snapshotTypeFull)
	return memoryObject, snapshotTypeFull, nil
}

func importedSandboxMemoryForCommit(
	ctx context.Context,
	cb *cubeboxstore.CubeBox,
	backend string,
) (*storage.CowSnapshotObject, error) {
	sandboxID := ""
	if cb != nil {
		sandboxID = strings.TrimSpace(cb.ID)
		if sandboxID == "" {
			sandboxID = strings.TrimSpace(cb.SandboxID)
		}
	}
	live, err := getImportedSandboxMemoryFor(ctx, backend, sandboxID)
	if err != nil {
		return nil, err
	}
	if !shouldUseImportedMemoryForCommit(cb, live) {
		return nil, nil
	}
	return live, nil
}

// importedMemoryIsCurrentRestore is the Tier 2b gate. ImportedMemoryVol
// is minted at Create/Resume (same-node clone or cross-node import).
// Rollback restamps the restore base onto the rollback target and
// leaves the import pointing at the pre-rollback disk. Opaque resume
// clears the restore base and also leaves the vol stale. Incremental
// onto that disk would drop pages that landed in the new restore image.
//
// Require the Cubelet-stamped pause binding, and require
// restore-base == pause id: that is the Resume path this fallback is for.
// FromSnap without a pause binding stays on catalog / full.
func importedMemoryIsCurrentRestore(cb *cubeboxstore.CubeBox) bool {
	if cb == nil {
		return false
	}
	restoreID := cb.RestoreBaseID()
	if restoreID == "" {
		return false
	}
	pauseID := cubeBoxLabel(cb, constants.MasterAnnotationPauseSnapshotID)
	if pauseID == "" {
		return false
	}
	return restoreID == pauseID
}

// importedMemoryWasLivePauseBase is the Pause-side gate. Pause stamps the new
// snapshot id before preparing the artifact, so comparing against the current
// pause binding would always fail. Instead it asks whether the imported/cloned
// disk is still the image this VM was restored from.
//
// Two shapes mint that disk:
//   - pause resume: the source is a pause package, and the live pause id is the
//     restore base until this Pause replaces it;
//   - cross-node FromSnap: the source is the customer snapshot itself and no
//     pause package exists yet, so the disk is the live base only while the VM
//     still sits on its launch ancestor (and has never been anywhere else).
//
// Restart and opaque resume leave the disk behind but replace guest memory;
// both have an empty restoreID, so every arm rejects them.
//
// The FromSnap arm additionally requires that no rollback followed the create.
// A rollback to the launch ancestor makes restoreID == ancestorID again, and
// the stale import is only released best-effort (releaseImportedMemoryAfterRollback
// returns nil on a metadata read error), so the ids alone cannot prove the disk
// still describes this guest. Refusing costs one full capture; getting it wrong
// would silently drop every page the rollback target brought in.
//
// Takes the ids the caller already resolved so the own-volume decision reads
// the lineage once, next to the ancestor lookup that uses the same values.
func importedMemoryWasLivePauseBase(restoreID, priorPauseID, ancestorID string, rolledBack bool) bool {
	restoreID = strings.TrimSpace(restoreID)
	if restoreID == "" {
		return false
	}
	// A rollback replaces guest memory from a different image, and the import is
	// only released best-effort. Nothing about the ids proves the disk survived
	// intact, so refuse both arms rather than reason about which one matched.
	if rolledBack {
		return false
	}
	if priorPauseID = strings.TrimSpace(priorPauseID); priorPauseID != "" {
		return restoreID == priorPauseID
	}
	return restoreID == strings.TrimSpace(ancestorID)
}

// sandboxRolledBackSinceCreate reports whether a rollback happened since this
// VM's current incarnation was created.
//
// Scope matters here. The guest metrics epoch is reset to fresh_create by every
// successful Create, and pause resume is a Create: it rebuilds the sandbox row
// and re-mints the sandbox-private memory disk from the pause package
// (prefetchRestoreMemoryVolURL -> CloneSandboxMemory), so the epoch's window and
// the imported disk's lifetime start together. Within that window the reason
// only ever moves away from rollback by another rollback.
//
// A missing epoch (non-cubecow, or a row predating the field) means "not known
// to be rolled back", which matches master's behaviour for the same rows.
func sandboxRolledBackSinceCreate(cb *cubeboxstore.CubeBox) bool {
	epoch := cb.GuestMetricsEpochCopy()
	return epoch != nil && epoch.Reason == cubeboxstore.GuestMetricsEpochRollback
}

// shouldUseImportedMemoryForCommit is the testable Tier 2b decision:
// a non-empty imported volume plus importedMemoryIsCurrentRestore.
func shouldUseImportedMemoryForCommit(cb *cubeboxstore.CubeBox, live *storage.CowSnapshotObject) bool {
	if live == nil || strings.TrimSpace(live.Name) == "" {
		return false
	}
	return importedMemoryIsCurrentRestore(cb)
}
