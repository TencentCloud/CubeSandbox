// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
)

const (
	// webhookGroup is the consumer group the Manager uses on the lifecycle
	// events stream. It is separate from the lifecycle reconciliation group
	// ("cube-proxy-sidecar"), so webhook delivery never blocks nor is blocked
	// by the CLM's main consume loop.
	webhookGroup = "cube-webhook-delivery"

	defaultWorkers = 4
	// defaultRetryInterval is how often the periodic reclaimLoop reclaims
	// stale pending entries (e.g. left behind by a dead peer replica or a
	// failed ack) and retries them.
	defaultRetryInterval = 60 * time.Second

	// streamReadBlock / streamReadCount tune the XREADGROUP calls.
	streamReadBlock = 5 * time.Second
	streamReadCount = 32
)

// ErrNotFound is returned by UpdateEndpoint / DeleteEndpoint when no endpoint
// matches the requested ID. The REST handlers map it to HTTP 404.
var ErrNotFound = errors.New("webhook endpoint not found")

// Options bundles the webhook Manager's dependencies and tuning knobs.
type Options struct {
	Endpoints []Endpoint
	Events    []string // global event filter; empty or "*" = all

	// Delivery tuning.
	Timeout time.Duration // per-request HTTP timeout
	Retries int           // additional attempts after the first
	Workers int           // number of stream consumers (default 4)

	// Sender is the HTTP delivery implementation; defaults to a *Client.
	// Tests inject a recording fake. See iface.go.
	Sender Sender
	// Stream is the lifecycle stream consumer; production passes
	// *redisstream.Client, tests an in-memory fake. See iface.go.
	Stream streamReader
	Log    *zap.Logger

	// Internal tuning, not exposed as env config (tests set small values):
	// RetryInterval is the cadence of the periodic stale-pending reclaim.
	// StaleMinIdle is the minimum idle time before a pending entry is
	// reclaimed; must exceed the delivery budget so in-flight entries are
	// never stolen.
	RetryInterval time.Duration
	StaleMinIdle  time.Duration
}

// Manager consumes the lifecycle Redis stream through its own consumer group
// and delivers each mapped webhook event to every subscribed endpoint.
//
// Durability: events are only ACKed after delivery (success or final-failure
// drop). While an event is read-but-unacked it lives in the group's pending
// list, so a process crash cannot lose it — Run() reclaims the pending list at
// startup and redelivers (same event_id → receiver dedupes).
type Manager struct {
	mu        sync.RWMutex
	endpoints []Endpoint
	filter    map[string]bool // nil/empty = subscribe to everything

	stream         streamReader
	sender         Sender
	workers        int
	consumerPrefix string
	deliverTimeout time.Duration
	retryInterval  time.Duration
	staleMinIdle   time.Duration
	log            *zap.Logger

	// metaCache lets delete/state events carry template_id etc. even though
	// their stream entries have no payload: the Manager records the meta from
	// the create/update entries it processes, keyed by sandbox_id.
	metaCache metaCache

	dropped   atomic.Uint64 // read but not mappable to a webhook event
	delivered atomic.Uint64
	failed    atomic.Uint64
}

// New builds a Manager, applying safe defaults for zero-valued Options.
func New(o Options) *Manager {
	if o.Workers <= 0 {
		o.Workers = defaultWorkers
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	if o.Sender == nil {
		o.Sender = NewClient(o.Timeout, o.Retries, o.Log)
	}

	filter := make(map[string]bool, len(o.Events))
	for _, ev := range o.Events {
		if ev == "*" {
			filter = nil // "*" means all
			break
		}
		filter[ev] = true
	}

	prefix := "webhook"
	if host, err := os.Hostname(); err == nil && host != "" {
		prefix = host
	}

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	// Budget for one delivery chain (retries + backoff), so graceful shutdown
	// can finish the in-flight batch without the parent ctx aborting it.
	deliverTimeout := time.Duration(o.Retries+1)*timeout +
		time.Duration(o.Retries)*time.Second + 5*time.Second
	retryInterval := o.RetryInterval
	if retryInterval <= 0 {
		retryInterval = defaultRetryInterval
	}
	staleMinIdle := o.StaleMinIdle
	if staleMinIdle <= 0 {
		// Must exceed the delivery budget so a pending entry being actively
		// delivered is never reclaimed mid-flight.
		staleMinIdle = deliverTimeout + 10*time.Second
	}

	return &Manager{
		endpoints:      o.Endpoints,
		filter:         filter,
		stream:         o.Stream,
		sender:         o.Sender,
		workers:        o.Workers,
		consumerPrefix: prefix,
		deliverTimeout: deliverTimeout,
		retryInterval:  retryInterval,
		staleMinIdle:   staleMinIdle,
		log:            o.Log,
		metaCache:      metaCache{m: make(map[string]lifecycle.SandboxLifecycleMeta)},
	}
}

func (m *Manager) consumerName(i int) string {
	return fmt.Sprintf("%s-%d", m.consumerPrefix, i)
}

// Run starts the consumers and blocks until ctx is cancelled. On startup it
// (1) ensures the group exists, (2) drains the group's pending list — entries
// left un-acked by a previous crash — processing them sequentially, so the
// backlog gets its delivery attempt BEFORE any new event is consumed. It then
// runs N consumer goroutines plus an async periodic reclaimLoop safety net.
// Unread events stay in Redis and are picked up on the next startup — graceful
// shutdown loses nothing.
func (m *Manager) Run(ctx context.Context) error {
	if m.stream == nil {
		return errors.New("webhook: nil stream reader (Stream not configured)")
	}
	if err := m.stream.EnsureGroup(ctx, webhookGroup); err != nil {
		return fmt.Errorf("ensure webhook group: %w", err)
	}
	// Drain pending (crash leftovers) before consuming any new event.
	m.reclaim(ctx)

	var wg sync.WaitGroup
	for i := 0; i < m.workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.consumeLoop(ctx, m.consumerName(i))
		}(i)
	}
	// Periodic safety net: retry entries stuck in pending (dead peer replica,
	// failed ack, anything idle past the delivery budget). Async; never blocks
	// Run or the consumers.
	go m.reclaimLoop(ctx)

	wg.Wait()
	return ctx.Err()
}

// reclaim drains the group's pending list (whole PEL, from a previous crash)
// and redelivers every entry sequentially, blocking until the backlog is
// resolved before new events are consumed.
func (m *Manager) reclaim(ctx context.Context) {
	pending, err := m.stream.ReadPending(ctx, webhookGroup, m.consumerName(0))
	if err != nil {
		m.log.Warn("webhook pending reclaim failed", zap.Error(err))
		return
	}
	for _, ev := range pending {
		m.process(ctx, ev)
	}
}

// reclaimLoop is the periodic safety net: every RetryInterval it reclaims
// pending entries idle longer than staleMinIdle (entries stuck from a dead
// peer replica, a failed ack, or any delivery that resolved to drop but whose
// ack failed) and redelivers them — the answer to "when do stuck/failed
// events get another attempt". Async; runs in its own goroutine.
func (m *Manager) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(m.retryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			events, err := m.stream.ReclaimStale(ctx, webhookGroup, m.consumerName(0), m.staleMinIdle)
			if err != nil {
				m.log.Warn("webhook periodic reclaim failed", zap.Error(err))
				continue
			}
			for _, ev := range events {
				m.process(ctx, ev)
			}
		}
	}
}

func (m *Manager) consumeLoop(ctx context.Context, consumer string) {
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := m.stream.ReadGroup(ctx, webhookGroup, consumer, streamReadBlock, streamReadCount)
		if err != nil {
			m.log.Warn("webhook stream read failed", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		for _, ev := range events {
			m.process(ctx, ev)
		}
	}
}

// process maps one stream entry to a webhook event, delivers it, then acks —
// in that order, so nothing is acked before its delivery outcome is decided.
func (m *Manager) process(ctx context.Context, ev redisstream.Event) {
	webEv, ok := m.buildEvent(ev)
	if !ok {
		m.dropped.Add(1)
		// Still ack the source entry so the consumer never loops on it.
		m.ack(ctx, ev.StreamID)
		return
	}
	// Global event filter (CUBE_LCM_WEBHOOK_EVENTS): consumed but not delivered.
	if !m.matchesGlobal(webEv.Event) {
		m.ack(ctx, ev.StreamID)
		return
	}
	// Detach delivery from the caller context so a graceful shutdown still
	// finishes what was read before the process exits.
	deliverCtx, cancel := context.WithTimeout(context.Background(), m.deliverTimeout)
	m.deliver(deliverCtx, webEv)
	cancel()
	m.ack(ctx, ev.StreamID)
}

func (m *Manager) matchesGlobal(eventType string) bool {
	return len(m.filter) == 0 || m.filter[eventType]
}

func (m *Manager) ack(ctx context.Context, id string) {
	if err := m.stream.Ack(ctx, webhookGroup, id); err != nil {
		m.log.Warn("webhook ack failed",
			zap.String("event_id", id), zap.Error(err))
	}
}

// buildEvent maps a lifecycle stream entry to a webhook Event, maintaining the
// meta cache so delete/state entries (which carry no payload) can still be
// enriched with template_id etc. Returns ok=false for unmappable entries.
func (m *Manager) buildEvent(ev redisstream.Event) (Event, bool) {
	switch ev.Op {
	case lifecycle.OpCreate:
		if ev.Meta == nil {
			return Event{}, false
		}
		m.metaCache.put(ev.SandboxID, *ev.Meta)
		return NewCreated(*ev.Meta, ev.StreamID, ev.Timestamp), true
	case lifecycle.OpUpdate:
		if ev.Meta == nil {
			return Event{}, false
		}
		m.metaCache.put(ev.SandboxID, *ev.Meta)
		return NewUpdated(*ev.Meta, ev.StreamID, ev.Timestamp), true
	case lifecycle.OpDelete:
		return NewDeleted(ev.SandboxID, m.metaCache.take(ev.SandboxID), ev.StreamID, ev.Timestamp), true
	case lifecycle.OpState:
		return NewState(ev.SandboxID, ev.State, m.metaCache.get(ev.SandboxID), ev.StreamID, ev.Timestamp)
	default:
		m.log.Warn("webhook: unknown event op",
			zap.String("op", ev.Op), zap.String("sandbox_id", ev.SandboxID))
		return Event{}, false
	}
}

// deliver sends one event to every subscribed endpoint against a snapshot of
// the fleet taken at delivery time.
func (m *Manager) deliver(ctx context.Context, ev Event) {
	m.mu.RLock()
	targets := make([]Endpoint, 0, len(m.endpoints))
	for _, ep := range m.endpoints {
		if ep.Enabled && ep.Matches(ev.Event) {
			targets = append(targets, ep)
		}
	}
	m.mu.RUnlock()

	for _, ep := range targets {
		if err := m.sender.Send(ctx, ep, ev); err != nil {
			m.failed.Add(1)
			m.log.Error("webhook delivery failed",
				zap.String("url", ep.URL),
				zap.String("event", ev.Event),
				zap.String("event_id", ev.EventID),
				zap.String("sandbox_id", ev.SandboxID),
				zap.Error(err))
			continue
		}
		m.delivered.Add(1)
	}
}

// Stats returns the running counters, useful for tests and /admin/webhooks/stats.
func (m *Manager) Stats() (dropped, delivered, failed uint64) {
	return m.dropped.Load(), m.delivered.Load(), m.failed.Load()
}

// Endpoints returns a defensive copy of the current endpoint list.
func (m *Manager) Endpoints() []Endpoint {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Endpoint, len(m.endpoints))
	copy(out, m.endpoints)
	return out
}

// AddEndpoint registers a new endpoint and returns the stored copy (with a
// generated ID when none was supplied).
func (m *Manager) AddEndpoint(ep Endpoint) (Endpoint, error) {
	if err := ValidateEndpoint(ep); err != nil {
		return Endpoint{}, err
	}
	if ep.ID == "" {
		ep.ID = uuid.NewString()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.endpoints {
		if e.ID == ep.ID {
			return Endpoint{}, fmt.Errorf("webhook endpoint id %q already exists", ep.ID)
		}
	}
	m.endpoints = append(m.endpoints, ep)
	return ep, nil
}

// UpdateEndpoint replaces an existing endpoint (preserving its ID).
func (m *Manager) UpdateEndpoint(id string, ep Endpoint) error {
	if err := ValidateEndpoint(ep); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, e := range m.endpoints {
		if e.ID == id {
			ep.ID = id
			m.endpoints[i] = ep
			return nil
		}
	}
	return fmt.Errorf("%w: id %q", ErrNotFound, id)
}

// DeleteEndpoint removes an endpoint by ID.
func (m *Manager) DeleteEndpoint(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, e := range m.endpoints {
		if e.ID == id {
			m.endpoints = append(m.endpoints[:i], m.endpoints[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: id %q", ErrNotFound, id)
}

// metaCache is a goroutine-safe sandbox_id → meta map used to enrich delete /
// state webhook events whose stream entries carry no payload.
type metaCache struct {
	mu sync.Mutex
	m  map[string]lifecycle.SandboxLifecycleMeta
}

func (c *metaCache) put(sandboxID string, meta lifecycle.SandboxLifecycleMeta) {
	c.mu.Lock()
	c.m[sandboxID] = meta
	c.mu.Unlock()
}

func (c *metaCache) get(sandboxID string) *lifecycle.SandboxLifecycleMeta {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta, ok := c.m[sandboxID]
	if !ok {
		return nil
	}
	return &meta
}

// take returns the cached meta and removes the entry (used on delete).
func (c *metaCache) take(sandboxID string) *lifecycle.SandboxLifecycleMeta {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta, ok := c.m[sandboxID]
	if !ok {
		return nil
	}
	delete(c.m, sandboxID)
	return &meta
}
