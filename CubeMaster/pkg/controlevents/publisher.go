// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package controlevents

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// redisDoer is the minimal redigo-shaped surface the publisher needs.
type redisDoer interface {
	Do(cmd string, args ...interface{}) (interface{}, error)
}

// Publisher performs Redis XADD writes for control-plane events. Runtime
// failures are logged and swallowed so a Redis hiccup cannot roll back a DB
// write that already succeeded.
type Publisher struct {
	doer    redisDoer
	origin  string
	enabled atomic.Bool
}

// NewPublisher wires a Publisher onto the supplied redis client.
func NewPublisher(doer redisDoer) *Publisher {
	origin, _ := os.Hostname()
	p := &Publisher{doer: doer, origin: origin}
	p.enabled.Store(true)
	return p
}

// SetEnabled toggles all writes.
func (p *Publisher) SetEnabled(v bool) {
	if p == nil {
		return
	}
	p.enabled.Store(v)
}

// PublishNodeIsolation emits node.isolate or node.unisolate after a successful
// DB cordon write. Best-effort: errors are warned, never returned.
func (p *Publisher) PublishNodeIsolation(ctx context.Context, nodeID string, disabled bool) {
	if p == nil || !p.enabled.Load() || p.doer == nil || nodeID == "" {
		return
	}
	op := OpNodeUnisolate
	if disabled {
		op = OpNodeIsolate
	}
	payload, err := json.Marshal(IsolationPayload{
		SchedulingDisabled: disabled,
		UpdatedAtUnixMs:    time.Now().UnixMilli(),
		Origin:             p.origin,
	})
	if err != nil {
		log.G(ctx).Warnf("controlevents: marshal isolation payload node=%s: %v", nodeID, err)
		return
	}
	if _, err := p.xadd(op, nodeID, payload); err != nil {
		log.G(ctx).Warnf("controlevents: XADD %s node=%s failed: %v", op, nodeID, err)
	}
}

func (p *Publisher) xadd(op, nodeID string, payload []byte) (interface{}, error) {
	args := make([]interface{}, 0, 12)
	args = append(args,
		EventStreamKey,
		"MAXLEN", "~", strconv.Itoa(EventStreamMaxLen),
		"*",
		FieldOp, op,
		FieldNodeID, nodeID,
		FieldTimestamp, time.Now().UnixMilli(),
	)
	if len(payload) > 0 {
		args = append(args, FieldPayload, payload)
	}
	return p.doer.Do("XADD", args...)
}

var defaultPublisher atomic.Pointer[Publisher]

func setDefaultPublisher(p *Publisher) { defaultPublisher.Store(p) }

func getDefaultPublisher() *Publisher { return defaultPublisher.Load() }

// PublishNodeIsolationDefault is the package-level entry used by nodemeta after
// a successful isolation write. Safe when Init has not run (no-op).
func PublishNodeIsolationDefault(ctx context.Context, nodeID string, disabled bool) {
	if p := getDefaultPublisher(); p != nil {
		p.PublishNodeIsolation(ctx, nodeID, disabled)
	}
}
