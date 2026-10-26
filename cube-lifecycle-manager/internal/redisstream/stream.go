// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package redisstream owns every interaction with the lifecycle Redis schema:
// the meta HSet bootstrap, the events stream consumer, and the per-sandbox
// state locks used to serialize pause/resume across CLM replicas.
package redisstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

// notifyPublishTimeout bounds the best-effort Redis PUBLISH. It is detached
// from the caller context so a cancelled request cannot drop the hint.
const notifyPublishTimeout = 500 * time.Millisecond

// ErrCursorTrimmed means XREAD could no longer prove that all entries after
// the caller's cursor were retained.
var ErrCursorTrimmed = errors.New("Redis stream cursor was trimmed")

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

// SetNotifyEnabled toggles the Pub/Sub publish that accompanies WriteState /
// ClearStateNotify. When disabled, those methods behave exactly like the
// legacy SetState / ClearState. Safe to call at startup only.
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

// LatestID returns the newest lifecycle stream ID. Callers capture it before
// HGETALL bootstrap, then XREAD from that cursor so events written during the
// bootstrap window are not missed. An empty stream starts at 0-0.
func (c *Client) LatestID(ctx context.Context) (string, error) {
	messages, err := c.rdb.XRevRangeN(ctx, lifecycle.EventStreamKey, "+", "-", 1).Result()
	if err != nil {
		return "", fmt.Errorf("xrevrange latest: %w", err)
	}
	if len(messages) == 0 {
		return "0-0", nil
	}
	return messages[0].ID, nil
}

// CursorValid reports whether cursor is still present in, or newer than, the
// retained stream window. Redis XREAD silently skips trimmed entries, so CLM
// must detect this condition and rebuild from the authoritative metadata Hash
// before the replica is allowed to perform leader work.
func (c *Client) CursorValid(ctx context.Context, cursor string) (bool, error) {
	if cursor == "" {
		return true, nil
	}
	if cursor == "0-0" {
		exists, err := c.rdb.Exists(ctx, lifecycle.EventStreamKey).Result()
		if err != nil {
			return false, fmt.Errorf("exists %s: %w", lifecycle.EventStreamKey, err)
		}
		if exists == 0 {
			return true, nil
		}
		info, err := c.rdb.XInfoStream(ctx, lifecycle.EventStreamKey).Result()
		if err != nil {
			return false, fmt.Errorf("xinfo stream %s: %w", lifecycle.EventStreamKey, err)
		}
		return info.EntriesAdded <= info.Length, nil
	}
	oldest, err := c.rdb.XRangeN(ctx, lifecycle.EventStreamKey, "-", "+", 1).Result()
	if err != nil {
		return false, fmt.Errorf("xrange oldest %s: %w", lifecycle.EventStreamKey, err)
	}
	if len(oldest) == 0 {
		return false, nil
	}
	cmp, err := CompareStreamIDs(cursor, oldest[0].ID)
	if err != nil {
		return false, err
	}
	return cmp >= 0, nil
}

// CompareStreamIDs compares two Redis stream IDs.
func CompareStreamIDs(left, right string) (int, error) {
	parse := func(id string) (uint64, uint64, error) {
		msText, seqText, ok := strings.Cut(id, "-")
		if !ok {
			return 0, 0, fmt.Errorf("invalid Redis stream ID %q", id)
		}
		ms, err := strconv.ParseUint(msText, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid Redis stream ID %q: %w", id, err)
		}
		seq, err := strconv.ParseUint(seqText, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid Redis stream ID %q: %w", id, err)
		}
		return ms, seq, nil
	}
	leftMS, leftSeq, err := parse(left)
	if err != nil {
		return 0, err
	}
	rightMS, rightSeq, err := parse(right)
	if err != nil {
		return 0, err
	}
	if leftMS < rightMS || (leftMS == rightMS && leftSeq < rightSeq) {
		return -1, nil
	}
	if leftMS == rightMS && leftSeq == rightSeq {
		return 0, nil
	}
	return 1, nil
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

// Read broadcasts lifecycle events to one CLM replica using a caller-owned
// cursor. Unlike XREADGROUP, every replica receives every event and can keep
// its in-memory registry warm. The returned cursor advances over malformed
// entries too, preventing one bad message from wedging the loop.
func (c *Client) Read(ctx context.Context, cursor string, block time.Duration, count int) ([]Event, string, error) {
	res, err := c.rdb.XRead(ctx, &redis.XReadArgs{
		Streams: []string{lifecycle.EventStreamKey, cursor},
		Count:   int64(count),
		Block:   block,
	}).Result()
	valid, checkErr := c.CursorValid(ctx, cursor)
	if checkErr != nil {
		return nil, cursor, checkErr
	}
	if !valid {
		return nil, cursor, ErrCursorTrimmed
	}
	if errors.Is(err, redis.Nil) || errors.Is(err, context.DeadlineExceeded) {
		return nil, cursor, nil
	}
	if err != nil {
		return nil, cursor, fmt.Errorf("xread: %w", err)
	}

	next := cursor
	var out []Event
	for _, stream := range res {
		for _, msg := range stream.Messages {
			next = msg.ID
			ev := decodeEvent(msg)
			if ev == nil {
				c.log.Warn("redisstream: dropping unparseable event",
					zap.String("id", msg.ID), zap.Any("values", msg.Values))
				continue
			}
			out = append(out, *ev)
		}
	}
	return out, next, nil
}

// AcquireState performs a SET NX EX on the per-sandbox lifecycle state key with
// the supplied desired state. Returns true on success. Used to coordinate
// concurrent pause/resume across CLM replicas: whoever wins the SETNX owns the
// transition.
func (c *Client) AcquireState(ctx context.Context, sandboxID, state string, ttl time.Duration) (bool, error) {
	key := lifecycle.StateKey(sandboxID)
	ok, err := c.rdb.SetNX(ctx, key, state, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("setnx %s: %w", key, err)
	}
	return ok, nil
}

// AcquireResume atomically changes an absent or paused state to resuming using
// a single-key WATCH transaction. acquired=true means this caller owns the
// transition. state is always the value observed before the write: paused or
// empty when acquired, otherwise the blocking marker a waiter should reconcile
// against. Transactions avoid Lua/EVAL and remain single-slot safe.
func (c *Client) AcquireResume(ctx context.Context, sandboxID string, ttl time.Duration) (state string, acquired bool, err error) {
	return c.acquireIf(ctx, sandboxID, "resuming", ttl, allowEmptyOrPaused)
}

// AcquireKill atomically changes an absent or paused state to killing. It is
// the timeout-kill counterpart of AcquireResume: the two CAS transitions
// are mutually exclusive, so a concurrent resume wins or this kill does,
// but neither overwrites an in-flight resuming / pausing / running lock.
// state is the pre-CAS observation (paused or empty when acquired) so a
// failed kill can restore the proxy to the right prior state.
func (c *Client) AcquireKill(ctx context.Context, sandboxID string, ttl time.Duration) (state string, acquired bool, err error) {
	return c.acquireIf(ctx, sandboxID, "killing", ttl, allowEmptyOrPaused)
}

func allowEmptyOrPaused(current string) bool {
	return current == "" || current == lifecycle.StatePaused
}

// acquireIf CAS-writes dest when allow(current) still holds under WATCH.
// The returned state is always the pre-write observation, including when acquired.
func (c *Client) acquireIf(
	ctx context.Context,
	sandboxID, dest string,
	ttl time.Duration,
	allow func(current string) bool,
) (string, bool, error) {
	const maxAttempts = 3
	key := lifecycle.StateKey(sandboxID)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var state string
		acquired := false
		err := c.rdb.Watch(ctx, func(tx *redis.Tx) error {
			current, getErr := tx.Get(ctx, key).Result()
			if errors.Is(getErr, redis.Nil) {
				current = ""
			} else if getErr != nil {
				return getErr
			}

			state = current
			if !allow(current) {
				return nil
			}

			_, txErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, dest, ttl)
				return nil
			})
			if txErr == nil {
				acquired = true
			}
			return txErr
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("acquire %s %s: %w", dest, key, err)
		}
		return state, acquired, nil
	}
	return "", false, fmt.Errorf("acquire %s %s: transaction conflicted after %d attempts", dest, key, maxAttempts)
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

// getStatesChunk bounds MGET so a large registry does not send one giant
// command. 128 keys stay well under typical Redis argument limits.
const getStatesChunk = 128

// GetStates returns the current state string for each sandbox that has a
// key. Missing keys are omitted from the map (not an error).
func (c *Client) GetStates(ctx context.Context, sandboxIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(sandboxIDs))
	if len(sandboxIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(sandboxIDs); start += getStatesChunk {
		chunk := sandboxIDs[start:min(start+getStatesChunk, len(sandboxIDs))]
		keys := make([]string, len(chunk))
		for i, id := range chunk {
			keys[i] = lifecycle.StateKey(id)
		}
		vals, err := c.rdb.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, fmt.Errorf("mget lifecycle states: %w", err)
		}
		for i, v := range vals {
			if v == nil {
				continue
			}
			s, ok := v.(string)
			if !ok || s == "" {
				continue
			}
			out[chunk[i]] = s
		}
	}
	return out, nil
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

// WriteStateCAS writes a terminal state only if the current value still
// matches expected. This prevents a state event from overwriting a transition
// marker installed after the event handler's initial read.
func (c *Client) WriteStateCAS(
	ctx context.Context, sandboxID, expected, state string, ttl time.Duration,
) (bool, error) {
	const maxAttempts = 3
	key := lifecycle.StateKey(sandboxID)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		updated := false
		err := c.rdb.Watch(ctx, func(tx *redis.Tx) error {
			current, getErr := tx.Get(ctx, key).Result()
			if errors.Is(getErr, redis.Nil) {
				current = ""
			} else if getErr != nil {
				return getErr
			}
			if current != expected {
				return nil
			}
			_, txErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, state, ttl)
				return nil
			})
			if txErr == nil {
				updated = true
			}
			return txErr
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("write state cas %s: %w", key, err)
		}
		if updated {
			c.publishNotify(sandboxID)
		}
		return updated, nil
	}
	return false, fmt.Errorf("write state cas %s: transaction conflicted after %d attempts", key, maxAttempts)
}

// ClearStateNotify is the ClearState + Pub/Sub companion used on rollback.
func (c *Client) ClearStateNotify(ctx context.Context, sandboxID string) error {
	if err := c.ClearState(ctx, sandboxID); err != nil {
		return err
	}
	c.publishNotify(sandboxID)
	return nil
}

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
