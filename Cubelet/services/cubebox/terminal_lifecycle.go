// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/Cubelet/services/cubebox/terminalcore"
)

func (s *service) drainTerminalSandbox(ctx context.Context, sandboxID, reason string) error {
	if s.terminal == nil {
		return nil
	}
	drainCtx, cancel := context.WithTimeout(ctx, s.terminalDrainTimeout)
	defer cancel()
	if err := s.terminal.DrainSandbox(drainCtx, sandboxID, reason); err != nil {
		// The caller aborts the transition when the drain fails, so the sandbox
		// is still running and its admission fence must not survive — otherwise
		// it is permanently blocked from new terminal sessions until a later
		// successful resume. AllowSandbox is a no-op for a deleted sandbox.
		s.terminal.AllowSandbox(sandboxID)
		return err
	}
	return nil
}

func (s *service) allowTerminalSandbox(sandboxID string) {
	if s.terminal != nil && sandboxID != "" {
		s.terminal.AllowSandbox(sandboxID)
	}
}

func (s *service) Shutdown(ctx context.Context) error {
	if s.terminal == nil {
		return nil
	}
	return s.terminal.Close(ctx, terminalcore.CloseServerDraining)
}
