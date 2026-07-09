// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

func TestBuildSnapshotRequestsUsesSnapshotID(t *testing.T) {
	req := &sandboxtypes.CreateCubeSandboxReq{
		Request: &sandboxtypes.Request{RequestID: "req-1"},
		Annotations: map[string]string{
			constants.CubeAnnotationsSystemDiskSize: "20",
		},
	}

	createReq, storedReq, err := buildSnapshotRequests(req, "snap-123")
	if err != nil {
		t.Fatalf("buildSnapshotRequests returned error: %v", err)
	}
	for _, item := range []*sandboxtypes.CreateCubeSandboxReq{createReq, storedReq} {
		if got := item.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]; got != "snap-123" {
			t.Fatalf("snapshot annotation = %q, want snap-123", got)
		}
		if got := constants.GetAppSnapshotVersion(item.Annotations); got != DefaultTemplateVersion {
			t.Fatalf("snapshot version = %q, want %q", got, DefaultTemplateVersion)
		}
	}
}

func TestSubmitSandboxSnapshotReusesExistingRequest(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	origLoad := loadSandboxCreateRequestFn
	defer func() { loadSandboxCreateRequestFn = origLoad }()
	loadSandboxCreateRequestFn = func(ctx context.Context, sandboxID string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return &sandboxtypes.CreateCubeSandboxReq{
			InstanceType: "cubebox",
			Annotations:  map[string]string{},
		}, nil
	}
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       "job-existing",
			Operation:   JobOperationSnapshotCreate,
			RequestJSON: `{"request_id":"req-existing","sandbox_id":"sb-1","node_id":"node-1","node_ip":"10.0.0.1","display_name":"snap"}`,
		}, nil
	})
	patches.ApplyFunc(GetTemplateImageJobInfo, func(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		return &sandboxtypes.TemplateImageJobInfo{
			JobID:      jobID,
			TemplateID: "snap-existing",
			Status:     JobStatusReady,
		}, nil
	})
	patches.ApplyFunc(snapshotCreateRequestMatches, func(_, _, _, _, _, _ string, _ *sandboxtypes.CreateCubeSandboxReq) bool {
		return true
	})

	info, err := SubmitSandboxSnapshot(context.Background(), "req-existing", "sb-1", "node-1", "10.0.0.1", "snap")
	if err != nil {
		t.Fatalf("SubmitSandboxSnapshot returned error: %v", err)
	}
	if info == nil || info.JobID != "job-existing" {
		t.Fatalf("unexpected job info: %#v", info)
	}
}

func TestSubmitSandboxSnapshotResumesPendingExistingRequest(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	ran := false
	infoCalls := 0

	origLoad := loadSandboxCreateRequestFn
	defer func() { loadSandboxCreateRequestFn = origLoad }()
	loadSandboxCreateRequestFn = func(ctx context.Context, sandboxID string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return &sandboxtypes.CreateCubeSandboxReq{
			InstanceType: "cubebox",
			Annotations:  map[string]string{},
		}, nil
	}
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:     "job-pending",
			Operation: JobOperationSnapshotCreate,
		}, nil
	})
	patches.ApplyFunc(snapshotCreateRequestMatches, func(_, _, _, _, _, _ string, _ *sandboxtypes.CreateCubeSandboxReq) bool {
		return true
	})
	patches.ApplyFunc(GetTemplateImageJobInfo, func(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		infoCalls++
		status := JobStatusPending
		if infoCalls > 1 {
			status = JobStatusReady
		}
		return &sandboxtypes.TemplateImageJobInfo{
			JobID:      jobID,
			RequestID:  "req-pending",
			TemplateID: "snap-existing",
			Status:     status,
		}, nil
	})
	patches.ApplyFunc(claimSnapshotJobExecution, func(ctx context.Context, jobID, phase string, progress int32) (bool, error) {
		return true, nil
	})
	patches.ApplyFunc(runSnapshotCreateJob, func(ctx context.Context, jobID, sandboxID, nodeID, nodeIP string, createReq, storedReq *sandboxtypes.CreateCubeSandboxReq) error {
		ran = true
		return nil
	})

	info, err := SubmitSandboxSnapshot(context.Background(), "req-pending", "sb-1", "node-1", "10.0.0.1", "snap")
	if err != nil {
		t.Fatalf("expected pending existing request to resume, got %v", err)
	}
	if !ran {
		t.Fatal("expected pending existing request to execute snapshot job")
	}
	if info == nil || info.Status != JobStatusReady {
		t.Fatalf("unexpected job info: %#v", info)
	}
}

func TestSubmitSandboxSnapshotReturnsStoredFailureForExistingRequest(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	origLoad := loadSandboxCreateRequestFn
	defer func() { loadSandboxCreateRequestFn = origLoad }()
	loadSandboxCreateRequestFn = func(ctx context.Context, sandboxID string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return &sandboxtypes.CreateCubeSandboxReq{
			InstanceType: "cubebox",
			Annotations:  map[string]string{},
		}, nil
	}
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:     "job-failed",
			Operation: JobOperationSnapshotCreate,
		}, nil
	})
	patches.ApplyFunc(snapshotCreateRequestMatches, func(_, _, _, _, _, _ string, _ *sandboxtypes.CreateCubeSandboxReq) bool {
		return true
	})
	patches.ApplyFunc(GetTemplateImageJobInfo, func(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		return &sandboxtypes.TemplateImageJobInfo{
			JobID:        jobID,
			RequestID:    "req-failed",
			TemplateID:   "snap-existing",
			Status:       JobStatusFailed,
			ErrorMessage: "snapshot create failed",
		}, nil
	})

	_, err := SubmitSandboxSnapshot(context.Background(), "req-failed", "sb-1", "node-1", "10.0.0.1", "snap")
	if err == nil {
		t.Fatal("expected stored failure for existing request")
	}
	if err.Error() != "snapshot create failed" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestFinalizeSynchronousSnapshotJobEnforcesTerminalContract pins down the
// gate that makes CubeMaster's snapshot create / rollback / *delete*
// synchronous: the finalizer accepts only `READY` (success) or `FAILED`
// (typed error), and converts every non-terminal status into a synthetic
// error.  This is the load-bearing invariant that lets CubeAPI / external
// SDKs treat `DeleteSnapshot` as a synchronous RPC — see
// SDKs treat `DeleteSnapshot` as a synchronous RPC — CubeAPI waits for
// a terminal state and does not expose a polling interface.  If a future refactor
// would start observing "the API said success but the snapshot is still
// being deleted" races; this test fails first.
func TestFinalizeSynchronousSnapshotJobEnforcesTerminalContract(t *testing.T) {
	cases := []struct {
		name        string
		info        *sandboxtypes.TemplateImageJobInfo
		wantNilInfo bool
		wantErr     bool
		errContains string
	}{
		{
			name:    "nil info is rejected",
			info:    nil,
			wantErr: true,
		},
		{
			name:        "ready returns the info verbatim",
			info:        &sandboxtypes.TemplateImageJobInfo{JobID: "job-ready", Status: JobStatusReady},
			wantNilInfo: false,
			wantErr:     false,
		},
		{
			name:        "lower-case ready is normalised",
			info:        &sandboxtypes.TemplateImageJobInfo{JobID: "job-ready-lc", Status: strings.ToLower(JobStatusReady)},
			wantNilInfo: false,
			wantErr:     false,
		},
		{
			name:        "failed is converted to a typed error",
			info:        &sandboxtypes.TemplateImageJobInfo{JobID: "job-failed", Status: JobStatusFailed, ErrorMessage: "cubelet rejected"},
			wantErr:     true,
			errContains: "cubelet rejected",
		},
		{
			name:        "pending leaks would break the synchronous contract",
			info:        &sandboxtypes.TemplateImageJobInfo{JobID: "job-pending", Status: JobStatusPending},
			wantErr:     true,
			errContains: "did not reach terminal status synchronously",
		},
		{
			name:        "running leaks would break the synchronous contract",
			info:        &sandboxtypes.TemplateImageJobInfo{JobID: "job-running", Status: JobStatusRunning},
			wantErr:     true,
			errContains: "did not reach terminal status synchronously",
		},
		{
			name:        "blank status is treated as non-terminal",
			info:        &sandboxtypes.TemplateImageJobInfo{JobID: "job-blank", Status: "  "},
			wantErr:     true,
			errContains: "did not reach terminal status synchronously",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := finalizeSynchronousSnapshotJob(tc.info)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q, got info=%+v", tc.name, got)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.name, err)
			}
			if tc.wantNilInfo {
				if got != nil {
					t.Fatalf("expected nil info, got %+v", got)
				}
				return
			}
			if got == nil || got.JobID != tc.info.JobID {
				t.Fatalf("expected info passthrough for %q, got %+v", tc.name, got)
			}
		})
	}
}

func TestSynchronousSnapshotJobContextDetachesFromParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	ctx, cancel := synchronousSnapshotJobContext(parent, "snapshot_create", map[string]any{"job_id": "job-1"})
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatalf("snapshot execution context should not inherit parent cancellation: %v", ctx.Err())
	default:
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("snapshot execution context should set a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > snapshotOperationTimeout {
		t.Fatalf("unexpected snapshot execution deadline remaining: %s", remaining)
	}
}

func TestParseSystemDiskSizeBytes(t *testing.T) {
	req := &sandboxtypes.CreateCubeSandboxReq{
		Annotations: map[string]string{
			constants.CubeAnnotationsSystemDiskSize: "20",
		},
	}
	if got := parseSystemDiskSizeBytes(req); got != 20<<30 {
		t.Fatalf("parseSystemDiskSizeBytes=%d, want %d", got, uint64(20<<30))
	}
}

func TestClassifySnapshotStorageMode(t *testing.T) {
	cases := []struct {
		name string
		used uint64
		want snapshotStorageMode
	}{
		{name: "healthy", used: 60, want: snapshotStorageModeHealthy},
		{name: "warn", used: 72, want: snapshotStorageModeWarn},
		{name: "reject", used: 86, want: snapshotStorageModeReject},
		{name: "delete-only", used: 95, want: snapshotStorageModeDeleteOnly},
	}
	for _, tc := range cases {
		if got := classifySnapshotStorageMode(tc.used); got != tc.want {
			t.Fatalf("%s: classifySnapshotStorageMode(%d)=%s, want %s", tc.name, tc.used, got, tc.want)
		}
	}
}

func TestDerivedUsagePct(t *testing.T) {
	cases := []struct {
		name    string
		metrics map[string]uint64
		want    uint64
	}{
		{name: "healthy", metrics: map[string]uint64{"total_bytes": 1000, "used_bytes": 250}, want: 25},
		{name: "full", metrics: map[string]uint64{"total_bytes": 1000, "used_bytes": 1000}, want: 100},
		{name: "over-reported caps at 100", metrics: map[string]uint64{"total_bytes": 1000, "used_bytes": 1500}, want: 100},
		{name: "missing total", metrics: map[string]uint64{"used_bytes": 50}, want: 0},
		{name: "zero total", metrics: map[string]uint64{"total_bytes": 0, "used_bytes": 50}, want: 0},
	}
	for _, tc := range cases {
		if got := derivedUsagePct(tc.metrics); got != tc.want {
			t.Fatalf("%s: derivedUsagePct=%d, want %d", tc.name, got, tc.want)
		}
	}
}

// After Phase 3 master only requires node identity on the replica row; the
// physical references (rootfs/memory vol, meta_dir) live in cubelet's local
// snapshot catalog. The validator therefore only rejects rows without a way
// to address the node.
func TestValidateSnapshotReadyReplicaRequiresNodeIdentity(t *testing.T) {
	if err := validateSnapshotReadyReplica(ReplicaStatus{}); err == nil {
		t.Fatal("expected validateSnapshotReadyReplica to reject empty replica")
	}
	if err := validateSnapshotReadyReplica(ReplicaStatus{NodeID: "node-a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := validateSnapshotReadyReplica(ReplicaStatus{NodeIP: "10.0.0.1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// After Phase 3 the snapshot replica row is intentionally thin: only the
// node identity and lifecycle bookkeeping survive. Physical references such
// as rootfs_vol / memory_vol / meta_dir / snapshot_path are no longer
// persisted on master; cubelet's local catalog is the source of truth and is
// queried lazily at rollback/cleanup/reconcile time. This test pins that
// contract so an accidental regression that re-adds physical writes here is
// caught immediately.
func TestRunSnapshotCreateJobWritesThinReplica(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var upserted ReplicaStatus
	var upsertCalled bool
	var readyTemplate bool
	var readyJob bool

	patches.ApplyFunc(cubelet.CommitSandbox, func(ctx context.Context, calleeEp string, req *cubeboxv1.CommitSandboxRequest) (*cubeboxv1.CommitSandboxResponse, error) {
		return &cubeboxv1.CommitSandboxResponse{
			Ret:          &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
			SnapshotPath: "/data/snapshots/snap-1",
			RootfsVol:    "tpl-snap-1-rootfs",
			MemoryVol:    "tpl-snap-1-memory",
			RootfsKind:   "snapshot",
			MemoryKind:   "volume",
		}, nil
	})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(hostIP string) string {
		return hostIP
	})
	patches.ApplyFunc(UpsertReplica, func(ctx context.Context, templateID, instanceType string, replica ReplicaStatus) error {
		upsertCalled = true
		upserted = replica
		return nil
	})
	patches.ApplyFunc(updateDefinitionFields, func(ctx context.Context, templateID string, fields map[string]any) error {
		if status, ok := fields["status"].(string); ok && status == StatusReady {
			readyTemplate = true
		}
		return nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, fields map[string]any) error {
		if status, ok := fields["status"].(string); ok && status == JobStatusReady {
			readyJob = true
		}
		return nil
	})
	patches.ApplyFunc(setTemplateLocalityCache, func(templateID string, replicas []ReplicaStatus) {})
	patches.ApplyFunc(setTemplateRequestCache, func(templateID string, req *sandboxtypes.CreateCubeSandboxReq) error { return nil })
	// localcache.RegisterTemplateReplica is a one-line wrapper that gomonkey
	// cannot patch reliably (it may inline). Skip its real implementation by
	// short-circuiting via package init: defer-restored later.
	patches.ApplyFunc(registerTemplateReplicaForSnapshot, func(templateID, nodeID string, sizeBytes int64) {})

	if err := runSnapshotCreateJob(context.Background(), "job-1", "sb-1", "node-a", "10.0.0.1", &sandboxtypes.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID: "snap-1",
		},
		SnapshotDir: "/data/snapshots/snap-1",
	}, &sandboxtypes.CreateCubeSandboxReq{
		Request: &sandboxtypes.Request{RequestID: "req-1"},
	}); err != nil {
		t.Fatalf("runSnapshotCreateJob returned error: %v", err)
	}

	if !upsertCalled {
		t.Fatal("expected UpsertReplica to be called on commit success")
	}
	if !readyTemplate || !readyJob {
		t.Fatalf("expected template + job to reach READY (template=%t job=%t)", readyTemplate, readyJob)
	}
	// v5: ReplicaStatus no longer carries any physical fields, so accidental
	// regressions cannot even compile. Verify control-plane identity only.
	if upserted.NodeID != "node-a" || upserted.NodeIP != "10.0.0.1" {
		t.Fatalf("snapshot replica must carry node identity, got %+v", upserted)
	}
}

func TestExecuteSnapshotCreateJobReturnsTerminalPersistenceError(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(claimSnapshotJobExecution, func(ctx context.Context, jobID, phase string, progress int32) (bool, error) {
		return true, nil
	})
	patches.ApplyFunc(runSnapshotCreateJob, func(ctx context.Context, jobID, sandboxID, nodeID, nodeIP string, createReq, storedReq *sandboxtypes.CreateCubeSandboxReq) error {
		return errors.New("persist ready status failed")
	})

	_, err := executeSnapshotCreateJob(context.Background(), &sandboxtypes.TemplateImageJobInfo{
		JobID:      "job-1",
		TemplateID: "snap-1",
		Status:     JobStatusPending,
	}, &sandboxtypes.CreateCubeSandboxReq{
		Request: &sandboxtypes.Request{RequestID: "req-1"},
	}, "sb-1", "node-a", "10.0.0.1")
	if err == nil {
		t.Fatal("expected executeSnapshotCreateJob to return persistence error")
	}
	if err.Error() != "persist ready status failed" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// After Phase 3 master no longer enumerates cubecow object refs when issuing
// snapshot cleanup. Each replica becomes a node-addressed locator with empty
// Objects; cubelet derives the canonical names from snapshot_id locally.
func TestSnapshotDeleteLocatorsEmitsNodeAddressedLocators(t *testing.T) {
	locators, err := snapshotDeleteLocators(&templateCleanupTargets{
		Replicas: []models.TemplateReplica{{
			TemplateID: "snap-1",
			NodeID:     "node-a",
			NodeIP:     "10.0.0.1",
		}},
	})
	if err != nil {
		t.Fatalf("snapshotDeleteLocators returned error: %v", err)
	}
	if len(locators) != 1 {
		t.Fatalf("unexpected locators: %+v", locators)
	}
	if locators[0].NodeID != "node-a" {
		t.Fatalf("unexpected locator: %+v", locators[0])
	}
	// v5: templateCleanupLocator no longer has Objects/SnapshotPath fields —
	// cubelet derives everything from its local catalog.
}

// When no replica rows survive (e.g. snapshot ready never propagated to
// replica table) the cleanup planner falls back to whatever locators the
// caller passed in (typically the origin node). Cubelet still resolves the
// objects locally.
func TestSnapshotDeleteLocatorsFallsBackToCallerProvidedLocators(t *testing.T) {
	locators, err := snapshotDeleteLocators(&templateCleanupTargets{
		Locators: []templateCleanupLocator{{
			NodeID: "node-a",
			NodeIP: "10.0.0.1",
		}},
	})
	if err != nil {
		t.Fatalf("snapshotDeleteLocators returned error: %v", err)
	}
	if len(locators) != 1 {
		t.Fatalf("unexpected locators: %+v", locators)
	}
	// v5: locators carry only (NodeID, NodeIP); cubelet derives Objects /
	// SnapshotPath from its local catalog.
}

func TestValidateSnapshotMetricsRejectsMissingKeys(t *testing.T) {
	err := validateSnapshotMetrics(map[string]uint64{
		"total_bytes": 1024,
		"used_bytes":  256,
	})
	if err == nil {
		t.Fatal("expected validateSnapshotMetrics to fail")
	}
	if !strings.Contains(err.Error(), "volume_count") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDeleteSnapshotBlocksWhenRuntimeRefsExist verifies that the active
// runtime binding table is the current-state authority: deleting a snapshot
// that is still attached to a sandbox must fail with a conflict.
//
// The legacy template in-use precheck remains warning-only, but active runtime
// refs are authoritative and must prevent reaching nextSnapshotAttempt.
func TestDeleteSnapshotBlocksWhenRuntimeRefsExist(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(getTemplateImageJobByRequestID, func(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
		return nil, gorm.ErrRecordNotFound
	})
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{
			TemplateID: templateID,
			Kind:       TemplateKindSnapshot,
			Status:     StatusReady,
		}, nil
	})
	patches.ApplyFunc(getActiveSnapshotJobByResourceID, func(ctx context.Context, resourceID string) (*models.TemplateImageJob, error) {
		return nil, gorm.ErrRecordNotFound
	})
	patches.ApplyFunc(discoverTemplateCleanupTargets, func(ctx context.Context, templateID, instanceType string) (*templateCleanupTargets, error) {
		return &templateCleanupTargets{
			InstanceType: "cubebox",
			Replicas: []models.TemplateReplica{{
				TemplateID: templateID,
				NodeID:     "node-a",
			}},
			Locators: []templateCleanupLocator{{
				NodeID: "node-a",
			}},
		}, nil
	})
	// The legacy template in-use precheck is still warning-only.
	patches.ApplyFunc(isTemplateInUse, func(ctx context.Context, templateID, instanceType string) (bool, error) {
		return true, nil
	})
	patches.ApplyFunc(ListActiveSnapshotRuntimeRefs, func(ctx context.Context, snapshotID string) ([]SnapshotRuntimeRefInfo, error) {
		return []SnapshotRuntimeRefInfo{{
			SnapshotID: snapshotID,
			SandboxID:  "sb-active",
			NodeID:     "node-a",
		}}, nil
	})

	sentinel := errors.New("sentinel: reached nextSnapshotAttempt past runtime-ref guard")
	patches.ApplyFunc(nextSnapshotAttempt, func(ctx context.Context, snapshotID string) (int32, string, error) {
		return 0, "", sentinel
	})

	_, err := DeleteSnapshot(context.Background(), "req-delete", "snap-in-use", "cubebox")
	if err == nil {
		t.Fatal("DeleteSnapshot returned nil error; expected active runtime refs to block deletion")
	}
	if errors.Is(err, sentinel) {
		t.Fatalf("DeleteSnapshot reached nextSnapshotAttempt despite active runtime refs: %v", err)
	}
	if !errors.Is(err, ErrTemplateAttemptInProgress) {
		t.Fatalf("DeleteSnapshot error = %v, want ErrTemplateAttemptInProgress", err)
	}
	if !strings.Contains(err.Error(), "active runtime ref") {
		t.Fatalf("DeleteSnapshot error = %q, want active runtime ref message", err.Error())
	}
}

func TestRunSnapshotDeleteJobCleansTemplateJobs(t *testing.T) {
	origReplicaCleanup := runReplicaCleanup
	origMetadataCleanup := runMetadataCleanup
	origJobCleanup := runTemplateJobCleanup
	t.Cleanup(func() {
		runReplicaCleanup = origReplicaCleanup
		runMetadataCleanup = origMetadataCleanup
		runTemplateJobCleanup = origJobCleanup
	})

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	jobsCleaned := false
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, fields map[string]any) error {
		return nil
	})
	patches.ApplyFunc(discoverTemplateCleanupTargets, func(ctx context.Context, templateID, instanceType string) (*templateCleanupTargets, error) {
		return &templateCleanupTargets{}, nil
	})
	patches.ApplyFunc(snapshotDeleteLocators, func(targets *templateCleanupTargets) ([]templateCleanupLocator, error) {
		return nil, nil
	})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})
	runReplicaCleanup = func(ctx context.Context, templateID string, locators []templateCleanupLocator) error {
		return nil
	}
	runMetadataCleanup = func(ctx context.Context, templateID string) error {
		return nil
	}
	runTemplateJobCleanup = func(ctx context.Context, templateID string) error {
		if templateID != "snap-del" {
			t.Fatalf("runTemplateJobCleanup templateID = %q, want snap-del", templateID)
		}
		jobsCleaned = true
		return nil
	}

	if err := runSnapshotDeleteJob(context.Background(), "job-del", "snap-del"); err != nil {
		t.Fatalf("runSnapshotDeleteJob returned error: %v", err)
	}
	if !jobsCleaned {
		t.Fatal("expected runTemplateJobCleanup to be called")
	}
}

func TestExecuteSnapshotDeleteJobReturnsReadyWithoutJobLookup(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(claimSnapshotJobExecution, func(ctx context.Context, jobID, phase string, progress int32) (bool, error) {
		return true, nil
	})
	patches.ApplyFunc(runSnapshotDeleteJob, func(ctx context.Context, jobID, snapshotID string) error {
		return nil
	})
	getJobInfoCallCount := 0
	patches.ApplyFunc(GetTemplateImageJobInfo, func(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		getJobInfoCallCount++
		return nil, nil
	})

	info, err := executeSnapshotDeleteJob(context.Background(), &sandboxtypes.TemplateImageJobInfo{
		JobID:      "job-del",
		TemplateID: "snap-del",
		Status:     JobStatusPending,
	}, "snap-del")
	if err != nil {
		t.Fatalf("executeSnapshotDeleteJob returned error: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil job info")
	}
	if info.JobID != "job-del" {
		t.Fatalf("jobID = %q, want %q", info.JobID, "job-del")
	}
	if info.TemplateID != "snap-del" {
		t.Fatalf("templateID = %q, want %q", info.TemplateID, "snap-del")
	}
	if info.Status != JobStatusReady {
		t.Fatalf("status = %q, want %q", info.Status, JobStatusReady)
	}
	if info.Phase != JobPhaseReady {
		t.Fatalf("phase = %q, want %q", info.Phase, JobPhaseReady)
	}
	if info.Progress != 100 {
		t.Fatalf("progress = %d, want 100", info.Progress)
	}
	if getJobInfoCallCount != 0 {
		t.Fatalf("GetTemplateImageJobInfo called %d time(s), want 0", getJobInfoCallCount)
	}
}
