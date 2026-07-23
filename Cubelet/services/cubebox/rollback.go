// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	shimUpdateActionAnnotation          = "cube.shimapi.update.action"
	shimUpdateRollbackRestoreAnnotation = "cube.shimapi.update.rollback.restore_config"
	shimUpdateRollbackAction            = "RollbackSnapshot"
)

type rollbackRestoreConfig struct {
	SourceURL    string               `json:"source_url"`
	MemoryVolURL string               `json:"memory_vol_url,omitempty"`
	Disks        []rollbackDiskConfig `json:"disks,omitempty"`
}

type rollbackDiskConfig struct {
	Path              string          `json:"path,omitempty"`
	ID                string          `json:"id,omitempty"`
	RateLimiterConfig json.RawMessage `json:"rate_limiter_config,omitempty"`
}

type snapshotDiskSpec struct {
	Path              string          `json:"path"`
	RateLimiterConfig json.RawMessage `json:"rate_limiter_config,omitempty"`
	VolumeSourceName  string          `json:"volume_source,omitempty"`
}

func (s *service) RollbackSandbox(ctx context.Context, req *cubebox.RollbackSandboxRequest) (*cubebox.RollbackSandboxResponse, error) {
	rsp := &cubebox.RollbackSandboxResponse{
		RequestID:  req.GetRequestID(),
		SandboxID:  req.GetSandboxID(),
		SnapshotID: req.GetSnapshotID(),
		NewGen:     req.GetNewGen(),
		Ret:        &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}

	if err := validateRollbackSandboxRequest(req); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	if !storage.IsCowBackend() {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = "RollbackSandbox requires storage_backend=cubecow"
		return rsp, nil
	}

	unlock := s.sandboxLifecycleLocks.Lock(req.GetSandboxID())
	defer unlock()

	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"step":       "rollbackSandbox",
		"sandboxID":  req.GetSandboxID(),
		"snapshotID": req.GetSnapshotID(),
		"newGen":     req.GetNewGen(),
	})

	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, req.GetSandboxID())
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = fmt.Sprintf("sandbox is not found: %v", err)
		return rsp, nil
	}
	if cb.GetStatus() == nil || cb.GetStatus().IsTerminated() {
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskStateInvalid
		rsp.Ret.RetMsg = "sandbox is not running"
		return rsp, nil
	}

	rootVolumeName, err := validateCommitSandboxTarget(cb)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}
	currentRootfs, err := storage.GetSandboxRootfsForSnapshot(ctx, req.GetSandboxID(), rootVolumeName)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = fmt.Sprintf("failed to resolve current rootfs: %v", err)
		return rsp, nil
	}
	if req.GetNewGen() <= currentRootfs.Gen {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("new_gen %d must be greater than current gen %d", req.GetNewGen(), currentRootfs.Gen)
		return rsp, nil
	}
	rsp.OldRootfsVol = currentRootfs.Name

	rootfsVol, memoryVol, memoryKind, metaDir, err := resolveRollbackTargets(ctx, req)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = err.Error()
		return rsp, nil
	}

	refs, err := storage.ResolveSnapshotForRollback(ctx, rootfsVol, memoryVol, memoryKind)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = fmt.Sprintf("failed to resolve snapshot objects: %v", err)
		return rsp, nil
	}

	newRootfs, err := storage.RollbackDeriveNewGen(ctx, req.GetSandboxID(), refs.Rootfs.Name, req.GetNewGen(), req.GetDesiredSize())
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to derive rollback rootfs: %v", err)
		return rsp, nil
	}
	cleanupNewRootfs := true
	defer func() {
		if cleanupNewRootfs {
			if cleanupErr := storage.DeleteCowObject(ctx, newRootfs.Name, newRootfs.Kind); cleanupErr != nil {
				stepLog.Warnf("failed to cleanup derived rollback rootfs %s: %v", newRootfs.Name, cleanupErr)
			}
		}
	}()

	restoreConfig, err := s.buildRollbackRestoreConfig(ctx, req.GetSandboxID(), metaDir, currentRootfs, newRootfs, refs.Memory)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to build restore config: %v", err)
		return rsp, nil
	}

	// Mark the cubebox as rolling-back BEFORE entering the shim. While
	// updateShimForRollback runs the shim holds its sandbox mutex doing
	// delete_vm + resume_vm_with_config; concurrent DeadGC heartbeats
	// calling task.Status() will time out or return Unknown and would
	// otherwise stamp the in-memory Status with Unknown=true / FinishedAt=now,
	// breaking a follow-up pause. The flag is cleared in the deferred unset
	// regardless of outcome; see scanDeadContainer for the matching skip.
	setSandboxRollingBack(cb, true)
	defer setSandboxRollingBack(cb, false)

	if err := s.updateShimForRollback(ctx, cb, restoreConfig); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("failed to update shim for rollback: %v", err)
		return rsp, nil
	}
	cleanupNewRootfs = false

	// Scrub any "terminated" markers a concurrent path may have stamped
	// onto the in-memory Status while shim's delete_vm + resume_vm_with_config
	// was running. We do NOT consult containerd here: the shim has already
	// confirmed rollback success, and containerd's task tracking lags
	// (delete_vm reports the OLD task as stopped before resume_vm_with_config
	// re-binds a new running task). The DeadGC heartbeat will refresh
	// Pid/StartedAt against the live shim once RollingBack clears.
	resetSandboxStatusAfterRollback(cb)

	newRootfs.MountName = currentRootfs.MountName
	if err := storage.PersistSandboxRootfsAfterRollback(ctx, req.GetSandboxID(), newRootfs); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = fmt.Sprintf("rollback restored VM but failed to persist storage info: %v", err)
		return rsp, nil
	}

	rsp.RootfsVol = newRootfs.Name
	rsp.RootfsKind = newRootfs.Kind
	rsp.RootfsDev = newRootfs.DevPath
	rsp.NewGen = newRootfs.Gen
	rsp.MemoryVol = refs.Memory.Name
	rollbackTime := time.Now().UTC()
	setRuntimeSnapshotBindingLabels(cb, req.GetSnapshotID(), rollbackTime)
	// Rollback restarts the VM from req.SnapshotID, so this is also the
	// new last-restore base. Update both labels here; the existing
	// SyncByID call below covers the persistence for both.
	setRuntimeRestoreBaseLabels(cb, req.GetSnapshotID(), rollbackTime)

	if err := storage.DeleteCowObject(ctx, currentRootfs.Name, currentRootfs.Kind); err != nil {
		rsp.OldRootfsDeleted = false
		rsp.Ret.RetMsg = fmt.Sprintf("rollback succeeded; old rootfs cleanup deferred: %v", err)
		stepLog.Warnf("rollback succeeded but failed to delete old rootfs %s: %v", currentRootfs.Name, err)
	} else {
		rsp.OldRootfsDeleted = true
	}

	s.cubeboxMgr.cubeboxManger.SyncByID(ctx, cb.ID)
	stepLog.Infof("RollbackSandbox completed successfully: newRootfs=%s oldRootfs=%s oldDeleted=%t", rsp.RootfsVol, rsp.OldRootfsVol, rsp.OldRootfsDeleted)
	return rsp, nil
}

func validateRollbackSandboxRequest(req *cubebox.RollbackSandboxRequest) error {
	if req.GetSandboxID() == "" {
		return fmt.Errorf("sandboxID is required")
	}
	if req.GetSnapshotID() == "" {
		return fmt.Errorf("snapshotID is required")
	}
	if err := pathutil.ValidateSafeID(req.GetSnapshotID()); err != nil {
		return fmt.Errorf("invalid snapshotID: %v", err)
	}
	if v := strings.TrimSpace(req.GetRootfsVol()); v != "" {
		if err := pathutil.ValidateSafeID(v); err != nil {
			return fmt.Errorf("invalid rootfs_vol: %v", err)
		}
	}
	if v := strings.TrimSpace(req.GetMemoryVol()); v != "" {
		if err := pathutil.ValidateSafeID(v); err != nil {
			return fmt.Errorf("invalid memory_vol: %v", err)
		}
	}
	if req.GetNewGen() == 0 {
		return fmt.Errorf("new_gen is required")
	}
	return nil
}

// resolveRollbackTargets returns the rootfs/memory volume names and meta_dir
// that should be used for this rollback. When master supplies them on the
// request they win (backward compatible); when they are empty cubelet looks
// up its local snapshot catalog keyed by snapshot_id. Mixed input is rejected
// because the partial state is almost always a master-side bug.
func resolveRollbackTargets(ctx context.Context, req *cubebox.RollbackSandboxRequest) (string, string, string, string, error) {
	rootfsVol := strings.TrimSpace(req.GetRootfsVol())
	memoryVol := strings.TrimSpace(req.GetMemoryVol())
	metaDir := strings.TrimSpace(req.GetMetaDir())
	if rootfsVol != "" && memoryVol != "" && metaDir != "" {
		// memoryKind is intentionally left empty here. The legacy contract
		// did not propagate it via the request, so we let
		// ResolveSnapshotForRollback fall back to its historical default
		// (kind=volume), keeping master-driven rollbacks bit-for-bit
		// compatible with their pre-incremental behavior.
		return rootfsVol, memoryVol, "", metaDir, nil
	}
	if rootfsVol != "" || memoryVol != "" || metaDir != "" {
		return "", "", "", "", fmt.Errorf("rollback: rootfs_vol/memory_vol/meta_dir must be all-set or all-empty; got rootfs_vol=%q memory_vol=%q meta_dir=%q", rootfsVol, memoryVol, metaDir)
	}
	entry, err := storage.GetLocalSnapshot(ctx, req.GetSnapshotID())
	if err != nil {
		return "", "", "", "", fmt.Errorf("rollback: local snapshot catalog lookup for %s failed: %w", req.GetSnapshotID(), err)
	}
	return entry.RootfsVol, entry.MemoryVol, entry.MemoryKind, entry.MetaDir, nil
}

func (s *service) buildRollbackRestoreConfig(ctx context.Context, sandboxID, metaDir string, currentRootfs, newRootfs, memory *storage.CowSnapshotObject) (string, error) {
	if currentRootfs == nil || currentRootfs.DevPath == "" {
		return "", fmt.Errorf("current rootfs dev path is required")
	}
	if newRootfs == nil || newRootfs.DevPath == "" {
		return "", fmt.Errorf("new rootfs dev path is required")
	}
	if memory == nil || memory.DevPath == "" {
		return "", fmt.Errorf("snapshot memory dev path is required")
	}

	spec, err := s.getCubeboxSnapshotSpec(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	disks, err := rollbackDisksFromSnapshotSpec(spec, currentRootfs.MountName, currentRootfs.DevPath, newRootfs.DevPath)
	if err != nil {
		return "", err
	}
	cfg := rollbackRestoreConfig{
		SourceURL:    fileURL(snapshotStateDir(metaDir)),
		MemoryVolURL: memory.DevPath,
		Disks:        disks,
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func rollbackDisksFromSnapshotSpec(spec *CubeboxSnapshotSpec, rootfsMountName, oldRootfsDev, newRootfsDev string) ([]rollbackDiskConfig, error) {
	if spec == nil || len(spec.Disk) == 0 {
		return nil, fmt.Errorf("snapshot disk spec is empty")
	}
	var src []snapshotDiskSpec
	if err := json.Unmarshal(spec.Disk, &src); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot disk spec: %w", err)
	}
	disks := make([]rollbackDiskConfig, 0, len(src))
	replaced := false
	for idx, disk := range src {
		path := disk.Path
		if path == oldRootfsDev || (rootfsMountName != "" && disk.VolumeSourceName == rootfsMountName) {
			path = newRootfsDev
			replaced = true
		}
		disks = append(disks, rollbackDiskConfig{
			Path:              path,
			ID:                fmt.Sprintf("disk-%d", idx),
			RateLimiterConfig: disk.RateLimiterConfig,
		})
	}
	if !replaced {
		return nil, fmt.Errorf("current rootfs dev %s not found in snapshot disk spec", oldRootfsDev)
	}
	return disks, nil
}

func fileURL(path string) string {
	if strings.Contains(path, "://") {
		return path
	}
	return "file://" + filepath.Clean(path)
}

func snapshotStateDir(metaDir string) string {
	if strings.Contains(metaDir, "://") {
		return metaDir
	}
	clean := filepath.Clean(metaDir)
	if filepath.Base(clean) == "snapshot" {
		return clean
	}
	return filepath.Join(clean, "snapshot")
}

// resetSandboxStatusAfterRollback wipes any "terminated" markers that a
// concurrent code path (DeadGC heartbeat, TaskExit event handler) may
// have stamped onto the in-memory Status while the shim was tearing
// down the old VM and resuming the new one. The shim has already
// confirmed rollback success by the time this is called, so the
// cubebox is logically Running again.
//
// Deliberately does NOT query containerd: with the shim's
// delete_vm + resume_vm_with_config sequence, the underlying
// containerd task transitions from running → stopped → running and is
// frequently reported as "stopped" or yields a transient NotFound
// while resume_vm_with_config re-binds. Using containerd as the
// source of truth here is racy and was the original failure mode.
// Pid/StartedAt are intentionally left to the next DeadGC heartbeat
// to refresh against the live shim once RollingBack clears.
func resetSandboxStatusAfterRollback(cb *cubeboxstore.CubeBox) {
	if cb == nil {
		return
	}
	now := time.Now().UnixNano()
	for _, c := range cb.AllContainers() {
		if c == nil || c.Status == nil {
			continue
		}
		_ = c.Status.Update(func(s cubeboxstore.Status) (cubeboxstore.Status, error) {
			s.FinishedAt = 0
			s.ExitCode = 0
			s.Reason = ""
			s.Message = ""
			s.Unknown = false
			s.PausedAt = 0
			s.PausingAt = 0
			if s.StartedAt == 0 {
				s.StartedAt = now
			}
			return s, nil
		})
	}
}

// setSandboxRollingBack toggles the in-memory RollingBack flag on every
// container of the cubebox. Used as a paired set/clear around the
// shim rollback call so background scanners (DeadGC) skip the cubebox
// while its task state is intentionally in flight.
func setSandboxRollingBack(cb *cubeboxstore.CubeBox, rollingBack bool) {
	if cb == nil {
		return
	}
	for _, c := range cb.AllContainers() {
		if c == nil || c.Status == nil {
			continue
		}
		_ = c.Status.Update(func(st cubeboxstore.Status) (cubeboxstore.Status, error) {
			st.RollingBack = rollingBack
			return st, nil
		})
	}
}

func (s *service) updateShimForRollback(ctx context.Context, cb *cubeboxstore.CubeBox, restoreConfig string) error {
	ns := cb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)
	firstContainer := cb.FirstContainer()
	if firstContainer == nil || firstContainer.Container == nil {
		return fmt.Errorf("sandbox %s has no first container task", cb.ID)
	}
	task, err := firstContainer.Container.Task(ctx, nil)
	if err != nil {
		return err
	}
	return task.Update(ctx, containerd.WithAnnotations(map[string]string{
		shimUpdateActionAnnotation:          shimUpdateRollbackAction,
		shimUpdateRollbackRestoreAnnotation: restoreConfig,
	}))
}
