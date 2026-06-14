// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"errors"
	"fmt"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func createExt4Image(ctx context.Context, rootfsDir, ext4Path string) error {
	sizeBytes, fileCount, err := directorySizeAndFileCount(rootfsDir)
	if err != nil {
		return err
	}

	const mib = int64(1024 * 1024)
	const gib = int64(1024 * 1024 * 1024)

	// Fixed overhead (default 256 MiB, configurable).
	fixedOverhead := ext4FixedOverheadMiB() * mib

	// Percentage overhead: configurable percentage of the data size (default 10%).
	percentageOverhead := sizeBytes * ext4OverheadPercent() / 100

	// Per-file overhead: ~1 KiB per file for inode (256 B) + directory entry + indirect block alignment.
	perFileOverhead := fileCount * 1024

	raw := sizeBytes + fixedOverhead + percentageOverhead + perFileOverhead

	// Minimum 1 GiB.
	if raw < gib {
		raw = gib
	}

	// Align up to 256 MiB boundary instead of next power-of-2.
	alignment := int64(256) * mib
	imageSize := ((raw + alignment - 1) / alignment) * alignment

	if err := runCommand(ctx, "", "truncate", "-s", strconv.FormatInt(imageSize, 10), ext4Path); err != nil {
		return fmt.Errorf("truncate ext4 image failed: %w", err)
	}
	if err := runCommand(ctx, "", "mkfs.ext4", "-F", "-d", rootfsDir, ext4Path); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %w", err)
	}
	return nil
}

// EnsureArtifactBuildPreflight asserts that the host has all necessary tools
// installed to build images before starting a long-running workflow.
func EnsureArtifactBuildPreflight(ctx context.Context) error {
	requiredCommands := []string{"mkfs.ext4", "truncate", "cp"}
	if !nativeRootfsExportEnabled() {
		if hasDockerlessRootfsExportTools() {
			requiredCommands = append(requiredCommands, "skopeo", "umoci")
		} else {
			requiredCommands = append(requiredCommands, "docker", "tar")
		}
	}
	if loopMountExt4Enabled() {
		requiredCommands = append(requiredCommands, "losetup", "mount", "umount", "resize2fs")
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
	keepStoreDir := false

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
			if err := createExt4ImageStreaming(ctx, source, workDir, ext4Path, estimatedPhase2, opts.PostRootfsExport); err != nil {
				log.G(ctx).Warnf("loop-mount streaming ext4 build failed, falling back to phase-1: %v", err)
				_ = os.RemoveAll(workDir)
				_ = os.Remove(ext4Path)
			} else {
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

	if err := createExt4Image(ctx, storeRootfsDir, ext4Path); err != nil {
		return BuildResult{}, err
	}
	shaValue, sizeBytes, err := computeFileSHA256(ext4Path)
	if err != nil {
		return BuildResult{}, err
	}
	keepStoreDir = true
	return BuildResult{Ext4Path: ext4Path, SHA256: shaValue, SizeBytes: sizeBytes}, nil
}
