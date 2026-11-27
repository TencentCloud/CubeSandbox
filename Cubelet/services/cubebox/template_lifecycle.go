// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
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
	templatePostStartTimeout   = 10 * time.Second
	templateTerminationGrace   = 5 * time.Second
	templateHookCleanupTimeout = time.Second
)

// Only hook execution spends the shared budget, not container startup. This
// avoids multiplying the 10s execution budget by the container count. Process
// cleanup has its own separately bounded budget.
func runTemplatePostStart(ctx context.Context, opts *workflow.CreateContext, c *cubeboxstore.Container, remaining *time.Duration) error {
	command := c.Config.GetLifecycle().GetPostStart()
	if len(command) == 0 || opts.IsCreateSnapshot() || opts.IsRestart || opts.IsPauseResume() {
		return nil
	}
	start := time.Now()
	defer func() { *remaining = max(0, *remaining-time.Since(start)) }()
	hookCtx, cancel := context.WithTimeout(ctx, *remaining)
	defer cancel()
	if _, err := execTemplateHook(hookCtx, c, command); err != nil {
		return ret.Errorf(errorcode.ErrorCode_ExecCommandInSandboxFailed, "postStart hook failed: %v", err)
	}
	return nil
}

func (l *local) runContainerStopHooks(ctx context.Context, sb *cubeboxstore.CubeBox, c *cubeboxstore.Container, opts *workflow.DestroyContext) {
	doPreStop(ctx, c)
	finishTermination := l.runTemplatePreStop(ctx, sb, c, opts)
	if finishTermination != nil {
		defer finishTermination()
	}
	doPostStop(ctx, c)
}

func shouldRunTemplatePreStop(sb *cubeboxstore.CubeBox, c *cubeboxstore.Container, opts *workflow.DestroyContext) bool {
	return len(c.Config.GetLifecycle().GetPreStop()) > 0 && c.Container != nil &&
		!c.Status.IsTerminated() && !c.Status.Get().TemplatePreStopStarted &&
		opts.DestroyInfo != nil && !opts.IsRollBack && !opts.IsRestartDestroy && !shouldKeepPausedTombstone(opts) &&
		opts.DestroyInfo.GetAnnotations()["cube.debug.destroy_forcibly"] != "true" &&
		sb.Annotations[constants.MasterAnnotationsAppSnapshotCreate] != "true"
}

// The returned function finishes graceful termination after the legacy HTTP
// hooks, sharing the preStop hook's original grace deadline.
func (l *local) runTemplatePreStop(ctx context.Context, sb *cubeboxstore.CubeBox, c *cubeboxstore.Container, opts *workflow.DestroyContext) func() {
	if !shouldRunTemplatePreStop(sb, c, opts) {
		return nil
	}
	command := c.Config.GetLifecycle().GetPreStop()

	// Preserve time for the ordinary kill and cleanup, even if the hook hangs.
	grace := templateTerminationGrace
	if deadline, ok := ctx.Deadline(); ok {
		grace = min(grace, time.Until(deadline)/2)
	}
	graceCtx, cancel := context.WithTimeout(ctx, grace)
	logger := log.G(ctx)
	if err := graceCtx.Err(); err != nil {
		logger.Warnf("preStop hook skipped: no termination grace time remains: %v", err)
		cancel()
		return nil
	}

	// Destroy holds the sandbox lock. Save the attempt before exec so a retried
	// delete cannot repeat a hook whose outcome the caller did not observe.
	c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.TemplatePreStopStarted = true
		return status, nil
	})
	if err := l.cubeboxManger.SyncByID(ctx, sb.ID, cubes.WithNoEvent); err != nil {
		// The hook has not run. Let a later delete retry persist the attempt
		// again rather than treating this failed write as an execution.
		c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
			status.TemplatePreStopStarted = false
			return status, nil
		})
		logger.Warnf("preStop hook skipped: failed to persist attempt: %v", err)
		cancel()
		return nil
	}
	startAttempted, err := execTemplateHook(graceCtx, c, command)
	if err != nil {
		logger.Warnf("preStop hook failed: %v", err)
		if !startAttempted {
			// No Start RPC was sent, so retrying cannot duplicate execution.
			// A failed Start RPC is ambiguous and must retain the marker.
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.TemplatePreStopStarted = false
				return status, nil
			})
			if err := l.cubeboxManger.SyncByID(ctx, sb.ID, cubes.WithNoEvent); err != nil {
				logger.Warnf("failed to persist unstarted preStop hook: %v", err)
			}
		}
	}
	return func() {
		defer cancel()
		if graceCtx.Err() != nil {
			return
		}
		// Any grace time left after the hook is available to the main process.
		// The existing destroy path sends SIGKILL if the container is still alive.
		task, err := c.Container.Task(graceCtx, nil)
		if err != nil {
			return
		}
		status, err := task.Wait(graceCtx)
		if err != nil {
			return
		}
		if err := task.Kill(graceCtx, syscall.SIGTERM); err != nil {
			return
		}
		select {
		case <-status:
		case <-graceCtx.Done():
		}
	}
}

func execTemplateHook(ctx context.Context, c *cubeboxstore.Container, command []string) (startAttempted bool, err error) {
	if err := ctx.Err(); err != nil {
		return startAttempted, err
	}
	task, err := c.Container.Task(ctx, nil)
	if err != nil {
		return startAttempted, fmt.Errorf("get container task: %w", err)
	}
	spec, err := c.Container.Spec(ctx)
	if err != nil {
		return startAttempted, fmt.Errorf("get container process spec: %w", err)
	}
	if spec.Process == nil {
		return startAttempted, fmt.Errorf("container spec has no process")
	}
	processSpec := *spec.Process
	processSpec.Args = command
	processSpec.CommandLine = ""
	processSpec.Terminal = false

	process, err := task.Exec(ctx, "exec-lifecycle-"+utils.GenerateID(), &processSpec, createDummyIO(false, "", "", ""))
	if err != nil {
		return startAttempted, fmt.Errorf("create hook process: %w", err)
	}
	exited := false
	defer func() { cleanupTemplateHook(ctx, process, exited) }()

	status, err := process.Wait(ctx)
	if err != nil {
		return startAttempted, fmt.Errorf("wait for hook process: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Start may succeed remotely even when its RPC returns an error.
	startAttempted = true
	if err := process.Start(ctx); err != nil {
		return startAttempted, fmt.Errorf("start hook process: %w", err)
	}
	select {
	case <-ctx.Done():
		return startAttempted, ctx.Err()
	case exit, ok := <-status:
		if !ok {
			return startAttempted, fmt.Errorf("hook process returned no exit status")
		}
		code, _, err := exit.Result()
		if err != nil {
			return startAttempted, fmt.Errorf("get hook exit status: %w", err)
		}
		exited = true
		if code != 0 {
			return startAttempted, fmt.Errorf("command exited with code %d", code)
		}
		return startAttempted, nil
	}
}

// Cleanup has its own deadline: timing out Wait does not stop a guest process.
func cleanupTemplateHook(ctx context.Context, process containerd.Process, exited bool) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), templateHookCleanupTimeout)
	defer cancel()
	if !exited {
		// Reserve half the budget for Delete if kill/wait stalls.
		killCtx, cancelKill := context.WithTimeout(cleanupCtx, templateHookCleanupTimeout/2)
		// containerd binds each Wait channel to its RPC context. The original
		// wait may already contain a cancellation error, not a process exit.
		// Register a fresh wait under the cleanup deadline before killing.
		status, waitErr := process.Wait(killCtx)
		if err := process.Kill(killCtx, syscall.SIGKILL); err != nil {
			log.G(ctx).Warnf("kill lifecycle hook process: %v", err)
		} else if waitErr == nil {
			select {
			case <-status:
			case <-killCtx.Done():
			}
		}
		cancelKill()
	}
	if _, err := process.Delete(cleanupCtx); err != nil {
		log.G(ctx).Warnf("delete lifecycle hook process: %v", err)
	}
}
