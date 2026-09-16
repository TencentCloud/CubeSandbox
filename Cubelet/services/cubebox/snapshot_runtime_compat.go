// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

const incompatibleSnapshotRuntimeMessage = "incompatible cube-runtime: consistent memory/rootfs snapshots require --keep-paused and snapshot-resume; upgrade or migrate the sandbox runtime"

func cubeRuntimeSupportsKeepPaused(ctx context.Context, runtimePath string) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(checkCtx, runtimePath, "snapshot", "--help").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("check cube-runtime snapshot capabilities: %w, output: %s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "--keep-paused" {
			// A held freeze window is only usable when explicit resume exists too.
			if output, err := exec.CommandContext(checkCtx, runtimePath, "snapshot-resume", "--help").CombinedOutput(); err != nil {
				return false, fmt.Errorf("check cube-runtime snapshot-resume capability: %w, output: %s", err, output)
			}
			return true, nil
		}
	}
	return false, nil
}

func (s *service) runtimeSnapshotSupportsKeepPaused(ctx context.Context, sandboxID string) (bool, error) {
	runtimePath, err := s.resolveCubeRuntimePath(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	return cubeRuntimeSupportsKeepPaused(ctx, runtimePath)
}

// runSnapshotWithRootfs captures memory and rootfs in one frozen window.
// Callers must reject runtimes without keep-paused support before invoking it.
func runSnapshotWithRootfs(snapshot, rootfs, resume func() error) (snapshotErr, rootfsErr, resumeErr error) {
	if snapshotErr = snapshot(); snapshotErr != nil {
		resumeErr = resume()
		return
	}
	defer func() { resumeErr = resume() }()
	rootfsErr = rootfs()
	return
}

func checkCommitSnapshotDestination(ctx context.Context, backend, snapshotID string, inspect func(context.Context, string, []storage.CowObjectRef) ([]storage.CowObjectStatus, error)) error {
	// Include work and sealed objects: S3 may resolve an existing writable
	// memory volume instead of rejecting it, so none may be reused here.
	refs := storage.DefaultTemplateObjectRefs(snapshotID)
	statuses, err := inspect(ctx, backend, refs)
	if err != nil {
		return err
	}
	for _, status := range statuses {
		if status.Exists {
			return fmt.Errorf("%w: name=%s kind=%s", storage.ErrCowObjectAlreadyExists, status.Name, status.Kind)
		}
	}
	return nil
}
