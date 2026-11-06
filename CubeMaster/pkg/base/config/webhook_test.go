// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestWebhookConfigIsolation(t *testing.T) {
	for _, block := range []string{"enabled: null", "subscriptions: [{id: 123, url: https://x.test, events: [sandbox.lifecycle.created]}]", "enabled: token-secret", "workers: nope", "workers: 0", "workers: 99999999", "request_timeout: forever", "request_timeout: 0s", "lease: 1s", "secret: token-secret", "secret_env: token-secret", "subscriptions: nope", "subscriptions: [{id: one, url: 'https://x.test', events: []}]", "store: {backend: unknown}", "store: {operation_timeout: 1h}", "store: {redis: {nodes: new.test, password: secret, unknown: bad}}", "subscriptions: [{id: one, url: 'https://u:token-secret@x.test', events: [sandbox.lifecycle.created]}]", "subscriptions: [{id: one, url: 'https://x.test', events: [wrong]}]"} {
		t.Run(block, func(t *testing.T) {
			var c Config
			require.NoError(t, yaml.Unmarshal([]byte("common: {http_port: 8089}\nwebhooks:\n  "+block+"\n"), &c))
			require.True(t, c.Webhooks.Invalid)
			require.False(t, c.Webhooks.Enabled)
			require.Equal(t, 8089, c.Common.HttpPort)
			b, _ := json.Marshal(c)
			require.NotContains(t, string(b), "token-secret")
			require.NotContains(t, fmt.Sprintf("%+v", c), "token-secret")
		})
	}
	var c Config
	require.NoError(t, yaml.Unmarshal([]byte("webhooks: {}"), &c))
	require.False(t, c.Webhooks.Enabled)
	require.Equal(t, DefaultWebhookConf().Workers, c.Webhooks.Workers)
	good := "webhooks:\n  enabled: true\n  subscriptions:\n    - id: one\n      url: https://example.test/path?token=token-secret\n      events: [sandbox.lifecycle.created]\n"
	require.NoError(t, yaml.Unmarshal([]byte(good), &c))
	require.NoError(t, c.Webhooks.Validate())
	require.False(t, c.Webhooks.Subscriptions[0].Enabled)
	raw, err := yaml.Marshal(c)
	require.NoError(t, err)
	require.Contains(t, string(raw), "token-secret", "watcher comparison must retain actual configuration")
	b, _ := json.Marshal(c)
	require.NotContains(t, string(b), "token-secret")
	c.Webhooks.Subscriptions = append(c.Webhooks.Subscriptions, c.Webhooks.Subscriptions[0])
	require.Error(t, c.Webhooks.Validate())
	c.Webhooks.Subscriptions = make([]WebhookSubscription, 65)
	require.Error(t, c.Webhooks.Validate())
	for _, u := range []string{"https://127.1", "https://2130706433", "https://0x7f000001", "https://127.000.0.1", "http://example.test", "https://x.test/#token-secret", "https://x.test:99999", "https://x.test:", "https://[fe80::1%25eth0]", "//example.test", "https://u:secret@example.test", "https://x.test/\n"} {
		_, err := ValidateWebhookURL(u, false)
		require.Error(t, err, u)
	}
}
func TestWebhookConfigProcessHelper(t *testing.T) {
	mode := os.Getenv("P3_CONFIG_HELPER")
	if mode == "" {
		return
	}
	c, err := Init()
	if mode == "syntax" || mode == "other-type" {
		require.Error(t, err)
		return
	}
	require.NoError(t, err)
	require.True(t, c.Webhooks.Invalid)
	if mode == "listener-fatal" {
		c.ExtraConf.BlkQos = "invalid-json"
		(&listener{}).OnEvent(c)
		t.Fatal("expected existing Fatal policy")
	}
	var next Config
	raw, err := os.ReadFile(os.Getenv("CUBE_MASTER_CONFIG_PATH"))
	require.NoError(t, err)
	raw = []byte(strings.ReplaceAll(string(raw), "enabled: token-secret", "workers: token-secret"))
	require.NoError(t, yaml.Unmarshal(raw, &next))
	(&listener{}).OnEvent(&next)
	require.True(t, GetConfig().Webhooks.Invalid)
}
func TestWebhookActualInitAndListener(t *testing.T) {
	fixture := []byte("common: {http_port: 8089}\nlog: {level: info}\n")
	for _, mode := range []string{"valid-main", "syntax", "other-type", "listener-fatal"} {
		t.Run(mode, func(t *testing.T) {
			body := string(fixture) + "\nwebhooks:\n  enabled: token-secret\n  store:\n    redis:\n      nodes: localhost:1234\n      password: redis-secret-injected\n"
			if mode == "syntax" {
				body += "broken: [\n"
			}
			if mode == "other-type" {
				body = "common: {http_port: nope}\n"
			}
			file := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(file, []byte(body), 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestWebhookConfigProcessHelper$")
			cmd.Env = append(os.Environ(), "P3_CONFIG_HELPER="+mode, "CUBE_MASTER_CONFIG_PATH="+file)
			out, err := cmd.CombinedOutput()
			if mode == "listener-fatal" {
				require.Error(t, err, string(out))
			} else {
				require.NoError(t, err, string(out))
			}
			require.NotContains(t, string(out), "token-secret")
			require.NotContains(t, string(out), "redis-secret-injected")
		})
	}
}
