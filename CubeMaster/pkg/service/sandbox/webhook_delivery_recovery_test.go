// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

// This is a real UpdateNetwork -> Publisher -> Redis -> HTTP component test.
// Node/spec dependencies are controlled by the P5 fixture; restarting Runtime
// below does not claim to restart a full Master or exercise a real node.
func TestP6BusinessLostHTTPAndRestart(t *testing.T) {
	addr := os.Getenv("WEBHOOK_P4_REDIS_ADDR")
	if addr == "" {
		t.Skip("dedicated Redis not configured")
	}
	for _, revoke := range []bool{false, true} {
		t.Run(strconv.FormatBool(revoke), func(t *testing.T) {
			p5Setup(t)
			defer SetWebhookPublisher(nil)
			type request struct {
				body                                []byte
				delivery, subscription, contentType string
			}
			received := make(chan request, 8)
			release := make(chan struct{})
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return
				}
				received <- request{body, r.Header.Get("e2b-delivery-id"), r.Header.Get("e2b-webhook-id"), r.Header.Get("Content-Type")}
				if calls.Add(1) == 1 {
					<-release // receiver accepted bytes but never gives the first HTTP reply
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				w.WriteHeader(204)
			}))
			defer srv.Close()
			defer close(release)
			c := config.DefaultWebhookConf()
			c.Enabled, c.AllowHTTP = true, true
			c.AllowedCIDRs = []string{"127.0.0.0/8"}
			c.Workers = 1
			sub := uuid.NewString()
			c.Subscriptions = []config.WebhookSubscription{{ID: sub, Enabled: true, URL: srv.URL, Events: []string{webhook.TypeUpdated}}}
			rc := &config.RedisConf{Nodes: addr, Password: os.Getenv("WEBHOOK_P4_REDIS_PASSWORD"), DbNo: 7}
			start := func() *webhook.Runtime {
				r, err := webhook.NewRuntime(context.Background(), c, rc)
				require.NoError(t, err)
				t.Cleanup(func() { r.Shutdown(context.Background()) })
				SetWebhookPublisher(r.Publisher())
				return r
			}
			first := start()
			require.Equal(t, 200, UpdateNetwork(context.Background(), p5Network()).Ret.RetCode)
			var original request
			select {
			case original = <-received:
			case <-time.After(10 * time.Second):
				t.Fatal("no initial HTTP")
			}
			require.Equal(t, sub, original.subscription)
			require.Equal(t, "application/json", original.contentType)
			require.NotEmpty(t, original.delivery)
			var event struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			require.NoError(t, json.Unmarshal(original.body, &event))
			require.Equal(t, webhook.TypeUpdated, event.Type)
			// Business returned while the receiver is still held. Force bounded shutdown
			// with a shorter caller deadline, leaving the accepted request unconfirmed.
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			first.Shutdown(ctx)
			cancel()
			require.EqualValues(t, 1, calls.Load())
			conn, err := redis.Dial("tcp", addr, redis.DialPassword(rc.Password), redis.DialDatabase(rc.DbNo))
			require.NoError(t, err)
			defer conn.Close()
			tasks, _, _ := rediskey.WebhookDelivery()
			key := event.ID + ":" + strconv.Itoa(len(sub)) + ":" + sub
			saved := func() (string, int) {
				raw, err := redis.Bytes(conn.Do("HGET", tasks, key))
				require.NoError(t, err)
				var record struct {
					State    string `json:"state"`
					Attempts int    `json:"attempts"`
				}
				require.NoError(t, json.Unmarshal(raw, &record))
				return record.State, record.Attempts
			}
			state, attempts := saved()
			require.Contains(t, []string{"pending", "inflight"}, state)
			require.Equal(t, 1, attempts)
			if revoke {
				c.Subscriptions = nil
			}
			second := start()
			want := "succeeded"
			if revoke {
				want = "cancelled"
			}
			require.Eventually(t, func() bool { s, _ := saved(); return s == want }, 25*time.Second, 20*time.Millisecond)
			second.Shutdown(context.Background())
			_, attempts = saved()
			require.Equal(t, 2, attempts, "restart must preserve consumed attempt")
			if revoke {
				require.EqualValues(t, 1, calls.Load(), "revoked backlog must not send")
			} else {
				require.EqualValues(t, 2, calls.Load())
				next := <-received
				require.Equal(t, original.body, next.body)
				require.NotEqual(t, original.delivery, next.delivery)
				require.Equal(t, sub, next.subscription)
			}
		})
	}
}
