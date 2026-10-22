// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package webhooktest connects real lifecycle tests to an opt-in disposable Redis.
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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func Runtime(t *testing.T, db int, events ...string) (*webhook.Runtime, <-chan []byte) {
	t.Helper()
	addr := os.Getenv("WEBHOOK_P4_REDIS_ADDR")
	if addr == "" {
		t.Skip("WEBHOOK_P4_REDIS_ADDR not set: isolated business pipeline skipped")
	}

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
	r, e := webhook.NewRuntime(context.Background(), c, &config.RedisConf{Nodes: addr, Password: os.Getenv("WEBHOOK_P4_REDIS_PASSWORD"), DbNo: db})
	require.NoError(t, e)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r.Shutdown(ctx)
	})
	return r, bodies
}

// FaultPublishers runs local ingress and bounded backend failure scenarios.
// The slow peer is a Redis-protocol blackhole, not a slow real Redis claim.
func FaultPublishers(t *testing.T, fn func(*testing.T, webhook.Publisher)) {
	t.Helper()
	for _, name := range []string{"disabled", "full", "stopped", "disconnected-redis", "slow-redis", "slow-http", "shutdown"} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "disabled":
				fn(t, webhook.NopPublisher{})
				return
			case "full", "stopped":
				p, e := webhook.NewBufferedPublisher(1)
				require.NoError(t, e)
				if name == "stopped" {
					p.Stop()
				} else {
					c, e := webhook.Capture(webhook.CaptureInput{ID: uuid.New(), Type: webhook.TypeCreated, Origin: webhook.OriginLifecycle, SandboxID: "filler", OccurredAt: time.Now()})
					require.NoError(t, e)
					p.TryPublish(c)
				}
				fn(t, p)
				return
			}
			c := config.DefaultWebhookConf()
			c.Enabled = true
			c.IngressCapacity = 1
			c.RequestTimeout = 30 * time.Second
			c.Lease = time.Minute
			c.Store.OperationTimeout = 10 * time.Second
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			addr := "127.0.0.1:1"
			if name == "slow-redis" {
				listener, e := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, e)
				addr = listener.Addr().String()
				stop := make(chan struct{})
				done := make(chan struct{})
				go func() {
					defer close(done)
					for {
						conn, e := listener.Accept()
						if e != nil {
							return
						}
						go func() {
							defer conn.Close()
							var b [1]byte
							if _, err := conn.Read(b[:]); err == nil {
								select {
								case entered <- struct{}{}:
								default:
								}
							}
							<-stop
						}()
					}
				}()
				defer func() { close(stop); listener.Close(); <-done }()
			}
			if name == "slow-http" || name == "shutdown" {
				addr = os.Getenv("WEBHOOK_P4_REDIS_ADDR")
				if addr == "" {
					t.Skip("isolated Redis not configured")
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
				w.WriteHeader(204)
			}))
			defer srv.Close()
			defer unblock()
			c.AllowHTTP = true
			c.AllowedCIDRs = []string{"127.0.0.0/8"}
			c.Subscriptions = []config.WebhookSubscription{{ID: uuid.NewString(), Enabled: true, URL: srv.URL, Events: []string{webhook.TypeCreated, webhook.TypeResumed, webhook.TypePaused, webhook.TypeCheckpointed}}}
			r, e := webhook.NewRuntime(context.Background(), c, &config.RedisConf{Nodes: addr, Password: os.Getenv("WEBHOOK_P4_REDIS_PASSWORD"), DbNo: 12})
			require.NoError(t, e)
			defer func() {
				unblock()
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				defer cancel()
				r.Shutdown(ctx)
			}()
			// The primer only establishes a blocked backend. Lifecycle assertions
			// below still invoke real business functions through fn.
			primer, err := webhook.Capture(webhook.CaptureInput{ID: uuid.New(), Type: webhook.TypeCreated, Origin: webhook.OriginLifecycle, SandboxID: "fault-primer", OccurredAt: time.Now()})
			require.NoError(t, err)
			require.Equal(t, webhook.PublishAccepted, r.Publisher().TryPublish(primer).Status)
			if name != "disconnected-redis" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("backend was never entered")
				}
			}
			var shutdownDone chan struct{}
			if name == "shutdown" {
				shutdownDone = make(chan struct{})
				go func() { r.Shutdown(context.Background()); close(shutdownDone) }()
				require.Eventually(t, func() bool {
					return r.Publisher().TryPublish(primer).Status == webhook.PublishStopped
				}, time.Second, time.Millisecond)
				select {
				case <-shutdownDone:
					t.Fatal("shutdown finished before business call")
				default:
				}
			}
			start := time.Now()
			fn(t, r.Publisher())
			// Backend waits are 10/30 seconds. This is a deadlock guard, not a
			// performance threshold: the business must finish before release.
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
