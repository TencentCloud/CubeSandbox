// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

var deliveryCounts = promauto.NewCounterVec(prometheus.CounterOpts{Name: "cubemaster_webhook_operations_total", Help: "Local observations, not an audit log."}, []string{"operation", "result"})
var deliveryAge = promauto.NewHistogram(prometheus.HistogramOpts{Name: "cubemaster_webhook_claim_age_seconds", Help: "Age since task creation at claim.", Buckets: prometheus.ExponentialBuckets(0.1, 4, 10)})
var queueDepth = promauto.NewGauge(prometheus.GaugeOpts{Name: "cubemaster_webhook_ingress_depth", Help: "Local buffered events."})
var backlog = promauto.NewGauge(prometheus.GaugeOpts{Name: "cubemaster_webhook_backlog", Help: "Shared pending plus inflight tasks; do not sum across Masters."})
var dueDelay = promauto.NewGauge(prometheus.GaugeOpts{Name: "cubemaster_webhook_due_delay_seconds", Help: "Oldest scheduling due delay, not event age."})
var sampleAvailable = promauto.NewGauge(prometheus.GaugeOpts{Name: "cubemaster_webhook_sample_available", Help: "One when the latest queue sample succeeded."})
var inFlight = promauto.NewGauge(prometheus.GaugeOpts{Name: "cubemaster_webhook_inflight", Help: "Local active worker tasks."})

func count(op, result string) { deliveryCounts.WithLabelValues(op, result).Inc() }

// Runtime owns a private work context: parent cancellation starts bounded flush
// instead of cancelling storage before the writer can drain. No lifecycle calls
// are installed here; P4/P5 receive Publisher through the owning App.
type Runtime struct {
	publisher    Publisher
	buffer       *BufferedPublisher
	provider     SubscriptionProvider
	store        DeliveryStore
	closeStore   func() error
	sender       *Sender
	conf         config.WebhookConf
	ctx          context.Context
	cancel       context.CancelFunc
	writerCtx    context.Context
	cancelWriter context.CancelFunc
	flush        chan struct{}
	writerDone   chan struct{}
	stopClaim    chan struct{}
	done         chan struct{}
	once         sync.Once
	wg           sync.WaitGroup
	corrupt      atomic.Bool
	lastWarning  atomic.Int64
}

func NewRuntime(parent context.Context, c config.WebhookConf, redisConf *config.RedisConf) (*Runtime, error) {
	return newRuntime(parent, c, redisConf, NewRedisDeliveryStore, NewSender)
}
func newRuntime(parent context.Context, c config.WebhookConf, redisConf *config.RedisConf,
	storeFactory func(config.RedisConf, RedisStoreOptions) (*RedisDeliveryStore, error), senderFactory func(config.WebhookConf) *Sender) (*Runtime, error) {

	r := &Runtime{publisher: NopPublisher{}, done: make(chan struct{})}
	if c.Invalid {
		close(r.done)
		return r, config.ErrWebhookConfig
	}
	if !c.Enabled {
		close(r.done)
		return r, nil
	}
	p, err := NewStaticConfigSubscriptionProvider(c)
	if err != nil {
		close(r.done)
		return r, err
	}
	var rc config.RedisConf
	if redisConf != nil {
		rc = *redisConf
	}
	if c.Store.Redis != nil {
		rc = *c.Store.Redis
	}
	s, err := storeFactory(rc, RedisStoreOptions{MaxRecords: c.Store.MaxRecords, MaxActive: c.Store.MaxActive, MaxIdle: c.Store.MaxIdle, OperationTimeout: c.Store.OperationTimeout, TerminalRetention: c.Store.TerminalRetention, WriterRetryWindow: 32 * time.Second})
	if err != nil {
		close(r.done)
		return r, config.ErrWebhookConfig
	}
	r = startRuntime(c, p, s, s.Close, senderFactory(c))
	go func() {
		select {
		case <-parent.Done():
			r.Shutdown(context.Background())
		case <-r.done:
		}
	}()
	return r, nil
}
func startRuntime(c config.WebhookConf, p SubscriptionProvider, s DeliveryStore, closeStore func() error, sender *Sender) *Runtime {
	c.Subscriptions = cloneSubscriptions(c.Subscriptions)
	c.AllowedCIDRs = append([]string(nil), c.AllowedCIDRs...)
	if c.Store.Redis != nil {
		rc := *c.Store.Redis
		c.Store.Redis = &rc
	}
	ctx, cancel := context.WithCancel(context.Background())
	wc, cw := context.WithCancel(ctx)
	b, _ := NewBufferedPublisher(c.IngressCapacity)
	r := &Runtime{publisher: b, buffer: b, provider: p, store: s, closeStore: closeStore, sender: sender, conf: c, ctx: ctx, cancel: cancel, writerCtx: wc, cancelWriter: cw, flush: make(chan struct{}), writerDone: make(chan struct{}), stopClaim: make(chan struct{}), done: make(chan struct{})}
	go r.writer()
	for i := 0; i < c.Workers; i++ {
		r.wg.Add(1)
		go r.worker()
	}
	r.wg.Add(1)
	go r.maintain()
	return r
}
func (r *Runtime) Publisher() Publisher { return r }

// Prebind finite labels: the business path performs atomic increments only,
// never a metric-vector lookup or registry lock.
var captureCounters = func() [4]prometheus.Counter {
	var out [4]prometheus.Counter
	for i, label := range []string{"accepted", "oversized", "suppressed", "invalid"} {
		out[i] = deliveryCounts.WithLabelValues("capture", label)
	}
	return out
}()
var ingressCounters = func() [5]prometheus.Counter {
	var out [5]prometheus.Counter
	for i, label := range []string{"accepted", "disabled", "full", "stopped", "invalid"} {
		out[i] = deliveryCounts.WithLabelValues("ingress", label)
	}
	return out
}()

func (r *Runtime) TryPublish(e *CapturedEvent) PublishResult {
	v := r.publisher.TryPublish(e)
	if v.Status != PublishDisabled {
		ingressCounters[v.Status].Inc()
	}
	return v
}
func (r *Runtime) Shutdown(ctx context.Context) {
	if r.buffer == nil {
		return
	}
	r.once.Do(func() {
		defer close(r.done)
		budget, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r.buffer.Stop()
		close(r.flush)
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-r.writerDone:
		case <-timer.C:
			r.cancelWriter()
		case <-budget.Done():
			r.cancelWriter()
		}
		timer.Stop()
		<-r.writerDone
		close(r.stopClaim)
		joined := make(chan struct{})
		go func() { r.wg.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-budget.Done():
			r.cancel()
			<-joined
		}
		r.cancelWriter()
		r.cancel()
		r.sender.Close()
		_ = r.closeStore()
	})
	<-r.done
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func backoff(n int) time.Duration {
	d := time.Second
	for i := 1; i < n && d < 30*time.Second; i++ {
		d *= 2
	}
	if d > 24*time.Second {
		d = 24 * time.Second
	}
	return d + time.Duration(rand.Int64N(int64(d/4)+1))
}
func (r *Runtime) storeError(op string, err error) {
	if err == nil {
		count(op, "confirmed")
		return
	}
	reason := "unavailable"
	switch {
	case errors.Is(err, ErrStoreUncertain):
		reason = "uncertain"
	case errors.Is(err, ErrStoreCorrupt):
		reason = "corrupt"
		r.buffer.Stop()
		r.corrupt.Store(true)
		r.cancelWriter()
	case errors.Is(err, ErrDeliveryCapacity):
		reason = "capacity"
	case errors.Is(err, ErrStaleReceipt):
		reason = "stale"
	case errors.Is(err, ErrInvalidDelivery):
		reason = "invalid"
	case errors.Is(err, ErrDeliveryConflict):
		reason = "conflict"
	}
	count(op, reason)
	now := time.Now().Unix()
	old := r.lastWarning.Load()
	if now-old >= 30 && r.lastWarning.CompareAndSwap(old, now) {
		CubeLog.Warnf("webhook storage operation=%s result=%s; values redacted", op, reason)
	}
}
func (r *Runtime) writer() {
	defer close(r.writerDone)
	defer func() {
		for {
			select {
			case <-r.buffer.Events():
				count("ingress", "unprocessed_shutdown")
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-r.writerCtx.Done():
			return
		default:
		}
		select {
		case e := <-r.buffer.Events():
			r.write(e)
		case <-r.flush:
			for {
				select {
				case <-r.writerCtx.Done():
					return
				default:
				}
				select {
				case e := <-r.buffer.Events():
					r.write(e)
				default:
					return
				}
			}
		case <-r.writerCtx.Done():
			return
		}
	}
}
func (r *Runtime) write(c *CapturedEvent) {
	var targets []Subscription
	for _, s := range r.provider.List() {
		if matches(s, c.input.Type) {
			targets = append(targets, s)
		}
	}
	if len(targets) == 0 {
		count("event", "unmatched")
		return
	}
	p, err := MapToProtocol(c)
	if err != nil {
		count("event", "mapping_failed")
		return
	}
	e, err := Encode(c, p)
	if err != nil {
		count("event", "encoding_failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.writerCtx, 30*time.Second)
	defer cancel()
	now := time.Now().UTC()
	tasks := make([]DeliveryTask, len(targets))
	pending := make([]bool, len(targets))
	uncertain := make([]bool, len(targets))
	remaining := len(targets)
	for i, s := range targets {
		tasks[i] = DeliveryTask{e, s.ID, s.URL, now, now, r.conf.MaxAttempts}
		pending[i] = true
	}
	for round := 1; remaining > 0 && ctx.Err() == nil; round++ {
		for i, t := range tasks {
			if !pending[i] || ctx.Err() != nil {
				continue
			}
			err = r.store.Put(ctx, t)
			r.storeError("put", err)
			if err == nil {
				pending[i] = false
				remaining--
				continue
			}
			if errors.Is(err, ErrStoreUncertain) || errors.Is(err, ErrStoreCorrupt) {
				uncertain[i] = true
			}
			if errors.Is(err, ErrInvalidDelivery) || errors.Is(err, ErrDeliveryConflict) {
				pending[i] = false
				remaining--
				count("persist", "rejected")
			}
		}
		if remaining > 0 && !pause(ctx, backoff(round)) {
			break
		}
	}
	for i, v := range pending {
		if v {
			if uncertain[i] {
				count("persist", "unconfirmed")
			} else {
				count("persist", "abandoned")
			}
		}
	}
}
func (r *Runtime) stopping() bool {
	select {
	case <-r.stopClaim:
		return true
	case <-r.ctx.Done():
		return true
	default:
		return r.corrupt.Load()
	}
}
func (r *Runtime) worker() {
	defer r.wg.Done()
	failures := 0
	for !r.stopping() {
		tasks, err := r.store.Claim(r.ctx, time.Now().UTC(), 1, r.conf.Lease)
		r.storeError("claim", err)
		if err != nil {
			failures++
			if !r.workerPause(backoff(failures)) {
				return
			}
			continue
		}
		failures = 0
		if len(tasks) == 0 {
			if !r.workerPause(500 * time.Millisecond) {
				return
			}
			continue
		}
		for _, t := range tasks {
			if r.stopping() {
				return
			}
			r.deliver(t)
		}
	}
}
func (r *Runtime) workerPause(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.stopClaim:
		return false
	case <-r.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (r *Runtime) deliver(t ClaimedTask) {
	count("task", "claimed")
	deliveryAge.Observe(time.Since(t.Task.CreatedAt).Seconds())
	inFlight.Inc()
	defer inFlight.Dec()
	if time.Until(t.LeaseUntil) < r.conf.RequestTimeout+r.conf.Store.OperationTimeout+2*time.Second {
		count("task", "lease_short")
		return
	}
	valid := false
	for _, s := range r.provider.List() {
		if s.ID == t.Task.SubscriptionID && s.URL == t.Task.TargetURL && matches(s, t.Task.Event.Type()) {
			valid = true
			break
		}
	}
	if !valid {
		err := r.store.Complete(r.ctx, t.Receipt, TerminalResult{State: DeliveryCancelled})
		r.storeError("complete", err)
		if err == nil {
			count("terminal", "cancelled")
		}
		return
	}
	if r.stopping() {
		return
	}
	id, err := uuid.NewRandom()
	if err != nil {
		count("send", "identity_failed")
		return
	}
	deadline := time.Now().Add(r.conf.RequestTimeout)
	leaseDeadline := t.LeaseUntil.Add(-r.conf.Store.OperationTimeout - 2*time.Second)
	if leaseDeadline.Before(deadline) {
		deadline = leaseDeadline
	}
	ctx, cancel := context.WithDeadline(r.ctx, deadline)
	result := r.sender.Send(ctx, t.Task, id.String())
	cancel()
	if result.Attempted {
		count("http", "attempt")
	}
	count("send", result.Summary.Code)
	status := result.Summary.HTTPStatus
	if status != 0 {
		count("http_status", strconv.Itoa(status))
	}
	if r.corrupt.Load() {
		count("submit", "paused_corrupt")
		return
	}
	if status >= 200 && status < 300 {
		count("http", "success")
		err = r.store.Complete(r.ctx, t.Receipt, TerminalResult{State: DeliverySucceeded, Summary: result.Summary})
	} else if t.Attempts >= t.Task.MaxAttempts {
		err = r.store.Complete(r.ctx, t.Receipt, TerminalResult{State: DeliveryFailed, Summary: result.Summary})
		if err == nil {
			count("terminal", "failed")
		}
	} else {
		err = r.store.Retry(r.ctx, t.Receipt, time.Now().UTC().Add(backoff(t.Attempts)), result.Summary)
		r.storeError("retry", err)
		return
	}
	r.storeError("complete", err)
}
func (r *Runtime) maintain() {
	defer r.wg.Done()
	for !r.stopping() {
		if !r.workerPause(30 * time.Second) {
			return
		}
		if r.stopping() {
			return
		}
		queueDepth.Set(float64(len(r.buffer.Events())))
		if store, ok := r.store.(*RedisDeliveryStore); ok {
			sample, err := store.Sample(r.ctx, time.Now().UTC())
			if err != nil {
				sampleAvailable.Set(0)
				r.storeError("sample", err)
			} else {
				sampleAvailable.Set(1)
				backlog.Set(float64(sample.PendingAndInflight))
				dueDelay.Set(sample.DueDelay.Seconds())
			}
			if r.stopping() {
				return
			}
		}
		n, err := r.store.Prune(r.ctx, time.Now().UTC(), 32)
		r.storeError("prune", err)
		if err == nil {
			deliveryCounts.WithLabelValues("prune", "removed").Add(float64(n))
		}
	}
}
