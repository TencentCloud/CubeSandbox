// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type eventSource interface {
	EnsureGroup(context.Context, string) error
	Read(context.Context, string, string, time.Duration, int64) ([]LifecycleEvent, error)
	Claim(context.Context, string, string, time.Duration, int64) ([]LifecycleEvent, error)
	Ack(context.Context, string, string) error
	Close() error
}

type redisSource struct {
	client *redis.Client
}

func newRedisSource(redisURL string) (*redisSource, error) {
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis_url: %w", err)
	}
	return &redisSource{client: redis.NewClient(options)}, nil
}

func (s *redisSource) EnsureGroup(ctx context.Context, group string) error {
	err := s.client.XGroupCreateMkStream(ctx, EventStreamKey, group, "$").Err()
	if err == nil || strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return fmt.Errorf("create webhook consumer group: %w", err)
}

func (s *redisSource) Read(
	ctx context.Context,
	group, consumer string,
	block time.Duration,
	count int64,
) ([]LifecycleEvent, error) {
	streams, err := s.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{EventStreamKey, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if errors.Is(err, redis.Nil) || errors.Is(err, context.DeadlineExceeded) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read lifecycle stream: %w", err)
	}
	return decodeStreams(streams), nil
}

func (s *redisSource) Claim(
	ctx context.Context,
	group, consumer string,
	minIdle time.Duration,
	count int64,
) ([]LifecycleEvent, error) {
	messages, _, err := s.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   EventStreamKey,
		Group:    group,
		Consumer: consumer,
		MinIdle:  minIdle,
		Start:    "0-0",
		Count:    count,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim pending lifecycle events: %w", err)
	}
	events := make([]LifecycleEvent, 0, len(messages))
	for _, message := range messages {
		event, _ := decodeMessage(message)
		events = append(events, event)
	}
	return events, nil
}

func (s *redisSource) Ack(ctx context.Context, group, streamID string) error {
	return s.client.XAck(ctx, EventStreamKey, group, streamID).Err()
}

func (s *redisSource) Close() error {
	return s.client.Close()
}

func decodeStreams(streams []redis.XStream) []LifecycleEvent {
	var events []LifecycleEvent
	for _, stream := range streams {
		for _, message := range stream.Messages {
			event, _ := decodeMessage(message)
			events = append(events, event)
		}
	}
	return events
}

func decodeMessage(message redis.XMessage) (LifecycleEvent, bool) {
	op := stringValue(message.Values["op"])
	sandboxID := stringValue(message.Values["sandbox_id"])
	event := LifecycleEvent{
		StreamID:  message.ID,
		EventID:   stringValue(message.Values["event_id"]),
		Op:        op,
		SandboxID: sandboxID,
	}
	if rawTimestamp := stringValue(message.Values["ts"]); rawTimestamp != "" {
		event.Timestamp, _ = strconv.ParseInt(rawTimestamp, 10, 64)
	}
	if rawPayload := stringValue(message.Values["payload"]); rawPayload != "" {
		event.Payload = json.RawMessage(rawPayload)
	}
	return event, op != "" && sandboxID != ""
}

func stringValue(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case int:
		return strconv.Itoa(value)
	default:
		return ""
	}
}
