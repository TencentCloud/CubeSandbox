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
	SetState(ctx context.Context, sandboxID, state string, ttl time.Duration) error
	ClearState(ctx context.Context, sandboxID string) error
	GetState(ctx context.Context, sandboxID string) (string, bool, error)
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
