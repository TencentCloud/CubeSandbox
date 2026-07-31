// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

type eventDispatcher interface {
	Deliver(context.Context, LifecycleEvent) error
}

// Service consumes CubeMaster lifecycle events and delivers external
// Webhooks. It is independent from the CubeOps HTTP server request path.
type Service struct {
	source      eventSource
	dispatcher  eventDispatcher
	group       string
	consumer    string
	readBlock   time.Duration
	pendingIdle time.Duration
	workers     int
}

func New(redisURL string, cfg config.WebhookConfig) (*Service, error) {
	if cfg.ConsumerGroup == "" || cfg.Workers <= 0 || cfg.ReadBlock <= 0 || cfg.PendingIdle <= 0 {
		return nil, fmt.Errorf("invalid webhook consumer configuration")
	}
	source, err := newRedisSource(redisURL)
	if err != nil {
		return nil, err
	}
	delivery, err := newDispatcher(cfg)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	consumer := cfg.ConsumerName
	if consumer == "" {
		hostname, _ := os.Hostname()
		consumer = fmt.Sprintf("%s-%d-%s", hostname, os.Getpid(), uuid.NewString()[:8])
	}
	return &Service{
		source:      source,
		dispatcher:  delivery,
		group:       cfg.ConsumerGroup,
		consumer:    consumer,
		readBlock:   cfg.ReadBlock,
		pendingIdle: cfg.PendingIdle,
		workers:     cfg.Workers,
	}, nil
}

func (s *Service) Close() error {
	if s == nil || s.source == nil {
		return nil
	}
	return s.source.Close()
}

func (s *Service) Run(ctx context.Context) error {
	if err := s.ensureGroup(ctx); err != nil {
		return err
	}

	jobs := make([]chan LifecycleEvent, s.workers)
	var workers sync.WaitGroup
	for i := 0; i < s.workers; i++ {
		jobs[i] = make(chan LifecycleEvent, 2)
		workers.Add(1)
		go func(queue <-chan LifecycleEvent) {
			defer workers.Done()
			s.worker(ctx, queue)
		}(jobs[i])
	}
	defer func() {
		for _, queue := range jobs {
			close(queue)
		}
		workers.Wait()
	}()

	slog.Info("webhook consumer started",
		"stream", EventStreamKey,
		"group", s.group,
		"consumer", s.consumer,
		"workers", s.workers)

	claimTicker := time.NewTicker(s.pendingIdle)
	defer claimTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-claimTicker.C:
			if !s.reclaimPending(ctx, jobs) {
				return nil
			}
		default:
			events, err := s.source.Read(ctx, s.group, s.consumer, s.readBlock, 100)
			if err != nil {
				slog.Warn("webhook stream read failed", "error", err)
				if !waitContext(ctx, time.Second) {
					return nil
				}
				continue
			}
			if !enqueue(ctx, jobs, events) {
				return nil
			}
		}
	}
}

// reclaimPending scans the whole pending-entry list from oldest to newest.
// XAUTOCLAIM returns a cursor for the next page; restarting each request at
// 0-0 would continually reclaim the oldest page and starve later entries.
func (s *Service) reclaimPending(ctx context.Context, jobs []chan LifecycleEvent) bool {
	start := "0-0"
	for {
		events, nextStart, err := s.source.Claim(ctx, s.group, s.consumer, s.pendingIdle, start, 100)
		if err != nil {
			slog.Warn("webhook pending reclaim failed", "error", err)
			return true
		}
		if !enqueue(ctx, jobs, events) {
			return false
		}
		if nextStart == "0-0" {
			return true
		}
		if nextStart == start {
			slog.Warn("webhook pending reclaim cursor did not advance", "start", start)
			return true
		}
		start = nextStart
	}
}

func (s *Service) ensureGroup(ctx context.Context) error {
	for {
		if err := s.source.EnsureGroup(ctx, s.group); err != nil {
			slog.Warn("webhook consumer group unavailable", "error", err)
			if !waitContext(ctx, time.Second) {
				return nil
			}
			continue
		}
		return nil
	}
}

func (s *Service) worker(ctx context.Context, jobs <-chan LifecycleEvent) {
	for event := range jobs {
		if ctx.Err() != nil {
			slog.Info("webhook event retained after consumer cancellation",
				"stream_id", event.StreamID,
				"event_id", event.EventID)
			continue
		}
		if event.Op == "" || event.SandboxID == "" {
			slog.Warn("discarding malformed lifecycle event",
				"stream_id", event.StreamID)
		} else if err := s.dispatcher.Deliver(ctx, event); err != nil {
			slog.Error("webhook delivery exhausted retries",
				"event_id", event.EventID,
				"stream_id", event.StreamID,
				"sandbox_id", event.SandboxID,
				"error", err)
		}
		if ctx.Err() != nil {
			slog.Info("webhook event retained after delivery cancellation",
				"stream_id", event.StreamID,
				"event_id", event.EventID)
			continue
		}
		// Delivery failures are acknowledged after the configured retry budget
		// is exhausted. Process crashes before this point leave the entry in
		// the PEL, where another CubeOps replica can reclaim it.
		if err := s.source.Ack(ctx, s.group, event.StreamID); err != nil {
			slog.Warn("webhook stream ack failed",
				"stream_id", event.StreamID,
				"event_id", event.EventID,
				"error", err)
		}
	}
}

func enqueue(ctx context.Context, jobs []chan LifecycleEvent, events []LifecycleEvent) bool {
	for _, event := range events {
		queue := jobs[workerIndex(event, len(jobs))]
		select {
		case queue <- event:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func workerIndex(event LifecycleEvent, workers int) int {
	key := event.SandboxID
	if key == "" {
		key = event.StreamID
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(key))
	return int(hash.Sum32() % uint32(workers))
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
