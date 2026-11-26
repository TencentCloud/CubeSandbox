// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type duplicateAdmissionFlow struct {
	workflow.Flow
	local     *local
	destroyed chan struct{}
}

func (f *duplicateAdmissionFlow) ID() string { return "admission" }
func (f *duplicateAdmissionFlow) Create(ctx context.Context, opts *workflow.CreateContext) error {
	return f.local.admitAndSaveWithQuota(ctx, admissionBox(opts.SandboxID, "1", "1Gi"), &config.HostConf{}, 2000, 2048)
}
func (f *duplicateAdmissionFlow) Destroy(context.Context, *workflow.DestroyContext) error {
	f.destroyed <- struct{}{}
	return nil
}

func TestDuplicateAdmissionDoesNotDestroyWinner(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	winner := admissionBox("same", "1", "1Gi")
	store := &admissionStore{boxes: []*cubeboxstore.CubeBox{winner}}
	flow := &duplicateAdmissionFlow{local: &local{cubeboxManger: store}, destroyed: make(chan struct{}, 1)}
	engine := &workflow.Engine{}
	for _, name := range []string{"create", "destroy"} {
		engine.AddFlow(name, &workflow.Workflow{Name: name, Limiter: semaphore.NewLimiter(1), Steps: []*workflow.Step{{Name: "admission", Actions: []workflow.Flow{flow}}}})
	}
	ctx := namespaces.WithNamespace(context.Background(), "test")
	ctx = CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{RequestID: "duplicate-create"})
	err = engine.Create(ctx, &workflow.CreateContext{BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "same"}, Failover: true, CubeBoxCreated: true})
	require.True(t, ret.IsErrorCode(err, errorcode.ErrorCode_PreConditionFailed), "%v", err)
	select {
	case <-flow.destroyed:
		t.Fatal("losing create destroyed the existing sandbox")
	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, []*cubeboxstore.CubeBox{winner}, store.boxes)
}

type blockedAdmissionResumeTask struct {
	fakeResumeTask
	entered chan struct{}
	release chan struct{}
}

func (f *blockedAdmissionResumeTask) Resume(ctx context.Context) error {
	close(f.entered)
	select {
	case <-f.release:
		return f.resumeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestResumeAdmissionFencesConcurrentCreate(t *testing.T) {
	for _, outcome := range []string{"running", "still-paused", "reconciled-running"} {
		t.Run(outcome, func(t *testing.T) {
			_, err := config.Init("", true)
			require.NoError(t, err)
			host := config.GetHostConf()
			before := host.Quota
			t.Cleanup(func() { host.Quota = before })
			host.Quota.Cpu, host.Quota.Mem, host.Quota.PausedResourceReleaseRatio = 1000, "1Gi", 1
			sb := sandboxWithResourceForTest("paused", cubeboxstore.Status{PausedAt: time.Now().UnixNano()}, "1", "1Gi", 0, 0, 0)
			task := &blockedAdmissionResumeTask{entered: make(chan struct{}), release: make(chan struct{})}
			if outcome != "running" {
				task.resumeErr = errors.New("resume RPC failed")
				task.status = containerd.Paused
			}
			if outcome == "reconciled-running" {
				task.status = containerd.Running
			}
			sb.FirstContainer().Container = &fakeDestroyContainer{task: task}
			store := &admissionStore{boxes: []*cubeboxstore.CubeBox{sb}}
			l := &local{cubeboxManger: store}
			s := &service{cubeboxMgr: l}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			resumed := make(chan resumeResult, 1)
			go func() {
				resumed <- s.resumeLocked(ctx, sb, resumeOptions{taskDeadline: deadline, reconcileDeadline: deadline})
			}()
			select {
			case <-task.entered:
			case <-ctx.Done():
				t.Fatal("resume did not reach the runtime")
			}
			created := make(chan error, 1)
			go func() { created <- l.admitAndSave(ctx, admissionBox("new", "1", "1Gi")) }()
			select {
			case err := <-created:
				t.Fatalf("create escaped the in-flight resume fence: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			close(task.release)
			result := <-resumed
			err = <-created
			if outcome == "still-paused" {
				require.False(t, result.running)
				require.NoError(t, err)
			} else {
				require.True(t, result.running)
				require.True(t, ret.IsErrorCode(err, errorcode.ErrorCode_HostResourcesNotEnough), "%v", err)
			}
		})
	}
}
