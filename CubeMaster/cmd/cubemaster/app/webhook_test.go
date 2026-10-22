// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

func TestAppWebhookOwnership(t *testing.T) {
	a := New()
	require.Equal(t, webhook.PublishDisabled, a.WebhookPublisher().TryPublish(nil).Status)
	a.startWebhooks(context.Background(), &config.Config{})
	require.Equal(t, webhook.PublishDisabled, a.WebhookPublisher().TryPublish(nil).Status)
	c := config.DefaultWebhookConf()
	c.Enabled = true
	ctx, cancel := context.WithCancel(context.Background())
	a.startWebhooks(ctx, &config.Config{Webhooks: c, RedisConf: &config.RedisConf{Nodes: "127.0.0.1:1"}})
	require.Equal(t, webhook.PublishInvalid, a.WebhookPublisher().TryPublish(nil).Status)
	cancel()
	done := make(chan struct{})
	go func() { a.webhooks.Shutdown(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("App did not await bounded runtime shutdown")
	}
	require.Equal(t, webhook.PublishStopped, a.WebhookPublisher().TryPublish(nil).Status)
}
