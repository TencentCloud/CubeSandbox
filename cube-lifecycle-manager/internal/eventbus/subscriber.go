// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

// Subscriber owns the always-on Redis Pub/Sub subscription that feeds the
// in-process Bus. go-redis handles reconnecting and resubscribing.
type Subscriber struct {
	rdb redis.UniversalClient
	bus *Bus
	log *zap.Logger
}

// NewSubscriber constructs a Subscriber. The Bus + Redis client are
// borrowed, not owned; caller is responsible for Close on the client.
func NewSubscriber(rdb redis.UniversalClient, bus *Bus, log *zap.Logger) *Subscriber {
	return &Subscriber{rdb: rdb, bus: bus, log: log}
}

// Run keeps retrying subscription setup until the context is cancelled.
// Pub/Sub is an optimization, so a transient setup failure must not stop CLM.
func (s *Subscriber) Run(ctx context.Context) error {
	const retryDelay = time.Second
	for {
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.Warn("eventbus subscriber unavailable; retrying",
			zap.Duration("retry_delay", retryDelay),
			zap.Error(err))

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Subscriber) runOnce(ctx context.Context) error {
	pubsub := s.rdb.Subscribe(ctx, lifecycle.EventChannel)
	defer func() { _ = pubsub.Close() }()

	if _, err := pubsub.Receive(ctx); err != nil {
		return fmt.Errorf("subscribe %s: %w", lifecycle.EventChannel, err)
	}

	ch := pubsub.Channel(redis.WithChannelSize(256))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				return errChannelClosed
			}
			var ev lifecycle.StateNotify
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
				s.log.Warn("eventbus decode failed",
					zap.String("channel", msg.Channel),
					zap.Int("payload_bytes", len(msg.Payload)),
					zap.Error(err))
				continue
			}
			s.bus.Publish(ev.SandboxID)
		}
	}
}

var errChannelClosed = errors.New("pubsub channel closed")
