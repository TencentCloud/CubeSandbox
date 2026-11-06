// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package rediskey

import (
	"strings"
	"testing"
)

func TestWebhookDeliveryKeys(t *testing.T) {
	a, b, c := WebhookDelivery()
	seen := map[string]bool{}
	for _, key := range []string{a, b, c} {
		if seen[key] || !strings.HasPrefix(key, "cube:v1:master:webhook:") || !strings.HasSuffix(key, "{delivery}") {
			t.Fatalf("invalid delivery key %q", key)
		}
		seen[key] = true
	}
}
