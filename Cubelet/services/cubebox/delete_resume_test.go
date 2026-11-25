// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteLifecycleLockDeadline(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	t.Run("no deadline uses the short lock budget", func(t *testing.T) {
		deadline, ok := deleteLifecycleLockDeadline(context.Background(), now)
		require.True(t, ok)
		assert.Equal(t, now.Add(deleteLifecycleLockMaxWait), deadline)
	})

	t.Run("normal delete deadline caps lock waiting", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(30*time.Second))
		defer cancel()

		deadline, ok := deleteLifecycleLockDeadline(ctx, now)
		require.True(t, ok)
		assert.Equal(t, now.Add(deleteLifecycleLockMaxWait), deadline)
	})

	t.Run("expired deadline skips lock waiting", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(-time.Millisecond))
		defer cancel()

		_, ok := deleteLifecycleLockDeadline(ctx, now)
		assert.False(t, ok)
	})
}
