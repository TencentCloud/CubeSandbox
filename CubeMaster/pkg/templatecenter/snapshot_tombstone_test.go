// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	_ "github.com/go-sql-driver/mysql"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestMatchesSnapshotRecordListOptionsHidesTombstone(t *testing.T) {
	if matchesSnapshotRecordListOptions(&models.SnapshotRecord{Status: StatusDeleted}, ListSnapshotsOptions{}) {
		t.Fatal("DELETED snapshots must be hidden from default list")
	}
	if matchesSnapshotRecordListOptions(&models.SnapshotRecord{Status: StatusDeleting}, ListSnapshotsOptions{}) {
		t.Fatal("DELETING snapshots must be hidden from default list")
	}
	if !matchesSnapshotRecordListOptions(&models.SnapshotRecord{Status: StatusReady}, ListSnapshotsOptions{}) {
		t.Fatal("READY snapshots must remain listed")
	}
	if !matchesSnapshotRecordListOptions(&models.SnapshotRecord{Status: StatusDeleted}, ListSnapshotsOptions{Status: StatusDeleted}) {
		t.Fatal("explicit status filter should still match DELETED")
	}
}

func TestGetSnapshotInfoHidesTombstone(t *testing.T) {
	for _, status := range []string{StatusDeleted, StatusDeleting} {
		t.Run(status, func(t *testing.T) {
			oldDB := store.db
			store.db = &gorm.DB{}
			defer func() { store.db = oldDB }()

			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
				return &models.SnapshotRecord{SnapshotID: snapshotID, Status: status}, nil
			})

			_, err := GetSnapshotInfo(context.Background(), "snap-gone", false)
			if !errors.Is(err, ErrSnapshotNotFound) {
				t.Fatalf("GetSnapshotInfo(%s) error = %v, want ErrSnapshotNotFound", status, err)
			}
		})
	}
}

func TestGetSnapshotRestoreSourceRejectsTombstone(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted}, nil
	})

	_, err := GetSnapshotRestoreSource(context.Background(), "snap-gone")
	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("GetSnapshotRestoreSource error = %v, want ErrSnapshotNotFound", err)
	}
}

func TestRollbackSandboxToSnapshotRejectsTombstone(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getTemplateImageJobByRequestID, func(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
		return nil, gorm.ErrRecordNotFound
	})
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted, OriginSandboxID: "sb-1"}, nil
	})

	_, err := RollbackSandboxToSnapshot(context.Background(), "req-rb", "sb-1", "snap-gone", "cubebox", "")
	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("RollbackSandboxToSnapshot error = %v, want ErrSnapshotNotFound", err)
	}
}

func TestEnsureSnapshotReadyForNewUseRejectsTombstone(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted}, nil
	})

	err := EnsureSnapshotReadyForNewUse(context.Background(), "snap-gone")
	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("EnsureSnapshotReadyForNewUse error = %v, want ErrSnapshotNotFound", err)
	}
}

func TestRegisterSnapshotRuntimeRefAllowsTombstoneAcquire(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted}, nil
	})
	origAcquire := acquireSnapshotRuntimeRefFn
	t.Cleanup(func() { acquireSnapshotRuntimeRefFn = origAcquire })
	acquired := false
	acquireSnapshotRuntimeRefFn = func(ctx context.Context, ref SnapshotRuntimeRefInfo) error {
		acquired = true
		if ref.SnapshotID != "snap-tomb" || ref.SandboxID != "sb-1" {
			t.Fatalf("unexpected acquire %#v", ref)
		}
		return nil
	}
	patches.ApplyFunc(getSnapshotReadyReplica, func(ctx context.Context, snapshotID, preferredNodeID string) (ReplicaStatus, error) {
		t.Fatal("tombstoned register must skip ready-replica check")
		return ReplicaStatus{}, nil
	})

	if err := RegisterSnapshotRuntimeRefForCreatedSandbox(context.Background(), "snap-tomb", "sb-1", "node-a", "10.0.0.1"); err != nil {
		t.Fatalf("RegisterSnapshotRuntimeRefForCreatedSandbox: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire on tombstoned snapshot")
	}
}

func TestRegisterSnapshotRuntimeRefAllowsDeletingAcquire(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleting}, nil
	})
	origAcquire := acquireSnapshotRuntimeRefFn
	t.Cleanup(func() { acquireSnapshotRuntimeRefFn = origAcquire })
	acquired := false
	acquireSnapshotRuntimeRefFn = func(ctx context.Context, ref SnapshotRuntimeRefInfo) error {
		acquired = true
		return nil
	}
	patches.ApplyFunc(getSnapshotReadyReplica, func(ctx context.Context, snapshotID, preferredNodeID string) (ReplicaStatus, error) {
		t.Fatal("DELETING register must skip ready-replica check")
		return ReplicaStatus{}, nil
	})

	if err := RegisterSnapshotRuntimeRefForCreatedSandbox(context.Background(), "snap-dying", "sb-1", "node-a", "10.0.0.1"); err != nil {
		t.Fatalf("RegisterSnapshotRuntimeRefForCreatedSandbox: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire on DELETING snapshot")
	}
}

func TestRegisterSnapshotRuntimeRefReadyStillValidates(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusReady}, nil
	})
	origAcquire := acquireSnapshotRuntimeRefFn
	t.Cleanup(func() { acquireSnapshotRuntimeRefFn = origAcquire })
	acquireSnapshotRuntimeRefFn = func(ctx context.Context, ref SnapshotRuntimeRefInfo) error {
		t.Fatal("READY register must not acquire after validate fails")
		return nil
	}
	patches.ApplyFunc(getSnapshotReadyReplica, func(ctx context.Context, snapshotID, preferredNodeID string) (ReplicaStatus, error) {
		return ReplicaStatus{}, errors.New("no ready replica")
	})

	err := RegisterSnapshotRuntimeRefForCreatedSandbox(context.Background(), "snap-ready", "sb-1", "node-a", "10.0.0.1")
	if err == nil {
		t.Fatal("READY register must run validate")
	}
}

func TestFailSnapshotDeleteJobReturnsTombstone(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	origTx := withStoreTx
	t.Cleanup(func() { withStoreTx = origTx })
	withStoreTx = func(ctx context.Context, fn func(*gorm.DB) error) error {
		return fn(store.db)
	}

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var status string
	patches.ApplyFunc(updateSnapshotFieldsTx, func(tx *gorm.DB, snapshotID string, values map[string]any) error {
		if s, ok := values["status"].(string); ok {
			status = s
		}
		return nil
	})
	patches.ApplyFunc(updateTemplateImageJobTx, func(tx *gorm.DB, jobID string, values map[string]any) error {
		return nil
	})

	if err := failSnapshotDeleteJob(context.Background(), "job-1", "snap-1", errors.New("cubelet busy")); err != nil {
		t.Fatalf("failSnapshotDeleteJob: %v", err)
	}
	if status != StatusDeleted {
		t.Fatalf("status = %q, want %q", status, StatusDeleted)
	}
}

func TestFailSnapshotDeleteJobRollsBackWhenJobUpdateFails(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	origTx := withStoreTx
	t.Cleanup(func() { withStoreTx = origTx })
	snapWritten := false
	withStoreTx = func(ctx context.Context, fn func(*gorm.DB) error) error {
		if err := fn(store.db); err != nil {
			snapWritten = false
			return err
		}
		return nil
	}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(updateSnapshotFieldsTx, func(tx *gorm.DB, snapshotID string, values map[string]any) error {
		snapWritten = true
		return nil
	})
	patches.ApplyFunc(updateTemplateImageJobTx, func(tx *gorm.DB, jobID string, values map[string]any) error {
		return errors.New("job write failed")
	})

	err := failSnapshotDeleteJob(context.Background(), "job-1", "snap-1", errors.New("cubelet busy"))
	if err == nil {
		t.Fatal("expected failSnapshotDeleteJob to return the job write error")
	}
	if snapWritten {
		t.Fatal("snapshot row must not stay DELETED when the job update fails")
	}
}

func TestBeginTombstonePhysicalCleanupRequiresZeroRefs(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted}, nil
	})
	origCount := countActiveSnapshotRuntimeRefsFn
	t.Cleanup(func() { countActiveSnapshotRuntimeRefsFn = origCount })
	countActiveSnapshotRuntimeRefsFn = func(ctx context.Context, snapshotID string) (int64, error) {
		return 1, nil
	}
	patches.ApplyFunc(nextSnapshotAttempt, func(ctx context.Context, snapshotID string) (int32, string, error) {
		t.Fatal("must not start delete job while refs remain")
		return 0, "", nil
	})

	jobID, err := beginTombstonePhysicalCleanup(context.Background(), "snap-1")
	if err != nil {
		t.Fatalf("beginTombstonePhysicalCleanup: %v", err)
	}
	if jobID != "" {
		t.Fatalf("jobID = %q, want empty while refs remain", jobID)
	}
}

func TestExpireOrphanSnapshotRuntimeRefsRequiresMissingSandboxAndTTL(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	origExists := sandboxExistsOnMasterFn
	origNow := snapshotNow
	t.Cleanup(func() {
		sandboxExistsOnMasterFn = origExists
		snapshotNow = origNow
	})

	seen := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	snapshotNow = func() time.Time { return seen.Add(snapshotOrphanRuntimeRefTTL + time.Hour) }

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(listSnapshotRecordsByStatus, func(ctx context.Context, statuses ...string) ([]models.SnapshotRecord, error) {
		return []models.SnapshotRecord{{SnapshotID: "snap-1", Status: StatusDeleted}}, nil
	})
	patches.ApplyFunc(ListActiveSnapshotRuntimeRefs, func(ctx context.Context, snapshotID string) ([]SnapshotRuntimeRefInfo, error) {
		return []SnapshotRuntimeRefInfo{{
			SnapshotID: snapshotID,
			SandboxID:  "sb-missing",
			LastSeenAt: &seen,
		}}, nil
	})

	sandboxExistsOnMasterFn = func(ctx context.Context, sandboxID string) bool { return true }
	detached := false
	patches.ApplyFunc(DetachSnapshotRuntimeBinding, func(ctx context.Context, sandboxID, bindingType, reason string) error {
		detached = true
		return nil
	})
	if err := expireOrphanSnapshotRuntimeRefs(context.Background()); err != nil {
		t.Fatalf("expire while sandbox exists: %v", err)
	}
	if detached {
		t.Fatal("must not expire refs while the sandbox still exists on master")
	}

	sandboxExistsOnMasterFn = func(ctx context.Context, sandboxID string) bool { return false }
	snapshotNow = func() time.Time { return seen.Add(time.Hour) }
	if err := expireOrphanSnapshotRuntimeRefs(context.Background()); err != nil {
		t.Fatalf("expire before TTL: %v", err)
	}
	if detached {
		t.Fatal("must not expire refs before last_seen TTL")
	}

	snapshotNow = func() time.Time { return seen.Add(snapshotOrphanRuntimeRefTTL + time.Hour) }
	if err := expireOrphanSnapshotRuntimeRefs(context.Background()); err != nil {
		t.Fatalf("expire after TTL: %v", err)
	}
	if !detached {
		t.Fatal("expected orphan ref detach after TTL when sandbox is gone")
	}
}

func TestGetTemplateKindSkipsTombstone(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getCachedTemplateKind, func(templateID string) (string, bool) {
		return "", false
	})
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted}, nil
	})
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return nil, ErrTemplateNotFound
	})

	_, err := GetTemplateKind(context.Background(), "snap-gone")
	if err == nil {
		t.Fatal("GetTemplateKind should not treat a tombstoned snapshot as a live snapshot")
	}
	if !errors.Is(err, ErrTemplateNotFound) && !strings.Contains(err.Error(), "not found") {
		t.Fatalf("GetTemplateKind error = %v, want not found", err)
	}
}

func TestSnapshotRuntimeRefFromTemplateIDIncludesTombstone(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted}, nil
	})

	ref, ok := snapshotRuntimeRefFromTemplateID(context.Background(), &sandboxtypes.SandboxBriefData{
		SandboxID:  "sb-1",
		TemplateID: "snap-tomb",
		HostID:     "node-a",
		HostIP:     "10.0.0.1",
	})
	if !ok {
		t.Fatal("tombstoned snapshot row must still be observed from TemplateID")
	}
	if ref.SnapshotID != "snap-tomb" || ref.SandboxID != "sb-1" {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestRetainUnobservedTombstoneRuntimeRef(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	origExists := sandboxExistsOnMasterFn
	t.Cleanup(func() { sandboxExistsOnMasterFn = origExists })

	item := models.SnapshotRuntimeActive{SnapshotID: "snap-1", SandboxID: "sb-1"}
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	status := StatusDeleted
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: status}, nil
	})

	sandboxExistsOnMasterFn = func(ctx context.Context, sandboxID string) bool { return true }
	if !retainUnobservedTombstoneRuntimeRef(context.Background(), item) {
		t.Fatal("tombstone + sandbox on master must keep the unobserved ref")
	}

	status = StatusDeleting
	if !retainUnobservedTombstoneRuntimeRef(context.Background(), item) {
		t.Fatal("DELETING + sandbox on master must keep the unobserved ref")
	}

	sandboxExistsOnMasterFn = func(ctx context.Context, sandboxID string) bool { return false }
	if retainUnobservedTombstoneRuntimeRef(context.Background(), item) {
		t.Fatal("tombstone + sandbox gone must still allow detach")
	}

	status = StatusReady
	sandboxExistsOnMasterFn = func(ctx context.Context, sandboxID string) bool { return true }
	if retainUnobservedTombstoneRuntimeRef(context.Background(), item) {
		t.Fatal("READY snapshot must not skip detach-on-unobserved")
	}
}

func TestSandboxExistsOnMasterFailClosedOnSpecError(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetSandboxCache, func(sandboxID string) *localcache.SandboxCache {
		return nil
	})
	patches.ApplyFunc(localcache.GetSandboxProxyMap, func(ctx context.Context, sandboxID string) (*types.SandboxProxyMap, bool) {
		return nil, false
	})
	patches.ApplyFunc(sandboxspec.Get, func(ctx context.Context, sandboxID string) (*sandboxtypes.CreateCubeSandboxReq, error) {
		return nil, errors.New("spec db unavailable")
	})

	if !sandboxExistsOnMaster(context.Background(), "sb-spec-err") {
		t.Fatal("spec store error must fail closed and treat the sandbox as still present")
	}
}

func TestGetTemplateInfoHidesTombstoneDespiteDefinition(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return &models.TemplateDefinition{TemplateID: templateID, Kind: TemplateKindSnapshot, Status: StatusReady}, nil
	})
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted}, nil
	})
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		t.Fatal("must not list replicas for a tombstoned snapshot")
		return nil, nil
	})

	_, err := GetTemplateInfo(context.Background(), "snap-gone")
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("GetTemplateInfo error = %v, want ErrTemplateNotFound", err)
	}
}

func TestListTemplatesHidesTombstonedDefinition(t *testing.T) {
	oldDB := store.db
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(hiddenSnapshotIDs, func(ctx context.Context) (map[string]struct{}, error) {
		return map[string]struct{}{"snap-tomb": {}}, nil
	})

	store.db = stubListTemplatesDB(t,
		[]models.TemplateDefinition{
			{TemplateID: "snap-tomb", Kind: TemplateKindSnapshot, Status: StatusReady},
			{TemplateID: "tpl-live", Status: StatusReady},
		},
		nil,
	)

	out, listErr := ListTemplates(context.Background())
	if listErr != nil {
		t.Fatalf("ListTemplates: %v", listErr)
	}
	ids := make([]string, 0, len(out))
	foundLive := false
	for _, item := range out {
		ids = append(ids, item.TemplateID)
		if item.TemplateID == "snap-tomb" {
			t.Fatalf("tombstoned definition leaked into ListTemplates: %v", ids)
		}
		if item.TemplateID == "tpl-live" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("live template missing from ListTemplates: %v", ids)
	}
}

func TestGetTemplateInfoHidesTombstoneDespiteCreateJob(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(GetDefinition, func(ctx context.Context, templateID string) (*models.TemplateDefinition, error) {
		return nil, ErrTemplateNotFound
	})
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted}, nil
	})
	patches.ApplyFunc(getLatestTemplateImageJobByTemplateID, func(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
		t.Fatal("job fallback must not resurrect a tombstoned snapshot")
		return &models.TemplateImageJob{TemplateID: templateID, Status: JobStatusReady}, nil
	})

	_, err := GetTemplateInfo(context.Background(), "snap-gone")
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("GetTemplateInfo error = %v, want ErrTemplateNotFound", err)
	}
}

func TestGetTemplateRequestRejectsTombstone(t *testing.T) {
	invalidateTemplateCaches("snap-gone-req")
	defer invalidateTemplateCaches("snap-gone-req")

	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		return &models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusDeleted, RequestJSON: `{}`}, nil
	})

	_, err := GetTemplateRequest(context.Background(), "snap-gone-req")
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("GetTemplateRequest error = %v, want ErrTemplateNotFound", err)
	}
}

func TestHiddenSnapshotIDsIncludesDeletedAndDeleting(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(listSnapshotRecordsByStatus, func(ctx context.Context, statuses ...string) ([]models.SnapshotRecord, error) {
		if len(statuses) != 2 || statuses[0] != StatusDeleted || statuses[1] != StatusDeleting {
			t.Fatalf("statuses = %v, want DELETED, DELETING", statuses)
		}
		return []models.SnapshotRecord{
			{SnapshotID: "snap-tomb", Status: StatusDeleted},
			{SnapshotID: "snap-dying", Status: StatusDeleting},
		}, nil
	})

	ids, err := hiddenSnapshotIDs(context.Background())
	if err != nil {
		t.Fatalf("hiddenSnapshotIDs: %v", err)
	}
	if _, ok := ids["snap-tomb"]; !ok {
		t.Fatal("missing snap-tomb")
	}
	if _, ok := ids["snap-dying"]; !ok {
		t.Fatal("missing snap-dying")
	}
	if _, ok := ids["snap-ready"]; ok {
		t.Fatal("READY snapshot must not be hidden from job fallback")
	}
}

func stubListTemplatesDB(t *testing.T, defs []models.TemplateDefinition, jobs []models.TemplateImageJob) *gorm.DB {
	t.Helper()
	sqlDB, err := sql.Open("mysql", "root:root@tcp(127.0.0.1:3306)/unused?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *[]models.TemplateDefinition:
			*dest = append([]models.TemplateDefinition(nil), defs...)
		case *[]models.TemplateImageJob:
			*dest = append([]models.TemplateImageJob(nil), jobs...)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return db
}
