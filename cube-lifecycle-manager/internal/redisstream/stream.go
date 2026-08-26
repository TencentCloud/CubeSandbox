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

// notifyPublishTimeout bounds the best-effort Redis PUBLISH. It is detached
// from the caller context so a cancelled request cannot drop the hint.
const notifyPublishTimeout = 500 * time.Millisecond

// Client wraps a go-redis client with lifecycle-shaped methods.
type Client struct {
	rdb redis.UniversalClient
	log *zap.Logger

	// notifyEnabled toggles the Pub/Sub companion emitted alongside every
	// terminal state write. Off by default so a rollout can enable the
	// subscriber side first; flipped on via SetNotifyEnabled(true) from
	// main once Config.EventBusEnabled is true.
	notifyEnabled bool
	// localBus, when non-nil, receives every StateNotify locally before it
	// hits Redis. Same-replica waiters wake without a Pub/Sub round-trip.
	// Nil is legal and simply means "publish to Redis only".
	localBus localPublisher
}

// localPublisher is the subset of eventbus.Bus we need. Kept as an
// interface here so redisstream does not import eventbus and create a
// dependency cycle (eventbus already imports lifecycle; main.go stitches
// the two together).
type localPublisher interface {
	Publish(sandboxID string)
}

func New(rdb redis.UniversalClient, log *zap.Logger) *Client {
	return &Client{rdb: rdb, log: log}
}

// SetNotifyEnabled toggles the Pub/Sub publish that accompanies terminal
// state writes (WriteState and the CommitTransition / ReleaseTransition
// settle paths). When disabled, those methods degrade to their plain Redis
// mutation. Safe to call at startup only.
func (c *Client) SetNotifyEnabled(on bool) { c.notifyEnabled = on }

// SetLocalBus wires a same-process fan-out that receives every StateNotify
// synchronously alongside the Redis PUBLISH. Same-replica waiters can
// therefore be woken with zero Redis round-trip. Passing nil disables the
// local fan-out. Safe to call at startup only.
func (c *Client) SetLocalBus(bus localPublisher) { c.localBus = bus }

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

// LookupMeta returns the meta for a single sandbox from the meta HSet.
// (nil, nil) means the field is absent — CubeMaster doesn't know the sandbox
// (anymore). Used by the resumer's registry-miss fallback so a
// freshly-promoted leader whose bootstrap hasn't landed yet (or a standby
// reached directly via pod IP) can still serve resume requests without
// waiting for the stream consumer to catch up.
func (c *Client) LookupMeta(ctx context.Context, sandboxID string) (*lifecycle.SandboxLifecycleMeta, error) {
	payload, err := c.rdb.HGet(ctx, lifecycle.MetaKey, sandboxID).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hget %s %s: %w", lifecycle.MetaKey, sandboxID, err)
	}
	var meta lifecycle.SandboxLifecycleMeta
	if err := json.Unmarshal([]byte(payload), &meta); err != nil {
		return nil, fmt.Errorf("unmarshal meta for %s: %w", sandboxID, err)
	}
	meta.SandboxID = sandboxID
	return &meta, nil
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

// ClaimPending transfers stream entries that have been pending for longer
// than minIdle — i.e. stuck on a dead or demoted consumer — to `consumer`,
// returning them for processing. In active-standby mode the new leader uses
// this to take over the previous leader's pending-entries list
// (issue #1211); minIdle must be ≥ the leader lease TTL (enforced by config
// validation when set through CUBE_LCM_*) so entries a live consumer is
// actively working on are never stolen. The caller must handle
// and Ack the returned events just like ReadGroup output. Requires Redis
// 6.2+ (XAUTOCLAIM).
//
// One call drains the entire backlog: XAUTOCLAIM scans the PEL in batches
// of `count` and returns a cursor, which we thread across passes until
// Redis reports the scan complete. Restarting every pass at "0-0" would
// drain a large failover backlog at only `count` entries per call. On error
// the events accumulated so far are still returned so the caller can
// process and ack them.
func (c *Client) ClaimPending(ctx context.Context, group, consumer string, minIdle time.Duration, count int64) ([]Event, error) {
	var out []Event
	start := "0-0"
	for {
		msgs, cursor, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   lifecycle.EventStreamKey,
			Group:    group,
			Consumer: consumer,
			MinIdle:  minIdle,
			Start:    start,
			Count:    count,
		}).Result()
		if err != nil {
			return out, fmt.Errorf("xautoclaim: %w", err)
		}
		for _, msg := range msgs {
			// Entries the stream's MAXLEN trim has since deleted come back
			// with no values. Redis ≥ 7 drops them from the PEL on claim,
			// but on 6.2 they stay pending and resurface on every pass —
			// ack them explicitly (a no-op where Redis already removed
			// them) so the PEL can't leak.
			if len(msg.Values) == 0 {
				_ = c.Ack(ctx, group, msg.ID)
				continue
			}
			ev := decodeEvent(msg)
			if ev != nil {
				out = append(out, *ev)
			} else {
				c.log.Warn("redisstream: dropping unparseable claimed event",
					zap.String("id", msg.ID), zap.Any("values", msg.Values))
				_ = c.Ack(ctx, group, msg.ID)
			}
		}
		// Redis signals a completed scan with a "0-0" cursor ("0" seen in
		// the wild from some proxies); anything else means more batches.
		if cursor == "0-0" || cursor == "0" {
			return out, nil
		}
		start = cursor
	}
}

// SetState forces the state value (overwriting any existing). Used by
// WriteState for terminal-state re-asserts; transitions that need ownership
// go through AcquireTransition / CommitTransition / ReleaseTransition.
func (c *Client) SetState(ctx context.Context, sandboxID, state string, ttl time.Duration) error {
	key := lifecycle.StateKey(sandboxID)
	return c.rdb.Set(ctx, key, state, ttl).Err()
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

// WriteState performs SetState and, when notifications are enabled,
// publishes a best-effort wakeup hint. Redis remains the source of truth.
//
// Failures on the notify path are logged, not returned: the durable Redis
// write is the source of truth. Waiters degrade to fallback polling and
// still converge.
func (c *Client) WriteState(ctx context.Context, sandboxID, state string, ttl time.Duration) error {
	if err := c.SetState(ctx, sandboxID, state, ttl); err != nil {
		return err
	}
	c.publishNotify(sandboxID)
	return nil
}

// AcquireTransition atomically takes the state key for an in-flight
// transition, writing "<transition>@<owner>". It succeeds only when the key
// is missing or currently holds one of fromStates; any other value (a peer's
// transition lock, a terminal state that must not be overwritten) makes it
// fail. This is the CAS that serializes cross-replica pause/resume/kill:
// whoever holds the returned ownership is the only one allowed to
// CommitTransition / ReleaseTransition it (issue #1211).
//
// fromStates accepts both bare and owner-tagged stored values as written —
// pass terminal states (e.g. "paused"); in-flight transitions of peers are
// never acceptable predecessors.
func (c *Client) AcquireTransition(ctx context.Context, sandboxID, transition, owner string, ttl time.Duration, fromStates ...string) (bool, error) {
	key := lifecycle.StateKey(sandboxID)
	args := make([]any, 0, len(fromStates)+2)
	args = append(args, lifecycle.TransitionValue(transition, owner), ttl.Milliseconds())
	for _, from := range fromStates {
		args = append(args, from)
	}
	n, err := acquireTransitionScript.Run(ctx, c.rdb, []string{key}, args...).Int()
	if err != nil {
		return false, fmt.Errorf("acquire transition %s: %w", key, err)
	}
	return n == 1, nil
}

// CommitTransition finalises an owned transition by writing the terminal
// newState — but only when the key still holds this owner's lock. Returns
// false (without touching the key) when ownership was lost in the meantime
// (TTL expiry followed by a peer's acquire, external clear, …). On success a
// best-effort wakeup hint is published, mirroring WriteState.
func (c *Client) CommitTransition(ctx context.Context, sandboxID, transition, owner, newState string, ttl time.Duration) (bool, error) {
	key := lifecycle.StateKey(sandboxID)
	n, err := commitTransitionScript.Run(ctx, c.rdb, []string{key},
		lifecycle.TransitionValue(transition, owner), newState, ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("commit transition %s: %w", key, err)
	}
	if n != 1 {
		return false, nil
	}
	c.publishNotify(sandboxID)
	return true, nil
}

// ReleaseTransition abandons an owned transition by deleting the key — but
// only when the key still holds this owner's lock, so a failed operation can
// never wipe a state a peer has since committed. Returns false when the key
// was no longer ours. On success a best-effort wakeup hint is published,
// mirroring WriteState.
func (c *Client) ReleaseTransition(ctx context.Context, sandboxID, transition, owner string) (bool, error) {
	key := lifecycle.StateKey(sandboxID)
	n, err := releaseTransitionScript.Run(ctx, c.rdb, []string{key},
		lifecycle.TransitionValue(transition, owner)).Int()
	if err != nil {
		return false, fmt.Errorf("release transition %s: %w", key, err)
	}
	if n != 1 {
		return false, nil
	}
	c.publishNotify(sandboxID)
	return true, nil
}

// acquireTransitionScript: ARGV = {value, ttlMs, fromState...}. Sets the key
// to value with PX ttlMs iff the key is absent or its value is one of the
// fromStates. Returns 1 on success, 0 otherwise.
var acquireTransitionScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if cur then
	local ok = false
	for i = 3, #ARGV do
		if cur == ARGV[i] then
			ok = true
			break
		end
	end
	if not ok then
		return 0
	end
end
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2])
return 1
`)

// commitTransitionScript: ARGV = {expectedValue, newState, ttlMs}. Overwrites
// the key with newState (PX ttlMs) iff the current value equals
// expectedValue. Returns 1 on success, 0 otherwise.
var commitTransitionScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3])
	return 1
end
return 0
`)

// releaseTransitionScript: ARGV = {expectedValue}. Deletes the key iff the
// current value equals expectedValue. Returns 1 on success, 0 otherwise.
var releaseTransitionScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

// publishNotify fans a hint out locally and over Redis Pub/Sub. It never
// returns an error: any notify failure degrades to fallback polling.
func (c *Client) publishNotify(sandboxID string) {
	if !c.notifyEnabled {
		return
	}

	// Local fan-out first: same-replica waiters wake immediately, even if
	// the Redis PUBLISH below is slow or the caller context is already done.
	if c.localBus != nil {
		c.localBus.Publish(sandboxID)
	}

	payload, err := json.Marshal(lifecycle.StateNotify{SandboxID: sandboxID})
	if err != nil {
		// Should never happen — StateNotify is a plain struct.
		c.log.Warn("state notify marshal failed",
			zap.String("sandbox_id", sandboxID), zap.Error(err))
		return
	}

	// Detach from the caller context and run off the owner path: a cancelled
	// request must not drop the hint, and resume/pause/kill must not wait
	// on the extra Redis RTT. Waiters still converge via fallback polling.
	go c.publishNotifyRedis(sandboxID, payload)
}

func (c *Client) publishNotifyRedis(sandboxID string, payload []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), notifyPublishTimeout)
	defer cancel()
	if err := c.rdb.Publish(ctx, lifecycle.EventChannel, payload).Err(); err != nil {
		c.log.Warn("state notify publish failed",
			zap.String("sandbox_id", sandboxID),
			zap.String("channel", lifecycle.EventChannel),
			zap.Error(err))
	}
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
