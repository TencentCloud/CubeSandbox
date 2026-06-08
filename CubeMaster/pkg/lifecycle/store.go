// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package lifecycle

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// redisDoer is the minimal redigo-shaped surface the writer needs. wrapredis's
// *RedisWrap satisfies it; tests substitute a fake.
type redisDoer interface {
	Do(cmd string, args ...interface{}) (interface{}, error)
}

// Store performs the actual Redis writes. It is intentionally tiny and never
// returns errors to its callers — every error is logged at warn level and
// swallowed so a Redis hiccup cannot fail a sandbox create/destroy.
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
