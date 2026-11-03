// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// Template replica backfill sweep.
//
// WHY THIS EXISTS
// ---------------
// Distribution only ever ran at create/redo time, against the healthy-node
// set of THAT moment. Anything that changes the node set afterwards leaves
// replicas stale:
//
//   - a NEW cubelet node joins (Kubernetes clusters scale out routinely);
//   - a NotReady node recovers;
//   - a node's distribution RPC failed during the original push and the job
//     still went READY on partial success (distributionFailure only fails
//     the job when ZERO nodes came up).
//
// In all three cases the node ends up schedulable for the template's
// instance type but without the template's rootfs artifact, and nothing
// pushed it again -- the only escape was a manual redo. The TC split did not
// change this: TC builds, Master distributes, and Master's distribution set
// was still point-in-time.
//
// The sweep closes the gap: for every READY template definition it computes
// (healthy nodes in scope) MINUS (nodes holding a READY replica) and pushes
// the artifact to exactly the difference. CreateImage is idempotent on the
// cubelet, the artifact URL is re-signed at push time, and the per-artifact
// resume lock serializes the sweep against live create/redo resumes, so a
// pass is safe to run every tick. It runs inside the image-job reconcile
// pass (advisory lock already held), so a multi-master deployment sweeps
// once per tick, not once per master.

// backfillDefinition is the definition projection the sweep needs.
type backfillDefinition struct {
	TemplateID       string `gorm:"column:template_id"`
	InstanceType     string `gorm:"column:instance_type"`
	RootfsArtifactID string `gorm:"column:rootfs_artifact_id"`
}

// Seams so the orchestration is testable without a database or live nodes.
var (
	listBackfillDefinitions = func(ctx context.Context, limit int) ([]backfillDefinition, error) {
		var defs []backfillDefinition
		err := store.db.WithContext(ctx).
			Table(constants.TemplateDefinitionTableName).
			Select("template_id, instance_type, rootfs_artifact_id").
			Where("status = ?", StatusReady).
			Limit(limit).
			Find(&defs).Error
		return defs, err
	}
	backfillLatestJob = func(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
		return getLatestCreateRedoImageJobByTemplateIDTx(store.db.WithContext(ctx), templateID)
	}
	backfillArtifact     = getRootfsArtifactByID
	backfillReplicas     = ListReplicas
	backfillResolveNodes = resolveTemplateNodes
	backfillDistribute   = distributeRootfsArtifactToNodes
)

// reconcileTemplateReplicaBackfill pushes READY templates to healthy nodes
// that lack a READY replica. Runs inside the image-job reconcile pass.
func reconcileTemplateReplicaBackfill(ctx context.Context) error {
	if !isReady() {
		return nil
	}
	defs, err := listBackfillDefinitions(ctx, imageJobReconcileBatch)
	if err != nil {
		return fmt.Errorf("scan ready definitions: %w", err)
	}
	logger := log.G(ctx).WithFields(map[string]any{"component": "image_job_reconcile"})
	for _, def := range defs {
		if err := backfillTemplateReplicas(ctx, def); err != nil {
			// Per-template failures must not stop the pass; the next tick
			// retries this template anyway.
			logger.Warnf("template replica backfill: template=%s err=%v", def.TemplateID, err)
		}
	}
	return nil
}

// backfillTemplateReplicas distributes one template's artifact to the
// healthy in-scope nodes that do not already hold a READY replica.
func backfillTemplateReplicas(ctx context.Context, def backfillDefinition) error {
	job, err := backfillLatestJob(ctx, def.TemplateID)
	if err != nil {
		// No create/redo job: the definition predates job bookkeeping or the
		// job rows were cleaned. Nothing to derive the original request from,
		// so there is no safe way to reconstruct the distribution input.
		return fmt.Errorf("load latest create/redo job: %w", err)
	}
	artifactID := strings.TrimSpace(def.RootfsArtifactID)
	if artifactID == "" {
		artifactID = strings.TrimSpace(job.ArtifactID)
	}
	if artifactID == "" {
		return fmt.Errorf("no artifact reference on definition or job")
	}
	artifact, err := backfillArtifact(ctx, artifactID)
	if err != nil {
		return fmt.Errorf("load artifact %s: %w", artifactID, err)
	}
	if err := ensureArtifactDistributable(ctx, artifact); err != nil {
		return err
	}

	req, err := decodeBackfillJobRequest(job.RequestJSON, artifact.WritableLayerSize)
	if err != nil {
		return fmt.Errorf("decode job request snapshot: %w", err)
	}
	if strings.TrimSpace(req.InstanceType) == "" {
		req.InstanceType = strings.TrimSpace(def.InstanceType)
	}
	if strings.TrimSpace(req.InstanceType) == "" {
		req.InstanceType = cubeboxv1.InstanceType_cubebox.String()
	}
	// The generated create request and the writable layer size are persisted
	// on the ARTIFACT row (finalizeRemoteArtifact), not on the job. The
	// artifact can be shared by multiple templates whose only difference is a
	// non-fingerprinted field (mergeable), so never reuse the artifact's
	// generated request verbatim: regenerate it from THIS job's request and
	// fall back to the stored snapshot on any regeneration failure.
	var generatedReq *types.CreateCubeSandboxReq
	imageCfg, cfgErr := decodeImageConfigJSON(artifact.ImageConfigJSON)
	if cfgErr == nil {
		generatedReq, err = generateTemplateCreateRequest(ctx, req, artifact, imageCfg, artifact.MasterNodeIP)
		if err != nil {
			log.G(ctx).Warnf(
				"template replica backfill: regenerate create request failed, falling back to the artifact snapshot: %v",
				err,
			)
			generatedReq = nil
			cfgErr = err
		}
	}
	if generatedReq == nil {
		if strings.TrimSpace(artifact.GeneratedRequestJSON) == "" {
			return fmt.Errorf("artifact %s has no usable generated request snapshot: %v", artifact.ArtifactID, cfgErr)
		}
		var stored types.CreateCubeSandboxReq
		if err := json.Unmarshal([]byte(artifact.GeneratedRequestJSON), &stored); err != nil {
			return fmt.Errorf("decode generated request snapshot: %w", err)
		}
		delete(stored.Annotations, constants.CubeAnnotationMemoryMergeable)
		for _, c := range stored.Containers {
			if c != nil {
				delete(c.Annotations, constants.CubeAnnotationMemoryMergeable)
			}
		}
		if req.Mergeable != nil && *req.Mergeable {
			if stored.Annotations == nil {
				stored.Annotations = map[string]string{}
			}
			stored.Annotations[constants.CubeAnnotationMemoryMergeable] = "true"
		}
		generatedReq = &stored
	}

	instanceType := def.InstanceType
	if instanceType == "" {
		instanceType = req.InstanceType
	}
	targets, err := backfillResolveNodes(instanceType, req.DistributionScope)
	if err != nil {
		return err
	}
	replicas, err := backfillReplicas(ctx, def.TemplateID)
	if err != nil {
		return fmt.Errorf("list replicas: %w", err)
	}
	missing := nodesWithoutReadyReplica(replicas, targets)
	if len(missing) == 0 {
		return nil
	}

	// Serialize against a live create/redo resume of the same artifact: the
	// callback goroutine and the stuck-BUILT replay both take the same
	// fingerprint-keyed lock.
	lockKey := strings.TrimSpace(artifact.TemplateSpecFingerprint)
	if lockKey == "" {
		lockKey = artifact.ArtifactID
	}
	release := acquireResumeArtifactLock(lockKey)
	defer release()

	logger := log.G(ctx).WithFields(map[string]any{
		"component":   "image_job_reconcile",
		"template_id": def.TemplateID,
		"artifact_id": artifact.ArtifactID,
	})
	_, _, ready, failed, distErr := backfillDistribute(ctx, req, generatedReq, artifact, def.TemplateID, job.JobID, missing)
	logger.Infof("template replica backfill: %d node(s) missing a READY replica, distributed ready=%d failed=%d err=%v",
		len(missing), ready, failed, distErr)
	return distErr
}

// decodeBackfillJobRequest rebuilds the Request envelope that
// marshalTemplateImageJobRequest deliberately strips (Request=nil in the job
// snapshot). Keeping the rest of the payload unnormalized lets older jobs
// that lack fields current normalization requires still be backfilled; the
// writable layer size is persisted on the artifact row, not the job.
//
// The regenerated request can differ from the artifact's persisted request
// for templates with a custom container user; backfill only uses the
// instance type and resource spec, so that difference is intentional.
func decodeBackfillJobRequest(payload, artifactWritableLayerSize string) (*types.CreateTemplateFromImageReq, error) {
	req := &types.CreateTemplateFromImageReq{}
	if err := json.Unmarshal([]byte(payload), req); err != nil {
		return nil, err
	}
	// A fresh request ID is safe here: backfillDistribute gates on existing
	// replica rows, not on request-id idempotency.
	req.Request = &types.Request{RequestID: uuid.NewString()}
	if strings.TrimSpace(req.WritableLayerSize) == "" {
		req.WritableLayerSize = artifactWritableLayerSize
	}
	return req, nil
}

// nodesWithoutReadyReplica returns the targets that do not hold a READY,
// clean replica for the template. A replica row needing redo (failed, or
// marked cleanup_required by an interrupted delete) counts as missing so the
// sweep also repairs half-cleaned nodes.
func nodesWithoutReadyReplica(replicas []models.TemplateReplica, targets []*node.Node) []*node.Node {
	ready := make(map[string]struct{}, len(replicas)*2)
	for _, replica := range replicas {
		if replicaNeedsRedo(replica) {
			continue
		}
		if id := strings.TrimSpace(replica.NodeID); id != "" {
			ready[id] = struct{}{}
		}
		if ip := strings.TrimSpace(replica.NodeIP); ip != "" {
			ready[ip] = struct{}{}
		}
	}
	missing := make([]*node.Node, 0, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		if _, ok := ready[target.ID()]; ok {
			continue
		}
		if _, ok := ready[target.HostIP()]; ok {
			continue
		}
		missing = append(missing, target)
	}
	return missing
}
