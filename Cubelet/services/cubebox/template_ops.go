// Copyright (c) 2024 Tencent Inc.
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
	"runtime/debug"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (s *service) CommitSandbox(ctx context.Context, req *cubebox.CommitSandboxRequest) (*cubebox.CommitSandboxResponse, error) {
	rsp := &cubebox.CommitSandboxResponse{
		RequestID:  req.GetRequestID(),
		SandboxID:  req.GetSandboxID(),
		TemplateID: strings.TrimSpace(req.GetTemplateID()),
		Ret:        &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if rsp.TemplateID == "" {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "templateID is required"
		return rsp, nil
	}
	if err := pathutil.ValidateSafeID(rsp.TemplateID); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid templateID: %v", err)
		return rsp, nil
	}
	if rsp.SandboxID == "" {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "sandboxID is required"
		return rsp, nil
	}

	rt := &CubeLog.RequestTrace{
		Action:       "CommitSandbox",
		RequestID:    req.GetRequestID(),
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "CommitSandbox",
	}
	ctx = CubeLog.WithRequestTrace(ctx, rt)
	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"step":       "commitSandbox",
		"templateID": rsp.TemplateID,
		"sandboxID":  rsp.SandboxID,
	})

	defer recov.HandleCrash(func(panicError interface{}) {
		stepLog.Fatalf("CommitSandbox panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = string(debug.Stack())
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	if !storage.IsCowBackend() {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = "CommitSandbox requires storage_backend=cubecow"
		return rsp, nil
	}

	unlock := s.sandboxLifecycleLocks.Lock(rsp.SandboxID)
	defer unlock()

	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, rsp.SandboxID)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = fmt.Sprintf("sandbox is not found: %v", err)
		return rsp, nil
	}
	rootVolumeName, err := validateCommitSandboxTarget(cb)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}

	spec, err := s.getCubeboxSnapshotSpec(ctx, rsp.SandboxID)
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

	snapshotDir := req.GetSnapshotDir()
	if snapshotDir == "" {
		snapshotDir = DefaultSnapshotDir
	}
	specDir := fmt.Sprintf("%dC%dM", resourceSpec.CPU, resourceSpec.Memory)
	snapshotPath := filepath.Join(snapshotDir, "cubebox", rsp.TemplateID, specDir)
	if _, err := pathutil.ValidatePathUnderBase(snapshotDir, snapshotPath); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid snapshot path: %v", err)
		return rsp, nil
	}
	rsp.SnapshotPath = snapshotPath
	tmpSnapshotPath := snapshotPath + ".tmp"
	if _, err := pathutil.ValidatePathUnderBase(snapshotDir, tmpSnapshotPath); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid tmp snapshot path: %v", err)
		return rsp, nil
	}
	memorySizeBytes := snapshotMemorySizeBytes(resourceSpec.Memory)

	sourceRootfs, err := storage.GetSandboxRootfsForSnapshot(ctx, rsp.SandboxID, rootVolumeName)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = fmt.Sprintf("failed to resolve sandbox rootfs: %v", err)
		return rsp, nil
	}
	rootfsObject, err := storage.CommitTemplateRootfs(ctx, sourceRootfs, rsp.TemplateID)
	if err != nil {
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("template rootfs already exists: %v", err)
			return rsp, nil
		}
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to create rootfs snapshot: %v", err)
		return rsp, nil
	}
	// Resolve / build the memory artifact:
	//   - if the sandbox is bound to a previous snapshot whose memory blob
	//     can be resolved, reflink-clone that blob and ask cube-runtime for
	//     a soft-dirty per-cycle delta;
	//   - otherwise (lineage broken: missing/purged catalog or upstream
	//     volume gone) create a fresh empty volume and fall back to a full
	//     snapshot.
	memoryObject, snapshotTypeForCmd, err := prepareCommitMemoryArtifact(ctx, stepLog, cb, rsp.TemplateID, memorySizeBytes)
	if err != nil {
		if cleanupErr := storage.DeleteCowObject(ctx, rootfsObject.Name, rootfsObject.Kind); cleanupErr != nil {
			stepLog.Warnf("failed to cleanup rootfs snapshot after memory artifact failure: %v", cleanupErr)
		}
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("template memory object already exists: %v", err)
			return rsp, nil
		}
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to prepare memory artifact for snapshot: %v", err)
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
	}

	_ = os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
	// CommitSandbox snapshots a running sandbox whose memory artifact has
	// just been prepared above. snapshotTypeForCmd carries the right type
	// for the path we took: soft-dirty when reflink-cloning a base, or full
	// when degrading because no base could be resolved. AppSnapshot keeps
	// using the default full type via its own call site.
	stepLog = stepLog.WithFields(CubeLog.Fields{"snapshotType": snapshotTypeForCmd})
	if err := s.executeCubeRuntimeSnapshot(ctx, rsp.SandboxID, spec, tmpSnapshotPath, memoryObject.DevPath, snapshotTypeForCmd); err != nil {
		_ = os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to execute cube-runtime snapshot: %v", err)
		return rsp, nil
	}
	// cube-runtime returned success, which means the hypervisor has
	// committed the delta to the memory file *and*, on the soft-dirty path,
	// already issued clear_soft_dirty() to start the next tracking window.
	// From this point on, the next CommitSandbox on the same VM must use
	// rsp.TemplateID as its base (anything older would lose the bytes the
	// guest just wrote into this snapshot). We stamp the in-memory binding
	// immediately so a follow-up commit picks it up; SyncByID at the end of
	// the success path persists it (mirroring the rollback flow). If a
	// later step fails and cleanupArtifacts deletes memoryObject, the stale
	// binding routes the next commit through the fallback-to-full branch
	// in prepareCommitMemoryArtifact, which is self-contained and safe.
	setRuntimeSnapshotBindingLabels(cb, rsp.TemplateID, time.Now().UTC())
	if err := writeMemoryDevFile(tmpSnapshotPath, memoryObject.DevPath); err != nil {
		_ = os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to write memory.dev: %v", err)
		return rsp, nil
	}
	if err := deactivateCowSnapshotObjects(ctx, stepLog, memoryObject, rootfsObject); err != nil {
		_ = os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to deactivate snapshot objects: %v", err)
		return rsp, nil
	}
	// NOCC:Path Traversal()
	if err := os.RemoveAll(snapshotPath); err != nil {
		stepLog.Warnf("failed to remove existing snapshot path: %v", err)
	}
	if err := os.Rename(tmpSnapshotPath, snapshotPath); err != nil {
		_ = os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to move snapshot: %v", err)
		return rsp, nil
	}
	if err := writeSnapshotFlag(stepLog); err != nil {
		stepLog.Warnf("failed to write snapshot flag: %v", err)
	}
	rsp.RootfsVol = rootfsObject.Name
	rsp.MemoryVol = memoryObject.Name
	rsp.RootfsKind = rootfsObject.Kind
	rsp.MemoryKind = memoryObject.Kind
	rsp.RootfsDev = rootfsObject.DevPath
	rsp.MemoryDev = memoryObject.DevPath
	rsp.RootfsSizeBytes = rootfsObject.SizeBytes
	versions := collectGuestEnvironmentVersions()
	rsp.GuestImageVersion = versions.GuestImage
	rsp.AgentVersion = versions.Agent
	rsp.KernelVersion = versions.Kernel
	// The source sandbox stays running through commit, so probe its real envd
	// version in-guest after the memory snapshot. Best-effort: empty on failure.
	rsp.EnvdVersion = s.collectEnvdVersion(ctx, rsp.SandboxID)
	if err := storage.WriteSnapshotCatalog(&storage.SnapshotCatalogEntry{
		SnapshotID:      rsp.TemplateID,
		InstanceType:    "cubebox",
		SpecDir:         specDir,
		SnapshotPath:    snapshotPath,
		MetaDir:         snapshotPath,
		RootfsVol:       rootfsObject.Name,
		RootfsKind:      rootfsObject.Kind,
		MemoryVol:       memoryObject.Name,
		MemoryKind:      memoryObject.Kind,
		RootfsSizeBytes: rootfsObject.SizeBytes,
		Kind:            storage.CatalogKindRuntimeSnapshot,
	}); err != nil {
		// Catalog write failures do not invalidate the snapshot: master will
		// still receive the physical references in the response and rollback
		// path keeps the legacy fallback. Log loudly so operators notice
		// drift between master and cubelet local view.
		stepLog.Warnf("failed to persist snapshot catalog for %s: %v", rsp.TemplateID, err)
	}
	// Persist the runtime-snapshot binding update we did in-memory after
	// cube-runtime returned. Mirrors the rollback flow's SyncByID call so
	// that a process restart recovers the new commit lineage and so any
	// downstream component reading the cubebox metadata sees the same
	// ancestor as resolveBaseSnapshotID will return on the next commit.
	s.cubeboxMgr.cubeboxManger.SyncByID(ctx, cb.ID)
	stepLog.Infof("CommitSandbox completed successfully: snapshotPath=%s", snapshotPath)
	return rsp, nil
}

func validateCommitSandboxTarget(cb *cubeboxstore.CubeBox) (string, error) {
	if cb == nil {
		return "", errors.New("sandbox is not found")
	}
	if cb.GetStatus() == nil || cb.GetStatus().Get().State() != cubebox.ContainerState_CONTAINER_RUNNING {
		return "", fmt.Errorf("sandbox %s is not running", cb.ID)
	}
	for _, container := range cb.AllContainers() {
		if container == nil || container.Config == nil {
			continue
		}
		if err := validateNoHostPathVolumes(container.Config); err != nil {
			return "", err
		}
	}
	if err := validateCommitVolumeSources(cb); err != nil {
		return "", err
	}
	rootVolumeName := ""
	for _, container := range cb.AllContainers() {
		if container == nil || container.Config == nil {
			continue
		}
		for _, mount := range container.Config.GetVolumeMounts() {
			if mount == nil || mount.GetContainerPath() != "/" {
				continue
			}
			if rootVolumeName != "" && rootVolumeName != mount.GetName() {
				return "", fmt.Errorf("multiple rootfs volume mounts found: %s and %s", rootVolumeName, mount.GetName())
			}
			rootVolumeName = mount.GetName()
		}
	}
	if rootVolumeName == "" {
		return "", fmt.Errorf("sandbox %s has no writable rootfs volume mount", cb.ID)
	}
	return rootVolumeName, nil
}

func validateCommitVolumeSources(cb *cubeboxstore.CubeBox) error {
	if cb == nil {
		return nil
	}
	if len(cb.Volumes) == 0 {
		for _, container := range cb.AllContainers() {
			if container == nil || container.Config == nil {
				continue
			}
			for _, mount := range container.Config.GetVolumeMounts() {
				if mount != nil && mount.GetContainerPath() != "/" {
					return fmt.Errorf("sandbox %s has volume mounts without persisted volume sources; CommitSandbox cannot verify host dependencies", cb.ID)
				}
			}
		}
		return nil
	}
	usedVolumes := map[string]struct{}{}
	for _, container := range cb.AllContainers() {
		if container == nil || container.Config == nil {
			continue
		}
		for _, mount := range container.Config.GetVolumeMounts() {
			if mount == nil || mount.GetName() == "" {
				continue
			}
			usedVolumes[mount.GetName()] = struct{}{}
		}
	}
	for _, volume := range cb.Volumes {
		if volume == nil || volume.GetName() == "" {
			continue
		}
		if _, ok := usedVolumes[volume.GetName()]; !ok {
			continue
		}
		source := volume.GetVolumeSource()
		if source == nil {
			continue
		}
		if hostDirs := source.GetHostDirVolumes(); hostDirs != nil {
			for _, hostDir := range hostDirs.GetVolumeSources() {
				if hostDir != nil && hostDir.GetHostPath() != "" {
					return fmt.Errorf("host_dir volume %s is not supported by CommitSandbox", volume.GetName())
				}
			}
		}
		if sandboxPath := source.GetSandboxPath(); sandboxPath != nil {
			switch sandboxPath.GetType() {
			case cubebox.SandboxPathType_Directory.String(), cubebox.SandboxPathType_SharedBindMount.String():
				return fmt.Errorf("sandbox_path volume %s with type %s is not supported by CommitSandbox", volume.GetName(), sandboxPath.GetType())
			}
		}
	}
	return nil
}

func validateNoHostPathVolumes(config *cubebox.ContainerConfig) error {
	if config == nil {
		return nil
	}
	for _, mount := range config.GetVolumeMounts() {
		if mount != nil && mount.GetHostPath() != "" {
			return fmt.Errorf("hostPath volume mount %s is not supported by CommitSandbox", mount.GetName())
		}
	}
	return nil
}

func (s *service) CleanupTemplate(ctx context.Context, req *cubebox.CleanupTemplateRequest) (*cubebox.CleanupTemplateResponse, error) {
	rsp := &cubebox.CleanupTemplateResponse{
		RequestID:  req.GetRequestID(),
		TemplateID: strings.TrimSpace(req.GetTemplateID()),
		Ret:        &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if rsp.TemplateID == "" {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "templateID is required"
		return rsp, nil
	}
	if err := pathutil.ValidateSafeID(rsp.TemplateID); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid templateID: %v", err)
		return rsp, nil
	}
	// snapshot_path is deprecated as of v4: cubelet resolves it from local
	// catalog. We still validate it for backward compatibility so old masters
	// can keep talking to new cubelets during a coordinated upgrade.
	if sp := req.GetSnapshotPath(); sp != "" {
		if err := pathutil.ValidateNoTraversal(sp); err != nil {
			rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
			rsp.Ret.RetMsg = fmt.Sprintf("invalid snapshotPath: %v", err)
			return rsp, nil
		}
	}
	refs, snapshotPath, err := resolveCleanupRefs(ctx, rsp.TemplateID, req.GetObjects(), req.GetSnapshotPath())
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	if storage.IsCowBackend() {
		if err := storage.CleanupCowTemplateObjects(ctx, refs); err != nil {
			rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
			rsp.Ret.RetMsg = fmt.Sprintf("failed to cleanup cubecow objects: %v", err)
			return rsp, nil
		}
	}
	if err := storage.CleanupTemplateLocalData(ctx, rsp.TemplateID, snapshotPath); err != nil {
		rerr, _ := ret.FromError(err)
		if rerr == nil || rerr.Code() == 0 {
			rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
			rsp.Ret.RetMsg = err.Error()
			return rsp, nil
		}
		rsp.Ret.RetCode = rerr.Code()
		rsp.Ret.RetMsg = rerr.Message()
	}
	storage.DeleteSnapshotCatalog(rsp.TemplateID)
	return rsp, nil
}

// resolveCleanupRefs is the v4 catalog-first resolution for CleanupTemplate.
// Priority:
//  1. caller-supplied Objects (legacy master compatibility) -> parse as-is
//  2. local snapshot catalog hit -> derive rootfs/memory/build_rootfs from
//     entry; prefer entry.SnapshotPath over caller-supplied
//  3. deterministic fallback (DefaultTemplateObjectRefs) with caller-supplied
//     snapshot path; logs catalog miss for operability
//
// snapshotPath returned is what CleanupTemplateLocalData should remove on disk;
// catalog entry SnapshotPath wins over caller-supplied path when both exist.
func resolveCleanupRefs(ctx context.Context, templateID string, objects []*cubebox.CowObjectRef, callerSnapshotPath string) ([]storage.CowObjectRef, string, error) {
	if len(objects) > 0 {
		refs, err := parseCowObjectRefs(objects)
		if err != nil {
			return nil, "", err
		}
		return refs, ensureSnapshotCleanupPath(ctx, templateID, callerSnapshotPath), nil
	}
	entry, err := storage.GetLocalSnapshot(ctx, templateID)
	if err == nil && entry != nil {
		refs := cubecowRefsFromCatalogEntry(templateID, entry)
		snapshotPath := strings.TrimSpace(entry.SnapshotPath)
		if snapshotPath == "" {
			snapshotPath = callerSnapshotPath
		}
		return refs, ensureSnapshotCleanupPath(ctx, templateID, snapshotPath), nil
	}
	if err != nil && !errors.Is(err, storage.ErrSnapshotCatalogNotFound) {
		log.G(ctx).Warnf("CleanupTemplate %s: catalog lookup failed (%v); falling back to deterministic refs", templateID, err)
	} else {
		log.G(ctx).Warnf("CleanupTemplate %s: catalog miss; falling back to deterministic refs", templateID)
	}
	return storage.DefaultTemplateObjectRefs(templateID), ensureSnapshotCleanupPath(ctx, templateID, callerSnapshotPath), nil
}

// ensureSnapshotCleanupPath implements FIX-4: when neither the caller nor the
// local catalog supplies a snapshot path (catalog miss / legacy master), derive
// the deterministic snapshot directory used by CommitSandbox so the on-disk
// snapshot is not orphaned (L11). The derived path is the templateID-level dir
// (`<DefaultSnapshotDir>/cubebox/<templateID>`) which contains every spec
// variant, and is bounded by ValidatePathUnderBase (S3). On any validation
// failure we return the original (empty) path rather than an unsafe guess.
func ensureSnapshotCleanupPath(ctx context.Context, templateID, snapshotPath string) string {
	if strings.TrimSpace(snapshotPath) != "" {
		return snapshotPath
	}
	if err := pathutil.ValidateSafeID(templateID); err != nil {
		log.G(ctx).Warnf("CleanupTemplate %s: cannot derive snapshot dir, unsafe templateID: %v", templateID, err)
		return snapshotPath
	}
	guessed := filepath.Join(DefaultSnapshotDir, "cubebox", templateID)
	if _, err := pathutil.ValidatePathUnderBase(DefaultSnapshotDir, guessed); err != nil {
		log.G(ctx).Warnf("CleanupTemplate %s: derived snapshot dir %q rejected: %v", templateID, guessed, err)
		return snapshotPath
	}
	log.G(ctx).Infof("CleanupTemplate %s: derived deterministic snapshot dir %q for cleanup", templateID, guessed)
	return guessed
}

func cubecowRefsFromCatalogEntry(templateID string, entry *storage.SnapshotCatalogEntry) []storage.CowObjectRef {
	if entry == nil {
		return storage.DefaultTemplateObjectRefs(templateID)
	}
	refs := make([]storage.CowObjectRef, 0, 3)
	if name := strings.TrimSpace(entry.RootfsVol); name != "" {
		refs = append(refs, storage.CowObjectRef{Name: name, Kind: entry.RootfsKind, Role: "rootfs"})
	}
	if name := strings.TrimSpace(entry.MemoryVol); name != "" {
		refs = append(refs, storage.CowObjectRef{Name: name, Kind: entry.MemoryKind, Role: "memory"})
	}
	if name := strings.TrimSpace(entry.BuildRootfsVol); name != "" {
		refs = append(refs, storage.CowObjectRef{Name: name, Kind: entry.BuildRootfsKind, Role: "build_rootfs"})
	}
	if len(refs) == 0 {
		return storage.DefaultTemplateObjectRefs(templateID)
	}
	return refs
}

func parseCowObjectRefs(objects []*cubebox.CowObjectRef) ([]storage.CowObjectRef, error) {
	refs := make([]storage.CowObjectRef, 0, len(objects))
	for _, object := range objects {
		if object == nil {
			continue
		}
		name := strings.TrimSpace(object.GetName())
		role := strings.TrimSpace(object.GetRole())
		kind := strings.TrimSpace(object.GetKind())
		if name == "" {
			continue
		}
		if err := pathutil.ValidateSafeID(name); err != nil {
			return nil, fmt.Errorf("invalid object name %q: %v", name, err)
		}
		if role == "" {
			return nil, fmt.Errorf("object role is required for %q", name)
		}
		refs = append(refs, storage.CowObjectRef{
			Name: name,
			Kind: kind,
			Role: role,
		})
	}
	return refs, nil
}
