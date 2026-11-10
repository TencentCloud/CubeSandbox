// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

const (
	templatePostStartTimeout = 10 * time.Second
	templateTerminationGrace = 5 * time.Second
)

func runTemplatePostStart(ctx context.Context, opts *workflow.CreateContext, c *cubeboxstore.Container) error {
	command := c.Config.GetLifecycle().GetPostStart()
	if len(command) == 0 || opts.IsCreateSnapshot() || opts.IsRestart || resumeFromPauseSandboxID(opts.ReqInfo) != "" {
		return nil
	}
	hookCtx, cancel := context.WithTimeout(ctx, templatePostStartTimeout)
	defer cancel()
	if err := execTemplateHook(hookCtx, c, command); err != nil {
		return ret.Errorf(errorcode.ErrorCode_ExecCommandInSandboxFailed, "postStart hook failed: %v", err)
	}
	return nil
}

func (l *local) runTemplatePreStop(ctx context.Context, sb *cubeboxstore.CubeBox, c *cubeboxstore.Container, opts *workflow.DestroyContext) {
	command := c.Config.GetLifecycle().GetPreStop()
	if len(command) == 0 || c.Container == nil || c.Status.IsTerminated() || c.Status.Get().TemplatePreStopStarted ||
		opts.DestroyInfo == nil || opts.IsRollBack || opts.IsRestartDestroy || shouldKeepPausedTombstone(opts) ||
		opts.DestroyInfo.GetAnnotations()["cube.debug.destroy_forcibly"] == "true" ||
		sb.Annotations[constants.MasterAnnotationsAppSnapshotCreate] == "true" {
		return
	}

	// Preserve time for the ordinary kill and cleanup, even if the hook hangs.
	grace := templateTerminationGrace
	if deadline, ok := ctx.Deadline(); ok {
		grace = min(grace, time.Until(deadline)/2)
	}
	graceCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	logger := log.G(ctx)
	if err := graceCtx.Err(); err != nil {
		logger.Warnf("preStop hook skipped: no termination grace time remains: %v", err)
		return
	}

	// Destroy holds the sandbox lock. Save the attempt before exec so a retried
	// delete cannot repeat a hook whose outcome the caller did not observe.
	c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.TemplatePreStopStarted = true
		return status, nil
	})
	if err := l.cubeboxManger.SyncByID(ctx, sb.ID, cubes.WithNoEvent); err != nil {
		logger.Warnf("preStop hook skipped: failed to persist attempt: %v", err)
		return
	}
	if err := execTemplateHook(graceCtx, c, command); err != nil {
		logger.Warnf("preStop hook failed: %v", err)
	}
	if graceCtx.Err() != nil {
		return
	}
	// Any grace time left after the hook is available to the main process.
	// The existing destroy path sends SIGKILL if the container is still alive.
	task, err := c.Container.Task(graceCtx, nil)
	if err != nil {
		return
	}
	if err := task.Kill(graceCtx, syscall.SIGTERM); err != nil {
		return
	}
	status, err := task.Wait(graceCtx)
	if err != nil {
		return
	}
	select {
	case <-status:
	case <-graceCtx.Done():
	}
}

func execTemplateHook(ctx context.Context, c *cubeboxstore.Container, command []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	task, err := c.Container.Task(ctx, nil)
	if err != nil {
		return fmt.Errorf("get container task: %w", err)
	}
	spec, err := c.Container.Spec(ctx)
	if err != nil {
		return fmt.Errorf("get container process spec: %w", err)
	}
	if spec.Process == nil {
		return fmt.Errorf("container spec has no process")
	}
	processSpec := *spec.Process
	processSpec.Args = command
	processSpec.CommandLine = ""
	processSpec.Terminal = false

	process, err := task.Exec(ctx, "exec-lifecycle-"+utils.GenerateID(), &processSpec, createDummyIO(false, "", "", ""))
	if err != nil {
		return fmt.Errorf("create hook process: %w", err)
	}
	exited := false
	defer func() {
		// The caller tears down the container if the hook exhausted its budget.
		if ctx.Err() != nil {
			return
		}
		if !exited {
			_ = process.Kill(ctx, syscall.SIGKILL)
		}
		if _, deleteErr := process.Delete(ctx); deleteErr != nil {
			log.G(ctx).Warnf("delete lifecycle hook process: %v", deleteErr)
		}
	}()

	status, err := process.Wait(ctx)
	if err != nil {
		return fmt.Errorf("wait for hook process: %w", err)
	}
	if err := process.Start(ctx); err != nil {
		return fmt.Errorf("start hook process: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case exit, ok := <-status:
		if !ok {
			return fmt.Errorf("hook process returned no exit status")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		code, _, err := exit.Result()
		if err != nil {
			return fmt.Errorf("get hook exit status: %w", err)
		}
		exited = true
		if code != 0 {
			return fmt.Errorf("command exited with code %d", code)
		}
		return nil
	}
}
