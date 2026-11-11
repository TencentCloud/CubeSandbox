// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxspec

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// resetCreatedAtCache isolates the package-level memo between tests.
func resetCreatedAtCache(t *testing.T) {
	t.Helper()
	createdAtCache = newCreatedAtMemo(createdAtCacheSize)
}

func TestGetCreatedAtReturnsDurableValue(t *testing.T) {
	database := setupSpecStore(t)
	resetCreatedAtCache(t)
	putSpec(t, "sb-cold", "a")

	var rec models.SandboxSpec
	require.NoError(t, database.Where("sandbox_id = ?", "sb-cold").First(&rec).Error)

	got, err := GetCreatedAt(context.Background(), "sb-cold")
	require.NoError(t, err)
	require.Equal(t, rec.CreatedAt, got)

	// With the row gone, a second read can only be served from the memo.
	require.NoError(t, database.Exec(
		"DELETE FROM t_cube_sandbox_spec WHERE sandbox_id = ?", "sb-cold").Error)
	again, err := GetCreatedAt(context.Background(), "sb-cold")
	require.NoError(t, err)
	require.Equal(t, rec.CreatedAt, again)
}

func TestGetCreatedAtSurvivesSpecUpsert(t *testing.T) {
	setupSpecStore(t)
	resetCreatedAtCache(t)

	putSpec(t, "sb-upsert", "a")
	first, err := GetCreatedAt(context.Background(), "sb-upsert")
	require.NoError(t, err)
	require.False(t, first.IsZero())

	// Drop the memo so the second read comes from the row itself, and leave
	// enough of a gap that an overwritten created_at would be distinguishable.
	resetCreatedAtCache(t)
	time.Sleep(10 * time.Millisecond)

	putSpec(t, "sb-upsert", "b")
	second, err := GetCreatedAt(context.Background(), "sb-upsert")
	require.NoError(t, err)
	require.Equal(t, first, second, "an upsert must preserve the original created_at")
}

// A deleted spec must not be answered from the memo.
func TestGetCreatedAtAfterDelete(t *testing.T) {
	setupSpecStore(t)
	resetCreatedAtCache(t)
	putSpec(t, "sb-delete", "a")

	_, err := GetCreatedAt(context.Background(), "sb-delete")
	require.NoError(t, err)

	require.NoError(t, Delete(context.Background(), "sb-delete"))
	_, err = GetCreatedAt(context.Background(), "sb-delete")
	require.ErrorIs(t, err, ErrSandboxSpecNotFound)
}

func TestGetCreatedAtManyMatchesGetCreatedAt(t *testing.T) {
	setupSpecStore(t)
	resetCreatedAtCache(t)
	putSpec(t, "sb-many-1", "a")
	putSpec(t, "sb-many-2", "b")

	got, err := GetCreatedAtMany(context.Background(), []string{"sb-many-1", "sb-missing", "sb-many-2"})
	require.NoError(t, err)
	require.Len(t, got, 2)

	one, err := GetCreatedAt(context.Background(), "sb-many-1")
	require.NoError(t, err)
	require.Equal(t, one, got["sb-many-1"])
}
