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

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	SnapshotRuntimeBindingMemoryBacking = "memory_backing"
	SnapshotRuntimeRefStatusActive      = "ACTIVE"
	SnapshotRuntimeRefStatusReleased    = "RELEASED"
)

var snapshotRuntimeActiveUpsertColumns = []string{
	"snapshot_id",
	"node_id",
	"node_ip",
	"memory_vol",
	"rootfs_vol",
	"sandbox_gen",
	"attached_at",
	"last_seen_at",
	"last_error",
	"updated_at",
}

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

func snapshotRuntimeActiveModelToInfo(model models.SnapshotRuntimeActive) SnapshotRuntimeRefInfo {
	return SnapshotRuntimeRefInfo{
		SnapshotID:  strings.TrimSpace(model.SnapshotID),
		SandboxID:   strings.TrimSpace(model.SandboxID),
		NodeID:      strings.TrimSpace(model.NodeID),
		NodeIP:      strings.TrimSpace(model.NodeIP),
		BindingType: strings.TrimSpace(model.BindingType),
		MemoryVol:   strings.TrimSpace(model.MemoryVol),
		RootfsVol:   strings.TrimSpace(model.RootfsVol),
		SandboxGen:  model.SandboxGen,
		Status:      SnapshotRuntimeRefStatusActive,
		AttachedAt:  model.AttachedAt,
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

func newSnapshotRuntimeActiveModel(ref SnapshotRuntimeRefInfo, attachedAt time.Time, lastSeenAt *time.Time, now time.Time) *models.SnapshotRuntimeActive {
	return &models.SnapshotRuntimeActive{
		SnapshotID:  ref.SnapshotID,
		SandboxID:   ref.SandboxID,
		NodeID:      ref.NodeID,
		NodeIP:      ref.NodeIP,
		BindingType: ref.BindingType,
		MemoryVol:   ref.MemoryVol,
		RootfsVol:   ref.RootfsVol,
		SandboxGen:  ref.SandboxGen,
		AttachedAt:  attachedAt,
		LastSeenAt:  lastSeenAt,
		LastError:   strings.TrimSpace(ref.LastError),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func AcquireSnapshotRuntimeRef(ctx context.Context, ref SnapshotRuntimeRefInfo) error {
	return AttachSnapshotRuntimeBinding(ctx, ref, "attached runtime ref")
}

func AttachSnapshotRuntimeBinding(ctx context.Context, ref SnapshotRuntimeRefInfo, reason string) error {
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
		return attachSnapshotRuntimeBindingTx(tx, ref, reason, attachedAt, lastSeenAt, now)
	})
}

func ReleaseSnapshotRuntimeRefsBySandbox(ctx context.Context, sandboxID, reason string) error {
	return DetachSnapshotRuntimeBinding(ctx, sandboxID, "", reason)
}

func DetachSnapshotRuntimeBinding(ctx context.Context, sandboxID, bindingType, reason string) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil
	}
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return detachSnapshotRuntimeBindingsTx(tx, sandboxID, bindingType, reason, time.Now())
	})
}

func upsertSnapshotRuntimeActiveTx(tx *gorm.DB, active *models.SnapshotRuntimeActive) error {
	return tx.Table(constants.SnapshotRuntimeActiveTableName).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "sandbox_id"},
				{Name: "binding_type"},
			},
			DoUpdates: clause.AssignmentColumns(snapshotRuntimeActiveUpsertColumns),
		}).Create(active).Error
}

func attachSnapshotRuntimeBindingTx(tx *gorm.DB, ref SnapshotRuntimeRefInfo, reason string, attachedAt time.Time, lastSeenAt *time.Time, now time.Time) error {
	previous, err := findSnapshotRuntimeActiveTx(tx, ref.SandboxID, ref.BindingType)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if previous != nil && snapshotRuntimeActiveChanged(*previous, ref) {
		if err := insertDetachedSnapshotRuntimeHistoryTx(tx, *previous, "switched runtime ref", now); err != nil {
			return err
		}
	}
	if err := releaseHistoricalActiveRuntimeRefsTx(tx, ref.SandboxID, ref.BindingType, now, "superseded by active binding"); err != nil {
		return err
	}
	active := newSnapshotRuntimeActiveModel(ref, attachedAt, lastSeenAt, now)
	if err := upsertSnapshotRuntimeActiveTx(tx, active); err != nil {
		return err
	}
	history := newActiveSnapshotRuntimeRefModel(ref, attachedAt, lastSeenAt)
	history.LastError = strings.TrimSpace(reason)
	return tx.Table(constants.SnapshotRuntimeRefTableName).Create(history).Error
}

func findSnapshotRuntimeActiveTx(tx *gorm.DB, sandboxID, bindingType string) (*models.SnapshotRuntimeActive, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	bindingType = strings.TrimSpace(bindingType)
	if bindingType == "" {
		bindingType = SnapshotRuntimeBindingMemoryBacking
	}
	record := &models.SnapshotRuntimeActive{}
	if err := tx.Table(constants.SnapshotRuntimeActiveTableName).
		Where("sandbox_id = ? AND binding_type = ?", sandboxID, bindingType).
		First(record).Error; err != nil {
		return nil, err
	}
	return record, nil
}

func snapshotRuntimeActiveChanged(existing models.SnapshotRuntimeActive, next SnapshotRuntimeRefInfo) bool {
	return strings.TrimSpace(existing.SnapshotID) != strings.TrimSpace(next.SnapshotID) ||
		strings.TrimSpace(existing.MemoryVol) != strings.TrimSpace(next.MemoryVol) ||
		strings.TrimSpace(existing.RootfsVol) != strings.TrimSpace(next.RootfsVol) ||
		existing.SandboxGen != next.SandboxGen
}

func detachSnapshotRuntimeBindingsTx(tx *gorm.DB, sandboxID, bindingType, reason string, now time.Time) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil
	}
	var existing []models.SnapshotRuntimeActive
	query := tx.Table(constants.SnapshotRuntimeActiveTableName).Where("sandbox_id = ?", sandboxID)
	if trimmed := strings.TrimSpace(bindingType); trimmed != "" {
		query = query.Where("binding_type = ?", trimmed)
	}
	if err := query.Clauses(clause.Locking{Strength: "UPDATE"}).Find(&existing).Error; err != nil {
		return err
	}
	for _, item := range existing {
		if err := detachObservedSnapshotRuntimeBindingTx(tx, item, reason, now); err != nil {
			return err
		}
	}
	return nil
}

func detachObservedSnapshotRuntimeBindingTx(tx *gorm.DB, item models.SnapshotRuntimeActive, reason string, now time.Time) error {
	result := snapshotRuntimeActiveVersionQuery(tx, item).Delete(&models.SnapshotRuntimeActive{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	if err := releaseHistoricalActiveRuntimeRefsTx(tx, item.SandboxID, item.BindingType, now, reason); err != nil {
		return err
	}
	return insertDetachedSnapshotRuntimeHistoryTx(tx, item, reason, now)
}

func insertDetachedSnapshotRuntimeHistoryTx(tx *gorm.DB, item models.SnapshotRuntimeActive, reason string, now time.Time) error {
	history := snapshotRuntimeActiveModelToInfo(item)
	history.Status = SnapshotRuntimeRefStatusReleased
	history.ReleasedAt = &now
	history.LastError = strings.TrimSpace(reason)
	model := newActiveSnapshotRuntimeRefModel(history, item.AttachedAt, item.LastSeenAt)
	model.Status = SnapshotRuntimeRefStatusReleased
	model.ReleasedAt = &now
	model.LastError = history.LastError
	return tx.Table(constants.SnapshotRuntimeRefTableName).Create(model).Error
}

func attachObservedSnapshotRuntimeBindingTx(tx *gorm.DB, ref SnapshotRuntimeRefInfo, expected *models.SnapshotRuntimeActive, now time.Time) error {
	current, err := findSnapshotRuntimeActiveTx(tx, ref.SandboxID, ref.BindingType)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if current != nil {
		if expected == nil || !snapshotRuntimeActiveSameVersion(*current, *expected) {
			return nil
		}
	}
	lastSeen := now
	return attachSnapshotRuntimeBindingTx(tx, ref, "reconciled runtime ref", now, &lastSeen, now)
}

func releaseHistoricalActiveRuntimeRefsTx(tx *gorm.DB, sandboxID, bindingType string, now time.Time, reason string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil
	}
	var ids []uint
	query := tx.Table(constants.SnapshotRuntimeRefTableName).
		Where("sandbox_id = ? AND status = ?", sandboxID, SnapshotRuntimeRefStatusActive)
	if trimmed := strings.TrimSpace(bindingType); trimmed != "" {
		query = query.Where("binding_type = ?", trimmed)
	}
	if err := query.Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return tx.Table(constants.SnapshotRuntimeRefTableName).
		Where("id IN ?", ids).
		Updates(map[string]any{
			"status":      SnapshotRuntimeRefStatusReleased,
			"released_at": now,
			"updated_at":  now,
			"last_error":  strings.TrimSpace(reason),
		}).Error
}

func snapshotRuntimeActiveSameVersion(a, b models.SnapshotRuntimeActive) bool {
	return strings.TrimSpace(a.SandboxID) == strings.TrimSpace(b.SandboxID) &&
		strings.TrimSpace(a.BindingType) == strings.TrimSpace(b.BindingType) &&
		strings.TrimSpace(a.SnapshotID) == strings.TrimSpace(b.SnapshotID) &&
		a.SandboxGen == b.SandboxGen &&
		a.AttachedAt.Equal(b.AttachedAt)
}

func snapshotRuntimeActiveVersionQuery(tx *gorm.DB, item models.SnapshotRuntimeActive) *gorm.DB {
	return tx.Table(constants.SnapshotRuntimeActiveTableName).
		Where("sandbox_id = ? AND binding_type = ? AND snapshot_id = ? AND sandbox_gen = ? AND attached_at = ?",
			item.SandboxID, item.BindingType, item.SnapshotID, item.SandboxGen, item.AttachedAt)
}

func snapshotRuntimeActiveRefreshValues(ref SnapshotRuntimeRefInfo, lastSeen *time.Time, now time.Time) map[string]any {
	return map[string]any{
		"node_id":      ref.NodeID,
		"node_ip":      ref.NodeIP,
		"memory_vol":   ref.MemoryVol,
		"rootfs_vol":   ref.RootfsVol,
		"sandbox_gen":  ref.SandboxGen,
		"last_seen_at": lastSeen,
		"last_error":   "",
		"updated_at":   now,
	}
}

func ListActiveSnapshotRuntimeRefs(ctx context.Context, snapshotID string) ([]SnapshotRuntimeRefInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil, nil
	}
	var modelsOut []models.SnapshotRuntimeActive
	if err := store.db.WithContext(ctx).Table(constants.SnapshotRuntimeActiveTableName).
		Where("snapshot_id = ?", snapshotID).
		Order("sandbox_id asc, binding_type asc").
		Find(&modelsOut).Error; err != nil {
		return nil, err
	}
	out := make([]SnapshotRuntimeRefInfo, 0, len(modelsOut))
	for _, item := range modelsOut {
		out = append(out, snapshotRuntimeActiveModelToInfo(item))
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
	record := &models.SnapshotRuntimeActive{}
	if err := store.db.WithContext(ctx).Table(constants.SnapshotRuntimeActiveTableName).
		Where("sandbox_id = ? AND binding_type = ?", sandboxID, SnapshotRuntimeBindingMemoryBacking).
		First(record).Error; err != nil {
		return nil, err
	}
	info := snapshotRuntimeActiveModelToInfo(*record)
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
	if err := store.db.WithContext(ctx).Table(constants.SnapshotRuntimeActiveTableName).
		Where("snapshot_id = ?", snapshotID).
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
		var existing []models.SnapshotRuntimeActive
		query := tx.Table(constants.SnapshotRuntimeActiveTableName)
		if nodeID != "" {
			query = query.Where("node_id = ?", nodeID)
		} else {
			query = query.Where("node_ip = ?", nodeIP)
		}
		if err := query.Find(&existing).Error; err != nil {
			return err
		}
		existingByKey := make(map[string]models.SnapshotRuntimeActive, len(existing))
		for _, item := range existing {
			existingByKey[snapshotRuntimeBindingKey(item.SandboxID, item.BindingType)] = item
		}
		observedKeys := make(map[string]struct{}, len(observed))
		for _, raw := range observed {
			ref := normalizeSnapshotRuntimeRef(raw)
			if ref.SandboxID == "" || ref.SnapshotID == "" {
				continue
			}
			key := snapshotRuntimeBindingKey(ref.SandboxID, ref.BindingType)
			observedKeys[key] = struct{}{}
			if ref.NodeID == "" {
				ref.NodeID = nodeID
			}
			if ref.NodeIP == "" {
				ref.NodeIP = nodeIP
			}
			lastSeen := now
			if existingRef, ok := existingByKey[key]; ok && strings.EqualFold(existingRef.SnapshotID, ref.SnapshotID) {
				result := snapshotRuntimeActiveVersionQuery(tx, existingRef).
					Updates(snapshotRuntimeActiveRefreshValues(ref, &lastSeen, now))
				if result.Error != nil {
					return result.Error
				}
				continue
			}
			var expected *models.SnapshotRuntimeActive
			if existingRef, ok := existingByKey[key]; ok {
				copy := existingRef
				expected = &copy
			}
			if err := attachObservedSnapshotRuntimeBindingTx(tx, ref, expected, now); err != nil {
				return err
			}
		}
		for _, item := range existing {
			if _, ok := observedKeys[snapshotRuntimeBindingKey(item.SandboxID, item.BindingType)]; ok {
				continue
			}
			if err := detachObservedSnapshotRuntimeBindingTx(tx, item, "runtime ref not observed on node", now); err != nil {
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
	query := store.db.WithContext(ctx).Table(constants.SnapshotRuntimeActiveTableName)
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

func snapshotRuntimeBindingKey(sandboxID, bindingType string) string {
	bindingType = strings.TrimSpace(bindingType)
	if bindingType == "" {
		bindingType = SnapshotRuntimeBindingMemoryBacking
	}
	return strings.TrimSpace(sandboxID) + "\x00" + bindingType
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
