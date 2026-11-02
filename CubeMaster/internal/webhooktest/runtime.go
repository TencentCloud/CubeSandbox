// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package webhooktest connects lifecycle tests to an in-memory Runtime and HTTP.
package webhooktest

import (
	"bytes"
	"context"
	"encoding/json"
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

func Runtime(t *testing.T, events ...string) (*webhook.Runtime, <-chan []byte) {
	t.Helper()

	bodies := make(chan []byte, 64)
	var mu sync.Mutex
	previous := map[string][]byte{}
	attempts := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, e := io.ReadAll(r.Body)
		require.NoError(t, e)
		var payload struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(b, &payload))
		require.NotEmpty(t, payload.ID)
		attempt := r.Header.Get("e2b-delivery-id")
		require.NotEmpty(t, attempt)
		mu.Lock()
		defer mu.Unlock()
		if old, ok := previous[payload.ID]; ok {
			require.True(t, bytes.Equal(old, b), "retry body changed")
			require.NotEqual(t, attempts[payload.ID], attempt)
			bodies <- b
			w.WriteHeader(204)
		} else {
			previous[payload.ID] = b
			attempts[payload.ID] = attempt
			w.WriteHeader(503)
		}
	}))
	t.Cleanup(srv.Close)
	c := config.DefaultWebhookConf()
	c.Enabled = true
	c.AllowHTTP = true
	c.AllowedCIDRs = []string{"127.0.0.0/8"}
	c.Subscriptions = []config.WebhookSubscription{{ID: uuid.NewString(), Enabled: true, URL: srv.URL, Events: events}}
	r, e := webhook.NewRuntime(context.Background(), c)
	require.NoError(t, e)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r.Shutdown(ctx)
	})
	return r, bodies
}

// FaultPublishers exercises bounded admission and slow/failing notification HTTP.
func FaultPublishers(t *testing.T, fn func(*testing.T, webhook.Publisher)) {
	t.Helper()
	for _, name := range []string{"disabled", "full", "stopped", "task-capacity", "failed-http", "slow-http", "shutdown"} {
		t.Run(name, func(t *testing.T) {
			if name == "disabled" {
				r, err := webhook.NewRuntime(context.Background(), config.DefaultWebhookConf())
				require.NoError(t, err)
				defer r.Shutdown(context.Background())
				fn(t, r.Publisher())
				return
			}
			if name == "full" || name == "stopped" {
				p, err := webhook.NewBufferedPublisher(1)
				require.NoError(t, err)
				if name == "stopped" {
					p.Stop()
				} else {
					event, err := webhook.Capture(webhook.CaptureInput{ID: uuid.New(), Type: webhook.TypeCreated, Origin: webhook.OriginLifecycle, SandboxID: "filler", OccurredAt: time.Now()})
					require.NoError(t, err)
					p.TryPublish(event)
				}
				fn(t, p)
				return
			}
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case entered <- struct{}{}:
				default:
				}
				if name == "failed-http" {
					w.WriteHeader(503)
					return
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
				w.WriteHeader(204)
			}))
			defer srv.Close()
			defer unblock()
			c := config.DefaultWebhookConf()
			c.Enabled = true
			c.AllowHTTP = true
			c.IngressCapacity = 1
			c.Workers = 1
			c.RequestTimeout = 30 * time.Second
			if name == "task-capacity" {
				c.Store.MaxRecords = 1
			}
			c.AllowedCIDRs = []string{"127.0.0.0/8"}
			c.Subscriptions = []config.WebhookSubscription{{ID: uuid.NewString(), Enabled: true, URL: srv.URL, Events: []string{webhook.TypeCreated, webhook.TypeResumed, webhook.TypePaused, webhook.TypeCheckpointed, webhook.TypeUpdated, webhook.TypeKilled}}}
			r, err := webhook.NewRuntime(context.Background(), c)
			require.NoError(t, err)
			defer func() {
				unblock()
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				defer cancel()
				r.Shutdown(ctx)
			}()
			primer, err := webhook.Capture(webhook.CaptureInput{ID: uuid.New(), Type: webhook.TypeCreated, Origin: webhook.OriginLifecycle, SandboxID: "fault-primer", OccurredAt: time.Now()})
			require.NoError(t, err)
			require.Equal(t, webhook.PublishAccepted, r.Publisher().TryPublish(primer).Status)
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP never entered")
			}
			var shutdownDone chan struct{}
			if name == "shutdown" {
				shutdownDone = make(chan struct{})
				go func() { r.Shutdown(context.Background()); close(shutdownDone) }()
				require.Eventually(t, func() bool { return r.Publisher().TryPublish(primer).Status == webhook.PublishStopped }, time.Second, time.Millisecond)
			}
			if shutdownDone != nil {
				select {
				case <-shutdownDone:
					t.Fatal("shutdown finished before business call")
				default:
				}
			}
			start := time.Now()
			fn(t, r.Publisher())
			// Deadlock guard against the held 30-second HTTP request.
			require.Less(t, time.Since(start), 5*time.Second)
			if shutdownDone != nil {
				select {
				case <-shutdownDone:
					t.Fatal("shutdown no longer overlapped business call")
				default:
				}
			}
		})
	}
}
