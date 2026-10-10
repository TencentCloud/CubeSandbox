// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// verifyAppSnapshotReadiness runs the template's declared readiness probe once
// immediately before cube-runtime freezes the guest. This intentionally does
// not add a task-status fallback: templates without a declared probe preserve
// their existing AppSnapshot behavior.
func (s *service) verifyAppSnapshotReadiness(ctx context.Context, sandboxID string) error {
	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("load cubebox %s: %w", sandboxID, err)
	}
	if cb == nil {
		return nil
	}

	ci := cb.FirstContainer()
	if ci == nil || ci.Config == nil {
		return nil
	}
	return runDeclaredAppSnapshotReadinessProbe(ci.Config, func() error {
		probeCtx, cancel := context.WithTimeout(ctx, snapshotReadinessProbeTimeout(ci.Config))
		defer cancel()
		return s.cubeboxMgr.doSnapshotReadinessProbe(probeCtx, ci.Config, ci)
	})
}

func runDeclaredAppSnapshotReadinessProbe(c *cubebox.ContainerConfig, probe func() error) error {
	if c == nil || c.GetProbe() == nil || c.GetProbe().GetProbeHandler() == nil {
		return nil
	}
	return probe()
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
