// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func computeFileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path) // NOCC:Path Traversal()
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hasher := sha256.New()
	// Use a 4 MiB buffer to reduce read syscall count for large files.
	buf := make([]byte, 4*1024*1024)
	size, err := io.CopyBuffer(hasher, f, buf)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

// directorySizeAndFileCount returns the total size of regular files and the count of
// regular files in the directory tree rooted at root.  A single filepath.Walk avoids
// a second I/O pass.
func directorySizeAndFileCount(root string) (int64, int64, error) {
	var totalSize, fileCount int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info == nil || info.IsDir() {
			return nil
		}
		totalSize += info.Size()
		fileCount++
		return nil
	})
	return totalSize, fileCount, err
}

// checkDiskSpace verifies that the filesystem hosting storeDir has enough free space
// for the estimated build requirements multiplied by a configurable safety margin.
// If storeDir does not exist yet, the function falls back to statfs-ing its parent.
func checkDiskSpace(ctx context.Context, storeDir string, estimatedSizeBytes int64) error {
	var stat syscall.Statfs_t
	dir := storeDir
	if err := syscall.Statfs(dir, &stat); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			dir = filepath.Dir(storeDir)
			if err2 := syscall.Statfs(dir, &stat); err2 != nil {
				return fmt.Errorf("statfs failed for %s (and parent %s): %w", storeDir, dir, err2)
			}
		} else {
			return fmt.Errorf("statfs failed for %s: %w", storeDir, err)
		}
	}
	availableBytes := int64(stat.Bavail) * int64(stat.Bsize)
	margin := diskSpaceSafetyMargin()
	requiredBytes := int64(float64(estimatedSizeBytes) * margin)
	if availableBytes < requiredBytes {
		return fmt.Errorf(
			"insufficient disk space: available=%d GiB, estimated_required=%d GiB (image_size_estimate * %.1fx safety_margin)",
			availableBytes/(1024*1024*1024),
			requiredBytes/(1024*1024*1024),
			margin,
		)
	}
	log.G(ctx).Infof("disk space check passed: available=%d GiB, required=%d GiB",
		availableBytes/(1024*1024*1024), requiredBytes/(1024*1024*1024))
	return nil
}

// isLocalFastFS returns true when the filesystem of the given path appears safe
// for direct rootfs export rather than requiring workDir + relocate.  It is
// conservative for known network or FUSE filesystems and falls back to the
// parent directory when the artifact directory has not been created yet.
func isLocalFastFS(path string) bool {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return false // cannot determine – be conservative
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false
		}
		if err := syscall.Statfs(parent, &stat); err != nil {
			return false
		}
	}
	// Known network or FUSE filesystem magic numbers.
	const (
		NFS_SUPER_MAGIC  = 0x6969
		CIFS_SUPER_MAGIC = 0xFF534D42
		FUSE_SUPER_MAGIC = 0x65735546
	)
	switch stat.Type {
	case NFS_SUPER_MAGIC, CIFS_SUPER_MAGIC, FUSE_SUPER_MAGIC:
		return false
	default:
		return true
	}
}

// canUseLoopMount checks whether the host environment supports loop-device
// mount-based ext4 creation (Phase 2).  Returns false when CubeMaster runs
// inside a container without CAP_SYS_ADMIN or /dev/loop-control.
func canUseLoopMount() bool {
	// Check CAP_SYS_ADMIN – mount(2) requires it.
	if !hasCapability(unix.CAP_SYS_ADMIN) {
		return false
	}
	// Check that /dev/loop-control exists.
	if _, err := os.Stat("/dev/loop-control"); err != nil {
		return false
	}
	// Check required commands.
	for _, cmd := range []string{"mount", "umount", "resize2fs"} {
		if _, err := exec.LookPath(cmd); err != nil {
			return false
		}
	}
	return true
}

// hasCapability returns true when the current process has the given capability
// in its effective set.  On Linux this reads /proc/self/status.
func hasCapability(cap uintptr) bool {
	// Attempt to read CapEff from /proc/self/status.
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "CapEff:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return false
			}
			caps, err := strconv.ParseUint(fields[1], 16, 64)
			if err != nil {
				return false
			}
			return caps&(1<<uint(cap)) != 0
		}
	}
	return false
}

// getFileBlockSize returns the actual on-disk size of a file (as reported by
// stat(2) st_blocks * 512), which for sparse files is smaller than the apparent
// length.
func getFileBlockSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fi.Size() // fallback to apparent size
	}
	return stat.Blocks * 512
}
