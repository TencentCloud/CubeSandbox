// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	// Load the CubeEgress CA fingerprint so the job's recorded
	// artifact_id matches what ensureRootfsArtifact will compute
	// downstream. We deliberately discard the PEM bytes here and
	// re-read inside ensureRootfsArtifact — small file, called once
	// per template build, simpler than threading the bytes through
	// runTemplateImageJob's existing structure. The early call
	// surfaces a missing/corrupt CA at JobPhasePulling instead of
	// halfway through ext4 build.
	withCubeCA := resolveWithCubeCA(req.WithCubeCA)
	_, caFingerprint, err := loadCubeEgressCA(ctx, withCubeCA)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhasePulling,
			"progress":      100,
			"error_message": err.Error(),
		})
		return
	}
	fingerprint := buildTemplateSpecFingerprintWithCA(req, source.Digest, caFingerprint)
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

func detachTemplateImageJobContext(ctx context.Context, name string, fields map[string]any) context.Context {
	detached := context.Background()
	rt := CubeLog.GetTraceInfo(ctx)
	if rt == nil {
		// Background workers (e.g. the snapshot reconciler) run without an
		// incoming request and therefore carry no trace. Synthesize one so the
		// detached context always has a usable trace and emits meaningful
		// metric labels. The caller names the operation explicitly so each
		// distinct job type gets its own trace label instead of sharing a
		// generic fallback.
		if strings.TrimSpace(name) == "" {
			name = "template_job"
		}
		rt = &CubeLog.RequestTrace{
			Action:         name,
			Caller:         name,
			Callee:         constants.CubeMasterServiceID,
			CalleeEndpoint: "localhost",
			Timestamp:      time.Now(),
		}
	} else {
		// Copy the inherited trace so mutations on the detached context do not
		// leak back into the originating request's trace.
		rt = rt.DeepCopy()
	}
	detached = CubeLog.WithRequestTrace(detached, rt)
	return log.WithLogger(detached, log.G(ctx).WithFields(fields))
}
