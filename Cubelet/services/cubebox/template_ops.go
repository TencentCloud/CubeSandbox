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

	unlock := s.updateSandboxLocks.Lock(rsp.SandboxID)
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

	// Resolve the base memory object FIRST: this is the source we will
	// reflink-clone for the incremental memory snapshot. We do it before
	// committing the rootfs so that a missing/broken catalog state fails
	// fast without producing partial cubecow garbage.
	baseMemoryObject, err := resolveBaseMemoryObject(ctx, cb)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = fmt.Sprintf("failed to resolve base memory for incremental snapshot: %v", err)
		return rsp, nil
	}

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
	// Reflink-clone the base memory blob into the new template memory name.
	// The resulting cubecow snapshot starts out byte-for-byte identical to
	// the base, satisfying the hypervisor's incremental snapshot precondition
	// of "destination memory file already exists with a valid base image".
	memoryObject, err := storage.CommitTemplateMemoryFromBase(ctx, baseMemoryObject, rsp.TemplateID, memorySizeBytes)
	if err != nil {
		if cleanupErr := storage.DeleteCowObject(ctx, rootfsObject.Name, rootfsObject.Kind); cleanupErr != nil {
			stepLog.Warnf("failed to cleanup rootfs snapshot after memory clone failure: %v", cleanupErr)
		}
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("template memory object already exists: %v", err)
			return rsp, nil
		}
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to clone base memory for incremental snapshot: %v", err)
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
	// CommitSandbox snapshots a running sandbox whose memory blob has just
	// been reflink-cloned above; with a valid base present, cube-runtime can
	// overlay only the CoW anon pages and skip writing the (much larger)
	// non-anonymous regions. AppSnapshot keeps using the default full type.
	if err := s.executeCubeRuntimeSnapshot(ctx, rsp.SandboxID, spec, tmpSnapshotPath, memoryObject.DevPath, snapshotTypeIncremental); err != nil {
		_ = os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupArtifacts()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to execute cube-runtime snapshot: %v", err)
		return rsp, nil
	}
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
		return refs, callerSnapshotPath, nil
	}
	entry, err := storage.GetLocalSnapshot(ctx, templateID)
	if err == nil && entry != nil {
		refs := cubecowRefsFromCatalogEntry(templateID, entry)
		snapshotPath := strings.TrimSpace(entry.SnapshotPath)
		if snapshotPath == "" {
			snapshotPath = callerSnapshotPath
		}
		return refs, snapshotPath, nil
	}
	if err != nil && !errors.Is(err, storage.ErrSnapshotCatalogNotFound) {
		log.G(ctx).Warnf("CleanupTemplate %s: catalog lookup failed (%v); falling back to deterministic refs", templateID, err)
	} else {
		log.G(ctx).Warnf("CleanupTemplate %s: catalog miss; falling back to deterministic refs", templateID)
	}
	return storage.DefaultTemplateObjectRefs(templateID), callerSnapshotPath, nil
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
