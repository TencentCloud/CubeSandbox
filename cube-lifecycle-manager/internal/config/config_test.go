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

func TestWebhookDefaultsDisabled(t *testing.T) {
	cfg := Default()
	assert.Empty(t, cfg.WebhookURLs)
	assert.Empty(t, cfg.WebhookEvents)
	assert.Empty(t, cfg.WebhookSecret)
	assert.Equal(t, 10*time.Second, cfg.WebhookTimeout)
	assert.Equal(t, 2, cfg.WebhookMaxRetries)
}

func TestWebhookLoadFromEnv(t *testing.T) {
	t.Setenv("CUBE_LCM_WEBHOOK_URLS", "http://host-a:8080/hook, http://host-b:8080/hook")
	t.Setenv("CUBE_LCM_WEBHOOK_EVENTS", "sandbox.paused,sandbox.resumed")
	t.Setenv("CUBE_LCM_WEBHOOK_SECRET", "s3cret")
	t.Setenv("CUBE_LCM_WEBHOOK_TIMEOUT", "15s")
	t.Setenv("CUBE_LCM_WEBHOOK_MAX_RETRIES", "5")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"http://host-a:8080/hook", "http://host-b:8080/hook"}, cfg.WebhookURLs)
	assert.Equal(t, []string{"sandbox.paused", "sandbox.resumed"}, cfg.WebhookEvents)
	assert.Equal(t, "s3cret", cfg.WebhookSecret)
	assert.Equal(t, 15*time.Second, cfg.WebhookTimeout)
	assert.Equal(t, 5, cfg.WebhookMaxRetries)
	require.NoError(t, cfg.Validate())
}

func TestWebhookLoadRejectsBadDuration(t *testing.T) {
	t.Setenv("CUBE_LCM_WEBHOOK_TIMEOUT", "not-a-duration")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUBE_LCM_WEBHOOK_TIMEOUT")
}

func TestWebhookValidateKnobsOnlyWhenEnabled(t *testing.T) {
	// Webhook disabled: zero tuning knobs are fine.
	cfg := testConfig(t)
	require.NoError(t, cfg.Validate())

	// Enabled with a non-positive timeout is rejected.
	cfg.WebhookURLs = []string{"http://host/hook"}
	cfg.WebhookTimeout = 0
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "webhook timeout")

	// Enabled with a negative retry budget is rejected.
	cfg.WebhookTimeout = 10 * time.Second
	cfg.WebhookMaxRetries = -1
	err = cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "webhook max retries")
}
