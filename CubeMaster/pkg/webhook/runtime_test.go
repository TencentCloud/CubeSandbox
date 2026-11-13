// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"gopkg.in/yaml.v3"
)

func runtimeCaptured(t *testing.T) *CapturedEvent {
	t.Helper()
	c, err := Capture(CaptureInput{ID: uuid.New(), Type: TypeCreated, Origin: OriginLifecycle, SandboxID: "synthetic", OccurredAt: time.Now()})
	require.NoError(t, err)
	return c
}
func runtimeFor(t *testing.T, c config.WebhookConf, s DeliveryStore) *Runtime {
	t.Helper()
	p, err := NewStaticConfigSubscriptionProvider(c)
	require.NoError(t, err)
	r := startRuntime(c, p, s, NewSender(c))
	t.Cleanup(func() { r.Shutdown(testCtx) })
	return r
}
func metricValue(t *testing.T, op, result string) float64 {
	t.Helper()
	m := &dto.Metric{}
	require.NoError(t, deliveryCounts.WithLabelValues(op, result).Write(m))
	return m.GetCounter().GetValue()
}
func TestDisabledRuntimePublisherSkipsCapture(t *testing.T) {
	empty := config.DefaultWebhookConf()
	empty.Enabled = true
	disabledSub := empty
	disabledSub.Subscriptions = []Subscription{{ID: "off", URL: "https://example.test", Events: []string{TypeCreated}}}
	invalid := empty
	invalid.Workers = 0
	for _, c := range []config.WebhookConf{{}, config.DefaultWebhookConf(), {Enabled: true, Invalid: true}, empty, disabledSub, invalid} {
		r, _ := NewRuntime(testCtx, c)
		require.IsType(t, NopPublisher{}, r.Publisher())
		require.Nil(t, r.buffer)
		require.Nil(t, r.store)
		require.Nil(t, r.sender)
		before := metricValue(t, "capture", "accepted")
		var holder PublisherHolder
		holder.Set(r.Publisher())
		require.False(t, holder.Enabled())
		holder.Publish(TypeCreated, "sb", "", LifecycleFields{})
		require.Equal(t, before, metricValue(t, "capture", "accepted"))
		r.Shutdown(testCtx)
	}
	require.IsType(t, NopPublisher{}, (&Runtime{}).Publisher())
}
func TestRuntimeFanoutFilterAndEnabledCounters(t *testing.T) {
	type request struct {
		body          []byte
		sub, id, path string
	}
	requests := make(chan request, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requests <- request{b, r.Header.Get("e2b-webhook-id"), r.Header.Get("e2b-delivery-id"), r.URL.Path}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c := localConf()
	c.Workers = 4
	c.Subscriptions = []Subscription{
		{ID: "a", Enabled: true, URL: srv.URL + "/a", Events: []string{TypeCreated}},
		{ID: "b", Enabled: true, URL: srv.URL + "/b", Events: []string{TypeCreated}},
		{ID: "filtered", Enabled: true, URL: srv.URL, Events: []string{TypeCheckpointed}},
		{ID: "off", Enabled: false, URL: srv.URL, Events: []string{TypeCreated}},
	}
	raw, err := yaml.Marshal(c)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(raw, &c))
	require.False(t, c.Invalid)
	r, err := NewRuntime(testCtx, c)
	require.NoError(t, err)
	defer r.Shutdown(testCtx)
	require.Same(t, r, r.Publisher())
	before := metricValue(t, "ingress", "accepted")
	event := runtimeCaptured(t)
	require.Equal(t, PublishAccepted, r.Publisher().TryPublish(event).Status)
	require.Equal(t, before+1, metricValue(t, "ingress", "accepted"))
	var got []request
	for i := 0; i < 2; i++ {
		select {
		case v := <-requests:
			got = append(got, v)
		case <-time.After(3 * time.Second):
			t.Fatal("missing fanout")
		}
	}
	require.Equal(t, got[0].body, got[1].body)
	require.NotEqual(t, got[0].id, got[1].id)
	require.NotEmpty(t, got[0].id)
	require.ElementsMatch(t, []string{"a", "b"}, []string{got[0].sub, got[1].sub})
	require.ElementsMatch(t, []string{"/a", "/b"}, []string{got[0].path, got[1].path})
	r.Shutdown(testCtx)
	require.Empty(t, requests)
}
func TestRuntimeFiniteRetryStableSnapshot(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "exhausted", true: "succeeded"}[success], func(t *testing.T) {
			var calls atomic.Int32
			var mu sync.Mutex
			var bodies [][]byte
			var ids []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, b)
				ids = append(ids, r.Header.Get("e2b-delivery-id"))
				mu.Unlock()
				n := calls.Add(1)
				require.Empty(t, r.Header.Get("e2b-signature"))
				if success && n == 2 {
					w.WriteHeader(204)
				} else {
					w.WriteHeader(401)
				}
			}))
			defer srv.Close()
			c := localConf()
			c.MaxAttempts = 2
			c.Subscriptions = []Subscription{{ID: "retry", Enabled: true, URL: srv.URL, Events: []string{TypeCreated}}}
			s := memoryStore(t, 1)
			r := runtimeFor(t, c, s)
			before := metricValue(t, "terminal", map[bool]string{false: "failed", true: "succeeded"}[success])
			require.Equal(t, PublishAccepted, r.Publisher().TryPublish(runtimeCaptured(t)).Status)
			require.Eventually(t, func() bool { return calls.Load() == 2 && s.size() == 0 }, 4*time.Second, 10*time.Millisecond)
			r.Shutdown(testCtx)
			require.EqualValues(t, 2, calls.Load())
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, bodies[0], bodies[1])
			require.NotEqual(t, ids[0], ids[1])
			require.Equal(t, before+1, metricValue(t, "terminal", map[bool]string{false: "failed", true: "succeeded"}[success]))
		})
	}
}
func TestRuntimeDropsWhenCapacityFull(t *testing.T) {
	s := memoryStore(t, 1)
	held := testTask()
	held.NextAttemptAt = time.Now().Add(time.Hour)
	require.NoError(t, s.Put(testCtx, held))
	c := localConf()
	c.Subscriptions = []Subscription{{ID: "capacity", Enabled: true, URL: "https://example.test", Events: []string{TypeCreated}}}
	r := runtimeFor(t, c, s)
	before := metricValue(t, "put", "capacity")
	for i := 0; i < 4; i++ {
		require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	}
	require.Eventually(t, func() bool { return metricValue(t, "put", "capacity") == before+4 }, time.Second, time.Millisecond)
	require.Equal(t, 1, s.size())
	claim := claimOne(t, s, held.NextAttemptAt)
	require.NoError(t, s.Complete(testCtx, claim.Receipt, TerminalResult{State: DeliveryCancelled}))
	// No queued writer retry competes for the released slot.
	require.NoError(t, s.Put(testCtx, testTask()))
	ctx, cancel := context.WithCancel(testCtx)
	cancel()
	r.Shutdown(ctx)
	require.Zero(t, s.size())
}

// A blocked writer fixture verifies bounded ingress without altering producers.
type faultStore struct {
	DeliveryStore
	put func(context.Context, DeliveryTask) error
}

func (s *faultStore) Put(ctx context.Context, t DeliveryTask) error { return s.put(ctx, t) }
func TestRuntimeFullIngressDoesNotWait(t *testing.T) {
	s := memoryStore(t, 1)
	entered := make(chan struct{})
	var once sync.Once
	fs := &faultStore{DeliveryStore: s, put: func(ctx context.Context, _ DeliveryTask) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}}
	c := localConf()
	c.IngressCapacity = 1
	c.Subscriptions = []Subscription{{ID: "full", Enabled: true, URL: "https://example.test", Events: []string{TypeCreated}}}
	r := runtimeFor(t, c, fs)
	require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	<-entered
	require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	done := make(chan PublishResult, 1)
	go func() { done <- r.TryPublish(runtimeCaptured(t)) }()
	select {
	case v := <-done:
		require.Equal(t, PublishFull, v.Status)
	case <-time.After(time.Second):
		t.Fatal("full ingress waited")
	}
	ctx, cancel := context.WithCancel(testCtx)
	cancel()
	r.Shutdown(ctx)
	require.Empty(t, r.buffer.Events())
}
func TestRuntimeShutdownCancelsHTTPAndBackoff(t *testing.T) {
	for _, mode := range []string{"http-total-budget", "http-earlier-deadline", "backoff"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{}, 1)
			cancelled := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_, _ = io.Copy(io.Discard, req.Body)
				entered <- struct{}{}
				if mode == "backoff" {
					w.WriteHeader(503)
					return
				}
				<-req.Context().Done()
				cancelled <- struct{}{}
			}))
			defer srv.Close()
			c := localConf()
			c.RequestTimeout = 30 * time.Second
			c.Subscriptions = []Subscription{{ID: "shutdown", Enabled: true, URL: srv.URL, Events: []string{TypeCreated}}}
			r, err := NewRuntime(testCtx, c)
			require.NoError(t, err)
			defer r.Shutdown(testCtx)
			retried := metricValue(t, "retry", "ok")
			require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("HTTP not entered")
			}
			if mode == "backoff" {
				require.Eventually(t, func() bool { return metricValue(t, "retry", "ok") > retried }, time.Second, time.Millisecond)
			}
			ctx := testCtx
			cancel := func() {}
			if mode != "http-total-budget" {
				ctx, cancel = context.WithTimeout(testCtx, 100*time.Millisecond)
			}
			defer cancel()
			start := time.Now()
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); r.Shutdown(ctx) }()
			}
			wg.Wait()
			limit := 2500 * time.Millisecond
			if mode != "http-total-budget" {
				limit = time.Second
			}
			require.Less(t, time.Since(start), limit)
			require.Zero(t, r.store.(*MemoryDeliveryStore).size())
			require.Equal(t, PublishStopped, r.TryPublish(nil).Status)
			if mode != "backoff" {
				select {
				case <-cancelled:
				case <-time.After(time.Second):
					t.Fatal("HTTP not cancelled")
				}
			}
			r.Shutdown(testCtx)
		})
	}
}

type brokenRandom struct{}

func (brokenRandom) Read([]byte) (int, error) { return 0, errors.New("identity unavailable") }
func TestRuntimePreSendFailuresReleaseClaim(t *testing.T) {
	for _, mode := range []string{"revoked", "cancelled", "identity", "address"} {
		t.Run(mode, func(t *testing.T) {
			c := localConf()
			task := testTask()
			task.MaxAttempts = 1
			if mode == "address" {
				task.TargetURL = "http://169.254.169.254/metadata"
			}
			c.Subscriptions = []Subscription{{ID: task.SubscriptionID, Enabled: mode != "revoked", URL: task.TargetURL, Events: []string{TypeCreated}}}
			s := memoryStore(t, 1)
			require.NoError(t, s.Put(testCtx, task))
			p, err := NewStaticConfigSubscriptionProvider(c)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(testCtx)
			defer cancel()
			r := &Runtime{store: s, provider: p, sender: NewSender(c), conf: c, ctx: ctx, stopClaim: make(chan struct{})}
			defer r.sender.Close()
			if mode == "cancelled" {
				cancel()
			}
			if mode == "identity" {
				uuid.SetRand(brokenRandom{})
				defer uuid.SetRand(nil)
			}
			r.deliver(claimOne(t, s, task.NextAttemptAt))
			require.Zero(t, s.size())
		})
	}
}
func TestRuntimeParentCancellation(t *testing.T) {
	c := localConf()
	c.Subscriptions = []Subscription{{ID: "parent", Enabled: true, URL: "https://example.test", Events: []string{TypeCreated}}}
	ctx, cancel := context.WithCancel(testCtx)
	r, err := NewRuntime(ctx, c)
	require.NoError(t, err)
	cancel()
	select {
	case <-r.done:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("parent cancellation did not finish")
	}
	require.Equal(t, PublishStopped, r.TryPublish(nil).Status)
}

func TestRuntimeKeepsStartupSubscriptionSnapshot(t *testing.T) {
	paths := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.RequestURI()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := localConf()
	c.Subscriptions = []Subscription{{ID: "one", Enabled: true, URL: srv.URL + "/old?token=original", Events: []string{TypeCreated}}}
	r, err := NewRuntime(testCtx, c)
	require.NoError(t, err)
	defer r.Shutdown(testCtx)

	// Change the source slices, then decode an invalid replacement. The running
	// webhook owns its startup snapshot; generic file-watcher behavior is separate.
	c.Subscriptions[0].URL = srv.URL + "/new"
	c.Subscriptions[0].Events[0] = TypeKilled
	c.Subscriptions[0].Enabled = false
	require.NoError(t, yaml.Unmarshal([]byte("enabled: true\nworkers: invalid-secret\n"), &c))
	require.True(t, c.Invalid)
	require.True(t, r.provider.List()[0].Enabled)
	require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	select {
	case path := <-paths:
		require.Equal(t, "/old?token=original", path)
	case <-time.After(3 * time.Second):
		t.Fatal("startup subscription did not receive event")
	}
}

func TestRuntimeRevokedTargets(t *testing.T) {
	for _, change := range []string{"delete", "disable", "path", "query", "events"} {
		t.Run(change, func(t *testing.T) {
			s := memoryStore(t, 1)
			task := testTask()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer srv.Close()
			task.TargetURL = srv.URL + "/old?token=original"
			require.NoError(t, s.Put(testCtx, task))
			c := localConf()
			sub := Subscription{ID: task.SubscriptionID, Enabled: true, URL: task.TargetURL, Events: []string{TypeCreated}}
			switch change {
			case "disable":
				sub.Enabled = false
			case "path":
				sub.URL = srv.URL + "/new?token=original"
			case "query":
				sub.URL = srv.URL + "/old?token=changed"
			case "events":
				sub.Events = []string{TypeCheckpointed}
			}
			if change != "delete" {
				c.Subscriptions = []Subscription{sub}
			}
			before := metricValue(t, "terminal", "cancelled")
			r := runtimeFor(t, c, s)
			require.Eventually(t, func() bool { return s.size() == 0 }, time.Second, time.Millisecond)
			r.Shutdown(testCtx)
			require.Zero(t, calls.Load())
			require.Equal(t, before+1, metricValue(t, "terminal", "cancelled"))
		})
	}
}
func TestRuntimeShutdownDrainsAcceptedEvents(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer srv.Close()
	c := localConf()
	c.Subscriptions = []Subscription{{ID: "drain", Enabled: true, URL: srv.URL, Events: []string{TypeCreated}}}
	r, err := NewRuntime(testCtx, c)
	require.NoError(t, err)
	defer r.Shutdown(testCtx)
	for i := 0; i < 8; i++ {
		require.Equal(t, PublishAccepted, r.Publisher().TryPublish(runtimeCaptured(t)).Status)
	}
	r.Shutdown(testCtx)
	require.EqualValues(t, 8, calls.Load())
	require.Empty(t, r.buffer.Events())
	require.Zero(t, r.store.(*MemoryDeliveryStore).size())
}
