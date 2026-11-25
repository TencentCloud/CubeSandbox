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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/hostcompat"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

var ErrRedoTemplateScopeChanged = errors.New("redo target scope changed concurrently, please retry")

var (
	redoTemplateInUse  = isTemplateInUseFailClosed
	redoArtifactShared = func(ctx context.Context, artifactID, excludeTemplateID string) (bool, error) {
		if !isReady() {
			return false, ErrTemplateStoreNotInitialized
		}
		var refs int64
		err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var err error
			refs, err = countArtifactReferencesTx(ctx, tx, artifactID, excludeTemplateID)
			return err
		})
		return refs > 0, err
	}
)

// cleanupRedoArtifactBeforeRedistribute removes the ext4 artifact on target
// nodes before redistributing only when no other template, active job, or
// snapshot references the same artifact. For shared artifacts, it skips node
// deletion and relies on CreateImage being idempotent so peer templates are
// never broken on nodes without running sandboxes.
func cleanupRedoArtifactBeforeRedistribute(ctx context.Context, artifactID, instanceType, templateID string, targets []*node.Node) error {
	shared, err := redoArtifactShared(ctx, artifactID, templateID)
	if err != nil {
		return fmt.Errorf("check shared artifact references before cleanup failed: %w", err)
	}
	if shared {
		log.G(ctx).Warnf("redo template=%s artifact=%s: skipping node artifact cleanup because other templates or snapshots still reference it; existing on-node ext4 files will be kept as-is", templateID, artifactID)
		return nil
	}
	if err := cleanupArtifactOnNodes(ctx, artifactID, instanceType, targets); err != nil {
		return fmt.Errorf("cleanup artifact before redistribute failed: %w", err)
	}
	return nil
}

func jfsNodeVariantID(templateID string, target *node.Node) (string, bool) {
	if target == nil || target.HostFacts == nil {
		return "", false
	}
	variantID, _, ok := hostcompat.TemplateVariantID(
		templateID, target.HostFacts.CPUIDHash, target.HostFacts.HostKernelRelease,
	)
	return variantID, ok
}

// resolveRedoAffectedNodes resolves the nodes whose template state will be
// invalidated by a redo on targets. It returns scoped=false when the blast
// radius cannot be bounded to specific nodes and must be checked cluster-wide
// (full rootfs rebuild, or JFS when target HostFacts are unavailable).
func resolveRedoAffectedNodes(templateID, instanceType, backend string, needsFullRebuild bool, targets []*node.Node) ([]*node.Node, bool) {
	if needsFullRebuild {
		return nil, false
	}
	if !constants.IsJFSBackend(pinnedCleanupBackend(backend)) {
		return targets, true
	}
	if len(targets) == 0 {
		return nil, false
	}
	targetVariants := make(map[string]struct{}, len(targets))
	seenNodeIDs := make(map[string]struct{}, len(targets))
	affected := make([]*node.Node, 0, len(targets))
	appendUnique := func(n *node.Node) {
		if id := strings.TrimSpace(n.ID()); id != "" {
			if _, exists := seenNodeIDs[id]; exists {
				return
			}
			seenNodeIDs[id] = struct{}{}
		}
		affected = append(affected, n)
	}
	for _, target := range targets {
		variantID, ok := jfsNodeVariantID(templateID, target)
		if !ok {
			return nil, false
		}
		targetVariants[variantID] = struct{}{}
		appendUnique(target)
	}
	// Only peers sharing a target's JFS variant can read/execute the same
	// shared rootfs files; candidates without HostFacts have no computable
	// variant ID and cannot host that variant. (Target nodes themselves fail
	// closed above if any lacks HostFacts.)
	for _, candidate := range healthyTemplateNodes(instanceType) {
		variantID, ok := jfsNodeVariantID(templateID, candidate)
		if !ok {
			continue
		}
		if _, match := targetVariants[variantID]; !match {
			continue
		}
		appendUnique(candidate)
	}
	return affected, true
}

type redoNodeKey struct {
	id string
	ip string
}

type redoInUseScope struct {
	instanceType string
	clusterWide  bool
	nodes        map[redoNodeKey]struct{}
}

func newRedoInUseScope(templateID, instanceType, backend string, needsFullRebuild bool, targets []*node.Node) ([]*node.Node, redoInUseScope) {
	affectedNodes, scoped := resolveRedoAffectedNodes(templateID, instanceType, backend, needsFullRebuild, targets)
	scope := redoInUseScope{
		instanceType: instanceType,
		clusterWide:  !scoped,
	}
	if scoped {
		scope.nodes = make(map[redoNodeKey]struct{}, len(affectedNodes))
		for _, n := range affectedNodes {
			if n == nil {
				continue
			}
			key := redoNodeKey{
				id: strings.TrimSpace(n.ID()),
				ip: strings.TrimSpace(n.HostIP()),
			}
			if key != (redoNodeKey{}) {
				scope.nodes[key] = struct{}{}
			}
		}
	}
	return affectedNodes, scope
}

func (s redoInUseScope) covers(cur redoInUseScope) bool {
	if s.instanceType != cur.instanceType {
		return false
	}
	if s.clusterWide {
		return true
	}
	if cur.clusterWide {
		return false
	}
	for key := range cur.nodes {
		if _, ok := s.nodes[key]; !ok {
			return false
		}
	}
	return true
}

func checkRedoTemplateInUse(ctx context.Context, templateID, instanceType, backend string, needsFullRebuild bool, targets []*node.Node) (redoInUseScope, error) {
	affectedNodes, scope := newRedoInUseScope(templateID, instanceType, backend, needsFullRebuild, targets)
	var (
		inUse bool
		err   error
	)
	if !scope.clusterWide {
		inUse, err = isTemplateInUseOnNodes(ctx, templateID, instanceType, affectedNodes)
	} else {
		inUse, err = redoTemplateInUse(ctx, templateID, instanceType)
	}
	if err != nil {
		return redoInUseScope{}, fmt.Errorf("check template references before redo: %w", err)
	}
	if inUse {
		return redoInUseScope{}, ErrTemplateInUse
	}
	return scope, nil
}

func ensureRedoTemplateNotInUse(ctx context.Context, templateID, instanceType, backend string, needsFullRebuild bool, targets []*node.Node) error {
	_, err := checkRedoTemplateInUse(ctx, templateID, instanceType, backend, needsFullRebuild, targets)
	return err
}

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

func jfsRedoVariantIDs(templateID string, targets []*node.Node) []string {
	variantIDs := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		variantID, ok := jfsNodeVariantID(templateID, target)
		if !ok {
			continue
		}
		if _, exists := seen[variantID]; exists {
			continue
		}
		seen[variantID] = struct{}{}
		variantIDs = append(variantIDs, variantID)
	}
	return variantIDs
}

func markJFSRedoReplicaRowsUnavailable(ctx context.Context, templateID string, targets []*node.Node) error {
	variantIDs := jfsRedoVariantIDs(templateID, targets)
	if len(variantIDs) == 0 {
		return nil
	}
	return store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND template_variant_id IN ?", templateID, variantIDs).
		Updates(map[string]any{
			"status":        ReplicaStatusFailed,
			"phase":         ReplicaPhaseSnapshotting,
			"error_message": "JFS template variant redo in progress",
		}).Error
}

func deleteJFSRedoReplicaRows(ctx context.Context, templateID string, targets []*node.Node) error {
	variantIDs := jfsRedoVariantIDs(templateID, targets)
	if len(variantIDs) == 0 {
		return nil
	}
	return store.db.WithContext(ctx).Unscoped().Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND template_variant_id IN ?", templateID, variantIDs).
		Delete(&models.TemplateReplica{}).Error
}

func prepareRedoTemplateReplicas(ctx context.Context, templateID, instanceType string, existingReplicas []models.TemplateReplica, targets []*node.Node, backend string) error {
	if err := ensureRedoTemplateNotInUse(ctx, templateID, instanceType, backend, false, targets); err != nil {
		return err
	}
	backend = pinnedCleanupBackend(backend)
	if constants.IsJFSBackend(backend) {
		if err := markJFSRedoReplicaRowsUnavailable(ctx, templateID, targets); err != nil {
			return fmt.Errorf("mark old JFS template variant replicas unavailable before redo: %w", err)
		}
	}
	if err := cleanupTemplateReplicasOnNodes(ctx, templateID, existingReplicas, targets, backend); err != nil {
		return fmt.Errorf("cleanup template replicas before redo snapshot: %w", err)
	}
	if constants.IsJFSBackend(backend) {
		if err := deleteJFSRedoReplicaRows(ctx, templateID, targets); err != nil {
			return fmt.Errorf("delete old JFS template variant replicas before redo: %w", err)
		}
	}
	return nil
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
	failedVariants := make(map[string]struct{}, len(replicas))
	for _, replica := range replicas {
		if replicaNeedsRedo(replica) {
			if variantID := strings.TrimSpace(replica.TemplateVariantID); variantID != "" {
				failedVariants[variantID] = struct{}{}
			}
		}
	}
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
		if variantID, ok := jfsNodeVariantID(req.TemplateID, target); ok {
			if _, failed := failedVariants[variantID]; failed {
				filtered = append(filtered, target)
				continue
			}
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

// rootfsArtifactReusableForRedo reports whether a redo may skip the rebuild and
// reuse the artifact recorded on the previous job.
//
// This must be checked before honouring a resume phase of DISTRIBUTING or
// later, because the "distribution failed on every node" path deletes the
// artifact it just built (files, node copies and the DB row) on its way out.
// A job that failed that way records phase=DISTRIBUTING, so determining the
// resume phase from the phase alone sends every redo straight into
// getRootfsArtifactByID on an artifact that no longer exists — the redo fails
// with "record not found" and the template can never be retried at all.
func rootfsArtifactReusableForRedo(ctx context.Context, artifactID string) bool {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return false
	}
	artifact, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil || artifact == nil {
		// Includes gorm.ErrRecordNotFound, which is the exact state the
		// all-nodes-failed cleanup leaves behind.
		return false
	}
	if !artifactStatusReusableForRedo(artifact.Status) {
		return false
	}
	// A READY row still has to be backed by its ext4 file. When the artifact
	// store did not survive a restart the row outlives the file, and reusing it
	// here would make the redo re-distribute a phantom artifact — which is the
	// one thing a redo exists to fix. Demote it so this redo falls through to the
	// full-rebuild branch instead.
	return readyArtifactUsableForReuse(ctx, artifact)
}

// artifactStatusReusableForRedo is the status half of the decision above, kept
// separate from the DB lookup so the state matrix can be tested directly.
//
// Only READY means "the ext4 exists and is complete". Everything else must
// rebuild:
//   - PENDING/BUILDING belongs to an in-flight build owned by someone else;
//     reusing it would read a half-written file.
//   - FAILED/CLEANUP_PENDING/ORPHANED means the files are gone or going.
//   - an unknown status is treated as not reusable, so adding a state to the
//     schema can never silently turn into "reuse it".
func artifactStatusReusableForRedo(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), ArtifactStatusReady)
}

// artifactBuildLocks serializes concurrent builds of the same artifactID
// within this process. Kept here (not in artifact_build.go) because redo's
// artifact-reuse path also needs to serialize against last-owner-cleanup.
var artifactBuildLocks sync.Map // map[string]*sync.Mutex

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
	// Hard invariant: the decoded snapshot's template_id MUST equal the
	// job row's own template_id, otherwise downstream steps could register
	// artifacts/definitions under a different template_id than the one this
	// job (and its replicas) actually belong to, orphaning the real record.
	if decodedID := strings.TrimSpace(sourceReq.TemplateID); decodedID != strings.TrimSpace(jobRecord.TemplateID) {
		err := fmt.Errorf("decoded request template_id %q does not match job %s template_id %q; refusing to resume to avoid orphaning the template", decodedID, jobID, jobRecord.TemplateID)
		logger.Errorf("%v", err)
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
	// v2: downgrade to a full rebuild when the artifact this redo intended to
	// reuse is no longer usable. Without this, a job that failed distribution
	// on all nodes is unretryable forever: that path deletes the artifact, so
	// the reuse branch below can only ever report "record not found".
	if resumePhase != JobPhaseBuildingExt4 && !rootfsArtifactReusableForRedo(ctx, jobRecord.ArtifactID) {
		// But never rebuild an artifact another CubeMaster holds: the rebuild
		// would overwrite the shared row (fresh token/sha) and break every
		// replica the holder already served (issue #1005). Surface it instead.
		if artifact, aerr := getRootfsArtifactByID(ctx, jobRecord.ArtifactID); aerr == nil && artifact != nil {
			if reuseErr := rootfsArtifactReuseVerdict(ctx, artifact); errors.Is(reuseErr, ErrRootfsArtifactForeign) {
				failRedoTemplateImageJob(ctx, jobID, resumePhase,
					fmt.Sprintf("redo cannot rebuild artifact %s here: %v", artifact.ArtifactID, reuseErr))
				return
			}
		}
		logger.Infof("redo: artifact %q is not reusable, falling back to a full rebuild (resume_phase %s -> %s)",
			jobRecord.ArtifactID, resumePhase, JobPhaseBuildingExt4)
		resumePhase = JobPhaseBuildingExt4
		if err := updateTemplateImageJob(ctx, jobID, map[string]any{
			"phase":        JobPhaseBuildingExt4,
			"resume_phase": JobPhaseBuildingExt4,
		}); err != nil {
			logger.Warnf("update redo resume phase fail: %v", err)
		}
	}
	if resumePhase == JobPhaseBuildingExt4 {
		if ShouldInjectEnvdIntoTemplate(&workingReq) {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseBuildingExt4, "redo cannot rebuild envd-enabled template rootfs because the original envd payload is not persisted")
			return
		}
		// v2: snapshot templates have no source_image_ref, so there is nothing
		// to rebuild FROM (issue #1159). Checked here, before the destructive
		// cleanup in prepareRootfsArtifactForRedoBuild, for the same reason as
		// before: that path deletes the previous ext4 and marks the artifact
		// FAILED, so without this guard a snapshot-based template would have
		// its remaining artifact destroyed on the way to an inevitable failure.
		if strings.TrimSpace(workingReq.SourceImageRef) == "" {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseBuildingExt4,
				"redo cannot rebuild this template: it has no source_image_ref "+
					"(templates created from a sandbox snapshot cannot be rebuilt from an image); "+
					"the existing artifact was left untouched")
			return
		}
		if err := ensureRedoTemplateNotInUse(ctx, req.TemplateID, sourceReq.InstanceType, "" /* unused on full rebuild */, true, targets); err != nil {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseBuildingExt4, err.Error())
			return
		}
		// Try to reuse a still-valid prior artifact under the build lock; if
		// none is usable, fall through to the full rebuild below. Holding the
		// lock guarantees that a concurrent create cannot replace the artifact
		// between our validation and our distribution.
		if reusableArtifact, reusable := prepareRootfsArtifactForRedoBuild(ctx, jobRecord.ArtifactID); reusable {
			artifact = reusableArtifact
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
		} else {
			// Full rebuild path is disabled in CubeMaster. Redo jobs requiring a
			// full rebuild must be forwarded to CubeTemplateCenter by the caller
			// (template_from_image.go:72). Reaching here means the caller's
			// forwarding logic has a bug.
			failRedoTemplateImageJob(ctx, jobID, JobPhaseBuildingExt4,
				"redo requires a full rebuild but local ext4 build is disabled in CubeMaster; "+
					"this job should have been forwarded to CubeTemplateCenter")
			return
		}
	} else {
		// resumePhase is DISTRIBUTING or SNAPSHOTTING and the artifact survived
		// the reusability gate above. The artifact was never loaded on this
		// path — it is only assigned inside the BuildingExt4 branch — so load it
		// here. Without this, artifact is nil and the ImageConfigJSON access
		// below panics, and because runRedoTemplateImageJob runs in a bare
		// goroutine with no recover, that panic crashes the whole process. This
		// is the common redo case: distribution/snapshot failed but the
		// artifact itself is intact.
		artifact, err = getRootfsArtifactByID(ctx, jobRecord.ArtifactID)
		if err != nil {
			failRedoTemplateImageJob(ctx, jobID, resumePhase, fmt.Sprintf("load rootfs artifact %s for resume: %v", jobRecord.ArtifactID, err))
			return
		}
	}

	var imageCfg DockerImageConfig
	if strings.TrimSpace(artifact.ImageConfigJSON) != "" {
		if err := json.Unmarshal([]byte(artifact.ImageConfigJSON), &imageCfg); err != nil {
			failRedoTemplateImageJob(ctx, jobID, resumePhase, fmt.Sprintf("decode artifact image config fail: %v", err))
			return
		}
	}
	generatedReq, err := generateTemplateCreateRequest(ctx, withResolvedTemplateUser(&workingReq, resolvedTemplateUserFromArtifact(artifact)), artifact, imageCfg, downloadBaseURL)
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
		if err := ensureRedoTemplateNotInUse(ctx, req.TemplateID, generatedReq.InstanceType, storageBackendFromCreate(generatedReq), false, targets); err != nil {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseDistributing, err.Error())
			return
		}
		if err := cleanupRedoArtifactBeforeRedistribute(ctx, artifact.ArtifactID, generatedReq.InstanceType, req.TemplateID, targets); err != nil {
			failRedoTemplateImageJob(ctx, jobID, JobPhaseDistributing, err.Error())
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

	jfsVariantRedo := constants.IsJFSBackend(storageBackendFromCreate(generatedReq))
	if err := prepareRedoTemplateReplicas(ctx, req.TemplateID, generatedReq.InstanceType, existingReplicas, readyTargets, storageBackendFromCreate(generatedReq)); err != nil {
		failRedoTemplateImageJob(ctx, jobID, JobPhaseSnapshotting, err.Error())
		return
	}
	storedReq, err := normalizeStoredTemplateRequest(generatedReq, jfsVariantRedo)
	if err != nil {
		failRedoTemplateImageJob(ctx, jobID, resumePhase, err.Error())
		return
	}
	if _, err := ensureTemplateDefinitionWithOptions(ctx, req.TemplateID, storedReq, generatedReq.InstanceType, constants.GetAppSnapshotVersion(generatedReq.Annotations), definitionCreateOptions{
		StorageBackend: storageBackendFromCreate(generatedReq),
	}); err != nil {
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
