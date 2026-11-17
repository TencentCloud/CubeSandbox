// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package templatecenter

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"sync"
	"testing"
	"time"
)

func TestSandboxSpecConcurrentDeleteMySQL(t *testing.T) {
	env := newMySQLDockerEnv(t)
	defer env.teardown()
	db := openMigratedMySQLGORM(t, env)
	restoreSpecDB := sandboxspec.SetDBForTest(db)
	defer restoreSpecDB()
	raw, err := db.DB()
	require.NoError(t, err)
	raw.SetMaxOpenConns(100)
	raw.SetMaxIdleConns(100)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	records := make([]models.SandboxSpec, 1000)
	for i := range records {
		records[i] = models.SandboxSpec{SandboxID: fmt.Sprintf("delete-spec-%04d", i), HostIP: "10.0.0.1"}
	}
	require.NoError(t, db.CreateInBatches(records, 100).Error)
	var plan []struct {
		Key  string `gorm:"column:key"`
		Type string `gorm:"column:type"`
	}
	require.NoError(t, db.Raw("EXPLAIN UPDATE t_cube_sandbox_spec FORCE INDEX (idx_sandbox_spec_sandbox_id) SET deleted_at=CURRENT_TIMESTAMP WHERE sandbox_id=? AND deleted_at IS NULL", records[0].SandboxID).Scan(&plan).Error)
	require.Len(t, plan, 1)
	require.Equal(t, "idx_sandbox_spec_sandbox_id", plan[0].Key)
	t.Logf("point delete plan: %+v", plan)
	for round := 0; round < 10; round++ {
		start := make(chan struct{})
		errs := make(chan error, 100)
		var wg sync.WaitGroup
		for i := round * 100; i < (round+1)*100; i++ {
			wg.Add(1)
			go func(id string) { defer wg.Done(); <-start; errs <- sandboxspec.Delete(ctx, id) }(records[i].SandboxID)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
	}
	var count int64
	require.NoError(t, db.Model(&models.SandboxSpec{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Unscoped().Model(&models.SandboxSpec{}).Count(&count).Error)
	require.EqualValues(t, 1000, count)
	require.NoError(t, sandboxspec.Delete(ctx, records[0].SandboxID))
}
