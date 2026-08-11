// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	volrefcount "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
)

// pauseSandbox:
//  1. allocate snap-* and record CREATING binding
//  2. Cubelet PauseToSnapshot (pack sandbox_spec, shim exit; CubeBox still there)
//  3. Destroy with skip-auto-resume → Detach volumes / wipe live runtime
//     (proxy kept; PAUSED CubeBox tombstone kept for Cubelet List;
//     volume refcount same as normal Destroy via ExtInfo)
//  4. mark pause binding READY
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

	snapID, err := pausesnap.Begin(ctx, req.SandboxID, nodeID, hostIP, req.InstanceType)
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
	log.G(ctx).Infof("pause: sandbox=%s snap=%s host=%s", req.SandboxID, snapID, hostIP)
	cubeRsp, err := cubelet.Update(ctx, calleeEndpoint, cubeletReq)
	if err != nil || cubeRsp == nil || cubeRsp.GetRet() == nil {
		pausesnap.Abort(ctx, req.SandboxID, snapID)
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		if err != nil {
			rsp.Ret.RetMsg = err.Error()
		} else {
			rsp.Ret.RetMsg = "cubelet pause response is nil"
		}
		return rsp
	}
	rsp.Ret.RetCode = int(cubeRsp.GetRet().GetRetCode())
	rsp.Ret.RetMsg = cubeRsp.GetRet().GetRetMsg()
	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		pausesnap.Abort(ctx, req.SandboxID, snapID)
		return rsp
	}

	// Detach volumes + wipe live CubeBox. Do not go through DestroySandbox —
	// that path clears sandbox proxy, which Pause must keep for Resume/Info.
	releasedVols, err := destroyLiveSandboxAfterPause(ctx, req, hostIP)
	if err != nil {
		pausesnap.Abort(ctx, req.SandboxID, snapID)
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = fmt.Sprintf("pause snapshot ok but volume detach/cleanup failed: %v", err)
		return rsp
	}

	if err := pausesnap.Complete(ctx, req.SandboxID, snapID, nodeID, hostIP, req.InstanceType, releasedVols); err != nil {
		log.G(ctx).Errorf("pause complete meta failed sandbox=%s snap=%s: %v", req.SandboxID, snapID, err)
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = fmt.Sprintf("pause ok on cubelet but master meta failed: %v", err)
		return rsp
	}
	runAfterUpdateSandboxSuccessHook(ctx, req.SandboxID, req.InstanceType, "pause", req.RequestID)
	return rsp
}

// destroyLiveSandboxAfterPause Detaches volumes and removes the live sandbox
// after PauseToSnapshot. Keeps Redis proxy / cache so the sandbox stays addressable.
// Returns plugin volume IDs released on this node (1→0) for Resume validation.
func destroyLiveSandboxAfterPause(ctx context.Context, req *types.UpdateRequest, hostIP string) ([]string, error) {
	destroyRsp, err := cubelet.Destroy(ctx, cubelet.GetCubeletAddr(hostIP), &cubebox.DestroyCubeSandboxRequest{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Annotations: map[string]string{
			constants.CubeAnnotationPauseSkipAutoResume: "true",
			constants.CubeAnnotationsInsType:            req.InstanceType,
		},
	})
	if err != nil {
		return nil, err
	}
	if destroyRsp == nil || destroyRsp.GetRet() == nil {
		return nil, errors.New("nil destroy response after pause")
	}
	code := destroyRsp.GetRet().GetRetCode()
	if code != cubeleterrorcode.ErrorCode_Success && code != cubeleterrorcode.ErrorCode_OK {
		return nil, fmt.Errorf("destroy ret=%v msg=%s", code, destroyRsp.GetRet().GetRetMsg())
	}
	released := volrefcount.ReleasedVolumeIDs(destroyRsp.GetExtInfo())
	// Same as normal Destroy: apply node 1→0 (and only those) to Master.
	volrefcount.ApplyFromExtInfo(ctx, destroyRsp.GetExtInfo())
	return released, nil
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

	// Thin Create: identity + snapshot binding only. Full recreate payload
	// (containers, volumes, plugin-volume-sources) is in sandbox_spec.json.
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

	// G5: running sandbox is an independent copy — delete pause snap local + meta.
	cleanupPauseSnapshotLocal(ctx, req.RequestID, targetIP, snapID)
	if err := pausesnap.Delete(ctx, snapID); err != nil {
		log.G(ctx).Warnf("resume: delete pause snap meta %s: %v", snapID, err)
	}
	runAfterUpdateSandboxSuccessHook(ctx, req.SandboxID, req.InstanceType, "resume", req.RequestID)
	return rsp
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
