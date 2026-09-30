// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// virtio-pmem requires the backing file size to be a multiple of 2 MiB.
const pmemAlignmentBytes = int64(2 * 1024 * 1024)

func createExt4Image(ctx context.Context, rootfsDir, ext4Path string) error {
	sizeBytes, fileCount, err := directorySizeAndFileCount(rootfsDir)
	if err != nil {
		return err
	}

	const mib = int64(1024 * 1024)

	// Fixed overhead (default 256 MiB, configurable).
	fixedOverhead := ext4FixedOverheadMiB() * mib

	// Percentage overhead: configurable percentage of the data size (default 10%).
	percentageOverhead := sizeBytes * ext4OverheadPercent() / 100

	// Per-file overhead: ~1 KiB per file for inode (256 B) + directory entry + indirect block alignment.
	perFileOverhead := fileCount * 1024

	raw := sizeBytes + fixedOverhead + percentageOverhead + perFileOverhead

	// Preserve scratch headroom for mkfs.ext4's default inode table. The final
	// artifact size is determined by resize2fs, not this initial allocation.
	if raw < 1024*mib {
		raw = 1024 * mib
	}

	// Align up to 256 MiB boundary for initial sparse allocation before mkfs.
	alignment := int64(256) * mib
	imageSize := ((raw + alignment - 1) / alignment) * alignment

	if err := runCommand(ctx, "", "truncate", "-s", strconv.FormatInt(imageSize, 10), ext4Path); err != nil {
		return fmt.Errorf("truncate ext4 image failed: %w", err)
	}
	if err := runCommand(ctx, "", "mkfs.ext4", "-F", "-b", "4096", "-d", rootfsDir, ext4Path); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %w", err)
	}

	// Shrink and align to 2 MiB boundary for virtio-pmem backing file compatibility.
	if err := runE2fsck(ctx, ext4Path); err != nil {
		return fmt.Errorf("pre-shrink e2fsck failed: %w", err)
	}
	if err := runCommand(ctx, "", "resize2fs", "-M", ext4Path); err != nil {
		return fmt.Errorf("resize2fs shrink failed: %w", err)
	}
	fi, err := os.Stat(ext4Path)
	if err != nil {
		return fmt.Errorf("stat shrunk ext4 image failed: %w", err)
	}
	alignedSize := alignUp(fi.Size(), pmemAlignmentBytes)
	if err := runCommand(ctx, "", "truncate", "-s", strconv.FormatInt(alignedSize, 10), ext4Path); err != nil {
		return fmt.Errorf("truncate ext4 image to 2MiB boundary failed: %w", err)
	}
	if err := verifyExt4Image(ctx, ext4Path); err != nil {
		return fmt.Errorf("post-truncate e2fsck failed: %w", err)
	}
	return nil
}

func verifyExt4Image(ctx context.Context, ext4Path string) error {
	// Reject any inconsistency after shrink without modifying the artifact.
	return runCommand(ctx, "", "e2fsck", "-fn", ext4Path)
}

func runE2fsck(ctx context.Context, ext4Path string) error {
	cmd := exec.CommandContext(ctx, "e2fsck", "-fy", ext4Path)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// e2fsck exit codes:
		// 0: no errors
		// 1: file system errors corrected
		// 2: file system errors corrected, system should be rebooted
		// >= 4: uncorrected or operational errors
		if exitErr.ExitCode() == 1 || exitErr.ExitCode() == 2 {
			log.G(ctx).Warnf("e2fsck repaired ext4 image %s (exit %d): %s", ext4Path, exitErr.ExitCode(), strings.TrimSpace(string(output)))
			return nil
		}
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
}

// EnsureArtifactBuildPreflight asserts that the host has all necessary tools
// installed to build images before starting a long-running workflow.
func EnsureArtifactBuildPreflight(ctx context.Context) error {
	requiredCommands := []string{"mkfs.ext4", "truncate", "cp", "resize2fs", "e2fsck"}
	if !nativeRootfsExportEnabled() {
		if hasDockerlessRootfsExportTools() {
			requiredCommands = append(requiredCommands, "skopeo", "umoci")
		} else {
			requiredCommands = append(requiredCommands, "docker", "tar")
		}
	}
	if loopMountExt4Enabled() {
		requiredCommands = append(requiredCommands, "losetup", "mount", "umount")
	}

	for _, cmd := range requiredCommands {
		if _, err := executableLookPath(cmd); err != nil {
			return fmt.Errorf("required command %q is not available: %w", cmd, err)
		}
	}
	return checkMkfsExt4DSupport(ctx)
}

func checkMkfsExt4DSupport(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, "mkfs.ext4", "-h").CombinedOutput()
	helpText := string(output)
	if err != nil && helpText == "" {
		return fmt.Errorf("failed to probe mkfs.ext4 help output: %w", err)
	}
	if !strings.Contains(helpText, "-d") {
		return fmt.Errorf("mkfs.ext4 on cubemaster node does not appear to support the -d option required for rootfs image creation")
	}
	return nil
}

func relocateRootfsToArtifactStore(ctx context.Context, srcRootfsDir, dstRootfsDir string) error {
	if err := os.RemoveAll(dstRootfsDir); err != nil { // NOCC:Path Traversal()
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstRootfsDir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(srcRootfsDir, dstRootfsDir); err == nil {
		return nil
	} else if !isCrossDeviceRenameErr(err) {
		return err
	}
	if err := runCommand(ctx, "", "cp", "-a", srcRootfsDir, dstRootfsDir); err != nil {
		return fmt.Errorf("copy rootfs to artifact store failed: %w", err)
	}
	return os.RemoveAll(srcRootfsDir) // NOCC:Path Traversal()
}

func isCrossDeviceRenameErr(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return errors.Is(linkErr.Err, syscall.EXDEV)
	}
	return errors.Is(err, syscall.EXDEV)
}

func BuildExt4(ctx context.Context, source *PreparedSource, opts BuildOptions) (BuildResult, error) {
	workDir := filepath.Join(ArtifactWorkRootDir(), opts.ArtifactID)
	storeDir, err := ResolveArtifactStoreDir(ctx, opts.ArtifactID)
	if err != nil {
		return BuildResult{}, err
	}
	storeRootfsDir := filepath.Join(storeDir, "rootfs")
	ext4Path := filepath.Join(storeDir, opts.ArtifactID+".ext4")
	// Build into a temp sibling and atomically rename into place. The artifact
	// is only ever visible under its final name once it is COMPLETE: a crash
	// mid-mkfs leaves a full-size sparse file whose stat and even sha256 look
	// plausible, and the in-progress marker expires after 2h, so neither can
	// prove completion. The build marker now only guards against concurrent
	// cleanup; existence of the final name is what reuse trusts.
	tmpExt4Path := ext4Path + ".tmp." + strconv.Itoa(os.Getpid())
	keepStoreDir := false

	// Publish the in-progress marker before anything is written under storeDir.
	// The native exporter keeps its layer prefetch dir inside storeDir, so a
	// cleanup running in ANOTHER process (CubeTemplateCenter builds while
	// CubeMaster cleans up) would otherwise be free to RemoveAll the directory
	// from under this build.
	releaseMarker, err := MarkArtifactBuildInProgress(storeDir)
	if err != nil {
		// Best-effort: losing the guard is preferable to failing the build.
		log.G(ctx).Warnf("cannot mark artifact build in progress for %s: %v", storeDir, err)
	}
	defer releaseMarker()

	// Phase 2: loop-mount streaming build (optional, auto-detects capability).
	// Passes PostRootfsExport down to be executed before unmounting the loop device.
	// Streaming Phase 2 is currently only implemented for docker and native modes.
	if loopMountExt4Enabled() && canUseLoopMount() && (source.ExportMode == ExportModeDocker || source.ExportMode == ExportModeNative) {
		estimatedPhase2, err := estimateImageSizeFromInspect(ctx, source)
		if err != nil {
			log.G(ctx).Warnf("cannot estimate image size for Phase 2, falling back to Phase 1: %v", err)
		} else {
			if err := checkDiskSpace(ctx, storeDir, estimatedPhase2); err != nil {
				return BuildResult{}, err
			}
			if err := createExt4ImageStreaming(ctx, source, workDir, tmpExt4Path, estimatedPhase2, opts.PostRootfsExport); err != nil {
				log.G(ctx).Warnf("loop-mount streaming ext4 build failed, falling back to phase-1: %v", err)
				_ = os.RemoveAll(workDir)
				_ = os.Remove(tmpExt4Path)
			} else {
				if err := os.Rename(tmpExt4Path, ext4Path); err != nil { // NOCC:Path Traversal()
					_ = os.Remove(tmpExt4Path)
					return BuildResult{}, fmt.Errorf("publish ext4 artifact failed: %w", err)
				}
				shaValue, sizeBytes, err := computeFileSHA256(ext4Path)
				if err != nil {
					return BuildResult{}, err
				}
				_ = os.RemoveAll(workDir)
				keepStoreDir = true
				return BuildResult{Ext4Path: ext4Path, SHA256: shaValue, SizeBytes: sizeBytes}, nil
			}
		}
	}

	estimatedSizeBytes, err := estimateImageSizeFromInspect(ctx, source)
	if err != nil {
		log.G(ctx).Warnf("cannot estimate image size for disk-space check, skipping: %v", err)
	} else if estimatedSizeBytes > 0 {
		if err := checkDiskSpace(ctx, storeDir, estimatedSizeBytes); err != nil {
			return BuildResult{}, err
		}
	}

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

	if err := createExt4Image(ctx, storeRootfsDir, tmpExt4Path); err != nil {
		_ = os.Remove(tmpExt4Path)
		return BuildResult{}, err
	}
	if err := os.Rename(tmpExt4Path, ext4Path); err != nil { // NOCC:Path Traversal()
		_ = os.Remove(tmpExt4Path)
		return BuildResult{}, fmt.Errorf("publish ext4 artifact failed: %w", err)
	}
	shaValue, sizeBytes, err := computeFileSHA256(ext4Path)
	if err != nil {
		return BuildResult{}, err
	}
	keepStoreDir = true
	return BuildResult{Ext4Path: ext4Path, SHA256: shaValue, SizeBytes: sizeBytes}, nil
}
