// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"testing"

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
