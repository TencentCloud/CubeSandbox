// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
	nmstore "github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/migrate"
	"gorm.io/gorm"
)

func TestNodeInventoryRollout(t *testing.T) {
	const inventoryMigrationVersion int64 = 20261125031758
	withWarehouseStores(t, func(t *testing.T, backend *store.Store) {
		ctx := context.Background()
		db := backend.DB()
		compat, split := nmstore.NewNodeStore(db, false), nmstore.NewNodeStore(db, true)
		report := nmstore.NodeStatus{NodeID: "node", HeartbeatUnix: 234, HeartbeatReceivedUnix: 101,
			Healthy: true, ConditionsJSON: "[]", ImagesJSON: "[]", LocalTemplatesJSON: `[{"id":"a"}]`}

		readInline := func() nmstore.NodeStatus {
			t.Helper()
			var row nmstore.NodeStatus
			require.NoError(t, db.Where("node_id = ?", report.NodeID).Take(&row).Error)
			return row
		}
		checkReaders := func() {
			t.Helper()
			for _, reader := range []nmstore.NodeStore{compat, split} {
				got, err := reader.GetStatus(ctx, report.NodeID)
				require.NoError(t, err)
				require.Equal(t, report.HeartbeatReceivedUnix, got.HeartbeatReceivedUnix)
				require.Equal(t, report.HeartbeatUnix, got.HeartbeatUnix)
				require.Equal(t, report.Healthy, got.Healthy)
				require.Equal(t, report.ImagesJSON, got.ImagesJSON)
				require.Equal(t, report.LocalTemplatesJSON, got.LocalTemplatesJSON)
				rows, err := reader.ListStatuses(ctx)
				require.NoError(t, err)
				require.Equal(t, []nmstore.NodeStatus{*got}, rows)
			}
		}

		require.NoError(t, compat.UpsertRegistration(ctx, &nmstore.NodeRegistration{NodeID: report.NodeID}))
		// Existing row before any new writer has reported.
		require.NoError(t, db.Create(&report).Error)
		checkReaders()
		report.HeartbeatReceivedUnix++
		queries := 0
		require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:compat_queries", func(tx *gorm.DB) {
			queries++
		}))
		input := report
		require.NoError(t, compat.UpsertStatus(ctx, &report))
		require.Zero(t, queries)
		require.Equal(t, input, report)
		require.NoError(t, db.Callback().Query().Remove("test:compat_queries"))
		require.Equal(t, report.LocalTemplatesJSON, readInline().LocalTemplatesJSON)
		var inventoryCount int64
		require.NoError(t, db.Table("t_cube_node_inventory").Count(&inventoryCount).Error)
		require.Zero(t, inventoryCount)
		// A legacy writer only updates the status table, even in the same second.
		report.LocalTemplatesJSON = `[{"id":"b"}]`
		require.NoError(t, db.Model(&report).Update("local_templates_json", report.LocalTemplatesJSON).Error)
		checkReaders()
		require.NoError(t, split.UpsertStatus(ctx, &report))
		require.Empty(t, readInline().LocalTemplatesJSON)
		var splitCount int64
		require.NoError(t, db.Model(&nmstore.NodeStatus{}).
			Where("node_id = ? AND images_json IS NULL AND local_templates_json IS NULL", report.NodeID).
			Count(&splitCount).Error)
		require.Equal(t, int64(1), splitCount)
		checkReaders()

		// A heartbeat with clock skew still advances the independent receipt time.
		// Count inventory writes to distinguish skipping the write from rewriting
		// the same bytes. Both inserts and conflict updates use Create.
		writes := 0
		require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:count_inventory", func(tx *gorm.DB) {
			if tx.Statement.Table == "t_cube_node_inventory" {
				writes++
			}
		}))
		for range 10 {
			report.HeartbeatUnix--
			report.HeartbeatReceivedUnix++
			report.Healthy = !report.Healthy
			require.NoError(t, split.UpsertStatus(ctx, &report))
		}
		require.Zero(t, writes)
		checkReaders()
		report.ImagesJSON = `[{"names":["new"]}]`
		require.NoError(t, split.UpsertStatus(ctx, &report))
		require.Equal(t, 1, writes)
		checkReaders()

		// Failing an inventory update must roll back the heartbeat as well.
		require.NoError(t, db.Exec("ALTER TABLE t_cube_node_inventory ADD CONSTRAINT inventory_test_guard CHECK (images_json <> 'reject')").Error)
		rejected := report
		rejected.ImagesJSON = "reject"
		rejected.HeartbeatReceivedUnix++
		require.Error(t, split.UpsertStatus(ctx, &rejected))
		checkReaders()

		// Switching the flag back also works on a node that still has split data.
		report.ImagesJSON = `[{"names":["compat"]}]`
		require.NoError(t, compat.UpsertStatus(ctx, &report))
		require.Equal(t, report.ImagesJSON, readInline().ImagesJSON)
		checkReaders()
		require.NoError(t, split.UpsertStatus(ctx, &report))
		checkReaders()
		require.NoError(t, split.DeleteNode(ctx, report.NodeID))
		var count int64
		require.NoError(t, db.Table("t_cube_node_inventory").Count(&count).Error)
		require.Zero(t, count)

		// A rollback must restore an offline node without waiting for a report.
		report.ID = 0
		require.NoError(t, split.UpsertStatus(ctx, &report))
		sqlDB, err := db.DB()
		require.NoError(t, err)
		// Keep all migrations older than this feature, including any that
		// landed on the base branch while this change was under review.
		require.NoError(t, migrate.DownTo(ctx, sqlDB, db.Dialector.Name(), nil, inventoryMigrationVersion-1))
		inline := readInline()
		require.Equal(t, report.ImagesJSON, inline.ImagesJSON)
		require.Equal(t, report.LocalTemplatesJSON, inline.LocalTemplatesJSON)
		require.Equal(t, report.HeartbeatReceivedUnix, inline.HeartbeatReceivedUnix)
		require.NoError(t, migrate.Run(ctx, sqlDB, db.Dialector.Name(), nil))
		checkReaders()
		// Simulate a crash after DROP commits, before goose records the
		// downgrade. Both dialects must tolerate the already-absent table.
		require.NoError(t, db.Exec("DROP TABLE t_cube_node_inventory").Error)
		require.NoError(t, migrate.DownTo(ctx, sqlDB, db.Dialector.Name(), nil, inventoryMigrationVersion-1))
		require.Equal(t, report.ImagesJSON, readInline().ImagesJSON)
		require.Equal(t, report.LocalTemplatesJSON, readInline().LocalTemplatesJSON)
	})
}

func TestNodeInventoryOrderDoesNotWrite(t *testing.T) {
	withWarehouseStores(t, func(t *testing.T, backend *store.Store) {
		ctx := context.Background()
		db := backend.DB()
		nodes := nmstore.NewNodeStore(db, true)
		require.NoError(t, nodes.UpsertRegistration(ctx, &nmstore.NodeRegistration{NodeID: "ordered-node"}))
		svc := service.NewNodeService(nodes, service.DeclaredVersionInfo{})
		writes := 0
		require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:inventory_order", func(tx *gorm.DB) {
			if tx.Statement.Table == "t_cube_node_inventory" {
				writes++
			}
		}))
		request := &model.UpdateNodeStatusRequest{
			HeartbeatTime: time.Unix(100, 0),
			Images: []model.ContainerImage{
				{Namespace: "b", Names: []string{"z", "a"}},
				{Namespace: "a", Names: []string{"c"}},
			},
			LocalTemplates: []model.LocalTemplate{{TemplateID: "b"}, {TemplateID: "a"}},
		}
		_, err := svc.UpdateNodeStatus(ctx, "ordered-node", request)
		require.NoError(t, err)
		require.Equal(t, 1, writes)

		request.Images = []model.ContainerImage{
			{Namespace: "a", Names: []string{"c"}},
			{Namespace: "b", Names: []string{"a", "z"}},
		}
		request.LocalTemplates = []model.LocalTemplate{{TemplateID: "a"}, {TemplateID: "b"}}
		request.HeartbeatTime = time.Unix(101, 0)
		writes = 0
		_, err = svc.UpdateNodeStatus(ctx, "ordered-node", request)
		require.NoError(t, err)
		require.Zero(t, writes)
		got, err := nodes.GetStatus(ctx, "ordered-node")
		require.NoError(t, err)
		require.Equal(t, int64(101), got.HeartbeatUnix)
		require.Equal(t, model.MustJSON(model.CanonicalImages(request.Images)), got.ImagesJSON)
		require.Equal(t, model.MustJSON(model.CanonicalLocalTemplates(request.LocalTemplates)), got.LocalTemplatesJSON)

		request.Images[0].Names = []string{"changed"}
		_, err = svc.UpdateNodeStatus(ctx, "ordered-node", request)
		require.NoError(t, err)
		require.Equal(t, 1, writes)
	})
}

func TestConcurrentNodeInventoryReports(t *testing.T) {
	withWarehouseStores(t, func(t *testing.T, backend *store.Store) {
		ctx := context.Background()
		const n = 8
		start := make(chan struct{})
		errs := make(chan error, n)
		for i := range n {
			go func() {
				<-start
				s := nmstore.NewNodeStore(backend.DB(), i%2 == 0)
				errs <- s.UpsertStatus(ctx, &nmstore.NodeStatus{NodeID: "node", HeartbeatReceivedUnix: int64(i + 1),
					ImagesJSON: fmt.Sprintf("[%d]", i+1), LocalTemplatesJSON: "[]"})
			}()
		}
		close(start)
		for range n {
			require.NoError(t, <-errs)
		}
		got, err := nmstore.NewNodeStore(backend.DB(), true).GetStatus(ctx, "node")
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("[%d]", got.HeartbeatReceivedUnix), got.ImagesJSON)
	})
}
