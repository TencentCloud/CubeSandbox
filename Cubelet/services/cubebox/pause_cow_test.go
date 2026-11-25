// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type blockingPauseTask struct {
	containerd.Task
	started chan context.Context
	saved   chan struct{}
}

func (task *blockingPauseTask) Update(ctx context.Context, _ ...containerd.UpdateTaskOpts) error {
	task.started <- ctx
	select {
	case <-task.saved:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCapturePauseSnapshotFinishesAfterCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := namespaces.WithNamespace(context.Background(), "cube-ns")
		ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		task := &blockingPauseTask{started: make(chan context.Context), saved: make(chan struct{}, 1)}
		defer close(task.saved)
		result := make(chan error, 1)
		go func() { result <- capturePauseSnapshot(ctx, task, "{}") }()

		shimCtx := <-task.started
		<-ctx.Done()
		synctest.Wait()
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		select {
		case err := <-result:
			t.Fatalf("pause capture returned before the writer finished: %v", err)
		default:
		}

		namespace, err := namespaces.NamespaceRequired(shimCtx)
		require.NoError(t, err)
		require.Equal(t, "cube-ns", namespace)
		task.saved <- struct{}{}
		require.NoError(t, <-result)
	})
}

func TestScanDeadContainerSkipsActivePause(t *testing.T) {
	pausingAt := time.Now().Add(-2 * pausingStuckThreshold).UnixNano()
	box := newCubeboxWithStatusForTest("sb-slow-pause", cubeboxstore.Status{
		StartedAt: pausingAt - 1,
		PausingAt: pausingAt,
	})
	task := &fakeResumeTask{status: containerd.Paused}
	box.FirstContainer().Container = &stubContainer{task: task}
	boxes := []*cubeboxstore.CubeBox{box}
	lifecycleLocks := utils.NewResourceLocks()

	unlock := lifecycleLocks.Lock(box.ID)
	scanDeadContainer(context.Background(), boxes, nil, time.Hour, lifecycleLocks)
	unlock()
	require.Zero(t, task.statusCalls)
	require.Equal(t, pausingAt, box.GetStatus().Get().PausingAt)

	scanDeadContainer(context.Background(), boxes, nil, time.Hour, lifecycleLocks)
	require.Equal(t, 1, task.statusCalls)
	require.Equal(t, cubebox.ContainerState_CONTAINER_PAUSED, box.GetStatus().Get().State())
}

func TestResolvePauseSnapshotID(t *testing.T) {
	t.Parallel()
	_, err := resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{})
	if err == nil {
		t.Fatal("expected error when snapshot id missing")
	}

	id, err := resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationPauseSnapshotID: "snap-abc123def456abc123def456",
		},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if id != "snap-abc123def456abc123def456" {
		t.Fatalf("id=%q", id)
	}

	_, err = resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationPauseSnapshotID: "pause-sbx1",
		},
	})
	if err == nil {
		t.Fatal("expected error for non snap- prefix")
	}

	id, err = resolvePauseSnapshotID(&cubebox.UpdateCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationRuntimeSnapshotID: "snap-from-runtime000000000001",
		},
	})
	if err != nil {
		t.Fatalf("runtime fallback err: %v", err)
	}
	if id != "snap-from-runtime000000000001" {
		t.Fatalf("id=%q", id)
	}
}

func TestStampPauseSnapshotID(t *testing.T) {
	t.Parallel()
	stampPauseSnapshotID(nil, "snap-abc123def456abc123def456")

	sb := newCubeboxWithStatusForTest("sb-stamp", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(sb, "snap-abc123def456abc123def456")
	if sb.Labels[constants.MasterAnnotationPauseSnapshotID] != "snap-abc123def456abc123def456" {
		t.Fatalf("labels=%v", sb.Labels)
	}
	if sb.Annotations[constants.MasterAnnotationPauseSnapshotID] != "snap-abc123def456abc123def456" {
		t.Fatalf("annotations=%v", sb.Annotations)
	}
	if got := stampedPauseSnapshotID(sb); got != "snap-abc123def456abc123def456" {
		t.Fatalf("stampedPauseSnapshotID=%q", got)
	}
}

func TestIsPauseBusyReject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{pauseBusyRejectMessage, true},
		{`Others("Other: update sandbox failed:sandbox pause forbidding, terminate exec tasks first")`, true},
		{"PAUSE FORBIDDING", true},
		{"terminate exec tasks first", true},
		{"task pause failed: oom", false},
		{"", false},
	} {
		if got := isPauseBusyReject(tc.msg); got != tc.want {
			t.Fatalf("isPauseBusyReject(%q)=%v want %v", tc.msg, got, tc.want)
		}
	}
}

func TestPauseCaptureLeftVMRunning(t *testing.T) {
	t.Parallel()
	require.True(t, pauseCaptureLeftVMRunning(pauseBusyRejectMessage))
	require.True(t, pauseCaptureLeftVMRunning("update sandbox failed: pause snapshot failed; original VM is running: no space left on device"))
	require.False(t, pauseCaptureLeftVMRunning("pause snapshot failed: no space left on device; resume original VM failed: device unavailable"))
}

func TestRollBackLocalPauseAttempt(t *testing.T) {
	t.Parallel()
	prev := "snap-prev000000000000000000000001"
	aborted := "snap-abort00000000000000000000001"
	sb := newCubeboxWithStatusForTest("sb-busy-apply", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
		PausingAt: time.Now().UnixNano(),
		Unknown:   true,
		Reason:    pauseFailedReason,
		Message:   "pause failed",
	})
	stampPauseSnapshotID(sb, aborted)

	rollBackLocalPauseAttempt(sb, prev, aborted, true)
	st := sb.GetStatus().Get()
	require.Equal(t, int64(0), st.PausingAt)
	require.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, st.State())
	require.False(t, st.Unknown)
	require.Empty(t, st.Reason)
	require.Equal(t, prev, stampedPauseSnapshotID(sb))
	require.Empty(t, abortedPauseSnapshotID(sb))

	stampPauseSnapshotID(sb, aborted)
	_ = sb.GetStatus().Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.PausingAt = time.Now().UnixNano()
		status.Unknown = true
		status.Reason = pauseFailedReason
		return status, nil
	})
	rollBackLocalPauseAttempt(sb, prev, aborted, false)
	require.Equal(t, prev, stampedPauseSnapshotID(sb))
	require.Equal(t, aborted, abortedPauseSnapshotID(sb))
	require.False(t, sb.GetStatus().Get().Unknown)
}

func TestUpdateWithPauseCowBusyPreflight(t *testing.T) {
	prev := "snap-prev000000000000000000000001"
	attempt := "snap-busy00000000000000000000001"
	sb := newCubeboxWithStatusForTest("sb-busy-preflight", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	})
	stampPauseSnapshotID(sb, prev)
	first := sb.FirstContainer()
	first.Config = &cubebox.ContainerConfig{
		VolumeMounts: []*cubebox.VolumeMounts{{
			Name:          "root",
			ContainerPath: "/",
		}},
	}

	preflightPauseNotBusyForTest = func(context.Context, *cubeboxstore.CubeBox) error {
		return errors.New(pauseBusyRejectMessage)
	}
	t.Cleanup(func() { preflightPauseNotBusyForTest = nil })

	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}
	rsp, err := s.updateWithPauseCow(context.Background(), &cubebox.UpdateCubeSandboxRequest{
		RequestID: "req-busy",
		SandboxID: sb.ID,
		Annotations: map[string]string{
			constants.MasterAnnotationPauseSnapshotID: attempt,
			constants.MasterAnnotationStorageBackend:  "xfs",
		},
	}, sb)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_TaskPauseFailed, rsp.Ret.RetCode)
	require.Contains(t, rsp.Ret.RetMsg, "pause forbidding")
	require.Equal(t, prev, stampedPauseSnapshotID(sb))
	st := sb.GetStatus().Get()
	require.False(t, st.Unknown)
	require.NotEqual(t, pauseFailedReason, st.Reason)
}

func TestUpdateWithPauseCowPreflightUnknownActionFallsThrough(t *testing.T) {
	// Older shims reject CheckPauseAllowed; Pause must still proceed past the
	// cheap preflight instead of failing closed until CubeShim rolls out.
	prev := "snap-prev000000000000000000000001"
	attempt := "snap-oldshim00000000000000000001"
	sb := newCubeboxWithStatusForTest("sb-old-shim-preflight", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	})
	stampPauseSnapshotID(sb, prev)
	first := sb.FirstContainer()
	first.Config = &cubebox.ContainerConfig{
		VolumeMounts: []*cubebox.VolumeMounts{{
			Name:          "root",
			ContainerPath: "/",
		}},
	}

	preflightPauseNotBusyForTest = func(context.Context, *cubeboxstore.CubeBox) error {
		return errors.New("unknown update ext action: CheckPauseAllowed")
	}
	t.Cleanup(func() { preflightPauseNotBusyForTest = nil })

	// getErr stops getCubeboxSnapshotSpec at the store lookup, before the nil
	// containerd client. Reaching that error proves the unknown preflight
	// action was not treated as a busy reject.
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{
		cb:     sb,
		getErr: errors.New("runtime unavailable"),
	}}}
	rsp, err := s.updateWithPauseCow(context.Background(), &cubebox.UpdateCubeSandboxRequest{
		RequestID: "req-old-shim",
		SandboxID: sb.ID,
		Annotations: map[string]string{
			constants.MasterAnnotationPauseSnapshotID: attempt,
			constants.MasterAnnotationStorageBackend:  "xfs",
		},
	}, sb)
	require.NoError(t, err)
	require.NotEqual(t, errorcode.ErrorCode_TaskPauseFailed, rsp.Ret.RetCode)
	require.Contains(t, rsp.Ret.RetMsg, "failed to get cubebox")
	require.NotContains(t, rsp.Ret.RetMsg, "CheckPauseAllowed")
	require.Equal(t, attempt, stampedPauseSnapshotID(sb), "fall-through keeps the in-flight stamp")
}

func TestGetCubeboxSnapshotSpecRejectsUninitializedDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		svc  *service
		want string
	}{
		{name: "nil service", want: "cubebox manager is not initialized"},
		{name: "nil local manager", svc: &service{}, want: "cubebox manager is not initialized"},
		{
			name: "nil cubebox store",
			svc:  &service{cubeboxMgr: &local{}},
			want: "cubebox manager is not initialized",
		},
		{
			name: "nil containerd client",
			svc:  &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{}}},
			want: "containerd client is not initialized",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.svc.getCubeboxSnapshotSpec(context.Background(), "sandbox")
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestClearLocalPauseAttemptRestoresRunning(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() { clearLocalPauseAttempt(nil) })

	sb := newCubeboxWithStatusForTest("sb-busy", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
		PausingAt: time.Now().UnixNano(),
		PausedAt:  time.Now().UnixNano(),
		Unknown:   true,
		Reason:    "PauseFailed",
		Message:   "pause failed",
	})
	clearLocalPauseAttempt(sb)
	st := sb.GetStatus().Get()
	require.Equal(t, int64(0), st.PausingAt)
	require.Equal(t, int64(0), st.PausedAt)
	require.False(t, st.Unknown)
	require.Empty(t, st.Reason)
	require.Empty(t, st.Message)
}

func TestRestorePauseSnapshotStamp(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() { restorePauseSnapshotStamp(nil, "snap-x") })

	prev := "snap-prev000000000000000000000001"
	aborted := "snap-abort00000000000000000000001"
	sb := newCubeboxWithStatusForTest("sb-stamp-restore", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	})
	stampPauseSnapshotID(sb, prev)
	stampPauseSnapshotID(sb, aborted)
	require.Equal(t, aborted, stampedPauseSnapshotID(sb))

	restorePauseSnapshotStamp(sb, prev)
	require.Equal(t, prev, stampedPauseSnapshotID(sb))

	stampPauseSnapshotID(sb, aborted)
	restorePauseSnapshotStamp(sb, "")
	require.Empty(t, stampedPauseSnapshotID(sb))
	require.NotContains(t, sb.Labels, constants.MasterAnnotationPauseSnapshotID)
	require.NotContains(t, sb.Annotations, constants.MasterAnnotationPauseSnapshotID)
}

func TestMarkPauseDestroyPending(t *testing.T) {
	t.Parallel()
	if markPauseDestroyPending(nil, "request-1") {
		t.Fatal("nil sandbox must not be marked")
	}

	sb := newCubeboxWithStatusForTest("sb-pause-destroy", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	})
	if !markPauseDestroyPending(sb, "request-1") {
		t.Fatal("first mark must report a state change")
	}
	if sb.UserMarkDeletedTime == nil || sb.DeleteRequestID != "request-1" {
		t.Fatalf("delete marker not populated: time=%v request=%q", sb.UserMarkDeletedTime, sb.DeleteRequestID)
	}
	first := sb.UserMarkDeletedTime
	if markPauseDestroyPending(sb, "request-2") {
		t.Fatal("second mark must not request another store sync")
	}
	if sb.UserMarkDeletedTime != first || sb.DeleteRequestID != "request-1" {
		t.Fatal("idempotent mark changed the original delete marker")
	}
}

func TestReplacedLivePauseSnapshotID(t *testing.T) {
	t.Parallel()
	if got := replacedLivePauseSnapshotID("", "snap-b00000000000000000000000001"); got != "" {
		t.Fatalf("first pause has no previous live snap, got %q", got)
	}
	if got := replacedLivePauseSnapshotID("snap-a00000000000000000000000001", "snap-a00000000000000000000000001"); got != "" {
		t.Fatalf("same id is not a replacement, got %q", got)
	}
	if got := replacedLivePauseSnapshotID("snap-a00000000000000000000000001", "snap-b00000000000000000000000001"); got != "snap-a00000000000000000000000001" {
		t.Fatalf("resume leftover should GC after next pause, got %q", got)
	}
}

func TestPauseSnapshotIDForGCCatalogMissUsesPauseLabel(t *testing.T) {
	t.Parallel()
	snapID := "snap-deadbeefdeadbeefdeadbeef"
	sb := newCubeboxWithStatusForTest("sb-unknown", cubeboxstore.Status{Unknown: true})
	stampPauseSnapshotID(sb, snapID)
	if got := pauseSnapshotIDForGC(sb); got != snapID {
		t.Fatalf("pauseSnapshotIDForGC=%q want %s (half-finished pause has no catalog)", got, snapID)
	}
}

func TestPauseSnapshotIDForGCIgnoresRuntimeSnapshotWithoutCatalog(t *testing.T) {
	t.Parallel()
	sb := newCubeboxWithStatusForTest("sb-runtime", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	sb.AddLabels(map[string]string{
		constants.MasterAnnotationRuntimeSnapshotID: "snap-usercommit00000000000001",
	})
	if got := pauseSnapshotIDForGC(sb); got != "" {
		t.Fatalf("must not GC a runtime/commit snap on catalog miss, got %q", got)
	}
}

func TestPauseSnapIDToGCOnDestroyUnknown(t *testing.T) {
	t.Parallel()
	snapID := "snap-deadbeefdeadbeefdeadbeef"
	sb := newCubeboxWithStatusForTest("sb-gc-unknown", cubeboxstore.Status{Unknown: true})
	stampPauseSnapshotID(sb, snapID)

	if got := pauseSnapIDToGCOnDestroy(&cubebox.DestroyCubeSandboxRequest{SandboxID: sb.ID}, sb); got != snapID {
		t.Fatalf("unknown delete should GC half-finished pause snap, got %q", got)
	}

	keep := &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sb.ID,
		Annotations: map[string]string{
			constants.AnnotationPauseKeepTombstone: "true",
		},
	}
	if got := pauseSnapIDToGCOnDestroy(keep, sb); got != "" {
		t.Fatalf("keep_tombstone must not GC, got %q", got)
	}

	tomb := &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sb.ID,
		Annotations: map[string]string{
			constants.AnnotationPauseDeleteTombstone: "true",
		},
	}
	if got := pauseSnapIDToGCOnDestroy(tomb, sb); got != "" {
		t.Fatalf("delete_tombstone must not GC, got %q", got)
	}
}

func TestPauseSnapIDToGCOnDestroySkipsPaused(t *testing.T) {
	t.Parallel()
	sb := newCubeboxWithStatusForTest("sb-gc-paused", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	})
	stampPauseSnapshotID(sb, "snap-paused000000000000000001")
	if got := pauseSnapIDToGCOnDestroy(&cubebox.DestroyCubeSandboxRequest{SandboxID: sb.ID}, sb); got != "" {
		t.Fatalf("PAUSED destroy without tombstone flags must not GC, got %q", got)
	}
}

func TestPauseSnapIDToGCOnDestroyPausingAndRunning(t *testing.T) {
	t.Parallel()
	snapID := "snap-pausing00000000000000001"
	pausing := newCubeboxWithStatusForTest("sb-gc-pausing", cubeboxstore.Status{
		PausingAt: time.Now().UnixNano(),
	})
	stampPauseSnapshotID(pausing, snapID)
	if got := pauseSnapIDToGCOnDestroy(&cubebox.DestroyCubeSandboxRequest{SandboxID: pausing.ID}, pausing); got != snapID {
		t.Fatalf("PAUSING delete should GC in-progress pause snap, got %q", got)
	}

	running := newCubeboxWithStatusForTest("sb-gc-running", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	})
	stampPauseSnapshotID(running, "snap-leftover0000000000000001")
	if got := pauseSnapIDToGCOnDestroy(&cubebox.DestroyCubeSandboxRequest{SandboxID: running.ID}, running); got != "snap-leftover0000000000000001" {
		t.Fatalf("RUNNING leftover pause snap should still GC, got %q", got)
	}
}

func TestPauseCatalogBackendPrefersAnnotationThenLabel(t *testing.T) {
	t.Parallel()
	sb := newCubeboxWithStatusForTest("sb-be", cubeboxstore.Status{})
	if got := pauseCatalogBackend(sb); got != "" {
		t.Fatalf("empty backend=%q", got)
	}
	sb.AddLabels(map[string]string{constants.MasterAnnotationStorageBackend: "s3"})
	if got := pauseCatalogBackend(sb); got != "s3" {
		t.Fatalf("label backend=%q", got)
	}
	sb.AddAnnotations(map[string]string{constants.MasterAnnotationStorageBackend: "xfs"})
	if got := pauseCatalogBackend(sb); got != "xfs" {
		t.Fatalf("annotation should win, got %q", got)
	}
}

func TestNewPauseSnapshotConfigCarriesSnapshotType(t *testing.T) {
	t.Parallel()
	cfg := newPauseSnapshotConfig("/data/snap/pause-1", "file:///dev/cubecow/mem1", snapshotTypeSoftDirty)
	if cfg.DestinationURL != "/data/snap/pause-1" {
		t.Fatalf("dest=%q", cfg.DestinationURL)
	}
	if cfg.MemoryVolURL == nil || *cfg.MemoryVolURL != "file:///dev/cubecow/mem1" {
		t.Fatalf("memory_vol=%v", cfg.MemoryVolURL)
	}
	if cfg.SnapshotType != snapshotTypeSoftDirty {
		t.Fatalf("snapshot_type=%q", cfg.SnapshotType)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got pauseSnapshotConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SnapshotType != snapshotTypeSoftDirty {
		t.Fatalf("roundtrip snapshot_type=%q", got.SnapshotType)
	}

	empty := newPauseSnapshotConfig("/data/snap/pause-1", "", "")
	if empty.MemoryVolURL != nil {
		t.Fatalf("empty mem url should omit pointer, got %v", empty.MemoryVolURL)
	}
	if empty.SnapshotType != snapshotTypeFull {
		t.Fatalf("empty type must default to full, got %q", empty.SnapshotType)
	}
}

func TestKeepLivePausePackage(t *testing.T) {
	t.Parallel()
	snap := "snap-keeppause0000000000000001"
	running := newCubeboxWithStatusForTest("sb-run", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(running, snap)
	running.BindRestoreBase(snap)
	if !keepLivePausePackage([]*cubeboxstore.CubeBox{running}, snap) {
		t.Fatal("running resume must keep the pause package")
	}

	paused := newCubeboxWithStatusForTest("sb-paused", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(paused, snap)
	paused.BindRestoreBase(snap)
	if keepLivePausePackage([]*cubeboxstore.CubeBox{paused}, snap) {
		t.Fatal("PAUSED DelPaused must be allowed to delete the package")
	}

	other := newCubeboxWithStatusForTest("sb-other", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(other, "snap-other00000000000000000001")
	other.BindRestoreBase("snap-other00000000000000000001")
	if keepLivePausePackage([]*cubeboxstore.CubeBox{other}, snap) {
		t.Fatal("unrelated sandbox must not pin this package")
	}

	forged := newCubeboxWithStatusForTest("sb-forged", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	forged.AddAnnotations(map[string]string{constants.MasterAnnotationPauseSnapshotID: snap})
	if keepLivePausePackage([]*cubeboxstore.CubeBox{forged}, snap) {
		t.Fatal("user Create annotation must not pin a pause package")
	}

	forgedLabel := newCubeboxWithStatusForTest("sb-forged-label", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(forgedLabel, snap)
	if keepLivePausePackage([]*cubeboxstore.CubeBox{forgedLabel}, snap) {
		t.Fatal("pause-id Label without restore-base must not pin another tenant's package")
	}

	nextPause := newCubeboxWithStatusForTest("sb-next", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(nextPause, "snap-new000000000000000000000001")
	nextPause.BindRestoreBase(snap)
	if !keepLivePausePackage([]*cubeboxstore.CubeBox{nextPause}, snap) {
		t.Fatal("restore-base must keep the previous package while Pause stamps a new id")
	}
}

func TestStripUserCubeMasterLabels(t *testing.T) {
	t.Parallel()
	got := stripUserCubeMasterLabels(map[string]string{
		constants.MasterAnnotationPauseSnapshotID:          "snap-forged",
		constants.MasterAnnotationPauseAbortedSnapshotID:   "snap-aborted",
		constants.MasterAnnotationRuntimeRestoreSnapshotID: "snap-restore",
		"app": "ok",
	})
	if _, ok := got[constants.MasterAnnotationPauseSnapshotID]; ok {
		t.Fatal("forged pause id must be stripped from Create Labels")
	}
	if _, ok := got[constants.MasterAnnotationPauseAbortedSnapshotID]; ok {
		t.Fatal("forged aborted pause id must be stripped from Create Labels")
	}
	if _, ok := got[constants.MasterAnnotationRuntimeRestoreSnapshotID]; ok {
		t.Fatal("forged restore-base must be stripped from Create Labels")
	}
	if got["app"] != "ok" {
		t.Fatalf("ordinary labels must pass through, got %#v", got)
	}
}

func TestPersistPauseStageTimings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := &pauseStageTimings{
		SnapshotID:          "snap-timing000000000000000001",
		SandboxID:           "sandbox-timing",
		Backend:             "jfs",
		MemoryCloneMS:       12.5,
		VMMPauseWriteSyncMS: 34.75,
		CubeletTotalMS:      56.25,
		RecordedAt:          time.Now().UTC().Format(time.RFC3339Nano),
	}
	require.NoError(t, persistPauseStageTimings(dir, want))
	body, err := os.ReadFile(filepath.Join(dir, want.SnapshotID+".json"))
	require.NoError(t, err)
	var got pauseStageTimings
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, *want, got)
}

func TestShouldKeepLivePausePackageHonorLive(t *testing.T) {
	t.Parallel()
	snap := "snap-keepgate00000000000000001"
	running := newCubeboxWithStatusForTest("sb-s3-live", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(running, snap)
	running.BindRestoreBase(snap)
	boxes := []*cubeboxstore.CubeBox{running}

	if !shouldKeepLivePausePackage(true, boxes, snap, "pause_snapshot") {
		t.Fatal("Master Resume Cleanup of a live S3/XFS pause package must no-op")
	}
	if shouldKeepLivePausePackage(false, boxes, snap, "pause_snapshot") {
		t.Fatal("Cubelet next-Pause / Destroy GC must still delete")
	}
	if shouldKeepLivePausePackage(true, boxes, snap, "snapshot") {
		t.Fatal("customer snap kind must not be pinned by a pause label")
	}
	if shouldKeepLivePausePackage(true, boxes, snap, "") {
		t.Fatal("missing catalog kind must not keep")
	}
}

func TestIsPauseSnapshotCatalogKind(t *testing.T) {
	t.Parallel()
	if !isPauseSnapshotCatalogKind("pause_snapshot") {
		t.Fatal("pause_snapshot must keep")
	}
	if isPauseSnapshotCatalogKind("snapshot") || isPauseSnapshotCatalogKind("template") || isPauseSnapshotCatalogKind("") {
		t.Fatal("non-pause kinds must not keep")
	}
}
