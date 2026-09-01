// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// resetSnapshotStorageCachesForTest clears the in-process snapshotStorageCache
// and localcache.snapshotStorageStateCache so that adjacent tests do not pollute each other.
func resetSnapshotStorageCachesForTest(t *testing.T) {
	t.Helper()
	snapshotStorageCache.Lock()
	snapshotStorageCache.byNode = make(map[string]snapshotStorageState)
	snapshotStorageCache.Unlock()

	// localcache.SnapshotStorageState is another in-process copy and must also
	// be cleared, otherwise ListSnapshotStorageStates would return stale data.
	localcache.ResetSnapshotStorageStateCacheForTest()
}

// healthyMetrics returns a metrics map that matches requiredSnapshotMetricKeys
// and yields snapshotStorageModeHealthy via classifySnapshotStorageMode.
// 300/1024 ≈ 29% which is well under snapshotWarnThreshold (70%).
func healthyMetrics() map[string]uint64 {
	return map[string]uint64{
		"total_bytes":    1024,
		"used_bytes":     300,
		"volume_count":   2,
		"snapshot_count": 1,
	}
}

// TestGetOrRefreshSkipsTTLForUnknown verifies that entries in unknown state
// trigger a synchronous refresh even when still within TTL. This prevents
// transient cold-start failures from blocking createSnapshot for an entire TTL window.
func TestGetOrRefreshSkipsTTLForUnknown(t *testing.T) {
	resetSnapshotStorageCachesForTest(t)

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string {
		return hostIP
	})

	var calls int32
	patches.ApplyFunc(cubelet.GetStorageMetrics, func(ctx context.Context, addr string,
		req *cubeboxv1.GetStorageMetricsRequest) (*cubeboxv1.GetStorageMetricsResponse, error) {
		atomic.AddInt32(&calls, 1)
		return &cubeboxv1.GetStorageMetricsResponse{
			Ret:     &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
			NodeId:  "node-1",
			Metrics: healthyMetrics(),
		}, nil
	})

	// Seed an unknown entry within TTL: under the old short-circuit logic,
	// GetStorageMetrics would not be called and Mode would never flip to healthy.
	cacheSnapshotStorageState("node-1", "10.0.0.1", snapshotStorageState{
		NodeID:        "node-1",
		NodeIP:        "10.0.0.1",
		Mode:          snapshotStorageModeUnknown,
		LastError:     "boot race",
		LastUpdatedAt: time.Now(),
	})

	state, err := getOrRefreshSnapshotStorageState(context.Background(), "node-1", "10.0.0.1")
	if err != nil {
		t.Fatalf("getOrRefreshSnapshotStorageState returned error: %v", err)
	}
	if state.Mode != snapshotStorageModeHealthy {
		t.Fatalf("expected healthy after refresh, got %q (lastError=%q)", state.Mode, state.LastError)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 GetStorageMetrics call, got %d", got)
	}
}

// TestGetOrRefreshHonoursTTLForHealthy verifies the inverse: healthy entries still
// respect TTL, so the new behaviour does not introduce unexpected extra RPCs.
func TestGetOrRefreshHonoursTTLForHealthy(t *testing.T) {
	resetSnapshotStorageCachesForTest(t)

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var calls int32
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP })
	patches.ApplyFunc(cubelet.GetStorageMetrics, func(ctx context.Context, addr string,
		req *cubeboxv1.GetStorageMetricsRequest) (*cubeboxv1.GetStorageMetricsResponse, error) {
		atomic.AddInt32(&calls, 1)
		return &cubeboxv1.GetStorageMetricsResponse{
			Ret:     &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
			Metrics: healthyMetrics(),
		}, nil
	})

	cacheSnapshotStorageState("node-1", "10.0.0.1", snapshotStorageState{
		NodeID:        "node-1",
		NodeIP:        "10.0.0.1",
		Mode:          snapshotStorageModeHealthy,
		LastUpdatedAt: time.Now(),
	})

	state, err := getOrRefreshSnapshotStorageState(context.Background(), "node-1", "10.0.0.1")
	if err != nil {
		t.Fatalf("getOrRefreshSnapshotStorageState returned error: %v", err)
	}
	if state.Mode != snapshotStorageModeHealthy {
		t.Fatalf("expected healthy from cache, got %q", state.Mode)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("expected no GetStorageMetrics call (cache hit), got %d", got)
	}
}

// TestRefreshSnapshotStorageMetricsRetriesCachedUnknown verifies that when the
// healthy node list is temporarily empty (e.g. redis node states not yet reported),
// the reconciler still retries unknown nodes from localcache, preventing cold-start
// residue from being stuck indefinitely.
func TestRefreshSnapshotStorageMetricsRetriesCachedUnknown(t *testing.T) {
	resetSnapshotStorageCachesForTest(t)

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	originalGetNodes := getSnapshotReconcilerNodes
	getSnapshotReconcilerNodes = func(n int, product string) node.NodeList {
		return nil
	}
	t.Cleanup(func() { getSnapshotReconcilerNodes = originalGetNodes })
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string { return hostIP })

	type call struct {
		addr string
	}
	var calls []call
	patches.ApplyFunc(cubelet.GetStorageMetrics, func(ctx context.Context, addr string,
		req *cubeboxv1.GetStorageMetricsRequest) (*cubeboxv1.GetStorageMetricsResponse, error) {
		calls = append(calls, call{addr: addr})
		return &cubeboxv1.GetStorageMetricsResponse{
			Ret:     &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
			NodeId:  "stale-node",
			Metrics: healthyMetrics(),
		}, nil
	})

	// Write an unknown node directly into localcache (simulating dirty state left
	// from a previous cold-start failure; snapshotStorageCache retains a copy too).
	cacheSnapshotStorageState("stale-node", "10.0.0.7", snapshotStorageState{
		NodeID:        "stale-node",
		NodeIP:        "10.0.0.7",
		Mode:          snapshotStorageModeUnknown,
		LastError:     "boot race",
		LastUpdatedAt: time.Now(),
	})

	if err := refreshSnapshotStorageMetrics(context.Background()); err != nil {
		t.Fatalf("refreshSnapshotStorageMetrics returned error: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 GetStorageMetrics call to retry cached unknown node, got %d", len(calls))
	}
	if calls[0].addr != "10.0.0.7" {
		t.Fatalf("expected call to be routed to 10.0.0.7, got %q", calls[0].addr)
	}

	cached, ok := localcache.GetSnapshotStorageState("stale-node", "10.0.0.7")
	if !ok {
		t.Fatalf("expected cached entry to be present after refresh")
	}
	if cached.Mode != string(snapshotStorageModeHealthy) {
		t.Fatalf("expected cached mode to be healthy after refresh, got %q (lastError=%q)", cached.Mode, cached.LastError)
	}
}

// stubReconcilerDB drives reconcileSnapshotReplicaPresence without a live
// database. The repo has no in-memory driver (see TestGetTemplateByAlias...),
// so a DryRun gorm.DB serves the snapshot row and its replica from memory via
// a replaced query callback, and records every UPDATE instead of running it.
func stubReconcilerDB(t *testing.T, rec models.SnapshotRecord, replica models.TemplateReplica) *[]string {
	t.Helper()
	sqlDB, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3306)/unused?parseTime=true")
	require.NoError(t, err)
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	require.NoError(t, err)

	require.NoError(t, db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *[]models.SnapshotRecord:
			*dest = []models.SnapshotRecord{rec}
		case *[]models.TemplateReplica:
			*dest = []models.TemplateReplica{replica}
		}
	}))
	updatedTables := make([]string, 0, 2)
	require.NoError(t, db.Callback().Update().Replace("gorm:update", func(tx *gorm.DB) {
		updatedTables = append(updatedTables, tx.Statement.Table)
	}))

	old := store.db
	store.db = db
	t.Cleanup(func() { store.db = old })
	return &updatedTables
}

// TestReconcileSnapshotReplicaPresenceSendsBackend pins the snapshot's backend
// onto the catalog probe. Cubelet reads one backend namespace per call and
// resolves an empty value to xfs, so an s3 snapshot probed without it comes
// back PreConditionFailed: the reconciler then treats a healthy snapshot as an
// orphan, stamps it FAILED and re-runs CleanupTemplate on every pass.
func TestReconcileSnapshotReplicaPresenceSendsBackend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend string
		want    string
	}{
		{name: "xfs", backend: constants.SnapshotBackendXFS, want: constants.SnapshotBackendXFS},
		{name: "historical cubecow stays empty", backend: "cubecow", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string {
				return hostIP
			})

			updated := stubReconcilerDB(t,
				models.SnapshotRecord{
					SnapshotID:   "snap-1",
					Status:       StatusReady,
					Backend:      tc.backend,
					InstanceType: "cubebox",
					OriginNodeID: "node-a",
					OriginNodeIP: "10.0.0.8",
				},
				models.TemplateReplica{
					TemplateID:   "snap-1",
					NodeID:       "node-a",
					NodeIP:       "10.0.0.8",
					InstanceType: "cubebox",
					Status:       ReplicaStatusReady,
					Phase:        ReplicaPhaseReady,
				})

			origProbe := getLocalSnapshotOnCubelet
			t.Cleanup(func() { getLocalSnapshotOnCubelet = origProbe })
			gotBackend := ""
			probes := 0
			getLocalSnapshotOnCubelet = func(ctx context.Context, addr string,
				req *cubeboxv1.GetLocalSnapshotRequest) (*cubeboxv1.GetLocalSnapshotResponse, error) {
				probes++
				gotBackend = req.GetBackend()
				return &cubeboxv1.GetLocalSnapshotResponse{
					Ret:      &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
					Snapshot: &cubeboxv1.LocalSnapshotInfo{SnapshotID: req.GetSnapshotID()},
				}, nil
			}

			require.NoError(t, reconcileSnapshotReplicaPresence(context.Background()))
			require.Equal(t, 1, probes, "the replica must be probed exactly once")
			require.Equal(t, tc.want, gotBackend)
			require.Empty(t, *updated, "a snapshot the node reports as present must not be stamped FAILED")
		})
	}
}

// TestReconcileSnapshotReplicaPresenceSkipsS3: S3 existence is remote_status,
// not a per-node catalog probe. Probing (and CleanupTemplate) would treat a
// normal Finalize unmount as an orphan and delete cluster-shared objects.
func TestReconcileSnapshotReplicaPresenceSkipsS3(t *testing.T) {
	updated := stubReconcilerDB(t,
		models.SnapshotRecord{
			SnapshotID:   "snap-s3",
			Status:       StatusReady,
			Backend:      constants.SnapshotBackendS3,
			InstanceType: "cubebox",
			OriginNodeID: "node-a",
			OriginNodeIP: "10.0.0.8",
		},
		models.TemplateReplica{
			TemplateID:   "snap-s3",
			NodeID:       "node-a",
			NodeIP:       "10.0.0.8",
			InstanceType: "cubebox",
			Status:       ReplicaStatusReady,
			Phase:        ReplicaPhaseReady,
		})

	origProbe := getLocalSnapshotOnCubelet
	t.Cleanup(func() { getLocalSnapshotOnCubelet = origProbe })
	probes := 0
	getLocalSnapshotOnCubelet = func(ctx context.Context, addr string,
		req *cubeboxv1.GetLocalSnapshotRequest) (*cubeboxv1.GetLocalSnapshotResponse, error) {
		probes++
		t.Fatalf("S3 snapshot must not be catalog-probed, got backend=%q snapshot=%q", req.GetBackend(), req.GetSnapshotID())
		return nil, nil
	}

	require.NoError(t, reconcileSnapshotReplicaPresence(context.Background()))
	require.Equal(t, 0, probes)
	require.Empty(t, *updated)
}
