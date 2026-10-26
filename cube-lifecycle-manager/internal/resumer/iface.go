// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package resumer

import (
	"context"
	"time"
)

// stateStore is the subset of redisstream.Client we use. Tests substitute an
// in-memory fake so we don't depend on a live Redis.
type stateStore interface {
	AcquireState(ctx context.Context, sandboxID, state string, ttl time.Duration) (bool, error)
	AcquireResume(ctx context.Context, sandboxID string, ttl time.Duration) (state string, acquired bool, err error)
	SetState(ctx context.Context, sandboxID, state string, ttl time.Duration) error
	ClearState(ctx context.Context, sandboxID string) error
	GetState(ctx context.Context, sandboxID string) (string, bool, error)
	// WriteState / ClearStateNotify are the notify-emitting equivalents of
	// SetState / ClearState. The concrete client can disable notifications.
	WriteState(ctx context.Context, sandboxID, state string, ttl time.Duration) error
	ClearStateNotify(ctx context.Context, sandboxID string) error
}

// resumePauser describes the slice of CubeMaster client we need.
type resumePauser interface {
	Resume(ctx context.Context, sandboxID, instanceType string) error
}

// stateNotifier is the slice of proxypush.Client we use. DeleteMeta is
// invoked when CubeMaster reports the sandbox no longer exists so we evict
// the local proxy entry alongside the shared registry one.
type stateNotifier interface {
	SetState(ctx context.Context, sandboxID, state string) error
	DeleteMeta(ctx context.Context, sandboxID string) error
}

// waitBus is the eventbus subset we consume: register a listener that
// receives the next StateNotify for a sandbox, plus the cancel func the
// listener MUST call before returning. Concrete impl is *eventbus.Bus.
type waitBus interface {
	Wait(sandboxID string) (<-chan struct{}, func())
}
