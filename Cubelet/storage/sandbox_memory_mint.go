// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

// The XFS pause-resume mint clones the restore image onto the sandbox's own
// memory volume so the next pause can promote it by rename. A dotfile written
// after the clone and removed before the volume vouches for a complete clone.

const mintMarkerName = ".mint-source"

// mintSerialWait bounds how long a destroy waits for an in-flight clone; the
// clone itself takes well under a second.
const mintSerialWait = 30 * time.Second

// SandboxMemoryMintSupported reports whether the backend runs the pause-resume
// memory mint, and therefore whether pause has a Tier 0 to promote.
func SandboxMemoryMintSupported(backend string) bool {
	return isXFSCleanupBackend(backend)
}

// mintSerials serializes one sandbox's clones and its destroy: the clone
// resolves an already-existing own-volume name instead of cloning it, and a
// destroy must not release the volume while a clone is publishing it.
var mintSerials = utils.NewResourceLocks()

// MintSandboxMemoryAfterResume clones the restore image of a same-node XFS
// pause resume onto the sandbox's own volume. Call it after Create returned
// success; other backends keep their existing own-volume flow.
func MintSandboxMemoryAfterResume(ctx context.Context, backend, sandboxID, snapshotID string) {
	if !isXFSCleanupBackend(backend) || sandboxID == "" || snapshotID == "" {
		return
	}
	mintCtx := context.WithoutCancel(ctx)
	go func() {
		unlock := mintSerials.Lock(sandboxID)
		defer unlock()
		sourceVol, err := resolveMintSourceVolume(mintCtx, backend, snapshotID)
		if err != nil {
			log.G(mintCtx).Warnf("sandbox memory mint: sandbox %s resolve source: %v", sandboxID, err)
			return
		}
		if sourceVol == "" {
			return
		}
		runSandboxMemoryMint(mintCtx, backend, sandboxID, sourceVol, snapshotID)
	}()
}

// runSandboxMemoryMint replaces any leftover own volume with a clone of the
// source, then records the source. Failure cleans up; pause sees only a
// complete volume with a marker.
func runSandboxMemoryMint(ctx context.Context, backend, sandboxID, sourceVol, sourceID string) {
	ownVol := SandboxMemoryName(sandboxID)
	store, err := requireCowStoreFor(backend)
	if err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s: %v", sandboxID, err)
		return
	}
	// The marker goes before the volume: cubecow's delete unlinks the main
	// file and then rmdirs a directory it expects to be empty.
	removeMintMarker(ctx, backend, ownVol)
	if err := store.DeleteByKind(ctx, ownVol, cowKindVolume); err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s drop stale own volume: %v", sandboxID, err)
		return
	}
	start := time.Now()
	if _, err := store.CloneSandboxMemory(ctx, sandboxID, sourceVol); err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s clone %s -> %s failed: %v", sandboxID, sourceVol, ownVol, err)
		dropOwnMemory(ctx, backend, ownVol)
		return
	}
	// A destroy that ran before this mint took the serial left no record;
	// publishing the marker now would leave a volume nothing ever releases.
	if info, err := localStorage.readBackendFileInfoRaw(ctx, sandboxID); err != nil || info == nil {
		dropOwnMemory(ctx, backend, ownVol)
		return
	}
	if err := writeMintMarker(ctx, backend, ownVol, sourceID); err != nil {
		log.G(ctx).Warnf("sandbox memory mint: sandbox %s write marker: %v", sandboxID, err)
		dropOwnMemory(ctx, backend, ownVol)
		return
	}
	log.G(ctx).Infof("sandbox memory mint: sandbox %s own volume %s ready (source %s) in %dms",
		sandboxID, ownVol, sourceVol, time.Since(start).Milliseconds())
}

// PromoteSandboxMemoryOwnForPause consumes the pre-cloned own memory volume as
// this pause's artifact: one rename of the volume's file into originVol. ok=false
// keeps the caller on the legacy ladder; detail explains a refused volume.
//
// It deliberately does not take the mint serial: holding it would put a
// background clone on the pause path. A clone racing this call either has not
// published its marker (ladder) or already finished (marker matches).
func PromoteSandboxMemoryOwnForPause(ctx context.Context, backend, sandboxID, snapshotID, restoreBaseID string, base *CowSnapshotObject) (*CowSnapshotObject, string, bool) {
	if !isXFSCleanupBackend(backend) || sandboxID == "" || base == nil || strings.TrimSpace(base.DevPath) == "" {
		return nil, "", false
	}
	originVol := filepath.Base(filepath.Dir(base.DevPath))
	ownVol := SandboxMemoryName(sandboxID)
	// TryLock keeps the pause off a background clone's critical path: if a mint
	// is publishing right now, take the legacy ladder instead of racing it.
	unlock, locked := mintSerials.TryLock(sandboxID)
	if !locked {
		return nil, "", false
	}
	defer unlock()
	marker, source := readMintMarker(ctx, backend, ownVol)
	if marker == "" {
		return nil, "", false
	}
	if source != strings.TrimSpace(restoreBaseID) {
		// Minted for a base this pause is not relative to: reclaim it instead
		// of promoting the wrong base.
		dropOwnMemory(ctx, backend, ownVol)
		return nil, fmt.Sprintf("own volume %s is not minted from restore base %s", ownVol, restoreBaseID), false
	}
	start := time.Now()
	memoryObject, err := promoteOwnVolume(ctx, backend, ownVol, snapshotID, originVol)
	if err != nil {
		return nil, fmt.Sprintf("promote own volume %s: %v", ownVol, err), false
	}
	// The rename consumed the volume name; the marker and the emptied
	// directory are all that remain.
	_ = os.Remove(marker)
	_ = os.Remove(filepath.Dir(marker))
	return memoryObject, fmt.Sprintf("own %s -> %s (origin %s) in %dus",
		ownVol, memoryObject.Name, originVol, time.Since(start).Microseconds()), true
}

// promoteOwnVolume renames the own volume's file into the pause package of
// snapshotID under originVol.
func promoteOwnVolume(ctx context.Context, backend, ownVol, snapshotID, originVol string) (*CowSnapshotObject, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	volume, err := store.PromoteVolumeToSnapshot(ctx, ownVol, cowTemplateMemoryName(snapshotID), originVol)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObject(ctx, store, volume)
}

// resolveMintSourceVolume resolves the memory volume a resume from snapshotID
// boots from, mirroring prefetchRestoreMemoryVolURL's catalog lookup.
func resolveMintSourceVolume(ctx context.Context, backend, snapshotID string) (string, error) {
	entry, err := GetLocalSnapshotFor(ctx, backend, snapshotID)
	if err != nil || entry == nil {
		return "", err
	}
	return strings.TrimSpace(entry.MemoryVol), nil
}

// mintMarkerFile returns the marker path of the own volume; empty when the
// volume does not resolve.
func mintMarkerFile(ctx context.Context, backend, ownVol string) string {
	devPath, err := ResolveObjectPathFor(ctx, backend, ownVol, cowKindVolume)
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(devPath), mintMarkerName)
}

// writeMintMarker records the source snapshot next to the volume. Runs after
// the clone completed, so a marker never vouches for an incomplete file.
func writeMintMarker(ctx context.Context, backend, ownVol, sourceID string) error {
	marker := mintMarkerFile(ctx, backend, ownVol)
	if marker == "" {
		return fmt.Errorf("own volume %s does not resolve", ownVol)
	}
	// Publish atomically: a reader must never observe a truncated source.
	tmp := marker + ".tmp"
	if err := os.WriteFile(tmp, []byte(sourceID), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, marker)
}

// readMintMarker returns the marker path and the recorded source; both empty
// when the volume or its marker is absent.
func readMintMarker(ctx context.Context, backend, ownVol string) (string, string) {
	marker := mintMarkerFile(ctx, backend, ownVol)
	if marker == "" {
		return "", ""
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		return "", ""
	}
	return marker, strings.TrimSpace(string(raw))
}

func removeMintMarker(ctx context.Context, backend, ownVol string) {
	marker := mintMarkerFile(ctx, backend, ownVol)
	if marker == "" {
		return
	}
	_ = os.Remove(marker)
	_ = os.Remove(marker + ".tmp")
}

// dropOwnMemory removes the own volume and its marker; missing objects are a
// no-op.
func dropOwnMemory(ctx context.Context, backend, ownVol string) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return
	}
	removeMintMarker(ctx, backend, ownVol)
	if err := store.DeleteByKind(ctx, ownVol, cowKindVolume); err != nil {
		log.G(ctx).Warnf("drop own memory volume %s: %v", ownVol, err)
	}
}
