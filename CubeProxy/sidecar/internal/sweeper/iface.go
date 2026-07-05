// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sweeper

import (
	"context"
	"time"
)

// stateStore is the data store interface the sweeper needs for lifecycle management.
// Defining it as an interface here lets tests substitute an in-memory fake
// without spinning up an external database. The concrete *redisstream.Client
// satisfies this interface implicitly.
type stateStore interface {
	// TryTransitionState atomically changes sandboxID's lifecycle state to newState
	// when the current underlying state is one of allowedCurrentStates.
	// It returns whether the swap happened and the state observed before the
	// decision. An empty observedState means the state was missing.
	TryTransitionState(ctx context.Context, sandboxID, newState string, newStateTTL time.Duration, allowedCurrentStates ...string) (swapped bool, observedState string, err error)
	SetState(ctx context.Context, sandboxID, state string, ttl time.Duration) error
	ClearState(ctx context.Context, sandboxID string) error
	GetState(ctx context.Context, sandboxID string) (string, bool, error)
}

// pauseKiller is the subset of cubemasterclient.Client that the sweeper needs.
// Pause + Kill are the two terminal transitions the sweeper can trigger.
type pauseKiller interface {
	Pause(ctx context.Context, sandboxID, instanceType string) error
	Kill(ctx context.Context, sandboxID, instanceType, reason string) error
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
