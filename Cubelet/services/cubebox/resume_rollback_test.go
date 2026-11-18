// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestResumeRetryWaitsForFailedAttemptCleanup(t *testing.T) {
	t.Run("fast retry", func(t *testing.T) { testResumeRetryWaitsForFailedAttemptCleanup(t, false) })
	t.Run("slow retry after exhaustion", func(t *testing.T) { testResumeRetryWaitsForFailedAttemptCleanup(t, true) })
}

func testResumeRetryWaitsForFailedAttemptCleanup(t *testing.T, exhausted bool) {
	t.Helper()
	id := "sandbox-resume"
	failed := errors.New("guest agent connection timed out")
	flow := &resumeRollbackTestFlow{
		createErr: failed,
		started:   make(chan *workflow.DestroyContext, 2),
	}
	s, _, _ := newDestroyLifecycleServiceForTest(nil)
	s.config.destroyDeadline = 100 * time.Millisecond
	s.engine.AddCleaupFlow(flow)
	for _, name := range []string{"create", "destroy"} {
		s.engine.AddFlow(name, &workflow.Workflow{
			Name: name, Limiter: semaphore.NewLimiter(1),
			Steps: []*workflow.Step{{Name: flow.ID(), Actions: []workflow.Flow{flow}}},
		})
	}
	ctx := namespaces.WithNamespace(t.Context(), namespaces.Default)
	ctx = CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{RequestID: "first-resume"})
	req := resumeReplaceRequest(id)
	req.Annotations[constants.MasterAnnotationPauseSnapshotID] = "snap-original"
	create := &workflow.CreateContext{ReqInfo: req, Failover: true}
	create.ScheduleResumeRollback = func() { s.scheduleResumeRollback(ctx, id, create) }

	unlock := s.sandboxLifecycleLocks.Lock(id)
	err := s.engine.Create(ctx, create)
	pending := s.resumeRollbackPending(id)
	attemptValue, _ := s.resumeRollbacks.Load(id)
	attempt := attemptValue.(*resumeRollback)
	if exhausted {
		// Exercise escalation without waiting for sixty real cleanup deadlines.
		attempt.failures.Store(resumeRollbackFastAttempts - 1)
	}
	unlock()
	require.ErrorIs(t, err, failed)
	require.True(t, pending, "the retry gate must be visible before the first attempt unlocks")

	select {
	case opts := <-flow.started:
		require.True(t, opts.IsResumeRollback)
		require.True(t, opts.IsRollBack)
		require.Equal(t, id, opts.SandboxID)
		require.Empty(t, opts.DestroyInfo.GetAnnotations(), "rollback must leave the pause package alone")
	case <-time.After(time.Second):
		t.Fatal("rollback did not start")
	}
	retry, err := s.Create(ctx, req)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_ResumeRollbackPending, retry.GetRet().GetRetCode())
	list, err := s.List(ctx, &cubeboxpb.ListCubeSandboxRequest{Id: &id})
	require.NoError(t, err)
	require.Empty(t, list.GetItems())

	require.Eventually(t, func() bool {
		return !attempt.busy.Load()
	}, time.Second, time.Millisecond)
	require.True(t, s.resumeRollbackPending(id), "a timed-out rollback must remain retryable")
	if exhausted {
		require.Equal(t, int32(resumeRollbackFastAttempts), attempt.failures.Load())
		require.Greater(t, attempt.nextRetry.Load(), time.Now().Add(9*time.Minute).UnixNano())
		s.retryResumeRollbacks()
		require.False(t, attempt.busy.Load(), "exhausted rollbacks must skip the normal ten-second retry loop")
		require.Empty(t, flow.started)
	} else {
		require.Equal(t, int32(1), attempt.failures.Load())
		require.Zero(t, attempt.nextRetry.Load())
	}

	flow.allowDestroy = true
	attempt.nextRetry.Store(0) // Advance past the slow retry interval.
	s.retryResumeRollbacks()
	require.Eventually(t, func() bool {
		return !s.resumeRollbackPending(id)
	}, time.Second, time.Millisecond)
	require.Zero(t, flow.createFallbackCalls, "generic cleanup must not run while the old runtime could be alive")
}

type resumeRollbackTestFlow struct {
	fakeCreateFlow
	createErr           error
	started             chan *workflow.DestroyContext
	allowDestroy        bool
	createFallbackCalls int
}

func TestResumeRollbackKeepsRuntimeIdentityAfterBundleRemoval(t *testing.T) {
	attempt := &resumeRollback{}
	vmm := utils.ProcessIdentity{Pid: 42, StartTime: 100}
	attempt.rememberRuntime(sandboxRuntimeEvidence{identities: []utils.ProcessIdentity{vmm}})

	// Destroy removed the bundle before the first wait timed out.
	next := attempt.rememberRuntime(sandboxRuntimeEvidence{})
	require.Equal(t, []utils.ProcessIdentity{vmm}, next.identities)
}

func TestResumeRollbackRechecksUnresolvedBundlePIDAfterRemoval(t *testing.T) {
	for _, resolution := range []string{"unreadable", "alive", "gone"} {
		t.Run(resolution, func(t *testing.T) {
			bundle := t.TempDir()
			withShimBundles(t, bundle)
			const pid = 424242
			writeBundlePidFile(t, bundle, vmmPidFileName, pid)
			_, identity := rollbackTestProcessIdentity(t, pid)
			previous := readRuntimeProcessIdentity
			t.Cleanup(func() { readRuntimeProcessIdentity = previous })
			readRuntimeProcessIdentity = func(int) (utils.ProcessIdentity, error) {
				return utils.ProcessIdentity{}, os.ErrPermission
			}
			sb := newCubeboxWithStatusForTest("sb-unresolved-vmm", cubeboxstore.Status{})
			l := &local{}
			attempt := &resumeRollback{}
			first := attempt.rememberRuntime(l.collectSandboxRuntimeEvidence(t.Context(), sb))
			require.Error(t, waitSandboxRuntimeGone(t.Context(), sb.ID, first))
			require.Len(t, attempt.unresolvedPIDs, 1)
			require.NoError(t, os.RemoveAll(bundle))

			// The bundle can no longer supply this PID. An unreadable process
			// must still block every retry, without accumulating duplicates.
			for range 2 {
				next := attempt.rememberRuntime(l.collectSandboxRuntimeEvidence(t.Context(), sb, attempt.unresolvedPIDs...))
				require.Error(t, waitSandboxRuntimeGone(t.Context(), sb.ID, next))
				require.Len(t, attempt.unresolvedPIDs, 1)
			}
			readRuntimeProcessIdentity = func(int) (utils.ProcessIdentity, error) {
				switch resolution {
				case "alive":
					return identity, nil
				case "gone":
					return utils.ProcessIdentity{}, os.ErrNotExist
				default:
					return utils.ProcessIdentity{}, os.ErrPermission
				}
			}
			next := attempt.rememberRuntime(l.collectSandboxRuntimeEvidence(t.Context(), sb, attempt.unresolvedPIDs...))
			ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
			defer cancel()
			err := waitSandboxRuntimeGone(ctx, sb.ID, next)
			if resolution == "gone" {
				require.NoError(t, err)
				require.Empty(t, attempt.unresolvedPIDs)
			} else {
				require.Error(t, err, "unknown or live VMM must keep cleanup blocked")
			}
			if resolution == "alive" {
				require.Equal(t, []utils.ProcessIdentity{identity}, attempt.runtimeIdentities)
				require.Empty(t, attempt.unresolvedPIDs)
			}
		})
	}
}

func TestResumeRollbackRetainsUnknownIdentityUntilVerifiable(t *testing.T) {
	for _, resolution := range []string{"alive", "reused", "gone"} {
		t.Run(resolution, func(t *testing.T) {
			const pid = 424242
			stat, identity := rollbackTestProcessIdentity(t, pid)
			require.NoError(t, os.WriteFile(stat, []byte("unreadable stat"), 0o644))
			l := &local{}
			sb := newCubeboxWithStatusForTest("sb-unknown-identity", cubeboxstore.Status{})
			attempt := &resumeRollback{}
			candidate := runtimePIDCandidate{pid: pid, source: holderSourceBundle, trusted: true, identity: &identity}
			first := attempt.rememberRuntime(l.collectSandboxRuntimeEvidence(t.Context(), sb, candidate))
			require.Error(t, waitSandboxRuntimeGone(t.Context(), sb.ID, first))
			require.Len(t, attempt.unresolvedPIDs, 1)
			require.Equal(t, identity, *attempt.unresolvedPIDs[0].identity)

			// Rechecking must use the old identity, not adopt whoever happens
			// to own this PID after the bundle disappears.
			switch resolution {
			case "gone":
				require.NoError(t, os.Remove(stat))
			case "reused":
				writeRollbackTestProcStat(t, stat, pid, identity.StartTime+1)
			case "alive":
				writeRollbackTestProcStat(t, stat, pid, identity.StartTime)
			}
			next := attempt.rememberRuntime(l.collectSandboxRuntimeEvidence(t.Context(), sb, attempt.unresolvedPIDs...))
			require.Empty(t, attempt.unresolvedPIDs)
			ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
			defer cancel()
			err := waitSandboxRuntimeGone(ctx, sb.ID, next)
			if resolution == "alive" {
				require.Error(t, err)
				require.Equal(t, []utils.ProcessIdentity{identity}, next.identities)
			} else {
				require.NoError(t, err)
				require.Empty(t, next.identities)
			}
		})
	}
}

func rollbackTestProcessIdentity(t *testing.T, pid int) (string, utils.ProcessIdentity) {
	t.Helper()
	procDir := t.TempDir()
	dir := filepath.Join(procDir, fmt.Sprint(pid))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	stat := filepath.Join(dir, "stat")
	writeRollbackTestProcStat(t, stat, pid, 123)
	identity, err := utils.ReadProcessIdentityAt(procDir, pid)
	require.NoError(t, err)
	return stat, identity
}

func writeRollbackTestProcStat(t *testing.T, path string, pid int, startTime uint64) {
	t.Helper()
	data := fmt.Sprintf("%d (vmm) S %s%d", pid, strings.Repeat("0 ", 18), startTime)
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))
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
	if f.allowDestroy {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}
