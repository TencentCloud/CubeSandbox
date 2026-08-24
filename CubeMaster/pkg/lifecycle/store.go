// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// updateTimeoutWindowScript atomically reads the latest lifecycle snapshot,
// optionally replaces timeout_seconds, moves the timeout window to now, and
// publishes the matching update event. ARGV[9] is empty when resume must
// preserve the stored timeout, or contains the explicit timeout supplied by
// set_timeout, refresh, or resume.
const updateTimeoutWindowScript = `
local raw = redis.call("HGET", KEYS[1], ARGV[1])
if not raw then
    return {-1, 0}
end

local meta = cjson.decode(raw)
local timeout
if ARGV[9] == "" then
    timeout = meta["timeout_seconds"]
    if type(timeout) ~= "number" then
        return {-2, 0}
    end
else
    timeout = tonumber(ARGV[9])
    if not timeout then
        return {-3, 0}
    end
    meta["timeout_seconds"] = timeout
end

local now = tonumber(ARGV[2])
meta["created_at"] = now

local end_at = 0
if timeout >= 0 then
    end_at = now + timeout * 1000
    meta["end_at"] = end_at
else
    meta["end_at"] = nil
end

local payload = cjson.encode(meta)
redis.call("HSET", KEYS[1], ARGV[1], payload)
redis.call(
    "XADD", KEYS[2], "MAXLEN", "~", ARGV[3], "*",
    ARGV[4], ARGV[5],
    ARGV[6], ARGV[1],
    ARGV[7], ARGV[2],
    ARGV[8], payload
)

return {1, end_at}
`

// redisDoer is the minimal redigo-shaped surface the writer needs. wrapredis's
// *RedisWrap satisfies it; tests substitute a fake.
type redisDoer interface {
	Do(cmd string, args ...interface{}) (interface{}, error)
}

// Store performs the actual Redis writes. Fire-and-forget lifecycle publishing
// logs and swallows Redis errors so create/destroy can continue. Operations
// whose callers need to distinguish a completed metadata mutation, such as
// RebaseTimeoutWindow, return their Redis error.
type Store struct {
	doer    redisDoer
	enabled atomic.Bool
}

// NewStore wires a Store onto the supplied redis client.
func NewStore(doer redisDoer) *Store {
	s := &Store{doer: doer}
	s.enabled.Store(true)
	return s
}

// SetEnabled toggles all writes. When disabled the Store becomes a no-op so
// the lifecycle subsystem can be feature-flagged off without recompiling.
func (s *Store) SetEnabled(v bool) {
	if s == nil {
		return
	}
	s.enabled.Store(v)
}

// PublishCreate persists a freshly-created sandbox to the registry: HSET the
// meta snapshot, then XADD an OpCreate event.
func (s *Store) PublishCreate(ctx context.Context, meta *SandboxLifecycleMeta) {
	if s == nil || !s.enabled.Load() || s.doer == nil || meta == nil || meta.SandboxID == "" {
		return
	}

	payload, err := json.Marshal(meta)
	if err != nil {
		log.G(ctx).Warnf("lifecycle: marshal meta sandbox=%s: %v", meta.SandboxID, err)
		return
	}

	if _, err := s.doer.Do("HSET", MetaKey, meta.SandboxID, payload); err != nil {
		log.G(ctx).Warnf("lifecycle: HSET %s %s failed: %v", MetaKey, meta.SandboxID, err)
		// Continue: stream event is still useful for sidecars that already
		// have a partial view, and the next reconcile cycle will retry.
	}

	if _, err := s.xadd(OpCreate, meta.SandboxID, payload); err != nil {
		log.G(ctx).Warnf("lifecycle: XADD create %s failed: %v", meta.SandboxID, err)
	}
}

// PublishDelete drops the registry entry. Stream payload is empty; sidecars
// only need the sandbox ID to evict.
func (s *Store) PublishDelete(ctx context.Context, sandboxID string) {
	if s == nil || !s.enabled.Load() || s.doer == nil || sandboxID == "" {
		return
	}

	if _, err := s.doer.Do("HDEL", MetaKey, sandboxID); err != nil {
		log.G(ctx).Warnf("lifecycle: HDEL %s %s failed: %v", MetaKey, sandboxID, err)
	}

	if _, err := s.xadd(OpDelete, sandboxID, nil); err != nil {
		log.G(ctx).Warnf("lifecycle: XADD delete %s failed: %v", sandboxID, err)
	}
}

// PublishState emits an OpState event announcing that the sandbox has
// transitioned to a new terminal runtime state (paused or running) as a
// result of a successful pause / resume RPC.
//
// Unlike PublishCreate/Update this deliberately does NOT touch MetaKey:
// runtime state is tracked separately by the CLM via per-sandbox state
// keys, and stuffing state into the meta snapshot would blur the "meta is
// stable" invariant that consumers rely on for restart bootstrap.
//
// Only the two terminal states are broadcast — transition markers
// ("pausing", "resuming") stay private to the CLM. Invalid values are
// warned and dropped rather than propagated.
func (s *Store) PublishState(ctx context.Context, sandboxID, state, source string) {
	if s == nil || !s.enabled.Load() || s.doer == nil || sandboxID == "" {
		return
	}
	if state != StatePaused && state != StateRunning {
		log.G(ctx).Warnf("lifecycle: PublishState sandbox=%s invalid state %q; dropped",
			sandboxID, state)
		return
	}
	payload, err := json.Marshal(StatePayload{
		State:  state,
		Actor:  ActorCubeMaster,
		Source: source,
	})
	if err != nil {
		log.G(ctx).Warnf("lifecycle: marshal state payload sandbox=%s: %v", sandboxID, err)
		return
	}
	if _, err := s.xadd(OpState, sandboxID, payload); err != nil {
		log.G(ctx).Warnf("lifecycle: XADD state %s failed: %v", sandboxID, err)
	}
}

// RebaseTimeoutWindow atomically preserves the timeout currently stored for a
// sandbox, restarts its timeout window at nowMs, and emits the corresponding
// update event. It returns the new absolute endAt in Unix milliseconds; 0
// means the stored timeout is NeverTimeout.
func (s *Store) RebaseTimeoutWindow(ctx context.Context, sandboxID string, nowMs int64) (int64, error) {
	return s.updateTimeoutWindow(ctx, sandboxID, nowMs, nil)
}

// SetTimeoutWindow atomically replaces the stored timeout, starts its new
// window at nowMs, and emits an update event containing that same snapshot.
func (s *Store) SetTimeoutWindow(ctx context.Context, sandboxID string, nowMs int64, timeoutSeconds int) (int64, error) {
	return s.updateTimeoutWindow(ctx, sandboxID, nowMs, &timeoutSeconds)
}

func (s *Store) updateTimeoutWindow(_ context.Context, sandboxID string, nowMs int64, timeoutSeconds *int) (int64, error) {
	if s == nil || !s.enabled.Load() || s.doer == nil {
		return 0, nil
	}
	if sandboxID == "" {
		return 0, fmt.Errorf("sandboxID is required")
	}
	requestedTimeout := ""
	if timeoutSeconds != nil {
		requestedTimeout = strconv.Itoa(*timeoutSeconds)
	}

	values, err := redis.Int64s(s.doer.Do(
		"EVAL",
		updateTimeoutWindowScript,
		2,
		MetaKey,
		EventStreamKey,
		sandboxID,
		nowMs,
		EventStreamMaxLen,
		FieldOp,
		OpUpdate,
		FieldSandboxID,
		FieldTimestamp,
		FieldPayload,
		requestedTimeout,
	))
	if err != nil {
		return 0, fmt.Errorf("update lifecycle timeout for sandbox %s: %w", sandboxID, err)
	}
	if len(values) != 2 {
		return 0, fmt.Errorf("update lifecycle timeout for sandbox %s returned %d values", sandboxID, len(values))
	}

	switch values[0] {
	case 1:
		return values[1], nil
	case -1:
		return 0, fmt.Errorf("lifecycle metadata for sandbox %s was not found", sandboxID)
	case -2:
		return 0, fmt.Errorf("lifecycle metadata for sandbox %s has no timeout", sandboxID)
	case -3:
		return 0, fmt.Errorf("invalid timeout for sandbox %s", sandboxID)
	default:
		return 0, fmt.Errorf("update lifecycle timeout for sandbox %s returned status %d", sandboxID, values[0])
	}
}

// LoadMeta reads back the canonical meta snapshot for a sandbox. Returns nil
// (no error) when the entry isn't present so callers can treat "missing" and
// "stale" identically. Used by set_timeout / refresh to base the new meta on
// the current values (preserving auto_pause, host_id, template_id, etc.)
func (s *Store) LoadMeta(ctx context.Context, sandboxID string) (*SandboxLifecycleMeta, error) {
	if s == nil || s.doer == nil || sandboxID == "" {
		return nil, nil
	}

	v, err := s.doer.Do("HGET", MetaKey, sandboxID)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}

	var raw []byte
	switch x := v.(type) {
	case []byte:
		raw = x
	case string:
		raw = []byte(x)
	default:
		log.G(ctx).Warnf("lifecycle: HGET %s %s unexpected type %T", MetaKey, sandboxID, v)
		return nil, nil
	}
	if len(raw) == 0 {
		return nil, nil
	}

	var meta SandboxLifecycleMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// xadd builds an XADD ... MAXLEN ~ <N> * op <op> sandbox_id <id> [payload <p>]
// ts <unix_ms> command and dispatches it.
func (s *Store) xadd(op, sandboxID string, payload []byte) (interface{}, error) {
	args := make([]interface{}, 0, 12)
	args = append(args,
		EventStreamKey,
		"MAXLEN", "~", strconv.Itoa(EventStreamMaxLen),
		"*",
		FieldOp, op,
		FieldSandboxID, sandboxID,
		FieldTimestamp, time.Now().UnixMilli(),
	)
	if len(payload) > 0 {
		args = append(args, FieldPayload, payload)
	}
	return s.doer.Do("XADD", args...)
}

// defaultStore is the package-level singleton wired by Init(). Hooks call into
// it from createSandbox / destroySandbox, where threading a *Store explicitly
// would require reaching into the sandbox package's hook signatures.
var defaultStore atomic.Pointer[Store]

func setDefaultStore(s *Store) { defaultStore.Store(s) }

func getDefaultStore() *Store { return defaultStore.Load() }
