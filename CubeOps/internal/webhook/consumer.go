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
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// Lifecycle stream / meta keys (must match CubeMaster pkg/lifecycle/schema.go;
// CubeMaster is the single writer, CubeOps only consumes).
const (
	lifecycleStreamKey = "cube:v1:shared:sandbox:lifecycle:events"
	lifecycleMetaKey   = "cube:v1:shared:sandbox:lifecycle:meta"

	streamFieldOp        = "op"
	streamFieldSandboxID = "sandbox_id"
	streamFieldPayload   = "payload"
	streamFieldTimestamp = "ts"

	streamOpCreate = "create"
	streamOpDelete = "delete"
	streamOpUpdate = "update"
	streamOpState  = "state"

	// materializationFailureThreshold stops retrying a poison entry (persisted
	// cross-replica in t_webhook_materialization_failure).
	materializationFailureThreshold = 5
)

// Consumer reads the lifecycle Redis Stream and materializes pending delivery
// rows. It never sends HTTP; the supervisor claims and sends.
type Consumer struct {
	redis            redis.UniversalClient
	store            *DeliveryStore
	subs             *store.Store
	group            string
	name             string
	batch            int
	block            time.Duration
	window           time.Duration
	watermark        int
	maxSubsPerEvent  int
	matFailThreshold int
	autoclaimMinIdle time.Duration
	backlog          *BacklogCache

	healthy atomic.Bool
}

// NewConsumer wires a Consumer over the shared Redis client and stores.
func NewConsumer(
	rdb redis.UniversalClient,
	ds *DeliveryStore,
	subs *store.Store,
	group, consumerName string,
	batch int,
	block, keepPendingWindow, autoclaimMinIdle time.Duration,
	watermark, maxSubsPerEvent int,
	backlog *BacklogCache,
) *Consumer {
	return &Consumer{
		redis: rdb, store: ds, subs: subs,
		group: group, name: consumerName,
		batch: batch, block: block, window: keepPendingWindow,
		watermark: watermark, maxSubsPerEvent: maxSubsPerEvent,
		matFailThreshold: materializationFailureThreshold,
		autoclaimMinIdle: autoclaimMinIdle,
		backlog:          backlog,
	}
}

// Run is the main consumption loop. It owns the consumer group, applies
// global backlog backpressure, processes own-pending entries first (including
// XAUTOCLAIMed ones) and periodically reclaims idle pending entries.
func (c *Consumer) Run(ctx context.Context) {
	if err := c.ensureGroup(ctx); err != nil {
		logging.G(ctx).Errorf("webhook consumer: ensure group: %v", err)
		return
	}
	c.healthy.Store(true)
	_ = c.backlog.Refresh(ctx, c.store) // best-effort initial fill

	backlogTicker := time.NewTicker(5 * time.Second)
	defer backlogTicker.Stop()
	autoclaimTicker := time.NewTicker(5 * time.Minute)
	defer autoclaimTicker.Stop()

	backoff := 500 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-backlogTicker.C:
			if err := c.backlog.Refresh(ctx, c.store); err != nil {
				logging.G(ctx).Warnf("webhook consumer: backlog refresh: %v", err)
			}
		default:
		}

		// 1) Own pending entries first (they may include XAUTOCLAIMed work).
		if err := c.readAndProcess(ctx, "0"); err != nil {
			if ctx.Err() != nil {
				return
			}
			logging.G(ctx).Warnf("webhook consumer: pending read: %v", err)
			c.sleepBackoff(ctx, &backoff)
			continue
		}
		backoff = 500 * time.Millisecond

		// 2) Global backlog backpressure: pause reading new entries when the
		// actionable backlog (pending + retryable failed, window-excluded) is
		// at the watermark. This is the last line of defense; per-subscription
		// soft limits handle individual slow endpoints.
		if c.backlog.Global() >= int64(c.watermark) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.block):
			}
			continue
		}

		if err := c.readAndProcess(ctx, ">"); err != nil {
			if ctx.Err() != nil {
				return
			}
			logging.G(ctx).Warnf("webhook consumer: stream read: %v", err)
			c.sleepBackoff(ctx, &backoff)
			continue
		}

		select {
		case <-autoclaimTicker.C:
			c.autoclaim(ctx)
		default:
		}
	}
}

func (c *Consumer) readAndProcess(ctx context.Context, id string) error {
	streams, err := c.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.name,
		Streams:  []string{lifecycleStreamKey, id},
		Count:    int64(c.batch),
		Block:    c.block,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return err
	}
	for _, s := range streams {
		for _, msg := range s.Messages {
			if err := c.processEntry(ctx, msg); err != nil {
				return err
			}
		}
	}
	return nil
}

// processEntry materializes one stream entry into delivery rows, or ACKs it
// when there is nothing to deliver. A materialization failure is recorded in
// t_webhook_materialization_failure and the entry stays pending (no XACK)
// until the threshold is reached, then the event is isolated and ACKed.
func (c *Consumer) processEntry(ctx context.Context, msg redis.XMessage) error {
	eventID := msg.ID
	sandboxID := streamValue(msg.Values[streamFieldSandboxID])
	op := streamValue(msg.Values[streamFieldOp])

	// XACK-failure self-heal: isolation was already performed but the ACK did
	// not land; skip straight to ACK instead of re-materializing.
	attempts, err := c.store.MaterializationFailureAttempts(ctx, eventID)
	if err != nil {
		return err
	}
	if attempts >= c.matFailThreshold {
		return c.ack(ctx, eventID)
	}

	eventType, payload, err := c.buildPayload(ctx, op, sandboxID, msg)
	if err != nil {
		return c.recordMaterializationFailure(ctx, eventID, sandboxID, op, nil, err)
	}
	if eventType == "" {
		return c.ack(ctx, eventID) // update / unknown ops are not delivered
	}

	subs, err := c.subs.ListWebhookSubscriptionsByEventType(ctx, eventType)
	if err != nil {
		return err // SQL unavailable → stop reading, keep pending
	}
	ids := make([]int64, 0, len(subs))
	for _, s := range subs {
		ids = append(ids, s.ID)
	}
	if len(ids) == 0 {
		return c.ack(ctx, eventID)
	}

	if _, err := c.store.MaterializeDeliveries(ctx, eventID, payload, ids, c.maxSubsPerEvent); err != nil {
		return c.recordMaterializationFailure(ctx, eventID, sandboxID, op, payload, err)
	}
	return c.ack(ctx, eventID)
}

// buildPayload maps a lifecycle stream entry to the public webhook payload.
// Returns an empty event type when the op produces no delivery.
func (c *Consumer) buildPayload(ctx context.Context, op, sandboxID string, msg redis.XMessage) (string, []byte, error) {
	ts, _ := strconv.ParseInt(streamValue(msg.Values[streamFieldTimestamp]), 10, 64)
	if ts <= 0 {
		ts = time.Now().UnixMilli()
	}
	base := publicPayload{
		SchemaVersion: "1",
		EventID:       msg.ID,
		Timestamp:     ts,
		OccurredAt:    time.UnixMilli(ts).UTC().Format(time.RFC3339),
		SandboxID:     sandboxID,
	}

	raw := streamValue(msg.Values[streamFieldPayload])
	switch op {
	case streamOpCreate:
		base.Event = "sandbox.created"
		var meta struct {
			TemplateID string `json:"template_id"`
		}
		if raw != "" && json.Unmarshal([]byte(raw), &meta) == nil {
			base.TemplateID = meta.TemplateID
		}
	case streamOpDelete:
		base.Event = "sandbox.deleted"
		var dp struct {
			TemplateID string `json:"template_id"`
			Reason     string `json:"reason"`
		}
		if raw != "" && json.Unmarshal([]byte(raw), &dp) == nil {
			base.TemplateID = dp.TemplateID
			base.Reason = dp.Reason
		}
	case streamOpState:
		var sp struct {
			State  string `json:"state"`
			Source string `json:"source"`
		}
		if raw == "" || json.Unmarshal([]byte(raw), &sp) != nil {
			return "", nil, fmt.Errorf("state event %s: missing/invalid payload", msg.ID)
		}
		switch sp.State {
		case "paused":
			base.Event = "sandbox.paused"
		case "running":
			base.Event = "sandbox.resumed"
		default:
			return "", nil, fmt.Errorf("state event %s: unknown state %q", msg.ID, sp.State)
		}
		base.Source = sp.Source
		// state payloads carry no template_id; recover it from the lifecycle
		// meta snapshot. HGET errors and nil are treated the same: omit the
		// optional field rather than blocking the consumer.
		if v, err := c.redis.HGet(ctx, lifecycleMetaKey, sandboxID).Result(); err == nil && v != "" {
			var meta struct {
				TemplateID string `json:"template_id"`
			}
			if json.Unmarshal([]byte(v), &meta) == nil {
				base.TemplateID = meta.TemplateID
			}
		}
	case streamOpUpdate:
		return "", nil, nil
	default:
		return "", nil, nil
	}

	b, err := json.Marshal(base)
	if err != nil {
		return "", nil, err
	}
	return base.Event, b, nil
}

func (c *Consumer) recordMaterializationFailure(ctx context.Context, eventID, sandboxID, op string, payload []byte, cause error) error {
	n, err := c.store.RecordMaterializationFailure(ctx, eventID, sandboxID, nil, op, payload, cause.Error())
	if err != nil {
		return err // SQL outage → stop reading
	}
	if n >= c.matFailThreshold {
		// Best-effort isolation: rows still actionable for this event are
		// marked permanent_failed; already-sent rows cannot be recalled.
		if _, err := c.store.IsolateMaterializationFailure(ctx, eventID); err != nil {
			return err
		}
		return c.ack(ctx, eventID)
	}
	return nil // below threshold: keep pending, retry on the next read
}

func (c *Consumer) ack(ctx context.Context, eventID string) error {
	return c.redis.XAck(ctx, lifecycleStreamKey, c.group, eventID).Err()
}

func (c *Consumer) ensureGroup(ctx context.Context) error {
	err := c.redis.XGroupCreateMkStream(ctx, lifecycleStreamKey, c.group, "$").Err()
	if err == nil || strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

func (c *Consumer) autoclaim(ctx context.Context) {
	// min-idle ≥ 2×effective lease (same value as the claim lease) so tasks
	// still in normal flight are never stolen.
	msgs, _, err := c.redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   lifecycleStreamKey,
		Group:    c.group,
		Consumer: c.name,
		MinIdle:  c.autoclaimMinIdle,
		Start:    "0",
		Count:    int64(c.batch),
	}).Result()
	if err != nil {
		logging.G(ctx).Warnf("webhook consumer: xautoclaim: %v", err)
		return
	}
	if len(msgs) > 0 {
		logging.G(ctx).Infof("webhook consumer: xautoclaim reclaimed %d pending entries", len(msgs))
	}
}

// Healthy reports whether the consumer can serve: group exists and Redis is
// reachable.
func (c *Consumer) Healthy(ctx context.Context) bool {
	if !c.healthy.Load() {
		return false
	}
	if err := c.redis.Ping(ctx).Err(); err != nil {
		return false
	}
	_, err := c.redis.XInfoGroups(ctx, lifecycleStreamKey).Result()
	return err == nil
}

func (c *Consumer) sleepBackoff(ctx context.Context, backoff *time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(*backoff):
	}
	*backoff *= 2
	if *backoff > 30*time.Second {
		*backoff = 30 * time.Second
	}
}

func streamValue(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

// publicPayload is the delivery payload stored in t_webhook_delivery and
// signed verbatim. Field set matches the plan §6.3 event table.
type publicPayload struct {
	SchemaVersion string `json:"schema_version"`
	EventID       string `json:"event_id"`
	Event         string `json:"event"`
	Timestamp     int64  `json:"timestamp"`
	OccurredAt    string `json:"occurred_at"`
	SandboxID     string `json:"sandbox_id"`
	TemplateID    string `json:"template_id,omitempty"`
	Source        string `json:"source,omitempty"`
	Reason        string `json:"reason,omitempty"`
}
