// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type pauseSnapshotConfig struct {
	DestinationURL string  `json:"destination_url"`
	MemoryVolURL   *string `json:"memory_vol_url,omitempty"`
}

// resolvePauseSnapshotID requires Master-allocated snap-* id (same format as
// normal Commit snapshots). Distinction is catalog Kind + Master DB type.
func resolvePauseSnapshotID(req *cubebox.UpdateCubeSandboxRequest) (string, error) {
	if req == nil {
		return "", errors.New("nil pause request")
	}
	ann := req.GetAnnotations()
	snapID := strings.TrimSpace(ann[constants.MasterAnnotationPauseSnapshotID])
	if snapID == "" {
		snapID = strings.TrimSpace(ann[constants.MasterAnnotationRuntimeSnapshotID])
	}
	if snapID == "" {
		return "", errors.New("pause requires Master-allocated snapshot id (cube.master.pause.snapshot.id)")
	}
	if err := pathutil.ValidateSafeID(snapID); err != nil {
		return "", fmt.Errorf("invalid pause snapshot id: %w", err)
	}
	if !strings.HasPrefix(snapID, "snap-") {
		return "", fmt.Errorf("pause snapshot id %q must use snap- prefix", snapID)
	}
	return snapID, nil
}

// stampPauseSnapshotID records the in-progress pause snap on the CubeBox so
// Destroy of an unknown / failed-pause sandbox can GC half-finished CoW
// objects even when catalog.json was never written.
func stampPauseSnapshotID(sb *cubeboxstore.CubeBox, snapID string) {
	snapID = strings.TrimSpace(snapID)
	if sb == nil || snapID == "" {
		return
	}
	sb.AddLabels(map[string]string{constants.MasterAnnotationPauseSnapshotID: snapID})
	sb.AddAnnotations(map[string]string{constants.MasterAnnotationPauseSnapshotID: snapID})
}

// updateWithPauseCow pauses a running sandbox into a CubeCow-backed catalog
// snapshot (same layout as CommitSandbox), packs sandbox_spec.json beside the
// snapshot files, asks the shim to exit, then fully destroys the live sandbox.
// Master only keeps sandboxID↔snapshotID; recreate meta travels with the snap.
func (s *service) updateWithPauseCow(
	ctx context.Context,
	req *cubebox.UpdateCubeSandboxRequest,
	sb *cubeboxstore.CubeBox,
) (*cubebox.UpdateCubeSandboxResponse, error) {
	rsp := &cubebox.UpdateCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"step":      "pauseCow",
		"sandboxID": req.SandboxID,
	})

	snapID, err := resolvePauseSnapshotID(req)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	stampPauseSnapshotID(sb, snapID)
	_ = s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID)

	// Pause allows host-mount / host_dir (same hostdir is re-bound on Resume).
	// User snapshot CommitSandbox still rejects those via validateCommitSandboxTarget.
	rootVolumeName, err := validatePauseSandboxTarget(sb)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}

	spec, err := s.getCubeboxSnapshotSpec(ctx, req.SandboxID)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to get cubebox spec: %v", err)
		return rsp, nil
	}
	var resourceSpec ResourceSpec
	if err := json.Unmarshal(spec.Resource, &resourceSpec); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to parse resource spec: %v", err)
		return rsp, nil
	}
	if resourceSpec.CPU <= 0 || resourceSpec.Memory <= 0 {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid resource spec: cpu=%d, memory=%d", resourceSpec.CPU, resourceSpec.Memory)
		return rsp, nil
	}

	specDir := fmt.Sprintf("%dC%dM", resourceSpec.CPU, resourceSpec.Memory)
	// Same layout as CommitSandbox: .../cubebox/<snapID>/<specDir>/
	snapshotPath := filepath.Join(DefaultSnapshotDir, "cubebox", snapID, specDir)
	if _, err := pathutil.ValidatePathUnderBase(DefaultSnapshotDir, snapshotPath); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid pause snapshot path: %v", err)
		return rsp, nil
	}
	tmpSnapshotPath := snapshotPath + ".tmp"
	if _, err := pathutil.ValidatePathUnderBase(DefaultSnapshotDir, tmpSnapshotPath); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid pause tmp snapshot path: %v", err)
		return rsp, nil
	}

	memorySizeBytes := snapshotMemorySizeBytes(resourceSpec.Memory)
	// Resolve live rootfs now; CommitRootfs runs *after* PauseToSnapshot so the
	// disk snapshot matches the frozen memory (guest cannot write after pause).
	sourceRootfs, err := storage.GetSandboxRootfs(ctx, req.SandboxID, rootVolumeName)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = fmt.Sprintf("failed to resolve sandbox rootfs: %v", err)
		return rsp, nil
	}
	memoryObject, err := storage.CreateMemoryVolume(ctx, snapID, memorySizeBytes)
	if err != nil {
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("pause memory object already exists: %v", err)
			return rsp, nil
		}
		// cubecow may have left a partial tpl-<snapID>-memory after ENOSPC.
		s.bestEffortCleanupPauseSnapshot(ctx, req.RequestID, snapID)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to create pause memory volume: %v", err)
		return rsp, nil
	}
	if err := validateSnapshotMemoryObject(memoryObject, memorySizeBytes); err != nil {
		cleanupCowSnapshotObjects(ctx, stepLog, memoryObject, nil)
		s.bestEffortCleanupPauseSnapshot(ctx, req.RequestID, snapID)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}

	var rootfsObject *storage.CowSnapshotObject
	cleanupArtifacts := func() {
		cleanupCowSnapshotObjects(ctx, stepLog, memoryObject, rootfsObject)
		_ = os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		s.bestEffortCleanupPauseSnapshot(ctx, req.RequestID, snapID)
	}

	_ = os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
	if err := os.MkdirAll(tmpSnapshotPath, 0o755); err != nil {
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to create pause snapshot dir: %v", err)
		return rsp, nil
	}

	// Capture recreate payload before PauseToSnapshot. Shim recreate_dir wipes
	// the destination, so sandbox_spec.json is written after the shim returns.
	pauseSpec := buildPauseSandboxSpec(sb, req.RequestID)

	ns := sb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)
	ctx = constants.WithPreStopType(ctx, constants.PreStopTypePause)
	ctx = addPauseResumeMetaData(ctx, req)

	// Detach from the Master RPC deadline: if the client times out, Cubelet must
	// still finish PauseToSnapshot + keep_tombstone Destroy. Budget covers both
	// under Master pauseCubeletRPCTimeout (120s); Destroy itself is still capped
	// by destroy_dead_line (60s).
	const pauseCowWorkTimeout = 120 * time.Second
	workCtx, workCancel := context.WithTimeout(context.WithoutCancel(ctx), pauseCowWorkTimeout)
	defer workCancel()

	failPause := func(code errorcode.ErrorCode, msg string) (*cubebox.UpdateCubeSandboxResponse, error) {
		cleanupArtifacts()
		markLocalPauseFailed(sb, errors.New(msg))
		_ = s.cubeboxMgr.cubeboxManger.SyncByID(workCtx, sb.ID)
		rsp.Ret.RetCode = code
		rsp.Ret.RetMsg = msg
		return rsp, nil
	}

	for _, c := range sb.AllContainers() {
		if c.Status != nil {
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausingAt = time.Now().UnixNano()
				// Entering pause lifecycle: never advertise EXITED from a prior
				// FinishedAt while PausingAt is set (State prefers PausingAt).
				status.FinishedAt = 0
				status.Unknown = false
				status.Reason = ""
				status.Message = ""
				return status, nil
			})
		}
	}
	for _, c := range sb.All() {
		doPreStop(workCtx, c)
	}
	doPreStop(workCtx, sb.FirstContainer())

	task, err := sb.FirstContainer().Container.Task(workCtx, nil)
	if err != nil {
		return failPause(errorcode.ErrorCode_TaskPauseFailed, err.Error())
	}

	memURL := snapshotMemoryVolURL(memoryObject.DevPath)
	pauseCfg := pauseSnapshotConfig{
		DestinationURL: tmpSnapshotPath,
		MemoryVolURL:   &memURL,
	}
	cfgJSON, err := json.Marshal(pauseCfg)
	if err != nil {
		return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("marshal pause snapshot config: %v", err))
	}

	stepLog.Infof("PauseToSnapshot destination=%s memory_vol=%s snapID=%s", tmpSnapshotPath, memURL, snapID)
	// Shim returns Update OK and stays alive (Paused). Any error is Pause
	// failure — do not treat ttrpc closed as success (shim no longer self-exits
	// on PauseToSnapshot). Cubelet reaps the shim via keep_tombstone Delete below.
	if err := task.Update(workCtx, containerd.WithAnnotations(map[string]string{
		shimUpdateActionAnnotation:        shimUpdatePauseToSnapshotAction,
		shimUpdatePauseSnapshotAnnotation: string(cfgJSON),
	})); err != nil {
		cleanupArtifacts()
		markLocalPauseFailed(sb, err)
		_ = s.cubeboxMgr.cubeboxManger.SyncByID(workCtx, sb.ID)
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskPauseFailed
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}

	// Disk after memory freeze: live rootfs volume is still present until
	// keep_tombstone Destroy. If this fails the MicroVM is already gone —
	// Pause fails and the sandbox is not Resume-able (delete only).
	rootfsObject, err = storage.CommitRootfs(workCtx, sourceRootfs, snapID)
	if err != nil {
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			return failPause(errorcode.ErrorCode_PreConditionFailed,
				fmt.Sprintf("pause rootfs already exists: %v", err))
		}
		return failPause(errorcode.ErrorCode_Unknown,
			fmt.Sprintf("failed to create pause rootfs snapshot: %v", err))
	}

	if err := writePauseSandboxSpec(tmpSnapshotPath, pauseSpec); err != nil {
		return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("failed to write pause sandbox_spec: %v", err))
	}

	if err := writeMemoryDevFile(tmpSnapshotPath, memoryObject.DevPath); err != nil {
		return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("failed to write memory.dev: %v", err))
	}
	if err := deactivateCowSnapshotObjects(workCtx, stepLog, memoryObject, rootfsObject); err != nil {
		return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("failed to deactivate pause snapshot objects: %v", err))
	}
	_ = os.RemoveAll(snapshotPath) // NOCC:Path Traversal()
	if err := os.Rename(tmpSnapshotPath, snapshotPath); err != nil {
		return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("failed to move pause snapshot: %v", err))
	}

	if err := storage.WriteSnapshotCatalog(&storage.SnapshotCatalogEntry{
		SnapshotID:      snapID,
		InstanceType:    "cubebox",
		SpecDir:         specDir,
		SnapshotPath:    snapshotPath,
		MetaDir:         snapshotPath,
		RootfsVol:       rootfsObject.Name,
		RootfsKind:      rootfsObject.Kind,
		MemoryVol:       memoryObject.Name,
		MemoryKind:      memoryObject.Kind,
		RootfsSizeBytes: rootfsObject.SizeBytes,
		Kind:            storage.CatalogKindPauseSnapshot,
	}); err != nil {
		// catalog.json is required for Resume / List / cross-node; do not mark
		// PAUSED without it (sandbox_spec / memory.dev already fail hard above).
		_ = os.RemoveAll(snapshotPath) // NOCC:Path Traversal()
		return failPause(errorcode.ErrorCode_Unknown,
			fmt.Sprintf("failed to persist pause snapshot catalog for %s: %v", snapID, err))
	}

	// Mark PAUSED before keep_tombstone Destroy so the destroy path takes the
	// IsPaused branch (task Delete reaps the still-living shim). Pause catalog
	// CoW objects (tpl-<snapID>-*) survive for Resume.
	for _, c := range sb.AllContainers() {
		if c.Status != nil {
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausedAt = time.Now().UnixNano()
				status.PausingAt = 0
				status.FinishedAt = 0
				status.Unknown = false
				return status, nil
			})
		}
	}
	_ = s.cubeboxMgr.cubeboxManger.SyncByID(workCtx, sb.ID)

	stepLog.Infof("PauseToSnapshot completed: snapID=%s path=%s; running in-process keep_tombstone Destroy", snapID, snapshotPath)
	extInfo, err := s.destroyLiveAfterPause(workCtx, req, sb)
	// Always attach whatever volume ref events were observed — Detach may have
	// succeeded (node 1→0) even when later cleanup fails. Master still needs
	// to apply those deltas on an explicit Pause failure.
	rsp.ExtInfo = extInfo
	if err != nil {
		// Snapshot is on disk; do not wipe it. Master records FAILED (no Resume).
		markLocalPauseFailed(sb, err)
		_ = s.cubeboxMgr.cubeboxManger.SyncByID(workCtx, sb.ID)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	return rsp, nil
}

// destroyLiveAfterPause runs engine.Destroy(keep_tombstone) in-process while
// Update already holds sandboxLifecycleLocks (do not call service.Destroy).
// The paused Destroy path uses localTask.Delete ("binary delete") to reap the
// shim that PauseToSnapshot left alive after Update OK.
func (s *service) destroyLiveAfterPause(
	ctx context.Context,
	req *cubebox.UpdateCubeSandboxRequest,
	sb *cubeboxstore.CubeBox,
) (map[string][]byte, error) {
	if req == nil || sb == nil {
		return nil, errors.New("nil pause destroy request")
	}
	if sb.UserMarkDeletedTime == nil {
		now := time.Now()
		sb.UserMarkDeletedTime = &now
		sb.DeleteRequestID = req.RequestID
		_ = s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID)
	}

	destroyReq := &cubebox.DestroyCubeSandboxRequest{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Annotations: map[string]string{
			constants.AnnotationPauseKeepTombstone: "true",
		},
	}
	if ann := req.GetAnnotations(); ann != nil {
		if v := strings.TrimSpace(ann[constants.MasterAnnotationInstanceType]); v != "" {
			destroyReq.Annotations[constants.MasterAnnotationInstanceType] = v
		}
	}
	destroyInfo := &workflow.DestroyContext{
		DestroyInfo: destroyReq,
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: req.SandboxID,
		},
	}
	ctx = context.WithValue(ctx, workflow.KDestroyContext, destroyInfo)

	// Standalone Destroy RPC still uses destroy_dead_line (60s). Pause already
	// holds a longer WithoutCancel workCtx (snap + cleanup); do not re-cap here
	// so concurrent keep_tombstone cleanup can finish under the Pause budget.
	ferr, _ := ret.FromError(s.engine.Destroy(ctx, destroyInfo))
	ext := map[string][]byte{}
	if data := marshalVolumeRefEvents(destroyInfo.VolumeRefEvents); data != nil {
		ext[constants.CubeExtVolumeRefEvents] = data
	}
	if !ret.IsSuccessCode(ferr.Code()) {
		// Return partial ext_info so Master can still decrement volume refs for
		// Detaches that already completed before the destroy error.
		return ext, fmt.Errorf("pause keep_tombstone destroy ret=%v msg=%s", ferr.Code(), ferr.Message())
	}

	if sb2, err := s.cubeboxMgr.cubeboxManger.Get(ctx, req.SandboxID); err == nil && sb2 != nil {
		sb2.Lock()
		sb2.UserMarkDeletedTime = nil
		sb2.DeleteRequestID = ""
		sb2.Unlock()
		_ = s.cubeboxMgr.cubeboxManger.SyncByID(ctx, req.SandboxID)
	}
	return ext, nil
}

// markLocalPauseFailed records a terminal Pause failure without EXITED.
// Master keeps the sandbox proxy + FAILED pausesnap for user visibility.
func markLocalPauseFailed(sb *cubeboxstore.CubeBox, cause error) {
	if sb == nil {
		return
	}
	msg := "pause failed"
	if cause != nil && strings.TrimSpace(cause.Error()) != "" {
		msg = cause.Error()
	}
	for _, c := range sb.AllContainers() {
		if c == nil || c.Status == nil {
			continue
		}
		_ = c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
			status.PausingAt = 0
			status.PausedAt = 0
			status.FinishedAt = 0
			status.Unknown = true
			status.Reason = "PauseFailed"
			status.Message = msg
			return status, nil
		})
	}
}

// pauseSnapshotIDForGC returns a leftover pause-snapshot id for Destroy-time
// GC. Catalog Kind=pause is authoritative. A catalog miss on the stamped
// pause.snapshot.id is still returned: half-finished Pause never wrote
// catalog.json but tpl-<snapID>-memory may still occupy disk.
func pauseSnapshotIDForGC(sb *cubeboxstore.CubeBox) string {
	if sb == nil {
		return ""
	}
	pauseIDs := []string{
		strings.TrimSpace(sb.Labels[constants.MasterAnnotationPauseSnapshotID]),
		strings.TrimSpace(sb.Annotations[constants.MasterAnnotationPauseSnapshotID]),
	}
	if id := firstPauseSnapshotID(pauseIDs, true); id != "" {
		return id
	}
	runtimeIDs := []string{
		strings.TrimSpace(sb.Labels[constants.MasterAnnotationRuntimeSnapshotID]),
		strings.TrimSpace(sb.Annotations[constants.MasterAnnotationRuntimeSnapshotID]),
	}
	return firstPauseSnapshotID(runtimeIDs, false)
}

func firstPauseSnapshotID(candidates []string, allowCatalogMiss bool) string {
	for _, id := range candidates {
		if id == "" {
			continue
		}
		entry, err := storage.GetLocalSnapshot(context.Background(), id)
		if err == nil && entry != nil {
			if strings.EqualFold(strings.TrimSpace(entry.Kind), storage.CatalogKindPauseSnapshot) {
				return id
			}
			continue
		}
		if allowCatalogMiss && strings.HasPrefix(id, "snap-") {
			return id
		}
	}
	return ""
}

// pauseSnapIDToGCOnDestroy is the Destroy-time pause-snap GC decision.
// keep_tombstone / delete_tombstone still own the snap (Master CleanupTemplate).
// PAUSED without those flags is not expected on the user Destroy path; skip GC
// so we cannot drop a live pause snap if someone cubecli-destroys a tombstone.
// UNKNOWN / FAILED / RUNNING / PAUSING may hold half-finished or leftover snaps.
func pauseSnapIDToGCOnDestroy(req *cubebox.DestroyCubeSandboxRequest, sb *cubeboxstore.CubeBox) string {
	if sb == nil || isPauseKeepTombstone(req) || isPauseDeleteTombstone(req) {
		return ""
	}
	if st := sb.GetStatus(); st != nil && st.Get().State() == cubebox.ContainerState_CONTAINER_PAUSED {
		return ""
	}
	return pauseSnapshotIDForGC(sb)
}

// bestEffortCleanupPauseSnapshot removes a leftover pause catalog (primary GC
// is Master CleanupTemplate right after Resume; this is a destroy-time fallback).
func (s *service) bestEffortCleanupPauseSnapshot(ctx context.Context, requestID, snapID string) {
	snapID = strings.TrimSpace(snapID)
	if snapID == "" {
		return
	}
	cleanupRsp, err := s.CleanupTemplate(ctx, &cubebox.CleanupTemplateRequest{
		RequestID:  requestID,
		TemplateID: snapID,
	})
	if err != nil {
		log.G(ctx).Warnf("pause-snap GC after destroy failed snap=%s: %v", snapID, err)
		return
	}
	if cleanupRsp != nil && cleanupRsp.GetRet() != nil &&
		cleanupRsp.GetRet().GetRetCode() != errorcode.ErrorCode_Success {
		log.G(ctx).Warnf("pause-snap GC after destroy snap=%s ret=%v msg=%s",
			snapID, cleanupRsp.GetRet().GetRetCode(), cleanupRsp.GetRet().GetRetMsg())
	}
}
