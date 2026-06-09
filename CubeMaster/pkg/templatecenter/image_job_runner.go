// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func runTemplateImageJob(ctx context.Context, jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL string) {
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"template_id": req.TemplateID,
		"image":       req.SourceImageRef,
	})
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    JobPhasePulling,
		"progress": 5,
	}); err != nil {
		logger.Errorf("update job start fail: %v", err)
		return
	}
	if err := image.EnsureArtifactBuildPreflight(ctx); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhasePulling,
			"progress":      100,
			"error_message": err.Error(),
		})
		return
	}
	source, err := image.PrepareSource(ctx, image.SourceSpec{ImageRef: req.SourceImageRef, RegistryUsername: req.RegistryUsername, RegistryPassword: req.RegistryPassword, DownloadBaseURL: downloadBaseURL})
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhasePulling,
			"progress":      100,
			"error_message": err.Error(),
		})
		return
	}
	if source.Cleanup != nil {
		defer source.Cleanup(ctx)
	}
	fingerprint := buildTemplateSpecFingerprint(req, source.Digest)
	artifactID := buildArtifactID(fingerprint)
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"artifact_id":               artifactID,
		"template_spec_fingerprint": fingerprint,
		"source_image_digest":       source.Digest,
		"phase":                     JobPhaseUnpacking,
		"progress":                  20,
	}); err != nil {
		logger.Errorf("update job source metadata fail: %v", err)
	}
	artifact, generatedReq, builtFreshArtifact, err := ensureRootfsArtifact(ctx, req, source, downloadBaseURL)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":                    JobStatusFailed,
			"phase":                     JobPhaseBuildingExt4,
			"artifact_id":               artifactID,
			"template_spec_fingerprint": fingerprint,
			"artifact_status":           ArtifactStatusFailed,
			"error_message":             err.Error(),
			"progress":                  100,
		})
		return
	}
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"artifact_id":               artifact.ArtifactID,
		"template_spec_fingerprint": artifact.TemplateSpecFingerprint,
		"source_image_digest":       artifact.SourceImageDigest,
		"artifact_status":           artifact.Status,
		"phase":                     JobPhaseDistributing,
		"progress":                  70,
	}); err != nil {
		logger.Errorf("update job artifact fail: %v", err)
	}
	readyTargets, expected, ready, failed, distErr := distributeRootfsArtifact(ctx, req, generatedReq, artifact, req.TemplateID, jobID)
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":               JobPhaseCreatingTemplate,
		"progress":            85,
		"expected_node_count": expected,
		"ready_node_count":    ready,
		"failed_node_count":   failed,
		"error_message":       errorString(distErr),
	}); err != nil {
		logger.Errorf("update distribution status fail: %v", err)
	}
	if expected > 0 && ready == 0 {
		if builtFreshArtifact {
			if cleanupErr := cleanupFailedRootfsArtifact(ctx, artifact, req.InstanceType); cleanupErr != nil {
				logger.Errorf("cleanup fresh rootfs artifact after distribution failure fail: %v", cleanupErr)
			}
		}
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseDistributing,
			"progress":      100,
			"error_message": fmt.Sprintf("artifact distribution failed on all %d nodes: %v", expected, distErr),
		})
		return
	}
	var info *TemplateInfo
	storedReq, err := normalizeStoredTemplateRequest(generatedReq)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseCreatingTemplate,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	if _, err := ensureTemplateDefinition(ctx, req.TemplateID, storedReq, generatedReq.InstanceType, constants.GetAppSnapshotVersion(generatedReq.Annotations)); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseCreatingTemplate,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	replicas, persistErr := createTemplateReplicasOnNodes(ctx, req.TemplateID, generatedReq, readyTargets, replicaRunOptions{
		ArtifactID: artifact.ArtifactID,
		JobID:      jobID,
	})
	if persistErr != nil {
		err = persistErr
	} else {
		info, err = finalizeTemplateReplicas(ctx, req.TemplateID, generatedReq.InstanceType, constants.GetAppSnapshotVersion(generatedReq.Annotations), replicas)
	}
	if err != nil {
		if builtFreshArtifact {
			if cleanupErr := cleanupFailedRootfsArtifact(ctx, artifact, req.InstanceType); cleanupErr != nil {
				logger.Errorf("cleanup fresh rootfs artifact after create template error fail: %v", cleanupErr)
			}
		}
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseCreatingTemplate,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return
	}
	resultPayload, _ := json.Marshal(info)
	jobStatus := JobStatusReady
	jobPhase := JobPhaseReady
	if info.Status == StatusFailed {
		if builtFreshArtifact {
			if cleanupErr := cleanupFailedRootfsArtifact(ctx, artifact, req.InstanceType); cleanupErr != nil {
				logger.Errorf("cleanup fresh rootfs artifact after failed template status fail: %v", cleanupErr)
			}
		}
		jobStatus = JobStatusFailed
		jobPhase = JobPhaseCreatingTemplate
	}
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":          jobStatus,
		"phase":           jobPhase,
		"progress":        100,
		"template_status": info.Status,
		"result_json":     string(resultPayload),
		"error_message":   info.LastError,
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func detachTemplateImageJobContext(ctx context.Context, fields map[string]any) context.Context {
	detached := context.Background()
	if rt := CubeLog.GetTraceInfo(ctx); rt != nil {
		detached = CubeLog.WithRequestTrace(detached, rt.DeepCopy())
	}
	return log.WithLogger(detached, log.G(ctx).WithFields(fields))
}
