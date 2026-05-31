// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (s *service) Update(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest) (*cubebox.UpdateCubeSandboxResponse, error) {
	rsp := &cubebox.UpdateCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	rt := &CubeLog.RequestTrace{
		Action:       "Update",
		RequestID:    req.RequestID,
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "Update",
		InstanceID:   req.SandboxID,
	}
	ctx = CubeLog.WithRequestTrace(ctx, rt)
	log.G(ctx).Errorf("Update:%s", utils.InterfaceToString(req))
	start := time.Now()
	defer func() {
		if !ret.IsSuccessCode(rsp.Ret.RetCode) {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("Update fail:%+v", rsp)
		}
		rt.Cost = time.Since(start)
		rt.RetCode = int64(rsp.Ret.RetCode)
		CubeLog.Trace(rt)
	}()

	if req.SandboxID == "" {
		rsp.Ret.RetMsg = "must provide container name"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	if req.Annotations == nil {
		rsp.Ret.RetMsg = "must provide Annotations"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	action := req.Annotations[constants.MasterAnnotationsUpdateAction]
	if action == "" {
		rsp.Ret.RetMsg = "must provide update action"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
	rt.CalleeAction = action

	unlock := s.updateSandboxLocks.Lock(req.SandboxID)
	defer unlock()
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("Update panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = fmt.Sprintf("Update panic info:%s", panicError)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, req.SandboxID)
	if err != nil {
		rsp.Ret.RetMsg = err.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
	rt.CalleeAction = action
	switch action {
	case constants.UpdateActionAddDevice, constants.UpdateActionRemoveDevice:
		rsp.Ret.RetMsg = "cloud disk hotplug is not supported in the open source build"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	case constants.UpdateActionPause:
		return s.UpdateWithPause(ctx, req, sb)
	case constants.UpdateActionResume:
		return s.UpdateWithResume(ctx, req, sb)
	default:
		rsp.Ret.RetMsg = "invalid update action"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
}

func addPauseResumeMetaData(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest) context.Context {
	md, ok := ttrpc.GetMetadata(ctx)
	if !ok {
		md = ttrpc.MD{}
	}
	md.Append("pod_scope", req.SandboxID)
	ctx = ttrpc.WithMetadata(ctx, md)
	tmpmd, _ := ttrpc.GetMetadata(ctx)
	log.G(ctx).Debugf("metadata:%+v", tmpmd)
	return ctx
}

func (s *service) UpdateWithPause(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest, sb *cubeboxstore.CubeBox) (*cubebox.UpdateCubeSandboxResponse, error) {
	rsp := &cubebox.UpdateCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if sb.GetStatus().IsPaused() {
		rsp.Ret.RetMsg = "sandbox is already paused"
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskStateInvalid
		return rsp, nil
	}
	if sb.GetStatus().IsTerminated() {
		// IsTerminated() covers both EXITED (FinishedAt!=0) and UNKNOWN
		// (Unknown=true). The legacy "sandbox is terminating" wording wrongly
		// implied a user-driven delete is in flight; use the same wording as
		// rollback.go's precheck so operators can tell the two states apart
		// from the message alone.
		rsp.Ret.RetMsg = "sandbox is not running"
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskStateInvalid
		return rsp, nil
	}

	ns := sb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)
	ctx = constants.WithPreStopType(ctx, constants.PreStopTypePause)
	task, err := sb.FirstContainer().Container.Task(ctx, nil)
	if err != nil {
		rsp.Ret.RetMsg = err.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskPauseFailed
		return rsp, nil
	}
	log.G(ctx).Infof("UpdateWithPause:%s", utils.InterfaceToString(req))
	ctx = addPauseResumeMetaData(ctx, req)
	defer func() {

		s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID)
	}()
	defer utils.Recover()
	for _, c := range sb.AllContainers() {
		if c.Status != nil {
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausingAt = time.Now().UnixNano()
				return status, nil
			})
		}
	}

	for _, c := range sb.All() {
		doPreStop(ctx, c)
	}

	doPreStop(ctx, sb.FirstContainer())

	// Give task.Pause an explicit timeout so it cannot be stretched out
	// arbitrarily by the upstream ctx; otherwise, once the upstream ctx is
	// cancelled the cubelet view stays stuck at PAUSING while cubeshim is
	// already PAUSED.
	pauseCtx, pauseCancel := context.WithTimeout(ctx, taskPauseTimeout)
	defer pauseCancel()
	if pauseErr := task.Pause(pauseCtx); pauseErr != nil {
		// Even when ttrpc reports an error (DeadlineExceeded / canceled /
		// ttrpc closed), cubeshim may have actually paused the VM. Query the
		// real status once with an independent, ctx-immune short timeout and
		// persist the truth, so the state never stays stuck at PAUSING.
		reconcileStatusAfterPauseError(ctx, sb, task, pauseErr)
		rsp.Ret.RetMsg = pauseErr.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskPauseFailed
		return rsp, nil
	}
	for _, c := range sb.AllContainers() {
		if c.Status != nil {
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausedAt = time.Now().UnixNano()
				status.PausingAt = 0
				return status, nil
			})
		}
	}
	return rsp, nil
}

func (s *service) UpdateWithResume(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest, sb *cubeboxstore.CubeBox) (*cubebox.UpdateCubeSandboxResponse, error) {
	rsp := &cubebox.UpdateCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if !sb.GetStatus().IsPaused() {
		rsp.Ret.RetMsg = "sandbox is not paused"
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskResumeFailed
		return rsp, nil
	}

	ns := sb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)
	task, err := sb.FirstContainer().Container.Task(ctx, nil)
	if err != nil {
		rsp.Ret.RetMsg = err.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskResumeFailed
		return rsp, nil
	}
	log.G(ctx).Infof("UpdateWithResume:%s", utils.InterfaceToString(req))
	ctx = addPauseResumeMetaData(ctx, req)

	// 保证无论是否 panic，状态都会落盘
	defer func() {
		s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID)
	}()
	defer utils.Recover()

	resumeCtx, resumeCancel := context.WithTimeout(ctx, taskResumeTimeout)
	defer resumeCancel()
	if err := task.Resume(resumeCtx); err != nil {
		// Same as pause: resume may time out midway while cubeshim has already
		// brought the VM back to RUNNING. Query the real status once and
		// converge to the truth, so the state never stays stuck at PAUSED.
		reconcileStatusAfterResumeError(ctx, sb, task, err)
		rsp.Ret.RetMsg = err.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskResumeFailed
		return rsp, nil
	}
	convergeResumeStateAfterOpaqueRestore(sb, time.Now().UTC())
	return rsp, nil
}

// Upper bound for the Pause/Resume ttrpc calls. 30s is used because cubeshim
// pausing a VM involves vCPU stop + device quiesce + memory eventual
// consistency, which is normally < 5s; 30s is a safety net to prevent the
// call from being stuck indefinitely when the upstream ctx is missing or
// blocked. Used together with the reconcile* error convergence.
const (
	taskPauseTimeout  = 30 * time.Second
	taskResumeTimeout = 30 * time.Second

	// Dedicated status-query timeout opened during reconcile. It MUST use a
	// fresh ctx and never reuse the already-expired ctx.
	reconcileStatusTimeout = 5 * time.Second

	// pausingStuckThreshold bounds how long a sandbox may legitimately remain
	// in the PAUSING transient. A real pause completes well within
	// taskPauseTimeout; once PausingAt is older than this -- e.g. the cubelet
	// restarted mid-pause and missed both the RPC-level reconcile and the
	// /tasks/paused event window -- the pause is no longer in flight and DeadGC
	// may safely query the shim to converge. It MUST stay comfortably larger
	// than taskPauseTimeout so an in-flight pause (during which the shim holds
	// its sandbox mutex and a ttrpc state() query would time out) is never
	// reconciled prematurely.
	pausingStuckThreshold = 2 * taskPauseTimeout
)

// reconcileStatusAfterPauseError, after task.Pause reports an error, actively
// queries cubeshim once for the real task status and straightens the cubelet
// in-memory view to the truth, so PausingAt never lingers forever. Note: all
// status writes here must stay consistent with the UpdateWithPause success
// path.
func reconcileStatusAfterPauseError(
	parentCtx context.Context,
	sb *cubeboxstore.CubeBox,
	task containerd.Task,
	pauseErr error,
) {
	// Deliberately start a fresh ctx from Background: parentCtx is very likely
	// already Done.
	queryCtx, cancel := context.WithTimeout(context.Background(), reconcileStatusTimeout)
	defer cancel()
	// Carry over the original ns to avoid namespaces.NamespaceRequired failing.
	if ns, ok := namespaces.Namespace(parentCtx); ok && ns != "" {
		queryCtx = namespaces.WithNamespace(queryCtx, ns)
	}

	st, qerr := task.Status(queryCtx)
	if qerr != nil {
		// Cannot determine the real status, so do not write blindly. Keep
		// PausingAt visible to operators and wait for the event-driven
		// reconcile (/tasks/paused subscription) to back it up.
		log.G(parentCtx).Errorf(
			"reconcileStatusAfterPauseError: task.Status failed sandbox=%s pauseErr=%v statusErr=%v",
			sb.ID, pauseErr, qerr)
		return
	}

	// Delegate to the shared converger so the PAUSE-direction rules cannot
	// drift between this RPC-level path and the DeadGC stuck-PAUSING fallback.
	// TaskPauseFailed is still returned to the upstream so it can alert; the
	// in-memory view here is merely straightened to match the real VM.
	convergePauseStateFromShim(parentCtx, sb, st.Status, fmt.Sprintf("pause RPC error: %v", pauseErr))
}

// convergePauseStateFromShim straightens PausingAt/PausedAt across every
// container of the sandbox so the in-memory view matches the shim's real task
// status. It is the single source of truth for the PAUSE-direction
// convergence rules, shared by reconcileStatusAfterPauseError (RPC path) and
// reconcileStuckPausingSandbox (DeadGC fallback) so the two can never drift.
// It never writes Unknown=true, so background scanners can use it without
// risking a spurious Terminated/Destroy cascade. reason only adds logging
// context.
func convergePauseStateFromShim(
	ctx context.Context,
	sb *cubeboxstore.CubeBox,
	shimStatus containerd.ProcessStatus,
	reason string,
) {
	switch shimStatus {
	case containerd.Paused:
		// cubeshim actually reached PAUSED -> write PausedAt exactly as the
		// UpdateWithPause success path does, so the next IsPaused() check sees
		// already-paused instead of staying stuck at PAUSING forever.
		log.G(ctx).Warnf(
			"convergePauseStateFromShim: shim reports PAUSED, converging to PAUSED sandbox=%s reason=%s",
			sb.ID, reason)
		for _, c := range sb.AllContainers() {
			if c.Status == nil {
				continue
			}
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausedAt = time.Now().UnixNano()
				status.PausingAt = 0
				return status, nil
			})
		}
	case containerd.Running, containerd.Created:
		// Really not paused -> clear PausingAt so it cannot linger forever.
		log.G(ctx).Warnf(
			"convergePauseStateFromShim: shim reports %s, clearing PausingAt sandbox=%s reason=%s",
			shimStatus, sb.ID, reason)
		for _, c := range sb.AllContainers() {
			if c.Status == nil {
				continue
			}
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausingAt = 0
				return status, nil
			})
		}
	default:
		// Intermediate states such as Stopped/Unknown/Pausing: leave the status
		// untouched and let TaskExit / the event subscription handle them.
		log.G(ctx).Warnf(
			"convergePauseStateFromShim: shim reports %s, leaving status untouched sandbox=%s reason=%s",
			shimStatus, sb.ID, reason)
	}
}

// convergeResumeStateAfterOpaqueRestore is the single source of truth for the
// RESUME-direction convergence rules. CubeShim resumes paused VMs from an
// internal full snapshot under /data/cubelet/root/pausevm/<sandbox> and does
// not expose that memory file as a cubecow catalog entry, so every successful
// resume convergence MUST both clear the paused markers and invalidate the
// runtime/restore-base bindings. Shared by the normal UpdateWithResume success
// path, the resume-RPC error reconcile path, and the /tasks/resumed event path
// so these flows cannot drift again.
func convergeResumeStateAfterOpaqueRestore(sb *cubeboxstore.CubeBox, attachedAt time.Time) {
	if sb == nil {
		return
	}
	invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(sb, attachedAt)
	for _, c := range sb.AllContainers() {
		if c.Status == nil {
			continue
		}
		c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
			status.PausedAt = 0
			status.PausingAt = 0
			return status, nil
		})
	}
}

// reconcileStatusAfterResumeError is the dual of the pause case.
func reconcileStatusAfterResumeError(
	parentCtx context.Context,
	sb *cubeboxstore.CubeBox,
	task containerd.Task,
	resumeErr error,
) {
	queryCtx, cancel := context.WithTimeout(context.Background(), reconcileStatusTimeout)
	defer cancel()
	if ns, ok := namespaces.Namespace(parentCtx); ok && ns != "" {
		queryCtx = namespaces.WithNamespace(queryCtx, ns)
	}

	st, qerr := task.Status(queryCtx)
	if qerr != nil {
		log.G(parentCtx).Errorf(
			"reconcileStatusAfterResumeError: task.Status failed sandbox=%s resumeErr=%v statusErr=%v",
			sb.ID, resumeErr, qerr)
		return
	}

	switch st.Status {
	case containerd.Running:
		// The shim has actually resumed successfully; likewise invalidate the
		// runtime snapshot bindings to stay consistent with the
		// UpdateWithResume success path.
		log.G(parentCtx).Warnf(
			"reconcileStatusAfterResumeError: shim reports RUNNING despite resumeErr=%v, converging sandbox=%s",
			resumeErr, sb.ID)
		convergeResumeStateAfterOpaqueRestore(sb, time.Now().UTC())
	case containerd.Paused:
		// Really not resumed, the state stays PAUSED and needs no rewrite (the
		// success path has not run yet).
		log.G(parentCtx).Warnf(
			"reconcileStatusAfterResumeError: shim still PAUSED resumeErr=%v sandbox=%s",
			resumeErr, sb.ID)
	default:
		log.G(parentCtx).Warnf(
			"reconcileStatusAfterResumeError: shim reports %s, leaving status untouched sandbox=%s",
			st.Status, sb.ID)
	}
}

// reconcileStuckPausingSandbox is the startup/background fallback -- the third
// line of defense for the PAUSING state behind reconcileStatusAfterPauseError
// (RPC path) and the /tasks/paused event subscription (events.go). If the
// cubelet crashes or restarts while a pause is in flight, neither of those
// fires again (events are not replayed, the RPC caller is gone), so without
// this a sandbox could stay stuck at PAUSING forever: DeadGC otherwise skips
// paused/pausing sandboxes outright.
//
// The caller (DeadGC) MUST only invoke this once PausingAt has lingered past
// pausingStuckThreshold, i.e. long after any genuine in-flight pause would
// have released the shim's sandbox mutex, so the ttrpc status query below
// cannot race it. Unlike cubes.RecoverContainer it never stamps Unknown=true,
// so it cannot trigger a spurious Terminated/Destroy cascade.
func reconcileStuckPausingSandbox(ctx context.Context, client *containerd.Client, cb *cubeboxstore.CubeBox) {
	fc := cb.FirstContainer()
	if fc == nil {
		return
	}
	ns := cb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	queryCtx, cancel := context.WithTimeout(context.Background(), reconcileStatusTimeout)
	defer cancel()
	queryCtx = namespaces.WithNamespace(queryCtx, ns)

	cntr := fc.Container
	if cntr == nil {
		loaded, err := client.LoadContainer(queryCtx, fc.ID)
		if err != nil {
			log.G(ctx).Errorf(
				"reconcileStuckPausingSandbox: load container %s failed sandbox=%s err=%v",
				fc.ID, cb.ID, err)
			return
		}
		cntr = loaded
	}
	task, err := cntr.Task(queryCtx, nil)
	if err != nil {
		log.G(ctx).Errorf(
			"reconcileStuckPausingSandbox: load task failed sandbox=%s err=%v", cb.ID, err)
		return
	}
	st, err := task.Status(queryCtx)
	if err != nil {
		log.G(ctx).Errorf(
			"reconcileStuckPausingSandbox: task.Status failed sandbox=%s err=%v", cb.ID, err)
		return
	}

	stuckFor := time.Duration(0)
	if pausingAt := cb.GetStatus().Get().PausingAt; pausingAt != 0 {
		stuckFor = time.Since(time.Unix(0, pausingAt))
	}
	convergePauseStateFromShim(ctx, cb, st.Status,
		fmt.Sprintf("DeadGC stuck PAUSING for %s", stuckFor))
}
