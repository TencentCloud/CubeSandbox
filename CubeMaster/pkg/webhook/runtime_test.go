// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"encoding/json"
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
	p, err := testSubscriptions(c)
	require.NoError(t, err)
	r := mustRuntime(t, testCtx, c, p, s, NewSender(c))
	t.Cleanup(func() { r.Shutdown(testCtx) })
	return r
}
func metricValue(t *testing.T, op, result string) float64 {
	t.Helper()
	m := &dto.Metric{}
	require.NoError(t, deliveryCounts.WithLabelValues(op, result).Write(m))
	return m.GetCounter().GetValue()
}

func TestRuntimeTimeoutMetricsAndRetryExhaustion(t *testing.T) {
	srv, requests := blockedWebhookReceiver(t)
	c := localConf()
	c.Workers = 1
	c.RequestTimeout = 300 * time.Millisecond
	c.MaxAttempts = 3
	c.Subscriptions = []config.WebhookSubscription{{ID: "timeout", Enabled: true, URL: srv.URL, Events: []string{TypeCreated}}}
	s := memoryStore(t, 1)
	p, err := testSubscriptions(c)
	require.NoError(t, err)
	sender := NewSender(c)
	// Let Runtime's per-request context expire before any client/transport
	// timeout, so this specifically exercises the original misclassification.
	sender.client.Timeout = 30 * time.Second
	sender.transport.ResponseHeaderTimeout = 30 * time.Second
	r := mustRuntime(t, testCtx, c, p, s, sender)
	t.Cleanup(func() { r.Shutdown(testCtx) })
	timeouts := metricValue(t, "send", "timeout")
	cancelled := metricValue(t, "send", "cancelled")
	failed := metricValue(t, "terminal", "failed")
	attempts := metricValue(t, "http", "attempt")
	retries := metricValue(t, "retry", "ok")
	event := runtimeCaptured(t)
	require.Equal(t, PublishAccepted, r.TryPublish(event).Status)
	var body []byte
	ids := make(map[string]bool)
	for i := 0; i < 3; i++ {
		select {
		case req := <-requests:
			require.NoError(t, req.err)
			if i == 0 {
				body = req.body
			}
			require.Equal(t, body, req.body)
			var envelope struct {
				ID string `json:"id"`
			}
			require.NoError(t, json.Unmarshal(req.body, &envelope))
			require.Equal(t, event.input.ID.String(), envelope.ID)
			_, err := uuid.Parse(req.deliveryID)
			require.NoError(t, err)
			require.False(t, ids[req.deliveryID], "delivery ID reused")
			ids[req.deliveryID] = true
			t.Logf("attempt=%d delivery_id=%s body=%s", i+1, req.deliveryID, req.body)
		case <-time.After(8 * time.Second):
			t.Fatal("missing timeout attempt")
		}
	}
	require.Eventually(t, func() bool { return metricValue(t, "terminal", "failed") == failed+1 }, 5*time.Second, 10*time.Millisecond)
	// A hypothetical fourth attempt would wait at most 4s + 25% jitter.
	select {
	case req := <-requests:
		t.Fatalf("unexpected retry after terminal failure: %s", req.deliveryID)
	case <-time.After(5100 * time.Millisecond):
	}
	require.Zero(t, s.size())
	task, err := s.Claim(testCtx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Nil(t, task, "no residual claim after terminal failure")
	t.Logf("metric deltas: timeout=%g cancelled=%g failed=%g http_attempt=%g retry=%g",
		metricValue(t, "send", "timeout")-timeouts, metricValue(t, "send", "cancelled")-cancelled,
		metricValue(t, "terminal", "failed")-failed, metricValue(t, "http", "attempt")-attempts,
		metricValue(t, "retry", "ok")-retries)
	require.Equal(t, timeouts+3, metricValue(t, "send", "timeout"))
	require.Equal(t, cancelled, metricValue(t, "send", "cancelled"))
	require.Equal(t, failed+1, metricValue(t, "terminal", "failed"))
	require.Equal(t, attempts+3, metricValue(t, "http", "attempt"))
	require.Equal(t, retries+2, metricValue(t, "retry", "ok"))
}

func TestRuntimeDeadlineAndShutdownOverlap(t *testing.T) {
	for _, first := range []string{"deadline", "shutdown"} {
		t.Run(first+"-first", func(t *testing.T) {
			srv, requests := blockedWebhookReceiver(t)
			c := localConf()
			c.Workers = 1
			c.RequestTimeout = 30 * time.Second
			if first == "deadline" {
				c.RequestTimeout = time.Second
			}
			c.Subscriptions = []config.WebhookSubscription{{ID: "overlap", Enabled: true, URL: srv.URL, Events: []string{TypeCreated}}}
			s := memoryStore(t, 1)
			p, err := testSubscriptions(c)
			require.NoError(t, err)
			sender := NewSender(c)
			sender.client.Timeout = 30 * time.Second
			sender.transport.ResponseHeaderTimeout = 30 * time.Second
			observed := make(chan error, 1)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			// Hold the transport error until both causes have happened. The
			// attempt context must retain its first cause; Runtime must still
			// cancel the task when its own context has been cancelled.
			sender.client.Transport = webhookRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				resp, err := sender.transport.RoundTrip(req)
				observed <- req.Context().Err()
				<-release
				return resp, err
			})
			r := mustRuntime(t, testCtx, c, p, s, sender)
			t.Cleanup(func() { r.Shutdown(testCtx) })
			t.Cleanup(unblock)
			timeouts := metricValue(t, "send", "timeout")
			cancelled := metricValue(t, "send", "cancelled")
			terminal := metricValue(t, "terminal", "cancelled")
			failed := metricValue(t, "terminal", "failed")
			completed := metricValue(t, "complete", "ok")
			unconfirmed := metricValue(t, "complete", "cancelled")
			discarded := metricValue(t, "task", "shutdown_discarded")
			retries := metricValue(t, "retry", "ok")
			claimed := metricValue(t, "task", "claimed")
			require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
			select {
			case req := <-requests:
				require.NoError(t, req.err)
			case <-time.After(5 * time.Second):
				t.Fatal("receiver did not read the request")
			}
			if first == "deadline" {
				select {
				case cause := <-observed:
					require.ErrorIs(t, cause, context.DeadlineExceeded)
				case <-time.After(5 * time.Second):
					t.Fatal("deadline not observed")
				}
			}
			ctx, cancel := context.WithCancel(testCtx)
			cancel()
			done := make(chan struct{})
			go func() { r.Shutdown(ctx); close(done) }()
			select {
			case <-r.ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not cancel Runtime")
			}
			if first == "shutdown" {
				select {
				case cause := <-observed:
					require.ErrorIs(t, cause, context.Canceled)
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation not observed")
				}
			}
			unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not finish")
			}
			wantTimeout, wantCancelled := float64(0), float64(1)
			if first == "deadline" {
				wantTimeout, wantCancelled = 1, 0
			}
			t.Logf("%s first: timeout=%g cancelled=%g terminal_cancelled=%g complete_ok=%g retry=%g",
				first, metricValue(t, "send", "timeout")-timeouts, metricValue(t, "send", "cancelled")-cancelled,
				metricValue(t, "terminal", "cancelled")-terminal, metricValue(t, "complete", "ok")-completed,
				metricValue(t, "retry", "ok")-retries)
			require.Equal(t, timeouts+wantTimeout, metricValue(t, "send", "timeout"))
			require.Equal(t, cancelled+wantCancelled, metricValue(t, "send", "cancelled"))
			// This caller has already cancelled the entire shutdown budget.
			// Classification still follows the attempt's first cause, but an
			// unconfirmed Complete must not claim a terminal acknowledgement.
			require.Equal(t, terminal, metricValue(t, "terminal", "cancelled"))
			require.Equal(t, failed, metricValue(t, "terminal", "failed"))
			require.Equal(t, completed, metricValue(t, "complete", "ok"))
			require.Equal(t, unconfirmed+1, metricValue(t, "complete", "cancelled"))
			require.Equal(t, discarded+1, metricValue(t, "task", "shutdown_discarded"))
			require.Equal(t, retries, metricValue(t, "retry", "ok"))
			require.Equal(t, claimed+1, metricValue(t, "task", "claimed"))
			require.Zero(t, s.size())
			require.Empty(t, requests)
		})
	}
}
func TestNilRuntimePublisherSkipsCapture(t *testing.T) {
	var r *Runtime
	require.IsType(t, NopPublisher{}, r.Publisher())
	require.IsType(t, NopPublisher{}, (&Runtime{}).Publisher())
	before := metricValue(t, "capture", "accepted")
	var holder PublisherHolder
	holder.Set(r.Publisher())
	require.False(t, holder.Enabled())
	holder.Publish(TypeCreated, "sb", "", LifecycleFields{})
	require.Equal(t, before, metricValue(t, "capture", "accepted"))
	r.Shutdown(testCtx)
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
	c.Subscriptions = []config.WebhookSubscription{
		{ID: "a", Enabled: true, URL: srv.URL + "/a", Events: []string{TypeCreated}},
		{ID: "b", Enabled: true, URL: srv.URL + "/b", Events: []string{TypeCreated}},
		{ID: "filtered", Enabled: true, URL: srv.URL, Events: []string{TypeCheckpointed}},
		{ID: "off", Enabled: false, URL: srv.URL, Events: []string{TypeCreated}},
	}
	raw, err := yaml.Marshal(c)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(raw, &c))
	require.False(t, c.Invalid)
	r, err := httpRuntime(testCtx, c)
	require.NoError(t, err)
	defer r.Shutdown(testCtx)
	require.Same(t, r, r.Publisher())
	before := metricValue(t, "ingress", "accepted")
	httpAttempts := metricValue(t, "http", "attempt")
	httpSuccess := metricValue(t, "http", "success")
	httpStatus := metricValue(t, "http_status", "204")
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
	require.Equal(t, httpAttempts+2, metricValue(t, "http", "attempt"))
	require.Equal(t, httpSuccess+2, metricValue(t, "http", "success"))
	require.Equal(t, httpStatus+2, metricValue(t, "http_status", "204"))
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
			c.Subscriptions = []config.WebhookSubscription{{ID: "retry", Enabled: true, URL: srv.URL, Events: []string{TypeCreated}}}
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
	c.Subscriptions = []config.WebhookSubscription{{ID: "capacity", Enabled: true, URL: "https://example.test", Events: []string{TypeCreated}}}
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
	put   func(context.Context, DeliveryTask) error
	claim func(context.Context, time.Time) (*ClaimedTask, error)
}

func (s *faultStore) Put(ctx context.Context, t DeliveryTask) error {
	if s.put != nil {
		return s.put(ctx, t)
	}
	return s.DeliveryStore.Put(ctx, t)
}
func (s *faultStore) Claim(ctx context.Context, now time.Time) (*ClaimedTask, error) {
	if s.claim != nil {
		return s.claim(ctx, now)
	}
	return s.DeliveryStore.Claim(ctx, now)
}
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
	c.Subscriptions = []config.WebhookSubscription{{ID: "full", Enabled: true, URL: "https://example.test", Events: []string{TypeCreated}}}
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
			c.Subscriptions = []config.WebhookSubscription{{ID: "shutdown", Enabled: true, URL: srv.URL, Events: []string{TypeCreated}}}
			r, err := httpRuntime(testCtx, c)
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
			c.Workers = 1
			task := testTask()
			task.MaxAttempts = 1
			if mode == "address" {
				task.Target = "http://169.254.169.254/metadata"
			}
			c.Subscriptions = []config.WebhookSubscription{{ID: task.SubscriptionID, Enabled: mode != "revoked", URL: task.Target, Events: []string{TypeCreated}}}
			s := memoryStore(t, 1)
			require.NoError(t, s.Put(testCtx, task))
			entered := make(chan struct{}, 1)
			fs := &faultStore{DeliveryStore: s, claim: func(ctx context.Context, now time.Time) (*ClaimedTask, error) {
				claim, err := s.Claim(ctx, now)
				if claim != nil {
					if mode == "identity" {
						uuid.SetRand(brokenRandom{})
					}
					if mode == "cancelled" {
						entered <- struct{}{}
						<-ctx.Done()
					}
				}
				return claim, err
			}}
			t.Cleanup(func() { uuid.SetRand(nil) })
			httpAttempts := metricValue(t, "http", "attempt")
			r := runtimeFor(t, c, fs)
			if mode == "cancelled" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("claim never entered")
				}
				ctx, cancel := context.WithTimeout(testCtx, 100*time.Millisecond)
				defer cancel()
				r.Shutdown(ctx)
			} else {
				require.Eventually(t, func() bool { return s.size() == 0 }, time.Second, time.Millisecond)
				r.Shutdown(testCtx)
			}
			require.Zero(t, s.size())
			require.Equal(t, httpAttempts, metricValue(t, "http", "attempt"))
		})
	}
}
func TestRuntimeParentCancellation(t *testing.T) {
	c := localConf()
	c.Subscriptions = []config.WebhookSubscription{{ID: "parent", Enabled: true, URL: "https://example.test", Events: []string{TypeCreated}}}
	ctx, cancel := context.WithCancel(testCtx)
	r, err := httpRuntime(ctx, c)
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
	c.Subscriptions = []config.WebhookSubscription{{ID: "one", Enabled: true, URL: srv.URL + "/old?token=original", Events: []string{TypeCreated}}}
	r, err := httpRuntime(testCtx, c)
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
			task.Target = srv.URL + "/old?token=original"
			require.NoError(t, s.Put(testCtx, task))
			c := localConf()
			sub := config.WebhookSubscription{ID: task.SubscriptionID, Enabled: true, URL: task.Target, Events: []string{TypeCreated}}
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
				c.Subscriptions = []config.WebhookSubscription{sub}
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
	c.Subscriptions = []config.WebhookSubscription{{ID: "drain", Enabled: true, URL: srv.URL, Events: []string{TypeCreated}}}
	r, err := httpRuntime(testCtx, c)
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

// HTTP fixtures explicitly assemble dependencies and always use NewRuntime.
func testOptions(c config.WebhookConf) RuntimeOptions {
	return RuntimeOptions{IngressCapacity: c.IngressCapacity, Workers: c.Workers, RequestTimeout: c.RequestTimeout, MaxAttempts: c.MaxAttempts, StoreOperationTimeout: time.Second}
}
func testSubscriptions(c config.WebhookConf) (*StaticSubscriptionProvider, error) {
	subscriptions := make([]Subscription, 0, len(c.Subscriptions))
	for _, s := range c.Subscriptions {
		subscriptions = append(subscriptions, Subscription{ID: s.ID, Enabled: s.Enabled, Target: s.URL, Events: s.Events})
	}
	return NewStaticSubscriptionProvider(subscriptions)
}
func mustRuntime(t *testing.T, parent context.Context, c config.WebhookConf, p SubscriptionProvider, store DeliveryStore, sender DeliverySender) *Runtime {
	t.Helper()
	r, err := NewRuntime(parent, testOptions(c), p, store, sender)
	require.NoError(t, err)
	return r
}
func httpRuntime(parent context.Context, c config.WebhookConf) (*Runtime, error) {
	p, err := testSubscriptions(c)
	if err != nil {
		return nil, err
	}
	s, err := NewMemoryDeliveryStore(c.Store.MaxRecords)
	if err != nil {
		return nil, err
	}
	sender := NewSender(c)
	r, err := NewRuntime(parent, testOptions(c), p, s, sender)
	if err != nil {
		sender.Close()
		_, _ = s.Close(context.Background())
	}
	return r, err
}
