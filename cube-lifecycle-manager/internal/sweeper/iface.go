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
//
// Ownership model (active-standby, issue #1211): pauses and kills run under
// an owner-tagged CAS lock ("pausing@<owner>" / "killing@<owner>"). The
// terminal write (CommitTransition) and the rollback (ReleaseTransition)
// only take effect while this replica still owns the lock, so a sweeper
// whose TTL expired mid-RPC can never clobber a state a concurrent resumer
// has since committed.
type stateStore interface {
	GetState(ctx context.Context, sandboxID string) (string, bool, error)
	AcquireTransition(ctx context.Context, sandboxID, transition, owner string, ttl time.Duration, fromStates ...string) (bool, error)
	CommitTransition(ctx context.Context, sandboxID, transition, owner, newState string, ttl time.Duration) (bool, error)
	ReleaseTransition(ctx context.Context, sandboxID, transition, owner string) (bool, error)
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
