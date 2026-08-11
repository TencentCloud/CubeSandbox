// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package pausesnap persists the thin pause binding on Master: sandboxID ↔
// snapshotID (Kind=pause_snapshot) plus source node. Recreate metadata
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

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"gorm.io/gorm"
)

// pauseRequestJSON is stored in TemplateDefinition.RequestJSON for pause snaps.
type pauseRequestJSON struct {
	PluginVolumeIDs []string `json:"plugin_volume_ids,omitempty"`
}

const (
	// KindPauseSnapshot is TemplateDefinition.Kind for Pause-produced snaps.
	// Normal Commit snaps use templatecenter.TemplateKindSnapshot ("snapshot").
	KindPauseSnapshot = "pause_snapshot"
	statusReady       = "READY"
	statusCreating    = "CREATING"
	replicaReady      = "READY"
	replicaPhaseReady = "READY"
	snapshotIDPrefix  = "snap-"
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
	SandboxID       string
	SnapshotID      string
	NodeID          string
	NodeIP          string
	Status          string
	PluginVolumeIDs []string
}

// Begin allocates snapshotID and inserts a CREATING definition. Master only
// stores sandboxID↔snapshotID (+ node on Complete); recreate meta is packed
// with the snapshot on Cubelet (sandbox_spec.json).
func Begin(ctx context.Context, sandboxID, nodeID, nodeIP, instanceType string) (string, error) {
	client := getDB()
	if client == nil {
		return "", ErrNotReady
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return "", errors.New("sandboxID is required")
	}
	if existing, err := GetBySandbox(ctx, sandboxID); err == nil && existing != nil {
		return "", fmt.Errorf("sandbox %s already has pause snapshot %s", sandboxID, existing.SnapshotID)
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}

	snapID := GenerateSnapshotID()
	def := &models.TemplateDefinition{
		TemplateID:      snapID,
		InstanceType:    strings.TrimSpace(instanceType),
		Version:         "v1",
		Status:          statusCreating,
		Kind:            KindPauseSnapshot,
		OriginSandboxID: sandboxID,
		OriginNodeID:    strings.TrimSpace(nodeID),
		StorageBackend:  "cubecow",
	}
	if err := client.WithContext(ctx).Table(constants.TemplateDefinitionTableName).Create(def).Error; err != nil {
		return "", err
	}
	_ = nodeIP // placement finalized in Complete
	return snapID, nil
}

// Complete marks the pause snapshot READY and records the node replica.
// pluginVolumeIDs are the plugin volumes Detached with the live sandbox (node
// 1→0); Resume rejects the sandbox if any of them were deleted meanwhile.
func Complete(ctx context.Context, sandboxID, snapshotID, nodeID, nodeIP, instanceType string, pluginVolumeIDs []string) error {
	client := getDB()
	if client == nil {
		return ErrNotReady
	}
	sandboxID = strings.TrimSpace(sandboxID)
	snapshotID = strings.TrimSpace(snapshotID)
	nodeID = strings.TrimSpace(nodeID)
	if sandboxID == "" || snapshotID == "" {
		return errors.New("sandboxID and snapshotID are required")
	}
	updates := map[string]any{
		"status":         statusReady,
		"last_error":     "",
		"origin_node_id": nodeID,
	}
	if raw, err := json.Marshal(pauseRequestJSON{PluginVolumeIDs: uniqueNonEmpty(pluginVolumeIDs)}); err == nil {
		updates["request_json"] = string(raw)
	}
	res := client.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("template_id = ? AND kind = ? AND origin_sandbox_id = ?", snapshotID, KindPauseSnapshot, sandboxID).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, snapshotID)
	}

	var existing models.TemplateReplica
	err := client.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ?", snapshotID, nodeID).
		First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return client.WithContext(ctx).Table(constants.TemplateReplicaTableName).Create(&models.TemplateReplica{
			TemplateID:   snapshotID,
			NodeID:       nodeID,
			NodeIP:       strings.TrimSpace(nodeIP),
			InstanceType: strings.TrimSpace(instanceType),
			Status:       replicaReady,
			Phase:        replicaPhaseReady,
		}).Error
	}
	if err != nil {
		return err
	}
	return client.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ?", snapshotID, nodeID).
		Updates(map[string]any{
			"node_ip":       strings.TrimSpace(nodeIP),
			"instance_type": strings.TrimSpace(instanceType),
			"status":        replicaReady,
			"phase":         replicaPhaseReady,
			"error_message": "",
		}).Error
}

// Abort removes a CREATING pause definition after Cubelet Pause failure.
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
	if err := client.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("template_id = ? AND kind = ? AND origin_sandbox_id = ? AND status = ?",
			snapshotID, KindPauseSnapshot, sandboxID, statusCreating).
		Delete(&models.TemplateDefinition{}).Error; err != nil {
		log.G(ctx).Warnf("pausesnap.Abort delete failed sandbox=%s snap=%s: %v", sandboxID, snapshotID, err)
	}
}

// GetBySandbox returns the READY (or latest) pause snapshot for a sandbox.
func GetBySandbox(ctx context.Context, sandboxID string) (*Record, error) {
	client := getDB()
	if client == nil {
		return nil, ErrNotReady
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, errors.New("sandboxID is required")
	}
	var def models.TemplateDefinition
	err := client.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("origin_sandbox_id = ? AND kind = ?", sandboxID, KindPauseSnapshot).
		Order("updated_at desc").
		First(&def).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rec := &Record{
		SandboxID:       sandboxID,
		SnapshotID:      def.TemplateID,
		NodeID:          def.OriginNodeID,
		Status:          def.Status,
		PluginVolumeIDs: pluginVolumeIDsFromRequestJSON(def.RequestJSON),
	}
	var replica models.TemplateReplica
	if err := client.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ?", def.TemplateID).
		Order("updated_at desc").
		First(&replica).Error; err == nil {
		rec.NodeIP = replica.NodeIP
		if rec.NodeID == "" {
			rec.NodeID = replica.NodeID
		}
	}
	return rec, nil
}

func pluginVolumeIDsFromRequestJSON(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var meta pauseRequestJSON
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return nil
	}
	return uniqueNonEmpty(meta.PluginVolumeIDs)
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

// Delete removes pause-snapshot definition + replicas (best-effort GC after Resume).
func Delete(ctx context.Context, snapshotID string) error {
	client := getDB()
	if client == nil {
		return ErrNotReady
	}
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return errors.New("snapshotID is required")
	}
	if err := client.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ?", snapshotID).
		Delete(&models.TemplateReplica{}).Error; err != nil {
		return err
	}
	return client.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("template_id = ? AND kind = ?", snapshotID, KindPauseSnapshot).
		Delete(&models.TemplateDefinition{}).Error
}
