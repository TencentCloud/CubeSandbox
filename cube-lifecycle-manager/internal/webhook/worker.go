// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
)

// Stream is the subset of redisstream.Client a Worker needs. Declared as an
// interface so tests can drive the worker with a fake.
type Stream interface {
	EnsureGroup(ctx context.Context, group string) error
	ReadGroup(ctx context.Context, group, consumer string, block time.Duration, count int) ([]redisstream.Event, error)
	ReadPending(ctx context.Context, group, consumer string, count int) ([]redisstream.Event, error)
	Ack(ctx context.Context, group, id string) error
}

// Worker consumes the lifecycle events stream for exactly one endpoint and
// delivers matching events to it. Workers share no state, so a slow or
// unreachable endpoint can only back up its own consumer group.
type Worker struct {
	endpoint  Endpoint
	stream    Stream
	deliverer *Deliverer
	consumer  string
	readBlock time.Duration
	log       *zap.Logger
	// now supplies the fallback event time when a stream entry carries no
	// timestamp. Injectable for tests.
	now func() time.Time
}

// NewWorker builds the worker for one endpoint. consumer is this replica's
// consumer name inside the endpoint's group — reusing the CLM's hostname
// means every replica gets an independent pending-entries list and the
// group's entries are split across replicas.
func NewWorker(endpoint Endpoint, stream Stream, deliverer *Deliverer, consumer string, readBlock time.Duration, log *zap.Logger) *Worker {
	return &Worker{
		endpoint:  endpoint,
		stream:    stream,
		deliverer: deliverer,
		consumer:  consumer,
		readBlock: readBlock,
		log:       log.With(zap.String("endpoint", endpoint.Label()), zap.String("group", endpoint.Group())),
		now:       time.Now,
	}
}

// Run ensures the endpoint's consumer group exists, redelivers anything this
// consumer left pending from a previous run, then consumes new events until
// the context is cancelled. Transient Redis errors are retried with a short
// backoff, mirroring the CLM's own stream consumer.
func (w *Worker) Run(ctx context.Context) error {
	group := w.endpoint.Group()
	if err := w.stream.EnsureGroup(ctx, group); err != nil {
		return err
	}
	w.log.Info("webhook worker started")

	// Crash recovery: entries this consumer read but never ACKed are still
	// on its pending-entries list; drain them before following new ones so
	// an interrupted delivery is retried rather than lost.
	var first string
	for {
		events, err := w.stream.ReadPending(ctx, group, w.consumer, 100)
		if err != nil {
			// Proceed to the main loop: pending entries stay on the list
			// and are retried on the next restart (or via XAUTOCLAIM).
			w.log.Warn("read pending failed; skipping crash recovery", zap.Error(err))
			break
		}
		if len(events) == 0 {
			break
		}
		if first == events[0].StreamID {
			// The previous batch's ACKs did not land (Redis is flapping):
			// re-reading would return the same entries forever. Leave them
			// pending and proceed; the next restart retries them.
			w.log.Warn("pending drain made no progress; leaving entries pending",
				zap.String("id", first))
			break
		}
		first = events[0].StreamID
		w.log.Info("redelivering pending events", zap.Int("count", len(events)))
		if err := w.deliverBatch(ctx, group, events); err != nil {
			return err
		}
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		events, err := w.stream.ReadGroup(ctx, group, w.consumer, w.readBlock, 100)
		if err != nil {
			w.log.Warn("xreadgroup failed; backing off", zap.Error(err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if err := w.deliverBatch(ctx, group, events); err != nil {
			return err
		}
	}
}

// deliverBatch delivers events in stream order, ACKing each one as it reaches
// a terminal state. It returns only when the context is cancelled mid-batch;
// the interrupted entry is left pending for the next run.
func (w *Worker) deliverBatch(ctx context.Context, group string, events []redisstream.Event) error {
	for _, ev := range events {
		if w.handleEvent(ctx, ev) {
			if err := w.stream.Ack(ctx, group, ev.StreamID); err != nil {
				w.log.Warn("ack failed", zap.String("id", ev.StreamID), zap.Error(err))
			}
			continue
		}
		return ctx.Err()
	}
	return nil
}

// handleEvent maps, filters, and delivers one stream event. The boolean
// reports whether the event reached a terminal state and may be ACKed:
//
//   - not a lifecycle transition, or not subscribed → true (skip)
//   - delivered, permanently rejected, or retries exhausted → true
//   - delivery interrupted by shutdown → false (stays pending, is
//     redelivered after restart)
func (w *Worker) handleEvent(ctx context.Context, ev redisstream.Event) bool {
	payload := MapEvent(ev, w.now)
	if payload == nil || !w.endpoint.Subscribes(payload.Event) {
		return true
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// Payload is a flat struct of strings; a marshal failure here is a
		// bug, not a transient condition, so do not retry it.
		w.log.Error("webhook payload serialization failed",
			zap.String("event", payload.Event), zap.Error(err))
		return true
	}
	if err := w.deliverer.Deliver(ctx, w.endpoint, body, payload.Event, payload.Timestamp); err != nil {
		if ctx.Err() != nil {
			w.log.Warn("delivery interrupted by shutdown; event stays pending",
				zap.String("id", ev.StreamID), zap.String("event", payload.Event))
			return false
		}
		// Terminal delivery failure (permanent rejection or exhausted
		// retries) is already logged by the Deliverer; ACK so one dead
		// endpoint cannot stall its group forever.
		return true
	}
	w.log.Info("webhook event delivered",
		zap.String("event", payload.Event),
		zap.String("sandbox_id", payload.SandboxID))
	return true
}
