// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

const (
	JobPhaseSnapshotting = "SNAPSHOTTING"
	JobPhaseRegistering  = "REGISTERING"
)

func SubmitTemplateCommit(ctx context.Context, sandboxID, nodeID, nodeIP string, req *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	if req == nil || req.Request == nil || strings.TrimSpace(req.RequestID) == "" {
		return nil, errors.New("requestID is required for commit; retry should generate a new request id")
	}
	requestID := strings.TrimSpace(req.RequestID)
	createReq, templateID, err := NormalizeRequest(req)
	if err != nil {
		return nil, err
	}
	storedReq, err := normalizeStoredTemplateRequest(req)
	if err != nil {
		return nil, err
	}
	requestSnapshot, err := marshalTemplateCommitJobRequest(storedReq)
	if err != nil {
		return nil, err
	}
	jobID := uuid.New().String()
	attemptNo := int32(1)
	retryOfJobID := ""
	reusedExistingJob := false
	if err := withTemplateWriteLock(templateID, func() error {
		// Idempotency: the same (request_id, COMMIT) tuple uniquely identifies a
		// commit attempt. Reuse a prior job when payload matches; reject on drift.
		if job, err := getTemplateImageJobByRequestOperation(ctx, requestID, JobOperationCommit); err == nil {
			if job.RequestJSON == requestSnapshot {
				jobID = job.JobID
				reusedExistingJob = true
				return nil
			}
			return fmt.Errorf("%w: request_id=%s already used with a different commit payload (job_id=%s)", ErrTemplateAttemptInProgress, requestID, job.JobID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		definitionFailed := false
		if def, err := GetDefinition(ctx, templateID); err == nil {
			if strings.EqualFold(def.Status, StatusFailed) {
				definitionFailed = true
			} else {
				return fmt.Errorf("template %s already exists; committed template specs are immutable", templateID)
			}
		} else if !errors.Is(err, ErrTemplateNotFound) {
			return err
		}

		if job, err := getActiveTemplateImageJobByTemplateID(ctx, templateID); err == nil {
			if job.RequestJSON == requestSnapshot {
				jobID = job.JobID
				reusedExistingJob = true
				return nil
			}
			return fmt.Errorf("%w: template %s is currently %s (job_id=%s)", ErrTemplateAttemptInProgress, templateID, strings.ToLower(job.Status), job.JobID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var latestJob *models.TemplateImageJob
		if job, err := getLatestTemplateImageJobByTemplateID(ctx, templateID); err == nil {
			latestJob = job
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if definitionFailed {
			if err := cleanupTemplateReplicas(ctx, templateID); err != nil {
				return err
			}
			if err := cleanupTemplateMetadata(ctx, templateID); err != nil {
				return err
			}
		}

		if latestJob != nil {
			attemptNo = nextAttemptNoFromLatest(latestJob.AttemptNo)
			retryOfJobID = latestJob.JobID
		}
		record := newCommitTemplateImageJobRecord(jobID, requestID, templateID, nodeID, nodeIP, storedReq, createReq, requestSnapshot, attemptNo, retryOfJobID)
		if createErr := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).Create(record).Error; createErr != nil {
			// Concurrent writer may have inserted the same (request_id, COMMIT)
			// tuple between our lookup and create. Fall back to idempotent reuse.
			if isDuplicateKeyError(createErr) {
				if job, lookupErr := getTemplateImageJobByRequestOperation(ctx, requestID, JobOperationCommit); lookupErr == nil {
					if job.RequestJSON == requestSnapshot {
						jobID = job.JobID
						reusedExistingJob = true
						return nil
					}
					return fmt.Errorf("%w: request_id=%s already used with a different commit payload (job_id=%s)", ErrTemplateAttemptInProgress, requestID, job.JobID)
				}
			}
			return createErr
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if reusedExistingJob {
		return GetTemplateImageJobInfo(ctx, jobID)
	}

	runTemplateCommitJob(detachTemplateImageJobContext(ctx, map[string]any{
		"job_id":          jobID,
		"template_id":     templateID,
		"attempt_no":      attemptNo,
		"retry_of_job_id": retryOfJobID,
		"sandbox_id":      sandboxID,
		"node_id":         nodeID,
		"node_ip":         nodeIP,
	}), jobID, sandboxID, nodeID, nodeIP, createReq, storedReq)

	return GetTemplateImageJobInfo(ctx, jobID)
}

func runTemplateCommitJob(ctx context.Context, jobID, sandboxID, nodeID, nodeIP string, createReq, storedReq *sandboxtypes.CreateCubeSandboxReq) {
	templateID := createReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"template_id": templateID,
		"sandbox_id":  sandboxID,
		"node_id":     nodeID,
		"node_ip":     nodeIP,
	})
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    JobPhaseSnapshotting,
		"progress": 10,
	})

	commitRsp, err := cubelet.CommitSandbox(ctx, cubelet.GetCubeletAddr(nodeIP), &cubeboxv1.CommitSandboxRequest{
		RequestID:   uuid.NewString(),
		SandboxID:   sandboxID,
		TemplateID:  templateID,
		SnapshotDir: createReq.SnapshotDir,
	})
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseSnapshotting,
			"progress":      100,
			"error_message": err.Error(),
		})
		return
	}
	if commitRsp.GetRet() == nil || commitRsp.GetRet().GetRetCode() != 0 {
		msg := "commit sandbox failed"
		if commitRsp.GetRet() != nil {
			msg = commitRsp.GetRet().GetRetMsg()
		}
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseSnapshotting,
			"progress":      100,
			"error_message": msg,
		})
		return
	}

	snapshotPath := commitRsp.GetSnapshotPath()
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":         JobPhaseRegistering,
		"progress":      70,
		"node_id":       nodeID,
		"node_ip":       nodeIP,
		"snapshot_path": snapshotPath,
	})

	definitionCreated := false
	cleanupOnFailure := func(cause error) {
		if snapshotPath != "" {
			if _, cleanupErr := cubelet.CleanupTemplate(ctx, cubelet.GetCubeletAddr(nodeIP), &cubeboxv1.CleanupTemplateRequest{
				RequestID:    uuid.NewString(),
				TemplateID:   templateID,
				SnapshotPath: snapshotPath,
			}); cleanupErr != nil {
				cause = errors.Join(cause, cleanupErr)
			}
		}
		if definitionCreated {
			if cleanupErr := cleanupTemplateMetadata(ctx, templateID); cleanupErr != nil {
				cause = errors.Join(cause, cleanupErr)
			}
		}
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseRegistering,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   cause.Error(),
		})
		invalidateTemplateCaches(templateID)
	}

	if err := createDefinition(ctx, templateID, storedReq, createReq.InstanceType, constants.GetAppSnapshotVersion(createReq.Annotations)); err != nil {
		cleanupOnFailure(err)
		return
	}
	definitionCreated = true
	if cacheErr := setTemplateRequestCache(templateID, storedReq); cacheErr != nil {
		logger.Warnf("set template request cache fail:%v", cacheErr)
	}

	replica := ReplicaStatus{
		NodeID:       nodeID,
		NodeIP:       nodeIP,
		InstanceType: createReq.InstanceType,
		Spec:         calculateRequestSpec(createReq),
		SnapshotPath: snapshotPath,
		Status:       ReplicaStatusReady,
	}
	if err := UpsertReplica(ctx, templateID, createReq.InstanceType, replica); err != nil {
		cleanupOnFailure(err)
		return
	}
	setTemplateLocalityCache(templateID, []ReplicaStatus{replica})

	templateStatus := StatusReady
	expectedNodes := int32(localcache.GetHealthyNodesByInstanceType(-1, createReq.InstanceType).Len())
	if expectedNodes > 1 {
		templateStatus = StatusPartiallyReady
	}
	if err := UpdateDefinitionStatus(ctx, templateID, templateStatus, ""); err != nil {
		cleanupOnFailure(err)
		return
	}

	localcache.RegisterTemplateReplica(templateID, nodeID, 1)
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":              JobStatusReady,
		"phase":               JobPhaseRegistering,
		"progress":            100,
		"expected_node_count": expectedNodes,
		"ready_node_count":    1,
		"failed_node_count":   max(expectedNodes-1, 0),
		"template_status":     templateStatus,
		"error_message":       "",
	})
	logger.Infof("template commit job finished successfully")
}

func max(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func newCommitTemplateImageJobRecord(
	jobID, requestID, templateID, nodeID, nodeIP string,
	storedReq, createReq *sandboxtypes.CreateCubeSandboxReq,
	requestSnapshot string,
	attemptNo int32,
	retryOfJobID string,
) *models.TemplateImageJob {
	return &models.TemplateImageJob{
		JobID:                   jobID,
		TemplateID:              templateID,
		RequestID:               requestID,
		AttemptNo:               attemptNo,
		RetryOfJobID:            retryOfJobID,
		Operation:               JobOperationCommit,
		NodeID:                  nodeID,
		NodeIP:                  nodeIP,
		TemplateSpecFingerprint: buildCommitTemplateSpecFingerprint(storedReq),
		InstanceType:            createReq.InstanceType,
		NetworkType:             createReq.NetworkType,
		Status:                  JobStatusPending,
		Phase:                   JobPhaseSnapshotting,
		Progress:                0,
		RequestJSON:             requestSnapshot,
	}
}

func getTemplateImageJobByRequestOperation(ctx context.Context, requestID, operation string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("request_id = ? AND operation = ?", requestID, operation).
		Order("id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "1062")
}
