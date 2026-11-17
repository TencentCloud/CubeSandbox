// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package app

import (
	"context"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWebhookShutdownDisabledStillCancels(t *testing.T) {
	for _, c := range []config.WebhookConf{{}, {Invalid: true, Enabled: true}} {
		a := New()
		a.startWebhooks(context.Background(), &config.Config{Webhooks: c})
		ctx, cancel := context.WithCancel(context.Background())
		a.shutdownWebhooksAndCancel(cancel)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	}
}
func TestWebhookShutdownCancelsBlockedHTTPAndApp(t *testing.T) {
	received := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- struct{}{}
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	defer srv.Close()
	c := config.DefaultWebhookConf()
	c.Enabled = true
	c.AllowHTTP = true
	c.RequestTimeout = 30 * time.Second
	c.Workers = 1
	c.AllowedCIDRs = []string{"127.0.0.0/8"}
	c.Subscriptions = []config.WebhookSubscription{{ID: "app-shutdown", Enabled: true, URL: srv.URL, Events: []string{webhook.TypeCreated}}}
	a := New()
	a.startWebhooks(context.Background(), &config.Config{Webhooks: c})
	defer a.webhooks.Shutdown(context.Background())
	event, err := webhook.Capture(webhook.CaptureInput{ID: uuid.New(), Type: webhook.TypeCreated, Origin: webhook.OriginLifecycle, SandboxID: "app-shutdown", OccurredAt: time.Now()})
	require.NoError(t, err)
	require.Equal(t, webhook.PublishAccepted, a.WebhookPublisher().TryPublish(event).Status)
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP not entered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.shutdownWebhooksAndCancel(cancel) }()
	}
	wg.Wait()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Less(t, time.Since(start), 2500*time.Millisecond)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("HTTP was not cancelled")
	}
	require.Equal(t, webhook.PublishStopped, a.WebhookPublisher().TryPublish(event).Status)
}
