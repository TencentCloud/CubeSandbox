// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"testing"

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
	// The node is absent from the live cache in tests, so the stored IP is
	// used as the fallback address.
	if cleaned["node-ok"] != "10.0.0.1" || cleaned["node-dead"] != "10.0.0.2" {
		t.Fatalf("locators = %v, want stored IPs used as fallback", cleaned)
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
