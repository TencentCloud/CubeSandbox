// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
	"gorm.io/gorm"
)

func normalizeRedoTemplateImageRequest(req *types.RedoTemplateFromImageReq) (*types.RedoTemplateFromImageReq, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	if req.Request == nil || strings.TrimSpace(req.RequestID) == "" {
		return nil, errors.New("requestID is required")
	}
	if strings.TrimSpace(req.TemplateID) == "" {
		return nil, errors.New("template_id is required")
	}
	cloned := *req
	if len(req.DistributionScope) > 0 {
		cloned.DistributionScope = append([]string(nil), req.DistributionScope...)
	}
	return &cloned, nil
}

func allowRedoResumePhase(job *models.TemplateImageJob) error {
	if job == nil {
		return ErrTemplateNotFound
	}
	switch strings.ToUpper(strings.TrimSpace(job.Phase)) {
	case "", JobPhasePulling:
		return errors.New("template redo is not allowed before source image has been pulled successfully")
	default:
		return nil
	}
}

func determineRedoMode(req *types.RedoTemplateFromImageReq) string {
	switch {
	case req == nil:
		return RedoModeAll
	case req.FailedOnly && len(req.DistributionScope) > 0:
		return RedoModeFailedNodes
	case req.FailedOnly:
		return RedoModeFailedOnly
	case len(req.DistributionScope) > 0:
		return RedoModeNodes
	default:
		return RedoModeAll
	}
}

func replicaNeedsRedo(replica models.TemplateReplica) bool {
	return replica.Status != ReplicaStatusReady || replica.CleanupRequired
}

func failedRedoScope(replicas []models.TemplateReplica) []string {
	failedScope := make([]string, 0, len(replicas))
	for _, replica := range replicas {
		if !replicaNeedsRedo(replica) {
			continue
		}
		if replica.NodeID != "" {
			failedScope = append(failedScope, replica.NodeID)
			continue
		}
		if replica.NodeIP != "" {
			failedScope = append(failedScope, replica.NodeIP)
		}
	}
	return failedScope
}

func marshalRedoScope(scope []string) string {
	if len(scope) == 0 {
		return ""
	}
	payload, err := json.Marshal(scope)
	if err != nil {
		return ""
	}
	return string(payload)
}

func unmarshalRedoScope(scopeJSON string) []string {
	if strings.TrimSpace(scopeJSON) == "" {
		return nil
	}
	var scope []string
	if err := json.Unmarshal([]byte(scopeJSON), &scope); err != nil {
		return nil
	}
	return scope
}

func determineRedoResumePhase(job *models.TemplateImageJob, replicas []models.TemplateReplica) string {
	if job != nil {
		switch strings.ToUpper(job.Phase) {
		case JobPhasePulling, JobPhaseUnpacking, JobPhaseBuildingExt4, JobPhaseGeneratingJSON:
			return JobPhaseBuildingExt4
		case JobPhaseDistributing:
			return JobPhaseDistributing
		case JobPhaseCreatingTemplate, JobPhaseSnapshotting, JobPhaseRegistering:
			return JobPhaseSnapshotting
		}
	}
	for _, replica := range replicas {
		if replica.Status == ReplicaStatusReady {
			continue
		}
		switch strings.ToUpper(replica.LastErrorPhase) {
		case ReplicaPhaseDistributing:
			return JobPhaseDistributing
		case ReplicaPhaseSnapshotting, ReplicaPhaseFailed:
			return JobPhaseSnapshotting
		}
	}
	return JobPhaseSnapshotting
}

func resolveRedoTargets(instanceType string, req *types.RedoTemplateFromImageReq, replicas []models.TemplateReplica) ([]*node.Node, error) {
	if req == nil {
		return resolveTemplateNodes(instanceType, nil)
	}
	baseScope := req.DistributionScope
	if len(baseScope) == 0 {
		baseScope = nil
	}
	targets, err := resolveTemplateNodes(instanceType, baseScope)
	if err != nil {
		return nil, err
	}
	if !req.FailedOnly {
		return targets, nil
	}
	failedScope := failedRedoScope(replicas)
	if len(failedScope) == 0 {
		return nil, ErrNoFailedTemplateReplicas
	}
	failedSet := make(map[string]struct{}, len(failedScope))
	for _, item := range failedScope {
		if strings.TrimSpace(item) == "" {
			continue
		}
		failedSet[item] = struct{}{}
	}
	filtered := make([]*node.Node, 0, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		if _, ok := failedSet[target.ID()]; ok {
			filtered = append(filtered, target)
			continue
		}
		if _, ok := failedSet[target.HostIP()]; ok {
			filtered = append(filtered, target)
		}
	}
	if len(filtered) == 0 {
		return nil, ErrNoFailedTemplateReplicas
	}
	return filtered, nil
}

func failRedoTemplateImageJob(ctx context.Context, jobID, phase, message string) {
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":        JobStatusFailed,
		"phase":         phase,
		"progress":      100,
		"error_message": message,
	})
}

// prepareRootfsArtifactForRedoBuild removes leftovers from an interrupted
// BUILDING_EXT4 attempt while holding the same process-local lock used by
// ensureRootfsArtifact. If another caller completed a reusable artifact before
// this redo acquired the lock, keep that fresh artifact instead of deleting it.
func prepareRootfsArtifactForRedoBuild(ctx context.Context, artifactID string) (*models.RootfsArtifact, bool) {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return nil, false
	}
	muV, _ := artifactBuildLocks.LoadOrStore(artifactID, &sync.Mutex{})
	mu := muV.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	previousArtifact, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		return nil, false
	}
	if previousArtifact.Status == ArtifactStatusReady && previousArtifact.GeneratedRequestJSON != "" {
		if validationErr := validateReusableRootfsArtifactFile(previousArtifact); validationErr == nil {
			return previousArtifact, true
		}
	}
	if previousArtifact.Ext4Path != "" {
		_ = cleanupLocalRootfsArtifact(previousArtifact.ArtifactID, previousArtifact.Ext4Path)
	}
	_ = updateRootfsArtifact(ctx, previousArtifact.ArtifactID, map[string]any{
		"status":     ArtifactStatusFailed,
		"last_error": "redo requested after artifact build failure",
	})
	return nil, false
}

func runRedoTemplateImageJob(ctx context.Context, jobID string, req *types.RedoTemplateFromImageReq, downloadBaseURL string) {
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"template_id": req.TemplateID,
	})
	jobRecord, err := getTemplateImageJobRecordByID(ctx, jobID)
	if err != nil {
		logger.Errorf("lookup redo job fail: %v", err)
		return
	}
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    jobRecord.ResumePhase,
		"progress": 5,
	}); err != nil {
		logger.Errorf("update redo job start fail: %v", err)
		return
	}
	sourceReq, err := unmarshalTemplateImageJobRequest(jobRecord.RequestJSON)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, jobRecord.ResumePhase, err.Error())
		return
	}
	existingReplicas, err := ListReplicas(ctx, req.TemplateID)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, jobRecord.ResumePhase, err.Error())
		return
	}
	targets, err := resolveRedoTargets(sourceReq.InstanceType, req, existingReplicas)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, jobRecord.ResumePhase, err.Error())
		return
	}
	workingReq := newRedoWorkingRequest(sourceReq, req.TemplateID, targets)

	var artifact *models.RootfsArtifact
	resumePhase := jobRecord.ResumePhase
	if resumePhase == "" {
		resumePhase = JobPhaseSnapshotting
	}
	needsBuild := resumePhase == JobPhaseBuildingExt4
	cleanupPreviousBuild := needsBuild
	if !needsBuild {
		artifact, err = getRootfsArtifactByID(ctx, jobRecord.ArtifactID)
		if err != nil {
			failRedoTemplateImageJob(ctx, jobID, resumePhase, err.Error())
			return
		}
		muV, _ := artifactBuildLocks.LoadOrStore(artifact.ArtifactID, &sync.Mutex{})
		mu := muV.(*sync.Mutex)
		validationErr, reloadErr := func() (error, error) {
			mu.Lock()
			defer mu.Unlock()

			// The artifact may have been rebuilt while this redo waited for the
			// per-artifact lock. Reload the authoritative row so validation and
			// subsequent distribution use the current token, SHA, and metadata.
			refreshedArtifact, err := getRootfsArtifactByID(ctx, jobRecord.ArtifactID)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return err, nil
				}
				return nil, err
			}
			artifact = refreshedArtifact
			switch {
			case artifact.Status != ArtifactStatusReady:
				return fmt.Errorf("rootfs artifact %s status is %q, want %q", artifact.ArtifactID, artifact.Status, ArtifactStatusReady), nil
			case strings.TrimSpace(artifact.GeneratedRequestJSON) == "":
				return fmt.Errorf("rootfs artifact %s generated request is empty", artifact.ArtifactID), nil
			default:
				return validateReusableRootfsArtifactFile(artifact), nil
			}
		}()
		if reloadErr != nil {
			failRedoTemplateImageJob(ctx, jobID, resumePhase, fmt.Sprintf("reload rootfs artifact %s after acquiring build lock: %v", jobRecord.ArtifactID, reloadErr))
			return
		}
		if validationErr != nil {
			logger.Warnf("redo rootfs artifact %s is not reusable: %v; rebuilding", artifact.ArtifactID, validationErr)
			needsBuild = true
			resumePhase = JobPhaseBuildingExt4
			// ensureRootfsArtifact revalidates and claims the row under the same
			// artifact lock. Do not separately clean the invalid file here: a
			// concurrent create may replace it with a fresh artifact meanwhile.
			cleanupPreviousBuild = false
		}
	}
	if needsBuild {
		if ShouldInjectEnvdIntoTemplate(&workingReq) {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseBuildingExt4, "redo cannot rebuild envd-enabled template rootfs because the original envd payload is not persisted")
			return
		}
		if cleanupPreviousBuild {
			if reusableArtifact, reusable := prepareRootfsArtifactForRedoBuild(ctx, jobRecord.ArtifactID); reusable {
				artifact = reusableArtifact
				needsBuild = false
				resumePhase = JobPhaseDistributing
				if err := updateTemplateImageJob(ctx, jobID, map[string]any{
					"artifact_id":               artifact.ArtifactID,
					"template_spec_fingerprint": artifact.TemplateSpecFingerprint,
					"source_image_digest":       artifact.SourceImageDigest,
					"artifact_status":           artifact.Status,
					"phase":                     JobPhaseDistributing,
					"progress":                  60,
				}); err != nil {
					logger.Errorf("update redo reusable artifact fail: %v", err)
				}
			}
		}
	}
	if needsBuild {
		if err := image.EnsureArtifactBuildPreflight(ctx); err != nil {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseBuildingExt4, err.Error())
			return
		}
		source, prepErr := image.PrepareLocalSource(ctx, image.SourceSpec{ImageRef: workingReq.SourceImageRef, RegistryUsername: workingReq.RegistryUsername, RegistryPassword: workingReq.RegistryPassword, DownloadBaseURL: downloadBaseURL})
		if prepErr != nil {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseBuildingExt4, prepErr.Error())
			return
		}
		if source.Cleanup != nil {
			defer source.Cleanup(ctx)
		}
		artifact, _, _, err = ensureRootfsArtifact(ctx, &workingReq, source, downloadBaseURL, nil)
		if err != nil {
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":          JobStatusFailed,
				"phase":           JobPhaseBuildingExt4,
				"progress":        100,
				"artifact_status": ArtifactStatusFailed,
				"error_message":   err.Error(),
			})
			return
		}
		workingReq = newRedoWorkingRequest(sourceReq, req.TemplateID, targets)
		jobRecord.ArtifactID = artifact.ArtifactID
		if err := updateTemplateImageJob(ctx, jobID, map[string]any{
			"artifact_id":               artifact.ArtifactID,
			"template_spec_fingerprint": artifact.TemplateSpecFingerprint,
			"source_image_digest":       artifact.SourceImageDigest,
			"artifact_status":           artifact.Status,
			"phase":                     JobPhaseDistributing,
			"progress":                  60,
		}); err != nil {
			logger.Errorf("update redo rebuilt artifact fail: %v", err)
		}
		resumePhase = JobPhaseDistributing
	}

	var imageCfg image.DockerImageConfig
	if strings.TrimSpace(artifact.ImageConfigJSON) != "" {
		if err := json.Unmarshal([]byte(artifact.ImageConfigJSON), &imageCfg); err != nil {
			failRedoTemplateImageJob(ctx, jobID, resumePhase, fmt.Sprintf("decode artifact image config fail: %v", err))
			return
		}
	}
	generatedReq, err := generateTemplateCreateRequest(&workingReq, artifact, imageCfg, downloadBaseURL)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, resumePhase, err.Error())
		return
	}
	generatedTemplateID := ""
	if generatedReq.Annotations != nil {
		generatedTemplateID = strings.TrimSpace(generatedReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
	if generatedTemplateID != req.TemplateID {
		failRedoTemplateImageJob(ctx, jobID, resumePhase, fmt.Sprintf("generated template request id mismatch: got %q, want %q", generatedTemplateID, req.TemplateID))
		return
	}

	readyTargets := targets
	if resumePhase == JobPhaseDistributing {
		if err := cleanupArtifactOnNodes(ctx, artifact.ArtifactID, generatedReq.InstanceType, targets); err != nil {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseDistributing, fmt.Sprintf("cleanup artifact before redistribute failed: %v", err))
			return
		}
		distributedTargets, expected, ready, failed, distErr := distributeRootfsArtifact(ctx, &workingReq, generatedReq, artifact, req.TemplateID, jobID)
		if err := updateTemplateImageJob(ctx, jobID, map[string]any{
			"phase":               JobPhaseSnapshotting,
			"progress":            80,
			"expected_node_count": expected,
			"ready_node_count":    ready,
			"failed_node_count":   failed,
			"artifact_status":     artifact.Status,
			"error_message":       errorString(distErr),
		}); err != nil {
			logger.Errorf("update redo distribution status fail: %v", err)
		}
		if expected > 0 && ready == 0 {
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":        JobStatusFailed,
				"phase":         JobPhaseDistributing,
				"progress":      100,
				"error_message": fmt.Sprintf("artifact redistribution failed on all %d nodes: %v", expected, distErr),
			})
			return
		}
		readyTargets = distributedTargets
		resumePhase = JobPhaseSnapshotting
	}

	if err := cleanupTemplateReplicasOnNodes(ctx, req.TemplateID, existingReplicas, readyTargets); err != nil {
		failRedoTemplateImageJob(ctx, jobID, JobPhaseSnapshotting, fmt.Sprintf("cleanup template replicas before redo snapshot failed: %v", err))
		return
	}
	storedReq, err := normalizeStoredTemplateRequest(generatedReq)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, resumePhase, err.Error())
		return
	}
	if _, err := ensureTemplateDefinitionWithOptions(ctx, req.TemplateID, storedReq, generatedReq.InstanceType, constants.GetAppSnapshotVersion(generatedReq.Annotations), definitionCreateOptions{}); err != nil {
		failRedoTemplateImageJob(ctx, jobID, resumePhase, err.Error())
		return
	}
	if _, err := createTemplateReplicasOnNodes(ctx, req.TemplateID, generatedReq, readyTargets, replicaRunOptions{
		ArtifactID: artifact.ArtifactID,
		JobID:      jobID,
	}); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseSnapshotting,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	// refreshTemplateReplicaSummary claims the alias *before* publishing the
	// READY status so a client that observes the template as READY can always
	// resolve the alias (closes the redo/claim publish-ordering race).
	claimWarning, err := refreshTemplateReplicaSummary(ctx, req.TemplateID, jobID)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseSnapshotting,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	info, err := GetTemplateInfo(ctx, req.TemplateID)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseSnapshotting,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	finalStatus := JobStatusReady
	finalPhase := JobPhaseReady
	if info.Status == StatusFailed {
		finalStatus = JobStatusFailed
		finalPhase = JobPhaseSnapshotting
	}
	resultPayload, _ := json.Marshal(info)
	errorMessage := info.LastError
	if errorMessage == "" && claimWarning != "" {
		errorMessage = claimWarning
	}
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":          finalStatus,
		"phase":           finalPhase,
		"progress":        100,
		"artifact_id":     artifact.ArtifactID,
		"artifact_status": artifact.Status,
		"template_status": info.Status,
		"result_json":     string(resultPayload),
		"error_message":   errorMessage,
	})
}
