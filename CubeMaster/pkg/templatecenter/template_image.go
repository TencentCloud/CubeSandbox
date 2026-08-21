// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"os"
	"strings"
)

var getTemplateImageJobPullProgress = localcache.GetTemplateImageJobPullProgress

func nextAttemptNoFromLatest(latestAttemptNo int32) int32 {
	if latestAttemptNo <= 0 {
		return 2
	}
	return latestAttemptNo + 1
}

func distributionScopeFromTargets(targets []*node.Node) []string {
	scope := make([]string, 0, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		scope = append(scope, target.ID())
	}
	return scope
}

func newRedoWorkingRequest(sourceReq *types.CreateTemplateFromImageReq, templateID string, targets []*node.Node) types.CreateTemplateFromImageReq {
	workingReq := *sourceReq
	workingReq.TemplateID = templateID
	workingReq.Request = &types.Request{RequestID: uuid.NewString()}
	workingReq.DistributionScope = distributionScopeFromTargets(targets)
	return workingReq
}

func newCreateTemplateImageJobRecord(jobID string, normalized *types.CreateTemplateFromImageReq, requestSnapshot string, attemptNo int32, retryOfJobID string) *models.TemplateImageJob {
	return &models.TemplateImageJob{
		JobID:             jobID,
		TemplateID:        normalized.TemplateID,
		RequestID:         normalized.RequestID,
		AttemptNo:         attemptNo,
		RetryOfJobID:      retryOfJobID,
		Operation:         JobOperationCreate,
		SourceImageRef:    normalized.SourceImageRef,
		WritableLayerSize: normalized.WritableLayerSize,
		InstanceType:      normalized.InstanceType,
		NetworkType:       normalized.NetworkType,
		Status:            JobStatusPending,
		Phase:             JobPhasePulling,
		Progress:          0,
		RequestJSON:       requestSnapshot,
	}
}

func newRedoTemplateImageJobRecord(jobID string, normalized *types.RedoTemplateFromImageReq, latestJob *models.TemplateImageJob, sourceReq *types.CreateTemplateFromImageReq, requestSnapshot string, attemptNo int32, targetScope []string, replicas []models.TemplateReplica) *models.TemplateImageJob {
	resumePhase := determineRedoResumePhase(latestJob, replicas)
	return &models.TemplateImageJob{
		JobID:             jobID,
		TemplateID:        normalized.TemplateID,
		RequestID:         normalized.RequestID,
		AttemptNo:         attemptNo,
		RetryOfJobID:      latestJob.JobID,
		Operation:         JobOperationRedo,
		RedoMode:          determineRedoMode(normalized),
		RedoScopeJSON:     marshalRedoScope(targetScope),
		ResumePhase:       resumePhase,
		ArtifactID:        latestJob.ArtifactID,
		SourceImageRef:    sourceReq.SourceImageRef,
		WritableLayerSize: sourceReq.WritableLayerSize,
		InstanceType:      sourceReq.InstanceType,
		NetworkType:       sourceReq.NetworkType,
		Status:            JobStatusPending,
		Phase:             resumePhase,
		Progress:          0,
		RequestJSON:       requestSnapshot,
	}
}

func SubmitTemplateFromImage(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*types.TemplateImageJobInfo, error) {
	return SubmitTemplateFromImageWithEnvdPayload(ctx, req, downloadBaseURL, nil)
}

func SubmitTemplateFromImageWithEnvdPayload(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string, envdPayload *EnvdInjectionPayload) (*types.TemplateImageJobInfo, error) {
	return submitTemplateFromImage(ctx, req, downloadBaseURL, envdPayload, true)
}

// SubmitTemplateFromImageWithoutBuild validates the request and persists the
// image_jobs record (PENDING) but does NOT start the in-process build
// goroutine. It is the entry point of the remote build mode: the caller
// (HTTP handler) forwards the job to CubeTemplateCenter, which builds the
// artifact and reports status back via the internal callback.
func SubmitTemplateFromImageWithoutBuild(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*types.TemplateImageJobInfo, error) {
	return submitTemplateFromImage(ctx, req, downloadBaseURL, nil, false)
}

func submitTemplateFromImage(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string, envdPayload *EnvdInjectionPayload, startBuild bool) (*types.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	normalized, err := normalizeTemplateImageRequest(req)
	if err != nil {
		return nil, err
	}
	log.G(ctx).Infof(
		"SubmitTemplateFromImage: template_id=%s image=%s network_type=%s cube_network_config=%s",
		normalized.TemplateID,
		normalized.SourceImageRef,
		normalized.NetworkType,
		formatTemplateImageCubeNetworkConfig(normalized.CubeNetworkConfig),
	)
	requestSnapshot, err := marshalTemplateImageJobRequest(normalized)
	if err != nil {
		return nil, err
	}
	jobID := uuid.New().String()
	attemptNo := int32(1)
	retryOfJobID := ""
	reusedExistingJob := false
	if err := withTemplateWriteLock(normalized.TemplateID, func() error {
		definitionFailed := false
		if def, err := GetDefinition(ctx, normalized.TemplateID); err == nil {
			if strings.EqualFold(def.Status, StatusFailed) {
				definitionFailed = true
			} else {
				return fmt.Errorf("template %s already exists; rootfs template specs are immutable, use a new template id to change writable layer size or rootfs settings", normalized.TemplateID)
			}
		} else if !errors.Is(err, ErrTemplateNotFound) {
			return err
		}

		if job, err := getActiveTemplateImageJobByTemplateID(ctx, normalized.TemplateID); err == nil {
			if job.RequestJSON == requestSnapshot {
				jobID = job.JobID
				reusedExistingJob = true
				return nil
			}
			return fmt.Errorf("%w: template %s is currently %s (job_id=%s)", ErrTemplateAttemptInProgress, normalized.TemplateID, strings.ToLower(job.Status), job.JobID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var latestJob *models.TemplateImageJob
		if job, err := getLatestTemplateImageJobByTemplateID(ctx, normalized.TemplateID); err == nil {
			latestJob = job
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if definitionFailed {
			if err := cleanupTemplateReplicas(ctx, normalized.TemplateID); err != nil {
				return err
			}
			if err := cleanupTemplateMetadata(ctx, normalized.TemplateID); err != nil {
				return err
			}
		}

		if latestJob != nil {
			attemptNo = nextAttemptNoFromLatest(latestJob.AttemptNo)
			retryOfJobID = latestJob.JobID
		}
		record := newCreateTemplateImageJobRecord(jobID, normalized, requestSnapshot, attemptNo, retryOfJobID)
		return store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).Create(record).Error
	}); err != nil {
		return nil, err
	}
	if reusedExistingJob {
		return GetTemplateImageJobInfo(ctx, jobID)
	}
	if startBuild {
		go runTemplateImageJob(detachTemplateImageJobContext(ctx, "template_image_create", map[string]any{
			"job_id":          jobID,
			"template_id":     normalized.TemplateID,
			"attempt_no":      attemptNo,
			"retry_of_job_id": retryOfJobID,
			"image":           normalized.SourceImageRef,
		}), jobID, normalized, downloadBaseURL, envdPayload)
	}
	return GetTemplateImageJobInfo(ctx, jobID)
}

func SubmitRedoTemplateFromImage(ctx context.Context, req *types.RedoTemplateFromImageReq, downloadBaseURL string) (*types.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	normalized, err := normalizeRedoTemplateImageRequest(req)
	if err != nil {
		return nil, err
	}
	jobID := uuid.NewString()
	var redoJob *models.TemplateImageJob
	if err := withTemplateWriteLock(normalized.TemplateID, func() error {
		return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if _, err := lockTemplateDefinitionTx(tx, normalized.TemplateID); err != nil {
				return err
			}
			if _, err := getActiveTemplateImageJobByTemplateIDTx(tx, normalized.TemplateID); err == nil {
				return fmt.Errorf("%w: template %s is currently running", ErrTemplateAttemptInProgress, normalized.TemplateID)
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			latestJob, err := getLatestTemplateImageJobByTemplateIDTx(tx, normalized.TemplateID)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrTemplateNotFound
				}
				return err
			}
			if err := allowRedoResumePhase(latestJob); err != nil {
				return err
			}
			sourceJob := latestJob
			if !isCreateRedoJobOperation(latestJob.Operation) {
				createRedoJob, lookupErr := getLatestCreateRedoImageJobByTemplateIDTx(tx, normalized.TemplateID)
				if lookupErr == nil {
					sourceJob = createRedoJob
				} else if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
					return lookupErr
				}
			}
			sourceReq, err := unmarshalTemplateImageJobRequest(sourceJob.RequestJSON)
			if err != nil {
				return fmt.Errorf("decode latest template image request fail: %w", err)
			}
			sourceReq.TemplateID = normalized.TemplateID
			if !isCreateRedoJobOperation(sourceJob.Operation) {
				sourceReq.Alias = ""
			}
			replicas, err := ListReplicas(ctx, normalized.TemplateID)
			if err != nil {
				return err
			}
			targetNodes, err := resolveRedoTargets(sourceReq.InstanceType, normalized, replicas)
			if err != nil {
				return err
			}
			targetScope := distributionScopeFromTargets(targetNodes)
			attemptNo := nextAttemptNoFromLatest(latestJob.AttemptNo)
			requestSnapshot, err := marshalTemplateImageJobRequest(sourceReq)
			if err != nil {
				return err
			}
			redoJob = newRedoTemplateImageJobRecord(jobID, normalized, latestJob, sourceReq, requestSnapshot, attemptNo, targetScope, replicas)
			return tx.Table(constants.TemplateImageJobTableName).Create(redoJob).Error
		})
	}); err != nil {
		return nil, err
	}
	go runRedoTemplateImageJob(detachTemplateImageJobContext(ctx, "template_image_redo", map[string]any{
		"job_id":      jobID,
		"template_id": normalized.TemplateID,
	}), jobID, normalized, downloadBaseURL)
	return GetTemplateImageJobInfo(ctx, jobID)
}

func GetTemplateImageJobInfo(ctx context.Context, jobID string) (*types.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	record := &models.TemplateImageJob{}
	if err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("job_id = ?", jobID).First(record).Error; err != nil {
		// Translate the driver-level miss into a domain error. Leaking
		// gorm.ErrRecordNotFound made every handler classify "this job does not
		// exist" as an internal error and answer 500 instead of NotFound.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: job_id=%s", ErrTemplateImageJobNotFound, jobID)
		}
		return nil, err
	}
	info, err := jobModelToInfo(ctx, record)
	if err != nil {
		return nil, err
	}
	overlayTemplateImageJobPullProgress(ctx, info)
	return info, nil
}

func overlayTemplateImageJobPullProgress(ctx context.Context, info *types.TemplateImageJobInfo) {
	if info == nil || info.JobID == "" || info.Status != JobStatusRunning {
		return
	}
	progress, ok := getTemplateImageJobPullProgress(ctx, info.JobID)
	if !ok || progress == nil {
		return
	}
	applyTemplateImageJobPullProgress(info, progress)
}

func applyTemplateImageJobPullProgress(info *types.TemplateImageJobInfo, progress *basetypes.TemplateImageJobPullProgressMap) {
	if info == nil || progress == nil {
		return
	}
	info.PullTotalBytes = progress.PullTotalBytes
	info.PullDownloadedBytes = progress.PullDownloadedBytes
	info.PullTotalLayers = progress.PullTotalLayers
	info.PullCompletedLayers = progress.PullCompletedLayers
	info.PullSpeedBPS = progress.PullSpeedBPS
}

func GetRootfsArtifactInfo(ctx context.Context, artifactID string) (*types.RootfsArtifactInfo, error) {
	record, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	return artifactModelToInfo(record), nil
}

func OpenRootfsArtifact(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, *os.File, error) {
	record, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		return nil, nil, err
	}
	if record.DownloadToken != "" && token != record.DownloadToken {
		return nil, nil, fmt.Errorf("invalid artifact token")
	}
	f, err := os.Open(record.Ext4Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// This is where the row/file drift is usually discovered: the row is
			// READY, distribution accepted it, and a cubelet is pulling right now.
			//
			// resolveMissingArtifact decides whether it is safe to demote. If this
			// node owns the artifact the row is demoted so the next create
			// rebuilds it, instead of every retry taking the reuse path and dying
			// on this same line forever (issue #852). If the artifact belongs to
			// another CubeMaster the row is left alone and the error says so:
			// the pull was routed to a node that never had the file (issue #1005),
			// and demoting here would destroy an artifact that is perfectly fine
			// elsewhere.
			if verdict := resolveMissingArtifact(ctx, record); verdict != artifactMissingVerdictNone {
				return nil, nil, fmt.Errorf("artifact source missing: %w", missingArtifactError(record, verdict))
			}
			return nil, nil, fmt.Errorf("artifact source missing: %w", err)
		}
		return nil, nil, err
	}
	return record, f, nil
}
