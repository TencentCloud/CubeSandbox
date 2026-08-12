// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"k8s.io/apimachinery/pkg/api/resource"
)

type snapshotImportJobRequest struct {
	RequestID        string `json:"request_id"`
	SnapshotID       string `json:"snapshot_id"`
	NodeID           string `json:"node_id"`
	NodeIP           string `json:"node_ip"`
	RootfsSourcePath string `json:"rootfs_source_path"`
	SpecFingerprint  string `json:"spec_fingerprint,omitempty"`
}

func ImportSnapshot(ctx context.Context, requestID, hostID, hostIP, rootfsSource string, createSpec *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, errors.New("requestID is required")
	}
	hostIP = strings.TrimSpace(hostIP)
	if hostIP == "" {
		return nil, errors.New("host_ip is required")
	}
	rootfsSource = strings.TrimSpace(rootfsSource)
	if rootfsSource == "" {
		return nil, errors.New("rootfs_source_path is required")
	}
	if createSpec == nil || len(createSpec.Containers) == 0 || createSpec.Containers[0] == nil {
		return nil, errors.New("create spec with at least one container is required")
	}
	if createSpec.Containers[0].Image == nil || strings.TrimSpace(createSpec.Containers[0].Image.Image) == "" {
		return nil, errors.New("create spec container image is required")
	}
	// The artifact carries a single overlay upper layer, so it can back exactly
	// one container's writable rootfs.
	if len(createSpec.Containers) > 1 {
		return nil, errors.New("snapshot import supports a single-container create spec")
	}
	// The first container sizes the restored VM, so a non-positive shape yields
	// a snapshot that cannot be restored. Cubelet applies the same rule when it
	// derives a spec dir, and keeping them in agreement also keeps the imported
	// catalog path consistent with the one a commit would produce.
	if createSpec.Containers[0].Resources == nil {
		return nil, errors.New("create spec first container resources are required")
	}
	for _, ctr := range createSpec.Containers {
		if ctr == nil || ctr.Resources == nil {
			continue
		}
		if cpu := strings.TrimSpace(ctr.Resources.Cpu); cpu != "" {
			if _, err := resource.ParseQuantity(cpu); err != nil {
				return nil, fmt.Errorf("invalid container cpu %q: %v", cpu, err)
			}
		}
		if mem := strings.TrimSpace(ctr.Resources.Mem); mem != "" {
			if _, err := resource.ParseQuantity(mem); err != nil {
				return nil, fmt.Errorf("invalid container mem %q: %v", mem, err)
			}
		}
	}
	if cpuCores, memMiB := firstContainerSpec(createSpec); cpuCores <= 0 || memMiB <= 0 {
		return nil, fmt.Errorf("create spec first container must request positive cpu and memory, got %dC%dM", cpuCores, memMiB)
	}
	// Cubelet's import path hardcodes the cubebox catalog layout, and the v2
	// (run-template) restore path cannot serve an imported snapshot.
	if it := strings.TrimSpace(createSpec.InstanceType); it != "" && it != cubeboxv1.InstanceType_cubebox.String() {
		return nil, fmt.Errorf("instance type %s does not support snapshot import", it)
	}

	node, ok := localcache.GetNodesByIp(hostIP)
	if !ok || node == nil {
		return nil, fmt.Errorf("host %s is not a registered node", hostIP)
	}
	nodeID := node.ID()
	nodeIP := node.IP
	if hostID = strings.TrimSpace(hostID); hostID != "" && hostID != nodeID {
		return nil, fmt.Errorf("host_id %s does not match node %s at %s", hostID, nodeID, hostIP)
	}

	if createSpec.Request == nil {
		createSpec.Request = &sandboxtypes.Request{RequestID: requestID}
	} else {
		createSpec.Request.RequestID = requestID
	}
	createReq, storedReq, err := buildSnapshotRequests(createSpec, "")
	if err != nil {
		return nil, err
	}

	var jobID string
	reusedExistingJob := false
	if err := withSnapshotWriteLocks([]string{
		snapshotRequestLockKey(requestID),
	}, func() error {
		if existing, err := getTemplateImageJobByRequestID(ctx, requestID); err == nil {
			if existing.Operation != JobOperationSnapshotImport {
				return fmt.Errorf("%w: request %s is already bound to %s", ErrTemplateAttemptInProgress, requestID, existing.Operation)
			}
			if !snapshotImportRequestMatches(existing.RequestJSON, requestID, nodeID, nodeIP, rootfsSource, storedReq) {
				return fmt.Errorf("%w: request %s payload does not match existing snapshot import job", ErrTemplateAttemptInProgress, requestID)
			}
			jobID = existing.JobID
			reusedExistingJob = true
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if err := ensureSnapshotNodeWritable(ctx, nodeID, nodeIP, false); err != nil {
			return err
		}

		fingerprint, err := importSpecFingerprint(storedReq)
		if err != nil {
			return err
		}
		snapshotID := generateSnapshotID()
		createReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = snapshotID
		storedReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = snapshotID
		requestJSON, err := json.Marshal(snapshotImportJobRequest{
			RequestID:        requestID,
			SnapshotID:       snapshotID,
			NodeID:           nodeID,
			NodeIP:           nodeIP,
			RootfsSourcePath: rootfsSource,
			SpecFingerprint:  fingerprint,
		})
		if err != nil {
			return err
		}
		jobID = uuid.NewString()
		record := &models.TemplateImageJob{
			JobID:                   jobID,
			TemplateID:              snapshotID,
			RequestID:               requestID,
			ResourceType:            JobResourceTypeSnapshot,
			ResourceID:              snapshotID,
			AttemptNo:               1,
			Operation:               JobOperationSnapshotImport,
			NodeID:                  nodeID,
			NodeIP:                  nodeIP,
			TemplateSpecFingerprint: fingerprint,
			InstanceType:            createReq.InstanceType,
			NetworkType:             createReq.NetworkType,
			Status:                  JobStatusPending,
			Phase:                   JobPhaseRegistering,
			Progress:                0,
			RequestJSON:             string(requestJSON),
			TemplateStatus:          StatusCreating,
		}
		defOpts := definitionCreateOptions{
			Kind:           TemplateKindSnapshot,
			OriginNodeID:   nodeID,
			StorageBackend: StorageBackendCow,
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
		return resumeSnapshotImportJob(ctx, info, createSpec, rootfsSource, nodeID, nodeIP)
	}
	return executeSnapshotImportJob(ctx, info, createSpec, rootfsSource, nodeID, nodeIP)
}

func resumeSnapshotImportJob(ctx context.Context, info *sandboxtypes.TemplateImageJobInfo, createSpec *sandboxtypes.CreateCubeSandboxReq, rootfsSource, nodeID, nodeIP string) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	if strings.ToUpper(strings.TrimSpace(info.Status)) != JobStatusPending {
		return resolveExistingSnapshotJob(info)
	}
	return executeSnapshotImportJob(ctx, info, createSpec, rootfsSource, nodeID, nodeIP)
}

func executeSnapshotImportJob(ctx context.Context, info *sandboxtypes.TemplateImageJobInfo, createSpec *sandboxtypes.CreateCubeSandboxReq, rootfsSource, nodeID, nodeIP string) (*sandboxtypes.TemplateImageJobInfo, error) {
	if info == nil {
		return nil, errors.New("snapshot job info is nil")
	}
	if strings.ToUpper(strings.TrimSpace(info.Status)) != JobStatusPending {
		return finalizeSynchronousSnapshotJob(info)
	}
	claimed, err := claimSnapshotJobExecution(ctx, info.JobID, JobPhaseRegistering, 10)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return resolveExistingSnapshotJobByID(ctx, info.JobID)
	}
	jobCtx, cancel := synchronousSnapshotJobContext(ctx, "snapshot_import", map[string]any{
		"job_id":      info.JobID,
		"snapshot_id": info.TemplateID,
		"node_id":     nodeID,
		"node_ip":     nodeIP,
	})
	defer cancel()
	createReq, storedReq, err := buildSnapshotRequests(createSpec, info.TemplateID)
	if err != nil {
		return nil, err
	}
	if err := runSnapshotImportJob(jobCtx, info.JobID, rootfsSource, nodeID, nodeIP, createReq, storedReq); err != nil {
		return nil, err
	}
	return finalizeSnapshotJobByID(ctx, info.JobID)
}

func runSnapshotImportJob(ctx context.Context, jobID, rootfsSource, nodeID, nodeIP string, createReq, storedReq *sandboxtypes.CreateCubeSandboxReq) error {
	snapshotID := strings.TrimSpace(createReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"snapshot_id": snapshotID,
		"node_id":     nodeID,
	})

	rsp, err := cubelet.ImportSnapshot(ctx, cubelet.GetCubeletAddr(nodeIP), &cubeboxv1.ImportSnapshotRequest{
		RequestID:        jobID,
		TemplateID:       snapshotID,
		RootfsSourcePath: rootfsSource,
		SpecDir:          computeSpecDir(createReq),
	})
	if err != nil {
		return failImportSnapshot(ctx, jobID, snapshotID, nodeIP, err)
	}
	if rsp.GetRet() == nil || int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		msg := "import snapshot failed"
		if rsp.GetRet() != nil && strings.TrimSpace(rsp.GetRet().GetRetMsg()) != "" {
			msg = rsp.GetRet().GetRetMsg()
		}
		return failImportSnapshot(ctx, jobID, snapshotID, nodeIP, errors.New(msg))
	}

	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":         JobPhaseRegistering,
		"progress":      70,
		"snapshot_path": rsp.GetSnapshotPath(),
	})

	replica := ReplicaStatus{
		NodeID:       nodeID,
		NodeIP:       nodeIP,
		InstanceType: createReq.InstanceType,
		Spec:         calculateRequestSpec(createReq),
		Status:       ReplicaStatusReady,
		Phase:        ReplicaPhaseReady,
		LastJobID:    jobID,
	}
	if err := UpsertReplica(ctx, snapshotID, createReq.InstanceType, replica); err != nil {
		return failImportSnapshot(ctx, jobID, snapshotID, nodeIP, err)
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
		"rootfs_size_bytes_at_snapshot": rsp.GetRootfsSizeBytes(),
	}); err != nil {
		return failImportSnapshot(ctx, jobID, snapshotID, nodeIP, err)
	}
	resultPayload, _ := json.Marshal(map[string]any{
		"snapshot_path":     rsp.GetSnapshotPath(),
		"rootfs_vol":        rsp.GetRootfsVol(),
		"rootfs_kind":       rsp.GetRootfsKind(),
		"rootfs_size_bytes": rsp.GetRootfsSizeBytes(),
		"storage_backend":   StorageBackendCow,
		"origin_node_id":    nodeID,
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
	logger.Infof("snapshot import finished successfully")
	return nil
}

func snapshotImportRequestMatches(raw, requestID, nodeID, nodeIP, rootfsSource string, currentSpec *sandboxtypes.CreateCubeSandboxReq) bool {
	var existing snapshotImportJobRequest
	if err := json.Unmarshal([]byte(raw), &existing); err != nil {
		return false
	}
	if existing.RequestID != requestID || existing.NodeID != nodeID || existing.NodeIP != nodeIP || existing.RootfsSourcePath != rootfsSource {
		return false
	}
	if existing.SpecFingerprint == "" {
		return false
	}
	current, err := importSpecFingerprint(currentSpec)
	if err != nil {
		return false
	}
	return existing.SpecFingerprint == current
}

// importSpecFingerprint hashes the stored spec with the template-id annotation
// stripped: NormalizeRequest stamps a generated id into it, so a retry could
// never reproduce the same value.
func importSpecFingerprint(spec *sandboxtypes.CreateCubeSandboxReq) (string, error) {
	cloned, err := cloneCreateRequest(spec)
	if err != nil {
		return "", err
	}
	delete(cloned.Annotations, constants.CubeAnnotationAppSnapshotTemplateID)
	return buildCommitTemplateSpecFingerprint(cloned), nil
}

func failImportSnapshot(ctx context.Context, jobID, snapshotID, nodeIP string, cause error) error {
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

// computeSpecDir mirrors cubelet's inferSnapshotResDirFromRequest, which reads
// the first container only. Import accepts a single-container spec, so the two
// agree by construction; reading the same container keeps them that way if the
// single-container restriction is ever lifted.
func computeSpecDir(req *sandboxtypes.CreateCubeSandboxReq) string {
	cpuCores, memMiB := firstContainerSpec(req)
	return fmt.Sprintf("%dC%dM", cpuCores, memMiB)
}

func firstContainerSpec(req *sandboxtypes.CreateCubeSandboxReq) (int64, int64) {
	var cpuMilli int64
	var memBytes int64
	if len(req.Containers) > 0 && req.Containers[0] != nil && req.Containers[0].Resources != nil {
		res := req.Containers[0].Resources
		if q, err := resource.ParseQuantity(strings.TrimSpace(res.Cpu)); err == nil {
			cpuMilli = q.MilliValue()
		}
		if q, err := resource.ParseQuantity(strings.TrimSpace(res.Mem)); err == nil {
			memBytes = q.Value()
		}
	}
	return int64(math.Ceil(float64(cpuMilli) / 1000)), memBytes / (1024 * 1024)
}
