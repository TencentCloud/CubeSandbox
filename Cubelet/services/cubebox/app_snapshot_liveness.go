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
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

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

// verifyAppSnapshotReadiness runs immediately before cube-runtime freezes the
// guest. Task status is checked before and after a declared readiness probe: a
// service-level probe alone may stay healthy after its init process exits.
// Images without a probe perform one task-status check. ExitCh is intentionally
// not used: it was created with Create's RPC context and can report context
// cancellation after a successful Create while the guest runs.
func (s *service) verifyAppSnapshotReadiness(ctx context.Context, sandboxID string) error {
	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("load cubebox %s: %w", sandboxID, err)
	}
	if cb == nil {
		return fmt.Errorf("cubebox %s has no init container", sandboxID)
	}

	ci := cb.FirstContainer()
	if ci == nil || ci.Config == nil {
		return fmt.Errorf("cubebox %s init container config is missing", sandboxID)
	}
	if ci.Container == nil {
		return fmt.Errorf("cubebox %s init container has no containerd handle", sandboxID)
	}

	verifyTask := func() error {
		return verifyAppSnapshotTaskRunning(ctx, cb.Namespace, ci.Container)
	}
	var probe func() error
	if ci.Config.GetProbe() != nil && ci.Config.GetProbe().GetProbeHandler() != nil {
		probe = func() error {
			probeCtx, cancel := context.WithTimeout(ctx, snapshotReadinessProbeTimeout(ci.Config))
			defer cancel()
			return s.cubeboxMgr.doSnapshotReadinessProbe(probeCtx, ci.Config, ci)
		}
	}
	if err := verifyTaskAroundSnapshotProbe(verifyTask, probe); err != nil {
		return fmt.Errorf("snapshot readiness for %s: %w", sandboxID, err)
	}
	return nil
}

func verifyTaskAroundSnapshotProbe(verifyTask func() error, probe func() error) error {
	if err := verifyTask(); err != nil {
		return fmt.Errorf("init task before probe: %w", err)
	}
	if probe == nil {
		return nil
	}
	if err := probe(); err != nil {
		return fmt.Errorf("readiness probe: %w", err)
	}
	if err := verifyTask(); err != nil {
		return fmt.Errorf("init task after probe: %w", err)
	}
	return nil
}

func verifyAppSnapshotTaskRunning(ctx context.Context, namespace string, container containerd.Container) error {
	if namespace == "" {
		namespace = namespaces.Default
	}
	taskCtx, cancel := context.WithTimeout(namespaces.WithNamespace(ctx, namespace), 2*time.Second)
	defer cancel()
	task, err := container.Task(taskCtx, nil)
	if err != nil {
		return fmt.Errorf("load init task: %w", err)
	}
	return verifyTaskRunning(taskCtx, task.Status)
}

func snapshotReadinessProbeTimeout(c *cubebox.ContainerConfig) time.Duration {
	if c == nil || c.GetProbe() == nil {
		return time.Second
	}
	timeout := time.Duration(c.GetProbe().GetProbeTimeoutMs()) * time.Millisecond
	if timeout <= 5*time.Millisecond {
		return 100 * time.Millisecond
	}
	return timeout
}
