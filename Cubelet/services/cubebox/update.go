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
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/proto"

	"github.com/tencentcloud/CubeSandbox/Cubelet/network"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubelethost "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/telemetry"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func (s *service) Update(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest) (*cubebox.UpdateCubeSandboxResponse, error) {
	rsp := &cubebox.UpdateCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	ctx = telemetry.ExtractGRPC(ctx)
	action := req.GetAnnotations()[constants.MasterAnnotationsUpdateAction]
	var pauseRoot trace.Span
	if action == constants.UpdateActionPause {
		ctx, pauseRoot = telemetry.Start(ctx, telemetry.SpanPause,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String(telemetry.AttrRequestID, req.GetRequestID()),
				attribute.String(telemetry.AttrSandboxID, req.GetSandboxID()),
				attribute.String(telemetry.AttrAction, action),
			))
		// Registered before the logging defer so it ends last, once rsp is final.
		defer func() { telemetry.EndWithCode(pauseRoot, int(rsp.GetRet().GetRetCode())) }()
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

	if action == "" {
		rsp.Ret.RetMsg = "must provide update action"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
	rt.CalleeAction = action

	var pauseLockSpan trace.Span
	if pauseRoot != nil {
		_, pauseLockSpan = telemetry.StartIfTraced(ctx, telemetry.SpanPauseLock,
			trace.WithAttributes(attribute.String(telemetry.AttrSandboxID, req.SandboxID)))
	}
	unlock := s.sandboxLifecycleLocks.Lock(req.SandboxID)
	if pauseLockSpan != nil {
		pauseLockSpan.End()
	}
	defer unlock()
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("Update panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = fmt.Sprintf("Update panic info:%s", panicError)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, req.SandboxID)
	if err != nil {
		if action == constants.UpdateActionResume {
			rsp.Ret.RetMsg = "pause resume is owned by CubeMaster Create; Update(resume) is not supported"
			rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
			return rsp, nil
		}
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
		// Pause is CubeCow-only: Master-allocated snap-* + PauseToSnapshot + shim exit.
		if !storage.IsCowBackend() {
			rsp.Ret.RetMsg = "pause requires cubecow storage backend; legacy pausevm is removed"
			rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
			return rsp, nil
		}
		// Rebind rsp so the deferred root span and logging see the real code.
		res, err := s.updateWithPauseCow(ctx, req, sb)
		rsp = res
		return res, err
	case constants.UpdateActionNetwork:
		return s.updateNetworkPolicy(ctx, req, sb, rsp)
	case constants.UpdateActionResume:
		// Resume is Master Create(same sandboxID from pause snap), not Update(resume).
		rsp.Ret.RetMsg = "pause resume is owned by CubeMaster Create; Update(resume) is not supported"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
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

// admitResume enforces the resume side of the release-paused-resources policy.
// When the policy is off it is a no-op (resume is always admitted, matching
// the legacy guaranteed-resume behaviour). When on, pausing this sandbox
// already released its CPU/memory quota so the scheduler could fill the node;
// resuming would re-add that demand, so we re-check against the node's local,
// real-time quota usage and reject when the sandbox no longer fits. Rejecting
// here -- rather than overcommitting host RAM and risking an OOM -- is the
// explicit trade-off the policy makes.
//
// It returns a release func plus a business error: on rejection (or no-op)
// release is noopRelease and the error is non-nil; on admission the error is
// nil and the caller MUST defer release() for as long as the resume is in
// flight (until the create flow's Save folds the demand into the store
// aggregate, or the failed attempt's synchronous rollback has restored the
// tombstone). Admission is a hard node-level reservation, not a best-effort
// snapshot: the check adds the in-flight reservations to the node aggregate
// and registers this sandbox's demand under the ledger lock, so concurrent
// resumes of different sandboxes serialize at the gate and cannot both pass
// on a stale snapshot (#2042). The aggregate itself is served from a short
// TTL cache (aggregateAllocatedCached) read just before the lock, keeping the
// critical section O(1) in the common case; the reservations, which are the
// part that must be exact between concurrent admissions, are always read
// live under the lock. See resumeReservationLedger for what the reservation
// covers and why every window over- (never under-)counts.
//
// Under a release ratio r the still-paused sandbox already contributes (1-r)
// of its quota to the aggregate, so resuming it only adds the remaining r
// fraction: the check compares that incremental demand and the reservation
// records exactly it. That is safe because the tombstone is guaranteed to
// stay visible in the store until the flow's Save overwrites it in place —
// the replacement branch never deletes the row, and a failed attempt gets it
// back from the synchronous rollback under the lifecycle lock — so
// T + r·F = F holds for the whole pre-Save window and only the post-Save
// tail over-counts, by r·F, until the deferred release.
//
// The quota itself is resolved exactly like the node status report resolves
// it: an unset mcpu_limit/mem_limit means "derive from the host" (CPU count ×
// overcommit, MemTotal × mem-overcommit), never "unbounded" — the shipped
// configs/single-node/cubelet.yaml leaves both unset, and an admission gate
// that skipped a dimension there would leave #2042 open on every default
// deployment. A dimension whose derivation fails (0) is skipped, matching
// what the node reports to CubeMaster in that case.
//
// The rejection boundary matches the scheduler's mem/cpu filters, which
// reject when free <= need: a resume that would fill the node exactly is
// rejected here too, so the same allocation cannot be refused as an ordinary
// create yet admitted as a resume.
func (s *service) admitResume(
	ctx context.Context,
	sb *cubeboxstore.CubeBox,
) (func(), *errorcode.Ret) {
	// GetHostConf always returns a non-nil config (it falls back to defaults).
	hostConf := config.GetHostConf()
	releaseRatio := clampRatio(hostConf.Quota.PausedResourceReleaseRatio)
	if releaseRatio <= 0 {
		// Policy off: paused sandboxes kept their full quota, so resume is
		// guaranteed and needs no admission check (legacy behaviour).
		return noopRelease, nil
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
		return noopRelease, ret
	}

	// Everything expensive stays outside the node-wide ledger lock: the quota
	// derivation may read the CPU count and MemTotal when the config leaves
	// the quota unset, and the aggregate scan is absorbed by the TTL cache.
	memQuotaMB := cubelethost.ResolveHostQuotaMemMB(hostConf)
	cpuQuotaMilli := cubelethost.ResolveHostQuotaCPUMilli(hostConf)
	// Only the released fraction is the incremental demand of resuming.
	needMemMB, needCPUMilli := resumeDemand(sb.ResourceWithOverHead, releaseRatio)
	alloc := s.cubeboxMgr.aggregateAllocatedCached()

	id := sb.SandboxID
	if id == "" {
		id = sb.ID
	}

	// One critical section for check-and-register: the live reservation
	// totals plus the aggregate snapshot above decide, and the demand is
	// registered before the lock is released, so no concurrent admission can
	// slip between observing and registering.
	ledger := &s.cubeboxMgr.reservations
	ledger.mu.Lock()
	reservedMemMB, reservedCPUMilli := ledger.totalsLocked()
	reason := resumeQuotaRejection(resumeQuotaCheck{
		// In-flight reservations count as used: they are admitted demand the
		// store cannot show yet (or still shows as a tombstone fraction).
		usedMemMB:     alloc.MemoryMB + reservedMemMB,
		needMemMB:     needMemMB,
		memQuotaMB:    memQuotaMB,
		usedCPUMilli:  alloc.MilliCPU + reservedCPUMilli,
		needCPUMilli:  needCPUMilli,
		cpuQuotaMilli: cpuQuotaMilli,
	})
	var release func()
	var replaced *resumeReservation
	if reason == "" {
		release, replaced = ledger.reserveLocked(id, needMemMB, needCPUMilli)
	}
	ledger.mu.Unlock()

	if reason != "" {
		ret := &errorcode.Ret{
			RetCode: errorcode.ErrorCode_Conflict,
			RetMsg:  "resume rejected by paused_resource_release_ratio policy: " + reason,
		}
		// Conflict (not TaskResumeFailed) so the capacity rejection surfaces as
		// HTTP 409 at the API edge: this is an expected, retriable state (free
		// up capacity and retry), not a backend failure that should read as a
		// 500. The descriptive RetMsg is carried through to the client.
		log.G(ctx).Warnf("admitResume reject sandbox=%s: %s", sb.ID, ret.RetMsg)
		return noopRelease, ret
	}
	if replaced != nil {
		// Logged outside the critical section, on the request context so the
		// line carries requestID and trace. An overwrite means two admissions
		// for one sandbox ran concurrently — the lifecycle lock makes that
		// impossible for the shapes Master sends, so this is the tripwire for
		// a caller breaking the request-shape contract (see
		// resumeReservationLedger).
		log.G(ctx).Warnf("resume reservation for %s overwrites an in-flight reservation (mem %d→%d MB, cpu %d→%d milli); admissions for one sandbox must be serialized by its lifecycle lock",
			id, replaced.memMB, needMemMB, replaced.cpuMilli, needCPUMilli)
	}
	return release, nil
}

// admitPausedReplace applies the admitResume quota check to the Create-from-pause
// path. Since the cubecow pause rework, Resume no longer goes through
// Update(resume) — that action answers "not supported": CubeMaster sends a
// thin Create carrying cube.master.desired.sandbox.id and Cubelet recreates
// the sandbox from the pause package, replacing the local PAUSED tombstone. Without this gate a
// same-node resume (XFS is always pinned to the origin; Master placement never
// consults the scheduler for it) bypasses the node CPU/memory quota entirely
// once paused_resource_release_ratio has released the paused sandbox's share —
// the exact allocation an ordinary create would be rejected for.
//
// It returns a release func plus a business error, mirroring admitResume: the
// caller defers release() so the node-level reservation covers the whole
// create flow and is dropped exactly once on every exit path. There is nothing
// to check when the request is not a paused replacement or this node holds no
// PAUSED record for it (cross-node restore: Master's restoreplace.Select
// already ran the scheduler filters against the target node). A real store
// error while resolving the tombstone fails closed: under the release-ratio
// policy an unresolvable local state must never silently admit a resume, and
// even with the policy off the replacement branch in the create flow needs a
// trustworthy answer to avoid colliding with a live sandbox.
func (s *service) admitPausedReplace(
	ctx context.Context,
	req *cubebox.RunCubeSandboxRequest,
) (func(), *errorcode.Ret) {
	sb, paused, err := pausedReplaceTarget(ctx, s.cubeboxMgr.cubeboxManger, req)
	if err != nil {
		if clampRatio(config.GetHostConf().Quota.PausedResourceReleaseRatio) <= 0 {
			// Policy off: admission is a no-op by design (legacy guaranteed
			// resume); the shim gate upstream of this call still fails closed
			// on the same store error, so nothing unsafe slips through here.
			return noopRelease, nil
		}
		// Policy on: the local state is unknown, so the quota cannot be
		// checked. Fail closed — admitting here is exactly the silent
		// quota bypass #2042 asks to prevent. Conflict keeps the rejection
		// retriable at the API edge once the store recovers.
		ret := &errorcode.Ret{
			RetCode: errorcode.ErrorCode_Conflict,
			RetMsg:  fmt.Sprintf("resume rejected by paused_resource_release_ratio policy: cannot read the local sandbox state: %v", err),
		}
		log.G(ctx).Warnf("admitPausedReplace fail closed sandbox=%s: %v",
			strings.TrimSpace(req.GetAnnotations()[constants.MasterAnnotationDesiredSandboxID]), err)
		return noopRelease, ret
	}
	if !paused {
		// Only a PAUSED tombstone is a resume replacement; live or
		// pausing-in-flight collisions were already rejected upstream by
		// gatePausedReplace, before the flow allocated anything.
		return noopRelease, nil
	}
	return s.admitResume(ctx, sb)
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
// fill or exceed the node's CPU or memory quota, or "" when the resume fits.
// A non-positive quota means the dimension could not be resolved (neither
// configured nor derivable from the host) and is skipped, matching what the
// node status report tells CubeMaster in that case.
//
// The boundary is >= (reject when free <= need), matching the scheduler's
// memfilter/cpufilter, which refuse a node when quotaMemFree <= need. A resume
// that would fill the node to exactly 100% is rejected here too, so the same
// allocation is never refused as an ordinary create yet admitted as a resume
// (the #2042 baseline: used 6144 + need 2048 vs quota 8192 must be rejected on
// both paths).
func resumeQuotaRejection(c resumeQuotaCheck) string {
	// NOTE: these two reason formats are an explicit cross-language contract:
	// the WebUI parses them by regex in web/src/lib/sandboxActionError.ts
	// (formatSandboxActionError) to render the localized capacity banner. Keep
	// the exact wording ("need %dMB + used %dMB >= mem quota %dMB" and the
	// "%dm" CPU variant) in sync with that regex, or the frontend silently
	// falls back to a generic message. TestResumeQuotaRejectionMessageFormat
	// locks the format on this side. The operator is ">=" — not ">" — so the
	// message stays truthful at the rejection boundary: an exact fill is
	// rejected, and its numbers read as equal because they are.
	if c.memQuotaMB > 0 && c.usedMemMB+c.needMemMB >= c.memQuotaMB {
		return fmt.Sprintf("need %dMB + used %dMB >= mem quota %dMB",
			c.needMemMB, c.usedMemMB, c.memQuotaMB)
	}
	if c.cpuQuotaMilli > 0 && c.usedCPUMilli+c.needCPUMilli >= c.cpuQuotaMilli {
		return fmt.Sprintf("need %dm + used %dm >= cpu quota %dm",
			c.needCPUMilli, c.usedCPUMilli, c.cpuQuotaMilli)
	}
	return ""
}

// Upper bound for the Pause ttrpc call. 30s is used because cubeshim pausing
// a VM involves vCPU stop + device quiesce + memory eventual consistency,
// which is normally < 5s; 30s is a safety net to prevent the call from being
// stuck indefinitely when the upstream ctx is missing or blocked. Used
// together with the reconcile* error convergence.
const (
	taskPauseTimeout = 30 * time.Second

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
	invalidateRuntimeSnapshotBindingsAfterOpaqueRestore(sb)
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

// updateNetworkPolicy applies a new egress policy to a running sandbox. The
// caller owns the sandbox lifecycle lock.
//
// The datapath is converged first and the store second. Persisting first would
// risk claiming a policy that never reached the node; this way a failure leaves
// both the store and the node on the previous policy.
func (s *service) updateNetworkPolicy(
	ctx context.Context,
	req *cubebox.UpdateCubeSandboxRequest,
	sb *cubeboxstore.CubeBox,
	rsp *cubebox.UpdateCubeSandboxResponse,
) (*cubebox.UpdateCubeSandboxResponse, error) {
	cfg := req.GetCubeNetworkConfig()
	if cfg == nil {
		rsp.Ret.RetMsg = "must provide cube_network_config for network update"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
	if sb.GetStatus().IsPaused() {
		rsp.Ret.RetMsg = "cannot update network policy of a paused sandbox"
		rsp.Ret.RetCode = errorcode.ErrorCode_Conflict
		return rsp, nil
	}

	if err := network.UpdateSandboxNetworkPolicy(ctx, req.SandboxID, cfg); err != nil {
		log.G(ctx).Errorf("update network policy failed sandbox=%s err=%v", req.SandboxID, err)
		rsp.Ret.RetMsg = err.Error()
		if network.IsSandboxNetworkNotActive(err) {
			rsp.Ret.RetCode = errorcode.ErrorCode_Conflict
		} else {
			rsp.Ret.RetCode = errorcode.ErrorCode_UpdateNetworkFailed
		}
		return rsp, nil
	}

	// Mirror the new policy into the sandbox object: it is what pause packages,
	// so without this a pause/resume cycle would silently restore the policy the
	// sandbox was created with.
	sb.CubeNetworkConfig = proto.Clone(cfg).(*cubebox.CubeNetworkConfig)
	if err := s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID); err != nil {
		// The datapath already carries the new policy, so this is not a failed
		// update — only a durability gap that a later sync or resume may expose.
		log.G(ctx).Errorf("persist updated network policy failed sandbox=%s err=%v", sb.ID, err)
	}
	return rsp, nil
}
