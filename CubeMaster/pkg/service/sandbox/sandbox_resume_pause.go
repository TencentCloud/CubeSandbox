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

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubeproxy"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/restoreplace"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	volrefcount "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
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
)

var decidePauseResumePlacementFn = restoreplace.Decide

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

	// Resume success + failed pausesnap.Delete must not brick the next Pause.
	clearStalePauseBindingIfRunning(ctx, req.RequestID, req.SandboxID, hostIP)

	// Client-supplied backend is ignored. Pause follows the sandbox spec
	// Master persisted at create (itself inherited from the template).
	req.Backend = ""
	if spec, specErr := sandboxspec.Get(ctx, req.SandboxID); specErr == nil && spec != nil {
		req.Backend = spec.Backend
	}
	snapID, err := pausesnap.Begin(ctx, req.SandboxID, nodeID, hostIP, req.InstanceType, req.Backend)
	if err != nil {
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
	if b := strings.TrimSpace(req.Backend); b != "" {
		cubeletReq.Annotations[constants.CubeAnnotationStorageBackend] = b
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
	exportUUIDs := ""
	if constants.IsS3Backend(req.Backend) {
		exportUUIDs = strings.TrimSpace(cubeRsp.GetRemoteUuids())
		if exportUUIDs == "" {
			exportUUIDs = remoteUUIDsFromExtInfo(cubeRsp.GetExtInfo())
		}
		// Empty uuids means export failed or still pending on Cubelet.
		// Pause itself succeeded — same-node Resume does not need uuids;
		// Complete records remote_status=failed when uuids are empty.
	}
	if err := pausesnap.Complete(ctx, req.SandboxID, snapID, nodeID, hostIP, req.InstanceType, released, exportUUIDs); err != nil {
		pausesnap.MarkFailed(ctx, req.SandboxID, snapID, err.Error())
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = fmt.Sprintf("pause ok on cubelet but master meta failed: %v", err)
		return rsp
	}
	runAfterUpdateSandboxSuccessHook(ctx, req.SandboxID, req.InstanceType, "pause", req.RequestID)
	return rsp
}

func cubeletReportsPaused(ctx context.Context, hostIP, sandboxID string) bool {
	listRsp, err := cubelet.List(ctx, cubelet.GetCubeletAddr(hostIP), &cubebox.ListCubeSandboxRequest{
		Id: &sandboxID,
	})
	if err != nil || listRsp == nil {
		return false
	}
	for _, item := range listRsp.GetItems() {
		if item.GetId() != sandboxID {
			continue
		}
		for _, c := range item.GetContainers() {
			if c.GetId() == sandboxID && c.GetState() == cubebox.ContainerState_CONTAINER_PAUSED {
				return true
			}
		}
	}
	return false
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
	if err := pausesnap.Complete(ctx, req.SandboxID, rec.SnapshotID, nodeID, probeIP, req.InstanceType, nil, rec.ExportUUIDs); err != nil {
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

// clearStalePauseBindingIfRunning drops a leftover pause binding when the
// sandbox is already RUNNING (typical: Resume succeeded but pausesnap.Delete
// failed). Does not touch a real Paused sandbox's binding.
func clearStalePauseBindingIfRunning(ctx context.Context, requestID, sandboxID, hostIP string) {
	rec, err := pausesnap.GetBySandbox(ctx, sandboxID)
	if err != nil || rec == nil || strings.TrimSpace(rec.SnapshotID) == "" {
		return
	}
	probeIP := hostIP
	if ip := strings.TrimSpace(rec.NodeIP); ip != "" {
		probeIP = ip
	}
	if probeIP == "" {
		return
	}
	listRsp, err := cubelet.List(ctx, cubelet.GetCubeletAddr(probeIP), &cubebox.ListCubeSandboxRequest{
		Id: &sandboxID,
	})
	if err != nil || listRsp == nil {
		log.G(ctx).Warnf("pause: probe sandbox %s for stale binding: %v", sandboxID, err)
		return
	}
	running := false
	for _, item := range listRsp.GetItems() {
		if item.GetId() != sandboxID {
			continue
		}
		for _, c := range item.GetContainers() {
			if c.GetId() == sandboxID && c.GetState() == cubebox.ContainerState_CONTAINER_RUNNING {
				running = true
				break
			}
		}
	}
	if !running {
		return
	}
	log.G(ctx).Warnf("pause: clearing stale pause binding sandbox=%s snap=%s (sandbox is RUNNING)",
		sandboxID, rec.SnapshotID)
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

	instanceType := strings.TrimSpace(req.InstanceType)
	if instanceType == "" {
		instanceType = rec.InstanceType
	}
	if instanceType == "" {
		instanceType = cubebox.InstanceType_cubebox.String()
	}

	createReq := loadResumeSandboxSpec(ctx, req.SandboxID)
	placement, err := resumePlacement(ctx, rec, instanceType, createReq)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_SelectNodesFailed)
		rsp.Ret.RetMsg = err.Error()
		return rsp
	}
	targetIP := strings.TrimSpace(hostIP)
	if rec.NodeIP != "" {
		targetIP = rec.NodeIP
	}
	if placement != nil {
		if ip := strings.TrimSpace(placement.NodeIP); ip != "" {
			targetIP = ip
		} else if id := strings.TrimSpace(placement.NodeID); id != "" {
			if n, ok := localcache.GetNode(id); ok && n != nil && n.HostIP() != "" {
				targetIP = n.HostIP()
			}
		}
	}
	if targetIP == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_SelectNodesFailed)
		rsp.Ret.RetMsg = "resume placement returned no host IP"
		return rsp
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
	if b := strings.TrimSpace(rec.Backend); b != "" {
		cubeletReq.Backend = b
		cubeletReq.Annotations[constants.CubeAnnotationStorageBackend] = b
	}
	if constants.IsS3Backend(rec.Backend) {
		if raw := strings.TrimSpace(rec.ExportUUIDs); raw != "" {
			cubeletReq.Annotations[constants.CubeAnnotationSnapshotRemoteUUIDs] = raw
		}
	}
	if placement != nil && placement.CrossNode {
		// Only Master knows the target holds no replica. Saying so lets
		// Cubelet import the package up front instead of inferring it from a
		// catalog miss, and lets it reject a restore it cannot serve.
		cubeletReq.Annotations[constants.CubeAnnotationSnapshotCrossNode] = "true"
	}
	fillResumeRecreateFields(ctx, req.SandboxID, cubeletReq, createReq)

	calleeEndpoint := cubelet.GetCubeletAddr(targetIP)
	if placement != nil && placement.CrossNode {
		log.G(ctx).Infof("resume-from-pause: cross-node sandbox=%s snap=%s origin=%s target=%s",
			req.SandboxID, snapID, rec.NodeIP, targetIP)
	} else {
		log.G(ctx).Infof("resume-from-pause: sandbox=%s snap=%s host=%s", req.SandboxID, snapID, targetIP)
	}
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
	// A purge that fails leaves the sandbox unreachable for good, so it has
	// to reach the caller — but only after the bookkeeping below, which
	// describes a sandbox that is by now running whatever the proxy thinks.
	purgeErr := cubeproxy.InvalidateBackendCache(ctx, req.SandboxID, targetIP)

	// After Create the sandbox runs on private disks. Drop the pause package
	// on the node that still holds it: origin when that IP is known and not
	// the target (also the PAUSED tombstone), otherwise this node. Do not
	// key this off CrossNode — a cross-node placement with a blank origin
	// would otherwise skip both cleanup paths and leak the package.
	origin := strings.TrimSpace(rec.NodeIP)
	if origin != "" && origin != targetIP {
		if err := pausesnap.DropOriginTombstone(ctx, req.RequestID, req.SandboxID, origin, rec.SnapshotID, rec.Backend); err != nil {
			log.G(ctx).Errorf("resume: sandbox %s runs on %s but origin %s still has pause leftovers: %v",
				req.SandboxID, targetIP, origin, err)
		}
	} else if err := pausesnap.CleanupPauseSnapshot(ctx, req.RequestID, targetIP, rec.SnapshotID, rec.Backend); err != nil {
		log.G(ctx).Errorf("resume: sandbox %s is running but pause package %s on %s was not deleted: %v",
			req.SandboxID, rec.SnapshotID, targetIP, err)
	}

	// Binding must go so the next Pause can allocate a new id.
	if err := pausesnap.Delete(ctx, snapID); err != nil {
		log.G(ctx).Warnf("resume: delete pause snap meta %s: %v", snapID, err)
	}
	runAfterUpdateSandboxSuccessHook(ctx, req.SandboxID, req.InstanceType, "resume", req.RequestID)

	if purgeErr != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = fmt.Sprintf(
			"sandbox %s resumed on %s but CubeProxy kept its pre-pause route and will not reach it: %v",
			req.SandboxID, targetIP, purgeErr)
		log.G(ctx).Errorf("resume: %s", rsp.Ret.RetMsg)
	}
	return rsp
}

// loadResumeSandboxSpec is the single sandboxspec.Get on the Resume path.
// Placement (host-mount pin) and thin-Create network fallback share the result.
func loadResumeSandboxSpec(ctx context.Context, sandboxID string) *types.CreateCubeSandboxReq {
	createReq, err := sandboxspec.Get(ctx, sandboxID)
	if err != nil {
		if !errors.Is(err, sandboxspec.ErrSandboxSpecNotFound) && !errors.Is(err, sandboxspec.ErrSandboxSpecStoreNotReady) {
			log.G(ctx).Warnf("resume: load sandboxspec for %s: %v", sandboxID, err)
		}
		return nil
	}
	return createReq
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

// resumePlacement returns where Resume should Create. Same-node cases
// (XFS, S3 not remote-ready, host-mount) use the pause row's node and
// never consult the in-memory heartbeat cache. Only S3 remote=ready may
// leave the origin via restoreplace.Decide.
func resumePlacement(ctx context.Context, rec *pausesnap.Record, instanceType string, createReq *types.CreateCubeSandboxReq) (*restoreplace.Placement, error) {
	in := pauseResumePlacementInput(rec, instanceType, createReq)
	if in.PinToOrigin || !restoreplace.CanCrossNode(in.Backend, in.RemoteStatus) {
		return homeResumePlacement(rec), nil
	}
	return decidePauseResumePlacementFn(ctx, in)
}

func homeResumePlacement(rec *pausesnap.Record) *restoreplace.Placement {
	if rec == nil {
		return &restoreplace.Placement{}
	}
	return &restoreplace.Placement{
		NodeID:    strings.TrimSpace(rec.NodeID),
		NodeIP:    strings.TrimSpace(rec.NodeIP),
		CrossNode: false,
	}
}

func pauseResumePlacementInput(rec *pausesnap.Record, instanceType string, createReq *types.CreateCubeSandboxReq) restoreplace.Input {
	in := restoreplace.Input{
		InstanceType: instanceType,
		PinToOrigin:  createRequestHasHostMount(createReq),
	}
	if rec != nil {
		in.SnapshotID = rec.SnapshotID
		in.Backend = rec.Backend
		in.RemoteStatus = rec.RemoteStatus
		in.OriginNodeID = rec.NodeID
		in.OriginNodeIP = rec.NodeIP
		in.OriginHostFactsJSON = rec.OriginHostFactsJSON
	}
	if createReq != nil && config.GetConfig() != nil {
		if reqRes, rerr := checkAndGetReqResource(createReq); rerr == nil {
			in.ReqRes = reqRes
		}
	}
	return in
}

const extInfoRemoteUUIDs = "remote_uuids"

func remoteUUIDsFromExtInfo(extInfo map[string][]byte) string {
	if len(extInfo) == 0 {
		return ""
	}
	return strings.TrimSpace(string(extInfo[extInfoRemoteUUIDs]))
}
