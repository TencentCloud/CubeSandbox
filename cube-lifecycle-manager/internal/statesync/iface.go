// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package statesync

import (
	"context"
	"time"
)

// stateStore is the subset of redisstream.Client we use. Tests substitute an
// in-memory fake so we don't depend on a live Redis.
type stateStore interface {
	GetState(ctx context.Context, sandboxID string) (string, bool, error)
	SetState(ctx context.Context, sandboxID, state string, ttl time.Duration) error
	// WriteState is the notify-emitting equivalent of SetState. See
	// internal/resumer/iface.go for the contract.
	WriteState(ctx context.Context, sandboxID, state string, ttl time.Duration) error
	WriteStateCAS(ctx context.Context, sandboxID, expected, state, eventID string, ttl time.Duration) (bool, error)
}

// stateNotifier is the subset of proxypush.Client we use.
type stateNotifier interface {
	SetState(ctx context.Context, sandboxID, state string) error
}
