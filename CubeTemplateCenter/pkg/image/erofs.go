// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// BuildErofs produces an EROFS rootfs image from a prepared source image,
// mirroring BuildExt4's structure so the surrounding build pipeline (envd +
// CubeEgress CA baking, artifact store layout, atomic publish, sha256
// computation) stays identical across formats.
//
// Unlike ext4, EROFS is built in a single pass by `mkfs.erofs` from a source
// directory — there is no loop-mount streaming path (Phase 2 /
// createExt4ImageStreaming is ext4-only). createErofsImage runs mkfs.erofs and
// then pads the result to a 2 MiB boundary; see its doc for the two virtio-pmem
// constraints (no compression, 2 MiB size alignment) that shape the command.
//
// The returned BuildResult reuses the Ext4Path field name on purpose: the BUILT
// callback payload and the CubeMaster rootfs_artifact columns are still named
// ext4_* today (the erofs renaming is a later step), so the caller in
// runBuildLocked and reportArtifactBuilt treat both formats through the same
// fields. Only the file suffix on disk differs (.erofs vs .ext4).
func BuildErofs(ctx context.Context, source *PreparedSource, opts BuildOptions) (BuildResult, error) {
	workDir := filepath.Join(ArtifactWorkRootDir(), opts.ArtifactID)
	storeDir, err := ResolveArtifactStoreDir(ctx, opts.ArtifactID)
	if err != nil {
		return BuildResult{}, err
	}
	storeRootfsDir := filepath.Join(storeDir, "rootfs")
	erofsPath := filepath.Join(storeDir, opts.ArtifactID+".erofs")
	// Build into a temp sibling and atomically rename into place, exactly as
	// BuildExt4 does: the artifact is only visible under its final name once it
	// is complete, so reuse and crash-recovery never trust a half-written file.
	tmpErofsPath := erofsPath + ".tmp." + strconv.Itoa(os.Getpid())
	keepStoreDir := false

	// Publish the in-progress marker before anything is written under storeDir
	// so a concurrent cleanup in another process cannot RemoveAll it mid-build.
	releaseMarker, err := MarkArtifactBuildInProgress(storeDir)
	if err != nil {
		log.G(ctx).Warnf("cannot mark artifact build in progress for %s: %v", storeDir, err)
	}
	defer releaseMarker()

	defer func() {
		if workDir != "" {
			if err := os.RemoveAll(workDir); err != nil {
				log.G(ctx).Warnf("cleanup workDir %s failed: %v", workDir, err)
			}
		}
		if !keepStoreDir {
			if storeDir != "" {
				if err := os.RemoveAll(storeDir); err != nil {
					log.G(ctx).Warnf("cleanup storeDir %s failed: %v", storeDir, err)
				}
			}
		} else {
			if storeRootfsDir != "" {
				if err := os.RemoveAll(storeRootfsDir); err != nil {
					log.G(ctx).Warnf("cleanup storeRootfsDir %s failed: %v", storeRootfsDir, err)
				}
			}
		}
	}()

	// EROFS does not need a precise pre-size (mkfs.erofs sizes the output from
	// the source tree), but the disk-space guard still protects against filling
	// the artifact filesystem. Reuse the same estimate the ext4 path uses.
	estimatedSizeBytes, err := estimateImageSizeFromInspect(ctx, source)
	if err != nil {
		log.G(ctx).Warnf("cannot estimate image size for disk-space check, skipping: %v", err)
	} else if estimatedSizeBytes > 0 {
		if err := checkDiskSpace(ctx, storeDir, estimatedSizeBytes); err != nil {
			return BuildResult{}, err
		}
	}

	// Export the image rootfs to the artifact store, then run the post-export
	// callback (envd + CubeEgress CA baking) on the exported tree before
	// mkfs.erofs packs it — identical to BuildExt4's Phase 1 ordering.
	if isLocalFastFS(storeDir) {
		if err := exportImageRootfs(ctx, source, storeRootfsDir); err != nil {
			return BuildResult{}, err
		}
	} else {
		rootfsDir := filepath.Join(workDir, "rootfs")
		if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
			return BuildResult{}, err
		}
		if err := exportImageRootfs(ctx, source, rootfsDir); err != nil {
			return BuildResult{}, err
		}
		if err := relocateRootfsToArtifactStore(ctx, rootfsDir, storeRootfsDir); err != nil {
			return BuildResult{}, err
		}
	}

	if workDir != "" {
		if err := os.RemoveAll(workDir); err != nil {
			log.G(ctx).Warnf("cleanup workDir %s failed: %v", workDir, err)
		}
	}

	if opts.PostRootfsExport != nil {
		if err := opts.PostRootfsExport(ctx, storeRootfsDir); err != nil {
			return BuildResult{}, err
		}
	}

	if err := createErofsImage(ctx, storeRootfsDir, tmpErofsPath); err != nil {
		_ = os.Remove(tmpErofsPath)
		return BuildResult{}, err
	}
	if err := os.Rename(tmpErofsPath, erofsPath); err != nil { // NOCC:Path Traversal()
		_ = os.Remove(tmpErofsPath)
		return BuildResult{}, fmt.Errorf("publish erofs artifact failed: %w", err)
	}
	shaValue, sizeBytes, err := computeFileSHA256(erofsPath)
	if err != nil {
		return BuildResult{}, err
	}
	keepStoreDir = true
	return BuildResult{Ext4Path: erofsPath, SHA256: shaValue, SizeBytes: sizeBytes}, nil
}

// createErofsImage runs mkfs.erofs to pack rootfsDir into an EROFS image at
// erofsPath, then pads the file up to a virtio-pmem-compatible size.
//
// mkfs.erofs takes the output image first, then the source directory:
// `mkfs.erofs [options] DESTINATION SOURCE`.
//
// Two hard constraints shape the command, both driven by how the microVM
// consumes the artifact as a virtio-pmem backing file:
//
//  1. NO compression. We deliberately do NOT pass -z (nor -C / -E frag*).
//     mkfs.erofs builds a plain (uncompressed) image by default, which keeps a
//     fixed block layout the virtio-pmem DAX mount requires: DAX maps host pages
//     directly into the guest, so every block must sit at a stable file offset.
//     A compressed EROFS image stores variable-length extents that DAX cannot
//     map, so a compressed rootfs would fail the in-guest DAX mount.
//
//  2. 2 MiB size alignment. mkfs.erofs sizes the image from the source tree, so
//     the output is almost never a multiple of 2 MiB. The microVM boots the
//     rootfs as a virtio-pmem device whose backing-file size MUST be a multiple
//     of 2 MiB, else the host device manager rejects it ("PmemSizeNotAligned").
//     We round the apparent file size UP with truncate (the same approach the
//     ext4 streaming path uses after resize2fs -M); the padding is unused
//     device space at the tail and never cuts into live data.
func createErofsImage(ctx context.Context, rootfsDir, erofsPath string) error {
	// No -z: plain uncompressed EROFS, required for virtio-pmem DAX mount.
	if err := runCommand(ctx, "", "mkfs.erofs", erofsPath, rootfsDir); err != nil {
		return fmt.Errorf("mkfs.erofs failed: %w", err)
	}

	// Pad the image up to a 2 MiB boundary so virtio-pmem accepts it. Round UP
	// only; the extra bytes are beyond the EROFS superblock-reported length.
	const pmemAlignmentBytes = int64(2 * 1024 * 1024)
	fi, err := os.Stat(erofsPath) // NOCC:Path Traversal()
	if err != nil {
		return fmt.Errorf("stat erofs image for pmem alignment: %w", err)
	}
	alignedSize := alignUp(fi.Size(), pmemAlignmentBytes)
	if alignedSize != fi.Size() {
		if err := runCommand(ctx, "", "truncate", "-s", strconv.FormatInt(alignedSize, 10), erofsPath); err != nil {
			return fmt.Errorf("truncate erofs image to pmem-aligned size %d: %w", alignedSize, err)
		}
	}
	return nil
}

// EnsureErofsBuildPreflight asserts that mkfs.erofs is installed. It is called
// on the erofs build path (via build.ensureBuildPreflight, after the shared
// EnsureArtifactBuildPreflight) so a host without erofs-utils fails fast at the
// PULLING preflight phase instead of deep inside BUILDING_EROFS, after the image
// is pulled and the rootfs exported.
func EnsureErofsBuildPreflight(ctx context.Context) error {
	if _, err := executableLookPath("mkfs.erofs"); err != nil {
		return fmt.Errorf("required command %q is not available: %w", "mkfs.erofs", err)
	}
	return nil
}
