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

// SubmitTemplateFromImage persists the image_jobs record (PENDING) but does
// NOT start any in-process build. CubeMaster no longer builds templates
// locally; the caller (HTTP handler) forwards the job to CubeTemplateCenter,
// which builds the artifact and reports status back via the internal callback.
func SubmitTemplateFromImage(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*types.TemplateImageJobInfo, error) {
	return submitTemplateFromImage(ctx, req, downloadBaseURL, nil)
}

// SubmitTemplateFromImageWithoutBuild is the explicit remote-build entry point.
// Kept as a separate name so callers state the intent ("no local build") rather
// than relying on a flag.
func SubmitTemplateFromImageWithoutBuild(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*types.TemplateImageJobInfo, error) {
	return submitTemplateFromImage(ctx, req, downloadBaseURL, nil)
}

func submitTemplateFromImage(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string, envdPayload *EnvdInjectionPayload) (*types.TemplateImageJobInfo, error) {
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

	// Compute the spec fingerprint early so we can check for a READY artifact
	// before creating a new job. This deduplicates concurrent identical
	// requests at the Master layer: if an artifact for this exact spec already
	// exists and is READY, we skip the build entirely.
	fingerprint := BuildTemplateSpecFingerprintWithEnvdSHA(normalized, "", "", "")

	jobID := uuid.New().String()
	attemptNo := int32(1)
	retryOfJobID := ""
	reusedExistingJob := false
	reusedExistingArtifact := false
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

		// Check for an existing READY artifact with the same fingerprint.
		// If found, reuse it instead of creating a new build job.
		if artifact, err := getRootfsArtifactByFingerprint(ctx, fingerprint); err == nil {
			if artifact.Status == ArtifactStatusReady {
				// Verify the ext4 file still exists on disk.
				fileErr := validateReusableRootfsArtifactFile(artifact)
				if fileErr == nil {
					log.G(ctx).Infof("reusing existing READY artifact %s for template %s (fingerprint match)", artifact.ArtifactID, normalized.TemplateID)
					reusedExistingArtifact = true
					// Create a synthetic job that immediately goes to DISTRIBUTING phase
					// with the existing artifact, skipping the build.
					record := newReuseArtifactJobRecord(jobID, normalized, requestSnapshot, artifact)
					return store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).Create(record).Error
				}
				log.G(ctx).Warnf("artifact %s is READY but ext4 file is missing/invalid: %v; will rebuild", artifact.ArtifactID, fileErr)
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
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
	if reusedExistingArtifact {
		log.G(ctx).Infof("template %s reuses existing artifact, job %s starts at DISTRIBUTING phase", normalized.TemplateID, jobID)
		// The caller (HTTP handler) will see the job is already BUILT and
		// trigger the resume/distribution flow instead of forwarding to TC.
		return GetTemplateImageJobInfo(ctx, jobID)
	}
	if reusedExistingJob {
		return GetTemplateImageJobInfo(ctx, jobID)
	}
	// No local build goroutine: CubeMaster only persists the job. The HTTP
	// handler forwards it to CubeTemplateCenter, which builds and calls back.
	return GetTemplateImageJobInfo(ctx, jobID)
}

// newReuseArtifactJobRecord creates a job record that skips the build phase
// and starts directly at BUILT status with the given artifact. This is used
// when Master detects a READY artifact with matching fingerprint at submit
// time, avoiding a redundant TC build.
func newReuseArtifactJobRecord(jobID string, req *types.CreateTemplateFromImageReq, requestSnapshot string, artifact *models.RootfsArtifact) *models.TemplateImageJob {
	return &models.TemplateImageJob{
		JobID:                   jobID,
		TemplateID:              req.TemplateID,
		Status:                  JobStatusBuilt,
		Phase:                   JobPhaseReady,
		Progress:                100,
		Operation:               JobOperationCreate,
		AttemptNo:               1,
		RequestJSON:             requestSnapshot,
		ArtifactID:              artifact.ArtifactID,
		TemplateSpecFingerprint: artifact.TemplateSpecFingerprint,
		SourceImageDigest:       artifact.SourceImageDigest,
		ArtifactStatus:          artifact.Status,
		ResultJSON:              artifact.GeneratedRequestJSON,
	}
}

// RedoNeedsFullRebuild reports whether a redo job requires a full rootfs
// rebuild (true) or can reuse the existing artifact and only redistribute it
// (false). A rebuild is required when the artifact is missing, failed, or not
// READY; reuse is possible only when the artifact row exists and is READY.
// Exported for the HTTP handler to decide whether to forward a redo to TC.
func RedoNeedsFullRebuild(ctx context.Context, jobID string) bool {
	job, err := getTemplateImageJobRecordByID(ctx, jobID)
	if err != nil || job == nil {
		return true
	}
	if strings.TrimSpace(job.ArtifactID) == "" {
		return true
	}
	artifact, err := getRootfsArtifactByID(ctx, job.ArtifactID)
	if err != nil || artifact == nil {
		return true
	}
	return !artifactStatusReusableForRedo(artifact.Status)
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
	// Redo has two paths:
	//  1. Reuse artifact and redistribute only: no build needed, CubeMaster
	//     handles it locally (the artifact already exists).
	//  2. Full rebuild: the build is data-plane work owned by
	//     CubeTemplateCenter, same as create. The HTTP handler forwards the
	//     job to TC; CubeMaster only persists it here.
	if RedoNeedsFullRebuild(ctx, jobID) {
		// Full rebuild: leave the job PENDING for the HTTP handler to forward
		// to TC. No local build goroutine.
		return GetTemplateImageJobInfo(ctx, jobID)
	}
	// Redistribution-only: run the local redo pipeline (no build).
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

// GetRootfsArtifactForRedirect loads the artifact row and validates the
// download token, but does NOT open the ext4 file. Used by the download
// handler to decide whether to 302-redirect to the artifact's presigned S3
// URL (artifact_url non-empty) or fall through to the local-file stream
// (artifact_url empty, i.e. legacy/local-disk artifacts).
func GetRootfsArtifactForRedirect(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
	record, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	if record.DownloadToken != "" && token != record.DownloadToken {
		return nil, fmt.Errorf("invalid artifact token")
	}
	return record, nil
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
