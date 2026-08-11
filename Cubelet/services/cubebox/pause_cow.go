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
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
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
	sourceRootfs, err := storage.GetSandboxRootfs(ctx, req.SandboxID, rootVolumeName)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = fmt.Sprintf("failed to resolve sandbox rootfs: %v", err)
		return rsp, nil
	}
	rootfsObject, err := storage.CommitRootfs(ctx, sourceRootfs, snapID)
	if err != nil {
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("pause rootfs already exists: %v", err)
			return rsp, nil
		}
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to create pause rootfs snapshot: %v", err)
		return rsp, nil
	}
	memoryObject, err := storage.CreateMemoryVolume(ctx, snapID, memorySizeBytes)
	if err != nil {
		_ = storage.DeleteObject(ctx, rootfsObject.Name, rootfsObject.Kind)
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("pause memory object already exists: %v", err)
			return rsp, nil
		}
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to create pause memory volume: %v", err)
		return rsp, nil
	}
	if err := validateSnapshotMemoryObject(memoryObject, memorySizeBytes); err != nil {
		cleanupCowSnapshotObjects(ctx, stepLog, memoryObject, rootfsObject)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}

	cleanupArtifacts := func() {
		cleanupCowSnapshotObjects(ctx, stepLog, memoryObject, rootfsObject)
		_ = os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
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

	task, err := sb.FirstContainer().Container.Task(ctx, nil)
	if err != nil {
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskPauseFailed
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}

	memURL := snapshotMemoryVolURL(memoryObject.DevPath)
	pauseCfg := pauseSnapshotConfig{
		DestinationURL: tmpSnapshotPath,
		MemoryVolURL:   &memURL,
	}
	cfgJSON, err := json.Marshal(pauseCfg)
	if err != nil {
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("marshal pause snapshot config: %v", err)
		return rsp, nil
	}

	pauseCtx, pauseCancel := context.WithTimeout(ctx, taskPauseTimeout)
	defer pauseCancel()
	stepLog.Infof("PauseToSnapshot destination=%s memory_vol=%s snapID=%s", tmpSnapshotPath, memURL, snapID)
	if err := task.Update(pauseCtx, containerd.WithAnnotations(map[string]string{
		shimUpdateActionAnnotation:        shimUpdatePauseToSnapshotAction,
		shimUpdatePauseSnapshotAnnotation: string(cfgJSON),
	})); err != nil {
		if !pauseShimLikelyExited(err) {
			cleanupArtifacts()
			reconcileStatusAfterPauseError(ctx, sb, task, err)
			rsp.Ret.RetCode = errorcode.ErrorCode_TaskPauseFailed
			rsp.Ret.RetMsg = err.Error()
			return rsp, nil
		}
		stepLog.Warnf("PauseToSnapshot RPC error after likely shim exit (treating as success): %v", err)
	}

	if err := writePauseSandboxSpec(tmpSnapshotPath, pauseSpec); err != nil {
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to write pause sandbox_spec: %v", err)
		return rsp, nil
	}

	if err := writeMemoryDevFile(tmpSnapshotPath, memoryObject.DevPath); err != nil {
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to write memory.dev: %v", err)
		return rsp, nil
	}
	if err := deactivateCowSnapshotObjects(ctx, stepLog, memoryObject, rootfsObject); err != nil {
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to deactivate pause snapshot objects: %v", err)
		return rsp, nil
	}
	_ = os.RemoveAll(snapshotPath) // NOCC:Path Traversal()
	if err := os.Rename(tmpSnapshotPath, snapshotPath); err != nil {
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to move pause snapshot: %v", err)
		return rsp, nil
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
		stepLog.Warnf("failed to persist pause snapshot catalog for %s: %v", snapID, err)
	}

	// Mark paused so Destroy skips auto-resume, then wipe the live sandbox.
	// Pause catalog CoW objects (tpl-<snapID>-*) survive.
	for _, c := range sb.AllContainers() {
		if c.Status != nil {
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausedAt = time.Now().UnixNano()
				status.PausingAt = 0
				return status, nil
			})
		}
	}
	s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID)

	// Live sandbox + volume Detach are owned by Master Destroy
	// (cube.pause.skip_auto_resume) so Master can Apply volume refcount
	// ExtInfo. Pause catalog stays for Resume.
	stepLog.Infof("PauseToSnapshot completed: snapID=%s path=%s; awaiting Master Destroy for volume detach", snapID, snapshotPath)
	return rsp, nil
}

func pauseShimLikelyExited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "ttrpc: closed") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such process") ||
		strings.Contains(msg, "not found")
}

// pauseSnapshotIDForGC returns a leftover pause-snapshot id if Resume-time
// CleanupTemplate failed and the catalog is still present (reconcile helper).
func pauseSnapshotIDForGC(sb *cubeboxstore.CubeBox) string {
	if sb == nil {
		return ""
	}
	candidates := []string{
		strings.TrimSpace(sb.Labels[constants.MasterAnnotationPauseSnapshotID]),
		strings.TrimSpace(sb.Annotations[constants.MasterAnnotationPauseSnapshotID]),
		strings.TrimSpace(sb.Labels[constants.MasterAnnotationRuntimeSnapshotID]),
		strings.TrimSpace(sb.Annotations[constants.MasterAnnotationRuntimeSnapshotID]),
	}
	for _, id := range candidates {
		if id == "" {
			continue
		}
		entry, err := storage.GetLocalSnapshot(context.Background(), id)
		if err != nil || entry == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(entry.Kind), storage.CatalogKindPauseSnapshot) {
			return id
		}
	}
	return ""
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
