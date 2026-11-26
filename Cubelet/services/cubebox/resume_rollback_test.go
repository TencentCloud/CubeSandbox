// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"
	sandboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/sandbox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/rollback"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func testRollbackCoordinator(t *testing.T, s *service, dir string) *rollback.Coordinator {
	t.Helper()
	c, err := rollback.New(dir, rollback.Options{Context: t.Context(), Timeout: s.config.destroyDeadline, Execute: s.executeResumeRollback})
	require.NoError(t, err)
	s.rollbacks = c
	return c
}

func TestRecoverSkipsResumeRollbackPending(t *testing.T) {
	cb := restartBox(t, "sandbox-pending", restartpolicy.OnFailure, restartpolicy.StateRunning,
		cubeboxstore.Status{StartedAt: time.Now().Add(-time.Hour).UnixNano()})
	store := newMemBoxSet(cb)
	s, _, _ := newDestroyLifecycleServiceForTest(nil)
	c := testRollbackCoordinator(t, s, t.TempDir())
	_, err := c.Prepare(cb.ID, namespaces.Default, "failed-resume")
	require.NoError(t, err)
	m := newRestartMgr(store)
	m.bind(s)
	t.Cleanup(func() { m.Stop(cb.ID) })
	m.Recover()
	m.mu.Lock()
	_, armed := m.runs[cb.ID]
	m.mu.Unlock()
	require.False(t, armed, "recovery must not restart a sandbox owned by rollback cleanup")
	require.Empty(t, store.snapshots(), "recovery must not rewrite pending rollback metadata")
	require.True(t, c.Pending(cb.ID))
}

func TestResumeRetryWaitsForFailedAttemptCleanup(t *testing.T) {
	id := "sandbox-resume"
	flow := &resumeRollbackTestFlow{createErr: errors.New("guest handshake timed out"), started: make(chan *workflow.DestroyContext, 4)}
	s, _, _ := newDestroyLifecycleServiceForTest(nil)
	s.config.destroyDeadline = 50 * time.Millisecond
	c := testRollbackCoordinator(t, s, t.TempDir())
	s.engine.AddCleaupFlow(flow)
	for _, name := range []string{"create", "destroy"} {
		s.engine.AddFlow(name, &workflow.Workflow{Name: name, Limiter: semaphore.NewLimiter(1), Steps: []*workflow.Step{{Name: flow.ID(), Actions: []workflow.Flow{flow}}}})
	}
	req := resumeReplaceRequest(id)
	req.Annotations[constants.MasterAnnotationPauseSnapshotID] = "snap-original"
	task, err := c.Prepare(id, namespaces.Default, "first-resume")
	require.NoError(t, err)
	ctx := namespaces.WithNamespace(t.Context(), namespaces.Default)
	ctx = CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{RequestID: "first-resume"})
	create := &workflow.CreateContext{ReqInfo: req, Failover: true, RollbackCoordinator: s}
	unlock := s.sandboxLifecycleLocks.Lock(id)
	err = s.engine.Create(ctx, create)
	require.ErrorIs(t, err, flow.createErr)
	require.True(t, c.Pending(id), "gate must be published before create unlocks")
	unlock()
	select {
	case opts := <-flow.started:
		require.Same(t, task, opts.Rollback)
		require.True(t, opts.IsRollBack)
		require.Empty(t, opts.DestroyInfo.GetAnnotations())
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start")
	}
	rsp, err := s.Create(ctx, req)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_ResumeRollbackPending, rsp.GetRet().GetRetCode())
	list, err := s.List(ctx, &cubeboxpb.ListCubeSandboxRequest{Id: &id})
	require.NoError(t, err)
	require.Empty(t, list.GetItems())
	require.Eventually(t, func() bool { return task.Snapshot().Failures == 1 }, time.Second, time.Millisecond)
	flow.allowDestroy.Store(true)
	require.Eventually(t, func() bool { return c.Join(t.Context(), id) == nil && !c.Pending(id) }, time.Second, time.Millisecond)
	require.Zero(t, flow.createFallbackCalls)
}

func TestResumeRollbackEvidenceRecoversWithoutBundleOrCubeBoxRow(t *testing.T) {
	identity := startSleeper(t)
	bundle := t.TempDir()
	withShimBundles(t, bundle)
	s, _, _ := newDestroyLifecycleServiceForTest(nil)
	dir := t.TempDir()
	c := testRollbackCoordinator(t, s, dir)
	task, err := c.Prepare("sandbox", namespaces.Default, "request")
	require.NoError(t, err)
	_, err = task.Remember(rollback.Evidence{
		Identities: []rollback.Identity{{PID: identity.Pid, StartTime: identity.StartTime}},
		Unresolved: []rollback.Candidate{{PID: identity.Pid, Source: holderSourceBundle, Trusted: true}},
	})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(bundle))
	// Recreate the coordinator from disk, with neither original metadata source.
	recovered := testRollbackCoordinator(t, s, dir)
	task = recovered.Task("sandbox")
	require.NotNil(t, task)
	previous := readRuntimeProcessIdentity
	t.Cleanup(func() { readRuntimeProcessIdentity = previous })
	readRuntimeProcessIdentity = func(int) (utils.ProcessIdentity, error) { return utils.ProcessIdentity{}, os.ErrPermission }
	ev, err := (&local{}).collectRollbackEvidence(t.Context(), nil, task)
	require.NoError(t, err)
	require.Empty(t, ev.unresolved, "a saved identity must take precedence over the duplicate bare PID")
	require.Empty(t, task.Snapshot().Evidence.Unresolved)
	readRuntimeProcessIdentity = previous
	ev, err = (&local{}).collectRollbackEvidence(t.Context(), nil, task)
	require.NoError(t, err)
	require.Empty(t, ev.unresolved)
	require.Len(t, ev.identities, 1)
	require.Equal(t, identity.Pid, ev.identities[0].Pid)
	require.Equal(t, identity.StartTime, ev.identities[0].StartTime)
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	require.Error(t, waitSandboxRuntimeGone(ctx, "sandbox", ev), "a live recovered VMM must block resource release")
}

func TestRecoveredRollbackWithoutCubeBoxRowKeepsUnknownEvidence(t *testing.T) {
	s, _, _ := newDestroyLifecycleServiceForTest(nil)
	dir := t.TempDir()
	c := testRollbackCoordinator(t, s, dir)
	task, err := c.Prepare("sandbox-missing-row", namespaces.Default, "request")
	require.NoError(t, err)
	unknownAge := time.Time{}
	saved := rollback.Evidence{
		Unresolved:  []rollback.Candidate{{PID: 123456, Source: holderSourceBundle, Trusted: true}},
		SpawnIntent: &unknownAge,
	}
	_, err = task.Remember(saved)
	require.NoError(t, err)
	recovered := testRollbackCoordinator(t, s, dir)
	task = recovered.Task("sandbox-missing-row")
	require.NotNil(t, task)
	previous := readRuntimeProcessIdentity
	t.Cleanup(func() { readRuntimeProcessIdentity = previous })
	readRuntimeProcessIdentity = func(int) (utils.ProcessIdentity, error) {
		return utils.ProcessIdentity{}, os.ErrPermission
	}
	var ev sandboxRuntimeEvidence
	require.NotPanics(t, func() {
		ev, err = (&local{}).collectRollbackEvidence(t.Context(), nil, task)
	})
	require.NoError(t, err)
	require.NotEmpty(t, ev.unresolved)
	require.ErrorContains(t, waitSandboxRuntimeGone(t.Context(), "sandbox-missing-row", ev), "runtime state unresolved")
	require.Equal(t, saved, task.Snapshot().Evidence, "missing metadata must not discard unknown process/intent evidence")
	require.True(t, recovered.Pending("sandbox-missing-row"))
}

func TestRecoveredRollbackDoesNotAdoptReusedPID(t *testing.T) {
	current := startSleeper(t)
	old := rollback.Identity{PID: current.Pid, StartTime: current.StartTime + 1}
	for _, source := range []string{"bundle", "status"} {
		t.Run(source, func(t *testing.T) {
			bundle := t.TempDir()
			withShimBundles(t, bundle)
			var sb *cubeboxstore.CubeBox
			if source == "bundle" {
				writeBundlePidFile(t, bundle, shimPidFileName, current.Pid)
			} else {
				sb = newCubeboxWithStatusForTest("sandbox", cubeboxstore.Status{Pid: uint32(current.Pid)})
			}
			s, _, _ := newDestroyLifecycleServiceForTest(nil)
			dir := t.TempDir()
			c := testRollbackCoordinator(t, s, dir)
			task, err := c.Prepare("sandbox", namespaces.Default, "request")
			require.NoError(t, err)
			_, err = task.Remember(rollback.Evidence{Identities: []rollback.Identity{old}})
			require.NoError(t, err)
			// A different start time models a recycled PID after restart, while
			// the bare bundle/status entry still points at that number.
			recovered := testRollbackCoordinator(t, s, dir)
			task = recovered.Task("sandbox")
			ev, err := (&local{}).collectRollbackEvidence(t.Context(), sb, task)
			require.NoError(t, err)
			require.Empty(t, ev.unresolved)
			require.Equal(t, []rollback.Identity{old}, task.Snapshot().Evidence.Identities,
				"must not persist the unrelated process as a new holder")
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			require.NoError(t, waitSandboxRuntimeGone(ctx, "sandbox", ev))
			require.Equal(t, utils.LivenessAlive, current.Status(), "cleanup must not affect the unrelated process")
		})
	}
}

func TestRecoveredRollbackPreservesUnknownSpawnIntentAge(t *testing.T) {
	s, _, _ := newDestroyLifecycleServiceForTest(nil)
	c := testRollbackCoordinator(t, s, t.TempDir())
	task, err := c.Prepare("sandbox", namespaces.Default, "request")
	require.NoError(t, err)
	unknown := time.Time{}
	_, err = task.Remember(rollback.Evidence{SpawnIntent: &unknown})
	require.NoError(t, err)
	sb := newCubeboxWithStatusForTest("sandbox", cubeboxstore.Status{})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now()}
	ev, err := localWithIntentTTL(time.Minute).collectRollbackEvidence(t.Context(), sb, task)
	require.NoError(t, err)
	require.NotNil(t, task.Snapshot().Evidence.SpawnIntent)
	require.True(t, task.Snapshot().Evidence.SpawnIntent.IsZero(), "metadata backfill must not replace durable unknown age")
	require.Error(t, waitSandboxRuntimeGone(t.Context(), sb.ID, ev))
}

func TestResumeRollbackEvidenceWriteFailureStopsTeardown(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sandbox", cubeboxstore.Status{StartedAt: 1})
	s, _, _ := newDestroyLifecycleServiceForTest(sb)
	dir := filepath.Join(t.TempDir(), "journal")
	c := testRollbackCoordinator(t, s, dir)
	task, err := c.Prepare(sb.ID, namespaces.Default, "request")
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.WriteFile(dir, nil, 0o600))
	opts := &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sb.ID, IsRollBack: true},
		DestroyInfo:      &cubeboxpb.DestroyCubeSandboxRequest{SandboxID: sb.ID}, Rollback: task,
	}
	// cbriManager is intentionally absent: failed journal persistence must
	// return before DestroySandbox can remove the original PID evidence.
	require.NotPanics(t, func() {
		err = s.cubeboxMgr.Destroy(t.Context(), opts)
	})
	require.ErrorContains(t, err, "persist rollback runtime evidence")
	require.True(t, c.Pending(sb.ID))
}

func TestDeleteJoinsResumeRollbackAndContinuesTombstoneDelete(t *testing.T) {
	s, manager, flow := newDestroyLifecycleServiceForTest(nil)
	manager.getErr = utils.ErrorKeyNotFound
	c := testRollbackCoordinator(t, s, t.TempDir())
	_, err := c.Prepare("sandbox", namespaces.Default, "request")
	require.NoError(t, err)
	rsp, err := s.Destroy(t.Context(), &cubeboxpb.DestroyCubeSandboxRequest{
		SandboxID: "sandbox", RequestID: "delete",
		Annotations: map[string]string{constants.AnnotationPauseDeleteTombstone: "true"},
	})
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, rsp.GetRet().GetRetCode())
	require.False(t, c.Pending("sandbox"))
	require.Equal(t, 1, flow.destroyCalls)
}

func TestDeleteDoesNotJoinRollbackWhenFilterDoesNotMatch(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sandbox", cubeboxstore.Status{StartedAt: 1})
	sb.Labels = map[string]string{"owner": "alice"}
	s, _, flow := newDestroyLifecycleServiceForTest(sb)
	c := testRollbackCoordinator(t, s, t.TempDir())
	_, err := c.Prepare(sb.ID, namespaces.Default, "request")
	require.NoError(t, err)
	rsp, err := s.Destroy(t.Context(), &cubeboxpb.DestroyCubeSandboxRequest{
		SandboxID: sb.ID, Filter: &cubeboxpb.CubeSandboxFilter{LabelSelector: map[string]string{"owner": "bob"}},
	})
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_PreConditionFailed, rsp.GetRet().GetRetCode())
	require.Zero(t, flow.destroyCalls)
	require.True(t, c.Pending(sb.ID))
}

func TestDeleteJoinReservesTimeForRPCResponse(t *testing.T) {
	s, _, flow := newDestroyLifecycleServiceForTest(nil)
	c := testRollbackCoordinator(t, s, t.TempDir())
	task, err := c.Prepare("sandbox", namespaces.Default, "request")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), deleteLifecycleResponseReserve/2)
	defer cancel()
	require.ErrorIs(t, s.joinResumeRollback(ctx, "sandbox"), context.DeadlineExceeded)
	require.True(t, task.Snapshot().Preparing, "insufficient response budget must not start a takeover")
	require.Zero(t, flow.destroyCalls)
}

func TestResumeRollbackSchedulingFailuresDoNotConsumeBudget(t *testing.T) {
	for _, busy := range []string{"lifecycle lock", "destroy limiter"} {
		t.Run(busy, func(t *testing.T) {
			s, _, flow := newDestroyLifecycleServiceForTest(nil)
			s.config.destroyDeadline = 30 * time.Millisecond
			c := testRollbackCoordinator(t, s, t.TempDir())
			task, err := c.Prepare("sandbox", namespaces.Default, "request")
			require.NoError(t, err)
			if busy == "lifecycle lock" {
				unlock := s.sandboxLifecycleLocks.Lock("sandbox")
				defer unlock()
			} else {
				limiter := semaphore.NewLimiter(1)
				require.True(t, limiter.TryAcquire())
				defer limiter.Release()
				s.engine.AddFlow("destroy", &workflow.Workflow{Name: "destroy", Limiter: limiter})
			}
			for range 2 {
				require.Error(t, c.Join(t.Context(), "sandbox"))
			}
			require.Zero(t, task.Snapshot().Failures)
			require.True(t, task.Snapshot().NextRetry.IsZero())
			require.Zero(t, flow.destroyCalls)
		})
	}
}

func TestResumeCoordinatorDeclinesAnotherSandbox(t *testing.T) {
	s, _, flow := newDestroyLifecycleServiceForTest(nil)
	c := testRollbackCoordinator(t, s, t.TempDir())
	task, err := c.Prepare("expected", namespaces.Default, "request")
	require.NoError(t, err)
	req := resumeReplaceRequest("expected")
	req.Annotations[constants.MasterAnnotationPauseSnapshotID] = "snap-original"
	owned := s.Schedule(t.Context(), &workflow.CreateContext{ReqInfo: req, BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "another"}})
	require.False(t, owned)
	require.True(t, task.Snapshot().Preparing)
	require.Zero(t, flow.destroyCalls)
}

func TestCleanupDeferredDoesNotHidePermanentErrors(t *testing.T) {
	pending := &shimIntentPendingError{sandboxID: "sandbox"}
	require.True(t, cleanupDeferred(pending))
	require.True(t, cleanupDeferred(statusErr(errorcode.ErrorCode_RemoveContainerFailed, pending)))
	require.False(t, cleanupDeferred(errors.Join(pending, errors.New("storage failed"))))
}

type resumeRollbackTestFlow struct {
	fakeCreateFlow
	createErr           error
	started             chan *workflow.DestroyContext
	allowDestroy        atomic.Bool
	createFallbackCalls int
}

func (f *resumeRollbackTestFlow) Create(_ context.Context, opts *workflow.CreateContext) error {
	if opts.ReqInfo == nil {
		f.createFallbackCalls++
		return nil
	}
	opts.SandboxID = resumeFromPauseSandboxID(opts.ReqInfo)
	return f.createErr
}

func (f *resumeRollbackTestFlow) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	f.started <- opts
	if f.allowDestroy.Load() {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

type testRollbackScheduler func(context.Context, *workflow.CreateContext) bool

func (f testRollbackScheduler) Schedule(ctx context.Context, opts *workflow.CreateContext) bool {
	return f(ctx, opts)
}

func TestResumeRollbackMissingRowStillPersistsRuntimeEvidence(t *testing.T) {
	s, manager, _ := newDestroyLifecycleServiceForTest(nil)
	manager.getErr = utils.ErrorKeyNotFound
	dir := filepath.Join(t.TempDir(), "journal")
	c := testRollbackCoordinator(t, s, dir)
	task, err := c.Prepare("sandbox-missing-row", namespaces.Default, "request")
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.WriteFile(dir, nil, 0o600))
	opts := &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "sandbox-missing-row", IsRollBack: true},
		DestroyInfo:      &cubeboxpb.DestroyCubeSandboxRequest{SandboxID: "sandbox-missing-row"},
		Rollback:         task,
	}
	// Missing metadata must not bypass durable evidence collection and report
	// cleanup success. A failed evidence write keeps the coordinator pending.
	require.ErrorContains(t, s.cubeboxMgr.Destroy(t.Context(), opts), "persist rollback runtime evidence")
	require.True(t, c.Pending("sandbox-missing-row"))
}
