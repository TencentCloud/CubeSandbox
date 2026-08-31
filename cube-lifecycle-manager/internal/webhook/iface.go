// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
)

// Sender delivers one webhook event to one endpoint. The production
// implementation is client.Client (HTTP POST + HMAC signature + retry); tests
// substitute a recording fake. See internal/resumer/iface.go for the same DI
// convention.
type Sender interface {
	// Send must return nil on success or when the event is permanently
	// undeliverable and should be dropped after the manager's retry budget.
	Send(ctx context.Context, ep Endpoint, ev Event) error
}

// streamReader is the subset of redisstream.Client the Manager needs to
// consume the lifecycle stream through its own consumer group. Tests inject an
// in-memory fake so no Redis is required. See internal/statesync/iface.go for
// the same convention.
type streamReader interface {
	EnsureGroup(ctx context.Context, group string) error
	ReadGroup(ctx context.Context, group, consumer string, block time.Duration, count int) ([]redisstream.Event, error)
	ReadPending(ctx context.Context, group, consumer string) ([]redisstream.Event, error)
	ReclaimStale(ctx context.Context, group, consumer string, minIdle time.Duration) ([]redisstream.Event, error)
	Ack(ctx context.Context, group, id string) error
}
