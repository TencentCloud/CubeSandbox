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
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
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

	unlock := s.sandboxLifecycleLocks.Lock(req.SandboxID)
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

func addSandboxTaskMetaData(ctx context.Context, sandboxID string) context.Context {
	md, ok := ttrpc.GetMetadata(ctx)
	if !ok {
		md = ttrpc.MD{}
	}
	md.Append("pod_scope", sandboxID)
	ctx = ttrpc.WithMetadata(ctx, md)
	tmpmd, _ := ttrpc.GetMetadata(ctx)
	log.G(ctx).Debugf("metadata:%+v", tmpmd)
	return ctx
}

func addPauseResumeMetaData(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest) context.Context {
	return addSandboxTaskMetaData(ctx, req.SandboxID)
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
		// Split the non-paused case into two: an already-running sandbox is
		// the *goal state* of resume, so we surface it as TaskStateInvalid
		// (130490) which the CLM / CubeProxy treat as an idempotent
		// success and use to reconcile a stale local "paused" cache. Any
		// other non-paused state (Exited / Unknown / Created / ...) is a
		// genuine resume failure and keeps TaskResumeFailed (130589).
		state := sb.GetStatus().Get().State()
		if state == cubebox.ContainerState_CONTAINER_RUNNING {
			rsp.Ret.RetCode = errorcode.ErrorCode_TaskStateInvalid
			rsp.Ret.RetMsg = "sandbox already running"
		} else {
			rsp.Ret.RetCode = errorcode.ErrorCode_TaskResumeFailed
			rsp.Ret.RetMsg = fmt.Sprintf("sandbox not resumable in state=%s", state)
		}
		return rsp, nil
	}

	log.G(ctx).Infof("UpdateWithResume:%s", utils.InterfaceToString(req))
	now := time.Now()
	result := s.resumeLocked(ctx, sb, resumeOptions{
		taskDeadline:      now.Add(taskResumeTimeout),
		reconcileDeadline: now.Add(taskResumeTimeout + reconcileStatusTimeout),
		persist:           true,
	})
	// Preserve the explicit Resume contract: an RPC error remains visible to
	// its caller even when reconciliation has already converged the local state
	// to RUNNING. Delete has a different terminal goal and handles that result
	// separately in resumePausedSandboxForDestroy.
	rsp.Ret = result.ret
	return rsp, nil
}

// resumeTask is the narrow part of containerd.Task needed by the resume
// transition. Keeping the core on this interface makes its timeout and
// reconciliation rules deterministic to test without a live containerd task.
type resumeTask interface {
	Resume(context.Context) error
	Status(context.Context) (containerd.Status, error)
}

type resumeOptions struct {
	taskDeadline      time.Time
	reconcileDeadline time.Time
	persist           bool
}

type resumeResult struct {
	ret               *errorcode.Ret
	running           bool
	reconciledRunning bool
}

// resumeLocked performs the pause -> running transition while the caller owns
// the sandbox lifecycle lock. It does not validate the current state because
// explicit Resume and Destroy intentionally apply different state policies.
func (s *service) resumeLocked(ctx context.Context, sb *cubeboxstore.CubeBox, opts resumeOptions) resumeResult {
	// The caller's runtime resume budget covers task loading and the resume RPC.
	// Delete persists the converged state with its normal delete marker after
	// this function returns, so it avoids a second synchronous store write in
	// the bounded wake-up phase.
	preflightCtx, preflightCancel := context.WithDeadline(ctx, opts.taskDeadline)
	defer preflightCancel()

	if rejected := s.admitResume(preflightCtx, sb); rejected != nil {
		return resumeResult{ret: rejected}
	}
	if err := preflightCtx.Err(); err != nil {
		return resumeResult{ret: &errorcode.Ret{
			RetCode: errorcode.ErrorCode_TaskResumeFailed,
			RetMsg:  err.Error(),
		}}
	}

	ns := sb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	syncCtx := namespaces.WithNamespace(ctx, ns)
	preflightCtx = namespaces.WithNamespace(preflightCtx, ns)
	firstContainer := sb.FirstContainer()
	if firstContainer == nil || firstContainer.Container == nil {
		return resumeResult{ret: &errorcode.Ret{
			RetCode: errorcode.ErrorCode_TaskResumeFailed,
			RetMsg:  "failed to load task for paused sandbox",
		}}
	}
	task, err := firstContainer.Container.Task(preflightCtx, nil)
	if err != nil {
		return resumeResult{ret: &errorcode.Ret{
			RetCode: errorcode.ErrorCode_TaskResumeFailed,
			RetMsg:  err.Error(),
		}}
	}

	preflightCtx = addSandboxTaskMetaData(preflightCtx, sb.ID)
	if opts.persist {
		// Explicit Resume persists both an ordinary successful transition and a
		// reconciliation that discovers the VM became RUNNING despite an RPC
		// error. DELETE persists its converged RUNNING state immediately after
		// this function returns, before it writes the delete marker.
		defer s.cubeboxMgr.cubeboxManger.SyncByID(syncCtx, sb.ID)
	}
	defer utils.Recover()

	return resumeTaskLocked(preflightCtx, sb, task, opts)
}

func resumeTaskLocked(ctx context.Context, sb *cubeboxstore.CubeBox, task resumeTask, opts resumeOptions) resumeResult {
	resumeCtx, resumeCancel := context.WithDeadline(ctx, opts.taskDeadline)
	defer resumeCancel()
	if err := task.Resume(resumeCtx); err != nil {
		// A ttrpc error can race a completed resume. Reconcile against the shim
		// within the caller-supplied budget before deciding that deletion must
		// stop. Explicit Resume still returns this original RPC error.
		reconciledRunning := reconcileStatusAfterResumeError(ctx, sb, task, err, opts.reconcileDeadline)
		return resumeResult{
			ret: &errorcode.Ret{
				RetCode: errorcode.ErrorCode_TaskResumeFailed,
				RetMsg:  err.Error(),
			},
			running:           reconciledRunning,
			reconciledRunning: reconciledRunning,
		}
	}
	convergeResumeStateAfterOpaqueRestore(sb, time.Now().UTC())
	return resumeResult{
		ret:     &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
		running: true,
	}
}

// admitResume enforces the resume side of the release-paused-resources policy.
// When the policy is off it is a no-op (resume is always admitted, matching
// the legacy guaranteed-resume behaviour). When on, pausing this sandbox
// already released its CPU/memory quota so the scheduler could fill the node;
// resuming would re-add that demand, so we re-check against the node's local,
// real-time quota usage and reject when the sandbox no longer fits. Rejecting
// here -- rather than overcommitting host RAM and risking an OOM -- is the
// explicit trade-off the policy makes.
//
// It returns a non-nil business error when resume must be rejected, or nil to
// proceed. Under a release ratio r the still-paused sandbox
// already contributes (1-r) of its quota to aggregateAllocated, so resuming it
// (a full reservation) only adds the remaining r fraction; we check that
// incremental demand against the node quota to model the post-resume state.
//
// The check is best-effort, not a hard reservation: concurrent resumes of
// different sandboxes each read their own snapshot of aggregateAllocated, so a
// burst can momentarily admit past the quota (the OS/cgroup remains the hard
// backstop). This matches how create scheduling is optimistic too; operators
// who want more headroom can lower the release ratio so the reserved fraction
// absorbs concurrent-resume overshoot.
func (s *service) admitResume(
	ctx context.Context,
	sb *cubeboxstore.CubeBox,
) *errorcode.Ret {
	// GetHostConf always returns a non-nil config (it falls back to defaults).
	hostConf := config.GetHostConf()
	releaseRatio := clampRatio(hostConf.Quota.PausedResourceReleaseRatio)
	if releaseRatio <= 0 {
		// Policy off: paused sandboxes kept their full quota, so resume is
		// guaranteed and needs no admission check (legacy behaviour).
		return nil
	}
	if sb.ResourceWithOverHead == nil {
		// Policy active but we cannot size this sandbox's demand. Fail closed
		// (reject) rather than silently admit and risk overcommit; this also
		// surfaces any sandbox missing resource metadata instead of hiding it.
		ret := &errorcode.Ret{
			RetCode: errorcode.ErrorCode_Conflict,
			RetMsg:  "resume rejected by paused_resource_release_ratio policy: missing resource metadata",
		}
		log.G(ctx).Warnf("admitResume reject sandbox=%s: missing resource metadata under paused_resource_release_ratio policy", sb.ID)
		return ret
	}

	// Recomputed live (no cache) on purpose: admission must reflect the current
	// allocation to avoid overcommit, and resume is far rarer than its VM-wake
	// cost, so this O(n) scan is acceptable on this path.
	alloc := s.cubeboxMgr.aggregateAllocated()
	memQuotaMB := int64(0)
	if memQuota, err := resource.ParseQuantity(hostConf.Quota.Mem); err == nil {
		memQuotaMB = bytesToMB(memQuota.Value())
	}
	// Only the released fraction is the incremental demand of resuming.
	needMemMB, needCPU := resumeDemand(sb.ResourceWithOverHead, releaseRatio)

	if reason := resumeQuotaRejection(resumeQuotaCheck{
		usedMemMB:     alloc.MemoryMB,
		needMemMB:     needMemMB,
		memQuotaMB:    memQuotaMB,
		usedCPUMilli:  alloc.MilliCPU,
		needCPUMilli:  needCPU,
		cpuQuotaMilli: int64(hostConf.Quota.Cpu),
	}); reason != "" {
		ret := &errorcode.Ret{
			RetCode: errorcode.ErrorCode_Conflict,
			RetMsg:  "resume rejected by paused_resource_release_ratio policy: " + reason,
		}
		// Conflict (not TaskResumeFailed) so the capacity rejection surfaces as
		// HTTP 409 at the API edge: this is an expected, retriable state (free
		// up capacity and retry), not a backend failure that should read as a
		// 500. The descriptive RetMsg is carried through to the client.
		log.G(ctx).Warnf("admitResume reject sandbox=%s: %s", sb.ID, ret.RetMsg)
		return ret
	}

	return nil
}

// resumeDemand returns the incremental CPU/memory a resume re-adds to the node
// under releaseRatio. While paused, the sandbox already contributes (1-ratio)
// of its quota to aggregateAllocated, so resuming it (a full reservation) only
// adds back the released `ratio` fraction. The memory side scales at byte
// precision and truncates to MB only at the very end, identical to
// aggregateSandboxResources, so the admission and accounting paths cannot
// disagree by a sub-MB rounding gap. Callers must pass a clamped ratio.
func resumeDemand(r *cubeboxstore.ResourceWithOverHead, releaseRatio float64) (needMemMB, needCPUMilli int64) {
	needMemMB = bytesToMB(scaleInt64(r.HostMemQ.Value(), releaseRatio))
	needCPUMilli = scaleInt64(r.HostCpuQ.MilliValue(), releaseRatio)
	return needMemMB, needCPUMilli
}

// resumeQuotaCheck bundles the post-resume quota inputs so call sites name each
// value explicitly (six positional int64s are easy to transpose) and future
// dimensions (e.g. disk) can be added without reshuffling arguments.
type resumeQuotaCheck struct {
	usedMemMB, needMemMB, memQuotaMB          int64
	usedCPUMilli, needCPUMilli, cpuQuotaMilli int64
}

// resumeQuotaRejection is the pure decision behind admitResume: it returns a
// non-empty human-readable reason when bringing a paused sandbox back would
// push the node past its CPU or memory quota, or "" when the resume fits. A
// non-positive quota means "unbounded" for that dimension and is skipped, which
// matches how the rest of the cubelet treats an unset host quota.
func resumeQuotaRejection(c resumeQuotaCheck) string {
	// NOTE: these two reason formats are an explicit cross-language contract:
	// the WebUI parses them by regex in web/src/lib/sandboxActionError.ts
	// (formatSandboxActionError) to render the localized capacity banner. Keep
	// the exact wording ("need %dMB + used %dMB > mem quota %dMB" and the "%dm"
	// CPU variant) in sync with that regex, or the frontend silently falls back
	// to a generic message. TestResumeQuotaRejectionMessageFormat locks the
	// format on this side.
	if c.memQuotaMB > 0 && c.usedMemMB+c.needMemMB > c.memQuotaMB {
		return fmt.Sprintf("need %dMB + used %dMB > mem quota %dMB",
			c.needMemMB, c.usedMemMB, c.memQuotaMB)
	}
	if c.cpuQuotaMilli > 0 && c.usedCPUMilli+c.needCPUMilli > c.cpuQuotaMilli {
		return fmt.Sprintf("need %dm + used %dm > cpu quota %dm",
			c.needCPUMilli, c.usedCPUMilli, c.cpuQuotaMilli)
	}
	return ""
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
			// A paused task restored after Cubelet restart may have only
			// PausedAt persisted. Once the shim proves it resumed, restore the
			// running marker so normal destroy does not treat it as terminated.
			if status.StartedAt == 0 {
				status.StartedAt = attachedAt.UnixNano()
			}
			return status, nil
		})
	}
}

// reconcileStatusAfterResumeError is the dual of the pause case. It returns
// true only when the shim proves the VM is RUNNING and local state has been
// converged accordingly. The fresh query context is bounded by deadline so a
// caller with a short operation budget cannot spend extra time reconciling.
func reconcileStatusAfterResumeError(
	parentCtx context.Context,
	sb *cubeboxstore.CubeBox,
	task resumeTask,
	resumeErr error,
	deadline time.Time,
) bool {
	queryTimeout, ok := boundedReconcileTimeout(deadline)
	if !ok {
		log.G(parentCtx).Errorf(
			"reconcileStatusAfterResumeError: deadline exhausted sandbox=%s resumeErr=%v",
			sb.ID, resumeErr)
		return false
	}
	queryCtx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	if ns, ok := namespaces.Namespace(parentCtx); ok && ns != "" {
		queryCtx = namespaces.WithNamespace(queryCtx, ns)
	}

	st, qerr := task.Status(queryCtx)
	if qerr != nil {
		log.G(parentCtx).Errorf(
			"reconcileStatusAfterResumeError: task.Status failed sandbox=%s resumeErr=%v statusErr=%v",
			sb.ID, resumeErr, qerr)
		return false
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
		return true
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
	return false
}

func boundedReconcileTimeout(deadline time.Time) (time.Duration, bool) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, false
	}
	if remaining < reconcileStatusTimeout {
		return remaining, true
	}
	return reconcileStatusTimeout, true
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
