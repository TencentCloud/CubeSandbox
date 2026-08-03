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
		// Drain is intentionally destructive: sessions are closed before the
		// caller knows whether pause/destroy will complete. If the transition
		// fails, the sandbox remains running but those terminal sessions cannot
		// be restored; clear only the admission fence so a later attempt can
		// open fresh sessions. AllowSandbox is a no-op for a deleted sandbox.
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
