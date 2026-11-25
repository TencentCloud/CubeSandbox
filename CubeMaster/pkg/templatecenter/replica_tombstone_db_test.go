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
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"gorm.io/gorm"
)

func newTombstoneDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, _ := newArtifactGCMySQL(t)
	if err := db.AutoMigrate(
		&models.RootfsArtifact{}, &models.TemplateReplica{}, &models.TemplateDefinition{},
		&models.SnapshotRecord{}, &models.TemplateImageJob{}, &models.ArtifactNodePlacement{},
	); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	orig := store.db
	store.db = db
	t.Cleanup(func() { store.db = orig })
	return db
}

// tombstoneCases run on both engines: the cleanup_required predicates are
// plain SQL whose boolean and NOT EXISTS handling differs per dialect.
var tombstoneCases = []struct {
	name string
	run  func(*testing.T, *gorm.DB)
}{
	{"MarkReplicaCleanupRequired", testMarkReplicaCleanupRequired},
	{"DeleteTombstonesVariantRowForVariantlessLocator", testDeleteTombstonesVariantRowForVariantlessLocator},
	{"MetadataAndSnapshotReleaseKeepTombstones", testMetadataAndSnapshotReleaseKeepTombstones},
	{"DeleteClearsConfirmedRedoFlags", testDeleteClearsConfirmedRedoFlags},
	{"CleanupTemplateReplicasClearsFlagsWhenAllConfirm", testCleanupTemplateReplicasClearsFlagsWhenAllConfirm},
	{"WarmReadyTemplateLocalitySkipsRowsAwaitingCleanup", testWarmReadyTemplateLocalitySkipsRowsAwaitingCleanup},
	{"CountArtifactReferencesIgnoresOnlyPureTombstones", testCountArtifactReferencesIgnoresOnlyPureTombstones},
	{"ArtifactStaysCleanupPendingWithTombstone", testArtifactStaysCleanupPendingWithTombstone},
	{"OrphanSweepPages", testOrphanSweepPages},
}

func TestReplicaTombstonesMySQL(t *testing.T) {
	for _, c := range tombstoneCases {
		t.Run(c.name, func(t *testing.T) { c.run(t, newTombstoneDB(t)) })
	}
}

// One migrated PostgreSQL serves every case; each starts from empty tables.
func TestReplicaTombstonesPostgreSQL(t *testing.T) {
	env := newPGDockerEnv(t)
	defer env.teardown()
	db := openMigratedPostgresGORM(t, env)
	orig := store.db
	store.db = db
	t.Cleanup(func() { store.db = orig })
	for _, c := range tombstoneCases {
		t.Run(c.name, func(t *testing.T) {
			truncateTombstoneTables(t, db)
			c.run(t, db)
		})
	}
}

func truncateTombstoneTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range []string{
		constants.TemplateReplicaTableName, constants.TemplateDefinitionTableName, constants.SnapshotTableName,
		constants.RootfsArtifactTableName, constants.TemplateImageJobTableName, constants.ArtifactNodePlacementTableName,
	} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
}

func testMarkReplicaCleanupRequired(t *testing.T, db *gorm.DB) {
	ctx := context.Background()
	rows := []models.TemplateReplica{
		{TemplateID: "tpl-a", NodeID: "n1"},
		{TemplateID: "tpl-a", NodeID: "n2", CleanupRequired: true}, // already marked: RowsAffected would be 0
		{TemplateID: "tpl-a", NodeID: "n3", TemplateVariantID: "v1"},
		{TemplateID: "tpl-a", NodeID: "n4", TemplateVariantID: "v2"},
		{TemplateID: "tpl-a", NodeID: "n5"},
		{TemplateID: "tpl-b", NodeID: "n1"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	// The row is keyed by (template_id, node_id): a locator's variant must not
	// decide whether the node's tombstone is recorded.
	cases := []struct {
		name    string
		locator templateCleanupLocator
		want    bool
	}{
		{"plain row is marked", templateCleanupLocator{NodeID: "n1"}, true},
		{"already marked row still reports a tombstone", templateCleanupLocator{NodeID: "n2"}, true},
		{"matching variant", templateCleanupLocator{NodeID: "n3", TemplateVariantID: "v1"}, true},
		{"variant-less locator on a variant row", templateCleanupLocator{NodeID: "n4"}, true},
		{"node without a row", templateCleanupLocator{NodeID: "nX"}, false},
	}
	for _, c := range cases {
		got, err := markReplicaCleanupRequired(ctx, "tpl-a", c.locator)
		if err != nil || got != c.want {
			t.Fatalf("%s: got=%v err=%v want=%v", c.name, got, err, c.want)
		}
	}
	for _, key := range [][2]string{{"tpl-a", "n5"}, {"tpl-b", "n1"}} {
		var untouched models.TemplateReplica
		if err := db.Where("template_id = ? AND node_id = ?", key[0], key[1]).First(&untouched).Error; err != nil {
			t.Fatal(err)
		}
		if untouched.CleanupRequired {
			t.Fatalf("%s/%s: another node or template must not be marked", key[0], key[1])
		}
	}
}

// A commit job's variant-less locator and the variant replica row name the same
// unreachable node; both must resolve to its tombstone instead of an RPC.
func testDeleteTombstonesVariantRowForVariantlessLocator(t *testing.T, db *gorm.DB) {
	ctx := context.Background()
	if err := db.Create(&models.TemplateReplica{
		TemplateID: "tpl-jfs", NodeID: "gone", NodeIP: "10.0.0.1", TemplateVariantID: "v1", Status: ReplicaStatusReady,
	}).Error; err != nil {
		t.Fatal(err)
	}
	realMark, realClear := markReplicaCleanupRequired, clearReplicaCleanupRequired
	_, rpcs, _ := stubDeferringCleanupFull(t, nil, nil, nil, nil)
	markReplicaCleanupRequired, clearReplicaCleanupRequired = realMark, realClear

	if err := cleanupTemplateReplicasDeferringUnreachable(ctx, "tpl-jfs", []templateCleanupLocator{
		{NodeID: "gone", NodeIP: "10.0.0.1", TemplateVariantID: "v1"},
		{NodeID: "gone", NodeIP: "10.0.0.1"},
	}, ""); err != nil {
		t.Fatalf("delete must not fail on the unreachable node: %v", err)
	}
	if err := cleanupTemplateMetadata(ctx, "tpl-jfs"); err != nil {
		t.Fatal(err)
	}
	var rows []models.TemplateReplica
	if err := db.Where("template_id = ?", "tpl-jfs").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].CleanupRequired || rpcs["10.0.0.1"] != 0 {
		t.Fatalf("the node must keep one tombstone and receive no RPC: rows=%+v rpcs=%v", rows, rpcs)
	}
}

func testMetadataAndSnapshotReleaseKeepTombstones(t *testing.T, db *gorm.DB) {
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
func testDeleteClearsConfirmedRedoFlags(t *testing.T, db *gorm.DB) {
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
	// Compare DB reads, not the Go value: zoneless PG timestamps shift on read.
	readyUpdatedAt := func() time.Time {
		t.Helper()
		var r models.TemplateReplica
		if err := db.Where("template_id = ? AND node_id = ?", "tpl-del", "ready").First(&r).Error; err != nil {
			t.Fatal(err)
		}
		return r.UpdatedAt
	}
	before := readyUpdatedAt()
	realMark, realClear := markReplicaCleanupRequired, clearReplicaCleanupRequired
	_, rpcs, _ := stubDeferringCleanupFull(t, nil, nil, nil, nil)
	markReplicaCleanupRequired, clearReplicaCleanupRequired = realMark, realClear

	if err := cleanupTemplateReplicasDeferringUnreachable(ctx, "tpl-del", []templateCleanupLocator{
		{NodeID: "failed", NodeIP: "10.0.0.2"}, {NodeID: "ready", NodeIP: "10.0.0.3"}, {NodeID: "gone", NodeIP: "10.0.0.1"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	if after := readyUpdatedAt(); !after.Equal(before) {
		t.Fatalf("an unflagged READY row must not be rewritten: updated_at %s -> %s", before, after)
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

// Rollback and FAILED-rebuild clean every node before metadata cleanup, so once
// all nodes confirm no flagged row may survive for the orphan sweep.
func testCleanupTemplateReplicasClearsFlagsWhenAllConfirm(t *testing.T, db *gorm.DB) {
	ctx := context.Background()
	for _, r := range []models.TemplateReplica{
		{TemplateID: "tpl-rb", NodeID: "n1", NodeIP: "10.0.0.1", Status: ReplicaStatusFailed, CleanupRequired: true},
		{TemplateID: "tpl-rb", NodeID: "n2", NodeIP: "10.0.0.2", Status: ReplicaStatusReady},
		{TemplateID: "tpl-other", NodeID: "n1", NodeIP: "10.0.0.1", Status: ReplicaStatusFailed, CleanupRequired: true},
	} {
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	rpcErr := errors.New("connection refused")
	_, rpcs, _ := stubDeferringCleanupFull(t, nil, nil, map[string]error{"10.0.0.2": rpcErr}, nil)
	flagged := func(id string) int64 {
		t.Helper()
		var n int64
		if err := db.Model(&models.TemplateReplica{}).Where("template_id = ? AND cleanup_required = ?", id, true).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := cleanupTemplateReplicas(ctx, "tpl-rb"); !errors.Is(err, rpcErr) || flagged("tpl-rb") != 1 {
		t.Fatalf("a failed node must keep the flag: err=%v flagged=%d", err, flagged("tpl-rb"))
	}
	cleanupTemplateOnCubelet = func(_ context.Context, addr string, _ *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		rpcs[addr]++
		return &cubeboxv1.CleanupTemplateResponse{}, nil
	}
	if err := cleanupTemplateReplicas(ctx, "tpl-rb"); err != nil {
		t.Fatal(err)
	}
	if err := cleanupTemplateMetadata(ctx, "tpl-rb"); err != nil {
		t.Fatal(err)
	}
	var left int64
	if err := db.Model(&models.TemplateReplica{}).Where("template_id = ?", "tpl-rb").Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 0 || flagged("tpl-other") != 1 {
		t.Fatalf("confirmed rows must go and other templates stay: left=%d other=%d", left, flagged("tpl-other"))
	}
}

// Startup warm-up must not seed the locality cache with rows awaiting cleanup.
func testWarmReadyTemplateLocalitySkipsRowsAwaitingCleanup(t *testing.T, db *gorm.DB) {
	ctx := context.Background()
	for _, r := range []models.TemplateReplica{
		{TemplateID: "tpl-warm", NodeID: "ok", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
		{TemplateID: "tpl-warm", NodeID: "pending", NodeIP: "10.0.0.2", Status: ReplicaStatusReady, CleanupRequired: true},
	} {
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { invalidateTemplateCaches("tpl-warm") })
	if err := warmReadyTemplateLocality(ctx); err != nil {
		t.Fatal(err)
	}
	replicas, ok := getCachedTemplateLocality("tpl-warm")
	if !ok || len(replicas) != 1 || replicas[0].NodeID != "ok" {
		t.Fatalf("only the clean READY row may be cached: ok=%v replicas=%+v", ok, replicas)
	}
}

func testCountArtifactReferencesIgnoresOnlyPureTombstones(t *testing.T, db *gorm.DB) {
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
func testArtifactStaysCleanupPendingWithTombstone(t *testing.T, db *gorm.DB) {
	h := newPlacementCleanupHarnessOn(t, db)
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

func testOrphanSweepPages(t *testing.T, db *gorm.DB) {
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
