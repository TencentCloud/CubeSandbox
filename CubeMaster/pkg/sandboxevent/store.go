// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sandboxevent keeps the lifecycle history of a sandbox (created,
// paused, resumed, killed). Schema lives in pkgs/cubedb/migrate.
//
// Recording never affects the sandbox operation that produced the event:
// events are queued and written by one background worker, and are dropped
// (with a warning) when the store is not initialised, the queue is full or
// the write fails.
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
// created by cubedb migrations. Calling Init again only swaps the handle.
func Init(client *gorm.DB) error {
	if client == nil {
		return errors.New("sandboxevent.Init: nil db")
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

// One worker keeps the row order equal to the order events were recorded.
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
