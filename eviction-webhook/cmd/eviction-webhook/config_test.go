// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_RequiresCubeMasterURL(t *testing.T) {
	t.Setenv("CUBE_MASTER_URL", "")

	_, err := loadConfig()

	require.Error(t, err)
}

func TestLoadConfig_RecoveryEnabledDefaultsToTrue(t *testing.T) {
	t.Setenv("CUBE_MASTER_URL", "http://cube-master:8089")
	t.Setenv("RECOVERY_ENABLE", "")

	cfg, err := loadConfig()

	require.NoError(t, err)
	assert.True(t, cfg.RecoveryEnabled)
}

func TestLoadConfig_RecoveryEnabledExplicitFalse(t *testing.T) {
	t.Setenv("CUBE_MASTER_URL", "http://cube-master:8089")
	t.Setenv("RECOVERY_ENABLE", "false")

	cfg, err := loadConfig()

	require.NoError(t, err)
	assert.False(t, cfg.RecoveryEnabled)
}

func TestLoadConfig_RecoveryEnabledExplicitTrue(t *testing.T) {
	t.Setenv("CUBE_MASTER_URL", "http://cube-master:8089")
	t.Setenv("RECOVERY_ENABLE", "true")

	cfg, err := loadConfig()

	require.NoError(t, err)
	assert.True(t, cfg.RecoveryEnabled)
}

func TestLoadConfig_RecoveryEnabledAnyNonFalseValueIsTrue(t *testing.T) {
	t.Setenv("CUBE_MASTER_URL", "http://cube-master:8089")
	t.Setenv("RECOVERY_ENABLE", "0")

	cfg, err := loadConfig()

	require.NoError(t, err)
	assert.True(t, cfg.RecoveryEnabled, "RECOVERY_ENABLE opts out only on the literal value \"false\"")
}

func TestLoadConfig_AuthRequiresCompleteCredentials(t *testing.T) {
	t.Setenv("CUBE_MASTER_URL", "http://cube-master:8089")
	t.Setenv("CUBE_AUTH_ENABLE", "true")
	t.Setenv("CUBE_AUTH_USER_ID", "shared-user")
	t.Setenv("CUBE_AUTH_SECRET_KEY", "")

	_, err := loadConfig()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires CUBE_AUTH_USER_ID and CUBE_AUTH_SECRET_KEY")
}

func TestLoadConfig_AuthAcceptsCompleteCredentials(t *testing.T) {
	t.Setenv("CUBE_MASTER_URL", "http://cube-master:8089")
	t.Setenv("CUBE_AUTH_ENABLE", "true")
	t.Setenv("CUBE_AUTH_USER_ID", "shared-user")
	t.Setenv("CUBE_AUTH_SECRET_KEY", "shared-secret")

	cfg, err := loadConfig()

	require.NoError(t, err)
	assert.True(t, cfg.AuthEnabled)
	assert.Equal(t, "shared-user", cfg.AuthUserID)
	assert.Equal(t, "shared-secret", cfg.AuthSecretKey)
}
