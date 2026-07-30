// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"log/slog"
	"time"
)

type deliverySubmitter interface {
	Submit(context.Context, *Endpoint, []Event) bool
}

type endpointBuffer struct {
	endpoint *Endpoint
	events   []Event
}

// Dispatcher filters internal events and forms independent endpoint batches.
type Dispatcher struct {
	ingress   *Ingress
	config    *Config
	submitter deliverySubmitter
	buffers   map[int]*endpointBuffer
	stats     *Stats
}

// NewDispatcher creates an endpoint dispatcher.
func NewDispatcher(ingress *Ingress, config *Config, submitter deliverySubmitter) *Dispatcher {
	return newDispatcher(ingress, config, submitter, NewStats())
}

func newDispatcher(ingress *Ingress, config *Config, submitter deliverySubmitter, stats *Stats) *Dispatcher {
	buffers := make(map[int]*endpointBuffer, len(config.Endpoints))
	for _, endpoint := range config.Endpoints {
		buffers[endpoint.ID] = &endpointBuffer{
			endpoint: endpoint,
			events:   make([]Event, 0, endpoint.BatchSize),
		}
	}
	return &Dispatcher{ingress: ingress, config: config, submitter: submitter, buffers: buffers, stats: stats}
}

// Stats returns the dispatcher statistics registry.
func (d *Dispatcher) Stats() *Stats {
	return d.stats
}

// Run consumes accepted internal batches until the context is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	interval := time.Duration(d.config.Delivery.FlushIntervalSecs) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	statsTicker := time.NewTicker(time.Minute)
	defer statsTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.drainAndFlush(context.Background())
			slog.Info("webhook delivery statistics", "stats", d.stats.Snapshot())
			return
		case <-ticker.C:
			d.Flush(ctx)
		case <-statsTicker.C:
			slog.Info("webhook delivery statistics", "stats", d.stats.Snapshot())
		case batch, ok := <-d.ingress.batches:
			if !ok {
				return
			}
			d.ingress.mu.Lock()
			d.ingress.queued -= len(batch.Events)
			d.ingress.mu.Unlock()
			d.dispatchBatch(ctx, batch)
		}
	}
}

func (d *Dispatcher) drainAndFlush(ctx context.Context) {
	for {
		select {
		case batch := <-d.ingress.batches:
			d.ingress.mu.Lock()
			d.ingress.queued -= len(batch.Events)
			d.ingress.mu.Unlock()
			d.dispatchBatch(ctx, batch)
		default:
			d.Flush(ctx)
			return
		}
	}
}

func (d *Dispatcher) dispatchBatch(ctx context.Context, batch InternalBatch) {
	for _, event := range batch.Events {
		endpoints := d.config.Routes[event.Name()]
		if len(endpoints) == 0 {
			d.stats.recordFiltered(event.Name())
			slog.Debug("webhook event filtered", "event", event.Name())
			continue
		}
		d.stats.recordMatched(event.Name())
		for _, endpoint := range endpoints {
			buffer := d.buffers[endpoint.ID]
			if buffer == nil {
				slog.Error("webhook endpoint buffer missing", "endpoint", endpoint.Name, "endpoint_id", endpoint.ID)
				continue
			}
			buffer.events = append(buffer.events, event)
			if len(buffer.events) >= endpoint.BatchSize {
				d.submit(ctx, buffer)
			}
		}
	}
}

// Flush submits every non-empty endpoint buffer.
func (d *Dispatcher) Flush(ctx context.Context) {
	for _, endpoint := range d.config.Endpoints {
		if buffer := d.buffers[endpoint.ID]; buffer != nil && len(buffer.events) > 0 {
			d.submit(ctx, buffer)
		}
	}
}

func (d *Dispatcher) submit(ctx context.Context, buffer *endpointBuffer) {
	events := buffer.events
	buffer.events = make([]Event, 0, buffer.endpoint.BatchSize)
	d.stats.endpointBatches.Add(1)
	slog.Debug("webhook endpoint batch formed", "endpoint", buffer.endpoint.Name, "event_count", len(events))
	if !d.submitter.Submit(ctx, buffer.endpoint, events) {
		slog.Warn("webhook delivery submission cancelled", "endpoint", buffer.endpoint.Name, "event_count", len(events))
	}
}
