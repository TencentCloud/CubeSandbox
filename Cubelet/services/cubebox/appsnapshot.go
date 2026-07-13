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
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/grpc/metadata"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	DefaultSnapshotDir = "/usr/local/services/cubetoolbox/cube-snapshot"

	DefaultCubeRuntimePath = "/usr/local/services/cubetoolbox/cube-shim/bin/cube-runtime"

	SnapshotStatusPath = "/data/cube-shim/snapshot"
)

type CubeboxSnapshotSpec struct {
	Resource    json.RawMessage `json:"resource,omitempty"`
	Disk        json.RawMessage `json:"disk,omitempty"`
	Pmem        json.RawMessage `json:"pmem,omitempty"`
	Kernel      string          `json:"kernel,omitempty"`
	ContainerID string          `json:"container_id,omitempty"`
}

type ResourceSpec struct {
	CPU    int `json:"cpu"`
	Memory int `json:"memory"`
}

func (s *service) AppSnapshot(ctx context.Context, req *cubebox.AppSnapshotRequest) (*cubebox.AppSnapshotResponse, error) {
	rsp := &cubebox.AppSnapshotResponse{
		Ret: &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}

	createReq := req.GetCreateRequest()
	if createReq == nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "create_request is required"
		return rsp, nil
	}

	rsp.RequestID = createReq.RequestID

	if err := validateAppSnapshotAnnotations(createReq); err != nil {
		rerr, _ := ret.FromError(err)
		rsp.Ret.RetMsg = rerr.Message()
		rsp.Ret.RetCode = rerr.Code()
		return rsp, nil
	}

	templateID := createReq.GetAnnotations()[constants.MasterAnnotationAppSnapshotTemplateID]
	rsp.TemplateID = templateID
	if err := pathutil.ValidateSafeID(templateID); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid templateID: %v", err)
		return rsp, nil
	}

	if !storage.IsCowBackend() {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = "AppSnapshot requires storage_backend=cubecow"
		return rsp, nil
	}

	rt := &CubeLog.RequestTrace{
		Action:       "AppSnapshot",
		RequestID:    createReq.RequestID,
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "AppSnapshot",
		AppID:        getAppID(createReq.Annotations),
		Qualifier:    getUserAgent(ctx),
	}
	ctx = CubeLog.WithRequestTrace(ctx, rt)

	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"step":       "appSnapshot",
		"templateID": templateID,
	})

	stepLog.Infof("AppSnapshotRequest: templateID=%s", templateID)

	defer recov.HandleCrash(func(panicError interface{}) {
		stepLog.Fatalf("AppSnapshot panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = string(debug.Stack())
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	stepLog.Info("Step 1: Creating cubebox...")
	createRsp, err := s.Create(ctx, createReq)
	if err != nil {
		stepLog.Errorf("Failed to create cubebox: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to create cubebox: %v", err)
		return rsp, nil
	}

	if createRsp.Ret.RetCode == errorcode.ErrorCode_PreConditionFailed {
		stepLog.Warnf("Create cubebox failed with PreConditionFailed, trying to cleanup and retry...")

		expectedSandboxID := templateID + "_0"
		stepLog.Infof("Attempting to destroy existing sandbox: %s", expectedSandboxID)

		cleanupReq := &cubebox.DestroyCubeSandboxRequest{
			RequestID: createReq.RequestID,
			SandboxID: expectedSandboxID,
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanupCtx = inheritIncomingMetadata(cleanupCtx, ctx)
		cleanupRsp, cleanupErr := s.Destroy(cleanupCtx, cleanupReq)
		cleanupCancel()

		if cleanupErr != nil {
			stepLog.Errorf("Cleanup destroy failed: %v", cleanupErr)
			rsp.Ret = createRsp.Ret
			return rsp, nil
		}
		if !ret.IsSuccessCode(cleanupRsp.Ret.RetCode) {
			stepLog.Errorf("Cleanup destroy failed: %s", cleanupRsp.Ret.RetMsg)
			rsp.Ret = createRsp.Ret
			return rsp, nil
		}
		stepLog.Infof("Cleaned up existing sandbox: %s, retrying create...", expectedSandboxID)

		createRsp, err = s.Create(ctx, createReq)
		if err != nil {
			stepLog.Errorf("Failed to create cubebox on retry: %v", err)
			rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
			rsp.Ret.RetMsg = fmt.Sprintf("failed to create cubebox on retry: %v", err)
			return rsp, nil
		}
	}

	if !ret.IsSuccessCode(createRsp.Ret.RetCode) {
		stepLog.Errorf("Create cubebox failed: %s", createRsp.Ret.RetMsg)
		rsp.Ret = createRsp.Ret
		return rsp, nil
	}

	sandboxID := createRsp.SandboxID
	rsp.SandboxID = sandboxID
	stepLog = stepLog.WithFields(CubeLog.Fields{"sandboxID": sandboxID})
	stepLog.Infof("Cubebox created successfully: %s", sandboxID)

	snapshotSuccess := false
	temporaryCubeboxDestroyed := false
	var memoryObject *storage.CowSnapshotObject
	var rootfsObject *storage.CowSnapshotObject

	forceDestroyCubebox := func() {
		destroyCtx, destroyCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer destroyCancel()
		destroyCtx = inheritIncomingMetadata(destroyCtx, ctx)
		destroyCtx = CubeLog.WithRequestTrace(destroyCtx, rt)
		destroyCtx = namespaces.WithNamespace(destroyCtx, namespaces.Default)

		stepLog.Info("Cleanup: Force destroying cubebox...")
		forceDestroyReq := &cubebox.DestroyCubeSandboxRequest{
			RequestID: createReq.RequestID,
			SandboxID: sandboxID,
		}
		if destroyRsp, destroyErr := s.Destroy(destroyCtx, forceDestroyReq); destroyErr != nil {
			stepLog.Errorf("Force destroy failed: %v", destroyErr)
		} else if !ret.IsSuccessCode(destroyRsp.Ret.RetCode) {
			stepLog.Errorf("Force destroy failed: %s", destroyRsp.Ret.RetMsg)
		} else {
			stepLog.Info("Force destroy succeeded")
		}
	}

	defer func() {
		if !snapshotSuccess && !temporaryCubeboxDestroyed {
			forceDestroyCubebox()
		}
	}()

	cleanupSnapshotObjects := func() {
		cleanupCowSnapshotObjects(ctx, stepLog, memoryObject, rootfsObject)
	}

	stepLog.Info("Step 2: Getting cubebox spec...")
	spec, err := s.getCubeboxSnapshotSpec(ctx, sandboxID)
	if err != nil {
		stepLog.Errorf("Failed to get cubebox spec: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to get cubebox spec: %v", err)
		return rsp, nil
	}
	stepLog.Infof("Cubebox spec retrieved: resource=%s, disk=%s, pmem=%s, kernel=%s",
		string(spec.Resource), string(spec.Disk), string(spec.Pmem), spec.Kernel)

	var resourceSpec ResourceSpec
	if err := json.Unmarshal(spec.Resource, &resourceSpec); err != nil {
		stepLog.Errorf("Failed to parse resource spec: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to parse resource spec: %v", err)
		return rsp, nil
	}
	if resourceSpec.CPU <= 0 || resourceSpec.Memory <= 0 {
		stepLog.Errorf("Invalid resource spec: cpu=%d, memory=%d", resourceSpec.CPU, resourceSpec.Memory)
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid resource spec: cpu=%d, memory=%d", resourceSpec.CPU, resourceSpec.Memory)
		return rsp, nil
	}

	snapshotDir := req.GetSnapshotDir()
	if snapshotDir == "" {
		snapshotDir = DefaultSnapshotDir
	}
	specDir := fmt.Sprintf("%dC%dM", resourceSpec.CPU, resourceSpec.Memory)
	snapshotPath := filepath.Join(snapshotDir, "cubebox", templateID, specDir)
	if _, err := pathutil.ValidatePathUnderBase(snapshotDir, snapshotPath); err != nil {
		stepLog.Errorf("Invalid snapshot path: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid snapshot path: %v", err)
		return rsp, nil
	}
	rsp.SnapshotPath = snapshotPath

	tmpSnapshotPath := snapshotPath + ".tmp"
	if _, err := pathutil.ValidatePathUnderBase(snapshotDir, tmpSnapshotPath); err != nil {
		stepLog.Errorf("Invalid tmp snapshot path: %v", err)
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid tmp snapshot path: %v", err)
		return rsp, nil
	}
	memorySizeBytes := snapshotMemorySizeBytes(resourceSpec.Memory)
	stepLog.Infof("Step 3: Creating snapshot at temporary path: %s", tmpSnapshotPath)

	// NOCC:Path Traversal()
	if err := os.RemoveAll(tmpSnapshotPath); err != nil {
		stepLog.Warnf("Failed to remove existing temp directory: %v", err)
	}

	memoryObject, err = storage.CreateTemplateMemoryVolume(ctx, templateID, memorySizeBytes)
	if err != nil {
		stepLog.Errorf("Failed to create template memory volume: %v", err)
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("template memory volume already exists: %v", err)
			return rsp, nil
		}
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to create template memory volume: %v", err)
		return rsp, nil
	}
	if err := validateSnapshotMemoryObject(memoryObject, memorySizeBytes); err != nil {
		cleanupSnapshotObjects()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}

	// collectEnvdVersion uses containerd Exec, which must run before
	// cube-runtime marks the guest as app-snapshotting and disables exec.
	envdVersion := s.collectEnvdVersion(ctx, sandboxID)

	stepLog.Info("Step 4: Executing cube-runtime snapshot...")
	// AppSnapshot builds a brand-new template from a fresh sandbox: there is
	// no base memory blob to overlay onto, so we always ask for a full memory
	// snapshot. Incremental is reserved for CommitSandbox where the running
	// sandbox is bound to a prior snapshot whose memory file we can clone.
	if err := s.executeCubeRuntimeSnapshot(ctx, sandboxID, spec, tmpSnapshotPath, memoryObject.DevPath, snapshotTypeFull); err != nil {
		stepLog.Errorf("Failed to execute cube-runtime snapshot: %v", err)

		os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupSnapshotObjects()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to execute cube-runtime snapshot: %v", err)
		return rsp, nil
	}
	stepLog.Info("cube-runtime snapshot executed successfully")

	if err := writeMemoryDevFile(tmpSnapshotPath, memoryObject.DevPath); err != nil {
		stepLog.Errorf("Failed to write memory.dev: %v", err)
		os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupSnapshotObjects()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to write memory.dev: %v", err)
		return rsp, nil
	}

	rootfsObject, err = storage.CreateTemplateRootfsFromBuild(ctx, templateID)
	if err != nil {
		stepLog.Errorf("Failed to create template rootfs snapshot: %v", err)
		os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupSnapshotObjects()
		if errors.Is(err, storage.ErrCowObjectAlreadyExists) {
			rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
			rsp.Ret.RetMsg = fmt.Sprintf("template rootfs already exists: %v", err)
			return rsp, nil
		}
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to create template rootfs snapshot: %v", err)
		return rsp, nil
	}

	stepLog.Infof("Step 5: Destroying temporary cubebox (templateID=%s)...", templateID)
	destroyCtx, destroyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer destroyCancel()
	destroyCtx = inheritIncomingMetadata(destroyCtx, ctx)
	destroyCtx = CubeLog.WithRequestTrace(destroyCtx, rt)
	destroyCtx = namespaces.WithNamespace(destroyCtx, namespaces.Default)

	destroyReq := &cubebox.DestroyCubeSandboxRequest{
		RequestID: createReq.RequestID,
		SandboxID: sandboxID,
	}

	failTemporaryDestroy := func(retCode errorcode.ErrorCode, retMsg string) (*cubebox.AppSnapshotResponse, error) {
		stepLog.Warn("Fallback: trying force destroy...")
		forceDestroyCubebox()
		os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupSnapshotObjects()
		rsp.Ret.RetCode = retCode
		rsp.Ret.RetMsg = retMsg
		return rsp, nil
	}

	destroyRsp, destroyErr := s.Destroy(destroyCtx, destroyReq)
	if destroyErr != nil {
		stepLog.Errorf("Temporary cubebox destroy failed: %v", destroyErr)
		return failTemporaryDestroy(errorcode.ErrorCode_Unknown, fmt.Sprintf("failed to destroy temporary cubebox: %v", destroyErr))
	}
	if !ret.IsSuccessCode(destroyRsp.Ret.RetCode) {
		stepLog.Errorf("Temporary cubebox destroy failed: %s", destroyRsp.Ret.RetMsg)
		return failTemporaryDestroy(destroyRsp.Ret.RetCode, fmt.Sprintf("failed to destroy temporary cubebox: %s", destroyRsp.Ret.RetMsg))
	}
	temporaryCubeboxDestroyed = true

	if err := deactivateCowSnapshotObjects(ctx, stepLog, memoryObject, rootfsObject); err != nil {
		os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupSnapshotObjects()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to deactivate snapshot objects: %v", err)
		return rsp, nil
	}

	stepLog.Info("Step 6: Moving snapshot to final path...")

	// NOCC:Path Traversal()
	if err := os.RemoveAll(snapshotPath); err != nil {
		stepLog.Warnf("Failed to remove existing snapshot directory: %v", err)
	}

	if err := os.Rename(tmpSnapshotPath, snapshotPath); err != nil {
		stepLog.Errorf("Failed to move snapshot to final path: %v", err)
		os.RemoveAll(tmpSnapshotPath) // NOCC:Path Traversal()
		cleanupSnapshotObjects()
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to move snapshot: %v", err)
		return rsp, nil
	}

	stepLog.Info("Step 7: Writing snapshot status flag file...")
	if err := writeSnapshotFlag(stepLog); err != nil {
		stepLog.Warnf("Failed to write snapshot flag: %v", err)

	}

	snapshotSuccess = true
	rsp.RootfsVol = rootfsObject.Name
	rsp.MemoryVol = memoryObject.Name
	rsp.RootfsKind = rootfsObject.Kind
	rsp.MemoryKind = memoryObject.Kind
	rsp.RootfsSizeBytes = rootfsObject.SizeBytes
	versions := collectGuestEnvironmentVersions()
	rsp.GuestImageVersion = versions.GuestImage
	rsp.AgentVersion = versions.Agent
	rsp.KernelVersion = versions.Kernel
	rsp.EnvdVersion = envdVersion

	// Persist the catalog entry so subsequent create-from-template and
	// CleanupTemplate calls can resolve physical refs locally. The build
	// rootfs name is deterministic on cubelet side; we record it so cleanup
	// works even if the live volume has already been removed by other paths.
	if err := storage.WriteSnapshotCatalog(&storage.SnapshotCatalogEntry{
		SnapshotID:      templateID,
		InstanceType:    "cubebox",
		SpecDir:         specDir,
		SnapshotPath:    snapshotPath,
		MetaDir:         snapshotPath,
		RootfsVol:       rootfsObject.Name,
		RootfsKind:      rootfsObject.Kind,
		MemoryVol:       memoryObject.Name,
		MemoryKind:      memoryObject.Kind,
		BuildRootfsVol:  storage.TemplateBuildRootfsName(templateID),
		BuildRootfsKind: storage.CowKindVolume,
		RootfsSizeBytes: rootfsObject.SizeBytes,
		Kind:            storage.CatalogKindTemplate,
	}); err != nil {
		// Catalog write failures do not invalidate the snapshot: master will
		// still receive the physical references in the response and the
		// deterministic-name cleanup fallback keeps working. Log loudly so
		// operators notice drift between master and cubelet local view.
		stepLog.Warnf("failed to persist snapshot catalog for %s: %v", templateID, err)
	}

	stepLog.Infof("AppSnapshot completed successfully: snapshotPath=%s", snapshotPath)
	rsp.Ret.RetMsg = "success"
	return rsp, nil
}

func inheritIncomingMetadata(dst context.Context, src context.Context) context.Context {
	if md, ok := metadata.FromIncomingContext(src); ok {
		return metadata.NewIncomingContext(dst, md.Copy())
	}
	return dst
}

func writeSnapshotFlag(stepLog *log.CubeWrapperLogEntry) error {

	if _, err := os.Stat(SnapshotStatusPath); err == nil {
		stepLog.Info("Snapshot status flag file already exists, skipping")
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(SnapshotStatusPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	file, err := os.Create(SnapshotStatusPath)
	if err != nil {
		return fmt.Errorf("failed to create flag file: %w", err)
	}
	file.Close()

	cmd := exec.Command("chattr", "+i", SnapshotStatusPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to set immutable attribute: %w", err)
	}

	stepLog.Infof("Snapshot status flag file created: %s", SnapshotStatusPath)
	return nil
}

func validateAppSnapshotAnnotations(req *cubebox.RunCubeSandboxRequest) error {
	annotations := req.GetAnnotations()
	if annotations == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat,
			"annotations are required for app snapshot")
	}

	createFlag, ok := annotations[constants.MasterAnnotationsAppSnapshotCreate]
	if !ok || createFlag != "true" {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat,
			fmt.Sprintf("annotation %s must be set to \"true\"", constants.MasterAnnotationsAppSnapshotCreate))
	}

	templateID, ok := annotations[constants.MasterAnnotationAppSnapshotTemplateID]
	if !ok || templateID == "" {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat,
			fmt.Sprintf("annotation %s is required and must not be empty", constants.MasterAnnotationAppSnapshotTemplateID))
	}

	return nil
}

func (s *service) getCubeboxSnapshotSpec(ctx context.Context, sandboxID string) (*CubeboxSnapshotSpec, error) {

	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cubebox from store: %w", err)
	}

	ns := cb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)

	container, err := s.cubeboxMgr.client.LoadContainer(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to load container: %w", err)
	}

	info, err := container.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get container info: %w", err)
	}

	if info.Spec == nil {
		return nil, fmt.Errorf("container spec is nil")
	}

	specAny, err := typeurl.UnmarshalAny(info.Spec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal container spec: %w", err)
	}

	type ociSpec struct {
		Annotations map[string]string `json:"annotations,omitempty"`
	}

	specBytes, err := json.Marshal(specAny)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal spec: %w", err)
	}

	var spec ociSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return nil, fmt.Errorf("failed to unmarshal spec to ociSpec: %w", err)
	}

	annotations := spec.Annotations
	if annotations == nil {
		return nil, fmt.Errorf("spec annotations are nil")
	}

	result := &CubeboxSnapshotSpec{
		Kernel:      annotations[constants.AnnotationsVMKernelPath],
		ContainerID: snapshotContainerIDFromAnnotations(annotations, sandboxID),
	}

	if vmmres, ok := annotations[constants.AnnotationsVMSpecKey]; ok && vmmres != "" {
		result.Resource = json.RawMessage(vmmres)
	}

	if disk, ok := annotations[constants.AnnotationsMountListKey]; ok && disk != "" {
		result.Disk = json.RawMessage(disk)
	}

	if pmem, ok := annotations[constants.AnnotationPmem]; ok && pmem != "" {
		result.Pmem = json.RawMessage(pmem)
	}

	return result, nil
}

// snapshotTypeFull asks cube-runtime to capture every memory page of the VM
// (the historical default).
const snapshotTypeFull = "full"

// snapshotTypeIncremental asks cube-runtime to write only CoW anonymous pages
// into the destination memory file, leaving non-anonymous regions to whatever
// the destination file already contains (i.e. the reflink-cloned base).
const snapshotTypeIncremental = "incremental"

// snapshotTypeSoftDirty asks cube-runtime to write only the pages dirtied
// since the previous soft-dirty snapshot (a true per-cycle delta) on top of
// the destination memory file. The destination MUST already contain a valid
// base image (the reflink-cloned previous snapshot's memory), otherwise pages
// untouched-since-last-clear would read back as zero on restore. Cubelet
// guarantees this precondition by reflink-cloning the binding base before
// invoking cube-runtime; if the base cannot be resolved, the caller falls
// back to snapshotTypeFull instead.
//
// The host kernel needs CONFIG_MEM_SOFT_DIRTY=y for soft-dirty to do
// anything useful; on kernels without it the hypervisor silently downgrades
// to the pagemap_anon (incremental) path, so this value is safe to send
// unconditionally.
const snapshotTypeSoftDirty = "soft-dirty"

// normalizeSnapshotType defaults to snapshotTypeFull when an empty value is
// supplied so callers that don't care (legacy code paths) keep producing full
// snapshots.
func normalizeSnapshotType(snapshotType string) string {
	switch strings.TrimSpace(strings.ToLower(snapshotType)) {
	case snapshotTypeIncremental:
		return snapshotTypeIncremental
	case snapshotTypeSoftDirty:
		return snapshotTypeSoftDirty
	case "", snapshotTypeFull:
		return snapshotTypeFull
	default:
		return snapshotTypeFull
	}
}

// buildCubeRuntimeSnapshotArgs is split out from executeCubeRuntimeSnapshot
// so tests can assert on the exact argv that will be passed to cube-runtime
// without touching exec or the filesystem.
func buildCubeRuntimeSnapshotArgs(sandboxID string, spec *CubeboxSnapshotSpec, snapshotPath, memoryVol, snapshotType string) []string {
	args := []string{
		"snapshot",
		"--app-snapshot",
		"--vm-id", sandboxID,
		"--path", snapshotPath,
		"--force",
		"--snapshot-type", normalizeSnapshotType(snapshotType),
	}
	if spec != nil {
		if len(spec.Resource) > 0 {
			args = append(args, "--resource", string(spec.Resource))
		}
		if len(spec.Disk) > 0 {
			args = append(args, "--disk", string(spec.Disk))
		}
		if len(spec.Pmem) > 0 {
			args = append(args, "--pmem", string(spec.Pmem))
		}
		if spec.Kernel != "" {
			args = append(args, "--kernel", spec.Kernel)
		}
		if spec.ContainerID != "" {
			args = append(args, "--container-id", spec.ContainerID)
		}
	}
	if memoryVol != "" {
		args = append(args, "--memory-vol", snapshotMemoryVolURL(memoryVol))
	}
	return args
}

func (s *service) executeCubeRuntimeSnapshot(ctx context.Context, sandboxID string, spec *CubeboxSnapshotSpec, snapshotPath, memoryVol, snapshotType string) error {
	snapshotType = normalizeSnapshotType(snapshotType)
	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"sandboxID":    sandboxID,
		"snapshotPath": snapshotPath,
		"snapshotType": snapshotType,
	})

	args := buildCubeRuntimeSnapshotArgs(sandboxID, spec, snapshotPath, memoryVol, snapshotType)

	stepLog.Infof("Executing: %s %v", DefaultCubeRuntimePath, args)

	cmd := exec.CommandContext(ctx, DefaultCubeRuntimePath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		stepLog.Errorf("cube-runtime snapshot failed: %v, output: %s", err, string(output))
		return fmt.Errorf("cube-runtime snapshot failed: %w, output: %s", err, string(output))
	}

	stepLog.Infof("cube-runtime snapshot output: %s", string(output))
	return nil
}

func snapshotContainerIDFromAnnotations(annotations map[string]string, sandboxID string) string {
	if len(annotations) == 0 {
		return strings.TrimSpace(sandboxID)
	}
	if value := strings.TrimSpace(annotations[constants.AnnotationAppSnapshotContainerID]); value != "" {
		return value
	}
	return strings.TrimSpace(sandboxID)
}

func snapshotMemorySizeBytes(memoryMiB int) uint64 {
	if memoryMiB <= 0 {
		return 0
	}
	const mib = 1024 * 1024
	size := uint64(memoryMiB) * mib
	return alignUp(size, mib)
}

func validateSnapshotMemoryObject(memoryObject *storage.CowSnapshotObject, requestedSizeBytes uint64) error {
	if memoryObject == nil {
		return fmt.Errorf("template memory volume is nil")
	}
	if memoryObject.DevPath == "" {
		return fmt.Errorf("template memory volume %s has empty dev path", memoryObject.Name)
	}
	if requestedSizeBytes > 0 && memoryObject.SizeBytes < requestedSizeBytes {
		return fmt.Errorf("template memory volume %s size %d is smaller than requested %d", memoryObject.Name, memoryObject.SizeBytes, requestedSizeBytes)
	}
	return nil
}

func snapshotMemoryVolURL(memoryVol string) string {
	if strings.Contains(memoryVol, "://") {
		return memoryVol
	}
	return "file://" + memoryVol
}

func alignUp(value, alignment uint64) uint64 {
	if alignment == 0 || value%alignment == 0 {
		return value
	}
	return value + alignment - value%alignment
}

func writeMemoryDevFile(snapshotPath, memoryDev string) error {
	if memoryDev == "" {
		return fmt.Errorf("memory dev path is empty")
	}
	return os.WriteFile(filepath.Join(snapshotPath, "memory.dev"), []byte(memoryDev+"\n"), 0644)
}

func cleanupCowSnapshotObjects(ctx context.Context, stepLog *log.CubeWrapperLogEntry, memoryObject, rootfsObject *storage.CowSnapshotObject) {
	cleanupCowSnapshotObject(ctx, stepLog, "memory volume", memoryObject)
	cleanupCowSnapshotObject(ctx, stepLog, "rootfs snapshot", rootfsObject)
}

func cleanupCowSnapshotObject(ctx context.Context, stepLog *log.CubeWrapperLogEntry, objectLabel string, object *storage.CowSnapshotObject) {
	if object == nil || object.Name == "" {
		return
	}
	if cleanupErr := storage.DeleteCowObject(ctx, object.Name, object.Kind); cleanupErr != nil {
		stepLog.Warnf("failed to cleanup %s %s: %v", objectLabel, object.Name, cleanupErr)
	}
}

func deactivateCowSnapshotObjects(ctx context.Context, stepLog *log.CubeWrapperLogEntry, memoryObject, rootfsObject *storage.CowSnapshotObject) error {
	if err := deactivateCowSnapshotObject(ctx, stepLog, "rootfs snapshot", rootfsObject); err != nil {
		return err
	}
	return deactivateCowSnapshotObject(ctx, stepLog, "memory volume", memoryObject)
}

func deactivateCowSnapshotObject(ctx context.Context, stepLog *log.CubeWrapperLogEntry, objectLabel string, object *storage.CowSnapshotObject) error {
	if object == nil || object.Name == "" {
		return nil
	}
	if err := storage.DeactivateCowObject(ctx, object.Name, object.Kind); err != nil {
		return fmt.Errorf("deactivate %s %s: %w", objectLabel, object.Name, err)
	}
	stepLog.Infof("deactivated %s %s", objectLabel, object.Name)
	return nil
}
