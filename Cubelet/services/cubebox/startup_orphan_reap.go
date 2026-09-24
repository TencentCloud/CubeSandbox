// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const startupOrphanReapPerShimTimeout = 30 * time.Second

// reapStartupOrphanShims scans the containerd bundle root and SIGKILLs any
// shim whose sandbox ID is unknown to the cubebox store AND whose VMM has
// already exited. Runs once after RecoverAllCubebox at cubelet boot.
//
// Motivation: resume-timesync failures can leave the shim process alive
// while its VMM crashes and containerd drops the task record. The old code
// then removed the cubebox record without waiting on the shim, letting the
// next Create reuse the same TAP/IP while the shim was still holding fds
// (→ 130459). Fix B (destroy_shim_wait.go, cube_container_create.go)
// prevents new leaks from being created; this reaper drains leaks that
// pre-date the fix. It also self-heals any future crash that happens to
// bypass the fix path.
//
// Set CUBELET_STARTUP_ORPHAN_REAP=false to disable (rollback switch).
func (l *local) reapStartupOrphanShims(ctx context.Context) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CUBELET_STARTUP_ORPHAN_REAP"))) {
	case "false", "0", "no", "off":
		log.G(ctx).Info("startup orphan-shim reap disabled by env")
		return
	}

	entries, err := os.ReadDir(cubeletBundleRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			log.G(ctx).Warnf("startup orphan reap: read %s: %v", cubeletBundleRoot, err)
		}
		return
	}

	known := map[string]struct{}{}
	for _, sb := range l.cubeboxManger.List() {
		if sb == nil {
			continue
		}
		known[sb.ID] = struct{}{}
		known[sb.SandboxID] = struct{}{}
		for _, ctr := range sb.AllContainers() {
			if ctr != nil {
				known[ctr.ID] = struct{}{}
			}
		}
	}

	var scanned, reaped, vmmAliveSkipped int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if _, ok := known[id]; ok {
			continue
		}
		scanned++
		bundle := filepath.Join(cubeletBundleRoot, id)
		vmm := readPidFile(filepath.Join(bundle, vmmPidFileName))
		shim := readPidFile(filepath.Join(bundle, shimPidFileName))
		if vmm > 1 && utils.ProcessAlive(vmm) {
			// VMM alive without a cubebox record means human intervention is
			// probably needed. Don't touch it.
			log.G(ctx).Warnf("startup orphan reap: id=%s VMM %d still alive, skipping", id, vmm)
			vmmAliveSkipped++
			continue
		}
		if shim <= 1 || !utils.ProcessAlive(shim) {
			continue
		}
		if err := syscall.Kill(shim, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			log.G(ctx).Warnf("startup orphan reap: SIGKILL shim %d id=%s: %v", shim, id, err)
			continue
		}
		waitCtx, cancel := context.WithTimeout(ctx, startupOrphanReapPerShimTimeout)
		if err := utils.WaitProcessGone(waitCtx, shim); err != nil {
			log.G(ctx).Warnf("startup orphan reap: shim %d id=%s still alive after SIGKILL: %v", shim, id, err)
		} else {
			reaped++
			log.G(ctx).Infof("startup orphan reap: id=%s shim=%d reaped", id, shim)
		}
		cancel()
	}
	if scanned > 0 || reaped > 0 || vmmAliveSkipped > 0 {
		log.G(ctx).Warnf("startup orphan reap done: scanned=%d reaped=%d vmm_alive_skipped=%d", scanned, reaped, vmmAliveSkipped)
	}
}
