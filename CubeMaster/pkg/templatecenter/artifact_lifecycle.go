// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/tcclient"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// requestTemplateCenterArtifactDelete asks CubeTemplateCenter to remove an
// artifact's data (S3 object, local/shared ext4 file) and its own copy of the
// row. This is a seam so tests can stub the network call.
//
// Boundary: CubeMaster never touches the filesystem or S3 for an artifact
// directly -- only TC, which wrote the data, is allowed to remove it (see
// CubeTemplateCenter/pkg/build/deleter.go). If this call fails (TC down,
// network error), the caller must leave the row CLEANUP_PENDING; TC's own
// reconciler sweeps CLEANUP_PENDING rows as a backstop, so nothing is
// permanently orphaned as long as the row survives.
var requestTemplateCenterArtifactDelete = func(ctx context.Context, artifactID string) error {
	endpoint := ""
	if cfg := config.GetConfig(); cfg != nil {
		endpoint = cfg.TemplateCenterAddr()
	}
	if endpoint == "" {
		return fmt.Errorf("template center endpoint is not configured")
	}
	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return tcclient.NewClient(endpoint).DeleteArtifact(callCtx, artifactID)
}

// countArtifactReferencesTx counts the live references to artifactID inside the
// given transaction: surviving template replicas, active (PENDING/RUNNING)
// build jobs, and template definitions indexed by rootfs_artifact_id. When
// excludeTemplateID is non-empty its own rows are excluded so the caller can
// decide based on "everyone else" while deleting that template.
func countArtifactReferencesTx(ctx context.Context, tx *gorm.DB, artifactID, excludeTemplateID string) (int64, error) {
	_ = ctx
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return 0, nil
	}
	if strings.ContainsAny(artifactID, "%_") {
		return 0, errors.New("artifact id contains SQL wildcard characters")
	}
	excludeTemplateID = strings.TrimSpace(excludeTemplateID)

	var replicaCount int64
	q := tx.Table(constants.TemplateReplicaTableName).Where("artifact_id = ?", artifactID)
	if excludeTemplateID != "" {
		q = q.Where("template_id <> ?", excludeTemplateID)
	}
	if err := q.Count(&replicaCount).Error; err != nil {
		return 0, err
	}

	var jobCount int64
	jq := tx.Table(constants.TemplateImageJobTableName).
		Where("artifact_id = ? AND status IN ?", artifactID, []string{JobStatusPending, JobStatusRunning})
	if excludeTemplateID != "" {
		jq = jq.Where("template_id <> ?", excludeTemplateID)
	}
	if err := jq.Count(&jobCount).Error; err != nil {
		return 0, err
	}

	var defCount int64
	dq := tx.Table(constants.TemplateDefinitionTableName).
		Where("rootfs_artifact_id = ?", artifactID)
	if excludeTemplateID != "" {
		dq = dq.Where("template_id <> ?", excludeTemplateID)
	}
	if err := dq.Count(&defCount).Error; err != nil {
		return 0, err
	}

	return replicaCount + jobCount + defCount, nil
}

// cleanupArtifactFully implements the three-phase last-owner-cleanup for one
// artifact, shared by online template deletion, fresh-build failure cleanup,
// and the orphan GC:
//
//	Phase 1 (short TX, artifact row FOR UPDATE): drop excludeTemplateID's
//	  replica bindings for this artifact, count remaining references; if any
//	  remain, keep the artifact. Otherwise snapshot the placement nodes and
//	  mark the row CLEANUP_PENDING.
//	Phase 2 (no lock, no TX, idempotent): destroy the ext4 files on every
//	  placement node.
//	Phase 3 (short TX, FOR UPDATE): only if node-side physical deletes
//	  succeeded, re-check that references are still zero and status is still
//	  CLEANUP_PENDING, then remove the master-local ext4 under the row lock
//	  before deleting placement rows and the artifact row; if it was
//	  re-referenced meanwhile, leave status as-is and return so Phase 1 /
//	  claimRootfsArtifactForBuild can converge it.
//
// Partial physical failures (a node still running a sandbox, transient RPC
// errors) leave the row in CLEANUP_PENDING and return nil so template deletion
// proceeds; the GC converges later. Only hard DB errors are returned.
func cleanupArtifactFully(ctx context.Context, artifactID, instanceType, excludeTemplateID string) error {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return nil
	}
	logger := log.G(ctx).WithFields(map[string]any{"artifact_id": artifactID})

	// ── Phase 1 ──────────────────────────────────────────────────────────────
	var (
		proceed bool
		nodes   []models.ArtifactNodePlacement
	)
	if err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var artifact models.RootfsArtifact
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Table(constants.RootfsArtifactTableName).
			Where("artifact_id = ?", artifactID).First(&artifact).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // already gone
		}
		if err != nil {
			return err
		}
		if excludeTemplateID != "" {
			if err := tx.Unscoped().Table(constants.TemplateReplicaTableName).
				Where("template_id = ? AND artifact_id = ?", excludeTemplateID, artifactID).
				Delete(&models.TemplateReplica{}).Error; err != nil {
				return err
			}
		}
		remaining, err := countArtifactReferencesTx(ctx, tx, artifactID, excludeTemplateID)
		if err != nil {
			return err
		}
		if remaining > 0 {
			// Still referenced by another template/job: keep it, renew the GC
			// TTL, and revert a stale CLEANUP_PENDING back to READY so a
			// re-referenced artifact becomes reusable again.
			updates := map[string]any{"gc_deadline": time.Now().Add(defaultTemplateArtifactTTL).Unix()}
			if artifact.Status == ArtifactStatusCleanupPending {
				updates["status"] = ArtifactStatusReady
			}
			return tx.Table(constants.RootfsArtifactTableName).
				Where("artifact_id = ?", artifactID).Updates(updates).Error
		}
		nodes, err = listArtifactNodePlacementsTx(tx, artifactID)
		if err != nil {
			return err
		}
		if err := tx.Table(constants.RootfsArtifactTableName).
			Where("artifact_id = ?", artifactID).
			Update("status", ArtifactStatusCleanupPending).Error; err != nil {
			return err
		}
		proceed = true
		return nil
	}); err != nil {
		return err
	}
	if !proceed {
		return nil
	}

	// ── Phase 2 (no lock) ──────────────────────────────────────────────────────
	allPhysicalOK := true
	for i := range nodes {
		target := placementToNode(&nodes[i])
		if target == nil {
			// Can't resolve an address: treat as not-yet-cleaned so GC retries
			// rather than dropping the placement record and leaking the files.
			allPhysicalOK = false
			logger.Warnf("artifact cleanup: cannot resolve address for placement node %s", nodes[i].NodeID)
			continue
		}
		inUse, err := destroyArtifactOnNode(ctx, artifactID, instanceType, target)
		if inUse {
			allPhysicalOK = false
			logger.Infof("artifact cleanup deferred: node %s still uses artifact, GC will retry", target.ID())
			continue
		}
		if err != nil {
			allPhysicalOK = false
			logger.Warnf("artifact cleanup: destroy on node %s failed: %v", target.ID(), err)
		}
	}
	if !allPhysicalOK {
		// Keep the row in CLEANUP_PENDING; GC retries after sandboxes exit.
		return nil
	}

	// ── Phase 3 ──────────────────────────────────────────────────────────────
	// CubeMaster no longer removes the ext4 file or the artifact row itself
	// here: only CubeTemplateCenter (which wrote the S3 object and the local
	// file) is allowed to do that (see requestTemplateCenterArtifactDelete).
	// Doing it here used to hard-delete the row in the SAME transaction that
	// removed the local file WITHOUT ever telling TC to delete the S3
	// object, so the S3 object leaked forever: there was no CLEANUP_PENDING
	// row left for TC's reconciler backstop to sweep. Phase 3 now only drops
	// Master's own bookkeeping (placement rows) and leaves the artifact row
	// as CLEANUP_PENDING for the notify-then-backstop flow below.
	var finalized bool
	if err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var artifact models.RootfsArtifact
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Table(constants.RootfsArtifactTableName).
			Where("artifact_id = ?", artifactID).First(&artifact).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// Re-check with the SAME exclusion as phase 1. The deleted template's own
		// definition/replica rows are still present here (metadata cleanup runs
		// after artifact cleanup), so a phase-1-consistent exclusion is required
		// to avoid counting the very template we are deleting as a live owner.
		remaining, err := countArtifactReferencesTx(ctx, tx, artifactID, excludeTemplateID)
		if err != nil {
			return err
		}
		// Only finalise deletion when (a) nothing else references the artifact
		// and (b) no concurrent build resurrected the row. Any re-reference that
		// appeared during the lock-free physical phase necessarily went through
		// claimRootfsArtifactForBuild, which flips the status to BUILDING and
		// rebuilds the physical files; in that case we must NOT delete and must
		// NOT clobber that build's status (reverting to READY would expose a row
		// pointing at the files we just deleted). Back off and let the build /
		// GC converge.
		if remaining > 0 || artifact.Status != ArtifactStatusCleanupPending {
			return nil
		}
		if err := deleteArtifactNodePlacementsTx(tx, artifactID); err != nil {
			return err
		}
		finalized = true
		return nil
	}); err != nil {
		return err
	}
	if !finalized {
		return nil
	}
	if err := requestTemplateCenterArtifactDelete(ctx, artifactID); err != nil {
		// Do not fail template deletion over this: the row stays
		// CLEANUP_PENDING and TC's reconciler backstop-sweeps it later.
		logger.Warnf("artifact cleanup: notify templatecenter to delete artifact failed (row stays CLEANUP_PENDING, TC reconciler will retry): %v", err)
	}
	return nil
}

// placementToNode resolves a placement row to a node with a usable host ip,
// falling back to the local node cache when the recorded ip is empty.
func placementToNode(p *models.ArtifactNodePlacement) *node.Node {
	if p == nil {
		return nil
	}
	nodeID := strings.TrimSpace(p.NodeID)
	nodeIP := strings.TrimSpace(p.NodeIP)
	if nodeIP == "" && nodeID != "" {
		if cached, ok := localcache.GetNode(nodeID); ok && cached != nil {
			nodeIP = strings.TrimSpace(cached.HostIP())
		}
	}
	if nodeIP == "" {
		return nil
	}
	return &node.Node{InsID: nodeID, IP: nodeIP}
}
