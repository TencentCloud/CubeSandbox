// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
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
// pushed it again — the only escape was a manual redo. The sweep closes the
// gap by re-pushing the artifact to in-scope nodes that lack a READY
// replica and converging the row. The sweep runs inside the image-job
// reconcile pass (advisory lock already held), so a multi-master deployment
// sweeps once per tick, not once per master.

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
	backfillPlacementScope = func(ctx context.Context, templateID string) ([]string, error) {
		records, err := listCreateRedoImageJobsByTemplateIDTx(store.db.WithContext(ctx), templateID)
		if err != nil {
			return nil, err
		}
		return placementScopeFromJobs(records)
	}
	backfillArtifact        = getRootfsArtifactByID
	backfillReplicas        = ListReplicas
	backfillResolveNodes    = resolveBackfillNodes
	backfillDistribute      = distributeRootfsArtifactToNodes
	backfillPlacements      = listArtifactNodePlacements
	backfillCreateReplica   = createReplicaOnNode
	backfillUpsertReplica   = UpsertReplica
	backfillGetDefinition   = GetDefinition
	backfillCleanupReplicas = cleanupTemplateReplicasOnNodes
)

// placementScopeFromJobs derives where a redone template belongs from its
// create/redo jobs, newest first: the latest create's scope plus the nodes of
// every redo after it. A create without a scope, or no create on record,
// means every in-scope node (nil).
func placementScopeFromJobs(records []models.TemplateImageJob) ([]string, error) {
	var redoScopes [][]string
	for _, record := range records {
		req, err := unmarshalTemplateImageJobRequest(record.RequestJSON)
		if err != nil {
			return nil, fmt.Errorf("decode %s job %s request: %w", strings.ToLower(record.Operation), record.JobID, err)
		}
		if !strings.EqualFold(record.Operation, JobOperationCreate) {
			redoScopes = append(redoScopes, req.DistributionScope)
			continue
		}
		if len(req.DistributionScope) == 0 {
			return nil, nil
		}
		seen := make(map[string]struct{})
		var scope []string
		add := func(items []string) {
			for _, item := range items {
				if item = strings.TrimSpace(item); item == "" {
					continue
				}
				if _, ok := seen[item]; ok {
					continue
				}
				seen[item] = struct{}{}
				scope = append(scope, item)
			}
		}
		add(req.DistributionScope)
		for i := len(redoScopes) - 1; i >= 0; i-- {
			add(redoScopes[i])
		}
		return scope, nil
	}
	return nil, nil
}

// resolveBackfillNodes is resolveTemplateNodes without the all-or-nothing
// scope check. A stored scope is a snapshot of the nodes that existed when
// the job ran; once one of them leaves or turns unhealthy, the strict check
// would fail the template on every tick and stop backfill to the nodes that
// are still in scope. Entries without a healthy match are skipped instead.
func resolveBackfillNodes(instanceType string, scope []string) ([]*node.Node, error) {
	nodes := healthyTemplateNodes(instanceType)
	if len(nodes) == 0 {
		return nil, ErrNoTemplateNodes
	}
	allowed := make(map[string]struct{}, len(scope))
	for _, item := range scope {
		if item = strings.TrimSpace(item); item != "" {
			allowed[item] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return nodes, nil
	}
	selected := make([]*node.Node, 0, len(nodes))
	for _, item := range nodes {
		if item == nil {
			continue
		}
		_, byID := allowed[item.ID()]
		_, byIP := allowed[item.HostIP()]
		if byID || byIP {
			selected = append(selected, item)
		}
	}
	if len(selected) == 0 {
		return nil, ErrNoTemplateNodes
	}
	return selected, nil
}

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

	// Decode like the redo/resume paths: the persisted snapshot has no
	// transport envelope, and generate below reads req.RequestID.
	req, err := unmarshalTemplateImageJobRequest(job.RequestJSON)
	if err != nil {
		return fmt.Errorf("decode job request snapshot: %w", err)
	}
	// A redo persists the nodes it rebuilt on as its scope. That is a
	// one-off target list, not the template's placement: keeping it alone
	// would stop every node that joins after the redo from being backfilled.
	// Dropping it would lose the nodes a redo moved the template to.
	if strings.EqualFold(job.Operation, JobOperationRedo) {
		scope, scopeErr := backfillPlacementScope(ctx, def.TemplateID)
		if scopeErr != nil {
			return fmt.Errorf("load placement scope: %w", scopeErr)
		}
		req.DistributionScope = scope
	}
	// jfs templates live on the shared volume: nodes never build their own
	// replica, so replica rows say nothing about ext4 coverage — only the
	// create-time placement rows do.
	if constants.IsJFSBackend(req.Backend) {
		if strings.TrimSpace(req.WritableLayerSize) == "" {
			req.WritableLayerSize = artifact.WritableLayerSize
		}
		instanceType := def.InstanceType
		if instanceType == "" {
			instanceType = req.InstanceType
		}
		targets, err := backfillResolveNodes(instanceType, req.DistributionScope)
		if err != nil {
			return err
		}
		placements, err := backfillPlacements(ctx, artifact.ArtifactID)
		if err != nil {
			return fmt.Errorf("list artifact placements: %w", err)
		}
		missing := nodesWithoutArtifactPlacement(placements, targets)
		if len(missing) == 0 {
			return nil
		}
		release := acquireResumeArtifactLock(artifact.ArtifactID)
		defer release()
		logger := log.G(ctx).WithFields(map[string]any{
			"component":   "image_job_reconcile",
			"template_id": def.TemplateID,
			"artifact_id": artifact.ArtifactID,
		})
		// trackReplicas=false: a placement push must not fabricate replica
		// rows on nodes that only hold the ext4 cache.
		missingTotal := len(missing)
		if len(missing) > backfillMaxTargetsPerSweep {
			// jfs pushes leave no row behind on failure, so there is no
			// FAILED-row signal to deprioritize a persistent failure block;
			// rotate the window every tick instead — every node is covered
			// within ceil(N/cap) sweeps.
			tick := int(time.Now().Unix() / int64(snapshotReconcilerInterval/time.Second))
			missing = backfillRotateWindow(missing, tick*backfillMaxTargetsPerSweep)
			missing = missing[:backfillMaxTargetsPerSweep]
		}
		_, _, ready, failed, distErr := backfillDistribute(ctx, req, nil, artifact, def.TemplateID, job.JobID, missing, false)
		logger.Infof("jfs artifact backfill: %d node(s) missing the ext4 artifact, distributed ready=%d failed=%d err=%v",
			missingTotal, ready, failed, distErr)
		return distErr
	}
	// The artifact row may be SHARED between templates (identical
	// fingerprint): GeneratedRequestJSON carries the FIRST builder's
	// template id, so the per-template request must be rebuilt from THIS
	// job's request or the node gets prepared for the wrong template.
	var imageCfg DockerImageConfig
	if strings.TrimSpace(artifact.ImageConfigJSON) != "" {
		imageCfg, err = decodeImageConfigJSON(artifact.ImageConfigJSON)
		if err != nil {
			return fmt.Errorf("decode artifact image config: %w", err)
		}
	}
	if strings.TrimSpace(req.WritableLayerSize) == "" {
		req.WritableLayerSize = artifact.WritableLayerSize
	}
	req.TemplateID = def.TemplateID
	generatedReq, err := generateTemplateCreateRequest(ctx, withResolvedTemplateUser(req, resolvedTemplateUserFromArtifact(artifact)), artifact, imageCfg, artifact.MasterNodeIP)
	if err != nil {
		return fmt.Errorf("generate template create request: %w", err)
	}
	generatedTemplateID := strings.TrimSpace(generatedReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	if generatedTemplateID != def.TemplateID {
		return fmt.Errorf("generated template request id mismatch: got %q, want %q", generatedTemplateID, def.TemplateID)
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

	// Best-effort dedup against the remote-build BUILT callback: same
	// in-process lock table, but a live redo takes no lock, so interleavings
	// are still possible and converge via AlreadyExists + FAILED rows.
	release := acquireResumeArtifactLock(artifact.ArtifactID)
	defer release()

	// The pre-lock listing can go stale while we wait: re-derive the work
	// set under the lock.
	replicas, err = backfillReplicas(ctx, def.TemplateID)
	if err != nil {
		return fmt.Errorf("list replicas after resume lock: %w", err)
	}
	missing = nodesWithoutReadyReplica(replicas, targets)
	if len(missing) == 0 {
		return nil
	}

	logger := log.G(ctx).WithFields(map[string]any{
		"component":   "image_job_reconcile",
		"template_id": def.TemplateID,
		"artifact_id": artifact.ArtifactID,
	})
	// A redo can switch the definition to a new artifact mid-pass; acting
	// from this stale view would delete the fresh package it just built.
	// Skip — the next tick loads the new artifact. (Narrows, not closes, the
	// window: the prepare-time recheck below still guards it.)
	definition, err := backfillGetDefinition(ctx, def.TemplateID)
	if err != nil {
		if errors.Is(err, ErrTemplateNotFound) {
			return nil
		}
		return fmt.Errorf("recheck definition before cleanup: %w", err)
	}
	if want := strings.TrimSpace(definition.RootfsArtifactID); want != "" && want != artifact.ArtifactID {
		logger.Infof("template replica backfill: definition switched artifact (%s -> %s), skipping this pass", artifact.ArtifactID, want)
		return nil
	}
	if len(missing) > backfillMaxTargetsPerSweep {
		missing = backfillPrioritizeFresh(replicas, missing)
		logger.Infof("template replica backfill: %d node(s) missing, capping this sweep at %d", len(missing), backfillMaxTargetsPerSweep)
		missing = missing[:backfillMaxTargetsPerSweep]
	}
	readyTargets, _, ready, failed, distErr := backfillDistribute(ctx, req, generatedReq, artifact, def.TemplateID, job.JobID, missing, true)
	logger.Infof("template replica backfill: %d node(s) missing a READY replica, distributed ready=%d failed=%d err=%v",
		len(missing), ready, failed, distErr)

	// Stale rows mean leftover CoW state that AppSnapshot refuses to
	// overwrite; clear them redo-style first. Distribute can take minutes
	// and a redo may converge rows meanwhile, so derive the set from a
	// fresh listing, not the pre-distribution view.
	replicas, err = backfillReplicas(ctx, def.TemplateID)
	if err != nil {
		logger.Warnf("template replica backfill: skip stale cleanup, list replicas fail, template=%s err=%v", def.TemplateID, err)
	} else {
		stale := make([]models.TemplateReplica, 0, len(replicas))
		for _, replica := range replicas {
			// In-flight rows (just distributed) are included: the node's
			// package may predate the row — a lost row does not remove the
			// package, and AppSnapshot refuses to overwrite it.
			if replicaNeedsRedo(replica) {
				stale = append(stale, replica)
			}
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(ctx, backfillCleanupTimeout)
		if cerr := backfillCleanupReplicas(cleanupCtx, def.TemplateID, stale, readyTargets, storageBackendFromCreate(generatedReq)); cerr != nil {
			logger.Warnf("template replica backfill: cleanup stale replicas fail, template=%s err=%v", def.TemplateID, cerr)
		}
		cleanupCancel()
		// A concurrent redo may have converged nodes while distribute
		// ran: skip preparing them instead of re-snapshotting into
		// AppSnapshot's AlreadyExists.
		readyTargets = nodesWithoutReadyReplica(replicas, readyTargets)
	}
	sem := make(chan struct{}, backfillPrepareConcurrency)
	var wg sync.WaitGroup
	var preparedMu sync.Mutex
	prepared := make([]ReplicaStatus, 0, len(readyTargets))
	for _, target := range readyTargets {
		target := target
		if target == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := ctx.Err(); err != nil {
				return
			}
			replica, persisted := backfillPrepareReplicaOnNode(ctx, def, instanceType, artifact, job, target, generatedReq)
			if persisted && replica.Status == ReplicaStatusReady {
				preparedMu.Lock()
				prepared = append(prepared, replica)
				preparedMu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Publish once for all rows: InvalidateImageState wipes the whole
	// template's registrations, so per-goroutine publish would stomp.
	if len(prepared) > 0 {
		invalidateTemplateCaches(def.TemplateID)
		// The re-register must cover the FULL READY set, not just the
		// backfilled subset, or previously READY nodes lose locality until
		// the next DB reload (same shape as refreshTemplateReplicaSummary).
		fresh, ferr := backfillReplicas(ctx, def.TemplateID)
		if ferr != nil {
			logger.Warnf("template replica backfill: re-register from fresh listing fail, template=%s err=%v", def.TemplateID, ferr)
			registerReadyTemplateReplicas(def.TemplateID, prepared)
		} else {
			current := make([]ReplicaStatus, 0, len(fresh))
			for _, r := range fresh {
				current = append(current, replicaModelToStatus(r))
			}
			registerReadyTemplateReplicas(def.TemplateID, current)
		}
	}
	return distErr
}

// backfillCleanupTimeout bounds one cleanup batch (one RPC per node,
// sequential) against a hung cubelet. shortcut: per-batch budget — a batch
// with more stale nodes than fit in 60s partially fails and is retried next
// tick; the real fix is a timeout on the CleanupTemplate RPC itself
// (cubelet/actions.go), which also covers the delete path.
const backfillCleanupTimeout = 60 * time.Second

// backfillPrepareConcurrency bounds parallel AppSnapshot prepares so a
// scale-out does not hold the cluster-wide reconcile lock serially.
const backfillPrepareConcurrency = 4

// backfillMaxTargetsPerSweep caps the nodes one sweep pushes + prepares: at
// minutes per node an unbounded sweep would hold the cluster-wide reconcile
// lock for hours. The remainder retries next tick.
const backfillMaxTargetsPerSweep = 64

// backfillRotateWindow left-rotates targets by offset so a capped sweep
// starts at a different position each tick.
func backfillRotateWindow(targets []*node.Node, offset int) []*node.Node {
	if len(targets) == 0 {
		return targets
	}
	offset %= len(targets)
	if offset < 0 {
		offset += len(targets)
	}
	return append(targets[offset:], targets[:offset]...)
}

// backfillPrioritizeFresh puts nodes with no replica row ahead of FAILED
// retries: the cap truncates the tail and retries re-enter missing every
// sweep, so a persistent failure block would otherwise starve the tail.
func backfillPrioritizeFresh(replicas []models.TemplateReplica, missing []*node.Node) []*node.Node {
	rowed := make(map[string]struct{}, len(replicas)*2)
	for _, r := range replicas {
		if id := strings.TrimSpace(r.NodeID); id != "" {
			rowed[id] = struct{}{}
		}
		if ip := strings.TrimSpace(r.NodeIP); ip != "" {
			rowed[ip] = struct{}{}
		}
	}
	fresh := make([]*node.Node, 0, len(missing))
	retries := make([]*node.Node, 0, len(missing))
	for _, t := range missing {
		if t == nil {
			continue
		}
		if _, ok := rowed[t.ID()]; ok {
			retries = append(retries, t)
			continue
		}
		if _, ok := rowed[t.HostIP()]; ok {
			retries = append(retries, t)
			continue
		}
		fresh = append(fresh, t)
	}
	return append(fresh, retries...)
}

// backfillPrepareReplicaOnNode prepares one target and persists the outcome
// (FAILED rows included — they are what the next sweep retries). The
// definition is re-checked before every persist: a DeleteTemplate
// mid-prepare must not be resurrected as a fresh READY row, and a redo that
// switched the artifact must not have its fresh row clobbered by our stale
// result.
func backfillPrepareReplicaOnNode(ctx context.Context, def backfillDefinition, instanceType string, artifact *models.RootfsArtifact, job *models.TemplateImageJob, target *node.Node, generatedReq *types.CreateCubeSandboxReq) (ReplicaStatus, bool) {
	logger := log.G(ctx).WithFields(map[string]any{
		"component":   "image_job_reconcile",
		"template_id": def.TemplateID,
		"artifact_id": artifact.ArtifactID,
	})
	replica, _ := backfillCreateReplica(ctx, target, def.TemplateID, generatedReq, replicaRunOptions{
		ArtifactID: artifact.ArtifactID,
		JobID:      job.JobID,
	})
	dropNodePackage := func() {
		dropCtx, cancel := context.WithTimeout(ctx, backfillCleanupTimeout)
		defer cancel()
		if cerr := backfillCleanupReplicas(dropCtx, def.TemplateID,
			[]models.TemplateReplica{{NodeID: replica.NodeID, NodeIP: replica.NodeIP}},
			[]*node.Node{target}, storageBackendFromCreate(generatedReq)); cerr != nil {
			logger.Warnf("template replica backfill: drop node package cleanup fail, node=%s err=%v", target.ID(), cerr)
		}
	}
	definition, err := backfillGetDefinition(ctx, def.TemplateID)
	if err != nil {
		if errors.Is(err, ErrTemplateNotFound) {
			// DeleteTemplate won the race: drop the just-created node
			// package and persist nothing, or it leaks on disk.
			if replica.Status == ReplicaStatusReady {
				dropNodePackage()
			}
			logger.Warnf("template replica backfill: definition gone mid-prepare, dropped node package, node=%s", target.ID())
		} else {
			logger.Warnf("template replica backfill: recheck definition before persist fail, node=%s err=%v", target.ID(), err)
		}
		return replica, false
	}
	if want := strings.TrimSpace(definition.RootfsArtifactID); want != "" && want != artifact.ArtifactID {
		// Redo switched the definition to a new artifact mid-prepare:
		// persist nothing, or our stale-artifact outcome would clobber
		// the redone row. Do NOT drop the node package — snapshots are
		// keyed by template id, so the cleanup could delete the fresh
		// package a concurrent redo just created on this node.
		logger.Warnf("template replica backfill: definition points at artifact %s now, dropped stale prepare outcome, node=%s", want, target.ID())
		return replica, false
	}
	if upsertErr := backfillUpsertReplica(ctx, def.TemplateID, instanceType, replica); upsertErr != nil {
		logger.Warnf("template replica backfill: persist prepare result fail, node=%s err=%v", target.ID(), upsertErr)
		return replica, false
	}
	if replica.Status != ReplicaStatusReady {
		logger.Warnf("template replica backfill: prepare replica fail, node=%s err=%s", target.ID(), replica.ErrorMessage)
		return replica, false
	}
	return replica, true
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

// nodesWithoutArtifactPlacement returns the in-scope targets without a
// recorded ext4 placement for the artifact. jfs uses this instead of
// nodesWithoutReadyReplica: replica rows mean "built the shared package"
// there, so coverage is tracked by the placement rows distribution writes.
func nodesWithoutArtifactPlacement(placements []models.ArtifactNodePlacement, targets []*node.Node) []*node.Node {
	have := make(map[string]struct{}, len(placements)*2)
	for _, placement := range placements {
		if id := strings.TrimSpace(placement.NodeID); id != "" {
			have[id] = struct{}{}
		}
		if ip := strings.TrimSpace(placement.NodeIP); ip != "" {
			have[ip] = struct{}{}
		}
	}
	missing := make([]*node.Node, 0, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		if _, ok := have[target.ID()]; ok {
			continue
		}
		if _, ok := have[target.HostIP()]; ok {
			continue
		}
		missing = append(missing, target)
	}
	return missing
}
