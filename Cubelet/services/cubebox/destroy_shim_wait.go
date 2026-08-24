// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const (
	shimPidFileName = "shim.pid"
	vmmPidFileName  = "vmm.pid"
)

// collectSandboxRuntimePIDs snapshots every host pid that may still hold
// sandbox disks: recorded task/endpoint pids plus the shim bundle's
// shim.pid / vmm.pid. Call this BEFORE DeleteTask — containerd removes the
// bundle on Delete, and TaskExit only means the task slot is gone, not that
// the shim process has released NVMe fds.
func (l *local) collectSandboxRuntimePIDs(ctx context.Context, sb *cubeboxstore.CubeBox) []int {
	if sb == nil {
		return nil
	}
	seen := map[int]struct{}{}
	add := func(pid int) {
		if pid <= 1 || pid == os.Getpid() {
			return
		}
		seen[pid] = struct{}{}
	}
	for _, pid := range recordedSandboxPIDs(sb) {
		add(pid)
	}
	for _, bundle := range l.shimBundlePaths(ctx, sb) {
		add(readPidFile(filepath.Join(bundle, shimPidFileName)))
		add(readPidFile(filepath.Join(bundle, vmmPidFileName)))
	}
	if len(seen) == 0 {
		return nil
	}
	pids := make([]int, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

func recordedSandboxPIDs(sb *cubeboxstore.CubeBox) []int {
	if sb == nil {
		return nil
	}
	var pids []int
	if status := sb.GetStatus(); status != nil {
		pids = append(pids, int(status.Get().Pid))
	}
	pids = append(pids, int(sb.Endpoint.Pid))
	if main := sb.FirstContainer(); main != nil && main.Status != nil {
		pids = append(pids, int(main.Status.Get().Pid))
	}
	for _, ctr := range sb.AllContainers() {
		if ctr == nil || ctr.Status == nil {
			continue
		}
		pids = append(pids, int(ctr.Status.Get().Pid))
	}
	return pids
}

func (l *local) shimBundlePaths(ctx context.Context, sb *cubeboxstore.CubeBox) []string {
	if l == nil || l.shims == nil || sb == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var bundles []string
	for _, id := range sandboxShimLookupIDs(sb) {
		shim, err := l.shims.Get(ctx, id)
		if err != nil || shim == nil {
			continue
		}
		bundle := strings.TrimSpace(shim.Bundle())
		if bundle == "" {
			continue
		}
		if _, ok := seen[bundle]; ok {
			continue
		}
		seen[bundle] = struct{}{}
		bundles = append(bundles, bundle)
	}
	return bundles
}

func sandboxShimLookupIDs(sb *cubeboxstore.CubeBox) []string {
	if sb == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(sb.ID)
	if main := sb.FirstContainer(); main != nil {
		add(main.ID)
	}
	for _, ctr := range sb.AllContainers() {
		if ctr != nil {
			add(ctr.ID)
		}
	}
	return ids
}

func readPidFile(path string) int {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return pid
}

// waitSandboxRuntimeGone blocks until every captured runtime pid has exited.
// Storage Destroy runs in the next workflow step and deletes S3 volumes;
// doing that while the shim still holds the NVMe path produces host I/O
// after delete_lvol.
func waitSandboxRuntimeGone(ctx context.Context, sandboxID string, pids []int) error {
	if len(pids) == 0 {
		return nil
	}
	start := time.Now()
	for _, pid := range pids {
		if err := utils.WaitProcessGone(ctx, pid); err != nil {
			log.G(ctx).Errorf("sandbox %s: runtime pid %d still alive after %s; refuse volume cleanup: %v",
				sandboxID, pid, time.Since(start), err)
			return err
		}
	}
	if waited := time.Since(start); waited > 20*time.Millisecond {
		log.G(ctx).Warnf("sandbox %s: waited %s for runtime pids %v to exit before volume cleanup",
			sandboxID, waited.Round(time.Millisecond), pids)
	}
	return nil
}
