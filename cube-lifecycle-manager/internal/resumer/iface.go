// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package resumer

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

// stateStore is the subset of redisstream.Client we use. Tests substitute an
// in-memory fake so we don't depend on a live Redis.
type stateStore interface {
	AcquireState(ctx context.Context, sandboxID, state string, ttl time.Duration) (bool, error)
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

// metaLookup is an optional fallback consulted when the in-memory registry
// has no entry for a sandbox. In active-standby mode (issue #1211) a standby
// replica — or a freshly promoted leader whose bootstrap hasn't landed yet —
// has an empty/partial registry; looking the meta up directly from the
// shared Redis hash lets those replicas still serve resume requests during
// the failover window instead of failing with "sandbox not in registry".
// Returns (nil, nil) when the sandbox is unknown to CubeMaster.
type metaLookup interface {
	LookupMeta(ctx context.Context, sandboxID string) (*lifecycle.SandboxLifecycleMeta, error)
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
