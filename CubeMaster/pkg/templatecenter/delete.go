// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

var ErrTemplateInUse = errors.New("template is still in use")
var ErrTemplateCleanupLocatorMissing = errors.New("template cleanup locator is missing")
var ErrSnapshotReplicaMetadataIncomplete = errors.New("snapshot replica metadata is incomplete")

var (
	cleanupTemplateOnCubelet = cubelet.CleanupTemplate
	deleteImageOnCubelet     = cubelet.DeleteImage
	getCubeletAddrForDelete  = cubelet.GetCubeletAddr
	listTemplateSandboxes    = sandbox.ListSandbox
	runReplicaCleanup        = cleanupTemplateReplicasWithLocators
	runArtifactCleanup       = cleanupTemplateArtifact
	runMetadataCleanup       = cleanupTemplateMetadata
	runTemplateJobCleanup    = cleanupTemplateJobs
	// runInUseCheck and runFailActiveWork follow the same seam convention as the
	// cleanup stages above so the force-delete ordering can be tested without a
	// live node cache (isTemplateInUse short-circuits to false when no healthy
	// node is registered, which would mask the in-use guard entirely).
	runInUseCheck     = isTemplateInUse
	runFailActiveWork = failActiveTemplateWork
)

// templateCleanupLocator identifies a single cubelet that may hold artifacts
// for a template. v4: cubelet is the authority for the actual physical
// objects, so master no longer carries SnapshotPath / Objects here. The
// fields are retained as deprecated for one release to keep DB rows from
// failing JSON decode on legacy upgrade paths.
// templateCleanupLocator identifies the cubelet node that hosts artifacts to
// be cleaned up for a template/snapshot. v5: the deprecated SnapshotPath and
// Objects fields were removed; cubelet is the sole authority on physical
// layout and resolves both from its local catalog (with deterministic
// fallback) keyed by templateID.
type templateCleanupLocator struct {
	NodeID string
	NodeIP string
}

type templateCleanupTargets struct {
	Definition   *models.TemplateDefinition
	Snapshot     *models.SnapshotRecord
	Replicas     []models.TemplateReplica
	Jobs         []models.TemplateImageJob
	Locators     []templateCleanupLocator
	ArtifactIDs  map[string]struct{}
	InstanceType string
}

// cleanupLocatorKey produces a stable dedup key for a locator. v4: identity
// is purely the (node id, node ip) pair; SnapshotPath is no longer included
// because master does not track it.
func cleanupLocatorKey(locator templateCleanupLocator) string {
	return strings.Join([]string{
		strings.TrimSpace(locator.NodeID),
		strings.TrimSpace(locator.NodeIP),
	}, "|")
}

// DeleteTemplateOptions carries the non-identifying knobs of a delete.
type DeleteTemplateOptions struct {
	// Force allows the delete to proceed even though a build job or a
	// definition build is still marked active.
	//
	// WHY THIS EXISTS
	// ---------------
	// A template whose job is stuck in PENDING/RUNNING cannot be deleted at
	// all: the guards below reject it, and the only thing that ever clears the
	// state is failStaleRunningJobs, which waits for the staleness window to
	// elapse. Until then the template is undeletable and its name is taken,
	// with no way for an operator to intervene. Force marks the in-flight work
	// FAILED and continues.
	//
	// Force deliberately does NOT relax the in-use check: a template still
	// referenced by a live sandbox stays undeletable, because deleting its
	// artifact would break a running workload. Force is about unsticking
	// bookkeeping, not about overriding data safety.
	Force bool
}

func DeleteTemplate(ctx context.Context, templateID, instanceType string) error {
	return DeleteTemplateWithOptions(ctx, templateID, instanceType, DeleteTemplateOptions{})
}

func DeleteTemplateWithOptions(ctx context.Context, templateID, instanceType string, opts DeleteTemplateOptions) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	if rec, err := getSnapshotRecord(ctx, templateID); err == nil && rec != nil {
		_, err := DeleteSnapshot(ctx, uuid.NewString(), templateID, instanceType)
		return err
	} else if err != nil && !errors.Is(err, ErrSnapshotNotFound) {
		return err
	}
	return withTemplateWriteLock(templateID, func() error {
		targets, err := discoverTemplateCleanupTargets(ctx, templateID, instanceType)
		if err != nil {
			return err
		}
		return deleteTemplateWithTargets(ctx, templateID, targets, opts)
	})
}

// activeWorkBlocker returns the error that stops a non-forced delete, or nil
// when no in-flight work is in the way. Split out from the delete flow so the
// state matrix is testable without a database.
func activeWorkBlocker(targets *templateCleanupTargets, templateID string) error {
	if targets == nil {
		return nil
	}
	if targets.hasActiveJob() {
		return fmt.Errorf("%w: template %s deletion is blocked while a build job is still active", ErrTemplateAttemptInProgress, templateID)
	}
	if targets.hasActiveDefinitionBuild() {
		return fmt.Errorf("%w: template %s deletion is blocked while definition creation is still active", ErrTemplateAttemptInProgress, templateID)
	}
	return nil
}

func deleteTemplateWithTargets(ctx context.Context, templateID string, targets *templateCleanupTargets, opts DeleteTemplateOptions) error {
	if !targets.hasCleanupState() {
		return ErrTemplateNotFound
	}
	blocker := activeWorkBlocker(targets, templateID)
	if blocker != nil && !opts.Force {
		return blocker
	}
	if targets.requiresCleanupLocator() {
		if !opts.Force {
			return fmt.Errorf("%w: template %s has historical cleanup state but no node locator", ErrTemplateCleanupLocatorMissing, templateID)
		}
		// Being unable to name a node is exactly the kind of stuck bookkeeping
		// force exists to clear: there is nowhere to send a cubelet cleanup, so
		// the remaining work is metadata-only.
		log.G(ctx).Warnf("force-deleting template %s with no node locator; cubelet-side cleanup is skipped", templateID)
	}
	// The in-use check MUST run before any status is rewritten. shouldCheckInUse
	// returns false once the definition reads FAILED, so failing the in-flight
	// work first would silently skip this check and let a force delete pull the
	// artifact out from under a running sandbox.
	if targets.shouldCheckInUse() {
		inUse, err := runInUseCheck(ctx, templateID, targets.InstanceType)
		if err != nil {
			return err
		}
		if inUse {
			return ErrTemplateInUse
		}
	}
	if blocker != nil {
		// Force path. The in-flight job's goroutine may still be running and
		// would otherwise write its rows back after the cleanup below,
		// resurrecting a half-deleted template. Marking it FAILED first makes
		// those late writes land on a terminal row instead.
		if err := runFailActiveWork(ctx, templateID, targets); err != nil {
			return err
		}
	}
	if err := runReplicaCleanup(ctx, templateID, targets.Locators, cleanupBackendFromTargets(targets)); err != nil {
		return err
	}
	if err := runArtifactCleanup(ctx, templateID, targets); err != nil {
		return err
	}
	if err := runMetadataCleanup(ctx, templateID); err != nil {
		invalidateTemplateCaches(templateID)
		return err
	}
	invalidateTemplateCaches(templateID)
	if err := runTemplateJobCleanup(ctx, templateID); err != nil {
		return err
	}
	return nil
}

// forcedDeleteJobError is recorded on jobs terminated by a force delete so the
// reason survives in logs and in any job row that outlives the cleanup.
const forcedDeleteJobError = "template force-deleted while this job was still active"

// failActiveTemplateWork marks every PENDING/RUNNING job and an in-progress
// definition build as FAILED. Errors are joined rather than returned early: a
// force delete that gives up halfway would leave exactly the inconsistent state
// the caller is trying to escape.
func failActiveTemplateWork(ctx context.Context, templateID string, targets *templateCleanupTargets) error {
	if targets == nil {
		return nil
	}
	var errs error
	for _, job := range targets.Jobs {
		if !strings.EqualFold(job.Status, JobStatusPending) && !strings.EqualFold(job.Status, JobStatusRunning) {
			continue
		}
		log.G(ctx).Warnf("force delete: marking active template job %s (status=%s phase=%s) as %s",
			job.JobID, job.Status, job.Phase, JobStatusFailed)
		if err := updateTemplateImageJob(ctx, job.JobID, map[string]any{
			"status":        JobStatusFailed,
			"progress":      100,
			"error_message": forcedDeleteJobError,
		}); err != nil {
			errs = errors.Join(errs, fmt.Errorf("fail active job %s: %w", job.JobID, err))
		}
	}
	if targets.hasActiveDefinitionBuild() {
		log.G(ctx).Warnf("force delete: marking template %s definition (status=%s) as %s",
			templateID, targets.Definition.Status, StatusFailed)
		if err := UpdateDefinitionStatus(ctx, templateID, StatusFailed, forcedDeleteJobError); err != nil {
			errs = errors.Join(errs, fmt.Errorf("fail definition build: %w", err))
		}
	}
	return errs
}

func discoverTemplateCleanupTargets(ctx context.Context, templateID, instanceType string) (*templateCleanupTargets, error) {
	targets := &templateCleanupTargets{
		ArtifactIDs: make(map[string]struct{}),
	}

	def, err := GetDefinition(ctx, templateID)
	switch {
	case err == nil:
		targets.Definition = def
		if instanceType == "" {
			instanceType = def.InstanceType
		}
	case errors.Is(err, ErrTemplateNotFound):
	default:
		return nil, err
	}

	if rec, snapErr := getSnapshotRecord(ctx, templateID); snapErr == nil && rec != nil {
		targets.Snapshot = rec
		if instanceType == "" {
			instanceType = rec.InstanceType
		}
		targets.addLocator(templateCleanupLocator{
			NodeID: rec.OriginNodeID,
			NodeIP: rec.OriginNodeIP,
		})
	} else if snapErr != nil && !errors.Is(snapErr, ErrSnapshotNotFound) {
		return nil, snapErr
	}

	replicas, err := ListReplicas(ctx, templateID)
	if err != nil {
		return nil, err
	}
	targets.Replicas = replicas
	for _, replica := range replicas {
		if replica.ArtifactID != "" {
			targets.ArtifactIDs[replica.ArtifactID] = struct{}{}
		}
		targets.addLocator(templateCleanupLocator{
			NodeID: replica.NodeID,
			NodeIP: replica.NodeIP,
		})
	}

	jobs, err := listTemplateImageJobsByTemplateID(ctx, templateID)
	if err != nil {
		return nil, err
	}
	targets.Jobs = jobs
	for _, job := range jobs {
		if instanceType == "" && job.InstanceType != "" {
			instanceType = job.InstanceType
		}
		if job.ArtifactID != "" {
			targets.ArtifactIDs[job.ArtifactID] = struct{}{}
		}
		targets.addLocator(templateCleanupLocator{
			NodeID: job.NodeID,
			NodeIP: job.NodeIP,
		})
	}

	if instanceType == "" {
		instanceType = cubeboxv1.InstanceType_cubebox.String()
	}
	targets.InstanceType = instanceType
	return targets, nil
}

func (t *templateCleanupTargets) addLocator(locator templateCleanupLocator) {
	if strings.TrimSpace(locator.NodeID) == "" && strings.TrimSpace(locator.NodeIP) == "" {
		return
	}
	key := cleanupLocatorKey(locator)
	for _, existing := range t.Locators {
		if cleanupLocatorKey(existing) == key {
			return
		}
	}
	t.Locators = append(t.Locators, locator)
}

func (t *templateCleanupTargets) hasCleanupState() bool {
	return t != nil && (t.Definition != nil || t.Snapshot != nil || len(t.Replicas) > 0 || len(t.Jobs) > 0)
}

func (t *templateCleanupTargets) hasActiveJob() bool {
	if t == nil {
		return false
	}
	for _, job := range t.Jobs {
		if strings.EqualFold(job.Status, JobStatusPending) || strings.EqualFold(job.Status, JobStatusRunning) {
			return true
		}
	}
	return false
}

func (t *templateCleanupTargets) hasActiveDefinitionBuild() bool {
	if t == nil || t.Definition == nil {
		return false
	}
	return strings.EqualFold(t.Definition.Status, StatusPending) || strings.EqualFold(t.Definition.Status, StatusCreating)
}

func (t *templateCleanupTargets) requiresCleanupLocator() bool {
	if t == nil {
		return false
	}
	if len(t.Locators) > 0 || t.Definition != nil || len(t.Replicas) > 0 || len(t.ArtifactIDs) > 0 {
		return false
	}
	// v4: a job is considered to need cleanup on a cubelet if it identifies
	// a host (node id or ip). The legacy snapshot_path signal is no longer
	// authoritative on master (cubelet owns it via local catalog), but jobs
	// with no node binding at all are orphaned records with nothing to clean
	// up anywhere and can be safely removed without a locator.
	for _, job := range t.Jobs {
		if strings.TrimSpace(job.NodeID) != "" || strings.TrimSpace(job.NodeIP) != "" {
			return true
		}
	}
	return false
}

func (t *templateCleanupTargets) shouldCheckInUse() bool {
	if t == nil {
		return false
	}
	if t.Snapshot != nil {
		return !strings.EqualFold(t.Snapshot.Status, StatusFailed)
	}
	if t.Definition == nil {
		return false
	}
	return !strings.EqualFold(t.Definition.Status, StatusFailed)
}

func cleanupTemplateMetadata(ctx context.Context, templateID string) error {
	var cleanupErr error
	if err := store.db.WithContext(ctx).Unscoped().Table(constants.TemplateReplicaTableName).
		Where("template_id = ?", templateID).Delete(&models.TemplateReplica{}).Error; err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := store.db.WithContext(ctx).Unscoped().Table(constants.TemplateDefinitionTableName).
		Where("template_id = ?", templateID).Delete(&models.TemplateDefinition{}).Error; err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := store.db.WithContext(ctx).Unscoped().Table(constants.SnapshotTableName).
		Where("snapshot_id = ?", templateID).Delete(&models.SnapshotRecord{}).Error; err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	return cleanupErr
}

func cleanupTemplateJobs(ctx context.Context, templateID string) error {
	return store.db.WithContext(ctx).Unscoped().Table(constants.TemplateImageJobTableName).
		Where("template_id = ?", templateID).Delete(&models.TemplateImageJob{}).Error
}

// cleanupTemplateArtifact runs reference-aware, last-owner cleanup for every
// artifact this template references. Artifacts are processed in lexical order
// of artifact_id so concurrent deletions of templates sharing multiple
// artifacts acquire the per-artifact row locks in a consistent order (deadlock
// avoidance). Each artifact's physical removal and metadata deletion are
// delegated to cleanupArtifactFully (three-phase, lock-free RPC).
func cleanupTemplateArtifact(ctx context.Context, templateID string, targets *templateCleanupTargets) error {
	if targets == nil || len(targets.ArtifactIDs) == 0 {
		return nil
	}
	artifactIDs := make([]string, 0, len(targets.ArtifactIDs))
	for artifactID := range targets.ArtifactIDs {
		if strings.TrimSpace(artifactID) != "" {
			artifactIDs = append(artifactIDs, artifactID)
		}
	}
	sort.Strings(artifactIDs)

	var cleanupErr error
	for _, artifactID := range artifactIDs {
		if err := cleanupArtifactFully(ctx, artifactID, targets.InstanceType, templateID); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
}

func cleanupTemplateReplicas(ctx context.Context, templateID string) error {
	targets, err := discoverTemplateCleanupTargets(ctx, templateID, "")
	if err != nil {
		return err
	}
	return cleanupTemplateReplicasWithLocators(ctx, templateID, targets.Locators, cleanupBackendFromTargets(targets))
}

func cleanupTemplateReplicasWithLocators(ctx context.Context, templateID string, locators []templateCleanupLocator, backend string) error {
	backend = pinnedCleanupBackend(backend)
	var cleanupErr error
	for _, locator := range locators {
		hostIP := locator.NodeIP
		if hostIP == "" && locator.NodeID != "" {
			if n, ok := localcache.GetNode(locator.NodeID); ok && n != nil {
				hostIP = n.HostIP()
			}
		}
		if hostIP == "" {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup template %s: missing node address for locator node=%s", templateID, locator.NodeID))
			continue
		}
		// v4: master sends only templateID + node identity. Cubelet derives
		// SnapshotPath + Objects from its local catalog (with deterministic
		// fallback) so master no longer needs to know any physical detail.
		rsp, err := cleanupTemplateOnCubelet(ctx, getCubeletAddrForDelete(hostIP), &cubeboxv1.CleanupTemplateRequest{
			RequestID:  uuid.NewString(),
			TemplateID: templateID,
			Backend:    backend,
		})
		if err != nil {
			if isIgnorableTemplateCleanupError(err) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup template %s on node %s: %w", templateID, locator.NodeID, err))
			continue
		}
		if rsp.GetRet() != nil && int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
			if isIgnorableTemplateCleanupMessage(rsp.GetRet().GetRetMsg()) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup template %s on node %s failed: %s", templateID, locator.NodeID, rsp.GetRet().GetRetMsg()))
		}
	}
	return cleanupErr
}

func isIgnorableTemplateCleanupError(err error) bool {
	if err == nil {
		return false
	}
	return isIgnorableTemplateCleanupMessage(err.Error())
}

func isIgnorableTemplateCleanupMessage(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "no such file") {
		return true
	}
	hasMissing := strings.Contains(msg, "not found") || strings.Contains(msg, "not exist") || strings.Contains(msg, "does not exist")
	hasTemplatePath := strings.Contains(msg, "snapshot") || strings.Contains(msg, "template") || strings.Contains(msg, "path") || strings.Contains(msg, "directory") || strings.Contains(msg, "file")
	return hasMissing && hasTemplatePath
}

func isIgnorableArtifactDeleteError(err error) bool {
	if err == nil {
		return false
	}
	return isIgnorableArtifactDeleteMessage(err.Error())
}

func isIgnorableArtifactDeleteMessage(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "no such image") {
		return true
	}
	hasMissing := strings.Contains(msg, "not found") || strings.Contains(msg, "not exist") || strings.Contains(msg, "does not exist")
	hasImage := strings.Contains(msg, "image")
	return hasMissing && hasImage
}

func isTemplateInUse(ctx context.Context, templateID, instanceType string) (bool, error) {
	nodeCount := localcache.GetHealthyNodesByInstanceType(-1, instanceType).Len()
	if nodeCount == 0 {
		return false, nil
	}
	rsp := listTemplateSandboxes(ctx, &sandboxtypes.ListCubeSandboxReq{
		RequestID:    uuid.New().String(),
		InstanceType: instanceType,
		StartIdx:     1,
		Size:         nodeCount,
	})
	if rsp == nil || rsp.Ret == nil {
		return false, errors.New("list sandbox returned empty response")
	}
	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		return false, fmt.Errorf("list sandbox failed: %s", rsp.Ret.RetMsg)
	}
	for _, item := range rsp.Data {
		if item == nil {
			continue
		}
		if item.TemplateID == templateID {
			return true, nil
		}
		if item.Labels[constants.CubeAnnotationAppSnapshotTemplateID] == templateID {
			return true, nil
		}
	}
	return false, nil
}
