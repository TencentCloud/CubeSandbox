// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

var deliveryCounts = promauto.NewCounterVec(prometheus.CounterOpts{Name: "cubemaster_webhook_operations_total", Help: "Local notification outcomes."}, []string{"operation", "result"})
var deliveryAge = promauto.NewHistogram(prometheus.HistogramOpts{Name: "cubemaster_webhook_claim_age_seconds", Help: "Age since task creation at claim.", Buckets: prometheus.ExponentialBuckets(0.1, 4, 10)})
var queueDepth = promauto.NewGauge(prometheus.GaugeOpts{Name: "cubemaster_webhook_ingress_depth", Help: "Local buffered events."})
var inFlight = promauto.NewGauge(prometheus.GaugeOpts{Name: "cubemaster_webhook_inflight", Help: "Local active worker tasks."})

func count(op, result string) { deliveryCounts.WithLabelValues(op, result).Inc() }

// Runtime owns one writer, fixed workers and a private, cancellable work context.
// Parent cancellation initiates the same short drain used by App shutdown.
type Runtime struct {
	publisher  Publisher
	buffer     *BufferedPublisher
	provider   SubscriptionProvider
	store      DeliveryStore
	sender     *Sender
	conf       config.WebhookConf
	ctx        context.Context
	cancel     context.CancelFunc
	writerCtx  context.Context
	flush      chan struct{}
	writerDone chan struct{}
	stopClaim  chan struct{}
	done       chan struct{}
	once       sync.Once
	wg         sync.WaitGroup
}

func NewRuntime(parent context.Context, c config.WebhookConf) (*Runtime, error) {
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
	enabled := false
	for _, s := range p.List() {
		if s.Enabled {
			enabled = true
			break
		}
	}
	if !enabled {
		close(r.done)
		return r, nil
	}
	store, err := NewMemoryDeliveryStore(c.Store.MaxRecords)
	if err != nil {
		close(r.done)
		return r, err
	}
	r = startRuntime(c, p, store, NewSender(c))
	go func() {
		select {
		case <-parent.Done():
			r.Shutdown(context.Background())
		case <-r.done:
		}
	}()
	return r, nil
}
func startRuntime(c config.WebhookConf, p SubscriptionProvider, s DeliveryStore, sender *Sender) *Runtime {
	c.Subscriptions = cloneSubscriptions(c.Subscriptions)
	c.AllowedCIDRs = append([]string(nil), c.AllowedCIDRs...)
	ctx, cancel := context.WithCancel(context.Background())
	b, _ := NewBufferedPublisher(c.IngressCapacity)
	r := &Runtime{publisher: b, buffer: b, provider: p, store: s, sender: sender, conf: c, ctx: ctx, cancel: cancel, writerCtx: ctx, flush: make(chan struct{}), writerDone: make(chan struct{}), stopClaim: make(chan struct{}), done: make(chan struct{})}
	go r.writer()
	for i := 0; i < c.Workers; i++ {
		r.wg.Add(1)
		go r.worker()
	}
	return r
}
func (r *Runtime) Publisher() Publisher {
	if r == nil || r.buffer == nil {
		return NopPublisher{}
	}
	return r
}

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
	if r.buffer == nil {
		return NopPublisher{}.TryPublish(e)
	}
	v := r.publisher.TryPublish(e)
	if v.Status != PublishDisabled {
		ingressCounters[v.Status].Inc()
	}
	return v
}
func (r *Runtime) Shutdown(ctx context.Context) {
	if r == nil || r.buffer == nil {
		return
	}
	// A concurrent caller with an earlier cancellation also stops notification I/O.
	stopCaller := context.AfterFunc(ctx, r.cancel)
	defer stopCaller()
	r.once.Do(func() {
		defer close(r.done)
		budget, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		stopBudget := context.AfterFunc(budget, r.cancel)
		defer stopBudget()
		r.buffer.Stop()
		close(r.flush)
		<-r.writerDone
		if s, ok := r.store.(*MemoryDeliveryStore); ok {
			for s.size() > 0 && r.ctx.Err() == nil && pause(budget, 10*time.Millisecond) {
			}
		}
		close(r.stopClaim)
		r.cancel()
		r.wg.Wait()
		r.sender.Close()
		if s, ok := r.store.(*MemoryDeliveryStore); ok {
			deliveryCounts.WithLabelValues("task", "shutdown_discarded").Add(float64(s.discard()))
		}
		queueDepth.Set(0)
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
	result := "ok"
	switch {
	case err == nil:
	case errors.Is(err, ErrDeliveryCapacity):
		result = "capacity"
	case errors.Is(err, ErrDeliveryConflict):
		result = "conflict"
	case errors.Is(err, ErrStaleReceipt):
		result = "stale"
	case errors.Is(err, ErrInvalidDelivery):
		result = "invalid"
	default:
		result = "cancelled"
	}
	count(op, result)
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
	queueDepth.Set(float64(len(r.buffer.Events())))
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
	now := time.Now().UTC()
	for _, s := range targets {
		err := r.store.Put(r.writerCtx, DeliveryTask{Event: e, SubscriptionID: s.ID, TargetURL: s.URL, CreatedAt: now, NextAttemptAt: now, MaxAttempts: r.conf.MaxAttempts})
		r.storeError("put", err)
	}
}
func (r *Runtime) stopping() bool {
	select {
	case <-r.stopClaim:
		return true
	case <-r.ctx.Done():
		return true
	default:
		return false
	}
}
func (r *Runtime) worker() {
	defer r.wg.Done()
	for !r.stopping() {
		task, err := r.store.Claim(r.ctx, time.Now().UTC())
		if err != nil {
			r.storeError("claim", err)
			return
		}
		if task == nil {
			if !r.workerPause(50 * time.Millisecond) {
				return
			}
			continue
		}
		r.deliver(*task)
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
func (r *Runtime) complete(t ClaimedTask, state DeliveryState, summary FailureSummary) {
	// Cleanup must release the local slot even after the work context expires.
	err := r.store.Complete(context.Background(), t.Receipt, TerminalResult{State: state, Summary: summary})
	r.storeError("complete", err)
	if err == nil {
		count("terminal", string(state))
	}
}
func (r *Runtime) deliver(t ClaimedTask) {
	count("task", "claimed")
	deliveryAge.Observe(time.Since(t.Task.CreatedAt).Seconds())
	inFlight.Inc()
	defer inFlight.Dec()
	valid := false
	for _, s := range r.provider.List() {
		if s.ID == t.Task.SubscriptionID && s.URL == t.Task.TargetURL && matches(s, t.Task.Event.Type()) {
			valid = true
			break
		}
	}
	if !valid || r.stopping() {
		r.complete(t, DeliveryCancelled, FailureSummary{})
		return
	}
	id, err := uuid.NewRandom()
	if err != nil {
		count("send", "identity_failed")
		r.retry(t, FailureSummary{Code: "identity_failed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.conf.RequestTimeout)
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
	if r.ctx.Err() != nil {
		r.complete(t, DeliveryCancelled, result.Summary)
	} else if status >= 200 && status < 300 {
		count("http", "success")
		r.complete(t, DeliverySucceeded, result.Summary)
	} else {
		r.retry(t, result.Summary)
	}
}
func (r *Runtime) retry(t ClaimedTask, summary FailureSummary) {
	if r.ctx.Err() != nil {
		r.complete(t, DeliveryCancelled, summary)
	} else if t.Attempts >= t.Task.MaxAttempts {
		r.complete(t, DeliveryFailed, summary)
	} else {
		err := r.store.Retry(context.Background(), t.Receipt, time.Now().UTC().Add(backoff(t.Attempts)), summary)
		r.storeError("retry", err)
	}
}
