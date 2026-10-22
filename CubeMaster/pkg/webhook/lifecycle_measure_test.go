// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// Measures the added preparation + success capture/publication, not business
// RPCs or background mapping. No timing threshold: distributions are evidence.
func TestP4CaptureDistribution(t *testing.T) {
	req := &types.CreateCubeSandboxReq{InstanceType: "cubebox", WebhookMetadata: UserMetadata(map[string]string{"user_token": strings.Repeat("v", 4096), "project": strings.Repeat("p", 4096)}), Containers: []*types.Container{{Resources: &types.Resource{Cpu: "1", Mem: "1Gi"}}}}
	for _, kind := range []string{TypeCreated, TypeResumed, TypePaused, TypeCheckpointed} {
		for _, scenario := range []string{"disabled", "enabled", "full", "stopped", "disconnected", "slow-store", "slow-http"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				var p Publisher = NopPublisher{}
				if scenario == "full" || scenario == "stopped" {
					b, e := NewBufferedPublisher(1)
					require.NoError(t, e)
					p = b
					if scenario == "stopped" {
						b.Stop()
					}
				}
				if scenario == "disconnected" {
					cfg := config.DefaultWebhookConf()
					cfg.Enabled = true
					cfg.Store.OperationTimeout = 50 * time.Millisecond
					cfg.Subscriptions = []config.WebhookSubscription{{ID: "measurement", Enabled: true, URL: "https://example.invalid/hook", Events: []string{kind}}}
					r, e := NewRuntime(context.Background(), cfg, &config.RedisConf{Nodes: "127.0.0.1:1"})
					require.NoError(t, e)
					p = r.Publisher()
					defer func() {
						ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
						defer cancel()
						r.Shutdown(ctx)
					}()
				}

				if scenario == "enabled" || scenario == "slow-store" || scenario == "slow-http" {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if scenario == "slow-http" {
							time.Sleep(200 * time.Millisecond)
						}
						w.WriteHeader(204)
					}))
					defer srv.Close()
					cfg := localConf()
					cfg.Subscriptions = []Subscription{{"measure", true, srv.URL, []string{kind}}}
					options := testOptions()
					options.MaxRecords = 2000
					store, _ := newTestStore(t, options)
					var delivery DeliveryStore = store
					if scenario == "slow-store" {
						delivery = &faultStore{DeliveryStore: store, put: func(ctx context.Context, task DeliveryTask) error {
							if !pause(ctx, 200*time.Millisecond) {
								return ErrStoreUncertain
							}
							return store.Put(ctx, task)
						}}
					}
					r := runtimeFor(t, cfg, delivery)
					p = r.Publisher()
					defer func() {
						ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
						defer cancel()
						r.Shutdown(ctx)
					}()
				}
				var h PublisherHolder
				h.Set(p)
				samples := make([]int64, 1000)
				for i := range samples {
					start := time.Now()
					if kind == TypeCreated {
						req.WebhookMetadata = UserMetadata(req.WebhookMetadata.Values)
					}
					template, f := PrepareLifecycle(req)
					if kind == TypePaused {
						f.PauseMode = "full"
					}
					if kind == TypeResumed {
						f.ResumeMode = "restore"
					}
					h.Publish(kind, "sb", template, f)
					samples[i] = time.Since(start).Nanoseconds()
				}
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				t.Logf("n=1000 metadata=8209B p50=%dns p95=%dns p99=%dns", samples[499], samples[949], samples[989])
			})
		}
	}
}
