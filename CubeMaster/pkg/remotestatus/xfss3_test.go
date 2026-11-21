// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package remotestatus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	snapshotv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/snapshot/v1"
	"gorm.io/gorm"
)

const verifiedLocator = `{"checkpoint":"cube-xfs-s3/snap-x/candidate.json","manifest_sha256":"abc"}`

type xfss3Node struct {
	state string
	msg   string
	err   error
}

// withXFSS3Cluster answers Status per node IP and records who was asked.
func withXFSS3Cluster(t *testing.T, nodes []string, answers map[string]xfss3Node) *[]string {
	t.Helper()
	var asked []string
	prevQuery, prevAddr, prevList := queryStatus, cubeletAddr, listVerifierNodes
	queryStatus = func(_ context.Context, addr string, req *snapshotv1.StatusRequest) (*snapshotv1.StatusResponse, error) {
		if req.GetBackend() != constants.SnapshotBackendXFSS3 {
			t.Errorf("xfs-s3 row queried with backend %q", req.GetBackend())
		}
		asked = append(asked, addr)
		a := answers[addr]
		if a.err != nil {
			return nil, a.err
		}
		return &snapshotv1.StatusResponse{State: a.state, Message: a.msg, RemoteReady: a.state == "ready"}, nil
	}
	cubeletAddr = func(ip string) string { return ip }
	listVerifierNodes = func() []string { return nodes }
	t.Cleanup(func() { queryStatus, cubeletAddr, listVerifierNodes = prevQuery, prevAddr, prevList })
	return &asked
}

func TestXFSS3ReadyOnlyAfterReadbackOnAnotherNode(t *testing.T) {
	db := setupReconcilerDB(t)
	requireNoErr(t, db.Create(&models.PauseSnapshotRecord{
		SnapshotID: "snap-x", SandboxID: "sb", NodeIP: "10.0.0.1", Status: "READY",
		Backend: constants.SnapshotBackendXFSS3, RemoteStatus: constants.RemoteStatusInProgress,
		ExportUUIDs: `{"checkpoint":"cube-xfs-s3/snap-x/candidate.json"}`,
	}).Error)
	ctx := context.Background()

	// Origin still uploading: the verifier is not asked.
	asked := withXFSS3Cluster(t, []string{"10.0.0.1", "10.0.0.2"}, map[string]xfss3Node{
		"10.0.0.1": {state: "running"},
	})
	ReconcileOnce(ctx, db)
	if len(*asked) != 1 || (*asked)[0] != "10.0.0.1" {
		t.Fatalf("asked %v, want only the origin", *asked)
	}
	assertRemote(t, db, constants.PauseSnapshotTableName, "snap-x", constants.RemoteStatusInProgress)

	// Origin committed; the other node is still reading back.
	asked = withXFSS3Cluster(t, []string{"10.0.0.1", "10.0.0.2"}, map[string]xfss3Node{
		"10.0.0.1": {state: "exported"},
		"10.0.0.2": {state: "running"},
	})
	ReconcileOnce(ctx, db)
	if len(*asked) != 2 || (*asked)[1] != "10.0.0.2" {
		t.Fatalf("asked %v, want origin then 10.0.0.2", *asked)
	}
	assertRemote(t, db, constants.PauseSnapshotTableName, "snap-x", constants.RemoteStatusInProgress)

	// Readback passed: ready, and export_uuids pins the verified digest.
	withXFSS3Cluster(t, []string{"10.0.0.1", "10.0.0.2"}, map[string]xfss3Node{
		"10.0.0.1": {state: "exported"},
		"10.0.0.2": {state: "ready", msg: verifiedLocator},
	})
	ReconcileOnce(ctx, db)
	assertRemote(t, db, constants.PauseSnapshotTableName, "snap-x", constants.RemoteStatusReady)
	var row models.PauseSnapshotRecord
	requireNoErr(t, db.Table(constants.PauseSnapshotTableName).Where("snapshot_id = ?", "snap-x").First(&row).Error)
	if row.ExportUUIDs != verifiedLocator {
		t.Fatalf("export_uuids=%q, want the verified locator", row.ExportUUIDs)
	}
}

func TestXFSS3OriginAloneNeverPublishesReady(t *testing.T) {
	db := setupReconcilerDB(t)
	requireNoErr(t, db.Create(&models.PauseSnapshotRecord{
		SnapshotID: "snap-x", SandboxID: "sb", NodeIP: "10.0.0.1", Status: "READY",
		Backend: constants.SnapshotBackendXFSS3, RemoteStatus: constants.RemoteStatusInProgress,
	}).Error)
	// A single-node cluster, and an origin that (wrongly) says ready.
	asked := withXFSS3Cluster(t, []string{"10.0.0.1"}, map[string]xfss3Node{
		"10.0.0.1": {state: "ready", msg: verifiedLocator},
	})
	ReconcileOnce(context.Background(), db)
	assertRemote(t, db, constants.PauseSnapshotTableName, "snap-x", constants.RemoteStatusInProgress)
	if len(*asked) != 1 {
		t.Fatalf("asked %v", *asked)
	}
}

func TestXFSS3UnreachableOriginStillVerifiedElsewhere(t *testing.T) {
	db := setupReconcilerDB(t)
	requireNoErr(t, db.Create(&models.PauseSnapshotRecord{
		SnapshotID: "snap-x", SandboxID: "sb", NodeIP: "10.0.0.1", Status: "READY",
		Backend: constants.SnapshotBackendXFSS3, RemoteStatus: constants.RemoteStatusInProgress,
	}).Error)
	withXFSS3Cluster(t, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, map[string]xfss3Node{
		"10.0.0.1": {err: errors.New("connection refused")},
		"10.0.0.2": {state: "ready", msg: verifiedLocator},
		"10.0.0.3": {state: "ready", msg: verifiedLocator},
	})
	ReconcileOnce(context.Background(), db)
	assertRemote(t, db, constants.PauseSnapshotTableName, "snap-x", constants.RemoteStatusReady)
}

func TestXFSS3FailuresSettleFailed(t *testing.T) {
	for name, answers := range map[string]map[string]xfss3Node{
		"origin export failed": {"10.0.0.1": {state: "failed"}},
		"readback corrupt":     {"10.0.0.1": {state: "exported"}, "10.0.0.2": {state: "failed"}},
	} {
		t.Run(name, func(t *testing.T) {
			db := setupReconcilerDB(t)
			requireNoErr(t, db.Create(&models.PauseSnapshotRecord{
				SnapshotID: "snap-x", SandboxID: "sb", NodeIP: "10.0.0.1", Status: "READY",
				Backend: constants.SnapshotBackendXFSS3, RemoteStatus: constants.RemoteStatusInProgress,
			}).Error)
			withXFSS3Cluster(t, []string{"10.0.0.1", "10.0.0.2"}, answers)
			ReconcileOnce(context.Background(), db)
			assertRemote(t, db, constants.PauseSnapshotTableName, "snap-x", constants.RemoteStatusFailed)
		})
	}
}

func TestXFSS3ReadyWithoutLocatorIsNotPublished(t *testing.T) {
	db := setupReconcilerDB(t)
	requireNoErr(t, db.Create(&models.PauseSnapshotRecord{
		SnapshotID: "snap-x", SandboxID: "sb", NodeIP: "10.0.0.1", Status: "READY",
		Backend: constants.SnapshotBackendXFSS3, RemoteStatus: constants.RemoteStatusInProgress,
	}).Error)
	withXFSS3Cluster(t, []string{"10.0.0.1", "10.0.0.2"}, map[string]xfss3Node{
		"10.0.0.1": {state: "exported"},
		"10.0.0.2": {state: "ready"},
	})
	ReconcileOnce(context.Background(), db)
	assertRemote(t, db, constants.PauseSnapshotTableName, "snap-x", constants.RemoteStatusInProgress)
}

func TestPickVerifierExcludesOriginAndIsStable(t *testing.T) {
	prev := listVerifierNodes
	t.Cleanup(func() { listVerifierNodes = prev })
	listVerifierNodes = func() []string { return []string{"10.0.0.3", "10.0.0.1", "10.0.0.2"} }
	for i := 0; i < 50; i++ {
		id := "snap-" + string(rune('a'+i%26))
		got := pickVerifier("10.0.0.1", id)
		if got == "10.0.0.1" || got == "" {
			t.Fatalf("pickVerifier(%s)=%q", id, got)
		}
		if again := pickVerifier("10.0.0.1", id); again != got {
			t.Fatalf("unstable verifier for %s: %s vs %s", id, got, again)
		}
	}
	listVerifierNodes = func() []string { return []string{"10.0.0.1"} }
	if got := pickVerifier("10.0.0.1", "snap-a"); got != "" {
		t.Fatalf("origin-only cluster picked %q", got)
	}
}

func TestXFSS3IncompatibleVerifierIsSkipped(t *testing.T) {
	db := setupReconcilerDB(t)
	requireNoErr(t, db.Create(&models.PauseSnapshotRecord{
		SnapshotID: "snap-cpu", SandboxID: "sb", NodeIP: "10.0.0.1", Status: "READY",
		Backend: constants.SnapshotBackendXFSS3, RemoteStatus: constants.RemoteStatusInProgress,
	}).Error)
	nodes := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	first := pickVerifierFor(t, nodes, "10.0.0.1", "snap-cpu")
	second := "10.0.0.2"
	if first == second {
		second = "10.0.0.3"
	}
	asked := withXFSS3Cluster(t, nodes, map[string]xfss3Node{
		"10.0.0.1": {state: "exported"},
		first:      {state: "pending", msg: "retry(compat): trial restore failed at compat: cpu feature compatibility"},
		second:     {state: "ready", msg: verifiedLocator},
	})
	ReconcileOnce(context.Background(), db)
	assertRemote(t, db, constants.PauseSnapshotTableName, "snap-cpu", constants.RemoteStatusInProgress)
	ReconcileOnce(context.Background(), db)
	assertRemote(t, db, constants.PauseSnapshotTableName, "snap-cpu", constants.RemoteStatusReady)
	if (*asked)[1] != first || (*asked)[3] != second {
		t.Fatalf("asked %v: want %s, then %s after it could not vouch", *asked, first, second)
	}
}

func TestXFSS3AgentRetryKeepsTheVerifier(t *testing.T) {
	if _, ok := verifierCannotVouch("retry(agent): agent did not answer"); ok {
		t.Fatal("an agent retry is not a property of the verifier")
	}
	if stage, ok := verifierCannotVouch("retry(exhausted): 6 trial restores failed on this node"); !ok || stage != "exhausted" {
		t.Fatalf("exhausted: %q %v", stage, ok)
	}
	if stage, ok := verifierCannotVouch("retry(deps): pmem missing"); !ok || stage != "deps" {
		t.Fatalf("deps: %q %v", stage, ok)
	}
}

func pickVerifierFor(t *testing.T, nodes []string, origin, snap string) string {
	t.Helper()
	prev := listVerifierNodes
	defer func() { listVerifierNodes = prev }()
	listVerifierNodes = func() []string { return nodes }
	return pickVerifier(origin, snap)
}

func seedXFSS3Row(t *testing.T, snap string) *gorm.DB {
	t.Helper()
	db := setupReconcilerDB(t)
	requireNoErr(t, db.Create(&models.PauseSnapshotRecord{
		SnapshotID: snap, SandboxID: "sb", NodeIP: "10.0.0.1", Status: "READY",
		Backend: constants.SnapshotBackendXFSS3, RemoteStatus: constants.RemoteStatusInProgress,
	}).Error)
	return db
}

func TestXFSS3FailsOnlyWhenTwoNodesFailToRestore(t *testing.T) {
	const snap = "snap-restore-two"
	db := seedXFSS3Row(t, snap)
	nodes := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	restoreFailed := xfss3Node{state: "pending", msg: "retry(restore): trial restore failed at restore: vm restore failed"}
	asked := withXFSS3Cluster(t, nodes, map[string]xfss3Node{
		"10.0.0.1": {state: "exported"},
		"10.0.0.2": restoreFailed,
		"10.0.0.3": restoreFailed,
	})
	ReconcileOnce(context.Background(), db)
	assertRemote(t, db, constants.PauseSnapshotTableName, snap, constants.RemoteStatusInProgress)
	ReconcileOnce(context.Background(), db)
	assertRemote(t, db, constants.PauseSnapshotTableName, snap, constants.RemoteStatusFailed)
	if (*asked)[1] == (*asked)[3] {
		t.Fatalf("asked %v: the second verdict must come from another node", *asked)
	}
}

func TestXFSS3OneNodeFailingToRestoreNeverFailsTheCheckpoint(t *testing.T) {
	const snap = "snap-restore-one"
	db := seedXFSS3Row(t, snap)
	now := time.Now()
	prevNow := nowFn
	t.Cleanup(func() { nowFn = prevNow })
	nowFn = func() time.Time { return now }
	withXFSS3Cluster(t, []string{"10.0.0.1", "10.0.0.2"}, map[string]xfss3Node{
		"10.0.0.1": {state: "exported"},
		"10.0.0.2": {state: "pending", msg: "retry(restore): trial restore failed at restore: vm restore failed"},
	})
	for i := 0; i < 3; i++ {
		ReconcileOnce(context.Background(), db)
		assertRemote(t, db, constants.PauseSnapshotTableName, snap, constants.RemoteStatusInProgress)
		// The only other node is asked again once its exclusion expires.
		now = now.Add(verifierExclusionTTL + time.Minute)
	}
}
