// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"encoding/json"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/hotswap"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"gopkg.in/yaml.v3"
)

func runtimeCaptured(t *testing.T) *CapturedEvent {
	t.Helper()
	c, e := Capture(CaptureInput{ID: uuid.New(), Type: TypeCreated, Origin: OriginLifecycle, SandboxID: "synthetic-p3", OccurredAt: time.Now()})
	require.NoError(t, e)
	return c
}
func taskKey(t DeliveryTask) string {
	return t.Event.ID().String() + ":" + strconv.Itoa(len(t.SubscriptionID)) + ":" + t.SubscriptionID
}
func runtimeFor(t *testing.T, c config.WebhookConf, s DeliveryStore) *Runtime {
	t.Helper()
	p, e := NewStaticConfigSubscriptionProvider(c)
	require.NoError(t, e)
	r := startRuntime(c, p, s, func() error { return nil }, NewSender(c))
	t.Cleanup(func() { r.Shutdown(testCtx) })
	return r
}
func waitTerminal(t *testing.T, s *RedisDeliveryStore, task DeliveryTask, state DeliveryState) {
	t.Helper()
	require.Eventually(t, func() bool {
		v, e := redis.Bytes(command(t, s, "HGET", s.keys[0], taskKey(task)), nil)
		if e != nil {
			return false
		}
		var r deliveryRecord
		if json.Unmarshal(v, &r) != nil {
			return false
		}
		return r.State == string(state)
	}, 12*time.Second, 20*time.Millisecond)
}
func TestRuntimeRealPipelineAndCompetition(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	c := localConf()
	c.Workers = 2
	bodies := make(chan []byte, 10)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		calls.Add(1)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c.Subscriptions = []Subscription{{"a", true, srv.URL, []string{TypeCreated}}, {"b", true, srv.URL + "/two", []string{TypeCreated}}, {"filtered", true, srv.URL, []string{TypeCheckpointed}}}
	raw, err := yaml.Marshal(struct {
		Webhooks config.WebhookConf `yaml:"webhooks"`
	}{c})
	require.NoError(t, err)
	var loaded config.Config
	require.NoError(t, yaml.Unmarshal(raw, &loaded))
	require.False(t, loaded.Webhooks.Invalid)
	c = loaded.Webhooks
	first := runtimeFor(t, c, s)
	second := runtimeFor(t, c, s)
	cap := runtimeCaptured(t)
	require.Equal(t, PublishAccepted, first.TryPublish(cap).Status)
	require.Eventually(t, func() bool { return calls.Load() == 2 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, <-bodies, <-bodies)
	p, _ := MapToProtocol(cap)
	ev, _ := Encode(cap, p)
	for _, id := range []string{"a", "b"} {
		waitTerminal(t, s, DeliveryTask{Event: ev, SubscriptionID: id}, DeliverySucceeded)
	}
	sample, err := s.Sample(testCtx, time.Now())
	require.NoError(t, err)
	require.Zero(t, sample.PendingAndInflight)
	first.Shutdown(testCtx)
	second.Shutdown(testCtx)
	require.EqualValues(t, 2, calls.Load())
	n, err := s.Prune(testCtx, time.Now().Add(2*time.Hour), 32)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}
func TestRuntimeRevokedTargets(t *testing.T) {
	for _, change := range []string{"delete", "disable", "path", "query", "events"} {
		t.Run(change, func(t *testing.T) {
			s, _ := newTestStore(t, testOptions())
			task := testTask()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer srv.Close()
			task.TargetURL = srv.URL + "/old?token=original"
			require.NoError(t, s.Put(testCtx, task))
			c := localConf()
			sub := Subscription{task.SubscriptionID, true, task.TargetURL, []string{TypeCreated}}
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
			r := runtimeFor(t, c, s)
			waitTerminal(t, s, task, DeliveryCancelled)
			r.Shutdown(testCtx)
			require.Zero(t, calls.Load())
			require.Contains(t, record(t, s, taskKey(task)).Input, "original")
		})
	}
}

// Tests inject faults around the real store, not a second production backend.
type faultStore struct {
	DeliveryStore
	put      func(context.Context, DeliveryTask) error
	claim    func(context.Context, time.Time, int, time.Duration) ([]ClaimedTask, error)
	complete func(context.Context, Receipt, TerminalResult) error
	retry    func(context.Context, Receipt, time.Time, FailureSummary) error
}

func (s *faultStore) Put(c context.Context, t DeliveryTask) error {
	if s.put != nil {
		return s.put(c, t)
	}
	return s.DeliveryStore.Put(c, t)
}
func (s *faultStore) Claim(c context.Context, n time.Time, l int, d time.Duration) ([]ClaimedTask, error) {
	if s.claim != nil {
		return s.claim(c, n, l, d)
	}
	return s.DeliveryStore.Claim(c, n, l, d)
}
func (s *faultStore) Complete(c context.Context, r Receipt, v TerminalResult) error {
	if s.complete != nil {
		return s.complete(c, r, v)
	}
	return s.DeliveryStore.Complete(c, r, v)
}
func (s *faultStore) Retry(c context.Context, r Receipt, n time.Time, f FailureSummary) error {
	if s.retry != nil {
		return s.retry(c, r, n, f)
	}
	return s.DeliveryStore.Retry(c, r, n, f)
}
func TestWriterRealLostPutResponseSameSnapshot(t *testing.T) {
	s, conf := newTestStore(t, testOptions())
	addr, mode := faultProxy(t, conf)
	conf.Nodes = addr
	proxy, err := NewRedisDeliveryStore(conf, testOptions())
	require.NoError(t, err)
	defer proxy.Close()
	proxy.keys = s.keys
	c := localConf()
	c.Subscriptions = []Subscription{{"a", true, "https://example.test/a", []string{TypeCreated}}, {"b", true, "https://example.test/b", []string{TypeCreated}}}
	var mu sync.Mutex
	var snapshots []DeliveryTask
	fs := &faultStore{DeliveryStore: s, put: func(ctx context.Context, task DeliveryTask) error {
		mu.Lock()
		snapshots = append(snapshots, task)
		n := len(snapshots)
		mu.Unlock()
		if n == 1 {
			mode.Store(1)
		}
		return proxy.Put(ctx, task)
	}, claim: func(ctx context.Context, _ time.Time, _ int, _ time.Duration) ([]ClaimedTask, error) { return nil, nil }}
	r := runtimeFor(t, c, fs)
	require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	r.Shutdown(testCtx)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, snapshots, 3)
	require.Equal(t, snapshots[0], snapshots[2])
	require.Equal(t, "b", snapshots[1].SubscriptionID)
	require.EqualValues(t, 2, command(t, s, "HLEN", s.keys[0]))
}
func TestWorkerRealUncertainTransitions(t *testing.T) {
	for _, op := range []string{"claim", "complete", "retry"} {
		for _, committed := range []bool{true, false} {
			t.Run(op+strconv.FormatBool(committed), func(t *testing.T) {
				s, conf := newTestStore(t, testOptions())
				addr, mode := faultProxy(t, conf)
				conf.Nodes = addr
				proxy, err := NewRedisDeliveryStore(conf, testOptions())
				require.NoError(t, err)
				defer proxy.Close()
				proxy.keys = s.keys
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if op == "retry" {
						w.WriteHeader(500)
					} else {
						w.WriteHeader(204)
					}
				}))
				defer srv.Close()
				task := testTask()
				task.TargetURL = srv.URL
				require.NoError(t, s.Put(testCtx, task))
				c := localConf()
				c.Subscriptions = []Subscription{{task.SubscriptionID, true, task.TargetURL, []string{TypeCreated}}}
				p, _ := NewStaticConfigSubscriptionProvider(c)
				ctx, cancel := context.WithCancel(testCtx)
				defer cancel()
				r := &Runtime{store: proxy, provider: p, sender: NewSender(c), ctx: ctx, conf: c, buffer: mustBuffer(t), stopClaim: make(chan struct{})}
				defer r.sender.Close()
				fault := int32(2)
				if committed {
					fault = 1
				}
				if op == "claim" {
					mode.Store(fault)
					v, e := proxy.Claim(ctx, time.Now(), 1, c.Lease)
					require.Error(t, e)
					require.Empty(t, v)
					require.Zero(t, calls.Load())
					v, e = s.Claim(ctx, time.Now().Add(c.Lease+time.Second), 1, c.Lease)
					require.NoError(t, e)
					require.Len(t, v, 1)
					if committed {
						require.Equal(t, 2, v[0].Attempts)
					} else {
						require.Equal(t, 1, v[0].Attempts)
					}
					return
				}
				v, err := s.Claim(ctx, time.Now(), 1, c.Lease)
				require.NoError(t, err)
				require.Len(t, v, 1)
				mode.Store(fault)
				r.deliver(v[0])
				require.EqualValues(t, 1, calls.Load())
				saved := record(t, s, taskKey(task))
				if committed && op == "complete" {
					require.Equal(t, "succeeded", saved.State)
				} else if committed {
					require.Equal(t, "pending", saved.State)
					require.Greater(t, saved.Next, time.Now().UnixMilli())
				} else {
					require.Equal(t, "inflight", saved.State)
				}
				next, err := s.Claim(ctx, time.Now().Add(c.Lease+time.Second), 1, c.Lease)
				require.NoError(t, err)
				if committed && op == "complete" {
					require.Empty(t, next)
				} else {
					require.Len(t, next, 1)
					require.Equal(t, 2, next[0].Attempts)
				}
				require.EqualValues(t, 1, calls.Load(), "current memory task must never resend")
			})
		}
	}
}
func mustBuffer(t *testing.T) *BufferedPublisher {
	b, e := NewBufferedPublisher(1)
	require.NoError(t, e)
	return b
}
func TestRuntimeDisabledAndBoundedShutdown(t *testing.T) {
	c := config.DefaultWebhookConf()
	c.Store.Redis = &config.RedisConf{Nodes: "127.0.0.1:1"}
	r, err := NewRuntime(testCtx, c, nil)
	require.NoError(t, err)
	require.Nil(t, r.store)
	require.Nil(t, r.sender)
	require.Nil(t, r.buffer)
	require.Equal(t, PublishDisabled, r.TryPublish(runtimeCaptured(t)).Status)
	c = localConf()
	c.IngressCapacity = 2
	c.Subscriptions = []Subscription{{"a", true, "https://example.test", []string{TypeCreated}}}
	entered := make(chan struct{})
	var once sync.Once
	fs := &faultStore{put: func(ctx context.Context, _ DeliveryTask) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ErrStoreUncertain
	}, claim: func(ctx context.Context, _ time.Time, _ int, _ time.Duration) ([]ClaimedTask, error) {
		<-ctx.Done()
		return nil, ErrStoreUnavailable
	}}
	before := runtime.NumGoroutine()
	r = runtimeFor(t, c, fs)
	require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	<-entered
	require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	start := time.Now()
	require.Equal(t, PublishFull, r.TryPublish(runtimeCaptured(t)).Status)
	require.Less(t, time.Since(start), 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(testCtx, 100*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Shutdown(ctx) }()
	}
	wg.Wait()
	require.Less(t, time.Since(start), time.Second)
	require.Equal(t, PublishStopped, r.TryPublish(runtimeCaptured(t)).Status)
	select {
	case _, ok := <-r.buffer.Events():
		require.True(t, ok)
		t.Fatal("writer should have drained")
	default:
	}
	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 }, time.Second, 10*time.Millisecond)
}
func TestRuntimeSignatureRequiredFiniteFailure(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("e2b-signature") == "" {
			w.WriteHeader(401)
		} else {
			t.Error("unexpected signature")
		}
	}))
	defer srv.Close()
	task := testTask()
	task.TargetURL = srv.URL
	task.MaxAttempts = 2
	require.NoError(t, s.Put(testCtx, task))
	c := localConf()
	c.Subscriptions = []Subscription{{task.SubscriptionID, true, task.TargetURL, []string{TypeCreated}}}
	r := runtimeFor(t, c, s)
	waitTerminal(t, s, task, DeliveryFailed)
	r.Shutdown(testCtx)
	require.EqualValues(t, 2, calls.Load())
	require.Equal(t, 2, record(t, s, taskKey(task)).Attempts)
}
func TestRuntimeParentCancellationFlush(t *testing.T) {
	conf := realConf(t)
	c := localConf()
	ctx, cancel := context.WithCancel(testCtx)
	// No subscriptions: no new records, but this exercises production construction
	// on an isolated instance and the same parent-cancel ownership used by App.
	r, e := NewRuntime(ctx, c, &conf)
	require.NoError(t, e)
	require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	cancel()
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not close runtime")
	}
	require.Equal(t, PublishStopped, r.TryPublish(runtimeCaptured(t)).Status)
	require.Zero(t, len(r.buffer.Events()))
}
func TestRuntimeProcessHelper(t *testing.T) {
	phase := os.Getenv("P3_RUNTIME_PHASE")
	if phase == "" {
		return
	}
	var keys []interface{}
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("P3_RUNTIME_KEYS")), &keys))
	s, e := NewRedisDeliveryStore(realConf(t), testOptions())
	require.NoError(t, e)
	s.keys = keys
	defer s.Close()
	c := localConf()
	var tasks []DeliveryTask
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("P3_RUNTIME_SUBS")), &c.Subscriptions))
	// Fixed payload and identity across processes; task records remain authoritative.
	for _, sub := range c.Subscriptions {
		task := testTask()
		task.SubscriptionID = sub.ID
		task.TargetURL = sub.URL
		task.Event.id = uuid.MustParse("d12727fc-ab97-43d8-80cf-556cc1215017")
		task.Event.body = []byte("{ \"id\":\"d12727fc-ab97-43d8-80cf-556cc1215017\" }\n")
		tasks = append(tasks, task)
	}
	if phase == "seed" {
		for i, task := range tasks {
			task.NextAttemptAt = time.Now().Add(time.Duration(i) * time.Millisecond)
			require.NoError(t, s.Put(testCtx, task))
		}
		v, e := s.Claim(testCtx, time.Now().Add(time.Second), 1, 2500*time.Millisecond)
		require.NoError(t, e)
		require.Len(t, v, 1)
		require.NoError(t, s.Retry(testCtx, v[0].Receipt, time.Now().Add(2*time.Second), FailureSummary{Code: "retry_saved"}))
		v, e = s.Claim(testCtx, time.Now().Add(time.Second), 1, 2500*time.Millisecond)
		require.NoError(t, e)
		require.Len(t, v, 1)
		// A Runtime exists in the seeding process; abrupt exit skips its shutdown.
		p, _ := NewStaticConfigSubscriptionProvider(c)
		startRuntime(c, p, &faultStore{DeliveryStore: s, claim: func(ctx context.Context, _ time.Time, _ int, _ time.Duration) ([]ClaimedTask, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}, s.Close, NewSender(c))
		os.Exit(0)
	}
	if phase == "disabled" {
		c.Enabled = false
		r, e := NewRuntime(testCtx, c, nil)
		require.NoError(t, e)
		require.Nil(t, r.store)
		time.Sleep(100 * time.Millisecond)
		return
	}
	r := runtimeFor(t, c, s)
	for _, task := range tasks {
		waitTerminal(t, s, task, DeliverySucceeded)
	}
	r.Shutdown(testCtx)
}
func TestRuntimeRealSubprocessRecovery(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != "{ \"id\":\"d12727fc-ab97-43d8-80cf-556cc1215017\" }\n" {
			t.Error("recovery changed exact body")
		}
		calls.Add(1)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	subs := []Subscription{{"retry", true, srv.URL, []string{TypeCreated}}, {"inflight", true, srv.URL, []string{TypeCreated}}, {"pending", true, srv.URL, []string{TypeCreated}}}
	keys, _ := json.Marshal(s.keys)
	raw, _ := json.Marshal(subs)
	for _, phase := range []string{"seed", "disabled", "recover"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeProcessHelper$", "-test.timeout=20s")
		cmd.Env = append(os.Environ(), "P3_RUNTIME_PHASE="+phase, "P3_RUNTIME_KEYS="+string(keys), "P3_RUNTIME_SUBS="+string(raw))
		out, e := cmd.CombinedOutput()
		require.NoError(t, e, string(out))
		if phase != "recover" {
			require.Zero(t, calls.Load())
			require.EqualValues(t, 3, command(t, s, "ZCARD", s.keys[1]))
		}
	}
	require.EqualValues(t, 3, calls.Load())
	for _, sub := range subs {
		task := testTask()
		task.SubscriptionID = sub.ID
		task.Event.id = uuid.MustParse("d12727fc-ab97-43d8-80cf-556cc1215017")
		r := record(t, s, taskKey(task))
		want := 2
		if sub.ID == "pending" {
			want = 1
		}
		require.Equal(t, want, r.Attempts)
	}
}

func TestRuntimeCorruptStopsAdmission(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	c := localConf()
	c.Subscriptions = []Subscription{{"a", true, "https://example.test", []string{TypeCreated}}}
	fs := &faultStore{DeliveryStore: s, claim: func(context.Context, time.Time, int, time.Duration) ([]ClaimedTask, error) {
		return nil, ErrStoreCorrupt
	}}
	r := runtimeFor(t, c, fs)
	require.Eventually(t, func() bool { return r.corrupt.Load() }, time.Second, time.Millisecond)
	require.Equal(t, PublishStopped, r.TryPublish(runtimeCaptured(t)).Status)
	r.Shutdown(testCtx)
	require.EqualValues(t, 0, command(t, s, "HLEN", s.keys[0]))
}
func TestQueueSampleAndRecoveryObservations(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	task := testTask()
	task.MaxAttempts = 2
	require.NoError(t, s.Put(testCtx, task))
	sample, e := s.Sample(testCtx, time.Now().Add(time.Second))
	require.NoError(t, e)
	require.Equal(t, 1, sample.PendingAndInflight)
	require.GreaterOrEqual(t, sample.DueDelay, time.Second)
	first := claimOne(t, s, task.NextAttemptAt)
	require.NoError(t, s.Retry(testCtx, first.Receipt, task.NextAttemptAt, FailureSummary{}))
	second := claimOne(t, s, task.NextAttemptAt)
	require.Equal(t, 2, second.Attempts)
	v, e := s.Claim(testCtx, second.LeaseUntil, 1, time.Second)
	require.NoError(t, e)
	require.Empty(t, v)
	require.Equal(t, "failed", record(t, s, taskKey(task)).State)
	sample, e = s.Sample(testCtx, time.Now())
	require.NoError(t, e)
	require.Zero(t, sample.PendingAndInflight)
}

func TestRuntimeFinalClaimCrashNeverSends(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	task := testTask()
	task.TargetURL = srv.URL
	task.MaxAttempts = 1
	require.NoError(t, s.Put(testCtx, task))
	claims, err := s.Claim(testCtx, time.Now(), 1, time.Millisecond)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	c := localConf()
	c.Subscriptions = []Subscription{{task.SubscriptionID, true, task.TargetURL, []string{TypeCreated}}}
	r := runtimeFor(t, c, s)
	waitTerminal(t, s, task, DeliveryFailed)
	r.Shutdown(testCtx)
	require.Zero(t, calls.Load())
	require.Equal(t, 1, record(t, s, taskKey(task)).Attempts)
}

func TestDisabledDoesNotConstructResources(t *testing.T) {
	var stores, senders int
	sf := func(config.RedisConf, RedisStoreOptions) (*RedisDeliveryStore, error) {
		stores++
		return nil, ErrStoreUnavailable
	}
	hf := func(config.WebhookConf) *Sender { senders++; return nil }
	for _, c := range []config.WebhookConf{{}, config.DefaultWebhookConf(), {Enabled: true, Invalid: true}} {
		r, _ := newRuntime(testCtx, c, nil, sf, hf)
		require.Equal(t, PublishDisabled, r.TryPublish(nil).Status)
		r.Shutdown(testCtx)
	}
	require.Zero(t, stores)
	require.Zero(t, senders)
}

func TestRuntimeNetworkFaultAndDisabledProbe(t *testing.T) {
	var connections atomic.Int32
	addr := respServer(t, func(cmd string, args []interface{}) (interface{}, bool) {
		connections.Add(1)
		time.Sleep(200 * time.Millisecond)
		return "OK", false
	})
	c := localConf()
	c.Enabled = false
	rc := config.RedisConf{Nodes: addr}
	r, err := NewRuntime(testCtx, c, &rc)
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)
	require.Zero(t, connections.Load())
	r.Shutdown(testCtx)
	c.Enabled = true
	c.Subscriptions = []Subscription{{"a", true, "https://example.test", []string{TypeCreated}}}
	r, err = NewRuntime(testCtx, c, &rc)
	require.NoError(t, err)
	require.Equal(t, PublishAccepted, r.TryPublish(runtimeCaptured(t)).Status)
	require.Eventually(t, func() bool { return connections.Load() > 0 }, time.Second, time.Millisecond)
	start := time.Now()
	for i := 0; i < 2000; i++ {
		r.TryPublish(runtimeCaptured(t))
	}
	require.Less(t, time.Since(start), time.Second)
	ctx, cancel := context.WithTimeout(testCtx, 150*time.Millisecond)
	defer cancel()
	r.Shutdown(ctx)
	require.Less(t, time.Since(start), time.Second)
}

type configUpdateChannel chan *config.Config

func (c configUpdateChannel) OnEvent(v interface{}) {
	select {
	case c <- v.(*config.Config):
	default:
	}
}
func TestWatcherDoesNotReplaceSubscriptionSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "webhooks:\n  enabled: true\n  subscriptions: [{id: one, enabled: true, url: 'https://example.test/old?token=private', events: [sandbox.lifecycle.created]}]\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0600))
	watcher, err := hotswap.NewWatcher(path, 1, &config.Config{})
	require.NoError(t, err)
	changes := make(configUpdateChannel, 8)
	watcher.AppendWatcher(changes)
	data, err := watcher.Init()
	require.NoError(t, err)
	defer watcher.Close()
	p, err := NewStaticConfigSubscriptionProvider(data.(*config.Config).Webhooks)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("webhooks: {enabled: true, workers: invalid-secret}\n"), 0600))
	select {
	case cfg := <-changes:
		require.True(t, cfg.Webhooks.Invalid)
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not report invalid local update")
	}
	require.True(t, p.List()[0].Enabled)
	require.Contains(t, p.List()[0].URL, "/old?")
}

func observationValue(t *testing.T, result string) float64 {
	metric := &dto.Metric{}
	require.NoError(t, deliveryCounts.WithLabelValues("claim", result).Write(metric))
	return metric.GetCounter().GetValue()
}
func TestClaimMetricsSeparateRetryRecoveryAndExhaustion(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	task := testTask()
	require.NoError(t, s.Put(testCtx, task))
	recovered := observationValue(t, "lease_recovered")
	exhausted := observationValue(t, "exhausted")
	first := claimOne(t, s, task.NextAttemptAt)
	require.NoError(t, s.Retry(testCtx, first.Receipt, task.NextAttemptAt, FailureSummary{}))
	second := claimOne(t, s, task.NextAttemptAt)
	require.Equal(t, recovered, observationValue(t, "lease_recovered"))
	third := claimOne(t, s, second.LeaseUntil)
	require.Equal(t, recovered+1, observationValue(t, "lease_recovered"))
	v, e := s.Claim(testCtx, third.LeaseUntil, 1, time.Second)
	require.NoError(t, e)
	require.Empty(t, v)
	require.Equal(t, exhausted+1, observationValue(t, "exhausted"))
}
