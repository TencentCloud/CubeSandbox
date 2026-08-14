// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := Default()
	cfg.ConsumerName = "test-consumer"
	return cfg
}

func TestValidateRedisStandalone(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedisAddr = "127.0.0.1:6379"
	require.NoError(t, cfg.Validate())
}

func TestValidateRedisSentinel(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedisAddr = ""
	cfg.RedisMasterName = "mymaster"
	cfg.RedisSentinelAddrs = []string{"10.0.0.11:26379", "10.0.0.12:26379"}
	require.NoError(t, cfg.Validate())
}

func TestValidateRedisSentinelMissingNodes(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedisAddr = ""
	cfg.RedisMasterName = "mymaster"
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUBE_LCM_REDIS_SENTINEL_NODES")
}

func TestValidateRedisEmpty(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedisAddr = ""
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUBE_LCM_REDIS_ADDR")
}

func TestEventBusEnabledDefaultAndKillSwitch(t *testing.T) {
	assert.True(t, Default().EventBusEnabled)

	t.Setenv("CUBE_LCM_EVENTBUS_ENABLED", "false")
	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.EventBusEnabled)
}

func TestEventBusEnabledRejectsInvalidValue(t *testing.T) {
	t.Setenv("CUBE_LCM_EVENTBUS_ENABLED", "disable")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUBE_LCM_EVENTBUS_ENABLED")
}

func TestParseSentinelAddrs(t *testing.T) {
	assert.Equal(t, []string{"10.0.0.11:26379", "10.0.0.12:26379"}, parseSentinelAddrs("10.0.0.11, 10.0.0.12"))
	assert.Equal(t, []string{"10.0.0.11:26379", "10.0.0.12:26380"}, parseSentinelAddrs("10.0.0.11:26379,10.0.0.12:26380"))
	assert.Equal(t, []string{"[::1]:26379"}, parseSentinelAddrs("[::1]"))
	assert.Equal(t, []string{"[2001:db8::1]:26379"}, parseSentinelAddrs("[2001:db8::1]:26379"))
	assert.Empty(t, parseSentinelAddrs(""))
	assert.Empty(t, parseSentinelAddrs(" , "))
}

func TestLoadHADefaults(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	// HA is opt-in: single-replica deployments keep the legacy behavior.
	assert.False(t, cfg.HAEnabled)
	assert.Equal(t, "cube:v1:shared:lock:lifecycle_manager:leader", cfg.LeaderKey)
	assert.Equal(t, 15*time.Second, cfg.LeaderTTL)
	assert.Equal(t, 5*time.Second, cfg.LeaderRenewInterval)
	assert.Equal(t, time.Minute, cfg.ReconcileInterval)
	// InstanceID falls back to the (hostname-derived) consumer name.
	assert.Equal(t, cfg.ConsumerName, cfg.InstanceID)
}

func TestLoadHAFromEnv(t *testing.T) {
	t.Setenv("CUBE_LCM_HA_ENABLED", "1")
	t.Setenv("CUBE_LCM_INSTANCE_ID", "clm-pod-a")
	t.Setenv("CUBE_LCM_LEADER_KEY", "custom:leader")
	t.Setenv("CUBE_LCM_LEADER_TTL", "20s")
	t.Setenv("CUBE_LCM_LEADER_RENEW_INTERVAL", "4s")
	t.Setenv("CUBE_LCM_RECONCILE_INTERVAL", "2m")

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.HAEnabled)
	assert.Equal(t, "clm-pod-a", cfg.InstanceID)
	assert.Equal(t, "custom:leader", cfg.LeaderKey)
	assert.Equal(t, 20*time.Second, cfg.LeaderTTL)
	assert.Equal(t, 4*time.Second, cfg.LeaderRenewInterval)
	assert.Equal(t, 2*time.Minute, cfg.ReconcileInterval)
	require.NoError(t, cfg.Validate())
}

func TestLoadHABadDuration(t *testing.T) {
	t.Setenv("CUBE_LCM_LEADER_TTL", "not-a-duration")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUBE_LCM_LEADER_TTL")
}

func TestValidateHA(t *testing.T) {
	cfg := testConfig(t)
	cfg.InstanceID = cfg.ConsumerName
	cfg.HAEnabled = true
	require.NoError(t, cfg.Validate())

	bad := *cfg
	bad.InstanceID = ""
	assert.ErrorContains(t, bad.Validate(), "instance id")

	bad = *cfg
	bad.LeaderKey = ""
	assert.ErrorContains(t, bad.Validate(), "leader key")

	bad = *cfg
	bad.LeaderRenewInterval = bad.LeaderTTL
	assert.ErrorContains(t, bad.Validate(), "renew interval")

	bad = *cfg
	bad.ReconcileInterval = 0
	assert.ErrorContains(t, bad.Validate(), "reconcile interval")

	// With HA disabled the HA fields are not validated at all: a
	// single-replica deployment must not have to care about them.
	off := testConfig(t)
	off.HAEnabled = false
	off.InstanceID = ""
	off.LeaderKey = ""
	require.NoError(t, off.Validate())
}
