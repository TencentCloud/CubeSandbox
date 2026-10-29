// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

func TestWebhookShutdownDisabledStillCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	New().shutdownWebhooksAndCancel(cancel)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// Component evidence: actual Runtime/Redis/HTTP, not a complete Master or VM.
func TestWebhookShutdownSavesInflightBeforeAppCancellation(t *testing.T) {
	addr := os.Getenv("WEBHOOK_P4_REDIS_ADDR")
	if addr == "" {
		t.Skip("dedicated Redis required")
	}
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- struct{}{}
		<-release
	}))
	defer srv.Close()
	defer close(release)
	c := config.DefaultWebhookConf()
	c.Enabled, c.AllowHTTP, c.Workers = true, true, 1
	c.AllowedCIDRs = []string{"127.0.0.0/8"}
	c.RequestTimeout = time.Second
	sub := uuid.NewString()
	c.Subscriptions = []config.WebhookSubscription{{ID: sub, Enabled: true, URL: srv.URL, Events: []string{webhook.TypeCreated}}}
	rc := &config.RedisConf{Nodes: addr, Password: os.Getenv("WEBHOOK_P4_REDIS_PASSWORD"), DbNo: 14}
	runtime, err := webhook.NewRuntime(context.Background(), c, rc)
	require.NoError(t, err)
	defer runtime.Shutdown(context.Background())
	eventID := uuid.New()
	event, err := webhook.Capture(webhook.CaptureInput{ID: eventID, Type: webhook.TypeCreated,
		Origin: webhook.OriginLifecycle, SandboxID: "app-webhook-shutdown", OccurredAt: time.Now()})
	require.NoError(t, err)
	require.Equal(t, webhook.PublishAccepted, runtime.TryPublish(event).Status)
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("no real inflight HTTP")
	}
	conn, err := redis.Dial("tcp", addr, redis.DialPassword(rc.Password), redis.DialDatabase(rc.DbNo))
	require.NoError(t, err)
	defer conn.Close()
	tasks, _, _ := rediskey.WebhookDelivery()
	key := eventID.String() + ":" + strconv.Itoa(len(sub)) + ":" + sub
	read := func() (string, int) {
		raw, err := redis.Bytes(conn.Do("HGET", tasks, key))
		require.NoError(t, err)
		var saved struct {
			State    string `json:"state"`
			Attempts int    `json:"attempts"`
		}
		require.NoError(t, json.Unmarshal(raw, &saved))
		return saved.State, saved.Attempts
	}
	state, attempts := read()
	require.Equal(t, "inflight", state)
	require.Equal(t, 1, attempts)
	a := &App{webhooks: runtime}
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	a.shutdownWebhooksAndCancel(func() {
		state, attempts := read()
		require.Equal(t, "pending", state, "save the webhook result before existing App cancellation")
		require.Equal(t, 1, attempts)
		require.Equal(t, webhook.PublishStopped, a.WebhookPublisher().TryPublish(event).Status)
		cancel()
	})
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Less(t, time.Since(started), 5*time.Second)
}
