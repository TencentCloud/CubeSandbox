// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// UpdateNetwork uses the real in-memory notification pipeline. Node/spec calls
// retain the existing controlled fixture; this is not a full Master/node test.
func TestNetworkNotificationRetryAndRestartLoss(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "restart-loses-backlog"}[restart], func(t *testing.T) {
			p5Setup(t)
			defer SetWebhookPublisher(nil)
			type request struct {
				body                          []byte
				id, subscription, contentType string
			}
			received := make(chan request, 8)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				received <- request{body, r.Header.Get("e2b-delivery-id"), r.Header.Get("e2b-webhook-id"), r.Header.Get("Content-Type")}
				if calls.Add(1) == 1 {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(204)
			}))
			defer srv.Close()
			c := config.DefaultWebhookConf()
			c.Enabled = true
			c.AllowHTTP = true
			c.Workers = 1
			c.RequestTimeout = 150 * time.Millisecond
			c.MaxAttempts = 2
			c.AllowedCIDRs = []string{"127.0.0.0/8"}
			c.Subscriptions = []config.WebhookSubscription{{ID: "network", Enabled: true, URL: srv.URL, Events: []string{webhook.TypeUpdated}}}
			start := func() *webhook.Runtime {
				r, err := webhook.NewRuntime(context.Background(), c)
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
			case <-time.After(3 * time.Second):
				t.Fatal("missing HTTP")
			}
			require.Equal(t, "network", original.subscription)
			require.Equal(t, "application/json", original.contentType)
			require.NotEmpty(t, original.id)
			var event struct{ ID, Type string }
			require.NoError(t, json.Unmarshal(original.body, &event))
			require.Equal(t, webhook.TypeUpdated, event.Type)
			if restart {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				first.Shutdown(ctx)
				second := start()
				second.Shutdown(context.Background())
				require.EqualValues(t, 1, calls.Load())
				require.Empty(t, received)
			} else {
				select {
				case next := <-received:
					require.Equal(t, original.body, next.body)
					require.NotEqual(t, original.id, next.id)
					require.Equal(t, original.subscription, next.subscription)
				case <-time.After(4 * time.Second):
					t.Fatal("retry missing")
				}
				first.Shutdown(context.Background())
				require.EqualValues(t, 2, calls.Load())
			}
		})
	}
}
