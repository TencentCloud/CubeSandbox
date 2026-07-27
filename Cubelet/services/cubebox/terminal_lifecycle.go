// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/cubebox/terminalcore"
)

func (s *service) drainTerminalSandbox(ctx context.Context, sandboxID, reason string) error {
	if s.terminal == nil {
		return nil
	}
	drainCtx, cancel := context.WithTimeout(ctx, s.terminalDrainTimeout)
	defer cancel()
	return s.terminal.DrainSandbox(drainCtx, sandboxID, reason)
}

func (s *service) allowTerminalSandbox(sandboxID string) {
	if s.terminal != nil && sandboxID != "" {
		s.terminal.AllowSandbox(sandboxID)
	}
}

func (s *service) allowTerminalSandboxIfRunning(ctx context.Context, sandboxID string) {
	if s.terminal == nil || sandboxID == "" {
		return
	}
	sandbox, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err == nil && sandbox.GetStatus().Get().State() == cubebox.ContainerState_CONTAINER_RUNNING {
		s.terminal.AllowSandbox(sandboxID)
	}
}

func (s *service) Shutdown(ctx context.Context) error {
	if s.terminal == nil {
		return nil
	}
	return s.terminal.Close(ctx, terminalcore.CloseServerDraining)
}
