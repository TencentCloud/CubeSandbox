// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sweeper

import (
	"context"
	"time"
)

// stateStore is the subset of redisstream.Client that the sweeper needs.
// Defining it as an interface here lets tests substitute an in-memory fake
// without spinning up a real Redis. The concrete *redisstream.Client
// satisfies this interface implicitly.
type stateStore interface {
	AcquireState(ctx context.Context, sandboxID, state string, ttl time.Duration) (bool, error)
	SetState(ctx context.Context, sandboxID, state string, ttl time.Duration) error
	ClearState(ctx context.Context, sandboxID string) error
	GetState(ctx context.Context, sandboxID string) (string, bool, error)
}

// pauser is the subset of cubemasterclient.Client that the sweeper needs.
type pauser interface {
	Pause(ctx context.Context, sandboxID, instanceType string) error
}

// stateNotifier is the subset of proxypush.Client that the sweeper needs.
// SetState pushes a transition (running/pausing/paused). DeleteMeta is
// invoked when CubeMaster reports a sandbox no longer exists, so we evict
// the corresponding entry from CubeProxy's local meta dict in the same
// step.
type stateNotifier interface {
	SetState(ctx context.Context, sandboxID, state string) error
	DeleteMeta(ctx context.Context, sandboxID string) error
}
