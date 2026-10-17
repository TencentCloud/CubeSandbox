// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

// sandboxMemoryMint is the XFS pause-resume background clone that gives a
// resumed sandbox its own memory volume (sb-<id>-memory), so the next pause
// can promote that volume into the pause snapshot by rename instead of a
// reflink clone. The persisted sandbox record is the state: once the clone
// completes it names the snapshot cloned from and the directory the promote
// files under, and pause promotes only while that snapshot still is the
// last restore — anything else (rollback, newer resume, clone in flight)
// stays on the legacy clone ladder. A restart keeps record and volume, so
// the next pause still promotes.

// mintTimeout bounds one background clone; the deepest measured chain takes
// well under a second, so this only trips on a broken filesystem.
const mintTimeout = 10 * time.Minute

// mintSerials serializes the background clones of one sandbox: every mint
// of a sandbox clones onto the same own-volume name, so overlapping clones
// must not resolve each other's half-written files.
var mintSerials = struct {
	sync.Mutex
	m map[string]*sync.Mutex
}{m: make(map[string]*sync.Mutex)}

func mintSerialFor(sandboxID string) *sync.Mutex {
	mintSerials.Lock()
	defer mintSerials.Unlock()
	mu, ok := mintSerials.m[sandboxID]
	if !ok {
		mu = &sync.Mutex{}
		mintSerials.m[sandboxID] = mu
	}
	return mu
}

// MintSandboxMemoryAfterResume starts the background clone for a same-node
// XFS pause resume. Call it only after Create returned success; non-XFS
// backends and cross-node restores keep their existing own-volume flow.
func MintSandboxMemoryAfterResume(ctx context.Context, backend, sandboxID string, annotations map[string]string) {
	if !isXFSCleanupBackend(backend) || strings.TrimSpace(sandboxID) == "" {
		return
	}
	if CrossNodeSandboxImport(annotations) != nil {
		return
	}
	sourceVol, sourceKind, err := resolveMintSourceVolume(ctx, backend, annotations)
	if err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s resolve source: %v", sandboxID, err)
		return
	}
	if sourceVol == "" {
		return
	}
	devPath, err := ResolveObjectPathFor(ctx, backend, sourceVol, sourceKind)
	if err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s resolve path of %s: %v", sandboxID, sourceVol, err)
		return
	}
	originVol := filepath.Base(filepath.Dir(devPath))
	if originVol == "." {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s derived empty origin from %s", sandboxID, devPath)
		return
	}

	// Whatever the record still names belongs to a previous restore. The
	// clone below rewrites it only after it completed, so until then pause
	// must not treat it as this resume's base.
	if err := clearMintedOwnMemory(ctx, sandboxID); err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s clear stale mint: %v", sandboxID, err)
		return
	}

	mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintTimeout)
	sourceID := mintSourceSnapshotID(annotations)
	go func() {
		defer cancel()
		mu := mintSerialFor(sandboxID)
		mu.Lock()
		defer mu.Unlock()
		runSandboxMemoryMint(mintCtx, backend, sandboxID, sourceVol, sourceID, originVol)
	}()
}

// runSandboxMemoryMint clones the restore image onto the sandbox's own
// volume and publishes the mint on the record. A failure cleans up after
// itself; pause only ever sees a complete record or none.
func runSandboxMemoryMint(ctx context.Context, backend, sandboxID, sourceVol, sourceID, originVol string) {
	ownVol := SandboxMemoryName(sandboxID)
	if err := cloneSandboxOwnVolume(ctx, backend, sandboxID, sourceVol); err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s clone %s -> %s failed: %v",
			sandboxID, sourceVol, ownVol, err)
		bestEffortDeleteOwnMemory(ctx, backend, ownVol)
		return
	}
	if err := recordSandboxOwnMemory(ctx, sandboxID, ownVol, sourceID, originVol); err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s record own volume: %v", sandboxID, err)
		bestEffortDeleteOwnMemory(ctx, backend, ownVol)
		return
	}
	log.G(ctx).Infof("sandbox memory mint: sandbox %s own volume %s ready (source %s)",
		sandboxID, ownVol, sourceVol)
}

// cloneSandboxOwnVolume drops any leftover own volume and clones this
// resume's restore image onto it. The delete is strict: resolving a stale
// clone would silently hand pause a base that no longer matches the VM.
func cloneSandboxOwnVolume(ctx context.Context, backend, sandboxID, sourceVol string) error {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return err
	}
	ownVol := SandboxMemoryName(sandboxID)
	_ = store.DeactivateByKind(ctx, ownVol, cowKindVolume)
	if err := store.DeleteByKind(ctx, ownVol, cowKindVolume); err != nil {
		return fmt.Errorf("drop stale own volume %s: %w", ownVol, err)
	}
	start := time.Now()
	cloned, err := store.CloneSandboxMemory(ctx, sandboxID, sourceVol)
	if err != nil {
		return fmt.Errorf("clone %s -> %s: %w", sourceVol, ownVol, err)
	}
	if cloned == nil || strings.TrimSpace(cloned.FilePath) == "" {
		return fmt.Errorf("clone %s -> %s: empty device path", sourceVol, ownVol)
	}
	log.G(ctx).Infof("sandbox memory mint: cloned %s -> %s in %dms",
		sourceVol, ownVol, time.Since(start).Milliseconds())
	return nil
}

// PromoteSandboxMemoryOwnForPause consumes the pre-cloned own memory volume
// as this pause's memory artifact: one rename under the mint's origin
// directory. ok=false keeps the caller on the legacy clone ladder; detail
// is empty when there was nothing to promote.
func PromoteSandboxMemoryOwnForPause(ctx context.Context, backend, sandboxID, snapshotID, restoreBaseID string) (*CowSnapshotObject, string, bool) {
	if !isXFSCleanupBackend(backend) || strings.TrimSpace(sandboxID) == "" {
		return nil, "", false
	}
	ownVol, originVol, stale, ok := mintedOwnMemoryFor(ctx, sandboxID, restoreBaseID)
	if !ok {
		if stale {
			// The record names a base this pause is not relative to, so it
			// is stale for every consumer of the record; drop it before the
			// clone ladder can clone the old base.
			if err := clearMintedOwnMemory(ctx, sandboxID); err != nil {
				log.G(ctx).Warnf("sandbox memory mint: sandbox %s clear stale mint: %v", sandboxID, err)
			}
		}
		return nil, "", false
	}
	start := time.Now()
	memoryObject, err := PromoteMemoryVolumeFor(ctx, backend,
		&CowSnapshotObject{Name: ownVol, Kind: cowKindVolume}, snapshotID, originVol)
	if err != nil {
		return nil, fmt.Sprintf("promote own volume %s: %v", ownVol, err), false
	}
	// The own volume name is gone; drop the recorded pointer so the
	// release paths do not chase a renamed object.
	if err := clearMintedOwnMemory(ctx, sandboxID); err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s clear consumed mint: %v", sandboxID, err)
	}
	return memoryObject, fmt.Sprintf("own %s -> %s (origin %s) in %dus",
		ownVol, memoryObject.Name, originVol, time.Since(start).Microseconds()), true
}

// mintedOwnMemoryFor returns the recorded own volume and its promote
// origin, but only while the record still names the snapshot this pause is
// relative to. A mint record naming anything else is stale=true: no promote
// from it, and the caller must drop it before the clone ladder reads it.
func mintedOwnMemoryFor(ctx context.Context, sandboxID, restoreBaseID string) (ownVol, originVol string, stale, ok bool) {
	if localStorage == nil || localStorage.db == nil {
		return "", "", false, false
	}
	info, err := localStorage.readBackendFileInfo(ctx, sandboxID)
	if err != nil || info == nil {
		return "", "", false, false
	}
	ownVol = strings.TrimSpace(info.ImportedMemoryVol)
	originVol = strings.TrimSpace(info.MintedMemoryOrigin)
	if ownVol == "" || originVol == "" || info.MintedMemorySource == "" {
		return "", "", false, false
	}
	if strings.TrimSpace(info.MintedMemorySource) != strings.TrimSpace(restoreBaseID) {
		return "", "", restoreBaseID != "", false
	}
	return ownVol, originVol, false, true
}

// mintSourceSnapshotID returns the logical snapshot id this resume's
// restore image is bound to; empty means the sandbox is not restored from
// a memory snapshot.
func mintSourceSnapshotID(annotations map[string]string) string {
	logicalID := strings.TrimSpace(annotations[constants.MasterAnnotationRuntimeSnapshotID])
	if logicalID == "" {
		logicalID = strings.TrimSpace(annotations[constants.MasterAnnotationAppSnapshotTemplateID])
	}
	return logicalID
}

// resolveMintSourceVolume resolves the memory volume this resume booted
// from, mirroring prefetchRestoreMemoryVolURL's catalog lookup. Empty means
// the sandbox is not bound to a memory snapshot.
func resolveMintSourceVolume(ctx context.Context, backend string, annotations map[string]string) (string, string, error) {
	logicalID := mintSourceSnapshotID(annotations)
	if logicalID == "" {
		return "", "", nil
	}
	entry, err := GetLocalSnapshotFor(ctx, backend, logicalID)
	if err != nil {
		return "", "", err
	}
	if entry == nil {
		return "", "", nil
	}
	volumeName := strings.TrimSpace(entry.MemoryVol)
	if volumeName == "" {
		return "", "", nil
	}
	kind := strings.TrimSpace(entry.MemoryKind)
	if kind == "" {
		kind = cowKindVolume
	}
	normalized, err := normalizeCowKind(kind)
	if err != nil {
		return "", "", err
	}
	return volumeName, normalized, nil
}

// recordSandboxOwnMemory persists the mint on the sandbox record: destroy
// and rollback clean the volume through the existing release paths, and
// pause promotes it while the source still is the last restore.
func recordSandboxOwnMemory(ctx context.Context, sandboxID, ownVol, sourceID, originVol string) error {
	if localStorage == nil || localStorage.db == nil {
		return fmt.Errorf("storage is not initialized")
	}
	info, err := localStorage.readBackendFileInfo(ctx, sandboxID)
	if err != nil {
		return err
	}
	if info == nil {
		return fmt.Errorf("sandbox record %s is empty", sandboxID)
	}
	info.ImportedMemoryVol = ownVol
	info.MintedMemorySource = sourceID
	info.MintedMemoryOrigin = originVol
	info.UpdateAt = time.Now()
	return localStorage.writeBackendFileInfo(ctx, sandboxID, info)
}

// clearMintedOwnMemory drops the mint from the record: a clone in flight
// (or a promote just consumed) must not hand a later pause a stale base,
// and the release paths must not chase a half-written or renamed volume.
func clearMintedOwnMemory(ctx context.Context, sandboxID string) error {
	if localStorage == nil || localStorage.db == nil {
		return fmt.Errorf("storage is not initialized")
	}
	info, err := localStorage.readBackendFileInfo(ctx, sandboxID)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			// A missing record has nothing to clear.
			return nil
		}
		return err
	}
	if info == nil {
		return nil
	}
	if info.ImportedMemoryVol == "" && info.MintedMemorySource == "" && info.MintedMemoryOrigin == "" {
		return nil
	}
	info.ImportedMemoryVol = ""
	info.MintedMemorySource = ""
	info.MintedMemoryOrigin = ""
	info.UpdateAt = time.Now()
	return localStorage.writeBackendFileInfo(ctx, sandboxID, info)
}

func bestEffortDeleteOwnMemory(ctx context.Context, backend, ownVol string) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return
	}
	_ = store.DeactivateByKind(ctx, ownVol, cowKindVolume)
	if err := store.DeleteByKind(ctx, ownVol, cowKindVolume); err != nil {
		log.G(ctx).Warnf("sandbox memory mint: delete own volume %s: %v", ownVol, err)
	}
}
