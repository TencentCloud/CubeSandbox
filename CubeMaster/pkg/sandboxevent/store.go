// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sandboxevent keeps the lifecycle history of a sandbox (created,
// paused, resumed, killed). Schema lives in pkgs/cubedb/migrate.
//
// Recording never affects the sandbox operation that produced the event:
// events are queued and written by one background worker, and are dropped
// with a warning when the queue is full or the write fails. Before Init has
// succeeded nothing is recorded; the hooks that call Record are only
// registered after it.
package sandboxevent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

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
)

var ErrNotReady = errors.New("sandbox event store is not initialized")

var (
	mu    sync.RWMutex
	db    *gorm.DB
	queue chan *models.SandboxEvent
)

// Init attaches the shared gorm handle and starts the writer. The table is
// created by cubedb migrations; when it is missing (CUBE_AUTO_MIGRATION=false
// with the DDL not applied yet) Init fails and the history stays disabled
// until the next restart. Calling Init again only swaps the handle.
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
	if queue == nil {
		queue = make(chan *models.SandboxEvent, queueSize)
		go writeLoop(queue)
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
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" || event == "" {
		return
	}
	mu.RLock()
	q := queue
	mu.RUnlock()
	if q == nil {
		return
	}
	ev := &models.SandboxEvent{
		SandboxID:  sandboxID,
		Event:      event,
		TemplateID: templateID,
		Reason:     reason,
		CreatedAt:  time.Now(),
	}
	select {
	case q <- ev:
	default:
		log.G(ctx).Warnf("sandboxevent: queue full, dropping %s event of sandbox %s", event, sandboxID)
	}
}

// One worker keeps the row order equal to the order events were recorded by
// this process. With several CubeMaster replicas, events of one sandbox
// recorded on different replicas can be inserted slightly out of order (only
// the enqueue-to-insert lag can invert them, since the sandbox lock serializes
// the operations), so readers that need a strict order should sort by
// CreatedAt, which is stamped when the event is recorded.
func writeLoop(q <-chan *models.SandboxEvent) {
	for ev := range q {
		write(context.Background(), ev)
	}
}

func write(ctx context.Context, ev *models.SandboxEvent) {
	client := getDB()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := client.WithContext(ctx).Create(ev).Error; err != nil {
		log.G(ctx).Warnf("sandboxevent: dropping %s event of sandbox %s: %v", ev.Event, ev.SandboxID, err)
	}
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
		Where("sandbox_id = ? AND id > ?", strings.TrimSpace(sandboxID), cursor).
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
