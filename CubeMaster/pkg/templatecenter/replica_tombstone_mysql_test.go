// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

func newTombstoneDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, _ := newArtifactGCMySQL(t)
	if err := db.AutoMigrate(
		&models.RootfsArtifact{}, &models.TemplateReplica{}, &models.TemplateDefinition{},
		&models.SnapshotRecord{}, &models.TemplateImageJob{},
	); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	orig := store.db
	store.db = db
	t.Cleanup(func() { store.db = orig })
	return db
}

func TestMarkReplicaCleanupRequiredMySQL(t *testing.T) {
	db := newTombstoneDB(t)
	ctx := context.Background()
	rows := []models.TemplateReplica{
		{TemplateID: "tpl-a", NodeID: "n1"},
		{TemplateID: "tpl-a", NodeID: "n2", CleanupRequired: true}, // already marked: RowsAffected would be 0
		{TemplateID: "tpl-a", NodeID: "n3", TemplateVariantID: "v1"},
		{TemplateID: "tpl-a", NodeID: "n3x", TemplateVariantID: "v2"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name    string
		locator templateCleanupLocator
		want    bool
	}{
		{"plain row is marked", templateCleanupLocator{NodeID: "n1"}, true},
		{"already marked row still reports a tombstone", templateCleanupLocator{NodeID: "n2"}, true},
		{"variant is matched exactly", templateCleanupLocator{NodeID: "n3", TemplateVariantID: "v1"}, true},
		{"wrong variant has no row", templateCleanupLocator{NodeID: "n3", TemplateVariantID: "other"}, false},
		{"node without a row", templateCleanupLocator{NodeID: "nX"}, false},
	}
	for _, c := range cases {
		got, err := markReplicaCleanupRequired(ctx, "tpl-a", c.locator)
		if err != nil || got != c.want {
			t.Fatalf("%s: got=%v err=%v want=%v", c.name, got, err, c.want)
		}
	}
	var untouched models.TemplateReplica
	if err := db.Where("template_id = ? AND node_id = ?", "tpl-a", "n3x").First(&untouched).Error; err != nil {
		t.Fatal(err)
	}
	if untouched.CleanupRequired {
		t.Fatal("a different variant on the same template must not be marked")
	}
}

func TestMetadataAndSnapshotReleaseKeepTombstonesMySQL(t *testing.T) {
	db := newTombstoneDB(t)
	ctx := context.Background()
	for _, id := range []string{"tpl-meta", "snap-rel"} {
		for _, r := range []models.TemplateReplica{
			{TemplateID: id, NodeID: "live"},
			{TemplateID: id, NodeID: "dead", CleanupRequired: true},
		} {
			if err := db.Create(&r).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Create(&models.SnapshotRecord{SnapshotID: "snap-rel", Status: StatusDeleting}).Error; err != nil {
		t.Fatal(err)
	}

	if err := cleanupTemplateMetadata(ctx, "tpl-meta"); err != nil {
		t.Fatal(err)
	}
	if err := releaseSnapshotArtifactReferences(ctx, "snap-rel", &templateCleanupTargets{ArtifactIDs: map[string]struct{}{}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"tpl-meta", "snap-rel"} {
		var rows []models.TemplateReplica
		if err := db.Where("template_id = ?", id).Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].NodeID != "dead" {
			t.Fatalf("%s: only the tombstone may survive, got %+v", id, rows)
		}
	}
}

// A redo flag left by failed distribution must not outlive a delete whose node
// confirmed cleanup; only the deferred node's tombstone may survive.
func TestDeleteClearsConfirmedRedoFlagsMySQL(t *testing.T) {
	db := newTombstoneDB(t)
	ctx := context.Background()
	stale := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, r := range []models.TemplateReplica{
		{TemplateID: "tpl-del", NodeID: "failed", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed, CleanupRequired: true},
		{TemplateID: "tpl-del", NodeID: "ready", NodeIP: "10.0.0.3", Status: ReplicaStatusReady},
		{TemplateID: "tpl-del", NodeID: "gone", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
	} {
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&models.TemplateReplica{}).Where("template_id = ? AND node_id = ?", "tpl-del", "ready").
		UpdateColumn("updated_at", stale).Error; err != nil {
		t.Fatal(err)
	}
	realMark, realClear := markReplicaCleanupRequired, clearReplicaCleanupRequired
	_, rpcs, _ := stubDeferringCleanupFull(t, nil, nil, nil, nil)
	markReplicaCleanupRequired, clearReplicaCleanupRequired = realMark, realClear

	if err := cleanupTemplateReplicasDeferringUnreachable(ctx, "tpl-del", []templateCleanupLocator{
		{NodeID: "failed", NodeIP: "10.0.0.2"}, {NodeID: "ready", NodeIP: "10.0.0.3"}, {NodeID: "gone", NodeIP: "10.0.0.1"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	var ready models.TemplateReplica
	if err := db.Where("template_id = ? AND node_id = ?", "tpl-del", "ready").First(&ready).Error; err != nil {
		t.Fatal(err)
	}
	if !ready.UpdatedAt.Equal(stale) {
		t.Fatalf("an unflagged READY row must not be rewritten: updated_at=%s", ready.UpdatedAt)
	}
	if err := cleanupTemplateMetadata(ctx, "tpl-del"); err != nil {
		t.Fatal(err)
	}
	var rows []models.TemplateReplica
	if err := db.Where("template_id = ?", "tpl-del").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].NodeID != "gone" || !rows[0].CleanupRequired || rpcs["10.0.0.1"] != 0 {
		t.Fatalf("only the deferred node's tombstone may survive: rows=%+v rpcs=%v", rows, rpcs)
	}
}

func TestCountArtifactReferencesIgnoresOnlyPureTombstonesMySQL(t *testing.T) {
	db := newTombstoneDB(t)
	ctx := context.Background()
	const art = "rfs-ref"
	mustCreate := func(v any) {
		t.Helper()
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	count := func() int64 {
		t.Helper()
		n, err := countArtifactReferencesTx(ctx, db, art, "")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	mustCreate(&models.TemplateReplica{TemplateID: "gone-a", NodeID: "n", ArtifactID: art, CleanupRequired: true})
	if got := count(); got != 0 {
		t.Fatalf("a tombstone without definition or snapshot must not own the artifact, got %d", got)
	}
	mustCreate(&models.TemplateReplica{TemplateID: "stale", NodeID: "n", ArtifactID: art})
	if got := count(); got != 1 {
		t.Fatalf("a replica that is not a tombstone must still count, got %d", got)
	}
	if err := db.Exec("DELETE FROM " + constants.TemplateReplicaTableName + " WHERE template_id = 'stale'").Error; err != nil {
		t.Fatal(err)
	}

	mustCreate(&models.TemplateDefinition{TemplateID: "gone-a", Status: StatusReady})
	if got := count(); got != 1 {
		t.Fatalf("a tombstone whose definition still exists must count, got %d", got)
	}
	if err := db.Exec("DELETE FROM " + constants.TemplateDefinitionTableName).Error; err != nil {
		t.Fatal(err)
	}

	mustCreate(&models.SnapshotRecord{SnapshotID: "gone-a", Status: StatusDeleting})
	if got := count(); got != 1 {
		t.Fatalf("a tombstone whose snapshot still exists must count, got %d", got)
	}
}

// With the notify to TC failing, a later GC pass (empty exclusion) must not
// flip the finalised artifact back to READY because of a pure tombstone.
func TestArtifactStaysCleanupPendingWithTombstoneMySQL(t *testing.T) {
	h := newPlacementCleanupHarness(t)
	ctx := context.Background()
	const art = "rfs-tomb"
	h.seed(t, art)
	if err := h.db.Create(&models.TemplateReplica{TemplateID: "gone", NodeID: "n", ArtifactID: art, CleanupRequired: true}).Error; err != nil {
		t.Fatal(err)
	}
	origNotify := requestTemplateCenterArtifactDelete
	t.Cleanup(func() { requestTemplateCenterArtifactDelete = origNotify })
	requestTemplateCenterArtifactDelete = func(context.Context, string) error {
		h.notifies++
		return errors.New("TC unavailable")
	}
	// The second call models a later GC pass after the failed notification.
	for range 2 {
		if err := cleanupArtifactFully(ctx, art, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if h.notifies != 2 || h.artifactStatus(t, art) != ArtifactStatusCleanupPending {
		t.Fatalf("tombstone must not hold or revive the artifact: notifies=%d status=%s", h.notifies, h.artifactStatus(t, art))
	}
}

func TestOrphanSweepPagesMySQL(t *testing.T) {
	db := newTombstoneDB(t)
	ctx := context.Background()
	for _, n := range []string{"a", "b", "c"} {
		if err := db.Create(&models.TemplateReplica{TemplateID: "gone-" + n, NodeID: n, CleanupRequired: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	orphanScanCursor.mu.Lock()
	orphanScanCursor.id = 0
	orphanScanCursor.mu.Unlock()
	t.Cleanup(func() { orphanScanCursor.mu.Lock(); orphanScanCursor.id = 0; orphanScanCursor.mu.Unlock() })

	first, err := listOrphanCleanupReplicas(ctx, 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("first page: %v err=%v", first, err)
	}
	second, err := listOrphanCleanupReplicas(ctx, 2)
	if err != nil || len(second) != 1 || second[0].ID <= first[1].ID {
		t.Fatalf("second page must continue after the cursor: %v err=%v", second, err)
	}
	third, err := listOrphanCleanupReplicas(ctx, 2)
	if err != nil || len(third) != 2 || third[0].ID != first[0].ID {
		t.Fatalf("the scan must wrap to the start: %v err=%v", third, err)
	}
}
