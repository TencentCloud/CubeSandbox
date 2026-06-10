// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

const (
	snapshotRequestLockPrefix = "snapshot-request:"
	snapshotIDPrefix          = "snap-"
)

// snapshotCreateJobRequest is the WAL payload for an in-flight snapshot-create
// operation. Note: the canonical create-request is no longer carried inside the
// job payload. It now lives in sandboxspec keyed by sandbox_id (Phase 1 of the
// snapshot control-plane refactor) and is loaded on demand. We keep
// SpecFingerprint so resume can detect if the spec has drifted between attempts.
type snapshotCreateJobRequest struct {
	RequestID       string `json:"request_id"`
	SandboxID       string `json:"sandbox_id"`
	SnapshotID      string `json:"snapshot_id"`
	NodeID          string `json:"node_id"`
	NodeIP          string `json:"node_ip"`
	DisplayName     string `json:"display_name,omitempty"`
	SpecFingerprint string `json:"spec_fingerprint,omitempty"`
}

type snapshotRollbackJobRequest struct {
	RequestID   string `json:"request_id"`
	SandboxID   string `json:"sandbox_id"`
	SnapshotID  string `json:"snapshot_id"`
	NodeID      string `json:"node_id"`
	NodeIP      string `json:"node_ip"`
	NewGen      uint32 `json:"new_gen"`
	DesiredSize uint64 `json:"desired_size"`
}

type snapshotDeleteJobRequest struct {
	RequestID  string `json:"request_id"`
	SnapshotID string `json:"snapshot_id"`
	NodeID     string `json:"node_id"`
	NodeIP     string `json:"node_ip"`
}

type snapshotRollbackResult struct {
	NewGen uint32 `json:"new_gen"`
}

// SubmitSandboxSnapshot snapshots an existing running sandbox.
//
// The caller only supplies identifiers (requestID/sandboxID/hostID/hostIP) and
// an optional displayName. The canonical create-request is resolved internally
// from sandboxspec (the create-time spec we persist for every sandbox), with a
// best-effort fallback to GetTemplateRequest(SandboxData.TemplateID) when
// specstore has no record (e.g. for sandboxes created before the spec store
// existed). This removes the historical requirement that callers re-supply the
// original CreateCubeSandboxReq, which was the original motivation for this
// refactor.
func SubmitSandboxSnapshot(ctx context.Context, requestID, sandboxID, hostID, hostIP, displayName string) (*sandboxtypes.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, errors.New("requestID is required")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, errors.New("sandboxID is required")
	}
	nodeID := strings.TrimSpace(hostID)
	nodeIP := strings.TrimSpace(hostIP)

	originReq, err := loadSandboxCreateRequestFn(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	if originReq.Request == nil {
		originReq.Request = &sandboxtypes.Request{RequestID: requestID}
	} else {
		originReq.Request.RequestID = requestID
	}
	createReq, storedReq, err := buildSnapshotRequests(originReq, "")
	if err != nil {
		return nil, err
	}
	lockKey := snapshotRequestLockPrefix + requestID
	var jobID string
	reusedExistingJob := false
	if err := withTemplateWriteLock(lockKey, func() error {
		if existing, err := getTemplateImageJobByRequestID(ctx, requestID); err == nil {
			if existing.Operation != JobOperationSnapshotCreate {
				return fmt.Errorf("%w: request %s is already bound to %s", ErrTemplateAttemptInProgress, requestID, existing.Operation)
			}
			if !snapshotCreateRequestMatches(existing.RequestJSON, requestID, sandboxID, nodeID, nodeIP, displayName, storedReq) {
				return fmt.Errorf("%w: request %s payload does not match existing snapshot create job", ErrTemplateAttemptInProgress, requestID)
			}
			jobID = existing.JobID
			reusedExistingJob = true
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if _, err := getActiveSnapshotJobBySandboxID(ctx, sandboxID); err == nil {
			return fmt.Errorf("%w: sandbox %s already has an active snapshot operation", ErrTemplateAttemptInProgress, sandboxID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if err := ensureSnapshotNodeWritable(ctx, nodeID, nodeIP, false); err != nil {
			return err
		}

		snapshotID := generateSnapshotID()
		createReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = snapshotID
		storedReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = snapshotID
		fingerprint := buildCommitTemplateSpecFingerprint(storedReq)
		requestJSON, err := marshalSnapshotCreateRequest(snapshotCreateJobRequest{
			RequestID:       requestID,
			SandboxID:       sandboxID,
			SnapshotID:      snapshotID,
			NodeID:          nodeID,
			NodeIP:          nodeIP,
			DisplayName:     displayName,
			SpecFingerprint: fingerprint,
		})
		if err != nil {
			return err
		}
		jobID = uuid.NewString()
		record := &models.TemplateImageJob{
			JobID:                   jobID,
			TemplateID:              snapshotID,
			RequestID:               requestID,
			SandboxID:               sandboxID,
			ResourceType:            JobResourceTypeSnapshot,
			ResourceID:              snapshotID,
			AttemptNo:               1,
			Operation:               JobOperationSnapshotCreate,
			NodeID:                  nodeID,
			NodeIP:                  nodeIP,
			TemplateSpecFingerprint: fingerprint,
			InstanceType:            createReq.InstanceType,
			NetworkType:             createReq.NetworkType,
			Status:                  JobStatusPending,
			Phase:                   JobPhaseSnapshotting,
			Progress:                0,
			RequestJSON:             requestJSON,
			TemplateStatus:          StatusCreating,
		}
		defOpts := definitionCreateOptions{
			Kind:                      TemplateKindSnapshot,
			OriginSandboxID:           sandboxID,
			OriginNodeID:              nodeID,
			DisplayName:               displayName,
			StorageBackend:            StorageBackendCow,
			RootfsSizeBytesAtSnapshot: parseSystemDiskSizeBytes(storedReq),
		}
		return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := createDefinitionTx(ctx, tx, snapshotID, storedReq, createReq.InstanceType, constants.GetAppSnapshotVersion(createReq.Annotations), defOpts); err != nil {
				return err
			}
			return tx.Table(constants.TemplateImageJobTableName).Create(record).Error
		})
	}); err != nil {
		return nil, err
	}

	info, err := GetTemplateImageJobInfo(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if reusedExistingJob {
		return resumeSnapshotCreateJob(ctx, info, originReq, sandboxID, nodeID, nodeIP)
	}
	return executeSnapshotCreateJob(ctx, info, originReq, sandboxID, nodeID, nodeIP)
}

// loadSandboxCreateRequestFn is the indirection point used by
// SubmitSandboxSnapshot so tests can swap in a deterministic spec without
// going through the sandboxspec table or sandbox info RPC. Production code
// should not reassign it.
var loadSandboxCreateRequestFn = loadSandboxCreateRequest

// registerTemplateReplicaForSnapshot wraps localcache.RegisterTemplateReplica
// so tests can patch the indirection (the one-liner wrapper in localcache is
// inlined and therefore not safely monkey-patchable).
//
//go:noinline
func registerTemplateReplicaForSnapshot(templateID, nodeID string, sizeBytes int64) {
	localcache.RegisterTemplateReplica(templateID, nodeID, sizeBytes)
}

// loadSandboxCreateRequest is the authoritative way to get a sandbox's
// create-time spec on master. It prefers the canonical sandboxspec record
// (always present for sandboxes created after Phase 1 lands) and falls back to
// reconstructing from the base template when the spec store has no entry
// (legacy sandboxes; the base-template fallback can be lossy because user
// overrides are not part of base templates).
func loadSandboxCreateRequest(ctx context.Context, sandboxID string) (*sandboxtypes.CreateCubeSandboxReq, error) {
	req, err := sandboxspec.Get(ctx, sandboxID)
	if err == nil {
		return req, nil
	}
	if !errors.Is(err, sandboxspec.ErrSandboxSpecNotFound) {
		return nil, fmt.Errorf("load sandbox spec: %w", err)
	}
	info, infoErr := getSandboxData(ctx, sandboxID, "")
	if infoErr != nil {
		return nil, fmt.Errorf("sandbox spec missing and sandbox info lookup failed: %w", infoErr)
	}
	templateID := strings.TrimSpace(info.TemplateID)
	if templateID == "" {
		return nil, fmt.Errorf("sandbox %s has no spec record and no base template id; cannot reconstruct create request", sandboxID)
	}
	tmplReq, tmplErr := GetTemplateRequest(ctx, templateID)
	if tmplErr != nil {
		return nil, fmt.Errorf("sandbox spec missing and base template %s lookup failed: %w", templateID, tmplErr)
	}
	log.G(ctx).Warnf("sandbox %s spec record missing; falling back to base template %s (overrides may be lost)", sandboxID, templateID)
	return tmplReq, nil
}

func runSnapshotCreateJob(ctx context.Context, jobID, sandboxID, nodeID, nodeIP string, createReq, storedReq *sandboxtypes.CreateCubeSandboxReq) error {
	success := false
	defer func() {
		recordSnapshotCommitResult(success)
	}()
	snapshotID := strings.TrimSpace(createReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"snapshot_id": snapshotID,
		"sandbox_id":  sandboxID,
		"node_id":     nodeID,
	})
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    JobPhaseSnapshotting,
		"progress": 10,
	})

	commitRsp, err := cubelet.CommitSandbox(ctx, cubelet.GetCubeletAddr(nodeIP), &cubeboxv1.CommitSandboxRequest{
		RequestID:   uuid.NewString(),
		SandboxID:   sandboxID,
		TemplateID:  snapshotID,
		SnapshotDir: createReq.SnapshotDir,
	})
	if err != nil {
		return failSnapshotCreateJob(ctx, jobID, snapshotID, nodeIP, "", nil, err)
	}
	if commitRsp.GetRet() == nil || int(commitRsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		msg := "commit sandbox failed"
		if commitRsp.GetRet() != nil && strings.TrimSpace(commitRsp.GetRet().GetRetMsg()) != "" {
			msg = commitRsp.GetRet().GetRetMsg()
		}
		return failSnapshotCreateJob(ctx, jobID, snapshotID, nodeIP, commitRsp.GetSnapshotPath(), commitRsp, errors.New(msg))
	}

	snapshotPath := commitRsp.GetSnapshotPath()
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":         JobPhaseRegistering,
		"progress":      70,
		"node_id":       nodeID,
		"node_ip":       nodeIP,
		"snapshot_path": snapshotPath,
	})

	// Snapshot replicas only carry control-plane state from this point onward.
	// Physical references (rootfs/memory vol/dev, meta_dir, snapshot path) live
	// in cubelet's local snapshot catalog and are looked up there by snapshot_id
	// for rollback/cleanup. Master is intentionally kept thin.
	replica := ReplicaStatus{
		NodeID:       nodeID,
		NodeIP:       nodeIP,
		InstanceType: createReq.InstanceType,
		Spec:         calculateRequestSpec(createReq),
		Status:       ReplicaStatusReady,
		Phase:        ReplicaPhaseReady,
		LastJobID:    jobID,
	}
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":    JobPhaseRegistering,
		"progress": 85,
	})
	if err := UpsertReplica(ctx, snapshotID, createReq.InstanceType, replica); err != nil {
		return failSnapshotCreateJob(ctx, jobID, snapshotID, nodeIP, snapshotPath, commitRsp, err)
	}
	setTemplateLocalityCache(snapshotID, []ReplicaStatus{replica})
	registerTemplateReplicaForSnapshot(snapshotID, nodeID, 1)

	if cacheErr := setTemplateRequestCache(snapshotID, storedReq); cacheErr != nil {
		logger.Warnf("set snapshot request cache failed: %v", cacheErr)
	}
	if err := updateDefinitionFields(ctx, snapshotID, map[string]any{
		"status":                        StatusReady,
		"last_error":                    "",
		"storage_backend":               StorageBackendCow,
		"rootfs_size_bytes_at_snapshot": commitRsp.GetRootfsSizeBytes(),
	}); err != nil {
		return failSnapshotCreateJob(ctx, jobID, snapshotID, nodeIP, snapshotPath, commitRsp, err)
	}
	resultPayload, _ := json.Marshal(map[string]any{
		"snapshot_path":     snapshotPath,
		"rootfs_vol":        commitRsp.GetRootfsVol(),
		"memory_vol":        commitRsp.GetMemoryVol(),
		"rootfs_kind":       commitRsp.GetRootfsKind(),
		"memory_kind":       commitRsp.GetMemoryKind(),
		"rootfs_dev":        commitRsp.GetRootfsDev(),
		"memory_dev":        commitRsp.GetMemoryDev(),
		"rootfs_size_bytes": commitRsp.GetRootfsSizeBytes(),
		"storage_backend":   StorageBackendCow,
		"origin_sandbox_id": sandboxID,
		"origin_node_id":    nodeID,
		"display_name":      "",
	})
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":          JobStatusReady,
		"phase":           JobPhaseRegistering,
		"progress":        100,
		"template_status": StatusReady,
		"result_json":     string(resultPayload),
		"error_message":   "",
	}); err != nil {
		return err
	}
	success = true
	logger.Infof("snapshot create finished successfully")
	return nil
}

func RollbackSandboxToSnapshot(ctx context.Context, requestID, sandboxID, snapshotID, instanceType string) (*sandboxtypes.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	if strings.TrimSpace(requestID) == "" {
		return nil, errors.New("requestID is required")
	}
	lockKey := snapshotRequestLockPrefix + requestID
	var jobID string
	reusedExistingJob := false
	var existingRequest snapshotRollbackJobRequest
	var newGen uint32
	var desiredSize uint64
	var replica ReplicaStatus
	var nodeID string
	var nodeIP string
	if err := withTemplateWriteLock(lockKey, func() error {
		if existing, err := getTemplateImageJobByRequestID(ctx, requestID); err == nil {
			if existing.Operation != JobOperationSnapshotRollback {
				return fmt.Errorf("%w: request %s is already bound to %s", ErrTemplateAttemptInProgress, requestID, existing.Operation)
			}
			if !snapshotRollbackRequestMatches(existing.RequestJSON, requestID, sandboxID, snapshotID) {
				return fmt.Errorf("%w: request %s payload does not match existing snapshot rollback job", ErrTemplateAttemptInProgress, requestID)
			}
			jobID = existing.JobID
			reusedExistingJob = true
			if err := json.Unmarshal([]byte(existing.RequestJSON), &existingRequest); err != nil {
				return err
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		def, err := GetDefinition(ctx, snapshotID)
		if err != nil {
			return err
		}
		if !isSnapshotDefinition(def) {
			return fmt.Errorf("%w: template %s is not a snapshot", ErrTemplateAttemptInProgress, snapshotID)
		}
		if !strings.EqualFold(def.Status, StatusReady) {
			return fmt.Errorf("%w: snapshot %s is in status %s", ErrTemplateAttemptInProgress, snapshotID, def.Status)
		}
		if def.OriginSandboxID != sandboxID {
			return fmt.Errorf("%w: snapshot %s does not belong to sandbox %s", ErrTemplateAttemptInProgress, snapshotID, sandboxID)
		}
		if !strings.EqualFold(def.StorageBackend, StorageBackendCow) {
			return fmt.Errorf("%w: snapshot %s does not use cubecow backend", ErrTemplateAttemptInProgress, snapshotID)
		}
		sandboxInfo, err := getSandboxData(ctx, sandboxID, instanceType)
		if err != nil {
			return err
		}
		nodeID = strings.TrimSpace(sandboxInfo.HostID)
		nodeIP = strings.TrimSpace(sandboxInfo.HostIP)
		if def.OriginNodeID != "" && nodeID != "" && def.OriginNodeID != nodeID {
			return fmt.Errorf("%w: snapshot %s is pinned to node %s, sandbox is on %s", ErrTemplateAttemptInProgress, snapshotID, def.OriginNodeID, nodeID)
		}
		if _, err := getActiveSnapshotJobBySandboxID(ctx, sandboxID); err == nil {
			return fmt.Errorf("%w: sandbox %s already has an active snapshot operation", ErrTemplateAttemptInProgress, sandboxID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if _, err := getActiveSnapshotJobByResourceID(ctx, snapshotID); err == nil {
			return fmt.Errorf("%w: snapshot %s already has an active snapshot operation", ErrTemplateAttemptInProgress, snapshotID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := ensureSnapshotNodeWritable(ctx, nodeID, nodeIP, false); err != nil {
			return err
		}
		replica, err = getSnapshotReadyReplica(ctx, snapshotID, def.OriginNodeID)
		if err != nil {
			return err
		}
		newGen, err = allocateNextRollbackGen(ctx, sandboxID)
		if err != nil {
			return err
		}
		sandboxSizeBytes, err := resolveSandboxDesiredSizeBytes(ctx, sandboxInfo)
		if err != nil {
			return err
		}
		desiredSize = maxUint64(def.RootfsSizeBytesAtSnapshot, sandboxSizeBytes)
		payload, err := marshalSnapshotRollbackRequest(requestID, sandboxID, snapshotID, nodeID, nodeIP, newGen, desiredSize)
		if err != nil {
			return err
		}
		attemptNo, retryOfJobID, err := nextSnapshotAttempt(ctx, snapshotID)
		if err != nil {
			return err
		}
		jobID = uuid.NewString()
		record := &models.TemplateImageJob{
			JobID:        jobID,
			TemplateID:   snapshotID,
			RequestID:    requestID,
			SandboxID:    sandboxID,
			ResourceType: JobResourceTypeSnapshot,
			ResourceID:   snapshotID,
			AttemptNo:    attemptNo,
			RetryOfJobID: retryOfJobID,
			Operation:    JobOperationSnapshotRollback,
			NodeID:       nodeID,
			NodeIP:       nodeIP,
			InstanceType: instanceType,
			Status:       JobStatusPending,
			Phase:        JobPhaseRollbackPreparing,
			Progress:     0,
			RequestJSON:  payload,
		}
		return store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).Create(record).Error
	}); err != nil {
		return nil, err
	}
	info, err := GetTemplateImageJobInfo(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if reusedExistingJob {
		replica, err = getSnapshotReadyReplica(ctx, existingRequest.SnapshotID, existingRequest.NodeID)
		if err != nil {
			return nil, err
		}
		return resumeSnapshotRollbackJob(ctx, info, existingRequest, replica)
	}
	return executeSnapshotRollbackJob(ctx, info, sandboxID, snapshotID, nodeID, nodeIP, replica, newGen, desiredSize)
}

func runSnapshotRollbackJob(ctx context.Context, jobID, sandboxID, snapshotID, nodeID, nodeIP string, replica ReplicaStatus, newGen uint32, desiredSize uint64) error {
	success := false
	defer func() {
		recordSnapshotRollbackResult(success)
	}()
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    JobPhaseRollbackDriving,
		"progress": 25,
	})
	// Master no longer persists physical refs on snapshot replicas. Cubelet
	// resolves rootfs_vol/memory_vol/meta_dir from its local catalog keyed by
	// snapshot_id (the all-empty branch in resolveRollbackTargets).
	rsp, err := cubelet.RollbackSandbox(ctx, cubelet.GetCubeletAddr(nodeIP), &cubeboxv1.RollbackSandboxRequest{
		RequestID:   uuid.NewString(),
		SandboxID:   sandboxID,
		SnapshotID:  snapshotID,
		NewGen:      newGen,
		DesiredSize: desiredSize,
	})
	if err != nil {
		return failSnapshotRollbackJob(ctx, jobID, JobPhaseRollbackDriving, nil, err)
	}
	if rsp.GetRet() == nil || int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		msg := "rollback sandbox failed"
		if rsp.GetRet() != nil && strings.TrimSpace(rsp.GetRet().GetRetMsg()) != "" {
			msg = rsp.GetRet().GetRetMsg()
		}
		return failSnapshotRollbackJob(ctx, jobID, JobPhaseRollbackDriving, nil, errors.New(msg))
	}
	resultPayload, _ := json.Marshal(rsp)
	// v5: replica rows no longer carry physical refs; the rollback RPC
	// response (driven from cubelet's catalog) is the only source.
	if err := AcquireSnapshotRuntimeRef(ctx, SnapshotRuntimeRefInfo{
		SnapshotID: snapshotID,
		SandboxID:  sandboxID,
		NodeID:     nodeID,
		NodeIP:     nodeIP,
		MemoryVol:  strings.TrimSpace(rsp.GetMemoryVol()),
		RootfsVol:  rsp.GetRootfsVol(),
		SandboxGen: rsp.GetNewGen(),
	}); err != nil {
		return failSnapshotRollbackJob(ctx, jobID, JobPhaseRollbackRecovering, resultPayload, err)
	}
	message := ""
	if rsp.GetRet() != nil {
		message = rsp.GetRet().GetRetMsg()
	}
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":        JobStatusReady,
		"phase":         JobPhaseReady,
		"progress":      100,
		"result_json":   string(resultPayload),
		"error_message": message,
	}); err != nil {
		return err
	}
	success = true
	return nil
}

// DeleteSnapshot tears down a snapshot synchronously: it returns only when
// the underlying delete job has settled into a terminal state (READY on
// success, FAILED on error).  There is no "started, please poll" return
// path — pending / running states are converted into errors by
// `finalizeSynchronousSnapshotJob`.  The caller can therefore treat a nil
// error as "snapshot is gone (replica + metadata + caches all cleaned)"
// and a non-nil error as "delete either rejected up-front or ran to
// failure".
//
// Behaviour summary:
//
//   - Up-front guards (kind, status, in-use, active-job, active runtime
//     refs) all run inside `withTemplateWriteLock`, so a duplicate request
//     for the same `requestID` is idempotent: a re-arrived call either
//     resumes the still-pending job or surfaces the prior terminal result.
//   - The actual delete (`runSnapshotDeleteJob`) runs under a detached
//     context produced by `synchronousSnapshotJobContext`, capped at
//     `snapshotOperationTimeout` (15 min) so a stuck cubelet cannot wedge
//     the master goroutine forever.  The wider request context is allowed
//     to cancel the *response*, but the job itself is owned by master and
//     completes (or fails) regardless.
//   - On crash / restart, `reconcileSnapshotDefinitionTimeouts` will mark
//     definitions left in `deleting` past the timeout as `failed`, and the
//     next `DeleteSnapshot` call for the same id will re-attempt cleanly.
//
// The snapshot API is synchronous — CubeAPI waits for a terminal state
// and does not expose a polling interface to callers.
func DeleteSnapshot(ctx context.Context, requestID, snapshotID, instanceType string) (*sandboxtypes.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	if strings.TrimSpace(requestID) == "" {
		return nil, errors.New("requestID is required")
	}
	lockKey := snapshotRequestLockPrefix + requestID
	var jobID string
	reusedExistingJob := false
	var existingRequest snapshotDeleteJobRequest
	if err := withTemplateWriteLock(lockKey, func() error {
		if existing, err := getTemplateImageJobByRequestID(ctx, requestID); err == nil {
			if existing.Operation != JobOperationSnapshotDelete {
				return fmt.Errorf("%w: request %s is already bound to %s", ErrTemplateAttemptInProgress, requestID, existing.Operation)
			}
			if !snapshotDeleteRequestMatches(existing.RequestJSON, requestID, snapshotID) {
				return fmt.Errorf("%w: request %s payload does not match existing snapshot delete job", ErrTemplateAttemptInProgress, requestID)
			}
			jobID = existing.JobID
			reusedExistingJob = true
			if err := json.Unmarshal([]byte(existing.RequestJSON), &existingRequest); err != nil {
				return err
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		def, err := GetDefinition(ctx, snapshotID)
		if err != nil {
			return err
		}
		if !isSnapshotDefinition(def) {
			return fmt.Errorf("%w: template %s is not a snapshot", ErrTemplateAttemptInProgress, snapshotID)
		}
		if strings.EqualFold(def.Status, StatusCreating) {
			return fmt.Errorf("%w: snapshot %s is still creating", ErrTemplateAttemptInProgress, snapshotID)
		}
		if strings.EqualFold(def.Status, StatusDeleting) {
			return fmt.Errorf("%w: snapshot %s is already deleting", ErrTemplateAttemptInProgress, snapshotID)
		}
		if _, err := getActiveSnapshotJobByResourceID(ctx, snapshotID); err == nil {
			return fmt.Errorf("%w: snapshot %s already has an active snapshot operation", ErrTemplateAttemptInProgress, snapshotID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		targets, err := discoverTemplateCleanupTargets(ctx, snapshotID, instanceType)
		if err != nil {
			return err
		}
		if _, err := snapshotDeleteLocators(targets); err != nil {
			return err
		}
		if targets.shouldCheckInUse() {
			if inUse, err := isTemplateInUse(ctx, snapshotID, targets.InstanceType); err != nil {
				log.G(ctx).Warnf("snapshot %s in-use precheck failed (continuing with delete): %v", snapshotID, err)
			} else if inUse {
				log.G(ctx).Warnf("snapshot %s still has active sandbox(es) referencing it; proceeding with delete (rootfs is reflink/CoW-derived and memory vol remains accessible to running hypervisors)", snapshotID)
			}
		}
		if activeRefs, err := ListActiveSnapshotRuntimeRefs(ctx, snapshotID); err != nil {
			log.G(ctx).Warnf("snapshot %s runtime-ref precheck failed (continuing with delete): %v", snapshotID, err)
		} else if len(activeRefs) > 0 {
			log.G(ctx).Warnf("snapshot %s still has %d active runtime ref(s): %s; proceeding with delete", snapshotID, len(activeRefs), formatSnapshotRuntimeRefConsumers(activeRefs))
		}
		attemptNo, retryOfJobID, err := nextSnapshotAttempt(ctx, snapshotID)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(snapshotDeleteJobRequest{
			RequestID:  requestID,
			SnapshotID: snapshotID,
			NodeID:     def.OriginNodeID,
		})
		if err != nil {
			return err
		}
		jobID = uuid.NewString()
		record := &models.TemplateImageJob{
			JobID:        jobID,
			TemplateID:   snapshotID,
			RequestID:    requestID,
			ResourceType: JobResourceTypeSnapshot,
			ResourceID:   snapshotID,
			AttemptNo:    attemptNo,
			RetryOfJobID: retryOfJobID,
			Operation:    JobOperationSnapshotDelete,
			NodeID:       def.OriginNodeID,
			InstanceType: instanceType,
			Status:       JobStatusPending,
			Phase:        JobPhaseDeleting,
			Progress:     0,
			RequestJSON:  string(payload),
		}
		return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Table(constants.TemplateDefinitionTableName).
				Where("template_id = ?", snapshotID).
				Updates(map[string]any{
					"status":     StatusDeleting,
					"updated_at": gorm.Expr("CURRENT_TIMESTAMP"),
				}).Error; err != nil {
				return err
			}
			return tx.Table(constants.TemplateImageJobTableName).Create(record).Error
		})
	}); err != nil {
		return nil, err
	}
	info, err := GetTemplateImageJobInfo(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if reusedExistingJob {
		return resumeSnapshotDeleteJob(ctx, info, existingRequest)
	}
	return executeSnapshotDeleteJob(ctx, info, snapshotID)
}

func runSnapshotDeleteJob(ctx context.Context, jobID, snapshotID string) error {
	success := false
	defer func() {
		recordSnapshotDeleteResult(success)
	}()
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    JobPhaseDeleting,
		"progress": 20,
	})
	targets, err := discoverTemplateCleanupTargets(ctx, snapshotID, "")
	if err != nil {
		return failSnapshotDeleteJob(ctx, jobID, snapshotID, err)
	}
	locators, err := snapshotDeleteLocators(targets)
	if err != nil {
		return failSnapshotDeleteJob(ctx, jobID, snapshotID, err)
	}
	if err := runReplicaCleanup(ctx, snapshotID, locators); err != nil {
		return failSnapshotDeleteJob(ctx, jobID, snapshotID, err)
	}
	if err := deleteSnapshotMetadataOnly(ctx, snapshotID); err != nil {
		return failSnapshotDeleteJob(ctx, jobID, snapshotID, err)
	}
	invalidateTemplateCaches(snapshotID)
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":      JobStatusReady,
		"phase":       JobPhaseReady,
		"progress":    100,
		"result_json": `{"deleted":true}`,
	}); err != nil {
		return err
	}
	success = true
	return nil
}

func failSnapshotCreateJob(ctx context.Context, jobID, snapshotID, nodeIP, snapshotPath string, commitRsp *cubeboxv1.CommitSandboxResponse, cause error) error {
	// v4+: master no longer sends SnapshotPath/Objects to cubelet. The
	// catalog entry written during CommitSandbox carries everything cubelet
	// needs to clean up; the snapshotPath / commitRsp arguments are retained
	// in this signature solely for logging/diagnostic continuity.
	_ = snapshotPath
	_ = commitRsp
	if strings.TrimSpace(nodeIP) != "" && strings.TrimSpace(snapshotID) != "" {
		_, _ = cubelet.CleanupTemplate(ctx, cubelet.GetCubeletAddr(nodeIP), &cubeboxv1.CleanupTemplateRequest{
			RequestID:  uuid.NewString(),
			TemplateID: snapshotID,
		})
	}
	_ = deleteReplicasByTemplateID(ctx, snapshotID)
	defErr := updateDefinitionFields(ctx, snapshotID, map[string]any{
		"status":     StatusFailed,
		"last_error": cause.Error(),
	})
	jobErr := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":          JobStatusFailed,
		"phase":           JobPhaseRegistering,
		"progress":        100,
		"template_status": StatusFailed,
		"error_message":   cause.Error(),
	})
	invalidateTemplateCaches(snapshotID)
	return errors.Join(defErr, jobErr)
}

func failSnapshotDeleteJob(ctx context.Context, jobID, snapshotID string, cause error) error {
	defErr := updateDefinitionFields(ctx, snapshotID, map[string]any{
		"status":     StatusFailed,
		"last_error": cause.Error(),
	})
	jobErr := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":        JobStatusFailed,
		"phase":         JobPhaseDeleting,
		"progress":      100,
		"error_message": cause.Error(),
	})
	return errors.Join(defErr, jobErr)
}

func failSnapshotRollbackJob(ctx context.Context, jobID, phase string, resultPayload []byte, cause error) error {
	fields := map[string]any{
		"status":        JobStatusFailed,
		"phase":         phase,
		"progress":      100,
		"error_message": cause.Error(),
	}
	if len(resultPayload) > 0 {
		fields["result_json"] = string(resultPayload)
	}
	return updateTemplateImageJob(ctx, jobID, fields)
}

func buildSnapshotRequests(req *sandboxtypes.CreateCubeSandboxReq, snapshotID string) (*sandboxtypes.CreateCubeSandboxReq, *sandboxtypes.CreateCubeSandboxReq, error) {
	createReq, err := cloneCreateRequest(req)
	if err != nil {
		return nil, nil, err
	}
	if createReq.Request == nil || strings.TrimSpace(createReq.RequestID) == "" {
		return nil, nil, errors.New("requestID is required")
	}
	if createReq.Annotations == nil {
		createReq.Annotations = make(map[string]string)
	}
	if createReq.InstanceType == "" {
		createReq.InstanceType = cubeboxv1.InstanceType_cubebox.String()
	}
	createReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = snapshotID
	if constants.GetAppSnapshotVersion(createReq.Annotations) == "" {
		constants.SetAppSnapshotVersion(createReq.Annotations, DefaultTemplateVersion)
	}
	storedReq, err := normalizeStoredTemplateRequest(createReq)
	if err != nil {
		return nil, nil, err
	}
	return createReq, storedReq, nil
}

func marshalSnapshotCreateRequest(payload snapshotCreateJobRequest) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func marshalSnapshotRollbackRequest(requestID, sandboxID, snapshotID, nodeID, nodeIP string, newGen uint32, desiredSize uint64) (string, error) {
	payload, err := json.Marshal(snapshotRollbackJobRequest{
		RequestID:   requestID,
		SandboxID:   sandboxID,
		SnapshotID:  snapshotID,
		NodeID:      nodeID,
		NodeIP:      nodeIP,
		NewGen:      newGen,
		DesiredSize: desiredSize,
	})
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func formatSnapshotRuntimeRefConsumers(refs []SnapshotRuntimeRefInfo) string {
	consumers := make([]string, 0, len(refs))
	for _, item := range refs {
		consumer := strings.TrimSpace(item.SandboxID)
		if node := firstNonEmpty(item.NodeID, item.NodeIP); node != "" {
			consumer = firstNonEmpty(consumer, "<unknown>") + "@" + node
		}
		consumers = append(consumers, firstNonEmpty(consumer, "<unknown>"))
	}
	return strings.Join(consumers, ", ")
}

func getSnapshotReadyReplica(ctx context.Context, snapshotID, preferredNodeID string) (ReplicaStatus, error) {
	replicas, err := ListReplicas(ctx, snapshotID)
	if err != nil {
		return ReplicaStatus{}, err
	}
	var firstReady *ReplicaStatus
	var firstErr error
	for _, item := range replicas {
		replica := replicaModelToStatus(item)
		if replica.Status != ReplicaStatusReady {
			continue
		}
		if preferredNodeID != "" && replica.NodeID != preferredNodeID {
			continue
		}
		if err := validateSnapshotReadyReplica(replica); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if preferredNodeID != "" {
			return replica, nil
		}
		if firstReady == nil {
			tmp := replica
			firstReady = &tmp
		}
	}
	if firstErr != nil {
		return ReplicaStatus{}, firstErr
	}
	if firstReady == nil {
		return ReplicaStatus{}, ErrTemplateHasNoReadyReplica
	}
	return *firstReady, nil
}

// validateSnapshotReadyReplica makes sure the replica row has enough
// control-plane state to address a node. Physical references (rootfs/memory
// vol, meta_dir) are no longer required: cubelet looks them up locally from
// the snapshot catalog at rollback/cleanup time. Legacy replica rows still
// carry those fields but we no longer consult them here.
func validateSnapshotReadyReplica(replica ReplicaStatus) error {
	if strings.TrimSpace(replica.NodeID) == "" && strings.TrimSpace(replica.NodeIP) == "" {
		return fmt.Errorf("%w: snapshot replica has no node identity", ErrSnapshotReplicaMetadataIncomplete)
	}
	return nil
}

// snapshotDeleteLocators builds the cleanup locators for snapshot deletion.
// v4: master sends only (nodeID, nodeIP) to cubelet's CleanupTemplate;
// cubelet resolves SnapshotPath + Objects from its local catalog (with
// deterministic fallback). SnapshotPath was kept here in Phase 3 to make
// rolling upgrades smoother, but in the v4 atomic cutover it is no longer
// populated.
func snapshotDeleteLocators(targets *templateCleanupTargets) ([]templateCleanupLocator, error) {
	if targets == nil {
		return nil, nil
	}
	locators := make([]templateCleanupLocator, 0, len(targets.Replicas))
	for _, replica := range targets.Replicas {
		locators = append(locators, templateCleanupLocator{
			NodeID: replica.NodeID,
			NodeIP: replica.NodeIP,
		})
	}
	if len(locators) > 0 {
		return locators, nil
	}
	for _, locator := range targets.Locators {
		if strings.TrimSpace(locator.NodeID) != "" || strings.TrimSpace(locator.NodeIP) != "" {
			locators = append(locators, templateCleanupLocator{
				NodeID: locator.NodeID,
				NodeIP: locator.NodeIP,
			})
		}
	}
	return locators, nil
}

func nextSnapshotAttempt(ctx context.Context, snapshotID string) (int32, string, error) {
	latestJob, err := getLatestTemplateImageJobByTemplateID(ctx, snapshotID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 1, "", nil
		}
		return 0, "", err
	}
	attemptNo := nextAttemptNoFromLatest(latestJob.AttemptNo)
	return attemptNo, latestJob.JobID, nil
}

func allocateNextRollbackGen(ctx context.Context, sandboxID string) (uint32, error) {
	if ref, err := GetActiveSnapshotRuntimeRefBySandbox(ctx, sandboxID); err == nil && ref != nil {
		if ref.SandboxGen == 0 {
			return 1, nil
		}
		return ref.SandboxGen + 1, nil
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("sandbox_id = ? AND operation = ? AND status = ?", sandboxID, JobOperationSnapshotRollback, JobStatusReady).
		Order("id desc").First(record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 1, nil
		}
		return 0, err
	}
	if strings.TrimSpace(record.ResultJSON) == "" {
		return 1, nil
	}
	var result snapshotRollbackResult
	if err := json.Unmarshal([]byte(record.ResultJSON), &result); err != nil {
		return 0, err
	}
	if result.NewGen == 0 {
		return 1, nil
	}
	return result.NewGen + 1, nil
}

func updateDefinitionFields(ctx context.Context, templateID string, values map[string]any) error {
	values["updated_at"] = gorm.Expr("CURRENT_TIMESTAMP")
	return store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("template_id = ?", templateID).Updates(values).Error
}

func deleteReplicasByTemplateID(ctx context.Context, templateID string) error {
	return store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ?", templateID).Delete(&models.TemplateReplica{}).Error
}

func deleteSnapshotMetadataOnly(ctx context.Context, snapshotID string) error {
	return cleanupTemplateMetadata(ctx, snapshotID)
}

func createDefinitionTx(ctx context.Context, tx *gorm.DB, templateID string, storedReq *sandboxtypes.CreateCubeSandboxReq, instanceType, version string, opts definitionCreateOptions) error {
	payload, err := json.Marshal(storedReq)
	if err != nil {
		return err
	}
	kind := strings.TrimSpace(opts.Kind)
	if kind == "" {
		kind = TemplateKindTemplate
	}
	status := StatusPending
	if kind == TemplateKindSnapshot {
		status = StatusCreating
	}
	model := &models.TemplateDefinition{
		TemplateID:                templateID,
		InstanceType:              instanceType,
		Version:                   version,
		Status:                    status,
		Kind:                      kind,
		OriginSandboxID:           opts.OriginSandboxID,
		OriginNodeID:              opts.OriginNodeID,
		DisplayName:               opts.DisplayName,
		StorageBackend:            opts.StorageBackend,
		Retain:                    opts.Retain,
		RootfsSizeBytesAtSnapshot: opts.RootfsSizeBytesAtSnapshot,
		RequestJSON:               string(payload),
	}
	if kind == TemplateKindSnapshot && model.StorageBackend == "" {
		model.StorageBackend = StorageBackendCow
	}
	if err := tx.Table(constants.TemplateDefinitionTableName).Create(model).Error; err != nil {
		if strings.Contains(err.Error(), "1062") || strings.Contains(err.Error(), "Duplicate entry") {
			return ErrDuplicateTemplate
		}
		return err
	}
	return nil
}

func getSandboxData(ctx context.Context, sandboxID, instanceType string) (*sandboxtypes.SandboxData, error) {
	rsp := sandbox.SandboxInfo(ctx, &sandboxtypes.GetCubeSandboxReq{
		RequestID:    uuid.NewString(),
		SandboxID:    sandboxID,
		InstanceType: instanceType,
	})
	if rsp == nil || rsp.Ret == nil {
		return nil, errors.New("sandbox info returned empty response")
	}
	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		return nil, fmt.Errorf("get sandbox info failed: %s", rsp.Ret.RetMsg)
	}
	if len(rsp.Data) == 0 || rsp.Data[0] == nil {
		return nil, ErrTemplateNotFound
	}
	return rsp.Data[0], nil
}

func resolveSandboxDesiredSizeBytes(ctx context.Context, sandboxInfo *sandboxtypes.SandboxData) (uint64, error) {
	if sandboxInfo == nil {
		return 0, nil
	}
	if size := parseSystemDiskSizeBytesFromAnnotations(sandboxInfo.Annotations); size > 0 {
		return size, nil
	}
	templateID := strings.TrimSpace(sandboxInfo.TemplateID)
	if templateID == "" {
		return 0, nil
	}
	req, err := GetTemplateRequest(ctx, templateID)
	if err != nil {
		if errors.Is(err, ErrTemplateNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return parseSystemDiskSizeBytes(req), nil
}

func parseSystemDiskSizeBytes(req *sandboxtypes.CreateCubeSandboxReq) uint64 {
	if req == nil {
		return 0
	}
	return parseSystemDiskSizeBytesFromAnnotations(req.Annotations)
}

func parseSystemDiskSizeBytesFromAnnotations(annotations map[string]string) uint64 {
	if annotations == nil {
		return 0
	}
	raw := strings.TrimSpace(annotations[constants.CubeAnnotationsSystemDiskSize])
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value << 30
}

func generateSnapshotID() string {
	return snapshotIDPrefix + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
}

func isSnapshotDefinition(def *models.TemplateDefinition) bool {
	if def == nil {
		return false
	}
	kind := strings.TrimSpace(def.Kind)
	if kind == "" {
		return false
	}
	return strings.EqualFold(kind, TemplateKindSnapshot)
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func resolveExistingSnapshotJob(info *sandboxtypes.TemplateImageJobInfo) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	switch strings.ToUpper(strings.TrimSpace(info.Status)) {
	case JobStatusReady:
		return info, nil
	case JobStatusFailed:
		return nil, snapshotJobFailedError(info)
	case JobStatusPending, JobStatusRunning:
		return nil, fmt.Errorf("%w: request %s is still bound to snapshot operation %s", ErrTemplateAttemptInProgress, info.RequestID, info.JobID)
	default:
		return nil, fmt.Errorf("snapshot job %s ended in unexpected status %s", info.JobID, info.Status)
	}
}

func resumeSnapshotCreateJob(ctx context.Context, info *sandboxtypes.TemplateImageJobInfo, req *sandboxtypes.CreateCubeSandboxReq, sandboxID, nodeID, nodeIP string) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	if strings.ToUpper(strings.TrimSpace(info.Status)) != JobStatusPending {
		return resolveExistingSnapshotJob(info)
	}
	return executeSnapshotCreateJob(ctx, info, req, sandboxID, nodeID, nodeIP)
}

func executeSnapshotCreateJob(ctx context.Context, info *sandboxtypes.TemplateImageJobInfo, req *sandboxtypes.CreateCubeSandboxReq, sandboxID, nodeID, nodeIP string) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	if strings.ToUpper(strings.TrimSpace(info.Status)) != JobStatusPending {
		return finalizeSynchronousSnapshotJob(info)
	}
	claimed, err := claimSnapshotJobExecution(ctx, info.JobID, JobPhaseSnapshotting, 10)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return resolveExistingSnapshotJobByID(ctx, info.JobID)
	}
	jobCtx, cancel := synchronousSnapshotJobContext(ctx, "snapshot_create", map[string]any{
		"job_id":      info.JobID,
		"snapshot_id": info.TemplateID,
		"sandbox_id":  sandboxID,
		"node_id":     nodeID,
		"node_ip":     nodeIP,
	})
	defer cancel()
	createReq, storedReq, err := buildSnapshotRequests(req, info.TemplateID)
	if err != nil {
		return nil, err
	}
	if err := runSnapshotCreateJob(jobCtx, info.JobID, sandboxID, nodeID, nodeIP, createReq, storedReq); err != nil {
		return nil, err
	}
	return finalizeSnapshotJobByID(ctx, info.JobID)
}

func resumeSnapshotRollbackJob(ctx context.Context, info *sandboxtypes.TemplateImageJobInfo, payload snapshotRollbackJobRequest, replica ReplicaStatus) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	if strings.ToUpper(strings.TrimSpace(info.Status)) != JobStatusPending {
		return resolveExistingSnapshotJob(info)
	}
	return executeSnapshotRollbackJob(ctx, info, payload.SandboxID, payload.SnapshotID, payload.NodeID, payload.NodeIP, replica, payload.NewGen, payload.DesiredSize)
}

func executeSnapshotRollbackJob(ctx context.Context, info *sandboxtypes.TemplateImageJobInfo, sandboxID, snapshotID, nodeID, nodeIP string, replica ReplicaStatus, newGen uint32, desiredSize uint64) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	if strings.ToUpper(strings.TrimSpace(info.Status)) != JobStatusPending {
		return finalizeSynchronousSnapshotJob(info)
	}
	claimed, err := claimSnapshotJobExecution(ctx, info.JobID, JobPhaseRollbackDriving, 25)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return resolveExistingSnapshotJobByID(ctx, info.JobID)
	}
	jobCtx, cancel := synchronousSnapshotJobContext(ctx, "snapshot_rollback", map[string]any{
		"job_id":      info.JobID,
		"snapshot_id": snapshotID,
		"sandbox_id":  sandboxID,
		"node_id":     nodeID,
		"node_ip":     nodeIP,
	})
	defer cancel()
	if err := runSnapshotRollbackJob(jobCtx, info.JobID, sandboxID, snapshotID, nodeID, nodeIP, replica, newGen, desiredSize); err != nil {
		return nil, err
	}
	return finalizeSnapshotJobByID(ctx, info.JobID)
}

func resumeSnapshotDeleteJob(ctx context.Context, info *sandboxtypes.TemplateImageJobInfo, payload snapshotDeleteJobRequest) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	if strings.ToUpper(strings.TrimSpace(info.Status)) != JobStatusPending {
		return resolveExistingSnapshotJob(info)
	}
	return executeSnapshotDeleteJob(ctx, info, payload.SnapshotID)
}

func executeSnapshotDeleteJob(ctx context.Context, info *sandboxtypes.TemplateImageJobInfo, snapshotID string) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	if strings.ToUpper(strings.TrimSpace(info.Status)) != JobStatusPending {
		return finalizeSynchronousSnapshotJob(info)
	}
	claimed, err := claimSnapshotJobExecution(ctx, info.JobID, JobPhaseDeleting, 20)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return resolveExistingSnapshotJobByID(ctx, info.JobID)
	}
	jobCtx, cancel := synchronousSnapshotJobContext(ctx, "snapshot_delete", map[string]any{
		"job_id":      info.JobID,
		"snapshot_id": snapshotID,
	})
	defer cancel()
	if err := runSnapshotDeleteJob(jobCtx, info.JobID, snapshotID); err != nil {
		return nil, err
	}
	return finalizeSnapshotJobByID(ctx, info.JobID)
}

func resolveExistingSnapshotJobByID(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
	info, err := GetTemplateImageJobInfo(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return resolveExistingSnapshotJob(info)
}

func synchronousSnapshotJobContext(ctx context.Context, name string, fields map[string]any) (context.Context, context.CancelFunc) {
	return context.WithTimeout(detachTemplateImageJobContext(ctx, name, fields), snapshotOperationTimeout)
}

func finalizeSnapshotJobByID(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
	info, err := GetTemplateImageJobInfo(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return finalizeSynchronousSnapshotJob(info)
}

func finalizeSynchronousSnapshotJob(info *sandboxtypes.TemplateImageJobInfo) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	switch strings.ToUpper(strings.TrimSpace(info.Status)) {
	case JobStatusReady:
		return info, nil
	case JobStatusFailed:
		return nil, snapshotJobFailedError(info)
	default:
		return nil, fmt.Errorf("snapshot job %s did not reach terminal status synchronously: %s", info.JobID, info.Status)
	}
}

func claimSnapshotJobExecution(ctx context.Context, jobID, phase string, progress int32) (bool, error) {
	tx := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("job_id = ? AND status = ?", jobID, JobStatusPending).
		Updates(map[string]any{
			"status":     JobStatusRunning,
			"phase":      phase,
			"progress":   progress,
			"updated_at": gorm.Expr("CURRENT_TIMESTAMP"),
		})
	if tx.Error != nil {
		return false, tx.Error
	}
	return tx.RowsAffected > 0, nil
}

func snapshotJobFailedError(info *sandboxtypes.TemplateImageJobInfo) error {
	if info == nil {
		return errors.New("snapshot job failed")
	}
	message := strings.TrimSpace(info.ErrorMessage)
	if message == "" {
		message = fmt.Sprintf("snapshot operation %s failed", firstNonEmpty(info.Operation, info.JobID))
	}
	return errors.New(message)
}

// snapshotCreateRequestMatches verifies idempotency: a re-arrival of the same
// request_id must target the same sandbox/host/displayName and the same
// canonical spec fingerprint. Because the spec is now fetched from sandboxspec
// on every call instead of being carried in the payload, we compare via
// fingerprint rather than deep-equal.
func snapshotCreateRequestMatches(raw, requestID, sandboxID, nodeID, nodeIP, displayName string, currentSpec *sandboxtypes.CreateCubeSandboxReq) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	var existing snapshotCreateJobRequest
	if err := json.Unmarshal([]byte(raw), &existing); err != nil {
		return false
	}
	if existing.RequestID != requestID || existing.SandboxID != sandboxID || existing.NodeID != nodeID || existing.NodeIP != nodeIP || existing.DisplayName != displayName {
		return false
	}
	if existing.SpecFingerprint == "" {
		return true
	}
	return existing.SpecFingerprint == buildCommitTemplateSpecFingerprint(currentSpec)
}

func snapshotRollbackRequestMatches(raw, requestID, sandboxID, snapshotID string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	var existing snapshotRollbackJobRequest
	if err := json.Unmarshal([]byte(raw), &existing); err != nil {
		return false
	}
	return existing.RequestID == requestID && existing.SandboxID == sandboxID && existing.SnapshotID == snapshotID
}

func snapshotDeleteRequestMatches(raw, requestID, snapshotID string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	var existing snapshotDeleteJobRequest
	if err := json.Unmarshal([]byte(raw), &existing); err != nil {
		return false
	}
	return existing.RequestID == requestID && existing.SnapshotID == snapshotID
}
