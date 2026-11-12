// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// ErrExt4RepairUnrecoverable stops the restart. Another pass will not fix the disk.
var ErrExt4RepairUnrecoverable = errors.New("ext4 repair unrecoverable")

// Per-pass cap when the caller context has no tighter deadline.
const restartExt4RepairTimeout = 60 * time.Second

// Recheck until the disk is clean, then boot.
const ext4RepairMaxPasses = 3

const (
	ext4ExitCorrected = 1 << 0
	ext4ExitReboot    = 1 << 1
	ext4ExitLeftDirty = 1 << 2
)

// Tests replace this.
var runExt4Fsck = utils.ExecVCtx

func (l *local) repairRetainedRootfs(ctx context.Context, sandboxID string) error {
	if l == nil || sandboxID == "" {
		return nil
	}
	info, err := l.readBackendFileInfo(ctx, sandboxID)
	if err != nil && errors.Is(err, ErrCowObjectMissing) {
		info, err = l.readBackendFileInfoRaw(ctx, sandboxID)
	}
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return nil
		}
		return fmt.Errorf("restart repair rootfs %s: %w", sandboxID, err)
	}
	if info == nil {
		return nil
	}
	targets, err := retainedRootfsTargets(sandboxID, info.Volumes)
	if err != nil {
		return err
	}
	var repaired error
	for _, vol := range targets {
		if err := repairExt4AfterCrash(ctx, vol.FilePath); err != nil {
			log.G(ctx).Errorf("restart repair ext4 %s (%s): %v", vol.FilePath, vol.VolumeName, err)
			repaired = errors.Join(repaired, err)
			continue
		}
		log.G(ctx).Warnf("restart repair ext4 %s (%s) done", vol.FilePath, vol.VolumeName)
	}
	return repaired
}

// The one rootfs disk a restart may fsck.
func retainedRootfsTargets(sandboxID string, vols map[string]*BackendFileInfo) ([]*BackendFileInfo, error) {
	prefix := fmt.Sprintf("sb-%s-rootfs-gen", sandboxID)
	var cow []*BackendFileInfo
	var plain []*BackendFileInfo
	for _, vol := range vols {
		if vol == nil || strings.TrimSpace(vol.FilePath) == "" {
			continue
		}
		if strings.HasPrefix(vol.VolumeName, prefix) {
			cow = append(cow, vol)
			continue
		}
		if vol.VolumeName == "" && vol.Type == "ext4" && vol.Medium == cubebox.StorageMedium_StorageMediumDefault {
			plain = append(plain, vol)
		}
	}
	if len(cow) > 1 {
		return nil, fmt.Errorf("multiple cubecow rootfs candidates for sandbox %s", sandboxID)
	}
	if len(cow) == 1 {
		return cow, nil
	}
	return plain, nil
}

// Exit 1 or 2 changed the disk and may be followed by another check.
func ext4RepairAccepted(exitCode int) bool {
	if exitCode <= 0 || ext4RepairUnrecoverable(exitCode) {
		return false
	}
	return exitCode&(ext4ExitCorrected|ext4ExitReboot) != 0 &&
		exitCode&^(ext4ExitCorrected|ext4ExitReboot) == 0
}

func repairExt4AfterCrash(ctx context.Context, path string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	ext4, err := hasExt4Superblock(path)
	if err != nil {
		return err
	}
	if !ext4 {
		return nil
	}
	var lastCode int
	var lastOut, lastErr string
	for pass := 1; pass <= ext4RepairMaxPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("e2fsck %s: %w", path, err)
		}
		passCtx, cancel := context.WithTimeout(ctx, restartExt4RepairTimeout)
		stdout, stderr, err := runExt4Fsck(passCtx, []string{"e2fsck", "-fy", path})
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("e2fsck %s: %w stdout=%s stderr=%s", path, ctxErr, trimFsckOutput(stdout), trimFsckOutput(stderr))
		}
		code, ok := ext4FsckExitCode(err)
		if !ok {
			return fmt.Errorf("e2fsck %s: %w stdout=%s stderr=%s", path, err, trimFsckOutput(stdout), trimFsckOutput(stderr))
		}
		log.G(ctx).Warnf("restart repair ext4 %s pass %d/%d exit %d: %s", path, pass, ext4RepairMaxPasses, code, trimFsckOutput(stdout))
		if code == 0 {
			return nil
		}
		lastCode, lastOut, lastErr = code, stdout, stderr
		if ext4RepairUnrecoverable(code) {
			return fmt.Errorf("%w: e2fsck %s exit %d stdout=%s stderr=%s", ErrExt4RepairUnrecoverable, path, code, trimFsckOutput(stdout), trimFsckOutput(stderr))
		}
		if !ext4RepairAccepted(code) {
			return fmt.Errorf("e2fsck %s exit %d stdout=%s stderr=%s", path, code, trimFsckOutput(stdout), trimFsckOutput(stderr))
		}
	}
	return fmt.Errorf("%w: e2fsck %s still exit %d after %d passes stdout=%s stderr=%s",
		ErrExt4RepairUnrecoverable, path, lastCode, ext4RepairMaxPasses, trimFsckOutput(lastOut), trimFsckOutput(lastErr))
}

func ext4FsckExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

func trimFsckOutput(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 240
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func ext4RepairUnrecoverable(exitCode int) bool {
	return exitCode > 0 && exitCode&ext4ExitLeftDirty != 0
}

// Skip anything that is not ext4.
func hasExt4Superblock(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	var magic [2]byte
	if _, err := f.ReadAt(magic[:], 0x438); err != nil {
		return false, fmt.Errorf("read ext4 superblock %s: %w", path, err)
	}
	return binary.LittleEndian.Uint16(magic[:]) == 0xEF53, nil
}
