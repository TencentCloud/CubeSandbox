// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var deliveryCounts = promauto.NewCounterVec(prometheus.CounterOpts{Name: "cubemaster_webhook_operations_total", Help: "Local notification outcomes."}, []string{"operation", "result"})
var deliveryAge = promauto.NewHistogram(prometheus.HistogramOpts{Name: "cubemaster_webhook_claim_age_seconds", Help: "Age since task creation at claim.", Buckets: prometheus.ExponentialBuckets(0.1, 4, 10)})
var queueDepth = promauto.NewGauge(prometheus.GaugeOpts{Name: "cubemaster_webhook_ingress_depth", Help: "Local buffered events."})
var inFlight = promauto.NewGauge(prometheus.GaugeOpts{Name: "cubemaster_webhook_inflight", Help: "Local active worker tasks."})

func count(op, result string) { deliveryCounts.WithLabelValues(op, result).Inc() }

// Runtime owns one writer, fixed workers and a private, cancellable work context.
// Parent cancellation initiates the same short drain used by App shutdown.
type Runtime struct {
	publisher           Publisher
	buffer              *BufferedPublisher
	provider            *StaticSubscriptionProvider
	store               DeliveryStore
	sender              DeliverySender
	options             RuntimeOptions
	ctx                 context.Context
	cancel              context.CancelFunc
	writerCtx           context.Context
	flush               chan struct{}
	writerDone          chan struct{}
	stopClaim           chan struct{}
	done                chan struct{}
	once                sync.Once
	wg                  sync.WaitGroup
	shutdownMu          sync.Mutex
	shutdownCtx         context.Context
	shutdownCancel      context.CancelFunc
	shutdownInterrupted bool
}

// RuntimeOptions contains background notification limits, independent of transport.
type RuntimeOptions struct {
	IngressCapacity       int
	Workers               int
	RequestTimeout        time.Duration
	MaxAttempts           int
	StoreOperationTimeout time.Duration
}

// NewRuntime snapshots subscriptions once. Dependencies remain caller-owned on
// error; on success this runtime exclusively owns their eventual Close calls.
func NewRuntime(parent context.Context, options RuntimeOptions, subscriptions SubscriptionProvider, store DeliveryStore, sender DeliverySender) (*Runtime, error) {
	if parent == nil || subscriptions == nil || store == nil || sender == nil ||
		options.IngressCapacity < 1 || options.IngressCapacity > 4096 || options.Workers < 1 || options.Workers > 32 ||
		options.RequestTimeout < time.Millisecond || options.RequestTimeout > time.Minute ||
		options.MaxAttempts < 1 || options.MaxAttempts > maxDeliveryAttempts || options.StoreOperationTimeout <= 0 {
		return nil, ErrInvalidDelivery
	}
	p, err := NewStaticSubscriptionProvider(subscriptions.List())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	b, err := NewBufferedPublisher(options.IngressCapacity)
	if err != nil {
		cancel()
		return nil, err
	}
	r := &Runtime{publisher: b, buffer: b, provider: p, store: store, sender: sender, options: options, ctx: ctx, cancel: cancel, writerCtx: ctx, flush: make(chan struct{}), writerDone: make(chan struct{}), stopClaim: make(chan struct{}), done: make(chan struct{})}
	go r.writer()
	for i := 0; i < options.Workers; i++ {
		r.wg.Add(1)
		go r.worker()
	}
	go func() {
		select {
		case <-parent.Done():
			r.Shutdown(context.Background())
		case <-r.done:
		}
	}()
	return r, nil
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

// cancelShutdown also lets a concurrent caller shorten the one shared budget.
func (r *Runtime) cancelShutdown() {
	r.shutdownMu.Lock()
	r.shutdownInterrupted = true
	if r.shutdownCancel != nil {
		r.shutdownCancel()
		// Install the cleanup budget before cancelling active operations.
		// Otherwise a worker could start detached cleanup in between.
		r.cancel()
	}
	r.shutdownMu.Unlock()
}
func (r *Runtime) Shutdown(ctx context.Context) {
	if r == nil || r.buffer == nil {
		return
	}
	stopCaller := context.AfterFunc(ctx, r.cancelShutdown)
	defer stopCaller()
	if ctx.Err() != nil {
		r.cancelShutdown()
	}
	r.once.Do(func() {
		defer close(r.done)
		budget, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		r.shutdownMu.Lock()
		r.shutdownCtx, r.shutdownCancel = budget, cancel
		if r.shutdownInterrupted || r.ctx.Err() != nil {
			cancel()
		}
		r.shutdownMu.Unlock()
		stopBudget := context.AfterFunc(budget, r.cancel)
		defer stopBudget()
		r.buffer.Stop()
		close(r.flush)
		<-r.writerDone
		r.storeError("drain", r.store.Drain(budget))
		close(r.stopClaim)
		r.cancel()
		r.wg.Wait()
		r.sender.Close()
		discarded, err := r.store.Close(budget)
		r.storeError("close", err)
		deliveryCounts.WithLabelValues("task", "shutdown_discarded").Add(float64(discarded))
		queueDepth.Set(0)
	})
	<-r.done
}

// Cancellation acknowledgements use a separate bounded context. During
// shutdown it inherits the SAME remaining budget, including concurrent callers.
func (r *Runtime) storeContext(cleanup bool) (context.Context, context.CancelFunc) {
	ctx := r.ctx
	if cleanup && ctx.Err() != nil {
		r.shutdownMu.Lock()
		ctx = r.shutdownCtx
		r.shutdownMu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
	}
	return context.WithTimeout(ctx, r.options.StoreOperationTimeout)
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
		ctx, cancel := r.storeContext(false)
		err := r.store.Put(ctx, DeliveryTask{Event: e, SubscriptionID: s.ID, Target: s.Target, CreatedAt: now, NextAttemptAt: now, MaxAttempts: r.options.MaxAttempts})
		cancel()
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
		ctx, cancel := r.storeContext(false)
		task, err := r.store.Claim(ctx, time.Now().UTC())
		cancel()
		if err != nil {
			r.storeError("claim", err)
			if !r.workerPause(50 * time.Millisecond) {
				return
			}
			continue
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
	ctx, cancel := r.storeContext(true)
	defer cancel()
	err := r.store.Complete(ctx, t.Receipt, TerminalResult{State: state, Summary: summary})
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
		if s.ID == t.Task.SubscriptionID && s.Target == t.Task.Target && matches(s, t.Task.Event.Type()) {
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
	ctx, cancel := context.WithTimeout(r.ctx, r.options.RequestTimeout)
	result := r.sender.Send(ctx, t.Task, id.String())
	cancel()
	count("send", sendCode(result.Summary.Code))
	if r.ctx.Err() != nil {
		r.complete(t, DeliveryCancelled, result.Summary)
	} else if result.Succeeded {
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
		ctx, cancel := r.storeContext(false)
		defer cancel()
		err := r.store.Retry(ctx, t.Receipt, time.Now().UTC().Add(backoff(t.Attempts)), summary)
		r.storeError("retry", err)
	}
}

// Adapter diagnostics cannot introduce unbounded metric labels or targets.
func sendCode(code string) string {
	switch code {
	case "http", "timeout", "cancelled", "address_rejected", "transport_error", "identity_failed", "invalid_request", "invalid_status":
		return code
	default:
		return "other"
	}
}
