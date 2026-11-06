// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// stubSweepStore installs a non-nil DryRun store handle so isReady() passes;
// the sweep itself only touches the database through its seams, which the
// tests replace.
func stubSweepStore(t *testing.T) {
	t.Helper()
	sqlDB, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3306)/unused?parseTime=true")
	if err != nil {
		t.Fatalf("open sql: %v", err)
	}
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	old := store.db
	store.db = db
	t.Cleanup(func() { store.db = old })
}

func TestReconcileOrphanReplicaCleanups(t *testing.T) {
	stubSweepStore(t)
	stubReplicaRegistry(t, true, map[string]string{
		"node-ok":   "10.1.0.1",
		"node-dead": "10.1.0.2",
	})

	rows := []orphanCleanupReplica{
		{ID: 1, TemplateID: "tpl-gone", NodeID: "node-ok", NodeIP: "10.0.0.1"},
		{ID: 2, TemplateID: "tpl-gone", NodeID: "node-dead", NodeIP: "10.0.0.2"},
	}
	oldList, oldCleanup, oldDelete := listOrphanCleanupReplicas, cleanupOrphanReplicaOnNode, deleteReplicaRowByID
	t.Cleanup(func() {
		listOrphanCleanupReplicas, cleanupOrphanReplicaOnNode, deleteReplicaRowByID = oldList, oldCleanup, oldDelete
	})
	listOrphanCleanupReplicas = func(context.Context, int) ([]orphanCleanupReplica, error) { return rows, nil }

	cleaned := map[string]string{} // nodeID -> hostIP used
	cleanupOrphanReplicaOnNode = func(_ context.Context, templateID string, locator templateCleanupLocator, backend string) error {
		if templateID != "tpl-gone" {
			t.Fatalf("unexpected template %s", templateID)
		}
		if backend != "" {
			t.Fatalf("backend hint must be empty after the definition row is gone, got %q", backend)
		}
		cleaned[locator.NodeID] = locator.NodeIP
		if locator.NodeID == "node-dead" {
			return errors.New("context deadline exceeded")
		}
		return nil
	}

	var deleted []uint
	deleteReplicaRowByID = func(_ context.Context, id uint) error {
		deleted = append(deleted, id)
		return nil
	}

	if err := reconcileOrphanReplicaCleanups(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// The node that answered is converged and its row removed; the dead node
	// keeps its row for the next tick.
	if len(deleted) != 1 || deleted[0] != 1 {
		t.Fatalf("deleted rows = %v, want [1]", deleted)
	}
	// Registered nodes are retried at their current addresses, even when the
	// persisted cleanup row still carries an old address.
	if cleaned["node-ok"] != "10.1.0.1" || cleaned["node-dead"] != "10.1.0.2" {
		t.Fatalf("locators = %v, want current registry addresses", cleaned)
	}
}

// A scan failure must surface as an error (the pass logs it) rather than
// silently skipping the sweep.
func TestReconcileOrphanReplicaCleanupsScanError(t *testing.T) {
	stubSweepStore(t)
	oldList := listOrphanCleanupReplicas
	t.Cleanup(func() { listOrphanCleanupReplicas = oldList })
	listOrphanCleanupReplicas = func(context.Context, int) ([]orphanCleanupReplica, error) {
		return nil, errors.New("db down")
	}
	if err := reconcileOrphanReplicaCleanups(context.Background()); err == nil {
		t.Fatal("want error on scan failure")
	}
}

func TestReconcileOrphanReplicaCleanupsDropsRetiredNodeWithoutRPC(t *testing.T) {
	stubSweepStore(t)
	stubReplicaRegistry(t, true, map[string]string{"node-live": "10.1.0.8"})
	oldList, oldCleanup, oldDelete := listOrphanCleanupReplicas, cleanupOrphanReplicaOnNode, deleteReplicaRowByID
	t.Cleanup(func() {
		listOrphanCleanupReplicas, cleanupOrphanReplicaOnNode, deleteReplicaRowByID = oldList, oldCleanup, oldDelete
	})
	listOrphanCleanupReplicas = func(context.Context, int) ([]orphanCleanupReplica, error) {
		return []orphanCleanupReplica{{
			ID: 7, TemplateID: "tpl-gone", NodeID: "node-retired", NodeIP: "10.0.0.7",
		}}, nil
	}
	cleanupOrphanReplicaOnNode = func(context.Context, string, templateCleanupLocator, string) error {
		t.Fatal("retired node must not receive a cleanup RPC")
		return nil
	}
	var deleted []uint
	deleteReplicaRowByID = func(_ context.Context, id uint) error {
		deleted = append(deleted, id)
		return nil
	}

	if err := reconcileOrphanReplicaCleanups(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != 7 {
		t.Fatalf("deleted rows = %v, want [7]", deleted)
	}
}

func TestListOrphanCleanupReplicasExcludesLiveOwners(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&models.TemplateReplica{},
		&models.SnapshotRecord{},
	))
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID: "tpl-active",
	}).Error)
	require.NoError(t, db.Create(&models.SnapshotRecord{
		SnapshotID: "snap-active",
	}).Error)
	require.NoError(t, db.Create(&[]models.TemplateReplica{
		{TemplateID: "tpl-active", NodeID: "node-a", CleanupRequired: true},
		{TemplateID: "snap-active", NodeID: "node-b", CleanupRequired: true},
		{TemplateID: "tpl-orphan", NodeID: "node-c", CleanupRequired: true},
	}).Error)

	rows, err := listOrphanCleanupReplicas(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "tpl-orphan", rows[0].TemplateID)
	require.Equal(t, "node-c", rows[0].NodeID)
}
