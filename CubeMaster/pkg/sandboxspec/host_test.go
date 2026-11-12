// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package sandboxspec

import (
	"context"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"testing"
)

func TestHostRouteHonorsSpecDeletion(t *testing.T) {
	client, err := gorm.Open(sqlite.Open("file:spec-host?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, client.AutoMigrate(&models.SandboxSpec{}))
	dbMu.Lock()
	old := db
	db = client
	dbMu.Unlock()
	t.Cleanup(func() { dbMu.Lock(); db = old; dbMu.Unlock(); raw, _ := client.DB(); raw.Close() })
	ctx := context.Background()
	require.NoError(t, client.Create(&models.SandboxSpec{SandboxID: "one", HostIP: "10.0.0.1"}).Error)
	host, err := GetHostIP(ctx, "one")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.1", host)
	require.NoError(t, Delete(ctx, "one"))
	_, err = GetHostIP(ctx, "one")
	require.ErrorIs(t, err, ErrSandboxSpecNotFound)
	require.NoError(t, client.Create(&models.SandboxSpec{SandboxID: "empty"}).Error)
	_, err = GetHostIP(ctx, "empty")
	require.ErrorContains(t, err, "no destroy route")
}
