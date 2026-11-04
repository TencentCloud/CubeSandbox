// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sandboxevent keeps the lifecycle history of a sandbox (created,
// paused, resumed, killed). Schema lives in pkgs/cubedb/migrate.
//
// Recording never affects the sandbox operation that produced the event:
// events are queued and written in batches by one background worker. An
// event is dropped, with a warning and a count in
// sandbox_event_dropped_total{reason}, when the store is not ready, the
// queue is full, the write fails, or CubeMaster is shutting down.
package sandboxevent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"gorm.io/gorm"
)

// Lifecycle event names.
const (
	EventCreated = "created"
	EventPaused  = "paused"
	EventResumed = "resumed"
	EventKilled  = "killed"
)

const (
	// DefaultListLimit and MaxListLimit bound one List page.
	DefaultListLimit = 100
	MaxListLimit     = 500

	queueSize    = 1024
	writeTimeout = 3 * time.Second

	// A batch is written when it holds maxBatch events or batchWait after
	// its first event, whichever comes first.
	maxBatch  = 100
	batchWait = 200 * time.Millisecond
)

// Drop reasons, the values of the reason label of droppedTotal.
const (
	dropNotReady    = "not_ready"
	dropQueueFull   = "queue_full"
	dropWriteFailed = "write_failed"
	dropShutdown    = "shutdown"
)

// droppedTotal counts events that were never written, so operators can tell
// an incomplete history from a quiet sandbox.
var droppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "sandbox_event_dropped_total",
	Help: "Total sandbox lifecycle events dropped without being written, by reason.",
}, []string{"reason"})

var ErrNotReady = errors.New("sandbox event store is not initialized")

var (
	mu         sync.RWMutex
	db         *gorm.DB
	queue      chan *models.SandboxEvent
	writerDone chan struct{}
	// stopped is set by Stop, so late events count as shutdown drops
	// rather than as an unready store.
	stopped bool
)

// Init attaches the shared gorm handle and starts the writer. The table is
// created by cubedb migrations; when it is missing (CUBE_AUTO_MIGRATION=false
// with the DDL not applied yet) Init fails and the caller may retry. Calling
// Init again only swaps the handle.
func Init(client *gorm.DB) error {
	if client == nil {
		return errors.New("sandboxevent.Init: nil db")
	}
	if !client.Migrator().HasTable(&models.SandboxEvent{}) {
		return errors.New("sandboxevent.Init: table t_cube_sandbox_event does not exist")
	}
	mu.Lock()
	defer mu.Unlock()
	db = client
	stopped = false
	if queue == nil {
		queue = make(chan *models.SandboxEvent, queueSize)
		writerDone = make(chan struct{})
		go writeLoop(queue, writerDone)
	}
	return nil
}

func getDB() *gorm.DB {
	mu.RLock()
	defer mu.RUnlock()
	return db
}

// Record queues one event without blocking the caller.
func Record(ctx context.Context, sandboxID, event, templateID, reason string) {
	sandboxID = normalizeID(sandboxID)
	if sandboxID == "" || event == "" {
		return
	}
	// The read lock is held across the send so Stop cannot close the queue
	// under it.
	mu.RLock()
	defer mu.RUnlock()
	q := queue
	if q == nil {
		if stopped {
			droppedTotal.WithLabelValues(dropShutdown).Inc()
		} else {
			droppedTotal.WithLabelValues(dropNotReady).Inc()
		}
		return
	}
	ev := &models.SandboxEvent{
		SandboxID:  sandboxID,
		Event:      event,
		TemplateID: truncate(templateID, maxTemplateIDLen),
		Reason:     truncate(reason, maxReasonLen),
		CreatedAt:  time.Now(),
	}
	select {
	case q <- ev:
	default:
		droppedTotal.WithLabelValues(dropQueueFull).Inc()
		log.G(ctx).Warnf("sandboxevent: queue full, dropping %s event of sandbox %s", event, sandboxID)
	}
}

// Column widths of t_cube_sandbox_event. Longer values would make the insert
// fail and drop the whole event, so they are cut instead.
const (
	maxTemplateIDLen = 128
	maxReasonLen     = 64
)

// truncate cuts s to at most n bytes without splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// One worker keeps the row order equal to the order events were recorded by
// this process. With several CubeMaster replicas, events of one sandbox
// recorded on different replicas can be inserted slightly out of order (only
// the enqueue-to-insert lag can invert them, since the sandbox lock serializes
// the operations), so readers that need a strict order should sort by
// CreatedAt, which is stamped when the event is recorded.
func writeLoop(q <-chan *models.SandboxEvent, done chan<- struct{}) {
	defer close(done)
	batch := make([]*models.SandboxEvent, 0, maxBatch)
	timer := time.NewTimer(batchWait)
	timer.Stop()
	flush := func() {
		if len(batch) > 0 {
			write(context.Background(), batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case ev, ok := <-q:
			if !ok {
				timer.Stop()
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) == 1 {
				timer.Reset(batchWait)
			}
			if len(batch) == maxBatch {
				timer.Stop()
				flush()
			}
		case <-timer.C:
			flush()
		}
	}
}

// Stop stops accepting events and waits, until ctx is done, for the queued
// ones to be written. Call it on shutdown after in-flight sandbox operations
// have finished, so their events (notably kills) are not lost.
func Stop(ctx context.Context) {
	mu.Lock()
	q, done := queue, writerDone
	queue, writerDone = nil, nil
	stopped = true
	mu.Unlock()
	if q == nil {
		return
	}
	close(q)
	select {
	case <-done:
	case <-ctx.Done():
		n := len(q)
		droppedTotal.WithLabelValues(dropShutdown).Add(float64(n))
		log.G(ctx).Warnf("sandboxevent: stopped before %d queued events were written", n)
	}
}

// Sandbox IDs are lowercase hex; matching is case-insensitive, which
// PostgreSQL varchar comparison is not.
func normalizeID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// write inserts a batch in one statement. When that fails it retries the
// rows one by one, so a single bad row does not take the batch with it.
func write(ctx context.Context, batch []*models.SandboxEvent) {
	client := getDB()
	if client == nil {
		droppedTotal.WithLabelValues(dropNotReady).Add(float64(len(batch)))
		return
	}
	err := insert(ctx, client, batch)
	if err == nil {
		return
	}
	if len(batch) == 1 {
		reportDropped(ctx, batch[0], err)
		return
	}
	for _, ev := range batch {
		if err := insert(ctx, client, ev); err != nil {
			reportDropped(ctx, ev, err)
		}
	}
}

func insert(ctx context.Context, client *gorm.DB, rows any) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return client.WithContext(ctx).Create(rows).Error
}

func reportDropped(ctx context.Context, ev *models.SandboxEvent, err error) {
	droppedTotal.WithLabelValues(dropWriteFailed).Inc()
	log.G(ctx).Warnf("sandboxevent: dropping %s event of sandbox %s: %v", ev.Event, ev.SandboxID, err)
}

// List returns the sandbox's events oldest first, starting after the event
// whose ID is cursor (0 for the beginning), and whether more follow.
//
// Paging is best effort: with several replicas an insert that got a lower ID
// can commit after a reader already moved its cursor past a higher one, and
// that event is then not returned by later pages.
func List(ctx context.Context, sandboxID string, cursor uint64, limit int) ([]models.SandboxEvent, bool, error) {
	client := getDB()
	if client == nil {
		return nil, false, ErrNotReady
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	var rows []models.SandboxEvent
	err := client.WithContext(ctx).
		Where("sandbox_id = ? AND id > ?", normalizeID(sandboxID), cursor).
		Order("id ASC").
		Limit(limit + 1).
		Find(&rows).Error
	if err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	return rows, hasMore, nil
}

// ResetForTest detaches the store so List reports ErrNotReady. Test-only.
func ResetForTest() {
	mu.Lock()
	db = nil
	mu.Unlock()
}

type killReasonKey struct{}

// WithKillReason carries the kill reason of a destroy to the after-destroy
// hooks, which only receive the sandbox ID.
func WithKillReason(ctx context.Context, reason string) context.Context {
	if reason == "" {
		return ctx
	}
	return context.WithValue(ctx, killReasonKey{}, reason)
}

// KillReason returns the reason set by WithKillReason, or "".
func KillReason(ctx context.Context) string {
	reason, _ := ctx.Value(killReasonKey{}).(string)
	return reason
}
