// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (s *service) Exec(ctx context.Context, req *cubebox.ExecCubeSandboxRequest) (*cubebox.ExecCubeSandboxResponse, error) {
	execID := fmt.Sprintf("cubelet-exec-%s", uuid.New().String())
	var err error
	startTime := time.Now()

	ctx = namespaces.WithNamespace(ctx, namespaces.Default)
	rsp := &cubebox.ExecCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	rt := &CubeLog.RequestTrace{
		Action:       "Exec",
		RequestID:    req.RequestID,
		InstanceID:   req.SandboxId,
		ContainerID:  req.ContainerId,
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "Exec",
	}
	ctx = CubeLog.WithRequestTrace(ctx, rt)
	log.G(ctx).Errorf("Exec:%s", utils.InterfaceToString(req))
	defer func() {
		if !ret.IsSuccessCode(rsp.Ret.RetCode) {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("Exec fail:%+v", rsp)
		}
		workflow.RecordCreateMetricIfGreaterThan(ctx, err, constants.CubeExecProcessId, time.Since(startTime), 50*time.Millisecond)
	}()
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("Exec panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = string(debug.Stack())
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	if req.SandboxId == "" || req.ContainerId == "" {
		rsp.Ret.RetMsg = "must provide container name"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, req.SandboxId)
	if err != nil {
		rsp.Ret.RetMsg = fmt.Sprintf("failed to get sandbox: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
	if sb.Namespace != "" {
		ctx = namespaces.WithNamespace(ctx, sb.Namespace)
	}

	container, err := sb.Get(req.ContainerId)
	if err != nil {
		rsp.Ret.RetMsg = fmt.Sprintf("container with id %s not found, error: %v", req.ContainerId, err)
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	tsk, err := container.Container.Task(ctx, nil)
	if err != nil {
		rsp.Ret.RetMsg = fmt.Sprintf("failed to get task for container: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	pspec, err := generateExecProcessSpec(ctx, tsk, req)
	if err != nil {
		rsp.Ret.RetMsg = fmt.Sprintf("failed to generate exec process spec: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
	ioCreator := createDummyIO(req.Terminal, "", "", "")

	process, err := tsk.Exec(ctx, execID, pspec, ioCreator)
	if err != nil {
		rsp.Ret.RetMsg = fmt.Sprintf("failed to exec process: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_ExecCommandInSandboxFailed
		return rsp, nil
	}

	if err := process.Start(ctx); err != nil {
		rsp.Ret.RetMsg = fmt.Sprintf("failed to start process: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_ExecCommandInSandboxFailed
		return rsp, nil
	}
	log.G(ctx).Info("process started successfully")

	rsp.Ret.RetMsg = "exec success"
	rsp.Ret.RetCode = errorcode.ErrorCode_Success
	return rsp, nil

}

func generateExecProcessSpec(ctx context.Context, tsk containerd.Task, req *cubebox.ExecCubeSandboxRequest) (*specs.Process, error) {
	spec, err := tsk.Spec(ctx)
	if err != nil {
		return nil, err
	}
	pspec := spec.Process
	pspec.Terminal = req.Terminal
	pspec.Args = req.Args

	if req.Cwd != "" {
		pspec.Cwd = req.Cwd
	}

	currentEnvs := make(map[string]string)
	for _, env := range spec.Process.Env {
		kv := strings.SplitN(env, "=", 2)
		if len(kv) == 2 {
			currentEnvs[kv[0]] = kv[1]
		}
	}

	if len(req.Env) > 0 {
		for _, env := range req.Env {
			kv := strings.SplitN(env, "=", 2)
			if len(kv) == 2 {
				currentEnvs[kv[0]] = kv[1]
			}
		}
	}

	for k, v := range currentEnvs {
		pspec.Env = append(pspec.Env, fmt.Sprintf("%s=%s", k, v))
	}
	return pspec, nil
}

type dummyIO struct {
	terminal bool
	stdin    string
	stdout   string
	stderr   string
}

func (d *dummyIO) Config() cio.Config {
	return cio.Config{
		Terminal: d.terminal,
		Stdin:    d.stdin,
		Stdout:   d.stdout,
		Stderr:   d.stderr,
	}
}

func (d *dummyIO) Cancel() {
}

func (d *dummyIO) Wait() {
}

func (d *dummyIO) Close() error {
	return nil
}

func createDummyIO(terminal bool, stdin, stdout, stderr string) cio.Creator {
	return func(id string) (cio.IO, error) {
		return &dummyIO{
			terminal: terminal,
			stdin:    stdin,
			stdout:   stdout,
			stderr:   stderr,
		}, nil
	}
}
