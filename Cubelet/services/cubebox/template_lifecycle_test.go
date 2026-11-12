// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
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

	annotations[constants.MasterAnnotationsAppSnapshotCreate] = "true"
	require.NoError(t, runTemplatePostStart(ctx, opts, c))
	require.Nil(t, task.spec)
	delete(annotations, constants.MasterAnnotationsAppSnapshotCreate)
	annotations[constants.MasterAnnotationPauseSnapshotID] = "snap-pause"
	annotations[constants.MasterAnnotationDesiredSandboxID] = "sandbox-existing"
	require.NoError(t, runTemplatePostStart(ctx, opts, c))
	require.Nil(t, task.spec)
	delete(annotations, constants.MasterAnnotationPauseSnapshotID)
	delete(annotations, constants.MasterAnnotationDesiredSandboxID)

	require.ErrorContains(t, runTemplatePostStart(ctx, opts, c), "postStart hook failed: command exited with code 7")
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
	l.runTemplatePreStop(context.Background(), sb, c, opts)
	l.runTemplatePreStop(context.Background(), sb, c, opts)

	require.Equal(t, command, task.spec.Args)
	require.True(t, process.started)
	require.True(t, process.deleted)
	require.Equal(t, []syscall.Signal{syscall.SIGTERM}, task.signals)
}

type templateHookTestStore struct {
	cubes.CubeboxAPI
	sync func(string)
}

func (s *templateHookTestStore) SyncByID(_ context.Context, id string, _ ...cubes.UpdateCubeboxOpt) error {
	s.sync(id)
	return nil
}

type templateHookTestContainer struct {
	containerd.Container
	task containerd.Task
}

func (c *templateHookTestContainer) Task(context.Context, cio.Attach) (containerd.Task, error) {
	return c.task, nil
}

func (*templateHookTestContainer) Spec(context.Context) (*oci.Spec, error) {
	return &oci.Spec{Process: &specs.Process{User: specs.User{UID: 42}}}, nil
}

type templateHookTestTask struct {
	containerd.Task
	process containerd.Process
	spec    *specs.Process
	signals []syscall.Signal
}

func (t *templateHookTestTask) Exec(_ context.Context, _ string, spec *specs.Process, _ cio.Creator) (containerd.Process, error) {
	t.spec = spec
	return t.process, nil
}

func (t *templateHookTestTask) Kill(_ context.Context, signal syscall.Signal, _ ...containerd.KillOpts) error {
	t.signals = append(t.signals, signal)
	return nil
}

func (*templateHookTestTask) Wait(context.Context) (<-chan containerd.ExitStatus, error) {
	status := make(chan containerd.ExitStatus, 1)
	status <- *containerd.NewExitStatus(0, time.Now(), nil)
	return status, nil
}

type templateHookTestProcess struct {
	containerd.Process
	exitCode uint32
	started  bool
	deleted  bool
}

func (p *templateHookTestProcess) Wait(context.Context) (<-chan containerd.ExitStatus, error) {
	status := make(chan containerd.ExitStatus, 1)
	status <- *containerd.NewExitStatus(p.exitCode, time.Now(), nil)
	return status, nil
}

func (p *templateHookTestProcess) Start(context.Context) error {
	p.started = true
	return nil
}

func (p *templateHookTestProcess) Delete(context.Context, ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	p.deleted = true
	return nil, nil
}
