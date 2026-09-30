// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

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
	var repaired error
	for _, vol := range info.Volumes {
		if !isRetainedRootfs(vol) {
			continue
		}
		if err := repairExt4AfterCrash(vol.FilePath); err != nil {
			log.G(ctx).Errorf("restart repair ext4 %s (%s): %v", vol.FilePath, vol.VolumeName, err)
			repaired = errors.Join(repaired, err)
			continue
		}
		log.G(ctx).Warnf("restart repair ext4 %s (%s) done", vol.FilePath, vol.VolumeName)
	}
	return repaired
}

func isRetainedRootfs(info *BackendFileInfo) bool {
	if info == nil || strings.TrimSpace(info.FilePath) == "" {
		return false
	}
	return strings.Contains(info.VolumeName, "rootfs") || strings.Contains(info.FilePath, "rootfs")
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
	stdout, stderr, err := utils.ExecV([]string{"e2fsck", "-fy", path}, restartExt4RepairTimeout)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ext4RepairAccepted(exitErr.ExitCode()) {
		return nil
	}
	return fmt.Errorf("e2fsck %s: %w stdout=%s stderr=%s", path, err, stdout, stderr)
}
