// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

const appSnapshotTaskStabilityTimeout = 100 * time.Millisecond

type taskStatusFn func(context.Context) (containerd.Status, error)

func verifyTaskRunning(ctx context.Context, statusFn taskStatusFn) error {
	status, err := statusFn(ctx)
	if err != nil {
		return fmt.Errorf("get init task status: %w", err)
	}
	if status.Status != containerd.Running {
		return fmt.Errorf("init task is not running: status=%s exit_status=%d", status.Status, status.ExitStatus)
	}
	return nil
}

// waitForTaskStability checks containerd's task status before and after a
// short delay. Task.Wait is intentionally not used here: runContainer starts
// that long-lived stream with the Create RPC context, so its ExitCh can report
// context cancellation after the RPC returns even while the guest task keeps
// running.
func waitForTaskStability(ctx context.Context, statusFn taskStatusFn, timeout time.Duration) error {
	if err := verifyTaskRunning(ctx, statusFn); err != nil {
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		return verifyTaskRunning(ctx, statusFn)
	case <-ctx.Done():
		return fmt.Errorf("wait for init task stability: %w", ctx.Err())
	}
}

// verifyAppSnapshotTaskRunning prevents a dead init process from being
// snapshotted as a valid template. The containerd task status is the
// authoritative host-side lifecycle signal; ExitCh is checked as well to
// catch a task that stopped between the status RPC and the snapshot command.
func (s *service) verifyAppSnapshotTaskRunning(ctx context.Context, sandboxID string) error {
	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("load cubebox %s: %w", sandboxID, err)
	}
	if cb == nil || cb.FirstContainer() == nil {
		return fmt.Errorf("cubebox %s has no init container", sandboxID)
	}

	ci := cb.FirstContainer()
	if ci.Container == nil {
		return fmt.Errorf("cubebox %s init container has no containerd handle", sandboxID)
	}

	ns := cb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	taskCtx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), ns), 2*time.Second)
	defer cancel()
	task, err := ci.Container.Task(taskCtx, nil)
	if err != nil {
		return fmt.Errorf("load init task for %s: %w", sandboxID, err)
	}

	// Give the task status a short window to settle. This closes the race where
	// StartTask returns successfully but the guest init process exits before
	// envd probing or cube-runtime snapshotting starts.
	if err := waitForTaskStability(taskCtx, task.Status, appSnapshotTaskStabilityTimeout); err != nil {
		return fmt.Errorf("init task for %s: %w", sandboxID, err)
	}
	return nil
}
