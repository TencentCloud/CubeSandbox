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
	var unmountOnce sync.Once
	var detachOnce sync.Once
	cleanup := func() {
		unmountOnce.Do(func() {
			_ = runCommand(cleanupCtx, "", "umount", "--", mountPoint)
		})
		if err := os.RemoveAll(mountPoint); err != nil {
			log.G(ctx).Warnf("cleanup mount point %s failed: %v", mountPoint, err)
		}
	}
	defer cleanup()

	// Allocate a free loop device and mount.
	loopOut, err := exec.CommandContext(ctx, "losetup", "--find", "--show", ext4Path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("losetup --find --show %s failed: %w: %s", ext4Path, err, string(loopOut))
	}
	loopDevice := strings.TrimSpace(string(loopOut))
	detachLoop := func() {
		detachOnce.Do(func() {
			_ = runCommand(cleanupCtx, "", "losetup", "--detach", "--", loopDevice)
		})
	}
	defer detachLoop()

	if err := runCommand(ctx, "", "mount", "-o", "nosuid,noexec,nodev,noatime", "--", loopDevice, mountPoint); err != nil {
		detachLoop() // explicit detach on mount failure (defer will be a no-op via sync.Once)
		return fmt.Errorf("mount loop device %s: %w", loopDevice, err)
	}

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

	// 5. Unmount (via cleanup).
	cleanup()

	// 6. Shrink the ext4 filesystem to minimum size (best-effort).
	if err := runCommand(cleanupCtx, "", "resize2fs", "-M", ext4Path); err != nil {
		log.G(ctx).Warnf("resize2fs -M failed (best-effort, using original size): %v", err)
	}

	// 7. Truncate to actual block size.
	finalSize := getFileBlockSize(ext4Path)
	if finalSize > 0 && finalSize < estimatedSizeBytes {
		if err := runCommand(cleanupCtx, "", "truncate", "-s", strconv.FormatInt(finalSize, 10), ext4Path); err != nil {
			log.G(ctx).Warnf("truncate to final size %d failed: %v", finalSize, err)
		}
	}

	return nil
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
