// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

const (
	cleanupIPNodePrefix = "__cleanup_ip__"

	// unregisteredReplicaError is the replica-level reason shown when a
	// recorded node id is absent from the live CubeOps registry.
	unregisteredReplicaError = "node is no longer registered"
	// noRegisteredReadyReplicaError is the aggregate reason shown when every
	// READY replica of a template or snapshot sits on an unregistered node.
	noRegisteredReadyReplicaError = "no ready replica on a registered node"
)

// lookupRegisteredNodeIP resolves a node id against the live node cache.
// present is false when the id is not registered. hostIP may still be empty
// when the node is present but has not reported an address.
//
// Tests replace this seam; production reads CubeMaster's CubeOps-backed cache.
var lookupRegisteredNodeIP = func(nodeID string) (hostIP string, present bool) {
	n, ok := localcache.GetNode(nodeID)
	if !ok || n == nil {
		return "", false
	}
	return strings.TrimSpace(n.HostIP()), true
}

// cleanupNodeRegistryAuthoritative reports whether the live node cache has
// completed an authoritative sync. Readiness is tracked separately from
// cardinality: an initialized registry may legitimately be empty after the
// cluster scales to zero.
var cleanupNodeRegistryAuthoritative = func() bool {
	return localcache.NodeRegistryReady()
}

// unregisteredCleanupNode reports whether nodeID is absent from an initialized
// live registry. An empty node id is not "unregistered": IP-only locators
// still have to be dialed, because there is no id to look up.
func unregisteredCleanupNode(nodeID string) bool {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" || strings.HasPrefix(nodeID, cleanupIPNodePrefix) ||
		!cleanupNodeRegistryAuthoritative() {
		return false
	}
	_, present := lookupRegisteredNodeIP(nodeID)
	return !present
}

// replicaOnUnregisteredNode reports whether a replica's stable node id is
// absent from an initialized registry. An IP-only legacy locator is deliberately
// not inferred to be retired: an address is not a durable node identity, so it
// must remain retryable until an operator explicitly repairs it.
func replicaOnUnregisteredNode(replica ReplicaStatus) bool {
	if !cleanupNodeRegistryAuthoritative() {
		return false
	}
	if nodeID := strings.TrimSpace(replica.NodeID); nodeID != "" {
		return unregisteredCleanupNode(nodeID)
	}
	return false
}

// Seams keep delete coordination testable without a database.
var (
	persistReplicaCleanupForDelete = markReplicaCleanupRequired
	clearReplicaCleanupForDelete   = clearReplicaCleanupRequired
)

// cleanupTemplateReplicasForDelete separates permanent retirement from
// temporary unreachability:
//   - a node id absent from the initialized CubeOps registry is permanently
//     retired, so its stale locator is dropped without an RPC;
//   - a registered (including unhealthy) or not-yet-classifiable node is
//     called normally; a failure is persisted as a retry tombstone and does
//     not block deletion.
//
// Only failure to persist/clear the durable retry state fails the delete.
func cleanupTemplateReplicasForDelete(ctx context.Context, templateID string, locators []templateCleanupLocator, backend string) error {
	var persistenceErr error
	for _, locator := range locators {
		if unregisteredCleanupNode(locator.NodeID) {
			logRetiredCleanupSkip(ctx, templateID, locator)
			if err := clearReplicaCleanupForDelete(ctx, templateID, locator); err != nil {
				persistenceErr = errors.Join(persistenceErr,
					fmt.Errorf("clear retired replica cleanup marker for node %s: %w", locator.NodeID, err))
			}
			continue
		}

		err := cleanupTemplateReplicasWithLocators(ctx, templateID, []templateCleanupLocator{locator}, backend)
		if err == nil {
			if clearErr := clearReplicaCleanupForDelete(ctx, templateID, locator); clearErr != nil {
				persistenceErr = errors.Join(persistenceErr,
					fmt.Errorf("clear replica cleanup marker for node %s: %w", locator.NodeID, clearErr))
			}
			continue
		}

		log.G(ctx).Warnf("template cleanup deferred: template=%s node=%s ip=%s err=%v",
			templateID, locator.NodeID, locator.NodeIP, err)
		if persistErr := persistReplicaCleanupForDelete(ctx, templateID, locator, err); persistErr != nil {
			persistenceErr = errors.Join(persistenceErr, err,
				fmt.Errorf("persist replica cleanup marker for node %s: %w", locator.NodeID, persistErr))
		}
	}
	return persistenceErr
}

func logRetiredCleanupSkip(ctx context.Context, templateID string, locator templateCleanupLocator) {
	log.G(ctx).Warnf(
		"cleanup template %s: node %s is no longer registered in CubeOps; dropping stale node metadata without an RPC (stored ip %s)",
		templateID, locator.NodeID, locator.NodeIP)
}

// cleanupLocatorPersistenceNodeID returns the stable row key used by the
// orphan-cleanup reconciler. Current records have a node id. Legacy IP-only
// jobs/snapshots receive a deterministic synthetic key while retaining the IP
// used for retry; synthetic keys are never interpreted as CubeOps retirement.
func cleanupLocatorPersistenceNodeID(locator templateCleanupLocator) string {
	if nodeID := strings.TrimSpace(locator.NodeID); nodeID != "" {
		return nodeID
	}
	ip := strings.TrimSpace(locator.NodeIP)
	if ip == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ip))
	return cleanupIPNodePrefix + fmt.Sprintf("%x", sum[:12])
}

func markReplicaCleanupRequired(ctx context.Context, templateID string, locator templateCleanupLocator, cause error) error {
	nodeID := cleanupLocatorPersistenceNodeID(locator)
	if nodeID == "" {
		return errors.New("cleanup locator has neither node id nor node ip")
	}
	message := "cubelet cleanup failed"
	if cause != nil {
		message = cause.Error()
	}
	updates := map[string]any{
		"node_ip":          strings.TrimSpace(locator.NodeIP),
		"status":           ReplicaStatusFailed,
		"phase":            ReplicaPhaseCleaning,
		"last_error_phase": ReplicaPhaseCleaning,
		"cleanup_required": true,
		"error_message":    message,
		"updated_at":       time.Now(),
	}
	result := store.db.WithContext(ctx).Unscoped().Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ?", templateID, nodeID).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		return nil
	}
	return store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Create(&models.TemplateReplica{
			TemplateID:      templateID,
			NodeID:          nodeID,
			NodeIP:          strings.TrimSpace(locator.NodeIP),
			Status:          ReplicaStatusFailed,
			Phase:           ReplicaPhaseCleaning,
			LastErrorPhase:  ReplicaPhaseCleaning,
			CleanupRequired: true,
			ErrorMessage:    message,
		}).Error
}

func clearReplicaCleanupRequired(ctx context.Context, templateID string, locator templateCleanupLocator) error {
	nodeID := cleanupLocatorPersistenceNodeID(locator)
	if nodeID == "" {
		return nil
	}
	return store.db.WithContext(ctx).Unscoped().Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ?", templateID, nodeID).
		Update("cleanup_required", false).Error
}

// applyUnregisteredReplicaView rewrites the operator-facing replica list so a
// READY row whose node is no longer registered is not presented as usable.
// Stored rows are left alone; only the response is adjusted.
//
// Aggregate READY / PARTIALLY_READY is downgraded when that rewrite removes
// every registered READY replica (the template is not servable) or when a
// READY template still has a mix of live and gone nodes. Other statuses
// (PENDING, CREATING, DELETING, FAILED) are kept. An uninitialized registry
// leaves the view unchanged.
func applyUnregisteredReplicaView(status, lastError string, replicas []ReplicaStatus) (string, string, []ReplicaStatus) {
	if len(replicas) == 0 || !cleanupNodeRegistryAuthoritative() {
		return status, lastError, replicas
	}
	out := replicas
	copied := false
	registeredReady := 0
	droppedReady := 0
	for i := range replicas {
		if !replicaOnUnregisteredNode(replicas[i]) {
			if replicas[i].Status == ReplicaStatusReady {
				registeredReady++
			}
			continue
		}
		if replicas[i].Status != ReplicaStatusReady {
			continue
		}
		droppedReady++
		if !copied {
			out = append([]ReplicaStatus(nil), replicas...)
			copied = true
		}
		out[i].Status = ReplicaStatusFailed
		out[i].Phase = ReplicaPhaseFailed
		if strings.TrimSpace(out[i].ErrorMessage) == "" {
			out[i].ErrorMessage = unregisteredReplicaError
		}
	}
	if droppedReady == 0 {
		return status, lastError, replicas
	}
	switch status {
	case StatusReady, StatusPartiallyReady:
		if registeredReady == 0 {
			status = StatusFailed
			if strings.TrimSpace(lastError) == "" {
				lastError = noRegisteredReadyReplicaError
			}
		} else if status == StatusReady {
			status = StatusPartiallyReady
			if strings.TrimSpace(lastError) == "" {
				lastError = unregisteredReplicaError
			}
		}
	}
	return status, lastError, out
}

// listReplicaNodeStatuses loads the node id and status of every live replica
// row, grouped by template (or snapshot) id. For a job-only recovery entry
// that has no replica row yet, the latest CREATE/REDO job locator is used as
// the best available placement evidence. This keeps list and detail fallback
// behavior aligned without attaching full replica payloads to list items.
func listReplicaNodeStatuses(ctx context.Context) (map[string][]ReplicaStatus, error) {
	var rows []models.TemplateReplica
	err := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Select("template_id, node_id, node_ip, status").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string][]ReplicaStatus, len(rows))
	for _, row := range rows {
		out[row.TemplateID] = append(out[row.TemplateID], ReplicaStatus{
			NodeID: row.NodeID,
			NodeIP: row.NodeIP,
			Status: row.Status,
		})
	}
	var jobs []models.TemplateImageJob
	err = store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("operation IN ?", createRedoJobOperations).
		Order("template_id asc, attempt_no desc, id desc").
		Find(&jobs).Error
	if err != nil {
		return nil, err
	}
	seenJobs := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		if _, seen := seenJobs[job.TemplateID]; seen {
			continue
		}
		seenJobs[job.TemplateID] = struct{}{}
		if len(out[job.TemplateID]) > 0 ||
			(strings.TrimSpace(job.NodeID) == "" && strings.TrimSpace(job.NodeIP) == "") {
			continue
		}
		status := strings.ToUpper(strings.TrimSpace(job.TemplateStatus))
		if status == "" {
			status = strings.ToUpper(strings.TrimSpace(job.Status))
		}
		out[job.TemplateID] = []ReplicaStatus{{
			NodeID:       job.NodeID,
			NodeIP:       job.NodeIP,
			InstanceType: job.InstanceType,
			Status:       status,
			ArtifactID:   job.ArtifactID,
		}}
	}
	return out, nil
}

// adjustListedTemplateForUnregisteredReplicas applies the same READY downgrade
// as template info to one list entry. Templates with no replica rows are left
// unchanged.
func adjustListedTemplateForUnregisteredReplicas(info TemplateInfo, replicasByTemplate map[string][]ReplicaStatus) TemplateInfo {
	replicas := replicasByTemplate[info.TemplateID]
	if len(replicas) == 0 {
		return info
	}
	info.Status, info.LastError, _ = applyUnregisteredReplicaView(info.Status, info.LastError, replicas)
	return info
}

// applyUnregisteredTemplateListView derives an operator-facing list from the
// raw cached/DB status on every read. Keeping the cache raw is important:
// registry membership can change without a template-table write, and a node
// that re-registers must be able to restore READY before the cache TTL expires.
func applyUnregisteredTemplateListView(ctx context.Context, infos []TemplateInfo) ([]TemplateInfo, error) {
	out := append([]TemplateInfo(nil), infos...)
	if len(out) == 0 || !cleanupNodeRegistryAuthoritative() {
		return out, nil
	}
	replicasByTemplate, err := listReplicaNodeStatuses(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i] = adjustListedTemplateForUnregisteredReplicas(out[i], replicasByTemplate)
	}
	return out, nil
}

// applyUnregisteredTemplateInfoView applies the same dynamic projection to a
// detail response while leaving the raw cached object untouched.
func applyUnregisteredTemplateInfoView(info *TemplateInfo) *TemplateInfo {
	out := cloneTemplateInfo(info)
	if out == nil {
		return nil
	}
	out.Status, out.LastError, out.Replicas =
		applyUnregisteredReplicaView(out.Status, out.LastError, out.Replicas)
	return out
}
