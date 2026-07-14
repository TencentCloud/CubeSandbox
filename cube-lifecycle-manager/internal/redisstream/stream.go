// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package redisstream owns every interaction with the lifecycle Redis schema:
// the meta HSet bootstrap, the events stream consumer, and the per-sandbox
// state locks used to serialize pause/resume across sidecar instances.
package redisstream

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

// Client wraps a go-redis client with lifecycle-shaped methods.
type Client struct {
	rdb *redis.Client
	log *zap.Logger
}

func New(rdb *redis.Client, log *zap.Logger) *Client {
	return &Client{rdb: rdb, log: log}
}

// Bootstrap returns every sandbox in cube:v1:shared:sandbox:lifecycle:meta.
// Empty result is fine — it just means CubeMaster hasn't published anything yet.
func (c *Client) Bootstrap(ctx context.Context) (map[string]lifecycle.SandboxLifecycleMeta, error) {
	raw, err := c.rdb.HGetAll(ctx, lifecycle.MetaKey).Result()
	if err != nil {
		return nil, fmt.Errorf("hgetall %s: %w", lifecycle.MetaKey, err)
	}
	out := make(map[string]lifecycle.SandboxLifecycleMeta, len(raw))
	for sid, payload := range raw {
		var meta lifecycle.SandboxLifecycleMeta
		if err := json.Unmarshal([]byte(payload), &meta); err != nil {
			c.log.Warn("bootstrap: skipping bad meta entry",
				zap.String("sandbox_id", sid), zap.Error(err))
			continue
		}
		// Defensive: ensure sandbox_id matches the hash field even if the
		// payload happens to have a different value — we trust the field.
		meta.SandboxID = sid
		out[sid] = meta
	}
	return out, nil
}

// EnsureGroup creates the consumer group on the events stream, ignoring
// "BUSYGROUP" (group already exists) errors. MKSTREAM lets the group be
// created before any events have been published.
func (c *Client) EnsureGroup(ctx context.Context, group string) error {
	err := c.rdb.XGroupCreateMkStream(ctx, lifecycle.EventStreamKey, group, "$").Err()
	if err == nil {
		return nil
	}
	// go-redis surfaces BUSYGROUP as a generic error with a known message.
	if isBusyGroup(err) {
		return nil
	}
	return fmt.Errorf("xgroup create mkstream: %w", err)
}

// Event is a decoded entry from the events stream.
type Event struct {
	StreamID  string
	Op        string // create | delete | update | state
	SandboxID string
	// Meta is populated for create/update events (delete carries only the
	// sandbox ID).
	Meta *lifecycle.SandboxLifecycleMeta
	// State is populated for state events emitted by CubeMaster after a
	// successful pause / resume RPC. Consumed by statesync.
	State     *lifecycle.StatePayload
	Timestamp int64
}

// ReadGroup blocks for up to `block` waiting for new events on the stream.
// Returns when at least one entry arrives, when the context is cancelled, or
// when the block timeout expires (in which case it returns an empty slice and
// nil error — the caller loops).
func (c *Client) ReadGroup(ctx context.Context, group, consumer string, block time.Duration, count int) ([]Event, error) {
	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{lifecycle.EventStreamKey, ">"},
		Count:    int64(count),
		Block:    block,
	}).Result()

	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		// Block-timeout shows up as a context-deadline-ish error from
		// go-redis when no entries arrive and BLOCK > 0; treat as empty.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, nil
		}
		return nil, fmt.Errorf("xreadgroup: %w", err)
	}

	var out []Event
	for _, stream := range res {
		for _, msg := range stream.Messages {
			ev := decodeEvent(msg)
			if ev != nil {
				out = append(out, *ev)
			} else {
				c.log.Warn("redisstream: dropping unparseable event",
					zap.String("id", msg.ID), zap.Any("values", msg.Values))
				// Still ack so we don't loop on it.
				_ = c.Ack(ctx, group, msg.ID)
			}
		}
	}
	return out, nil
}

// Ack marks the event as processed so it leaves the consumer's pending list.
func (c *Client) Ack(ctx context.Context, group, id string) error {
	return c.rdb.XAck(ctx, lifecycle.EventStreamKey, group, id).Err()
}

// AcquireState performs a SET NX EX on the per-sandbox lifecycle state key with
// the supplied desired state. Returns true on success. Used to coordinate
// concurrent pause/resume across sidecars: whoever wins the SETNX owns the
// transition.
func (c *Client) AcquireState(ctx context.Context, sandboxID, state string, ttl time.Duration) (bool, error) {
	key := lifecycle.StateKey(sandboxID)
	ok, err := c.rdb.SetNX(ctx, key, state, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("setnx %s: %w", key, err)
	}
	return ok, nil
}

// SetState forces the state value (overwriting any existing). Used to
// transition pausing → paused or resuming → running once the underlying
// operation has actually completed.
func (c *Client) SetState(ctx context.Context, sandboxID, state string, ttl time.Duration) error {
	key := lifecycle.StateKey(sandboxID)
	return c.rdb.Set(ctx, key, state, ttl).Err()
}

// ClearState drops the key altogether. Used on rollback (operation failed)
// and on sandbox delete.
func (c *Client) ClearState(ctx context.Context, sandboxID string) error {
	key := lifecycle.StateKey(sandboxID)
	return c.rdb.Del(ctx, key).Err()
}

// GetState returns the current state and whether the key exists.
func (c *Client) GetState(ctx context.Context, sandboxID string) (string, bool, error) {
	key := lifecycle.StateKey(sandboxID)
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func decodeEvent(msg redis.XMessage) *Event {
	op, _ := msg.Values[lifecycle.FieldOp].(string)
	sid, _ := msg.Values[lifecycle.FieldSandboxID].(string)
	if op == "" || sid == "" {
		return nil
	}
	ev := &Event{
		StreamID:  msg.ID,
		Op:        op,
		SandboxID: sid,
	}
	if ts, ok := msg.Values[lifecycle.FieldTimestamp].(string); ok {
		// CubeMaster writes the millisecond unix timestamp; tolerate both
		// string and numeric forms.
		var t int64
		if _, err := fmt.Sscanf(ts, "%d", &t); err == nil {
			ev.Timestamp = t
		}
	}
	switch op {
	case lifecycle.OpCreate, lifecycle.OpUpdate:
		if payload, ok := msg.Values[lifecycle.FieldPayload].(string); ok && payload != "" {
			var meta lifecycle.SandboxLifecycleMeta
			if err := json.Unmarshal([]byte(payload), &meta); err == nil {
				meta.SandboxID = sid
				ev.Meta = &meta
			}
		}
	case lifecycle.OpState:
		if payload, ok := msg.Values[lifecycle.FieldPayload].(string); ok && payload != "" {
			var sp lifecycle.StatePayload
			if err := json.Unmarshal([]byte(payload), &sp); err == nil {
				ev.State = &sp
			}
			// If unmarshal fails ev.State stays nil; downstream
			// statesync warns and drops, keeping the stream consumer
			// resilient to schema drift.
		}
	}
	return ev
}

func isBusyGroup(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == "BUSYGROUP Consumer Group name already exists"
}
