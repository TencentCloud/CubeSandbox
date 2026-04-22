// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/prometheus/procfs"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/rootfs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/runc"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/taskio"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (l *local) Destroy(ctx context.Context, opts *workflow.DestroyContext) (err error) {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.DestroyContext nil")
	}

	sandBoxID := opts.SandboxID
	defer func() {

		if err == nil {
			err = l.cubeboxManger.Delete(ctx, &cubes.DeleteOption{
				CubeboxID: sandBoxID,
			})
		}
	}()
	sb, err := l.cubeboxManger.Get(ctx, sandBoxID)
	if err != nil {

		if errors.Is(err, utils.ErrorKeyNotFound) {
			return nil
		}
		return err
	}

	sb.Lock()
	defer sb.Unlock()

	if opts.BaseWorkflowInfo.IsRollBack {
		if sb.UserMarkDeletedTime == nil {
			now := time.Now()
			sb.UserMarkDeletedTime = &now
			sb.DeleteRequestID = opts.DestroyInfo.RequestID
			l.cubeboxManger.SyncByID(ctx, sandBoxID)
		}
	}

	if !sandboxDeletable(sb, opts.DestroyInfo.GetFilter()) {
		return ret.Err(errorcode.ErrorCode_PreConditionFailed, "unmatched filter")
	}

	if sb.Namespace != "" {
		ctx = namespaces.WithNamespace(ctx, sb.Namespace)
	} else {
		ctx = namespaces.WithNamespace(ctx, namespaces.Default)
	}

	if !sandboxDeletable(sb, opts.DestroyInfo.GetFilter()) {
		return ret.Err(errorcode.ErrorCode_PreConditionFailed, "unmatched filter")
	}

	sb.GetStatus().Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.Removing = true
		return status, nil
	})

	if sb.GetStatus().IsTerminated() {
		ctx = constants.WithTerminatingPod(ctx)
	}

	if constants.IsCollectMemory(ctx) {
		l.collectSandboxMaxMemoryUsage(ctx, sb)
	}
	err = l.cbriManager.DestroySandbox(ctx, sb.SandboxID)
	if err != nil {
		log.G(ctx).Errorf("failed to destroy cbri sandbox %s", err.Error())
		return fmt.Errorf("failed to destroy cbri sandbox")
	}

	l.collectSandboxLifeTimeMetric(ctx, sb)

	var (
		result *multierror.Error
	)

	var containers []*cubeboxstore.Container
	for _, ci := range sb.All() {
		containers = append(containers, ci)
	}
	if sb.GetStatus().IsPaused() {

		ctx = constants.WithSkipRuntimeAPI(ctx)
		if _, err := l.localTask.Delete(ctx, &tasks.DeleteTaskRequest{
			ContainerID: sb.ID,
		}); err != nil {
			result = multierror.Append(result, fmt.Errorf("delete sandbox by binary failed: %w", err))
		}
	} else {

		containers = append(containers, sb.FirstContainer())

		for i, cntr := range containers {
			if cntr == nil {
				continue
			}
			if cntr.DeletedTime != nil {
				continue
			}

			tmpCtx := log.WithLogger(ctx, log.G(ctx).WithFields(map[string]interface{}{
				"ContainerId": cntr.ID,
			}))
			tmpCtx = context.WithValue(tmpCtx, constants.KCubeIndexContext, fmt.Sprint(len(containers)-1-i))
			tmpCtx = context.WithValue(tmpCtx, "sandboxID", sandBoxID)

			cntr.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.Removing = true
				return status, nil
			})

			if er := l.destroyContainer(tmpCtx, cntr); er != nil {
				if strings.Contains(er.Error(), "closed") {
					continue
				}
				log.G(tmpCtx).WithField("cubebox", log.WithJsonValue(sb)).Warnf("Destroy container [%s] fail: %s", cntr.ID, er)
				result = multierror.Append(result, fmt.Errorf("destroy container [%s] fail: %w", cntr.ID, er))
				continue
			}
			if er := l.cubeboxManger.Delete(ctx, &cubes.DeleteOption{
				CubeboxID:   sb.ID,
				ContainerID: cntr.ID,
			}); er != nil {
				log.G(tmpCtx).Warnf("Delete container [%s] in cubestore fail: %v", cntr.ID, er)
			}
		}
	}

	if er := runc.Clean(ctx, opts.SandboxID); er != nil {
		result = multierror.Append(result, fmt.Errorf("destroy runc files [%s] fail: %w", sandBoxID, er))
	}
	if er := result.ErrorOrNil(); er != nil {
		return ret.Errorf(errorcode.ErrorCode_RemoveContainerFailed, "%s", er.Error())
	}
	return nil
}

func sandboxDeletable(sb *cubeboxstore.CubeBox, filter *cubebox.CubeSandboxFilter) bool {
	if filter == nil || len(filter.GetLabelSelector()) == 0 {
		return true
	}

	for k, v := range filter.GetLabelSelector() {

		got, exist := sb.Labels[k]
		if !exist {
			return true
		}
		if got != v {
			return false
		}
	}

	return true
}

func (l *local) destroyContainer(ctx context.Context, c *cubeboxstore.Container) error {
	if c == nil {
		return nil
	}
	ctx = constants.WithPreStopType(ctx, constants.PreStopTypeDestroy)
	start := time.Now()
	defer func() {
		workflow.RecordDestroyMetric(ctx, nil, constants.DelContainer, time.Since(start))
	}()
	log.G(ctx).Debugf("destroyContainer %s", c.ID)
	if !constants.IsFailoverOperation(ctx) && !constants.IsTerminatingPod(ctx) {
		if constants.IsCollectMemory(ctx) {
			collectContainerMaxMemoryUsage(ctx, c)
		}
		if config.GetCommon().EnableSandboxExecCmdBeforeExist {
			doExecCmdBeforeExit(ctx, c)
		}

		doPreStop(ctx, c)
		doPostStop(ctx, c)
	} else {
		log.G(ctx).Infof("skip preStop for failover operation")
	}

	err := l.cleanContainerdContainer(ctx, c.Container)

	if er := rootfs.CleanRootfs(ctx, c.ID); er != nil {
		log.G(ctx).Warnf("clean rootfs failed.%s", er)
	}
	if er := taskio.Clean(ctx, c.ID); er != nil {
		log.G(ctx).Warnf("clean fifo failed.%s", er)
	}

	return err
}

func (l *local) cleanContainerdContainer(ctx context.Context, container containerd.Container) error {

	if container == nil {
		return nil
	}
	id := container.ID()
	err := l.stopTask(ctx, container)
	if cubes.IsNotFoundContainerError(err) || isTtrpcError(err) {
		log.G(ctx).Warnf("ignore stop task [%s] fail: %v", container.ID(), err)
	} else if err != nil {

		log.G(ctx).Warnf("stopTask %s error: %s", id, err)
		return ret.Err(errorcode.ErrorCode_DeleteTaskFailed, err.Error())
	}

	err = deleteContainer(ctx, container)
	if cubes.IsNotFoundContainerError(err) || isTtrpcError(err) {
		log.G(ctx).Warnf("ignore delete container [%s] fail: %v", container.ID(), err)
	} else if err != nil {
		log.G(ctx).Errorf("deleteContainer %s error: %s", id, err)
		return ret.Err(errorcode.ErrorCode_DeleteContainerFromMetaDataFailed, err.Error())
	}
	return nil
}

func (l *local) stopTask(ctx context.Context, container containerd.Container) (err error) {
	start := time.Now()
	defer func() {
		workflow.RecordDestroyMetric(ctx, err, constants.CubeDeleteTaskId, time.Since(start))
	}()
	if container == nil {
		return nil
	}
	id := container.ID()
	log.G(ctx).Debugf("stopTask %s ", id)
	if debugDestroy := ctx.Value("destroy_forcibly"); debugDestroy != nil {
		s, ok := debugDestroy.(string)
		if ok && s == "true" {
			log.G(ctx).Debugf("forcibly delete task %s ", id)
			_, err = l.localTask.Delete(ctx, &tasks.DeleteTaskRequest{ContainerID: id})
			err = errgrpc.ToNative(err)
			if cubes.IsNotFoundContainerError(err) {
				return nil
			} else {
				log.G(ctx).Warnf("forcibly delete task %s fail: %v", id, err)
				return err
			}
		}
	}
	if !constants.IsTerminatingPod(ctx) {

		_, err = container.Task(ctx, nil)
		if err != nil {

			log.G(ctx).Warnf("Get and State task %s ret: %v", id, err)
			if cubes.IsNotFoundContainerError(err) {
				_, err = l.localTask.Kill(ctx, &tasks.KillRequest{ContainerID: id,
					Signal: uint32(syscall.SIGKILL), All: true})
				if err == nil {
					_, err = l.localTask.Wait(ctx, &tasks.WaitRequest{ContainerID: id})
				}

				_, err = l.localTask.Delete(ctx, &tasks.DeleteTaskRequest{ContainerID: id})
				if err != nil {
					err = errgrpc.ToNative(err)
					if cubes.IsNotFoundContainerError(err) || isTtrpcError(err) {
						return nil
					}
					log.G(ctx).Warnf("forcibly delete task %s fail: %v", id, err)
				}
				return err
			}

			_, err = l.localTask.Kill(ctx, &tasks.KillRequest{ContainerID: id,
				Signal: uint32(syscall.SIGKILL), All: true})
			if err == nil {
				_, err = l.localTask.Wait(ctx, &tasks.WaitRequest{ContainerID: id})
			} else {
				log.G(ctx).Warn(errors.Wrapf(err, "failed to send SIGKILL for container[%s]", id))
			}
			return err
		}

		_, err = l.localTask.Kill(ctx, &tasks.KillRequest{
			ContainerID: id,
			Signal:      uint32(syscall.SIGKILL),
			All:         true,
		})
		if err == nil {
			_, err = l.localTask.Wait(ctx, &tasks.WaitRequest{ContainerID: id})
		} else {
			log.G(ctx).Warn(errors.Wrapf(err, "failed to send SIGKILL for container[%s]", id))
		}
	}

	_, err = l.localTask.Delete(ctx, &tasks.DeleteTaskRequest{ContainerID: id})
	if err != nil {
		err = errgrpc.ToNative(err)
		if cubes.IsNotFoundContainerError(err) {
			return nil
		}
		log.G(ctx).Warnf("forcibly delete task %s fail: %v", id, err)
	}
	return err
}

func deleteContainer(ctx context.Context, container containerd.Container) (err error) {
	start := time.Now()
	defer func() {
		workflow.RecordDestroyMetric(ctx, err, constants.CubeDelContainerId, time.Since(start))
	}()
	var delOpts []containerd.DeleteOpts
	if _, err := container.Image(ctx); err == nil {
		delOpts = append(delOpts, containerd.WithSnapshotCleanup)
	}

	return container.Delete(ctx, delOpts...)
}

func (l *local) collectSandboxMaxMemoryUsage(ctx context.Context, sandBox *cubeboxstore.CubeBox) {
	if sandBox.CGroupPath == "" {
		return
	}
	pid := sandBox.Endpoint.Pid
	if pid == 0 {
		return
	}

	shimVmHWM, _ := readVmHWM(int(pid))

	total := (shimVmHWM) / 1024 / 1024

	rt := &CubeLog.RequestTrace{
		Action:     "SandboxMaxMemoryUsage",
		Caller:     constants.CubeboxID.ID(),
		Callee:     sandBox.ID,
		InstanceID: sandBox.ID,
		RetCode:    int64(total),
	}
	CubeLog.Trace(rt)
}

func readVmHWM(pid int) (uint64, error) {
	p, err := procfs.NewProc(pid)
	if err != nil {
		return 0, fmt.Errorf("failed to get procfs: %v", err)
	}
	stat, err := p.NewStatus()
	if err != nil {
		return 0, fmt.Errorf("failed to get procfs stat: %v", err)
	}
	return stat.VmHWM, nil
}

func collectContainerMaxMemoryUsage(ctx context.Context, ci *cubeboxstore.Container) {
	execCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	cmd := []string{"cat", "/sys/fs/cgroup/memory/memory.max_usage_in_bytes"}
	out, err := containerExecWithOutput(execCtx, ci, cmd)
	if err != nil {
		log.G(ctx).Errorf("collectContainerMaxMemoryUsage failed: exec command: %v", err)
		return
	}
	if len(out) == 0 {
		return
	}

	out = bytes.TrimSpace(out)
	maxUsageInBytes, err := strconv.ParseInt(string(out), 10, 64)
	if err != nil {
		log.G(ctx).Errorf("collectContainerMaxMemoryUsage failed: parse max usage: %v", err)
		return
	}

	rt := &CubeLog.RequestTrace{
		Action:     "ContainerMaxMemoryUsage",
		Caller:     constants.CubeboxID.ID(),
		Callee:     ci.ID,
		InstanceID: ci.SandboxID,
		RetCode:    maxUsageInBytes / 1024 / 1024,
	}

	CubeLog.Trace(rt)
}

func doExecCmdBeforeExit(ctx context.Context, ci *cubeboxstore.Container) {
	if len(config.GetCommon().SandboxExecCmdBeforeExist) == 0 {
		return
	}
	if ci.Metadata.Labels != nil && ci.Metadata.Labels[constants.LabelHealthCheckPod] != "" {

		return
	}
	execCtx, cancel := context.WithTimeout(ctx, config.GetCommon().SandboxExecCmdTimeOut)
	defer cancel()

	out, err := containerExecWithOutput(execCtx, ci, config.GetCommon().SandboxExecCmdBeforeExist)
	if err != nil {
		log.G(ctx).Errorf("doExecCmdBeforeExit failed: %v,%s",
			utils.InterfaceToString(config.GetCommon().SandboxExecCmdBeforeExist), err.Error())
		return
	}
	if len(out) == 0 {
		return
	}
	logEntry := CubeLog.GetLogger("stdout").WithContext(ctx)
	fields := CubeLog.Fields{"Module": "Stdout", "InstanceId": ci.SandboxID}
	shimLog := logEntry.WithFields(fields)
	maxLines := config.GetCommon().SandboxExecCmdOutMaxLines
	trimOut := func(out []byte) string {
		lines := strings.Split(string(out), "\r\n")
		if len(lines) > maxLines {
			lines = lines[:maxLines]
		}
		return strings.Join(lines, "\r\n")
	}
	if config.GetCommon().SandboxExecCmdBeforeExistLogOut {
		shimLog.Error(trimOut(out))
	}
	if config.GetCommon().SandboxExecCmdMatchLine == "" {
		return
	}

	if strings.Contains(string(out), config.GetCommon().SandboxExecCmdMatchLine) {
		log.G(ctx).Errorf("doExecCmdBeforeExit match: [%s]", config.GetCommon().SandboxExecCmdMatchLine)
		execCtx, cancel := context.WithTimeout(ctx, config.GetCommon().SandboxExecCmdTimeOut)
		defer cancel()

		out, err := containerExecWithOutput(execCtx, ci, config.GetCommon().SandboxExecCmdAfterMatch)
		if err != nil {
			log.G(ctx).Errorf("doExecCmdBeforeExit failed: %v,%s",
				utils.InterfaceToString(config.GetCommon().SandboxExecCmdAfterMatch), err.Error())
			return
		}
		if len(out) == 0 {
			return
		}
		if config.GetCommon().SandboxExecCmdAfterMatchLogOut {
			if config.GetCommon().SandboxExecCmdBeforeExistLogOut {
				shimLog.Errorf("[SandboxExecCmdAfterMatch]:%s", trimOut(out))
			}
		}
	}
}

func containerExecWithOutput(ctx context.Context, ci *cubeboxstore.Container, cmd []string) ([]byte, error) {
	if ci == nil || ci.Container == nil {
		return nil, nil
	}
	task, err := ci.Container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	var stdout, stderr bytes.Buffer

	cioOpts := []cio.Opt{
		cio.WithStreams(nil, &stdout, &stderr),
		cio.WithTerminal, cio.WithFIFODir("/data/cubelet/fifo"),
	}
	ioCreator := cio.NewCreator(cioOpts...)

	spec, err := ci.Container.Spec(ctx)
	if err != nil {
		return nil, err
	}
	pspec := spec.Process
	pspec.CommandLine = ""
	pspec.Args = cmd

	process, err := task.Exec(ctx, "exec-"+utils.GenerateID(), pspec, ioCreator)
	if err != nil {
		return nil, err
	}
	defer process.Delete(ctx)

	statusC, err := process.Wait(ctx)
	if err != nil {
		return nil, err
	}

	if err := process.Start(ctx); err != nil {
		return nil, err
	}
	status := <-statusC
	code, _, err := status.Result()
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("exec failed with exit code %d,stderr:%s", code, stderr.String())
	}

	process.IO().Wait()

	return stdout.Bytes(), nil
}

func (l *local) collectSandboxLifeTimeMetric(ctx context.Context, sb *cubeboxstore.CubeBox) {
	if sb.GetStatus().Get().LifeTimeMetricReported {
		return
	}

	sb.GetStatus().Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.LifeTimeMetricReported = true
		return status, nil
	})

	rt := &CubeLog.RequestTrace{
		Caller:       constants.CubeboxID.ID(),
		Callee:       "lifetimemetric",
		InstanceID:   sb.ID,
		InstanceType: sb.InstanceType,
	}
	if hostcfg := config.GetHostConf(); hostcfg != nil {
		rt.CalleeCluster = hostcfg.SchedulerLabel
	}

	if sb.ResourceWithOverHead != nil {
		lifeTime := time.Now().UnixNano() - sb.CreatedAt
		if sb.GetStatus().Get().FinishedAt != 0 {
			lifeTime = sb.GetStatus().Get().FinishedAt - sb.CreatedAt
		}

		if lifeTime <= 0 {
			return
		}
		lifeTime /= 1e9

		rt.CalleeAction = "cpu"
		rt.RetCode = sb.ResourceWithOverHead.VmCpuQ.Value() * lifeTime
		CubeLog.Trace(rt)

		rt.CalleeAction = "memory"
		rt.RetCode = int64(float64(sb.ResourceWithOverHead.VmMemQ.Value()) / 1024 / 1024 / 1024 * float64(lifeTime))
		CubeLog.Trace(rt)

		rt.CalleeAction = "lifetime"
		rt.RetCode = lifeTime
		CubeLog.Trace(rt)
	}
}
