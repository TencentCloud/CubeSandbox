// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func stubReplicaRegistry(t *testing.T, authoritative bool, nodes map[string]string) {
	t.Helper()
	origLookup := lookupRegisteredNodeIP
	origAuth := cleanupNodeRegistryAuthoritative
	t.Cleanup(func() {
		lookupRegisteredNodeIP = origLookup
		cleanupNodeRegistryAuthoritative = origAuth
	})
	cleanupNodeRegistryAuthoritative = func() bool { return authoritative }
	lookupRegisteredNodeIP = func(nodeID string) (string, bool) {
		ip, ok := nodes[nodeID]
		return ip, ok
	}
}

func newUnregisteredViewTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.TemplateDefinition{},
		&models.TemplateReplica{},
		&models.SnapshotRecord{},
		&models.TemplateImageJob{},
	); err != nil {
		t.Fatalf("migrate view tables: %v", err)
	}
	oldDB := store.db
	store.db = db
	templateDefinitionCache.Flush()
	templateInfoCache.Flush()
	templateListCache.Flush()
	t.Cleanup(func() {
		store.db = oldDB
		templateDefinitionCache.Flush()
		templateInfoCache.Flush()
		templateListCache.Flush()
	})
	return db
}

func TestCleanupSkipsUnregisteredNodeAndCleansRegisteredOne(t *testing.T) {
	stubReplicaRegistry(t, true, map[string]string{
		"node-live": "10.1.0.8",
	})
	origCleanup := cleanupTemplateOnCubelet
	origAddr := getCubeletAddrForDelete
	t.Cleanup(func() {
		cleanupTemplateOnCubelet = origCleanup
		getCubeletAddrForDelete = origAddr
	})

	getCubeletAddrForDelete = func(hostIP string) string { return hostIP + ":9000" }
	var dialed []string
	cleanupTemplateOnCubelet = func(ctx context.Context, calleeEp string, req *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		dialed = append(dialed, calleeEp)
		if strings.Contains(calleeEp, "10.0.0.1") {
			t.Fatalf("dialed stored ip of the live node; want the registry address, ep=%s", calleeEp)
		}
		return &cubeboxv1.CleanupTemplateResponse{
			Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
		}, nil
	}

	err := cleanupTemplateReplicasWithLocators(context.Background(), "tpl-1965", []templateCleanupLocator{
		{NodeID: "node-live", NodeIP: "10.0.0.1"},
		{NodeID: "node-gone", NodeIP: "10.0.0.2"},
	}, "xfs")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(dialed) != 1 || dialed[0] != "10.1.0.8:9000" {
		t.Fatalf("dialed %v, want only the live node's current address", dialed)
	}
}

func TestCleanupKeepsIPOnlyLocatorRetryable(t *testing.T) {
	stubReplicaRegistry(t, true, map[string]string{
		"node-live": "10.1.0.8",
	})
	origCleanup := cleanupTemplateOnCubelet
	origAddr := getCubeletAddrForDelete
	t.Cleanup(func() {
		cleanupTemplateOnCubelet = origCleanup
		getCubeletAddrForDelete = origAddr
	})
	getCubeletAddrForDelete = func(hostIP string) string { return hostIP + ":9000" }
	dialed := 0
	cleanupTemplateOnCubelet = func(ctx context.Context, calleeEp string, req *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		dialed++
		return &cubeboxv1.CleanupTemplateResponse{
			Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
		}, nil
	}

	err := cleanupTemplateReplicasWithLocators(context.Background(), "tpl-ip-only", []templateCleanupLocator{
		{NodeIP: "10.0.0.9"},
		{NodeIP: "10.1.0.8"},
	}, "")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if dialed != 2 {
		t.Fatalf("dialed %d times, want both IP-only locators attempted", dialed)
	}
}

func TestCleanupStillFailsWhenRegisteredNodeTimesOut(t *testing.T) {
	stubReplicaRegistry(t, true, map[string]string{
		"node-live": "10.1.0.8",
	})
	origCleanup := cleanupTemplateOnCubelet
	origAddr := getCubeletAddrForDelete
	t.Cleanup(func() {
		cleanupTemplateOnCubelet = origCleanup
		getCubeletAddrForDelete = origAddr
	})
	getCubeletAddrForDelete = func(hostIP string) string { return hostIP + ":9000" }
	cleanupTemplateOnCubelet = func(ctx context.Context, calleeEp string, req *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		return nil, errors.New("context deadline exceeded")
	}

	err := cleanupTemplateReplicasWithLocators(context.Background(), "tpl-1965", []templateCleanupLocator{
		{NodeID: "node-live", NodeIP: "10.1.0.8"},
		{NodeID: "node-gone", NodeIP: "10.0.0.2"},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "node-live") {
		t.Fatalf("want registered-node failure, got %v", err)
	}
	if strings.Contains(err.Error(), "node-gone") {
		t.Fatalf("unregistered node must not fail the delete, got %v", err)
	}
}

func TestDeleteCleanupDefersRegisteredNodeAndDropsRetiredNode(t *testing.T) {
	stubReplicaRegistry(t, true, map[string]string{
		"node-live": "10.1.0.8",
	})
	origCleanup := cleanupTemplateOnCubelet
	origAddr := getCubeletAddrForDelete
	origPersist := persistReplicaCleanupForDelete
	origClear := clearReplicaCleanupForDelete
	t.Cleanup(func() {
		cleanupTemplateOnCubelet = origCleanup
		getCubeletAddrForDelete = origAddr
		persistReplicaCleanupForDelete = origPersist
		clearReplicaCleanupForDelete = origClear
	})

	getCubeletAddrForDelete = func(hostIP string) string { return hostIP + ":9000" }
	var dialed []string
	cleanupTemplateOnCubelet = func(_ context.Context, calleeEp string, _ *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		dialed = append(dialed, calleeEp)
		return nil, errors.New("context deadline exceeded")
	}
	var persisted, cleared []string
	persistReplicaCleanupForDelete = func(_ context.Context, _ string, locator templateCleanupLocator, cause error) error {
		persisted = append(persisted, locator.NodeID)
		if cause == nil || !strings.Contains(cause.Error(), "deadline") {
			t.Fatalf("unexpected cleanup cause: %v", cause)
		}
		return nil
	}
	clearReplicaCleanupForDelete = func(_ context.Context, _ string, locator templateCleanupLocator) error {
		cleared = append(cleared, locator.NodeID)
		return nil
	}

	err := cleanupTemplateReplicasForDelete(context.Background(), "tpl-1965", []templateCleanupLocator{
		{NodeID: "node-live", NodeIP: "10.0.0.1"},
		{NodeID: "node-gone", NodeIP: "10.0.0.2"},
	}, "xfs")
	if err != nil {
		t.Fatalf("delete cleanup: %v", err)
	}
	if len(dialed) != 1 || dialed[0] != "10.1.0.8:9000" {
		t.Fatalf("dialed %v, want only registered node at its live address", dialed)
	}
	if len(persisted) != 1 || persisted[0] != "node-live" {
		t.Fatalf("persisted %v, want transient node marker", persisted)
	}
	if len(cleared) != 1 || cleared[0] != "node-gone" {
		t.Fatalf("cleared %v, want retired node marker removed", cleared)
	}
}

func TestDeleteCleanupFailsWhenRetryMarkerCannotBePersisted(t *testing.T) {
	stubReplicaRegistry(t, true, map[string]string{
		"node-live": "10.1.0.8",
	})
	origCleanup := cleanupTemplateOnCubelet
	origAddr := getCubeletAddrForDelete
	origPersist := persistReplicaCleanupForDelete
	origClear := clearReplicaCleanupForDelete
	t.Cleanup(func() {
		cleanupTemplateOnCubelet = origCleanup
		getCubeletAddrForDelete = origAddr
		persistReplicaCleanupForDelete = origPersist
		clearReplicaCleanupForDelete = origClear
	})

	getCubeletAddrForDelete = func(hostIP string) string { return hostIP }
	cleanupTemplateOnCubelet = func(context.Context, string, *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		return nil, errors.New("rpc unavailable")
	}
	persistReplicaCleanupForDelete = func(context.Context, string, templateCleanupLocator, error) error {
		return errors.New("database unavailable")
	}
	clearReplicaCleanupForDelete = func(context.Context, string, templateCleanupLocator) error {
		return nil
	}

	err := cleanupTemplateReplicasForDelete(context.Background(), "tpl-1965",
		[]templateCleanupLocator{{NodeID: "node-live", NodeIP: "10.1.0.8"}}, "")
	if err == nil || !strings.Contains(err.Error(), "database unavailable") ||
		!strings.Contains(err.Error(), "rpc unavailable") {
		t.Fatalf("want RPC and durable-marker errors, got %v", err)
	}
}

func TestCleanupLocatorPersistenceNodeIDSynthesizesIPOnlyKey(t *testing.T) {
	first := cleanupLocatorPersistenceNodeID(templateCleanupLocator{NodeIP: "10.0.0.9"})
	second := cleanupLocatorPersistenceNodeID(templateCleanupLocator{NodeIP: "10.0.0.9"})
	if first == "" || first != second || !strings.HasPrefix(first, cleanupIPNodePrefix) {
		t.Fatalf("unexpected synthetic key: first=%q second=%q", first, second)
	}
	if unregisteredCleanupNode(first) {
		t.Fatal("synthetic IP-only cleanup key must never be classified as retired")
	}
}

func TestMarkReplicaCleanupRequiredPreservesExistingArtifactAndCreatesLegacyMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.TemplateReplica{},
		&models.TemplateDefinition{},
		&models.SnapshotRecord{},
	); err != nil {
		t.Fatalf("migrate cleanup tables: %v", err)
	}
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })

	existing := models.TemplateReplica{
		TemplateID: "tpl-existing",
		NodeID:     "node-live",
		NodeIP:     "10.0.0.1",
		ArtifactID: "rfs-shared",
		Status:     ReplicaStatusReady,
	}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatalf("create existing replica: %v", err)
	}
	if err := markReplicaCleanupRequired(context.Background(), "tpl-existing",
		templateCleanupLocator{NodeID: "node-live", NodeIP: "10.1.0.8"},
		errors.New("rpc timeout")); err != nil {
		t.Fatalf("mark existing replica: %v", err)
	}
	var got models.TemplateReplica
	if err := db.Where("template_id = ? AND node_id = ?", "tpl-existing", "node-live").
		First(&got).Error; err != nil {
		t.Fatalf("read existing marker: %v", err)
	}
	if !got.CleanupRequired || got.ArtifactID != "rfs-shared" ||
		got.NodeIP != "10.1.0.8" || !strings.Contains(got.ErrorMessage, "timeout") {
		t.Fatalf("existing marker = %+v", got)
	}

	legacy := templateCleanupLocator{NodeIP: "10.0.0.9"}
	if err := markReplicaCleanupRequired(context.Background(), "snap-legacy", legacy,
		errors.New("unreachable")); err != nil {
		t.Fatalf("create IP-only marker: %v", err)
	}
	var created models.TemplateReplica
	if err := db.Where("template_id = ?", "snap-legacy").First(&created).Error; err != nil {
		t.Fatalf("read IP-only marker: %v", err)
	}
	if !created.CleanupRequired || created.NodeIP != "10.0.0.9" ||
		!strings.HasPrefix(created.NodeID, cleanupIPNodePrefix) {
		t.Fatalf("IP-only marker = %+v", created)
	}
	if err := clearReplicaCleanupRequired(context.Background(), "snap-legacy", legacy); err != nil {
		t.Fatalf("clear IP-only marker: %v", err)
	}
	if err := db.Where("template_id = ?", "snap-legacy").First(&created).Error; err != nil {
		t.Fatalf("re-read IP-only marker: %v", err)
	}
	if created.CleanupRequired {
		t.Fatal("IP-only marker was not cleared")
	}

	if err := db.Create(&models.TemplateDefinition{TemplateID: "tpl-delete"}).Error; err != nil {
		t.Fatalf("create definition: %v", err)
	}
	if err := db.Create(&models.SnapshotRecord{SnapshotID: "tpl-delete"}).Error; err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := db.Create(&[]models.TemplateReplica{
		{TemplateID: "tpl-delete", NodeID: "node-retry", CleanupRequired: true},
		{TemplateID: "tpl-delete", NodeID: "node-done", CleanupRequired: false},
	}).Error; err != nil {
		t.Fatalf("create cleanup rows: %v", err)
	}
	if err := cleanupTemplateMetadata(context.Background(), "tpl-delete"); err != nil {
		t.Fatalf("cleanup template metadata: %v", err)
	}
	var retryRows, completedRows, definitions, snapshots int64
	db.Model(&models.TemplateReplica{}).
		Where("template_id = ? AND node_id = ?", "tpl-delete", "node-retry").Count(&retryRows)
	db.Unscoped().Model(&models.TemplateReplica{}).
		Where("template_id = ? AND node_id = ?", "tpl-delete", "node-done").Count(&completedRows)
	db.Unscoped().Model(&models.TemplateDefinition{}).
		Where("template_id = ?", "tpl-delete").Count(&definitions)
	db.Unscoped().Model(&models.SnapshotRecord{}).
		Where("snapshot_id = ?", "tpl-delete").Count(&snapshots)
	if retryRows != 1 || completedRows != 0 || definitions != 0 || snapshots != 0 {
		t.Fatalf("metadata cleanup counts retry=%d completed=%d definitions=%d snapshots=%d",
			retryRows, completedRows, definitions, snapshots)
	}
}

func TestCleanupKeepsDialingWhenRegistryIsNotReady(t *testing.T) {
	stubReplicaRegistry(t, false, map[string]string{})
	origCleanup := cleanupTemplateOnCubelet
	origAddr := getCubeletAddrForDelete
	t.Cleanup(func() {
		cleanupTemplateOnCubelet = origCleanup
		getCubeletAddrForDelete = origAddr
	})
	getCubeletAddrForDelete = func(hostIP string) string { return hostIP + ":9000" }
	var dialed []string
	cleanupTemplateOnCubelet = func(ctx context.Context, calleeEp string, req *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		dialed = append(dialed, calleeEp)
		return nil, errors.New("context deadline exceeded")
	}

	err := cleanupTemplateReplicasWithLocators(context.Background(), "tpl-cold", []templateCleanupLocator{
		{NodeID: "node-maybe", NodeIP: "10.0.0.4"},
	}, "")
	if err == nil {
		t.Fatal("uninitialized registry must not treat a missing node as gone")
	}
	if len(dialed) != 1 || dialed[0] != "10.0.0.4:9000" {
		t.Fatalf("dialed %v, want the stored address", dialed)
	}
}

func TestApplyUnregisteredReplicaView(t *testing.T) {
	replicas := []ReplicaStatus{
		{NodeID: "node-gone", NodeIP: "10.0.0.2", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady},
		{NodeID: "node-live", NodeIP: "10.1.0.8", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady},
	}

	stubReplicaRegistry(t, false, map[string]string{"node-live": "10.1.0.8"})
	status, lastError, got := applyUnregisteredReplicaView(StatusReady, "", replicas)
	if status != StatusReady || lastError != "" || got[0].Status != ReplicaStatusReady {
		t.Fatalf("uninitialized registry must leave the view alone, status=%s err=%s replica=%s", status, lastError, got[0].Status)
	}
	if replicas[0].Status != ReplicaStatusReady {
		t.Fatal("input slice was mutated")
	}

	stubReplicaRegistry(t, true, map[string]string{"node-live": "10.1.0.8"})
	status, lastError, got = applyUnregisteredReplicaView(StatusReady, "", replicas)
	if status != StatusPartiallyReady || lastError != unregisteredReplicaError {
		t.Fatalf("mixed replicas: status=%s err=%s", status, lastError)
	}
	if got[0].Status != ReplicaStatusFailed || got[0].ErrorMessage != unregisteredReplicaError {
		t.Fatalf("gone replica = %+v", got[0])
	}
	if got[1].Status != ReplicaStatusReady {
		t.Fatalf("live replica = %+v", got[1])
	}
	if replicas[0].Status != ReplicaStatusReady {
		t.Fatal("input slice was mutated")
	}

	onlyGone := []ReplicaStatus{{NodeID: "node-gone", Status: ReplicaStatusReady}}
	status, lastError, got = applyUnregisteredReplicaView(StatusReady, "", onlyGone)
	if status != StatusFailed || lastError != noRegisteredReadyReplicaError {
		t.Fatalf("all gone: status=%s err=%s", status, lastError)
	}
	if got[0].Status != ReplicaStatusFailed {
		t.Fatalf("gone replica status = %s", got[0].Status)
	}

	status, lastError, _ = applyUnregisteredReplicaView(StatusCreating, "still building", onlyGone)
	if status != StatusCreating || lastError != "still building" {
		t.Fatalf("non-servable status must be kept, status=%s err=%s", status, lastError)
	}

	ipOnly := []ReplicaStatus{{NodeIP: "10.0.0.9", Status: ReplicaStatusReady}}
	status, _, got = applyUnregisteredReplicaView(StatusReady, "", ipOnly)
	if status != StatusReady || got[0].Status != ReplicaStatusReady {
		t.Fatalf("ip-only locator must stay retryable: status=%s replica=%s", status, got[0].Status)
	}
}

func TestAdjustListedTemplateForUnregisteredReplicas(t *testing.T) {
	stubReplicaRegistry(t, true, map[string]string{"node-live": "10.1.0.8"})
	info := adjustListedTemplateForUnregisteredReplicas(TemplateInfo{
		TemplateID: "tpl-1965",
		Status:     StatusReady,
	}, map[string][]ReplicaStatus{
		"tpl-1965": {{NodeID: "node-gone", Status: ReplicaStatusReady}},
	})
	if info.Status != StatusFailed || info.LastError != noRegisteredReadyReplicaError {
		t.Fatalf("listed status=%s err=%s", info.Status, info.LastError)
	}

	untouched := adjustListedTemplateForUnregisteredReplicas(TemplateInfo{
		TemplateID: "tpl-no-rows",
		Status:     StatusReady,
	}, nil)
	if untouched.Status != StatusReady {
		t.Fatalf("template without replicas changed to %s", untouched.Status)
	}
}

func TestGetTemplateInfoCacheRevalidatesNodeRegistry(t *testing.T) {
	templateInfoCache.Flush()
	t.Cleanup(templateInfoCache.Flush)
	nodes := map[string]string{}
	stubReplicaRegistry(t, true, nodes)
	setTemplateInfoCache("tpl-cache", &TemplateInfo{
		TemplateID: "tpl-cache",
		Status:     StatusReady,
		Replicas: []ReplicaStatus{{
			NodeID: "node-a",
			Status: ReplicaStatusReady,
		}},
	})

	info, err := GetTemplateInfo(context.Background(), "tpl-cache")
	if err != nil {
		t.Fatalf("GetTemplateInfo retired node: %v", err)
	}
	if info.Status != StatusFailed {
		t.Fatalf("cached retired-node status=%s, want FAILED", info.Status)
	}

	nodes["node-a"] = "10.1.0.8"
	info, err = GetTemplateInfo(context.Background(), "tpl-cache")
	if err != nil {
		t.Fatalf("GetTemplateInfo re-registered node: %v", err)
	}
	if info.Status != StatusReady || info.Replicas[0].Status != ReplicaStatusReady {
		t.Fatalf("re-registered cached info=%+v, want raw READY restored", info)
	}
}

func TestListTemplatesCacheRevalidatesNodeRegistry(t *testing.T) {
	db := newUnregisteredViewTestDB(t)
	nodes := map[string]string{}
	stubReplicaRegistry(t, true, nodes)
	if err := db.Create(&models.TemplateReplica{
		TemplateID: "tpl-list-cache",
		NodeID:     "node-a",
		Status:     ReplicaStatusReady,
	}).Error; err != nil {
		t.Fatalf("create replica: %v", err)
	}
	setTemplateListCache([]TemplateInfo{{
		TemplateID: "tpl-list-cache",
		Status:     StatusReady,
	}})

	infos, err := ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates retired node: %v", err)
	}
	if len(infos) != 1 || infos[0].Status != StatusFailed {
		t.Fatalf("retired-node list=%+v, want FAILED", infos)
	}

	nodes["node-a"] = "10.1.0.8"
	infos, err = ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates re-registered node: %v", err)
	}
	if len(infos) != 1 || infos[0].Status != StatusReady {
		t.Fatalf("re-registered list=%+v, want raw READY restored", infos)
	}
}

func TestGetTemplateInfoJobFallbackChecksReplicaRegistry(t *testing.T) {
	db := newUnregisteredViewTestDB(t)
	stubReplicaRegistry(t, true, map[string]string{})
	if err := db.Create(&models.TemplateImageJob{
		JobID:          "job-only-ready",
		TemplateID:     "tpl-job-only",
		AttemptNo:      1,
		Operation:      JobOperationCreate,
		NodeID:         "node-gone",
		NodeIP:         "10.0.0.9",
		Status:         JobStatusReady,
		TemplateStatus: StatusReady,
	}).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}

	info, err := GetTemplateInfo(context.Background(), "tpl-job-only")
	if err != nil {
		t.Fatalf("GetTemplateInfo job fallback: %v", err)
	}
	if info.Status != StatusFailed || len(info.Replicas) != 1 ||
		info.Replicas[0].Status != ReplicaStatusFailed {
		t.Fatalf("job-only info=%+v, want retired replica projected FAILED", info)
	}

	setTemplateListCache([]TemplateInfo{{
		TemplateID: "tpl-job-only",
		Status:     StatusReady,
	}})
	infos, err := ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates job fallback: %v", err)
	}
	if len(infos) != 1 || infos[0].Status != StatusFailed {
		t.Fatalf("job-only list=%+v, want retired job locator projected FAILED", infos)
	}
}
