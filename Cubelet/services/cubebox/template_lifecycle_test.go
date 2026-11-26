// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestTemplatePostStartRunsOnNewSandboxFromSnapshot(t *testing.T) {
	command := []string{"/bin/init", "with spaces"}
	process := &templateHookTestProcess{exitCode: 7}
	task := &templateHookTestTask{process: process}
	c := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{Config: &cubeboxpb.ContainerConfig{
			Lifecycle: &cubeboxpb.TemplateLifecycleCommands{PostStart: command},
		}},
		Container: &templateHookTestContainer{task: task},
	}
	annotations := map[string]string{constants.MasterAnnotationAppSnapshotTemplateID: "tpl-test"}
	opts := &workflow.CreateContext{ReqInfo: &cubeboxpb.RunCubeSandboxRequest{
		InstanceType: cubeboxpb.InstanceType_cubebox.String(),
		Annotations:  annotations,
	}}
	ctx := context.Background()
	budget := templatePostStartTimeout

	annotations[constants.MasterAnnotationsAppSnapshotCreate] = "true"
	require.NoError(t, runTemplatePostStart(ctx, opts, c, &budget))
	require.Nil(t, task.spec)
	delete(annotations, constants.MasterAnnotationsAppSnapshotCreate)
	annotations[constants.MasterAnnotationPauseSnapshotID] = "snap-pause"
	require.NotContains(t, annotations, constants.MasterAnnotationDesiredSandboxID)
	require.NoError(t, runTemplatePostStart(ctx, opts, c, &budget))
	require.Nil(t, task.spec)
	delete(annotations, constants.MasterAnnotationPauseSnapshotID)

	require.ErrorContains(t, runTemplatePostStart(ctx, opts, c, &budget), "postStart hook failed: command exited with code 7")
	require.Equal(t, command, task.spec.Args)
	require.Equal(t, uint32(42), task.spec.User.UID)
	require.True(t, process.started)
	require.True(t, process.deleted)
}

func TestTemplatePreStopFailureContinuesTerminationAndIsNotRetried(t *testing.T) {
	command := []string{"/bin/flush"}
	process := &templateHookTestProcess{exitCode: 7}
	task := &templateHookTestTask{process: process}
	c := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{Config: &cubeboxpb.ContainerConfig{
			Lifecycle: &cubeboxpb.TemplateLifecycleCommands{PreStop: command},
		}},
		Status:    cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: 1}),
		Container: &templateHookTestContainer{task: task},
	}
	sb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox-test"}}
	manager := &templateHookTestStore{sync: func(id string) {
		require.Equal(t, sb.ID, id)
		require.True(t, c.Status.Get().TemplatePreStopStarted)
		require.False(t, process.started)
	}}
	l := &local{cubeboxManger: manager}
	opts := &workflow.DestroyContext{DestroyInfo: &cubeboxpb.DestroyCubeSandboxRequest{}}
	finish := l.runTemplatePreStop(t.Context(), sb, c, opts)
	require.NotNil(t, finish)
	require.Empty(t, task.signals, "legacy HTTP hooks must run before SIGTERM")
	finish()
	require.Nil(t, l.runTemplatePreStop(t.Context(), sb, c, opts))

	require.Equal(t, command, task.spec.Args)
	require.True(t, process.started)
	require.True(t, process.deleted)
	require.Equal(t, []syscall.Signal{syscall.SIGTERM}, task.signals)
}

func TestTemplatePreStopRetriesAfterPersistFailure(t *testing.T) {
	process := &templateHookTestProcess{}
	task := &templateHookTestTask{process: process}
	c := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{Config: &cubeboxpb.ContainerConfig{
			Lifecycle: &cubeboxpb.TemplateLifecycleCommands{PreStop: []string{"/bin/flush"}},
		}},
		Status:    cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: 1}),
		Container: &templateHookTestContainer{task: task},
	}
	sb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox-persist-retry"}}
	syncCalls := 0
	manager := &templateHookTestStore{
		syncErr: errors.New("temporary write failure"),
		sync: func(id string) {
			syncCalls++
			require.Equal(t, sb.ID, id)
			require.True(t, c.Status.Get().TemplatePreStopStarted)
			require.False(t, process.started, "the attempt must be saved before exec")
		},
	}
	l := &local{cubeboxManger: manager}
	opts := &workflow.DestroyContext{DestroyInfo: &cubeboxpb.DestroyCubeSandboxRequest{}}
	require.Nil(t, l.runTemplatePreStop(t.Context(), sb, c, opts))
	require.False(t, c.Status.Get().TemplatePreStopStarted)
	require.False(t, process.started)
	require.Nil(t, task.spec)
	require.Empty(t, task.signals)

	manager.syncErr = nil
	finish := l.runTemplatePreStop(t.Context(), sb, c, opts)
	require.NotNil(t, finish)
	finish()
	require.True(t, c.Status.Get().TemplatePreStopStarted)
	require.True(t, process.started)
	require.True(t, process.deleted)
	require.Equal(t, []syscall.Signal{syscall.SIGTERM}, task.signals)
	require.Equal(t, 2, syncCalls)

	require.Nil(t, l.runTemplatePreStop(t.Context(), sb, c, opts))
	require.Equal(t, 2, syncCalls, "a successfully recorded attempt must not be repeated")
	require.Len(t, task.signals, 1)
}

type templateHookTestStore struct {
	cubes.CubeboxAPI
	sync    func(string)
	syncErr error
}

func (s *templateHookTestStore) SyncByID(_ context.Context, id string, _ ...cubes.UpdateCubeboxOpt) error {
	s.sync(id)
	return s.syncErr
}

type templateHookTestContainer struct {
	containerd.Container
	task    containerd.Task
	taskErr error
	specErr error
}

func (c *templateHookTestContainer) Task(context.Context, cio.Attach) (containerd.Task, error) {
	return c.task, c.taskErr
}

func (c *templateHookTestContainer) Spec(context.Context) (*oci.Spec, error) {
	return &oci.Spec{Process: &specs.Process{User: specs.User{UID: 42}}}, c.specErr
}

type templateHookTestTask struct {
	containerd.Task
	execErr    error
	process    containerd.Process
	spec       *specs.Process
	signals    []syscall.Signal
	exitStatus chan containerd.ExitStatus
	onKill     func()
}

func (t *templateHookTestTask) Exec(_ context.Context, _ string, spec *specs.Process, _ cio.Creator) (containerd.Process, error) {
	t.spec = spec
	return t.process, t.execErr
}

func (t *templateHookTestTask) Kill(_ context.Context, signal syscall.Signal, _ ...containerd.KillOpts) error {
	if t.exitStatus == nil {
		return errors.New("task.Wait must be registered before SIGTERM")
	}
	t.signals = append(t.signals, signal)
	if t.onKill != nil {
		t.onKill()
	}
	t.exitStatus <- *containerd.NewExitStatus(0, time.Now(), nil)
	return nil
}

func (t *templateHookTestTask) Wait(context.Context) (<-chan containerd.ExitStatus, error) {
	t.exitStatus = make(chan containerd.ExitStatus, 1)
	return t.exitStatus, nil
}

type templateHookTestProcess struct {
	containerd.Process
	waitErr          error
	startErr         error
	waitCalls        int
	cancelledWait    chan struct{}
	exitCode         uint32
	started          bool
	deleted          bool
	block            bool
	waitStatus       chan containerd.ExitStatus
	onStart          func(context.Context)
	signals          []syscall.Signal
	killContextErr   error
	deleteContextErr error
}

func (p *templateHookTestProcess) Wait(ctx context.Context) (<-chan containerd.ExitStatus, error) {
	p.waitCalls++
	if p.waitErr != nil {
		return nil, p.waitErr
	}
	status := make(chan containerd.ExitStatus, 1)
	if p.cancelledWait != nil && p.waitCalls == 1 {
		go func() {
			<-ctx.Done()
			status <- *containerd.NewExitStatus(255, time.Now(), ctx.Err())
			close(status)
			close(p.cancelledWait)
		}()
		return status, nil
	}
	if p.block {
		p.waitStatus = status
		return status, nil
	}
	status <- *containerd.NewExitStatus(p.exitCode, time.Now(), nil)
	return status, nil
}

func (p *templateHookTestProcess) Start(ctx context.Context) error {
	p.started = true
	if p.onStart != nil {
		p.onStart(ctx)
	}
	return p.startErr
}

func (p *templateHookTestProcess) Delete(ctx context.Context, _ ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	p.deleteContextErr = ctx.Err()
	p.deleted = ctx.Err() == nil
	return nil, ctx.Err()
}

func (p *templateHookTestProcess) Kill(ctx context.Context, signal syscall.Signal, _ ...containerd.KillOpts) error {
	p.killContextErr = ctx.Err()
	p.signals = append(p.signals, signal)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.block && p.waitStatus != nil {
		p.waitStatus <- *containerd.NewExitStatus(137, time.Now(), nil)
	}
	return nil
}

func TestTemplateHookTimeoutCleansUpWithLiveContext(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancelled"}[cancelled], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			process := &templateHookTestProcess{block: true}
			if cancelled {
				process.onStart = func(context.Context) { cancel() }
			}
			c := &cubeboxstore.Container{Container: &templateHookTestContainer{task: &templateHookTestTask{process: process}}}
			attempted, err := execTemplateHook(ctx, c, []string{"/bin/hang"})
			require.True(t, attempted)
			require.ErrorIs(t, err, ctx.Err())
			require.Equal(t, []syscall.Signal{syscall.SIGKILL}, process.signals)
			require.NoError(t, process.killContextErr)
			require.NoError(t, process.deleteContextErr)
			require.True(t, process.deleted)
		})
	}
}

func TestTemplatePostStartSharesSandboxBudget(t *testing.T) {
	budget := time.Second
	var deadlines []time.Time
	opts := &workflow.CreateContext{ReqInfo: &cubeboxpb.RunCubeSandboxRequest{}}
	for i := 0; i < 2; i++ {
		index := i
		process := &templateHookTestProcess{onStart: func(ctx context.Context) {
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			deadlines = append(deadlines, deadline)
			if index == 0 {
				time.Sleep(30 * time.Millisecond)
			}
		}}
		c := &cubeboxstore.Container{
			Metadata:  cubeboxstore.Metadata{Config: &cubeboxpb.ContainerConfig{Lifecycle: &cubeboxpb.TemplateLifecycleCommands{PostStart: []string{"/bin/init"}}}},
			Container: &templateHookTestContainer{task: &templateHookTestTask{process: process}},
		}
		require.NoError(t, runTemplatePostStart(t.Context(), opts, c, &budget))
	}
	require.WithinDuration(t, deadlines[0], deadlines[1], 10*time.Millisecond, "each container must not get a fresh budget")
	require.Less(t, budget, 970*time.Millisecond)
	budget = 0
	task := &templateHookTestTask{process: &templateHookTestProcess{}}
	c := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{Config: &cubeboxpb.ContainerConfig{
			Lifecycle: &cubeboxpb.TemplateLifecycleCommands{PostStart: []string{"/bin/init"}},
		}},
		Container: &templateHookTestContainer{task: task},
	}
	require.ErrorContains(t, runTemplatePostStart(t.Context(), opts, c, &budget), "context deadline exceeded")
	require.Nil(t, task.spec, "an exhausted budget must not start another container's hook")
}

func TestTemplatePreStopKeepsLegacyHTTPHookBeforeSIGTERM(t *testing.T) {
	hookStarted := make(chan struct{})
	mainStopped := make(chan struct{})
	process := &templateHookTestProcess{onStart: func(context.Context) { close(hookStarted) }}
	task := &templateHookTestTask{process: process, onKill: func() { close(mainStopped) }}
	observations := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := false
		select {
		case <-hookStarted:
			started = true
		default:
		}
		select {
		case <-mainStopped:
			started = false
		default:
		}
		observations <- started
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	address := server.Listener.Addr().(*net.TCPAddr)
	c := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{Config: &cubeboxpb.ContainerConfig{
			Lifecycle: &cubeboxpb.TemplateLifecycleCommands{PreStop: []string{"/bin/flush"}},
			Poststop:  &cubeboxpb.PostStop{TimeoutMs: 1000, LifecyleHandler: &cubeboxpb.LifecycleHandler{HttpGet: &cubeboxpb.HTTPGetAction{Port: int32(address.Port)}}},
		}},
		IP: address.IP.String(), Status: cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: 1}),
		Container: &templateHookTestContainer{task: task},
	}
	sb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox-http-hook"}}
	l := &local{cubeboxManger: &templateHookTestStore{sync: func(string) {}}}
	l.runContainerStopHooks(t.Context(), sb, c, &workflow.DestroyContext{DestroyInfo: &cubeboxpb.DestroyCubeSandboxRequest{}})
	select {
	case alive := <-observations:
		require.True(t, alive, "HTTP Poststop must observe the app before SIGTERM")
	default:
		t.Fatal("legacy Poststop HTTP hook was not called")
	}
	require.Equal(t, []syscall.Signal{syscall.SIGTERM}, task.signals)
}

func TestTemplatePreStopRetriesOnlyBeforeStartAttempt(t *testing.T) {
	for _, stage := range []string{"task", "spec", "exec", "wait", "start"} {
		t.Run(stage, func(t *testing.T) {
			failure := errors.New("temporary failure")
			process := &templateHookTestProcess{}
			task := &templateHookTestTask{process: process}
			container := &templateHookTestContainer{task: task}
			switch stage {
			case "task":
				container.taskErr = failure
			case "spec":
				container.specErr = failure
			case "exec":
				task.execErr = failure
			case "wait":
				process.waitErr = failure
			case "start":
				process.startErr = failure
			}
			c := &cubeboxstore.Container{
				Metadata: cubeboxstore.Metadata{Config: &cubeboxpb.ContainerConfig{
					Lifecycle: &cubeboxpb.TemplateLifecycleCommands{PreStop: []string{"/bin/flush"}},
				}},
				Status:    cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: 1}),
				Container: container,
			}
			sb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sandbox-before-start"}}
			var savedMarkers []bool
			l := &local{cubeboxManger: &templateHookTestStore{sync: func(string) {
				savedMarkers = append(savedMarkers, c.Status.Get().TemplatePreStopStarted)
			}}}
			opts := &workflow.DestroyContext{DestroyInfo: &cubeboxpb.DestroyCubeSandboxRequest{}}
			finish := l.runTemplatePreStop(t.Context(), sb, c, opts)
			require.NotNil(t, finish)
			finish()
			if stage == "start" {
				require.True(t, c.Status.Get().TemplatePreStopStarted)
				require.Equal(t, []bool{true}, savedMarkers)
				require.Nil(t, l.runTemplatePreStop(t.Context(), sb, c, opts), "a lost Start reply must not cause duplicate execution")
				return
			}
			require.False(t, c.Status.Get().TemplatePreStopStarted)
			require.Equal(t, []bool{true, false}, savedMarkers)
			require.False(t, process.started)
			container.taskErr, container.specErr, task.execErr, process.waitErr = nil, nil, nil, nil
			finish = l.runTemplatePreStop(t.Context(), sb, c, opts)
			require.NotNil(t, finish)
			finish()
			require.True(t, process.started)
			require.Equal(t, []bool{true, false, true}, savedMarkers)
		})
	}
}

func TestTemplateHookCleanupWaitsAgainAfterOriginalWaitCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	process := &templateHookTestProcess{block: true, cancelledWait: make(chan struct{}), onStart: func(context.Context) { cancel() }}
	c := &cubeboxstore.Container{Container: &templateHookTestContainer{task: &templateHookTestTask{process: process}}}
	attempted, err := execTemplateHook(ctx, c, []string{"/bin/hang"})
	require.True(t, attempted)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-process.cancelledWait:
	case <-time.After(time.Second):
		t.Fatal("original Wait RPC did not observe cancellation")
	}
	require.Equal(t, 2, process.waitCalls, "cleanup needs a fresh Wait RPC under its own context")
	require.Equal(t, []syscall.Signal{syscall.SIGKILL}, process.signals)
	require.NoError(t, process.killContextErr)
	require.NoError(t, process.deleteContextErr)
	require.True(t, process.deleted)
}
