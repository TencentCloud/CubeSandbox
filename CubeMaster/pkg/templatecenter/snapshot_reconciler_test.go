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

func TestReconcileSnapshotReplicaPresenceDoesNotOverwriteTombstone(t *testing.T) {
	for _, raced := range []string{StatusDeleted, StatusDeleting} {
		t.Run(raced, func(t *testing.T) {
			casCalls := 0
			unguardedFailed := false
			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string {
				return hostIP
			})
			patches.ApplyFunc(updateSnapshotFieldsIfStatusIn, func(ctx context.Context, snapshotID string, values map[string]any, statuses ...string) (bool, error) {
				casCalls++
				if snapshotID != "snap-1" {
					t.Fatalf("snapshotID = %q", snapshotID)
				}
				if values["status"] != StatusFailed {
					t.Fatalf("CAS status = %v, want FAILED", values["status"])
				}
				if len(statuses) != 2 || statuses[0] != StatusReady || statuses[1] != StatusFailed {
					t.Fatalf("CAS allowed statuses = %v, want READY, FAILED", statuses)
				}
				return false, nil
			})
			patches.ApplyFunc(updateSnapshotFields, func(ctx context.Context, snapshotID string, values map[string]any) error {
				if values["status"] == StatusFailed {
					unguardedFailed = true
				}
				return nil
			})

			_ = stubReconcilerDB(t,
				models.SnapshotRecord{
					SnapshotID:   "snap-1",
					Status:       StatusReady,
					Backend:      constants.SnapshotBackendXFS,
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
			getLocalSnapshotOnCubelet = func(ctx context.Context, addr string,
				req *cubeboxv1.GetLocalSnapshotRequest) (*cubeboxv1.GetLocalSnapshotResponse, error) {
				return &cubeboxv1.GetLocalSnapshotResponse{
					Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
				}, nil
			}

			require.NoError(t, reconcileSnapshotReplicaPresence(context.Background()))
			require.Equal(t, 1, casCalls, "replica-presence must CAS FAILED against READY/FAILED")
			require.False(t, unguardedFailed, "must not write FAILED through unguarded updateSnapshotFields after a %s race", raced)
		})
	}
}

func staleDeleteJob(status string) *models.TemplateImageJob {
	job := &models.TemplateImageJob{
		JobID:     "job-stale-delete",
		Operation: JobOperationSnapshotDelete,
		Status:    status,
	}
	job.UpdatedAt = time.Now().Add(-snapshotOperationTimeout - time.Minute)
	return job
}

func TestShouldReclaimStaleSnapshotDeleteJob(t *testing.T) {
	deleting := models.SnapshotRecord{SnapshotID: "snap-1", Status: StatusDeleting}
	creating := models.SnapshotRecord{SnapshotID: "snap-1", Status: StatusCreating}

	if !shouldReclaimStaleSnapshotDeleteJob(deleting, staleDeleteJob(JobStatusPending)) {
		t.Fatal("DELETING + stale pending SNAPSHOT_DELETE must reclaim")
	}
	if !shouldReclaimStaleSnapshotDeleteJob(deleting, staleDeleteJob(JobStatusRunning)) {
		t.Fatal("DELETING + stale running SNAPSHOT_DELETE must reclaim")
	}

	fresh := staleDeleteJob(JobStatusPending)
	fresh.UpdatedAt = time.Now()
	if shouldReclaimStaleSnapshotDeleteJob(deleting, fresh) {
		t.Fatal("fresh delete job must not reclaim")
	}
	if shouldReclaimStaleSnapshotDeleteJob(creating, staleDeleteJob(JobStatusPending)) {
		t.Fatal("CREATING must not reclaim a stale job")
	}

	createJob := staleDeleteJob(JobStatusPending)
	createJob.Operation = JobOperationSnapshotCreate
	if shouldReclaimStaleSnapshotDeleteJob(deleting, createJob) {
		t.Fatal("non-delete job must not reclaim")
	}
	if shouldReclaimStaleSnapshotDeleteJob(deleting, nil) {
		t.Fatal("nil job must not reclaim")
	}
}

func TestShouldFailStaleDeleteJobOnTombstone(t *testing.T) {
	deleted := models.SnapshotRecord{SnapshotID: "snap-1", Status: StatusDeleted}
	deleting := models.SnapshotRecord{SnapshotID: "snap-1", Status: StatusDeleting}

	if !shouldFailStaleDeleteJobOnTombstone(deleted, staleDeleteJob(JobStatusPending)) {
		t.Fatal("DELETED + stale pending SNAPSHOT_DELETE must fail the job")
	}
	if shouldFailStaleDeleteJobOnTombstone(deleting, staleDeleteJob(JobStatusPending)) {
		t.Fatal("DELETING is handled by the timeout reclaim path, not this helper")
	}
	fresh := staleDeleteJob(JobStatusRunning)
	fresh.UpdatedAt = time.Now()
	if shouldFailStaleDeleteJobOnTombstone(deleted, fresh) {
		t.Fatal("fresh delete job on a tombstone must not be failed")
	}
}

func TestFailStaleDeleteJobOnTombstoneMarksJobFailed(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	job := staleDeleteJob(JobStatusPending)
	var failed map[string]any
	patches.ApplyFunc(getActiveSnapshotJobByResourceID, func(ctx context.Context, resourceID string) (*models.TemplateImageJob, error) {
		return job, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		failed = values
		return nil
	})

	err := failStaleDeleteJobOnTombstone(context.Background(), models.SnapshotRecord{
		SnapshotID: "snap-split",
		Status:     StatusDeleted,
	})
	require.NoError(t, err)
	require.Equal(t, JobStatusFailed, failed["status"])
	require.Contains(t, failed["error_message"], "on tombstone")
}

func TestReclaimTimedOutSnapshotRecordStaleDeleteJob(t *testing.T) {
	for _, jobStatus := range []string{JobStatusPending, JobStatusRunning} {
		t.Run(jobStatus, func(t *testing.T) {
			patches := gomonkey.NewPatches()
			defer patches.Reset()

			job := staleDeleteJob(jobStatus)
			var failedJob map[string]any
			var snapFields map[string]any
			patches.ApplyFunc(getActiveSnapshotJobByResourceID, func(ctx context.Context, resourceID string) (*models.TemplateImageJob, error) {
				return job, nil
			})
			patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
				failedJob = values
				return nil
			})
			patches.ApplyFunc(updateSnapshotFields, func(ctx context.Context, snapshotID string, values map[string]any) error {
				snapFields = values
				return nil
			})

			err := reclaimTimedOutSnapshotRecord(context.Background(), models.SnapshotRecord{
				SnapshotID: "snap-stuck",
				Status:     StatusDeleting,
			})
			require.NoError(t, err)
			require.Equal(t, JobStatusFailed, failedJob["status"])
			require.Equal(t, StatusDeleted, snapFields["status"])
			require.Contains(t, snapFields["last_error"], "stale delete job")
		})
	}
}

func TestReclaimTimedOutSnapshotRecordSkipsFreshDeleteJob(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	job := staleDeleteJob(JobStatusPending)
	job.UpdatedAt = time.Now()
	patches.ApplyFunc(getActiveSnapshotJobByResourceID, func(ctx context.Context, resourceID string) (*models.TemplateImageJob, error) {
		return job, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		t.Fatal("must not fail a fresh delete job")
		return nil
	})
	patches.ApplyFunc(updateSnapshotFields, func(ctx context.Context, snapshotID string, values map[string]any) error {
		t.Fatal("must not rewrite snapshot while a fresh delete job is active")
		return nil
	})

	err := reclaimTimedOutSnapshotRecord(context.Background(), models.SnapshotRecord{
		SnapshotID: "snap-live",
		Status:     StatusDeleting,
	})
	require.NoError(t, err)
}

func TestReclaimTimedOutSnapshotRecordSkipsCreatingWithStaleJob(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(getActiveSnapshotJobByResourceID, func(ctx context.Context, resourceID string) (*models.TemplateImageJob, error) {
		return staleDeleteJob(JobStatusPending), nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		t.Fatal("CREATING must not fail a stale job")
		return nil
	})
	patches.ApplyFunc(updateSnapshotFields, func(ctx context.Context, snapshotID string, values map[string]any) error {
		t.Fatal("CREATING + active job must keep the existing skip path")
		return nil
	})

	err := reclaimTimedOutSnapshotRecord(context.Background(), models.SnapshotRecord{
		SnapshotID: "snap-creating",
		Status:     StatusCreating,
	})
	require.NoError(t, err)
}

func TestReclaimTimedOutSnapshotRecordNoJob(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var snapFields map[string]any
	patches.ApplyFunc(getActiveSnapshotJobByResourceID, func(ctx context.Context, resourceID string) (*models.TemplateImageJob, error) {
		return nil, gorm.ErrRecordNotFound
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		t.Fatal("must not touch a job when none is active")
		return nil
	})
	patches.ApplyFunc(updateSnapshotFields, func(ctx context.Context, snapshotID string, values map[string]any) error {
		snapFields = values
		return nil
	})

	err := reclaimTimedOutSnapshotRecord(context.Background(), models.SnapshotRecord{
		SnapshotID: "snap-no-job",
		Status:     StatusDeleting,
	})
	require.NoError(t, err)
	require.Equal(t, StatusDeleted, snapFields["status"])
}
