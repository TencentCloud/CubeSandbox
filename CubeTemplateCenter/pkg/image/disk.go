// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"bytes"
	"context"
	"fmt"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// dockerInspectSizeMultiplier expands the uncompressed on-disk image size
	// reported by `docker image inspect` to account for rootfs data, ext4
	// overhead, and temporary workspace.
	dockerInspectSizeMultiplier = 4
	// skopeoInspectSizeMultiplier expands the *compressed* layer size reported
	// by `skopeo inspect` (LayersData[].Size). Compressed layers typically
	// decompress ~2-3x; the extra headroom covers ext4 overhead and temporary
	// workspace, mirroring dockerInspectSizeMultiplier on top of decompression.
	skopeoInspectSizeMultiplier = 8
)

func sourceRefForLog(source *PreparedSource) string {
	if source == nil {
		return "<nil>"
	}
	return source.LocalRef
}

// createExt4ImageStreaming uses loop-mount to stream docker export directly into
// an ext4 image, avoiding any intermediate rootfs directory on disk (Phase 2).
// Falls back to Phase 1 when prerequisites are not met.
// estimatedSizeBytes should be obtained from estimateImageSizeFromInspect.
func createExt4ImageStreaming(ctx context.Context, source *PreparedSource, workDir, ext4Path string, estimatedSizeBytes int64, postExport func(context.Context, string) error) error {
	if !canUseLoopMount() {
		return fmt.Errorf("loop mount not available")
	}

	// Ensure the per-artifact store dir exists (Phase-1 creates it lazily via relocate); otherwise truncate ENOENTs → silent fallback.
	// 0o700 (owner-only, matching the mount point below): the finished image is a full container rootfs and must not be world-readable on a shared host.
	if err := os.MkdirAll(filepath.Dir(ext4Path), 0o700); err != nil {
		return fmt.Errorf("create artifact store dir for streaming: %w", err)
	}

	// 2. Create empty ext4 image using the caller-provided size estimate.
	if err := runCommand(ctx, "", "truncate", "-s", strconv.FormatInt(estimatedSizeBytes, 10), ext4Path); err != nil {
		return fmt.Errorf("truncate ext4 image for streaming: %w", err)
	}
	if err := runCommand(ctx, "", "mkfs.ext4", "-F", ext4Path); err != nil {
		return fmt.Errorf("mkfs.ext4 for streaming: %w", err)
	}

	// 3. Mount the ext4 image via loop device.
	mountPoint := filepath.Join(workDir, "ext4-mnt")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}

	// Use context.Background() for cleanup so it runs even after request cancellation.
	cleanupCtx := context.Background()
	var loopDevice string
	var mounted bool
	var mountStuck bool
	var unmountOnce sync.Once
	var detachOnce sync.Once
	detachLoop := func() {
		if loopDevice == "" {
			return
		}
		detachOnce.Do(func() {
			// A lazy umount releases the mount asynchronously, so the detach can fail
			// EBUSY for a short window afterwards; retry briefly. When even the lazy
			// umount failed the mount is still there and every attempt is guaranteed to
			// fail, so don't spend the budget.
			attempts := 5
			if mountStuck {
				attempts = 1
			}
			var err error
			for attempt := 0; attempt < attempts; attempt++ {
				if attempt > 0 {
					time.Sleep(200 * time.Millisecond)
				}
				if err = runCommand(cleanupCtx, "", "losetup", "--detach", "--", loopDevice); err == nil {
					return
				}
			}
			// Be precise about how narrow recovery is from here: reclaim only picks a
			// device up once its backing file has been unlinked — which happens on the
			// build's failure path only — and the mount is gone. On the success path the
			// image stays, so the device stays attached until the host reboots.
			log.G(ctx).Warnf("losetup --detach %s failed; device stays attached, reclaimable later only if its backing file is unlinked and the mount released: %v", loopDevice, err)
		})
	}
	// A single cleanup closure, so the detach always runs *after* the umount:
	// detaching a still-mounted device fails EBUSY and pins the loop device — and,
	// once the backing file is unlinked, its disk space too — until the host
	// reboots. Two separate defers would run in LIFO order and get this backwards.
	cleanup := func() {
		if mounted {
			unmountOnce.Do(func() {
				if err := runCommand(cleanupCtx, "", "umount", "--", mountPoint); err != nil {
					log.G(ctx).Warnf("umount %s failed, retrying lazily: %v", mountPoint, err)
					if lazyErr := runCommand(cleanupCtx, "", "umount", "-l", "--", mountPoint); lazyErr != nil {
						mountStuck = true
						log.G(ctx).Warnf("lazy umount %s also failed, loop device will leak: %v", mountPoint, lazyErr)
					}
				}
			})
		}
		detachLoop()
		if err := os.RemoveAll(mountPoint); err != nil {
			log.G(ctx).Warnf("cleanup mount point %s failed: %v", mountPoint, err)
		}
	}
	defer cleanup()

	// Allocate a free loop device and mount. Capture stdout ONLY for the device
	// path: losetup emits warnings (e.g. "file does not fit into a 512-byte
	// sector") to stderr, and CombinedOutput would fold that warning into the
	// parsed device path — corrupting the loopDevice string so every later mount
	// and detach receives a garbage argument (mount fails "bad option", detach
	// fails → loop device leaks).
	findLoop := func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, "losetup", "--find", "--show", "--", ext4Path)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("losetup --find --show %s failed: %w: %s", ext4Path, err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
	loopOut, err := findLoop()
	if err != nil {
		// Running out of loop devices can be self-inflicted: a detach that could not
		// be completed (e.g. the umount above only succeeded lazily) keeps one pinned
		// for the lifetime of the host, so the fast path would be lost permanently
		// once enough builds have leaked. Reclaim devices whose backing file is
		// already gone and retry before degrading to the much slower phase-1 build.
		// This is best-effort and only meaningful for exhaustion; losetup reports
		// every failure the same way, so other causes (cancellation, permissions)
		// pay one extra `losetup --list` and then fail the retry as they should.
		// It also only frees devices this process managed to attach, i.e. ones whose
		// /dev node exists here — it cannot create the node that LOOP_CTL_GET_FREE
		// returned, so a container whose /dev tmpfs is short of loop nodes and has no
		// orphans to reclaim still degrades to phase-1 (see the deployment note).
		if n := reclaimOrphanLoopDevices(cleanupCtx, artifactStoreDirOf(ext4Path)); n > 0 {
			log.G(ctx).Warnf("loop device allocation failed (%v); reclaimed %d orphaned loop device(s), retrying", err, n)
			retryOut, retryErr := findLoop()
			if retryErr != nil {
				return fmt.Errorf("%w (retry after reclaiming %d device(s) also failed: %v)", err, n, retryErr)
			}
			loopOut = retryOut
		} else {
			return err
		}
	}
	loopDevice = strings.TrimSpace(string(loopOut))

	if err := runCommand(ctx, "", "mount", "-o", "nosuid,noexec,nodev,noatime", "--", loopDevice, mountPoint); err != nil {
		return fmt.Errorf("mount loop device %s: %w", loopDevice, err)
	}
	mounted = true

	// 4. Stream export directly into the mounted ext4.
	if source.ExportMode == ExportModeNative {
		if err := StreamRegistryToDir(ctx, source, mountPoint); err != nil {
			return fmt.Errorf("native streaming to mount point: %w", err)
		}
	} else {
		containerIDBytes, err := dockerOutput(ctx, "", "create", "--", source.LocalRef)
		if err != nil {
			return fmt.Errorf("docker create for streaming: %w", err)
		}
		containerID := strings.TrimSpace(string(containerIDBytes))
		defer func() {
			_ = dockerRun(cleanupCtx, "", "rm", "-f", "--", containerID)
		}()

		if err := pipeExportToDir(ctx, containerID, mountPoint); err != nil {
			return fmt.Errorf("pipe export to mount point: %w", err)
		}
	}

	// 4.5 Execute post-export hook before unmounting.
	if postExport != nil {
		if err := postExport(ctx, mountPoint); err != nil {
			return fmt.Errorf("post-rootfs export hook failed during streaming: %w", err)
		}
	}

	// 5. Unmount and detach (via cleanup).
	cleanup()

	// 6. Shrink the ext4 filesystem to minimum size (best-effort).
	if err := runCommand(cleanupCtx, "", "resize2fs", "-M", ext4Path); err != nil {
		log.G(ctx).Warnf("resize2fs -M failed (best-effort, using original size): %v", err)
	}

	// 7. Round the image file UP to a pmem-compatible size. The microVM boots the
	// rootfs as a virtio-pmem device whose backing-file size MUST be a multiple of
	// 2 MiB, else the host device manager rejects it ("PmemSizeNotAligned"). Phase-1
	// sizing gets this for free (it aligns up to a 256 MiB boundary); the streaming
	// path shrinks with resize2fs -M, which truncates the file to the exact
	// filesystem length (not 2 MiB-aligned). Align that apparent size up here —
	// rounding UP never cuts into live filesystem blocks; the small tail is just
	// unused device space. Use the apparent size (fi.Size(), the fs logical length
	// after resize2fs -M), NOT the physically-allocated block count, which can be
	// smaller than the fs length for a minimized image and would truncate below it.
	const pmemAlignmentBytes = int64(2 * 1024 * 1024)
	fsSize := estimatedSizeBytes
	haveActualSize := false
	if fi, statErr := os.Stat(ext4Path); statErr != nil {
		log.G(ctx).Warnf("stat %s for pmem alignment failed, using estimated size %d (may over-provision guest address space): %v", ext4Path, estimatedSizeBytes, statErr)
	} else if fi.Size() > 0 {
		fsSize = fi.Size()
		haveActualSize = true
		if fi.Size() > estimatedSizeBytes {
			log.G(ctx).Warnf("stat %s reported size %d exceeding estimate %d (unexpected); using the actual size for pmem alignment", ext4Path, fi.Size(), estimatedSizeBytes)
		}
	}
	alignedSize := alignUp(fsSize, pmemAlignmentBytes)
	// Skip the truncate only when the real file size is already exactly aligned. If
	// the stat failed we must still truncate: the file sits at resize2fs -M's length
	// (very likely unaligned) and estimatedSizeBytes is only a fallback target.
	if !haveActualSize || alignedSize != fsSize {
		if err := runCommand(cleanupCtx, "", "truncate", "-s", strconv.FormatInt(alignedSize, 10), ext4Path); err != nil {
			log.G(ctx).Warnf("truncate to pmem-aligned size %d failed: %v", alignedSize, err)
		}
	}

	return nil
}

// artifactStoreDirOf returns the artifact store root for an ext4 image path,
// mirroring the <store>/<artifact>/<artifact>.ext4 layout BuildExt4 constructs
// (including when it falls back to ArtifactFallbackStoreRootDir, so a fallback
// build only ever reclaims fallback-store orphans). A path of another shape just
// yields a prefix that matches nothing in orphanLoopCandidates, i.e. reclaim
// no-ops rather than reaching outside the store. Nested store roots are the one
// case where the scope is wider than the build's own store: the shallower root's
// prefix also covers the deeper one's orphans, still bounded to devices whose
// backing file is already gone.
func artifactStoreDirOf(ext4Path string) string {
	return filepath.Dir(filepath.Dir(ext4Path))
}

// orphanLoopCandidates parses the output of
// `losetup --list --noheadings --raw --output NAME,BACK-FILE` and returns the
// devices whose backing file lives under storeDir and has already been unlinked
// (losetup marks those with a trailing " (deleted)").
//
// Requiring the unlink is what makes the reclaim safe against a build running
// concurrently: the image file is only unlinked once a build has given up on it,
// so a live build's device can never become a candidate — not even in the
// attach→mount window, where the device is not yet in the mount table and the
// kernel's EBUSY refusal would not protect it either.
func orphanLoopCandidates(losetupList, storeDir string) []string {
	if storeDir == "" || storeDir == "/" {
		return nil
	}
	prefix := strings.TrimSuffix(storeDir, "/") + "/"
	var names []string
	for _, line := range strings.Split(losetupList, "\n") {
		name, backing, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || name == "" {
			continue
		}
		backing = strings.TrimSpace(backing)
		if !strings.HasSuffix(backing, " (deleted)") {
			continue
		}
		backing = strings.TrimSpace(strings.TrimSuffix(backing, " (deleted)"))
		if strings.HasPrefix(backing, prefix) {
			names = append(names, name)
		}
	}
	return names
}

// reclaimOrphanLoopDevices detaches loop devices whose backing file under
// storeDir has already been unlinked, i.e. devices no build can still be using.
// Returns how many were freed.
func reclaimOrphanLoopDevices(ctx context.Context, storeDir string) int {
	out, err := exec.CommandContext(ctx, "losetup", "--list", "--noheadings", "--raw",
		"--output", "NAME,BACK-FILE").Output()
	if err != nil {
		log.G(ctx).Warnf("losetup --list for orphan reclaim failed: %v", err)
		return 0
	}
	reclaimed := 0
	for _, name := range orphanLoopCandidates(string(out), storeDir) {
		if err := runCommand(ctx, "", "losetup", "--detach", "--", name); err != nil {
			log.G(ctx).Debugf("orphan loop device %s not reclaimable (likely still in use): %v", name, err)
			continue
		}
		log.G(ctx).Debugf("reclaimed orphaned loop device %s", name)
		reclaimed++
	}
	return reclaimed
}

// pipeExportToDir streams the docker export of a container directly into a target
// directory via tar -xf -.
func pipeExportToDir(ctx context.Context, containerID, destDir string) error {
	exportCmd := exec.CommandContext(ctx, "docker", "export", "--", containerID)
	// --same-owner --numeric-owner preserves the image's original uid/gid.
	// GNU tar restores ownership only with --same-owner (the default for root,
	// set explicitly to be robust); --numeric-owner avoids name lookups
	// against the host's passwd/group. Without this, files owned by non-root
	// uids with restrictive modes (e.g. /home/user uid 1000 mode 0700)
	// collapse to the extracting user and the in-sandbox account loses access
	// to its own files (envd exec EACCES, chromium profile write failures).
	// Mirrors the umoci (no --rootless) fix in export.go.
	tarCmd := exec.CommandContext(ctx, "tar", "--same-owner", "--numeric-owner", "-xf", "-", "-C", destDir)

	pipe, err := exportCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create pipe from docker export to tar: %w", err)
	}
	tarCmd.Stdin = pipe

	// Best-effort: increase pipe buffer to 1 MiB.
	if f, ok := pipe.(*os.File); ok {
		_, _ = unix.FcntlInt(f.Fd(), unix.F_SETPIPE_SZ, 1<<20)
	}

	var exportErrBuf, tarErrBuf bytes.Buffer
	exportCmd.Stderr = &exportErrBuf
	tarCmd.Stderr = &tarErrBuf

	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("start tar extract: %w", err)
	}
	if err := exportCmd.Start(); err != nil {
		tarCmd.Process.Kill()
		_ = tarCmd.Wait()
		return fmt.Errorf("start docker export: %w", err)
	}

	exportWaitErr := exportCmd.Wait()
	tarWaitErr := tarCmd.Wait()

	if exportWaitErr != nil {
		return fmt.Errorf("docker export %s failed: %w (stderr: %s)", containerID, exportWaitErr, exportErrBuf.String())
	}
	if tarWaitErr != nil {
		return fmt.Errorf("extract tar to %s failed: %w (stderr: %s)", destDir, tarWaitErr, tarErrBuf.String())
	}
	return nil
}

// skopeoLayersTotalSize sums the compressed layer blob sizes from a
// `skopeo inspect` result. Returns 0 when no LayersData is present.
func skopeoLayersTotalSize(info skopeoInspectImage) int64 {
	var total int64
	for _, layer := range info.LayersData {
		if layer.Size > 0 {
			total += layer.Size
		}
	}
	return total
}

// estimateImageSizeFromInspect returns an approximate on-disk size for the
// source image, used for the disk-space pre-check and Phase 2 ext4 sizing.
//
// The dockerless path (skopeo+umoci) must not depend on the docker binary, so
// it derives the estimate from the compressed layer sizes captured at prepare
// time via `skopeo inspect`. The docker path keeps using the per-image
// cumulative Size field from `docker image inspect`.
func estimateImageSizeFromInspect(ctx context.Context, source *PreparedSource) (int64, error) {
	if source != nil && (source.ExportMode == ExportModeDockerless || source.ExportMode == ExportModeNative) {
		return estimateImageSizeFromCompressedBytes(source)
	}
	return estimateImageSizeFromDocker(ctx, source)
}

// estimateImageSizeFromCompressedBytes derives the estimate from the compressed layer
// sizes recorded during PrepareSource, avoiding any docker invocation.
func estimateImageSizeFromCompressedBytes(source *PreparedSource) (int64, error) {
	if source == nil || source.CompressedSizeBytes <= 0 {
		return 0, fmt.Errorf("did not find any compressed layer sizes for %s", sourceRefForLog(source))
	}
	return source.CompressedSizeBytes * skopeoInspectSizeMultiplier, nil
}

// estimateImageSizeFromDocker extracts an approximate image size from the
// per-image cumulative Size field in docker inspect output.
func estimateImageSizeFromDocker(ctx context.Context, source *PreparedSource) (int64, error) {
	// Try the per-image cumulative Size field first (fast, single call).
	out, err := dockerOutput(ctx, "", "image", "inspect", "--format", "{{.Size}}", "--", source.LocalRef)
	if err != nil {
		return 0, fmt.Errorf("docker inspect for size estimation failed: %w", err)
	}
	sizeBytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse docker inspect size output %q: %w", strings.TrimSpace(string(out)), err)
	}
	if sizeBytes <= 0 {
		return 0, fmt.Errorf("docker image inspect reported zero or negative size (%d bytes)", sizeBytes)
	}
	// RootFS data plus writable layer overhead typically expands 3-4x from the
	// on-disk layer size. Use a conservative multiplier for the disk-space
	// pre-check.
	return sizeBytes * dockerInspectSizeMultiplier, nil
}
