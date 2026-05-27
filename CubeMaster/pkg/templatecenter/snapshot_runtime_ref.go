// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

const (
	SnapshotRuntimeBindingMemoryBacking = "memory_backing"
	SnapshotRuntimeRefStatusActive      = "ACTIVE"
	SnapshotRuntimeRefStatusReleased    = "RELEASED"
)

type SnapshotRuntimeRefInfo struct {
	ID          uint
	SnapshotID  string
	SandboxID   string
	NodeID      string
	NodeIP      string
	BindingType string
	MemoryVol   string
	MemoryDev   string
	RootfsVol   string
	SandboxGen  uint32
	Status      string
	AttachedAt  time.Time
	ReleasedAt  *time.Time
	LastSeenAt  *time.Time
	LastError   string
}

func snapshotRuntimeRefModelToInfo(model models.SnapshotRuntimeRef) SnapshotRuntimeRefInfo {
	return SnapshotRuntimeRefInfo{
		ID:          uint(model.ID),
		SnapshotID:  strings.TrimSpace(model.SnapshotID),
		SandboxID:   strings.TrimSpace(model.SandboxID),
		NodeID:      strings.TrimSpace(model.NodeID),
		NodeIP:      strings.TrimSpace(model.NodeIP),
		BindingType: strings.TrimSpace(model.BindingType),
		MemoryVol:   strings.TrimSpace(model.MemoryVol),
		MemoryDev:   strings.TrimSpace(model.MemoryDev),
		RootfsVol:   strings.TrimSpace(model.RootfsVol),
		SandboxGen:  model.SandboxGen,
		Status:      strings.TrimSpace(model.Status),
		AttachedAt:  model.AttachedAt,
		ReleasedAt:  model.ReleasedAt,
		LastSeenAt:  model.LastSeenAt,
		LastError:   strings.TrimSpace(model.LastError),
	}
}

func normalizeSnapshotRuntimeRef(ref SnapshotRuntimeRefInfo) SnapshotRuntimeRefInfo {
	ref.SnapshotID = strings.TrimSpace(ref.SnapshotID)
	ref.SandboxID = strings.TrimSpace(ref.SandboxID)
	ref.NodeID = strings.TrimSpace(ref.NodeID)
	ref.NodeIP = strings.TrimSpace(ref.NodeIP)
	ref.BindingType = strings.TrimSpace(ref.BindingType)
	ref.MemoryVol = strings.TrimSpace(ref.MemoryVol)
	ref.MemoryDev = strings.TrimSpace(ref.MemoryDev)
	ref.RootfsVol = strings.TrimSpace(ref.RootfsVol)
	ref.Status = strings.ToUpper(strings.TrimSpace(ref.Status))
	if ref.BindingType == "" {
		ref.BindingType = SnapshotRuntimeBindingMemoryBacking
	}
	if ref.Status == "" {
		ref.Status = SnapshotRuntimeRefStatusActive
	}
	return ref
}

func newActiveSnapshotRuntimeRefModel(ref SnapshotRuntimeRefInfo, attachedAt time.Time, lastSeenAt *time.Time) *models.SnapshotRuntimeRef {
	return &models.SnapshotRuntimeRef{
		SnapshotID:  ref.SnapshotID,
		SandboxID:   ref.SandboxID,
		NodeID:      ref.NodeID,
		NodeIP:      ref.NodeIP,
		BindingType: ref.BindingType,
		MemoryVol:   ref.MemoryVol,
		MemoryDev:   "",
		RootfsVol:   ref.RootfsVol,
		SandboxGen:  ref.SandboxGen,
		Status:      SnapshotRuntimeRefStatusActive,
		AttachedAt:  attachedAt,
		LastSeenAt:  lastSeenAt,
		LastError:   "",
	}
}

func releasedSnapshotRuntimeRefValues(now time.Time, reason string) map[string]any {
	return map[string]any{
		"status":      SnapshotRuntimeRefStatusReleased,
		"released_at": now,
		"updated_at":  now,
		"last_error":  strings.TrimSpace(reason),
	}
}

func AcquireSnapshotRuntimeRef(ctx context.Context, ref SnapshotRuntimeRefInfo) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	ref = normalizeSnapshotRuntimeRef(ref)
	if ref.SnapshotID == "" {
		return fmt.Errorf("snapshot_id is required")
	}
	if ref.SandboxID == "" {
		return fmt.Errorf("sandbox_id is required")
	}
	now := time.Now()
	attachedAt := ref.AttachedAt
	if attachedAt.IsZero() {
		attachedAt = now
	}
	lastSeenAt := ref.LastSeenAt
	if lastSeenAt == nil {
		lastSeenAt = &now
	}
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := releaseSnapshotRuntimeRefsBySandboxTx(tx, ref.SandboxID, ref.BindingType, "switched runtime ref"); err != nil {
			return err
		}
		model := newActiveSnapshotRuntimeRefModel(ref, attachedAt, lastSeenAt)
		return tx.Table(constants.SnapshotRuntimeRefTableName).Create(model).Error
	})
}

func ReleaseSnapshotRuntimeRefsBySandbox(ctx context.Context, sandboxID, reason string) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil
	}
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return releaseSnapshotRuntimeRefsBySandboxTx(tx, sandboxID, "", reason)
	})
}

func releaseSnapshotRuntimeRefsBySandboxTx(tx *gorm.DB, sandboxID, bindingType, reason string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil
	}
	now := time.Now()
	query := tx.Table(constants.SnapshotRuntimeRefTableName).
		Where("sandbox_id = ? AND status = ?", sandboxID, SnapshotRuntimeRefStatusActive)
	if trimmed := strings.TrimSpace(bindingType); trimmed != "" {
		query = query.Where("binding_type = ?", trimmed)
	}
	return query.Updates(releasedSnapshotRuntimeRefValues(now, reason)).Error
}

func ListActiveSnapshotRuntimeRefs(ctx context.Context, snapshotID string) ([]SnapshotRuntimeRefInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil, nil
	}
	var modelsOut []models.SnapshotRuntimeRef
	if err := store.db.WithContext(ctx).Table(constants.SnapshotRuntimeRefTableName).
		Where("snapshot_id = ? AND status = ?", snapshotID, SnapshotRuntimeRefStatusActive).
		Order("id asc").
		Find(&modelsOut).Error; err != nil {
		return nil, err
	}
	out := make([]SnapshotRuntimeRefInfo, 0, len(modelsOut))
	for _, item := range modelsOut {
		out = append(out, snapshotRuntimeRefModelToInfo(item))
	}
	return out, nil
}

func GetActiveSnapshotRuntimeRefBySandbox(ctx context.Context, sandboxID string) (*SnapshotRuntimeRefInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, gorm.ErrRecordNotFound
	}
	record := &models.SnapshotRuntimeRef{}
	if err := store.db.WithContext(ctx).Table(constants.SnapshotRuntimeRefTableName).
		Where("sandbox_id = ? AND status = ?", sandboxID, SnapshotRuntimeRefStatusActive).
		Order("id desc").
		First(record).Error; err != nil {
		return nil, err
	}
	info := snapshotRuntimeRefModelToInfo(*record)
	return &info, nil
}

func CountActiveSnapshotRuntimeRefs(ctx context.Context, snapshotID string) (int64, error) {
	if !isReady() {
		return 0, ErrTemplateStoreNotInitialized
	}
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return 0, nil
	}
	var count int64
	if err := store.db.WithContext(ctx).Table(constants.SnapshotRuntimeRefTableName).
		Where("snapshot_id = ? AND status = ?", snapshotID, SnapshotRuntimeRefStatusActive).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func RefreshSnapshotRuntimeRefsFromNode(ctx context.Context, nodeID, nodeIP string, observed []SnapshotRuntimeRefInfo) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	nodeID = strings.TrimSpace(nodeID)
	nodeIP = strings.TrimSpace(nodeIP)
	if nodeID == "" && nodeIP == "" {
		return fmt.Errorf("node id or ip is required")
	}
	now := time.Now()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []models.SnapshotRuntimeRef
		query := tx.Table(constants.SnapshotRuntimeRefTableName).
			Where("status = ?", SnapshotRuntimeRefStatusActive)
		if nodeID != "" {
			query = query.Where("node_id = ?", nodeID)
		} else {
			query = query.Where("node_ip = ?", nodeIP)
		}
		if err := query.Find(&existing).Error; err != nil {
			return err
		}
		existingBySandbox := make(map[string]models.SnapshotRuntimeRef, len(existing))
		for _, item := range existing {
			existingBySandbox[item.SandboxID] = item
		}
		observedSandboxes := make(map[string]struct{}, len(observed))
		for _, raw := range observed {
			ref := normalizeSnapshotRuntimeRef(raw)
			if ref.SandboxID == "" || ref.SnapshotID == "" {
				continue
			}
			observedSandboxes[ref.SandboxID] = struct{}{}
			if ref.NodeID == "" {
				ref.NodeID = nodeID
			}
			if ref.NodeIP == "" {
				ref.NodeIP = nodeIP
			}
			lastSeen := now
			if existingRef, ok := existingBySandbox[ref.SandboxID]; ok &&
				strings.EqualFold(existingRef.BindingType, ref.BindingType) &&
				strings.EqualFold(existingRef.SnapshotID, ref.SnapshotID) {
				if err := tx.Table(constants.SnapshotRuntimeRefTableName).
					Where("id = ?", existingRef.ID).
					Updates(map[string]any{
						"node_id":      ref.NodeID,
						"node_ip":      ref.NodeIP,
						"memory_vol":   ref.MemoryVol,
						"memory_dev":   "",
						"rootfs_vol":   ref.RootfsVol,
						"sandbox_gen":  ref.SandboxGen,
						"last_seen_at": &lastSeen,
						"last_error":   "",
						"updated_at":   now,
					}).Error; err != nil {
					return err
				}
				continue
			}
			if err := releaseSnapshotRuntimeRefsBySandboxTx(tx, ref.SandboxID, ref.BindingType, "reconciled runtime ref"); err != nil {
				return err
			}
			model := newActiveSnapshotRuntimeRefModel(ref, now, &lastSeen)
			if err := tx.Table(constants.SnapshotRuntimeRefTableName).Create(model).Error; err != nil {
				return err
			}
		}
		for _, item := range existing {
			if _, ok := observedSandboxes[item.SandboxID]; ok {
				continue
			}
			if err := tx.Table(constants.SnapshotRuntimeRefTableName).
				Where("id = ? AND status = ?", item.ID, SnapshotRuntimeRefStatusActive).
				Updates(releasedSnapshotRuntimeRefValues(now, "runtime ref not observed on node")).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func UpdateSnapshotRuntimeRefsNodeError(ctx context.Context, nodeID, nodeIP, message string) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	query := store.db.WithContext(ctx).Table(constants.SnapshotRuntimeRefTableName).
		Where("status = ?", SnapshotRuntimeRefStatusActive)
	if strings.TrimSpace(nodeID) != "" {
		query = query.Where("node_id = ?", strings.TrimSpace(nodeID))
	} else if strings.TrimSpace(nodeIP) != "" {
		query = query.Where("node_ip = ?", strings.TrimSpace(nodeIP))
	} else {
		return nil
	}
	return query.Updates(map[string]any{
		"last_error": strings.TrimSpace(message),
		"updated_at": time.Now(),
	}).Error
}

func SnapshotRuntimeRefFromSandboxData(sandbox *sandboxtypes.SandboxData) (SnapshotRuntimeRefInfo, bool) {
	if sandbox == nil {
		return SnapshotRuntimeRefInfo{}, false
	}
	ref := snapshotRuntimeRefFromAnnotationMap(sandbox.SandboxID, sandbox.HostID, sandbox.HostIP, sandbox.Annotations)
	return ref, ref.SnapshotID != ""
}

func SnapshotRuntimeRefFromSandboxBriefData(sandbox *sandboxtypes.SandboxBriefData) (SnapshotRuntimeRefInfo, bool) {
	if sandbox == nil {
		return SnapshotRuntimeRefInfo{}, false
	}
	ref := snapshotRuntimeRefFromAnnotationMap(sandbox.SandboxID, sandbox.HostID, sandbox.HostIP, sandbox.Annotations)
	return ref, ref.SnapshotID != ""
}

func snapshotRuntimeRefFromAnnotationMap(sandboxID, nodeID, nodeIP string, annotations map[string]string) SnapshotRuntimeRefInfo {
	// v5: the physical memory_vol annotation no longer exists. The ref's
	// MemoryVol is populated only from the rollback RPC response (see
	// runRollbackSandboxJob); cubelet's local catalog is the authority.
	ref := normalizeSnapshotRuntimeRef(SnapshotRuntimeRefInfo{
		SandboxID:   sandboxID,
		NodeID:      nodeID,
		NodeIP:      nodeIP,
		SnapshotID:  strings.TrimSpace(annotations[constants.CubeAnnotationRuntimeSnapshotID]),
		BindingType: SnapshotRuntimeBindingMemoryBacking,
	})
	if attachedAt, ok := parseSnapshotRuntimeRefTime(annotations[constants.CubeAnnotationRuntimeSnapshotAttachedAt]); ok {
		ref.AttachedAt = attachedAt
		ref.LastSeenAt = &attachedAt
	}
	return ref
}

func parseSnapshotRuntimeRefTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// RegisterSnapshotRuntimeRefForCreatedSandbox records a sandbox↔snapshot
// binding for the runtime ref tracker. v4: master no longer carries a
// physical memory_vol reference; MemoryVol on the ref is intentionally left
// empty. The replica lookup is still performed for its side-effect of
// validating that a bindable ready replica exists on the chosen node before
// registering the ref - callers should fail fast if the snapshot is not
// actually consumable.
func RegisterSnapshotRuntimeRefForCreatedSandbox(ctx context.Context, snapshotID, sandboxID, nodeID, nodeIP string) error {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil
	}
	if _, err := getSnapshotReadyReplica(ctx, snapshotID, nodeID); err != nil {
		return err
	}
	return AcquireSnapshotRuntimeRef(ctx, SnapshotRuntimeRefInfo{
		SnapshotID: snapshotID,
		SandboxID:  sandboxID,
		NodeID:     nodeID,
		NodeIP:     nodeIP,
	})
}

// RegisterSnapshotRuntimeRefForCreatedSandboxWithReplica is a fast-path
// variant of RegisterSnapshotRuntimeRefForCreatedSandbox that skips the
// extra ListReplicas round-trip when the caller has already selected a
// ready replica earlier in the request (e.g. during bindSnapshotCreateReplica).
//
// The supplied replica MUST originate from a successful bind call for the
// same (snapshotID, sandboxID's host) - i.e. the chosen replica that was
// stamped onto reqInOut.DistributionScope. The function still validates the
// replica metadata before acquiring the ref so a stale value cannot create
// a half-baked runtime ref row.
func RegisterSnapshotRuntimeRefForCreatedSandboxWithReplica(
	ctx context.Context,
	snapshotID, sandboxID, nodeID, nodeIP string,
	replica ReplicaStatus,
) error {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil
	}
	if err := validateSnapshotReadyReplica(replica); err != nil {
		return err
	}
	return AcquireSnapshotRuntimeRef(ctx, SnapshotRuntimeRefInfo{
		SnapshotID: snapshotID,
		SandboxID:  sandboxID,
		NodeID:     nodeID,
		NodeIP:     nodeIP,
	})
}
