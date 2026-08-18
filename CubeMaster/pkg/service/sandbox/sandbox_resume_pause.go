// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubeproxy"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	volrefcount "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// pauseCubeletRPCTimeout is the unified Pause budget (CubeAPI / SDK / Master
	//↔Cubelet). Cubelet finishes PauseToSnapshot + keep_tombstone Destroy in
	// one Update RPC and returns ext_info. On RPC timeout Master records FAILED;
	// Resume may heal only for timeout + Cubelet PAUSED. Explicit Cubelet
	// failure cannot Resume (delete only).
	pauseCubeletRPCTimeout = 120 * time.Second

	// alreadyPausedMarker is a stable machine token that Master appends to the
	// ret_msg ONLY when a pause is reported as idempotent already-paused
	// (TaskStateInvalid). CubeAPI keys its "redundant pause → HTTP 200" decision
	// on this exact marker rather than free-form message text, so that (a)
	// rewording / i18n of the human-readable prefix cannot silently regress a
	// redundant pause back to 500, and (b) a Cubelet-originated 130490 that Master
	// passes through verbatim (it never carries this marker) is never swallowed as
	// success. CONTRACT: keep this literal in sync with CubeAPI's
	// ALREADY_PAUSED_MARKER (CubeAPI/src/services/sandboxes.rs).
	alreadyPausedMarker = "[cube:already-paused]"
)

// pauseSandbox:
//  1. allocate snap-* and record CREATING binding
//  2. Cubelet Update(pause): PauseToSnapshot + in-process keep_tombstone Destroy
//  3. on success: ApplyFromExtInfo + mark pause binding READY
//
// Called under sandboxlock. Returns only after READY or FAILED is recorded so
// resume/delete cannot interleave. Never CleanupTemplate / delete the sandbox record.
func pauseSandbox(ctx context.Context, req *types.UpdateRequest, hostIP string) *types.Res {
	rsp := &types.Res{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	nodeID := ""
	if n, ok := localcache.GetNodesByIp(hostIP); ok {
		nodeID = n.ID()
	}

	// A leftover binding is what drives both the stale-RUNNING cleanup and the
	// already-paused convergence below; a clean pause (no binding) needs neither.
	// So only probe the live Cubelet state when a binding exists — a normal pause
	// issues zero List calls (as before this fix), and a retried auto-pause of a
	// stranded sandbox issues exactly one (both consumers share this single probe
	// instead of taking a List each).
	existing, getErr := pausesnap.GetBySandbox(ctx, req.SandboxID)
	if getErr != nil && !errors.Is(getErr, pausesnap.ErrNotFound) {
		log.G(ctx).Warnf("pause: get snapshot binding for sandbox %s: %v", req.SandboxID, getErr)
	}
	var (
		liveState cubebox.ContainerState
		liveFound bool
		probeIP   string
	)
	if existing != nil {
		probeIP = strings.TrimSpace(hostIP)
		if ip := strings.TrimSpace(existing.NodeIP); ip != "" {
			probeIP = ip
		}
		liveState, liveFound = probePauseLiveState(ctx, probeIP, req.SandboxID)

		// Resume success + failed pausesnap.Delete must not brick the next Pause:
		// clear the leftover binding only when the sandbox is confirmed RUNNING.
		if liveFound && liveState == cubebox.ContainerState_CONTAINER_RUNNING {
			clearStalePauseBinding(ctx, req.RequestID, probeIP, existing)
		}
	}

	snapID, err := pausesnap.Begin(ctx, req.SandboxID, nodeID, hostIP, req.InstanceType)
	if err != nil {
		// A leftover binding means the sandbox may already be paused — most
		// commonly when a prior pause RPC timed out on the caller side but
		// actually completed on Master/Cubelet. Report this as an idempotent
		// "already in state" (TaskStateInvalid) rather than a generic param error
		// so the caller (CLM auto-pause) recognises it and converges the dataplane
		// to paused instead of rolling proxy state back to running, which would
		// leave auto-resume unable to fire (HTTP 504 on the next request). Only
		// converge when the live probe confirms PAUSED: a stale binding on a
		// genuinely RUNNING sandbox (or an inconclusive probe) must stay a hard
		// error so CLM does not brick a running box. READY converges on a confirmed
		// PAUSED probe; FAILED additionally requires the failure to be a timeout.
		// Stale CREATING and timeout-FAILED bindings are healed to READY first.
		if shouldTreatAsAlreadyPaused(err, existing, liveState, liveFound) {
			status := strings.TrimSpace(existing.Status)
			if strings.EqualFold(status, pausesnap.StatusCreating) || strings.EqualFold(status, pausesnap.StatusFailed) {
				healedNodeID := strings.TrimSpace(existing.NodeID)
				if healedNodeID == "" {
					if n, ok := localcache.GetNodesByIp(probeIP); ok {
						healedNodeID = n.ID()
					}
				}
				if err := pausesnap.Complete(ctx, req.SandboxID, existing.SnapshotID, healedNodeID, probeIP, req.InstanceType, nil); err != nil {
					rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
					rsp.Ret.RetMsg = fmt.Sprintf("heal pause snapshot: %v", err)
					return rsp
				}
			}
			rsp.Ret.RetCode = int(errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid))
			rsp.Ret.RetMsg = fmt.Sprintf("%s begin pause snapshot: %v", alreadyPausedMarker, err)
			return rsp
		}
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = fmt.Sprintf("begin pause snapshot: %v", err)
		return rsp
	}

	calleeEndpoint := cubelet.GetCubeletAddr(hostIP)
	cubeletReq := &cubebox.UpdateCubeSandboxRequest{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Annotations: map[string]string{
			constants.CubeAnnotationsUpdateAction:     "pause",
			constants.CubeAnnotationsInsType:          req.InstanceType,
			constants.CubeAnnotationPauseSnapshotID:   snapID,
			constants.CubeAnnotationRuntimeSnapshotID: snapID,
		},
	}
	log.G(ctx).Infof("pause: sandbox=%s snap=%s host=%s", req.SandboxID, snapID, hostIP)
	cubeRsp, err := cubelet.UpdateWithTimeout(ctx, calleeEndpoint, cubeletReq, pauseCubeletRPCTimeout)
	if err != nil || cubeRsp == nil || cubeRsp.GetRet() == nil {
		msg := "cubelet pause response is nil"
		if err != nil {
			msg = err.Error()
		}
		// Timeout / incomplete RPC: FAILED. Resume may heal if Cubelet PAUSED.
		// Explicit nil-ret without err is also FAILED (no Resume).
		if err != nil && isPauseRPCTimeout(err) {
			log.G(ctx).Warnf("pause rpc timed out sandbox=%s snap=%s: %v", req.SandboxID, snapID, err)
		}
		pausesnap.MarkFailed(ctx, req.SandboxID, snapID, msg)
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = msg
		return rsp
	}
	rsp.Ret.RetCode = int(cubeRsp.GetRet().GetRetCode())
	rsp.Ret.RetMsg = cubeRsp.GetRet().GetRetMsg()
	// Apply volume ref deltas whenever Cubelet returned a response with
	// ext_info — including explicit Pause failure after a successful Detach
	// (node 1→0). Timeout / nil-response paths have no ext_info (do not adjust).
	volrefcount.ApplyFromExtInfo(ctx, cubeRsp.GetExtInfo())
	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		// Definitive Cubelet failure: cannot Resume (delete only).
		pausesnap.MarkFailed(ctx, req.SandboxID, snapID, rsp.Ret.RetMsg)
		return rsp
	}

	released := volrefcount.ReleasedVolumeIDs(cubeRsp.GetExtInfo())
	if err := pausesnap.Complete(ctx, req.SandboxID, snapID, nodeID, hostIP, req.InstanceType, released); err != nil {
		pausesnap.MarkFailed(ctx, req.SandboxID, snapID, err.Error())
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = fmt.Sprintf("pause ok on cubelet but master meta failed: %v", err)
		return rsp
	}
	runAfterUpdateSandboxSuccessHook(ctx, req.SandboxID, req.InstanceType, "pause", req.RequestID)
	return rsp
}

// shouldTreatAsAlreadyPaused decides whether a Begin rejection for a leftover
// binding should be reported as idempotent already-paused (TaskStateInvalid)
// rather than a hard param error. It is a pure classifier over the Begin error,
// the leftover binding, and the single live Cubelet probe result, so its logic
// is unit-testable without patching:
//
//   - READY (ErrAlreadyExists): converge only when the live probe CONFIRMS
//     PAUSED. A stale READY binding on a genuinely RUNNING sandbox, or an
//     inconclusive probe (probe failed / no container found), stays a hard error
//     so CLM does not converge a running sandbox to paused.
//   - FAILED: additionally require the failure to be an RPC timeout that the
//     Cubelet confirms PAUSED — the same signal recoverTimedOutPauseForResume
//     heals on. An explicit failure stays hard.
//   - CREATING: a retry has acquired the sandbox lock after the prior holder
//     exited, so a confirmed PAUSED probe identifies a stale crash-window binding.
func shouldTreatAsAlreadyPaused(beginErr error, rec *pausesnap.Record, liveState cubebox.ContainerState, liveFound bool) bool {
	if rec == nil || !liveFound || liveState != cubebox.ContainerState_CONTAINER_PAUSED {
		return false
	}
	if errors.Is(beginErr, pausesnap.ErrAlreadyExists) {
		return true
	}
	status := strings.TrimSpace(rec.Status)
	if strings.EqualFold(status, pausesnap.StatusCreating) {
		return true
	}
	return strings.EqualFold(status, pausesnap.StatusFailed) && isPauseTimeoutFailure(rec)
}

// probePauseLiveState returns the observed container state for a sandbox on the
// given node. liveFound is false when the probe could not be completed (List
// error / nil response) or the sandbox container was not found — callers must
// treat that as inconclusive, not as a confirmed absence of PAUSED.
func probePauseLiveState(ctx context.Context, hostIP, sandboxID string) (cubebox.ContainerState, bool) {
	hostIP = strings.TrimSpace(hostIP)
	if hostIP == "" {
		return cubebox.ContainerState_CONTAINER_UNKNOWN, false
	}
	req := &cubebox.ListCubeSandboxRequest{Id: &sandboxID}
	endpoint := cubelet.GetCubeletAddr(hostIP)
	listRsp, err := cubelet.List(ctx, endpoint, req)
	if err != nil || listRsp == nil {
		log.G(ctx).Warnf("pause: probe sandbox %s live state failed, retrying: %v", sandboxID, err)
		listRsp, err = cubelet.List(ctx, endpoint, req)
	}
	if err != nil || listRsp == nil {
		log.G(ctx).Warnf("pause: probe sandbox %s live state after retry: %v", sandboxID, err)
		return cubebox.ContainerState_CONTAINER_UNKNOWN, false
	}
	var observed cubebox.ContainerState
	found := false
	for _, item := range listRsp.GetItems() {
		if item.GetId() != sandboxID {
			continue
		}
		for _, c := range item.GetContainers() {
			if c.GetId() != sandboxID {
				continue
			}
			found = true
			observed = c.GetState()
			if observed == cubebox.ContainerState_CONTAINER_PAUSED {
				return observed, true
			}
		}
	}
	return observed, found
}

func cubeletReportsPaused(ctx context.Context, hostIP, sandboxID string) bool {
	state, found := probePauseLiveState(ctx, hostIP, sandboxID)
	return found && state == cubebox.ContainerState_CONTAINER_PAUSED
}

// recoverTimedOutPauseForResume heals a Master FAILED pause binding only when
// the failure was an RPC timeout and Cubelet already reports PAUSED. Completes
// READY with empty plugin-volume list (no ext_info → do not adjust refcount).
func recoverTimedOutPauseForResume(ctx context.Context, req *types.UpdateRequest, rec *pausesnap.Record, hostIP string) (*pausesnap.Record, error) {
	if req == nil || rec == nil {
		return nil, errors.New("cannot resume: pause snapshot status=FAILED")
	}
	if !isPauseTimeoutFailure(rec) {
		msg := "cannot resume: pause snapshot status=FAILED"
		if e := strings.TrimSpace(rec.LastError); e != "" {
			msg = fmt.Sprintf("%s (%s)", msg, e)
		}
		return nil, errors.New(msg)
	}
	probeIP := strings.TrimSpace(hostIP)
	if ip := strings.TrimSpace(rec.NodeIP); ip != "" {
		probeIP = ip
	}
	if probeIP == "" {
		return nil, fmt.Errorf("cannot resume: pause rpc timed out (no host to probe)")
	}
	if !cubeletReportsPaused(ctx, probeIP, req.SandboxID) {
		return nil, fmt.Errorf("cannot resume: pause rpc timed out and cubelet is not PAUSED")
	}
	nodeID := strings.TrimSpace(rec.NodeID)
	if nodeID == "" {
		if n, ok := localcache.GetNodesByIp(probeIP); ok {
			nodeID = n.ID()
		}
	}
	log.G(ctx).Warnf("resume: healing timed-out pause binding sandbox=%s snap=%s host=%s (cubelet PAUSED; no ext_info)",
		req.SandboxID, rec.SnapshotID, probeIP)
	// No ApplyFromExtInfo: timeout means Master never saw volume events.
	if err := pausesnap.Complete(ctx, req.SandboxID, rec.SnapshotID, nodeID, probeIP, req.InstanceType, nil); err != nil {
		return nil, fmt.Errorf("cannot resume: pause timeout heal complete failed: %w", err)
	}
	healed, err := pausesnap.GetBySandbox(ctx, req.SandboxID)
	if err != nil || healed == nil {
		return nil, fmt.Errorf("cannot resume: pause heal lost binding: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(healed.Status), "READY") {
		return nil, fmt.Errorf("cannot resume: pause heal left status=%s", healed.Status)
	}
	return healed, nil
}

func isPauseRPCTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.DeadlineExceeded {
		return true
	}
	return isPauseTimeoutMessage(err.Error())
}

func isPauseTimeoutFailure(rec *pausesnap.Record) bool {
	if rec == nil {
		return false
	}
	return isPauseTimeoutMessage(rec.LastError)
}

func isPauseTimeoutMessage(msg string) bool {
	e := strings.ToLower(strings.TrimSpace(msg))
	if e == "" {
		return false
	}
	return strings.Contains(e, "deadline exceeded") ||
		strings.Contains(e, "context deadline") ||
		strings.Contains(e, "client.timeout") ||
		(strings.Contains(e, "timeout") && strings.Contains(e, "rpc"))
}

// clearStalePauseBinding drops a leftover pause binding for a sandbox the caller
// has already confirmed is RUNNING (typical: Resume succeeded but pausesnap.Delete
// failed). The caller supplies the binding and the probe node so this does not
// re-probe the Cubelet. Never called for a real Paused sandbox's binding.
func clearStalePauseBinding(ctx context.Context, requestID, probeIP string, rec *pausesnap.Record) {
	if rec == nil || strings.TrimSpace(rec.SnapshotID) == "" || strings.TrimSpace(probeIP) == "" {
		return
	}
	log.G(ctx).Warnf("pause: clearing stale pause binding sandbox=%s snap=%s (sandbox is RUNNING)",
		rec.SandboxID, rec.SnapshotID)
	cleanupPauseSnapshotLocal(ctx, requestID, probeIP, rec.SnapshotID)
	if delErr := pausesnap.Delete(ctx, rec.SnapshotID); delErr != nil {
		log.G(ctx).Warnf("pause: delete stale binding %s: %v", rec.SnapshotID, delErr)
	}
}

// resumeFromPauseSnapshot asks Cubelet to Create with only sandboxID + snapID;
// Cubelet expands sandbox_spec.json (incl. volume mounts) and Attaches volumes.
func resumeFromPauseSnapshot(ctx context.Context, req *types.UpdateRequest, hostIP string) *types.Res {
	rsp := &types.Res{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	rec, err := pausesnap.GetBySandbox(ctx, req.SandboxID)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		if errors.Is(err, pausesnap.ErrNotFound) {
			rsp.Ret.RetMsg = fmt.Sprintf("no pause snapshot for sandbox %s", req.SandboxID)
		} else {
			rsp.Ret.RetMsg = fmt.Sprintf("load pause snapshot: %v", err)
		}
		return rsp
	}
	if !strings.EqualFold(strings.TrimSpace(rec.Status), "READY") {
		if strings.EqualFold(strings.TrimSpace(rec.Status), pausesnap.StatusFailed) {
			healed, healErr := recoverTimedOutPauseForResume(ctx, req, rec, hostIP)
			if healErr != nil {
				rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
				rsp.Ret.RetMsg = healErr.Error()
				return rsp
			}
			rec = healed
		} else {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			msg := fmt.Sprintf("cannot resume: pause snapshot status=%s", rec.Status)
			if e := strings.TrimSpace(rec.LastError); e != "" {
				msg = fmt.Sprintf("%s (%s)", msg, e)
			}
			rsp.Ret.RetMsg = msg
			return rsp
		}
	}
	snapID := rec.SnapshotID

	// Pause Detach released these plugin volumes (node 1→0). If the user
	// deleted any meanwhile, refuse Resume — Attach would otherwise recreate
	// orphan hostdirs (localdir) or bind missing backends.
	if err := validatePauseResumeVolumes(rec.PluginVolumeIDs); err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = err.Error()
		return rsp
	}

	targetIP := hostIP
	if rec.NodeIP != "" {
		targetIP = rec.NodeIP
	}
	instanceType := strings.TrimSpace(req.InstanceType)
	if instanceType == "" {
		instanceType = cubebox.InstanceType_cubebox.String()
	}

	// Thin Create: identity + snapshot binding. Containers/volumes come from
	// Cubelet sandbox_spec.json. Recreate-needed network fields are also
	// injected from Master sandboxspec (same store snapshot/template commit
	// uses) so legacy pause packages without cube_network_config still
	// rebuild egress policy; Cubelet prefers packed values when present.
	cubeletReq := &cubebox.RunCubeSandboxRequest{
		RequestID:    req.RequestID,
		InstanceType: instanceType,
		Annotations: map[string]string{
			constants.CubeAnnotationDesiredSandboxID:          req.SandboxID,
			constants.CubeAnnotationRuntimeSnapshotID:         snapID,
			constants.CubeAnnotationRuntimeSnapshotAttachedAt: time.Now().UTC().Format(time.RFC3339Nano),
			constants.CubeAnnotationPauseSnapshotID:           snapID,
		},
	}
	applyResumeRecreateFieldsFromSandboxSpec(ctx, req.SandboxID, cubeletReq)

	calleeEndpoint := cubelet.GetCubeletAddr(targetIP)
	log.G(ctx).Infof("resume-from-pause: sandbox=%s snap=%s host=%s", req.SandboxID, snapID, targetIP)
	cubeRsp, err := cubelet.Create(ctx, calleeEndpoint, cubeletReq)
	if err != nil || cubeRsp == nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		if err != nil {
			rsp.Ret.RetMsg = err.Error()
		} else {
			rsp.Ret.RetMsg = "cubelet create response is nil"
		}
		return rsp
	}
	if cubeRsp.GetRet() == nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_Unknown)
		rsp.Ret.RetMsg = "cubelet create response ret is nil"
		return rsp
	}
	rsp.Ret.RetCode = int(cubeRsp.GetRet().GetRetCode())
	rsp.Ret.RetMsg = cubeRsp.GetRet().GetRetMsg()
	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		return rsp
	}
	if got := strings.TrimSpace(cubeRsp.GetSandboxID()); got != "" && got != req.SandboxID {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_Unknown)
		rsp.Ret.RetMsg = fmt.Sprintf("pause resume returned different sandboxID %q (want %q)", got, req.SandboxID)
		return rsp
	}
	// Same as normal Create: apply node 0→1 (and only those) to Master.
	volrefcount.ApplyFromExtInfo(ctx, cubeRsp.GetExtInfo())

	// Resume recreates the guest NIC / host ports. Normal Create writes Redis
	// proxy metadata in dealSuccResult; thin resume Create must rewrite it or
	// CubeProxy keeps routing to the pre-pause SandboxIP (same-node 504).
	if err := refreshProxyMapAfterResume(ctx, req.SandboxID, targetIP, cubeRsp); err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_DBError)
		rsp.Ret.RetMsg = fmt.Sprintf("refreshSandboxProxyMap after resume fail:%s", err.Error())
		return rsp
	}
	// Drop CubeProxy local_cache so the new SandboxIP is used immediately
	// (cache hits renew TTL and would otherwise keep routing to the old NIC).
	cubeproxy.InvalidateBackendCache(ctx, req.SandboxID, targetIP)

	// G5: running sandbox is an independent copy — delete pause snap local + meta.
	cleanupPauseSnapshotLocal(ctx, req.RequestID, targetIP, snapID)
	if err := pausesnap.Delete(ctx, snapID); err != nil {
		log.G(ctx).Warnf("resume: delete pause snap meta %s: %v", snapID, err)
	}
	runAfterUpdateSandboxSuccessHook(ctx, req.SandboxID, req.InstanceType, "resume", req.RequestID)
	return rsp
}

// applyResumeRecreateFieldsFromSandboxSpec copies create-time network fields
// from Master sandboxspec onto a thin Resume Create. Cubelet expand prefers
// packed sandbox_spec.json values and uses these as fallback for older snaps.
// Ingress-only fields (AllowPublicTraffic / MaskRequestHost) stay on the
// Redis proxy map path and are intentionally not mapped to Cubelet.
func applyResumeRecreateFieldsFromSandboxSpec(ctx context.Context, sandboxID string, out *cubebox.RunCubeSandboxRequest) {
	if out == nil {
		return
	}
	createReq, err := sandboxspec.Get(ctx, sandboxID)
	if err != nil {
		if !errors.Is(err, sandboxspec.ErrSandboxSpecNotFound) && !errors.Is(err, sandboxspec.ErrSandboxSpecStoreNotReady) {
			log.G(ctx).Warnf("resume: load sandboxspec for %s: %v", sandboxID, err)
		}
		return
	}
	fillResumeRecreateFields(ctx, sandboxID, out, createReq)
}

func fillResumeRecreateFields(ctx context.Context, sandboxID string, out *cubebox.RunCubeSandboxRequest, createReq *types.CreateCubeSandboxReq) {
	if out == nil || createReq == nil {
		return
	}
	out.CubeNetworkConfig = mapCubeNetworkConfig(createReq.CubeNetworkConfig)
	if nt := strings.TrimSpace(createReq.NetworkType); nt != "" {
		out.NetworkType = nt
	}
	if rh := strings.TrimSpace(createReq.RuntimeHandler); rh != "" {
		out.RuntimeHandler = rh
	}
	if err := getExposedPorts(createReq, out); err != nil {
		// Non-fatal: Cubelet may still recover ports from packed annotations.
		log.G(ctx).Warnf("resume: restore exposed_ports from sandboxspec for %s: %v", sandboxID, err)
	}
}

// refreshProxyMapAfterResume rewrites sandbox→backend routing for CubeProxy.
// Preserves AllowPublicTraffic / TrafficAccessToken / MaskRequestHost /
// CreatedAt from the pre-pause entry when present.
func refreshProxyMapAfterResume(ctx context.Context, sandboxID, hostIP string, cubeRsp *cubebox.RunCubeSandboxResponse) error {
	sandboxID = strings.TrimSpace(sandboxID)
	hostIP = strings.TrimSpace(hostIP)
	if sandboxID == "" || hostIP == "" || cubeRsp == nil {
		return fmt.Errorf("missing sandboxID/hostIP/create response")
	}

	proxy := &proxytypes.SandboxProxyMap{
		HostIP:             hostIP,
		SandboxID:          sandboxID,
		SandboxIP:          strings.TrimSpace(cubeRsp.GetSandboxIP()),
		SandboxPort:        "8080",
		CreatedAt:          strconv.FormatInt(time.Now().UnixNano(), 10),
		AllowPublicTraffic: true,
	}
	if prev, ok := getSandboxProxyMapFn(ctx, sandboxID); ok && prev != nil {
		proxy.AllowPublicTraffic = prev.AllowPublicTraffic
		proxy.TrafficAccessToken = prev.TrafficAccessToken
		proxy.MaskRequestHost = prev.MaskRequestHost
		if strings.TrimSpace(prev.CreatedAt) != "" {
			proxy.CreatedAt = prev.CreatedAt
		}
		if strings.TrimSpace(prev.SandboxPort) != "" {
			proxy.SandboxPort = prev.SandboxPort
		}
	}
	if proxy.SandboxIP == "" {
		return fmt.Errorf("cubelet create response missing SandboxIP")
	}

	if cfg := config.GetConfig(); cfg != nil && cfg.CubeletConf != nil && cfg.CubeletConf.EnableExposedPort {
		ports := make(map[string]string, len(cubeRsp.GetPortMappings()))
		for _, m := range cubeRsp.GetPortMappings() {
			if m == nil {
				continue
			}
			ports[strconv.FormatInt(int64(m.GetContainerPort()), 10)] = strconv.FormatInt(int64(m.GetHostPort()), 10)
		}
		proxy.ContainerToHostPorts = ports
		if len(ports) == 0 {
			log.G(ctx).Warnf("resume: no port mapping in create response sandbox=%s", sandboxID)
		}
	}

	if err := setSandboxProxyMapFn(ctx, proxy); err != nil {
		return err
	}
	localcache.SetSandboxCache(sandboxID, &localcache.SandboxCache{
		SandboxID: sandboxID,
		HostIP:    hostIP,
	})
	log.G(ctx).Infof("resume: refreshed proxy map sandbox=%s host=%s sandboxIP=%s ports=%v",
		sandboxID, hostIP, proxy.SandboxIP, proxy.ContainerToHostPorts)
	return nil
}

func cleanupPauseSnapshotLocal(ctx context.Context, requestID, hostIP, snapID string) {
	snapID = strings.TrimSpace(snapID)
	hostIP = strings.TrimSpace(hostIP)
	if snapID == "" || hostIP == "" {
		return
	}
	// G5 after Resume: delete local pause snap only (no volume refcount change).
	rsp, err := cubelet.CleanupTemplate(ctx, cubelet.GetCubeletAddr(hostIP), &cubebox.CleanupTemplateRequest{
		RequestID:  requestID,
		TemplateID: snapID,
	})
	if err != nil {
		log.G(ctx).Warnf("resume: cleanup pause snap %s on %s: %v", snapID, hostIP, err)
		return
	}
	if rsp != nil && rsp.GetRet() != nil && int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		log.G(ctx).Warnf("resume: cleanup pause snap %s ret=%v msg=%s",
			snapID, rsp.GetRet().GetRetCode(), rsp.GetRet().GetRetMsg())
	}
}

// validatePauseResumeVolumes ensures plugin volumes released at Pause still
// exist before Resume Create/Attach.
func validatePauseResumeVolumes(volumeIDs []string) error {
	for _, vid := range volumeIDs {
		vid = strings.TrimSpace(vid)
		if vid == "" {
			continue
		}
		if _, err := resolveVolumeRecord(vid); err != nil {
			return fmt.Errorf("cannot resume: volume %s is unavailable (%v)", vid, err)
		}
	}
	return nil
}

// lookupPauseSnapshotID returns the Master-recorded pause snap id if any.
func lookupPauseSnapshotID(ctx context.Context, sandboxID string) string {
	if rec, err := pausesnap.GetBySandbox(ctx, sandboxID); err == nil && rec != nil {
		return rec.SnapshotID
	}
	return ""
}

// resolvePauseHostIP prefers the pause-snap replica node, then cache/proxy.
func resolvePauseHostIP(ctx context.Context, sandboxID string) (string, bool) {
	if rec, err := pausesnap.GetBySandbox(ctx, sandboxID); err == nil && rec != nil && rec.NodeIP != "" {
		return rec.NodeIP, true
	}
	if v := localcache.GetSandboxCache(sandboxID); v != nil && v.HostIP != "" {
		return v.HostIP, true
	}
	if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok && proxyMap != nil {
		return proxyMap.HostIP, true
	}
	return "", false
}
