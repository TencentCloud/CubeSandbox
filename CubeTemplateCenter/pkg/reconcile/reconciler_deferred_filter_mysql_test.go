// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

// TestCleanupPendingArtifactsDeferredFilterMySQL verifies deferred placements
// don't block the TC backstop deleter, while live placements do.
func TestCleanupPendingArtifactsDeferredFilterMySQL(t *testing.T) {
	env := newMySQLDockerEnv(t)
	db := openMySQLGORM(t, env)

	now := time.Now()
	old := now.Add(-cleanupPendingGrace - time.Minute)

	seed := func(t *testing.T, artifactID string, deferredAt int64) {
		t.Helper()
		if err := db.Exec("DELETE FROM " + constants.RootfsArtifactTableName).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("DELETE FROM " + constants.ArtifactNodePlacementTableName).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.RootfsArtifact{
			Model:      gorm.Model{UpdatedAt: old},
			ArtifactID: artifactID,
			Status:     "CLEANUP_PENDING",
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.ArtifactNodePlacement{
			ArtifactID:   artifactID,
			NodeID:       "node-1",
			NodeIP:       "10.0.0.1",
			InstanceType: "test",
			DeferredAt:   deferredAt,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	t.Run("only deferred placement: deleter is called", func(t *testing.T) {
		seed(t, "art-deferred", now.Unix())
		deleter := &fakeDeleter{}
		r := &Reconciler{db: db, deleter: deleter}
		if err := r.cleanupPendingArtifacts(context.Background()); err != nil {
			t.Fatalf("cleanupPendingArtifacts: %v", err)
		}
		if len(deleter.deleted) != 1 || deleter.deleted[0] != "art-deferred" {
			t.Fatalf("deferred placement must not block delete, deleted=%v", deleter.deleted)
		}
	})

	t.Run("live placement: delete is skipped", func(t *testing.T) {
		seed(t, "art-live", 0)
		deleter := &fakeDeleter{}
		r := &Reconciler{db: db, deleter: deleter}
		if err := r.cleanupPendingArtifacts(context.Background()); err != nil {
			t.Fatalf("cleanupPendingArtifacts: %v", err)
		}
		if len(deleter.deleted) != 0 {
			t.Fatalf("live placement must block delete, deleted=%v", deleter.deleted)
		}
	})

	t.Run("mixed: deferred deleted, live skipped", func(t *testing.T) {
		if err := db.Exec("DELETE FROM " + constants.RootfsArtifactTableName).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("DELETE FROM " + constants.ArtifactNodePlacementTableName).Error; err != nil {
			t.Fatal(err)
		}
		for _, a := range []struct {
			id         string
			deferredAt int64
		}{
			{"art-deferred", now.Unix()},
			{"art-live", 0},
		} {
			if err := db.Create(&models.RootfsArtifact{
				Model:      gorm.Model{UpdatedAt: old},
				ArtifactID: a.id,
				Status:     "CLEANUP_PENDING",
			}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&models.ArtifactNodePlacement{
				ArtifactID:   a.id,
				NodeID:       "node-1",
				NodeIP:       "10.0.0.1",
				InstanceType: "test",
				DeferredAt:   a.deferredAt,
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
		deleter := &fakeDeleter{}
		r := &Reconciler{db: db, deleter: deleter}
		if err := r.cleanupPendingArtifacts(context.Background()); err != nil {
			t.Fatalf("cleanupPendingArtifacts: %v", err)
		}
		if len(deleter.deleted) != 1 || deleter.deleted[0] != "art-deferred" {
			t.Fatalf("only deferred artifact must be deleted, deleted=%v", deleter.deleted)
		}
	})
}
