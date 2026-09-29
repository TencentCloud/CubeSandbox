// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/statusclient"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

const (
	statusReconcileEvery = 60 * time.Second
	statusFlushBackoff   = 30 * time.Second
)

// statusReporter batches restart status toward CubeMaster. Losing the
// master does not stop the node from restarting.
type statusReporter struct {
	client *statusclient.Client
	store  cubes.CubeboxAPI
	hostID string

	mu      sync.Mutex
	pending map[string]statusclient.Item
	acked   map[string]int64
}

func (m *restartMgr) startStatusReporter() {
	if m == nil || m.store == nil || !config.StatusReportEnabled() {
		return
	}
	host, _ := utils.GetInstanceID()
	r := &statusReporter{
		client:  statusclient.NewFromConfig(),
		store:   m.store,
		hostID:  host,
		pending: map[string]statusclient.Item{},
		acked:   map[string]int64{},
	}
	m.reporter = r
	go r.run()
}

func (m *restartMgr) observe(sandboxID string) {
	if m == nil || m.reporter == nil || sandboxID == "" {
		return
	}
	m.reporter.observe(sandboxID)
}

func (r *statusReporter) run() {
	r.reconcile()
	interval := config.StatusReportInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	nextReconcile := time.Now().Add(statusJitter())
	backoff := time.Second
	for range ticker.C {
		if time.Now().After(nextReconcile) {
			r.reconcile()
			nextReconcile = time.Now().Add(statusJitter())
		}
		if err := r.flush(); err != nil {
			log.G(context.Background()).Warnf("sandbox status report: %v", err)
			if backoff < statusFlushBackoff {
				backoff *= 2
			}
			time.Sleep(backoff)
			continue
		}
		backoff = time.Second
	}
}

func statusJitter() time.Duration {
	span := int64(statusReconcileEvery) / 5
	return statusReconcileEvery - time.Duration(span) + time.Duration(rand.Int63n(span*2))
}

func (r *statusReporter) observe(sandboxID string) {
	cb, err := r.store.Get(context.Background(), sandboxID)
	if err != nil || cb == nil {
		return
	}
	item, ok := statusItem(cb, r.hostID)
	if !ok {
		return
	}
	r.enqueue(item)
}

func (r *statusReporter) reconcile() {
	if r == nil || r.store == nil {
		return
	}
	for _, cb := range r.store.List() {
		if cb == nil {
			continue
		}
		item, ok := statusItem(cb, r.hostID)
		if !ok {
			continue
		}
		r.enqueue(item)
	}
}

func statusItem(cb *cubeboxstore.CubeBox, host string) (statusclient.Item, bool) {
	cb.Lock()
	item := statusclient.Item{
		SandboxID:               cb.ID,
		HostID:                  host,
		Phase:                   cb.RestartState,
		RestartState:            cb.RestartState,
		RestartCount:            cb.RestartCount,
		LastExitReason:          cb.LastExitReason,
		StatusSeq:               cb.StatusSeq,
		LastRestartAt:           unixNanoTime(cb.LastRestartAt),
		LastSuccessfulRestartAt: unixNanoTime(cb.LastSuccessfulRestartAt),
		LastFailedRestartAt:     unixNanoTime(cb.LastFailedRestartAt),
		NextRestartAt:           unixNanoTime(cb.NextRestartAt),
	}
	if cb.LastExitReason != "" || cb.LastExitCode != 0 {
		code := cb.LastExitCode
		item.LastExitCode = &code
	}
	if cb.Labels != nil {
		item.RestartPolicy = cb.Labels[restartpolicy.Label]
	}
	cb.Unlock()
	if item.SandboxID == "" || (item.RestartState == "" && item.RestartCount == 0 && item.StatusSeq == 0) {
		return statusclient.Item{}, false
	}
	if item.RestartPolicy == "" {
		if req, err := cb.CopyOriginalRequest(); err == nil && req != nil {
			item.RestartPolicy = req.GetRestartPolicy()
		}
	}
	return item, true
}

func unixNanoTime(ns int64) *time.Time {
	if ns <= 0 {
		return nil
	}
	t := time.Unix(0, ns).UTC()
	return &t
}

func (r *statusReporter) enqueue(item statusclient.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.acked[item.SandboxID] >= item.StatusSeq && item.StatusSeq > 0 {
		return
	}
	if prev, ok := r.pending[item.SandboxID]; ok && prev.StatusSeq > item.StatusSeq {
		return
	}
	r.pending[item.SandboxID] = item
}

func (r *statusReporter) flush() error {
	items := r.take(config.StatusReportBatchSize())
	if len(items) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.client.Report(ctx, r.hostID, items); err != nil {
		r.restore(items)
		return err
	}
	r.ack(items)
	return nil
}

func (r *statusReporter) take(n int) []statusclient.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 {
		n = 100
	}
	out := make([]statusclient.Item, 0, n)
	for id, item := range r.pending {
		if len(out) >= n {
			break
		}
		out = append(out, item)
		delete(r.pending, id)
	}
	return out
}

func (r *statusReporter) restore(items []statusclient.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range items {
		if prev, ok := r.pending[item.SandboxID]; ok && prev.StatusSeq > item.StatusSeq {
			continue
		}
		r.pending[item.SandboxID] = item
	}
}

func (r *statusReporter) ack(items []statusclient.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range items {
		if r.acked[item.SandboxID] < item.StatusSeq {
			r.acked[item.SandboxID] = item.StatusSeq
		}
	}
}
