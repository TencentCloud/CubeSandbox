// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package pausesnap persists the thin pause binding on Master: sandboxID ↔
// snapshotID ↔ node, plus backend / remote_status. Recreate metadata
// (sandbox_spec.json) is packed with the snapshot on Cubelet for same-/cross-
// node Resume; it is not stored in Master DB.
package pausesnap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"gorm.io/gorm"
)

const (
	// KindPauseSnapshot is the logical kind returned by GetTemplateKind for
	// pause bindings. Pause rows no longer live in t_cube_template_definition.
	KindPauseSnapshot = "pause_snapshot"
	// StatusReady marks a completed pause: the shim is gone and the sandbox
	// exists only as this binding plus the node-local package.
	StatusReady    = "READY"
	statusReady    = StatusReady
	statusCreating = "CREATING"
	// StatusFailed is a terminal Pause failure. Binding and sandbox proxy are
	// kept so the user can see the failure; Resume is rejected until Delete.
	StatusFailed = "FAILED"
	statusFailed = StatusFailed
	// StatusDeleteFailed marks a Delete whose node-side cleanup failed: the
	// shim is gone but the pause package is still on the node, so the binding
	// stays as the record of what has to be swept. Distinct from StatusFailed,
	// which says Pause itself failed and the shim is still alive.
	StatusDeleteFailed = "DELETE_FAILED"
	statusDeleteFailed = StatusDeleteFailed
	snapshotIDPrefix   = "snap-"
)

var (
	mu     sync.RWMutex
	gormDB *gorm.DB

	ErrNotReady = errors.New("pausesnap store not initialized")
	ErrNotFound = errors.New("pause snapshot not found")
)

// Init attaches to the shared Master DB. Safe to call multiple times.
func Init(database *gorm.DB) {
	mu.Lock()
	defer mu.Unlock()
	gormDB = database
}

func getDB() *gorm.DB {
	mu.RLock()
	defer mu.RUnlock()
	return gormDB
}

// GenerateSnapshotID matches the normal snapshot ID format (snap- + 24 hex).
func GenerateSnapshotID() string {
	return snapshotIDPrefix + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
}

// Record is the Master-side pause snapshot binding.
type Record struct {
	SandboxID           string
	SnapshotID          string
	NodeID              string
	NodeIP              string
	InstanceType        string
	Status              string
	LastError           string
	PluginVolumeIDs     []string
	Backend             string
	RemoteStatus        string
	OriginHostFactsJSON string
	ExportUUIDs         string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// ListOptions narrows List. Empty fields match every binding.
type ListOptions struct {
	// HostID matches either node_id or node_ip, mirroring how callers
	// address a node elsewhere in Master.
	HostID       string
	InstanceType string
}

// List returns pause bindings, newest first.
//
// This table is Master's source of truth for paused sandboxes: their shim is
// gone, so a node scan cannot be relied on to report them. Callers that page
// over nodes should still call this once and merge the full result.
func List(ctx context.Context, opts ListOptions) ([]*Record, error) {
	client := getDB()
	if client == nil {
		return nil, ErrNotReady
	}
	query := client.WithContext(ctx).Table(constants.PauseSnapshotTableName)
	if host := strings.TrimSpace(opts.HostID); host != "" {
		query = query.Where("node_id = ? OR node_ip = ?", host, host)
	}
	if instanceType := strings.TrimSpace(opts.InstanceType); instanceType != "" {
		query = query.Where("instance_type = ? OR instance_type = ''", instanceType)
	}
	var rows []models.PauseSnapshotRecord
	if err := query.Order("updated_at desc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(rows))
	for i := range rows {
		out = append(out, recordFromModel(&rows[i]))
	}
	return out, nil
}

// Begin allocates snapshotID and inserts a CREATING pause binding.
func Begin(ctx context.Context, sandboxID, nodeID, nodeIP, instanceType, backend string) (string, error) {
	client := getDB()
	if client == nil {
		return "", ErrNotReady
	}
	ctx = context.WithoutCancel(ctx)
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return "", errors.New("sandboxID is required")
	}
	normalized, err := constants.NormalizeSnapshotBackend(backend)
	if err != nil {
		return "", err
	}
	if existing, err := GetBySandbox(ctx, sandboxID); err == nil && existing != nil {
		return "", fmt.Errorf("sandbox %s already has pause snapshot %s", sandboxID, existing.SnapshotID)
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}

	snapID := GenerateSnapshotID()
	row := &models.PauseSnapshotRecord{
		SnapshotID:          snapID,
		SandboxID:           sandboxID,
		NodeID:              strings.TrimSpace(nodeID),
		NodeIP:              strings.TrimSpace(nodeIP),
		InstanceType:        strings.TrimSpace(instanceType),
		Status:              statusCreating,
		Backend:             normalized,
		RemoteStatus:        constants.SnapshotRemoteStatus(normalized),
		OriginHostFactsJSON: freezeOriginHostFacts(ctx, nodeID),
	}
	if err := client.WithContext(ctx).Table(constants.PauseSnapshotTableName).Create(row).Error; err != nil {
		return "", err
	}
	return snapID, nil
}

// Complete marks the pause snapshot READY and records the node + plugin volumes.
// S3 export_uuids move remote_status to inprogress; empty uuids on an S3 row
// mean export failed — remote_status=failed so CanCrossNode stays false while
// same-node Resume still works. CubeMaster does not query Cubelet here.
func Complete(ctx context.Context, sandboxID, snapshotID, nodeID, nodeIP, instanceType string, pluginVolumeIDs []string, exportUUIDs string) error {
	client := getDB()
	if client == nil {
		return ErrNotReady
	}
	ctx = context.WithoutCancel(ctx)
	sandboxID = strings.TrimSpace(sandboxID)
	snapshotID = strings.TrimSpace(snapshotID)
	nodeID = strings.TrimSpace(nodeID)
	if sandboxID == "" || snapshotID == "" {
		return errors.New("sandboxID and snapshotID are required")
	}
	updates := map[string]any{
		"status":        statusReady,
		"last_error":    "",
		"node_id":       nodeID,
		"node_ip":       strings.TrimSpace(nodeIP),
		"instance_type": strings.TrimSpace(instanceType),
		"updated_at":    gorm.Expr("CURRENT_TIMESTAMP"),
	}
	if raw, err := json.Marshal(uniqueNonEmpty(pluginVolumeIDs)); err == nil {
		updates["plugin_volume_ids"] = string(raw)
	}
	var existing models.PauseSnapshotRecord
	if err := client.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("snapshot_id = ? AND sandbox_id = ?", snapshotID, sandboxID).
		First(&existing).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if constants.IsS3Backend(existing.Backend) {
		if raw := strings.TrimSpace(exportUUIDs); raw != "" {
			updates["export_uuids"] = raw
			updates["remote_status"] = constants.RemoteStatusInProgress
		} else {
			updates["export_uuids"] = ""
			updates["remote_status"] = constants.RemoteStatusFailed
		}
	}
	res := client.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("snapshot_id = ? AND sandbox_id = ?", snapshotID, sandboxID).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, snapshotID)
	}
	return nil
}

// MarkFailed records a terminal Pause failure without deleting the binding or
// asking Cubelet to drop local snap/sandbox rows. Used when Pause RPC fails or
// times out: do not undo in-flight Cubelet work.
func MarkFailed(ctx context.Context, sandboxID, snapshotID, errMsg string) {
	client := getDB()
	if client == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	sandboxID = strings.TrimSpace(sandboxID)
	snapshotID = strings.TrimSpace(snapshotID)
	errMsg = strings.TrimSpace(errMsg)
	if sandboxID == "" || snapshotID == "" {
		return
	}
	if errMsg == "" {
		errMsg = "pause failed"
	}
	if len(errMsg) > 1024 {
		errMsg = errMsg[:1024]
	}
	res := client.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("snapshot_id = ? AND sandbox_id = ? AND status IN ?",
			snapshotID, sandboxID, []string{statusCreating, statusFailed}).
		Updates(map[string]any{
			"status":     statusFailed,
			"last_error": errMsg,
			"updated_at": gorm.Expr("CURRENT_TIMESTAMP"),
		})
	if res.Error != nil {
		log.G(ctx).Warnf("pausesnap.MarkFailed sandbox=%s snap=%s: %v", sandboxID, snapshotID, res.Error)
	}
}

// MarkDeleteFailed records that Delete tore down the sandbox but could not
// clear the pause package on the node. The binding is kept on purpose: it is
// the only thing that still names the leftover package, and Delete is
// idempotent, so a later retry (manual or from the lifecycle sweeper) resumes
// from here once the node can drop the objects.
func MarkDeleteFailed(ctx context.Context, sandboxID, snapshotID, errMsg string) {
	client := getDB()
	if client == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	sandboxID = strings.TrimSpace(sandboxID)
	snapshotID = strings.TrimSpace(snapshotID)
	if sandboxID == "" || snapshotID == "" {
		return
	}
	errMsg = strings.TrimSpace(errMsg)
	if errMsg == "" {
		errMsg = "delete pause snapshot failed"
	}
	if len(errMsg) > 1024 {
		errMsg = errMsg[:1024]
	}
	res := client.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("snapshot_id = ? AND sandbox_id = ?", snapshotID, sandboxID).
		Updates(map[string]any{
			"status":     statusDeleteFailed,
			"last_error": errMsg,
			"updated_at": gorm.Expr("CURRENT_TIMESTAMP"),
		})
	if res.Error != nil {
		log.G(ctx).Warnf("pausesnap.MarkDeleteFailed sandbox=%s snap=%s: %v", sandboxID, snapshotID, res.Error)
	}
}

// Abort removes a CREATING pause binding. Prefer MarkFailed for user-visible
// Pause failures; Abort is only for clearing stale bindings when the sandbox is
// already RUNNING again (see clearStalePauseBindingIfRunning).
func Abort(ctx context.Context, sandboxID, snapshotID string) {
	client := getDB()
	if client == nil {
		return
	}
	sandboxID = strings.TrimSpace(sandboxID)
	snapshotID = strings.TrimSpace(snapshotID)
	if sandboxID == "" || snapshotID == "" {
		return
	}
	if err := client.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("snapshot_id = ? AND sandbox_id = ? AND status = ?",
			snapshotID, sandboxID, statusCreating).
		Delete(&models.PauseSnapshotRecord{}).Error; err != nil {
		log.G(ctx).Warnf("pausesnap.Abort delete failed sandbox=%s snap=%s: %v", sandboxID, snapshotID, err)
	}
}

// GetBySandbox returns the pause snapshot binding for a sandbox.
func GetBySandbox(ctx context.Context, sandboxID string) (*Record, error) {
	client := getDB()
	if client == nil {
		return nil, ErrNotReady
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, errors.New("sandboxID is required")
	}
	var row models.PauseSnapshotRecord
	err := client.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("sandbox_id = ?", sandboxID).
		Order("updated_at desc").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return recordFromModel(&row), nil
}

// GetBySnapshotID returns the pause binding for a snapshot id.
func GetBySnapshotID(ctx context.Context, snapshotID string) (*Record, error) {
	client := getDB()
	if client == nil {
		return nil, ErrNotReady
	}
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil, errors.New("snapshotID is required")
	}
	var row models.PauseSnapshotRecord
	err := client.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("snapshot_id = ?", snapshotID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return recordFromModel(&row), nil
}

func recordFromModel(row *models.PauseSnapshotRecord) *Record {
	if row == nil {
		return nil
	}
	return &Record{
		SandboxID:           row.SandboxID,
		SnapshotID:          row.SnapshotID,
		NodeID:              row.NodeID,
		NodeIP:              row.NodeIP,
		InstanceType:        row.InstanceType,
		Status:              row.Status,
		LastError:           strings.TrimSpace(row.LastError),
		PluginVolumeIDs:     pluginVolumeIDsFromJSON(row.PluginVolumeIDs),
		Backend:             row.Backend,
		RemoteStatus:        row.RemoteStatus,
		OriginHostFactsJSON: row.OriginHostFactsJSON,
		ExportUUIDs:         row.ExportUUIDs,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
	}
}

func freezeOriginHostFacts(ctx context.Context, nodeID string) string {
	facts, ok := nodemeta.GetNodeHostFacts(ctx, nodeID)
	if !ok || facts == nil {
		facts, ok = nodemeta.GetPersistedNodeHostFacts(ctx, nodeID)
		if !ok || facts == nil {
			return ""
		}
	}
	return nodemeta.RestoreMatchFactsJSON(facts)
}

func pluginVolumeIDsFromJSON(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return uniqueNonEmpty(ids)
}

func uniqueNonEmpty(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// Delete removes the pause-snapshot binding (best-effort GC after Resume).
func Delete(ctx context.Context, snapshotID string) error {
	client := getDB()
	if client == nil {
		return ErrNotReady
	}
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return errors.New("snapshotID is required")
	}
	return client.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("snapshot_id = ?", snapshotID).
		Delete(&models.PauseSnapshotRecord{}).Error
}
