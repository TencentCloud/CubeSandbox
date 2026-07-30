// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Runtime owns the ingress, dispatcher, sender, and their lifecycle.
type Runtime struct {
	ingress    *Ingress
	sender     *Sender
	dispatcher *Dispatcher

	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewRuntime constructs an in-memory webhook delivery runtime.
func NewRuntime(config *Config) *Runtime {
	stats := NewStats()
	return newRuntimeWithStats(config, newSenderWithStats(config.Delivery, &http.Client{
		Timeout: time.Duration(config.Delivery.RequestTimeoutSecs) * time.Second,
	}, stats), stats)
}

func newRuntime(config *Config, sender *Sender) *Runtime {
	return newRuntimeWithStats(config, sender, NewStats())
}

func newRuntimeWithStats(config *Config, sender *Sender, stats *Stats) *Runtime {
	ingress := newIngress(config.Delivery.EventQueueCapacity, stats)
	return &Runtime{
		ingress:    ingress,
		sender:     sender,
		dispatcher: newDispatcher(ingress, config, sender, stats),
		done:       make(chan struct{}),
	}
}

// Ingress returns the internal batch admission queue.
func (r *Runtime) Ingress() *Ingress {
	return r.ingress
}

// Handler returns the unauthenticated internal HTTP handler.
func (r *Runtime) Handler() *Handler {
	return NewHandler(r.ingress)
}

// Start begins dispatching accepted batches.
func (r *Runtime) Start() {
	r.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		go func() {
			defer close(r.done)
			r.dispatcher.Run(ctx)
		}()
	})
}

// Shutdown stops admission, drains queued batches, flushes endpoint buffers,
// and waits for submitted external deliveries within the caller's deadline.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.Start()
	r.stopOnce.Do(func() {
		r.ingress.Close()
		r.cancel()
	})
	select {
	case <-r.done:
		if err := r.sender.Wait(ctx); err != nil {
			r.sender.Stop()
			return err
		}
		return nil
	case <-ctx.Done():
		r.sender.Stop()
		return ctx.Err()
	}
}
