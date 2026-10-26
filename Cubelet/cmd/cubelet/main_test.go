// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
	"github.com/urfave/cli/v2"
)

func TestEnsureRequiredPluginsAddsCriticalCubeletPlugins(t *testing.T) {
	cfg := platformAgnosticDefaultConfig()
	cfg.RequiredPlugins = []string{string(constants.InternalPlugin) + "." + constants.StorageID.ID()}

	ensureRequiredPlugins(cfg)

	assert.Contains(t, cfg.RequiredPlugins, string(constants.InternalPlugin)+"."+constants.StorageID.ID())
	assert.Contains(t, cfg.RequiredPlugins, string(constants.InternalPlugin)+"."+constants.CubeboxID.ID())
	assert.Contains(t, cfg.RequiredPlugins, string(constants.WorkflowPlugin)+"."+constants.WorkflowID.ID())
	assert.Contains(t, cfg.RequiredPlugins, string(constants.CubeboxServicePlugin)+"."+constants.CubeboxServiceID.ID())
}

func TestApplyFlagsCubeLogCLIOverridesTomlWhenSet(t *testing.T) {
	cfg := platformAgnosticDefaultConfig()
	cfg.CubeLog = srvconfig.CubeLogConfig{
		Path:     "/from/toml",
		FileNum:  3,
		FileSize: "100m",
	}

	require.NoError(t, runApplyFlags(cfg,
		"--logpath", "/from/cli",
		"--log-roll-num", "7",
		"--log-roll-size", "1g",
	))
	require.NoError(t, cfg.CubeLog.ApplyDefaults())

	assert.Equal(t, "/from/cli", cfg.CubeLog.Path)
	assert.Equal(t, 7, cfg.CubeLog.FileNum)
	assert.Equal(t, srvconfig.CubeLogFileSize("1g"), cfg.CubeLog.FileSize)
	assert.Equal(t, 1024, cfg.CubeLog.FileSizeMB())
}

func TestApplyFlagsCubeLogKeepsTomlWhenCLIUnset(t *testing.T) {
	cfg := platformAgnosticDefaultConfig()
	cfg.CubeLog = srvconfig.CubeLogConfig{
		Path:     "/from/toml",
		FileNum:  3,
		FileSize: "100m",
	}

	require.NoError(t, runApplyFlags(cfg))
	require.NoError(t, cfg.CubeLog.ApplyDefaults())

	assert.Equal(t, "/from/toml", cfg.CubeLog.Path)
	assert.Equal(t, 3, cfg.CubeLog.FileNum)
	assert.Equal(t, srvconfig.CubeLogFileSize("100m"), cfg.CubeLog.FileSize)
	assert.Equal(t, 100, cfg.CubeLog.FileSizeMB())
}

func TestApplyFlagsCubeLogZeroCLIClampedByDefaults(t *testing.T) {
	cfg := platformAgnosticDefaultConfig()
	cfg.CubeLog = srvconfig.CubeLogConfig{
		Path:     "/from/toml",
		FileNum:  3,
		FileSize: "100m",
	}

	require.NoError(t, runApplyFlags(cfg, "--log-roll-num", "0", "--log-roll-size", "0"))
	require.NoError(t, cfg.CubeLog.ApplyDefaults())

	assert.Equal(t, "/from/toml", cfg.CubeLog.Path)
	assert.Equal(t, srvconfig.DefaultCubeLogFileNum, cfg.CubeLog.FileNum)
	assert.Equal(t, srvconfig.DefaultCubeLogFileSize, cfg.CubeLog.FileSize)
	assert.Equal(t, srvconfig.DefaultCubeLogFileSizeMB, cfg.CubeLog.FileSizeMB())
}

func runApplyFlags(cfg *srvconfig.Config, args ...string) error {
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "log-level", Value: "warn"},
			&cli.StringFlag{Name: "root"},
			&cli.StringFlag{Name: "state"},
			&cli.StringFlag{Name: "address"},
			&cli.StringFlag{Name: "dynamic-conf-path"},
			&cli.StringFlag{Name: "logpath", Value: srvconfig.DefaultCubeLogPath},
			&cli.IntFlag{Name: "log-roll-num", Value: srvconfig.DefaultCubeLogFileNum},
			&cli.StringFlag{Name: "log-roll-size", Value: string(srvconfig.DefaultCubeLogFileSize)},
		},
		Action: func(c *cli.Context) error {
			return applyFlags(c, cfg)
		},
	}
	return app.Run(append([]string{"cubelet"}, args...))
}
