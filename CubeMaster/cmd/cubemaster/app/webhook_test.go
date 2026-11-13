// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package app

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func webhookCounter(t *testing.T, op, result string) float64 {
	all, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, f := range all {
		if f.GetName() != "cubemaster_webhook_operations_total" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["operation"] == op && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}
func TestAppWebhookOwnership(t *testing.T) {
	defer sandbox.SetWebhookPublisher(nil)
	defer task.SetWebhookPublisher(nil)
	defer templatecenter.SetWebhookPublisher(nil)
	a := New()
	require.IsType(t, webhook.NopPublisher{}, a.WebhookPublisher())
	empty := config.DefaultWebhookConf()
	empty.Enabled = true
	invalid := empty
	invalid.Workers = 0
	for _, c := range []config.WebhookConf{{}, config.DefaultWebhookConf(), {Invalid: true, Enabled: true}, empty, invalid} {
		a.startWebhooks(context.Background(), &config.Config{Webhooks: c})
		require.IsType(t, webhook.NopPublisher{}, a.WebhookPublisher())
		var holder webhook.PublisherHolder
		holder.Set(a.WebhookPublisher())
		require.False(t, holder.Enabled())
		before := webhookCounter(t, "capture", "accepted")
		holder.Publish(webhook.TypeCreated, "sb", "", webhook.LifecycleFields{})
		require.Equal(t, before, webhookCounter(t, "capture", "accepted"))
		a.webhooks.Shutdown(context.Background())
	}
	sent := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent <- struct{}{}; w.WriteHeader(204) }))
	defer srv.Close()
	c := config.DefaultWebhookConf()
	c.Enabled = true
	c.AllowHTTP = true
	c.AllowedCIDRs = []string{"127.0.0.0/8"}
	c.Subscriptions = []config.WebhookSubscription{{ID: "app", Enabled: true, URL: srv.URL, Events: []string{webhook.TypeCreated}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startWebhooks(ctx, &config.Config{Webhooks: c})
	require.Same(t, a.webhooks, a.WebhookPublisher())
	var holder webhook.PublisherHolder
	holder.Set(a.WebhookPublisher())
	require.True(t, holder.Enabled())
	before := webhookCounter(t, "ingress", "accepted")
	holder.Publish(webhook.TypeCreated, "sb", "", webhook.LifecycleFields{})
	require.Equal(t, before+1, webhookCounter(t, "ingress", "accepted"))
	select {
	case <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("App holder not delivered")
	}
	cancel()
	a.webhooks.Shutdown(context.Background())
	require.Equal(t, webhook.PublishStopped, a.WebhookPublisher().TryPublish(nil).Status)
}
