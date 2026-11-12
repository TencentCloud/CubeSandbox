// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	imagev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
	"gorm.io/gorm"
)

// placementCleanupHarness drives the real cleanupArtifactFully against MySQL,
// with only the network edges (cubelet RPC, TC notify, node view) stubbed.
type placementCleanupHarness struct {
	db       *gorm.DB
	destroys map[string]int // node ip -> destroy RPC count
	notifies int
	absent   map[string]time.Time // node id -> absent since (absent from view)
	present  map[string]string    // node id -> ip (present in view)
	rpcErr   map[string]error     // node ip -> destroy error
}

func newPlacementCleanupHarness(t *testing.T) *placementCleanupHarness {
	t.Helper()
	gormDB, _ := newArtifactGCMySQL(t)
	if err := gormDB.AutoMigrate(
		&models.ArtifactNodePlacement{},
		&models.TemplateReplica{},
		&models.TemplateImageJob{},
		&models.TemplateDefinition{},
		&models.SnapshotRecord{},
	); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	origDB := store.db
	store.db = gormDB
	resetDeferredScanCursor()
	t.Cleanup(func() { store.db = origDB; resetDeferredScanCursor() })

	h := &placementCleanupHarness{
		db:       gormDB,
		destroys: map[string]int{},
		absent:   map[string]time.Time{},
		present:  map[string]string{},
		rpcErr:   map[string]error{},
	}

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(localcache.AbsentSince, func(id string) (time.Time, bool) {
		since, ok := h.absent[id]
		return since, ok
	})
	patches.ApplyFunc(localcache.GetNode, func(id string) (*node.Node, bool) {
		ip, ok := h.present[id]
		if !ok {
			return nil, false
		}
		return &node.Node{InsID: id, IP: ip}, true
	})
	patches.ApplyFunc(localcache.NodeIDs, func() []string {
		ids := make([]string, 0, len(h.present))
		for id := range h.present {
			ids = append(ids, id)
		}
		return ids
	})

	origDelete, origNotify, origAddr := deleteImageOnCubelet, requestTemplateCenterArtifactDelete, getCubeletAddrForDelete
	t.Cleanup(func() {
		deleteImageOnCubelet, requestTemplateCenterArtifactDelete, getCubeletAddrForDelete = origDelete, origNotify, origAddr
	})
	getCubeletAddrForDelete = func(hostIP string) string { return hostIP + ":9000" }
	deleteImageOnCubelet = func(_ context.Context, addr string, _ *imagev1.DestroyImageRequest) (*imagev1.DestroyImageResponse, error) {
		ip := strings.SplitN(addr, ":", 2)[0]
		h.destroys[ip]++
		return &imagev1.DestroyImageResponse{}, h.rpcErr[ip]
	}
	requestTemplateCenterArtifactDelete = func(context.Context, string) error {
		h.notifies++
		return nil
	}
	return h
}

func (h *placementCleanupHarness) seed(t *testing.T, artifactID string, placements ...models.ArtifactNodePlacement) {
	t.Helper()
	if err := h.db.Exec("DELETE FROM " + constants.RootfsArtifactTableName).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Exec("DELETE FROM " + constants.ArtifactNodePlacementTableName).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Create(&models.RootfsArtifact{ArtifactID: artifactID, Status: ArtifactStatusReady}).Error; err != nil {
		t.Fatal(err)
	}
	for i := range placements {
		placements[i].ArtifactID = artifactID
		if err := h.db.Create(&placements[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func (h *placementCleanupHarness) placementIDs(t *testing.T, artifactID string) []string {
	t.Helper()
	var rows []models.ArtifactNodePlacement
	if err := h.db.Where("artifact_id = ?", artifactID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.NodeID)
	}
	sort.Strings(ids)
	return ids
}

func (h *placementCleanupHarness) deferredAt(t *testing.T, artifactID, nodeID string) int64 {
	t.Helper()
	var row models.ArtifactNodePlacement
	if err := h.db.Where("artifact_id = ? AND node_id = ?", artifactID, nodeID).First(&row).Error; err != nil {
		t.Fatalf("load placement %s/%s: %v", artifactID, nodeID, err)
	}
	return row.DeferredAt
}

func (h *placementCleanupHarness) artifactStatus(t *testing.T, artifactID string) string {
	t.Helper()
	var a models.RootfsArtifact
	if err := h.db.Where("artifact_id = ?", artifactID).First(&a).Error; err != nil {
		t.Fatal(err)
	}
	return a.Status
}

func TestCleanupArtifactFullyAbsentNodeMySQL(t *testing.T) {
	h := newPlacementCleanupHarness(t)
	ctx := context.Background()
	const art = "art-1"
	longAgo := time.Now().Add(-placementAbsenceGrace - time.Minute)

	t.Run("absent past grace defers the row, finalises and notifies", func(t *testing.T) {
		h.destroys, h.notifies = map[string]int{}, 0
		h.absent = map[string]time.Time{"gone": longAgo}
		h.present = map[string]string{"live": "10.0.0.2"}
		h.seed(t, art,
			models.ArtifactNodePlacement{NodeID: "gone", NodeIP: "10.0.0.1"},
			models.ArtifactNodePlacement{NodeID: "live", NodeIP: "10.0.0.2"},
		)
		if err := cleanupArtifactFully(ctx, art, "", ""); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if h.destroys["10.0.0.1"] != 0 {
			t.Fatal("absent node must not receive a destroy RPC")
		}
		if h.destroys["10.0.0.2"] != 1 {
			t.Fatalf("live node destroys = %d, want 1", h.destroys["10.0.0.2"])
		}
		if got := h.placementIDs(t, art); len(got) != 1 || got[0] != "gone" {
			t.Fatalf("surviving placements = %v, want only the deferred one", got)
		}
		if h.deferredAt(t, art, "gone") == 0 {
			t.Fatal("surviving row must be marked deferred")
		}
		if h.notifies != 1 {
			t.Fatalf("TC notifies = %d, want 1 (pin released)", h.notifies)
		}
	})

	t.Run("absent inside grace still attempts destroy and finalises normally", func(t *testing.T) {
		h.destroys, h.notifies = map[string]int{}, 0
		h.absent = map[string]time.Time{"flicker": time.Now().Add(-time.Minute)}
		h.present = map[string]string{}
		h.seed(t, art, models.ArtifactNodePlacement{NodeID: "flicker", NodeIP: "10.0.0.3"})
		if err := cleanupArtifactFully(ctx, art, "", ""); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		// Not deferred: the stored IP is still tried (node may be reachable).
		if h.destroys["10.0.0.3"] != 1 {
			t.Fatalf("destroys = %d, want the stored IP tried once", h.destroys["10.0.0.3"])
		}
		if h.notifies != 1 {
			t.Fatalf("notifies = %d: a successful destroy finalises as before", h.notifies)
		}
		if got := h.placementIDs(t, art); len(got) != 0 {
			t.Fatalf("placements = %v, want none after a confirmed destroy", got)
		}
	})

	t.Run("absent inside grace with failing RPC stays pending", func(t *testing.T) {
		h.destroys, h.notifies = map[string]int{}, 0
		h.absent = map[string]time.Time{"flicker": time.Now().Add(-time.Minute)}
		h.rpcErr = map[string]error{"10.0.0.3": errors.New("deadline exceeded")}
		t.Cleanup(func() { h.rpcErr = map[string]error{} })
		h.seed(t, art, models.ArtifactNodePlacement{NodeID: "flicker", NodeIP: "10.0.0.3"})
		if err := cleanupArtifactFully(ctx, art, "", ""); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if h.notifies != 0 || h.artifactStatus(t, art) != ArtifactStatusCleanupPending {
			t.Fatalf("must stay CLEANUP_PENDING without notifying (notifies=%d status=%s)", h.notifies, h.artifactStatus(t, art))
		}
		if got := h.placementIDs(t, art); len(got) != 1 {
			t.Fatalf("placement row must survive a failed destroy, got %v", got)
		}
	})

	t.Run("legacy ip: placement is never treated as absent", func(t *testing.T) {
		h.destroys, h.notifies = map[string]int{}, 0
		h.absent = map[string]time.Time{"ip:10.0.0.9": longAgo}
		h.rpcErr = map[string]error{"10.0.0.9": errors.New("unreachable")}
		t.Cleanup(func() { h.rpcErr = map[string]error{} })
		h.seed(t, art, models.ArtifactNodePlacement{NodeID: "ip:10.0.0.9", NodeIP: "10.0.0.9"})
		if err := cleanupArtifactFully(ctx, art, "", ""); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if h.destroys["10.0.0.9"] != 1 || h.notifies != 0 {
			t.Fatalf("legacy row must keep the old retry path (destroys=%d notifies=%d)", h.destroys["10.0.0.9"], h.notifies)
		}
	})

	t.Run("unhealthy but present node is cleaned, not deferred", func(t *testing.T) {
		h.destroys, h.notifies = map[string]int{}, 0
		h.absent = map[string]time.Time{}
		h.present = map[string]string{"sick": "10.0.0.4"} // present in the view, health irrelevant
		h.seed(t, art, models.ArtifactNodePlacement{NodeID: "sick", NodeIP: "10.0.0.4"})
		if err := cleanupArtifactFully(ctx, art, "", ""); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if h.destroys["10.0.0.4"] != 1 {
			t.Fatalf("present node must be destroyed normally, got %d", h.destroys["10.0.0.4"])
		}
	})

	t.Run("node re-registered under a new ip is destroyed at the live address", func(t *testing.T) {
		h.destroys, h.notifies = map[string]int{}, 0
		h.absent = map[string]time.Time{}
		h.present = map[string]string{"moved": "10.0.0.6"}
		h.rpcErr = map[string]error{"10.0.0.5": errors.New("deadline exceeded")} // the stale address is dead
		t.Cleanup(func() { h.rpcErr = map[string]error{} })
		h.seed(t, art, models.ArtifactNodePlacement{NodeID: "moved", NodeIP: "10.0.0.5"})
		if err := cleanupArtifactFully(ctx, art, "", ""); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if h.destroys["10.0.0.6"] != 1 || h.destroys["10.0.0.5"] != 0 {
			t.Fatalf("must use the live address, not the stale stored one: %v", h.destroys)
		}
		if h.notifies != 1 || len(h.placementIDs(t, art)) != 0 {
			t.Fatalf("artifact must finalise (notifies=%d placements=%v)", h.notifies, h.placementIDs(t, art))
		}
	})
}

func TestReconcileDeferredArtifactPlacementsMySQL(t *testing.T) {
	h := newPlacementCleanupHarness(t)
	ctx := context.Background()
	const art = "art-gone"

	// The artifact row is already deleted by TC; only the deferred row remains.
	reset := func(rows ...models.ArtifactNodePlacement) {
		h.destroys = map[string]int{}
		if err := h.db.Exec("DELETE FROM " + constants.ArtifactNodePlacementTableName).Error; err != nil {
			t.Fatal(err)
		}
		if err := h.db.Exec("DELETE FROM " + constants.RootfsArtifactTableName).Error; err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			rows[i].ArtifactID = art
			if err := h.db.Create(&rows[i]).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	deferred := models.ArtifactNodePlacement{NodeID: "back", NodeIP: "10.1.0.1", DeferredAt: time.Now().Unix()}

	t.Run("node still absent: row kept, no RPC", func(t *testing.T) {
		h.present = map[string]string{}
		reset(deferred)
		reconcileDeferredArtifactPlacements(ctx)
		if len(h.destroys) != 0 || len(h.placementIDs(t, art)) != 1 {
			t.Fatalf("absent node must be left alone (destroys=%v)", h.destroys)
		}
	})

	t.Run("node back: destroyed with its CURRENT address and row removed", func(t *testing.T) {
		h.present = map[string]string{"back": "10.1.9.9"} // re-registered under a new IP
		reset(deferred)
		reconcileDeferredArtifactPlacements(ctx)
		if h.destroys["10.1.9.9"] != 1 || h.destroys["10.1.0.1"] != 0 {
			t.Fatalf("must use the live address, not the stale stored one: %v", h.destroys)
		}
		if got := h.placementIDs(t, art); len(got) != 0 {
			t.Fatalf("row must be deleted after confirmed destroy, got %v", got)
		}
	})

	t.Run("destroy failure keeps the row", func(t *testing.T) {
		h.present = map[string]string{"back": "10.1.0.1"}
		h.rpcErr = map[string]error{"10.1.0.1": errors.New("boom")}
		t.Cleanup(func() { h.rpcErr = map[string]error{} })
		reset(deferred)
		reconcileDeferredArtifactPlacements(ctx)
		if got := h.placementIDs(t, art); len(got) != 1 {
			t.Fatalf("row must survive a failed destroy, got %v", got)
		}
	})

	t.Run("artifact resurrected by a rebuild: files untouched", func(t *testing.T) {
		h.present = map[string]string{"back": "10.1.0.1"}
		reset(deferred)
		if err := h.db.Create(&models.RootfsArtifact{ArtifactID: art, Status: ArtifactStatusBuilding}).Error; err != nil {
			t.Fatal(err)
		}
		reconcileDeferredArtifactPlacements(ctx)
		if len(h.destroys) != 0 {
			t.Fatalf("a live artifact must not be destroyed: %v", h.destroys)
		}
	})

	t.Run("non-deferred rows are never scanned", func(t *testing.T) {
		h.present = map[string]string{"live": "10.1.0.2"}
		reset(models.ArtifactNodePlacement{NodeID: "live", NodeIP: "10.1.0.2"})
		reconcileDeferredArtifactPlacements(ctx)
		if len(h.destroys) != 0 {
			t.Fatalf("live placement must not be touched: %v", h.destroys)
		}
	})

	// Regression: rows of permanently-gone nodes must not fill the LIMIT and
	// starve a newer row whose node came back; node_view filtering prevents it.
	t.Run("a flood of permanently-gone nodes does not starve one that came back", func(t *testing.T) {
		h.present = map[string]string{"reachable": "10.2.0.1"}
		rows := make([]models.ArtifactNodePlacement, 0, artifactGCMaxPerPass+1)
		oldest := time.Now().Add(-24 * time.Hour).Unix()
		for i := 0; i < artifactGCMaxPerPass; i++ {
			// Never present in h.present: these nodes are gone for good.
			rows = append(rows, models.ArtifactNodePlacement{
				NodeID: fmt.Sprintf("gone-%03d", i), NodeIP: "10.9.0.1", DeferredAt: oldest,
			})
		}
		// Deferred far later than the flood: an unfiltered asc scan never
		// reaches it while any never-deleted "gone" row remains.
		rows = append(rows, models.ArtifactNodePlacement{
			NodeID: "reachable", NodeIP: "10.2.0.1", DeferredAt: time.Now().Unix(),
		})
		reset(rows...)
		reconcileDeferredArtifactPlacements(ctx)
		if h.destroys["10.2.0.1"] != 1 {
			t.Fatalf("reachable node must converge despite %d permanently-gone rows, destroys=%v",
				artifactGCMaxPerPass, h.destroys)
		}
		if got := h.placementIDs(t, art); len(got) != artifactGCMaxPerPass {
			t.Fatalf("only the reachable row may be deleted, got %d remaining, want %d", len(got), artifactGCMaxPerPass)
		}
	})

	// Regression: in-view rows whose destroy keeps failing must yield the page
	// so a later row is reached on the next pass instead of starving behind them.
	t.Run("failing in-view rows do not starve a later row", func(t *testing.T) {
		resetDeferredScanCursor()
		h.present = map[string]string{"z-late": "10.3.0.2"}
		h.rpcErr = map[string]error{"10.3.0.1": errors.New("persistent rpc error")}
		t.Cleanup(func() { h.rpcErr = map[string]error{} })
		rows := make([]models.ArtifactNodePlacement, 0, artifactGCMaxPerPass+1)
		for i := 0; i < artifactGCMaxPerPass; i++ {
			id := fmt.Sprintf("stuck-%03d", i)
			h.present[id] = "10.3.0.1"
			rows = append(rows, models.ArtifactNodePlacement{NodeID: id, NodeIP: "10.3.0.1", DeferredAt: 1})
		}
		rows = append(rows, models.ArtifactNodePlacement{NodeID: "z-late", NodeIP: "10.3.0.2", DeferredAt: 2})
		reset(rows...)

		reconcileDeferredArtifactPlacements(ctx)
		if h.destroys["10.3.0.2"] != 0 || h.destroys["10.3.0.1"] != artifactGCMaxPerPass {
			t.Fatalf("first page must be the failing rows only, destroys=%v", h.destroys)
		}
		reconcileDeferredArtifactPlacements(ctx)
		if h.destroys["10.3.0.2"] != 1 {
			t.Fatalf("second pass must reach the later row, destroys=%v", h.destroys)
		}
		if got := h.placementIDs(t, art); len(got) != artifactGCMaxPerPass {
			t.Fatalf("only the later row may be deleted, got %d remaining", len(got))
		}
	})

	t.Run("a backlog of exactly one page wraps instead of idling a pass", func(t *testing.T) {
		resetDeferredScanCursor()
		h.present = map[string]string{}
		h.rpcErr = map[string]error{"10.4.0.1": errors.New("persistent rpc error")}
		t.Cleanup(func() { h.rpcErr = map[string]error{} })
		rows := make([]models.ArtifactNodePlacement, 0, artifactGCMaxPerPass)
		for i := 0; i < artifactGCMaxPerPass; i++ {
			id := fmt.Sprintf("full-%03d", i)
			h.present[id] = "10.4.0.1"
			rows = append(rows, models.ArtifactNodePlacement{NodeID: id, NodeIP: "10.4.0.1", DeferredAt: 1})
		}
		reset(rows...)

		reconcileDeferredArtifactPlacements(ctx)
		reconcileDeferredArtifactPlacements(ctx)
		if want := 2 * artifactGCMaxPerPass; h.destroys["10.4.0.1"] != want {
			t.Fatalf("every pass must retry the full page, destroys=%d want %d", h.destroys["10.4.0.1"], want)
		}
	})
}

func resetDeferredScanCursor() {
	deferredScanCursor.mu.Lock()
	defer deferredScanCursor.mu.Unlock()
	deferredScanCursor.artifactID, deferredScanCursor.nodeID = "", ""
}

func TestUpsertArtifactNodePlacementResetsDeferred(t *testing.T) {
	h := newPlacementCleanupHarness(t)
	if err := h.db.Exec("DELETE FROM " + constants.ArtifactNodePlacementTableName).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Create(&models.ArtifactNodePlacement{
		ArtifactID: "a", NodeID: "n", NodeIP: "10.0.0.1", DeferredAt: time.Now().Unix(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := upsertArtifactNodePlacement(context.Background(), "a", "n", "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	if h.deferredAt(t, "a", "n") != 0 {
		t.Fatal("re-distribution must make the row live again")
	}
}
