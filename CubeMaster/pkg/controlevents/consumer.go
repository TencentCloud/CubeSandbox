// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package controlevents

import (
	"context"
	"fmt"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
)

const (
	defaultBlockMs = 5000
	defaultCount   = 32
)

// Handler applies a decoded control-plane event to the local process view.
// Implementations must be idempotent and must not write MySQL.
type Handler func(ctx context.Context, ev Event) error

// Consumer independently XREADs the control stream starting from "$" so every
// Cubemaster replica observes every new event (broadcast). Missed events are
// recovered via nodemeta's periodic DB reload.
type Consumer struct {
	pool    *redis.Pool
	handler Handler
	blockMs int
	count   int
}

// NewConsumer builds a Consumer. pool must be non-nil; handler may be nil
// (events are then logged and dropped).
func NewConsumer(pool *redis.Pool, handler Handler) *Consumer {
	return &Consumer{
		pool:    pool,
		handler: handler,
		blockMs: defaultBlockMs,
		count:   defaultCount,
	}
}

// Run blocks until ctx is cancelled. It starts reading at "$" (new events only).
func (c *Consumer) Run(ctx context.Context) {
	if c == nil || c.pool == nil {
		return
	}
	lastID := "$"
	log.G(ctx).Infof("controlevents: consumer started stream=%s from=%s", EventStreamKey, lastID)
	for {
		select {
		case <-ctx.Done():
			log.G(ctx).Infof("controlevents: consumer stopped: %v", ctx.Err())
			return
		default:
		}

		events, nextID, err := c.readOnce(ctx, lastID)
		if err != nil {
			log.G(ctx).Warnf("controlevents: XREAD failed: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if nextID != "" {
			lastID = nextID
		}
		for _, ev := range events {
			c.dispatch(ctx, ev)
		}
	}
}

func (c *Consumer) dispatch(ctx context.Context, ev Event) {
	defer recov.HandleCrash(func(r interface{}) {
		log.G(ctx).Errorf("controlevents: handler panic op=%s node=%s: %v", ev.Op, ev.NodeID, r)
	})
	if c.handler == nil {
		return
	}
	if err := c.handler(ctx, ev); err != nil {
		log.G(ctx).Warnf("controlevents: handler failed op=%s node=%s: %v", ev.Op, ev.NodeID, err)
	}
}

func (c *Consumer) readOnce(ctx context.Context, lastID string) ([]Event, string, error) {
	conn := c.pool.Get()
	defer conn.Close()
	if err := conn.Err(); err != nil {
		return nil, "", err
	}

	reply, err := conn.Do("XREAD",
		"BLOCK", c.blockMs,
		"COUNT", c.count,
		"STREAMS", EventStreamKey, lastID,
	)
	if err == redis.ErrNil || reply == nil {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}

	events, nextID, err := parseXReadReply(reply)
	if err != nil {
		return nil, "", err
	}
	_ = ctx
	return events, nextID, nil
}

// parseXReadReply decodes a redigo XREAD reply into events and the last stream ID.
// Reply shape: [[streamName, [[id, [k, v, ...]], ...]]]
func parseXReadReply(reply interface{}) ([]Event, string, error) {
	streams, err := redis.Values(reply, nil)
	if err != nil {
		return nil, "", fmt.Errorf("decode streams: %w", err)
	}
	if len(streams) == 0 {
		return nil, "", nil
	}

	var out []Event
	var lastID string
	for _, streamRaw := range streams {
		stream, err := redis.Values(streamRaw, nil)
		if err != nil || len(stream) < 2 {
			continue
		}
		entries, err := redis.Values(stream[1], nil)
		if err != nil {
			return nil, "", fmt.Errorf("decode entries: %w", err)
		}
		for _, entryRaw := range entries {
			entry, err := redis.Values(entryRaw, nil)
			if err != nil || len(entry) < 2 {
				continue
			}
			id, err := redis.String(entry[0], nil)
			if err != nil {
				continue
			}
			fields, err := redis.Values(entry[1], nil)
			if err != nil {
				continue
			}
			ev := Event{StreamID: id}
			for i := 0; i+1 < len(fields); i += 2 {
				k, _ := redis.String(fields[i], nil)
				switch k {
				case FieldOp:
					ev.Op, _ = redis.String(fields[i+1], nil)
				case FieldNodeID:
					ev.NodeID, _ = redis.String(fields[i+1], nil)
				case FieldPayload:
					ev.Payload, _ = redis.Bytes(fields[i+1], nil)
				case FieldTimestamp:
					ev.Timestamp, _ = redis.Int64(fields[i+1], nil)
				}
			}
			out = append(out, ev)
			lastID = id
		}
	}
	return out, lastID, nil
}
