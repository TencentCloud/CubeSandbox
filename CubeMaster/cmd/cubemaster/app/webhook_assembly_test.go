// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package app

import (
	"context"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

func TestAppDisabledWebhooksDoNotConstructDependencies(t *testing.T) {
	defer sandbox.SetWebhookPublisher(nil)
	defer task.SetWebhookPublisher(nil)
	defer templatecenter.SetWebhookPublisher(nil)
	empty := config.DefaultWebhookConf()
	empty.Enabled = true
	invalid := empty
	invalid.Workers = 0
	off := empty
	off.Subscriptions = []config.WebhookSubscription{{ID: "off", URL: "https://example.test", Events: []string{webhook.TypeCreated}}}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(webhook.NewMemoryDeliveryStore, func(int) (*webhook.MemoryDeliveryStore, error) { panic("disabled store construction") })
	patches.ApplyFunc(webhook.NewSender, func(config.WebhookConf) *webhook.Sender { panic("disabled sender construction") })
	patches.ApplyFunc(webhook.NewRuntime, func(context.Context, webhook.RuntimeOptions, webhook.SubscriptionProvider, webhook.DeliveryStore, webhook.DeliverySender) (*webhook.Runtime, error) {
		panic("disabled worker construction")
	})
	for _, c := range []config.WebhookConf{{}, config.DefaultWebhookConf(), {Enabled: true, Invalid: true}, empty, invalid, off} {
		a := New()
		a.startWebhooks(context.Background(), &config.Config{Webhooks: c})
		require.Nil(t, a.webhooks)
		require.IsType(t, webhook.NopPublisher{}, a.WebhookPublisher())
	}
}

func TestAppWebhookConstructionFailureClosesCallerResources(t *testing.T) {
	defer sandbox.SetWebhookPublisher(nil)
	defer task.SetWebhookPublisher(nil)
	defer templatecenter.SetWebhookPublisher(nil)
	c := config.DefaultWebhookConf()
	c.Enabled = true
	c.Subscriptions = []config.WebhookSubscription{{ID: "enabled", Enabled: true, URL: "https://example.test", Events: []string{webhook.TypeCreated}}}
	store, err := webhook.NewMemoryDeliveryStore(c.Store.MaxRecords)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = store.Close(context.Background()) })
	closed := 0
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(webhook.NewMemoryDeliveryStore, func(int) (*webhook.MemoryDeliveryStore, error) {
		return store, nil
	})
	patches.ApplyFunc((*webhook.Sender).Close, func(*webhook.Sender) { closed++ })
	a := New()
	// A wide NewRuntime patch corrupts its store argument on arm64 (R10).
	// Use the real constructor's nil-parent rejection instead.
	a.startWebhooks(nil, &config.Config{Webhooks: c})
	require.Nil(t, a.webhooks)
	require.IsType(t, webhook.NopPublisher{}, a.WebhookPublisher())
	_, err = store.Claim(context.Background(), time.Now())
	require.ErrorIs(t, err, webhook.ErrDeliveryClosed)
	require.Equal(t, 1, closed)
}
