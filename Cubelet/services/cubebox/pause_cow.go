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
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type pauseSnapshotConfig struct {
	DestinationURL string  `json:"destination_url"`
	MemoryVolURL   *string `json:"memory_vol_url,omitempty"`
	// SnapshotType is the same wire value CommitSandbox passes to
	// cube-runtime (--snapshot-type): soft-dirty, incremental, or full.
	SnapshotType string `json:"snapshot_type,omitempty"`
}

func newPauseSnapshotConfig(dest, memURL, snapshotType string) pauseSnapshotConfig {
	cfg := pauseSnapshotConfig{
		DestinationURL: dest,
		SnapshotType:   normalizeSnapshotType(snapshotType),
	}
	if memURL != "" {
		cfg.MemoryVolURL = &memURL
	}
	return cfg
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

func stampedPauseSnapshotID(sb *cubeboxstore.CubeBox) string {
	if sb == nil {
		return ""
	}
	if id := strings.TrimSpace(sb.Labels[constants.MasterAnnotationPauseSnapshotID]); id != "" {
		return id
	}
	return strings.TrimSpace(sb.Annotations[constants.MasterAnnotationPauseSnapshotID])
}

func (s *service) listCubeboxes() []*cubeboxstore.CubeBox {
	if s == nil || s.cubeboxMgr == nil || s.cubeboxMgr.cubeboxManger == nil {
		return nil
	}
	return s.cubeboxMgr.cubeboxManger.List()
}

// keepLiveXFSPausePackage is true when Master's Resume-time CleanupTemplate
// must leave this pause catalog on disk. XFS Resume mmaps the package file;
// S3 already cloned onto sb-*-memory and must drop the package. PAUSED /
// EXITED / UNKNOWN do not hold a live mmap, so DelPaused and leftover GC
// still delete.
//
// Only Cubelet-stamped Labels count (not user Create annotations). Match
// the current pause id or the restore-base: Pause overwrites the pause id
// before the overlay finishes, so a delayed Cleanup of the previous package
// must still see the mmap / incremental source. cleanupTemplate also
// requires catalog Kind=pause_snapshot so a forged label cannot pin a
// template or customer snap.
func keepLiveXFSPausePackage(boxes []*cubeboxstore.CubeBox, snapID, backend string) bool {
	if storage.IsS3Backend(backend) {
		return false
	}
	snapID = strings.TrimSpace(snapID)
	if snapID == "" {
		return false
	}
	for _, sb := range boxes {
		if sandboxHoldsLiveXFSPausePackage(sb, snapID) {
			return true
		}
	}
	return false
}

func sandboxHoldsLiveXFSPausePackage(sb *cubeboxstore.CubeBox, snapID string) bool {
	if sb == nil || !sandboxLiveForXFSPauseKeep(sb) {
		return false
	}
	if cubeBoxLabel(sb, constants.MasterAnnotationPauseSnapshotID) == snapID {
		return true
	}
	return cubeBoxLabel(sb, constants.MasterAnnotationRuntimeRestoreSnapshotID) == snapID
}

func sandboxLiveForXFSPauseKeep(sb *cubeboxstore.CubeBox) bool {
	st := sb.GetStatus()
	if st == nil {
		return false
	}
	switch st.Get().State() {
	case cubebox.ContainerState_CONTAINER_CREATED,
		cubebox.ContainerState_CONTAINER_RUNNING,
		cubebox.ContainerState_CONTAINER_PAUSING:
		return true
	default:
		return false
	}
}

func cubeBoxLabel(sb *cubeboxstore.CubeBox, key string) string {
	if sb == nil || key == "" {
		return ""
	}
	sb.MetaLock.Lock()
	defer sb.MetaLock.Unlock()
	if sb.Labels == nil {
		return ""
	}
	return strings.TrimSpace(sb.Labels[key])
}

func isPauseSnapshotCatalogKind(kind string) bool {
	return strings.EqualFold(strings.TrimSpace(kind), storage.CatalogKindPauseSnapshot)
}

// replacedLivePauseSnapshotID is the pause snap Resume left as live. After a
// new Pause succeeds, Cubelet CleanupTemplate's it (keep_tombstone already
// tore down the running overlay). Empty on the first Pause from a template.
// XFS Resume keeps that package on disk so this Pause can incremental-
// overlay onto it; this GC is what finally removes it.
func replacedLivePauseSnapshotID(prev, newID string) string {
	prev = strings.TrimSpace(prev)
	newID = strings.TrimSpace(newID)
	if prev == "" || prev == newID {
		return ""
	}
	return prev
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
	backend, err := storageBackendFromAnnotations(req.GetAnnotations())
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	stepLog = stepLog.WithFields(CubeLog.Fields{"backend": backend})
	prevLiveSnap := stampedPauseSnapshotID(sb)
	stampLaunchMemoryAncestorOnce(sb, resolveLaunchAncestorSnapshotID(sb))
	stampPauseSnapshotID(sb, snapID)
	_ = s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID)

	// Pause allows host-mount / host_dir / plugin_volume (same host path is
	// re-bound on Resume). User snapshot CommitSandbox still rejects those
	// via validateCommitSandboxTarget.
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
	layout, err := prepareSnapshotWorkLayout(backend, storage.SnapshotKindPause, snapID, "", specDir)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid pause snapshot path: %v", err)
		return rsp, nil
	}
	snapshotPath := layout.Home
	tmpSnapshotPath := layout.TmpHome

	memorySizeBytes := snapshotMemorySizeBytes(resourceSpec.Memory)
	// Resolve live rootfs now; CommitRootfs runs *after* PauseToSnapshot so the
	// disk snapshot matches the frozen memory (guest cannot write after pause).
	sourceRootfs, err := storage.GetSandboxRootfsFor(ctx, backend, req.SandboxID, rootVolumeName)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = fmt.Sprintf("failed to resolve sandbox rootfs: %v", err)
		return rsp, nil
	}
	// Own volume if this sandbox has one; otherwise the start template/snapshot.
	memoryObject, snapshotType, err := preparePauseMemoryArtifact(ctx, stepLog, sb, snapID, memorySizeBytes, backend)
	if err != nil {
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("pause memory object already exists: %v", err)
			return rsp, nil
		}
		// cubecow may have left a partial tpl-<snapID>-memory after ENOSPC.
		s.bestEffortCleanupPauseSnapshot(ctx, req.RequestID, snapID, backend)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to prepare pause memory artifact: %v", err)
		return rsp, nil
	}
	if err := validateSnapshotMemoryObject(memoryObject, memorySizeBytes); err != nil {
		cleanupCowSnapshotObjectsOn(ctx, stepLog, backend, memoryObject, nil)
		s.bestEffortCleanupPauseSnapshot(ctx, req.RequestID, snapID, backend)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}

	var rootfsObject *storage.CowSnapshotObject
	cleanupArtifacts := func() {
		layout.releaseMetadata(ctx)
		cleanupCowSnapshotObjectsOn(ctx, stepLog, backend, memoryObject, rootfsObject)
		layout.discardTmpDir()
		if !layout.usesTmpRename() {
			_ = os.RemoveAll(layout.Home) // NOCC:Path Traversal()
		}
		s.bestEffortCleanupPauseSnapshot(ctx, req.RequestID, snapID, backend)
	}

	layout.resetTmpDir()
	if err := layout.prepareWork(ctx); err != nil {
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
	pauseCfg := newPauseSnapshotConfig(layout.MetaWork, memURL, snapshotType)
	cfgJSON, err := json.Marshal(pauseCfg)
	if err != nil {
		return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("marshal pause snapshot config: %v", err))
	}

	stepLog.Infof("PauseToSnapshot destination=%s memory_vol=%s snapID=%s snapshot_type=%s",
		layout.MetaWork, memURL, snapID, pauseCfg.SnapshotType)
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
	rootfsObject, err = storage.CommitRootfsFor(workCtx, backend, sourceRootfs, snapID)
	if err != nil {
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			return failPause(errorcode.ErrorCode_PreConditionFailed,
				fmt.Sprintf("pause rootfs already exists: %v", err))
		}
		return failPause(errorcode.ErrorCode_Unknown,
			fmt.Sprintf("failed to create pause rootfs snapshot: %v", err))
	}

	if err := writePauseSandboxSpec(layout.MetaWork, pauseSpec); err != nil {
		return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("failed to write pause sandbox_spec: %v", err))
	}
	// Do not write memory.dev — restore uses catalog vol name + ResolveDevPath.
	if err := deactivateCowSnapshotObjectsOn(workCtx, stepLog, backend, memoryObject, rootfsObject); err != nil {
		return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("failed to deactivate pause snapshot objects: %v", err))
	}
	if layout.usesTmpRename() {
		_ = os.RemoveAll(snapshotPath) // NOCC:Path Traversal()
		if err := os.Rename(tmpSnapshotPath, snapshotPath); err != nil {
			return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("failed to move pause snapshot: %v", err))
		}
	}
	if err := storage.EnsureShimSpecDirLink(layout.Home, specDir); err != nil {
		return failPause(errorcode.ErrorCode_Unknown, fmt.Sprintf("failed to expose shim spec dir: %v", err))
	}

	if err := storage.WriteSnapshotCatalogFor(backend, &storage.SnapshotCatalogEntry{
		SnapshotID:      snapID,
		InstanceType:    "cubebox",
		SpecDir:         specDir,
		SnapshotPath:    layout.Home,
		MetaDir:         layout.MetaDir,
		RootfsVol:       rootfsObject.Name,
		RootfsKind:      rootfsObject.Kind,
		MemoryVol:       memoryObject.Name,
		MemoryKind:      memoryObject.Kind,
		MetadataVol:     storage.S3MetadataCatalogVol(backend, snapID),
		MetadataKind:    storage.S3MetadataCatalogKind(backend),
		RootfsSizeBytes: rootfsObject.SizeBytes,
		Kind:            storage.CatalogKindPauseSnapshot,
		Backend:         backend,
	}); err != nil {
		// catalog.json is required for Resume / List / cross-node; do not mark
		// PAUSED without it (sandbox_spec already fails hard above).
		_ = storage.UnmountS3Metadata(layout.MetaDir)
		_ = os.RemoveAll(snapshotPath) // NOCC:Path Traversal()
		return failPause(errorcode.ErrorCode_Unknown,
			fmt.Sprintf("failed to persist pause snapshot catalog for %s: %v", snapID, err))
	}
	// S3: seal memory/metadata work volumes to RO snapshots before Upload.
	if err := storage.FinalizeS3PackageSnapshots(workCtx, backend, snapID); err != nil {
		_ = storage.UnmountS3Metadata(layout.MetaDir)
		_ = os.RemoveAll(snapshotPath) // NOCC:Path Traversal()
		return failPause(errorcode.ErrorCode_Unknown,
			fmt.Sprintf("failed to seal s3 pause package snapshots for %s: %v", snapID, err))
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

	remoteUUIDsJSON := uploadRemoteUUIDsIfS3(workCtx, backend, snapID)

	stepLog.Infof("PauseToSnapshot completed: snapID=%s path=%s; running in-process keep_tombstone Destroy", snapID, snapshotPath)
	extInfo, err := s.destroyLiveAfterPause(workCtx, req, sb)
	// Always attach whatever volume ref events were observed — Detach may have
	// succeeded (node 1→0) even when later cleanup fails. Master still needs
	// to apply those deltas on an explicit Pause failure.
	if remoteUUIDsJSON != "" {
		if extInfo == nil {
			extInfo = map[string][]byte{}
		}
		extInfo[storage.ExtInfoRemoteUUIDs] = []byte(remoteUUIDsJSON)
	}
	rsp.ExtInfo = extInfo
	rsp.RemoteUuids = remoteUUIDsJSON
	if err != nil {
		// Snapshot is on disk; do not wipe it. Master records FAILED (no Resume).
		markLocalPauseFailed(sb, err)
		_ = s.cubeboxMgr.cubeboxManger.SyncByID(workCtx, sb.ID)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	if prev := replacedLivePauseSnapshotID(prevLiveSnap, snapID); prev != "" {
		stepLog.Infof("pause replaced live snap %s with %s; CleanupTemplate previous", prev, snapID)
		s.bestEffortCleanupPauseSnapshot(workCtx, req.RequestID, prev, cleanupBackendForPauseSnap(backend, prev))
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
	backend := pauseCatalogBackend(sb)
	pauseIDs := []string{
		strings.TrimSpace(sb.Labels[constants.MasterAnnotationPauseSnapshotID]),
		strings.TrimSpace(sb.Annotations[constants.MasterAnnotationPauseSnapshotID]),
	}
	if id := firstPauseSnapshotID(pauseIDs, true, backend); id != "" {
		return id
	}
	runtimeIDs := []string{
		strings.TrimSpace(sb.Labels[constants.MasterAnnotationRuntimeSnapshotID]),
		strings.TrimSpace(sb.Annotations[constants.MasterAnnotationRuntimeSnapshotID]),
	}
	return firstPauseSnapshotID(runtimeIDs, false, backend)
}

func pauseCatalogBackend(sb *cubeboxstore.CubeBox) string {
	if sb == nil {
		return ""
	}
	if backend, err := storageBackendFromAnnotations(sb.Annotations); err == nil && strings.TrimSpace(sb.Annotations[constants.MasterAnnotationStorageBackend]) != "" {
		return backend
	}
	if backend, err := storageBackendFromAnnotations(sb.Labels); err == nil && strings.TrimSpace(sb.Labels[constants.MasterAnnotationStorageBackend]) != "" {
		return backend
	}
	return ""
}

func firstPauseSnapshotID(candidates []string, allowCatalogMiss bool, backend string) string {
	for _, id := range candidates {
		if id == "" {
			continue
		}
		if entry, _ := lookupPauseCatalog(id, backend); entry != nil {
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

func lookupPauseCatalog(id, preferred string) (*storage.SnapshotCatalogEntry, string) {
	seen := map[string]struct{}{}
	for _, backend := range []string{preferred, cow.BackendS3, cow.BackendXFS} {
		backend = strings.TrimSpace(backend)
		if backend == "" {
			continue
		}
		if _, ok := seen[backend]; ok {
			continue
		}
		seen[backend] = struct{}{}
		entry, err := storage.GetLocalSnapshotFor(context.Background(), backend, id)
		if err == nil && entry != nil {
			return entry, backend
		}
	}
	return nil, preferred
}

func cleanupBackendForPauseSnap(preferred, snapID string) string {
	if _, used := lookupPauseCatalog(snapID, preferred); strings.TrimSpace(used) != "" {
		return used
	}
	if strings.TrimSpace(preferred) != "" {
		return preferred
	}
	return cow.BackendXFS
}

// pauseSnapIDToGCOnDestroy is the Destroy-time pause-snap GC decision.
// keep_tombstone / delete_tombstone still own the new pause snap. Cubelet
// CleanupTemplate's the previous live pause snap after Pause succeeds.
// PAUSED without those flags is not expected on the user Destroy path; skip
// GC so we cannot drop a live pause snap if someone cubecli-destroys a
// tombstone. UNKNOWN / FAILED / RUNNING / PAUSING may hold half-finished
// or leftover snaps. After XFS Resume Master's CleanupTemplate no-ops
// while this RUNNING sandbox still holds the pause id; a user Destroy
// GCs it here (honorLiveXFSPause=false).
func pauseSnapIDToGCOnDestroy(req *cubebox.DestroyCubeSandboxRequest, sb *cubeboxstore.CubeBox) string {
	if sb == nil || isPauseKeepTombstone(req) || isPauseDeleteTombstone(req) {
		return ""
	}
	if st := sb.GetStatus(); st != nil && st.Get().State() == cubebox.ContainerState_CONTAINER_PAUSED {
		return ""
	}
	return pauseSnapshotIDForGC(sb)
}

// bestEffortCleanupPauseSnapshot removes a pause catalog. After Pause
// succeeds Cubelet GCs the previous live pause snap here; Destroy is the
// leftover / failed-pause fallback. Master is not in this path.
func (s *service) bestEffortCleanupPauseSnapshot(ctx context.Context, requestID, snapID, backend string) {
	snapID = strings.TrimSpace(snapID)
	if snapID == "" {
		return
	}
	cleanupRsp, err := s.cleanupTemplate(ctx, &cubebox.CleanupTemplateRequest{
		RequestID:  requestID,
		TemplateID: snapID,
		Backend:    backend,
	}, false)
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
