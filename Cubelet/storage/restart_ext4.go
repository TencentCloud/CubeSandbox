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

// ErrExt4RepairUnrecoverable is an e2fsck result that will not change on
// retry: exit 4 leaves errors on the disk, exit 8 is an operational failure
// of the check itself. The restart loop should stop instead of rewriting it.
var ErrExt4RepairUnrecoverable = errors.New("ext4 repair unrecoverable")

// restartExt4RepairTimeout bounds e2fsck of one retained rootfs. These
// volumes are about a gigabyte and lightly used; the check has to finish
// inside the restart destroy window.
const restartExt4RepairTimeout = 60 * time.Second

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
		if err := repairExt4AfterCrash(vol.FilePath); err != nil {
			log.G(ctx).Errorf("restart repair ext4 %s (%s): %v", vol.FilePath, vol.VolumeName, err)
			repaired = errors.Join(repaired, err)
			continue
		}
		log.G(ctx).Warnf("restart repair ext4 %s (%s) done", vol.FilePath, vol.VolumeName)
	}
	return repaired
}

// retainedRootfsTargets picks the disk a restart may fsck.
// Cubecow rootfs is only sb-<sandboxID>-rootfs-genN. A node that does not
// use cubecow still has a default-medium ext4 with no volume name.
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

// ext4RepairAccepted reports an e2fsck status the next mount can use.
// 0 is clean, 1 is corrected, 2 is corrected and wants a reboot. The guest
// is not running, so 2 is still a repaired disk.
func ext4RepairAccepted(exitCode int) bool {
	return exitCode == 0 || exitCode == 1 || exitCode == 2
}

func repairExt4AfterCrash(path string) error {
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
	stdout, stderr, err := utils.ExecV([]string{"e2fsck", "-fy", path}, restartExt4RepairTimeout)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ext4RepairAccepted(exitErr.ExitCode()) {
		return nil
	}
	if errors.As(err, &exitErr) && ext4RepairUnrecoverable(exitErr.ExitCode()) {
		return fmt.Errorf("%w: e2fsck %s exit %d stdout=%s stderr=%s", ErrExt4RepairUnrecoverable, path, exitErr.ExitCode(), stdout, stderr)
	}
	return fmt.Errorf("e2fsck %s: %w stdout=%s stderr=%s", path, err, stdout, stderr)
}

func ext4RepairUnrecoverable(exitCode int) bool {
	return exitCode == 4 || exitCode == 8
}

// hasExt4Superblock reads the little-endian magic at offset 0x438.
// A non-ext4 retained disk must not be passed to e2fsck -fy.
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
