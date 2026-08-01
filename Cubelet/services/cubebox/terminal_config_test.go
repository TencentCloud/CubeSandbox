// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/services/cubebox/terminalcore"
)

func TestDefaultTerminalServicesConfigUsesCoreDefaults(t *testing.T) {
	config, drainTimeout, err := defaultTerminalServicesConfig().coreConfig()
	require.NoError(t, err)
	require.Equal(t, terminalcore.DefaultConfig(), config)
	require.Equal(t, 15*time.Second, drainTimeout)
}

func TestTerminalServicesConfigPreservesExplicitZeroDurations(t *testing.T) {
	config := defaultTerminalServicesConfig()
	config.ReconnectGraceSeconds = 0
	config.ResizeCoalesceMillis = 0
	config.CleanupGraceSeconds = 0

	coreConfig, drainTimeout, err := config.coreConfig()
	require.NoError(t, err)
	require.Zero(t, coreConfig.ReconnectGrace)
	require.Zero(t, coreConfig.ResizeCoalesce)
	require.Zero(t, coreConfig.CleanupGrace)
	require.Equal(t, 15*time.Second, drainTimeout)
}
